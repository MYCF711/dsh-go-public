package plan

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
	"dshgo/modules/tools"
)

// fakeTool 是假工具：只记「有没有真的被执行」，能力由用例声明。
//
// 闸门判的是 Caps 不是名字，所以这里必须能伪造出 writes / executes / destructive。
type fakeTool struct {
	name string
	caps spi.Caps
	ran  int
}

func (f *fakeTool) Name() string            { return f.name }
func (f *fakeTool) Description() string     { return "测试用假工具" }
func (f *fakeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeTool) Caps() spi.Caps          { return f.caps }
func (f *fakeTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	f.ran++
	return spi.Result{Summary: "执行了 " + f.name}, nil
}

// newHost 装配一个最小但**真实**的宿主：工具箱 + 计划模块 + 若干假工具。
//
// 走的是与 dshd 同一条装配路径（kernel.Assemble → spi.ToolRunnerKey），
// 所以这里验的是真实执行链，不是手搓的替身。
func newHost(t *testing.T, fake ...spi.Tool) (spi.Runner, *kernel.Kernel) {
	t.Helper()
	resetForTest()
	k := kernel.New("plan-test")
	t.Cleanup(func() {
		k.Root().Dispose()
		resetForTest()
	})
	mods := []kernel.Module{
		tools.Module(tools.Options{SpillDir: t.TempDir()}),
		Module(""), // 空目录 = 内存态，测试不碰真 home
		{
			ID:       "test-fakes",
			Backend:  kernel.BackendStatic,
			Requires: []string{"tools"},
			Register: func(s *kernel.Scope) error {
				reg, err := kernel.Inject(s, spi.ToolsKey)
				if err != nil {
					return err
				}
				for _, tl := range fake {
					if err := reg.Register(tl); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
	if _, err := kernel.Assemble(k.Root(), mods); err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	runner, err := kernel.Inject(k.Root(), spi.ToolRunnerKey)
	if err != nil {
		t.Fatal(err)
	}
	return runner, k
}

// TestWriteToolBlockedInPlanMode：进计划模式后，写类工具被拒，且**根本没执行**。
//
// "返回错误但工具已经跑了"是最坏的形状：模型以为被拦住了，盘上却已经变了。
func TestWriteToolBlockedInPlanMode(t *testing.T) {
	writeFake := &fakeTool{name: "fake_write", caps: spi.Caps{Writes: true}}
	run, _ := newHost(t, writeFake)

	res, err := run.Run(testCtx(), "enter_plan_mode", json.RawMessage(`{"reason":"先看看怎么改"}`))
	if err != nil {
		t.Fatalf("进计划模式不该失败: %v（Hint %q）", err, res.Hint)
	}
	if !Active(testSession) {
		t.Fatal("enter_plan_mode 之后状态必须是 active")
	}

	res, err = run.Run(testCtx(), "fake_write", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("计划模式下写类工具必须被拒")
	}
	if writeFake.ran != 0 {
		t.Fatalf("被拒的工具绝不能真的执行，实际执行了 %d 次", writeFake.ran)
	}
	if !strings.Contains(res.Hint, "计划模式") {
		t.Fatalf("Hint 要提到计划模式，实际 %q", res.Hint)
	}
	if !strings.Contains(res.Hint, "exit_plan_mode") {
		t.Fatalf("Hint 要指路 exit_plan_mode，实际 %q", res.Hint)
	}
}

// TestReadOnlyToolStillRuns：只读工具照常跑。
//
// 计划模式是「不许动」，不是「不许看」——拦掉 read/grep，模型就只能瞎写方案。
func TestReadOnlyToolStillRuns(t *testing.T) {
	readFake := &fakeTool{name: "fake_read", caps: spi.Caps{}}
	run, _ := newHost(t, readFake)

	if _, err := run.Run(testCtx(), "enter_plan_mode", nil); err != nil {
		t.Fatal(err)
	}
	res, err := run.Run(testCtx(), "fake_read", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("只读工具在计划模式下必须放行: %v（Hint %q）", err, res.Hint)
	}
	if readFake.ran != 1 {
		t.Fatalf("只读工具应该真的跑了 1 次，实际 %d", readFake.ran)
	}
}

// TestExecutesAndDestructiveAlsoBlocked：判据只看 Caps，四种能力里三种要拦。
func TestExecutesAndDestructiveAlsoBlocked(t *testing.T) {
	cases := []struct {
		name string
		caps spi.Caps
		want bool // true = 应被拒
	}{
		{"fake_writes", spi.Caps{Writes: true}, true},
		{"fake_executes", spi.Caps{Executes: true}, true},
		{"fake_destructive", spi.Caps{Destructive: true}, true},
		{"fake_network", spi.Caps{Network: true}, false}, // 联网取资料属于"想"
		{"fake_plain", spi.Caps{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeTool{name: c.name, caps: c.caps}
			run, _ := newHost(t, f)
			if _, err := run.Run(testCtx(), "enter_plan_mode", nil); err != nil {
				t.Fatal(err)
			}
			_, err := run.Run(testCtx(), c.name, json.RawMessage(`{}`))
			if blocked := err != nil; blocked != c.want {
				t.Fatalf("%s（caps %+v）被拒 = %v，期望 %v", c.name, c.caps, blocked, c.want)
			}
			if !c.want && f.ran != 1 {
				t.Fatalf("%s 没跑起来", c.name)
			}
		})
	}
}

// TestExitRestoresWriteTools：exit_plan_mode 之后恢复写权限——否则计划模式成了单向门。
func TestExitRestoresWriteTools(t *testing.T) {
	writeFake := &fakeTool{name: "fake_write", caps: spi.Caps{Writes: true}}
	run, _ := newHost(t, writeFake)

	if _, err := run.Run(testCtx(), "enter_plan_mode", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Run(testCtx(), "fake_write", nil); err == nil {
		t.Fatal("先确认被拦住了")
	}
	if _, err := run.Run(testCtx(), "exit_plan_mode",
		json.RawMessage(`{"plan":"# 方案\n1. 改 registry\n2. 补测试"}`)); err != nil {
		t.Fatalf("exit_plan_mode 不该失败: %v", err)
	}
	if Active(testSession) {
		t.Fatal("exit 之后状态必须是 inactive")
	}
	res, err := run.Run(testCtx(), "fake_write", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("退出计划模式后写类工具必须恢复: %v（Hint %q）", err, res.Hint)
	}
	if writeFake.ran != 1 {
		t.Fatalf("应该真的跑了一次，实际 %d", writeFake.ran)
	}
}

// TestExitPlanModeStops：Stop 必须为 true。
//
// 这是整个工具的**目的**：人还没批，模型不能自己编一个"已批准"接着动手。
// Stop 为假，循环就会拿方案当"已完成"继续跑。
func TestExitPlanModeStops(t *testing.T) {
	run, _ := newHost(t)
	if _, err := run.Run(testCtx(), "enter_plan_mode", nil); err != nil {
		t.Fatal(err)
	}
	res, err := run.Run(testCtx(), "exit_plan_mode",
		json.RawMessage(`{"plan":"# 方案\n- 第一步"}`))
	if err != nil {
		t.Fatalf("提交方案不该失败: %v（Hint %q）", err, res.Hint)
	}
	if !res.Stop {
		t.Fatal("exit_plan_mode 的 Stop 必须是 true，否则人还没批模型就继续动手了")
	}
	if res.Summary != "方案已提交，等待批准" {
		t.Fatalf("Summary 要说清在等人批，实际 %q", res.Summary)
	}
	if !strings.Contains(res.Text, "第一步") {
		t.Fatalf("方案正文必须原样交给模型与人，实际 %q", res.Text)
	}
}

// TestEnterSummaryAndHint：进模式这一步要把"下一步做什么"说清楚。
func TestEnterSummaryAndHint(t *testing.T) {
	run, _ := newHost(t)
	res, err := run.Run(testCtx(), "enter_plan_mode", json.RawMessage(`{"reason":"调研"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "已进入计划模式" {
		t.Fatalf("Summary 不对: %q", res.Summary)
	}
	if !strings.Contains(res.Hint, "exit_plan_mode") {
		t.Fatalf("Hint 要指路 exit_plan_mode，实际 %q", res.Hint)
	}
	if got := loadState(testSession).Reason; got != "调研" {
		t.Fatalf("reason 要留住，实际 %q", got)
	}
}

// TestExitWithoutPlanIsError：空方案不许提交——人批的是内容，不是标题。
func TestExitWithoutPlanIsError(t *testing.T) {
	run, _ := newHost(t)
	if _, err := run.Run(testCtx(), "enter_plan_mode", nil); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"plan":"   "}`} {
		res, err := run.Run(testCtx(), "exit_plan_mode", json.RawMessage(raw))
		if err == nil {
			t.Fatalf("%s 应该报错", raw)
		}
		if !strings.Contains(res.Hint, "plan") {
			t.Fatalf("Hint 要说清 plan 怎么给，实际 %q", res.Hint)
		}
	}
	if !Active(testSession) {
		t.Fatal("方案没交成，不能悄悄退出计划模式")
	}
}

// 契约变了，这里说清楚为什么：
//
// 最早 plan 的状态是**进程内单例**，模块撤销即归零（A5「卸载即撤销」）。
// 但 `serve` 每轮起一个新的 `dshd run` 子进程，进程内状态活不过一轮——
// 真机验过：第一轮进的计划模式，第二轮 write 照样执行。
// 所以改成**按会话落盘**，而落盘就意味着"撤销也不该清"：
// 清了等于回到一轮就失效。
//
// 现在的契约是两条：
//  1. 状态按会话隔离：A 会话进计划模式，B 会话不受影响
//  2. 撤销/重启后仍在（这正是它想解决的问题）
func TestPlanStateIsPerSessionNotGlobal(t *testing.T) {
	run, k := newHost(t)
	if _, err := run.Run(testCtx(), "enter_plan_mode", nil); err != nil {
		t.Fatal(err)
	}
	if !Active(testSession) {
		t.Fatal("进入后本会话应当是计划模式")
	}
	if Active("another-session") {
		t.Fatal("另一个会话不该被牵连——那是进程级开关，不是计划模式")
	}
	// 撤销之后本会话仍然是计划模式：这是**有意的**，否则每轮都失效
	k.Root().Dispose()
	if !Active(testSession) {
		t.Fatal("撤销后状态不该清空：清了就等于每轮重新进（serve 每轮是新子进程）")
	}
}

// resetForTest 把状态清干净（内存态 + 临时目录，绝不碰真 home）。
func resetForTest() {
	state.mu.Lock()
	state.dir = ""
	state.mem = map[string]planState{}
	state.mu.Unlock()
}

// testCtx 带上会话 id：闸门按会话判，没会话就等于另一个会话。
func testCtx() context.Context { return spi.WithSession(context.Background(), testSession) }

const testSession = "s1"
