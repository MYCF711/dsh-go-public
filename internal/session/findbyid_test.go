package session

import (
	"path/filepath"
	"testing"

	"dshgo/codecs/v4"
)

func TestFindByID(t *testing.T) {
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/a", "sess-x", "standard")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	got, err := FindByID(home, "sess-x")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.ToSlash(got) != filepath.ToSlash(l.Path) {
		t.Fatalf("找到 %s，期望 %s", got, l.Path)
	}

	if _, err := FindByID(home, "nope"); err == nil {
		t.Fatal("找不到时必须报错")
	}
}

func TestFindByIDAmbiguous(t *testing.T) {
	home := t.TempDir()
	for _, cwd := range []string{"/tmp/a", "/tmp/b"} {
		l, err := CreateSession(home, cwd, "dup", "standard")
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
	}
	if _, err := FindByID(home, "dup"); err == nil {
		t.Fatal("同一个 id 在两个项目下都有时必须报错，不能随便挑一个删")
	}
}

var _ = v4.EncodeSegment
