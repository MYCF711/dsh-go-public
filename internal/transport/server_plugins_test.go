package transport

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPluginHandlerMountedAfterCookieAuthentication proves that the additive
// handler is reached only through the existing cookie gate. It also proves that
// mounting it does not consume one of the 25 canonical methods.
func TestPluginHandlerMountedAfterCookieAuthentication(t *testing.T) {
	var calls atomic.Int32
	var gotPath, gotBody string
	plugins := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath = r.URL.Path
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		gotBody = string(raw)
		writeJSON(w, http.StatusOK, map[string]any{"plugins": []any{}})
	})
	s, err := NewServer(Options{
		PluginHandler: plugins,
		List: func() ([]SessionSummary, error) {
			return []SessionSummary{{ID: "canonical", Title: "untouched"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	anonymous := &http.Client{}
	resp, err := anonymous.Post(base+"/api/plugin.list", "application/json", strings.NewReader(`{"params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated plugin.list status=%d body=%s", resp.StatusCode, body)
	}
	assertPluginTransportError(t, body, "unauthorized")
	if calls.Load() != 0 {
		t.Fatalf("bare plugin handler ran before authentication: calls=%d", calls.Load())
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = client.Get(base + "/connect?token=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("connect status=%d", resp.StatusCode)
	}

	resp, err = client.Post(base+"/api/plugin.list", "application/json", strings.NewReader(`{"id":"bare"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated plugin.list status=%d body=%s", resp.StatusCode, body)
	}
	if calls.Load() != 1 || gotPath != "/api/plugin.list" || gotBody != `{"id":"bare"}` {
		t.Fatalf("plugin adapter did not receive original request: calls=%d path=%q body=%q", calls.Load(), gotPath, gotBody)
	}

	resp, err = client.Post(base+"/api/session.list", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "canonical") {
		t.Fatalf("canonical session.list changed: status=%d body=%s", resp.StatusCode, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("canonical method was intercepted by plugin handler: calls=%d", calls.Load())
	}
}

func TestPluginHandlerAbsentKeepsClosedDispatchBehavior(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cookies["plugin-test-cookie"] = struct{}{}
	s.mu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, "/api/plugin.list", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "plugin-test-cookie"})
	recorder := newPluginResponseRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.status != http.StatusNotImplemented {
		t.Fatalf("nil PluginHandler status=%d body=%s", recorder.status, recorder.body.String())
	}
	assertPluginTransportError(t, recorder.body.Bytes(), "not_implemented")
}

// newPluginResponseRecorder is deliberately tiny and keeps the test independent
// of httptest's default 200 behavior when asserting a closed-dispatch 501.
type pluginResponseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newPluginResponseRecorder() *pluginResponseRecorder {
	return &pluginResponseRecorder{header: make(http.Header)}
}

func (r *pluginResponseRecorder) Header() http.Header { return r.header }
func (r *pluginResponseRecorder) WriteHeader(status int) {
	r.status = status
}
func (r *pluginResponseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}

func assertPluginTransportError(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var envelope struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("error is not JSON: %v body=%s", err, body)
	}
	if len(envelope.Error) != 4 || envelope.Error["code"] != wantCode || envelope.Error["message"] == "" || envelope.Error["hint"] == "" {
		t.Fatalf("bad four-field error: %s", body)
	}
	if _, ok := envelope.Error["retryable"].(bool); !ok {
		t.Fatalf("retryable is not bool: %s", body)
	}
}
