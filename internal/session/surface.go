package session

import (
	"encoding/json"
	"sort"

	"dshgo/codecs/v4"
	"dshgo/internal/llm"
)

// SurfaceMsg 是 surface 上的一个节点（模型真正看到的东西）。
type SurfaceMsg struct {
	Seq     int64
	Type    string // 事件类型
	Source  string // user / system / developer / tool / compact-checkpoint
	Message llm.Message
	Summary bool // 来自 compaction/summary

	// Reasoning 是这条事件里的思考正文（`type:"reasoning"` 的块，按原顺序）。
	//
	// 为什么不在 Message 里：`llm.Message` 是**请求形状**（role/content/tool_calls），
	// 没有 reasoning 字段，思考块在 surfaceMessage 那一步就被丢掉了。而它是生成这次
	// 回答时真实占掉 token 的文本（服务端把思考计进 completion），Meter 的锚点比例
	// 就靠它对齐：漏了它，`anchorEst` 偏小 ⇒ 刻度偏大 ⇒ 自动压缩提前触发。
	//
	// **只加不改**：这是可选字段，既有消费者（compact.Select、History、fork 之外
	// 的读路径）都不读它；fork 也不用它（那边要从事件搬正文，见 ReasoningBlocksBySeq）。
	Reasoning []string

	// Images 是这条消息里的图片出现位置（深度优先，模型请求顺序）；纯文本消息为 nil。
	//
	// 为什么要有：`Message.Content` 是**文本投影**（joinText 只留 text 块），
	// 图片本来整块消失。而图片预算（`RequiredImageOffload`）要按每张图的真实
	// 字节数算、占位文案要带附件 id 供模型取回原件——两者都在这份 durable 块上。
	//
	// `Offloaded` 为 true 的出现位置**同时**已经以官方占位文本
	// （`OffloadedImageText`）原地替换进 `Message.Content`，模型看到的正是那份说明。
	Images []SurfaceImage
}

