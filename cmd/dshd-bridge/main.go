// dshd-bridge：把 dshd（ACP over stdio）伪装成 DSH 宿主的 HTTP 契约，
// 让 dsh-tui 这类 **DSH 客户端**直接连上来。
//
// 为什么需要它：dsh-tui 说的是 DSH 的客户端契约（`POST /api/<method>` +
// `GET /api/events.mux` 的 SSE），不是 ACP；dshd 说的是 ACP（stdio JSON-RPC）。
// 两边都对，只是不是一种语言。这个桥只做翻译，不含业务。
//
// 设计上刻意做薄的地方：**事件的真相在会话日志里**（append-only 的 v4 文件），
// 桥不自己攒事件，只把日志增量翻成 mux 帧。这样"历史"和"实时"天然一致，
// 也不会出现两份事件真值。
//
// 注意：这是**界面线的原型**，不是宿主线的产品件。宿主线不改它。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
)

// ---------------- ACP 客户端 ----------------

type acpClient struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader

	mu      sync.Mutex
	nextID  int
	waiters map[int]chan json.RawMessage
	// 服务端反向请求（session/request_permission）等客户端回答
	pending map[string]chan json.RawMessage
}

func newACP(exe string, args []string) (*acpClient, error) {
	cmd := exec.Command(exe, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &acpClient{
		cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20),
		waiters: map[int]chan json.RawMessage{},
		pending: map[string]chan json.RawMessage{},
	}
	go c.readLoop()
	return c, nil
}

func (c *acpClient) readLoop() {
	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			c.mu.Lock()
			for _, ch := range c.waiters {
				close(ch)
			}
			c.waiters = map[int]chan json.RawMessage{}
			c.mu.Unlock()
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if msg.Method != "" && len(msg.ID) > 0 {
			// 服务端 → 客户端 的请求：登记起来等客户端回答。
			var key string
			if json.Unmarshal(msg.ID, &key) != nil {
				continue
			}
			ch := make(chan json.RawMessage, 1)
			c.mu.Lock()
			c.pending[key] = ch
			c.mu.Unlock()
			go func() { // 没人回答就给个"取消"，别把一轮挂死
				select {
				case <-ch:
				case <-time.After(60 * time.Second):
					c.respond(key, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
				}
			}()
			continue
		}
		var id int
		if json.Unmarshal(msg.ID, &id) != nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.waiters[id]
		delete(c.waiters, id)
		c.mu.Unlock()
		if ok {
			ch <- msg.Result
		}
	}
}

func (c *acpClient) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan json.RawMessage, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	c.mu.Lock()
	_, err := c.in.Write(append(body, '\n'))
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case res, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("ACP 连接已断")
		}
		return res, nil
	case <-time.After(10 * time.Minute):
		return nil, fmt.Errorf("ACP 调用超时: %s", method)
	}
}

