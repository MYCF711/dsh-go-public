package officialwire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// cookie 前缀与版本。证据 EVIDENCE §4.2：
//
//	Set-Cookie: dsh-auth-<base64url(sha256(authority))>=v2.<body>.<sig>
//	             ; Max-Age=<秒>; Path=/; Expires=<UTC>; HttpOnly; SameSite=Strict[; Secure]
//
// **cookie 名必须是无填充 base64url**：cookie-name 是 RFC6265 的 token，
// 不允许 `=`；带填充会生成一个客户端根本存不下的 name。
const (
	cookiePrefix  = "dsh-auth-"
	cookieVersion = "v2"
)

// Interactions 是 /connect 的 interactions 取值（EVIDENCE §3.2、§4.1）。
const (
	InteractionsNone   = "none"
	InteractionsAnswer = "answer"
)

// Credential 是一份通过校验的 API Peers 凭据（v2 payload 的服务端投影）。
type Credential struct {
	// ID 是 Peer id（payload v2 比 v1 多出来的那个字段）。
	ID string
	// Authority 是签发时的 authority：换 authority（Host 变了）就不认。
	Authority string
	// IssuedAt / ExpiresAt 来自 payload。
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// AuthConfig 是认证层的全部外部输入。
type AuthConfig struct {
	// Secret 是 HMAC 密钥（生产由宿主持久化 32 字节）。
	Secret []byte
	// TokenSource 返回当前 launch token；每次 /connect 现取，便于壳轮换。
	TokenSource func() string
	// TrustedHosts 是额外允许的 Host（EVIDENCE §4.3 Host 围栏的第三档）。
	TrustedHosts []string
	// BindHost 是监听器的 bind 字面量（Host 等于它也算可信）。
	BindHost string
	// MaxAge 是 cookie 寿命，缺省 30 天（EVIDENCE §4.2）。
	MaxAge time.Duration
	// AllowTokenReuse 为 true 时同一个 launch token 可以换多次。
	// 默认 false：与 L1 /connect 的一次性语义一致（换过再换 = 401）。
	AllowTokenReuse bool
	// Now 可注入时钟（测试用）。
	Now func() time.Time
}

// Auth 负责 authority 绑定的 HMAC cookie 与 host/origin 围栏。
type Auth struct {
	cfg AuthConfig

	mu     sync.Mutex
	used   map[string]struct{}
	secret []byte
}

// NewAuth 校验配置并造认证层。
func NewAuth(cfg AuthConfig) (*Auth, *Error) {
	if len(cfg.Secret) < 16 {
		return nil, Errorf(CodeInternal, "officialwire: HMAC secret 至少要 16 字节，实际 %d", len(cfg.Secret))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 30 * 24 * time.Hour
	}
	return &Auth{
		cfg:    cfg,
		used:   map[string]struct{}{},
		secret: append([]byte(nil), cfg.Secret...),
	}, nil
}

// Authority 返回这次请求的 authority。
//
// EVIDENCE §4.2：HTTP 下是 `host[:port]`（取 Host 头），TLS 下是 origin。
func Authority(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := strings.TrimSpace(r.Host)
	if r.TLS != nil {
		return "https://" + host
	}
	return host
}

// CookieName 是 authority 绑定的 cookie 名。
func CookieName(authority string) string {
	sum := sha256.Sum256([]byte(authority))
	return cookiePrefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

// Fence 做 Host / Origin / sec-fetch-site 三档围栏（EVIDENCE §4.3）。
//
// 返回的 *Error 直接对应 HTTP 403：围栏是**认证之前**的拒绝，不打 cookie 的主意。
func (a *Auth) Fence(r *http.Request) *Error {
	host := strings.TrimSpace(r.Host)
	if host == "" || !a.hostTrusted(host) {
		return &Error{Code: CodeAccessDenied,
			Message: "不可信的 Host：" + host,
			Details: map[string]any{"action": "request", "reason": "untrusted-host"}}
	}
	// 浏览器标记：跨站一律拒（EVIDENCE §4.3）。
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return &Error{Code: CodeAccessDenied,
			Message: "拒绝跨站请求（sec-fetch-site: cross-site）",
			Details: map[string]any{"action": "request", "reason": "cross-site"}}
	}
	// 给了 Origin 就必须与 Host 同 authority（EVIDENCE §4.3）。
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "null" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, host) {
			return &Error{Code: CodeAccessDenied,
				Message: "Origin 与 Host 不是同一 authority：" + origin,
				Details: map[string]any{"action": "request", "reason": "origin-mismatch"}}
		}
	}
	return nil
}

