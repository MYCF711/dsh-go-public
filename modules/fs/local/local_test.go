package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newFS(t *testing.T) (*FS, string) {
	t.Helper()
	root := t.TempDir()
	fs, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	return fs, root
}

func write(t *testing.T, root, name, content string) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveRejectsEscape(t *testing.T) {
	fs, _ := newFS(t)
	if _, err := fs.Resolve("../outside.txt"); err == nil {
		t.Fatal("越界路径必须被拒绝")
	}
	abs := filepath.Join(os.TempDir(), "outside.txt")
	if _, err := fs.Resolve(abs); err == nil {
		t.Fatal("工作区外的绝对路径必须被拒绝")
	}
	if _, err := fs.Resolve("sub/../a.txt"); err != nil {
		t.Fatalf("工作区内的 .. 归一后应允许: %v", err)
	}
}

func TestReadThenWriteAllowed(t *testing.T) {
	fs, root := newFS(t)
	write(t, root, "a.txt", "hello")

	if _, err := fs.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.CheckWritable("a.txt", "write"); err != nil {
		t.Fatalf("读过之后应允许写: %v", err)
	}
	if err := fs.Write("a.txt", []byte("world")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "world" {
		t.Fatalf("内容 = %q", got)
	}
}

func TestBlindWriteRejected(t *testing.T) {
	fs, root := newFS(t)
	write(t, root, "a.txt", "hello")
	err := fs.CheckWritable("a.txt", "write")
	if err == nil {
		t.Fatal("没读过就改必须被拒绝")
	}
	// 一句话要说清「做了什么、为什么不行、下一步怎么做」——真实语料里 edit 的
	// 失败 84% 来自这条策略，措辞直接决定模型能不能一次改对。
	if !strings.Contains(err.Error(), "file has not been read") {
		t.Fatalf("错误信息要能指导下一步: %v", err)
	}
	if !strings.Contains(err.Error(), "read the file, then retry") {
		t.Fatalf("要直接给出下一步动作: %v", err)
	}
}

func TestStaleWriteRejected(t *testing.T) {
	fs, root := newFS(t)
	p := write(t, root, "a.txt", "hello")
	if _, err := fs.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	// 模拟别人在我们读过之后改了文件。
	if err := os.WriteFile(p, []byte("changed by someone"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, time.Now(), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	err := fs.CheckWritable("a.txt", "write")
	if err == nil || !strings.Contains(err.Error(), "file changed since it was read") {
		t.Fatalf("过期观测必须要求重读，得到 %v", err)
	}
	// 措辞按工具区分动词：写用 cannot write，改用 cannot edit。
	if !strings.Contains(err.Error(), `cannot write "a.txt"`) {
		t.Fatalf("动词不对: %v", err)
	}
	if _, err := fs.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.CheckWritable("a.txt", "write"); err != nil {
		t.Fatalf("重读之后应允许写: %v", err)
	}
}

func TestCreateNewFileNeedsNoRead(t *testing.T) {
	fs, root := newFS(t)
	if err := fs.CheckWritable("new/dir/b.txt", "write"); err != nil {
		t.Fatalf("新建文件不该要求先读: %v", err)
	}
	if err := fs.Write("new/dir/b.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "new/dir/b.txt")); err != nil {
		t.Fatal("父目录应被自动创建")
	}
}

func TestWriteMarksObserved(t *testing.T) {
	fs, _ := newFS(t)
	if err := fs.Write("c.txt", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := fs.CheckWritable("c.txt", "write"); err != nil {
		t.Fatalf("自己写的文件应视为已观测: %v", err)
	}
}

func TestStat(t *testing.T) {
	fs, root := newFS(t)
	write(t, root, "d.txt", "abc")
	size, exists, err := fs.Stat("d.txt")
	if err != nil || !exists || size != 3 {
		t.Fatalf("Stat = %d,%v,%v", size, exists, err)
	}
	_, exists, err = fs.Stat("nope.txt")
	if err != nil || exists {
		t.Fatalf("不存在的文件应返回 exists=false，得到 %v %v", exists, err)
	}
}
