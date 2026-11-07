// tool_merged.go 是"只改模型看到什么"的两个投影层。
//
//  1. merged：把同族工具合成一个带 action 参数的工具（见下方 mergedTable）。
//  2. tighten：compact 表在去掉参数说明之后，再去掉 schema 里可省的骨架
//     （见文件末尾 tightenTable）。两者都只改序列化，modules/ 里的 Exec、
//     Caps、审批链路一个字节都不动。无参对象的 type/properties 不是可省骨架：
//     上游 schema 转换器会拒绝只剩 additionalProperties 的形态。
//
// 为什么合并要单独一档、收紧却并进 compact：合并改的是**模型看到的接口**
// （工具名与 action），属于 R4 契约变更，必须显式打开；收紧只改 JSON 的写法，
// 解析出来的语义逐条相等（TestCompactTableKeepsContract 钉住），
// 所以它就是 compact 该有的样子，不需要再多一个开关让人选错。
//
// 发给模型的工具表就是一列 JSON，而执行侧的所有中间件
// （审批、沙箱、截断、v4 记账）都挂在 spi.Runner 上，并且按**原工具名**去查
// 能力声明（Caps）。所以投影只改"模型看到什么"：
//
//	表：17 个同族工具 → 4 个工具，每个多一个 action 枚举
//	调用：mergedRunner 把 action 还原成原工具名 + 原参数，再交给原来的 Runner
//
// modules/ 里的 Exec 一个字节都不动；审批仍按原工具名的 Caps 判定，
// 因为还原发生在守卫**之前**（mergedRunner 在外、guardedRunner 在内）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	// 别名：cmd/dshd 的测试包里有个同名类型 schema（parity_test.go 的夹具）。
	schemarule "dshgo/internal/schema"
	"dshgo/internal/spi"
)

// 工具表的四种形态。full / compact 是原有语义，merged 是合并档，
// lazy 是按需装载档（见 tool_lazy.go）——后两档都必须显式打开。
const (
	toolsModeFull    = "full"
	toolsModeCompact = "compact"
	toolsModeMerged  = "merged"
	toolsModeLazy    = "lazy"
)

// mergedAction 是一个 action 取值与它对应的原工具。
type mergedAction struct {
	// Action 是外层 action 的取值，恒等于原工具名的后缀。
	Action string
	// Tool 是原工具名（执行侧看到的还是它）。
	Tool string
	// Rename 是"模型看到的参数名 → 原工具的参数名"。
	//
	// 只为一处存在：team_task_update 自己就有一个 action（claim/complete/…），
	// 和外层的 action 撞名。改叫 update_action 之后，路由时再改回 action。
	// 宁可多一个改名表，也不要把两个 action 混成一个枚举——
	// 那样模型永远分不清 action=claim 是"合并工具的 action"还是"任务的子动作"。
	Rename map[string]string
	// Aliases 是"同一个参数的另一种叫法"：只给别名不算缺参数。
	//
	// 只为一处存在：hindsight_read_knowledge_page 的 schema 把 page_id 标成必填，
	// 但它自己的说明写着"id 是另一种叫法"，只给 id 也照常工作。
	// 合并层不能因为原 schema 写得死板就拒绝一个原本能跑通的调用。
	Aliases map[string][]string
	// Required 是这个 action 自己的必填参数（**模型看到的**名字）。
	//
	// 为什么必须单独存一份：合并后的 schema 只能有一个 required，而它只能是
	// 各成员的**交集**（否则所有 action 都被逼着填别人的参数）。于是
	// "create 必填 description" 这条约束在表里就消失了——实测模型真的会漏，
	// 而 team_task_create 不校验、直接建成了一条没描述的任务。
	// 所以约束改由路由执行：缺参数就在这一步报错，让模型补，而不是静默收下。
	Required []string
}

