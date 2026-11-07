package session

import (
	"unicode/utf8"

	"dshgo/internal/llm"
)

// Meter 是 token 计量的**单一口径**（PLAN §10）：
// 服务端实测值只负责绝对值，启发式只负责"上次实测之后又涨了多少"。
//
// 没有锚点时纯靠启发式，误差大概 ±30%——够用来触发压缩，不够用来做账。
type Meter struct {
	anchorReal int // 上次服务端给的 totalTokens
	anchorEst  int // 那一刻的启发式估算
}

// Observe 喂一个服务端实测值。
//
// realTotal 是 usage.totalTokens，estNow 是同一条表面的启发式估算。
func (m *Meter) Observe(realTotal int, estNow int) {
	if realTotal <= 0 {
		return
	}
	m.anchorReal = realTotal
	m.anchorEst = estNow
}

// Calibrated 表示已经拿到过服务端锚点。
func (m *Meter) Calibrated() bool { return m.anchorReal > 0 && m.anchorEst > 0 }

// Estimate 给出当前表面的 token 估算。
func (m *Meter) Estimate(surface []SurfaceMsg) int {
	raw := rawEstimate(surface)
	if !m.Calibrated() {
		return raw
	}
	// 用锚点校准：刻度 = 实测/估算。
	scaled := raw * m.anchorReal / m.anchorEst
	// 估出来的数不能比原始启发式还离谱。
	if scaled <= 0 {
		return raw
	}
	return scaled
}

// rawEstimate 是纯启发式：ASCII 4 字节 ≈ 1 token，其它（中日韩等）1 个字 ≈ 1 token。
//
// 这个比例是拿本机 magpie 的 usage 反推出来的经验值：9 个工具 3,760 字节 → 906 token，
// 约 4.15 字节/token；而中文正文大约 1 字/token。
//
// **思考正文也要算**，理由不是"读数好看"，是它直接决定压缩时机：
//   - 服务端的 `usage.totalTokens` 里，这一次生成吐出的思考是计进 completion 的；
//     而 Meter 拿它当锚点时，`anchorEst` 用的就是这个函数。少算思考 ⇒ `anchorEst`
//     偏小 ⇒ 刻度 `anchorReal/anchorEst` 偏大 ⇒ 之后每一步估算都偏高 ⇒
//     **压缩提前触发**（白花一次摘要的钱，还把还有用的上下文提前压掉）。
//   - 历史里的思考其实不会被重新发给模型（`llm.Message` 装不下 reasoning，
//     请求路径只拿它判"有没有内容"，见 serve_turn.historyForModel）。所以把历史
//     思考一并计入是**保守的上界**：宁可早一点压，也不要在真超窗时才动手。
//
// 不会重复计数：tool/result 的正文只从 m.Message.Content 算一次（ToolCalls 是
// assistant 侧的调用参数，两者本来是不同的文本）；思考块单独走 m.Reasoning，
// 与 Content 无交集。结构开销按**节点**加 4，不按块数加——一条消息里的多个块
// 本来也共用同一份 role/分隔符。
func rawEstimate(surface []SurfaceMsg) int {
	total := 0
	for _, m := range surface {
		total += estimateString(m.Message.Content)
		for _, tc := range m.Message.ToolCalls {
			total += estimateString(tc.Function.Name)
			total += estimateString(tc.Function.Arguments)
		}
		for _, text := range m.Reasoning {
			total += estimateString(text)
		}
		// 每条消息的结构开销（role、分隔符等）。
		total += 4
	}
	return total
}

func estimateString(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return ascii/4 + other
}

// CharsOf 是给诊断用的：一条表面里有多少字符。
func CharsOf(surface []SurfaceMsg) (ascii, other int) {
	for _, m := range surface {
		for _, r := range m.Message.Content {
			if r < utf8.RuneSelf {
				ascii++
			} else {
				other++
			}
		}
	}
	return ascii, other
}

// HistoryToSurface 把 canonical 历史转成 surface 消息，供 meter 估算。
//
// 历史里没有 seq/来源信息，这里给 -1 作为"不是从日志派生"的标记——
// meter 只用 Content 与 ToolCalls，不依赖 seq。
func HistoryToSurface(hist []llm.Message) []SurfaceMsg {
	out := make([]SurfaceMsg, 0, len(hist))
	for _, m := range hist {
		out = append(out, SurfaceMsg{Seq: -1, Message: m})
	}
	return out
}
