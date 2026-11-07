package schedule

// rrule_test.go —— RFC 5545 复发引擎的语义钉子。
//
// 全部用固定时刻（不读 time.Now()），这样"下一次"是确定值而不是"大概"。
// 每条语义至少一个用例；错误分支点名到具体 RRULE 部分。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// rrAt 解析 RFC3339，坏格式直接 fail（测试里不许静默）。
func rrAt(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("测试时刻 %q 不是 RFC3339: %v", s, err)
	}
	return ts
}

// rrParse 解析一条必须成功的规则。
func rrParse(t *testing.T, text string) RRuleSpec {
	t.Helper()
	spec, err := ParseRRule(text, time.UTC)
	if err != nil {
		t.Fatalf("ParseRRule(%q) 意外失败: %v", text, err)
	}
	return spec
}

// rrNext 断言 Next 命中并返回 UTC 时刻。
func rrNext(t *testing.T, spec RRuleSpec, after string) time.Time {
	t.Helper()
	next, ok := spec.Next(rrAt(t, after))
	if !ok {
		t.Fatalf("Next(%s) 返回 ok=false，期望有一次发生（%s）", after, spec.Expression())
	}
	return next.UTC()
}

// rrNoNext 断言 Next 没有下一次。
func rrNoNext(t *testing.T, spec RRuleSpec, after string) {
	t.Helper()
	if next, ok := spec.Next(rrAt(t, after)); ok {
		t.Fatalf("Next(%s) = %s，期望 ok=false（%s）", after, next.UTC().Format(time.RFC3339), spec.Expression())
	}
}

func rrWant(t *testing.T, got time.Time, want string) {
	t.Helper()
	if wantTime := rrAt(t, want); !got.Equal(wantTime) {
		t.Fatalf("下一次 = %s，期望 %s", got.UTC().Format(time.RFC3339), wantTime.UTC().Format(time.RFC3339))
	}
}

// 语义 1：严格晚于 after，且 DAILY;INTERVAL=2 每次跳两天。
func TestRRuleDailyInterval2(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;INTERVAL=2")

	// after 就是 DTSTART 本身：不算，下一次是 11-09。
	rrWant(t, rrNext(t, spec, "2026-11-07T09:00:00Z"), "2026-11-09T09:00:00Z")
	// after 落在两次之间：同样收敛到 11-09（不能退回到 11-08 那种非网格日）。
	rrWant(t, rrNext(t, spec, "2026-11-08T00:00:00Z"), "2026-11-09T09:00:00Z")
	// 再往后：11-11。
	rrWant(t, rrNext(t, spec, "2026-11-09T09:00:00Z"), "2026-11-11T09:00:00Z")
	// 亚秒级 after 也是"严格晚于"。
	rrWant(t, rrNext(t, spec, "2026-11-09T09:00:00.000000001Z"), "2026-11-11T09:00:00Z")
}

// 语义 1 补充：after 早于 DTSTART 时，第一次就是 DTSTART。
func TestRRuleFirstOccurrenceIsDTSTART(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;INTERVAL=2")

	rrWant(t, rrNext(t, spec, "2020-01-01T00:00:00Z"), "2026-11-07T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-07T08:59:59Z"), "2026-11-07T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-07T09:00:00Z"), "2026-11-09T09:00:00Z")
}

// 语义 2：DTSTART 本身算第 1 次发生，COUNT 从它开始数。
func TestRRuleCountStartsAtDTSTART(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;COUNT=3")

	rrWant(t, rrNext(t, spec, "2026-11-06T00:00:00Z"), "2026-11-07T09:00:00Z") // #1
	rrWant(t, rrNext(t, spec, "2026-11-07T09:00:00Z"), "2026-11-08T09:00:00Z") // #2
	rrWant(t, rrNext(t, spec, "2026-11-08T09:00:00Z"), "2026-11-09T09:00:00Z") // #3
	rrNoNext(t, spec, "2026-11-09T09:00:00Z")                                  // 用尽
	rrNoNext(t, spec, "2026-12-31T00:00:00Z")
}

// COUNT 用尽后即使 after 很远也必须说有界地返回 false（不能从 DTSTART 爬 30 万年）。
func TestRRuleCountExhaustedFarAfterIsBounded(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;COUNT=3")
	// 顺带钉住：RRULE 行必须用 ':' 连接，写成 '=' 是错格式。
	if _, err := ParseRRule("DTSTART:20261107T090000Z\nRRULE=FREQ=DAILY;COUNT=3", time.UTC); err == nil {
		t.Fatal("RRULE 行用 = 分隔应报错")
	}

	start := time.Now()
	rrNoNext(t, spec, "3000-01-01T00:00:00Z")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("COUNT 用尽后 Next 花了 %s，超出上界", elapsed)
	}
}

