// Package teamtask 提供团队任务板的四个工具：create / list / get / update。
//
// 为什么四个工具共用一个文件、一个 Tool 类型：它们操作的是**同一份**共享状态——
// 一个代理 claim 的活，另一个代理必须看得见。拆成四个模块只会让"同一块板子"变成四块，
// 拆成四个类型则要把 board 与 JSON 编解码抄四遍。所以内部按 kind 分派。
package teamtask

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

const fileName = "team-tasks.json"

const (
	statusPending    = "pending"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
)

// 四个工具的 kind，同时也是模型看到的工具名。
const (
	kindCreate = "team_task_create"
	kindList   = "team_task_list"
	kindGet    = "team_task_get"
	kindUpdate = "team_task_update"
)

// 输出有界：板子再大也只回这么多条，列表里的描述只回前 listDescRunes 字。
const (
	maxListItems    = 200
	maxSubjectRunes = 200
	maxDescRunes    = 4000
	listDescRunes   = 240
)

var (
	statuses = []string{statusPending, statusInProgress, statusCompleted}
	actions  = []string{"claim", "release", "complete", "reopen", "edit", "set_dependencies", "delete"}
)

// Task 是任务板上的一条任务。字段顺序与落盘形状一致，方便人直接看文件。
type Task struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	Status      string   `json:"status"` // pending | in_progress | completed
	Owner       string   `json:"owner"`
	BlockedBy   []string `json:"blocked_by"`
	CreatedAt   string   `json:"created_at"` // RFC3339
	UpdatedAt   string   `json:"updated_at"`
}

type taskFile struct {
	Tasks []Task `json:"tasks"`
}

// board 是四个工具共享的存储句柄。
type board struct {
	mu  sync.Mutex
	dir string
}

func (b *board) path() string { return filepath.Join(b.dir, fileName) }

