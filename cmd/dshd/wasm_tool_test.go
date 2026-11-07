package main

import (
	"strings"
	"testing"
)

// `dshd wasm tool` 是按**工具名**调，不是按导出函数名调。
//
// 这条不值钱的判据有个真实来由：多工具模块里 `call` 靠信封的 tool 字段分流，
// 而 `wasm call --fn` 只认导出函数名（alloc/call），所以以前**没法从命令行验一个
// 多工具插件**——只能写 manifest 再走 dshd exec。
func TestWasmToolRequiresToolName(t *testing.T) {
	err := cmdWasmTool([]string{"--module", "x.wasm"})
	if err == nil {
		t.Fatal("缺 --tool 必须报错")
	}
	if !strings.Contains(err.Error(), "--tool") || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("要指出缺什么、以及看导出函数该用哪个命令：%v", err)
	}
}

// --args 必须是 JSON 对象：工具参数是一堆键值，不是裸值。
func TestWasmToolArgsMustBeJSONObject(t *testing.T) {
	err := cmdWasmTool([]string{"--module", "x.wasm", "--tool", "echo", "--args", "[1,2,3]"})
	if err == nil {
		t.Fatal("数组不是合法工具参数，应当报错")
	}
	if !strings.Contains(err.Error(), "JSON 对象") {
		t.Fatalf("要说清期望的是对象：%v", err)
	}
}

// 三个 flag 缺一个都要在**装载模块之前**拒绝——别拿错误参数去开 runtime。
func TestWasmToolRejectsBeforeLoading(t *testing.T) {
	err := cmdWasmTool([]string{"--module", "", "--tool", "echo"})
	if err == nil || !strings.Contains(err.Error(), "--module") {
		t.Fatalf("缺 --module 应当先报：%v", err)
	}
}
