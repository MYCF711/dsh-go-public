// Package goal 提供 `goal` 的三个工具：get_goal / create_goal / update_goal。
//
// 三个工具共用一个文件、一个 Tool 类型：它们操作的是**同一份**按会话划分的状态。
// 拆开只会把存储与编解码抄三遍，并且多出三个互不知道对方改了什么的写入口。
//
// 为什么落盘而不是放内存：目标必须**跨会话存活**——进程重启、会话恢复之后，
// 模型还得能接着干。每个会话一个文件，没有文件就是这个会话没有目标。
package goal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// 三个工具的 kind，同时也是模型看到的工具名。
const (
	kindGet    = "get_goal"
	kindCreate = "create_goal"
	kindUpdate = "update_goal"
)

// 阶段。durable 落在盘上，进程重启后照旧——这正是它存在的意义。
const (
	phaseActive   = "active"
	phaseComplete = "complete"
	phaseBlocked  = "blocked"
	phasePaused   = "paused"
)

// 轮次边界与输入上界。objective 有界是因为它会原样回给模型，
// 无界输入等于让人往上下文里塞任意长度的正文。
const (
	defaultMaxRounds  = 256
	minRounds         = 1
	maxRounds         = 10000
	maxObjectiveRunes = 4000
	maxReasonRunes    = 1000
)

var actions = []string{"edit", "pause", "resume", "complete", "blocked"}

var (
	schemaGet = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

	schemaCreate = json.RawMessage(`{
  "type": "object",
  "properties": {
    "objective": {"type": "string", "description": "这个长程目标要达成什么，一句话说清"},
    "max_goal_rounds": {"type": "integer", "minimum": 1, "maximum": 10000,
      "description": "允许的总轮次上限，可省略，默认 256"}
  },
  "required": ["objective"],
  "additionalProperties": false
}`)

	schemaUpdate = json.RawMessage(`{
  "type": "object",
  "properties": {
    "goal_id": {"type": "string", "description": "从 get_goal 原样抄来的目标 id"},
    "revision": {"type": "integer", "description": "从 get_goal 原样抄来的当前 revision"},
    "action": {"type": "string", "enum": ["edit", "pause", "resume", "complete", "blocked"],
      "description": "edit 改 objective/max_goal_rounds；pause/resume 暂停或恢复；complete 完成；blocked 需给 blocked_reason"},
    "objective": {"type": "string", "description": "action=edit 时传，才改"},
    "max_goal_rounds": {"type": "integer", "minimum": 1, "maximum": 10000,
      "description": "action=edit 时传，才改"},
    "blocked_reason": {"type": "string", "description": "action=blocked 时必填：具体卡在哪"}
  },
  "required": ["revision", "action"],
  "additionalProperties": false
}`)
)

// Goal 是一个会话的目标。字段顺序与落盘形状一致，方便人直接看文件。
type Goal struct {
	ID            string `json:"id"`
	Objective     string `json:"objective"`
	Phase         string `json:"phase"` // active | complete | blocked | paused
	RoundsStarted int    `json:"rounds_started"`
	MaxGoalRounds int    `json:"max_goal_rounds"`
	Revision      int    `json:"revision"`
	BlockedReason string `json:"blocked_reason"`
	CreatedAt     string `json:"created_at"` // RFC3339
	UpdatedAt     string `json:"updated_at"`
}

type createArgs struct {
	Objective     string `json:"objective"`
	MaxGoalRounds *int   `json:"max_goal_rounds"`
}

type updateArgs struct {
	GoalID        string  `json:"goal_id"`
	Revision      int     `json:"revision"`
	Action        string  `json:"action"`
	Objective     *string `json:"objective"`
	MaxGoalRounds *int    `json:"max_goal_rounds"`
	BlockedReason *string `json:"blocked_reason"`
}

// store 是三个工具共享的存储句柄。Mutex 串行化整个读-改-写，
// 因为 revision 是"比较后写"的乐观锁，中间被插一刀就等于没有校验。
type store struct {
	mu  sync.Mutex
	dir string
}

func (s *store) path(session string) string { return filepath.Join(s.dir, session+".json") }

