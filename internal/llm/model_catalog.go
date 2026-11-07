package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const modelCatalogVersion = 1

var modelCredentialRefPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ModelCatalogError 是目录存储与测试入口的机读失败。Message 可以安全地交给
// Transport；credential literal 从不进入 error。
type ModelCatalogError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *ModelCatalogError) Error() string { return e.Message }

// ModelCatalogErrorCode 给 HTTP 适配器与调用方一个稳定的错误码。
func ModelCatalogErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var catalogErr *ModelCatalogError
	if errors.As(err, &catalogErr) {
		return catalogErr.Code
	}
	return "internal"
}

func modelCatalogError(code, format string, args ...any) error {
	return &ModelCatalogError{Code: code, Message: fmt.Sprintf(format, args...), Retryable: code == "internal" || code == "upstream_error"}
}

// ModelCatalogCredentialStatus 是 secret 的唯一公开投影。
type ModelCatalogCredentialStatus struct {
	Configured bool `json:"configured"`
	Writable   bool `json:"writable"`
}

// ModelCredentialSource* 是 describe 回的来源层：值只可能来自这两层之一。
// stored > env（环境变量只是回退），因此不存在"合并出第三份真值"的情形。
const (
	ModelCredentialSourceCatalog = "catalog"
	ModelCredentialSourceEnv     = "env"
)

// ModelCredentialView 是一条凭据引用的 describe 结果，逐字段对应官方
// `CredentialInfo`。**没有值字段**：官方的读取路径就没有它，这里也不加。
// Source 只在 Configured 为真时给出，且只有 catalog/env 两个取值。
type ModelCredentialView struct {
	Configured bool
	Source     string
	Writable   bool
}

// ModelCatalogView 保留 models 数组与每个 raw object 的字段顺序。Credentials
// 覆盖两类 ref：被目录模型 `apiKeyEnv` 引用的，以及经独立凭据入口
// （SetCredential）设置、没有任何模型引用它的游离 ref。只回
// configured/writable，永不回值。
type ModelCatalogView struct {
	Revision    int64                                   `json:"revision"`
	Models      []json.RawMessage                       `json:"models"`
	Credentials map[string]ModelCatalogCredentialStatus `json:"credentials"`
}

// ModelTestOverride 是官方 discovery/test 同款“按表单当前值询问”的入口。
// nil 表示沿用目录；非空 credential 仅用于本次请求，不写盘。
type ModelTestOverride struct {
	BaseURL    *string
	API        *string
	Credential *string
}

