package main

import (
	"context"
	"strings"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// inheritKeep 是转写的上限。
//
// 为什么要限条数：父会话可能很长，全塞进去等于把子代理的上下文预算一次耗光，
// 而它真正需要的是"刚才在聊什么"。
//
// 单位是**条消息**（转写里一条消息一行），不是日志事件：旧实现先截 20 条消息
// 再过滤，过滤掉的那些本该由后来的补上；现在是先判"这条有没有东西"，再取最后 20 条。
const inheritKeep = 20

// inheritFromCtx 取父会话最近若干条，拼成一段给子代理看的文本。
//
// 取不到就返回空串（子代理退化成普通 subagent）——不报错：
// 继承失败不该让任务失败，但**必须在结果里看得出来没继承**（见 modules/tool/subagentfork
// 的 inheritNote：空串会明说"这次没带上历史"）。
func inheritFromCtx(home string) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		sid := spi.SessionFrom(ctx)
		if sid == "" {
			return ""
		}
		// 从 v4 日志派生，**不用 sessionSource.Messages**：那个投影是给界面看的，
		// 只有 Text——照它转写，只有 tool_calls 或只有 reasoning 的消息会整条消失。
		// 这与 serve_turn.go 的 modelRunner.history 是同一条理由、同一份投影
		// （session.Surface），也是同一个坑的第二次出现。
		path, err := newSessionSource(home, "").resolve(sid)
		if err != nil {
			return ""
		}
		log, err := v4.ReadFile(path)
		if err != nil {
			return ""
		}
		return inheritTranscript(log)
	}
}

// inheritTranscript 把父会话的 surface 铺成一段给子代理看的纯文本。
//
// 转写与 modelRunner.history 的区别**只在输出形态**，不在判据：
//   - 那边要发 messages（tool_calls 是结构化字段），所以工具调用原样带过去；
//   - 这边是纯文本，工具调用只能写成一句人话 `[调用 bash：{…}]`，
//     但**不许因为"塞不进 messages"就整条丢掉**——子代理会因此以为父会话
//     根本没查过东西，然后把已经做过的事再做一遍。
//
// 判据只有一份：assistantHasPayload（和 serve 的历史过滤、推帧、落盘同一个函数）。
// 有正文、有思考、有工具调用，占一样就不算空；三样都没有才是真空消息。
// 「有没有思考块」要另解一遍事件：llm.Message 里没有 reasoning 字段，
// Surface 投影时把 reasoning 块丢了——所以借 serve 那条路已经写好的 reasoningBySeq。
func inheritTranscript(log *v4.Log) string {
	nodes := session.Surface(log)
	reasoning := reasoningBySeq(log)

	lines := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if line, ok := inheritLine(n, reasoning[n.Seq]); ok {
			lines = append(lines, line)
		}
	}
	if len(lines) > inheritKeep {
		lines = lines[len(lines)-inheritKeep:]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// inheritLine 把 surface 上的一个节点画成一行 `角色: 内容`，画不出内容就返回 false。
//
// 三种角色各有各的"有内容"：
//   - user / tool 只有正文，空的就跳过；
//   - assistant 有三样（正文、思考、工具调用），空的是且仅是"三样都没有"。
//
// system / developer 不进来：它是父会话的装配期提示，不是"刚才在聊什么"，
// 与 modelRunner.history 的取舍一致。
func inheritLine(n session.SurfaceMsg, reasoning string) (string, bool) {
	switch n.Message.Role {
	case "assistant":
		// 判据就是这一处调用；这里不另写"有没有内容"。
		if !assistantHasPayload(n.Message.Content, reasoning, len(n.Message.ToolCalls)) {
			return "", false
		}
		parts := make([]string, 0, 2+len(n.Message.ToolCalls))
		if text := strings.TrimSpace(n.Message.Content); text != "" {
			parts = append(parts, flattenText(text))
		}
		// 思考块带正文，不只是一句"它想过"：子代理需要知道父会话**为什么**这么干。
		// （这与 serve 那条路不同：那里思考只能出"有没有"，因为它要发的是 assistant
		// content——把心里话当回答发出去是错的；这里发的是给人看的转写。）
		if think := strings.TrimSpace(reasoning); think != "" {
			parts = append(parts, reasoningText(think))
		}
		for _, tc := range n.Message.ToolCalls {
			parts = append(parts, toolCallText(tc.Function.Name, tc.Function.Arguments))
		}
		return "assistant: " + strings.Join(parts, " "), true

	case "system", "developer", "":
		// 空角色没有任何投影认得它（与 modelRunner.history 同一处理）。
		return "", false
	}

	// user（含压缩替身）/ tool：只有正文能带信息，空的跳过。
	// 注意 tool 的调用名与参数不在 tool/result 事件里，能看的只有结果正文；
	// 它上面那行 `[调用 …]` 已经把"调了什么"说清了。
	text := strings.TrimSpace(n.Message.Content)
	if text == "" {
		return "", false
	}
	return n.Message.Role + ": " + flattenText(text), true
}

// flattenText 把一段正文压成一行。
//
// 为什么必须一行：子代理的结果摘要按**非空行数**报"继承 N 条历史"（inheritNote，
// 在 modules/tool/subagentfork 里；那个包不认识会话，也不该为这件事改）。
// 正文里带换行时，一条消息会被数成好几条，那个数字就是假的——模型会以为
// 自己拿到了比实际更多的历史。转写本来就是"一条消息一行"的格式，
// 把换行与连续空白折成单个空格既保住全部内容，也让那个计数说得准。
func flattenText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// toolCallText 是"工具调用"在给人看的文本里的**唯一**写法。
//
// 唯一：`subagent_fork` 的继承转写（inheritLine）与 `history_search` 的可检索正文
// （history.go 的 eventText）都调它。两处各写一份的话形态迟早漂，而"搜得到"
// 依赖的正是这个形态——模型看见 `[调用 bash：{"command":"ls"}]` 才知道下次该按什么搜。
// 参数原样保留、只折空白：它是给人读的 JSON 片段，不是给机器解析的字段。
func toolCallText(name, args string) string {
	return "[调用 " + name + "：" + flattenText(args) + "]"
}

// reasoningText 是"思考块"在给人看的文本里的唯一写法（与 toolCallText 同一理由）。
//
// 带正文，不只是一句"它想过"：子代理要据此知道父会话**为什么**这么干；
// 历史检索也要能按心里那句话把那一轮找回来。
func reasoningText(s string) string {
	return "[思考：" + flattenText(s) + "]"
}
