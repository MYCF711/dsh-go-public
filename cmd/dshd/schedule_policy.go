package main

// schedulePolicy 决定这条到期任务这一轮用哪个审批策略。
//
// 单独抽出来是因为它是**权限闸门**，而它原来埋在 cmdSchedule 的循环里、一行测试都没有：
// 定时任务无人值守、提示词由模型写，没显式 opt-in 就必须只读。
// 抽成纯函数才测得动，也才不会在下一次重构里被顺手改掉。
func schedulePolicy(allowWrites bool, flagPolicy string) (policy, note string) {
	if allowWrites {
		return flagPolicy, "（允许写：登记时开了 allow_writes）"
	}
	// deny 不是"问人"，是"写类工具直接拒"——只读工具不经过审批，照常可用。
	return "deny", "（只读：登记时没开 allow_writes）"
}
