package transport

// 官方桌面端 0.2.2-rc.1 的 session.* 会话方法（L1 语义/字段对齐）。
//
// # 契约来源（不是猜的）
//
// 参数与结果 schema 全部照抄官方 zod 契约原件：
//
//	@deepseek-ai/dsh-api-session-controller/lib/typert.host.js
//	  'session/page'        L2316-2348（参数 L2329/L2335，结果 L2345）
//	  'session/events'      L2019-2051
//	  'session/state'       L2512-2544
//	  'session/projections' L2349-2381
//	  'session/wait'        L2577-2609
//	  'session/search'      L2447-2479
//	  'session/attachment'  L1891-1922
//	  'session/selectModel' L2480-2511
//	  'session/updateQueue' L2545-2576
//	  'session/openWorkspacePath'        L2283-2315
//	  'session/workspacePathApplications' L2610-2642
//	  'session/observeState' / 'session/observeProjections' / 'session/observeEvent' / 'session/follow'
//
// 语义取自同包的 index.js：paginate(L1195-1225)、history(L1971-1985)、
// readState(L2330-2352)、wait(L2249-2320)、assistantOutput(L1901-1925)、
// search 常量在 lib/types/types.js:3-5。
//
// # 为什么这些方法不在 server.go 的既有分派里
//
// 本文件是 W145/官方对齐的 additive 扩展面：方法名、常量、handler 全在这里，
// 由 Lead 在 server.go 的 `switch Method(name)` 里补 case（见 RESULT 的接线清单）。
// 既有 58 个方法一个都不改名、不改语义。
//
// # 流式方法与本宿主的关系（不许假装）
//
// 官方的 follow/observeEvent/observeProjections/observeState 是 typert 流
// （snapshot → entries → notification，cursor 单调）。本宿主没有 typert 流层，
// 真实流在**每会话 WS journal** 上（`/ws` + `{t:"sub"}`）。所以这四个方法在这里
// 一律回 501 并写明去哪里拿真流，**不伪造一次性快照冒充流**。
// 只有 `session.events` / `session.page` 是真·一次性读（官方这两个也是 unary）。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 官方 session 方法名。既有 58 个方法一个都不改名；这 15 个是新增面。
const (
	MethodSessionPage               Method = "session.page"
	MethodSessionState              Method = "session.state"
	MethodSessionProjections        Method = "session.projections"
	MethodSessionObserveState       Method = "session.observeState"
	MethodSessionObserveProjections Method = "session.observeProjections"
	MethodSessionObserveEvent       Method = "session.observeEvent"
	MethodSessionFollow             Method = "session.follow"
	MethodSessionEvents             Method = "session.events"
	MethodSessionUpdateQueue        Method = "session.updateQueue"
	MethodSessionWait               Method = "session.wait"
	MethodSessionSearch             Method = "session.search"
	MethodSessionAttachment         Method = "session.attachment"
	MethodSessionSelectModel        Method = "session.selectModel"
	MethodSessionOpenWorkspacePath  Method = "session.openWorkspacePath"
	MethodSessionWorkspacePathApps  Method = "session.workspacePathApplications"
)

// OfficialSessionMethods 是官方 session 对齐面，**绝不加入 canonical 25**。
func OfficialSessionMethods() []Method {
	return []Method{
		MethodSessionPage, MethodSessionState, MethodSessionProjections,
		MethodSessionObserveState, MethodSessionObserveProjections, MethodSessionObserveEvent,
		MethodSessionFollow, MethodSessionEvents, MethodSessionUpdateQueue, MethodSessionWait,
		MethodSessionSearch, MethodSessionAttachment, MethodSessionSelectModel,
		MethodSessionOpenWorkspacePath, MethodSessionWorkspacePathApps,
	}
}

// 官方常量（lib/types/types.js:3-5）。
const (
	// officialSearchResultLimit 是 session.search 的结果条数上限。
	officialSearchResultLimit = 20
	// officialSearchSnippetMaxCodePoints 是 snippet 的 Unicode code point 上限。
	officialSearchSnippetMaxCodePoints = 240
	// officialSearchQueryMaxChars 是查询本身的 UTF-16 长度上限（list.js:9）。
	officialSearchQueryMaxChars = 500
	// officialPageDefaultMessages 是 maxMessages 缺省值（index.js:2008 `?? 50`）。
	officialPageDefaultMessages = 50
	// officialWaitDefaultTimeout 是 session.wait 的缺省等待（index.js:2199）。
	officialWaitDefaultTimeout = 30 * time.Second
	// officialWaitMaxTimeout 是官方 MAX_TIMER_DELAY_MS（2^31-1 毫秒）。
	officialWaitMaxTimeout = 2147483647 * time.Millisecond
	// officialWaitPollInterval 是本地轮询真实状态的间隔。
	// 官方是靠通知唤醒的；本宿主没有 agent 通知源，只能采样——间隔要小到
	// 让人感觉不到，又不能把一次 wait 变成忙等。
	officialWaitPollInterval = 100 * time.Millisecond
)

// ErrNoSuchQueueItem 表示 itemId 不在这一会话的真实 pending 队列里。
// 与 ErrNoSuchSession 分开：前者换 itemId，后者换会话。
var ErrNoSuchQueueItem = errors.New("队列里没有这一项")

// ErrSteerUnavailable 表示这一项**存在**但当前轮已不接受 steer
// （官方 `session/steer-unavailable`：目标不是 next-turn，或者 agent 没在跑）。
//
// 与 ErrNoSuchQueueItem 分开：那一个是"没有这一项"（换 itemId），
// 这一个是"有，但现在塞不进正在跑的那一轮"（等它成为当前轮，或撤回）。
var ErrSteerUnavailable = errors.New("当前轮已不再接受 steer")

// ErrSessionOfficialUnsupported 表示这条官方方法本宿主没有真实能力；
// HTTP 侧映射成 501 + 说明，**绝不返回假 ok**。
var ErrSessionOfficialUnsupported = errors.New("本宿主没有该官方能力的真实实现")

// DurableEvent 是官方事件信封（zod: type/seq/time/data + 三个可选键）。
//
// 它是 v4 durable 事件的**原样**线上表示：seq 就是 v4 seq，不是 Transport journal seq。
// 两个序号域不能混用——journal seq 在追加时才分配，v4 seq 是落盘身份。
type DurableEvent struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
	// Ignorable 只在 true 时出现（官方 `z.literal(true).optional()`）。
	Ignorable bool `json:"ignorable,omitempty"`
	// SourceEventSeqs 是当前 surface 事件指向的更早来源；未知时整个省略。
	SourceEventSeqs []int64 `json:"sourceEventSeqs,omitempty"`
	// SurfaceOp 是 canonical 落位（"append" 或 {op:"replace",startSeq,endSeq}）。
	SurfaceOp json.RawMessage `json:"surfaceOp,omitempty"`
}

