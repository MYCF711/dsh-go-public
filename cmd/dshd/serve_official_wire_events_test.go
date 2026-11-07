package main

// `$events` 的**接线层**验收：真实审批请求 → waterfall 帧 → 客户端回答 →
// `$events/result` → 真实 approvalBroker 结算 → 子进程 stdin 收到应答 → 宿主继续。
//
// 这里没有一处打桩：broker 是 serve.go 用的那个 approvalBroker，传输层是真的
// transport.Server，挂载是真的 officialwire，wire 是真的 WebSocket 与真的 unary
// 信封。唯一被替代的是**子进程本身**（用 approvalStdin 顶替 io.WriteCloser）——
// 那正是 serve_approval_test.go 里既有的做法，也是"我们检查的边界"。
//
// 为什么这条回路必须存在：官方客户端的 `answer()` 里
//
//	const response = await this.connection.rpc.call('/api', '$events/result', {args: result}, signal)
//	if (!response.ok) throw new Error(response.error.message)
//
// 抛出后 `pumpEvents` 的 catch 会 `failed.abort(error)` —— 整个 generation 失败并重连
// （lib/types/client/remote-events.js:126-143）。所以"只做 ready 不做 result"在接上
// 事件源那天会变成连接级故障。

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/officialwire"
)

// frameValueOfWire 取 mux item 帧的 value 对象。
func frameValueOfWire(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	value, ok := frame["value"].(map[string]any)
	if !ok {
		t.Fatalf("item 帧的 value 不是对象：%v", frame)
	}
	return value
}

// readRawFrame 读一条原始帧（不解析 JSON），用于"不该再有帧"的否定断言。
//
// 不能用 wireWS.readJSON：那个在超时时 t.Fatalf，而这里"超时"正是要断言的结论。
func readRawFrame(w *wireWS, timeout time.Duration) (byte, []byte, error) {
	_ = w.conn.SetReadDeadline(time.Now().Add(timeout))
	defer w.conn.SetReadDeadline(time.Time{})
	var header [2]byte
	if _, err := io.ReadFull(w.br, header[:]); err != nil {
		return 0, nil, err
	}
	op := header[0] & 0x0f
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

// approvalStdin 是子进程 stdin 的替身：记下每一行写回。
//
// 不复用 serve_approval_test.go 的 fakeStdin：那个没有 Close()，而
// `newApprovalBroker` 要的是 io.WriteCloser。给它加方法等于改另一个文件的
// 夹具语义，不如在本文件里明确写出"这条测试需要什么"。
type approvalStdin struct {
	mu    sync.Mutex
	lines []string
}

func (s *approvalStdin) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.lines = append(s.lines, string(p))
	s.mu.Unlock()
	return len(p), nil
}

func (s *approvalStdin) Close() error { return nil }

// wait 等到出现一条以 prefix 开头的写回。
func (s *approvalStdin) wait(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, line := range s.lines {
			if strings.HasPrefix(line, prefix) {
				s.mu.Unlock()
				return line
			}
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	got := strings.Join(s.lines, "")
	s.mu.Unlock()
	t.Fatalf("等不到以 %q 开头的写回；实际收到：%q", prefix, got)
	return ""
}

// approvalWire 是一套"真 broker + 真事件源 + 真 officialwire"的夹具。
type approvalWire struct {
	harness *officialWireHarness
	broker  *approvalBroker
	source  *approvalEventSource
	stdin   *approvalStdin
}

// newApprovalWire 造夹具并把 broker 的 emit 接到事件源上。
//
// 这一行就是生产 serve.go:323 的同一个接法（那里接的是 `lr.emit`，这里接
// 事件源；两者都在"审批帧产生"这一刻被调用）。
func newApprovalWire(t *testing.T) *approvalWire {
	t.Helper()
	stdin := &approvalStdin{}
	broker := newApprovalBroker(stdin, nil)
	// 超时给足，免得这条 goroutine 抢在断言之前结算。
	broker.timeout = 60 * time.Second
	source := newApprovalEventSource(broker)
	if source == nil {
		t.Fatalf("newApprovalEventSource 返回 nil")
	}
	broker.setEmit(func(sessionID, typ string, data json.RawMessage) {
		source.OnApprovalFrame(sessionID, typ, data)
	})
	harness := newOfficialWireHarnessWith(t, func(deps *officialWireDeps) {
		deps.Events = source
	})
	return &approvalWire{harness: harness, broker: broker, source: source, stdin: stdin}
}

// openEventsStream 打开 `$events` 并返回 (socket, clientId)。
func (w *approvalWire) openEventsStream(t *testing.T) (*wireWS, string) {
	t.Helper()
	ws, response := dialWireMux(t, w.harness.mounted.URL, w.harness.officialCookies(t))
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("mux 升级应 101，实际 %d", response.StatusCode)
	}
	t.Cleanup(ws.Close)
	ws.writeText(t, `{"type":"open","streamId":"ev","endpoint":"$events","payload":{"args":{}}}`)
	ready := frameValueOfWire(t, ws.readJSON(t, 3*time.Second))
	if ready["type"] != "ready" {
		t.Fatalf("首帧必须是 ready：%v", ready)
	}
	clientID, _ := ready["clientId"].(string)
	if clientID == "" {
		t.Fatalf("ready 必须带 clientId：%v", ready)
	}
	return ws, clientID
}

