package main

// Markdown 基础排版：段落、标题、有序/无序列表、行内 code/bold/strike、围栏代码块。
//
// 移植自上游（MIT）：
//   - `src/components/Markdown.tsx`            —— 用 marked 分词、按块分派
//   - `src/terminal-utils/markdown.ts`         —— 每种块渲染成什么（标题阶梯、
//     列表悬挂缩进、行内 code 用 permission 色、hr 用 subtle 色）
//   - `src/components/StreamingMarkdown.tsx`   —— 流式文本按"块"推进
//
// 这里**不引 marked**：宿主不嵌 JS（R6），而需要的子集很小。自己写解析器的
// 代价是"会不会和 marked 不一致"——所以范围刻意收窄到上面那几样，
// 别的语法**原样显示**（不吞字、不改写），而不是猜一个像样的样子。
//
// 一条硬规矩：**渲染结果里出现的每个字符，都必须能在原文里找到**（除了我们自己
// 加的缩进/边框/符号）。"不支持的内联语法原样显示"就是这个意思——
// 把 `[标题](url)` 渲染成 `标题` 也是一种吞字，人再也看不出原文写了什么。

import (
	"strings"
)

// mdLine 是排版结果的一行：文本 + 它该按哪种折叠规则处理。
//
// 为什么不返回 []string：代码块/思考正文要能**按阈值折叠**，
// 而折叠是显示层的决定（Ctrl+O）。等折完再拼字符串的话，
// 展开时就得重放源文——那条路会重新触发计数与审批，不能走。
type mdLine struct {
	text string
	meta lineMeta
}

// mdBlockKind 是块类型。收窄到"能确定显示成什么"的几种。
type mdBlockKind int

const (
	mdPara mdBlockKind = iota
	mdHeading
	mdList
	mdCode
	mdRule
)

// mdItem 是列表的一项：层级（0 起）、符号（"•" 或 "1."）、正文。
type mdItem struct {
	level  int
	marker string
	text   string
}

type mdBlock struct {
	kind  mdBlockKind
	level int // 标题级别 1..6
	items []mdItem
	lang  string // 代码块语言（围栏后的第一个词；没有就是空）
	body  string
}

// codeFoldRows 是代码块默认显示的行数阈值。
//
// 12 行的来由：120×36 的屏幕正文区约 33 行，一个块占三分之一就已经很长了；
// 再大的块折叠起来才划算，而折叠提示会明说还有多少行——藏东西不是目的，
// 把正文留给正在读的那段才是。
const codeFoldRows = 12

// renderMarkdownLines 把一段 Markdown 排成若干行（宽 width 列）。
// 名字与 /export 的 renderMarkdown([]message) 区分，二者不是同一层。
func renderMarkdownLines(src string, width int) []mdLine {
	if width < 8 {
		width = 8
	}
	blocks := parseMarkdown(src)
	out := make([]mdLine, 0, len(blocks)*2)
	for i, blk := range blocks {
		if i > 0 && len(out) > 0 && out[len(out)-1].text != "" {
			// 块之间留一行空：这是 markdown 的"分段"，也是人眼分段。
			out = append(out, mdLine{})
		}
		out = append(out, renderBlock(blk, width)...)
	}
	return out
}

func renderBlock(blk mdBlock, width int) []mdLine {
	switch blk.kind {
	case mdHeading:
		return renderHeadingBlock(blk, width)
	case mdList:
		return renderListBlock(blk, width)
	case mdCode:
		return renderCodeFrameLines(blk.lang, blk.body, width)
	case mdRule:
		return []mdLine{{text: stSubtle + strings.Repeat("─", width) + stSubtleEnd}}
	default:
		return renderParagraphBlock(blk, width)
	}
}

// renderHeadingBlock：H1 带下划线+品牌蓝、H2 次蓝、H3 起加粗，
// 越深的级别越安静（上游的"蓝阶"）。这里用标记表达同样的阶梯。
func renderHeadingBlock(blk mdBlock, width int) []mdLine {
	text := inlineFormat(blk.body)
	var styled string
	switch blk.level {
	case 1:
		styled = stBold + stAccent + text + stAccentEnd + stBoldEnd
	case 2:
		styled = stBold + stAccent + text + stAccentEnd + stBoldEnd
	case 3:
		styled = stBold + text + stBoldEnd
	default:
		styled = stDim + text + stDimEnd
	}
	var out []mdLine
	for _, ln := range wrapDisp(styled, width) {
		out = append(out, mdLine{text: ln})
	}
	return out
}

