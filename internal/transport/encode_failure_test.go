package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// H8：序列化失败也是**一个错误出口**，必须和其它错误同形状（四字段）。
//
// 这条出口平时永远走不到——自家结构都能序列化。但"平时走不到"不等于可以例外：
// 客户端只有一个解析器，它遇到 code/message/hint/retryable 缺一个就得特判，
// 而特判的分支**没有人会为它写测试**。
func TestWriteJSONEncodeFailureHasFourFields(t *testing.T) {
	rec := httptest.NewRecorder()
	// chan 不能序列化，正好用来触发这条出口。
	writeJSON(rec, http.StatusOK, make(chan int))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("序列化失败要 500，拿到 %d", rec.Code)
	}
	var env struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Hint      string `json:"hint"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("兜底正文不是 JSON：%v（原文 %s）", err, rec.Body.String())
	}
	e := env.Error
	if e.Code != "encode_failure" {
		t.Fatalf("code 要 encode_failure，实际 %q", e.Code)
	}
	if e.Message == "" {
		t.Fatalf("message 不能空")
	}
	if e.Hint == "" {
		t.Fatalf("**hint 不能空**：客户端拿到 500 却不知道下一步做什么")
	}
	if e.Retryable {
		t.Fatalf("序列化失败重试也没用，retryable 应为 false")
	}
}

// H8：正常路径不能被这次改动弄坏——任何类型都要套 result 信封。
func TestWriteJSONEnvelopesAnyType(t *testing.T) {
	cases := map[string]any{
		"map":     map[string]any{"a": 1},
		"struct":  struct{ A int }{1},
		"slice":   []string{"x"},
		"pointer": &struct{ A int }{1},
	}
	for name, v := range cases {
		rec := httptest.NewRecorder()
		writeJSON(rec, http.StatusOK, v)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s：状态码应 200，实际 %d", name, rec.Code)
		}
		var env map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s：不是 JSON", name)
		}
		if _, ok := env["result"]; !ok {
			t.Fatalf("%s：没有 result 信封：%s", name, rec.Body.String())
		}
	}
}

// H8：兜底正文里的 hint 必须是**能照做的一句话**，不是空壳。
func TestEncodeFailureHintIsActionable(t *testing.T) {
	const body = `{"error":{"code":"encode_failure","message":"响应序列化失败","hint":"` // 形状示例
	_ = body
	hint := errorHint("encode_failure")
	if strings.TrimSpace(hint) == "" {
		t.Fatalf("errorHint(\"encode_failure\") 不能为空")
	}
	// 至少要告诉调用方"这是宿主内部的事，不是你参数写错了"。
	if !strings.Contains(hint, "重试") && !strings.Contains(hint, "宿主") && !strings.Contains(hint, "报告") {
		t.Fatalf("hint 太笼统，看不出下一步：%q", hint)
	}
}
