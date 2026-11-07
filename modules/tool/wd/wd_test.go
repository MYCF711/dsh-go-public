package wd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 不带参数只报告，不改任何东西。
func TestReportsCurrentDir(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if !strings.Contains(res.Summary, filepath.ToSlash(cwd)) {
		t.Fatalf("要报告当前目录，实际: %q", res.Summary)
	}
	if d := res.Data.(data); d.Changed {
		t.Fatal("没给 path 不该改目录")
	}
}

// 切到真实子目录：真的改了，且说清 bash 那边没改。
func TestSwitchToSubdir(t *testing.T) {
	start, _ := os.Getwd()
	defer os.Chdir(start)

	tmp := t.TempDir()
	sub := filepath.Join(tmp, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"path": "`+filepath.ToSlash(sub)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(data)
	if !d.Changed {
		t.Fatal("给了 path 应该算改过")
	}
	now, _ := os.Getwd()
	if filepath.Clean(now) != filepath.Clean(sub) {
		t.Fatalf("没真切过去: %s", now)
	}
	// 已开着的 bash 会话有自己的 cwd，必须说清，否则模型会两边困惑
	if !strings.Contains(d.BashNote, "bash") {
		t.Fatalf("要说清 bash 会话不受影响: %q", d.BashNote)
	}
	if res.Hint == "" {
		t.Fatal("要带 Hint")
	}
}

// 目录不存在 / 是文件：报错并给下一步，不是静默失败。
func TestBadTargets(t *testing.T) {
	tool := &Tool{}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"path": "definitely-not-here-xyz"}`)); err == nil {
		t.Fatal("不存在的目录必须报错")
	} else if !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("错误要说清: %v", err)
	}

	f := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(f, []byte("x"), 0o644)
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"path": "`+filepath.ToSlash(f)+`"}`))
	if err == nil {
		t.Fatal("指向文件必须报错")
	}
	if !strings.Contains(res.Hint, "read") {
		t.Fatalf("要引导用 read: %q", res.Hint)
	}
}
