package officialwire

// `$events` 应用层的验收：判别式、exact-key、无损 JSON、以及"帧真的从 mux 走一条"。
//
// 官方出处（dsh-api-gateway@0.2.2-rc.1）：
//
//	parseRemoteEventFrame     lib/types/client/remote-events.js:198-226
//	parseRemoteEventReady     lib/types/client/remote-events.js:181-193
//	assertRemoteEventFrame    lib/index.js:1477-1480
//	parseRemoteEventResult    lib/types/stream-protocol.js:26-53
//	parseRemoteEventRejection lib/types/stream-protocol.js:229-245
//
// 这里每一条都对应官方的一个 `return false` / `throw`：少一条，"宿主发出去了、
// 官方客户端判非法帧"就会变成"用户的连接断了并重连"。

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------------ 协议层

func TestEventPayloadExactKeys(t *testing.T) {
	cases := []struct {
		name  string
		event Event
		want  []string
	}{
		{
			name:  "emit 三键",
			event: Event{Type: EventEmit, Event: "turn/done", Args: json.RawMessage(`[{"turnId":"t1"}]`)},
			want:  []string{"type", "event", "args"},
		},
		{
			name: "waterfall 五键",
			event: Event{Type: EventWaterfall, Event: "approval/request", EventID: "e1",
				AgentID: "s1", Request: json.RawMessage(`{"requestId":"r1"}`)},
			want: []string{"type", "event", "eventId", "agentId", "request"},
		},
		{
			name:  "cancel 两键",
			event: Event{Type: EventCancel, EventID: "e1"},
			want:  []string{"type", "eventId"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, failure := tc.event.Payload()
			if failure != nil {
				t.Fatalf("Payload: %v", failure)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(payload, &fields); err != nil {
				t.Fatalf("payload 不是对象：%v 原文=%s", err, payload)
			}
			if len(fields) != len(tc.want) {
				t.Fatalf("键数应为 %d，实际 %d：%s", len(tc.want), len(fields), payload)
			}
			for _, key := range tc.want {
				if _, ok := fields[key]; !ok {
					t.Fatalf("缺键 %s：%s", key, payload)
				}
			}
		})
	}
}

func TestEventValidateRejectsOfficialViolations(t *testing.T) {
	cases := []struct {
		name  string
		event Event
	}{
		{"emit 缺 event 名", Event{Type: EventEmit, Args: json.RawMessage(`[]`)}},
		{"emit 的 args 不是数组", Event{Type: EventEmit, Event: "x", Args: json.RawMessage(`{}`)}},
		{"emit 的 args 带 -0", Event{Type: EventEmit, Event: "x", Args: json.RawMessage(`[-0]`)}},
		{"emit 的 args 带 1e999", Event{Type: EventEmit, Event: "x", Args: json.RawMessage(`[1e999]`)}},
		{"waterfall 缺 eventId", Event{Type: EventWaterfall, Event: "x", AgentID: "a", Request: json.RawMessage(`{}`)}},
		{"waterfall 缺 agentId", Event{Type: EventWaterfall, Event: "x", EventID: "e", Request: json.RawMessage(`{}`)}},
		{"waterfall 的 request 带 agent", Event{Type: EventWaterfall, Event: "x", EventID: "e", AgentID: "a",
			Request: json.RawMessage(`{"agent":1}`)}},
		{"waterfall 的 request 带 signal", Event{Type: EventWaterfall, Event: "x", EventID: "e", AgentID: "a",
			Request: json.RawMessage(`{"signal":1}`)}},
		{"waterfall 的 request 不是对象", Event{Type: EventWaterfall, Event: "x", EventID: "e", AgentID: "a",
			Request: json.RawMessage(`[]`)}},
		{"cancel 缺 eventId", Event{Type: EventCancel}},
		{"不认识的类型", Event{Type: "ready"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if failure := tc.event.Validate(); failure == nil {
				t.Fatalf("应拒绝：%+v", tc.event)
			}
			if _, failure := tc.event.Payload(); failure == nil {
				t.Fatalf("Payload 也应拒绝：%+v", tc.event)
			}
		})
	}
}

func TestParseEventResultAcceptsOfficialShapes(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string // outcome.kind
	}{
		{"next", `{"clientId":"c","eventId":"e","outcome":{"kind":"next"}}`, OutcomeNext},
		{"result 无 value", `{"clientId":"c","eventId":"e","outcome":{"kind":"result"}}`, OutcomeResult},
		{"result 带 value", `{"clientId":"c","eventId":"e","outcome":{"kind":"result","value":{"optionId":"allow_once"}}}`, OutcomeResult},
		{"rejected", `{"clientId":"c","eventId":"e","outcome":{"kind":"rejected","error":{"name":"Error","message":"boom"}}}`, OutcomeRejected},
		{"rejected 带 code/details", `{"clientId":"c","eventId":"e","outcome":{"kind":"rejected","error":{"name":"E","message":"m","code":"x/y","details":{"a":1}}}}`, OutcomeRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := decodeArgs(t, tc.args)
			result, failure := ParseEventResult(EndpointEventResult, args)
			if failure != nil {
				t.Fatalf("应接受：%v", failure)
			}
			if result.Outcome.Kind != tc.want {
				t.Fatalf("kind 应为 %s，实际 %s", tc.want, result.Outcome.Kind)
			}
			if result.ClientID != "c" || result.EventID != "e" {
				t.Fatalf("clientId/eventId 应逐字透传：%+v", result)
			}
		})
	}
}

