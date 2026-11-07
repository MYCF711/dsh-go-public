package spi

import "context"

// 会话上下文：让**工具**知道自己正在为哪个会话干活。
//
// 为什么必须走 ctx 而不是工具字段：工具是全局注册一次的，
// 一个进程里可能服务多个会话。要在工具里回答"我是谁"（比如读自己的信箱、
// 记自己的目标），只能靠每次调用带下来的 ctx。
type ctxKey int

const sessionKey ctxKey = 0

// WithSession 把会话 id 绑到 ctx 上；空 id 原样返回。
func WithSession(ctx context.Context, id string) context.Context {
	if ctx == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey, id)
}

// SessionFrom 取当前会话 id；没有就返回空串（工具据此给出明确的 Hint）。
func SessionFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(sessionKey).(string)
	return s
}

// RunIDKey 是"这一次 run 的标识"，与 SessionID 分开：
// 同一个会话里可能跑多次 run（每轮一次），信箱去重靠它。
const runIDKey ctxKey = 1

// WithRunID 绑定本次 run 的 id。
func WithRunID(ctx context.Context, id string) context.Context {
	if ctx == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, runIDKey, id)
}

// RunIDFrom 取本次 run 的 id。
func RunIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(runIDKey).(string)
	return s
}

// SubDispatch 让"工具里再调工具"走同一条受管通道。
//
// 为什么必须这样传，而不是让模块去 Inject(spi.ToolRunnerKey)：
// 那个 key 给出的是**没有审批的裸 Pipeline**（modules/tools 提供的是纯管道），
// 审批层 guardedRunner 是在 kernel 外面手搭的。模块若直接拿 toolrunner，
// 计划模式闸门和 spill 还在，**只有"问人"这一层静默消失**——
// 而"我自己编排十次写盘、一次都不问"正是最容易出事的形状。
//
// 所以：子调用一律拿 ctx 里这一份，由宿主在每轮入口放一次；
// 谁拿不到就别调（返回 Hint 说清"这个环境不支持编排"），不许自己找后门。
type SubDispatch struct {
	// Runner 是已经带审批的那一层（宿主放进来的是 guardedRunner 自己）。
	Runner Runner
	// Depth 是嵌套层数：workflow 里再调 workflow 要能被拦住。
	Depth int
}

const subDispatchKey ctxKey = 1

// MaxSubDepth 是允许的最大嵌套层数。超过就拒，避免自调用把栈打穿。
const MaxSubDepth = 3

// WithSubDispatch 把受管子调用通道绑到 ctx 上。
func WithSubDispatch(ctx context.Context, d SubDispatch) context.Context {
	if ctx == nil || d.Runner == nil {
		return ctx
	}
	return context.WithValue(ctx, subDispatchKey, d)
}

// SubDispatchFrom 取子调用通道；没有就返回零值（Runner 为 nil）。
func SubDispatchFrom(ctx context.Context) SubDispatch {
	if ctx == nil {
		return SubDispatch{}
	}
	d, _ := ctx.Value(subDispatchKey).(SubDispatch)
	return d
}
