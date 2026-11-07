package prune_test

// 注意包名是 prune_test（外部测试包）：第 6 条要验证「投影函数接上了裁剪」，
// 需要 import internal/session，而 session 本身 import prune。外部测试包不构成循环。

import (
	"strings"
	"testing"
	"unicode/utf8"

	"dshgo/internal/prune"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// lines 造 n 行、每行形如 "line-0001" 的正文（带行号便于断言"保留了哪几行"）。
func lines(n int) string {
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, "line-"+pad4(i))
	}
	return strings.Join(parts, "\n")
}

func pad4(i int) string {
	b := []byte{byte('0' + i/1000%10), byte('0' + i/100%10), byte('0' + i/10%10), byte('0' + i%10)}
	return string(b)
}

func countLines(s string) int { return strings.Count(s, "\n") + 1 }

// 1. 最重要的一条：**不超预算就一个字都不动**。
// 裁剪是有代价的（模型看不见被砍的内容，可能要多调一次工具），
// 正常大小的结果不该被顺手整理。
func TestApplyPassesThroughSmallText(t *testing.T) {
	cases := []string{
		"",
		"短输出",
		"a\r\nb\r\n", // CRLF 也不许"顺手"改成 LF
		lines(199),   // 差一行到默认行数预算
		strings.Repeat("x", prune.DefaultMaxBytes), // 恰好等于字节预算
	}
	for i, in := range cases {
		out, truncated, ev := prune.Apply(in, prune.Budget{})
		if truncated {
			t.Fatalf("case %d：没超预算却报 truncated=true", i)
		}
		if ev != nil {
			t.Fatalf("case %d：没裁剪却给了 Truncation %+v", i, ev)
		}
		if out != in {
			t.Fatalf("case %d：正文被改动了（%d 字节 → %d 字节）", i, len(in), len(out))
		}
	}

	// 显式预算同样适用：预算内的结果原样返回。
	in := "只有 3 行\n第二行\n第三行"
	if out, truncated, _ := prune.Apply(in, prune.Budget{MaxBytes: 100, MaxLines: 10}); truncated || out != in {
		t.Fatalf("显式预算内不该裁剪：truncated=%v out=%q", truncated, out)
	}
}

// 2. 超行数：头尾都保留，中间被砍；Total 是**原始行数**。
func TestApplyTruncatesByLines(t *testing.T) {
	const total = 1000
	in := lines(total)

	out, truncated, ev := prune.Apply(in, prune.Budget{})
	if !truncated || ev == nil {
		t.Fatal("1000 行必须被裁剪，且要留下证据")
	}
	if ev.Total != total {
		t.Fatalf("Total = %d，期望原始行数 %d", ev.Total, total)
	}
	if got := countLines(out); got > prune.DefaultMaxLines {
		t.Fatalf("裁剪后 %d 行，超过预算 %d", got, prune.DefaultMaxLines)
	}
	if got := countLines(out); ev.Kept != got {
		t.Fatalf("Kept = %d，与输出的实际行数 %d 不符", ev.Kept, got)
	}

	// 头在、尾在、中间没了。
	if !strings.Contains(out, "line-0001") {
		t.Fatal("头（第 1 行）必须保留——那是命令/文件的开头")
	}
	if !strings.Contains(out, "line-1000") {
		t.Fatal("尾（最后一行）必须保留——错误和结论通常在最后")
	}
	if strings.Contains(out, "line-0500") {
		t.Fatal("中间的第 500 行不该还在")
	}
	// 省略说明要说清"少了哪几行、一共多少行"，否则模型只能再试一次。
	if !strings.Contains(out, "已省略") || !strings.Contains(out, "共 1000 行") {
		t.Fatalf("缺少省略说明，输出尾部：%q", tailOf(out, 200))
	}
	t.Logf("%d 行 → %d 行；Kept=%d Total=%d Pointer=%q；省略说明：%s",
		total, countLines(out), ev.Kept, ev.Total, ev.Pointer, noticeLine(out))

	// 头占比 60%：前 119 行留下，第 120 行开始省略（可用行数 199 = 200-1 行说明）。
	if !strings.Contains(out, "line-0119") || strings.Contains(out, "line-0120") {
		t.Fatal("头段边界必须是第 119/120 行（(200-1)*60%）")
	}
}

