package transport

// 本文件是官方桌面端 0.2.2-rc.1「settings / credentials」5 个 typert 方法的
// L1 语义实现。官方契约原件：
//
//	@deepseek-ai/dsh-api-settings-controller/lib/typert.host.js
//	  'settings/update'   L430  ns / patch   / expectedRevision? → SettingsNamespaceView
//	  'settings/replace'  L363  ns / section / expectedRevision? → SettingsNamespaceView
//	  'credentials/describe' L154 refs: string[] (≤64, ^[A-Za-z_][A-Za-z0-9_]*$)
//	                             → Record<string, {configured, source?, writable}>
//	  'credentials/set'   L186  ref / value → void
//	  'credentials/unset' L234  ref → void
//
//	@deepseek-ai/dsh-api-settings-controller/lib/index.js
//	  L55  MAX_DESCRIBE_REFS = 64（超过整批拒，不截断）
//	  L56  credentialRefSchema = ^[A-Za-z_][A-Za-z0-9_]*$
//	  L57  describeRequestSchema（refs 数组上限）
//	  L58  setRequestSchema（value: z.string().min(1)）
//	  L62  unsetRequestSchema
//	  L76  projectCredentialInfo：只投影 configured/source/writable 三个字段
//	  L192 没有挂 provider → gateway/internal（本文件回 501 not_implemented）
//	  L208 provider 拒绝 → credential/rejected（本仓收进既有四字段码表）
//
// 对齐口径与 handlers_agent_official.go 相同：**L1 字段对齐**，wire 仍走本仓
// 既有的 `POST /api/<method>` + 裸参数对象 + `{"result":…}` / 四字段 error；
// L2（`remote.mux` + typert Connection envelope）未实现，因此不声称与官方
// 桌面端 wire 兼容。
//
// 三条硬性要求（本文件的核心，不要为了"看起来能跑"而放松）：
//  1. settings.update/replace **必须**走真实 settings 文档与 revision 语义：
//     expectedRevision 不匹配要 409，写失败要 5xx 且磁盘/内存都不许留下新值。
//     本文件因此只调用 settingsform.Service，不自己存任何东西。
//  2. credentials.describe **只回掩码/是否存在/来源类型**：值的明文一次也不
//     进 JSON、不进错误串、不进日志（本文件不打印任何凭据）。
//  3. 没有真实后端就回 not_implemented 并写清缺什么，**禁止**在内存里搭一个
//     假密钥库顶替（那会让界面显示"已保存"而重启后消失）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	"dshgo/internal/llm"
	"dshgo/internal/settingsform"
)

// 官方方法名。官方 `createTypertRemoteInfo` 里是 `'settings/update'` 这种
// 带 `/` 的键；本仓 58 个既有方法与另外三个官方文件一致，统一用点号。
const (
	MethodSettingsUpdate      Method = "settings.update"
	MethodSettingsReplace     Method = "settings.replace"
	MethodCredentialsDescribe Method = "credentials.describe"
	MethodCredentialsSet      Method = "credentials.set"
	MethodCredentialsUnset    Method = "credentials.unset"
)

// officialCredentialRefPattern 与官方 zod 的 `credentialRefSchema` 逐字对应，
// 也与 llm catalog 的 apiKeyEnv 语法同一条正则：凭据名字是 POSIX shell
// identifier，因此它既能当引用名，也能当环境变量名。
var officialCredentialRefPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// officialCredentialDescribeLimit 是官方 `MAX_DESCRIBE_REFS`：一次 describe
// 最多 64 个引用，超过整次拒绝（不是截断——截断会让界面以为剩下的不存在）。
const officialCredentialDescribeLimit = 64

// OfficialSettingsCredentialMethods 按固定顺序返回本文件实现的 5 个方法。
// 新增/删除方法必须显式改这里，不能让"方法悄悄少了一个"从路由表漏过去。
func OfficialSettingsCredentialMethods() []Method {
	return []Method{
		MethodSettingsUpdate, MethodSettingsReplace,
		MethodCredentialsDescribe, MethodCredentialsSet, MethodCredentialsUnset,
	}
}

