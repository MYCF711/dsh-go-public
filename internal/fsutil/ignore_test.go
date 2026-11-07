package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 没有 .dshdignore 时 LoadIgnore 返回 nil（而不是报错）。
func TestLoadIgnoreMissingIsNotAnError(t *testing.T) {
	ig, err := LoadIgnore(t.TempDir())
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if !ig.Empty() {
		t.Fatal("没有文件时应该是空规则")
	}
}

// 空行和注释不算规则；斜杠方向与 ./ 前缀要被归一。
func TestLoadIgnoreParsesRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, IgnoreFile, "\n# 注释不算\nrefs\n./dist/\n\\build\n")
	ig, err := LoadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	if ig.Empty() {
		t.Fatal("应该有规则")
	}
	cases := []struct {
		rel  string
		want bool
	}{
		{"refs", true},
		{"refs/dsc/libs/go.mod", true},
		{"dist/app.exe", true},
		{"build/out.o", true},
		{"internal/kernel/kernel.go", false},
		{"README.md", false},
	}
	for _, c := range cases {
		if got := ig.Match(c.rel); got != c.want {
			t.Errorf("Match(%q) = %v，期望 %v", c.rel, got, c.want)
		}
	}
}

// `**` 写法要能用（path.Match 不认它，但写声明的人会用）。
func TestIgnoreDoubleStar(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, IgnoreFile, "**/vendor\n")
	ig, err := LoadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a/vendor", "a/b/vendor", "a/vendor/x.go"} {
		if !ig.Match(rel) {
			t.Errorf("%q 应该被 **/vendor 命中", rel)
		}
	}
	if ig.Match("internal/kernel/kernel.go") {
		t.Error("不该误伤普通路径")
	}
}

// 遍历时要真的跳过：
func TestWalkRespectsIgnore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, IgnoreFile, "vendored\n")
	writeFile(t, root, "vendored/big_test.go", "func TestVendored() {}")
	writeFile(t, root, "internal/a_test.go", "func TestReal() {}")

	ig, err := LoadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	var visited []string
	var ignored int
	err = WalkFilesWith(root, WalkOptions{Root: root, Ignore: ig, Ignored: &ignored}, func(abs string) error {
		rel, _ := filepath.Rel(root, abs)
		visited = append(visited, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range visited {
		if v == "vendored" || strings.HasPrefix(v, "vendored/") {
			t.Fatalf("被忽略的目录不该被遍历到: %v", visited)
		}
	}
	if len(visited) != 2 { // .dshdignore + internal/a_test.go
		t.Fatalf("应该只看到 2 个文件，实际 %v", visited)
	}
	if ignored != 1 {
		t.Fatalf("应该记下 1 个被忽略的目录，实际 %d", ignored)
	}
}
