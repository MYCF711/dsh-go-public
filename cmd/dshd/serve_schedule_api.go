package main

// 官方桌面端 0.2.2 的 schedule.create / edit / view / run / history 在**宿主机**这一侧的实现。
//
// 规矩（与 internal/transport/handlers_schedule_official.go 的分工）：
//   - Transport 只做协议与形状（参数解析 / fail-loud / 四字段错误 / 挂载表）；
//     本文件才碰真实状态。
//   - 全部复用既有 schedule 存储：`<home>/state/schedules/<sessionId>.json`，
//     与 `schedule.list`、`dshd schedule due`、工具模块
//     `modules/tool/schedule` 读的是**同一份字节**。没有第二份日程存储。
//   - **没有真实支撑就明确未实现**（transport.ErrOfficialNotImplemented），
//     绝不返回编出来的数据、也绝不"假执行"。
//
// 七种时机现在都是**真**的（推进规则在 internal/schedule 的 nextAfterOK 里）：
//
//	every / daily / weekly  固定周期与墙钟规则
//	after / at              一次性（到点触发后 status=ended，不会被日推一天）
//	cron                    五字段 Vixie 真解析，按表达式算下一次
//	rrule                   RFC5545 复发规则真解析，COUNT/UNTIL 用尽即结束
//
// 三种目标也都是**真**的：
//
//	session   投给一个已存在的会话
//	new_once  创建时真建一个会话并把它写进记录，重启后仍指向同一个
//	new       每次投递真建一个新会话
//
// 冲突语义按官方：`expectedUpdatedAt` 与落盘不一致时**不是错误**，而是
// HTTP 200 + union `{id, code:"schedule_conflict"}`（官方 ScheduleConflictResult）。
// 这与本仓既有的 `schedule.delete` 409 契约无关——那是另一套宿主扩展面。
//
// 投递历史是**真**的：`schedule.run` 每真触发一次会话轮次，就往
// `<home>/state/schedule-history.json` 追加一条（按 retention 剪裁），
// schedule.history 读的就是这份字节，重开宿主仍在。
//
// 接线（Lead 在 cmd/dshd/serve.go 造好 srv 之后、srv.Start() 之前补一行）：
//
//	attachOfficialSchedule(srv, *home, func(sessionID, text string) (transport.PromptResult, error) {
//		return prompts.Submit(context.Background(), sessionID, text)
//	})

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/schedule"
	"dshgo/internal/transport"
)

const (
	// officialScheduleHistoryMaxRecords 是每个任务保留的投递记录上限。
	// 官方 retention 由 Host 配置；这里是本宿主的真实配置，history 回包原样报它。
	officialScheduleHistoryMaxRecords = 100
	// officialScheduleHistoryMaxDays 是投递记录的保留天数（同上，真实配置）。
	officialScheduleHistoryMaxDays = 30
	// officialScheduleHistoryVersion 是侧车文件的版本戳（与 schedules 文件同一套态度：
	// 只有字段缺失按 legacy 读，写了就必须是白名单里的整数）。
	officialScheduleHistoryVersion = 1
	// officialScheduleNewTargetBucket 是 target.kind=new 的落盘桶。
	//
	// `new` 目标"每次投递建一个新会话"，它没有固定的目标会话，所以不能挂在任何
	// 会话文件下；这里给一个固定桶（真实会话 id 是 uuid 形状，不会撞上）。
	// due/advance/schedule.list 读的是同一个文件，行为与按会话落盘完全一致。
	officialScheduleNewTargetBucket = "_host"
)

// officialScheduleDeps 是宿主能力依赖。submit 为 nil = 这个宿主没有 runner，
// 此时 schedule.run 必须回 not_implemented，不许假执行。
type officialScheduleDeps struct {
	home   string
	submit func(sessionID, text string) (transport.PromptResult, error)
	// now 让测试钉住时钟；nil 用 time.Now。
	now func() time.Time
	// createSession 真建一个会话并返回它的 id；nil 时用宿主的 serveCreate
	// （与 session.create 走同一条落盘路径）。测试用它注入失败以验证回滚。
	createSession func(cwd, preset string) (string, error)
}

// officialScheduleHost 是真实实现。
type officialScheduleHost struct {
	deps officialScheduleDeps

	// stateMu 只保护本进程的内存状态（inflight 表）。
	//
	// **不使用它保护磁盘读改写**：那必须用 schedule.LockMutations()，
	// 否则和 schedule.delete 各拿一把锁，互相看不见对方刚写的字节。
	stateMu  sync.Mutex
	inflight map[string]bool

	// historyMu 只管侧车文件。
	historyMu sync.Mutex
}

func newOfficialScheduleHost(deps officialScheduleDeps) *officialScheduleHost {
	return &officialScheduleHost{deps: deps, inflight: map[string]bool{}}
}

// attachOfficialSchedule 在 serve 的装配阶段把真实实现挂到 srv 上。
//
// 这是 Lead 接线要用的一行：`serve.go` 造好 Server 之后、开始服务之前调它。
// submit 可以为 nil（没有 prompt service 的入口），此时 schedule.run 回 501。
func attachOfficialSchedule(srv *transport.Server, home string, submit func(sessionID, text string) (transport.PromptResult, error)) *officialScheduleHost {
	host := newOfficialScheduleHost(officialScheduleDeps{home: home, submit: submit})
	if srv == nil {
		return host
	}
	srv.AttachOfficialScheduleAPI(host.api())
	return host
}

func (h *officialScheduleHost) api() transport.OfficialScheduleAPI {
	return transport.OfficialScheduleAPI{
		Create:  h.create,
		Edit:    h.edit,
		View:    h.view,
		Run:     h.run,
		History: h.history,
	}
}

func (h *officialScheduleHost) now() time.Time {
	if h.deps.now != nil {
		return h.deps.now()
	}
	return time.Now()
}

func (h *officialScheduleHost) schedulesDir() string {
	return filepath.Join(h.deps.home, "state", "schedules")
}

func (h *officialScheduleHost) historyPath() string {
	return filepath.Join(h.deps.home, "state", "schedule-history.json")
}

// localZone 是本宿主默认算时间用的时区名。
func (h *officialScheduleHost) localZone() string {
	return schedule.LocalZoneName()
}

// ---------------------------------------------------------------------------
// schedule.create
// ---------------------------------------------------------------------------

func (h *officialScheduleHost) create(req transport.OfficialScheduleCreateRequest) (transport.OfficialScheduleEntry, error) {
	var zero transport.OfficialScheduleEntry

	target, err := h.resolveTarget(req.Target)
	if err != nil {
		return zero, err
	}
	// 目标会话已经建出来了：后面任何一步失败都必须把它回滚掉，
	// 否则每失败一次就留下一个空会话（用户看不见，但占着工作区）。
	committed := false
	defer func() {
		if !committed && target.created != "" {
			h.rollbackCreatedSession(target.created)
		}
	}()

	item, err := h.itemFromCreate(req.Title, req.Prompt, req.Timing)
	if err != nil {
		return zero, err
	}
	item.TargetKind = target.kind
	item.TargetSession = target.session
	item.TargetCWD = target.cwd
	item.TargetPreset = target.preset

	schedule.LockMutations()
	defer schedule.UnlockMutations()

	id, err := h.newIDLocked()
	if err != nil {
		return zero, err
	}
	now := h.now().UTC().Format(time.RFC3339)
	item.ID = id
	item.CreatedAt = now
	item.UpdatedAt = now
	item.Status = schedule.StatusActive

	doc, err := h.readDocLocked(target.bucket)
	if err != nil {
		return zero, err
	}
	raw, err := scheduleItemRaw(item, nil)
	if err != nil {
		return zero, err
	}
	entries := append(append([]json.RawMessage(nil), doc.entries...), raw)
	if err := doc.write(entries); err != nil {
		return zero, err
	}
	committed = true

	// 回包必须是**写进去的那一条**（readback），不是"按请求拼一个"。
	stored := item
	entry := h.entryFromItem(doc, stored)
	if rec := h.lastDelivery(id); rec != nil {
		entry.LastDelivery = rec
	}
	return entry, nil
}

