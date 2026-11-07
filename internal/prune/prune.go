// Package prune 是「工具结果主动裁剪」：在正文**送进模型上下文之前**，
// 按预算决定留什么、压什么，而不是等超长之后在边界上粗暴截断。
//
// 它和 modules/tools 的 spill 分工不同，两层都要有：
//   - spill（事后）：结果超阈值 → 外置成 artifact + 指针，保证**落盘**的那份可回取；
//   - prune（事前）：投影到模型上下文时，按预算决定**模型看到哪一段**。
//
// 为什么不能只靠截断：截断只有一个"阈值"，越过就砍，于是模型要么看到全文、
// 要么看到断口；而真实输出的关键信息分布是**头（命令/文件开头）+ 尾（错误/结论）**，
// 中间最不重要。所以这里按比例留头留尾，把中间换成一行"省略了哪一段、去哪取"。
//
// 预算口径：本包只做**字节 / 行**两维预算，不引入任何 token 估算。
// 全仓 token 估算仍然只有一处口径——session.EstimateRunes（compact 的 KeepTokens 用它），
// 再养一套估算必然两边漂移（一条输出在这里"没超"，在那里"超了"）。
package prune

import (
	"fmt"
	"strings"

	"dshgo/internal/spi"
)

const (
	// DefaultMaxBytes 是正文的默认字节上限（16KB）。
	//
	// 为什么是 16KB 而不是 8KB：上游 `modules/tools` 的 spill 已经先把
	// 超过 4000 **字符**的结果外置成 artifact 并留指针。同样是 4000 字符，
	// ASCII 约 4KB，而纯中文约 12KB——默认定 8KB 的话，中文结果会被
	// spill 压一次、再被 prune 压第二次，**在已经截断过的内容上再截一刀**，
	// 那是净损失。定在 16KB，prune 就只在 spill 够不着的地方兜底。
	DefaultMaxBytes = 16 << 10
	// DefaultMaxLines 是正文的默认行数上限。
	DefaultMaxLines = 200
	// NoPointer 是"原文没有外置"时的指针文案。
	//
	// Pointer 不许为空：模型看到一段被砍的正文却没被告知"原文在哪/有没有原文"，
	// 唯一的下意识反应是再调一次工具，把省下的 token 又花回去。
	NoPointer = "来源未外置，只有这份裁剪后的正文"

	// headShare 是头段占比的分子（60%）：头是"命令/文件开头"，尾是"错误/结论"。
	headShare = 6
	shareBase = 10
)

// Budget 描述一次裁剪的预算。零值就是默认预算（8KB / 200 行）。
type Budget struct {
	MaxBytes int // 正文上限（字节）；<=0 用默认
	MaxLines int // 行数上限；<=0 用默认
}

// withDefaults 补齐默认值。<=0 一律当"没设"，所以 Budget{} 永远等于默认预算。
func (b Budget) withDefaults() Budget {
	if b.MaxBytes <= 0 {
		b.MaxBytes = DefaultMaxBytes
	}
	if b.MaxLines <= 0 {
		b.MaxLines = DefaultMaxLines
	}
	return b
}

// Apply 按预算裁剪一段工具结果正文。
//
// 返回：裁剪后的正文、是否发生了裁剪、以及被砍掉部分的证据（未裁剪时为 nil）。
// 不超预算的正文**原样返回**，一个字都不动（连尾随换行、CRLF 都不碰）。
func Apply(text string, b Budget) (out string, truncated bool, ev *spi.Truncation) {
	return ApplyWithPointer(text, b, "")
}

