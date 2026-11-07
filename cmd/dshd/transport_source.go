package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"dshgo/codecs/v4"
	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// sessionSource 是 transport.Source 的宿主侧实现：它才被允许读 v4 日志。
//
// 为什么放在 cmd/ 而不是 internal/transport：transport 是宿主线与界面线的契约
// （红线 R7），它不 import 任何会话日志的编解码包，也不碰文件系统。
// 「读日志 → 映射成 Message」这件事只能由宿主做，所以实现落在这一侧，
// transport 那边只留接口与分页逻辑。
type sessionSource struct {
	home string // <home>/sessions 的上一级
	cwd  string // 用来定位 projectKey；空则按 id 全盘找
	// perms 是会话级审批策略覆盖（`<home>/state/session-permission.json`）。
	//
	// **可以 nil**：不带权限覆盖的场景（老测试、别的入口）照样能 work，
	// 只是 `session.load` 回空串——那正是"不知道"的表示。
	perms *permissionOverrideStore
	// goals 是会话级目标（`<home>/state/session-goal.json`）。同样可以 nil。
	goals *goalOverrideStore
	// efforts 是独立的会话 effort key 覆盖；与上面的 v4 实际 effort 不混用。
	efforts *effortOverrideStore
	// catalog 与生产 serve/runner 共享同一 env fallback，不用新开的空 store 判自定义 level。
	catalog *llm.ModelCatalogStore
}

func newSessionSource(home, cwd string) *sessionSource {
	return &sessionSource{home: home, cwd: cwd}
}

// Messages 实现 transport.Source。
//
// 映射只做三件事（表见任务）：user/message → user、assistant/message → assistant、
// tool/result → tool，其余事件一律跳过。`tool/call` 虽然不是消息，但它带着工具名与参数，
// 用来给紧随其后的 tool/result 补上 Tool/Args（补不了就留空，不编）。
// MessagesSince 只投影 seq > sinceSeq 的事件。
//
// 先过滤再投影，而不是先投影再过滤：投影要解 JSON、拼 text、查 tool 索引，
// 在 24k 事件的会话上那是 400 ms 级的活，而界面补洞时通常只要最后几条。
func (s *sessionSource) MessagesSince(sessionID string, sinceSeq int) ([]transport.Message, error) {
	path, err := s.resolve(sessionID)
	if err != nil {
		return nil, err
	}
	log, err := v4.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if sinceSeq > 0 {
		kept := log.Events[:0]
		for _, ev := range log.Events {
			if ev.Seq > int64(sinceSeq) {
				kept = append(kept, ev)
			}
		}
		log.Events = kept
	}
	return s.messagesOf(log.Events)
}

