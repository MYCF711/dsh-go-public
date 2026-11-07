package main

// 官方 `session.state.queue` / `session.updateQueue` 在**真实轮次服务**上的端到端。
//
// 这一份证明的是另外两份证明不了的东西：
//   - `internal/transport` 的假源只证明协议翻译对；
//   - 这里用真实 `promptService` + 真实 `officialSessionHost`，证明
//     "排队 → state 读得到 → edit 改的是**真会发出去**的内容 → steer 真的塞进
//     正在跑的那一轮"，并且错误分类来自真实队列状态而不是编出来的。
//
// 不碰模型：runner 是可控假 runner，但它实现的是生产上的同一组接口
// （turnRunner / steerTurnRunner / contentTurnRunner / steerContentTurnRunner）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/transport"
)

// queueRecordingRunner 记录"真的跑了什么内容"与"steer 注入了什么"。
//
// run 一直挂到 release 关闭（模拟"上一轮还没结束"），所以第二次提交只能排队。
type queueRecordingRunner struct {
	mu      sync.Mutex
	runs    []string
	steers  []string
	turnIDs []string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

// open 放行所有 run（幂等：测试体与 cleanup 都会调）。
func (r *queueRecordingRunner) open() { r.once.Do(func() { close(r.release) }) }

func newQueueRecordingRunner() *queueRecordingRunner {
	return &queueRecordingRunner{started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (r *queueRecordingRunner) Run(ctx context.Context, _, turnID, text string) error {
	r.mu.Lock()
	r.runs = append(r.runs, text)
	r.turnIDs = append(r.turnIDs, turnID)
	r.mu.Unlock()
	r.started <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}

func (r *queueRecordingRunner) RunContent(ctx context.Context, _, turnID string, blocks []transport.ContentBlock) error {
	return r.Run(ctx, "", turnID, joinQueueTextBlocks(blocks))
}

func (r *queueRecordingRunner) Steer(_ context.Context, _, turnID, text string) error {
	r.mu.Lock()
	r.steers = append(r.steers, turnID+"|"+text)
	r.mu.Unlock()
	return nil
}

func (r *queueRecordingRunner) SteerContent(ctx context.Context, sid, turnID string, blocks []transport.ContentBlock) error {
	return r.Steer(ctx, sid, turnID, joinQueueTextBlocks(blocks))
}

func (r *queueRecordingRunner) snapshot() (runs, steers, turnIDs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.runs...), append([]string(nil), r.steers...), append([]string(nil), r.turnIDs...)
}

func waitQueueRunnerStarted(t *testing.T, runner *queueRecordingRunner) {
	t.Helper()
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner 没有启动")
	}
}

// queueContentTexts 取出 LiveQueue 里每一项的文本内容（断言用）。
func queueContentTexts(t *testing.T, items []transport.OfficialQueueItem) []string {
	t.Helper()
	out := make([]string, 0, len(items))
	for _, item := range items {
		var parts []string
		for _, raw := range item.Content {
			var block struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &block); err != nil {
				t.Fatalf("queue content 不是 JSON 块：%v", err)
			}
			if block.Type == "text" {
				parts = append(parts, block.Text)
			}
		}
		out = append(out, strings.Join(parts, "\n"))
	}
	return out
}