// requestApproval 让**真实 broker** 登记一条审批（等价于子进程 stderr 那一行）。
func (w *approvalWire) requestApproval(t *testing.T, requestID, sessionID, tool string) {
	t.Helper()
	w.broker.handle(approvalReq{
		RequestID: requestID,
		SessionID: sessionID,
		Tool:      tool,
		Args:      json.RawMessage(`{"command":"echo hi"}`),
	})
}

// answer 发一条 `$events/result`。
func (w *approvalWire) answer(t *testing.T, rpcID, clientID, eventID, outcome string) map[string]any {
	t.Helper()
	args := `{"clientId":` + quoteWire(clientID) + `,"eventId":` + quoteWire(eventID) +
		`,"outcome":` + outcome + `}`
	status, payload := w.harness.post(t, "$events/result", wireEnvelope(rpcID, "$events/result", args))
	return wireResult(t, status, payload)
}

// required 是"这条 waterfall 帧必须满足官方 exact-key"的断言。
func requiredWaterfall(t *testing.T, value map[string]any, wantRequestID, wantSessionID string) string {
	t.Helper()
	if value["type"] != "waterfall" {
		t.Fatalf("应是 waterfall 帧：%v", value)
	}
	if value["event"] != approvalRequestEvent {
		t.Fatalf("event 应为 %s：%v", approvalRequestEvent, value)
	}
	if len(value) != 5 {
		t.Fatalf("waterfall 必须恰好五个键（官方 exact-key，多一个就 1008 断连）：%v", value)
	}
	eventID, _ := value["eventId"].(string)
	if eventID == "" {
		t.Fatalf("必须带 eventId：%v", value)
	}
	if value["agentId"] != wantSessionID {
		t.Fatalf("agentId 应为会话 id %q：%v", wantSessionID, value)
	}
	request, ok := value["request"].(map[string]any)
	if !ok {
		t.Fatalf("request 必须是对象：%v", value)
	}
	if request["requestId"] != wantRequestID {
		t.Fatalf("request.requestId 应为 %q：%v", wantRequestID, request)
	}
	if _, has := request["agent"]; has {
		t.Fatalf("request 不许带 agent（官方由客户端补）：%v", request)
	}
	if _, has := request["signal"]; has {
		t.Fatalf("request 不许带 signal：%v", request)
	}
	return eventID
}

