package transport

// **mux 面**的流测试：把帧机接成 `officialwire.Stream`，逐帧断言 mux 的
// `item.value` 就是官方帧（不是包壳、不是空流）。
//
// # 为什么要在这里再测一遍（HTTP 面已经测过帧语义）
//
// 两条面共用一台帧机，但**承接方式**不同：HTTP 直接写 NDJSON 行，mux 把同一帧
// 塞进 `item.value`。历史事故的形状就是"HTTP 对了、mux 少一格"，所以 mux 侧
// 必须有自己的断言。
//
// # 下面这个适配器是接线代码的**逐字副本**
//
// 生产里它要放在 cmd/dshd/serve_official_wire.go（本任务的写作用域不含 cmd），
// 内容与 HOST-STREAM-METHODS-RESULT.md 的「接线清单」完全一致。放一份在测试里
// 是为了让"接线能不能编译、帧能不能过 officialwire 的 Stream 接口"是可验证的，
// 而不是等 Lead 接线时才发现签名不对。

import (
	"context"
	"encoding/json"
	"testing"

	"dshgo/internal/officialwire"
)

// officialMuxStreamOpener 把 mux 的 open 帧接到官方帧机上（生产接线用）。
type officialMuxStreamOpener struct {
	server *Server
}

var _ officialwire.StreamOpener = officialMuxStreamOpener{}

// Open 把 mux 的 open 帧接到官方帧机上。
func (o officialMuxStreamOpener) Open(ctx context.Context, endpoint string, args json.RawMessage) (officialwire.Stream, *officialwire.Error) {
	if endpoint == string(MethodSessionEvents) {
		// 本仓自有的尾部流（官方 session/events 是 unary 页，不是流）。
		var request struct {
			SessionID string `json:"sessionId"`
			SinceSeq  *int   `json:"sinceSeq"`
			Limit     *int   `json:"limit"`
		}
		if err := json.Unmarshal(args, &request); err != nil {
			return nil, officialwire.ArgumentsInvalid(endpoint, "args", "args 必须是对象：sessionId + 可选 sinceSeq/limit")
		}
		sinceSeq, limit := SinceNothing, officialStreamPageLimit
		if request.SinceSeq != nil {
			sinceSeq = *request.SinceSeq
		}
		if request.Limit != nil {
			limit = *request.Limit
		}
		stream, fault := o.server.OpenOfficialJournalTail(request.SessionID, sinceSeq, limit)
		if fault != nil {
			return nil, officialwire.NewError(fault.Code, fault.Message, map[string]any{"endpoint": endpoint})
		}
		return &officialMuxStream{stream: stream}, nil
	}
	stream, fault := o.server.OpenOfficialFrameStream(ctx, endpoint, args)
	if fault != nil {
		return nil, officialwire.NewError(fault.Code, fault.Message, map[string]any{"endpoint": endpoint})
	}
	return &officialMuxStream{stream: stream}, nil
}

// officialMuxStream 把 OfficialFrameStream 接成 officialwire.Stream。
type officialMuxStream struct {
	stream OfficialFrameStream
}

func (m *officialMuxStream) Next(ctx context.Context) (officialwire.StreamItem, bool, *officialwire.Error) {
	frame, done, fault := m.stream.Next(ctx)
	if fault != nil {
		return officialwire.StreamItem{}, true,
			officialwire.NewError(fault.Code, fault.Message, map[string]any{"endpoint": ""})
	}
	if done {
		return officialwire.StreamItem{}, true, nil
	}
	return officialwire.StreamItem{Value: frame, HasValue: true}, false, nil
}

func (m *officialMuxStream) Close() { m.stream.Close() }

// TestOfficialMuxStreamFollowFrames 钉住 mux 上的 snapshot → event 两帧。
func TestOfficialMuxStreamFollowFrames(t *testing.T) {
	src := newMutableOfficialSource(officialFixtureEvents())
	s := officialStreamTestServer(t, src)
	opener := officialMuxStreamOpener{server: s}

	args := json.RawMessage(`{"address":{"kind":"session","sessionId":"s1"}}`)
	stream, failure := opener.Open(context.Background(), "session/follow", args)
	if failure != nil {
		t.Fatalf("open 失败：%v", failure)
	}
	defer stream.Close()

	item, done, failure := stream.Next(context.Background())
	if failure != nil || done {
		t.Fatalf("首帧必须是一个 item：done=%v failure=%v", done, failure)
	}
	if !item.HasValue {
		t.Fatal("item 必须带 value（mux 的 value 就是官方帧本身）")
	}
	var snapshot map[string]any
	if err := json.Unmarshal(item.Value, &snapshot); err != nil {
		t.Fatalf("item.value 不是 JSON：%v", err)
	}
	if snapshot["type"] != "snapshot" || snapshot["cursor"] != float64(6) {
		t.Fatalf("mux 上首帧必须是 snapshot 且 cursor=6：%v", snapshot)
	}

	src.append(eventAt(7, "assistant/message", `{"content":[]}`))
	item, done, failure = stream.Next(context.Background())
	if failure != nil || done {
		t.Fatalf("第二帧必须是一个 item：done=%v failure=%v", done, failure)
	}
	var event map[string]any
	if err := json.Unmarshal(item.Value, &event); err != nil {
		t.Fatalf("item.value 不是 JSON：%v", err)
	}
	if event["type"] != "event" {
		t.Fatalf("mux 上第二帧必须是 event 帧：%v", event)
	}
	payload, _ := event["event"].(map[string]any)
	if payload["seq"] != float64(7) {
		t.Fatalf("事件 seq 应为 7：%v", payload)
	}
}

