// badplugin 是一个**故意不守规矩**的客人：它只用来钉住宿主面对恶意/坏插件时的行为。
//
// 和 testdata/toolplugin 的区别只有一条：它不保证自己返回的东西是对的。
// **别照抄这个文件写插件**——正面样例是 toolplugin/main.go，这份是反面。
//
// 构建（scripts/build-wasm-fixture.py 跑同一条命令，产物入库、测试只读）：
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -trimpath -buildvcs=false \
//	    -o ../badplugin.wasm .
//
// `-buildvcs=false` 与 toolplugin 同一个理由：产物入库、manifest 按 digest 钉内容，
// 它就不该随仓库指针漂（见 docs/wasm-tool-abi.md §10.8）。
//
// 为什么不给 toolplugin 加分支：那个文件是**参考实现**，要能让人照着抄。
// 坏行为混进去以后，读它的人分不清哪些是 ABI 要求、哪些是测试用的故意破坏。
//
// 行为由请求 args 里的 `mode` 字段选择（见 call 的 switch）。**alloc 是唯一的例外**：
// 它在 args 写进线性内存之前就被调用，读不到 args，所以"下一次 alloc 怎么坏"
// 只能由上一次 call 预先武装（mode = "arm-alloc-zero" / "arm-alloc-far"）。
// 这条正好也是真实攻击的形状：客人可以先正常一次，再在随后的 alloc 上使坏。
package main

import (
	"encoding/json"
	"unsafe"
)

