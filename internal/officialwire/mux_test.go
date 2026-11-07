package officialwire

import (
	"context"
	"encoding/base64"
	"errors"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpener 是 mux 测试用的流工厂。
type fakeOpener struct {
	streams map[string]func() Stream
	failures map[string]*Error
}

func (o fakeOpener) Open(_ context.Context, endpoint string, _ json.RawMessage) (Stream, *Error) {
	if failure, ok := o.failures[endpoint]; ok {
		return nil, failure
	}
	if factory, ok := o.streams[endpoint]; ok {
		return factory(), nil
	}
	return nil, MethodUnavailable(endpoint, "本测试没有登记这条流")
}

// syncStream 按顺序吐 items，然后（可选）一直等到取消。
type syncStream struct {
	items      []StreamItem
	index      int
	blockAfter bool
	closed     int
	mu         sync.Mutex
}

func (s *syncStream) Next(ctx context.Context) (StreamItem, bool, *Error) {
	s.mu.Lock()
	index := s.index
	if index < len(s.items) {
		s.index++
		s.mu.Unlock()
		return s.items[index], false, nil
	}
	block := s.blockAfter
	s.mu.Unlock()
	if block {
		<-ctx.Done()
	}
	return StreamItem{}, true, nil
}

func (s *syncStream) Close() {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
}

// uplinkStream 是既下行又接收上行的流。
type uplinkStream struct {
	syncStream
	upMu   sync.Mutex
	pushed []json.RawMessage
}

func (s *uplinkStream) Push(value json.RawMessage) *Error {
	s.upMu.Lock()
	s.pushed = append(s.pushed, append(json.RawMessage(nil), value...))
	s.upMu.Unlock()
	return nil
}

func (s *uplinkStream) pushedCount() int {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	return len(s.pushed)
}

func textItem(value string) StreamItem {
	encoded, _ := json.Marshal(value)
	return StreamItem{Value: encoded, HasValue: true}
}

// muxConfig 造一份带流工厂的配置。
func muxConfig(opener StreamOpener) ServerConfig {
	cfg := testServerConfig()
	cfg.Opener = opener
	return cfg
}

// dialAuthorized 完成 /connect 并连上 mux，返回客户端与 cookie。
func dialAuthorized(t *testing.T, ts *httptest.Server, client *http.Client, interactions string) (*wsClient, []*http.Cookie) {
	t.Helper()
	cookies := connectAndCookies(t, ts, client, interactions)
	ws, response := dialMux(t, ts, cookies)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("mux 升级应 101，实际 %d", response.StatusCode)
	}
	t.Cleanup(ws.Close)
	return ws, cookies
}

// TestMuxRequiresCookie 钉住"升级前先认证"（EVIDENCE §3、§4.3）。
func TestMuxRequiresCookie(t *testing.T) {
	_, ts, _ := startWire(t, muxConfig(fakeOpener{}), nil)
	ws, response := dialMux(t, ts, nil)
	defer ws.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("没有 cookie 应 401，实际 %d", response.StatusCode)
	}
}

// TestMuxSingleSocketPerPeer 钉住"一个 Peer 一条 socket，第二条 409"。
func TestMuxSingleSocketPerPeer(t *testing.T) {
	_, ts, client := startWire(t, muxConfig(fakeOpener{}), nil)
	cookies := connectAndCookies(t, ts, client, InteractionsNone)

	first, response := dialMux(t, ts, cookies)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("第一条 socket 应 101，实际 %d", response.StatusCode)
	}
	defer first.Close()

	second, secondResponse := dialMux(t, ts, cookies)
	defer second.Close()
	if secondResponse.StatusCode != http.StatusConflict {
		t.Fatalf("同一 Peer 第二条 socket 应 409，实际 %d", secondResponse.StatusCode)
	}
}

