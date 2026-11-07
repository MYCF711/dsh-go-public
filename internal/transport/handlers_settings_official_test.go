package transport

// 官方桌面端 0.2.2-rc.1「settings / credentials」5 个方法的契约测试。
//
// 与官方 agent 侧测试同一口径：这些测试只打本包的
// `serveOfficialSettingsCredentialAPI`，**不依赖 server.go 的分发表**——
// 路由接线是 Lead 的验收项；这里用 `IsOfficialSettingsCredentialMethod`
// 钉住"方法名集合"，避免新增方法从路由漏下去。
//
// 真实优先：settings 侧走 `settingsform.Service` + 一个**会落盘**的
// Store（写临时 home，重开一次读回），credentials 侧走
// `llm.OpenModelCatalog`（真实 model-catalog.json）。这样"只改内存"、
// "假密钥库"两种假实现都过不了这里的断言。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"net/http"
	"net/http/httptest"

	"dshgo/internal/llm"
	"dshgo/internal/settingsform"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// testSettingsStore 是 settingsform.Store 的真实落盘实现：
// 每次 Write 在同一个锁内检查 expectedRevision → 改文档 → 原子写盘，
// 写盘失败把内存旧值放回去（与 cmd/dshd 的既有 store 同语义）。
type testSettingsStore struct {
	mu       sync.Mutex
	path     string
	docs     map[string]settingsform.Document
	writeErr error
	writes   int
}

func newTestSettingsStore(t *testing.T, base map[string]settingsform.Document) *testSettingsStore {
	t.Helper()
	store := &testSettingsStore{path: filepath.Join(t.TempDir(), "settings.json"), docs: map[string]settingsform.Document{}}
	for ns, doc := range base {
		doc.Base = cloneTestMap(doc.Base)
		doc.User = cloneTestMap(doc.User)
		store.docs[ns] = doc
	}
	if err := store.persistLocked(); err != nil {
		t.Fatalf("初始落盘: %v", err)
	}
	return store
}

// reopenTestSettingsStore 用同一个文件重开一份 store，证明写是真的落了盘。
func reopenTestSettingsStore(t *testing.T, path string) *testSettingsStore {
	t.Helper()
	store := &testSettingsStore{path: path, docs: map[string]settingsform.Document{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("重开读盘: %v", err)
	}
	var docs map[string]settingsform.Document
	if err := json.Unmarshal(raw, &docs); err != nil {
		t.Fatalf("重开解析: %v", err)
	}
	store.docs = docs
	return store
}

func (s *testSettingsStore) persistLocked() error {
	raw, err := json.Marshal(s.docs)
	if err != nil {
		return err
	}
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, s.path)
}

func (s *testSettingsStore) Read(namespace string) (settingsform.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[namespace]
	if !ok {
		return settingsform.Document{}, &settingsform.Error{Code: "not_found", Message: "未知设置 namespace: " + namespace, Hint: "只使用 settings.describe 返回的 ns", Status: 404}
	}
	return settingsform.Document{Base: cloneTestMap(doc.Base), User: cloneTestMap(doc.User), Revision: doc.Revision}, nil
}

func (s *testSettingsStore) Write(namespace string, user map[string]any, expected *int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	doc, ok := s.docs[namespace]
	if !ok {
		return 0, &settingsform.Error{Code: "not_found", Message: "未知设置 namespace: " + namespace, Hint: "只使用 settings.describe 返回的 ns", Status: 404}
	}
	if expected != nil && *expected != doc.Revision {
		return 0, settingsform.RevisionConflict(namespace, *expected, doc.Revision)
	}
	old := doc
	doc.Revision++
	doc.User = cloneTestMap(user)
	s.docs[namespace] = doc
	if err := s.persistLocked(); err != nil {
		s.docs[namespace] = old
		return 0, err
	}
	return doc.Revision, nil
}

func (s *testSettingsStore) document(t *testing.T, namespace string) settingsform.Document {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.docs[namespace]
	return settingsform.Document{Base: cloneTestMap(doc.Base), User: cloneTestMap(doc.User), Revision: doc.Revision}
}

func cloneTestMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func defaultTestSettings() map[string]settingsform.Document {
	return map[string]settingsform.Document{
		"permission": {Base: map[string]any{"defaultPreset": "allow"}},
		"model":      {Base: map[string]any{"defaultModel": "base-model"}},
	}
}

// newOfficialSettingsServer 挂上官方 settings/credentials 实现的最小 Server。
func newOfficialSettingsServer(t *testing.T, api OfficialSettingsCredentialAPI) *Server {
	t.Helper()
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.AttachOfficialSettingsCredentialAPI(api)
	t.Cleanup(srv.DetachOfficialSettingsCredentialAPI)
	return srv
}

// callOfficialSettings 打一次方法，返回状态码与解码后的信封。
func callOfficialSettings(t *testing.T, srv *Server, method string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case nil:
		raw = nil
	case string:
		raw = []byte(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		raw = encoded
	}
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	srv.serveOfficialSettingsCredentialAPI(rec, req, method)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
	}
	return rec.Code, out
}

func officialResult(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("成功信封缺 result 对象：%v", out)
	}
	return result
}

// ---------------------------------------------------------------------------
// 方法集合：钉住名字，防止新增/漏接
// ---------------------------------------------------------------------------

func TestOfficialSettingsCredentialMethodsAreStable(t *testing.T) {
	methods := OfficialSettingsCredentialMethods()
	if len(methods) != 5 {
		t.Fatalf("官方 settings/credentials 方法应是 5 个，实际 %d：%v", len(methods), methods)
	}
	want := []string{
		"credentials.describe", "credentials.set", "credentials.unset",
		"settings.replace", "settings.update",
	}
	got := make([]string, 0, len(methods))
	seen := map[Method]bool{}
	for _, method := range methods {
		if seen[method] {
			t.Fatalf("方法重复：%s", method)
		}
		seen[method] = true
		if !IsOfficialSettingsCredentialMethod(string(method)) {
			t.Fatalf("%s 没被 IsOfficialSettingsCredentialMethod 认出来（路由会漏）", method)
		}
		if strings.Contains(string(method), "/") {
			t.Fatalf("%s 含 '/'：本仓方法名用点号", method)
		}
		got = append(got, string(method))
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("方法集合不一致\n got=%v\nwant=%v", got, want)
	}
	// 既有方法不许被本文件"顺手"接管：settings.describe/mutate 已有实现。
	for _, other := range []string{
		"settings.describe", "settings.mutate", "settings.form.set", "credentials", "credentials.describe2", "",
	} {
		if IsOfficialSettingsCredentialMethod(other) {
			t.Fatalf("%q 不该被当成官方 settings/credentials 方法", other)
		}
	}
}

