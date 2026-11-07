// Package schedule 提供四个工具：schedule_create / schedule_list /
// schedule_update / schedule_delete。
//
// 本模块只做"登记与计算"：把任务写进 <dir>/<sessionId>.json，并算出 next_at。
// 到点唤醒由宿主以后做——这里**不起 goroutine、不起定时器**：
// 工具调用有生命周期，留下一个没人管的闹钟既回收不了，也无法跨进程存活。
package schedule

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dshgo/internal/kernel"
	schedulestore "dshgo/internal/schedule"
	"dshgo/internal/spi"
)

// 四个工具的 kind，同时也是模型看到的工具名。
const (
	kindCreate = "schedule_create"
	kindList   = "schedule_list"
	kindUpdate = "schedule_update"
	kindDelete = "schedule_delete"
)

// 三种调度方式。
const (
	kindEvery  = "every"
	kindDaily  = "daily"
	kindWeekly = "weekly"
)

const (
	// 当前值与到期推进层共用；兼容版本白名单也由 internal/schedule 单点校验。
	currentScheduleVersion = schedulestore.CurrentScheduleVersion

	maxTitleRunes = 120
	// 列表回给模型的上限：定时任务再多也不是"清单"而是噪声，截断后让人去删。
	maxListItems = 200
	// 间隔下限一分钟：更密的调度靠工具轮询实现不了，报错比假装支持更诚实。
	minEverySec = 60
	// 上限一年：再长就没有"定时"的意义了，也免得 time.Duration 溢出。
	maxEverySec = 366 * 24 * 3600
)

// Schedule 是登记下来的一条定时任务。字段顺序与落盘形状一致，方便人直接看文件。
type Schedule struct {
	ID     string `json:"id"`     // "sch-0001-3f9a"
	Title  string `json:"title"`  //
	Prompt string `json:"prompt"` //
	Kind   string `json:"kind"`   // every | daily | weekly

	EverySec int    `json:"every_seconds,omitempty"` //
	DailyAt  string `json:"daily_at,omitempty"`      // "09:30"
	Weekdays []int  `json:"weekdays,omitempty"`      // 0=周日 .. 6=周六
	WeeklyAt string `json:"weekly_at,omitempty"`     // "09:30"

	NextAt    string `json:"next_at"`    // RFC3339，算出来的
	CreatedAt string `json:"created_at"` //
	UpdatedAt string `json:"updated_at"` //

	// AllowWrites 决定这条任务到点时**能不能动写类工具**。
	// 默认 false = 只跑只读工具：提示词是模型写的、执行时人不在场，
	// 授权又是在登记时给的——不留这道门，模型就能安排"两小时后无条件跑 bash"。
	AllowWrites bool `json:"allow_writes,omitempty"`
}

// scheduleFile 是一个会话落盘的形状。
type scheduleFile struct {
	Version   int        `json:"version"`
	Schedules []Schedule `json:"schedules"`
}

// store 是四个工具共享的存储句柄：一个会话一个文件。
//
// mu 只串行化**本进程内**的读改写；跨进程安全靠"写 .tmp 再 rename"
// （rename 在同一文件系统上是原子的，读的人要么看到旧文件，要么看到新文件）。
type store struct {
	mu  sync.Mutex
	dir string
}

func (s *store) ready() error {
	if strings.TrimSpace(s.dir) == "" {
		return fmt.Errorf("定时任务没有配置数据目录")
	}
	return nil
}

func (s *store) path(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".json")
}

