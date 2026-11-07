package main

// serve 这条链上的 lazy：接线、按会话隔离、重启后仍能回放。
//
// 先说清这三条测试各自钉的是什么，免得把"验过 run"当成"验过 serve"：
//
//	serve 的一轮 = 子进程 `dshd run`（serve_tools.go 的 loopRunner）——也就是 cmdRun
//	本身。所以 lazy 的接线**没有第二份实现**：投影、装载集回放、系统提示目录、
//	挡回，全在子进程那一侧。父进程只干一件事：把 --tools-mode 原样传下去。
//	于是 serve 侧能坏的地方只有两个，这两个正好是前两条测试：
//
//	 1. TestServeForwardsToolsModeToTurnChild
//	    传没传（少传 = 表永远停在 compact，而界面上完全看不出来）；
//	    以及每轮那个逐字段拷贝的 loopRunner 副本有没有把它带过去。
//	 2. TestServeLazyLoadedSetIsPerSession
//	    一条 serve 进程里会并发/切换多个会话。装载集必须按会话隔离：
//	    A 取过的工具不许让 B 也能直接调（否则 lazy 唯一要挡的那件事就漏了）。
//	    这里用"每轮一张全新的表 + 只喂这个会话的日志"来模拟 serve 的一轮，
//	    形状与真机一致（新进程 = 新表 = 零内存态）。
//	 3. TestServeLazyReplaySurvivesServeRestart
//	    serve 每一轮都是新进程，所以"重启后同一会话还能回放"是这条链的核心；
//	    顺带钉住回放**不落盘**（会话目录里只有那一份日志，不许长出 sidecar）。
//
// 真机证据（真 serve 两轮 + 两会话隔离）见 docs/tool-table-lazy.md §10。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/modes"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// ---- 1) 接线：--tools-mode 必须跟着 --session 一起进子进程 ----

// TestServeForwardsToolsModeToTurnChild 钉住 serve 侧的**唯一**一处接线。
//
// 判据三条：
//   - 显式 lazy 时，每一轮的子进程 argv 里都有 --tools-mode lazy；
//   - 同一个 serve 进程里换会话（A 然后 B），argv 带的仍是各自会话 + 同一形态
//     ——副本漏字段的话，第二次就会掉；
//   - 没显式打开时（toolsMode 为空）**不传**这个开关：默认仍是 compact，
//     lazy 只能显式开。
func TestServeForwardsToolsModeToTurnChild(t *testing.T) {
	home := t.TempDir()
	capture := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", capture)
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")

	// cmdServe 就是把 resolveToolsMode 的结果塞进 loopRunner.toolsMode。
	tm, err := resolveToolsMode("lazy", defaultToolSchema)
	if err != nil {
		t.Fatal(err)
	}
	if tm != toolsModeLazy {
		t.Fatalf("显式给 lazy 必须解析成 lazy，拿到 %q", tm)
	}
	base := &loopRunner{home: home, policy: "deny", maxSteps: 6, model: "m", toolsMode: tm}
	runner := &modelOverrideLoopRunner{base: base}

	// A、B 两个会话在**同一条** serve 进程里先后跑（这就是"会话切换"）。
	for _, sid := range []string{"session-serve-a", "session-serve-b"} {
		if err := runner.Run(context.Background(), sid, "turn-"+sid, "prompt"); err != nil {
			t.Fatalf("Run(%s): %v", sid, err)
		}
		_, args := readLoopModelArg(t, capture)
		if !containsArgPair(args, "--tools-mode", toolsModeLazy) {
			t.Fatalf("%s 这一轮的子进程没带 --tools-mode lazy：%v\n"+
				"（没有它，serve 这条链根本没接上懒加载：子进程会用默认的 compact，\n"+
				" 表里 57 个工具全可见，装载集也无从回放——而界面上看不出任何异常）", sid, args)
		}
		if !containsArgPair(args, "--session", sid) {
			t.Fatalf("%s 这一轮带上了别人的会话：%v", sid, args)
		}
		for _, other := range []string{"session-serve-a", "session-serve-b"} {
			if other != sid && strings.Contains(strings.Join(args, " "), other) {
				t.Fatalf("%s 的 argv 里出现了别的会话 %s：%v", sid, other, args)
			}
		}
	}

	// 默认：不显式打开就不传——compact 仍是默认档。
	plain := &loopRunner{home: home, policy: "deny", maxSteps: 6, model: "m"}
	for _, a := range plain.childArgs("session-serve-a", "prompt") {
		if a == "--tools-mode" {
			t.Fatalf("没显式打开时不该透传 --tools-mode：%v", plain.childArgs("session-serve-a", "prompt"))
		}
	}
	// cmdServe 的默认解析结果必须是 compact（这条与 TestLazyNeedsExplicitFlag 同源，
	// 但走的是 serve 那个入口的调用形状）。
	if def, err := resolveToolsMode("", defaultToolSchema); err != nil || def != toolsModeCompact {
		t.Fatalf("serve 的默认形态必须是 compact，拿到 %q（err=%v）", def, err)
	}
	// 拼错的形态要在起服务之前报错，不能静默退回某一档。
	if _, err := resolveToolsMode("lazzy", defaultToolSchema); err == nil {
		t.Fatal("拼错的 --tools-mode 必须报错，不能静默退回 compact")
	}
}

