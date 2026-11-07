package transport

// 官方桌面端 0.2.2 的 schedule.create / edit / view / run / history。
//
// # 契约来源（只读镜像，未改安装目录）
//
//	镜像根：%TEMP%/official-api-src/dsh/node_modules/@deepseek-ai/dsh-schedule/lib/typert.host.js
//
//	typert.host.js:1908  'schedule/create'  参数 request（title,prompt,timing,target?）
//	                      结果 @deepseek-ai/dsh-schedule/client#ScheduleCatalogEntry（7 kind 的 union）
//	typert.host.js:1972  'schedule/edit'    参数 request（id, action=update|delete, patch?, expectedUpdatedAt?）
//	                      结果 ScheduleCatalogEntry | {id,code:'schedule_not_found'}
//	                        | {id,deleted:true} | {id,code:'schedule_conflict'}
//	typert.host.js:2006  'schedule/history' 参数 request（id, limit, before?）
//	                      结果 {id,records[],earlierRecordsUnavailable,earlierRecordsPruned,
//	                        retention{days,records},nextBefore?} | {id,code:'schedule_not_found'|'delivery_cursor_not_found'}
//	typert.host.js:2072  'schedule/run'     参数 request（id）
//	                      结果 ScheduleCatalogEntry | {id,code:'schedule_not_found'|'schedule_execution_pending'}
//	typert.host.js:2105  'schedule/view'    参数 request（id）
//	                      结果 ScheduleCatalogEntry | {id,code:'schedule_not_found'}
//
// 结果的 zod 原文（同一文件）：
//
//	typert.host.js:321   create 结果 union（jsonSchema 可读，7 个 kind 分支）
//	typert.host.js:653   edit 结果 union（7 个 kind 分支 + not_found + deleted + conflict）
//	typert.host.js:925   history 结果 union（page + not_found/cursor_not_found）
//	typert.host.js:1219/1486  run / view 结果 union（typert 自己的 jsonSchemaError 说
//	                     "selected union branches overlap"，所以 jsonSchema 是空的——
//	                     分支形状取自同一行的 zod 源码，不是我们编的）
//	delivery-history.js  分页语义：newest-first、before 是 messageId 排他游标、
//	                     只有"更旧还有"时才给 nextBefore、retention 由 Host 配置
//
// # 边界
//
// 做：官方参数/结果的**字段名与可选性**翻译、fail-loud 校验、四字段错误契约、
// 按 *Server 隔离的挂载表。
//
// 不做（不许假装）：
//   - 不碰存储。真实记录由宿主经 OfficialScheduleAPI 注入（cmd/dshd/serve_schedule_api.go）；
//     本文件不读盘、不 import internal/schedule（红线 R7）。
//   - 不实现"假执行"：宿主没有 runner 时 schedule.run 必须回 501 并说清缺什么。
//   - 不改既有 schedule.list/delete/clear：它们仍在 api.go + handlers_jobs.go 里，
//     本文件的方法集合**故意认领不到**这三个名字（见 IsOfficialScheduleMethod）。
//
// # 接线（Lead 在 internal/transport/server.go 的 handleAPI 里补一行）
//
//	if IsOfficialScheduleMethod(name) {
//		s.serveOfficialScheduleAPI(w, r, name)
//		return
//	}
//
// 与既有挂钩同层即可：必须排在 `switch Method(name)` 之前（那之后是既有的
// schedule.list/delete/clear case，既不冲突也不需要改）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 官方 5 个方法名。既有 schedule.list/delete/clear 不在这里（它们是宿主 additive 面）。
const (
	MethodScheduleCreate  Method = "schedule.create"
	MethodScheduleEdit    Method = "schedule.edit"
	MethodScheduleView    Method = "schedule.view"
	MethodScheduleRun     Method = "schedule.run"
	MethodScheduleHistory Method = "schedule.history"
)

const (
	// scheduleTitleMaxRunes 是官方 title 的上限（"at most 120 characters"）。
	scheduleTitleMaxRunes = 120
	// scheduleHistoryLimitMin/Max 是官方 history limit 的范围（"safe integer from 1 through 100"）。
	scheduleHistoryLimitMin = 1
	scheduleHistoryLimitMax = 100
	// scheduleMinEverySeconds：官方 every_seconds 的下界按本仓既有存储的
	// 一分钟下限执行（internal/schedule 的 every 分支 < 60 会被推成整分钟，
	// 与其静默改值，不如在入口拒掉）。
	scheduleMinEverySeconds = 60
)

// MinScheduleEverySeconds 对外暴露 every_seconds 的下界，供宿主侧复校验用。
//
// 为什么不在宿主里再写一个 60：两处各写一个，迟早一处改了一处没改；
// 这里给出的就是 Transport 入口拒掉的那条线。
func MinScheduleEverySeconds() int { return scheduleMinEverySeconds }

// OfficialScheduleMethods 返回本层负责的 5 个方法（顺序固定，快照按它排）。
//
// **additive 扩展**：不得加入 SessionMethods 的 canonical 25，
// 也不得进 TaskActionMethods（那是 jobs.stop 与既有 schedule.delete/clear 的扩展面）。
func OfficialScheduleMethods() []Method {
	return []Method{
		MethodScheduleCreate, MethodScheduleEdit, MethodScheduleView,
		MethodScheduleRun, MethodScheduleHistory,
	}
}