// 没挂实现时每一条都必须是四字段 501，并点出缺什么。
func TestOfficialSettingsCredentialNotAttachedIsLoud(t *testing.T) {
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{})
	bodies := map[Method]any{
		MethodSettingsUpdate:      map[string]any{"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"}},
		MethodSettingsReplace:     map[string]any{"ns": "permission", "section": map[string]any{}},
		MethodCredentialsDescribe: map[string]any{"refs": []string{"DSH_KEY"}},
		MethodCredentialsSet:      map[string]any{"ref": "DSH_KEY", "value": "x"},
		MethodCredentialsUnset:    map[string]any{"ref": "DSH_KEY"},
	}
	for _, method := range OfficialSettingsCredentialMethods() {
		code, out := callOfficialSettings(t, srv, string(method), bodies[method])
		assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
		msg := out["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, string(method)) {
			t.Fatalf("%s 的未实现错误没点出方法名：%q", method, msg)
		}
		if !strings.Contains(msg, "未接线") && !strings.Contains(msg, "没有") {
			t.Fatalf("%s 的未实现错误没说清缺什么：%q", method, msg)
		}
	}
}

// 挂了一半实现时：缺的那一半仍然是 501，不许用另一半的实现冒充。
func TestOfficialSettingsCredentialPartialMountIsLoud(t *testing.T) {
	store := newTestSettingsStore(t, defaultTestSettings())
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{Settings: settingsform.New(store)})
	code, out := callOfficialSettings(t, srv, "credentials.describe", map[string]any{"refs": []string{"DSH_KEY"}})
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	msg := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "credential") && !strings.Contains(msg, "凭据") {
		t.Fatalf("credentials 未接线的错误没点出缺凭据后端：%q", msg)
	}

	srv2 := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{})
	// 只挂凭据、没挂 settings：settings.update 必须 501 而不是 500/静默。
	code, out = callOfficialSettings(t, srv2, "settings.update", map[string]any{
		"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"},
	})
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
}

// ---------------------------------------------------------------------------
// settings.update
// ---------------------------------------------------------------------------

func TestOfficialSettingsUpdateMergesIntoRealDocument(t *testing.T) {
	store := newTestSettingsStore(t, defaultTestSettings())
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{Settings: settingsform.New(store)})

	code, out := callOfficialSettings(t, srv, "settings.update", map[string]any{
		"ns":               "permission",
		"patch":            map[string]any{"defaultPreset": "deny"},
		"expectedRevision": 0,
	})
	if code != http.StatusOK {
		t.Fatalf("settings.update 状态 %d：%v", code, out)
	}
	view := officialResult(t, out)
	// 官方 SettingsNamespaceView 的必填字段逐个点名。
	for _, key := range []string{"ns", "autoGenerate", "schema", "value", "applies", "secrets", "revision"} {
		if _, ok := view[key]; !ok {
			t.Fatalf("结果缺官方必填字段 %q：%v", key, view)
		}
	}
	if view["ns"] != "permission" {
		t.Fatalf("ns=%v", view["ns"])
	}
	if view["applies"] != "live" {
		t.Fatalf("applies=%v，官方字面量是 live", view["applies"])
	}
	if _, ok := view["secrets"].([]any); !ok {
		t.Fatalf("secrets 必须是数组（官方是对象数组，空也要是 []）：%v", view["secrets"])
	}
	value, _ := view["value"].(map[string]any)
	if value["defaultPreset"] != "deny" {
		t.Fatalf("value 没反映新值：%v", value)
	}
	user, _ := view["user"].(map[string]any)
	if user["defaultPreset"] != "deny" {
		t.Fatalf("user 段没写进去：%v", view["user"])
	}
	if view["revision"] != float64(1) {
		t.Fatalf("revision=%v，期望 1", view["revision"])
	}

	// 真实落盘：重开 store 读回同一个 revision 与 user。
	reopened := newTestSettingsStoreFromPath(t, store.path)
	doc := reopened.document(t, "permission")
	if doc.Revision != 1 || doc.User["defaultPreset"] != "deny" {
		t.Fatalf("落盘后的文档不对：%+v", doc)
	}
	if doc.Base["defaultPreset"] != "allow" {
		t.Fatalf("base 被写坏了：%+v", doc)
	}
}

// newTestSettingsStoreFromPath 用已有文件重开，模拟进程重启。
func newTestSettingsStoreFromPath(t *testing.T, path string) *testSettingsStore {
	t.Helper()
	return reopenTestSettingsStore(t, path)
}

func TestOfficialSettingsUpdateRejectsStaleRevision(t *testing.T) {
	store := newTestSettingsStore(t, defaultTestSettings())
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{Settings: settingsform.New(store)})

	if code, out := callOfficialSettings(t, srv, "settings.update", map[string]any{
		"ns": "permission", "patch": map[string]any{"defaultPreset": "ask"},
	}); code != http.StatusOK {
		t.Fatalf("第一次写失败 %d：%v", code, out)
	}
	mid := store.document(t, "permission").Revision
	code, out := callOfficialSettings(t, srv, "settings.update", map[string]any{
		"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"}, "expectedRevision": 0,
	})
	assertFourFieldError(t, code, out, http.StatusConflict, "revision_conflict")
	msg := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "expected") || !strings.Contains(msg, "actual") {
		t.Fatalf("冲突消息没带 expected/actual：%q", msg)
	}
	// 冲突不许写：revision 没动，值也没被覆盖。
	doc := store.document(t, "permission")
	if doc.Revision != mid || doc.User["defaultPreset"] != "ask" {
		t.Fatalf("冲突后文档被改了：%+v", doc)
	}
}

