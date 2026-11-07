package main

// OSC 11 终端背景查询与颜色解析。
//
// 协议按上游 dsh-TUI@1bd7cb5 的真实实现移植：
//   - ink/terminal-querier.ts：oscColor(11) 发 `OSC 11 ; ?`，随后以 DA1 (`CSI c`)
//     作为“不支持查询”的有序哨兵；ThemeProvider 再给整个探测加 400ms 上限。
//   - ink/termio/osc.ts：parseOscColor 接受 #RRGGBB 与 rgb:R/G/B；rgb 每通道
//     1–4 位十六进制，按 XParseColor 的 value/(16^N-1) 规则缩放到 8 位。
//   - components/design-system/ThemeProvider.tsx：Rec.601 亮度严格 >140 才选 light。
//
// 查询只在真实交互终端启动时进行。stdin 的泵始终只有一个：探测和后续按键解码
// 共用 byteSource，探测期间读到的普通按键经 prefixSource 回放，不会抢输入或把
// 迟到的 OSC 响应当作用户文字。

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	osc11Query = "\x1b]11;?\x07"
	da1Query   = "\x1b[c"

	// osc11Timeout 与上游 DETECT_TIMEOUT_MS 一致。正常本地往返约 10ms，
	// 400ms 只限制不响应终端造成的启动停滞。
	osc11Timeout  = 400 * time.Millisecond
	maxOSC11Frame = 4096
)

// osc11Set 构造标准的动态背景色设置序列。启动路径只查询、不调用它：
// 读取用户终端背景不应顺手把终端颜色改掉。
func osc11Set(spec string) (string, error) {
	if _, ok := parseOSCColor(spec); !ok {
		return "", fmt.Errorf("非法 OSC 11 颜色 %q", spec)
	}
	return "\x1b]11;" + spec + "\x07", nil
}

// parseOSCColor 是上游 parseOscColor 的严格等价形式。这里刻意不 TrimSpace：
// 颜色是协议字段，带空格、换行或多余字符的响应都应走明确的非法回退。
func parseOSCColor(spec string) (color, bool) {
	if len(spec) == 7 && spec[0] == '#' {
		r, okR := parseOSCHexByte(spec[1:3])
		g, okG := parseOSCHexByte(spec[3:5])
		b, okB := parseOSCHexByte(spec[5:7])
		if okR && okG && okB {
			return color{r: r, g: g, b: b, set: true}, true
		}
		return color{}, false
	}
	if len(spec) < 8 || !strings.EqualFold(spec[:4], "rgb:") {
		return color{}, false
	}
	parts := strings.Split(spec[4:], "/")
	if len(parts) != 3 {
		return color{}, false
	}
	values := [3]uint8{}
	for i, part := range parts {
		if len(part) < 1 || len(part) > 4 || !isHex(part) {
			return color{}, false
		}
		v, err := strconv.ParseUint(part, 16, 16)
		if err != nil {
			return color{}, false
		}
		max := uint64(1)<<(4*len(part)) - 1
		// Math.round(value/max*255)，全部用正整数运算，避免浮点边界分歧。
		values[i] = uint8((v*255 + max/2) / max)
	}
	return color{r: values[0], g: values[1], b: values[2], set: true}, true
}

// parseOSCHexByte 只解析 #RRGGBB 中的一个两位十六进制通道。
func parseOSCHexByte(s string) (uint8, bool) {
	if len(s) != 2 || !isHex(s) {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 8)
	return uint8(v), err == nil
}

func isHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// parseOSC11Response 解析完整的 `ESC ] 11 ; color BEL|ST` 响应。
func parseOSC11Response(raw string) (color, bool) {
	if !strings.HasPrefix(raw, "\x1b]11;") {
		return color{}, false
	}
	var spec string
	switch {
	case strings.HasSuffix(raw, "\x07"):
		spec = raw[len("\x1b]11;") : len(raw)-1]
	case strings.HasSuffix(raw, "\x1b\\"):
		spec = raw[len("\x1b]11;") : len(raw)-2]
	default:
		return color{}, false
	}
	return parseOSCColor(spec)
}

// themeForBackground 用与上游完全相同的 Rec.601 系数和严格阈值。
// 整数形式 299R+587G+114B > 140000 等价于 0.299R+0.587G+0.114B > 140。
func themeForBackground(c color) string {
	luma := 299*int(c.r) + 587*int(c.g) + 114*int(c.b)
	if luma > 140000 {
		return "light"
	}
	return "dark"
}

type oscProbeStatus string

const (
	oscProbeOK          oscProbeStatus = "ok"
	oscProbeExplicit    oscProbeStatus = "explicit-theme"
	oscProbeNonTTY      oscProbeStatus = "non-tty"
	oscProbeTimeout     oscProbeStatus = "timeout"
	oscProbeInvalid     oscProbeStatus = "invalid-response"
	oscProbeEOF         oscProbeStatus = "eof"
	oscProbeUnsupported oscProbeStatus = "unsupported"
	oscProbeWriteError  oscProbeStatus = "write-error"
)

