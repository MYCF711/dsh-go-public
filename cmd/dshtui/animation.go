package main

// 鲸鱼动画：开屏整帧序列 + settled 独立层规划器。
//
// 像素、开屏步序和毫秒节奏来自已验收 assets/frames.json；合成顺序与
// nextWhaleIdleStep 对照 MIT 上游 whaleLayers.ts / whaleIdle.ts。资产目录只读，
// 这里只 embed，不生成或重写任何素材。规划器全是 (state,input,now) 纯函数，
// 测试用毫秒假时钟推进，不靠 sleep；交互主循环也只有一个可停止的 timer。

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

//go:embed assets/frames.json assets/whale/*.txt
var whaleEmbed embed.FS

//go:embed assets/frames.json
var embeddedFramesJSON []byte

type whaleOpeningStep struct {
	frame int
	ms    int64
	loop  bool
}

type whaleAssetCatalog struct {
	names        []string
	frames       map[string][]string
	opening      map[string][]whaleOpeningStep
	openingTotal map[string]int64
}

var whaleOpeningIDs = []string{"classic", "heart", "sleep"}

var whaleAssets = mustLoadWhaleAssets()

func mustLoadWhaleAssets() whaleAssetCatalog {
	var manifest struct {
		Frames []struct {
			Index  int    `json:"index"`
			Name   string `json:"name"`
			File   string `json:"file"`
			SHA256 string `json:"sha256"`
		} `json:"frames"`
		Opening struct {
			Sequences map[string]struct {
				Loop    bool  `json:"loop"`
				TotalMS int64 `json:"total_ms"`
				Steps   []struct {
					Frame int   `json:"frame_index"`
					MS    int64 `json:"ms"`
				} `json:"steps"`
			} `json:"sequences"`
		} `json:"opening"`
	}
	if err := json.Unmarshal(embeddedFramesJSON, &manifest); err != nil {
		panic(fmt.Sprintf("内嵌 frames.json 无法解析: %v", err))
	}
	if len(manifest.Frames) != 22 {
		panic(fmt.Sprintf("内嵌鲸鱼帧数=%d，应为 22", len(manifest.Frames)))
	}
	catalog := whaleAssetCatalog{
		names:        make([]string, len(manifest.Frames)),
		frames:       make(map[string][]string, len(manifest.Frames)),
		opening:      make(map[string][]whaleOpeningStep, len(manifest.Opening.Sequences)),
		openingTotal: make(map[string]int64, len(manifest.Opening.Sequences)),
	}
	for i, item := range manifest.Frames {
		if item.Index != i || item.Name == "" {
			panic(fmt.Sprintf("内嵌鲸鱼帧索引损坏: %+v", item))
		}
		raw, err := whaleEmbed.ReadFile("assets/" + item.File)
		if err != nil {
			panic(fmt.Sprintf("内嵌鲸鱼帧 %s 读取失败: %v", item.Name, err))
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != item.SHA256 {
			panic(fmt.Sprintf("内嵌鲸鱼帧 %s 哈希=%s，验收值=%s", item.Name, got, item.SHA256))
		}
		text := string(raw)
		if !strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\n\n") {
			panic(fmt.Sprintf("内嵌鲸鱼帧 %s 必须恰有一个末尾 LF", item.Name))
		}
		rows := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		if len(rows) != 25 {
			panic(fmt.Sprintf("内嵌鲸鱼帧 %s 行数=%d，应为 25", item.Name, len(rows)))
		}
		for y, row := range rows {
			if len(row) != 40 {
				panic(fmt.Sprintf("内嵌鲸鱼帧 %s 第 %d 行宽=%d，应为 40", item.Name, y, len(row)))
			}
		}
		catalog.names[i] = item.Name
		catalog.frames[item.Name] = rows
	}
	for id, sequence := range manifest.Opening.Sequences {
		steps := make([]whaleOpeningStep, len(sequence.Steps))
		var total int64
		for i, step := range sequence.Steps {
			if step.Frame < 0 || step.Frame >= len(catalog.names) || step.MS <= 0 {
				panic(fmt.Sprintf("开屏 %s 第 %d 步损坏: %+v", id, i, step))
			}
			steps[i] = whaleOpeningStep{frame: step.Frame, ms: step.MS, loop: sequence.Loop}
			total += step.MS
		}
		if total != sequence.TotalMS {
			panic(fmt.Sprintf("开屏 %s 总时长=%d，清单=%d", id, total, sequence.TotalMS))
		}
		catalog.opening[id] = steps
		catalog.openingTotal[id] = total
	}
	for _, id := range whaleOpeningIDs {
		if len(catalog.opening[id]) == 0 {
			panic("缺少开屏序列 " + id)
		}
	}
	return catalog
}