// display 只用于提示：对外一律正斜杠，Windows 的反斜杠不喂给模型。
func (s *store) display(sessionID string) string {
	return strings.ReplaceAll(s.path(sessionID), `\`, "/")
}

// load 读出一个会话的定时任务；只有文件不存在才当空表。
// 空白/损坏 JSON 与不兼容版本都必须报错，调用方因此不会在随后 save 时覆盖原文件。
// 调用方必须已持锁。
func (s *store) load(sessionID string) ([]Schedule, error) {
	buf, err := os.ReadFile(s.path(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读定时任务文件 %s 失败: %w", s.display(sessionID), err)
	}
	var f scheduleFile
	if err := json.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("定时任务文件 %s 不是合法 JSON: %w", s.display(sessionID), err)
	}
	if err := schedulestore.ValidateScheduleVersion(buf); err != nil {
		return nil, fmt.Errorf("定时任务文件 %s 版本不兼容: %w", s.display(sessionID), err)
	}
	return f.Schedules, nil
}

// save 先写 .tmp 再 rename：崩在中间时旧文件仍然完整，不会留下半截 JSON。
// 调用方必须已持锁。
func (s *store) save(sessionID string, items []Schedule) error {
	if items == nil {
		items = []Schedule{}
	}
	buf, err := json.MarshalIndent(scheduleFile{Version: currentScheduleVersion, Schedules: items}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化定时任务失败: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("建状态目录 %s 失败: %w", strings.ReplaceAll(s.dir, `\`, "/"), err)
	}
	tmp := s.path(sessionID) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("写临时文件 %s.tmp 失败: %w", s.display(sessionID), err)
	}
	if err := os.Rename(tmp, s.path(sessionID)); err != nil {
		os.Remove(tmp) // 留个 .tmp 会让人以为还有一次没跑完的写
		return fmt.Errorf("替换定时任务文件 %s 失败: %w", s.display(sessionID), err)
	}
	return nil
}

// ---------------------------------------------------------------- 参数与工具

type createArgs struct {
	Title    string `json:"title"`
	Prompt   string `json:"prompt"`
	Kind     string `json:"kind"`
	EverySec *int   `json:"every_seconds"`
	DailyAt  string `json:"daily_at"`
	WeeklyAt string `json:"weekly_at"`
	Weekdays []int  `json:"weekdays"`
	// AllowWrites 默认 false = 到点只跑只读工具。
	AllowWrites bool `json:"allow_writes"`
}

// 指针字段用来区分"没传"和"传了空值"——update 是增量语义，不能把没提的字段清掉。
type updateArgs struct {
	ID       string  `json:"id"`
	Title    *string `json:"title"`
	Prompt   *string `json:"prompt"`
	Kind     *string `json:"kind"`
	EverySec *int    `json:"every_seconds"`
	DailyAt  *string `json:"daily_at"`
	WeeklyAt *string `json:"weekly_at"`
	Weekdays *[]int  `json:"weekdays"`
}

type deleteArgs struct {
	ID string `json:"id"`
}

// Tool 是四个工具的共同外壳：kind 决定名字、参数与行为，store 决定存哪里。
// 四者必须看到同一份状态，所以共享一个句柄。
type Tool struct {
	st   *store
	kind string
}

func (t *Tool) Name() string { return t.kind }

// Caps 必填且如实：只有 create / update / delete 会写盘。
func (t *Tool) Caps() spi.Caps {
	if t.kind == kindList {
		return spi.Caps{}
	}
	return spi.Caps{Writes: true}
}

// Description 是"什么时候用它"。四条写在同一处，因为它们是同一份登记表的四个动作，
// 分开写最容易漂移成互相矛盾的措辞。
func (t *Tool) Description() string {
	switch t.kind {
	case kindCreate:
		return "登记定时任务（every/daily/weekly），算出下次触发时间。"
	case kindList:
		return "列出本会话的定时任务，按下次触发时间升序。"
	case kindUpdate:
		return "按 id 改定时任务；改时间字段会重算下次时间。"
	case kindDelete:
		return "按 id 删掉一条定时任务。"
	}
	return ""
}

func (t *Tool) Schema() json.RawMessage {
	switch t.kind {
	case kindCreate:
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "一句话名字，最多 120 字"},
    "prompt": {"type": "string", "description": "到点时要交给模型的话（祈使句）"},
    "kind": {"type": "string", "enum": ["every", "daily", "weekly"],
             "description": "调度方式：every=每 N 秒；daily=每天某时刻；weekly=每周若干天的某时刻"},
    "every_seconds": {"type": "integer", "description": "kind=every 时必填，>= 60"},
    "allow_writes": {"type": "boolean",
                     "description": "默认 false：到点只跑只读工具。设 true 才会在无人值守时执行写操作，用户要清楚后果"},
    "daily_at": {"type": "string", "description": "kind=daily 时必填，本地时区 HH:MM，如 09:30"},
    "weekly_at": {"type": "string", "description": "kind=weekly 时必填，本地时区 HH:MM，如 09:30"},
    "weekdays": {"type": "array", "items": {"type": "integer"}, "description": "kind=weekly 时必填，0=周日 .. 6=周六，可多选"}
  },
  "required": ["title", "prompt", "kind"],
  "additionalProperties": false
}`)
	case kindList:
		return json.RawMessage(`{"type": "object", "properties": {}, "additionalProperties": false}`)
	case kindUpdate:
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "要改的定时任务 id，形如 sch-0001-3f9a"},
    "title": {"type": "string", "description": "新名字，最多 120 字"},
    "prompt": {"type": "string", "description": "新提示词"},
    "kind": {"type": "string", "enum": ["every", "daily", "weekly"]},
    "every_seconds": {"type": "integer", "description": "kind=every 时间隔秒数，>= 60"},
    "daily_at": {"type": "string", "description": "kind=daily 时的 HH:MM，本地时区"},
    "weekly_at": {"type": "string", "description": "kind=weekly 时的 HH:MM，本地时区"},
    "weekdays": {"type": "array", "items": {"type": "integer"}, "description": "kind=weekly 时的星期集合，0=周日"}
  },

  "additionalProperties": false
}`)
	case kindDelete:
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "要删的定时任务 id"}
  },

  "additionalProperties": false
}`)
	}
	return json.RawMessage(`{"type": "object"}`)
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	switch t.kind {
	case kindCreate:
		return t.create(ctx, raw)
	case kindList:
		return t.list(ctx, raw)
	case kindUpdate:
		return t.update(ctx, raw)
	case kindDelete:
		return t.delete(ctx, raw)
	}
	// 走到这里说明接线把 kind 写错了，不是模型的锅，Hint 指向人。
	return spi.Result{Hint: "未知的工具分支，这是接线错误：别重复调用，告诉维护者检查 Module() 里的 kind"},
		fmt.Errorf("schedule: 未知 kind %q", t.kind)
}

