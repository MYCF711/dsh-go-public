package officialwire

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// RFC6455 握手魔术串。
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsMaxFramePayload 是单条 mux 文本帧的硬上限。
//
// 它**不是**协议里的 streamInboxBytes（那是 256 KiB/流的**累计**上行配额）：
// 一条 item 帧本身可以很小，累计才会超。帧级上限只用来挡住"一个帧就吃掉全部内存"。
const wsMaxFramePayload = 1 << 20

// 官方关闭码（EVIDENCE §3.3）。只有这四个，别自己加：
//
//	1003 二进制帧（mux 只收文本）
//	1008 非法帧 / 同一个 streamId 重复 open
//	1011 失败帧送不出去
//	1001 Peer 作用域释放
const (
	closeBinaryUnsupported = 1003
	closeInvalidFrame      = 1008
	closeInternal          = 1011
	closePeerLeft          = 1001
)

// 帧操作码。
const (
	opText   byte = 0x1
	opBinary byte = 0x2
	opClose  byte = 0x8
	opPing   byte = 0x9
	opPong   byte = 0xA
)

// wsConn 是一条**服务端** WebSocket 连接：只收发 FIN=1 的单帧文本消息。
//
// 为什么自己写而不是复用 internal/transport/ws.go：那边的 Conn 接口按
// ClientFrame/ServerFrame 收口，暴露不出 close code、ping/pong 与原始文本帧；
// 而 mux 需要的恰恰是这三样（1003/1008/1011/1001、2s×2 心跳、七种帧）。
// 两者互不 import，两条 wire 各自演进。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu    sync.Mutex
	closed bool
}

// upgradeWS 完成握手。失败时自己写 HTTP 响应并返回错误。
func upgradeWS(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, errors.New("officialwire: WebSocket 升级必须是 GET")
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") {
		http.Error(w, "upgrade required", http.StatusBadRequest)
		return nil, errors.New("officialwire: 缺 Upgrade: websocket")
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "upgrade required", http.StatusBadRequest)
		return nil, errors.New("officialwire: 缺 Connection: Upgrade")
	}
	if version := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); version != "" && version != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, fmt.Errorf("officialwire: 不支持的 WebSocket 版本 %q", version)
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "bad handshake", http.StatusBadRequest)
		return nil, errors.New("officialwire: 缺 Sec-WebSocket-Key")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return nil, errors.New("officialwire: ResponseWriter 不支持 Hijack")
	}
	raw, brw, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("officialwire: 接管连接失败：%w", err)
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptFor(key) + "\r\n" +
		// 不协商 permessage-deflate：官方客户端 offer 了我们不接，它就发未压缩帧
		// （RFC7692 允许服务端拒绝 offer）。少一个压缩器就少一处"解压后超限"。
		"\r\n"
	if _, err := raw.Write([]byte(resp)); err != nil {
		raw.Close()
		return nil, fmt.Errorf("officialwire: 回 101 失败：%w", err)
	}
	return &wsConn{conn: raw, br: brw.Reader}, nil
}

func acceptFor(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// wsRead 是一条读出来的帧。
type wsRead struct {
	op      byte
	payload []byte
}

// readMessage 读一条消息。
//
// 只接受 FIN=1、客户端掩码、RSV 全 0 的帧；分片、置位 RSV、超限一律按
// 协议违规返回 closeError（调用方据此用 1003/1008 关连接）。
func (c *wsConn) readMessage() (wsRead, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return wsRead{}, err
	}
	if hdr[0]&0x80 == 0 {
		return wsRead{}, wsProtocolError{closeInvalidFrame, "不支持分片帧（FIN=0）"}
	}
	if hdr[0]&0x70 != 0 {
		return wsRead{}, wsProtocolError{closeInvalidFrame, "RSV 位被置位但没有协商扩展"}
	}
	op := hdr[0] & 0x0f
	length := uint64(hdr[1] & 0x7f)
	if hdr[1]&0x80 == 0 {
		return wsRead{}, wsProtocolError{closeInvalidFrame, "客户端帧缺掩码位"}
	}
	if op&0x08 != 0 && length > 125 {
		return wsRead{}, wsProtocolError{closeInvalidFrame, "控制帧正文超过 125 字节"}
	}
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsRead{}, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsRead{}, err
		}
		length = binary.BigEndian.Uint64(ext[:])
		if ext[0]&0x80 != 0 {
			return wsRead{}, wsProtocolError{closeInvalidFrame, "64 位长度非法"}
		}
	}
	if length > wsMaxFramePayload {
		return wsRead{}, wsProtocolError{closeInvalidFrame,
			fmt.Sprintf("单帧正文 %d 字节超过上限 %d", length, wsMaxFramePayload)}
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return wsRead{}, err
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return wsRead{}, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return wsRead{op: op, payload: payload}, nil
}

// wsProtocolError 是"要按某个关闭码收场"的错误。
type wsProtocolError struct {
	code   int
	reason string
}

func (e wsProtocolError) Error() string { return e.reason }

// codeOf 取错误对应的关闭码；不是协议错就是 1011。
func codeOf(err error) (int, string) {
	var pe wsProtocolError
	if errors.As(err, &pe) {
		return pe.code, pe.reason
	}
	return closeInternal, err.Error()
}

// writeText 写一条文本消息。
func (c *wsConn) writeText(payload []byte) error {
	return c.writeFrame(opText, payload)
}

// writePing 写一个 ping（2s 心跳用）。
func (c *wsConn) writePing(payload []byte) error {
	return c.writeFrame(opPing, payload)
}

// writeClose 写关闭帧。
func (c *wsConn) writeClose(code int, reason string) error {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	return c.writeFrame(opClose, payload)
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	if len(payload) > wsMaxFramePayload {
		return fmt.Errorf("officialwire: 要发的帧 %d 字节超上限", len(payload))
	}
	first := byte(0x80) | op
	var hdr []byte
	switch {
	case len(payload) < 126:
		hdr = []byte{first, byte(len(payload))}
	case len(payload) <= 0xffff:
		hdr = make([]byte, 4)
		hdr[0], hdr[1] = first, 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(len(payload)))
	default:
		hdr = make([]byte, 10)
		hdr[0], hdr[1] = first, 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(len(payload)))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if err := writeAll(c.conn, hdr); err != nil {
		return err
	}
	return writeAll(c.conn, payload)
}

// closeWith 尽力写一个关闭帧再关 TCP。重复调用是空操作。
func (c *wsConn) closeWith(code int, reason string) {
	c.wmu.Lock()
	if c.closed {
		c.wmu.Unlock()
		return
	}
	c.closed = true
	c.wmu.Unlock()

	// 关闭帧写在关闭标记之前会与并发写抢锁；这里直接裸写，失败也无所谓。
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	first := byte(0x80) | opClose
	hdr := []byte{first, byte(len(payload))}
	_ = writeAll(c.conn, hdr)
	_ = writeAll(c.conn, payload)
	_ = c.conn.Close()
}

// terminate 直接断 TCP（心跳判死走这条：EVIDENCE §3.3 的 terminate()）。
func (c *wsConn) terminate() {
	c.wmu.Lock()
	c.closed = true
	c.wmu.Unlock()
	_ = c.conn.Close()
}

func writeAll(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}
