package read

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

func fixture(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, tool *Tool, args map[string]any) (spi.Result, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Exec(context.Background(), raw)
}

func TestExecReadsLines(t *testing.T) {
	tool, root := newTool(t)
	fixture(t, root, "sample.txt", "a\nb\nc\n")

	res, err := call(t, tool, map[string]any{"file_path": "sample.txt"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	data, ok := res.Data.(payload)
	if !ok {
		t.Fatalf("Data 类型 = %T", res.Data)
	}
	if data.TotalLines != 3 {
		t.Fatalf("TotalLines = %d，期望 3（尾换行不该多算一行）", data.TotalLines)
	}
	if data.Content != "a\nb\nc" {
		t.Fatalf("Content = %q", data.Content)
	}
	if data.Truncated {
		t.Fatal("未超限不应标记截断")
	}
}

func TestExecOffsetAndLimit(t *testing.T) {
	tool, root := newTool(t)
	fixture(t, root, "n.txt", "1\n2\n3\n4\n5\n")
	res, err := call(t, tool, map[string]any{"file_path": "n.txt", "offset": 2, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(payload)
	if data.StartLine != 2 || data.EndLine != 3 || data.Content != "2\n3" {
		t.Fatalf("窗口 = %d-%d %q", data.StartLine, data.EndLine, data.Content)
	}
	if !data.Truncated {
		t.Fatal("还有剩余行时应标记 Truncated")
	}
	if res.Hint == "" || !strings.Contains(res.Hint, "offset=4") {
		t.Fatalf("截断时必须给下一步（Hint），得到 %q", res.Hint)
	}
}

func TestExecMissingFileHasHint(t *testing.T) {
	tool, _ := newTool(t)
	res, err := call(t, tool, map[string]any{"file_path": "nope.txt"})
	if err == nil {
		t.Fatal("读不存在的文件必须报错")
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint（R3）")
	}
}

func TestExecBadArgsHasHint(t *testing.T) {
	tool, _ := newTool(t)
	res, err := tool.Exec(context.Background(), json.RawMessage(`{`))
	if err == nil {
		t.Fatal("非法 JSON 必须报错")
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint（R3）")
	}
}

func TestSchemaIsStableJSON(t *testing.T) {
	tool, _ := newTool(t)
	var v any
	if err := json.Unmarshal(tool.Schema(), &v); err != nil {
		t.Fatalf("Schema 必须是合法 JSON: %v", err)
	}
	if strings.Contains(string(tool.Schema()), "\\\\") {
		t.Fatal("Schema 里出现反斜杠转义，可能混入了机器路径")
	}
}

// offset 越过文件末尾必须报错。
// 早先是静默返回空内容 —— 模型会以为"这一段是空的"，那是错误答案，比报错糟得多。
func TestReadOffsetOutOfRange(t *testing.T) {
	tl, root := newTool(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("1\n2\n3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tl.Exec(context.Background(), json.RawMessage(`{"file_path":"a.txt","offset":99}`))
	if err == nil {
		t.Fatalf("越过末尾应该报错，却返回了 %+v", res)
	}
	if !strings.Contains(err.Error(), "超出范围") {
		t.Fatalf("错误信息要说清原因: %v", err)
	}
	if res.Hint == "" {
		t.Fatal("要给出 Hint：告诉模型该用多少 offset")
	}
}

// 但空文件上的默认 offset 不该报错。
func TestReadEmptyFileOK(t *testing.T) {
	tl, root := newTool(t)
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Exec(context.Background(), json.RawMessage(`{"file_path":"empty.txt"}`)); err != nil {
		t.Fatalf("空文件不该报错: %v", err)
	}
}
