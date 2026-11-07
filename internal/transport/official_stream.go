package transport

// 官方桌面端 0.2.2-rc.1 的**流式**方法（typert `mode:'stream'`）的真实现。
//
// # 这份文件解决什么
//
// 官方清单里有 6 个 `mode:'stream'` 方法（`session.events` **不在其中**：
// 官方它是 `Promise<SessionEventsPage>`，一次性读，本仓早已是真实现）：
//
//	session.follow              typert.host.js:2019  signature L2866
//	session.observeEvent        typert.host.js:2184  signature L2831
//	session.observeProjections  typert.host.js:2055  signature L2838
//	session.observeState        typert.host.js:2252  signature L2852
//	workspaceFiles.changes      workspace-files/typert.host.js:160
//	toolResults.watch           dsh-api-remotes/lib/client.js:17415
//
// 以前这 6 个一律回 501（说"真流在 /ws"）。501 本身不是假流，但**也不是实现**：
// 官方客户端拿到 501 就无法订阅。这里把 6 个都做成真实现：
//
//   - **帧机**（本文件）：拉模式的 `Next(ctx)`，一次一帧，帧形状逐字段照官方 zod。
//   - **两条传输**（official_stream_face.go）：HTTP NDJSON 长连接 + mux 逻辑流。
//     两条面**共用同一个帧机**，所以不存在"HTTP 上对了、mux 上错一格"。
//   - **数据来源**（official_stream_sources.go）：`workspaceFiles.changes` 走
//     `OfficialWorkspaceAPI.FilesStat` 的真实版本令牌；`toolResults.watch` 走
//     `OfficialAgentAPI.ToolResultsWatch` 的真实 durable `tool/result` 读取。
//     没有来源时**明确未实现**（点名缺哪个能力），不返回假 ready 帧。
//
// # seq 语义（这是最容易做错的一处，固定下来）
//
// 官方 follow 的游标是 **durable v4 事件 seq**，不是 Transport journal seq、
// 不是连接序号、不是页内下标。理由：同一个客户端会把 follow 拿到的 cursor
// 交给 `session.events(beforeSeq)`、`session.wait(afterSeq)`、
// `toolResults.watch(seq)`——三个方法都在 v4 序域里。用 journal seq 顶上会让
// 这些跨方法游标静默错位。
//
// 语义（与官方 `afterSeq ?? -1` 的约定一致）：
//
//	sinceSeq = -1   「我什么都没拿」→ 从第 0 帧起（**seq 0 在这里是可达的**）
//	sinceSeq >= 0   排他下界「我已经有第 sinceSeq 帧了」→ 只要 (sinceSeq, …]
//
// 这就是那个已知坑：`Window` 的判据是 `Seq > sinceSeq`，而"从头开始"如果只能
// 写 0，第 0 帧永远取不到（0 表示"我有第 0 帧"）。本仓三条面统一用 -1 哨兵
// （`SinceNothing`，见 journal.go），WS `sub`、mux 流、官方 follow 都不例外。
//
// # 断线重连不重放历史
//
// 官方 follow 的 opening 是"一次 snapshot + 之后的实时帧"，续传靠
// `throughSeq`（**inclusive** cut）与 `beforeSeq`（**exclusive** 上界）表达：
//
//	{throughSeq: N, beforeSeq: 0}  →  cursor=N、records=[]、hasMore=false
//
// 也就是"把切点钉在 N，别再给我历史"。本文件额外接受 `sinceSeq`（additive，
// 语义 = `throughSeq: sinceSeq, beforeSeq: 0`），因为 mux 与 WS 面本来就用
// 这个名字，两套名字并存会让人以为有两套语义。三个字段同时出现且矛盾 → 400。
//
// # 背压有界
//
// 帧机是**拉模式**：调用方不取下一帧，宿主就不产下一帧。所以慢客户端只会
// 卡住自己那条流（mux 侧还有 256 格发送队列 + 256 KiB 上行配额），不会
// 让宿主的 journal 或 v4 日志无界增长。等待用 journal 广播（`Journal.Wait`）
// 唤醒，不是忙等；ticker 只是**兜底**（数据源不在本进程 journal 上时也能醒）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// officialStreamPollInterval 是流在没有 journal 广播可等时的兜底轮询间隔。
//
// 为什么仍然需要它：follow/observe* 的数据源是 durable v4 日志，而**不是**
// 本进程的 Transport journal（见文件头 seq 语义）。生产写路径会把每条 v4 事件
// 同时追加进 journal，所以正常情况下 `Journal.Wait` 立刻唤醒；但"另一个进程
// 写了日志""宿主没接 journal"这两类情况下只剩 ticker。
//
// 100ms 与 session.wait 的采样间隔同档：界面上看不出来，又不会把一次订阅
// 变成忙等。它是变量（不是 const）只为了让测试把等待压到毫秒级。
var officialStreamPollInterval = 100 * time.Millisecond

