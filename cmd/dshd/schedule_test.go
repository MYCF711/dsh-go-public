package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/internal/schedule"
	"dshgo/internal/session"
)

// 这个文件钉住 cmdSchedule 里唯一与放行有关的那处：
// 循环体把 schedulePolicy 算出来的策略（不是 --policy 原值）交给 cmdRun。
//
// 为什么值钉：定时任务是**无人值守**的，提示词由模型写、授权在登记时给。
// 循环体里把 itemPolicy 写成 *policy，只读任务就会带着 --policy=allow 跑起来，
// 而屏幕上那行提示还写着"只读"——没有任何一层会报错。

// runCmdSchedule 跑一次 cmdSchedule，把 stdout 收下来（它只用 fmt.Printf）。
func runCmdSchedule(t *testing.T, argv ...string) string {
	t.Helper()
	old := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	os.Stdout = pw
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, pr)
		got <- b.String()
	}()
	callErr := cmdSchedule(argv)
	_ = pw.Close()
	os.Stdout = old
	out := <-got
	_ = pr.Close()
	if callErr != nil {
		t.Fatalf("cmdSchedule 出错: %v", callErr)
	}
	return out
}

// writeDueSchedule 在 <home>/state/schedules/<sid>.json 里放一条**已经到期**的任务。
func writeDueSchedule(t *testing.T, home, sid string, allowWrites bool) {
	t.Helper()
	dir := filepath.Join(home, "state", "schedules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"schedules": []schedule.Item{{
		ID: "it-1", Title: "探针任务", Prompt: "看一眼就好", Kind: "every",
		EverySec: 3600,
		NextAt:   time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		// 这一位决定到点时能不能动写类工具。
		AllowWrites: allowWrites,
	}}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// prepareScheduleProbe 把进程切到一个只有最小 manifest 的临时工作区，
// 并把那条任务要续的会话预建出来。
//
// 为什么要 chdir：cmdSchedule 调 cmdRun 时不传 --manifest，cmdRun 用的是相对的
// "dsh.toml"。换 cwd 才能让那条链路接在一个只装了 tools 模块的工作区上，
// 而不是把整张工具表（含真 bash/fs）都装进测试进程。
func prepareScheduleProbe(t *testing.T) (home, sid string) {
	t.Helper()
	base := t.TempDir()
	manifest := "# 测试用最小 manifest：cmdRun 只需要 ToolsKey / ToolRunnerKey\n" +
		"[[module]]\nid = \"tools\"\n"
	if err := os.WriteFile(filepath.Join(base, "dsh.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	// cmdRun 会写全局 rulesPath（--agents 的默认值也是它），跑完还原，
	// 免得后面用 buildSystemPrompt 的测试拿到这次留下的值。
	oldRules := *rulesPath
	t.Cleanup(func() { *rulesPath = oldRules })

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	home = t.TempDir()
	sid = "sched-probe"
	l, err := session.CreateSession(home, wd, sid, "default")
	if err != nil {
		t.Fatalf("预建会话失败: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return home, sid
}

// TestScheduleRunHandsPolicyToCmdRun 用一个**不合法的 --policy** 当探针：
// cmdRun 认出它会把它原样打进错误里（未知 --policy "..."）。
// 于是两个方向都能观测：
//
//	allow_writes=true  → 原值必须原样传下去（探针必须出现）
//	allow_writes=false → 循环体必须先换成 deny（探针必须不出现），
//	                     并且要真的走到模型调用那一步，
//	                     证明它不是"在更早的地方失败所以看起来没问题"。
func TestScheduleRunHandsPolicyToCmdRun(t *testing.T) {
	// 只读那条会走到模型调用：指到一个没人听的端口，让它立刻失败，
	// 绝不碰真网关（3425 上是用户正在用的 magpie）。
	t.Setenv("DSHD_LLM_BASE", "http://127.0.0.1:1/v1")

	const probe = "bogus"

	t.Run("allow_writes=true：--policy 原样传给 cmdRun", func(t *testing.T) {
		home, sid := prepareScheduleProbe(t)
		writeDueSchedule(t, home, sid, true)

		out := runCmdSchedule(t, "run", "--home", home, "--policy", probe)
		if !strings.Contains(out, `未知 --policy "`+probe+`"`) {
			t.Fatalf("登记时开了 allow_writes，--policy 就必须原样到 cmdRun 手上；"+
				"没看到探针说明它被吞了或换掉了：\n%s", out)
		}
	})

	t.Run("allow_writes=false：换成了 deny，原值不能漏下去", func(t *testing.T) {
		home, sid := prepareScheduleProbe(t)
		writeDueSchedule(t, home, sid, false)

		out := runCmdSchedule(t, "run", "--home", home, "--policy", probe)
		if strings.Contains(out, `未知 --policy "`+probe+`"`) {
			t.Fatalf("只读的定时任务把 --policy 原值漏给了 cmdRun——"+
				"那等于把这条任务从只读换成了可写：\n%s", out)
		}
		// 前提：这一轮真的进了 cmdRun 的模型调用（策略检查在它之前）。
		// 否则上面那条"没出现探针"可能只是它在更早的地方就失败了。
		if !strings.Contains(out, "步模型调用失败") {
			t.Fatalf("这一轮没有走到模型调用，说明探针没有真的经过策略那一段：\n%s", out)
		}
	})
}
