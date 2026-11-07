package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var expectedWhaleNames = []string{
	"standard", "blink", "fin1", "fin2", "spout1", "spout2", "spout3",
	"spout4", "spout5", "spout6", "tail1", "tail2", "tail3", "tail4",
	"heart1", "heart2", "heart3", "sleep1", "sleep2", "sleep3", "sleep4", "sleep5",
}

func spriteDigest(rows []string) string {
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// 22 帧的名字、索引、字节哈希与 25×40 几何全部钉住。运行时 sprite 来自 embed，
// 这里的摘要来自已验收 frames.json，避免“ loader 和自己生成的表相互证明”。
func TestWhaleCatalogMatchesAccepted22Frames(t *testing.T) {
	if got := len(whaleAssets.names); got != 22 {
		t.Fatalf("帧数=%d，应为 22", got)
	}
	if !reflect.DeepEqual(whaleAssets.names, expectedWhaleNames) {
		t.Fatalf("帧顺序不对：%v", whaleAssets.names)
	}
	var manifest struct {
		Frames []struct {
			Index  int    `json:"index"`
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(embeddedFramesJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	for i, name := range expectedWhaleNames {
		rows, ok := whaleAssets.frames[name]
		if !ok || len(rows) != 25 {
			t.Fatalf("%s：缺失或行数不是 25", name)
		}
		for y, row := range rows {
			if len(row) != 40 {
				t.Fatalf("%s 第 %d 行宽=%d，应为 40", name, y, len(row))
			}
		}
		if manifest.Frames[i].Index != i || manifest.Frames[i].Name != name {
			t.Fatalf("manifest 第 %d 项=%+v，应为 %s", i, manifest.Frames[i], name)
		}
		if got := spriteDigest(rows); got != manifest.Frames[i].SHA256 {
			t.Fatalf("%s 字节哈希=%s，验收值=%s", name, got, manifest.Frames[i].SHA256)
		}
	}
}

type openingWant struct {
	frames []string
	ms     []int64
	total  int64
}

var openingWants = map[string]openingWant{
	"classic": {
		frames: []string{"standard", "blink", "standard", "spout1", "spout2", "spout3", "spout4", "spout5", "spout6", "standard", "tail1", "tail2", "tail3", "tail4", "tail3", "tail2", "tail1", "standard"},
		ms:     []int64{400, 250, 300, 150, 150, 150, 150, 150, 150, 250, 150, 150, 150, 180, 150, 150, 150, 300},
		total:  3480,
	},
	"heart": {
		frames: []string{"standard", "heart1", "heart2", "heart3", "heart2", "heart1", "standard"},
		ms:     []int64{400, 350, 350, 450, 350, 350, 500},
		total:  2750,
	},
	"sleep": {
		frames: []string{"standard", "sleep1", "sleep2", "sleep3", "sleep4", "sleep5", "sleep4", "sleep3", "sleep2", "sleep1", "standard"},
		ms:     []int64{400, 250, 250, 250, 250, 300, 250, 250, 250, 250, 400},
		total:  3100,
	},
}

func TestOpeningSequencesExactStepsAndDurations(t *testing.T) {
	if !reflect.DeepEqual(whaleOpeningIDs, []string{"classic", "heart", "sleep"}) {
		t.Fatalf("开屏随机顺序=%v", whaleOpeningIDs)
	}
	for _, id := range whaleOpeningIDs {
		want := openingWants[id]
		seq := whaleAssets.opening[id]
		if len(seq) != len(want.frames) || len(want.frames) != len(want.ms) {
			t.Fatalf("%s 步数=%d，期望 %d", id, len(seq), len(want.frames))
		}
		var total int64
		for i, step := range seq {
			if got := whaleAssets.names[step.frame]; got != want.frames[i] || step.ms != want.ms[i] {
				t.Fatalf("%s 第 %d 步=%s/%dms，应为 %s/%dms", id, i, got, step.ms, want.frames[i], want.ms[i])
			}
			if step.loop {
				t.Fatalf("%s 第 %d 步不应循环", id, i)
			}
			total += step.ms
		}
		if total != want.total || whaleAssets.openingTotal[id] != want.total {
			t.Fatalf("%s 总时长=%d/manifest=%d，应为 %d", id, total, whaleAssets.openingTotal[id], want.total)
		}
	}
}

func TestOpeningFakeClockEveryDwellThenStandard(t *testing.T) {
	for _, id := range whaleOpeningIDs {
		t.Run(id, func(t *testing.T) {
			const start int64 = 123456
			a := newWhaleAnimationWithOpening(start, id)
			var now = start
			for i, step := range whaleAssets.opening[id] {
				if a.settled || a.openingStep != i {
					t.Fatalf("第 %d 步状态 settled=%v step=%d", i, a.settled, a.openingStep)
				}
				want := whaleAssets.frames[whaleAssets.names[step.frame]]
				if !reflect.DeepEqual(a.currentSprite(), want) {
					t.Fatalf("第 %d 步没有显示真实帧 %s", i, whaleAssets.names[step.frame])
				}
				if got := a.advance(now+step.ms-1, false, false); got != 1 || a.openingStep != i {
					t.Fatalf("第 %d 步提前 1ms 跳帧：delay=%d step=%d", i, got, a.openingStep)
				}
				now += step.ms
				a.advance(now, false, false)
			}
			if !a.settled || !reflect.DeepEqual(a.currentSprite(), whaleAssets.frames["standard"]) {
				t.Fatalf("结束后没有回 standard：settled=%v", a.settled)
			}
			if now-start != openingWants[id].total {
				t.Fatalf("假时钟走了 %dms，应为 %dms", now-start, openingWants[id].total)
			}
		})
	}
}

func TestOpeningSeedInjectionIsDeterministic(t *testing.T) {
	seen := map[string]bool{}
	for seed := uint64(0); seed < 64; seed++ {
		a := newWhaleAnimationAt(0, seed)
		b := newWhaleAnimationAt(999, seed)
		if a.openingID != b.openingID {
			t.Fatalf("seed %d 不可复现：%s / %s", seed, a.openingID, b.openingID)
		}
		seen[a.openingID] = true
	}
	for _, id := range whaleOpeningIDs {
		if !seen[id] {
			t.Fatalf("64 个 seed 没覆盖 %s", id)
		}
	}
}

// referenceCompose 用上游的文字算法独立算一份；生产实现可做位掩码与缓存，
// 但合成结果必须逐格等于这个简单参照。
func referenceCompose(pose whaleLayerPose) []string {
	standard := whaleAssets.frames["standard"]
	base := make([][]byte, 25)
	for y := range base {
		base[y] = []byte(standard[y])
	}
	families := []struct {
		value  int
		frames []string
	}{
		{boolLayer(pose.blink), []string{"blink"}},
		{pose.tail, []string{"tail1", "tail2", "tail3", "tail4"}},
		{pose.fin, []string{"fin1", "fin2"}},
		{pose.spout, []string{"spout1", "spout2", "spout3", "spout4", "spout5", "spout6"}},
		{pose.heart, []string{"heart1", "heart2", "heart3"}},
		{pose.sleep, []string{"sleep1", "sleep2", "sleep3", "sleep4", "sleep5"}},
	}
	regions := make([]map[[2]int]bool, len(families))
	for i, family := range families {
		regions[i] = map[[2]int]bool{}
		for _, name := range family.frames {
			for y := 0; y < 25; y++ {
				for x := 0; x < 40; x++ {
					if whaleAssets.frames[name][y][x] != standard[y][x] {
						regions[i][[2]int{x, y}] = true
						base[y][x] = '.'
					}
				}
			}
		}
	}
	for i, family := range families {
		name := "standard"
		if family.value > 0 {
			name = family.frames[family.value-1]
		}
		for p := range regions[i] {
			base[p[1]][p[0]] = whaleAssets.frames[name][p[1]][p[0]]
		}
	}
	out := make([]string, 25)
	for y := range out {
		out[y] = string(base[y])
	}
	return out
}

func boolLayer(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestWhaleLayersComposeIndependentlyInUpstreamOrder(t *testing.T) {
	if !reflect.DeepEqual(whaleLayerPaintOrder, []string{"eyes", "tail", "fin", "spout", "heart", "sleep"}) {
		t.Fatalf("绘制顺序=%v", whaleLayerPaintOrder)
	}
	if got := composeWhaleGrid(whaleLayerPose{}); !reflect.DeepEqual(got, whaleAssets.frames["standard"]) {
		t.Fatal("resting pose 不等于 standard")
	}
	single := []struct {
		pose  whaleLayerPose
		frame string
	}{
		{whaleLayerPose{blink: true}, "blink"},
		{whaleLayerPose{tail: 4}, "tail4"},
		{whaleLayerPose{fin: 2}, "fin2"},
		{whaleLayerPose{spout: 6}, "spout6"},
		{whaleLayerPose{heart: 3}, "heart3"},
		{whaleLayerPose{sleep: 5}, "sleep5"},
	}
	for _, tc := range single {
		got := composeWhaleGrid(tc.pose)
		if !reflect.DeepEqual(got, referenceCompose(tc.pose)) {
			t.Fatalf("单层 %s 不等于上游 composeWhaleGrid 算法", tc.frame)
		}
		// spout 与后续 heart/sleep 区域有 9 个交叠格；上游算法让后层
		// resting standard 覆盖它们，所以 spout6 是注释所称“整帧相等”的例外。
		if tc.frame == "spout6" {
			continue
		}
		if !reflect.DeepEqual(got, whaleAssets.frames[tc.frame]) {
			t.Fatalf("无后续覆盖的单层 %s 不等于真实整帧", tc.frame)
		}
	}
	poses := []whaleLayerPose{
		{tail: 4, fin: 2, spout: 6, heart: 3, sleep: 5, blink: true},
		{tail: 2, fin: 1, heart: 1, sleep: 3},
		{tail: 3, sleep: 4, blink: true},
	}
	for _, pose := range poses {
		if got, want := composeWhaleGrid(pose), referenceCompose(pose); !reflect.DeepEqual(got, want) {
			t.Fatalf("独立层合成与上游算法不同：pose=%+v", pose)
		}
	}
}

func idleAt(prev whaleIdleState, now int64, working, heart bool) (whaleIdleState, whaleLayerPose, int64) {
	step := nextWhaleIdleStep(prev, whaleIdleInput{working: working, heart: heart}, now)
	return step.state, step.pose, step.delayMs
}

func TestIdlePlanesHaveIndependentCadenceAndLoops(t *testing.T) {
	state := initialWhaleIdleState(0)
	state, pose, delay := idleAt(state, 0, false, false)
	if pose != (whaleLayerPose{}) || delay != 1920 {
		t.Fatalf("idle 初始 pose=%+v delay=%d，应为 rest/1920", pose, delay)
	}
	// fin 单趟 1→2→1→0，idle 每步 240ms，结束后重新计 1920ms 间隔。
	state, pose, _ = idleAt(state, 1920, false, false)
	if pose.fin != 1 || pose.tail != 0 {
		t.Fatalf("1920ms fin 应独立启动：%+v", pose)
	}
	state, pose, _ = idleAt(state, 2160, false, false)
	if pose.fin != 2 {
		t.Fatalf("2160ms fin 应为 2：%+v", pose)
	}
	state, pose, _ = idleAt(state, 2400, false, false)
	if pose.fin != 1 {
		t.Fatalf("2400ms fin 应回到 1：%+v", pose)
	}
	state, pose, _ = idleAt(state, 2640, false, false)
	if pose.fin != 0 || state.flutterAt != 4560 {
		t.Fatalf("2640ms fin 应休息且 4560ms 再触发：pose=%+v at=%d", pose, state.flutterAt)
	}
	// tail 不被 fin 的四个切换带着走；3000ms 才启动自己的 idle 单趟。
	state, pose, _ = idleAt(state, 3000, false, false)
	if pose.tail != 1 {
		t.Fatalf("tail 应独立在 3000ms 启动：%+v", pose)
	}
	state, pose, _ = idleAt(state, 3359, false, false)
	if pose.tail != 1 {
		t.Fatalf("idle tail 应保持 360ms：%+v", pose)
	}
	state, pose, _ = idleAt(state, 3360, false, false)
	if pose.tail != 2 {
		t.Fatalf("idle tail 360ms 后应为 2：%+v", pose)
	}

	// working 两肢都从 1 开始，240ms 连续循环，不在一趟后回 0。
	working := initialWhaleIdleState(0)
	working, pose, delay = idleAt(working, 0, true, false)
	if pose.tail != 1 || pose.fin != 1 || delay != 240 {
		t.Fatalf("working 初始 pose=%+v delay=%d", pose, delay)
	}
	for i, now := range []int64{240, 480, 720, 960, 1200, 1440, 1680} {
		working, pose, _ = idleAt(working, now, true, false)
		wantTail := []int{2, 3, 4, 3, 2, 1, 1}[i]
		if pose.tail != wantTail || pose.fin == 0 {
			t.Fatalf("working %dms pose=%+v，tail 应为 %d 且 fin 不停", now, pose, wantTail)
		}
	}
}

func TestSleepEntryWrapAndHeartOrWorkWake(t *testing.T) {
	state := initialWhaleIdleState(0)
	state, pose, _ := idleAt(state, 9999, false, false)
	if pose.sleep != 0 || state.asleep {
		t.Fatalf("9999ms 不该提前睡：%+v", pose)
	}
	state, pose, _ = idleAt(state, 10000, false, false)
	if !state.asleep || pose.sleep != 0 {
		t.Fatalf("10000ms 应先进入 sleep 层 0：state=%+v pose=%+v", state, pose)
	}
	for i, now := range []int64{10360, 10720, 11080, 11440, 11800, 12160} {
		state, pose, _ = idleAt(state, now, false, false)
		want := []int{1, 2, 3, 4, 5, 1}[i]
		if pose.sleep != want {
			t.Fatalf("sleep %dms 层=%d，应为 %d（sleep5 后只能回 sleep1）", now, pose.sleep, want)
		}
	}
	// heart 在循环中唤醒并叠加自己的心层；再次触发从 heart1 重启。
	state, pose, _ = idleAt(state, 12520, false, true)
	if state.asleep || pose.sleep != 0 || pose.heart != 1 || state.sleepAt != 22520 {
		t.Fatalf("heart 没唤醒或没重排睡眠：state=%+v pose=%+v", state, pose)
	}
	state, pose, _ = idleAt(state, 12880, false, false)
	if pose.heart != 2 {
		t.Fatalf("heart 360ms 后应为 2：%+v", pose)
	}
	state, pose, _ = idleAt(state, 13000, false, true)
	if pose.heart != 1 || state.heartHoldUntil != 13360 {
		t.Fatalf("heart 重触发没从 1 重启：state=%+v pose=%+v", state, pose)
	}
	for _, now := range []int64{13360, 13720, 14080} {
		state, pose, _ = idleAt(state, now, false, false)
	}
	if pose.heart != 0 {
		t.Fatalf("heart 单趟结束应回层 0：%+v", pose)
	}

	asleep := initialWhaleIdleState(0)
	asleep, _, _ = idleAt(asleep, 10000, false, false)
	asleep, pose, _ = idleAt(asleep, 10360, false, false)
	if pose.sleep != 1 {
		t.Fatalf("前置：应已进入 Z 循环：%+v", pose)
	}
	asleep, pose, _ = idleAt(asleep, 11000, true, false)
	if asleep.asleep || pose.sleep != 0 || pose.tail == 0 || pose.fin == 0 {
		t.Fatalf("working 应唤醒并保持两肢各自连续运动：state=%+v pose=%+v", asleep, pose)
	}
}

func TestBlinkIsAnIndependentOverlay(t *testing.T) {
	state := initialWhaleIdleState(0)
	state.blinkAt = 0
	state.tailStep = 0
	state.tailHoldUntil = 240
	state, pose, _ := idleAt(state, 0, true, false)
	if !pose.blink || pose.tail != 1 || state.blinkUntil != 120 || state.blinkAt != 1680 {
		t.Fatalf("working blink 应叠加在 tail 上且间隔 1680：state=%+v pose=%+v", state, pose)
	}
	state, pose, _ = idleAt(state, 120, true, false)
	if pose.blink {
		t.Fatalf("blink 只能保持 120ms：%+v", pose)
	}
	state.blinkAt = 5040
	state, pose, _ = idleAt(state, 5040, false, false)
	if !pose.blink || state.blinkAt != 10080 {
		t.Fatalf("idle blink 间隔应为 5040：state=%+v pose=%+v", state, pose)
	}
}

func TestIdleSchedulerAlwaysBoundedWithoutRealSleeps(t *testing.T) {
	state := initialWhaleIdleState(0)
	now := int64(0)
	for i := 0; i < 3000; i++ {
		working := i%37 < 11
		heart := i%101 == 0
		step := nextWhaleIdleStep(state, whaleIdleInput{working: working, heart: heart}, now)
		if step.delayMs < 16 || step.delayMs > 10000 {
			t.Fatalf("第 %d 步 delay=%d 越界", i, step.delayMs)
		}
		state = step.state
		now += step.delayMs
	}
}

func newAnimatedTestApp(w, h int, intro string) (*app, *fakeClock) {
	a := newTestApp(w, h)
	clk := &fakeClock{t: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)}
	a.now = clk.now
	a.animateWhale = true
	a.whaleSeed = 7
	if intro == "" {
		a.restartWhaleAnimation()
	} else {
		a.whale = newWhaleAnimationWithOpening(a.clock().UnixMilli(), intro)
	}
	return a, clk
}

func settleAppWhale(a *app) {
	now := a.clock().UnixMilli()
	a.whale.openingStep = len(whaleAssets.opening[a.whale.openingID])
	a.whale.settled = true
	a.whale.idle = initialWhaleIdleState(now)
	a.whale.lastWorking = nil
	a.whale.pose = whaleLayerPose{}
	a.whale.advance(now, false, false)
}

func TestAppUsesRealFramesAndResizePausesWithoutReroll(t *testing.T) {
	t.Setenv("DSH_TUI_SPLASH_FONT", "bold") // 本测试钉 120 列鲸鱼几何，不随 daily 阈值漂移。
	a, clk := newAnimatedTestApp(120, 36, "heart")
	id := a.whale.openingID
	a.advanceWhaleAnimation()
	if !a.whaleVisible() {
		t.Fatal("120×36 空会话应显示鲸鱼")
	}
	if _, ok := a.whaleWakeup(); !ok {
		t.Fatal("可见开屏没有动画唤醒期限")
	}
	a.whale.advance(clk.t.UnixMilli()+400, false, false)
	if got, want := a.currentWhaleSprite(), whaleAssets.frames["heart1"]; !reflect.DeepEqual(got, want) {
		t.Fatal("首屏没有消费 heart1 真实帧")
	}
	for _, size := range [][2]int{{80, 24}, {40, 10}, {120, 36}} {
		a.scr.width, a.scr.height = size[0], size[1]
		visible := a.whaleVisible()
		_, wake := a.whaleWakeup()
		if wake != visible {
			t.Fatalf("%dx%d visible=%v wake=%v：隐藏后仍有后台动画", size[0], size[1], visible, wake)
		}
		if size[0] == 120 && (!visible || !wake) {
			t.Fatal("resize 回宽屏后动画没恢复")
		}
		for i, row := range a.view() {
			if got := displayWidth(row); got > size[0] {
				t.Fatalf("%dx%d 第 %d 行宽 %d 溢出", size[0], size[1], i, got)
			}
		}
	}
	if a.whale.openingID != id || a.whale.openingStep != 1 {
		t.Fatalf("resize 重摇或串帧：id=%s step=%d", a.whale.openingID, a.whale.openingStep)
	}
}

func TestAppBusyConnectionTranscriptAndQuitControlWakeup(t *testing.T) {
	t.Setenv("DSH_TUI_SPLASH_FONT", "bold") // 只固定标题几何，busy/连接/退出仍走真实事件链。
	a, _ := newAnimatedTestApp(120, 36, "classic")
	settleAppWhale(a)
	// 不直接写 busy：走 TUI-SEND-ACK 的真实 accepted 结算。
	a.pendingSend = &pendingPrompt{id: 1, sid: a.sid, text: "工作"}
	a.handlePromptResult(promptOutcome{id: 1, sid: a.sid, text: "工作",
		ack: promptAck{Accepted: true, TurnID: "turn-animation"}})
	a.advanceWhaleAnimation()
	if !a.busy || a.whale.pose.tail != 1 || a.whale.pose.fin != 1 {
		t.Fatalf("真实 accepted/busy 没切到 working 节奏：busy=%v pose=%+v", a.busy, a.whale.pose)
	}
	// 同样走 DISCONNECT-CLOCK 的事件入口，不直接翻 connectionDown。
	a.connectionSubscription, a.connectionGeneration = 1, 1
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 1, generation: 1})
	if !a.connectionDown {
		t.Fatal("前置：wsDown 没进入断线态")
	}
	if _, ok := a.whaleWakeup(); ok {
		t.Fatal("断线期间仍有后台动画唤醒")
	}
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 1, generation: 2})
	a.handleFrame(ServerFrame{Type: "turn/done", Data: json.RawMessage(`{"turnId":"turn-animation"}`)})
	a.advanceWhaleAnimation()
	if a.busy || a.whale.pose.tail != 0 || a.whale.pose.fin != 0 {
		t.Fatalf("真实 turn/done 回 idle 应重建调度：busy=%v pose=%+v", a.busy, a.whale.pose)
	}
	a.pushRole(roleAssistant, "正文")
	if _, ok := a.whaleWakeup(); ok {
		t.Fatal("header 被正文替换后仍有动画唤醒")
	}
	a.lines, a.meta = nil, nil
	a.quit = true
	if _, ok := a.whaleWakeup(); ok {
		t.Fatal("退出后仍有动画唤醒")
	}
}