// 语义 3：UNTIL 含边界（发生时刻 == UNTIL 允许），过了就 ok=false。
func TestRRuleUntilInclusive(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;UNTIL=20261110T000000Z")

	// 11-09 09:00 是 UNTIL 之前最后一次；11-10 09:00 已经晚于 UNTIL。
	rrWant(t, rrNext(t, spec, "2026-11-08T09:00:00Z"), "2026-11-09T09:00:00Z")
	rrNoNext(t, spec, "2026-11-09T09:00:00Z")
	rrNoNext(t, spec, "2026-11-10T00:00:00Z")

	// 边界本身：UNTIL 正好等于某次发生时刻时，那一次允许。
	edge := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;UNTIL=20261109T090000Z")
	rrWant(t, rrNext(t, edge, "2026-11-08T09:00:00Z"), "2026-11-09T09:00:00Z")
	rrNoNext(t, edge, "2026-11-09T09:00:00Z")
}

// UNTIL 带 Z 时按 UTC 比较（不是 DTSTART 的墙钟）。
func TestRRuleUntilUTCComparedAsInstant(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载 Asia/Shanghai: %v", err)
	}
	// 当地 09:00 = 01:00Z。UNTIL=20261110T000000Z 早于当地 11-10 09:00，
	// 所以 11-10 那次必须被排除。
	spec, err := ParseRRule("DTSTART;TZID=Asia/Shanghai:20261108T090000\nRRULE:FREQ=DAILY;UNTIL=20261110T000000Z", shanghai)
	if err != nil {
		t.Fatalf("ParseRRule: %v", err)
	}
	rrWant(t, rrNext(t, spec, "2026-11-08T01:00:00Z"), "2026-11-09T01:00:00Z")
	rrNoNext(t, spec, "2026-11-09T01:00:00Z")
}

// 语义 4：WEEKLY;BYDAY=MO,WE,FR;WKST=MO —— 周日之后的第一次是周一。
func TestRRuleWeeklyByDay(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;WKST=MO")

	// 2026-11-07 是周六；周六之后的第一次是周一 11-09。
	rrWant(t, rrNext(t, spec, "2026-11-07T09:00:00Z"), "2026-11-09T09:00:00Z")
	// 2026-11-08 是周日（WKST 那天），之后的第一次仍是周一。
	rrWant(t, rrNext(t, spec, "2026-11-08T00:00:00Z"), "2026-11-09T09:00:00Z")
	// 周一之后是周三，再之后是周五。
	rrWant(t, rrNext(t, spec, "2026-11-09T09:00:00Z"), "2026-11-11T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-11T09:00:00Z"), "2026-11-13T09:00:00Z")
	// 周五之后跨周回到周一。
	rrWant(t, rrNext(t, spec, "2026-11-13T09:00:00Z"), "2026-11-16T09:00:00Z")
}

// WEEKLY 不给 BYDAY 时沿用 DTSTART 的星期。
func TestRRuleWeeklyDefaultsToDTSTARTWeekday(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=WEEKLY")
	rrWant(t, rrNext(t, spec, "2026-11-07T09:00:00Z"), "2026-11-14T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-14T09:00:00Z"), "2026-11-21T09:00:00Z")
}

// MONTHLY;BYMONTHDAY=-1：-1 = 当月最后一天。
func TestRRuleMonthlyNegativeMonthDay(t *testing.T) {
	spec := rrParse(t, "DTSTART:20260101T000000Z\nRRULE:FREQ=MONTHLY;BYMONTHDAY=-1")

	rrWant(t, rrNext(t, spec, "2026-01-15T00:00:00Z"), "2026-01-31T00:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-01-31T00:00:00Z"), "2026-02-28T00:00:00Z") // 2026 非闰年
	rrWant(t, rrNext(t, spec, "2026-02-28T00:00:00Z"), "2026-03-31T00:00:00Z")
}

// MONTHLY;BYDAY=2MO：每月第二个周一。
func TestRRuleMonthlyOrdinalWeekday(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261101T090000Z\nRRULE:FREQ=MONTHLY;BYDAY=2MO")

	rrWant(t, rrNext(t, spec, "2026-11-01T09:00:00Z"), "2026-11-09T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-09T09:00:00Z"), "2026-12-14T09:00:00Z")
}

