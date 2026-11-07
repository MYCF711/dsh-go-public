// Package grep 是内容搜索工具：正则匹配，返回「文件:行:文本」的结构化结果。
//
// 与 shell 里的 grep 的差别全在返回侧：只回匹配行（不回上下文噪声）、
// 每条带行号、总量有界、二进制与大文件直接跳过并说明。
package grep

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"dshgo/internal/fsutil"
	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Go 正则语法"},
    "path": {"type": "string", "description": "起始目录，默认工作区根"},
    "include": {"type": "string", "description": "只搜匹配该 glob 的文件，如 **/*.go"},
    "ignore_case": {"type": "boolean", "description": "默认 false"},
    "limit": {"type": "integer", "minimum": 1, "description": "最多返回多少条匹配，默认 100"}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)

const (
	defaultLimit  = 100
	maxLimit      = 500
	maxFileBytes  = 2 << 20
	maxLineChars  = 400
	scannerBufMax = 1 << 20
)

// Tool 实现 spi.Tool。
type Tool struct {
	fs spi.FS
}

func (t *Tool) Name() string   { return "grep" }
func (t *Tool) Caps() spi.Caps { return spi.Caps{} }
func (t *Tool) Description() string {
	// 说明里必须点出「跳过 .git/refs/node_modules 等目录」：
	// 模型默认爱用 bash 的 grep -r，那会把 vendored 代码一起扫进来。
	// 实测一次这样的事故：refs/ 里 3,569 个文件让答案从 252 变成 9,852，
	// 模型为了消除这个歧义多花了 6 次调用、多烧 22k token。
	return "按 Go 正则搜内容，返回 文件:行:文本；自动跳过 .git/refs/node_modules，比 bash grep -r 准。"
}
func (t *Tool) Schema() json.RawMessage { return schema }

type args struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Include    string `json:"include"`
	IgnoreCase bool   `json:"ignore_case"`
	Limit      int    `json:"limit"`
}

// Match 是一条命中。
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type payload struct {
	Pattern      string  `json:"pattern"`
	Base         string  `json:"base"`
	Count        int     `json:"count"`
	FilesScanned int     `json:"files_scanned"`
	FilesSkipped int     `json:"files_skipped"`
	Truncated    bool    `json:"truncated"`
	Matches      []Match `json:"matches"`
}

