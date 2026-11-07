package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"dshgo/internal/settingsform"
)

// 两个保留键落在 runner 已经读取的 session override 文件中。NUL 不可能出现在
// session id 中，因此不会冒充会话；其它会话键与既有读写路径原样保留。
const (
	settingsDefaultKey  = "\x00settingsFormDefault"
	settingsRevisionKey = "\x00settingsFormRevision"
)

// serveSettingsStore 把 ConfigForm 核心接到既有 model/permission override store。
// Base 是本次 serve 启动 flag；User 是同文件中的默认覆盖，Revision 与其原子提交。
type serveSettingsStore struct {
	models            *modelOverrideStore
	permissions       *permissionOverrideStore
	baseModel         string
	basePolicy        string
	modelLoadErr      error
	permissionLoadErr error
}

// setLoadErrors 在 serve 启动装配时记录既有 store 的真实读盘结果。
// 设置 API 不得把“文件损坏、启动时沿用了空 map”描述成正常默认值。
func (s *serveSettingsStore) setLoadErrors(modelErr, permissionErr error) {
	s.modelLoadErr = modelErr
	s.permissionLoadErr = permissionErr
}

func newServeSettingsStore(baseModel, basePolicy string, models *modelOverrideStore, permissions *permissionOverrideStore) *serveSettingsStore {
	return &serveSettingsStore{models: models, permissions: permissions, baseModel: baseModel, basePolicy: basePolicy}
}

func (s *serveSettingsStore) Read(namespace string) (settingsform.Document, error) {
	switch namespace {
	case settingsform.NamespaceModel:
		if s.models == nil {
			return settingsform.Document{}, fmt.Errorf("model override store 未接线")
		}
		s.models.mu.Lock()
		defer s.models.mu.Unlock()
		if s.modelLoadErr != nil {
			return settingsform.Document{}, fmt.Errorf("既有 model 设置存储读取失败: %w", s.modelLoadErr)
		}
		return readSettingsDocumentLocked(s.models.m, s.baseModel, settingsform.FieldDefaultModel)
	case settingsform.NamespacePermission:
		if s.permissions == nil {
			return settingsform.Document{}, fmt.Errorf("permission override store 未接线")
		}
		s.permissions.mu.Lock()
		defer s.permissions.mu.Unlock()
		if s.permissionLoadErr != nil {
			return settingsform.Document{}, fmt.Errorf("既有 permission 设置存储读取失败: %w", s.permissionLoadErr)
		}
		return readSettingsDocumentLocked(s.permissions.m, s.basePolicy, settingsform.FieldDefaultPreset)
	default:
		return settingsform.Document{}, &settingsform.Error{Code: "not_found", Message: "未知设置 namespace: " + namespace, Hint: "只使用 model 或 permission", Status: http.StatusNotFound}
	}
}

func readSettingsDocumentLocked(values map[string]string, base, field string) (settingsform.Document, error) {
	doc := settingsform.Document{Base: map[string]any{field: base}}
	if raw, ok := values[settingsRevisionKey]; ok {
		revision, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || revision < 0 {
			return settingsform.Document{}, fmt.Errorf("设置 revision 损坏 %q", raw)
		}
		doc.Revision = revision
	}
	if value, ok := values[settingsDefaultKey]; ok {
		doc.User = map[string]any{field: value}
	}
	return doc, nil
}

func (s *serveSettingsStore) Write(namespace string, user map[string]any, expected *int64) (int64, error) {
	switch namespace {
	case settingsform.NamespaceModel:
		if s.models == nil {
			return 0, fmt.Errorf("model override store 未接线")
		}
		s.models.mu.Lock()
		defer s.models.mu.Unlock()
		if s.modelLoadErr != nil {
			return 0, fmt.Errorf("既有 model 设置存储读取失败，拒绝写入: %w", s.modelLoadErr)
		}
		return writeSettingsDocumentLocked(&s.models.m, user, expected, settingsform.NamespaceModel, settingsform.FieldDefaultModel, s.models.flushLocked)
	case settingsform.NamespacePermission:
		if s.permissions == nil {
			return 0, fmt.Errorf("permission override store 未接线")
		}
		s.permissions.mu.Lock()
		defer s.permissions.mu.Unlock()
		if s.permissionLoadErr != nil {
			return 0, fmt.Errorf("既有 permission 设置存储读取失败，拒绝写入: %w", s.permissionLoadErr)
		}
		return writeSettingsDocumentLocked(&s.permissions.m, user, expected, settingsform.NamespacePermission, settingsform.FieldDefaultPreset, s.permissions.flushLocked)
	default:
		return 0, &settingsform.Error{Code: "not_found", Message: "未知设置 namespace: " + namespace, Hint: "只使用 model 或 permission", Status: http.StatusNotFound}
	}
}

