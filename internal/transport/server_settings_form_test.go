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

// 设置扩展必须走现有 cookie 门，且不能挤进 canonical 25。
func TestSettingsFormHandlerMountedAfterCookieAuthentication(t *testing.T) {
	if got := len(SessionMethods()); got != 25 {
		t.Fatalf("canonical methods changed: %d", got)
	}
	if got := len(SettingsMethods()); got != 6 {
		t.Fatalf("settings methods=%d, want 6", got)
	}
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"ns":"permission"}` {
			t.Errorf("bare body changed: %s", body)
		}
		writeJSON(w, http.StatusOK, map[string]any{"sections": []any{}, "errors": []any{}})
	})
	s, err := NewServer(Options{SettingsFormHandler: handler})
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	resp, err := http.Post(base+"/api/settings.form.get", "application/json", strings.NewReader(`{"ns":"permission"}`))
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
	for _, method := range SettingsMethods() {
		resp, err = client.Post(base+"/api/"+string(method), "application/json", strings.NewReader(`{"ns":"permission"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", method, resp.StatusCode, body)
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("handler calls=%d, want 6", calls.Load())
	}
}

func TestSettingsFormAdditiveSnapshotLeavesCanonicalAlone(t *testing.T) {
	raw, err := os.ReadFile("testdata/settings-form.snapshot.json")
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
	if snapshot.Canonical != 25 || len(snapshot.Methods) != len(SettingsMethods()) {
		t.Fatalf("snapshot canonical=%d methods=%d", snapshot.Canonical, len(snapshot.Methods))
	}
	for i, method := range SettingsMethods() {
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
		if strings.HasPrefix(name, "settings.") {
			t.Fatalf("settings extension leaked into canonical snapshot: %q", name)
		}
	}
}

func TestSettingsFormHandlerAbsentKeepsClosedDispatchBehavior(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cookies["settings-test-cookie"] = struct{}{}
	s.mu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, "/api/settings.form.get", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "settings-test-cookie"})
	recorder := newPluginResponseRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.status != http.StatusNotImplemented {
		t.Fatalf("nil SettingsFormHandler status=%d body=%s", recorder.status, recorder.body.String())
	}
	assertPluginTransportError(t, recorder.body.Bytes(), "not_implemented")
}
