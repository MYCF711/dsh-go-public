package transport

import (
	"encoding/json"
	"net/http"
	"strings"
)

// maxGoalLen 是一条 goal 的长度上限（按 rune 算，不是字节）。
//
// 500 是"一句话"的上限——GoalBar 停靠在 composer 上方，超过两三行就没有
// "一眼看到我现在在干嘛"的作用了。超出**截断而不是报错**：用户写长了
// 不该收到一个错误弹窗，他该看到自己的话被削短了。
const maxGoalLen = 500

// handleSessionSetGoal 是 /api/session.setGoal。
//
// 参数：`id`（必填）+ `goal`（空串 = 清除）。
func (s *Server) handleSessionSetGoal(w http.ResponseWriter, r *http.Request) {
	if s.opt.SetSessionGoal == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("session.setGoal 未接线"))
		return
	}
	var params struct {
		ID   string `json:"id"`
		Goal string `json:"goal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 id：给哪个会话设目标"))
		return
	}
	goal := strings.TrimSpace(params.Goal)
	if n := len([]rune(goal)); n > maxGoalLen {
		goal = string([]rune(goal)[:maxGoalLen])
	}
	if err := s.opt.SetSessionGoal(params.ID, goal); err != nil {
		// 参数/迁移错误维持既有 400；真实 state/journal 提交失败必须是 500。
		writeEffortGoalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{
		"ok":   true,
		"goal": goal, // 回显**截断之后**的值：界面该用这个，不是它发上来的那个
	}})
}