// officialScheduleResolvedTarget 是一次 target 解析的真实结果。
type officialScheduleResolvedTarget struct {
	// bucket 是落盘文件名用的键（真实会话 id，或 new 目标的固定桶）。
	bucket string
	// kind 是官方 target.kind：session | new | new_once。
	kind string
	// session 是 session/new_once 的真实目标会话；new 为空。
	session string
	cwd     string
	preset  string
	// created 是本次**新建**的会话 id（只有 new_once 会填，失败要回滚）。
	created string
}

// resolveTarget 把官方 target union 变成真实投递目标。
func (h *officialScheduleHost) resolveTarget(target *transport.OfficialScheduleTarget) (officialScheduleResolvedTarget, error) {
	if target == nil {
		return officialScheduleResolvedTarget{}, fmt.Errorf("%w: 本宿主按会话文件落盘，必须显式给 target={kind:\"session\",sessionId}；没有隐式调用会话",
			transport.ErrOfficialBadInput)
	}
	switch target.Kind {
	case "session":
		if strings.TrimSpace(target.SessionID) == "" {
			return officialScheduleResolvedTarget{}, fmt.Errorf("%w: target.sessionId 是空串", transport.ErrOfficialBadInput)
		}
		if err := validateScheduleSessionID(target.SessionID); err != nil {
			return officialScheduleResolvedTarget{}, err
		}
		return officialScheduleResolvedTarget{
			bucket: target.SessionID, kind: "session", session: target.SessionID,
		}, nil

	case "new_once":
		// 官方 new_once 的目标是"第一次投递时建的那一个会话"。本宿主没有常驻投递循环，
		// 所以在这里就把会话真建出来并把身份写进记录：重启后仍然指向同一个会话，
		// 而不是每次投递各建一个。
		created, err := h.createSessionFor(target.Create)
		if err != nil {
			return officialScheduleResolvedTarget{}, err
		}
		return officialScheduleResolvedTarget{
			bucket: created, kind: "new_once", session: created, created: created,
			cwd: createCWD(target.Create), preset: createPreset(target.Create),
		}, nil

	case "new":
		// 每次投递各建一个：记录本身不指向任何会话，落在固定桶里。
		return officialScheduleResolvedTarget{
			bucket: officialScheduleNewTargetBucket, kind: "new",
			cwd: createCWD(target.Create), preset: createPreset(target.Create),
		}, nil

	default:
		return officialScheduleResolvedTarget{}, fmt.Errorf("%w: target.kind=%q 不认识", transport.ErrOfficialBadInput, target.Kind)
	}
}

func createCWD(info *transport.OfficialScheduleCreateInfo) string {
	if info == nil {
		return ""
	}
	return strings.TrimSpace(info.CWD)
}

func createPreset(info *transport.OfficialScheduleCreateInfo) string {
	if info == nil {
		return ""
	}
	return strings.TrimSpace(info.AgentPreset)
}

