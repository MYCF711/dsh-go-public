package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

var (
	// ErrInvalidAnchor 表示 anchor 不存在、歧义、或指向非用户消息。
	ErrInvalidAnchor = errors.New("invalid user anchor")
	// ErrNoRewindConversation 表示所选用户消息之前没有可续的对话。
	ErrNoRewindConversation = errors.New("no conversation before user anchor")
)

// Fork 从一个已有会话派生一个**新会话**。无 anchor 的旧语义逐字保留：
// 继承 surface、记 parentSession、不继承授权，也不凭空新增 title 事件。
func Fork(src *v4.Log, home, cwd, newID string) (*Log, error) {
	return forkCopy(src, home, cwd, newID, nil, nil)
}

// ForkAt 复制到指定用户消息（inclusive）。anchor 与 dsh-go 事件桥一致：
// 优先是源 v4 Event.Seq 的十进制字符串，也接受该用户消息已持久化的 id。
// title 非 nil 时按官方 session/title 形状原样保存，包括首尾空格。
func ForkAt(src *v4.Log, home, cwd, newID, anchor string, title *string) (*Log, error) {
	seq, err := ResolveUserAnchor(src, anchor)
	if err != nil {
		return nil, err
	}
	cut := logThrough(src, seq)
	visible := false
	for _, node := range Surface(cut) {
		if node.Seq == seq && node.Type == "user/message" && node.Source == "user" {
			visible = true
			break
		}
	}
	if !visible {
		return nil, fmt.Errorf("%w: anchored user message is not in the visible surface", ErrInvalidAnchor)
	}
	return forkCopy(src, home, cwd, newID, &forkCut{seq: seq, inclusive: true}, title)
}

// ForkBefore 复制到指定用户消息之前，供 conversation rewind 使用。
// 与 /fork 的 inclusive anchor 不同；第一条用户消息之前没有对话时明确拒绝。
func ForkBefore(src *v4.Log, home, cwd, newID, anchor string) (*Log, error) {
	seq, err := ValidateForkBefore(src, anchor)
	if err != nil {
		return nil, err
	}
	return forkCopy(src, home, cwd, newID, &forkCut{seq: seq, inclusive: false}, nil)
}

// ValidateForkBefore 在不创建子会话的前提下验证 conversation rewind 的切点。
// both 模式必须先过这道检查再恢复文件，避免非法/首个 anchor 留下半完成状态。
func ValidateForkBefore(src *v4.Log, anchor string) (int64, error) {
	seq, err := ResolveUserAnchor(src, anchor)
	if err != nil {
		return 0, err
	}
	cut := logBefore(src, seq)
	for _, node := range Surface(cut) {
		if node.Type != "system/message" {
			return seq, nil
		}
	}
	return 0, fmt.Errorf("%w: %s", ErrNoRewindConversation, anchor)
}

// ResolveUserAnchor 把 durable anchor 解析成唯一、真实的用户事件 seq。
func ResolveUserAnchor(src *v4.Log, anchor string) (int64, error) {
	if src == nil || anchor == "" {
		return 0, fmt.Errorf("%w: empty anchor", ErrInvalidAnchor)
	}
	var found int64
	matches := 0
	for _, ev := range src.Events {
		if ev.Type != "user/message" {
			continue
		}
		var d struct {
			ID        string `json:"id"`
			MessageID string `json:"messageId"`
			Message   struct {
				ID        string `json:"id"`
				MessageID string `json:"messageId"`
			} `json:"message"`
			Source struct {
				Kind string `json:"kind"`
			} `json:"source"`
		}
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		matched := anchor == strconv.FormatInt(ev.Seq, 10)
		for _, id := range []string{d.ID, d.MessageID, d.Message.ID, d.Message.MessageID} {
			if id != "" && anchor == id {
				matched = true
			}
		}
		if !matched {
			continue
		}
		if d.Source.Kind != "user" {
			return 0, fmt.Errorf("%w: anchor is not a user message", ErrInvalidAnchor)
		}
		found = ev.Seq
		matches++
	}
	if matches != 1 {
		return 0, fmt.Errorf("%w: matched %d user messages", ErrInvalidAnchor, matches)
	}
	return found, nil
}

