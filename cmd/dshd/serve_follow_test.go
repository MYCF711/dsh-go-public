package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// 这一份钉住「一次 prompt 只推一轮的帧」。
//
// 症状（真机，scripts/tui-session-e2e.py 四轮）：四次 prompt、v4 日志里恰好
// 4 条 user/message，而界面收到的 journal 里有 10 条 user/message、
// 10 个 turn/start —— 一次 prompt 在界面上变成两三turn。
// 根因是 follow 每轮从 offset 0 起跑，把整份日志从头再推一遍：
// 第 k 轮就多出前 k-1 轮。日志是干净的，脏的是推给界面的那一路。
//
// 判据只认**推出去的东西**（pub 收到的帧）+ 日志本身没被改坏，不看内部状态。

const (
	// fakeChildTurnEnv 打开"假子进程"模式：测试二进制以 `run ...` 被重 exec 时，
	// 不跑模型，只往 --session 指的日志里追加一轮，然后退出 0。
	//
	// 为什么要它：follow 是**父进程**从日志读帧推给界面的，真子进程要模型才会写。
	// 单测要证明"只推这一轮"，就得有一个会写日志、但不需要模型的最小 run。
	fakeChildTurnEnv = "DSHD_FAKE_CHILD_TURN"

	// fakeChildDelayEnv 让假子进程写完后多活一会儿。0/空 = 立刻退出（"快轮次"）。
	//
	// 为什么要有：follow 只在**活着的时候**每 200ms 读一次日志，done 之后只补
	// 一次 drain。子进程 200ms 内就退出的轮次，只走 drain 那条路——这正是
	// "一轮跑得太快、界面整轮没帧"的形状。
	fakeChildDelayEnv = "DSHD_FAKE_CHILD_DELAY_MS"

	historyUserText = "历史那一轮的用户消息（早该推过了）"
	childUserText   = "这一轮子进程写的用户消息"
)

func init() {
	if os.Getenv(fakeChildTurnEnv) == "" || len(os.Args) < 2 || os.Args[1] != "run" {
		return
	}
	home, sid := "", ""
	for i := 2; i+1 < len(os.Args); i++ {
		switch os.Args[i] {
		case "--home":
			home = os.Args[i+1]
		case "--session":
			sid = os.Args[i+1]
		}
	}
	if home == "" || sid == "" {
		fmt.Fprintln(os.Stderr, "假子进程缺 --home/--session")
		os.Exit(81)
	}
	path, err := newSessionSource(home, "").resolve(sid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "假子进程找不到会话: %v\n", err)
		os.Exit(82)
	}
	l, err := session.OpenSessionAt(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "假子进程打不开会话: %v\n", err)
		os.Exit(83)
	}
	app := func(typ string, data any) {
		if err := l.Append(typ, data); err != nil {
			fmt.Fprintf(os.Stderr, "假子进程写 %s 失败: %v\n", typ, err)
			_ = l.Close()
			os.Exit(84)
		}
	}
	app("turn/start", session.TurnStartData(2))
	app("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: childUserText}}))
	app("assistant/message", session.AssistantMessageEvent(2, 1,
		[]map[string]any{session.TextBlock("好的")}, "stop", nil))
	app("turn/end", session.TurnEndData(2, "completed", ""))
	if err := l.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "假子进程关会话失败: %v\n", err)
		os.Exit(85)
	}
	if ms, aerr := strconv.Atoi(os.Getenv(fakeChildDelayEnv)); aerr == nil && ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	os.Exit(0)
}

