package officialwire

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// 协议常量（EVIDENCE §3.3）。
const (
	// DefaultHeartbeat 是服务端 ping 间隔：2s。
	DefaultHeartbeat = 2 * time.Second
	// DefaultUplinkBytes 是单条逻辑流的上行 inbox 配额：256 KiB。
	DefaultUplinkBytes = 256 * 1024
)

// MuxConfig 是 /api/remote.mux 的全部外部输入。
type MuxConfig struct {
	// Auth/Peers 是认证层；两者都必需。
	Auth  *Auth
	Peers *Peers
	// Opener 把 endpoint 变成真实流；nil 时所有 open 都回 method-unavailable。
	Opener StreamOpener
	// Events 是 `$events` 的宿主事件源（emit/waterfall/cancel）。
	//
	// nil = 这个宿主没有转发事件源：`$events` 只发 ready 帧，之后挂住。
	// **不伪造 emit**——官方客户端收到 waterfall 就会去答 `$events/result`，
	// 而一条没有真实来源的事件会让界面等一个永远不存在的批准（EVIDENCE §7.3）。
	Events EventSource
	// Home 只用于 `$events` ready 帧里的 `host.home`（EVIDENCE §3.2）。
	Home string
	// Heartbeat 缺省 2s；UplinkBytes 缺省 256 KiB。
	Heartbeat   time.Duration
	UplinkBytes int
	// NewClientID 可注入；缺省每次连接生成一个 UUID v4。
	NewClientID func() string
	// WriteQueue 是下行发送队列长度，缺省 256。
	WriteQueue int
}

// Mux 是 `/api/remote.mux` 的服务端。
type Mux struct {
	cfg MuxConfig
}