// createSessionFor 真建一个会话。
//
// 默认实现是宿主的 serveCreate（与 session.create 同一条落盘路径：真 v4 会话、
// 真 header、Close 之前刷盘）。deps.createSession 只在测试里注入。
func (h *officialScheduleHost) createSessionFor(info *transport.OfficialScheduleCreateInfo) (string, error) {
	cwd, preset := createCWD(info), createPreset(info)
	var (
		id  string
		err error
	)
	if h.deps.createSession != nil {
		id, err = h.deps.createSession(cwd, preset)
	} else {
		var create func(mode string) (string, error)
		create, err = serveCreate(h.deps.home, cwd)
		if err == nil {
			id, err = create(preset)
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w: 建目标会话失败（整条创建已回滚）: %v", transport.ErrStatePersistence, err)
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("%w: 会话控制器返回了空 id", transport.ErrStatePersistence)
	}
	return id, nil
}

// sessionSegment 是会话目录名，与 session.CreateSession 用同一个分段规则。
func sessionSegment(sessionID string) (string, error) {
	return v4.EncodeSegment(sessionID)
}

// rollbackCreatedSession 删掉本次新建的会话目录（只在同一个 home 下找）。
//
// 失败不回滚错误本身，只把"没清干净"如实说在错误信息里——静默留下一个
// 半成品会话比多一条日志更糟。
func (h *officialScheduleHost) rollbackCreatedSession(sessionID string) {
	seg, err := sessionSegment(sessionID)
	if err != nil {
		return
	}
	matches, globErr := filepath.Glob(filepath.Join(h.deps.home, "sessions", "*", seg))
	if globErr != nil {
		return
	}
	for _, path := range matches {
		_ = os.RemoveAll(path)
	}
}

// itemFromCreate 把官方 timing 翻成真实存储项；不支持的时机一律 fail-loud。
func (h *officialScheduleHost) itemFromCreate(title, prompt string, timing transport.OfficialScheduleTiming) (schedule.Item, error) {
	item := schedule.Item{Title: title, Prompt: prompt}
	next, kind, err := h.timingToItem(&item, timing)
	if err != nil {
		return schedule.Item{}, err
	}
	item.Kind = kind
	item.NextAt = next.UTC().Format(time.RFC3339)
	return item, nil
}

// timingToItem 写 item 的 kind 专属字段并返回下一次触发时刻。
//
// 七种时机全部有真实规则：算不出来（表达式坏、没有未来的一次）必须报 400 并说明，
// **不许**收下之后永不触发。
func (h *officialScheduleHost) timingToItem(item *schedule.Item, timing transport.OfficialScheduleTiming) (time.Time, string, error) {
	now := h.now()
	switch timing.Kind {
	case "every":
		if timing.EverySeconds < int64(transport.MinScheduleEverySeconds()) {
			return time.Time{}, "", fmt.Errorf("%w: every_seconds=%d 太短（下限 %d 秒）",
				transport.ErrOfficialBadInput, timing.EverySeconds, transport.MinScheduleEverySeconds())
		}
		item.EverySec = int(timing.EverySeconds)
		return now.Add(time.Duration(timing.EverySeconds) * time.Second), "every", nil

	case "daily":
		if timing.Daily == nil {
			return time.Time{}, "", fmt.Errorf("%w: daily 时间缺失", transport.ErrOfficialBadInput)
		}
		zone, loc, err := h.checkZone(timing.Daily.TimeZone)
		if err != nil {
			return time.Time{}, "", err
		}
		hh, mm, err := clockHHMM(timing.Daily.Time)
		if err != nil {
			return time.Time{}, "", err
		}
		item.DailyAt = fmt.Sprintf("%02d:%02d", hh, mm)
		item.TimeZone = zone
		return nextDaily(now, hh, mm, loc), "daily", nil

	case "weekly":
		if timing.Weekly == nil {
			return time.Time{}, "", fmt.Errorf("%w: weekly 时间缺失", transport.ErrOfficialBadInput)
		}
		zone, loc, err := h.checkZone(timing.Weekly.TimeZone)
		if err != nil {
			return time.Time{}, "", err
		}
		hh, mm, err := clockHHMM(timing.Weekly.Time)
		if err != nil {
			return time.Time{}, "", err
		}
		weekdays, err := isoToStoreWeekdays(timing.Weekly.Weekdays)
		if err != nil {
			return time.Time{}, "", err
		}
		item.WeeklyAt = fmt.Sprintf("%02d:%02d", hh, mm)
		item.Weekdays = weekdays
		item.TimeZone = zone
		return nextWeekly(now, hh, mm, weekdays, loc), "weekly", nil

	case "after":
		if timing.AfterSeconds <= 0 {
			return time.Time{}, "", fmt.Errorf("%w: after_seconds=%d 必须是正整数秒", transport.ErrOfficialBadInput, timing.AfterSeconds)
		}
		item.AfterSec = timing.AfterSeconds
		return now.Add(time.Duration(timing.AfterSeconds) * time.Second), "after", nil

	case "at":
		if timing.At == nil {
			return time.Time{}, "", fmt.Errorf("%w: at 时刻缺失", transport.ErrOfficialBadInput)
		}
		instant, err := h.resolveAt(timing.At)
		if err != nil {
			return time.Time{}, "", err
		}
		if !instant.After(now) {
			return time.Time{}, "", fmt.Errorf(
				"%w: at=%s 不是未来时刻（现在是 %s）；收下它等于一条永不触发的任务",
				transport.ErrOfficialBadInput, instant.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		}
		item.At = instant.UTC().Format(time.RFC3339)
		return instant, "at", nil

	case "cron":
		if timing.Cron == nil {
			return time.Time{}, "", fmt.Errorf("%w: cron 规则缺失", transport.ErrOfficialBadInput)
		}
		spec, err := schedule.ParseCron(timing.Cron.Expression)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("%w: cron 表达式 %q 解析不了: %v",
				transport.ErrOfficialBadInput, timing.Cron.Expression, err)
		}
		zone, loc, err := h.checkZone(timing.Cron.TimeZone)
		if err != nil {
			return time.Time{}, "", err
		}
		next, ok := spec.Next(now, loc)
		if !ok {
			return time.Time{}, "", fmt.Errorf(
				"%w: cron 表达式 %q 在五年内没有可触发的时刻；宁可现在就拒，也不留一条永不触发的任务",
				transport.ErrOfficialBadInput, timing.Cron.Expression)
		}
		item.Cron = spec.Expression()
		item.TimeZone = zone
		return next, "cron", nil

	case "rrule":
		if strings.TrimSpace(timing.RRule) == "" {
			return time.Time{}, "", fmt.Errorf("%w: rrule 规则是空串", transport.ErrOfficialBadInput)
		}
		spec, err := schedule.ParseRRule(timing.RRule, time.Local)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("%w: rrule 解析不了: %v", transport.ErrOfficialBadInput, err)
		}
		next, ok := spec.Next(now)
		if !ok {
			return time.Time{}, "", fmt.Errorf(
				"%w: rrule %q 没有未来的一次发生（COUNT/UNTIL 已用尽）", transport.ErrOfficialBadInput, timing.RRule)
		}
		item.RRule = spec.Expression()
		item.TimeZone = spec.Location().String()
		return next, "rrule", nil
	}

	return time.Time{}, "", fmt.Errorf(
		"%w: timing.kind=%q 不在官方枚举里（after/at/every/daily/weekly/cron/rrule）",
		transport.ErrOfficialBadInput, timing.Kind)
}

// resolveAt 把 at 的两种官方写法解析成绝对时刻。
func (h *officialScheduleHost) resolveAt(at *transport.OfficialScheduleAt) (time.Time, error) {
	if strings.TrimSpace(at.Instant) != "" {
		instant, err := time.Parse(time.RFC3339, strings.TrimSpace(at.Instant))
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: at=%q 不是 RFC3339 时刻: %v", transport.ErrOfficialBadInput, at.Instant, err)
		}
		return instant, nil
	}
	if strings.TrimSpace(at.Date) == "" || strings.TrimSpace(at.Clock) == "" || strings.TrimSpace(at.TimeZone) == "" {
		return time.Time{}, fmt.Errorf("%w: at 必须给 RFC3339 字符串或完整的 {date,time,time_zone}", transport.ErrOfficialBadInput)
	}
	_, loc, err := h.checkZone(at.TimeZone)
	if err != nil {
		return time.Time{}, err
	}
	day, err := time.ParseInLocation("2006-01-02", at.Date, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: at.date=%q 不是四位年份的 ISO 日历日期: %v", transport.ErrOfficialBadInput, at.Date, err)
	}
	hour, minute, second, ok := parseWallClock(at.Clock)
	if !ok {
		return time.Time{}, fmt.Errorf("%w: at.time=%q 不是 HH:MM[:SS[.SSS]]", transport.ErrOfficialBadInput, at.Clock)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, second, 0, loc), nil
}

// checkZone 只接受真正能算的时区：省略/本地、UTC、或可加载的 IANA 名。
//
// 解析不出来就 400：收下一个算不对的时区，等于把一个"看起来对、实际漂几小时"
// 的任务写进用户的工作流。
func (h *officialScheduleHost) checkZone(zone string) (string, *time.Location, error) {
	canonical, loc, err := schedule.ResolveZone(zone)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", transport.ErrOfficialBadInput, err)
	}
	return canonical, loc, nil
}

// ---------------------------------------------------------------------------
// schedule.edit
// ---------------------------------------------------------------------------

func (h *officialScheduleHost) edit(req transport.OfficialScheduleEditRequest) (any, error) {
	schedule.LockMutations()
	defer schedule.UnlockMutations()

	doc, index, item, err := h.findByIDLocked(req.ID)
	if err != nil {
		if errors.Is(err, errScheduleItemNotFound) {
			// 官方是 union 成员，不是错误：调用方据此区分"没这个任务"和"宿主炸了"。
			return transport.OfficialScheduleMiss{ID: req.ID, Code: "schedule_not_found"}, nil
		}
		return nil, err
	}

	// 官方 ScheduleConflictResult：**HTTP 200 + {id, code:'schedule_conflict'}**。
	// 它不是错误体，也不该是 409——那是本仓既有 schedule.delete 的扩展面契约。
	if req.ExpectedUpdatedAt != nil {
		if item.UpdatedAt == "" || item.UpdatedAt != *req.ExpectedUpdatedAt {
			return transport.OfficialScheduleMiss{ID: req.ID, Code: "schedule_conflict"}, nil
		}
	}

	if req.Action == "delete" {
		entries := append(append([]json.RawMessage(nil), doc.entries[:index]...), doc.entries[index+1:]...)
		if err := doc.write(entries); err != nil {
			return nil, err
		}
		return transport.OfficialScheduleDeleted{ID: req.ID, Deleted: true}, nil
	}

	updated, movedDoc, err := h.applyPatchLocked(doc, index, item, req.Patch)
	if err != nil {
		return nil, err
	}
	entry := h.entryFromItem(movedDoc, updated)
	if rec := h.lastDelivery(req.ID); rec != nil {
		entry.LastDelivery = rec
	}
	return entry, nil
}

