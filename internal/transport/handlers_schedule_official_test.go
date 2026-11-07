package transport

// 官方桌面端 0.2.2 的 schedule.create / edit / view / run / history 契约测试。
//
// 这些测试只打本包的 `serveOfficialScheduleAPI`（Lead 接线后由 /api 路由调它），
// 因此不依赖 server.go 的分发表——**路由接线是 Lead 的验收项**，测试里
// 用 IsOfficialScheduleMethod 钉住"方法名集合"，避免新增方法从路由漏下去。
//
// 与既有 `schedule.list/delete/clear` 的关系：那三个方法名不进本文件的方法集，
// 也不改任何既有 handler；本文件只覆盖新增的 5 个官方方法。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// scheduleOfficialServer 造一个挂了 schedule 实现的 Server。
func scheduleOfficialServer(t *testing.T, api OfficialScheduleAPI) *Server {
	t.Helper()
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.AttachOfficialScheduleAPI(api)
	t.Cleanup(srv.DetachOfficialScheduleAPI)
	return srv
}

// callScheduleOfficial 打一次方法，返回 HTTP 状态与解码后的信封。
func callScheduleOfficial(t *testing.T, srv *Server, method string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case nil:
		raw = nil
	case string:
		raw = []byte(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		raw = encoded
	}
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.serveOfficialScheduleAPI(rec, req, method)
	var envelope map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
	}
	// 成功信封与既有 /api 一致：`{"result": …}`；错误信封是四字段 `{"error": …}`。
	// 这里把成功的 result 拆出来，调用点只关心官方对象本身。
	if rec.Code < 400 {
		payload, ok := envelope["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s 的成功响应没有 result 对象: %q", method, rec.Body.String())
		}
		return rec.Code, payload
	}
	return rec.Code, envelope
}

// scheduleErrorObject 取出四字段错误体；不是错误体就 Fatal。
func scheduleErrorObject(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	body, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应没有 error 对象: %#v", out)
	}
	for _, key := range []string{"code", "message", "hint", "retryable"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("错误体缺 %q（四字段契约）: %#v", key, body)
		}
	}
	return body
}

func scheduleErrorCode(t *testing.T, out map[string]any) string {
	t.Helper()
	code, _ := scheduleErrorObject(t, out)["code"].(string)
	return code
}

func scheduleErrorMessage(t *testing.T, out map[string]any) string {
	t.Helper()
	msg, _ := scheduleErrorObject(t, out)["message"].(string)
	return msg
}

// ---------------------------------------------------------------------------
// 方法集合与接线
// ---------------------------------------------------------------------------

func TestOfficialScheduleMethodsAreTheFiveOfficialOnes(t *testing.T) {
	want := []Method{
		MethodScheduleCreate, MethodScheduleEdit, MethodScheduleView,
		MethodScheduleRun, MethodScheduleHistory,
	}
	wantNames := []string{
		"schedule.create", "schedule.edit", "schedule.view",
		"schedule.run", "schedule.history",
	}
	got := OfficialScheduleMethods()
	if len(got) != len(want) {
		t.Fatalf("官方 schedule 方法数 = %d，want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("方法顺序[%d] = %q，want %q（顺序固定，快照按它排）", i, got[i], want[i])
		}
		if string(got[i]) != wantNames[i] {
			t.Fatalf("方法名[%d] = %q，want %q", i, got[i], wantNames[i])
		}
	}
}

func TestOfficialSchedulePredicateClaimsOnlyTheFive(t *testing.T) {
	for _, name := range []string{"schedule.create", "schedule.edit", "schedule.view", "schedule.run", "schedule.history"} {
		if !IsOfficialScheduleMethod(name) {
			t.Fatalf("IsOfficialScheduleMethod(%q) = false，want true", name)
		}
	}
	// 既有宿主方法必须认领不到：它们走 server.go 自己的 case，不能被本层吞掉。
	for _, name := range []string{
		"schedule.list", "schedule.delete", "schedule.clear",
		"schedule", "schedule.", "schedule.create ", "Schedules.create",
		"schedule.catalog", "schedule.deleteInactive", "schedule.deliveryLimits",
	} {
		if IsOfficialScheduleMethod(name) {
			t.Fatalf("IsOfficialScheduleMethod(%q) = true，但它不属于本文件实现的 5 个方法", name)
		}
	}
}