// NewMux 校验配置并造 mux。
func NewMux(cfg MuxConfig) (*Mux, *Error) {
	if cfg.Auth == nil || cfg.Peers == nil {
		return nil, Errorf(CodeInternal, "officialwire: Mux 需要 Auth 与 Peers")
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.UplinkBytes <= 0 {
		cfg.UplinkBytes = DefaultUplinkBytes
	}
	if cfg.WriteQueue <= 0 {
		cfg.WriteQueue = 256
	}
	if cfg.NewClientID == nil {
		cfg.NewClientID = func() string {
			id, err := newUUID()
			if err != nil {
				return "unknown"
			}
			return id
		}
	}
	return &Mux{cfg: cfg}, nil
}

// Serve 处理一次 mux 升级。
//
// 升级前先做认证（调用方已做完）与**单 socket**判定：一个 Peer 同时只允许一条
// API socket，第二条回 409（EVIDENCE §3.3）。这两条都必须在写 101 之前，
// 否则"拒绝"就只能以关闭码表达，客户端会把它当成"连接坏了"而不是"你已经有连接了"。
func (m *Mux) Serve(w http.ResponseWriter, r *http.Request, cred *Credential) {
	peer := m.cfg.Peers.Get(cred.ID)
	if peer == nil {
		http.Error(w, "peer disposed", http.StatusUnauthorized)
		return
	}
	if !m.cfg.Peers.Attach(peer) {
		http.Error(w, "conflict: peer already holds an api socket", http.StatusConflict)
		return
	}
	conn, err := upgradeWS(w, r)
	if err != nil {
		m.cfg.Peers.Detach(peer)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &muxSession{
		mux:     m,
		conn:    conn,
		peer:    peer,
		out:     make(chan []byte, m.cfg.WriteQueue),
		done:    make(chan struct{}),
		streams: map[string]*streamState{},
		ended:   map[string]struct{}{},
		ctx:     ctx,
		cancel:  cancel,
	}
	s.run()
}

// streamState 是一条逻辑流的会话内状态。
type streamState struct {
	id      string
	stream  Stream
	bytes   int  // 上行累计字节（只对 UplinkStream 计）
	closed  bool // 客户端 end 过（半关闭上行）
	gone    bool // 已被 cancel/fail，后续帧不再发给它
	released bool // 上行被释放（`$events`）
}

// muxSession 是一条已经升级的连接。
type muxSession struct {
	mux  *Mux
	conn *wsConn
	peer *Peer

	out  chan []byte
	done chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	streams   map[string]*streamState
	ended     map[string]struct{}
	wg        sync.WaitGroup

	missed atomic.Int32

	ctx    context.Context
	cancel context.CancelFunc
}

func (s *muxSession) run() {
	go s.writeLoop()
	go s.heartbeatLoop()
	go func() {
		select {
		case <-s.peer.Disposed():
			s.shutdown(closePeerLeft, "peer left")
		case <-s.done:
		}
	}()
	defer s.teardown()

	for {
		msg, err := s.conn.readMessage()
		if err != nil {
			s.handleReadError(err)
			return
		}
		switch msg.op {
		case opText:
			s.missed.Store(0)
			if !s.dispatch(msg.payload) {
				return
			}
		case opPing:
			s.missed.Store(0)
			if err := s.conn.writeFrame(opPong, msg.payload); err != nil {
				s.terminate()
				return
			}
		case opPong:
			s.missed.Store(0)
		case opClose:
			// 客户端主动关：把关闭码回一遍再收场。
			code := 1000
			if len(msg.payload) >= 2 {
				code = int(binary.BigEndian.Uint16(msg.payload[:2]))
			}
			s.shutdown(code, "")
			return
		case opBinary:
			// mux 只收文本帧；二进制一律 1003（EVIDENCE §3.3）。
			s.shutdown(closeBinaryUnsupported, "text messages required")
			return
		default:
			s.shutdown(closeInvalidFrame, "unsupported opcode")
			return
		}
	}
}

func (s *muxSession) handleReadError(err error) {
	var pe wsProtocolError
	if errors.As(err, &pe) {
		s.shutdown(pe.code, pe.reason)
		return
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		// 对端直接断 TCP：不可能再送关闭帧，直接收场。
		s.terminate()
		return
	}
	s.shutdown(closeInternal, "read failure")
}

// dispatch 处理一条文本帧；返回 false 表示会话应当结束。
func (s *muxSession) dispatch(payload []byte) bool {
	frame, ferr := parseClientFrame(payload)
	if ferr != nil {
		// 非法帧 → 1008 invalid Remote stream request（EVIDENCE §3.3）。
		s.shutdown(closeInvalidFrame, ferr.Message)
		return false
	}
	switch frame.Type {
	case FrameOpen:
		return s.handleOpen(frame)
	case FrameItem:
		s.handleItem(frame)
		return true
	case FrameEnd:
		s.handleEnd(frame)
		return true
	case FrameCancel:
		s.handleCancel(frame)
		return true
	default:
		s.shutdown(closeInvalidFrame, "unknown frame type")
		return false
	}
}

func (s *muxSession) handleOpen(frame *clientFrame) bool {
	s.mu.Lock()
	_, live := s.streams[frame.StreamID]
	_, ended := s.ended[frame.StreamID]
	s.mu.Unlock()
	if live || ended {
		// 同一个 streamId 重复 open 是**唯一**会关 socket 的协议违规（EVIDENCE §3.1）。
		s.shutdown(closeInvalidFrame, "duplicate streamId open")
		return false
	}

	state := &streamState{id: frame.StreamID}
	switch {
	case frame.Endpoint == EndpointEvents:
		// 打开即释放上行（EVIDENCE §3.2）：后续 item 丢弃、不计数。
		//
		// clientId **每次 open 现生成**（官方 lib/index.js:1033 的
		// `let clientId = randomUUID()`）：一个 socket 上开两条 `$events` 流时，
		// 共用 id 会让 `$events/result` 分不清是哪条在答，也会把同一条事件
		// 投给两条流。
		state.stream = newEventsStream(s.mux.cfg.NewClientID(), s.mux.cfg.Home, s.mux.cfg.Events)
		state.released = true
	case s.mux.cfg.Opener == nil:
		s.sendError(frame.StreamID, MethodUnavailable(frame.Endpoint, "本宿主没有登记任何 mux 流 endpoint"))
		s.markEnded(frame.StreamID)
		return true
	default:
		stream, oerr := s.mux.cfg.Opener.Open(s.ctx, frame.Endpoint, frame.Args)
		if oerr != nil {
			s.sendError(frame.StreamID, oerr)
			s.markEnded(frame.StreamID)
			return true
		}
		if stream == nil {
			s.sendError(frame.StreamID, Errorf(CodeInternal, "opener 对 %s 返回了 nil 流", frame.Endpoint))
			s.markEnded(frame.StreamID)
			return true
		}
		state.stream = stream
	}
	s.mu.Lock()
	s.streams[frame.StreamID] = state
	s.mu.Unlock()
	s.wg.Add(1)
	go s.runStream(state)
	return true
}

func (s *muxSession) runStream(state *streamState) {
	defer s.wg.Done()
	defer state.stream.Close()
	for {
		item, done, serr := state.stream.Next(s.ctx)
		if s.isGone(state) {
			return
		}
		if serr != nil {
			s.sendError(state.id, serr)
			s.markEnded(state.id)
			return
		}
		if done {
			s.sendEnd(state.id)
			s.markEnded(state.id)
			return
		}
		if !s.sendItem(state.id, item) {
			return
		}
	}
}

func (s *muxSession) handleItem(frame *clientFrame) {
	s.mu.Lock()
	state, live := s.streams[frame.StreamID]
	_, ended := s.ended[frame.StreamID]
	s.mu.Unlock()
	if !live {
		message := "没有这条流"
		if ended {
			message = "这条流已经 end/cancel，不能再送 item"
		}
		// end 之后再 item → gateway/protocol（EVIDENCE §3.3），**不关 socket**。
		s.sendError(frame.StreamID, protocolError(frame.StreamID, message))
		return
	}
	if state.closed {
		// end 之后再 item → gateway/protocol（EVIDENCE §3.3），不关 socket。
		s.sendError(frame.StreamID, protocolError(frame.StreamID, "这条流已经 end 过，不能再送 item"))
		return
	}
	if state.released {
		return // `$events` 的上行被释放：丢弃
	}
	uplink, ok := state.stream.(UplinkStream)
	if !ok {
		s.sendError(frame.StreamID, protocolError(frame.StreamID, "这条流不接受上行数据"))
		return
	}
	delta := 0
	if frame.HasValue {
		delta = len(frame.Value)
	}
	s.mu.Lock()
	state.bytes += delta
	total := state.bytes
	s.mu.Unlock()
	if limit := s.mux.cfg.UplinkBytes; total > limit {
		// 超限失败这一条流，**不关 socket**（EVIDENCE §3.3）。
		s.failStream(state, NewError(CodeUplinkOverflow,
			fmt.Sprintf("单条流上行超过 %d 字节（已收 %d）", limit, total),
			map[string]any{"streamId": frame.StreamID, "limit": limit, "received": total}))
		return
	}
	if perr := uplink.Push(frame.Value); perr != nil {
		s.failStream(state, perr)
	}
}

func (s *muxSession) handleEnd(frame *clientFrame) {
	s.mu.Lock()
	state, live := s.streams[frame.StreamID]
	if live {
		state.closed = true
	}
	_, ended := s.ended[frame.StreamID]
	s.mu.Unlock()
	if !live && !ended {
		s.sendError(frame.StreamID, protocolError(frame.StreamID, "end 了一条不存在的流"))
	}
	// 半关闭：流本身继续（官方语义：end 只是客户端不再上行）。
}

func (s *muxSession) handleCancel(frame *clientFrame) {
	s.mu.Lock()
	state, live := s.streams[frame.StreamID]
	if live {
		state.gone = true
		delete(s.streams, frame.StreamID)
	}
	_, ended := s.ended[frame.StreamID]
	s.mu.Unlock()
	if live {
		state.stream.Close()
		s.markEnded(frame.StreamID)
		return
	}
	if ended {
		// 客户端在流结束后会补发 cancel（EVIDENCE §3.3 的 finally）：容忍，不回错。
		return
	}
	s.sendError(frame.StreamID, protocolError(frame.StreamID, "cancel 了一条不存在的流"))
}

func (s *muxSession) failStream(state *streamState, err *Error) {
	s.mu.Lock()
	if _, live := s.streams[state.id]; !live {
		s.mu.Unlock()
		return
	}
	state.gone = true
	delete(s.streams, state.id)
	s.mu.Unlock()
	s.sendError(state.id, err)
	s.markEnded(state.id)
	state.stream.Close()
}

func (s *muxSession) isGone(state *streamState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return state.gone
}

func (s *muxSession) markEnded(streamID string) {
	s.mu.Lock()
	delete(s.streams, streamID)
	s.ended[streamID] = struct{}{}
	s.mu.Unlock()
}

func protocolError(streamID, message string) *Error {
	return &Error{Code: CodeProtocol, Message: message,
		Details: map[string]any{"streamId": streamID}}
}

func (s *muxSession) sendItem(streamID string, item StreamItem) bool {
	return s.send(hostFrame{Type: FrameItem, StreamID: streamID, Value: item.Value})
}

func (s *muxSession) sendEnd(streamID string) bool {
	return s.send(hostFrame{Type: FrameEnd, StreamID: streamID})
}

func (s *muxSession) sendError(streamID string, err *Error) bool {
	if err == nil {
		err = Errorf(CodeInternal, "officialwire: 空的流错误")
	}
	return s.send(hostFrame{Type: FrameError, StreamID: streamID, Error: err})
}

func (s *muxSession) send(frame hostFrame) bool {
	payload, err := json.Marshal(frame)
	if err != nil {
		return false
	}
	select {
	case s.out <- payload:
		return true
	case <-s.done:
		return false
	}
}

// writeLoop 把队列里的帧逐条写出。
//
// 单写者：官方下行也是串行化写（EVIDENCE §3.3），并发写同一 socket 会把
// 帧切开——那是最难查的一类"偶发"。
func (s *muxSession) writeLoop() {
	for {
		select {
		case payload := <-s.out:
			if err := s.conn.writeText(payload); err != nil {
				s.terminate()
				return
			}
		case <-s.done:
			return
		}
	}
}

// heartbeatLoop 是 2s×2 判死（EVIDENCE §3.3）。
//
// 判据：连续两次 tick 都没有收到**任何**上行（文本或 pong）→ terminate()。
// 所以真实死亡时刻是第 3 个 tick（3×interval），与官方的"两次未响应再断"一致。
func (s *muxSession) heartbeatLoop() {
	ticker := time.NewTicker(s.mux.cfg.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if s.missed.Load() >= 2 {
				s.terminate()
				return
			}
			s.missed.Add(1)
			if err := s.conn.writePing(nil); err != nil {
				s.terminate()
				return
			}
		}
	}
}

// shutdown 关一次会话：写关闭帧 + 关 TCP。可重复调用。
func (s *muxSession) shutdown(code int, reason string) {
	s.closeOnce.Do(func() {
		close(s.done)
		s.cancel()
		s.conn.closeWith(code, reason)
	})
}

// terminate 直接断 TCP，不写关闭帧（心跳判死与对端硬断走这条）。
func (s *muxSession) terminate() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.cancel()
		s.conn.terminate()
	})
}