// MessagesTail 只投影最后 limit 条消息，同时把总数算出来。
//
// 为什么要单独一个：`session.load` 是界面点开会话时的第一条 method，
// 它只要首屏 50 条，但现在走的是"先投影全部再截尾"——24k 事件的会话要 480 ms。
// 计数只需要认事件类型，投影才要解 JSON、拼正文，两者代价差一个数量级。
func (s *sessionSource) MessagesTail(sessionID string, limit int) ([]transport.Message, int, error) {
	path, err := s.resolve(sessionID)
	if err != nil {
		return nil, 0, err
	}
	st, serr := os.Stat(path)
	if serr != nil {
		return nil, 0, serr
	}

	// 大会话走尾部快路：只解最后若干帧，total 报 -1（契约允许，见 transport-obligations）。
	//
	// 为什么不是"后台补 total"：那要起 goroutine，而并发点必须登记并想清同步方式
	// （race-guard 会拦）。为了状态栏一行字付这个复杂度不划算——先把首屏做到毫秒级，
	// total 留成明确的"未知"，比一个永远不来的"稍后"诚实。
	if st.Size() > tailThresholdBytes {
		evs, rerr := v4.ReadTail(path, tailFrames)
		if rerr != nil {
			return nil, 0, rerr
		}
		msgs, merr := s.messagesOf(evs)
		if merr != nil {
			return nil, 0, merr
		}
		if limit > 0 && len(msgs) > limit {
			msgs = msgs[len(msgs)-limit:]
		}
		return msgs, -1, nil
	}

	return s.messagesTailFull(path, limit)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// UsageTotal 扫全会话的 assistant/message，算出累计用量与最近一次上下文占用（H1）。
//
// 代价是一次全量读——与 load 本身同量级，所以只在 LoadResult 里用。
func (s *sessionSource) UsageTotal(sessionID string) (transport.UsageMsg, int, error) {
	path, err := s.resolve(sessionID)
	if err != nil {
		return transport.UsageMsg{}, 0, err
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		return transport.UsageMsg{}, 0, err
	}
	var total transport.UsageMsg
	ctx := 0
	for _, ev := range lg.Events {
		if ev.Type != "assistant/message" {
			continue
		}
		var meta struct {
			Usage *struct {
				InputTokens     int `json:"inputTokens"`
				OutputTokens    int `json:"outputTokens"`
				TotalTokens     int `json:"totalTokens"`
				CacheReadTokens int `json:"cacheReadTokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(ev.Data, &meta) != nil || meta.Usage == nil {
			continue
		}
		total.InputTokens += meta.Usage.InputTokens
		total.OutputTokens += meta.Usage.OutputTokens
		total.CacheReadTokens += meta.Usage.CacheReadTokens
		// 上下文占用取**最后一条**的 totalTokens，不是累加。
		if meta.Usage.TotalTokens > 0 {
			ctx = meta.Usage.TotalTokens
		}
	}
	total.TotalTokens = total.InputTokens + total.OutputTokens
	return total, ctx, nil
}

// Model 从最近一条 request/context 事件取模型 id（H4）。
//
// 为什么是 request/context：真实语料里 model 只出现在这种事件上（实测 3 个会话命中，
// 其它事件类型都没有）。从后往前找，取第一条有的——那次才是"现在在用什么模型"。
func (s *sessionSource) Model(sessionID string) (string, error) {
	path, err := s.resolve(sessionID)
	if err != nil {
		return "", err
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		return "", err
	}
	for i := len(lg.Events) - 1; i >= 0; i-- {
		if lg.Events[i].Type != "request/context" {
			continue
		}
		var d struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(lg.Events[i].Data, &d) == nil && d.Model != "" {
			return d.Model, nil
		}
	}
	return "", nil
}

// Effort 报这条会话最新配置记录里的推理强度；空串 = 不知道。
//
// v4 header 的严格白名单没有 effort，request/context 也只有路由与容量。
// 真值只在 request/header.header.config.reasoningEffort（完整配置快照）或
// model/selection.reasoningEffort（显式选择）里。两类记录按日志顺序取最新一条：
// 最新记录缺键、空值或未知枚举都表示当前值未知，绝不回退到更早记录，也不拿
// 本进程 --reasoning、模型名或目录里的 reasoningEfforts 冒充会话实际配置。
func (s *sessionSource) Effort(sessionID string) (string, error) {
	path, err := s.resolve(sessionID)
	if err != nil {
		return "", err
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		return "", err
	}
	for i := len(lg.Events) - 1; i >= 0; i-- {
		ev := lg.Events[i]
		switch ev.Type {
		case "request/header":
			var d struct {
				Header struct {
					Config json.RawMessage `json:"config"`
				} `json:"header"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				return "", nil
			}
			return s.effortFromRecord(d.Header.Config), nil
		case "model/selection":
			return s.effortFromRecord(ev.Data), nil
		}
	}
	return "", nil
}

// effortFromRecord 保留旧枚举兼容，同时允许当前 catalog 真实声明的自定义 key。
// 未知值仍回空串，不把任意历史字符串当成可选择的 level。
func (s *sessionSource) effortFromRecord(raw json.RawMessage) string {
	if effort := knownEffortField(raw); effort != "" {
		return effort
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	var effort, model string
	if value, exists := obj["reasoningEffort"]; !exists || json.Unmarshal(value, &effort) != nil || effort == "" {
		return ""
	}
	if value, exists := obj["model"]; exists {
		_ = json.Unmarshal(value, &model)
	}
	if model == "" {
		return ""
	}
	_, declared, err := catalogEffortWire(s.home, model, effort, s.catalog)
	if err == nil && declared {
		return effort
	}
	return ""
}

// knownEffortField 只认对象里精确小写的已知枚举。off 是显式记录的真值；
// 缺键、null、空串、非字符串和未来自定义枚举都回空串（未知），不是 off。
func knownEffortField(raw json.RawMessage) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	value, exists := obj["reasoningEffort"]
	if !exists {
		return ""
	}
	var effort string
	if json.Unmarshal(value, &effort) != nil {
		return ""
	}
	switch effort {
	case "off", "low", "high", "xhigh", "max":
		return effort
	default:
		return ""
	}
}

// EffortSelection 只读独立覆盖 store；无覆盖与读取失败不会被进程默认值冒充。
func (s *sessionSource) EffortSelection(sessionID string) (string, error) {
	if s.efforts == nil {
		return "", nil
	}
	return s.efforts.Selection(sessionID)
}

// Goal 报这条会话当前在做的事（没设过就回空串 = 没有目标）。
//
// **不是从 v4 读**：goal 是界面状态（用户/agent 当场设的），
// 不是会话内容。从日志里读会得出"上一次设过什么"，那是历史不是意图。
func (s *sessionSource) Goal(sessionID string) (string, error) {
	if s.goals == nil {
		return "", nil
	}
	snapshot, err := s.goals.Snapshot(sessionID)
	if err != nil || snapshot.Goal == nil {
		return "", err
	}
	return snapshot.Goal.Objective, nil
}

// Permission 报这条会话的审批策略覆盖（没设过就回空串 = 不知道）。
//
// **不是从 v4 读**：审批策略是界面状态（用户选的），不是会话内容。
// 从日志里读会得出"上一次实际用的是什么"，那是历史不是意图。
func (s *sessionSource) Permission(sessionID string) (string, error) {
	if s.perms == nil {
		return "", nil
	}
	return s.perms.Get(sessionID), nil
}

// Cwd 报这条会话的工作目录（v4 日志头部的 cwd）。
//
// 为什么界面非要这个数：`workspace.list` 只回 `{name,isDir,size}`（没有 path），
// 界面要读一个文件就得拼「工作区根 + name」，而它不知道根在哪——
// `fs.read` 于是永远调不出来（sidebar-files 卡死在这里）。
// 读日志头而不是进程 cwd：多会话可能属于不同工作区，进程 cwd 只是"当前这个"。
func (s *sessionSource) Cwd(sessionID string) (string, error) {
	path, err := s.resolve(sessionID)
	if err != nil {
		return "", err
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		return "", err
	}
	return lg.Header.Cwd, nil
}

// ContextWindow 报这条会话模型的上限（token）。
//
// 从哪来：环境变量 DSHD_CONTEXT_WINDOW。**没有就返回 0**——界面据此不画进度条。
// 不做"按模型名查表猜一个数"：猜错的话百分比是假精确，比不给更糟。
func (s *sessionSource) ContextWindow(string) int {
	if v := os.Getenv("DSHD_CONTEXT_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func (s *sessionSource) Messages(sessionID string) ([]transport.Message, error) {
	path, err := s.resolve(sessionID)
	if err != nil {
		return nil, err
	}
	// 读日志出错原样返回：调用方要看得懂是权限、损坏还是尾部半帧，
	// 这里包一层反而会把 v4 已经写好的诊断盖掉。
	log, err := v4.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return s.messagesOf(log.Events)
}

// messagesOf 把一份（可能已被过滤过的）日志投影成消息。
// 大会话走尾部快路的阈值：超过 4 MB 或末尾 200 帧时，不再为了 total 全量解压。
const (
	tailThresholdBytes = 4 << 20
	tailFrames         = 200
)

// messagesTailFull 是小会话的路：全量投影，total 精确。
func (s *sessionSource) messagesTailFull(path string, limit int) ([]transport.Message, int, error) {
	log, err := v4.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	msgs, merr := s.messagesOf(log.Events)
	if merr != nil {
		return nil, 0, merr
	}
	total := len(msgs)
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs, total, nil
}

func (s *sessionSource) messagesOf(events []v4.Event) ([]transport.Message, error) {
	calls := indexToolCalls(events)

	// 思考块不在 llm.Message（也不在 transport.Message 的 Text）里，要另解事件。
	// 复用 serve_turn.go 的 reasoningBySeq——"哪条 assistant 带思考"只许这一份判据。
	// 它只读 Events，这里包一份临时日志即可；**延迟到第一条正文为空的 assistant
	// 再算**：绝大多数消息有正文，不必为它们多解一遍整批 JSON。
	var reasoning map[int64]string
	reasoningOf := func(seq int64) string {
		if reasoning == nil {
			reasoning = reasoningBySeq(&v4.Log{Events: events})
		}
		return reasoning[seq]
	}

	out := make([]transport.Message, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
		case "user/message":
			blocks, ok := contentBlocks(ev.Data, false)
			if !ok {
				continue
			}
			text := joinText(blocks)
			out = append(out, transport.Message{
				Seq: int(ev.Seq), Role: "user", Text: text, Time: ev.Time,
				V4Seq: v4SeqPtr(ev.Seq), MessageID: persistentMessageID(ev),
				Content: durableUserContent(blocks),
			})

		case "assistant/message":
			blocks, ok := contentBlocks(ev.Data, true)
			if !ok {
				continue
			}
			text := joinText(blocks)
			// 思考块只在正文为空时才需要：有正文就一定有内容（assistantHasPayload），
			// 也只有那时才会把 reasoning 放进投影。这里因此不必读它——省掉一次全批解 JSON。
			think := ""
			if strings.TrimSpace(text) == "" {
				think = reasoningOf(ev.Seq)
			}
			// 判据只有一份（assistantHasPayload）：有正文、有思考、有工具调用，
			// 占一样就不算空。旧实现只要求"JSON 解得开"，于是真正空的消息会变成
			// 一条空气泡；而只有思考的回答同样变成空消息——两种静默失败长得一样。
			if !assistantHasPayload(text, think, countToolCalls(blocks)) {
				continue
			}
			msg := transport.Message{
				Seq:       int(ev.Seq),
				Role:      "assistant",
				Text:      text,
				Time:      ev.Time,
				Usage:     usageOf(ev.Data),
				V4Seq:     v4SeqPtr(ev.Seq),
				MessageID: persistentMessageID(ev),
			}
			// 与 serve 推帧同形：只有正文为空时才带 reasoning。
			// 有正文时思考是草稿，不是已经给用户的回答；界面那条路径也只在正文为空时显示它。
			if strings.TrimSpace(text) == "" {
				msg.Reasoning = think
			}
			out = append(out, msg)

		case "tool/result":
			res, ok := loadToolResultOf(ev.Data)
			if !ok {
				continue
			}
			msg := transport.Message{
				Seq: int(ev.Seq), Role: "tool", Text: res.Text, Time: ev.Time,
				// TOOL-IDENTITY-LOAD（W187）：关联身份原样来自事件本身。
				// 缺键就是 nil，由 omitempty 整个省略——**不补 0**，
				// 界面会把 0 当成"第 0 轮第 0 步"去恢复一条编出来的结果。
				CallID: res.CallID,
				Turn:   res.Turn,
				Step:   res.Step,
				// TOOL-RESULT-ENVELOPE（W190）：成败与 durable 元数据/结构化结果
				// 原样透传。缺键/`false`/`null` 都由 omitempty 整个省略——
				// 界面靠缺键判断"没有"，凭空补 false/空对象就是编。
				IsError:    res.IsError,
				Meta:       res.Meta,
				Structured: res.Structured,
			}
			if c, found := calls[res.CallID]; found {
				msg.Tool = c.name
				msg.Args = c.args
			}
			// H3：工具把自己声明的 ViewSpec 落到事件里，这里原样透传给界面。
			var meta struct {
				View json.RawMessage `json:"view"`
			}
			if json.Unmarshal(ev.Data, &meta) == nil && len(meta.View) > 0 {
				msg.View = meta.View
			}
			out = append(out, msg)
		}
	}

	// 契约要求按 Seq 升序（transport.LoadSession/HistorySession 都依赖它）。
	// 日志本来就是顺序追加的，这里排序只是兜住"文件被手工拼过"的边角。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// resolve 找出会话文件的位置。
//
// 不用 session.ResolvePath 的原因：它把"没找到"和"多处命中"都合成一句话，
// 调用方拿不到 transport.ErrNoSuchSession 这个哨兵值——界面得靠它区分
// 「会话没了」和「读失败」，所以这里自己判一次，多出来的是 10 行判定。
func (s *sessionSource) resolve(sessionID string) (string, error) {
	if sessionID == "" {
		return "", transport.ErrNoSuchSession
	}
	// 界面从列表拿到的是 "session-<uuid>"，磁盘上是裸 uuid；两种都得认，
	// 否则界面把 id 原样传回来就是 404，而那个 404 看起来像"会话不存在"。
	var segs []string
	for _, id := range session.CandidateIDs(sessionID) {
		seg, err := v4.EncodeSegment(id)
		if err != nil {
			continue
		}
		segs = append(segs, seg)
	}
	if len(segs) == 0 {
		// 非法 id（空、带路径分隔符之类）等于没有这个会话，不是读失败。
		return "", transport.ErrNoSuchSession
	}

	// 出问题时按行看：`DSHD_DEBUG_RESOLVE=1 dshd serve ...`。
	// 为什么要有：界面报"列表能拿到会话、点进去 404"时，光看 404 分不清是哪一步——
	// home 不对、seg 不对、还是 glob 没命中。这三件事长得一模一样。
	dbg := func(step string, extra ...any) {
		if os.Getenv("DSHD_DEBUG_RESOLVE") == "" {
			return
		}
		fmt.Fprintf(os.Stderr, "resolve[%s] home=%q id=%q segs=%v cwd=%q %v\n",
			step, s.home, sessionID, segs, s.cwd, extra)
	}
	dbg("start")

	var matches []string
	for _, seg := range segs {
		if s.cwd != "" {
			direct := filepath.Join(s.home, "sessions", v4.ProjectKey(s.cwd), seg, "session.v4.jsonl.zstd")
			if _, statErr := os.Stat(direct); statErr == nil {
				return direct, nil
			}
		}
		got, gerr := filepath.Glob(filepath.Join(s.home, "sessions", "*", seg, "session.v4.jsonl.zstd"))
		if gerr != nil {
			return "", gerr
		}
		if len(got) > 0 {
			matches = got
			break
		}
	}
	dbg("glob", "matches", matches)
	switch len(matches) {
	case 0:
		dbg("miss")
		return "", transport.ErrNoSuchSession
	case 1:
		return matches[0], nil
	default:
		// 与 session.FindByID 的取舍一致：多处命中就报错让人指定 cwd，不替人猜。
		return "", fmt.Errorf("会话 %s 在多个项目目录下都有（%d 个），请显式给 cwd", sessionID, len(matches))
	}
}

// block 是 v4 内容块的最小形状；它只有一份定义（session.ContentBlock），
// 这里只是一个别名——以前这里是**独立的第二份结构体**，v4 加字段时两边各跟一半。
type block = session.ContentBlock

// durableUserContent 只在确有附件/引用时暴露完整块，旧纯文本 load 形状不变。
func durableUserContent(blocks []block) []transport.ContentBlock {
	special := false
	for _, b := range blocks {
		if b.Type == "attachment" || b.Type == "reference" {
			special = true
			break
		}
	}
	if !special {
		return nil
	}
	return transportBlocksFromSession(blocks)
}

// textOf 取一条消息的可见正文。
//
// user/message 的 content 在 data 顶层；assistant/message 的在 data.message.content。
// 只拼 type=="text" 的块——tool-call 块不是正文，它由 tool/result 那边对应。
func textOf(data json.RawMessage, nested bool) (string, bool) {
	blocks, ok := contentBlocks(data, nested)
	if !ok {
		return "", false
	}
	return joinText(blocks), true
}

// contentBlocks 按两种落法取一条消息的内容块 —— 取块本身不在这儿判，只有一份实现在
// session.ContentBlocks；这里只把"先看哪个位置"（user 顶层优先、assistant 嵌套优先）
// 翻成那个函数的参数。
//
// 两种落法都要认（H7）：真实语料把助手内容放在 message.content，
// 但也见过放在顶层 content 的日志。**只认一种，那些会话就是一条消息都没有**，
// 而界面上"整会话空白"看起来像会话坏了。
func contentBlocks(data json.RawMessage, nested bool) ([]block, bool) {
	prefer := session.PreferTop
	if nested {
		prefer = session.PreferNested
	}
	return session.ContentBlocks(data, prefer)
}

// countToolCalls 数一条 assistant 消息里的 tool-call 块。
// 只服务 assistantHasPayload——"有没有内容"不许在别处另写一份判据。
func countToolCalls(blocks []block) int {
	n := 0
	for _, b := range blocks {
		if b.Type == "tool-call" {
			n++
		}
	}
	return n
}

// usageOf 取助手事件里的用量（H1）。
//
// 两种"没有"都要返回 nil，不能返回 &UsageMsg{}：
//   - data 里根本没有 usage 键；
//   - usage 存在但四个字段全是 0。
//
// 界面靠 nil 与 0 区分「未知」和「确实是 0」——返回空结构体会让状态栏画出
// "0 tok/s" 这种假数字，比不显示更糟。
func usageOf(data json.RawMessage) *transport.UsageMsg {
	var meta struct {
		Usage *struct {
			InputTokens     int `json:"inputTokens"`
			OutputTokens    int `json:"outputTokens"`
			TotalTokens     int `json:"totalTokens"`
			CacheReadTokens int `json:"cacheReadTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &meta); err != nil || meta.Usage == nil {
		return nil
	}
	u := meta.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 && u.CacheReadTokens == 0 {
		return nil
	}
	return &transport.UsageMsg{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		TotalTokens:     u.TotalTokens,
		CacheReadTokens: u.CacheReadTokens,
	}
}

// toolResultData 是 tool/result 事件里 load 投影要用的字段。
//
// Turn/Step 用指针：v4 的 tool/result **总是**带数值 turn/step，但老日志、
// 手写事件或非 v4 来源可能没有。缺键时必须是 nil（省略），不能落成 0——
// 0 是合法数字，界面会拿它当真实身份去恢复。
//
// IsError/Meta/Structured 是 durable 结果信封的其余部分（W190）：
// 成败、宿主元数据、工具自己的结构化结果。三者都只有**真值**才投影。
type toolResultData struct {
	Text       string
	CallID     string
	Turn       *int
	Step       *int
	IsError    bool
	Meta       json.RawMessage
	Structured json.RawMessage
}

// loadToolResultOf 取 tool/result 的正文、真实关联身份（callId/turn/step）
// 与结果信封（isError/meta/structured）。
//
// 身份只认事件自己的 `message.toolCallId` 与顶层 `turn`/`step`：
// 不从 tool/call 索引反推（那是调用方的身份，不是结果的身份），也不合成。
func loadToolResultOf(data json.RawMessage) (toolResultData, bool) {
	var d struct {
		Turn       *int            `json:"turn"`
		Step       *int            `json:"step"`
		Meta       json.RawMessage `json:"meta"`
		Structured json.RawMessage `json:"structured"`
		Message    struct {
			ToolCallID string          `json:"toolCallId"`
			Content    []block         `json:"content"`
			IsError    json.RawMessage `json:"isError"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return toolResultData{}, false
	}
	return toolResultData{
		Text:       joinText(d.Message.Content),
		CallID:     d.Message.ToolCallID,
		Turn:       d.Turn,
		Step:       d.Step,
		IsError:    isTrueJSON(d.Message.IsError),
		Meta:       rawValue(d.Meta),
		Structured: rawValue(d.Structured),
	}, true
}

// isTrueJSON 只认 durable 里**字面量** true。
//
// 为什么不用 `*bool` 字段：`isError` 不是布尔（外来日志写成 `"true"`/`1`）时，
// encoding/json 会让整个 tool/result 解不开——那条结果连同正文一起消失。
// 那是"新增一个字段反而把老日志弄丢"的回归，比少一个 isError 严重得多；
// 而界面认的本来也只有 `=== true`。
func isTrueJSON(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "true"
}

// rawValue 只在 durable 真的带了一个**值**时返回原样字节。
//
// 缺键（len==0）与显式 `null` 都返回 nil，由 omitempty 整个省略。
// 为什么连 null 也省略：界面用 `undefined` 区分"没有元数据"，
// 透传 `null` 会变成"有元数据、值是 null"，那是另一种编。
func rawValue(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	return trimmed
}

func joinText(blocks []block) string {
	out := ""
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += b.Text
	}
	return out
}

type toolCallInfo struct {
	name string
	args string
}

// indexToolCalls 建 callId → 工具名/参数的索引，给 tool/result 补 Tool/Args。
func indexToolCalls(events []v4.Event) map[string]toolCallInfo {
	idx := make(map[string]toolCallInfo)
	for _, ev := range events {
		if ev.Type != "tool/call" {
			continue
		}
		var d struct {
			CallID    string `json:"callId"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			continue
		}
		if d.CallID == "" {
			continue
		}
		idx[d.CallID] = toolCallInfo{name: d.Name, args: d.Arguments}
	}
	return idx
}

// viewOf 把工具的 ViewSpec 序列化成可落盘的 JSON（H3）。
//
// nil 时返回 nil：**不写 `"view": null`**——那会让界面收到一个"有字段但为空"的值，
// 而它该看到的是"这次没有视图声明"。
func viewOf(r *spi.Result) json.RawMessage {
	if r == nil || r.View == nil {
		return nil
	}
	b, err := json.Marshal(r.View)
	if err != nil {
		return nil
	}
	return b
}
