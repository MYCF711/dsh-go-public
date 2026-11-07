package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func rewindWrite(t *testing.T, root, name string, data []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func rewindRead(t *testing.T, root, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func rewindAssertFile(t *testing.T, root, name string, want []byte) {
	t.Helper()
	if got := rewindRead(t, root, name); !bytes.Equal(got, want) {
		t.Fatalf("%s = %q，期望 %q", name, got, want)
	}
}

func rewindAssertMissing(t *testing.T, root, name string) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s 应不存在，err=%v", name, err)
	}
}

func rewindManifest(t *testing.T, home, id string, seq int64) string {
	t.Helper()
	dir, err := rewindCheckpointDir(home, id, seq)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "manifest.json")
}

func TestRewindPreviewRestoreModifyAddDeleteAndRestart(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const id = "rewind-basic"
	modifiedBefore := []byte("one\ntwo\n")
	deletedBefore := []byte("put me back\n")
	rewindWrite(t, cwd, "modified.txt", modifiedBefore)
	rewindWrite(t, cwd, "deleted.txt", deletedBefore)
	rewindWrite(t, cwd, "same.txt", []byte("same\n"))
	rewindWrite(t, cwd, ".git/config", []byte("git-before"))
	rewindWrite(t, cwd, "node_modules/pkg/index.js", []byte("module-before"))

	if err := CaptureRewindCheckpoint(home, id, 7, cwd); err != nil {
		t.Fatal(err)
	}
	empty, err := PreviewRewindFiles(home, id, 7, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if empty.FilesChanged == nil || len(empty.FilesChanged) != 0 || empty.Insertions != 0 || empty.Deletions != 0 {
		t.Fatalf("完整 checkpoint 的空差异 = %+v", empty)
	}

	modifiedAfter := []byte("one\nchanged\nthree\n")
	addedAfter := []byte("new\nfile\n")
	rewindWrite(t, cwd, "modified.txt", modifiedAfter)
	rewindWrite(t, cwd, "added.txt", addedAfter)
	if err := os.Remove(filepath.Join(cwd, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	rewindWrite(t, cwd, ".git/config", []byte("git-after"))
	rewindWrite(t, cwd, "node_modules/pkg/index.js", []byte("module-after"))

	want := RewindFilesPreview{
		FilesChanged: []string{"added.txt", "deleted.txt", "modified.txt"},
		Insertions:   2,
		Deletions:    4,
	}
	preview, err := PreviewRewindFiles(home, id, 7, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preview, want) {
		t.Fatalf("preview = %+v，期望 %+v", preview, want)
	}
	again, err := PreviewRewindFiles(home, id, 7, cwd)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("dry-run 重复读取 = %+v, %v", again, err)
	}
	rewindAssertFile(t, cwd, "modified.txt", modifiedAfter)
	rewindAssertFile(t, cwd, "added.txt", addedAfter)
	rewindAssertMissing(t, cwd, "deleted.txt")

	// 不依赖 Capture 留下任何进程内状态：后续调用只读 home 中的 checkpoint。
	restored, err := RestoreRewindFiles(home, id, 7, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, want) {
		t.Fatalf("restore preview = %+v，期望 %+v", restored, want)
	}
	rewindAssertFile(t, cwd, "modified.txt", modifiedBefore)
	rewindAssertFile(t, cwd, "deleted.txt", deletedBefore)
	rewindAssertMissing(t, cwd, "added.txt")
	rewindAssertFile(t, cwd, ".git/config", []byte("git-after"))
	rewindAssertFile(t, cwd, "node_modules/pkg/index.js", []byte("module-after"))

	after, err := PreviewRewindFiles(home, id, 7, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if after.FilesChanged == nil || len(after.FilesChanged) != 0 || after.Insertions != 0 || after.Deletions != 0 {
		t.Fatalf("恢复后仍有差异: %+v", after)
	}

	// 再次修改并重新调用，模拟另一个进程读取同一 durable checkpoint。
	rewindWrite(t, cwd, "modified.txt", []byte("other process\n"))
	if _, err := RestoreRewindFiles(home, id, 7, cwd); err != nil {
		t.Fatal(err)
	}
	rewindAssertFile(t, cwd, "modified.txt", modifiedBefore)
}

func TestRewindBinarySafe(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	before := []byte{0, 1, 0, 2, 255, '\n', 0}
	rewindWrite(t, cwd, "binary.dat", before)
	if err := CaptureRewindCheckpoint(home, "binary", 1, cwd); err != nil {
		t.Fatal(err)
	}
	rewindWrite(t, cwd, "binary.dat", []byte{0, 3, 254})
	rewindWrite(t, cwd, "new-binary.dat", []byte{0, 0, 255})
	preview, err := PreviewRewindFiles(home, "binary", 1, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preview.FilesChanged, []string{"binary.dat", "new-binary.dat"}) {
		t.Fatalf("二进制变更列表 = %v", preview.FilesChanged)
	}
	if preview.Insertions < 0 || preview.Deletions < 0 {
		t.Fatalf("差异计数不能为负: %+v", preview)
	}
	if _, err := RestoreRewindFiles(home, "binary", 1, cwd); err != nil {
		t.Fatal(err)
	}
	rewindAssertFile(t, cwd, "binary.dat", before)
	rewindAssertMissing(t, cwd, "new-binary.dat")
}

func TestRewindMissingCorruptAndIncompleteCheckpoint(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		if preview, err := PreviewRewindFiles(t.TempDir(), "none", 1, t.TempDir()); err == nil || preview.FilesChanged != nil {
			t.Fatalf("缺失 checkpoint 不能返回空成功: %+v, %v", preview, err)
		}
	})
	t.Run("corrupt manifest", func(t *testing.T) {
		home := t.TempDir()
		cwd := t.TempDir()
		rewindWrite(t, cwd, "a.txt", []byte("a"))
		if err := CaptureRewindCheckpoint(home, "corrupt", 2, cwd); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rewindManifest(t, home, "corrupt", 2), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if preview, err := PreviewRewindFiles(home, "corrupt", 2, cwd); err == nil || preview.FilesChanged != nil {
			t.Fatalf("损坏 checkpoint 不能返回空成功: %+v, %v", preview, err)
		}
		if _, err := RestoreRewindFiles(home, "corrupt", 2, cwd); err == nil {
			t.Fatal("损坏 checkpoint 不能恢复成功")
		}
	})
	t.Run("corrupt unchanged blob", func(t *testing.T) {
		home := t.TempDir()
		cwd := t.TempDir()
		rewindWrite(t, cwd, "a.txt", []byte("unchanged"))
		if err := CaptureRewindCheckpoint(home, "blob", 3, cwd); err != nil {
			t.Fatal(err)
		}
		manifestPath := rewindManifest(t, home, "blob", 3)
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var cp rewindCheckpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			t.Fatal(err)
		}
		if len(cp.Files) != 1 {
			t.Fatalf("checkpoint 文件数 = %d", len(cp.Files))
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(manifestPath), filepath.FromSlash(cp.Files[0].Blob)), []byte("bad"), 0o600); err != nil {
			t.Fatal(err)
		}
		if preview, err := PreviewRewindFiles(home, "blob", 3, cwd); err == nil || preview.FilesChanged != nil {
			t.Fatalf("即使工作区无差异，blob 损坏也必须报错: %+v, %v", preview, err)
		}
	})
	t.Run("explicit incomplete", func(t *testing.T) {
		home := t.TempDir()
		cwd := t.TempDir()
		rewindWrite(t, cwd, "too-large.bin", bytes.Repeat([]byte{'x'}, rewindMaxFileBytes+1))
		if err := CaptureRewindCheckpoint(home, "limits", 4, cwd); err == nil {
			t.Fatal("超过 capture 上限必须失败")
		}
		preview, err := PreviewRewindFiles(home, "limits", 4, cwd)
		if err == nil || preview.FilesChanged != nil {
			t.Fatalf("incomplete checkpoint 不能冒充完整: %+v, %v", preview, err)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "incomplete") && !strings.Contains(err.Error(), "不完整") {
			t.Fatalf("错误要显式说明 incomplete: %v", err)
		}
	})
}