// MONTHLY;BYDAY=-1FR：每月最后一个周五。
func TestRRuleMonthlyNegativeOrdinalWeekday(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261101T090000Z\nRRULE:FREQ=MONTHLY;BYDAY=-1FR")
	// 2026-11 的最后一个周五是 11-27。
	rrWant(t, rrNext(t, spec, "2026-11-01T09:00:00Z"), "2026-11-27T09:00:00Z")
	// 2026-12 的最后一个周五是 12-25。
	rrWant(t, rrNext(t, spec, "2026-11-27T09:00:00Z"), "2026-12-25T09:00:00Z")
}

// YEARLY;BYMONTH=3;BYMONTHDAY=15：每年 3 月 15 日。
func TestRRuleYearlyByMonthAndMonthDay(t *testing.T) {
	spec := rrParse(t, "DTSTART:20260315T090000Z\nRRULE:FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=15")

	rrWant(t, rrNext(t, spec, "2026-03-15T09:00:00Z"), "2027-03-15T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2027-03-15T09:00:00Z"), "2028-03-15T09:00:00Z")
}

// YEARLY + 序号 BYDAY 且没有 BYMONTH：序号相对"年"（RFC 5545 的 20MO）。
func TestRRuleYearlyOrdinalWeekday(t *testing.T) {
	spec := rrParse(t, "DTSTART:20260101T000000Z\nRRULE:FREQ=YEARLY;BYDAY=20MO")

	rrWant(t, rrNext(t, spec, "2026-01-01T00:00:00Z"), "2026-05-18T00:00:00Z") // 2026 年第 20 个周一
	rrWant(t, rrNext(t, spec, "2026-05-18T00:00:00Z"), "2027-05-17T00:00:00Z")
}

// HOURLY;INTERVAL=6;BYMINUTE=30：每 6 小时的 :30。
func TestRRuleHourlyIntervalByMinute(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T000000Z\nRRULE:FREQ=HOURLY;INTERVAL=6;BYMINUTE=30")

	rrWant(t, rrNext(t, spec, "2026-11-07T00:00:00Z"), "2026-11-07T00:30:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-07T00:30:00Z"), "2026-11-07T06:30:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-07T06:30:00Z"), "2026-11-07T12:30:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-07T12:30:00Z"), "2026-11-07T18:30:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-07T18:30:00Z"), "2026-11-08T00:30:00Z")
}

// MINUTELY / SECONDLY 的基本步进。
func TestRRuleMinutelyAndSecondly(t *testing.T) {
	minutely := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=MINUTELY;INTERVAL=30")
	rrWant(t, rrNext(t, minutely, "2026-11-07T09:00:00Z"), "2026-11-07T09:30:00Z")
	rrWant(t, rrNext(t, minutely, "2026-11-07T09:30:00Z"), "2026-11-07T10:00:00Z")

	secondly := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=SECONDLY;INTERVAL=15")
	rrWant(t, rrNext(t, secondly, "2026-11-07T09:00:00Z"), "2026-11-07T09:00:15Z")
	rrWant(t, rrNext(t, secondly, "2026-11-07T09:00:15Z"), "2026-11-07T09:00:30Z")
}

// 语义 4：TZID 用 IANA 时区，墙钟不变（UTC 都是 01:00）。
func TestRRuleTZIDKeepsWallClock(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载 Asia/Shanghai: %v", err)
	}
	spec, err := ParseRRule("DTSTART;TZID=Asia/Shanghai:20261108T090000\nRRULE:FREQ=DAILY", shanghai)
	if err != nil {
		t.Fatalf("ParseRRule: %v", err)
	}
	if got := spec.Location().String(); got != "Asia/Shanghai" {
		t.Fatalf("Location() = %s，期望 Asia/Shanghai", got)
	}

	first := rrNext(t, spec, "2026-11-08T00:00:00Z")
	second := rrNext(t, spec, "2026-11-08T01:00:00Z")

	if first.UTC().Format("15:04") != "01:00" || second.UTC().Format("15:04") != "01:00" {
		t.Fatalf("UTC 时刻 = %s / %s，期望都是 01:00Z",
			first.UTC().Format(time.RFC3339), second.UTC().Format(time.RFC3339))
	}
	if h := first.In(shanghai).Hour(); h != 9 {
		t.Fatalf("当地时间小时 = %d，期望 9", h)
	}
	if h := second.In(shanghai).Hour(); h != 9 {
		t.Fatalf("当地时间小时 = %d，期望 9", h)
	}
	rrWant(t, first, "2026-11-08T01:00:00Z")
	rrWant(t, second, "2026-11-09T01:00:00Z")
	if gap := second.Sub(first); gap != 24*time.Hour {
		t.Fatalf("连续两次间隔 = %s，期望 24h（上海无夏令时）", gap)
	}
}

