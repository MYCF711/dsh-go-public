package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/modes"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// lazy 的装载集是**会话状态**，不是进程状态。
//
// 这一组测试钉住的就是这件事，而且用的是真链路：一份真的 v4 会话日志 +
// 两个互不相干的 lazyTable 实例（等价于 serve 每轮起的那个新 run 进程）。
//
// 红线三条：
//  1. 上一轮取过的工具，这一轮**直接可用**（不挡回、不再要 tool_load）；
//  2. 状态坏了/过期了，唯一允许的后果是**回到未装载**——原来那条自愈路径
//     （挡回 → Hint → tool_load）必须原样还在，工具不能因此不可用；
//  3. 不额外落盘：装载史就在那份 append-only 会话日志里，回放不新增文件。

// TestLazyLoadedSetSurvivesNewProcess 是这一轮的主判据：
// 第一轮装载 → 写进会话日志 → **新表实例**（新进程）回放 → 直接可用。
func TestLazyLoadedSetSurvivesNewProcess(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	const cwd = "/tmp/lazy-state"
	const sid = "session-lazy-state"
	log, err := session.CreateSession(home, cwd, sid, modes.DefaultID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	path := log.Path
	reg := assembleRegistry(t)

	// ---- 第一轮（新进程 #1）：模型 tool_load 取两个表外工具 ----
	view1, _, t1, _, err := projectTable(toolsModeLazy, reg)
	if err != nil {
		t.Fatal(err)
	}
	const args = `{"names":["now_line","schedule_list"]}`
	inner1 := runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		t.Error("tool_load 不该落到执行器")
		return spi.Result{}, nil
	})
	if _, err := (&lazyRunner{inner: inner1, table: t1}).Run(ctx, toolLoadName, []byte(args)); err != nil {
		t.Fatalf("第一轮装载失败: %v", err)
	}
	for _, n := range []string{"now_line", "schedule_list"} {
		if !containsStr(toolNamesOf(view1.Tools()), n) {
			t.Fatalf("第一轮装载后 %s 应该在表里", n)
		}
	}
	// 真实链路里这条事件由 agent.Loop 的 EventToolCall → cmdRun 的 Emit 写入；
	// 这里直接写同一条，形状完全一致（name=tool_load，arguments 是 JSON 字符串）。
	if err := log.Append("tool/call",
		session.ToolCallEvent(1, 1, "call-1", toolLoadName, args)); err != nil {
		t.Fatal(err)
	}
	if err := log.Append("tool/result", session.ToolResultEvent(
		1, 1, "call-1", []spi.Block{{Type: "text", Text: "已装载"}}, false, cwd, nil)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// 不新增落盘文件：会话目录里只有那一份日志（.lock 已随 Close 清掉）。
	// 这条是给"以后有人想加个 sidecar 文件"准备的——加了他会先看到这条红。
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "session.v4.jsonl.zstd" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("装载集不该额外落盘，会话目录里却多了东西：%v", names)
	}

	// ---- 第二轮（新进程 #2）：全新表实例，内存里不可能有上一轮的状态 ----
	view2, _, t2, _, err := projectTable(toolsModeLazy, reg)
	if err != nil {
		t.Fatal(err)
	}
	if got := t2.loadedNames(); len(got) != 0 {
		t.Fatalf("新表实例一开始必须是空的，实际已装载 %v", got)
	}
	calls := 0
	inner := runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		calls++
		return spi.Result{Summary: "执行了"}, nil
	})
	// 回放之前：还是要挡回（证明下面的"直接可用"是回放带来的，不是本来就通）。
	if _, err := (&lazyRunner{inner: inner, table: t2}).Run(ctx, "now_line", nil); err == nil {
		t.Fatal("回放之前就调通了 now_line：这条测试证明不了任何事")
	}
	if calls != 0 {
		t.Fatalf("被挡回的调用不该落到执行器（calls=%d）", calls)
	}

	// 回放（cmdRun 里那一步的全部决策）。
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读会话日志失败: %v", err)
	}
	got := restoreLoaded(t2, back)
	if strings.Join(got, ",") != "now_line,schedule_list" {
		t.Fatalf("回放出来的装载集不对：%v（期望 now_line,schedule_list）", got)
	}

	// 判据 1：这两个工具**直接可用**——不再挡回、不再需要 tool_load。
	for _, n := range []string{"now_line", "schedule_list"} {
		if !containsStr(toolNamesOf(view2.Tools()), n) {
			t.Fatalf("%s 回放之后应该出现在这一轮的 tools 里", n)
		}
		if _, ok := view2.Get(n); !ok {
			t.Fatalf("%s 回放之后应该能 Get 到", n)
		}
		if _, err := (&lazyRunner{inner: inner, table: t2}).Run(ctx, n, nil); err != nil {
			t.Fatalf("%s 回放之后应该直接落到执行器，实际被挡回: %v", n, err)
		}
	}
	if calls != 2 {
		t.Fatalf("两个工具都应落到执行器一次，实际 %d 次", calls)
	}

	// 已装载的工具不该再出现在系统提示的目录里（重复信息 = 白花字节）。
	dir := t2.Directory()
	for _, n := range []string{"now_line", "schedule_list"} {
		if strings.Contains(dir, n+" — ") {
			t.Fatalf("已装载的 %s 不该还在目录里：%s", n, dir)
		}
	}
	if !strings.Contains(dir, "repo_here — ") {
		t.Fatalf("没装载的表外工具必须还在目录里（否则模型不知道它存在）：%s", dir)
	}

	// 幂等：同一份日志回放两次，第二次什么都不该"新装载"。
	if again := restoreLoaded(t2, back); len(again) != 0 {
		t.Fatalf("重复回放不该再装载任何东西，实际 %v", again)
	}
	if got := t2.loadedNames(); strings.Join(got, ",") != "now_line,schedule_list" {
		t.Fatalf("重复回放把装载集改了：%v", got)
	}
}

