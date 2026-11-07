package main

import "testing"

func TestContextUsedIsNotAccumulated(t *testing.T) {
	// 同一次调用：prompt 4000 + completion 26 = 4026，而不是把历史调用累加。
	if got := contextUsed(4000, 26, 4026, 0); got != 4026 {
		t.Fatalf("used = %d，期望 4026", got)
	}
	// 只有 total 时退回 total。
	if got := contextUsed(0, 0, 500, 0); got != 500 {
		t.Fatalf("used = %d，期望 500", got)
	}
	// 估算更大时取估算（宁可早报警）。
	if got := contextUsed(100, 10, 110, 900); got != 900 {
		t.Fatalf("used = %d，期望 900", got)
	}
	// 估算更小时信实测。
	if got := contextUsed(1000, 10, 1010, 300); got != 1010 {
		t.Fatalf("used = %d，期望 1010", got)
	}
}
