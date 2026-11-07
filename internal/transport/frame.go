package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 帧协议的哨兵错误。
//
// 调用方（WS 处理循环）靠它们区分"这条连接断了"和"这个客户端在说胡话"：
// 前者安静收尾，后者要回一条错误并说清哪里不对。
// 一律用 errors.Is 判定，别比字符串。
var (
	// ErrEmptyFrame 表示客户端发来的是一段空白（或根本没内容）。
	ErrEmptyFrame = errors.New("transport: 收到空帧")

	// ErrNotJSONObject 表示这段正文不是 JSON 对象（数组、字符串、null、纯文本都算）。
	ErrNotJSONObject = errors.New("transport: 帧必须是 JSON 对象")

	// ErrBadFrameJSON 表示正文是对象形状，但解析不出帧结构（语法错、字段类型错）。
	ErrBadFrameJSON = errors.New("transport: 帧的 JSON 不合法")

	// ErrUnknownFrameType 表示 t 不是 sub / unsub / ping。
	// **不要把它当成 ping 放过**——客户端会以为自己订阅成功了，然后一直等不到事件。
	ErrUnknownFrameType = errors.New("transport: 不认识的帧类型")

	// ErrMissingTopic 表示 sub / unsub 没带 topic。
	ErrMissingTopic = errors.New("transport: 这个帧必须带 topic")

	// ErrInvalidTopic 表示 topic 不是 "session:<非空 id>"。
	ErrInvalidTopic = errors.New("transport: topic 不合法")

	// ErrBadSeq 表示要造的 seq <= 0。seq 是客户端判连续性的唯一依据，不许造 0。
	ErrBadSeq = errors.New("transport: seq 必须大于 0")

	// ErrEmptyFrameType 表示要造的事件没有 type。
	ErrEmptyFrameType = errors.New("transport: 事件必须有 type")

	// ErrBadFrameData 表示 data 那段字节不是合法 JSON。
	ErrBadFrameData = errors.New("transport: data 不是合法 JSON")
)

// 客户端帧的类型。就这三种，多一种都不加——加了就是协议变更。
const (
	// FrameSub 订阅一个 topic，可带 sinceSeq 从某个序号起重放。
	FrameSub = "sub"
	// FrameUnsub 取消订阅。
	FrameUnsub = "unsub"
	// FramePing 心跳，不改变订阅状态。
	FramePing = "ping"
)

// SessionTopicPrefix 是唯一合法的 topic 前缀。
const SessionTopicPrefix = "session:"

// ClientFrame 是客户端发来的一帧。
//
// 形状不能改（界面线已经按这个形状写了客户端）：
//
//	{"t":"sub","topic":"session:<id>","sinceSeq":123}
//	{"t":"unsub","topic":"session:<id>"}
//	{"t":"ping"}
type ClientFrame struct {
	T        string `json:"t"`
	Topic    string `json:"topic,omitempty"`
	SinceSeq int    `json:"sinceSeq,omitempty"`
	// JournalEpoch 是客户端以为自己在这条 journal 的哪个世代（H14）。
	// 不带（0）时按"首次订阅"处理；带上且对不上 → 服务端发 session/reset。
	JournalEpoch int64 `json:"journalEpoch,omitempty"`
}

