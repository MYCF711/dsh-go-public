package main

// 官方 0.2.2 `goal-round-driver`（官方插件 #60，包 `@deepseek-ai/dsh-goal-round-driver`）
// 在 Go 宿主里的最小落点：**真实 turn 正常收工之后推进 goal 轮次**。
//
// 官方证据（解 `Desktop-0.2.2-rc.1/resources/app.asar` 只读得到）：
//   - 包/README：`/dsh/node_modules/@deepseek-ai/dsh-goal-round-driver/README.md`
//     §"What each round does"：一个 active + armed 的 goal 在 agent idle 时排一輪
//     `<goal_round>` prompt，"only an entered goal message consumes the cap"；
//     §"When continuation stops"：完成/暂停/阻塞都抑制续轮，轮次用尽写 `round-limit` blocker。
//   - 同一包 `lib/index.js` 的 fold：`user/message` 的 goal source 命中后
//     `state.roundsStarted = source.round`，即**计数来自真实进入历史的轮次**。
//   - goal 服务 `lib/index.js`：`resolveMaxGoalRounds` 要求正整数，create 时物化
//     部署默认值；`resume` 在 `roundsStarted >= maxGoalRounds` 时拒绝。
//
// 本宿主的三处已知差异（都写在 RESULT 的未验项里，不在这里偷偷抹平）：
//  1. 宿主没有 goal-source 消息，所以"哪一轮算 goal round"没有官方那种标记。
//     这里按本任务书口径：**每一轮真实 turn 正常收工 = 一轮**。
//  2. 官方驱动器会自己排 `<goal_round>` prompt 起新 turn；本任务明确不新建 turn，
//     因此这里只推进计数（证据在，能力留给后续接线，见 RESULT）。
//  3. `session.setGoal` 建 goal 时 `maxGoalRounds` 落 0（既有存储语义，本任务不改），
//     官方无法表达 0。这里把 `<=0` 解成"未配置上限"，否则宿主自己建的 goal 永远不计数。
//
// 重启一致性：不靠进程内计数器，而是把"这个 goal 已消化到第几轮真实 turn"做成
// 持久游标（`<home>/state/session-goal-rounds.json`），每次推进都从 v4 日志重算
// 目标值。重复调用算出同一个结果（幂等），崩溃后 `Reconcile` 能把漏记的轮次补上。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"dshgo/codecs/v4"
	"dshgo/internal/transport"
)

const goalRoundStoreVersion = 1

// goalRoundCursor 是"这个 goal 已经消化到哪"的持久游标。
//
//   - Turns：已消化的**正常完成**真实 turn 数（v4 `turn/end` 且 reason.kind=completed）。
//   - Rounds：消化完这些 turn 之后 `roundsStarted` 应有的值。
//
// 两个值都存"当前应为多少"而不是增量：崩溃后重算时用它和真实日志对齐，
// 不会因为一次重复调用把轮次多算一格。
type goalRoundCursor struct {
	GoalID string `json:"goalId"`
	Turns  int    `json:"turns"`
	Rounds int    `json:"rounds"`
}

type goalRoundDocument struct {
	Version  int                        `json:"version"`
	Sessions map[string]goalRoundCursor `json:"sessions"`
}

// goalRoundDriver 把 goal 存储接到真实 turn 生命周期上。
//
// 它**不**新建 turn、不排 prompt、不改 prompt 队列：唯一动作是 turn 收工后
// 推进 `roundsStarted`，并沿既有 goal 通道发一帧 `goal/change{operation:"round"}`。
type goalRoundDriver struct {
	goals *goalOverrideStore
	home  string
	path  string

	// mu 只保护游标；store 自己的锁在它外层（先 goals.mu 再 mu，绝不反向）。
	mu      sync.Mutex
	cursors map[string]goalRoundCursor
	loadErr error
}

func newGoalRoundDriver(goals *goalOverrideStore, home string) *goalRoundDriver {
	return &goalRoundDriver{
		goals:   goals,
		home:    home,
		path:    filepath.Join(home, "state", "session-goal-rounds.json"),
		cursors: map[string]goalRoundCursor{},
	}
}

