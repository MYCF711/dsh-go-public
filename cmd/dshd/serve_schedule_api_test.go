package main

// 官方 0.2.2 schedule.create/edit/view/run/history 在**宿主机**这一侧的真实实现测试。
//
// 全部用真实临时 home、真实 JSON 文件：不 mock 存储。
// 覆盖：create 落盘 → 重开宿主仍可读；A/B home 不串；edit revision 栅栏 409；
// view 真实单条；run 真触发一次提交并记录真实历史；history 分页与游标。

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/schedule"
	"dshgo/internal/transport"
)

// scheduleHome 造一个临时 home（只建目录，不预置文件）。
func scheduleHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "state", "schedules"), 0o755); err != nil {
		t.Fatalf("建 schedules 目录: %v", err)
	}
	return home
}

// scheduleHostFixture 是一个真宿主 + 一个可断言的提交器。
type scheduleHostFixture struct {
	home   string
	host   *officialScheduleHost
	submit *fakeScheduleSubmit
	now    time.Time
}

type fakeScheduleSubmit struct {
	mu    sync.Mutex
	calls []fakeScheduleSubmitCall
	err   error
}

type fakeScheduleSubmitCall struct {
	sessionID string
	text      string
}

func (f *fakeScheduleSubmit) submit(sessionID, text string) (transport.PromptResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return transport.PromptResult{}, f.err
	}
	f.calls = append(f.calls, fakeScheduleSubmitCall{sessionID: sessionID, text: text})
	return transport.PromptResult{Accepted: true, TurnID: "turn-" + itoa(len(f.calls))}, nil
}

func (f *fakeScheduleSubmit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeScheduleSubmit) last() fakeScheduleSubmitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeScheduleSubmitCall{}
	}
	return f.calls[len(f.calls)-1]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// newScheduleHostFixture 造一个真宿主；now 固定，便于断言 next_at。
func newScheduleHostFixture(t *testing.T, home string) *scheduleHostFixture {
	t.Helper()
	submit := &fakeScheduleSubmit{}
	fx := &scheduleHostFixture{home: home, submit: submit, now: time.Date(2026, 11, 7, 9, 0, 0, 0, time.Local)}
	// 时钟读的是 fixture 字段（测试会推进它以验证 updatedAt 真的变了），
	// 不是一个被闭包冻住的局部变量。
	fx.host = newOfficialScheduleHost(officialScheduleDeps{
		home:   home,
		submit: submit.submit,
		now:    func() time.Time { return fx.now },
	})
	return fx
}

func scheduleSessionFile(home, sessionID string) string {
	return filepath.Join(home, "state", "schedules", sessionID+".json")
}

// readStoredItem 直接读真实落盘文件，断言 create/edit 写的是不是真记录。
func readStoredItem(t *testing.T, home, sessionID, id string) (schedule.Item, bool) {
	t.Helper()
	items, _, err := schedule.LoadWithRevision(filepath.Join(home, "state", "schedules"), sessionID)
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
	}
	return schedule.Item{}, false
}

func everyCreateRequest(sessionID string) transport.OfficialScheduleCreateRequest {
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: sessionID}
	return transport.OfficialScheduleCreateRequest{
		Title:  "喝水",
		Prompt: "起来喝水",
		Timing: transport.OfficialScheduleTiming{Kind: "every", EverySeconds: 300},
		Target: &target,
	}
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func TestOfficialScheduleHostCreatePersistsRealRecord(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)

	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if entry.Kind != "every" || entry.EverySeconds != 300 {
		t.Fatalf("返回的 entry 与请求不符: %#v", entry)
	}
	if entry.Status != "active" {
		t.Fatalf("新建任务 status = %q，want active", entry.Status)
	}
	if entry.ID == "" {
		t.Fatal("create 必须给出持久身份")
	}
	if entry.Target.Kind != "session" || entry.Target.SessionID != "s1" {
		t.Fatalf("target 回包错: %#v", entry.Target)
	}
	if entry.ScheduledAt != fx.now.Add(300*time.Second).UTC().Format(time.RFC3339) {
		t.Fatalf("scheduledAt = %q，want %q", entry.ScheduledAt,
			fx.now.Add(300*time.Second).UTC().Format(time.RFC3339))
	}
	if entry.CreatedAt == "" || entry.UpdatedAt == "" {
		t.Fatalf("createdAt/updatedAt 必须来自真实写入: %#v", entry)
	}

	// 真实落盘：字段与 internal/schedule 的 Item 形状一致，别的读者（schedule.list、
	// dshd schedule due）能读到同一条。
	item, ok := readStoredItem(t, home, "s1", entry.ID)
	if !ok {
		t.Fatalf("文件 %s 里没有 id=%s", scheduleSessionFile(home, "s1"), entry.ID)
	}
	if item.Title != "喝水" || item.Prompt != "起来喝水" || item.Kind != "every" || item.EverySec != 300 {
		t.Fatalf("落盘记录与请求不符: %#v", item)
	}
	if item.Status != "active" {
		t.Fatalf("落盘 status = %q，want active", item.Status)
	}
	if item.NextAt == "" {
		t.Fatal("落盘 next_at 不能为空（否则永远不会到期）")
	}
	if _, err := time.Parse(time.RFC3339, item.NextAt); err != nil {
		t.Fatalf("落盘 next_at 不是 RFC3339: %q (%v)", item.NextAt, err)
	}

	// 文件版本必须是当前白名单里的，否则重开就报"版本不兼容"。
	raw, err := os.ReadFile(scheduleSessionFile(home, "s1"))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if err := schedule.ValidateScheduleVersion(raw); err != nil {
		t.Fatalf("落盘版本不兼容: %v", err)
	}
}

func TestOfficialScheduleHostReopenStillReads(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// "重开 server"：同一个 home 新造一个宿主实例，不共享任何内存状态。
	reopened := newScheduleHostFixture(t, home).host
	view, err := reopened.api().View(entry.ID)
	if err != nil {
		t.Fatalf("重开后 View: %v", err)
	}
	got, ok := view.(transport.OfficialScheduleEntry)
	if !ok {
		t.Fatalf("View 返回 %T，want OfficialScheduleEntry", view)
	}
	if got.ID != entry.ID || got.Title != entry.Title || got.EverySeconds != 300 {
		t.Fatalf("重开后读到的是另一条: %#v", got)
	}
	if got.ScheduledAt != entry.ScheduledAt {
		t.Fatalf("重开后 scheduledAt 漂了：%q → %q", entry.ScheduledAt, got.ScheduledAt)
	}
}

func TestOfficialScheduleHostHomesAreIsolated(t *testing.T) {
	homeA := scheduleHome(t)
	homeB := scheduleHome(t)
	fxA := newScheduleHostFixture(t, homeA)
	fxB := newScheduleHostFixture(t, homeB)

	entry, err := fxA.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create(A): %v", err)
	}
	if _, err := os.Stat(scheduleSessionFile(homeB, "s1")); !os.IsNotExist(err) {
		t.Fatalf("B home 不该有 A 的文件: %v", err)
	}
	view, err := fxB.host.api().View(entry.ID)
	if err != nil {
		t.Fatalf("View(B): %v", err)
	}
	miss, ok := view.(transport.OfficialScheduleMiss)
	if !ok {
		t.Fatalf("B home 的 View 返回 %T，want miss", view)
	}
	if miss.Code != "schedule_not_found" || miss.ID != entry.ID {
		t.Fatalf("B home 的 miss 形状错: %#v", miss)
	}
	// 反过来 A 仍能读到，说明隔离不是"两边都空"。
	back, err := fxA.host.api().View(entry.ID)
	if err != nil {
		t.Fatalf("View(A): %v", err)
	}
	if _, ok := back.(transport.OfficialScheduleEntry); !ok {
		t.Fatalf("A home 读不到自己的任务: %#v", back)
	}
}

