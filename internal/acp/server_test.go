package acp

import (
	"bufio"
	"context"
	"dshgo/internal/session"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/agent"
)

// fakeAgent 按脚本发事件，用来在不碰模型的前提下测协议投影。
var errNotExist = fmt.Errorf("会话不存在")

type fakeAgent struct {
	lastMode   string
	events     []agent.Event
	stop       string
	blockOnCtx bool
	mu         sync.Mutex
}

func (a *fakeAgent) Prompt(ctx context.Context, sessionID, cwd, text string, emit func(agent.Event) error) (string, map[string]any, error) {
	if a.blockOnCtx {
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	for _, ev := range a.events {
		if err := emit(ev); err != nil {
			return "", nil, err
		}
	}
	stop := a.stop
	if stop == "" {
		stop = "end_turn"
	}
	return stop, map[string]any{"inputTokens": 1}, nil
}

func (a *fakeAgent) Close(string) error { return nil }

func (a *fakeAgent) Fork(ctx context.Context, sessionID, cwd string) (string, error) {
	return "forked-" + sessionID, nil
}

func (a *fakeAgent) Load(ctx context.Context, sessionID, cwd string, emit func(agent.Event) error) error {
	if sessionID == "missing" {
		return errNotExist
	}
	return emit(agent.Event{Kind: agent.EventText, Text: "历史里的第一条"})
}

func (a *fakeAgent) Delete(ctx context.Context, sessionID, cwd string) error { return nil }

func (a *fakeAgent) Shutdown() error { return nil }

func (a *fakeAgent) ListSessions(ctx context.Context, cwd string) ([]session.Listing, error) {
	return []session.Listing{{SessionID: "s1", Cwd: cwd, Summary: "摘要", Readable: true}}, nil
}

func (a *fakeAgent) SetMode(ctx context.Context, sessionID, modeID string) (string, error) {
	if modeID != "default" {
		return "", fmt.Errorf("未知模式 %q", modeID)
	}
	a.lastMode = modeID
	return modeID, nil
}

func (a *fakeAgent) Resume(ctx context.Context, sessionID, cwd string) error {
	if sessionID == "unknown" {
		return fmt.Errorf("没有这个会话")
	}
	return nil
}

type harness struct {
	srv     *Server
	in      *io.PipeWriter
	msgs    chan map[string]any
	pending []map[string]any
	cancel  context.CancelFunc
	done    chan error
}

func start(t *testing.T, ag Agent) *harness {
	t.Helper()
	// client → server
	srvIn, cliOut := io.Pipe()
	// server → client
	cliIn, srvOut := io.Pipe()

	srv := &Server{In: srvIn, Out: srvOut, Agent: ag, Name: "dsh-go-acp", Version: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	msgs := make(chan map[string]any, 64)
	go func() {
		sc := bufio.NewScanner(cliIn)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
				msgs <- m
			}
		}
		close(msgs)
	}()

	return &harness{srv: srv, in: cliOut, msgs: msgs, cancel: cancel, done: done}
}

func (h *harness) send(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) recv(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m, ok := <-h.msgs:
		if !ok {
			t.Fatal("连接已关闭")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等消息超时")
	}
	return nil
}

// recvResult 跳过通知，直到拿到与给定 id 对应的应答。
func (h *harness) recvResult(t *testing.T, id float64) map[string]any {
	t.Helper()
	for {
		m := h.recv(t)
		if m["method"] == MethodSessionUpdate {
			h.pending = append(h.pending, m)
			continue
		}
		if got, ok := m["id"].(float64); ok && got == id {
			return m
		}
		t.Fatalf("收到意外消息: %+v（已缓存通知 %d 条）", m, len(h.pending))
	}
}

func (h *harness) updates(t *testing.T) []map[string]any {
	t.Helper()
	// 还在管道里的通知先收干净。
	for {
		select {
		case m := <-h.msgs:
			if m["method"] == MethodSessionUpdate {
				h.pending = append(h.pending, m)
			}
			continue
		case <-time.After(30 * time.Millisecond):
			return h.pending
		}
	}
}

// closeInput 关掉客户端→服务端这条管道，模拟"客户端直接消失"。
func (h *harness) closeInput() {
	_ = h.in.Close()
}

func (h *harness) close() {
	h.cancel()
	h.in.Close()
}

