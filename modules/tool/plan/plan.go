// Package plan 提供「计划模式」（对齐 DSH 的 planning + plan-mode）。
//
// 它回答一个问题：模型能不能先想清楚、再动手？进入计划模式之后，凡是会改盘、
// 起进程、或可能造成不可恢复后果的工具**全部被拒**；只读工具照常放行。
// 方案出完，模型调 exit_plan_mode 提交，Result.Stop 让这一轮停在方案上，
// 控制权交回人——人没批，模型就不能自己往下动手。
//
// 为什么闸门挂在总线的 tools/pre-execute，而不是让每个工具自己判断：
//   - 工具作者不该知道「计划模式」这件事；
//   - tools/pre-execute 是本仓唯一的执行前拦截点（modules/tools 的 Pipeline
//     是唯一的 Runner，绕过它直接调 Tool.Exec 本身已经违规），挂一处拦全部，
//     以后加新工具零改动；
//   - 这里不许 import modules/tools：模块之间只经 spi 相遇，闸门只依赖
//     spi.Call + spi.ToolRegistry 两个接口。
//
// 状态**按会话落盘**（`<home>/state/plan/<会话 id>.json`）：`serve` 每轮起一个
// 新的 `dshd run` 子进程，进程内状态活不过一轮——真机验过：第一轮进的计划模式，
// 第二轮 write 照样执行。退出即删文件，不留 active=false 的残骸。
package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

// blockedHint 是闸门的正文，同时也是返回给模型的 Hint。
//
// 一句话必须同时回答三件事：现在为什么不行、下一步该做什么、想硬来怎么退出。
// 少了第三句，模型会在被拒之后反复重试同一件事。
const blockedHint = "现在在计划模式：先出方案、调 exit_plan_mode 提交。要直接动手就先退出计划模式"

// errBlocked 是拒绝这一次工具调用的错误。文案即 Hint，见上面。
var errBlocked = errors.New(blockedHint)

// state 是计划模式状态**按会话**存在磁盘上的那份。
//
// 为什么必须落盘：`serve` 每轮起一个新的 `dshd run` 子进程（serve_tools.go:44-60），
// 进程内的状态活不过一轮——第一轮进的计划模式，第二轮就没了，
// 而人就以为"我明明让它先出方案"。这是 2026-11-04 真机验出来的。
//
// 位置：`<home>/state/plan/<会话 id>.json`。人出事时能直接打开看懂并删掉。
var state struct {
	mu sync.Mutex
	// dir 为空表示不落盘（测试用内存态）
	dir string
	// mem 只在 dir 为空时使用
	mem map[string]planState
}

// planState 是一个会话的计划模式状态。
type planState struct {
	Active bool   `json:"active"`
	Reason string `json:"reason,omitempty"`
	At     string `json:"at,omitempty"`
}

func setState(sid string, st planState) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.dir == "" {
		if state.mem == nil {
			state.mem = map[string]planState{}
		}
		state.mem[sid] = st
		return nil
	}
	if err := os.MkdirAll(state.dir, 0o755); err != nil {
		return err
	}
	p := statePath(sid)
	if !st.Active {
		// 退出就是删文件：**不落一个"active=false"的残骸**，
		// 否则将来读到它的人分不清"没进过"和"进过又退了"。
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(raw, '\n'), 0o644)
}

func statePath(sid string) string {
	if sid == "" {
		sid = "_unknown"
	}
	return filepath.Join(state.dir, sid+".json")
}

func loadState(sid string) planState {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.dir == "" {
		return state.mem[sid]
	}
	raw, err := os.ReadFile(statePath(sid))
	if err != nil {
		return planState{}
	}
	var st planState
	if json.Unmarshal(raw, &st) != nil {
		return planState{}
	}
	return st
}

func isActive(sid string) bool { return loadState(sid).Active }

// Active 报告**某个会话**现在是否处于计划模式。
//
// 导出的理由：宿主的审批层（cmd/dshd 的 guardedRunner）在调用流水线**之前**
// 就问人要批准；它先看一眼这里，才能避免"问完再被闸门拒"的废话。
func Active(sid string) bool { return isActive(sid) }