// 3. 超字节：切点落在多字节字符中间时，输出仍是合法 UTF-8。
func TestApplyTruncatesByBytesKeepsValidUTF8(t *testing.T) {
	// 单行巨长的中文：6 个汉字 × 3 字节 = 18 字节/份，1000 份 = 18000 字节。
	in := strings.Repeat("中文内容测试", 1000)
	if len(in) <= prune.DefaultMaxBytes {
		t.Fatalf("用例没超预算：%d 字节", len(in))
	}

	out, truncated, ev := prune.Apply(in, prune.Budget{})
	if !truncated || ev == nil {
		t.Fatal("单行超字节必须被裁剪")
	}
	if len(out) > prune.DefaultMaxBytes {
		t.Fatalf("裁剪后 %d 字节，超过预算 %d", len(out), prune.DefaultMaxBytes)
	}
	// 这条是核心：头段结尾、尾段开头都落在汉字中间，残缺字节必须被丢掉。
	if !utf8.ValidString(out) {
		t.Fatal("裁剪后不是合法 UTF-8（残缺字节没丢掉）")
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Fatal("裁剪后出现了 U+FFFD —— 坏字节被替换而不是丢掉，模型会看到一堆 �")
	}
	if !strings.HasPrefix(out, "中文内容测试") {
		t.Fatalf("头段丢了：%q", headOf(out, 60))
	}
	if !strings.HasSuffix(out, "中文内容测试") {
		t.Fatalf("尾段丢了：%q", tailOf(out, 60))
	}
	if !strings.Contains(out, "中间省略") {
		t.Fatal("字节裁剪也要说明省略了多少字节")
	}
	if ev.Total != len(in) {
		t.Fatalf("只动字节时 Total 应是原始字节数 %d，实际 %d", len(in), ev.Total)
	}
	t.Logf("%d 字节 → %d 字节（单行中文）；Kept=%d Total=%d；省略说明：%s",
		len(in), len(out), ev.Kept, ev.Total, noticeLine(out))

	// 显式预算也一样：切点故意落在汉字中间（maxBytes-预留 不是 3 的倍数）。
	for _, max := range []int{100, 101, 1000, 4097} {
		out, truncated, _ := prune.Apply(in, prune.Budget{MaxBytes: max})
		if !truncated || len(out) > max {
			t.Fatalf("MaxBytes=%d：truncated=%v len=%d", max, truncated, len(out))
		}
		if !utf8.ValidString(out) {
			t.Fatalf("MaxBytes=%d：输出不是合法 UTF-8", max)
		}
	}
}

// 4. Pointer 不许为空：要么给 artifact 路径，要么明说"没有外置"。
func TestApplyPointerNeverEmpty(t *testing.T) {
	in := lines(1000)

	_, _, ev := prune.Apply(in, prune.Budget{})
	if ev == nil || ev.Pointer == "" {
		t.Fatalf("Pointer 为空：模型只知道少了东西、不知道去哪取（ev=%+v）", ev)
	}
	if ev.Pointer != prune.NoPointer {
		t.Fatalf("没给 artifact 时 Pointer 应明说没有外置，实际 %q", ev.Pointer)
	}
	if !strings.Contains(ev.Pointer, "未外置") {
		t.Fatalf("Pointer 文案没说清「没有外置」：%q", ev.Pointer)
	}

	// 调用方给了 artifact 路径就原样填进去。
	const artifact = "/tmp/dshd-spill/tool-42.txt"
	_, _, ev = prune.ApplyWithPointer(in, prune.Budget{}, artifact)
	if ev == nil || ev.Pointer != artifact {
		t.Fatalf("给了 artifact 路径却没填进 Pointer：%+v", ev)
	}

	// 只给空白也算没给——不许落一个空指针出去。
	_, _, ev = prune.ApplyWithPointer(in, prune.Budget{}, "   ")
	if ev == nil || ev.Pointer != prune.NoPointer {
		t.Fatalf("空白 pointer 应退回默认文案，实际 %+v", ev)
	}
}

// 5. 幂等：对已裁剪的正文再 Apply 一次，不该再缩水。
func TestApplyIsIdempotent(t *testing.T) {
	cases := map[string]string{
		"超行数": lines(1000),
		"超字节": strings.Repeat("中文内容测试", 1000),
	}
	for name, in := range cases {
		once, truncated, _ := prune.Apply(in, prune.Budget{})
		if !truncated {
			t.Fatalf("%s：第一次就没裁", name)
		}
		twice, truncatedAgain, ev := prune.Apply(once, prune.Budget{})
		if truncatedAgain || ev != nil {
			t.Fatalf("%s：第二次又裁了一遍（说明第一次没把结果压进预算）", name)
		}
		if twice != once {
			t.Fatalf("%s：第二次改动了正文（%d → %d 字节）", name, len(once), len(twice))
		}
	}
}

