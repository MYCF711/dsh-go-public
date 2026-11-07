package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dshgo/internal/transport"
)

type mcpServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	Enabled *bool             `json:"enabled"`
}

func (c mcpServerConfig) enabled() bool { return c.Enabled == nil || *c.Enabled }

type mcpConnection struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	reader  *bufio.Reader
	stderr  *mcpCappedBuffer
	tree    *mcpProcessTree
	done    chan struct{}
	waitErr error
	nextID  int64
}

type mcpServerState struct {
	name      string
	config    mcpServerConfig
	raw       map[string]json.RawMessage
	conn      *mcpConnection
	status    string
	lastError string
	toolCount *int
}

type mcpManager struct {
	mu      sync.Mutex
	path    string
	root    map[string]json.RawMessage
	servers map[string]*mcpServerState
	loadErr error
	closed  bool
}

func openMCPManager(home string) *mcpManager {
	m := &mcpManager{path: filepath.Join(home, "mcp.json"), servers: map[string]*mcpServerState{}}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			m.root = map[string]json.RawMessage{"mcpServers": json.RawMessage(`{}`)}
			return m
		}
		m.loadErr = err
		return m
	}
	if err := json.Unmarshal(raw, &m.root); err != nil || m.root == nil {
		m.loadErr = fmt.Errorf("解析 MCP 配置 %s: %w", m.path, err)
		return m
	}
	var servers map[string]json.RawMessage
	rawServers, exists := m.root["mcpServers"]
	if !exists {
		servers = map[string]json.RawMessage{}
	} else if err := json.Unmarshal(rawServers, &servers); err != nil || servers == nil {
		m.loadErr = fmt.Errorf("MCP 配置 mcpServers 必须是对象: %w", err)
		return m
	}
	for name, rawServer := range servers {
		var fields map[string]json.RawMessage
		var config mcpServerConfig
		if name == "" || json.Unmarshal(rawServer, &fields) != nil || json.Unmarshal(rawServer, &config) != nil || config.Command == "" {
			m.loadErr = fmt.Errorf("MCP server %q 配置无效：需要非空 name/command 与合法参数", name)
			return m
		}
		m.servers[name] = &mcpServerState{name: name, config: config, raw: fields, status: "disconnected"}
	}
	return m
}

func (m *mcpManager) StartAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil || m.closed {
		return
	}
	for _, name := range m.sortedNamesLocked() {
		server := m.servers[name]
		if server.config.enabled() {
			_ = m.connectLocked(server)
		} else {
			server.status = "disabled"
		}
	}
}

func (m *mcpManager) sortedNamesLocked() []string {
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *mcpManager) List() ([]transport.MCPServerView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	out := make([]transport.MCPServerView, 0, len(m.servers))
	for _, name := range m.sortedNamesLocked() {
		server := m.servers[name]
		m.refreshLocked(server)
		row := transport.MCPServerView{Name: name, Status: server.status}
		if server.toolCount != nil {
			count := *server.toolCount
			row.ToolCount = &count
		}
		out = append(out, row)
	}
	return out, nil
}

func (m *mcpManager) refreshLocked(server *mcpServerState) {
	if !server.config.enabled() {
		server.status = "disabled"
		server.toolCount = nil
		return
	}
	if server.conn == nil {
		if server.status != "error" {
			server.status = "disconnected"
		}
		server.toolCount = nil
		return
	}
	select {
	case <-server.conn.done:
		if server.conn.waitErr != nil {
			server.status = "error"
			server.lastError = server.conn.waitErr.Error()
		} else if server.status != "error" {
			server.status = "disconnected"
		}
		server.toolCount = nil
	default:
	}
}

func (m *mcpManager) Reconnect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return m.loadErr
	}
	if m.closed {
		return errors.New("MCP manager 已关闭")
	}
	server := m.servers[name]
	if server == nil {
		return fmt.Errorf("%w: %s", transport.ErrNoSuchMCPServer, name)
	}
	if !server.config.enabled() {
		return fmt.Errorf("MCP server %s 已 disabled，不能 reconnect", name)
	}
	m.stopLocked(server)
	if err := m.connectLocked(server); err != nil {
		return err
	}
	return nil
}

func (m *mcpManager) Toggle(name string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return m.loadErr
	}
	if m.closed {
		return errors.New("MCP manager 已关闭")
	}
	server := m.servers[name]
	if server == nil {
		return fmt.Errorf("%w: %s", transport.ErrNoSuchMCPServer, name)
	}
	oldRaw := append(json.RawMessage(nil), m.root["mcpServers"]...)
	newRawServer := make(map[string]json.RawMessage, len(server.raw)+1)
	for key, value := range server.raw {
		newRawServer[key] = value
	}
	encodedEnabled, _ := json.Marshal(enabled)
	newRawServer["enabled"] = encodedEnabled
	rawServer, _ := json.Marshal(newRawServer)
	servers := map[string]json.RawMessage{}
	for key, value := range m.servers {
		if key == name {
			servers[key] = rawServer
		} else {
			servers[key], _ = json.Marshal(value.raw)
		}
	}
	rawServers, _ := json.Marshal(servers)
	m.root["mcpServers"] = rawServers
	if err := m.flushLocked(); err != nil {
		m.root["mcpServers"] = oldRaw
		return err
	}
	server.raw = newRawServer
	server.config.Enabled = &enabled
	if !enabled {
		m.stopLocked(server)
		server.status = "disabled"
		server.toolCount = nil
		return nil
	}
	server.status = "disconnected"
	if err := m.connectLocked(server); err != nil {
		// 磁盘与内存都保持 enabled=true + error；不得回滚成用户没有请求过的状态。
		return err
	}
	return nil
}

