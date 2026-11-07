// tool_lazy.go 是工具表的第四档：lazy（按需装载）。
//
// 前三档（full / compact / merged）都只改"模型看到什么"。lazy 多走一步：
// 表里只常驻最常用的 30 个工具，其余工具**既没有 schema 也没有外壳**，
// 只在系统提示的目录里留"名字 + 一句话"；模型要用时先调 tool_load 取完整
// schema，宿主把取到的工具**注入后续每一步的 tools 字段**（状态化），
// 下一步就能直接调用。
//
// 为什么是状态化注入（而不是"看一眼"）：本仓的循环每步都从 l.Tools.Tools()
// 重建 toolDefs（internal/agent/loop.go），所以"表"本来就可以在两步之间变。
// 若取用后不注入，模型拿到 schema 却没有可调用的入口——那等于把 merged 的
// 赌注再赌一遍，且这次连成功路径都没有。
//
// 为什么 lazy 会**挡回**没装载的调用（唯一一档这么做）：投影层本来只改可见性，
// 执行仍按真 registry 查名字（merged 也是这么做的）。但如果这里不挡，模型
// 凭记忆直接喊一个隐藏工具名也能跑通——那"先 tool_load"这条规则就是假的，
// A/B 也测不出"模型会不会用机制"。挡回的代价是一次 tool_load；收益是这条
// 规则可被证伪、可被测量。
//
// 装载集是**会话状态**，不是进程状态：serve 每轮起一个新的 run 进程，上一轮
// 取过的工具这一轮必须直接可用，否则模型凭记忆喊它会被挡回、白跑一次 tool_load。
// 它不额外落盘——每次 tool_load 本来就是一条 tool/call 事件，写在 append-only
// 会话日志里，新一轮只是把它**回放**出来（见下面"装载集是会话状态"那一段）。
// 于是挡回只剩一种情形：**这个会话从没取过它**。
//
// 隐藏的 30 名之外的 27 个是谁：由 scripts/tool-table-hot.py 从真实语料
// （宿主 home 下的 sessions）重算，**不是手抄的名单**。
// 2026-11-04 的语料：80,898 次 tool/call，我们自己的 57 个工具里前 30 名覆盖
// 99.43%；被藏的 27 个合计只占 0.57%。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

// defaultHotTools 是 lazy 的常驻集：语料里调用次数前 30 的**我们自己的**工具。
//
// 生成方式（可复现，别手改）：
//
//	go build -o "tmp/DSH GO.exe" ./cmd/dshd
//	python scripts/tool-table-hot.py              # 打印并核对这张表
//	python scripts/tool-table-hot.py --go         # 生成下面这段字面量
//
// 排序即名次（第 1 名 bash 69.53% … 第 30 名 spawn_teammate 0.07%）。
// 换语料/加新工具后重跑脚本，把新的 --go 输出整段替换进来，再跑
// go test ./cmd/dshd -run TestLazyHotList。
var defaultHotTools = []string{
	"bash",
	"read",
	"edit",
	"write",
	"job_output",
	"grep",
	"read_image",
	"glob",
	"send_message",
	"todo_write",
	"web_fetch",
	"subagent_fork",
	"subagent",
	"pwsh",
	"github",
	"get_goal",
	"wait_agent",
	"team_task_update",
	"list_agents",
	"team_task_get",
	"job_kill",
	"create_goal",
	"schedule_create",
	"update_goal",
	"hindsight_search_knowledge_pages",
	"team_task_create",
	"web_search",
	"hindsight_list_knowledge_pages",
	"ask_user_question",
	"spawn_teammate",
}

// toolLoadName 是取用入口。它不在 modules/ 里注册——它是投影层自带的一个工具，
// 由 lazyRunner 在守卫外层接住（与 mergedRunner 同一个位置）。
const toolLoadName = "tool_load"

