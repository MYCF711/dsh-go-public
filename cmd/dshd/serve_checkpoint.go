package main

import (
	"encoding/json"
	"fmt"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// 官方 0.2.2 插件 #78 `@deepseek-ai/dsh-session-checkpoint-policy` 的宿主接线。
//
// 官方原件（asar 只读解出，dsh/node_modules/@deepseek-ai/dsh-session-checkpoint-policy/lib/index.js）：
//
//	:26-31  async function afterCheckpoint(ctx, session, next) {
//	           await ctx.sessions.flush(session);   // ① 请求前缀先耐久
//	           yield* next();                       // ② 才让适配器发第一个 chunk
//	        }
//	:60-65  ctx.on("llm/stream", ...)  session 存在才包 checkpoint；不存在原样放行
//	:55-57  “Checkpoint failures are fail-closed … the downstream adapter … is not invoked.”
//
// 我方没有 llm/stream 中间件（R6：宿主不嵌 JS 运行时），对应物是 Turn 的入口
// 回调 transport.Options.Prompt*：**它一返回，runner 就开始派发模型请求**。
// 所以“在它之前落盘”= 在模型请求之前落盘，且失败时直接不返回成功 ACK ——
// 这是一条比官方更早、但同样 fail-closed 的边界（差异逐条写在 RESULT 里）。
//
// 只补 durability 时机：rewind 的 preview/apply 行为一条不改（那些在
// serve_session_ops.go 的 serveRewindPreview/serveRewindApply 里，本文件不碰）。

// withDurableCheckpointOptions 给四个 prompt 入口套上“请求前先落盘”。
//
// 四个入口都要包：只包 Prompt 的话，带 placement / content 的请求会绕过 durability，
// 而它们在生产里是常态（附件、引用、排队轮次）。
func withDurableCheckpointOptions(home string, opt transport.Options) transport.Options {
	if opt.Prompt != nil {
		inner := opt.Prompt
		opt.Prompt = func(sessionID, text string) (transport.PromptResult, error) {
			if err := checkpointBeforeModelRequest(home, sessionID, session.NewCheckpointRequest("text", []byte(text))); err != nil {
				return transport.PromptResult{}, err
			}
			return inner(sessionID, text)
		}
	}
	if opt.PromptPlacement != nil {
		inner := opt.PromptPlacement
		opt.PromptPlacement = func(sessionID, text, placement string) (transport.PromptResult, error) {
			if err := checkpointBeforeModelRequest(home, sessionID, session.NewCheckpointRequest("text", []byte(text))); err != nil {
				return transport.PromptResult{}, err
			}
			return inner(sessionID, text, placement)
		}
	}
	if opt.PromptBlocks != nil {
		inner := opt.PromptBlocks
		opt.PromptBlocks = func(sessionID string, blocks []transport.ContentBlock) (transport.PromptResult, error) {
			req, err := checkpointRequestForBlocks(blocks)
			if err != nil {
				return transport.PromptResult{}, err
			}
			if err := checkpointBeforeModelRequest(home, sessionID, req); err != nil {
				return transport.PromptResult{}, err
			}
			return inner(sessionID, blocks)
		}
	}
	if opt.PromptBlocksPlacement != nil {
		inner := opt.PromptBlocksPlacement
		opt.PromptBlocksPlacement = func(sessionID string, blocks []transport.ContentBlock, placement string) (transport.PromptResult, error) {
			req, err := checkpointRequestForBlocks(blocks)
			if err != nil {
				return transport.PromptResult{}, err
			}
			if err := checkpointBeforeModelRequest(home, sessionID, req); err != nil {
				return transport.PromptResult{}, err
			}
			return inner(sessionID, blocks, placement)
		}
	}
	return opt
}

// checkpointRequestForBlocks 把有序内容块压成请求身份。
//
// 用整块的 canonical JSON（transport.ContentBlock 是固定字段结构，键序稳定），
// 不拿文件名或摘要顶替：崩溃后要能看出这一轮到底带了哪些附件/引用。
func checkpointRequestForBlocks(blocks []transport.ContentBlock) (session.CheckpointRequest, error) {
	raw, err := json.Marshal(blocks)
	if err != nil {
		return session.CheckpointRequest{}, fmt.Errorf("durability checkpoint 序列化内容块: %w", err)
	}
	return session.NewCheckpointRequest("blocks", raw), nil
}

// checkpointBeforeModelRequest 是“请求前落盘”的唯一实现。
//
// 判定顺序有意如此：
//  1. 会话解析不出来 → 不落盘、也不报错，让内层给出既有的四字段错误。
//     这里抢先报“checkpoint 失败”会把「会话没了」伪装成「磁盘坏了」。
//  2. 落盘失败 → **响亮失败**，内层（也就是模型请求）根本不进入。
//  3. 落盘成功 → 原样交给内层；同一轮重复调用只会命中同一份快照（幂等）。
func checkpointBeforeModelRequest(home, sessionID string, req session.CheckpointRequest) error {
	if sessionID == "" {
		return fmt.Errorf("durability checkpoint 需要会话 id")
	}
	path, err := newSessionSource(home, "").resolve(sessionID)
	if err != nil {
		// 不吞错也不改写语义：会话不存在时内层的错误更准确（ErrNoSuchSession）。
		return nil
	}
	if _, err := session.WriteDurableCheckpoint(home, path, &req); err != nil {
		return fmt.Errorf("模型请求前落盘失败，本轮未派发: %w", err)
	}
	return nil
}
