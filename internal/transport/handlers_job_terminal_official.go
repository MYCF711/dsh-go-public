package transport

// 官方桌面端 0.2.2-rc.1 的 job.*（3）与 terminal.*（8），L1 语义对齐层。
//
// # 契约来源（只读镜像，未改安装目录）
//
//	app.asar sha256 5fa8a6b21bceb8f3e99e30efa2270efd0cc773a11e42aa0488f804a64b34e637
//	@deepseek-ai/dsh-api-job-controller/lib/typert.host.js:73/77   job/list   {sessionId} -> {type:'rows',jobs}
//	@deepseek-ai/dsh-api-job-controller/lib/typert.host.js:64/69   job/kill   {sessionId,jobId} -> {outcome}
//	@deepseek-ai/dsh-api-job-controller/lib/typert.host.js:7/13    job/follow {sessionId?,jobId,from?} -> opened|output|status
//	@deepseek-ai/dsh-api-terminal-controller/lib/typert.host.js:82/89   terminal/create request {shellPath?,id,cols,rows}
//	@deepseek-ai/dsh-api-terminal-controller/lib/typert.host.js:107      terminal/environment
//	@deepseek-ai/dsh-api-terminal-controller/lib/typert.host.js:22       terminal/attach result（snapshot|output|state|ack）
//
// # 这一层做什么、不做什么
//
// 做：把官方参数与结果**按官方字段名**翻译成现有宿主能力，沿用本仓的四字段错误
// 契约与既有 cookie 认证（本文件不碰认证：路由必须挂在 handleAPI 的 authorized
// 之后，见 RESULT 的接线清单）。
//
// 不做（不许假装）：
//   - 官方 job/list、job/follow、terminal/attach、terminal/retain 都是 typert
//     `mode:'stream'`。本宿主 HTTP 侧只有 `POST /api/<method>` 一问一答，
//     所以这里**一律只返回一帧**，并在结果里显式写 `oneShot:true`；
//     绝不发 SSE、绝不注册流式端点、绝不返回"看起来会继续来"的帧。
//   - 官方 job.kill 的 `already-finished` 是一个成功 outcome；本仓契约要求
//     "任务已结束" 落 409 job_finished（既有 jobs.stop 也是这么回的），
//     所以这里 409 优先，`already-finished` 仅在底层服务真的这么回报时才出现。
//   - 官方 attach 的 uplink（write/resize）要求长连接；本宿主 attach 只给快照，
//     写入仍走既有 terminal.input。terminal.environment 的 maxInputBytes 因此
//     是本层 Input 强制的上限，不是 attach 通道的上限（已登记）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// 官方 11 个方法名。顺序与 parity 清单一致，方便和 docs/official-desktop-api-parity.md 对读。
const (
	MethodJobList   Method = "job.list"
	MethodJobKill   Method = "job.kill"
	MethodJobFollow Method = "job.follow"

	MethodTerminalCreate      Method = "terminal.create"
	MethodTerminalAttach      Method = "terminal.attach"
	MethodTerminalList        Method = "terminal.list"
	MethodTerminalClose       Method = "terminal.close"
	MethodTerminalEnvironment Method = "terminal.environment"
	MethodTerminalShells      Method = "terminal.shells"
	MethodTerminalRename      Method = "terminal.rename"
	MethodTerminalRetain      Method = "terminal.retain"
)

// OfficialJobTerminalMethods 返回本层负责的 11 个方法。
//
// **它们是 additive 扩展**：不得加入 SessionMethods 的 canonical 25，
// 也不得进 TaskActionMethods（那是 jobs.stop / schedule.* 的既有扩展面）。
func OfficialJobTerminalMethods() []Method {
	return []Method{
		MethodJobList, MethodJobKill, MethodJobFollow,
		MethodTerminalCreate, MethodTerminalAttach, MethodTerminalList, MethodTerminalClose,
		MethodTerminalEnvironment, MethodTerminalShells, MethodTerminalRename, MethodTerminalRetain,
	}
}