func (a *Auth) hostTrusted(host string) bool {
	if host == a.cfg.BindHost {
		return true
	}
	for _, want := range a.cfg.TrustedHosts {
		if strings.EqualFold(host, want) {
			return true
		}
	}
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.Trim(hostname, "[]")
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// ConsumeToken 校验 launch token。返回 nil 表示通过。
//
// 一次性语义与 L1 /connect 一致：换过之后再用一律 401（EVIDENCE §4.1 的 401 分支）。
func (a *Auth) ConsumeToken(r *http.Request) *Error {
	want := ""
	if a.cfg.TokenSource != nil {
		want = a.cfg.TokenSource()
	}
	got := strings.TrimSpace(r.URL.Query().Get("token"))
	if want == "" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return &Error{Code: CodeAccessDenied, Message: "unauthorized",
			Details: map[string]any{"action": "connect", "reason": "bad-token"}}
	}
	if a.cfg.AllowTokenReuse {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, used := a.used[got]; used {
		return &Error{Code: CodeAccessDenied, Message: "unauthorized（launch token 已用过）",
			Details: map[string]any{"action": "connect", "reason": "token-reused"}}
	}
	a.used[got] = struct{}{}
	return nil
}

// Issue 造一份 v2 cookie（EVIDENCE §4.2）。
func (a *Auth) Issue(r *http.Request, peerID string) (*http.Cookie, *Error) {
	authority := Authority(r)
	if authority == "" {
		return nil, Errorf(CodeInternal, "officialwire: 请求没有 Host，无法绑定 authority")
	}
	now := a.cfg.Now()
	payload := struct {
		Version   int    `json:"version"`
		Authority string `json:"authority"`
		IssuedAt  int64  `json:"issuedAt"`
		ExpiresAt int64  `json:"expiresAt"`
		ID        string `json:"id"`
	}{
		Version:   2,
		Authority: authority,
		IssuedAt:  now.UnixMilli(),
		ExpiresAt: now.Add(a.cfg.MaxAge).UnixMilli(),
		ID:        peerID,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, Errorf(CodeInternal, "officialwire: cookie payload 序列化失败：%v", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	sig := a.sign(body)
	cookie := &http.Cookie{
		Name:     CookieName(authority),
		Value:    cookieVersion + "." + body + "." + sig,
		Path:     "/",
		MaxAge:   int(a.cfg.MaxAge / time.Second),
		Expires:  now.Add(a.cfg.MaxAge).UTC(),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	}
	return cookie, nil
}

// Verify 校验请求带的 cookie，返回凭据。
//
// 校验顺序与官方一致：名字按当前 authority 算 → 三段形状 → HMAC → payload →
// authority 相等 → 未过期。任何一步失败都是 401（不告诉调用方失败在哪一步）。
func (a *Auth) Verify(r *http.Request) (*Credential, *Error) {
	authority := Authority(r)
	unauthorized := &Error{Code: CodeAccessDenied, Message: "unauthorized",
		Details: map[string]any{"action": "request", "reason": "unauthenticated"}}
	cookie, err := r.Cookie(CookieName(authority))
	if err != nil || cookie == nil || cookie.Value == "" {
		return nil, unauthorized
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 || parts[0] != cookieVersion {
		return nil, unauthorized
	}
	body, sig := parts[1], parts[2]
	if subtle.ConstantTimeCompare([]byte(sig), []byte(a.sign(body))) != 1 {
		return nil, unauthorized
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, unauthorized
	}
	var payload struct {
		Version   int    `json:"version"`
		Authority string `json:"authority"`
		IssuedAt  int64  `json:"issuedAt"`
		ExpiresAt int64  `json:"expiresAt"`
		ID        string `json:"id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Version != 2 {
		return nil, unauthorized
	}
	if payload.Authority != authority || payload.ID == "" {
		return nil, unauthorized
	}
	now := a.cfg.Now()
	if payload.ExpiresAt <= now.UnixMilli() {
		return nil, unauthorized
	}
	return &Credential{
		ID:        payload.ID,
		Authority: payload.Authority,
		IssuedAt:  time.UnixMilli(payload.IssuedAt),
		ExpiresAt: time.UnixMilli(payload.ExpiresAt),
	}, nil
}

// sign 对 body 段做 HMAC-SHA256，返回无填充 base64url。
//
// 签的是**编码后的 body 段**（不是解码后的 JSON）：验证时只需重算一次、
// 不必先解码，也就没有"解码器与签名器对字节理解不同"的可乘之机。
func (a *Auth) sign(body string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Peer 是一个 API Peer（v2 凭据指向的那个身份）。
//
// 证据 EVIDENCE §3.3：一个 Peer 同时只允许**一条** API socket，第二次升级 409。
type Peer struct {
	// ID 是 Peer id（UUID v4）。
	ID string
	// Interactions 只有 answer 型 Peer 能收/答转发事件（EVIDENCE §3.2）。
	Interactions string

	mu        sync.Mutex
	socket    bool
	disposed  bool
	disposeCh chan struct{}
}

// Answering 表示这个 Peer 声明过 interactions=answer。
func (p *Peer) Answering() bool {
	return p != nil && p.Interactions == InteractionsAnswer
}

// Peers 是 Peer 表。
type Peers struct {
	mu sync.Mutex
	m  map[string]*Peer
}

// NewPeers 造一张空表。
func NewPeers() *Peers { return &Peers{m: map[string]*Peer{}} }

// New 造一个 Peer（UUID v4）。random 为 nil 时用 crypto/rand。
func (p *Peers) New(interactions string) (*Peer, *Error) {
	id, err := newUUID()
	if err != nil {
		return nil, Errorf(CodeInternal, "officialwire: 生成 peer id 失败：%v", err)
	}
	peer := &Peer{ID: id, Interactions: interactions, disposeCh: make(chan struct{})}
	p.mu.Lock()
	p.m[id] = peer
	p.mu.Unlock()
	return peer, nil
}

// Get 按 id 取 Peer；不存在返回 nil。
func (p *Peers) Get(id string) *Peer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m[id]
}

// Attach 给 Peer 挂 socket。已有 socket 时返回 false（调用方回 409）。
func (p *Peers) Attach(peer *Peer) bool {
	if peer == nil {
		return false
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.socket || peer.disposed {
		return false
	}
	peer.socket = true
	return true
}

// Detach 摘掉 socket。
func (p *Peers) Detach(peer *Peer) {
	if peer == nil {
		return
	}
	peer.mu.Lock()
	peer.socket = false
	peer.mu.Unlock()
}

// Dispose 释放 Peer 作用域：活跃 socket 会以 1001 关闭（EVIDENCE §3.3）。
func (p *Peers) Dispose(peer *Peer) {
	if peer == nil {
		return
	}
	peer.mu.Lock()
	if peer.disposed {
		peer.mu.Unlock()
		return
	}
	peer.disposed = true
	close(peer.disposeCh)
	peer.mu.Unlock()
	p.mu.Lock()
	delete(p.m, peer.ID)
	p.mu.Unlock()
}

// Disposed 返回一个在 Peer 释放时关闭的通道。
func (p *Peer) Disposed() <-chan struct{} { return p.disposeCh }

// newUUID 生成 RFC4122 v4。
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:]), nil
}