// ---- 公共夹具：模拟 serve 起一轮（= 新进程里的 cmdRun） ----

// serveTurn 造一张**全新的** lazy 表并只喂这个会话的日志——这就是 serve 的
// 一轮在 lazy 这一面上的全部输入。真实链路里"新表"来自新进程，这里用新实例
// 等价替代（tool_lazy_state_test.go 用同一个等价关系）。
//
// 日志读不出来时按 cmdRun 的规矩办：不当致命错误，只把回放输入当空
// （back == nil），唯一允许的后果是回到"什么都没装载"。
//
// 返回值里的 dir 是**系统提示目录**（prepareLazy 拼好的那一份）。
func serveTurn(t *testing.T, reg spi.ToolRegistry, path string) (lt *lazyTable, dir string, restored []string) {
	t.Helper()
	_, _, lt, _, err := projectTable(toolsModeLazy, reg)
	if err != nil {
		t.Fatalf("lazy 投影失败: %v", err)
	}
	var back *v4.Log
	if path != "" {
		if b, rerr := v4.ReadFile(path); rerr == nil {
			back = b
		}
	}
	if dir, restored = prepareLazy(lt, back); strings.Join(lt.loadedNames(), ",") != strings.Join(restored, ",") {
		// 新表 + 只喂这一份日志 → 表里的装载必须**正好**是回放出来的那些。
		// 多了 = 有内存态漏进来（serve 里就是 A 漏给 B）；少了 = 回放没生效。
		t.Fatalf("回放之后装载集应正好是回放出来的那些：表里 %v，回放 %v", lt.loadedNames(), restored)
	}
	// 目录必须在回放**之后**拼：已装载的工具不该再列一遍。
	// （这条卡得住"return t.Directory(), restoreLoaded(...)"那种写法——Go 按
	//  书写顺序求值操作数，那样写就是先拼目录。）
	for _, n := range restored {
		if strings.Contains(dir, n+" — ") {
			t.Fatalf("已装载的 %s 还留在系统提示目录里（拼目录早于回放）：\n%s", n, dir)
		}
	}
	return lt, dir, restored
}

// newServeSession 在临时 home 里建一份真会话，返回它的 id 与日志路径。
func newServeSession(t *testing.T, home, sid string) (string, string) {
	t.Helper()
	log, err := session.CreateSession(home, "/tmp/serve-lazy", sid, modes.DefaultID)
	if err != nil {
		t.Fatalf("建会话 %s 失败: %v", sid, err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("关会话 %s 失败: %v", sid, err)
	}
	return sid, log.Path
}

// loadToolInSession 走一遍真实的 tool_load（投影层那条路），并把同一形状的
// tool/call 事件写进会话日志——真机里这一步由 agent.Loop 的 EventToolCall →
// cmdRun 的 Emit 完成。
func loadToolInSession(t *testing.T, reg spi.ToolRegistry, path, callID string, names ...string) {
	t.Helper()
	_, _, lt, _, err := projectTable(toolsModeLazy, reg)
	if err != nil {
		t.Fatal(err)
	}
	args := `{"names":[` + quoteAll(names) + `]}`
	inner := runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		t.Error("tool_load 不该落到执行器")
		return spi.Result{}, nil
	})
	if _, err := (&lazyRunner{inner: inner, table: lt}).Run(context.Background(), toolLoadName, []byte(args)); err != nil {
		t.Fatalf("装载 %v 失败: %v", names, err)
	}
	for _, n := range names {
		if _, ok := lt.Get(n); !ok {
			t.Fatalf("装载 %s 之后应能 Get 到", n)
		}
	}
	logw, err := session.OpenSessionAt(path)
	if err != nil {
		t.Fatalf("打开会话写入失败: %v", err)
	}
	defer logw.Close()
	if err := logw.Append("tool/call",
		session.ToolCallEvent(1, 1, callID, toolLoadName, args)); err != nil {
		t.Fatal(err)
	}
	if err := logw.Append("tool/result", session.ToolResultEvent(
		1, 1, callID, []spi.Block{{Type: "text", Text: "已装载"}}, false, "/tmp/serve-lazy", nil)); err != nil {
		t.Fatal(err)
	}
}

