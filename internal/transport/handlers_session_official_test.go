package transport

// 官方 session.* 方法的契约测试。
//
// 每条测试都断言**官方 zod 契约里的字段名与形状**（见
// @deepseek-ai/dsh-api-session-controller/lib/typert.host.js 的 jsonSchema），
// 而不是"随便有个字段就行"。
//
// 为什么直接调 handler 而不是 POST /api/<method>：这 15 个方法的分派 case 在
// server.go 里，本轮写作用域不允许改它（由 Lead 统一接线）。handler 就是 case
// 会调的那一个函数，所以这里测的是同一份实现；`/api` 端到端由 Lead 接线后覆盖。

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// officialHandler 是"这个 handler 的签名"，让测试能按表驱动。
type officialHandler func(http.ResponseWriter, *http.Request)

// postOfficial 直接把请求打到 handler 上（绕过还没接线的 switch 分派）。
func postOfficial(t *testing.T, handler officialHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/official", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// fakeOfficialSource 是实现了 OfficialSessionHost 的假 Source。
type fakeOfficialSource struct {
	events     []DurableEvent
	logEnd     int64
	durErr     error
	live       OfficialLiveState
	liveErr    error
	searchFn   func(query string, limit int) ([]OfficialSearchItem, bool, error)
	removeErr  error
	removedIDs []string
}

func (f *fakeOfficialSource) Messages(string) ([]Message, error) { return nil, nil }

func (f *fakeOfficialSource) DurableEvents(string) ([]DurableEvent, int64, error) {
	if f.durErr != nil {
		return nil, 0, f.durErr
	}
	return f.events, f.logEnd, nil
}

func (f *fakeOfficialSource) LiveState(string) (OfficialLiveState, error) {
	return f.live, f.liveErr
}

func (f *fakeOfficialSource) Search(query string, limit int) ([]OfficialSearchItem, bool, error) {
	if f.searchFn != nil {
		return f.searchFn(query, limit)
	}
	return nil, false, nil
}

func (f *fakeOfficialSource) RemoveQueueItem(_ string, itemID string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removedIDs = append(f.removedIDs, itemID)
	return nil
}

// officialFixtureEvents 造一份形状真实的 v4 事件序列（seq 从 0 连续）。
//
//	turn/start(turn=1) → user → assistant → tool/result → turn/end → user → assistant
func officialFixtureEvents() []DurableEvent {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	return []DurableEvent{
		{Type: "turn/start", Seq: 0, Time: 1000, Data: raw(`{"turn":1}`)},
		{Type: "user/message", Seq: 1, Time: 1001, Data: raw(`{"content":[{"type":"text","text":"hello-one"}],"source":{"kind":"user"}}`)},
		{Type: "assistant/message", Seq: 2, Time: 1002, Data: raw(`{"content":[{"type":"text","text":"answer-one"}]}`)},
		{Type: "tool/result", Seq: 3, Time: 1003, Data: raw(`{"callId":"c1","text":"tool-out"}`)},
		{Type: "turn/end", Seq: 4, Time: 1004, Data: raw(`{"turn":1}`)},
		{Type: "user/message", Seq: 5, Time: 1005, Data: raw(`{"content":[{"type":"text","text":"hello-two"}],"source":{"kind":"user"}}`)},
		{Type: "assistant/message", Seq: 6, Time: 1006, Data: raw(`{"content":[{"type":"text","text":"answer-two"}]}`)},
	}
}

func officialServerWith(t *testing.T, src *fakeOfficialSource, extra Options) *Server {
	t.Helper()
	extra.Source = src
	return newTestServer(t, extra)
}

// 官方 session.page 结果只有 cursor/records/hasMore（typert.host.js:2345）。
func TestOfficialSessionPageShapeAndOldestFirst(t *testing.T) {
	src := &fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}
	s := officialServerWith(t, src, Options{})

	rec := postOfficial(t, s.handleSessionPage, `{"address":{"kind":"session","sessionId":"s1"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["cursor"] != float64(6) {
		t.Fatalf("cursor 必须是日志末端 v4 seq=6，实际 %v", got["cursor"])
	}
	if got["hasMore"] != false {
		t.Fatalf("整段都在一页里，hasMore 必须 false：%v", got["hasMore"])
	}
	records, ok := got["records"].([]any)
	if !ok || len(records) != 7 {
		t.Fatalf("records 必须是 7 条事件：%v", got["records"])
	}
	for i, raw := range records {
		rec := raw.(map[string]any)
		if rec["type"] != "event" {
			t.Fatalf("records[%d].type=%v，官方只认 event", i, rec["type"])
		}
		event := rec["event"].(map[string]any)
		if event["seq"] != float64(i) {
			t.Fatalf("records 必须 oldest-first 且 seq 连续：records[%d].seq=%v", i, event["seq"])
		}
		for _, key := range []string{"type", "seq", "time", "data"} {
			if _, has := event[key]; !has {
				t.Fatalf("records[%d].event 缺官方必填键 %q：%v", i, key, event)
			}
		}
	}
	// 官方 page 结果**没有** projections 键（任务书那句与 schema 不符，以 schema 为准）。
	if _, has := got["projections"]; has {
		t.Fatalf("page 结果不该有 projections：%v", got)
	}
}

func TestOfficialSessionPageBackwardCursorAndCut(t *testing.T) {
	src := &fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}
	s := officialServerWith(t, src, Options{})

	// 只取最近 1 条消息：分页要把 cut 落在那一组的起点之后，页是 tail-aligned 的。
	rec := postOfficial(t, s.handleSessionPage,
		`{"address":{"kind":"session","sessionId":"s1"},"maxMessages":1,"throughSeq":6}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	records := got["records"].([]any)
	if len(records) == 0 {
		t.Fatal("maxMessages=1 至少要有一条记录")
	}
	first := records[0].(map[string]any)["event"].(map[string]any)
	if first["seq"] != float64(6) {
		t.Fatalf("maxMessages=1 的页必须从 seq=6 起（assistant-two）：%v", first["seq"])
	}
	if got["cursor"] != float64(6) {
		t.Fatalf("cursor 是这次读的上界（throughSeq），不是页首 seq：%v", got["cursor"])
	}
	if got["hasMore"] != true {
		t.Fatalf("还有更旧的事件，hasMore 必须 true：%v", got["hasMore"])
	}

	// 用页首 seq 当 beforeSeq 取更旧的一页：闭区间上界仍是 cursor=6，页内含不出 seq=6。
	rec = postOfficial(t, s.handleSessionPage,
		`{"address":{"kind":"session","sessionId":"s1"},"beforeSeq":6,"maxMessages":2,"throughSeq":6}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	got = unwrapResult(t, rec.Body.Bytes())
	for _, raw := range got["records"].([]any) {
		seq := raw.(map[string]any)["event"].(map[string]any)["seq"].(float64)
		if seq >= 6 {
			t.Fatalf("beforeSeq=6 的页不许含 seq>=6 的事件：%v", seq)
		}
	}
}

func TestOfficialSessionPageThroughSeqMinusOneIsEmptyPage(t *testing.T) {
	src := &fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}
	s := officialServerWith(t, src, Options{})
	rec := postOfficial(t, s.handleSessionPage, `{"address":{"kind":"session","sessionId":"s1"},"throughSeq":-1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["cursor"] != float64(-1) {
		t.Fatalf("throughSeq=-1 时 cursor 必须是 -1：%v", got["cursor"])
	}
	if records, ok := got["records"].([]any); !ok || len(records) != 0 {
		t.Fatalf("空页必须是真数组且为空：%v", got["records"])
	}
	if got["hasMore"] != false {
		t.Fatalf("空页 hasMore 必须 false：%v", got["hasMore"])
	}
}

func TestOfficialSessionPageTurnWindowStopsAtTurnStart(t *testing.T) {
	events := []DurableEvent{
		{Type: "turn/start", Seq: 0, Time: 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: "user/message", Seq: 1, Time: 2, Data: json.RawMessage(`{"source":{"kind":"user"}}`)},
		{Type: "assistant/message", Seq: 2, Time: 3, Data: json.RawMessage(`{}`)},
		{Type: "turn/start", Seq: 3, Time: 4, Data: json.RawMessage(`{"turn":2}`)},
		{Type: "user/message", Seq: 4, Time: 5, Data: json.RawMessage(`{"source":{"kind":"user"}}`)},
		{Type: "assistant/message", Seq: 5, Time: 6, Data: json.RawMessage(`{}`)},
	}
	src := &fakeOfficialSource{events: events, logEnd: 5}
	s := officialServerWith(t, src, Options{})
	// minMessages=1、minTurns=1：数到 1 条消息后跨到 turn/start 就停，cut 落在 seq=3。
	rec := postOfficial(t, s.handleSessionPage,
		`{"address":{"kind":"session","sessionId":"s1"},"maxMessages":10,"turnWindow":{"minMessages":1,"minTurns":1}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	records := got["records"].([]any)
	firstSeq := records[0].(map[string]any)["event"].(map[string]any)["seq"]
	if firstSeq != float64(3) {
		t.Fatalf("turnWindow 必须把 cut 落在 turn/start(seq=3)，实际页首 %v", firstSeq)
	}
}

func TestOfficialSessionPageFailsLoudOnBadParams(t *testing.T) {
	src := &fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}
	s := officialServerWith(t, src, Options{})
	cases := []struct {
		name string
		body string
		want int
	}{
		{"缺 address", `{}`, http.StatusBadRequest},
		{"address.kind 非法", `{"address":{"kind":"nope"}}`, http.StatusBadRequest},
		{"address.sessionId 为空", `{"address":{"kind":"session"}}`, http.StatusBadRequest},
		{"未知字段", `{"address":{"kind":"session","sessionId":"s1"},"nope":1}`, http.StatusBadRequest},
		{"maxMessages=0", `{"address":{"kind":"session","sessionId":"s1"},"maxMessages":0}`, http.StatusBadRequest},
		{"maxMessages 负数", `{"address":{"kind":"session","sessionId":"s1"},"maxMessages":-3}`, http.StatusBadRequest},
		{"beforeSeq<0", `{"address":{"kind":"session","sessionId":"s1"},"beforeSeq":-2}`, http.StatusBadRequest},
		{"throughSeq<-1", `{"address":{"kind":"session","sessionId":"s1"},"throughSeq":-2}`, http.StatusBadRequest},
		// throughSeq 超过日志末端：官方抛 bad-request（history cut exceeds cursor）。
		{"throughSeq 超末端", `{"address":{"kind":"session","sessionId":"s1"},"throughSeq":99}`, http.StatusBadRequest},
		{"turnWindow.minMessages>max", `{"address":{"kind":"session","sessionId":"s1"},"maxMessages":2,"turnWindow":{"minMessages":5,"minTurns":1}}`, http.StatusBadRequest},
		{"turnWindow.minTurns=0", `{"address":{"kind":"session","sessionId":"s1"},"turnWindow":{"minMessages":1,"minTurns":0}}`, http.StatusBadRequest},
		{"多段 JSON", `{"address":{"kind":"session","sessionId":"s1"}}{}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := postOfficial(t, s.handleSessionPage, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s：status=%d want %d body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// subagent 地址要求证明 child 属于 parent；本宿主没有那套投影，必须明确 501。
func TestOfficialSessionPageSubagentAddressIsExplicitly501(t *testing.T) {
	src := &fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}
	s := officialServerWith(t, src, Options{})
	rec := postOfficial(t, s.handleSessionPage,
		`{"address":{"kind":"subagent","parentSessionId":"p","childSessionId":"c","mode":"unknown"}}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("subagent 地址必须 501，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if code := errorField(t, rec.Body.Bytes(), "code"); code != "not_implemented" {
		t.Fatalf("code=%q，官方能力缺失必须是 not_implemented", code)
	}
}

// session.events 与 session.page 的结果同形（官方两者都调 historyController.events）。
func TestOfficialSessionEventsMatchesPageShape(t *testing.T) {
	src := &fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}
	s := officialServerWith(t, src, Options{})
	page := unwrapResult(t, postOfficial(t, s.handleSessionPage, `{"address":{"kind":"session","sessionId":"s1"}}`).Body.Bytes())
	events := unwrapResult(t, postOfficial(t, s.handleSessionEvents, `{"address":{"kind":"session","sessionId":"s1"}}`).Body.Bytes())
	if page["cursor"] != events["cursor"] || page["hasMore"] != events["hasMore"] {
		t.Fatalf("page/events 的 cursor/hasMore 必须一致：%v vs %v", page, events)
	}
	if len(page["records"].([]any)) != len(events["records"].([]any)) {
		t.Fatalf("page/events 的 records 条数必须一致：%v vs %v", len(page["records"].([]any)), len(events["records"].([]any)))
	}
}

func TestOfficialSessionStateDerivesFromRealSources(t *testing.T) {
	src := &fakeOfficialSource{
		events: officialFixtureEvents(),
		logEnd: 6,
		live:   OfficialLiveState{Active: true, Running: true, QueueIDs: []string{"turn-9"}},
	}
	approval := ServerFrame{Type: "approval/request", Data: json.RawMessage(`{"requestId":"r1"}`)}
	s := officialServerWith(t, src, Options{
		PendingApprovals: func(string) []ServerFrame { return []ServerFrame{approval} },
	})
	rec := postOfficial(t, s.handleSessionState, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	for _, key := range []string{"cursor", "active", "running", "queue", "pendingInput"} {
		if _, has := got[key]; !has {
			t.Fatalf("state 缺官方必填键 %q：%v", key, got)
		}
	}
	if got["cursor"] != float64(6) {
		t.Fatalf("cursor 必须是真实 v4 末端 seq=6：%v", got["cursor"])
	}
	if got["active"] != true || got["running"] != true {
		t.Fatalf("active/running 必须来自真实 live turn：%v", got)
	}
	// queue 恒为 null（本宿主排队项不保留 content，见 handler 注释）。
	if got["queue"] != nil {
		t.Fatalf("queue 必须是 null（不编空 content）：%v", got["queue"])
	}
	pending := got["pendingInput"].([]any)
	if len(pending) != 1 || pending[0] != "approval" {
		t.Fatalf("pendingInput 必须来自真实挂起审批：%v", pending)
	}

	// 空会话：cursor=-1，且 pendingInput 必须是空数组而不是 null。
	empty := officialServerWith(t, &fakeOfficialSource{logEnd: -1}, Options{})
	got = unwrapResult(t, postOfficial(t, empty.handleSessionState, `{"sessionId":"s2"}`).Body.Bytes())
	if got["cursor"] != float64(-1) {
		t.Fatalf("空会话 cursor 必须 -1：%v", got["cursor"])
	}
	if list, ok := got["pendingInput"].([]any); !ok || len(list) != 0 {
		t.Fatalf("pendingInput 必须始终是数组：%v", got["pendingInput"])
	}
}

func TestOfficialSessionStateMissingSessionIs404(t *testing.T) {
	src := &fakeOfficialSource{durErr: ErrNoSuchSession}
	s := officialServerWith(t, src, Options{})
	rec := postOfficial(t, s.handleSessionState, `{"sessionId":"gone"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的会话必须 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if code := errorField(t, rec.Body.Bytes(), "code"); code != "no_such_session" {
		t.Fatalf("code=%q", code)
	}
}

// session.wait：不在跑就立刻 idle；在跑就到超时，且 state 必须是真实采样。
func TestOfficialSessionWaitIdleAndTimeout(t *testing.T) {
	idle := officialServerWith(t, &fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}, Options{})
	rec := postOfficial(t, idle.handleSessionWait, `{"sessionId":"s1","timeoutMs":2000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("wait status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["reason"] != "idle" {
		t.Fatalf("没有 live turn 必须 idle：%v", got["reason"])
	}
	state := got["state"].(map[string]any)
	if state["running"] != false {
		t.Fatalf("idle 时 running 必须 false：%v", state)
	}
	output := got["output"].(map[string]any)
	if _, ok := output["records"].([]any); !ok {
		t.Fatalf("output.records 必须是数组：%v", output)
	}
	if _, ok := output["truncated"].(bool); !ok {
		t.Fatalf("output.truncated 必须是 bool：%v", output)
	}

	running := officialServerWith(t, &fakeOfficialSource{
		events: officialFixtureEvents(), logEnd: 6,
		live: OfficialLiveState{Active: true, Running: true},
	}, Options{})
	start := time.Now()
	rec = postOfficial(t, running.handleSessionWait, `{"sessionId":"s1","timeoutMs":120}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("wait status=%d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("timeoutMs=120 必须真的等过 100ms，实际 %v（没有真等待）", elapsed)
	}
	got = unwrapResult(t, rec.Body.Bytes())
	if got["reason"] != "timeout" {
		t.Fatalf("一直在跑必须 timeout：%v", got["reason"])
	}
	if got["state"].(map[string]any)["running"] != true {
		t.Fatalf("超时也要带回真实采样到的 running=true：%v", got["state"])
	}
}

// session.wait 的 output 是**新到旧**（官方 assistantOutput 从 throughSeq 往下走）。
func TestOfficialSessionWaitOutputNewestFirstAndTruncated(t *testing.T) {
	events := []DurableEvent{
		{Type: "assistant/message", Seq: 0, Time: 1, Data: json.RawMessage(`{"n":0}`)},
		{Type: "assistant/message", Seq: 1, Time: 2, Data: json.RawMessage(`{"n":1}`)},
		{Type: "assistant/message", Seq: 2, Time: 3, Data: json.RawMessage(`{"n":2}`)},
	}
	s := officialServerWith(t, &fakeOfficialSource{events: events, logEnd: 2}, Options{})
	rec := postOfficial(t, s.handleSessionWait, `{"sessionId":"s1","maxMessages":2,"afterSeq":-1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("wait status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	output := got["output"].(map[string]any)
	records := output["records"].([]any)
	if len(records) != 2 {
		t.Fatalf("maxMessages=2 只允许 2 条：%v", records)
	}
	if records[0].(map[string]any)["event"].(map[string]any)["seq"] != float64(2) {
		t.Fatalf("output 必须新到旧：%v", records[0])
	}
	if output["truncated"] != true {
		t.Fatalf("还有更早的 assistant 记录，truncated 必须 true：%v", output)
	}

	// afterSeq 是排他下界。
	rec = postOfficial(t, s.handleSessionWait, `{"sessionId":"s1","maxMessages":10,"afterSeq":1}`)
	got = unwrapResult(t, rec.Body.Bytes())
	records = got["output"].(map[string]any)["records"].([]any)
	if len(records) != 1 || records[0].(map[string]any)["event"].(map[string]any)["seq"] != float64(2) {
		t.Fatalf("afterSeq=1 只该有 seq=2：%v", records)
	}
}

func TestOfficialSessionWaitRejectsBadTimeout(t *testing.T) {
	s := officialServerWith(t, &fakeOfficialSource{logEnd: 0}, Options{})
	for _, body := range []string{
		`{"sessionId":"s1","timeoutMs":0}`,
		`{"sessionId":"s1","timeoutMs":-5}`,
		`{"sessionId":"s1","maxMessages":0}`,
		`{"sessionId":"s1","afterSeq":-2}`,
		`{}`,
	} {
		if rec := postOfficial(t, s.handleSessionWait, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d want 400", body, rec.Code)
		}
	}
}

func TestOfficialSessionSearchLimitsAndEmptyResult(t *testing.T) {
	calls := 0
	src := &fakeOfficialSource{logEnd: 0, searchFn: func(query string, limit int) ([]OfficialSearchItem, bool, error) {
		calls++
		if limit != officialSearchResultLimit {
			t.Fatalf("搜索上限必须是官方的 %d，实际 %d", officialSearchResultLimit, limit)
		}
		return []OfficialSearchItem{{SessionID: "s1", Snippet: OfficialSnippet(strings.Repeat("a", 500), 0)}}, false, nil
	}}
	s := officialServerWith(t, src, Options{})

	rec := postOfficial(t, s.handleSessionSearch, `{"query":"  hello  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("search status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	items := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%v", items)
	}
	snippet := items[0].(map[string]any)["snippet"].(string)
	if len([]rune(snippet)) > officialSearchSnippetMaxCodePoints+1 {
		t.Fatalf("snippet 超过官方 %d code point 上限：%d", officialSearchSnippetMaxCodePoints, len([]rune(snippet)))
	}
	if got["hasMore"] != false {
		t.Fatalf("hasMore=%v", got["hasMore"])
	}

	// 搜不到是空数组，不是错误。
	empty := officialServerWith(t, &fakeOfficialSource{logEnd: 0}, Options{})
	rec = postOfficial(t, empty.handleSessionSearch, `{"query":"nothing"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("搜不到必须 200，实际 %d", rec.Code)
	}
	got = unwrapResult(t, rec.Body.Bytes())
	if list, ok := got["items"].([]any); !ok || len(list) != 0 {
		t.Fatalf("items 必须是空数组：%v", got["items"])
	}

	for _, body := range []string{`{"query":"   "}`, `{}`, `{"query":"` + strings.Repeat("x", officialSearchQueryMaxChars+1) + `"}`} {
		if rec := postOfficial(t, empty.handleSessionSearch, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d want 400", body[:minInt(len(body), 40)], rec.Code)
		}
	}
}

// officialTextStore 是一份**非图片**存储：官方只承载四种图片类型，
// 拿它证明非图片会被响亮拒掉（attachments_test.go 的 fakeAttachmentStore 恒为 png）。
type officialTextStore struct{}

func (officialTextStore) Put(string) (Attachment, error) { return Attachment{}, nil }

func (officialTextStore) Get(string) (AttachmentContent, error) {
	return AttachmentContent{
		Attachment: Attachment{ID: "att-text", ByteSize: 2, MIME: "text/plain"},
		Data:       []byte("hi"),
	}, nil
}

func TestOfficialSessionAttachmentReturnsStoreTruth(t *testing.T) {
	s := newTestServer(t, Options{Attachments: &fakeAttachmentStore{}})
	rec := postOfficial(t, s.handleSessionAttachment,
		`{"sessionId":"s1","attachment":{"attachmentId":"att-real","mediaType":"image/png","bytes":3,"width":3,"height":2}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("attachment status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	att := got["attachment"].(map[string]any)
	if att["attachmentId"] != "att-real" || att["mediaType"] != "image/png" ||
		att["bytes"] != float64(3) || att["width"] != float64(3) || att["height"] != float64(2) {
		t.Fatalf("attachment 必须回 store 真值：%v", att)
	}
	if got["data"] != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) {
		t.Fatalf("data 必须是真实字节的 base64：%v", got["data"])
	}
	// attachmentId 是 opaque 存储 id，绝不能带路径。
	id := att["attachmentId"].(string)
	if strings.ContainsAny(id, `/:\`) {
		t.Fatalf("attachmentId 不许是路径：%v", id)
	}
}

func TestOfficialSessionAttachmentRejectsMismatchedReference(t *testing.T) {
	s := newTestServer(t, Options{Attachments: &fakeAttachmentStore{}})
	for _, body := range []string{
		`{"sessionId":"s1","attachment":{"attachmentId":"att-real","bytes":99}}`,
		`{"sessionId":"s1","attachment":{"attachmentId":"att-real","width":9}}`,
		`{"sessionId":"s1","attachment":{"attachmentId":"att-real","height":9}}`,
		`{"sessionId":"s1","attachment":{"attachmentId":"att-real","mediaType":"image/webp"}}`,
		`{"sessionId":"s1","attachment":{"attachmentId":""}}`,
		`{"attachment":{"attachmentId":"att-real"}}`,
	} {
		if rec := postOfficial(t, s.handleSessionAttachment, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d want 400（引用与真值不一致必须响亮失败）", body, rec.Code)
		}
	}
	// 非图片存储：官方只承载四种图片类型。
	text := newTestServer(t, Options{Attachments: officialTextStore{}})
	if rec := postOfficial(t, text.handleSessionAttachment,
		`{"sessionId":"s1","attachment":{"attachmentId":"att-text"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非图片必须 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 存储里没有的 id 是 404（换 id，不是重试）。
	if rec := postOfficial(t, s.handleSessionAttachment,
		`{"sessionId":"s1","attachment":{"attachmentId":"missing"}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的 attachment 必须 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOfficialSessionSelectModelPersistsOverrides(t *testing.T) {
	var gotModel, gotEffort string
	s := newTestServer(t, Options{
		SetSessionModel:  func(sessionID, model string) error { gotModel = sessionID + "/" + model; return nil },
		SetSessionEffort: func(sessionID, effort string) error { gotEffort = sessionID + "/" + effort; return nil },
	})
	rec := postOfficial(t, s.handleSessionSelectModel,
		`{"sessionId":"s1","provider":"deepseek","model":"v4","reasoningEffort":"high"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("selectModel status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	selected := got["selected"].(map[string]any)
	if selected["provider"] != "deepseek" || selected["model"] != "v4" || selected["reasoningEffort"] != "high" {
		t.Fatalf("selected 形状不符：%v", selected)
	}
	if gotModel != "s1/v4" || gotEffort != "s1/high" {
		t.Fatalf("必须真的写两处覆盖存储：model=%q effort=%q", gotModel, gotEffort)
	}

	for _, body := range []string{
		`{"sessionId":"s1","model":"v4"}`,
		`{"sessionId":"s1","provider":"deepseek"}`,
		`{"provider":"deepseek","model":"v4"}`,
		`{"sessionId":"s1","provider":"deepseek","model":"v4","reasoningEffort":""}`,
	} {
		if rec := postOfficial(t, s.handleSessionSelectModel, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d want 400", body, rec.Code)
		}
	}

	unwired := newTestServer(t, Options{})
	if rec := postOfficial(t, unwired.handleSessionSelectModel,
		`{"sessionId":"s1","provider":"deepseek","model":"v4"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("没接 model override 存储必须 501，实际 %d", rec.Code)
	}
}

func TestOfficialSessionUpdateQueueRemoveAndRefusals(t *testing.T) {
	src := &fakeOfficialSource{logEnd: 0}
	s := officialServerWith(t, src, Options{})
	rec := postOfficial(t, s.handleSessionUpdateQueue, `{"sessionId":"s1","itemId":"turn-3","action":{"kind":"remove"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("updateQueue.remove status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["accepted"] != true {
		t.Fatalf("remove 必须回官方 {accepted:true}：%v", got)
	}
	if len(src.removedIDs) != 1 || src.removedIDs[0] != "turn-3" {
		t.Fatalf("必须真的操作队列：%v", src.removedIDs)
	}

	// 不在队列里 → 404，不能假装撤过。
	src.removeErr = ErrNoSuchQueueItem
	if rec := postOfficial(t, s.handleSessionUpdateQueue,
		`{"sessionId":"s1","itemId":"nope","action":{"kind":"remove"}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("队列里没有该项必须 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// edit：参数校验在前（非文本/全空白 400），能力缺失在后（501）。
	if rec := postOfficial(t, s.handleSessionUpdateQueue,
		`{"sessionId":"s1","itemId":"t","action":{"kind":"edit","content":[{"type":"image","text":""}]}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非 text 块必须 400，实际 %d", rec.Code)
	}
	if rec := postOfficial(t, s.handleSessionUpdateQueue,
		`{"sessionId":"s1","itemId":"t","action":{"kind":"edit","content":[{"type":"text","text":"   "}]}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("全空白必须 400，实际 %d", rec.Code)
	}
	// 宿主没有内容视图（只实现 OfficialSessionHost）时，edit/steer 仍是 501 并点名缺什么；
	// 有内容视图的真实能力见 handlers_session_official_queue_test.go。
	if rec := postOfficial(t, s.handleSessionUpdateQueue,
		`{"sessionId":"s1","itemId":"t","action":{"kind":"edit","content":[{"type":"text","text":"new"}]}}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("没有内容视图的宿主 edit 必须 501，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postOfficial(t, s.handleSessionUpdateQueue,
		`{"sessionId":"s1","itemId":"t","action":{"kind":"steer"}}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("没有内容视图的宿主 steer 必须 501，实际 %d", rec.Code)
	}
	for _, body := range []string{
		`{"itemId":"t","action":{"kind":"remove"}}`,
		`{"sessionId":"s1","action":{"kind":"remove"}}`,
		`{"sessionId":"s1","itemId":"t"}`,
		`{"sessionId":"s1","itemId":"t","action":{"kind":"nope"}}`,
	} {
		if rec := postOfficial(t, s.handleSessionUpdateQueue, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d want 400", body, rec.Code)
		}
	}
}

func TestOfficialSessionProjectionsCarryOnlyRealKeys(t *testing.T) {
	src := &fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}
	s := officialServerWith(t, src, Options{
		Load: func(string, int) (LoadResult, error) {
			return LoadResult{CWD: `D:\work`, Permission: "ask"}, nil
		},
	})
	rec := postOfficial(t, s.handleSessionProjections, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("projections status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["asOfSeq"] != float64(6) {
		t.Fatalf("asOfSeq 必须是真实日志末端：%v", got["asOfSeq"])
	}
	values := got["values"].(map[string]any)
	if values["workingDirectory"] != `D:\work` {
		t.Fatalf("workingDirectory 必须来自 load 投影：%v", values["workingDirectory"])
	}
	perms, ok := values["permissions"].(map[string]any)
	if !ok || perms["currentValue"] != "ask" {
		t.Fatalf("permissions.currentValue 必须来自 load 投影：%v", values["permissions"])
	}
	meta := values["sessionListMetadata"].(map[string]any)
	if meta["blank"] != false {
		t.Fatalf("有 turn/start 时 blank 必须 false：%v", meta)
	}
	if meta["lastPromptAt"] != float64(1005) {
		t.Fatalf("lastPromptAt 必须是最新人类 prompt 的时间 1005：%v", meta["lastPromptAt"])
	}
	// 没有真值的键必须整个不出现（官方 optional = 该能力不存在）。
	for _, key := range []string{"modelSelection", "todos", "inbox", "actionCards", "agentPreset", "subagent", "imageLimits"} {
		if _, has := values[key]; has {
			t.Fatalf("%q 本宿主没有真值，不许编：%v", key, values)
		}
	}
}

// 四个流方法必须是**真流**（NDJSON 长连接），不是 501。
//
// 这条只守"别再退回 501"这一层；帧语义（snapshot→event、cursor 单调、
// hasMore、不重放历史、A/B 隔离、取消退出）在 official_stream_test.go 里逐条钉。
func TestOfficialSessionStreamMethodsAreRealStreams(t *testing.T) {
	s := officialStreamTestServer(t, newMutableOfficialSource(officialFixtureEvents()))
	cases := []struct {
		name    string
		handler officialHandler
		body    string
		// wantType 是官方首帧的 type；observeState 的 item 是 SessionState 本身
		// （没有 type 判别式），所以给它空串，靠 wantCursor 判。
		wantType   string
		wantCursor bool
	}{
		{"observeState", s.handleSessionObserveState, `{"sessionId":"s1"}`, "", true},
		{"observeProjections", s.handleSessionObserveProjections, `{"sessionId":"s1"}`, "ready", false},
		{"observeEvent", s.handleSessionObserveEvent, `{"sessionId":"s1"}`, "ready", false},
		{"follow", s.handleSessionFollow, `{"address":{"kind":"session","sessionId":"s1"}}`, "snapshot", true},
	}
	for _, tc := range cases {
		rec, _ := runStream(t, tc.handler, tc.body)
		frames := rec.waitFrames(t, 1)
		if code := rec.status(); code != http.StatusOK {
			t.Errorf("%s status=%d want 200", tc.name, code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
			t.Errorf("%s Content-Type=%q want NDJSON 长连接", tc.name, got)
		}
		if tc.wantType != "" && frames[0]["type"] != tc.wantType {
			t.Errorf("%s 首帧 type=%v want %q", tc.name, frames[0]["type"], tc.wantType)
		}
		if tc.wantCursor {
			if _, has := frames[0]["cursor"]; !has {
				t.Errorf("%s 首帧缺 cursor：%v", tc.name, frames[0])
			}
		}
	}

	// 没有 header 来源时 follow 明确 501 并点名缺的能力（不编 header）。
	noHeader := newTestServer(t, Options{
		Source:   &noHeaderSource{fakeOfficialSource: fakeOfficialSource{events: officialFixtureEvents(), logEnd: 6}},
		Journals: NewJournals(nil),
	})
	rec, _ := runStream(t, noHeader.handleSessionFollow, `{"address":{"kind":"session","sessionId":"s1"}}`)
	if code := rec.waitStatus(t); code != http.StatusNotImplemented {
		t.Fatalf("没有 header 来源必须 501，实际 %d", code)
	}
}

func TestOfficialSessionWorkspacePathMethods(t *testing.T) {
	s := newTestServer(t, Options{})
	// 没有桌面打开通道：opened 不能假装 true。
	rec := postOfficial(t, s.handleSessionOpenWorkspacePath, `{"path":"/work/a.txt"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("openWorkspacePath 必须 501，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if code := errorField(t, rec.Body.Bytes(), "code"); code != "not_implemented" {
		t.Fatalf("openWorkspacePath code=%q", code)
	}
	if rec := postOfficial(t, s.handleSessionOpenWorkspacePath, `{"path":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 path 必须 400，实际 %d", rec.Code)
	}
	if rec := postOfficial(t, s.handleSessionOpenWorkspacePath, `{"path":"x","action":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 action 必须 400，实际 %d", rec.Code)
	}

	// 文件关联查询同样 501：不拿空数组冒充"这个系统没有关联程序"。
	rec = postOfficial(t, s.handleSessionWorkspacePathApplications, `{"path":"/work/a.txt"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("workspacePathApplications 必须 501，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if code := errorField(t, rec.Body.Bytes(), "code"); code != "not_implemented" {
		t.Fatalf("workspacePathApplications code=%q", code)
	}
	if rec := postOfficial(t, s.handleSessionWorkspacePathApplications, `{"path":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 path 必须 400，实际 %d", rec.Code)
	}
	if rec := postOfficial(t, s.handleSessionWorkspacePathApplications, `{"path":"x","nope":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段必须 400，实际 %d", rec.Code)
	}
}

// 没接线时统一 501（不是 500，也不是假成功）。
func TestOfficialSessionHandlersUnwiredReturn501(t *testing.T) {
	s := newTestServer(t, Options{})
	cases := []struct {
		name    string
		handler officialHandler
		body    string
	}{
		{"page", s.handleSessionPage, `{"address":{"kind":"session","sessionId":"s1"}}`},
		{"events", s.handleSessionEvents, `{"address":{"kind":"session","sessionId":"s1"}}`},
		{"state", s.handleSessionState, `{"sessionId":"s1"}`},
		{"projections", s.handleSessionProjections, `{"sessionId":"s1"}`},
		{"wait", s.handleSessionWait, `{"sessionId":"s1","timeoutMs":10}`},
		{"search", s.handleSessionSearch, `{"query":"x"}`},
		{"updateQueue", s.handleSessionUpdateQueue, `{"sessionId":"s1","itemId":"t","action":{"kind":"remove"}}`},
	}
	for _, tc := range cases {
		rec := postOfficial(t, tc.handler, tc.body)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s 未接线必须 501，实际 %d body=%s", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if code := errorField(t, rec.Body.Bytes(), "code"); code != "not_implemented" {
			t.Errorf("%s code=%q", tc.name, code)
		}
	}
}

// 官方 15 个方法不得进入 canonical 25，也不得与既有方法重名。
func TestOfficialSessionMethodsStayOutsideCanonical25(t *testing.T) {
	methods := OfficialSessionMethods()
	if len(methods) != 15 {
		t.Fatalf("官方 session 对齐面必须是 15 个方法，实际 %d", len(methods))
	}
	if len(SessionMethods()) != 25 {
		t.Fatalf("canonical 25 被改了：%d", len(SessionMethods()))
	}
	canonical := map[Method]struct{}{}
	for _, m := range SessionMethods() {
		canonical[m] = struct{}{}
	}
	for _, m := range methods {
		if _, dup := canonical[m]; dup {
			t.Fatalf("%s 不能进 canonical 25", m)
		}
	}
	// 方法名必须是官方那 15 个原样字符串。
	want := []string{
		"session.page", "session.state", "session.projections",
		"session.observeState", "session.observeProjections", "session.observeEvent",
		"session.follow", "session.events", "session.updateQueue", "session.wait",
		"session.search", "session.attachment", "session.selectModel",
		"session.openWorkspacePath", "session.workspacePathApplications",
	}
	got := map[string]struct{}{}
	for _, m := range methods {
		got[string(m)] = struct{}{}
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("缺官方方法 %s", name)
		}
	}
}

// errorField 取四字段错误信封里的一个字段（没有就返回 nil）。
func errorField(t *testing.T, raw []byte, key string) any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("错误信封不是 JSON：%v（%s）", err, string(raw))
	}
	return env.Error[key]
}

// minInt 是本文件用的小工具（不复用 Go 内建 min，免得在包内遮蔽它）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
