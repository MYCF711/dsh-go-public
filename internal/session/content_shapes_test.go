package session

import (
	"encoding/json"
	"testing"
)

// H7：`assistant/message` 的正文有两种落法，两种都要认。
//
//   - `{"message":{"content":[…]}}` —— 官方 v4 的写法
//   - `{"content":[…]}`           —— 顶层的写法（早期/别处产生的事件）
//
// 只认一种的表现很隐蔽：**历史里少了几条助手消息**，而这看起来像"那次模型没回答"。
func TestAssistantMessageBothContentShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"message.content", `{"message":{"content":[{"type":"text","text":"包装里的正文"}]}}`, "包装里的正文"},
		{"顶层 content", `{"content":[{"type":"text","text":"顶层的正文"}]}`, "顶层的正文"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := assistantMessage(json.RawMessage(c.raw))
			if !ok {
				t.Fatalf("%s：没解析出来", c.name)
			}
			if msg.Content != c.want {
				t.Fatalf("%s：正文是 %q，期望 %q", c.name, msg.Content, c.want)
			}
			if msg.Role != "assistant" {
				t.Fatalf("%s：role 是 %q", c.name, msg.Role)
			}
		})
	}
}

// H7：工具调用块也要跟着正文一起被认出来（否则历史里"它调了工具"就丢了）。
func TestAssistantMessageKeepsToolCalls(t *testing.T) {
	raw := `{"message":{"content":[
		{"type":"text","text":"我查一下"},
		{"type":"tool-call","id":"c1","name":"glob","arguments":"{\"pattern\":\"*.go\"}"}
	]}}`
	msg, ok := assistantMessage(json.RawMessage(raw))
	if !ok {
		t.Fatal("没解析出来")
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "glob" {
		t.Fatalf("工具调用丢了：%+v", msg.ToolCalls)
	}
}

// H7 的反面：**不许**用"有 user 就必须有 assistant"的恒强判据。
// 失败/取消/在途的轮次可以合法地只有 user，投影不该报错也不该造一条空的 assistant。
func TestUserOnlyTurnIsLegal(t *testing.T) {
	msg, ok := userMessage(json.RawMessage(`{"content":[{"type":"text","text":"提问"}]}`))
	if !ok || msg.Content != "提问" {
		t.Fatalf("user 消息解析失败：%+v", msg)
	}
}