// TestLazyRestoreFallsBackToUnloaded 是判据 2：状态坏了只能降级，不能变砖。
func TestLazyRestoreFallsBackToUnloaded(t *testing.T) {
	ctx := context.Background()
	reg := assembleRegistry(t)

	// (a) 日志根本读不出来（磁盘满/文件被占/不是 v4 文件）：
	//     cmdRun 那边把 back 传成 nil，只打一行警告。
	bad := filepath.Join(t.TempDir(), "session.v4.jsonl.zstd")
	if err := os.WriteFile(bad, []byte("这不是一份 v4 会话文件"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := v4.ReadFile(bad); err == nil {
		t.Fatal("坏文件应当读不出来（否则这条测试的前置就不成立）")
	}

	_, _, lt, _, err := projectTable(toolsModeLazy, reg)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoreLoaded(lt, nil); len(got) != 0 {
		t.Fatalf("日志读不出来时装载集必须是空的，实际 %v", got)
	}
	if got := lt.loadedNames(); len(got) != 0 {
		t.Fatalf("日志读不出来时表里不该有任何装载，实际 %v", got)
	}

	// 自愈路径必须原样还在：挡回 + Hint（模型下一步照着 Hint 调 tool_load）。
	calls := 0
	inner := runnerFunc(func(context.Context, string, json.RawMessage) (spi.Result, error) {
		calls++
		return spi.Result{Summary: "执行了"}, nil
	})
	r := &lazyRunner{inner: inner, table: lt}
	res, err := r.Run(ctx, "now_line", nil)
	if err == nil {
		t.Fatal("未装载就该挡回——否则模型不会去 tool_load")
	}
	if calls != 0 {
		t.Fatalf("被挡回的调用不该落到执行器（calls=%d）", calls)
	}
	if !strings.Contains(res.Hint, toolLoadName) || !strings.Contains(res.Hint, "now_line") {
		t.Fatalf("挡回的 Hint 要指出用哪个工具取什么：%q", res.Hint)
	}
	// 一次 tool_load 就恢复，不需要重启、不需要清状态。
	if _, err := r.Run(ctx, toolLoadName, []byte(`{"names":["now_line"]}`)); err != nil {
		t.Fatalf("自愈装载失败: %v", err)
	}
	if _, err := r.Run(ctx, "now_line", nil); err != nil {
		t.Fatalf("自愈之后必须能直接调: %v", err)
	}
	if calls != 1 {
		t.Fatalf("自愈后应落到执行器一次，实际 %d", calls)
	}

	// (b) 日志能读、但里面的事件是垃圾：跳过坏的，装好的；一条都不装也不报错。
	junk, err := session.CreateSession(t.TempDir(), "/tmp/lazy-junk", "session-lazy-junk", modes.DefaultID)
	if err != nil {
		t.Fatal(err)
	}
	junkPath := junk.Path
	badArgs := []string{
		`这不是 JSON`,             // 参数根本不是 JSON
		`{"names":["没有这个工具"]}`, // 名字在装配里不存在（模块卸了/名单变了）
		`{}`,                   // 缺参数
		`null`,                 // 空
		`{"names":[]}`,         // 空数组
	}
	for i, a := range badArgs {
		if err := junk.Append("tool/call",
			session.ToolCallEvent(1, i, fmt.Sprintf("bad-%d", i), toolLoadName, a)); err != nil {
			t.Fatal(err)
		}
	}
	// 不是 tool_load 的调用：参数里就算有 names 也不算装载。
	if err := junk.Append("tool/call",
		session.ToolCallEvent(1, 10, "other", "bash", `{"names":["repo_here"]}`)); err != nil {
		t.Fatal(err)
	}
	// 一条**好的**装载事件，参数是对象（不是 JSON 字符串）——别的写入者的形状，也要认。
	if err := junk.Append("tool/call", map[string]any{
		"turn": 1, "step": 11, "callId": "obj", "name": toolLoadName,
		"arguments": map[string]any{"names": []string{"repo_here"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := junk.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(junkPath)
	if err != nil {
		t.Fatalf("这份日志本身是合法的 v4，应当读得出来: %v", err)
	}
	_, _, lt2, _, err := projectTable(toolsModeLazy, reg)
	if err != nil {
		t.Fatal(err)
	}
	got := restoreLoaded(lt2, back)
	if strings.Join(got, ",") != "repo_here" {
		t.Fatalf("垃圾事件之间那一条合法的装载应当被认出来，实际回放 %v", got)
	}

	// (c) 只有垃圾的日志：一条都不装，而且**不报错**（restoreLoaded 没有 error 返回）。
	junk2, err := session.CreateSession(t.TempDir(), "/tmp/lazy-junk2", "session-lazy-junk2", modes.DefaultID)
	if err != nil {
		t.Fatal(err)
	}
	junk2Path := junk2.Path
	if err := junk2.Append("tool/call",
		session.ToolCallEvent(1, 1, "bad", toolLoadName, `{"names":["没有这个工具"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := junk2.Close(); err != nil {
		t.Fatal(err)
	}
	back2, err := v4.ReadFile(junk2Path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, lt3, _, err := projectTable(toolsModeLazy, reg)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoreLoaded(lt3, back2); len(got) != 0 {
		t.Fatalf("全是垃圾时应当回到未装载，实际 %v", got)
	}
	if len(lt3.loadedNames()) != 0 {
		t.Fatalf("全是垃圾时表里不该有装载，实际 %v", lt3.loadedNames())
	}
}
