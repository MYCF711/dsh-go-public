package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func catalogRevision(n int64) *int64 { return &n }

func catalogModelID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value.ID
}

func assertCatalogOrder(t *testing.T, view ModelCatalogView, want ...string) {
	t.Helper()
	if len(view.Models) != len(want) {
		t.Fatalf("models=%d want=%d: %+v", len(view.Models), len(want), view.Models)
	}
	for i, id := range want {
		if got := catalogModelID(t, view.Models[i]); got != id {
			t.Fatalf("model[%d]=%q want=%q", i, got, id)
		}
	}
}

func TestModelCatalogRoundTripReorderConflictAndUnknownFields(t *testing.T) {
	home := t.TempDir()
	store := OpenModelCatalog(home)
	initial, err := store.List()
	if err != nil || initial.Revision != 0 || initial.Models == nil || len(initial.Models) != 0 {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	if _, err := store.Reorder([]string{}, catalogRevision(0)); ModelCatalogErrorCode(err) != "empty_catalog" {
		t.Fatalf("empty reorder error=%v code=%s", err, ModelCatalogErrorCode(err))
	}

	secret := "catalog-test-secret"
	modelA := json.RawMessage(`{"id":"model-a","name":"Alpha","owned_by":"local","api":"openai-completions","baseURL":"http://127.0.0.1:1/v1","apiKeyEnv":"LOCAL_API_KEY","contextWindow":128000,"maxTokens":8192,"input":["text","image"],"reasoningEfforts":{"off":null,"low":"low","custom":"wire-value"},"futureField":{"z":1,"a":[true,null,"keep"]},"futureScalar":42}`)
	view, err := store.Upsert(modelA, &secret, catalogRevision(0))
	if err != nil || view.Revision != 1 {
		t.Fatalf("upsert A revision=%d err=%v", view.Revision, err)
	}
	assertCatalogOrder(t, view, "model-a")
	if status := view.Credentials["LOCAL_API_KEY"]; !status.Configured || !status.Writable {
		t.Fatalf("credential status=%+v", status)
	}
	modelB := json.RawMessage(`{"id":"model-b","name":"Beta","api":"openai-completions","baseURL":"http://127.0.0.1:1/v1","input":[],"futureB":{"preserved":true}}`)
	view, err = store.Upsert(modelB, nil, catalogRevision(1))
	if err != nil || view.Revision != 2 {
		t.Fatalf("upsert B revision=%d err=%v", view.Revision, err)
	}
	assertCatalogOrder(t, view, "model-a", "model-b")

	// A partial official edit must retain omitted and unknown fields. Existing key
	// positions stay put; the updated name keeps its original position.
	view, err = store.Upsert(json.RawMessage(`{"id":"model-a","name":"Alpha Two"}`), nil, catalogRevision(2))
	if err != nil || view.Revision != 3 {
		t.Fatalf("update A revision=%d err=%v", view.Revision, err)
	}
	updatedA := string(view.Models[0])
	for _, fragment := range []string{`"futureField":{"z":1,"a":[true,null,"keep"]}`, `"futureScalar":42`, `"baseURL":"http://127.0.0.1:1/v1"`, `"name":"Alpha Two"`, `"reasoningEfforts":{"off":null,"low":"low","custom":"wire-value"}`} {
		if !strings.Contains(updatedA, fragment) {
			t.Fatalf("updated model lost %s: %s", fragment, updatedA)
		}
	}
	if strings.Index(updatedA, `"id"`) > strings.Index(updatedA, `"name"`) || strings.Index(updatedA, `"futureField"`) > strings.Index(updatedA, `"futureScalar"`) {
		t.Fatalf("object field order changed: %s", updatedA)
	}

	if _, err := store.Upsert(json.RawMessage(`{"id":"model-c"}`), nil, catalogRevision(1)); ModelCatalogErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale upsert error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	if _, err := store.Reorder([]string{"model-b", "model-a"}, catalogRevision(0)); ModelCatalogErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale reorder error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	unchanged, err := store.List()
	if err != nil || unchanged.Revision != 3 {
		t.Fatalf("conflict changed revision: %+v err=%v", unchanged, err)
	}

	before := map[string]string{}
	for _, raw := range unchanged.Models {
		before[catalogModelID(t, raw)] = string(raw)
	}
	view, err = store.Reorder([]string{"model-b", "model-a"}, catalogRevision(3))
	if err != nil || view.Revision != 4 {
		t.Fatalf("reorder revision=%d err=%v", view.Revision, err)
	}
	assertCatalogOrder(t, view, "model-b", "model-a")
	for _, raw := range view.Models {
		id := catalogModelID(t, raw)
		if string(raw) != before[id] {
			t.Fatalf("reorder rewrote %s\nbefore=%s\nafter=%s", id, before[id], raw)
		}
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "catalog-test-secret") {
		t.Fatalf("credential echoed in list: %s", encoded)
	}

	reopened := OpenModelCatalog(home)
	after, err := reopened.List()
	if err != nil || after.Revision != 4 {
		t.Fatalf("reopen revision=%d err=%v", after.Revision, err)
	}
	assertCatalogOrder(t, after, "model-b", "model-a")
	for _, raw := range after.Models {
		id := catalogModelID(t, raw)
		if string(raw) != before[id] {
			t.Fatalf("round trip rewrote %s\nbefore=%s\nafter=%s", id, before[id], raw)
		}
	}
	if status := after.Credentials["LOCAL_API_KEY"]; !status.Configured || !status.Writable {
		t.Fatalf("reopen credential status=%+v", status)
	}
}

func TestModelCatalogRejectsBadIDsDuplicatesAndIllegalFieldsWithoutMutation(t *testing.T) {
	valid := json.RawMessage(`{"id":"model-a","name":"Alpha","api":"openai-completions","baseURL":"http://127.0.0.1:1/v1","contextWindow":128000,"maxTokens":8192,"input":["text"],"reasoningEfforts":{"off":null,"low":"low"}}`)
	tests := []struct {
		name string
		raw  string
		code string
	}{
		{"empty id", `{"id":""}`, "invalid_id"},
		{"blank id", `{"id":"  "}`, "invalid_id"},
		{"untrimmed id", `{"id":" model-a "}`, "invalid_id"},
		{"missing id", `{"name":"missing"}`, "invalid_id"},
		{"wrong id type", `{"id":1}`, "invalid_id"},
		{"empty name", `{"id":"x","name":""}`, "invalid_field"},
		{"zero context", `{"id":"x","contextWindow":0}`, "invalid_field"},
		{"fractional context", `{"id":"x","contextWindow":1.5}`, "invalid_field"},
		{"negative max tokens", `{"id":"x","maxTokens":-1}`, "invalid_field"},
		{"bad input", `{"id":"x","input":["audio"]}`, "invalid_field"},
		{"duplicate input", `{"id":"x","input":["text","text"]}`, "invalid_field"},
		{"off only reasoning", `{"id":"x","reasoningEfforts":{"off":null}}`, "invalid_field"},
		{"bad reasoning wire", `{"id":"x","reasoningEfforts":{"low":null}}`, "invalid_field"},
		{"unsupported api", `{"id":"x","api":"anthropic-messages"}`, "invalid_field"},
		{"bad base URL", `{"id":"x","baseURL":"file:///tmp/model"}`, "invalid_field"},
		{"URL credential", `{"id":"x","baseURL":"http://user:pass@127.0.0.1/v1"}`, "invalid_field"},
		{"bad credential ref", `{"id":"x","apiKeyEnv":"1BAD-REF"}`, "invalid_field"},
		{"secret field", `{"id":"x","apiKey":"must-not-persist"}`, "invalid_field"},
		{"duplicate JSON member", `{"id":"x","id":"y"}`, "invalid_field"},
		{"trailing JSON", `{"id":"x"} true`, "invalid_field"},
		{"non-object", `[]`, "invalid_field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := OpenModelCatalog(t.TempDir())
			if _, err := store.Upsert(valid, nil, catalogRevision(0)); err != nil {
				t.Fatal(err)
			}
			_, err := store.Upsert(json.RawMessage(test.raw), nil, catalogRevision(1))
			if got := ModelCatalogErrorCode(err); got != test.code {
				t.Fatalf("error=%v code=%q want=%q", err, got, test.code)
			}
			view, listErr := store.List()
			if listErr != nil || view.Revision != 1 || len(view.Models) != 1 || string(view.Models[0]) != string(valid) {
				t.Fatalf("failed upsert mutated catalog: view=%+v err=%v", view, listErr)
			}
		})
	}

	t.Run("duplicate IDs on disk fail load", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "state", "model-catalog.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		raw := `{"version":1,"revision":7,"models":[{"id":"same"},{"id":"same"}]}`
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		store := OpenModelCatalog(home)
		if _, err := store.List(); ModelCatalogErrorCode(err) != "duplicate_id" {
			t.Fatalf("duplicate load error=%v code=%s", err, ModelCatalogErrorCode(err))
		}
		if _, err := store.Upsert(json.RawMessage(`{"id":"new"}`), nil, nil); ModelCatalogErrorCode(err) != "duplicate_id" {
			t.Fatalf("corrupt store allowed overwrite: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != raw {
			t.Fatalf("corrupt source changed: %s err=%v", after, err)
		}
	})

	t.Run("reorder rejects duplicates omissions and empty order", func(t *testing.T) {
		store := OpenModelCatalog(t.TempDir())
		if _, err := store.Upsert(valid, nil, catalogRevision(0)); err != nil {
			t.Fatal(err)
		}
		for _, ids := range [][]string{{"model-a", "model-a"}, {"missing"}, {}, nil} {
			if _, err := store.Reorder(ids, catalogRevision(1)); err == nil {
				t.Fatalf("reorder %v unexpectedly succeeded", ids)
			}
		}
		view, err := store.List()
		if err != nil || view.Revision != 1 {
			t.Fatalf("failed reorder changed revision: %+v err=%v", view, err)
		}
	})
}