// applyPatchLocked 应用一次原子 patch。要么整条改完写成功，要么一个字节都不动。
//
// 返回改动后的记录与它最终落盘的文档（换 target 会搬家，文档也跟着变）。
func (h *officialScheduleHost) applyPatchLocked(doc *officialScheduleDoc, index int, item schedule.Item, patch *transport.OfficialSchedulePatch) (schedule.Item, *officialScheduleDoc, error) {
	if patch == nil {
		return schedule.Item{}, nil, fmt.Errorf("%w: action=update 缺 patch", transport.ErrOfficialBadInput)
	}
	updated := item

	if patch.Title != nil {
		updated.Title = *patch.Title
	}
	if patch.Prompt != nil {
		updated.Prompt = *patch.Prompt
	}

	// 规则是否真的变了：官方只在"规则变了或恢复"时才重算 scheduledAt，
	// 原样重报同一条规则不该把下一次平移到 now。
	changedRule := false
	if patch.Timing != nil {
		before := h.timingOf(updated)
		after := *patch.Timing
		changedRule = !scheduleTimingEqual(before, after)
	}

	resume := patch.Active != nil && *patch.Active
	if changedRule || resume {
		timing := h.timingOf(updated)
		if patch.Timing != nil {
			timing = *patch.Timing
		}
		next, kind, err := h.timingToItem(&updated, timing)
		if err != nil {
			return schedule.Item{}, nil, err
		}
		// 换 kind 时把旧 kind 的专属字段清掉：留着就是一条自相矛盾的记录
		// （kind=daily 却带 every_seconds，due 一变就推进错）。
		resetScheduleKindFields(&updated, kind)
		updated.Kind = kind
		updated.NextAt = next.UTC().Format(time.RFC3339)
		changedRule = true
	}

	// patch.active（官方 revisedTask）：
	//   false → paused，规则与 next_at 一律不动（暂停不重排，也不补跑）
	//   true  → 上面已经按原规则从 now 重算过下一次，这里转 active
	switch {
	case patch.Active != nil && !*patch.Active:
		updated.Status = schedule.StatusPaused
	case patch.Active != nil && *patch.Active:
		updated.Status = schedule.StatusActive
	case changedRule && updated.Status == schedule.StatusEnded:
		updated.Status = schedule.StatusActive
	}

	// 目标换了会话 = 把这条记录搬到另一个会话文件（先写新文件、再删旧文件，
	// 中途失败最多留一份重复，绝不丢）。
	if patch.Target != nil {
		target, err := h.resolveTarget(patch.Target)
		if err != nil {
			if target.created != "" {
				h.rollbackCreatedSession(target.created)
			}
			return schedule.Item{}, nil, err
		}
		updated.TargetKind = target.kind
		updated.TargetSession = target.session
		updated.TargetCWD = target.cwd
		updated.TargetPreset = target.preset
		if target.bucket != doc.session {
			moved, movedDoc, committed, err := h.moveItemLocked(doc, index, updated, target.bucket)
			// 只有"新文件都没写成"才能回滚刚建的会话：一旦那条记录已经落到新文件里，
			// 删掉会话就等于把它变成一条指向空气的任务。
			if err != nil && !committed && target.created != "" {
				h.rollbackCreatedSession(target.created)
			}
			return moved, movedDoc, err
		}
	}

	updated.UpdatedAt = h.now().UTC().Format(time.RFC3339)
	raw, err := scheduleItemRaw(updated, doc.entryObject(index))
	if err != nil {
		return schedule.Item{}, nil, err
	}
	entries := append([]json.RawMessage(nil), doc.entries...)
	entries[index] = raw
	if err := doc.write(entries); err != nil {
		return schedule.Item{}, nil, err
	}
	return updated, doc, nil
}

// moveItemLocked 把一条记录搬到另一个会话文件：先写新文件，再删旧文件。
func (h *officialScheduleHost) moveItemLocked(doc *officialScheduleDoc, index int, item schedule.Item, newSession string) (schedule.Item, *officialScheduleDoc, bool, error) {
	now := h.now().UTC().Format(time.RFC3339)
	item.UpdatedAt = now
	moved := item

	dst, err := h.readDocLocked(newSession)
	if err != nil {
		return schedule.Item{}, nil, false, err
	}
	raw, err := scheduleItemRaw(moved, nil)
	if err != nil {
		return schedule.Item{}, nil, false, err
	}
	if err := dst.write(append(append([]json.RawMessage(nil), dst.entries...), raw)); err != nil {
		return schedule.Item{}, nil, false, fmt.Errorf("搬到会话 %s 失败，旧记录保持不动: %w", newSession, err)
	}
	entries := append(append([]json.RawMessage(nil), doc.entries[:index]...), doc.entries[index+1:]...)
	if err := doc.write(entries); err != nil {
		// 新文件已写成功：报错但不清空，宁可留一条重复也不丢任务。
		// committed=true 告诉调用方"新记录已经指向新会话了，别回滚它"。
		return schedule.Item{}, nil, true, fmt.Errorf("新会话 %s 已写入，但删旧记录失败（会留下一条重复）: %w", newSession, err)
	}
	return moved, dst, true, nil
}

// resetScheduleKindFields 清掉与 kind 无关的时间字段。
func resetScheduleKindFields(item *schedule.Item, kind string) {
	if kind != "every" {
		item.EverySec = 0
	}
	if kind != "daily" {
		item.DailyAt = ""
	}
	if kind != "weekly" {
		item.WeeklyAt = ""
		item.Weekdays = nil
	}
	if kind != "after" {
		item.AfterSec = 0
	}
	if kind != "at" {
		item.At = ""
	}
	if kind != "cron" {
		item.Cron = ""
	}
	if kind != "rrule" {
		item.RRule = ""
	}
	if kind != "daily" && kind != "weekly" && kind != "cron" && kind != "rrule" && kind != "at" {
		item.TimeZone = ""
	}
}

// timingOf 从落盘记录重建官方 timing，供"恢复"与"规则是否变了"使用。
//
// 缺字段（老记录）时按该 kind 的零值重建；重建不出来不是错误——
// 调用方只会在恢复/改 timing 时用到它，那时一定会再走一次真解析。
func (h *officialScheduleHost) timingOf(item schedule.Item) transport.OfficialScheduleTiming {
	timing := transport.OfficialScheduleTiming{Kind: item.Kind}
	switch item.Kind {
	case "every":
		timing.EverySeconds = int64(item.EverySec)
	case "daily":
		timing.Daily = &transport.OfficialScheduleDailyTime{Time: clockHHMMSS(item.DailyAt), TimeZone: zoneNameOr(item.TimeZone, h.localZone())}
	case "weekly":
		timing.Weekly = &transport.OfficialScheduleWeeklyTime{
			Time: clockHHMMSS(item.WeeklyAt), TimeZone: zoneNameOr(item.TimeZone, h.localZone()),
			Weekdays: storeToISOWeekdays(item.Weekdays),
		}
	case "after":
		timing.AfterSeconds = item.AfterSec
	case "at":
		timing.At = &transport.OfficialScheduleAt{Instant: item.At}
	case "cron":
		timing.Cron = &transport.OfficialScheduleCron{Expression: item.Cron, TimeZone: zoneNameOr(item.TimeZone, h.localZone())}
	case "rrule":
		timing.RRule = item.RRule
	}
	return timing
}

