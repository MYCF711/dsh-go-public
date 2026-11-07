package main

import (
	"strconv"
	"strings"
	"time"
)

// 本文件只负责把纯动画规划器接到 app。timer 本身住在 app.run 的主事件循环，
// 不在这里起 goroutine；所有状态仍只由主循环（或单测当前 goroutine）修改。

func (a *app) restartWhaleAnimation() {
	if !a.animateWhale {
		return
	}
	a.whale = newWhaleAnimationAt(a.clock().UnixMilli(), a.whaleSeed)
	// 同一进程的新 logo mount 取确定性 seed 流的下一项；生产初 seed 随机，
	// 测试仍可从固定初 seed 逐次重放，而不是每次 mount 都重复同一段。
	a.whaleSeed = splitMix64(a.whaleSeed)
	a.pendingHeart = false
}

func (a *app) currentWhaleSprite() []string {
	if !a.animateWhale || a.whale == nil {
		return whaleAssets.frames["standard"]
	}
	return a.whale.currentSprite()
}

// splashChromeAt 与 splashFirstScreen 走同一个 full→minimal→plain 顺序，
// 供可见性和点击命中测试复用，避免“以为画了鲸鱼，实际已降级成一行字”。
func (a *app) splashChromeAt() (splashChrome, bool) {
	body := a.bodyHeight()
	if body < 1 {
		body = 1
	}
	info := a.splashInfo()
	for _, chrome := range []splashChrome{chromeFull, chromeMinimal} {
		if len(splashHeader(info, chrome)) <= body {
			return chrome, true
		}
	}
	return chromeFull, false
}

func (a *app) whaleVisible() bool {
	if !a.animateWhale || a.whale == nil || a.visibleCount() != 0 {
		return false
	}
	chrome, ok := a.splashChromeAt()
	if !ok {
		return false
	}
	return resolveSplashLayout(a.scr.width, true, splashFace).showWhale &&
		len(splashHeader(a.splashInfo(), chrome)) > 1
}

func (a *app) advanceWhaleAnimation() {
	if !a.animateWhale || a.whale == nil || !a.whaleVisible() || a.connectionDown || a.quit {
		return
	}
	heart := a.pendingHeart
	a.pendingHeart = false
	a.whale.advance(a.clock().UnixMilli(), a.busy, heart)
}

func (a *app) requestWhaleHeart() {
	if !a.animateWhale || a.whale == nil || !a.whaleVisible() || a.connectionDown || a.quit {
		return
	}
	// 点击立即消费一次 tick，不等当前 timer；主循环下一轮只重设同一个 timer，
	// 不会因连点叉出多条调度链。
	a.pendingHeart = true
	a.advanceWhaleAnimation()
}

// whaleWakeup 是主循环唯一的动画定时依据。ok=false 时必须停掉 timer 并置 nil
// channel；仅靠 duration 很大仍会造成不可见页面的后台唤醒。
func (a *app) whaleWakeup() (time.Duration, bool) {
	if !a.animateWhale || a.whale == nil || a.connectionDown || a.quit || !a.whaleVisible() {
		return 0, false
	}
	d := time.Duration(a.whale.nextDelay) * time.Millisecond
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d, true
}

// handleWhaleSpecial 识别 heart 测试/脚本输入与 SGR 鼠标。普通键盘、滚动、
// 审批和发送确认完全仍走旧路径；鼠标序列绝不落成输入草稿。
func (a *app) handleWhaleSpecial(key string) bool {
	if key == "heart" {
		a.requestWhaleHeart()
		return true
	}
	if !strings.HasPrefix(key, "mouse:") {
		return false
	}
	if x, y, ok := parseSGRLeftClick(strings.TrimPrefix(key, "mouse:")); ok && a.whaleHit(x, y) {
		a.requestWhaleHeart()
	}
	return true
}

func parseSGRLeftClick(seq string) (x, y int, ok bool) {
	if len(seq) < 2 || seq[0] != '<' || seq[len(seq)-1] != 'M' {
		return 0, 0, false // 只认左键按下；m=release 与滚轮都不触发爱心
	}
	parts := strings.Split(seq[1:len(seq)-1], ";")
	if len(parts) != 3 || parts[0] != "0" {
		return 0, 0, false
	}
	x, errX := strconv.Atoi(parts[1])
	y, errY := strconv.Atoi(parts[2])
	if errX != nil || errY != nil || x < 1 || y < 1 {
		return 0, 0, false
	}
	return x, y, true
}

// whaleHit 使用终端的 1-based 坐标，命中实际 40×13 立绘槽，不把右侧标题、
// 欢迎语或 footer 的点击误当成鲸鱼。off 与 splashHeader 的垂直居中公式一致。
func (a *app) whaleHit(x, y int) bool {
	if !a.whaleVisible() || x < 1 || x > whaleBoxWidth {
		return false
	}
	chrome, ok := a.splashChromeAt()
	if !ok {
		return false
	}
	lay := resolveSplashLayout(a.scr.width, true, splashFace)
	textRows := 0
	if lay.showBigTitle || lay.showPlainTitle {
		if chrome == chromeFull {
			textRows++ // wordmark
		}
		if lay.showBigTitle {
			textRows += bigTextRows + 1 + bigTextRows
		} else {
			textRows++
		}
		if chrome == chromeFull {
			textRows += 3 // model / cwd / tip
		}
	}
	off := 0
	if textRows > 0 {
		off = (maxInt(textRows, whaleArtRows) - whaleArtRows) / 2
	}
	top := 1 + off // splashHeader 的 marginTop 1；这里转成 1-based 时下面再 +1
	return y >= top+1 && y <= top+whaleArtRows
}