// mergedFamily 是一个被合并的族。
type mergedFamily struct {
	// Name 是合并后模型看到的工具名（族名本身，见 mergedFamilies 的注释）。
	Name string
	// Summary 是一句话说明这族干什么。
	Summary string
	// Actions 是 action → 原工具，顺序即 schema 里 enum 的顺序（按常用度排）。
	Actions []mergedAction
	// Note 追加在描述末尾，写清"哪些参数属于哪个 action"里最容易错的那条。
	Note string
}

// mergedFamilies 是本次合并的全部范围——只这四族，别的工具一个都不动。
//
// 名字取"族名本身"（watch / hindsight / schedule / team_task）而不是最常用的
// 那个成员：成员名（watch_start…）都带前缀，取任何一个都会让模型误以为
// 这个工具的专门用途是那一个动作；族名是唯一的公共前缀，和 action 拼起来
// 正好还原成原名，模型在旧记忆里也认得住。
func mergedFamilies() []mergedFamily {
	return []mergedFamily{
		{
			Name:    "watch",
			Summary: "盯 GitHub 仓库：登记/查看/停止、推进已处理水位",
			Actions: []mergedAction{
				{Action: "start", Tool: "watch_start"},
				{Action: "status", Tool: "watch_status"},
				{Action: "stop", Tool: "watch_stop"},
				{Action: "ack", Tool: "watch_ack"},
			},
			// watch_doctor **没有**并进来：它的 schema 是 watch_ack 的复制
			// （required=["numbers"]，连属性都一模一样），而体检根本不看编号。
			// 合进来只有两条路：要么信这份 schema（模型得为体检编几个编号），
			// 要么不信（那就等于合并层教模型无视 schema）。两条都不干——
			// 少合一个成员，把它原样留给模型。
			Note: "start/stop 必填 repo；ack 必填 numbers；status 不用参数。体检用单独的 watch_doctor。",
		},
		{
			Name:    "hindsight",
			Summary: "本机记忆（知识页/文档）：搜、列、读、存、记倡议",
			Actions: []mergedAction{
				{Action: "search_knowledge_pages", Tool: "hindsight_search_knowledge_pages"},
				{Action: "list_knowledge_pages", Tool: "hindsight_list_knowledge_pages"},
				{Action: "read_knowledge_page", Tool: "hindsight_read_knowledge_page",
					Aliases: map[string][]string{"page_id": {"id"}}},
				{Action: "ingest_document", Tool: "hindsight_ingest_document"},
				{Action: "capture_initiative", Tool: "hindsight_capture_initiative"},
			},
			Note: "search 必填 query（可给 limit）；read 必填 page_id（也可写 id）；ingest 必填 title/content；" +
				"capture_initiative 必填 title/summary；list_knowledge_pages 不用参数。",
		},
		{
			Name:    "schedule",
			Summary: "定时任务：建、列、改、删（every/daily/weekly）",
			Actions: []mergedAction{
				{Action: "create", Tool: "schedule_create"},
				{Action: "list", Tool: "schedule_list"},
				{Action: "update", Tool: "schedule_update"},
				{Action: "delete", Tool: "schedule_delete"},
			},
			Note: "create 必填 title/prompt/kind；update/delete 必给 id。",
		},
		{
			Name:    "team_task",
			Summary: "团队任务看板：建、取、列、改",
			Actions: []mergedAction{
				{Action: "create", Tool: "team_task_create"},
				{Action: "list", Tool: "team_task_list"},
				{Action: "get", Tool: "team_task_get"},
				{Action: "update", Tool: "team_task_update", Rename: map[string]string{"update_action": "action"}},
			},
			Note: "create 必填 subject/description；update 时再给 " +
				"update_action=claim/release/complete/reopen/edit/set_dependencies/delete。",
		},
	}
}

