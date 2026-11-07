package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// JobInfo 是 `jobs.list` 返回的一行。
//
// 它和 `spi.JobInfo` 字段一致但**是这个包自己的类型**：协议层不 import 内核包
// （和 Message/LoadResult 的处理方式一样）。两边的对应关系由接线处那一层负责，
// 字段名一旦分叉，`jobs.list` 的快照测试会立刻红。
type JobInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId,omitempty"`
	Command   string `json:"command"`
	Done      bool   `json:"done"`
	ExitCode  int    `json:"exit_code"`
	Bytes     int    `json:"bytes"`
}

var (
	ErrNoSuchJob        = errors.New("no such job")
	ErrBadJobCursor     = errors.New("job output cursor beyond current output")
	ErrJobFinished      = errors.New("job already finished")
	ErrCrossSession     = errors.New("job belongs to another session or home")
	ErrNoSuchSchedule   = errors.New("no such schedule")
	ErrRevisionConflict = errors.New("schedule revision conflict")
)

// JobOutput 是 `jobs.output` 的返回。游标以 Unicode code point 为单位，
// 因而分页不会把 UTF-8 中文或 emoji 切成半个字符。Output 永远只是一页。
type JobOutput struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionId,omitempty"`
	Output     string `json:"output"`
	Done       bool   `json:"done"`
	ExitCode   int    `json:"exit_code"`
	Cwd        string `json:"cwd,omitempty"`
	Cursor     int    `json:"cursor"`
	NextCursor int    `json:"nextCursor"`
	HasMore    bool   `json:"hasMore"`
	Total      int    `json:"total"`
	// Truncated=true 表示执行层 1 MiB 保留窗口之前已有输出被淘汰。
	// Total 始终是当前保留文本的 rune 数，不是含淘汰前缀的 SPI 总字节数。
	Truncated bool `json:"truncated,omitempty"`
}

// JobStopResult 只在所属进程确实停止后返回 stopped=true。
type JobStopResult struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Stopped   bool   `json:"stopped"`
	Done      bool   `json:"done"`
	ExitCode  int    `json:"exit_code"`
}

const (
	DefaultJobOutputLimit = 16 * 1024
	MaxJobOutputLimit     = 64 * 1024
)

// handleJobsList 是 /api/jobs.list。
//
// 没接线 → 501（这个宿主没有 exec 模块，不是我们的 bug）。
func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	if s.opt.JobsList == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("jobs.list 未接线"))
		return
	}
	jobs := s.opt.JobsList()
	if jobs == nil {
		// nil 会被编成 null，界面就得同时处理 null 和 []。归一成空数组。
		jobs = []JobInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"jobs": jobs}})
}

// handleJobsOutput 是 /api/jobs.output。既有 id/waitMs 仍可用；生产 scoped 回调
// 另收 sessionId/cursor/limit，让子进程 IPC 与 HTTP 都只携带当前页，而不是先传全量再截断。
func (s *Server) handleJobsOutput(w http.ResponseWriter, r *http.Request) {
	var params struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
		WaitMs    int    `json:"waitMs"`
		Cursor    int    `json:"cursor"`
		Limit     int    `json:"limit"`
	}
	if err := decodeTaskActionParams(r, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	if params.ID == "" || params.Cursor < 0 || params.Limit < 0 {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "id 必填，cursor/limit 不能为负数"))
		return
	}
	if s.opt.JobsOutput == nil && s.opt.JobsOutputScoped == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("jobs.output 未接线"))
		return
	}
	if s.opt.JobsOutputScoped != nil && params.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "生产 jobs.output 必须带 sessionId，拒绝跨会话猜任务"))
		return
	}
	wait := time.Duration(params.WaitMs) * time.Millisecond
	if wait < 0 {
		wait = 0
	}
	if wait > 10*time.Second {
		wait = 10 * time.Second
	}
	limit := params.Limit
	if limit == 0 {
		limit = DefaultJobOutputLimit
	}
	if limit > MaxJobOutputLimit {
		limit = MaxJobOutputLimit
	}
	var out JobOutput
	var err error
	if s.opt.JobsOutputScoped != nil {
		out, err = s.opt.JobsOutputScoped(params.SessionID, params.ID, params.Cursor, limit, wait)
		if err == nil {
			err = validateJobOutputPage(out, params.Cursor, limit)
		}
	} else {
		out, err = s.opt.JobsOutput(params.ID, wait)
		if err == nil && params.Cursor > len([]rune(out.Output)) {
			err = ErrBadJobCursor
		}
		if err == nil {
			out = PageJobOutput(out, params.Cursor, limit)
		}
	}
	if err != nil {
		writeTaskActionError(w, err)
		return
	}
	out.ID = params.ID
	out.SessionID = params.SessionID
	writeJSON(w, http.StatusOK, map[string]any{"result": out})
}

