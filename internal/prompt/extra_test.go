package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ExtraRulesPath 指向别的文件时，用它；否则回落到工作区的 AGENTS.md。
func TestExtraRulesPathWins(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("维护者手册"), 0o644)

	got := Default(Context{Cwd: dir, MaxWorkspaceBytes: 4096})
	if !strings.Contains(got, "维护者手册") {
		t.Fatal("默认应当读到 AGENTS.md")
	}

	worker := filepath.Join(dir, "WORKER.md")
	os.WriteFile(worker, []byte("执行者手册"), 0o644)
	got = Default(Context{Cwd: dir, MaxWorkspaceBytes: 4096, ExtraRulesPath: worker})
	if !strings.Contains(got, "执行者手册") {
		t.Fatal("指定了 ExtraRulesPath 就该用它")
	}
	if strings.Contains(got, "维护者手册") {
		t.Fatal("两本手册不能同时进提示词")
	}
}

// 相对路径按 Cwd 解析——派活时写 docs/WORKER.md 更顺手。
func TestExtraRulesPathRelative(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "W.md"), []byte("相对路径手册"), 0o644)
	got := Default(Context{Cwd: dir, MaxWorkspaceBytes: 4096, ExtraRulesPath: "W.md"})
	if !strings.Contains(got, "相对路径手册") {
		t.Fatal("相对路径要按 Cwd 解析")
	}
}

// 指定的文件不存在时回落，而不是把提示词清空。
func TestExtraRulesPathMissingFallsBack(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("兜底手册"), 0o644)
	got := Default(Context{Cwd: dir, MaxWorkspaceBytes: 4096, ExtraRulesPath: "不存在.md"})
	if !strings.Contains(got, "兜底手册") {
		t.Fatal("文件不存在应当回落到 AGENTS.md")
	}
}