// IsOfficialSettingsCredentialMethod 判断一个 /api 路径名是否属于本文件的 5 个方法。
//
// 路由侧只需要这一次判断。注意它与既有 `settings.` 前缀分支的**优先级**：
// 本判断必须排在 `strings.HasPrefix(name, "settings.")` 之前，否则
// settings.update/replace 会被 settings.form.* 的适配器当成未知方法 404
// （见 RESULT 的接线清单）。
func IsOfficialSettingsCredentialMethod(name string) bool {
	switch Method(name) {
	case MethodSettingsUpdate, MethodSettingsReplace,
		MethodCredentialsDescribe, MethodCredentialsSet, MethodCredentialsUnset:
		return true
	}
	return false
}

// OfficialCredentialInfo 逐字段对齐官方 `CredentialInfo`：
//
//	configured —— 现在解析这个引用能不能拿到值
//	source     —— 当前供货的那一层（未配置时缺省；判定不了时也不编）
//	writable   —— 当前 provider 能不能写这个引用
//
// source 只有两个取值（来源类型，不是值）：
//
//	"catalog" —— 本地凭据存储（<home>/state/model-catalog.json 的 credentials 段）
//	"env"     —— 同名环境变量回退；写入之后 stored 覆盖 env
//
// **值本身没有字段**：官方的读取路径就没有它，本仓也不加。
type OfficialCredentialInfo struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"`
	Writable   bool   `json:"writable"`
}

// OfficialCredentialDescribeResult 是 credentials.describe 的结果
// （官方 `Record<ref, CredentialInfo>`）。
//
// 用**命名类型**而不是 map[string]OfficialCredentialInfo 是有原因的：
// 信封判定 alreadyEnveloped 认的是 map[string]any，裸 map 里若有一个 ref
// 正好叫 "result"/"error"（合法名字），整份结果就会被当成"已经包过信封"，
// 成功响应当场变成裸对象。命名类型让这条路径不可能发生。
type OfficialCredentialDescribeResult map[string]OfficialCredentialInfo

// OfficialCredentialStore 是 credentials.* 的真实后端。
//
// 三个方法都是"真事实"的唯一入口：Describe 只能回 configured/source/writable，
// Set/Unset 必须落到真实存储。没有这样的后端时，宿主把该字段留空（nil），
// 调用回 not_implemented——**不要**实现一个内存版凑数。
type OfficialCredentialStore interface {
	Describe(refs []string) (map[string]OfficialCredentialInfo, error)
	Set(ref, value string) error
	Unset(ref string) error
}

// OfficialSettingsCredentialAPI 是宿主侧的真实实现集合。
//
// 两个字段都可以为 nil（= 当前宿主没有真实支撑），也可以只挂一个：
// settings.update/replace 与 credentials.* 是两条独立的能力，缺哪条就
// 只让哪条 501，不许用另一条假装。
type OfficialSettingsCredentialAPI struct {
	// Settings 是真实设置文档 + revision 语义的持有者（settingsform.Service）。
	Settings *settingsform.Service
	// Credentials 是真实凭据存储的适配器（见 NewCatalogCredentialStore）。
	Credentials OfficialCredentialStore
}

// ---------------------------------------------------------------------------
// 挂载点
// ---------------------------------------------------------------------------

// officialSettingsCredentialMount 把一份实现挂到一个 Server 上。
//
// 用"Server 指针 → 实现"的映射而不是给 Server 加字段：server.go 是冻结的
// 路由文件，新增官方方法不许动它；每个 Server 各自的实现也天然满足 A/B
// home 隔离（两个 Server 不会共享同一份 settings/credentials 后端）。
var officialSettingsCredentialMount = struct {
	mu sync.RWMutex
	m  map[*Server]OfficialSettingsCredentialAPI
}{m: map[*Server]OfficialSettingsCredentialAPI{}}

// AttachOfficialSettingsCredentialAPI 给这个 Server 挂上官方 settings/credentials
// 的真实实现。装配点在 cmd/dshd 的 serve 链上（见 RESULT 的接线清单）。
func (s *Server) AttachOfficialSettingsCredentialAPI(api OfficialSettingsCredentialAPI) {
	if s == nil {
		return
	}
	officialSettingsCredentialMount.mu.Lock()
	defer officialSettingsCredentialMount.mu.Unlock()
	officialSettingsCredentialMount.m[s] = api
}

