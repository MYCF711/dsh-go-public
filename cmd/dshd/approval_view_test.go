package main

import (
	"strings"
	"testing"
)

// 审批框里给用户看的是**要执行什么**，不是模型说它要干什么。
func TestDescribeApprovalShowsCommandNotClaim(t *testing.T) {
	args := `{"command":"rm -rf /tmp/build","description":"看看临时目录"}`
	got := describeApproval("bash", args)

	if !strings.Contains(got, "rm -rf /tmp/build") {
		t.Fatalf("必须显示真实命令: %q", got)
	}
	// 命令要在描述之前——眼睛先落到的应该是命令
	if strings.Index(got, "rm -rf") > strings.Index(got, "看看临时目录") {
		t.Fatalf("命令必须排在模型自述之前: %q", got)
	}
	// 描述要标明是模型自己说的
	if !strings.Contains(got, "模型自述") {
		t.Fatalf("模型的 description 必须标明来源: %q", got)
	}
}

// 长命令截中间：危险的部分常在尾巴。
func TestDescribeApprovalKeepsCommandTail(t *testing.T) {
	long := strings.Repeat("echo x; ", 80) + "rm -rf /"
	got := describeApproval("bash", `{"command":"`+long+`"}`)
	if !strings.Contains(got, "rm -rf /") {
		t.Fatalf("结尾的危险命令不能被截掉: %q", got)
	}
	if !strings.Contains(got, "省略") {
		t.Fatalf("要明确说中间省略了: %q", got)
	}
}

func TestDescribeApprovalWriteShowsPathAndSize(t *testing.T) {
	got := describeApproval("write", `{"file_path":"a.txt","content":"hello"}`)
	if !strings.Contains(got, "a.txt") || !strings.Contains(got, "5 字节") {
		t.Fatalf("write 要显示路径与字节数: %q", got)
	}
	if strings.Contains(got, "hello") {
		t.Fatalf("不该把正文整个塞进审批框: %q", got)
	}
}

// 认不出的参数就原样给 JSON，不能吞掉。
func TestDescribeApprovalFallsBackToRawArgs(t *testing.T) {
	got := describeApproval("bash", `{not json`)
	if !strings.Contains(got, "not json") {
		t.Fatalf("解析失败要原样显示: %q", got)
	}
}
