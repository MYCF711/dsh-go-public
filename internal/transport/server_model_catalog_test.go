package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestModelCatalogHandlerMountedAfterCookieAuthentication(t *testing.T) {
	if got := len(SessionMethods()); got != 25 {
		t.Fatalf("canonical methods changed: %d", got)
	}
	if got := len(ModelCatalogMethods()); got != 5 {
		t.Fatalf("model catalog methods=%d, want 5", got)
	}
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{}` {
			t.Errorf("bare body changed: %s", body)
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"models": []any{}}})
	})
	s, err := NewServer(Options{ModelCatalogHandler: handler})
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	resp, err := http.Post(base+"/api/models.catalog.list", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("unauthenticated status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
	assertPluginTransportError(t, body, "unauthorized")

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = client.Get(base + "/connect?token=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, method := range ModelCatalogMethods() {
		resp, err = client.Post(base+"/api/"+string(method), "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", method, resp.StatusCode, body)
		}
	}
	if calls.Load() != 5 {
		t.Fatalf("handler calls=%d, want 5", calls.Load())
	}
}

func TestModelCatalogAdditiveSnapshotLeavesCanonicalAlone(t *testing.T) {
	raw, err := os.ReadFile("testdata/model-catalog.snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Canonical int `json:"canonicalMethodsUnchanged"`
		Methods   []struct {
			Name string `json:"name"`
		} `json:"methods"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Canonical != 25 || len(snapshot.Methods) != len(ModelCatalogMethods()) {
		t.Fatalf("snapshot canonical=%d methods=%d", snapshot.Canonical, len(snapshot.Methods))
	}
	for i, method := range ModelCatalogMethods() {
		if snapshot.Methods[i].Name != string(method) {
			t.Fatalf("snapshot method[%d]=%q, want %q", i, snapshot.Methods[i].Name, method)
		}
	}
	canonicalRaw, err := os.ReadFile(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var canonical struct {
		Methods []string `json:"methods"`
	}
	if err := json.Unmarshal(canonicalRaw, &canonical); err != nil {
		t.Fatal(err)
	}
	if len(canonical.Methods) != 25 {
		t.Fatalf("canonical snapshot methods=%d", len(canonical.Methods))
	}
	for _, name := range canonical.Methods {
		if strings.HasPrefix(name, "models.catalog.") {
			t.Fatalf("model catalog extension leaked into canonical snapshot: %q", name)
		}
	}
}

func TestModelCatalogHandlerAbsentKeepsClosedDispatchBehavior(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cookies["model-catalog-test-cookie"] = struct{}{}
	s.mu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, "/api/models.catalog.list", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "model-catalog-test-cookie"})
	recorder := newPluginResponseRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.status != http.StatusNotImplemented {
		t.Fatalf("nil ModelCatalogHandler status=%d body=%s", recorder.status, recorder.body.String())
	}
	assertPluginTransportError(t, recorder.body.Bytes(), "not_implemented")
}