// DetachOfficialSettingsCredentialAPI 摘掉实现（测试隔离与热重载收尾用）。
func (s *Server) DetachOfficialSettingsCredentialAPI() {
	if s == nil {
		return
	}
	officialSettingsCredentialMount.mu.Lock()
	defer officialSettingsCredentialMount.mu.Unlock()
	delete(officialSettingsCredentialMount.m, s)
}

func (s *Server) officialSettingsCredentialAPI() (OfficialSettingsCredentialAPI, bool) {
	if s == nil {
		return OfficialSettingsCredentialAPI{}, false
	}
	officialSettingsCredentialMount.mu.RLock()
	defer officialSettingsCredentialMount.mu.RUnlock()
	api, ok := officialSettingsCredentialMount.m[s]
	return api, ok
}

// ---------------------------------------------------------------------------
// 分发
// ---------------------------------------------------------------------------

// serveOfficialSettingsCredentialAPI 处理本文件的 5 个方法。
//
// 调用方（server.go 的 /api 路由）只需在**既有 prefix 分支之前**判断
// `IsOfficialSettingsCredentialMethod(name)` 并调用它；认证、POST 校验、
// 失败状态日志都已经在 handleAPI 里做过。
func (s *Server) serveOfficialSettingsCredentialAPI(w http.ResponseWriter, r *http.Request, name string) {
	api, ok := s.officialSettingsCredentialAPI()
	if !ok {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
			fmt.Sprintf("%s 未接线：宿主没有 AttachOfficialSettingsCredentialAPI", name)))
		return
	}
	params, err := decodeOfficialParams(r)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	result, err := routeOfficialSettingsCredentialMethod(api, Method(name), params)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// routeOfficialSettingsCredentialMethod 是一个小 switch，放在本文件里而不是
// server.go：官方方法还会继续加，路由表却是冻结的。
func routeOfficialSettingsCredentialMethod(api OfficialSettingsCredentialAPI, method Method, p *officialParams) (any, error) {
	switch method {
	case MethodSettingsUpdate:
		return officialSettingsWrite(api, p, false)
	case MethodSettingsReplace:
		return officialSettingsWrite(api, p, true)
	case MethodCredentialsDescribe:
		return officialCredentialsDescribe(api, p)
	case MethodCredentialsSet:
		return officialCredentialsSet(api, p)
	case MethodCredentialsUnset:
		return officialCredentialsUnset(api, p)
	}
	return nil, officialNotImplemented("未知的官方 settings/credentials 方法 %q", method)
}

// ---------------------------------------------------------------------------
// settings.update / settings.replace
// ---------------------------------------------------------------------------

func officialSettingsWrite(api OfficialSettingsCredentialAPI, p *officialParams, replace bool) (any, error) {
	method, sectionName := "settings.update", "patch"
	if replace {
		method, sectionName = "settings.replace", "section"
	}
	if api.Settings == nil {
		return nil, officialNotImplemented(
			"%s 未接线：当前 home 上没有真实 settings 文档后端（settingsform.Service），"+
				"所以 revision 语义无从谈起", method)
	}

	// 官方参数名是 ns/patch|section/expectedRevision。`namespace` 是别名
	// （本仓既有 58 个方法用 `ns`，界面偶尔写全名），两个都给且不一致 → 400。
	o := newOfficialObject("参数", p.m)
	ns, hasNS, err := o.str("ns")
	if err != nil {
		return nil, err
	}
	alias, hasAlias, err := o.str("namespace")
	if err != nil {
		return nil, err
	}
	rawSection, hasSection := o.take(sectionName)
	expected, err := officialExpectedRevision(o)
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if hasNS && hasAlias && ns != alias {
		return nil, officialBadRequest("ns 与 namespace 冲突")
	}
	if !hasNS {
		ns = alias
	}
	if !hasNS && !hasAlias {
		return nil, officialBadRequest("缺 ns（官方 %s 的第一个参数）", method)
	}
	if ns == "" {
		return nil, officialBadRequest("ns 是空串：设置命名空间不能为空")
	}
	if !hasSection {
		return nil, officialBadRequest("缺 %s（官方 %s 的参数名）", sectionName, method)
	}
	if strings.TrimSpace(string(rawSection)) == "null" {
		return nil, officialBadRequest("%s 不能是 null：要清空 user 段就传 {}", sectionName)
	}
	var section map[string]json.RawMessage
	if err := json.Unmarshal(rawSection, &section); err != nil {
		return nil, officialBadRequest("%s 必须是 JSON 对象（官方 z.record(string, json)）", sectionName)
	}
	if section == nil {
		section = map[string]json.RawMessage{}
	}
	input := make(map[string]any, len(section))
	for key, raw := range section {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, officialBadRequest("%s.%s 不是合法 JSON", sectionName, key)
		}
		input[key] = value
	}

	var result settingsform.MutationResult
	if replace {
		result, err = api.Settings.Replace(ns, input, expected)
	} else {
		result, err = api.Settings.Update(ns, input, expected)
	}
	if err != nil {
		return nil, officialSettingsError(err)
	}
	return result.Namespace.Official(), nil
}