// IsOfficialScheduleMethod 判断一个 /api 路径名是否属于本文件的 5 个方法。
//
// 路由侧只需要这一次判断（**不要**在 server.go 的 switch 里摊开 5 个 case：
// 那是冻结的路由表）。既有 schedule.list/delete/clear **必须**返回 false：
// 它们由 handlers_jobs.go 处理，被这里认领会等于悄悄换掉它们的读语义。
func IsOfficialScheduleMethod(name string) bool {
	switch Method(name) {
	case MethodScheduleCreate, MethodScheduleEdit, MethodScheduleView,
		MethodScheduleRun, MethodScheduleHistory:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 官方数据结构（字段名/可选性逐条照 zod 契约）
// ---------------------------------------------------------------------------

// OfficialScheduleCreateInfo 是 new / new_once 目标的会话创建参数。
type OfficialScheduleCreateInfo struct {
	CWD         string `json:"cwd,omitempty"`
	AgentPreset string `json:"agentPreset,omitempty"`
}

// OfficialScheduleTarget 是"下一次投递去哪"。
//
// kind=session 时带 sessionId；new_once / new 带 create。本仓宿主只实现
// session（没有到点自动开会话的执行器），另两种由宿主回 501。
type OfficialScheduleTarget struct {
	Kind      string                      `json:"kind"`
	SessionID string                      `json:"sessionId,omitempty"`
	Create    *OfficialScheduleCreateInfo `json:"create,omitempty"`
}

// OfficialScheduleAt 是 at 型时间：官方既接受一个字符串，也接受
// {date,time,time_zone} 三段。两种写法都原样收下，由宿主决定能不能落。
type OfficialScheduleAt struct {
	// Instant 是字符串形式（RFC3339）。
	Instant string
	// Date/Clock/TimeZone 是三段对象形式。Clock 保留官方叫法 time。
	Date     string
	Clock    string
	TimeZone string
}

// OfficialScheduleDailyTime 是 daily 的 {time,time_zone}。
type OfficialScheduleDailyTime struct {
	Time     string
	TimeZone string
}

// OfficialScheduleWeeklyTime 是 weekly 的 {time,time_zone,weekdays}。
type OfficialScheduleWeeklyTime struct {
	Time     string
	TimeZone string
	Weekdays []int
}

// OfficialScheduleCron 是 cron 的 {expression,time_zone}。
type OfficialScheduleCron struct {
	Expression string
	TimeZone   string
}

// OfficialScheduleTiming 是官方 timing union 的宿主侧形状。
//
// 只有一个分支的字段非零（Kind 决定哪个）；其余保持零值。
type OfficialScheduleTiming struct {
	Kind         string
	AfterSeconds int64
	At           *OfficialScheduleAt
	EverySeconds int64
	Daily        *OfficialScheduleDailyTime
	Weekly       *OfficialScheduleWeeklyTime
	Cron         *OfficialScheduleCron
	RRule        string
}

// OfficialScheduleLastDelivery 是一条已确认投递的凭据。
type OfficialScheduleLastDelivery struct {
	ScheduledAt     string `json:"scheduledAt"`
	DeliveredAt     string `json:"deliveredAt"`
	MessageID       string `json:"messageId"`
	TargetSessionID string `json:"targetSessionId"`
}

// OfficialScheduleEntry 是官方 ScheduleCatalogEntry（7 个 kind 的 union）。
//
// 用**一个结构 + omitempty**表示 union：kind 决定哪些字段出现。
// 官方必填的 9 个字段（id/kind/title/prompt/scheduledAt/target/
// involvedSessionIds/status + 各 kind 自己的时间字段）不带 omitempty 的只有
// 通用那几个；kind 专属字段靠 omitempty 天然缺省。
type OfficialScheduleEntry struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Prompt string `json:"prompt"`

	// 各 kind 专属字段（同一条记录只会有其中一组）。
	AfterSeconds int64  `json:"afterSeconds,omitempty"`
	EverySeconds int64  `json:"everySeconds,omitempty"`
	Time         string `json:"time,omitempty"`
	TimeZone     string `json:"timeZone,omitempty"`
	Weekdays     []int  `json:"weekdays,omitempty"`
	Expression   string `json:"expression,omitempty"`
	RRule        string `json:"rrule,omitempty"`

	ScheduledAt         string                        `json:"scheduledAt"`
	Target              OfficialScheduleTarget        `json:"target"`
	InvolvedSessionIDs  []string                      `json:"involvedSessionIds"`
	Status              string                        `json:"status"`
	CreatedAt           string                        `json:"createdAt,omitempty"`
	UpdatedAt           string                        `json:"updatedAt,omitempty"`
	ExecutionActive     bool                          `json:"executionActive,omitempty"`
	DeliveryFloorEndsAt string                        `json:"deliveryFloorEndsAt,omitempty"`
	LastDelivery        *OfficialScheduleLastDelivery `json:"lastDelivery,omitempty"`
}

// OfficialScheduleMiss 是官方 union 里的"非改动命中"：
// {id, code}，code 取 schedule_not_found / schedule_conflict /
// schedule_execution_pending / delivery_cursor_not_found。
//
// 它**不是错误**：官方把它当成功结果返回（"a non-mutating miss"），
// 所以这里也走 200 + 这个对象，而不是四字段错误。
type OfficialScheduleMiss struct {
	ID   string `json:"id"`
	Code string `json:"code"`
}

// OfficialScheduleDeleted 是 edit(action=delete) 成功后的结果。
type OfficialScheduleDeleted struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// OfficialScheduleHistoryRecord 是一次已保存的投递。
type OfficialScheduleHistoryRecord struct {
	Prompt          string `json:"prompt,omitempty"`
	ScheduledAt     string `json:"scheduledAt"`
	DeliveredAt     string `json:"deliveredAt"`
	MessageID       string `json:"messageId"`
	TargetSessionID string `json:"targetSessionId"`
}

// OfficialScheduleRetention 是 Host 真实的保留口径（与写入方用同一份配置）。
type OfficialScheduleRetention struct {
	Days    int `json:"days"`
	Records int `json:"records"`
}

// OfficialScheduleHistoryPage 是 schedule.history 的一页（newest first）。
type OfficialScheduleHistoryPage struct {
	ID                        string                          `json:"id"`
	Records                   []OfficialScheduleHistoryRecord `json:"records"`
	EarlierRecordsUnavailable bool                            `json:"earlierRecordsUnavailable"`
	EarlierRecordsPruned      bool                            `json:"earlierRecordsPruned"`
	Retention                 OfficialScheduleRetention       `json:"retention"`
	NextBefore                string                          `json:"nextBefore,omitempty"`
}

// ---------------------------------------------------------------------------
// 请求形状（transport 解析 → 宿主执行）
// ---------------------------------------------------------------------------

// OfficialScheduleCreateRequest 是官方 ScheduleCreateRequest。
type OfficialScheduleCreateRequest struct {
	Title  string
	Prompt string
	Timing OfficialScheduleTiming
	// Target 为 nil 表示官方没给。本宿主没有"隐式调用会话"，宿主据此拒。
	Target *OfficialScheduleTarget
}

// OfficialSchedulePatch 是 edit(action=update) 的 patch；至少一个非 nil。
type OfficialSchedulePatch struct {
	Title  *string
	Prompt *string
	Timing *OfficialScheduleTiming
	Target *OfficialScheduleTarget
	Active *bool
}

// OfficialScheduleEditRequest 是官方 ScheduleEditRequest。
type OfficialScheduleEditRequest struct {
	ID                string
	Action            string // update | delete
	Patch             *OfficialSchedulePatch
	ExpectedUpdatedAt *string // nil = 没给或显式 null
}

// OfficialScheduleHistoryRequest 是官方 ScheduleHistoryRequest。
type OfficialScheduleHistoryRequest struct {
	ID     string
	Limit  int
	Before string
}

// ---------------------------------------------------------------------------
// 挂载点
// ---------------------------------------------------------------------------

// OfficialScheduleAPI 是宿主侧的真实实现集合。
//
// **每个字段都可以为 nil**：为 nil 表示这个能力在当前宿主上没有真实支撑，
// 调用回 not_implemented 并说清缺什么——不是静默成功，也不是假数据。
//
// 返回类型是 any 的那几个对应官方 union：宿主必须返回本文件里的
// OfficialScheduleEntry / OfficialScheduleMiss / OfficialScheduleDeleted /
// OfficialScheduleHistoryPage 之一；别的类型会被当成宿主 bug 报 500。
type OfficialScheduleAPI struct {
	Create  func(req OfficialScheduleCreateRequest) (OfficialScheduleEntry, error)
	Edit    func(req OfficialScheduleEditRequest) (any, error)
	View    func(id string) (any, error)
	Run     func(id string) (any, error)
	History func(req OfficialScheduleHistoryRequest) (any, error)
}

// officialScheduleMount 把一份实现挂到一个 Server 上。
//
// 为什么不用 `Options` 加字段：Options 在 server.go（本任务的写作用域之外）。
// 按 *Server 索引的隔离性与"每个 Server 一份实现"完全一致：A/B 两个 Server
// 各挂各的，互不可见，也不会有进程级单例被两个宿主共享。
var officialScheduleMount = struct {
	mu sync.RWMutex
	m  map[*Server]OfficialScheduleAPI
}{m: map[*Server]OfficialScheduleAPI{}}

// AttachOfficialScheduleAPI 给这个 Server 挂上官方 schedule API 的真实实现。
// 必须发生在服务开始接受 /api 调用之前（serve 的装配阶段）。
func (s *Server) AttachOfficialScheduleAPI(api OfficialScheduleAPI) {
	if s == nil {
		return
	}
	officialScheduleMount.mu.Lock()
	defer officialScheduleMount.mu.Unlock()
	officialScheduleMount.m[s] = api
}

// DetachOfficialScheduleAPI 摘掉实现（测试收尾用；生产不需要调）。
func (s *Server) DetachOfficialScheduleAPI() {
	if s == nil {
		return
	}
	officialScheduleMount.mu.Lock()
	defer officialScheduleMount.mu.Unlock()
	delete(officialScheduleMount.m, s)
}

func (s *Server) officialScheduleAPI() (OfficialScheduleAPI, bool) {
	if s == nil {
		return OfficialScheduleAPI{}, false
	}
	officialScheduleMount.mu.RLock()
	defer officialScheduleMount.mu.RUnlock()
	api, ok := officialScheduleMount.m[s]
	return api, ok
}

// ---------------------------------------------------------------------------
// 分发
// ---------------------------------------------------------------------------

// serveOfficialScheduleAPI 处理本文件的 5 个方法（Lead 的接线入口）。
//
// 调用方（server.go 的 /api 路由）只需在既有 switch 之前判断
// `IsOfficialScheduleMethod(name)` 并调用它；认证、POST 校验、失败日志
// 都已经在 handleAPI 里做过了。
func (s *Server) serveOfficialScheduleAPI(w http.ResponseWriter, r *http.Request, name string) {
	api, ok := s.officialScheduleAPI()
	if !ok {
		api = OfficialScheduleAPI{}
	}
	ServeOfficialSchedule(w, r, name, api)
}

// ServeOfficialSchedule 是纯函数形态的分发入口（挂载表之外也能直接打）：
// 测试与"非 *Server 宿主"都用它，路由接线走上面的方法。
func ServeOfficialSchedule(w http.ResponseWriter, r *http.Request, name string, api OfficialScheduleAPI) {
	if !IsOfficialScheduleMethod(name) {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
			fmt.Sprintf("不是官方 schedule 族的方法：%q", name)))
		return
	}
	params, err := decodeOfficialParams(r)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	result, err := routeOfficialScheduleMethod(api, Method(name), params)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	if result == nil {
		// 本仓出口不接受 `{"result":null}`；宿主没给载荷时用空对象，
		// 保持"成功但无内容"与"失败"可区分。
		result = map[string]any{}
	}
	writeJSON(w, http.StatusOK, result)
}