// ModelTestResult 只报告真实端点响应，不带请求头、credential 或响应正文。
type ModelTestResult struct {
	ID        string `json:"id"`
	OK        bool   `json:"ok"`
	Model     string `json:"model,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
}

// ModelProviderConfig 是一轮真实请求使用的显式 provider 配置。credential
// 只经 Credential() 交给 runner/受管子进程 env，不带 JSON 字段或 String 投影。
// 零值表示该 catalog 模型只是普通 model id，调用方保持 legacy 行为。
type ModelProviderConfig struct {
	BaseURL    string
	API        string
	APIKeyEnv  string
	credential string
}

// Credential 返回 stored credential 或 apiKeyEnv 同名环境变量的解析结果。
func (c ModelProviderConfig) Credential() string { return c.credential }

type modelCatalogDocument struct {
	Version     int               `json:"version"`
	Revision    int64             `json:"revision"`
	Models      []json.RawMessage `json:"models"`
	Credentials map[string]string `json:"credentials,omitempty"`
	// StandaloneCredentials 记录经独立凭据入口（SetCredential）建立、没有任何
	// catalog 模型引用它的 ref 名字（排序去重）。它**不是**第二份凭据真值：
	// 值只存在 Credentials 一张表里，这里只有名字，作用是在
	// `models.catalog.upsert` 的孤儿清理里保护"用户显式写入的游离引用"。
	//
	// 为什么需要它：旧实现的凭据槽位由模型的 apiKeyEnv 引用建立，upsert 会把
	// 没被任何模型引用的 ref 当孤儿删掉。没有这份名单，一次无关的模型写入就会
	// 把游离凭据顺手清掉（见 TestOfficialCredentialsSetFreeRefPersists）。
	StandaloneCredentials []string `json:"standaloneCredentials,omitempty"`
}

// ModelCatalogStore 是单个 home 的目录真相。active model 故意不在这里：
// 它仍由 W61 的 session-model.json 默认键唯一持有。
type ModelCatalogStore struct {
	mu      sync.Mutex
	path    string
	doc     modelCatalogDocument
	loadErr error
	// missing 只表示启动读盘时文件物理不存在；存在的 models=[] 不是 missing。
	// envModel 是从当前 provider 发现的 ephemeral entry，不会被一次只读 List 落盘。
	missing  bool
	envModel json.RawMessage
}

// OpenModelCatalog 读取 <home>/state/model-catalog.json。损坏/非法状态留在
// loadErr，后续所有读写都失败，不会拿空目录覆盖原文件。
func OpenModelCatalog(home string) *ModelCatalogStore {
	store := &ModelCatalogStore{
		path: filepath.Join(home, "state", "model-catalog.json"),
		doc: modelCatalogDocument{
			Version: modelCatalogVersion, Models: []json.RawMessage{}, Credentials: map[string]string{},
		},
	}
	store.loadErr = store.load()
	return store
}

// Missing 报告 catalog 文件是否物理缺失。存在但 models=[]、损坏或非法都不是
// missing，因此不会触发 env fallback，也不会被 env 模型覆盖。
func (s *ModelCatalogStore) Missing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.missing && s.loadErr == nil
}

// SetEnvModelFallback 安装当前进程发现的真实 env entry。返回 applied=false
// 表示 catalog 已存在，调用方必须保留文件真值。显式写操作会把 effective view
// 固化为普通 catalog；只读 List/effort/runner 不会创建文件。
func (s *ModelCatalogStore) SetEnvModelFallback(raw json.RawMessage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false, s.loadErr
	}
	if !s.missing {
		return false, nil
	}
	if _, err := validateCatalogModel(raw); err != nil {
		return false, err
	}
	s.envModel = append(json.RawMessage(nil), raw...)
	return true, nil
}

func (s *ModelCatalogStore) modelsLocked() []json.RawMessage {
	if !s.missing || len(s.envModel) == 0 {
		return s.doc.Models
	}
	models := make([]json.RawMessage, 0, len(s.doc.Models)+1)
	models = append(models, s.envModel)
	models = append(models, s.doc.Models...)
	return models
}

func (s *ModelCatalogStore) documentLocked() modelCatalogDocument {
	doc := s.doc
	doc.Models = s.modelsLocked()
	return doc
}

func (s *ModelCatalogStore) load() error {
	s.missing = false
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.missing = true
			return nil
		}
		return modelCatalogError("internal", "读模型目录 %s: %v", s.path, err)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return modelCatalogError("invalid_field", "模型目录 %s 含重复或非法 JSON 成员: %v", s.path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var doc modelCatalogDocument
	if err := decoder.Decode(&doc); err != nil {
		return modelCatalogError("internal", "解析模型目录 %s: %v", s.path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return modelCatalogError("internal", "模型目录 %s 含额外 JSON: %v", s.path, err)
	}
	if doc.Version != modelCatalogVersion {
		return modelCatalogError("invalid_field", "模型目录 version=%d，只支持 %d", doc.Version, modelCatalogVersion)
	}
	if doc.Revision < 0 {
		return modelCatalogError("invalid_field", "模型目录 revision 不能为负数")
	}
	if doc.Models == nil {
		return modelCatalogError("invalid_field", "模型目录缺 models 数组或 models=null")
	}
	if doc.Credentials == nil {
		doc.Credentials = map[string]string{}
	}
	seen := map[string]struct{}{}
	for i, rawModel := range doc.Models {
		info, err := validateCatalogModel(rawModel)
		if err != nil {
			return modelCatalogError(ModelCatalogErrorCode(err), "模型目录第 %d 行无效: %v", i+1, err)
		}
		if _, exists := seen[info.ID]; exists {
			return modelCatalogError("duplicate_id", "模型目录含重复 id %q", info.ID)
		}
		seen[info.ID] = struct{}{}
	}
	for ref, value := range doc.Credentials {
		if !modelCredentialRefPattern.MatchString(ref) {
			return modelCatalogError("invalid_field",
				"模型目录 credentials 段含非法 ref（长度 %d）：必须匹配 ^[A-Za-z_][A-Za-z0-9_]*$；名字本身不回显", len(ref))
		}
		// NUL 进不了 exec 的环境变量，读盘时就 fail-loud；错误串只说 ref，
		// 值本身一次也不回显。
		if strings.ContainsRune(value, '\x00') {
			return modelCatalogError("invalid_field", "凭据 %q 的值含 NUL 字符（值本身不回显）", ref)
		}
	}
	standalone := make([]string, 0, len(doc.StandaloneCredentials))
	seenStandalone := map[string]struct{}{}
	for i, ref := range doc.StandaloneCredentials {
		if !modelCredentialRefPattern.MatchString(ref) {
			return modelCatalogError("invalid_field",
				"模型目录 standaloneCredentials 第 %d 项不是合法 ref（长度 %d）：必须匹配 ^[A-Za-z_][A-Za-z0-9_]*$；名字本身不回显", i, len(ref))
		}
		if _, dup := seenStandalone[ref]; dup {
			continue
		}
		seenStandalone[ref] = struct{}{}
		standalone = append(standalone, ref)
	}
	sort.Strings(standalone)
	if len(standalone) == 0 {
		standalone = nil
	}
	doc.StandaloneCredentials = standalone
	s.doc = doc
	return nil
}

// List 返回深拷贝；调用方不能绕过锁改 store 内的 raw bytes。
func (s *ModelCatalogStore) List() (ModelCatalogView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ModelCatalogView{}, s.loadErr
	}
	return s.viewLocked(), nil
}

func (s *ModelCatalogStore) viewLocked() ModelCatalogView {
	models := s.modelsLocked()
	view := ModelCatalogView{
		Revision: s.doc.Revision, Models: make([]json.RawMessage, len(models)),
		Credentials: map[string]ModelCatalogCredentialStatus{},
	}
	record := func(ref string) {
		if ref == "" {
			return
		}
		info := s.credentialStatusLocked(ref, os.Getenv)
		view.Credentials[ref] = ModelCatalogCredentialStatus{Configured: info.Configured, Writable: info.Writable}
	}
	for i, raw := range models {
		view.Models[i] = append(json.RawMessage(nil), raw...)
		info, _ := inspectCatalogModel(raw)
		record(info.APIKeyEnv)
	}
	// 游离 ref：没有任何模型引用它，但它确实存在于凭据存储里（或曾经被显式
	// 声明为独立槽位）。少了这一段，界面就会把用户刚存进去的值显示成"未配置"。
	for ref := range s.doc.Credentials {
		record(ref)
	}
	for _, ref := range s.doc.StandaloneCredentials {
		record(ref)
	}
	return view
}

// credentialStatusLocked 是 configured/source/writable 的唯一判定点。
// 优先级 stored > env：同一时刻只有一个值在供货，回 source 时不会编中间态。
// writable 恒为 true —— 独立凭据存储能给任何合法 ref（包括只有环境变量的
// 那个）原子落一个值；写下去之后 stored 覆盖 env，这是真实能力。
func (s *ModelCatalogStore) credentialStatusLocked(ref string, env func(string) string) ModelCredentialView {
	info := ModelCredentialView{Writable: true}
	if strings.TrimSpace(s.doc.Credentials[ref]) != "" {
		info.Configured = true
		info.Source = ModelCredentialSourceCatalog
		return info
	}
	if env != nil && strings.TrimSpace(env(ref)) != "" {
		info.Configured = true
		info.Source = ModelCredentialSourceEnv
	}
	return info
}

func checkCatalogRevision(doc modelCatalogDocument, expected *int64) error {
	if expected != nil {
		if *expected < 0 {
			return modelCatalogError("bad_request", "expectedRevision 不能为负数")
		}
		if *expected != doc.Revision {
			return modelCatalogError("revision_conflict", "模型目录 revision 已变化：expected=%d actual=%d，未覆盖新值", *expected, doc.Revision)
		}
	}
	if doc.Revision == math.MaxInt64 {
		return modelCatalogError("internal", "模型目录 revision 已溢出")
	}
	return nil
}

// Upsert 同 id 做部分更新，新 id 追加。已存字段保持位置，新字段按请求顺序
// 追加；可省略的可选字段显式 null 表示删除。未知字段与 raw value 原样保留。
func (s *ModelCatalogStore) Upsert(model json.RawMessage, credential *string, expectedRevision *int64) (ModelCatalogView, error) {
	incoming, err := parseCatalogObject(model)
	if err != nil {
		return ModelCatalogView{}, err
	}
	if _, err := validateCatalogFields(incoming); err != nil {
		return ModelCatalogView{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ModelCatalogView{}, s.loadErr
	}
	if err := checkCatalogRevision(s.doc, expectedRevision); err != nil {
		return ModelCatalogView{}, err
	}
	incomingInfo := inspectCatalogFields(incoming)
	models := s.modelsLocked()
	index := -1
	for i, raw := range models {
		info, _ := inspectCatalogModel(raw)
		if info.ID == incomingInfo.ID {
			index = i
			break
		}
	}

	merged := stripNullableCatalogNulls(incoming)
	if index >= 0 {
		existing, err := parseCatalogObject(models[index])
		if err != nil {
			return ModelCatalogView{}, modelCatalogError("internal", "已存模型无法解析: %v", err)
		}
		merged = mergeCatalogFields(existing, incoming)
	}
	info, err := validateCatalogFields(merged)
	if err != nil {
		return ModelCatalogView{}, err
	}
	raw, err := marshalCatalogObject(merged)
	if err != nil {
		return ModelCatalogView{}, modelCatalogError("internal", "序列化模型失败: %v", err)
	}

	next := cloneCatalogDocument(s.documentLocked())
	if index >= 0 {
		next.Models[index] = raw
	} else {
		next.Models = append(next.Models, raw)
	}
	if credential != nil {
		value := strings.TrimSpace(*credential)
		if strings.ContainsRune(value, '\x00') {
			return ModelCatalogView{}, modelCatalogError("invalid_field", "credential 含 NUL 字符")
		}
		if value != "" && info.APIKeyEnv == "" {
			return ModelCatalogView{}, modelCatalogError("invalid_field", "model 未声明 apiKeyEnv，credential 没有可保存的引用")
		}
		if value == "" {
			delete(next.Credentials, info.APIKeyEnv)
		} else {
			next.Credentials[info.APIKeyEnv] = value
		}
	}
	used := map[string]bool{}
	for _, rawModel := range next.Models {
		modelInfo, _ := inspectCatalogModel(rawModel)
		if modelInfo.APIKeyEnv != "" {
			used[modelInfo.APIKeyEnv] = true
		}
	}
	// 孤儿清理只清"既没有模型引用、也不是用户显式写进来的游离 ref"。后者由
	// StandaloneCredentials 保护：那是凭据存储自己的槽位，与模型生命周期无关。
	standalone := make(map[string]bool, len(next.StandaloneCredentials))
	for _, ref := range next.StandaloneCredentials {
		standalone[ref] = true
	}
	for ref := range next.Credentials {
		if !used[ref] && !standalone[ref] {
			delete(next.Credentials, ref)
		}
	}
	if err := s.commitLocked(next); err != nil {
		return ModelCatalogView{}, err
	}
	return s.viewLocked(), nil
}

// Reorder 只重排 raw slice，绝不 unmarshal/rebuild 每个 model。
func (s *ModelCatalogStore) Reorder(ids []string, expectedRevision *int64) (ModelCatalogView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ModelCatalogView{}, s.loadErr
	}
	if err := checkCatalogRevision(s.doc, expectedRevision); err != nil {
		return ModelCatalogView{}, err
	}
	models := s.modelsLocked()
	if len(models) == 0 {
		return ModelCatalogView{}, modelCatalogError("empty_catalog", "模型目录为空，不能 reorder")
	}
	if len(ids) == 0 {
		return ModelCatalogView{}, modelCatalogError("empty_order", "reorder ids 不能为空")
	}
	if len(ids) != len(models) {
		return ModelCatalogView{}, modelCatalogError("bad_request", "reorder ids 数量=%d，目录模型数=%d；必须逐项给出完整排列", len(ids), len(models))
	}
	byID := map[string]int{}
	for i, raw := range models {
		info, _ := inspectCatalogModel(raw)
		byID[info.ID] = i
	}
	seen := map[string]bool{}
	next := cloneCatalogDocument(s.documentLocked())
	for i, id := range ids {
		if err := validateCatalogID(id); err != nil {
			return ModelCatalogView{}, err
		}
		if seen[id] {
			return ModelCatalogView{}, modelCatalogError("duplicate_id", "reorder 含重复 id %q", id)
		}
		seen[id] = true
		index, ok := byID[id]
		if !ok {
			return ModelCatalogView{}, modelCatalogError("bad_request", "reorder 含目录外 id %q", id)
		}
		next.Models[i] = append(json.RawMessage(nil), models[index]...)
	}
	if err := s.commitLocked(next); err != nil {
		return ModelCatalogView{}, err
	}
	return s.viewLocked(), nil
}

// ---------------------------------------------------------------------------
// 独立凭据存储（官方 credentials.describe/set/unset 的真值）
// ---------------------------------------------------------------------------

// DescribeCredentials 报告每个 ref 当前能否解析到值、由哪一层供货、是否可写。
// 它是 `credentials.describe` 的唯一后端：**不回值**，只回这三项。
//
// refs 里出现非法名字不是"跳过这一条"，而是整次拒绝（官方 grammar guard：
// 一个名字不合语法就把整批打成 bad-request）。错误串只报位置与长度：ref 是
// 调用方可控字段，把密钥误填进 ref 时也不许被错误串带出去；值则从不出现。
// env 由调用方注入（nil = 不查环境变量），方便测试与 A/B home 隔离。
func (s *ModelCatalogStore) DescribeCredentials(refs []string, env func(string) string) (map[string]ModelCredentialView, error) {
	for i, ref := range refs {
		if err := validateCredentialRef(ref, i); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	views := make(map[string]ModelCredentialView, len(refs))
	for _, ref := range refs {
		views[ref] = s.credentialStatusLocked(ref, env)
	}
	return views, nil
}

// SetCredential 独立写入一个凭据引用：ref **不需要**被任何 catalog 模型引用。
// 值与 `models.catalog.upsert` 的顶层 credential 写的是同一张表（同一个文件、
// 同一把锁、同一个 revision），因此两条路径不会出现两份真值。
//
// expectedRevision 是同一套栅栏：不匹配 → revision_conflict 且一个字都不写。
// 提交仍是 revision+1 + 临时文件 rename：失败时内存与磁盘都保持旧值。
func (s *ModelCatalogStore) SetCredential(ref, value string, expected *int64) (ModelCatalogView, error) {
	if err := validateCredentialRef(ref, 0); err != nil {
		return ModelCatalogView{}, err
	}
	if err := validateCredentialValue(value); err != nil {
		return ModelCatalogView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ModelCatalogView{}, s.loadErr
	}
	if err := checkCatalogRevision(s.doc, expected); err != nil {
		return ModelCatalogView{}, err
	}
	next := cloneCatalogDocument(s.documentLocked())
	next.Credentials[ref] = value
	next.StandaloneCredentials = addStandaloneCredential(next.StandaloneCredentials, ref)
	if err := s.commitLocked(next); err != nil {
		return ModelCatalogView{}, err
	}
	return s.viewLocked(), nil
}

// DeleteCredential 删掉一个凭据引用（存储值与独立槽位标记一起删）。
//
// 本来就不存在时是**幂等成功**：不写盘、不动 revision —— 删一个没有的东西
// 不该被渲染成 500，也不该假装写过一次（那会让 revision 无谓地跳一格，
// 让正在编辑的界面误判"有人改过"）。
func (s *ModelCatalogStore) DeleteCredential(ref string, expected *int64) (ModelCatalogView, error) {
	if err := validateCredentialRef(ref, 0); err != nil {
		return ModelCatalogView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ModelCatalogView{}, s.loadErr
	}
	if err := checkCatalogRevision(s.doc, expected); err != nil {
		return ModelCatalogView{}, err
	}
	_, stored := s.doc.Credentials[ref]
	if !stored && !containsStandaloneCredential(s.doc.StandaloneCredentials, ref) {
		return s.viewLocked(), nil
	}
	next := cloneCatalogDocument(s.documentLocked())
	delete(next.Credentials, ref)
	next.StandaloneCredentials = removeStandaloneCredential(next.StandaloneCredentials, ref)
	if err := s.commitLocked(next); err != nil {
		return ModelCatalogView{}, err
	}
	return s.viewLocked(), nil
}

// validateCredentialRef 是写入侧的 grammar guard，与读取侧（describe）和
// catalog 的 apiKeyEnv 同一条正则：凭据名字既是引用名也是环境变量名。
//
// **错误串里不回显这个字符串**：ref 是调用方可控的字段，把密钥误填进 ref 时
// 若原样回显，就成了一条泄漏路径。只报位置与长度，够定位、不够泄密。
func validateCredentialRef(ref string, index int) error {
	if ref == "" {
		return modelCatalogError("invalid_field", "credential ref 不能是空串（第 %d 个）", index)
	}
	if !modelCredentialRefPattern.MatchString(ref) {
		return modelCatalogError("invalid_field",
			"credential ref 不合法（第 %d 个，长度 %d）：必须匹配 ^[A-Za-z_][A-Za-z0-9_]*$（POSIX shell identifier）；"+
				"名字本身不回显，避免客户端把值误填进 ref 时被错误串带出去", index, len(ref))
	}
	return nil
}

// validateCredentialValue 在写盘前把三种"存下去就会变成另一个值"的输入拒掉。
// 三条消息都只说形状，**绝不带值**：credential 值不进错误串。
func validateCredentialValue(value string) error {
	if strings.TrimSpace(value) == "" {
		return modelCatalogError("invalid_field", "credential 值不能是空串或全空白：删除请走 DeleteCredential/credentials.unset")
	}
	if strings.ContainsRune(value, '\x00') {
		return modelCatalogError("invalid_field", "credential 值含 NUL 字符，不能保存（值本身不回显）")
	}
	if value != strings.TrimSpace(value) {
		return modelCatalogError("invalid_field", "credential 值首尾不能有空白：读取时会 trim，存下去就是另一个值（值本身不回显）")
	}
	return nil
}

func addStandaloneCredential(refs []string, ref string) []string {
	if containsStandaloneCredential(refs, ref) {
		return refs
	}
	next := append(append([]string(nil), refs...), ref)
	sort.Strings(next)
	return next
}

func containsStandaloneCredential(refs []string, ref string) bool {
	for _, candidate := range refs {
		if candidate == ref {
			return true
		}
	}
	return false
}

func removeStandaloneCredential(refs []string, ref string) []string {
	if !containsStandaloneCredential(refs, ref) {
		return refs
	}
	out := make([]string, 0, len(refs))
	for _, candidate := range refs {
		if candidate != ref {
			out = append(out, candidate)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneCatalogDocument(doc modelCatalogDocument) modelCatalogDocument {
	next := modelCatalogDocument{
		Version: doc.Version, Revision: doc.Revision,
		Models: make([]json.RawMessage, len(doc.Models)), Credentials: map[string]string{},
	}
	for i, raw := range doc.Models {
		next.Models[i] = append(json.RawMessage(nil), raw...)
	}
	for ref, value := range doc.Credentials {
		next.Credentials[ref] = value
	}
	if len(doc.StandaloneCredentials) > 0 {
		next.StandaloneCredentials = append([]string(nil), doc.StandaloneCredentials...)
	}
	return next
}

func (s *ModelCatalogStore) commitLocked(next modelCatalogDocument) error {
	next.Version = modelCatalogVersion
	next.Revision = s.doc.Revision + 1
	raw, err := json.Marshal(next)
	if err != nil {
		return modelCatalogError("internal", "序列化模型目录失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return modelCatalogError("internal", "创建模型目录存储目录失败: %v", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return modelCatalogError("internal", "写模型目录临时文件失败: %v", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return modelCatalogError("internal", "原子替换模型目录失败: %v", err)
	}
	s.doc = next
	s.missing = false
	s.envModel = nil
	return nil
}

// modelCredential 在锁内取得模型与其 credential 的快照，网络请求绝不持锁。
// stored credential 优先；只有它为空时才读取 apiKeyEnv 同名环境变量。
func (s *ModelCatalogStore) modelCredential(id string) (catalogModelInfo, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return catalogModelInfo{}, "", s.loadErr
	}
	for _, candidate := range s.modelsLocked() {
		info, _ := inspectCatalogModel(candidate)
		if info.ID != id {
			continue
		}
		credential := ""
		if info.APIKeyEnv != "" {
			credential = strings.TrimSpace(s.doc.Credentials[info.APIKeyEnv])
			if credential == "" {
				credential = strings.TrimSpace(os.Getenv(info.APIKeyEnv))
			}
		}
		return info, credential, nil
	}
	return catalogModelInfo{}, "", modelCatalogError("not_found", "模型目录没有 id %q", id)
}

// ResolveProvider 把 catalog 模型解析成一轮请求使用的显式 provider。
// 三个 provider 字段全缺时是普通 model id，返回零值让调用方走 legacy；
// 只要声明了其中任意字段，缺 baseURL、坏 api、缺 credential 都必须 fail-loud。
func (s *ModelCatalogStore) ResolveProvider(id string) (ModelProviderConfig, error) {
	info, credential, err := s.modelCredential(id)
	if err != nil {
		return ModelProviderConfig{}, err
	}
	if info.API == "" && info.BaseURL == "" && info.APIKeyEnv == "" {
		return ModelProviderConfig{}, nil
	}
	if info.API == "" {
		info.API = "openai-completions"
	}
	if info.API != "openai-completions" {
		return ModelProviderConfig{}, modelCatalogError("invalid_config", "模型 %q 的 api=%q 不受 runner 支持", id, info.API)
	}
	if err := validateCatalogBaseURL(info.BaseURL); err != nil {
		return ModelProviderConfig{}, modelCatalogError("invalid_config", "模型 %q 缺合法 baseURL: %v", id, err)
	}
	if info.APIKeyEnv != "" && credential == "" {
		return ModelProviderConfig{}, modelCatalogError("credential_missing", "模型 %q 引用的 credential %s 未配置", id, info.APIKeyEnv)
	}
	return ModelProviderConfig{
		BaseURL: info.BaseURL, API: info.API, APIKeyEnv: info.APIKeyEnv, credential: credential,
	}, nil
}

// Test 对指定 model 发真实非流式 OpenAI chat/completions。成功只在 HTTP 200
// 且 choices[0].message 确实是 assistant 时返回；连接、协议、配置错误都失败。
func (s *ModelCatalogStore) Test(ctx context.Context, id string, override ModelTestOverride) (ModelTestResult, error) {
	info, credential, err := s.modelCredential(id)
	if err != nil {
		return ModelTestResult{}, err
	}

	if override.BaseURL != nil {
		info.BaseURL = *override.BaseURL
	}
	if override.API != nil {
		info.API = *override.API
	}
	if override.Credential != nil && strings.TrimSpace(*override.Credential) != "" {
		credential = strings.TrimSpace(*override.Credential)
	}
	if info.API == "" {
		info.API = "openai-completions"
	}
	if info.API != "openai-completions" {
		return ModelTestResult{}, modelCatalogError("invalid_config", "模型 %q 的 api=%q 不受当前测试端支持", id, info.API)
	}
	if err := validateCatalogBaseURL(info.BaseURL); err != nil {
		return ModelTestResult{}, modelCatalogError("invalid_config", "模型 %q 缺合法 baseURL: %v", id, err)
	}
	if info.APIKeyEnv != "" && credential == "" {
		return ModelTestResult{}, modelCatalogError("credential_missing", "模型 %q 引用的 credential %s 未配置", id, info.APIKeyEnv)
	}

	// 这条诊断请求同样只发标准字段；唯一要谈的可选字段是限制输出的那个
	// （max_tokens / max_completion_tokens），协商规则与真实会话请求完全一致。
	profile := defaultWireProfile()
	if known, ok := knownEndpointCompat(info.BaseURL); ok {
		profile = known
	}
	testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := strings.TrimSuffix(info.BaseURL, "/") + "/chat/completions"
	testClient := &http.Client{Timeout: 5 * time.Second, Transport: http.DefaultTransport}
	started := time.Now()
	var resp *http.Response
	for attempt := 1; ; attempt++ {
		body, err := json.Marshal(catalogTestRequest(id, profile))
		if err != nil {
			return ModelTestResult{}, modelCatalogError("internal", "序列化模型测试请求失败: %v", err)
		}
		req, err := http.NewRequestWithContext(testCtx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return ModelTestResult{}, modelCatalogError("invalid_config", "构造模型测试请求失败: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "dshd/model-catalog-test")
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		resp, err = testClient.Do(req)
		if err != nil {
			return ModelTestResult{}, redactedCatalogError(credential, "请求模型测试端点失败: %v", err)
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		_ = resp.Body.Close()
		if next, _, ok := negotiateWire(profile, wireSent{maxTokens: true}, resp.StatusCode, string(responseBody)); ok && attempt < maxWireAttempts {
			profile = next
			continue
		}
		return ModelTestResult{}, redactedCatalogError(credential, "模型测试端点回 %d：%s", resp.StatusCode, errorSnippet(responseBody, credential))
	}
	defer resp.Body.Close()
	var answer struct {
		Model   string `json:"model"`
		Choices *[]struct {
			Message *struct {
				Role string `json:"role"`
			} `json:"message"`
		} `json:"choices"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&answer); err != nil {
		return ModelTestResult{}, redactedCatalogError(credential, "模型测试响应不是合法 JSON: %v", err)
	}
	if answer.Choices == nil || len(*answer.Choices) == 0 || (*answer.Choices)[0].Message == nil || (*answer.Choices)[0].Message.Role != "assistant" {
		return ModelTestResult{}, modelCatalogError("upstream_error", "模型测试响应缺 choices[0].message.role=assistant，不能冒充成功")
	}
	return ModelTestResult{ID: id, OK: true, Model: answer.Model, LatencyMs: time.Since(started).Milliseconds()}, nil
}