// officialExpectedRevision 取官方可选参数 expectedRevision。
//
// 官方的类型是 `z.union([z.undefined(), z.number()])`：**null 不合法**
// （它既不是 undefined 也不是 number），所以这里显式拒绝，而不是把
// "无条件写"当成 null 的默认含义——那会把一次客户端 bug 变成覆盖别人的修改。
func officialExpectedRevision(o *officialObject) (*int64, error) {
	// 先自己看一眼原文：encoding/json 把 `null` 解成零值且**不报错**，
	// 于是 `expectedRevision:null` 会静默变成 0——那是最坏的一种：
	// 客户端以为"无条件写"，宿主以为"必须等于 revision 0"。
	if raw, has := o.take("expectedRevision"); has && strings.TrimSpace(string(raw)) == "null" {
		return nil, officialBadRequest("expectedRevision 不能是 null：官方只接受省略（无条件写）或数字")
	}
	revision, ok, err := o.number("expectedRevision")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if revision < 0 {
		return nil, officialBadRequest("expectedRevision 不能是负数（它是 describe 返回的非负 revision）")
	}
	return &revision, nil
}

// officialSettingsError 把 settingsform / llm catalog 的失败翻译成四字段错误。
//
// 码只取本仓既有词表里能对上 hint/retryable 的那几个：参数/取值类 → 400
// bad_request，revision 过期 → 409 revision_conflict，未知 ns → 404，
// 其余 → 500 internal（绝不渲染成成功）。
func officialSettingsError(err error) error {
	var typed *officialError
	if errors.As(err, &typed) {
		return err
	}
	var apiErr *settingsform.Error
	if errors.As(err, &apiErr) {
		code, status := "bad_request", apiErr.Status
		switch apiErr.Code {
		case "revision_conflict":
			code = "revision_conflict"
		case "not_found":
			code = "not_found"
		case "internal":
			code = "internal"
		}
		if status == 0 {
			status = officialStatusForCode(code)
		}
		// 具体分类（unknown_field / invalid_type / invalid_enum / out_of_range）
		// 保留在 message 里：错误码词表是契约，细分理由要能被看见。
		message := apiErr.Message
		if apiErr.Code != "" && apiErr.Code != code {
			message = apiErr.Code + ": " + message
		}
		return &officialError{status: status, code: code, msg: message}
	}
	var catalogErr *llm.ModelCatalogError
	if errors.As(err, &catalogErr) {
		switch catalogErr.Code {
		case "revision_conflict":
			return officialRevisionConflict("%s", catalogErr.Message)
		case "bad_request", "invalid_field", "invalid_id", "invalid_config", "credential_missing", "empty_order":
			return officialBadRequest("%s", catalogErr.Message)
		}
	}
	if errors.Is(err, ErrOfficialNotImplemented) {
		return err
	}
	return officialInternal("%v", err)
}

func officialStatusForCode(code string) int {
	switch code {
	case "revision_conflict":
		return http.StatusConflict
	case "not_found":
		return http.StatusNotFound
	case "internal":
		return http.StatusInternalServerError
	case "not_implemented":
		return http.StatusNotImplemented
	}
	return http.StatusBadRequest
}

// ---------------------------------------------------------------------------
// credentials.describe
// ---------------------------------------------------------------------------