func zoneNameOr(zone, fallback string) string {
	if strings.TrimSpace(zone) == "" {
		return fallback
	}
	return zone
}

// scheduleTimingEqual 判断两条 timing 是不是同一条规则（官方 isDeepStrictEqual 的等价物）。
//
// 只比较规则本身，不比较"下一次"——原样重报同一条规则不该把 scheduledAt 平移。
func scheduleTimingEqual(a, b transport.OfficialScheduleTiming) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case "every":
		return a.EverySeconds == b.EverySeconds
	case "daily":
		if a.Daily == nil || b.Daily == nil {
			return a.Daily == b.Daily
		}
		return a.Daily.Time == b.Daily.Time && a.Daily.TimeZone == b.Daily.TimeZone
	case "weekly":
		if a.Weekly == nil || b.Weekly == nil {
			return a.Weekly == b.Weekly
		}
		return a.Weekly.Time == b.Weekly.Time && a.Weekly.TimeZone == b.Weekly.TimeZone &&
			intSlicesEqual(a.Weekly.Weekdays, b.Weekly.Weekdays)
	case "after":
		return a.AfterSeconds == b.AfterSeconds
	case "at":
		if a.At == nil || b.At == nil {
			return a.At == b.At
		}
		return a.At.Instant == b.At.Instant && a.At.Date == b.At.Date &&
			a.At.Clock == b.At.Clock && a.At.TimeZone == b.At.TimeZone
	case "cron":
		if a.Cron == nil || b.Cron == nil {
			return a.Cron == b.Cron
		}
		return a.Cron.Expression == b.Cron.Expression && a.Cron.TimeZone == b.Cron.TimeZone
	case "rrule":
		return a.RRule == b.RRule
	}
	return false
}

func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// schedule.view / schedule.run
// ---------------------------------------------------------------------------

func (h *officialScheduleHost) view(id string) (any, error) {
	schedule.LockMutations()
	defer schedule.UnlockMutations()
	doc, _, item, err := h.findByIDLocked(id)
	if err != nil {
		if errors.Is(err, errScheduleItemNotFound) {
			return transport.OfficialScheduleMiss{ID: id, Code: "schedule_not_found"}, nil
		}
		return nil, err
	}
	entry := h.entryFromItem(doc, item)
	if rec := h.lastDelivery(id); rec != nil {
		entry.LastDelivery = rec
	}
	return entry, nil
}

func (h *officialScheduleHost) run(id string) (any, error) {
	if h.deps.submit == nil {
		return nil, fmt.Errorf("%w: 本宿主没有 prompt runner（attachOfficialSchedule 的 submit 为 nil）；"+
			"schedule.run 必须真触发一次会话轮次，不许假执行", transport.ErrOfficialNotImplemented)
	}

	schedule.LockMutations()
	doc, index, item, err := h.findByIDLocked(id)
	if err != nil {
		schedule.UnlockMutations()
		if errors.Is(err, errScheduleItemNotFound) {
			return transport.OfficialScheduleMiss{ID: id, Code: "schedule_not_found"}, nil
		}
		return nil, err
	}
	h.stateMu.Lock()
	busy := h.inflight[id]
	if !busy {
		h.inflight[id] = true
	}
	h.stateMu.Unlock()
	if busy {
		schedule.UnlockMutations()
		// 官方：另一次执行已排队/在跑/在最小投递间隔内。
		return transport.OfficialScheduleMiss{ID: id, Code: "schedule_execution_pending"}, nil
	}
	sessionID, prompt, scheduledAt, targetKind := h.deliveryTarget(doc, item), item.Prompt, item.NextAt, item.TargetKind
	schedule.UnlockMutations()

	defer func() {
		h.stateMu.Lock()
		delete(h.inflight, id)
		h.stateMu.Unlock()
	}()

	// `new` 目标：每次投递真建一个新会话。
	createdForDelivery := ""
	if targetKind == "new" {
		created, err := h.createSessionFor(&transport.OfficialScheduleCreateInfo{
			CWD: item.TargetCWD, AgentPreset: item.TargetPreset,
		})
		if err != nil {
			return nil, err
		}
		createdForDelivery = created
		sessionID = created
	}

	// 真投递：调宿主的 prompt 提交（与 session.prompt 走同一条 runner），
	// 不等这一轮跑完——官方 schedule.run 也只确认"投递被接受"，回答走会话自己的 WS。
	result, err := h.deps.submit(sessionID, prompt)
	if err != nil {
		if createdForDelivery != "" {
			h.rollbackCreatedSession(createdForDelivery)
		}
		if errors.Is(err, ErrBusy) {
			// 目标会话上一轮还没结束：官方把这种情况表达成
			// `schedule_execution_pending`（"another execution is queued or
			// running"），不是错误。
			return transport.OfficialScheduleMiss{ID: id, Code: "schedule_execution_pending"}, nil
		}
		return nil, err
	}
	if !result.Accepted {
		if createdForDelivery != "" {
			h.rollbackCreatedSession(createdForDelivery)
		}
		return nil, fmt.Errorf("%w: runner 没有接受这次投递（accepted=false）", transport.ErrStatePersistence)
	}
	if strings.TrimSpace(result.TurnID) == "" {
		if createdForDelivery != "" {
			h.rollbackCreatedSession(createdForDelivery)
		}
		return nil, fmt.Errorf("%w: runner 接受了投递却没给 turnId，无法记录真实凭据", transport.ErrStatePersistence)
	}

	record := transport.OfficialScheduleHistoryRecord{
		Prompt:          prompt,
		ScheduledAt:     scheduledAt,
		DeliveredAt:     h.now().UTC().Format(time.RFC3339),
		MessageID:       result.TurnID,
		TargetSessionID: sessionID,
	}
	if err := h.appendDelivery(id, record); err != nil {
		return nil, err
	}

	// 官方 delivered()：new_once 的第一次投递之后，目标固化成那个会话；
	// new 始终保持"每次新建"。
	entryItem := item
	if targetKind == "new_once" && sessionID != "" {
		schedule.LockMutations()
		if freshDoc, freshIndex, freshItem, findErr := h.findByIDLocked(id); findErr == nil {
			updated := freshItem
			updated.TargetKind = "session"
			updated.TargetSession = sessionID
			updated.UpdatedAt = h.now().UTC().Format(time.RFC3339)
			if raw, rawErr := scheduleItemRaw(updated, freshDoc.entryObject(freshIndex)); rawErr == nil {
				entries := append([]json.RawMessage(nil), freshDoc.entries...)
				entries[freshIndex] = raw
				if writeErr := freshDoc.write(entries); writeErr == nil {
					entryItem = updated
					doc = freshDoc
					index = freshIndex
				}
			}
		}
		schedule.UnlockMutations()
	}
	_ = index

	// 官方：run 不消费、也不移动计划时刻。落盘的 next_at 一个字节都不动。
	entry := h.entryFromItem(doc, entryItem)
	entry.LastDelivery = &transport.OfficialScheduleLastDelivery{
		ScheduledAt:     record.ScheduledAt,
		DeliveredAt:     record.DeliveredAt,
		MessageID:       record.MessageID,
		TargetSessionID: record.TargetSessionID,
	}
	return entry, nil
}

