// Package prompt 负责系统提示的**装配**：把若干段拼成一条 system 消息。
//
// 规矩：
//  1. 装配必须**确定性**——同样的输入必须得到同样的字节（否则会话快照会抖）；
//  2. 每段都有稳定的名字，空段直接消失（不留空行）；
//  3. 任何外部内容（AGENTS.md 等）必须**有界**并标注截断。
package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dshgo/internal/spi"
)

// Context 是装配所需的运行时信息。
type Context struct {
	Model    string
	Cwd      string
	Platform string
	Date     string
	Tools    []spi.Tool
	// ExtraRulesPath 是额外要注入的规则文件（绝对路径或相对 Cwd）。
	// 用途：给不同角色的会话喂不同的手册——维护者读 AGENTS.md，
	// 写代码的执行者读 docs/WORKER.md。两者内容差一个量级。
	ExtraRulesPath string
	// GlobalRulesPath 是全局 AGENTS.md 的路径（绝对路径或相对 Cwd）；空 = 不注入全局规则。
	GlobalRulesPath string
	// MaxGlobalBytes 与 MaxWorkspaceBytes 是两条规则链各自的正文字节上限，互不争用。
	MaxGlobalBytes    int
	MaxWorkspaceBytes int
}

// Provider 是提示词的一段。返回空串即表示这一段这轮不出现。
type Provider interface {
	Name() string
	Build(Context) string
}

// defaultMaxWorkspaceBytes 是实测选出来的：带上 14 KB 的 AGENTS.md 会让一次
// 很小的提问从 1,909 input token 涨到 6,248（+4,339）。8000 字节 ≈ 2.5k token，
// 是「规则基本还在、每轮不心疼」的折中；要全量传用 MaxWorkspaceBytes 或 CLI 参数。
const defaultMaxWorkspaceBytes = 8000
const defaultMaxGlobalBytes = 8000

// Assemble 按顺序拼接所有非空段。
func Assemble(ctx Context, providers ...Provider) string {
	if ctx.MaxGlobalBytes <= 0 {
		ctx.MaxGlobalBytes = defaultMaxGlobalBytes
	}
	if ctx.MaxWorkspaceBytes <= 0 {
		ctx.MaxWorkspaceBytes = defaultMaxWorkspaceBytes
	}
	var parts []string
	for _, p := range providers {
		s := strings.TrimSpace(p.Build(ctx))
		if s == "" {
			continue
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}

// ---------- 内置的几段 ----------

// Runtime 说明"你在哪、用什么模型、今天几号"。
type Runtime struct{}

func (Runtime) Name() string { return "runtime" }

func (Runtime) Build(c Context) string {
	var b strings.Builder
	b.WriteString("Runtime: dsh-go.\n")
	if c.Model != "" {
		fmt.Fprintf(&b, "Model: %s.\n", c.Model)
	}
	if c.Cwd != "" {
		fmt.Fprintf(&b, "Workspace: %s.\n", filepath.ToSlash(c.Cwd))
	}
	if c.Platform != "" {
		fmt.Fprintf(&b, "Platform: %s.\n", c.Platform)
	}
	if c.Date != "" {
		fmt.Fprintf(&b, "Date: %s.\n", c.Date)
	}
	return b.String()
}

// ToolGuide 用一两句话说明工具怎么用（工具定义本身走 API 的 tools 字段，不在这里重复）。
type ToolGuide struct{}

func (ToolGuide) Name() string { return "tools" }

func (ToolGuide) Build(c Context) string {
	if len(c.Tools) == 0 {
		return ""
	}
	var ro, rw []string
	for _, t := range c.Tools {
		if t.Caps().NeedsApproval() {
			rw = append(rw, t.Name())
		} else {
			ro = append(ro, t.Name())
		}
	}
	var b strings.Builder
	b.WriteString("需要信息就调用工具，不要猜。\n")
	if len(ro) > 0 {
		fmt.Fprintf(&b, "只读工具：%s。\n", strings.Join(ro, ", "))
	}
	if len(rw) > 0 {
		fmt.Fprintf(&b, "会改世界的工具：%s——调用前需要用户批准，被拒绝时换只读方式或先说明意图。\n",
			strings.Join(rw, ", "))
	}
	if c.Cwd != "" {
		b.WriteString("路径一律用正斜杠、相对工作区根。\n")
	}
	return b.String()
}

// GlobalRules 把全局 AGENTS.md 带进来；它与工作区规则各有独立预算。
type GlobalRules struct{}

func (GlobalRules) Name() string { return "global" }

func (GlobalRules) Build(c Context) string {
	if c.GlobalRulesPath == "" {
		return ""
	}
	p := c.GlobalRulesPath
	if !filepath.IsAbs(p) && c.Cwd != "" {
		p = filepath.Join(c.Cwd, p)
	}
	return rulesFromFile("全局约定", p, c.MaxGlobalBytes)
}

// WorkspaceRules 把工作区里的 AGENTS.md 带进来（有界、可缺省）。
type WorkspaceRules struct{}

func (WorkspaceRules) Name() string { return "workspace" }

func (WorkspaceRules) Build(c Context) string {
	if c.Cwd == "" {
		return ""
	}
	candidates := make([]string, 0, 2)
	if c.ExtraRulesPath != "" {
		p := c.ExtraRulesPath
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Cwd, p)
		}
		candidates = append(candidates, p)
	}
	candidates = append(candidates, filepath.Join(c.Cwd, "AGENTS.md"))
	for _, p := range candidates {
		if section := rulesFromFile("工作区约定", p, c.MaxWorkspaceBytes); section != "" {
			return section
		}
	}
	return ""
}

// rulesFromFile 的 limit 只限制文件正文，不包含段标题和截断说明。
func rulesFromFile(title, path string, limit int) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	text := string(b)
	if limit > 0 && len(text) > limit {
		text = text[:limit] + "\n\n（以上内容已截断：原文件更大，需要细节请用 read 工具）"
	}
	return title + "（" + filepath.ToSlash(path) + "）：\n" + strings.TrimSpace(text)
}

// ---------- 便捷入口 ----------

// Default 是默认的装配组合。
func Default(c Context) string {
	return Assemble(c, Runtime{}, ToolGuide{}, GlobalRules{}, WorkspaceRules{})
}