// TestOfficialWireEventsApprovalWaterfallFullLoop 是要求 3 的**完整回路**：
// emit(waterfall) → 客户端 answer → 宿主继续。
func TestOfficialWireEventsApprovalWaterfallFullLoop(t *testing.T) {
	w := newApprovalWire(t)
	ws, clientID := w.openEventsStream(t)

	// ① 真实审批请求 → waterfall 帧
	w.requestApproval(t, "perm-1", "sess-1", "bash")
	frame := ws.readJSON(t, 3*time.Second)
	if frame["type"] != "item" || frame["streamId"] != "ev" {
		t.Fatalf("应是 item 帧：%v", frame)
	}
	eventID := requiredWaterfall(t, frameValueOfWire(t, frame), "perm-1", "sess-1")

	// ② 客户端回答（官方 outcome.kind=result 的形状）
	result := w.answer(t, "r-1", clientID, eventID, `{"kind":"result","value":{"optionId":"allow_once"}}`)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("$events/result 应 ok:true：%v", result)
	}
	if _, has := result["value"]; has {
		t.Fatalf("官方成功时 value 是 void 0（信封里不出现）：%v", result)
	}

	// ③ 宿主继续：子进程 stdin 真的收到那一行
	line := w.stdin.wait(t, approvalResponseTag)
	if !strings.Contains(line, `"requestId":"perm-1"`) || !strings.Contains(line, `"allow":true`) {
		t.Fatalf("写回子进程的行不对：%q", line)
	}

	// ④ 事件已经结算：再答一次是静默 ok（官方 receiveRemoteEventResult 的早返回），
	//    不会把"重复回答"变成错误——那会让客户端以为自己的 generation 坏了。
	again := w.answer(t, "r-2", clientID, eventID, `{"kind":"next"}`)
	if ok, _ := again["ok"].(bool); !ok {
		t.Fatalf("重复回答应静默 ok:true：%v", again)
	}
}

// TestOfficialWireEventsApprovalRejected 钉住 outcome.kind=rejected → 按拒绝结算。
func TestOfficialWireEventsApprovalRejected(t *testing.T) {
	w := newApprovalWire(t)
	ws, clientID := w.openEventsStream(t)

	w.requestApproval(t, "perm-2", "sess-2", "bash")
	eventID := requiredWaterfall(t, frameValueOfWire(t, ws.readJSON(t, 3*time.Second)), "perm-2", "sess-2")

	result := w.answer(t, "r-3", clientID, eventID,
		`{"kind":"rejected","error":{"name":"Error","message":"客户端处理时抛了","code":"client/boom"}}`)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("应 ok:true：%v", result)
	}
	line := w.stdin.wait(t, approvalResponseTag)
	if !strings.Contains(line, `"allow":false`) {
		t.Fatalf("rejected 必须按拒绝结算（工具一定不执行）：%q", line)
	}
}

// TestOfficialWireEventsApprovalTimeoutBroadcasts 钉住"审批结束是广播"：
// 超时 → emit 帧 + 撤销那条 waterfall（cancel 帧）。
func TestOfficialWireEventsApprovalTimeoutBroadcasts(t *testing.T) {
	w := newApprovalWire(t)
	w.broker.timeout = 120 * time.Millisecond
	ws, _ := w.openEventsStream(t)

	w.requestApproval(t, "perm-3", "sess-3", "bash")
	eventID := requiredWaterfall(t, frameValueOfWire(t, ws.readJSON(t, 3*time.Second)), "perm-3", "sess-3")

	emit := frameValueOfWire(t, ws.readJSON(t, 3*time.Second))
	if emit["type"] != "emit" || emit["event"] != approvalTimeoutEvent {
		t.Fatalf("超时应发 emit 帧：%v", emit)
	}
	if len(emit) != 3 {
		t.Fatalf("emit 必须恰好三个键：%v", emit)
	}
	args, ok := emit["args"].([]any)
	if !ok || len(args) != 1 {
		t.Fatalf("emit.args 必须是单元素数组：%v", emit)
	}
	if payload, _ := args[0].(map[string]any); payload["requestId"] != "perm-3" {
		t.Fatalf("emit.args[0] 应是审批负载：%v", emit)
	}

	cancel := frameValueOfWire(t, ws.readJSON(t, 3*time.Second))
	if cancel["type"] != "cancel" || cancel["eventId"] != eventID || len(cancel) != 2 {
		t.Fatalf("应撤销那条 waterfall：%v", cancel)
	}
}