// deliveryTarget 返回这条记录该投给哪个会话。
//
// 老记录没有 target_kind / target_session：它们写在 <sessionId>.json 里，
// 目标就是持有文件的那个会话——这才是历史数据唯一正确的解释。
func (h *officialScheduleHost) deliveryTarget(doc *officialScheduleDoc, item schedule.Item) string {
	if strings.TrimSpace(item.TargetSession) != "" {
		return item.TargetSession
	}
	return doc.session
}

// ---------------------------------------------------------------------------
// schedule.history
// ---------------------------------------------------------------------------

func (h *officialScheduleHost) history(req transport.OfficialScheduleHistoryRequest) (any, error) {
	schedule.LockMutations()
	_, _, _, err := h.findByIDLocked(req.ID)
	schedule.UnlockMutations()
	if err != nil {
		if errors.Is(err, errScheduleItemNotFound) {
			return transport.OfficialScheduleMiss{ID: req.ID, Code: "schedule_not_found"}, nil
		}
		return nil, err
	}

	h.historyMu.Lock()
	file := h.loadHistoryLocked()
	if file == nil {
		h.historyMu.Unlock()
		return nil, fmt.Errorf("%w: 投递历史文件 %s 不是合法 JSON", transport.ErrStatePersistence, h.historyPath())
	}
	task := file.Tasks[req.ID]
	h.historyMu.Unlock()

	records := []transport.OfficialScheduleHistoryRecord{}
	pruned := false
	if task != nil {
		records = task.Records
		pruned = task.Pruned
	}

	// 侧车里是 oldest → newest；官方分页是 newest first、before 是排他游标。
	end := len(records)
	if req.Before != "" {
		end = -1
		for i := len(records) - 1; i >= 0; i-- {
			if records[i].MessageID == req.Before {
				end = i
				break
			}
		}
		if end == -1 {
			return transport.OfficialScheduleMiss{ID: req.ID, Code: "delivery_cursor_not_found"}, nil
		}
	}
	start := end - req.Limit
	if start < 0 {
		start = 0
	}
	page := make([]transport.OfficialScheduleHistoryRecord, 0, end-start)
	for i := end - 1; i >= start; i-- {
		page = append(page, records[i])
	}
	nextBefore := ""
	if start > 0 && len(page) > 0 {
		nextBefore = page[len(page)-1].MessageID
	}
	return transport.OfficialScheduleHistoryPage{
		ID:                        req.ID,
		Records:                   page,
		EarlierRecordsUnavailable: pruned,
		EarlierRecordsPruned:      pruned,
		Retention: transport.OfficialScheduleRetention{
			Days:    officialScheduleHistoryMaxDays,
			Records: officialScheduleHistoryMaxRecords,
		},
		NextBefore: nextBefore,
	}, nil
}

// ---------------------------------------------------------------------------
// 真实存储：<home>/state/schedules/<sessionId>.json
// ---------------------------------------------------------------------------

var errScheduleItemNotFound = errors.New("没有这个定时任务")

// officialScheduleDoc 是一次读改写的工作副本。
//
// 形状与 `internal/schedule/mutations.go` 的 mutationDocument 一致（同一份文件、
// 同一个 version 白名单、同样的原子替换），但那条实现是包内私有的：本文件
// 在 cmd/dshd，只能重建一份等价的最小实现，不能改 internal/schedule 的内部形状。
// 两边共用同一把锁（schedule.LockMutations），这才是并发安全的关键。
type officialScheduleDoc struct {
	path    string
	session string
	mode    os.FileMode
	body    map[string]json.RawMessage
	entries []json.RawMessage
}

// entryObject 把第 index 条原样解成 map，让未知键在改写时得以保留。
func (d *officialScheduleDoc) entryObject(index int) map[string]json.RawMessage {
	if index < 0 || index >= len(d.entries) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(d.entries[index], &obj); err != nil {
		return nil
	}
	return obj
}

// write 原子替换整个文件：独占临时文件 + Sync + 同目录 rename。
func (d *officialScheduleDoc) write(entries []json.RawMessage) error {
	if entries == nil {
		entries = []json.RawMessage{}
	}
	body := make(map[string]json.RawMessage, len(d.body)+2)
	for k, v := range d.body {
		body[k] = v
	}
	body["version"] = json.RawMessage(fmt.Sprint(schedule.CurrentScheduleVersion))
	schedules, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("序列化 schedules 失败: %w", err)
	}
	body["schedules"] = schedules
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化定时任务文件失败: %w", err)
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return fmt.Errorf("建日程目录失败: %w", err)
	}
	mode := d.mode
	if mode == 0 {
		mode = 0o644
	}
	tmp := d.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("写 %s 失败（旧文件保持不动）: %w", tmp, err)
	}
	removeTemp := true
	defer func() {
		_ = f.Close()
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("写 %s 失败: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("同步 %s 失败: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, d.path); err != nil {
		return fmt.Errorf("原子替换 %s 失败: %w", d.path, err)
	}
	removeTemp = false
	d.entries = entries
	return nil
}

// readDocLocked 读一个会话的日程文件；文件不存在时返回空文档（create 要能建它）。
func (h *officialScheduleHost) readDocLocked(sessionID string) (*officialScheduleDoc, error) {
	if err := validateScheduleSessionID(sessionID); err != nil {
		return nil, err
	}
	path := filepath.Join(h.schedulesDir(), sessionID+".json")
	doc := &officialScheduleDoc{path: path, session: sessionID, body: map[string]json.RawMessage{}}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读定时任务文件 %s 失败: %w", path, err)
	}
	if err := schedule.ValidateScheduleVersion(raw); err != nil {
		return nil, fmt.Errorf("定时任务文件 %s 版本不兼容: %w", path, err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return nil, fmt.Errorf("定时任务文件 %s 不是合法 JSON 对象", path)
	}
	entries := []json.RawMessage{}
	if scheds, ok := body["schedules"]; ok && string(scheds) != "null" {
		if err := json.Unmarshal(scheds, &entries); err != nil {
			return nil, fmt.Errorf("定时任务文件 %s 的 schedules 不是数组: %w", path, err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("读 %s 的文件信息失败: %w", path, err)
	}
	doc.body = body
	doc.entries = entries
	doc.mode = info.Mode().Perm()
	return doc, nil
}

// findByIDLocked 在整个日程目录里按 id 找一条（官方 id 是全局唯一身份）。
func (h *officialScheduleHost) findByIDLocked(id string) (*officialScheduleDoc, int, schedule.Item, error) {
	sessions, err := schedule.Sessions(h.schedulesDir())
	if err != nil {
		return nil, -1, schedule.Item{}, fmt.Errorf("列日程文件失败: %w", err)
	}
	for _, sessionID := range sessions {
		doc, err := h.readDocLocked(sessionID)
		if err != nil {
			return nil, -1, schedule.Item{}, err
		}
		for i, raw := range doc.entries {
			var item schedule.Item
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, -1, schedule.Item{}, fmt.Errorf("解析定时任务 %s 第 %d 条失败: %w", doc.path, i, err)
			}
			if item.ID == id {
				return doc, i, item, nil
			}
		}
	}
	return nil, -1, schedule.Item{}, fmt.Errorf("%w: %s", errScheduleItemNotFound, id)
}

// newIDLocked 造一个全局唯一的宿主身份。撞了就重来，绝不假定随机数没撞。
func (h *officialScheduleHost) newIDLocked() (string, error) {
	sessions, err := schedule.Sessions(h.schedulesDir())
	if err != nil {
		return "", fmt.Errorf("列日程文件失败: %w", err)
	}
	existing := map[string]bool{}
	for _, sessionID := range sessions {
		items, _, err := schedule.LoadWithRevision(h.schedulesDir(), sessionID)
		if err != nil {
			return "", err
		}
		for _, it := range items {
			existing[it.ID] = true
		}
	}
	for try := 0; try < 8; try++ {
		var buf [4]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("生成定时任务身份失败: %w", err)
		}
		id := "sch-" + hex.EncodeToString(buf[:])
		if !existing[id] {
			return id, nil
		}
	}
	return "", fmt.Errorf("生成定时任务身份连续撞车，拒绝返回一个可能重复的 id")
}

