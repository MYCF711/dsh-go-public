package officialwire

import (
	"encoding/json"
	"fmt"
)

// 官方 gateway 错误码。EVIDENCE §2.3 明确「没有中心枚举」——这串常量只是
// 我们在本包里用到的那些，照 GW/index.js 全文字面量抄下来，不许自己发明同义词。
const (
	// CodeBadRequest 是信封/形状层面的拒绝（GW/index.js:1496、:907-914）。
	CodeBadRequest = "gateway/bad-request"
	// CodeArgumentsInvalid 是 args 字段集与 descriptor 不一致（GW/index.js:1776-1788）。
	CodeArgumentsInvalid = "gateway/arguments-invalid"
	// CodeUnknownMethod 是任务书指定码：官方码空间没有中心枚举，桌面端只按
	// "ok:false + code" 分支处理，因此这个码不会与官方已有码冲突。
	// 语义：URL endpoint 合法但本宿主没接这个方法（**区别于**已接但当前不可用）。
	CodeUnknownMethod = "gateway/unknown-method"
	// CodeMethodUnavailable 表示方法在官方面上存在、但本宿主这一侧没有真实能力
	// （EVIDENCE §2.3 的 gateway/method-unavailable）。
	CodeMethodUnavailable = "gateway/method-unavailable"
	// CodeProtocol 是帧/流协议违规（end 之后再 item 等，GW/index.js:598-607）。
	CodeProtocol = "gateway/protocol"
	// CodeUplinkOverflow 是单条流上行超过 streamInboxBytes（默认 256 KiB）。
	CodeUplinkOverflow = "gateway/uplink-overflow"
	// CodeInternal 是宿主侧异常折叠（非 RemoteError 一律折成它，GW/index.js:1704-1725）。
	CodeInternal = "gateway/internal"
	// CodeCancelled 是流被取消（GW/index.js:461-463, 477-480）。
	CodeCancelled = "gateway/cancelled"
	// CodePeerDisposed 是 Peer 作用域已释放（GW/index.js:679-683 附近的 peer disposed）。
	CodePeerDisposed = "gateway/peer-disposed"
	// CodeInteractionsForbidden 是 interactions 不是 answer 的 Peer 想收/答转发事件。
	CodeInteractionsForbidden = "gateway/interactions-forbidden"
	// CodeAccessDenied 是访问控制拒绝（GW/index.js:1217-1220，details{action,reason}）。
	CodeAccessDenied = "remote-access/denied"
)

// Error 是官方 `result.error` 的形状：`{code,message,details}`。
//
// details 必须是 record<string,unknown>（EVIDENCE §2.2）——所以它**永远不会
// 序列化成 null**：MarshalJSON 在 nil 时补 `{}`。少一个 details 字段就是
// 官方 zod 校验失败（客户端会整条响应判为 invalid）。
type Error struct {
	Code    string
	Message string
	Details map[string]any
}

// Error 让 *Error 可以直接当 error 用（宿主适配层要 err 时不必包一层）。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

// WithDetail 返回一个带额外 details 的副本（不改原对象）。
func (e *Error) WithDetail(key string, value any) *Error {
	if e == nil {
		return nil
	}
	details := make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		details[k] = v
	}
	details[key] = value
	return &Error{Code: e.Code, Message: e.Message, Details: details}
}

// MarshalJSON 保证 details 一定是对象。
func (e *Error) MarshalJSON() ([]byte, error) {
	type wire struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	return json.Marshal(wire{Code: e.Code, Message: e.Message, Details: details})
}

// NewError 造一个官方错误。
func NewError(code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

// Errorf 是带格式化的 NewError。
func Errorf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// BadRequestf 是 gateway/bad-request 的快捷方式。
func BadRequestf(format string, a ...any) *Error {
	return Errorf(CodeBadRequest, format, a...)
}

// ArgumentsInvalid 是 args 字段集不一致的唯一构造点。
//
// details 带 endpoint/field：官方 TypertGatewayError 的 details 就是 `{endpoint, field?}`
// （EVIDENCE §2.2），客户端靠它说出"哪个参数不对"。
func ArgumentsInvalid(endpoint, field, message string) *Error {
	return &Error{
		Code:    CodeArgumentsInvalid,
		Message: message,
		Details: map[string]any{"endpoint": endpoint, "field": field},
	}
}

// MethodUnavailable 表示方法在官方面上存在但本宿主没有真实能力。
func MethodUnavailable(endpoint, detail string) *Error {
	return &Error{
		Code:    CodeMethodUnavailable,
		Message: detail,
		Details: map[string]any{"endpoint": endpoint},
	}
}

// UnknownMethod 是「URL 合法、方法没接」的唯一构造点。
func UnknownMethod(endpoint string) *Error {
	return &Error{
		Code:    CodeUnknownMethod,
		Message: "本宿主没有挂载这个官方方法：" + endpoint,
		Details: map[string]any{"endpoint": endpoint},
	}
}

// TranslateLegacy 把 L1 的四字段错误翻成官方码空间。
//
// 为什么必须翻：L1 的码（no_such_session / not_implemented / bad_request …）在
// 官方客户端里没有分支；不翻就等于"业务失败但客户端看不出是什么失败"。
//
// 映射规则（按证据 §6 的"业务失败 200 + 状态码只表达传输与认证"设计）：
//
//	401            → remote-access/denied{action:request, reason:unauthorized}
//	403            → remote-access/denied{action:request, reason:forbidden}
//	400            → gateway/arguments-invalid（参数问题，客户端改参数才有用）
//	404            → <namespace>/not-found
//	409            → <namespace>/conflict
//	501            → gateway/method-unavailable（宿主没接线，不是客户端错）
//	>=500          → gateway/internal（宿主侧未知错误，可重试）
//	其它           → gateway/internal（宁可报宿主错，也不把未知码伪装成业务码）
//
// namespace 是被调 endpoint 的第一段（`session/page` → `session`）。
func TranslateLegacy(namespace string, status int, legacyCode, message string) *Error {
	details := map[string]any{"legacyCode": legacyCode, "legacyStatus": status}
	if namespace == "" {
		namespace = "gateway"
	}
	switch {
	case status == 401:
		return &Error{Code: CodeAccessDenied, Message: message,
			Details: map[string]any{"action": "request", "reason": "unauthorized"}}
	case status == 403:
		return &Error{Code: CodeAccessDenied, Message: message,
			Details: map[string]any{"action": "request", "reason": "forbidden"}}
	case status == 400:
		return &Error{Code: CodeArgumentsInvalid, Message: message, Details: details}
	case status == 404:
		return &Error{Code: namespace + "/not-found", Message: message, Details: details}
	case status == 409:
		return &Error{Code: namespace + "/conflict", Message: message, Details: details}
	case status == 501 || legacyCode == "not_implemented":
		return &Error{Code: CodeMethodUnavailable, Message: message, Details: details}
	default:
		return &Error{Code: CodeInternal, Message: message, Details: details}
	}
}