// lazyTable 是"按需装载"的工具表。
//
// 内部（inner）已经是 tightTable：隐藏的工具在这里拿到的就是模型最终会看到的
// 那一份 schema，装载时原样发出去，不再二次投影——否则模型拿到的和真正调用的
// 可能是两个形状。
type lazyTable struct {
	inner spi.ToolRegistry

	mu      sync.RWMutex
	hot     []spi.Tool          // 常驻（按名字排好）
	hotSet  map[string]bool     // 常驻名单（判定用）
	hidden  map[string]spi.Tool // 表外工具（tight 后的）
	names   []string            // 表外工具名，按名字排好（目录与装载都靠它，顺序稳定）
	loaded  map[string]bool     // 已装载：从下一步起进 Tools()
	loader  spi.Tool            // tool_load 自己
	missing []string            // 常驻名单里装配没有的名字（warn 用，不致命）
}

// newLazyTable 建 lazy 表。hot 是常驻名单（用 defaultHotTools；
// 测试可以传别的名单）。返回的 warn 要打给运维看，但不阻断——
// 名单里少一个工具只意味着它被藏起来了，不是装配失败。
func newLazyTable(inner spi.ToolRegistry, hot []string) (*lazyTable, []string, error) {
	if inner == nil {
		return nil, nil, fmt.Errorf("lazy 表需要一个底层注册表")
	}
	all := inner.Tools()
	byName := make(map[string]spi.Tool, len(all))
	for _, t := range all {
		byName[t.Name()] = t
	}
	if _, dup := byName[toolLoadName]; dup {
		return nil, nil, fmt.Errorf("装配里已经有叫 %s 的工具，lazy 表没法再加取用入口", toolLoadName)
	}
	t := &lazyTable{
		inner:  inner,
		hotSet: map[string]bool{},
		hidden: map[string]spi.Tool{},
		loaded: map[string]bool{},
		loader: &toolLoadTool{},
	}
	var warn []string
	for _, n := range hot {
		n = strings.TrimSpace(n)
		if n == "" || t.hotSet[n] {
			continue
		}
		if _, ok := byName[n]; !ok {
			t.missing = append(t.missing, n)
			continue
		}
		t.hotSet[n] = true
	}
	for _, tool := range all { // all 已按名字排序
		if t.hotSet[tool.Name()] {
			t.hot = append(t.hot, tool)
			continue
		}
		t.hidden[tool.Name()] = tool
		t.names = append(t.names, tool.Name())
	}
	sort.Strings(t.names)
	if len(t.missing) > 0 {
		warn = append(warn, fmt.Sprintf("lazy 常驻名单里有 %d 个工具装配里没有（已跳过）：%s",
			len(t.missing), strings.Join(t.missing, ", ")))
	}
	if len(t.hot) == 0 {
		// 全藏起来等于把工具表清空：宁可这张表没有意义，也不能让模型无工具可用。
		return nil, nil, fmt.Errorf("lazy 常驻集为空（名单=%v，装配=%d 个工具）", hot, len(all))
	}
	return t, warn, nil
}

