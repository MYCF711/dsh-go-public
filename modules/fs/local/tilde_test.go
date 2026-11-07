package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTildeFS(t *testing.T, opts ...Option) (*FS, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	home := filepath.Join(base, "home")
	for _, dir := range []string{root, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opts = append([]Option{WithHome(home)}, opts...)
	fs, err := New(root, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return fs, root, home
}

func slashClean(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

func TestResolveTildeUsesInjectedHome(t *testing.T) {
	fs, _, home := newTildeFS(t)
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "home only", input: "~", want: home},
		{name: "forward slashes", input: "~/notes/today.txt", want: filepath.Join(home, "notes", "today.txt")},
		{name: "backslashes", input: `~\notes\today.txt`, want: filepath.Join(home, "notes", "today.txt")},
		{name: "mixed separators", input: `~/notes\archive/today.txt`, want: filepath.Join(home, "notes", "archive", "today.txt")},
		{name: "repeated separator", input: "~//notes/today.txt", want: filepath.Join(home, "notes", "today.txt")},
		{name: "missing file still resolves", input: "~/missing.txt", want: filepath.Join(home, "missing.txt")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fs.Resolve(tt.input)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.input, err)
			}
			if want := slashClean(tt.want); got != want {
				t.Fatalf("Resolve(%q) = %q, want %q", tt.input, got, want)
			}
		})
	}
}

