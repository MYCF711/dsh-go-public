package transport

// 官方 `session.state.queue` 与 `session.updateQueue{edit,steer}` 的契约测试。
//
// 与 handlers_session_official_test.go 那一份的分工：
//   - 那一份钉"没有内容视图的宿主仍然 501，且参数校验在前"；
//   - 这一份钉"宿主有真实内容视图时，state.queue 回真实条目、edit/steer 真的落到
//     宿主动作上、错误分类正确（404 / 409 / 400）"。
//
// 为什么是独立假源：`fakeOfficialSource` 只实现 OfficialSessionHost，故意**不**
// 实现 OfficialQueueHost，用来证明"没有内容视图就不编内容"这条边界仍然成立。

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// fakeQueueSource 在 fakeOfficialSource 之上加真实队列能力。
type fakeQueueSource struct {
	*fakeOfficialSource
	queue     []OfficialQueueItem
	queueErr  error
	editErr   error
	steerErr  error
	edits     []string
	editBody  [][]byte
	steered   []string
	editCalls int
	steerCall int
}

func (f *fakeQueueSource) LiveQueue(string) ([]OfficialQueueItem, error) {
	return f.queue, f.queueErr
}

func (f *fakeQueueSource) EditQueueItem(_ string, itemID string, content []json.RawMessage) error {
	f.editCalls++
	if f.editErr != nil {
		return f.editErr
	}
	f.edits = append(f.edits, itemID)
	for _, raw := range content {
		f.editBody = append(f.editBody, raw)
	}
	return nil
}

func (f *fakeQueueSource) SteerQueueItem(_ string, itemID string) error {
	f.steerCall++
	if f.steerErr != nil {
		return f.steerErr
	}
	f.steered = append(f.steered, itemID)
	return nil
}

