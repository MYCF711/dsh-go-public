package schedule

import (
	"testing"
	"time"
)

func mustParseCron(t *testing.T, expr string) CronSpec {
	t.Helper()
	spec, err := ParseCron(expr)
	if err != nil {
		t.Fatalf("ParseCron(%q): %v", expr, err)
	}
	return spec
}

// 表达式必须被原样规范化后保存：客户端读到的是它自己写的那条规则。
func TestParseCronCanonicalExpression(t *testing.T) {
	cases := map[string]string{
		" 0  9 * * 1-5 ": "0 9 * * 1-5",
		"*/5 * * * *":    "*/5 * * * *",
		"0 0 1 jan *":    "0 0 1 JAN *",
	}
	for in, want := range cases {
		if got := mustParseCron(t, in).Expression(); got != want {
			t.Fatalf("ParseCron(%q).Expression() = %q，want %q", in, got, want)
		}
	}
	// 幂等：规范化后的表达式必须还能解析回同一个表达式。
	spec := mustParseCron(t, " 0  9 * * 1-5 ")
	if again := mustParseCron(t, spec.Expression()); again.Expression() != spec.Expression() {
		t.Fatalf("规范化不幂等: %q -> %q", spec.Expression(), again.Expression())
	}
}

// 每五分钟：下一次必须是下一个 5 的整数倍分钟，且严格晚于 after。
func TestCronNextEveryFiveMinutes(t *testing.T) {
	spec := mustParseCron(t, "*/5 * * * *")
	after := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	got, ok := spec.Next(after, time.UTC)
	if !ok {
		t.Fatal("Next 应当有结果")
	}
	if want := time.Date(2026, 11, 7, 9, 5, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next = %v，want %v", got, want)
	}
	// 09:05 之后是 09:10（不是 09:05 原地打转）。
	if again, _ := spec.Next(got, time.UTC); !again.Equal(time.Date(2026, 11, 7, 9, 10, 0, 0, time.UTC)) {
		t.Fatalf("再下一次 = %v，want 09:10", again)
	}
	// 09:01 之后是 09:05（不被秒数带偏）。
	if v, _ := spec.Next(time.Date(2026, 11, 7, 9, 1, 30, 0, time.UTC), time.UTC); !v.Equal(time.Date(2026, 11, 7, 9, 5, 0, 0, time.UTC)) {
		t.Fatalf("09:01:30 之后 = %v，want 09:05", v)
	}
}

// 工作日 09:00：周六的下一次是周一。
func TestCronNextWeekdays(t *testing.T) {
	spec := mustParseCron(t, "0 9 * * 1-5")
	saturday := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	got, ok := spec.Next(saturday, time.UTC)
	if !ok {
		t.Fatal("Next 应当有结果")
	}
	if want := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("周六之后 = %v，want 周一 %v", got, want)
	}
	if got.Weekday() != time.Monday {
		t.Fatalf("落在 %v，不在工作日里", got.Weekday())
	}
}

// dom 与 dow 都受限时取或（Vixie 的经典行为）。
func TestCronDOMAndDOWAreOred(t *testing.T) {
	spec := mustParseCron(t, "0 0 1 * 0") // 每月 1 号 **或** 每个周日
	saturday := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	got, ok := spec.Next(saturday, time.UTC)
	if !ok {
		t.Fatal("Next 应当有结果")
	}
	// 2026-11-08 是周日，比 12-01 早。
	if want := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next = %v，want %v（dow 命中）", got, want)
	}
	// 只有 dom 受限时，周日不算。
	domOnly := mustParseCron(t, "0 0 1 * *")
	if v, _ := domOnly.Next(saturday, time.UTC); !v.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("只有 dom 受限时 = %v，want 12-01", v)
	}
}

// 永远不成立的组合必须返回 ok=false，且要快（不许扫到天荒地老）。
func TestCronImpossibleRuleReturnsFalseQuickly(t *testing.T) {
	spec := mustParseCron(t, "0 0 31 2 *") // 2 月 31 日
	start := time.Now()
	got, ok := spec.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if ok {
		t.Fatalf("2 月 31 日不该有下一次，得到 %v", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("不可能的规则扫了 %v，太慢", elapsed)
	}
}

// 名字、步长区间、周日=7 都要能解析。
func TestCronNamesAndRanges(t *testing.T) {
	spec := mustParseCron(t, "0 0 1 JAN *")
	got, ok := spec.Next(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if !ok || !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("JAN 规则下一次 = %v (ok=%v)，want 2027-01-01", got, ok)
	}

	// 周一与周日（0 与 7 都是周日）。
	both := mustParseCron(t, "0 12 * * 0,7")
	for _, day := range []time.Time{
		time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),  // 周日
		time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),  // 周一
		time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), // 周日
	} {
		next, ok := both.Next(day, time.UTC)
		if !ok || next.Weekday() != time.Sunday {
			t.Fatalf("%v 之后 = %v (ok=%v)，want 周日", day, next, ok)
		}
	}

	// 区间步长：0-30/10 只命中 0/10/20/30。
	stepped := mustParseCron(t, "0-30/10 8 * * *")
	next, ok := stepped.Next(time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC), time.UTC)
	if !ok || !next.Equal(time.Date(2026, 11, 7, 8, 10, 0, 0, time.UTC)) {
		t.Fatalf("0-30/10 之后 = %v (ok=%v)，want 08:10", next, ok)
	}
}

// 墙钟语义：同一个 cron 在上海时区算出来的是当地时刻。
func TestCronUsesZoneWallClock(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("没有 tzdata: %v", err)
	}
	spec := mustParseCron(t, "30 9 * * *")
	after := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC) // 上海 08:00
	got, ok := spec.Next(after, loc)
	if !ok {
		t.Fatal("Next 应当有结果")
	}
	if want := time.Date(2026, 11, 7, 9, 30, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("Next = %v，want %v", got, want)
	}
	if got.UTC().Hour() != 1 || got.UTC().Minute() != 30 {
		t.Fatalf("UTC = %v，want 01:30Z", got.UTC())
	}
}

// 坏表达式必须报错，并且点名是哪个字段。
func TestParseCronRejects(t *testing.T) {
	cases := []string{
		"*/0 * * * *",
		"61 * * * *",
		"* 24 * * *",
		"* * 32 * *",
		"* * * 13 *",
		"* * * * 8",
		"* * * *",
		"not a cron",
		"@reboot",
		"1,,2 * * * *",
		"5-1 * * * *",
		"* * * * MONDAY",
	}
	for _, expr := range cases {
		if _, err := ParseCron(expr); err == nil {
			t.Fatalf("ParseCron(%q) 应当报错", expr)
		} else if err.Error() == "" {
			t.Fatalf("ParseCron(%q) 的错误没有说明", expr)
		}
	}
}