func TestTildeReadWriteStatAndMissing(t *testing.T) {
	fs, _, home := newTildeFS(t)
	write(t, home, "nested/a.txt", "from injected home")

	data, err := fs.Read(`~\nested\a.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "from injected home" {
		t.Fatalf("Read tilde content = %q", data)
	}
	size, exists, err := fs.Stat("~/nested/a.txt")
	if err != nil || !exists || size != int64(len(data)) {
		t.Fatalf("Stat tilde = size %d, exists %v, err %v", size, exists, err)
	}

	if err := fs.CheckWritable("~/created/new.txt", "modify"); err == nil || !strings.Contains(err.Error(), "不允许写工作区外") {
		t.Fatalf("CheckWritable tilde outside workspace must be rejected, got %v", err)
	}
	if err := fs.Write("~/created/new.txt", []byte("new")); err == nil || !strings.Contains(err.Error(), "不允许写工作区外") {
		t.Fatalf("Write tilde outside workspace must be rejected, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "created", "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected tilde write must not create a file, stat err=%v", err)
	}

	if _, exists, err := fs.Stat("~/missing.txt"); err != nil || exists {
		t.Fatalf("missing tilde path: exists=%v err=%v", exists, err)
	}
	if _, err := fs.Read("~/missing.txt"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing tilde read should retain not-exist error, got %v", err)
	}
}

func TestResolveOnlyLeadingBareTildeExpands(t *testing.T) {
	fs, root, _ := newTildeFS(t)
	tests := []struct {
		input string
		want  string
	}{
		{input: "~other", want: filepath.Join(root, "~other")},
		{input: "~other/file.txt", want: filepath.Join(root, "~other", "file.txt")},
		{input: "~~/file.txt", want: filepath.Join(root, "~~", "file.txt")},
		{input: "dir/~", want: filepath.Join(root, "dir", "~")},
		{input: `dir\~\file.txt`, want: filepath.Join(root, "dir", "~", "file.txt")},
		{input: "dir/file~name", want: filepath.Join(root, "dir", "file~name")},
	}
	for _, tt := range tests {
		got, err := fs.Resolve(tt.input)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tt.input, err)
		}
		if want := slashClean(tt.want); got != want {
			t.Fatalf("Resolve(%q) = %q, want workspace-relative %q", tt.input, got, want)
		}
	}
}

func TestTildeHomeChannelDoesNotRelaxAbsolutePaths(t *testing.T) {
	fs, root, home := newTildeFS(t)
	base := filepath.Dir(root)

	rootAbs := filepath.Join(root, "inside.txt")
	if got, err := fs.Resolve(rootAbs); err != nil || got != slashClean(rootAbs) {
		t.Fatalf("workspace absolute path changed: got %q, err %v", got, err)
	}
	if _, err := fs.Resolve("~/inside.txt"); err != nil {
		t.Fatalf("explicit tilde should use injected home: %v", err)
	}
	for _, p := range []string{
		filepath.Join(home, "inside.txt"),
		filepath.Join(base, "outside.txt"),
		filepath.Join(base, "workspace-evil", "inside.txt"),
	} {
		if _, err := fs.Resolve(p); err == nil {
			t.Fatalf("absolute path %q must keep old root/extra rejection", p)
		}
	}
}

func TestTildeHomeChannelHonoursExtraReadRoot(t *testing.T) {
	fs, root, home := newTildeFS(t)
	base := filepath.Dir(root)
	extra := filepath.Join(base, "extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	fs, err := New(root, WithHome(home), WithExtraReadRoot(extra))
	if err != nil {
		t.Fatal(err)
	}

	extraAbs := filepath.Join(extra, "artifact.txt")
	if got, err := fs.Resolve(extraAbs); err != nil || got != slashClean(extraAbs) {
		t.Fatalf("extra absolute path changed: got %q, err %v", got, err)
	}
	if _, err := fs.Resolve(filepath.Join(home, "inside.txt")); err == nil {
		t.Fatal("adding an unrelated extra root must not expose home as an absolute path")
	}
	if _, err := fs.Resolve("~/inside.txt"); err != nil {
		t.Fatalf("explicit tilde home channel should remain available: %v", err)
	}
	if _, err := fs.Resolve(filepath.Join(base, "extra-evil", "artifact.txt")); err == nil {
		t.Fatal("extra root sibling with a common prefix must remain outside")
	}
}

func TestTildeCannotEscapeInjectedHome(t *testing.T) {
	fs, _, _ := newTildeFS(t)
	for _, p := range []string{
		"~/..",
		"~/../outside.txt",
		`~\..\home-evil\file.txt`,
		"~//../outside.txt",
		"~/../workspace/inside.txt",
	} {
		if got, err := fs.Resolve(p); err == nil {
			t.Fatalf("Resolve(%q) = %q; tilde escape must be rejected", p, got)
		}
	}
}

func TestTildeWithinRootOrExtraStillCannotEscapeHome(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	extra := filepath.Join(base, "extra")
	for _, dir := range []string{root, extra} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("home is workspace root", func(t *testing.T) {
		fs, err := New(root, WithHome(root))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Resolve("~/inside.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Resolve(filepath.Join(root, "inside.txt")); err != nil {
			t.Fatalf("absolute workspace path must remain allowed: %v", err)
		}
		if _, err := fs.Resolve("~/../outside.txt"); err == nil {
			t.Fatal("tilde path must not escape home even when its parent is outside workspace")
		}
	})

	t.Run("home is extra root", func(t *testing.T) {
		fs, err := New(root, WithHome(extra), WithExtraReadRoot(extra))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Resolve("~/inside.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Resolve(filepath.Join(extra, "inside.txt")); err != nil {
			t.Fatalf("absolute extra path must remain allowed: %v", err)
		}
		if _, err := fs.Resolve("~/../workspace/inside.txt"); err == nil {
			t.Fatal("tilde path must stay in home, not switch to another allowed root")
		}
	})
}

func TestWriteRejectsExtraReadRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	extra := filepath.Join(base, "extra")
	for _, dir := range []string{root, extra} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := New(root, WithExtraReadRoot(extra))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(extra, "artifact.txt")
	if err := fs.Write(target, []byte("must not write")); err == nil || !strings.Contains(err.Error(), "不允许写工作区外") {
		t.Fatalf("extra read root must reject writes, got %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("rejected extra write must not create a file, stat err=%v", err)
	}
}