// display 只用于提示：对外一律正斜杠，Windows 的反斜杠不喂给模型。
func (b *board) display() string { return strings.ReplaceAll(b.path(), `\`, "/") }

func (b *board) ready() error {
	if strings.TrimSpace(b.dir) == "" {
		return fmt.Errorf("团队任务板没有配置数据目录")
	}
	return nil
}

// load 读出全部任务；文件不存在或为空都当作空板子（第一天上工不该报错）。
// 调用方必须已持锁。
func (b *board) load() ([]Task, error) {
	buf, err := os.ReadFile(b.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读任务板 %s 失败: %w", b.display(), err)
	}
	if len(strings.TrimSpace(string(buf))) == 0 {
		return nil, nil
	}
	var f taskFile
	if err := json.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("任务板 %s 不是合法 JSON: %w", b.display(), err)
	}
	return f.Tasks, nil
}

// save 先写 .tmp 再 rename：崩在中间时旧文件仍然完整，不会留下半截 JSON。
// 调用方必须已持锁。
// archive 把被删掉的条目追加到 <dir>/team-tasks.deleted.jsonl。
//
// 为什么不直接删：任务板是**共享**的，删完就没有任何东西能回答
// "这条是谁、什么时候、以什么理由删的"。一行追加的成本换一次可追溯。
func (b *board) archive(t Task) error {
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(b.dir, "team-tasks.deleted.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(map[string]any{"deleted_at": now(), "task": t})
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func (b *board) save(tasks []Task) error {
	if tasks == nil {
		tasks = []Task{}
	}
	buf, err := json.MarshalIndent(taskFile{Tasks: tasks}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化任务板失败: %w", err)
	}
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return fmt.Errorf("建状态目录 %s 失败: %w", b.display(), err)
	}
	tmp := b.path() + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("写临时文件 %s.tmp 失败: %w", b.display(), err)
	}
	if err := os.Rename(tmp, b.path()); err != nil {
		os.Remove(tmp) // 留个 .tmp 会让人以为还有一次没跑完的写
		return fmt.Errorf("替换任务板 %s 失败: %w", b.display(), err)
	}
	return nil
}

// ---------------------------------------------------------------- 公共小工具

func validStatus(s string) bool {
	for _, v := range statuses {
		if s == v {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// cleanIDs 去掉空白与重复，保住顺序——blocked_by 的顺序对读的人有意义。
func cleanIDs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func indexOf(tasks []Task, id string) int {
	for i := range tasks {
		if tasks[i].ID == id {
			return i
		}
	}
	return -1
}

func idSeq(id string) int {
	parts := strings.Split(id, "-")
	if len(parts) < 2 {
		return 0
	}
	n := 0
	for _, ch := range parts[1] {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// nextID 生成 task-0001-3f9a 这种形状：序号取板上最大值 +1，后缀是随机 4 位十六进制。
// 不用 UUID 库：这个 id 是给人念、给人贴进对话里的，短才有用。
func nextID(tasks []Task) (string, error) {
	max := 0
	for _, t := range tasks {
		if n := idSeq(t.ID); n > max {
			max = n
		}
	}
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成任务 id 失败: %w", err)
	}
	return fmt.Sprintf("task-%04d-%s", max+1, hex.EncodeToString(buf[:])), nil
}

// recentIDs 给"找不到 id"的提示用：列出最近几条，让模型少猜一轮。
func recentIDs(tasks []Task, n int) []string {
	if len(tasks) == 0 {
		return nil
	}
	start := len(tasks) - n
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, len(tasks)-start)
	for _, t := range tasks[start:] {
		out = append(out, t.ID)
	}
	return out
}

func notFoundHint(tasks []Task) string {
	ids := recentIDs(tasks, 3)
	if len(ids) == 0 {
		return "板子上还没有任务；先用 team_task_create 建一条，或 team_task_list 确认板子是空的"
	}
	return "没有这个 id；板子上最近的 id 是 " + strings.Join(ids, "、") + "，再不对就用 team_task_list 看全量"
}

// missingDeps 报出 blocked_by 里板上不存在的 id；只是提示，不拦——依赖可以先写后建。
func missingDeps(tasks []Task, ids []string) []string {
	var out []string
	for _, id := range ids {
		if indexOf(tasks, id) < 0 {
			out = append(out, id)
		}
	}
	return out
}

func unfinishedDeps(tasks []Task, ids []string) []string {
	var out []string
	for _, id := range ids {
		j := indexOf(tasks, id)
		if j < 0 || tasks[j].Status != statusCompleted {
			out = append(out, id)
		}
	}
	return out
}

func withDepNote(hint string, missing []string) string {
	if len(missing) == 0 {
		return hint
	}
	note := "blocked_by 里的 " + strings.Join(missing, "、") + " 不在板子上（可能是笔误，也可能还没建）"
	if hint == "" {
		return note
	}
	return hint + "；" + note
}

func now() string { return time.Now().Format(time.RFC3339) }

// ---------------------------------------------------------------- Tool

// Tool 是任务板四个工具的共同外壳：kind 决定名字、参数与行为，
// board 决定它们操作哪块板子——四者必须看到同一份状态，所以共享一个句柄。
type Tool struct {
	b    *board
	kind string
}

func (t *Tool) Name() string { return t.kind }

// Caps 必填且如实：只有 create / update 会写盘。
func (t *Tool) Caps() spi.Caps {
	switch t.kind {
	case kindCreate, kindUpdate:
		return spi.Caps{Writes: true}
	}
	return spi.Caps{}
}

// Description 是"什么时候用它"。四条写在同一处，因为它们是同一块板子的四个动作，
// 分开写最容易漂移成互相矛盾的措辞。
func (t *Tool) Description() string {
	switch t.kind {
	case kindCreate:
		return "建一条团队任务并返回 id；分工前先建任务。"
	case kindList:
		return "列出任务，可按 status 过滤；开工前先看板子。"
	case kindGet:
		return "按 id 取一条任务：内容、认领人、未完成依赖。"
	case kindUpdate:
		return "改任务：claim 认领、complete 完成、delete 删除等七个动作。"
	}
	return ""
}

func (t *Tool) Schema() json.RawMessage {
	switch t.kind {
	case kindCreate:
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "subject": {"type": "string", "description": "一句话目标，祈使句"},
    "description": {"type": "string", "description": "细节与验收标准"},
    "blocked_by": {"type": "array", "items": {"type": "string"},
                   "description": "依赖的任务 id；它们没完成前不该开工"}
  },
  "required": ["subject", "description"],
  "additionalProperties": false
}`)
	case kindList:
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["pending", "in_progress", "completed"],
               "description": "只看这个状态；省略表示全部"}
  },
  "additionalProperties": false
}`)
	case kindGet:
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "task_id": {"type": "string", "description": "任务 id，形如 task-0001-3f9a"}
  },
  "required": ["task_id"],
  "additionalProperties": false
}`)
	case kindUpdate:
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "task_id": {"type": "string", "description": "任务 id，形如 task-0001-3f9a"},
    "expected_revision": {"type": "integer",
                          "description": "可选：DSH 契约里的版本号参数；本实现暂不校验（改前先 team_task_get 看最新状态）"},
    "action": {"type": "string",
               "enum": ["claim", "release", "complete", "reopen", "edit", "set_dependencies", "delete"],
               "description": "claim=认领并置 in_progress；release=退回 pending 并清 owner；complete=完成；reopen=重开；edit=改 subject/description；set_dependencies=整份替换 blocked_by；delete=删除"},
    "owner": {"type": "string", "description": "claim 必填：你的名字或角色"},
    "force": {"type": "boolean", "description": "action=delete 专用：强行删掉别人认领的任务（会进归档）"},
    "subject": {"type": "string", "description": "action=edit 时传，才改"},
    "description": {"type": "string", "description": "action=edit 时传，才改（传空串表示清空）"},
    "blocked_by": {"type": "array", "items": {"type": "string"},
                   "description": "action=set_dependencies 时必填：整份替换（空数组=清空依赖）"}
  },
  "required": ["task_id", "action"],
  "additionalProperties": false
}`)
	}
	return json.RawMessage(`{"type":"object"}`)
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	switch t.kind {
	case kindCreate:
		return t.execCreate(ctx, raw)
	case kindList:
		return t.execList(ctx, raw)
	case kindGet:
		return t.execGet(ctx, raw)
	case kindUpdate:
		return t.execUpdate(ctx, raw)
	}
	return spi.Result{Hint: "未知工具 kind：这是装配错误，报告监工"}, fmt.Errorf("未知 kind %q", t.kind)
}

// ---------------------------------------------------------------- create

type createArgs struct {
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	BlockedBy   []string `json:"blocked_by"`
}

func (t *Tool) execCreate(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a createArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"subject": "改登录页文案", "description": "……", "blocked_by": ["task-0001-3f9a"]}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	if err := t.b.ready(); err != nil {
		return spi.Result{Hint: "任务板没有数据目录：这是接线问题，报告监工（Module(dir) 要传 <home>/state），别自己换地方存"},
			err
	}
	subject := strings.TrimSpace(a.Subject)
	if subject == "" {
		return spi.Result{Hint: `subject 不能为空：用一句祈使句写清目标，例如 {"subject": "把登录页错误提示改成中文"}`},
			fmt.Errorf("subject 为空")
	}
	if len([]rune(subject)) > maxSubjectRunes {
		return spi.Result{Hint: fmt.Sprintf("subject 超过 %d 字：只写一句目标，细节挪到 description", maxSubjectRunes)},
			fmt.Errorf("subject 过长")
	}
	desc := strings.TrimSpace(a.Description)
	if len([]rune(desc)) > maxDescRunes {
		return spi.Result{Hint: fmt.Sprintf("description 超过 %d 字：把这块活拆成几条任务，每条一个可验收的产出", maxDescRunes)},
			fmt.Errorf("description 过长")
	}

	t.b.mu.Lock()
	defer t.b.mu.Unlock()

	tasks, err := t.b.load()
	if err != nil {
		return spi.Result{Hint: "任务板读不出来：先别写，检查 " + t.b.display() + " 是否损坏（损坏时保留原文件，别覆盖）"}, err
	}
	id, err := nextID(tasks)
	if err != nil {
		return spi.Result{Hint: "生成 id 失败（随机源不可用）：重试一次；连续失败就报告监工，别自己造 id"}, err
	}
	ts := now()
	task := Task{
		ID:          id,
		Subject:     subject,
		Description: desc,
		Status:      statusPending,
		Owner:       "",
		BlockedBy:   cleanIDs(a.BlockedBy),
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}
	missing := missingDeps(tasks, task.BlockedBy)
	tasks = append(tasks, task)
	if err := t.b.save(tasks); err != nil {
		return spi.Result{Hint: "任务没落盘：检查状态目录权限与磁盘空间，重试一次；别以为建好了就往下走"}, err
	}

	hint := ""
	if len(task.BlockedBy) > 0 {
		hint = "这条任务被 " + strings.Join(task.BlockedBy, "、") + " 阻塞：先完成它们，清掉依赖后 set_dependencies 置空"
	}
	hint = withDepNote(hint, missing)
	return spi.Result{
		Summary: "已建任务 " + id + "（" + task.Status + "）",
		Data:    map[string]any{"id": id, "status": task.Status, "task": task, "total": len(tasks)},
		Hint:    hint,
	}, nil
}

// ---------------------------------------------------------------- list

type listArgs struct {
	Status string `json:"status"`
}

func (t *Tool) execList(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a listArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"status": "pending"} 或空对象 {}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	if err := t.b.ready(); err != nil {
		return spi.Result{Hint: "任务板没有数据目录：这是接线问题，报告监工（Module(dir) 要传 <home>/state）"}, err
	}
	status := strings.TrimSpace(a.Status)
	if status != "" && !validStatus(status) {
		return spi.Result{Hint: `status 只能是 pending / in_progress / completed；全部都要就别传这个字段`},
			fmt.Errorf("status %q 不合法", status)
	}

	t.b.mu.Lock()
	defer t.b.mu.Unlock()

	tasks, err := t.b.load()
	if err != nil {
		return spi.Result{Hint: "任务板读不出来：检查 " + t.b.display() + " 的 JSON 是否完好；确认不了就报告监工，别删文件"}, err
	}

	// 列表里压缩描述：200 条 × 4000 字会直接撑爆上下文，要看细节用 team_task_get。
	picked := make([]Task, 0, len(tasks))
	clipped := false
	for _, task := range tasks {
		if status != "" && task.Status != status {
			continue
		}
		if len([]rune(task.Description)) > listDescRunes {
			task.Description = clip(task.Description, listDescRunes)
			clipped = true
		}
		picked = append(picked, task)
	}

	total := len(picked)
	truncated := false
	if total > maxListItems {
		picked = picked[:maxListItems]
		truncated = true
	}

	data := map[string]any{
		"tasks":     picked,
		"count":     len(picked),
		"total":     total,
		"truncated": truncated,
	}
	if status != "" {
		data["status"] = status
	}

	if total == 0 {
		note := "还没有任务——任务板是空的"
		hint := "用 team_task_create 建第一条"
		if status != "" {
			note = "还没有 " + status + " 状态的任务"
			hint = "去掉 status 看全部，或用 team_task_create 建一条"
		}
		data["note"] = note
		return spi.Result{Summary: note, Data: data, Hint: hint}, nil
	}

	hint := ""
	if truncated {
		hint = fmt.Sprintf("只回了前 %d 条（共 %d 条）：用 status 过滤，或 team_task_get 按 id 取具体任务", maxListItems, total)
		data["note"] = hint
	} else if clipped {
		hint = "列表里的 description 已截短，完整内容用 team_task_get 取"
	}
	return spi.Result{
		Summary: fmt.Sprintf("任务板 %d 条", total),
		Data:    data,
		Hint:    hint,
	}, nil
}

// ---------------------------------------------------------------- get

type getArgs struct {
	// 参数名跟 DSH 对齐：叫 task_id 而不是 id（TestToolParityWithDSH 会查）
	ID string `json:"task_id"`
}

func (t *Tool) execGet(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a getArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"task_id": "task-0001-3f9a"}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	if err := t.b.ready(); err != nil {
		return spi.Result{Hint: "任务板没有数据目录：这是接线问题，报告监工（Module(dir) 要传 <home>/state）"}, err
	}
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return spi.Result{Hint: "id 不能为空：先用 team_task_list 拿到 id，再按 id 取详情"},
			fmt.Errorf("id 为空")
	}

	t.b.mu.Lock()
	defer t.b.mu.Unlock()

	tasks, err := t.b.load()
	if err != nil {
		return spi.Result{Hint: "任务板读不出来：检查 " + t.b.display() + " 的 JSON 是否完好；确认不了就报告监工，别删文件"}, err
	}
	i := indexOf(tasks, id)
	if i < 0 {
		return spi.Result{Hint: notFoundHint(tasks)}, fmt.Errorf("找不到任务 %q", id)
	}
	task := tasks[i]

	// 依赖里还有哪几条没完成——这才是开工前真正要判断的东西。
	unfinished := unfinishedDeps(tasks, task.BlockedBy)
	data := map[string]any{"task": task, "unfinished_blockers": unfinished}
	hint := ""
	if len(unfinished) > 0 {
		hint = "还有 " + strings.Join(unfinished, "、") + " 没完成：先做它们，别抢这条"
	} else if task.Status == statusPending && len(task.BlockedBy) > 0 {
		hint = "依赖都完成了，可以 claim 开工"
	}
	return spi.Result{
		Summary: task.ID + " [" + task.Status + "] " + clip(task.Subject, 60),
		Data:    data,
		Hint:    hint,
	}, nil
}

// ---------------------------------------------------------------- update

// ownerOf 是"这次调用代表谁"。认领时默认用它署名（见 claim 分支），
// 删除时也用它判断"这条是不是我认领的"。
func ownerOf(ctx context.Context) string {
	q := spi.SessionFrom(ctx)
	return strings.TrimSpace(q)
}

type updateArgs struct {
	ID string `json:"task_id"`
	// ExpectedRevision 留着占位：DSH 的契约里有这个参数，我们暂时不做 CAS
	ExpectedRevision *int `json:"expected_revision"`

	Action string `json:"action"`
	Owner  string `json:"owner"`
	// Force 只对 delete 生效：强行删掉别人认领的任务（会进归档）。
	Force       bool      `json:"force"`
	Subject     *string   `json:"subject"`
	Description *string   `json:"description"`
	BlockedBy   *[]string `json:"blocked_by"`
}

func (t *Tool) execUpdate(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a updateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"task_id": "task-0001-3f9a", "action": "claim", "owner": "alice"}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	if err := t.b.ready(); err != nil {
		return spi.Result{Hint: "任务板没有数据目录：这是接线问题，报告监工（Module(dir) 要传 <home>/state）"}, err
	}
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return spi.Result{Hint: "id 不能为空：先用 team_task_list 拿到 id，再带 id 调本工具"},
			fmt.Errorf("id 为空")
	}
	action := strings.TrimSpace(a.Action)
	known := false
	for _, v := range actions {
		if action == v {
			known = true
			break
		}
	}
	if !known {
		return spi.Result{Hint: "未知 action：可选 " + strings.Join(actions, "|") + "；改标题用 edit，接手活用 claim（带 owner）"},
			fmt.Errorf("未知 action %q", action)
	}

	owner := strings.TrimSpace(a.Owner)
	if action == "claim" && owner == "" {
		// 没给 owner 就认领成"当前会话"：身份本来就在 ctx 里，问一遍纯属多余。
		// 实测代价：只靠参数说明提示"claim 必填 owner"时，压缩工具表后模型会漏填，
		// 白白多跑两轮。让工具有个合理默认比把要求写进说明更可靠。
		owner = spi.SessionFrom(ctx)
	}
	if action == "claim" && owner == "" {
		return spi.Result{Hint: `claim 认领时需要知道是谁：参数里带 owner，或在会话里调用（用会话 id 署名）`},
			fmt.Errorf("claim 缺少 owner")
	}
	if action == "set_dependencies" && a.BlockedBy == nil {
		return spi.Result{Hint: `set_dependencies 必须带 blocked_by：整份替换，清空依赖就传 []`},
			fmt.Errorf("set_dependencies 缺少 blocked_by")
	}
	if action == "edit" && a.Subject == nil && a.Description == nil {
		return spi.Result{Hint: `edit 至少要带 subject 或 description 之一`},
			fmt.Errorf("edit 没有可改的字段")
	}
	if action == "edit" && a.Subject != nil && strings.TrimSpace(*a.Subject) == "" {
		return spi.Result{Hint: "subject 不能改成空：不想要这条就 delete；只改措辞就写清新的目标"},
			fmt.Errorf("subject 为空")
	}
	if action == "edit" && a.Subject != nil && len([]rune(strings.TrimSpace(*a.Subject))) > maxSubjectRunes {
		return spi.Result{Hint: fmt.Sprintf("subject 超过 %d 字：只写一句目标，细节放 description", maxSubjectRunes)},
			fmt.Errorf("subject 过长")
	}
	if action == "edit" && a.Description != nil && len([]rune(*a.Description)) > maxDescRunes {
		return spi.Result{Hint: fmt.Sprintf("description 超过 %d 字：拆成多条任务，或把长材料放到文件里引用", maxDescRunes)},
			fmt.Errorf("description 过长")
	}

	t.b.mu.Lock()
	defer t.b.mu.Unlock()

	tasks, err := t.b.load()
	if err != nil {
		return spi.Result{Hint: "任务板读不出来：先别写，检查 " + t.b.display() + " 是否损坏（损坏时保留原文件，别覆盖）"}, err
	}
	i := indexOf(tasks, id)
	if i < 0 {
		return spi.Result{Hint: notFoundHint(tasks)}, fmt.Errorf("找不到任务 %q", id)
	}
	task := tasks[i]

	hint := ""
	switch action {
	case "claim":
		if task.Status == statusInProgress && task.Owner != "" && task.Owner != owner {
			hint = "这条已经被 " + task.Owner + " 认领了；要接手先和对方确认，或 release 之后再 claim"
		}
		task.Status = statusInProgress
		task.Owner = owner
	case "release":
		task.Status = statusPending
		task.Owner = ""
		hint = "已退回 pending：别人现在可以 claim"
	case "complete":
		task.Status = statusCompleted
		if task.Owner == "" {
			task.Owner = owner // 没认领过就直接完成，至少记下是谁做的
		}
		hint = "完成后看一眼有没有别的任务 blocked_by 这条，它们现在能开工了"
	case "reopen":
		task.Status = statusPending
		task.Owner = ""
		hint = "已重开：在 description 里补一句为什么要重开，别人接手才不会重犯"
	case "edit":
		if a.Subject != nil {
			task.Subject = strings.TrimSpace(*a.Subject)
		}
		if a.Description != nil {
			task.Description = strings.TrimSpace(*a.Description)
		}
	case "set_dependencies":
		deps := cleanIDs(*a.BlockedBy)
		for _, dep := range deps {
			if dep == id {
				return spi.Result{Hint: "任务不能依赖自己：blocked_by 里去掉 " + id + " 再调一次"},
					fmt.Errorf("blocked_by 含自身")
			}
		}
		task.BlockedBy = deps
		hint = withDepNote("", missingDeps(tasks, deps))
	case "delete":
		// 别人认领的任务不能随手删：那是在删**别人正在做的活**，
		// 而且删完没有任何痕迹。要删得显式 force，并且照样进归档。
		if owner := strings.TrimSpace(task.Owner); owner != "" && owner != ownerOf(ctx) && !a.Force {
			return spi.Result{Hint: fmt.Sprintf(
				"%s 正被 %s 认领，不能直接删。要么让认领者先 release，要么带 force=true 强行删（会进归档）",
				task.ID, owner)}, fmt.Errorf("team_task_update: %s 已被认领，需要 force", task.ID)
		}
		// 归档再删：任务板上删掉就没有历史了，事后想追"谁把这条删了"无从查起。
		if err := t.b.archive(task); err != nil {
			return spi.Result{Hint: "归档没写成：删除已中止，任务还在板上。检查状态目录权限后重试"}, err
		}
		tasks = append(tasks[:i], tasks[i+1:]...)
		if err := t.b.save(tasks); err != nil {
			return spi.Result{Hint: "删除没落盘：检查状态目录权限与磁盘空间，重试一次"}, err
		}
		return spi.Result{
			Summary: "已删除 " + task.ID + "（" + clip(task.Subject, 40) + "）",
			Data:    map[string]any{"action": "delete", "deleted_id": task.ID, "total": len(tasks)},
			Hint:    "删除不可恢复：如果只是想暂缓，下次用 release 或 reopen",
		}, nil
	}

	task.UpdatedAt = now()
	tasks[i] = task
	if err := t.b.save(tasks); err != nil {
		return spi.Result{Hint: "改动没落盘：检查状态目录权限与磁盘空间，重试一次；别当作已生效"}, err
	}

	// claim 时点名还欠着的依赖：认领成功不等于可以开工，语气要让人停一下。
	if action == "claim" {
		if deps := unfinishedDeps(tasks, task.BlockedBy); len(deps) > 0 {
			note := "依赖 " + strings.Join(deps, "、") + " 还没完成：确认过再动手，否则可能白做"
			if hint == "" {
				hint = note
			} else {
				hint += "；" + note
			}
		}
	}

	return spi.Result{
		Summary: fmt.Sprintf("%s %s → %s", action, task.ID, task.Status),
		Data:    map[string]any{"action": action, "task": task, "total": len(tasks)},
		Hint:    hint,
	}, nil
}

// Module 返回装配用的模块描述。dir 是状态目录（接线时传 <home>/state）。
func Module(dir string) kernel.Module {
	b := &board{dir: dir}
	return kernel.Module{
		ID:       "tool-teamtask",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			for _, kind := range []string{kindCreate, kindList, kindGet, kindUpdate} {
				if err := reg.Register(&Tool{b: b, kind: kind}); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