// PageJobOutput 是 HTTP 与父子 IPC 共用的 rune 游标分页实现。
func PageJobOutput(out JobOutput, cursor, limit int) JobOutput {
	runes := []rune(out.Output)
	total := len(runes)
	if cursor > total {
		cursor = total
	}
	end := cursor + limit
	if end > total {
		end = total
	}
	out.Output = string(runes[cursor:end])
	out.Cursor = cursor
	out.NextCursor = end
	out.Total = total
	out.HasMore = end < total
	return out
}

func validateJobOutputPage(out JobOutput, cursor, limit int) error {
	n := len([]rune(out.Output))
	if out.Cursor != cursor || n > limit || out.Total < 0 || out.NextCursor != cursor+n || out.NextCursor > out.Total || out.HasMore != (out.NextCursor < out.Total) {
		return fmt.Errorf("jobs.output scoped 回调返回了越界或不一致的分页：%+v", out)
	}
	return nil
}

func decodeTaskActionParams(r *http.Request, target any) error {
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("请求体必须是裸 JSON 参数对象: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("请求体只能有一个 JSON 参数对象")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("请求体必须是裸 JSON 参数对象，不能是 null、数组或标量")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(target); err != nil {
		return fmt.Errorf("裸参数含未知字段或类型错误: %w", err)
	}
	return nil
}

func writeTaskActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrBadJobCursor):
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
	case errors.Is(err, ErrNoSuchJob), errors.Is(err, ErrNoSuchSchedule):
		writeJSON(w, http.StatusNotFound, errorBodyFor("not_found", err.Error()))
	case errors.Is(err, ErrRevisionConflict):
		writeJSON(w, http.StatusConflict, errorBodyFor("revision_conflict", err.Error()))
	case errors.Is(err, ErrJobFinished):
		writeJSON(w, http.StatusConflict, errorBodyFor("job_finished", err.Error()))
	case errors.Is(err, ErrCrossSession):
		writeJSON(w, http.StatusForbidden, errorBodyFor("cross_session", err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
	}
}

