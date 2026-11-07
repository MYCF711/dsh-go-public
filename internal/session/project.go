// Package session 是事件溯源与 canonical 模型。
//
// 这一层的职责边界：内核只认本包的 canonical 事件；DSH 的 v4 文件格式是
// 编解码器（codecs/v4），不是内核结构。拔掉 v4 不需要改内核一行。
package session

import (
	"encoding/json"
	"fmt"

	"dshgo/internal/prune"
	"dshgo/internal/spi"
)

// Block 就是 spi.Block：内容块只有一份定义，避免会话层与 agent 层各自漂移。
type Block = spi.Block

// ProjectResult 把内部 Result 投影成会话边界上的 content[]。
//
// 这是「结构化是宿主内部契约、v4 是边界编码」的那条缝：
// 内部可以有 Data / Artifacts / Truncated / Hint，落到会话里只剩文本块。
// 顺序固定：摘要 → 数据 → 截断说明 → 提示，保证同一结果永远产生同样的字节。
//
// **主动裁剪就在这一层**：正文在这一步才第一次"变成模型要读的字"，
// 也只有这里同时看得到正文、外置 artifact 和已有的截断证据。
// 只裁超预算的那条路径（prune 内部先判 fits），正常大小的结果原样透传；
// Data / View 一律不碰——它们不是送进模型的那份字节。
func ProjectResult(r spi.Result) []Block {
	out := make([]Block, 0, 5)

	// 工具自己渲染好了就用它的（对齐 DSH 的呈现约定），否则退回 Summary+JSON。
	if r.Text != "" {
		text, cut, ev := prune.ApplyWithPointer(r.Text, prune.Budget{}, sourcePointer(r))
		out = append(out, Block{Type: "text", Text: text})
		// 裁剪证据要单独说一句：正文里那行"第 X–Y 行已省略"只说"少了什么"，
		// 不说"原文还在不在、在哪"，模型据此决定是重读还是收工。
		//
		// 但下面那条「已截断」说明已经给出指针时就不再说第二遍——三条消息说同一件事
		// 纯属浪费 token（见 modules/tools/pipeline.go 同类取舍）。只有截断证据残缺、
		// 指针为空时才由 prune 补上，保证模型永远拿到一个非空的"去哪取"。
		if cut && ev != nil && (r.Truncated == nil || r.Truncated.Pointer == "") {
			out = append(out, Block{
				Type: "text",
				Text: fmt.Sprintf("（已按预算裁剪：保留 %d/%d；取全文：%s）",
					ev.Kept, ev.Total, ev.Pointer),
			})
		}
	} else {
		if r.Summary != "" {
			out = append(out, Block{Type: "text", Text: r.Summary})
		}
		if r.Data != nil {
			if b, err := json.Marshal(r.Data); err == nil && len(b) > 0 {
				out = append(out, Block{Type: "text", Text: string(b)})
			}
		}
	}

	if r.Truncated != nil {
		out = append(out, Block{
			Type: "text",
			Text: fmt.Sprintf("（已截断：保留 %d/%d 字节，全文在 %s）",
				r.Truncated.Kept, r.Truncated.Total, r.Truncated.Pointer),
		})
	}
	if r.Hint != "" {
		out = append(out, Block{Type: "text", Text: "提示：" + r.Hint})
	}
	if len(out) == 0 {
		out = append(out, Block{Type: "text", Text: "（无输出）"})
	}
	return out
}

// sourcePointer 说清"原文去哪儿取"：已有的截断指针优先（那是 spill 真的写下的路径），
// 其次第一个外置 artifact 的路径；都没有则返回空串，由 prune 明写"来源未外置"。
func sourcePointer(r spi.Result) string {
	if r.Truncated != nil && r.Truncated.Pointer != "" {
		return r.Truncated.Pointer
	}
	for _, a := range r.Artifacts {
		if a.Path != "" {
			return a.Path
		}
	}
	return ""
}

// Flatten 把内容块拼成一段纯文本（给不接受分块的老接口用）。
func Flatten(blocks []Block) string {
	out := ""
	for i, b := range blocks {
		if i > 0 {
			out += "\n"
		}
		out += b.Text
	}
	return out
}
