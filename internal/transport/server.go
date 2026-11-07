package transport

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 界面线的三条铁律，本文件是它们的落点：
//
//  1. 端口一律由 OS 分配（默认 "127.0.0.1:0"），不许写死；
//  2. 壳从 Start 返回的地址拿 URL，不自己拼；
//  3. launch token 经 GET /connect 换 HttpOnly cookie，cookie 永不进 WebView。
//
// 本文件只做协议翻译与认证，不读盘、不 import 会话编解码包（红线 R7）。
// 会话数据的来源由宿主注入（Options.Source / Options.List）。

const (
	// defaultAddr 是开始监听的默认地址：端口 0 = 交给 OS 分配。
	defaultAddr = "127.0.0.1:0"

	// cookieName 是会话 cookie 的名字。
	cookieName = "dshd_session"

	// secretBytes 是 launch token 与 cookie 值的随机字节数。
	// 32 字节 → 64 位 hex，满足「≥32 字符 hex」。
	secretBytes = 32
)

// Options 起服务要的东西。
type Options struct {
	// Source 提供会话数据；nil 时 /api/session.list 返回空数组（测试用）。
	Source Source
	// ContentWidth 是会话内容列宽（0 = 不知道，界面自己算）。由壳/宿主注入。
	ContentWidth int
	// Policy 是宿主这一进程实际用的审批策略（deny/ask/allow/auto）。
	//
	// 它是**进程级事实**，不是会话级"预设"——H5 要的是"当前权限状态"，
	// 而本仓根本没有 preset 这个概念（preset 在我们的代码里指 mode，那是工具可见性）。
	// 所以这里如实返回 policy 名字；没有就留空，界面整段不画。
	Policy string
	// Addr 是监听地址，留空则 "127.0.0.1:0"（端口由 OS 分配）。
	Addr string
	// List 列会话的摘要；真正实现由宿主注入，这里不直接读盘。
	// 返回 nil 时按空数组处理。
	List func() ([]SessionSummary, error)
	// Load 取首屏（最新 limit 条）；宿主注入。
	Load func(sessionID string, limit int) (LoadResult, error)
	// History 取断流之后补洞的那些（seq > sinceSeq）；宿主注入。
	History func(sessionID string, sinceSeq, limit int) (Page, error)
	// Compact 主动压一次；宿主注入。nil 时 session.compact 回 501 并说清原因。
	Compact func(sessionID string, keep int) (CompactResult, error)
	// Prompt 发起一轮，立刻返回（回答走 WS）；宿主注入。
	Prompt func(sessionID, text string) (PromptResult, error)
	// PromptPlacement 是 additive placement 入口。缺省 placement 仍走旧 Prompt，
	// 因而旧宿主/旧测试完全不变；非 turn 在新回调缺失时明确 501。
	PromptPlacement func(sessionID, text, placement string) (PromptResult, error)
	// PromptBlocks 是 additive 有序内容块入口。仅当请求带 content 时调用；
	// 旧 text 请求始终走上面的 Prompt，保持既有 runner 与客户端完全兼容。
	PromptBlocks func(sessionID string, blocks []ContentBlock) (PromptResult, error)
	// PromptBlocksPlacement 是 content 的 additive placement 入口。
	PromptBlocksPlacement func(sessionID string, blocks []ContentBlock, placement string) (PromptResult, error)
	// Attachments 提供授权读取与受控字节存储；nil 时两个 additive 方法回 501。
	Attachments AttachmentStore
	// OfficialJobTerminal 是官方 job.*/terminal.* 11 个方法的路由层（0.2.2 对齐）。
	// nil 时这些方法回 501 并说清未接线；既有 jobs.*/terminal.* 方法不受影响。
	OfficialJobTerminal *OfficialJobTerminal
	// Delete 删掉一个会话；宿主注入（删除是不可逆操作，由宿主决定物理删还是标记）。
	Delete func(sessionID string) error
	// Fork 从一个会话开分支；宿主注入。保留给旧宿主/旧测试。
	Fork func(sessionID, title string) (string, error)
	// ForkAnchored 是 additive 实现。生产宿主优先走这里；旧回调仍保留，
	// 缺新回调时旧宿主忽略 title 的行为不被 Transport 静默改写成已确认。
	ForkAnchored func(sessionID string, anchor, title *string) (ForkResult, error)
	// RewindPreview/RewindApply 是真实 checkpoint 与 cut-before-anchor 入口。
	RewindPreview func(sessionID, anchor string) (RewindPreview, error)
	RewindApply   func(sessionID, anchor, mode string) (RewindResult, error)
	// TranscriptPage 读 durable v4 投影，不允许用 Transport journal/history 代替。
	TranscriptPage func(sessionID, cursor string, limit int) (TranscriptPageResult, error)
	// RenameSession / SetSessionColor 的 commit 必须在 durable 写成功后调用，
	// 并返回其结果。生产宿主把 durable 写入与 commit 放在同一 per-home 锁内，
	// 因而并发 A/B 写入的 WS 顺序不会反超磁盘真值。Transport 在 commit 内先追加
	// 同一 Transport journal，成功后才向 WS 发布；任一步失败都不返回成功 ACK。
	RenameSession   func(sessionID, title string, commit func() error) error
	GetSessionColor func(sessionID string) (string, error)
	SetSessionColor func(sessionID, color string, commit func() error) error
	// WorkspaceInit 由宿主真实独占创建模板并逐项报告，不由 Transport 碰文件系统。
	WorkspaceInit func(cwd string) (WorkspaceInitResult, error)
	// WorkspaceList 列当前工作区的顶层条目；宿主注入。
	// WorkspaceList 列目录。**path 为空 = 进程工作目录**（向后兼容旧调用）。
	//
	// 为什么收 path：只列根目录做不出文件树——界面点进子目录时就断了。
	// 收了口之后调用方传绝对路径，就能一层层往下走。
	WorkspaceList func(path string) ([]WorkspaceEntry, error)
	// JobsList 列后台任务；JobsOutput 取某个任务的输出。
	// **nil = 这个宿主没有后台任务能力**（比如不带 exec 模块），回 501 不是 500。
	JobsList   func() []JobInfo
	JobsOutput func(id string, wait time.Duration) (JobOutput, error)
	// JobsOutputScoped 是真实分页/会话隔离入口；cursor/limit 单位是 Unicode code point。
	// 非 nil 时生产路径必须带 sessionId，旧 JobsOutput 只作兼容 fallback。
	JobsOutputScoped func(sessionID, id string, cursor, limit int, wait time.Duration) (JobOutput, error)
	JobsStop         func(sessionID, id string) (JobStopResult, error)
	// ScheduleList 列定时任务。nil = 这个宿主没有 schedule 能力（回 501）。
	ScheduleList   func() []ScheduleItem
	ScheduleDelete func(sessionID, id, expectedRevision string) (ScheduleDeleteResult, error)
	ScheduleClear  func() (ScheduleClearResult, error)
	// SetSessionModel 改一个会话的模型覆盖。nil = 这个宿主不支持（回 501）。
	SetSessionModel func(sessionID, model string) error
	// SetSessionPermission 改一个会话的审批策略覆盖。nil = 不支持（回 501）。
	SetSessionPermission func(sessionID, perm string) error
	// SetSessionPermissionMode 是 SetSessionPermission 的加法版本：除了写覆盖，
	// 还回真实生效的 sandbox 档位（官方 0.2.2 三档：read-only/workspace-write/
	// danger-full-access）。nil 时退回上面的旧回调，响应保持 {ok:true}。
	SetSessionPermissionMode func(sessionID, perm string) (map[string]any, error)
	// HandlerWrap 让宿主在 HTTP 层外面再包一层（官方 0.2.2 wire 兼容用）。
	// nil 或返回 nil 时**一个字节都不变**：Start 直接用 Server 自己当 handler。
	HandlerWrap func(*Server) http.Handler
	// ModelsList 列网关上的模型。nil = 这个宿主没接模型出口（回 501）。
	ModelsList func(ctx context.Context) ([]ModelEntry, error)
	// SetSessionGoal 设/清一个会话的 goal。nil = 不支持（回 501）。
	SetSessionGoal func(sessionID, goal string) error
	// SetSessionEffort 按当前模型 catalog 校验并持久化会话 effort 覆盖。
	SetSessionEffort func(sessionID, effort string) error
	// GoalGet/Pause/Resume 读取或迁移真实 snapshot；ACK 本身不是 change event。
	GoalGet    func(sessionID string) (GoalResult, error)
	GoalPause  func(sessionID string) error
	GoalResume func(sessionID string) error
	// 长连接终端。nil = 这个宿主不给终端（回 501）。
	TerminalOpen   func(cwd string) (string, error)
	TerminalInput  func(id, data string) error
	TerminalOutput func(id string, since int) (TerminalOutput, error)
	// FSRead 读一个文件；路径解析、大小上限、越界检查都在宿主那边。
	FSRead func(path string) (FSContent, error)
	// FSWrite 写一个文件；宿主注入。
	FSWrite func(path, content string) error
	// ConfigGet 取全部界面配置；宿主注入。
	ConfigGet func() (map[string]string, error)
	// ConfigSet 改若干项（只覆盖给出的 key）；宿主注入。
	ConfigSet func(kv map[string]string) error
	// Cancel 取消在飞的一轮；没有在飞的返回 ErrNoSuchTurn。
	Cancel func(sessionID string) error
	// CancelPlacement 是 additive cancel receipt 入口。cause=user 保留队列，
	// interrupt/switch/dispose 清队列；turnID 可只撤一个 queued 项。
	CancelPlacement func(sessionID, cause, turnID string) (CancelResult, error)
	// Journals 是每会话的序号域（H14）。非 nil 时 session.history 直接读它，
	// 保证与 WS 推送同一个 seq 域；nil 时退回宿主注入的 History（老行为）。
	Journals *Journals
	// PendingApprovals 给出某会话上仍挂着的审批帧，供新连接订阅时重放（H13）。
	PendingApprovals func(sessionID string) []ServerFrame
	// Approve 回一次审批（H13）：requestID 是服务端推给客户端的那个，optionID 是客户端选的。
	Approve func(requestID, optionID string) error
	// SetMode 换模式；宿主注入。
	Create func(mode string) (string, error)
	// PluginHandler 是 additive plugin.* 适配器。它只在下面的既有 cookie
	// 认证成功后调用；nil 时保持闭合分派的 501。handler 自己限定八个方法，
	// 这里不复制第二份方法表，也不把裸 handler 注册成独立路由。
	PluginHandler http.Handler
	// SettingsFormHandler 是 additive ConfigForm 适配器，同样只在 cookie
	// 认证成功后调用。它不改 SessionMethods 的 canonical 25。
	SettingsFormHandler http.Handler
	// ModelCatalogHandler 是 additive models.catalog.* 适配器。它在同一
	// cookie 门后分派，不改变 canonical models.list 的网关语义。
	ModelCatalogHandler http.Handler
	// W149 additive 能力；全部 nil 时保持原有 501 闭合分派。
	ContextUsage func(sessionID, detail string) (ContextUsageView, error)
	MCPList      func(sessionID string) ([]MCPServerView, error)
	MCPReconnect func(ctx context.Context, sessionID, name string) error
	MCPToggle    func(ctx context.Context, sessionID, name string, enabled bool) error
	SideQuery    func(ctx context.Context, sessionID, requestID, prompt string, onText func(string) error) (string, error)
	SideCancel   func(sessionID, requestID string) error
	SideClose    func(sessionID string) error
}

