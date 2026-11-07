package wasmhost

// 恶意客人：把「宿主会不会被坏插件带走」从"wazero 应该挡住"变成可判定的断言。
//
// 背景：docs/wasm-tool-abi.md §8.7 第 6 条、§9.5 第 5 条、§10.8 第 7 条一直把
// 「攻击面 / 恶意客人」列在**未验证**里（当时原文："wazero 应该挡住，但没打"）。
// 这个文件就是去打它。五类坏行为，一类一组用例：
//
//	1. 返回巨量结果（2 MiB）  → 按长度拒绝，不读进内存再解析
//	2. call 里死循环            → deadline 真的切断，宿主进程活着
//	3. 垃圾信封（4 种）         → 每种一句自己的报错，不混成"失败"
//	4. 越界指针（含 ptr+len 溢出）→ 协议错，**不许 panic**
//	5. alloc 使坏（返回 0 / 越界偏移）→ 拒绝，不往 0 地址写
//
// 夹具是 testdata/badplugin.wasm（源码 testdata/badplugin/main.go），
// 与 toolplugin 同一个构建形态（Go reactor、-buildmode=c-shared、-buildvcs=false），
// 只是它故意不守规矩。call 的行为由 args.mode 选；alloc 的坏行为由上一次 call
// 预先武装（alloc 跑在 args 写进内存之前，读不到 mode）。
//
// 为什么每条都要挡 panic：工具调用是**进程内**的，客人把宿主 panic 掉不是
// 「这次调用失败」，是整个宿主没了。所以 invokeBad 统一 recover：
// panic 与"返回了错误"在用例层面必须能被分开看见。
//
// 测试只读入库产物，不在 go test 里现场编译（同 abi_test.go 的理由：
// 别把"这台机器有没有 Go 工具链 / wasip1 能不能编"变成测试前提）。

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// badFixture 是坏客人夹具（testdata/badplugin/main.go）的编译产物。
// 生成命令见 scripts/build-wasm-fixture.py（产物入库，测试只读）。
const badFixture = "testdata/badplugin.wasm"

// loadBadFixture 装载坏客人夹具；收尾时卸载，但**不允许收尾把用例挂住**。
//
// 为什么收尾也要防挂：如果某条坏行为真的把实例钉住了（比如死循环没被切断），
// 关实例会卡在那把锁上。用例本身已经报错了，收尾不该变成第二处事故。
func loadBadFixture(t *testing.T) *Module {
	t.Helper()
	m, err := Load(context.Background(), "badplugin", badFixture)
	if err != nil {
		t.Fatalf("装载坏客人夹具失败（%s）：%v\n夹具是不是没入库？跑 python scripts/build-wasm-fixture.py 重新生成", badFixture, err)
	}
	t.Cleanup(func() { closeWithoutHanging(m) })
	return m
}

// closeWithoutHanging 最多等 3 秒把模块关掉；关不掉就算了（见上）。
func closeWithoutHanging(m *Module) {
	done := make(chan struct{})
	go func() {
		_ = m.Close(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// invokeBad 调一次坏客人，并把它 panic 宿主这件事变成一句可读的失败。
//
// 单纯断言 err != nil 是不够的：panic 也会让用例失败，但错误信息里
// 分不清"客人坏"还是"宿主炸了"。这里是后者的唯一出口。
func invokeBad(t *testing.T, ctx context.Context, m *Module, args string) ([]byte, error) {
	t.Helper()
	var (
		out []byte
		err error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("宿主在坏客人面前 panic 了（args=%s）：%v\n"+
					"这一层是进程内的：panic 会把整个宿主带走，比「调用失败」严重一级", args, r)
			}
		}()
		out, err = m.InvokeTool(ctx, []byte(args))
	}()
	return out, err
}

// wantProtocolError 断言错误是**宿主协议错**（不是 trap、不是超时），
// 并且错误文本里能读到给定的每一句关键词。
func wantProtocolError(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("必须返回 error，实际 nil")
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("要能被认成宿主协议错（errors.Is(err, ErrProtocol)），实际：%v", err)
	}
	if errors.Is(err, ErrGuestTrap) {
		t.Fatalf("协议错不是客人崩了，不该同时是 ErrGuestTrap：%v", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("错误里要能读到 %q，实际：%v", w, err)
		}
	}
}

