// Package schedule 让"登记过的定时任务"真的会到期。
//
// 分工：
//   - `modules/tool/schedule` 只负责**登记**（create/list/update/delete，算出 next_at）
//   - 这个包负责**到点判定**与**推进 next_at**
//
// 为什么不做成常驻 ticker：宿主的两种运行形态（`run` 一次性、`acp` 长连）
// 都没有"宿主主动发起一轮对话"的通道。硬起 goroutine 只会得到一个
// 会自己调模型、但没人能插话的怪东西。
// 所以这里只回答两个问题——"现在有谁到期了"和"把它们推到下一次"——
// 由调用方（cron、shell 循环、以后的 ACP 客户端）决定怎么处理。
//
// 七种时机（官方 schedule 的 timing union）都落在这一个 Item 上：
//
//	every   每 N 秒            → every_seconds + next_at
//	daily   每天 HH:MM         → daily_at + time_zone + next_at
//	weekly  每周某几天 HH:MM   → weekly_at + weekdays + time_zone + next_at
//	after   从现在起 N 秒后一次 → after_seconds + next_at（一次性）
//	at      某个绝对时刻一次    → at + next_at（一次性）
//	cron    五字段 Vixie 表达式 → cron + time_zone + next_at
//	rrule   RFC5545 复发规则    → rrule + next_at
//
// 一次性（after/at）到点后**结束**（status=ended），绝不被日推一天变成"天天响"；
// 周期任务到点后按自己的规则算下一次（cron/rrule 真解析，不是固定加一天）。
package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 任务的三种生命周期（官方 ScheduleCatalogEntry.status 同名词）。
const (
	StatusActive = "active"
	StatusPaused = "paused"
	StatusEnded  = "ended"
)

// Item 与 modules/tool/schedule 里的结构逐字对齐：两边读的是同一份 JSON。
type Item struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Prompt   string `json:"prompt"`
	Kind     string `json:"kind"`
	EverySec int    `json:"every_seconds,omitempty"`
	DailyAt  string `json:"daily_at,omitempty"`
	Weekdays []int  `json:"weekdays,omitempty"`
	WeeklyAt string `json:"weekly_at,omitempty"`
	NextAt   string `json:"next_at"`

	// At / AfterSec / Cron / RRule / TimeZone 是官方 timing 里 7 个分支的其余四个。
	//
	// 为什么要落原样的规则文本（cron/rrule）：推进 next_at 时必须按**原表达式**
	// 重新算，不能只留一个 next_at —— 否则到点之后就没有下一次可算了。
	At       string `json:"at,omitempty"`            // kind=at：绝对时刻（RFC3339，UTC）
	AfterSec int64  `json:"after_seconds,omitempty"` // kind=after：相对秒数（恢复时按它重新定时）
	Cron     string `json:"cron,omitempty"`          // kind=cron：规范化五字段表达式
	RRule    string `json:"rrule,omitempty"`         // kind=rrule：规范化 DTSTART+RRULE 文本
	TimeZone string `json:"time_zone,omitempty"`     // daily/weekly/cron/at 的时区名

	// TargetKind/TargetSession 是"到点把这轮投给谁"。
	// 缺省（空）= session，且 TargetSession 为空时表示"就是持有这份文件的会话"，
	// 这与历史记录（写在 <sessionId>.json 里、目标就是那个会话）完全兼容。
	TargetKind    string `json:"target_kind,omitempty"`    // session | new | new_once
	TargetSession string `json:"target_session,omitempty"` // session/new_once 的目标会话
	TargetCWD     string `json:"target_cwd,omitempty"`     // new/new_once 建会话的 cwd
	TargetPreset  string `json:"target_preset,omitempty"`  // new/new_once 建会话的 agentPreset

	// Status 是官方 Host 生命周期真值；只有 ended 才能被 schedule.clear 清理。
	// 旧记录缺字段表示活跃/未知，不根据 next_at 猜测 ended。
	Status    string `json:"status,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`

	// AllowWrites 决定这条任务到点时**能不能动写类工具**。
	//
	// 默认 false = 只跑只读工具。理由：任务的提示词是模型写的、执行时人不在场，
	// 而授权是在**登记时**给出的——不留一道显式的门，等于让模型安排
	// "两小时后无条件跑 bash"。
	// 老数据没有这个字段 → 零值 false → 按只读处理，这是安全的默认。
	AllowWrites bool `json:"allow_writes,omitempty"`
}

// Due 是一个到期的任务，带上它的归属会话。
type Due struct {
	Session string
	Item    Item
	Next    time.Time
	// Ends 表示这一格触发之后就没有下一次了（after/at 一次性，
	// 或 rrule 的 COUNT/UNTIL 已经用尽）。Advance 据此把记录置为 ended。
	Ends bool
}