// 6. 接在投影函数上：只有超预算的正文被裁，正常大小原样透传，Data/View 不动。
func TestProjectResultPrunesOnlyOverBudget(t *testing.T) {
	// 6.1 正常大小：逐字节透传，不多出一个块。
	view := &spi.ViewSpec{Kind: spi.ViewPlain, Title: "t"}
	small := "文件读完了\n一共 3 行"
	r := spi.Result{
		Summary: "读到了 3 行",
		Text:    small,
		Data:    map[string]any{"total": 3},
		View:    view,
	}
	blocks := session.ProjectResult(r)
	if len(blocks) != 1 || blocks[0].Text != small {
		t.Fatalf("正常大小的正文必须原样透传，得到 %+v", blocks)
	}
	// 不许动 Data / View：裁剪只作用于"送进模型的那份正文"。
	if r.View != view || r.Data.(map[string]any)["total"] != 3 {
		t.Fatal("投影改动了 Result.Data / Result.View")
	}

	// 6.2 超预算：正文被裁，且补一条"保留多少、去哪取"的说明。
	big := lines(1000)
	r = spi.Result{Text: big, Data: map[string]any{"total": 1000}, View: view}
	blocks = session.ProjectResult(r)
	if len(blocks) != 2 {
		t.Fatalf("裁剪后应有「正文 + 裁剪说明」两块，得到 %d 块", len(blocks))
	}
	if !strings.Contains(blocks[0].Text, "已省略") {
		t.Fatal("块 0 应该是裁剪后的正文（含省略说明）")
	}
	if countLines(blocks[0].Text) > prune.DefaultMaxLines {
		t.Fatalf("投影出去的正文 %d 行，超预算", countLines(blocks[0].Text))
	}
	if !strings.Contains(blocks[1].Text, "已按预算裁剪") || !strings.Contains(blocks[1].Text, "未外置") {
		t.Fatalf("缺少裁剪说明块：%q", blocks[1].Text)
	}
	if !strings.Contains(blocks[1].Text, "1000") {
		t.Fatalf("裁剪说明要报原始行数：%q", blocks[1].Text)
	}
	// 投影不改 Result 本体。
	if r.View != view || r.Text != big {
		t.Fatal("投影改动了 Result 本体")
	}

	// 6.3 已经有截断证据（且带指针）时，止损于两块：不再重复第三条"去哪取"。
	r = spi.Result{
		Text:      big,
		Artifacts: []spi.Artifact{{Name: "big", Path: "/tmp/spill/big.txt", Bytes: 9000}},
		Truncated: &spi.Truncation{Kept: 3000, Total: 9000, Pointer: "/tmp/spill/big.txt"},
	}
	blocks = session.ProjectResult(r)
	if len(blocks) != 2 {
		t.Fatalf("已有截断说明时不该再多一块，得到 %d 块：%+v", len(blocks), blocks)
	}
	if !strings.Contains(blocks[1].Text, "/tmp/spill/big.txt") {
		t.Fatalf("指针丢了：%q", blocks[1].Text)
	}
	// 指针只出现一次（spill 的路径被 sourcePointer 带进了正文的省略说明里）。
	if n := strings.Count(strings.Join([]string{blocks[0].Text, blocks[1].Text}, "\n"), "/tmp/spill/big.txt"); n > 2 {
		t.Fatalf("同一个指针在一屏里出现了 %d 次，冗余", n)
	}

	// 6.4 截断证据的指针是空的（残缺证据）：prune 必须补上"没有外置"，
	// 绝不能让模型看到一句「全文在 」然后就没了。
	r = spi.Result{Text: big, Truncated: &spi.Truncation{Kept: 1, Total: 2}}
	blocks = session.ProjectResult(r)
	if len(blocks) != 3 {
		t.Fatalf("残缺证据时要补一块裁剪说明，得到 %d 块", len(blocks))
	}
	if !strings.Contains(blocks[1].Text, "未外置") {
		t.Fatalf("prune 的说明必须明说没有外置：%q", blocks[1].Text)
	}
}

// noticeLine 取出正文里那行省略说明，方便 -v 输出直接看到"模型被告知了什么"。
func noticeLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, "已省略") || strings.Contains(ln, "中间省略") {
			return ln
		}
	}
	return "（没有省略说明）"
}

func headOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// 默认预算必须**大于 spill 能产生的最大正文**，否则会对已截断的内容再截一刀。
//
// spill 的阈值是 4000 **字符**；同样是 4000 字符串，纯中文约 12KB。
// 默认定在 8KB 时，中文结果会被 spill 压一次、prune 再压一次——
// 第二次是在"已经截断过的内容"上动手，纯属净损失。
func TestDefaultBudgetExceedsSpillReach(t *testing.T) {
	const spillMaxChars = 4000
	worstCJK := spillMaxChars * 3 // 一个汉字 UTF-8 占 3 字节
	if prune.DefaultMaxBytes <= worstCJK {
		t.Fatalf("默认字节预算 %d 小于 spill 的最坏产出 %d——中文结果会被二次裁剪",
			prune.DefaultMaxBytes, worstCJK)
	}
}