// oscProbeResult 的 replay 是探测期间读到、但不属于终端应答的原始字节。
// 状态永远显式给出：调用方只在 OK 时采用颜色，其余状态全部保留回退主题。
type oscProbeResult struct {
	color  color
	status oscProbeStatus
	replay []byte
}

// startupThemeName 把显式主题优先级与自动探测放在同一个可测入口。
// explicit=true 时连查询字节都不发；所有非 OK 分支都返回调用方给的 fallback。
func startupThemeName(explicit bool, fallback string, src byteSource, out io.Writer, interactive bool, timeout time.Duration) (string, oscProbeResult) {
	if explicit {
		return fallback, oscProbeResult{status: oscProbeExplicit}
	}
	result := probeOSC11(src, out, interactive, timeout)
	if result.status == oscProbeOK {
		return themeForBackground(result.color), result
	}
	return fallback, result
}

// prefixSource 先把探测期保存的普通输入交还解码器，再继续读同一个源。
type prefixSource struct {
	prefix []byte
	src    byteSource
}

func (s *prefixSource) next(afterEsc bool) (byte, bool, bool) {
	if len(s.prefix) > 0 {
		c := s.prefix[0]
		s.prefix = s.prefix[1:]
		return c, false, true
	}
	return s.src.next(afterEsc)
}

// probeOSC11 发一轮 OSC 11 + DA1，并在 timeout 内等待有序响应。
// DA1 先到表示终端不支持 OSC 11；其它 CSI/OSC 不当键盘输入，也不冒充背景色。
func probeOSC11(src byteSource, out io.Writer, interactive bool, timeout time.Duration) oscProbeResult {
	if !interactive {
		return oscProbeResult{status: oscProbeNonTTY}
	}
	if _, err := io.WriteString(out, osc11Query+da1Query); err != nil {
		return oscProbeResult{status: oscProbeWriteError}
	}
	if timeout <= 0 {
		timeout = osc11Timeout
	}
	deadline := time.Now().Add(timeout)

	const (
		probeGround = iota
		probeEscape
		probeCSI
		probeOSC
	)
	state := probeGround
	var frame []byte
	oscEscape := false
	oscMalformed := false
	result := oscProbeResult{}
	finish := func(status oscProbeStatus) oscProbeResult {
		// 半截控制序列也交还同一个解码器：迟到的剩余字节仍有前缀可识别，
		// 不会退化成 `11;rgb:...` 这样的普通文字。
		result.replay = append(result.replay, frame...)
		result.status = status
		return result
	}

	for {
		if time.Now().After(deadline) {
			return finish(oscProbeTimeout)
		}
		c, timedOut, ok := src.next(true)
		if timedOut {
			if time.Now().After(deadline) {
				return finish(oscProbeTimeout)
			}
			continue
		}
		if !ok {
			return finish(oscProbeEOF)
		}

		switch state {
		case probeGround:
			if c == 0x1b {
				frame = append(frame[:0], c)
				state = probeEscape
			} else {
				result.replay = append(result.replay, c)
			}

		case probeEscape:
			frame = append(frame, c)
			switch c {
			case ']':
				state = probeOSC
				oscEscape = false
				oscMalformed = false
			case '[', 'O':
				state = probeCSI
			case 0x1b:
				// 新的 ESC 取代孤立 ESC；孤立 ESC 仍是普通按键输入。
				result.replay = append(result.replay, frame[:len(frame)-1]...)
				frame = frame[len(frame)-1:]
				state = probeEscape
			default:
				result.replay = append(result.replay, frame...)
				frame = frame[:0]
				state = probeGround
			}

		case probeCSI:
			frame = append(frame, c)
			if c >= 0x40 && c <= 0x7e {
				if len(frame) >= 3 && frame[0] == 0x1b && frame[1] == '[' && c == 'c' {
					return finish(oscProbeUnsupported)
				}
				result.replay = append(result.replay, frame...)
				frame = frame[:0]
				state = probeGround
			} else if len(frame) > 64 {
				result.replay = append(result.replay, frame...)
				frame = frame[:0]
				state = probeGround
			}

		case probeOSC:
			frame = append(frame, c)
			complete := false
			if oscEscape {
				oscEscape = false
				if c == '\\' {
					complete = true
				} else {
					oscMalformed = true
				}
			} else {
				switch c {
				case 0x07:
					complete = true
				case 0x1b:
					oscEscape = true
				}
			}
			if len(frame) > maxOSC11Frame {
				return finish(oscProbeInvalid)
			}
			if !complete {
				continue
			}
			if bytes.HasPrefix(frame, []byte("\x1b]11;")) {
				parsed, valid := parseOSC11Response(string(frame))
				if oscMalformed || !valid {
					return finish(oscProbeInvalid)
				}
				result.color = parsed
				result.status = oscProbeOK
				return result
			}
			// 其它 OSC 是终端控制响应，不是用户文字；吞掉后继续等 OSC 11/DA1。
			frame = frame[:0]
			state = probeGround
		}
	}
}

// isTerminalFile 只认字符设备。管道、重定向文件和脚本输出都不能收到探测控制码。
func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