// request / reply 与 ABI 的信封完全一致（docs/wasm-tool-abi.md §3.2）。
type request struct {
	V    int             `json:"v"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type reply struct {
	V       int    `json:"v"`
	Summary string `json:"summary"`
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
	Hint    string `json:"hint,omitempty"`
}

// keep 让 alloc / 结果缓冲区不被 GC 回收（与 toolplugin 同一条理由）。
var keep [][]byte

// nextAlloc 是**下一次** alloc 的行为："" 正常 / "zero" 返回 0 / "far" 返回越界偏移。
//
// 只能由 call 武装，因为 alloc 跑在 args 写进内存之前——那时客人手里还没有 mode。
var nextAlloc string

// sink 让 spinForever 里的循环真的有事可做（写包级变量），
// 免得编译器把空循环整个删掉、用例变成"什么都没跑"。
var sink uint64

// alloc 在客人线性内存里划 n 字节。
//
// 坏行为：
//   - "zero" 模式返回 0：C 里那是 NULL，但在 wasm 里 0 是**合法地址**——
//     宿主如果照写不误，就会把 args 盖在客人自己的数据段上。宿主必须把 0 当分配失败。
//   - "far" 模式返回 0xFFFF0000：超出任何真实线性内存，宿主的边界检查必须拦住。
//
//go:wasmexport alloc
func alloc(n uint32) uint32 {
	mode := nextAlloc
	nextAlloc = "" // 只用一次：武装是"下一次分配"，不是永久状态
	switch mode {
	case "zero":
		return 0
	case "far":
		return 0xFFFF0000
	}
	if n == 0 {
		return 0
	}
	b := make([]byte, n)
	keep = append(keep, b)
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

// call 吃 args JSON，按 mode 吐出**各种各样的坏结果**。
//
// 每个分支对应宿主的一条防线（hostile_test.go 一条用例钉一条）：
//
//	huge           2 MiB 的**合法**信封 → 宿主必须按长度拒绝，不许读进来再解析
//	spin           死循环 → 只能靠 ctx 到期（WithCloseOnContextDone）切断
//	notjson        不是 JSON
//	array          是 JSON 但不是对象
//	v2             是对象、v 不等于 1
//	empty          长度 0（返回 0，不是"空字符串"）
//	oob            指针越界
//	oobwrap        指针 + 长度在 uint32 里溢出（最容易写成"回绕到 0x10 然后读成功"）
//	arm-alloc-zero 武装下一次 alloc 返回 0
//	arm-alloc-far  武装下一次 alloc 返回越界偏移
//
//go:wasmexport call
//go:nocheckptr
func call(ptr, n uint32) uint64 {
	var req request
	if n > 0 {
		in := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), n)
		// 坏 JSON 不在这里喊：host 一侧已经校验过；这里的 mode 读不到就等于默认分支。
		_ = json.Unmarshal(in, &req)
	}

	var args struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(req.Args, &args)

	switch args.Mode {
	case "", "echo":
		return writeRaw([]byte(`{"v":1,"summary":"正常","text":"badplugin:正常返回","isError":false}`))

	case "huge":
		// **合法**的 JSON 信封，正好 2 MiB：宿主若"读进来再解析"，这次调用会成功——
		// 而它必须失败。这就是「不许把它塞进内存还想解析」的可判定形态。
		return writeRaw(hugeEnvelope(2 << 20))

	case "spin":
		spinForever()
		return 0 // 不可达

	case "notjson":
		return writeRaw([]byte("这不是 JSON，只是几个字节"))

	case "array":
		return writeRaw([]byte(`[1,2,3]`))

	case "v2":
		return writeRaw([]byte(`{"v":2,"summary":"版本对不上","text":"信封 v=2","isError":false}`))

	case "empty":
		// 长度 0 是**协议错**，不是"工具说没事"（§3.1）。
		return 0

	case "oob":
		// 指针远在内存之外，长度本身很小（64）：宿主的"长度上限"放得过它，
		// 只有边界检查能拦。
		return pack(0xFFFF0000, 64)

	case "oobwrap":
		// ptr+len = 0x100000010，在 uint32 里回绕成 0x10：
		// 宿主如果用 uint32 算区间，就会"读成功"并把垃圾当成结果。
		return pack(0xFFFFFFF0, 0x20)

	case "arm-alloc-zero":
		nextAlloc = "zero"
		return writeRaw([]byte(`{"v":1,"summary":"已武装","text":"下一次 alloc 返回 0","isError":false}`))

	case "arm-alloc-far":
		nextAlloc = "far"
		return writeRaw([]byte(`{"v":1,"summary":"已武装","text":"下一次 alloc 返回越界偏移","isError":false}`))

	default:
		return writeRaw([]byte(`{"v":1,"summary":"未知模式","text":"badplugin 不认识这个 mode","isError":true,` +
			`"hint":"可用的 mode：huge/spin/notjson/array/v2/empty/oob/oobwrap/arm-alloc-zero/arm-alloc-far"}`))
	}
}

// hugeEnvelope 造一个**长度正好 n 字节**的合法结果信封，text 段填 'x'。
//
// 正好 n 很重要：用例要断言错误里报出来的字节数与上限，而不是"一个很大的数"。
func hugeEnvelope(n int) []byte {
	const prefix = `{"v":1,"summary":"巨量结果","text":"`
	const suffix = `","isError":false}`
	if n < len(prefix)+len(suffix) {
		panic("hugeEnvelope: n 太小")
	}
	out := make([]byte, 0, n)
	out = append(out, prefix...)
	for len(out) < n-len(suffix) {
		out = append(out, 'x')
	}
	out = append(out, suffix...)
	return out
}

// spinForever 是"死循环不退"：没有 sleep、没有 I/O，自己永远不会结束。
//
// 打断只能靠 wazero 在每个循环回边检查 ctx（CloseOnContextDone），
// 也就是宿主那条 deadline 真的传到了 wazero。
func spinForever() {
	for i := uint64(0); ; i++ {
		sink += i
	}
}

// writeRaw 把一段字节放进线代内存并返回 (ptr<<32)|len。
func writeRaw(b []byte) uint64 {
	keep = append(keep, b)
	return pack(uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b)))
}

// pack 就是 ABI 的返回编码：(ptr<<32)|len。
func pack(ptr, n uint32) uint64 {
	return uint64(ptr)<<32 | uint64(n)
}

// main 在 reactor 形态里不会被执行，但 c-shared 要求一个 main 包。
func main() {}