func logThrough(src *v4.Log, seq int64) *v4.Log {
	cut := *src
	cut.Events = nil
	for _, ev := range src.Events {
		if ev.Seq <= seq {
			cut.Events = append(cut.Events, ev)
		}
	}
	return &cut
}

func logBefore(src *v4.Log, seq int64) *v4.Log {
	cut := *src
	cut.Events = nil
	for _, ev := range src.Events {
		if ev.Seq < seq {
			cut.Events = append(cut.Events, ev)
		}
	}
	return &cut
}

// AppendSessionTitle 追加官方 title 事件，并写一个小的同目录读索引。
// 索引让 session.list 不必为了找可能被后续几十万事件推远的 title 而解压整份日志；
// v4 事件仍是跨实现可读的权威记录，二者都在返回成功前落盘。
func AppendSessionTitle(l *Log, title string) error {
	return appendSessionMetadata(l, "session/title", map[string]any{
		"title":       title,
		"messageSeqs": []int64{},
		"source":      map[string]any{"kind": "user"},
	})
}

type forkCut struct {
	seq       int64
	inclusive bool
}

// forkCopy 是 whole/inclusive/exclusive 三种边界共用的唯一复制实现。
func forkCopy(src *v4.Log, home, cwd, newID string, cutoff *forkCut, title *string) (*Log, error) {
	if newID == "" {
		return nil, fmt.Errorf("fork 需要新的会话 id")
	}
	if src.Header.ID == newID {
		return nil, fmt.Errorf("新会话 id 不能和源会话相同")
	}

	source := src
	if cutoff != nil {
		if cutoff.inclusive {
			source = logThrough(src, cutoff.seq)
		} else {
			source = logBefore(src, cutoff.seq)
		}
	}
	surface := Surface(source)
	if len(surface) == 0 {
		return nil, fmt.Errorf("源会话没有可继承的 surface")
	}
	// Surface 只装 text + tool_calls；"这条回答是怎么收工的、这个工具体成没成、
	// 在哪跑的、属于哪一轮哪一步"都不在里面。两张表都按 seq 回原始事件取，
	// 不往 SurfaceMsg 上加字段（见 ReasoningBlocksBySeq / forkFactsBySeq）。
	// 保留 src 作为本函数唯一的只读日志变量；cutoff 时它已是边界后的副本。
	src = source
	reasoning := ReasoningBlocksBySeq(src)
	facts := forkFactsBySeq(src)

	seg, err := v4.EncodeSegment(newID)
	if err != nil {
		return nil, err
	}
	path := dirOf(home, v4.ProjectKey(cwd), seg)
	h := src.Header
	h.ID = newID
	h.Cwd = cwd
	h.ParentSession = src.Header.ID
	h.CreatedAt = 0 // 由 v4.Create 重新打时间

	w, err := v4.Create(path, h)
	if err != nil {
		return nil, err
	}
	l := &Log{w: w, Path: path, now: time.Now}
	defer func() {
		if err != nil {
			l.Close()
		}
	}()

	for _, node := range surface {
		if err = copyNode(l, node, facts[node.Seq], reasoning[node.Seq]); err != nil {
			return nil, err
		}
	}
	if title != nil {
		if err = AppendSessionTitle(l, *title); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// srcFact 是一条源事件里"Surface 装不下、fork 又必须原样搬走"的事实。
//
// 为什么不给 SurfaceMsg 加字段：History / compact.Select / Meter 这些读路径一个都不
// 需要它们，fork 是唯一消费者；回原始事件按 seq 取，反而不动公开形状。
//
// 更重要的原因：这些字段**不能填默认值**。以前 copyNode 把 finish 写成 "stop"、
// isError 写成 false、cwd 写成 ""、turn/step 写成 1,1、developer/message 整类跳过——
// 于是一个被 max-tokens 截断的回合在 fork 之后显示"正常收工"，失败的工具体变成成功。
// 那不是"少一个字段"，是把事实改写成了结论，而且全链路不报错。
type srcFact struct {
	turn, step *int // nil = 源里没写（只好沿用旧默认）

	// assistant/message
	stream      json.RawMessage // 原样的 stream（含 finish 记录）；有才整段搬
	usage       json.RawMessage
	finish      string // 顶层 finish / finishReason 兜底（stream 缺失的老语料）
	interrupted bool

	// tool/result
	isError bool
	meta    json.RawMessage // meta 原样（cwd 与将来加的键都跟着走）
	errData json.RawMessage // error 原样（只在 isError 时才有意义）
	view    json.RawMessage // H3：客户端渲染声明

	// developer/message：message 原样（tool-addition / tool-removal 块只在原始事件里）
	developer json.RawMessage
}

// forkFactsBySeq 按 seq 建"源事件事实表"。
//
// 与 ReasoningBlocksBySeq 同一个做法（Surface 一个字段不加），但这里管的是**结论类**
// 字段：finish / isError / cwd / turn-step / view / developer 形状。
// 单条事件解不开就当作"源里没写这些字段"（与旧行为一致），不让一条坏事件打断整个 fork。
func forkFactsBySeq(log *v4.Log) map[int64]srcFact {
	out := make(map[int64]srcFact, len(log.Events))
	for _, ev := range log.Events {
		var f srcFact
		switch ev.Type {
		case "assistant/message":
			var d struct {
				Turn         *int            `json:"turn"`
				Step         *int            `json:"step"`
				Stream       json.RawMessage `json:"stream"`
				Usage        json.RawMessage `json:"usage"`
				Interrupted  bool            `json:"interrupted"`
				Finish       json.RawMessage `json:"finish"`
				FinishReason json.RawMessage `json:"finishReason"`
			}
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				f.turn, f.step = d.Turn, d.Step
				f.interrupted = d.Interrupted
				f.finish = finishName(d.Finish, d.FinishReason)
				f.usage = d.Usage
				// 只在 stream 里**真的有** finish 记录时才整段搬：没有 finish 的流
				// 搬过去会让新事件看起来"没有收工凭据"，不如按 finish 兜底重写。
				if _, ok := streamFinishKind(d.Stream); ok {
					f.stream = d.Stream
				}
			}

		case "tool/result":
			var d struct {
				Turn    *int `json:"turn"`
				Step    *int `json:"step"`
				Message struct {
					IsError *bool `json:"isError"`
				} `json:"message"`
				Meta  json.RawMessage `json:"meta"`
				Error json.RawMessage `json:"error"`
				View  json.RawMessage `json:"view"`
			}
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				f.turn, f.step = d.Turn, d.Step
				if d.Message.IsError != nil {
					f.isError = *d.Message.IsError
				}
				f.meta, f.errData, f.view = d.Meta, d.Error, d.View
			}

		case "system/message", "developer/message":
			var d struct {
				Turn    *int            `json:"turn"`
				Step    *int            `json:"step"`
				Message json.RawMessage `json:"message"`
			}
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				f.turn, f.step = d.Turn, d.Step
				if ev.Type == "developer/message" {
					f.developer = d.Message
				}
			}
		}
		out[ev.Seq] = f
	}
	return out
}