// 排队 → state 读得到 → edit 生效（下一轮**真的**按新内容发）。
func TestOfficialQueueEditReachesTheRealRun(t *testing.T) {
	runner := newQueueRecordingRunner()
	prompts := newPromptService(runner)
	host := &officialSessionHost{prompts: prompts}
	t.Cleanup(func() {
		runner.open()
		_ = prompts.Cancel("s-q")
	})

	if _, err := prompts.SubmitPlacement(context.Background(), "s-q", "第一轮", string(transport.PlacementTurn)); err != nil {
		t.Fatalf("起第一轮：%v", err)
	}
	waitQueueRunnerStarted(t, runner)

	queued, err := prompts.SubmitPlacement(context.Background(), "s-q", "原始的第二轮", string(transport.PlacementFollowup))
	if err != nil {
		t.Fatalf("排队第二轮：%v", err)
	}
	// 冷会话（还没起轮）必须回 null：官方 readState 对没有 agent 的会话就是 null。
	if cold, err := host.LiveQueue("s-q-cold"); err != nil || cold != nil {
		t.Fatalf("冷会话的 queue 必须是 null：%v %v", cold, err)
	}
	items, err := host.LiveQueue("s-q")
	if err != nil {
		t.Fatalf("LiveQueue: %v", err)
	}
	if len(items) != 1 || items[0].ID != queued.TurnID {
		t.Fatalf("queue 必须是真实排队项 %q：%+v", queued.TurnID, items)
	}
	if items[0].Placement != "queued" {
		t.Fatalf("placement 必须是 queued：%q", items[0].Placement)
	}
	if got := queueContentTexts(t, items); len(got) != 1 || got[0] != "原始的第二轮" {
		t.Fatalf("state.queue 必须带真实待发内容：%v", got)
	}

	// edit 换成新文本。
	newContent := []json.RawMessage{json.RawMessage(`{"type":"text","text":"改过的第二轮"}`)}
	if err := host.EditQueueItem("s-q", queued.TurnID, newContent); err != nil {
		t.Fatalf("EditQueueItem: %v", err)
	}
	items, err = host.LiveQueue("s-q")
	if err != nil {
		t.Fatalf("LiveQueue after edit: %v", err)
	}
	if got := queueContentTexts(t, items); len(got) != 1 || got[0] != "改过的第二轮" {
		t.Fatalf("edit 之后 state.queue 必须看到新内容：%v", got)
	}

	// 放行第一轮：第二轮真的启动，而且跑的是**改过**的正文。
	runner.open()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runs, _, _ := runner.snapshot()
		if len(runs) >= 2 {
			if runs[1] != "改过的第二轮" {
				t.Fatalf("edit 必须改到真会发出去的内容，实际跑了 %q", runs[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("第二轮没有真的启动：runs=%v", runs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// steer 真的塞进正在跑的那一轮，并且从队列里消失。
func TestOfficialQueueSteerInjectsIntoCurrentTurn(t *testing.T) {
	runner := newQueueRecordingRunner()
	prompts := newPromptService(runner)
	host := &officialSessionHost{prompts: prompts}
	t.Cleanup(func() {
		runner.open()
		_ = prompts.Cancel("s-s")
	})

	first, err := prompts.SubmitPlacement(context.Background(), "s-s", "当前轮", string(transport.PlacementTurn))
	if err != nil {
		t.Fatalf("起当前轮：%v", err)
	}
	waitQueueRunnerStarted(t, runner)
	queued, err := prompts.SubmitPlacement(context.Background(), "s-s", "本来要排队的", string(transport.PlacementFollowup))
	if err != nil {
		t.Fatalf("排队：%v", err)
	}

	if err := host.SteerQueueItem("s-s", queued.TurnID); err != nil {
		t.Fatalf("SteerQueueItem: %v", err)
	}
	_, steers, _ := runner.snapshot()
	if len(steers) != 1 {
		t.Fatalf("steer 必须真的注入一次：%v", steers)
	}
	if want := first.TurnID + "|本来要排队的"; steers[0] != want {
		t.Fatalf("steer 必须注入**当前轮**（%s）且带上排队项内容，实际 %q", first.TurnID, steers[0])
	}
	items, err := host.LiveQueue("s-s")
	if err != nil {
		t.Fatalf("LiveQueue: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("steer 成功之后它不再是排队项：%+v", items)
	}
	// 当前轮必须还在（steer 不许把正在跑的一轮弄没）。
	if !prompts.Busy("s-s") {
		t.Fatal("steer 之后当前轮必须还在")
	}
}

// 真实错误分类：没有当前轮 / 已经不在队列里 / now 保留项，三种都不是 200。
func TestOfficialQueueErrorsComeFromRealQueueState(t *testing.T) {
	runner := newQueueRecordingRunner()
	prompts := newPromptService(runner)
	host := &officialSessionHost{prompts: prompts}
	t.Cleanup(func() {
		runner.open()
		_ = prompts.Cancel("s-e")
	})

	// 队列空：steer 不存在的项 → ErrNoSuchQueueItem。
	if err := host.SteerQueueItem("s-e", "turn-404"); !errors.Is(err, transport.ErrNoSuchQueueItem) {
		t.Fatalf("不存在的项必须 ErrNoSuchQueueItem，实际 %v", err)
	}
	// edit 不存在的项 → ErrNoSuchQueueItem。
	if err := host.EditQueueItem("s-e", "turn-404",
		[]json.RawMessage{json.RawMessage(`{"type":"text","text":"x"}`)}); !errors.Is(err, transport.ErrNoSuchQueueItem) {
		t.Fatalf("edit 不存在的项必须 ErrNoSuchQueueItem，实际 %v", err)
	}
	// 非 text 块的 edit → 参数错（官方 QUEUE_EDIT_NON_TEXT）。
	if err := host.EditQueueItem("s-e", "turn-404",
		[]json.RawMessage{json.RawMessage(`{"type":"image","text":""}`)}); !errors.Is(err, transport.ErrPlacementInvalid) {
		t.Fatalf("非 text 块 edit 必须 ErrPlacementInvalid，实际 %v", err)
	}

	// 没有 live turn 但项在队列里：官方是 steer-unavailable，不是 no-such-item。
	// 造这个状态：先排一项，再让当前轮收工（队列项会立即接班——所以这里直接
	// 用只排队不启动的方式：当前轮还在时排队，然后把当前轮撤掉是不可能的
	// （撤了就会 drain）。改用「队列里有一项 + 没有 live」的瞬间不存在于真实
	// 状态机，因此这里只钉一个真实可达的分支：项在队列里且当前轮在跑 → 成功；
	// 项不在队列里或没有当前轮 → 上面两条错误分类）。
	if _, err := prompts.SubmitPlacement(context.Background(), "s-e", "起轮", string(transport.PlacementFollowup)); err != nil {
		t.Fatalf("起轮：%v", err)
	}
	waitQueueRunnerStarted(t, runner)
	if err := host.SteerQueueItem("s-e", "turn-404"); !errors.Is(err, transport.ErrNoSuchQueueItem) {
		t.Fatalf("有当前轮时 steer 不存在的项仍必须 ErrNoSuchQueueItem，实际 %v", err)
	}
}
