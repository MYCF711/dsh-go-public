package main

import (
	"context"
	"testing"
)

func TestMakeAskFuncPolicies(t *testing.T) {
	ctx := context.Background()

	allow, err := makeAskFunc("allow")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := allow(ctx, "s", "bash", "{}"); !ok {
		t.Fatal("allow 策略应该放行")
	}

	deny, err := makeAskFunc("deny")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := deny(ctx, "s", "bash", "{}"); ok {
		t.Fatal("deny 策略必须拒绝")
	}

	// auto 在非终端必须拒绝——没人能回答时不能默认同意。
	autoPipe, err := makeAskFuncWith("auto", false)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := autoPipe(ctx, "s", "bash", "{}"); ok {
		t.Fatal("非终端的 auto 必须拒绝")
	}

	// auto 在终端应该走"问"，这里只验证它没有变成自动放行：真问的时候会读 stdin，
	// 测试里给一个空 reader，答案应当是否。
	autoTTY, err := makeAskFuncWith("auto", true)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := autoTTY(ctx, "s", "bash", "{}"); ok {
		t.Fatal("终端里没回答 y 时不能放行")
	}

	if _, err := makeAskFunc("yolo"); err == nil {
		t.Fatal("未知策略必须报错")
	}
}
