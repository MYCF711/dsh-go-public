package spi

import "context"

// SubagentRunner 是"派一个子代理去干一件独立的事"这一能力的抽象。
//
// 为什么是 SPI：子代理要跑一整个 turn（模型 + 工具 + 系统提示），这些东西
// 只有宿主装配层拿得到，工具本身够不着。做成服务之后，工具只管提问与收结果，
// "怎么跑"留给宿主——换一种跑法（同步/异步/换模型）不用改工具。
type SubagentRunner interface {
	RunSub(ctx context.Context, req SubagentRequest) (SubagentResult, error)
}

// SubagentRequest 是一次委派。
type SubagentRequest struct {
	Prompt      string // 完整、自包含的任务描述
	Description string // 3-5 个词的短描述，给人看
	Cwd         string // 子代理的初始工作目录；空表示继承
	// InheritSession 非空时，子代理的初始历史从 <这个会话> 读最近 N 条。
	// 空 = 不带历史（现有行为，一个字节都不多传）。
	InheritSession string
	// Inherit 是已读好的父会话片段（由宿主填入）。有它就不必再给 InheritSession。
	Inherit string

	// Continuable 为真表示这次委派要留下一条**可续问**的子会话。
	//
	// 为什么需要：官方 `subagents.prompt` 的地址是一个
	// (parentSessionId, childSessionId) 控制对，而这个 child 只能由一次真实
	// spawn 产生。同步委派（本仓原来的唯一形态）跑完就散，没有地址可投，
	// 所以那个方法在真机上只能是 no_such_session。
	//
	// 语义（宿主实现必须逐条兑现）：
	//   - 真建一条 durable v4 子会话，并在子会话日志里写 `subagent/spawn`
	//     父子帧（childSessionId / parentSessionId / cwd / spawnedAt）；
	//   - 把这次委派的真实往返写进子会话历史（一条 user + 一条 assistant），
	//     这样续问时子代理记得自己做过什么；
	//   - 只有**真的建成了**子会话才回 SubagentResult.ChildSessionID；
	//     建不成就是错误——不许回一个指向空空如也的 id。
	//   - 默认 false：一个字节都不多写，既有调用方的行为与线上形状完全不变。
	Continuable bool
	// ChildSessionID 让调用方指定子会话 id（空 = 宿主分配）。
	// 非空即意味着 Continuable 语义（对齐官方"identity minted before the call"）。
	ChildSessionID string
	// ParentSessionID 是显式父会话 id；空 = 取 ctx 上的会话身份
	// （spi.SessionFrom）。两者都没有时 Continuable 必须**响亮失败**：
	// 没有父就没有授权边界，编一个父等于没有归属。
	ParentSessionID string
}

// SubagentResult 是子代理交回来的东西：**只要结果，不要中间步骤**。
type SubagentResult struct {
	Text   string // 子代理最后的正文
	Steps  int    // 用了几步
	Calls  int    // 调了几次工具
	Tokens struct {
		In  int
		Out int
	}
	// ChildSessionID 非空表示这次委派留下了一条**可续问**的子会话
	// （= Continuable 真的生效了：durable v4 会话已落盘、父子帧已写、
	// 这次委派已进子会话历史）。空 = 这次委派不可续问。
	//
	// 它是**地址**，不是承诺：`subagents.prompt` 能否投进去，还要看投递通道
	// 是否接受（投不进就出错，不会回一个假的 messageId）。
	ChildSessionID string
	// BorrowedApprovals 为真表示子代理**沿用了主会话的审批记忆**：
	// 主会话里被"永远允许"过的工具，子代理不会再问一次。
	//
	// 这是有意的（同一个人、同一件事，问两遍纯属打扰），但它是一次
	// **权限面的扩大**——模型自己派出去的子代理，用的是你刚才批准的授权。
	// 所以必须回传给调用方，让它出现在 Summary 里，而不是埋在行为里。
	BorrowedApprovals bool
}
