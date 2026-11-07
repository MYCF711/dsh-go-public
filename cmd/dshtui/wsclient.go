package main

// 一个够用的 WebSocket **客户端**：RFC 6455，只做我们真正需要的那部分。
//
// 为什么自己写而不是引库：我们只用得上「握手 + 读文本帧 + 写文本帧 + 应答 ping」。
// 引一个通用库会带进压缩、扩展协商、子协议、重连……而这些我们都不需要，
// 二进制体积却是实打实的（门禁 ≤25 MiB）。
// 不做的部分写在这里，免得后来人以为它支持：**不支持分片重组、扩展、压缩、子协议**。
// 服务端（本仓 internal/transport）发的帧都是单个 FIN 文本帧，够用。

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wsConn 是一条已握手的连接。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	w    *bufio.Writer
}

// dialWS 连到 ws://host/path（http URL 也接受，自动换成 ws 方案）。
func dialWS(rawURL string, hdr http.Header) (*wsConn, error) {
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

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	path := u.RequestURI()
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	for k, vs := range hdr {
		for _, v := range vs {
			fmt.Fprintf(&req, "%s: %s\r\n", k, v)
		}
	}
	req.WriteString("\r\n")

	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		return nil, fmt.Errorf("握手写失败: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("握手响应读失败: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		conn.Close()
		// 这一条最容易误判成"服务端坏了"，其实是认证/路径不对。把状态码带出去。
		return nil, fmt.Errorf("握手被拒：HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return &wsConn{conn: conn, br: br, w: bufio.NewWriter(conn)}, nil
}

// ReadText 读下一帧文本。控制帧（ping/pong/close）在内部处理：ping 立刻回 pong。
func (c *wsConn) ReadText() (string, error) {
	for {
		var head [2]byte
		if _, err := io.ReadFull(c.br, head[:]); err != nil {
			return "", err
		}
		opcode := head[0] & 0x0f
		masked := head[1]&0x80 != 0
		n := uint64(head[1] & 0x7f)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return "", err
			}
			n = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return "", err
			}
			n = binary.BigEndian.Uint64(ext[:])
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return "", err
			}
		}
		if n > 32<<20 {
			return "", errors.New("帧太大（>32MB），拒绝")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return "", err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}

		switch opcode {
		case 0x1: // text
			return string(payload), nil
		case 0x8: // close
			return "", io.EOF
		case 0x9: // ping → 必须回 pong，否则服务端会判死
			if err := c.writeFrame(0xA, payload); err != nil {
				return "", err
			}
			continue
		case 0xA: // pong
			continue
		default:
			// 二进制/分片：本客户端不实现。**明确报错**而不是静默丢弃——
			// 静默丢会让上层以为"服务端没发东西"。
			return "", fmt.Errorf("不支持的控制/数据帧 opcode=%d", opcode)
		}
	}
}

// WriteText 发一帧文本。
func (c *wsConn) WriteText(s string) error { return c.writeFrame(0x1, []byte(s)) }

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	var head []byte
	head = append(head, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n < 65536:
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
	for i := 0; i < n; i++ {
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

func (c *wsConn) Close() error { return c.conn.Close() }