func TestRewindSessionAndHomeIsolation(t *testing.T) {
	cwd := t.TempDir()
	homeA := t.TempDir()
	homeB := t.TempDir()
	rewindWrite(t, cwd, "value.txt", []byte("session-a"))
	if err := CaptureRewindCheckpoint(homeA, "same-id", 9, cwd); err != nil {
		t.Fatal(err)
	}
	rewindWrite(t, cwd, "value.txt", []byte("session-b"))
	if err := CaptureRewindCheckpoint(homeA, "other-id", 9, cwd); err != nil {
		t.Fatal(err)
	}
	rewindWrite(t, cwd, "value.txt", []byte("home-b"))
	if err := CaptureRewindCheckpoint(homeB, "same-id", 9, cwd); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		home, id, want string
	}{
		{homeA, "same-id", "session-a"},
		{homeA, "other-id", "session-b"},
		{homeB, "same-id", "home-b"},
	} {
		rewindWrite(t, cwd, "value.txt", []byte("current"))
		if _, err := RestoreRewindFiles(tc.home, tc.id, 9, cwd); err != nil {
			t.Fatalf("%s/%s: %v", tc.home, tc.id, err)
		}
		rewindAssertFile(t, cwd, "value.txt", []byte(tc.want))
	}
	if _, err := PreviewRewindFiles(homeA, "same-id", 10, cwd); err == nil {
		t.Fatal("anchor seq 之间也必须隔离")
	}
}

