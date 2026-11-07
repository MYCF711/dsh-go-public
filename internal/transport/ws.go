package transport

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// RFC6455 里写死的握手魔术串：accept = base64(sha1(key + 这一串))。
const wsAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsMaxPayload 同时限制线上帧正文与解压后的消息。压缩只改变编码，不放大消息上限。
const wsMaxPayload = 1 << 20

const (
	wsCompressionThreshold = 512
	wsDeflateOfferResponse = "permessage-deflate; client_no_context_takeover; server_no_context_takeover"
)

var wsDeflateTail = []byte{0x00, 0x00, 0xff, 0xff}

// 读取时补一个 BFINAL 空块，让 compress/flate 把正常 sync-flush 结束
// 与真正截断区分开；这些字节只用于解压，绝不出现在线上帧里。
var wsDeflateReadTail = []byte{0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}

const (
	wsOpText  byte = 0x1
	wsOpClose byte = 0x8
	wsOpPing  byte = 0x9
	wsOpPong  byte = 0xA
)

// WSConn 是 Conn 的 WebSocket 实现：单条完整文本消息、可选 RFC7692 压缩。
// 每条消息独立 deflate（双向 no_context_takeover），不保留跨消息字典。
//
// 读走 br（Hijack 时 http 层可能已经缓冲了几个字节），写走 conn 并由 wmu 串行化。
type WSConn struct {
	conn       net.Conn
	br         *bufio.Reader
	compressed bool
	wmu        sync.Mutex
}

var _ Conn = (*WSConn)(nil)

// Upgrade 完成 RFC6455 握手，并可选协商 RFC7692 permessage-deflate。
// 没有 Sec-WebSocket-Extensions 的旧客户端走原始路径，响应也不带扩展头。
// 有扩展 offer 但没有可接受的 permessage-deflate 时明确 400，不静默误判协议能力。
func Upgrade(w http.ResponseWriter, r *http.Request) (*WSConn, error) {
	if r == nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_handshake", "WebSocket 握手需要请求对象"))
		return nil, errors.New("升级失败：请求为 nil")
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_handshake", "缺少 Upgrade: websocket"))
		return nil, errors.New("升级失败：请求头缺少 Upgrade: websocket")
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_handshake", "缺少 Connection: Upgrade"))
		return nil, errors.New("升级失败：请求头 Connection 不含 upgrade")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_handshake", "缺少 Sec-WebSocket-Key"))
		return nil, errors.New("升级失败：请求头缺少 Sec-WebSocket-Key")
	}
	compressed, err := negotiateDeflate(r.Header)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_handshake", err.Error()))
		return nil, fmt.Errorf("升级失败：%w", err)
	}

	h, ok := w.(http.Hijacker)
	if !ok {
		writeJSON(w, http.StatusInternalServerError,
			errorBodyFor("internal", "这个 http.ResponseWriter 不支持 Hijack，无法接管连接"))
		return nil, errors.New("升级失败：http.ResponseWriter 不支持 Hijack")
	}
	conn, brw, err := h.Hijack()
	if err != nil {
		return nil, fmt.Errorf("升级失败：接管连接出错：%w", err)
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n"
	if compressed {
		resp += "Sec-WebSocket-Extensions: " + wsDeflateOfferResponse + "\r\n"
	}
	resp += "\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("升级失败：回 101 时写不出去：%w", err)
	}
	return &WSConn{conn: conn, br: brw.Reader, compressed: compressed}, nil
}

// negotiateDeflate 从逗号分隔的多份 offer 中选第一份可接受的 deflate。
// 只实现 Go compress/flate 能精确保证的 15 位窗口、双向无上下文接管。
func negotiateDeflate(h http.Header) (bool, error) {
	values := h.Values("Sec-WebSocket-Extensions")
	if len(values) == 0 {
		return false, nil
	}
	for _, line := range values {
		for _, raw := range strings.Split(line, ",") {
			name, params, ok := parseExtensionOffer(raw)
			if !ok || name != "permessage-deflate" {
				continue
			}
			if validDeflateParams(params) {
				return true, nil
			}
		}
	}
	return false, errors.New("Sec-WebSocket-Extensions 没有受支持的 permessage-deflate offer；仅支持 15 位窗口与 no_context_takeover")
}