// appendHistoryTurn 写一轮"已经推过"的历史。
func appendHistoryTurn(t *testing.T, l *session.Log) {
	t.Helper()
	must := func(typ string, data any) {
		t.Helper()
		if err := l.Append(typ, data); err != nil {
			t.Fatalf("写 %s 失败: %v", typ, err)
		}
	}
	must("turn/start", session.TurnStartData(1))
	must("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: historyUserText}}))
	must("assistant/message", session.AssistantMessageEvent(1, 1,
		[]map[string]any{session.TextBlock("第一轮的回答")}, "stop", nil))
	must("turn/end", session.TurnEndData(1, "completed", ""))
}

// runFakeChildTurn 建一份会话（withHistory 时再写一轮"早就推过"的），
// 然后用假子进程跑一轮，返回**推给界面的帧**与日志路径。
//
// delayMS 是假子进程写完后再活多久（见 fakeChildDelayEnv）：
// 400 = follow 至少跑过一次 tick；0 = 子进程 200ms 内退出，只走 done 之后的 drain。
//
// withHistory=false 时日志里除了 header 什么都没有，于是假子进程写的第一条事件
// 就是 **seq=0**——正是"每个会话的第一条事件"那一格。
func runFakeChildTurn(t *testing.T, sid string, delayMS int, withHistory bool) ([]transport.ServerFrame, string) {
	t.Helper()
	home := t.TempDir()
	l, err := session.CreateSession(home, "/tmp/dsh-dup", sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if withHistory {
		appendHistoryTurn(t, l)
	}
	path := l.Path
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv(fakeChildTurnEnv, "1")
	t.Setenv(fakeChildDelayEnv, strconv.Itoa(delayMS))
	journals := transport.NewJournals(nil)
	var mu sync.Mutex
	var frames []transport.ServerFrame
	r := &loopRunner{
		home: home, policy: "deny", maxSteps: 2, model: "m",
		journals: journals,
		pub: func(_ string, f transport.ServerFrame) {
			mu.Lock()
			frames = append(frames, f)
			mu.Unlock()
		},
	}
	if err := r.Run(context.Background(), sid, "turn-2", "第二轮"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]transport.ServerFrame(nil), frames...), path
}

// assertSingleTurnFrames 是判据本体：一次 prompt → 恰好一轮的帧，
// 且历史那一轮的字面量不许出现在任何一帧里。
func assertSingleTurnFrames(t *testing.T, got []transport.ServerFrame) {
	t.Helper()
	var starts, users, ends int
	for _, f := range got {
		switch f.Type {
		case "turn/start":
			starts++
		case "user/message":
			users++
		case "turn/end":
			ends++
		}
		if strings.Contains(string(f.Data), historyUserText) {
			t.Errorf("历史那一轮又被推了一遍（seq=%d type=%s）：%s", f.Seq, f.Type, f.Data)
		}
	}
	if starts != 1 || users != 1 || ends != 1 {
		var types []string
		for _, f := range got {
			types = append(types, fmt.Sprintf("%d:%s", f.Seq, f.Type))
		}
		t.Fatalf("一次 prompt 只该推一轮：turn/start=%d user/message=%d turn/end=%d；实际帧=%v",
			starts, users, ends, types)
	}
	if s := mustFrame(t, got, "user/message").Data; !strings.Contains(string(s), childUserText) {
		t.Fatalf("user/message 不是这一轮的内容: %s", s)
	}
	if !hasFrameType(got, TurnDone) {
		t.Fatalf("没有收尾帧 %s：帧=%v", TurnDone, typesOf(got))
	}
	// H14：seq 由 journal 发号，必须严格递增且不重号。
	for i, f := range got {
		if f.Seq != i+1 {
			t.Fatalf("第 %d 帧 seq=%d，期望 %d（journal 单源）", i, f.Seq, i+1)
		}
	}
}

// TestLoopRunnerOnlyPushesThisTurnsFrames 是这条缺陷的回归测试。
//
// 旧实现：follow 从 offset 0 起跑 → 历史那 4 帧被再推一遍，user/message 变成 2 条。
// 新实现：起跑水位取在**起子进程之前**，只推这一轮。
func TestLoopRunnerOnlyPushesThisTurnsFrames(t *testing.T) {
	got, path := runFakeChildTurn(t, "dup-follow-1", 400, true)
	assertSingleTurnFrames(t, got)

	// 日志不许被改坏：历史 1 条 + 这一轮 1 条 = 2 条。
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读不回日志: %v", err)
	}
	var logUsers, logStarts int
	for _, ev := range back.Events {
		switch ev.Type {
		case "user/message":
			logUsers++
		case "turn/start":
			logStarts++
		}
	}
	if logUsers != 2 || logStarts != 2 {
		t.Fatalf("v4 日志被改坏了：user/message=%d turn/start=%d（期望 2/2）", logUsers, logStarts)
	}
}