// handleJobsStop 只停止调用方明确命名的 session+id；不接受进程名、镜像名或全局 PID。
func (s *Server) handleJobsStop(w http.ResponseWriter, r *http.Request) {
	var params struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
	}
	if err := decodeTaskActionParams(r, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	if params.SessionID == "" || params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "jobs.stop 必须同时带 sessionId 与 id"))
		return
	}
	if s.opt.JobsStop == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("jobs.stop 未接线"))
		return
	}
	result, err := s.opt.JobsStop(params.SessionID, params.ID)
	if err != nil {
		writeTaskActionError(w, err)
		return
	}
	if !result.Stopped || !result.Done {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "停止动作未确认进程已结束，不能返回 stopped=true"))
		return
	}
	result.SessionID, result.ID = params.SessionID, params.ID
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// ScheduleItem 是 `schedule.list` 的一行。
//
// 和 JobInfo 同理：协议层自己的类型，不 import internal/schedule。
type ScheduleItem struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId,omitempty"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	NextAt    string `json:"next_at"`
	EverySec  int    `json:"every_seconds,omitempty"`
	DailyAt   string `json:"daily_at,omitempty"`
	WeeklyAt  string `json:"weekly_at,omitempty"`
	Status    string `json:"status,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	Revision  string `json:"revision,omitempty"`
}

type ScheduleDeleteResult struct {
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	Deleted   bool   `json:"deleted"`
	Remaining int    `json:"remaining"`
	Revision  string `json:"revision"`
}

type ScheduleClearResult struct {
	Deleted   int               `json:"deleted"`
	Revisions map[string]string `json:"revisions"`
	Schedules []ScheduleItem    `json:"schedules"`
}

// handleScheduleList 是 /api/schedule.list。
func (s *Server) handleScheduleList(w http.ResponseWriter, r *http.Request) {
	if s.opt.ScheduleList == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("schedule.list 未接线"))
		return
	}
	items := s.opt.ScheduleList()
	if items == nil {
		items = []ScheduleItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"schedules": items}})
}

func (s *Server) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	var params struct {
		SessionID        string `json:"sessionId"`
		ID               string `json:"id"`
		ExpectedRevision string `json:"expectedRevision"`
	}
	if err := decodeTaskActionParams(r, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	if params.SessionID == "" || params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "schedule.delete 必须同时带 sessionId 与 id"))
		return
	}
	if s.opt.ScheduleDelete == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("schedule.delete 未接线"))
		return
	}
	result, err := s.opt.ScheduleDelete(params.SessionID, params.ID, params.ExpectedRevision)
	if err != nil {
		writeTaskActionError(w, err)
		return
	}
	if !result.Deleted {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "存储没有确认删除，不能返回 deleted=true"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) handleScheduleClear(w http.ResponseWriter, r *http.Request) {
	var params struct{}
	if err := decodeTaskActionParams(r, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	if s.opt.ScheduleClear == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("schedule.clear 未接线"))
		return
	}
	result, err := s.opt.ScheduleClear()
	if err != nil {
		writeTaskActionError(w, err)
		return
	}
	if result.Schedules == nil {
		result.Schedules = []ScheduleItem{}
	}
	if result.Revisions == nil {
		result.Revisions = map[string]string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// handleSessionSetModel 是 /api/session.setModel。
//
// 参数：`id`（必填）+ `model`（必填，空串表示"清除覆盖、回到默认"）。
func (s *Server) handleSessionSetModel(w http.ResponseWriter, r *http.Request) {
	if s.opt.SetSessionModel == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("session.setModel 未接线"))
		return
	}
	var params struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 id：要改哪个会话"))
		return
	}
	if err := s.opt.SetSessionModel(params.ID, params.Model); err != nil {
		// 400 是既有约定：改不动通常是 model/id 认不出来（客户端该改请求）。
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": true}})
}

// handleSessionSetPermission 是 /api/session.setPermission。
//
// 参数：`id`（必填）+ `permission`（deny/ask/allow/auto；空串 = 清除覆盖）。
// **值要白名单校验**：写进 store 之后会被 serve 拿去当审批策略用，
// 收一个 `"block"` 这种没定义的词，后果是那一轮的行为没人说得清。
func (s *Server) handleSessionSetPermission(w http.ResponseWriter, r *http.Request) {
	if s.opt.SetSessionPermission == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("session.setPermission 未接线"))
		return
	}
	var params struct {
		ID         string `json:"id"`
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 id：要改哪个会话"))
		return
	}
	switch params.Permission {
	case "", "deny", "ask", "allow", "auto",
		// 官方 0.2.2 的 sandbox 三档：它们不是审批策略，而是会话的沙盒档位。
		// 收下它们必须由宿主的 sandbox 策略层解释（只收窄不放宽），不能当审批词写进 store。
		"read-only", "workspace-write", "danger-full-access":
	default:
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request",
			"permission 只能是 deny/ask/allow/auto 或 read-only/workspace-write/danger-full-access（或空串清除），收到 "+params.Permission))
		return
	}
	// 有 sandbox 策略层时优先走它：它会返回真实生效档位，且只允许收窄。
	if s.opt.SetSessionPermissionMode != nil {
		result, err := s.opt.SetSessionPermissionMode(params.ID, params.Permission)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
			return
		}
		if result == nil {
			result = map[string]any{"ok": true}
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": result})
		return
	}
	if err := s.opt.SetSessionPermission(params.ID, params.Permission); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": true}})
}

// ModelEntry 是 `models.list` 的一行。
type ModelEntry struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// handleModelsList 是 /api/models.list。
//
// **失败不回空列表**：空列表在界面上等于"没有可用模型"，而真实原因可能是
// 网关没起、凭据过期。两者长得一样，但用户该做的事完全不同。
func (s *Server) handleModelsList(w http.ResponseWriter, r *http.Request) {
	if s.opt.ModelsList == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("models.list 未接线"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := s.opt.ModelsList(ctx)
	if err != nil {
		// 502：网关是上游，不是我们的代码；按 internal 判为可重试（网关抖完就好）。
		writeJSON(w, http.StatusBadGateway, errorBodyFor("internal", "列模型失败："+err.Error()))
		return
	}
	if list == nil {
		list = []ModelEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"models": list}})
}
