package main

import "testing"

// 定时任务到点时用哪个审批策略，是这个产品里**最容易放行太多权限**的一处：
// 无人值守 + 提示词由模型写 + 授权在登记时给。
//
// 所以这里逐条钉死：默认只读，只有显式 opt-in 才把 flag 的策略透传下去。
func TestSchedulePolicyDefaultsToReadOnly(t *testing.T) {
	policy, note := schedulePolicy(false, "allow")
	if policy != "deny" {
		t.Fatalf("没开 allow_writes 时策略 = %q，必须是 deny（写类工具直接拒）", policy)
	}
	if note == "" {
		t.Fatal("必须有一句给人看的说明，否则屏幕上分不出这条任务是要不要写")
	}
	// 关键：flag 传 allow 也不能把只读任务变成可写。
	if policy == "allow" {
		t.Fatal("--policy=allow 不能压过任务的只读默认")
	}
}

func TestSchedulePolicyHonorsExplicitOptIn(t *testing.T) {
	for _, flag := range []string{"allow", "ask", "auto", "deny"} {
		policy, note := schedulePolicy(true, flag)
		if policy != flag {
			t.Fatalf("开了 allow_writes 时应当透传 %q，实际 %q", flag, policy)
		}
		if note == "" {
			t.Fatal("开了写权限也要说一句")
		}
	}
}

// 两种情况的说明必须是**不同的字**，否则屏幕上区分不出来。
func TestSchedulePolicyNotesDiffer(t *testing.T) {
	_, ro := schedulePolicy(false, "allow")
	_, rw := schedulePolicy(true, "allow")
	if ro == rw {
		t.Fatalf("只读与可写的说明不能一样: %q", ro)
	}
}