// writeSettingsDocumentLocked 在同一个既有 store 锁内检查 revision、改 map、原子写盘。
// flush 失败恢复旧 map，避免 HTTP 报错后内存却显示新值的假成功。
func writeSettingsDocumentLocked(target *map[string]string, user map[string]any, expected *int64, namespace, field string, flush func() error) (int64, error) {
	values := *target
	var revision int64
	if raw, ok := values[settingsRevisionKey]; ok {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return 0, fmt.Errorf("设置 revision 损坏 %q", raw)
		}
		revision = parsed
	}
	if expected != nil && *expected != revision {
		return 0, settingsform.RevisionConflict(namespace, *expected, revision)
	}
	if revision == math.MaxInt64 {
		return 0, fmt.Errorf("%s revision 已溢出", namespace)
	}
	old := make(map[string]string, len(values))
	for key, value := range values {
		old[key] = value
	}
	next := make(map[string]string, len(values)+2)
	for key, value := range values {
		next[key] = value
	}
	if value, ok := user[field]; ok {
		text, ok := value.(string)
		if !ok {
			return 0, fmt.Errorf("%s.%s 持久值不是 string", namespace, field)
		}
		next[settingsDefaultKey] = text
	} else {
		delete(next, settingsDefaultKey)
	}
	revision++
	next[settingsRevisionKey] = strconv.FormatInt(revision, 10)
	*target = next
	if err := flush(); err != nil {
		*target = old
		return 0, err
	}
	return revision, nil
}

// createWithSettingsDefaults 固化“后续新会话默认”语义。总是写有效值（包括
// 尚未显式覆盖的启动默认），因此未来 set/reset 不会 retroactive 改旧会话。
func createWithSettingsDefaults(create func(string) (string, error), settings *serveSettingsStore) func(string) (string, error) {
	return func(mode string) (string, error) {
		id, err := create(mode)
		if err != nil {
			return "", err
		}
		model, err := settings.Read(settingsform.NamespaceModel)
		if err != nil {
			return "", fmt.Errorf("会话 %s 已创建，但读取默认 model 失败: %w", id, err)
		}
		permission, err := settings.Read(settingsform.NamespacePermission)
		if err != nil {
			return "", fmt.Errorf("会话 %s 已创建，但读取默认 permission 失败: %w", id, err)
		}
		modelValue := effectiveSettingsValue(model, settingsform.FieldDefaultModel)
		permissionValue := effectiveSettingsValue(permission, settingsform.FieldDefaultPreset)
		if err := settings.models.Set(id, modelValue); err != nil {
			return "", fmt.Errorf("会话 %s 已创建，但保存默认 model 失败: %w", id, err)
		}
		if err := settings.permissions.Set(id, permissionValue); err != nil {
			return "", fmt.Errorf("会话 %s 已创建，但保存默认 permission 失败: %w", id, err)
		}
		return id, nil
	}
}

func effectiveSettingsValue(doc settingsform.Document, field string) string {
	if value, ok := doc.User[field].(string); ok {
		return value
	}
	value, _ := doc.Base[field].(string)
	return value
}

// settingsParams 是所有 additive HTTP 裸参数的并集；每个方法另有键白名单。
type settingsParams struct {
	NS               string                `json:"ns,omitempty"`
	Ops              []settingsform.PathOp `json:"ops,omitempty"`
	ExpectedRevision *int64                `json:"expectedRevision,omitempty"`
	Path             *[]string             `json:"path,omitempty"`
	Value            any                   `json:"value,omitempty"`
}

type settingsEnvelope struct {
	Result any                 `json:"result,omitempty"`
	Error  *settingsform.Error `json:"error,omitempty"`
}

var settingsMethodParams = map[string][]string{
	"settings.describe":      {},
	"settings.mutate":        {"ns", "ops", "expectedRevision"},
	"settings.form.describe": {},
	"settings.form.get":      {"ns"},
	"settings.form.set":      {"ns", "path", "value", "expectedRevision"},
	"settings.form.reset":    {"ns", "path", "expectedRevision"},
}