// TestOfficialWireEventsInvalidOutcomeKeepsGeneration 钉住"非法 outcome fail-loud，
// 但 generation 不崩"：同一个 socket 上后续事件照常走完。
func TestOfficialWireEventsInvalidOutcomeKeepsGeneration(t *testing.T) {
	w := newApprovalWire(t)
	ws, clientID := w.openEventsStream(t)

	w.requestApproval(t, "perm-4", "sess-4", "bash")
	eventID := requiredWaterfall(t, frameValueOfWire(t, ws.readJSON(t, 3*time.Second)), "perm-4", "sess-4")

	// ① fail-loud：result 不带 value（官方形状非法）
	bad := w.answer(t, "r-4", clientID, eventID, `{"kind":"result"}`)
	failure, hasError := bad["error"].(map[string]any)
	if !hasError {
		t.Fatalf("非法 outcome 必须 ok:false：%v", bad)
	}
	if failure["code"] != "gateway/arguments-invalid" {
		t.Fatalf("应回 gateway/arguments-invalid，实际 %v", failure)
	}
	if _, has := failure["details"].(map[string]any); !has {
		t.Fatalf("error.details 必须是对象（官方 zod 要求）：%v", failure)
	}

	// ② 未知 eventId 是静默 ok（官方 receiveRemoteEventResult 早返回）
	unknown := w.answer(t, "r-5", clientID, "no-such-event", `{"kind":"next"}`)
	if ok, _ := unknown["ok"].(bool); !ok {
		t.Fatalf("未知 eventId 应静默 ok:true：%v", unknown)
	}

	// ③ generation 没崩：同一个 socket 上再走一条完整回路
	w.requestApproval(t, "perm-5", "sess-5", "bash")
	frame := ws.readJSON(t, 3*time.Second)
	nextEventID := requiredWaterfall(t, frameValueOfWire(t, frame), "perm-5", "sess-5")
	result := w.answer(t, "r-6", clientID, nextEventID, `{"kind":"result","value":{"optionId":"reject_once"}}`)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("后续事件应正常结算：%v", result)
	}
	line := w.stdin.wait(t, approvalResponseTag)
	if !strings.Contains(line, `"requestId":"perm-5"`) || !strings.Contains(line, `"allow":false`) {
		t.Fatalf("reject_once 应写回 allow:false：%q", line)
	}
}

// TestOfficialWireEventsWithoutSource 钉住"没有事件源时只有 ready，result 明确未接线"。
//
// 这一条就是生产 `serve()` 的现状（officialWireDeps.Events 为 nil）：
// `$events` 可用、可以连上、绝不伪造 emit。
func TestOfficialWireEventsWithoutSource(t *testing.T) {
	h := newOfficialWireHarness(t)
	ws, response := dialWireMux(t, h.mounted.URL, h.officialCookies(t))
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("mux 升级应 101，实际 %d", response.StatusCode)
	}
	defer ws.Close()

	ws.writeText(t, `{"type":"open","streamId":"ev","endpoint":"$events","payload":{"args":{}}}`)
	ready := frameValueOfWire(t, ws.readJSON(t, 3*time.Second))
	if ready["type"] != "ready" || ready["clientId"] == "" {
		t.Fatalf("首帧必须是带 clientId 的 ready：%v", ready)
	}

	// 没有事件源 → 之后**什么都不许发**。
	if op, payload, err := readRawFrame(ws, 500*time.Millisecond); err == nil {
		t.Fatalf("没有事件源时不该再下发任何帧（op=%d 原文=%s）", op, payload)
	}

	args := `{"clientId":"c","eventId":"e","outcome":{"kind":"next"}}`
	status, payload := h.post(t, "$events/result", wireEnvelope("r-none", "$events/result", args))
	result := wireResult(t, status, payload)
	failure, _ := result["error"].(map[string]any)
	if failure["code"] != "gateway/method-unavailable" {
		t.Fatalf("没有事件源应回 gateway/method-unavailable，实际 %v", failure)
	}
}

// TestOfficialWireEventsResultRequiresAnswerPeer 钉住 interactions=answer 准入
// （EVIDENCE §3.2；官方 requireInteractions）。
func TestOfficialWireEventsResultRequiresAnswerPeer(t *testing.T) {
	// 准入判定在 Handle 之前（server.go:377），所以这一条不需要事件源。
	passive := buildOfficialWireHarness(t, officialwire.InteractionsNone)

	args := `{"clientId":"c","eventId":"e","outcome":{"kind":"next"}}`
	status, payload := passive.post(t, "$events/result", wireEnvelope("r-passive", "$events/result", args))
	result := wireResult(t, status, payload)
	failure, _ := result["error"].(map[string]any)
	if failure["code"] != "gateway/interactions-forbidden" {
		t.Fatalf("非 answer Peer 应回 gateway/interactions-forbidden，实际 %v", failure)
	}
}