func TestOfficialSettingsUpdateFailLoudOnUnknownFieldAndBadType(t *testing.T) {
	store := newTestSettingsStore(t, defaultTestSettings())
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{Settings: settingsform.New(store)})

	code, out := callOfficialSettings(t, srv, "settings.update", map[string]any{
		"ns": "permission", "patch": map[string]any{"nope": "x"},
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	if msg, _ := out["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "nope") {
		t.Fatalf("未知字段的错误没点出字段名：%q", msg)
	}
	if store.document(t, "permission").Revision != 0 {
		t.Fatalf("未知字段也写了存储")
	}

	code, out = callOfficialSettings(t, srv, "settings.update", map[string]any{
		"ns": "permission", "patch": map[string]any{"defaultPreset": 42},
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")

	// 未知 ns 是 404 家族的 fail-loud，不许当成功。
	code, out = callOfficialSettings(t, srv, "settings.update", map[string]any{
		"ns": "nope", "patch": map[string]any{"defaultPreset": "deny"},
	})
	if code != http.StatusNotFound {
		t.Fatalf("未知 ns 状态 %d：%v", code, out)
	}
	if store.document(t, "permission").Revision != 0 {
		t.Fatalf("无效请求写了存储")
	}
}

func TestOfficialSettingsUpdateParamContract(t *testing.T) {
	store := newTestSettingsStore(t, defaultTestSettings())
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{Settings: settingsform.New(store)})

	cases := []struct {
		name string
		body any
	}{
		{"空请求体", ""},
		{"缺 ns", map[string]any{"patch": map[string]any{"defaultPreset": "deny"}}},
		{"ns 是空串", map[string]any{"ns": "", "patch": map[string]any{"defaultPreset": "deny"}}},
		{"缺 patch", map[string]any{"ns": "permission"}},
		{"patch 是数组", map[string]any{"ns": "permission", "patch": []any{}}},
		{"patch 是 null", map[string]any{"ns": "permission", "patch": nil}},
		{"未知字段", map[string]any{"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"}, "extra": 1}},
		{"expectedRevision 是小数", map[string]any{"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"}, "expectedRevision": 1.5}},
		{"expectedRevision 是负数", map[string]any{"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"}, "expectedRevision": -1}},
		{"expectedRevision 是 null", map[string]any{"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"}, "expectedRevision": nil}},
	}
	for _, testCase := range cases {
		code, out := callOfficialSettings(t, srv, "settings.update", testCase.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s：状态 %d，期望 400（body=%v）", testCase.name, code, out)
		}
		assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	}
	if store.document(t, "permission").Revision != 0 {
		t.Fatalf("无效参数写了存储")
	}
}

func TestOfficialSettingsUpdateWriteFailureIsInternalAndKeepsState(t *testing.T) {
	store := newTestSettingsStore(t, defaultTestSettings())
	svc := settingsform.New(store)
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{Settings: svc})

	store.mu.Lock()
	store.writeErr = os.ErrPermission
	store.mu.Unlock()
	code, out := callOfficialSettings(t, srv, "settings.update", map[string]any{
		"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"},
	})
	assertFourFieldError(t, code, out, http.StatusInternalServerError, "internal")
	if store.document(t, "permission").Revision != 0 || store.document(t, "permission").User != nil {
		t.Fatalf("写失败后内存里留下了新值：%+v", store.document(t, "permission"))
	}

	store.mu.Lock()
	store.writeErr = nil
	store.mu.Unlock()
	if code, out := callOfficialSettings(t, srv, "settings.update", map[string]any{
		"ns": "permission", "patch": map[string]any{"defaultPreset": "deny"},
	}); code != http.StatusOK {
		t.Fatalf("清掉故障后仍写不进去 %d：%v", code, out)
	}
	if store.document(t, "permission").User["defaultPreset"] != "deny" {
		t.Fatalf("恢复后的写入没落盘")
	}
}

// ---------------------------------------------------------------------------
// settings.replace
// ---------------------------------------------------------------------------

func TestOfficialSettingsReplaceWholesaleAndClears(t *testing.T) {
	store := newTestSettingsStore(t, defaultTestSettings())
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{Settings: settingsform.New(store)})

	code, out := callOfficialSettings(t, srv, "settings.replace", map[string]any{
		"ns": "model", "section": map[string]any{"defaultModel": "next-model"},
	})
	if code != http.StatusOK {
		t.Fatalf("settings.replace 状态 %d：%v", code, out)
	}
	view := officialResult(t, out)
	if user, _ := view["user"].(map[string]any); user["defaultModel"] != "next-model" {
		t.Fatalf("replace 没整段写进去：%v", view["user"])
	}
	if view["revision"] != float64(1) {
		t.Fatalf("revision=%v", view["revision"])
	}

	// 整段替换（不是合并）：只给一个字段时，之前多出来的字段必须消失。
	code, out = callOfficialSettings(t, srv, "settings.replace", map[string]any{
		"ns": "model", "section": map[string]any{},
	})
	if code != http.StatusOK {
		t.Fatalf("清空 replace 状态 %d：%v", code, out)
	}
	view = officialResult(t, out)
	if _, has := view["user"]; has {
		t.Fatalf("清空后 user 段还在：%v", view["user"])
	}
	value, _ := view["value"].(map[string]any)
	if value["defaultModel"] != "base-model" {
		t.Fatalf("清空后应回落到 base：%v", value)
	}
	if view["revision"] != float64(2) {
		t.Fatalf("清空也是一次真实写入，revision=%v 期望 2", view["revision"])
	}

	code, out = callOfficialSettings(t, srv, "settings.replace", map[string]any{
		"ns": "model", "section": map[string]any{"defaultModel": "stale"}, "expectedRevision": 1,
	})
	assertFourFieldError(t, code, out, http.StatusConflict, "revision_conflict")
}

func TestOfficialSettingsReplaceParamContract(t *testing.T) {
	store := newTestSettingsStore(t, defaultTestSettings())
	srv := newOfficialSettingsServer(t, OfficialSettingsCredentialAPI{Settings: settingsform.New(store)})
	for _, body := range []any{
		map[string]any{"section": map[string]any{}},
		map[string]any{"ns": "model"},
		map[string]any{"ns": "model", "section": map[string]any{"defaultModel": "x"}, "patch": map[string]any{}},
		map[string]any{"ns": "model", "section": map[string]any{"unknown": "x"}},
	} {
		code, out := callOfficialSettings(t, srv, "settings.replace", body)
		if code != http.StatusBadRequest {
			t.Fatalf("body=%v 状态 %d，期望 400（%v）", body, code, out)
		}
		assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	}
}