// ---------------------------------------------------------------- create

func (t *Tool) create(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a createArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"title":"…","prompt":"…","kind":"every|daily|weekly"}，再按 kind 补 every_seconds / daily_at / weekly_at+weekdays`},
			fmt.Errorf("schedule_create: 解析参数: %w", err)
	}
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "定时任务没有数据目录：这是接线问题，报告监工（Module(dir) 要传 <home>/state/schedules）"},
			fmt.Errorf("schedule_create: %w", err)
	}
	sessionID, err := currentSession(ctx)
	if err != nil {
		return spi.Result{Hint: "这次调用没有会话上下文，就不知道该把任务记在谁名下：别重试，告诉维护者接线要带会话"},
			fmt.Errorf("schedule_create: %w", err)
	}

	title := strings.TrimSpace(a.Title)
	if title == "" {
		return spi.Result{Hint: `title 不能为空：一句话说明这个定时任务干什么，例如 "每天早报汇总"`},
			fmt.Errorf("schedule_create: title 为空")
	}
	if n := len([]rune(title)); n > maxTitleRunes {
		return spi.Result{Hint: fmt.Sprintf("title 有 %d 字，超过 %d 上界：压成一句话再登记", n, maxTitleRunes)},
			fmt.Errorf("schedule_create: title 过长")
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return spi.Result{Hint: `prompt 不能为空：写清到点时要模型做什么，例如 "汇总昨天的提交并给下一步建议"`},
			fmt.Errorf("schedule_create: prompt 为空")
	}

	s := Schedule{
		Title:       title,
		Prompt:      prompt,
		Kind:        strings.ToLower(strings.TrimSpace(a.Kind)),
		DailyAt:     a.DailyAt,
		WeeklyAt:    a.WeeklyAt,
		Weekdays:    a.Weekdays,
		AllowWrites: a.AllowWrites,
	}
	if a.EverySec != nil {
		s.EverySec = *a.EverySec
	}
	// 校验并规范化时间字段；hint 非空即表示哪一项要补/改。
	if hint := checkTiming(&s); hint != "" {
		return spi.Result{Hint: hint}, fmt.Errorf("schedule_create: 时间参数不合法: kind=%q", s.Kind)
	}
	trimToKind(&s)

	now := time.Now()
	next, ok := nextAt(&s, now)
	if !ok {
		return spi.Result{Hint: "算不出下次触发时间：检查 weekdays 是否为空（kind=weekly 至少要一天），或改用 kind=every"},
			fmt.Errorf("schedule_create: next_at 无法计算")
	}

	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	items, err := t.st.load(sessionID)
	if err != nil {
		return spi.Result{Hint: "定时任务文件读不出来或已损坏：先让人看一眼 " + t.st.display(sessionID) + "，别覆盖它"},
			fmt.Errorf("schedule_create: %w", err)
	}
	id, err := nextID(items)
	if err != nil {
		return spi.Result{Hint: "生成 id 失败（取不到随机数）：重试一次；还不行就报告维护者，别自己改文件"},
			fmt.Errorf("schedule_create: %w", err)
	}
	stamp := now.Format(time.RFC3339)
	s.ID = id
	s.CreatedAt = stamp
	s.UpdatedAt = stamp
	s.NextAt = next.Format(time.RFC3339)

	items = append(items, s)
	if err := t.st.save(sessionID, items); err != nil {
		return spi.Result{Hint: "写盘失败：确认 " + t.st.display(sessionID) + " 所在目录可写、磁盘没满；别重复创建，先解决权限"},
			fmt.Errorf("schedule_create: %w", err)
	}

	return spi.Result{
		Summary: fmt.Sprintf("已登记 %s · %s · 下次 %s", s.ID, describeTiming(s), s.NextAt),
		Data:    map[string]any{"schedule": s, "total": len(items)},
		Hint:    "宿主目前不做真实唤醒；到点执行要靠以后接上的调度器。随时可用 schedule_list 复核",
	}, nil
}

// ---------------------------------------------------------------- list

func (t *Tool) list(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "定时任务没有数据目录：这是接线问题，报告监工（Module(dir) 要传 <home>/state/schedules）"},
			fmt.Errorf("schedule_list: %w", err)
	}
	sessionID, err := currentSession(ctx)
	if err != nil {
		return spi.Result{Hint: "这次调用没有会话上下文，就不知道列谁的任务：别重试，告诉维护者接线要带会话"},
			fmt.Errorf("schedule_list: %w", err)
	}

	t.st.mu.Lock()
	items, err := t.st.load(sessionID)
	t.st.mu.Unlock()
	if err != nil {
		return spi.Result{Hint: "定时任务文件读不出来或已损坏：先让人看一眼 " + t.st.display(sessionID) + "，别覆盖它"},
			fmt.Errorf("schedule_list: %w", err)
	}
	if len(items) == 0 {
		return spi.Result{
			Summary: "还没有定时任务",
			Data:    map[string]any{"schedules": []Schedule{}, "total": 0},
			Hint:    "这个会话还没有定时任务；要建就用 schedule_create，kind 选 every / daily / weekly",
		}, nil
	}

	// 按 next_at 升序：模型关心的是"下一个到点的是谁"，不是登记顺序。
	sort.SliceStable(items, func(i, j int) bool {
		return parseStamp(items[i].NextAt).Before(parseStamp(items[j].NextAt))
	})

	hint := ""
	if len(items) > maxListItems {
		items = items[:maxListItems]
		hint = fmt.Sprintf("只回了最近到点的 %d 条（还有更多）；用 schedule_list 之前先删掉不要的，或按 id 精确处理", maxListItems)
	}
	if hint == "" {
		hint = "要看某条的完整提示词用 schedule_list 的输出；改字段用 schedule_update，删用 schedule_delete"
	}
	return spi.Result{
		Summary: fmt.Sprintf("%d 条定时任务，最近的是 %s（%s）", len(items), items[0].ID, items[0].NextAt),
		Data:    map[string]any{"schedules": items, "total": len(items)},
		Hint:    hint,
	}, nil
}

// ---------------------------------------------------------------- update

// pickID 处理"只有一个任务时不填 id"的情形。
// 有多条时必须让模型选——猜错就改/删了别人的任务。
func pickID(items []Schedule, id, who string) (string, error) {
	if id != "" {
		return id, nil
	}
	switch len(items) {
	case 0:
		return "", fmt.Errorf("还没有定时任务：先 schedule_create 建一个")
	case 1:
		return items[0].ID, nil
	default:
		ids := make([]string, 0, len(items))
		for _, it := range items {
			ids = append(ids, it.ID)
		}
		return "", fmt.Errorf("%s：这个会话有 %d 条定时任务（%s），id 必须填一个", who, len(items), strings.Join(ids, "、"))
	}
}

func (t *Tool) update(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a updateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：至少给 {"id":"sch-0001-3f9a"}，其余字段按需增量给`},
			fmt.Errorf("schedule_update: 解析参数: %w", err)
	}
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "定时任务没有数据目录：这是接线问题，报告监工（Module(dir) 要传 <home>/state/schedules）"},
			fmt.Errorf("schedule_update: %w", err)
	}
	sessionID, err := currentSession(ctx)
	if err != nil {
		return spi.Result{Hint: "这次调用没有会话上下文，就不知道改谁的任务：别重试，告诉维护者接线要带会话"},
			fmt.Errorf("schedule_update: %w", err)
	}
	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	items, err := t.st.load(sessionID)
	if err != nil {
		return spi.Result{Hint: "定时任务文件读不出来或已损坏：先让人看一眼 " + t.st.display(sessionID) + "，别覆盖它"},
			fmt.Errorf("schedule_update: %w", err)
	}
	// id 不填就用唯一的那条：一个会话通常只挂一两个定时任务，
	// 让模型抄一遍刚建出来的 id 只是多一次被拒的机会；有多条时必须自己选。
	id, err := pickID(items, strings.TrimSpace(a.ID), "schedule_update")
	if err != nil {
		return spi.Result{Hint: err.Error()}, err
	}
	idx := indexOf(items, id)
	if idx < 0 {
		return spi.Result{Hint: notFoundHint(items)},
			fmt.Errorf("schedule_update: 没有 id %q", id)
	}

	changed := make([]string, 0, 7)
	if a.Title != nil {
		title := strings.TrimSpace(*a.Title)
		if title == "" {
			return spi.Result{Hint: "title 不能改成空：不想改名就别传这个字段（这是增量更新）"},
				fmt.Errorf("schedule_update: title 为空")
		}
		if n := len([]rune(title)); n > maxTitleRunes {
			return spi.Result{Hint: fmt.Sprintf("title 有 %d 字，超过 %d 上界：压成一句话", n, maxTitleRunes)},
				fmt.Errorf("schedule_update: title 过长")
		}
		items[idx].Title = title
		changed = append(changed, "title")
	}
	if a.Prompt != nil {
		prompt := strings.TrimSpace(*a.Prompt)
		if prompt == "" {
			return spi.Result{Hint: "prompt 不能改成空：不想改就别传这个字段"},
				fmt.Errorf("schedule_update: prompt 为空")
		}
		items[idx].Prompt = prompt
		changed = append(changed, "prompt")
	}

	// 时间相关字段：只要动过任何一个，就必须重算 next_at。
	timing := false
	if a.Kind != nil {
		items[idx].Kind = strings.ToLower(strings.TrimSpace(*a.Kind))
		timing = true
		changed = append(changed, "kind")
	}
	if a.EverySec != nil {
		items[idx].EverySec = *a.EverySec
		timing = true
		changed = append(changed, "every_seconds")
	}
	if a.DailyAt != nil {
		items[idx].DailyAt = *a.DailyAt
		timing = true
		changed = append(changed, "daily_at")
	}
	if a.WeeklyAt != nil {
		items[idx].WeeklyAt = *a.WeeklyAt
		timing = true
		changed = append(changed, "weekly_at")
	}
	if a.Weekdays != nil {
		// 深拷贝：JSON 里的切片可能与别处共享，落盘前不该被外部再改。
		items[idx].Weekdays = append([]int(nil), (*a.Weekdays)...)
		timing = true
		changed = append(changed, "weekdays")
	}
	if len(changed) == 0 {
		return spi.Result{Hint: `什么都没改：至少传一个字段，例如 {"id":"…","title":"新名字"}`},
			fmt.Errorf("schedule_update: 没有可改的字段")
	}

	if hint := checkTiming(&items[idx]); hint != "" {
		return spi.Result{Hint: hint}, fmt.Errorf("schedule_update: 时间参数不合法: id=%q", id)
	}
	trimToKind(&items[idx])

	now := time.Now()
	if timing {
		next, ok := nextAt(&items[idx], now)
		if !ok {
			return spi.Result{Hint: "算不出下次触发时间：检查 weekdays 是否为空（kind=weekly 至少要一天），或改用 kind=every"},
				fmt.Errorf("schedule_update: next_at 无法计算: id=%q", id)
		}
		items[idx].NextAt = next.Format(time.RFC3339)
	}
	items[idx].UpdatedAt = now.Format(time.RFC3339)

	if err := t.st.save(sessionID, items); err != nil {
		return spi.Result{Hint: "写盘失败：确认 " + t.st.display(sessionID) + " 所在目录可写、磁盘没满；别重复调用，先解决权限"},
			fmt.Errorf("schedule_update: %w", err)
	}

	return spi.Result{
		Summary: fmt.Sprintf("已改 %s（%s）· 下次 %s", id, strings.Join(changed, ","), items[idx].NextAt),
		Data: map[string]any{
			"schedule":      items[idx],
			"changed":       changed,
			"next_at_reset": timing,
			"total":         len(items),
		},
	}, nil
}

