package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// connectionEvent 是订阅内部连接状态变化。subscription 区分一次会话订阅，
// generation 区分这次订阅里的每一条 WS；两者都核对，旧连接迟到的 close
// 才不能把新连接误判成 down。
type connectionEventKind uint8

const (
	wsOpen connectionEventKind = iota
	wsDown
)

type connectionEvent struct {
	kind         connectionEventKind
	sid          string
	subscription uint64
	generation   uint64
	err          error
}

// subscriptionConn 是重连循环实际需要的 WS 能力；测试用它注入可控 read error。
type subscriptionConn interface {
	ReadText() (string, error)
	Close() error
}

type subscriptionWriteConn interface {
	subscriptionConn
	WriteText(string) error
}

// SubscribeWithEvents 与普通 Subscribe 共用同一个帧重连循环，额外暴露连接状态。
// --send 不需要状态面，仍走原 Subscribe，不会多一条必须消费的事件流。
func (c *client) SubscribeWithEvents(sid string, sinceSeq int) (<-chan ServerFrame, <-chan connectionEvent, func(), error) {
	events := make(chan connectionEvent, 64)
	frames, stop, err := c.subscribe(sid, sinceSeq, events)
	if err != nil {
		close(events)
		return nil, nil, nil, err
	}
	return frames, events, stop, nil
}

const disconnectedStatus = "连接已断开，正在重连…"

// runningElapsed 是本轮累计活跃时间。runningSince 为零时保留旧测试/旧调用
// 直接写 start 的兼容路径；down 时不读取当前墙钟，因此断线时长不会混入。
func (a *app) runningElapsed() time.Duration {
	d := a.runningAccum
	if a.busy && !a.connectionDown {
		since := a.runningSince
		if since.IsZero() && a.runningAccum == 0 {
			since = a.start
		}
		if !since.IsZero() {
			if delta := a.clock().Sub(since); delta > 0 {
				d += delta
			}
		}
	}
	if d < 0 {
		return 0
	}
	return d
}

// handleConnectionEvent 只改连接态和计时分段，不伪造 busy、审批或轮次终局。
func (a *app) handleConnectionEvent(ev connectionEvent) {
	if ev.sid == "" || ev.sid != a.sid || ev.subscription == 0 || ev.generation == 0 {
		return
	}
	if ev.subscription != a.connectionSubscription {
		// 新事件流的首个 open 可以绑定订阅；之后的跨订阅 open/down 都是旧事件。
		// A/B 切换还会换事件 channel，这里再做一次状态面防御。
		if a.connectionSubscription != 0 || ev.kind != wsOpen {
			return
		}
		a.connectionSubscription = ev.subscription
		a.connectionGeneration = 0
	}
	if ev.generation < a.connectionGeneration {
		return
	}

	switch ev.kind {
	case wsOpen:
		if ev.generation == a.connectionGeneration && !a.connectionDown {
			return // 重复 open 不重置当前活跃分段
		}
		a.connectionGeneration = ev.generation
		if a.connectionDown {
			a.connectionDown = false
			if a.busy {
				a.runningSince = a.clock()
			}
		}
		if a.status == disconnectedStatus {
			a.status = ""
		}
	case wsDown:
		// 同代重复 down（read error 后每次重连失败）只在第一次结算活跃段。
		if ev.generation != a.connectionGeneration || a.connectionDown {
			return
		}
		a.runningAccum = a.runningElapsed()
		a.runningSince = time.Time{}
		a.connectionDown = true
		a.status = disconnectedStatus
	}
}

// ── 可选 permessage-deflate ─────────────────────────────────────────────
//
// 旧 wsclient.go 保持原样：服务端不协商扩展时，这里返回其中的 *wsConn，
// 后续 ReadText/WriteText 完全走旧实现。只有明确回回合法 no-context
// permessage-deflate 的连接才使用 deflateWSConn。

const (
	tuiWSMaxPayload           = 1 << 20
	tuiWSCompressionThreshold = 512
	tuiWSDeflateOffer         = "permessage-deflate; client_no_context_takeover; server_no_context_takeover"
)

var (
	tuiWSDeflateTail     = []byte{0x00, 0x00, 0xff, 0xff}
	tuiWSDeflateReadTail = []byte{0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}
)

