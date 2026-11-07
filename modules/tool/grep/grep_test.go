package grep

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

func touch(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, tool *Tool, args map[string]any) (spi.Result, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tool.Exec(context.Background(), raw)
}

func TestFindsMatchesWithLineNumbers(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "a.go", "package a\n\nfunc Foo() {}\n")
	touch(t, root, "b.txt", "nothing here\n")

	res, err := run(t, tool, map[string]any{"pattern": "func Foo"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if p.Count != 1 {
		t.Fatalf("命中 %d 条：%+v", p.Count, p.Matches)
	}
	m := p.Matches[0]
	if m.Path != "a.go" || m.Line != 3 {
		t.Fatalf("命中位置不对：%+v", m)
	}
}

func TestIncludeFilter(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "a.go", "target\n")
	touch(t, root, "b.txt", "target\n")

	res, err := run(t, tool, map[string]any{"pattern": "target", "include": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if p.Count != 1 || p.Matches[0].Path != "a.go" {
		t.Fatalf("include 过滤失效：%+v", p.Matches)
	}
}

func TestIgnoreCase(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "a.go", "Hello\n")
	res, err := run(t, tool, map[string]any{"pattern": "hello", "ignore_case": true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Data.(payload).Count != 1 {
		t.Fatal("ignore_case 未生效")
	}
}

func TestBinarySkipped(t *testing.T) {
	tool, root := newTool(t)
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte("abc\x00target"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, root, "a.go", "target\n")
	res, err := run(t, tool, map[string]any{"pattern": "target"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if p.Count != 1 || p.FilesSkipped == 0 {
		t.Fatalf("二进制应被跳过：count=%d skipped=%d", p.Count, p.FilesSkipped)
	}
}

func TestLimitTruncates(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "a.go", "hit\nhit\nhit\nhit\n")
	res, err := run(t, tool, map[string]any{"pattern": "hit", "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if p.Count != 2 || !p.Truncated {
		t.Fatalf("截断语义不对：count=%d truncated=%v", p.Count, p.Truncated)
	}
	if !strings.Contains(res.Hint, "include") {
		t.Fatalf("截断 Hint 应给出收窄办法：%q", res.Hint)
	}
}

func TestBadRegexHasHint(t *testing.T) {
	tool, _ := newTool(t)
	res, err := run(t, tool, map[string]any{"pattern": "([a-z"})
	if err == nil {
		t.Fatal("非法正则必须报错")
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint")
	}
}

func TestNoMatchHasHint(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "a.go", "abc\n")
	res, err := run(t, tool, map[string]any{"pattern": "zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hint == "" {
		t.Fatal("空结果要给 Hint")
	}
}

func TestTableView(t *testing.T) {
	tool, root := newTool(t)
	touch(t, root, "a.go", "target\n")
	res, err := run(t, tool, map[string]any{"pattern": "target"})
	if err != nil {
		t.Fatal(err)
	}
	if res.View == nil || res.View.Kind != spi.ViewTable {
		t.Fatalf("grep 应给表格视图，得到 %+v", res.View)
	}
	if len(res.View.Rows) != 1 || res.View.Rows[0][0] != "a.go" {
		t.Fatalf("表格行不对：%+v", res.View.Rows)
	}
}

// 搜索一个不存在的路径必须报错，不能返回"0 处命中"。
// 后者会让模型以为"这个目录里没有这个符号"，而真实情况是路径就错了。
func TestGrepMissingPathErrors(t *testing.T) {
	dir := t.TempDir()
	tl, _ := newTool(t)
	_ = dir
	_, err := tl.Exec(context.Background(), json.RawMessage(`{"pattern":"x","path":"no-such-dir"}`))
	if err == nil {
		t.Fatal("路径不存在必须报错")
	}
}

// 0 命中且有 .dshdignore 时，Hint 必须点出"可能被挡住了"。
// 和 glob 同一个坑：把"声明挡住了"说成"没有匹配"，模型会去改 pattern。
func TestGrepZeroHitMentionsIgnore(t *testing.T) {
	tl, root := newTool(t)
	touch(t, root, ".dshdignore", "vendor")
	touch(t, root, "vendor/a.txt", "NEEDLE_XYZ")

	res, err := tl.Exec(context.Background(), json.RawMessage(`{"pattern":"NEEDLE_XYZ"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Hint, ".dshdignore") {
		t.Fatalf("0 命中且有声明时，Hint 必须点出 .dshdignore: %q（summary=%q）", res.Hint, res.Summary)
	}
}
