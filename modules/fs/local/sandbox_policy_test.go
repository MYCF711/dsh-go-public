package local

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dshgo/internal/sandbox"
)

func mustBoundary(t *testing.T, mode sandbox.Mode, root string) *sandbox.Boundary {
	t.Helper()
	b, err := sandbox.NewBoundary(mode, root)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func makeDirLink(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("创建目录符号链接: %v", err)
		}
		return
	}
	if err := os.Symlink(target, link); err == nil {
		return
	}
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("创建 Windows directory junction 失败: %v (%s)", err, output)
	}
}

// read-only：一切写都拒绝，包括工作区内新建；已有文件内容不变。
func TestSandboxReadOnlyDeniesAllWrites(t *testing.T) {
	root := t.TempDir()
	existing := write(t, root, "keep.txt", "original")
	fs, err := New(root, WithSandboxBoundary(mustBoundary(t, sandbox.ModeReadOnly, root)))
	if err != nil {
		t.Fatal(err)
	}
	if fs.SandboxMode() != sandbox.ModeReadOnly {
		t.Fatalf("FS 档位 = %s", fs.SandboxMode())
	}

	if err := fs.CheckWritable("new/dir/x.txt", "write"); !errors.Is(err, sandbox.ErrReadOnlyWrite) {
		t.Fatalf("read-only 必须拒绝新建，得到 %v", err)
	}
	if err := fs.Write("new/dir/x.txt", []byte("x")); !errors.Is(err, sandbox.ErrReadOnlyWrite) {
		t.Fatalf("read-only Write 必须拒绝，得到 %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "dir", "x.txt")); err == nil {
		t.Fatal("read-only 会话仍然写出了文件")
	}

	if _, err := fs.Read("keep.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.CheckWritable("keep.txt", "write"); !errors.Is(err, sandbox.ErrReadOnlyWrite) {
		t.Fatalf("已有文件也必须拒绝写，得到 %v", err)
	}
	if err := fs.Write("keep.txt", []byte("changed")); !errors.Is(err, sandbox.ErrReadOnlyWrite) {
		t.Fatalf("Write 必须拒绝，得到 %v", err)
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("read-only 会话改掉了文件: %q", got)
	}
}

// workspace-write：只允许 canonical 工作区；越界、前缀兄弟、符号链接越界都拒绝。
func TestSandboxWorkspaceWriteCanonicalBoundary(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	fs, err := New(root, WithSandboxBoundary(mustBoundary(t, sandbox.ModeWorkspaceWrite, root)))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Write("inside/new.txt", []byte("ok")); err != nil {
		t.Fatalf("工作区内写应允许: %v", err)
	}
	outsideFile := filepath.Join(outside, "escaped.txt")
	if err := fs.Write(outsideFile, []byte("no")); !errors.Is(err, sandbox.ErrWriteOutsideWorkspace) {
		t.Fatalf("工作区外写必须拒绝，得到 %v", err)
	}
	if err := fs.Write(root+"-evil/x.txt", []byte("no")); !errors.Is(err, sandbox.ErrWriteOutsideWorkspace) {
		t.Fatalf("前缀兄弟目录必须拒绝，得到 %v", err)
	}

	link := filepath.Join(root, "link")
	makeDirLink(t, link, outside)
	throughLink := filepath.Join(link, "not-created", "deep", "x.txt")
	if err := fs.Write(throughLink, []byte("no")); !errors.Is(err, sandbox.ErrWriteOutsideWorkspace) {
		t.Fatalf("经符号链接的越界写必须按 canonical 路径拒绝，得到 %v", err)
	}

	// Resolve 也要按 canonical 规则挡住读取到的越界路径。
	if _, err := fs.Resolve(outsideFile); err == nil {
		t.Fatal("工作区外绝对路径 Resolve 必须拒绝")
	}
}

// danger-full-access：不虚构路径沙盒，但读后写/防陈旧覆盖保留。
func TestSandboxDangerFullAccessKeepsStaleWriteGuard(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	fs, err := New(root, WithSandboxBoundary(mustBoundary(t, sandbox.ModeDangerFullAccess, root)))
	if err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "outside.txt")
	if err := fs.Write(outsideFile, []byte("written")); err != nil {
		t.Fatalf("danger-full-access 应允许工作区外写: %v", err)
	}
	if _, err := fs.Resolve(outsideFile); err != nil {
		t.Fatalf("danger-full-access 应允许工作区外 Resolve: %v", err)
	}

	// 工作区内已有文件：没读过就不许改，danger 档也一样。
	write(t, root, "a.txt", "hello")
	if err := fs.CheckWritable("a.txt", "write"); err == nil || !strings.Contains(err.Error(), "file has not been read") {
		t.Fatalf("danger 档仍必须保留读后写，得到 %v", err)
	}
	if _, err := fs.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.CheckWritable("a.txt", "write"); err != nil {
		t.Fatalf("读过之后应允许写: %v", err)
	}
}

// 环境：未设置 = workspace-write（只收窄）；未知值 fail-loud。
func TestSandboxFSEnvDefaultAndUnknown(t *testing.T) {
	root := t.TempDir()
	t.Setenv(sandbox.EnvMode, "")
	fs, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if fs.SandboxMode() != sandbox.ModeWorkspaceWrite {
		t.Fatalf("缺省 FS 档位 = %s，期望 workspace-write", fs.SandboxMode())
	}
	if err := fs.Write(filepath.Join(t.TempDir(), "x.txt"), []byte("x")); !errors.Is(err, sandbox.ErrWriteOutsideWorkspace) {
		t.Fatalf("缺省档位必须仍然只写工作区，得到 %v", err)
	}

	t.Setenv(sandbox.EnvMode, "read-only")
	fs, err = New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Write("x.txt", []byte("x")); !errors.Is(err, sandbox.ErrReadOnlyWrite) {
		t.Fatalf("环境 read-only 没生效: %v", err)
	}

	t.Setenv(sandbox.EnvMode, "yolo")
	if _, err := New(root); !errors.Is(err, sandbox.ErrUnknownMode) {
		t.Fatalf("未知环境档位必须在装配期 fail-loud，得到 %v", err)
	}
}

// SetRoot 换根后档位不变（read-only 不能因为换根变成可写）。
func TestSandboxSetRootKeepsMode(t *testing.T) {
	root := t.TempDir()
	next := t.TempDir()
	fs, err := New(root, WithSandboxBoundary(mustBoundary(t, sandbox.ModeReadOnly, root)))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.SetRoot(next); err != nil {
		t.Fatal(err)
	}
	if fs.SandboxMode() != sandbox.ModeReadOnly {
		t.Fatalf("SetRoot 后档位 = %s", fs.SandboxMode())
	}
	if err := fs.Write("x.txt", []byte("x")); !errors.Is(err, sandbox.ErrReadOnlyWrite) {
		t.Fatalf("SetRoot 后 read-only 必须仍拒绝写，得到 %v", err)
	}
}