func TestOfficialScheduleHostCreateDailyWeekly(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	zone := time.Local.String()

	dailyTarget := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}
	daily, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "日报", Prompt: "写日报",
		Timing: transport.OfficialScheduleTiming{Kind: "daily", Daily: &transport.OfficialScheduleDailyTime{Time: "10:30:00", TimeZone: zone}},
		Target: &dailyTarget,
	})
	if err != nil {
		t.Fatalf("Create daily: %v", err)
	}
	if daily.Kind != "daily" || daily.Time != "10:30:00" || daily.TimeZone != zone {
		t.Fatalf("daily entry 形状错: %#v", daily)
	}
	item, ok := readStoredItem(t, home, "s1", daily.ID)
	if !ok || item.Kind != "daily" || item.DailyAt != "10:30" {
		t.Fatalf("daily 落盘错: %#v (found=%v)", item, ok)
	}
	// 10:30 已经过了（fixture 的 now 是 09:00 之前？不是：09:00 < 10:30），
	// 所以下一次就是今天 10:30 本地时间。
	dailyNext, err := time.Parse(time.RFC3339, item.NextAt)
	if err != nil {
		t.Fatalf("daily next_at 坏: %q", item.NextAt)
	}
	if want := time.Date(2026, 11, 7, 10, 30, 0, 0, time.Local); !dailyNext.Equal(want) {
		t.Fatalf("daily next_at = %v，want %v", dailyNext, want)
	}

	weeklyTarget := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s2"}
	weekly, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "周会", Prompt: "准备周会",
		Timing: transport.OfficialScheduleTiming{Kind: "weekly",
			Weekly: &transport.OfficialScheduleWeeklyTime{Time: "08:00", TimeZone: zone, Weekdays: []int{1, 3, 5}}},
		Target: &weeklyTarget,
	})
	if err != nil {
		t.Fatalf("Create weekly: %v", err)
	}
	if len(weekly.Weekdays) != 3 || weekly.Weekdays[0] != 1 {
		t.Fatalf("weekly entry 形状错: %#v", weekly)
	}
	weekItem, ok := readStoredItem(t, home, "s2", weekly.ID)
	if !ok || weekItem.Kind != "weekly" || weekItem.WeeklyAt != "08:00" {
		t.Fatalf("weekly 落盘错: %#v (found=%v)", weekItem, ok)
	}
	// 周任务的下一次必须落在 1/3/5（ISO 周一到周日）之一。
	next, err := time.Parse(time.RFC3339, weekItem.NextAt)
	if err != nil {
		t.Fatalf("weekly next_at 坏: %q", weekItem.NextAt)
	}
	switch next.Weekday() {
	case time.Monday, time.Wednesday, time.Friday:
	default:
		t.Fatalf("weekly next_at 落在 %v，不在 1/3/5 里", next.Weekday())
	}
}

// 真做不到的输入必须继续 fail-loud（M401 之后 cron/rrule/after/at 都是真实现，
// 这里只留"确实算不出来/确实没有真实支撑"的那几种）。
func TestOfficialScheduleHostCreateRejectsUnrealInputs(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}

	cases := []struct {
		name string
		req  transport.OfficialScheduleCreateRequest
	}{
		{
			name: "cron 表达式解析不了",
			req: transport.OfficialScheduleCreateRequest{Title: "t", Prompt: "p",
				Timing: transport.OfficialScheduleTiming{Kind: "cron", Cron: &transport.OfficialScheduleCron{Expression: "*/0 * * * *", TimeZone: "UTC"}},
				Target: &target},
		},
		{
			name: "cron 时区不存在",
			req: transport.OfficialScheduleCreateRequest{Title: "t", Prompt: "p",
				Timing: transport.OfficialScheduleTiming{Kind: "cron", Cron: &transport.OfficialScheduleCron{Expression: "*/5 * * * *", TimeZone: "Mars/Phobos"}},
				Target: &target},
		},
		{
			name: "rrule 缺 DTSTART",
			req: transport.OfficialScheduleCreateRequest{Title: "t", Prompt: "p",
				Timing: transport.OfficialScheduleTiming{Kind: "rrule", RRule: "RRULE:FREQ=DAILY"},
				Target: &target},
		},
		{
			name: "after 秒数不是正数",
			req: transport.OfficialScheduleCreateRequest{Title: "t", Prompt: "p",
				Timing: transport.OfficialScheduleTiming{Kind: "after", AfterSeconds: 0}, Target: &target},
		},
		{
			name: "at 已经过去",
			req: transport.OfficialScheduleCreateRequest{Title: "t", Prompt: "p",
				Timing: transport.OfficialScheduleTiming{Kind: "at", At: &transport.OfficialScheduleAt{Instant: "2020-11-08T01:00:00Z"}},
				Target: &target},
		},
		{
			name: "daily 时区不存在",
			req: transport.OfficialScheduleCreateRequest{Title: "t", Prompt: "p",
				Timing: transport.OfficialScheduleTiming{Kind: "daily",
					Daily: &transport.OfficialScheduleDailyTime{Time: "09:30", TimeZone: "Mars/Phobos"}},
				Target: &target},
		},
		{
			name: "timing.kind 不认识",
			req: transport.OfficialScheduleCreateRequest{Title: "t", Prompt: "p",
				Timing: transport.OfficialScheduleTiming{Kind: "whenever"}, Target: &target},
		},
		{
			name: "target.kind 不认识",
			req: transport.OfficialScheduleCreateRequest{Title: "t", Prompt: "p",
				Timing: transport.OfficialScheduleTiming{Kind: "every", EverySeconds: 300},
				Target: &transport.OfficialScheduleTarget{Kind: "nowhere"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fx.host.api().Create(tc.req)
			if !errors.Is(err, transport.ErrOfficialBadInput) {
				t.Fatalf("err = %v，want ErrOfficialBadInput", err)
			}
			// 失败的 create 不许留下半条记录。
			if items, _, err := schedule.LoadWithRevision(filepath.Join(home, "state", "schedules"), "s1"); err == nil && len(items) != 0 {
				t.Fatalf("失败的 create 落了 %d 条记录: %#v", len(items), items)
			}
			if filepath.Base(home) == "" {
				t.Fatal("unreachable")
			}
		})
	}
}

// 别的时区现在是真的能算：daily 09:30 America/New_York 必须落到正确的 UTC 瞬时。
func TestOfficialScheduleHostCreateDailyInForeignZone(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	fx.now = time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC) // 纽约 11-06 19:00（EST，UTC-5）
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}
	entry, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "纽约日报", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "daily",
			Daily: &transport.OfficialScheduleDailyTime{Time: "09:30:00", TimeZone: "America/New_York"}},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(daily NY): %v", err)
	}
	if entry.TimeZone != "America/New_York" {
		t.Fatalf("回包时区 = %q，want America/New_York", entry.TimeZone)
	}
	// 11-06 的 09:30 已过 → 下一次是 11-07 09:30 EST = 14:30Z。
	if want := "2026-11-07T14:30:00Z"; entry.ScheduledAt != want {
		t.Fatalf("scheduledAt = %q，want %q", entry.ScheduledAt, want)
	}
	item, ok := readStoredItem(t, home, "s1", entry.ID)
	if !ok || item.TimeZone != "America/New_York" {
		t.Fatalf("落盘时区错: %#v (found=%v)", item, ok)
	}
}