// resolveToolsMode 把两个 flag（新名 --tools-mode、旧名 --tool-schema）折成一个值。
//
// 旧名保留：scripts/tool-table-ab.py、历史文档、以及人的肌肉记忆都还用它。
// 新名优先——显式给了 --tools-mode 就以它为准。
func resolveToolsMode(mode, legacy string) (string, error) {
	m := strings.TrimSpace(mode)
	if m == "" {
		m = strings.TrimSpace(legacy)
	}
	if m == "" {
		m = defaultToolSchema
	}
	switch m {
	case toolsModeFull, toolsModeCompact, toolsModeMerged, toolsModeLazy:
		return m, nil
	}
	return "", fmt.Errorf("--tools-mode 只能是 %s|%s|%s|%s，收到 %q",
		toolsModeCompact, toolsModeFull, toolsModeMerged, toolsModeLazy, m)
}

// mergedTool 是投影出来的工具：名字/描述/schema 是新的，执行回落到原工具。
type mergedTool struct {
	name   string
	desc   string
	schema json.RawMessage
	caps   spi.Caps
	// run 由 mergedTable 回填（真正的路由在 mergedRunner 里）。
	run func(ctx context.Context, name string, args json.RawMessage) (spi.Result, error)
}

func (t *mergedTool) Name() string            { return t.name }
func (t *mergedTool) Description() string     { return t.desc }
func (t *mergedTool) Schema() json.RawMessage { return t.schema }
func (t *mergedTool) Caps() spi.Caps          { return t.caps }
func (t *mergedTool) Exec(ctx context.Context, args json.RawMessage) (spi.Result, error) {
	if t.run == nil {
		return spi.Result{Hint: "合并工具要经工具执行器调用，别直接 Exec"},
			fmt.Errorf("%s 是合并视图，没有直接实现", t.name)
	}
	return t.run(ctx, t.name, args)
}

// mergedTable 是"模型看到的工具表"：同族成员被换成族名工具，其余原样透传。
type mergedTable struct {
	inner  spi.ToolRegistry
	tools  []spi.Tool
	byName map[string]*mergedTool
	spec   map[string]mergedFamily
}

// newMergedTable 建投影。装配里缺成员的族**整族跳过**（宁可少合并一个族，
// 也不要生成一张"action 指向不存在的工具"的表），跳过的族会出现在 warn 里。
func newMergedTable(reg spi.ToolRegistry) (*mergedTable, []string, error) {
	all := reg.Tools()
	have := make(map[string]spi.Tool, len(all))
	for _, t := range all {
		have[t.Name()] = t
	}
	t := &mergedTable{
		inner:  reg,
		byName: map[string]*mergedTool{},
		spec:   map[string]mergedFamily{},
	}
	var warn []string
	replaced := map[string]bool{}     // 被合并掉的原工具名
	insertAt := map[string]spi.Tool{} // 族的第一个成员名 -> 族名工具（插在它原来的位置：表顺序对模型也是线索）
	for _, fam := range mergedFamilies() {
		if _, dup := have[fam.Name]; dup {
			warn = append(warn, fmt.Sprintf("合并 %s 跳过：装配里已经有同名工具", fam.Name))
			continue
		}
		missing := ""
		for _, a := range fam.Actions {
			if _, ok := have[a.Tool]; !ok {
				missing = a.Tool
				break
			}
		}
		if missing != "" {
			warn = append(warn, fmt.Sprintf("合并 %s 跳过：装配里没有 %s", fam.Name, missing))
			continue
		}
		// 每个 action 自己的必填参数：合并后的 schema 表达不了（只能写交集），
		// 所以抄一份到路由里，缺参数时当场报错（见 rewrite）。
		badReq := ""
		for i := range fam.Actions {
			req, err := memberRequired(fam.Actions[i], have[fam.Actions[i].Tool])
			if err != nil {
				badReq = err.Error()
				break
			}
			fam.Actions[i].Required = req
		}
		if badReq != "" {
			warn = append(warn, fmt.Sprintf("合并 %s 跳过：%s", fam.Name, badReq))
			continue
		}
		params, err := buildMergedSchema(fam, have)
		if err != nil {
			// 合不进来的族**不硬合**：一条结构冲突就够让模型看不懂 schema。
			warn = append(warn, fmt.Sprintf("合并 %s 跳过：%v", fam.Name, err))
			continue
		}
		for _, a := range fam.Actions {
			replaced[a.Tool] = true
		}
		caps := spi.Caps{}
		for _, a := range fam.Actions {
			c := have[a.Tool].Caps()
			caps.Writes = caps.Writes || c.Writes
			caps.Executes = caps.Executes || c.Executes
			caps.Network = caps.Network || c.Network
			caps.Destructive = caps.Destructive || c.Destructive
		}
		mt := &mergedTool{
			name:   fam.Name,
			desc:   mergedDescription(fam, have),
			schema: params,
			caps:   caps,
		}
		t.byName[fam.Name] = mt
		t.spec[fam.Name] = fam
		insertAt[fam.Actions[0].Tool] = mt
	}
	// 保持原有顺序（registry 的 Tools() 已按名字排好），把成员换成族名工具。
	t.tools = make([]spi.Tool, 0, len(all))
	for _, tool := range all {
		if replaced[tool.Name()] {
			if mt, ok := insertAt[tool.Name()]; ok {
				t.tools = append(t.tools, mt)
			}
			continue
		}
		t.tools = append(t.tools, tool)
	}
	return t, warn, nil
}

