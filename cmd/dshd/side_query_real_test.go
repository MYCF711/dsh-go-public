package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/spi"
)

type w149SeenSideRequest struct {
	Model   string
	Prompt  string
	History []string
	Tools   int
}

type w149SideGateway struct {
	cancelled chan string
	releaseA  chan struct{}
	mu        sync.Mutex
	seen      map[string]w149SeenSideRequest
}

func newW149SideGateway() *w149SideGateway {
	return &w149SideGateway{
		cancelled: make(chan string, 8),
		releaseA:  make(chan struct{}), seen: map[string]w149SeenSideRequest{},
	}
}

func (g *w149SideGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	var request struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	textOf := func(raw json.RawMessage) string {
		var text string
		_ = json.Unmarshal(raw, &text)
		return text
	}
	last := request.Messages[len(request.Messages)-1]
	seen := w149SeenSideRequest{Model: request.Model, Prompt: textOf(last.Content), Tools: len(request.Tools)}
	for _, message := range request.Messages[:len(request.Messages)-1] {
		seen.History = append(seen.History, textOf(message.Content))
	}
	g.mu.Lock()
	g.seen[seen.Prompt] = seen
	g.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	emit := func(content, finish string) bool {
		payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": content}, "finish_reason": finish,
		}}})
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	done := func() { _, _ = fmt.Fprint(w, "data: [DONE]\n\n") }
	switch seen.Prompt {
	case "A-block":
		if !emit("A-start", "") {
			return
		}
		select {
		case <-g.releaseA:
		case <-r.Context().Done():
			return
		}
		emit("-finish", "stop")
		done()
	case "B-cancel", "B-cancel-nonstream", "close-me":
		if !emit("before-"+seen.Prompt, "") {
			return
		}
		<-r.Context().Done()
		g.cancelled <- seen.Prompt
		// 迟到正文即使到达 HTTP 层，也不能再成为 side.query 的成功回答。
		emit("late-"+seen.Prompt, "stop")
		done()
	case "stream-A":
		emit("side ", "")
		emit("answer", "stop")
		done()
	default:
		emit(map[string]string{
			"whole-A": "whole answer", "after-restart": "restart answer",
		}[seen.Prompt], "stop")
		done()
	}
}

func (g *w149SideGateway) request(t *testing.T, prompt string) w149SeenSideRequest {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	seen, ok := g.seen[prompt]
	if !ok {
		t.Fatalf("gateway 未收到 %q；已有 %+v", prompt, g.seen)
	}
	return seen
}

func waitW149Signal(t *testing.T, signals <-chan string, want string) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case got := <-signals:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("等待信号 %q 超时", want)
		}
	}
}

func waitW149GatewayRequest(t *testing.T, gateway *w149SideGateway, prompt string, failure <-chan w149StreamResult) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		gateway.mu.Lock()
		_, exists := gateway.seen[prompt]
		gateway.mu.Unlock()
		if exists {
			return
		}
		select {
		case got := <-failure:
			t.Fatalf("%s 在 gateway 开始前已结束: err=%v lines=%+v raw=%q", prompt, got.err, got.lines, got.raw)
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("等待 gateway 请求 %q 超时", prompt)
		}
	}
}

type w149StreamResult struct {
	lines []map[string]any
	raw   []string
	err   error
}

type w149NonStreamResult struct {
	status   int
	envelope map[string]any
	err      error
}

func startW149SideNonStream(client *http.Client, base, sid, requestID, prompt string) <-chan w149NonStreamResult {
	result := make(chan w149NonStreamResult, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"id": sid, "requestId": requestID, "prompt": prompt})
		req, err := http.NewRequest(http.MethodPost, base+"/api/side.query", bytes.NewReader(body))
		if err != nil {
			result <- w149NonStreamResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			result <- w149NonStreamResult{err: err}
			return
		}
		defer resp.Body.Close()
		out := w149NonStreamResult{status: resp.StatusCode}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			out.err = err
			result <- out
			return
		}
		out.err = json.Unmarshal(raw, &out.envelope)
		result <- out
	}()
	return result
}

func startW149SideStream(client *http.Client, base, sid, requestID, prompt string) <-chan w149StreamResult {
	result := make(chan w149StreamResult, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"id": sid, "requestId": requestID, "prompt": prompt, "stream": true})
		req, err := http.NewRequest(http.MethodPost, base+"/api/side.query", bytes.NewReader(body))
		if err != nil {
			result <- w149StreamResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			result <- w149StreamResult{err: err}
			return
		}
		defer resp.Body.Close()
		out := w149StreamResult{}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			out.err = fmt.Errorf("status=%d body=%s", resp.StatusCode, raw)
			result <- out
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			out.raw = append(out.raw, line)
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				out.err = err
				result <- out
				return
			}
			out.lines = append(out.lines, decoded)
		}
		out.err = scanner.Err()
		result <- out
	}()
	return result
}