// Server 是界面线的宿主入口。
type Server struct {
	hub   *Hub
	opt   Options
	token string // 一次性 launch token，只经 Token() 交出

	mu      sync.Mutex
	used    bool                // launch token 是否已被兑换（一次性）
	cookies map[string]struct{} // 已发放的有效会话 cookie
	srv     *http.Server
	url     string
}

// NewServer 造一个还没开始监听的服务，并生成一次性 launch token。
// 返回的 token 只会出现在这一处——调用方把它交给壳，壳用它换 cookie。
func NewServer(opt Options) (*Server, error) {
	token, err := newSecret()
	if err != nil {
		return nil, err
	}
	h := NewHub()
	// 订阅时要核对世代（H14），所以哈希表和序号域必须成对存在。
	h.SetJournals(opt.Journals)
	h.SetPendingReplay(opt.PendingApprovals)
	return &Server{
		hub:     h,
		opt:     opt,
		token:   token,
		cookies: make(map[string]struct{}),
	}, nil
}

// Token 给壳用：壳拿它去 GET /connect 换 cookie，之后再不出现。
func (s *Server) Token() string {
	if s == nil {
		return ""
	}
	return s.token
}

// Start 开始监听 opt 里的地址（默认 "127.0.0.1:0"）并返回真实地址，
// 形如 "http://127.0.0.1:52341"。重复调用返回同一个地址。
func (s *Server) Start() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srv != nil {
		return s.url, nil
	}

	addr := s.opt.Addr
	if addr == "" {
		addr = defaultAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}

	var handler http.Handler = s
	if s.opt.HandlerWrap != nil {
		if wrapped := s.opt.HandlerWrap(s); wrapped != nil {
			handler = wrapped
		}
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.srv = srv
	s.url = baseURL(ln.Addr())

	go func() {
		// 监听已被 Close 时这里返回 ErrServerClosed，不是错误。
		_ = srv.Serve(ln)
	}()
	return s.url, nil
}