func renderParagraphBlock(blk mdBlock, width int) []mdLine {
	text := inlineFormat(blk.body)
	var out []mdLine
	for _, ln := range wrapDisp(text, width) {
		out = append(out, mdLine{text: ln})
	}
	return out
}

// renderListBlock：无序用上游的 `-`，有序保留原编号；续行与正文左对齐
// （悬挂缩进），这样多行的一项不会看起来像"又一项"。
func renderListBlock(blk mdBlock, width int) []mdLine {
	var out []mdLine
	for _, it := range blk.items {
		indent := strings.Repeat("  ", it.level)
		prefix := indent + stAccent + it.marker + stAccentEnd + " "
		bodyIndent := strings.Repeat(" ", dispWidth(prefix))
		bodyWidth := width - dispWidth(prefix)
		if bodyWidth < 4 {
			bodyWidth = 4
		}
		text := inlineFormat(it.text)
		lines := wrapDisp(text, bodyWidth)
		for i, ln := range lines {
			if i == 0 {
				out = append(out, mdLine{text: prefix + ln})
				continue
			}
			out = append(out, mdLine{text: bodyIndent + ln})
		}
	}
	return out
}

// ---------------------------------------------------------------- 解析

// parseMarkdown 把源文切成块。**不认识的行不会被丢掉**：
// 它们会作为段落的一部分原样进正文（表格、引用、HTML 全都走这条路）。
func parseMarkdown(src string) []mdBlock {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var blocks []mdBlock
	for i := 0; i < len(lines); {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}
		if lang, ok := fenceOpen(line); ok {
			body, next := readFence(lines, i+1, line)
			blocks = append(blocks, mdBlock{kind: mdCode, lang: lang, body: body})
			i = next
			continue
		}
		if level, text, ok := headingLine(line); ok {
			blocks = append(blocks, mdBlock{kind: mdHeading, level: level, body: text})
			i++
			continue
		}
		if isRule(line) {
			blocks = append(blocks, mdBlock{kind: mdRule})
			i++
			continue
		}
		if _, _, ok := listItem(line); ok {
			blk, next := readList(lines, i)
			blocks = append(blocks, blk)
			i = next
			continue
		}
		// 段落：吃到空行或下一个块的开头。
		start := i
		var para []string
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "" {
				break
			}
			if _, _, ok := headingLine(lines[i]); ok && i > start {
				break
			}
			if _, ok := fenceOpen(lines[i]); ok && i > start {
				break
			}
			if _, _, ok := listItem(lines[i]); ok && i > start {
				break
			}
			if isRule(lines[i]) && i > start {
				break
			}
			para = append(para, lines[i])
			i++
		}
		blocks = append(blocks, mdBlock{kind: mdPara, body: softJoin(para)})
	}
	return blocks
}

// softJoin 保留段落里的软换行。上游 marked 把它留在 text token 中，
// terminal-utils/renderText 不改写这个换行；本端由 wrapDisp 分成独立显示行。
// 不能在这里拼空格：那既改变原文，也会让中英文的续行位置一起漂移。
func softJoin(lines []string) string {
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// fenceOpen 认 ``` 或 ~~~；返回语言（围栏后第一个词）。
func fenceOpen(line string) (string, bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return "", false
	}
	for _, fence := range []string{"```", "~~~"} {
		if strings.HasPrefix(t, fence) {
			info := strings.TrimSpace(strings.TrimPrefix(t, fence))
			fields := strings.Fields(info)
			if len(fields) == 0 {
				return "", true
			}
			// marked 的 codeLanguageTag：info 只取第一个词，后面的都是 meta。
			return fields[0], true
		}
	}
	return "", false
}

// readFence 读到收尾围栏为止。**没闭合也算一个代码块**：流式输出时
// 围栏永远先出现、后闭合，按"没闭合就当正文"处理会让半截代码闪一下变样。
func readFence(lines []string, start int, open string) (string, int) {
	fence := "```"
	if strings.HasPrefix(strings.TrimLeft(open, " "), "~~~") {
		fence = "~~~"
	}
	var body []string
	for i := start; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == "" {
			return strings.Join(body, "\n"), i + 1
		}
		body = append(body, lines[i])
	}
	return strings.Join(body, "\n"), len(lines)
}