// Official 层的稳定错误。**status 与 code 都在本文件里定死**，不靠中文消息猜：
// 猜错码的后果是客户端走错分支（把"任务已结束"当成"宿主崩了"去重试）。
var (
	// ErrJTOfficialUnsupported：本宿主没有这个能力（回 501 not_implemented）。
	// 用它的时候必须说清缺的是什么，禁止用它掩盖实现 bug。
	ErrJTOfficialUnsupported = errors.New("官方方法在本宿主未实现")
	// ErrJTOfficialTerminalGone：终端身份不存在或已在本会话关闭（回 404）。
	ErrJTOfficialTerminalGone = errors.New("没有这个终端")
	// ErrJTOfficialBadParam：底层服务发现参数不合法（回 400）。
	ErrJTOfficialBadParam = errors.New("参数不合法")
)

// ---------------------------------------------------------------------------
// 官方结果形状（字段名逐字对齐 zod；可选字段用 omitempty，空值不出现）
// ---------------------------------------------------------------------------

// OfficialJobOutput 对应 JobView.output。
//
// total 是"下一块从哪个字节开始"，earliest 是仍保留的最旧字节；
// 两者都是**字节**偏移（官方是 byte offset，不是 rune）。
type OfficialJobOutput struct {
	Total      int64    `json:"total"`
	Earliest   int64    `json:"earliest"`
	SpillPaths []string `json:"spillPaths,omitempty"`
}

// OfficialJobView 对应 JobView。
type OfficialJobView struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Label 是产物自己给的一行标签；本宿主是命令首行（有界）。
	Label string `json:"label"`
	// Owner 是归属会话；未归属的 job 不出现该字段。
	Owner string `json:"owner,omitempty"`
	// OutputLimitBytes 是产物声明的上限；本宿主不声明，故不出现。
	OutputLimitBytes int `json:"outputLimitBytes,omitempty"`
	// Status 取 running|stopping|completed|killed|failed。
	Status    string `json:"status"`
	Progress  string `json:"progress,omitempty"`
	Detail    string `json:"detail,omitempty"`
	StartedAt int64  `json:"startedAt"`
	// FinishedAt 只在已结算时出现；0 表示服务没有给出，不出现。
	FinishedAt int64             `json:"finishedAt,omitempty"`
	Output     OfficialJobOutput `json:"output"`
}

// OfficialJobListFrame 对应 JobListFrame（官方只有 rows 一种）。
type OfficialJobListFrame struct {
	Type string            `json:"type"`
	Jobs []OfficialJobView `json:"jobs"`
}

// OfficialJobKillValue 对应 JobKillValue。
type OfficialJobKillValue struct {
	Outcome string `json:"outcome"`
}

// OfficialJobChunk 对应 JobChunk。at 是**绝对字节偏移**。
type OfficialJobChunk struct {
	At        int64  `json:"at"`
	Text      string `json:"text"`
	Channel   string `json:"channel,omitempty"`
	GapBefore bool   `json:"gapBefore,omitempty"`
}

// OfficialJobFollowFrame 是 job/follow 的**一帧**。
//
// 官方那是 AsyncIterable；本宿主一次性读取，所以每个响应恰好一帧，
// 且带 OneShot:true。帧类型不同，出现的字段也不同——MarshalJSON 按类型裁。
type OfficialJobFollowFrame struct {
	Type string
	// opened / status 帧带 Job。
	Job *OfficialJobView
	// opened 帧带 From（第一块 output 从这里继续）。
	From *int64
	// output 帧带 Chunks 与 Next。
	Chunks []OfficialJobChunk
	Next   *int64
	Lossy  bool
	// OneShot 恒为 true：告诉客户端"这不是流，别等下一帧"。
	OneShot bool
}

