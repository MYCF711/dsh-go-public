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
	"testing"
)

func TestDiscoverEnvModelCatalogEntryUsesOnlyProviderTruth(t *testing.T) {
	const secret = "env-catalog-secret"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(w, "unexpected request", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[`+
			`{"id":"env-model-a","display_name":"Env A","owned_by":"provider-a","context_window":1000000,"max_output_tokens":128000,"modalities":{"input":["text","image"]},"supported_reasoning_levels":[{"effort":"xhigh"},{"effort":"max"}],"reasoning_levels":["xhigh","max"]},`+
			`{"id":"env-model-b","display_name":"Env B","owned_by":"provider-b","context_window":2000000,"max_output_tokens":64000,"modalities":{"input":["text"]},"supported_reasoning_levels":[{"effort":"low"}]},`+
			`{"id":"metadata-unknown","reasoning":true}`+
			`]}`)
	}))
	defer gateway.Close()
	t.Setenv("DSHD_LLM_KEY", secret)
	client := New(gateway.URL+"/v1", secret)

	raw, found, err := DiscoverEnvModelCatalogEntry(context.Background(), client, "env-model-a", "DSHD_LLM_KEY")
	if err != nil || !found {
		t.Fatalf("discover found=%v err=%v", found, err)
	}
	want := fmt.Sprintf(`{"id":"env-model-a","name":"Env A","owned_by":"provider-a","api":"openai-completions","baseURL":%q,"apiKeyEnv":"DSHD_LLM_KEY","contextWindow":1000000,"maxTokens":128000,"input":["text","image"],"reasoningEfforts":{"xhigh":"xhigh","max":"max"}}`, gateway.URL+"/v1")
	if string(raw) != want {
		t.Fatalf("entry mismatch\n got=%s\nwant=%s", raw, want)
	}
	for _, fabricated := range []string{`"off"`, `"low"`, `"high"`, `"reasoningEfforts":false`} {
		if strings.Contains(string(raw), fabricated) {
			t.Fatalf("fabricated %s in %s", fabricated, raw)
		}
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("credential literal leaked: %s", raw)
	}

	raw, found, err = DiscoverEnvModelCatalogEntry(context.Background(), client, "metadata-unknown", "")
	if err != nil || !found {
		t.Fatalf("metadata-unknown found=%v err=%v", found, err)
	}
	var unknown map[string]any
	if err := json.Unmarshal(raw, &unknown); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reasoningEfforts", "contextWindow", "maxTokens", "input", "name", "owned_by"} {
		if _, exists := unknown[key]; exists {
			t.Fatalf("unknown metadata fabricated %s: %s", key, raw)
		}
	}
	if _, found, err = DiscoverEnvModelCatalogEntry(context.Background(), client, "not-offered", ""); err != nil || found {
		t.Fatalf("not-offered found=%v err=%v", found, err)
	}
}

func TestEnvModelFallbackOnlyWhenCatalogFileIsMissing(t *testing.T) {
	home := t.TempDir()
	store := OpenModelCatalog(home)
	if !store.Missing() {
		t.Fatal("new home must report a physically missing catalog")
	}
	raw := json.RawMessage(`{"id":"env-model-a","name":"Env A","api":"openai-completions","baseURL":"http://127.0.0.1:3425/v1","apiKeyEnv":"DSHD_LLM_KEY","contextWindow":1000000,"maxTokens":128000,"input":["text","image"],"reasoningEfforts":{"xhigh":"xhigh","max":"max"}}`)
	t.Setenv("DSHD_LLM_KEY", "env-secret")
	applied, err := store.SetEnvModelFallback(raw)
	if err != nil || !applied {
		t.Fatalf("set fallback applied=%v err=%v", applied, err)
	}
	view, err := store.List()
	if err != nil || view.Revision != 0 {
		t.Fatalf("fallback list=%+v err=%v", view, err)
	}
	assertCatalogOrder(t, view, "env-model-a")
	if string(view.Models[0]) != string(raw) {
		t.Fatalf("fallback rewritten\n got=%s\nwant=%s", view.Models[0], raw)
	}
	if status := view.Credentials["DSHD_LLM_KEY"]; !status.Configured || !status.Writable {
		t.Fatalf("credential projection=%+v", status)
	}
	provider, err := store.ResolveProvider("env-model-a")
	if err != nil || provider.BaseURL != "http://127.0.0.1:3425/v1" || provider.Credential() != "env-secret" {
		t.Fatalf("provider=%+v credential=%q err=%v", provider, provider.Credential(), err)
	}
	catalogPath := filepath.Join(home, "state", "model-catalog.json")
	if _, err := os.Stat(catalogPath); !os.IsNotExist(err) {
		t.Fatalf("read-only fallback persisted a file: %v", err)
	}

	view, err = store.Upsert(json.RawMessage(`{"id":"added-model"}`), nil, catalogRevision(0))
	if err != nil || view.Revision != 1 {
		t.Fatalf("upsert fallback view=%+v err=%v", view, err)
	}
	assertCatalogOrder(t, view, "env-model-a", "added-model")
	if store.Missing() {
		t.Fatal("successful upsert must transition to persistent catalog semantics")
	}
	if _, err := os.Stat(catalogPath); err != nil {
		t.Fatalf("persistent catalog missing after explicit write: %v", err)
	}

	emptyHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(emptyHome, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	emptyRaw := []byte(`{"version":1,"revision":7,"models":[]}`)
	if err := os.WriteFile(filepath.Join(emptyHome, "state", "model-catalog.json"), emptyRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	empty := OpenModelCatalog(emptyHome)
	if empty.Missing() {
		t.Fatal("an existing empty catalog is not a missing catalog")
	}
	applied, err = empty.SetEnvModelFallback(raw)
	if err != nil || applied {
		t.Fatalf("existing empty catalog accepted fallback applied=%v err=%v", applied, err)
	}
	emptyView, err := empty.List()
	if err != nil || emptyView.Revision != 7 || len(emptyView.Models) != 0 {
		t.Fatalf("existing empty catalog changed: %+v err=%v", emptyView, err)
	}
}
