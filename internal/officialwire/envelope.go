package officialwire

import (
	"encoding/json"
	"strconv"
)

// 请求信封的四个键（EVIDENCE §2.1：clientRequestSchema 是 exact-key）。
const (
	fieldType    = "type"
	fieldRPCID   = "rpcId"
	fieldMethod  = "method"
	fieldPayload = "payload"
	fieldArgs    = "args"

	requestType  = "client-request"
	responseType = "server-response"
)

// Request 是解析后的 client-request。
//
// RPCID 在**任何**解析错误下都尽量填好：官方客户端拿响应的 rpcId 与自己的请求
// 逐字比对（EVIDENCE §2.2 "客户端校验响应 rpcId 必须回显同一个值"），
// 回一个空串等于把"参数错"变成"连接协议错"。
type Request struct {
	// Endpoint 是从 URL 推出来的 `<namespace>/<method>`。
	Endpoint string
	// Method 是 body 里的 method 字段（必须与 Endpoint 逐字相等）。
	Method string
	// RPCID 是客户端生成的 id，响应必须逐字回显。
	RPCID string
	// ArgsRaw 是 payload.args 的原始字节。
	ArgsRaw json.RawMessage
	// Args 是 args 对象解出来的键值（供 Descriptor 做 exact-key 校验）。
	Args map[string]json.RawMessage
}

// ParseClientRequest 解析并严格校验一个 client-request。
//
// 返回的 *Request **可能是非 nil 而 *Error 也非 nil**：那种情况下请求本身不合法，
// 但 rpcId 已经解出来，调用方应当用 200 + `{ok:false}` 回它（EVIDENCE §2.2、§6）。
// body 连 JSON 对象都不是时才返回 (nil, err)——那一条按官方走 400（EVIDENCE §6）。
//
// 每一次拒绝都只带一个 details.field，别把多个问题混在一条里：官方客户端只会
// 显示第一个 field。
func ParseClientRequest(endpoint string, body []byte) (*Request, *Error) {
	req := &Request{Endpoint: endpoint}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		// 不是 JSON 对象：400 的判据来自解码失败，不是信封校验（EVIDENCE §6）。
		return nil, BadRequestf("请求体不是 JSON 对象")
	}
	// 先把 rpcId 解出来：后面任何一条失败都要能回显它。
	if raw, ok := top[fieldRPCID]; ok {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			req.RPCID = id
		}
	}

	// 1. 顶层 exact-key：四个键，一个不多一个不少。
	allowed := map[string]struct{}{
		fieldType: {}, fieldRPCID: {}, fieldMethod: {}, fieldPayload: {},
	}
	for key := range top {
		if _, ok := allowed[key]; !ok {
			return req, envelopeError(endpoint, key, "client-request 里没有这个字段")
		}
	}
	for _, key := range []string{fieldType, fieldRPCID, fieldMethod, fieldPayload} {
		if _, ok := top[key]; !ok {
			return req, envelopeError(endpoint, key, "client-request 缺必填字段")
		}
	}

	// 2. type 必须是 client-request。
	var typ string
	if err := json.Unmarshal(top[fieldType], &typ); err != nil || typ != requestType {
		return req, envelopeError(endpoint, fieldType, "type 必须是 client-request")
	}

	// 3. rpcId 必须是字符串（zod: z.string()）。
	if err := json.Unmarshal(top[fieldRPCID], &req.RPCID); err != nil {
		return req, envelopeError(endpoint, fieldRPCID, "rpcId 必须是字符串")
	}

	// 4. method 必须与 URL endpoint 逐字相等（EVIDENCE §2.1，仍是 200）。
	var method string
	if err := json.Unmarshal(top[fieldMethod], &method); err != nil {
		return req, envelopeError(endpoint, fieldMethod, "method 必须是字符串")
	}
	req.Method = method
	if method != endpoint {
		return req, envelopeError(endpoint, fieldMethod,
			"method 与 URL endpoint 不一致："+strconv.Quote(method)+" ≠ "+strconv.Quote(endpoint))
	}

	// 5. payload 必须**恰好**有一个 args 字段（EVIDENCE §2.1，GW/index.js:1496）。
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(top[fieldPayload], &payload); err != nil || payload == nil {
		return req, envelopeError(endpoint, fieldPayload, "payload 必须是对象")
	}
	for key := range payload {
		if key != fieldArgs {
			return req, envelopeError(endpoint, fieldPayload+"."+key, "payload 只允许一个 args 字段")
		}
	}
	rawArgs, ok := payload[fieldArgs]
	if !ok {
		return req, envelopeError(endpoint, fieldPayload+"."+fieldArgs, "payload 缺 args")
	}

	// 6. args 必须是普通对象（数组/null/标量都不是）。
	if err := json.Unmarshal(rawArgs, &req.Args); err != nil || req.Args == nil {
		return req, envelopeError(endpoint, fieldPayload+"."+fieldArgs, "args 必须是对象")
	}
	req.ArgsRaw = append(json.RawMessage(nil), rawArgs...)
	return req, nil
}

func envelopeError(endpoint, field, message string) *Error {
	return &Error{
		Code:    CodeBadRequest,
		Message: message,
		Details: map[string]any{"endpoint": endpoint, "field": field},
	}
}

// resultBody 是官方 `result` 的联合：`{ok:true,value?}` 或 `{ok:false,error}`。
type resultBody struct {
	OK    bool   `json:"ok"`
	Value any    `json:"value,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// responseEnvelope 是官方 server-response。
//
// Attachments 只在有二进制附件时出现（EVIDENCE §5.1）；它是 metadata JSON 里
// 多出来的那个顶层键，二进制本体走 multipart 的 bytes-N part。
type responseEnvelope struct {
	Type        string       `json:"type"`
	RPCID       string       `json:"rpcId"`
	Result      resultBody   `json:"result"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// NewSuccess 造一个成功响应。
func NewSuccess(rpcID string, value any) responseEnvelope {
	return responseEnvelope{
		Type:   responseType,
		RPCID:  rpcID,
		Result: resultBody{OK: true, Value: value},
	}
}

// NewFailure 造一个业务失败响应（HTTP 仍然是 200）。
func NewFailure(rpcID string, err *Error) responseEnvelope {
	return responseEnvelope{
		Type:   responseType,
		RPCID:  rpcID,
		Result: resultBody{OK: false, Error: err},
	}
}