// ---------------------------------------------------------------------------
// credentials.describe
// ---------------------------------------------------------------------------

// newCatalogServer 造真实 llm catalog（临时 home 上的 model-catalog.json）。
func newCatalogServer(t *testing.T, home string) (*Server, *llm.ModelCatalogStore) {
	t.Helper()
	store := llm.OpenModelCatalog(home)
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.AttachOfficialSettingsCredentialAPI(OfficialSettingsCredentialAPI{
		Credentials: NewCatalogCredentialStore(store),
	})
	t.Cleanup(srv.DetachOfficialSettingsCredentialAPI)
	return srv, store
}

// seedCatalogModel 往目录里种一个引用 apiKeyEnv 的真实模型条目。
func seedCatalogModel(t *testing.T, store *llm.ModelCatalogStore, id, ref string) {
	t.Helper()
	raw := json.RawMessage(`{"id":"` + id + `","api":"openai-completions","baseURL":"https://api.example.com","apiKeyEnv":"` + ref + `"}`)
	if _, err := store.Upsert(raw, nil, nil); err != nil {
		t.Fatalf("种模型失败: %v", err)
	}
}

func TestOfficialCredentialsDescribeIsMasked(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_OFFICIAL_TEST_KEY")

	code, out := callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{"DSH_OFFICIAL_TEST_KEY", "DSH_NEVER_SET"},
	})
	if code != http.StatusOK {
		t.Fatalf("credentials.describe 状态 %d：%v", code, out)
	}
	views := officialResult(t, out)
	configured, _ := views["DSH_OFFICIAL_TEST_KEY"].(map[string]any)
	if configured == nil {
		t.Fatalf("结果缺请求的 ref：%v", views)
	}
	if configured["configured"] != false {
		t.Fatalf("还没写过就报 configured=true：%v", configured)
	}
	if configured["writable"] != true {
		t.Fatalf("被模型引用的 ref 应可写：%v", configured)
	}
	missing, _ := views["DSH_NEVER_SET"].(map[string]any)
	if missing == nil || missing["configured"] != false {
		t.Fatalf("未配置的 ref 描述不对：%v", views)
	}
	if _, has := configured["value"]; has {
		t.Fatalf("describe 回了 value 键：%v", configured)
	}
}

func TestOfficialCredentialsDescribeParamContract(t *testing.T) {
	home := t.TempDir()
	srv, _ := newCatalogServer(t, home)

	// 官方：refs 是 string[]，最多 64 个，名字必须匹配 ^[A-Za-z_][A-Za-z0-9_]*$。
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = "DSH_KEY_" + strings.Repeat("A", 1)
	}
	for _, body := range []any{
		map[string]any{},
		map[string]any{"refs": "DSH_KEY"},
		map[string]any{"refs": []any{"DSH_KEY", 1}},
		map[string]any{"refs": []string{"1BAD"}},
		map[string]any{"refs": []string{"BAD-NAME"}},
		map[string]any{"refs": tooMany},
		map[string]any{"refs": []string{"DSH_KEY"}, "extra": true},
	} {
		code, out := callOfficialSettings(t, srv, "credentials.describe", body)
		if code != http.StatusBadRequest {
			t.Fatalf("body=%v 状态 %d，期望 400（%v）", body, code, out)
		}
		assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	}

	// 重复 ref 合法：结果是按名字的对象，天然去重。
	code, out := callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{"DSH_KEY", "DSH_KEY"},
	})
	if code != http.StatusOK {
		t.Fatalf("重复 ref 状态 %d：%v", code, out)
	}
	if views := officialResult(t, out); len(views) != 1 {
		t.Fatalf("重复 ref 没去重：%v", views)
	}
}

// 名字与 "result"/"error" 相撞时，成功信封仍然只能是 result。
func TestOfficialCredentialsDescribeKeepsEnvelopeForTrickyRefs(t *testing.T) {
	home := t.TempDir()
	srv, _ := newCatalogServer(t, home)
	code, out := callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{"result", "error"},
	})
	if code != http.StatusOK {
		t.Fatalf("状态 %d：%v", code, out)
	}
	views := officialResult(t, out)
	if len(views) != 2 {
		t.Fatalf("result/error 两个 ref 没进来：%v", out)
	}
	for _, ref := range []string{"result", "error"} {
		if views[ref] == nil {
			t.Fatalf("ref %q 丢了：%v", ref, views)
		}
	}
}

// ---------------------------------------------------------------------------
// credentials.set / unset
// ---------------------------------------------------------------------------

func TestOfficialCredentialsSetPersistsAndNeverLeaks(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_OFFICIAL_TEST_KEY")
	const secret = "sk-official-2f9c-never-echo"

	code, out := callOfficialSettings(t, srv, "credentials.set", map[string]any{
		"ref": "DSH_OFFICIAL_TEST_KEY", "value": secret,
	})
	if code != http.StatusOK {
		t.Fatalf("credentials.set 状态 %d：%v", code, out)
	}
	// 任何响应都不得回显密钥：整份 body 里不许出现它。
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("set 响应回显了密钥：%s", raw)
	}

	// 真值落既有凭据存储：同一个 home 新开一份 catalog 也要看到 configured。
	reopened := llm.OpenModelCatalog(home)
	view, err := reopened.List()
	if err != nil {
		t.Fatalf("重开目录: %v", err)
	}
	status, ok := view.Credentials["DSH_OFFICIAL_TEST_KEY"]
	if !ok || !status.Configured || !status.Writable {
		t.Fatalf("凭据没落进目录：%+v", view.Credentials)
	}

	// 再经 Transport 确认一次：describe 只说有没有，不说是什么。
	code, out = callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{"DSH_OFFICIAL_TEST_KEY"},
	})
	if code != http.StatusOK {
		t.Fatalf("describe 状态 %d：%v", code, out)
	}
	views := officialResult(t, out)
	info, _ := views["DSH_OFFICIAL_TEST_KEY"].(map[string]any)
	if info["configured"] != true {
		t.Fatalf("写完后 describe 仍说未配置：%v", info)
	}
	raw, _ = json.Marshal(out)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("describe 回显了密钥：%s", raw)
	}
}

