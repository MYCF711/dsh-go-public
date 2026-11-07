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

func TestModelCatalogResolveProviderCredentialSourcesAndValidation(t *testing.T) {
	home := t.TempDir()
	store := OpenModelCatalog(home)
	stored := "stored-provider-secret"
	view, err := store.Upsert(json.RawMessage(`{"id":"stored","api":"openai-completions","baseURL":"http://127.0.0.1:4101/v1","apiKeyEnv":"STORED_PROVIDER_KEY"}`), &stored, catalogRevision(0))
	if err != nil || view.Revision != 1 {
		t.Fatalf("upsert stored: view=%+v err=%v", view, err)
	}
	t.Setenv("ENV_PROVIDER_KEY", "env-provider-secret")
	view, err = store.Upsert(json.RawMessage(`{"id":"environment","baseURL":"http://127.0.0.1:4102/v1","apiKeyEnv":"ENV_PROVIDER_KEY"}`), nil, catalogRevision(1))
	if err != nil || view.Revision != 2 {
		t.Fatalf("upsert environment: view=%+v err=%v", view, err)
	}
	view, err = store.Upsert(json.RawMessage(`{"id":"plain","name":"legacy model id"}`), nil, catalogRevision(2))
	if err != nil || view.Revision != 3 {
		t.Fatalf("upsert plain: view=%+v err=%v", view, err)
	}

	provider, err := store.ResolveProvider("stored")
	if err != nil {
		t.Fatal(err)
	}
	if provider.BaseURL != "http://127.0.0.1:4101/v1" || provider.API != "openai-completions" || provider.APIKeyEnv != "STORED_PROVIDER_KEY" || provider.Credential() != stored {
		t.Fatalf("stored provider mismatch: base=%q api=%q ref=%q credential-redacted=%s", provider.BaseURL, provider.API, provider.APIKeyEnv, RedactCredential(provider.Credential(), stored))
	}
	provider, err = store.ResolveProvider("environment")
	if err != nil {
		t.Fatal(err)
	}
	if provider.API != "openai-completions" || provider.Credential() != "env-provider-secret" {
		t.Fatalf("environment provider was not resolved: base=%q api=%q credential-redacted=%s", provider.BaseURL, provider.API, RedactCredential(provider.Credential(), "env-provider-secret"))
	}
	provider, err = store.ResolveProvider("plain")
	if err != nil {
		t.Fatal(err)
	}
	if provider.BaseURL != "" || provider.API != "" || provider.APIKeyEnv != "" || provider.Credential() != "" {
		t.Fatalf("plain model unexpectedly acquired provider config: %+v", provider)
	}
	if _, err := store.ResolveProvider("does-not-exist"); ModelCatalogErrorCode(err) != "not_found" {
		t.Fatalf("missing model error=%v code=%s", err, ModelCatalogErrorCode(err))
	}

	t.Setenv("MISSING_PROVIDER_KEY", "")
	if _, err := store.Upsert(json.RawMessage(`{"id":"missing-key","baseURL":"http://127.0.0.1:4103/v1","apiKeyEnv":"MISSING_PROVIDER_KEY"}`), nil, catalogRevision(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveProvider("missing-key"); ModelCatalogErrorCode(err) != "credential_missing" {
		t.Fatalf("missing credential error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
	if _, err := store.Upsert(json.RawMessage(`{"id":"missing-base","apiKeyEnv":"STORED_PROVIDER_KEY"}`), nil, catalogRevision(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveProvider("missing-base"); ModelCatalogErrorCode(err) != "invalid_config" {
		t.Fatalf("missing base error=%v code=%s", err, ModelCatalogErrorCode(err))
	}

	badURLHome := t.TempDir()
	badURLPath := filepath.Join(badURLHome, "state", "model-catalog.json")
	if err := os.MkdirAll(filepath.Dir(badURLPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badURLPath, []byte(`{"version":1,"revision":0,"models":[{"id":"bad-url","baseURL":"://bad"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenModelCatalog(badURLHome).ResolveProvider("bad-url"); err == nil {
		t.Fatal("bad on-disk baseURL must fail loudly")
	}

	corruptHome := t.TempDir()
	corruptPath := filepath.Join(corruptHome, "state", "model-catalog.json")
	if err := os.MkdirAll(filepath.Dir(corruptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte(`{"version":1,"revision":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenModelCatalog(corruptHome).ResolveProvider("stored"); ModelCatalogErrorCode(err) != "invalid_field" {
		t.Fatalf("corrupt catalog error=%v code=%s", err, ModelCatalogErrorCode(err))
	}
}

func TestStreamAndCatalogTestShareCredentialRedaction(t *testing.T) {
	const secret = "shared-redaction-secret"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":"upstream echoed %s"}`, secret)
	}))
	t.Cleanup(gateway.Close)

	_, err := New(gateway.URL+"/v1", secret).Stream(context.Background(), Request{Model: "provider-a"}, nil)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("stream credential redaction failed: %v", err)
	}

	store := OpenModelCatalog(t.TempDir())
	credential := secret
	if _, err := store.Upsert(json.RawMessage(fmt.Sprintf(`{"id":"provider-a","baseURL":%q,"apiKeyEnv":"SHARED_REDACTION_KEY"}`, gateway.URL+"/v1")), &credential, catalogRevision(0)); err != nil {
		t.Fatal(err)
	}
	_, err = store.Test(context.Background(), "provider-a", ModelTestOverride{})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || ModelCatalogErrorCode(err) != "upstream_error" {
		t.Fatalf("catalog test credential redaction failed: %v code=%s", err, ModelCatalogErrorCode(err))
	}
}
