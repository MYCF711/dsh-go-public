package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// 没有图片时，序列化必须和以前**字节一致**——否则历史会话与快照全会漂移。
func TestMessageWithoutImagesUnchanged(t *testing.T) {
	m := Message{Role: "user", Content: "hi"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"role":"user","content":"hi"}` {
		t.Fatalf("无图消息的字节变了: %s", b)
	}
}

// 有图片时，content 变成内容块数组：一段文字 + 一张 data URL。
func TestMessageWithImageBecomesParts(t *testing.T) {
	m := Message{
		Role:    "user",
		Content: "看图",
		Images:  []MessageImage{{MIME: "image/png", DataBase64: "AAA"}},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("content 不是数组: %s", b)
	}
	if len(got.Content) != 2 {
		t.Fatalf("要两段: %s", b)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "看图" {
		t.Fatalf("第一段要是文字: %s", b)
	}
	if got.Content[1].Type != "image_url" || !strings.HasPrefix(got.Content[1].ImageURL.URL, "data:image/png;base64,AAA") {
		t.Fatalf("第二段要是 data URL: %s", b)
	}
}
