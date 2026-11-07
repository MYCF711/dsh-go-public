// Package skill 提供 skill 工具：按名字把一份 SKILL.md 全文取进上下文。
//
// 它只做"取文档"这一件事：不执行 skill、不解析 frontmatter、不缓存。
// 内容由人维护在 <dir>/<name>/SKILL.md，本工具只读。
package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

// maxBody 是单次返回正文的字节上限；超过就截断，并在末尾留下可见说明。
const maxBody = 32 << 10

// truncatedTail 是截断时追加在正文末尾的说明。
const truncatedTail = "…（内容超过 32KB，已截断）"

// schema：name 变成**可选**。不给 name = 列清单。
//
// 为什么要有清单模式：模型不可能凭空知道本机装了哪些 skill。
// 没有清单时它只能猜名字，猜错一次就是一次往返——而猜错的成本全在用户那里（等待）。
var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "skill 名。省略则列出本机所有 skill（名字 + 一句话说明）"
    }
  },
  "additionalProperties": false
}`)

// Tool 按名字加载 <dir>/<name>/SKILL.md。
type Tool struct{ dir string }

func (t *Tool) Name() string { return "skill" }

// Caps：只读本地文件，不改盘、不起进程、不联网，因此不需要审批。
func (t *Tool) Caps() spi.Caps { return spi.Caps{} }

func (t *Tool) Schema() json.RawMessage { return schema }

func (t *Tool) Description() string {
	return "按名字加载本机已安装的 skill 正文（<dir>/<name>/SKILL.md）。当某类任务的做法写在 skill 说明里、你需要那份步骤再动手时，先用它取全文。"
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var args struct {
		Name string `json:"name"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return spi.Result{
				Hint: `参数形状是 {"name":"<skill 名>"}；name 必填、必须是字符串。`,
			}, skillErr("skill 参数不是合法 JSON 对象：" + err.Error())
		}
	}

	name := strings.TrimSpace(args.Name)
	// 不给 name（或给 "*" / "list"）= 列清单。这是"先看看有什么"的那一步，
	// 没有它，模型只能猜名字。
	if name == "" || name == "*" || name == "list" {
		return t.list()
	}

	// 按 roots() 的顺序找：第一个有 <root>/<name>/SKILL.md 的就是它。
	// 多个根的理由：主 home 可能是空的，而真正的 skill 目录在别处（本机就是这种情况）。
	var dir string
	for _, r := range t.roots() {
		cand := filepath.Join(r, name, "SKILL.md")
		if !underDir(r, cand) {
			continue // 带 ../ 的 name：不许逃出任何一根
		}
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			dir = r
			break
		}
	}
	if dir == "" {
		slashDir := filepath.ToSlash(strings.Join(t.roots(), ", "))
		return spi.Result{
			Hint: notFoundHintAll(t.roots(), name),
		}, skillErr("没有名为「" + name + "」的 skill（找过：" + slashDir + "）")
	}

	path := filepath.Join(dir, name, "SKILL.md")

	body, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return spi.Result{Hint: notFoundHintAll(t.roots(), name)}, skillErr("没有名为「" + name + "」的 skill")
		}
		return spi.Result{
			Hint: "确认该文件可读（不是目录、没有被权限挡住），或换一个 skill 名。",
		}, skillErr("读不到 " + filepath.ToSlash(path) + "：" + readErr.Error())
	}

	text := string(body)
	chars := len([]rune(text))
	truncated := false
	if len(body) > maxBody {
		// 按字节切 32KB；切点可能落在多字节字符中间，ToValidUTF8 负责丢掉残缺尾字节。
		text = strings.ToValidUTF8(text[:maxBody], "") + truncatedTail
		truncated = true
	}

	return spi.Result{
		Text:    text,
		Summary: "已加载 skill「" + name + "」（" + itoa(chars) + " 字）",
		Data: map[string]any{
			"name":      name,
			"chars":     chars,
			"bytes":     len(body),
			"truncated": truncated,
		},
	}, nil
}

// skillErr 是本包的 error 类型。用它而不是 fmt.Errorf：本文件只许用上面那批 import，
// 而 spi.Result 已经承载了给人/给模型看的说明，error 只需说明"失败在哪"。
type skillErr string

func (e skillErr) Error() string { return string(e) }

