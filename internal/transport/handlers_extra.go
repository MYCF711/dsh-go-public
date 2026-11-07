package transport

import (
	"encoding/json"
	"errors"
	"net/http"
)

// 本文件的错误体全部走 `errorBodyFor(码, 消息)`：码是**这一处知道的事实**
// （参数错 / 找不到 / 宿主失败），不该交给中文消息去猜。
// hint 与 retryable 由 errorBodyFor 背后的 errorObject 统一判定，这里不写第二份。

// handleSessionDelete 是 /api/session.delete。
//
// 删会话不可逆，所以这里只做三件事：校验参数、调宿主、把宿主的错误原文回给界面。
// **不在协议层自作主张**（不缓存、不"删不动就当成功"）。
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if s.opt.Delete == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("session.delete 未接线"))
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	// `id` 是同一个东西的另一个名字：界面那边两种写法都有（审计实测）。
	// 只认一种的后果是 400，而调用方以为自己传对了。
	if req.SessionID == "" {
		req.SessionID = req.ID
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 sessionId（或 id）"))
		return
	}
	if err := s.opt.Delete(req.SessionID); err != nil {
		// 找不到会话和删失败必须分开：前者是 404（界面换个 id 重试），
		// 后者是 500（界面该提示"这次没删掉"，并且**可以重试**）。
		if errors.Is(err, ErrNoSuchSession) {
			writeJSON(w, http.StatusNotFound, errorBodyFor("no_such_session", "会话不存在"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "删除失败："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleSessionFork 是 /api/session.fork。新回调接收并确认真实生效的
// anchor/title；只有 sessionId 的旧回调与旧无参数结果原样保留。
func (s *Server) handleSessionFork(w http.ResponseWriter, r *http.Request) {
	if s.opt.Fork == nil && s.opt.ForkAnchored == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("session.fork 未接线"))
		return
	}
	var req struct {
		SessionID string  `json:"sessionId"`
		ID        string  `json:"id"`
		Anchor    *string `json:"anchor"`
		Title     *string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if req.SessionID == "" {
		req.SessionID = req.ID
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 sessionId（或 id）"))
		return
	}
	if req.Anchor != nil && *req.Anchor == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "anchor 不能为空"))
		return
	}
	if s.opt.ForkAnchored != nil {
		res, err := s.opt.ForkAnchored(req.SessionID, req.Anchor, req.Title)
		if err != nil {
			writeSessionOperationError(w, err, "分支失败：")
			return
		}
		if res.SessionID == "" || res.SessionID == req.SessionID ||
			(req.Anchor != nil && (res.Anchor == nil || *res.Anchor != *req.Anchor)) ||
			(req.Title != nil && (res.Title == nil || *res.Title != *req.Title)) {
			writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "分支失败：宿主没有确认实际生效的 sessionId/anchor/title"))
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	title := ""
	if req.Title != nil {
		title = *req.Title
	}
	id, err := s.opt.Fork(req.SessionID, title)
	if err != nil {
		writeSessionOperationError(w, err, "分支失败：")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": id})
}

func writeSessionOperationError(w http.ResponseWriter, err error, prefix string) {
	switch {
	case errors.Is(err, ErrNoSuchSession):
		writeJSON(w, http.StatusNotFound, errorBodyFor("no_such_session", "会话不存在"))
	case errors.Is(err, ErrInvalidAnchor), errors.Is(err, ErrInvalidCursor):
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", prefix+err.Error()))
	}
}

func (s *Server) handleSessionRewindPreview(w http.ResponseWriter, r *http.Request) {
	if s.opt.RewindPreview == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", "session.rewind.preview 未接线"))
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
		Anchor    string `json:"anchor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if req.SessionID == "" {
		req.SessionID = req.ID
	}
	if req.SessionID == "" || req.Anchor == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "rewind preview 需要 id 与非空 anchor"))
		return
	}
	res, err := s.opt.RewindPreview(req.SessionID, req.Anchor)
	if err != nil {
		writeSessionOperationError(w, err, "rewind preview 失败：")
		return
	}
	if res.FilesChanged == nil {
		res.FilesChanged = []string{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSessionRewindApply(w http.ResponseWriter, r *http.Request) {
	if s.opt.RewindApply == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", "session.rewind.apply 未接线"))
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
		Anchor    string `json:"anchor"`
		Mode      string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if req.SessionID == "" {
		req.SessionID = req.ID
	}
	if req.SessionID == "" || req.Anchor == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "rewind apply 需要 id 与非空 anchor"))
		return
	}
	if req.Mode != "conversation" && req.Mode != "files" && req.Mode != "both" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "rewind mode 必须是 conversation/files/both"))
		return
	}
	res, err := s.opt.RewindApply(req.SessionID, req.Anchor, req.Mode)
	if err != nil {
		writeSessionOperationError(w, err, "rewind apply 失败：")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSessionTranscriptPage(w http.ResponseWriter, r *http.Request) {
	if s.opt.TranscriptPage == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", "session.transcript.page 未接线"))
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
		Cursor    string `json:"cursor"`
		Limit     int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if req.SessionID == "" {
		req.SessionID = req.ID
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "transcript.page 需要 id"))
		return
	}
	if req.Limit == 0 {
		req.Limit = 50
	}
	if req.Limit < 0 || req.Limit > 200 {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "transcript.page limit 必须在 1..200"))
		return
	}
	res, err := s.opt.TranscriptPage(req.SessionID, req.Cursor, req.Limit)
	if err != nil {
		writeSessionOperationError(w, err, "transcript.page 失败：")
		return
	}
	if res.Messages == nil {
		res.Messages = []Message{}
	}
	writeJSON(w, http.StatusOK, res)
}

// handleSessionCompact 是 /api/session.compact：让界面能主动压一次。
//
// 两条要求：
//   - 没接线就回 501 并说清"是宿主没实现"，不是"你这个会话有问题"；
//   - "现在不需要压"回 200 + compacted=false——那是正常结果，
//     不是失败（会话还没长到该压的时候）。
func (s *Server) handleSessionCompact(w http.ResponseWriter, r *http.Request) {
	if s.opt.Compact == nil {
		// 码显式给 not_implemented：这条消息里没有"未接线"三个字，
		// 交给 errorCode 去猜会落到兜底的 `error`，客户端看不出"换个宿主才有"。
		writeJSON(w, http.StatusNotImplemented,
			errorBodyFor("not_implemented", "这个宿主还没实现 session.compact（压缩仍可在 CLI 用 dshd session compact）"))
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
		Keep      int    `json:"keep"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	sid := req.SessionID
	if sid == "" {
		sid = req.ID
	}
	if sid == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 sessionId"))
		return
	}
	if req.Keep <= 0 {
		req.Keep = 6
	}
	res, err := s.opt.Compact(sid, req.Keep)
	if err != nil {
		// 压缩失败要让人知道**是哪一步**：读日志、生成摘要、还是写盘。
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "压缩失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, res)
}