func headingLine(line string) (int, string, bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return 0, "", false
	}
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	rest := t[n:]
	if rest == "" {
		return n, "", true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false // `#标签` 不是标题
	}
	text := strings.TrimSpace(rest)
	text = strings.TrimRight(text, "#")
	return n, strings.TrimSpace(text), true
}

func isRule(line string) bool {
	t := strings.TrimSpace(line)
	if len(t) < 3 {
		return false
	}
	ch := t[0]
	if ch != '-' && ch != '*' && ch != '_' {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] == ' ' || t[i] == '\t' {
			continue
		}
		if t[i] != ch {
			return false
		}
	}
	return true
}

// listItem 认 `- `、`* `、`+ `、`1. `、`1) `，返回层级、符号与正文。
func listItem(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	level := (len(line) - len(trimmed)) / 2
	if len(trimmed) < 2 {
		return 0, "", false
	}
	// 无序
	if strings.ContainsRune("-*+", rune(trimmed[0])) {
		if len(trimmed) > 1 && (trimmed[1] == ' ' || trimmed[1] == '\t') {
			return level, "-", true
		}
		return 0, "", false
	}
	// 有序：数字 + `.` 或 `)` + 空格
	n := 0
	for n < len(trimmed) && trimmed[n] >= '0' && trimmed[n] <= '9' {
		n++
	}
	if n == 0 || n > 9 || n >= len(trimmed) {
		return 0, "", false
	}
	if trimmed[n] != '.' && trimmed[n] != ')' {
		return 0, "", false
	}
	if n+1 >= len(trimmed) || (trimmed[n+1] != ' ' && trimmed[n+1] != '\t') {
		return 0, "", false
	}
	return level, trimmed[:n+1], true
}

// readList 读一整段列表（允许项之间有缩进续行、允许嵌套）。
func readList(lines []string, start int) (mdBlock, int) {
	blk := mdBlock{kind: mdList}
	i := start
	var cur *mdItem
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			// 空行之后如果还是列表项，就继续（松散列表）；否则列表结束。
			if i+1 < len(lines) {
				if _, _, ok := listItem(lines[i+1]); ok {
					i++
					continue
				}
			}
			break
		}
		if level, marker, ok := listItem(line); ok {
			text := strings.TrimLeft(line, " ")
			text = strings.TrimSpace(text[len(marker):])
			blk.items = append(blk.items, mdItem{level: level, marker: marker, text: text})
			cur = &blk.items[len(blk.items)-1]
			i++
			continue
		}
		// 续行：接在上一项后面（同一项的第二行）。
		if cur == nil {
			break
		}
		if _, _, ok := headingLine(line); ok {
			break
		}
		if _, ok := fenceOpen(line); ok {
			break
		}
		cur.text = cur.text + " " + strings.TrimSpace(line)
		i++
	}
	return blk, i
}

// ---------------------------------------------------------------- 行内

// inlineFormat 处理行内 code、bold 与 GFM 删除线，别的语法原样保留。
//
// 只有**配对成功**才改写：单个 `**`、没有闭合的 “ ` “、单 `~` 和未闭合
// `~~` 都原样留在文本里。流式输出时半个分隔符是常态，不能为了抢先显示
// 样式而吞掉后面的正文。删除线只认恰好两个 `~`，与 GUI 的 viva/markdown
// Strike 语义一致；链接仍是本端原有的可见原文，只有 label 会递归排版。
func inlineFormat(s string) string {
	return inlineFormatDepth(s, 0)
}