func TestOfficialCredentialsSetParamContract(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_OFFICIAL_TEST_KEY")

	for _, body := range []any{
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY"},
		map[string]any{"value": "x"},
		map[string]any{"ref": "", "value": "x"},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": ""},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": "   "},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": 42},
		map[string]any{"ref": "BAD-REF", "value": "x"},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": " sk-padded "},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": "x", "extra": 1},
	} {
		code, out := callOfficialSettings(t, srv, "credentials.set", body)
		if code != http.StatusBadRequest {
			t.Fatalf("body=%v 状态 %d，期望 400（%v）", body, code, out)
		}
		assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	}

	// 值里带 NUL：错误串里不许出现这个值本身。
	const bad = "sk-\x00-should-not-leak"
	code, out := callOfficialSettings(t, srv, "credentials.set", map[string]any{
		"ref": "DSH_OFFICIAL_TEST_KEY", "value": bad,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("NUL 值状态 %d，期望 400（%v）", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), bad) || strings.Contains(string(raw), "sk-") {
		t.Fatalf("错误响应泄漏了值：%s", raw)
	}
}

// 游离 ref（没有任何 catalog 模型引用它）必须能直接设置：官方
// `credentials/set` 的语义是"按引用名存一个值"，不要求这个引用先被某条模型
// 声明。写进去之后 describe 必须报 configured + writable，值落盘、可重开，
// 并且不能被 `models.catalog.upsert` 的孤儿清理顺手删掉。
func TestOfficialCredentialsSetFreeRefPersists(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_OFFICIAL_TEST_KEY")
	const freeRef = "DSH_NO_MODEL_USES_ME"
	const secret = "sk-free-ref-secret-4c71"

	code, out := callOfficialSettings(t, srv, "credentials.set", map[string]any{
		"ref": freeRef, "value": secret,
	})
	if code != http.StatusOK {
		t.Fatalf("游离 ref 仍然写不进去：状态 %d（%v）", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("set 响应回显了密钥：%s", raw)
	}

	// describe：能解析到值、来源是本地凭据存储、可写。
	code, out = callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{freeRef},
	})
	if code != http.StatusOK {
		t.Fatalf("describe 状态 %d：%v", code, out)
	}
	info, _ := officialResult(t, out)[freeRef].(map[string]any)
	if info["configured"] != true || info["writable"] != true || info["source"] != "catalog" {
		t.Fatalf("游离 ref 的 describe 不对：%v", info)
	}

	// 真落盘：同一个 home 新开一份目录也要看到它。
	view, err := llm.OpenModelCatalog(home).List()
	if err != nil {
		t.Fatalf("重开目录: %v", err)
	}
	if status, ok := view.Credentials[freeRef]; !ok || !status.Configured {
		t.Fatalf("游离 ref 没落进凭据存储：%+v", view.Credentials)
	}

	// 与 models.catalog.upsert 共存：一次无关的模型写入不许把它当孤儿删掉。
	seedCatalogModel(t, store, "another-model", "DSH_ANOTHER_KEY")
	view, err = store.List()
	if err != nil {
		t.Fatalf("再读目录: %v", err)
	}
	if status, ok := view.Credentials[freeRef]; !ok || !status.Configured {
		t.Fatalf("models.catalog.upsert 把游离 ref 清掉了：%+v", view.Credentials)
	}

	// unset 之后必须干净地消失。
	if code, out := callOfficialSettings(t, srv, "credentials.unset", map[string]any{"ref": freeRef}); code != http.StatusOK {
		t.Fatalf("unset 游离 ref 状态 %d：%v", code, out)
	}
	view, err = llm.OpenModelCatalog(home).List()
	if err != nil {
		t.Fatalf("三次读目录: %v", err)
	}
	if status, ok := view.Credentials[freeRef]; ok && status.Configured {
		t.Fatalf("unset 之后游离 ref 仍然 configured：%+v", status)
	}
}