// MarshalJSON 按帧类型只输出该类型**有定义**的键。
//
// 为什么要手写：output 帧的 chunks 是必填数组，status 帧又没有 chunks；
// 用 omitempty 会把空数组一起省掉（官方 zod 会判缺字段），
// 不用又会给 status 帧塞 `"chunks":null`。两种都是错形状。
func (f OfficialJobFollowFrame) MarshalJSON() ([]byte, error) {
	type opened struct {
		Type    string           `json:"type"`
		Job     *OfficialJobView `json:"job"`
		From    *int64           `json:"from"`
		OneShot bool             `json:"oneShot"`
	}
	type output struct {
		Type    string             `json:"type"`
		Chunks  []OfficialJobChunk `json:"chunks"`
		Next    *int64             `json:"next"`
		Lossy   bool               `json:"lossy,omitempty"`
		OneShot bool               `json:"oneShot"`
	}
	type status struct {
		Type    string           `json:"type"`
		Job     *OfficialJobView `json:"job"`
		OneShot bool             `json:"oneShot"`
	}
	switch f.Type {
	case "opened":
		return json.Marshal(opened{Type: f.Type, Job: f.Job, From: f.From, OneShot: f.OneShot})
	case "output":
		chunks := f.Chunks
		if chunks == nil {
			chunks = []OfficialJobChunk{}
		}
		return json.Marshal(output{Type: f.Type, Chunks: chunks, Next: f.Next, Lossy: f.Lossy, OneShot: f.OneShot})
	case "status":
		return json.Marshal(status{Type: f.Type, Job: f.Job, OneShot: f.OneShot})
	}
	return nil, fmt.Errorf("官方 job.follow 帧类型只有 opened/output/status，收到 %q", f.Type)
}

// OfficialTerminalShell 对应 TerminalShell。
//
// Supported 是本仓 additive 字段：本宿主的持久 shell 会话只能起默认 shell
// （底层 modules/exec/bash 写死 bash），其余候选即使装上也不可 create。
// 官方没有这个字段；给出来是为了让客户端能把不可用的项灰掉，而不是点了报错。
type OfficialTerminalShell struct {
	Path      string   `json:"path"`
	Args      []string `json:"args"`
	Name      string   `json:"name"`
	Supported *bool    `json:"supported,omitempty"`
}

// OfficialWebTerminalInfo 对应 WebTerminalInfo。
type OfficialWebTerminalInfo struct {
	ID       string                `json:"id"`
	Title    string                `json:"title"`
	Shell    OfficialTerminalShell `json:"shell"`
	Cwd      string                `json:"cwd"`
	Cols     int                   `json:"cols"`
	Rows     int                   `json:"rows"`
	State    string                `json:"state"`
	ExitCode *int                  `json:"exitCode"`
	Error    string                `json:"error,omitempty"`
}