// display 只用于提示：对外一律正斜杠，Windows 的反斜杠不喂给模型。
func (s *store) display() string { return strings.ReplaceAll(s.dir, `\`, "/") }

func (s *store) ready() error {
	if strings.TrimSpace(s.dir) == "" {
		return fmt.Errorf("目标存储没有配置数据目录")
	}
	return nil
}

// load 读当前会话的目标；没有文件就是没有目标，不算错误。
// 调用方必须已持锁。
func (s *store) load(session string) (*Goal, error) {
	buf, err := os.ReadFile(s.path(session))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读目标文件 %s 失败: %w", s.display(), err)
	}
	var g Goal
	if err := json.Unmarshal(buf, &g); err != nil {
		return nil, fmt.Errorf("目标文件 %s 不是合法 JSON: %w", s.display(), err)
	}
	return &g, nil
}

// save 先写 .tmp 再 rename：崩在中间时旧文件仍然完整，不会留下半截 JSON。
// 调用方必须已持锁。
func (s *store) save(session string, g *Goal) error {
	buf, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化目标失败: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("建状态目录 %s 失败: %w", s.display(), err)
	}
	dst := s.path(session)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp) // 留个 .tmp 会让人以为还有一次没跑完的写
		return fmt.Errorf("替换目标文件失败: %w", err)
	}
	return nil
}

// Tool 的三个形态共用一个 struct，靠 kind 分派；这样 Module() 只需注册三次。
type Tool struct {
	st   *store
	kind string
}

func (t *Tool) Name() string { return t.kind }

// Description 只讲"什么时候用"，不讲实现；<=120 字由 desc-len.py 把关。
func (t *Tool) Description() string {
	switch t.kind {
	case kindGet:
		return "查当前会话的目标和 revision；改目标前先调它。"
	case kindCreate:
		return "为需要多轮才能完成的事建一个跨会话存活的目标。"
	default:
		return "改目标：edit/pause/resume/complete/blocked，要带最新 revision。"
	}
}

func (t *Tool) Schema() json.RawMessage {
	switch t.kind {
	case kindGet:
		return schemaGet
	case kindCreate:
		return schemaCreate
	default:
		return schemaUpdate
	}
}

// Caps：get_goal 只读文件系统；create/update 会写盘。
func (t *Tool) Caps() spi.Caps {
	if t.kind == kindGet {
		return spi.Caps{}
	}
	return spi.Caps{Writes: true}
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	switch t.kind {
	case kindGet:
		return t.get(ctx, raw)
	case kindCreate:
		return t.create(ctx, raw)
	case kindUpdate:
		return t.update(ctx, raw)
	}
	// 走到这里说明接线把 kind 写错了，不是模型的锅，所以 Hint 指向人。
	return spi.Result{Hint: "未知的工具分支，这是接线错误：别重复调用，告诉维护者检查 Module() 的 kind"},
		fmt.Errorf("goal: 未知 kind %q", t.kind)
}

// session 取当前会话 id；没有就明说，别让调用方去猜路径。
func session(ctx context.Context) (string, error) {
	id := spi.SessionFrom(ctx)
	if id == "" {
		return "", fmt.Errorf("当前调用没有会话 id")
	}
	// 会话 id 会拼进路径：挡住能跳出目录的字符。
	if id == "." || id == ".." || strings.ContainsAny(id, `/\:*?"<>|`) || strings.ContainsAny(id, "\r\n\t") {
		return "", fmt.Errorf("会话 id %q 含路径分隔符或控制字符", id)
	}
	return id, nil
}

// ---- get_goal ----

func (t *Tool) get(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "目标目录没接线：这是部署配置问题，别重试；告诉维护者检查 Module(dir) 的入参"},
			fmt.Errorf("get_goal: %w", err)
	}
	id, err := session(ctx)
	if err != nil {
		return spi.Result{Hint: "没有会话上下文就读不到目标：换个能带会话的调用方式，别反复试"},
			fmt.Errorf("get_goal: %w", err)
	}

	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	g, err := t.st.load(id)
	if err != nil {
		return spi.Result{Hint: "目标文件读不出来或已损坏：先让人看一眼 " + t.st.display() + " 下的文件，别覆盖它"},
			fmt.Errorf("get_goal: %w", err)
	}
	if g == nil {
		return spi.Result{
			Summary: "当前会话没有目标",
			Data:    map[string]any{"goal": nil},
			Hint:    "要开始一件需要多轮才能完成的事，就 create_goal 建一个；单轮小事不用建",
		}, nil
	}

	hint := ""
	switch g.Phase {
	case phaseBlocked:
		hint = "目标是 blocked：条件解除后用 update_goal action resume 恢复；确实做完了就 action complete，先 get_goal 拿最新 revision"
	case phasePaused:
		hint = "目标是 paused：要接着干就 update_goal action resume（带 goal_id 与 revision）"
	}
	return spi.Result{
		Summary: fmt.Sprintf("目标 %s · %s · revision %d", g.ID, g.Phase, g.Revision),
		Data:    map[string]any{"goal": g},
		Hint:    hint,
	}, nil
}

// ---- create_goal ----