// routeOfficialScheduleMethod 是一个小 switch，放在本文件里而不是 server.go：
// 官方方法还会继续加，路由表却是冻结的。
func routeOfficialScheduleMethod(api OfficialScheduleAPI, method Method, p *officialParams) (any, error) {
	switch method {
	case MethodScheduleCreate:
		return officialScheduleCreate(api, p)
	case MethodScheduleEdit:
		return officialScheduleEdit(api, p)
	case MethodScheduleView:
		return officialScheduleView(api, p)
	case MethodScheduleRun:
		return officialScheduleRun(api, p)
	case MethodScheduleHistory:
		return officialScheduleHistory(api, p)
	}
	return nil, officialNotImplemented("未知的官方 schedule 方法 %q", method)
}

// normalizeScheduleEntry 保证官方必填的数组字段不是 null。
//
// 官方 `involvedSessionIds` 是 `type:array`（必填）：Go 的 nil slice 会编码成
// `null`，那是形状错误。空数组是"没有任何会话被通知"的真实结论，不是编数据。
func normalizeScheduleEntry(entry OfficialScheduleEntry) OfficialScheduleEntry {
	if entry.InvolvedSessionIDs == nil {
		entry.InvolvedSessionIDs = []string{}
	}
	if entry.Weekdays == nil && entry.Kind == "weekly" {
		entry.Weekdays = []int{}
	}
	return entry
}

