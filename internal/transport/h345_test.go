package transport

import (
	"encoding/json"
	"net/http"
	"testing"
)

// fakeSrc 是一个可控的 Source：想看哪条字段就让它返回什么。
type fakeSrc struct {
	msgs  []Message
	model string
}

func (f fakeSrc) Messages(string) ([]Message, error) { return f.msgs, nil }

// modelSrc 带 model：用独立字段，避免和外层 fakeSrc 的字段名打架。
type modelSrc struct {
	src   fakeSrc
	model string
}

func (m modelSrc) Messages(id string) ([]Message, error) { return m.src.Messages(id) }
func (m modelSrc) Model(string) (string, error)          { return m.model, nil }

// H5：load 返回的是**这一进程真实的 policy**，不是 mode、不是 preset。
//
// 判据写死成四种取值：界面只认这四个词，多一个就得猜。
func TestLoadCarriesProcessPolicy(t *testing.T) {
	for _, p := range []string{"deny", "ask", "allow", "auto"} {
		s := newTestServer(t, Options{Policy: p, Load: emptyLoad})
		rec := postJSON(t, s, "/api/session.load", `{"id":"s1"}`)
		var env struct {
			Result struct {
				Permission string `json:"permission"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s：响应不是 JSON", p)
		}
		if env.Result.Permission != p {
			t.Fatalf("该报 %q，实际 %q", p, env.Result.Permission)
		}
	}
}

// H3：`messages[].view` 要原样透传；没有 view 时字段整个省略（不是空对象）。
//
// 空对象会让界面以为"有 view 但是空的"，于是去渲染一个空卡片。
func TestLoadPassesViewThrough(t *testing.T) {
	src := fakeSrc{msgs: []Message{
		{Seq: 1, Role: "tool", Text: "有视图", View: json.RawMessage(`{"kind":"table"}`)},
		{Seq: 2, Role: "assistant", Text: "没有视图"},
	}}
	s := newTestServer(t, Options{
		Load: func(string, int) (LoadResult, error) {
			return LoadSession(src, "s1", 10)
		},
	})
	rec := postJSON(t, s, "/api/session.load", `{"id":"s1"}`)

	var env struct {
		Result struct {
			Messages []struct {
				View *json.RawMessage `json:"view"`
			} `json:"messages"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if len(env.Result.Messages) != 2 {
		t.Fatalf("应有 2 条消息，实际 %d", len(env.Result.Messages))
	}
	if env.Result.Messages[0].View == nil {
		t.Fatalf("第一条的 view 丢了")
	}
	if env.Result.Messages[1].View != nil {
		t.Fatalf("没有 view 的那条不该出现该字段")
	}
}

// H4：model 来自宿主，不认识就省略——**不许拿默认值冒充**。
func TestLoadCarriesModelOrOmitsIt(t *testing.T) {
	withModel := modelSrc{model: "group/deepseek-v4.1-flash-1017"}
	s := newTestServer(t, Options{
		Load: func(string, int) (LoadResult, error) { return LoadSession(withModel, "s1", 10) },
	})
	if body := postJSON(t, s, "/api/session.load", `{"id":"s1"}`).Body.String(); !stringsContains(body, "deepseek") {
		t.Fatalf("model 没带上：%s", body)
	}

	// 没有 model 的 Source：字段必须不出现。
	s2 := newTestServer(t, Options{
		Load: func(string, int) (LoadResult, error) { return LoadSession(fakeSrc{}, "s1", 10) },
	})
	if body := postJSON(t, s2, "/api/session.load", `{"id":"s1"}`).Body.String(); stringsContains(body, `"model"`) {
		t.Fatalf("不知道 model 时不该编一个：%s", body)
	}
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// W187 / TOOL-IDENTITY-LOAD：`session.load` 的 tool 结果带真实 callId/turn/step。
//
// 这是**线上契约**测试：TUI 的 `dshGoLoadToolResult` 只认 `callId` 非空且
// `turn`/`step` 是有限数字，三者缺一就整条丢弃；所以这里按响应 JSON 判，
// 而不是按结构体字段判。
//
// 同时钉住反向规矩：没有真值的消息**不许**出现这三个键（0 也是编的），
// user/assistant 也不带——它们本来就没有工具身份。
func TestLoadPassesToolIdentityThrough(t *testing.T) {
	turn, step := 2, 3
	src := fakeSrc{msgs: []Message{
		{
			Seq: 1, Role: "user", Text: "看看目录",
		},
		{
			Seq: 2, Role: "tool", Text: "68 个文件", Tool: "bash", Args: `{"command":"ls"}`,
			CallID: "call_w187", Turn: &turn, Step: &step,
			View: json.RawMessage(`{"kind":"card","title":"目录"}`),
		},
		// 没有身份的老结果：正文照常出现，三个身份键必须整个省略。
		{Seq: 3, Role: "tool", Text: "没有身份的老结果"},
		{Seq: 4, Role: "assistant", Text: "目录里 68 个文件"},
	}}
	s := newTestServer(t, Options{
		Load: func(string, int) (LoadResult, error) { return LoadSession(src, "s1", 10) },
	})
	body := postJSON(t, s, "/api/session.load", `{"id":"s1"}`).Body.Bytes()

	var env struct {
		Result struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, body)
	}
	if len(env.Result.Messages) != 4 {
		t.Fatalf("应有 4 条消息，实际 %d：%s", len(env.Result.Messages), body)
	}

	withIdentity := env.Result.Messages[1]
	var gotCallID string
	if err := json.Unmarshal(withIdentity["callId"], &gotCallID); err != nil || gotCallID != "call_w187" {
		t.Fatalf("callId 必须原样透传：%s（整条：%v）", withIdentity["callId"], withIdentity)
	}
	for key, want := range map[string]float64{"turn": 2, "step": 3} {
		var got float64
		if err := json.Unmarshal(withIdentity[key], &got); err != nil {
			t.Fatalf("%s 必须是 JSON 数字（TUI 的 position() 只认 number）：%s", key, withIdentity[key])
		}
		if got != want {
			t.Fatalf("%s 必须是真实值 %v，实际 %v", key, want, got)
		}
	}
	var gotTool, gotText string
	_ = json.Unmarshal(withIdentity["tool"], &gotTool)
	_ = json.Unmarshal(withIdentity["text"], &gotText)
	if gotTool != "bash" || gotText != "68 个文件" || len(withIdentity["view"]) == 0 {
		t.Fatalf("工具名/结果/View 少了一个：%v", withIdentity)
	}

	identityKeys := []string{"callId", "turn", "step"}
	for _, idx := range []int{0, 2, 3} {
		for _, key := range identityKeys {
			if _, exists := env.Result.Messages[idx][key]; exists {
				t.Fatalf("第 %d 条（role=%s）不该出现 %q：没有真值就不许写，0 也是编的",
					idx, env.Result.Messages[idx]["role"], key)
			}
		}
	}
}

// TOOL-RESULT-ENVELOPE（W190）：HTTP 响应里 tool 消息必须带**真实**的
// isError/meta/structured，user/assistant 不带；没有真值的那条工具结果
// 三个键**整个省略**（不写 false、不写空对象、不写 null）。
//
// 为什么在 HTTP 层再钉一遍：TUI 读的是响应 JSON，不是 Go 结构体。
// 结构体上有字段而线上被 omitempty 吃掉、或者反过来把 false 写出来，
// 都只有这一层看得见。
func TestLoadPassesToolResultEnvelopeThrough(t *testing.T) {
	turn, step := 2, 3
	src := fakeSrc{msgs: []Message{
		{Seq: 1, Role: "user", Text: "跑一下"},
		{
			Seq: 2, Role: "tool", Text: "exit status 1", Tool: "bash", Args: `{"command":"false"}`,
			CallID: "call_w190_fail", Turn: &turn, Step: &step,
			IsError:    true,
			Meta:       json.RawMessage(`{"cwd":"D:\\work"}`),
			Structured: json.RawMessage(`{"exitCode":2}`),
		},
		// 成功的结果：没有 meta/structured，isError 也不该出现。
		{Seq: 3, Role: "tool", Text: "成功但没元数据", CallID: "call_w190_ok", Turn: &turn, Step: &step},
		{Seq: 4, Role: "assistant", Text: "退出码 2"},
	}}
	s := newTestServer(t, Options{
		Load: func(string, int) (LoadResult, error) { return LoadSession(src, "s1", 10) },
	})
	body := postJSON(t, s, "/api/session.load", `{"id":"s1"}`).Body.Bytes()

	var env struct {
		Result struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, body)
	}
	if len(env.Result.Messages) != 4 {
		t.Fatalf("应有 4 条消息，实际 %d：%s", len(env.Result.Messages), body)
	}

	// ① 失败结果：成败与两段 durable 值原样上线。
	failed := env.Result.Messages[1]
	var gotIsError bool
	if err := json.Unmarshal(failed["isError"], &gotIsError); err != nil || !gotIsError {
		t.Fatalf("isError 必须是 true，实际 %s（err=%v）；失败结果会被画成成功", failed["isError"], err)
	}
	var meta struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(failed["meta"], &meta); err != nil || meta.Cwd != `D:\work` {
		t.Fatalf("meta 必须原样透传 durable 值，实际 %s（err=%v）", failed["meta"], err)
	}
	var structured struct {
		ExitCode int `json:"exitCode"`
	}
	if err := json.Unmarshal(failed["structured"], &structured); err != nil || structured.ExitCode != 2 {
		t.Fatalf("structured 必须原样透传，实际 %s（err=%v）", failed["structured"], err)
	}

	// ② 没有真值的三条（含那条成功的工具结果）：三个键一个都不许出现。
	envelope := []string{"isError", "meta", "structured"}
	for _, idx := range []int{0, 2, 3} {
		for _, key := range envelope {
			if got, exists := env.Result.Messages[idx][key]; exists {
				t.Fatalf("第 %d 条（role=%s）不该出现 %q（%s）：没有真值就不许写，false/null 也是编",
					idx, env.Result.Messages[idx]["role"], key, got)
			}
		}
	}
}

var _ = http.StatusOK
