package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/agent"
	"dshgo/internal/session"
)

// Agent 是 ACP 服务端依赖的会话执行体（由 cmd 层用 agent.Loop 实现）。
type Agent interface {
	// Prompt 跑一个 turn；emit 里的每条事件都会被投影成 ACP update。
	Prompt(ctx context.Context, sessionID, cwd, text string, emit func(agent.Event) error) (stopReason string, usage map[string]any, err error)
	// Resume 把一个已持久化的会话接回内存；不重放旧 update。
	Resume(ctx context.Context, sessionID, cwd string) error
	// Fork 从已有会话派生一个新会话，返回新会话 id。
	Fork(ctx context.Context, sessionID, cwd string) (string, error)
	// Load 把一个已有会话载入并**重放**历史给客户端（resume 不重放）。
	Load(ctx context.Context, sessionID, cwd string, emit func(agent.Event) error) error
	// Delete 删除一个已持久化的会话。
	Delete(ctx context.Context, sessionID, cwd string) error
	// SetMode 切换会话模式，返回当前生效的模式 id。
	SetMode(ctx context.Context, sessionID, modeID string) (string, error)
	// Close 释放一个会话。
	Close(sessionID string) error
	// ListSessions 列出磁盘上的会话（不是"这个进程开过的"）。
	// cwd 非空时只列这个项目。
	ListSessions(ctx context.Context, cwd string) ([]session.Listing, error)
	// Shutdown 在服务端退出时调用：把所有会话日志落盘并关闭。
	Shutdown() error
}

type sess struct {
	id     string
	cwd    string
	cancel context.CancelFunc
	busy   bool
}

// Server 是 ACP 服务端。
type Server struct {
	In    io.Reader
	Out   io.Writer
	Agent Agent
	// Name/Version 会出现在 agentInfo 里。
	Name    string
	Version string

	// Err 是杂项日志输出（默认 stderr）；wg 用来等在飞的 turn 收尾。
	Err      io.Writer
	wg       sync.WaitGroup
	wmu      sync.Mutex
	mu       sync.Mutex
	sessions map[string]*sess
	order    []string
	callSeq  int
	pending  map[string]chan json.RawMessage
}

func (s *Server) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.Out.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *Server) reply(id json.RawMessage, result any, rerr *RPCError) error {
	resp := Response{JSONRPC: "2.0", ID: id}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	return s.write(resp)
}

func (s *Server) notify(method string, params any) error {
	return s.write(Notification{JSONRPC: "2.0", Method: method, Params: params})
}

// Serve 读 stdin 的消息直到 EOF 或 ctx 结束。
func (s *Server) Serve(ctx context.Context) error {
	if s.Name == "" {
		s.Name = "dsh-go-acp"
	}
	if s.Version == "" {
		s.Version = "0.2.2-go.1"
	}
	if s.Err == nil {
		s.Err = os.Stderr
	}
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]*sess{}
	}
	if s.pending == nil {
		s.pending = map[string]chan json.RawMessage{}
	}
	s.mu.Unlock()

	sc := bufio.NewScanner(s.In)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = s.reply(nil, nil, &RPCError{Code: CodeParseError, Message: "不是合法 JSON: " + err.Error()})
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// 没有 method 的是「客户端对我们请求的应答」，交给等待方。
		if req.Method == "" && len(req.ID) > 0 {
			s.resolve([]byte(line))
			continue
		}
		s.dispatch(ctx, req)
	}

	// stdin EOF = 客户端走了。先把在飞的 turn 取消掉——不然它们会继续跑，
	// 往一个没人读的管道里写，还占着会话文件。
	s.mu.Lock()
	for _, se := range s.sessions {
		if se.cancel != nil {
			se.cancel()
		}
	}
	s.mu.Unlock()

	// 等一下在飞的 turn 收尾：它们还要把取消的尾巴事件写进日志。
	// 不等就 Close，那些事件会丢（写已关闭的文件）。最多等 3 秒。
	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		fmt.Fprintf(s.Err, "断连后仍有 turn 没在 3 秒内收尾，强制退出\n")
	}

	return sc.Err()
}

