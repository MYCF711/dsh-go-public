package main

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/internal/transport"
)

// TestParseJobsLine 验 `@@JOBS@@ [{...}]` 的解析。
//
// 它测的是**父进程那一半**：子进程写什么、父进程能读回什么。
// 坏行必须整条丢掉（返回 false），**不许返回"半个清单"**——
// 半个清单会让界面把已经结束的任务画成还在跑。
func TestParseJobsLine(t *testing.T) {
	good := `@@JOBS@@ [{"id":"j1","command":"sleep 9","done":false,"exit_code":0,"bytes":12}]`
	jobs, ok := parseJobsLine(good)
	if !ok || len(jobs) != 1 {
		t.Fatalf("好行应当解析出 1 条：ok=%v len=%d", ok, len(jobs))
	}
	if jobs[0].ID != "j1" || jobs[0].Command != "sleep 9" || jobs[0].Done {
		t.Fatalf("字段解析错：%+v", jobs[0])
	}

	// 不是我们的行 → 不认（交给审批那一路）
	if _, ok := parseJobsLine(`@@APPROVAL@@ {"requestId":"x"}`); ok {
		t.Fatal("审批行不该被 parseJobsLine 认领")
	}
	if _, ok := parseJobsLine(`会话：/tmp/x（3 事件）`); ok {
		t.Fatal("普通诊断行不该被认领")
	}

	// 坏 JSON → 整条丢
	if _, ok := parseJobsLine(`@@JOBS@@ [{"id":`); ok {
		t.Fatal("坏 JSON 应当整条丢掉，不许返回半个清单")
	}
}

// TestParseJobsLineRoundTrip 验 emitJobs 写出的形状能被 rename 读回。
//
// 这里不碰 kernel（那要装配整个宿主），只钉"两侧的 JSON 形状一致"——
// 用 transport.JobInfo 编一遍、parseJobsLine 解一遍，字段名靠两边同构保证。
func TestParseJobsLineRoundTrip(t *testing.T) {
	in := []transport.JobInfo{
		{ID: "a", Command: "make", Done: true, ExitCode: 0, Bytes: 100},
		{ID: "b", Command: "npm test", Done: false, ExitCode: -1, Bytes: 7},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	line := "@@JOBS@@ " + string(b)
	out, ok := parseJobsLine(line)
	if !ok || len(out) != len(in) {
		t.Fatalf("往返失败：ok=%v len=%d", ok, len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("第 %d 条不一致：in=%+v out=%+v", i, in[i], out[i])
		}
	}
	if !strings.HasPrefix(line, "@@JOBS@@ ") {
		t.Fatal("前缀常量变了？")
	}
}