// ---------------------------------------------------------------- 独立层合成

type whaleLayerPose struct {
	tail  int
	fin   int
	spout int
	heart int
	sleep int
	blink bool
}

var whaleLayerPaintOrder = []string{"eyes", "tail", "fin", "spout", "heart", "sleep"}

type whaleLayerFamily struct {
	name   string
	frames []string
}

var whaleLayerFamilies = []whaleLayerFamily{
	{name: "eyes", frames: []string{"blink"}},
	{name: "tail", frames: []string{"tail1", "tail2", "tail3", "tail4"}},
	{name: "fin", frames: []string{"fin1", "fin2"}},
	{name: "spout", frames: []string{"spout1", "spout2", "spout3", "spout4", "spout5", "spout6"}},
	{name: "heart", frames: []string{"heart1", "heart2", "heart3"}},
	{name: "sleep", frames: []string{"sleep1", "sleep2", "sleep3", "sleep4", "sleep5"}},
}

type whaleRegion [25]uint64

type whaleComposedFamily struct {
	region whaleRegion
	frames []string
}

var whaleComposerBase, whaleComposerFamilies = newWhaleComposer()

func newWhaleComposer() ([]string, []whaleComposedFamily) {
	standard := whaleAssets.frames["standard"]
	base := make([][]byte, 25)
	families := make([]whaleComposedFamily, len(whaleLayerFamilies))
	for y := 0; y < 25; y++ {
		base[y] = []byte(standard[y])
	}
	for i, family := range whaleLayerFamilies {
		families[i].frames = family.frames
		for _, name := range family.frames {
			for y := 0; y < 25; y++ {
				for x := 0; x < 40; x++ {
					if whaleAssets.frames[name][y][x] != standard[y][x] {
						families[i].region[y] |= uint64(1) << uint(x)
						base[y][x] = '.'
					}
				}
			}
		}
	}
	rows := make([]string, 25)
	for y := range rows {
		rows[y] = string(base[y])
	}
	return rows, families
}

func composeWhaleGrid(pose whaleLayerPose) []string {
	base, families := whaleComposerBase, whaleComposerFamilies
	grid := make([][]byte, 25)
	for y := range grid {
		grid[y] = []byte(base[y])
	}
	values := []int{boolWhaleLayer(pose.blink), pose.tail, pose.fin, pose.spout, pose.heart, pose.sleep}
	standard := whaleAssets.frames["standard"]
	for i, family := range families {
		frame := standard
		if values[i] > 0 && values[i] <= len(family.frames) {
			frame = whaleAssets.frames[family.frames[values[i]-1]]
		}
		for y := 0; y < 25; y++ {
			for x := 0; x < 40; x++ {
				if family.region[y]&(uint64(1)<<uint(x)) != 0 {
					grid[y][x] = frame[y][x]
				}
			}
		}
	}
	out := make([]string, 25)
	for y := range out {
		out[y] = string(grid[y])
	}
	return out
}

