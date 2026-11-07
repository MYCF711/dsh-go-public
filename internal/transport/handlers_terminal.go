package transport

import (
	"encoding/json"
	"net/http"
)

// TerminalOutput 是 `terminal.output` 的返回。
type TerminalOutput struct {
	Output string `json:"output"`
	// Next 是下次调用该传的 `since`（字节偏移）。
	// **增量而不是全量**：一个跑了半小时的终端，每帧整段传会把界面拖死。
	Next int  `json:"next"`
	Dead bool `json:"dead"`
}

// handleTerminalOpen 是 /api/terminal.open：起一条长连接 shell。
//
// 参数 `cwd` 可选（空 = 宿主当前工作目录）。
func (s *Server) handleTerminalOpen(w http.ResponseWriter, r *http.Request) {
	if s.opt.TerminalOpen == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("terminal.open 未接线"))
		return
	}
	var params struct {
		Cwd string `json:"cwd"`
	}
	_ = json.NewDecoder(r.Body).Decode(&params) // 空 body 合法（用默认目录）
	id, err := s.opt.TerminalOpen(params.Cwd)
	if err != nil {
		// **502 而不是 500**：起不来说明底层 shell 有问题（不是我们的代码错）。
		// 码按 internal 报：这是宿主这一侧起不来，客户端重试是合理的。
		writeJSON(w, http.StatusBadGateway, errorBodyFor("internal", "起终端失败："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"id": id}})
}

// handleTerminalInput 是 /api/terminal.input：往终端写一段（原样，含换行）。
func (s *Server) handleTerminalInput(w http.ResponseWriter, r *http.Request) {
	if s.opt.TerminalInput == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("terminal.input 未接线"))
		return
	}
	var params struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 id：往哪个终端写"))
		return
	}
	if err := s.opt.TerminalInput(params.ID, params.Data); err != nil {
		// 404 只表示"这个终端不在了"；别的失败（写不动）不该混进这个码里。
		writeJSON(w, http.StatusNotFound, errorBodyFor("not_found", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": true}})
}

// handleTerminalOutput 是 /api/terminal.output：取增量输出。
//
// 参数 `since` 是上次拿到的 `next`（0 = 从头）。
func (s *Server) handleTerminalOutput(w http.ResponseWriter, r *http.Request) {
	if s.opt.TerminalOutput == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("terminal.output 未接线"))
		return
	}
	var params struct {
		ID    string `json:"id"`
		Since int    `json:"since"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if params.ID == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 id：取哪个终端的输出"))
		return
	}
	out, err := s.opt.TerminalOutput(params.ID, params.Since)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBodyFor("not_found", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": out})
}