// --- 1. 返回巨量结果 ---

// TestHostileHugeResultRejected：客人回 2 MiB 结果 → 宿主按长度拒绝。
//
// 时序是这条的重点：**先比长度、再读内存、最后才解析**。
// 夹具回的是一个**完全合法**的 JSON 信封，只是有 2 MiB——
// 所以"宿主读进来解析了"这个错误实现会让这次调用**成功**，用例立刻红；
// 而正确实现必须报协议错并说清超了多少。
func TestHostileHugeResultRejected(t *testing.T) {
	ctx := context.Background()
	m := loadBadFixture(t)

	out, err := invokeBad(t, ctx, m, `{"mode":"huge"}`)
	if err == nil {
		t.Fatalf("客人返回 2 MiB 结果，宿主必须拒绝；实际上它成功返回了 %d 字节（说明结果被读进内存并解析了）", len(out))
	}
	if len(out) != 0 {
		t.Fatalf("被拒绝的调用不该有结果字节，实际 %d 字节", len(out))
	}
	wantProtocolError(t, err, "2097152", "1048576", "超过上限", "MiB")
	// 说清"超了多大"：两个数字都得在，而且要能一眼换算成 MiB。
	if !strings.Contains(err.Error(), "2.0 MiB") {
		t.Fatalf("错误里要说清结果有 2.0 MiB：%v", err)
	}

	// 协议错不判死实例：客人只是不守约定，实例本身没坏。
	// 下一次正常调用必须照常成功，而且**不该**发生重建（重建是 trap/超时的事）。
	out2, err2 := invokeBad(t, ctx, m, `{"mode":"echo"}`)
	if err2 != nil {
		t.Fatalf("协议错之后再调一次正常请求必须成功，实际：%v", err2)
	}
	if _, ok := m.mod.Memory().Read(0, 1); !ok {
		t.Fatal("实例应当还活着")
	}
	if rep, perr := ParseReply(out2); perr != nil || rep.IsError {
		t.Fatalf("正常调用的结果信封不合法：%v / %s", perr, out2)
	}
	if got := m.Rebuilds(); got != 0 {
		t.Fatalf("协议错不该触发重建，实际 Rebuilds=%d", got)
	}
}

// --- 2. 死循环不退 ---

