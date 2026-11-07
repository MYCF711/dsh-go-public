package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBudgetRule(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// budgetRuleBody 只取文件正文：段标题、截断说明和其他提示段都不计入 maxBytes。
func budgetRuleBody(t *testing.T, assembled, section string) string {
	t.Helper()
	start := strings.Index(assembled, section+"（")
	if start < 0 {
		t.Fatalf("缺 %s 段：\n%s", section, assembled)
	}
	body := assembled[start:]
	newline := strings.IndexByte(body, '\n')
	if newline < 0 {
		t.Fatalf("%s 段没有正文：\n%s", section, assembled)
	}
	body = body[newline+1:]
	end := len(body)
	for _, marker := range []string{
		"\n\n（以上内容已截断",
		"\n\n全局约定（",
		"\n\n工作区约定（",
	} {
		if i := strings.Index(body, marker); i >= 0 && i < end {
			end = i
		}
	}
	return strings.TrimSpace(body[:end])
}

func TestGlobalAndWorkspaceRulesHaveIndependentBudgets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
	}{
		{name: "default_8000_each", limit: defaultMaxWorkspaceBytes},
		{name: "custom_each", limit: 193},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			project := filepath.Join(root, "project")
			if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(project, 0o755); err != nil {
				t.Fatal(err)
			}
			globalPath := filepath.Join(home, "AGENTS.md")
			writeBudgetRule(t, globalPath, strings.Repeat("G", tc.limit+257))
			writeBudgetRule(t, filepath.Join(project, "AGENTS.md"), strings.Repeat("P", tc.limit+257))

			ctx := Context{Cwd: project, GlobalRulesPath: globalPath}
			if tc.limit != defaultMaxWorkspaceBytes {
				ctx.MaxGlobalBytes = tc.limit
				ctx.MaxWorkspaceBytes = tc.limit
			}
			got := Default(ctx)
			globalBody := budgetRuleBody(t, got, "全局约定")
			projectBody := budgetRuleBody(t, got, "工作区约定")
			if len(globalBody) != tc.limit {
				t.Fatalf("全局正文=%d 字节，want %d", len(globalBody), tc.limit)
			}
			if len(projectBody) != tc.limit {
				t.Fatalf("项目正文=%d 字节，want %d", len(projectBody), tc.limit)
			}
			if total := len(globalBody) + len(projectBody); total > 2*tc.limit {
				t.Fatalf("两链正文合计=%d 字节，超过 2×%d", total, tc.limit)
			}
			if strings.Count(got, "以上内容已截断") != 2 {
				t.Fatalf("两条链都应各自标注截断：\n%s", got)
			}
		})
	}
}

func TestGlobalAndWorkspaceBudgetsAcceptDifferentValues(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	globalPath := filepath.Join(root, "global.md")
	writeBudgetRule(t, globalPath, strings.Repeat("G", 100))
	writeBudgetRule(t, filepath.Join(project, "AGENTS.md"), strings.Repeat("P", 100))
	got := Default(Context{
		Cwd: project, GlobalRulesPath: globalPath,
		MaxGlobalBytes: 23, MaxWorkspaceBytes: 47,
	})
	globalBody := budgetRuleBody(t, got, "全局约定")
	projectBody := budgetRuleBody(t, got, "工作区约定")
	if len(globalBody) != 23 || len(projectBody) != 47 {
		t.Fatalf("两链未分别使用 23/47 字节预算：global=%d project=%d", len(globalBody), len(projectBody))
	}
	if total := len(globalBody) + len(projectBody); total != 70 {
		t.Fatalf("两链正文合计=%d，want 70", total)
	}
}