func TestRewindRejectsInvalidRootsAndManifestEscape(t *testing.T) {
	home := t.TempDir()
	fileRoot := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileRoot, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", filepath.Join(t.TempDir(), "missing"), fileRoot} {
		if err := CaptureRewindCheckpoint(home, "bad-root", 1, root); err == nil {
			t.Fatalf("非法 root %q 必须失败", root)
		}
	}
	if err := CaptureRewindCheckpoint(home, "", 1, t.TempDir()); err == nil {
		t.Fatal("空 session id 必须失败")
	}
	if err := CaptureRewindCheckpoint(home, "bad-seq", -1, t.TempDir()); err == nil {
		t.Fatal("负 anchor seq 必须失败")
	}

	cwd := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	rewindWrite(t, cwd, "inside.txt", []byte("inside"))
	if err := CaptureRewindCheckpoint(home, "escape", 2, cwd); err != nil {
		t.Fatal(err)
	}
	manifestPath := rewindManifest(t, home, "escape", 2)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var cp rewindCheckpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatal(err)
	}
	cp.Files[0].Path = "../escape.txt"
	raw, err = json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewRewindFiles(home, "escape", 2, cwd); err == nil {
		t.Fatal("manifest 路径穿越必须失败")
	}
	if _, err := RestoreRewindFiles(home, "escape", 2, cwd); err == nil {
		t.Fatal("manifest 路径穿越不能恢复成功")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
		t.Fatalf("越界文件被改动: %q, %v", got, err)
	}
}

func TestRewindRejectsSymlinkRootAndNeverFollowsFileSymlink(t *testing.T) {
	home := t.TempDir()
	realRoot := t.TempDir()
	rootLink := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realRoot, rootLink); err != nil {
		t.Skip("当前平台/权限不能创建 symlink:", err)
	}
	if err := CaptureRewindCheckpoint(home, "root-link", 1, rootLink); err == nil {
		t.Fatal("symlink root 必须拒绝")
	}

	cwd := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "target.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	rewindWrite(t, cwd, "link.txt", []byte("inside"))
	if err := CaptureRewindCheckpoint(home, "file-link", 2, cwd); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cwd, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(cwd, "link.txt")); err != nil {
		t.Skip("当前平台/权限不能创建文件 symlink:", err)
	}
	preview, err := PreviewRewindFiles(home, "file-link", 2, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preview.FilesChanged, []string{"link.txt"}) {
		t.Fatalf("symlink 替换 regular file 的 preview = %+v", preview)
	}
	if _, err := RestoreRewindFiles(home, "file-link", 2, cwd); err != nil {
		t.Fatal(err)
	}
	rewindAssertFile(t, cwd, "link.txt", []byte("inside"))
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "outside" {
		t.Fatalf("恢复跟随 symlink 改到了外部: %q, %v", got, err)
	}
}