// OfficialRecord 是官方 `{type:"event", event}` 记录。
type OfficialRecord struct {
	Type  string       `json:"type"`
	Event DurableEvent `json:"event"`
}

// OfficialEventPage 是 session.page / session.events 的结果。
//
// 字段与顺序照官方 L2345/L2048：cursor / records / hasMore。
// **没有 projections**——任务书里写的 `records/hasMore/projections` 与官方 schema
// 不符；真 schema 只有这三个键（`additionalProperties:true`），本实现以 schema 为准。
type OfficialEventPage struct {
	// Cursor 是这次读的**闭区间上界**（官方 `request.throughSeq ?? observation.cursor`），
	// 也就是一个真实 v4 seq，不是页内首条的 seq。
	// 要取更旧一页：把 records[0].event.seq 当 beforeSeq 再发一次。
	Cursor  int64            `json:"cursor"`
	Records []OfficialRecord `json:"records"`
	HasMore bool             `json:"hasMore"`
}

// OfficialAddress 是官方 address（普通会话或 direct-subagent）。
type OfficialAddress struct {
	Kind            string `json:"kind"`
	SessionID       string `json:"sessionId"`
	ParentSessionID string `json:"parentSessionId"`
	ChildSessionID  string `json:"childSessionId"`
	Mode            string `json:"mode"`
}

// sessionID 返回这个 address 指向的会话 id。
//
// subagent 地址读的是 childSessionId 的日志（官方 addressId，index.js:1141）；
// 但本宿主没有 subagent 目录投影，无法证明 identity 属于 parent，
// 所以 subagent 地址在 handler 里会显式回 501（见 checkAddress）。
func (a OfficialAddress) sessionID() string {
	if a.Kind == "subagent" {
		return a.ChildSessionID
	}
	return a.SessionID
}

// OfficialQueueItem 是官方 state.queue 的一项。
type OfficialQueueItem struct {
	ID        string            `json:"id"`
	Placement string            `json:"placement"`
	RPCID     string            `json:"rpcId,omitempty"`
	Content   []json.RawMessage `json:"content"`
}

// OfficialLiveState 是宿主能如实说出的实时状态。
//
// QueueIDs 是真实 pending 队列里的 TurnID（promptService.queues + interrupting）；
// Queue 是同一条队列的**内容视图**（官方 `state.queue` 的形状：id/placement/content）。
//
// 两者不是二选一：QueueIDs 在宿主只有 TurnID 时也照样如实给出，Queue 只在
// 宿主能说出**真实待发内容**时才填（它必须来自真实 pending 项，不能由 TurnID
// 编造）。没有 Queue 就是 nil——官方对"这边没有队列视图"的既有表达就是 null。
// 见 handleSessionUpdateQueue 与 OfficialQueueHost。
type OfficialLiveState struct {
	Active   bool
	Running  bool
	QueueIDs []string
	Queue    []OfficialQueueItem
}

// OfficialQueueHost 是"能看见并改写真实待发内容"的官方宿主能力。
//
// 单独一个接口而不是塞进 OfficialSessionHost：既有宿主（测试假源、第三方
// 宿主）没有内容视图时仍能实现前者，transport 按类型断言取本接口，
// 断言不到就回 501——**不拿 TurnID 拼一份空 content 冒充内容**。
type OfficialQueueHost interface {
	// LiveQueue 返回真实 pending 项的完整内容（官方顺序：next-turn 在前）。
	LiveQueue(sessionID string) ([]OfficialQueueItem, error)
	// EditQueueItem 替换一项的真实待发内容；不在队列里回 ErrNoSuchQueueItem。
	EditQueueItem(sessionID, itemID string, content []json.RawMessage) error
	// SteerQueueItem 把一项 pending 内容注入**正在跑**的那一轮；目标不再是
	// pending 项回 ErrNoSuchQueueItem，当前轮不接受 steer 回 ErrSteerUnavailable。
	SteerQueueItem(sessionID, itemID string) error
}

// OfficialSearchItem 是官方 search 的一项。
type OfficialSearchItem struct {
	SessionID string `json:"sessionId"`
	Snippet   string `json:"snippet"`
}

// OfficialSearchResult 是 session.search 的结果。
type OfficialSearchResult struct {
	Items   []OfficialSearchItem `json:"items"`
	HasMore bool                 `json:"hasMore"`
}

// OfficialState 是官方 state（L2279/L2541）。返回快照与 wait 里的 state 同形。
type OfficialState struct {
	Cursor  int64 `json:"cursor"`
	Active  bool  `json:"active"`
	Running bool  `json:"running"`
	// Queue 是 `null | array`：官方对冷会话给 null。本宿主恒为 null，
	// 原因见 handleSessionState。
	Queue        []OfficialQueueItem `json:"queue"`
	PendingInput []string            `json:"pendingInput"`
}

// OfficialOutput 是 wait 里的 assistant 输出窗（官方 `{records, truncated}`）。
type OfficialOutput struct {
	// Records 是**新到旧**（官方 assistantOutput 从 throughSeq 往下走，index.js:1911）。
	Records   []OfficialRecord `json:"records"`
	Truncated bool             `json:"truncated"`
}

// OfficialWaitResult 是 session.wait 的结果（L2606）。
type OfficialWaitResult struct {
	Reason string         `json:"reason"`
	State  OfficialState  `json:"state"`
	Output OfficialOutput `json:"output"`
}

// OfficialProjections 是 session.projections 的非 null 分支（L2362-2380）。
type OfficialProjections struct {
	AsOfSeq int64          `json:"asOfSeq"`
	Values  map[string]any `json:"values"`
}

