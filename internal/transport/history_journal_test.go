package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// H14：history 必须返回**原始帧**（含 reasoning），并与 WS 同一个序号域。
//
// 这条测试能抓住"用投影补洞"的退化：投影只留 text，reasoning/tool 会消失，
// 而断线重连后最需要恢复的恰恰是它们。
func TestHistoryReturnsRawFrames(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s1")
	j.Append("assistant/message", json.RawMessage(`{"message":{"content":[{"type":"reasoning","text":"想了"}]}}`), 100)
	j.Append("tool/call", json.RawMessage(`{"name":"bash"}`), 101)

	s := newTestServer(t, Options{Journals: js})
	rec := postJSON(t, s, "/api/session.history", `{"id":"s1","sinceSeq":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result HistoryResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	r := env.Result
	if len(r.Frames) != 2 {
		t.Fatalf("应返回 2 帧，实际 %d", len(r.Frames))
	}
	if r.Frames[0].Seq != 1 || r.Frames[1].Seq != 2 {
		t.Fatalf("seq 应由 journal 分配（1,2），实际 %d,%d", r.Frames[0].Seq, r.Frames[1].Seq)
	}
	if r.LastSeq != 2 || r.JournalEpoch == 0 {
		t.Fatalf("要带水位与世代：last=%d epoch=%d", r.LastSeq, r.JournalEpoch)
	}
	if got := string(r.Frames[0].Data); got == "" {
		t.Fatalf("帧载荷不能是空的")
	}
	if !strings.Contains(string(r.Frames[0].Data), "reasoning") {
		t.Fatalf("reasoning 被投影吃掉了：%s", string(r.Frames[0].Data))
	}
}

// H14：客户端带着不认识的老世代或超前水位来补洞 → 明确要求重建基线。
//
// 这条比"补出错位数据"重要：错位数据看起来是对的，用户不会发现。
func TestHistoryRejectsStaleEpoch(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s1")
	j.Append("text", nil, 1)
	epoch := j.Epoch()

	s := newTestServer(t, Options{Journals: js})

	// 老世代
	rec := postJSON(t, s, "/api/session.history",
		`{"id":"s1","sinceSeq":0,"journalEpoch":`+strconv.Itoa(int(epoch)+1)+`}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("世代不匹配要 409，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stale_epoch") {
		t.Fatalf("错误码要 stale_epoch：%s", rec.Body.String())
	}

	// 水位比当前还大
	rec = postJSON(t, s, "/api/session.history", `{"id":"s1","sinceSeq":99,"journalEpoch":`+strconv.Itoa(int(epoch))+`}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("超前水位要 409，拿到 %d：%s", rec.Code, rec.Body.String())
	}
}

// 水位之前的帧已经滑出内存 journal 时，history 必须报缺口，不能给一个"看着连续"的短数组。
//
// capacity 丢最老帧之后，sinceSeq=0 的请求只能从最老保留帧开始回——客户端拿到的是
// seq 3..N，中间 1..2 永远补不回来，而响应是 200、帧是连续的，客户端不会怀疑。
// 契约里已经有 stale_epoch 这个出口（重建基线），这里必须用它。
func TestHistorySignalsGapAfterTrim(t *testing.T) {
	// 直接造一条容量只有 3 的 journal：等价于长跑会话把最老帧挤出去了。
	js := &Journals{m: map[string]*Journal{"s1": NewJournal(1, 3)}}
	j := js.Get("s1")
	for i := 1; i <= 5; i++ {
		j.Append("assistant/text", json.RawMessage(`{"text":"x"}`), int64(i))
	}

	s := newTestServer(t, Options{Journals: js})
	rec := postJSON(t, s, "/api/session.history", `{"id":"s1","sinceSeq":0}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("水位滑出内存必须 409（stale_epoch），拿到 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "stale_epoch") {
		t.Fatalf("错误码要 stale_epoch：%s", body)
	}
	if !strings.Contains(body, "lastSeq") || !strings.Contains(body, `"lastSeq":5`) {
		t.Fatalf("要带上当前水位，客户端才知道重载后从哪接：%s", body)
	}

	// 还没滑出去的水位仍然正常：sinceSeq=2 正好能从 3 接到。
	rec = postJSON(t, s, "/api/session.history", `{"id":"s1","sinceSeq":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("水位没滑出内存时不该报缺口，拿到 %d：%s", rec.Code, rec.Body.String())
	}
}

// 空集必须是 []，不能是 null。
//
// 界面审计实测：会话还一条事件都没有时，frames 序列化成 null；
// 客户端按数组解会得到 nil，for 起来什么也不做——界面表现是"什么都没有"，
// 而它分不清"没数据"和"接口坏了"。
func TestEmptyFramesSerializeAsArray(t *testing.T) {
	js := NewJournals(func() int64 { return 1 })
	j := js.Get("s-empty")
	frames, _, _ := j.Since(0, 100)
	if frames == nil {
		t.Fatal("Since 返回了 nil，序列化会变成 null")
	}
	raw, err := json.Marshal(HistoryResult{Frames: frames, LastSeq: 0, JournalEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"frames":[]`) {
		t.Fatalf("空集应当序列化成 []，实际 %s", raw)
	}
}