func TestOfficialCredentialsUnsetClearsStoredValue(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_OFFICIAL_TEST_KEY")
	const secret = "sk-official-unset-71ab"

	if code, out := callOfficialSettings(t, srv, "credentials.set", map[string]any{
		"ref": "DSH_OFFICIAL_TEST_KEY", "value": secret,
	}); code != http.StatusOK {
		t.Fatalf("set 状态 %d：%v", code, out)
	}
	code, out := callOfficialSettings(t, srv, "credentials.unset", map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY"})
	if code != http.StatusOK {
		t.Fatalf("credentials.unset 状态 %d：%v", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("unset 响应回显了密钥：%s", raw)
	}
	view, err := llm.OpenModelCatalog(home).List()
	if err != nil {
		t.Fatalf("重开目录: %v", err)
	}
	if status, ok := view.Credentials["DSH_OFFICIAL_TEST_KEY"]; ok && status.Configured {
		t.Fatalf("unset 之后目录里仍然 configured：%+v", status)
	}

	code, out = callOfficialSettings(t, srv, "credentials.unset", map[string]any{"ref": "BAD-REF"})
	if code != http.StatusBadRequest {
		t.Fatalf("非法 ref 状态 %d，期望 400（%v）", code, out)
	}
	// 未引用/不存在的 ref：干净地报告为已不存在，而不是 500。
	code, out = callOfficialSettings(t, srv, "credentials.unset", map[string]any{"ref": "DSH_NO_MODEL_USES_ME"})
	if code != http.StatusBadRequest && code != http.StatusOK {
		t.Fatalf("清理未引用 ref 状态 %d：%v", code, out)
	}
}

// ---------------------------------------------------------------------------
// A/B home 隔离
// ---------------------------------------------------------------------------

func TestOfficialSettingsCredentialHomesAreIsolated(t *testing.T) {
	homeA, homeB := t.TempDir(), t.TempDir()
	srvA, storeA := newCatalogServer(t, homeA)
	srvB, storeB := newCatalogServer(t, homeB)
	seedCatalogModel(t, storeA, "model-a", "DSH_KEY_A")
	seedCatalogModel(t, storeB, "model-b", "DSH_KEY_A")

	if code, out := callOfficialSettings(t, srvA, "credentials.set", map[string]any{
		"ref": "DSH_KEY_A", "value": "sk-home-a-only",
	}); code != http.StatusOK {
		t.Fatalf("A 写凭据状态 %d：%v", code, out)
	}
	code, out := callOfficialSettings(t, srvB, "credentials.describe", map[string]any{"refs": []string{"DSH_KEY_A"}})
	if code != http.StatusOK {
		t.Fatalf("B describe 状态 %d：%v", code, out)
	}
	if info, _ := officialResult(t, out)["DSH_KEY_A"].(map[string]any); info["configured"] != false {
		t.Fatalf("A 的凭据漏到了 B：%v", info)
	}
	// B 的目录里也不许出现 A 的模型。
	viewB, err := llm.OpenModelCatalog(homeB).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(viewB.Models) != 1 {
		t.Fatalf("B 的模型数不对：%d", len(viewB.Models))
	}

	// 游离 ref 同样按 home 隔离：A 里没有模型引用它，更不该出现在 B 的存储里。
	const freeSecret = "sk-free-home-a-only"
	if code, out := callOfficialSettings(t, srvA, "credentials.set", map[string]any{
		"ref": "DSH_FREE_A_ONLY", "value": freeSecret,
	}); code != http.StatusOK {
		t.Fatalf("A 写游离凭据状态 %d：%v", code, out)
	}
	code, out = callOfficialSettings(t, srvB, "credentials.describe", map[string]any{"refs": []string{"DSH_FREE_A_ONLY"}})
	if code != http.StatusOK {
		t.Fatalf("B describe 游离 ref 状态 %d：%v", code, out)
	}
	if info, _ := officialResult(t, out)["DSH_FREE_A_ONLY"].(map[string]any); info["configured"] != false {
		t.Fatalf("A 的游离凭据漏到了 B：%v", info)
	}
	rawB, err := os.ReadFile(filepath.Join(homeB, "state", "model-catalog.json"))
	if err != nil {
		t.Fatalf("读 B 的文件: %v", err)
	}
	if strings.Contains(string(rawB), "DSH_FREE_A_ONLY") || strings.Contains(string(rawB), freeSecret) {
		t.Fatalf("B 的文件里出现了 A 的游离凭据：%s", rawB)
	}
}

// 没有 catalog 槽位但同名环境变量有值时，describe 必须报"能解析到值"，
// 且来源写成 env —— 这不是我们写进去的，不能记在 catalog 头上。
func TestOfficialCredentialsDescribeReportsEnvSource(t *testing.T) {
	home := t.TempDir()
	srv, _ := newCatalogServer(t, home)
	t.Setenv("DSH_OFFICIAL_ENV_KEY", "sk-env-only-9d31")

	code, out := callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{"DSH_OFFICIAL_ENV_KEY"},
	})
	if code != http.StatusOK {
		t.Fatalf("describe 状态 %d：%v", code, out)
	}
	info, _ := officialResult(t, out)["DSH_OFFICIAL_ENV_KEY"].(map[string]any)
	if info["configured"] != true {
		t.Fatalf("环境变量有值时应报 configured=true：%v", info)
	}
	if info["source"] != "env" {
		t.Fatalf("source 应写成 env：%v", info)
	}
	// 环境变量本身写不了，但独立凭据存储能给**任何**合法 ref 落一个值
	// （落下去之后 stored > env，覆盖这条环境变量）。官方 writable 问的是
	// "当前 provider 能不能写这个引用"，所以这里是 true。
	if info["writable"] != true {
		t.Fatalf("独立凭据存储能写任何 ref，writable 必须是 true：%v", info)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "sk-env-only-9d31") {
		t.Fatalf("describe 回显了环境变量里的值：%s", raw)
	}
}

// 全部非法写入之后，目录里必须一条凭据都没有（失败不留半份）。
//
// 注意："游离 ref 写不进去"不再属于这一组——独立凭据存储的整个存在理由就是
// 让没有模型引用的 ref 也能落值（见 TestOfficialCredentialsSetFreeRefPersists）。
// 这里换成别的非法形状，验证的仍然是"失败一个字都不写"。
func TestOfficialCredentialsSetFailuresLeaveNoCredential(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_OFFICIAL_TEST_KEY")

	for _, body := range []any{
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": ""},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": "   "},
		map[string]any{"ref": "1BAD-REF", "value": "sk-illegal-ref"},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": " sk-padded-secret "},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": "sk-a\x00b"},
		map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": "sk-x", "extra": 1},
	} {
		if code, out := callOfficialSettings(t, srv, "credentials.set", body); code != http.StatusBadRequest {
			t.Fatalf("body=%v 状态 %d（%v）", body, code, out)
		}
	}
	view, err := llm.OpenModelCatalog(home).List()
	if err != nil {
		t.Fatal(err)
	}
	if status, ok := view.Credentials["DSH_OFFICIAL_TEST_KEY"]; ok && status.Configured {
		t.Fatalf("非法写入留下了凭据：%+v", status)
	}
	if _, ok := view.Credentials["1BAD-REF"]; ok {
		t.Fatalf("非法 ref 也进了凭据投影：%+v", view.Credentials)
	}
	// 磁盘上也不许留：连一次 rename 都不该发生（文件仍然只有种模型那一次提交）。
	raw, err := os.ReadFile(filepath.Join(home, "state", "model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"sk-illegal-ref", "sk-padded-secret", "sk-a"} {
		if strings.Contains(string(raw), fragment) {
			t.Fatalf("失败写入把值落盘了（%s）：%s", fragment, raw)
		}
	}
}

// 凭据路径**不许有日志出口**：一次 set 的值只允许进存储，绝不允许进
// stdout/stderr。这条用源码扫描钉住——比"我读了一遍代码"可验证。
//
// 不用正则：`catalog.` 里也含 "log."，带边界的写法一旦写坏就会静默变成
// 永不命中的空断言（本测试第一版就踩了这个坑）。这里只扫不可能的巧合串。
//
// 扫描范围是两个文件：Transport 适配器（handlers_settings_official.go）与
// llm 的独立凭据存储（model_catalog.go）——值会经过后者落盘，两处都可能是
// 泄漏点。下面还有一条**变异自检**证明这条扫描抓得住真正的泄漏形态。
var credentialLogSinks = []string{
	"fmt.Print", "fmt.Printf", "fmt.Println",
	"fmt.Fprint", "fmt.Fprintf", "fmt.Fprintln",
	"log.Print", "log.Printf", "log.Println", "log.Fatal", "log.Fatalf",
	"os.Stderr", "os.Stdout", "println(",
}

