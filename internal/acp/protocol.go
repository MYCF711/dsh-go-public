// Package acp 是 ACP（Agent Client Protocol）服务端：stdio JSON-RPC 2.0。
//
// 三条规矩（research/22 §5）：
//  1. ACP 是**语义投影**，不是事件镜像：只发 ACP 定义过的 update 变体；
//  2. stdout **只放协议**，任何调试输出都必须走 stderr；
//  3. 每会话同一时刻只有一个在飞的 prompt。
package acp

import "encoding/json"

// JSON-RPC 2.0 信封。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response 是应答。Result 与 Error 互斥。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification 是没有 id 的消息（服务端 → 客户端的 session/update 走这条）。
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError 是 JSON-RPC 错误。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// 标准错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// ACP 方法名（来自 @agentclientprotocol/sdk 的 methods.agent.*）。
const (
	MethodInitialize        = "initialize"
	MethodAuthenticate      = "authenticate"
	MethodSessionNew        = "session/new"
	MethodSessionList       = "session/list"
	MethodSessionResume     = "session/resume"
	MethodSessionClose      = "session/close"
	MethodSessionPrompt     = "session/prompt"
	MethodSessionCancel     = "session/cancel"
	MethodSessionSetConfig  = "session/set_config_option"
	MethodSessionUpdate     = "session/update"
	MethodRequestPermission = "session/request_permission"
	MethodCancelRequest     = "$/cancel_request"
)

// ---------- 请求/响应载荷 ----------

// InitializeRequest 见 initialize.request。
type InitializeRequest struct {
	ProtocolVersion    int            `json:"protocolVersion"`
	ClientCapabilities map[string]any `json:"clientCapabilities,omitempty"`
	ClientInfo         map[string]any `json:"clientInfo,omitempty"`
}

// AgentInfo 是 agentInfo 字段。
type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResponse 见 initialize.response。
type InitializeResponse struct {
	ProtocolVersion   int            `json:"protocolVersion"`
	AgentCapabilities map[string]any `json:"agentCapabilities,omitempty"`
	AgentInfo         *AgentInfo     `json:"agentInfo,omitempty"`
	AuthMethods       []any          `json:"authMethods,omitempty"`
}

// ContentBlock 是 prompt 与 update 里共用的内容块。
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// resource_link / image 的字段：M3 只透传，不解释。
	URI      string          `json:"uri,omitempty"`
	Name     string          `json:"name,omitempty"`
	MimeType string          `json:"mimeType,omitempty"`
	Data     string          `json:"data,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// SessionNewRequest / Response。
type SessionNewRequest struct {
	Cwd                   string           `json:"cwd"`
	McpServers            []map[string]any `json:"mcpServers"`
	AdditionalDirectories []string         `json:"additionalDirectories,omitempty"`
	// Mode 可选：建会话时就定模式。
	// 不给的话客户端得"先建再 set_mode"，中间存在一个全权限窗口。
	Mode string `json:"mode,omitempty"`
}

// ConfigOption 是 configOptions 的一行。
type ConfigOption struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	CurrentValue string `json:"currentValue,omitempty"`
}

// SessionNewResponse。
type SessionNewResponse struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}

// SessionPromptRequest / Response。
type SessionPromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// SessionPromptResponse 的 stopReason 取值（ACP 1.4）。
type SessionPromptResponse struct {
	StopReason string         `json:"stopReason"`
	Usage      map[string]any `json:"usage,omitempty"`
}

// SessionCancelParams 是通知载荷。
type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// SessionUpdateParams 是唯一的流式通知出口。
type SessionUpdateParams struct {
	SessionID string         `json:"sessionId"`
	Update    map[string]any `json:"update"`
}