// Close 关掉监听。没 Start 过时是空操作。
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	return srv.Close()
}

// ServeHTTP 只有三条路由，别加。
//
//	GET  /connect?token=<launch>
//	POST /api/<method>
//	其它 404
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/connect":
		s.handleConnect(w, r)
	case r.URL.Path == "/debug/stats":
		s.handleDebugStats(w, r)
	case r.URL.Path == "/ws":
		s.handleWS(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		s.handleAPI(w, r)
	default:
		writeJSON(w, http.StatusNotFound, errorBody("not found"))
	}
}

// Publish 往某个 topic 推一帧。宿主侧在 turn 产生事件时调它。
func (s *Server) Publish(topic string, f ServerFrame) {
	if s == nil || s.hub == nil {
		return
	}
	s.hub.Publish(topic, f)
}

// handleDebugStats 暴露两个"长跑才会出问题"的数：goroutine 数与堆大小。
//
// 为什么需要它：serve 会为每一轮起 follow goroutine。
// 它有没有按时退出，从外面完全看不出来——除非把数打出来。
// 要 cookie：这不是给公网看的东西。
func (s *Server) handleDebugStats(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, errorBody("unauthorized"))
		return
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, http.StatusOK, map[string]any{
		"goroutines": runtime.NumGoroutine(),
		// 三个数放一起才能分清"漏"和"分配器保留"：
		// Sys 涨、HeapAlloc 平、HeapIdle 跟着涨 = 保留；三个都涨才是真漏。
		"heap_bytes":    m.HeapAlloc,
		"heap_idle":     m.HeapIdle,
		"heap_released": m.HeapReleased,
		"sys_bytes":     m.Sys,
	})
}

// handleWS 是界面订阅事件流的地方。
//
// 和 /api 一样要 cookie：**没有 cookie 就不许升级**。
// 升级之后这条连接交给 ServeConn，它负责把 Hub 的帧写回来。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, errorBody("unauthorized"))
		return
	}
	conn, err := Upgrade(w, r)
	if err != nil {
		return // Upgrade 已经写过 400
	}
	go ServeConn(conn, s.hub)
}

// handleConnect 拿 launch token 换 cookie。同一个 token 只换一次。
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}

	got := r.URL.Query().Get("token")
	s.mu.Lock()
	// 一次性：换过之后再用必须是 401，且不再生成新 cookie。
	if s.used || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, errorBody("unauthorized"))
		return
	}
	value, err := newSecret()
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, errorBody("cannot mint cookie"))
		return
	}
	s.used = true
	s.cookies[value] = struct{}{}
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusSeeOther)
}

