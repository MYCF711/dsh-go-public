package schedule

// rrule.go —— 官方 0.2.2 schedule 时机 kind="rrule" 用的 RFC 5545 复发引擎。
//
// 只做这个宿主真需要的那一块：一条 DTSTART + 一条 RRULE，正向求"下一次发生"。
// 不支持的部分（BYWEEKNO / BYYEARDAY / 未知键）在解析期就报错并点名，
// 不静默降级：宿主会把 error 变成 400，用户要能看到到底是哪一段不支持。
//
// 语义（rrule_test.go 逐条钉住）：
//   - Next 严格晚于 after；DTSTART 本身算第 1 次发生，COUNT 从它开始数。
//   - UNTIL 含边界（发生时刻 <= UNTIL 才允许）；带 Z 按 UTC 比较，
//     不带 Z 按 DTSTART 的时区解释。
//   - 墙钟语义：FREQ=DAILY 的 09:00 在夏令时切换前后都是当地时间 09:00
//     （America/New_York 2026-03-08 那两次相差 23 小时，不是 24 小时）。
//   - 纯函数、确定性：不读 time.Now()、不起 goroutine、不碰文件、无外部依赖。
//   - 有界：按周期迭代最多 rruleMaxIterations 次、最多看 rruleMaxYears 年，
//     超限返回 ok=false。不可能满足的规则必须快速收敛，绝不死循环。
//
// 为什么内部全是"墙钟分量"（rruleWall）而不是 time.Time 运算：
// RFC 5545 的复发是在**本地时间轴**上定义的。先算本地年月日时分秒，
// 最后一步才用 time.Date(..., loc) 物化成绝对时刻，夏令时切换才不会漂一小时。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 搜索上界。两者任一越界就认为"没有下一次"。
//
// 200000 个周期足够覆盖任何真实日程（秒级规则也有两天多窗口）；
// 100 年是日期级规则的天花板（4 个完整的 400 年格里历周期之外没有意义）。
// 上界的意义是让 FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30 这种
// "永远不成立"的规则在毫秒级返回 false，而不是把调用方卡住。
const (
	rruleMaxIterations = 200000
	rruleMaxYears      = 100
	// rruleMaxCount 是宿主愿意枚举的 COUNT 上限；再大就不是日程而是暴力枚举了。
	rruleMaxCount = 100000
)

// rruleFreq 是 FREQ 的取值。零值表示"还没给"，解析结束后必须非零。
type rruleFreq int

const (
	rruleFreqUnset rruleFreq = iota
	rruleFreqSecondly
	rruleFreqMinutely
	rruleFreqHourly
	rruleFreqDaily
	rruleFreqWeekly
	rruleFreqMonthly
	rruleFreqYearly
)

// rruleFreqHint 是 FREQ 的合法取值清单，直接进错误信息。
const rruleFreqHint = "SECONDLY/MINUTELY/HOURLY/DAILY/WEEKLY/MONTHLY/YEARLY"

func (f rruleFreq) String() string {
	switch f {
	case rruleFreqSecondly:
		return "SECONDLY"
	case rruleFreqMinutely:
		return "MINUTELY"
	case rruleFreqHourly:
		return "HOURLY"
	case rruleFreqDaily:
		return "DAILY"
	case rruleFreqWeekly:
		return "WEEKLY"
	case rruleFreqMonthly:
		return "MONTHLY"
	case rruleFreqYearly:
		return "YEARLY"
	}
	return ""
}

// rruleWeekdayNum 是 BYDAY 的一项：可选的序号（2MO / -1FR）+ 星期码。
// n == 0 表示不指定序号（MO 就是"每个周一"）。
type rruleWeekdayNum struct {
	n  int
	wd time.Weekday
}

func (w rruleWeekdayNum) String() string {
	code := rruleWeekdayCode(w.wd)
	if w.n == 0 {
		return code
	}
	return strconv.Itoa(w.n) + code
}

// rruleWeekdayCode 是 RFC 5545 的两位星期码。
func rruleWeekdayCode(wd time.Weekday) string {
	switch wd {
	case time.Monday:
		return "MO"
	case time.Tuesday:
		return "TU"
	case time.Wednesday:
		return "WE"
	case time.Thursday:
		return "TH"
	case time.Friday:
		return "FR"
	case time.Saturday:
		return "SA"
	case time.Sunday:
		return "SU"
	}
	return "MO"
}

// rruleWeekdayFromCode 解析两位星期码（大小写不敏感）。
func rruleWeekdayFromCode(code string) (time.Weekday, bool) {
	switch strings.ToUpper(code) {
	case "MO":
		return time.Monday, true
	case "TU":
		return time.Tuesday, true
	case "WE":
		return time.Wednesday, true
	case "TH":
		return time.Thursday, true
	case "FR":
		return time.Friday, true
	case "SA":
		return time.Saturday, true
	case "SU":
		return time.Sunday, true
	}
	return 0, false
}

// RRuleSpec 是解析后的复发规则。
//
// 值类型、只读：切片在解析后不再被改写，Next 可以并发调用。
type RRuleSpec struct {
	freq     rruleFreq
	interval int
	count    int        // 0 = 不限
	until    *time.Time // 绝对时刻；nil = 不限
	untilUTC bool       // UNTIL 原文带 Z（回写时保留这个种类）

	byDay      []rruleWeekdayNum
	byMonthDay []int
	byMonth    []int
	byHour     []int
	byMinute   []int
	bySecond   []int
	bySetPos   []int

	wkst        time.Weekday
	hasWKST     bool
	hasInterval bool

	start time.Time      // DTSTART 物化后的绝对时刻
	loc   *time.Location // DTSTART 所在时区
	tzid  string         // DTSTART 行里的 TZID（规范化后）；没有就是 ""
	utc   bool           // DTSTART 带 Z
}

