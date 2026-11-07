package officialwire

import (
	"context"
	"encoding/json"
)

// mux 帧的七种 type（EVIDENCE §3.1）。
//
// 客户端 → 宿主：open / item / end / cancel
// 宿主 → 客户端：item / end / error
//
// **ready / emit / waterfall / cancel{eventId} 不是 mux 帧**——它们是
// `$events` 逻辑流里的 item 载荷（EVIDENCE §3.2）。别把两层混在一个判别式里。
const (
	FrameOpen   = "open"
	FrameItem   = "item"
	FrameEnd    = "end"
	FrameCancel = "cancel"
	FrameError  = "error"
)

// EndpointEvents 是应用级转发事件逻辑流的固定 endpoint（EVIDENCE §3.2）。
const EndpointEvents = "$events"

// clientFrame 是一条解析通过的上行帧。
type clientFrame struct {
	Type     string
	StreamID string
	Endpoint string
	// Args 只在 open 上有（`payload:{args}` 里的 args，已经过 exact-key 校验）。
	Args json.RawMessage
	// Value 只在 item 上可能有；HasValue=false 表示 `undefined`（value 缺省）。
	Value    json.RawMessage
	HasValue bool
}

// parseClientFrame 解析并**逐键校验**一条上行帧。
//
// 校验失败一律由调用方按 1008 关连接：官方桌面端对 mux 帧是 exact-key
// （多一个键就断连，EVIDENCE §3.1），我们照同样的严格度要求客户端，
// 才不会出现"我们收下了、官方客户端却会断"的双标。
func parseClientFrame(payload []byte) (*clientFrame, *Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil || raw == nil {
		return nil, Errorf(CodeProtocol, "mux 帧不是 JSON 对象")
	}
	var typ string
	if err := json.Unmarshal(raw["type"], &typ); err != nil {
		return nil, Errorf(CodeProtocol, "mux 帧缺 type")
	}
	frame := &clientFrame{Type: typ}
	if err := json.Unmarshal(raw["streamId"], &frame.StreamID); err != nil || frame.StreamID == "" {
		return nil, Errorf(CodeProtocol, "mux 帧 %s 缺 streamId", typ)
	}

	switch typ {
	case FrameOpen:
		if extra, field := extraKey(raw, "type", "streamId", "endpoint", "payload"); extra {
			return nil, Errorf(CodeProtocol, "open 帧多了字段 %s", field)
		}
		if err := json.Unmarshal(raw["endpoint"], &frame.Endpoint); err != nil || frame.Endpoint == "" {
			return nil, Errorf(CodeProtocol, "open 帧缺 endpoint")
		}
		args, err := payloadArgs(raw["payload"])
		if err != nil {
			return nil, err
		}
		frame.Args = args
	case FrameItem:
		if extra, field := extraKey(raw, "type", "streamId", "value"); extra {
			return nil, Errorf(CodeProtocol, "item 帧多了字段 %s", field)
		}
		if value, ok := raw["value"]; ok {
			// mux 上的 value 必须是**无损 JSON**（EVIDENCE §5.3）：这里不做
			// 类型推断，只保证它是合法 JSON——Uint8Array 本来就到不了这一层。
			if !json.Valid(value) {
				return nil, Errorf(CodeProtocol, "item 帧的 value 不是合法 JSON")
			}
			frame.Value = append(json.RawMessage(nil), value...)
			frame.HasValue = true
		}
	case FrameEnd, FrameCancel:
		if extra, field := extraKey(raw, "type", "streamId"); extra {
			return nil, Errorf(CodeProtocol, "%s 帧多了字段 %s", typ, field)
		}
	default:
		return nil, Errorf(CodeProtocol, "不认识的 mux 帧 type：%s", typ)
	}
	return frame, nil
}

// payloadArgs 校验 open 的 payload：必须恰好一个 args 字段，且 args 是对象。
func payloadArgs(rawPayload json.RawMessage) (json.RawMessage, *Error) {
	if len(rawPayload) == 0 {
		return nil, Errorf(CodeProtocol, "open 帧缺 payload")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rawPayload, &payload); err != nil || payload == nil {
		return nil, Errorf(CodeProtocol, "open 帧的 payload 必须是对象")
	}
	if extra, field := extraKey(payload, fieldArgs); extra {
		return nil, Errorf(CodeProtocol, "open 帧的 payload 多了字段 %s（只允许 args）", field)
	}
	args, ok := payload[fieldArgs]
	if !ok {
		return nil, Errorf(CodeProtocol, "open 帧的 payload 缺 args")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(args, &object); err != nil || object == nil {
		return nil, Errorf(CodeProtocol, "open 帧的 args 必须是对象")
	}
	return append(json.RawMessage(nil), args...), nil
}

// extraKey 报告 raw 里是否出现了 allowed 之外的键（返回第一个，按 map 顺序不稳定，
// 所以错误消息只用它做出诊断，不做断言依据）。
func extraKey(raw map[string]json.RawMessage, allowed ...string) (bool, string) {
	ok := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		ok[name] = struct{}{}
	}
	for key := range raw {
		if _, has := ok[key]; !has {
			return true, key
		}
	}
	return false, ""
}

// hostFrame 是宿主 → 客户端的三帧之一。
//
// Value 用 json.RawMessage 而不是 any：`value` 允许缺省（表示 undefined，
// EVIDENCE §3.1），而 any 的 omitempty 分不清 "undefined" 与 "null"。
type hostFrame struct {
	Type     string          `json:"type"`
	StreamID string          `json:"streamId"`
	Value    json.RawMessage `json:"value,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}

// StreamItem 是流上的一项。
//
// HasValue=false 表示这一项是 `undefined`（帧上不出现 value 键）。
type StreamItem struct {
	Value    json.RawMessage
	HasValue bool
}

// Stream 是一条逻辑流的宿主侧实现。
//
// Next 三个返回值：item / done / err。
//   - done=true 且 err=nil：正常结束，宿主发 end 帧
//   - err!=nil：失败结束，宿主发 error 帧（带 code/message/details）
//
// Next 必须尊重 ctx（取消时尽快返回 done=true）：不然 socket 已经没了，
// 这条 goroutine 还挂在那里，长跑就成了泄漏。
type Stream interface {
	Next(ctx context.Context) (item StreamItem, done bool, err *Error)
	Close()
}

// UplinkStream 是**接收上行 item** 的流。
//
// 不实现它 = 这条流的 uplink 被释放（官方 `$events` 就是"打开即释放上行"）。
// 实现了它才会做 256 KiB 累计配额与 Push 路由。
type UplinkStream interface {
	Push(value json.RawMessage) *Error
}

// StreamOpener 把 endpoint 变成一条流。
//
// 不认识 / 未接线时返回 `gateway/method-unavailable`，别返回 (nil, nil)：
// 那会让宿主发一个空 end，客户端以为"流正常结束了"。
type StreamOpener interface {
	Open(ctx context.Context, endpoint string, args json.RawMessage) (Stream, *Error)
}
