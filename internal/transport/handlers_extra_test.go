package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errBoom = errors.New("磁盘满了")

func newTestServer(t *testing.T, o Options) *Server {
	t.Helper()
	s, err := NewServer(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func postJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	// 走真实的鉴权入口会需要 token/cookie；这里只测 handler 本身的分支，
	// 鉴权由 server_test.go 的三条测试单独覆盖。
	rec := httptest.NewRecorder()
	s.mu.Lock()
	s.cookies["test-cookie"] = struct{}{}
	s.mu.Unlock()
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "test-cookie"})
	s.handleAPI(rec, req)
	return rec
}

// 删会话：找不到是 404（换个 id 重试），删失败是 500（提示没删掉）——
// 这两种在界面上长得一样，混在一起用户没法判断该不该重试。
func TestDeleteDistinguishesNotFoundFromFailure(t *testing.T) {
	s := newTestServer(t, Options{
		Delete: func(id string) error {
			if id == "missing" {
				return ErrNoSuchSession
			}
			return errBoom
		},
	})
	if rec := postJSON(t, s, "/api/session.delete", `{"sessionId":"missing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的会话要 404，拿到 %d", rec.Code)
	}
	if rec := postJSON(t, s, "/api/session.delete", `{"sessionId":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("删失败要 500，拿到 %d", rec.Code)
	}
}

func TestDeleteRejectsEmptyID(t *testing.T) {
	s := newTestServer(t, Options{Delete: func(string) error { t.Fatal("不该被调用"); return nil }})
	if rec := postJSON(t, s, "/api/session.delete", `{"sessionId":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 id 要 400，拿到 %d", rec.Code)
	}
}

// fork 成功要回新 id——界面拿它直接跳转，回空等于让用户手动猜。
func TestForkReturnsNewSessionID(t *testing.T) {
	s := newTestServer(t, Options{Fork: func(id, title string) (string, error) {
		if id != "src" {
			t.Fatalf("源会话 id 没传对：%s", id)
		}
		return "session-new", nil
	}})
	rec := postJSON(t, s, "/api/session.fork", `{"sessionId":"src","title":"分支"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d", rec.Code)
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["sessionId"] != "session-new" {
		t.Fatalf("没回新会话 id：%v", got)
	}
}

// 没接线时要 501 而不是 500：501 说的是"这个能力还没有"，500 说的是"它坏了"。
func TestUnwiredMethodsReturn501(t *testing.T) {
	s := newTestServer(t, Options{})
	for _, path := range []string{"/api/session.delete", "/api/session.fork"} {
		if rec := postJSON(t, s, path, `{"sessionId":"a"}`); rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s 未接线要 501，拿到 %d", path, rec.Code)
		}
	}
}

// total 未知时必须报 truncated=true：另一种选择会让界面以为已经到底，
// 而用户看到的是一条被截断的历史——这类错误不报错，只会让人找不到东西。
func TestLoadWithUnknownTotalIsTruncated(t *testing.T) {
	src := &fakeTailSrc{msgs: []Message{{Seq: 1, Role: "user", Text: "hi"}}, total: -1}
	got, err := LoadSession(src, "s-1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != -1 || !got.Truncated {
		t.Fatalf("total=-1 时 truncated 必须是 true，实际 total=%d truncated=%v", got.Total, got.Truncated)
	}
}

type fakeTailSrc struct {
	msgs  []Message
	total int
}

func (f *fakeTailSrc) Messages(string) ([]Message, error) { return f.msgs, nil }
func (f *fakeTailSrc) MessagesTail(string, int) ([]Message, int, error) {
	return f.msgs, f.total, nil
}

// unwrapResult 剥掉 H8 的响应信封（成功 {"result":…}）。
// 断言应当贴着**业务载荷**写，信封只在这里剥一次，别在每个测试里各写一遍。
func unwrapResult(t interface{ Helper() }, raw []byte) map[string]any {
	var env struct {
		Result map[string]any `json:"result"`
	}
	_ = json.Unmarshal(raw, &env)
	return env.Result
}

// delete / fork 认 `id` 这个别名。
//
// 界面审计实测：界面那边两种写法都有，宿主只认 sessionId 时回 400，
// 而调用方以为自己传对了——它不会去读 400 的 body。
func TestDeleteAndForkAcceptIDAlias(t *testing.T) {
	var gotDelete string
	s := newTestServer(t, Options{
		Delete: func(sid string) error { gotDelete = sid; return nil },
		Fork:   func(sid, title string) (string, error) { return "new-session", nil },
	})

	if rec := postJSON(t, s, "/api/session.delete", `{"id":"s-1"}`); rec.Code != 200 {
		t.Fatalf("delete 用 id 应当 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if gotDelete != "s-1" {
		t.Fatalf("delete 没拿到 id：%q", gotDelete)
	}
	if rec := postJSON(t, s, "/api/session.fork", `{"id":"s-1","title":"分支"}`); rec.Code != 200 {
		t.Fatalf("fork 用 id 应当 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// 409 stale_epoch 也必须是**四字段信封**。
//
// 少 retryable 的后果：通用客户端解不出这个字段，而这条恰恰是
// "必须让用户知道可以重试"的那一类错误（界面审计实测）。
func TestStaleEpochEnvelopeIsComplete(t *testing.T) {
	// 要有 journal 才会走到 409 那条分支；没有 journals 是 501。
	js := NewJournals(func() int64 { return 7 })
	js.Get("s-1") // 先建出这个会话的 journal（epoch=7）
	s := newTestServer(t, Options{Journals: js})
	rec := postJSON(t, s, "/api/session.history", `{"sessionId":"s-1","sinceSeq":99,"journalEpoch":1}`)
	if rec.Code != 409 {
		t.Fatalf("应当 409，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Hint      string `json:"hint"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "stale_epoch" {
		t.Fatalf("code 不对：%q", env.Error.Code)
	}
	if env.Error.Retryable == nil {
		t.Fatal("缺 retryable —— 客户端没法判断该不该重试")
	}
	if env.Error.Hint == "" {
		t.Fatal("缺 hint")
	}
}
