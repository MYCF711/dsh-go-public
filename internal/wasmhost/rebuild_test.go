package wasmhost

// 缺口 1 的测试：实例死了要能重建。
//
// 这一组的共同前提是**实测出来的 wazero 行为**（本文件不假设，直接钉住）：
//   - 客人 trap 之后 `IsClosed()` 仍是 false —— wazero 不关实例，
//     但客人是 panic 出来的，Go 运行时状态没人担保。所以"死"必须由宿主自己判，
//     不能靠 IsClosed 一个信号；
//   - 超时/取消之后 wazero 真的把 module 关掉（`module closed with context deadline exceeded`）。
//     不重建的话这个模块就永久坏了，只能重启宿主。
//
// 每条用例都断言 `Rebuilds()`：只证明"第二次调用成功了"是不够的，
// 那也可能只是"实例其实没死"。要证明的是**重建发生过，而且只发生一次**。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestInvokeToolRebuildsAfterTrap 是缺口 1 的主用例：先炸一次，再正常调一次。
//
// 顺序不能反：先成功再 trap 只能证明"TrapError 报出来了"，
// 证明不了"下一次调用会自愈"。
func TestInvokeToolRebuildsAfterTrap(t *testing.T) {
	ctx := context.Background()
	m := loadToolFixture(t)
	m.SetToolName("echo")

	// 1) 先炸：客人 panic → trap。
	out, err := m.InvokeTool(ctx, []byte(`{"boom":true}`))
	if err == nil {
		t.Fatalf("客人 panic 必须返回 error，实际拿到结果 %q", out)
	}
	if !errors.Is(err, ErrGuestTrap) {
		t.Fatalf("错误要能被认成客人 trap，实际：%v", err)
	}
	if got := m.Rebuilds(); got != 0 {
		t.Fatalf("trap 当次不该重建（那会在客人状态不干净时重放调用），实际 Rebuilds=%d", got)
	}
	// 钉住实测事实：wazero 在 trap 之后**不**关实例，所以宿主必须自己判死。
	if m.mod.IsClosed() {
		t.Log("注意：这一版 wazero 在 trap 之后把实例关掉了 —— 判死逻辑仍然成立（更严格）")
	}

	// 2) 再正常调一次：必须成功，而且中间重建过一次。
	out2, err2 := m.InvokeTool(ctx, []byte(`{"text":"重建之后"}`))
	if err2 != nil {
		t.Fatalf("trap 之后的正常调用必须成功（实例应当被自动重建），实际：%v", err2)
	}
	rep, perr := ParseReply(out2)
	if perr != nil {
		t.Fatalf("重建之后的结果信封不合法：%v（原文 %s）", perr, out2)
	}
	if rep.IsError {
		t.Fatalf("重建之后的调用不该是工具失败：%s", out2)
	}
	if !strings.Contains(rep.Text, "重建之后") {
		t.Fatalf("重建之后应当真的调到了客人，实际 text=%q", rep.Text)
	}
	if got := m.Rebuilds(); got != 1 {
		t.Fatalf("trap 之后应当自动重建 1 次，实际 %d 次", got)
	}
	if got := m.InstanceGen(); got != 2 {
		t.Fatalf("重建之后实例代数应当是 2，实际 %d", got)
	}
	// 装载参数是宿主侧的，不该因为重建丢：工具名还要写进请求信封。
	if got := m.ToolName(); got != "echo" {
		t.Fatalf("重建不该丢掉工具名，实际 %q", got)
	}

	// 3) 重建之后还能继续正常调（不是"只能成功一次"）。
	if _, err := m.InvokeTool(ctx, []byte(`{"text":"再一次"}`)); err != nil {
		t.Fatalf("重建之后应当能反复调用，实际：%v", err)
	}
	if got := m.Rebuilds(); got != 1 {
		t.Fatalf("正常调用不该触发重建，实际累计 %d 次", got)
	}
}

// TestConcurrentCallsRebuildOnce：两个以上 goroutine 同时撞上死实例，只许重建一次。
//
// 为什么这件事重要：重建会实例化一个新实例。若两个 goroutine 各自重建，
// 就有一个实例被漏在 wazero 的 store 里（线性内存白占 3.5 MiB），
// 而且它们可能开始各调各的实例——客人状态分裂，这种 bug 在生产里几乎查不出来。
//
// onRebuild 里故意睡一会儿：如果实现是"先判死、放锁、再重建"，
// 窗口会被这个 sleep 撑开，计数立刻变成 2。计数 = 1 才说明决策和重建在同一把锁里。
func TestConcurrentCallsRebuildOnce(t *testing.T) {
	ctx := context.Background()
	m := loadToolFixture(t)

	// 先把它弄死。
	if _, err := m.InvokeTool(ctx, []byte(`{"boom":true}`)); !errors.Is(err, ErrGuestTrap) {
		t.Fatalf("先要炸出一次 trap，实际：%v", err)
	}

	// 放慢重建：让"并发撞死实例"的窗口足够宽。
	m.mu.Lock()
	m.onRebuild = func() { time.Sleep(30 * time.Millisecond) }
	m.mu.Unlock()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	texts := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			out, err := m.InvokeTool(ctx, []byte(`{"text":"并发"}`))
			if err != nil {
				errs[i] = err
				return
			}
			if rep, perr := ParseReply(out); perr == nil {
				texts[i] = rep.Text
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发调用失败：%v", i, errs[i])
		}
		if !strings.Contains(texts[i], "并发") {
			t.Fatalf("第 %d 个并发调用没真的调到客人：%q", i, texts[i])
		}
	}
	if got := m.Rebuilds(); got != 1 {
		t.Fatalf("%d 个 goroutine 同时撞上死实例，只许重建 1 次，实际 %d 次"+
			"（多出来的那几次说明重建跑在锁外面）", n, got)
	}
}