// teardown 收尾：停掉所有流、摘掉 socket。
func (s *muxSession) teardown() {
	s.shutdown(1000, "")
	s.cancel()

	s.mu.Lock()
	states := make([]*streamState, 0, len(s.streams))
	for _, state := range s.streams {
		state.gone = true
		states = append(states, state)
	}
	s.streams = map[string]*streamState{}
	s.mu.Unlock()
	for _, state := range states {
		state.stream.Close()
	}

	// 流必须尊重 ctx；但不允许一个坏实现把连接 goroutine 永久挂死。
	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
	}
	s.mux.cfg.Peers.Detach(s.peer)
}

// eventsStream 是 `$events` 逻辑流：首个 item 是 ready，之后转发事件源的事件。
//
// 生命周期（官方 openRemoteEvents，lib/index.js:1021-1062 逐条对齐）：
//
//  1. 先**订阅**事件源，再发 ready —— 反过来的话，ready 与订阅之间发生的
//     事件会丢，而客户端已经"就绪"，那条事件永远不会再来。
//  2. 首帧 ready 带 clientId：客户端用它做 generation 身份，也是
//     `$events/result` 的关联键。
//  3. 之后逐条转发；事件源关闭（通道关闭）= 宿主发 `end` 帧。
//  4. ctx 取消 / Close() → 退订。退订之后事件源**不许**再发（发送方会永久阻塞），
//     所以订阅契约里 stop 必须幂等且自带"发完之后检查"。
//
// 事件源为 nil 时：ready 之后**什么都不发**。这不是"没实现"，是"这个宿主没有
// 转发事件源"的诚实表达——伪造一条 emit 会让客户端去等一个不存在的应答。
type eventsStream struct {
	clientID string
	home     string
	source   EventSource

	mu       sync.Mutex
	sent     bool
	events   <-chan Event
	stop     func()
	stopOnce sync.Once
}

