package schedule

// 五字段 Vixie cron：`minute hour day-of-month month day-of-week`。
//
// 为什么自己写而不引第三方：宿主要能**确定地**回答"下一次是哪一刻"，
// 并且算不出来的表达式必须在入口 fail-loud（400），不能收下之后永不触发。
// 这里只接受 Vixie 的经典五字段语法，不认识的写法直接报错并说明是哪个字段。
//
// 语义（与 Vixie cron 一致，测试逐条钉住）：
//   - 每个字段：`*`、`*/n`、`a`、`a-b`、`a-b/n`、`a/n`，多个用逗号连接；
//   - 月与星期接受英文三字母缩写（JAN..DEC / SUN..SAT，大小写不敏感）；
//   - 星期 0 与 7 都是周日；
//   - day-of-month 与 day-of-week 都受限时取**或**（任一命中即触发），
//     只有一个受限时只看那一个——这是 Vixie 的经典行为，不是"两个都要满足"。

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSpec 是解析后的五字段表达式。
type CronSpec struct {
	// expr 是规范化后的表达式（字段间单空格），落盘与回包都用它。
	expr string

	minute [60]bool
	hour   [24]bool
	dom    [32]bool // 下标 1..31
	month  [13]bool // 下标 1..12
	dow    [7]bool  // 下标 0..6，0=周日

	domStar bool
	dowStar bool
}

// cronMonthNames / cronDayNames 是 Vixie 允许的缩写（大小写不敏感）。
var cronMonthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var cronDayNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// ParseCron 解析五字段 Vixie 表达式。
//
// 任何不认识的东西都返回错误，并在错误里点名：哪个字段、哪个值、合法范围是多少。
func ParseCron(expr string) (CronSpec, error) {
	var spec CronSpec
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return spec, fmt.Errorf("cron 表达式 %q 有 %d 个字段，Vixie 必须是 5 个（分 时 日 月 周）", expr, len(fields))
	}

	minutes, _, err := parseCronField(fields[0], "minute", 0, 59, nil)
	if err != nil {
		return spec, err
	}
	hours, _, err := parseCronField(fields[1], "hour", 0, 23, nil)
	if err != nil {
		return spec, err
	}
	doms, domStar, err := parseCronField(fields[2], "day-of-month", 1, 31, nil)
	if err != nil {
		return spec, err
	}
	months, _, err := parseCronField(fields[3], "month", 1, 12, cronMonthNames)
	if err != nil {
		return spec, err
	}
	dows, dowStar, err := parseCronField(fields[4], "day-of-week", 0, 7, cronDayNames)
	if err != nil {
		return spec, err
	}

	copy(spec.minute[:], minutes)
	copy(spec.hour[:], hours)
	copy(spec.month[:], months)
	for i := 1; i <= 31; i++ {
		spec.dom[i] = doms[i]
	}
	// 星期 7 归一到 0（Vixie：0 与 7 都是周日）。
	for i := 0; i <= 7; i++ {
		if dows[i] {
			spec.dow[i%7] = true
		}
	}
	spec.domStar = domStar
	spec.dowStar = dowStar
	// 规范化：字段间单空格，字母缩写统一大写。数字与通配符原样保留。
	canon := make([]string, len(fields))
	for i, f := range fields {
		canon[i] = strings.ToUpper(f)
	}
	spec.expr = strings.Join(canon, " ")
	return spec, nil
}

// Expression 返回规范化后的表达式（ParseCron 幂等）。
func (c CronSpec) Expression() string { return c.expr }

// parseCronField 解析一个字段，返回逐值命中表与"是不是 *"。
func parseCronField(text, name string, min, max int, names map[string]int) ([]bool, bool, error) {
	set := make([]bool, max+2)
	if text == "" {
		return nil, false, fmt.Errorf("cron 的 %s 字段是空串", name)
	}
	star := false
	for _, item := range strings.Split(text, ",") {
		if item == "" {
			return nil, false, fmt.Errorf("cron 的 %s 字段 %q 里有空项（逗号两边都要有值）", name, text)
		}
		itemStar, err := parseCronItem(item, name, min, max, names, set)
		if err != nil {
			return nil, false, err
		}
		star = star || itemStar
	}
	// `*` 必须独占字段：`*,5` 在 Vixie 里也算合法但没意义，这里按"至少有一项是 *"处理，
	// 但"是不是 * 字段"只用来判 dom/dow 的或语义，所以要求整串就是 *。
	return set, strings.TrimSpace(text) == "*" && star, nil
}