// catalogTestRequest 是诊断请求体；默认形状与历史实现逐字节一致（map 键排序后
// 仍是 max_tokens/messages/model/stream 四个键）。
func catalogTestRequest(id string, profile wireProfile) map[string]any {
	body := map[string]any{
		"model": id, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "Reply with OK."}},
	}
	if profile.maxTokensField == maxTokensFieldCompletion {
		body["max_completion_tokens"] = 1
	} else {
		body["max_tokens"] = 1
	}
	return body
}

func redactedCatalogError(credential, format string, args ...any) error {
	message := RedactCredential(fmt.Sprintf(format, args...), credential)
	return modelCatalogError("upstream_error", "%s", message)
}

type catalogObjectField struct {
	name  string
	value json.RawMessage
}

type catalogModelInfo struct {
	ID        string
	API       string
	BaseURL   string
	APIKeyEnv string
}

func parseCatalogObject(raw json.RawMessage) ([]catalogObjectField, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return nil, modelCatalogError("invalid_field", "model 含重复或非法 JSON 成员: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, modelCatalogError("invalid_field", "model 不是 JSON object: %v", err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, modelCatalogError("invalid_field", "model 必须是 JSON object")
	}
	fields := []catalogObjectField{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, modelCatalogError("invalid_field", "读取 model 字段失败: %v", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, modelCatalogError("invalid_field", "model 字段名不是 string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, modelCatalogError("invalid_field", "model.%s 不是合法 JSON: %v", key, err)
		}
		fields = append(fields, catalogObjectField{name: key, value: append(json.RawMessage(nil), value...)})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, modelCatalogError("invalid_field", "model object 未闭合: %v", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, modelCatalogError("invalid_field", "model 后有额外 JSON: %v", err)
	}
	return fields, nil
}

func marshalCatalogObject(fields []catalogObjectField) (json.RawMessage, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			buffer.WriteByte(',')
		}
		name, err := json.Marshal(field.name)
		if err != nil {
			return nil, err
		}
		buffer.Write(name)
		buffer.WriteByte(':')
		buffer.Write(field.value)
	}
	buffer.WriteByte('}')
	return json.RawMessage(buffer.Bytes()), nil
}

