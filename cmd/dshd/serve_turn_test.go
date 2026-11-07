package main

import (
	"bytes"
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

	"dshgo/internal/llm"
	"dshgo/internal/session"
)

func createModelWireSession(t *testing.T, home, cwd, id string) string {
	t.Helper()
	lg, err := session.CreateSession(home, cwd, id, "default")
	if err != nil {
		t.Fatal(err)
	}
	path := lg.Path
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func newModelRequestRecorder(t *testing.T) (base string, requests <-chan llm.Request, count *atomic.Int32) {
	t.Helper()
	seen := make(chan llm.Request, 16)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("请求路径 = %q", r.URL.Path)
		}
		// llm.Message.Content 只有 MarshalJSON（字段标签是 json:"-"），
		// 所以先按线上 JSON 解码，再还原成 llm.Request 做断言。
		var wire struct {
			Model     string `json:"model"`
			Reasoning string `json:"reasoning_effort"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Errorf("解码 llm.Request: %v", err)
		}
		req := llm.Request{Model: wire.Model, Reasoning: wire.Reasoning}
		for _, msg := range wire.Messages {
			req.Messages = append(req.Messages, llm.Message{Role: msg.Role, Content: msg.Content})
		}
		seen <- req
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", seen, &calls
}

func runModelAndReadRequest(t *testing.T, r *modelRunner, requests <-chan llm.Request, sid, prompt string) llm.Request {
	t.Helper()
	if err := r.Run(context.Background(), sid, "turn-"+prompt, prompt); err != nil {
		t.Fatalf("Run(%s): %v", sid, err)
	}
	req := <-requests
	if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Content != prompt {
		t.Fatalf("请求没有消费本轮 prompt %q: %+v", prompt, req.Messages)
	}
	return req
}

// 旧实现只会读 state/session-model.json，发出的真实 llm.Request 仍是固定模型。
// 这里不断言 JSON 内容，而是让每一轮真的打到 HTTP 端点并检查 model。
func TestModelRunnerSessionModelOverrideLifecycle(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	createModelWireSession(t, home, cwd, "session-a")
	createModelWireSession(t, home, cwd, "session-b")
	base, requests, _ := newModelRequestRecorder(t)
	runner := &modelRunner{home: home, base: base, model: "default-model"}
	overrides := newModelOverrideStore(home)

	if err := overrides.Set("session-a", "model-x"); err != nil {
		t.Fatal(err)
	}
	if got := runModelAndReadRequest(t, runner, requests, "session-a", "a-override").Model; got != "model-x" {
		t.Fatalf("A 设置后的下一轮 model = %q，要 model-x", got)
	}
	if got := runModelAndReadRequest(t, runner, requests, "session-b", "b-default").Model; got != "default-model" {
		t.Fatalf("B 不应受 A 影响，model = %q", got)
	}
	if err := overrides.Set("session-a", ""); err != nil {
		t.Fatal(err)
	}
	if got := runModelAndReadRequest(t, runner, requests, "session-a", "a-cleared").Model; got != "default-model" {
		t.Fatalf("清空后的下一轮应回默认，model = %q", got)
	}

	if err := overrides.Set("session-a", "model-after-restart"); err != nil {
		t.Fatal(err)
	}
	restartedStore := newModelOverrideStore(home)
	if err := restartedStore.Load(); err != nil {
		t.Fatal(err)
	}
	if got := restartedStore.Get("session-a"); got != "model-after-restart" {
		t.Fatalf("重启 Load 后 Get(A) = %q", got)
	}
	restartedRunner := &modelRunner{home: home, base: base, model: "default-model"}
	if got := runModelAndReadRequest(t, restartedRunner, requests, "session-a", "a-restarted").Model; got != "model-after-restart" {
		t.Fatalf("重启 Load 后的下一轮 model = %q，要 model-after-restart", got)
	}
}

// 损坏文件必须让这一轮明确失败，不能悄悄按默认模型发出请求并假成功。
func TestModelRunnerRejectsCorruptModelOverrideStore(t *testing.T) {
	home := t.TempDir()
	createModelWireSession(t, home, t.TempDir(), "session-a")
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state", "session-model.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, requests, calls := newModelRequestRecorder(t)
	runner := &modelRunner{home: home, base: base, model: "default-model"}
	err := runner.Run(context.Background(), "session-a", "turn-corrupt", "must-not-send")
	if err == nil || !strings.Contains(err.Error(), "session-model.json") {
		t.Fatalf("损坏 store 要返回含路径的错误，拿到 %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("损坏 store 后仍发出了 %d 次模型请求", got)
	}
	select {
	case req := <-requests:
		t.Fatalf("损坏 store 后仍收到请求: %+v", req)
	default:
	}
}

// set/get 是界面状态操作：不许借切换模型向 append-only v4 写任何事件。
func TestModelOverrideSetGetDoesNotWriteV4(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	pathA := createModelWireSession(t, home, cwd, "session-a")
	pathB := createModelWireSession(t, home, cwd, "session-b")
	beforeA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}

	store := newModelOverrideStore(home)
	if err := store.Set("session-a", "model-x"); err != nil {
		t.Fatal(err)
	}
	if got := store.Get("session-a"); got != "model-x" {
		t.Fatalf("Get(A) = %q", got)
	}
	if got := store.Get("session-b"); got != "" {
		t.Fatalf("Get(B) = %q", got)
	}
	if err := store.Set("session-a", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("session-a", "model-restart"); err != nil {
		t.Fatal(err)
	}
	restarted := newModelOverrideStore(home)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if got := restarted.Get("session-a"); got != "model-restart" {
		t.Fatalf("重启后 Get(A) = %q", got)
	}

	afterA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	afterB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeA, afterA) || !bytes.Equal(beforeB, afterB) {
		t.Fatal("session.setModel/Get 改写了 v4 会话文件")
	}
}