// handleAPI 走 cookie 认证；没有效 cookie 一律 401。
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/")
	rec := &statusRecorder{ResponseWriter: w}
	w = rec
	defer func() {
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= http.StatusBadRequest {
			fmt.Fprintf(os.Stderr, "transport: /api/%s status=%d\n", name, status)
		}
	}()

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, errorBody("unauthorized"))
		return
	}

	// 成功调用默认不刷 stderr；只有显式打开 trace 才逐条打印。
	// 失败调用由上面的 defer 统一打 status，避免把正常轮询误读成报错。
	if apiTraceEnabled() {
		fmt.Fprintf(os.Stderr, "transport: /api/%s\n", name)
	}
	// 官方 0.2.2 桌面端方法（L1 语义/字段对齐）。顺序是硬要求：
	// settings.official 必须排在 `settings.` 前缀分支之前，否则被既有 SettingsFormHandler 接走；
	// pluginManager.* 虽然以 plugin 开头但下一字符是 'M'，`plugin.` 前缀不会误吞。
	if IsOfficialSettingsCredentialMethod(name) {
		s.serveOfficialSettingsCredentialAPI(w, r, name)
		return
	}
	if IsOfficialPluginManagerMethod(name) {
		s.ServeOfficialPluginManagerAPI(w, r)
		return
	}
	if IsOfficialAgentMethod(name) {
		s.serveOfficialAgentAPI(w, r, name)
		return
	}
	if IsOfficialWorkspaceMethod(name) {
		s.serveOfficialWorkspaceAPI(w, r, name)
		return
	}
	if IsOfficialScheduleMethod(name) {
		s.serveOfficialScheduleAPI(w, r, name)
		return
	}
	// 官方 forge / liveshare 27 个方法：本仓无外部服务，全部返回
	// 结构化未实现（点名方法 + 缺什么），不返回假数据。
	// 官方 account / llm.discoverModels / 媒体市场（cotTranslation 等）：
	// 能本地实现的真实现，其余返回结构化未实现（点名缺什么）。
	if IsOfficialAccountMethod(name) {
		s.serveOfficialAccountAPI(w, r, name)
		return
	}
	if IsOfficialMediaMarketMethod(name) {
		s.ServeOfficialMediaMarketAPI(w, r)
		return
	}
	if IsOfficialForgeLiveshareMethod(name) {
		s.serveOfficialForgeLiveshareAPI(w, r, name)
		return
	}
	if s.opt.OfficialJobTerminal != nil && s.opt.OfficialJobTerminal.Handles(name) {
		s.opt.OfficialJobTerminal.Serve(w, r)
		return
	}
	if strings.HasPrefix(name, "plugin.") && s.opt.PluginHandler != nil {
		s.opt.PluginHandler.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(name, "settings.") && s.opt.SettingsFormHandler != nil {
		s.opt.SettingsFormHandler.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(name, "models.catalog.") && s.opt.ModelCatalogHandler != nil {
		s.opt.ModelCatalogHandler.ServeHTTP(w, r)
		return
	}
	switch Method(name) {
	// 官方 0.2.2 `session.*` 方法（常量与 handler 都在 handlers_session_official.go）。
	// 注意常量名是 MethodSessionWorkspacePathApps、handler 名是
	// handleSessionWorkspacePathApplications——名字故意不同，别按后缀猜。
	case MethodSessionPage:
		s.handleSessionPage(w, r)
	case MethodSessionState:
		s.handleSessionState(w, r)
	case MethodSessionProjections:
		s.handleSessionProjections(w, r)
	case MethodSessionObserveState:
		s.handleSessionObserveState(w, r)
	case MethodSessionObserveProjections:
		s.handleSessionObserveProjections(w, r)
	case MethodSessionObserveEvent:
		s.handleSessionObserveEvent(w, r)
	case MethodSessionFollow:
		s.handleSessionFollow(w, r)
	case MethodSessionEvents:
		s.handleSessionEvents(w, r)
	case MethodSessionUpdateQueue:
		s.handleSessionUpdateQueue(w, r)
	case MethodSessionWait:
		s.handleSessionWait(w, r)
	case MethodSessionSearch:
		s.handleSessionSearch(w, r)
	case MethodSessionAttachment:
		s.handleSessionAttachment(w, r)
	case MethodSessionSelectModel:
		s.handleSessionSelectModel(w, r)
	case MethodSessionOpenWorkspacePath:
		s.handleSessionOpenWorkspacePath(w, r)
	case MethodSessionWorkspacePathApps:
		s.handleSessionWorkspacePathApplications(w, r)
	case MethodAttachmentPut:
		s.handleAttachmentPut(w, r)
	case MethodAttachmentGet:
		s.handleAttachmentGet(w, r)
	case MethodContextUsage:
		s.handleContextUsage(w, r)
	case MethodMCPList:
		s.handleMCPList(w, r)
	case MethodMCPReconnect:
		s.handleMCPReconnect(w, r)
	case MethodMCPToggle:
		s.handleMCPToggle(w, r)
	case MethodSideQuery:
		s.handleSideQuery(w, r)
	case MethodSideCancel:
		s.handleSideCancel(w, r)
	case MethodSideClose:
		s.handleSideClose(w, r)
	case MethodSessionList:
		s.handleSessionList(w)
	case MethodSessionLoad:
		s.handleSessionLoad(w, r)
	case MethodSessionHistory:
		s.handleSessionHistory(w, r)
	case MethodSessionPrompt:
		s.handleSessionPrompt(w, r)
	case MethodSessionApprove:
		s.handleSessionApprove(w, r)
	case MethodSessionCancel:
		s.handleSessionCancel(w, r)
	case MethodSessionCreate:
		s.handleSessionCreate(w, r)
	case MethodSessionDelete:
		s.handleSessionDelete(w, r)
	case MethodSessionFork:
		s.handleSessionFork(w, r)
	case MethodSessionRewindPreview:
		s.handleSessionRewindPreview(w, r)
	case MethodSessionRewindApply:
		s.handleSessionRewindApply(w, r)
	case MethodSessionTranscriptPage:
		s.handleSessionTranscriptPage(w, r)
	case MethodSessionRename:
		s.handleSessionRename(w, r)
	case MethodSessionColorGet:
		s.handleSessionColorGet(w, r)
	case MethodSessionColorSet:
		s.handleSessionColorSet(w, r)
	case MethodSessionCompact:
		s.handleSessionCompact(w, r)
	case MethodWorkspaceList:
		s.handleWorkspaceList(w, r)
	case MethodWorkspaceInit:
		s.handleWorkspaceInit(w, r)
	case MethodJobsList:
		s.handleJobsList(w, r)
	case MethodJobsOutput:
		s.handleJobsOutput(w, r)
	case MethodJobsStop:
		s.handleJobsStop(w, r)
	case MethodScheduleList:
		s.handleScheduleList(w, r)
	case MethodScheduleDelete:
		s.handleScheduleDelete(w, r)
	case MethodScheduleClear:
		s.handleScheduleClear(w, r)
	case MethodSessionSetModel:
		s.handleSessionSetModel(w, r)
	case MethodSessionSetPermission:
		s.handleSessionSetPermission(w, r)
	case MethodModelsList:
		s.handleModelsList(w, r)
	case MethodSessionSetGoal:
		s.handleSessionSetGoal(w, r)
	case MethodSessionSetEffort:
		s.handleSessionSetEffort(w, r)
	case MethodSessionGoalGet:
		s.handleSessionGoalGet(w, r)
	case MethodSessionGoalPause:
		s.handleSessionGoalTransition(w, r, false)
	case MethodSessionGoalResume:
		s.handleSessionGoalTransition(w, r, true)
	case MethodTerminalOpen:
		s.handleTerminalOpen(w, r)
	case MethodTerminalInput:
		s.handleTerminalInput(w, r)
	case MethodTerminalOutput:
		s.handleTerminalOutput(w, r)
	case MethodFSRead:
		s.handleFSRead(w, r)
	case MethodFSWrite:
		s.handleFSWrite(w, r)
	case MethodConfigGet:
		s.handleConfigGet(w, r)
	case MethodConfigSet:
		s.handleConfigSet(w, r)
	default:
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
	}
}

// attachmentErrorStatus 让宿主适配器给出精确错误码，同时保持四字段信封唯一构造点。
type attachmentErrorStatus interface {
	AttachmentError() (code string, status int)
}

func writeAttachmentError(w http.ResponseWriter, err error, fallbackCode string, fallbackStatus int) {
	code, status := fallbackCode, fallbackStatus
	if typed, ok := err.(attachmentErrorStatus); ok {
		code, status = typed.AttachmentError()
	}
	writeJSON(w, status, errorBodyFor(code, err.Error()))
}

func (s *Server) handleAttachmentPut(w http.ResponseWriter, r *http.Request) {
	if s.opt.Attachments == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
		return
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Path == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "attachment.put 需要非空 path"))
		return
	}
	attachment, err := s.opt.Attachments.Put(in.Path)
	if err != nil {
		writeAttachmentError(w, err, "bad_request", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachment": attachment})
}

func (s *Server) handleAttachmentGet(w http.ResponseWriter, r *http.Request) {
	if s.opt.Attachments == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
		return
	}
	var in struct {
		ID           string `json:"id"`
		AttachmentID string `json:"attachmentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "attachment.get 需要合法 JSON"))
		return
	}
	id := fillID(in.ID, in.AttachmentID)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "attachment.get 需要 id 或 attachmentId"))
		return
	}
	content, err := s.opt.Attachments.Get(id)
	if err != nil {
		writeAttachmentError(w, err, "not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"attachment": content.Attachment,
		"dataBase64": base64.StdEncoding.EncodeToString(content.Data),
	})
}

// handleSessionPrompt 是 /api/session.prompt。placement 是 additive 请求字段；
// canonical 成功结果仍只有 accepted/turnId，模型回答与 runner 错误走 WS。
func (s *Server) handleSessionPrompt(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string `json:"sessionId"`
		// ID 是契约快照里的名字（H9）。两个都收：只认一个就是静默 404。
		ID        string         `json:"id"`
		Text      string         `json:"text"`
		Content   []ContentBlock `json:"content"`
		Placement string         `json:"placement"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "session.prompt 需要合法 JSON"))
		return
	}
	placement, err := NormalizeSubmitPlacement(in.Placement)
	if err != nil {
		writePromptError(w, err)
		return
	}
	sid := fillID(in.SessionID, in.ID)
	if in.Content != nil {
		var res PromptResult
		switch {
		case in.Placement != "" && s.opt.PromptBlocksPlacement != nil:
			res, err = s.opt.PromptBlocksPlacement(sid, in.Content, placement)
		case placement == string(PlacementTurn) && s.opt.PromptBlocks != nil:
			res, err = s.opt.PromptBlocks(sid, in.Content)
		case s.opt.PromptBlocksPlacement != nil:
			res, err = s.opt.PromptBlocksPlacement(sid, in.Content, placement)
		default:
			writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
			return
		}
		if err != nil {
			if _, ok := err.(attachmentErrorStatus); ok {
				writeAttachmentError(w, err, "bad_request", http.StatusBadRequest)
			} else {
				writePromptError(w, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	var res PromptResult
	switch {
	case in.Placement != "" && s.opt.PromptPlacement != nil:
		res, err = s.opt.PromptPlacement(sid, in.Text, placement)
	case placement == string(PlacementTurn) && s.opt.Prompt != nil:
		res, err = s.opt.Prompt(sid, in.Text)
	case s.opt.PromptPlacement != nil:
		res, err = s.opt.PromptPlacement(sid, in.Text, placement)
	default:
		// 非 turn 不能在旧 Prompt 上静默降级：那会把 steer/now 伪造成普通新轮。
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
		return
	}
	if err != nil {
		writePromptError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writePromptError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPlacementInvalid):
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
	case errors.Is(err, ErrPlacementUnsupported):
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented", err.Error()))
	default:
		writeJSON(w, http.StatusConflict, errorBody(err.Error()))
	}
}

// handleSessionCancel 是 /api/session.cancel。cause/turnId 是 additive；
// 旧宿主只在 user 且未指定 turnID 时走旧 Cancel，绝不把 interrupt 假装成清过队列。
func (s *Server) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string `json:"sessionId"`
		// ID 是契约快照里的名字（H9）。两个都收：只认一个就是静默 404。
		ID     string `json:"id"`
		Cause  string `json:"cause"`
		TurnID string `json:"turnId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	sid := fillID(in.SessionID, in.ID)
	if s.opt.CancelPlacement != nil {
		res, err := s.opt.CancelPlacement(sid, in.Cause, in.TurnID)
		if err != nil {
			writePromptError(w, err)
			return
		}
		if res.StillQueued == nil {
			res.StillQueued = []string{}
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	if (in.Cause != "" && in.Cause != "user") || in.TurnID != "" {
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
		return
	}
	if s.opt.Cancel == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
		return
	}
	if err := s.opt.Cancel(sid); err != nil {
		writeJSON(w, http.StatusConflict, errorBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// handleSessionCreate 是 /api/session.create：建会话，返回 id。
func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if s.opt.Create == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
		return
	}
	id, err := s.opt.Create(in.Mode)
	if err != nil {
		// 400 是既有约定：建不出来通常是 mode 认不出来（客户端改请求才能过）。
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": id})
}

// fillID 把契约里的 id 别名补进 SessionID。
//
// 为什么必须有：界面按快照发 `{"id":...}`，宿主只读 `sessionId` → 一律 404，
// 而那个 404 长得像"会话不存在"（2026-11-03 H10 就是这么来的）。
// **同一个字段两个名字，等于两套真相；收下两个名字至少不会静默失败。**
// firstNonZero 取第一个非零值：别名冲突时以第一个为准，不允许"两个都不认"。
func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func fillID(sessionID, id string) string {
	if sessionID != "" {
		return sessionID
	}
	return id
}

// handleSessionLoad 是 /api/session.load：首屏。
func (s *Server) handleSessionLoad(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string `json:"sessionId"`
		// ID 是契约快照里的名字（H9）。两个都收：只认一个就是静默 404。
		ID    string `json:"id"`
		Limit int    `json:"limit"`
		// TailMessages 是契约快照里的名字（H9）。两个都收；冲突时以 limit 为准。
		TailMessages int `json:"tailMessages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if s.opt.Load == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
		return
	}
	res, err := s.opt.Load(fillID(in.SessionID, in.ID), firstNonZero(in.Limit, in.TailMessages))
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody(err.Error()))
		return
	}
	// H5：权限状态是进程级事实，在这里补上（宿主注入的 Load 只关心会话内容）。
	//
	// **只在 source 没给的时候才用进程级 --policy 顶上**。
	// 原来这里无条件 `res.Permission = s.opt.Policy`，于是：
	//   - source.go 里 PermissionSourcer 填的会话级覆盖被当场盖掉
	//   - 界面设了 `ask`、`session.load` 回来还是 `allow`
	//   - 而且**没有任何报错**：两个地方都"尽职地"写了同一个字段，
	//     后写的那个赢——这种冲突不会以 bug 出现，只会以"设了没用"出现
	if res.Permission == "" {
		res.Permission = s.opt.Policy
	}
	// H12：只有**正数**才算"知道宽度"。0 和负数都当未知、字段直接不出现。
	//
	// 负数放过去不是报错，而是让界面画出一个宽度为负的容器——看起来"怪但没崩"，
	// 这种 bug 没人会去查宿主。宁可不给，让界面自己算。
	if s.opt.ContentWidth > 0 {
		res.ContentWidth = s.opt.ContentWidth
	}
	// H14：load 是首屏投影，不能拿来补洞；但它必须告诉客户端
	// "从哪个世代、哪个水位接着收"——否则客户端只能猜。
	if s.opt.Journals != nil {
		j := s.opt.Journals.Get(fillID(in.SessionID, in.ID))
		res.JournalEpoch = j.Epoch()
		res.LastSeq = j.LastSeq()
	}
	writeJSON(w, http.StatusOK, res)
}