// Surface 把事件日志折叠成 surface。
//
// 三条规则（research/21 §"投影"）：
//  1. 只有 5 类事件参与派生；
//  2. `compaction/summary` / `compaction/prune` 的 `shadowedSeqs` **从 surface 消失**（日志本身永不删）；
//  3. 摘要本身作为 `compact-checkpoint` 节点进入 surface——它是"被压缩历史的替身"。
//
// 日志里仍有全部事件；这里只是派生。
func Surface(log *v4.Log) []SurfaceMsg {
	hidden := map[int64]bool{}
	type checkpoint struct {
		text  string
		start int64 // 被它遮蔽的第一条 seq
		seq   int64 // 压缩事件自己的 seq（排序兜底）
	}
	var checkpoints []checkpoint

	for _, ev := range log.Events {
		switch ev.Type {
		case "compaction/summary", "compaction/prune":
			var d struct {
				Summary       []ContentBlock `json:"summary"`
				ShadowedSeqs  []int64        `json:"shadowedSeqs"`
				ShadowedRange *struct {
					Start int64 `json:"start"`
					End   int64 `json:"end"`
				} `json:"shadowedRange"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue
			}
			minSeq := int64(-1)
			for _, s := range d.ShadowedSeqs {
				hidden[s] = true
				if minSeq < 0 || s < minSeq {
					minSeq = s
				}
			}
			// shadowedRange 是 shadowedSeqs 的宽松上界；只在没有明细时兜底。
			if len(d.ShadowedSeqs) == 0 && d.ShadowedRange != nil {
				for s := d.ShadowedRange.Start; s <= d.ShadowedRange.End; s++ {
					hidden[s] = true
				}
				minSeq = d.ShadowedRange.Start
			}
			if ev.Type == "compaction/summary" && minSeq >= 0 {
				checkpoints = append(checkpoints, checkpoint{
					text: joinText(d.Summary), start: minSeq, seq: ev.Seq,
				})
			}
		}
	}

	// surfaceOp 的替换：把 [start,end] 从 surface 摘掉（日志里不动）。
	for _, ev := range log.Events {
		kind, start, end := ev.SurfaceOpKind()
		if kind != "replace" {
			continue
		}
		for s := start; s <= end; s++ {
			hidden[s] = true
		}
	}

	// 思考正文得回原始事件取：surfaceMessage 只能产出 llm.Message，装不下 reasoning。
	// 识别规则只有一份（ReasoningBlocksBySeq）；只解 assistant/message 的事件，
	// 别的类型在那个函数里先按 type 跳过。
	reasoning := ReasoningBlocksBySeq(log)

	// 官方 image/offload 决策（插件 80）：只改派生投影，v4 日志一个字节都不动。
	// 先按事件顺序把"哪些出现位置已退役"摊平，再在派生每个节点时原地替换占位。
	offloads := imageOffloadIndex(log)

	// 先把可见节点按日志顺序铺好（压缩事件本身不出现在这里）。
	out := make([]SurfaceMsg, 0, len(log.Events))
	for _, ev := range log.Events {
		if hidden[ev.Seq] {
			continue
		}
		if ev.Type == "compaction/summary" || ev.Type == "compaction/prune" {
			continue
		}
		if m, src, ok := surfaceMessage(ev); ok {
			// 替身有两种身份：compaction/summary 事件（下面插入），
			// 或 fork 过来的 source.kind = compact-checkpoint 的 user/message（就在这里）。
			node := SurfaceMsg{
				Seq: ev.Seq, Type: ev.Type, Source: src, Message: m,
				Summary:   src == "compact-checkpoint",
				Reasoning: reasoning[ev.Seq],
			}
			// 图片出现位置只对 user/message 与 tool/result 存在（assistant 永不被选）；
			// 纯文本消息这里是 nil，行为与以前逐字节一致。
			node.Images = surfaceImages(ev)
			if indexes := offloads[ev.Seq]; len(indexes) > 0 {
				node = applyImageOffload(node, ev, indexes)
			}
			out = append(out, node)
		}
	}

	// 再把替身插回**它遮蔽的那段起点**，而不是追加到末尾。
	// 追加到末尾会让模型以为"摘要"是最新发生的事——真实语料里压缩过的会话就是这样被读反的。
	// 被后一次压缩遮蔽掉的摘要不该再出现（级联压缩）。
	kept := checkpoints[:0]
	for _, cp := range checkpoints {
		if hidden[cp.seq] {
			continue
		}
		kept = append(kept, cp)
	}
	checkpoints = kept

	sort.Slice(checkpoints, func(i, j int) bool {
		if checkpoints[i].start != checkpoints[j].start {
			return checkpoints[i].start < checkpoints[j].start
		}
		return checkpoints[i].seq < checkpoints[j].seq
	})
	for _, cp := range checkpoints {
		idx := len(out)
		for i, n := range out {
			if n.Summary {
				continue // 别插到别的替身前面去
			}
			if n.Seq >= cp.start {
				idx = i
				break
			}
		}
		node := SurfaceMsg{
			Seq: cp.seq, Type: "compaction/summary", Source: "compact-checkpoint",
			Summary: true,
			Message: llm.Message{Role: "user", Content: cp.text},
		}
		out = append(out, SurfaceMsg{})
		copy(out[idx+1:], out[idx:])
		out[idx] = node
	}
	return out
}

// surfaceMessage 把一条耐久事件变成 surface 消息。不在 5 类里的返回 ok=false。
func surfaceMessage(ev v4.Event) (llm.Message, string, bool) {
	switch ev.Type {
	case "system/message", "developer/message":
		var d struct {
			Message struct {
				Content []ContentBlock `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return llm.Message{}, "", false
		}
		role := "system"
		if ev.Type == "developer/message" {
			role = "developer"
		}
		return llm.Message{Role: role, Content: joinText(d.Message.Content)}, role, true

	case "user/message":
		m, ok := userMessage(ev.Data)
		if !ok {
			return llm.Message{}, "", false
		}
		src := "user"
		var d struct {
			Source struct {
				Kind string `json:"kind"`
			} `json:"source"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Source.Kind != "" {
			src = d.Source.Kind
		}
		return m, src, true

	case "assistant/message":
		m, ok := assistantMessage(ev.Data)
		return m, "assistant", ok

	case "tool/result":
		m, ok := toolMessage(ev.Data)
		return m, "tool", ok
	}
	return llm.Message{}, "", false
}