func newEventsStream(clientID, home string, source EventSource) *eventsStream {
	return &eventsStream{clientID: clientID, home: home, source: source}
}

func (e *eventsStream) Next(ctx context.Context) (StreamItem, bool, *Error) {
	e.mu.Lock()
	first := !e.sent
	e.sent = true
	e.mu.Unlock()

	if first {
		// 订阅必须在发 ready **之前**（见类型注释第 1 条）。
		if e.source != nil {
			events, stop := e.source.Subscribe(ctx, e.clientID)
			e.mu.Lock()
			e.events = events
			e.stop = stop
			e.mu.Unlock()
		}
		ready, err := json.Marshal(map[string]any{
			"type":     EventReady,
			"clientId": e.clientID,
			"host":     map[string]any{"home": e.home},
		})
		if err != nil {
			return StreamItem{}, true, Errorf(CodeInternal, "officialwire: ready 帧序列化失败：%v", err)
		}
		return StreamItem{Value: ready, HasValue: true}, false, nil
	}

	e.mu.Lock()
	events := e.events
	e.mu.Unlock()
	if events == nil {
		// 没有事件源：ready 之后挂住，直到流被取消。这是**正常**的终态。
		<-ctx.Done()
		return StreamItem{}, true, nil
	}
	select {
	case event, ok := <-events:
		if !ok {
			// 事件源结束：宿主发 `end` 帧（官方 queue.end() 的语义）。
			return StreamItem{}, true, nil
		}
		payload, failure := event.Payload()
		if failure != nil {
			// 宿主自己发出了一条非法事件：**这一条**失败，别静默丢弃，
			// 也别断整条流（后面还有合法事件）。
			return StreamItem{}, false, failure
		}
		return StreamItem{Value: payload, HasValue: true}, false, nil
	case <-ctx.Done():
		return StreamItem{}, true, nil
	}
}

// Close 退订事件源。幂等：mux 的 cancel/teardown 两条路径都会调它。
func (e *eventsStream) Close() {
	e.mu.Lock()
	stop := e.stop
	e.mu.Unlock()
	if stop == nil {
		return
	}
	e.stopOnce.Do(stop)
}