func mergeCatalogFields(existing, incoming []catalogObjectField) []catalogObjectField {
	merged := append([]catalogObjectField(nil), existing...)
	positions := map[string]int{}
	for i, field := range merged {
		positions[field.name] = i
	}
	for _, field := range incoming {
		nullable := isNullableCatalogField(field.name) && bytes.Equal(bytes.TrimSpace(field.value), []byte("null"))
		if nullable {
			if index, ok := positions[field.name]; ok {
				merged = append(merged[:index], merged[index+1:]...)
				positions = map[string]int{}
				for i, current := range merged {
					positions[current.name] = i
				}
			}
			continue
		}
		if index, ok := positions[field.name]; ok {
			merged[index].value = field.value
		} else {
			positions[field.name] = len(merged)
			merged = append(merged, field)
		}
	}
	return merged
}

func isNullableCatalogField(name string) bool {
	switch name {
	case "name", "owned_by", "api", "baseURL", "apiKeyEnv", "contextWindow", "maxTokens", "input", "reasoningEfforts":
		return true
	default:
		return false
	}
}

func stripNullableCatalogNulls(fields []catalogObjectField) []catalogObjectField {
	out := make([]catalogObjectField, 0, len(fields))
	for _, field := range fields {
		if isNullableCatalogField(field.name) && bytes.Equal(bytes.TrimSpace(field.value), []byte("null")) {
			continue
		}
		out = append(out, field)
	}
	return out
}