// handleSessionApprove 是 /api/session.approve：客户端对一次审批给出决定（H13）。
func (s *Server) handleSessionApprove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RequestID string `json:"requestId"`
		OptionID  string `json:"optionId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.RequestID == "" || in.OptionID == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("缺参数：要 requestId 与 optionId"))
		return
	}
	if s.opt.Approve == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("session.approve 未接线"))
		return
	}
	switch err := s.opt.Approve(in.RequestID, in.OptionID); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "optionId": in.OptionID})
	case errors.Is(err, ErrNoSuchRequest):
		writeJSON(w, http.StatusNotFound, errorBody("no_such_request"))
	case errors.Is(err, ErrBadOption):
		writeJSON(w, http.StatusBadRequest, errorBody("bad_option"))
	default:
		// 审批通道自己炸了（broker 没了、写回失败）：按 internal 报，可重试。
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
	}
}

// handleSessionHistory 是 /api/session.history：断流后按 seq 补洞。
//
// H14：补洞必须返回**同一 journal 的原始帧**，不是抽过 text 的 messages。
// 用投影补洞会丢掉 reasoning/tool/approval——那些恰恰是断线后最需要恢复的。
func (s *Server) handleSessionHistory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string `json:"sessionId"`
		// ID 是契约快照里的名字（H9）。两个都收：只认一个就是静默 404。
		ID           string `json:"id"`
		SinceSeq     int    `json:"sinceSeq"`
		Limit        int    `json:"limit"`
		JournalEpoch int64  `json:"journalEpoch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	// 有 journal 时以它为准：WS 推的和这里补的是同一个序号域。
	if s.opt.Journals != nil {
		sid := fillID(in.SessionID, in.ID)
		if sid == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("缺参数：要 id 或 sessionId"))
			return
		}
		j := s.opt.Journals.Get(sid)
		// 客户端带着一个不认识的老世代、水位比当前还大、或水位之前的历史
		// 已经滑出内存 journal：三种都说明"按 seq 补"补不出完整数据，
		// 必须明确要求它重建基线。
		//
		// 第三种最隐蔽：容量丢最老帧之后，补洞会从最老的保留帧开始回，
		// 帧是连续的、seq 也在涨，客户端**看不出中间缺了一段**——它只会以为
		// "那段时间就这么点事"。这是典型的"有产出、但产出里没有完整载荷、没人报错"。
		//
		// 用 Window 一次拿全（帧/水位/世代/有没有缺口）：分三次读的话，
		// 恰好在两次之间挤掉一帧，"检查通过、取帧却少一帧"。
		win := j.Window(in.SinceSeq, in.Limit)
		staleEpoch := in.JournalEpoch != 0 && in.JournalEpoch != win.Epoch
		tooNew := in.SinceSeq > win.LastSeq
		gap := !staleEpoch && !tooNew && !win.Covered
		if staleEpoch || tooNew || gap {
			msg := "journal 世代或水位对不上，需要重新 load 基线"
			if gap {
				msg = fmt.Sprintf("sinceSeq=%d 之前的历史已经滑出内存 journal（只保留最近 %d 帧）："+
					"继续按 seq 补会缺一段，而且缺口看不出来", in.SinceSeq, win.LastSeq-win.Oldest+1)
			}
			// 码、hint、retryable **全部**走统一构造。这一段以前是手写 JSON：
			// 四字段靠手抄，漏了 `retryable`（界面审计实测通用客户端解不出它），
			// 而这条恰恰是"丢掉旧水位就能继续"的那一类——必须可重试。
			// journalEpoch/lastSeq 平铺在顶层：客户端靠它们重建基线，不是信封的一部分。
			writeJSON(w, http.StatusConflict, errorBodyFor("stale_epoch", msg, map[string]any{
				"journalEpoch": win.Epoch,
				"lastSeq":      win.LastSeq,
			}))
			return
		}
		writeJSON(w, http.StatusOK, HistoryResult{
			Frames:       win.Frames,
			LastSeq:      win.LastSeq,
			JournalEpoch: win.Epoch,
		})
		return
	}

	if s.opt.History == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("not implemented"))
		return
	}
	pg, err := s.opt.History(fillID(in.SessionID, in.ID), in.SinceSeq, in.Limit)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, pg)
}

