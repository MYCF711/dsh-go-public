package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/internal/spi"
	"time"
)

// 模型看到的正文必须是**原始输出**，不是 JSON 包裹。
//
// 之前走的是 Summary + JSON(Data)：换行被转义、引号被转义，外面还裹着
// exit_code / cwd 这些模型用不上的字段。bash 占工具回显的 64.5%，
// 这层浪费在它身上被放大得最厉害。
func TestShellViewIsRawTextNotJSON(t *testing.T) {
	requireBash(t)
	svc := NewService(t.TempDir())
	defer svc.Close()
	tool := &shellTool{svc: svc, budget: 10 * time.Second}

	res, err := tool.Exec(context.Background(),
		json.RawMessage(`{"command":"seq 3","description":"三行"}`))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(res.Text, "[exit 0]") {
		t.Fatalf("正文应以 [exit N] 开头，得到 %q", res.Text)
	}

	// 真换行：三行数字之间是 "\n" 一个字节，不是两个字符的反斜杠 n。
	want := "1" + string(rune(10)) + "2" + string(rune(10)) + "3"
	if !strings.Contains(res.Text, want) {
		t.Fatalf("正文里必须是真换行（不是转义）: %q", res.Text)
	}

	// 不能有 JSON 外壳
	if strings.Contains(res.Text, `"exit_code"`) || strings.Contains(res.Text, `"output"`) {
		t.Fatalf("正文里不该有 JSON 外壳: %q", res.Text)
	}
}

// 外置之后的正文也要是原始文本（不是 JSON 里的 preview）。
func TestSpilledShellViewStaysRaw(t *testing.T) {
	requireBash(t)
	svc := NewService(t.TempDir())
	defer svc.Close()
	tool := &shellTool{svc: svc, budget: 20 * time.Second}

	res, err := tool.Exec(context.Background(),
		json.RawMessage(`{"command":"seq 1 50","description":"50 行"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, `"output"`) {
		t.Fatalf("正文里不该有 JSON 外壳: %q", res.Text)
	}
}

// 空 job_id 是「参数没给」，不是「任务不存在」。
// 报成后者会把模型送去 job_list 找一个根本不存在的 id。
func TestJobToolsRejectEmptyID(t *testing.T) {
	svc := NewService(t.TempDir())
	defer svc.Close()

	cases := []struct {
		name string
		tool spi.Tool
	}{
		{"job_output", &jobOutputTool{svc: svc}},
		{"job_kill", &jobKillTool{svc: svc}},
	}
	for _, c := range cases {
		// 一条任务都没有时，空 id 的正确回答是"还没有任务、先起一个"，
		// 而不是"id 为空"（那会让模型补一个它编不出来的值）。
		res, err := c.tool.Exec(context.Background(), json.RawMessage(`{}`))
		if err == nil {
			t.Fatalf("%s: 空 id 且没有任务时必须报错", c.name)
		}
		if !strings.Contains(res.Hint, "还没有后台任务") {
			t.Fatalf("%s: Hint 应当说清还没有任务，实际 %q", c.name, res.Hint)
		}
		if strings.Contains(err.Error(), "没有任务") && !strings.Contains(err.Error(), "还没有后台任务") {
			t.Fatalf("%s: 不该说成「任务不存在」: %q", c.name, err.Error())
		}

		// 给了不存在的 id 才该说"没有任务"
		_, err = c.tool.Exec(context.Background(), json.RawMessage(`{"job_id":"nope"}`))
		if err == nil || !strings.Contains(err.Error(), "没有任务") {
			t.Fatalf("%s: 未知 id 应该说没这个任务，实际 %v", c.name, err)
		}
	}
}

// 只有一个后台任务时，job_output / job_kill 可以省 job_id。
// 多个时必须自己选——猜错就杀错了进程。
func TestJobToolsDefaultToSingleJob(t *testing.T) {
	svc := NewService(t.TempDir())
	defer svc.Close()

	if _, err := svc.Run("sleep 30", 150*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	out := &jobOutputTool{svc: svc}
	res, err := out.Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("只有一条任务时应当能省 job_id: %v / %q", err, res.Hint)
	}
}

func TestJobToolsRequireIDWhenMultiple(t *testing.T) {
	svc := NewService(t.TempDir())
	defer svc.Close()

	for i := 0; i < 2; i++ {
		if _, err := svc.Run("sleep 30", 150*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	kill := &jobKillTool{svc: svc}
	res, err := kill.Exec(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("多个任务时必须要求 job_id")
	}
	if !strings.Contains(res.Hint, "必须填一个") {
		t.Fatalf("Hint 要说清要选一个: %q", res.Hint)
	}
}