func validateCatalogModel(raw json.RawMessage) (catalogModelInfo, error) {
	fields, err := parseCatalogObject(raw)
	if err != nil {
		return catalogModelInfo{}, err
	}
	return validateCatalogFields(fields)
}

func inspectCatalogModel(raw json.RawMessage) (catalogModelInfo, error) {
	fields, err := parseCatalogObject(raw)
	if err != nil {
		return catalogModelInfo{}, err
	}
	return inspectCatalogFields(fields), nil
}

func inspectCatalogFields(fields []catalogObjectField) catalogModelInfo {
	var info catalogModelInfo
	for _, field := range fields {
		var target *string
		switch field.name {
		case "id":
			target = &info.ID
		case "api":
			target = &info.API
		case "baseURL":
			target = &info.BaseURL
		case "apiKeyEnv":
			target = &info.APIKeyEnv
		}
		if target != nil {
			_ = json.Unmarshal(field.value, target)
		}
	}
	return info
}

func validateCatalogFields(fields []catalogObjectField) (catalogModelInfo, error) {
	info := inspectCatalogFields(fields)
	if err := validateCatalogID(info.ID); err != nil {
		return catalogModelInfo{}, err
	}
	for _, field := range fields {
		if isNullableCatalogField(field.name) && bytes.Equal(bytes.TrimSpace(field.value), []byte("null")) {
			// upsert 的 null 是删除可选字段；merge 后不会把这个值写入磁盘。
			continue
		}
		switch field.name {
		case "apiKey", "credential", "secret", "token":
			return catalogModelInfo{}, modelCatalogError("invalid_field", "model.%s 是禁止持久化/回显的秘密字段；使用顶层 credential", field.name)
		case "id":
			var id string
			if err := json.Unmarshal(field.value, &id); err != nil {
				return catalogModelInfo{}, modelCatalogError("invalid_id", "model.id 必须是 string")
			}
		case "name":
			var value string
			if err := json.Unmarshal(field.value, &value); err != nil || value == "" {
				return catalogModelInfo{}, modelCatalogError("invalid_field", "model.name 必须是非空 string")
			}
		case "owned_by":
			var value string
			if err := json.Unmarshal(field.value, &value); err != nil {
				return catalogModelInfo{}, modelCatalogError("invalid_field", "model.owned_by 必须是 string")
			}
		case "api":
			var value string
			if err := json.Unmarshal(field.value, &value); err != nil || value != "openai-completions" {
				return catalogModelInfo{}, modelCatalogError("invalid_field", "model.api 只支持 openai-completions")
			}
		case "baseURL":
			var value string
			if err := json.Unmarshal(field.value, &value); err != nil {
				return catalogModelInfo{}, modelCatalogError("invalid_field", "model.baseURL 必须是 string")
			}
			if err := validateCatalogBaseURL(value); err != nil {
				return catalogModelInfo{}, modelCatalogError("invalid_field", "model.baseURL 非法: %v", err)
			}
		case "apiKeyEnv":
			var value string
			if err := json.Unmarshal(field.value, &value); err != nil || !modelCredentialRefPattern.MatchString(value) {
				return catalogModelInfo{}, modelCatalogError("invalid_field", "model.apiKeyEnv 必须是 POSIX shell identifier")
			}
		case "contextWindow", "maxTokens":
			if err := validateCatalogPositiveInteger(field.value); err != nil {
				return catalogModelInfo{}, modelCatalogError("invalid_field", "model.%s 必须是正整数 token 数: %v", field.name, err)
			}
		case "input":
			var values []string
			if err := json.Unmarshal(field.value, &values); err != nil || values == nil {
				return catalogModelInfo{}, modelCatalogError("invalid_field", "model.input 必须是 text/image string 数组")
			}
			seen := map[string]bool{}
			for _, value := range values {
				if value != "text" && value != "image" {
					return catalogModelInfo{}, modelCatalogError("invalid_field", "model.input 含不支持的值 %q", value)
				}
				if seen[value] {
					return catalogModelInfo{}, modelCatalogError("invalid_field", "model.input 含重复值 %q", value)
				}
				seen[value] = true
			}
		case "reasoningEfforts":
			if err := validateCatalogReasoningEfforts(field.value); err != nil {
				return catalogModelInfo{}, err
			}
		}
	}
	return info, nil
}