// checkScheduleResultType 拦住宿主返回的意外类型：
// 返回 nil 用空对象兜底，但一个 `string` 之类的载荷必须报宿主 bug，
// 而不是把 {@code "result":"oops"} 当成功发给客户端。
func checkScheduleResultType(result any) (any, error) {
	switch v := result.(type) {
	case nil:
		return map[string]any{}, nil
	case OfficialScheduleEntry:
		return normalizeScheduleEntry(v), nil
	case *OfficialScheduleEntry:
		if v == nil {
			return map[string]any{}, nil
		}
		return normalizeScheduleEntry(*v), nil
	case OfficialScheduleMiss, OfficialScheduleDeleted, OfficialScheduleHistoryPage, *OfficialScheduleHistoryPage:
		return v, nil
	default:
		return nil, officialInternal("schedule 宿主返回了不认识的载荷类型 %T", result)
	}
}

// ---------------------------------------------------------------------------
// schedule.create
// ---------------------------------------------------------------------------

func officialScheduleCreate(api OfficialScheduleAPI, p *officialParams) (any, error) {
	req, err := scheduleCreateRequest(p)
	if err != nil {
		return nil, err
	}
	if api.Create == nil {
		return nil, officialNotImplemented("schedule.create 未接线：宿主没有 AttachOfficialScheduleAPI")
	}
	entry, err := api.Create(req)
	if err != nil {
		return nil, err
	}
	return normalizeScheduleEntry(entry), nil
}

func scheduleCreateRequest(p *officialParams) (OfficialScheduleCreateRequest, error) {
	var req OfficialScheduleCreateRequest
	o, err := scheduleRequestObject(p)
	if err != nil {
		return req, err
	}
	if err := p.finish(); err != nil {
		return req, err
	}

	title, ok, err := o.str("title")
	if err != nil {
		return req, err
	}
	if !ok {
		return req, officialBadRequest("缺 title（官方 ScheduleCreateRequest.title 必填）")
	}
	req.Title = strings.TrimSpace(title)
	if req.Title == "" {
		return req, officialBadRequest("title 是空串")
	}
	if n := utf8.RuneCountInString(req.Title); n > scheduleTitleMaxRunes {
		return req, officialBadRequest("title 有 %d 个字，官方上限是 %d", n, scheduleTitleMaxRunes)
	}

	prompt, ok, err := o.str("prompt")
	if err != nil {
		return req, err
	}
	if !ok {
		return req, officialBadRequest("缺 prompt（官方 ScheduleCreateRequest.prompt 必填）")
	}
	req.Prompt = strings.TrimSpace(prompt)
	if req.Prompt == "" {
		return req, officialBadRequest("prompt 是空串")
	}

	timing, ok, err := scheduleTimingField(o, "timing", true)
	if err != nil {
		return req, err
	}
	if !ok {
		return req, officialBadRequest("缺 timing（官方 ScheduleCreateRequest.timing 必填）")
	}
	req.Timing = *timing

	target, ok, err := scheduleTargetField(o, "target")
	if err != nil {
		return req, err
	}
	if ok {
		req.Target = target
	}

	if err := o.finish(); err != nil {
		return req, err
	}
	return req, nil
}