// officialStreamPageLimit 是 mux 侧一次 Window 最多取多少帧。
//
// 与 cmd/dshd 的 officialWireStreamPage 同值：这是"一次补洞窗口"的上限，
// 不是总量上限（下一轮会继续取）。
const officialStreamPageLimit = 256

// OfficialSessionHeader 是官方 follow snapshot 的 header
// （typert.host.js 的 `SessionFollowFrame.snapshot.header`）。
//
// 字段与 v4 日志首帧**一一对应**（codecs/v4.Header）：官方 header 就是
// durable 会话头的线上表示，不是另一套投影。必填是 version/id/createdAt/isSeeded，
// 其余省略表示"宿主不知道"。
type OfficialSessionHeader struct {
	Version         int             `json:"version"`
	ID              string          `json:"id"`
	CreatedAt       int64           `json:"createdAt"`
	CWD             string          `json:"cwd,omitempty"`
	ParentSession   string          `json:"parentSession,omitempty"`
	IsSeeded        bool            `json:"isSeeded"`
	Origin          json.RawMessage `json:"origin,omitempty"`
	DelegationDepth *int            `json:"delegationDepth,omitempty"`
	AgentPreset     string          `json:"agentPreset,omitempty"`
}

// OfficialSessionHeaderSource 提供官方 follow snapshot 需要的会话头。
//
// 为什么是**可选**接口而不是扩 OfficialSessionHost：OfficialSessionHost 由
// cmd/dshd 实现（`officialSessionHost`），往该接口加方法会让它的编译当场红
// （本任务的写作用域不含 cmd）。所以按可选能力断言：宿主接了就真，没接就
// 明确回"缺这一项"，**绝不拿零值编一个 header**——`createdAt=0` 会被客户端
// 当成 1970 年，那是假数据里最难查的一类。
type OfficialSessionHeaderSource interface {
	SessionHeader(sessionID string) (OfficialSessionHeader, error)
}

// OfficialSessionTailSource 是**可选**的增量日志读入口。
//
// 没有它时 follow 的实时段每次唤醒都重读整份日志再按 seq 过滤（结果正确、
// 语法诚实，长会话上是 O(n²)）。实现了它就只读增量。
type OfficialSessionTailSource interface {
	// DurableEventsSince 返回 seq > afterSeq 的 durable 事件与日志末端 seq。
	DurableEventsSince(sessionID string, afterSeq int64) ([]DurableEvent, int64, error)
}

func officialStreamBadRequest(field, format string, a ...any) *OfficialStreamFault {
	return &OfficialStreamFault{Code: "gateway/bad-request", Status: 400, Field: field,
		Message: fmt.Sprintf(format, a...)}
}

func officialStreamNotImplemented(method Method, detail string) *OfficialStreamFault {
	return &OfficialStreamFault{Code: "gateway/method-unavailable", Status: 501,
		Message: string(method) + " 未接线：" + detail}
}

func officialStreamNotFound(method Method, detail string) *OfficialStreamFault {
	return &OfficialStreamFault{Code: "session/not-found", Status: 404,
		Message: string(method) + "：" + detail}
}

// ---------------------------------------------------------------------------
// 请求解析
// ---------------------------------------------------------------------------

// officialFollowRequest 是 `session.follow` 的参数（官方 L2019）+ 本仓 add 的 sinceSeq。
type officialFollowRequest struct {
	officialPageRequest
	// Activate 请求后台激活；本宿主没有"被动读 vs 激活"的区别（读日志不激活
	// 任何东西），所以它被接受但**不改变行为**——如实记在 RESULT 的差异里。
	Activate *bool `json:"activate"`
	// AssistantStream 请求 process-local 助手呈现帧。本宿主没有那类帧
	// （助手正文走 `assistant/message` durable 事件），接受但不产生额外帧。
	AssistantStream *bool `json:"assistantStream"`
	// SinceSeq 是本仓 additive 的续传游标（排他；-1 = 什么都没拿）。
	SinceSeq *int64 `json:"sinceSeq"`
}