func TestInitialize(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": 1}})
	res := h.recvResult(t, 1)
	r := res["result"].(map[string]any)
	if r["protocolVersion"].(float64) != 1 {
		t.Fatalf("protocolVersion = %v", r["protocolVersion"])
	}
	info := r["agentInfo"].(map[string]any)
	if info["name"] != "dsh-go-acp" {
		t.Fatalf("agentInfo = %+v", info)
	}
}

func TestSessionNewRequiresAbsoluteCwd(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new",
		"params": map[string]any{"cwd": "relative/path", "mcpServers": []any{}}})
	res := h.recvResult(t, 2)
	if res["error"] == nil {
		t.Fatal("相对路径 cwd 必须被拒")
	}
}

func TestPromptStreamsUpdates(t *testing.T) {
	ag := &fakeAgent{events: []agent.Event{
		{Kind: agent.EventAssistantMsg, Step: 1},
		{Kind: agent.EventReasoning, Text: "先看目录"},
		{Kind: agent.EventText, Text: "好的"},
		{Kind: agent.EventToolCall, CallID: "c1", ToolName: "glob", Arguments: `{"pattern":"*.go"}`},
		{Kind: agent.EventToolResult, CallID: "c1", ToolName: "glob"},
		{Kind: agent.EventTurnEnd, Finish: "completed"},
	}}
	h := start(t, ag)
	defer h.close()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/new",
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}})
	sid := h.recvResult(t, 1)["result"].(map[string]any)["sessionId"].(string)

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/prompt",
		"params": map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "数一下"}}}})

	res := h.recvResult(t, 2)
	if res["error"] != nil {
		t.Fatalf("prompt 失败: %+v", res["error"])
	}
	if got := res["result"].(map[string]any)["stopReason"]; got != "end_turn" {
		t.Fatalf("stopReason = %v", got)
	}

	// 通知可能还在管道里：再收一会儿。
	time.Sleep(50 * time.Millisecond)
	notes := h.updates(t)
	var kinds []string
	for _, n := range notes {
		p := n["params"].(map[string]any)
		u := p["update"].(map[string]any)
		kinds = append(kinds, u["sessionUpdate"].(string))
		if u["sessionUpdate"] == "tool_call" {
			if u["status"] != "in_progress" || u["toolCallId"] != "c1" {
				t.Fatalf("tool_call 载荷 = %+v", u)
			}
			if _, ok := u["rawInput"].(map[string]any); !ok {
				t.Fatalf("rawInput 应是解析后的对象: %T", u["rawInput"])
			}
		}
	}
	want := []string{"agent_thought_chunk", "agent_message_chunk", "tool_call", "tool_call_update"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("update 序列 = %v，期望 %v", kinds, want)
	}
}

func TestCancelReturnsCancelled(t *testing.T) {
	ag := &fakeAgent{blockOnCtx: true}
	h := start(t, ag)
	defer h.close()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/new",
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}})
	sid := h.recvResult(t, 1)["result"].(map[string]any)["sessionId"].(string)

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/prompt",
		"params": map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "跑"}}}})
	time.Sleep(50 * time.Millisecond)
	h.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/cancel",
		"params": map[string]any{"sessionId": sid}})

	res := h.recvResult(t, 2)
	r := res["result"].(map[string]any)
	if r["stopReason"] != "cancelled" {
		t.Fatalf("stopReason = %v，期望 cancelled", r["stopReason"])
	}
}

func TestCallClientRoundTrip(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()

	type answer struct {
		raw []byte
		err error
	}
	done := make(chan answer, 1)
	go func() {
		raw, err := h.srv.CallClient(context.Background(), MethodRequestPermission, map[string]any{
			"sessionId": "s", "options": []any{map[string]any{"optionId": "allow-once"}},
		})
		done <- answer{raw, err}
	}()

	req := h.recv(t)
	if req["method"] != MethodRequestPermission {
		t.Fatalf("服务端应发出 request_permission，收到 %+v", req)
	}
	id := req["id"]
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"},
	}})

	select {
	case a := <-done:
		if a.err != nil {
			t.Fatal(a.err)
		}
		if !strings.Contains(string(a.raw), "allow-once") {
			t.Fatalf("应答没传回来: %s", a.raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CallClient 没有返回")
	}
}

func TestCallClientHonorsContext(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := h.srv.CallClient(ctx, MethodRequestPermission, map[string]any{}); err == nil {
		t.Fatal("客户端不应答时必须超时返回")
	} else {
		_ = h.recv(t) // 吃掉那条发出去的请求
	}
}

func TestSessionResume(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": MethodSessionResume,
		"params": map[string]any{"sessionId": "unknown", "cwd": t.TempDir()}})
	if res := h.recvResult(t, 1); res["error"] == nil {
		t.Fatal("恢复不存在的会话必须报错")
	}

	cwd := t.TempDir()
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": MethodSessionResume,
		"params": map[string]any{"sessionId": "s1", "cwd": cwd}})
	res := h.recvResult(t, 2)
	if res["error"] != nil {
		t.Fatalf("恢复失败: %+v", res["error"])
	}
	if got := res["result"].(map[string]any)["sessionId"]; got != "s1" {
		t.Fatalf("sessionId = %v", got)
	}
}

