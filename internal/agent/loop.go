// Package agent 是 turn/step 双循环：一次用户输入 = 一个 turn，turn 里每一步 = 一次模型调用。
//
// 它只做编排，不实现任何东西：模型来自 Chat，工具来自 spi.Runner，
// 结果如何变成消息来自 Project。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"dshgo/internal/llm"
	"dshgo/internal/schema"
	"dshgo/internal/spi"
)

// Chat 是模型入口（消费方定义的窄接口，internal/llm 天然满足）。
type Chat interface {
	Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event) error) (*llm.Response, error)
}

// EventKind 是 agent 层的事件种类。写会话日志时按这些种类落盘。
type EventKind string

const (
	EventStepStart    EventKind = "step/start"
	EventReasoning    EventKind = "assistant/reasoning"
	EventText         EventKind = "assistant/text"
	EventAssistantMsg EventKind = "assistant/message"
	EventUserMsg      EventKind = "user/message"
	EventToolCall     EventKind = "tool/call"
	EventToolResult   EventKind = "tool/result"
	EventStepEnd      EventKind = "step/end"
	EventTurnEnd      EventKind = "turn/end"
	EventUsage        EventKind = "usage/update"
)

// Event 是循环里发生的一件事。
type Event struct {
	Kind       EventKind
	Turn, Step int
	Text       string // reasoning / text 增量
	CallID     string
	ToolName   string
	Arguments  string
	Result     *spi.Result
	IsError    bool
	Finish     string
	Err        error

	// 以下只在 EventAssistantMsg 上出现：一步的完整产物（写会话用）。
	Assistant *AssistantTurn

	// 以下只在 EventUserMsg 上出现：下一模型步边界接管的用户消息。
	User *UserMessage

	// 以下只在 EventUsage 上出现：累计用量与窗口大小。
	UsageUsed int
	UsageSize int
}

// AssistantTurn 是一步模型的完整输出。
type AssistantTurn struct {
	Content   string
	Reasoning string
	ToolCalls []llm.ToolCall
	Finish    string
	Usage     *llm.Usage
}

// UserMessage 是一条待注入当前 turn 的用户消息。Data 由调用方持久化，agent 不解释。
type UserMessage struct {
	Message llm.Message
	Data    any
}

// ErrSteerClosed 表示 steer 信箱已经关闭，不再接受消息。
var ErrSteerClosed = errors.New("agent: steer inbox closed")

// SteerInbox 在当前 turn 内按 FIFO 接收用户消息；Run 返回时关闭并清空未消费项。
type SteerInbox struct {
	mu     sync.Mutex
	items  []UserMessage
	closed bool
}

// NewSteerInbox 创建一个初始开放的 steer 信箱。
func NewSteerInbox() *SteerInbox {
	return &SteerInbox{}
}

// Submit 在信箱开放时追加一条消息；关闭后必定返回 ErrSteerClosed。
func (s *SteerInbox) Submit(item UserMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSteerClosed
	}
	s.items = append(s.items, item)
	return nil
}

// Close 幂等关闭信箱，并丢弃所有尚未消费的消息。
func (s *SteerInbox) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.items = nil
}

// drain 取出当前已有消息。信箱保持开放，Submit 可在下一边界被接管。
func (s *SteerInbox) drain() []UserMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.items
	s.items = nil
	return items
}

// drainOrClose 与 Submit 使用同一把锁：有消息就原子取走并继续 turn，
// 没有消息就关闭；此后的 Submit 不可能成功却错过收尾边界。
func (s *SteerInbox) drainOrClose() ([]UserMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		s.closed = true
		return nil, false
	}
	items := s.items
	s.items = nil
	return items, true
}