// Load 读游标文件。缺失 = 全新（空游标）；损坏/不可读 = 记 loadErr 并**响亮**失败，
// 之后每次推进都会报错，而不是把"读不到"当成"从头开始"（那会重复计轮）。
func (d *goalRoundDriver) Load() error {
	raw, err := os.ReadFile(d.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		d.loadErr = fmt.Errorf("读 goal 轮次游标 %s: %w", d.path, err)
		return d.loadErr
	}
	var doc goalRoundDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		d.loadErr = fmt.Errorf("解析 goal 轮次游标 %s: %w", d.path, err)
		return d.loadErr
	}
	if doc.Version != goalRoundStoreVersion || doc.Sessions == nil {
		d.loadErr = fmt.Errorf("goal 轮次游标 %s version/sessions 非法：version=%d", d.path, doc.Version)
		return d.loadErr
	}
	next := make(map[string]goalRoundCursor, len(doc.Sessions))
	for sessionID, cursor := range doc.Sessions {
		if sessionID == "" || cursor.GoalID == "" || cursor.Turns < 0 || cursor.Rounds < 0 {
			d.loadErr = fmt.Errorf("goal 轮次游标 %s 会话 %q 非法：%+v", d.path, sessionID, cursor)
			return d.loadErr
		}
		next[sessionID] = cursor
	}
	d.mu.Lock()
	d.cursors = next
	d.mu.Unlock()
	return nil
}

// wrap 只加一层"turn 收工后推进轮次"，其余能力原样透传。
//
// 必须把 RunContent/Steer/SteerContent 一起透出去：包装层少了任何一个，都会让
// busy steer / 有序附件块在**没有任何提示**的情况下退化成 501——那是回归，
// 不是本任务想要的"只加驱动"。
func (d *goalRoundDriver) wrap(next turnRunner) turnRunner {
	return &goalRoundRunner{next: next, driver: d}
}

type goalRoundRunner struct {
	next   turnRunner
	driver *goalRoundDriver
}

func (r *goalRoundRunner) Run(ctx context.Context, sessionID, turnID, text string) error {
	err := r.next.Run(ctx, sessionID, turnID, text)
	if err == nil {
		r.driver.afterTurn(sessionID)
	}
	return err
}

func (r *goalRoundRunner) RunContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	next, ok := r.next.(contentTurnRunner)
	if !ok {
		// 与 promptService 的判据保持同一句话：包装层不许把"不支持"变成"先接受再失败"。
		return fmt.Errorf("%w: 当前 runner 未实现有序 content blocks", transport.ErrPlacementUnsupported)
	}
	err := next.RunContent(ctx, sessionID, turnID, blocks)
	if err == nil {
		r.driver.afterTurn(sessionID)
	}
	return err
}

// Steer 只是往**当前**轮里注入，不结束 turn，所以不推进轮次。
func (r *goalRoundRunner) Steer(ctx context.Context, sessionID, turnID, text string) error {
	next, ok := r.next.(steerTurnRunner)
	if !ok {
		return fmt.Errorf("%w: 当前 runner 没有真实 steer 注入口", transport.ErrPlacementUnsupported)
	}
	return next.Steer(ctx, sessionID, turnID, text)
}

func (r *goalRoundRunner) SteerContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	next, ok := r.next.(steerContentTurnRunner)
	if !ok {
		return fmt.Errorf("%w: 当前 runner 没有真实 content steer 注入口", transport.ErrPlacementUnsupported)
	}
	return next.SteerContent(ctx, sessionID, turnID, blocks)
}

// afterTurn 在 turn 已经成功收工之后跑：推进失败只能打到 stderr。
//
// 为什么不能把错误回传给 promptService：turn/done 在这一步之前就已经推给客户端了，
// 这时把 runner 的返回值改成错误，只会得到一个"客户端看到成功、宿主内部报失败"的
// 更糟状态；而 promptService 本来也会丢弃 runner 的返回值（`_ = lt.run(...)`）。
// 所以响亮 = 真实 stderr + 后续每次推进继续失败（store/cursor 的 loadErr 不会自愈），
// 不是假装成功。
func (d *goalRoundDriver) afterTurn(sessionID string) {
	if err := d.Advance(sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "goal 轮次推进失败（session=%s）：%v\n", sessionID, err)
	}
}

// Reconcile 在 serve 启动时把"上次进程结束前已经完成、但没来得及记账"的轮次补上。
//
// 没有它，"turn 已落盘 → 进程被杀 → 计数没写"这一格就永久丢一轮；有了它，
// 启动重算从 v4 日志得到真实应得数，游标只记录已经消化到哪。
func (d *goalRoundDriver) Reconcile() error {
	sessions, err := d.sessionsWithGoals()
	if err != nil {
		return err
	}
	var errs []error
	for _, sessionID := range sessions {
		if err := d.advance(sessionID, false); err != nil {
			errs = append(errs, fmt.Errorf("session %s: %w", sessionID, err))
		}
	}
	return errors.Join(errs...)
}

func (d *goalRoundDriver) sessionsWithGoals() ([]string, error) {
	if d.goals == nil {
		return nil, fmt.Errorf("%w: goal store 未接线", transport.ErrStatePersistence)
	}
	g := d.goals
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.loadErr != nil {
		return nil, fmt.Errorf("%w: %v", transport.ErrStatePersistence, g.loadErr)
	}
	out := make([]string, 0, len(g.m))
	for sessionID, record := range g.m {
		if record.Goal != nil {
			out = append(out, sessionID)
		}
	}
	sort.Strings(out) // 固定顺序：重启重算不因 map 遍历随机性产生不同帧序
	return out, nil
}