// officialStreamRequest 是两条面共用的"已经解析并校验过"的启动参数。
type officialStreamRequest struct {
	Method Method
	// SessionID 是这一条流绑定的会话（A/B 隔离靠它，不靠"当前会话"）。
	SessionID string
	// Follow 只在 session.follow 上非 nil。
	Follow *officialFollowRequest
	// Path 只在 workspaceFiles.changes 上使用（工作区相对路径，空 = 根）。
	Path string
	// ToolSeq 只在 toolResults.watch 上使用（durable 工具结果事件 seq）。
	ToolSeq int64
}

// parseOfficialStreamRequest 把两种面的 args（mux 的 open args / HTTP 的 body）
// 解析成同一份请求。**未知字段一律拒绝**（官方 strict schema 是
// additionalProperties:false；容忍拼错的键会把"客户端传错了"变成"订阅成功"）。
func parseOfficialStreamRequest(method Method, raw json.RawMessage) (officialStreamRequest, *OfficialStreamFault) {
	if len(raw) == 0 {
		return officialStreamRequest{}, officialStreamBadRequest("", "%s 需要参数对象", method)
	}
	trimmed := raw
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\n' || trimmed[0] == '\t' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return officialStreamRequest{}, officialStreamBadRequest("", "%s 的参数必须是 JSON 对象", method)
	}

	switch method {
	case MethodSessionFollow:
		var req officialFollowRequest
		if fault := decodeOfficialStreamArgs(method, raw, &req); fault != nil {
			return officialStreamRequest{}, fault
		}
		return officialStreamRequest{Method: method, Follow: &req}, nil

	case MethodSessionObserveEvent, MethodSessionObserveProjections, MethodSessionObserveState:
		var req officialSessionRequest
		if fault := decodeOfficialStreamArgs(method, raw, &req); fault != nil {
			return officialStreamRequest{}, fault
		}
		return officialStreamRequest{Method: method, SessionID: req.SessionID}, nil

	case MethodWorkspaceFilesChanges:
		var req struct {
			WorkspaceFileScopeID *string `json:"workspaceFileScopeId"`
			SessionID            *string `json:"sessionId"`
			AgentID              *string `json:"agentId"`
			Path                 *string `json:"path"`
		}
		if fault := decodeOfficialStreamArgs(method, raw, &req); fault != nil {
			return officialStreamRequest{}, fault
		}
		scope := ""
		for _, candidate := range []*string{req.WorkspaceFileScopeID, req.SessionID, req.AgentID} {
			if candidate != nil {
				scope = *candidate
				break
			}
		}
		if scope == "" {
			return officialStreamRequest{}, officialStreamBadRequest("workspaceFileScopeId",
				"%s 需要 workspaceFileScopeId（官方 wire 名；会话身份不能缺）", method)
		}
		if req.Path == nil {
			return officialStreamRequest{}, officialStreamBadRequest("path",
				"%s 需要 path（工作区内的目标路径；根目录给空串）", method)
		}
		return officialStreamRequest{Method: method, SessionID: scope, Path: *req.Path}, nil

	case MethodToolResultsWatch:
		var req struct {
			SessionID *string `json:"sessionId"`
			Seq       *int64  `json:"seq"`
		}
		if fault := decodeOfficialStreamArgs(method, raw, &req); fault != nil {
			return officialStreamRequest{}, fault
		}
		if req.SessionID == nil || *req.SessionID == "" {
			return officialStreamRequest{}, officialStreamBadRequest("sessionId",
				"%s 需要非空 sessionId（授权身份随请求走，没有「当前会话」）", method)
		}
		if req.Seq == nil {
			return officialStreamRequest{}, officialStreamBadRequest("seq",
				"%s 需要 seq（durable 工具结果事件序号）", method)
		}
		if *req.Seq < 0 {
			return officialStreamRequest{}, officialStreamBadRequest("seq",
				"%s 的 seq=%d 不能为负（它是 durable 事件序号）", method, *req.Seq)
		}
		return officialStreamRequest{Method: method, SessionID: *req.SessionID, ToolSeq: *req.Seq}, nil
	}
	return officialStreamRequest{}, officialStreamNotImplemented(method,
		"这条方法不是流式方法（IsOfficialStreamMethod 判据见 official_stream_face.go）")
}

