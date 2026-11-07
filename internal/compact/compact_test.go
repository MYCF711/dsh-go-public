package compact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dshgo/internal/llm"
	"dshgo/internal/session"
)

func msg(seq int64, typ, role, content string) session.SurfaceMsg {
	return session.SurfaceMsg{
		Seq: seq, Type: typ, Source: role,
		Message: llm.Message{Role: role, Content: content},
	}
}

func surfaceOf(n int) []session.SurfaceMsg {
	out := make([]session.SurfaceMsg, 0, n)
	for i := 0; i < n; i++ {
		role, typ := "user", "user/message"
		if i%2 == 1 {
			role, typ = "assistant", "assistant/message"
		}
		out = append(out, msg(int64(i), typ, role, "内容"))
	}
	return out
}

func TestPlanKeepsRecent(t *testing.T) {
	s := surfaceOf(10)
	span, n, ok := Plan(s, Options{KeepRecent: 4, KeepTokens: 1})
	if !ok {
		t.Fatal("应该能压")
	}
	if n != 6 {
		t.Fatalf("压缩条数 = %d，期望 6（10-4）", n)
	}
	if span.Start != 0 || span.End != 5 {
		t.Fatalf("区间 = [%d,%d]，期望 [0,5]", span.Start, span.End)
	}
}

func TestPlanNeverTouchesLeadingSystem(t *testing.T) {
	// seq 必须连续：system 占 0，其余从 1 开始。
	s := []session.SurfaceMsg{msg(0, "system/message", "system", "你是 dsh-go")}
	for i, m := range surfaceOf(9) {
		m.Seq = int64(i + 1)
		s = append(s, m)
	}
	span, n, ok := Plan(s, Options{KeepRecent: 3, KeepTokens: 1})
	if !ok {
		t.Fatal("应该能压")
	}
	if span.Start == 0 {
		t.Fatal("开头的 system/message 不能被压掉——压掉模型就忘了自己是谁")
	}
	if span.Start != 1 {
		t.Fatalf("区间起点 = %d，期望 1", span.Start)
	}
	if n != 6 {
		t.Fatalf("压缩条数 = %d，期望 6", n)
	}
}

func TestPlanRespectsMinAndMax(t *testing.T) {
	if _, _, ok := Plan(surfaceOf(5), Options{KeepRecent: 4, KeepTokens: 1}); ok {
		t.Fatal("只有 1 条可压时不该触发（MinToCompact=2）")
	}
	if _, _, ok := Plan(surfaceOf(8), Options{MaxMessages: 6, KeepRecent: 3, KeepTokens: 1}); !ok {
		t.Fatal("表面 8 条已超过 MaxMessages=6，应该触发")
	}
	if _, _, ok := Plan(surfaceOf(4), Options{MaxMessages: 6}); ok {
		t.Fatal("表面没超过阈值不该触发")
	}
}

func TestPlanOnlyPrefix(t *testing.T) {
	s := surfaceOf(20)
	span, _, ok := Plan(s, Options{KeepRecent: 5, KeepTokens: 1})
	if !ok {
		t.Fatal("应该能压")
	}
	if span.End != 14 {
		t.Fatalf("区间终点 = %d，期望 14（20-5-1）", span.End)
	}
	if span.End >= s[len(s)-1].Seq {
		t.Fatal("压到了最后一条")
	}
}

type fakeChat struct {
	got    llm.Request
	reply  string
	err    error
	called int
}

func (f *fakeChat) Stream(_ context.Context, req llm.Request, _ func(llm.Event) error) (*llm.Response, error) {
	f.called++
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Response{Content: f.reply}, nil
}

func TestSummarizeCallsModelOnce(t *testing.T) {
	chat := &fakeChat{reply: "## 目标\n- 用 Go 写 DSH\n"}
	out, err := Summarize(context.Background(), chat, "m", surfaceOf(4))
	if err != nil {
		t.Fatal(err)
	}
	if chat.called != 1 {
		t.Fatalf("应该只调一次模型，实际 %d 次", chat.called)
	}
	if !strings.Contains(out, "## 目标") {
		t.Fatalf("摘要 = %q", out)
	}
	if chat.got.Model != "m" || len(chat.got.Messages) != 2 {
		t.Fatalf("请求不对: %+v", chat.got)
	}
	prompt := chat.got.Messages[1].Content
	if !strings.Contains(prompt, "[user]") || !strings.Contains(prompt, "不要编造") {
		t.Fatalf("提示词缺要求：%s", prompt[:min(120, len(prompt))])
	}
}

