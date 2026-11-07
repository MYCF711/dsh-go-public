package modes

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dshgo/internal/sandbox"
)

// 三档真值：preset 的 sandbox 字段就是 runtime 档位，不是显示值。
func TestBuiltinSandboxMapping(t *testing.T) {
	want := map[string]sandbox.Mode{
		DefaultID:            sandbox.ModeWorkspaceWrite,
		"read-only":          sandbox.ModeReadOnly,
		"workspace-write":    sandbox.ModeWorkspaceWrite,
		"danger-full-access": sandbox.ModeDangerFullAccess,
	}
	for _, m := range Builtins() {
		mode, err := m.SandboxMode()
		if err != nil {
			t.Fatalf("%s: %v", m.ID, err)
		}
		expected, ok := want[m.ID]
		if !ok {
			t.Fatalf("新增内置模式 %q 没有登记期望档位", m.ID)
		}
		if mode != expected {
			t.Fatalf("%s 的 sandbox = %s，期望 %s", m.ID, mode, expected)
		}
	}
	for id := range want {
		if _, err := Resolve(id); err != nil {
			t.Fatalf("三档 preset %q 不存在: %v", id, err)
		}
	}
}

func TestSandboxModeRejectsUnknownValue(t *testing.T) {
	for _, raw := range []string{"default", "yolo", "workspace_write", "danger"} {
		m := Mode{ID: "custom", Sandbox: raw}
		if _, err := m.SandboxMode(); !errors.Is(err, sandbox.ErrUnknownMode) {
			t.Fatalf("sandbox=%q 应 fail-loud，得到 %v", raw, err)
		}
	}
	// 空值 = workspace-write（安全默认：只收窄不放宽）。
	m := Mode{ID: "custom"}
	if mode, err := m.SandboxMode(); err != nil || mode != sandbox.ModeWorkspaceWrite {
		t.Fatalf("空 sandbox 默认 = %v, %v", mode, err)
	}
	// 合法值原样生效。
	m = Mode{ID: "custom", Sandbox: "danger-full-access"}
	if mode, err := m.SandboxMode(); err != nil || mode != sandbox.ModeDangerFullAccess {
		t.Fatalf("显式 danger-full-access = %v, %v", mode, err)
	}
}

func TestUserModeSandboxFieldLoadAndInherit(t *testing.T) {
	dir := t.TempDir()
	writeMode := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMode("child.toml", "id = \"child\"\ncopy_of = \"workspace-write\"\n")
	got, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Mode{}
	for _, m := range got {
		byID[m.ID] = m
	}
	child, ok := byID["child"]
	if !ok {
		t.Fatal("copy_of 出来的模式丢了")
	}
	mode, err := child.SandboxMode()
	if err != nil || mode != sandbox.ModeWorkspaceWrite {
		t.Fatalf("copy_of 应继承 sandbox: %v %v", mode, err)
	}
	if len(child.Tools) != 0 {
		t.Fatalf("copy_of 的工具继承不应凭空产生工具: %+v", child.Tools)
	}
}

func TestUserModeUnknownSandboxFailsAtLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.toml"), []byte("id = \"bad\"\nsandbox = \"yolo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); !errors.Is(err, sandbox.ErrUnknownMode) {
		t.Fatalf("未知 sandbox 必须在装配期 fail-loud，得到 %v", err)
	}
}
