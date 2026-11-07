package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// 这一组测试钉住官方 0.2.2 `goal-round-driver`（官方插件 #60）在 Go 宿主里的
// 最小语义：**一轮真实 turn 正常收工之后**，该会话若有 active goal，
// `roundsStarted` 前进一格并持久化；到上限 / 非 active 就停；崩溃重启不重复计、
// 不丢计；A/B 会话各自独立。
//
// 全部只走宿主真实形状：v4 会话日志 + goal store + Transport journal，
// 不伪造 snapshot、不拿 HTTP ACK 冒充事件。

// createGoalRoundSession 建一条真实 v4 会话（与 serve 落盘同一形状）。
func createGoalRoundSession(t *testing.T, home, id string) string {
	t.Helper()
	lg, err := session.CreateSession(home, t.TempDir(), id, "default")
	if err != nil {
		t.Fatalf("建会话 %s: %v", id, err)
	}
	path := lg.Path
	if err := lg.Close(); err != nil {
		t.Fatalf("关闭会话 %s: %v", id, err)
	}
	return path
}

// writeGoalRoundTurn 把一轮写进 v4：turn/start → user → assistant → turn/end。
//
// kind 用 v4 自己的词汇（completed / max-tokens / error …），与
// session.TurnEndKind 的映射一致；驱动只认 completed。
func writeGoalRoundTurn(home, sessionID, text, kind string) error {
	src := newSessionSource(home, "")
	path, err := src.resolve(sessionID)
	if err != nil {
		return err
	}
	turn := nextTurnNumber(path)
	logw, err := session.OpenSessionAt(path)
	if err != nil {
		return err
	}
	defer logw.Close()
	if err := logw.Append("turn/start", session.TurnStartData(turn)); err != nil {
		return err
	}
	if err := logw.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: text}})); err != nil {
		return err
	}
	if err := logw.Append("assistant/message",
		session.AssistantMessageEvent(turn, 1, []map[string]any{session.TextBlock("ok")}, "stop", nil)); err != nil {
		return err
	}
	return logw.Append("turn/end", session.TurnEndData(turn, kind, ""))
}

