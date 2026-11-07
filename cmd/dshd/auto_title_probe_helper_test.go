package main

import "strings"

// autoTitleWireMessage 是探针侧看到的一条模型请求消息。
type autoTitleWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// isAutoTitleWireRequest 判断一次模型请求是不是自动标题调用（官方 0.2.2 #14
// session-title-first-prompt-llm）。
//
// 为什么需要它：自动标题是首轮正常收口之后的**额外一次真实模型调用**，与主 turn
// 同路由、同凭据。任何按「第 N 个请求 / 请求顺序」断言主 turn 的探针，都必须把这次
// 调用排除在计数之外，否则拿到的是标题请求（reasoning 为空、模型可能回退到 legacy
// 路由），从而把官方行为误报成产品缺陷。
//
// 判据只用官方 systemPrompt 的原文前缀：标题请求的系统提示逐字来自官方，主 turn 的
// 系统提示不会以它开头。
func isAutoTitleWireRequest(messages []autoTitleWireMessage) bool {
	for _, message := range messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, "Create a concise title for an AI coding-assistant session") {
			return true
		}
	}
	return false
}