// handleSessionList 是 /api/session.list：宿主注入的 List 决定内容。
func (s *Server) handleSessionList(w http.ResponseWriter) {
	sessions := make([]SessionSummary, 0)
	if s.opt.List != nil {
		got, err := s.opt.List()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", err.Error()))
			return
		}
		if got != nil {
			sessions = got
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Sessions []SessionSummary `json:"sessions"`
	}{Sessions: sessions})
}

// authorized 校验会话 cookie。
//
// 逐个比对全部已发放值（subtle.ConstantTimeCompare），不提前 break，
// 免得"第几个 cookie 命中"从耗时里漏出去。
func (s *Server) authorized(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	got := []byte(c.Value)

	s.mu.Lock()
	defer s.mu.Unlock()

	ok := 0
	for want := range s.cookies {
		ok |= subtle.ConstantTimeCompare(got, []byte(want))
	}
	return ok == 1
}

// MintInternalCookie 给**进程内**的消费者发一份有效 cookie。
//
// 为什么需要它：launch token 是一次性的（`s.used`），桌面壳启动时已经用它换过
// 一张 cookie；进程内另一个消费者（官方 0.2.2 wire 的 LegacyClient）再拿同一张
// token 去换只会得到 401。这里直接发一张新的 cookie，不消耗 token、不影响外部分发。
func (s *Server) MintInternalCookie() string {
	value, err := newSecret()
	if err != nil {
		return ""
	}
	s.mu.Lock()
	s.cookies[value] = struct{}{}
	s.mu.Unlock()
	return value
}