func TestOfficialScheduleUnwiredIs501WithFourFields(t *testing.T) {
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	code, out := callScheduleOfficial(t, srv, "schedule.create", `{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60}}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("未接线时状态 = %d，want 501", code)
	}
	if got := scheduleErrorCode(t, out); got != "not_implemented" {
		t.Fatalf("未接线时 code = %q，want not_implemented", got)
	}
	if msg := scheduleErrorMessage(t, out); !strings.Contains(msg, "AttachOfficialScheduleAPI") {
		t.Fatalf("未接线时的消息必须说清缺哪一步，got %q", msg)
	}
}

func TestOfficialScheduleMountIsolatesServers(t *testing.T) {
	a := scheduleOfficialServer(t, OfficialScheduleAPI{
		View: func(id string) (any, error) {
			return OfficialScheduleEntry{ID: id, Kind: "every", Title: "A", Prompt: "a", EverySeconds: 60,
				ScheduledAt: "2026-11-07T00:00:00Z", Status: "active",
				Target: OfficialScheduleTarget{Kind: "session", SessionID: "sA"}}, nil
		},
	})
	b := scheduleOfficialServer(t, OfficialScheduleAPI{
		View: func(id string) (any, error) {
			return OfficialScheduleEntry{ID: id, Kind: "every", Title: "B", Prompt: "b", EverySeconds: 60,
				ScheduledAt: "2026-11-07T00:00:00Z", Status: "active",
				Target: OfficialScheduleTarget{Kind: "session", SessionID: "sB"}}, nil
		},
	})
	_, outA := callScheduleOfficial(t, a, "schedule.view", `{"id":"x"}`)
	_, outB := callScheduleOfficial(t, b, "schedule.view", `{"id":"x"}`)
	if got, _ := outA["title"].(string); got != "A" {
		t.Fatalf("Server A 的 view 落到 %q，want A（挂载表必须按 *Server 隔离）", got)
	}
	if got, _ := outB["title"].(string); got != "B" {
		t.Fatalf("Server B 的 view 落到 %q，want B", got)
	}
	// Detach 之后回到未接线：条目是按 *Server 索引的，不留进程级单例。
	b.DetachOfficialScheduleAPI()
	code, out := callScheduleOfficial(t, b, "schedule.view", `{"id":"x"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("Detach 后状态 = %d，want 501（实现已被摘掉）", code)
	}
	_ = scheduleErrorObject(t, out)
}

// ---------------------------------------------------------------------------
// schedule.create
// ---------------------------------------------------------------------------

func TestOfficialScheduleCreatePassesTypedRequest(t *testing.T) {
	var got OfficialScheduleCreateRequest
	calls := 0
	srv := scheduleOfficialServer(t, OfficialScheduleAPI{
		Create: func(req OfficialScheduleCreateRequest) (OfficialScheduleEntry, error) {
			calls++
			got = req
			return OfficialScheduleEntry{
				ID: "sch-1", Kind: "every", Title: req.Title, Prompt: req.Prompt,
				EverySeconds: req.Timing.EverySeconds, ScheduledAt: "2026-11-07T01:00:00Z",
				Status: "active", Target: *req.Target,
			}, nil
		},
	})
	code, out := callScheduleOfficial(t, srv, "schedule.create", `{
		"title":"  喝水  ",
		"prompt":"  起来喝水  ",
		"timing":{"kind":"every","every_seconds":300},
		"target":{"kind":"session","sessionId":"s1"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("create 状态 = %d，want 200: %#v", code, out)
	}
	if calls != 1 {
		t.Fatalf("宿主 Create 被调 %d 次，want 1", calls)
	}
	if got.Title != "喝水" || got.Prompt != "起来喝水" {
		t.Fatalf("title/prompt 必须已 trim：got %q / %q", got.Title, got.Prompt)
	}
	if got.Timing.Kind != "every" || got.Timing.EverySeconds != 300 {
		t.Fatalf("timing 解析错: %#v", got.Timing)
	}
	if got.Target == nil || got.Target.Kind != "session" || got.Target.SessionID != "s1" {
		t.Fatalf("target 解析错: %#v", got.Target)
	}
	if out["kind"] != "every" || out["id"] != "sch-1" {
		t.Fatalf("结果形状错: %#v", out)
	}
}

func TestOfficialScheduleCreateTimingVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want func(t *testing.T, req OfficialScheduleCreateRequest)
	}{
		{
			name: "after",
			body: `{"title":"t","prompt":"p","timing":{"kind":"after","after_seconds":900}}`,
			want: func(t *testing.T, req OfficialScheduleCreateRequest) {
				if req.Timing.AfterSeconds != 900 {
					t.Fatalf("after_seconds = %d，want 900", req.Timing.AfterSeconds)
				}
			},
		},
		{
			name: "at-string",
			body: `{"title":"t","prompt":"p","timing":{"kind":"at","at":"2026-11-08T09:30:00Z"}}`,
			want: func(t *testing.T, req OfficialScheduleCreateRequest) {
				if req.Timing.At == nil || req.Timing.At.Instant != "2026-11-08T09:30:00Z" {
					t.Fatalf("at 字符串形式解析错: %#v", req.Timing.At)
				}
			},
		},
		{
			name: "at-object",
			body: `{"title":"t","prompt":"p","timing":{"kind":"at","at":{"date":"2026-11-08","time":"09:30:00","time_zone":"Asia/Shanghai"}}}`,
			want: func(t *testing.T, req OfficialScheduleCreateRequest) {
				if req.Timing.At == nil || req.Timing.At.Date != "2026-11-08" ||
					req.Timing.At.Clock != "09:30:00" || req.Timing.At.TimeZone != "Asia/Shanghai" {
					t.Fatalf("at 对象形式解析错: %#v", req.Timing.At)
				}
			},
		},
		{
			name: "daily",
			body: `{"title":"t","prompt":"p","timing":{"kind":"daily","daily":{"time":"09:30:00","time_zone":"UTC"}}}`,
			want: func(t *testing.T, req OfficialScheduleCreateRequest) {
				if req.Timing.Daily == nil || req.Timing.Daily.Time != "09:30:00" || req.Timing.Daily.TimeZone != "UTC" {
					t.Fatalf("daily 解析错: %#v", req.Timing.Daily)
				}
			},
		},
		{
			name: "weekly",
			body: `{"title":"t","prompt":"p","timing":{"kind":"weekly","weekly":{"time":"08:00","time_zone":"UTC","weekdays":[1,3,5]}}}`,
			want: func(t *testing.T, req OfficialScheduleCreateRequest) {
				if req.Timing.Weekly == nil || len(req.Timing.Weekly.Weekdays) != 3 ||
					req.Timing.Weekly.Weekdays[0] != 1 || req.Timing.Weekly.Weekdays[2] != 5 {
					t.Fatalf("weekly 解析错: %#v", req.Timing.Weekly)
				}
			},
		},
		{
			name: "cron",
			body: `{"title":"t","prompt":"p","timing":{"kind":"cron","cron":{"expression":"*/5 * * * *","time_zone":"UTC"}}}`,
			want: func(t *testing.T, req OfficialScheduleCreateRequest) {
				if req.Timing.Cron == nil || req.Timing.Cron.Expression != "*/5 * * * *" {
					t.Fatalf("cron 解析错: %#v", req.Timing.Cron)
				}
			},
		},
		{
			name: "rrule",
			body: `{"title":"t","prompt":"p","timing":{"kind":"rrule","rrule":"DTSTART:20261108T010000Z\nRRULE:FREQ=DAILY"}}`,
			want: func(t *testing.T, req OfficialScheduleCreateRequest) {
				if req.Timing.RRule == "" {
					t.Fatalf("rrule 没解析到: %#v", req.Timing)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured OfficialScheduleCreateRequest
			srv := scheduleOfficialServer(t, OfficialScheduleAPI{
				Create: func(req OfficialScheduleCreateRequest) (OfficialScheduleEntry, error) {
					captured = req
					return OfficialScheduleEntry{ID: "sch-1", Kind: req.Timing.Kind, Title: req.Title,
						Prompt: req.Prompt, ScheduledAt: "2026-11-07T01:00:00Z", Status: "active",
						Target: OfficialScheduleTarget{Kind: "session", SessionID: "s1"}}, nil
				},
			})
			code, out := callScheduleOfficial(t, srv, "schedule.create", tc.body)
			if code != http.StatusOK {
				t.Fatalf("状态 = %d，want 200: %#v", code, out)
			}
			tc.want(t, captured)
		})
	}
}

func TestOfficialScheduleCreateRejects(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		wants []string
	}{
		{name: "空请求", body: `{}`, wants: []string{"title"}},
		{name: "缺 prompt", body: `{"title":"t","timing":{"kind":"every","every_seconds":60}}`, wants: []string{"prompt"}},
		{name: "缺 timing", body: `{"title":"t","prompt":"p"}`, wants: []string{"timing"}},
		{name: "空 title", body: `{"title":"   ","prompt":"p","timing":{"kind":"every","every_seconds":60}}`, wants: []string{"title"}},
		{name: "空 prompt", body: `{"title":"t","prompt":"  ","timing":{"kind":"every","every_seconds":60}}`, wants: []string{"prompt"}},
		{
			name:  "title 超长",
			body:  `{"title":"` + strings.Repeat("好", 121) + `","prompt":"p","timing":{"kind":"every","every_seconds":60}}`,
			wants: []string{"title", "120"},
		},
		{name: "未知 kind", body: `{"title":"t","prompt":"p","timing":{"kind":"minutely"}}`, wants: []string{"timing", "kind"}},
		{name: "timing 不是对象", body: `{"title":"t","prompt":"p","timing":"every"}`, wants: []string{"timing"}},
		{name: "every 缺秒", body: `{"title":"t","prompt":"p","timing":{"kind":"every"}}`, wants: []string{"every_seconds"}},
		{name: "every 太短", body: `{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":59}}`, wants: []string{"every_seconds", "60"}},
		{name: "every 小数", body: `{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60.5}}`, wants: []string{"every_seconds"}},
		{name: "daily 缺时区", body: `{"title":"t","prompt":"p","timing":{"kind":"daily","daily":{"time":"09:30"}}}`, wants: []string{"time_zone"}},
		{name: "daily 时间坏", body: `{"title":"t","prompt":"p","timing":{"kind":"daily","daily":{"time":"9点半","time_zone":"UTC"}}}`, wants: []string{"time"}},
		{name: "weekly 空 weekdays", body: `{"title":"t","prompt":"p","timing":{"kind":"weekly","weekly":{"time":"08:00","time_zone":"UTC","weekdays":[]}}}`, wants: []string{"weekdays"}},
		{name: "weekly 越界星期", body: `{"title":"t","prompt":"p","timing":{"kind":"weekly","weekly":{"time":"08:00","time_zone":"UTC","weekdays":[0]}}}`, wants: []string{"weekdays"}},
		{name: "weekly 重复星期", body: `{"title":"t","prompt":"p","timing":{"kind":"weekly","weekly":{"time":"08:00","time_zone":"UTC","weekdays":[2,2]}}}`, wants: []string{"weekdays"}},
		{name: "cron 缺表达式", body: `{"title":"t","prompt":"p","timing":{"kind":"cron","cron":{"time_zone":"UTC"}}}`, wants: []string{"expression"}},
		{name: "at 字符串坏", body: `{"title":"t","prompt":"p","timing":{"kind":"at","at":"明天九点"}}`, wants: []string{"at"}},
		{name: "at 对象缺字段", body: `{"title":"t","prompt":"p","timing":{"kind":"at","at":{"date":"2026-11-08","time":"09:30"}}}`, wants: []string{"time_zone"}},
		{
			name:  "未知顶层字段",
			body:  `{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60},"sessionId":"s1"}`,
			wants: []string{"sessionId"},
		},
		{
			name:  "timing 里的未知字段",
			body:  `{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60,"at":"x"}}`,
			wants: []string{"at"},
		},
		{name: "target kind 未知", body: `{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60},"target":{"kind":"bogus"}}`, wants: []string{"target"}},
		{name: "target session 缺 id", body: `{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60},"target":{"kind":"session"}}`, wants: []string{"sessionId"}},
		{name: "target new 的未知字段", body: `{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60},"target":{"kind":"new","cwd":"/tmp"}}`, wants: []string{"cwd"}},
		{name: "请求体不是对象", body: `[]`, wants: []string{"JSON"}},
		{name: "请求体是 null", body: `null`, wants: []string{"null"}},
		{name: "请求体为空", body: ``, wants: []string{"空"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			srv := scheduleOfficialServer(t, OfficialScheduleAPI{
				Create: func(OfficialScheduleCreateRequest) (OfficialScheduleEntry, error) {
					called = true
					return OfficialScheduleEntry{}, nil
				},
			})
			code, out := callScheduleOfficial(t, srv, "schedule.create", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("状态 = %d，want 400: %#v", code, out)
			}
			if called {
				t.Fatal("参数不合法时不许调宿主 Create（fail-loud，不静默兜底）")
			}
			msg := scheduleErrorMessage(t, out)
			if got := scheduleErrorCode(t, out); got != "bad_request" {
				t.Fatalf("code = %q，want bad_request: %s", got, msg)
			}
			for _, want := range tc.wants {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误消息里没有 %q，实际 %q", want, msg)
				}
			}
		})
	}
}

func TestOfficialScheduleRequestWrapper(t *testing.T) {
	t.Run("嵌套 request 可以", func(t *testing.T) {
		var captured OfficialScheduleCreateRequest
		srv := scheduleOfficialServer(t, OfficialScheduleAPI{
			Create: func(req OfficialScheduleCreateRequest) (OfficialScheduleEntry, error) {
				captured = req
				return OfficialScheduleEntry{ID: "sch-1", Kind: "every", Title: req.Title, Prompt: req.Prompt,
					ScheduledAt: "2026-11-07T01:00:00Z", Status: "active",
					Target: OfficialScheduleTarget{Kind: "session", SessionID: "s1"}}, nil
			},
		})
		code, out := callScheduleOfficial(t, srv, "schedule.create",
			`{"request":{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60}}}`)
		if code != http.StatusOK {
			t.Fatalf("状态 = %d，want 200: %#v", code, out)
		}
		if captured.Title != "t" {
			t.Fatalf("嵌套 request 没解析到: %#v", captured)
		}
	})

	t.Run("嵌套与平铺同时给是冲突", func(t *testing.T) {
		srv := scheduleOfficialServer(t, OfficialScheduleAPI{})
		code, out := callScheduleOfficial(t, srv, "schedule.create",
			`{"request":{"title":"t"},"title":"t"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("状态 = %d，want 400: %#v", code, out)
		}
	})
}

func TestOfficialScheduleCreateRejectsUnknownFieldInsideRequest(t *testing.T) {
	srv := scheduleOfficialServer(t, OfficialScheduleAPI{
		Create: func(OfficialScheduleCreateRequest) (OfficialScheduleEntry, error) {
			t.Fatal("不该调到宿主")
			return OfficialScheduleEntry{}, nil
		},
	})
	code, out := callScheduleOfficial(t, srv, "schedule.create",
		`{"request":{"title":"t","prompt":"p","timing":{"kind":"every","every_seconds":60},"nope":1}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("状态 = %d，want 400: %#v", code, out)
	}
	if msg := scheduleErrorMessage(t, out); !strings.Contains(msg, "nope") {
		t.Fatalf("错误消息必须点出未知字段名，got %q", msg)
	}
}

// ---------------------------------------------------------------------------
// schedule.edit
// ---------------------------------------------------------------------------

func TestOfficialScheduleEditPassesTypedRequest(t *testing.T) {
	var got OfficialScheduleEditRequest
	srv := scheduleOfficialServer(t, OfficialScheduleAPI{
		Edit: func(req OfficialScheduleEditRequest) (any, error) {
			got = req
			return OfficialScheduleEntry{ID: req.ID, Kind: "daily", Title: *req.Patch.Title,
				Prompt: "p", Time: "09:30:00", TimeZone: "UTC", ScheduledAt: "2026-11-08T01:30:00Z",
				Status: "active", Target: OfficialScheduleTarget{Kind: "session", SessionID: "s1"}}, nil
		},
	})
	code, out := callScheduleOfficial(t, srv, "schedule.edit", `{
		"request":{
			"id":"sch-1",
			"action":"update",
			"patch":{"title":"  新名字  ","timing":{"kind":"daily","daily":{"time":"09:30","time_zone":"UTC"}}},
			"expectedUpdatedAt":"2026-11-07T00:00:00Z"
		}
	}`)
	if code != http.StatusOK {
		t.Fatalf("状态 = %d，want 200: %#v", code, out)
	}
	if got.ID != "sch-1" || got.Action != "update" {
		t.Fatalf("id/action 解析错: %#v", got)
	}
	if got.Patch == nil || got.Patch.Title == nil || *got.Patch.Title != "新名字" {
		t.Fatalf("patch.title 必须 trim 后传出: %#v", got.Patch)
	}
	if got.Patch.Timing == nil || got.Patch.Timing.Kind != "daily" {
		t.Fatalf("patch.timing 解析错: %#v", got.Patch)
	}
	if got.ExpectedUpdatedAt == nil || *got.ExpectedUpdatedAt != "2026-11-07T00:00:00Z" {
		t.Fatalf("expectedUpdatedAt 没传到宿主: %#v", got.ExpectedUpdatedAt)
	}
}

func TestOfficialScheduleEditDelete(t *testing.T) {
	var got OfficialScheduleEditRequest
	srv := scheduleOfficialServer(t, OfficialScheduleAPI{
		Edit: func(req OfficialScheduleEditRequest) (any, error) {
			got = req
			return OfficialScheduleDeleted{ID: req.ID, Deleted: true}, nil
		},
	})
	code, out := callScheduleOfficial(t, srv, "schedule.edit",
		`{"id":"sch-9","action":"delete","expectedUpdatedAt":null}`)
	if code != http.StatusOK {
		t.Fatalf("状态 = %d，want 200: %#v", code, out)
	}
	if got.Action != "delete" || got.Patch != nil {
		t.Fatalf("delete 请求解析错: %#v", got)
	}
	if got.ExpectedUpdatedAt != nil {
		t.Fatalf("显式 null 必须等价于没给 fence: %#v", got.ExpectedUpdatedAt)
	}
	if out["deleted"] != true || out["id"] != "sch-9" {
		t.Fatalf("delete 结果形状错: %#v", out)
	}
}

func TestOfficialScheduleEditRejects(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		wants []string
	}{
		{name: "缺 id", body: `{"action":"delete"}`, wants: []string{"id"}},
		{name: "空 id", body: `{"id":"  ","action":"delete"}`, wants: []string{"id"}},
		{name: "缺 action", body: `{"id":"a"}`, wants: []string{"action"}},
		{name: "action 未知", body: `{"id":"a","action":"rename"}`, wants: []string{"action"}},
		{name: "update 缺 patch", body: `{"id":"a","action":"update"}`, wants: []string{"patch"}},
		{name: "update 空 patch", body: `{"id":"a","action":"update","patch":{}}`, wants: []string{"patch"}},
		{
			name:  "patch 未知字段",
			body:  `{"id":"a","action":"update","patch":{"kind":"every"}}`,
			wants: []string{"kind"},
		},
		{
			name:  "delete 带 patch",
			body:  `{"id":"a","action":"delete","patch":{"title":"x"}}`,
			wants: []string{"patch"},
		},
		{
			name:  "patch.title 空串",
			body:  `{"id":"a","action":"update","patch":{"title":"   "}}`,
			wants: []string{"title"},
		},
		{
			name:  "expectedUpdatedAt 类型错",
			body:  `{"id":"a","action":"delete","expectedUpdatedAt":5}`,
			wants: []string{"expectedUpdatedAt"},
		},
		{
			name:  "patch.active 非布尔",
			body:  `{"id":"a","action":"update","patch":{"active":"no"}}`,
			wants: []string{"active"},
		},
		{
			name:  "未知顶层字段",
			body:  `{"id":"a","action":"delete","sessionId":"s1"}`,
			wants: []string{"sessionId"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			srv := scheduleOfficialServer(t, OfficialScheduleAPI{
				Edit: func(OfficialScheduleEditRequest) (any, error) {
					called = true
					return nil, nil
				},
			})
			code, out := callScheduleOfficial(t, srv, "schedule.edit", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("状态 = %d，want 400: %#v", code, out)
			}
			if called {
				t.Fatal("参数不合法时不许调宿主 Edit")
			}
			msg := scheduleErrorMessage(t, out)
			for _, want := range tc.wants {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误消息里没有 %q，实际 %q", want, msg)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// schedule.view / run / history 的参数面
// ---------------------------------------------------------------------------

func TestOfficialScheduleViewAndRunRequireID(t *testing.T) {
	for _, method := range []string{"schedule.view", "schedule.run"} {
		t.Run(method, func(t *testing.T) {
			srv := scheduleOfficialServer(t, OfficialScheduleAPI{
				View: func(string) (any, error) { t.Fatal("不该调到宿主 View"); return nil, nil },
				Run:  func(string) (any, error) { t.Fatal("不该调到宿主 Run"); return nil, nil },
			})
			for _, body := range []string{`{}`, `{"id":""}`, `{"id":"   "}`, `{"id":5}`} {
				code, out := callScheduleOfficial(t, srv, method, body)
				if code != http.StatusBadRequest {
					t.Fatalf("%s %s 状态 = %d，want 400", method, body, code)
				}
				_ = scheduleErrorObject(t, out)
			}
		})
	}
}

func TestOfficialScheduleHistoryLimitIsOneToOneHundred(t *testing.T) {
	var got OfficialScheduleHistoryRequest
	srv := scheduleOfficialServer(t, OfficialScheduleAPI{
		History: func(req OfficialScheduleHistoryRequest) (any, error) {
			got = req
			return OfficialScheduleHistoryPage{
				ID: req.ID, Records: []OfficialScheduleHistoryRecord{},
				Retention: OfficialScheduleRetention{Days: 30, Records: 100},
			}, nil
		},
	})
	code, out := callScheduleOfficial(t, srv, "schedule.history", `{"id":"a","limit":100,"before":"msg-7"}`)
	if code != http.StatusOK {
		t.Fatalf("limit=100 状态 = %d，want 200: %#v", code, out)
	}
	if got.Limit != 100 || got.Before != "msg-7" {
		t.Fatalf("history 请求解析错: %#v", got)
	}
	if out["id"] != "a" {
		t.Fatalf("history 结果形状错: %#v", out)
	}
	for _, body := range []string{
		`{"id":"a"}`,
		`{"id":"a","limit":0}`,
		`{"id":"a","limit":101}`,
		`{"id":"a","limit":-1}`,
		`{"id":"a","limit":5.5}`,
		`{"id":"a","limit":"5"}`,
		`{"id":"a","limit":5,"before":5}`,
	} {
		code, out := callScheduleOfficial(t, srv, "schedule.history", body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 状态 = %d，want 400: %#v", body, code, out)
		}
		if msg := scheduleErrorMessage(t, out); !strings.Contains(msg, "limit") && !strings.Contains(msg, "before") {
			t.Fatalf("%s 的错误必须点名字段，got %q", body, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// 结果形状与错误映射
// ---------------------------------------------------------------------------

func TestOfficialScheduleHostErrorsMapToFourFields(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		want string
	}{
		{name: "未实现", err: fmt.Errorf("%w: 缺 runner", ErrOfficialNotImplemented), code: http.StatusNotImplemented, want: "not_implemented"},
		{name: "参数非法", err: fmt.Errorf("%w: 时区不支持", ErrOfficialBadInput), code: http.StatusBadRequest, want: "bad_request"},
		// 共享错误映射：ErrOfficialConflict 仍是非 schedule 族（goal/workspace）的 409。
		// schedule.edit 的 revision 冲突不走这里——官方是 200 + union，见下面的用例。
		{name: "共享映射：非 schedule 的 revision 冲突", err: fmt.Errorf("%w: updatedAt 过期", ErrOfficialConflict), code: http.StatusConflict, want: "revision_conflict"},
		{name: "会话不存在", err: fmt.Errorf("%w: 没有 s9", ErrNoSuchSession), code: http.StatusBadRequest, want: "no_such_session"},
		{name: "写入失败", err: fmt.Errorf("%w: 磁盘满", ErrStatePersistence), code: http.StatusInternalServerError, want: "internal"},
		{name: "无类型错误", err: errors.New("boom"), code: http.StatusInternalServerError, want: "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := scheduleOfficialServer(t, OfficialScheduleAPI{
				Edit: func(OfficialScheduleEditRequest) (any, error) { return nil, tc.err },
			})
			code, out := callScheduleOfficial(t, srv, "schedule.edit", `{"id":"a","action":"delete"}`)
			if code != tc.code {
				t.Fatalf("状态 = %d，want %d: %#v", code, tc.code, out)
			}
			if got := scheduleErrorCode(t, out); got != tc.want {
				t.Fatalf("code = %q，want %q", got, tc.want)
			}
		})
	}
}

// 官方 ScheduleConflictResult：expectedUpdatedAt 不匹配是 **200 + {id,code:'schedule_conflict'}**，
// 不是 409，也不是四字段错误体。这条钉住"冲突分支只走 union"的 wire 形状。
func TestOfficialScheduleEditConflictIsTwoHundredUnion(t *testing.T) {
	srv := scheduleOfficialServer(t, OfficialScheduleAPI{
		Edit: func(req OfficialScheduleEditRequest) (any, error) {
			return OfficialScheduleMiss{ID: req.ID, Code: "schedule_conflict"}, nil
		},
	})
	code, out := callScheduleOfficial(t, srv, "schedule.edit",
		`{"id":"a","action":"update","patch":{"title":"t"},"expectedUpdatedAt":"2000-01-01T00:00:00Z"}`)
	if code != http.StatusOK {
		t.Fatalf("冲突状态 = %d，want 200: %#v", code, out)
	}
	if out["id"] != "a" || out["code"] != "schedule_conflict" {
		t.Fatalf("冲突 union 形状错: %#v", out)
	}
	if len(out) != 2 {
		t.Fatalf("冲突 union 只该有 id 与 code: %#v", out)
	}
}

func TestOfficialScheduleEntryJSONMatchesOfficialShape(t *testing.T) {
	every := OfficialScheduleEntry{
		ID: "sch-1", Kind: "every", Title: "t", Prompt: "p", EverySeconds: 300,
		ScheduledAt: "2026-11-07T01:00:00Z", Status: "active",
		Target:             OfficialScheduleTarget{Kind: "session", SessionID: "s1"},
		InvolvedSessionIDs: []string{"s1"},
		CreatedAt:          "2026-11-07T00:00:00Z",
		UpdatedAt:          "2026-11-07T00:00:00Z",
	}
	raw, err := json.Marshal(every)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"id", "kind", "title", "prompt", "everySeconds", "scheduledAt", "target", "involvedSessionIds", "status"} {
		if _, ok := obj[key]; !ok {
			t.Fatalf("官方必填字段 %q 缺失: %s", key, raw)
		}
	}
	// 与 kind 无关的字段不许出现：官方 union 的关键是"哪个 kind 带哪些键"。
	for _, key := range []string{"afterSeconds", "time", "timeZone", "weekdays", "expression", "rrule", "executionActive", "lastDelivery"} {
		if _, ok := obj[key]; ok {
			t.Fatalf("kind=every 不该带 %q: %s", key, raw)
		}
	}
	target, _ := obj["target"].(map[string]any)
	if target["kind"] != "session" || target["sessionId"] != "s1" {
		t.Fatalf("target 形状错: %#v", target)
	}

	weekly := OfficialScheduleEntry{
		ID: "sch-2", Kind: "weekly", Title: "t", Prompt: "p", Time: "08:00:00", TimeZone: "UTC",
		Weekdays: []int{1, 3}, ScheduledAt: "2026-11-09T08:00:00Z", Status: "active",
		Target: OfficialScheduleTarget{Kind: "session", SessionID: "s1"},
	}
	raw, err = json.Marshal(weekly)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	obj = map[string]any{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := obj["weekdays"]; !ok {
		t.Fatalf("kind=weekly 必须带 weekdays: %s", raw)
	}
	if _, ok := obj["everySeconds"]; ok {
		t.Fatalf("kind=weekly 不该带 everySeconds: %s", raw)
	}
}

func TestOfficialScheduleMissAndPendingShapes(t *testing.T) {
	for _, tc := range []struct {
		code string
		obj  any
		want string
	}{
		{code: "not-found", obj: OfficialScheduleMiss{ID: "a", Code: "schedule_not_found"}, want: "schedule_not_found"},
		{code: "pending", obj: OfficialScheduleMiss{ID: "a", Code: "schedule_execution_pending"}, want: "schedule_execution_pending"},
		{code: "conflict", obj: OfficialScheduleMiss{ID: "a", Code: "schedule_conflict"}, want: "schedule_conflict"},
		{code: "cursor", obj: OfficialScheduleMiss{ID: "a", Code: "delivery_cursor_not_found"}, want: "delivery_cursor_not_found"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			raw, err := json.Marshal(tc.obj)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if obj["code"] != tc.want || obj["id"] != "a" {
				t.Fatalf("miss/pending 形状错: %s", raw)
			}
			if len(obj) != 2 {
				t.Fatalf("miss/pending 只能是 {id,code} 两个键: %s", raw)
			}
		})
	}
}

func TestOfficialScheduleConcurrentCallsDoNotRaceOnMount(t *testing.T) {
	srv := scheduleOfficialServer(t, OfficialScheduleAPI{
		View: func(id string) (any, error) {
			return OfficialScheduleEntry{ID: id, Kind: "every", Title: "t", Prompt: "p", EverySeconds: 60,
				ScheduledAt: "2026-11-07T00:00:00Z", Status: "active",
				Target: OfficialScheduleTarget{Kind: "session", SessionID: "s1"}}, nil
		},
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 不用带 t 的 helper：t.Fatalf 不能在非测试 goroutine 里调。
			req := httptest.NewRequest(http.MethodPost, "/api/schedule.view", strings.NewReader(`{"id":"a"}`))
			rec := httptest.NewRecorder()
			srv.serveOfficialScheduleAPI(rec, req, "schedule.view")
			if rec.Code != http.StatusOK {
				t.Errorf("并发调用状态 = %d，want 200", rec.Code)
			}
		}()
	}
	wg.Wait()
}