// TestLoopRunnerFastChildStillPushesFrames：子进程 200ms 内就退出的轮次，
// follow 只走 done 之后的 drain。旧实现在那条路上连 path 都还是空串，
// drain 读一个空路径直接返回——**整轮的帧一条都没推**，界面只剩 turn/done。
//
// 起跑水位同时把 path 带过来了，这条分支于是有了能读的路径。
func TestLoopRunnerFastChildStillPushesFrames(t *testing.T) {
	got, _ := runFakeChildTurn(t, "dup-follow-fast", 0, true)
	assertSingleTurnFrames(t, got)
}

// TestLoopRunnerPushesFirstEventOfNewSession 钉住"seq=0 那条不许吞"。
//
// v4 的 seq 从 0 起（`session.Log` 的第一条事件就是 seq=0），而 follow 的判据是
// `ev.Seq <= lastSeq`。lastSeq 初值取 0 时，**每个会话的第一条事件**永远推不出去：
// 真机 e2e 的日志第一条是 seq=0 的 system/message，四轮界面上一次都没有；
// 换成一个首条就是 turn/start 的会话，界面上就会"有 user/message、没有 turn/start"。
func TestLoopRunnerPushesFirstEventOfNewSession(t *testing.T) {
	got, path := runFakeChildTurn(t, "dup-follow-first", 400, false)
	assertSingleTurnFrames(t, got)

	// 日志里这一轮从 seq=0 起——测试自己先确认"第一格"确实是 0，
	// 否则这条用例证明不了它想证明的东西。
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读不回日志: %v", err)
	}
	if len(back.Events) == 0 || back.Events[0].Seq != 0 {
		t.Fatalf("这份日志的第一条不是 seq=0（共 %d 条事件）", len(back.Events))
	}
	if !hasFrameType(got, back.Events[0].Type) {
		t.Fatalf("第一条事件（seq=0 %s）没有推出去：帧=%v", back.Events[0].Type, typesOf(got))
	}
}

func mustFrame(t *testing.T, frames []transport.ServerFrame, typ string) transport.ServerFrame {
	t.Helper()
	for _, f := range frames {
		if f.Type == typ {
			return f
		}
	}
	t.Fatalf("没有 %s 帧；帧=%v", typ, typesOf(frames))
	return transport.ServerFrame{}
}

func hasFrameType(frames []transport.ServerFrame, typ string) bool {
	for _, f := range frames {
		if f.Type == typ {
			return true
		}
	}
	return false
}

func typesOf(frames []transport.ServerFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Type)
	}
	return out
}

// TestFollowBaselineIsTheEndOfTheLog 钉住水位口径：**文件长度**，不是 0。
//
// 用长度而不是"最后一条事件的 seq"：日志是 O_APPEND 追加的，长度就是下一帧
// 的起点；按 seq 过滤要先把整份日志读一遍，每轮付一次没有意义。
func TestFollowBaselineIsTheEndOfTheLog(t *testing.T) {
	home := t.TempDir()
	l, err := session.CreateSession(home, "/tmp/dsh-baseline", "baseline-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	appendHistoryTurn(t, l)
	path := l.Path
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	r := &loopRunner{home: home}
	base := r.followBaseline("baseline-1")
	if base.path != path {
		t.Fatalf("水位路径 = %q，期望 %q", base.path, path)
	}
	if base.offset != st.Size() {
		t.Fatalf("水位 offset = %d，期望文件长度 %d（0 会把历史再推一遍）", base.offset, st.Size())
	}
	if base.lastSeq != -1 {
		t.Fatalf("lastSeq 初值必须是 -1（v4 的 seq 从 0 起，用 0 会吞掉每个会话的第一条），实际 %d", base.lastSeq)
	}
}

// TestFollowBaselineMissingSessionIsZero：会话还不存在（子进程会建）时没有历史，
// offset 0 不会重复——这正是"新建会话的第一轮"。
//
// lastSeq 仍是 -1：文件还不存在时，子进程写下的第一条事件就是 **seq=0**。
func TestFollowBaselineMissingSessionIsZero(t *testing.T) {
	r := &loopRunner{home: t.TempDir()}
	base := r.followBaseline("no-such-session-id")
	if base.offset != 0 || base.path != "" || base.lastSeq != -1 {
		t.Fatalf("没有会话时水位应为 {offset:0 lastSeq:-1 path:\"\"}，实际 %+v", base)
	}
}
