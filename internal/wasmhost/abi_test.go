package wasmhost

// ABI 层的测试。
//
// 两条硬前提（本文件要钉住它们，因为它们都是"静默失败"形态）：
//   - 夹具是 Go 用 -buildmode=c-shared 编出来的 reactor（导出 _initialize），
//     装载时宿主必须真的跑过它，否则调用一律 unreachable；
//   - 客人崩溃（trap）与工具失败（isError:true）必须走**不同**的出口。
//
// 测试只读入库产物 testdata/toolplugin.wasm，**不在 go test 里现场编译**：
// 那会把"这台机器有没有 Go 工具链 / wasip1 能不能编"变成测试前提。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toolFixture 是 Go 源码夹具 internal/wasmhost/testdata/toolplugin 的编译产物。
// 生成命令见 scripts/build-wasm-fixture.py（产物入库，测试只读）。
const toolFixture = "testdata/toolplugin.wasm"

// loadToolFixture 装载工具夹具，并在用例结束时卸载。
func loadToolFixture(t *testing.T) *Module {
	t.Helper()
	ctx := context.Background()
	m, err := Load(ctx, "toolplugin", toolFixture)
	if err != nil {
		t.Fatalf("装载工具夹具失败（%s）：%v\n夹具是不是没入库？跑 python scripts/build-wasm-fixture.py 重新生成", toolFixture, err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

// TestInvokeToolRoundTrip 是 ABI 的往返：给出 args，拿回合法 JSON。
//
// 同时钉住 reactor 那件事：夹具导出 _initialize，说明它是 c-shared 构建；
// 它现在能被调用，说明 Load 真的把 _initialize 跑过了。
// 没有这一步，Go 客人会在 runtime.notInitialized 里 abort（unreachable）。
func TestInvokeToolRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := loadToolFixture(t)

	if !hasExport(m.Exports(), "_initialize") {
		t.Fatalf("夹具应当是 reactor（导出 _initialize），实际导出：%v", m.Exports())
	}
	m.SetToolName("echo")

	out, err := m.InvokeTool(ctx, []byte(`{"text":"你好"}`))
	if err != nil {
		t.Fatalf("InvokeTool 失败：%v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("结果必须是合法 JSON，实际：%q", out)
	}
	rep, err := ParseReply(out)
	if err != nil {
		t.Fatalf("结果信封解析失败：%v（原文 %s）", err, out)
	}
	if rep.IsError {
		t.Fatalf("正常调用不该是工具失败：%s", out)
	}
	if !strings.Contains(rep.Summary, "你好") {
		t.Fatalf("summary 应当包含传进去的 text，实际 %q", rep.Summary)
	}
	// UTF-8 跨边界：中文要是坏的，这里会看到替换字符而不是"你好"。
	if !strings.Contains(rep.Text, "你好") {
		t.Fatalf("text 应当包含传进去的 text（UTF-8 跨边界），实际 %q", rep.Text)
	}
}

// TestInvokeToolTrapIsError：客人 trap → 返回 error，**不是**一个空的成功结果。
//
// 这是本轮最重要的一条区分。如果 trap 被当成"工具返回了空结果"，
// 模型会拿着空结果继续往下做，而真正的原因（插件崩了）在会话里彻底消失。
func TestInvokeToolTrapIsError(t *testing.T) {
	ctx := context.Background()
	m := loadToolFixture(t)

	out, err := m.InvokeTool(ctx, []byte(`{"boom":true}`))
	if err == nil {
		t.Fatalf("客人 panic（unreachable）必须返回 error，实际拿到结果 %q", out)
	}
	if len(out) != 0 {
		t.Fatalf("trap 之后不该有结果字节，实际 %q", out)
	}
	if !errors.Is(err, ErrGuestTrap) {
		t.Fatalf("错误要能被认成客人 trap（errors.Is(err, ErrGuestTrap)），实际：%v", err)
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("trap 不是宿主协议错，不该同时是 ErrProtocol：%v", err)
	}
	// 错误信息要能让人看出"是插件崩了"，而不是"参数不对"。
	if !strings.Contains(err.Error(), "trap") {
		t.Fatalf("错误里要说清是 trap：%v", err)
	}
	if !strings.Contains(err.Error(), "call") {
		t.Fatalf("错误里要点出是哪个导出炸的：%v", err)
	}
}

// TestInvokeToolBusinessErrorIsNotTrap：结果 JSON 里 isError:true → **nil error**。
//
// 工具失败是正常返回值，不是异常：Exec 把它翻成 Result{Hint} + err，
// 由工具层决定怎么让模型看懂。wasmhost 这一层不许把它当 trap。
func TestInvokeToolBusinessErrorIsNotTrap(t *testing.T) {
	ctx := context.Background()
	m := loadToolFixture(t)

	out, err := m.InvokeTool(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("工具失败（isError:true）不该在这里变成 error：%v", err)
	}
	rep, err := ParseReply(out)
	if err != nil {
		t.Fatalf("信封解析失败：%v", err)
	}
	if !rep.IsError {
		t.Fatalf("空 text 应当被客人判成工具失败，实际 %s", out)
	}
	if rep.Hint == "" {
		t.Fatalf("工具失败必须带 hint（模型要知道下一步）：%s", out)
	}
}

// TestInvokeToolRequiresABIExports：没有 alloc/call 的模块不是工具插件。
//
// 用现成的 60 字节夹具（只有 add/spin）：它装载得起来、也能调 add，
// 但它**不符合工具 ABI**。这种情况必须报协议错并说清缺什么，
// 而不是让人第一次调用时看到一句莫名奇妙的 "没有导出函数"。
func TestInvokeToolRequiresABIExports(t *testing.T) {
	ctx := context.Background()
	m, err := Load(ctx, "fixture", fixture)
	if err != nil {
		t.Fatalf("装载基础夹具失败：%v", err)
	}
	defer m.Close(context.Background())

	out, err := m.InvokeTool(ctx, []byte(`{}`))
	if err == nil {
		t.Fatalf("没有 alloc/call 的模块不该被当成工具插件，实际返回 %q", out)
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("缺导出属于协议错（ErrProtocol），实际：%v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "alloc") || !strings.Contains(msg, "call") {
		t.Fatalf("错误里要写清 ABI 需要哪两个导出：%v", err)
	}
	if !strings.Contains(msg, "c-shared") {
		t.Fatalf("错误里要给出可执行的下一步（-buildmode=c-shared）：%v", err)
	}
}

// TestInvokeToolRejectsBadArgs：args 不是 JSON 是宿主自己的 bug，按协议错报。
func TestInvokeToolRejectsBadArgs(t *testing.T) {
	ctx := context.Background()
	m := loadToolFixture(t)

	if _, err := m.InvokeTool(ctx, []byte("这不是 JSON")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("坏 args 属于宿主协议错，实际：%v", err)
	}
	// 超过上限也别塞给客人：那是宿主该拦的。
	big := []byte(`{"text":"` + strings.Repeat("x", MaxArgsBytes) + `"}`)
	if _, err := m.InvokeTool(ctx, big); !errors.Is(err, ErrProtocol) {
		t.Fatalf("超限 args 属于宿主协议错，实际：%v", err)
	}
}

// TestParseReplyEnvelopeRules：信封的三条硬规矩。
func TestParseReplyEnvelopeRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 期望出现在错误里的关键词
	}{
		{"空结果", ``, "空结果"},
		{"不是 JSON", `{"v":1,`, "JSON"},
		{"版本不认识", `{"v":99,"summary":"a","text":"b","isError":false}`, "v=99"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseReply([]byte(c.in))
			if err == nil {
				t.Fatalf("%s 应当被拒绝", c.name)
			}
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("%s 属于协议错，实际：%v", c.name, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里要有 %q：%v", c.want, err)
			}
		})
	}
}

// TestLoadRejectsCommandModule：只导出 _start 的模块是 command 形态，
// 装载时就该报错并说清怎么重编——它跑完 main 就退出，宿主调不到任何导出。
//
// 这个夹具在测试里现拼，纯 wasm，不依赖任何工具链。
func TestLoadRejectsCommandModule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "command.wasm")
	if err := os.WriteFile(path, commandFormWasm(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), "cmd", path)
	if err == nil {
		t.Fatal("command 形态的 wasm 必须装载失败，不能装上一个调不动的模块")
	}
	msg := err.Error()
	if !strings.Contains(msg, "_start") || !strings.Contains(msg, "_initialize") {
		t.Fatalf("错误里要对比两种形态（_start / _initialize）：%v", err)
	}
	if !strings.Contains(msg, "c-shared") {
		t.Fatalf("错误里要给可执行的下一步（-buildmode=c-shared）：%v", err)
	}
}