// waitGoalRoundIdle 等到 promptService 真的收工（runner 返回、finish 跑完）。
func waitGoalRoundIdle(t *testing.T, p *promptService, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.Busy(sessionID) {
		if time.Now().After(deadline) {
			t.Fatalf("会话 %s 的轮次 5s 内没有收工", sessionID)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// goalRoundFrames 只挑"续轮"帧（operation=round）。
//
// create/pause/resume 也走同一条 goal/change 通道，它们是既有 store 的行为，
// 不参与轮次断言——这样断言只盯本任务新增的那一格。
func goalRoundFrames(frames []transport.ServerFrame) []transport.ServerFrame {
	out := make([]transport.ServerFrame, 0, len(frames))
	for _, frame := range frames {
		if frame.Type != "goal/change" {
			continue
		}
		var change transport.GoalChange
		if err := json.Unmarshal(frame.Data, &change); err != nil {
			continue
		}
		if change.Operation == "round" {
			out = append(out, frame)
		}
	}
	return out
}

// goalRoundCount 把 *int 的 roundsStarted 打印成人能读的计数（nil = 未知）。
func goalRoundCount(result transport.GoalResult) string {
	if result.RoundsStarted == nil {
		return "未知(nil)"
	}
	return strconv.Itoa(*result.RoundsStarted)
}

// roundFrame 解一帧 goal/change，并钉住"续轮帧"的形状：
// operation=round + 真实 round 值 + 不重复 snapshot。
func roundFrame(t *testing.T, frame transport.ServerFrame, wantRound int) {
	t.Helper()
	if frame.Type != "goal/change" {
		t.Fatalf("帧类型=%q，want goal/change", frame.Type)
	}
	var change transport.GoalChange
	if err := json.Unmarshal(frame.Data, &change); err != nil {
		t.Fatalf("解 goal/change: %v", err)
	}
	if change.Kind != "goal/change" || change.Version != 1 || change.Operation != "round" {
		t.Fatalf("续轮帧形状不对：%+v", change)
	}
	if change.Round == nil || *change.Round != wantRound {
		t.Fatalf("续轮帧 round=%v，want %d", change.Round, wantRound)
	}
	if change.Goal != nil {
		t.Fatalf("续轮帧不得重复 snapshot：%+v", change.Goal)
	}
	if change.RoundsStarted != nil {
		t.Fatalf("续轮帧只带 round，不重复 roundsStarted：%+v", change.RoundsStarted)
	}
}

// goalRoundFixture 是一套"真实宿主形状"的夹具：goal store + 驱动 + journal。
type goalRoundFixture struct {
	home     string
	journals *transport.Journals
	pushed   []transport.ServerFrame
	store    *goalOverrideStore
	driver   *goalRoundDriver
}

func newGoalRoundFixture(t *testing.T, home string) *goalRoundFixture {
	t.Helper()
	journals := transport.NewJournals(nil)
	f := &goalRoundFixture{home: home, journals: journals}
	f.store = openGoalStoreForTest(t, home, journals, &f.pushed)
	f.driver = newGoalRoundDriver(f.store, home)
	if err := f.driver.Load(); err != nil {
		t.Fatalf("读 goal 轮次游标: %v", err)
	}
	return f
}

func (f *goalRoundFixture) goalRunner() turnRunner {
	return f.driver.wrap(turnRunnerFunc(func(_ context.Context, sessionID, _, text string) error {
		return writeGoalRoundTurn(f.home, sessionID, text, "completed")
	}))
}

func (f *goalRoundFixture) count(t *testing.T, sessionID string) int {
	t.Helper()
	result, err := f.store.Snapshot(sessionID)
	if err != nil {
		t.Fatalf("读 goal: %v", err)
	}
	if result.RoundsStarted == nil {
		t.Fatalf("会话 %s 的 roundsStarted 未知，want 真实计数", sessionID)
	}
	return *result.RoundsStarted
}

// TestGoalRoundTurnEndAdvancesRoundsStarted 是这一块的红点：
// 以前 turn 正常结束后 roundsStarted 恒为 0（只有存储、没有驱动）。
func TestGoalRoundTurnEndAdvancesRoundsStarted(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-round"
	createGoalRoundSession(t, home, sid)

	f := newGoalRoundFixture(t, home)
	if err := f.store.Set(sid, "把 goal 轮次驱动补齐"); err != nil {
		t.Fatalf("设 goal: %v", err)
	}
	if got := f.count(t, sid); got != 0 {
		t.Fatalf("新建 goal 的 roundsStarted=%d，want 0", got)
	}

	// 一轮真实 turn：runner 把这一轮正常落盘（turn/end kind=completed）后返回 nil。
	p := newPromptService(f.goalRunner())
	if _, err := p.Submit(context.Background(), sid, "第一轮"); err != nil {
		t.Fatalf("提交第一轮: %v", err)
	}
	waitGoalRoundIdle(t, p, sid)

	if got := f.count(t, sid); got != 1 {
		t.Fatalf("turn 正常结束后 roundsStarted=%d，want 1：宿主没有 goal 轮次驱动，goal 永不推进", got)
	}

	rounds := goalRoundFrames(f.pushed)
	if len(rounds) != 1 {
		t.Fatalf("goal/change 帧=%d，want 1（只该有这一轮的 round 帧）：%+v", len(rounds), rounds)
	}
	roundFrame(t, rounds[0], 1)

	// 同一份事实要能由 session.history/断线补洞读到（同一个 journal）：
	// 先是建 goal 的 create 帧，紧接着就是这一轮的 round 帧。
	frames, last, _ := f.journals.Get(sid).Since(0, 0)
	if last != 2 || len(frames) != 2 {
		t.Fatalf("journal last=%d frames=%+v，want create + round 两帧", last, frames)
	}
	var created, advanced transport.GoalChange
	if err := json.Unmarshal(frames[0].Data, &created); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(frames[1].Data, &advanced); err != nil {
		t.Fatal(err)
	}
	if created.Operation != "create" || frames[0].Seq != 1 {
		t.Fatalf("第一帧应当是 create：seq=%d %+v", frames[0].Seq, created)
	}
	if advanced.Operation != "round" || frames[1].Seq != 2 || frames[1].Type != "goal/change" {
		t.Fatalf("第二帧应当是 round：seq=%d %+v", frames[1].Seq, advanced)
	}

	// 落盘的才是真推进：重启后还得到 1（不是只改了内存）。
	var doc goalDocument
	raw, err := os.ReadFile(filepath.Join(home, "state", "session-goal.json"))
	if err != nil {
		t.Fatalf("读 goal store: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解 goal store: %v", err)
	}
	if record := doc.Goals[sid]; record.RoundsStarted == nil || *record.RoundsStarted != 1 {
		t.Fatalf("goal store 里的 roundsStarted=%v，want 1", record.RoundsStarted)
	}

	// 幂等：再推进一次（同一轮重复回调）不得变成 2。
	if err := f.driver.Advance(sid); err != nil {
		t.Fatalf("重复推进: %v", err)
	}
	if got := f.count(t, sid); got != 1 {
		t.Fatalf("同一轮重复推进后 roundsStarted=%d，want 1", got)
	}
	if got := len(goalRoundFrames(f.pushed)); got != 1 {
		t.Fatalf("重复推进多发了一帧：%d", got)
	}
}

// TestGoalRoundAbnormalTurnDoesNotAdvance 钉住"正常结束"的判据：
// 被 max tokens 截断、或 runner 报错，都不算一轮。
func TestGoalRoundAbnormalTurnDoesNotAdvance(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-abnormal"
	createGoalRoundSession(t, home, sid)
	f := newGoalRoundFixture(t, home)
	if err := f.store.Set(sid, "只统计正常收工的轮次"); err != nil {
		t.Fatal(err)
	}

	// 1) turn/end kind=max-tokens：日志里有这一轮，但不是"做完了"。
	truncated := f.driver.wrap(turnRunnerFunc(func(_ context.Context, sessionID, _, text string) error {
		return writeGoalRoundTurn(home, sessionID, text, "max-tokens")
	}))
	p := newPromptService(truncated)
	if _, err := p.Submit(context.Background(), sid, "截断的一轮"); err != nil {
		t.Fatal(err)
	}
	waitGoalRoundIdle(t, p, sid)
	if got := f.count(t, sid); got != 0 {
		t.Fatalf("max-tokens 收工后 roundsStarted=%d，want 0", got)
	}

	// 2) runner 直接报错（取消/模型错误走这条）：更不该推进。
	failed := f.driver.wrap(turnRunnerFunc(func(context.Context, string, string, string) error {
		return errors.New("模型连不上")
	}))
	p = newPromptService(failed)
	if _, err := p.Submit(context.Background(), sid, "失败的一轮"); err != nil {
		t.Fatal(err)
	}
	waitGoalRoundIdle(t, p, sid)
	if got := f.count(t, sid); got != 0 {
		t.Fatalf("失败的一轮之后 roundsStarted=%d，want 0", got)
	}
	if frames := goalRoundFrames(f.pushed); len(frames) != 0 {
		t.Fatalf("异常轮次不得发续轮帧：%+v", frames)
	}
}

// TestGoalRoundPauseStopsAndResumeContinues 钉住 pause/resume：
// pause 之后不再推进（暂停期间跑完的轮次也不许在 resume 后补算），resume 之后继续。
func TestGoalRoundPauseStopsAndResumeContinues(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-pause"
	createGoalRoundSession(t, home, sid)
	f := newGoalRoundFixture(t, home)
	if err := f.store.Set(sid, "暂停就不许再推进"); err != nil {
		t.Fatal(err)
	}
	p := newPromptService(f.goalRunner())

	if _, err := p.Submit(context.Background(), sid, "第一轮"); err != nil {
		t.Fatal(err)
	}
	waitGoalRoundIdle(t, p, sid)
	if got := f.count(t, sid); got != 1 {
		t.Fatalf("第一轮后 roundsStarted=%d，want 1", got)
	}

	if err := f.store.Pause(sid); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := p.Submit(context.Background(), sid, "暂停期间的一轮"); err != nil {
		t.Fatal(err)
	}
	waitGoalRoundIdle(t, p, sid)
	if got := f.count(t, sid); got != 1 {
		t.Fatalf("pause 之后 roundsStarted=%d，want 1（不许推进）", got)
	}
	if frames := goalRoundFrames(f.pushed); len(frames) != 1 {
		t.Fatalf("pause 期间不得发续轮帧：%d", len(frames))
	}

	if err := f.store.Resume(sid); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := p.Submit(context.Background(), sid, "resume 之后的一轮"); err != nil {
		t.Fatal(err)
	}
	waitGoalRoundIdle(t, p, sid)
	if got := f.count(t, sid); got != 2 {
		t.Fatalf("resume 之后 roundsStarted=%d，want 2（暂停期间那一轮不补算）", got)
	}
	rounds := goalRoundFrames(f.pushed)
	if len(rounds) != 2 {
		t.Fatalf("续轮帧=%d，want 2", len(rounds))
	}
	roundFrame(t, rounds[1], 2)
}

// TestGoalRoundCapStopsAtMaxGoalRounds 钉住上限：到 maxGoalRounds 就停，不多发帧。
func TestGoalRoundCapStopsAtMaxGoalRounds(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-cap"
	createGoalRoundSession(t, home, sid)
	f := newGoalRoundFixture(t, home)
	if err := f.store.Set(sid, "两轮就停"); err != nil {
		t.Fatal(err)
	}
	// 既有 session.setGoal 不传上限（落 0）；测试直接把上限钉成 2，模拟 goal 自带 cap。
	f.store.mu.Lock()
	record := f.store.m[sid]
	record.Goal.MaxGoalRounds = 2
	f.store.m[sid] = record
	f.store.mu.Unlock()

	p := newPromptService(f.goalRunner())
	for i := 0; i < 4; i++ {
		if _, err := p.Submit(context.Background(), sid, "第 "+strconv.Itoa(i+1)+" 轮"); err != nil {
			t.Fatal(err)
		}
		waitGoalRoundIdle(t, p, sid)
	}
	if got := f.count(t, sid); got != 2 {
		t.Fatalf("到上限后 roundsStarted=%d，want 2", got)
	}
	rounds := goalRoundFrames(f.pushed)
	if len(rounds) != 2 {
		t.Fatalf("续轮帧=%d，want 2（到上限后不再发）", len(rounds))
	}
	roundFrame(t, rounds[0], 1)
	roundFrame(t, rounds[1], 2)
}

// TestGoalRoundClearMissingIsIdempotentAndSilent 钉住 clear-missing：
// 没有 goal 的会话推进/清除都幂等，且不写 state、不发事件。
func TestGoalRoundClearMissingIsIdempotentAndSilent(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-none"
	createGoalRoundSession(t, home, sid)
	f := newGoalRoundFixture(t, home)

	for i := 0; i < 2; i++ {
		if err := f.driver.Advance(sid); err != nil {
			t.Fatalf("无 goal 时推进应当幂等成功：%v", err)
		}
		if err := f.store.Set(sid, ""); err != nil {
			t.Fatalf("无 goal 时清除应当幂等成功：%v", err)
		}
	}
	if frames := goalRoundFrames(f.pushed); len(frames) != 0 {
		t.Fatalf("无 goal 不得发帧：%+v", frames)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "session-goal.json")); !os.IsNotExist(err) {
		t.Fatalf("无 goal 不得写 goal state：err=%v", err)
	}
	result, err := f.store.Snapshot(sid)
	if err != nil {
		t.Fatal(err)
	}
	if result.Goal != nil || result.RoundsStarted != nil {
		t.Fatalf("无 goal 的 snapshot=%+v", result)
	}
}

// TestGoalRoundRestartKeepsCountWithoutDoubleCount 钉住重启：
// 已经记过的轮次重启后还在，且不会被重算成两格、不会多发帧。
func TestGoalRoundRestartKeepsCountWithoutDoubleCount(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-restart"
	createGoalRoundSession(t, home, sid)
	f := newGoalRoundFixture(t, home)
	if err := f.store.Set(sid, "重启不重复计"); err != nil {
		t.Fatal(err)
	}
	p := newPromptService(f.goalRunner())
	if _, err := p.Submit(context.Background(), sid, "第一轮"); err != nil {
		t.Fatal(err)
	}
	waitGoalRoundIdle(t, p, sid)
	if got := f.count(t, sid); got != 1 {
		t.Fatalf("第一轮后 roundsStarted=%d，want 1", got)
	}

	// 重启：新 store（重新 Load）+ 新 driver（重新 Load 游标）+ 启动重算。
	restarted := newGoalRoundFixture(t, home)
	if err := restarted.driver.Reconcile(); err != nil {
		t.Fatalf("重启重算: %v", err)
	}
	if got := restarted.count(t, sid); got != 1 {
		t.Fatalf("重启后 roundsStarted=%d，want 1（不重复计、不丢计）", got)
	}
	if frames := goalRoundFrames(restarted.pushed); len(frames) != 0 {
		t.Fatalf("重启重算不得重发续轮帧：%+v", frames)
	}
	// 重启后的下一轮从 1 走到 2，而不是从 0 重来。
	p2 := newPromptService(restarted.goalRunner())
	if _, err := p2.Submit(context.Background(), sid, "重启后的第一轮"); err != nil {
		t.Fatal(err)
	}
	waitGoalRoundIdle(t, p2, sid)
	if got := restarted.count(t, sid); got != 2 {
		t.Fatalf("重启后跑一轮 roundsStarted=%d，want 2", got)
	}
}

// TestGoalRoundRestartRecoversTurnMissedBeforeCrash 钉住"不丢计"：
// turn 已经落盘、进程在记账前被杀 → 重启重算把这一轮补上。
func TestGoalRoundRestartRecoversTurnMissedBeforeCrash(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-crash"
	createGoalRoundSession(t, home, sid)

	// 进程第一次起来：建立了游标基线（此时 0 轮），随后被杀死。
	before := newGoalRoundFixture(t, home)
	if err := before.store.Set(sid, "崩溃前漏记账要能补"); err != nil {
		t.Fatal(err)
	}
	if err := before.driver.Reconcile(); err != nil {
		t.Fatalf("基线重算: %v", err)
	}
	if got := before.count(t, sid); got != 0 {
		t.Fatalf("基线 roundsStarted=%d，want 0", got)
	}
	if frames := goalRoundFrames(before.pushed); len(frames) != 0 {
		t.Fatalf("基线不得发帧：%+v", frames)
	}

	// 一轮真实 turn 已经落盘（turn/end completed），但进程在这里被杀，没来得及记账。
	if err := writeGoalRoundTurn(home, sid, "崩溃前跑完的一轮", "completed"); err != nil {
		t.Fatal(err)
	}

	// 重启：重算必须把这一轮补上，并且只补一格。
	after := newGoalRoundFixture(t, home)
	if err := after.driver.Reconcile(); err != nil {
		t.Fatalf("重启重算: %v", err)
	}
	if got := after.count(t, sid); got != 1 {
		t.Fatalf("重启重算后 roundsStarted=%d，want 1（不丢计）", got)
	}
	frames := goalRoundFrames(after.pushed)
	if len(frames) != 1 {
		t.Fatalf("补记应当发一帧续轮：%d", len(frames))
	}
	roundFrame(t, frames[0], 1)
	// 再重算一次：幂等，不多记也不多发。
	if err := after.driver.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := after.count(t, sid); got != 1 {
		t.Fatalf("二次重算 roundsStarted=%d，want 1", got)
	}
	if got := len(goalRoundFrames(after.pushed)); got != 1 {
		t.Fatalf("二次重算多发帧：%d", got)
	}
}

// TestGoalRoundPriorTurnsAreBaseline 钉住"goal 建之前的轮次不算"：
// 会话里已有的 3 轮是基线，goal 建完之后跑的一轮才是第 1 轮。
func TestGoalRoundPriorTurnsAreBaseline(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-baseline"
	createGoalRoundSession(t, home, sid)
	for i := 0; i < 3; i++ {
		if err := writeGoalRoundTurn(home, sid, "建 goal 之前的一轮", "completed"); err != nil {
			t.Fatal(err)
		}
	}
	f := newGoalRoundFixture(t, home)
	if err := f.store.Set(sid, "历史轮次不算"); err != nil {
		t.Fatal(err)
	}
	if err := f.driver.Reconcile(); err != nil {
		t.Fatalf("建 goal 后的基线重算: %v", err)
	}
	if got := f.count(t, sid); got != 0 {
		t.Fatalf("建 goal 之前的历史轮次被算进去了：roundsStarted=%d，want 0", got)
	}
	p := newPromptService(f.goalRunner())
	if _, err := p.Submit(context.Background(), sid, "建 goal 之后的一轮"); err != nil {
		t.Fatal(err)
	}
	waitGoalRoundIdle(t, p, sid)
	if got := f.count(t, sid); got != 1 {
		t.Fatalf("建 goal 之后跑一轮 roundsStarted=%d，want 1", got)
	}
}

// TestGoalRoundABIsolationUnderConcurrentTurns 钉住并发独立：
// A/B 各自计数、各自 journal，不串。
func TestGoalRoundABIsolationUnderConcurrentTurns(t *testing.T) {
	home := t.TempDir()
	const (
		sidA = "session-goal-a"
		sidB = "session-goal-b"
	)
	createGoalRoundSession(t, home, sidA)
	createGoalRoundSession(t, home, sidB)
	f := newGoalRoundFixture(t, home)
	for _, sid := range []string{sidA, sidB} {
		if err := f.store.Set(sid, "并发 A/B "+sid); err != nil {
			t.Fatal(err)
		}
	}
	p := newPromptService(f.goalRunner())
	var wg sync.WaitGroup
	for _, sid := range []string{sidA, sidB} {
		sid := sid
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Submit(context.Background(), sid, "并发的一轮"); err != nil {
				t.Errorf("Submit(%s): %v", sid, err)
			}
		}()
	}
	wg.Wait()
	waitGoalRoundIdle(t, p, sidA)
	waitGoalRoundIdle(t, p, sidB)

	if got := f.count(t, sidA); got != 1 {
		t.Fatalf("A roundsStarted=%d，want 1", got)
	}
	if got := f.count(t, sidB); got != 1 {
		t.Fatalf("B roundsStarted=%d，want 1", got)
	}
	// 每条会话一条独立 journal：各自 create(seq=1) + round(seq=2)，互不串号。
	for _, sid := range []string{sidA, sidB} {
		frames, last, _ := f.journals.Get(sid).Since(0, 0)
		if last != 2 || len(frames) != 2 || frames[0].Seq != 1 || frames[1].Seq != 2 {
			t.Fatalf("会话 %s 的 journal last=%d frames=%+v", sid, last, frames)
		}
		roundFrame(t, frames[1], 1)
	}
}

// TestGoalRoundDiskFailureIsLoudAndRollsBack 钉住失败语义：
// 写盘失败必须报错、回滚内存、且不发帧——不许静默半写。
func TestGoalRoundDiskFailureIsLoudAndRollsBack(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-diskfail"
	createGoalRoundSession(t, home, sid)
	f := newGoalRoundFixture(t, home)
	if err := f.store.Set(sid, "写盘失败要响亮"); err != nil {
		t.Fatal(err)
	}
	if err := writeGoalRoundTurn(home, sid, "这一轮会撞上坏盘", "completed"); err != nil {
		t.Fatal(err)
	}

	// 把 goal store 的落点变成目录：writeGoalDocument 的 rename 必然失败。
	storePath := filepath.Join(home, "state", "session-goal.json")
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("移除 goal store: %v", err)
	}
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatalf("把 goal store 换成目录: %v", err)
	}

	err := f.driver.Advance(sid)
	if err == nil {
		t.Fatal("写盘失败必须报错，不能静默半写")
	}
	if !errors.Is(err, transport.ErrStatePersistence) {
		t.Fatalf("错误应当是 ErrStatePersistence：%v", err)
	}
	if got := f.count(t, sid); got != 0 {
		t.Fatalf("失败后内存 roundsStarted=%d，want 0（已回滚）", got)
	}
	if frames := goalRoundFrames(f.pushed); len(frames) != 0 {
		t.Fatalf("状态没写成功就不许发帧：%+v", frames)
	}
}

