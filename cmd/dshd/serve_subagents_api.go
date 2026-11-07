package main

// 官方 `subagents.*` 在**宿主侧**的真实装配点。
//
// # 形状
//
// 官方 wire 是"浏览器持有一个 (parentSessionId, childSessionId, mode)
// 控制地址，往子会话的 inbox 投一条消息，再能按父授权中断它"。所以这一层要
// 提供三样真实能力，一件都不能是假的：
//
//	Create    子会话 = 一条**真实 durable v4 会话**，父子归属落 `subagent/spawn` 帧
//	Deliver   真的走宿主 prompt 通道（promptService）起一轮 / 注入当前轮
//	Persist   把投递回执写进子会话日志（`subagent/message` 帧）
//	Interrupt 宿主真实的取消通道（promptService.CancelPlacement）
//
// # 为什么 spawn 落在 internal/agent，而不是这个文件里现场建会话
//
// `dshd serve` 每一轮都是**子进程** `dshd run --session <id>` 跑的
// （serve_tools.go 的 childArgs），子代理工具在那个子进程里执行。也就是说
// "spawn child" 与 "prompt child" 天然跨进程：前者在 run 子进程，后者在 serve
// 进程。内存登记表过不去，只有 v4 日志过得了。所以真实 spawn（建会话 +
// 写父子帧 + 写委派往返）唯一实现在 `internal/agent.ChildSpawn`，serve 侧只做
// **认领**（adoptChild）：按 child id 从盘上认出它属于谁，再进注册表。
//
// # inbox 只有一副
//
// queue / steer 两道仍然是宿主的 promptService（transport 已把 official 的
// delivery 词表翻成 PlacementFollowup / PlacementSteer）。本文件不排队、
// 不持有待发消息、不复制第二份队列——那样会出现两个真值：谁是"下一条"。
//
// # 诚实边界
//
//   - 未登记的 child 一律 no_such_session；父 id 不匹配 403 cross_session；
//     同一个 requestId 重投回同一个 messageId。这三条由 transport 注册表实现，
//     本文件保证它拿到的都是**真实**能力（不是空实现）。
//   - 投递失败**绝不**回 messageId：回执只在宿主投递通道真的接受之后才产生。
//   - `delivery: steer` 需要宿主注入带 placement 的投递通道。没有注入时它
//     **响亮失败**，绝不偷偷降成"开新一轮"——那是把"塞进这一轮"换成"排到最后"，
//     语义变了却长得一样。
//   - 回执帧是 best-effort：子会话正在跑时它的 v4 写锁被 turn 持有，
//     这次 append 会写不进去（不重试成死循环，也不假装写过）。这条不影响
//     messageId 的真实性——messageId 属于"投递被接受"这件事，不属于回执帧。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dshgo/internal/agent"
	"dshgo/internal/transport"
)

