package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"dshgo/internal/spi"
)

// 这两段文本是从官方 0.2.2 原件逐字节抄下来的基准：
//
//	app.asar!dsh/node_modules/@deepseek-ai/dsh-repeat-tool-reminder/lib/index.js
//	GENTLE_REMINDER / detailedReminder()
//
// 抄一份在这里是有意的：实现里改了一个字（少个标点、换个措辞），这条测试就会红，
// 而不是等到有人拿模型语料发现"提醒文案跟官方不一样"。
const (
	officialGentleReminder = "You are repeating the exact same tool call with identical arguments. " +
		"Carefully analyze the previous result before calling again: if the task is not complete, " +
		"try a different approach or different arguments instead of repeating the call."

	officialDetailedTail = "\nThe repeated calls are not making progress. Do not call this tool with " +
		"these exact arguments again. Inspect the latest result and choose a different action, " +
		"different arguments, or finish the task if enough evidence has been gathered."
)

// repeatCall 跑一次**成功**的调用；失败直接 Fatal（本文件的成功路径不该报错）。
func repeatCall(t *testing.T, p *Pipeline, ctx context.Context, name, args string) spi.Result {
	t.Helper()
	res, err := p.Run(ctx, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s 不该失败: %v", name, err)
	}
	return res
}

func detailedReminderFor(tool, args string, count int) string {
	return "Repeated tool call detected:\n- tool: " + tool +
		"\n- consecutive_calls: " + itoa(count) +
		"\n- arguments: " + args + officialDetailedTail
}

// 达到官方阈值 3/5/8 各注入一次，且只在那一刻注入。
func TestRepeatReminderFiresAtOfficialThresholds(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{
		name: "grep",
		res:  spi.Result{Summary: "命中 1 处", Text: "a.txt:1:x"},
	})
	defer k.Root().Dispose()

	ctx := spi.WithSession(context.Background(), "s-third")
	const args = `{"path":".","pattern":"TODO"}`

	for i := 1; i <= 9; i++ {
		res := repeatCall(t, p, ctx, "grep", args)
		switch i {
		case 1, 2, 4, 6, 7, 9:
			if res.Hint != "" {
				t.Fatalf("第 %d 次不是阈值，不该提醒，Hint=%q", i, res.Hint)
			}
		case 3:
			if res.Hint != officialGentleReminder {
				t.Fatalf("第 3 次应给官方 gentle 文案：\n want=%q\n  got=%q", officialGentleReminder, res.Hint)
			}
		case 5, 8:
			want := detailedReminderFor("grep", args, i)
			if res.Hint != want {
				t.Fatalf("第 %d 次应给官方详细文案：\n want=%q\n  got=%q", i, want, res.Hint)
			}
		}
		// 提醒不能吞掉工具自己的产出。
		if res.Text != "a.txt:1:x" || res.Summary != "命中 1 处" {
			t.Fatalf("第 %d 次原始输出被改：Text=%q Summary=%q", i, res.Text, res.Summary)
		}
	}
}