func (m *mcpManager) flushLocked() error {
	raw, err := json.MarshalIndent(m.root, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mcp-*.tmp")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err = tmp.Write(append(raw, '\n')); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(path, m.path)
}

func (m *mcpManager) connectLocked(server *mcpServerState) error {
	m.stopLocked(server)
	server.status = "connecting"
	server.toolCount = nil
	runCtx, cancel := context.WithCancel(context.Background())
	config := server.config
	cmd := exec.CommandContext(runCtx, config.Command, config.Args...)
	cmd.Dir = config.Cwd
	cmd.Env = os.Environ()
	for key, value := range config.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stderr := &mcpCappedBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		server.status = "error"
		server.lastError = err.Error()
		return err
	}
	tree, err := attachMCPProcessTree(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cancel()
		server.status = "error"
		server.lastError = err.Error()
		return err
	}
	conn := &mcpConnection{cmd: cmd, cancel: cancel, stdin: stdin, reader: bufio.NewReaderSize(stdout, 64*1024), stderr: stderr, tree: tree, done: make(chan struct{})}
	server.conn = conn
	go func() { conn.waitErr = cmd.Wait(); close(conn.done) }()
	handshakeCtx, handshakeCancel := context.WithTimeout(runCtx, 10*time.Second)
	stopHandshakeWatch := context.AfterFunc(handshakeCtx, conn.cancel)
	defer func() {
		stopHandshakeWatch()
		handshakeCancel()
	}()
	fail := func(err error) error {
		server.status = "error"
		server.lastError = err.Error()
		m.stopLocked(server)
		return err
	}
	var initialize json.RawMessage
	if err := conn.rpc(handshakeCtx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "dsh-go", "version": version},
	}, &initialize); err != nil {
		return fail(err)
	}
	if err := conn.notify("notifications/initialized", map[string]any{}); err != nil {
		return fail(err)
	}
	count := 0
	cursor := ""
	for page := 0; page < 1024; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var tools struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := conn.rpc(handshakeCtx, "tools/list", params, &tools); err != nil {
			return fail(err)
		}
		for _, rawTool := range tools.Tools {
			var tool struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(rawTool, &tool) != nil || tool.Name == "" {
				return fail(errors.New("MCP tools/list 含无名工具或非法行"))
			}
		}
		count += len(tools.Tools)
		if tools.NextCursor == "" {
			server.toolCount = &count
			server.status = "connected"
			server.lastError = ""
			return nil
		}
		if tools.NextCursor == cursor {
			return fail(errors.New("MCP tools/list nextCursor 没有前进"))
		}
		cursor = tools.NextCursor
	}
	return fail(errors.New("MCP tools/list 超过 1024 页"))
}

func (m *mcpManager) stopLocked(server *mcpServerState) {
	conn := server.conn
	server.conn = nil
	server.toolCount = nil
	if conn == nil {
		return
	}
	_ = conn.stdin.Close()
	conn.cancel()
	if conn.tree != nil {
		_ = conn.tree.Stop()
	}
	if conn.cmd.Process != nil {
		_ = conn.cmd.Process.Kill() // 只杀本 manager 启动且已登记的 PID。
	}
	select {
	case <-conn.done:
	case <-time.After(5 * time.Second):
		server.lastError = "MCP 子进程退出等待超时"
	}
	if conn.tree != nil {
		_ = conn.tree.Close()
	}
}

func (m *mcpManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	for _, server := range m.servers {
		m.stopLocked(server)
	}
}

func (c *mcpConnection) rpc(ctx context.Context, method string, params any, result any) error {
	c.nextID++
	id := c.nextID
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	type response struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := c.stdin.Write(append(raw, '\n')); err != nil {
			return err
		}
		for {
			line, err := readMCPLine(c.reader, 4<<20)
			if err != nil {
				return err
			}
			var message response
			if err := json.Unmarshal(line, &message); err != nil {
				return err
			}
			if len(message.ID) == 0 {
				continue
			}
			var gotID int64
			if json.Unmarshal(message.ID, &gotID) != nil || gotID != id {
				continue
			}
			if message.Error != nil {
				return errors.New("MCP " + method + ": " + message.Error.Message)
			}
			if len(message.Result) == 0 {
				return fmt.Errorf("MCP %s 缺 result", method)
			}
			return json.Unmarshal(message.Result, result)
		}
	}
}

func (c *mcpConnection) notify(method string, params any) error {
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(raw, '\n'))
	return err
}

func readMCPLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(out)+len(fragment) > limit {
			return nil, errors.New("MCP stdio 消息超过 4 MiB")
		}
		out = append(out, fragment...)
		if err == nil {
			return bytes.TrimSpace(out), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

type mcpCappedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *mcpCappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 8192 {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-8192:]...)
	}
	return len(p), nil
}

func (b *mcpCappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
