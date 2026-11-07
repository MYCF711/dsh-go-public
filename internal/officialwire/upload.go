package officialwire

// 上行裸字节路由（`POST /api/session/uploadFileBinary`）。
//
// 证据：EVIDENCE §5.3 —— 文件上传是**独立 exact 路由**，body 是原始字节
// （不是 base64、不是 envelope），单请求体默认上限 300 MiB。
//
// 为什么单列一层：它是 exact Fetch 路由，和 typert unary 的 envelope 是两套
// 编码。把它塞进 unary 分派会让"JSON 信封"这条规则出现例外，而例外正是
// 客户端与宿主对不上的地方。
//
// 本包里它是**纯编解码 + 路由**：字节进来、交给宿主注入的 sink、把 sink 给的
// 字节原样回出去。没有 sink 就明确回 501（不假装成功——上传"成功"但没存下来
// 是最坏的一种静默失败）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// UploadPath 是上行裸字节的 exact 路由。
const UploadPath = "/api/session/uploadFileBinary"

// RawUploadRequest 是解码后的一次上传。
type RawUploadRequest struct {
	// Name 是查询参数 name（官方包自己决定文件名；没给就是空）。
	Name string
	// SessionID 是查询参数 sessionId（给了就带上，没给就是空）。
	SessionID string
	// ContentType 是请求的 content-type（原始值）。
	ContentType string
	// Body 是**完整裸字节**（二进制安全，不做任何转码）。
	Body []byte
}

// RawUploadResponse 是要回给客户端的字节。
//
// 用字节对字节（而不是让它返回 `any` 再套 JSON）是刻意的：这个路由的响应
// 形状没有逐字取证（EVIDENCE U1/U5），套一个我们猜的 JSON 壳等于凭空发明契约。
// 宿主返回什么字节就发什么字节，形状由宿主负责。
type RawUploadResponse struct {
	ContentType string
	Body        []byte
}

// RawUploadSink 由宿主实现（真实业务：写盘后登记进既有 attachment store）。
//
// 返回 *Error 时按**业务失败**处理（HTTP 200 + `{ok:false,error}`），
// 只有认证/传输/处理器异常才是 4xx/5xx——与 unary 同一套状态码规则。
type RawUploadSink interface {
	Upload(ctx context.Context, request RawUploadRequest) (RawUploadResponse, *Error)
}

// DecodeRawUpload 读裸字节体（二进制安全）。ok=false 表示超限或读失败。
//
// 它就是这条路由的**解码层**：不解 base64、不剥 JSON、不按文本规范化。
func DecodeRawUpload(r *http.Request, limit int64) ([]byte, bool) {
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	if r.Body == nil {
		return nil, true
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, false
	}
	if int64(len(data)) > limit {
		return nil, false
	}
	return data, true
}

// EncodeRawUpload 是**编码层**（客户端侧/测试侧）：字节原样作为 body。
//
// 它在这里的意义是"契约只此一条"：上行不 base64、不 JSON 包一层。任何
// 需要包装的调用方都是在发明第二套编码。
func EncodeRawUpload(content []byte) io.Reader {
	return &rawUploadReader{data: content}
}

type rawUploadReader struct {
	data []byte
	read bool
}

func (r *rawUploadReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	if n < len(r.data) {
		r.data = r.data[n:]
		return n, nil
	}
	r.read = true
	return n, nil
}

// serveRawUpload 处理上行裸字节路由。
func (s *Server) serveRawUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, failure := s.authorize(r); failure != nil {
		s.writeDenied(w, failure)
		return
	}
	if s.cfg.RawUpload == nil {
		// 明确未实现：501 + 官方错误形状。不用 404 —— 那是"没有这条路由"，
		// 而这条路由**存在**，只是本宿主没有真实业务可映射。
		body, _ := json.Marshal(MethodUnavailable("session/uploadFileBinary",
			"本宿主没有接线裸字节上传：没有可映射的 attachment store 或写盘入口"))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write(append(body, '\n'))
		return
	}
	body, ok := DecodeRawUpload(r, s.cfg.MaxUploadBytes)
	if !ok {
		w.Header().Set("Connection", "close")
		http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
		return
	}
	response, failure := s.cfg.RawUpload.Upload(r.Context(), RawUploadRequest{
		Name:        r.URL.Query().Get("name"),
		SessionID:   r.URL.Query().Get("sessionId"),
		ContentType: r.Header.Get("Content-Type"),
		Body:        body,
	})
	if failure != nil {
		s.writeFailure(w, "", failure)
		return
	}
	contentType := response.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response.Body)
}
