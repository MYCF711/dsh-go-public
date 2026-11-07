package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// metadataCommit 把 mutation 的“已 durable”事实接到同一 Transport journal。
// 只有 journal 追加成功后才 Publish；这样 WS 永远不会先于可恢复的真实提交。
func (s *Server) metadataCommit(sessionID, typ string, payload any) (func() error, func() bool, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("序列化 %s change: %w", typ, err)
	}
	committed := false
	commit := func() error {
		if committed {
			return fmt.Errorf("%s change 重复提交", typ)
		}
		now := time.Now().UnixMilli()
		frame := ServerFrame{Type: typ, Data: data, Time: now}
		if s.opt.Journals != nil {
			var err error
			frame, err = s.opt.Journals.Append(sessionID, typ, data, now)
			if err != nil {
				return err
			}
		}
		s.Publish("session:"+sessionID, frame)
		committed = true
		return nil
	}
	return commit, func() bool { return committed }, nil
}

func metadataID(sessionID, id string) string {
	if sessionID != "" {
		return sessionID
	}
	return id
}

func (s *Server) handleSessionRename(w http.ResponseWriter, r *http.Request) {
	if s.opt.RenameSession == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", "session.rename 未接线"))
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
		Title     string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.rename 需要合法 JSON"))
		return
	}
	req.SessionID = metadataID(req.SessionID, req.ID)
	if req.SessionID == "" || strings.TrimSpace(req.Title) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.rename 需要 id 与非空白 title"))
		return
	}
	commit, committed, err := s.metadataCommit(req.SessionID, "session/title", map[string]any{
		"title": req.Title, "messageSeqs": []int64{}, "source": map[string]any{"kind": "user"},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
		return
	}
	if err := s.opt.RenameSession(req.SessionID, req.Title, commit); err != nil {
		writeSessionOperationError(w, err, "改名失败：")
		return
	}
	if !committed() {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "改名失败：宿主没有确认 durable change 已入 journal"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": req.SessionID, "title": req.Title})
}

func (s *Server) handleSessionColorGet(w http.ResponseWriter, r *http.Request) {
	if s.opt.GetSessionColor == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", "session.color.get 未接线"))
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.color.get 需要合法 JSON"))
		return
	}
	req.SessionID = metadataID(req.SessionID, req.ID)
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.color.get 需要 id"))
		return
	}
	color, err := s.opt.GetSessionColor(req.SessionID)
	if err != nil {
		writeSessionOperationError(w, err, "读取 accent 失败：")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"color": color})
}

func (s *Server) handleSessionColorSet(w http.ResponseWriter, r *http.Request) {
	if s.opt.SetSessionColor == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", "session.color.set 未接线"))
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
		Color     string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.color.set 需要合法 JSON"))
		return
	}
	req.SessionID = metadataID(req.SessionID, req.ID)
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.color.set 需要 id"))
		return
	}
	commit, committed, err := s.metadataCommit(req.SessionID, "session/color", map[string]any{"color": req.Color})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
		return
	}
	if err := s.opt.SetSessionColor(req.SessionID, req.Color, commit); err != nil {
		writeSessionOperationError(w, err, "写 accent 失败：")
		return
	}
	if !committed() {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "写 accent 失败：宿主没有确认 durable change 已入 journal"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"color": req.Color})
}

func (s *Server) handleWorkspaceInit(w http.ResponseWriter, r *http.Request) {
	if s.opt.WorkspaceInit == nil {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", "workspace.init 未接线"))
		return
	}
	var req struct {
		CWD string `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.CWD) == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "workspace.init 需要非空 cwd 与合法 JSON"))
		return
	}
	report, err := s.opt.WorkspaceInit(req.CWD)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "workspace.init 失败："+err.Error()))
		return
	}
	if err := validateWorkspaceInitReport(report); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func validateWorkspaceInitReport(report WorkspaceInitResult) error {
	if len(report.Files) == 0 {
		return fmt.Errorf("workspace.init 返回了空 files，不能冒充成功 no-op")
	}
	for i, file := range report.Files {
		if file.Path == "" {
			return fmt.Errorf("workspace.init files[%d] 缺 path", i)
		}
		switch file.Status {
		case "created", "unchanged":
			if file.Error != "" {
				return fmt.Errorf("workspace.init files[%d] 非 failed 却带 error", i)
			}
		case "failed":
			if file.Error == "" {
				return fmt.Errorf("workspace.init files[%d] failed 缺真实 error", i)
			}
		default:
			return fmt.Errorf("workspace.init files[%d] status=%q 非法", i, file.Status)
		}
	}
	return nil
}