// TestGoalRoundWrapperKeepsRunnerCapabilities 钉住包装层不吞能力：
// RunContent/Steer/SteerContent 必须原样转发（否则 busy steer 与附件块会静默退化）。
func TestGoalRoundWrapperKeepsRunnerCapabilities(t *testing.T) {
	home := t.TempDir()
	const sid = "session-goal-wrap"
	createGoalRoundSession(t, home, sid)
	f := newGoalRoundFixture(t, home)

	var (
		ran, contentRan   bool
		steered, steeredC bool
	)
	full := &goalRoundFullFake{
		run: func(context.Context, string, string, string) error { ran = true; return nil },
		content: func(context.Context, string, string, []transport.ContentBlock) error {
			contentRan = true
			return nil
		},
		steer:        func(context.Context, string, string, string) error { steered = true; return nil },
		steerContent: func(context.Context, string, string, []transport.ContentBlock) error { steeredC = true; return nil },
	}
	wrapped := f.driver.wrap(full)
	if err := wrapped.Run(context.Background(), sid, "t", "x"); err != nil {
		t.Fatal(err)
	}
	content, ok := wrapped.(contentTurnRunner)
	if !ok {
		t.Fatal("包装层丢了 RunContent 能力")
	}
	if err := content.RunContent(context.Background(), sid, "t", []transport.ContentBlock{{Type: "text", Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	steer, ok := wrapped.(steerTurnRunner)
	if !ok {
		t.Fatal("包装层丢了 Steer 能力")
	}
	if err := steer.Steer(context.Background(), sid, "t", "x"); err != nil {
		t.Fatal(err)
	}
	steerC, ok := wrapped.(steerContentTurnRunner)
	if !ok {
		t.Fatal("包装层丢了 SteerContent 能力")
	}
	if err := steerC.SteerContent(context.Background(), sid, "t", []transport.ContentBlock{{Type: "text", Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	if !ran || !contentRan || !steered || !steeredC {
		t.Fatalf("转发不完整：run=%v content=%v steer=%v steerContent=%v", ran, contentRan, steered, steeredC)
	}

	// 底层没有该能力时，包装层必须明确拒绝，而不是"先接受再失败"。
	runOnly := f.driver.wrap(turnRunnerFunc(func(context.Context, string, string, string) error { return nil }))
	if _, ok := runOnly.(contentTurnRunner); !ok {
		t.Fatal("包装层对 Run-only runner 也必须暴露 RunContent 以给出明确拒绝")
	}
	if err := runOnly.(contentTurnRunner).RunContent(context.Background(), sid, "t", nil); !errors.Is(err, transport.ErrPlacementUnsupported) {
		t.Fatalf("Run-only runner 的 RunContent 错误=%v，want ErrPlacementUnsupported", err)
	}
	if err := runOnly.(steerTurnRunner).Steer(context.Background(), sid, "t", "x"); !errors.Is(err, transport.ErrPlacementUnsupported) {
		t.Fatalf("Run-only runner 的 Steer 错误=%v，want ErrPlacementUnsupported", err)
	}
}

// goalRoundFullFake 是"四种能力都在"的 runner，用来证明包装层不吞能力。
type goalRoundFullFake struct {
	run          func(context.Context, string, string, string) error
	content      func(context.Context, string, string, []transport.ContentBlock) error
	steer        func(context.Context, string, string, string) error
	steerContent func(context.Context, string, string, []transport.ContentBlock) error
}

func (f *goalRoundFullFake) Run(ctx context.Context, sessionID, turnID, text string) error {
	return f.run(ctx, sessionID, turnID, text)
}

func (f *goalRoundFullFake) RunContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	return f.content(ctx, sessionID, turnID, blocks)
}

func (f *goalRoundFullFake) Steer(ctx context.Context, sessionID, turnID, text string) error {
	return f.steer(ctx, sessionID, turnID, text)
}

func (f *goalRoundFullFake) SteerContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	return f.steerContent(ctx, sessionID, turnID, blocks)
}
