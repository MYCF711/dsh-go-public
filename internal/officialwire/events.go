package officialwire

// `$events` 的**应用级转发事件**协议（EVIDENCE §3.2）。
//
// 两层判别式必须分清（parity 文档 §2 曾把两层混写；本包从 muxframe.go 起就分开）：
//
//	mux 帧（7 种）         open / item / end / cancel  ×  item / end / error
//	`$events` item 载荷    ready / emit / waterfall / cancel{eventId}
//
// `clientId / eventId / outcome` 属应用层，不是 mux 判别式 —— 把它们塞进 mux 的
// 解析表会让"多一个字段"的判定跑错层，而官方的判据正是"多一个字段就 1008 断连"。
//
// 官方出处（`@deepseek-ai/dsh-api-gateway@0.2.2-rc.1`，全部逐行读过）：
//
//	REMOTE_EVENT_STREAM_ENDPOINT   lib/types/stream-protocol.js:6
//	REMOTE_EVENT_RESULT_ENDPOINT   lib/types/stream-protocol.js:8
//	REMOTE_EVENT_STREAM_READY      lib/types/stream-protocol.js:12
//	parseRemoteEventResult         lib/types/stream-protocol.js:26-53
//	parseRemoteEventRejection      lib/types/stream-protocol.js:229-245
//	parseRemoteEventReady          lib/types/client/remote-events.js:181-193
//	parseRemoteEventFrame          lib/types/client/remote-events.js:198-226
//	assertRemoteEventFrame         lib/index.js:1477-1480
//	broadcastRemoteEvent           lib/index.js:1068-1094
//	startRemoteEvent               lib/index.js:1098-1150
//	receiveRemoteEventResult       lib/index.js:1164-1176
//	dispatchRpc("$events/result")  lib/index.js:993-1009

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strconv"
)

// EndpointEventResult 是 `$events/result` 的 wire endpoint（URL `/api/$events/result`）。
//
// 官方 `REMOTE_EVENT_RESULT_ENDPOINT = '$events/result'`（stream-protocol.js:8）。
// 客户端的调用形态是 `rpc.call('/api', '$events/result', {args: {clientId, eventId, outcome}})`。
const EndpointEventResult = "$events/result"

// `$events` item 载荷的 type 判别式（remote-events.js:198-226 的四个分支）。
const (
	// EventReady 是**首帧**：`{type:'ready', clientId, host:{home}}`。
	// 客户端用它换到 generation 的 clientId，是"这条流已就绪"的唯一证据。
	EventReady = "ready"
	// EventEmit 是广播：`{type:'emit', event, args}`。客户端只投给本地 listener，
	// **不回** `$events/result`。
	EventEmit = "emit"
	// EventWaterfall 是瀑布：`{type:'waterfall', event, eventId, agentId, request}`。
	// 宿主把事件当请求抛给客户端，等客户端回 `$events/result` 的 outcome 才继续。
	EventWaterfall = "waterfall"
	// EventCancel 是撤销：`{type:'cancel', eventId}`。客户端据此 abort 那条等待。
	EventCancel = "cancel"
)

// `$events/result` 的 outcome.kind 三态（stream-protocol.js:26-53）。
const (
	// OutcomeNext 表示"这条事件我不处理，交给下一个"（无其它字段）。
	OutcomeNext = "next"
	// OutcomeResult 表示"处理完了，这是结果"（可选 `value`）。
	OutcomeResult = "result"
	// OutcomeRejected 表示"处理时抛了"（**必须**带 `error`）。
	OutcomeRejected = "rejected"
)

