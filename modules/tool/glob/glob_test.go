package glob

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/spi"
	"dshgo/modules/fs/local"
)

func newTool(t *testing.T) (*Tool, string) {
	t.Helper()
	root := t.TempDir()
	fs, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Tool{fs: fs}, root
}

func touch(t *testing.T, root, name string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, tool *Tool, args map[string]any) (spi.Result, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tool.Exec(context.Background(), raw)
}

func TestMatchSegments(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "x/a.go", true},
		{"**/*.go", "x/y/a.go", true},
		{"*.go", "x/a.go", false},
		{"internal/**/*.go", "internal/a.go", true},
		{"internal/**/*.go", "internal/k/a.go", true},
		{"internal/**/*.go", "other/a.go", false},
		{"a?c.txt", "abc.txt", true},
		{"*.{go,md}", "a.go", false}, // 刻意不支持花括号展开：少一种隐式语法
	}
	for _, c := range cases {
		got := matchSegments(splitPat(c.pat), splitPath(c.path))
		if got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func splitPat(s string) []string  { return splitSep(s) }
func splitPath(s string) []string { return splitSep(s) }

func splitSep(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '/' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

func TestGlobFindsFiles(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "a.go")
	touch(t, root, "x/b.go")
	touch(t, root, "x/y/c.go")
	touch(t, root, "x/d.md")

	res, err := run(t, tool, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if p.Count != 3 {
		t.Fatalf("命中 %d 个，期望 3：%v", p.Count, p.Paths)
	}
	// 排序稳定，模型侧缓存友好。
	if p.Paths[0] != "a.go" || p.Paths[1] != "x/b.go" {
		t.Fatalf("顺序 = %v", p.Paths)
	}
}

func TestGlobSkipsNoiseDirs(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "keep.go")
	touch(t, root, "node_modules/dep/x.go")
	touch(t, root, ".git/y.go")

	res, err := run(t, tool, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if p.Count != 1 || p.Paths[0] != "keep.go" {
		t.Fatalf("噪声目录没被跳过：%v", p.Paths)
	}
}

func TestGlobSubdirBase(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "x/a.go")
	touch(t, root, "y/b.go")

	res, err := run(t, tool, map[string]any{"pattern": "*.go", "path": "x"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if p.Count != 1 || p.Paths[0] != "x/a.go" {
		t.Fatalf("子目录基准不对：%v", p.Paths)
	}
}

func TestGlobTruncates(t *testing.T) {
	tool, root := newTool(t)
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		touch(t, root, n)
	}
	res, err := run(t, tool, map[string]any{"pattern": "*.go", "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if p.Count != 2 || !p.Truncated {
		t.Fatalf("截断语义不对: count=%d truncated=%v", p.Count, p.Truncated)
	}
	if res.Hint == "" {
		t.Fatal("截断必须给 Hint")
	}
}

func TestGlobEmptyHasHint(t *testing.T) {
	tool, _ := newTool(t)
	res, err := run(t, tool, map[string]any{"pattern": "**/*.none"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Data.(payload).Count != 0 {
		t.Fatal("空结果 count 应为 0")
	}
	if res.Hint == "" {
		t.Fatal("空结果要给 Hint")
	}
}

func TestGlobEscapeRejected(t *testing.T) {
	tool, _ := newTool(t)
	res, err := run(t, tool, map[string]any{"pattern": "*", "path": ".."})
	if err == nil {
		t.Fatal("越界 path 必须被拒绝")
	}
	if res.Hint == "" {
		t.Fatal("失败要带 Hint")
	}
}

// 0 命中可能是"被 .dshdignore 挡住了"，不是"真的没有"。
// 两种情况模型下一步不一样：前者去改声明或换工具，后者去改 pattern。
func TestGlobZeroMatchMentionsIgnore(t *testing.T) {
	tl, root := newTool(t)
	if err := os.WriteFile(filepath.Join(root, ".dshdignore"), []byte("vendor"+string(rune(10))), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, root, "vendor/x/a.lua")

	res, err := tl.Exec(context.Background(), json.RawMessage(`{"pattern":"**/*.lua"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Hint, ".dshdignore") {
		t.Fatalf("0 命中且有声明时，Hint 必须点出 .dshdignore: %q（summary=%q）", res.Hint, res.Summary)
	}
	if !strings.Contains(res.Summary, "挡掉") {
		t.Fatalf("摘要也要说清挡掉了多少: %q", res.Summary)
	}
}
