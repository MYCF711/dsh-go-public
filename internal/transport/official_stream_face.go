package transport

// 官方流式方法的**两条传输面**与**唯一启动入口**。
//
// # 为什么两条面共用一台帧机
//
// 官方一个 `mode:'stream'` 方法同时出现在两处：
//
//	HTTP  `POST /api/<method>`（本仓既有 unary 面，长连接用 NDJSON）
//	mux   `/api/remote.mux` 的 `open` 帧（官方流载体）
//
// 两条面各写一套就会分叉：出现"HTTP 上 cursor 对了、mux 上重放了一遍"这种
// 只在某一侧复现的 bug。所以帧机只有一份（official_stream.go /
// official_stream_sources.go），这里只做**帧 → 传输**的搬运和**故障 → 各自
// 错误形状**的翻译。
//
// # HTTP 面的选择（明确写下来，别猜）
//
// 官方是流；本仓 HTTP 是"一次请求一次响应"。两条都不能丢，所以：
//
//	POST /api/session.follow 等 6 个流方法 → 200 + application/x-ndjson
//	每一行 = 官方的一帧（{"type":"snapshot"…} / {"type":"event"…} /
//	{"type":"ready"} / {"type":"projection"…} / {"kind":"ready"} /
//	{"kind":"change"…} / {"available":…}），一行一帧，**没有额外包壳**；
//	流正常结束 = 连接关闭；中途失败 = 最后一行 {"type":"error","error":{…}}
//	（四字段，与本仓 side.query 的长连接约定一致）。
//
// 为什么不是 SSE：本仓已有 NDJSON 长连接先例（context_mcp_sidequery.go 的
// `side.query` stream）。同一进程里两套长连接形状只会让客户端多一份解析器。
// 为什么**不**只回 501 指向 /ws：那让官方客户端无法订阅，等于没实现。
//
// 参数校验失败（未知字段、缺 address、sinceSeq < -1…）发生在**写响应头之前**，
// 所以仍然是普通四字段 JSON 错误 + 状态码——不是"200 之后才报错"。
//
// # mux 面为什么不在本文件
//
// `internal/officialwire` 的包文档写明"只做协议翻译，真实能力由宿主通过
// Binding / StreamOpener 注入"，它的**测试**又 import 了本包（legacy_coexistence_test.go）。
// 本包反过来 import officialwire 会形成测试期导入环，也会把业务判断塞进协议层。
// 所以这里只暴露**不含 officialwire 类型**的启动入口：
//
//	func (s *Server) OpenOfficialFrameStream(ctx, method, args) (OfficialFrameStream, *OfficialStreamFault)
//
// 接线方（cmd/dshd/serve_official_wire.go）用 ~20 行把它包成
// officialwire.StreamOpener。完整代码见 HOST-STREAM-METHODS-RESULT.md 的
// 「接线清单」，测试 side 有一份逐字节相同的副本（official_stream_mux_test.go），
// 用来钉住 mux 七帧上的真实行为。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// officialStreamBodyLimit 是流方法请求体的上限（参数对象，不是数据）。
const officialStreamBodyLimit = 1 << 20

// OfficialFrameStream 是一条官方流方法的帧机（拉模式）。
//
// 三个返回值：一帧、是否正常结束、启动后的致命故障。
// **必须尊重 ctx**：对端断了不会通知数据源，只有 ctx 会。
type OfficialFrameStream interface {
	Next(ctx context.Context) (frame json.RawMessage, done bool, fault *OfficialStreamFault)
	Close()
}

// OfficialStreamFault 是流故障：既够 HTTP 面翻四字段信封，也够 mux 面翻
// `gateway/*` 错误帧。Code 用官方 gateway 词表（两条面共用同一份判断）。
type OfficialStreamFault struct {
	Code    string
	Message string
	Field   string
	Status  int
}

func (f *OfficialStreamFault) Error() string {
	if f == nil {
		return "<nil>"
	}
	return f.Code + ": " + f.Message
}

// normalizeOfficialEndpoint 把官方 mux 的 `<namespace>/<method>` 端点翻成本仓
// 的 `<namespace>.<method>` 方法名。
//
// 两个仓的写法不同不是笔误：**官方 wire 用斜杠、本仓 L1 用点号**（既有 58 个
// 方法一个都不改名）。mux 的 open 帧带的是官方斜杠形式，所以这里必须归一，
// 否则每条流都会走到"不是流式方法"分支——一个只在这一层出现的假 501。
func normalizeOfficialEndpoint(endpoint string) Method {
	if !strings.Contains(endpoint, "/") {
		return Method(endpoint)
	}
	// 只换**第一个**斜杠：方法名本身不含斜杠，但 `$events` 之类的内部端点
	// 可能有前缀，保留原样比猜更安全。
	namespace, rest, _ := strings.Cut(endpoint, "/")
	return Method(namespace + "." + rest)
}