// Loop 是一次会话的驱动器。
type Loop struct {
	Chat   Chat
	Tools  spi.ToolRegistry
	Runner spi.Runner
	// Project 把工具结果压成模型可见的文本块（默认用 session.ProjectResult 的同类语义）。
	Project func(spi.Result) []spi.Block
	// Model 与系统提示由调用方给；Loop 不猜。
	Model string
	// Capacity 是调用方显式声明的容量；零值字段由 llm 的唯一解析点补全。
	// 未声明自定义模型的 MaxOutput 会进入常规请求的 MaxTokens。
	Capacity llm.ModelCapacity
	// Reasoning 是推理强度（网关的 reasoning_effort）。空 = 不带这个参数。
	// 不是所有模型都认它，所以只能由调用方显式开。
	Reasoning string
	System    string
	// CompactTools 为真时，工具表压掉参数里的 description。
	// 实测能省四成字节——但参数说明也帮模型填对参数，所以默认关，
	// 用 --tool-schema=compact 显式打开，并靠 A/B 数据决定要不要改默认。
	CompactTools bool
	// SessionID 绑到每一步的 ctx 上，让工具能回答"我在为哪个会话干活"。
	// 空字符串不算错——单会话场景（`dshd run`）本来就是匿名的。
	SessionID string
	MaxSteps  int
	// Steers 非空时，在当前 turn 的下一模型步边界注入用户消息。
	Steers *SteerInbox
	// Emit 观测量；返回错误即中断整个 turn。
	Emit func(Event) error
	// lastImages 暂存"上一步工具产出的图片"，由 runTool2 写、调用方读走即清。
	// 放在结构体上是因为 Run 会并发处理多个 tool call，但它们是顺序执行的。
	lastImages []llm.MessageImage
	// Compact 可选：每步开始前调用，返回可能被瘦身过的历史。
	//
	// 为什么放在 Loop 里：压缩是**策略**（选区间 + 生成摘要），
	// 但"什么时候压"必须由跑 turn 的人决定——它才知道预算和会话文件在哪。
	Compact func(ctx context.Context, step int, history []llm.Message) ([]llm.Message, error)
	// PreStep 可选：每一步模型请求**发出之前**调用（官方 0.2.2 #78
	// `session-checkpoint-policy` 的 `agent/pre-step` 钩子）。
	//
	// 官方原件（asar 只读解出，dsh-session-checkpoint-policy/lib/index.js:67-70）：
	//
	//	ctx.on("agent/pre-step", async ({ agent }, next) => {
	//	    await ctx.sessions.flush(agent.session);
	//	    return next();
	//	});
	//
	// 返回错误即中止本轮：模型请求一次都不派发（fail-closed）。放在这里而不是
	// 各入口各写一遍，是因为"每一步都要"这件事只由循环知道——入口只看得见第一步。
	PreStep func(ctx context.Context, step int) error
}

func (l *Loop) emit(ev Event) error {
	if l.Emit == nil {
		return nil
	}
	return l.Emit(ev)
}

func (l *Loop) maxSteps() int {
	if l.MaxSteps > 0 {
		return l.MaxSteps
	}
	return 32
}

