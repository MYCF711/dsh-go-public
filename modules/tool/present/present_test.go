package present

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mk(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 声明一个真实文件：带字节数，路径是正斜杠。
func TestPresentsExistingFile(t *testing.T) {
	p := mk(t, "report.md", "hello")
	tool := &Tool{}
	res, err := tool.Exec(context.Background(),
		json.RawMessage(`{"files":[{"path":"`+filepath.ToSlash(p)+`","description":"报告"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(data)
	if d.Count != 1 || d.Files[0].Bytes != 5 {
		t.Fatalf("要带真实字节数: %+v", d)
	}
	if !strings.Contains(res.Summary, "report.md") {
		t.Fatalf("摘要要点名: %q", res.Summary)
	}
}

// 文件不存在：必须报错并给"用 glob 确认"的下一步。
// 不校验的话模型会说"已交付"，人打开却是空的——声明的价值全在真实。
func TestMissingFileIsError(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(),
		json.RawMessage(`{"files":[{"path":"nope-does-not-exist.pdf"}]}`))
	if err == nil {
		t.Fatal("文件不存在必须报错")
	}
	if !strings.Contains(res.Hint, "glob") {
		t.Fatalf("Hint 要指路: %q", res.Hint)
	}
}

// 目录不是交付物。
func TestDirIsError(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(),
		json.RawMessage(`{"files":[{"path":"`+filepath.ToSlash(t.TempDir())+`"}]}`))
	if err == nil || !strings.Contains(res.Hint, "目录") {
		t.Fatalf("目录要报错并说清: %v %q", err, res.Hint)
	}
}

// 空文件：算存在，但值得拦一下——多半是写失败了。
func TestEmptyFileIsRejected(t *testing.T) {
	p := mk(t, "empty.txt", "")
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"files":[{"path":"`+filepath.ToSlash(p)+`"}]}`))
	if err == nil || !strings.Contains(res.Hint, "空文件") {
		t.Fatalf("空文件要提示: %v %q", err, res.Hint)
	}
}

// 上限 4 个；空数组与缺字段都要报错（没有"清空"语义）。
func TestLimitsAndEmpty(t *testing.T) {
	tool := &Tool{}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("缺 files 必须报错")
	}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"files":[]}`))
	if err == nil || !strings.Contains(res.Hint, "至少") {
		t.Fatalf("空数组要报错: %v %q", err, res.Hint)
	}

	one := mk(t, "a.txt", "x")
	var parts []string
	for i := 0; i < 5; i++ {
		parts = append(parts, `{"path":"`+filepath.ToSlash(one)+`"}`)
	}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"files":[`+strings.Join(parts, ",")+`]}`)); err == nil {
		t.Fatal("超过 4 个必须报错")
	}
}
