package transport

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testDeflateOffer = "permessage-deflate; client_no_context_takeover; server_no_context_takeover"
const testDeflateResponse = testDeflateOffer

type compressionTestFrame struct {
	fin     bool
	rsv1    bool
	op      byte
	payload []byte
}

func startCompressionUpgrade(t *testing.T) (*httptest.Server, <-chan *WSConn, <-chan error) {
	t.Helper()
	conns := make(chan *WSConn, 1)
	errs := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err != nil {
			errs <- err
			return
		}
		conns <- conn
	}))
	t.Cleanup(srv.Close)
	return srv, conns, errs
}

func dialCompressionWS(t *testing.T, srv *httptest.Server, extension *string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	var req strings.Builder
	fmt.Fprintf(&req, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", addr, key)
	if extension != nil {
		fmt.Fprintf(&req, "Sec-WebSocket-Extensions: %s\r\n", *extension)
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols && resp.Header.Get("Sec-WebSocket-Accept") != acceptFor(key) {
		conn.Close()
		t.Fatalf("Sec-WebSocket-Accept 错误：%q", resp.Header.Get("Sec-WebSocket-Accept"))
	}
	return conn, br, resp
}

func serverCompressionConn(t *testing.T, conns <-chan *WSConn, errs <-chan error) *WSConn {
	t.Helper()
	select {
	case conn := <-conns:
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	case err := <-errs:
		t.Fatalf("Upgrade 失败：%v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Upgrade 超时")
	}
	return nil
}

func readCompressionFrame(t *testing.T, br *bufio.Reader) compressionTestFrame {
	t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		t.Fatal(err)
	}
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			t.Fatal(err)
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			t.Fatal(err)
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > 4<<20 {
		t.Fatalf("测试帧异常大：%d", n)
	}
	var mask [4]byte
	if head[1]&0x80 != 0 {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			t.Fatal(err)
		}
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatal(err)
	}
	if head[1]&0x80 != 0 {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return compressionTestFrame{fin: head[0]&0x80 != 0, rsv1: head[0]&0x40 != 0, op: head[0] & 0x0f, payload: payload}
}

func compressionTestClientFrame(fin, rsv1 bool, op byte, payload []byte) []byte {
	first := op
	if fin {
		first |= 0x80
	}
	if rsv1 {
		first |= 0x40
	}
	out := []byte{first}
	n := len(payload)
	switch {
	case n < 126:
		out = append(out, 0x80|byte(n))
	case n <= 0xffff:
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	default:
		out = append(out, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(out[len(out)-8:], uint64(n))
	}
	mask := [4]byte{1, 2, 3, 4}
	out = append(out, mask[:]...)
	for i, b := range payload {
		out = append(out, b^mask[i%4])
	}
	return out
}

func deflateCompressionTest(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), buf.Bytes()...)
	if len(out) < 4 || !bytes.Equal(out[len(out)-4:], []byte{0, 0, 0xff, 0xff}) {
		t.Fatalf("deflate 缺 sync-flush 尾：%x", out)
	}
	return out[:len(out)-4]
}

func inflateCompressionTest(t *testing.T, payload []byte) []byte {
	t.Helper()
	wire := append(append([]byte(nil), payload...), 0, 0, 0xff, 0xff, 1, 0, 0, 0xff, 0xff)
	r := flate.NewReader(bytes.NewReader(wire))
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		t.Fatalf("解压测试帧失败：%v", err)
	}
	return out
}

func writeAndReadServerFrame(t *testing.T, conn *WSConn, br *bufio.Reader, frame ServerFrame) compressionTestFrame {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- conn.WriteServerFrame(frame) }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("WriteServerFrame：%v", err)
		}
	default:
	}
	got := readCompressionFrame(t, br)
	if err := <-errCh; err != nil {
		t.Fatalf("WriteServerFrame：%v", err)
	}
	return got
}

func requireServerJSONFrame(t *testing.T, got compressionTestFrame, want ServerFrame, compressed bool) {
	t.Helper()
	if !got.fin || got.op != wsOpText {
		t.Fatalf("不是完整文本帧：%+v", got)
	}
	payload := got.payload
	if got.rsv1 {
		if !compressed {
			t.Fatal("未协商连接出现 RSV1")
		}
		payload = inflateCompressionTest(t, payload)
	}
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, wantRaw) {
		t.Fatalf("线上 JSON 失真：\n got %.300s\nwant %.300s", payload, wantRaw)
	}
}

