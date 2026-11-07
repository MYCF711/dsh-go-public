package officialwire

import "encoding/json"

// Descriptor 是官方 Typert 方法参数描述符在本仓的**字段集投影**。
//
// 证据：GW/index.js:1776-1788 —— `args` 的字段集必须与 descriptor 完全一致，
// 缺必填或多未知字段都是 `gateway/arguments-invalid`；官方桌面端对 mux 帧更是
// exact-key（多一个键就 1008 断连，EVIDENCE §3.1）。
//
// 本仓拿到的是**字段集**而不是完整的 zod schema（U5 未取证：官方 descriptor 的
// `mode`/`lookup`/默认值没有全量导出）。所以这里只校字段集，不校字段类型——
// 类型校验在真实 handler 里（它读的就是同一批字段）。
type Descriptor struct {
	// Required 是必填字段名。
	Required []string
	// Optional 是可出现但可省略的字段名。
	Optional []string
}

// Validate 做 exact-key 校验：多一个字段、少一个必填字段都拒。
//
// endpoint 只用于 details，不参与判断。
func (d Descriptor) Validate(endpoint string, args map[string]json.RawMessage) *Error {
	allowed := make(map[string]struct{}, len(d.Required)+len(d.Optional))
	for _, name := range d.Required {
		allowed[name] = struct{}{}
	}
	for _, name := range d.Optional {
		allowed[name] = struct{}{}
	}
	for name := range args {
		if _, ok := allowed[name]; !ok {
			return ArgumentsInvalid(endpoint, name,
				"官方 descriptor 里没有这个参数："+name+"（字段集必须完全一致，多一个字段官方客户端会判 arguments-invalid）")
		}
	}
	for _, name := range d.Required {
		if _, ok := args[name]; !ok {
			return ArgumentsInvalid(endpoint, name, "官方 descriptor 要求必填参数："+name)
		}
	}
	return nil
}

// Fields 返回 descriptor 声明的全部字段（按 required→optional 顺序）。
func (d Descriptor) Fields() []string {
	out := make([]string, 0, len(d.Required)+len(d.Optional))
	out = append(out, d.Required...)
	out = append(out, d.Optional...)
	return out
}

// Validate 是 Descriptor 的 nil 安全入口：descriptor 为 nil 时只允许空 args。
//
// 为什么要这条：没登记 descriptor 的方法不该被静默放过任意字段——那等于
// "宿主没做出校验" 被当成 "什么参数都对"。真要放行就显式给一个空 Descriptor。
func ValidateArgs(d *Descriptor, endpoint string, args map[string]json.RawMessage) *Error {
	if d == nil {
		if len(args) == 0 {
			return nil
		}
		names := make([]string, 0, len(args))
		for name := range args {
			names = append(names, name)
		}
		return ArgumentsInvalid(endpoint, names[0], "这个方法没有登记 descriptor，但请求带了参数")
	}
	return d.Validate(endpoint, args)
}