// Tools 返回当前这一步模型看到的表：常驻 + tool_load + 已装载。
//
// 每次都重新拼、并按名字排序：同一状态必须产生同样的字节（提示装配要确定性），
// 而装载是会话内的状态变化——变了就该在下一步体现，不需要重建整张表。
func (t *lazyTable) Tools() []spi.Tool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]spi.Tool, 0, len(t.hot)+1+len(t.loaded))
	out = append(out, t.hot...)
	out = append(out, t.loader)
	for _, n := range t.names {
		if t.loaded[n] {
			out = append(out, t.hidden[n])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Get 对"当前可见"的工具返回；没装载的表外工具**查不到**。
//
// 注意：执行侧用的是真 registry（guardedRunner 里那个），这里的 Get 只服务
// 可见性查询（子代理的 noSubagent 包装等）。挡回未装载调用的是 lazyRunner。
func (t *lazyTable) Get(name string) (spi.Tool, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if name == toolLoadName {
		return t.loader, true
	}
	if t.hotSet[name] {
		for _, x := range t.hot {
			if x.Name() == name {
				return x, true
			}
		}
	}
	if t.loaded[name] {
		if x, ok := t.hidden[name]; ok {
			return x, true
		}
	}
	return nil, false
}

// Register 拒绝：投影层只改"看到的"，不往宿主的表里加东西。
func (t *lazyTable) Register(spi.Tool) error {
	return fmt.Errorf("lazy 视图不能注册工具")
}

// isHidden 报告某个名字是不是"表外且未装载"。
func (t *lazyTable) isHidden(name string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, hidden := t.hidden[name]
	return hidden && !t.loaded[name]
}

// Directory 是给系统提示的那一段：规则 + 每个表外工具的"名字 — 一句话"。
//
// 为什么放系统提示而不是 tool_load 的描述里：被藏起来的工具，它自己的描述也
// 一起藏了，"什么时候用我"这句话没有别的地方可放（docs/tool-table-budget.md
// 候选 C 的第 2 条）。放 tool_load 的描述里效果相同、字节相同，但规则和目录
// 分在两处更容易漂移；而"工具不全"这件事本身就是系统提示该说的。
func (t *lazyTable) Directory() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var b strings.Builder
	fmt.Fprintf(&b, "## 工具表是精简的（lazy 模式）\n\n")
	fmt.Fprintf(&b, "tools 里只常驻最常用的 %d 个工具。下面这些工具**没有 schema**，直接调用会被挡回；\n", len(t.hot))
	b.WriteString("缺什么就先调 tool_load（参数 {\"names\":[\"工具名\"]}，一次 1~8 个），")
	b.WriteString("它返回完整参数，**下一步**就能直接调用。\n\n")
	b.WriteString("目录（名字 — 用途）：\n")
	for _, n := range t.names {
		if t.loaded[n] {
			continue // 已装载的已经回到表里，目录里再列一遍是重复信息
		}
		fmt.Fprintf(&b, "%s — %s\n", n, shortClause(t.hidden[n].Description(), 30))
	}
	return b.String()
}

// ---- 装载集是会话状态：跨进程（跨轮）保留 ----

// restore 把"回放出来的名字"灌回表里，返回这次**真的**灌进去的那些。
//
// 只认一条：名字必须还在当前的表外集合里。模块被卸掉、常驻名单重算过
// （某个工具从表外变成常驻、或反过来）都会让老日志里的名字对不上——
// 对不上就不装载：宁可少给一个入口（模型下一步 tool_load 还能拿回来），
// 也不能让工具表里出现一个装配根本提供不了的入口。
func (t *lazyTable) restore(names []string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || t.hotSet[n] || t.loaded[n] {
			continue
		}
		if _, ok := t.hidden[n]; !ok {
			continue
		}
		t.loaded[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// loadedNames 返回当前已装载的名字（排好序）。诊断与测试用。
func (t *lazyTable) loadedNames() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.loaded))
	for n := range t.loaded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// lazyLoadHistory 从一份已解析的会话日志里回放"这个会话取过哪些工具"。
//
// 存哪、为什么存在这里（而不是会话目录下的派生文件 / Transport journal /
// home 里的按会话索引）：
//
//	tool_load 每次被调用，都会以一条 `tool/call` 事件写进 v4 会话日志
//	（agent.Loop 的 EventToolCall → session.ToolCallEvent）。也就是说
//	**装载史本来就在那份 append-only 日志里**，这里只是把它读出来——
//	不新增文件、不新增事件、读路径不写盘（R5）。
//
// 另外三处的取舍：
//   - 会话目录下的派生文件：同一件事的第二个写者。日志里明明记着这次装载，
//     再造一份就可能对不上（进程被 kill 在写 tool/result 之前），而两边不一致时
//     没人能判谁对。而且它会长出 v4 之外的新落盘格式，得自己处理路径规则、
//     写锁与清理，收益却是零——需要的事实日志里已经有了。
//   - Transport journal：那是 serve 里按连接派生的投影，`run` 根本没有
//     （而且是同一份日志的下游，绕远）。本轮也不许改 serve*.go。
//   - home 下的按会话索引：要么读路径写盘（R5 不允许），要么另做失效/GC
//     （会话被删、被 fork、id 复用），而它索引的事实就在会话文件里。
//
// 扫的是**原始事件**（l.Events），不是 surface：压缩只改模型的上下文，
// 不改变"这个会话取过哪些工具"。压缩之后模型照样能直接调它们，因为
// 每一轮的 tools 字段里都有它们（见本文件开头"状态化注入"那一段）。
//
// 宽容到底：任何一条事件读不动（参数不是 JSON、名字是空串）就跳过它。
// 回放的是派生状态，没有"因为一条坏事件整轮跑不起来"的道理。
func lazyLoadHistory(l *v4.Log) []string {
	if l == nil {
		return nil
	}
	var out []string
	for _, ev := range l.Events {
		if ev.Type != "tool/call" {
			continue
		}
		var d struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			continue
		}
		if d.Name != toolLoadName {
			continue
		}
		args := d.Arguments
		// 我们自己的写入点把 arguments 写成 JSON **字符串**；这里容忍它
		// 本来就是对象的情况（别的写入者/手工造的样本），否则一条就废掉整段回放。
		var asStr string
		if err := json.Unmarshal(args, &asStr); err == nil {
			args = json.RawMessage(asStr)
		}
		names, err := parseLoadArgs(args)
		if err != nil {
			continue
		}
		out = append(out, names...)
	}
	return out
}