func officialCredentialsDescribe(api OfficialSettingsCredentialAPI, p *officialParams) (any, error) {
	if api.Credentials == nil {
		return nil, officialNotImplemented(
			"credentials.describe 未接线：当前 home 上没有真实凭据后端" +
				"（catalog credential 存储或等价物），没有可描述的真值")
	}
	o := newOfficialObject("参数", p.m)
	rawRefs, hasRefs := o.take("refs")
	if err := o.finish(); err != nil {
		return nil, err
	}
	if !hasRefs {
		return nil, officialBadRequest("缺 refs（官方 credentials.describe 的参数名）")
	}
	if strings.TrimSpace(string(rawRefs)) == "null" {
		return nil, officialBadRequest("refs 必须是字符串数组，不能是 null")
	}
	var refs []string
	if err := json.Unmarshal(rawRefs, &refs); err != nil {
		return nil, officialBadRequest("refs 必须是字符串数组（官方 z.array(z.string())）")
	}
	if err := validateOfficialCredentialRefs(refs); err != nil {
		return nil, err
	}

	views, err := api.Credentials.Describe(refs)
	if err != nil {
		return nil, officialSettingsError(err)
	}
	out := make(OfficialCredentialDescribeResult, len(refs))
	for _, ref := range refs {
		info, ok := views[ref]
		if !ok {
			// 后端漏答一个请求的引用 = 事实不完整。宁可 500 也不静默少一行：
			// 界面少一行会显示成"未配置"，那是假信息。
			return nil, officialInternal("凭据后端没有返回 %q 的描述（官方要求每个请求的引用各一条）", ref)
		}
		out[ref] = info
	}
	return out, nil
}

func validateOfficialCredentialRefs(refs []string) error {
	if len(refs) > officialCredentialDescribeLimit {
		return officialBadRequest("refs 最多 %d 个，收到 %d 个（官方 MAX_DESCRIBE_REFS）",
			officialCredentialDescribeLimit, len(refs))
	}
	for i, ref := range refs {
		if err := validateOfficialCredentialRef(ref, i); err != nil {
			return err
		}
	}
	return nil
}

// validateOfficialCredentialRef 是官方 `credentialRefSchema` 的同一道 grammar
// guard：`^[A-Za-z_][A-Za-z0-9_]*$`（POSIX shell identifier，因此它同时能当
// 环境变量名）。
//
// **错误串里不回显这个字符串**：官方 `parseRequest` 只回 zod 的 message/path，
// 不回值；而 ref 字段是客户端可控的，把密钥误填进 ref 时若原样回显，就成了一条
// 泄漏路径。这里只报位置与长度，够定位、不够泄密。
func validateOfficialCredentialRef(ref string, index int) error {
	if ref == "" {
		return officialBadRequest("凭据引用名不能是空串（第 %d 个）", index)
	}
	if !officialCredentialRefPattern.MatchString(ref) {
		return officialBadRequest(
			"凭据引用名不合法（第 %d 个，长度 %d）：必须匹配 ^[A-Za-z_][A-Za-z0-9_]*$（POSIX shell identifier）；"+
				"名字本身不回显，避免客户端把值误填进 ref 时被错误串带出去", index, len(ref))
	}
	return nil
}

// ---------------------------------------------------------------------------
// credentials.set / credentials.unset
// ---------------------------------------------------------------------------

func officialCredentialsSet(api OfficialSettingsCredentialAPI, p *officialParams) (any, error) {
	if api.Credentials == nil {
		return nil, officialNotImplemented(
			"credentials.set 未接线：当前 home 上没有真实凭据写入后端" +
				"（catalog credential 存储或等价物），不会用内存假库顶替")
	}
	o := newOfficialObject("参数", p.m)
	ref, hasRef, err := o.str("ref")
	if err != nil {
		return nil, err
	}
	value, hasValue, err := o.str("value")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if !hasRef {
		return nil, officialBadRequest("缺 ref（官方 credentials.set 的第一个参数）")
	}
	if err := validateOfficialCredentialRef(ref, 0); err != nil {
		return nil, err
	}
	if !hasValue {
		return nil, officialBadRequest("缺 value（官方 credentials.set 的第二个参数）")
	}
	// 官方是 z.string().min(1)：空串直接拒。空白串另外拒，因为既有存储会
	// TrimSpace 后当作"删除"，那等于让一次手滑把旧密钥抹掉。
	if value == "" {
		return nil, officialBadRequest("value 不能是空串（官方 min(1)）")
	}
	if strings.TrimSpace(value) == "" {
		return nil, officialBadRequest("value 不能全是空白：既有凭据存储会 trim 成空并当作删除")
	}
	// 错误串里**绝不出值**：下面每条消息都只提 ref，不 Echo value。
	if strings.ContainsRune(value, '\x00') {
		return nil, officialBadRequest("value 含 NUL 字符：凭据存储拒绝保存（值本身不回显）")
	}
	if value != strings.TrimSpace(value) {
		return nil, officialBadRequest("value 首尾含空白：既有凭据存储写入时会 trim，存下去的会是另一个值")
	}
	if err := api.Credentials.Set(ref, value); err != nil {
		return nil, officialSettingsError(err)
	}
	// 官方结果是 void；本仓成功信封不接受 null，用空对象表达"没有载荷"。
	return map[string]any{}, nil
}