func quoteAll(names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, `"`+n+`"`)
	}
	return strings.Join(parts, ",")
}

// TestAnnounceToolsMode 钉住"打到屏幕上的形态必须等于实际用的那个"。
//
// 和审批策略同一条理由：lazy 下"模型说没有这个工具"是**预期行为**（它该先
// tool_load）。播报里看不出这一档，机制就会被当成故障去查。
func TestAnnounceToolsMode(t *testing.T) {
	var b strings.Builder
	announceToolsMode(&b, toolsModeLazy)
	if !strings.Contains(b.String(), toolsModeLazy) || !strings.Contains(b.String(), toolLoadName) {
		t.Fatalf("lazy 的播报要说明表外工具先 %s：%s", toolLoadName, b.String())
	}
	b.Reset()
	announceToolsMode(&b, toolsModeCompact)
	if !strings.Contains(b.String(), toolsModeCompact) {
		t.Fatalf("播报里没有实际形态：%s", b.String())
	}
	b.Reset()
	announceToolsMode(&b, "") // 空 = 还没解析，按默认值播报
	if !strings.Contains(b.String(), defaultToolSchema) {
		t.Fatalf("空形态应按默认值播报：%s", b.String())
	}
}

// ---- 2) 会话隔离 ----
// TestServeLazyLoadedSetIsPerSession 是 serve 侧最重要的一条：
// **A 会话的装载集不许漏到 B**。
//
// 同一条 serve 进程里，A 取过 now_line 之后，B 必须照旧被挡回（并靠一次
// tool_load 自愈）——否则 lazy 唯一要挡的那件事（凭记忆直接喊隐藏工具名）
// 就成了假的，而 A 的历史里也确实没有 B 那次取用。
//
// 反过来也要成立：B 被挡回、B 自己取了一次，不许影响 A 的回放结果。
func TestServeLazyLoadedSetIsPerSession(t *testing.T) {
	const (
		sidA = "session-serve-iso-a"
		sidB = "session-serve-iso-b"
	)
	home := t.TempDir()
	reg := assembleRegistry(t)
	_, pathA := newServeSession(t, home, sidA)
	_, pathB := newServeSession(t, home, sidB)

	// A 的第一轮：模型调 tool_load 取 now_line（表外工具）。
	loadToolInSession(t, reg, pathA, "a-1", "now_line")
	// B 的第一轮：什么都没取（只有一条用户消息）。
	logB, err := session.OpenSessionAt(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if err := logB.Append("user/message",
		session.UserMessage([]spi.Block{{Type: "text", Text: "你好"}})); err != nil {
		t.Fatal(err)
	}
	if err := logB.Close(); err != nil {
		t.Fatal(err)
	}

	// 两份日志的字节先记下来：后面的"回放"是读路径，一个字节都不许改（R5）。
	beforeA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}

	// ---- 同一进程里依次跑 A 的一轮、B 的一轮 ----
	ltA, dirA, gotA := serveTurn(t, reg, pathA)
	if strings.Join(gotA, ",") != "now_line" {
		t.Fatalf("A 回放出来的装载集不对：%v（期望 now_line）", gotA)
	}
	if !containsStr(toolNamesOf(ltA.Tools()), "now_line") {
		t.Fatal("A 回放之后 now_line 应该在这一轮的 tools 里")
	}
	if strings.Contains(dirA, "now_line — ") {
		t.Fatalf("A 的目录里还列着已装载的 now_line：\n%s", dirA)
	}

	ltB, dirB, gotB := serveTurn(t, reg, pathB)
	if len(gotB) != 0 {
		t.Fatalf("B 从没取过工具，回放必须是空集，实际 %v", gotB)
	}
	if got := ltB.loadedNames(); len(got) != 0 {
		t.Fatalf("B 的表里出现了 A 的装载：%v", got)
	}
	if containsStr(toolNamesOf(ltB.Tools()), "now_line") {
		t.Fatal("now_line 出现在 B 的 tools 里了：A 的装载集漏到了 B")
	}
	if !strings.Contains(dirB, "now_line — ") {
		t.Fatalf("B 的目录里应该有 now_line（它只是没装载，不是不存在）：\n%s", dirB)
	}

	// A 那一侧：直接可用（不挡回、不落到别的工具）。
	callsA := 0
	innerA := runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		callsA++
		return spi.Result{Summary: "执行了"}, nil
	})
	if _, err := (&lazyRunner{inner: innerA, table: ltA}).Run(context.Background(), "now_line", nil); err != nil {
		t.Fatalf("A 装载过 now_line，这一轮必须直接可用: %v", err)
	}
	if callsA != 1 {
		t.Fatalf("A 的 now_line 应落到执行器一次，实际 %d", callsA)
	}

	// B 那一侧：必须被挡回，而且被挡回的调用不许落到执行器。
	callsB := 0
	innerB := runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		callsB++
		return spi.Result{Summary: "执行了"}, nil
	})
	rB := &lazyRunner{inner: innerB, table: ltB}
	res, err := rB.Run(context.Background(), "now_line", nil)
	if err == nil {
		t.Fatal("B 没装载过 now_line，必须被挡回——A 的装载集不许漏过来")
	}
	if callsB != 0 {
		t.Fatalf("被挡回的调用不该落到执行器（calls=%d）", callsB)
	}
	if !strings.Contains(res.Hint, toolLoadName) || !strings.Contains(res.Hint, "now_line") {
		t.Fatalf("挡回的 Hint 要指出用哪个工具取什么：%q", res.Hint)
	}
	// 但 B 得**知道它存在**：目录里还留着它，否则模型连取什么都不知道。
	if !strings.Contains(ltB.Directory(), "now_line — ") {
		t.Fatalf("B 的目录里应该有 now_line（它只是没装载，不是不存在）：%s", ltB.Directory())
	}
	// 自愈：B 自己取一次就通。
	if _, err := rB.Run(context.Background(), toolLoadName, []byte(`{"names":["now_line"]}`)); err != nil {
		t.Fatalf("B 自愈装载失败: %v", err)
	}
	if _, err := rB.Run(context.Background(), "now_line", nil); err != nil {
		t.Fatalf("B 自愈之后必须能直接调: %v", err)
	}
	if callsB != 1 {
		t.Fatalf("B 自愈后应落到执行器一次，实际 %d", callsB)
	}

	// 反向：B 自愈之后，A 那一轮的表**没被改**（隔离是双向的）。
	if got := ltA.loadedNames(); strings.Join(got, ",") != "now_line" {
		t.Fatalf("B 的取用改动了 A 的表：%v", got)
	}

	// 顺序无关：先跑 B 再跑 A，结果一样（不存在"谁先跑谁决定"的隐藏共享态）。
	ltB2, _, gotB2 := serveTurn(t, reg, pathB)
	ltA2, _, gotA2 := serveTurn(t, reg, pathA)
	if len(gotB2) != 0 {
		t.Fatalf("颠倒顺序后 B 的回放仍必须是空集，实际 %v", gotB2)
	}
	if strings.Join(gotA2, ",") != "now_line" {
		t.Fatalf("颠倒顺序后 A 仍应回放出 now_line，实际 %v", gotA2)
	}
	if _, err := (&lazyRunner{inner: runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		return spi.Result{}, nil
	}), table: ltB2}).Run(context.Background(), "now_line", nil); err == nil {
		t.Fatal("颠倒顺序后 B 仍应被挡回")
	}
	if _, err := (&lazyRunner{inner: runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		return spi.Result{}, nil
	}), table: ltA2}).Run(context.Background(), "now_line", nil); err != nil {
		t.Fatalf("颠倒顺序后 A 仍应直接可用: %v", err)
	}

	// 读路径不写盘：两份会话日志一个字节都没变。
	afterA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	afterB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterA) != string(beforeA) {
		t.Fatal("回放改动了 A 的会话日志：装载集是派生投影，读路径不许写盘（R5）")
	}
	if string(afterB) != string(beforeB) {
		t.Fatal("回放改动了 B 的会话日志：装载集是派生投影，读路径不许写盘（R5）")
	}
}