// 有真实内容视图时，state.queue 必须是真实条目：id/placement/content 都在，
// content 是**待发内容本身**（不是空数组），active 跟着它变真。
func TestOfficialSessionStateQueueCarriesRealContent(t *testing.T) {
	src := &fakeQueueSource{
		fakeOfficialSource: &fakeOfficialSource{logEnd: 3},
		queue: []OfficialQueueItem{
			{ID: "turn-4", Placement: "queued", Content: []json.RawMessage{
				json.RawMessage(`{"type":"text","text":"排队的第一条"}`),
			}},
			{ID: "turn-5", Placement: "queued", Content: []json.RawMessage{
				json.RawMessage(`{"type":"text","text":"排队的第二条"}`),
			}},
		},
	}
	s := officialServerWith(t, src.fakeOfficialSource, Options{})
	// 用真实假源（实现了 OfficialQueueHost）建服务器。
	s = newTestServer(t, Options{Source: src})

	rec := postOfficial(t, s.handleSessionState, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	queue, ok := got["queue"].([]any)
	if !ok {
		t.Fatalf("有内容视图时 queue 必须是数组，实际 %v（null 等于把真实待发内容藏起来）", got["queue"])
	}
	if len(queue) != 2 {
		t.Fatalf("queue 条数 = %d，要 2", len(queue))
	}
	first := queue[0].(map[string]any)
	if first["id"] != "turn-4" || first["placement"] != "queued" {
		t.Fatalf("queue[0] 必须是真实 TurnID 与 placement=queued：%v", first)
	}
	content, ok := first["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("queue[0].content 必须是真实内容数组：%v", first["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "排队的第一条" {
		t.Fatalf("content 里的块必须是待发内容本身：%v", block)
	}
	if got["active"] != true {
		t.Fatalf("有排队项时 active 必须为真：%v", got)
	}
}

// 没有内容视图的宿主（只实现 OfficialSessionHost）仍然给 null：不拿 TurnID 拼空 content。
func TestOfficialSessionStateQueueStaysNullWithoutContentView(t *testing.T) {
	src := &fakeOfficialSource{logEnd: 3, live: OfficialLiveState{Active: true, QueueIDs: []string{"turn-4"}}}
	s := officialServerWith(t, src, Options{})
	rec := postOfficial(t, s.handleSessionState, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("state status=%d", rec.Code)
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["queue"] != nil {
		t.Fatalf("没有内容视图时 queue 必须是 null，实际 %v", got["queue"])
	}
}

// edit 真的落到宿主动作上；不在队列里 / 当前轮不接受 steer 各回正确的错。
func TestOfficialSessionUpdateQueueEditAndSteerAreReal(t *testing.T) {
	src := &fakeQueueSource{fakeOfficialSource: &fakeOfficialSource{logEnd: 3}}
	s := newTestServer(t, Options{Source: src})

	rec := postOfficial(t, s.handleSessionUpdateQueue,
		`{"sessionId":"s1","itemId":"turn-4","action":{"kind":"edit","content":[{"type":"text","text":"改过的正文"}]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := unwrapResult(t, rec.Body.Bytes()); got["accepted"] != true {
		t.Fatalf("edit 必须回官方 {accepted:true}：%v", got)
	}
	if len(src.edits) != 1 || src.edits[0] != "turn-4" {
		t.Fatalf("edit 必须真的打到宿主：%v", src.edits)
	}
	if len(src.editBody) != 1 {
		t.Fatalf("edit 必须把新内容透传给宿主：%d 块", len(src.editBody))
	}
	var block map[string]any
	if err := json.Unmarshal(src.editBody[0], &block); err != nil {
		t.Fatalf("透传的块不是 JSON：%v", err)
	}
	if block["type"] != "text" || block["text"] != "改过的正文" {
		t.Fatalf("透传的内容必须是客户端给的新文本：%v", block)
	}

	rec = postOfficial(t, s.handleSessionUpdateQueue, `{"sessionId":"s1","itemId":"turn-4","action":{"kind":"steer"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("steer status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(src.steered) != 1 || src.steered[0] != "turn-4" {
		t.Fatalf("steer 必须真的打到宿主注入通道：%v", src.steered)
	}

	// 不在队列里：404（不是 200、也不是 501）。
	src.editErr = ErrNoSuchQueueItem
	if rec := postOfficial(t, s.handleSessionUpdateQueue,
		`{"sessionId":"s1","itemId":"gone","action":{"kind":"edit","content":[{"type":"text","text":"x"}]}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("edit 不在队列里必须 404，实际 %d", rec.Code)
	}
	// 当前轮不再接受 steer：409（官方 session/steer-unavailable）。
	src.steerErr = ErrSteerUnavailable
	rec = postOfficial(t, s.handleSessionUpdateQueue, `{"sessionId":"s1","itemId":"turn-4","action":{"kind":"steer"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("steer 不可用必须 409，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if code := errorField(t, rec.Body.Bytes(), "code"); code != "conflict" {
		t.Fatalf("steer 不可用的 code=%v，要 conflict", code)
	}

	// 参数校验仍然在前：非 text 块/全空白 400，且**不许**碰宿主。
	before := src.editCalls
	for _, body := range []string{
		`{"sessionId":"s1","itemId":"t","action":{"kind":"edit","content":[{"type":"image","text":""}]}}`,
		`{"sessionId":"s1","itemId":"t","action":{"kind":"edit","content":[{"type":"text","text":"   "}]}}`,
		`{"sessionId":"s1","itemId":"t","action":{"kind":"edit","content":[]}}`,
	} {
		if rec := postOfficial(t, s.handleSessionUpdateQueue, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s 必须 400，实际 %d", body, rec.Code)
		}
	}
	if src.editCalls != before {
		t.Fatalf("参数非法时不许调用宿主 edit：%d → %d", before, src.editCalls)
	}
}

// 宿主把"没有这一项"与"没有内容视图"分开报，transport 不许把两者混成一个错。
func TestOfficialSessionQueueErrorClassMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"no-such-item", ErrNoSuchQueueItem, http.StatusNotFound},
		{"steer-unavailable", ErrSteerUnavailable, http.StatusConflict},
		{"unsupported", ErrSessionOfficialUnsupported, http.StatusNotImplemented},
		{"placement-invalid", ErrPlacementInvalid, http.StatusBadRequest},
		{"wrapped-no-such-item", errors.Join(ErrNoSuchQueueItem, errors.New("detail")), http.StatusNotFound},
	}
	for _, tc := range cases {
		src := &fakeQueueSource{fakeOfficialSource: &fakeOfficialSource{logEnd: 0}, editErr: tc.err}
		s := newTestServer(t, Options{Source: src})
		rec := postOfficial(t, s.handleSessionUpdateQueue,
			`{"sessionId":"s1","itemId":"t","action":{"kind":"edit","content":[{"type":"text","text":"x"}]}}`)
		if rec.Code != tc.want {
			t.Errorf("%s: status=%d want %d body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}