// OfficialAttachment 是官方附件引用（L1919 的 attachment 对象）。
// attachmentId 是**不透明存储 id**，绝不是文件系统路径或 bearer URL。
type OfficialAttachment struct {
	AttachmentID string `json:"attachmentId"`
	MediaType    string `json:"mediaType"`
	Bytes        int64  `json:"bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Name         string `json:"name,omitempty"`
}

// OfficialAttachmentResult 是 session.attachment 的结果。
type OfficialAttachmentResult struct {
	Attachment OfficialAttachment `json:"attachment"`
	Data       string             `json:"data"`
}

// OfficialModelSelection 是 selectModel 的 selected（provider/model 必有）。
type OfficialModelSelection struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// OfficialSelectModelResult 是 session.selectModel 的结果。
type OfficialSelectModelResult struct {
	Selected OfficialModelSelection `json:"selected"`
}

// OfficialQueueAck 是 updateQueue / openWorkspacePath 一类"只回确认"的结果。
type OfficialQueueAck struct {
	Accepted bool `json:"accepted"`
}

// OfficialOpened 是 openWorkspacePath 的结果（`opened` 是 literal true）。
type OfficialOpened struct {
	Opened bool `json:"opened"`
}

// OfficialSessionHost 是官方会话方法需要的真实宿主能力。
//
// 为什么是"可选接口 + 类型断言"而不是新增 Options 字段：
// Options 在 server.go 里，本轮写作用域不允许改它。宿主把能力挂在 Source 上
// （官方会话读本来就是会话数据源的活），transport 断言不到就回 501，
// 而不是拿 journal/内存缓存冒充 durable 真值。
type OfficialSessionHost interface {
	// DurableEvents 返回该会话**全部** durable v4 事件（seq 升序）与日志末端 seq。
	// 空会话返回 (nil 或空切片, -1, nil)；会话不存在必须返回 ErrNoSuchSession。
	DurableEvents(sessionID string) ([]DurableEvent, int64, error)
	// LiveState 返回实时状态；没有 live 能力时回零值 + nil（不是错误）。
	LiveState(sessionID string) (OfficialLiveState, error)
	// Search 按官方语义做真实会话正文搜索，limit 是上限（官方 20）。
	Search(query string, limit int) ([]OfficialSearchItem, bool, error)
	// RemoveQueueItem 从真实 pending 队列里撤掉一项；不在队列里回 ErrNoSuchQueueItem。
	RemoveQueueItem(sessionID, itemID string) error
}

// officialHost 取这一 Server 上的官方宿主能力；没有就是 nil（→ 501）。
func (s *Server) officialHost() OfficialSessionHost {
	if s == nil {
		return nil
	}
	h, _ := s.opt.Source.(OfficialSessionHost)
	return h
}

// officialSessionRequest 是"只要会话 id"的那批方法的公共参数。
type officialSessionRequest struct {
	SessionID string `json:"sessionId"`
}

// decodeOfficial 解官方参数：未知字段、非对象、多段 JSON 全部 fail-loud。
//
// 为什么必须 DisallowUnknownFields：官方 strict schema 是
// `additionalProperties:false`，容忍拼错的键会把"客户端传错了字段"变成
// "调用成功但什么都没改"——那是本仓最贵的一类 bug。
func decodeOfficial(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "官方参数解析失败："+err.Error()))
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体只允许一个 JSON 对象"))
		return false
	}
	return true
}

// requireOfficialHost 在没接线时统一回 501，并把"是宿主没能力"说清楚。
func (s *Server) requireOfficialHost(w http.ResponseWriter, method string) (OfficialSessionHost, bool) {
	h := s.officialHost()
	if h == nil {
		writeJSON(w, http.StatusNotImplemented,
			errorBodyFor("not_implemented", method+" 未接线：宿主没有提供官方会话数据源（OfficialSessionHost）"))
		return nil, false
	}
	return h, true
}

// writeOfficialError 把宿主错误翻成四字段信封。
func writeSessionOfficialError(w http.ResponseWriter, err error, prefix string) {
	switch {
	case errors.Is(err, ErrNoSuchSession):
		writeJSON(w, http.StatusNotFound, errorBodyFor("no_such_session", "会话不存在"))
	case errors.Is(err, ErrNoSuchQueueItem):
		writeJSON(w, http.StatusNotFound, errorBodyFor("not_found", prefix+err.Error()))
	case errors.Is(err, ErrSteerUnavailable):
		// 官方 session/steer-unavailable：项还在，只是当前轮不再接受注入。
		writeJSON(w, http.StatusConflict, errorBodyFor("conflict", prefix+err.Error()))
	case errors.Is(err, ErrSessionOfficialUnsupported), errors.Is(err, ErrNotImplemented):
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", prefix+err.Error()))
	case errors.Is(err, ErrInvalidCursor):
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", prefix+err.Error()))
	case errors.Is(err, ErrPlacementInvalid), errors.Is(err, ErrPlacementUnsupported):
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", prefix+err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", prefix+err.Error()))
	}
}

// checkOfficialAddress 校验 address 并返回会话 id。
//
// 只接受 kind=session：subagent 地址要求证明"这个 child 属于这个 parent"
// （官方是 sessionProjections["subagent"] + isOwnSeq），本宿主没有那套投影，
// 认了就等于替调用方担保一条它证明不了的归属关系。
func checkOfficialAddress(w http.ResponseWriter, method string, addr OfficialAddress) (string, bool) {
	switch addr.Kind {
	case "session":
		if strings.TrimSpace(addr.SessionID) == "" {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", method+" 的 address.sessionId 不能为空"))
			return "", false
		}
		return addr.SessionID, true
	case "subagent":
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
			method+" 的 subagent 地址未实现：本宿主没有 subagent 目录投影，无法证明 child 属于该 parent"))
		return "", false
	case "":
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", method+" 缺 address.kind"))
		return "", false
	default:
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", method+" 的 address.kind 只能是 session/subagent"))
		return "", false
	}
}

// officialTurnWindow 是官方的 turnWindow（L2329）。
type officialTurnWindow struct {
	MinMessages int64 `json:"minMessages"`
	MinTurns    int64 `json:"minTurns"`
}

// officialPageRequest 是 page/events 的参数（官方 L2329）。
type officialPageRequest struct {
	Address     OfficialAddress     `json:"address"`
	ThroughSeq  *int64              `json:"throughSeq"`
	BeforeSeq   *int64              `json:"beforeSeq"`
	MaxMessages *int64              `json:"maxMessages"`
	TurnWindow  *officialTurnWindow `json:"turnWindow"`
}

// isOfficialMessageType 是官方 MESSAGE_TYPES（user/message + assistant/message）。
func isOfficialMessageType(t string) bool {
	return t == "user/message" || t == "assistant/message"
}

// isOfficialAppendSurface 判断事件是否在 canonical append surface 上。
//
// 官方 isAppendSurfaceEvent 的判据是 surfaceOp 缺省/append；
// 替换（replace）事件不进分页计数，否则一页会重复算被替换掉的内容。
func isOfficialAppendSurface(ev DurableEvent) bool {
	raw := bytes.TrimSpace(ev.SurfaceOp)
	if len(raw) == 0 {
		return true
	}
	var kind string
	if raw[0] == '"' && json.Unmarshal(raw, &kind) == nil {
		return kind == "append"
	}
	var obj struct {
		Op string `json:"op"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Op == "append" || obj.Op == ""
	}
	return false
}

