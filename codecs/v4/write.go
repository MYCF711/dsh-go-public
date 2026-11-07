package v4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// MarshalJSON 按 DSH 的**字段顺序**输出 header：type,version,id,createdAt,cwd,isSeeded,
// delegationDepth,parentSession,origin,agentPreset。
//
// 顺序本身不影响解析，但影响「同一份历史在磁盘上的字节」——golden 比对就靠这个。
func (h Header) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"type":`)
	b.WriteString(jstr(h.Type))
	b.WriteString(`,"version":`)
	b.WriteString(strconv.Itoa(h.Version))
	b.WriteString(`,"id":`)
	b.WriteString(jstr(h.ID))
	b.WriteString(`,"createdAt":`)
	b.WriteString(strconv.FormatInt(h.CreatedAt, 10))
	if h.Cwd != "" {
		b.WriteString(`,"cwd":`)
		b.WriteString(jstr(h.Cwd))
	}
	b.WriteString(`,"isSeeded":`)
	b.WriteString(strconv.FormatBool(h.IsSeeded))
	b.WriteString(`,"delegationDepth":`)
	b.WriteString(strconv.Itoa(h.DelegationDepth))
	if h.ParentSession != "" {
		b.WriteString(`,"parentSession":`)
		b.WriteString(jstr(h.ParentSession))
	}
	if len(h.Origin) > 0 {
		b.WriteString(`,"origin":`)
		b.Write(h.Origin)
	}
	if h.AgentPreset != "" {
		b.WriteString(`,"agentPreset":`)
		b.WriteString(jstr(h.AgentPreset))
	}
	b.WriteString(`}`)
	return b.Bytes(), nil
}

// jstr 是「不转义 HTML 字符」的 JSON 字符串（默认 encoder 会把 < > & 转成 \\u003c 等，
// 那会让我们写出的历史与 DSH 写出的历史出现无意义的字节差异）。
func jstr(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

// Writer 只做两件事：写一个独立的 header 帧，之后每批事件写一个独立帧。
//
// 三条落到字节上的规矩（AGENTS §4）：
//   - 每帧都是**完整独立**的 zstd 帧（用 EncodeAll，而不是 Flush）；
//   - 只开 checksum、不设 level；
//   - append-only：已提交的字节永不重写。
type Writer struct {
	f      *os.File
	enc    *zstd.Encoder
	frames int
	closed bool
}

// Create 新建一个 v4 会话文件，并写入 header 帧。
func Create(path string, h Header) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("建会话目录失败（%s）: %w；确认 home 指向一个目录、且可写", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("创建会话文件失败: %w", err)
	}
	w, err := newWriter(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := w.writeFrame(append(h.marshalNoErr(), '\n')); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

// OpenAppend 打开已有文件，校验首帧后从尾部继续追加。
func OpenAppend(path string) (*Writer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读会话文件失败: %w", err)
	}
	if _, err := Read(b); err != nil {
		return nil, fmt.Errorf("拒绝追加：现有文件不合法: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return newWriter(f)
}

func newWriter(f *os.File) (*Writer, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, enc: enc}, nil
}

func (h Header) marshalNoErr() []byte {
	b, err := h.MarshalJSON()
	if err != nil {
		// 这里不可能失败：全部字段都是基础类型。
		panic("header 序列化失败: " + err.Error())
	}
	return b
}

// Append 把一批事件写成一帧。事件带上 Raw 时按原始字节写（保真），否则按结构序列化。
func (w *Writer) Append(events ...Event) error {
	if w.closed {
		return fmt.Errorf("writer 已关闭")
	}
	if len(events) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, ev := range events {
		line := ev.Raw
		if len(line) == 0 {
			b, err := json.Marshal(ev)
			if err != nil {
				return fmt.Errorf("事件 %q 序列化失败: %w", ev.Type, err)
			}
			line = b
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return w.writeFrame(buf.Bytes())
}

func (w *Writer) writeFrame(payload []byte) error {
	frame := w.enc.EncodeAll(payload, nil)
	if len(frame) < 8 {
		return fmt.Errorf("编码出的帧异常短（%d 字节）", len(frame))
	}
	if _, err := w.f.Write(frame); err != nil {
		return fmt.Errorf("写帧失败: %w", err)
	}
	// durable：DSH 的契约是「fsync 之后才 resolve」。
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("fsync 失败: %w", err)
	}
	w.frames++
	return nil
}

// Frames 返回已写入的帧数（含 header 帧）。
func (w *Writer) Frames() int { return w.frames }

// Close 关闭文件与编码器。
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.enc.Close()
	return w.f.Close()
}