func TestHeartInputWakesSleepAndMouseDoesNotEnterDraft(t *testing.T) {
	t.Setenv("DSH_TUI_SPLASH_FONT", "bold") // 点击命中需要 120 列可见鲸鱼；daily shadow 需要 130 列。
	a, clk := newAnimatedTestApp(120, 36, "classic")
	settleAppWhale(a)
	a.whale.advance(clk.t.UnixMilli()+10000+360, false, false)
	if !a.whale.idle.asleep {
		t.Fatal("前置：鲸鱼应已睡着")
	}
	for _, r := range "abc" {
		a.handleRune(r)
	}
	a.handleSpecial("mouse:<0;12;4M")
	if got := string(a.input); got != "abc" {
		t.Fatalf("鼠标字节漏进输入行：%q", got)
	}
	if a.whale.idle.asleep || a.whale.pose.heart != 1 || a.whale.pose.sleep != 0 {
		t.Fatalf("点击没有唤醒并触发 heart1：state=%+v pose=%+v", a.whale.idle, a.whale.pose)
	}
	a.handleSpecial("heart")
	if a.whale.pose.heart != 1 || string(a.input) != "abc" {
		t.Fatalf("heart 重触发或输入隔离不对：pose=%+v input=%q", a.whale.pose, string(a.input))
	}
}

