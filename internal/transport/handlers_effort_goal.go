package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
)

// writeEffortGoalError 保留四字段错误信封，同时把客户端输入/非法迁移与
// 真实状态读写失败分开；持久化失败绝不返回 ok:true。
func writeEffortGoalError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrStatePersistence) {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
}

func decodeEffortGoalSessionID(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleSessionSetEffort(w http.ResponseWriter, r *http.Request) {
	if s.opt.SetSessionEffort == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("session.setEffort 未接线"))
		return
	}
	var params struct {
		ID     string          `json:"id"`
		Effort json.RawMessage `json:"effort"`
	}
	if err := decodeEffortGoalSessionID(r, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.setEffort 需要合法 JSON"))
		return
	}
	if params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.setEffort 缺 id"))
		return
	}
	if len(params.Effort) == 0 || bytes.Equal(bytes.TrimSpace(params.Effort), []byte("null")) {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.setEffort 缺 effort；null 不等于空串清除"))
		return
	}
	var effort string
	if err := json.Unmarshal(params.Effort, &effort); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.setEffort.effort 必须是 string"))
		return
	}
	if err := s.opt.SetSessionEffort(params.ID, effort); err != nil {
		writeEffortGoalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": true}})
}

func (s *Server) handleSessionGoalGet(w http.ResponseWriter, r *http.Request) {
	if s.opt.GoalGet == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("session.goal.get 未接线"))
		return
	}
	var params struct {
		ID string `json:"id"`
	}
	if err := decodeEffortGoalSessionID(r, &params); err != nil || params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.goal.get 需要非空 id"))
		return
	}
	result, err := s.opt.GoalGet(params.ID)
	if err != nil {
		writeEffortGoalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) handleSessionGoalTransition(w http.ResponseWriter, r *http.Request, resume bool) {
	callback := s.opt.GoalPause
	method := "session.goal.pause"
	if resume {
		callback = s.opt.GoalResume
		method = "session.goal.resume"
	}
	if callback == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody(method+" 未接线"))
		return
	}
	var params struct {
		ID string `json:"id"`
	}
	if err := decodeEffortGoalSessionID(r, &params); err != nil || params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", method+" 需要非空 id"))
		return
	}
	if err := callback(params.ID); err != nil {
		writeEffortGoalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": true}})
}
