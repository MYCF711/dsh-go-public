package approval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if err := s.Save("sess-1", []string{"bash", "write"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "bash" || got[1] != "write" {
		t.Fatalf("读回 = %v", got)
	}
}

func TestStoreMissingIsEmpty(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	got, err := s.Load("never-seen")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("没记过应该是空，得到 %v", got)
	}
}

func TestStoreCorruptIsTolerated(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("bad")
	if err == nil {
		t.Fatal("坏文件应该报错而不是静默当成已授权")
	}
	if len(got) != 0 {
		t.Fatalf("坏文件时不能返回任何授权，得到 %v", got)
	}
}

func TestStoreDelete(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if err := s.Save("s1", []string{"bash"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("s1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load("s1"); len(got) != 0 {
		t.Fatalf("删掉之后应该为空，得到 %v", got)
	}
	if err := s.Delete("s1"); err != nil {
		t.Fatalf("重复删除不该报错: %v", err)
	}
}

func TestStoreSanitizesName(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	if err := s.Save("../evil/id with spaces", []string{"bash"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("应该只写了一个文件，实际 %d 个", len(entries))
	}
	name := entries[0].Name()
	if filepath.Base(name) != name {
		t.Fatalf("文件名跑出目录了: %s", name)
	}
	for _, r := range name {
		if r == ' ' || r == '/' || r == '\\' {
			t.Fatalf("文件名里有不安全字符: %q", name)
		}
	}
}

func TestStoreEmptyDirIsNoop(t *testing.T) {
	s := &Store{}
	if err := s.Save("x", []string{"bash"}); err == nil {
		t.Fatal("没配目录时 Save 应该报错")
	}
	if got, err := s.Load("x"); err != nil || len(got) != 0 {
		t.Fatalf("没配目录时 Load 应该是空且无错：%v %v", got, err)
	}
}
