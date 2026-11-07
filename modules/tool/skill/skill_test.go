package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill 造一个 <dir>/<name>/SKILL.md。
func writeSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 清单模式：不带 name 要能列出所有 skill，并带上 frontmatter 里的一句话说明。
//
// 没有它，模型只能猜名字——猜错一次就是一次往返。
func TestListShowsNameAndDescription(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "office-xlsx", "---\nname: office-xlsx\ndescription: 读写 Excel 工作簿\n---\n正文\n")
	writeSkill(t, dir, "no-front", "# 没有 frontmatter 的 skill\n正文\n")

	res, err := (&Tool{dir: dir}).Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("列清单不该失败：%v", err)
	}
	if !strings.Contains(res.Text, "office-xlsx") || !strings.Contains(res.Text, "读写 Excel 工作簿") {
		t.Fatalf("清单缺名字或说明：%s", res.Text)
	}
	// 没有 frontmatter 的只列名字，不许编说明
	if !strings.Contains(res.Text, "no-front") {
		t.Fatalf("没有 frontmatter 的 skill 也要列出来：%s", res.Text)
	}
	if strings.Contains(res.Text, "no-front — ") {
		t.Fatalf("没有 description 就别编：%s", res.Text)
	}
}

// 空目录：列清单要失败，但要说清"没装任何 skill"，而不是含糊的"参数错"。
func TestListOnEmptyDirExplains(t *testing.T) {
	res, err := (&Tool{dir: t.TempDir()}).Exec(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("空目录列清单应当报错")
	}
	if !strings.Contains(res.Hint, "没装任何 skill") {
		t.Fatalf("Hint 要说清是空目录：%s", res.Hint)
	}
}

// 加载正文：名字对就返回全文，且 Text 里就是 SKILL.md 的内容。
func TestLoadReturnsBody(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "demo", "---\ndescription: 演示\n---\n# 步骤\n1. 做这个\n")
	res, err := (&Tool{dir: dir}).Exec(context.Background(), json.RawMessage(`{"name":"demo"}`))
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if !strings.Contains(res.Text, "# 步骤") || !strings.Contains(res.Text, "1. 做这个") {
		t.Fatalf("正文没带回来：%s", res.Text)
	}
}

// 名字不存在：Hint 里要列出真实存在的名字，让模型下一句能改对。
func TestNotFoundListsAlternatives(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "x")
	writeSkill(t, dir, "beta", "x")
	res, err := (&Tool{dir: dir}).Exec(context.Background(), json.RawMessage(`{"name":"gamma"}`))
	if err == nil {
		t.Fatal("不存在的名字应当报错")
	}
	if !strings.Contains(res.Hint, "alpha") || !strings.Contains(res.Hint, "beta") {
		t.Fatalf("Hint 要给出可选名字：%s", res.Hint)
	}
}

// 路径逃逸：name 里带 ../ 一律按"没有这个 skill"处理，绝不读目录外的文件。
func TestRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 在 skills 外面放一个文件，若能读到就说明逃逸了
	if err := os.WriteFile(filepath.Join(root, "secret.md"), []byte("不该被读到"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&Tool{dir: dir}).Exec(context.Background(), json.RawMessage(`{"name":"../secret"}`))
	if err == nil {
		t.Fatalf("逃逸的 name 必须失败，实际拿到：%s", res.Text)
	}
	if strings.Contains(res.Text, "不该被读到") {
		t.Fatal("读到了目录外的文件")
	}
}

// Caps：只读，不该触发审批——否则每次用 skill 都要人点一下。
func TestCapsDoNotNeedApproval(t *testing.T) {
	if (&Tool{}).Caps().NeedsApproval() {
		t.Fatal("skill 只读本地文件，不该要求审批")
	}
}

// DSHD_SKILLS_EXTRA：主目录空着时，额外根里的 skill 也要能列、能取。
//
// 这台机器就是这种情况——<home>/skills 根本不存在，真正的 skill 在别处。
// 没有这条，工具在真机上等于不可用，而单测全绿。
func TestExtraRootIsSearched(t *testing.T) {
	main := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, extra, "hindsight-coding-agent", "---\ndescription: 记忆插件怎么用\n---\n正文\n")
	t.Setenv("DSHD_SKILLS_EXTRA", extra)

	tool := &Tool{dir: main}

	res, err := tool.Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("列清单失败：%v", err)
	}
	if !strings.Contains(res.Text, "hindsight-coding-agent") || !strings.Contains(res.Text, "记忆插件怎么用") {
		t.Fatalf("额外根没被列出来：%s", res.Text)
	}

	got, err := tool.Exec(context.Background(), json.RawMessage(`{"name":"hindsight-coding-agent"}`))
	if err != nil {
		t.Fatalf("从额外根加载失败：%v", err)
	}
	if !strings.Contains(got.Text, "正文") {
		t.Fatalf("正文没取到：%s", got.Text)
	}
}

// 主目录有的优先：同名时不能被额外根盖掉。
func TestMainRootWinsOverExtra(t *testing.T) {
	main := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, main, "dup", "主目录的版本\n")
	writeSkill(t, extra, "dup", "额外根的版本\n")
	t.Setenv("DSHD_SKILLS_EXTRA", extra)

	got, err := (&Tool{dir: main}).Exec(context.Background(), json.RawMessage(`{"name":"dup"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "主目录的版本") {
		t.Fatalf("同名时应当主目录优先：%s", got.Text)
	}
}