// newSecret 生成 secretBytes 字节的随机数，返回 hex 串。
func newSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// baseURL 把监听地址变成壳能直接用的 URL。
// 监听在通配地址时换成回环地址给客户端用。
func baseURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String()
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// alreadyEnveloped 判断这个值是不是已经带了信封（防止二次包装）。
func alreadyEnveloped(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if _, has := m["error"]; has {
		return true
	}
	_, has := m["result"]
	return has
}

// errorBody 造一个契约里的错误信封：
//
//	{"error":{"code":…,"message":…,"hint":…,"retryable":…}}
//
// 这是**唯一**的错误体构造。别处不许手写这个 map：手写的那几处迟早会少
// 一个字段（H8 的 `stale_epoch` 就是这么漏掉 `retryable` 的——界面审计实测，
// 通用客户端解不出这个字段）。`TestNoHandwrittenErrorBodies` 扫源码钉住这点。
func errorBody(msg string) map[string]any {
	return errorBodyFor(errorCode(msg), msg)
}

// errorBodyFor 用**调用方已经知道的码**造信封。
//
// 什么时候用它：码不该由中文消息猜出来的地方——`stale_epoch`、WS 握手的
// `bad_handshake`、宿主侧失败的 `internal`、参数错的 `bad_request`。
// 猜错码的后果是客户端按错的分支走（把"宿主崩了"当成"参数写错"，
// 于是它去改参数、并且不重试）。
//
// hint 与 retryable 仍然只在 errorObject 那一处判定：加一个新码，
// 就必须同时在 errorHint 与 retryableFor 里给出说法——
// `TestRetryableDecisionCoversEveryCode` 会盯着。
//
// extra 平铺到信封**顶层**（`stale_epoch` 要带 journalEpoch/lastSeq，
// 好让客户端知道该从哪个世代/水位重建基线）。"error" 与 "result" 两个键
// 不许被 extra 覆盖——那等于让附加信息伪装成信封本身。
func errorBodyFor(code, msg string, extra ...map[string]any) map[string]any {
	body := make(map[string]any, len(extra)+1)
	body["error"] = errorObject(code, msg)
	for _, kv := range extra {
		for k, v := range kv {
			if k == "error" || k == "result" {
				continue
			}
			body[k] = v
		}
	}
	return body
}

// errorObject 就是那四个字段本身，也是唯一的构造点。
// hint 是必需的：客户端要知道**下一步怎么办**，而这正是 DSH 错误契约的意义。
func errorObject(code, msg string) map[string]any {
	return map[string]any{
		"code":      code,
		"message":   msg,
		"hint":      errorHint(code),
		"retryable": retryableFor(code),
	}
}

// retryableFor 是"这条错误能不能重试"的**唯一**判据。
//
// 只按码判，不按 HTTP 状态、不按消息文本——客户端也只能按码判。
//
//   - internal：宿主自己出错（读盘/序列化/接线炸了），同一个请求再发一次可能就过
//   - busy：上一轮还没结束，等 turn/done（或先 session.cancel）再发就行
//   - stale_epoch：水位/世代过期，丢掉旧水位重新 load 就能接着收
//
// 其余一律 false：参数错、鉴权失败、未接线、找不到东西——重发同一个请求
// 还是同一个结果，让客户端自动重试只是把错误刷屏。
func retryableFor(code string) bool {
	switch code {
	case "internal", "busy", "stale_epoch":
		return true
	}
	return false
}