// ServerFrame 是推给客户端的一帧事件。type/data 用 v4 的原始词汇，不另造名字。
type ServerFrame struct {
	Seq  int             `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
	// Time 是这条事件的墙钟时间（epoch 毫秒，H2）。
	//
	// 界面要用它算 tok/s：输出 token ÷ 相邻两条 assistant 的时间差。
	// **没有 time 就算不出速率**——消息里有 time 但流里没有，等于流式期间算不了。
	Time int64 `json:"time,omitempty"`
}

// ParseClientFrame 校验并解析一帧。
//
// 三种帧：sub（必须带 topic，且 topic 必须是 "session:<非空>"）、unsub（必须带 topic）、ping。
// 别的 t 一律报错——**不认识的帧不许当 ping 放过**：客户端会以为自己订阅成功了。
func ParseClientFrame(raw []byte) (ClientFrame, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ClientFrame{}, ErrEmptyFrame
	}
	// 先卡形状：null / 数组 / 字符串都会让 json.Unmarshal 安静地给出零值，
	// 到后面就变成"不认识的帧类型 t=\"\""，那句话对客户端毫无帮助。
	if trimmed[0] != '{' {
		return ClientFrame{}, fmt.Errorf("%w: 正文以 %q 开头，要的是对象 {}", ErrNotJSONObject, firstRune(trimmed))
	}

	var f ClientFrame
	if err := json.Unmarshal(trimmed, &f); err != nil {
		return ClientFrame{}, fmt.Errorf("%w: %v", ErrBadFrameJSON, err)
	}

	switch f.T {
	case FrameSub:
		if err := requireTopic(f.Topic, f.T); err != nil {
			return ClientFrame{}, err
		}
		if err := requireSessionTopic(f.Topic); err != nil {
			return ClientFrame{}, err
		}
		if f.SinceSeq < SinceNothing {
			return ClientFrame{}, fmt.Errorf("%w: sinceSeq=%d 小于 %d；要「我什么都没拿」就省略该字段或写 %d",
				ErrBadSeq, f.SinceSeq, SinceNothing, SinceNothing)
		}
		return f, nil

	case FrameUnsub:
		// unsub 只要求带 topic：客户端取消订阅时我们不该因为前缀细节把它卡住，
		// 否则那条订阅会一直挂着（取消不掉比多挂一条更糟）。
		if err := requireTopic(f.Topic, f.T); err != nil {
			return ClientFrame{}, err
		}
		return f, nil

	case FramePing:
		// ping 不改变任何状态，topic / sinceSeq 带了也忽略。
		return f, nil

	case "":
		// 到这里说明是 `{}` 或 t 字段类型不对被忽略成了空串。
		return ClientFrame{}, fmt.Errorf("%w: 缺 t 字段；可选 %s / %s / %s",
			ErrUnknownFrameType, FrameSub, FrameUnsub, FramePing)

	default:
		return ClientFrame{}, fmt.Errorf("%w: t=%q；可选 %s / %s / %s",
			ErrUnknownFrameType, f.T, FrameSub, FrameUnsub, FramePing)
	}
}

// NewServerFrame 造一帧推给客户端。
//
// data 可以是 nil（有些事件没有负载），但只要给了就必须是合法 JSON——
// 一坨坏字节塞进 json.RawMessage 要等到写 socket 时才炸，那时已经没法告诉客户端是哪条事件坏了。
func NewServerFrame(seq int, typ string, data json.RawMessage) (ServerFrame, error) {
	if seq <= 0 {
		return ServerFrame{}, fmt.Errorf("%w: 收到 seq=%d", ErrBadSeq, seq)
	}
	if strings.TrimSpace(typ) == "" {
		return ServerFrame{}, ErrEmptyFrameType
	}
	if len(data) > 0 && !json.Valid(data) {
		return ServerFrame{}, fmt.Errorf("%w: seq=%d type=%q 的 data 解析不了", ErrBadFrameData, seq, typ)
	}
	return ServerFrame{
		Seq:  seq,
		Type: typ,
		Data: data,
	}, nil
}

// requireTopic 检查 sub / unsub 是否带了 topic。
func requireTopic(topic, kind string) error {
	if strings.TrimSpace(topic) == "" {
		return fmt.Errorf("%w: %s 帧需要 topic（形如 %s<会话 id>）", ErrMissingTopic, kind, SessionTopicPrefix)
	}
	return nil
}

// requireSessionTopic 检查 sub 的 topic 是 "session:<非空 id>"。
func requireSessionTopic(topic string) error {
	rest, ok := strings.CutPrefix(topic, SessionTopicPrefix)
	if !ok {
		return fmt.Errorf("%w: topic=%q 必须以 %q 开头（现在只支持会话流）",
			ErrInvalidTopic, topic, SessionTopicPrefix)
	}
	if strings.TrimSpace(rest) == "" {
		return fmt.Errorf("%w: topic=%q 里的会话 id 是空的", ErrInvalidTopic, topic)
	}
	return nil
}

// firstRune 取首个字符，只为了把错误说具体（别把整段正文回显给客户端）。
func firstRune(b []byte) string {
	for _, r := range string(b) {
		return string(r)
	}
	return ""
}