func TestOfficialScheduleHostCreateRequiresExplicitTarget(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	_, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "t", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "every", EverySeconds: 300},
	})
	if !errors.Is(err, transport.ErrOfficialBadInput) {
		t.Fatalf("没有 target 时 err = %v，want ErrOfficialBadInput（本宿主没有隐式调用会话）", err)
	}
}

// ---------------------------------------------------------------------------
// edit
// ---------------------------------------------------------------------------

func TestOfficialScheduleHostEditRevisionFence(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stale := "2000-01-01T00:00:00Z"
	title := "改过"
	// 官方把 revision 不匹配表达成 union 成员（200 + {id,code:'schedule_conflict'}），
	// 不是 409 错误体。
	staleResult, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch:             &transport.OfficialSchedulePatch{Title: &title},
		ExpectedUpdatedAt: &stale,
	})
	if err != nil {
		t.Fatalf("过期 fence 不是错误: %v", err)
	}
	if miss, ok := staleResult.(transport.OfficialScheduleMiss); !ok || miss.Code != "schedule_conflict" {
		t.Fatalf("过期 fence 返回 %#v，want schedule_conflict union", staleResult)
	}
	if item, _ := readStoredItem(t, home, "s1", entry.ID); item.Title != entry.Title {
		t.Fatalf("冲突也改到了落盘 title: %q", item.Title)
	}

	// 用真实 updatedAt 重试就能过；时间推进后 updatedAt 必须变。
	fx.now = fx.now.Add(time.Minute)
	good := entry.UpdatedAt
	updated, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch:             &transport.OfficialSchedulePatch{Title: &title},
		ExpectedUpdatedAt: &good,
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	got, ok := updated.(transport.OfficialScheduleEntry)
	if !ok {
		t.Fatalf("Edit 返回 %T，want OfficialScheduleEntry", updated)
	}
	if got.Title != "改过" {
		t.Fatalf("title 没改到: %#v", got)
	}
	if got.UpdatedAt == entry.UpdatedAt {
		t.Fatalf("updatedAt 没变（fence 就永远拦不住第二次）: %q", got.UpdatedAt)
	}
	item, ok := readStoredItem(t, home, "s1", entry.ID)
	if !ok || item.Title != "改过" {
		t.Fatalf("落盘没改到: %#v (found=%v)", item, ok)
	}
}

func TestOfficialScheduleHostEditTimingRecomputesNext(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch: &transport.OfficialSchedulePatch{
			Timing: &transport.OfficialScheduleTiming{Kind: "every", EverySeconds: 3600},
		},
	})
	if err != nil {
		t.Fatalf("Edit timing: %v", err)
	}
	got := updated.(transport.OfficialScheduleEntry)
	if got.EverySeconds != 3600 {
		t.Fatalf("everySeconds = %d，want 3600", got.EverySeconds)
	}
	if got.ScheduledAt == entry.ScheduledAt {
		t.Fatalf("改了周期却没重算 scheduledAt: %q", got.ScheduledAt)
	}
	item, _ := readStoredItem(t, home, "s1", entry.ID)
	if item.EverySec != 3600 {
		t.Fatalf("落盘 every_seconds = %d，want 3600", item.EverySec)
	}
}

// 暂停必须真的落盘：status=paused、next_at 一格不动；用 cron 恢复时按表达式重算。
func TestOfficialScheduleHostEditPauseIsRealAndPersists(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	fx.now = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}
	entry, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "每小时", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "cron",
			Cron: &transport.OfficialScheduleCron{Expression: "0 * * * *", TimeZone: "UTC"}},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	paused := false
	if _, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update", Patch: &transport.OfficialSchedulePatch{Active: &paused},
	}); err != nil {
		t.Fatalf("暂停: %v", err)
	}
	item, _ := readStoredItem(t, home, "s1", entry.ID)
	if item.Status != schedule.StatusPaused {
		t.Fatalf("落盘 status = %q，want paused", item.Status)
	}
	if item.NextAt != entry.ScheduledAt {
		t.Fatalf("暂停改动了 next_at：%q → %q", entry.ScheduledAt, item.NextAt)
	}

	// 重开宿主仍然是 paused（内存里不暂停、磁盘上暂停，等于没暂停）。
	reopened := newScheduleHostFixture(t, home).host
	viewAny, err := reopened.api().View(entry.ID)
	if err != nil {
		t.Fatalf("重开后 View: %v", err)
	}
	if view := scheduleEntryOf(t, viewAny); view.Status != "paused" {
		t.Fatalf("重开后 status = %q，want paused", view.Status)
	}

	// 03:20 恢复：下一次是 04:00（按表达式），不是"恢复时刻 + 1 小时"。
	fx.now = time.Date(2026, 11, 7, 3, 20, 0, 0, time.UTC)
	on := true
	resumedAny, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update", Patch: &transport.OfficialSchedulePatch{Active: &on},
	})
	if err != nil {
		t.Fatalf("恢复: %v", err)
	}
	resumed := scheduleEntryOf(t, resumedAny)
	if want := "2026-11-07T04:00:00Z"; resumed.ScheduledAt != want {
		t.Fatalf("cron 恢复后 scheduledAt = %q，want %q", resumed.ScheduledAt, want)
	}
	if resumed.Status != "active" {
		t.Fatalf("恢复后 status = %q，want active", resumed.Status)
	}
}

func TestOfficialScheduleHostEditDeleteAndMiss(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deleted, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{ID: entry.ID, Action: "delete"})
	if err != nil {
		t.Fatalf("Edit delete: %v", err)
	}
	got, ok := deleted.(transport.OfficialScheduleDeleted)
	if !ok || !got.Deleted || got.ID != entry.ID {
		t.Fatalf("delete 结果错: %#v", deleted)
	}
	if _, ok := readStoredItem(t, home, "s1", entry.ID); ok {
		t.Fatal("delete 之后文件里还有这条")
	}

	// 再删一次：官方是 `schedule_not_found` 的 union 成员，不是 500。
	again, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{ID: entry.ID, Action: "delete"})
	if err != nil {
		t.Fatalf("重复删除: %v", err)
	}
	miss, ok := again.(transport.OfficialScheduleMiss)
	if !ok || miss.Code != "schedule_not_found" {
		t.Fatalf("重复删除的返回 %#v，want miss(schedule_not_found)", again)
	}
}

func TestOfficialScheduleHostEditMovesTargetAcrossSessions(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	moved, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch: &transport.OfficialSchedulePatch{
			Target: &transport.OfficialScheduleTarget{Kind: "session", SessionID: "s2"},
		},
	})
	if err != nil {
		t.Fatalf("Edit target: %v", err)
	}
	got := moved.(transport.OfficialScheduleEntry)
	if got.Target.SessionID != "s2" {
		t.Fatalf("target 没搬: %#v", got.Target)
	}
	if _, ok := readStoredItem(t, home, "s1", entry.ID); ok {
		t.Fatal("搬走后旧会话文件里还留着这条")
	}
	if _, ok := readStoredItem(t, home, "s2", entry.ID); !ok {
		t.Fatal("搬走后新会话文件里没有这条")
	}
}