func inlineFormatDepth(s string, depth int) string {
	// 恶意/损坏文本不能靠嵌套分隔符耗尽栈；超过预算时整段按原文展示。
	if depth >= 32 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				// 上游 renderCodeSpan 渲染 token.text，反引号只是源语法，不进屏幕。
				b.WriteString(stAccent)
				b.WriteString(s[i+1 : i+1+j])
				b.WriteString(stAccentEnd)
				i += j + 2
				continue
			}
		case strings.HasPrefix(s[i:], "**"), strings.HasPrefix(s[i:], "__"):
			mark := s[i : i+2]
			if j := strings.Index(s[i+2:], mark); j >= 0 && j > 0 {
				b.WriteString(stBold)
				b.WriteString(inlineFormatDepth(s[i+2:i+2+j], depth+1))
				b.WriteString(stBoldEnd)
				i += j + 4
				continue
			}
		case s[i] == '~':
			n := inlineByteRun(s, i, '~')
			if n == 2 {
				if end := findStrikeEnd(s, i); end >= 0 {
					body := inlineFormatDepth(s[i+2:end], depth+1)
					if terminalStrikeSupported() {
						b.WriteString(stStrike)
						b.WriteString(body)
						b.WriteString(stStrikeEnd)
					} else {
						// 无 SGR 9 时不装作已经画出删除线：dim 是明确降级，
						// 可见 ~~ 则保证纯文本/NO_COLOR 下也不丢语义与内容。
						b.WriteString(stDim)
						b.WriteString("~~")
						b.WriteString(body)
						b.WriteString("~~")
						b.WriteString(stDimEnd)
					}
					i = end + 2
					continue
				}
			}
			// 单波浪、长运行、转义运行与未闭合运行全部逐字保留。
			b.WriteString(s[i : i+n])
			i += n
			continue
		case s[i] == '[':
			if labelEnd, end, ok := rawMarkdownLink(s, i); ok {
				b.WriteByte('[')
				b.WriteString(inlineFormatDepth(s[i+1:labelEnd], depth+1))
				// 与既有的“不吞链接原文”规则一致：](destination) 不解析、
				// 不隐藏，URL 里的 ~~ 也绝不能被当成标签外的分隔符吃掉。
				b.WriteString(s[labelEnd:end])
				i = end
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// findStrikeEnd 找与 open 配对的恰好双波浪。开/闭分隔符两侧不能是空白，
// 与 GUI findEmphasisEnd 相同；扫描时跳过 code span 与完整链接，避免代码
// 或 destination 内的 ~~ 提前关掉外层删除线。
func findStrikeEnd(s string, open int) int {
	if open+2 >= len(s) || inlineSpace(s[open+2]) || inlineEscaped(s, open) {
		return -1
	}
	for j := open + 2; j < len(s); {
		switch s[j] {
		case '\\':
			// 反斜杠也保留在屏幕上，只让它后面的字符失去分隔符意义。
			j += 2
		case '`':
			n := inlineByteRun(s, j, '`')
			if end := inlineCodeEnd(s, j+n, n); end >= 0 {
				j = end + n
			} else {
				j += n
			}
		case '[':
			if _, end, ok := rawMarkdownLink(s, j); ok {
				j = end
			} else {
				j++
			}
		case '~':
			n := inlineByteRun(s, j, '~')
			if n == 2 && j > open+2 && !inlineSpace(s[j-1]) && !inlineEscaped(s, j) {
				return j
			}
			j += n
		default:
			j++
		}
	}
	return -1
}

func inlineByteRun(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

func inlineCodeEnd(s string, from, ticks int) int {
	for j := from; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		n := inlineByteRun(s, j, '`')
		if n == ticks {
			return j
		}
		j += n
	}
	return -1
}

func inlineSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func inlineEscaped(s string, i int) bool {
	backslashes := 0
	for i > 0 && s[i-1] == '\\' {
		backslashes++
		i--
	}
	return backslashes%2 == 1
}

// rawMarkdownLink 只负责找出链接边界，不把链接变成别的显示文本。
// label 允许嵌套方括号与 code span，destination 允许平衡圆括号；不完整
// 或不平衡的链接返回 false，调用方会按普通原文逐字处理。
func rawMarkdownLink(s string, start int) (labelEnd, end int, ok bool) {
	if start >= len(s) || s[start] != '[' {
		return 0, 0, false
	}
	brackets := 0
	labelEnd = -1
	for j := start; j < len(s); {
		switch s[j] {
		case '\\':
			j += 2
		case '`':
			n := inlineByteRun(s, j, '`')
			if codeEnd := inlineCodeEnd(s, j+n, n); codeEnd >= 0 {
				j = codeEnd + n
			} else {
				j += n
			}
		case '[':
			brackets++
			j++
		case ']':
			brackets--
			j++
			if brackets == 0 {
				labelEnd = j - 1
				j = len(s)
			}
		default:
			j++
		}
	}
	if labelEnd < 0 || labelEnd+1 >= len(s) || s[labelEnd+1] != '(' {
		return 0, 0, false
	}
	parens := 0
	for j := labelEnd + 2; j < len(s); {
		switch s[j] {
		case '\\':
			j += 2
		case '(':
			parens++
			j++
		case ')':
			if parens == 0 {
				return labelEnd, j + 1, true
			}
			parens--
			j++
		default:
			j++
		}
	}
	return 0, 0, false
}
