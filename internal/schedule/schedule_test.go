package schedule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// every：从登记的时间一格一格往前推，跨过 now；不许原地不动（会立刻再触发一次）。
func TestNextAfterEvery(t *testing.T) {
	it := Item{Kind: "every", EverySec: 300, NextAt: "2026-11-03T05:44:17+08:00"}
	got := nextAfter(it, at("2026-11-03T05:44:17+08:00"))
	if !got.After(at("2026-11-03T05:44:17+08:00")) {
		t.Fatalf("下一次必须严格晚于 now，得到 %v", got)
	}
	if want := at("2026-11-03T05:49:17+08:00"); !got.Equal(want) {
		t.Fatalf("下一次 = %v，期望 %v", got, want)
	}
}

// every 落后好几格时，要一次推到 now 之后，而不是只加一格。
func TestNextAfterEveryCatchesUp(t *testing.T) {
	it := Item{Kind: "every", EverySec: 60, NextAt: "2026-11-03T05:00:00+08:00"}
	got := nextAfter(it, at("2026-11-03T05:10:30+08:00"))
	if want := at("2026-11-03T05:11:00+08:00"); !got.Equal(want) {
		t.Fatalf("赶上后 = %v，期望 %v", got, want)
	}
}

// daily：当天时刻已过就明天，没过就今天。
func TestNextAfterDaily(t *testing.T) {
	it := Item{Kind: "daily", DailyAt: "23:59"}
	if got, want := nextAfter(it, at("2026-11-03T05:00:00+08:00")), at("2026-11-03T23:59:00+08:00"); !got.Equal(want) {
		t.Fatalf("没过点 = %v，期望 %v", got, want)
	}
	if got, want := nextAfter(it, at("2026-11-04T00:30:00+08:00")), at("2026-11-04T23:59:00+08:00"); !got.Equal(want) {
		t.Fatalf("过了点 = %v，期望 %v", got, want)
	}
}

// weekly：只在指定星期触发；当天时刻没过也算。
func TestNextAfterWeekly(t *testing.T) {
	it := Item{Kind: "weekly", WeeklyAt: "08:00", Weekdays: []int{1, 3}} // 周一、周三
	// 2026-11-03 是周二 → 下一个是周三 08:00
	got := nextAfter(it, at("2026-11-03T05:00:00+08:00"))
	if want := at("2026-11-04T08:00:00+08:00"); !got.Equal(want) {
		t.Fatalf("周二之后 = %v，期望 %v", got, want)
	}
}

// 坏输入不许 panic，也不许返回零值时间（那会让任务疯狂触发）。
func TestBadInputsDoNotPanic(t *testing.T) {
	for _, it := range []Item{
		{Kind: "every", EverySec: 0, NextAt: "不是时间"},
		{Kind: "daily", DailyAt: "25:99"},
		{Kind: "weekly", WeeklyAt: "x", Weekdays: nil},
		{Kind: "unknown"},
	} {
		got := nextAfter(it, at("2026-11-03T05:00:00+08:00"))
		if !got.After(at("2026-11-03T05:00:00+08:00")) {
			t.Fatalf("%+v 的兜底也必须晚于 now，得到 %v", it, got)
		}
	}
}

