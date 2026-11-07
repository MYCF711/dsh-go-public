package local

import (
	"os"
	"path/filepath"
	"testing"
)

// 换根之后，相对路径要按**新根**解析——这是 ACP 客户端带 cwd 来的那条路。
func TestSetRootChangesResolution(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(b, "only-in-b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := New(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read("only-in-b.txt"); err == nil {
		t.Fatal("根还是 A 的时候不该读到 B 的文件")
	}
	if err := f.SetRoot(b); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read("only-in-b.txt"); err != nil {
		t.Fatalf("换根之后要能读到: %v", err)
	}
	if _, err := f.Read(filepath.Join(a, "only-in-b.txt")); err == nil {
		t.Fatal("换根之后旧根的文件就应当在界外")
	}
}

// 换根要清掉"读过哪些文件"的账，否则写类工具会因为旧账直接放行或拦截。
func TestSetRootClearsSeen(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(a, "f.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(b, "f.txt"), []byte("b"), 0o644)

	f, _ := New(a)
	if _, err := f.Read("f.txt"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetRoot(b); err != nil {
		t.Fatal(err)
	}
	// 换根后同名文件是**另一个**文件：原来 A 的 f.txt 已经读过，
	// 换到 B 之后写 B 的 f.txt 必须重新走"先读再写"。
	if err := f.CheckWritable("f.txt", "write"); err == nil {
		t.Fatal("换根后旧根的读记录必须清空，否则会放行没读过的文件")
	}
}

// 不存在的目录要报错，而不是把根悄悄改坏。
func TestSetRootRejectsMissing(t *testing.T) {
	f, _ := New(t.TempDir())
	if err := f.SetRoot(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("不存在的目录要报错")
	}
	if err := f.SetRoot(""); err == nil {
		t.Fatal("空的根要报错")
	}
}