// readStoredRawEntry 原样读一条落盘记录的键集合（用来钉住"换 kind 后旧字段必须消失"）。
func readStoredRawEntry(t *testing.T, home, sessionID, id string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(scheduleSessionFile(home, sessionID))
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	var doc struct {
		Schedules []map[string]json.RawMessage `json:"schedules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析落盘文件: %v", err)
	}
	for _, entry := range doc.Schedules {
		var gotID string
		if err := json.Unmarshal(entry["id"], &gotID); err == nil && gotID == id {
			return entry
		}
	}
	t.Fatalf("落盘文件里没有 id=%s", id)
	return nil
}

func TestOfficialScheduleHostEditTimingAndTargetInOnePatch(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch: &transport.OfficialSchedulePatch{
			Timing: &transport.OfficialScheduleTiming{Kind: "every", EverySeconds: 600},
			Target: &transport.OfficialScheduleTarget{Kind: "session", SessionID: "s2"},
		},
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	got := updated.(transport.OfficialScheduleEntry)
	// 一次 patch 里两个字段都必须生效：先搬后算会把 timing 丢掉（静默部分成功）。
	if got.EverySeconds != 600 {
		t.Fatalf("everySeconds = %d，want 600（timing 被搬移吃掉了）", got.EverySeconds)
	}
	if got.Target.SessionID != "s2" {
		t.Fatalf("target = %#v，want s2", got.Target)
	}
	item, ok := readStoredItem(t, home, "s2", entry.ID)
	if !ok || item.EverySec != 600 {
		t.Fatalf("新会话文件里的记录不对: %#v (found=%v)", item, ok)
	}
	if _, ok := readStoredItem(t, home, "s1", entry.ID); ok {
		t.Fatal("旧会话文件里还留着这条")
	}
}

func TestOfficialScheduleHostEditKindSwitchClearsOldFields(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch: &transport.OfficialSchedulePatch{
			Timing: &transport.OfficialScheduleTiming{Kind: "daily",
				Daily: &transport.OfficialScheduleDailyTime{Time: "11:15", TimeZone: time.Local.String()}},
		},
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	raw := readStoredRawEntry(t, home, "s1", entry.ID)
	if _, ok := raw["every_seconds"]; ok {
		t.Fatalf("换成 daily 之后落盘还带 every_seconds: %v", raw)
	}
	if _, ok := raw["daily_at"]; !ok {
		t.Fatalf("换成 daily 之后落盘没有 daily_at: %v", raw)
	}
}

// ---------------------------------------------------------------------------
// view / run / history
// ---------------------------------------------------------------------------

func TestOfficialScheduleHostRunTriggersRealSubmission(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ran, err := fx.host.api().Run(entry.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, ok := ran.(transport.OfficialScheduleEntry)
	if !ok {
		t.Fatalf("Run 返回 %T，want OfficialScheduleEntry", ran)
	}
	if fx.submit.count() != 1 {
		t.Fatalf("真执行次数 = %d，want 1", fx.submit.count())
	}
	if call := fx.submit.last(); call.sessionID != "s1" || call.text != "起来喝水" {
		t.Fatalf("提交到 %q / %q，want s1 / 起来喝水", call.sessionID, call.text)
	}
	if got.ScheduledAt != entry.ScheduledAt {
		t.Fatalf("run 不该移动计划时刻：%q → %q", entry.ScheduledAt, got.ScheduledAt)
	}
	// lastDelivery 必须来自这次真实提交。
	if got.LastDelivery == nil || got.LastDelivery.MessageID != "turn-1" {
		t.Fatalf("lastDelivery 不是真实提交的凭据: %#v", got.LastDelivery)
	}
	if got.LastDelivery.TargetSessionID != "s1" || got.LastDelivery.ScheduledAt != entry.ScheduledAt {
		t.Fatalf("lastDelivery 字段错: %#v", got.LastDelivery)
	}
	// 计划时刻不被消费：落盘 next_at 不变。
	item, _ := readStoredItem(t, home, "s1", entry.ID)
	if item.NextAt != entry.ScheduledAt {
		t.Fatalf("run 之后落盘 next_at 变了：%q → %q", entry.ScheduledAt, item.NextAt)
	}
}

func TestOfficialScheduleHostRunWithoutRunnerIsNotImplemented(t *testing.T) {
	home := scheduleHome(t)
	host := newOfficialScheduleHost(officialScheduleDeps{home: home})
	entry, err := host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := host.api().Run(entry.ID); !errors.Is(err, transport.ErrOfficialNotImplemented) {
		t.Fatalf("没有 runner 时 err = %v，want ErrOfficialNotImplemented（不许假执行）", err)
	}
}

func TestOfficialScheduleHostRunMissAndPending(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)

	missed, err := fx.host.api().Run("sch-nope")
	if err != nil {
		t.Fatalf("Run(unknown): %v", err)
	}
	miss, ok := missed.(transport.OfficialScheduleMiss)
	if !ok || miss.Code != "schedule_not_found" {
		t.Fatalf("未知 id 的 Run 返回 %#v", missed)
	}

	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 卡住一次提交，让并发 run 撞上 in-flight。
	block := make(chan struct{})
	release := make(chan struct{})
	fx.host.deps.submit = func(sessionID, text string) (transport.PromptResult, error) {
		close(block)
		<-release
		return transport.PromptResult{Accepted: true, TurnID: "turn-x"}, nil
	}
	done := make(chan any, 1)
	go func() {
		res, err := fx.host.api().Run(entry.ID)
		if err != nil {
			done <- err
			return
		}
		done <- res
	}()
	<-block
	pending, err := fx.host.api().Run(entry.ID)
	if err != nil {
		t.Fatalf("并发 Run: %v", err)
	}
	if p, ok := pending.(transport.OfficialScheduleMiss); !ok || p.Code != "schedule_execution_pending" {
		t.Fatalf("并发 Run 返回 %#v，want schedule_execution_pending", pending)
	}
	close(release)
	if _, ok := (<-done).(transport.OfficialScheduleEntry); !ok {
		t.Fatal("第一次 Run 应该成功返回 entry")
	}
}

func TestOfficialScheduleHostRunBusyTargetIsPending(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 目标会话上一轮还在跑：验收标准是"官方 pending union"，不是 500。
	fx.submit.err = ErrBusy
	res, err := fx.host.api().Run(entry.ID)
	if err != nil {
		t.Fatalf("Run: %v（busy 应该是 pending 结果，不是错误）", err)
	}
	if miss, ok := res.(transport.OfficialScheduleMiss); !ok || miss.Code != "schedule_execution_pending" {
		t.Fatalf("busy 时 Run 返回 %#v，want schedule_execution_pending", res)
	}
	// 被拒的投递不许留下历史记录。
	page, err := fx.host.api().History(transport.OfficialScheduleHistoryRequest{ID: entry.ID, Limit: 10})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if got := page.(transport.OfficialScheduleHistoryPage); len(got.Records) != 0 {
		t.Fatalf("busy 也写了历史: %#v", got.Records)
	}
}

func TestOfficialScheduleHostHistoryIsRealAndPaged(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	empty, err := fx.host.api().History(transport.OfficialScheduleHistoryRequest{ID: entry.ID, Limit: 10})
	if err != nil {
		t.Fatalf("History(空): %v", err)
	}
	page := empty.(transport.OfficialScheduleHistoryPage)
	if len(page.Records) != 0 {
		t.Fatalf("还没跑过就有记录: %#v", page.Records)
	}
	if page.EarlierRecordsPruned || page.EarlierRecordsUnavailable {
		t.Fatalf("没跑过不该说自己剪裁过: %#v", page)
	}
	if page.Retention.Days <= 0 || page.Retention.Records <= 0 {
		t.Fatalf("retention 必须是真实配置: %#v", page.Retention)
	}

	for i := 0; i < 3; i++ {
		if _, err := fx.host.api().Run(entry.ID); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}

	// 重开宿主：历史必须来自磁盘，不是内存。
	reopened := newScheduleHostFixture(t, home).host
	all, err := reopened.api().History(transport.OfficialScheduleHistoryRequest{ID: entry.ID, Limit: 10})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	full := all.(transport.OfficialScheduleHistoryPage)
	if len(full.Records) != 3 {
		t.Fatalf("记录数 = %d，want 3", len(full.Records))
	}
	// 官方 newest first。
	if full.Records[0].MessageID != "turn-3" || full.Records[2].MessageID != "turn-1" {
		t.Fatalf("记录不是 newest first: %#v", full.Records)
	}
	if full.Records[0].Prompt != "起来喝水" {
		t.Fatalf("记录没带 prompt: %#v", full.Records[0])
	}
	if full.Records[0].DeliveredAt == "" || full.Records[0].ScheduledAt == "" {
		t.Fatalf("记录时间字段不全: %#v", full.Records[0])
	}

	// 分页：limit=2 → 最新两条 + nextBefore=该页最旧的 messageId。
	first, err := reopened.api().History(transport.OfficialScheduleHistoryRequest{ID: entry.ID, Limit: 2})
	if err != nil {
		t.Fatalf("History(2): %v", err)
	}
	p1 := first.(transport.OfficialScheduleHistoryPage)
	if len(p1.Records) != 2 || p1.Records[0].MessageID != "turn-3" || p1.Records[1].MessageID != "turn-2" {
		t.Fatalf("第一页错: %#v", p1.Records)
	}
	if p1.NextBefore != "turn-2" {
		t.Fatalf("nextBefore = %q，want turn-2", p1.NextBefore)
	}
	second, err := reopened.api().History(transport.OfficialScheduleHistoryRequest{ID: entry.ID, Limit: 2, Before: p1.NextBefore})
	if err != nil {
		t.Fatalf("History(before): %v", err)
	}
	p2 := second.(transport.OfficialScheduleHistoryPage)
	if len(p2.Records) != 1 || p2.Records[0].MessageID != "turn-1" {
		t.Fatalf("第二页错: %#v", p2.Records)
	}
	if p2.NextBefore != "" {
		t.Fatalf("最后一页不该再有 nextBefore: %q", p2.NextBefore)
	}
	// 游标指不到真实记录 → 官方 delivery_cursor_not_found。
	cursorMiss, err := reopened.api().History(transport.OfficialScheduleHistoryRequest{ID: entry.ID, Limit: 2, Before: "turn-404"})
	if err != nil {
		t.Fatalf("History(bad cursor): %v", err)
	}
	if m, ok := cursorMiss.(transport.OfficialScheduleMiss); !ok || m.Code != "delivery_cursor_not_found" {
		t.Fatalf("坏游标返回 %#v", cursorMiss)
	}
	// 未知任务 → schedule_not_found。
	notFound, err := reopened.api().History(transport.OfficialScheduleHistoryRequest{ID: "sch-nope", Limit: 2})
	if err != nil {
		t.Fatalf("History(unknown): %v", err)
	}
	if m, ok := notFound.(transport.OfficialScheduleMiss); !ok || m.Code != "schedule_not_found" {
		t.Fatalf("未知任务的 history 返回 %#v", notFound)
	}
}

func TestOfficialScheduleHostHistoryIsBoundedByRetention(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 越过记录上限后，旧记录被真实剪裁，且必须自报 pruned。
	for i := 0; i < officialScheduleHistoryMaxRecords+5; i++ {
		if _, err := fx.host.api().Run(entry.ID); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}
	page, err := fx.host.api().History(transport.OfficialScheduleHistoryRequest{ID: entry.ID, Limit: 100})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	got := page.(transport.OfficialScheduleHistoryPage)
	if len(got.Records) != officialScheduleHistoryMaxRecords {
		t.Fatalf("保留 %d 条，want %d", len(got.Records), officialScheduleHistoryMaxRecords)
	}
	if !got.EarlierRecordsPruned || !got.EarlierRecordsUnavailable {
		t.Fatalf("剪裁后必须自报 pruned/unavailable: %#v", got)
	}
}

// ---------------------------------------------------------------------------
// HTTP 层：与 transport 的官方入口一起跑（接线后 server.go 只需一行）
// ---------------------------------------------------------------------------

func TestOfficialScheduleHostOverHTTP(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	api := fx.host.api()

	post := func(method, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/"+method, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		transport.ServeOfficialSchedule(rec, req, method, api)
		var envelope map[string]any
		if rec.Body.Len() > 0 {
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("%s 响应不是 JSON: %q", method, rec.Body.String())
			}
		}
		// 成功信封是 {"result": …}；错误信封是四字段 {"error": …}。
		if rec.Code < 400 {
			payload, ok := envelope["result"].(map[string]any)
			if !ok {
				t.Fatalf("%s 的成功响应没有 result 对象: %q", method, rec.Body.String())
			}
			return rec.Code, payload
		}
		return rec.Code, envelope
	}

	code, out := post("schedule.create",
		`{"request":{"title":"喝水","prompt":"起来喝水","timing":{"kind":"every","every_seconds":300},"target":{"kind":"session","sessionId":"s1"}}}`)
	if code != http.StatusOK {
		t.Fatalf("create 状态 = %d: %#v", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create 没给 id: %#v", out)
	}

	code, out = post("schedule.view", `{"id":"`+id+`"}`)
	if code != http.StatusOK || out["id"] != id {
		t.Fatalf("view 状态 = %d: %#v", code, out)
	}

	code, out = post("schedule.run", `{"id":"`+id+`"}`)
	if code != http.StatusOK {
		t.Fatalf("run 状态 = %d: %#v", code, out)
	}
	if fx.submit.count() != 1 {
		t.Fatalf("HTTP run 没真触发提交: %d", fx.submit.count())
	}

	code, out = post("schedule.history", `{"id":"`+id+`","limit":10}`)
	if code != http.StatusOK {
		t.Fatalf("history 状态 = %d: %#v", code, out)
	}
	records, _ := out["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("history 记录数 = %d，want 1: %#v", len(records), out)
	}

	// 未知字段必须 400，而不是静默忽略。
	code, out = post("schedule.view", `{"id":"`+id+`","extra":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("带未知字段时状态 = %d，want 400: %#v", code, out)
	}
	if _, ok := out["error"].(map[string]any); !ok {
		t.Fatalf("错误体不是四字段信封: %#v", out)
	}

	// 重复删除 → 官方 miss union（200），不是错误。
	code, out = post("schedule.edit", `{"id":"`+id+`","action":"delete"}`)
	if code != http.StatusOK || out["deleted"] != true {
		t.Fatalf("delete 状态 = %d: %#v", code, out)
	}
	code, out = post("schedule.edit", `{"id":"`+id+`","action":"delete"}`)
	if code != http.StatusOK || out["code"] != "schedule_not_found" {
		t.Fatalf("重复删除状态 = %d: %#v", code, out)
	}
	if !strings.Contains(fx.home, filepath.Base(fx.home)) {
		t.Fatal("unreachable")
	}
}

// ---------------------------------------------------------------------------
// B. 时机语义：after / at / cron / rrule 必须真能算出下一次
// ---------------------------------------------------------------------------

func scheduleParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("%q 不是 RFC3339: %v", s, err)
	}
	return ts
}

