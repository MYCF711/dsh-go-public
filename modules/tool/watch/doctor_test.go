package watch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// stubGh 把 runCmd 换成假实现：按命令关键字决定成功还是失败，**不真的联网**。
func stubGh(t *testing.T, fn func(name string, args []string) ([]byte, error)) {
	t.Helper()
	old := runCmd
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) { return fn(name, args) }
	t.Cleanup(func() { runCmd = old })
}

// 一个"什么都好"的假 gh。
func healthyGh(name string, args []string) ([]byte, error) {
	switch {
	case name == "gh" && len(args) > 0 && args[0] == "--version":
		return []byte("gh version 2.62.0 (2024-11-14)\n"), nil
	case name == "gh" && len(args) > 0 && args[0] == "auth":
		return []byte("github.com\n  ✓ Logged in to github.com account MYCF711\n"), nil
	case name == "gh" && len(args) > 1 && args[0] == "api":
		return []byte("MYCF711/dsh-go\n"), nil
	case name == "gh" && len(args) > 0 && args[0] == "label":
		return []byte("已处理\t处理后打的标\nother\t别的\n"), nil
	}
	return nil, errors.New("unexpected command")
}

func newDoctorTool(t *testing.T) *Tool {
	t.Helper()
	dir := t.TempDir()
	return &Tool{st: &store{dir: dir}, kind: kindDoctor}
}

// 正常路径：全绿时 Ready 为真，且 Checked 里写清验过什么。
func TestDoctorAllGreen(t *testing.T) {
	stubGh(t, healthyGh)
	tool := newDoctorTool(t)
	// 登记一个仓库（直接落盘，走 store，不经过 watch_start）
	if err := tool.st.save([]Watcher{{Repo: "MYCF711/dsh-go", AckLabel: defaultAckLabel}}); err != nil {
		t.Fatal(err)
	}

	res, err := tool.Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("体检不该返回错误：%v", err)
	}
	var got doctorResult
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("正文不是 JSON：%s", res.Text)
	}
	if !got.Ready || len(got.Repos) != 1 || !got.Repos[0].OK {
		t.Fatalf("应当全绿：%+v", got)
	}
	if len(got.Repos[0].Checked) == 0 {
		t.Fatalf("通过的检查项也要列出来，否则看不出验过什么")
	}
}

// gh 没登录：**工具不能失败**，要把问题写进 problems 并给出下一步。
//
// 这条是核心：体检工具的职责是报告事实，不是替用户把环境修好。
func TestDoctorReportsGhNotLoggedIn(t *testing.T) {
	stubGh(t, func(name string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "auth" {
			return []byte("You are not logged into any GitHub hosts."), errors.New("exit status 1")
		}
		return healthyGh(name, args)
	})
	tool := newDoctorTool(t)
	_ = tool.st.save([]Watcher{{Repo: "MYCF711/dsh-go"}})

	res, err := tool.Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("gh 没登录不是工具的错误：%v", err)
	}
	if res.Hint == "" {
		t.Fatalf("必须给下一步（gh auth login）")
	}
	if !strings.Contains(res.Text, "登录") {
		t.Fatalf("正文要说清是登录问题：%s", res.Text)
	}
	var got doctorResult
	if json.Unmarshal([]byte(res.Text), &got) == nil && got.Ready {
		t.Fatalf("没登录却报了 ready")
	}
}

// 标签不存在：这是最容易拖到"打标那一刻才炸"的坑，必须在体检里拦下。
func TestDoctorCatchesMissingAckLabel(t *testing.T) {
	stubGh(t, func(name string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "label" {
			return []byte("bug\t缺陷\nenhancement\t新功能\n"), nil
		}
		return healthyGh(name, args)
	})
	tool := newDoctorTool(t)
	_ = tool.st.save([]Watcher{{Repo: "MYCF711/dsh-go", AckLabel: "已处理"}})

	res, err := tool.Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("不该返回错误：%v", err)
	}
	if !strings.Contains(res.Text, "已处理") {
		t.Fatalf("必须点名缺的是哪个标签：%s", res.Text)
	}
}

// 一个仓库都没登记：不是错误，但要说清下一步。
func TestDoctorEmptyListIsNotError(t *testing.T) {
	stubGh(t, healthyGh)
	tool := newDoctorTool(t)
	res, err := tool.Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("空名单不算失败：%v", err)
	}
	if res.Hint == "" {
		t.Fatalf("空名单要告诉人先登记")
	}
}

// Caps：不写盘，但要起进程 + 联网。写错了审批会漏问。
func TestDoctorCaps(t *testing.T) {
	c := (&Tool{kind: kindDoctor}).Caps()
	if c.Writes {
		t.Fatalf("doctor 不写盘，不该报 Writes")
	}
	if !c.Executes || !c.Network {
		t.Fatalf("doctor 起 gh 且联网，必须报 Executes+Network，实际 %+v", c)
	}
	if !c.NeedsApproval() {
		t.Fatalf("要联网就该触发审批")
	}
}