func scanCredentialSourceForLogSinks(body string) error {
	for _, sink := range credentialLogSinks {
		if strings.Contains(body, sink) {
			return fmt.Errorf("出现日志出口 %q：值可能被写进日志/终端", sink)
		}
	}
	return nil
}

// credentialSourceFiles 是凭据这条链上全部会被扫描的实现文件。
func credentialSourceFiles() []string {
	return []string{"handlers_settings_official.go", filepath.Join("..", "llm", "model_catalog.go")}
}

func TestOfficialSettingsCredentialSourceHasNoLogSink(t *testing.T) {
	for _, name := range credentialSourceFiles() {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读实现文件 %s: %v", name, err)
		}
		if err := scanCredentialSourceForLogSinks(string(src)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	src, err := os.ReadFile("handlers_settings_official.go")
	if err != nil {
		t.Fatalf("读实现文件: %v", err)
	}
	body := string(src)
	// Set 的调用点只允许是 (ref, value)：别把值塞进消息或别的参数。
	if strings.Count(body, "api.Credentials.Set(") != 1 || !strings.Contains(body, "api.Credentials.Set(ref, value)") {
		t.Fatalf("credentials.set 的调用点形状变了，重新核对值不会流到别处")
	}
}

// 变异自检：往源码副本里"塞一行打印"，扫描必须立刻红。
//
// 这条测试存在的理由：源码扫描最容易退化成永不命中的空断言（改个名字、
// 换行拼接、注释掉一半都会静默失效）。这里用注入变异证明它抓得住真正的
// 泄漏形态——值跟着一条日志跑出去——并且对未变异的源码不误报。
func TestCredentialSourceScanCatchesInjectedPrint(t *testing.T) {
	src, err := os.ReadFile("handlers_settings_official.go")
	if err != nil {
		t.Fatalf("读实现文件: %v", err)
	}
	storeSrc, err := os.ReadFile(filepath.Join("..", "llm", "model_catalog.go"))
	if err != nil {
		t.Fatalf("读存储实现: %v", err)
	}
	if err := scanCredentialSourceForLogSinks(string(src)); err != nil {
		t.Fatalf("未变异的实现不该被判成泄漏：%v", err)
	}
	if err := scanCredentialSourceForLogSinks(string(storeSrc)); err != nil {
		t.Fatalf("未变异的存储实现不该被判成泄漏：%v", err)
	}

	transportLeak := string(src) + "\nfunc credentialLeakProbe(ref, value string) { fmt.Println(ref, value) }\n"
	if err := scanCredentialSourceForLogSinks(transportLeak); err == nil {
		t.Fatalf("注入 fmt.Println 后扫描仍然通过：这条扫描是空断言")
	}
	storeLeaks := []string{
		string(storeSrc) + "\nfunc credentialLeakProbe(value string) { fmt.Printf(\"credential %s\\n\", value) }\n",
		string(storeSrc) + "\nfunc credentialLeakProbe(value string) { log.Println(value) }\n",
		string(storeSrc) + "\nfunc credentialLeakProbe(value string) { os.Stderr.WriteString(value) }\n",
	}
	for i, leak := range storeLeaks {
		if err := scanCredentialSourceForLogSinks(leak); err == nil {
			t.Fatalf("第 %d 种注入（日志/终端出口）没被扫描抓住", i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// 单真值：credentials.set 与 models.catalog.upsert 的顶层 credential
// ---------------------------------------------------------------------------

// 两条入口写同一张表：任何一条写完，另一条必须立刻看到同一个值，磁盘上
// 每个 ref 只有一个值，revision 栅栏对两条路径是同一套。
func TestOfficialCredentialsSingleTruthWithCatalogUpsert(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_SHARED_KEY")

	const first = "shared-first-secret-b3d7"
	if code, out := callOfficialSettings(t, srv, "credentials.set", map[string]any{
		"ref": "DSH_SHARED_KEY", "value": first,
	}); code != http.StatusOK {
		t.Fatalf("set 状态 %d：%v", code, out)
	}
	// 模型解析读的是同一份存储：官方入口写的值必须是模型真正会用到的那个。
	resolved, err := store.ResolveProvider("official-test-model")
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if resolved.Credential() != first {
		t.Fatalf("模型没有读到 credentials.set 写的值")
	}
	revBefore := mustCatalogRevision(t, store)

	// upsert 顶层 credential 覆盖同一个 ref。
	second := "shared-second-secret-b3d7"
	model := json.RawMessage(`{"id":"official-test-model","api":"openai-completions","baseURL":"https://api.example.com","apiKeyEnv":"DSH_SHARED_KEY"}`)
	if _, err := store.Upsert(model, &second, nil); err != nil {
		t.Fatalf("upsert credential: %v", err)
	}
	resolved, err = store.ResolveProvider("official-test-model")
	if err != nil {
		t.Fatalf("覆盖后 ResolveProvider: %v", err)
	}
	if resolved.Credential() != second {
		t.Fatalf("upsert 与 credentials.set 出现了两份真值")
	}
	code, out := callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{"DSH_SHARED_KEY"},
	})
	if code != http.StatusOK {
		t.Fatalf("describe 状态 %d：%v", code, out)
	}
	info, _ := officialResult(t, out)["DSH_SHARED_KEY"].(map[string]any)
	if info["configured"] != true || info["source"] != "catalog" || info["writable"] != true {
		t.Fatalf("describe 没跟上 upsert 的写入：%v", info)
	}
	if len(info) != 3 {
		t.Fatalf("describe 只允许 configured/source/writable 三个字段：%v", info)
	}
	raw, err := os.ReadFile(filepath.Join(home, "state", "model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), second) != 1 || strings.Contains(string(raw), first) {
		t.Fatalf("磁盘上不是单值：%s", raw)
	}

	// 反向：官方入口覆盖 upsert 写的值。
	const third = "shared-third-secret-b3d7"
	if code, out := callOfficialSettings(t, srv, "credentials.set", map[string]any{
		"ref": "DSH_SHARED_KEY", "value": third,
	}); code != http.StatusOK {
		t.Fatalf("覆盖 set 状态 %d：%v", code, out)
	}
	resolved, err = store.ResolveProvider("official-test-model")
	if err != nil {
		t.Fatalf("反向覆盖后 ResolveProvider: %v", err)
	}
	if resolved.Credential() != third {
		t.Fatalf("models.catalog.upsert 与 credentials.set 出现了两份真值")
	}
	raw, err = os.ReadFile(filepath.Join(home, "state", "model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), third) != 1 || strings.Contains(string(raw), second) {
		t.Fatalf("反向覆盖后磁盘上不是单值：%s", raw)
	}

	// 同一套 revision 栅栏：凭据写入推进 revision 之后，拿旧值 upsert 必须冲突。
	staleRevision := revBefore
	if _, err := store.Upsert(model, nil, &staleRevision); llm.ModelCatalogErrorCode(err) != "revision_conflict" {
		t.Fatalf("跨路径 revision 栅栏失效：err=%v code=%s", err, llm.ModelCatalogErrorCode(err))
	}
}

func mustCatalogRevision(t *testing.T, store *llm.ModelCatalogStore) int64 {
	t.Helper()
	view, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return view.Revision
}

// ---------------------------------------------------------------------------
// 写失败：500 + 不留半份
// ---------------------------------------------------------------------------

// 让原子提交的临时文件位置被一个目录占住：rename 之前就失败。这时必须
// 四字段 500，内存/磁盘都没有新值，且 describe 仍然如实说"未配置"。
func TestOfficialCredentialsWriteFailureLeavesNoHalf(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_OFFICIAL_TEST_KEY")
	const secret = "sk-blocked-write-71e4"

	blocker := filepath.Join(home, "state", "model-catalog.json.tmp")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatalf("造故障: %v", err)
	}
	code, out := callOfficialSettings(t, srv, "credentials.set", map[string]any{
		"ref": "DSH_OFFICIAL_TEST_KEY", "value": secret,
	})
	assertFourFieldError(t, code, out, http.StatusInternalServerError, "internal")
	if encoded, _ := json.Marshal(out); strings.Contains(string(encoded), secret) {
		t.Fatalf("失败响应泄漏了值：%s", encoded)
	}

	view, err := store.List()
	if err != nil {
		t.Fatalf("写失败后 List: %v", err)
	}
	if status, ok := view.Credentials["DSH_OFFICIAL_TEST_KEY"]; ok && status.Configured {
		t.Fatalf("写失败在内存里留下了值：%+v", status)
	}
	raw, err := os.ReadFile(filepath.Join(home, "state", "model-catalog.json"))
	if err != nil {
		t.Fatalf("写失败后读盘: %v", err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "DSH_OFFICIAL_TEST_KEY\":") {
		t.Fatalf("写失败把值落盘了：%s", raw)
	}

	// 故障解除后同一次写入要成功（不存在"卡住的半份"）。
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("清故障: %v", err)
	}
	if code, out := callOfficialSettings(t, srv, "credentials.set", map[string]any{
		"ref": "DSH_OFFICIAL_TEST_KEY", "value": secret,
	}); code != http.StatusOK {
		t.Fatalf("故障解除后仍写不进 %d：%v", code, out)
	}
	code, out = callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{"DSH_OFFICIAL_TEST_KEY"},
	})
	if code != http.StatusOK {
		t.Fatalf("describe 状态 %d：%v", code, out)
	}
	if info, _ := officialResult(t, out)["DSH_OFFICIAL_TEST_KEY"].(map[string]any); info["configured"] != true {
		t.Fatalf("恢复后仍报未配置：%v", info)
	}
}

// ---------------------------------------------------------------------------
// 错误路径零泄漏
// ---------------------------------------------------------------------------

// 每一条拒绝路径都要带一个可识别的"密钥片段"，然后逐字确认它没有出现在
// 响应里（错误 message/hint/code 全都不许带值）。
func TestOfficialCredentialsErrorPathsNeverEchoValue(t *testing.T) {
	home := t.TempDir()
	srv, store := newCatalogServer(t, home)
	seedCatalogModel(t, store, "official-test-model", "DSH_OFFICIAL_TEST_KEY")

	cases := []struct {
		name   string
		body   any
		secret []string
	}{
		{"空串", map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": ""}, nil},
		{"全空白", map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": "   "}, nil},
		{"首尾空白", map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": " sk-pad-leak-0ac1 "}, []string{"sk-pad-leak-0ac1"}},
		{"NUL", map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": "sk-nul-leak-0ac1\x00x"}, []string{"sk-nul-leak-0ac1"}},
		{"非法 ref", map[string]any{"ref": "BAD-REF", "value": "sk-ref-leak-0ac1"}, nil},
		// 客户端把密钥填进 ref 字段：错误串也不能把它带出去（官方 parseRequest
		// 同样只回 zod 的 path/message，不回值）。
		{"密钥填进 ref", map[string]any{"ref": "sk-put-in-ref-0ac1", "value": "x"}, []string{"sk-put-in-ref-0ac1"}},
		{"缺 value", map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY"}, nil},
		{"未知字段", map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": "sk-extra-leak-0ac1", "extra": 1}, []string{"sk-extra-leak-0ac1"}},
		{"value 类型错", map[string]any{"ref": "DSH_OFFICIAL_TEST_KEY", "value": 42}, nil},
	}
	for _, testCase := range cases {
		code, out := callOfficialSettings(t, srv, "credentials.set", testCase.body)
		assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
		encoded, _ := json.Marshal(out)
		for _, fragment := range testCase.secret {
			if strings.Contains(string(encoded), fragment) {
				t.Fatalf("%s：错误响应泄漏了值片段 %q：%s", testCase.name, fragment, encoded)
			}
		}
	}

	// 未接线时也不许泄漏（走 501 分支）。
	unattached, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	code, out := callOfficialSettings(t, unattached, "credentials.set", map[string]any{
		"ref": "DSH_OFFICIAL_TEST_KEY", "value": "sk-unattached-leak-0ac1",
	})
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	if encoded, _ := json.Marshal(out); strings.Contains(string(encoded), "sk-unattached-leak-0ac1") {
		t.Fatalf("未接线错误泄漏了值：%s", encoded)
	}

	// describe 的 refs 里塞密钥：同样只报位置/长度，不回显名字。
	code, out = callOfficialSettings(t, srv, "credentials.describe", map[string]any{
		"refs": []string{"sk-in-describe-0ac1"},
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	if encoded, _ := json.Marshal(out); strings.Contains(string(encoded), "sk-in-describe-0ac1") {
		t.Fatalf("describe 的 grammar 错误泄漏了 ref 原文：%s", encoded)
	}
}
