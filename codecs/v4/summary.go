package v4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ReadSummary 从日志尾部往回找第一条助手/用户消息，返回截断到 maxRunes 的一句话。
//
// 有界：最多解 maxBytes 字节的帧（从最后一帧往前数）。找不到就返回空串和 nil——
// 没有消息的会话是合法的（header + 控制事件），那不是错误。
//
// 为什么要有它：列会话只要一句话，全量解码一份大会话要几十毫秒（183 个会话实测 4.37 秒）。
func ReadSummary(path string, maxBytes, maxRunes int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读会话文件失败: %w", err)
	}
	frames, _, err := splitFrames(b)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}

	budget := maxBytes
	var assistant, user string
	for i := len(frames) - 1; i >= 0 && budget > 0; i-- {
		f := frames[i]
		budget -= f.length // 先按压缩长度扣，解完再按真实长度扣一次
		raw, derr := decodeOneFrame(b[f.offset : f.offset+f.length])
		if derr != nil {
			continue
		}
		budget -= len(raw) // 真实开销是解压后的长度，不是压缩长度
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var e struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal([]byte(line), &e) != nil {
				continue
			}
			// 注意：要在**这一条事件的 data** 里找，不是在整帧 raw 里找——
			// 一帧装很多行 JSON，拿它整体去 Unmarshal 必然失败，于是永远找不到文字。
			text := firstText(e.Data)
			if strings.TrimSpace(text) == "" {
				continue
			}
			switch e.Type {
			case "assistant/message":
				if assistant == "" {
					assistant = text
				}
			case "user/message":
				if user == "" {
					user = text
				}
			}
		}
		if assistant != "" {
			break
		}
	}
	out := assistant
	if out == "" {
		out = user
	}
	return ClipOneLine(out, maxRunes), nil
}

// ClipOneLine 把多行压成一行并截到 maxRunes 个字符。
func ClipOneLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// firstText 从事件 data 里挖出第一段文字。
//
// 不去猜具体层级：v4 的消息体形状有好几种（message.content 块数组、
// content 直接是块数组、或更老的字符串），**递归找第一个 "text" 字符串**最省事，
// 也不会因为上游多加一层就整个失效。
func firstText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return walkText(v)
}

func walkText(v any) string {
	switch t := v.(type) {
	case string:
		return ""
	case []any:
		for _, e := range t {
			if s := walkText(e); s != "" {
				return s
			}
		}
	case map[string]any:
		if s, ok := t["text"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		for _, k := range []string{"message", "content", "blocks"} {
			if e, ok := t[k]; ok {
				if s := walkText(e); s != "" {
					return s
				}
			}
		}
		for _, e := range t {
			if s := walkText(e); s != "" {
				return s
			}
		}
	}
	return ""
}

// decodeOneFrame 解一个 zstd 帧。
//
// 这里不用 decodeFrame：它的切片假设只对第一帧成立（ReadHeader 只解第一帧所以一直没事）。
// 一帧一个流式解码器虽然多一点开销，但**只解最后一两帧**，代价可以接受。
func decodeOneFrame(b []byte) ([]byte, error) {
	r, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