func newSettingsFormHTTPHandler(store *serveSettingsStore) http.Handler {
	service := settingsform.New(store)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		allowed, ok := settingsMethodParams[method]
		if r.Method != http.MethodPost || !ok || "/api/"+method != r.URL.Path {
			writeSettingsFormError(w, &settingsform.Error{Code: "not_found", Message: "未知设置 Transport 方法", Hint: "检查 /api/settings.* 路径与 POST 方法", Status: http.StatusNotFound})
			return
		}
		var params settingsParams
		if err := decodeSettingsParams(r, method, allowed, &params); err != nil {
			writeSettingsFormError(w, err)
			return
		}
		if method != "settings.describe" && method != "settings.form.describe" && params.NS == "" {
			writeSettingsFormError(w, &settingsform.Error{Code: "bad_request", Message: method + " 缺 ns", Hint: "ns 必须是 describe 返回的 model 或 permission", Status: http.StatusBadRequest})
			return
		}
		var result any
		var err error
		switch method {
		case "settings.describe", "settings.form.describe":
			result, err = service.Describe()
		case "settings.form.get":
			result, err = service.Get(params.NS)
		case "settings.mutate":
			var mutation settingsform.MutationResult
			mutation, err = service.Mutate(params.NS, params.Ops, params.ExpectedRevision)
			result = mutation.Namespace
		case "settings.form.set":
			if params.Path == nil {
				err = &settingsform.Error{Code: "invalid_path", Message: "settings.form.set 缺 path", Hint: "path 必须是 [field]", Status: http.StatusBadRequest}
				break
			}
			var mutation settingsform.MutationResult
			mutation, err = service.Mutate(params.NS, []settingsform.PathOp{{Op: "set", Path: *params.Path, Value: params.Value}}, params.ExpectedRevision)
			result = mutation.Section
		case "settings.form.reset":
			var path []string
			if params.Path != nil {
				path = *params.Path
			}
			var mutation settingsform.MutationResult
			mutation, err = service.Reset(params.NS, path, params.ExpectedRevision)
			result = mutation.Section
		}
		if err != nil {
			writeSettingsFormError(w, err)
			return
		}
		if result == nil {
			writeSettingsFormError(w, fmt.Errorf("%s 没有返回设置描述符", method))
			return
		}
		writeSettingsFormJSON(w, http.StatusOK, settingsEnvelope{Result: result})
	})
}

func decodeSettingsParams(r *http.Request, method string, allowed []string, target *settingsParams) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return &settingsform.Error{Code: "bad_request", Message: "读取设置参数失败: " + err.Error(), Hint: "直接发送该方法定义的 JSON 裸对象", Status: http.StatusBadRequest}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return &settingsform.Error{Code: "bad_request", Message: method + " 参数必须是 JSON 裸对象", Hint: "不要包 params，也不要发送数组或 null", Status: http.StatusBadRequest}
	}
	if _, wrapped := fields["params"]; wrapped {
		return &settingsform.Error{Code: "bad_request", Message: method + " 不接受 params 包装", Hint: "把参数直接放在顶层 JSON 对象中", Status: http.StatusBadRequest}
	}
	whitelist := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		whitelist[key] = true
	}
	for key := range fields {
		if !whitelist[key] {
			return &settingsform.Error{Code: "bad_request", Message: method + " 不接受参数 " + key, Hint: "只发送 settings-form.snapshot.json 中此 method 列出的参数", Status: http.StatusBadRequest}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &settingsform.Error{Code: "bad_request", Message: method + " 参数类型错误: " + err.Error(), Hint: "检查 ns/op/path/value/expectedRevision 的 JSON 类型", Status: http.StatusBadRequest}
	}
	if rawPath, exists := fields["path"]; exists && bytes.Equal(bytes.TrimSpace(rawPath), []byte("null")) {
		return &settingsform.Error{Code: "invalid_path", Message: method + " 的 path 不能为 null", Hint: "reset 省略 path 才清整个 namespace；否则传 [field]", Status: http.StatusBadRequest}
	}
	if method == "settings.mutate" {
		rawOps, exists := fields["ops"]
		if !exists || bytes.Equal(bytes.TrimSpace(rawOps), []byte("null")) {
			return &settingsform.Error{Code: "bad_request", Message: "settings.mutate 的 ops 必须是数组，不能省略或 null", Hint: "无字段要改时发送空数组 []，不要发 null", Status: http.StatusBadRequest}
		}
	}
	return nil
}

func writeSettingsFormError(w http.ResponseWriter, err error) {
	apiErr := settingsform.AsError(err)
	status := apiErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeSettingsFormJSON(w, status, settingsEnvelope{Error: apiErr})
}

func writeSettingsFormJSON(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		fallback := &settingsform.Error{Code: "internal", Message: err.Error(), Hint: "检查宿主设置响应序列化错误", Retryable: true}
		raw, _ = json.Marshal(settingsEnvelope{Error: fallback})
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}
