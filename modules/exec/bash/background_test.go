package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// run_in_background：**立刻**拿到 job_id，不等命令结束。
//
// 这条是补官方语义：此前只能等 idle 超时被动转后台，
// 想"起个长任务马上继续"就只能把 idle 调小，而那会让短命令也误判超时。
func TestRunInBackgroundReturnsImmediately(t *testing.T) {
	svc := NewService(t.TempDir())
	tool := &shellTool{svc: svc}

	start := time.Now()
	res, err := tool.Exec(context.Background(), json.RawMessage(
		`{"command":"sleep 3; echo done","description":"长任务","run_in_background":true}`))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("后台启动失败：%v", err)
	}
	if took > 2*time.Second {
		t.Fatalf("等太久（%s）——run_in_background 应当立刻返回", took)
	}

	var d shellData
	if err := json.Unmarshal(mustJSON(res.Data), &d); err != nil {
		t.Fatal(err)
	}
	if d.JobID == "" {
		t.Fatalf("没给出 job_id：%+v", d)
	}
	if !strings.Contains(res.Summary, d.JobID) {
		t.Fatalf("摘要要点名任务：%q", res.Summary)
	}

	// 它必须真的在 jobs 表里，否则 job_output 找不到它。
	var found bool
	for _, j := range svc.List() {
		if j.ID == d.JobID {
			found = true
		}
	}
	if !found {
		t.Fatalf("job_list 里没有 %s", d.JobID)
	}

	// 收尾：别让 sleep 3 拖住整个测试进程。
	_ = svc.Kill(d.JobID)
	svc.Close()
}

// 与官方语义对齐：**只要给了 run_in_background 就一定有 job_id**，
// 哪怕命令瞬间结束。结果照样能从 job_output 立刻取到——
// 不能因为"它跑得快"就换一种返回形状，调用方要处理两种形状是负担。
func TestRunInBackgroundAlwaysYieldsJob(t *testing.T) {
	svc := NewService(t.TempDir())
	defer svc.Close()
	tool := &shellTool{svc: svc}

	res, err := tool.Exec(context.Background(), json.RawMessage(
		`{"command":"echo hi","description":"短命令","run_in_background":true}`))
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	var d shellData
	_ = json.Unmarshal(mustJSON(res.Data), &d)
	if d.JobID == "" {
		t.Fatalf("给了 run_in_background 却没有 job_id：%+v", d)
	}

	// 结果必须能立刻取到，而不是要等
	out, err := svc.Output(d.JobID, 2*time.Second)
	if err != nil {
		t.Fatalf("取输出失败：%v", err)
	}
	if !strings.Contains(out.Output, "hi") {
		t.Fatalf("输出丢了：%q", out.Output)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
