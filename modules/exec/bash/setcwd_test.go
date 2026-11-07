package bash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 换默认目录之后，新开 shell 的 pwd 就是新目录——这是 ACP 客户端带 cwd 来的那条路。
func TestSetCwdAffectsNewShell(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(t.TempDir())
	defer svc.Close()
	if err := svc.SetCwd(dir); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Run("pwd", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(res.Output)
	want, _ := filepath.Abs(dir)
	// Git Bash 的 pwd 输出 /d/... 形式，比对尾部即可。
	if !strings.HasSuffix(filepath.ToSlash(got), filepath.Base(want)) {
		t.Fatalf("pwd = %q，期望在 %q 下", got, want)
	}
}

// 不存在的目录要报错，别把默认目录悄悄改坏。
func TestSetCwdRejectsMissing(t *testing.T) {
	svc := NewService(t.TempDir())
	defer svc.Close()
	if err := svc.SetCwd(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("不存在的目录要报错")
	}
	if err := svc.SetCwd(""); err == nil {
		t.Fatal("空目录要报错")
	}
	if _, err := os.Stat(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