// ParseRRule 解析官方 rrule 文本（DTSTART 行 + RRULE 行，两行都必需）。
//
// defaultLoc 是"DTSTART 既没 TZID 也没 Z"时用的时区（浮动的墙钟时间）；
// 传 nil 等于宿主本地时区。
func ParseRRule(text string, defaultLoc *time.Location) (RRuleSpec, error) {
	if defaultLoc == nil {
		defaultLoc = time.Local
	}
	spec := RRuleSpec{interval: 1, wkst: time.Monday}

	var dtLine, ruleLine string
	haveDT, haveRule := false, false
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" {
			// 空行不算"第三行"：从 JSON 里读回来的文本末尾常带换行。
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "DTSTART"):
			rest := line[len("DTSTART"):]
			if rest == "" || (rest[0] != ';' && rest[0] != ':') {
				return RRuleSpec{}, fmt.Errorf(
					"rrule 的 DTSTART 行 %q 不是 DTSTART[;TZID=...]:YYYYMMDDTHHMMSS[Z]", line)
			}
			if haveDT {
				return RRuleSpec{}, fmt.Errorf("rrule 有多行 DTSTART（只允许一行）")
			}
			dtLine, haveDT = line, true
		case strings.HasPrefix(upper, "RRULE:"):
			if haveRule {
				return RRuleSpec{}, fmt.Errorf("rrule 有多行 RRULE（只允许一行）")
			}
			ruleLine, haveRule = line, true
		default:
			return RRuleSpec{}, fmt.Errorf("rrule 有无法识别的行 %q（只支持 DTSTART 和 RRULE 两行）", line)
		}
	}
	if !haveDT {
		return RRuleSpec{}, fmt.Errorf("rrule 缺 DTSTART 行（应为 DTSTART[;TZID=...]:YYYYMMDDTHHMMSS[Z]）")
	}
	if !haveRule {
		return RRuleSpec{}, fmt.Errorf("rrule 缺 RRULE 行（应为 RRULE:FREQ=...）")
	}
	// 两趟：先定 DTSTART（时区/起点），再解析 RRULE——UNTIL 不带 Z 时要按 DTSTART 的时区解释。
	if err := spec.parseDTStart(dtLine, defaultLoc); err != nil {
		return RRuleSpec{}, err
	}
	if err := spec.parseRuleLine(ruleLine); err != nil {
		return RRuleSpec{}, err
	}
	if err := spec.validate(); err != nil {
		return RRuleSpec{}, err
	}
	return spec, nil
}

// parseDTStart 解析 DTSTART 行：DTSTART[;TZID=<zone>]:<YYYYMMDDTHHMMSS>[Z]。
func (r *RRuleSpec) parseDTStart(line string, defaultLoc *time.Location) error {
	rest := line[len("DTSTART"):]
	tzid := ""
	switch {
	case strings.HasPrefix(rest, ";"):
		idx := strings.Index(rest, ":")
		if idx < 0 {
			return fmt.Errorf("rrule 的 DTSTART 行 %q 缺时间值（应为 DTSTART[;TZID=...]:YYYYMMDDTHHMMSS[Z]）", line)
		}
		params, value := rest[1:idx], rest[idx+1:]
		rest = value
		for _, param := range strings.Split(params, ";") {
			kv := strings.SplitN(param, "=", 2)
			if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
				return fmt.Errorf("rrule 的 DTSTART 参数 %q 不是 KEY=VALUE", param)
			}
			key := strings.ToUpper(strings.TrimSpace(kv[0]))
			val := strings.TrimSpace(kv[1])
			switch key {
			case "TZID":
				if tzid != "" {
					return fmt.Errorf("rrule 的 DTSTART 重复给了 TZID")
				}
				if val == "" {
					return fmt.Errorf("rrule 的 DTSTART;TZID 为空")
				}
				tzid = val
			case "VALUE":
				if !strings.EqualFold(val, "DATE-TIME") {
					return fmt.Errorf("rrule 的 DTSTART;VALUE=%s 不支持（只支持 DATE-TIME）", val)
				}
			default:
				return fmt.Errorf("rrule 不支持 DTSTART 参数 %s", key)
			}
		}
	case strings.HasPrefix(rest, ":"):
		rest = rest[1:]
	default:
		return fmt.Errorf("rrule 的 DTSTART 行 %q 不是 DTSTART[;TZID=...]:YYYYMMDDTHHMMSS[Z]", line)
	}

	hasZ := strings.HasSuffix(rest, "Z") || strings.HasSuffix(rest, "z")
	value := rest
	if hasZ {
		value = value[:len(value)-1]
	}
	// 值里的 T 允许小写（iCalendar 的属性值大小写不敏感）；Z 已经在上面剥掉了。
	wall, err := time.Parse("20060102T150405", strings.ToUpper(value))
	if err != nil {
		return fmt.Errorf("rrule 的 DTSTART 值 %q 不是 YYYYMMDDTHHMMSS[Z]", rest)
	}
	if tzid != "" && hasZ {
		return fmt.Errorf("rrule 的 DTSTART 不能同时带 TZID 和 Z（%q）", line)
	}

	loc := defaultLoc
	switch {
	case hasZ:
		loc = time.UTC
		r.utc = true
	case tzid != "":
		canonical, zone, zerr := resolveZone(tzid)
		if zerr != nil {
			return fmt.Errorf("rrule 的 DTSTART;TZID=%q 不可用: %w", tzid, zerr)
		}
		r.tzid, loc = canonical, zone
	}
	r.loc = loc
	// 墙钟分量 → 绝对时刻。time.Date 会按 loc 的夏令时规则换算。
	r.start = time.Date(wall.Year(), wall.Month(), wall.Day(),
		wall.Hour(), wall.Minute(), wall.Second(), 0, loc)
	return nil
}