// newHostChildRegistry 把宿主真实能力装配成子会话地址表。
func newHostChildRegistry(deps agentOfficialDeps) *transport.OfficialChildRegistry {
	return transport.NewOfficialChildRegistry(transport.OfficialChildSessionOptions{
		Create: func(childSessionID, parentSessionID, cwd string) error {
			if strings.TrimSpace(deps.home) == "" {
				return fmt.Errorf("宿主没有 home，建不了子会话")
			}
			// 幂等：盘上已经有这个 child（另一个进程 spawn 的、或重启后重新认领）
			// 就**不重建**。rebuild 会截断子会话历史，而调用方看到的会是"成功"。
			if rec, found, err := agent.FindChild(deps.home, childSessionID); err != nil {
				return err
			} else if found {
				if rec.ParentSessionID != parentSessionID {
					return fmt.Errorf(
						"子会话 %q 已经属于父会话 %q，不是 %q（归属冲突不许静默改写）",
						childSessionID, rec.ParentSessionID, parentSessionID)
				}
				return nil
			}
			// 真实 spawn：durable v4 会话 + `subagent/spawn` 父子帧。
			if _, err := (agent.ChildSpawn{
				Home:            deps.home,
				ParentSessionID: parentSessionID,
				Cwd:             cwd,
			}).Spawn(agent.ChildSpawnOptions{ChildSessionID: childSessionID}); err != nil {
				return fmt.Errorf("建子会话 %q 失败: %w", childSessionID, err)
			}
			return nil
		},
		Deliver: func(ctx context.Context, childSessionID string, content []transport.OfficialSubagentPromptContent, placement string) error {
			if deps.submit == nil {
				return fmt.Errorf("宿主没有把内容投进子会话的 prompt 通道")
			}
			text, err := renderChildPromptContent(childSessionID, content)
			if err != nil {
				return err
			}
			// 有 placement 通道就按官方两道真投（queue → followup，
			// steer → 注入当前轮）；没有就把 queue 退成 turn，steer 明确失败。
			if deps.submitPlacement != nil {
				if _, err := deps.submitPlacement(childSessionID, text, placement); err != nil {
					return fmt.Errorf("投递到子会话 %q 失败（%s）: %w", childSessionID, placement, err)
				}
				return nil
			}
			if placement != string(transport.PlacementFollowup) {
				return fmt.Errorf(
					"宿主没有 placement 投递通道：子会话 %q 的 %q 道投不进去"+
						"（需要 serve.go 往 agentOfficialDeps.submitPlacement 注入一行，"+
						"见 RESULT 的接线清单）", childSessionID, placement)
			}
			if _, err := deps.submit(childSessionID, text); err != nil {
				return fmt.Errorf("投递到子会话 %q 失败: %w", childSessionID, err)
			}
			return nil
		},
		Persist: func(childSessionID, eventType string, data any) error {
			_, err := appendCommandLifecycle(deps.src, childSessionID, eventType, data)
			return err
		},
		Interrupt: func(childSessionID string) (bool, error) {
			if deps.interrupt == nil {
				return false, fmt.Errorf(
					"宿主没有取消子会话当前轮的通道：agentOfficialDeps.interrupt 为 nil")
			}
			if err := deps.interrupt(childSessionID); err != nil {
				// 官方语义：accepted 只表示"取消信号被接受了"，不代表目标已静止。
				// 没有在飞的一轮 = 目标本来就静止，这是**真实当前状态**，不是失败。
				// 把它当 500 回出去，会让界面把"已经停了"显示成"取消失败"。
				if errors.Is(err, ErrNoSuchTurn) {
					return false, nil
				}
				return false, err
			}
			// 官方：accepted 只表示取消信号已被接受，不代表目标已静止；
			// "有没有在飞的一轮"由真实取消通道自己判断，这里不编。
			return true, nil
		},
	})
}

// adoptChild 把一个**别的进程（或上一次进程）**spawn 的 child 认领进注册表。
//
// 为什么必须有这一步：子代理工具在 `dshd run` 子进程里执行，而 `subagents.prompt`
// 由 serve 进程处理——两个进程，两张内存表。能跨过去的只有盘上的
// `subagent/spawn` 帧，所以每次投递前先按 child id 认一次人。
//
// 认领**按真实父**登记（不是按请求里的父）：父 id 对不对留给注册表判，
// 它才知道该回 403 cross_session；这里提前改归属就等于把 403 变成 200。
func (a *agentOfficialAPI) adoptChild(childSessionID string) {
	if a == nil {
		return
	}
	reg := a.childRegistry()
	if _, ok := reg.Lookup(childSessionID); ok {
		return
	}
	rec, found, err := agent.FindChild(a.deps.home, childSessionID)
	if err != nil || !found {
		// 认不出归属：不编，让 Prompt 按 no_such_session 回。
		return
	}
	// Create 闭包在盘上已认到时是幂等的（不会重建、不会截断历史）。
	_, _ = reg.Spawn(rec.ChildSessionID, rec.ParentSessionID, rec.Cwd)
}

// renderChildPromptContent 把浏览器消息内容渲染成一条可投递的正文。
//
// 图片段**明确拒绝**：宿主 prompt 通道只接受文本，把图片 base64 拼进正文
// 会让子代理把一坨乱码当成任务描述。需要真支持时走 `SubmitBlocks` 接线。
func renderChildPromptContent(childSessionID string, content []transport.OfficialSubagentPromptContent) (string, error) {
	var b strings.Builder
	for i, part := range content {
		switch part.Type {
		case "text":
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(part.Text)
		case "image":
			return "", fmt.Errorf(
				"子会话 %q 的第 %d 段是图片：宿主 prompt 通道只接受文本（SubmitBlocks 未接线），"+
					"拒绝把图片当成文本投递", childSessionID, i)
		default:
			return "", fmt.Errorf("子会话 %q 的第 %d 段类型 %q 不认识", childSessionID, i, part.Type)
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", fmt.Errorf("子会话 %q 的投递正文是空串", childSessionID)
	}
	return text, nil
}