func (s *Server) dispatch(ctx context.Context, req Request) {
	switch req.Method {
	case MethodInitialize:
		var p InitializeRequest
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
			return
		}
		_ = s.reply(req.ID, InitializeResponse{
			ProtocolVersion: 1,
			AgentInfo:       &AgentInfo{Name: s.Name, Version: s.Version},
			AgentCapabilities: map[string]any{
				"promptCapabilities": map[string]any{"image": false},
			},
		}, nil)

	case MethodAuthenticate:
		_ = s.reply(req.ID, map[string]any{}, nil)

	case MethodSessionNew:
		var p SessionNewRequest
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
			return
		}
		// 客户端可能带着 `\?\` 前缀（Node 的扩展长度路径），先归一化，
		// 否则会话会落进一个真实 DSH 永远不会产生的 projectKey 目录。
		p.Cwd = v4.NormalizeCwd(p.Cwd)
		if p.Cwd == "" || !filepath.IsAbs(filepath.FromSlash(strings.ReplaceAll(p.Cwd, "\\", "/"))) {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "cwd 必须是绝对路径"})
			return
		}
		id := "acp-" + NewUUID()
		// 建会话就定模式：先定再建文件，这样 header.agentPreset 从一开始就是对的，
		// 不存在"建完全权限、再切只读"的窗口。
		if p.Mode != "" {
			if _, err := s.Agent.SetMode(ctx, id, p.Mode); err != nil {
				_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
				return
			}
		}
		s.mu.Lock()
		s.sessions[id] = &sess{id: id, cwd: p.Cwd}
		s.order = append(s.order, id)
		s.mu.Unlock()
		_ = s.reply(req.ID, SessionNewResponse{
			SessionID: id,
			ConfigOptions: []ConfigOption{
				{ID: "model", Name: "Model"},
				{ID: "reasoning_effort", Name: "Reasoning effort"},
			},
		}, nil)

	case MethodSessionList:
		var p struct {
			Cwd string `json:"cwd"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if s.Agent == nil {
			_ = s.reply(req.ID, map[string]any{"sessions": []any{}}, nil)
			return
		}
		list, err := s.Agent.ListSessions(ctx, p.Cwd)
		if err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInternalError, Message: "列会话失败: " + err.Error()})
			return
		}
		_ = s.reply(req.ID, map[string]any{"sessions": list}, nil)

	case MethodSessionResume:
		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
			return
		}
		// cwd 不是必填：session id 全局唯一，按 id 就能找到日志。
		// 要求 cwd 的话，"换个盘符/别人机器上恢复"就做不到了——客户端手里常常
		// 只有 id（比如收回信、接别人的活）。cwd 给了就优先按它算。
		if p.SessionID == "" {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "sessionId 必填"})
			return
		}
		if s.Agent == nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInternalError, Message: "没有配置 Agent"})
			return
		}
		if err := s.Agent.Resume(ctx, p.SessionID, p.Cwd); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{
				Code: CodeInvalidParams, Message: sessionErrText("恢复", p.SessionID, err),
			})
			return
		}
		s.mu.Lock()
		s.sessions[p.SessionID] = &sess{id: p.SessionID, cwd: p.Cwd}
		s.mu.Unlock()
		_ = s.reply(req.ID, SessionNewResponse{
			SessionID: p.SessionID,
			ConfigOptions: []ConfigOption{
				{ID: "model", Name: "Model"},
				{ID: "reasoning_effort", Name: "Reasoning effort"},
			},
		}, nil)

	case "session/load":
		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
			return
		}
		// cwd 不是必填：session id 全局唯一，按 id 就能找到日志。
		// 要求 cwd 的话，"换个盘符/别人机器上恢复"就做不到了——客户端手里常常
		// 只有 id（比如收回信、接别人的活）。cwd 给了就优先按它算。
		if p.SessionID == "" {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "sessionId 必填"})
			return
		}
		if s.Agent == nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInternalError, Message: "没有配置 Agent"})
			return
		}
		emit := func(ev agent.Event) error {
			if u, ok := updateFor(ev); ok {
				return s.notify("session/update", map[string]any{"sessionId": p.SessionID, "update": u})
			}
			return nil
		}
		if err := s.Agent.Load(ctx, p.SessionID, p.Cwd, emit); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{
				Code: CodeInvalidParams, Message: sessionErrText("载入", p.SessionID, err),
			})
			return
		}
		s.mu.Lock()
		s.sessions[p.SessionID] = &sess{id: p.SessionID, cwd: p.Cwd}
		s.mu.Unlock()
		_ = s.reply(req.ID, SessionNewResponse{SessionID: p.SessionID}, nil)

	case "session/delete":
		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
			return
		}
		if p.SessionID == "" {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "sessionId 必填"})
			return
		}
		// 客户端（SDK 就是这样的）只带 sessionId 不带 cwd；cwd 缺失时由 Agent 自己去查。
		if p.Cwd == "" {
			s.mu.Lock()
			if se, ok := s.sessions[p.SessionID]; ok {
				p.Cwd = se.cwd
			}
			s.mu.Unlock()
		}
		if err := s.Agent.Delete(ctx, p.SessionID, p.Cwd); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "删除失败: " + err.Error()})
			return
		}
		s.mu.Lock()
		delete(s.sessions, p.SessionID)
		s.mu.Unlock()
		_ = s.reply(req.ID, map[string]any{}, nil)

	case "session/set_mode":
		var p struct {
			SessionID string `json:"sessionId"`
			ModeID    string `json:"modeId"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
			return
		}
		if p.SessionID == "" || p.ModeID == "" {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "sessionId 与 modeId 都必填"})
			return
		}
		mode, err := s.Agent.SetMode(ctx, p.SessionID, p.ModeID)
		if err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
			return
		}
		_ = s.reply(req.ID, map[string]any{"modeId": mode}, nil)

	case "session/fork":
		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
			return
		}
		if p.SessionID == "" {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "sessionId 必填"})
			return
		}
		s.mu.Lock()
		src, ok := s.sessions[p.SessionID]
		s.mu.Unlock()
		if !ok {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "未知会话: " + p.SessionID})
			return
		}
		cwd := p.Cwd
		if cwd == "" {
			cwd = src.cwd
		}
		newID, err := s.Agent.Fork(ctx, p.SessionID, cwd)
		if err != nil {
			_ = s.reply(req.ID, nil, &RPCError{Code: CodeInternalError, Message: "fork 失败: " + err.Error()})
			return
		}
		s.mu.Lock()
		s.sessions[newID] = &sess{id: newID, cwd: cwd}
		s.order = append(s.order, newID)
		s.mu.Unlock()
		_ = s.reply(req.ID, SessionNewResponse{SessionID: newID}, nil)

	case MethodSessionClose:
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.drop(p.SessionID)
		_ = s.reply(req.ID, map[string]any{}, nil)

	case MethodSessionCancel:
		var p SessionCancelParams
		_ = json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		if se, ok := s.sessions[p.SessionID]; ok && se.cancel != nil {
			se.cancel()
		}
		s.mu.Unlock()
		if len(req.ID) > 0 {
			_ = s.reply(req.ID, map[string]any{}, nil)
		}

	case MethodSessionPrompt:
		// prompt 必须异步：客户端会在它 settle 之前发 session/cancel，
		// 同步处理会把读循环堵死，取消永远进不来。
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handlePrompt(ctx, req)
		}()

	case MethodSessionSetConfig:
		var p struct {
			SessionID string `json:"sessionId"`
			ConfigID  string `json:"configId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		_ = s.reply(req.ID, map[string]any{"configOptions": []ConfigOption{
			{ID: "model", Name: "Model"},
			{ID: "reasoning_effort", Name: "Reasoning effort"},
		}}, nil)

	default:
		_ = s.reply(req.ID, nil, &RPCError{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("不支持的方法: %s", req.Method),
		})
	}
}

func (s *Server) handlePrompt(ctx context.Context, req Request) {
	var p SessionPromptRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()})
		return
	}
	s.mu.Lock()
	se, ok := s.sessions[p.SessionID]
	if !ok {
		s.mu.Unlock()
		_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "未知会话: " + p.SessionID})
		return
	}
	if se.busy {
		s.mu.Unlock()
		_ = s.reply(req.ID, nil, &RPCError{Code: CodeInvalidRequest, Message: "该会话已有在飞的 prompt"})
		return
	}
	se.busy = true
	runCtx, cancel := context.WithCancel(ctx)
	se.cancel = cancel
	s.mu.Unlock()

	// release 释放 busy 标志。必须在**发出终态应答之前**调用：
	// 客户端收到 cancelled 之后会立刻发下一条 prompt，
	// 若此时 busy 还没清，那条请求会被"已有在飞的 prompt"挡掉——
	// 用户看到的就是"取消完这一轮就不能再说话了"。
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			s.mu.Lock()
			if cur, ok := s.sessions[p.SessionID]; ok && cur == se {
				se.busy = false
				se.cancel = nil
			}
			s.mu.Unlock()
		})
	}
	defer release()

	text := promptText(p.Prompt)
	if strings.TrimSpace(text) == "" {
		release()
		_ = s.reply(req.ID, SessionPromptResponse{StopReason: "end_turn"}, nil)
		return
	}

	var mu sync.Mutex
	msgID := ""
	emit := func(ev agent.Event) error {
		mu.Lock()
		defer mu.Unlock()
		switch ev.Kind {
		case agent.EventAssistantMsg:
			msgID = "msg-" + NewUUID()
		case agent.EventReasoning:
			return s.sendUpdate(p.SessionID, map[string]any{
				"sessionUpdate": "agent_thought_chunk",
				"messageId":     msgID,
				"content":       map[string]any{"type": "text", "text": ev.Text},
			})
		case agent.EventText:
			return s.sendUpdate(p.SessionID, map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"messageId":     msgID,
				"content":       map[string]any{"type": "text", "text": ev.Text},
			})
		case agent.EventToolCall:
			return s.sendUpdate(p.SessionID, map[string]any{
				"sessionUpdate": "tool_call",
				"toolCallId":    ev.CallID,
				"title":         ev.ToolName,
				"kind":          "other",
				"status":        "in_progress",
				"rawInput":      rawJSON(ev.Arguments),
			})
		case agent.EventUsage:
			return s.sendUpdate(p.SessionID, map[string]any{
				"sessionUpdate": "usage_update",
				"used":          ev.UsageUsed,
				"size":          ev.UsageSize,
			})
		case agent.EventToolResult:
			status := "completed"
			if ev.IsError {
				status = "failed"
			}
			text := ""
			if ev.Result != nil {
				text = ev.Result.Summary
			}
			return s.sendUpdate(p.SessionID, map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    ev.CallID,
				"status":        status,
				"content": []any{map[string]any{
					"type":    "content",
					"content": map[string]any{"type": "text", "text": text},
				}},
			})
		}
		return nil
	}

	stop, usage, err := s.Agent.Prompt(runCtx, p.SessionID, se.cwd, text, emit)
	if err != nil {
		if runCtx.Err() != nil {
			release()
			_ = s.reply(req.ID, SessionPromptResponse{StopReason: "cancelled"}, nil)
			return
		}
		_ = s.reply(req.ID, nil, &RPCError{Code: CodeInternalError, Message: err.Error()})
		return
	}
	release()
	_ = s.reply(req.ID, SessionPromptResponse{StopReason: stop, Usage: usage}, nil)
}

// CallClient 发一个 agent→client 的请求并等应答（目前只用于 session/request_permission）。
func (s *Server) CallClient(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]chan json.RawMessage{}
	}
	s.callSeq++
	id := fmt.Sprintf("srv-%d", s.callSeq)
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	if err := s.write(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		return nil, err
	}
	select {
	case raw := <-ch:
		return raw, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// resolve 把客户端应答投递给等待方。
func (s *Server) resolve(raw []byte) {
	var env struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	var key string
	if err := json.Unmarshal(env.ID, &key); err != nil {
		return
	}
	s.mu.Lock()
	ch := s.pending[key]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- append([]byte(nil), raw...):
		default:
		}
	}
}

func (s *Server) sendUpdate(sessionID string, update map[string]any) error {
	return s.notify(MethodSessionUpdate, SessionUpdateParams{SessionID: sessionID, Update: update})
}

func (s *Server) drop(sessionID string) {
	s.mu.Lock()
	se, ok := s.sessions[sessionID]
	if ok {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()
	if ok {
		if se.cancel != nil {
			se.cancel()
		}
		if s.Agent != nil {
			_ = s.Agent.Close(sessionID)
		}
	}
}

// promptText 把相邻 text 块拼成一条消息（ACP 规定）。
func promptText(blocks []ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func rawJSON(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	return v
}

// updateFor 把一条 agent 事件投影成 ACP update（**无状态**版本）。
//
// prompt 那条路径需要一个跨事件的 msgID 做增量拼接，所以自己维护；
// 这里是给 session/load 重放历史用的：一条事件一个 update，不需要拼。
func updateFor(ev agent.Event) (map[string]any, bool) {
	switch ev.Kind {
	case agent.EventReasoning:
		return map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": ev.Text},
		}, true
	case agent.EventText:
		return map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": ev.Text},
		}, true
	case agent.EventToolCall:
		return map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    ev.CallID,
			"title":         ev.ToolName,
			"kind":          "other",
			"status":        "completed",
			"rawInput":      rawJSON(ev.Arguments),
		}, true
	case agent.EventToolResult:
		status := "completed"
		if ev.IsError {
			status = "failed"
		}
		text := ""
		if ev.Result != nil {
			text = ev.Result.Summary
		}
		return map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    ev.CallID,
			"status":        status,
			"content": []any{map[string]any{
				"type":    "content",
				"content": map[string]any{"type": "text", "text": text},
			}},
		}, true
	}
	return nil, false
}

// sessionErrText 把"找不到会话"和"真出错了"分开说。
//
// 原来直接把 err.Error() 拼进去，客户端看到的是这样一串：
//
//	载入失败: 读会话失败: 读会话文件失败: open <home>/sessions/.../session.v4.jsonl.zstd:
//	the system cannot find the file specified
//
// 里面是宿主路径和三层嵌套。客户端要的是一句话：**没有这个会话**。
// 路径是我们自己的实现细节，不该出现在协议层。
func sessionErrText(verb, sessionID string, err error) string {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return verb + "失败: 找不到会话 " + sessionID
	}
	return verb + "失败: " + err.Error()
}