// parseRuleLine 解析 RRULE 行：RRULE:FREQ=...;INTERVAL=...。
func (r *RRuleSpec) parseRuleLine(line string) error {
	body := line[len("RRULE:"):]
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("rrule 的 RRULE 行为空（至少要 FREQ=...）")
	}
	seen := make(map[string]bool, 12)
	for _, part := range strings.Split(body, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("rrule 的片段 %q 不是 KEY=VALUE", part)
		}
		key := strings.ToUpper(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])
		if key == "" {
			return fmt.Errorf("rrule 的片段 %q 没有键名", part)
		}
		if seen[key] {
			return fmt.Errorf("rrule 的 %s 重复出现", key)
		}
		seen[key] = true
		if val == "" {
			return fmt.Errorf("rrule 的 %s 没有值", key)
		}

		switch key {
		case "FREQ":
			freq, ok := rruleFreqFromName(val)
			if !ok {
				return fmt.Errorf("rrule 的 FREQ=%s 不认识（只能是 %s）", strings.ToUpper(val), rruleFreqHint)
			}
			r.freq = freq

		case "INTERVAL":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return fmt.Errorf("rrule 的 INTERVAL=%q 不是正整数", val)
			}
			r.interval, r.hasInterval = n, true

		case "COUNT":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return fmt.Errorf("rrule 的 COUNT=%q 不是正整数", val)
			}
			if n > rruleMaxCount {
				return fmt.Errorf("rrule 的 COUNT=%d 超出宿主上限（最多 %d 次）", n, rruleMaxCount)
			}
			r.count = n

		case "UNTIL":
			until, utc, err := rruleParseUntil(val, r.loc)
			if err != nil {
				return err
			}
			r.until, r.untilUTC = &until, utc

		case "BYDAY":
			days, err := rruleParseByDay(val)
			if err != nil {
				return err
			}
			r.byDay = days

		case "BYMONTHDAY":
			days, err := rruleParseIntList(val, "BYMONTHDAY", -31, 31)
			if err != nil {
				return err
			}
			if rruleContainsInt(days, 0) {
				return fmt.Errorf("rrule 的 BYMONTHDAY 不能是 0（负数表示从月末倒数，-1 = 当月最后一天）")
			}
			r.byMonthDay = days

		case "BYMONTH":
			months, err := rruleParseIntList(val, "BYMONTH", 1, 12)
			if err != nil {
				return err
			}
			r.byMonth = months

		case "BYHOUR":
			hours, err := rruleParseIntList(val, "BYHOUR", 0, 23)
			if err != nil {
				return err
			}
			r.byHour = hours

		case "BYMINUTE":
			minutes, err := rruleParseIntList(val, "BYMINUTE", 0, 59)
			if err != nil {
				return err
			}
			r.byMinute = minutes

		case "BYSECOND":
			seconds, err := rruleParseIntList(val, "BYSECOND", 0, 59)
			if err != nil {
				return err
			}
			r.bySecond = seconds

		case "BYSETPOS":
			pos, err := rruleParseIntList(val, "BYSETPOS", -366, 366)
			if err != nil {
				return err
			}
			if rruleContainsInt(pos, 0) {
				return fmt.Errorf("rrule 的 BYSETPOS 不能是 0（1 = 周期内第一个，-1 = 最后一个）")
			}
			r.bySetPos = pos

		case "WKST":
			wd, ok := rruleWeekdayFromCode(val)
			if !ok {
				return fmt.Errorf("rrule 的 WKST=%q 不是 MO/TU/WE/TH/FR/SA/SU", val)
			}
			r.wkst, r.hasWKST = wd, true

		default:
			// 点名报错：宿主把它变成 400，用户能看见是哪一部分不支持。
			return fmt.Errorf("rrule 不支持 %s", key)
		}
	}
	if r.freq == rruleFreqUnset {
		return fmt.Errorf("rrule 缺 FREQ（只能是 %s）", rruleFreqHint)
	}
	return nil
}

// validate 检查解析后仍可能自相矛盾的组合。
func (r *RRuleSpec) validate() error {
	if r.count > 0 && r.until != nil {
		return fmt.Errorf("rrule 不能同时有 COUNT 和 UNTIL（RFC 5545 二者互斥）")
	}
	// RFC 5545：BYDAY 的序号形式只能用于 MONTHLY / YEARLY。
	if r.freq != rruleFreqMonthly && r.freq != rruleFreqYearly {
		for _, d := range r.byDay {
			if d.n != 0 {
				return fmt.Errorf("rrule 的 BYDAY=%s 带序号，但序号形式只能用于 MONTHLY/YEARLY（FREQ=%s）",
					d.String(), r.freq)
			}
		}
	}
	return nil
}

// rruleFreqFromName 把 FREQ 的值翻成枚举；不认识（含 NEVER）返回 false。
func rruleFreqFromName(name string) (rruleFreq, bool) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "SECONDLY":
		return rruleFreqSecondly, true
	case "MINUTELY":
		return rruleFreqMinutely, true
	case "HOURLY":
		return rruleFreqHourly, true
	case "DAILY":
		return rruleFreqDaily, true
	case "WEEKLY":
		return rruleFreqWeekly, true
	case "MONTHLY":
		return rruleFreqMonthly, true
	case "YEARLY":
		return rruleFreqYearly, true
	}
	return rruleFreqUnset, false
}

