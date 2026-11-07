package prompt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/spi"
)

type fakeReal struct {
	name string
	caps spi.Caps
}

func (f fakeReal) Name() string            { return f.name }
func (f fakeReal) Description() string     { return "d" }
func (f fakeReal) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f fakeReal) Caps() spi.Caps          { return f.caps }
func (f fakeReal) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	return spi.Result{}, nil
}

func tools() []spi.Tool {
	return []spi.Tool{
		fakeReal{name: "read"},
		fakeReal{name: "write", caps: spi.Caps{Writes: true}},
		fakeReal{name: "shell", caps: spi.Caps{Writes: true, Executes: true}},
	}
}

func TestAssembleIsDeterministic(t *testing.T) {
	ctx := Context{Model: "m", Cwd: "/ws", Platform: "windows/amd64", Date: "2026-11-02", Tools: tools()}
	a := Assemble(ctx, Runtime{}, ToolGuide{})
	b := Assemble(ctx, Runtime{}, ToolGuide{})
	if a != b {
		t.Fatal("同样的输入必须得到同样的字节")
	}
}

func TestEmptySectionsDisappear(t *testing.T) {
	got := Assemble(Context{}, ToolGuide{}, WorkspaceRules{})
	if got != "" {
		t.Fatalf("没有工具、没有工作区时应该是空串，得到 %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatal("不该出现多余空行")
	}
}

func TestRuntimeSection(t *testing.T) {
	got := Runtime{}.Build(Context{Model: "group/x", Cwd: `D:\ws\demo`, Platform: "windows/amd64", Date: "2026-11-02"})
	for _, want := range []string{"Runtime: dsh-go", "Model: group/x", "Workspace: D:/ws/demo", "Platform: windows/amd64", "Date: 2026-11-02"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, `\`) {
		t.Errorf("路径必须用正斜杠：%s", got)
	}
}

func TestToolGuideSplitsByCaps(t *testing.T) {
	got := ToolGuide{}.Build(Context{Tools: tools(), Cwd: "/ws"})
	if !strings.Contains(got, "只读工具：read") {
		t.Errorf("只读工具没列出来：\n%s", got)
	}
	if !strings.Contains(got, "write") || !strings.Contains(got, "shell") {
		t.Errorf("需要批准的工具没列出来：\n%s", got)
	}
	if !strings.Contains(got, "被拒绝时换只读方式") {
		t.Errorf("缺拒绝后的指引：\n%s", got)
	}
}

func TestWorkspaceRulesFromFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# 规矩\n- 先说结论"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := WorkspaceRules{}.Build(Context{Cwd: dir})
	if !strings.Contains(got, "先说结论") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("没读到工作区约定：%s", got)
	}
}

func TestWorkspaceRulesTruncates(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 5000)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got := WorkspaceRules{}.Build(Context{Cwd: dir, MaxWorkspaceBytes: 1000})
	if !strings.Contains(got, "已截断") {
		t.Fatalf("超长内容必须标注截断：%s", got[len(got)-80:])
	}
	if len(got) > 1400 {
		t.Fatalf("截断没生效，长度 %d", len(got))
	}
}

func TestWorkspaceRulesMissingFile(t *testing.T) {
	if got := (WorkspaceRules{}).Build(Context{Cwd: t.TempDir()}); got != "" {
		t.Fatalf("没有 AGENTS.md 时应该是空串，得到 %q", got)
	}
	if got := (WorkspaceRules{}).Build(Context{}); got != "" {
		t.Fatal("没有 cwd 时应该是空串")
	}
}

func TestDefaultCombinesSections(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("规矩"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Default(Context{Model: "m", Cwd: dir, Tools: tools()})
	if !strings.Contains(got, "Runtime: dsh-go") || !strings.Contains(got, "只读工具") || !strings.Contains(got, "规矩") {
		t.Fatalf("默认装配缺段：\n%s", got)
	}
	if strings.Count(got, "Runtime: dsh-go") != 1 {
		t.Fatal("段被重复拼接了")
	}
}