func validateCatalogID(id string) error {
	if id == "" || id != strings.TrimSpace(id) {
		return modelCatalogError("invalid_id", "model.id 必须是非空且首尾无空白的 string")
	}
	if len([]byte(id)) > 256 {
		return modelCatalogError("invalid_id", "model.id 长度 %d 超过 256 字节", len([]byte(id)))
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return modelCatalogError("invalid_id", "model.id 不能含控制字符")
		}
	}
	return nil
}

func validateCatalogPositiveInteger(raw json.RawMessage) error {
	text := string(bytes.TrimSpace(raw))
	if text == "" || strings.HasPrefix(text, "-") {
		return fmt.Errorf("收到 %s", text)
	}
	value, err := strconv.ParseUint(text, 10, 63)
	if err != nil || value == 0 {
		return fmt.Errorf("收到 %s", text)
	}
	return nil
}

func validateCatalogBaseURL(value string) error {
	if value == "" || value != strings.TrimSpace(value) {
		return fmt.Errorf("必须是非空且首尾无空白的绝对 http(s) URL")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("scheme 必须是 http/https 且 host 非空")
	}
	if parsed.User != nil {
		return fmt.Errorf("URL 不得携带 userinfo credential")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("baseURL 不得携带 query/fragment")
	}
	return nil
}