// hasExport 判断导出表里有没有某个名字。
func hasExport(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// --- 手拼一个 command 形态的 wasm（只导出 _start） ---

// commandFormWasm 生成最小 command 形态模块：导出 _start（() -> ()），函数体只有 end。
//
// 为什么不用脚本产：这个夹具只服务一条"装载就该被拒绝"的断言，
// 入库一个文件反而会让 scripts/build-wasm-fixture.py 的产出多一个没人读的产物。
func commandFormWasm() []byte {
	body := []byte{0x00, 0x0B} // 无 local，end
	wasm := []byte("\x00asm\x01\x00\x00\x00")
	wasm = append(wasm, wasmSection(1, wasmVec([][]byte{
		{0x60, 0x00, 0x00}, // () -> ()
	}))...)
	wasm = append(wasm, wasmSection(3, wasmVec([][]byte{{0x00}}))...) // func 0: type 0
	wasm = append(wasm, wasmSection(7, wasmVec([][]byte{              // export "_start" -> func 0
		append(wasmName("_start"), 0x00, 0x00),
	}))...)
	wasm = append(wasm, wasmSection(10, wasmVec([][]byte{ // code
		append(uleb(len(body)), body...),
	}))...)
	return wasm
}

func wasmSection(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, uleb(len(payload))...)
	return append(out, payload...)
}

func wasmVec(items [][]byte) []byte {
	out := uleb(len(items))
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

func wasmName(s string) []byte {
	return append(uleb(len(s)), []byte(s)...)
}

func uleb(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7F)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}