// TestHostileSpinLoopIsCutByTimeout：call 里死循环 → deadline 真的切断。
//
// 三条都要：错误可读（errors.Is(context.DeadlineExceeded) + 说清是被中断的）、
// 时间是**真的**被切断的（120ms 的 deadline 不会拖到几秒），
// 宿主进程活着（本次用例还能继续跑，下一次调用会重建出可用实例）。
//
// 如果 CloseOnContextDone 失效，InvokeTool 会永远不返回——那正是
// "死循环把宿主 goroutine 永久钉住"的故障本身，所以这里用看门狗把它变成断言。
func TestHostileSpinLoopIsCutByTimeout(t *testing.T) {
	m := loadBadFixture(t)

	// deadline 是安全参数（§3.4）：这里调小只是为了让用例快，语义与默认 10s 一样。
	m.SetTimeout(120 * time.Millisecond)

	out, err, elapsed := invokeWithWatchdog(t, m, `{"mode":"spin"}`, 10*time.Second)
	if err == nil {
		t.Fatalf("死循环必须被 deadline 切断；实际上它返回了结果 %q（%s）", out, elapsed)
	}
	if len(out) != 0 {
		t.Fatalf("被中断的调用不该有结果字节，实际 %d 字节", len(out))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误要能被认成 deadline 到期（errors.Is(err, context.DeadlineExceeded)），实际：%v", err)
	}
	if !strings.Contains(err.Error(), "被中断") {
		t.Fatalf("错误里要说清这次调用是**被中断**的，而不是客人 trap：%v", err)
	}
	if errors.Is(err, ErrGuestTrap) {
		t.Fatalf("超时不是客人崩了，不该同时是 ErrGuestTrap：%v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("调用 %s 就返回了：比 120ms 的 deadline 还早，这不可能是超时切断", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("切断花了 %s：deadline 是 120ms，说明 ctx 没真的传到 wazero 的循环回边", elapsed)
	}

	// 实测事实（§9.1.1）：ctx 到期后 wazero 会把**实例**关掉，不是只丢结果。
	if !m.mod.IsClosed() {
		t.Fatal("超时之后 wazero 应当已关闭实例（IsClosed=true）——没关说明打断不是真的")
	}
	if got := m.Rebuilds(); got != 0 {
		t.Fatalf("超时当次不该就地重建（客人状态不干净时不重放调用），实际 Rebuilds=%d", got)
	}

	// 宿主没挂：下一次调用自动重建，并正常返回。
	out2, err2 := invokeBad(t, context.Background(), m, `{"mode":"echo"}`)
	if err2 != nil {
		t.Fatalf("超时之后的第一次调用必须自动重建并成功，实际：%v", err2)
	}
	if rep, perr := ParseReply(out2); perr != nil || rep.IsError {
		t.Fatalf("重建之后的结果信封不合法：%v / %s", perr, out2)
	}
	if got := m.Rebuilds(); got != 1 {
		t.Fatalf("超时之后应当重建且只重建一次，实际 Rebuilds=%d", got)
	}
}

// invokeWithWatchdog 在 goroutine 里调一次，等到 watchdog 到期还没回来就判红。
//
// 为什么不在测试 goroutine 里直接调：那样一旦切断失效，go test 会挂到
// 全局超时（10 分钟），报出来的是"测试超时"而不是"宿主被客人钉住了"。
// 这两种失败对读日志的人意义完全不同。
func invokeWithWatchdog(t *testing.T, m *Module, args string, watchdog time.Duration) ([]byte, error, time.Duration) {
	t.Helper()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		out, err := m.InvokeTool(context.Background(), []byte(args))
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err, time.Since(start)
	case <-time.After(watchdog):
		t.Fatalf("调用 %s 超过 %s 还没返回：宿主没切断客人（CloseOnContextDone 失效，或者 deadline 没传进 wazero）。"+
			"这正是「死循环把宿主 goroutine 永久钉住」的故障本身", args, watchdog)
		return nil, nil, 0
	}
}

// --- 3. 返回垃圾 ---

// TestHostileGarbageRepliesHaveDistinctErrors：四种垃圾各有各的报错。
//
// "不能混成一句失败"是这条的核心：四种情况在排障时的下一步完全不同——
//   - 空结果    → 客人没给信封（忘了 return / 信封写丢了）
//   - 不是 JSON → 客人序列化坏了
//   - 不是对象 → 客人回了个数组/字符串（信封形状错）
//   - v 不对    → 两边 ABI 版本不一致（重编插件或降宿主）
//
// 所以除了"每种都报错"，还要断言四句话**两两不同**。
func TestHostileGarbageRepliesHaveDistinctErrors(t *testing.T) {
	ctx := context.Background()
	m := loadBadFixture(t)

	cases := []struct {
		mode   string
		want   string // 这句话必须出现在错误里（= 能区分是哪种垃圾）
		reason string
	}{
		{"empty", "长度 0", "客人返回了空结果"},
		{"notjson", "不是合法 JSON", "客人返回的不是 JSON"},
		{"array", "结果信封解析失败", "是 JSON 但不是对象"},
		{"v2", "v=2", "信封版本对不上"},
	}
	seen := make(map[string]string, len(cases))
	for _, c := range cases {
		out, err := invokeBad(t, ctx, m, `{"mode":"`+c.mode+`"}`)
		if err == nil {
			t.Fatalf("%s（%s）必须报错，实际成功返回 %q", c.mode, c.reason, out)
		}
		if len(out) != 0 {
			t.Fatalf("%s 被拒绝时不该有结果字节，实际 %q", c.mode, out)
		}
		wantProtocolError(t, err, c.want, "读回")
		if prev, dup := seen[err.Error()]; dup {
			t.Fatalf("%s（%s）与 %s 报了**同一句话**，排障时分不出是哪种垃圾：%v",
				c.mode, c.reason, prev, err)
		}
		seen[err.Error()] = c.mode
	}
	if len(seen) != len(cases) {
		t.Fatalf("四种垃圾应当有四句不同的报错，实际 %d 句", len(seen))
	}

	// 四条都是协议错，实例没被判死：垃圾信封不代表客人实例坏了。
	if _, err := invokeBad(t, ctx, m, `{"mode":"echo"}`); err != nil {
		t.Fatalf("垃圾信封之后，正常调用应当照常成功，实际：%v", err)
	}
	if got := m.Rebuilds(); got != 0 {
		t.Fatalf("协议错不该触发重建，实际 Rebuilds=%d", got)
	}
}