// 语义 4 关键用例：America/New_York 2026-03-08 夏令时切换，
// 当地 09:00 仍是 09:00，两个 UTC 时刻相差 23 小时（不是 24）。
func TestRRuleDSTNewYorkWallClock(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("加载 America/New_York: %v", err)
	}
	spec, err := ParseRRule("DTSTART;TZID=America/New_York:20260307T090000\nRRULE:FREQ=DAILY", ny)
	if err != nil {
		t.Fatalf("ParseRRule: %v", err)
	}

	before := rrNext(t, spec, "2026-03-06T00:00:00Z") // 切换前一天
	after := rrNext(t, spec, "2026-03-07T14:00:00Z")  // 切换当天（前一次发生之后）

	// 墙钟：两次都是当地 09:00。
	if h, m := before.In(ny).Hour(), before.In(ny).Minute(); h != 9 || m != 0 {
		t.Fatalf("切换前墙钟 = %02d:%02d，期望 09:00", h, m)
	}
	if h, m := after.In(ny).Hour(), after.In(ny).Minute(); h != 9 || m != 0 {
		t.Fatalf("切换后墙钟 = %02d:%02d，期望 09:00", h, m)
	}
	// 偏移从 -05:00 变 -04:00。
	if _, off := before.In(ny).Zone(); off != -5*3600 {
		t.Fatalf("切换前偏移 = %d 秒，期望 -18000", off)
	}
	if _, off := after.In(ny).Zone(); off != -4*3600 {
		t.Fatalf("切换后偏移 = %d 秒，期望 -14400", off)
	}
	// UTC 绝对时刻相差 23 小时。
	rrWant(t, before, "2026-03-07T14:00:00Z")
	rrWant(t, after, "2026-03-08T13:00:00Z")
	if gap := after.Sub(before); gap != 23*time.Hour {
		t.Fatalf("跨夏令时两次间隔 = %s，期望 23h", gap)
	}
	// 再下一次回到 24 小时。
	third := rrNext(t, spec, "2026-03-08T13:00:00Z")
	if gap := third.Sub(after); gap != 24*time.Hour {
		t.Fatalf("切换后第二次间隔 = %s，期望 24h", gap)
	}
}

// 语义 6：不可能的规则必须在上界内返回 false（不能死循环）。
func TestRRuleImpossibleRuleReturnsFast(t *testing.T) {
	spec := rrParse(t, "DTSTART:20260101T000000Z\nRRULE:FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30")

	start := time.Now()
	if next, ok := spec.Next(rrAt(t, "2027-01-01T00:00:00Z")); ok {
		t.Fatalf("2 月 30 日不该有下一次，却返回 %s", next.UTC().Format(time.RFC3339))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("不可能规则花了 %s，超出上界（要求有界返回）", elapsed)
	}
}

// 上界也覆盖"DTSTART 那天不存在"的月规则（31 日 + 2 月）。
func TestRRuleImpossibleMonthDayIsBounded(t *testing.T) {
	spec := rrParse(t, "DTSTART:20260201T000000Z\nRRULE:FREQ=MONTHLY;BYMONTHDAY=31;BYMONTH=2")
	start := time.Now()
	rrNoNext(t, spec, "2026-02-01T00:00:00Z")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("不可能规则花了 %s", elapsed)
	}
}

// BYSETPOS：取周期内第 N 个（负数从末尾数）。
func TestRRuleBySetPosLastWeekdayOfMonth(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261101T090000Z\nRRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1")

	rrWant(t, rrNext(t, spec, "2026-11-01T09:00:00Z"), "2026-11-30T09:00:00Z") // 11 月最后一个工作日
	rrWant(t, rrNext(t, spec, "2026-11-30T09:00:00Z"), "2026-12-31T09:00:00Z") // 12 月最后一个工作日
}

func TestRRuleBySetPosFirst(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261101T090000Z\nRRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=1")
	rrWant(t, rrNext(t, spec, "2026-11-01T09:00:00Z"), "2026-11-02T09:00:00Z") // 11 月第一个工作日
}

// 语义 5：纯函数 —— 源码里不许读时钟、不许起 goroutine、不许碰文件/随机数。
//
// 用 AST 判而不是字符串搜：注释里写 time.Now 不算违规，真调用才算。
func TestRRuleSourceIsPure(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("拿不到测试源文件路径")
	}
	path := filepath.Join(filepath.Dir(self), "rrule.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 rrule.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("解析 rrule.go: %v", err)
	}
	// 依赖面：只许标准库的时间/字符串/排序/格式化。
	allowed := map[string]bool{
		"fmt": true, "sort": true, "strconv": true, "strings": true, "time": true,
	}
	for _, imp := range file.Imports {
		name := strings.Trim(imp.Path.Value, `"`)
		if !allowed[name] {
			t.Fatalf("rrule.go 引入了 %q：复发引擎必须无副作用", name)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.GoStmt:
			t.Fatalf("rrule.go:%d 起了 goroutine", fset.Position(node.Pos()).Line)
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name == "time" && sel.Sel.Name == "Now" {
				t.Fatalf("rrule.go:%d 读了时钟（time.Now）：Next 必须是 after 的纯函数",
					fset.Position(node.Pos()).Line)
			}
		}
		return true
	})
}