// rruleParseUntil 解析 UNTIL：YYYYMMDDTHHMMSS[Z]。
// 不带 Z 时按 DTSTART 的时区解释（DTO 是一次墙钟时刻），带 Z 就是 UTC 绝对时刻。
func rruleParseUntil(val string, loc *time.Location) (time.Time, bool, error) {
	if loc == nil {
		loc = time.UTC
	}
	text := val
	utc := false
	if strings.HasSuffix(text, "Z") || strings.HasSuffix(text, "z") {
		text, utc = text[:len(text)-1], true
	}
	wall, err := time.Parse("20060102T150405", strings.ToUpper(text))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("rrule 的 UNTIL=%q 不是 YYYYMMDDTHHMMSS[Z]", val)
	}
	zone := loc
	if utc {
		zone = time.UTC
	}
	return time.Date(wall.Year(), wall.Month(), wall.Day(),
		wall.Hour(), wall.Minute(), wall.Second(), 0, zone), utc, nil
}

// rruleParseByDay 解析 BYDAY=MO,2WE,-1FR。
func rruleParseByDay(val string) ([]rruleWeekdayNum, error) {
	out := make([]rruleWeekdayNum, 0, strings.Count(val, ",")+1)
	for _, item := range strings.Split(val, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("rrule 的 BYDAY=%q 含空项", val)
		}
		upper := strings.ToUpper(item)
		if len(upper) < 2 {
			return nil, fmt.Errorf("rrule 的 BYDAY 项 %q 不是 [±N]MO/TU/WE/TH/FR/SA/SU", item)
		}
		code := upper[len(upper)-2:]
		wd, ok := rruleWeekdayFromCode(code)
		if !ok {
			return nil, fmt.Errorf("rrule 的 BYDAY 项 %q 不是 [±N]MO/TU/WE/TH/FR/SA/SU", item)
		}
		n := 0
		prefix := upper[:len(upper)-2]
		if prefix != "" {
			parsed, err := strconv.Atoi(prefix)
			if err != nil {
				return nil, fmt.Errorf("rrule 的 BYDAY 项 %q 的序号不是整数", item)
			}
			if parsed == 0 || parsed > 53 || parsed < -53 {
				return nil, fmt.Errorf("rrule 的 BYDAY 项 %q 的序号越界（1..53 或 -1..-53，且不能是 0）", item)
			}
			n = parsed
		}
		out = append(out, rruleWeekdayNum{n: n, wd: wd})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].wd != out[j].wd {
			return out[i].wd < out[j].wd
		}
		return out[i].n < out[j].n
	})
	return rruleDedupeWeekdays(out), nil
}

func rruleDedupeWeekdays(in []rruleWeekdayNum) []rruleWeekdayNum {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, w := range in[1:] {
		last := out[len(out)-1]
		if last.n == w.n && last.wd == w.wd {
			continue
		}
		out = append(out, w)
	}
	return out
}

// rruleParseIntList 解析逗号分隔的整数列表，逐项校验范围（含负值）。
func rruleParseIntList(val, name string, min, max int) ([]int, error) {
	parts := strings.Split(val, ",")
	out := make([]int, 0, len(parts))
	for _, item := range parts {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("rrule 的 %s=%q 含空项", name, val)
		}
		n, err := strconv.Atoi(item)
		if err != nil {
			return nil, fmt.Errorf("rrule 的 %s 项 %q 不是整数", name, item)
		}
		if n < min || n > max {
			return nil, fmt.Errorf("rrule 的 %s 项 %d 越界（%d..%d）", name, n, min, max)
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return rruleDedupeInts(out), nil
}

func rruleDedupeInts(in []int) []int {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, n := range in[1:] {
		if out[len(out)-1] == n {
			continue
		}
		out = append(out, n)
	}
	return out
}

func rruleContainsInt(set []int, v int) bool {
	for _, n := range set {
		if n == v {
			return true
		}
	}
	return false
}

// Expression 返回规范化后的文本，落盘与回包都用它。
//
// 固定两行：DTSTART 原样保留种类（TZID / Z / 浮动），RRULE 按固定顺序、
// 固定大小写输出，未给出的部分不输出。ParseRRule(Expression()) 得到等价规则。
func (r RRuleSpec) Expression() string {
	var b strings.Builder
	b.WriteString("DTSTART")
	if r.tzid != "" {
		b.WriteString(";TZID=")
		b.WriteString(r.tzid)
	}
	b.WriteString(":")
	loc := r.loc
	if loc == nil {
		loc = time.Local
	}
	b.WriteString(r.start.In(loc).Format("20060102T150405"))
	if r.utc {
		b.WriteString("Z")
	}

	b.WriteString("\nRRULE:FREQ=")
	b.WriteString(r.freq.String())
	if r.hasInterval {
		b.WriteString(";INTERVAL=")
		b.WriteString(strconv.Itoa(r.interval))
	}
	if r.count > 0 {
		b.WriteString(";COUNT=")
		b.WriteString(strconv.Itoa(r.count))
	}
	if r.until != nil {
		b.WriteString(";UNTIL=")
		if r.untilUTC {
			b.WriteString(r.until.UTC().Format("20060102T150405"))
			b.WriteString("Z")
		} else {
			b.WriteString(r.until.In(loc).Format("20060102T150405"))
		}
	}
	if len(r.byDay) > 0 {
		b.WriteString(";BYDAY=")
		for i, d := range r.byDay {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(d.String())
		}
	}
	rruleWriteIntList(&b, "BYMONTHDAY", r.byMonthDay)
	rruleWriteIntList(&b, "BYMONTH", r.byMonth)
	rruleWriteIntList(&b, "BYHOUR", r.byHour)
	rruleWriteIntList(&b, "BYMINUTE", r.byMinute)
	rruleWriteIntList(&b, "BYSECOND", r.bySecond)
	rruleWriteIntList(&b, "BYSETPOS", r.bySetPos)
	if r.hasWKST {
		b.WriteString(";WKST=")
		b.WriteString(rruleWeekdayCode(r.wkst))
	}
	return b.String()
}

func rruleWriteIntList(b *strings.Builder, name string, values []int) {
	if len(values) == 0 {
		return
	}
	b.WriteString(";")
	b.WriteString(name)
	b.WriteString("=")
	for i, v := range values {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strconv.Itoa(v))
	}
}