// ---- 3) 服务端重启后同一会话仍能回放 ----

// TestServeLazyReplaySurvivesServeRestart 钉住这条链的核心事实：
// serve 的每一轮都是**新进程**，进程里没有任何内存态能跨轮活下来，
// 装载集只能从这份会话日志里回放。
//
// 所以判据反过来写：每一"代"进程都必须从空表出发（证明它没靠内存），
// 却仍然拿到同一个装载集（证明日志里的回放是充分的）。
func TestServeLazyReplaySurvivesServeRestart(t *testing.T) {
	const sid = "session-serve-restart"
	home := t.TempDir()
	reg := assembleRegistry(t)
	newServeSession(t, home, sid)
	path, err := session.ResolvePath(home, "", sid)
	if err != nil {
		t.Fatalf("按 id 找会话失败: %v", err)
	}

	// 第 0 轮：取一个表外工具。
	loadToolInSession(t, reg, path, "r-1", "now_line")

	// 三代"新进程"（serve 重启 / 每一轮起的那种子进程）：每代都要能回放。
	var prev *lazyTable
	for gen := 1; gen <= 3; gen++ {
		lt, state, got := serveTurn(t, reg, path)
		if strings.Join(got, ",") != "now_line" {
			t.Fatalf("第 %d 代进程回放不出装载集：%v（日志里明明有一条 tool_load now_line）", gen, got)
		}
		if lt == prev {
			t.Fatalf("第 %d 代还复用了上一代的表：那样这条测试就证明不了「跨进程」", gen)
		}
		if got := lt.loadedNames(); strings.Join(got, ",") != "now_line" {
			t.Fatalf("第 %d 代装载集不对：%v", gen, got)
		}
		// 系统提示目录跨进程一致：每一代都不能再把已装载的列一遍。
		if strings.Contains(state, "now_line — ") {
			t.Fatalf("第 %d 代的目录里还有 now_line：\n%s", gen, state)
		}
		prev = lt
	}

	// 日志长了一段（第 2 轮又取了一个工具）之后，下一代要看到**两个**。
	loadToolInSession(t, reg, path, "r-2", "schedule_list")
	lt, _, got := serveTurn(t, reg, path)
	if strings.Join(got, ",") != "now_line,schedule_list" {
		t.Fatalf("新取的装载没被下一代看到：%v（期望 now_line,schedule_list）", got)
	}
	for _, n := range []string{"now_line", "schedule_list"} {
		if _, ok := lt.Get(n); !ok {
			t.Fatalf("重启后 %s 应能 Get 到", n)
		}
	}

	// 日志读不出来（坏文件）只降级成"没装载"，不许变砖：自愈路径还在。
	bad := filepath.Join(t.TempDir(), "session.v4.jsonl.zstd")
	if err := os.WriteFile(bad, []byte("这不是一份 v4 会话文件"), 0o644); err != nil {
		t.Fatal(err)
	}
	ltBad, _, gotBad := serveTurn(t, reg, bad)
	if len(gotBad) != 0 {
		t.Fatalf("坏日志应回放出空集，实际 %v", gotBad)
	}
	res, err := (&lazyRunner{inner: runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		t.Error("被挡回的调用不该落到执行器")
		return spi.Result{}, nil
	}), table: ltBad}).Run(context.Background(), "now_line", nil)
	if err == nil {
		t.Fatal("坏日志之后仍应挡回（模型下一步 tool_load 就能拿回来）")
	}
	if !strings.Contains(res.Hint, toolLoadName) {
		t.Fatalf("挡回要带 Hint：%q", res.Hint)
	}

	// 回放不落盘：serve 的 home 里除了会话日志，不该出现任何新文件。
	// （侧车文件正是 §6.1 拒绝过的那个方案——这条红就是给"以后有人想加一个"的。）
	var stray []string
	err = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		switch info.Name() {
		case "session.v4.jsonl.zstd", "session.v4.jsonl.zstd.lock":
			return nil
		}
		stray = append(stray, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) != 0 {
		t.Fatalf("回放不该额外落盘，home 里却多了东西：%v", stray)
	}
}