// 同一次调用重复执行结果一致（确定性）。
func TestRRuleDeterministic(t *testing.T) {
	spec := rrParse(t, "DTSTART;TZID=America/New_York:20260307T090000\nRRULE:FREQ=DAILY;COUNT=10")
	after := rrAt(t, "2026-03-07T00:00:00Z")
	first, ok1 := spec.Next(after)
	second, ok2 := spec.Next(after)
	if ok1 != ok2 || !first.Equal(second) || !first.Equal(second.In(first.Location())) {
		t.Fatalf("两次调用结果不一致: %v/%v vs %v/%v", first, ok1, second, ok2)
	}
}

// Expression 规范化 + 幂等：ParseRRule(r.Expression()) 得到等价规则。
func TestRRuleExpressionCanonicalAndIdempotent(t *testing.T) {
	cases := []string{
		"DTSTART;TZID=Asia/Shanghai:20261108T090000\nRRULE:FREQ=DAILY;INTERVAL=2",
		"DTSTART:20261107T090000Z\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;WKST=MO",
		"DTSTART:20261107T090000Z\nRRULE:FREQ=MONTHLY;BYMONTHDAY=-1",
		"DTSTART:20261107T090000Z\nRRULE:FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=15;COUNT=5",
		"DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;UNTIL=20261110T000000Z",
		"DTSTART:20261107T090000Z\nRRULE:FREQ=HOURLY;INTERVAL=6;BYMINUTE=30;BYSECOND=0",
		"DTSTART:20261107T090000Z\nRRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1",
		"DTSTART:20261107T090000\nRRULE:FREQ=MINUTELY;INTERVAL=15",
	}
	for _, text := range cases {
		first := rrParse(t, text)
		expr := first.Expression()
		second, err := ParseRRule(expr, time.UTC)
		if err != nil {
			t.Fatalf("ParseRRule(Expression()=%q): %v", expr, err)
		}
		if got := second.Expression(); got != expr {
			t.Fatalf("Expression 不幂等:\n  第一次 = %q\n  第二次 = %q", expr, got)
		}
		// 行为也要等价：从 DTSTART 起连推三次。
		after := first.start
		for i := 0; i < 3; i++ {
			a, okA := first.Next(after)
			b, okB := second.Next(after)
			if okA != okB || !a.Equal(b) {
				t.Fatalf("%q 第 %d 次不一致: %v/%v vs %v/%v", expr, i+1, a, okA, b, okB)
			}
			if !okA {
				break
			}
			after = a
		}
	}
}

// Expression 的固定形状：两行、DTSTART 在前、RRULE 在后、大写。
func TestRRuleExpressionShape(t *testing.T) {
	spec := rrParse(t, "DTSTART;TZID=Asia/Shanghai:20261108T090000\nRRULE:FREQ=DAILY;INTERVAL=2")
	want := "DTSTART;TZID=Asia/Shanghai:20261108T090000\nRRULE:FREQ=DAILY;INTERVAL=2"
	if got := spec.Expression(); got != want {
		t.Fatalf("Expression() = %q，期望 %q", got, want)
	}
	if lines := strings.Split(spec.Expression(), "\n"); len(lines) != 2 {
		t.Fatalf("Expression 必须正好两行，得到 %d 行", len(lines))
	}
	if !strings.HasPrefix(spec.Expression(), "DTSTART") || !strings.Contains(spec.Expression(), "\nRRULE:") {
		t.Fatalf("Expression 形状错: %q", spec.Expression())
	}

	// 浮动时间（无 TZID 无 Z）原样保留，不偷偷加 Z。
	floating := rrParse(t, "DTSTART:20261107T090000\nRRULE:FREQ=MINUTELY;INTERVAL=15")
	if got, want := floating.Expression(), "DTSTART:20261107T090000\nRRULE:FREQ=MINUTELY;INTERVAL=15"; got != want {
		t.Fatalf("浮动时间 Expression() = %q，期望 %q", got, want)
	}
}

