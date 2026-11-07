package officialwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 官方 wire 的三条固定路径（EVIDENCE §1、§3、§4）。
const (
	// ConnectPath 是换 cookie 的引导入口。
	ConnectPath = "/connect"
	// MuxPath 是远程流复用端点。
	MuxPath = "/api/remote.mux"
	// APIPrefix 是 unary 通道前缀。
	APIPrefix = "/api/"
)

// defaultMaxBodyBytes 是 unary 请求体上限。
//
// 官方默认 300 MiB（EVIDENCE §5.3：为 base64 图片聚合留余量）。照抄它而不是
// 自己拍一个更小的数：客户端可能在一次 session/prompt 里带几张图，
// 上限比官方小就会变成"官方能过、我们不能过"的假故障。
const defaultMaxBodyBytes = 300 << 20

// Binding 是一个官方方法的宿主实现。
type Binding struct {
	// Descriptor 是 args 的字段集；nil 时只允许空 args（见 ValidateArgs）。
	Descriptor *Descriptor
	// RequireAnswer 为 true 时只有 interactions=answer 的 Peer 能调
	// （EVIDENCE §3.2：`$events/result` 必须有它）。
	RequireAnswer bool
	// Handle 返回 result.value；业务失败返回 *Error（HTTP 仍是 200）。
	Handle func(ctx context.Context, args map[string]json.RawMessage) (any, *Error)
}

// ServerConfig 是 officialwire.Server 的全部外部输入。
type ServerConfig struct {
	// Secret 是 cookie 的 HMAC 密钥（至少 16 字节）。
	Secret []byte
	// TokenSource 返回 launch token（每次 /connect 现取）。
	TokenSource func() string
	// TrustedHosts / BindHost 参与 Host 围栏。
	TrustedHosts []string
	BindHost     string
	// Home 只用于 `$events` ready 帧。
	Home string
	// MaxBodyBytes 缺省 300 MiB。
	MaxBodyBytes int64
	// Heartbeat / UplinkBytes 是 mux 参数（缺省 2s / 256 KiB）。
	Heartbeat   time.Duration
	UplinkBytes int
	// Opener 是 mux 的流工厂。
	Opener StreamOpener
	// Events 是 `$events` 的宿主转发事件源；nil = 本宿主没有事件源
	// （`$events` 只发 ready，`$events/result` 回 gateway/method-unavailable）。
	Events EventSource
	// RawUpload 是上行裸字节路由（/api/session/uploadFileBinary）的宿主实现。
	// nil 时该路由回 501 + gateway/method-unavailable（明确未实现）。
	RawUpload RawUploadSink
	// MaxUploadBytes 是上行裸字节的上限，缺省 300 MiB（EVIDENCE §5.3）。
	MaxUploadBytes int64
	// CookieMaxAge 缺省 30 天；AllowTokenReuse 缺省 false（一次性 token）。
	CookieMaxAge    time.Duration
	AllowTokenReuse bool
	// Now / NewClientID 可注入。
	Now         func() time.Time
	NewClientID func() string
}

// Server 是官方 wire 的服务端：unary + /connect + mux。
type Server struct {
	cfg  ServerConfig
	auth *Auth
	peer *Peers
	mux  *Mux

	mu       sync.RWMutex
	bindings map[string]Binding
}

// NewServer 造服务端。Secret 缺失或过短直接报错——不退化成无签名 cookie。
func NewServer(cfg ServerConfig) (*Server, *Error) {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = defaultMaxBodyBytes
	}
	auth, err := NewAuth(AuthConfig{
		Secret:         cfg.Secret,
		TokenSource:    cfg.TokenSource,
		TrustedHosts:   cfg.TrustedHosts,
		BindHost:       cfg.BindHost,
		MaxAge:          cfg.CookieMaxAge,
		AllowTokenReuse: cfg.AllowTokenReuse,
		Now:            cfg.Now,
	})
	if err != nil {
		return nil, err
	}
	peers := NewPeers()
	mux, merr := NewMux(MuxConfig{
		Auth:        auth,
		Peers:       peers,
		Opener:      cfg.Opener,
		Events:      cfg.Events,
		Home:        cfg.Home,
		Heartbeat:   cfg.Heartbeat,
		UplinkBytes: cfg.UplinkBytes,
		NewClientID: cfg.NewClientID,
	})
	if merr != nil {
		return nil, merr
	}
	return &Server{cfg: cfg, auth: auth, peer: peers, mux: mux, bindings: map[string]Binding{}}, nil
}

