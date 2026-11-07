package wasmhost

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// 连调 1000 次之后，内存与 goroutine 不能一路涨。
//
// 为什么要有这条：wasm 每次装载都起一个 wazero runtime（含编译产物、线性内存），
// 而工具插件是**常驻**的——真跑起来是几万次调用。泄漏一类的毛病单次调用看不出来，
// 只有在同一个实例上反复调用才现形。
//
// 判据不是"绝对不涨"（GC 会让数字锯齿），而是：
//   - goroutine 数在跑完之后回到起点附近（漏了就是每步 +1）
//   - 堆在用完之后不显著高于跑之前（增长超过 8 MiB 就算不合格）
func TestManyToolCallsStayFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过：这条要跑 1000 次调用")
	}
	ctx := context.Background()
	m, err := Load(ctx, "stress", "testdata/toolplugin.wasm")
	if err != nil {
		t.Fatalf("装载失败：%v", err)
	}
	defer m.Close(ctx)
	m.SetToolName("echo")

	// 先跑几次把 lazily 分配的东西（编译缓存、GC 目标）稳定下来，
	// 否则第一次调用那一下会被算成"增长"。
	for i := 0; i < 5; i++ {
		if _, err := m.InvokeTool(ctx, []byte(`{"text":"warm"}`)); err != nil {
			t.Fatalf("预热失败：%v", err)
		}
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	g0 := runtime.NumGoroutine()

	const n = 1000
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := m.InvokeTool(ctx, []byte(`{"text":"x"}`)); err != nil {
			t.Fatalf("第 %d 次调用失败：%v", i, err)
		}
	}
	elapsed := time.Since(start)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	g1 := runtime.NumGoroutine()

	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 8<<20 {
		t.Fatalf("%d 次调用后堆增长 %d 字节（>8 MiB），像是每次都留了东西", n, grew)
	}
	if g1 > g0+2 {
		t.Fatalf("goroutine 从 %d 涨到 %d：每次调用漏了 goroutine", g0, g1)
	}
	t.Logf("%d 次调用用时 %v（%.1f µs/次），堆 %d → %d，goroutine %d → %d",
		n, elapsed, float64(elapsed.Microseconds())/n, before.HeapAlloc, after.HeapAlloc, g0, g1)
}

// 反复 trap 之后重建，也不能越重建越涨。
//
// 重建会关旧实例、开新实例；如果关得不干净（句柄、runtime 留着），
// 崩溃一次涨一点，跑一天就是灾难。
func TestRepeatedRebuildsStayFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过：这条要炸 50 次")
	}
	ctx := context.Background()
	m, err := Load(ctx, "rebuild", "testdata/toolplugin.wasm")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx)
	m.SetToolName("echo")

	if _, err := m.InvokeTool(ctx, []byte(`{"text":"ok"}`)); err != nil {
		t.Fatalf("先来一次正常调用：%v", err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	g0 := runtime.NumGoroutine()

	const rounds = 50
	for i := 0; i < rounds; i++ {
		// 炸一次（trap），下一次调用会触发重建
		if _, err := m.InvokeTool(ctx, []byte(`{"boom":true}`)); err == nil {
			t.Fatalf("第 %d 次：trap 应当报错", i)
		}
		if _, err := m.InvokeTool(ctx, []byte(`{"text":"again"}`)); err != nil {
			t.Fatalf("第 %d 次重建后调用失败：%v", i, err)
		}
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	g1 := runtime.NumGoroutine()

	if got := m.Rebuilds(); got < rounds {
		t.Fatalf("重建次数 %d 少于预期（至少 %d）", got, rounds)
	}
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 8<<20 {
		t.Fatalf("%d 轮重建后堆增长 %d 字节", rounds, grew)
	}
	if g1 > g0+2 {
		t.Fatalf("goroutine 从 %d 涨到 %d：重建漏了 goroutine", g0, g1)
	}
	t.Logf("%d 轮重建（Rebuilds=%d），堆 %d → %d，goroutine %d → %d",
		rounds, m.Rebuilds(), before.HeapAlloc, after.HeapAlloc, g0, g1)
}