func TestRewindRestoreFailureRollsBack(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	rewindWrite(t, cwd, "a.txt", []byte("a-before"))
	rewindWrite(t, cwd, "b.txt", []byte("b-before"))
	if err := CaptureRewindCheckpoint(home, "rollback", 1, cwd); err != nil {
		t.Fatal(err)
	}
	rewindWrite(t, cwd, "a.txt", []byte("a-current"))
	rewindWrite(t, cwd, "b.txt", []byte("b-current"))

	calls := 0
	_, err := restoreRewindFiles(home, "rollback", 1, cwd, func(path string) error {
		calls++
		if calls == 2 {
			return fmt.Errorf("injected install failure at %s", path)
		}
		return nil
	})
	if err == nil {
		t.Fatal("注入的恢复失败不能假成功")
	}
	rewindAssertFile(t, cwd, "a.txt", []byte("a-current"))
	rewindAssertFile(t, cwd, "b.txt", []byte("b-current"))
	entries, readErr := os.ReadDir(cwd)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"a.txt", "b.txt"}) {
		t.Fatalf("回滚后留下事务残留: %v", names)
	}

	if _, err := RestoreRewindFiles(home, "rollback", 1, cwd); err != nil {
		t.Fatalf("回滚后 checkpoint 仍应可用: %v", err)
	}
	rewindAssertFile(t, cwd, "a.txt", []byte("a-before"))
	rewindAssertFile(t, cwd, "b.txt", []byte("b-before"))
}

func TestRewindRestoresFileDirectoryTypeChanges(t *testing.T) {
	t.Run("file becomes directory", func(t *testing.T) {
		home := t.TempDir()
		cwd := t.TempDir()
		rewindWrite(t, cwd, "node", []byte("file-at-anchor"))
		if err := CaptureRewindCheckpoint(home, "types", 1, cwd); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(cwd, "node")); err != nil {
			t.Fatal(err)
		}
		rewindWrite(t, cwd, "node/child.txt", []byte("new-child"))
		preview, err := RestoreRewindFiles(home, "types", 1, cwd)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(preview.FilesChanged, []string{"node", "node/child.txt"}) {
			t.Fatalf("类型互换 preview = %+v", preview)
		}
		rewindAssertFile(t, cwd, "node", []byte("file-at-anchor"))
	})

	t.Run("directory becomes file", func(t *testing.T) {
		home := t.TempDir()
		cwd := t.TempDir()
		rewindWrite(t, cwd, "node/child.txt", []byte("child-at-anchor"))
		if err := CaptureRewindCheckpoint(home, "types", 2, cwd); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(cwd, "node")); err != nil {
			t.Fatal(err)
		}
		rewindWrite(t, cwd, "node", []byte("current-file"))
		if _, err := RestoreRewindFiles(home, "types", 2, cwd); err != nil {
			t.Fatal(err)
		}
		rewindAssertFile(t, cwd, "node/child.txt", []byte("child-at-anchor"))
	})
}

func TestRewindHomeInsideCwdSkipsOnlyInternalState(t *testing.T) {
	cwd := t.TempDir()
	home := filepath.Join(cwd, "private-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	rewindWrite(t, cwd, ".dsh-rewind-user", []byte("ordinary-user-file"))
	rewindWrite(t, cwd, "normal.txt", []byte("normal-before"))
	rewindWrite(t, home, "sessions/internal.txt", []byte("internal-before"))
	if err := CaptureRewindCheckpoint(home, "nested-home", 1, cwd); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(rewindManifest(t, home, "nested-home", 1))
	if err != nil {
		t.Fatal(err)
	}
	var cp rewindCheckpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatal(err)
	}
	var captured []string
	for _, file := range cp.Files {
		captured = append(captured, file.Path)
	}
	if !reflect.DeepEqual(captured, []string{".dsh-rewind-user", "normal.txt"}) {
		t.Fatalf("home 内状态被递归捕获，或普通文件被误跳过: %v", captured)
	}

	rewindWrite(t, cwd, ".dsh-rewind-user", []byte("changed"))
	rewindWrite(t, cwd, "normal.txt", []byte("normal-after"))
	rewindWrite(t, home, "sessions/internal.txt", []byte("internal-after"))
	if _, err := RestoreRewindFiles(home, "nested-home", 1, cwd); err != nil {
		t.Fatal(err)
	}
	rewindAssertFile(t, cwd, ".dsh-rewind-user", []byte("ordinary-user-file"))
	rewindAssertFile(t, cwd, "normal.txt", []byte("normal-before"))
	rewindAssertFile(t, home, "sessions/internal.txt", []byte("internal-after"))
}