func scheduleEntryOf(t *testing.T, v any) transport.OfficialScheduleEntry {
	t.Helper()
	entry, ok := v.(transport.OfficialScheduleEntry)
	if !ok {
		t.Fatalf("返回 %T，want OfficialScheduleEntry", v)
	}
	return entry
}

func scheduleLoadDir(t *testing.T, home, sessionID string) []schedule.Item {
	t.Helper()
	items, err := schedule.Load(filepath.Join(home, "state", "schedules"), sessionID)
	if err != nil {
		t.Fatalf("读 %s: %v", sessionID, err)
	}
	return items
}

// after 是一次性：到点触发一次之后必须 ended，绝不能被日推一天变成"天天响"。
func TestOfficialScheduleHostCreateAfterIsOneShot(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}

	entry, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "提醒", Prompt: "十点开会",
		Timing: transport.OfficialScheduleTiming{Kind: "after", AfterSeconds: 900},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(after): %v", err)
	}
	if entry.Kind != "after" || entry.AfterSeconds != 900 {
		t.Fatalf("after entry 形状错: %#v", entry)
	}
	want := fx.now.Add(900 * time.Second).UTC().Format(time.RFC3339)
	if entry.ScheduledAt != want {
		t.Fatalf("after scheduledAt = %q，want %q", entry.ScheduledAt, want)
	}
	item, ok := readStoredItem(t, home, "s1", entry.ID)
	if !ok || item.Kind != "after" || item.AfterSec != 900 || item.NextAt != want {
		t.Fatalf("after 落盘错: %#v (found=%v)", item, ok)
	}

	// 到点：真到期，推进后必须 ended，且不再有下一次。
	now := scheduleParse(t, entry.ScheduledAt).Add(time.Second)
	dues := schedule.DueAt(scheduleLoadDir(t, home, "s1"), now)
	if len(dues) != 1 {
		t.Fatalf("到点应当有 1 条 due，得到 %d", len(dues))
	}
	if !dues[0].Ends {
		t.Fatalf("after 到点后必须结束，不许再排下一次: %+v", dues[0])
	}
	if n, err := schedule.Advance(filepath.Join(home, "state", "schedules"), "s1", dues, now); err != nil || n != 1 {
		t.Fatalf("Advance: n=%d err=%v", n, err)
	}
	back := scheduleLoadDir(t, home, "s1")
	if back[0].Status != "ended" {
		t.Fatalf("一次性任务触发后 status = %q，want ended", back[0].Status)
	}
	if again := schedule.DueAt(back, now.Add(48*time.Hour)); len(again) != 0 {
		t.Fatalf("ended 的任务还在到期: %+v", again)
	}
}