func TestSessionFork(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": MethodSessionNew,
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}})
	sid := h.recvResult(t, 1)["result"].(map[string]any)["sessionId"].(string)

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/fork",
		"params": map[string]any{"sessionId": sid}})
	res := h.recvResult(t, 2)
	if res["error"] != nil {
		t.Fatalf("fork 失败: %+v", res["error"])
	}
	newID := res["result"].(map[string]any)["sessionId"].(string)
	if newID != "forked-"+sid {
		t.Fatalf("新会话 id = %q", newID)
	}

	// 未知会话要报错，不能凭空造一个。
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/fork",
		"params": map[string]any{"sessionId": "nope"}})
	if h.recvResult(t, 3)["error"] == nil {
		t.Fatal("未知会话 fork 必须报错")
	}
}

func TestUnknownMethod(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "providers/list", "params": map[string]any{}})
	res := h.recvResult(t, 9)
	e := res["error"].(map[string]any)
	if int(e["code"].(float64)) != CodeMethodNotFound {
		t.Fatalf("error = %+v", e)
	}
}

func TestSessionLoadReplaysHistory(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/load",
		"params": map[string]any{"sessionId": "s1", "cwd": t.TempDir()}})
	res := h.recvResult(t, 1)
	if res["error"] != nil {
		t.Fatalf("load 失败: %+v", res["error"])
	}
	if got := res["result"].(map[string]any)["sessionId"]; got != "s1" {
		t.Fatalf("sessionId = %v", got)
	}
	// 应该先收到重放的历史 update（recvResult 会把途中的通知缓存起来）。
	us := h.updates(t)
	if len(us) == 0 {
		t.Fatal("load 应该重放历史 update，但一条都没收到")
	}
	first := us[0]["params"].(map[string]any)["update"].(map[string]any)
	if first["sessionUpdate"] != "agent_message_chunk" {
		t.Fatalf("重放的 update 类型 = %v", first["sessionUpdate"])
	}

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/load",
		"params": map[string]any{"sessionId": "missing", "cwd": t.TempDir()}})
	if h.recvResult(t, 2)["error"] == nil {
		t.Fatal("载入不存在的会话必须报错")
	}
}

func TestSessionDelete(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": MethodSessionNew,
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}})
	sid := h.recvResult(t, 1)["result"].(map[string]any)["sessionId"].(string)

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/delete",
		"params": map[string]any{"sessionId": sid, "cwd": t.TempDir()}})
	if res := h.recvResult(t, 2); res["error"] != nil {
		t.Fatalf("删除失败: %+v", res["error"])
	}
	// 删完之后这个会话不该还能 prompt。
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}}})
	if h.recvResult(t, 3)["error"] == nil {
		t.Fatal("删掉的会话不该还能用")
	}
}

func TestSessionSetMode(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/set_mode",
		"params": map[string]any{"sessionId": "s1", "modeId": "default"}})
	if res := h.recvResult(t, 1); res["error"] != nil {
		t.Fatalf("设置 default 失败: %+v", res["error"])
	}
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/set_mode",
		"params": map[string]any{"sessionId": "s1", "modeId": "turbo"}})
	if h.recvResult(t, 2)["error"] == nil {
		t.Fatal("未知模式必须报错，不能静默接受")
	}
}

func TestSessionNewWithMode(t *testing.T) {
	fa := &fakeAgent{}
	h := start(t, fa)
	defer h.close()

	// 正常：模式在建会话时就定下来。
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": MethodSessionNew,
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}, "mode": "default"}})
	res := h.recvResult(t, 1)
	if res["error"] != nil {
		t.Fatalf("带合法 mode 建会话失败: %+v", res["error"])
	}
	if fa.lastMode != "default" {
		t.Fatalf("SetMode 收到 %q", fa.lastMode)
	}

	// 非法模式：必须当场拒绝，不能建出一个"全权限但标着只读"的会话。
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": MethodSessionNew,
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}, "mode": "turbo"}})
	if h.recvResult(t, 2)["error"] == nil {
		t.Fatal("未知模式必须报错")
	}
}