// Location 返回 DTSTART 所在时区。
func (r RRuleSpec) Location() *time.Location {
	if r.loc == nil {
		return time.Local
	}
	return r.loc
}

// Next 返回严格晚于 after 的下一次发生时刻。
//
// 没有下一次（COUNT 用尽 / UNTIL 已过 / 规则在搜索上界内不再命中）时 ok=false。
// 返回值的时区是 DTSTART 的时区；调用方要 UTC 自己 .UTC()。
func (r RRuleSpec) Next(after time.Time) (time.Time, bool) {
	if r.loc == nil || r.freq == rruleFreqUnset {
		// 零值 RRuleSpec（没 Parse 过）没有下一次，不 panic。
		return time.Time{}, false
	}
	start := r.start
	if r.until != nil && start.After(*r.until) {
		// UNTIL 早于 DTSTART：整个复发集为空。
		return time.Time{}, false
	}
	// 1. 严格晚于 after —— after 本身不算。
	// 2. DTSTART 本身算第 1 次发生：after 早于它时，第一次就是它。
	if after.Before(start) {
		return start, true
	}
	if r.count == 1 {
		return time.Time{}, false
	}

	period := r.periodOf(start)
	remaining := r.count - 1 // count>0 时表示 start 之后还允许几次；count==0 不数
	if r.count == 0 {
		// 不数次数，可以快进到 after 所在周期，避免从 DTSTART 一格一格爬。
		if target := r.periodOf(after); target.After(period) {
			n := r.rawPeriods(period, target)
			n -= n % r.interval
			if n > 0 {
				period = r.addPeriods(period, n)
			}
		}
	}

	// 搜索上界以 after 为基准：从当下往后看 100 年已经远超任何真实日程。
	maxYear := after.In(r.loc).Year() + rruleMaxYears
	for i := 0; i < rruleMaxIterations; i++ {
		if period.Year() > maxYear {
			return time.Time{}, false
		}
		// 周期起点已经晚于 UNTIL：本周期及之后的候选只可能更晚。
		if r.until != nil && period.After(*r.until) {
			return time.Time{}, false
		}
		candidates := r.periodCandidates(period)
		candidates = rruleApplySetPos(candidates, r.bySetPos)
		for _, cand := range candidates {
			t := r.materialize(cand)
			// DTSTART 之前的时刻不属于复发集；DTSTART 本身就是第 1 次，
			// 已经由 remaining = count-1 记过了，所以这里也必须跳过。
			if !t.After(start) {
				continue
			}
			if r.until != nil && t.After(*r.until) {
				return time.Time{}, false
			}
			// 计数必须在"是否晚于 after"之前：COUNT 数的是整条复发集，
			// 不是"after 之后的第几次"。
			if r.count > 0 {
				if remaining <= 0 {
					return time.Time{}, false
				}
				remaining--
			}
			if !t.After(after) {
				continue
			}
			return t, true
		}
		period = r.addPeriods(period, r.interval)
	}
	return time.Time{}, false
}

// ---- 内部：墙钟分量 ----

// rruleWall 是本地时间轴上的一个点（还没落到时区）。
type rruleWall struct {
	y  int
	mo time.Month
	d  int
	h  int
	mi int
	s  int
}

func (a rruleWall) less(b rruleWall) bool {
	if a.y != b.y {
		return a.y < b.y
	}
	if a.mo != b.mo {
		return a.mo < b.mo
	}
	if a.d != b.d {
		return a.d < b.d
	}
	if a.h != b.h {
		return a.h < b.h
	}
	if a.mi != b.mi {
		return a.mi < b.mi
	}
	return a.s < b.s
}

func (a rruleWall) equal(b rruleWall) bool { return a == b }

// materialize 把墙钟分量落到 DTSTART 的时区。
//
// 夏令时"跳过"的那一小时（如 America/New_York 的 02:30）由 time.Date 规范化，
// 不会 panic；这种时刻本来就不是合法的本地时间。
func (r RRuleSpec) materialize(w rruleWall) time.Time {
	return time.Date(w.y, w.mo, w.d, w.h, w.mi, w.s, 0, r.loc)
}

// periodOf 返回 t 所在周期的起点（按 FREQ 对齐）。
func (r RRuleSpec) periodOf(t time.Time) time.Time {
	t = t.In(r.loc)
	y, mo, d := t.Date()
	h, mi, s := t.Clock()
	switch r.freq {
	case rruleFreqYearly:
		return time.Date(y, time.January, 1, 0, 0, 0, 0, r.loc)
	case rruleFreqMonthly:
		return time.Date(y, mo, 1, 0, 0, 0, 0, r.loc)
	case rruleFreqWeekly:
		back := (int(t.Weekday()) - int(r.wkst) + 7) % 7
		return time.Date(y, mo, d-back, 0, 0, 0, 0, r.loc)
	case rruleFreqDaily:
		return time.Date(y, mo, d, 0, 0, 0, 0, r.loc)
	case rruleFreqHourly:
		return time.Date(y, mo, d, h, 0, 0, 0, r.loc)
	case rruleFreqMinutely:
		return time.Date(y, mo, d, h, mi, 0, 0, r.loc)
	default:
		return time.Date(y, mo, d, h, mi, s, 0, r.loc)
	}
}

