// Package llm 是 OpenAI 兼容的流式客户端。
//
// 只做一件事：把一条请求变成一串增量事件（reasoning / content / tool_call），
// 最后给出完整响应。它不认识会话、不认识工具执行——那是 agent 层的事。
package llm

import (
	"encoding/json"
	"fmt"
)

// Message 是对话消息。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"-"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	// Images 非空时，content 会序列化成 [{type:text},{type:image_url},…]。
	// OpenAI 兼容接口的 tool 消息不能带图片，所以带图的消息必须是 user 角色。
	Images []MessageImage `json:"-"`
	// Parts 非空时**完全接管** content：按切片顺序逐段输出，Content/Images 不再参与。
	// 用来保住 [text, image, text] 这种图片夹在文字中间的顺序——Content+Images 只能
	// 表达「先一整段文字，再一串图片」。为空（nil 或 len == 0）时行为与以前逐字节一致。
	Parts []MessagePart `json:"-"`
}

// MessageImage 是一张按 data URL 内联发送的图片。
type MessageImage struct {
	MIME       string
	DataBase64 string
}

// dataURL 是这张图片在 content 里的 data URL。
func (img MessageImage) dataURL() string {
	return "data:" + img.MIME + ";base64," + img.DataBase64
}

// MessagePart 是 content 数组里的一段，只有文字与内联图片两种。
// 零值不可用：Type 必须是 PartText 或 PartImageURL，建议用 TextPart / ImagePart 构造。
type MessagePart struct {
	Type string // PartText 或 PartImageURL
	Text string // Type == PartText 时的正文
	URL  string // Type == PartImageURL 时的 data URL（ImagePart 会拼好）
}

const (
	// PartText 对应 {"type":"text","text":…}。
	PartText = "text"
	// PartImageURL 对应 {"type":"image_url","image_url":{"url":…}}。
	PartImageURL = "image_url"
)

// TextPart 构造一段文字。
func TextPart(text string) MessagePart { return MessagePart{Type: PartText, Text: text} }

// ImagePart 构造一张内联图片段，data URL 的编码与 Images 字段完全一致。
func ImagePart(img MessageImage) MessagePart {
	return MessagePart{Type: PartImageURL, URL: img.dataURL()}
}

// MarshalJSON 手写：只输出该段自己的形状，字段顺序与旧实现逐字节一致。
// 未知类型直接报错——宁可发不出去，也不要把半个块发给模型。
func (p MessagePart) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case PartText:
		if p.URL != "" {
			return nil, fmt.Errorf("llm: text 段不能带 image_url")
		}
		type textPart struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		return json.Marshal(textPart{Type: PartText, Text: p.Text})
	case PartImageURL:
		if p.URL == "" {
			return nil, fmt.Errorf("llm: image_url 段缺 url")
		}
		type urlPart struct {
			Type     string `json:"type"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		var out urlPart
		out.Type = PartImageURL
		out.ImageURL.URL = p.URL
		return json.Marshal(out)
	default:
		return nil, fmt.Errorf("llm: 未知 content 段类型 %q（只支持 %s / %s）", p.Type, PartText, PartImageURL)
	}
}

// MarshalJSON 手写，三条路径，前两条是历史行为：
//
//  1. Parts 为空且没有图片：content 是普通字符串（**字节不变**，免得历史会话与
//     快照全部漂移）。
//  2. Parts 为空但有图片：content 是 [{type:text}?, {type:image_url}…]，文字在前、
//     图片按 Images 顺序跟在后面（同样字节不变）。
//  3. Parts 非空：content 完全按 Parts 顺序输出，Content/Images 不再参与。这样
//     [text, image, text] 这类图片夹在文字中间的输入，才不会像 W83 的
//     model-blocks 报文那样被压成「一大段文字 + 一串图片」，把图片位置丢掉。
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if len(m.Parts) > 0 {
		return json.Marshal(struct {
			plain
			Content []MessagePart `json:"content"`
		}{plain: plain(m), Content: m.Parts})
	}
	if len(m.Images) == 0 {
		return json.Marshal(struct {
			plain
			Content string `json:"content,omitempty"`
		}{plain: plain(m), Content: m.Content})
	}
	parts := make([]MessagePart, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, TextPart(m.Content))
	}
	for _, img := range m.Images {
		parts = append(parts, ImagePart(img))
	}
	return json.Marshal(struct {
		plain
		Content []MessagePart `json:"content"`
	}{plain: plain(m), Content: parts})
}

// ToolCall 是一次工具调用（arguments 是 **JSON 字符串**，与 DSH 会话格式一致）。
type ToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function Func   `json:"function"`
}

// Func 是工具名 + 参数。
type Func struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ToolDef 是暴露给模型的工具定义。
type ToolDef struct {
	Type     string      `json:"type"`
	Function ToolFuncDef `json:"function"`
}

// ToolFuncDef 是工具的名字、描述与参数 schema。
type ToolFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// StreamOptions 打开 usage 上报（OpenAI 兼容网关都认）。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Request 是一条 chat/completions 请求。
//
// model / messages / tools / stream 是标准字段，永远按原样发出；StreamOptions /
// MaxTokens / Reasoning 是可选字段，严格网关明确拒绝时客户端会做一次最小回退
// （见 compat.go）：去掉不被支持的 stream_options / reasoning_effort，或把
// max_tokens 换成 OpenAI 官方现行的 max_completion_tokens。默认形状逐字节不变。
type Request struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Tools         []ToolDef      `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Reasoning     string         `json:"reasoning_effort,omitempty"`
}

// Usage 是 token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response 是一次流式调用的最终结果。
type Response struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// EventKind 是增量事件的种类。
type EventKind string

const (
	EventReasoning EventKind = "reasoning"
	EventContent   EventKind = "content"
	EventToolCall  EventKind = "tool_call"
	EventDone      EventKind = "done"
)

// Event 是一条增量。
type Event struct {
	Kind         EventKind
	Text         string
	ToolCall     *ToolCall // Kind == EventToolCall 时非空（可能是碎片）
	FinishReason string    // Kind == EventDone 时非空
}
