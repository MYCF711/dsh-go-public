package llm

import (
	"fmt"
	"strings"
)

// GatewayErrorKind 区分"连不上"和"网关回了个错误状态"。
//
// 两类问题的下一步动作不同：前者去查网关起没起、地址对不对；
// 后者看状态码——401 是 key 的问题，5xx 是网关自己的问题。
type GatewayErrorKind int

const (
	GatewayConnect GatewayErrorKind = iota
	GatewayStatus
)

// GatewayError 是给**协议层看**的错误：短、可执行、不含内部细节。
//
// 原始错误留着（Unwrap），但不出现在 Error() 里——
// 客户端不需要知道 `dial tcp 127.0.0.1:1: connectex: ...`。
type GatewayError struct {
	Kind GatewayErrorKind
	Base string // 只到 base，不带 /chat/completions
	Code int
	Body string
	Err  error
	// Attempts 是这条请求实际发出的次数：1 表示没有重试（400/422 以外一律是 1）。
	Attempts int
	// Adjust 是协商里做过的可选字段回退，例如「去掉 stream_options」。
	// 它是给人看的：客户端的下一步动作取决于"到底哪个字段被端点拒了"。
	Adjust []string
}

// base 去掉尾斜杠，免得文案里出现 `http://x/v1/：`
func (e *GatewayError) base() string {
	return strings.TrimSuffix(strings.TrimSuffix(e.Base, "/"), "/chat/completions")
}

func (e *GatewayError) Error() string {
	switch e.Kind {
	case GatewayConnect:
		return "连不上模型网关 " + e.base() + "：确认网关在跑、--base 地址对"
	case GatewayStatus:
		switch e.Code {
		case 401, 403:
			return fmt.Sprintf("模型网关拒绝认证（%d）：检查 API key 与 --base", e.Code)
		case 404:
			return "模型网关没有这个接口（404）：--base 是不是少了 /v1？"
		case 429:
			return "模型网关限流（429）：稍等再试"
		}
		if e.Code >= 500 {
			return fmt.Sprintf("模型网关出错（%d）", e.Code)
		}
		msg := fmt.Sprintf("模型网关返回 %d", e.Code)
		if b := strings.TrimSpace(e.Body); b != "" {
			if r := []rune(b); len(r) > 200 {
				b = string(r[:200]) + "…"
			}
			msg += "：" + b
		}
		if len(e.Adjust) > 0 {
			// 回退过还失败，必须说清试过什么：否则客户端只会看到"还是 400"，
			// 不知道该去改哪个开关（或者该换个模型/端点）。
			msg += fmt.Sprintf("（已做 %d 次可选字段回退：%s；最后一次响应同上）",
				len(e.Adjust), strings.Join(e.Adjust, "、"))
		}
		return msg
	}
	return "模型网关调用失败"
}

// Unwrap 让日志和 errors.Is/As 还能看到底层原因。
func (e *GatewayError) Unwrap() error { return e.Err }
