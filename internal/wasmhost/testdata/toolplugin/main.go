// toolplugin 是 wasm 工具插件的**参考实现 + 测试夹具**。
//
// 它同时是「一个 wasm 插件作者要写什么」的最小答案：两个导出（alloc / call）
// 加上信封，没有 WIT、没有 component model、没有绑定库。
//
// 构建（宿主侧固定用这条命令，见 scripts/build-wasm-fixture.py）：
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -trimpath -buildvcs=false \
//	    -o ../toolplugin.wasm .
//
// `-buildvcs=false` 不是可选项：go 默认把仓库的 VCS 身份（`vcs.revision` /
// `vcs.modified`）写进 main 包的 build info，于是同一个源文件在不同 commit 上
// 编出来的 .wasm 字节不同。产物是入库夹具、manifest 又按 digest 钉内容，
// 那它就不该随仓库指针漂（实例见 docs/wasm-tool-abi.md §10.8）。
//
// 两个硬前提（docs/wasm-tool-abi.md §1 有实测支撑，不是约定俗成）：
//   - 必须 -buildmode=c-shared：默认 go build 产出的是 command 形态，
//     导出表里只有 _start，跑完就 proc_exit，宿主根本调不到 alloc/call；
//   - 必须是 reactor：宿主用 WithStartFunctions("_initialize") 先把 Go 运行时
//     拉起来，否则任何 //go:wasmexport 函数一进就被 runtime.notInitialized 打断（unreachable）。
//
// ABI（见 docs/wasm-tool-abi.md §3）：
//
//	alloc(n) -> ptr                 客人划 n 字节，返回线性内存偏移
//	call(ptr, n) -> (ptr<<32)|len    吃 args JSON，吐结果 JSON
//
// 这个夹具提供**两个**工具，按请求信封里的 tool 字段分流（§10）：
//
//	echo   把 text 原样回显（默认分支）
//	upper  把 text 里的 ASCII 字母转成大写
//
// 两个工具逻辑不同，所以"上游到底有没有把工具名送进来"是可以用输出判定的：
// 送对了是 upper:HI，没送是 echo:Hi。
//
// 为什么不引入调用约定库：两个 i32/i64 导出就够，且这样 Go / TinyGo 两种客人
// 用同一套宿主代码——切换客人编译器不需要改宿主。
package main

import (
	"encoding/json"
	"unsafe"
)