// Run 跑一个 turn：直到模型不再请求工具且没有待接管 steer，或步数用尽。
//
// messages 会被追加（assistant 消息与 tool 结果都进历史），返回值是新历史。
func (l *Loop) Run(ctx context.Context, turn int, history []llm.Message) ([]llm.Message, error) {
	if l.Steers != nil {
		defer l.Steers.Close()
	}
	// 工具靠这个知道"我是哪个会话"；不设的话读信箱一类的工具只能拒绝。
	if l.SessionID != "" {
		ctx = spi.WithSession(ctx, l.SessionID)
	}
	if l.Chat == nil {
		return history, fmt.Errorf("agent: 没有配置 Chat")
	}
	// 模型在一轮内固定，容量也只解析一次；各 step 复用同一份结果，
	// 不因其它模型或会话的声明而改变。
	capacity := llm.ResolveModelCapacity(l.Model, l.Capacity)
	msgs := append([]llm.Message(nil), history...)
	if l.System != "" && (len(msgs) == 0 || msgs[0].Role != "system") {
		msgs = append([]llm.Message{{Role: "system", Content: l.System}}, msgs...)
	}

	for step := 1; step <= l.maxSteps(); step++ {
		if err := ctx.Err(); err != nil {
			return msgs, err
		}
		// 官方 #78 agent/pre-step：每一步模型请求之前先让请求前缀耐久。
		// 放在步首（steer / 压缩 / step-start 之前）：这一步里的**任何**模型
		// 请求（包括压缩用的侧查询）都在耐久边界之后；失败即整步不派发。
		if l.PreStep != nil {
			if err := l.PreStep(ctx, step); err != nil {
				return msgs, fmt.Errorf("第 %d 步请求前落盘失败，模型请求未派发: %w", step, err)
			}
		}
		if l.Steers != nil {
			var err error
			msgs, err = l.emitSteers(ctx, turn, step, msgs, l.Steers.drain())
			if err != nil {
				return msgs, err
			}
		}
		if l.Compact != nil {
			next, err := l.Compact(ctx, step, msgs)
			if err != nil {
				return msgs, err
			}
			if next != nil {
				msgs = next
			}
		}
		if err := ctx.Err(); err != nil {
			return msgs, err
		}
		if err := l.emit(Event{Kind: EventStepStart, Turn: turn, Step: step}); err != nil {
			return msgs, err
		}

		req := llm.Request{
			Model:     l.Model,
			Messages:  msgs,
			Tools:     l.toolDefs(),
			MaxTokens: capacity.MaxOutput,
			// 只有显式要了才带：默认不带，避免把不支持该参数的网关打挂。
			Reasoning: l.Reasoning,
		}
		resp, err := l.Chat.Stream(ctx, req, func(e llm.Event) error {
			switch e.Kind {
			case llm.EventReasoning:
				return l.emit(Event{Kind: EventReasoning, Turn: turn, Step: step, Text: e.Text})
			case llm.EventContent:
				return l.emit(Event{Kind: EventText, Turn: turn, Step: step, Text: e.Text})
			}
			return nil
		})
		if err != nil {
			if resp != nil {
				msgs = append(msgs, assistantMessage(resp))
			}
			return msgs, fmt.Errorf("第 %d 步模型调用失败: %w", step, err)
		}

		// 空响应不是"模型没话说"，是**这一次调用根本没成**。
		//
		// 真机踩过：网关列着一个模型 id，但那个上游是坏的（502 / 直接空流），
		// 而某些网关在这种情况下回的是 **200 + 空 SSE**。于是这一轮会被记成
		// "assistant 说了空话、turn completed"，退出码 0——用户看到的是
		// "模型什么都没说"，而真相是"这个模型不能用"。两种事的下一步完全不同。
		if err := emptyResponseErr(resp); err != nil {
			return msgs, fmt.Errorf("第 %d 步模型调用失败: %w", step, err)
		}

		// 这是**整个 completed response** 的执行闸门，不是单个 call 的参数检查。
		// 任一 call 的完整参数不是 JSON object，就在**任何** assistant 消息落历史、
		// assistant/message 事件、首个 pre-execute/审批/Runner.Run/EventToolCall、
		// 下一次模型请求之前终止整条响应；否则排在前面的合法 call 会已经产生副作用。
		// 作废的响应不写历史：它不是模型产出的一部分（下一次请求也不该看见它），
		// 也不补 tool/result——错误由调用方现有的 turn/error、turn/end{kind:error} 出口记录。
		if err := validateCompletedToolCalls(resp.ToolCalls); err != nil {
			return msgs, fmt.Errorf("第 %d 步响应终止: %w", step, err)
		}

		msgs = append(msgs, assistantMessage(resp))

		var usage *llm.Usage
		if resp.Usage.TotalTokens > 0 {
			u := resp.Usage
			usage = &u
		}
		if err := l.emit(Event{Kind: EventAssistantMsg, Turn: turn, Step: step, Finish: resp.FinishReason,
			Assistant: &AssistantTurn{
				Content: resp.Content, Reasoning: resp.Reasoning,
				ToolCalls: resp.ToolCalls, Finish: resp.FinishReason, Usage: usage,
			}}); err != nil {
			return msgs, err
		}

		if len(resp.ToolCalls) == 0 {
			// 无工具步在 drain-or-close 之前就已经完成；steer 只会开启下一步，
			// 不能把上一步的 step/end 留到 steer user 之后或最终 turn/end 才补造。
			if err := l.emit(Event{Kind: EventStepEnd, Turn: turn, Step: step}); err != nil {
				return msgs, err
			}
			var continueTurn bool
			msgs, continueTurn, err = l.drainSteersOrClose(ctx, turn, step, msgs)
			if err != nil {
				return msgs, err
			}
			if continueTurn {
				continue
			}
			if err := l.emit(Event{Kind: EventTurnEnd, Turn: turn, Step: step, Finish: resp.FinishReason}); err != nil {
				return msgs, err
			}
			return msgs, nil
		}

		stopAfter := false
		for _, call := range resp.ToolCalls {
			msg, stop := l.runTool2(ctx, turn, step, call)
			msgs = append(msgs, msg)
			// 带图的结果：图片不能塞进 tool 消息（OpenAI 兼容接口不支持），
			// 所以紧跟一条 user 消息把图送过去。少了这一步，模型只会看到
			// "读到了图片"这几个字，却看不见图——工具等于白跑。
			if img := l.lastImages; len(img) > 0 {
				l.lastImages = nil
				msgs = append(msgs, llm.Message{
					Role:    "user",
					Content: "（这是工具刚返回的图片）",
					Images:  img,
				})
			}
			if stop {
				stopAfter = true
			}
		}
		if err := l.emit(Event{Kind: EventStepEnd, Turn: turn, Step: step}); err != nil {
			return msgs, err
		}
		// 工具有话说"必须等人"：有 steer 就接管同 turn，没有才收尾。
		if stopAfter {
			var continueTurn bool
			msgs, continueTurn, err = l.drainSteersOrClose(ctx, turn, step, msgs)
			if err != nil {
				return msgs, err
			}
			if continueTurn {
				continue
			}
			if err := l.emit(Event{Kind: EventTurnEnd, Turn: turn, Step: step, Finish: "awaiting_user"}); err != nil {
				return msgs, err
			}
			return msgs, nil
		}
	}

	return msgs, fmt.Errorf("turn 用满了 %d 步仍未收敛（模型一直在请求工具）", l.maxSteps())
}

