package transport

// 官方 `userQuestions.attachWait` 的**真实定时等待**契约测试。
//
// 钉住的语义（`@deepseek-ai/dsh-user-questions` 的 TimedQuestionWait）：
//   - remainingMs 是 deadline - now 的真实剩余，随真实时间单调递减；
//   - deadline 由"问题被问出来的时刻 + 前台等待窗口"算出，**不是编的**；
//   - 窗口走完 → 不再发帧（L1 空结果），不是回一个 0 让 UI 继续转；
//   - 结算（答案到达）之后同 callId 不再产生等待；
//   - 一个会话的等待不能串到另一个会话（A/B 隔离）。

import (
	"net/http"
	"testing"
	"time"
)

func TestOfficialQuestionWaitsRemainingCountsDown(t *testing.T) {
	var now time.Time
	waits := NewOfficialQuestionWaits(OfficialQuestionWaitOptions{
		Timeout: 10 * time.Second,
		Now:     func() time.Time { return now },
	})
	askedAt := time.Unix(1_800_000_000, 0)
	now = askedAt

	remaining, live := waits.Arm("s1", "call-1", askedAt)
	if !live || remaining != 10_000 {
		t.Fatalf("第一次剩余应该是 10000ms/live，实际 %d/%v", remaining, live)
	}

	now = askedAt.Add(3 * time.Second)
	remaining, live = waits.Arm("s1", "call-1", askedAt)
	if !live || remaining != 7_000 {
		t.Fatalf("3s 后剩余应该是 7000ms，实际 %d/%v", remaining, live)
	}

	// 同一个等待重复 arm 不改 deadline（官方 attach 也只 yield 一帧）。
	remaining, live = waits.Arm("s1", "call-1", askedAt.Add(9*time.Second))
	if !live || remaining != 7_000 {
		t.Fatalf("重复 arm 不该重置 deadline，实际 %d/%v", remaining, live)
	}
}

func TestOfficialQuestionWaitsEndAfterDeadline(t *testing.T) {
	var now time.Time
	waits := NewOfficialQuestionWaits(OfficialQuestionWaitOptions{
		Timeout: 5 * time.Second,
		Now:     func() time.Time { return now },
	})
	askedAt := time.Unix(1_800_000_000, 0)
	now = askedAt

	if _, live := waits.Arm("s1", "call-1", askedAt); !live {
		t.Fatal("刚开始就该 live")
	}
	now = askedAt.Add(6 * time.Second)
	if _, live := waits.Arm("s1", "call-1", askedAt); live {
		t.Fatal("窗口走完之后不该再发帧")
	}
	// 结算过之后即使把时钟拨回去也不再 live。
	now = askedAt
	if _, live := waits.Arm("s1", "call-1", askedAt); live {
		t.Fatal("已结算的等待不该复活")
	}
}

func TestOfficialQuestionWaitsSettle(t *testing.T) {
	var now time.Time
	waits := NewOfficialQuestionWaits(OfficialQuestionWaitOptions{
		Timeout: 10 * time.Second,
		Now:     func() time.Time { return now },
	})
	askedAt := time.Unix(1_800_000_000, 0)
	now = askedAt

	if _, live := waits.Arm("s1", "call-1", askedAt); !live {
		t.Fatal("刚开始就该 live")
	}
	waits.Settle("s1", "call-1")
	if _, live := waits.Remaining("s1", "call-1"); live {
		t.Fatal("结算之后不该还有剩余")
	}
	if _, live := waits.Arm("s1", "call-1", askedAt); live {
		t.Fatal("结算之后 arm 不该复活")
	}
}

func TestOfficialQuestionWaitsIsolatedPerSession(t *testing.T) {
	var now time.Time
	waits := NewOfficialQuestionWaits(OfficialQuestionWaitOptions{
		Timeout: 10 * time.Second,
		Now:     func() time.Time { return now },
	})
	askedAt := time.Unix(1_800_000_000, 0)
	now = askedAt

	if _, live := waits.Arm("session-A", "call-1", askedAt); !live {
		t.Fatal("A 应该 live")
	}
	if _, live := waits.Remaining("session-B", "call-1"); live {
		t.Fatal("B 不该看到 A 的等待")
	}
	waits.Settle("session-A", "call-1")
	if _, live := waits.Remaining("session-B", "call-1"); live {
		t.Fatal("结算 A 不该影响 B（B 本来就没有）")
	}
}

func TestOfficialQuestionWaitsDefaultTimeout(t *testing.T) {
	waits := NewOfficialQuestionWaits(OfficialQuestionWaitOptions{})
	if got := waits.Timeout(); got != agentQuestionWaitTimeout {
		t.Fatalf("默认窗口应该是 %v，实际 %v", agentQuestionWaitTimeout, got)
	}
	if got := NewOfficialQuestionWaits(OfficialQuestionWaitOptions{Timeout: -1}).Timeout(); got != agentQuestionWaitTimeout {
		t.Fatalf("非法窗口应该回落默认值，实际 %v", got)
	}
}

// 走真实 /api 出口：空等待（nil）必须投影成空 result 对象，而不是 500。
func TestOfficialQuestionAttachWaitWireEmpty(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{
		UserQuestionsAttachWait: func(string, string) (*OfficialQuestionWait, error) {
			return nil, nil
		},
	})
	code, out := callOfficial(t, srv, "userQuestions.attachWait", map[string]any{
		"agentId": "s1", "callId": "call-1",
	})
	if code != http.StatusOK {
		t.Fatalf("空等待应该 200：%d %v", code, out)
	}
	result, ok := out["result"].(map[string]any)
	if !ok || len(result) != 0 {
		t.Fatalf("空等待应该是空 result 对象：%v", out)
	}
}

// 有等待时 remainingMs 原样透传。
func TestOfficialQuestionAttachWaitWireValue(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{
		UserQuestionsAttachWait: func(sessionID, callID string) (*OfficialQuestionWait, error) {
			if sessionID != "s1" || callID != "call-1" {
				t.Fatalf("参数没透传：%s/%s", sessionID, callID)
			}
			return &OfficialQuestionWait{RemainingMs: 4200}, nil
		},
	})
	code, out := callOfficial(t, srv, "userQuestions.attachWait", map[string]any{
		"agentId": "s1", "callId": "call-1",
	})
	if code != http.StatusOK {
		t.Fatalf("状态 %d: %v", code, out)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("结果不是对象：%v", out)
	}
	if got, _ := result["remainingMs"].(float64); got != 4200 {
		t.Fatalf("remainingMs 没透传：%v", result)
	}
}