func TestModelCatalogFailedFlushRollsBackAndCorruptionStaysLoud(t *testing.T) {
	home := t.TempDir()
	store := OpenModelCatalog(home)
	valid := json.RawMessage(`{"id":"model-a","name":"before"}`)
	if _, err := store.Upsert(valid, nil, catalogRevision(0)); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(home, "blocked-target")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	store.path = blocked
	if _, err := store.Upsert(json.RawMessage(`{"id":"model-a","name":"after"}`), nil, catalogRevision(1)); ModelCatalogErrorCode(err) != "internal" {
		t.Fatalf("flush error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	view, err := store.List()
	if err != nil || view.Revision != 1 || string(view.Models[0]) != string(valid) {
		t.Fatalf("failed flush changed memory: view=%+v err=%v", view, err)
	}

	corruptHome := t.TempDir()
	path := filepath.Join(corruptHome, "state", "model-catalog.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"revision":"broken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := OpenModelCatalog(corruptHome)
	if _, err := corrupt.List(); ModelCatalogErrorCode(err) != "internal" {
		t.Fatalf("corrupt catalog error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
}

func TestModelCatalogTestUsesRealOpenAIEndpointAndNeverEchoesCredential(t *testing.T) {
	const secret = "real-local-secret"
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":"bad credential wrong-secret"}`)
			return
		}
		var request struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Model != "model-a" || request.Stream || request.MaxTokens != 1 || len(request.Messages) != 1 || request.Messages[0].Role != "user" {
			http.Error(w, fmt.Sprintf("bad request: %+v", request), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"local-response","model":"model-a","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer gateway.Close()

	store := OpenModelCatalog(t.TempDir())
	credential := secret
	model := json.RawMessage(fmt.Sprintf(`{"id":"model-a","api":"openai-completions","baseURL":%q,"apiKeyEnv":"LOCAL_API_KEY"}`, gateway.URL+"/v1"))
	view, err := store.Upsert(model, &credential, catalogRevision(0))
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Test(context.Background(), "model-a", ModelTestOverride{})
	if err != nil || !result.OK || result.ID != "model-a" || result.Model != "model-a" || result.LatencyMs < 0 {
		t.Fatalf("test result=%+v err=%v", result, err)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("credential echoed: %s", encoded)
	}

	wrong := "wrong-secret"
	_, err = store.Test(context.Background(), "model-a", ModelTestOverride{Credential: &wrong})
	if ModelCatalogErrorCode(err) != "upstream_error" || strings.Contains(err.Error(), wrong) {
		t.Fatalf("upstream credential error leaked or wrong code: %v code=%s", err, ModelCatalogErrorCode(err))
	}

	t.Setenv("MISSING_MODEL_CATALOG_KEY", "")
	view, err = store.Upsert(json.RawMessage(fmt.Sprintf(`{"id":"missing-key","api":"openai-completions","baseURL":%q,"apiKeyEnv":"MISSING_MODEL_CATALOG_KEY"}`, gateway.URL+"/v1")), nil, catalogRevision(1))
	if err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	_, err = store.Test(context.Background(), "missing-key", ModelTestOverride{})
	if ModelCatalogErrorCode(err) != "credential_missing" || calls.Load() != before {
		t.Fatalf("missing credential error=%v code=%s calls=%d->%d", err, ModelCatalogErrorCode(err), before, calls.Load())
	}

	view, err = store.Upsert(json.RawMessage(`{"id":"no-endpoint","api":"openai-completions"}`), nil, catalogRevision(2))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Test(context.Background(), "no-endpoint", ModelTestOverride{})
	if ModelCatalogErrorCode(err) != "invalid_config" {
		t.Fatalf("missing endpoint error=%v code=%s", err, ModelCatalogErrorCode(err))
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer malformed.Close()
	view, err = store.Upsert(json.RawMessage(fmt.Sprintf(`{"id":"malformed","baseURL":%q}`, malformed.URL+"/v1")), nil, catalogRevision(3))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Test(context.Background(), "malformed", ModelTestOverride{})
	if ModelCatalogErrorCode(err) != "upstream_error" {
		t.Fatalf("malformed success error=%v code=%s", err, ModelCatalogErrorCode(err))
	}

	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := unreachable.URL
	unreachable.Close()
	view, err = store.Upsert(json.RawMessage(fmt.Sprintf(`{"id":"unreachable","baseURL":%q}`, base+"/v1")), nil, catalogRevision(4))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Test(context.Background(), "unreachable", ModelTestOverride{})
	if ModelCatalogErrorCode(err) != "upstream_error" {
		t.Fatalf("unreachable error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	if _, err := store.Test(context.Background(), "bad-id", ModelTestOverride{}); ModelCatalogErrorCode(err) != "not_found" {
		t.Fatalf("bad test id error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
}

// ---------------------------------------------------------------------------
// 独立凭据存储：游离 ref + 与 models.catalog.upsert 顶层 credential 的单真值
// ---------------------------------------------------------------------------

func readCatalogFile(t *testing.T, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "state", "model-catalog.json"))
	if err != nil {
		t.Fatalf("读凭据文件: %v", err)
	}
	return string(raw)
}

func TestModelCatalogCredentialStoreFreeRefLifecycle(t *testing.T) {
	const secret = "free-ref-secret-9a2f"
	home := t.TempDir()
	store := OpenModelCatalog(home)

	// 1) 游离 ref（没有任何模型引用它）能直接写。
	view, err := store.SetCredential("FREE_REF", secret, catalogRevision(0))
	if err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	if view.Revision != 1 {
		t.Fatalf("revision=%d，期望 1", view.Revision)
	}
	status, ok := view.Credentials["FREE_REF"]
	if !ok || !status.Configured || !status.Writable {
		t.Fatalf("游离 ref 没进视图：%+v", view.Credentials)
	}
	// 视图（models.catalog.list 的投影）里不许出现值。
	if encoded, _ := json.Marshal(view); strings.Contains(string(encoded), secret) {
		t.Fatalf("视图回显了密钥：%s", encoded)
	}

	// 2) describe：configured/source/writable 三项，且没有值字段。
	views, err := store.DescribeCredentials([]string{"FREE_REF", "NEVER_SET"}, nil)
	if err != nil {
		t.Fatalf("DescribeCredentials: %v", err)
	}
	if got := views["FREE_REF"]; !got.Configured || got.Source != ModelCredentialSourceCatalog || !got.Writable {
		t.Fatalf("FREE_REF describe=%+v", got)
	}
	if got := views["NEVER_SET"]; got.Configured || got.Source != "" || !got.Writable {
		t.Fatalf("NEVER_SET describe=%+v（未配置时 source 必须缺省，writable 仍为 true）", got)
	}

	// 3) 真落盘，且值只出现一处（单真值）。
	raw := readCatalogFile(t, home)
	if strings.Count(raw, secret) != 1 {
		t.Fatalf("值在磁盘上出现 %d 次（应为 1）：%s", strings.Count(raw, secret), raw)
	}
	if !strings.Contains(raw, `"standaloneCredentials":["FREE_REF"]`) {
		t.Fatalf("游离槽位没被标记：%s", raw)
	}

	// 4) 重开一份目录也要看到它（不是内存假库）。
	reopened := OpenModelCatalog(home)
	views, err = reopened.DescribeCredentials([]string{"FREE_REF"}, nil)
	if err != nil {
		t.Fatalf("重开 describe: %v", err)
	}
	if got := views["FREE_REF"]; !got.Configured || got.Source != ModelCredentialSourceCatalog {
		t.Fatalf("重开后 describe=%+v", got)
	}

	// 5) 一次无关的模型写入不许把它当孤儿清掉。
	model := json.RawMessage(`{"id":"model-a","api":"openai-completions","baseURL":"https://api.example.com","apiKeyEnv":"MODEL_KEY"}`)
	if _, err := reopened.Upsert(model, nil, catalogRevision(1)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	views, err = reopened.DescribeCredentials([]string{"FREE_REF"}, nil)
	if err != nil {
		t.Fatalf("upsert 后 describe: %v", err)
	}
	if got := views["FREE_REF"]; !got.Configured {
		t.Fatalf("models.catalog.upsert 清掉了游离凭据：%+v", got)
	}

	// 6) 两条入口写同一个 ref：后写的赢，磁盘上仍只有一个值。
	second := "upsert-shared-secret-77c1"
	if _, err := reopened.Upsert(model, &second, catalogRevision(2)); err != nil {
		t.Fatalf("Upsert credential: %v", err)
	}
	resolved, err := reopened.ResolveProvider("model-a")
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if resolved.Credential() != second {
		t.Fatalf("模型解析到的凭据不是 upsert 写的那个")
	}
	raw = readCatalogFile(t, home)
	// FREE_REF 的值仍在（它是另一个引用），这里要钉的是 MODEL_KEY 只有一份真值。
	if strings.Count(raw, second) != 1 {
		t.Fatalf("upsert 之后的值不唯一：%s", raw)
	}
	if !strings.Contains(raw, secret) {
		t.Fatalf("无关的游离凭据被清掉了：%s", raw)
	}

	const third = "set-overrides-secret-31d0"
	if _, err := reopened.SetCredential("MODEL_KEY", third, catalogRevision(3)); err != nil {
		t.Fatalf("SetCredential 覆盖: %v", err)
	}
	resolved, err = reopened.ResolveProvider("model-a")
	if err != nil {
		t.Fatalf("覆盖后 ResolveProvider: %v", err)
	}
	if resolved.Credential() != third {
		t.Fatalf("credentials.set 与 upsert 出现了两份真值")
	}
	raw = readCatalogFile(t, home)
	if strings.Count(raw, third) != 1 || strings.Contains(raw, second) {
		t.Fatalf("覆盖之后磁盘上不是单值：%s", raw)
	}
}

func TestModelCatalogDeleteCredentialIsIdempotentAndFenced(t *testing.T) {
	const secret = "delete-me-secret-5b83"
	home := t.TempDir()
	store := OpenModelCatalog(home)
	if _, err := store.SetCredential("DEL_REF", secret, catalogRevision(0)); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	view, err := store.DeleteCredential("DEL_REF", catalogRevision(1))
	if err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if view.Revision != 2 {
		t.Fatalf("删除后 revision=%d，期望 2", view.Revision)
	}
	if _, ok := view.Credentials["DEL_REF"]; ok {
		t.Fatalf("删除后视图里还有 DEL_REF：%+v", view.Credentials)
	}
	raw := readCatalogFile(t, home)
	if strings.Contains(raw, secret) || strings.Contains(raw, "DEL_REF") {
		t.Fatalf("删除后磁盘上还留着：%s", raw)
	}

	// 幂等：删一个本来不存在的东西 = 成功，且不写盘、不动 revision。
	before := raw
	view, err = store.DeleteCredential("DEL_REF", nil)
	if err != nil {
		t.Fatalf("幂等删除失败: %v", err)
	}
	if view.Revision != 2 {
		t.Fatalf("幂等删除动了 revision：%d", view.Revision)
	}
	if after := readCatalogFile(t, home); after != before {
		t.Fatalf("幂等删除仍然写了盘：%s", after)
	}

	// 栅栏：stale expectedRevision 一个字都不写。
	before = readCatalogFile(t, home)
	if _, err := store.SetCredential("DEL_REF", "fresh-secret-c19e", catalogRevision(0)); ModelCatalogErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale set error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	if _, err := store.DeleteCredential("MODEL_KEY", catalogRevision(1)); ModelCatalogErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale delete error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	if after := readCatalogFile(t, home); after != before {
		t.Fatalf("冲突写入改了磁盘：%s", after)
	}
}

// 失败路径：值一个字符都不许进错误串，也不许留半份。
func TestModelCatalogCredentialStoreFailuresNeverEchoValue(t *testing.T) {
	home := t.TempDir()
	store := OpenModelCatalog(home)
	if _, err := store.SetCredential("KEEP_REF", "keep-secret-7d2b", catalogRevision(0)); err != nil {
		t.Fatalf("种值: %v", err)
	}
	before := readCatalogFile(t, home)

	badValues := []string{"", "   ", " leading-secret-4a1c", "trailing-secret-4a1c ", "nul-secret-4a1c\x00x"}
	for _, value := range badValues {
		_, err := store.SetCredential("KEEP_REF", value, nil)
		if err == nil {
			t.Fatalf("value=%q 竟然写成功了", value)
		}
		if ModelCatalogErrorCode(err) != "invalid_field" {
			t.Fatalf("value=%q code=%s", value, ModelCatalogErrorCode(err))
		}
		for _, fragment := range []string{"secret-4a1c", "leading-secret", "trailing-secret", "nul-secret"} {
			if strings.Contains(err.Error(), fragment) {
				t.Fatalf("错误串泄漏了值片段 %q：%v", fragment, err)
			}
		}
	}
	for _, ref := range []string{"", "BAD-REF", "1BAD", "BAD REF"} {
		_, err := store.SetCredential(ref, "sk-should-not-persist", nil)
		if ModelCatalogErrorCode(err) != "invalid_field" {
			t.Fatalf("ref=%q error=%v code=%s", ref, err, ModelCatalogErrorCode(err))
		}
		_, err = store.DescribeCredentials([]string{ref}, nil)
		if ModelCatalogErrorCode(err) != "invalid_field" {
			t.Fatalf("describe ref=%q error=%v code=%s", ref, err, ModelCatalogErrorCode(err))
		}
	}
	// 密钥被误填进 ref：错误串只报位置/长度，不回显原文。
	_, err := store.SetCredential("sk-ref-secret-4a1c", "x", nil)
	if ModelCatalogErrorCode(err) != "invalid_field" {
		t.Fatalf("密钥形状的 ref error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	if strings.Contains(err.Error(), "sk-ref-secret") {
		t.Fatalf("grammar 错误回显了 ref 原文：%v", err)
	}
	if after := readCatalogFile(t, home); after != before {
		t.Fatalf("失败路径写了盘：%s", after)
	}

	// 写失败（临时文件位置被一个目录占住）：返回 internal，内存与磁盘都不动。
	tmpDir := filepath.Join(home, "state", "model-catalog.json.tmp")
	if err := os.Mkdir(tmpDir, 0o755); err != nil {
		t.Fatalf("造故障: %v", err)
	}
	if _, err := store.SetCredential("KEEP_REF", "blocked-secret-e88a", nil); ModelCatalogErrorCode(err) != "internal" {
		t.Fatalf("写失败 error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	after, listErr := store.List()
	if listErr != nil || after.Revision != 1 {
		t.Fatalf("写失败改了内存：revision=%d err=%v", after.Revision, listErr)
	}
	if _, ok := after.Credentials["KEEP_REF"]; !ok {
		t.Fatalf("写失败丢了旧值：%+v", after.Credentials)
	}
	if raw := readCatalogFile(t, home); raw != before {
		t.Fatalf("写失败改了磁盘：%s", raw)
	}

	// 故障解除后同一次写入能成功（不留半份状态）。
	if err := os.Remove(tmpDir); err != nil {
		t.Fatalf("清故障: %v", err)
	}
	if _, err := store.SetCredential("BLOCKED_REF", "recovered-secret-e88a", nil); err != nil {
		t.Fatalf("故障解除后仍写不进: %v", err)
	}
	views, err := store.DescribeCredentials([]string{"BLOCKED_REF"}, nil)
	if err != nil || !views["BLOCKED_REF"].Configured {
		t.Fatalf("恢复后 describe=%+v err=%v", views["BLOCKED_REF"], err)
	}
}

func TestModelCatalogDescribeCredentialsPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PRECEDENCE_REF", "env-value-f2c9")
	store := OpenModelCatalog(home)

	// 只有环境变量：能解析到值，来源是 env。
	views, err := store.DescribeCredentials([]string{"PRECEDENCE_REF"}, os.Getenv)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if got := views["PRECEDENCE_REF"]; !got.Configured || got.Source != ModelCredentialSourceEnv || !got.Writable {
		t.Fatalf("env 层 describe=%+v", got)
	}

	// 写入之后 stored > env：来源变成 catalog，环境变量不再参与。
	if _, err := store.SetCredential("PRECEDENCE_REF", "stored-value-f2c9", catalogRevision(0)); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	views, err = store.DescribeCredentials([]string{"PRECEDENCE_REF"}, os.Getenv)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if got := views["PRECEDENCE_REF"]; got.Source != ModelCredentialSourceCatalog {
		t.Fatalf("stored 层 describe=%+v", got)
	}

	// 删除之后回落到 env（不是"未配置"）。
	if _, err := store.DeleteCredential("PRECEDENCE_REF", nil); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	views, err = store.DescribeCredentials([]string{"PRECEDENCE_REF"}, os.Getenv)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if got := views["PRECEDENCE_REF"]; !got.Configured || got.Source != ModelCredentialSourceEnv {
		t.Fatalf("回落 describe=%+v", got)
	}
}

// 脏文档：非法独立槽位 / 值里带 NUL 都在读盘时 fail-loud，且错误串不带值。
func TestModelCatalogRejectsBadCredentialDocument(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		home := t.TempDir()
		path := filepath.Join(home, "state", "model-catalog.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return home
	}

	badRef := write(t, `{"version":1,"revision":2,"models":[],"standaloneCredentials":["1BAD-REF"]}`)
	store := OpenModelCatalog(badRef)
	_, err := store.List()
	if ModelCatalogErrorCode(err) != "invalid_field" {
		t.Fatalf("非法独立槽位 error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	// 读盘错误也会经 Transport 回到客户端：同样不许回显 ref 原文。
	if strings.Contains(err.Error(), "1BAD") {
		t.Fatalf("读盘错误回显了非法 ref：%v", err)
	}
	if _, err := store.SetCredential("OK_REF", "x", nil); ModelCatalogErrorCode(err) != "invalid_field" {
		t.Fatalf("脏文档仍允许写: %v", err)
	}

	nulValue := write(t, `{"version":1,"revision":2,"models":[],"credentials":{"NUL_REF":"sk-nul\u0000-value"}}`)
	store = OpenModelCatalog(nulValue)
	_, err = store.DescribeCredentials([]string{"NUL_REF"}, nil)
	if ModelCatalogErrorCode(err) != "invalid_field" {
		t.Fatalf("NUL 值 error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	if strings.Contains(err.Error(), "sk-nul") {
		t.Fatalf("错误串泄漏了值：%v", err)
	}
}