// 到期判定 + 推进要能落盘，且只推进到期的那些。
func TestDueAtAndAdvance(t *testing.T) {
	dir := t.TempDir()
	doc := `{"schedules":[
	  {"id":"sch-1","kind":"every","every_seconds":300,"next_at":"2026-11-03T05:00:00+08:00"},
	  {"id":"sch-2","kind":"daily","daily_at":"23:59","next_at":"2026-11-03T23:59:00+08:00"}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "s1.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := Load(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	now := at("2026-11-03T05:10:00+08:00")
	dues := DueAt(items, now)
	if len(dues) != 1 || dues[0].Item.ID != "sch-1" {
		t.Fatalf("只有 sch-1 到期，得到 %+v", dues)
	}
	if n, err := Advance(dir, "s1", dues, now); err != nil || n != 1 {
		t.Fatalf("推进失败: n=%d err=%v", n, err)
	}
	back, _ := Load(dir, "s1")
	if back[0].NextAt != "2026-11-03T05:15:00+08:00" {
		t.Fatalf("sch-1 下次 = %q，期望 05:15", back[0].NextAt)
	}
	if back[1].NextAt != "2026-11-03T23:59:00+08:00" {
		t.Fatalf("没到期的不该被改：%q", back[1].NextAt)
	}
}

// 目录不存在时是"没有任务"，不是错误。
func TestLoadMissingDir(t *testing.T) {
	items, err := Load(filepath.Join(t.TempDir(), "nope"), "x")
	if err != nil || items != nil {
		t.Fatalf("应当返回空且无错: %v %v", items, err)
	}
}

// ---------------------------------------------------------------------------
// M401：是一次性 / 暂停 / 未知键保真
// ---------------------------------------------------------------------------

// 一次性（at/after）到点之后必须 ended，不许再排下一次。
func TestAdvanceEndsOneShot(t *testing.T) {
	dir := t.TempDir()
	doc := `{"version":1,"schedules":[
	  {"id":"sch-at","kind":"at","at":"2026-11-03T05:00:00Z","next_at":"2026-11-03T05:00:00Z","status":"active"}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "s1.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := Load(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	now := at("2026-11-03T05:00:30Z")
	dues := DueAt(items, now)
	if len(dues) != 1 || !dues[0].Ends {
		t.Fatalf("at 到点必须是一次性的结束格，得到 %+v", dues)
	}
	if n, err := Advance(dir, "s1", dues, now); err != nil || n != 1 {
		t.Fatalf("Advance: n=%d err=%v", n, err)
	}
	back, _ := Load(dir, "s1")
	if back[0].Status != StatusEnded {
		t.Fatalf("status = %q，want ended", back[0].Status)
	}
	if back[0].EndedAt != "2026-11-03T05:00:00Z" {
		t.Fatalf("ended_at = %q，want 该格计划时刻", back[0].EndedAt)
	}
	if again := DueAt(back, now.Add(72*time.Hour)); len(again) != 0 {
		t.Fatalf("ended 之后还在到期: %+v", again)
	}
}

// 暂停的记录：due 不认、Advance 也不推（调用方硬塞 due 进来也不行）。
func TestPausedNeverDueOrAdvanced(t *testing.T) {
	dir := t.TempDir()
	doc := `{"version":1,"schedules":[
	  {"id":"sch-p","kind":"every","every_seconds":60,"next_at":"2026-11-03T05:00:00Z","status":"paused"}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "s1.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	items, _ := Load(dir, "s1")
	now := at("2026-11-03T09:00:00Z")
	if dues := DueAt(items, now); len(dues) != 0 {
		t.Fatalf("暂停的任务不该 due: %+v", dues)
	}
	// 就算调用方把一条 due 硬塞进来（比如它读的是更早的快照），也不许推进。
	forced := []Due{{Item: items[0], Next: now.Add(time.Hour)}}
	if n, err := Advance(dir, "s1", forced, now); err != nil || n != 0 {
		t.Fatalf("暂停的任务被推进了: n=%d err=%v", n, err)
	}
	back, _ := Load(dir, "s1")
	if back[0].NextAt != "2026-11-03T05:00:00Z" || back[0].Status != StatusPaused {
		t.Fatalf("暂停期间文件被改了: %+v", back[0])
	}
}

// Advance 只改它负责的键：顶层未知键与条目里的未知键都要原样留着。
func TestAdvancePreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	doc := `{"version":1,"extra_top":{"keep":true},"schedules":[
	  {"id":"sch-u","kind":"every","every_seconds":300,"next_at":"2026-11-03T05:00:00Z","future_field":42}
	]}`
	path := filepath.Join(dir, "s1.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	items, _ := Load(dir, "s1")
	now := at("2026-11-03T05:10:00Z")
	dues := DueAt(items, now)
	if len(dues) != 1 {
		t.Fatalf("应当有 1 条 due，得到 %d", len(dues))
	}
	if n, err := Advance(dir, "s1", dues, now); err != nil || n != 1 {
		t.Fatalf("Advance: n=%d err=%v", n, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ExtraTop map[string]any               `json:"extra_top"`
		Sches    []map[string]json.RawMessage `json:"schedules"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("落盘不是合法 JSON: %v", err)
	}
	if got.ExtraTop["keep"] != true {
		t.Fatalf("顶层未知键被删了: %s", raw)
	}
	if len(got.Sches) != 1 {
		t.Fatalf("条目数 = %d", len(got.Sches))
	}
	if string(got.Sches[0]["future_field"]) != "42" {
		t.Fatalf("条目里的未知键被删了: %s", raw)
	}
	back, _ := Load(dir, "s1")
	if back[0].NextAt != "2026-11-03T05:15:00Z" {
		t.Fatalf("next_at = %q，want 05:15", back[0].NextAt)
	}
}