func (c *acpClient) respond(reqID string, result any) {
	c.mu.Lock()
	ch, ok := c.pending[reqID]
	delete(c.pending, reqID)
	c.mu.Unlock()
	if ok {
		select {
		case ch <- nil:
		default:
		}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": reqID, "result": result})
	c.mu.Lock()
	c.in.Write(append(body, '\n'))
	c.mu.Unlock()
}

// ---------------- 会话 ----------------

type bridge struct {
	home     string
	exe      string
	extra    []string
	mu       sync.Mutex
	sessions map[string]*sess
}

type sess struct {
	id      string
	path    string
	cwd     string
	preset  string
	acp     *acpClient
	acpID   string
	lastSeq int64
	running bool
}

func (b *bridge) acpFor() (*acpClient, error) { return newACP(b.exe, b.extra) }

func (b *bridge) findLog(id string) string {
	matches, _ := filepath.Glob(filepath.Join(b.home, "sessions", "*", id, "session.v4.jsonl.zstd"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// ---------------- HTTP 契约 ----------------

type envelope struct {
	Type    string          `json:"type"`
	RPCID   string          `json:"rpcId"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

func writeResult(w http.ResponseWriter, rpcID string, value any) {
	json.NewEncoder(w).Encode(map[string]any{
		"type": "server-response", "rpcId": rpcID,
		"result": map[string]any{"ok": true, "value": value},
	})
}

func writeError(w http.ResponseWriter, rpcID, code, msg string) {
	json.NewEncoder(w).Encode(map[string]any{
		"type": "server-response", "rpcId": rpcID,
		"result": map[string]any{"ok": false, "error": map[string]any{"code": code, "message": msg}},
	})
}

func (b *bridge) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/events.mux" {
		b.mux(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	var env envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		http.Error(w, "bad envelope", 400)
		return
	}
	w.Header().Set("content-type", "application/json")

	switch method {
	case "session.create":
		b.createSession(w, env)
	case "session.list":
		b.listSessions(w, env)
	case "session.history":
		b.history(w, env)
	case "session.prompt":
		b.prompt(w, env)
	case "session.cancel":
		b.cancel(w, env)
	case "subagent.list":
		writeResult(w, env.RPCID, map[string]any{"items": []any{}})
	case "rpc.respond":
		b.respondRPC(w, env)
	default:
		writeError(w, env.RPCID, "method-not-found", "桥没有实现 "+method)
	}
}

func (b *bridge) createSession(w http.ResponseWriter, env envelope) {
	var p struct {
		Cwd         string `json:"cwd"`
		AgentPreset string `json:"agentPreset"`
	}
	json.Unmarshal(env.Payload, &p)
	if p.Cwd == "" {
		p.Cwd, _ = os.Getwd()
	}
	if p.AgentPreset == "" {
		p.AgentPreset = "default" // dsh-tui 默认要 "code"（PTC），我们没有那个 preset
	}

	acp, err := b.acpFor()
	if err != nil {
		writeError(w, env.RPCID, "acp-spawn-failed", err.Error())
		return
	}
	if _, err := acp.call("initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
	}); err != nil {
		writeError(w, env.RPCID, "acp-init-failed", err.Error())
		return
	}
	res, err := acp.call("session/new", map[string]any{
		"cwd": filepath.ToSlash(p.Cwd), "mcpServers": []any{}, "mode": p.AgentPreset,
	})
	if err != nil {
		writeError(w, env.RPCID, "acp-new-failed", err.Error())
		return
	}
	var sr struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(res, &sr)

	s := &sess{id: sr.SessionID, cwd: p.Cwd, preset: p.AgentPreset, acp: acp, acpID: sr.SessionID}
	s.path = b.waitLog(sr.SessionID, 5*time.Second)
	b.mu.Lock()
	b.sessions[sr.SessionID] = s
	b.mu.Unlock()

	writeResult(w, env.RPCID, map[string]any{"sessionId": sr.SessionID, "agentPreset": p.AgentPreset})
}

func (b *bridge) waitLog(id string, d time.Duration) string {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p := b.findLog(id); p != "" {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	return b.findLog(id)
}

func (b *bridge) listSessions(w http.ResponseWriter, env envelope) {
	items, err := session.ListSessions(b.home, "")
	if err != nil {
		writeError(w, env.RPCID, "list-failed", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"sessionId": it.SessionID, "cwd": it.Cwd, "agentPreset": it.Preset,
			"updatedAt": it.UpdatedAt.UTC().Format(time.RFC3339),
			"summary":   it.Summary, "events": it.Events,
		})
	}
	writeResult(w, env.RPCID, map[string]any{"items": out})
}
func (b *bridge) history(w http.ResponseWriter, env envelope) {
	var p struct {
		SessionID   string `json:"sessionId"`
		MaxMessages int    `json:"maxMessages"`
	}
	json.Unmarshal(env.Payload, &p)
	path := b.logPath(p.SessionID)
	if path == "" {
		writeError(w, env.RPCID, "session-not-found", "找不到会话 "+p.SessionID)
		return
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		writeError(w, env.RPCID, "history-failed", err.Error())
		return
	}
	entries := make([]map[string]any, 0, len(lg.Events))
	for _, ev := range lg.Events {
		var data any
		json.Unmarshal(ev.Data, &data)
		entries = append(entries, map[string]any{"event": map[string]any{
			"type": ev.Type, "seq": ev.Seq, "time": ev.Time, "data": data,
		}})
	}
	writeResult(w, env.RPCID, map[string]any{"events": entries, "lastSeq": lastSeqOf(lg.Events)})
}

func lastSeqOf(evs []v4.Event) int64 {
	var m int64 = -1
	for _, e := range evs {
		if e.Seq > m {
			m = e.Seq
		}
	}
	return m
}

func (b *bridge) logPath(id string) string {
	b.mu.Lock()
	s := b.sessions[id]
	b.mu.Unlock()
	if s != nil && s.path != "" {
		return s.path
	}
	return b.findLog(id)
}
func (b *bridge) prompt(w http.ResponseWriter, env envelope) {
	var p struct {
		SessionID string `json:"sessionId"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(env.Payload, &p)

	b.mu.Lock()
	s := b.sessions[p.SessionID]
	b.mu.Unlock()
	if s == nil {
		if path := b.findLog(p.SessionID); path != "" {
			if acp, err := b.acpFor(); err == nil {
				acp.call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
				acp.call("session/resume", map[string]any{"sessionId": p.SessionID, "cwd": "", "mcpServers": []any{}})
				s = &sess{id: p.SessionID, acp: acp, acpID: p.SessionID, path: path}
				b.mu.Lock()
				b.sessions[p.SessionID] = s
				b.mu.Unlock()
			}
		}
	}
	if s == nil {
		writeError(w, env.RPCID, "session-not-found", "找不到会话 "+p.SessionID)
		return
	}
	if s.running {
		writeError(w, env.RPCID, "busy", "上一轮还没结束")
		return
	}

	var text strings.Builder
	for _, c := range p.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	s.running = true
	writeResult(w, env.RPCID, map[string]any{"accepted": true})

	go func() {
		defer func() {
			b.mu.Lock()
			s.running = false
			b.mu.Unlock()
		}()
		if _, err := s.acp.call("session/prompt", map[string]any{
			"sessionId": s.acpID,
			"prompt":    []any{map[string]any{"type": "text", "text": text.String()}},
		}); err != nil {
			log.Printf("prompt 失败: %v", err)
		}
	}()
}

func (b *bridge) cancel(w http.ResponseWriter, env envelope) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(env.Payload, &p)
	b.mu.Lock()
	s := b.sessions[p.SessionID]
	b.mu.Unlock()
	if s != nil {
		s.acp.call("session/cancel", map[string]any{"sessionId": s.acpID})
	}
	writeResult(w, env.RPCID, map[string]any{"ok": true})
}

func (b *bridge) respondRPC(w http.ResponseWriter, env envelope) {
	var p struct {
		RPCID  string          `json:"rpcId"`
		Result json.RawMessage `json:"result"`
	}
	json.Unmarshal(env.Payload, &p)
	var res struct {
		OK    bool            `json:"ok"`
		Value json.RawMessage `json:"value"`
	}
	json.Unmarshal(p.Result, &res)
	var v struct {
		OptionID string `json:"optionId"`
	}
	json.Unmarshal(res.Value, &v)
	opt := v.OptionID
	if opt == "" {
		opt = "allow-once"
	}
	for _, s := range b.allSessions() {
		s.acp.respond(p.RPCID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": opt}})
	}
	writeResult(w, env.RPCID, map[string]any{"accepted": true})
}

func (b *bridge) allSessions() []*sess {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*sess, 0, len(b.sessions))
	for _, s := range b.sessions {
		out = append(out, s)
	}
	return out
}

// mux 是 SSE 事件流：把会话日志的增量翻成 server-request 帧。
func (b *bridge) mux(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			b.emitNew(w, flusher)
		}
	}
}

func (b *bridge) emitNew(w http.ResponseWriter, f http.Flusher) {
	b.mu.Lock()
	sessions := make([]*sess, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.mu.Unlock()

	for _, s := range sessions {
		if s.path == "" {
			continue
		}
		lg, err := v4.ReadFile(s.path)
		if err != nil {
			continue
		}
		for _, ev := range lg.Events {
			if ev.Seq <= s.lastSeq {
				continue
			}
			s.lastSeq = ev.Seq
			var data any
			json.Unmarshal(ev.Data, &data)
			frame := map[string]any{
				"type": "server-request", "rpcId": fmt.Sprintf("mux-%s-%d", s.id, ev.Seq),
				"method": "session/event",
				"payload": map[string]any{
					"sessionId": s.id,
					"event": map[string]any{
						"type": ev.Type, "seq": ev.Seq, "time": ev.Time, "data": data,
					},
				},
			}
			body, _ := json.Marshal(frame)
			fmt.Fprintf(w, "data: %s\n\n", body)
		}
	}
	f.Flush()
}

func main() {
	home := flag.String("home", "", "会话 home（要和 dshd 用同一个）")
	exe := flag.String("dshd", "", "dshd 可执行文件")
	addr := flag.String("addr", "127.0.0.1:3080", "监听地址（dsh-tui 默认连 3080）")
	flag.Parse()

	if *home == "" {
		fmt.Fprintln(os.Stderr, "必须给 --home")
		os.Exit(2)
	}
	exePath := *exe
	if exePath == "" {
		self, _ := os.Executable()
		dir := filepath.Dir(self)
		// 宿主的产品名就是**文件名**：`DSH GO.exe`（scripts/build.sh 的产物）。
		// 旧的 `dshd.exe` 只作兜底——老构建件还躺在某些目录里，
		// 桥不该因为改名就突然"找不到宿主"，那会被读成产品坏了。
		exePath = filepath.Join(dir, "DSH GO.exe")
		if _, statErr := os.Stat(exePath); statErr != nil {
			exePath = filepath.Join(dir, "dshd.exe")
		}
	}
	b := &bridge{home: *home, exe: exePath,
		extra: []string{"acp", "--home", *home}, sessions: map[string]*sess{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/", b.handle)
	log.Printf("dshd-bridge 监听 %s（home=%s, dshd=%s）", *addr, *home, exePath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