func TestSGRMouseDecodesAsSpecialNotRunes(t *testing.T) {
	rch := make(chan rune, 8)
	sch := make(chan string, 8)
	readKeysFrom(strings.NewReader("\x1b[<0;12;4M"), rch, sch)
	close(rch)
	close(sch)
	if len(rch) != 0 {
		t.Fatal("SGR 鼠标序列漏成普通字符")
	}
	if got := <-sch; got != "mouse:<0;12;4M" {
		t.Fatalf("SGR 鼠标解码=%q", got)
	}
}

func TestScreenMouseModeIsEnabledAndRestored(t *testing.T) {
	var out strings.Builder
	s := newScreenTo(&out, 120, 36)
	s.enterAlt()
	entered := out.String()
	if !strings.Contains(entered, "\x1b[?1000h") || !strings.Contains(entered, "\x1b[?1006h") {
		t.Fatalf("进入界面没开启按钮/SGR 鼠标：%q", entered)
	}
	out.Reset()
	s.Leave()
	left := out.String()
	if !strings.Contains(left, "\x1b[?1006l") || !strings.Contains(left, "\x1b[?1000l") {
		t.Fatalf("退出界面没恢复鼠标模式：%q", left)
	}
}

func TestAll22FramesAndLayerPosesFitScreen(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		for _, name := range expectedWhaleNames {
			a := newTestApp(size[0], size[1])
			a.scr.width, a.scr.height = size[0], size[1]
			a.animateWhale = true
			a.whale = newWhaleAnimationWithOpening(0, "classic")
			a.whale.forcedSprite = whaleAssets.frames[name]
			rows := a.view()
			if len(rows) != size[1] {
				t.Fatalf("%dx%d/%s 行数=%d", size[0], size[1], name, len(rows))
			}
			for i, row := range rows {
				if got := displayWidth(row); got > size[0] {
					t.Fatalf("%dx%d/%s 第 %d 行宽=%d", size[0], size[1], name, i, got)
				}
			}
		}
		a, _ := newAnimatedTestApp(size[0], size[1], "classic")
		settleAppWhale(a)
		a.whale.pose = whaleLayerPose{tail: 4, fin: 2, heart: 3, sleep: 5, blink: true}
		for i, row := range a.view() {
			if got := displayWidth(row); got > size[0] {
				t.Fatalf("%dx%d/合成帧第 %d 行宽=%d", size[0], size[1], i, got)
			}
		}
	}
}

func TestDumpAndScriptSnapshotsStayDeterministicStandard(t *testing.T) {
	first, err := dumpScreenSnapshot("120x36", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dumpScreenSnapshot("120x36", false)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("--dump-screen 默认快照不确定")
	}
	a := newTestApp(120, 36)
	if a.whale != nil || a.animateWhale {
		t.Fatal("script/test 零值 app 不应自动开启动画")
	}
	if got := a.currentWhaleSprite(); !reflect.DeepEqual(got, whaleAssets.frames["standard"]) {
		t.Fatal("script 默认不是 standard")
	}
	report := snapshotOf(a, "default", "manual", 0)
	standard := newTestApp(120, 36).view()
	if !reflect.DeepEqual(report.Lines, plainRows(standard)) {
		t.Fatal("script 快照与确定性 standard 路径不同")
	}
}

func plainRows(rows []string) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = strings.TrimRight(rows[i], " ")
	}
	return out
}