// Advance 是唯一推进点：把一个会话的 goal 轮次对齐到"真实完成的 turn 数"。
//
// 幂等是核心性质：重复调用（同一轮收工被调用两次、重启重算、崩溃后补算）
// 都只会得到同一个 `roundsStarted`，因为它不是 `+1`，而是"由 v4 日志重算目标值"。
func (d *goalRoundDriver) Advance(sessionID string) error {
	return d.advance(sessionID, true)
}

// advance 的 countCurrentTurn 区分两条真实调用路径：
//   - turn 收工（Advance）：刚刚完成的就是目标的第 1 轮（目标可能在本轮里创建）；
//   - 启动重算（Reconcile）：没有"刚收工的那一轮"，基线就是全部历史轮次。
//
// 别小看这一格：目标是在两次 turn 之间建的（`session.setGoal`）时，少了它
// 第一轮就永远不算，目标的上限也就永远差一格。
func (d *goalRoundDriver) advance(sessionID string, countCurrentTurn bool) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id 为空", transport.ErrStatePersistence)
	}
	if d.goals == nil {
		return fmt.Errorf("%w: goal store 未接线", transport.ErrStatePersistence)
	}
	if err := d.cursorLoadError(); err != nil {
		return err
	}
	g := d.goals
	// 整段持 store 锁：goal 的 phase/rounds 判定与随后的写入必须是同一份事实，
	// 否则 pause/clear 插在中间就会把轮次推进到一个已经停用的 goal 上。
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.loadErr != nil {
		return fmt.Errorf("%w: %v", transport.ErrStatePersistence, g.loadErr)
	}

	record, exists := g.m[sessionID]
	if !exists || record.Goal == nil {
		// clear-missing 幂等：没有 goal 就没有轮次。不写盘、不发事件。
		return nil
	}
	goal := record.Goal
	rounds := 0
	if record.RoundsStarted != nil {
		rounds = *record.RoundsStarted
	}

	completed, err := d.completedTurns(sessionID)
	if err != nil {
		return err
	}
	previous, hasPrevious := d.cursor(sessionID)
	if !hasPrevious || previous.GoalID != goal.ID {
		// 第一次见到这个 goal：基线是它出现之前已经完成的轮次。
		baseline := completed
		if countCurrentTurn && completed > 0 {
			baseline = completed - 1
		}
		previous = goalRoundCursor{GoalID: goal.ID, Turns: baseline, Rounds: rounds}
	}
	next := goalRoundCursor{GoalID: goal.ID, Turns: completed, Rounds: previous.Rounds}
	if previous.Turns > completed {
		// 日志比游标短（换过 home、文件被截）：游标不倒退，也不倒推轮次。
		next.Turns = previous.Turns
	} else if goal.Phase == "active" && completed > previous.Turns {
		next.Rounds = previous.Rounds + (completed - previous.Turns)
	}
	if next.Rounds < rounds {
		// 别处已经推进过（或游标比存储旧）：以存储为准，绝不倒推。
		next.Rounds = rounds
	}
	if cap := goal.MaxGoalRounds; cap > 0 && next.Rounds > cap {
		next.Rounds = cap
	}

	if next.Rounds > rounds {
		advanced := next.Rounds
		nextRecord := cloneGoalRecord(record)
		nextRecord.RoundsStarted = &advanced
		nextGoals := cloneGoalMap(g.m)
		nextGoals[sessionID] = nextRecord
		if err := writeGoalDocument(g.path, goalDocument{Version: goalStoreVersion, Goals: nextGoals}); err != nil {
			return fmt.Errorf("%w: 写 goal store %s: %v", transport.ErrStatePersistence, g.path, err)
		}
		previousGoals := cloneGoalMap(g.m)
		g.m = nextGoals
		if err := d.appendRoundFrame(sessionID, advanced); err != nil {
			// state 已写、帧没提交：回滚 state，不留"没有事件的成功计数"。
			if rollbackErr := writeGoalDocument(g.path, goalDocument{Version: goalStoreVersion, Goals: previousGoals}); rollbackErr != nil {
				g.loadErr = fmt.Errorf("goal round 帧提交失败 %v；roundsStarted 回滚也失败 %v", err, rollbackErr)
				return fmt.Errorf("%w: %v", transport.ErrStatePersistence, g.loadErr)
			}
			g.m = previousGoals
			return err
		}
	}

	if !hasPrevious || previous != next {
		if err := d.setCursor(sessionID, next); err != nil {
			return err
		}
	}
	return nil
}

