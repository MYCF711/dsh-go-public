package transport

// 本文件是官方桌面端 0.2.2-rc.1「agent 侧」24 个 typert 方法的 L1 语义实现。
//
// 对齐口径（见 docs/official-desktop-api-parity.md §2）：
//   - **L1**：参数与结果**字段**按官方 zod 契约；wire 仍走本仓既有的
//     `POST /api/<method>` + 裸参数对象 + `{"result":…}` / 四字段 error。
//   - **L2**（`remote.mux` 多路复用 + typert Connection envelope）**未实现**，
//     所以本文不声称与官方桌面端 wire 兼容。
//
// 本文件只做协议翻译与形状校验：不读盘、不碰会话日志、不认识 goal 存储
// （红线 R7）。所有真实状态由宿主经 OfficialAgentAPI 注入；某个能力宿主
// 没接线时回**四字段的 not_implemented**，绝不返回假数据。
//
// 接线的唯一入口是 `IsOfficialAgentMethod` + `(*Server).serveOfficialAgentAPI`：
// `cmd/dshd` 侧 `AttachOfficialAgentAPI` 注入实现，`internal/transport/server.go`
// 的 `/api` 路由在既有 switch 之前调用一次即可（见 RESULT 的接线清单）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// 官方方法名。字符串必须与官方 `@deepseek-ai/*` 的 `createTypertRemoteInfo`
// 里 `'goals/get'` 这类键逐字对应（本仓把 `/` 换成 `.`，与既有 58 个方法同形）。
const (
	MethodGoalsGet      Method = "goals.get"
	MethodGoalsCreate   Method = "goals.create"
	MethodGoalsEdit     Method = "goals.edit"
	MethodGoalsPause    Method = "goals.pause"
	MethodGoalsResume   Method = "goals.resume"
	MethodGoalsComplete Method = "goals.complete"
	MethodGoalsClear    Method = "goals.clear"

	MethodCommandsList    Method = "commands.list"
	MethodCommandsExecute Method = "commands.execute"

	MethodSubagentsPrompt            Method = "subagents.prompt"
	MethodSubagentsInterruptByParent Method = "subagents.interruptByParent"

	MethodUserQuestionsAnswer     Method = "userQuestions.answer"
	MethodUserQuestionsAttachWait Method = "userQuestions.attachWait"

	MethodAgentPresetsRead   Method = "agentPresets.read"
	MethodAgentPresetsSelect Method = "agentPresets.select"

	MethodSkillsList         Method = "skills.list"
	MethodFileReferencesList Method = "fileReferences.list"
	MethodFileUploadsUpload  Method = "fileUploads.upload"

	MethodMessageFeedbackList   Method = "messageFeedback.list"
	MethodMessageFeedbackPut    Method = "messageFeedback.put"
	MethodMessageFeedbackDelete Method = "messageFeedback.delete"

	MethodSessionFeedbackRecord              Method = "sessionFeedback.record"
	MethodSessionReferenceResolverCandidates Method = "sessionReferenceResolver.candidates"
	MethodToolResultsWatch                   Method = "toolResults.watch"
)

// OfficialAgentMethods 按命名空间分组返回本文件实现的 24 个方法（顺序固定）。
//
// 它给快照/文档/门禁用：新增或删除一个方法必须显式改这里，不能让
// "方法悄悄多了一个"从路由表里漏过去。
func OfficialAgentMethods() []Method {
	return []Method{
		MethodGoalsGet, MethodGoalsCreate, MethodGoalsEdit, MethodGoalsPause,
		MethodGoalsResume, MethodGoalsComplete, MethodGoalsClear,
		MethodCommandsList, MethodCommandsExecute,
		MethodSubagentsPrompt, MethodSubagentsInterruptByParent,
		MethodUserQuestionsAnswer, MethodUserQuestionsAttachWait,
		MethodAgentPresetsRead, MethodAgentPresetsSelect,
		MethodSkillsList, MethodFileReferencesList, MethodFileUploadsUpload,
		MethodMessageFeedbackList, MethodMessageFeedbackPut, MethodMessageFeedbackDelete,
		MethodSessionFeedbackRecord, MethodSessionReferenceResolverCandidates,
		MethodToolResultsWatch,
	}
}