// emitSteers 在一个模型步边界按 FIFO 发事件；只有 emit 成功的项才进入模型历史。
func (l *Loop) emitSteers(ctx context.Context, turn, step int, msgs []llm.Message, items []UserMessage) ([]llm.Message, error) {
	for i := range items {
		if err := ctx.Err(); err != nil {
			return msgs, err
		}
		item := &items[i]
		if err := l.emit(Event{Kind: EventUserMsg, Turn: turn, Step: step, User: item}); err != nil {
			return msgs, err
		}
		msgs = append(msgs, item.Message)
	}
	return msgs, nil
}

// drainSteersOrClose 决定一个看似收尾的响应是否真的结束当前 turn。
// 有待接管的 steer 时，它们属于下一模型步，因此事件使用 step+1。
func (l *Loop) drainSteersOrClose(ctx context.Context, turn, step int, msgs []llm.Message) ([]llm.Message, bool, error) {
	if l.Steers == nil {
		return msgs, false, nil
	}
	items, ok := l.Steers.drainOrClose()
	if !ok {
		return msgs, false, nil
	}
	if step >= l.maxSteps() {
		return msgs, false, fmt.Errorf("turn 用满了 %d 步，仍有 steer 等待下一模型步", l.maxSteps())
	}
	msgs, err := l.emitSteers(ctx, turn, step+1, msgs, items)
	if err != nil {
		return msgs, false, err
	}
	return msgs, true, nil
}

