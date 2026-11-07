package main

import (
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/internal/transport"
)

func TestTUIDeflateNegotiationAndRoundTrip(t *testing.T) {
	offers := make(chan string, 1)
	serverConns := make(chan *transport.WSConn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offers <- r.Header.Get("Sec-WebSocket-Extensions")
		conn, err := transport.Upgrade(w, r)
		if err == nil {
			serverConns <- conn
		}
	}))
	defer srv.Close()

	sid := strings.Repeat("A", 2048) // 让 sub 超过 512 字节，验证客户端→宿主压缩。
	client := &client{base: srv.URL}
	conn, err := client.dialSub(sid, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if offer := <-offers; !strings.Contains(offer, "permessage-deflate") || !strings.Contains(offer, "client_no_context_takeover") || !strings.Contains(offer, "server_no_context_takeover") {
		t.Fatalf("TUI 默认 offer 错误：%q", offer)
	}
	server := <-serverConns
	defer server.Close()
	sub, err := server.ReadClientFrame()
	if err != nil {
		t.Fatalf("宿主读 TUI sub：%v", err)
	}
	if sub.Topic != "session:"+sid || sub.SinceSeq != 7 {
		t.Fatalf("TUI sub 失真：%+v", sub)
	}

	frame := transport.ServerFrame{Seq: 8, Type: "assistant/message", Time: 1762000000000, Data: json.RawMessage(fmt.Sprintf(`{"text":%q}`, strings.Repeat("reasoning/text/tool/", 400)))}
	writeErr := make(chan error, 1)
	go func() { writeErr <- server.WriteServerFrame(frame) }()
	got, err := conn.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(frame)
	if got != string(want) {
		t.Fatalf("TUI 解压后帧失真：got %.200s want %.200s", got, want)
	}
}

func TestTUIDeflateFallbackUsesLegacyConn(t *testing.T) {
	serverConns := make(chan *transport.WSConn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("Sec-WebSocket-Extensions") // 模拟不认识扩展的旧宿主。
		conn, err := transport.Upgrade(w, r)
		if err == nil {
			serverConns <- conn
		}
	}))
	defer srv.Close()
	client := &client{base: srv.URL}
	conn, err := client.dialSub("legacy", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok := any(conn).(*wsConn); !ok {
		t.Fatalf("未协商必须返回旧 *wsConn，实际 %T", conn)
	}
	server := <-serverConns
	defer server.Close()
	if _, err := server.ReadClientFrame(); err != nil {
		t.Fatal(err)
	}
	go server.WriteServerFrame(transport.ServerFrame{Seq: 1, Type: "pong", Time: 1})
	if _, err := conn.ReadText(); err != nil {
		t.Fatalf("旧路径读文本失败：%v", err)
	}
}

func TestTUIDeflateRejectsUnexpectedExtension(t *testing.T) {
	srv := rawTUICompressionServer(t, "permessage-deflate; unknown=1", nil)
	client := &client{base: srv.URL}
	if _, err := client.dialSub("bad", 0); err == nil || !strings.Contains(err.Error(), "扩展") {
		t.Fatalf("非法服务端扩展必须 fail-loud，实际 %v", err)
	}
}

func TestTUIDeflateReadLimitAndFragmentReject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frame   []byte
		wantErr string
	}{
		{name: "inflate bomb", frame: tuiTestServerFrame(true, true, 0x1, tuiTestDeflate(t, bytes.Repeat([]byte("A"), 2<<20))), wantErr: "解压后"},
		{name: "fragment", frame: tuiTestServerFrame(false, false, 0x1, []byte(`{"seq":1}`)), wantErr: "分片"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := rawTUICompressionServer(t, "permessage-deflate; client_no_context_takeover; server_no_context_takeover", tc.frame)
			client := &client{base: srv.URL}
			conn, err := client.dialSub("bad-frame", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_, err = conn.ReadText()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("坏帧错误应含 %q，实际 %v", tc.wantErr, err)
			}
		})
	}
}