// Location：三种 DTSTART 种类分别对应 UTC / IANA / defaultLoc。
func TestRRuleLocation(t *testing.T) {
	utcSpec := rrParse(t, "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY")
	if got := utcSpec.Location(); got != time.UTC {
		t.Fatalf("Z 形式 Location() = %v，期望 UTC", got)
	}

	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载 Asia/Shanghai: %v", err)
	}
	tzSpec, err := ParseRRule("DTSTART;TZID=Asia/Shanghai:20261107T090000\nRRULE:FREQ=DAILY", shanghai)
	if err != nil {
		t.Fatalf("ParseRRule: %v", err)
	}
	if got := tzSpec.Location().String(); got != "Asia/Shanghai" {
		t.Fatalf("TZID 形式 Location() = %s，期望 Asia/Shanghai", got)
	}

	custom := time.FixedZone("TESTZONE", 3*3600)
	floatSpec, err := ParseRRule("DTSTART:20261107T090000\nRRULE:FREQ=DAILY", custom)
	if err != nil {
		t.Fatalf("ParseRRule: %v", err)
	}
	if got := floatSpec.Location(); got != custom {
		t.Fatalf("浮动形式 Location() = %v，期望 defaultLoc", got)
	}
	// 浮动时间的第一次就是 DTSTART 本身（按 defaultLoc 物化）。
	next := rrNext(t, floatSpec, "2026-11-06T00:00:00Z")
	rrWant(t, next, "2026-11-07T06:00:00Z") // 当地 09:00 +03:00
}