// addPeriods 在墙钟轴上把周期起点往后推 n 个周期。
//
// 用 time.Date 的日历算术（不是 time.Add 的绝对时长）：夏令时切换那天
// 墙钟仍然是同一格钟点，这正是 RFC 5545 的本地时间语义。
func (r RRuleSpec) addPeriods(p time.Time, n int) time.Time {
	if n == 0 {
		return p
	}
	y, mo, d := p.Date()
	h, mi, s := p.Clock()
	switch r.freq {
	case rruleFreqYearly:
		return time.Date(y+n, mo, d, h, mi, s, 0, r.loc)
	case rruleFreqMonthly:
		return time.Date(y, mo+time.Month(n), d, h, mi, s, 0, r.loc)
	case rruleFreqWeekly:
		return time.Date(y, mo, d+7*n, h, mi, s, 0, r.loc)
	case rruleFreqDaily:
		return time.Date(y, mo, d+n, h, mi, s, 0, r.loc)
	case rruleFreqHourly:
		return time.Date(y, mo, d, h+n, mi, s, 0, r.loc)
	case rruleFreqMinutely:
		return time.Date(y, mo, d, h, mi+n, s, 0, r.loc)
	default:
		return time.Date(y, mo, d, h, mi, s+n, 0, r.loc)
	}
}

// rawPeriods 数出 from 到 to 之间隔了多少个"未乘 INTERVAL 的"周期。
//
// 允许低估（夏令时切换让绝对时长比墙钟差少一小时），不允许高估：
// 低估只是多走几格，高估会跳过真正该命中的周期。
func (r RRuleSpec) rawPeriods(from, to time.Time) int {
	from, to = from.In(r.loc), to.In(r.loc)
	switch r.freq {
	case rruleFreqYearly:
		return to.Year() - from.Year()
	case rruleFreqMonthly:
		return (to.Year()-from.Year())*12 + int(to.Month()) - int(from.Month())
	case rruleFreqWeekly:
		return int(to.Sub(from).Hours()/24) / 7
	case rruleFreqDaily:
		return int(to.Sub(from).Hours() / 24)
	case rruleFreqHourly:
		return int(to.Sub(from).Hours())
	case rruleFreqMinutely:
		return int(to.Sub(from).Minutes())
	default:
		return int(to.Sub(from).Seconds())
	}
}

// periodCandidates 展开一个周期内的全部候选（已排序、已去重）。
func (r RRuleSpec) periodCandidates(p time.Time) []rruleWall {
	p = p.In(r.loc)
	y, mo, d := p.Date()
	h, mi, s := p.Clock()
	switch r.freq {
	case rruleFreqYearly:
		return r.yearlyCandidates(y)
	case rruleFreqMonthly:
		return r.monthlyCandidates(y, mo)
	case rruleFreqWeekly:
		return r.weeklyCandidates(p)
	case rruleFreqDaily:
		return r.dailyCandidates(y, mo, d)
	case rruleFreqHourly:
		return r.hourlyCandidates(y, mo, d, h)
	case rruleFreqMinutely:
		return r.minutelyCandidates(y, mo, d, h, mi)
	default:
		return r.secondlyCandidates(y, mo, d, h, mi, s)
	}
}

// timesFor 展开某一天内的小时/分钟/秒（BYHOUR/BYMINUTE/BYSECOND 是"扩展"）。
func (r RRuleSpec) timesFor(y int, mo time.Month, d int) []rruleWall {
	s := r.start.In(r.loc)
	hours := r.byHour
	if len(hours) == 0 {
		hours = []int{s.Hour()}
	}
	minutes := r.byMinute
	if len(minutes) == 0 {
		minutes = []int{s.Minute()}
	}
	seconds := r.bySecond
	if len(seconds) == 0 {
		seconds = []int{s.Second()}
	}
	out := make([]rruleWall, 0, len(hours)*len(minutes)*len(seconds))
	for _, h := range hours {
		for _, mi := range minutes {
			for _, sec := range seconds {
				out = append(out, rruleWall{y: y, mo: mo, d: d, h: h, mi: mi, s: sec})
			}
		}
	}
	return out
}

// yearlyCandidates 展开一年。
func (r RRuleSpec) yearlyCandidates(y int) []rruleWall {
	s := r.start.In(r.loc)
	// 带序号的 BYDAY 且没给 BYMONTH：序号相对"年"（RFC 5545 的 20MO = 当年第 20 个周一）。
	if len(r.byMonth) == 0 && len(r.byMonthDay) == 0 && rruleHasOrdinal(r.byDay) {
		var out []rruleWall
		for _, wd := range r.byDay {
			if wd.n == 0 {
				for _, dt := range rruleAllWeekdaysInYear(y, wd.wd) {
					out = append(out, r.timesFor(y, dt.mo, dt.d)...)
				}
				continue
			}
			if dt, ok := rruleNthWeekdayOfYear(y, wd.wd, wd.n); ok {
				out = append(out, r.timesFor(y, dt.mo, dt.d)...)
			}
		}
		return rruleDedupeWalls(out)
	}

	months := r.byMonth
	if len(months) == 0 {
		if len(r.byMonthDay) == 0 && len(r.byDay) == 0 {
			// 没有任何日期扩展：就是 DTSTART 的"周年"。
			months = []int{int(s.Month())}
		} else {
			months = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
		}
	}
	var out []rruleWall
	for _, m := range months {
		mo := time.Month(m)
		for _, d := range r.daysInMonth(y, mo) {
			out = append(out, r.timesFor(y, mo, d)...)
		}
	}
	return rruleDedupeWalls(out)
}

