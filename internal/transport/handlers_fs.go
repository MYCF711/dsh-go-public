package transport

import (
	"encoding/json"
	"errors"
	"net/http"
)

// handleFSRead 是 /api/fs.read：把工作区里一个文件的内容交给界面预览。
//
// 这一层只做三件事：校验形状、转给宿主、把结果（或失败）如实说回去。
// **路径解析、大小上限、越界检查都不在这里**——那是宿主 FSRead 的事。
// 协议层一旦自己拼路径，就会和宿主的"工作区"定义长出第二个版本，
// 迟早出现"界面读得到、工具读不到"这种对不上的怪事。
//
// 四种失败分开说，因为界面的下一步动作各不相同（码也一并区分：
// 客户端按码分支，不该去读中文消息）：
//   - 没接线 → 501 not_implemented（宿主没给实现，重试没意义）
//   - 形状不对 → 400 bad_request（界面改请求）
//   - 文件不存在 → 404 not_found（界面换个路径，别当成宿主坏了）
//   - 其它错误 → 500 internal，带宿主原文（"读不动"和"是空的"必须能分辨；
//     这类是宿主自己出错，按 internal 判为可重试）
func (s *Server) handleFSRead(w http.ResponseWriter, r *http.Request) {
	if s.opt.FSRead == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("fs.read 未接线"))
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 path"))
		return
	}

	content, err := s.opt.FSRead(req.Path)
	if err != nil {
		if errors.Is(err, ErrNoSuchFile) {
			writeJSON(w, http.StatusNotFound, errorBodyFor("not_found", "文件不存在"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "读失败："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, content)
}

// handleFSWrite 是 /api/fs.write：把界面上编辑后的内容写回工作区。
//
// 写是不可逆的一侧，所以这里同样不自作主张：不建目录、不补换行、
// 不把"写失败"当成功。content 缺省即空串——界面清空文件是一个合法意图，
// 不能和"没给 content"混为一谈再去猜。
//
// 成功时只回 {"written": true}：实际字节数、落盘路径以宿主为准，
// 这一层不复述，免得两份数字不一致时没人知道该信哪个。
func (s *Server) handleFSWrite(w http.ResponseWriter, r *http.Request) {
	if s.opt.FSWrite == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("fs.write 未接线"))
		return
	}

	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 path"))
		return
	}

	if err := s.opt.FSWrite(req.Path, req.Content); err != nil {
		if errors.Is(err, ErrNoSuchFile) {
			writeJSON(w, http.StatusNotFound, errorBodyFor("not_found", "文件不存在"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "写失败："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"written": true})
}