// 换工具或换参数即重置：链上只记"最近一次"的 (工具名, 参数指纹)。
func TestRepeatReminderResetsOnToolOrArgumentChange(t *testing.T) {
	p, k := newPipe(t, Options{},
		&stubTool{name: "grep", res: spi.Result{Summary: "ok"}},
		&stubTool{name: "glob", res: spi.Result{Summary: "ok"}},
	)
	defer k.Root().Dispose()

	ctx := spi.WithSession(context.Background(), "s-reset")
	const argsA = `{"pattern":"A"}`
	const argsB = `{"pattern":"B"}`

	for i := 1; i <= 3; i++ {
		res := repeatCall(t, p, ctx, "grep", argsA)
		if i < 3 && res.Hint != "" {
			t.Fatalf("argsA 第 %d 次不该提醒", i)
		}
		if i == 3 && res.Hint != officialGentleReminder {
			t.Fatalf("argsA 第 3 次应提醒，得到 %q", res.Hint)
		}
	}

	// 参数变了：计数从 1 重新开始，前两次安静。
	for i := 1; i <= 2; i++ {
		if res := repeatCall(t, p, ctx, "grep", argsB); res.Hint != "" {
			t.Fatalf("argsB 第 %d 次不该提醒（参数变了就该重新数），得到 %q", i, res.Hint)
		}
	}

	// 换工具同理：glob 从 1 数起——第 3 次是 gentle，第 5 次才给详细文案。
	for i := 1; i <= 2; i++ {
		if res := repeatCall(t, p, ctx, "glob", argsA); res.Hint != "" {
			t.Fatalf("glob 第 %d 次不该提醒（换工具了），得到 %q", i, res.Hint)
		}
	}
	if res := repeatCall(t, p, ctx, "glob", argsA); res.Hint != officialGentleReminder {
		t.Fatalf("glob 第 3 次应提醒，得到 %q", res.Hint)
	}
	if res := repeatCall(t, p, ctx, "glob", argsA); res.Hint != "" {
		t.Fatalf("glob 第 4 次不是阈值，不该提醒，得到 %q", res.Hint)
	}
	if res := repeatCall(t, p, ctx, "glob", argsA); !strings.Contains(res.Hint, "- tool: glob") {
		t.Fatalf("glob 第 5 次应给详细提醒（说明它按自己的链数到 5），得到 %q", res.Hint)
	}
}

// 参数键序不影响判定：官方是"深度键排序 + JSON.stringify"，{"a":1,"b":2} 与
// {"b":2,"a":1} 是同一条链，否则模型换个键序就能绕过提醒。
func TestRepeatReminderIgnoresArgumentKeyOrder(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{name: "read", res: spi.Result{Summary: "ok"}})
	defer k.Root().Dispose()

	ctx := spi.WithSession(context.Background(), "s-order")
	ordered := `{"a":1,"b":{"y":2,"x":3}}`
	shuffled := `{"b":{"x":3,"y":2},"a":1}`

	for i := 1; i <= 5; i++ {
		args := ordered
		if i%2 == 0 {
			args = shuffled
		}
		res := repeatCall(t, p, ctx, "read", args)
		if i == 5 {
			want := detailedReminderFor("read", `{"a":1,"b":{"x":3,"y":2}}`, 5)
			if !strings.Contains(res.Hint, want) {
				t.Fatalf("键序不同的同一份参数应算同一条链：\n want 含 %q\n  got=%q", want, res.Hint)
			}
		}
	}
}

// R3：失败的那一次也必须带 Hint，而且原来的 Hint 与原始输出都不能被提醒挤掉。
func TestRepeatReminderKeepsFailureHintAndOutput(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{
		name: "flaky",
		res: spi.Result{
			Summary: "读取失败",
			Text:    "raw stderr body",
			Hint:    "先看日志再决定重试",
			Data:    map[string]any{"exit": 1},
		},
		err: errTestBoom,
	})
	defer k.Root().Dispose()

	ctx := spi.WithSession(context.Background(), "s-fail")
	const args = `{"path":"x"}`

	for i := 1; i <= 3; i++ {
		res, err := p.Run(ctx, "flaky", json.RawMessage(args))
		if err == nil {
			t.Fatalf("第 %d 次应返回错误", i)
		}
		if res.Text != "raw stderr body" {
			t.Fatalf("第 %d 次原始输出丢了：Text=%q", i, res.Text)
		}
		if res.Data == nil {
			t.Fatalf("第 %d 次结构化 Data 丢了", i)
		}
		if i < 3 {
			if res.Hint != "先看日志再决定重试" {
				t.Fatalf("第 %d 次不该提醒，Hint=%q", i, res.Hint)
			}
			continue
		}
		if !strings.Contains(res.Hint, "先看日志再决定重试") {
			t.Fatalf("提醒不许挤掉工具自己的 Hint，得到 %q", res.Hint)
		}
		if !strings.Contains(res.Hint, officialGentleReminder) {
			t.Fatalf("第 3 次失败也应带提醒，Hint=%q", res.Hint)
		}
	}
}

