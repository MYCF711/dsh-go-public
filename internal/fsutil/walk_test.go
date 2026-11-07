package fsutil

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestWalkFilesSkipsNoise(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "sub/b.go", "node_modules/x/c.go", ".git/d.go"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	if err := WalkFiles(root, DefaultSkipDirs, func(abs string) error {
		rel, _ := filepath.Rel(root, abs)
		got = append(got, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"a.go", "sub/b.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("遍历结果 = %v，期望 %v", got, want)
	}
}

func TestIsBinary(t *testing.T) {
	if IsBinary([]byte("plain text")) {
		t.Fatal("纯文本不该被判定为二进制")
	}
	if !IsBinary([]byte{'a', 0, 'b'}) {
		t.Fatal("含 NUL 应判定为二进制")
	}
}