// ---------------------------------------------------------------- delete

func (t *Tool) delete(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a deleteArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"id": "sch-0001-3f9a"}`},
			fmt.Errorf("schedule_delete: 解析参数: %w", err)
	}
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "定时任务没有数据目录：这是接线问题，报告监工（Module(dir) 要传 <home>/state/schedules）"},
			fmt.Errorf("schedule_delete: %w", err)
	}
	sessionID, err := currentSession(ctx)
	if err != nil {
		return spi.Result{Hint: "这次调用没有会话上下文，就不知道删谁的任务：别重试，告诉维护者接线要带会话"},
			fmt.Errorf("schedule_delete: %w", err)
	}

	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	items, err := t.st.load(sessionID)
	if err != nil {
		return spi.Result{Hint: "定时任务文件读不出来或已损坏：先让人看一眼 " + t.st.display(sessionID) + "，别覆盖它"},
			fmt.Errorf("schedule_delete: %w", err)
	}
	// 与 update 同一条规则：只有一条时可以省 id。
	id, err := pickID(items, strings.TrimSpace(a.ID), "schedule_delete")
	if err != nil {
		return spi.Result{Hint: err.Error()}, err
	}
	idx := indexOf(items, id)
	if idx < 0 {
		return spi.Result{Hint: notFoundHint(items)},
			fmt.Errorf("schedule_delete: 没有 id %q", id)
	}

	removed := items[idx]
	items = append(items[:idx], items[idx+1:]...)
	if err := t.st.save(sessionID, items); err != nil {
		return spi.Result{Hint: "写盘失败：确认 " + t.st.display(sessionID) + " 所在目录可写、磁盘没满；别重复调用，先解决权限"},
			fmt.Errorf("schedule_delete: %w", err)
	}

	return spi.Result{
		Summary: fmt.Sprintf("已删 %s（%s），还剩 %d 条", removed.ID, removed.Title, len(items)),
		Data:    map[string]any{"deleted": removed, "total": len(items)},
		Hint:    afterDeleteHint(items),
	}, nil
}

// ---------------------------------------------------------------- 校验与计算

// checkTiming 校验并规范化 s 的时间参数；返回空串表示合格，
// 非空串是给模型看的下一步建议（不合法时用它当 Result.Hint）。
// 只检查与 s.Kind 对应的字段——别的字段留着也无害，落盘前由 trimToKind 清掉。
func checkTiming(s *Schedule) string {
	switch s.Kind {
	case kindEvery:
		if s.EverySec < minEverySec {
			return fmt.Sprintf("kind=every 要 every_seconds 且 >= %d（秒）；现在缺或太小，补上再调", minEverySec)
		}
		if s.EverySec > maxEverySec {
			return fmt.Sprintf("every_seconds 最大 %d（一年）；更长周期的需求该拆成多次登记", maxEverySec)
		}
	case kindDaily:
		norm, ok := normalizeHHMM(s.DailyAt)
		if !ok {
			return `kind=daily 要 daily_at，格式 "HH:MM"（本地时区），例如 "09:30"`
		}
		s.DailyAt = norm
	case kindWeekly:
		norm, ok := normalizeHHMM(s.WeeklyAt)
		if !ok {
			return `kind=weekly 要 weekly_at，格式 "HH:MM"（本地时区），例如 "09:30"`
		}
		s.WeeklyAt = norm
		days, hint := normalizeWeekdays(s.Weekdays)
		if hint != "" {
			return hint
		}
		s.Weekdays = days
	case "":
		return `kind 必填，只能是 every / daily / weekly 三者之一`
	default:
		return fmt.Sprintf(`kind=%q 不认识：只能是 every（每 N 秒）/ daily（每天某时刻）/ weekly（每周某几天）`, s.Kind)
	}
	return ""
}

// normalizeWeekdays 去重、排序并挡住越界值——0=周日 .. 6=周六。
func normalizeWeekdays(in []int) ([]int, string) {
	if len(in) == 0 {
		return nil, "kind=weekly 要 weekdays 且至少一天：0=周日 .. 6=周六，例如 [1,3,5]"
	}
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, d := range in {
		if d < 0 || d > 6 {
			return nil, fmt.Sprintf("weekdays 里有 %d：只能是 0..6（0=周日，1=周一 … 6=周六）", d)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Ints(out)
	return out, ""
}

// trimToKind 清掉与 kind 无关的时间字段：文件里留着旧字段，
// 下次切回那个 kind 时会凭空冒出一个过期值。
func trimToKind(s *Schedule) {
	if s.Kind != kindEvery {
		s.EverySec = 0
	}
	if s.Kind != kindDaily {
		s.DailyAt = ""
	}
	if s.Kind != kindWeekly {
		s.WeeklyAt = ""
		s.Weekdays = nil
	}
}

// nextAt 算下一次触发时刻，全部用本地时区（time.Local）——用户嘴里的"九点半"
// 就是他墙上的九点半，不换算成 UTC。
func nextAt(s *Schedule, now time.Time) (time.Time, bool) {
	switch s.Kind {
	case kindEvery:
		return now.Add(time.Duration(s.EverySec) * time.Second), true

	case kindDaily:
		h, m := splitHHMM(s.DailyAt)
		cand := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.Local)
		if !cand.After(now) {
			// 今天这个点已经过了（或正好是现在）→ 明天；用 AddDate 而不是 +24h，
			// 夏令时切换那天 +24h 会偏一小时。
			cand = cand.AddDate(0, 0, 1)
		}
		return cand, true

	case kindWeekly:
		h, m := splitHHMM(s.WeeklyAt)
		// 最多看 8 天：weekdays 非空，7 天内必有一天命中；多一天兜住"今天已过"。
		for i := 0; i < 8; i++ {
			d := now.AddDate(0, 0, i)
			if !containsInt(s.Weekdays, int(d.Weekday())) {
				continue
			}
			cand := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, time.Local)
			if cand.After(now) {
				return cand, true
			}
		}
	}
	return time.Time{}, false
}

// normalizeHHMM 把 "9:30" / "09:30" 都收敛成 "09:30"；范围越界也判不合格。
func normalizeHHMM(s string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return "", false
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return "", false
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return "", false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", h, m), true
}

// splitHHMM 只接受已经 normalizeHHMM 过的值，所以忽略解析错误。
func splitHHMM(s string) (int, int) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h, m
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// parseStamp 解析 RFC3339；坏值排到最后而不是让排序 panic。
func parseStamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// describeTiming 是给人看的一行调度说明，用来拼 Summary。
func describeTiming(s Schedule) string {
	switch s.Kind {
	case kindEvery:
		return fmt.Sprintf("每 %d 秒", s.EverySec)
	case kindDaily:
		return "每天 " + s.DailyAt
	case kindWeekly:
		names := make([]string, 0, len(s.Weekdays))
		for _, d := range s.Weekdays {
			names = append(names, weekdayName(d))
		}
		return "每周 " + strings.Join(names, "、") + " " + s.WeeklyAt
	}
	return s.Kind
}

func weekdayName(d int) string {
	names := []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	if d < 0 || d >= len(names) {
		return strconv.Itoa(d)
	}
	return names[d]
}

// ---------------------------------------------------------------- 会话与 id

// currentSession 取当前会话 id；没有就明说，别让调用方去猜路径。
func currentSession(ctx context.Context) (string, error) {
	id := spi.SessionFrom(ctx)
	if id == "" {
		return "", fmt.Errorf("当前调用没有会话 id")
	}
	// 会话 id 会拼进文件名：挡住能跳出目录的字符。
	if id == "." || id == ".." || strings.ContainsAny(id, `/\:*?"<>|`) || strings.ContainsAny(id, "\r\n\t") {
		return "", fmt.Errorf("会话 id %q 含路径分隔符或控制字符", id)
	}
	return id, nil
}