var errLimitReached = errors.New("limit reached")

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"pattern": "...", "path"?: ".", "include"?: "**/*.go"}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	if a.Pattern == "" {
		return spi.Result{Hint: "pattern 不能为空；这是 Go 正则，特殊字符要转义"}, fmt.Errorf("pattern 为空")
	}
	if a.Limit <= 0 {
		a.Limit = defaultLimit
	}
	if a.Limit > maxLimit {
		a.Limit = maxLimit
	}

	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return spi.Result{Hint: "正则不合法：检查括号/转义；要搜字面量就把特殊字符转义"},
			fmt.Errorf("正则编译失败: %w", err)
	}

	base := a.Path
	if strings.TrimSpace(base) == "" {
		base = "."
	}
	absBase, err := t.fs.Resolve(base)
	if err != nil {
		return spi.Result{Hint: "path 必须在工作区内"}, err
	}
	// 目录/文件不存在**必须报错**：返回"0 处命中"会让模型以为
	// "这个目录里没有这个符号"，而真实情况是路径就错了。
	if _, statErr := os.Stat(absBase); statErr != nil {
		return spi.Result{
				Hint: "确认 path 拼写；先用 glob 列出目录，或把 path 去掉（默认搜当前工作区）",
			},
			fmt.Errorf("grep 搜索失败：%q 不存在", base)
	}

	var (
		matches []Match
		scanned int
		skipped int
		patSegs []string
	)
	if a.Include != "" {
		patSegs = strings.Split(strings.ReplaceAll(a.Include, "\\", "/"), "/")
	}

	// .dshdignore 生效：仓库自己声明的"别扫我"（例如 vendored 参考代码）。
	ig, _ := fsutil.LoadIgnore(t.fs.Root())
	var ignoredDirs int
	walkErr := fsutil.WalkFilesWith(absBase, fsutil.WalkOptions{
		Root: t.fs.Root(), Ignore: ig, Ignored: &ignoredDirs,
	}, func(abs string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		relBase, err := filepath.Rel(absBase, abs)
		if err != nil {
			return nil
		}
		relBase = filepath.ToSlash(relBase)
		if patSegs != nil && !matchSegments(patSegs, strings.Split(relBase, "/")) {
			return nil
		}
		if st, err := os.Stat(abs); err != nil || st.Size() > maxFileBytes {
			skipped++
			return nil
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			skipped++
			return nil
		}
		head := data
		if len(head) > 8192 {
			head = head[:8192]
		}
		if fsutil.IsBinary(head) {
			skipped++
			return nil
		}
		scanned++

		rootRel, err := filepath.Rel(t.fs.Root(), abs)
		if err != nil {
			rootRel = relBase
		}
		rootRel = filepath.ToSlash(rootRel)

		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), scannerBufMax)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if !re.MatchString(line) {
				continue
			}
			if len(line) > maxLineChars {
				line = line[:maxLineChars]
			}
			matches = append(matches, Match{Path: rootRel, Line: lineNo, Text: line})
			if len(matches) >= a.Limit {
				return errLimitReached
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errLimitReached) {
		return spi.Result{Hint: "遍历失败；换个小一点的范围（path）再试"}, walkErr
	}

	truncated := errors.Is(walkErr, errLimitReached)
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Path != matches[j].Path {
			return matches[i].Path < matches[j].Path
		}
		return matches[i].Line < matches[j].Line
	})
	if matches == nil {
		matches = []Match{}
	}

	p := payload{
		Pattern:      a.Pattern,
		Base:         strings.ReplaceAll(base, "\\", "/"),
		Count:        len(matches),
		FilesScanned: scanned,
		FilesSkipped: skipped,
		Truncated:    truncated,
		Matches:      matches,
	}
	// 跳过的数字要分成两截说：目录名规则 + 仓库自己的 .dshdignore。
	// 混成一个数字的话，模型没法判断"是不是这一层把结果挡掉了"。
	skipNote := fmt.Sprintf("%d 个噪声目录", skipped)
	if ignoredDirs > 0 {
		skipNote += fmt.Sprintf(" + %d 个 .dshdignore 目录", ignoredDirs)
	}
	summary := fmt.Sprintf("%s 下 /%s/ 命中 %d 处（扫描 %d 文件，跳过 %s）",
		p.Base, a.Pattern, p.Count, scanned, skipNote)
	hint := ""
	switch {
	case len(matches) == 0 && ignoredDirs > 0:
		// 和 glob 同一个坑：0 命中可能是"被 .dshdignore 挡住了"，不是"真的没有"。
		hint = fmt.Sprintf("0 处命中，但有 %d 个目录被 .dshdignore 挡掉了；"+
			"确认要找的东西不在那些目录里，或把它从 .dshdignore 去掉", ignoredDirs)
	case len(matches) == 0:
		hint = "没有命中：确认 base 路径对不对，或把 pattern 放宽（这是正则，不是通配符）"
	case truncated:
		hint = fmt.Sprintf("已到 %d 条上限；用 include 收窄文件类型，或换更精确的 pattern", a.Limit)
	}
	return spi.Result{
		Summary: summary,
		Data:    p,
		Hint:    hint,
		View: &spi.ViewSpec{
			Kind:    spi.ViewTable,
			Title:   "grep " + a.Pattern,
			Columns: []string{"文件", "行", "内容"},
			Rows:    tableRows(matches),
		},
	}, nil
}

func tableRows(ms []Match) [][]string {
	rows := make([][]string, 0, len(ms))
	for _, m := range ms {
		rows = append(rows, []string{m.Path, fmt.Sprintf("%d", m.Line), m.Text})
	}
	return rows
}

// matchSegments 与 glob 工具同一套语义（** 跨目录）。
func matchSegments(pat, parts []string) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchSegments(pat[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], parts[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], parts[1:])
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-grep",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools", "fs"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			fsv, err := kernel.Inject(s, spi.FSKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{fs: fsv})
		},
	}
}