// at 是绝对时刻：字符串与 {date,time,time_zone} 两种写法都必须落到同一个 UTC 瞬时。
func TestOfficialScheduleHostCreateAtAbsolute(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}

	instant, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "绝对", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "at",
			At: &transport.OfficialScheduleAt{Instant: "2026-11-08T01:30:00Z"}},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(at string): %v", err)
	}
	if instant.Kind != "at" || instant.ScheduledAt != "2026-11-08T01:30:00Z" {
		t.Fatalf("at(string) 形状错: %#v", instant)
	}

	obj, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "绝对2", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "at",
			At: &transport.OfficialScheduleAt{Date: "2026-11-08", Clock: "09:30:00", TimeZone: "Asia/Shanghai"}},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(at object): %v", err)
	}
	// 上海 09:30 = UTC 01:30，必须真按 zone 算。
	if want := "2026-11-08T01:30:00Z"; obj.ScheduledAt != want {
		t.Fatalf("at(object) scheduledAt = %q，want %q", obj.ScheduledAt, want)
	}

	// 过去的绝对时刻必须 fail-loud：收下它等于一条永不触发的任务。
	_, err = fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "过去", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "at",
			At: &transport.OfficialScheduleAt{Instant: "2020-01-01T00:00:00Z"}},
		Target: &target,
	})
	if !errors.Is(err, transport.ErrOfficialBadInput) {
		t.Fatalf("过去的 at err = %v，want ErrOfficialBadInput", err)
	}
}

// cron 必须真解析、真算下一次；算不出来的表达式必须 400 并说出来。
func TestOfficialScheduleHostCreateCronComputesRealNext(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	fx.now = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC) // 周六
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}

	entry, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "每五分钟", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "cron",
			Cron: &transport.OfficialScheduleCron{Expression: "*/5 * * * *", TimeZone: "UTC"}},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(cron): %v", err)
	}
	if entry.Kind != "cron" || entry.Expression != "*/5 * * * *" || entry.TimeZone != "UTC" {
		t.Fatalf("cron entry 形状错: %#v", entry)
	}
	if want := "2026-11-07T09:05:00Z"; entry.ScheduledAt != want {
		t.Fatalf("cron scheduledAt = %q，want %q", entry.ScheduledAt, want)
	}
	item, ok := readStoredItem(t, home, "s1", entry.ID)
	if !ok || item.Kind != "cron" || item.Cron != "*/5 * * * *" || item.TimeZone != "UTC" {
		t.Fatalf("cron 落盘错: %#v (found=%v)", item, ok)
	}

	// 推进必须按表达式算（09:05 之后是 09:10），不是按固定间隔瞎加。
	now := scheduleParse(t, entry.ScheduledAt).Add(time.Second)
	dues := schedule.DueAt(scheduleLoadDir(t, home, "s1"), now)
	if len(dues) != 1 || dues[0].Ends {
		t.Fatalf("cron 到点结果错: %+v", dues)
	}
	if n, err := schedule.Advance(filepath.Join(home, "state", "schedules"), "s1", dues, now); err != nil || n != 1 {
		t.Fatalf("Advance: n=%d err=%v", n, err)
	}
	back := scheduleLoadDir(t, home, "s1")
	if want := "2026-11-07T09:10:00Z"; back[0].NextAt != want {
		t.Fatalf("cron 推进后 next_at = %q，want %q", back[0].NextAt, want)
	}
	if back[0].Status != "active" {
		t.Fatalf("周期任务推进后 status = %q，want active", back[0].Status)
	}

	// 工作日 09:00：周六的下一次是周一。
	weekday, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "工作日", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "cron",
			Cron: &transport.OfficialScheduleCron{Expression: "0 9 * * 1-5", TimeZone: "UTC"}},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(cron 工作日): %v", err)
	}
	if want := "2026-11-09T09:00:00Z"; weekday.ScheduledAt != want {
		t.Fatalf("工作日 cron scheduledAt = %q，want %q", weekday.ScheduledAt, want)
	}

	// 解析不了的表达式：400，且错误里必须出现表达式本身（能定位是哪一条）。
	for _, bad := range []string{"*/0 * * * *", "not a cron", "* * * *", "61 * * * *"} {
		_, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
			Title: "坏表达式", Prompt: "p",
			Timing: transport.OfficialScheduleTiming{Kind: "cron",
				Cron: &transport.OfficialScheduleCron{Expression: bad, TimeZone: "UTC"}},
			Target: &target,
		})
		if !errors.Is(err, transport.ErrOfficialBadInput) {
			t.Fatalf("cron=%q 的 err = %v，want ErrOfficialBadInput", bad, err)
		}
		if !strings.Contains(err.Error(), bad) {
			t.Fatalf("cron=%q 的错误没说清是哪条表达式: %v", bad, err)
		}
	}
}