type extensionParams map[string]*string

func parseExtensionOffer(raw string) (string, extensionParams, bool) {
	parts := strings.Split(raw, ";")
	if len(parts) == 0 {
		return "", nil, false
	}
	name := strings.ToLower(strings.TrimSpace(parts[0]))
	if !isWSToken(name) {
		return "", nil, false
	}
	params := extensionParams{}
	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		pieces := strings.Split(part, "=")
		if len(pieces) > 2 {
			return "", nil, false
		}
		key := strings.ToLower(strings.TrimSpace(pieces[0]))
		if !isWSToken(key) {
			return "", nil, false
		}
		if _, duplicate := params[key]; duplicate {
			return "", nil, false
		}
		var value *string
		if len(pieces) == 2 {
			v := strings.TrimSpace(pieces[1])
			if !isWSToken(v) {
				return "", nil, false
			}
			value = &v
		}
		params[key] = value
	}
	return name, params, true
}

func validDeflateParams(params extensionParams) bool {
	for name, value := range params {
		switch name {
		case "client_no_context_takeover", "server_no_context_takeover":
			if value != nil {
				return false
			}
		case "client_max_window_bits":
			if value == nil { // offer 允许无值；响应不选择这个参数。
				continue
			}
			n, err := strconv.Atoi(*value)
			if err != nil || n < 8 || n > 15 {
				return false
			}
		case "server_max_window_bits":
			// 当前压缩器只承诺 15 位窗口，不能假装接受更小的服务端窗口。
			if value == nil || *value != "15" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isWSToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func acceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, wsAcceptGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ReadClientFrame 读一条完整文本消息并解成 ClientFrame。
// WS ping/pong/close 是控制帧，永不压缩；pong 忽略，ping 原样回 pong，close 回 close。
func (c *WSConn) ReadClientFrame() (ClientFrame, error) {
	for {
		frame, err := c.readFrame()
		if err != nil {
			return ClientFrame{}, err
		}
		switch frame.op {
		case wsOpText:
			var f ClientFrame
			if err := json.Unmarshal(frame.payload, &f); err != nil {
				return ClientFrame{}, fmt.Errorf("文本帧正文不是合法 JSON（%v）：ws 上收发的是 ClientFrame 的 JSON，请检查对端序列化", err)
			}
			return f, nil
		case wsOpClose:
			_ = c.writeFrame(false, wsOpClose, frame.payload)
			return ClientFrame{}, io.EOF
		case wsOpPing:
			if err := c.writeFrame(false, wsOpPong, frame.payload); err != nil {
				return ClientFrame{}, fmt.Errorf("收到 ping 但回 pong 失败：%w", err)
			}
			continue
		case wsOpPong:
			continue
		default:
			return ClientFrame{}, fmt.Errorf("不支持的 opcode 0x%x：本实现只处理 text(1)/close(8)/ping(9)/pong(10)", frame.op)
		}
	}
}

func (c *WSConn) WriteServerFrame(f ServerFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("ServerFrame 序列化失败：%w", err)
	}
	if len(b) > wsMaxPayload {
		return fmt.Errorf("要发的消息 %d 字节，超过解压后上限 %d 字节", len(b), wsMaxPayload)
	}
	if c.compressed && len(b) >= wsCompressionThreshold {
		compressed, err := deflateMessage(b)
		if err != nil {
			return fmt.Errorf("压缩 ServerFrame 失败：%w", err)
		}
		if len(compressed) < len(b) {
			return c.writeFrame(true, wsOpText, compressed)
		}
	}
	return c.writeFrame(false, wsOpText, b)
}

func (c *WSConn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

type wsFrame struct {
	op      byte
	payload []byte
}

// readFrame 只接受 FIN=1 的单帧消息。分片、RSV2/3、未协商 RSV1、
// 压缩控制帧与超过上限的线上/解压后正文全部 fail-loud。
func (c *WSConn) readFrame() (wsFrame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return wsFrame{}, err
	}
	if hdr[0]&0x80 == 0 {
		return wsFrame{}, errors.New("收到分片帧（FIN=0）：本实现只支持整帧消息，请让对端一次性发完")
	}
	if hdr[0]&0x30 != 0 {
		return wsFrame{}, errors.New("收到 RSV2/RSV3 置位帧：没有协商对应扩展")
	}
	op := hdr[0] & 0x0f
	rsv1 := hdr[0]&0x40 != 0
	control := op&0x08 != 0
	lengthCode := hdr[1] & 0x7f
	if control && (rsv1 || lengthCode > 125) {
		return wsFrame{}, errors.New("控制帧必须 FIN=1、正文不超过 125 字节且不得压缩")
	}
	if rsv1 && (!c.compressed || op != wsOpText) {
		return wsFrame{}, errors.New("收到非法 RSV1：只有协商 permessage-deflate 后的文本消息可压缩")
	}
	if hdr[1]&0x80 == 0 {
		return wsFrame{}, errors.New("客户端帧没有掩码位：RFC6455 要求客户端发往服务端的帧必须掩码")
	}

	n := uint64(lengthCode)
	switch lengthCode {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
		if n < 126 {
			return wsFrame{}, errors.New("帧长度使用了非最短编码")
		}
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		n = binary.BigEndian.Uint64(ext[:])
		if ext[0]&0x80 != 0 || n <= 0xffff {
			return wsFrame{}, errors.New("64 位帧长度非法或使用了非最短编码")
		}
	}
	if n > wsMaxPayload {
		return wsFrame{}, fmt.Errorf("帧正文 %d 字节，超过单帧上限 %d 字节", n, wsMaxPayload)
	}

	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return wsFrame{}, err
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return wsFrame{}, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	if rsv1 {
		inflated, err := inflateMessage(payload)
		if err != nil {
			return wsFrame{}, err
		}
		payload = inflated
	}
	return wsFrame{op: op, payload: payload}, nil
}

func inflateMessage(payload []byte) ([]byte, error) {
	r := flate.NewReader(io.MultiReader(bytes.NewReader(payload), bytes.NewReader(wsDeflateReadTail)))
	out, readErr := io.ReadAll(io.LimitReader(r, wsMaxPayload+1))
	closeErr := r.Close()
	if readErr != nil {
		return nil, fmt.Errorf("解压 permessage-deflate 消息失败：%w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("关闭 permessage-deflate 解压器失败：%w", closeErr)
	}
	if len(out) > wsMaxPayload {
		return nil, fmt.Errorf("解压后正文超过 %d 字节上限，拒绝继续分配", wsMaxPayload)
	}
	return out, nil
}

func deflateMessage(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(payload); err != nil {
		return nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) < len(wsDeflateTail) || !bytes.Equal(out[len(out)-len(wsDeflateTail):], wsDeflateTail) {
		return nil, errors.New("deflate sync flush 缺少 00 00 ff ff 结尾")
	}
	return append([]byte(nil), out[:len(out)-len(wsDeflateTail)]...), nil
}

// writeFrame 写一条 FIN=1、服务端不掩码的消息；仅文本数据可设 RSV1。
func (c *WSConn) writeFrame(compressed bool, op byte, payload []byte) error {
	n := len(payload)
	if n > wsMaxPayload {
		return fmt.Errorf("要发的帧 %d 字节，超过单帧上限 %d 字节", n, wsMaxPayload)
	}
	if op&0x08 != 0 && (compressed || n > 125) {
		return errors.New("控制帧不得压缩且正文不得超过 125 字节")
	}
	if compressed && op != wsOpText {
		return errors.New("只有文本消息可以设 RSV1")
	}

	first := byte(0x80) | op
	if compressed {
		first |= 0x40
	}
	var hdr []byte
	switch {
	case n < 126:
		hdr = []byte{first, byte(n)}
	case n <= 0xffff:
		hdr = make([]byte, 4)
		hdr[0], hdr[1] = first, 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
	default:
		hdr = make([]byte, 10)
		hdr[0], hdr[1] = first, 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := writeAll(c.conn, hdr); err != nil {
		return err
	}
	if n > 0 {
		if err := writeAll(c.conn, payload); err != nil {
			return err
		}
	}
	return nil
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