// Tools 返回投影后的表。
func (t *mergedTable) Tools() []spi.Tool { return t.tools }

// bindRunner 把真实执行入口回填给每个投影工具。
//
// 只有 mergedTool.Exec 会用到它：循环永远走 Runner，这里只是让"绕过 Runner
// 直接 Exec"的调用也落在同一条路由上（否则它就成了一个看着能用、实际没实现的壳）。
func (t *mergedTable) bindRunner(run func(ctx context.Context, name string, args json.RawMessage) (spi.Result, error)) {
	for _, mt := range t.byName {
		mt.run = run
	}
}

// Get 对合并名返回投影工具，其余透传到底层表。
//
// 注意：guardedRunner 拿的是**原** registry，所以审批永远看到原工具名的 Caps。
func (t *mergedTable) Get(name string) (spi.Tool, bool) {
	if mt, ok := t.byName[name]; ok {
		return mt, true
	}
	return t.inner.Get(name)
}

// Register 透传：合并只投影，不允许往宿主的表里加东西。
func (t *mergedTable) Register(spi.Tool) error {
	return fmt.Errorf("合并视图不能注册工具")
}

// rewrite 把一次合并调用还原成"原工具名 + 原参数"。
//
// 不是合并名就原样返回（nil, nil 表示"不用管"）。
func (t *mergedTable) rewrite(name string, args json.RawMessage) (string, json.RawMessage, error) {
	fam, ok := t.spec[name]
	if !ok {
		return "", nil, nil
	}
	var in map[string]json.RawMessage
	if len(strings.TrimSpace(string(args))) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", nil, fmt.Errorf("%s 的参数不是 JSON 对象: %v", name, err)
		}
	}
	if in == nil {
		in = map[string]json.RawMessage{}
	}
	var action string
	if raw, ok := in["action"]; ok {
		_ = json.Unmarshal(raw, &action)
	}
	delete(in, "action")
	var picked *mergedAction
	for i := range fam.Actions {
		if fam.Actions[i].Action == action {
			picked = &fam.Actions[i]
			break
		}
	}
	if picked == nil && action != "" {
		// 容错：有的模型会把**全名**填进 action（hindsight_ingest_document）。
		// 全名唯一确定一个成员，语义没有歧义，直接认——为拼写白跑一步不值得。
		for i := range fam.Actions {
			if fam.Actions[i].Tool == action {
				picked = &fam.Actions[i]
				break
			}
		}
	}
	if picked == nil {
		return "", nil, fmt.Errorf("%s 需要 action=%s（收到 %q）", name, strings.Join(actionNames(fam), "|"), action)
	}
	// 必填参数在这里兜：合并后的 schema 只有一个 required（交集），
	// 原来的"create 必填 description"在表里看不见了，而工具自己不校验时
	// 就会静默建成一条没描述的任务。缺了就当场说清楚，别让它悄悄过去。
	if miss := missingRequired(picked.Required, in, picked.Aliases); len(miss) > 0 {
		return "", nil, fmt.Errorf("%s action=%s 缺必填参数 %s",
			name, picked.Action, strings.Join(miss, "/"))
	}
	// update_action → action：只有声明了 Rename 的 action 才做改名。
	for from, to := range picked.Rename {
		if raw, ok := in[from]; ok {
			in[to] = raw
			delete(in, from)
		}
	}
	out, err := json.Marshal(in)
	if err != nil {
		return "", nil, fmt.Errorf("%s 的参数序列化失败: %v", name, err)
	}
	return picked.Tool, out, nil
}

