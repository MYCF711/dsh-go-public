package transport

import (
	"encoding/json"
	"testing"
)

// "上一轮还没结束"必须是**可读的**错误：有意义的 code、非空 hint、retryable=true。
//
// 真机实测过旧行为：code="error"、hint=""、retryable=false —— 三样都错。
// 界面拿不到任何下一步，只能弹一个"失败了"，而其实等一下再发就行。
func TestBusyErrorIsActionable(t *testing.T) {
	body := errorBody("上一轮还没结束")
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Error struct {
			Code      string `json:"code"`
			Hint      string `json:"hint"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "busy" {
		t.Fatalf("code 应当是 busy，实际 %q", env.Error.Code)
	}
	if env.Error.Hint == "" {
		t.Fatal("hint 不能为空：每个错误都要给出下一步")
	}
	if !env.Error.Retryable {
		t.Fatal("busy 应当是可重试的：等这一轮结束再发")
	}
}