func validateCatalogReasoningEfforts(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
		return nil
	}
	fields, err := parseCatalogObject(raw)
	if err != nil {
		return modelCatalogError("invalid_field", "model.reasoningEfforts 必须是 false 或 object: %v", err)
	}
	hasNonOff := false
	for _, field := range fields {
		if field.name != "off" {
			hasNonOff = true
		}
		if field.name == "" {
			return modelCatalogError("invalid_field", "model.reasoningEfforts 含空 level")
		}
		if field.name == "off" && bytes.Equal(bytes.TrimSpace(field.value), []byte("null")) {
			continue
		}
		var value string
		if err := json.Unmarshal(field.value, &value); err != nil || value == "" {
			return modelCatalogError("invalid_field", "model.reasoningEfforts.%s 只允许非空 string，off 另允许 null", field.name)
		}
	}
	if !hasNonOff {
		return modelCatalogError("invalid_field", "model.reasoningEfforts 至少要有 off 以外的一个 level")
	}
	return nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := walkUniqueJSON(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func walkUniqueJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key 不是 string")
			}
			if seen[key] {
				return fmt.Errorf("重复 key %q", key)
			}
			seen[key] = true
			if err := walkUniqueJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := walkUniqueJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("非法 JSON delimiter %q", delim)
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("存在第二个 JSON value")
		}
		return err
	}
	return nil
}