func TestWSCompressionNegotiatesAndRoundTripsBothDirections(t *testing.T) {
	srv, conns, errs := startCompressionUpgrade(t)
	offer := testDeflateOffer
	client, br, resp := dialCompressionWS(t, srv, &offer)
	defer client.Close()
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Extensions") != testDeflateResponse {
		t.Fatalf("压缩协商响应错误：status=%d extensions=%q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Extensions"))
	}
	server := serverCompressionConn(t, conns, errs)

	large := strings.Repeat("history/reasoning/tool/approval/", 300)
	frame := ServerFrame{Seq: 9, Type: "assistant/message", Data: json.RawMessage(fmt.Sprintf(`{"text":%q}`, large)), Time: 1762000000000}
	got := writeAndReadServerFrame(t, server, br, frame)
	if !got.rsv1 {
		t.Fatal("大文本帧应使用 permessage-deflate（RSV1=1）")
	}
	requireServerJSONFrame(t, got, frame, true)

	raw := []byte(`{"t":"sub","topic":"session:` + strings.Repeat("A", 2048) + `","sinceSeq":7}`)
	if _, err := client.Write(compressionTestClientFrame(true, true, wsOpText, deflateCompressionTest(t, raw))); err != nil {
		t.Fatal(err)
	}
	decoded, err := server.ReadClientFrame()
	if err != nil {
		t.Fatalf("读压缩客户端帧：%v", err)
	}
	if decoded.T != "sub" || decoded.Topic != "session:"+strings.Repeat("A", 2048) || decoded.SinceSeq != 7 {
		t.Fatalf("压缩 sub 解码失真：%+v", decoded)
	}
}

func TestWSCompressionAbsentKeepsLegacyWireAndSupportsLargeFrame(t *testing.T) {
	srv, conns, errs := startCompressionUpgrade(t)
	client, br, resp := dialCompressionWS(t, srv, nil)
	defer client.Close()
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		t.Fatalf("无 offer 不得协商扩展：status=%d extensions=%q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Extensions"))
	}
	server := serverCompressionConn(t, conns, errs)

	large := strings.Repeat("x", 128<<10)
	frame := ServerFrame{Seq: 1, Type: "assistant/message", Data: json.RawMessage(fmt.Sprintf(`{"text":%q}`, large)), Time: 1}
	got := writeAndReadServerFrame(t, server, br, frame)
	if got.rsv1 {
		t.Fatal("未协商的大帧也不能设 RSV1")
	}
	requireServerJSONFrame(t, got, frame, false)
}

func TestWSCompressionGoldenReplayMatchesUncompressedAB(t *testing.T) {
	golden := loadGoldenContract(t)
	for _, compressed := range []bool{false, true} {
		for _, session := range golden.Sessions {
			t.Run(fmt.Sprintf("compressed=%v/session=%s", compressed, session.ID), func(t *testing.T) {
				srv, conns, errs := startCompressionUpgrade(t)
				var offer *string
				if compressed {
					value := testDeflateOffer
					offer = &value
				}
				client, br, resp := dialCompressionWS(t, srv, offer)
				defer client.Close()
				if compressed && resp.Header.Get("Sec-WebSocket-Extensions") != testDeflateResponse {
					t.Fatalf("没有协商压缩：%q", resp.Header.Get("Sec-WebSocket-Extensions"))
				}
				server := serverCompressionConn(t, conns, errs)
				frames := append(append([]ServerFrame(nil), session.Frames...), session.Controls...)
				for _, frame := range frames {
					got := writeAndReadServerFrame(t, server, br, frame)
					requireServerJSONFrame(t, got, frame, compressed)
				}
			})
		}
	}
}

func TestWSCompressionRejectsUnsupportedNegotiation(t *testing.T) {
	bad := []string{
		"",
		"x-webkit-deflate-frame",
		"permessage-deflate; unknown=1",
		"permessage-deflate; server_no_context_takeover=1",
		"permessage-deflate; server_max_window_bits=8",
		"permessage-deflate; client_no_context_takeover; client_no_context_takeover",
	}
	for _, offer := range bad {
		t.Run(fmt.Sprintf("%q", offer), func(t *testing.T) {
			srv, _, _ := startCompressionUpgrade(t)
			client, _, resp := dialCompressionWS(t, srv, &offer)
			defer client.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("非法扩展应 400，实际 %d", resp.StatusCode)
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			if !bytes.Contains(body, []byte(`"code":"bad_handshake"`)) {
				t.Fatalf("非法扩展错误信封不清晰：%s", body)
			}
		})
	}

	srv, conns, errs := startCompressionUpgrade(t)
	alternative := "x-unknown-extension, " + testDeflateOffer
	client, _, resp := dialCompressionWS(t, srv, &alternative)
	defer client.Close()
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Extensions") != testDeflateResponse {
		t.Fatalf("多 offer 应选择支持的 deflate：status=%d ext=%q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Extensions"))
	}
	_ = serverCompressionConn(t, conns, errs)
}