// IsOfficialStreamMethod 报告一个官方方法是不是 `mode:'stream'`。
//
// 注意 `session.events` **不在**其中：官方它是 `Promise<SessionEventsPage>`
// （typert.host.js:2019 没有 `mode` 键），一次性读，本仓早就是真实现。
// 所以"官方流式方法"是 **6 个**，不是 7 个——任务书里的 7 个包含它，
// 这里按官方 schema 决策，并在 RESULT 里写明差异。
func IsOfficialStreamMethod(name string) bool {
	switch Method(name) {
	case MethodSessionFollow,
		MethodSessionObserveEvent,
		MethodSessionObserveProjections,
		MethodSessionObserveState,
		MethodWorkspaceFilesChanges,
		MethodToolResultsWatch:
		return true
	}
	return false
}

// OpenOfficialFrameStream 校验参数、接通数据源、预取 opening 帧。
//
// 为什么预取：参数/能力错误必须在**写响应头之前**暴露（HTTP 面），也必须以
// 错误帧而不是"空 end"出现（mux 面）。一手交帧、一手交流，两条面都说得清。
func (s *Server) OpenOfficialFrameStream(ctx context.Context, method string, args json.RawMessage) (OfficialFrameStream, *OfficialStreamFault) {
	stream, head, fault := s.startOfficialStream(normalizeOfficialEndpoint(method), args)
	if fault != nil {
		return nil, fault
	}
	return &officialHeadStream{stream: stream, head: head, ctx: ctx}, nil
}

// officialHeadStream 把预取的 opening 帧接回流里（只允许取一次）。
type officialHeadStream struct {
	stream OfficialFrameStream
	head   json.RawMessage
	ctx    context.Context
}

func (h *officialHeadStream) Next(ctx context.Context) (json.RawMessage, bool, *OfficialStreamFault) {
	if h.head != nil {
		head := h.head
		h.head = nil
		return head, false, nil
	}
	return h.stream.Next(ctx)
}

func (h *officialHeadStream) Close() { h.stream.Close() }

// OpenOfficialJournalTail 打开 `session/events` 的既有**尾部流**。
//
// 这不是官方形状（官方 `session/events` 是 unary 页），是本仓从 H14 起就有的
// mux 扩展：按 journal seq 跟随一条会话日志。保留它是为了既有接线与测试不断，
// **不冒充**官方流方法。
//
// sinceSeq 语义与官方一致：排他下界；`-1` = 我什么都没拿（含 seq 0）。
func (s *Server) OpenOfficialJournalTail(sessionID string, sinceSeq, limit int) (OfficialFrameStream, *OfficialStreamFault) {
	if s == nil || s.opt.Journals == nil {
		return nil, officialStreamNotImplemented(MethodSessionEvents,
			"session/events 未接线：宿主没有每会话 journal")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, officialStreamBadRequest("sessionId",
			"session/events 需要 sessionId（本宿主没有「当前会话」这种全局概念）")
	}
	if sinceSeq < SinceNothing {
		return nil, officialStreamBadRequest("sinceSeq",
			"session/events 的 sinceSeq 必须 >= %d（-1 = 我什么都没拿）", SinceNothing)
	}
	if limit <= 0 || limit > officialStreamPageLimit {
		return nil, officialStreamBadRequest("limit",
			"session/events 的 limit 必须在 1..%d 之间", officialStreamPageLimit)
	}
	return &officialJournalTailStream{
		journal: s.opt.Journals.Get(sessionID),
		next:    sinceSeq,
		limit:   limit,
	}, nil
}

// officialJournalTailStream 按 journal seq 跟随一条会话日志（**只读**）。
//
// 只读是硬要求：它不 Append、不 Reset，所以官方 mux 连接不会产生第二个 seq 域，
// 也不会影响既有 /ws 的推送顺序。客户端看到的 seq 就是 L1 journal 的 seq。
type officialJournalTailStream struct {
	journal *Journal
	next    int
	limit   int
}