// OfficialTerminalCreateRequest 对应 TerminalCreateRequest。
//
// id 由**调用方**给（官方语义：caller-generated identity，用来做幂等）。
type OfficialTerminalCreateRequest struct {
	ShellPath string `json:"shellPath,omitempty"`
	ID        string `json:"id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// OfficialTerminalEnvironment 对应 TerminalEnvironment。
type OfficialTerminalEnvironment struct {
	Cwd              string `json:"cwd"`
	MaxInputBytes    int    `json:"maxInputBytes"`
	MaxBufferedBytes int    `json:"maxBufferedBytes"`
	MaxCols          int    `json:"maxCols"`
	MaxRows          int    `json:"maxRows"`
	Scrollback       int    `json:"scrollback"`
}

// OfficialTerminalSnapshot 是 attach 的**一帧快照**（官方首帧形状）。
//
// 官方 attach 是流：snapshot → output…（+ uplink 写/resize）。
// 本宿主没有长连接端点，所以只回 snapshot 这一帧，并标 OneShot:true；
// 后续输出走既有 terminal.output，写入走既有 terminal.input。
type OfficialTerminalSnapshot struct {
	Type     string                  `json:"type"`
	Sequence int64                   `json:"sequence"`
	Screen   string                  `json:"screen"`
	Info     OfficialWebTerminalInfo `json:"info"`
	Writable bool                    `json:"writable"`
	OneShot  bool                    `json:"oneShot"`
}

// OfficialTerminalRetentionFrame 对应 TerminalRetentionFrame（只有 retained 一种）。
type OfficialTerminalRetentionFrame struct {
	Type    string `json:"type"`
	OneShot bool   `json:"oneShot"`
}

// ---------------------------------------------------------------------------
// 服务面
// ---------------------------------------------------------------------------

// OfficialJobTerminalService 是官方 job.*/terminal.* 需要的能力。
//
// 实现放在宿主侧（cmd/dshd/serve_terminal_api.go）：job.* 复用既有 jobs 服务
// （Job Object/进程树归属、实例校验、跨会话 403、已结束 409 都由它给），
// terminal.* 复用 terminal.open/input/output 背后的真实 shell 会话。
//
// 每个方法返回的错误按下面这套 sentinel 翻状态码；未实现的能力必须返回
// ErrJTOfficialUnsupported（501），**不许返回零值当成功**。
type OfficialJobTerminalService interface {
	// JobList 列这条会话能看到的 job（自己的 + 未归属的）。
	JobList(sessionID string) (OfficialJobListFrame, error)
	// JobKill 让人来杀一个后台任务；跨会话/实例不符由实现判错。
	JobKill(ctx context.Context, sessionID, jobID string) (OfficialJobKillValue, error)
	// JobFollow 从 from 字节起读一页；已排干且已结算时返回 status 帧。
	JobFollow(ctx context.Context, sessionID, jobID string, from int64) (OfficialJobFollowFrame, error)

	TerminalCreate(ctx context.Context, sessionID string, req OfficialTerminalCreateRequest) (OfficialWebTerminalInfo, error)
	TerminalAttach(ctx context.Context, sessionID, id string) (OfficialTerminalSnapshot, error)
	TerminalList(sessionID string) ([]OfficialWebTerminalInfo, error)
	TerminalClose(ctx context.Context, sessionID, id string) error
	TerminalEnvironment(ctx context.Context, sessionID string) (OfficialTerminalEnvironment, error)
	TerminalShells(ctx context.Context, sessionID string) ([]OfficialTerminalShell, error)
	TerminalRename(ctx context.Context, sessionID, id, title string) error
	TerminalRetain(ctx context.Context, sessionID, id string) error
}

// OfficialJobTerminal 是 11 个方法的路由与参数校验层。
//
// 接线（Lead）：Options 加一个 `OfficialJobTerminal *OfficialJobTerminal` 字段，
// 在 handleAPI 的 authorized 之后、switch 的 default 之前加两行：
//
//	if s.opt.OfficialJobTerminal != nil && s.opt.OfficialJobTerminal.Handles(name) {
//		s.opt.OfficialJobTerminal.Serve(w, r); return
//	}
//
// 这样 11 个方法一次接上，且**不会**碰到既有 case（jobs.* / terminal.open|input|output）。
type OfficialJobTerminal struct {
	Svc OfficialJobTerminalService
}

// Handles 报这个方法名是不是本层的（只认官方 11 个，绝不抢既有方法）。
func (h *OfficialJobTerminal) Handles(method string) bool {
	switch Method(method) {
	case MethodJobList, MethodJobKill, MethodJobFollow,
		MethodTerminalCreate, MethodTerminalAttach, MethodTerminalList, MethodTerminalClose,
		MethodTerminalEnvironment, MethodTerminalShells, MethodTerminalRename, MethodTerminalRetain:
		return true
	}
	return false
}

// Serve 按 URL 里的方法名分发（Lead 的单点接线入口）。
func (h *OfficialJobTerminal) Serve(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	switch Method(method) {
	case MethodJobList:
		h.HandleJobList(w, r)
	case MethodJobKill:
		h.HandleJobKill(w, r)
	case MethodJobFollow:
		h.HandleJobFollow(w, r)
	case MethodTerminalCreate:
		h.HandleTerminalCreate(w, r)
	case MethodTerminalAttach:
		h.HandleTerminalAttach(w, r)
	case MethodTerminalList:
		h.HandleTerminalList(w, r)
	case MethodTerminalClose:
		h.HandleTerminalClose(w, r)
	case MethodTerminalEnvironment:
		h.HandleTerminalEnvironment(w, r)
	case MethodTerminalShells:
		h.HandleTerminalShells(w, r)
	case MethodTerminalRename:
		h.HandleTerminalRename(w, r)
	case MethodTerminalRetain:
		h.HandleTerminalRetain(w, r)
	default:
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
			method+" 不是官方 job.*/terminal.* 方法，本层不接管"))
	}
}

// ---------------------------------------------------------------------------
// 参数：官方字段名 + 既有别名（sessionId/agentId、jobId/id）
// ---------------------------------------------------------------------------

// officialIdentity 收会话身份。官方 wire 字段是 `agentId`（typert 的 lookup 参数），
// 本仓既有方法一律用 `sessionId`；两边都认，但**冲突必须报错**——
// 猜任何一个都会把 A 会话的操作打到 B 会话上。
type officialIdentity struct {
	SessionID string `json:"sessionId"`
	AgentID   string `json:"agentId"`
}

func (o officialIdentity) resolve(method string) (string, error) {
	switch {
	case o.SessionID != "" && o.AgentID != "" && o.SessionID != o.AgentID:
		return "", fmt.Errorf("%w: %s 同时给了 sessionId=%q 与 agentId=%q，两者不一致时不许猜",
			ErrJTOfficialBadParam, method, o.SessionID, o.AgentID)
	case o.SessionID != "":
		return o.SessionID, nil
	case o.AgentID != "":
		return o.AgentID, nil
	}
	return "", fmt.Errorf("%w: %s 缺 sessionId（官方 wire 名是 agentId，两个都认）", ErrJTOfficialBadParam, method)
}

// officialJobRef 收 job 身份：官方 `jobId`，既有 jobs.stop 用 `id`。
type officialJobRef struct {
	JobID string `json:"jobId"`
	ID    string `json:"id"`
}

func (o officialJobRef) resolve(method string) (string, error) {
	switch {
	case o.JobID != "" && o.ID != "" && o.JobID != o.ID:
		return "", fmt.Errorf("%w: %s 同时给了 jobId=%q 与 id=%q，两者不一致时不许猜",
			ErrJTOfficialBadParam, method, o.JobID, o.ID)
	case o.JobID != "":
		return o.JobID, nil
	case o.ID != "":
		return o.ID, nil
	}
	return "", fmt.Errorf("%w: %s 缺 jobId（既有写法 id 也认）", ErrJTOfficialBadParam, method)
}

// officialTerminalRef 收终端身份（官方 id 由 create 的调用方给）。
type officialTerminalRef struct {
	ID string `json:"id"`
}

func (o officialTerminalRef) resolve(method string) (string, error) {
	if strings.TrimSpace(o.ID) == "" {
		return "", fmt.Errorf("%w: %s 缺 id（终端身份由 terminal.create 的调用方给出）", ErrJTOfficialBadParam, method)
	}
	return o.ID, nil
}

// decodeJTOfficialParams 用既有 decodeTaskActionParams（裸参数对象 + 未知字段 fail-loud）。
func decodeJTOfficialParams(r *http.Request, target any) error {
	if err := decodeTaskActionParams(r, target); err != nil {
		return fmt.Errorf("%w: %v", ErrJTOfficialBadParam, err)
	}
	return nil
}

// writeJTOfficialResult 是成功侧的**唯一**出口（与既有方法同信封）。
func writeJTOfficialResult(w http.ResponseWriter, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// writeJTOfficialVoid 给官方的 void 结果用。
//
// 官方 close/rename 的结果是 z.void()；本仓的空 result 会被判成
// `empty_result` 编码失败，所以这里回 {"ok":true}——additive 字段，不是假数据。
func writeJTOfficialVoid(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": true}})
}

// writeJTOfficialError 是失败侧的**唯一**出口：四字段信封仍然只由 errorObject 造。
//
// 状态码映射就是语义：400 改参数、403 换会话、404 换 id、409 别重试、
// 501 换方法、500 可以重试。
func writeJTOfficialError(w http.ResponseWriter, method string, err error) {
	switch {
	case errors.Is(err, ErrJTOfficialBadParam):
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", method+"："+err.Error()))
	case errors.Is(err, ErrCrossSession):
		writeJSON(w, http.StatusForbidden, errorBodyFor("cross_session", method+"："+err.Error()))
	case errors.Is(err, ErrNoSuchJob), errors.Is(err, ErrJTOfficialTerminalGone), errors.Is(err, ErrNoSuchSession):
		writeJSON(w, http.StatusNotFound, errorBodyFor("not_found", method+"："+err.Error()))
	case errors.Is(err, ErrJobFinished):
		writeJSON(w, http.StatusConflict, errorBodyFor("job_finished", method+"："+err.Error()))
	case errors.Is(err, ErrBadJobCursor):
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", method+"："+err.Error()))
	case errors.Is(err, ErrJTOfficialUnsupported), errors.Is(err, ErrNotImplemented):
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+"："+err.Error()))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// 调用方自己走了：这不是服务端错误，但也别回 200。
		writeJSON(w, http.StatusRequestTimeout, errorBodyFor("internal", method+"：调用被取消（"+err.Error()+"）"))
	default:
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", method+"："+err.Error()))
	}
}

// ---------------------------------------------------------------------------
// job.*
// ---------------------------------------------------------------------------

// HandleJobList 是官方 job/list（官方是 stream；本宿主只回一帧 rows）。
func (h *OfficialJobTerminal) HandleJobList(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodJobList)
	var params officialIdentity
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	frame, err := h.Svc.JobList(sessionID)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	// 官方 JobListFrame 的 type 只有 'rows'；实现没填就补上，填错就是实现 bug。
	if frame.Type == "" {
		frame.Type = "rows"
	}
	if frame.Type != "rows" {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal",
			method+"：结果帧 type 只能是 rows，实现给了 "+frame.Type))
		return
	}
	if frame.Jobs == nil {
		frame.Jobs = []OfficialJobView{}
	}
	writeJTOfficialResult(w, frame)
}

// HandleJobKill 是官方 job/kill（unary，结果 {outcome}）。
func (h *OfficialJobTerminal) HandleJobKill(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodJobKill)
	var params officialJobParams
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.officialIdentity.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	jobID, err := params.officialJobRef.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	value, err := h.Svc.JobKill(r.Context(), sessionID, jobID)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	switch value.Outcome {
	case "requested", "already-finished":
	default:
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal",
			method+"：outcome 只能是 requested/already-finished，实现给了 "+value.Outcome))
		return
	}
	writeJTOfficialResult(w, value)
}

// HandleJobFollow 是官方 job/follow 的**一次性读取**（一帧，带 oneShot）。
func (h *OfficialJobTerminal) HandleJobFollow(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodJobFollow)
	var params struct {
		officialJobParams
		From *int64 `json:"from"`
	}
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.officialJobParams.officialIdentity.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	jobID, err := params.officialJobParams.officialJobRef.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	var from int64
	if params.From != nil {
		if *params.From < 0 {
			writeJTOfficialError(w, method, fmt.Errorf("%w: from 不能为负（现在是 %d）", ErrJTOfficialBadParam, *params.From))
			return
		}
		from = *params.From
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	frame, err := h.Svc.JobFollow(r.Context(), sessionID, jobID, from)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	// 这一层**总是**一次性：实现不许把它当成流（比如挂个 long-poll 就不返回）。
	frame.OneShot = true
	// 不许把 K/V 之外的流式信号漏出去：Content-Type 保持 application/json。
	writeJTOfficialResult(w, frame)
}

// officialTerminalParams 是 terminal.attach/close/retain 的共同参数：会话 + 终端身份。
type officialTerminalParams struct {
	officialIdentity
	officialTerminalRef
}

// officialJobParams 是 job.kill / job.follow 的共同参数：会话 + 任务身份。
type officialJobParams struct {
	officialIdentity
	officialJobRef
}

// ---------------------------------------------------------------------------
// terminal.*
// ---------------------------------------------------------------------------

// HandleTerminalCreate 是官方 terminal/create。
//
// 参数两种形状都认：官方 wire 的 `{request:{shellPath?,id,cols,rows}}`，
// 以及本仓惯用的扁平 `{shellPath?,id,cols,rows}`。混用即 400。
func (h *OfficialJobTerminal) HandleTerminalCreate(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodTerminalCreate)
	var params struct {
		officialIdentity
		Request *OfficialTerminalCreateRequest `json:"request"`
		// 扁平形状（与 request 互斥）
		ShellPath string `json:"shellPath"`
		ID        string `json:"id"`
		Cols      int    `json:"cols"`
		Rows      int    `json:"rows"`
	}
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.officialIdentity.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	flatUsed := params.ShellPath != "" || params.ID != "" || params.Cols != 0 || params.Rows != 0
	var req OfficialTerminalCreateRequest
	switch {
	case params.Request != nil && flatUsed:
		writeJTOfficialError(w, method, fmt.Errorf(
			"%w: %s 的 request 与扁平字段（shellPath/id/cols/rows）不能同时用，只挑一种形状",
			ErrJTOfficialBadParam, method))
		return
	case params.Request != nil:
		req = *params.Request
	default:
		req = OfficialTerminalCreateRequest{ShellPath: params.ShellPath, ID: params.ID, Cols: params.Cols, Rows: params.Rows}
	}
	switch {
	case strings.TrimSpace(req.ID) == "":
		writeJTOfficialError(w, method, fmt.Errorf("%w: %s 缺 id（终端身份由调用方给，用于幂等）", ErrJTOfficialBadParam, method))
		return
	case req.Cols <= 0 || req.Rows <= 0:
		writeJTOfficialError(w, method, fmt.Errorf("%w: %s 的 cols/rows 必须是正整数（收到 %d×%d）",
			ErrJTOfficialBadParam, method, req.Cols, req.Rows))
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	info, err := h.Svc.TerminalCreate(r.Context(), sessionID, req)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	writeJTOfficialResult(w, info)
}

// HandleTerminalAttach 是官方 terminal/attach 的**首帧快照**。
func (h *OfficialJobTerminal) HandleTerminalAttach(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodTerminalAttach)
	var params officialTerminalParams
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.officialIdentity.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	id, err := params.officialTerminalRef.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	snap, err := h.Svc.TerminalAttach(r.Context(), sessionID, id)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	snap.Type = "snapshot"
	snap.OneShot = true
	writeJTOfficialResult(w, snap)
}

// HandleTerminalList 是官方 terminal/list（result 是数组）。
func (h *OfficialJobTerminal) HandleTerminalList(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodTerminalList)
	var params officialIdentity
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	infos, err := h.Svc.TerminalList(sessionID)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if infos == nil {
		infos = []OfficialWebTerminalInfo{}
	}
	writeJTOfficialResult(w, infos)
}

// HandleTerminalClose 是官方 terminal/close（官方：repeated closes succeed）。
func (h *OfficialJobTerminal) HandleTerminalClose(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodTerminalClose)
	var params officialTerminalParams
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.officialIdentity.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	id, err := params.officialTerminalRef.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	if err := h.Svc.TerminalClose(r.Context(), sessionID, id); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	writeJTOfficialVoid(w)
}

// HandleTerminalEnvironment 是官方 terminal/environment（6 个字段全是必填）。
func (h *OfficialJobTerminal) HandleTerminalEnvironment(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodTerminalEnvironment)
	var params officialIdentity
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	env, err := h.Svc.TerminalEnvironment(r.Context(), sessionID)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	writeJTOfficialResult(w, env)
}

// HandleTerminalShells 是官方 terminal/shells（真实探测，不是硬编码清单）。
func (h *OfficialJobTerminal) HandleTerminalShells(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodTerminalShells)
	var params officialIdentity
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	shells, err := h.Svc.TerminalShells(r.Context(), sessionID)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if shells == nil {
		shells = []OfficialTerminalShell{}
	}
	for i := range shells {
		if shells[i].Args == nil {
			shells[i].Args = []string{}
		}
	}
	writeJTOfficialResult(w, shells)
}

// OfficialTerminalTitleMaxRunes 是官方 rename 的上限（index.ts:229「at most 120 characters」）。
//
// 按**字符**而不是字节：中文标题 40 个字就 120 字节，按字节算会误判。
const OfficialTerminalTitleMaxRunes = 120

// HandleTerminalRename 是官方 terminal/rename（void 结果）。
func (h *OfficialJobTerminal) HandleTerminalRename(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodTerminalRename)
	var params struct {
		officialIdentity
		officialTerminalRef
		Title string `json:"title"`
	}
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.officialIdentity.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	id, err := params.officialTerminalRef.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	title := strings.TrimSpace(params.Title)
	switch {
	case title == "":
		writeJTOfficialError(w, method, fmt.Errorf("%w: %s 的 title 不能为空", ErrJTOfficialBadParam, method))
		return
	case utf8.RuneCountInString(title) > OfficialTerminalTitleMaxRunes:
		writeJTOfficialError(w, method, fmt.Errorf("%w: %s 的 title 最多 %d 个字符（收到 %d）",
			ErrJTOfficialBadParam, method, OfficialTerminalTitleMaxRunes, utf8.RuneCountInString(title)))
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	if err := h.Svc.TerminalRename(r.Context(), sessionID, id, title); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	writeJTOfficialVoid(w)
}

// HandleTerminalRetain 是官方 terminal/retain 的**一次性确认**。
//
// 官方是流：yield {type:'retained'} 然后一直挂着（持有保留窗口）。
// 本宿主没有长连接，所以回这一帧 + oneShot:true；真正的保留效果
// （持有期间不做无人值守回收、状态落盘）由实现负责，不靠这条 HTTP 活着。
func (h *OfficialJobTerminal) HandleTerminalRetain(w http.ResponseWriter, r *http.Request) {
	const method = string(MethodTerminalRetain)
	var params officialTerminalParams
	if err := decodeJTOfficialParams(r, &params); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	sessionID, err := params.officialIdentity.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	id, err := params.officialTerminalRef.resolve(method)
	if err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", method+" 未接线"))
		return
	}
	if err := h.Svc.TerminalRetain(r.Context(), sessionID, id); err != nil {
		writeJTOfficialError(w, method, err)
		return
	}
	writeJTOfficialResult(w, OfficialTerminalRetentionFrame{Type: "retained", OneShot: true})
}

// officialMethodFromRequest 给测试与其他调用方解出方法名（/api/<method> → <method>）。
func officialMethodFromRequest(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	// 有的服务器会拼上查询串；这里只取路径部分。
	if u, err := url.Parse(r.URL.String()); err == nil && u.Path != "" {
		return strings.TrimPrefix(u.Path, "/api/")
	}
	return strings.TrimPrefix(r.URL.Path, "/api/")
}