func TestWSCompressionRejectsBadFramesAndInflateBomb(t *testing.T) {
	valid := []byte(`{"t":"ping"}`)
	bomb := deflateCompressionTest(t, bytes.Repeat([]byte("A"), 2<<20))
	tests := []struct {
		name    string
		offer   bool
		frames  [][]byte
		wantErr string
	}{
		{name: "RSV1 without negotiation", frames: [][]byte{compressionTestClientFrame(true, true, wsOpText, valid)}, wantErr: "RSV1"},
		{name: "fragmented", offer: true, frames: [][]byte{compressionTestClientFrame(false, false, wsOpText, valid)}, wantErr: "分片"},
		{name: "compressed ping", offer: true, frames: [][]byte{compressionTestClientFrame(true, true, wsOpPing, []byte("x"))}, wantErr: "控制帧"},
		{name: "corrupt deflate", offer: true, frames: [][]byte{compressionTestClientFrame(true, true, wsOpText, []byte{1, 2, 3, 4, 5})}, wantErr: "解压"},
		{name: "inflate bomb", offer: true, frames: [][]byte{compressionTestClientFrame(true, true, wsOpText, bomb)}, wantErr: "解压后"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, conns, errs := startCompressionUpgrade(t)
			var offer *string
			if tc.offer {
				value := testDeflateOffer
				offer = &value
			}
			client, _, _ := dialCompressionWS(t, srv, offer)
			defer client.Close()
			server := serverCompressionConn(t, conns, errs)
			for _, frame := range tc.frames {
				if _, err := client.Write(frame); err != nil {
					t.Fatal(err)
				}
			}
			readResult := make(chan error, 1)
			go func() {
				_, err := server.ReadClientFrame()
				readResult <- err
			}()
			select {
			case err := <-readResult:
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("坏帧错误应含 %q，实际 %v", tc.wantErr, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("坏帧没有在 2 秒内被拒绝（应含 %q）", tc.wantErr)
			}
		})
	}
}

func TestWSCompressionOutboundLimit(t *testing.T) {
	conn := &WSConn{compressed: true} // 超限必须在触碰 net.Conn 之前返回。
	frame := ServerFrame{Seq: 1, Type: "assistant/message", Time: 1, Data: json.RawMessage(fmt.Sprintf(`{"text":%q}`, strings.Repeat("x", wsMaxPayload+1)))}
	if err := conn.WriteServerFrame(frame); err == nil || !strings.Contains(err.Error(), "解压后上限") {
		t.Fatalf("出站超过解压后上限必须 fail-loud，实际 %v", err)
	}
}

func TestWSCompressionPingPongCloseAndPongAreNotCompressed(t *testing.T) {
	srv, conns, errs := startCompressionUpgrade(t)
	offer := testDeflateOffer
	client, br, _ := dialCompressionWS(t, srv, &offer)
	defer client.Close()
	server := serverCompressionConn(t, conns, errs)

	readResult := make(chan error, 1)
	go func() {
		_, err := server.ReadClientFrame()
		readResult <- err
	}()
	if _, err := client.Write(compressionTestClientFrame(true, false, wsOpPong, []byte("ignored"))); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(compressionTestClientFrame(true, false, wsOpPing, []byte("hb"))); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(compressionTestClientFrame(true, true, wsOpText, deflateCompressionTest(t, []byte(`{"t":"ping"}`)))); err != nil {
		t.Fatal(err)
	}
	pong := readCompressionFrame(t, br)
	if pong.op != wsOpPong || pong.rsv1 || string(pong.payload) != "hb" {
		t.Fatalf("控制 pong 必须原样且不压缩：%+v", pong)
	}
	if err := <-readResult; err != nil {
		t.Fatalf("pong 后压缩文本未正确处理：%v", err)
	}

	go func() {
		_, err := server.ReadClientFrame()
		readResult <- err
	}()
	if _, err := client.Write(compressionTestClientFrame(true, false, wsOpClose, []byte{0x03, 0xe8})); err != nil {
		t.Fatal(err)
	}
	closed := readCompressionFrame(t, br)
	if closed.op != wsOpClose || closed.rsv1 {
		t.Fatalf("close 回帧错误：%+v", closed)
	}
	if err := <-readResult; err != io.EOF {
		t.Fatalf("close 应映射为 io.EOF，实际 %v", err)
	}
}