// nextID 生成 sch-0001-3f9a 这种形状：序号取表内最大值 +1，后缀随机 4 位十六进制。
// 不用 UUID 库：这个 id 是给人念、给人贴进对话里的，短才有用。
func nextID(items []Schedule) (string, error) {
	max := 0
	for _, s := range items {
		if n := idSeq(s.ID); n > max {
			max = n
		}
	}
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成定时任务 id 失败: %w", err)
	}
	return fmt.Sprintf("sch-%04d-%s", max+1, hex.EncodeToString(buf[:])), nil
}

func idSeq(id string) int {
	parts := strings.Split(id, "-")
	if len(parts) < 2 {
		return 0
	}
	n := 0
	for _, ch := range parts[1] {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func indexOf(items []Schedule, id string) int {
	for i := range items {
		if items[i].ID == id {
			return i
		}
	}
	return -1
}

// recentIDs 给"找不到 id"的提示用：列出最近几条，让模型少猜一轮。
func recentIDs(items []Schedule, n int) []string {
	if len(items) == 0 {
		return nil
	}
	start := len(items) - n
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, len(items)-start)
	for _, s := range items[start:] {
		out = append(out, s.ID)
	}
	return out
}

func notFoundHint(items []Schedule) string {
	ids := recentIDs(items, 3)
	if len(ids) == 0 {
		return "这个会话还没有定时任务；先用 schedule_create 登记一条，或用 schedule_list 确认是空的"
	}
	return "没有这个 id；本会话最近的 id 是 " + strings.Join(ids, "、") + "，再不对就用 schedule_list 看全量"
}

// afterDeleteHint 在删完之后给下一步：还有就报最近的那条，没了就明说。
func afterDeleteHint(items []Schedule) string {
	if len(items) == 0 {
		return "已经一条不剩；要再建就用 schedule_create"
	}
	ids := recentIDs(items, 3)
	return "剩下的最近 id：" + strings.Join(ids, "、") + "；要改就用 schedule_update"
}

// ---------------------------------------------------------------- 装配

// Module 返回装配用的模块描述。dir 是状态目录（接线时传 <home>/state/schedules）。
func Module(dir string) kernel.Module {
	st := &store{dir: dir}
	return kernel.Module{
		ID:       "tool-schedule",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			for _, kind := range []string{kindCreate, kindList, kindUpdate, kindDelete} {
				if err := reg.Register(&Tool{st: st, kind: kind}); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
