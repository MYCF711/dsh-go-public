// Package spi 是本仓的全部接口与数据类型，零实现。
//
// 规则：模块之间只能看见这里的类型；任何“模块 A 直接 import 模块 B”都是违规。
package spi

import (
	"context"
	"encoding/json"
)

// Tool 是模型可见的一个工具。这是本项目的产品本体（PLAN §7）。
//
// 三条硬约束由中间件统一实现，工具作者不要自己写：
//  1. 序列化层：Data → JSON，键名 snake_case；
//  2. 截断层：超阈值自动外置 artifact + 指针，替换体本身永不超阈值；
//  3. 提示层：error 必须带 Hint。
type Tool interface {
	// Name 是模型看到的工具名，全局唯一、稳定。
	Name() string
	// Description 是模型看到的「什么时候用它」——不是功能介绍。
	// 与 Schema 一样属于稳定性契约（R4）：改了要走快照。
	Description() string
	// Schema 必须稳定：不得含时间戳、绝对路径、随机 id。
	Schema() json.RawMessage
	// Caps 声明这个工具会对外部世界造成什么影响。
	//
	// **必填**：审批与沙箱都靠它判定，不许让它们按工具名猜（dsc 的反例：sandbox.go:52）。
	// 漏声明在编译期就会暴露，不会静默变成"只读"。
	Caps() Caps
	// Exec 执行。失败时必须通过 Result.Hint 给出下一步。
	Exec(ctx context.Context, args json.RawMessage) (Result, error)
}

// Caps 是工具的能力声明。
type Caps struct {
	Writes      bool `json:"writes,omitempty"`      // 会改文件系统
	Executes    bool `json:"executes,omitempty"`    // 会起子进程
	Network     bool `json:"network,omitempty"`     // 会访问网络
	Destructive bool `json:"destructive,omitempty"` // 可能造成不可恢复的后果
}

// NeedsApproval 是"要不要先问人"的判定：只读工具不问，其余都问。
func (c Caps) NeedsApproval() bool {
	return c.Writes || c.Executes || c.Network || c.Destructive
}

// Result 是宿主内部的工具结果契约。
//
// 注意与 DSH 的差异：DSH 只有 {isError, content[], meta?}，会话 v4 也只认这个形状。
// 所以结构化是我们的内部契约，v4 是**边界上的编码**——由投影函数把 Data 压成 content。
type Result struct {
	Summary string // 一行，给人看（TUI/桌面端）
	Data    any    // 结构化，给模型
	// Text 是**已经渲染好的**模型可见正文。非空时投影只用它，不再拼 Summary+JSON。
	// 用它来对齐 DSH 的呈现约定（例如 read 的 <path>/<content> 包裹）。
	Text      string
	Artifacts []Artifact  // 大结果外置后的引用
	Truncated *Truncation // 发生截断时的证据
	Hint      string      // 失败或为空时的下一步建议
	View      *ViewSpec   // 客户端渲染契约
	// Images 是这次结果附带的图片。宿主会把它们**挪进一条用户消息**再发给模型——
	// 因为 OpenAI 兼容接口的 tool 消息不支持图片，只有 user 消息能带。
	// 不写进会话日志：图片是二进制，塞进 append-only 的日志会把文件撑爆，
	// 而且日志是给人回看的，不是给模型复读的。
	Images []Image `json:"-"`
	// Stop 为 true 时，agent 循环在这一步之后**结束本轮**，把控制权交回人。
	// 给 ask_user_question 这类"必须等人回答"的工具用：模型不能自己编一个答案往下走。
	Stop bool
}

// Image 是一张要送给模型看的图片。
//
// DataBase64 是**裸 base64**（不带 data: 前缀）——前缀由发送方按需拼，
// 这样存进会话日志时不必重复存一份 URL 形式的元数据。
type Image struct {
	MIME       string
	DataBase64 string
}

// Artifact 是一份被外置的大结果。
type Artifact struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// Truncation 记录“保留了多少、总共多少、去哪儿取”。
type Truncation struct {
	Kept    int    `json:"kept"`
	Total   int    `json:"total"`
	Pointer string `json:"pointer"`
}

// ViewKind 是三种版式。
type ViewKind string

const (
	ViewCard  ViewKind = "card"
	ViewTable ViewKind = "table"
	ViewPlain ViewKind = "plain"
)

// ViewSpec 是给客户端（TUI / 桌面端）的结构化视图声明。
// 未声明时客户端退化为键值卡片，再退化为原文。
type ViewSpec struct {
	Kind    ViewKind   `json:"kind"`
	Title   string     `json:"title,omitempty"`
	Badge   string     `json:"badge,omitempty"`
	Fields  []Field    `json:"fields,omitempty"`
	Columns []string   `json:"columns,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`
	Body    string     `json:"body,omitempty"`
}

// Field 是 card 版式的一行键值。
type Field struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ToolRegistry 是工具目录。中间件链与可见性都由它负责，工具自己不管。
type ToolRegistry interface {
	Register(Tool) error
	Tools() []Tool
	Get(name string) (Tool, bool)
}

// Runner 是**唯一**允许执行工具的入口。绕过它直接调 Tool.Exec 是违规：
// 审批、沙箱、超时、截断、提示、事件广播全在 Runner 里。
type Runner interface {
	Run(ctx context.Context, name string, args json.RawMessage) (Result, error)
}

// Call 是穿过工具流水线的可变载荷（waterfall 可就地改写）。
type Call struct {
	Name string
	Args json.RawMessage
	// Session 是发起这次调用所属的会话 id。
	//
	// 为什么 Call 上要带它：pre-execute 上的拦截器（比如计划模式闸门）
	// 需要知道"这是哪个会话在调"，而 Call 原本没有 ctx——拿不到会话就只能
	// 做一个进程级开关，那会让 A 会话进计划模式把 B 会话也锁住。
	Session string
	// Nested 表示这次调用是"工具里再调工具"（例如 workflow 的子调用）。
	//
	// 谁需要它：宿主写 v4 时只给**顶层**调用写（单写者，R5）。
	// 子调用若也写入，必须能被区分出来，否则同一次调用会被记两遍。
	Nested bool
	Result Result
	Err    error
}

// 工具流水线的四个槽。顺序固定，监听器按注册顺序执行。
const (
	EventToolPreExecute  = "tools/pre-execute"
	EventToolExecute     = "tools/execute"
	EventToolPostExecute = "tools/post-execute"
	EventToolResult      = "tools/result"
)

// Block 是模型可见的内容块（agent 层投影的产物，会话层原样落盘）。
type Block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