func (s *officialJournalTailStream) Next(ctx context.Context) (json.RawMessage, bool, *OfficialStreamFault) {
	for {
		window := s.journal.Window(s.next, s.limit)
		if len(window.Frames) > 0 {
			frame := window.Frames[0]
			s.next = frame.Seq
			payload, err := json.Marshal(frame)
			if err != nil {
				return nil, false, officialStreamInternal(MethodSessionEvents,
					"journal 帧序列化失败：%v", err)
			}
			return payload, false, nil
		}
		wait := s.journal.Wait()
		// 注册等待之后再查一次：这一瞬间刚追加的帧不会丢。
		if again := s.journal.Window(s.next, s.limit); len(again.Frames) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return nil, true, nil
		case <-wait:
		}
	}
}

func (s *officialJournalTailStream) Close() {}

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------

// startOfficialStream 校验参数、接通数据源、并**预取 opening 帧**。
func (s *Server) startOfficialStream(method Method, raw json.RawMessage) (OfficialFrameStream, json.RawMessage, *OfficialStreamFault) {
	if s == nil {
		return nil, nil, officialStreamNotImplemented(method, "transport Server 是 nil")
	}
	request, fault := parseOfficialStreamRequest(method, raw)
	if fault != nil {
		return nil, nil, fault
	}

	var stream OfficialFrameStream
	switch method {
	case MethodSessionFollow, MethodSessionObserveEvent, MethodSessionObserveProjections, MethodSessionObserveState:
		host := s.officialHost()
		if host == nil {
			return nil, nil, officialStreamNotImplemented(method,
				"宿主没有提供官方会话数据源（OfficialSessionHost）")
		}
		if request.SessionID == "" {
			// follow 的会话身份在 address 里，其余在 sessionId 里。
			if request.Follow == nil {
				return nil, nil, officialStreamBadRequest("sessionId", "%s 需要非空 sessionId", method)
			}
			// 只接受 kind=session：subagent 地址要求证明"这个 child 属于这个
			// parent"（官方是 sessionProjections["subagent"]），本宿主没有那套
			// 投影，认了就等于替调用方担保一条它证明不了的归属关系
			// （与 unary 面 checkOfficialAddress 同一个判据）。
			if request.Follow.Address.Kind != "session" {
				return nil, nil, officialStreamFaultAddress(method, request.Follow.Address)
			}
			sessionID, ok := officialAddressSessionID(request.Follow.Address)
			if !ok {
				return nil, nil, officialStreamFaultAddress(method, request.Follow.Address)
			}
			request.SessionID = sessionID
		}
		if method == MethodSessionFollow {
			// follow 还需要 header 来源：没有就**现在**报，不等 opening 时才发现。
			if _, ok := host.(OfficialSessionHeaderSource); !ok {
				return nil, nil, officialStreamNotImplemented(method,
					"宿主没有提供 follow snapshot 的 header 来源（OfficialSessionHeaderSource）")
			}
		}
		stream = &officialSessionStream{
			server:    s,
			source:    officialServerStreamSource{server: s, host: host},
			kind:      method,
			sessionID: request.SessionID,
			follow:    request.Follow,
			cursor:    SinceNothing,
		}

	case MethodWorkspaceFilesChanges:
		api, _ := s.officialWorkspaceMounts()
		if api.FilesStat == nil {
			return nil, nil, officialStreamNotImplemented(method,
				"宿主没有真实文件系统读入口（OfficialWorkspaceAPI.FilesStat 未接线）")
		}
		stream = &officialWorkspaceChangesStream{
			api:       api,
			sessionID: request.SessionID,
			path:      request.Path,
		}

	case MethodToolResultsWatch:
		api, ok := s.officialAgentAPI()
		if !ok || api.ToolResultsWatch == nil {
			return nil, nil, officialStreamNotImplemented(method,
				"宿主没有 durable 工具结果读取（OfficialAgentAPI.ToolResultsWatch 未接线）")
		}
		stream = &officialToolResultStream{
			api:       api,
			sessionID: request.SessionID,
			seq:       request.ToolSeq,
		}

	default:
		return nil, nil, officialStreamNotImplemented(method, "不是官方流式方法")
	}

	head, done, fault := stream.Next(context.Background())
	if fault != nil {
		stream.Close()
		return nil, nil, fault
	}
	if done {
		stream.Close()
		return nil, nil, officialStreamInternal(method, "流在 opening 帧之前就结束了（帧机实现错误）")
	}
	return stream, head, nil
}

// officialAddressSessionID 取 address 指向的会话 id（subagent 读 child 的日志）。
func officialAddressSessionID(addr OfficialAddress) (string, bool) {
	switch addr.Kind {
	case "session":
		return addr.SessionID, strings.TrimSpace(addr.SessionID) != ""
	case "subagent":
		return addr.ChildSessionID, strings.TrimSpace(addr.ChildSessionID) != ""
	}
	return "", false
}