// A/B 两个会话各自计数，互不串。
func TestRepeatReminderIsolatedPerSession(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{name: "grep", res: spi.Result{Summary: "ok"}})
	defer k.Root().Dispose()

	ctxA := spi.WithSession(context.Background(), "sess-A")
	ctxB := spi.WithSession(context.Background(), "sess-B")
	const args = `{"pattern":"x"}`

	for i := 0; i < 2; i++ {
		if res := repeatCall(t, p, ctxA, "grep", args); res.Hint != "" {
			t.Fatalf("A 第 %d 次不该提醒", i+1)
		}
		if res := repeatCall(t, p, ctxB, "grep", args); res.Hint != "" {
			t.Fatalf("B 第 %d 次不该提醒（B 只有 2 次）", i+1)
		}
	}
	if res := repeatCall(t, p, ctxA, "grep", args); res.Hint != officialGentleReminder {
		t.Fatalf("A 的第 3 次要提醒（B 的调用不能累加进来），得到 %q", res.Hint)
	}
	// B 自己数到 3 才提醒：证明两本账互不影响。
	if res := repeatCall(t, p, ctxB, "grep", args); res.Hint != officialGentleReminder {
		t.Fatalf("B 的第 3 次要提醒，得到 %q", res.Hint)
	}
	if res := repeatCall(t, p, ctxB, "grep", args); res.Hint != "" {
		t.Fatalf("B 第 4 次不是阈值，不该提醒，得到 %q", res.Hint)
	}
}

// 一轮结束（turn ctx 结束）即清链：新的一轮里重复 3 次要**重新**从 1 数。
func TestRepeatReminderResetsPerTurn(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{name: "grep", res: spi.Result{Summary: "ok"}})
	defer k.Root().Dispose()

	const args = `{"pattern":"x"}`

	ctx1, cancel1 := context.WithCancel(spi.WithSession(context.Background(), "sess-turn"))
	defer cancel1()
	for i := 1; i <= 3; i++ {
		res := repeatCall(t, p, ctx1, "grep", args)
		if i == 3 && res.Hint != officialGentleReminder {
			t.Fatalf("第一轮第 3 次应提醒，得到 %q", res.Hint)
		}
	}
	cancel1()

	ctx2, cancel2 := context.WithCancel(spi.WithSession(context.Background(), "sess-turn"))
	defer cancel2()
	for i := 1; i <= 3; i++ {
		res := repeatCall(t, p, ctx2, "grep", args)
		if i < 3 && res.Hint != "" {
			t.Fatalf("新一轮第 %d 次不该提醒（上一轮的计数必须清掉），得到 %q", i, res.Hint)
		}
		if i == 3 && res.Hint != officialGentleReminder {
			t.Fatalf("新一轮第 3 次应重新提醒，得到 %q", res.Hint)
		}
	}
}

// 并发调用不许串账：两个会话同时各打 5 次，各自的阈值点必须还是 3 和 5。
func TestRepeatReminderConcurrentSessionsDoNotMix(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{name: "grep", res: spi.Result{Summary: "ok"}})
	defer k.Root().Dispose()

	const args = `{"pattern":"x"}`
	sessions := []string{"conc-A", "conc-B"}
	hits := make(map[string][]int, len(sessions))

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, sess := range sessions {
		wg.Add(1)
		go func(sess string) {
			defer wg.Done()
			ctx := spi.WithSession(context.Background(), sess)
			local := make([]int, 0, 2)
			for i := 1; i <= 5; i++ {
				res, err := p.Run(ctx, "grep", json.RawMessage(args))
				if err != nil {
					t.Errorf("%s 第 %d 次失败: %v", sess, i, err)
					return
				}
				if res.Hint != "" {
					local = append(local, i)
				}
			}
			mu.Lock()
			hits[sess] = local
			mu.Unlock()
		}(sess)
	}
	wg.Wait()

	for _, sess := range sessions {
		got := hits[sess]
		if len(got) != 2 || got[0] != 3 || got[1] != 5 {
			t.Fatalf("%s 的提醒点应为 [3 5]，得到 %v", sess, got)
		}
	}
}