// --- 4. 越界指针 ---

// TestHostileOutOfBoundsResultPointer：结果 ptr/len 越界 → 协议错，**不许 panic**。
//
// 两条形态都要打：
//   - oob     ：ptr 远在内存之外（0xFFFF0000），len 只有 64——"长度上限"放得过它，
//     只有边界检查能拦；
//   - oobwrap ：ptr+len 在 uint32 里溢出（0xFFFFFFF0+0x20 → 0x10）。
//     宿主如果用 uint32 加出区间，就会"读成功"并把垃圾当结果；那是最隐蔽的一种错。
//
// 这条是全组最关键的：客人给的偏移是**不可信输入**，宿主读它的时候
// 一次 panic 就等于整个宿主进程没了（工具调用是进程内的）。
func TestHostileOutOfBoundsResultPointer(t *testing.T) {
	ctx := context.Background()
	m := loadBadFixture(t)

	for _, mode := range []string{"oob", "oobwrap"} {
		t.Run(mode, func(t *testing.T) {
			// invokeBad 已经兜住 panic：真 panic 会在这里变成一句"宿主 panic 了"。
			out, err := invokeBad(t, ctx, m, `{"mode":"`+mode+`"}`)
			if err == nil {
				t.Fatalf("越界指针必须报错；实际成功返回了 %d 字节（说明区间被 uint32 回绕后读成功了）", len(out))
			}
			if len(out) != 0 {
				t.Fatalf("越界被拒绝时不该有结果字节，实际 %q", out)
			}
			wantProtocolError(t, err, "不在线性内存", "当前")
			// 错误里要给出"当前内存多大"：只说"越界"没法判断是客人算错了还是宿主看错了。
			if !strings.Contains(err.Error(), "字节") {
				t.Fatalf("错误里要带上当前线性内存大小：%v", err)
			}
		})
	}

	// 越界是协议错：实例不该被判死，下一次正常调用照常成功、且不重建。
	if _, err := invokeBad(t, ctx, m, `{"mode":"echo"}`); err != nil {
		t.Fatalf("越界之后，正常调用应当照常成功（实例没坏），实际：%v", err)
	}
	if got := m.Rebuilds(); got != 0 {
		t.Fatalf("协议错不该触发重建，实际 Rebuilds=%d", got)
	}
}

// --- 5. 分配失败 ---