// turnOr / stepOr：源里有就沿用，没有才退到旧默认。
func (f srcFact) turnOr(def int) int {
	if f.turn != nil {
		return *f.turn
	}
	return def
}

func (f srcFact) stepOr(def int) int {
	if f.step != nil {
		return *f.step
	}
	return def
}

// cwdOf 取 meta.cwd——工具结果"在哪跑的"就记在这儿。
func (f srcFact) cwdOf() string {
	var d struct {
		Cwd string `json:"cwd"`
	}
	if rawJSON(f.meta) && json.Unmarshal(f.meta, &d) == nil {
		return d.Cwd
	}
	return ""
}

// rawJSON 报告这段 RawMessage 是不是一个真正的值（nil 与 "null" 都不算）。
func rawJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// streamFinishKind 从 assistant/message 的 stream 里取 finish 记录。
//
// 认的是 v4 的形状：`[{"type":"finish","reason":{"kind":"max-tokens"}}]`。
// 找到就整段搬（流里还可能有别的记录，全保留）；找不到返回 ok=false。
func streamFinishKind(raw json.RawMessage) (string, bool) {
	if !rawJSON(raw) {
		return "", false
	}
	var recs []struct {
		Type   string `json:"type"`
		Reason struct {
			Kind string `json:"kind"`
		} `json:"reason"`
	}
	if err := json.Unmarshal(raw, &recs); err != nil {
		return "", false
	}
	for _, r := range recs {
		if r.Type == "finish" && r.Reason.Kind != "" {
			return r.Reason.Kind, true
		}
	}
	return "", false
}

