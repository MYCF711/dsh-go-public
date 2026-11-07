// Package glob 是文件模式匹配工具：按路径找文件，返回结构化清单。
//
// 它是独立工具而不是塞进 shell：`find` 的输出是线，结构化清单是数据；
// 模型拿到清单能做下一步判断，拿到一屏文件路径只能再猜。
package glob

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"dshgo/internal/fsutil"
	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "glob 模式（相对 path），支持 ** 跨目录，如 **/*.go"},
    "path": {"type": "string", "description": "起始目录，默认工作区根"},
    "limit": {"type": "integer", "minimum": 1, "description": "最多返回多少条，默认 200"}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)

const (
	defaultLimit = 200
	maxLimit     = 1000
)

// Tool 实现 spi.Tool。
type Tool struct {
	fs spi.FS
}

func (t *Tool) Name() string   { return "glob" }
func (t *Tool) Caps() spi.Caps { return spi.Caps{} }
func (t *Tool) Description() string {
	return "按 glob 模式找文件（支持 **），只返回路径；自动跳过 .git/refs/node_modules。"
}
func (t *Tool) Schema() json.RawMessage { return schema }

type args struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Limit   int    `json:"limit"`
}

type payload struct {
	Pattern   string   `json:"pattern"`
	Base      string   `json:"base"`
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated"`
	Paths     []string `json:"paths"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"pattern": "**/*.go", "path"?: ".", "limit"?: 200}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return spi.Result{Hint: "pattern 不能为空；例如 **/*.go 或 internal/**/testdata/*.json"},
			fmt.Errorf("pattern 为空")
	}
	if a.Limit <= 0 {
		a.Limit = defaultLimit
	}
	if a.Limit > maxLimit {
		a.Limit = maxLimit
	}

	base := a.Path
	if strings.TrimSpace(base) == "" {
		base = "."
	}
	absBase, err := t.fs.Resolve(base)
	if err != nil {
		return spi.Result{Hint: "path 必须在工作区内"}, err
	}

	patSegs := strings.Split(strings.ReplaceAll(a.Pattern, "\\", "/"), "/")
	var matches []string
	// 仓库级声明优先：<root>/.dshdignore 里写的东西不该被扫到。
	ig, _ := fsutil.LoadIgnore(t.fs.Root())
	// 记下"被 .dshdignore 挡掉了多少个目录"：0 命中时，
	// "真的没有这个文件"和"被声明挡住了"是两件事，模型要能分清。
	var ignoredDirs, scanned int
	err = fsutil.WalkFilesWith(absBase, fsutil.WalkOptions{
		Root: t.fs.Root(), Ignore: ig, Ignored: &ignoredDirs,
	}, func(abs string) error {
		scanned++
		relBase, err := filepath.Rel(absBase, abs)
		if err != nil {
			return nil
		}
		relBase = filepath.ToSlash(relBase)
		if !matchSegments(patSegs, strings.Split(relBase, "/")) {
			return nil
		}
		rootRel, err := filepath.Rel(t.fs.Root(), abs)
		if err != nil {
			rootRel = relBase
		}
		matches = append(matches, filepath.ToSlash(rootRel))
		return nil
	})
	if err != nil {
		return spi.Result{Hint: "遍历失败；换个小一点的范围（path）再试"}, err
	}
	sort.Strings(matches)

	truncated := false
	if len(matches) > a.Limit {
		matches = matches[:a.Limit]
		truncated = true
	}
	if matches == nil {
		matches = []string{}
	}

	p := payload{
		Pattern:   a.Pattern,
		Base:      strings.ReplaceAll(base, "\\", "/"),
		Count:     len(matches),
		Truncated: truncated,
		Paths:     matches,
	}
	summary := fmt.Sprintf("%s 下匹配 %s：%d 个文件（扫过 %d）", p.Base, p.Pattern, p.Count, scanned)
	if ignoredDirs > 0 {
		summary += fmt.Sprintf("，.dshdignore 挡掉 %d 个目录", ignoredDirs)
	}
	hint := ""
	switch {
	case truncated:
		hint = fmt.Sprintf("结果被截到 %d 条；缩小 path 或用更具体的 pattern", a.Limit)
	case len(matches) == 0 && ignoredDirs > 0:
		// 这条最容易误判：文件可能就在被挡住的那个目录里。
		hint = fmt.Sprintf("0 个匹配，但有 %d 个目录被 .dshdignore 挡掉了；"+
			"要搜就把它从 .dshdignore 里去掉，或者用 bash 直接找", ignoredDirs)
	case len(matches) == 0:
		hint = "没有匹配：确认 pattern 的相对基准（相对 path），或先 ls 一下目录结构"
	}
	return spi.Result{Summary: summary, Data: p, Hint: hint}, nil
}

// matchSegments 支持 ** 跨目录、* 与 ? 与字符类（交给 path.Match 处理单段）。
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
		ID:       "tool-glob",
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