// TestHostileAllocReturnsZeroIsRejected：alloc 返回 0 → 拒绝，**不往 0 地址写**。
//
// 这条原本是**真漏洞**：wasm 里地址 0 是合法地址（不像 C 里是 NULL），
// 宿主 `mem.Write(0, args)` 会成功，于是 args 被盖在客人自己的数据段上，
// 而 call(0,len) 还能从 0 读到刚写进去的合法请求——看起来"调用成功"，
// 实际上宿主已经改写了客人的内存。夹具的 alloc 用上一次 call 预先武装，
// 形状就是真实攻击：先正常一次，再在随后的分配上使坏。
func TestHostileAllocReturnsZeroIsRejected(t *testing.T) {
	ctx := context.Background()
	m := loadBadFixture(t)

	// 武装：这一次 alloc 是正常的，call 里把"下一次 alloc 返回 0"记下来。
	if out, err := invokeBad(t, ctx, m, `{"mode":"arm-alloc-zero"}`); err != nil {
		t.Fatalf("武装调用本身必须成功（它就是一个正常的 call），实际 %q / %v", out, err)
	}

	before := readGuestMem(t, m, 0, 32)
	out, err := invokeBad(t, ctx, m, `{"mode":"echo"}`)
	if err == nil {
		t.Fatalf("alloc 返回 0（NULL）必须报错；实际上宿主把 args 写进了 0 地址还成功返回 %q", out)
	}
	if len(out) != 0 {
		t.Fatalf("alloc 失败时不该有结果字节，实际 %q", out)
	}
	wantProtocolError(t, err, "alloc 返回 0", "NULL")
	after := readGuestMem(t, m, 0, 32)
	if !bytes.Equal(before, after) {
		t.Fatalf("宿主往 0 地址写了东西：线性内存 [0,32) 之前是 % x，之后是 % x\n"+
			"0 在 wasm 里是合法地址，那里是客人自己的数据段——写进去等于改客人的内存", before, after)
	}

	// 实例仍然可用：下一次分配恢复正常。
	if _, err := invokeBad(t, ctx, m, `{"mode":"echo"}`); err != nil {
		t.Fatalf("alloc 失败之后，下一次正常调用必须成功，实际：%v", err)
	}
	if got := m.Rebuilds(); got != 0 {
		t.Fatalf("协议错不该触发重建，实际 Rebuilds=%d", got)
	}
}

// TestHostileAllocOutOfRangeIsRejected：alloc 返回写不进去的偏移 → 拒绝。
//
// 与"结果指针越界"同一个道理，但发生在**写**那一侧：宿主只往客人给的区间里写，
// 客人给的区间不在内存里就必须报错，不许绕过去写别处、也不许 panic。
func TestHostileAllocOutOfRangeIsRejected(t *testing.T) {
	ctx := context.Background()
	m := loadBadFixture(t)

	if out, err := invokeBad(t, ctx, m, `{"mode":"arm-alloc-far"}`); err != nil {
		t.Fatalf("武装调用本身必须成功，实际 %q / %v", out, err)
	}
	before := readGuestMem(t, m, 0, 32)

	out, err := invokeBad(t, ctx, m, `{"mode":"echo"}`)
	if err == nil {
		t.Fatalf("alloc 返回越界偏移必须报错；实际成功返回 %q", out)
	}
	if len(out) != 0 {
		t.Fatalf("alloc 失败时不该有结果字节，实际 %q", out)
	}
	wantProtocolError(t, err, "alloc", "不在线性内存", "当前")
	if !strings.Contains(err.Error(), "4294901760") {
		// 0xFFFF0000 = 4294901760：指着这个数字才能对上是客人算错了。
		t.Fatalf("错误里要报出客人给的偏移（4294901760 = 0xFFFF0000）：%v", err)
	}
	if after := readGuestMem(t, m, 0, 32); !bytes.Equal(before, after) {
		t.Fatalf("alloc 失败时宿主不该往任何地方写：线性内存 [0,32) 变了")
	}
	if _, err := invokeBad(t, ctx, m, `{"mode":"echo"}`); err != nil {
		t.Fatalf("alloc 失败之后，下一次正常调用必须成功，实际：%v", err)
	}
}

// readGuestMem 读一段客人线性内存并**拷贝**出来。
//
// 必须拷贝：wazero 的 Read 返回的是底层缓冲的子切片，不拷贝的话"改动前的快照"
// 会跟着后来的写入一起变——那样这条断言永远成立，等于没测。
func readGuestMem(t *testing.T, m *Module, offset, n uint32) []byte {
	t.Helper()
	mem := m.mod.Memory()
	if mem == nil {
		t.Fatal("夹具应当导出线性内存")
	}
	b, ok := mem.Read(offset, n)
	if !ok {
		t.Fatalf("读线性内存 [%d,%d) 失败（当前 %d 字节）", offset, offset+n, mem.Size())
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