// ---------------------------------------------------------------------------
// schedule.edit
// ---------------------------------------------------------------------------

func officialScheduleEdit(api OfficialScheduleAPI, p *officialParams) (any, error) {
	req, err := scheduleEditRequest(p)
	if err != nil {
		return nil, err
	}
	if api.Edit == nil {
		return nil, officialNotImplemented("schedule.edit 未接线：宿主没有 AttachOfficialScheduleAPI")
	}
	result, err := api.Edit(req)
	if err != nil {
		return nil, err
	}
	return checkScheduleResultType(result)
}

func scheduleEditRequest(p *officialParams) (OfficialScheduleEditRequest, error) {
	var req OfficialScheduleEditRequest
	o, err := scheduleRequestObject(p)
	if err != nil {
		return req, err
	}
	if err := p.finish(); err != nil {
		return req, err
	}

	id, ok, err := o.str("id")
	if err != nil {
		return req, err
	}
	if !ok {
		return req, officialBadRequest("缺 id（官方 ScheduleEditRequest.id 必填）")
	}
	req.ID = strings.TrimSpace(id)
	if req.ID == "" {
		return req, officialBadRequest("id 是空串")
	}

	action, ok, err := o.str("action")
	if err != nil {
		return req, err
	}
	if !ok {
		return req, officialBadRequest("缺 action（update 或 delete）")
	}
	switch action {
	case "update", "delete":
		req.Action = action
	default:
		return req, officialBadRequest("action = %q 不在官方枚举里（只有 update / delete）", action)
	}

	// expectedUpdatedAt：字符串是乐观锁，显式 null 等价于没给，别的类型 fail-loud。
	if raw, has := o.take("expectedUpdatedAt"); has {
		if string(raw) != "null" {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return req, officialBadRequest("request.expectedUpdatedAt 必须是字符串或 null")
			}
			req.ExpectedUpdatedAt = &s
		}
	}

	patchObj, hasPatch, err := scheduleObjectField(o, "patch")
	if err != nil {
		return req, err
	}
	switch req.Action {
	case "update":
		if !hasPatch {
			return req, officialBadRequest("action=update 必须给 patch（官方至少一个字段）")
		}
		patch, err := schedulePatchFrom(patchObj)
		if err != nil {
			return req, err
		}
		req.Patch = patch
	case "delete":
		// 官方 delete 分支根本没有 patch 这个键；给了就是拼错了请求。
		if hasPatch {
			return req, officialBadRequest("action=delete 不接受 patch（官方 delete 分支没有这个键）")
		}
	}

	if err := o.finish(); err != nil {
		return req, err
	}
	return req, nil
}

func schedulePatchFrom(obj map[string]json.RawMessage) (*OfficialSchedulePatch, error) {
	o := newOfficialObject("request.patch", obj)
	patch := &OfficialSchedulePatch{}
	count := 0

	if v, ok, err := o.str("title"); err != nil {
		return nil, err
	} else if ok {
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, officialBadRequest("patch.title 是空串")
		}
		if n := utf8.RuneCountInString(trimmed); n > scheduleTitleMaxRunes {
			return nil, officialBadRequest("patch.title 有 %d 个字，官方上限是 %d", n, scheduleTitleMaxRunes)
		}
		patch.Title = &trimmed
		count++
	}

	if v, ok, err := o.str("prompt"); err != nil {
		return nil, err
	} else if ok {
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, officialBadRequest("patch.prompt 是空串")
		}
		patch.Prompt = &trimmed
		count++
	}

	timing, hasTiming, err := scheduleTimingField(o, "timing", false)
	if err != nil {
		return nil, err
	}
	if hasTiming {
		patch.Timing = timing
		count++
	}

	target, hasTarget, err := scheduleTargetField(o, "target")
	if err != nil {
		return nil, err
	}
	if hasTarget {
		patch.Target = target
		count++
	}

	if active, ok, err := o.boolean("active"); err != nil {
		return nil, err
	} else if ok {
		patch.Active = &active
		count++
	}

	if err := o.finish(); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, officialBadRequest("patch 是空的：官方要求至少改一个字段（title/prompt/timing/target/active）")
	}
	return patch, nil
}

// ---------------------------------------------------------------------------
// schedule.view / schedule.run
// ---------------------------------------------------------------------------

func officialScheduleView(api OfficialScheduleAPI, p *officialParams) (any, error) {
	id, err := scheduleIDParam(p)
	if err != nil {
		return nil, err
	}
	if api.View == nil {
		return nil, officialNotImplemented("schedule.view 未接线：宿主没有 AttachOfficialScheduleAPI")
	}
	result, err := api.View(id)
	if err != nil {
		return nil, err
	}
	return checkScheduleResultType(result)
}

