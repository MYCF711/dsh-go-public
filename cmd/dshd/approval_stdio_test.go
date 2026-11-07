package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// H13：没有终端时，审批走 stdin/stderr 的标记通道。
//
// 这条测试同时钉住最坏情形：**读不到应答 = 拒绝**。
// 如果哪天有人把"没回答"改成"放行"，写操作的审批就形同虚设。
func TestMarkerAskReadsResponse(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"允许", approvalResponseTag + `{"allow":true}` + "\n", true},
		{"拒绝", approvalResponseTag + `{"allow":false}` + "\n", false},
		{"没应答（EOF）", "", false},
		{"应答格式不对", "随便一行\n", false},
		{"JSON 坏了", approvalResponseTag + "{不是 json}\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oldIn, oldErr := os.Stdin, os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(w, c.input)
			_ = w.Close()
			os.Stdin = r
			os.Stderr, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			defer func() {
				os.Stdin, os.Stderr = oldIn, oldErr
				_ = r.Close()
			}()

			got, err := markerAsk(context.Background(), "s1", "write", `{"path":"a.txt"}`)
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if got != c.want {
				t.Fatalf("%s：得到 allow=%v，期望 %v", c.name, got, c.want)
			}
		})
	}
}

// 开关默认关闭：没这个环境变量就不能改变原来的终端行为。
func TestApprovalViaStdioDefaultOff(t *testing.T) {
	old := os.Getenv("DSHD_APPROVAL_STDIO")
	_ = os.Unsetenv("DSHD_APPROVAL_STDIO")
	defer func() { _ = os.Setenv("DSHD_APPROVAL_STDIO", old) }()
	if approvalViaStdio() {
		t.Fatalf("没设环境变量时不该走父子通道")
	}
	_ = os.Setenv("DSHD_APPROVAL_STDIO", "1")
	if !approvalViaStdio() {
		t.Fatalf("设了 1 就该走")
	}
	if !strings.Contains(approvalRequestTag, "@@APPROVAL") {
		t.Fatalf("标记变了，父进程解析会失配")
	}
}