func TestParseEventResultRejectsOfficialViolations(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"缺 clientId", `{"eventId":"e","outcome":{"kind":"next"}}`},
		{"clientId 空串", `{"clientId":"","eventId":"e","outcome":{"kind":"next"}}`},
		{"eventId 空串", `{"clientId":"c","eventId":"","outcome":{"kind":"next"}}`},
		{"outcome 不是对象", `{"clientId":"c","eventId":"e","outcome":"next"}`},
		{"不认识的 kind", `{"clientId":"c","eventId":"e","outcome":{"kind":"maybe"}}`},
		{"next 多一个字段", `{"clientId":"c","eventId":"e","outcome":{"kind":"next","value":1}}`},
		{"result 多一个字段", `{"clientId":"c","eventId":"e","outcome":{"kind":"result","extra":1}}`},
		{"result 的 value 不是无损 JSON", `{"clientId":"c","eventId":"e","outcome":{"kind":"result","value":-0}}`},
		{"rejected 缺 error", `{"clientId":"c","eventId":"e","outcome":{"kind":"rejected"}}`},
		{"rejected 的 error 缺 message", `{"clientId":"c","eventId":"e","outcome":{"kind":"rejected","error":{"name":"E"}}}`},
		{"rejected 的 error.name 空串", `{"clientId":"c","eventId":"e","outcome":{"kind":"rejected","error":{"name":"","message":"m"}}}`},
		{"rejected 的 error 多一个字段", `{"clientId":"c","eventId":"e","outcome":{"kind":"rejected","error":{"name":"E","message":"m","stack":"x"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, failure := ParseEventResult(EndpointEventResult, decodeArgs(t, tc.args)); failure == nil {
				t.Fatalf("应拒绝：%s", tc.args)
			}
		})
	}
}

// 顶层字段集由 Descriptor 校（server.go 的 ValidateArgs）；这里只钉住它确实声明了三个。
func TestEventsResultDescriptorMatchesOfficial(t *testing.T) {
	descriptor := &Descriptor{Required: []string{"clientId", "eventId", "outcome"}}
	args := decodeArgs(t, `{"clientId":"c","eventId":"e","outcome":{"kind":"next"},"extra":1}`)
	failure := ValidateArgs(descriptor, EndpointEventResult, args)
	if failure == nil || failure.Code != CodeArgumentsInvalid {
		t.Fatalf("多一个字段应判 gateway/arguments-invalid，实际 %v", failure)
	}
}

func TestIsLosslessJSON(t *testing.T) {
	lossless := []string{`null`, `"s"`, `true`, `false`, `0`, `1`, `-1`, `1.5`, `[]`, `{}`,
		`[1,"a",null,{"k":[true]}]`, `{"a":{"b":[]}}`}
	for _, raw := range lossless {
		if !isLosslessJSON(json.RawMessage(raw)) {
			t.Fatalf("%s 应判无损", raw)
		}
	}
	lossy := []string{``, ` `, `-0`, `1e999`, `-1e999`, `NaN`, `{"a":1} {"b":2}`, `1 2`, `[1,]`, `{`}
	for _, raw := range lossy {
		if isLosslessJSON(json.RawMessage(raw)) {
			t.Fatalf("%s 应判有损", raw)
		}
	}
}

// ------------------------------------------------------------ 走真 mux 的链路

// fakeEventSource 是协议层测试用的事件源。
type fakeEventSource struct {
	mu       sync.Mutex
	subs     map[string]chan Event
	answers  []recordedAnswer
	answerFn func(clientID, eventID string, outcome Outcome) *Error
	stopped  int
}

type recordedAnswer struct {
	clientID string
	eventID  string
	outcome  Outcome
}

func newFakeEventSource() *fakeEventSource {
	return &fakeEventSource{subs: map[string]chan Event{}}
}

func (f *fakeEventSource) Subscribe(_ context.Context, clientID string) (<-chan Event, func()) {
	events := make(chan Event, 16)
	f.mu.Lock()
	f.subs[clientID] = events
	f.mu.Unlock()
	var once sync.Once
	return events, func() {
		once.Do(func() {
			f.mu.Lock()
			if current, ok := f.subs[clientID]; ok && current == events {
				delete(f.subs, clientID)
				close(events)
				f.stopped++
			}
			f.mu.Unlock()
		})
	}
}

// push 在锁内非阻塞投递：锁内投递 + 锁内 close 才不会出现"往已关闭通道写"。
func (f *fakeEventSource) push(event Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, events := range f.subs {
		select {
		case events <- event:
		default:
		}
	}
}

func (f *fakeEventSource) Answer(clientID, eventID string, outcome Outcome) *Error {
	f.mu.Lock()
	f.answers = append(f.answers, recordedAnswer{clientID: clientID, eventID: eventID, outcome: outcome})
	answerFn := f.answerFn
	f.mu.Unlock()
	if answerFn != nil {
		return answerFn(clientID, eventID, outcome)
	}
	return nil
}

func (f *fakeEventSource) recorded() []recordedAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedAnswer(nil), f.answers...)
}

// eventsResultBinding 是 `$events/result` 的协议层接线（生产上是 serve_official_wire.go 的那段）。
func eventsResultBinding(source EventSource) Binding {
	return Binding{
		Descriptor:    &Descriptor{Required: []string{"clientId", "eventId", "outcome"}},
		RequireAnswer: true,
		Handle: func(_ context.Context, args map[string]json.RawMessage) (any, *Error) {
			result, failure := ParseEventResult(EndpointEventResult, args)
			if failure != nil {
				return nil, failure
			}
			if failure := source.Answer(result.ClientID, result.EventID, result.Outcome); failure != nil {
				return nil, failure
			}
			return nil, nil
		},
	}
}

func eventsConfig(source EventSource) ServerConfig {
	cfg := testServerConfig()
	cfg.Events = source
	return cfg
}

// TestMuxEventsForwardsWaterfall 钉住"事件源的事件真的从 mux 出来，且是官方形状"。
func TestMuxEventsForwardsWaterfall(t *testing.T) {
	source := newFakeEventSource()
	_, ts, client := startWire(t, eventsConfig(source), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsAnswer)

	ws.writeText(t, openFrame("ev", EndpointEvents, ""))
	ready := frameValueOf(t, ws.readJSONFrame(t, 2*time.Second))
	clientID, _ := ready["clientId"].(string)
	if clientID == "" {
		t.Fatalf("ready 必须带 clientId：%v", ready)
	}

	source.push(Event{Type: EventWaterfall, Event: "approval/request", EventID: "e1",
		AgentID: "s1", Request: json.RawMessage(`{"requestId":"r1","tool":"bash"}`)})

	frame := ws.readJSONFrame(t, 2*time.Second)
	if frame["type"] != FrameItem || frame["streamId"] != "ev" {
		t.Fatalf("应是 item：%v", frame)
	}
	value := frameValueOf(t, frame)
	if value["type"] != EventWaterfall || value["event"] != "approval/request" ||
		value["eventId"] != "e1" || value["agentId"] != "s1" {
		t.Fatalf("waterfall 载荷不对：%v", value)
	}
	request, _ := value["request"].(map[string]any)
	if request["requestId"] != "r1" {
		t.Fatalf("request 应逐字透传：%v", value)
	}
	if len(value) != 5 {
		t.Fatalf("waterfall 必须恰好五个键（官方 exact-key）：%v", value)
	}

	// emit 也要能走同一条流。
	source.push(Event{Type: EventEmit, Event: "approval/timeout", Args: json.RawMessage(`[{"requestId":"r1"}]`)})
	emit := frameValueOf(t, ws.readJSONFrame(t, 2*time.Second))
	if emit["type"] != EventEmit || emit["event"] != "approval/timeout" {
		t.Fatalf("emit 载荷不对：%v", emit)
	}
	if len(emit) != 3 {
		t.Fatalf("emit 必须恰好三个键：%v", emit)
	}
}

// TestMuxEventsResultDeliversOutcome 钉住"unary → 事件源"的关联（clientId/eventId 逐字）。
func TestMuxEventsResultDeliversOutcome(t *testing.T) {
	source := newFakeEventSource()
	_, ts, client := startWire(t, eventsConfig(source), map[string]Binding{
		EndpointEventResult: eventsResultBinding(source),
	})
	connectAndCookies(t, ts, client, InteractionsAnswer)

	status, payload := postEnvelope(t, ts, client, EndpointEventResult,
		envelopeBody("r-1", EndpointEventResult,
			`{"clientId":"c1","eventId":"e1","outcome":{"kind":"result","value":{"optionId":"allow_once"}}}`))
	result := resultOf(t, decodeEnvelope(t, status, payload))
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("应 ok:true：%v", result)
	}
	if _, has := result["value"]; has {
		t.Fatalf("官方 $events/result 成功时 value 为 void 0（信封里不出现）：%v", result)
	}

	recorded := source.recorded()
	if len(recorded) != 1 {
		t.Fatalf("事件源应收到一次 outcome，实际 %d", len(recorded))
	}
	if recorded[0].clientID != "c1" || recorded[0].eventID != "e1" {
		t.Fatalf("关联 id 应逐字透传：%+v", recorded[0])
	}
	if recorded[0].outcome.Kind != OutcomeResult {
		t.Fatalf("outcome.kind 应为 result：%+v", recorded[0].outcome)
	}
	var value map[string]string
	if err := json.Unmarshal(recorded[0].outcome.Value, &value); err != nil {
		t.Fatalf("outcome.value 应原样交给事件源：%v", err)
	}
	if value["optionId"] != "allow_once" {
		t.Fatalf("outcome.value 不对：%v", value)
	}
}

// TestMuxEventsResultInvalidOutcomeIsFailLoud 钉住"非法 outcome 不静默成功"。
func TestMuxEventsResultInvalidOutcomeIsFailLoud(t *testing.T) {
	source := newFakeEventSource()
	_, ts, client := startWire(t, eventsConfig(source), map[string]Binding{
		EndpointEventResult: eventsResultBinding(source),
	})
	connectAndCookies(t, ts, client, InteractionsAnswer)

	status, payload := postEnvelope(t, ts, client, EndpointEventResult,
		envelopeBody("r-bad", EndpointEventResult,
			`{"clientId":"c1","eventId":"e1","outcome":{"kind":"maybe"}}`))
	failure := failureOf(t, decodeEnvelope(t, status, payload))
	if failure["code"] != CodeArgumentsInvalid {
		t.Fatalf("非法 outcome 应回 %s，实际 %v", CodeArgumentsInvalid, failure)
	}
	if len(source.recorded()) != 0 {
		t.Fatalf("非法 outcome 不许交给事件源：%v", source.recorded())
	}
}

// TestMuxEventsNoSourceOnlyReady 钉住"没有事件源时只发 ready，不伪造 emit"。
func TestMuxEventsNoSourceOnlyReady(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsAnswer)

	ws.writeText(t, openFrame("ev", EndpointEvents, ""))
	ready := frameValueOf(t, ws.readJSONFrame(t, 2*time.Second))
	if ready["type"] != EventReady {
		t.Fatalf("首帧必须是 ready：%v", ready)
	}

	// 之后**什么都不许发**：伪造一条 emit 会让客户端去等一个不存在的应答。
	_ = ws.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	op, payload, err := ws.readFrame(t, 0)
	_ = ws.conn.SetReadDeadline(time.Time{})
	if err == nil {
		t.Fatalf("没有事件源时不该再下发任何帧（收到 op=%d 原文=%s）", op, payload)
	}

	// 流仍然可用：cancel 之后还能开别的流。
	ws.writeText(t, `{"type":"cancel","streamId":"ev"}`)
	ws.writeText(t, openFrame("ev2", EndpointEvents, ""))
	if value := frameValueOf(t, ws.readJSONFrame(t, 2*time.Second)); value["type"] != EventReady {
		t.Fatalf("cancel 之后新流应重新 ready：%v", value)
	}
}

// TestMuxEventsSourceCloseSendsEnd 钉住"事件源结束 → end 帧"。
func TestMuxEventsSourceCloseSendsEnd(t *testing.T) {
	source := newFakeEventSource()
	_, ts, client := startWire(t, eventsConfig(source), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsAnswer)

	ws.writeText(t, openFrame("ev", EndpointEvents, ""))
	ready := frameValueOf(t, ws.readJSONFrame(t, 2*time.Second))
	clientID, _ := ready["clientId"].(string)

	source.push(Event{Type: EventCancel, EventID: "gone"})
	cancel := frameValueOf(t, ws.readJSONFrame(t, 2*time.Second))
	if cancel["type"] != EventCancel || cancel["eventId"] != "gone" || len(cancel) != 2 {
		t.Fatalf("cancel 载荷不对：%v", cancel)
	}

	source.mu.Lock()
	events := source.subs[clientID]
	source.mu.Unlock()
	if events == nil {
		t.Fatalf("事件源没有这条订阅：%q", clientID)
	}
	// 直接关掉事件源 → 宿主必须发 end。
	source.mu.Lock()
	close(events)
	delete(source.subs, clientID)
	source.mu.Unlock()

	frame := ws.readJSONFrame(t, 2*time.Second)
	if frame["type"] != FrameEnd || frame["streamId"] != "ev" {
		t.Fatalf("事件源结束应发 end 帧：%v", frame)
	}
}

// TestMuxEventsAnswerUnknownClientIsForbidden 钉住"不是你的事件不能答"。
func TestMuxEventsAnswerUnknownClientIsForbidden(t *testing.T) {
	source := newFakeEventSource()
	source.answerFn = func(clientID, eventID string, _ Outcome) *Error {
		return &Error{Code: CodeInteractionsForbidden, Message: "不属于这个 Peer",
			Details: map[string]any{"clientId": clientID, "eventId": eventID}}
	}
	_, ts, client := startWire(t, eventsConfig(source), map[string]Binding{
		EndpointEventResult: eventsResultBinding(source),
	})
	connectAndCookies(t, ts, client, InteractionsAnswer)

	status, payload := postEnvelope(t, ts, client, EndpointEventResult,
		envelopeBody("r-x", EndpointEventResult,
			`{"clientId":"other","eventId":"e1","outcome":{"kind":"next"}}`))
	failure := failureOf(t, decodeEnvelope(t, status, payload))
	if failure["code"] != CodeInteractionsForbidden {
		t.Fatalf("应回 %s，实际 %v", CodeInteractionsForbidden, failure)
	}
}

// TestMuxEventsPerOpenClientID 钉住"每次 open 一个新 clientId"（官方 lib/index.js:1033）。
func TestMuxEventsPerOpenClientID(t *testing.T) {
	source := newFakeEventSource()
	_, ts, client := startWire(t, eventsConfig(source), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsAnswer)

	ws.writeText(t, openFrame("a", EndpointEvents, ""))
	first := frameValueOf(t, ws.readJSONFrame(t, 2*time.Second))
	ws.writeText(t, openFrame("b", EndpointEvents, ""))
	second := frameValueOf(t, ws.readJSONFrame(t, 2*time.Second))

	if first["clientId"] == second["clientId"] {
		t.Fatalf("两条 $events 流必须各有 clientId，实际都是 %v", first["clientId"])
	}
}

// decodeArgs 把 args JSON 解成 map（协议层入口的形状）。
func decodeArgs(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("args 不是对象：%v 原文=%s", err, raw)
	}
	return args
}

// 编译期：http 包在本文件里只用于 dialAuthorized 的签名推断。
var _ = http.StatusOK