func officialCredentialsUnset(api OfficialSettingsCredentialAPI, p *officialParams) (any, error) {
	if api.Credentials == nil {
		return nil, officialNotImplemented(
			"credentials.unset 未接线：当前 home 上没有真实凭据写入后端" +
				"（catalog credential 存储或等价物），不会用内存假库顶替")
	}
	o := newOfficialObject("参数", p.m)
	ref, hasRef, err := o.str("ref")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if !hasRef {
		return nil, officialBadRequest("缺 ref（官方 credentials.unset 的第一个参数）")
	}
	if err := validateOfficialCredentialRef(ref, 0); err != nil {
		return nil, err
	}
	if err := api.Credentials.Unset(ref); err != nil {
		return nil, officialSettingsError(err)
	}
	return map[string]any{}, nil
}

// ---------------------------------------------------------------------------
// 真实后端：llm 的独立凭据存储
// ---------------------------------------------------------------------------

// catalogCredentialStore 是 credentials.* 在 llm.ModelCatalogStore 上的实现。
//
// 真值就是 `<home>/state/model-catalog.json` 的 `credentials` 段——与
// `models.catalog.upsert` 顶层 `credential` 是**同一张表、同一次原子提交**，
// 因此没有第二份凭据真相。区别只在入口：
//
//   - describe：只回 configured/source/writable，结构上就没有值字段；
//   - set/unset：走独立的 SetCredential/DeleteCredential，ref 不需要任何模型
//     引用它（游离 ref），与 upsert 共用同一把锁与同一个 revision；
//   - 提交：revision+1 + 临时文件 rename，写失败内存/磁盘都保持旧值。
type catalogCredentialStore struct {
	store *llm.ModelCatalogStore
	// env 只用来判定 source（本地凭据存储还是环境变量），默认 os.Getenv。
	env func(string) string
}

// NewCatalogCredentialStore 用宿主的真实模型目录造一个凭据后端。
func NewCatalogCredentialStore(store *llm.ModelCatalogStore) OfficialCredentialStore {
	return &catalogCredentialStore{store: store, env: os.Getenv}
}

// Describe 逐条投影官方 CredentialInfo 的三个字段。
//
// 后端多给的字段一律丢掉——官方 projectCredentialInfo 的同一理由：provider
// 的额外属性不许骑到响应里。值根本不在 ModelCredentialView 上，所以这条路径
// 结构上不可能回显密钥。
func (c *catalogCredentialStore) Describe(refs []string) (map[string]OfficialCredentialInfo, error) {
	views, err := c.store.DescribeCredentials(refs, c.env)
	if err != nil {
		return nil, err
	}
	out := make(map[string]OfficialCredentialInfo, len(refs))
	for _, ref := range refs {
		view, ok := views[ref]
		if !ok {
			// 后端漏答一条 = 事实不完整。交给上层 500，不静默显示"未配置"。
			return nil, fmt.Errorf("凭据存储没有返回 %q 的描述", ref)
		}
		out[ref] = OfficialCredentialInfo{
			Configured: view.Configured, Source: view.Source, Writable: view.Writable,
		}
	}
	return out, nil
}

// Set 落一次真实写入。错误串里只有 ref 与形状说明，值一个字符都不带。
func (c *catalogCredentialStore) Set(ref, value string) error {
	_, err := c.store.SetCredential(ref, value, nil)
	return err
}

// Unset 删掉一个引用；本来不存在时是幂等成功（不写盘、不动 revision）。
func (c *catalogCredentialStore) Unset(ref string) error {
	_, err := c.store.DeleteCredential(ref, nil)
	return err
}
