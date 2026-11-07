package write

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

func newTool(t *testing.T) (*Tool, *local.FS, string) {
	t.Helper()
	root := t.TempDir()
	fs, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Tool{fs: fs}, fs, root
}

func call(t *testing.T, tool *Tool, args map[string]any) (spi.Result, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Exec(context.Background(), raw)
}

func TestCreateNewFile(t *testing.T) {
	tool, _, root := newTool(t)
	res, err := call(t, tool, map[string]any{"file_path": "a/b.txt", "content": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Data.(payload)
	if !p.Created {
		t.Fatal("新建文件应标记 created")
	}
	got, err := os.ReadFile(filepath.Join(root, "a/b.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("落盘内容 = %q, %v", got, err)
	}
}

func TestOverwriteWithoutReadIsRejected(t *testing.T) {
	tool, _, root := newTool(t)
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := call(t, tool, map[string]any{"file_path": "x.txt", "content": "new"})
	if err == nil {
		t.Fatal("没读过就覆盖必须被拒绝")
	}
	if res.Hint == "" || !strings.Contains(res.Hint, "read") {
		t.Fatalf("拒绝时要告诉模型先 read，得到 %q", res.Hint)
	}
}

func TestOverwriteAfterReadIsAllowed(t *testing.T) {
	tool, fs, root := newTool(t)
	if err := os.WriteFile(filepath.Join(root, "y.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Read("y.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, tool, map[string]any{"file_path": "y.txt", "content": "new"}); err != nil {
		t.Fatalf("读过之后应允许覆盖: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "y.txt"))
	if string(got) != "new" {
		t.Fatalf("内容 = %q", got)
	}
}

func TestPathEscapeRejected(t *testing.T) {
	tool, _, _ := newTool(t)
	res, err := call(t, tool, map[string]any{"file_path": "../escape.txt", "content": "x"})
	if err == nil {
		t.Fatal("越界写必须被拒绝")
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint")
	}
}

func TestEmptyPathRejected(t *testing.T) {
	tool, _, _ := newTool(t)
	if _, err := call(t, tool, map[string]any{"file_path": "", "content": "x"}); err == nil {
		t.Fatal("空路径必须报错")
	}
}