// ApplyWithPointer 是 Apply 的完整版：调用方知道原文外置在哪（artifact 路径）就传进来，
// 会原样落进 ev.Pointer；传空则用 NoPointer 明说"没有外置"。
func ApplyWithPointer(text string, b Budget, pointer string) (out string, truncated bool, ev *spi.Truncation) {
	width := b.withDefaults()

	// 快速通道：不超预算就原样透传。正常大小的结果不该被"顺手整理"——
	// 裁剪是有代价的（模型看不到原文里的一句话可能要再调一次工具），
	// 只有真的超了才付这个代价。
	if len(text) <= width.MaxBytes && countLines(text) <= width.MaxLines {
		return text, false, nil
	}

	totalLines := countLines(text)
	totalBytes := len(text)

	// 顺序固定：先按行，再按字节。行是语义单位（一条日志、一行代码），
	// 字节是硬上限；反过来先切字节会把行切碎，后面就数不出省略了哪几行。
	out, lineCut := cutLines(text, width.MaxLines)
	if cut, ok := cutBytes(out, width.MaxBytes); ok {
		out = cut
	}

	// 兜底：自定义预算极小（例如 MaxLines=1）时两维互相牵制，
	// 可能"行超了但放不下省略说明"。这里硬收一次，保证输出同时落在两个预算内——
	// 这也是"对已裁剪的正文再 Apply 一次几乎不变"（幂等）的前提。
	if countLines(out) > width.MaxLines {
		if trimmed, ok := cutLines(out, width.MaxLines); ok {
			out, lineCut = trimmed, true
		} else if n := width.MaxLines; n > 0 {
			lines := strings.Split(out, "\n")
			if n < len(lines) {
				out, lineCut = strings.Join(lines[:n], "\n"), true
			}
		}
	}
	if len(out) > width.MaxBytes {
		if trimmed, ok := cutBytes(out, width.MaxBytes); ok {
			out = trimmed
		}
	}

	if strings.TrimSpace(pointer) == "" {
		pointer = NoPointer
	}

	// Kept/Total 的单位跟着"裁的是哪一维"：
	//   - 动了行 → 行数（"保留 200/1000 行"才是模型能用的信息）；
	//   - 只动了字节 → 字节数（单行巨长的输出报行数恒等于 1，没有意义）。
	if lineCut {
		return out, true, &spi.Truncation{
			Kept:    countLines(out),
			Total:   totalLines,
			Pointer: pointer,
		}
	}
	return out, true, &spi.Truncation{
		Kept:    len(out),
		Total:   totalBytes,
		Pointer: pointer,
	}
}

// countLines 数行数：换行数 + 1（"a\nb" 是 2 行，"a\nb\n" 是 3 行——末尾空行也算一行，
// 否则"保留的行数"会和编辑器行号对不上，而 read 的输出就是带行号的）。
func countLines(s string) int {
	return strings.Count(s, "\n") + 1
}

// cutLines 按行数留头留尾：头 60%、尾 40%，中间换成一行省略说明。
//
// 中间那行说明也占预算，所以可用行数先减 1：保证输出行数**不超** maxLines。
func cutLines(s string, maxLines int) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, false
	}
	usable := maxLines - 1
	if usable < 2 {
		// 预算小到连"头 + 说明 + 尾"都放不下，这一维让位给字节维/兜底逻辑。
		return s, false
	}
	head := usable * headShare / shareBase
	if head < 1 {
		head = 1
	}
	tail := usable - head
	if tail < 1 {
		tail = 1
		head = usable - tail
	}
	omittedFrom := head + 1        // 被砍的第一行（1 基，和 read 的行号一致）
	omittedTo := len(lines) - tail // 被砍的最后一行
	notice := fmt.Sprintf("…（第 %d–%d 行已省略，共 %d 行）…", omittedFrom, omittedTo, len(lines))

	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(strings.Join(lines[:head], "\n"))
	b.WriteString("\n")
	b.WriteString(notice)
	b.WriteString("\n")
	b.WriteString(strings.Join(lines[len(lines)-tail:], "\n"))
	return b.String(), true
}

// cutBytes 按字节留头留尾：同样 60/40，切点落在多字节字符中间时丢掉残缺字节。
//
// 用 ToValidUTF8 而不是自己找 rune 边界：切点两侧都可能落在 UTF-8 序列中间
// （头段的**尾部**、尾段的**头部**），只往前找边界修不好后者。
// ToValidUTF8(s, "") 会把这些残缺字节整段删掉，代价最多 3 字节，换来的是
// 输出永远是合法 UTF-8——否则下游 JSON 编码会把坏字节换成 U+FFFD，模型看到一堆 �。
func cutBytes(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	// 省略说明本身也要占预算：按"最长的可能写法"预留，
	// 这样 len(头) + len(说明) + len(尾) 必然 <= maxBytes（否则会出现
	// "裁剪完还超预算，下次再裁一次"的反复缩水）。
	reserve := len(fmt.Sprintf("\n…（中间省略 %020d 字节）…\n", 0))
	avail := maxBytes - reserve
	if avail < 2 {
		// 预算比说明还小：只留头，说明让位（Pointer 里仍然交代了去向）。
		return strings.ToValidUTF8(s[:maxBytes], ""), true
	}
	head := avail * headShare / shareBase
	tail := avail - head
	h := strings.ToValidUTF8(s[:head], "")
	t := strings.ToValidUTF8(s[len(s)-tail:], "")
	omitted := len(s) - len(h) - len(t)

	var b strings.Builder
	b.Grow(maxBytes)
	b.WriteString(h)
	fmt.Fprintf(&b, "\n…（中间省略 %d 字节）…\n", omitted)
	b.WriteString(t)
	return b.String(), true
}
