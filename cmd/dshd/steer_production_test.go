package main

import "testing"

// W138 先红：W125 只把 promptService 接好了，三个生产 runner 必须真正提供
// 同一 turn 的 text/content steer；只实现 Run/RunContent 不算能力。
func TestProductionRunnersExposeSteer(t *testing.T) {
	var _ contentTurnRunner = (*modelRunner)(nil)
	var _ steerTurnRunner = (*modelRunner)(nil)
	var _ steerContentTurnRunner = (*modelRunner)(nil)
	var _ steerTurnRunner = (*loopRunner)(nil)
	var _ steerContentTurnRunner = (*loopRunner)(nil)
	var _ steerTurnRunner = (*modelOverrideLoopRunner)(nil)
	var _ steerContentTurnRunner = (*modelOverrideLoopRunner)(nil)
}