func actionNames(fam mergedFamily) []string {
	out := make([]string, 0, len(fam.Actions))
	for _, a := range fam.Actions {
		out = append(out, a.Action)
	}
	return out
}

// memberRequired 读成员 schema 的 required，并把参数名换成"模型看到的"那个。
//
// 成员自己的 action（team_task_update 的 claim/complete/…）不算：它由外层
// action 决定，模型不会（也不该）再填一遍。
func memberRequired(a mergedAction, tool spi.Tool) ([]string, error) {
	var s struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &s); err != nil {
		return nil, fmt.Errorf("%s 的 schema 解析失败: %v", a.Tool, err)
	}
	out := make([]string, 0, len(s.Required))
	for _, r := range s.Required {
		if r == "action" {
			continue
		}
		for from, to := range a.Rename {
			if to == r {
				r = from
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func missingRequired(required []string, in map[string]json.RawMessage, aliases map[string][]string) []string {
	var miss []string
	for _, r := range required {
		if _, ok := in[r]; ok {
			continue
		}
		ok := false
		for _, alt := range aliases[r] {
			if _, has := in[alt]; has {
				ok = true
				break
			}
		}
		if !ok {
			miss = append(miss, r)
		}
	}
	return miss
}

// mergedRunner 是唯一知道"action → 原工具名"的执行入口。
//
// 它在守卫**外层**：先把名字还原，守卫按原工具的 Caps 判审批，
// 所以合并不改变任何一次调用的审批结果。
type mergedRunner struct {
	inner spi.Runner
	table *mergedTable
}

func (m *mergedRunner) Run(ctx context.Context, name string, args json.RawMessage) (spi.Result, error) {
	real, rewritten, err := m.table.rewrite(name, args)
	if err != nil {
		// 失败要给下一步：Hint 里直接列出合法 action。
		return spi.Result{Hint: err.Error() + "；换成表里列出的 action 再试"}, err
	}
	if real == "" {
		return m.inner.Run(ctx, name, args)
	}
	return m.inner.Run(ctx, real, rewritten)
}

// mergedDescription 由成员自己的描述拼出来，不另写一份——
// 另写一份就会和 modules/ 里的真实描述漂移。
func mergedDescription(fam mergedFamily, have map[string]spi.Tool) string {
	var b strings.Builder
	b.WriteString(fam.Summary)
	b.WriteString("。action=")
	for i, a := range fam.Actions {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(a.Action)
		b.WriteString("：")
		b.WriteString(shortClause(have[a.Tool].Description(), 34))
	}
	if fam.Note != "" {
		b.WriteString("。")
		b.WriteString(fam.Note)
	}
	return b.String()
}

// shortClause 取描述的第一句（到第一个逗号/句号/分号），再按字数截断。
//
// 整段照搬会把这四族的描述拼成一篇长文，省下来的字节又被吃掉；
// 取第一句刚好保留"什么时候用它"这半句。
func shortClause(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "，。；,;\n"); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes]) + "…"
}

