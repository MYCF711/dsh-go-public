package spi

import "dshgo/internal/kernel"

// 服务键集中在这里：模块之间只经 spi 相遇，不互相 import 对方的包。
//
// 这是 R2「SPI 之外无通路」的具体化——如果两个模块需要共享一个键，
// 说明那个键本来就属于契约层，而不是某一方的实现细节。
var (
	ToolsKey      = kernel.NewKey[ToolRegistry]("tools")
	ToolRunnerKey = kernel.NewKey[Runner]("toolrunner")
	FSKey         = kernel.NewKey[FS]("fs")
	ExecKey       = kernel.NewKey[Exec]("exec")
	JobKey        = kernel.NewKey[Job]("jobs")
)

// HistoryKey 提供服务：在当前会话的**原始日志**里按关键词回查。
//
// 为什么需要它：压缩会把细节压进摘要，但日志是 append-only 的，
// 原文永远在。模型需要一条"压得掉也找得回来"的路。
var HistoryKey = kernel.NewKey[HistorySearch]("history")

// SearchKey 提供服务：网页搜索。
//
// 默认实现是"没配置"——它会明确报错并说清缺什么，而不是返回空结果。
var SearchKey = kernel.NewKey[SearchProvider]("search")

// SubagentKey 提供服务：派子代理。
//
// 没装这个服务时 `subagent` 工具必须明确报错——不能假装"派了"。
var SubagentKey = kernel.NewKey[SubagentRunner]("subagent")