// officialStreamFaultAddress 把不支持的 address 翻成流故障。
func officialStreamFaultAddress(method Method, addr OfficialAddress) *OfficialStreamFault {
	switch addr.Kind {
	case "subagent":
		return &OfficialStreamFault{Code: "gateway/method-unavailable", Status: 501,
			Message: string(method) + " 的 subagent 地址未实现：本宿主没有 subagent 目录投影，" +
				"无法证明 child 属于该 parent"}
	case "":
		return officialStreamBadRequest("address", "%s 缺 address.kind", method)
	default:
		return officialStreamBadRequest("address.kind", "%s 的 address.kind 只能是 session/subagent", method)
	}
}

// ---------------------------------------------------------------------------
// HTTP 面：NDJSON 长连接
// ---------------------------------------------------------------------------

// serveOfficialStream 是 6 个流方法的 HTTP 入口。
//
// 调用方（server.go 的 /api 分派）在**既有 unary 路由之前**判断
// `IsOfficialStreamMethod(name)`；参数/能力错误仍是普通四字段 JSON + 状态码。
func (s *Server) serveOfficialStream(w http.ResponseWriter, r *http.Request, name string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, officialStreamBodyLimit))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "读请求体失败："+err.Error()))
		return
	}
	stream, fault := s.OpenOfficialFrameStream(r.Context(), name, raw)
	if fault != nil {
		writeOfficialStreamFault(w, fault)
		return
	}
	defer stream.Close()

	// 头必须**在**第一行之前：客户端拿到 200 + ndjson 就知道"订阅成功"。
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeLine := func(frame json.RawMessage) bool {
		if len(frame) == 0 {
			return true
		}
		if _, err := w.Write(append(frame, '\n')); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	for {
		frame, done, fault := stream.Next(r.Context())
		if fault != nil {
			// 已经开始写帧，不能改状态码；用一行错误帧结束（客户端据此知道
			// "流断了，而且不是正常结束"）。
			body := errorBodyFor(faultCodeForEnvelope(fault), fault.Message)
			body["type"] = "error"
			if fault.Field != "" {
				body["field"] = fault.Field
			}
			writeLine(mustMarshalStreamLine(body))
			return
		}
		if done {
			return
		}
		if !writeLine(frame) {
			return
		}
	}
}

// writeOfficialStreamFault 是"还没开始写帧"时的错误出口：状态码 + 四字段。
func writeOfficialStreamFault(w http.ResponseWriter, fault *OfficialStreamFault) {
	body := errorBodyFor(faultCodeForEnvelope(fault), fault.Message)
	if fault.Field != "" {
		body["field"] = fault.Field
	}
	writeJSON(w, fault.Status, body)
}

// faultCodeForEnvelope 把官方 gateway 码翻成本仓四字段码。
//
// 为什么不能直接用 gateway/*：本仓 unary 信封的 code 词表是既有契约
// （bad_request/not_implemented/internal/…），混进 gateway/* 会让既有客户端
// 认不出来。mux 面**仍然**用官方 gateway 码（那是官方 wire 的形状）。
func faultCodeForEnvelope(fault *OfficialStreamFault) string {
	switch {
	case fault == nil:
		return "internal"
	case fault.Status == http.StatusBadRequest:
		return "bad_request"
	case fault.Status == http.StatusNotFound:
		return "no_such_session"
	case fault.Status == http.StatusNotImplemented:
		return "not_implemented"
	default:
		return "internal"
	}
}

// mustMarshalStreamLine 只用于自己造的 map（一定可序列化）。
func mustMarshalStreamLine(value map[string]any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"type":"error","error":{"code":"internal","message":"序列化错误帧失败","hint":"","retryable":false}}`)
	}
	return raw
}

// ---------------------------------------------------------------------------
// 四个会话流方法的 handler（server.go 的分派 case 调它们）
// ---------------------------------------------------------------------------

func (s *Server) handleSessionFollow(w http.ResponseWriter, r *http.Request) {
	s.serveOfficialStream(w, r, string(MethodSessionFollow))
}

func (s *Server) handleSessionObserveEvent(w http.ResponseWriter, r *http.Request) {
	s.serveOfficialStream(w, r, string(MethodSessionObserveEvent))
}

func (s *Server) handleSessionObserveProjections(w http.ResponseWriter, r *http.Request) {
	s.serveOfficialStream(w, r, string(MethodSessionObserveProjections))
}

func (s *Server) handleSessionObserveState(w http.ResponseWriter, r *http.Request) {
	s.serveOfficialStream(w, r, string(MethodSessionObserveState))
}
