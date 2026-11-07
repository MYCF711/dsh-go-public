package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --home 指到一个文件上时，要在建任何东西之前就说清楚。
//
// 实测踩过的原文：
//
//	mkdir tmp\afile: The system cannot find the path specified.
//
// 用户看不出是"作业目录有问题"还是"权限不够"。
func TestCheckHomeRejectsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := checkHome(f)
	if err == nil {
		t.Fatal("--home 是文件时必须报错")
	}
	if !strings.Contains(err.Error(), "文件") || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("要说清「这是文件、要的是目录」: %v", err)
	}
}

func TestCheckHomeAllowsMissingAndDir(t *testing.T) {
	dir := t.TempDir()
	if err := checkHome(dir); err != nil {
		t.Fatalf("目录应该通过: %v", err)
	}
	// 不存在也没关系——建的时候会说
	if err := checkHome(filepath.Join(dir, "not-yet")); err != nil {
		t.Fatalf("不存在的路径应该在创建阶段处理，这里先放行: %v", err)
	}
}