func TestTUIDeflateReconnectKeepsGenerationAndReset(t *testing.T) {
	var connectionID atomic.Int32
	offers := make(chan string, 2)
	attempts := make(chan int, 2)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	serverErrors := make(chan error, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offers <- r.Header.Get("Sec-WebSocket-Extensions")
		conn, err := transport.Upgrade(w, r)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		sub, err := conn.ReadClientFrame()
		if err != nil {
			serverErrors <- err
			return
		}
		attempts <- sub.SinceSeq
		id := connectionID.Add(1)
		large := json.RawMessage(fmt.Sprintf(`{"text":%q}`, strings.Repeat("reconnect/", 600)))
		if id == 1 {
			if err := conn.WriteServerFrame(transport.ServerFrame{Seq: 1, Type: "assistant/message", Data: large, Time: 1}); err != nil {
				serverErrors <- err
			}
			return // TCP 断开，触发真实 ReadText error 与自动重连。
		}
		reset := json.RawMessage(`{"journalEpoch":2,"lastSeq":1}`)
		if err := conn.WriteServerFrame(transport.ServerFrame{Type: "session/reset", Data: reset, Time: 2}); err != nil {
			serverErrors <- err
			return
		}
		if err := conn.WriteServerFrame(transport.ServerFrame{Seq: 2, Type: "assistant/message", Data: large, Time: 3}); err != nil {
			serverErrors <- err
			return
		}
		<-release
	}))
	defer srv.Close()

	client := &client{base: srv.URL, reconnectBackoff: func(int) time.Duration { return 0 }}
	frames, events, stop, err := client.SubscribeWithEvents("A", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for i, want := range []struct {
		seq      int
		typeName string
	}{{1, "assistant/message"}, {0, "session/reset"}, {2, "assistant/message"}} {
		select {
		case frame := <-frames:
			if frame.Seq != want.seq || frame.Type != want.typeName {
				t.Fatalf("第 %d 帧=%+v，want seq=%d type=%s", i, frame, want.seq, want.typeName)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("等第 %d 帧超时", i)
		}
	}
	for i, want := range []struct {
		kind       connectionEventKind
		generation uint64
	}{{wsOpen, 1}, {wsDown, 1}, {wsOpen, 2}} {
		select {
		case event := <-events:
			if event.kind != want.kind || event.generation != want.generation {
				t.Fatalf("第 %d 连接事件=%+v，want kind=%v generation=%d", i, event, want.kind, want.generation)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("等第 %d 连接事件超时", i)
		}
	}
	for i, want := range []int{0, 2} {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("第 %d 次订阅 sinceSeq=%d，want %d", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("订阅次数不足")
		}
	}
	for range 2 {
		if offer := <-offers; !strings.Contains(offer, "permessage-deflate") {
			t.Fatalf("重连未继续协商压缩：%q", offer)
		}
	}
	select {
	case err := <-serverErrors:
		t.Fatalf("测试宿主错误：%v", err)
	default:
	}
	stop()
	close(release)
}

func rawTUICompressionServer(t *testing.T, extension string, frame []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, brw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		digest := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + tuiTestWSGUID))
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		if extension != "" {
			fmt.Fprintf(brw, "Sec-WebSocket-Extensions: %s\r\n", extension)
		}
		fmt.Fprint(brw, "\r\n")
		if frame != nil {
			brw.Write(frame)
		}
		brw.Flush()
		time.Sleep(250 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tuiTestDeflate(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if len(out) < 4 {
		t.Fatal("短 deflate")
	}
	return append([]byte(nil), out[:len(out)-4]...)
}

func tuiTestServerFrame(fin, compressed bool, op byte, payload []byte) []byte {
	first := op
	if fin {
		first |= 0x80
	}
	if compressed {
		first |= 0x40
	}
	out := []byte{first}
	n := len(payload)
	switch {
	case n < 126:
		out = append(out, byte(n))
	case n <= 0xffff:
		out = append(out, 126, byte(n>>8), byte(n))
	default:
		out = append(out, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(out[len(out)-8:], uint64(n))
	}
	return append(out, payload...)
}

const tuiTestWSGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