func TestSummarizeErrors(t *testing.T) {
	if _, err := Summarize(context.Background(), &fakeChat{reply: "x"}, "m", nil); err == nil {
		t.Fatal("空输入必须报错")
	}
	if _, err := Summarize(context.Background(), &fakeChat{reply: "   "}, "m", surfaceOf(2)); err == nil {
		t.Fatal("空摘要必须报错")
	}
	boom := errors.New("网关 502")
	if _, err := Summarize(context.Background(), &fakeChat{err: boom}, "m", surfaceOf(2)); err == nil {
		t.Fatal("模型错误必须冒出来")
	}
}

func TestSummarizeMarksEarlierSummary(t *testing.T) {
	chat := &fakeChat{reply: "ok"}
	s := surfaceOf(2)
	s = append(s, session.SurfaceMsg{
		Seq: 9, Type: "compaction/summary", Source: "compact-checkpoint", Summary: true,
		Message: llm.Message{Role: "user", Content: "（更早的摘要）"},
	})
	if _, err := Summarize(context.Background(), chat, "m", s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chat.got.Messages[1].Content, "[earlier-summary]") {
		t.Fatal("更早的摘要应该被标成 earlier-summary，否则模型会把它当用户原话")
	}
}

// TestSelectHandlesCheckpointOrdering 锁死一个真 bug：
// 压缩替身被插到 surface 最前面，但它的 seq 是最后追加的。
// 如果按 seq 区间回头筛节点，就会筛空 → 日志里出现「没有可摘要的消息」。
func TestSelectHandlesCheckpointOrdering(t *testing.T) {
	s := []session.SurfaceMsg{
		// 替身在最前，seq 却是最大的。
		{Seq: 100, Type: "compaction/summary", Source: "compact-checkpoint", Summary: true,
			Message: llm.Message{Role: "user", Content: "（更早的摘要）"}},
		msg(1, "user/message", "user", "问一"),
		msg(2, "assistant/message", "assistant", "答一"),
		msg(3, "user/message", "user", "问二"),
		msg(4, "assistant/message", "assistant", "答二"),
		msg(5, "user/message", "user", "问三"),
		msg(6, "assistant/message", "assistant", "答三"),
	}
	items, span, ok := Select(s, Options{KeepRecent: 2, KeepTokens: 1})
	if !ok {
		t.Fatal("应该能选出一段可压的（替身不该挡住整个计划）")
	}
	if len(items) == 0 {
		t.Fatal("选出来 0 条——正是那个 bug")
	}
	if span.Start <= 0 || span.End <= 0 {
		t.Fatalf("区间非法: %+v", span)
	}
	if span.Start > span.End {
		t.Fatalf("start > end: %+v（说明用了错误的顺序）", span)
	}
	for _, it := range items {
		if it.Summary {
			t.Fatal("不该把替身再压一次")
		}
		if it.Seq < span.Start || it.Seq > span.End {
			t.Fatalf("节点 seq %d 不在区间 %+v 内", it.Seq, span)
		}
	}
}

// 保留尾巴要按 **token 预算**，不是按条数。
//
// 踩过：KeepRecent=8 在"一条工具输出就 2,300 token"的会话里能顶到 8k 以上，
// 压缩看着一直在跑，上下文纹丝不动。
func TestKeepByTokenBudgetNotCount(t *testing.T) {
	// 每条 2,000 token 的大消息 × 10 条
	big := strings.Repeat("x", 8000)
	s := make([]session.SurfaceMsg, 0, 10)
	for i := 0; i < 10; i++ {
		s = append(s, msg(int64(i), "tool/result", "tool", big))
	}

	items, _, ok := Select(s, Options{KeepRecent: 2, KeepTokens: 2500, MinToCompact: 2})
	if !ok {
		t.Fatal("应该能压")
	}
	// 预算 2500 → 尾巴最多留 1 条大的；至少留 2 条是下限，所以留 2、压 8
	if len(items) != 8 {
		t.Fatalf("压缩条数 = %d，期望 8（10 条里只留 2 条大的）", len(items))
	}
}

// 小消息就该多留几条：预算按体积算，不是按条数算。
func TestKeepManySmallMessages(t *testing.T) {
	s := surfaceOf(10) // 每条 ≈ 1 token
	_, _, ok := Select(s, Options{KeepRecent: 2, KeepTokens: 8, MinToCompact: 2})
	if !ok {
		t.Fatal("应该能压")
	}
	items, _, _ := Select(s, Options{KeepRecent: 2, KeepTokens: 8, MinToCompact: 2})
	// 每条小消息按估算占几个 token；预算 8 能装下不止下限的 2 条。
	per := session.EstimateRunes("内容")
	want := 8 / per
	if want < 2 {
		want = 2
	}
	if keep := len(s) - len(items); keep < want {
		t.Fatalf("只留了 %d 条，8 token 预算应该能留 %d 条（每条 %d）", keep, want, per)
	}
}