// 错误分支：每一个都必须点名到具体部分/取值。
func TestRRuleParseErrors(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"空文本", "", "缺 DTSTART"},
		{"缺 DTSTART", "RRULE:FREQ=DAILY", "缺 DTSTART"},
		{"缺 RRULE", "DTSTART:20261107T090000Z", "缺 RRULE"},
		{"缺 FREQ", "DTSTART:20261107T090000Z\nRRULE:INTERVAL=2", "缺 FREQ"},
		{"FREQ=NEVER", "DTSTART:20261107T090000Z\nRRULE:FREQ=NEVER", "FREQ=NEVER 不认识"},
		{"FREQ 拼错", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILYY", "FREQ=DAILYY 不认识"},
		{"BYWEEKNO", "DTSTART:20261107T090000Z\nRRULE:FREQ=YEARLY;BYWEEKNO=1", "rrule 不支持 BYWEEKNO"},
		{"BYYEARDAY", "DTSTART:20261107T090000Z\nRRULE:FREQ=YEARLY;BYYEARDAY=1", "rrule 不支持 BYYEARDAY"},
		{"未知键", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;BYFOO=1", "rrule 不支持 BYFOO"},
		{"COUNT 超上限", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;COUNT=100001", "COUNT=100001 超出宿主上限"},
		{"COUNT 非正", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;COUNT=0", "COUNT="},
		{"DTSTART 坏格式", "DTSTART:xxx\nRRULE:FREQ=DAILY", "DTSTART 值"},
		{"DTSTART 缺时间值", "DTSTART;TZID=Asia/Shanghai\nRRULE:FREQ=DAILY", "缺时间值"},
		{"DTSTART 无分隔", "DTSTART20261107T090000Z\nRRULE:FREQ=DAILY", "不是 DTSTART"},
		{"DTSTART 重复", "DTSTART:20261107T090000Z\nDTSTART:20261108T090000Z\nRRULE:FREQ=DAILY", "多行 DTSTART"},
		{"RRULE 重复", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY\nRRULE:FREQ=WEEKLY", "多行 RRULE"},
		{"陌生行", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY\nEXDATE:20261108T090000Z", "无法识别的行"},
		{"COUNT 与 UNTIL 互斥", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;COUNT=3;UNTIL=20261231T000000Z", "不能同时有 COUNT 和 UNTIL"},
		{"DAILY 带序号 BYDAY", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;BYDAY=2MO", "序号形式只能用于 MONTHLY/YEARLY"},
		{"BYDAY 序号 0", "DTSTART:20261107T090000Z\nRRULE:FREQ=MONTHLY;BYDAY=0MO", "序号越界"},
		{"BYDAY 坏星期码", "DTSTART:20261107T090000Z\nRRULE:FREQ=WEEKLY;BYDAY=XX", "不是 [±N]MO"},
		{"BYMONTHDAY=0", "DTSTART:20261107T090000Z\nRRULE:FREQ=MONTHLY;BYMONTHDAY=0", "不能是 0"},
		{"BYMONTH 越界", "DTSTART:20261107T090000Z\nRRULE:FREQ=YEARLY;BYMONTH=13", "越界"},
		{"BYHOUR 越界", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;BYHOUR=24", "越界"},
		{"BYSECOND 越界", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;BYSECOND=60", "越界"},
		{"BYSETPOS=0", "DTSTART:20261107T090000Z\nRRULE:FREQ=MONTHLY;BYDAY=MO;BYSETPOS=0", "不能是 0"},
		{"INTERVAL=0", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;INTERVAL=0", "不是正整数"},
		{"UNTIL 坏格式", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;UNTIL=2026-11-10", "不是 YYYYMMDDTHHMMSS"},
		{"WKST 坏值", "DTSTART:20261107T090000Z\nRRULE:FREQ=WEEKLY;WKST=MONDAY", "不是 MO/TU"},
		{"空值", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;BYDAY=", "没有值"},
		{"重复键", "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;FREQ=WEEKLY", "重复出现"},
		{"TZID 不存在", "DTSTART;TZID=Mars/Olympus:20261107T090000\nRRULE:FREQ=DAILY", "TZID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseRRule(tc.text, time.UTC)
			if err == nil {
				t.Fatalf("ParseRRule(%q) 应该报错，却得到 %q", tc.text, spec.Expression())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// 零值 RRuleSpec 不能 panic（没 Parse 就调 Next 也要安全）。
func TestRRuleZeroValueNext(t *testing.T) {
	var spec RRuleSpec
	if _, ok := spec.Next(time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)); ok {
		t.Fatal("零值 RRuleSpec 不该有下一次")
	}
	if got := spec.Location(); got != time.Local {
		t.Fatalf("零值 Location() = %v，期望 Local", got)
	}
}

// INTERVAL 与周期边界：MONTHLY;INTERVAL=2 跳月。
func TestRRuleMonthlyInterval(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261115T090000Z\nRRULE:FREQ=MONTHLY;INTERVAL=2")
	rrWant(t, rrNext(t, spec, "2026-11-15T09:00:00Z"), "2027-01-15T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2027-01-15T09:00:00Z"), "2027-03-15T09:00:00Z")
}

// 远未来 after 的快进不能跳过命中（DAILY 跨很多年）。
func TestRRuleFastForwardDoesNotSkip(t *testing.T) {
	spec := rrParse(t, "DTSTART:20260101T090000Z\nRRULE:FREQ=DAILY")
	rrWant(t, rrNext(t, spec, "2126-06-30T09:00:00Z"), "2126-07-01T09:00:00Z")

	// 带 INTERVAL 的网格同样不能漂。
	grid := rrParse(t, "DTSTART:20260101T090000Z\nRRULE:FREQ=DAILY;INTERVAL=7")
	rrWant(t, rrNext(t, grid, "2026-01-10T00:00:00Z"), "2026-01-15T09:00:00Z")
	rrWant(t, rrNext(t, grid, "2026-01-15T09:00:00Z"), "2026-01-22T09:00:00Z")
}

// 不存在的日期直接跳过：BYMONTHDAY=31 只在 31 天的月份发生。
func TestRRuleMonthDay31SkipsShortMonths(t *testing.T) {
	spec := rrParse(t, "DTSTART:20260101T000000Z\nRRULE:FREQ=MONTHLY;BYMONTHDAY=31")

	rrWant(t, rrNext(t, spec, "2026-01-01T00:00:00Z"), "2026-01-31T00:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-01-31T00:00:00Z"), "2026-03-31T00:00:00Z") // 2 月没有 31 日
	rrWant(t, rrNext(t, spec, "2026-03-31T00:00:00Z"), "2026-05-31T00:00:00Z") // 4 月也没有
}

// 闰日：FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29 只在闰年发生。
func TestRRuleLeapDayYearly(t *testing.T) {
	spec := rrParse(t, "DTSTART:20240229T000000Z\nRRULE:FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29")

	rrWant(t, rrNext(t, spec, "2024-02-29T00:00:00Z"), "2028-02-29T00:00:00Z")
	rrWant(t, rrNext(t, spec, "2028-02-29T00:00:00Z"), "2032-02-29T00:00:00Z")
}

// BYHOUR 在 DAILY 下是"扩展"：一天多次。
func TestRRuleDailyByHourExpands(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261107T000000Z\nRRULE:FREQ=DAILY;BYHOUR=9,17")

	rrWant(t, rrNext(t, spec, "2026-11-07T00:00:00Z"), "2026-11-07T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-07T09:00:00Z"), "2026-11-07T17:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-07T17:00:00Z"), "2026-11-08T09:00:00Z")
}

// WEEKLY;INTERVAL=2：每两周的周一/周五。
func TestRRuleWeeklyInterval(t *testing.T) {
	spec := rrParse(t, "DTSTART:20261102T090000Z\nRRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR")

	rrWant(t, rrNext(t, spec, "2026-11-02T09:00:00Z"), "2026-11-06T09:00:00Z")
	rrWant(t, rrNext(t, spec, "2026-11-06T09:00:00Z"), "2026-11-16T09:00:00Z") // 跳过中间那一周
	rrWant(t, rrNext(t, spec, "2026-11-16T09:00:00Z"), "2026-11-20T09:00:00Z")
}

// BYDAY=5MO：只有含第 5 个周一的月份才发生。
func TestRRuleMonthlyFifthWeekday(t *testing.T) {
	spec := rrParse(t, "DTSTART:20260101T000000Z\nRRULE:FREQ=MONTHLY;BYDAY=5MO")
	// 2026-03-30 是 3 月第 5 个周一（3 月 2/9/16/23/30）。
	rrWant(t, rrNext(t, spec, "2026-01-01T00:00:00Z"), "2026-03-30T00:00:00Z")
}

// 大小写不敏感、CRLF、空行都要能吃；Expression 仍然输出大写的规范形式。
func TestRRuleAcceptsLowercaseAndCRLF(t *testing.T) {
	for _, text := range []string{
		"dtstart:20261107t090000z\r\nrrule:freq=weekly;byday=mo,we,fr;wkst=mo\r\n",
		"DTSTART:20261107T090000Z\r\n\r\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;WKST=MO\r\n",
		"  DTSTART:20261107T090000Z  \n  RRULE:FREQ=WEEKLY;BYDAY=mo,we,fr;WKST=mo  \n",
	} {
		spec, err := ParseRRule(text, time.UTC)
		if err != nil {
			t.Fatalf("ParseRRule(%q): %v", text, err)
		}
		want := "DTSTART:20261107T090000Z\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;WKST=MO"
		if got := spec.Expression(); got != want {
			t.Fatalf("Expression() = %q，期望 %q", got, want)
		}
		rrWant(t, rrNext(t, spec, "2026-11-07T09:00:00Z"), "2026-11-09T09:00:00Z")
	}

	// DTSTART;VALUE=DATE-TIME 是允许的显式写法（不支持 VALUE=DATE）。
	spec, err := ParseRRule("DTSTART;VALUE=DATE-TIME:20261107T090000Z\nRRULE:FREQ=DAILY", time.UTC)
	if err != nil {
		t.Fatalf("ParseRRule(VALUE=DATE-TIME): %v", err)
	}
	rrWant(t, rrNext(t, spec, "2026-11-07T09:00:00Z"), "2026-11-08T09:00:00Z")
	if _, err := ParseRRule("DTSTART;VALUE=DATE:20261107\nRRULE:FREQ=DAILY", time.UTC); err == nil {
		t.Fatal("DTSTART;VALUE=DATE 应该报错（只支持 DATE-TIME）")
	}
}

// 不变量：连推 200 次，每一次都必须严格晚于上一次（引擎不能原地不动或倒退）。
func TestRRuleStrictlyIncreasing(t *testing.T) {
	rules := []string{
		"DTSTART:20260101T090000Z\nRRULE:FREQ=DAILY;INTERVAL=3",
		"DTSTART:20260101T090000Z\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR",
		"DTSTART:20260101T090000Z\nRRULE:FREQ=MONTHLY;BYDAY=2MO,-1FR",
		"DTSTART:20260101T090000Z\nRRULE:FREQ=YEARLY;BYMONTH=1,7;BYMONTHDAY=1",
		"DTSTART:20260101T090000Z\nRRULE:FREQ=HOURLY;INTERVAL=7;BYMINUTE=0,30",
		"DTSTART:20260101T090000Z\nRRULE:FREQ=MINUTELY;INTERVAL=17",
		"DTSTART:20260101T090000Z\nRRULE:FREQ=SECONDLY;INTERVAL=11",
		"DTSTART;TZID=America/New_York:20260101T090000\nRRULE:FREQ=DAILY;BYHOUR=9,18",
	}
	for _, text := range rules {
		spec := rrParse(t, text)
		cursor := spec.start
		var prev time.Time
		for i := 0; i < 200; i++ {
			next, ok := spec.Next(cursor)
			if !ok {
				break
			}
			if !next.After(cursor) {
				t.Fatalf("%s 第 %d 次 = %s 没有晚于 %s",
					spec.Expression(), i+1, next.Format(time.RFC3339), cursor.Format(time.RFC3339))
			}
			if !prev.IsZero() && !next.After(prev) {
				t.Fatalf("%s 序列非严格递增: %s -> %s",
					spec.Expression(), prev.Format(time.RFC3339), next.Format(time.RFC3339))
			}
			prev, cursor = next, next
		}
	}
}