// officialPaginate 是官方 paginate（index.js:1195-1225）的逐行移植。
//
// 语义（顺序都不能换）：
//  1. end = min(throughSeq+1, beforeSeq ?? throughSeq+1)：页是**闭区间上界**切出来的；
//  2. 从 end-1 往前数 **append-surface 的 user/assistant 消息**，数满 maxMessages
//     就把 cut 落在这一组的 groupStart（含 sourceEventSeqs 里更早的来源）；
//  3. turnWindow 命中 turn/start 且两个最小值都满足时，cut 落在那个 turn/start；
//  4. 页 = events[cut:end]（页内 oldest-first），hasMore = cut > 0。
//
// events 必须是**从 seq 0 开始连续**的前缀，这样下标就是 seq。
func officialPaginate(events []DurableEvent, beforeSeq *int64, maxMessages, throughSeq int64, window *officialTurnWindow) ([]DurableEvent, bool) {
	// 官方：end = min(throughSeq+1, beforeSeq ?? throughSeq+1)。beforeSeq 是**排他**上界。
	end := throughSeq + 1
	if beforeSeq != nil && *beforeSeq < end {
		end = *beforeSeq
	}
	if end > int64(len(events)) {
		end = int64(len(events))
	}
	if end < 0 {
		end = 0
	}
	count := int64(0)
	turns := int64(0)
	cut := int64(0)
	for i := end - 1; i >= 0; i-- {
		ev := events[i]
		if window != nil && ev.Type == "turn/start" {
			turns++
			if count >= window.MinMessages && turns >= window.MinTurns {
				cut = i
				break
			}
		}
		if !isOfficialMessageType(ev.Type) || !isOfficialAppendSurface(ev) {
			continue
		}
		count++
		groupStart := ev.Seq
		for _, source := range ev.SourceEventSeqs {
			if source < groupStart {
				groupStart = source
			}
		}
		if count >= maxMessages {
			cut = groupStart
			break
		}
	}
	if cut > int64(len(events)) {
		cut = int64(len(events))
	}
	if cut < 0 {
		cut = 0
	}
	page := make([]DurableEvent, 0, end-cut)
	for _, ev := range events[cut:end] {
		page = append(page, ev)
	}
	return page, cut > 0
}

// officialPageRecords 把事件页包成官方 records（不做二次裁剪）。
func officialPageRecords(events []DurableEvent) []OfficialRecord {
	records := make([]OfficialRecord, 0, len(events))
	for _, ev := range events {
		records = append(records, OfficialRecord{Type: "event", Event: ev})
	}
	return records
}

// validateOfficialPageWindow 复刻官方 validateHistoryWindow（index.js:1127-1137）。
func validateOfficialPageWindow(method string, req officialPageRequest) (int64, error) {
	maxMessages := int64(officialPageDefaultMessages)
	if req.MaxMessages != nil {
		if *req.MaxMessages <= 0 {
			return 0, fmt.Errorf("%s 的 maxMessages 必须是正整数", method)
		}
		maxMessages = *req.MaxMessages
	}
	if req.ThroughSeq != nil && *req.ThroughSeq < -1 {
		return 0, fmt.Errorf("%s 的 throughSeq 必须 >= -1", method)
	}
	if req.BeforeSeq != nil && *req.BeforeSeq < 0 {
		return 0, fmt.Errorf("%s 的 beforeSeq 必须 >= 0", method)
	}
	if w := req.TurnWindow; w != nil {
		if w.MinMessages <= 0 || w.MinMessages > maxMessages {
			return 0, fmt.Errorf("%s 的 turnWindow.minMessages 必须为正且不超过 maxMessages", method)
		}
		if w.MinTurns <= 0 {
			return 0, fmt.Errorf("%s 的 turnWindow.minTurns 必须是正整数", method)
		}
	}
	return maxMessages, nil
}

// serveOfficialEventPage 是 page 与 events 的共用实现。
//
// 官方 page 是 events 的 deprecated 别名（index.js:4085-4110，两者都调
// historyController.events），所以这里就是同一个函数——不写两份会分叉的逻辑。
func (s *Server) serveOfficialEventPage(w http.ResponseWriter, r *http.Request, method string) {
	host, ok := s.requireOfficialHost(w, method)
	if !ok {
		return
	}
	var req officialPageRequest
	if !decodeOfficial(w, r, &req) {
		return
	}
	sessionID, ok := checkOfficialAddress(w, method, req.Address)
	if !ok {
		return
	}
	maxMessages, err := validateOfficialPageWindow(method, req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	events, logEnd, err := host.DurableEvents(sessionID)
	if err != nil {
		writeSessionOfficialError(w, err, method+" 失败：")
		return
	}
	// cursor 是**这次读的上界**：给了 throughSeq 就用它，否则是日志末端。
	cursor := logEnd
	if req.ThroughSeq != nil {
		cursor = *req.ThroughSeq
	}
	if cursor > logEnd {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
			fmt.Sprintf("%s 的 throughSeq=%d 超过该会话日志末端 %d", method, cursor, logEnd)))
		return
	}
	page, hasMore := officialPaginate(events, req.BeforeSeq, maxMessages, cursor, req.TurnWindow)
	writeJSON(w, http.StatusOK, OfficialEventPage{
		Cursor:  cursor,
		Records: officialPageRecords(page),
		HasMore: hasMore,
	})
}

// handleSessionPage 是 /api/session.page（官方 deprecated 的事件页入口）。
func (s *Server) handleSessionPage(w http.ResponseWriter, r *http.Request) {
	s.serveOfficialEventPage(w, r, string(MethodSessionPage))
}

// handleSessionEvents 是 /api/session.events（官方推荐的事件页入口）。
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	s.serveOfficialEventPage(w, r, string(MethodSessionEvents))
}

