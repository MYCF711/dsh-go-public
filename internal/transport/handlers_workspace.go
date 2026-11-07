package transport

import (
	"encoding/json"
	"net/http"
)

// handleWorkspaceList 是 /api/workspace.list。
//
// 它只回答一件事：当前工作区的顶层有哪些条目。名字与 isDir 由宿主的
// WorkspaceList 决定，这一层不自己读盘——协议层一旦能碰文件系统，
// "工作区"这个概念就会有两个实现，迟早对不上。
//
// 三种结果分开说：
//   - 没接线 → 501（调用方需要宿主补实现，重试没有意义）
//   - 宿主报错 → 500，带上宿主原文（"列不动"和"是空的"必须能分辨）
//   - 成功 → 200；空工作区给空数组，不给 null
func (s *Server) handleWorkspaceList(w http.ResponseWriter, r *http.Request) {
	if s.opt.WorkspaceList == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("workspace.list 未接线"))
		return
	}

	// path 可选：不传就列进程工作目录（旧行为）。
	var params struct {
		Path string `json:"path"`
	}
	// 空 body 也算合法：旧调用方不发参数，那 path 就是空串。
	_ = json.NewDecoder(r.Body).Decode(&params)
	entries, err := s.opt.WorkspaceList(params.Path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "列工作区失败："+err.Error()))
		return
	}

	// nil 切片会被 encoding/json 编成 null，界面那边就得同时处理 null 和 []。
	// 空工作区在这里归一成空数组，让界面只认一种形状。
	if entries == nil {
		entries = []WorkspaceEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
