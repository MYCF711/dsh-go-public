package transport

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func acceptFor(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// dialWS 用裸 TCP 做一次客户端握手，返回连接和读缓冲。
func dialWS(t *testing.T, addr, key string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", addr, key)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("握手要 101，拿到 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptFor(key) {
		t.Fatalf("Accept 算错了: %s（期望 %s）", got, acceptFor(key))
	}
	return c, br
}

// 客户端帧：必须带 mask。
func writeMaskedText(t *testing.T, w net.Conn, payload string) {
	t.Helper()
	mask := [4]byte{1, 2, 3, 4}
	b := []byte(payload)
	hdr := []byte{0x81}
	if len(b) < 126 {
		hdr = append(hdr, byte(0x80|len(b)))
	} else {
		hdr = append(hdr, 0x80|126, byte(len(b)>>8), byte(len(b)))
	}
	hdr = append(hdr, mask[:]...)
	for i := range b {
		b[i] ^= mask[i%4]
	}
	if _, err := w.Write(append(hdr, b...)); err != nil {
		t.Fatal(err)
	}
}

// 服务端帧：不许有 mask。
func readServerText(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	h1, err := br.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != 0x81 {
		t.Fatalf("首字节要 0x81（FIN+text），拿到 %#x", h1)
	}
	h2, err := br.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	if h2&0x80 != 0 {
		t.Fatal("服务端发出的帧不许带 mask")
	}
	n := int(h2 & 0x7f)
	if n == 126 {
		var ext [2]byte
		if _, err := br.Read(ext[:]); err != nil {
			t.Fatal(err)
		}
		n = int(binary.BigEndian.Uint16(ext[:]))
	}
	buf := make([]byte, n)
	if _, err := br.Read(buf); err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

// 端到端：握手 → 订阅 → 收到推送 → 发坏帧 → 断开。
func TestWSRoundTrip(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		go ServeConn(c, hub)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	conn, br := dialWS(t, addr, "dGhlIHNhbXBsZSBub25jZQ==")
	defer conn.Close()

	writeMaskedText(t, conn, `{"t":"sub","topic":"session:x","sinceSeq":0}`)
	// 给订阅一点时间生效（ServeConn 是另一个 goroutine）
	time.Sleep(200 * time.Millisecond)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	hub.Publish("session:x", ServerFrame{Seq: 1, Type: "user/message"})
	body := readServerText(t, br)
	if !strings.Contains(body, `"seq":1`) {
		t.Fatalf("推送内容不对: %s", body)
	}
}

// 握手缺 key 必须 400，不许"凑合过"。
//
// 状态码和正文都由 Upgrade 自己写（统一错误信封），所以这里**不能再补一次
// WriteHeader**——补了就是 "superfluous response.WriteHeader"（日志噪音，
// 而且说明调用方以为还有第二个写出口）。
func TestUpgradeRejectsBadHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = Upgrade(w, r)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 Sec-WebSocket-Key 要 400，拿到 %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"error"`) {
		t.Fatalf("握手失败也要走统一错误信封，实际正文：%s", body)
	}
}