// decodeOfficialStreamArgs 是 strict 解码（未知字段/多段 JSON 全部 fail-loud）。
func decodeOfficialStreamArgs(method Method, raw json.RawMessage, dst any) *OfficialStreamFault {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return officialStreamBadRequest("", "%s 的参数解析失败：%v", method, err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return officialStreamBadRequest("", "%s 的请求体只允许一个 JSON 对象", method)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 帧机
// ---------------------------------------------------------------------------

// officialStreamSource 是帧机用到的宿主能力（便于测试替换）。
type officialStreamSource interface {
	events(sessionID string) ([]DurableEvent, int64, error)
	eventsSince(sessionID string, afterSeq int64) ([]DurableEvent, int64, error)
	liveState(sessionID string) (OfficialLiveState, error)
	header(sessionID string) (OfficialSessionHeader, bool, error)
	wait(sessionID string) (<-chan struct{}, func())
}

// officialServerStreamSource 是**生产**实现：全部落在真实宿主能力上。
type officialServerStreamSource struct {
	server *Server
	host   OfficialSessionHost
}

func (s officialServerStreamSource) events(sessionID string) ([]DurableEvent, int64, error) {
	return s.host.DurableEvents(sessionID)
}

// eventsSince 有增量入口就只读增量，没有就重读整份日志再按 seq 过滤。
//
// 两条路径**结果相同**（seq > afterSeq 的事件，升序），区别只在读放大。
func (s officialServerStreamSource) eventsSince(sessionID string, afterSeq int64) ([]DurableEvent, int64, error) {
	if tail, ok := s.host.(OfficialSessionTailSource); ok {
		return tail.DurableEventsSince(sessionID, afterSeq)
	}
	events, logEnd, err := s.host.DurableEvents(sessionID)
	if err != nil {
		return nil, 0, err
	}
	return officialEventsAfter(events, afterSeq), logEnd, nil
}

func (s officialServerStreamSource) liveState(sessionID string) (OfficialLiveState, error) {
	return s.host.LiveState(sessionID)
}

func (s officialServerStreamSource) header(sessionID string) (OfficialSessionHeader, bool, error) {
	src, ok := s.host.(OfficialSessionHeaderSource)
	if !ok {
		return OfficialSessionHeader{}, false, nil
	}
	header, err := src.SessionHeader(sessionID)
	return header, true, err
}

func (s officialServerStreamSource) wait(sessionID string) (<-chan struct{}, func()) {
	if s.server == nil || s.server.opt.Journals == nil {
		return nil, func() {}
	}
	journal := s.server.opt.Journals.Get(sessionID)
	return journal.Wait(), func() {}
}

// officialEventsAfter 取 seq > afterSeq 的事件（afterSeq = -1 时全给）。
//
// 显式写成两条分支：这是"seq 0 取不到"那个坑的唯一正确写法，别为了省行数
// 写成 `ev.Seq > afterSeq` 再加一个 `afterSeq < 0` 的预处理——那种写法在
// 有人重构掉预处理时不会红，只会静默丢第 0 帧。
func officialEventsAfter(events []DurableEvent, afterSeq int64) []DurableEvent {
	if afterSeq < 0 {
		return events
	}
	out := make([]DurableEvent, 0, len(events))
	for _, ev := range events {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	return out
}

// officialEventFrame 是官方 `{type:"event", event}` 帧。
type officialEventFrame struct {
	Type  string       `json:"type"`
	Event DurableEvent `json:"event"`
}

// officialReadyFrame 是官方 `{type:"ready"}` 帧（observeEvent / observeProjections）。
type officialReadyFrame struct {
	Type string `json:"type"`
}

// officialProjectionChange 是官方 projection 帧里的一项。
type officialProjectionChange struct {
	Key string `json:"key"`
	Val any    `json:"val"`
}

// officialProjectionFrame 是官方 `{type:"projection", seq, changes}` 帧。
type officialProjectionFrame struct {
	Type    string                     `json:"type"`
	Seq     int64                      `json:"seq"`
	Changes []officialProjectionChange `json:"changes"`
}

// officialFollowSnapshot 是 follow 的 opening 帧。
//
// 字段顺序照官方：type / header / projections / cursor / records / hasMore。
type officialFollowSnapshot struct {
	Type        string            `json:"type"`
	Header      OfficialSessionHeader `json:"header"`
	Projections OfficialProjections   `json:"projections"`
	Cursor      int64             `json:"cursor"`
	Records     []OfficialRecord  `json:"records"`
	HasMore     bool              `json:"hasMore"`
}

// officialSessionStream 是 follow / observeEvent / observeProjections / observeState
// 四个会话流的**同一台**帧机。
//
// 四个方法只在"第一帧是什么"和"之后每帧怎么算"上不同，所以用一个类型 + 一个
// kind 分支，而不是四份几乎一样的代码（四份就会分叉，分叉就会有一个方法在
// 断线重连时重放历史）。
type officialSessionStream struct {
	server *Server
	source officialStreamSource
	kind   Method

	sessionID string

	// follow 专用：opening 参数。
	follow *officialFollowRequest
	// cursor 是"已经交付到哪一帧"（durable v4 seq）。
	cursor int64
	// opened 是 opening 帧是否已经发出（follow 的 snapshot / observe* 的 ready）。
	opened bool

	// fold 是 observeProjections 的增量投影折叠。
	fold *officialProjectionFold
	// lastState 是 observeState 上一次交付的状态（去重：只有变化才发替换帧）。
	lastState []byte
}

var _ OfficialFrameStream = (*officialSessionStream)(nil)

func (s *officialSessionStream) Close() {}

// Next 取下一帧。
func (s *officialSessionStream) Next(ctx context.Context) (json.RawMessage, bool, *OfficialStreamFault) {
	if !s.opened {
		return s.open()
	}
	for {
		// 先看有没有新事件（"读"必须发生在注册等待**之前**：注册之后的
		// 追加会关掉 channel，不会丢；反过来则会漏）。
		frame, more, fault := s.step()
		if fault != nil {
			return nil, false, fault
		}
		if more {
			return frame, false, nil
		}
		wait, _ := s.source.wait(s.sessionID)
		// 注册等待之后再查一次：这一瞬间刚追加的事件已经在它的 channel 上，
		// 但那一帧现在才可见——不复查就会睡到下一次追加。
		if frame, more, fault := s.step(); fault != nil {
			return nil, false, fault
		} else if more {
			return frame, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, true, nil
		case <-wait:
		case <-time.After(officialStreamPollInterval):
		}
	}
}

// open 发 opening 帧（四个方法各一张）。
func (s *officialSessionStream) open() (json.RawMessage, bool, *OfficialStreamFault) {
	events, logEnd, err := s.source.events(s.sessionID)
	if err != nil {
		return nil, false, officialStreamFaultOf(s.kind, err)
	}
	s.opened = true

	switch s.kind {
	case MethodSessionFollow:
		header, ok, herr := s.source.header(s.sessionID)
		if herr != nil {
			return nil, false, officialStreamFaultOf(s.kind, herr)
		}
		if !ok {
			// 没有 header 来源就不能编一个：官方 header 的 version/createdAt
			// 是**会话身份**，编出来会让客户端把 1970 当成创建时间。
			return nil, false, officialStreamNotImplemented(s.kind,
				"宿主没有提供 follow snapshot 的 header 来源（OfficialSessionHeaderSource）；"+
					"本任务不改 cmd/dshd，接线见 RESULT 的接线清单")
		}
		throughSeq, page, hasMore, fault := s.followOpening(events, logEnd)
		if fault != nil {
			return nil, false, fault
		}
		s.cursor = throughSeq
		snapshot := officialFollowSnapshot{
			Type:        "snapshot",
			Header:      header,
			Projections: s.projectionsBaseline(events, throughSeq),
			Cursor:      throughSeq,
			Records:     officialPageRecords(page),
			HasMore:     hasMore,
		}
		raw, merr := json.Marshal(snapshot)
		if merr != nil {
			return nil, false, officialStreamInternal(s.kind, "序列化 follow snapshot 失败：%v", merr)
		}
		return raw, false, nil

	case MethodSessionObserveEvent:
		// 官方 EventListener 从**订阅那一刻的日志末端**开始：ready 之后只发新事件，
		// 不重放历史（重放是 follow 的活）。
		s.cursor = logEnd
		return officialReady(s.kind)

	case MethodSessionObserveProjections:
		s.cursor = logEnd
		s.fold = newOfficialProjectionFold()
		for _, ev := range events {
			s.fold.Apply(ev)
		}
		return officialReady(s.kind)

	case MethodSessionObserveState:
		s.cursor = logEnd
		state, fault := s.state(logEnd)
		if fault != nil {
			return nil, false, fault
		}
		s.lastState = state
		return state, false, nil
	}
	return nil, false, officialStreamNotImplemented(s.kind, "未知的流方法")
}

// followOpening 算 opening 的切点、页与 hasMore（官方 paginate 的同一份实现）。
func (s *officialSessionStream) followOpening(events []DurableEvent, logEnd int64) (int64, []DurableEvent, bool, *OfficialStreamFault) {
	req := s.follow
	// throughSeq 缺省 = 观测到的日志末端（官方 `request.throughSeq ?? observation.cursor`）。
	throughSeq := logEnd
	if req.ThroughSeq != nil {
		throughSeq = *req.ThroughSeq
	}
	if throughSeq < SinceNothing {
		return 0, nil, false, officialStreamBadRequest("throughSeq",
			"session.follow 的 throughSeq 必须 >= %d", SinceNothing)
	}
	if req.SinceSeq != nil {
		if *req.SinceSeq < SinceNothing {
			return 0, nil, false, officialStreamBadRequest("sinceSeq",
				"session.follow 的 sinceSeq 必须 >= %d（-1 = 我什么都没拿）", SinceNothing)
		}
		// 与 throughSeq/beforeSeq 并存时必须自洽：sinceSeq 就是"切点 + 不重放历史"，
		// 写了两套还互相矛盾，说明客户端两边的心智模型不一样，不能猜。
		if req.ThroughSeq != nil && *req.ThroughSeq != *req.SinceSeq {
			return 0, nil, false, officialStreamBadRequest("sinceSeq",
				"session.follow 的 sinceSeq=%d 与 throughSeq=%d 矛盾（两者是同一个切点）",
				*req.SinceSeq, *req.ThroughSeq)
		}
		if req.BeforeSeq != nil && *req.BeforeSeq != 0 {
			return 0, nil, false, officialStreamBadRequest("beforeSeq",
				"session.follow 的 sinceSeq 与 beforeSeq 不能同时给（sinceSeq 已经表示不重放历史）")
		}
		throughSeq = *req.SinceSeq
	}
	if throughSeq > logEnd {
		return 0, nil, false, officialStreamBadRequest("throughSeq",
			"session.follow 的 throughSeq=%d 超过该会话日志末端 %d", throughSeq, logEnd)
	}

	// sinceSeq 给了 = 客户端已持有到切点：页必须是空的（不重放历史）。
	empty := int64(0)
	beforeSeq := req.BeforeSeq
	if req.SinceSeq != nil {
		beforeSeq = &empty
	}
	maxMessages, ferr := validateOfficialPageWindow(string(s.kind), req.officialPageRequest)
	if ferr != nil {
		return 0, nil, false, officialStreamBadRequest("", "%v", ferr)
	}
	page, hasMore := officialPaginate(events, beforeSeq, maxMessages, throughSeq, req.TurnWindow)
	return throughSeq, page, hasMore, nil
}

// step 产出一帧"增量"（opening 之后的每一帧）。
//
// more=false 表示这一刻没有新东西（调用方去等唤醒）。
func (s *officialSessionStream) step() (json.RawMessage, bool, *OfficialStreamFault) {
	switch s.kind {
	case MethodSessionFollow, MethodSessionObserveEvent:
		events, _, err := s.source.eventsSince(s.sessionID, s.cursor)
		if err != nil {
			return nil, false, officialStreamFaultOf(s.kind, err)
		}
		if len(events) == 0 {
			return nil, false, nil
		}
		ev := events[0]
		s.cursor = ev.Seq
		raw, merr := json.Marshal(officialEventFrame{Type: "event", Event: ev})
		if merr != nil {
			return nil, false, officialStreamInternal(s.kind, "序列化事件帧失败：%v", merr)
		}
		return raw, true, nil

	case MethodSessionObserveProjections:
		events, _, err := s.source.eventsSince(s.sessionID, s.cursor)
		if err != nil {
			return nil, false, officialStreamFaultOf(s.kind, err)
		}
		if len(events) == 0 {
			return nil, false, nil
		}
		ev := events[0]
		s.cursor = ev.Seq
		// 官方语义：**每个已提交事件一帧**，changes 只列这个事件改动的键
		// （可以是空数组——"这一条没改任何客户端可见键"，不是"没有能力"）。
		changes := s.fold.Apply(ev)
		raw, merr := json.Marshal(officialProjectionFrame{
			Type: "projection", Seq: ev.Seq, Changes: changes,
		})
		if merr != nil {
			return nil, false, officialStreamInternal(s.kind, "序列化投影帧失败：%v", merr)
		}
		return raw, true, nil

	case MethodSessionObserveState:
		// 官方 observeState 是"完整状态替换"：只有真的变了才发（去重靠序列化比较，
		// 不是靠猜字段）。游标跟着日志末端走。
		_, logEnd, err := s.source.eventsSince(s.sessionID, s.cursor)
		if err != nil {
			return nil, false, officialStreamFaultOf(s.kind, err)
		}
		if logEnd <= s.cursor {
			// 没有新事件也可能有 live 变化（running 翻转、挂起审批），所以不早退。
			state, fault := s.state(s.cursor)
			if fault != nil {
				return nil, false, fault
			}
			if string(state) == string(s.lastState) {
				return nil, false, nil
			}
			s.lastState = state
			return state, true, nil
		}
		s.cursor = logEnd
		state, fault := s.state(logEnd)
		if fault != nil {
			return nil, false, fault
		}
		if string(state) == string(s.lastState) {
			return nil, false, nil
		}
		s.lastState = state
		return state, true, nil
	}
	return nil, false, officialStreamNotImplemented(s.kind, "未知的流方法")
}

// state 采一次真实状态（游标 = 给定 seq）。
func (s *officialSessionStream) state(cursor int64) (json.RawMessage, *OfficialStreamFault) {
	live, err := s.source.liveState(s.sessionID)
	if err != nil {
		return nil, officialStreamFaultOf(s.kind, err)
	}
	pending := s.server.officialPendingInput(s.sessionID)
	state := OfficialState{
		Cursor:       cursor,
		Active:       live.Running || len(live.QueueIDs) > 0 || len(pending) > 0,
		Running:      live.Running,
		Queue:        nil,
		PendingInput: pending,
	}
	raw, merr := json.Marshal(state)
	if merr != nil {
		return nil, officialStreamInternal(s.kind, "序列化会话状态失败：%v", merr)
	}
	return raw, nil
}

// projectionsBaseline 是 follow snapshot 里的 projections（与 session.projections 同一份）。
func (s *officialSessionStream) projectionsBaseline(events []DurableEvent, asOfSeq int64) OfficialProjections {
	fold := newOfficialProjectionFold()
	for _, ev := range events {
		fold.Apply(ev)
	}
	return fold.Values(asOfSeq, s.server.loadProjectionExtras(s.sessionID))
}

func officialReady(method Method) (json.RawMessage, bool, *OfficialStreamFault) {
	raw, err := json.Marshal(officialReadyFrame{Type: "ready"})
	if err != nil {
		return nil, false, officialStreamInternal(method, "序列化 ready 帧失败：%v", err)
	}
	return raw, false, nil
}

func officialStreamInternal(method Method, format string, a ...any) *OfficialStreamFault {
	return &OfficialStreamFault{Code: "gateway/internal", Status: 500,
		Message: string(method) + "：" + fmt.Sprintf(format, a...)}
}

// officialStreamFaultOf 把宿主错误折叠成流故障。
func officialStreamFaultOf(method Method, err error) *OfficialStreamFault {
	if err == nil {
		return officialStreamInternal(method, "宿主返回了 nil 错误")
	}
	switch {
	case errors.Is(err, ErrNoSuchSession):
		return officialStreamNotFound(method, err.Error())
	case errors.Is(err, ErrOfficialNotImplemented), errors.Is(err, ErrSessionOfficialUnsupported),
		errors.Is(err, ErrNotImplemented):
		return &OfficialStreamFault{Code: "gateway/method-unavailable", Status: 501,
			Message: string(method) + " 未接线：" + err.Error()}
	default:
		return &OfficialStreamFault{Code: "gateway/internal", Status: 500,
			Message: string(method) + " 失败：" + err.Error()}
	}
}

// ---------------------------------------------------------------------------
// 投影折叠（session.projections 与 observeProjections 共用一份）
// ---------------------------------------------------------------------------

// officialProjectionFold 是**逐事件**的投影折叠。
//
// 为什么要逐事件：observeProjections 的每一帧要列"这个事件改了哪些键"，
// 整份重算只能得到"现在是什么"，差不出改动集。所以折叠必须是增量的，
// 而 session.projections 的 baseline 也用同一台折叠（全量 apply 一次），
// 免得两条路径各有一份"标题取哪条"的逻辑。
type officialProjectionFold struct {
	blank        bool
	lastPromptAt int64 // -1 = 还没有人类 prompt
	title        string
	hasTitle     bool
}

func newOfficialProjectionFold() *officialProjectionFold {
	return &officialProjectionFold{blank: true, lastPromptAt: -1}
}

// Apply 折叠一个事件，返回它改动的键（按官方语义：没改任何键就是空数组）。
func (f *officialProjectionFold) Apply(ev DurableEvent) []officialProjectionChange {
	changes := make([]officialProjectionChange, 0, 2)
	switch ev.Type {
	case "turn/start":
		if f.blank {
			f.blank = false
			changes = append(changes, officialProjectionChange{
				Key: "sessionListMetadata", Val: f.sessionListMetadata(),
			})
		}
	case "session/title":
		var data struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return changes
		}
		title := data.Title
		if title == "" {
			title = data.Text
		}
		if title == "" {
			return changes
		}
		f.title, f.hasTitle = title, true
		changes = append(changes, officialProjectionChange{Key: "title", Val: title})
	case "user/message":
		if !isOfficialAppendSurface(ev) {
			return changes
		}
		var data struct {
			Source struct {
				Kind string `json:"kind"`
			} `json:"source"`
		}
		if json.Unmarshal(ev.Data, &data) != nil || data.Source.Kind != "user" {
			return changes
		}
		f.lastPromptAt = ev.Time
		changes = append(changes, officialProjectionChange{
			Key: "sessionListMetadata", Val: f.sessionListMetadata(),
		})
	}
	return changes
}

// sessionListMetadata 复刻官方 applySessionListMetadata（list.js:128-134）。
func (f *officialProjectionFold) sessionListMetadata() map[string]any {
	out := map[string]any{"blank": f.blank}
	if f.lastPromptAt >= 0 {
		out["lastPromptAt"] = f.lastPromptAt
	} else {
		out["lastPromptAt"] = nil
	}
	return out
}

// officialProjectionExtras 是"事件推不出来、只能由宿主投影给"的键
// （workingDirectory / permissions 来自 load 投影）。
type officialProjectionExtras struct {
	WorkingDirectory string
	Permission       string
}

// Values 是 baseline（官方 session/projections 的 values）。
func (f *officialProjectionFold) Values(asOfSeq int64, extras officialProjectionExtras) OfficialProjections {
	values := map[string]any{"sessionListMetadata": f.sessionListMetadata()}
	if f.hasTitle {
		values["title"] = f.title
	}
	if extras.WorkingDirectory != "" {
		values["workingDirectory"] = extras.WorkingDirectory
	}
	if extras.Permission != "" {
		values["permissions"] = map[string]any{"currentValue": extras.Permission}
	}
	return OfficialProjections{AsOfSeq: asOfSeq, Values: values}
}

// loadProjectionExtras 取 load 投影里那两个真值（没有就给零值 = 整段不画）。
func (s *Server) loadProjectionExtras(sessionID string) officialProjectionExtras {
	if s.opt.Load == nil {
		return officialProjectionExtras{}
	}
	res, err := s.opt.Load(sessionID, 1)
	if err != nil {
		return officialProjectionExtras{}
	}
	return officialProjectionExtras{WorkingDirectory: res.CWD, Permission: res.Permission}
}