// list 列出本机所有 skill：名字 + 从 SKILL.md 的 frontmatter 里取的一句话说明。
//
// 说明取不到就不编：只给名字。**空描述比假描述好**——假描述会让模型挑错 skill。
func (t *Tool) list() (spi.Result, error) {
	names := existingSkillsAll(t.roots())
	if len(names) == 0 {
		return spi.Result{
			Hint: "这台机器上没装任何 skill（找过：" + filepath.ToSlash(strings.Join(t.roots(), ", ")) +
				"）。先建 <根>/<名字>/SKILL.md，或用 DSHD_SKILLS_EXTRA 指一个已有的目录。",
		}, skillErr("没有可用的 skill")
	}

	var b strings.Builder
	b.WriteString("本机可用 skill（用 {\"name\":\"<名字>\"} 取全文）：\n")
	for _, n := range names {
		desc := readDescriptionAny(t.roots(), n)
		if desc == "" {
			b.WriteString("- " + n + "\n")
			continue
		}
		b.WriteString("- " + n + " — " + desc + "\n")
	}

	return spi.Result{
		Summary: "本机有 " + itoa(len(names)) + " 个 skill",
		Text:    strings.TrimRight(b.String(), "\n"),
		Data:    map[string]any{"count": len(names), "names": names},
	}, nil
}

// roots 是查找顺序：主目录在前，`DSHD_SKILLS_EXTRA` 里的追加在后（用 ; 分隔）。
//
// 为什么需要额外根：主 home 的 skills 可能是空的，而真正的 skill 目录在别处。
// 不给一条"指过去"的路，这个工具在那种机器上就等于没有。
func (t *Tool) roots() []string {
	out := []string{}
	if strings.TrimSpace(t.dir) != "" {
		out = append(out, t.dir)
	}
	for _, p := range strings.Split(os.Getenv("DSHD_SKILLS_EXTRA"), ";") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// existingSkillsAll 合并多个根里的 skill 名（同名只算一次，先出现的根优先）。
func existingSkillsAll(roots []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range roots {
		for _, n := range existingSkills(r) {
			if seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// readDescriptionAny 按 roots 顺序找第一个有这个 skill 的根，读它的 description。
func readDescriptionAny(roots []string, name string) string {
	for _, r := range roots {
		p := filepath.Join(r, name, "SKILL.md")
		if !underDir(r, p) {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return readDescription(p)
		}
	}
	return ""
}

// notFoundHintAll 把"真实存在的名字"跨根汇总给模型。
func notFoundHintAll(roots []string, name string) string {
	names := existingSkillsAll(roots)
	if len(names) == 0 {
		return "没有名为「" + name + "」的 skill，而且一个都没装（找过：" +
			filepath.ToSlash(strings.Join(roots, ", ")) + "）。"
	}
	return "没有名为「" + name + "」的 skill。现有：" + strings.Join(names, "、")
}

// readDescription 从 SKILL.md 顶部的 YAML frontmatter 里取 description。
//
// 只认最常见的形状：文件以 `---` 开头，之后的若干行 `key: value`，遇到第二个 `---` 结束。
// 取不到就返回空串——**不猜、不拼**。
func readDescription(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.SplitN(text, "\n", 40) // 只看前 40 行：frontmatter 不会更长
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, ln := range lines[1:] {
		t := strings.TrimSpace(ln)
		if t == "---" {
			break
		}
		if !strings.HasPrefix(t, "description:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(t, "description:"))
		v = strings.Trim(v, `"'`)
		if len([]rune(v)) > 90 {
			v = string([]rune(v)[:90]) + "…"
		}
		return v
	}
	return ""
}

// underDir 判断 p 是否落在 base 之内（跨盘符一律判否）。
func underDir(base, p string) bool {
	ab, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	ap, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(ab, ap)
	if err != nil {
		return false // Windows 跨盘符会在这里报错
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// notFoundHint 列出目录里真实存在的 skill 名，让模型下一句能改对参数。
func notFoundHint(dir, name string) string {
	names := existingSkills(dir)
	if len(names) == 0 {
		return "没有名为「" + name + "」的 skill。目录是空的（" + filepath.ToSlash(dir) + "）。"
	}
	return "没有名为「" + name + "」的 skill。现有：" + strings.Join(names, "、")
}

// existingSkills 返回 dir 下含 SKILL.md 的子目录名，已排序。
func existingSkills(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// itoa 打印十进制整数。不引 strconv：本文件只许用上面那批 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Module 把工具注册进 tools 服务。
func Module(dir string) kernel.Module {
	return kernel.Module{
		ID:       "tool-skill",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{dir: dir})
		},
	}
}