// rrule 必须真解析、真算下一次，并按 COUNT/INTERVAL 收敛。
func TestOfficialScheduleHostCreateRRuleComputesRealNext(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	fx.now = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}

	entry, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "隔天", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "rrule",
			RRule: "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;INTERVAL=2"},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(rrule): %v", err)
	}
	if entry.Kind != "rrule" || entry.RRule == "" {
		t.Fatalf("rrule entry 形状错: %#v", entry)
	}
	// DTSTART 就是 now：严格晚于 now 的下一次是 11-09。
	if want := "2026-11-09T09:00:00Z"; entry.ScheduledAt != want {
		t.Fatalf("rrule scheduledAt = %q，want %q", entry.ScheduledAt, want)
	}
	item, ok := readStoredItem(t, home, "s1", entry.ID)
	if !ok || item.Kind != "rrule" || item.RRule == "" || item.NextAt != entry.ScheduledAt {
		t.Fatalf("rrule 落盘错: %#v (found=%v)", item, ok)
	}

	// COUNT=2：只有 11-07 与 11-08 两格，推进一次之后就该 ended。
	counted, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "两次", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "rrule",
			RRule: "DTSTART:20261107T090000Z\nRRULE:FREQ=DAILY;COUNT=2"},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(rrule COUNT): %v", err)
	}
	now := scheduleParse(t, counted.ScheduledAt).Add(time.Second)
	var dues []schedule.Due
	for _, it := range scheduleLoadDir(t, home, "s1") {
		if it.ID == counted.ID {
			dues = schedule.DueAt([]schedule.Item{it}, now)
		}
	}
	if len(dues) != 1 || !dues[0].Ends {
		t.Fatalf("COUNT 用尽的那一格必须结束: %+v", dues)
	}

	// 解析不了的规则 + 没有未来的规则都要 400。
	for _, bad := range []string{"RRULE:FREQ=DAILY", "DTSTART:20261107T090000Z\nRRULE:FREQ=NEVER", "DTSTART:xxx\nRRULE:FREQ=DAILY"} {
		_, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
			Title: "坏规则", Prompt: "p",
			Timing: transport.OfficialScheduleTiming{Kind: "rrule", RRule: bad},
			Target: &target,
		})
		if !errors.Is(err, transport.ErrOfficialBadInput) {
			t.Fatalf("rrule=%q 的 err = %v，want ErrOfficialBadInput", bad, err)
		}
	}
}

// ---------------------------------------------------------------------------
// B. patch.active：暂停必须真的不 due，恢复必须按原表达式算下一次
// ---------------------------------------------------------------------------

func TestOfficialScheduleHostPauseSuppressesDueAndResumeRecomputes(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	dir := filepath.Join(home, "state", "schedules")
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	off := false
	pausedAny, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch: &transport.OfficialSchedulePatch{Active: &off},
	})
	if err != nil {
		t.Fatalf("暂停: %v", err)
	}
	paused := scheduleEntryOf(t, pausedAny)
	if paused.Status != "paused" {
		t.Fatalf("暂停后 status = %q，want paused", paused.Status)
	}
	if paused.ScheduledAt != entry.ScheduledAt {
		t.Fatalf("暂停不许平移 next_at：%q → %q", entry.ScheduledAt, paused.ScheduledAt)
	}

	// 暂停期间即使过了到期时刻，也不许 due、不许被 Advance 推走。
	later := fx.now.Add(2 * time.Hour)
	if got := schedule.DueAt(scheduleLoadDir(t, home, "s1"), later); len(got) != 0 {
		t.Fatalf("暂停期间还在 due: %+v", got)
	}
	if n, err := schedule.Advance(dir, "s1", nil, later); err != nil || n != 0 {
		t.Fatalf("暂停期间 Advance 不该推进: n=%d err=%v", n, err)
	}

	// 恢复：按 every 语义从恢复时刻重新起算（官方 createEveryScheduleRecord: now+interval）。
	fx.now = later
	on := true
	resumedAny, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch: &transport.OfficialSchedulePatch{Active: &on},
	})
	if err != nil {
		t.Fatalf("恢复: %v", err)
	}
	resumed := scheduleEntryOf(t, resumedAny)
	if resumed.Status != "active" {
		t.Fatalf("恢复后 status = %q，want active", resumed.Status)
	}
	if want := fx.now.Add(300 * time.Second).UTC().Format(time.RFC3339); resumed.ScheduledAt != want {
		t.Fatalf("恢复后 scheduledAt = %q，want %q（不许把暂停期间的次数补跑）", resumed.ScheduledAt, want)
	}
	got := schedule.DueAt(scheduleLoadDir(t, home, "s1"), fx.now.Add(301*time.Second))
	if len(got) != 1 {
		t.Fatalf("恢复后到点应当 due 1 条，得到 %d", len(got))
	}
}

// daily 的恢复必须按原表达式（墙钟时刻）算，不是"恢复时刻 + 24h"。
func TestOfficialScheduleHostResumeDailyUsesTheRule(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	target := transport.OfficialScheduleTarget{Kind: "session", SessionID: "s1"}
	entry, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "日报", Prompt: "写日报",
		Timing: transport.OfficialScheduleTiming{Kind: "daily",
			Daily: &transport.OfficialScheduleDailyTime{Time: "10:30:00", TimeZone: time.Local.String()}},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	off := false
	if _, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update", Patch: &transport.OfficialSchedulePatch{Active: &off},
	}); err != nil {
		t.Fatalf("暂停: %v", err)
	}
	// 11:00 恢复：今天 10:30 已过，下一次必须是明天 10:30。
	fx.now = time.Date(2026, 11, 7, 11, 0, 0, 0, time.Local)
	on := true
	resumedAny, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update", Patch: &transport.OfficialSchedulePatch{Active: &on},
	})
	if err != nil {
		t.Fatalf("恢复: %v", err)
	}
	resumed := scheduleEntryOf(t, resumedAny)
	want := time.Date(2026, 11, 8, 10, 30, 0, 0, time.Local).UTC().Format(time.RFC3339)
	if resumed.ScheduledAt != want {
		t.Fatalf("daily 恢复后 scheduledAt = %q，want %q", resumed.ScheduledAt, want)
	}
}

// ---------------------------------------------------------------------------
// B. target=new / new_once：真建会话、真持久化、失败回滚
// ---------------------------------------------------------------------------

// sessionDirExists 在 <home>/sessions/*/<segment> 里找会话目录（真落盘才算建成）。
func sessionDirExists(t *testing.T, home, sessionID string) bool {
	t.Helper()
	seg, err := v4.EncodeSegment(sessionID)
	if err != nil {
		t.Fatalf("EncodeSegment(%q): %v", sessionID, err)
	}
	matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", seg))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return len(matches) > 0
}

func TestOfficialScheduleHostTargetNewOnceCreatesRealSession(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)

	target := transport.OfficialScheduleTarget{Kind: "new_once", Create: &transport.OfficialScheduleCreateInfo{}}
	entry, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "开个新会话", Prompt: "第一次投递建会话",
		Timing: transport.OfficialScheduleTiming{Kind: "after", AfterSeconds: 600},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(new_once): %v", err)
	}
	if entry.Target.Kind != "new_once" || entry.Target.SessionID == "" {
		t.Fatalf("new_once 必须带上真实会话身份: %#v", entry.Target)
	}
	if !sessionDirExists(t, home, entry.Target.SessionID) {
		t.Fatalf("new_once 的会话 %q 没有真的建到磁盘上", entry.Target.SessionID)
	}
	// 重启后仍指向同一个会话。
	reopened := newScheduleHostFixture(t, home).host
	viewAny, err := reopened.api().View(entry.ID)
	if err != nil {
		t.Fatalf("重开后 View: %v", err)
	}
	view := scheduleEntryOf(t, viewAny)
	if view.Target.Kind != "new_once" || view.Target.SessionID != entry.Target.SessionID {
		t.Fatalf("重开后 target = %#v，want new_once/%s", view.Target, entry.Target.SessionID)
	}
	// 投递真的打到那个会话上。
	if _, err := fx.host.api().Run(entry.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if call := fx.submit.last(); call.sessionID != entry.Target.SessionID {
		t.Fatalf("投递到 %q，want %q", call.sessionID, entry.Target.SessionID)
	}
}