func officialScheduleRun(api OfficialScheduleAPI, p *officialParams) (any, error) {
	id, err := scheduleIDParam(p)
	if err != nil {
		return nil, err
	}
	if api.Run == nil {
		return nil, officialNotImplemented("schedule.run 未接线：宿主没有 AttachOfficialScheduleAPI")
	}
	result, err := api.Run(id)
	if err != nil {
		return nil, err
	}
	return checkScheduleResultType(result)
}

// scheduleIDParam 读 view/run 的 {id}。
func scheduleIDParam(p *officialParams) (string, error) {
	o, err := scheduleRequestObject(p)
	if err != nil {
		return "", err
	}
	if err := p.finish(); err != nil {
		return "", err
	}
	id, ok, err := o.str("id")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", officialBadRequest("缺 id（官方 request.id 必填）")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", officialBadRequest("id 是空串")
	}
	if err := o.finish(); err != nil {
		return "", err
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// schedule.history
// ---------------------------------------------------------------------------

func officialScheduleHistory(api OfficialScheduleAPI, p *officialParams) (any, error) {
	req, err := scheduleHistoryRequest(p)
	if err != nil {
		return nil, err
	}
	if api.History == nil {
		return nil, officialNotImplemented("schedule.history 未接线：宿主没有 AttachOfficialScheduleAPI")
	}
	result, err := api.History(req)
	if err != nil {
		return nil, err
	}
	return checkScheduleResultType(result)
}

func scheduleHistoryRequest(p *officialParams) (OfficialScheduleHistoryRequest, error) {
	var req OfficialScheduleHistoryRequest
	o, err := scheduleRequestObject(p)
	if err != nil {
		return req, err
	}
	if err := p.finish(); err != nil {
		return req, err
	}

	id, ok, err := o.str("id")
	if err != nil {
		return req, err
	}
	if !ok {
		return req, officialBadRequest("缺 id（官方 ScheduleHistoryRequest.id 必填）")
	}
	req.ID = strings.TrimSpace(id)
	if req.ID == "" {
		return req, officialBadRequest("id 是空串")
	}

	limit, ok, err := o.number("limit")
	if err != nil {
		return req, err
	}
	if !ok {
		return req, officialBadRequest("缺 limit（官方 ScheduleHistoryRequest.limit 必填）")
	}
	if limit < scheduleHistoryLimitMin || limit > scheduleHistoryLimitMax {
		return req, officialBadRequest("limit = %d 越界：官方是 1 到 %d 的安全整数",
			limit, scheduleHistoryLimitMax)
	}
	req.Limit = int(limit)

	if before, ok, err := o.str("before"); err != nil {
		return req, err
	} else if ok {
		req.Before = before
	}

	if err := o.finish(); err != nil {
		return req, err
	}
	return req, nil
}

// ---------------------------------------------------------------------------
// 参数对象
// ---------------------------------------------------------------------------

// scheduleRequestObject 取官方唯一的参数 `request`。
//
// 官方 wire 是位置参数数组，本仓 /api 的 body 是一个裸对象，所以按官方
// **参数名**取值：`{"request":{…}}`，也接受把 request 的字段平铺在顶层。
// 两种都给算冲突（不猜哪个是真的）。
func scheduleRequestObject(p *officialParams) (*officialObject, error) {
	nested, hasNested, err := p.object("request")
	if err != nil {
		return nil, err
	}
	if hasNested {
		if len(p.m) > 1 {
			return nil, officialBadRequest("request 与顶层字段不能同时给（官方参数是单个 request 对象）")
		}
		return newOfficialObject("request", nested), nil
	}
	flat := make(map[string]json.RawMessage, len(p.m))
	for k, v := range p.m {
		flat[k] = v
		p.used[k] = true
	}
	return newOfficialObject("请求体", flat), nil
}

// scheduleObjectField 从对象里取一个嵌套对象。
func scheduleObjectField(o *officialObject, name string) (map[string]json.RawMessage, bool, error) {
	raw, ok := o.take(name)
	if !ok {
		return nil, false, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false, officialBadRequest("%s.%s 必须是 JSON 对象", o.owner, name)
	}
	return obj, true, nil
}

// scheduleArrayField 从对象里取一个数组。
func scheduleArrayField(o *officialObject, name string) ([]json.RawMessage, bool, error) {
	raw, ok := o.take(name)
	if !ok {
		return nil, false, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false, officialBadRequest("%s.%s 必须是 JSON 数组", o.owner, name)
	}
	return arr, true, nil
}

// ---------------------------------------------------------------------------
// timing / target
// ---------------------------------------------------------------------------

func scheduleTimingField(o *officialObject, name string, required bool) (*OfficialScheduleTiming, bool, error) {
	obj, ok, err := scheduleObjectField(o, name)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	timing, err := scheduleTimingFrom(o.owner+"."+name, obj)
	if err != nil {
		return nil, false, err
	}
	return timing, true, nil
}

// scheduleTimingFrom 解析官方 timing union 的 7 个分支。
func scheduleTimingFrom(owner string, obj map[string]json.RawMessage) (*OfficialScheduleTiming, error) {
	o := newOfficialObject(owner, obj)
	kind, ok, err := o.str("kind")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("缺 %s.kind（after/at/every/daily/weekly/cron/rrule）", owner)
	}
	timing := &OfficialScheduleTiming{Kind: kind}

	switch kind {
	case "after":
		secs, ok, err := o.number("after_seconds")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, officialBadRequest("%s.after_seconds 必填", owner)
		}
		if secs <= 0 {
			return nil, officialBadRequest("%s.after_seconds 必须是正整数秒（got %d）", owner, secs)
		}
		timing.AfterSeconds = secs

	case "at":
		raw, has := o.take("at")
		if !has {
			return nil, officialBadRequest("%s.at 必填（字符串或 {date,time,time_zone}）", owner)
		}
		at, err := scheduleAtFrom(owner, raw)
		if err != nil {
			return nil, err
		}
		timing.At = at

	case "every":
		secs, ok, err := o.number("every_seconds")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, officialBadRequest("%s.every_seconds 必填", owner)
		}
		if secs < scheduleMinEverySeconds {
			return nil, officialBadRequest("%s.every_seconds = %d 太短：本宿主下限是 %d 秒",
				owner, secs, scheduleMinEverySeconds)
		}
		timing.EverySeconds = secs

	case "daily":
		inner, ok, err := scheduleObjectField(o, "daily")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, officialBadRequest("%s.daily 必填（{time,time_zone}）", owner)
		}
		do := newOfficialObject(owner+".daily", inner)
		clock, err := scheduleClockValue(owner+".daily", do)
		if err != nil {
			return nil, err
		}
		zone, err := scheduleZoneValue(owner+".daily", do)
		if err != nil {
			return nil, err
		}
		if err := do.finish(); err != nil {
			return nil, err
		}
		timing.Daily = &OfficialScheduleDailyTime{Time: clock, TimeZone: zone}

	case "weekly":
		inner, ok, err := scheduleObjectField(o, "weekly")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, officialBadRequest("%s.weekly 必填（{time,time_zone,weekdays}）", owner)
		}
		wo := newOfficialObject(owner+".weekly", inner)
		clock, err := scheduleClockValue(owner+".weekly", wo)
		if err != nil {
			return nil, err
		}
		zone, err := scheduleZoneValue(owner+".weekly", wo)
		if err != nil {
			return nil, err
		}
		weekdays, err := scheduleWeekdays(owner+".weekly", wo)
		if err != nil {
			return nil, err
		}
		if err := wo.finish(); err != nil {
			return nil, err
		}
		timing.Weekly = &OfficialScheduleWeeklyTime{Time: clock, TimeZone: zone, Weekdays: weekdays}

	case "cron":
		inner, ok, err := scheduleObjectField(o, "cron")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, officialBadRequest("%s.cron 必填（{expression,time_zone}）", owner)
		}
		co := newOfficialObject(owner+".cron", inner)
		expr, ok, err := co.str("expression")
		if err != nil {
			return nil, err
		}
		if !ok || strings.TrimSpace(expr) == "" {
			return nil, officialBadRequest("%s.cron.expression 必填且非空", owner)
		}
		zone, ok, err := co.str("time_zone")
		if err != nil {
			return nil, err
		}
		if !ok || strings.TrimSpace(zone) == "" {
			return nil, officialBadRequest("%s.cron.time_zone 必填且非空", owner)
		}
		if err := co.finish(); err != nil {
			return nil, err
		}
		timing.Cron = &OfficialScheduleCron{Expression: strings.TrimSpace(expr), TimeZone: strings.TrimSpace(zone)}

	case "rrule":
		rule, ok, err := o.str("rrule")
		if err != nil {
			return nil, err
		}
		if !ok || strings.TrimSpace(rule) == "" {
			return nil, officialBadRequest("%s.rrule 必填且非空", owner)
		}
		timing.RRule = strings.TrimSpace(rule)

	default:
		return nil, officialBadRequest("%s.kind = %q 不在官方枚举里（after/at/every/daily/weekly/cron/rrule）",
			owner, kind)
	}

	if err := o.finish(); err != nil {
		return nil, err
	}
	return timing, nil
}