// 取消之后会话必须能继续用。这条最容易被忽略：
// 如果 busy 标志没清，下一次 prompt 会被"该会话已有在飞的 prompt"挡掉。
func TestSessionUsableAfterCancel(t *testing.T) {
	ag := &fakeAgent{blockOnCtx: true}
	h := start(t, ag)
	defer h.close()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": MethodSessionNew,
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}})
	sid := h.recvResult(t, 1)["result"].(map[string]any)["sessionId"].(string)

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": MethodSessionPrompt,
		"params": map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "跑"}}}})
	time.Sleep(50 * time.Millisecond)
	h.send(t, map[string]any{"jsonrpc": "2.0", "method": MethodSessionCancel,
		"params": map[string]any{"sessionId": sid}})
	if res := h.recvResult(t, 2); res["error"] != nil {
		t.Fatalf("取消后 prompt 应返回结果而不是错误: %+v", res["error"])
	}

	// 让 fake agent 这次立刻返回
	ag.blockOnCtx = false

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 3, "method": MethodSessionPrompt,
		"params": map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "再来"}}}})
	res := h.recvResult(t, 3)
	if res["error"] != nil {
		t.Fatalf("取消之后会话应该还能用，却报错: %+v", res["error"])
	}
}

// 客户端断连（stdin EOF）时：在飞的 turn 要被取消，Serve 要干净返回。
func TestEOFDisconnectsAndCancels(t *testing.T) {
	ag := &fakeAgent{blockOnCtx: true}
	h := start(t, ag)
	defer h.close()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": MethodSessionNew,
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}})
	sid := h.recvResult(t, 1)["result"].(map[string]any)["sessionId"].(string)

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": MethodSessionPrompt,
		"params": map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "跑"}}}})
	time.Sleep(50 * time.Millisecond)

	// 直接关掉客户端这一侧的写端 = 断连
	h.closeInput()

	select {
	case <-h.done:
		// Serve 返回了，正是期望
	case <-time.After(3 * time.Second):
		t.Fatal("客户端断连后服务端没有退出（在飞的 turn 把读循环卡住了？）")
	}
}

func TestSessionListUsesAgent(t *testing.T) {
	h := start(t, &fakeAgent{})
	defer h.close()
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": MethodSessionList,
		"params": map[string]any{"cwd": "/tmp/x"}})
	res := h.recvResult(t, 1)
	if res["error"] != nil {
		t.Fatalf("列会话失败: %+v", res["error"])
	}
	ss := res["result"].(map[string]any)["sessions"].([]any)
	if len(ss) != 1 {
		t.Fatalf("应该返回 1 条，得到 %d", len(ss))
	}
	first := ss[0].(map[string]any)
	if first["sessionId"] != "s1" || first["summary"] != "摘要" {
		t.Fatalf("返回内容不对: %+v", first)
	}
}

// 找不到会话时，协议层只该说"没有这个会话"。
// 原来直接把 err 拼进去，客户端看到的是宿主路径 + 三层嵌套：
//
//	载入失败: 读会话失败: 读会话文件失败: open <home>/sessions/.../session.v4.jsonl.zstd
//
// 路径是宿主的实现细节，不该出现在协议里。
func TestSessionErrTextHidesPaths(t *testing.T) {
	inner := fmt.Errorf("读会话文件失败: %w", fs.ErrNotExist)
	err := fmt.Errorf("读会话失败: %w", inner)

	msg := sessionErrText("载入", "nope", err)
	if strings.Contains(msg, "session.v4.jsonl") || strings.Contains(msg, "sessions") {
		t.Fatalf("不该泄漏宿主路径: %q", msg)
	}
	if !strings.Contains(msg, "找不到会话 nope") {
		t.Fatalf("应该说找不到这个会话: %q", msg)
	}

	// 其它错误原样透出——不能把"真出错了"也说成"没有这个会话"
	other := sessionErrText("载入", "x", fmt.Errorf("权限不足"))
	if !strings.Contains(other, "权限不足") || strings.Contains(other, "找不到会话") {
		t.Fatalf("非 not-exist 错误应原样保留: %q", other)
	}
}