// IsOfficialAgentMethod 判断一个 /api 路径名是否属于本文件的 24 个方法。
//
// 路由侧只需要这一次判断（**不要**在 server.go 里摊开 24 个 case：那样
// 每加一个官方方法都要动一次早已冻结的路由表）。
func IsOfficialAgentMethod(name string) bool {
	switch Method(name) {
	case MethodGoalsGet, MethodGoalsCreate, MethodGoalsEdit, MethodGoalsPause,
		MethodGoalsResume, MethodGoalsComplete, MethodGoalsClear,
		MethodCommandsList, MethodCommandsExecute,
		MethodSubagentsPrompt, MethodSubagentsInterruptByParent,
		MethodUserQuestionsAnswer, MethodUserQuestionsAttachWait,
		MethodAgentPresetsRead, MethodAgentPresetsSelect,
		MethodSkillsList, MethodFileReferencesList, MethodFileUploadsUpload,
		MethodMessageFeedbackList, MethodMessageFeedbackPut, MethodMessageFeedbackDelete,
		MethodSessionFeedbackRecord, MethodSessionReferenceResolverCandidates,
		MethodToolResultsWatch:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 挂载点
// ---------------------------------------------------------------------------

// OfficialAgentAPI 是宿主侧的真实实现集合。**每个字段都可以为 nil**：
// 为 nil 表示这个能力在**当前宿主**上没有真实支撑，调用回 not_implemented
// 并说清缺什么——不是静默成功，也不是假数据。
//
// 为什么用"每方法一个函数"而不是一个 interface：这些方法分属 7 个官方包，
// 宿主侧的真实来源各不相同（goal 存储 / modes / skill 目录 / 会话日志 /
// 上传存储），硬塞进一个 interface 只会逼调用方写一堆空实现。
type OfficialAgentAPI struct {
	GoalGet      func(sessionID string) (OfficialGoalGetResult, error)
	GoalCreate   func(sessionID string, req OfficialGoalCreateRequest) (OfficialGoalRef, error)
	GoalEdit     func(sessionID string, ref OfficialGoalRef, req OfficialGoalEditRequest) (OfficialGoalView, error)
	GoalPause    func(sessionID string, ref OfficialGoalRef) (OfficialGoalView, error)
	GoalResume   func(sessionID string, ref OfficialGoalRef) (OfficialGoalView, error)
	GoalComplete func(sessionID string, ref OfficialGoalRef) (OfficialGoalView, error)
	GoalClear    func(sessionID string, ref OfficialGoalRef) (OfficialGoalRef, error)

	CommandsList    func(sessionID string) ([]OfficialCommand, error)
	CommandsExecute func(sessionID, line string, attachments []OfficialSubmittedAttachment) (*OfficialCommandExecution, error)

	SubagentsPrompt            func(req OfficialSubagentPromptRequest) (OfficialSubagentPromptReceipt, error)
	SubagentsInterruptByParent func(childSessionID, parentSessionID string) (OfficialAccepted, error)

	UserQuestionsAnswer     func(sessionID, callID string, answer OfficialQuestionAnswer) (bool, error)
	UserQuestionsAttachWait func(sessionID, callID string) (*OfficialQuestionWait, error)

	AgentPresetRead   func(presetID string) (OfficialAgentPresetDocument, error)
	AgentPresetSelect func(sessionID, presetID string) (string, error)

	SkillsList         func(sessionID string) (OfficialSkillCatalog, error)
	FileReferencesList func(sessionID, query string) ([]OfficialFileReference, error)
	FileUploadsUpload  func(sessionID string, req OfficialFileUploadRequest) (OfficialFileUploadValue, error)

	MessageFeedbackList   func(sessionID string) (OfficialMessageFeedbackListResult, error)
	MessageFeedbackPut    func(req OfficialMessageFeedbackPutRequest) (OfficialMessageFeedbackPutResult, error)
	MessageFeedbackDelete func(req OfficialMessageFeedbackDeleteRequest) (OfficialMessageFeedbackDeleteResult, error)

	SessionFeedbackRecord      func(req OfficialSessionFeedbackRequest) (OfficialSessionFeedbackResult, error)
	SessionReferenceCandidates func(sessionID, query string) ([]OfficialSessionReferenceCandidate, error)
	ToolResultsWatch           func(sessionID string, seq int64) (OfficialToolResultWatch, error)
}

// officialAgentMount 把一份实现挂到一个 Server 上。
//
// 为什么不用 `Options` 加字段：Options 在 server.go（本任务的写作用域之外，
// 由 Lead 统一接线）。这里用按 *Server 索引的表，隔离性与"每个 Server 一份
// 实现"完全一致：两个 Server（A/B）各挂各的，互不可见，也不会有进程级单例
// 被两个宿主共享。实现方用完可以 Detach 释放条目（测试里都 defer 了）。
var officialAgentMount = struct {
	mu sync.RWMutex
	m  map[*Server]OfficialAgentAPI
}{m: map[*Server]OfficialAgentAPI{}}

// AttachOfficialAgentAPI 给这个 Server 挂上官方 agent API 的真实实现。
// 必须发生在服务开始接受 /api 调用之前（serve 的装配阶段）。
func (s *Server) AttachOfficialAgentAPI(api OfficialAgentAPI) {
	if s == nil {
		return
	}
	officialAgentMount.mu.Lock()
	defer officialAgentMount.mu.Unlock()
	officialAgentMount.m[s] = api
}

// DetachOfficialAgentAPI 摘掉实现（测试收尾用；生产不需要调）。
func (s *Server) DetachOfficialAgentAPI() {
	if s == nil {
		return
	}
	officialAgentMount.mu.Lock()
	defer officialAgentMount.mu.Unlock()
	delete(officialAgentMount.m, s)
}

func (s *Server) officialAgentAPI() (OfficialAgentAPI, bool) {
	if s == nil {
		return OfficialAgentAPI{}, false
	}
	officialAgentMount.mu.RLock()
	defer officialAgentMount.mu.RUnlock()
	api, ok := officialAgentMount.m[s]
	return api, ok
}

// ---------------------------------------------------------------------------
// 错误契约
// ---------------------------------------------------------------------------

// ErrOfficialNotImplemented 由宿主返回，表示"这个能力在当前宿主上没有真实支撑"。
// Transport 据此回 501 + not_implemented（四字段），不吞成成功。
var ErrOfficialNotImplemented = errors.New("官方 agent API 能力未实现")

// ErrOfficialBadInput 由宿主返回，表示参数形状合法但**取值**不合法
// （未知 preset、问题不存在、路径逃出工作目录…）。回 400 bad_request。
var ErrOfficialBadInput = errors.New("官方 agent API 参数不合法")

// ErrOfficialConflict 由宿主返回，表示真实状态不允许这次迁移
// （goal 版本过期、phase 不允许、已有未完成 goal…）。回 409 revision_conflict。
var ErrOfficialConflict = errors.New("官方 agent API 状态冲突")

// officialError 是本文内部携带"HTTP 状态 + 稳定码"的错误类型。
// 它只在本包内使用；对外仍然收敛到 errorObject 那一个四字段构造点。
type officialError struct {
	status int
	code   string
	msg    string
}

func (e *officialError) Error() string { return e.msg }

// Is 让 officialError 与既有哨兵错误**互通**：
//
//	errors.Is(err, ErrOfficialNotImplemented)   // 501 未实现
//	errors.Is(err, ErrOfficialBadInput)         // 400
//	errors.Is(err, ErrNoSuchSession)            // 400 no_such_session
//	errors.Is(err, ErrOfficialConflict)         // 409
//
// 为什么需要：调用方（宿主、测试）习惯按哨兵判断"是不是未实现"，而
// writeOfficialError 按 code 出四字段。两套判据必须能互相翻译，否则
// "同一个错误两种问法"会得出不同结论。
func (e *officialError) Is(target error) bool {
	switch e.code {
	case "not_implemented":
		return target == ErrOfficialNotImplemented
	case "bad_request":
		return target == ErrOfficialBadInput
	case "no_such_session":
		return target == ErrNoSuchSession
	case "revision_conflict":
		return target == ErrOfficialConflict
	}
	return false
}

func officialBadRequest(format string, args ...any) error {
	return &officialError{status: http.StatusBadRequest, code: "bad_request", msg: fmt.Sprintf(format, args...)}
}

func officialNotImplemented(format string, args ...any) error {
	return &officialError{status: http.StatusNotImplemented, code: "not_implemented", msg: fmt.Sprintf(format, args...)}
}

func officialInternal(format string, args ...any) error {
	return &officialError{status: http.StatusInternalServerError, code: "internal", msg: fmt.Sprintf(format, args...)}
}

func officialNotFound(format string, args ...any) error {
	return &officialError{status: http.StatusNotFound, code: "not_found", msg: fmt.Sprintf(format, args...)}
}

func officialNoSuchSession(sessionID string) error {
	return &officialError{
		status: http.StatusBadRequest, code: "no_such_session",
		msg: fmt.Sprintf("没有这个会话：%q", sessionID),
	}
}

func officialRevisionConflict(format string, args ...any) error {
	return &officialError{status: http.StatusConflict, code: "revision_conflict", msg: fmt.Sprintf(format, args...)}
}

// writeOfficialError 是官方 agent 方法的唯一错误出口。
//
// 码只取本仓既有的那几个（bad_request / not_implemented / not_found /
// no_such_session / revision_conflict / internal / cross_session），
// 因而 errorHint 与 retryableFor 不需要新增分支。
func writeOfficialError(w http.ResponseWriter, err error) {
	var typed *officialError
	if errors.As(err, &typed) {
		writeJSON(w, typed.status, errorBodyFor(typed.code, typed.msg))
		return
	}
	if errors.Is(err, ErrOfficialNotImplemented) {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", err.Error()))
		return
	}
	if errors.Is(err, ErrOfficialBadInput) {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	if errors.Is(err, ErrOfficialConflict) {
		writeJSON(w, http.StatusConflict, errorBodyFor("revision_conflict", err.Error()))
		return
	}
	if errors.Is(err, ErrNoSuchSession) {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("no_such_session", err.Error()))
		return
	}
	if errors.Is(err, ErrStatePersistence) {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
		return
	}
	if errors.Is(err, ErrGoalTransition) {
		writeJSON(w, http.StatusConflict, errorBodyFor("revision_conflict", err.Error()))
		return
	}
	// 没有类型的错误一律按宿主内部问题报：它绝不能被渲染成成功。
	writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
}

// ---------------------------------------------------------------------------
// 参数解析
//
// 官方方法的参数是**位置参数数组**（typert `parameters: [...]`），而本仓 L1
// 的 wire 是一个裸 JSON 对象。这里按官方**参数名**取值：`agent` 位置的参数
// 官方 wire 名是 `agentId`，其余用官方 `name`（`ref`/`request`/`line`/
// `callId`/`answer`/`query`/`agentPreset`/`childSessionId`/…）。
//
// 校验是 fail-loud 的：
//   - 不是 JSON 对象 → 400；
//   - 未知字段 → 400 并指出字段名（官方的 strictCodec 是
//     `additionalProperties:false`，不认识的键本来就会被拒）；
//   - 缺必填 → 400 并指出名字；
//   - 类型不对 → 400。
//
// 对象型参数（`ref` / `request` / `answer`）允许两种写法：嵌套
// `{"request":{"objective":"…"}}`，或把字段平铺在顶层。两种都给算冲突。
type officialParams struct {
	m    map[string]json.RawMessage
	used map[string]bool
}

func decodeOfficialParams(r *http.Request) (*officialParams, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, officialBadRequest("读请求体失败: %v", err)
	}
	if len(raw) == 0 {
		return nil, officialBadRequest("请求体为空：这些方法要一个 JSON 对象")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, officialBadRequest("请求体不是 JSON 对象: %v", err)
	}
	if m == nil {
		return nil, officialBadRequest("请求体是 null：这些方法要一个 JSON 对象")
	}
	return &officialParams{m: m, used: map[string]bool{}}, nil
}

func (p *officialParams) take(name string) (json.RawMessage, bool) {
	raw, ok := p.m[name]
	if ok {
		p.used[name] = true
	}
	return raw, ok
}

func (p *officialParams) has(name string) bool {
	_, ok := p.m[name]
	return ok
}

// finish 在解析结束时调用：把没用到的键当成未知字段拒掉。
func (p *officialParams) finish() error {
	for k := range p.m {
		if !p.used[k] {
			return officialBadRequest("不认识的字段 %q（官方 strict 契约不接受 additionalProperties）", k)
		}
	}
	return nil
}

// str 取一个必填字符串。
func (p *officialParams) str(name string) (string, error) {
	raw, ok := p.take(name)
	if !ok {
		return "", officialBadRequest("缺 %s", name)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", officialBadRequest("%s 必须是字符串", name)
	}
	return s, nil
}

// strAlias 取必填字符串，接受若干别名（官方 wire 名在前）。
// 别名之间互为替代，同时给两个且不同 → 400（不猜哪个是真的）。
//
// 别名存在的理由：官方用 `agentId` 指会话，本仓既有 58 个方法用 `sessionId`
// 或 `id`。三种都认，界面上就不会因为换了个名字而 400。
func (p *officialParams) strAlias(names ...string) (string, string, error) {
	found := ""
	which := ""
	for _, name := range names {
		raw, ok := p.take(name)
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", "", officialBadRequest("%s 必须是字符串", name)
		}
		if found != "" && s != found {
			return "", "", officialBadRequest("%s 与其他别名冲突", name)
		}
		found, which = s, name
	}
	if which == "" {
		return "", "", officialBadRequest("缺 %s", names[0])
	}
	return found, which, nil
}