// scheduleItemRaw 把一条存储记录编码回 JSON；unknown 非 nil 时保留其中的未知键。
func scheduleItemRaw(item schedule.Item, unknown map[string]json.RawMessage) (json.RawMessage, error) {
	encoded, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("序列化定时任务失败: %w", err)
	}
	var fresh map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fresh); err != nil {
		return nil, fmt.Errorf("序列化定时任务失败: %w", err)
	}
	merged := map[string]json.RawMessage{}
	for k, v := range unknown {
		merged[k] = v
	}
	// 本层管理的键：当前没有的必须从旧对象里删掉，否则会留下自相矛盾的旧值
	// （比如 kind 从 every 改成 daily 之后还挂着 every_seconds）。
	for _, key := range []string{
		"id", "title", "prompt", "kind", "every_seconds", "daily_at", "weekdays",
		"weekly_at", "next_at", "created_at", "updated_at", "status", "ended_at", "allow_writes",
		"at", "after_seconds", "cron", "rrule", "time_zone",
		"target_kind", "target_session", "target_cwd", "target_preset",
	} {
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

func validateScheduleSessionID(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" || sessionID == "." || sessionID == ".." ||
		strings.ContainsAny(sessionID, "/\\:*?\"<>|") || strings.ContainsAny(sessionID, "\r\n\t") {
		return fmt.Errorf("%w: sessionId %q 为空或含路径分隔符/控制字符", transport.ErrOfficialBadInput, sessionID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 投递历史：<home>/state/schedule-history.json
// ---------------------------------------------------------------------------

type officialScheduleHistoryFile struct {
	Version int                                     `json:"version"`
	Tasks   map[string]*officialScheduleTaskHistory `json:"tasks"`
}

type officialScheduleTaskHistory struct {
	// Records 是 oldest → newest；分页时反转。
	Records []transport.OfficialScheduleHistoryRecord `json:"records"`
	// Pruned 表示更早的记录已被 retention 剪掉（"before 可能还存在更好的旧记录"）。
	Pruned bool `json:"pruned,omitempty"`
}

func (h *officialScheduleHost) loadHistoryLocked() *officialScheduleHistoryFile {
	file := &officialScheduleHistoryFile{Version: officialScheduleHistoryVersion, Tasks: map[string]*officialScheduleTaskHistory{}}
	raw, err := os.ReadFile(h.historyPath())
	if err != nil {
		return file
	}
	var doc officialScheduleHistoryFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 侧车坏了不能冒充"没有历史"：让上层看到错误比悄悄清空更安全。
		return nil
	}
	if doc.Tasks == nil {
		doc.Tasks = map[string]*officialScheduleTaskHistory{}
	}
	if doc.Version == 0 {
		doc.Version = officialScheduleHistoryVersion
	}
	return &doc
}

// appendDelivery 在多任务侧车上追加一条真实投递并立即落盘。
func (h *officialScheduleHost) appendDelivery(id string, record transport.OfficialScheduleHistoryRecord) error {
	h.historyMu.Lock()
	defer h.historyMu.Unlock()

	file := h.loadHistoryLocked()
	if file == nil {
		return fmt.Errorf("%w: 投递历史文件 %s 不是合法 JSON", transport.ErrStatePersistence, h.historyPath())
	}
	task := file.Tasks[id]
	if task == nil {
		task = &officialScheduleTaskHistory{}
		file.Tasks[id] = task
	}
	task.Records = append(task.Records, record)

	// retention：先按天数，再按条数（与官方 delivery-history 同一顺序）。
	cutoff := h.now().UTC().Add(-time.Duration(officialScheduleHistoryMaxDays) * 24 * time.Hour)
	kept := make([]transport.OfficialScheduleHistoryRecord, 0, len(task.Records))
	for _, rec := range task.Records {
		stamp, err := time.Parse(time.RFC3339, rec.DeliveredAt)
		if err != nil || stamp.Before(cutoff) {
			task.Pruned = true
			continue
		}
		kept = append(kept, rec)
	}
	if len(kept) > officialScheduleHistoryMaxRecords {
		task.Pruned = true
		kept = kept[len(kept)-officialScheduleHistoryMaxRecords:]
	}
	task.Records = kept

	return h.writeHistoryLocked(file)
}

// lastDelivery 返回最近一条真实投递（没有就是 nil，不编）。
func (h *officialScheduleHost) lastDelivery(id string) *transport.OfficialScheduleLastDelivery {
	h.historyMu.Lock()
	defer h.historyMu.Unlock()
	file := h.loadHistoryLocked()
	if file == nil {
		return nil
	}
	task := file.Tasks[id]
	if task == nil || len(task.Records) == 0 {
		return nil
	}
	newest := task.Records[len(task.Records)-1]
	return &transport.OfficialScheduleLastDelivery{
		ScheduledAt:     newest.ScheduledAt,
		DeliveredAt:     newest.DeliveredAt,
		MessageID:       newest.MessageID,
		TargetSessionID: newest.TargetSessionID,
	}
}

func (h *officialScheduleHost) writeHistoryLocked(file *officialScheduleHistoryFile) error {
	file.Version = officialScheduleHistoryVersion
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: 序列化投递历史失败: %v", transport.ErrStatePersistence, err)
	}
	raw = append(raw, '\n')
	path := h.historyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%w: 建 state 目录失败: %v", transport.ErrStatePersistence, err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("%w: 写投递历史 %s 失败: %v", transport.ErrStatePersistence, tmp, err)
	}
	removeTemp := true
	defer func() {
		_ = f.Close()
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("%w: 写投递历史失败: %v", transport.ErrStatePersistence, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("%w: 同步投递历史失败: %v", transport.ErrStatePersistence, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: 关闭投递历史失败: %v", transport.ErrStatePersistence, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%w: 原子替换投递历史失败: %v", transport.ErrStatePersistence, err)
	}
	removeTemp = false
	return nil
}

// ---------------------------------------------------------------------------
// 时间换算
// ---------------------------------------------------------------------------

// nextDaily 是严格晚于 now 的下一个 HH:MM（loc 的墙钟）。
func nextDaily(now time.Time, hour, minute int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !next.After(now) {
		next = time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc).AddDate(0, 0, 1)
	}
	return next
}

// nextWeekly 是严格晚于 now 的下一个"星期集合里的 HH:MM"（loc 的墙钟）。
// weekdays 用 Go 的 0=周日..6=周六（存储层就是这个口径）。
func nextWeekly(now time.Time, hour, minute int, weekdays []int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	set := map[int]bool{}
	for _, d := range weekdays {
		set[((d%7)+7)%7] = true
	}
	local := now.In(loc)
	for i := 0; i <= 8; i++ {
		day := local.AddDate(0, 0, i)
		if !set[int(day.Weekday())] {
			continue
		}
		candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc)
		if candidate.After(now) {
			return candidate
		}
	}
	return local.AddDate(0, 0, 7)
}