// request 是宿主写进线性内存的请求信封。
//
// tool 字段不是一个装饰：**一个模块可以挂多个工具**，客人靠它知道自己这次是谁。
// 宿主每次调用都会把当前工具名写进来（cmd/dshd 的 wasmToolRouter），
// 所以客人这边必须按它分流——不看它，两个工具就会跑到同一个分支上。
type request struct {
	V    int             `json:"v"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// reply 是客人吐出的结果信封。
//
// summary / text / isError 三个字段必填；hint 只在失败时给下一步。
// **工具失败走 isError，不走 trap**：trap 是客人崩了，宿主会当成插件内部错误。
type reply struct {
	V       int    `json:"v"`
	Summary string `json:"summary"`
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
	Hint    string `json:"hint,omitempty"`
}

// keep 让 alloc 出去的切片不被 GC 回收。
//
// 一定要有：宿主拿到的只是线性内存偏移，Go 看不见还有人在用它；
// 没有这行引用，那个缓冲区随时可能被回收、被下一个分配覆盖。
var keep [][]byte

// alloc 在客人线性内存里划 n 字节并返回偏移。宿主随后写 args 进这块区间。
//
//go:wasmexport alloc
func alloc(n uint32) uint32 {
	if n == 0 {
		return 0
	}
	b := make([]byte, n)
	keep = append(keep, b)
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

// call 是 ABI 的另一半：ptr/len 指向宿主写好的 args JSON，
// 返回值高 32 位是结果缓冲区偏移、低 32 位是长度。
//
// 三种结局都在这一个函数里，因为它们对应三种**不同的**宿主处理路径：
//   - 正常返回 + isError:false → 成功；
//   - 正常返回 + isError:true  → 工具失败（模型该看到 hint）；
//   - panic（trap）            → 客人崩溃，宿主必须当错误而不是空结果。
//
//go:wasmexport call
//go:nocheckptr
func call(ptr, n uint32) uint64 {
	var req request
	if n > 0 {
		in := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), n)
		// 坏 JSON 不在这里喊：宿主已经校验过它是合法 JSON，
		// 真坏了也属于宿主协议错，不该伪装成工具失败。
		_ = json.Unmarshal(in, &req)
	}

	var args struct {
		Text string `json:"text"`
		// Boom 只给测试用：让它 trap，验「客人崩溃」与「工具失败」是两层。
		Boom bool `json:"boom"`
	}
	_ = json.Unmarshal(req.Args, &args)

	if args.Boom {
		// 故意 panic：Go 运行时把它变成 unreachable，宿主看到的是 trap。
		panic("toolplugin: 故意 panic，用来验证 trap 不等于工具失败")
	}

	// **按 tool 字段分流**：同一个 .wasm 里两个工具，逻辑完全不同。
	//
	// 宿主没把 tool 写进信封时（老宿主 / internal/wasmhost 里不设工具名的直接调用），
	// upper 会落到 echo 分支上、返回 "echo:…"——这正是 cmd/dshd 的路由用例
	// 要钉住的失败形态：它证明"路由"不是摆设，而是这一层唯一的落点。
	if req.Tool == "upper" {
		return writeReply(upperReply(args.Text))
	}
	// echo 是默认分支：不设工具名的直接调用落在这里，行为与加 upper 之前一致。
	return writeReply(echoReply(args.Text))
}

// echoReply 是第一个工具：把 text 原样回显。
func echoReply(text string) reply {
	if text == "" {
		return reply{
			V: 1, Summary: "空输入", Text: "args.text 是空的", IsError: true,
			Hint: `传 {"text": "..."}：这个夹具工具只把 text 原样回显`,
		}
	}
	return reply{V: 1, Summary: "收到 " + text, Text: "echo:" + text}
}

// upperReply 是第二个工具：把 text 里的 ASCII 字母转成大写。
//
// 与 echo **只差 tool 字段一个条件**，所以两者的输出可以拿来判定
// "宿主到底有没有把工具名送进来"：送对了是 upper:HI，没送是 echo:Hi。
func upperReply(text string) reply {
	if text == "" {
		return reply{
			V: 1, Summary: "空输入", Text: "args.text 是空的", IsError: true,
			Hint: `传 {"text": "..."}：这个夹具工具把 text 里的 ASCII 字母转成大写`,
		}
	}
	up := upperASCII(text)
	return reply{V: 1, Summary: "大写 " + up, Text: "upper:" + up}
}

// upperASCII 只转 ASCII 字母，不引 unicode 表（那会把产物撑大几百 KB）。
// 非 ASCII 原样保留，顺带证明 UTF-8 跨边界不受影响。
func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// writeReply 把结果信封放进线性内存，返回 (ptr<<32)|len。
func writeReply(r reply) uint64 {
	out, err := json.Marshal(r)
	if err != nil {
		// 兜底：连 reply 都序列化不了说明编码本身错了，给一条合法的最小信封，
		// 免得宿主拿到 0 长度结果（那是协议错，比工具失败更难查）。
		out = []byte(`{"v":1,"summary":"内部错误","text":"结果编码失败","isError":true}`)
	}
	buf := make([]byte, len(out))
	copy(buf, out)
	keep = append(keep, buf)
	p := uint32(uintptr(unsafe.Pointer(&buf[0])))
	return uint64(p)<<32 | uint64(uint32(len(out)))
}

// main 在 reactor 形态里不会被执行（_initialize 只拉运行时，不跑 main），
// 但 c-shared 要求一个 main 包，必须有它。
func main() {}