func waitW149SideStream(t *testing.T, result <-chan w149StreamResult) w149StreamResult {
	t.Helper()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("stream: %v raw=%q", got.err, got.raw)
		}
		return got
	case <-time.After(15 * time.Second):
		t.Fatal("side.query NDJSON 15 秒未收工")
		return w149StreamResult{}
	}
}

func requireW149Cancelled(t *testing.T, got w149StreamResult, requestID string) {
	t.Helper()
	if len(got.lines) == 0 {
		t.Fatal("cancel stream 没有 done 行")
	}
	last := got.lines[len(got.lines)-1]
	if last["type"] != "done" || last["requestId"] != requestID || last["cancelled"] != true || last["answer"] != nil {
		t.Fatalf("cancel done=%+v raw=%q", last, got.raw)
	}
	for _, line := range got.lines {
		if strings.Contains(fmt.Sprint(line["text"]), "late-") {
			t.Fatalf("取消后出现迟到正文: %+v", got.lines)
		}
	}
}

func seedW149SideSession(t *testing.T, home, sid, marker string) string {
	t.Helper()
	lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	systemText := "Runtime: dsh-go.\nModel: model-a.\n\n需要信息就调用工具，不要猜。\n只读工具：read。"
	events := []struct {
		kind string
		data any
	}{
		{"system/message", session.SystemMessageData(1, 1, systemText)},
		{"request/context", map[string]any{"model": "model-a", "contextWindow": 1000}},
		{"request/header", map[string]any{"header": map[string]any{"tools": []any{map[string]any{"name": "read", "description": "read"}}}}},
		{"user/message", session.UserMessage([]spi.Block{{Type: "text", Text: marker}})},
		{"assistant/message", session.AssistantMessageEvent(1, 1, []map[string]any{session.TextBlock(marker + "-assistant")}, "stop", map[string]any{"inputTokens": 70, "outputTokens": 30, "totalTokens": 100})},
	}
	for _, event := range events {
		if err := lg.Append(event.kind, event.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	return lg.Path
}

func snapshotW149Files(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func requireW149FilesUnchanged(t *testing.T, home string, before map[string]string) {
	t.Helper()
	after := snapshotW149Files(t, home)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("side.query 改写了父 home:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestW149RealDSHDSideQueryNonStreamingNDJSONABCancelCloseRestart(t *testing.T) {
	gateway := newW149SideGateway()
	httpGateway := httptest.NewServer(gateway)
	t.Cleanup(httpGateway.Close)
	t.Cleanup(httpGateway.CloseClientConnections)
	t.Setenv("DSHD_LLM_BASE", httpGateway.URL+"/v1")
	t.Setenv("DSHD_LLM_KEY", "")
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "")

	exe := buildPluginGlueDSHD(t)
	home := t.TempDir()
	const sidA, sidB = "session-w149-side-a", "session-w149-side-b"
	pathA := seedW149SideSession(t, home, sidA, "history-A-marker")
	pathB := seedW149SideSession(t, home, sidB, "history-B-marker")
	server := startPluginGlueServer(t, exe, home, t.TempDir())
	client := connectPluginGlueServer(t, server)
	streamClient := *client
	streamClient.Timeout = 0 // 长流由 side.cancel/close 与 go test 全局 timeout 收口。
	before := snapshotW149Files(t, home)
	initialContext := postPluginGlue(t, client, server.base, "context.usage", map[string]any{"id": sidA, "detail": "full"}, http.StatusOK)
	if initialContext["result"].(map[string]any)["used"] != float64(100) {
		t.Fatalf("real context.usage=%+v", initialContext)
	}

	whole := postPluginGlue(t, client, server.base, "side.query", map[string]any{"id": sidA, "requestId": "whole-1", "prompt": "whole-A"}, http.StatusOK)
	if whole["result"].(map[string]any)["answer"] != "whole answer" {
		t.Fatalf("non-stream=%+v", whole)
	}
	stream := waitW149SideStream(t, startW149SideStream(&streamClient, server.base, sidA, "stream-1", "stream-A"))
	if len(stream.lines) != 3 || stream.lines[0]["text"] != "side " || stream.lines[1]["text"] != "answer" || stream.lines[2]["answer"] != "side answer" {
		t.Fatalf("NDJSON=%+v", stream.lines)
	}

	aDone := startW149SideStream(&streamClient, server.base, sidA, "A-request", "A-block")
	bDone := startW149SideStream(&streamClient, server.base, sidB, "B-request", "B-cancel")
	waitW149GatewayRequest(t, gateway, "A-block", aDone)
	waitW149GatewayRequest(t, gateway, "B-cancel", bDone)
	postPluginGlue(t, client, server.base, "side.cancel", map[string]any{"id": sidB, "requestId": "B-request"}, http.StatusOK)
	requireW149Cancelled(t, waitW149SideStream(t, bDone), "B-request")
	waitW149Signal(t, gateway.cancelled, "B-cancel")
	select {
	case got := <-aDone:
		t.Fatalf("取消 B 却结束 A: %+v", got)
	default:
	}
	close(gateway.releaseA)
	aResult := waitW149SideStream(t, aDone)
	if aResult.lines[len(aResult.lines)-1]["answer"] != "A-start-finish" || aResult.lines[len(aResult.lines)-1]["cancelled"] == true {
		t.Fatalf("A 未独立收工: %+v", aResult.lines)
	}
	postPluginGlue(t, client, server.base, "side.cancel", map[string]any{"id": sidB, "requestId": "B-request"}, http.StatusNotFound)

	nonStreamDone := startW149SideNonStream(&streamClient, server.base, sidB, "nonstream-cancel", "B-cancel-nonstream")
	waitW149GatewayRequest(t, gateway, "B-cancel-nonstream", nil)
	postPluginGlue(t, client, server.base, "side.query", map[string]any{"id": sidB, "requestId": "nonstream-cancel", "prompt": "whole-A"}, http.StatusConflict)
	postPluginGlue(t, client, server.base, "side.cancel", map[string]any{"id": sidB, "requestId": "nonstream-cancel"}, http.StatusOK)
	select {
	case got := <-nonStreamDone:
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("non-stream cancel: status=%d err=%v envelope=%+v", got.status, got.err, got.envelope)
		}
		result := got.envelope["result"].(map[string]any)
		if result["requestId"] != "nonstream-cancel" || result["answer"] != nil || result["cancelled"] != true {
			t.Fatalf("non-stream cancel result=%+v", result)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("非流式 side.cancel 未结束请求")
	}
	waitW149Signal(t, gateway.cancelled, "B-cancel-nonstream")

	for prompt, marker := range map[string]string{"whole-A": "history-A-marker", "A-block": "history-A-marker", "B-cancel": "history-B-marker", "B-cancel-nonstream": "history-B-marker"} {
		seen := gateway.request(t, prompt)
		if seen.Tools != 0 || seen.Model == "" || !strings.Contains(strings.Join(seen.History, "\n"), marker) {
			t.Fatalf("%s 未使用独立无工具 runner 或历史串台: %+v", prompt, seen)
		}
	}
	requireW149FilesUnchanged(t, home, before)

	server.stop()
	server = startPluginGlueServer(t, exe, home, t.TempDir())
	client = connectPluginGlueServer(t, server)
	streamClient = *client
	streamClient.Timeout = 0
	restarted := postPluginGlue(t, client, server.base, "side.query", map[string]any{"id": sidA, "requestId": "restart-1", "prompt": "after-restart"}, http.StatusOK)
	if restarted["result"].(map[string]any)["answer"] != "restart answer" {
		t.Fatalf("restart side.query=%+v", restarted)
	}
	reopenedContext := postPluginGlue(t, client, server.base, "context.usage", map[string]any{"id": sidA, "detail": "full"}, http.StatusOK)
	if !reflect.DeepEqual(initialContext, reopenedContext) {
		t.Fatalf("restart 后 context 快照改变:\nbefore=%+v\nafter=%+v", initialContext, reopenedContext)
	}

	closeDone := startW149SideStream(&streamClient, server.base, sidA, "close-request", "close-me")
	waitW149GatewayRequest(t, gateway, "close-me", closeDone)
	postPluginGlue(t, client, server.base, "side.close", map[string]any{"id": sidA}, http.StatusOK)
	requireW149Cancelled(t, waitW149SideStream(t, closeDone), "close-request")
	waitW149Signal(t, gateway.cancelled, "close-me")
	postPluginGlue(t, client, server.base, "side.close", map[string]any{"id": "session-missing"}, http.StatusNotFound)
	requireW149FilesUnchanged(t, home, before)
	for _, path := range []string{pathA, pathB} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("W149_SIDE_REAL_SUMMARY OK=17 FAIL=0")
}
