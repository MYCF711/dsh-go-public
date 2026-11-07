package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// H6：思考块必须能**原样穿过** layout —— 写进去什么样，读出来什么样。
//
// 这条不依赖模型：模型给不给 reasoning 是它的事，宿主有没有这条路是另一件事。
// 之前这两件事被混成一个问题，结论就成了"宿主可能没实现"，而实际是
// **宿主压根没把 reasoning 写进日志**（2026-11-03 实测：assistant/message 里没有该块）。
func TestReasoningBlockSurvivesEvent(t *testing.T) {
	blocks := []map[string]any{
		ReasoningBlock("先看看目录里有什么"),
		TextBlock("一共 47 个文件"),
	}
	ev := AssistantMessageEvent(1, 1, blocks, "stop", nil)

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"type":"reasoning"`) {
		t.Fatalf("事件里没有 reasoning 块：%s", s)
	}
	if !strings.Contains(s, "先看看目录里有什么") {
		t.Fatalf("思考正文丢了：%s", s)
	}

	// 顺序也要对：思考在正文之前，折叠出来才是"先想再答"。
	// 注意要比 `"type":` 那一段——块里还有个键叫 text，直接找 `"text"` 会先命中
	// reasoning 块自己的正文（我第一版就这么写错的）。
	if strings.Index(s, `"type":"reasoning"`) > strings.Index(s, `"type":"text"`) {
		t.Fatalf("reasoning 应排在 text 之前：%s", s)
	}
}