// officialPendingInput 把真实挂起审批帧翻成官方的 pendingInput 词表。
//
// 官方映射（index.js:2353-2362）：approval/request→approval、
// user-questions/request→question、其余→other。本宿主只有 approval/request。
func (s *Server) officialPendingInput(sessionID string) []string {
	kinds := make([]string, 0, 1)
	seen := map[string]struct{}{}
	if s.opt.PendingApprovals == nil {
		return kinds
	}
	for _, frame := range s.opt.PendingApprovals(sessionID) {
		kind := "other"
		switch frame.Type {
		case "approval/request":
			kind = "approval"
		case "user-questions/request":
			kind = "question"
		}
		if _, dup := seen[kind]; dup {
			continue
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	return kinds
}

// handleSessionState 是 /api/session.state。
//
// 每个字段都来自真实来源：
//   - cursor = 真实 v4 日志末端 seq（空会话 -1）；
//   - running = 本进程真有这一轮的 live turn；
//   - active = 有 live turn、有真实排队项、或有挂起审批；
//   - pendingInput = 真实挂起审批帧（PendingApprovals）；
//   - queue = 真实待发内容（OfficialQueueHost.LiveQueue），没有内容视图时 null。
//
// # queue 为什么曾经是 null，现在不是
//
// 以前 `promptService.queues` 里的 queuedTurn 只保存 turnID 与 runner 闭包，
// **不保存 content**，所以只能给 null（官方对"没有队列视图"的既有表达）。
// 现在 pending 项保留 content（transport 块），编辑/steer 都作用在这份内容上，
// 于是这里能给真实条目：id=TurnID、placement=queued、content=待发块。
//
// 判据只有一条：**有内容视图才给数组**。宿主没实现 OfficialQueueHost 时仍回 null，
// 不拿 TurnID 拼一份空 content 冒充内容。
// officialQueue 取真实待发内容；宿主没有内容视图时回 nil（官方 null 分支）。
//
// 这里**不**拿 live.QueueIDs 兜底拼项：那会把"有内容但我读不到"写成"内容为空"，
// 客户端会拿一个空 content 去渲染一条空消息。读不到就回 null，让客户端知道
// 这个宿主没有队列内容视图（真实 TurnID 仍从 prompt ACK / cancel receipt 拿）。
func officialQueue(host OfficialSessionHost, sessionID string) ([]OfficialQueueItem, error) {
	qh, ok := host.(OfficialQueueHost)
	if !ok {
		return nil, nil
	}
	return qh.LiveQueue(sessionID)
}

func (s *Server) handleSessionState(w http.ResponseWriter, r *http.Request) {
	state, ok := s.serveOfficialState(w, r, string(MethodSessionState))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) serveOfficialState(w http.ResponseWriter, r *http.Request, method string) (OfficialState, bool) {
	host, ok := s.requireOfficialHost(w, method)
	if !ok {
		return OfficialState{}, false
	}
	var req officialSessionRequest
	if !decodeOfficial(w, r, &req) {
		return OfficialState{}, false
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", method+" 需要非空 sessionId"))
		return OfficialState{}, false
	}
	_, logEnd, err := host.DurableEvents(req.SessionID)
	if err != nil {
		writeSessionOfficialError(w, err, method+" 失败：")
		return OfficialState{}, false
	}
	live, err := host.LiveState(req.SessionID)
	if err != nil {
		writeSessionOfficialError(w, err, method+" 失败：")
		return OfficialState{}, false
	}
	pending := s.officialPendingInput(req.SessionID)
	queue, err := officialQueue(host, req.SessionID)
	if err != nil {
		writeSessionOfficialError(w, err, method+" 失败：")
		return OfficialState{}, false
	}
	state := OfficialState{
		Cursor:       logEnd,
		Active:       live.Running || len(queue) > 0 || len(live.QueueIDs) > 0 || len(pending) > 0,
		Running:      live.Running,
		Queue:        queue,
		PendingInput: pending,
	}
	return state, true
}

// 四个流方法（observeState / observeProjections / observeEvent / follow）的
// handler 在 official_stream_face.go：它们**共用一台拉模式帧机**（official_stream.go），
// 帧形状逐字段照官方 zod，HTTP 面走 NDJSON 长连接（见该文件顶部"HTTP 面的选择"）。
//
// 为什么从 501 改成真实现：官方客户端拿到 501 就无法订阅——"指向 /ws" 只是
// 诚实，不是实现。现在的实现同时满足两条：不假 ready（帧只在真的有数据时发）、
// 不重放历史（follow 的 sinceSeq/throughSeq 是排他下界，见 journal.go 的
// `SinceNothing` 哨兵）。

// handleSessionProjections 是 /api/session.projections。
//
// 官方结果要么是 null 要么是 {asOfSeq, values}。本宿主**永远能给出真实 baseline**
// （v4 事件 + load 投影），所以 null 分支不可达——空会话给 asOfSeq=-1 与最小 values。
//
// values 里只放本宿主**能如实推出**的键（官方这些键都是 optional，
// 键缺失的官方含义是"该能力不存在"，正好是这里的真实情况）：
//
//	sessionListMetadata  {blank, lastPromptAt}      来自 v4 事件折叠
//	title               最新 session/title 的 text   来自 v4 事件
//	workingDirectory    会话记录的工作目录           来自 load 投影
//	permissions         {currentValue}               来自 load 投影
//
// modelSelection / todos / inbox / actionCards / agentPreset / subagent /
// imageLimits / subagentCatalog / subagentTiming 一律不出现：本宿主没有对应真值。
func (s *Server) handleSessionProjections(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireOfficialHost(w, string(MethodSessionProjections))
	if !ok {
		return
	}
	var req officialSessionRequest
	if !decodeOfficial(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.projections 需要非空 sessionId"))
		return
	}
	events, logEnd, err := host.DurableEvents(req.SessionID)
	if err != nil {
		writeSessionOfficialError(w, err, string(MethodSessionProjections)+" 失败：")
		return
	}
	values := map[string]any{
		"sessionListMetadata": officialSessionListMetadata(events),
	}
	if title, ok := officialLatestTitle(events); ok {
		values["title"] = title
	}
	// load 是首屏投影，只用来取它真有的那两个字段。
	if s.opt.Load != nil {
		if res, lerr := s.opt.Load(req.SessionID, 1); lerr == nil {
			if res.CWD != "" {
				values["workingDirectory"] = res.CWD
			}
			if res.Permission != "" {
				values["permissions"] = map[string]any{"currentValue": res.Permission}
			}
		}
	}
	writeJSON(w, http.StatusOK, OfficialProjections{AsOfSeq: logEnd, Values: values})
}

// officialSessionListMetadata 复刻官方 applySessionListMetadata（list.js:128-134）。
//
// blank：折叠前缀里没有 turn/start 就是 true；
// lastPromptAt：最新一条**人类发起**的 user/message 时间（source.kind=="user"）。
func officialSessionListMetadata(events []DurableEvent) map[string]any {
	blank := true
	lastPromptAt := int64(-1)
	for _, ev := range events {
		if ev.Type == "turn/start" {
			blank = false
			continue
		}
		if ev.Type != "user/message" || !isOfficialAppendSurface(ev) {
			continue
		}
		var data struct {
			Source struct {
				Kind string `json:"kind"`
			} `json:"source"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && data.Source.Kind == "user" {
			lastPromptAt = ev.Time
		}
	}
	out := map[string]any{"blank": blank}
	if lastPromptAt >= 0 {
		out["lastPromptAt"] = lastPromptAt
	} else {
		out["lastPromptAt"] = nil
	}
	return out
}

// officialLatestTitle 取最新 session/title 的 text（官方 last-wins，L2349 的 title 说明）。
func officialLatestTitle(events []DurableEvent) (string, bool) {
	title := ""
	found := false
	for _, ev := range events {
		if ev.Type != "session/title" {
			continue
		}
		var data struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			continue
		}
		if data.Title != "" {
			title = data.Title
		} else if data.Text != "" {
			title = data.Text
		} else {
			continue
		}
		found = true
	}
	return title, found
}

// handleSessionWait 是 /api/session.wait。
//
// 真实等待语义（复刻官方 index.js:2249-2320 的 decision table）：
//
//	有 pendingInput      → reason=input-required，立刻返回；
//	还在 running         → 继续等（到 timeoutMs 仍未停 → reason=timeout）；
//	既不在跑也没挂起     → reason=idle；
//	超时                 → reason=timeout，且 state 必须是真实采样到的状态。
//
// 然后按 state.cursor 取 (afterSeq, cursor] 里的 assistant 记录（新到旧，官方
// assistantOutput），最多 maxMessages 条，多出来的 truncated=true。
func (s *Server) handleSessionWait(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireOfficialHost(w, string(MethodSessionWait))
	if !ok {
		return
	}
	var req struct {
		SessionID   string `json:"sessionId"`
		TimeoutMs   *int64 `json:"timeoutMs"`
		AfterSeq    *int64 `json:"afterSeq"`
		MaxMessages *int64 `json:"maxMessages"`
	}
	if !decodeOfficial(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.wait 需要非空 sessionId"))
		return
	}
	timeout := officialWaitDefaultTimeout
	if req.TimeoutMs != nil {
		if *req.TimeoutMs <= 0 || time.Duration(*req.TimeoutMs)*time.Millisecond > officialWaitMaxTimeout {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
				"session.wait 的 timeoutMs 必须是正数且不超过官方 MAX_TIMER_DELAY_MS"))
			return
		}
		timeout = time.Duration(*req.TimeoutMs) * time.Millisecond
	}
	afterSeq := int64(-1)
	if req.AfterSeq != nil {
		if *req.AfterSeq < -1 {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.wait 的 afterSeq 必须 >= -1"))
			return
		}
		afterSeq = *req.AfterSeq
	}
	maxMessages := int64(officialPageDefaultMessages)
	if req.MaxMessages != nil {
		if *req.MaxMessages <= 0 {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.wait 的 maxMessages 必须是正整数"))
			return
		}
		maxMessages = *req.MaxMessages
	}

	outcome, ok := s.waitForOfficialStop(w, r, host, req.SessionID, timeout)
	if !ok {
		return
	}
	events, logEnd, err := host.DurableEvents(req.SessionID)
	if err != nil {
		writeSessionOfficialError(w, err, "session.wait 失败：")
		return
	}
	if outcome.state.Cursor > logEnd {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal",
			"session.wait 的等待游标已超出当前日志（状态在等待期间被截断）"))
		return
	}
	records, truncated := officialAssistantOutput(events, afterSeq, outcome.state.Cursor, maxMessages)
	writeJSON(w, http.StatusOK, OfficialWaitResult{
		Reason: outcome.reason,
		State:  outcome.state,
		Output: OfficialOutput{Records: records, Truncated: truncated},
	})
}

// officialWaitOutcome 把"停下来的原因"与"停下来时的真实状态"绑在一起。
type officialWaitOutcome struct {
	reason string
	state  OfficialState
}

// waitForOfficialStop 轮询真实状态直到可以停。
//
// 官方是靠 agent 的 whenIdle 通知唤醒的；本宿主没有那类通知源，
// 所以如实采样。采样间隔 100ms，且每次采样都读真实 pending 审批。
func (s *Server) waitForOfficialStop(w http.ResponseWriter, r *http.Request, host OfficialSessionHost, sessionID string, timeout time.Duration) (officialWaitOutcome, bool) {
	deadline := time.Now().Add(timeout)
	sample := func() (officialWaitOutcome, error) {
		_, logEnd, err := host.DurableEvents(sessionID)
		if err != nil {
			return officialWaitOutcome{}, err
		}
		live, err := host.LiveState(sessionID)
		if err != nil {
			return officialWaitOutcome{}, err
		}
		pending := s.officialPendingInput(sessionID)
		queue, err := officialQueue(host, sessionID)
		if err != nil {
			return officialWaitOutcome{}, err
		}
		state := OfficialState{
			Cursor:       logEnd,
			Active:       live.Running || len(queue) > 0 || len(live.QueueIDs) > 0 || len(pending) > 0,
			Running:      live.Running,
			Queue:        queue,
			PendingInput: pending,
		}
		switch {
		case len(pending) > 0:
			return officialWaitOutcome{reason: "input-required", state: state}, nil
		case live.Running:
			return officialWaitOutcome{reason: "", state: state}, nil
		default:
			return officialWaitOutcome{reason: "idle", state: state}, nil
		}
	}
	timer := time.NewTimer(officialWaitPollInterval)
	defer timer.Stop()
	for {
		outcome, err := sample()
		if err != nil {
			writeSessionOfficialError(w, err, "session.wait 失败：")
			return officialWaitOutcome{}, false
		}
		if outcome.reason != "" {
			return outcome, true
		}
		if !time.Now().Before(deadline) {
			// 超时必须带回**真实采样到的**状态，不能编一个"看起来停了"的。
			outcome.reason = "timeout"
			return outcome, true
		}
		select {
		case <-r.Context().Done():
			writeJSON(w, http.StatusRequestTimeout, errorBodyFor("internal", "session.wait 被调用方取消"))
			return officialWaitOutcome{}, false
		case <-timer.C:
			timer.Reset(officialWaitPollInterval)
		}
	}
}

// officialAssistantOutput 是官方 assistantOutput（index.js:1901-1925）的移植。
//
// 从 throughSeq 往下走到 afterSeq+1（不含），收 assistant/message，
// 收满 maxMessages 条就停下并标 truncated=true。**结果顺序是新到旧**。
func officialAssistantOutput(events []DurableEvent, afterSeq, throughSeq, maxMessages int64) ([]OfficialRecord, bool) {
	records := make([]OfficialRecord, 0, maxMessages)
	end := throughSeq + 1
	if end > int64(len(events)) {
		end = int64(len(events))
	}
	for i := end - 1; i > afterSeq; i-- {
		ev := events[i]
		if ev.Type != "assistant/message" || !isOfficialAppendSurface(ev) {
			continue
		}
		if int64(len(records)) == maxMessages {
			return records, true
		}
		records = append(records, OfficialRecord{Type: "event", Event: ev})
	}
	return records, false
}

// handleSessionSearch 是 /api/session.search。
//
// 官方语义（list.js:180-275）：
//   - query 先 trim，空 → 400；>500 UTF-16 单元 → 400；含 NUL → 400；
//   - 只搜 user/message 与 assistant/message，只搜 canonical surface；
//   - 结果按会话去重，最多 SESSION_SEARCH_RESULT_LIMIT=20 条；
//   - snippet 最多 240 code point；
//   - 搜不到就是空数组，**不是错误**。
func (s *Server) handleSessionSearch(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireOfficialHost(w, string(MethodSessionSearch))
	if !ok {
		return
	}
	var req struct {
		Query string `json:"query"`
	}
	if !decodeOfficial(w, r, &req) {
		return
	}
	normalized := strings.TrimSpace(req.Query)
	if normalized == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.search 的 query 不能为空"))
		return
	}
	if len([]rune(normalized)) > officialSearchQueryMaxChars {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
			fmt.Sprintf("session.search 的 query 最多 %d 个字符", officialSearchQueryMaxChars)))
		return
	}
	if strings.ContainsRune(normalized, 0) {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.search 的 query 不能包含 NUL"))
		return
	}
	items, hasMore, err := host.Search(normalized, officialSearchResultLimit)
	if err != nil {
		writeSessionOfficialError(w, err, "session.search 失败：")
		return
	}
	if items == nil {
		items = []OfficialSearchItem{}
	}
	writeJSON(w, http.StatusOK, OfficialSearchResult{Items: items, HasMore: hasMore})
}

// OfficialSnippet 把命中正文裁成官方 snippet（≤240 code point）。
//
// 命中点通常不在开头，直接从开头截会把命中内容截掉——所以取以命中点开始的窗口，
// 前面被裁掉时补一个省略号。裁剪按 code point 数，不按字节（多字节字符会切坏）。
func OfficialSnippet(text string, hit int) string {
	runes := []rune(text)
	if len(runes) <= officialSearchSnippetMaxCodePoints {
		return text
	}
	start := 0
	if hit > 0 {
		// 命中点落到第 hit 个 rune；回退一点上下文，但不退到 0 之前。
		start = hit - officialSearchSnippetMaxCodePoints/4
		if start < 0 {
			start = 0
		}
	}
	if start+officialSearchSnippetMaxCodePoints > len(runes) {
		start = len(runes) - officialSearchSnippetMaxCodePoints
	}
	if start < 0 {
		start = 0
	}
	out := string(runes[start : start+officialSearchSnippetMaxCodePoints])
	if start > 0 {
		out = "…" + out
	}
	return out
}

// officialSearchWindow 在 text 里找 query（大小写不敏感）并给出命中点的 rune 下标。
func OfficialSearchWindow(text, query string) (int, bool) {
	lowerText := strings.ToLower(text)
	lowerQuery := strings.ToLower(query)
	byteIdx := strings.Index(lowerText, lowerQuery)
	if byteIdx < 0 {
		return 0, false
	}
	return len([]rune(text[:byteIdx])), true
}

// officialImageMediaType 校验媒体类型落在官方四类里（官方从存储字节验，不信扩展名）。
func officialImageMediaType(mime string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return "image/png", true
	case "image/jpeg", "image/jpg":
		return "image/jpeg", true
	case "image/webp":
		return "image/webp", true
	case "image/gif":
		return "image/gif", true
	}
	return "", false
}

// handleSessionAttachment 是 /api/session.attachment。
//
// 官方参数里的 attachment 是**校验过的引用**（id/格式/字节数/尺寸），
// 结果回 store 里的真值与 base64 字节。sessionId 只是网关权限范围，
// **不检查会话是否存在、也不检查这条会话引用过它**（官方 L1904 明确说的），
// 所以这里不拿 sessionId 去 resolve——那会把"范围"错当成"所有权"。
func (s *Server) handleSessionAttachment(w http.ResponseWriter, r *http.Request) {
	if s.opt.Attachments == nil {
		writeJSON(w, http.StatusNotImplemented,
			errorBodyFor("not_implemented", "session.attachment 未接线：宿主的 attachment store 没接上"))
		return
	}
	var req struct {
		SessionID  string `json:"sessionId"`
		Attachment struct {
			AttachmentID       string `json:"attachmentId"`
			MediaType          string `json:"mediaType"`
			Bytes              int64  `json:"bytes"`
			Width              int    `json:"width"`
			Height             int    `json:"height"`
			Name               string `json:"name"`
			OriginalDimensions *struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			} `json:"originalDimensions"`
		} `json:"attachment"`
	}
	if !decodeOfficial(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.attachment 需要非空 sessionId"))
		return
	}
	if strings.TrimSpace(req.Attachment.AttachmentID) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.attachment 需要 attachment.attachmentId"))
		return
	}
	content, err := s.opt.Attachments.Get(req.Attachment.AttachmentID)
	if err != nil {
		writeAttachmentError(w, err, "not_found", http.StatusNotFound)
		return
	}
	mediaType, imageOK := officialImageMediaType(content.Attachment.MIME)
	if !imageOK {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
			"session.attachment 只承载 image/png、image/jpeg、image/webp、image/gif；这份存储的 MIME 是 "+
				quoteOrUnknown(content.Attachment.MIME)))
		return
	}
	// 官方会核对引用里的格式/字节数/尺寸与存储真值是否一致；不一致说明引用过期或被改过。
	if req.Attachment.MediaType != "" {
		if declared, ok := officialImageMediaType(req.Attachment.MediaType); !ok || declared != mediaType {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
				"session.attachment 的 mediaType 与存储真值（"+mediaType+"）不一致"))
			return
		}
	}
	if req.Attachment.Bytes != 0 && req.Attachment.Bytes != content.Attachment.ByteSize {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
			fmt.Sprintf("session.attachment 的 bytes=%d 与存储真值 %d 不一致", req.Attachment.Bytes, content.Attachment.ByteSize)))
		return
	}
	if req.Attachment.Width != 0 && req.Attachment.Width != content.Attachment.Width {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
			fmt.Sprintf("session.attachment 的 width=%d 与存储真值 %d 不一致", req.Attachment.Width, content.Attachment.Width)))
		return
	}
	if req.Attachment.Height != 0 && req.Attachment.Height != content.Attachment.Height {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
			fmt.Sprintf("session.attachment 的 height=%d 与存储真值 %d 不一致", req.Attachment.Height, content.Attachment.Height)))
		return
	}
	name := req.Attachment.Name
	if name == "" {
		name = content.Attachment.Name
	}
	writeJSON(w, http.StatusOK, OfficialAttachmentResult{
		Attachment: OfficialAttachment{
			AttachmentID: content.Attachment.ID,
			MediaType:    mediaType,
			Bytes:        content.Attachment.ByteSize,
			Width:        content.Attachment.Width,
			Height:       content.Attachment.Height,
			Name:         name,
		},
		Data: base64.StdEncoding.EncodeToString(content.Data),
	})
}

// quoteOrUnknown 让错误消息里"没有 MIME"这件事看得见，而不是一个空串。
func quoteOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "（空）"
	}
	return value
}

// handleSessionSelectModel 是 /api/session.selectModel。
//
// 走既有 model override 存储（`<home>/state/session-model.json`），
// 带 reasoningEffort 时走既有 effort override 存储（同一个 model catalog 校验）。
// **不写 v4**（R5：读路径不写盘；模型选择是界面状态）。
//
// 契约差异（如实登记）：provider 只回显、不持久化——本宿主没有 provider 维度的
// 覆盖存储，模型 id 已经唯一到一个网关路由。所以本方法**不校验 provider 是否可用**
// （官方 requireModel 会），校验的是 model/effort 在本机 catalog 里真实存在。
func (s *Server) handleSessionSelectModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID       string  `json:"sessionId"`
		Provider        string  `json:"provider"`
		Model           string  `json:"model"`
		ReasoningEffort *string `json:"reasoningEffort"`
	}
	if !decodeOfficial(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.selectModel 需要非空 sessionId"))
		return
	}
	if strings.TrimSpace(req.Provider) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.selectModel 需要非空 provider"))
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.selectModel 需要非空 model"))
		return
	}
	if s.opt.SetSessionModel == nil {
		writeJSON(w, http.StatusNotImplemented,
			errorBodyFor("not_implemented", "session.selectModel 未接线：宿主没有 model override 存储"))
		return
	}
	if err := s.opt.SetSessionModel(req.SessionID, req.Model); err != nil {
		writeSessionOfficialError(w, err, "session.selectModel 失败：")
		return
	}
	selected := OfficialModelSelection{Provider: req.Provider, Model: req.Model}
	if req.ReasoningEffort != nil {
		if *req.ReasoningEffort == "" {
			writeJSON(w, http.StatusBadRequest,
				errorBodyFor("bad_request", "session.selectModel 的 reasoningEffort 给了就必须非空"))
			return
		}
		if s.opt.SetSessionEffort == nil {
			writeJSON(w, http.StatusNotImplemented,
				errorBodyFor("not_implemented", "session.selectModel 带 reasoningEffort 时需要 effort override 存储，宿主没接线"))
			return
		}
		if err := s.opt.SetSessionEffort(req.SessionID, *req.ReasoningEffort); err != nil {
			writeSessionOfficialError(w, err, "session.selectModel 的 reasoningEffort 失败：")
			return
		}
		selected.ReasoningEffort = *req.ReasoningEffort
	}
	writeJSON(w, http.StatusOK, OfficialSelectModelResult{Selected: selected})
}

// handleSessionUpdateQueue 是 /api/session.updateQueue。
//
// 三种 action 都走真实能力（OfficialQueueHost）：
//
//	remove  ✅ CancelPlacement(sessionId,"user",itemId) 只撤那一项，不碰当前轮；
//	        不在队列里 → 404（不能假装撤过）。
//	edit    ✅ 直接把这一项的真实待发内容换成新文本块；排队项保留 content，
//	        所以改完之后**真的会按新内容发**（不是改一份视图）。不在队列里 → 404。
//	steer   ✅ 把这一项从队列里摘掉，走真实的 steer 注入通道塞进**正在跑**的那一轮
//	        （promptService 的 steerTurnRunner/steerContentTurnRunner）。当前轮已
//	        不接受 steer → 409（官方 session/steer-unavailable），注入失败则原样
//	        放回队列并如实报错——不吞消息。
//
// 宿主没实现 OfficialQueueHost 时 edit/steer 回 501 并点名缺什么。
func (s *Server) handleSessionUpdateQueue(w http.ResponseWriter, r *http.Request) {
	host, ok := s.requireOfficialHost(w, string(MethodSessionUpdateQueue))
	if !ok {
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ItemID    string `json:"itemId"`
		Action    struct {
			Kind    string `json:"kind"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"action"`
	}
	if !decodeOfficial(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.ItemID) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.updateQueue 需要非空 sessionId 与 itemId"))
		return
	}
	switch req.Action.Kind {
	case "edit":
		// 先按官方把参数校验完（非文本、全空白都要拒），再说能力。
		if len(req.Action.Content) == 0 {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.updateQueue 的 edit 需要非空 content"))
			return
		}
		hasText := false
		for _, block := range req.Action.Content {
			if block.Type != "text" {
				writeJSON(w, http.StatusBadRequest,
					errorBodyFor("bad_request", "session.updateQueue 的 edit 只接受 text 块（官方 QUEUE_EDIT_NON_TEXT）"))
				return
			}
			if strings.TrimSpace(block.Text) != "" {
				hasText = true
			}
		}
		if !hasText {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.updateQueue 的 edit 内容必须含非空白文本"))
			return
		}
		qh, ok := host.(OfficialQueueHost)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
				"session.updateQueue.edit 需要宿主提供真实待发内容视图（OfficialQueueHost）"))
			return
		}
		content := make([]json.RawMessage, 0, len(req.Action.Content))
		for _, block := range req.Action.Content {
			raw, err := json.Marshal(map[string]string{"type": "text", "text": block.Text})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "session.updateQueue.edit 序列化失败："+err.Error()))
				return
			}
			content = append(content, raw)
		}
		if err := qh.EditQueueItem(req.SessionID, req.ItemID, content); err != nil {
			writeSessionOfficialError(w, err, "session.updateQueue.edit 失败：")
			return
		}
		writeJSON(w, http.StatusOK, OfficialQueueAck{Accepted: true})
	case "remove":
		if err := host.RemoveQueueItem(req.SessionID, req.ItemID); err != nil {
			writeSessionOfficialError(w, err, "session.updateQueue.remove 失败：")
			return
		}
		writeJSON(w, http.StatusOK, OfficialQueueAck{Accepted: true})
	case "steer":
		qh, ok := host.(OfficialQueueHost)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
				"session.updateQueue.steer 需要宿主提供真实待发内容视图（OfficialQueueHost）"))
			return
		}
		if err := qh.SteerQueueItem(req.SessionID, req.ItemID); err != nil {
			writeSessionOfficialError(w, err, "session.updateQueue.steer 失败：")
			return
		}
		writeJSON(w, http.StatusOK, OfficialQueueAck{Accepted: true})
	case "":
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.updateQueue 缺 action.kind"))
	default:
		writeJSON(w, http.StatusBadRequest,
			errorBodyFor("bad_request", "session.updateQueue 的 action.kind 只能是 edit/remove/steer"))
	}
}

// handleSessionOpenWorkspacePath 是 /api/session.openWorkspacePath。
//
// 本宿主没有桌面打开通道（官方走 nativeFileManager + verifyDesktopPath 把
// Host 路径映射到进程可见路径再 spawn）。用 `cmd /c start` 之类去凑一个"能开"
// 会把 dshd 变成一个可以拉起任意本地程序的远程执行面，而本仓的沙盒三档还没生产接线。
// 所以这里**明确回 501**，不返回 `{opened:true}` 假 ok。
func (s *Server) handleSessionOpenWorkspacePath(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action      string `json:"action"`
		Application string `json:"application"`
		Path        string `json:"path"`
	}
	if !decodeOfficial(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.openWorkspacePath 需要非空 path"))
		return
	}
	if req.Action != "" && req.Action != "reveal" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.openWorkspacePath 的 action 只能是 reveal 或缺省"))
		return
	}
	writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
		"session.openWorkspacePath 未实现：本宿主没有桌面打开通道（不 spawn 本地程序）；"+
			"path 已校验，但 opened 不能假装为 true"))
}

// handleSessionWorkspacePathApplications 是 /api/session.workspacePathApplications。
//
// 同样回 501：官方的实现是"能开路径就列系统文件关联程序，不能开就回 []"
// （index.js:4001 `if (!this.canOpenPath()) return [];`）。本宿主没有桌面打开通道，
// 按任务口径**返回明确的未实现错误**而不是一个空数组——空数组在客户端看起来
// 与"这个系统确实没有关联程序"完全一样，那正是"假 ok"的一种。
func (s *Server) handleSessionWorkspacePathApplications(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decodeOfficial(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.workspacePathApplications 需要非空 path"))
		return
	}
	writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
		"session.workspacePathApplications 未实现：本宿主没有桌面文件关联查询通道；"+
			"官方对无桌面能力的部署回 []，本宿主不拿空数组冒充这个结论"))
}