// ValidateToolCallArguments 是「tool_call.arguments 必须是 JSON 对象」的**唯一**实现。
//
// 两个调用点，判据完全相同：
//   - Loop 在完整响应返回后、任何 assistant 消息/事件/审批/执行之前，对响应里
//     **全部** tool call 做一次预检（全有或全无）；
//   - tools.Pipeline 在 pre-execute 之后、Tool.Exec 之前做同一条判据的深防御。
//
// 为什么要说"唯一"：这两层各写一份时已经漂过一次——循环把 JSON `null` 判成
// malformed，流水线却放行（`json.Unmarshal` 到 map 对 `null` 会成功并留下 nil
// map）。同一个参数在两处得到两种结论，模型看到的行为就取决于走的是哪条路径。
//
// 判据只有一条：**有参数时**必须能 unmarshal 成 `map[string]json.RawMessage`
// 且结果非 nil。没有参数（空串/纯空白）不是"参数坏了"——零参数工具的 tool call
// 本来就可能是空串，把它当 malformed 会让整条响应作废。
func ValidateToolCallArguments(args json.RawMessage) error {
	if len(strings.TrimSpace(string(args))) == 0 {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(args, &probe); err != nil {
		return err
	}
	if probe == nil {
		return errors.New("得到 null")
	}
	return nil
}

// validateCompletedToolCalls 校验一次模型响应里的全部完整工具参数。
//
// 只判断 JSON object 这一层，不查工具名、schema 或 required 字段；那些仍由
// Runner 按原有语义处理（未知工具、缺必填字段都是**可恢复**的 tool 错误，
// 让模型下一步自我纠正，不能被这道格式闸门顺手改成整轮失败）。
func validateCompletedToolCalls(calls []llm.ToolCall) error {
	for _, call := range calls {
		if err := ValidateToolCallArguments(json.RawMessage(call.Function.Arguments)); err != nil {
			return fmt.Errorf("工具调用 %q 的参数不是 JSON 对象: %w", call.ID, err)
		}
	}
	return nil
}

// runTool 执行一次工具调用并返回给模型的 tool 消息。
func (l *Loop) runTool(ctx context.Context, turn, step int, call llm.ToolCall) llm.Message {
	msg, _ := l.runTool2(ctx, turn, step, call)
	return msg
}

func (l *Loop) runTool2(ctx context.Context, turn, step int, call llm.ToolCall) (llm.Message, bool) {
	args := call.Function.Arguments
	_ = l.emit(Event{
		Kind: EventToolCall, Turn: turn, Step: step,
		CallID: call.ID, ToolName: call.Function.Name, Arguments: args,
	})

	msg := llm.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name}
	if l.Runner == nil {
		msg.Content = "（宿主没有工具执行器）"
		return msg, false
	}
	res, err := l.Runner.Run(ctx, call.Function.Name, json.RawMessage(args))
	if err != nil {
		// 失败结果**也是**工具真实产出：从原 res 出发，保留
		// Text / Data / Artifacts / Truncated / View，只附加 err 与 IsError。
		// 新建一个只有 Summary/Hint 的空 Result 会把 actual body 整个丢掉——
		// 而失败时"它到底吐了什么"恰恰是模型判断下一步的唯一依据。
		// Hint 随原 res 保留；err 与 IsError 仍经 Event 显式传递。
		shown := res
		failure := "Error: " + err.Error()
		// 取消/中断 ≠ 失败：命令可能已经跑完了，只是我们没收到结果。
		// DSH 的措辞就是这个意思——"结果未知，你自己决定要不要重试"。
		// 说成 "failed" 会让模型以为命令没执行，从而做出错误判断。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			failure = "The tool call was interrupted after it was recorded, but no result was durably recorded. " +
				"Its outcome is unknown. Decide whether to retry from the tool's output or a safer read."
			shown.Hint = "结果未知：先读一下现场（文件/进程/日志）确认它到底做没做，再决定重试与否"
		}
		// 没有真实正文时，错误摘要就是模型能看到的全部内容。
		if shown.Text == "" {
			shown.Summary = failure
		}
		msg.Content = renderBlocks(l.project(shown))
		if shown.Text != "" {
			// ProjectResult 在 Text 非空时**只用** Text，会把错误摘要整条藏掉：
			// 模型于是只看到工具正文，以为这次调用成功了。失败既有"错在哪"，
			// 也有工具真实返回了什么，两样都要进模型消息。
			msg.Content = failure + "\n\n" + msg.Content
		}
		_ = l.emit(Event{
			Kind: EventToolResult, Turn: turn, Step: step,
			CallID: call.ID, ToolName: call.Function.Name, IsError: true, Err: err, Result: &shown,
		})
		return msg, false
	}
	msg.Content = renderBlocks(l.project(res))
	_ = l.emit(Event{
		Kind: EventToolResult, Turn: turn, Step: step,
		CallID: call.ID, ToolName: call.Function.Name, Result: &res,
	})
	if len(res.Images) > 0 {
		imgs := make([]llm.MessageImage, 0, len(res.Images))
		for _, im := range res.Images {
			imgs = append(imgs, llm.MessageImage{MIME: im.MIME, DataBase64: im.DataBase64})
		}
		l.lastImages = imgs
	}
	return msg, res.Stop
}