// gate 是计划模式的闸门：返回非 nil 即拒绝这次调用。
//
// 判据只看 Caps，不看工具名——审批与沙箱也是这么判的（R3.6：不按名字猜）。
// 注意不含 Network：联网读资料正是「想」的一部分，拦它会把计划模式变成瞎子。
func gate(reg spi.ToolRegistry, c *spi.Call) error {
	if c == nil || !isActive(c.Session) {
		return nil
	}
	t, ok := reg.Get(c.Name)
	if !ok {
		// 未知工具交给流水线自己报 unknown tool：那句 Hint 比我们准确。
		return nil
	}
	caps := t.Caps()
	if !caps.Writes && !caps.Executes && !caps.Destructive {
		return nil
	}
	return errBlocked
}

// ---- enter_plan_mode ----

var enterSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "reason": {"type": "string", "description": "一句话说明要规划什么（可选）"}
  },
  "additionalProperties": false
}`)

type enterArgs struct {
	Reason string `json:"reason"`
}

type enterTool struct{}

func (t *enterTool) Name() string            { return "enter_plan_mode" }
func (t *enterTool) Caps() spi.Caps          { return spi.Caps{} }
func (t *enterTool) Schema() json.RawMessage { return enterSchema }
func (t *enterTool) Description() string {
	return "进入计划模式：此后只能读和想，不能改盘、不能跑命令。把方案想清楚再用 exit_plan_mode 提交给人批。"
}

func (t *enterTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a enterArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return spi.Result{Hint: `参数不合法：期望 {"reason": "一句话说明要规划什么"}（reason 可以不给）`}, err
		}
	}
	sid := spi.SessionFrom(ctx)
	if err := setState(sid, planState{
		Active: true,
		Reason: strings.TrimSpace(a.Reason),
		At:     time.Now().Format(time.RFC3339),
	}); err != nil {
		return spi.Result{
			Hint: "计划模式状态写不下去（看 <home>/state/plan 是否可写）；别当作已经进了计划模式",
		}, err
	}
	return spi.Result{
		Summary: "已进入计划模式",
		Hint:    "出完方案调 exit_plan_mode 交给人批",
	}, nil
}

// ---- exit_plan_mode ----

var exitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "plan": {"type": "string", "description": "方案正文（markdown）"}
  },
  "required": ["plan"],
  "additionalProperties": false
}`)

type exitArgs struct {
	Plan string `json:"plan"`
}

type exitTool struct{}

func (t *exitTool) Name() string            { return "exit_plan_mode" }
func (t *exitTool) Caps() spi.Caps          { return spi.Caps{} }
func (t *exitTool) Schema() json.RawMessage { return exitSchema }
func (t *exitTool) Description() string {
	return "提交方案并退出计划模式。把 markdown 方案正文放进 plan；提交后本轮结束、控制权交回人。"
}

func (t *exitTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a exitArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"plan": "方案正文（markdown）"}`}, err
	}
	body := strings.TrimSpace(a.Plan)
	if body == "" {
		return spi.Result{Hint: "plan 不能为空：把方案正文（markdown）整段放进去，人要看的是内容"}, fmt.Errorf("缺 plan")
	}
	if err := setState(spi.SessionFrom(ctx), planState{}); err != nil {
		return spi.Result{Hint: "状态清不掉（看 <home>/state/plan 是否可写）；方案已经给出，但闸门可能还拦着"}, err
	}
	// Stop: true 是本工具的意义所在——人还没批，模型不能自己接着动手。
	return spi.Result{
		Summary: "方案已提交，等待批准",
		Text:    body,
		Stop:    true,
	}, nil
}

// Module 返回装配用的模块描述。
func Module(dir string) kernel.Module {
	state.dir = dir
	return kernel.Module{
		ID:       "tool-plan",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			// 两个工具自己都不写盘、不起进程：否则它们在计划模式里也会被自己的闸门拒。
			if err := reg.Register(&enterTool{}); err != nil {
				return err
			}
			if err := reg.Register(&exitTool{}); err != nil {
				return err
			}
			// 闸门挂在唯一的执行前拦截点上；随作用域撤销。
			kernel.OnTyped[*spi.Call](s.Bus(), s, spi.EventToolPreExecute, func(c *spi.Call) error {
				return gate(reg, c)
			})
			// 计划模式按会话落在 <dir>/<会话 id>.json：serve 每轮是新子进程，
			// 内存态活不过一轮（真机验过）。退出即删文件，不留 active=false 的残骸。
			return nil
		},
	}
}