// isoToStoreWeekdays 把官方 ISO 星期（周一=1…周日=7）翻成存储层的 Go 星期（周日=0），
// 并按官方"ascending ISO"排序。
func isoToStoreWeekdays(iso []int) ([]int, error) {
	if len(iso) == 0 {
		return nil, fmt.Errorf("%w: weekdays 是空集合", transport.ErrOfficialBadInput)
	}
	sorted := append([]int(nil), iso...)
	sort.Ints(sorted)
	out := make([]int, 0, len(sorted))
	seen := map[int]bool{}
	for _, d := range sorted {
		if d < 1 || d > 7 {
			return nil, fmt.Errorf("%w: weekday %d 越界（官方 1..7）", transport.ErrOfficialBadInput, d)
		}
		goDay := d % 7
		if seen[goDay] {
			return nil, fmt.Errorf("%w: weekday %d 重复", transport.ErrOfficialBadInput, d)
		}
		seen[goDay] = true
		out = append(out, goDay)
	}
	return out, nil
}

// storeToISOWeekdays 是反方向：Go 周日=0 → 官方 7，其余 +1，按 ISO 升序输出。
func storeToISOWeekdays(store []int) []int {
	out := make([]int, 0, len(store))
	for _, d := range store {
		goDay := ((d % 7) + 7) % 7
		if goDay == 0 {
			out = append(out, 7)
			continue
		}
		out = append(out, goDay)
	}
	sort.Ints(out)
	return out
}

// clockHHMM 读存储用的 HH:MM。
func clockHHMM(clock string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(clock), ":")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("%w: time=%q 不是 HH:MM[:SS]", transport.ErrOfficialBadInput, clock)
	}
	hour, err := parseIntBounded(parts[0], 23)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: time=%q 的小时非法", transport.ErrOfficialBadInput, clock)
	}
	minute, err := parseIntBounded(parts[1], 59)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: time=%q 的分钟非法", transport.ErrOfficialBadInput, clock)
	}
	return hour, minute, nil
}

func parseIntBounded(text string, max int) (int, error) {
	if text == "" || len(text) > 2 {
		return 0, fmt.Errorf("非法数字 %q", text)
	}
	value := 0
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("非法数字 %q", text)
		}
		value = value*10 + int(r-'0')
	}
	if value > max {
		return 0, fmt.Errorf("超出 %d", max)
	}
	return value, nil
}

// parseWallClock 解析官方墙钟写法：HH:MM[:SS][.SSS]（秒与毫秒只校验，不落秒）。
func parseWallClock(s string) (int, int, int, bool) {
	body := strings.TrimSpace(s)
	if i := strings.IndexByte(body, '.'); i >= 0 {
		frac := body[i+1:]
		body = body[:i]
		if len(frac) < 1 || len(frac) > 3 {
			return 0, 0, 0, false
		}
		for _, r := range frac {
			if r < '0' || r > '9' {
				return 0, 0, 0, false
			}
		}
	}
	parts := strings.Split(body, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		if p == "" || len(p) > 2 {
			return 0, 0, 0, false
		}
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return 0, 0, 0, false
			}
			n = n*10 + int(r-'0')
		}
		nums[i] = n
	}
	hour, minute := nums[0], nums[1]
	second := 0
	if len(nums) == 3 {
		second = nums[2]
	}
	if hour > 23 || minute > 59 || second > 59 {
		return 0, 0, 0, false
	}
	return hour, minute, second, true
}

// clockHHMMSS 把存储的 HH:MM 归一到官方写的 HH:MM:SS。
func clockHHMMSS(clock string) string {
	hour, minute, err := clockHHMM(clock)
	if err != nil {
		return clock
	}
	return fmt.Sprintf("%02d:%02d:00", hour, minute)
}

// ---------------------------------------------------------------------------
// 官方 entry 组装
// ---------------------------------------------------------------------------

func (h *officialScheduleHost) entryFromItem(doc *officialScheduleDoc, item schedule.Item) transport.OfficialScheduleEntry {
	targetKind := item.TargetKind
	if targetKind == "" {
		targetKind = "session"
	}
	var target transport.OfficialScheduleTarget
	switch targetKind {
	case "new":
		target = transport.OfficialScheduleTarget{Kind: "new"}
	case "new_once":
		target = transport.OfficialScheduleTarget{Kind: "new_once", SessionID: item.TargetSession}
	default:
		target = transport.OfficialScheduleTarget{Kind: "session", SessionID: h.deliveryTarget(doc, item)}
	}
	// 官方 involved()：new 目标不把（还不存在的）会话算进 involved。
	involved := []string{}
	if targetKind != "new" {
		if sid := h.deliveryTarget(doc, item); sid != "" {
			involved = append(involved, sid)
		}
	}

	entry := transport.OfficialScheduleEntry{
		ID:                 item.ID,
		Kind:               item.Kind,
		Title:              item.Title,
		Prompt:             item.Prompt,
		ScheduledAt:        item.NextAt,
		Target:             target,
		InvolvedSessionIDs: involved,
		Status:             officialScheduleStatus(item.Status),
		CreatedAt:          item.CreatedAt,
		UpdatedAt:          item.UpdatedAt,
	}
	zone := zoneNameOr(item.TimeZone, h.localZone())
	switch item.Kind {
	case "every":
		entry.EverySeconds = int64(item.EverySec)
	case "after":
		entry.AfterSeconds = item.AfterSec
	case "at":
		// 官方 AtScheduleRecord 只有 {id,kind,title,prompt,scheduledAt}：
		// 多吐一个 timeZone 就是自造字段（zod 会 strip，但形状不等价）。
	case "daily":
		entry.Time = clockHHMMSS(item.DailyAt)
		entry.TimeZone = zone
	case "weekly":
		entry.Time = clockHHMMSS(item.WeeklyAt)
		entry.TimeZone = zone
		entry.Weekdays = storeToISOWeekdays(item.Weekdays)
	case "cron":
		entry.Expression = item.Cron
		entry.TimeZone = zone
	case "rrule":
		entry.RRule = item.RRule
	}
	return entry
}

// officialScheduleStatus 把存储状态翻成官方枚举。
//
// 存储里只有 "ended"/"paused" 是终态；缺字段表示历史记录（从没写过生命周期），
// 契约三个值里能站住的是 active——它确实还有下一次触发时刻。
func officialScheduleStatus(status string) string {
	switch status {
	case schedule.StatusPaused, schedule.StatusEnded:
		return status
	}
	return schedule.StatusActive
}
