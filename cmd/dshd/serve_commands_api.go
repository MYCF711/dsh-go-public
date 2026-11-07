package main

// 宿主侧**真实**命令注册表的装配点（`commands.list` / `commands.execute`）。
//
// # 为什么需要这一层
//
// 本宿主原来没有命令注册表：TUI 的 `/help` `/new` `/compact` 都在
// `cmd/dshtui` 里就地处理，宿主这一侧只有 transport 方法名。所以
// `commands.list` 只能回空数组、`commands.execute` 只能明确 501。
//
// 这里落一份**真注册表**（`transport.OfficialCommandRegistry`：可插拔、
// 可列出、生命周期记账、附件准入），并把能在本宿主落到**真实状态**的官方
// 内置命令注册进去。官方 builtin 是
// `goal / plan / feedback / compact / permission / export`（见
// `@deepseek-ai/dsh-commands/lib/types/builtin.js`）；本宿主里
// `plan`（计划模式）、`compact`（压缩）、`permission`（权限预设）、
// `export`（导出）各自缺一条真实依赖（见 RESULT 的"未接线"），
// **不注册它们**——注册一个只会回"未实现"的命令，等于让客户端以为能用。
//
// 因此这里注册的是能**真的做事**的那几个：
//
//	/help                列出当前注册表（真实发现）
//	/goal                显示本会话的 goal（真实 goal 存储）
//	/feedback <正文>     记录一条会话反馈（真实 durable 写入）
//
// 三条都不是"复制 TUI 命令表"：它们的 handler 走的是宿主真实状态。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// 宿主内置命令名（不带斜杠，官方字符集是小写）。
const (
	hostCommandHelp     = "help"
	hostCommandGoal     = "goal"
	hostCommandFeedback = "feedback"
)

// commandLifecycleMu 串行化 v4 生命周期追加。
//
// 为什么需要：`session.OpenSessionAt` 抢的是**文件**写锁
// （`<session>.lock`），两个并发 append 会互相撞成"会话正被另一个进程写入"。
// 锁的作用域是进程内，因此这一层的并发由这把互斥锁兜住；跨进程仍由文件锁
// 负责（撞上就重试，重试完还失败就**响亮报错**，绝不静默丢帧）。
var commandLifecycleMu sync.Mutex

// commandLifecycleRetries 是抢写锁的重试次数（写锁持有时间很短）。
const commandLifecycleRetries = 20

// newHostCommandRegistry 把真实命令装配成注册表。
func newHostCommandRegistry(api *agentOfficialAPI) (*transport.OfficialCommandRegistry, error) {
	if api == nil {
		return nil, fmt.Errorf("命令注册表没有宿主对象")
	}
	registry := transport.NewOfficialCommandRegistry(transport.OfficialCommandRegistryOptions{
		Lifecycle: func(sessionID, eventType string, data any) (int64, error) {
			return appendCommandLifecycle(api.deps.src, sessionID, eventType, data)
		},
	})

	if _, err := registry.Register(transport.OfficialCommandDefinition{
		DefinitionID: hostCommandGoal,
		Name:         hostCommandGoal,
		Description:  "显示本会话的目标（阶段 / 轮次 / 剩余轮数）",
		Input:        &transport.OfficialCommandInput{Hint: "（goal 不需要参数）"},
		Handler:      api.commandGoal,
	}); err != nil {
		return nil, err
	}

	if _, err := registry.Register(transport.OfficialCommandDefinition{
		DefinitionID: hostCommandFeedback,
		Name:         hostCommandFeedback,
		Description:  "记录一条会话反馈（落 durable 存储）",
		Input:        &transport.OfficialCommandInput{Hint: "想反馈的内容"},
		Handler:      api.commandFeedback,
	}); err != nil {
		return nil, err
	}

	// help 最后注册：它要列出**包含前面那些**的当前注册表。
	helpers := &hostCommandHelpHandler{registry: registry}
	if _, err := registry.Register(transport.OfficialCommandDefinition{
		DefinitionID: hostCommandHelp,
		Name:         hostCommandHelp,
		Description:  "列出所有可用命令",
		Handler:      helpers.run,
	}); err != nil {
		return nil, err
	}
	return registry, nil
}

// hostCommandHelpHandler 需要一个"注册表已存在"的引用，所以单独立一个类型。
type hostCommandHelpHandler struct {
	registry *transport.OfficialCommandRegistry
}