// TestRebuildFailureSaysWhy：重建也失败时，错误必须同时说清
// "实例为什么死" 与 "重建为什么失败"。
//
// 怎么造：先让客人 trap（实例判死），再绕过 Close 直接把 runtime 关掉——
// 这模拟"运行环境没了"（宿主自己关过 runtime、或者资源已经还回去了）：
// 实例确实不可用，而重建也真的会失败。
func TestRebuildFailureSaysWhy(t *testing.T) {
	ctx := context.Background()
	m := loadToolFixture(t)

	// 1) 先弄死实例（trap），这样下一次调用才会走重建。
	if _, err := m.InvokeTool(ctx, []byte(`{"boom":true}`)); err == nil {
		t.Fatal("trap 必须返回错误")
	}

	// 2) 把运行环境抽掉：重建一定失败。
	m.mu.Lock()
	rt := m.runtime
	m.mu.Unlock()
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("关 runtime 失败：%v", err)
	}

	_, err := m.InvokeTool(ctx, []byte(`{"text":"还能调吗"}`))
	if err == nil {
		t.Fatal("重建不可能成功（runtime 已关），必须报错而不是假装成功")
	}
	msg := err.Error()
	for _, want := range []string{"实例已失效", "自动重建也失败了", "trap"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("重建失败的错误里要有 %q（既要说明为什么死，也要说明重建为什么失败）：\n%s", want, msg)
		}
	}
	// 失败之后不能变成"每次调用都重建一遍"：仍然是一次尝试，然后老实报错。
	if got := m.Rebuilds(); got != 0 {
		t.Fatalf("重建没成功就不该计数，实际 %d", got)
	}
	// 第二次失败要把"上一次重建也失败过"带上，别让人以为这次是新问题。
	_, err = m.InvokeTool(ctx, []byte(`{"text":"再来"}`))
	if err == nil {
		t.Fatal("修好之前应当持续报错")
	}
	if !strings.Contains(err.Error(), "重建也失败过") {
		t.Fatalf("持续的失败应当说明上一次重建也失败过：%v", err)
	}
}

// TestCallTimeoutThenRebuild：超时把实例关掉之后，下一次调用要能自愈。
//
// 用纯 wasm 夹具的 spin()（空死循环）：它是**唯一**能让超时真的发生的装置，
// 而且它不依赖 Go 运行时，装载只要几毫秒。这条同时钉住 §3.4 的那半句：
// ctx 到期 = 实例没了（不是"这次调用失败而已"）。
func TestCallTimeoutThenRebuild(t *testing.T) {
	ctx := context.Background()
	m, err := Load(ctx, "fixture", fixture)
	if err != nil {
		t.Fatalf("装载失败：%v", err)
	}
	defer m.Close(context.Background())

	cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err := m.Call(cctx, "spin"); err == nil {
		t.Fatal("spin() 是死循环，超时必须返回错误")
	}
	// 超时真的把实例关了——这是 wazero 的行为，不是我们的判断。
	if !m.mod.IsClosed() {
		t.Log("注意：这一版 wazero 超时之后没有关实例 —— 判死逻辑仍然成立（我们自己标）")
	}

	got, err := m.Call(ctx, "add", 2, 3)
	if err != nil {
		t.Fatalf("超时之后的调用必须自动重建并成功，实际：%v", err)
	}
	if len(got) != 1 || got[0] != 5 {
		t.Fatalf("重建之后 add(2,3) = %v，期望 5", got)
	}
	if n := m.Rebuilds(); n != 1 {
		t.Fatalf("超时之后应当重建 1 次，实际 %d", n)
	}
}

// TestClosedModuleDoesNotRebuild：Close 是**终点**，不是"死"。
//
// 卸载之后自动重建等于把卸载撤销：调用方以为资源已经还了，
// 实际上进程又装了一个实例回来。这两件事必须分开。
func TestClosedModuleDoesNotRebuild(t *testing.T) {
	ctx := context.Background()
	m := loadToolFixture(t)
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}

	if _, err := m.InvokeTool(ctx, []byte(`{"text":"x"}`)); err == nil {
		t.Fatal("已卸载的模块不该还能调用")
	} else if !strings.Contains(err.Error(), "已卸载") {
		t.Fatalf("错误要说清是已卸载（而不是别的失败）：%v", err)
	}
	if got := m.Rebuilds(); got != 0 {
		t.Fatalf("Close 之后不许重建，实际重建 %d 次", got)
	}
}