// TestMuxBinaryFrameCloses1003 钉住"mux 只收文本帧"。
func TestMuxBinaryFrameCloses1003(t *testing.T) {
	_, ts, client := startWire(t, muxConfig(fakeOpener{}), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	ws.writeFrame(t, opBinary, []byte{0x01, 0x02})
	if code := ws.readCloseCode(t, 2*time.Second); code != closeBinaryUnsupported {
		t.Fatalf("二进制帧应关 1003，实际 %d", code)
	}
}

// TestMuxInvalidFrameCloses1008 钉住 exact-key：多一个字段就断连（EVIDENCE §3.1）。
func TestMuxInvalidFrameCloses1008(t *testing.T) {
	_, ts, client := startWire(t, muxConfig(fakeOpener{}), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	ws.writeText(t, `{"type":"open","streamId":"a","endpoint":"test/stream","payload":{"args":{}},"extra":1}`)
	if code := ws.readCloseCode(t, 2*time.Second); code != closeInvalidFrame {
		t.Fatalf("非法帧应关 1008，实际 %d", code)
	}
}

// TestMuxDuplicateOpenCloses1008 钉住"同一个 streamId 重复 open 关 socket"。
func TestMuxDuplicateOpenCloses1008(t *testing.T) {
	opener := fakeOpener{streams: map[string]func() Stream{
		"test/stream": func() Stream { return &syncStream{blockAfter: true} },
	}}
	_, ts, client := startWire(t, muxConfig(opener), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	ws.writeText(t, openFrame("dup", "test/stream", ""))
	// 等第一条流的 goroutine 起来（它不会立刻发帧）。
	time.Sleep(50 * time.Millisecond)
	ws.writeText(t, openFrame("dup", "test/stream", ""))
	if code := ws.readCloseCode(t, 2*time.Second); code != closeInvalidFrame {
		t.Fatalf("重复 open 应关 1008，实际 %d", code)
	}
}

// TestMuxStreamDeliversItemsThenEnd 钉住 item/end 两帧与 cancel 容忍。
func TestMuxStreamDeliversItemsThenEnd(t *testing.T) {
	opener := fakeOpener{streams: map[string]func() Stream{
		"test/stream": func() Stream {
			return &syncStream{items: []StreamItem{textItem("one"), textItem("two")}}
		},
	}}
	_, ts, client := startWire(t, muxConfig(opener), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	ws.writeText(t, openFrame("s1", "test/stream", ""))
	for _, want := range []string{"one", "two"} {
		frame := ws.readJSONFrame(t, 2*time.Second)
		if frame["type"] != FrameItem || frame["streamId"] != "s1" {
			t.Fatalf("应收到 item 帧：%v", frame)
		}
		if frame["value"] != want {
			t.Fatalf("item 值不对：%v", frame)
		}
	}
	end := ws.readJSONFrame(t, 2*time.Second)
	if end["type"] != FrameEnd || end["streamId"] != "s1" {
		t.Fatalf("流结束应发 end 帧：%v", end)
	}

	// 客户端 finally 会补发 cancel（EVIDENCE §3.3）：这里不许回错、也不许断连。
	ws.writeText(t, `{"type":"cancel","streamId":"s1"}`)

	// socket 仍然可用。
	ws.writeText(t, openFrame("s2", "test/stream", ""))
	next := ws.readJSONFrame(t, 2*time.Second)
	if next["streamId"] != "s2" {
		t.Fatalf("end 之后的 cancel 不该影响后续流：%v", next)
	}
}

// TestMuxOpenUnknownEndpointIsMethodUnavailable 钉住"未接线的流回 error 帧，不断连"。
func TestMuxOpenUnknownEndpointIsMethodUnavailable(t *testing.T) {
	_, ts, client := startWire(t, muxConfig(fakeOpener{}), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	ws.writeText(t, openFrame("s1", "session/nothing", ""))
	frame := ws.readJSONFrame(t, 2*time.Second)
	if frame["type"] != FrameError {
		t.Fatalf("应收到 error 帧：%v", frame)
	}
	failure, _ := frame["error"].(map[string]any)
	if failure["code"] != CodeMethodUnavailable {
		t.Fatalf("码应为 %s，实际 %v", CodeMethodUnavailable, failure)
	}
}

// TestMuxUplinkOverflowIsPerStream 钉住 256 KiB 配额与"不关 socket"。
func TestMuxUplinkOverflowIsPerStream(t *testing.T) {
	stream := &uplinkStream{}
	stream.blockAfter = true
	opener := fakeOpener{streams: map[string]func() Stream{
		"test/uplink": func() Stream { return stream },
		"test/stream": func() Stream { return &syncStream{items: []StreamItem{textItem("alive")}} },
	}}
	_, ts, client := startWire(t, muxConfig(opener), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	ws.writeText(t, openFrame("up", "test/uplink", ""))
	time.Sleep(30 * time.Millisecond)

	big := strings.Repeat("x", 200*1024)
	ws.writeText(t, `{"type":"item","streamId":"up","value":`+quoteJSON(big)+`}`)
	time.Sleep(20 * time.Millisecond)
	ws.writeText(t, `{"type":"item","streamId":"up","value":`+quoteJSON(strings.Repeat("y", 100*1024))+`}`)

	frame := ws.readJSONFrame(t, 3*time.Second)
	if frame["type"] != FrameError {
		t.Fatalf("超限应收到 error 帧：%v", frame)
	}
	failure, _ := frame["error"].(map[string]any)
	if failure["code"] != CodeUplinkOverflow {
		t.Fatalf("码应为 %s，实际 %v", CodeUplinkOverflow, failure)
	}

	// socket 还活着：另开一条流必须能收到 item。
	ws.writeText(t, openFrame("s2", "test/stream", ""))
	next := ws.readJSONFrame(t, 2*time.Second)
	if next["streamId"] != "s2" || next["type"] != FrameItem {
		t.Fatalf("超限后 socket 必须仍然可用：%v", next)
	}
}

// TestMuxItemAfterEndIsProtocolError 钉住"end 之后再 item → gateway/protocol"。
func TestMuxItemAfterEndIsProtocolError(t *testing.T) {
	stream := &uplinkStream{}
	stream.blockAfter = true
	opener := fakeOpener{streams: map[string]func() Stream{
		"test/uplink": func() Stream { return stream },
	}}
	_, ts, client := startWire(t, muxConfig(opener), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	ws.writeText(t, openFrame("up", "test/uplink", ""))
	time.Sleep(30 * time.Millisecond)
	ws.writeText(t, `{"type":"end","streamId":"up"}`)
	ws.writeText(t, `{"type":"item","streamId":"up","value":"late"}`)

	frame := ws.readJSONFrame(t, 2*time.Second)
	if frame["type"] != FrameError {
		t.Fatalf("end 之后的 item 应回 error 帧：%v", frame)
	}
	failure, _ := frame["error"].(map[string]any)
	if failure["code"] != CodeProtocol {
		t.Fatalf("码应为 %s，实际 %v", CodeProtocol, failure)
	}
}

// TestMuxCancelClosesStream 钉住 cancel 真的会 Close 流。
func TestMuxCancelClosesStream(t *testing.T) {
	stream := &syncStream{blockAfter: true}
	opener := fakeOpener{streams: map[string]func() Stream{
		"test/stream": func() Stream { return stream },
	}}
	_, ts, client := startWire(t, muxConfig(opener), nil)
	cookies := connectAndCookies(t, ts, client, InteractionsNone)
	ws, response := dialMux(t, ts, cookies)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("升级失败：%d", response.StatusCode)
	}
	defer ws.Close()

	ws.writeText(t, openFrame("s1", "test/stream", ""))
	time.Sleep(50 * time.Millisecond)
	ws.writeText(t, `{"type":"cancel","streamId":"s1"}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.mu.Lock()
		closed := stream.closed
		stream.mu.Unlock()
		if closed > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cancel 之后流没有被 Close")
}

// TestMuxHeartbeatTerminatesSilentPeer 钉住 2s×2 判死（这里把间隔压到 25ms）。
func TestMuxHeartbeatTerminatesSilentPeer(t *testing.T) {
	cfg := muxConfig(fakeOpener{})
	cfg.Heartbeat = 25 * time.Millisecond
	_, ts, client := startWire(t, cfg, nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	// 不回 pong、不发任何帧：3 个 tick 内必须被 terminate。
	ws.expectEOF(t, 1500*time.Millisecond)
}

// TestMuxHeartbeatKeepsResponsivePeer 钉住"有活动就不判死"。
func TestMuxHeartbeatKeepsResponsivePeer(t *testing.T) {
	cfg := muxConfig(fakeOpener{streams: map[string]func() Stream{
		"test/stream": func() Stream { return &syncStream{items: []StreamItem{textItem("alive")}} },
	}})
	cfg.Heartbeat = 25 * time.Millisecond
	_, ts, client := startWire(t, cfg, nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsNone)

	// 连续 8 个心跳周期都回 pong。
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		op, payload, err := ws.readFrame(t, 50*time.Millisecond)
		if err != nil {
			t.Fatalf("还在心跳期就被断开了：%v", err)
		}
		if op == opPing {
			ws.writeFrame(t, opPong, payload)
		}
	}

	// 仍然可用。
	ws.writeText(t, openFrame("s1", "test/stream", ""))
	frame := ws.readJSONFrame(t, 2*time.Second)
	if frame["streamId"] != "s1" {
		t.Fatalf("心跳之后连接必须仍然可用：%v", frame)
	}
}

// TestMuxPeerDisposeCloses1001 钉住 Peer 释放 → 1001 peer left。
func TestMuxPeerDisposeCloses1001(t *testing.T) {
	wire, ts, client := startWire(t, muxConfig(fakeOpener{}), nil)
	ws, cookies := dialAuthorized(t, ts, client, InteractionsNone)

	peerID := peerIDFromCookie(t, cookies)
	peer := wire.peer.Get(peerID)
	if peer == nil {
		t.Fatalf("服务端没有这个 Peer：%s", peerID)
	}
	wire.peer.Dispose(peer)

	if code := ws.readCloseCode(t, 2*time.Second); code != closePeerLeft {
		t.Fatalf("Peer 释放应关 1001，实际 %d", code)
	}
}

// TestMuxEventsReadyItem 钉住 `$events` 的首帧（EVIDENCE §3.2）。
func TestMuxEventsReadyItem(t *testing.T) {
	cfg := muxConfig(fakeOpener{})
	cfg.Home = "/home/ready"
	_, ts, client := startWire(t, cfg, nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsAnswer)

	ws.writeText(t, openFrame("ev", EndpointEvents, ""))
	frame := ws.readJSONFrame(t, 2*time.Second)
	if frame["type"] != FrameItem || frame["streamId"] != "ev" {
		t.Fatalf("`$events` 首帧应是 item：%v", frame)
	}
	value := frameValueOf(t, frame)
	if value["type"] != "ready" {
		t.Fatalf("首帧载荷必须是 ready：%v", value)
	}
	if value["clientId"] == "" || value["clientId"] == nil {
		t.Fatalf("ready 必须带 clientId：%v", value)
	}
	host, _ := value["host"].(map[string]any)
	if host["home"] != "/home/ready" {
		t.Fatalf("ready.host.home 不对：%v", value)
	}
}

// TestMuxEventsReleasesUplink 钉住"`$events` 打开即释放上行"（不计数、不报错）。
func TestMuxEventsReleasesUplink(t *testing.T) {
	_, ts, client := startWire(t, muxConfig(fakeOpener{streams: map[string]func() Stream{
		"test/stream": func() Stream { return &syncStream{items: []StreamItem{textItem("alive")}} },
	}}), nil)
	ws, _ := dialAuthorized(t, ts, client, InteractionsAnswer)

	ws.writeText(t, openFrame("ev", EndpointEvents, ""))
	ws.readJSONFrame(t, 2*time.Second) // ready

	// 远超 256 KiB 的上行：`$events` 释放了上行，所以不许出现 overflow。
	ws.writeText(t, `{"type":"item","streamId":"ev","value":`+quoteJSON(strings.Repeat("z", 300*1024))+`}`)
	ws.writeText(t, `{"type":"cancel","streamId":"ev"}`)

	// 没有 error 帧：下一条帧必须来自新开的流。
	ws.writeText(t, openFrame("s2", "test/stream", ""))
	frame := ws.readJSONFrame(t, 3*time.Second)
	if frame["type"] != FrameItem || frame["streamId"] != "s2" {
		t.Fatalf("`$events` 上行应被丢弃（不该有 overflow 帧）：%v", frame)
	}
}

// TestMuxEventsResultRequiresAnswerPeer 钉住 interactions 准入（EVIDENCE §3.2）。
func TestMuxEventsResultRequiresAnswerPeer(t *testing.T) {
	bindings := map[string]Binding{
		"$events/result": {
			Descriptor:    &Descriptor{Required: []string{"clientId", "eventId", "outcome"}},
			RequireAnswer: true,
			Handle: func(context.Context, map[string]json.RawMessage) (any, *Error) {
				return nil, MethodUnavailable("$events/result", "本宿主没有待应答事件")
			},
		},
	}
	_, ts, passive := startWire(t, testServerConfig(), bindings)
	connectAndCookies(t, ts, passive, InteractionsNone)
	status, payload := postEnvelope(t, ts, passive, "$events/result",
		envelopeBody("r-1", "$events/result", `{"clientId":"c","eventId":"e","outcome":{"kind":"next"}}`))
	envelope := decodeEnvelope(t, status, payload)
	failure := failureOf(t, envelope)
	if failure["code"] != CodeInteractionsForbidden {
		t.Fatalf("非 answer Peer 应回 %s，实际 %v", CodeInteractionsForbidden, failure)
	}

	_, ts2, answering := startWire(t, testServerConfig(), bindings)
	connectAndCookies(t, ts2, answering, InteractionsAnswer)
	status, payload = postEnvelope(t, ts2, answering, "$events/result",
		envelopeBody("r-2", "$events/result", `{"clientId":"c","eventId":"e","outcome":{"kind":"next"}}`))
	envelope = decodeEnvelope(t, status, payload)
	failure = failureOf(t, envelope)
	if failure["code"] != CodeMethodUnavailable {
		t.Fatalf("answer Peer 也应拿到 method-unavailable（能力未接线），实际 %v", failure)
	}
}

// peerIDFromCookie 从 v2 cookie 里解出 Peer id。
//
// 顺手证明 cookie 的 payload 就是 `{version,authority,issuedAt,expiresAt,id}`
// ——测试自己解析一遍，比"读源码相信它"强。
func peerIDFromCookie(t *testing.T, cookies []*http.Cookie) string {
	t.Helper()
	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Name, cookiePrefix) {
			continue
		}
		parts := strings.Split(cookie.Value, ".")
		if len(parts) != 3 {
			t.Fatalf("cookie 值不是三段：%q", cookie.Value)
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("cookie body 不是 base64url：%v", err)
		}
		var payload struct {
			Version   int    `json:"version"`
			Authority string `json:"authority"`
			ID        string `json:"id"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("cookie body 不是 JSON：%v", err)
		}
		if payload.Version != 2 {
			t.Fatalf("cookie version 应为 2，实际 %d", payload.Version)
		}
		if payload.ID == "" {
			t.Fatalf("cookie 缺 peer id")
		}
		return payload.ID
	}
	t.Fatalf("没有找到 %s 前缀的 cookie", cookiePrefix)
	return ""
}

// TestCloseCodeMapping 钉住四个关闭码的分派（EVIDENCE §3.3）。
//
// 1003/1008/1001 由帧与 Peer 生命周期用例覆盖；1011 只在"失败错误送不出去"
// 时出现（读侧异常），这条用例直接钉住映射函数，免得它被顺手改成 1008。
func TestCloseCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"协议错 1008", wsProtocolError{closeInvalidFrame, "非法帧"}, closeInvalidFrame},
		{"二进制 1003", wsProtocolError{closeBinaryUnsupported, "只收文本"}, closeBinaryUnsupported},
		{"读侧异常 1011", errors.New("写失败"), closeInternal},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code, _ := codeOf(testCase.err)
			if code != testCase.want {
				t.Fatalf("关闭码应为 %d，实际 %d", testCase.want, code)
			}
		})
	}
}
