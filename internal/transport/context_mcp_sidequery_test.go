package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func callW149(t *testing.T, server *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	server.mu.Lock()
	server.cookies["w149-test"] = struct{}{}
	server.mu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "w149-test"})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

func decodeW149Result(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("非 JSON: %v body=%s", err, recorder.Body.String())
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("缺 result: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return result
}

func TestW149ContextAndMCPBareContracts(t *testing.T) {
	if len(SessionMethods()) != 25 || len(ContextMCPMethods()) != 4 || len(SideQueryMethods()) != 3 {
		t.Fatalf("additive 方法污染 canonical: canonical=%d context=%d side=%d", len(SessionMethods()), len(ContextMCPMethods()), len(SideQueryMethods()))
	}
	var toggleEnabled bool
	server, err := NewServer(Options{
		ContextUsage: func(id, detail string) (ContextUsageView, error) {
			if id != "session-a" || detail != "full" {
				t.Fatalf("context params=%q/%q", id, detail)
			}
			sections := []ContextItemView{{Name: "system", Tokens: 30}}
			files := []ContextFileView{{Path: "D:/work/AGENTS.md", Tokens: 5}}
			skills := []ContextItemView{}
			tools := []ContextToolView{{Name: "search", Server: "docs", Tokens: 10}}
			return ContextUsageView{
				Used: 100, Max: 1000, Categories: []ContextCategory{{Name: "messages", Tokens: 100, Kind: "used"}},
				Sections: &sections, Files: &files, Skills: &skills, Tools: &tools,
			}, nil
		},
		MCPList: func(id string) ([]MCPServerView, error) { return nil, nil },
		MCPReconnect: func(_ context.Context, id, name string) error {
			if name == "broken" {
				return errors.New("dial failed exactly")
			}
			return nil
		},
		MCPToggle: func(_ context.Context, id, name string, enabled bool) error { toggleEnabled = enabled; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	usage := decodeW149Result(t, callW149(t, server, "context.usage", `{"id":"session-a","detail":"full"}`))
	if usage["used"] != float64(100) || usage["max"] != float64(1000) {
		t.Fatalf("usage=%+v", usage)
	}
	if usage["categories"].([]any)[0].(map[string]any)["name"] != "messages" || usage["sections"].([]any)[0].(map[string]any)["tokens"] != float64(30) ||
		usage["files"].([]any)[0].(map[string]any)["path"] != "D:/work/AGENTS.md" || len(usage["skills"].([]any)) != 0 ||
		usage["tools"].([]any)[0].(map[string]any)["server"] != "docs" {
		t.Fatalf("full categories/details 被丢失: %+v", usage)
	}
	empty := decodeW149Result(t, callW149(t, server, "mcp.list", `{"id":"session-a"}`))
	if servers, ok := empty["servers"].([]any); !ok || len(servers) != 0 {
		t.Fatalf("真实空目录=%+v", empty)
	}
	if got := decodeW149Result(t, callW149(t, server, "mcp.reconnect", `{"id":"session-a","name":"docs"}`)); got["ok"] != true {
		t.Fatalf("reconnect=%+v", got)
	}
	if got := decodeW149Result(t, callW149(t, server, "mcp.toggle", `{"id":"session-a","name":"docs","enabled":false}`)); got["ok"] != true || toggleEnabled {
		t.Fatalf("toggle=%+v enabled=%v", got, toggleEnabled)
	}
	for _, test := range []struct{ method, body string }{
		{"context.usage", `{"id":"session-a","detail":"summary"}`},
		{"mcp.toggle", `{"id":"session-a","name":"docs"}`},
		{"mcp.list", `{"params":{"id":"session-a"}}`},
	} {
		recorder := callW149(t, server, test.method, test.body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", test.method, recorder.Code, recorder.Body.String())
		}
	}
	recorder := callW149(t, server, "mcp.reconnect", `{"id":"session-a","name":"broken"}`)
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "dial failed exactly") {
		t.Fatalf("动作错误被改写: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestW149ContextUsageRejectsMalformedHostSuccess(t *testing.T) {
	for name, view := range map[string]ContextUsageView{
		"null-categories": {Used: 1},
		"negative-used":   {Used: -1, Categories: []ContextCategory{}},
		"negative-detail": {Used: 1, Categories: []ContextCategory{}, Sections: &[]ContextItemView{{Name: "system", Tokens: -1}}},
		"empty-name":      {Used: 1, Categories: []ContextCategory{{Name: "", Tokens: 1, Kind: "used"}}},
	} {
		t.Run(name, func(t *testing.T) {
			server, err := NewServer(Options{ContextUsage: func(string, string) (ContextUsageView, error) { return view, nil }})
			if err != nil {
				t.Fatal(err)
			}
			recorder := callW149(t, server, "context.usage", `{"id":"session-a","detail":"full"}`)
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("畸形成功被发出: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestW149SideQueryStreamingNonStreamingCancelClose(t *testing.T) {
	server, err := NewServer(Options{
		SideQuery: func(ctx context.Context, id, requestID, prompt string, onText func(string) error) (string, error) {
			switch prompt {
			case "cancel":
				if onText != nil {
					_ = onText("before")
				}
				return "", context.Canceled
			case "stream":
				_ = onText("side ")
				_ = onText("answer")
				return "side answer", nil
			case "fail":
				return "", errors.New("runner failed exactly")
			default:
				return "whole answer", nil
			}
		},
		SideCancel: func(id, requestID string) error { return nil },
		SideClose:  func(id string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	whole := decodeW149Result(t, callW149(t, server, "side.query", `{"id":"session-a","requestId":"r-1","prompt":"whole"}`))
	if whole["answer"] != "whole answer" {
		t.Fatalf("whole=%+v", whole)
	}
	stream := callW149(t, server, "side.query", `{"id":"session-a","requestId":"r-2","prompt":"stream","stream":true}`)
	lines := strings.Split(strings.TrimSpace(stream.Body.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"type":"text"`) || !strings.Contains(lines[2], `"answer":"side answer"`) {
		t.Fatalf("stream body=%s", stream.Body.String())
	}
	cancel := callW149(t, server, "side.query", `{"id":"session-a","requestId":"r-3","prompt":"cancel","stream":true}`)
	if !strings.Contains(cancel.Body.String(), `"cancelled":true`) || strings.Contains(cancel.Body.String(), "late") {
		t.Fatalf("cancel body=%s", cancel.Body.String())
	}
	failure := callW149(t, server, "side.query", `{"id":"session-a","requestId":"r-4","prompt":"fail","stream":true}`)
	if !strings.Contains(failure.Body.String(), `"type":"error"`) || !strings.Contains(failure.Body.String(), "runner failed exactly") {
		t.Fatalf("failure body=%s", failure.Body.String())
	}
	if got := decodeW149Result(t, callW149(t, server, "side.cancel", `{"id":"session-a","requestId":"r-3"}`)); got["ok"] != true {
		t.Fatalf("cancel ack=%+v", got)
	}
	if got := decodeW149Result(t, callW149(t, server, "side.close", `{"id":"session-a"}`)); got["ok"] != true {
		t.Fatalf("close ack=%+v", got)
	}
}