// buildMergedSchema 把一族的 schema 合成一张：
//
//	properties = 各成员 properties 的并集（同名必须同构，否则整族不合并）
//	required   = 各成员 required 的交集（不逼模型为别的 action 填参数）+ action
//	action     = 枚举，取值就是原工具名的后缀
//
// 交集丢掉的那部分约束没有消失——它进了每个 action 的 Required，
// 由路由在调用前校验（schema 本身只有一个 required，表达不了"按 action 变"）。
//
// 不使用 oneOf/anyOf：本仓对接的是 OpenAI 兼容的 function calling，
// 复杂的组合 schema 会被网关或模型误读——宁可少合并一个族。
func buildMergedSchema(fam mergedFamily, have map[string]spi.Tool) (json.RawMessage, error) {
	props := make(map[string]json.RawMessage, 8)
	var required []string
	first := true
	for _, a := range fam.Actions {
		var s struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(have[a.Tool].Schema(), &s); err != nil {
			return nil, fmt.Errorf("%s 的 schema 解析失败: %v", a.Tool, err)
		}
		if s.Type != "object" {
			return nil, fmt.Errorf("%s 的 schema 顶层不是 object", a.Tool)
		}
		for k, raw := range s.Properties {
			vis := k
			for from, to := range a.Rename {
				if to == k {
					vis = from
				}
			}
			if vis == "action" {
				return nil, fmt.Errorf("%s 自带 action 参数且没有改名，和外层 action 冲突", a.Tool)
			}
			if prev, ok := props[vis]; ok {
				if !sameShape(prev, raw) {
					return nil, fmt.Errorf("%s 与同族其它工具对参数 %q 的定义不一致（结构不同）", a.Tool, vis)
				}
				continue
			}
			props[vis] = raw
		}
		req := a.Required
		if first {
			required = req
			first = false
		} else {
			required = intersectStrings(required, req)
		}
	}
	props["action"] = mustRaw(map[string]any{
		"type": "string",
		"enum": actionNames(fam),
	})
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             append([]string{"action"}, required...),
		"additionalProperties": false,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("合并后的 schema 序列化失败: %v", err)
	}
	return b, nil
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // 只喂字面量，不可能失败
	}
	return b
}