// monthlyCandidates 展开一个月（BYDAY 的序号相对"月"）。
func (r RRuleSpec) monthlyCandidates(y int, mo time.Month) []rruleWall {
	if len(r.byMonth) > 0 && !rruleContainsInt(r.byMonth, int(mo)) {
		return nil
	}
	var out []rruleWall
	for _, d := range r.daysInMonth(y, mo) {
		out = append(out, r.timesFor(y, mo, d)...)
	}
	return rruleDedupeWalls(out)
}

// weeklyCandidates 展开 WKST 起始的一周。
func (r RRuleSpec) weeklyCandidates(p time.Time) []rruleWall {
	s := r.start.In(r.loc)
	set := make(map[time.Weekday]bool, len(r.byDay)+1)
	if len(r.byDay) > 0 {
		for _, wd := range r.byDay {
			set[wd.wd] = true
		}
	} else {
		set[s.Weekday()] = true
	}
	var out []rruleWall
	for i := 0; i < 7; i++ {
		day := p.AddDate(0, 0, i)
		y, mo, d := day.Date()
		if !set[day.Weekday()] {
			continue
		}
		if len(r.byMonth) > 0 && !rruleContainsInt(r.byMonth, int(mo)) {
			continue
		}
		if len(r.byMonthDay) > 0 && !rruleMonthDayMatches(y, mo, d, r.byMonthDay) {
			continue
		}
		out = append(out, r.timesFor(y, mo, d)...)
	}
	return out
}

// dailyCandidates 展开一天（BYDAY/BYMONTH/BYMONTHDAY 在 DAILY 下是"限制"）。
func (r RRuleSpec) dailyCandidates(y int, mo time.Month, d int) []rruleWall {
	if !r.dateAllowed(y, mo, d) {
		return nil
	}
	return r.timesFor(y, mo, d)
}

// hourlyCandidates 展开一小时。
func (r RRuleSpec) hourlyCandidates(y int, mo time.Month, d, h int) []rruleWall {
	if !r.dateAllowed(y, mo, d) {
		return nil
	}
	if len(r.byHour) > 0 && !rruleContainsInt(r.byHour, h) {
		return nil
	}
	s := r.start.In(r.loc)
	minutes := r.byMinute
	if len(minutes) == 0 {
		minutes = []int{s.Minute()}
	}
	seconds := r.bySecond
	if len(seconds) == 0 {
		seconds = []int{s.Second()}
	}
	out := make([]rruleWall, 0, len(minutes)*len(seconds))
	for _, mi := range minutes {
		for _, sec := range seconds {
			out = append(out, rruleWall{y: y, mo: mo, d: d, h: h, mi: mi, s: sec})
		}
	}
	return out
}

// minutelyCandidates 展开一分钟。
func (r RRuleSpec) minutelyCandidates(y int, mo time.Month, d, h, mi int) []rruleWall {
	if !r.dateAllowed(y, mo, d) {
		return nil
	}
	if len(r.byHour) > 0 && !rruleContainsInt(r.byHour, h) {
		return nil
	}
	if len(r.byMinute) > 0 && !rruleContainsInt(r.byMinute, mi) {
		return nil
	}
	seconds := r.bySecond
	if len(seconds) == 0 {
		seconds = []int{r.start.In(r.loc).Second()}
	}
	out := make([]rruleWall, 0, len(seconds))
	for _, sec := range seconds {
		out = append(out, rruleWall{y: y, mo: mo, d: d, h: h, mi: mi, s: sec})
	}
	return out
}

// secondlyCandidates：一秒就是一个周期，BY* 全是限制。
func (r RRuleSpec) secondlyCandidates(y int, mo time.Month, d, h, mi, s int) []rruleWall {
	if !r.dateAllowed(y, mo, d) {
		return nil
	}
	if len(r.byHour) > 0 && !rruleContainsInt(r.byHour, h) {
		return nil
	}
	if len(r.byMinute) > 0 && !rruleContainsInt(r.byMinute, mi) {
		return nil
	}
	if len(r.bySecond) > 0 && !rruleContainsInt(r.bySecond, s) {
		return nil
	}
	return []rruleWall{{y: y, mo: mo, d: d, h: h, mi: mi, s: s}}
}

// dateAllowed 是日期级的限制条件（BYMONTH / BYMONTHDAY / BYDAY）。
func (r RRuleSpec) dateAllowed(y int, mo time.Month, d int) bool {
	if len(r.byMonth) > 0 && !rruleContainsInt(r.byMonth, int(mo)) {
		return false
	}
	if len(r.byMonthDay) > 0 && !rruleMonthDayMatches(y, mo, d, r.byMonthDay) {
		return false
	}
	if len(r.byDay) > 0 && !rruleWeekdayMatches(y, mo, d, r.byDay) {
		return false
	}
	return true
}

// daysInMonth 给出一个月里满足日期规则的"日"（升序、去重）。
func (r RRuleSpec) daysInMonth(y int, mo time.Month) []int {
	dim := rruleDaysInMonth(y, mo)
	if len(r.byMonthDay) > 0 {
		var out []int
		for _, md := range r.byMonthDay {
			d := md
			if md < 0 {
				// -1 = 当月最后一天。
				d = dim + md + 1
			}
			if d < 1 || d > dim {
				// 2 月 30 日这种不存在的日期直接忽略（RFC 5545）。
				continue
			}
			if len(r.byDay) > 0 && !rruleWeekdayMatches(y, mo, d, r.byDay) {
				continue
			}
			out = append(out, d)
		}
		sort.Ints(out)
		return rruleDedupeInts(out)
	}
	if len(r.byDay) > 0 {
		return rruleDaysByWeekday(y, mo, dim, r.byDay)
	}
	// 没有任何日期扩展：用 DTSTART 的日；该月没有这一天就跳过。
	if d := r.start.In(r.loc).Day(); d <= dim {
		return []int{d}
	}
	return nil
}