// restoreLoaded 是 cmdRun 里那一步的全部决策，单独拎出来是为了可测：
// 日志在（back != nil）就回放，读不出来（back == nil）就当空集。
//
// **它永远不返回错误**——这是这个设计的兜底：装载集坏掉/过期/读不出来时，
// 唯一允许的后果是"回到什么都没装载"，也就是改造前那条自愈路径
// （挡回 → Hint → tool_load）原样还在。工具不会因为状态坏了而不可用。
func restoreLoaded(t *lazyTable, back *v4.Log) []string {
	if t == nil {
		return nil
	}
	return t.restore(lazyLoadHistory(back))
}

// prepareLazy 是 lazy 模式"一轮开始时"的全部动作，**只有这一份定义**：
//
//	回放装载集（从这一轮所属会话的 v4 日志）→ 再拼系统提示目录
//
// 顺序不能反：Directory 会跳过已装载的工具，先拼目录就会把上一轮取过的工具
// 又列一遍（重复信息 = 白花字节，而且模型会以为它还没装载）。
//
// 为什么入参只有"这个会话的日志"：装载集是**会话状态**。
// cmdRun 走这一条；serve 的每一轮走的是子进程 `dshd run`，也就是 cmdRun 本身
// ——所以 serve 与 run 不是"两条一样的接线"，是**同一条**（serve 只负责把
// --tools-mode 透传下去，见 serve_tools.go 的 childArgs）。这样处理还有一个
// 直接后果：serve 一个进程里并发/切换多个会话时，回放的输入永远只有
// `--session` 指的那份日志，**没有任何比它更宽的来源**，A 会话的装载集
// 也就没有载体能漏到 B——B 照旧会被挡回并自愈。
func prepareLazy(t *lazyTable, back *v4.Log) (dir string, restored []string) {
	if t == nil {
		return "", nil
	}
	// **两行，不能合成一句 return。** Go 会把 return 的操作数按书写顺序求值，
	// 写成 `return t.Directory(), restoreLoaded(t, back)` 就是先拼目录再回放——
	// 目录里于是还列着"已经装载"的工具，模型会被告知一个它其实可以直接调的工具
	// 需要 tool_load（实测：真 serve 第二轮直接调用成功，而系统提示还在说它没装载）。
	restored = restoreLoaded(t, back)
	dir = t.Directory()
	return dir, restored
}