// scheduleAtFrom 解析 at 的两种写法：RFC3339 字符串，或 {date,time,time_zone}。
func scheduleAtFrom(owner string, raw json.RawMessage) (*OfficialScheduleAt, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, officialBadRequest("%s.at 必须是字符串或对象", owner)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, officialBadRequest("%s.at 是空串", owner)
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return nil, officialBadRequest("%s.at = %q 不是 RFC3339 时刻: %v", owner, s, err)
		}
		return &OfficialScheduleAt{Instant: s}, nil
	}

	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw, &inner); err != nil || inner == nil {
		return nil, officialBadRequest("%s.at 必须是 RFC3339 字符串或 {date,time,time_zone} 对象", owner)
	}
	ao := newOfficialObject(owner+".at", inner)
	date, ok, err := ao.str("date")
	if err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(date) == "" {
		return nil, officialBadRequest("%s.at.date 必填且非空", owner)
	}
	clock, ok, err := ao.str("time")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("%s.at.time 必填", owner)
	}
	if _, _, _, ok := scheduleParseClock(clock); !ok {
		return nil, officialBadRequest("%s.at.time = %q 不是 HH:MM[:SS[.SSS]]", owner, clock)
	}
	zone, ok, err := ao.str("time_zone")
	if err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(zone) == "" {
		return nil, officialBadRequest("%s.at.time_zone 必填且非空", owner)
	}
	if err := ao.finish(); err != nil {
		return nil, err
	}
	return &OfficialScheduleAt{
		Date: strings.TrimSpace(date), Clock: strings.TrimSpace(clock), TimeZone: strings.TrimSpace(zone),
	}, nil
}