// rruleDaysByWeekday 给出一个月里 BYDAY 命中的日（支持 2MO / -1FR）。
func rruleDaysByWeekday(y int, mo time.Month, dim int, byDay []rruleWeekdayNum) []int {
	var out []int
	for _, wd := range byDay {
		if wd.n == 0 {
			for d := 1; d <= dim; d++ {
				if rruleWeekdayOf(y, mo, d) == wd.wd {
					out = append(out, d)
				}
			}
			continue
		}
		if wd.n > 0 {
			first := rruleFirstWeekdayOnOrAfter(y, mo, 1, wd.wd)
			if d := first + (wd.n-1)*7; d <= dim {
				out = append(out, d)
			}
			continue
		}
		last := rruleLastWeekdayOnOrBefore(y, mo, dim, wd.wd)
		if d := last + (wd.n+1)*7; d >= 1 {
			out = append(out, d)
		}
	}
	sort.Ints(out)
	return rruleDedupeInts(out)
}

// ---- 日历小工具（全部在 UTC 上算，只看年月日） ----

func rruleDaysInMonth(y int, mo time.Month) int {
	return time.Date(y, mo+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func rruleDaysInYear(y int) int {
	return time.Date(y, time.December, 31, 0, 0, 0, 0, time.UTC).YearDay()
}

func rruleWeekdayOf(y int, mo time.Month, d int) time.Weekday {
	return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Weekday()
}

func rruleWeekdayOfDoy(y, doy int) time.Weekday {
	return time.Date(y, time.January, doy, 0, 0, 0, 0, time.UTC).Weekday()
}

func rruleFirstWeekdayOnOrAfter(y int, mo time.Month, from int, wd time.Weekday) int {
	return from + (int(wd)-int(rruleWeekdayOf(y, mo, from))+7)%7
}

func rruleLastWeekdayOnOrBefore(y int, mo time.Month, to int, wd time.Weekday) int {
	return to - (int(rruleWeekdayOf(y, mo, to))-int(wd)+7)%7
}

// rruleDate 只有年月日，给 YEARLY 的"年内序号"路径用。
type rruleDate struct {
	mo time.Month
	d  int
}

// rruleNthWeekdayOfYear 返回当年第 n 个（n<0 时倒数第 |n| 个）星期 wd 的日期。
func rruleNthWeekdayOfYear(y int, wd time.Weekday, n int) (rruleDate, bool) {
	days := rruleDaysInYear(y)
	if n > 0 {
		doy := (int(wd)-int(rruleWeekdayOfDoy(y, 1))+7)%7 + 1 + (n-1)*7
		if doy > days {
			return rruleDate{}, false
		}
		mo, d := rruleDoyToMonthDay(y, doy)
		return rruleDate{mo: mo, d: d}, true
	}
	lastDoy := days - (int(rruleWeekdayOfDoy(y, days))-int(wd)+7)%7
	doy := lastDoy + (n+1)*7
	if doy < 1 {
		return rruleDate{}, false
	}
	mo, d := rruleDoyToMonthDay(y, doy)
	return rruleDate{mo: mo, d: d}, true
}

func rruleDoyToMonthDay(y, doy int) (time.Month, int) {
	t := time.Date(y, time.January, doy, 0, 0, 0, 0, time.UTC)
	return t.Month(), t.Day()
}

// rruleAllWeekdaysInYear 返回一年里所有星期 wd 的日期。
func rruleAllWeekdaysInYear(y int, wd time.Weekday) []rruleDate {
	var out []rruleDate
	for m := time.January; m <= time.December; m++ {
		dim := rruleDaysInMonth(y, m)
		for d := 1; d <= dim; d++ {
			if rruleWeekdayOf(y, m, d) == wd {
				out = append(out, rruleDate{mo: m, d: d})
			}
		}
	}
	return out
}

// ---- 集合工具 ----

func rruleHasOrdinal(days []rruleWeekdayNum) bool {
	for _, d := range days {
		if d.n != 0 {
			return true
		}
	}
	return false
}

// rruleWeekdayMatches 只比星期（忽略序号）：作为"限制"使用时序号没有意义。
func rruleWeekdayMatches(y int, mo time.Month, d int, byDay []rruleWeekdayNum) bool {
	wd := rruleWeekdayOf(y, mo, d)
	for _, item := range byDay {
		if item.wd == wd {
			return true
		}
	}
	return false
}

// rruleMonthDayMatches 判断某日是否命中 BYMONTHDAY（支持负数，-1 = 月末）。
func rruleMonthDayMatches(y int, mo time.Month, d int, byMonthDay []int) bool {
	dim := rruleDaysInMonth(y, mo)
	for _, md := range byMonthDay {
		if md > 0 && md == d {
			return true
		}
		if md < 0 && dim+md+1 == d {
			return true
		}
	}
	return false
}

// rruleApplySetPos 按 BYSETPOS 从周期候选里挑位置（1 基，负数从末尾数）。
func rruleApplySetPos(in []rruleWall, setPos []int) []rruleWall {
	if len(setPos) == 0 || len(in) == 0 {
		return in
	}
	seen := make(map[int]bool, len(setPos))
	out := make([]rruleWall, 0, len(setPos))
	for _, pos := range setPos {
		idx := pos - 1
		if pos < 0 {
			idx = len(in) + pos
		}
		if idx < 0 || idx >= len(in) || seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, in[idx])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

func rruleDedupeWalls(in []rruleWall) []rruleWall {
	if len(in) < 2 {
		return in
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].less(in[j]) })
	out := in[:1]
	for _, w := range in[1:] {
		if out[len(out)-1].equal(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}
