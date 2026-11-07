package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dshgo/internal/agent"
	"dshgo/internal/llm"
	"dshgo/internal/spi"
)

// capacityPromptServer 记录真实 HTTP 请求体，并回一条带 usage 的完整响应。
// 这样同一条测试能同时钉住 max_tokens 与 ACP usage_update 的分母。
func capacityPromptServer(t *testing.T) (string, chan llm.Request) {
	t.Helper()
	reqs := make(chan llm.Request, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解码模型请求: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reqs <- req
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, reqs
}

func newCapacityPromptRunner(t *testing.T, base, model string) *acpRunner {
	t.Helper()
	r := newACPRunner(&fakeReg{tools: map[string]spi.Tool{}}, &recorder{}, base, model, t.TempDir(), 2)
	t.Cleanup(func() { _ = r.Shutdown() })
	return r
}

func onlyCapacityRequest(t *testing.T, reqs chan llm.Request) llm.Request {
	t.Helper()
	select {
	case req := <-reqs:
		return req
	default:
		t.Fatal("没有捕获到常规模型请求")
		return llm.Request{}
	}
}

func usageSize(events []agent.Event) int {
	for _, event := range events {
		if event.Kind == agent.EventUsage {
			return event.UsageSize
		}
	}
	return 0
}

func TestACPUndeclaredCustomModelUsesDecimalCapacityDefaults(t *testing.T) {
	base, reqs := capacityPromptServer(t)
	r := newCapacityPromptRunner(t, base, "custom/undeclared")

	// 不在请求前提前 Fatal：旧实现的 128K 窗口和缺失 max_tokens 是两个独立缺陷，都要留下证据。
	if got := r.ctxWindow; got != 1000000 {
		t.Errorf("未声明自定义模型 context = %d，期望十进制 1000000", got)
	}
	events := promptOnce(t, r, "custom-session")
	req := onlyCapacityRequest(t, reqs)
	if got := req.MaxTokens; got != 128000 {
		t.Errorf("常规请求 max_tokens = %d，期望 128000", got)
	}
	if got := usageSize(events); got != 1000000 {
		t.Errorf("usage_update size = %d，期望 1000000", got)
	}
}

func TestACPExplicitCapacityOverridesDefaults(t *testing.T) {
	base, reqs := capacityPromptServer(t)
	r := newCapacityPromptRunner(t, base, "custom/small")
	// 与 cmdACP 的 --context-window=32000 --max-output=8000 走同一个入口。
	r.setModelCapacity(llm.ModelCapacity{ContextWindow: 32000, MaxOutput: 8000})
	if got := r.ctxWindow; got != 32000 {
		t.Errorf("显式 --context-window 后 context = %d，期望 32000", got)
	}
	events := promptOnce(t, r, "explicit-session")
	req := onlyCapacityRequest(t, reqs)
	if got := req.MaxTokens; got != 8000 {
		t.Errorf("显式 --max-output 后 max_tokens = %d，期望 8000", got)
	}
	if got := usageSize(events); got != 32000 {
		t.Errorf("显式覆盖后 usage_update size = %d，期望 32000", got)
	}
}

func TestACPModelAndSessionCapacityIsolation(t *testing.T) {
	base, reqs := capacityPromptServer(t)
	customA := newCapacityPromptRunner(t, base, "custom/a")
	customB := newCapacityPromptRunner(t, base, "custom/b")
	customB.setModelCapacity(llm.ModelCapacity{ContextWindow: 32000, MaxOutput: 8000})

	cwdA, cwdB := t.TempDir(), t.TempDir()
	promptAt := func(r *acpRunner, sessionID, cwd string) []agent.Event {
		t.Helper()
		var events []agent.Event
		if _, _, err := r.Prompt(context.Background(), sessionID, cwd, "做点事", func(event agent.Event) error {
			events = append(events, event)
			return nil
		}); err != nil {
			t.Fatalf("Prompt(%s) 出错: %v", sessionID, err)
		}
		return events
	}
	eventsA1 := promptAt(customA, "session-a-1", cwdA)
	eventsB := promptAt(customB, "session-b", cwdB)
	eventsA2 := promptAt(customA, "session-a-2", cwdA)
	for i, want := range []struct {
		model     string
		maxTokens int
		ctxWindow int
		events    []agent.Event
	}{
		{model: "custom/a", maxTokens: 128000, ctxWindow: 1000000, events: eventsA1},
		{model: "custom/b", maxTokens: 8000, ctxWindow: 32000, events: eventsB},
		{model: "custom/a", maxTokens: 128000, ctxWindow: 1000000, events: eventsA2},
	} {
		req := onlyCapacityRequest(t, reqs)
		if req.Model != want.model || req.MaxTokens != want.maxTokens {
			t.Errorf("第 %d 个会话请求 model=%q max_tokens=%d，期望 %q/%d", i+1, req.Model, req.MaxTokens, want.model, want.maxTokens)
		}
		if got := usageSize(want.events); got != want.ctxWindow {
			t.Errorf("第 %d 个会话 usage_update size=%d，期望 %d；容量跨会话串值", i+1, got, want.ctxWindow)
		}
	}
}

func TestACPKnownModelKeepsExistingCapacity(t *testing.T) {
	base, reqs := capacityPromptServer(t)
	r := newCapacityPromptRunner(t, base, "group/deepseek-v4.1-flash-1017")
	if got := r.ctxWindow; got != 128000 {
		t.Errorf("内置已知模型 context = %d，期望保持 128000", got)
	}
	events := promptOnce(t, r, "known-session")
	req := onlyCapacityRequest(t, reqs)
	if got := req.MaxTokens; got != 0 {
		t.Errorf("内置已知模型新增了 max_tokens=%d，期望保持缺省 0", got)
	}
	if got := usageSize(events); got != 128000 {
		t.Errorf("已知模型 usage_update size = %d，期望保持 128000", got)
	}
}