// Load 读某个会话的定时任务；没有文件就是空的。
func Load(dir, sessionID string) ([]Item, error) {
	items, _, err := LoadWithRevision(dir, sessionID)
	return items, err
}

// LoadWithRevision 让 list 内容与 revision 来自同一份字节，避免并发写时拼出
// “旧 schedules + 新 revision”，随后用错误的 fence 删除别人刚改的内容。
func LoadWithRevision(dir, sessionID string) ([]Item, string, error) {
	b, err := os.ReadFile(filepath.Join(dir, sessionID+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	var doc struct {
		Schedules []Item `json:"schedules"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, "", fmt.Errorf("定时任务文件坏了（%s）: %w", sessionID+".json", err)
	}
	if err := ValidateScheduleVersion(b); err != nil {
		return nil, "", fmt.Errorf("定时任务文件版本不兼容（%s）: %w", sessionID+".json", err)
	}
	return doc.Schedules, revisionBytes(b), nil
}

// Sessions 列出所有登记过任务的会话 id。
func Sessions(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out, nil
}

// DueAt 找出 now 之前该触发的任务（含 now），按到期时间升序。
//
// **暂停（paused）与结束（ended）一律不算到期**：patch.active 暂停之后，
// 这条任务在 due/advance 两条路径上都必须真的不来（否则就是假暂停）。
func DueAt(items []Item, now time.Time) []Due {
	var out []Due
	for _, it := range items {
		if it.Status == StatusPaused || it.Status == StatusEnded {
			continue
		}
		t, err := time.Parse(time.RFC3339, it.NextAt)
		if err != nil {
			// next_at 坏了不静默跳过：调用方需要知道这一条永远不会触发。
			continue
		}
		if t.After(now) {
			continue
		}
		next, ok := nextAfterOK(it, now)
		if !ok {
			// 没有下一次：这一格就是最后一格，Next 报这一格的计划时刻。
			next = t
		}
		out = append(out, Due{Item: it, Next: next, Ends: !ok})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Item.NextAt < out[j].Item.NextAt })
	return out
}

// nextAfter 算出严格晚于 now 的下一次触发时间。
//
// 拿不到确定的下一格时返回一个兜底时刻（绝不返回零值时间：零值会让任务疯狂触发）。
// 需要区分"到点就结束"的调用方用 nextAfterOK。
func nextAfter(it Item, now time.Time) time.Time {
	if next, ok := nextAfterOK(it, now); ok {
		return next
	}
	return now.Add(24 * time.Hour)
}

// nextAfterOK 是 nextAfter 的真实版本：ok=false 表示这条规则**没有下一次**
// （一次性任务，或 rrule 的 COUNT/UNTIL 已用尽）。
//
// 规则本身坏掉（存下来的 cron/rrule 解析不了）**不算结束**：那是一条损坏的记录，
// 结束它等于悄悄丢掉用户的任务；这里退成"24 小时后再试"并保持 active。
func nextAfterOK(it Item, now time.Time) (time.Time, bool) {
	switch it.Kind {
	case "every":
		if it.EverySec < 60 {
			return now.Add(time.Minute), true
		}
		step := time.Duration(it.EverySec) * time.Second
		// 从登记的 next_at 一格一格往前推，跨过 now。
		t, err := time.Parse(time.RFC3339, it.NextAt)
		if err != nil {
			return now.Add(step), true
		}
		for !t.After(now) {
			t = t.Add(step)
		}
		return t, true
	case "daily":
		hh, mm, ok := parseClock(it.DailyAt)
		if !ok {
			return now.Add(24 * time.Hour), true
		}
		loc := zoneOrLocal(it.TimeZone)
		local := now.In(loc)
		t := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, loc)
		if !t.After(now) {
			t = time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, loc).AddDate(0, 0, 1)
		}
		return t, true
	case "weekly":
		hh, mm, ok := parseClock(it.WeeklyAt)
		if !ok || len(it.Weekdays) == 0 {
			return now.AddDate(0, 0, 7), true
		}
		loc := zoneOrLocal(it.TimeZone)
		set := map[int]bool{}
		for _, d := range it.Weekdays {
			set[((d%7)+7)%7] = true
		}
		local := now.In(loc)
		// 最多看 8 天：今天剩下的时间也算一次机会。
		for i := 0; i <= 8; i++ {
			day := local.AddDate(0, 0, i)
			if !set[int(day.Weekday())] {
				continue
			}
			t := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, loc)
			if t.After(now) {
				return t, true
			}
		}
		return now.AddDate(0, 0, 7), true
	case "after", "at":
		// 一次性：这一格之后没有下一次。
		return time.Time{}, false
	case "cron":
		spec, err := ParseCron(it.Cron)
		if err != nil {
			return now.Add(24 * time.Hour), true
		}
		next, ok := spec.Next(now, zoneOrLocal(it.TimeZone))
		if !ok {
			// 表达式本身没错，只是永远不成立（例如 2 月 31 日）：
			// 这是一条不会触发的规则，如实报告"没有下一次"。
			return time.Time{}, false
		}
		return next, true
	case "rrule":
		spec, err := ParseRRule(it.RRule, zoneOrLocal(it.TimeZone))
		if err != nil {
			return now.Add(24 * time.Hour), true
		}
		next, ok := spec.Next(now)
		if !ok {
			return time.Time{}, false
		}
		return next, true
	}
	return now.Add(24 * time.Hour), true
}

func parseClock(s string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(parts[0])
	mm, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// Advance 把到期的任务推到下一次，写回文件。返回被推进的条数。
//
// 与 Delete/ClearEnded 共用 mutationMu：官方 schedule.edit 与本函数的
// 读改写必须互相可见，否则两个写者会各自基于同一份旧快照互相覆盖。
//
// 先写 .tmp 再 rename，并保留文件里的未知顶层键（与工具模块共用同一份字节）。
func Advance(dir, sessionID string, dues []Due, now time.Time) (int, error) {
	if len(dues) == 0 {
		return 0, nil
	}
	mutationMu.Lock()
	defer mutationMu.Unlock()
	if err := validateMutationSession(sessionID); err != nil {
		return 0, err
	}
	doc, err := readMutationDocument(dir, sessionID)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]Due, len(dues))
	for _, d := range dues {
		byID[d.Item.ID] = d
	}
	changed := false
	n := 0
	items := make([]Item, len(doc.entries))
	for i, raw := range doc.entries {
		if err := json.Unmarshal(raw, &items[i]); err != nil {
			return 0, fmt.Errorf("解析定时任务 %s 第 %d 条失败: %w", doc.path, i, err)
		}
	}
	entries := append([]json.RawMessage(nil), doc.entries...)
	for i := range items {
		d, ok := byID[items[i].ID]
		if !ok {
			continue
		}
		// 暂停/结束的记录绝不推进：即便调用方把一条过期的 due 传进来。
		if items[i].Status == StatusPaused || items[i].Status == StatusEnded {
			continue
		}
		if d.Ends {
			// 一次性（或规则用尽）：这一格之后没有下一次，记录到此结束。
			items[i].Status = StatusEnded
			items[i].EndedAt = items[i].NextAt
		} else {
			items[i].NextAt = d.Next.Format(time.RFC3339)
			items[i].Status = StatusActive
		}
		items[i].UpdatedAt = now.UTC().Format(time.RFC3339)
		raw, err := marshalItemPreservingUnknown(items[i], doc.entries[i])
		if err != nil {
			return 0, err
		}
		entries[i] = raw
		n++
		changed = true
	}
	if !changed {
		return 0, nil
	}
	raw, err := marshalMutationDocument(doc.body, entries)
	if err != nil {
		return 0, err
	}
	if err := writeFileAtomic(doc.path, raw, doc.mode); err != nil {
		return 0, fmt.Errorf("写 %s 失败，旧文件保持不动: %w", doc.path, err)
	}
	return n, nil
}

// itemManagedKeys 是本包负责写回的键。不在表里的键（未来版本或别的读者加的）
// 必须在改写时原样保留：due/advance 与 schedule.edit 读写的必须是同一份字节，
// 顺手把别人的字段删掉就是数据丢失。
var itemManagedKeys = []string{
	"id", "title", "prompt", "kind", "every_seconds", "daily_at", "weekdays",
	"weekly_at", "next_at", "at", "after_seconds", "cron", "rrule", "time_zone",
	"target_kind", "target_session", "target_cwd", "target_preset",
	"status", "ended_at", "created_at", "updated_at", "allow_writes",
}

// marshalItemPreservingUnknown 把一条记录编码回 JSON，并保留旧对象里的未知键。
//
// 当前 Item 没有的键必须从旧对象里删掉，否则换 kind 之后会留下自相矛盾的旧值
// （kind=daily 却还挂着 every_seconds）。
func marshalItemPreservingUnknown(item Item, previous json.RawMessage) (json.RawMessage, error) {
	encoded, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("序列化定时任务失败: %w", err)
	}
	var fresh map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fresh); err != nil {
		return nil, fmt.Errorf("序列化定时任务失败: %w", err)
	}
	merged := map[string]json.RawMessage{}
	if len(previous) > 0 {
		_ = json.Unmarshal(previous, &merged)
	}
	for _, key := range itemManagedKeys {
		if _, ok := fresh[key]; !ok {
			delete(merged, key)
		}
	}
	for k, v := range fresh {
		merged[k] = v
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("序列化定时任务失败: %w", err)
	}
	return raw, nil
}
