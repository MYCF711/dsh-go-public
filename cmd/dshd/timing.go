package main

import (
	"fmt"
	"os"
	"time"
)

var startedAt = time.Now()

// timingf 在 DSHD_TIMING=1 时打印阶段耗时（诊断冷启动用）。
func timingf(phase string) {
	if os.Getenv("DSHD_TIMING") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[timing] %-24s %6.2f ms\n", phase, float64(time.Since(startedAt).Microseconds())/1000)
}
