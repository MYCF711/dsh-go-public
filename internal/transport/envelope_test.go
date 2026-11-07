package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// H8：**任何类型**都要套信封，不只 map。
// 之前只包 map，struct / 切片裸返回，照契约读 result 的客户端拿到空。
func TestEnvelopeWrapsStructAndSlice(t *testing.T) {
	s := newTestServer(t, Options{
		List: func() ([]SessionSummary, error) {
			return []SessionSummary{{ID: "a", Title: "t"}}, nil
		},
		Load: func(id string, limit int) (LoadResult, error) {
			return LoadResult{Total: 1}, nil
		},
	})
	cases := []struct {
		name, path, body string
	}{
		{"list（切片装在 struct 里）", "/api/session.list", `{}`},
		{"session.load（struct）", "/api/session.load", `{"sessionId":"a"}`},
	}
	for _, c := range cases {
		rec := postJSON(t, s, c.path, c.body)
		var env map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s：响应不是 JSON：%v", c.name, err)
		}
		if _, ok := env["result"]; !ok {
			t.Fatalf("%s：没有 result 键，实际 %s", c.name, rec.Body.String())
		}
	}
}

// 成功响应不许是 nil。
//
// `{"result":null}` 是"接口成功、但没有载荷"：照契约读 result 的客户端会拿到
// 一个 null 对象/数组，多数语言里 for 一下就是空——界面表现为"什么都没有"，
// 而 HTTP 200 让所有重试/告警逻辑都认为这是正常结果。
func TestWriteJSONRejectsNilResult(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("nil 载荷不该回 200：%s", rec.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误响应不是 JSON：%v（%s）", err, rec.Body.String())
	}
	if env.Error.Code == "" || env.Error.Message == "" || env.Error.Hint == "" {
		t.Fatalf("错误信封要四字段：%s", rec.Body.String())
	}

	// 类型化的 nil（例如 (*LoadResult)(nil)）也要拦：它会序列化成 {"result":null}。
	rec = httptest.NewRecorder()
	var typedNil *LoadResult
	writeJSON(rec, http.StatusOK, typedNil)
	if rec.Code == http.StatusOK {
		t.Fatalf("typed-nil 载荷不该回 200：%s", rec.Body.String())
	}
}

// H8：错误信封必须带 code/message/hint/retryable 四个键。
func TestErrorEnvelopeHasHint(t *testing.T) {
	s := newTestServer(t, Options{})
	rec := postJSON(t, s, "/api/session.load", `{"sessionId":"nope"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("不存在的会话不该 200")
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
		t.Fatalf("错误响应不是 JSON：%v", err)
	}
	if env.Error.Code == "" || env.Error.Message == "" {
		t.Fatalf("错误信封缺 code/message：%s", rec.Body.String())
	}
	if env.Error.Hint == "" {
		t.Fatalf("错误信封缺 hint（客户端要靠它知道下一步）：%s", rec.Body.String())
	}
}

// H9：只发契约快照里的名字（id / tailMessages）也必须生效。
func TestLoadAcceptsContractNames(t *testing.T) {
	var gotID string
	var gotLimit int
	s := newTestServer(t, Options{
		Load: func(id string, limit int) (LoadResult, error) {
			gotID, gotLimit = id, limit
			return LoadResult{Total: 1}, nil
		},
	})
	rec := postJSON(t, s, "/api/session.load", `{"id":"abc","tailMessages":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if gotID != "abc" {
		t.Fatalf("id 没生效：%q", gotID)
	}
	if gotLimit != 2 {
		t.Fatalf("tailMessages 没生效：limit=%d", gotLimit)
	}
}

type usageFake struct {
	total UsageMsg
	ctx   int
}

func (f usageFake) Messages(string) ([]Message, error)       { return nil, nil }
func (f usageFake) UsageTotal(string) (UsageMsg, int, error) { return f.total, f.ctx, nil }

// H1：全会话合计与当前上下文占用是两个数；**tail 切页不许改变合计**。
func TestLoadSessionUsageTotalIndependentOfTail(t *testing.T) {
	src := usageFake{
		total: UsageMsg{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
		ctx:   4321,
	}
	small, err := LoadSession(src, "s", 1)
	if err != nil {
		t.Fatal(err)
	}
	big, err := LoadSession(src, "s", 50)
	if err != nil {
		t.Fatal(err)
	}
	if small.UsageTotal == nil || big.UsageTotal == nil {
		t.Fatalf("两页都该有 usageTotal")
	}
	if *small.UsageTotal != *big.UsageTotal {
		t.Fatalf("切页改变了合计：%+v vs %+v", *small.UsageTotal, *big.UsageTotal)
	}
	if small.UsageTotal.InputTokens != 100 || small.UsageTotal.OutputTokens != 20 {
		t.Fatalf("合计不对：%+v", *small.UsageTotal)
	}
	if small.ContextTokens != 4321 {
		t.Fatalf("上下文占用不对：%d", small.ContextTokens)
	}
}