// Handle 注册一个官方方法。endpoint 形状必须是 `<namespace>/<method>`。
func (s *Server) Handle(endpoint string, binding Binding) *Error {
	if err := ValidateEndpoint(endpoint); err != nil {
		return err
	}
	s.mu.Lock()
	s.bindings[endpoint] = binding
	s.mu.Unlock()
	return nil
}

// Binding 取一个已注册的方法。
func (s *Server) Binding(endpoint string) (Binding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	binding, ok := s.bindings[endpoint]
	return binding, ok
}

// Endpoints 返回已注册的 endpoint（按注册顺序不稳定，仅供诊断）。
func (s *Server) Endpoints() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.bindings))
	for endpoint := range s.bindings {
		out = append(out, endpoint)
	}
	return out
}

// Handler 把本服务端挂到既有 handler 前面。
//
// 返回的 Router 在 Enabled=true 时只截走三条：`/connect?interactions=…`、
// `/api/remote.mux`、两段的 `/api/<ns>/<m>`；**其余全部原样交给 legacy**。
func (s *Server) Handler(legacy http.Handler) http.Handler {
	return Router{Enabled: true, Legacy: legacy, Wire: s}
}

// Router 是"官方 wire + 既有 L1"的复合分派。
//
// Enabled=false 时它**退化成 legacy 本身**：这是"关闭时字节不变"的构造性保证
// （不是"我们尽量不改"）——开关关掉时本包一行代码都不跑。
type Router struct {
	Enabled bool
	Legacy  http.Handler
	Wire    *Server
}

// ServeHTTP 按路径形状分派：两段 /api → 官方 unary；否则 legacy。
func (rt Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !rt.Enabled || rt.Wire == nil {
		rt.serveLegacy(w, r)
		return
	}
	switch {
	case r.URL.Path == ConnectPath && isOfficialConnect(r):
		rt.Wire.serveConnect(w, r)
	case r.URL.Path == MuxPath:
		rt.Wire.serveMux(w, r)
	case r.URL.Path == UploadPath:
		// exact 路由，body 是裸字节：必须排在两段 /api 分派之前，
		// 否则它会被当成 `session/uploadFileBinary` 的 JSON 信封调用。
		rt.Wire.serveRawUpload(w, r)
	case strings.HasPrefix(r.URL.Path, APIPrefix):
		if endpoint, ok := officialEndpoint(strings.TrimPrefix(r.URL.Path, APIPrefix)); ok {
			rt.Wire.serveUnary(w, r, endpoint)
			return
		}
		rt.serveLegacy(w, r)
	default:
		rt.serveLegacy(w, r)
	}
}

func (rt Router) serveLegacy(w http.ResponseWriter, r *http.Request) {
	if rt.Legacy == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rt.Legacy.ServeHTTP(w, r)
}

// isOfficialConnect 判断这次 /connect 是不是官方的（带 interactions 就是）。
//
// L1 的 `/connect?token=` 不带 interactions，因此两条引导路径不会互相吞掉。
func isOfficialConnect(r *http.Request) bool {
	_, ok := r.URL.Query()["interactions"]
	return ok
}

// officialEndpoint 判断去掉 `/api/` 之后的剩余路径是不是 `<ns>/<method>`。
//
// 判据照官方（EVIDENCE §1）：恰好两段、每段匹配 `[A-Za-z0-9_$.-]+`、
// 不许空段、`.`、`..`。`$events/result` 里的 `$` 也在这个字符集里。
func officialEndpoint(rest string) (string, bool) {
	segments := strings.Split(rest, "/")
	if len(segments) != 2 {
		return "", false
	}
	for _, segment := range segments {
		if !validSegment(segment) {
			return "", false
		}
	}
	return segments[0] + "/" + segments[1], true
}

func validSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '$', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateEndpoint 校验 `<namespace>/<method>` 的字面形状。
func ValidateEndpoint(endpoint string) *Error {
	segments := strings.Split(endpoint, "/")
	if len(segments) != 2 || !validSegment(segments[0]) || !validSegment(segments[1]) {
		return Errorf(CodeBadRequest, "官方 endpoint 必须是 <namespace>/<method>：%q", endpoint)
	}
	return nil
}

// identity 是一次请求解析出来的身份。
type identity struct {
	cred *Credential
	peer *Peer
}

// authorize 做围栏 + cookie 校验 + Peer 存活检查。
func (s *Server) authorize(r *http.Request) (*identity, *Error) {
	if ferr := s.auth.Fence(r); ferr != nil {
		return nil, ferr
	}
	cred, err := s.auth.Verify(r)
	if err != nil {
		return nil, err
	}
	peer := s.peer.Get(cred.ID)
	if peer == nil {
		return nil, &Error{Code: CodePeerDisposed,
			Message: "这个 Peer 已经被释放（重新 GET /connect 换一份 cookie）",
			Details: map[string]any{"peerId": cred.ID}}
	}
	return &identity{cred: cred, peer: peer}, nil
}

// serveConnect 是官方引导：`GET /connect?token=&interactions=none|answer`
// → 302 `./api/remote.mux` + v2 cookie（EVIDENCE §4.1）。
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// interactions 必须恰好出现一次且取值合法，否则 400（EVIDENCE §4.1）。
	values, present := r.URL.Query()["interactions"]
	if !present || len(values) != 1 {
		http.Error(w, "bad request: interactions 必须出现恰好一次", http.StatusBadRequest)
		return
	}
	interactions := strings.TrimSpace(values[0])
	if interactions != InteractionsNone && interactions != InteractionsAnswer {
		http.Error(w, "bad request: interactions 只能是 none 或 answer", http.StatusBadRequest)
		return
	}
	if ferr := s.auth.Fence(r); ferr != nil {
		s.writeDenied(w, ferr)
		return
	}
	if terr := s.auth.ConsumeToken(r); terr != nil {
		s.writeDenied(w, terr)
		return
	}
	peer, perr := s.peer.New(interactions)
	if perr != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cookie, cerr := s.auth.Issue(r, peer.ID)
	if cerr != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, cookie)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", "./api/remote.mux")
	w.WriteHeader(http.StatusFound)
}

// serveMux 处理 `/api/remote.mux` 的升级。
func (s *Server) serveMux(w http.ResponseWriter, r *http.Request) {
	identified, err := s.authorize(r)
	if err != nil {
		s.writeDenied(w, err)
		return
	}
	s.mux.Serve(w, r, identified.cred)
}