func (t *Tool) create(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a createArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"objective": "..."}，可选 max_goal_rounds（1..10000）`},
			fmt.Errorf("create_goal: 解析参数: %w", err)
	}
	objective := strings.TrimSpace(a.Objective)
	if objective == "" {
		return spi.Result{Hint: `objective 不能为空：一句话写清"要达成什么"，例如 "把 X 仓库的 Y 功能做完并跑通测试"`},
			fmt.Errorf("create_goal: objective 为空")
	}
	if n := len([]rune(objective)); n > maxObjectiveRunes {
		return spi.Result{Hint: fmt.Sprintf("objective 有 %d 字，超过 %d 上界：压缩成一句话再建", n, maxObjectiveRunes)},
			fmt.Errorf("create_goal: objective 过长")
	}
	rounds := defaultMaxRounds
	if a.MaxGoalRounds != nil {
		rounds = *a.MaxGoalRounds
	}
	if rounds < minRounds || rounds > maxRounds {
		return spi.Result{Hint: fmt.Sprintf("max_goal_rounds 要在 %d..%d 之间；不确定就不传，默认 %d", minRounds, maxRounds, defaultMaxRounds)},
			fmt.Errorf("create_goal: max_goal_rounds 越界 %d", rounds)
	}

	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "目标目录没接线：这是部署配置问题，别重试；告诉维护者检查 Module(dir) 的入参"},
			fmt.Errorf("create_goal: %w", err)
	}
	id, err := session(ctx)
	if err != nil {
		return spi.Result{Hint: "没有会话上下文就存不了目标：换个能带会话的调用方式"},
			fmt.Errorf("create_goal: %w", err)
	}

	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	cur, err := t.st.load(id)
	if err != nil {
		return spi.Result{Hint: "目标文件读不出来或已损坏：先让人看一眼 " + t.st.display() + " 下的文件，别覆盖它"},
			fmt.Errorf("create_goal: %w", err)
	}
	if cur != nil {
		return spi.Result{Hint: "先 get_goal 看现状，要改就 update_goal"},
			fmt.Errorf("create_goal: 当前会话已有目标 %s（%s）", cur.ID, cur.Phase)
	}

	now := now()
	g := &Goal{
		ID:            newID(),
		Objective:     objective,
		Phase:         phaseActive,
		RoundsStarted: 0,
		MaxGoalRounds: rounds,
		Revision:      1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := t.st.save(id, g); err != nil {
		return spi.Result{Hint: "目标没落盘：检查状态目录权限与磁盘空间，重试一次；别当作已建成"},
			fmt.Errorf("create_goal: %w", err)
	}
	return spi.Result{
		Summary: fmt.Sprintf("已建目标 %s（revision %d，上限 %d 轮）", g.ID, g.Revision, g.MaxGoalRounds),
		Data:    map[string]any{"goal": g, "id": g.ID, "revision": g.Revision},
		Hint:    "记住这个 id 与 revision：以后每次 update_goal 都要带上，改之前先 get_goal 核对",
	}, nil
}

// ---- update_goal ----

func (t *Tool) update(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a updateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"goal_id": "...", "revision": N, "action": "edit|pause|resume|complete|blocked"}`},
			fmt.Errorf("update_goal: 解析参数: %w", err)
	}
	if !validAction(a.Action) {
		return spi.Result{Hint: "action 只能是 edit / pause / resume / complete / blocked 之一；不确定就先用 get_goal 看目标现在的阶段"},
			fmt.Errorf("update_goal: 非法 action %q", a.Action)
	}
	if a.Revision <= 0 {
		return spi.Result{Hint: "revision 要填 get_goal 返回的那个整数（从 1 起）；先 get_goal 抄一遍"},
			fmt.Errorf("update_goal: revision 非法 %d", a.Revision)
	}

	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "目标目录没接线：这是部署配置问题，别重试；告诉维护者检查 Module(dir) 的入参"},
			fmt.Errorf("update_goal: %w", err)
	}
	id, err := session(ctx)
	if err != nil {
		return spi.Result{Hint: "没有会话上下文就读不到目标：换个能带会话的调用方式"},
			fmt.Errorf("update_goal: %w", err)
	}

	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	g, err := t.st.load(id)
	if err != nil {
		return spi.Result{Hint: "目标文件读不出来或已损坏：先让人看一眼 " + t.st.display() + " 下的文件，别覆盖它"},
			fmt.Errorf("update_goal: %w", err)
	}
	if g == nil {
		return spi.Result{Hint: "当前会话没有目标：先 create_goal 建一个，再改"},
			fmt.Errorf("update_goal: 当前会话没有目标")
	}
	// goal_id 不填就用当前会话的目标：一个会话只有一个目标，
	// 让模型抄一遍 id 只是多一次被拒的机会。填了就必须对上（防串目标）。
	if strings.TrimSpace(a.GoalID) == "" {
		a.GoalID = g.ID
	}
	if g.ID != a.GoalID {
		return spi.Result{Hint: "goal_id 和当前会话的目标对不上（可能抄错了）：先 get_goal 拿准确 id"},
			fmt.Errorf("update_goal: goal_id %q 与当前目标 %q 不符", a.GoalID, g.ID)
	}
	if g.Revision != a.Revision {
		return spi.Result{Hint: "你先 get_goal 拿最新 revision 再改"},
			fmt.Errorf("update_goal: revision 过期（盘上 %d，传来 %d）", g.Revision, a.Revision)
	}

	switch a.Action {
	case "edit":
		changed := false
		if a.Objective != nil {
			obj := strings.TrimSpace(*a.Objective)
			if obj == "" {
				return spi.Result{Hint: "edit 的 objective 不能是空串：要么别传这个字段，要么写一句非空的目标"},
					fmt.Errorf("update_goal: edit 的 objective 为空")
			}
			if n := len([]rune(obj)); n > maxObjectiveRunes {
				return spi.Result{Hint: fmt.Sprintf("objective 有 %d 字，超过 %d 上界：压缩成一句话再改", n, maxObjectiveRunes)},
					fmt.Errorf("update_goal: objective 过长")
			}
			g.Objective = obj
			changed = true
		}
		if a.MaxGoalRounds != nil {
			if *a.MaxGoalRounds < minRounds || *a.MaxGoalRounds > maxRounds {
				return spi.Result{Hint: fmt.Sprintf("max_goal_rounds 要在 %d..%d 之间", minRounds, maxRounds)},
					fmt.Errorf("update_goal: max_goal_rounds 越界 %d", *a.MaxGoalRounds)
			}
			g.MaxGoalRounds = *a.MaxGoalRounds
			changed = true
		}
		if !changed {
			return spi.Result{Hint: "edit 至少要传 objective 或 max_goal_rounds 之一；只改阶段就用 pause/resume/complete/blocked"},
				fmt.Errorf("update_goal: edit 没有任何可改字段")
		}
	case "pause":
		g.Phase = phasePaused
	case "resume":
		g.Phase = phaseActive
	case "complete":
		g.Phase = phaseComplete
	case "blocked":
		reason := ""
		if a.BlockedReason != nil {
			reason = strings.TrimSpace(*a.BlockedReason)
		}
		if reason == "" {
			return spi.Result{Hint: "blocked 必须给 blocked_reason：写清具体卡在哪（缺什么权限、等谁、哪条命令失败），别写“很难/不确定”"},
				fmt.Errorf("update_goal: blocked 缺 blocked_reason")
		}
		if n := len([]rune(reason)); n > maxReasonRunes {
			return spi.Result{Hint: fmt.Sprintf("blocked_reason 有 %d 字，超过 %d 上界：只留那一条具体障碍", n, maxReasonRunes)},
				fmt.Errorf("update_goal: blocked_reason 过长")
		}
		g.Phase = phaseBlocked
		g.BlockedReason = reason
	}

	g.Revision++
	g.UpdatedAt = now()
	if err := t.st.save(id, g); err != nil {
		return spi.Result{Hint: "改动没落盘：检查状态目录权限与磁盘空间，重试一次；别当作已生效"},
			fmt.Errorf("update_goal: %w", err)
	}

	hint := ""
	switch g.Phase {
	case phaseComplete:
		hint = "目标已 complete：别再自动接着干；还有新活就另建目标"
	case phaseBlocked:
		hint = "已标记 blocked：把 blocked_reason 里那条障碍告诉人，等它解除后 resume"
	}
	return spi.Result{
		Summary: fmt.Sprintf("%s %s → %s（revision %d）", a.Action, g.ID, g.Phase, g.Revision),
		Data:    map[string]any{"goal": g, "action": a.Action, "revision": g.Revision},
		Hint:    hint,
	}, nil
}

// ---------------------------------------------------------------- 公共小工具

func validAction(a string) bool {
	for _, v := range actions {
		if v == a {
			return true
		}
	}
	return false
}

// newID 生成 goal-<8 位十六进制>：够短、够唯一，且不泄漏时间。
func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 拿不到随机源时退回时间戳；id 只要在本会话内唯一即可。
		return fmt.Sprintf("goal-%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "goal-" + hex.EncodeToString(b[:])
}

func now() string { return time.Now().Format(time.RFC3339) }

// Module 返回装配用的模块描述。dir 是目标目录（接线时传 <home>/state/goals）。
func Module(dir string) kernel.Module {
	st := &store{dir: dir}
	return kernel.Module{
		ID:       "tool-goal",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			for _, kind := range []string{kindGet, kindCreate, kindUpdate} {
				if err := reg.Register(&Tool{st: st, kind: kind}); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