// Rejection 是 `outcome.rejected` 里的 error 形状。
//
// 官方 `parseRemoteEventRejection`（stream-protocol.js:229-245）：
//
//	{name, message}                       必填，name 非空
//	+ code?: string
//	+ details?: 无损 JSON
//
// `code`/`details` 是 **omitempty**：官方对"键存在"与"键不存在"分别判定
// （`Object.hasOwn`），所以不能给一个空串占位——那会被判成"客户端给了个空 code"。
type Rejection struct {
	Name    string          `json:"name"`
	Message string          `json:"message"`
	Code    *string         `json:"code,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// Error 让 Rejection 能当普通 error 用（宿主把它写回子进程/日志时不必再包）。
func (r *Rejection) Error() string {
	if r == nil {
		return "<nil>"
	}
	if r.Code != nil && *r.Code != "" {
		return r.Name + ": " + r.Message + " (" + *r.Code + ")"
	}
	return r.Name + ": " + r.Message
}

// Outcome 是客户端对一条 waterfall 的回答。
//
// 三个字段互斥（按 Kind 取值）：
//
//	Kind == OutcomeNext      → Value/Error 都 nil
//	Kind == OutcomeResult    → Value 可能 nil（官方 `{kind:'result'}` 允许不带 value）
//	Kind == OutcomeRejected  → Error 非 nil
type Outcome struct {
	Kind string
	// Value 是 `outcome.value` 的原始字节；nil = 客户端没带这个键。
	//
	// 用 RawMessage 而不是 any：官方只要求它"无损 JSON"，**不投影**——
	// 宿主把它交回等待中的 handler，由 handler 自己解。
	Value json.RawMessage
	// Error 只在 Kind == OutcomeRejected 时非 nil。
	Error *Rejection
}

// EventResult 是一条解析通过的 `$events/result`。
type EventResult struct {
	ClientID string
	EventID  string
	Outcome  Outcome
}

// ParseEventResult 解析 `$events/result` 的 args。
//
// 只校验**顶层三个字段之外的形状**：字段集本身（exactly clientId/eventId/outcome）
// 由 Descriptor 在更外层校验（server.go 的 ValidateArgs）——两处都做一遍就会
// 出现"两个不同的错误码描述同一个问题"，客户端只会显示其中一条。
//
// 官方 `parseRemoteEventResult`（stream-protocol.js:26-53）的判定逐条对齐：
//
//	clientId / eventId      非空字符串（isRemoteEventClientId / isRemoteEventId）
//	outcome.kind == next     exact-key {kind}
//	outcome.kind == result   exact-key {kind} 或 {kind,value}，value 无损 JSON
//	outcome.kind == rejected exact-key {kind,error}，error 走 Rejection 形状
//	其它                     拒绝（官方抛 `api gateway: invalid Remote event result`）
//
// 拒绝一律 **fail-loud**：给一个空的/猜的 outcome 会让"客户端答错了"变成
// "宿主静默接受了垃圾"，而那条 waterfall 会被当成 next 继续往下跑。
func ParseEventResult(endpoint string, args map[string]json.RawMessage) (*EventResult, *Error) {
	result := &EventResult{}

	clientID, failure := nonEmptyStringArg(endpoint, args, "clientId")
	if failure != nil {
		return nil, failure
	}
	result.ClientID = clientID

	eventID, failure := nonEmptyStringArg(endpoint, args, "eventId")
	if failure != nil {
		return nil, failure
	}
	result.EventID = eventID

	rawOutcome, ok := args["outcome"]
	if !ok {
		return nil, ArgumentsInvalid(endpoint, "outcome", "缺 outcome 判别式")
	}
	outcome, failure := parseOutcome(endpoint, rawOutcome)
	if failure != nil {
		return nil, failure
	}
	result.Outcome = *outcome
	return result, nil
}

// parseOutcome 解析 outcome 判别式。
func parseOutcome(endpoint string, raw json.RawMessage) (*Outcome, *Error) {
	fields, ok := rawObject(raw)
	if !ok {
		return nil, ArgumentsInvalid(endpoint, "outcome", "outcome 必须是对象")
	}
	var kind string
	if err := json.Unmarshal(fields["kind"], &kind); err != nil {
		return nil, ArgumentsInvalid(endpoint, "outcome.kind", "outcome 缺 kind 判别式")
	}
	switch kind {
	case OutcomeNext:
		if extra, field := extraKey(fields, "kind"); extra {
			return nil, ArgumentsInvalid(endpoint, "outcome."+field,
				"outcome.kind=next 只允许一个 kind 字段（多一个字段官方客户端会判 result 非法）")
		}
		return &Outcome{Kind: OutcomeNext}, nil

	case OutcomeResult:
		if extra, field := extraKey(fields, "kind", "value"); extra {
			return nil, ArgumentsInvalid(endpoint, "outcome."+field,
				"outcome.kind=result 只允许 kind 与 value")
		}
		outcome := &Outcome{Kind: OutcomeResult}
		if value, has := fields["value"]; has {
			if !isLosslessJSON(value) {
				return nil, ArgumentsInvalid(endpoint, "outcome.value",
					"outcome.value 不是无损 JSON（NaN/Infinity/-0/不可序列化对象都过不了官方 isRemoteJsonValue）")
			}
			outcome.Value = append(json.RawMessage(nil), value...)
		}
		return outcome, nil

	case OutcomeRejected:
		if extra, field := extraKey(fields, "kind", "error"); extra {
			return nil, ArgumentsInvalid(endpoint, "outcome."+field,
				"outcome.kind=rejected 只允许 kind 与 error")
		}
		rawError, has := fields["error"]
		if !has {
			return nil, ArgumentsInvalid(endpoint, "outcome.error",
				"outcome.kind=rejected 必须带 error")
		}
		rejection, failure := parseRejection(endpoint, rawError)
		if failure != nil {
			return nil, failure
		}
		return &Outcome{Kind: OutcomeRejected, Error: rejection}, nil

	default:
		return nil, ArgumentsInvalid(endpoint, "outcome.kind",
			"不认识的 outcome.kind："+strconv.Quote(kind)+"（只有 next/result/rejected 三态）")
	}
}

// parseRejection 解析 `outcome.error`（官方 parseRemoteEventRejection）。
//
// 必填 name/message；name 非空；可选 code（字符串）、details（无损 JSON）。
// 允许的键集是 `{name,message}` + `{code,details}`，**多一个键就拒**。
func parseRejection(endpoint string, raw json.RawMessage) (*Rejection, *Error) {
	fields, ok := rawObject(raw)
	if !ok {
		return nil, ArgumentsInvalid(endpoint, "outcome.error", "outcome.error 必须是对象")
	}
	if extra, field := extraKey(fields, "name", "message", "code", "details"); extra {
		return nil, ArgumentsInvalid(endpoint, "outcome.error."+field,
			"outcome.error 只允许 name/message/code/details")
	}
	var name string
	if err := json.Unmarshal(fields["name"], &name); err != nil || name == "" {
		return nil, ArgumentsInvalid(endpoint, "outcome.error.name",
			"outcome.error.name 必须是非空字符串")
	}
	var message string
	if err := json.Unmarshal(fields["message"], &message); err != nil {
		return nil, ArgumentsInvalid(endpoint, "outcome.error.message",
			"outcome.error.message 必须是字符串")
	}
	rejection := &Rejection{Name: name, Message: message}
	if rawCode, has := fields["code"]; has {
		var code string
		if err := json.Unmarshal(rawCode, &code); err != nil {
			return nil, ArgumentsInvalid(endpoint, "outcome.error.code",
				"outcome.error.code 必须是字符串")
		}
		rejection.Code = &code
	}
	if rawDetails, has := fields["details"]; has {
		if !isLosslessJSON(rawDetails) {
			return nil, ArgumentsInvalid(endpoint, "outcome.error.details",
				"outcome.error.details 不是无损 JSON")
		}
		rejection.Details = append(json.RawMessage(nil), rawDetails...)
	}
	return rejection, nil
}

// Event 是宿主推给客户端的一条 `$events` 事件。
//
// 三种 Type 用的字段不同（照官方四个判别式）：
//
//	EventEmit      → Event + Args（JSON 数组）
//	EventWaterfall → Event + EventID + AgentID + Request（对象）
//	EventCancel    → EventID
type Event struct {
	// Type 是 EventEmit / EventWaterfall / EventCancel。
	Type string
	// Event 是事件名（emit/waterfall 必填、非空，官方 validRemoteEventName）。
	Event string
	// Args 是 emit 的参数数组。官方判据是 `Array.isArray(args) && isRemoteJsonValue(args)`。
	Args json.RawMessage
	// EventID 是 waterfall/cancel 的关联 id（非空）。
	EventID string
	// AgentID 是 waterfall 的 Agent 身份（非空，官方 isRemoteEventAgentId）。
	AgentID string
	// Request 是 waterfall 的请求负载：普通对象，且**不许**带 agent/signal
	// （官方 `!Object.hasOwn(value.request,'agent') && !Object.hasOwn(...,'signal')`）。
	Request json.RawMessage
}

// Validate 逐条对齐官方 `parseRemoteEventFrame` 的判定。
//
// 为什么在**宿主侧**也校验：官方客户端解析失败时是
// `invalidRemoteEventFrame()` → 抛 → `pumpEvents` 的 catch → 整个 generation 失败并重连。
// 也就是"宿主发错一个字段 = 用户的连接断了"。发之前自己先拒，才叫 fail-loud。
func (e Event) Validate() *Error {
	const endpoint = EndpointEvents
	switch e.Type {
	case EventEmit:
		if e.Event == "" {
			return Errorf(CodeInternal, "emit 事件的 event 名不能为空")
		}
		if !isJSONArray(e.Args) {
			return Errorf(CodeInternal, "emit 事件的 args 必须是 JSON 数组")
		}
		if !isLosslessJSON(e.Args) {
			return Errorf(CodeInternal, "emit 事件的 args 不是无损 JSON")
		}
		return nil

	case EventWaterfall:
		if e.Event == "" {
			return Errorf(CodeInternal, "waterfall 事件的 event 名不能为空")
		}
		if e.EventID == "" {
			return Errorf(CodeInternal, "waterfall 事件必须有 eventId（客户端靠它回 $events/result）")
		}
		if e.AgentID == "" {
			return Errorf(CodeInternal, "waterfall 事件必须有 agentId（官方要求非空 Agent 身份）")
		}
		fields, ok := rawObject(e.Request)
		if !ok {
			return Errorf(CodeInternal, "waterfall 事件的 request 必须是普通对象")
		}
		if _, has := fields["agent"]; has {
			return Errorf(CodeInternal, "waterfall 事件的 request 不许带 agent（官方由客户端补）")
		}
		if _, has := fields["signal"]; has {
			return Errorf(CodeInternal, "waterfall 事件的 request 不许带 signal（官方由客户端补）")
		}
		if !isLosslessJSON(e.Request) {
			return Errorf(CodeInternal, "waterfall 事件的 request 不是无损 JSON")
		}
		return nil

	case EventCancel:
		if e.EventID == "" {
			return Errorf(CodeInternal, "cancel 事件必须有 eventId")
		}
		return nil

	default:
		return Errorf(CodeInternal, "不认识的 $events 事件类型：%s", e.Type)
	}
}

// Payload 把事件编成 `$events` item 的 value 对象。
//
// 键集是 exact-key（多一个键官方客户端就判非法帧）：emit 三键、waterfall 五键、
// cancel 两键。用 map 而不是结构体，是为了让"某个键这次不该出现"直接体现成
// 没有这个键——`omitempty` 分不清"空串"与"没有"。
func (e Event) Payload() (json.RawMessage, *Error) {
	if failure := e.Validate(); failure != nil {
		return nil, failure
	}
	fields := make(map[string]json.RawMessage, 5)
	typ, err := json.Marshal(e.Type)
	if err != nil {
		return nil, Errorf(CodeInternal, "$events 事件类型序列化失败：%v", err)
	}
	fields["type"] = typ

	switch e.Type {
	case EventEmit:
		name, err := json.Marshal(e.Event)
		if err != nil {
			return nil, Errorf(CodeInternal, "$events 事件名序列化失败：%v", err)
		}
		fields["event"] = name
		fields["args"] = e.Args
	case EventWaterfall:
		name, err := json.Marshal(e.Event)
		if err != nil {
			return nil, Errorf(CodeInternal, "$events 事件名序列化失败：%v", err)
		}
		eventID, err := json.Marshal(e.EventID)
		if err != nil {
			return nil, Errorf(CodeInternal, "$events eventId 序列化失败：%v", err)
		}
		agentID, err := json.Marshal(e.AgentID)
		if err != nil {
			return nil, Errorf(CodeInternal, "$events agentId 序列化失败：%v", err)
		}
		fields["event"] = name
		fields["eventId"] = eventID
		fields["agentId"] = agentID
		fields["request"] = e.Request
	case EventCancel:
		eventID, err := json.Marshal(e.EventID)
		if err != nil {
			return nil, Errorf(CodeInternal, "$events eventId 序列化失败：%v", err)
		}
		fields["eventId"] = eventID
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, Errorf(CodeInternal, "$events 事件序列化失败：%v", err)
	}
	return payload, nil
}

// EventSource 是宿主侧的**转发事件源**：`$events` 流的数据从哪来。
//
// 为什么要这个接口而不是直接把事件塞进 MuxConfig：`$events` 的每一项都是
// **宿主业务事件**（approval/request、turn 结束……），协议层不认识它们，也不该认识。
// 协议层只负责"把 Event 编成合法帧"和"把 outcome 交回给谁"，事件源负责"事件什么时候发生"。
//
// 与官方的对应关系（lib/index.js）：
//
//	Subscribe  ← openRemoteEvents 的 remoteEventClients.set + 首次 ready 后的 queue.iterate
//	Answer     ← dispatchRpc("$events/result") 的 receiveRemoteEventResult
//
// 没有任何事件源时（nil）：`$events` 只发 ready，`$events/result` 回
// gateway/method-unavailable。**不伪造 emit**——官方客户端会去等一个不存在的应答，
// 界面上的表现是"永远转圈"，比"没有事件"糟得多。
type EventSource interface {
	// Subscribe 开一条订阅。clientID 是这条 `$events` 流的世代 id
	// （`$events/result` 靠它把 outcome 关联回来）。
	//
	// 通道关闭 = 事件源结束（宿主随后发 `end` 帧）。stop 必须幂等，
	// 且 stop 之后不得再向通道发送（否则发送方会永久阻塞）。
	Subscribe(ctx context.Context, clientID string) (events <-chan Event, stop func())

	// Answer 交付客户端对一条 waterfall 的 outcome。
	//
	// 返回非 nil 表示这次 outcome 无法交付：未知 clientId（官方
	// "Remote event result identifies no active event stream"）、
	// 不属于这个 Peer（官方 gateway/interactions-forbidden）等。
	// 返回 nil 表示已交付（**包括**"这条 eventId 已经不在了"——官方对
	// 未知 eventId 是静默 ok:true，见 receiveRemoteEventResult 的早返回）。
	Answer(clientID, eventID string, outcome Outcome) *Error
}

// nonEmptyStringArg 取一个必填的非空字符串参数。
func nonEmptyStringArg(endpoint string, args map[string]json.RawMessage, field string) (string, *Error) {
	raw, ok := args[field]
	if !ok {
		return "", ArgumentsInvalid(endpoint, field, "缺必填参数："+field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", ArgumentsInvalid(endpoint, field, field+" 必须是字符串")
	}
	if value == "" {
		return "", ArgumentsInvalid(endpoint, field, field+" 不能是空串（官方要求非空 id）")
	}
	return value, nil
}

// rawObject 把原始字节解成键值表；不是对象就 ok=false。
func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

// isJSONArray 判断原始字节是不是一个 JSON 数组。
func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '[' && json.Valid(trimmed)
}

// isLosslessJSON 是官方 `isRemoteJsonValue` 的 Go 直译（json-value.js:16-64）。
//
// 为什么不用 json.Valid：`json.Valid` 认 `1e999`（解出来是 +Inf）、也认 `-0`，
// 而官方这两个都拒。差一个都算"我们能过、官方客户端过不了"——那正是
// "宿主发错一个字段 = 用户连接断了"的成因。
//
// 判定（与官方逐条一致）：
//
//	null / string / bool           → 通过
//	number                         → 必须有限且不是 -0
//	array                          → 每个元素都通过
//	object                         → 每个值都通过（键天然是字符串）
//	其它（含 JSON 语法错）          → 拒
func isLosslessJSON(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeLossless(decoder)
	if err != nil {
		return false
	}
	// 尾部还有东西（`1 2`、`{} {}`）不是单个 JSON 值。
	if _, err := decoder.Token(); err == nil {
		return false
	}
	return value
}

// decodeLossless 递归判定一个 JSON 值，返回是否无损。
func decodeLossless(decoder *json.Decoder) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	switch typed := token.(type) {
	case nil:
		return true, nil
	case string, bool:
		return true, nil
	case json.Number:
		return losslessNumber(typed), nil
	case json.Delim:
		switch typed {
		case '[':
			for decoder.More() {
				ok, err := decodeLossless(decoder)
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}
			if _, err := decoder.Token(); err != nil { // 吃掉 ']'
				return false, err
			}
			return true, nil
		case '{':
			for decoder.More() {
				if _, err := decoder.Token(); err != nil { // 键
					return false, err
				}
				ok, err := decodeLossless(decoder)
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}
			if _, err := decoder.Token(); err != nil { // 吃掉 '}'
				return false, err
			}
			return true, nil
		}
		return false, nil
	default:
		return false, nil
	}
}

// losslessNumber 判定一个数字：有限、且不是 -0（官方 `!Object.is(value, -0)`）。
func losslessNumber(value json.Number) bool {
	parsed, err := value.Float64()
	if err != nil {
		return false
	}
	if math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return false
	}
	return !(parsed == 0 && math.Signbit(parsed))
}