// TestOfficialMuxStreamStateHasNoTypeDiscriminator 钉住 observeState 的 item
// **就是 SessionState 本身**（官方没有 type 判别式，多包一层客户端就解不出）。
func TestOfficialMuxStreamStateHasNoTypeDiscriminator(t *testing.T) {
	src := newMutableOfficialSource(officialFixtureEvents())
	s := officialStreamTestServer(t, src)
	opener := officialMuxStreamOpener{server: s}

	stream, failure := opener.Open(context.Background(), "session/observeState",
		json.RawMessage(`{"sessionId":"s1"}`))
	if failure != nil {
		t.Fatalf("open 失败：%v", failure)
	}
	defer stream.Close()

	item, _, failure := stream.Next(context.Background())
	if failure != nil {
		t.Fatalf("next 失败：%v", failure)
	}
	var state map[string]any
	if err := json.Unmarshal(item.Value, &state); err != nil {
		t.Fatalf("item.value 不是 JSON：%v", err)
	}
	if _, has := state["type"]; has {
		t.Fatalf("SessionState 没有 type 字段，多包一层客户端解不出：%v", state)
	}
	for _, key := range []string{"cursor", "active", "running", "pendingInput"} {
		if _, has := state[key]; !has {
			t.Fatalf("SessionState 缺 %s：%v", key, state)
		}
	}
}

// TestOfficialMuxStreamUnwiredIsMethodUnavailable 钉住"宿主没能力"是
// gateway/method-unavailable，而不是空 end（客户端会以为流正常结束了）。
func TestOfficialMuxStreamUnwiredIsMethodUnavailable(t *testing.T) {
	s := newTestServer(t, Options{Journals: NewJournals(nil)})
	opener := officialMuxStreamOpener{server: s}

	_, failure := opener.Open(context.Background(), "session/follow",
		json.RawMessage(`{"address":{"kind":"session","sessionId":"s1"}}`))
	if failure == nil {
		t.Fatal("宿主没有官方会话数据源时必须报错")
	}
	if failure.Code != officialwire.CodeMethodUnavailable {
		t.Fatalf("码应为 gateway/method-unavailable，实际 %q", failure.Code)
	}
}

// TestOfficialMuxJournalTailSinceSeqSentinel 钉住 `session/events` 尾部流的
// -1 哨兵（`Window` 的排他判据 + Covered 修正）。
func TestOfficialMuxJournalTailSinceSeqSentinel(t *testing.T) {
	s := newTestServer(t, Options{Journals: NewJournals(nil)})
	for i := 1; i <= 3; i++ {
		s.opt.Journals.Append("s1", "assistant/message", json.RawMessage(`{"text":"x"}`), 1000+int64(i))
	}

	// 尾部流是**不会自己结束**的（跟到取消为止），所以读满 3 帧就停。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, failure := s.OpenOfficialJournalTail("s1", SinceNothing, officialStreamPageLimit)
	if failure != nil {
		t.Fatalf("sinceSeq=-1 必须被接受：%v", failure)
	}
	defer stream.Close()
	for want := 1; want <= 3; want++ {
		frame, done, fault := stream.Next(ctx)
		if fault != nil {
			t.Fatalf("流失败：%v", fault)
		}
		if done {
			t.Fatalf("第 %d 帧之前流就结束了（journal 里有 3 帧）", want)
		}
		var decoded struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(frame, &decoded); err != nil {
			t.Fatalf("journal 帧不是 JSON：%v", err)
		}
		if decoded.Seq != want {
			t.Fatalf("第 %d 帧 seq=%d（-1 必须从第一帧开始给）", want, decoded.Seq)
		}
	}

	// 小于 -1 是参数错（不是"从头开始"的另一种写法）。
	if _, failure := s.OpenOfficialJournalTail("s1", -2, 10); failure == nil ||
		failure.Code != "gateway/bad-request" {
		t.Fatalf("sinceSeq=-2 必须 400，实际 %v", failure)
	}
}
