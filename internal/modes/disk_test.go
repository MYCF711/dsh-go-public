package modes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMode(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirEmpty(t *testing.T) {
	got, err := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(got) != 0 {
		t.Fatalf("目录不存在应该返回空且无错：%v %v", got, err)
	}
	if got, err := LoadDir(""); err != nil || got != nil {
		t.Fatal("空 dir 应该直接返回 nil")
	}
}

func TestLoadDirReadsMode(t *testing.T) {
	dir := t.TempDir()
	writeMode(t, dir, "strict.toml", `
id = "strict"
name = "Strict"
description = "只允许读和写"
tools = ["read", "write"]
approval = "ask"
`)
	ms, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != "strict" {
		t.Fatalf("读到 %+v", ms)
	}
	if ms[0].Builtin {
		t.Fatal("用户模式不该是 builtin")
	}
	if !ms[0].Allows("write") || ms[0].Allows("bash") {
		t.Fatalf("工具白名单不对: %+v", ms[0].Tools)
	}
}

func TestLoadDirCopyOf(t *testing.T) {
	dir := t.TempDir()
	writeMode(t, dir, "ro-plus.toml", `
id = "ro-plus"
copy_of = "read-only"
`)
	ms, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("读到 %d 个", len(ms))
	}
	if !ms[0].Allows("grep") {
		t.Fatal("copy_of 应该继承 read-only 的工具白名单")
	}
	if ms[0].Allows("bash") {
		t.Fatal("copy_of 不该凭空多出 bash")
	}
	if ms[0].Name != "ro-plus" {
		t.Fatalf("没给 name 时应该回落到 id，得到 %q", ms[0].Name)
	}
}

func TestLoadDirRejectsBuiltinID(t *testing.T) {
	dir := t.TempDir()
	writeMode(t, dir, "evil.toml", `
id = "default"
tools = ["read"]
`)
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("用内置 id 必须报错（内置模式只读）")
	}
	if !strings.Contains(err.Error(), "内置") {
		t.Fatalf("错误信息要说清原因: %v", err)
	}
}

func TestLoadDirRejectsSelfDeclaredBuiltin(t *testing.T) {
	dir := t.TempDir()
	writeMode(t, dir, "x.toml", `
id = "owned"
builtin = true
`)
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("用户模式自称 builtin 必须报错")
	}
}

func TestLoadDirRejectsBadCopyOf(t *testing.T) {
	dir := t.TempDir()
	writeMode(t, dir, "x.toml", `
id = "x"
copy_of = "no-such-mode"
`)
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("copy_of 指向不存在的模式必须报错")
	}
}

func TestLoadDirRejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeMode(t, dir, "a.toml", "id = \"dup\"\n")
	writeMode(t, dir, "b.toml", "id = \"dup\"\n")
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("同一个 id 出现在两个文件里必须报错")
	}
}

func TestRegistryCombines(t *testing.T) {
	reg, err := NewRegistry([]Mode{{ID: "mine", Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve("mine"); err != nil {
		t.Fatal("用户模式应该能解析到")
	}
	if _, err := reg.Resolve("read-only"); err != nil {
		t.Fatal("内置模式仍要在")
	}
	if _, err := reg.Resolve("nope"); err == nil {
		t.Fatal("未知模式必须报错")
	}
	if got := reg.IDs(); !strings.Contains(got, "mine") || !strings.Contains(got, "default") {
		t.Fatalf("IDs = %q", got)
	}
}