func intersectStrings(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

// sameShape 比较两张参数 schema 的**结构**（忽略 description）。
//
// 只差说明文字算同构：合并时保留先遇到的那份说明。
func sameShape(a, b json.RawMessage) bool {
	x, y := shapeless(a), shapeless(b)
	return x != "" && x == y
}

func shapeless(raw json.RawMessage) string {
	// 形状比对要忽略的只有注解 description。这条语义与 agent.CompactSchema
	// 是同一个东西，所以直接用它的实现（internal/schema.Compact），不另写一份。
	// 坏输入仍由这里判空：sameShape 要求两边都非空，不拿两个空串当同构。
	if !json.Valid(raw) {
		return ""
	}
	return string(schemarule.Compact(raw))
}

// ---- compact 的后半段：把 schema 骨架里"不用写"的那几条去掉 ----
//
// 为什么还压得动：compact 去掉的只是参数的说明文字（"这个参数填什么"），
// 剩下的 JSON Schema 里还有一类东西**不携带任何信息**——接口已经规定、
// 或者省略就等于默认值：
//
//	顶层 "type":"object"    function calling 的 parameters 必须是对象，
//	                        这是接口规定，写出来是冗余（57 个工具都写了一遍）
//	"properties":{}         空的 properties 与"没有 properties"等价
//	"required":[]           同上
//	"additionalProperties":true   JSON Schema 的默认值就是 true
//
// 一个字节都不动的：参数名、type 取值、enum、required 里的项、
// additionalProperties:false（它是有内容的约束）、minimum/maximum/format。
// 判据不是"看起来等价"，是 cmd/dshd/tool_schema_test.go 里逐工具比对
// **归一化后的结构**：少一个 required、enum 变短，那条测试就红。
//
// 说明文字怎么删：与 internal/agent.CompactSchema 共用 internal/schema。
// 那个包只负责一件事——判断某个 description 是**注解**（schema 对象上，
// 删）还是**参数名**（properties 映射里的键，原样保留，哪怕它就叫
// description）。本仓有 8 个工具真有这个参数，删掉之后 required 里点名了它、
// properties 里却没有，模型只看到一个"必填但没类型"的名字。
// 2026-11-04 这条规则在 CompactSchema 与这里的 stripDescriptions 里各写了
// 一遍、各错一次，所以现在只有一处判断函数（internal/schema.IsAnnotationKey），
// 两处都调它。
//
// 为什么整段收紧不直接复用 CompactSchema、而要保留这一份：两者是两种变换，
// 不是同一件事的两种写法。
//   - 这里还要删冗余骨架（上面那四条），CompactSchema 不删；
//   - 这里用 json.RawMessage 承载子节点，type/enum/数字**逐字节**不变
//     （9007199254740993 不会过 float64 变成 ...992，1e+04 不会重排）；
//   - 这里只递归 properties/items；CompactSchema 递归一切对象，连 default
//     里的同名键也会删——那是它不该动的地方。
// 完整差异与实测见 docs/silent-success-audit.md §4 D3，判据是
// cmd/dshd/tool_merged_rule_test.go 的两处实现互相对照。
//
// 为什么不做 $ref 去重：OpenAI 兼容接口的每个 function 各带一份 parameters，
// 没有跨工具的 definitions 可引用；同一个工具内部重复的片段实测只有
// 31 处 565 字节，而定义块本身也要占字节（scripts/tool-table-anatomy.py）。
//
// 为什么不做"顶层 type 之外的类型简写"：类型取值是模型判断填什么的唯一依据，
// 任何简写（"str"）都是另一种契约，网关不认、模型也只能猜。

// tightTool 是模型看到的工具外壳：名字/描述/caps/执行都回落原工具，
// 只有 Schema 换成收紧后的那一份。
type tightTool struct {
	inner  spi.Tool
	schema json.RawMessage
}

func (t *tightTool) Name() string            { return t.inner.Name() }
func (t *tightTool) Description() string     { return t.inner.Description() }
func (t *tightTool) Schema() json.RawMessage { return t.schema }
func (t *tightTool) Caps() spi.Caps          { return t.inner.Caps() }
func (t *tightTool) Exec(ctx context.Context, args json.RawMessage) (spi.Result, error) {
	return t.inner.Exec(ctx, args)
}

// tightTable 是收紧后的表。Get 也返回收紧过的那个：同一个工具在"列表"和
// "按名取"两条路上必须是同一份 schema，否则模型看到的和按名取到的会不一样。
type tightTable struct {
	inner  spi.ToolRegistry
	tools  []spi.Tool
	byName map[string]spi.Tool
}

// tightenTable 投影出一张"模型看到的最终形态"的表：去参数说明 + 去冗余骨架。
func tightenTable(reg spi.ToolRegistry) *tightTable {
	all := reg.Tools()
	t := &tightTable{
		inner:  reg,
		tools:  make([]spi.Tool, 0, len(all)),
		byName: make(map[string]spi.Tool, len(all)),
	}
	for _, tool := range all {
		view := &tightTool{
			inner:  tool,
			schema: tightenSchema(tool.Schema()),
		}
		t.tools = append(t.tools, view)
		t.byName[tool.Name()] = view
	}
	return t
}

func (t *tightTable) Tools() []spi.Tool { return t.tools }

func (t *tightTable) Get(name string) (spi.Tool, bool) {
	if x, ok := t.byName[name]; ok {
		return x, true
	}
	return t.inner.Get(name)
}

// Register 透传拒绝：投影层只改"看到的"，不往宿主的表里加东西。
func (t *tightTable) Register(spi.Tool) error {
	return fmt.Errorf("投影视图不能注册工具")
}

// tightenSchema 是 compact 投影的全部：去掉参数说明文字，再去掉冗余骨架。
// 不是 JSON 对象就原样返回——投影宁可少压一点，也不能把看不懂的东西改坏。
func tightenSchema(raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	out, err := tightenObject(obj, true)
	if err != nil {
		return raw
	}
	return out
}

// tightenObject 就地收紧一个 schema 对象并序列化回去。
//
// 只用 json.RawMessage 承载子节点、不解析成 any：这样 type 取值、enum 字面量、
// 数字（10000 不能变成 1e+04）都**逐字节**保持原样，压表动不到语义。
//
// 注意 properties 是**单独一段循环**、不交给通用的对象遍历：那一层的键是
// 参数名（模型要填的东西），通用遍历会把名叫 description 的参数当注解删掉。
// 这也是这个函数要调 schemarule.IsAnnotationKey 的原因：到了这里，obj 一定是
// **schema 对象**（properties 的值、items 的值），所以位置参数是 false；
// 参数名那一层永远不走这段遍历。
func tightenObject(obj map[string]json.RawMessage, root bool) (json.RawMessage, error) {
	// 这一层是 schema 对象：description 是给模型看的说明，compact 本来就不发。
	// 删哪个键由唯一那份判断决定，不在这里按 key 名写删除分支。
	for k := range obj {
		if schemarule.IsAnnotationKey(k, false) {
			delete(obj, k)
		}
	}
	// properties 缺失也按"空对象"处理：parameters 的根必须是对象。
	emptyProps := true
	if raw, ok := obj["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(raw, &props); err != nil {
			return nil, err
		}
		emptyProps = len(props) == 0
		for name, sub := range props {
			var child map[string]json.RawMessage
			if err := json.Unmarshal(sub, &child); err != nil {
				continue // 不是对象的子 schema（罕见写法）：原样留着
			}
			next, err := tightenObject(child, false)
			if err != nil {
				return nil, err
			}
			props[name] = next
		}
		if props == nil {
			props = map[string]json.RawMessage{}
		}
		// **空 properties 不能删**：无参工具若只剩
		// {"additionalProperties":false}，WPS 灵犀的上游 schema 转换器会在
		// 开流前直接拒绝，插件把它归一成 503 upstream_unavailable。顶层
		// type:object 同理，见下面。参数区因此固定保留空对象骨架。
		b, err := json.Marshal(props)
		if err != nil {
			return nil, err
		}
		obj["properties"] = b
	}
	// type:object 但 properties 缺失/为空时，补回空对象。无参工具与
	// 嵌套空对象都必须保持可被上游 schema 转换器解析的完整形状。
	if isRawString(obj["type"], "object") && emptyProps {
		obj["properties"] = json.RawMessage("{}")
	}
	// 顶层 type:object 只有在 properties 非空时才是可省的冗余骨架。
	// 无参对象必须同时留下 type 与 properties，否则上游解析器直接失败。
	if root && isRawString(obj["type"], "object") && !emptyProps {
		delete(obj, "type")
	}
	if raw, ok := obj["items"]; ok {
		var child map[string]json.RawMessage
		if err := json.Unmarshal(raw, &child); err == nil {
			next, err := tightenObject(child, false)
			if err != nil {
				return nil, err
			}
			obj["items"] = next
		}
	}
	if isRawBool(obj["additionalProperties"], true) {
		delete(obj, "additionalProperties")
	}
	if raw, ok := obj["required"]; ok {
		var req []string
		if err := json.Unmarshal(raw, &req); err == nil && len(req) == 0 {
			delete(obj, "required")
		}
	}
	return json.Marshal(obj)
}

// isRawString 判断一段 RawMessage 是不是指定的字符串字面量。
func isRawString(raw json.RawMessage, want string) bool {
	return strings.TrimSpace(string(raw)) == `"`+want+`"`
}

// isRawBool 判断一段 RawMessage 是不是指定的布尔字面量。
//
// 单独一个函数而不是复用 isRawString：布尔在 JSON 里不带引号，
// 复用那个（比较 `"true"`）会永远匹配不上——踩过，additionalProperties:true 一个都没删掉。
func isRawBool(raw json.RawMessage, want bool) bool {
	if want {
		return strings.TrimSpace(string(raw)) == "true"
	}
	return strings.TrimSpace(string(raw)) == "false"
}