func dialWSOptionalDeflate(rawURL string, hdr http.Header) (subscriptionWriteConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("地址解析失败: %w", err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连不上 %s: %w", host, err)
	}
	fail := func(err error) (subscriptionWriteConn, error) {
		_ = conn.Close()
		return nil, err
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fail(err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\n", u.RequestURI(), u.Host)
	req.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", key)
	for k, values := range hdr {
		for _, value := range values {
			fmt.Fprintf(&req, "%s: %s\r\n", k, value)
		}
	}
	fmt.Fprintf(&req, "Sec-WebSocket-Extensions: %s\r\n\r\n", tuiWSDeflateOffer)
	if _, err := io.WriteString(conn, req.String()); err != nil {
		return fail(fmt.Errorf("握手写失败: %w", err))
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fail(fmt.Errorf("握手响应读失败: %w", err))
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fail(fmt.Errorf("握手被拒：HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), base64.StdEncoding.EncodeToString(digest[:]); got != want {
		return fail(fmt.Errorf("握手 Sec-WebSocket-Accept 错误：%q", got))
	}
	legacy := &wsConn{conn: conn, br: br, w: bufio.NewWriter(conn)}
	extension := strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Extensions"))
	if extension == "" {
		return legacy, nil
	}
	if !validTUIDeflateResponse(extension) {
		return fail(fmt.Errorf("服务端返回了不支持的 WebSocket 扩展或参数：%q", extension))
	}
	return &deflateWSConn{wsConn: legacy}, nil
}

func validTUIDeflateResponse(header string) bool {
	if len(strings.Split(header, ",")) != 1 {
		return false
	}
	parts := strings.Split(header, ";")
	if strings.TrimSpace(parts[0]) != "permessage-deflate" {
		return false
	}
	seen := map[string]bool{}
	for _, part := range parts[1:] {
		name := strings.TrimSpace(part)
		if name != "client_no_context_takeover" && name != "server_no_context_takeover" || seen[name] {
			return false
		}
		seen[name] = true
	}
	return seen["client_no_context_takeover"] && seen["server_no_context_takeover"]
}

type deflateWSConn struct {
	*wsConn
	wmu sync.Mutex
}

func (c *deflateWSConn) ReadText() (string, error) {
	for {
		var head [2]byte
		if _, err := io.ReadFull(c.br, head[:]); err != nil {
			return "", err
		}
		if head[0]&0x80 == 0 {
			return "", errors.New("收到分片帧（FIN=0）：TUI 只接受完整消息")
		}
		if head[0]&0x30 != 0 {
			return "", errors.New("收到 RSV2/RSV3 置位帧")
		}
		op := head[0] & 0x0f
		compressed := head[0]&0x40 != 0
		control := op&0x08 != 0
		if control && (compressed || head[1]&0x7f > 125) {
			return "", errors.New("控制帧不得压缩且正文不得超过 125 字节")
		}
		if compressed && op != 0x1 {
			return "", errors.New("RSV1 只允许出现在文本消息")
		}
		if head[1]&0x80 != 0 {
			return "", errors.New("服务端帧不得带掩码")
		}
		n := uint64(head[1] & 0x7f)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return "", err
			}
			n = uint64(binary.BigEndian.Uint16(ext[:]))
			if n < 126 {
				return "", errors.New("帧长度使用了非最短编码")
			}
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return "", err
			}
			n = binary.BigEndian.Uint64(ext[:])
			if ext[0]&0x80 != 0 || n <= 0xffff {
				return "", errors.New("64 位帧长度非法或非最短编码")
			}
		}
		if n > tuiWSMaxPayload {
			return "", fmt.Errorf("帧正文 %d 字节，超过上限 %d", n, tuiWSMaxPayload)
		}
		payload := make([]byte, int(n))
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return "", err
		}
		if compressed {
			inflated, err := inflateTUIMessage(payload)
			if err != nil {
				return "", err
			}
			payload = inflated
		}

		switch op {
		case 0x1:
			return string(payload), nil
		case 0x8:
			_ = c.writeFrame(false, 0x8, payload)
			return "", io.EOF
		case 0x9:
			if err := c.writeFrame(false, 0xA, payload); err != nil {
				return "", err
			}
		case 0xA:
			// pong 只是控制面响应，继续等下一条文本。
		default:
			return "", fmt.Errorf("不支持的控制/数据帧 opcode=%d", op)
		}
	}
}

func (c *deflateWSConn) WriteText(text string) error {
	payload := []byte(text)
	if len(payload) > tuiWSMaxPayload {
		return fmt.Errorf("文本消息 %d 字节，超过上限 %d", len(payload), tuiWSMaxPayload)
	}
	if len(payload) >= tuiWSCompressionThreshold {
		compressed, err := deflateTUIMessage(payload)
		if err != nil {
			return err
		}
		if len(compressed) < len(payload) {
			return c.writeFrame(true, 0x1, compressed)
		}
	}
	return c.writeFrame(false, 0x1, payload)
}

func (c *deflateWSConn) writeFrame(compressed bool, op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if !compressed {
		return c.wsConn.writeFrame(op, payload)
	}
	if op != 0x1 {
		return errors.New("只有文本消息可以压缩")
	}
	if len(payload) > tuiWSMaxPayload {
		return fmt.Errorf("帧正文超过 %d 字节", tuiWSMaxPayload)
	}

	head := []byte{0xC0 | op}
	n := len(payload)
	switch {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n <= 0xffff:
		head = append(head, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(head[len(head)-2:], uint16(n))
	default:
		head = append(head, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[len(head)-8:], uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	head = append(head, mask[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.w.Write(head); err != nil {
		return err
	}
	if _, err := c.w.Write(masked); err != nil {
		return err
	}
	return c.w.Flush()
}

func inflateTUIMessage(payload []byte) ([]byte, error) {
	reader := flate.NewReader(io.MultiReader(bytes.NewReader(payload), bytes.NewReader(tuiWSDeflateReadTail)))
	out, readErr := io.ReadAll(io.LimitReader(reader, tuiWSMaxPayload+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("解压 permessage-deflate 失败：%w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("关闭解压器失败：%w", closeErr)
	}
	if len(out) > tuiWSMaxPayload {
		return nil, fmt.Errorf("解压后正文超过 %d 字节上限", tuiWSMaxPayload)
	}
	return out, nil
}

func deflateTUIMessage(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(payload); err != nil {
		return nil, err
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) < 4 || !bytes.Equal(out[len(out)-4:], tuiWSDeflateTail) {
		return nil, errors.New("deflate sync flush 结尾错误")
	}
	return append([]byte(nil), out[:len(out)-4]...), nil
}