// sessionRef 取会话 id（官方 wire 名 agentId；兼容 sessionId / id）。
// 空串一律 400：没有会话就没有作用域，不能拿进程默认值顶上。
func (p *officialParams) sessionRef() (string, error) {
	id, _, err := p.strAlias("agentId", "sessionId", "id")
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", officialBadRequest("会话 id 是空串")
	}
	return id, nil
}

func (p *officialParams) object(name string) (map[string]json.RawMessage, bool, error) {
	raw, ok := p.take(name)
	if !ok {
		return nil, false, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false, officialBadRequest("%s 必须是 JSON 对象", name)
	}
	return obj, true, nil
}

func (p *officialParams) array(name string) ([]json.RawMessage, bool, error) {
	raw, ok := p.take(name)
	if !ok {
		return nil, false, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false, officialBadRequest("%s 必须是 JSON 数组", name)
	}
	return arr, true, nil
}

// objectField 从对象里取一个字段，并把它标记为"这个对象里认识的键"。
type officialObject struct {
	owner string
	m     map[string]json.RawMessage
	used  map[string]bool
}

func newOfficialObject(owner string, m map[string]json.RawMessage) *officialObject {
	return &officialObject{owner: owner, m: m, used: map[string]bool{}}
}

func (o *officialObject) take(name string) (json.RawMessage, bool) {
	raw, ok := o.m[name]
	if ok {
		o.used[name] = true
	}
	return raw, ok
}

func (o *officialObject) has(name string) bool {
	_, ok := o.m[name]
	return ok
}

func (o *officialObject) finish() error {
	for k := range o.m {
		if !o.used[k] {
			return officialBadRequest("%s 里不认识的字段 %q", o.owner, k)
		}
	}
	return nil
}

func (o *officialObject) str(name string) (string, bool, error) {
	raw, ok := o.take(name)
	if !ok {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, officialBadRequest("%s.%s 必须是字符串", o.owner, name)
	}
	return s, true, nil
}

func (o *officialObject) number(name string) (int64, bool, error) {
	raw, ok := o.take(name)
	if !ok {
		return 0, false, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false, officialBadRequest("%s.%s 必须是数字", o.owner, name)
	}
	if f != float64(int64(f)) {
		return 0, false, officialBadRequest("%s.%s 必须是整数", o.owner, name)
	}
	return int64(f), true, nil
}

func (o *officialObject) boolean(name string) (bool, bool, error) {
	raw, ok := o.take(name)
	if !ok {
		return false, false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false, officialBadRequest("%s.%s 必须是布尔", o.owner, name)
	}
	return b, true, nil
}

// ---------------------------------------------------------------------------
// 官方的数据结构（字段名/可选性逐条照 zod 契约）
// ---------------------------------------------------------------------------

// OfficialGoalRef 是 goals 的稳定身份 + 乐观并发版本号。
type OfficialGoalRef struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// OfficialGoalBlockedReason 只在 phase=blocked 时出现。
type OfficialGoalBlockedReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// OfficialGoalView 是官方 `GoalView`：goal 快照 + channel 级计数与时间。
//
// createdAt/updatedAt 是**毫秒时间戳**，来自真实持久记录；没有真实来源时
// （例如旧扁平格式迁移出来的 goal 从未写过 goal/change 帧）宿主必须报错，
// 不能编一个 now。
type OfficialGoalView struct {
	ID            string                     `json:"id"`
	Revision      int64                      `json:"revision"`
	Objective     string                     `json:"objective"`
	Phase         string                     `json:"phase"`
	MaxGoalRounds int                        `json:"maxGoalRounds"`
	RoundsStarted int                        `json:"roundsStarted"`
	CreatedAt     int64                      `json:"createdAt"`
	UpdatedAt     int64                      `json:"updatedAt"`
	Activation    string                     `json:"activation"`
	BlockedReason *OfficialGoalBlockedReason `json:"blockedReason,omitempty"`
}

// OfficialGoalGetResult 承载官方 `goals/get` 的 `GoalView | undefined`。
//
// **与官方 wire 的一处显式差异**：本仓 `/api` 的成功信封不允许
// `{"result":null}`（出口会把 null 变成 500），因此"没有当前 goal"用
// `{"goal":null}` 表达——与既有 `session.goal.get` 的回包同形。
type OfficialGoalGetResult struct {
	Goal *OfficialGoalView `json:"goal"`
}

// OfficialGoalCreateRequest 是官方 `CreateGoalRequest`。
type OfficialGoalCreateRequest struct {
	Objective     string
	MaxGoalRounds *int
}

// OfficialGoalEditRequest 是官方 `EditGoalRequest`：至少一个字段。
type OfficialGoalEditRequest struct {
	Objective     *string
	MaxGoalRounds *int
}