func TestOfficialScheduleHostTargetNewCreatesFreshSessionPerDelivery(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	target := transport.OfficialScheduleTarget{Kind: "new", Create: &transport.OfficialScheduleCreateInfo{}}
	entry, err := fx.host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "每次都新开", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "every", EverySeconds: 300},
		Target: &target,
	})
	if err != nil {
		t.Fatalf("Create(new): %v", err)
	}
	if entry.Target.Kind != "new" {
		t.Fatalf("new target 形状错: %#v", entry.Target)
	}
	if _, err := fx.host.api().Run(entry.ID); err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	first := fx.submit.last().sessionID
	if first == "" || !sessionDirExists(t, home, first) {
		t.Fatalf("new 目标第一次投递没有真建会话: %q", first)
	}
	if _, err := fx.host.api().Run(entry.ID); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	second := fx.submit.last().sessionID
	if second == first {
		t.Fatalf("new 目标每次都该是新会话，两次都用了 %q", first)
	}
	if !sessionDirExists(t, home, second) {
		t.Fatalf("第二次投递的会话 %q 没落盘", second)
	}
}

func TestOfficialScheduleHostTargetCreateFailureRollsBack(t *testing.T) {
	home := scheduleHome(t)
	host := newOfficialScheduleHost(officialScheduleDeps{
		home:   home,
		submit: (&fakeScheduleSubmit{}).submit,
		now:    func() time.Time { return time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC) },
		createSession: func(cwd, preset string) (string, error) {
			return "", errors.New("会话控制器拒绝建会话")
		},
	})
	target := transport.OfficialScheduleTarget{Kind: "new_once", Create: &transport.OfficialScheduleCreateInfo{}}
	_, err := host.api().Create(transport.OfficialScheduleCreateRequest{
		Title: "建不出来", Prompt: "p",
		Timing: transport.OfficialScheduleTiming{Kind: "every", EverySeconds: 300},
		Target: &target,
	})
	if err == nil {
		t.Fatal("建会话失败时 create 必须报错，不许留下一条指向不存在会话的任务")
	}
	ents, readErr := os.ReadDir(filepath.Join(home, "state", "schedules"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("读 schedules 目录: %v", readErr)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			raw, _ := os.ReadFile(filepath.Join(home, "state", "schedules", e.Name()))
			var doc struct {
				Schedules []json.RawMessage `json:"schedules"`
			}
			_ = json.Unmarshal(raw, &doc)
			if len(doc.Schedules) != 0 {
				t.Fatalf("失败的 create 落盘了 %d 条: %s", len(doc.Schedules), raw)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// A. 官方冲突语义：200 + union {id, code:'schedule_conflict'}
// ---------------------------------------------------------------------------

func TestOfficialScheduleHostEditConflictIsUnionResult(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stale := "2000-01-01T00:00:00Z"
	title := "改过"
	res, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch:             &transport.OfficialSchedulePatch{Title: &title},
		ExpectedUpdatedAt: &stale,
	})
	if err != nil {
		t.Fatalf("冲突不是错误（官方是 union 成员）: %v", err)
	}
	miss, ok := res.(transport.OfficialScheduleMiss)
	if !ok {
		t.Fatalf("冲突返回 %T，want OfficialScheduleMiss", res)
	}
	if miss.ID != entry.ID || miss.Code != "schedule_conflict" {
		t.Fatalf("冲突形状错: %#v", miss)
	}
	// 冲突不许改到任何字节。
	item, _ := readStoredItem(t, home, "s1", entry.ID)
	if item.Title != entry.Title {
		t.Fatalf("冲突也改了落盘 title: %q", item.Title)
	}
}

// HTTP 层：冲突必须是 200 + union，不能是 409 + 四字段错误。
func TestOfficialScheduleHostEditConflictOverHTTP(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	api := fx.host.api()
	req := httptest.NewRequest(http.MethodPost, "/api/schedule.create", bytes.NewBufferString(
		`{"request":{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":300},"target":{"kind":"session","sessionId":"s1"}}}`))
	rec := httptest.NewRecorder()
	transport.ServeOfficialSchedule(rec, req, "schedule.create", api)
	if rec.Code != http.StatusOK {
		t.Fatalf("create 状态 = %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create 响应不是 JSON: %q", rec.Body.String())
	}
	id, _ := created["result"].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("create 没给 id: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/schedule.edit", bytes.NewBufferString(
		`{"id":"`+id+`","action":"update","patch":{"title":"改"},"expectedUpdatedAt":"2000-01-01T00:00:00Z"}`))
	rec = httptest.NewRecorder()
	transport.ServeOfficialSchedule(rec, req, "schedule.edit", api)
	if rec.Code != http.StatusOK {
		t.Fatalf("冲突状态 = %d，want 200: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("冲突响应不是 JSON: %q", rec.Body.String())
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("冲突响应没有 result 对象（那是错误信封）: %s", rec.Body.String())
	}
	if _, has := out["error"]; has {
		t.Fatalf("冲突不许走四字段错误: %s", rec.Body.String())
	}
	if result["id"] != id || result["code"] != "schedule_conflict" {
		t.Fatalf("冲突 union 形状错: %#v", result)
	}
	if len(result) != 2 {
		t.Fatalf("冲突 union 只该有 id 与 code: %#v", result)
	}
}

// ---------------------------------------------------------------------------
// 跨包并发写：官方 edit 与 schedule.delete 必须共用同一把锁
// ---------------------------------------------------------------------------

func TestOfficialScheduleMutationsShareOneLock(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title := "并发"
	schedule.LockMutations()
	done := make(chan error, 1)
	go func() {
		_, err := fx.host.api().Edit(transport.OfficialScheduleEditRequest{
			ID: entry.ID, Action: "update",
			Patch: &transport.OfficialSchedulePatch{Title: &title},
		})
		done <- err
	}()
	select {
	case err := <-done:
		schedule.UnlockMutations()
		t.Fatalf("官方 edit 没等共享锁就写完了（err=%v）——它与 schedule.delete 互相看不见", err)
	case <-time.After(120 * time.Millisecond):
	}
	schedule.UnlockMutations()
	if err := <-done; err != nil {
		t.Fatalf("放锁之后 Edit: %v", err)
	}
	if item, _ := readStoredItem(t, home, "s1", entry.ID); item.Title != "并发" {
		t.Fatalf("放锁后没写进去: %#v", item)
	}
}

// edit 换 target=new_once 时若建会话失败，整条 patch 必须一个字节都不动。
func TestOfficialScheduleHostEditRetargetFailureChangesNothing(t *testing.T) {
	home := scheduleHome(t)
	fx := newScheduleHostFixture(t, home)
	entry, err := fx.host.api().Create(everyCreateRequest("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := os.ReadFile(scheduleSessionFile(home, "s1"))
	if err != nil {
		t.Fatalf("读落盘: %v", err)
	}

	host := newOfficialScheduleHost(officialScheduleDeps{
		home:   home,
		submit: fx.submit.submit,
		now:    func() time.Time { return fx.now },
		createSession: func(cwd, preset string) (string, error) {
			return "", errors.New("会话控制器拒绝建会话")
		},
	})
	title := "改到一半"
	_, err = host.api().Edit(transport.OfficialScheduleEditRequest{
		ID: entry.ID, Action: "update",
		Patch: &transport.OfficialSchedulePatch{
			Title:  &title,
			Target: &transport.OfficialScheduleTarget{Kind: "new_once"},
		},
	})
	if err == nil {
		t.Fatal("建会话失败时 edit 必须报错")
	}
	after, readErr := os.ReadFile(scheduleSessionFile(home, "s1"))
	if readErr != nil {
		t.Fatalf("读落盘: %v", readErr)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("失败的 retarget 改了落盘字节:\nbefore=%s\nafter=%s", before, after)
	}
}