// load 执行一次 tool_load：把名字解析出来、校验、标记装载，并把 schema 发回去。
//
// 返回的 Text 会进模型上下文（tool 消息），之后每一步都跟着重发——所以只发
// **紧凑的一行一个 JSON**，不发缩进美化版。
func (t *lazyTable) load(args json.RawMessage) (spi.Result, error) {
	names, err := parseLoadArgs(args)
	if err != nil {
		return spi.Result{Hint: `用法：tool_load {"names":["web_search"]}（也接受 "name":"web_search"）`}, err
	}
	var (
		loaded  []string // 这次新装载的
		already []string // 本来就在表里（常驻或已装载）
		unknown []string // 没这个名字
	)
	t.mu.Lock()
	for _, n := range names {
		switch {
		case n == toolLoadName || t.hotSet[n] || t.loaded[n]:
			already = append(already, n)
		case t.hidden[n] != nil:
			t.loaded[n] = true
			loaded = append(loaded, n)
		default:
			unknown = append(unknown, n)
		}
	}
	var b strings.Builder
	if len(loaded) > 0 {
		b.WriteString("已装载（下一步起出现在 tools 里，可直接调用；参数照下面这份用）：\n")
		for _, n := range loaded {
			tool := t.hidden[n]
			fmt.Fprintf(&b, `{"name":%s,"description":%s,"parameters":%s}`+"\n",
				mustJSON(tool.Name()), mustJSON(tool.Description()), string(tool.Schema()))
		}
	}
	t.mu.Unlock()
	if len(already) > 0 {
		fmt.Fprintf(&b, "已在表里，不用再取：%s\n", strings.Join(already, ", "))
	}
	if len(unknown) > 0 {
		fmt.Fprintf(&b, "没有叫这些名字的工具：%s。目录里的名字要精确抄；常驻工具不用取。\n",
			strings.Join(unknown, ", "))
		if s := t.suggest(unknown); s != "" {
			fmt.Fprintf(&b, "最接近的：%s\n", s)
		}
	}
	if len(loaded) == 0 && len(already) == 0 {
		// 一个都没装上，也没有一个是"本来就在表里"的：这是失败，要带 Hint（R3）。
		hint := strings.TrimSpace(b.String()) + "从系统提示的目录里抄一个名字再调一次"
		return spi.Result{Summary: "没有装载任何工具", Text: strings.TrimSpace(b.String()), Hint: hint},
			fmt.Errorf("tool_load 没有装载任何工具（%s）", strings.Join(names, ", "))
	}
	sum := "已装载 " + strings.Join(loaded, ", ")
	if len(loaded) == 0 {
		// 全是"本来就在表里"：这不是失败，告诉模型直接调用即可（省掉重复回填）。
		sum = "已在表里：" + strings.Join(already, ", ")
	}
	return spi.Result{Summary: sum, Text: strings.TrimSpace(b.String())}, nil
}

// suggest 给拼错的名字找最接近的表外工具（前缀或包含都算）。
func (t *lazyTable) suggest(names []string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	all := make([]string, 0, len(t.hot)+len(t.names))
	for _, h := range t.hot {
		all = append(all, h.Name())
	}
	all = append(all, t.names...)
	var out []string
	for _, q := range names {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			continue
		}
		for _, n := range all {
			ln := strings.ToLower(n)
			// 包含关系覆盖"少打一段"，编辑距离覆盖"打错字母/调换顺序"
			// （实测最常见的拼错是 web_serach 这种相邻字母调换）。
			if strings.Contains(ln, q) || strings.Contains(q, ln) || editDistanceLE(q, ln, 2) {
				out = append(out, n)
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	if len(out) > 5 {
		out = out[:5]
	}
	return strings.Join(out, ", ")
}

// editDistanceLE 判断两个字符串的编辑距离是否 ≤ max。
//
// 只给拼错的名字找候选，长度都很短，朴素 DP 就够；不做早期截断的优化，
// 免得"看起来对但边界错"——这段代码只影响提示文本，不影响执行。
func editDistanceLE(a, b string, max int) bool {
	ra, rb := []rune(a), []rune(b)
	if abs(len(ra)-len(rb)) > max {
		return false
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if cur[j] < best {
				best = cur[j]
			}
		}
		if best > max {
			return false // 这一行已经全部超过 max，后面只会更大
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)] <= max
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// parseLoadArgs 解析 tool_load 的参数。
//
// 宽容到"多认几个键"是有意的：参数名是模型自己写的，names/name/tools/tool
// 都是常见叫法；为拼写白跑一步不值得（和 merged 认全名 action 同一条理由）。
func parseLoadArgs(args json.RawMessage) ([]string, error) {
	var in map[string]json.RawMessage
	if s := strings.TrimSpace(string(args)); s != "" && s != "null" {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("参数不是 JSON 对象: %v", err)
		}
	}
	var names []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}
	for _, key := range []string{"names", "tools"} {
		raw, ok := in[key]
		if !ok {
			continue
		}
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			add(one)
			continue
		}
		var many []string
		if err := json.Unmarshal(raw, &many); err != nil {
			return nil, fmt.Errorf("%s 要是字符串或字符串数组", key)
		}
		for _, s := range many {
			add(s)
		}
	}
	for _, key := range []string{"name", "tool", "tool_name"} {
		if raw, ok := in[key]; ok {
			var one string
			if err := json.Unmarshal(raw, &one); err == nil {
				add(one)
			}
		}
	}
	// 去重，保序，最多 8 个：一次取太多等于把整张表搬回来，不如不藏。
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
		if len(out) == 8 {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(`缺参数：{"names":["工具名"]}`)
	}
	return out, nil
}