// run 列出当前注册表。
//
// 刻意**不再列一份硬编码清单**：清单的真值就是注册表本身，复制一份必然漂移。
func (h *hostCommandHelpHandler) run(_ context.Context, _ transport.OfficialCommandInvocation) (transport.OfficialCommandOutcome, error) {
	var b strings.Builder
	b.WriteString("可用命令：\n")
	for _, cmd := range h.registry.List() {
		b.WriteString("  /")
		b.WriteString(cmd.Name)
		b.WriteString("  ")
		b.WriteString(cmd.Description)
		b.WriteString("\n")
	}
	return transport.OfficialCommandOutcome{Kind: "success", Text: strings.TrimRight(b.String(), "\n")}, nil
}

// commandGoal 显示本会话真实 goal 记录。
//
// 数据源是 goals.* 用的**同一个** versioned snapshot 存储——没有第二份 goal
// 存储，也没有"为了有输出编一个"。
func (a *agentOfficialAPI) commandGoal(_ context.Context, inv transport.OfficialCommandInvocation) (transport.OfficialCommandOutcome, error) {
	record, exists, err := a.agentCurrentGoal(inv.SessionID)
	if err != nil {
		return transport.OfficialCommandOutcome{}, err
	}
	if !exists {
		return transport.OfficialCommandOutcome{Kind: "success", Text: "本会话没有 goal。"}, nil
	}
	view, err := a.agentGoalView(inv.SessionID, record)
	if err != nil {
		return transport.OfficialCommandOutcome{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "goal %s：%s\n", view.ID, view.Phase)
	fmt.Fprintf(&b, "目标：%s\n", view.Objective)
	fmt.Fprintf(&b, "轮次：%d", view.RoundsStarted)
	if view.MaxGoalRounds > 0 {
		fmt.Fprintf(&b, " / 上限 %d", view.MaxGoalRounds)
	}
	return transport.OfficialCommandOutcome{Kind: "success", Text: b.String()}, nil
}

// commandFeedback 记录一条真实会话反馈。
//
// 走的是 sessionFeedback.record 的**同一个** durable 写入；空正文明确报错
// （官方 error text 必须非空，空反馈等于什么都没记）。
func (a *agentOfficialAPI) commandFeedback(_ context.Context, inv transport.OfficialCommandInvocation) (transport.OfficialCommandOutcome, error) {
	text := strings.TrimSpace(inv.RawInput)
	if text == "" {
		return transport.OfficialCommandOutcome{
			Kind: "error",
			Text: "/feedback 需要正文：/feedback <想反馈的内容>",
		}, nil
	}
	result, err := a.sessionFeedbackRecord(transport.OfficialSessionFeedbackRequest{
		SessionID: inv.SessionID, Text: text,
	})
	if err != nil {
		return transport.OfficialCommandOutcome{}, err
	}
	if !result.OK {
		code := "记录失败"
		if result.Error != nil {
			code = result.Error.Code
		}
		return transport.OfficialCommandOutcome{Kind: "error", Text: "记录反馈失败：" + code}, nil
	}
	return transport.OfficialCommandOutcome{Kind: "success", Text: "已记录这条反馈。"}, nil
}

// appendCommandLifecycle 把一帧命令生命周期写进会话 v4 日志，返回帧上的 seq。
//
// 官方 `appendLifecycle` 是"log-only 直接追加、没有 turn 包裹"。这里同样：
// 只追加这一帧，不新开 turn、不强制 flush（持久化在普通检查点收）。
func appendCommandLifecycle(src *sessionSource, sessionID, eventType string, data any) (int64, error) {
	if src == nil {
		return 0, fmt.Errorf("宿主没有接会话来源（sessionSource 为 nil）")
	}
	path, err := src.resolve(sessionID)
	if err != nil {
		return 0, err
	}
	var lastErr error
	for attempt := 0; attempt < commandLifecycleRetries; attempt++ {
		if attempt > 0 {
			// 写锁持有时间在微秒级；退避只是为了给并发的 turn 收尾让路。
			time.Sleep(time.Duration(attempt) * time.Millisecond)
		}
		commandLifecycleMu.Lock()
		logw, err := session.OpenSessionAt(path)
		if err != nil {
			commandLifecycleMu.Unlock()
			lastErr = err
			continue
		}
		appendErr := logw.Append(eventType, data)
		seq := logw.Seq()
		closeErr := logw.Close()
		commandLifecycleMu.Unlock()
		if appendErr != nil {
			return 0, fmt.Errorf("追加 %s 失败: %w", eventType, appendErr)
		}
		if closeErr != nil {
			return 0, fmt.Errorf("关闭 %s 写入失败: %w", eventType, closeErr)
		}
		return seq, nil
	}
	return 0, fmt.Errorf("追加 %s 失败（写锁一直抢不到）: %w", eventType, lastErr)
}