// finishName 取顶层 finish / finishReason 里的收工原因，作为 stream 缺失时的兜底。
//
// 三种形状都认：`"finish":"stop"`、`"finish":{"kind":"max-tokens"}`、
// `"finishReason":"max-tokens"`。返回值直接喂给 AssistantMessageEvent 的映射——
// 简化词（stop / length）与 v4 词（completed / max-tokens）它两种都收。
func finishName(raws ...json.RawMessage) string {
	for _, raw := range raws {
		if !rawJSON(raw) {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
		var o struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &o) == nil && o.Kind != "" {
			return o.Kind
		}
	}
	return ""
}

// copyNode 把 surface 上的一个节点写成新会话里的一条事件。
//
// f 是这条事件在源日志里的事实（finish / isError / cwd / turn-step / view / developer 形状），
// reasoning 是源事件里的思考正文（surface 装不下，见 ReasoningBlocksBySeq）。
func copyNode(l *Log, node SurfaceMsg, f srcFact, reasoning []string) error {
	m := node.Message
	switch {
	case node.Summary:
		// 压缩替身没法照搬（shadowedSeqs 指向的 seq 在新文件里不存在），
		// 改成一条来源标记为 compact-checkpoint 的 user 消息——语义最接近且不会撒谎。
		return l.Append("user/message", map[string]any{
			"content":   []spi.Block{{Type: "text", Text: m.Content}},
			"source":    map[string]any{"kind": "compact-checkpoint"},
			"role":      "user",
			"id":        NewID(),
			"surfaceOp": "append",
		})

	case node.Type == "system/message":
		// turn/step 沿用源里的：系统提示是在第几轮第几步渲染的，也是审计事实。
		return l.Append("system/message", SystemMessageData(f.turnOr(1), f.stepOr(1), m.Content))

	case node.Type == "developer/message":
		// developer/message 装的是工具增删声明（tool-addition / tool-removal 块）。
		// Surface 只留 joinText 之后的正文，形状在那一步就丢了，所以**原样搬 message**。
		//
		// 不搬 headerSeq：它指向源日志里的 request/header，而 fork 不复制 request/*，
		// 新日志的 seq 又整体重排——照搬会指向一条**不相关**的事件，比缺失更糟。
		var payload any = f.developer
		if !rawJSON(f.developer) {
			payload = map[string]any{
				"role":    "developer",
				"content": []map[string]any{TextBlock(m.Content)},
			}
		}
		return l.Append("developer/message", map[string]any{
			"turn":      f.turnOr(1),
			"step":      f.stepOr(1),
			"message":   payload,
			"surfaceOp": "append",
		})

	case node.Type == "user/message":
		return l.Append("user/message", UserMessage([]spi.Block{{Type: "text", Text: m.Content}}))

	case node.Type == "assistant/message":
		// 块顺序与 cmd/dshd/serve_turn.go 的 assistantBlocks 一致：
		// 思考 → 正文 → 工具调用。
		//
		// 以前这里只铺 text + tool_calls：fork 出来的会话看起来一切正常，
		// 只是模型再也想不起父会话为什么那么干；而"只有思考"的回答会变成
		// 一条 content 为空的事件（投影读回来一个空气泡）。
		blocks := make([]map[string]any, 0, len(reasoning)+1+len(m.ToolCalls))
		for _, text := range reasoning {
			blocks = append(blocks, ReasoningBlock(text))
		}
		if m.Content != "" {
			blocks = append(blocks, TextBlock(m.Content))
		}
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, ToolCallBlock(tc.ID, tc.Function.Name, tc.Function.Arguments))
		}
		// finish 不再恒写 "stop"：源里 stream 有 finish 记录就整段搬（max-tokens /
		// tool-calls / aborted 都跟着走）；没有才用顶层 finish 兜底，再没有才退到 "stop"。
		finish := f.finish
		if finish == "" {
			finish = "stop"
		}
		data := AssistantMessageEvent(f.turnOr(1), f.stepOr(1), blocks, finish, nil)
		if f.stream != nil {
			data["stream"] = f.stream
		}
		if rawJSON(f.usage) {
			data["usage"] = f.usage
		}
		if f.interrupted {
			data["interrupted"] = true
		}
		return l.Append("assistant/message", data)

	case node.Type == "tool/result":
		blocks := []spi.Block{{Type: "text", Text: m.Content}}
		data := ToolResultEvent(f.turnOr(1), f.stepOr(1), m.ToolCallID, blocks, f.isError, f.cwdOf(), nil)
		// meta 原样搬：cwd 就在里面，将来 meta 加键也不会在 fork 时被抹掉。
		if rawJSON(f.meta) {
			data["meta"] = f.meta
		}
		// error 只在源里有时才写（v4：isError=true 才允许有 error）。
		if rawJSON(f.errData) {
			data["error"] = f.errData
		}
		// H3：view 是工具产出的一部分（客户端按它画卡片），不是渲染缓存。
		if rawJSON(f.view) {
			data["view"] = f.view
		}
		return l.Append("tool/result", data)
	}
	return nil // 其它类型不进 surface，Fork 只保证 surface 语义
}