// 详细提醒里的参数预览按官方 {argumentsPreviewChars}=500 截断，
// 并且以 "… (+N more chars)" 结尾——只截"给模型看的那段"，指纹仍用全文。
func TestRepeatReminderArgumentPreviewIsCapped(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{name: "grep", res: spi.Result{Summary: "ok"}})
	defer k.Root().Dispose()

	ctx := spi.WithSession(context.Background(), "s-preview")
	long := strings.Repeat("a", 600)
	canonical := `{"q":"` + long + `"}`

	for i := 1; i <= 5; i++ {
		res := repeatCall(t, p, ctx, "grep", canonical)
		if i < 5 {
			continue
		}
		if !strings.Contains(res.Hint, "… (+"+itoa(len(canonical)-500)+" more chars)") {
			t.Fatalf("参数预览应按 500 字符截断并标出省略量，得到 %q", res.Hint)
		}
		if strings.Contains(res.Hint, long) {
			t.Fatal("参数预览没截断：整段 600 字符都进提醒了")
		}
	}
}

// 子调用（workflow 里发的）不是"调用模型的链"：官方按 agent 分链，子代理重复
// 不能算到父代理头上，也不能把父代理的链冲掉。
func TestRepeatReminderSkipsNestedCalls(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{name: "grep", res: spi.Result{Summary: "ok"}})
	defer k.Root().Dispose()

	base := spi.WithSession(context.Background(), "s-nested")
	nested := spi.WithSubDispatch(base, spi.SubDispatch{Runner: p, Depth: 1})
	const args = `{"pattern":"x"}`

	for i := 1; i <= 8; i++ {
		if res := repeatCall(t, p, nested, "grep", args); res.Hint != "" {
			t.Fatalf("子调用第 %d 次不该提醒，得到 %q", i, res.Hint)
		}
	}

	// 父调用的链仍是干净的：三次就提醒。
	for i := 1; i <= 3; i++ {
		res := repeatCall(t, p, base, "grep", args)
		if i == 3 && res.Hint != officialGentleReminder {
			t.Fatalf("父调用第 3 次应提醒（子调用不该累加），得到 %q", res.Hint)
		}
	}
}

// 失败/被拒的那几次也算"尝试"：官方把计数放在 post-execute，被 pre-execute 拒绝的
// 调用同样经过它——模型反复撞同一堵墙正是最该被提醒的循环。
//
// 这里用"未知工具"当样本：它走的是流水线里最早的提前 return，提醒能落上去就说明
// 收尾那层确实覆盖了所有出口，而不是只覆盖"执行完的那条路"。
func TestRepeatReminderCountsFailedAttempts(t *testing.T) {
	p, k := newPipe(t, Options{}, &stubTool{name: "grep", res: spi.Result{Summary: "ok"}})
	defer k.Root().Dispose()

	ctx := spi.WithSession(context.Background(), "s-denied")
	const args = `{"pattern":"x"}`

	for i := 1; i <= 3; i++ {
		res, err := p.Run(ctx, "no_such_tool", json.RawMessage(args))
		if err == nil {
			t.Fatalf("第 %d 次应返回 unknown tool 错误", i)
		}
		if i < 3 {
			if res.Hint != "用工具表里真实存在的名字；不确定就先看当前模式给了哪些工具" {
				t.Fatalf("第 %d 次不该提醒，Hint=%q", i, res.Hint)
			}
			continue
		}
		// 原来的 R3 Hint 必须在，提醒追加在后面。
		if !strings.Contains(res.Hint, "用工具表里真实存在的名字") {
			t.Fatalf("提醒不许挤掉未知工具的 R3 Hint，得到 %q", res.Hint)
		}
		if !strings.Contains(res.Hint, officialGentleReminder) {
			t.Fatalf("第 3 次失败也应带提醒，Hint=%q", res.Hint)
		}
	}
}