// serveUnary 处理 `POST /api/<ns>/<m>`。
//
// 状态码分工（EVIDENCE §6）：
//
//	非 POST            → 404
//	content-type 不是 json → 415
//	body 不是 JSON 对象 → 400
//	body 超限          → 413
//	未认证             → 401
//	Host/Origin 不可信  → 403
//	处理器 panic        → 500 handler failure
//	**其它一切**        → 200 + `result.ok:false`
func (s *Server) serveUnary(w http.ResponseWriter, r *http.Request, endpoint string) {
	if r.Method != http.MethodPost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type: expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	identified, authErr := s.authorize(r)
	if authErr != nil {
		s.writeDenied(w, authErr)
		return
	}
	body, ok := readBodyLimited(r, s.cfg.MaxBodyBytes)
	if !ok {
		// 官方：413 + connection: close + 销毁请求（EVIDENCE §5.3）。
		w.Header().Set("Connection", "close")
		http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
		return
	}
	request, parseErr := ParseClientRequest(endpoint, body)
	if request == nil {
		// 连 JSON 对象都不是：400（EVIDENCE §6）。
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if parseErr != nil {
		s.writeFailure(w, request.RPCID, parseErr)
		return
	}
	binding, bound := s.Binding(endpoint)
	if !bound {
		s.writeFailure(w, request.RPCID, UnknownMethod(endpoint))
		return
	}
	if binding.RequireAnswer && !identified.peer.Answering() {
		s.writeFailure(w, request.RPCID, &Error{Code: CodeInteractionsForbidden,
			Message: "这个方法的 Peer 必须声明 interactions=answer",
			Details: map[string]any{"endpoint": endpoint}})
		return
	}
	if argErr := ValidateArgs(binding.Descriptor, endpoint, request.Args); argErr != nil {
		s.writeFailure(w, request.RPCID, argErr)
		return
	}
	if binding.Handle == nil {
		s.writeFailure(w, request.RPCID, MethodUnavailable(endpoint, "这个方法没有实现"))
		return
	}
	value, failure, panicMsg := invokeBinding(r.Context(), binding, endpoint, request.Args)
	if panicMsg != "" {
		// 处理器异常是**传输/处理器层**的错误，按官方走 500（EVIDENCE §6）。
		http.Error(w, "handler failure: "+panicMsg, http.StatusInternalServerError)
		return
	}
	if failure != nil {
		s.writeFailure(w, request.RPCID, failure)
		return
	}
	s.writeValue(w, request.RPCID, value)
}

// invokeBinding 调用处理器并把 panic 折成字符串（不许让 panic 冒到 net/http，
// 那会直接掐掉连接 —— 对客户端就是"偶发断连"，查不出来）。
func invokeBinding(ctx context.Context, binding Binding, endpoint string, args map[string]json.RawMessage) (value any, failure *Error, panicMsg string) {
	defer func() {
		if rec := recover(); rec != nil {
			value, failure = nil, nil
			panicMsg = fmt.Sprintf("%v", rec)
		}
	}()
	value, failure = binding.Handle(ctx, args)
	return value, failure, ""
}

// writeValue 写成功响应（有二进制时自动走 multipart）。
func (s *Server) writeValue(w http.ResponseWriter, rpcID string, value any) {
	contentType, body, err := EncodeResult(rpcID, value)
	if err != nil {
		http.Error(w, "encoder failure: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeFailure 写业务失败：**HTTP 200** + `{ok:false,error}`。
func (s *Server) writeFailure(w http.ResponseWriter, rpcID string, failure *Error) {
	body, err := EncodeFailure(rpcID, failure)
	if err != nil {
		http.Error(w, "encoder failure: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// writeDenied 写认证/围栏拒绝：401 或 403，正文是官方错误形状（方便客户端打印）。
func (s *Server) writeDenied(w http.ResponseWriter, failure *Error) {
	status := http.StatusUnauthorized
	if failure.Code == CodeAccessDenied {
		switch reason, _ := failure.Details["reason"].(string); reason {
		case "untrusted-host", "origin-mismatch", "cross-site", "forbidden":
			status = http.StatusForbidden
		}
	}
	body, _ := json.Marshal(failure)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// readBodyLimited 读请求体；超限或读失败返回 ok=false。
func readBodyLimited(r *http.Request, limit int64) ([]byte, bool) {
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

// isJSONContentType 判断 content-type 是不是 application/json。
//
// 官方判据是"必须是 application/json"，否则 415（EVIDENCE §6）；
// 带 charset 参数的 `application/json; charset=utf-8` 依然算它。
func isJSONContentType(value string) bool {
	mediaType := strings.TrimSpace(strings.ToLower(value))
	if index := strings.IndexByte(mediaType, ';'); index >= 0 {
		mediaType = strings.TrimSpace(mediaType[:index])
	}
	return mediaType == "application/json"
}

// errNoSink 是"没有可达的处理器"的内部哨兵（仅供 Proxy 用）。
var errNoSink = errors.New("officialwire: 没有下游处理器")