func TestOversizedRulesDoNotStarveTheOtherChain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		global      string
		project     string
		wantGlobal  string
		wantProject string
	}{
		{
			name: "oversized_global", global: strings.Repeat("G", 400), project: "project-survives",
			wantGlobal: strings.Repeat("G", 32), wantProject: "project-survives",
		},
		{
			name: "oversized_project", global: "global-survives", project: strings.Repeat("P", 400),
			wantGlobal: "global-survives", wantProject: strings.Repeat("P", 32),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			if err := os.Mkdir(project, 0o755); err != nil {
				t.Fatal(err)
			}
			globalPath := filepath.Join(root, "global.md")
			writeBudgetRule(t, globalPath, tc.global)
			writeBudgetRule(t, filepath.Join(project, "AGENTS.md"), tc.project)
			got := Default(Context{
				Cwd: project, GlobalRulesPath: globalPath,
				MaxGlobalBytes: 32, MaxWorkspaceBytes: 32,
			})
			if body := budgetRuleBody(t, got, "全局约定"); body != tc.wantGlobal {
				t.Fatalf("全局正文=%q，want %q", body, tc.wantGlobal)
			}
			if body := budgetRuleBody(t, got, "工作区约定"); body != tc.wantProject {
				t.Fatalf("项目正文=%q，want %q", body, tc.wantProject)
			}
		})
	}
}

func TestMissingGlobalRulesPreserveOriginalWorkspaceBehavior(t *testing.T) {
	project := t.TempDir()
	writeBudgetRule(t, filepath.Join(project, "AGENTS.md"), "project-rules")
	original := Default(Context{Cwd: project, MaxWorkspaceBytes: 41})

	for _, globalPath := range []string{"", filepath.Join(project, "missing-global.md")} {
		got := Default(Context{
			Cwd: project, GlobalRulesPath: globalPath,
			MaxGlobalBytes: 41, MaxWorkspaceBytes: 41,
		})
		if got != original {
			t.Fatalf("GlobalRulesPath=%q 时原项目提示改变：\n got %q\nwant %q", globalPath, got, original)
		}
		if strings.Contains(got, "全局约定") {
			t.Fatalf("全局文件缺失时不应留下空段：%q", got)
		}
	}
}

func TestMissingProjectRulesKeepGlobalRules(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global.md")
	writeBudgetRule(t, globalPath, "global-without-project")
	got := Default(Context{
		Cwd: filepath.Join(root, "missing-project"), GlobalRulesPath: globalPath,
		MaxGlobalBytes: 41, MaxWorkspaceBytes: 41,
	})
	if body := budgetRuleBody(t, got, "全局约定"); body != "global-without-project" {
		t.Fatalf("项目缺失时全局正文丢失：%q", body)
	}
	if strings.Contains(got, "工作区约定") {
		t.Fatalf("项目文件缺失时不应留下空段：%q", got)
	}
}

func TestExtraRulesPathSemanticsWithGlobalRules(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extra       string
		wantProject string
		denyProject string
	}{
		{name: "absolute_override", extra: "WORKER.md", wantProject: "worker-rules", denyProject: "project-agents"},
		{name: "relative_override", extra: "WORKER.md", wantProject: "worker-rules", denyProject: "project-agents"},
		{name: "missing_falls_back", extra: "missing.md", wantProject: "project-agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			if err := os.Mkdir(project, 0o755); err != nil {
				t.Fatal(err)
			}
			globalPath := filepath.Join(root, "global.md")
			writeBudgetRule(t, globalPath, "global-rules")
			writeBudgetRule(t, filepath.Join(project, "AGENTS.md"), "project-agents")
			writeBudgetRule(t, filepath.Join(project, "WORKER.md"), "worker-rules")
			extra := tc.extra
			if tc.name == "absolute_override" {
				extra = filepath.Join(project, extra)
			}
			got := Default(Context{
				Cwd: project, GlobalRulesPath: globalPath, ExtraRulesPath: extra,
				MaxGlobalBytes: 41, MaxWorkspaceBytes: 41,
			})
			if body := budgetRuleBody(t, got, "全局约定"); body != "global-rules" {
				t.Fatalf("ExtraRulesPath 不应改动全局链：%q", body)
			}
			if body := budgetRuleBody(t, got, "工作区约定"); body != tc.wantProject {
				t.Fatalf("项目正文=%q，want %q", body, tc.wantProject)
			}
			if tc.denyProject != "" && strings.Contains(got, tc.denyProject) {
				t.Fatalf("ExtraRulesPath 覆盖后仍注入了项目 AGENTS.md：%q", got)
			}
		})
	}
}