// errorHint 给每类错误一句"下一步"。叫不出名字的错误也必须有一句（见兜底）：
// 空 hint 等于让客户端把"宿主未知错误"翻译成"什么都不做"。
func errorHint(code string) string {
	switch code {
	case "unauthorized":
		return "先 GET /connect?token=… 换 cookie，再调这个接口"
	case "not_implemented":
		return "宿主这一侧还没接线，换一个 method 或等它实现"
	case "not_found":
		return "检查请求路径与 HTTP 方法；要找的会话/文件/id 可能已经不在了（先 session.list 或 workspace.list 确认）"
	case "no_such_session":
		return "先 session.list 拿 id；确认 --home 指向的目录里有这个会话"
	case "no_such_request":
		return "这个审批请求已经不在了（已应答或已超时）；等下一次 approval/request"
	case "busy":
		return "上一轮还没结束：等 turn/done（或先 session.cancel），再发下一条"
	case "bad_option":
		return "optionId 必须是 allow_once / allow_always / reject_once 之一"
	case "bad_request":
		return "检查入参名字（id/sessionId 都认）与类型"
	case "revision_conflict":
		return "重新 schedule.list 读取最新 revision，再基于最新内容重试；不要覆盖别人的修改"
	case "job_finished":
		return "任务已经结束，只能 jobs.output 读取保留输出，不能重复停止"
	case "cross_session":
		return "只能操作当前 home、当前会话且由本宿主启动的任务；先核对 sessionId 与 jobs.list"
	case "bad_handshake":
		return "WS 握手要带 Upgrade: websocket、Connection: Upgrade、Sec-WebSocket-Key 三个头；先 GET /connect 换 cookie 再连 /ws"
	case "stale_epoch":
		return "丢弃旧水位，重新 session.load 拿 journalEpoch/lastSeq，再从 lastSeq 往后订阅（补洞只覆盖内存里还在的帧）"
	case "encode_failure":
		return "这是宿主内部序列化出错，不是你参数写错了；把这一步操作和 dshd 的 stderr 报告出来"
	case "empty_result":
		return "宿主这一侧没给出任何载荷（不是你的参数写错）；把这一步操作和 dshd 的 stderr 报告出来"
	case "internal":
		return "可以直接重试；反复失败就看 dshd 的 stderr"
	}
	return "这是宿主侧的错误（不是你参数写错了）：把这一步操作和 dshd 的 stderr 一起报出来"
}

// errorCode 把旧的中文错误串映射成稳定的机器码。
//
// 映射表就是契约：加新错误时**先加码再加消息**，否则客户端只能靠猜中文。
func errorCode(msg string) string {
	switch msg {
	case "unauthorized":
		return "unauthorized"
	case "not implemented":
		return "not_implemented"
	case "没有这个会话":
		return "no_such_session"
	case "上一轮还没结束":
		// 这条以前落到兜底 code="error" + hint 空 + retryable=false——
		// 三样都错：code 没意义、hint 违反契约（每个错误都要有下一步）、
		// 而这一轮结束后**确实可以重试**。界面拿不到这些信息只能猜。
		return "busy"
	}
	// 这两个是审批（H13）的稳定错误码，调用方按码判断，不按中文猜。
	if strings.Contains(msg, "序列化失败") {
		return "encode_failure"
	}
	if strings.Contains(msg, "响应是空值") {
		return "empty_result"
	}
	if msg == "no_such_request" || msg == "bad_option" {
		return msg
	}
	if strings.Contains(msg, "未接线") {
		return "not_implemented"
	}
	if strings.Contains(msg, "参数") {
		return "bad_request"
	}
	return "error"
}

// writeJSON 统一套信封：成功 {"result":…}、失败 {"error":{…}}。
//
// 只在 /api 的 handler 里用，所以可以在这里一把收口——
// 逐个 handler 改的话，总有一个会漏，而漏掉的那个就是"界面看到 0 条"。
func writeJSON(w http.ResponseWriter, status int, v any) {
	// 成功响应不许是空值。
	//
	// `{"result":null}` 的形状是"HTTP 200 + 没有任何载荷"：客户端拿到的是 null，
	// 照契约读 result 的代码大多会安静地把它当空处理——界面表现是"什么都没有"，
	// 而所有重试/告警逻辑都认为这是成功。这与本仓一天撞到的两条静默成功同形，
	// 所以宁可在出口把它变成 500（带上可执行的 hint），也不放过一个 null。
	if v != nil && !alreadyEnveloped(v) {
		v = map[string]any{"result": v}
	}
	var body []byte
	var err error
	switch {
	case v == nil:
		err = fmt.Errorf("响应是空值：handler 没有给出 result")
	default:
		body, err = json.Marshal(v)
		if err == nil && string(body) == `{"result":null}` {
			// 类型化的 nil（(*T)(nil)）会走到这里：它不是 nil 接口，但序列化后同样没有载荷。
			err = fmt.Errorf("响应是空值：handler 给了一个 nil 的 result")
		} else if err != nil {
			err = fmt.Errorf("响应序列化失败: %w", err)
		}
	}
	if err != nil {
		// 这里的 payload 全是自家结构，真出错也只剩一个兜底正文。
		//
		// 但它仍然是**一个错误出口**，必须和别的出口同形状（四字段）。
		// 客户端只有一个解析器：少一个字段就要特判，而特判分支没人会为它写测试。
		fallback, _ := json.Marshal(errorBody(err.Error()))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(append(fallback, '\n'))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// apiTraceEnabled 控制是否逐条打印成功的 /api 调用。
// 默认关闭；失败调用始终由 handleAPI 的 defer 打印 status。
func apiTraceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSHD_TRACE_API"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// statusRecorder 记住实际写出去的 HTTP 状态码。
//
// 为什么需要：`/api` 的日志原来只有方法名，看不出这一调用是成功还是 404/501。
// 界面联调时"它调了但报错"和"它调成功了"在日志上必须能分开。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

// Flush 保留流式接口能力；sideQuery 等 handler 会 type assert http.Flusher。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能取回底层 writer。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
