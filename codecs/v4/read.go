package v4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// headerKeys 是 header 行的**白名单**：多一个键即非法。
// 宽进会把 v5 的字段读成 v4，然后写坏历史。
var headerKeys = map[string]struct{}{
	"type": {}, "version": {}, "id": {}, "createdAt": {}, "isSeeded": {}, "delegationDepth": {},
	"cwd": {}, "parentSession": {}, "origin": {}, "agentPreset": {},
}

// Header 是文件第一行（type 恒为 "session"）。
type Header struct {
	Type            string          `json:"type"`
	Version         int             `json:"version"`
	ID              string          `json:"id"`
	CreatedAt       int64           `json:"createdAt"`
	IsSeeded        bool            `json:"isSeeded"`
	DelegationDepth int             `json:"delegationDepth"`
	Cwd             string          `json:"cwd,omitempty"`
	ParentSession   string          `json:"parentSession,omitempty"`
	Origin          json.RawMessage `json:"origin,omitempty"`
	AgentPreset     string          `json:"agentPreset,omitempty"`
}

// UnmarshalJSON 严格校验键集。
func (h *Header) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("header 不是 JSON 对象: %w", err)
	}
	var extra []string
	for k := range raw {
		if _, ok := headerKeys[k]; !ok {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("header 出现白名单外的键: %v（多一个键即非法）", extra)
	}
	type alias Header
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*h = Header(a)
	return nil
}

// Event 是日志里的一行事件。Data 保留原始字节，交给上层的 canonical 模型解释。
type Event struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
	Raw  json.RawMessage `json:"-"`

	// 信封上的可选字段。surfaceOp 有两种形状：字符串 "append"，
	// 或 {op:"replace", startSeq, endSeq}（含端点）。
	SurfaceOp       json.RawMessage `json:"surfaceOp,omitempty"`
	SourceEventSeqs []int64         `json:"sourceEventSeqs,omitempty"`
	Ignorable       bool            `json:"ignorable,omitempty"`
}

// SurfaceOpKind 返回 surfaceOp 的种类与替换区间。
// append（或缺省）返回 ("append", 0, 0)；替换返回 ("replace", start, end)。
func (e Event) SurfaceOpKind() (kind string, start, end int64) {
	raw := bytes.TrimSpace(e.SurfaceOp)
	if len(raw) == 0 {
		return "append", 0, 0
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s, 0, 0
		}
		return "append", 0, 0
	}
	var op struct {
		Op       string `json:"op"`
		StartSeq int64  `json:"startSeq"`
		EndSeq   int64  `json:"endSeq"`
	}
	if err := json.Unmarshal(raw, &op); err != nil || op.Op == "" {
		return "append", 0, 0
	}
	return op.Op, op.StartSeq, op.EndSeq
}

// Log 是一个会话文件的解析结果。
type Log struct {
	Path   string
	Frames int
	Header Header
	Events []Event

	// TailTruncatedAt 非零表示文件尾部有半帧（断电/磁盘满），
	// 值是那半帧的起始偏移。前面的内容都还能用。
	TailTruncatedAt int
}

// ReadFile 读并解析一个 v4 会话文件。
func ReadFile(path string) (*Log, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读会话文件失败: %w", err)
	}
	l, err := Read(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	l.Path = path
	return l, nil
}

// Read 从字节解析。
func Read(b []byte) (*Log, error) {
	frames, tornAt, err := splitFrames(b)
	if err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("文件里没有 zstd 帧")
	}

	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	head, err := decodeFrame(dec, b[frames[0].offset:frames[0].offset+frames[0].length])
	if err != nil {
		return nil, fmt.Errorf("首帧: %w", err)
	}
	// 契约：首帧只装 header 一行，且以 \n 结尾（list/stat 只解首帧，靠这条跳过整个 body）。
	if !bytes.HasSuffix(head, []byte("\n")) {
		return nil, fmt.Errorf("首帧明文没有以 \\n 结尾")
	}
	body := head[:len(head)-1]
	if bytes.ContainsRune(body, '\n') {
		return nil, fmt.Errorf("首帧含 %d 行，契约要求恰好 1 行（header 必须独立成帧）", bytes.Count(head, []byte("\n")))
	}

	var l Log
	// 首帧校验与 ReadHeader 共用同一份（parseHeader），避免「列会话」与「打开会话」
	// 对 header 的判断分叉。
	h, err := parseHeader(head)
	if err != nil {
		return nil, err
	}
	l.Header = h
	l.Frames = len(frames)
	// 尾部有半帧（断电/磁盘满）：前面的照常返回，只把位置记下来。
	l.TailTruncatedAt = tornAt

	for i, f := range frames[1:] {
		plain, err := decodeFrame(dec, b[f.offset:f.offset+f.length])
		if err != nil {
			// 断电/磁盘满会在尾部留下半帧。**为了半帧丢掉整份历史是最坏的结果**，
			// 所以这里把"从第 N 帧开始读不动了"记下来，前面的照样返回。
			//
			// 判据只认一条：坏帧在尾巴上（它之后没有完整帧了）。
			// 中间坏一帧说明文件真的坏了，那时才整体报错。
			if i == len(frames)-2 {
				l.TailTruncatedAt = f.offset
				l.Frames = i + 1
				return &l, nil
			}
			return nil, fmt.Errorf("第 %d 帧: %w", i+1, err)
		}
		for _, line := range strings.Split(string(plain), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				return nil, fmt.Errorf("第 %d 帧里的事件不是合法 JSON: %w", i+1, err)
			}
			if ev.Type == "" {
				return nil, fmt.Errorf("第 %d 帧里有一行没有 type", i+1)
			}
			ev.Raw = json.RawMessage(line)
			l.Events = append(l.Events, ev)
		}
	}
	return &l, nil
}