// cursorLoadError 把"游标文件坏了"变成每次推进都失败的明确错误。
func (d *goalRoundDriver) cursorLoadError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loadErr != nil {
		return fmt.Errorf("%w: %v", transport.ErrStatePersistence, d.loadErr)
	}
	return nil
}

func (d *goalRoundDriver) cursor(sessionID string) (goalRoundCursor, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cursor, ok := d.cursors[sessionID]
	return cursor, ok
}

func (d *goalRoundDriver) setCursor(sessionID string, cursor goalRoundCursor) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	previous, hadPrevious := d.cursors[sessionID]
	d.cursors[sessionID] = cursor
	if err := d.writeCursorsLocked(); err != nil {
		if hadPrevious {
			d.cursors[sessionID] = previous
		} else {
			delete(d.cursors, sessionID)
		}
		return err
	}
	return nil
}

// writeCursorsLocked 原子落盘（tmp + rename）；调用方必须持有 d.mu。
//
// 游标写失败**不会**把已经提交的 roundsStarted 退回去：state 是权威，游标只是
// 重算用的进度。下一次 Advance 从旧游标重算会得到同一个目标值（幂等），
// 所以这里只报错，不制造第二次状态变更。
func (d *goalRoundDriver) writeCursorsLocked() error {
	doc := goalRoundDocument{Version: goalRoundStoreVersion, Sessions: make(map[string]goalRoundCursor, len(d.cursors))}
	for sessionID, cursor := range d.cursors {
		doc.Sessions[sessionID] = cursor
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: 序列化 goal 轮次游标: %v", transport.ErrStatePersistence, err)
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return fmt.Errorf("%w: 建 goal 轮次游标目录: %v", transport.ErrStatePersistence, err)
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("%w: 写 goal 轮次游标 %s: %v", transport.ErrStatePersistence, tmp, err)
	}
	if err := os.Rename(tmp, d.path); err != nil {
		return fmt.Errorf("%w: 提交 goal 轮次游标 %s: %v", transport.ErrStatePersistence, d.path, err)
	}
	return nil
}

// appendRoundFrame 沿既有 goal 通道发一帧续轮。
//
// 形状按 `docs/tui-effort-modes-goals-host-note.md` §3：`operation:"round"` +
// 真实非负整数 `round`，**不重复 snapshot**；不新造帧类型，仍是 `goal/change`。
//
// 一次最多一帧：崩溃重启后跨多轮补算时，`round` 是推进后的**绝对计数**，
// 不是增量。这样 journal 里不会出现"中间帧已入账、state 却回滚了"的半套记录
// ——读方只要认最后一次 round 值就能对齐。
func (d *goalRoundDriver) appendRoundFrame(sessionID string, round int) error {
	journal := d.goals.journal
	if journal == nil || journal.journals == nil {
		return fmt.Errorf("%w: goal/change journal 未接线", transport.ErrStatePersistence)
	}
	value := round
	data, err := json.Marshal(transport.GoalChange{
		Kind: "goal/change", Version: 1, Operation: "round", Round: &value,
	})
	if err != nil {
		return fmt.Errorf("%w: 序列化 goal round 帧: %v", transport.ErrStatePersistence, err)
	}
	if _, err := emitFrameResult(journal.journals, journal.pub, sessionID, "goal/change", data); err != nil {
		return fmt.Errorf("%w: 追加 goal round 帧: %v", transport.ErrStatePersistence, err)
	}
	return nil
}

// completedTurns 数这份会话里**正常收工**的真实 turn 数。
//
// 判据只认 v4 日志自己的结算：`turn/end` 且 `reason.kind` 为 completed（空 kind 是
// 早期日志形状，同样表示"这一轮结束了"）。取消（aborted/interrupted）、模型报错
// （error）、被 max tokens 截断（max-tokens）都不算——官方驱动器同样在
// max tokens 收工时停止续轮。
func (d *goalRoundDriver) completedTurns(sessionID string) (int, error) {
	path, err := newSessionSource(d.home, "").resolve(sessionID)
	if err != nil {
		if errors.Is(err, transport.ErrNoSuchSession) {
			return 0, nil // 会话文件还没建：没有已完成轮次，不是失败
		}
		return 0, fmt.Errorf("goal 轮次推进读会话 %s: %v", sessionID, err)
	}
	log, err := v4.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("goal 轮次推进读 v4 %s: %v", path, err)
	}
	completed := 0
	for _, event := range log.Events {
		if event.Type != "turn/end" {
			continue
		}
		var data struct {
			Reason struct {
				Kind string `json:"kind"`
			} `json:"reason"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return 0, fmt.Errorf("解析 %s seq=%d 的 turn/end: %v", path, event.Seq, err)
		}
		switch data.Reason.Kind {
		case "", "completed":
			completed++
		}
	}
	return completed, nil
}