func (l *Loop) project(r spi.Result) []spi.Block {
	if l.Project != nil {
		return l.Project(r)
	}
	// 默认投影：摘要 + 提示；Data 交给上层（会话层有更完整的实现）。
	blocks := make([]spi.Block, 0, 2)
	if r.Summary != "" {
		blocks = append(blocks, spi.Block{Type: "text", Text: r.Summary})
	}
	if r.Hint != "" {
		blocks = append(blocks, spi.Block{Type: "text", Text: "提示：" + r.Hint})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, spi.Block{Type: "text", Text: "（无输出）"})
	}
	return blocks
}

func renderBlocks(blocks []spi.Block) string {
	var b strings.Builder
	for i, blk := range blocks {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(blk.Text)
	}
	return b.String()
}

// assistantMessage 把一次响应变成历史里的 assistant 消息。
func assistantMessage(resp *llm.Response) llm.Message {
	return llm.Message{
		Role:      "assistant",
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	}
}

func (l *Loop) toolDefs() []llm.ToolDef {
	if l.Tools == nil {
		return nil
	}
	tools := l.Tools.Tools()
	defs := make([]llm.ToolDef, 0, len(tools))
	for _, t := range tools {
		params := t.Schema()
		if l.CompactTools {
			params = CompactSchema(params)
		}
		defs = append(defs, llm.ToolDef{
			Type: "function",
			Function: llm.ToolFuncDef{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  params,
			},
		})
	}
	return defs
}

// CompactSchema 去掉 JSON schema 里所有 **annotation** description，只留结构。
//
// 为什么值得做：工具表 17 KB 里有 6.6 KB 是参数的说明文字与它们的键名开销。
// 保留结构（类型、enum、required）就够模型填对参数的情形很常见——
// 但"常见"不等于"总是"，所以这只是个开关，默认仍是完整表。
//
// 实现只有一份：internal/schema。这里留一个入口，只是为了让循环不 import
// 业务包之外的细节。规则本身（哪个 description 是注解、哪个是参数名）也
// 只有那一处判断——2026-11-04 那天，这个函数与 cmd/dshd 的 stripDescriptions
// 各自按「见 description 键就删」写了一遍，于是同一个 bug 在两处各犯一次：
// properties 里的参数名被当注解删掉，required 里却还点着它。
func CompactSchema(raw json.RawMessage) json.RawMessage {
	return schema.Compact(raw)
}

// emptyResponseErr 把"看起来成功、其实什么都没拿到"的响应判成错误。
//
// 判据只有一条：**正文、思考、工具调用三者皆空**。这时候无论是哪种 finish，
// 这一轮都没有任何可用的产出，继续走下去只会让模型对着空历史瞎猜。
// 分两种情况说清，因为下一步完全不同：
//   - finish=length：是被 max_tokens 截断了（把上限调大，不是换模型）
//   - 其它：这个模型 id 大概率不可用（`dshd models` 看一眼，别在这上面重试）
func emptyResponseErr(resp *llm.Response) error {
	if resp == nil {
		return fmt.Errorf("模型没有返回响应体（网关可能中途断了）：先 dshd doctor 看端点通不通")
	}
	if len(resp.Content) > 0 || len(resp.Reasoning) > 0 || len(resp.ToolCalls) > 0 {
		return nil
	}
	if resp.FinishReason == "length" {
		return fmt.Errorf("模型输出被 max_tokens 截断了，这一轮没有任何可用内容：把上限调大再试")
	}
	return fmt.Errorf("模型返回了空响应（finish=%q）：这个模型 id 可能不可用——用 dshd models 看有哪些、"+
		"或在 --model 上换一个；不要在这上面反复重试", resp.FinishReason)
}
