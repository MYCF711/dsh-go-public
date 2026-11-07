package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 真屏走的是带 ANSI 的同一个 layout：CJK、长行、样式标记都不能把列宽算坏。
func TestRunDumpTranscriptColorWidth(t *testing.T) {
	path := filepath.Join("testdata", "message_frames.json")
	for _, tc := range []struct {
		size string
		w, h int
	}{{"120x36", 120, 36}, {"80x24", 80, 24}} {
		var out, errb strings.Builder
		code := run([]string{"--dump-transcript", path, "--size", tc.size, "--color", "--expand", "--show-tool-results"}, &out, &errb)
		if code != 0 {
			t.Fatalf("彩色转储 %s 退出码 %d：%s", tc.size, code, errb.String())
		}
		if !strings.Contains(out.String(), "\x1b[") {
			t.Fatalf("--color 没有输出 ANSI：%q", out.String())
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(lines) != tc.h {
			t.Fatalf("彩色 %s 应有 %d 行，实际 %d", tc.size, tc.h, len(lines))
		}
		for i, ln := range lines {
			if n := displayWidth(ln); n > tc.w {
				t.Fatalf("彩色 %s 第 %d 行宽 %d，超过 %d", tc.size, i, n, tc.w)
			}
			if strings.Contains(stripANSI(ln), "\x1b") {
				t.Fatalf("彩色 %s 第 %d 行有半截 ANSI：%q", tc.size, i, ln)
			}
			for j := 0; j < len(ln); j++ {
				if ln[j] >= 1 && ln[j] <= 12 {
					t.Fatalf("内部样式标记漏到彩色输出第 %d 行：%q", i, ln)
				}
			}
		}
	}
}