func parseCronItem(item, name string, min, max int, names map[string]int, set []bool) (bool, error) {
	body := item
	step := 1
	if slash := strings.IndexByte(body, '/'); slash >= 0 {
		stepText := body[slash+1:]
		body = body[:slash]
		n, err := strconv.Atoi(stepText)
		if err != nil || n <= 0 {
			return false, fmt.Errorf("cron 的 %s 字段步长 %q 不是正整数", name, stepText)
		}
		step = n
	}

	if body == "*" {
		for v := min; v <= max; v += step {
			set[v] = true
		}
		return true, nil
	}

	if dash := strings.IndexByte(body, '-'); dash >= 0 {
		lowText, highText := body[:dash], body[dash+1:]
		low, err := parseCronValue(lowText, name, min, max, names)
		if err != nil {
			return false, err
		}
		high, err := parseCronValue(highText, name, min, max, names)
		if err != nil {
			return false, err
		}
		if low > high {
			return false, fmt.Errorf("cron 的 %s 字段区间 %q 反了（%d > %d）", name, item, low, high)
		}
		for v := low; v <= high; v += step {
			set[v] = true
		}
		return false, nil
	}

	value, err := parseCronValue(body, name, min, max, names)
	if err != nil {
		return false, err
	}
	if step > 1 {
		// `a/n`：从 a 到上界，每 n 一个。
		for v := value; v <= max; v += step {
			set[v] = true
		}
		return false, nil
	}
	set[value] = true
	return false, nil
}

func parseCronValue(text, name string, min, max int, names map[string]int) (int, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, fmt.Errorf("cron 的 %s 字段有空的取值", name)
	}
	if names != nil {
		if v, ok := names[strings.ToUpper(trimmed)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("cron 的 %s 字段 %q 不是数字%s", name, trimmed, cronNameHint(names))
	}
	if v < min || v > max {
		return 0, fmt.Errorf("cron 的 %s 字段 %d 越界（合法范围 %d-%d）", name, v, min, max)
	}
	return v, nil
}

func cronNameHint(names map[string]int) string {
	if names == nil {
		return ""
	}
	return "或三字母缩写"
}

// Next 返回严格晚于 after 的下一次触发时刻（按 loc 的墙钟）。
//
// 找不到（例如 `0 0 31 2 *` 这种永远不成立的组合）时 ok=false：
// 调用方必须把它当成"这条规则不会触发"，而不是排一个猜出来的时刻。
func (c CronSpec) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	if c.expr == "" {
		return time.Time{}, false
	}
	// 从 after 之后的第一分钟开始找；秒一律清零（cron 只到分钟）。
	start := after.In(loc)
	startMinute := time.Date(start.Year(), start.Month(), start.Day(), start.Hour(), start.Minute(), 0, 0, loc)
	cursor := startMinute.Add(time.Minute)

	day := time.Date(cursor.Year(), cursor.Month(), cursor.Day(), 0, 0, 0, 0, loc)
	// 5 年足够覆盖"每 4 年一次的 2 月 29 日"这类规则；超过就认为不会发生。
	for i := 0; i < 366*5; i++ {
		if c.month[int(day.Month())] && c.dayMatches(day) {
			if t, ok := c.firstOnDay(day, loc, after); ok {
				return t, true
			}
		}
		day = day.AddDate(0, 0, 1)
		// AddDate 在夏令时切换日可能给出 00:00 之外的时间；归一到当天 0 点。
		day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	}
	return time.Time{}, false
}

// firstOnDay 在这一天里找第一个严格晚于 after 的时刻。
func (c CronSpec) firstOnDay(day time.Time, loc *time.Location, after time.Time) (time.Time, bool) {
	for hour := 0; hour < 24; hour++ {
		if !c.hour[hour] {
			continue
		}
		for minute := 0; minute < 60; minute++ {
			if !c.minute[minute] {
				continue
			}
			candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc)
			if candidate.After(after) {
				return candidate, true
			}
		}
	}
	return time.Time{}, false
}

// dayMatches 实现 Vixie 的日/星期组合语义：两个都受限时取或。
func (c CronSpec) dayMatches(day time.Time) bool {
	domOK := c.dom[day.Day()]
	dowOK := c.dow[int(day.Weekday())]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}