// mustJSON 把一个字符串序列化成 JSON 字面量（引号/转义交给 encoding/json）。
func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// toolLoadTool 是取用入口的工具外壳。Exec 永远是错的——真正的实现在
// lazyRunner 里（守卫外层），和 mergedTool 同一条规矩。
type toolLoadTool struct{}

func (t *toolLoadTool) Name() string        { return toolLoadName }
func (t *toolLoadTool) Description() string { return toolLoadDescription }
func (t *toolLoadTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"}},"name":{"type":"string"}}}`)
}

// Caps 是空的：取一份 schema 不写盘、不起进程、不联网，不需要审批。
func (t *toolLoadTool) Caps() spi.Caps { return spi.Caps{} }

func (t *toolLoadTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	return spi.Result{Hint: "tool_load 要经工具执行器调用"},
		fmt.Errorf("%s 是投影层工具，没有直接实现", toolLoadName)
}

// toolLoadDescription 要回答"什么时候用我"，不是"我是什么"。
//
// 这段每步都在表里，不能长；真正的目录在系统提示里。
const toolLoadDescription = "取工具：表里没有的工具，先用它拿完整参数 schema，下一步就能直接调用。" +
	"参数给工具名（names 数组或 name 字符串，一次 1~8 个）。名字见系统提示里的目录。"

// lazyRunner 是 lazy 模式的执行入口：接住 tool_load，并挡回没装载的表外调用。
//
// 位置在守卫**外层**（和 mergedRunner 一样）：tool_load 自己不需要审批，
// 而被挡回的调用根本不该走到审批（它没有真实执行）。
type lazyRunner struct {
	inner spi.Runner
	table *lazyTable
}

func (l *lazyRunner) Run(ctx context.Context, name string, args json.RawMessage) (spi.Result, error) {
	if name == toolLoadName {
		return l.table.load(args)
	}
	if l.table.isHidden(name) {
		hint := fmt.Sprintf("%s 不在工具表里（lazy 模式只常驻 %d 个）。先 tool_load "+
			`{"names":[%q]} 拿到 schema，下一步再调用。`,
			name, len(l.table.hot), name)
		return spi.Result{Hint: hint}, fmt.Errorf("工具 %s 没有装载", name)
	}
	return l.inner.Run(ctx, name, args)
}

// projectTable 把装配出来的注册表投影成"模型看到的表"。
//
// cmdRun 与 cmd tools 必须走同一条链——历史上 serve/run/acp 三条路各投影一次，
// 结果漂过（表不一样、字节不一样）。顺序也是有讲究的：
//
//	merged 先：它要读成员的**原始** schema（顶层 type 判断、required 抄写）；
//	tighten 后：先收紧会让 merged 把"顶层不是 object"误判成合不进来；
//	lazy 最后：它拿到的必须是 tight 后的工具（装载时原样发出去）。
func projectTable(mode string, reg spi.ToolRegistry) (view spi.ToolRegistry, mt *mergedTable, lt *lazyTable, warns []string, err error) {
	view = reg
	switch mode {
	case toolsModeMerged:
		m, w, e := newMergedTable(reg)
		if e != nil {
			return nil, nil, nil, nil, e
		}
		mt, view = m, m
		warns = append(warns, w...)
	case toolsModeLazy:
		tight := tightenTable(reg)
		l, w, e := newLazyTable(tight, defaultHotTools)
		if e != nil {
			return nil, nil, nil, nil, e
		}
		lt, view = l, l
		warns = append(warns, w...)
	}
	if mode != toolsModeFull && mode != toolsModeLazy {
		view = tightenTable(view)
	}
	return view, mt, lt, warns, nil
}