// OfficialCommandInput 是命令对客户端的输入提示。
type OfficialCommandInput struct {
	Hint        string `json:"hint"`
	Attachments *bool  `json:"attachments,omitempty"`
}

// OfficialCommand 是 `commands.list` 的一行。
type OfficialCommand struct {
	DefinitionID string                `json:"definitionId,omitempty"`
	Name         string                `json:"name"`
	Description  string                `json:"description"`
	Input        *OfficialCommandInput `json:"input,omitempty"`
}

// OfficialSubmittedAttachment 是 `commands.execute` 里随行提交的附件。
type OfficialSubmittedAttachment struct {
	Type      string `json:"type"`
	MediaType string `json:"mediaType,omitempty"`
	Data      string `json:"data,omitempty"`
	Name      string `json:"name,omitempty"`
	ReceiptID string `json:"receiptId,omitempty"`
}

// OfficialCommandOutcome 是命令的归一化结果。
type OfficialCommandOutcome struct {
	Kind           string `json:"kind"`
	Text           string `json:"text,omitempty"`
	SourceEventSeq *int64 `json:"sourceEventSeq,omitempty"`
}

// OfficialCommandExecution 是 `commands.execute` 的结果。
type OfficialCommandExecution struct {
	CommandID string                 `json:"commandId"`
	Result    OfficialCommandOutcome `json:"result"`
}

// OfficialSubagentPromptContent 是子代理消息的一段内容。
type OfficialSubagentPromptContent struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Data      string `json:"data,omitempty"`
	Name      string `json:"name,omitempty"`
}

// OfficialSubagentPromptRequest 是官方 `SubagentPromptRequest`。
type OfficialSubagentPromptRequest struct {
	RequestID       string
	ParentSessionID string
	ChildSessionID  string
	Mode            string
	Delivery        string
	Content         []OfficialSubagentPromptContent
	ClientTimeZone  string
}

// OfficialSubagentPromptReceipt 是消息被 inbox 接受后的身份。
type OfficialSubagentPromptReceipt struct {
	MessageID string `json:"messageId"`
}

// OfficialAccepted 是"取消信号已被接受"的回执（不代表目标已静止）。
type OfficialAccepted struct {
	Accepted bool `json:"accepted"`
}

// OfficialQuestionAnswerItem 是单个问题的答案。
type OfficialQuestionAnswerItem struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

// OfficialQuestionAnswer 是整批答案（一个问题一项）。
type OfficialQuestionAnswer struct {
	Answers []OfficialQuestionAnswerItem `json:"answers"`
}

// OfficialQuestionWait 是答案 UI 能占用的剩余等待时长。
type OfficialQuestionWait struct {
	RemainingMs int64 `json:"remainingMs"`
}