// dirOf 拼出会话文件路径（与 CreateSession 同规则）。
func dirOf(home, projectKey, seg string) string {
	parts := []string{home, "sessions", projectKey, seg, "session.v4.jsonl.zstd"}
	out := ""
	for i, p := range parts {
		if i > 0 && p != "" {
			out += "/"
		}
		out += p
	}
	return out
}

// ForkIDs 是给诊断用的：列出源会话的 surface 里各类节点数量。
func ForkIDs(src *v4.Log) map[string]int {
	out := map[string]int{}
	for _, n := range Surface(src) {
		key := n.Type
		if n.Summary {
			key = "compaction/summary"
		}
		out[key]++
	}
	return out
}

// SortedKeys 返回有序的键（诊断输出用）。
func SortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FindByID 在 <home>/sessions/*/<segment>/ 里按会话 id 找会话文件。
//
// 只用于"客户端只给了 id"的场景（比如 ACP 的 deleteSession）。
// 找到多个就报错——同名会话不该被随便挑一个删掉。
func FindByID(home, sessionID string) (string, error) {
	seg, err := v4.EncodeSegment(sessionID)
	if err != nil {
		return "", err
	}
	pattern := filepath.Join(home, "sessions", "*", seg, "session.v4.jsonl.zstd")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("找不到会话 %s（在 %s 下）", sessionID, home)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("会话 %s 在多个项目目录下都有（%d 个），请显式给 cwd", sessionID, len(matches))
	}
}

// ResolvePath 找一份会话的文件路径：先按 (cwd, id) 算，找不到再按 id 全库找。
//
// 为什么需要回退：projectKey 是按 **cwd 字符串**算出来的，同一个逻辑 workspace
// 换个盘符/挂载点/另一台机器，目录名就变了。而 session id 是全局唯一的（uuid），
// 实测踩过：拿正确的 id + 换过的 cwd 去 resume，报"找不到会话"。
//
// 与 FindByID 的取舍一致：多处命中就报错让人指定 cwd，不替人猜。
func ResolvePath(home, cwd, sessionID string) (string, error) {
	direct, err := SessionPath(home, cwd, sessionID)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(direct); statErr == nil {
		return direct, nil
	}
	found, ferr := FindByID(home, sessionID)
	if ferr != nil {
		return "", fmt.Errorf("按 cwd 算出的位置没有（%s），按 id 也没找到: %w", direct, ferr)
	}
	return found, nil
}