// scheduleClockValue 读并校验 {time}（官方墙钟写法 HH:MM[:SS][.SSS]）。
func scheduleClockValue(owner string, o *officialObject) (string, error) {
	clock, ok, err := o.str("time")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", officialBadRequest("%s.time 必填", owner)
	}
	if _, _, _, ok := scheduleParseClock(clock); !ok {
		return "", officialBadRequest("%s.time = %q 不是 HH:MM[:SS[.SSS]]", owner, clock)
	}
	return strings.TrimSpace(clock), nil
}

// scheduleZoneValue 读 {time_zone}。值域校验（本宿主只支持本地时区）由宿主做：
// transport 只负责"这是不是一个非空字符串"。
func scheduleZoneValue(owner string, o *officialObject) (string, error) {
	zone, ok, err := o.str("time_zone")
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(zone) == "" {
		return "", officialBadRequest("%s.time_zone 必填且非空", owner)
	}
	return strings.TrimSpace(zone), nil
}

// scheduleWeekdays 读 weekly.weekdays：非空、1..7、不重复（官方 ISO 周一到周日）。
func scheduleWeekdays(owner string, o *officialObject) ([]int, error) {
	arr, ok, err := scheduleArrayField(o, "weekdays")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("%s.weekdays 必填（非空 ISO 星期集合，周一=1 … 周日=7）", owner)
	}
	if len(arr) == 0 {
		return nil, officialBadRequest("%s.weekdays 是空数组（官方要求非空）", owner)
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(arr))
	for i, raw := range arr {
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, officialBadRequest("%s.weekdays[%d] 必须是数字", owner, i)
		}
		if f != float64(int(f)) {
			return nil, officialBadRequest("%s.weekdays[%d] 必须是整数", owner, i)
		}
		d := int(f)
		if d < 1 || d > 7 {
			return nil, officialBadRequest("%s.weekdays[%d] = %d 越界：官方是 1..7", owner, i, d)
		}
		if seen[d] {
			return nil, officialBadRequest("%s.weekdays 里 %d 重复（官方不允许重复）", owner, d)
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, nil
}

// scheduleParseClock 解析官方墙钟写法：HH:MM[:SS][.SSS]。
func scheduleParseClock(s string) (hour, minute, second int, ok bool) {
	body := strings.TrimSpace(s)
	if body == "" {
		return 0, 0, 0, false
	}
	if i := strings.IndexByte(body, '.'); i >= 0 {
		frac := body[i+1:]
		body = body[:i]
		if len(frac) < 1 || len(frac) > 3 {
			return 0, 0, 0, false
		}
		for _, r := range frac {
			if r < '0' || r > '9' {
				return 0, 0, 0, false
			}
		}
	}
	parts := strings.Split(body, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		if p == "" || len(p) > 2 {
			return 0, 0, 0, false
		}
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return 0, 0, 0, false
			}
			n = n*10 + int(r-'0')
		}
		nums[i] = n
	}
	hour, minute = nums[0], nums[1]
	if len(nums) == 3 {
		second = nums[2]
	}
	if hour > 23 || minute > 59 || second > 59 {
		return 0, 0, 0, false
	}
	return hour, minute, second, true
}

// scheduleTargetField 解析 target union：session / new_once / new。
func scheduleTargetField(o *officialObject, name string) (*OfficialScheduleTarget, bool, error) {
	obj, ok, err := scheduleObjectField(o, name)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	to := newOfficialObject(name, obj)
	kind, ok, err := to.str("kind")
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, officialBadRequest("缺 %s.kind（session/new_once/new）", name)
	}
	target := &OfficialScheduleTarget{Kind: kind}
	switch kind {
	case "session":
		id, ok, err := to.str("sessionId")
		if err != nil {
			return nil, false, err
		}
		if !ok || strings.TrimSpace(id) == "" {
			return nil, false, officialBadRequest("%s.sessionId 必填且非空（kind=session）", name)
		}
		target.SessionID = strings.TrimSpace(id)
	case "new_once", "new":
		create, hasCreate, err := scheduleObjectField(to, "create")
		if err != nil {
			return nil, false, err
		}
		if hasCreate {
			co := newOfficialObject(name+".create", create)
			info := &OfficialScheduleCreateInfo{}
			if v, ok, err := co.str("cwd"); err != nil {
				return nil, false, err
			} else if ok {
				info.CWD = v
			}
			if v, ok, err := co.str("agentPreset"); err != nil {
				return nil, false, err
			} else if ok {
				info.AgentPreset = v
			}
			if err := co.finish(); err != nil {
				return nil, false, err
			}
			target.Create = info
		}
	default:
		return nil, false, officialBadRequest("%s.kind = %q 不在官方枚举里（session/new_once/new）", name, kind)
	}
	if err := to.finish(); err != nil {
		return nil, false, err
	}
	return target, true, nil
}
