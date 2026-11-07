package session

import (
	"path/filepath"
	"testing"

	"dshgo/internal/llm"
)

func TestRawEstimate(t *testing.T) {
	cases := []struct {
		content string
		want    int // 不含每条 4 的结构开销
	}{
		{"", 0},
		{"abcd", 1},
		{"abcdefgh", 2},
		{"汉字", 2},
		{"a汉", 1},
	}
	for _, c := range cases {
		s := []SurfaceMsg{{Message: llm.Message{Role: "user", Content: c.content}}}
		got := rawEstimate(s)
		if got != c.want+4 {
			t.Errorf("%q 估算 = %d，期望 %d（内容 %d + 结构 4）", c.content, got, c.want+4, c.want)
		}
	}
}

func TestMeterUncalibrated(t *testing.T) {
	var m Meter
	s := []SurfaceMsg{{Message: llm.Message{Role: "user", Content: "abcdefgh"}}}
	if got := m.Estimate(s); got != 6 {
		t.Fatalf("未校准时 = %d，期望 6", got)
	}
	if m.Calibrated() {
		t.Fatal("没喂过锚点不该自称已校准")
	}
}

func TestMeterCalibrates(t *testing.T) {
	var m Meter
	s := []SurfaceMsg{{Message: llm.Message{Role: "user", Content: "abcdefgh"}}} // raw = 6
	m.Observe(120, 6)                                                            // 服务端说 120，我们估 6 → 刻度 20
	if !m.Calibrated() {
		t.Fatal("应该已校准")
	}
	if got := m.Estimate(s); got != 120 {
		t.Fatalf("校准后 = %d，期望 120", got)
	}
	// 表面翻倍，估算也应翻倍。
	s = append(s, s...)
	if got := m.Estimate(s); got != 240 {
		t.Fatalf("翻倍后 = %d，期望 240", got)
	}
}

func TestMeterIgnoresBadAnchor(t *testing.T) {
	var m Meter
	m.Observe(0, 0)
	if m.Calibrated() {
		t.Fatal("0 锚点必须被忽略")
	}
	m.Observe(100, 0)
	if m.Calibrated() {
		t.Fatal("分母为 0 的锚点必须被忽略")
	}
}

func TestHistoryGuardRejectsDSHHome(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "sessions", "--x--", "s1", "session.v4.jsonl.zstd")

	ok, reason := HistoryGuard(target, home, false)
	if ok {
		t.Fatal("DSH_HOME 下面的会话必须拒绝")
	}
	if reason == "" {
		t.Fatal("拒绝时要说清原因")
	}
	if ok, _ := HistoryGuard(target, home, true); !ok {
		t.Fatal("--force 之后应该放行")
	}
}

func TestHistoryGuardRejectsSessionsDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sessions", "s1.v4.jsonl.zstd")
	if ok, _ := HistoryGuard(target, "", false); ok {
		t.Fatal("带 sessions 段的路径默认要拒绝")
	}
	if ok, _ := HistoryGuard(target, "", true); !ok {
		t.Fatal("--force 之后应该放行")
	}
}

func TestHistoryGuardAllowsOrdinaryPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "copy.v4.jsonl.zstd")
	if ok, reason := HistoryGuard(target, "/nonexistent/home", false); !ok {
		t.Fatalf("普通路径不该拦：%s", reason)
	}
}

func TestUnderDSHHome(t *testing.T) {
	home := t.TempDir()
	if !UnderDSHHome(filepath.Join(home, "sessions", "x"), home) {
		t.Fatal("home 下面应该判 true")
	}
	if UnderDSHHome(filepath.Join(t.TempDir(), "sessions", "x"), home) {
		t.Fatal("不在 home 下面必须判 false——我们自己 home 里也有 sessions 目录")
	}
	if UnderDSHHome("/any/path", "") {
		t.Fatal("没配 DSH_HOME 时一律 false")
	}
}