// OfficialAgentPreset 是预设清单的一行。
type OfficialAgentPreset struct {
	ID          string `json:"id"`
	IsDefault   bool   `json:"isDefault"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Broken      string `json:"broken,omitempty"`
}

// OfficialAgentPresetRoster 是预设清单。
type OfficialAgentPresetRoster struct {
	Presets []OfficialAgentPreset `json:"presets"`
}

// OfficialAgentPresetDocument 是某个预设的声明正文 + 已发布元数据。
type OfficialAgentPresetDocument struct {
	AgentPreset string `json:"agentPreset"`
	Content     string `json:"content"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// OfficialSkill 是一个可被用户调用的 skill 的元数据（不含正文）。
type OfficialSkill struct {
	Path           string `json:"path,omitempty"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	WhenToUse      string `json:"whenToUse,omitempty"`
	ModelInvocable bool   `json:"modelInvocable"`
}

// OfficialSkillCatalog 是 skill 清单。
type OfficialSkillCatalog struct {
	Skills []OfficialSkill `json:"skills"`
}

// OfficialFileReference 是 `@` 引用补全的一项。
type OfficialFileReference struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// OfficialFileUploadRequest 是一次编码上传。
type OfficialFileUploadRequest struct {
	Data string
	Name string
}

// OfficialUploadedFile 是内容寻址存储里的文件引用。
type OfficialUploadedFile struct {
	AttachmentID string `json:"attachmentId"`
	Name         string `json:"name"`
	Bytes        int64  `json:"bytes"`
}

// OfficialFileUploadValue 是"已暂存回执 + 持久文件引用"。
type OfficialFileUploadValue struct {
	ReceiptID string               `json:"receiptId"`
	File      OfficialUploadedFile `json:"file"`
}

// OfficialMessageFeedbackItem 是一条消息反馈的不可变快照。
type OfficialMessageFeedbackItem struct {
	MessageID string `json:"messageId"`
	Rating    string `json:"rating"`
	Note      string `json:"note,omitempty"`
	Category  string `json:"category,omitempty"`
	Version   string `json:"version"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// OfficialFeedbackFailure 是反馈类方法的业务失败体。
//
// Current 用 RawMessage：`version-conflict` 要求 `current` **存在且可为 null**，
// 而别的码不该带这个键——所以只有冲突时把它设成 `null` 或真实快照。
type OfficialFeedbackFailure struct {
	Code        string          `json:"code"`
	SessionID   string          `json:"sessionId,omitempty"`
	MessageID   string          `json:"messageId,omitempty"`
	Current     json.RawMessage `json:"current,omitempty"`
	MaxBytes    *int64          `json:"maxBytes,omitempty"`
	ActualBytes *int64          `json:"actualBytes,omitempty"`
}

// OfficialMessageFeedbackListResult 是 `list` 的 union（ok/value | ok/error）。
type OfficialMessageFeedbackListResult struct {
	OK    bool                          `json:"ok"`
	Value *OfficialMessageFeedbackValue `json:"value,omitempty"`
	Error *OfficialFeedbackFailure      `json:"error,omitempty"`
}

// OfficialMessageFeedbackValue 包一层 items（官方就是这个形状）。
type OfficialMessageFeedbackValue struct {
	Items []OfficialMessageFeedbackItem `json:"items"`
}

// OfficialMessageFeedbackPutRequest 是 `put` 的入参。
type OfficialMessageFeedbackPutRequest struct {
	SessionID string
	MessageID string
	Rating    string
	Note      string
	Category  string
	// IfVersion 为 nil 表示 null（要求当前不存在）；非 nil 是观察到的版本号。
	IfVersion *string
}

// OfficialMessageFeedbackPutResult 是 `put` 的结果。
type OfficialMessageFeedbackPutResult struct {
	OK    bool                         `json:"ok"`
	Value *OfficialMessageFeedbackItem `json:"value,omitempty"`
	Error *OfficialFeedbackFailure     `json:"error,omitempty"`
}

// OfficialMessageFeedbackDeleteRequest 是 `delete` 的入参。
type OfficialMessageFeedbackDeleteRequest struct {
	SessionID string
	MessageID string
	IfVersion string
}

// OfficialMessageFeedbackDeleteValue 是删除成功的稳定后置条件。
type OfficialMessageFeedbackDeleteValue struct {
	Absent bool `json:"absent"`
}

// OfficialMessageFeedbackDeleteResult 是 `delete` 的结果。
type OfficialMessageFeedbackDeleteResult struct {
	OK    bool                                `json:"ok"`
	Value *OfficialMessageFeedbackDeleteValue `json:"value,omitempty"`
	Error *OfficialFeedbackFailure            `json:"error,omitempty"`
}

// 官方反馈码表（消息反馈与命令反馈共用）。
const (
	OfficialFeedbackOther                     = "other"
	OfficialFeedbackTaskResult                = "task-result"
	OfficialFeedbackInstructionFollowing      = "instruction-following"
	OfficialFeedbackProductInteraction        = "product-interaction"
	OfficialFeedbackServiceStability          = "service-stability"
	OfficialFeedbackResourceCost              = "resource-cost"
	OfficialFeedbackSecurityPrivacyPermission = "security-privacy-permission"
)

// OfficialSessionFeedbackRequest 是 `sessionFeedback.record` 的入参。
type OfficialSessionFeedbackRequest struct {
	SessionID string
	Text      string
	Category  string
}

// OfficialSessionFeedbackRecorded 是"已追加"的后置条件。
type OfficialSessionFeedbackRecorded struct {
	Recorded bool `json:"recorded"`
}

// OfficialSessionFeedbackResult 是 `sessionFeedback.record` 的结果。
type OfficialSessionFeedbackResult struct {
	OK    bool                             `json:"ok"`
	Value *OfficialSessionFeedbackRecorded `json:"value,omitempty"`
	Error *OfficialFeedbackFailure         `json:"error,omitempty"`
}

// OfficialSessionReferenceCandidate 是一条会话引用候选。
type OfficialSessionReferenceCandidate struct {
	Mention       string `json:"mention"`
	SessionID     string `json:"sessionId"`
	Label         string `json:"label"`
	DisplayTitle  string `json:"displayTitle,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	SameWorkspace bool   `json:"sameWorkspace"`
	CreatedAt     int64  `json:"createdAt"`
}

// OfficialToolResultWatch 是 `toolResults.watch` 的一次读数。
//
// 官方是 **stream**：值会随结果被淘汰或会话实例离开注册表而消失。
// L1 阶段只做**一次性读取**（不订阅后续变化、不假装流式），宿主返回
// `available:false` 就是官方的"cold read stays unavailable"。
type OfficialToolResultWatch struct {
	Available bool            `json:"available"`
	Value     json.RawMessage `json:"value,omitempty"`
}

// ---------------------------------------------------------------------------
// 分发
// ---------------------------------------------------------------------------

// serveOfficialAgentAPI 处理本文件的 24 个方法。
//
// 调用方（server.go 的 /api 路由）只需在既有 switch 之前判断
// `IsOfficialAgentMethod(name)` 并调用它；认证、`POST` 校验、失败日志都已经
// 在 handleAPI 里做过了。
func (s *Server) serveOfficialAgentAPI(w http.ResponseWriter, r *http.Request, name string) {
	// 流式方法（`toolResults.watch`）走 NDJSON 长连接：**先于** unary 路由。
	// 官方它是 `mode:'stream'`（值会随结果淘汰而消失），一次性响应只能表达
	// "这一刻可不可用"，表达不了"之后变了"。
	if IsOfficialStreamMethod(name) {
		s.serveOfficialStream(w, r, name)
		return
	}
	api, ok := s.officialAgentAPI()
	if !ok {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
			"官方 agent API 未接线：宿主没有 AttachOfficialAgentAPI"))
		return
	}
	params, err := decodeOfficialParams(r)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	result, err := routeOfficialAgentMethod(api, Method(name), params)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	if result == nil {
		// 官方 `commands.execute` 在语法/名字未解析时返回 undefined。
		// 本仓出口不接受 `{"result":null}`（会变成 500），所以用**空 result 对象**
		// 表达"这次调用没有载荷"：客户端据此与"真执行了"（有 commandId/result）区分。
		// 该差异登记在 RESULT 的"与官方 wire 的显式差异"里。
		result = map[string]any{}
	}
	writeJSON(w, http.StatusOK, result)
}

// routeOfficialAgentMethod 是一个大 switch，放在本文件里而不是 server.go：
// 官方方法还会继续加，路由表却是冻结的。
func routeOfficialAgentMethod(api OfficialAgentAPI, method Method, p *officialParams) (any, error) {
	switch method {
	case MethodGoalsGet:
		return officialGoalsGet(api, p)
	case MethodGoalsCreate:
		return officialGoalsCreate(api, p)
	case MethodGoalsEdit:
		return officialGoalsEdit(api, p)
	case MethodGoalsPause, MethodGoalsResume, MethodGoalsComplete:
		return officialGoalsTransition(api, method, p)
	case MethodGoalsClear:
		return officialGoalsClear(api, p)
	case MethodCommandsList:
		return officialCommandsList(api, p)
	case MethodCommandsExecute:
		return officialCommandsExecute(api, p)
	case MethodSubagentsPrompt:
		return officialSubagentsPrompt(api, p)
	case MethodSubagentsInterruptByParent:
		return officialSubagentsInterrupt(api, p)
	case MethodUserQuestionsAnswer:
		return officialUserQuestionsAnswer(api, p)
	case MethodUserQuestionsAttachWait:
		return officialUserQuestionsAttachWait(api, p)
	case MethodAgentPresetsRead:
		return officialAgentPresetsRead(api, p)
	case MethodAgentPresetsSelect:
		return officialAgentPresetsSelect(api, p)
	case MethodSkillsList:
		return officialSkillsList(api, p)
	case MethodFileReferencesList:
		return officialFileReferencesList(api, p)
	case MethodFileUploadsUpload:
		return officialFileUploadsUpload(api, p)
	case MethodMessageFeedbackList:
		return officialMessageFeedbackList(api, p)
	case MethodMessageFeedbackPut:
		return officialMessageFeedbackPut(api, p)
	case MethodMessageFeedbackDelete:
		return officialMessageFeedbackDelete(api, p)
	case MethodSessionFeedbackRecord:
		return officialSessionFeedbackRecord(api, p)
	case MethodSessionReferenceResolverCandidates:
		return officialSessionReferenceCandidates(api, p)
	case MethodToolResultsWatch:
		return officialToolResultsWatch(api, p)
	}
	return nil, officialNotImplemented("未知的官方 agent 方法 %q", method)
}

// ---------------------------------------------------------------------------
// goals.*
// ---------------------------------------------------------------------------

func officialGoalsGet(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.GoalGet == nil {
		return nil, officialNotImplemented("goals.get 未接线：宿主没有 goal 存储")
	}
	return api.GoalGet(sessionID)
}

func officialGoalRefFrom(p *officialParams) (OfficialGoalRef, error) {
	obj, ok, err := p.object("ref")
	if err != nil {
		return OfficialGoalRef{}, err
	}
	if !ok {
		return OfficialGoalRef{}, officialBadRequest("缺 ref（{\"id\":…,\"revision\":…}）")
	}
	o := newOfficialObject("ref", obj)
	id, hasID, err := o.str("id")
	if err != nil {
		return OfficialGoalRef{}, err
	}
	rev, hasRev, err := o.number("revision")
	if err != nil {
		return OfficialGoalRef{}, err
	}
	if err := o.finish(); err != nil {
		return OfficialGoalRef{}, err
	}
	if !hasID || id == "" {
		return OfficialGoalRef{}, officialBadRequest("ref.id 必填且非空")
	}
	if !hasRev || rev < 1 {
		return OfficialGoalRef{}, officialBadRequest("ref.revision 必填且必须是正整数")
	}
	return OfficialGoalRef{ID: id, Revision: rev}, nil
}

// goalRequestFields 读 `request`（或平铺）里的 objective / maxGoalRounds。
//
// 官方 create/edit 的这两个字段都是 optional 容器里的字段（create 要求
// objective 必填，edit 要求至少一个），这里只做形状解析，业务判定交给宿主。
func goalRequestFields(p *officialParams, requiredObjective bool) (obj *string, maxRounds *int, err error) {
	nested, hasNested, err := p.object("request")
	if err != nil {
		return nil, nil, err
	}
	flatObjective, hasFlatObjective, err := p.takeStr("objective")
	if err != nil {
		return nil, nil, err
	}
	flatMax, hasFlatMax, err := p.takeInt("maxGoalRounds")
	if err != nil {
		return nil, nil, err
	}
	if hasNested {
		if hasFlatObjective || hasFlatMax {
			return nil, nil, officialBadRequest("request 与顶级 objective/maxGoalRounds 不能同时给")
		}
		o := newOfficialObject("request", nested)
		objective, hasObjective, err := o.str("objective")
		if err != nil {
			return nil, nil, err
		}
		rounds, hasRounds, err := o.number("maxGoalRounds")
		if err != nil {
			return nil, nil, err
		}
		if err := o.finish(); err != nil {
			return nil, nil, err
		}
		if hasObjective {
			obj = &objective
		}
		if hasRounds {
			if rounds < 0 {
				return nil, nil, officialBadRequest("request.maxGoalRounds 不能为负")
			}
			value := int(rounds)
			maxRounds = &value
		}
	} else if hasFlatObjective || hasFlatMax {
		if hasFlatObjective {
			obj = &flatObjective
		}
		if hasFlatMax {
			if flatMax < 0 {
				return nil, nil, officialBadRequest("maxGoalRounds 不能为负")
			}
			value := int(flatMax)
			maxRounds = &value
		}
	}
	if requiredObjective && obj == nil {
		return nil, nil, officialBadRequest("缺 objective")
	}
	return obj, maxRounds, nil
}

func (p *officialParams) takeStr(name string) (string, bool, error) {
	raw, ok := p.take(name)
	if !ok {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, officialBadRequest("%s 必须是字符串", name)
	}
	return s, true, nil
}

func (p *officialParams) takeInt(name string) (int64, bool, error) {
	raw, ok := p.take(name)
	if !ok {
		return 0, false, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false, officialBadRequest("%s 必须是数字", name)
	}
	if f != float64(int64(f)) {
		return 0, false, officialBadRequest("%s 必须是整数", name)
	}
	return int64(f), true, nil
}

func officialGoalsCreate(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	objective, maxRounds, err := goalRequestFields(p, true)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if *objective == "" {
		return nil, officialBadRequest("objective 是空串")
	}
	if api.GoalCreate == nil {
		return nil, officialNotImplemented("goals.create 未接线：宿主没有 goal 存储")
	}
	ref, err := api.GoalCreate(sessionID, OfficialGoalCreateRequest{Objective: *objective, MaxGoalRounds: maxRounds})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ref": ref}, nil
}

func officialGoalsEdit(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	ref, err := officialGoalRefFrom(p)
	if err != nil {
		return nil, err
	}
	objective, maxRounds, err := goalRequestFields(p, false)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if objective == nil && maxRounds == nil {
		return nil, officialBadRequest("goals.edit 至少要给 objective 或 maxGoalRounds 之一")
	}
	if objective != nil && *objective == "" {
		return nil, officialBadRequest("objective 是空串")
	}
	if api.GoalEdit == nil {
		return nil, officialNotImplemented("goals.edit 未接线：宿主没有 goal 存储")
	}
	return api.GoalEdit(sessionID, ref, OfficialGoalEditRequest{Objective: objective, MaxGoalRounds: maxRounds})
}

func officialGoalsTransition(api OfficialAgentAPI, method Method, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	ref, err := officialGoalRefFrom(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	switch method {
	case MethodGoalsPause:
		if api.GoalPause == nil {
			return nil, officialNotImplemented("goals.pause 未接线：宿主没有 goal 存储")
		}
		return api.GoalPause(sessionID, ref)
	case MethodGoalsResume:
		if api.GoalResume == nil {
			return nil, officialNotImplemented("goals.resume 未接线：宿主没有 goal 存储")
		}
		return api.GoalResume(sessionID, ref)
	default:
		if api.GoalComplete == nil {
			return nil, officialNotImplemented("goals.complete 未接线：宿主没有 goal 存储")
		}
		return api.GoalComplete(sessionID, ref)
	}
}

func officialGoalsClear(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	ref, err := officialGoalRefFrom(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.GoalClear == nil {
		return nil, officialNotImplemented("goals.clear 未接线：宿主没有 goal 存储")
	}
	return api.GoalClear(sessionID, ref)
}

// ---------------------------------------------------------------------------
// commands.*
// ---------------------------------------------------------------------------

func officialCommandsList(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.CommandsList == nil {
		return nil, officialNotImplemented("commands.list 未接线：宿主没有命令注册表")
	}
	list, err := api.CommandsList(sessionID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []OfficialCommand{}
	}
	return list, nil
}

func officialCommandsExecute(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	line, err := p.str("line")
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, officialBadRequest("line 是空串")
	}
	rawAttachments, hasAttachments, err := p.array("submittedAttachments")
	if err != nil {
		return nil, err
	}
	var attachments []OfficialSubmittedAttachment
	if hasAttachments {
		for i, raw := range rawAttachments {
			var item OfficialSubmittedAttachment
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, officialBadRequest("submittedAttachments[%d] 不是对象", i)
			}
			attachments = append(attachments, item)
		}
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.CommandsExecute == nil {
		return nil, officialNotImplemented("commands.execute 未接线：宿主没有命令注册表，不能假装执行")
	}
	exec, err := api.CommandsExecute(sessionID, line, attachments)
	if err != nil {
		return nil, err
	}
	if exec == nil {
		return nil, nil
	}
	return exec, nil
}

// ---------------------------------------------------------------------------
// subagents.*
// ---------------------------------------------------------------------------

func officialSubagentsPrompt(api OfficialAgentAPI, p *officialParams) (any, error) {
	obj, ok, err := p.object("request")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("缺 request（官方 SubagentPromptRequest）")
	}
	o := newOfficialObject("request", obj)
	requestID, hasRequestID, err := o.str("requestId")
	if err != nil {
		return nil, err
	}
	parentID, hasParent, err := o.str("parentSessionId")
	if err != nil {
		return nil, err
	}
	childID, hasChild, err := o.str("childSessionId")
	if err != nil {
		return nil, err
	}
	mode, hasMode, err := o.str("mode")
	if err != nil {
		return nil, err
	}
	delivery, hasDelivery, err := o.str("delivery")
	if err != nil {
		return nil, err
	}
	timeZone, _, err := o.str("clientTimeZone")
	if err != nil {
		return nil, err
	}
	rawContent, hasContent := o.take("content")
	if err := o.finish(); err != nil {
		return nil, err
	}
	if !hasRequestID || requestID == "" {
		return nil, officialBadRequest("request.requestId 必填且非空")
	}
	if !hasParent || parentID == "" {
		return nil, officialBadRequest("request.parentSessionId 必填且非空")
	}
	if !hasChild || childID == "" {
		return nil, officialBadRequest("request.childSessionId 必填且非空")
	}
	if !hasMode || mode != "continuable" {
		return nil, officialBadRequest("request.mode 必须是 \"continuable\"")
	}
	if !hasDelivery || (delivery != "queue" && delivery != "steer") {
		return nil, officialBadRequest("request.delivery 必须是 \"queue\" 或 \"steer\"")
	}
	if !hasContent {
		return nil, officialBadRequest("request.content 必填")
	}
	var parts []OfficialSubagentPromptContent
	if err := json.Unmarshal(rawContent, &parts); err != nil {
		return nil, officialBadRequest("request.content 必须是数组")
	}
	if len(parts) == 0 {
		return nil, officialBadRequest("request.content 是空数组")
	}
	for i, part := range parts {
		switch part.Type {
		case "text":
			if part.Text == "" {
				return nil, officialBadRequest("request.content[%d].text 是空串", i)
			}
		case "image":
			switch part.MediaType {
			case "image/png", "image/jpeg", "image/webp", "image/gif":
			default:
				return nil, officialBadRequest("request.content[%d].mediaType 不在官方枚举里", i)
			}
			if part.Data == "" {
				return nil, officialBadRequest("request.content[%d].data 是空串", i)
			}
		default:
			return nil, officialBadRequest("request.content[%d].type 必须是 text 或 image", i)
		}
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.SubagentsPrompt == nil {
		return nil, officialNotImplemented("subagents.prompt 未接线：宿主没有可续问的子会话（child inbox）")
	}
	return api.SubagentsPrompt(OfficialSubagentPromptRequest{
		RequestID: requestID, ParentSessionID: parentID, ChildSessionID: childID,
		Mode: mode, Delivery: delivery, Content: parts, ClientTimeZone: timeZone,
	})
}

func officialSubagentsInterrupt(api OfficialAgentAPI, p *officialParams) (any, error) {
	childID, err := p.str("childSessionId")
	if err != nil {
		return nil, err
	}
	parentID, err := p.str("parentSessionId")
	if err != nil {
		return nil, err
	}
	mode, err := p.str("mode")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if childID == "" || parentID == "" {
		return nil, officialBadRequest("childSessionId / parentSessionId 不能是空串")
	}
	if mode != "continuable" {
		return nil, officialBadRequest("mode 必须是 \"continuable\"")
	}
	if api.SubagentsInterruptByParent == nil {
		return nil, officialNotImplemented("subagents.interruptByParent 未接线：宿主没有可续问的子会话（child activation）")
	}
	return api.SubagentsInterruptByParent(childID, parentID)
}

// ---------------------------------------------------------------------------
// userQuestions.*
// ---------------------------------------------------------------------------

func officialUserQuestionsAnswer(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	callID, err := p.str("callId")
	if err != nil {
		return nil, err
	}
	obj, ok, err := p.object("answer")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("缺 answer（{\"answers\":[…] }）")
	}
	o := newOfficialObject("answer", obj)
	rawAnswers, hasAnswers := o.take("answers")
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if callID == "" {
		return nil, officialBadRequest("callId 是空串")
	}
	if !hasAnswers {
		return nil, officialBadRequest("answer.answers 必填")
	}
	var answers []OfficialQuestionAnswerItem
	if err := json.Unmarshal(rawAnswers, &answers); err != nil {
		return nil, officialBadRequest("answer.answers 必须是数组")
	}
	if len(answers) == 0 {
		return nil, officialBadRequest("answer.answers 是空数组")
	}
	for i, item := range answers {
		if item.ID == "" {
			return nil, officialBadRequest("answer.answers[%d].id 必填", i)
		}
		if item.Selected == nil {
			return nil, officialBadRequest("answer.answers[%d].selected 必填（可为空数组）", i)
		}
	}
	if api.UserQuestionsAnswer == nil {
		return nil, officialNotImplemented("userQuestions.answer 未接线：宿主没有活的 question 通道")
	}
	stillContinued, err := api.UserQuestionsAnswer(sessionID, callID, OfficialQuestionAnswer{Answers: answers})
	if err != nil {
		return nil, err
	}
	return stillContinued, nil
}

func officialUserQuestionsAttachWait(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	callID, err := p.str("callId")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if callID == "" {
		return nil, officialBadRequest("callId 是空串")
	}
	if api.UserQuestionsAttachWait == nil {
		return nil, officialNotImplemented("userQuestions.attachWait 未接线：宿主没有定时等待通道（官方这个是 stream）")
	}
	wait, err := api.UserQuestionsAttachWait(sessionID, callID)
	if err != nil {
		return nil, err
	}
	if wait == nil {
		// 官方："wait 结束之后一帧都不发"。本仓信封不接受 null，所以用**空
		// result 对象**表达"这次没有帧"——与 commands.execute 的 undefined 同形。
		return nil, nil
	}
	return wait, nil
}

// ---------------------------------------------------------------------------
// agentPresets.*
// ---------------------------------------------------------------------------

func officialAgentPresetsRead(api OfficialAgentAPI, p *officialParams) (any, error) {
	presetID, err := p.str("agentPreset")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if presetID == "" {
		return nil, officialBadRequest("agentPreset 是空串")
	}
	if api.AgentPresetRead == nil {
		return nil, officialNotImplemented("agentPresets.read 未接线：宿主没有 preset 目录")
	}
	return api.AgentPresetRead(presetID)
}

func officialAgentPresetsSelect(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	presetID, err := p.str("agentPreset")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if presetID == "" {
		return nil, officialBadRequest("agentPreset 是空串")
	}
	if api.AgentPresetSelect == nil {
		return nil, officialNotImplemented("agentPresets.select 未接线：宿主没有 preset 目录")
	}
	return api.AgentPresetSelect(sessionID, presetID)
}

// ---------------------------------------------------------------------------
// skills.list / fileReferences.list / fileUploads.upload
// ---------------------------------------------------------------------------

func officialSkillsList(api OfficialAgentAPI, p *officialParams) (any, error) {
	obj, ok, err := p.object("request")
	if err != nil {
		return nil, err
	}
	if !ok {
		// 兼容：直接把会话 id 平铺在顶层。
		sessionID, err := p.sessionRef()
		if err != nil {
			return nil, err
		}
		if err := p.finish(); err != nil {
			return nil, err
		}
		return officialSkillsCatalog(api, sessionID)
	}
	o := newOfficialObject("request", obj)
	sessionID, hasSession, err := o.str("sessionId")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasSession || sessionID == "" {
		return nil, officialBadRequest("request.sessionId 必填且非空")
	}
	return officialSkillsCatalog(api, sessionID)
}

func officialSkillsCatalog(api OfficialAgentAPI, sessionID string) (any, error) {
	if api.SkillsList == nil {
		return nil, officialNotImplemented("skills.list 未接线：宿主没有 skill 目录")
	}
	return api.SkillsList(sessionID)
}

func officialFileReferencesList(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	query, hasQuery, err := p.takeStr("query")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasQuery {
		return nil, officialBadRequest("缺 query（@ 之后那段路径文本，可以是空串）")
	}
	if api.FileReferencesList == nil {
		return nil, officialNotImplemented("fileReferences.list 未接线：宿主没有可列的工作目录")
	}
	list, err := api.FileReferencesList(sessionID, query)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []OfficialFileReference{}
	}
	return list, nil
}

func officialFileUploadsUpload(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	obj, ok, err := p.object("request")
	if err != nil {
		return nil, err
	}
	var data, name string
	if ok {
		o := newOfficialObject("request", obj)
		value, hasData, err := o.str("data")
		if err != nil {
			return nil, err
		}
		displayName, _, err := o.str("name")
		if err != nil {
			return nil, err
		}
		if err := o.finish(); err != nil {
			return nil, err
		}
		if !hasData {
			return nil, officialBadRequest("request.data 必填")
		}
		data, name = value, displayName
	} else {
		value, hasData, err := p.takeStr("data")
		if err != nil {
			return nil, err
		}
		if !hasData {
			return nil, officialBadRequest("缺 request（{\"data\":\"<base64>\"}）")
		}
		displayName, _, err := p.takeStr("name")
		if err != nil {
			return nil, err
		}
		data, name = value, displayName
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if data == "" {
		return nil, officialBadRequest("data 是空串（base64 编码的完整文件字节）")
	}
	if api.FileUploadsUpload == nil {
		return nil, officialNotImplemented("fileUploads.upload 未接线：宿主没有上传存储")
	}
	return api.FileUploadsUpload(sessionID, OfficialFileUploadRequest{Data: data, Name: name})
}

// ---------------------------------------------------------------------------
// messageFeedback.* / sessionFeedback.record
// ---------------------------------------------------------------------------

var officialFeedbackCategories = map[string]bool{
	OfficialFeedbackOther: true, OfficialFeedbackTaskResult: true,
	OfficialFeedbackInstructionFollowing: true, OfficialFeedbackProductInteraction: true,
	OfficialFeedbackServiceStability: true, OfficialFeedbackResourceCost: true,
	OfficialFeedbackSecurityPrivacyPermission: true,
}

func officialMessageFeedbackList(api OfficialAgentAPI, p *officialParams) (any, error) {
	obj, ok, err := p.object("request")
	if err != nil {
		return nil, err
	}
	var sessionID string
	if ok {
		o := newOfficialObject("request", obj)
		value, hasSession, err := o.str("sessionId")
		if err != nil {
			return nil, err
		}
		if err := o.finish(); err != nil {
			return nil, err
		}
		if !hasSession || value == "" {
			return nil, officialBadRequest("request.sessionId 必填且非空")
		}
		sessionID = value
	} else {
		value, err := p.sessionRef()
		if err != nil {
			return nil, err
		}
		sessionID = value
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.MessageFeedbackList == nil {
		return nil, officialNotImplemented("messageFeedback.list 未接线：宿主没有消息反馈存储")
	}
	return api.MessageFeedbackList(sessionID)
}

func officialMessageFeedbackPut(api OfficialAgentAPI, p *officialParams) (any, error) {
	obj, ok, err := p.object("request")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("缺 request（官方 MessageFeedbackPutRequest）")
	}
	o := newOfficialObject("request", obj)
	sessionID, hasSession, err := o.str("sessionId")
	if err != nil {
		return nil, err
	}
	messageID, hasMessage, err := o.str("messageId")
	if err != nil {
		return nil, err
	}
	rating, hasRating, err := o.str("rating")
	if err != nil {
		return nil, err
	}
	note, _, err := o.str("note")
	if err != nil {
		return nil, err
	}
	category, hasCategory, err := o.str("category")
	if err != nil {
		return nil, err
	}
	rawVersion, hasVersion := o.take("ifVersion")
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasSession || sessionID == "" {
		return nil, officialBadRequest("request.sessionId 必填且非空")
	}
	if !hasMessage || messageID == "" {
		return nil, officialBadRequest("request.messageId 必填且非空")
	}
	if !hasRating || (rating != "positive" && rating != "negative") {
		return nil, officialBadRequest("request.rating 必须是 positive 或 negative")
	}
	if hasCategory && !officialFeedbackCategories[category] {
		return nil, officialBadRequest("request.category 不在官方枚举里")
	}
	if !hasVersion {
		return nil, officialBadRequest("request.ifVersion 必填（null 表示要求当前不存在）")
	}
	var ifVersion *string
	if string(rawVersion) != "null" {
		var value string
		if err := json.Unmarshal(rawVersion, &value); err != nil {
			return nil, officialBadRequest("request.ifVersion 必须是字符串或 null")
		}
		ifVersion = &value
	}
	if api.MessageFeedbackPut == nil {
		return nil, officialNotImplemented("messageFeedback.put 未接线：宿主没有消息反馈存储")
	}
	return api.MessageFeedbackPut(OfficialMessageFeedbackPutRequest{
		SessionID: sessionID, MessageID: messageID, Rating: rating,
		Note: note, Category: category, IfVersion: ifVersion,
	})
}

func officialMessageFeedbackDelete(api OfficialAgentAPI, p *officialParams) (any, error) {
	obj, ok, err := p.object("request")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("缺 request（官方 MessageFeedbackDeleteRequest）")
	}
	o := newOfficialObject("request", obj)
	sessionID, hasSession, err := o.str("sessionId")
	if err != nil {
		return nil, err
	}
	messageID, hasMessage, err := o.str("messageId")
	if err != nil {
		return nil, err
	}
	ifVersion, hasVersion, err := o.str("ifVersion")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasSession || sessionID == "" {
		return nil, officialBadRequest("request.sessionId 必填且非空")
	}
	if !hasMessage || messageID == "" {
		return nil, officialBadRequest("request.messageId 必填且非空")
	}
	if !hasVersion {
		return nil, officialBadRequest("request.ifVersion 必填")
	}
	if api.MessageFeedbackDelete == nil {
		return nil, officialNotImplemented("messageFeedback.delete 未接线：宿主没有消息反馈存储")
	}
	return api.MessageFeedbackDelete(OfficialMessageFeedbackDeleteRequest{
		SessionID: sessionID, MessageID: messageID, IfVersion: ifVersion,
	})
}

func officialSessionFeedbackRecord(api OfficialAgentAPI, p *officialParams) (any, error) {
	obj, ok, err := p.object("request")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("缺 request（官方 SessionFeedbackRecordRequest）")
	}
	o := newOfficialObject("request", obj)
	sessionID, hasSession, err := o.str("sessionId")
	if err != nil {
		return nil, err
	}
	text, _, err := o.str("text")
	if err != nil {
		return nil, err
	}
	category, hasCategory, err := o.str("category")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasSession || sessionID == "" {
		return nil, officialBadRequest("request.sessionId 必填且非空")
	}
	if hasCategory && !officialFeedbackCategories[category] {
		return nil, officialBadRequest("request.category 不在官方枚举里")
	}
	if api.SessionFeedbackRecord == nil {
		return nil, officialNotImplemented("sessionFeedback.record 未接线：宿主没有会话反馈存储")
	}
	return api.SessionFeedbackRecord(OfficialSessionFeedbackRequest{
		SessionID: sessionID, Text: text, Category: category,
	})
}

// ---------------------------------------------------------------------------
// sessionReferenceResolver.candidates / toolResults.watch
// ---------------------------------------------------------------------------

func officialSessionReferenceCandidates(api OfficialAgentAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	query, hasQuery, err := p.takeStr("query")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasQuery {
		return nil, officialBadRequest("缺 query（可以是空串 = 不过滤）")
	}
	if api.SessionReferenceCandidates == nil {
		return nil, officialNotImplemented("sessionReferenceResolver.candidates 未接线：宿主没有会话索引")
	}
	list, err := api.SessionReferenceCandidates(sessionID, query)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []OfficialSessionReferenceCandidate{}
	}
	return list, nil
}

// officialToolResultsWatch 是 unary 路由的兜底：真正的实现是流式
// （official_stream_sources.go 的 officialToolResultStream）。
//
// **它不会走到这里**：`serveOfficialAgentAPI` 在 unary 路由之前用
// `IsOfficialStreamMethod` 把 `toolResults.watch` 接走了。保留这个 case 的
// 理由与 workspaceFiles.changes 相同：路由表完整可读，且被误调时明确报错。
func officialToolResultsWatch(api OfficialAgentAPI, p *officialParams) (any, error) {
	return nil, officialNotImplemented(
		"toolResults.watch 是流式方法：分派应在 IsOfficialStreamMethod 分支" +
			"（NDJSON 长连接）被接走，走到 unary 路由说明分派顺序坏了")
}
