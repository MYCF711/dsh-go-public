package schedule

import (
	"fmt"
	"strings"
	"time"
)

// 时区解析：官方 schedule 的 time_zone 只接受三种写法——
// 省略（用 Host 本地时区）、"UTC"、或 IANA 的 Area/Location。
// 这里不猜：解析不出来就报错，让调用方 fail-loud，绝不"算个差不多的时刻"。
//
// 为什么不用固定偏移字符串（如 "+08:00"）：日/周/cron/rrule 都是墙钟语义，
// 固定偏移在夏令时切换那天会漂一小时；只有真正的位置（Location）才有切换规则。

// localZoneName 是本进程本地时区的名字。
//
// Go 在多数 Windows 机器上直接给 "Local"（没有 tzdata 名字），
// 所以对外报时区时不能假定它是 "Asia/Shanghai" 这种 IANA 名。
func localZoneName() string { return time.Local.String() }

// LocalZoneName 是给宿主用的导出形态：回包里的 timeZone 必须与真实算法一致。
func LocalZoneName() string { return localZoneName() }

// ResolveZone 是给宿主用的导出形态：解析失败必须 fail-loud（400），
// 不许"猜一个差不多的时区"把任务写进用户的工作流。
func ResolveZone(name string) (string, *time.Location, error) { return resolveZone(name) }

// resolveZone 解析一个官方 time_zone 值。
//
//	""、"Local"、宿主本地时区的名字 -> 进程本地时区
//	"UTC"、"Z"、"Etc/UTC"            -> UTC
//	其它                             -> time.LoadLocation（IANA Area/Location）
//
// 返回规范化后的名字（写进存储与回包）与 *time.Location。
func resolveZone(name string) (string, *time.Location, error) {
	trimmed := strings.TrimSpace(name)
	switch trimmed {
	case "", "Local":
		return localZoneName(), time.Local, nil
	case "UTC", "Z", "Etc/UTC", "GMT":
		return "UTC", time.UTC, nil
	}
	if trimmed == localZoneName() {
		return trimmed, time.Local, nil
	}
	loc, err := time.LoadLocation(trimmed)
	if err != nil {
		return "", nil, fmt.Errorf("time_zone=%q 不是可用的 IANA 时区（本宿主接受省略、UTC、或 Area/Location）: %w", name, err)
	}
	return loc.String(), loc, nil
}

// zoneOrLocal 把存储里的时区名解析成 Location；坏了就退回本地，
// 只用于"读旧记录时不要炸"的读路径，写路径必须用 resolveZone 的真错误。
func zoneOrLocal(name string) *time.Location {
	if strings.TrimSpace(name) == "" {
		return time.Local
	}
	_, loc, err := resolveZone(name)
	if err != nil {
		return time.Local
	}
	return loc
}