func boolWhaleLayer(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- idle 纯规划器

const (
	whaleTickMS          int64 = 120
	whaleTailHoldWorking       = 2 * whaleTickMS
	whaleTailHoldIdle          = 3 * whaleTickMS
	whaleFinHold               = 2 * whaleTickMS
	whaleBlinkHold             = 1 * whaleTickMS
	whaleBlinkGapWorking       = 14 * whaleTickMS
	whaleBlinkGapIdle          = 42 * whaleTickMS
	whaleTailGap               = 25 * whaleTickMS
	whaleFinGap                = 16 * whaleTickMS
	whaleSleepHold             = 3 * whaleTickMS
	whaleHeartHold             = 3 * whaleTickMS
	whaleSleepDelay            = int64(10000)
)

var (
	whaleTailSequence  = []int{1, 2, 3, 4, 3, 2, 1}
	whaleFinSequence   = []int{1, 2, 1}
	whaleHeartSequence = []int{1, 2, 3}
)

type whaleIdleState struct {
	tailStep       int
	tailHoldUntil  int64
	thumpAt        int64
	finStep        int
	finHoldUntil   int64
	flutterAt      int64
	blinkAt        int64
	blinkUntil     int64
	heartStep      int
	heartHoldUntil int64
	asleep         bool
	sleepStep      int
	sleepHoldUntil int64
	sleepAt        int64
}

type whaleIdleInput struct {
	working bool
	heart   bool
}

type whaleIdleStep struct {
	state   whaleIdleState
	pose    whaleLayerPose
	delayMs int64
}

func initialWhaleIdleState(now int64) whaleIdleState {
	return whaleIdleState{
		tailStep: -1, thumpAt: now + whaleTailGap,
		finStep: -1, flutterAt: now + whaleFinGap,
		blinkAt:   now + whaleBlinkGapIdle,
		heartStep: -1,
		sleepStep: -1, sleepAt: now + whaleSleepDelay,
	}
}

type whaleLimbStep struct {
	step      int
	holdUntil int64
	passAt    int64
}

func advanceWhaleLimb(step int, holdUntil, passAt int64, sequence []int,
	holdWorking, holdIdle, passGap int64, continuous bool, now int64) whaleLimbStep {
	hold := holdIdle
	if continuous {
		hold = holdWorking
	}
	if continuous {
		if step < 0 {
			return whaleLimbStep{step: 0, holdUntil: now + hold, passAt: now + passGap}
		}
		if now >= holdUntil {
			return whaleLimbStep{step: (step + 1) % len(sequence), holdUntil: now + hold, passAt: now + passGap}
		}
		return whaleLimbStep{step: step, holdUntil: holdUntil, passAt: passAt}
	}
	if step >= 0 {
		if now >= holdUntil {
			if step >= len(sequence)-1 {
				return whaleLimbStep{step: -1, passAt: now + passGap}
			}
			return whaleLimbStep{step: step + 1, holdUntil: now + hold, passAt: passAt}
		}
		return whaleLimbStep{step: step, holdUntil: holdUntil, passAt: passAt}
	}
	if now >= passAt {
		return whaleLimbStep{step: 0, holdUntil: now + hold, passAt: passAt}
	}
	return whaleLimbStep{step: -1, passAt: passAt}
}

func nextWhaleIdleStep(prev whaleIdleState, input whaleIdleInput, now int64) whaleIdleStep {
	heartStep := prev.heartStep
	heartHoldUntil := prev.heartHoldUntil
	if input.heart {
		heartStep = 0
		heartHoldUntil = now + whaleHeartHold
	} else if heartStep >= 0 && now >= heartHoldUntil {
		heartStep++
		if heartStep >= len(whaleHeartSequence) {
			heartStep = -1
		} else {
			heartHoldUntil = now + whaleHeartHold
		}
	}

	asleep := prev.asleep
	sleepStep := prev.sleepStep
	sleepHoldUntil := prev.sleepHoldUntil
	sleepAt := prev.sleepAt
	if input.working || input.heart {
		if asleep || sleepStep >= 0 {
			asleep = false
			sleepStep = -1
		}
		sleepAt = now + whaleSleepDelay
	} else if !asleep && now >= sleepAt {
		asleep = true
		sleepStep = 0
		sleepHoldUntil = now + whaleSleepHold
	} else if asleep && now >= sleepHoldUntil {
		if sleepStep >= 5 {
			sleepStep = 1
		} else {
			sleepStep++
		}
		sleepHoldUntil = now + whaleSleepHold
	}

	tail := advanceWhaleLimb(prev.tailStep, prev.tailHoldUntil, prev.thumpAt,
		whaleTailSequence, whaleTailHoldWorking, whaleTailHoldIdle, whaleTailGap, input.working, now)
	fin := advanceWhaleLimb(prev.finStep, prev.finHoldUntil, prev.flutterAt,
		whaleFinSequence, whaleFinHold, whaleFinHold, whaleFinGap, input.working, now)

	blinkAt := prev.blinkAt
	blinkUntil := prev.blinkUntil
	if now >= blinkAt {
		blinkUntil = now + whaleBlinkHold
		blinkAt = now + whaleBlinkGapIdle
		if input.working {
			blinkAt = now + whaleBlinkGapWorking
		}
	}
	blinking := now < blinkUntil

	pose := whaleLayerPose{
		tail:  sequenceWhaleLayer(tail.step, whaleTailSequence),
		fin:   sequenceWhaleLayer(fin.step, whaleFinSequence),
		spout: 0,
		heart: sequenceWhaleLayer(heartStep, whaleHeartSequence),
		blink: blinking,
	}
	if asleep {
		pose.sleep = sleepStep
		if pose.sleep < 0 {
			pose.sleep = 0
		}
	}

	dues := []int64{tail.passAt, fin.passAt, sleepAt, blinkAt}
	if tail.step >= 0 {
		dues[0] = tail.holdUntil
	}
	if fin.step >= 0 {
		dues[1] = fin.holdUntil
	}
	if heartStep >= 0 {
		dues = append(dues, heartHoldUntil)
	}
	if asleep {
		dues[2] = sleepHoldUntil
	}
	if blinking {
		dues = append(dues, blinkUntil)
	}
	next := int64(math.MaxInt64)
	for _, due := range dues {
		if due > now && due < next {
			next = due
		}
	}
	delay := int64(16)
	if next != math.MaxInt64 {
		delay = next - now
		if delay < 16 {
			delay = 16
		}
	}

	return whaleIdleStep{state: whaleIdleState{
		tailStep: tail.step, tailHoldUntil: tail.holdUntil, thumpAt: tail.passAt,
		finStep: fin.step, finHoldUntil: fin.holdUntil, flutterAt: fin.passAt,
		blinkAt: blinkAt, blinkUntil: blinkUntil,
		heartStep: heartStep, heartHoldUntil: heartHoldUntil,
		asleep: asleep, sleepStep: sleepStep, sleepHoldUntil: sleepHoldUntil, sleepAt: sleepAt,
	}, pose: pose, delayMs: delay}
}

func sequenceWhaleLayer(step int, sequence []int) int {
	if step < 0 || step >= len(sequence) {
		return 0
	}
	return sequence[step]
}

// ---------------------------------------------------------------- 开屏 + settled 状态

type whaleAnimation struct {
	openingID    string
	openingStep  int
	openingUntil int64
	settled      bool
	idle         whaleIdleState
	pose         whaleLayerPose
	nextDelay    int64
	lastWorking  *bool
	// forcedSprite 是测试可控帧注入；生产路径始终为空。
	forcedSprite []string
}

func newWhaleAnimationAt(now int64, seed uint64) *whaleAnimation {
	// SplitMix64 是本端明确记录的 seed→uniform-roll 派生规则；上游只要求
	// Math.random 均匀三选一，没有规定某个 seed 必须对应哪一段。
	roll := splitMix64(seed)
	id := whaleOpeningIDs[int(roll%uint64(len(whaleOpeningIDs)))]
	return newWhaleAnimationWithOpening(now, id)
}

func splitMix64(seed uint64) uint64 {
	x := seed + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func newWhaleAnimationWithOpening(now int64, id string) *whaleAnimation {
	sequence := whaleAssets.opening[id]
	if len(sequence) == 0 {
		panic("未知鲸鱼开屏序列 " + id)
	}
	return &whaleAnimation{
		openingID: id, openingStep: 0, openingUntil: now + sequence[0].ms,
		nextDelay: sequence[0].ms,
	}
}

func (w *whaleAnimation) currentSprite() []string {
	if w == nil {
		return whaleAssets.frames["standard"]
	}
	if w.forcedSprite != nil {
		return w.forcedSprite
	}
	if !w.settled {
		sequence := whaleAssets.opening[w.openingID]
		if w.openingStep >= 0 && w.openingStep < len(sequence) {
			return whaleAssets.frames[whaleAssets.names[sequence[w.openingStep].frame]]
		}
	}
	return composeWhaleGrid(w.pose)
}

func (w *whaleAnimation) advance(now int64, working, heart bool) int64 {
	if w == nil {
		return 0
	}
	sequence := whaleAssets.opening[w.openingID]
	for !w.settled && now >= w.openingUntil {
		w.openingStep++
		if w.openingStep >= len(sequence) {
			w.settled = true
			w.idle = initialWhaleIdleState(now)
			w.lastWorking = nil
			w.pose = whaleLayerPose{}
			break
		}
		// 用累计 deadline 追赶迟到的 timer；18/7/11 是固定上界，不会无界循环。
		w.openingUntil += sequence[w.openingStep].ms
	}
	if !w.settled {
		w.nextDelay = w.openingUntil - now
		if w.nextDelay < 1 {
			w.nextDelay = 1
		}
		return w.nextDelay
	}
	if w.lastWorking == nil || *w.lastWorking != working {
		// LogoV2 的 working effect 会重建 idle schedule；纯 next 函数本身不
		// 替调用方做这次重置。这里保留组件语义，避免 working→idle 接着旧趟。
		w.idle = initialWhaleIdleState(now)
		v := working
		w.lastWorking = &v
	}
	step := nextWhaleIdleStep(w.idle, whaleIdleInput{working: working, heart: heart}, now)
	w.idle = step.state
	w.pose = step.pose
	w.nextDelay = step.delayMs
	return w.nextDelay
}
