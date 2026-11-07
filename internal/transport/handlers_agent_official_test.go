package transport

// 官方桌面端 agent 侧 24 个方法的契约测试。
//
// 这些测试只打本包的 `serveOfficialAgentAPI`（Lead 接线后由 /api 路由调它），
// 因此不依赖 server.go 的分发表——**路由接线是 Lead 的验收项**，测试里
// 用 IsOfficialAgentMethod 钉住"方法名集合"，避免新增方法从路由漏下去。

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// officialTestServer 造一个挂了实现的最小 Server。
func officialTestServer(t *testing.T, api OfficialAgentAPI) *Server {
	t.Helper()
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.AttachOfficialAgentAPI(api)
	t.Cleanup(srv.DetachOfficialAgentAPI)
	return srv
}

// callOfficial 打一次方法，返回 HTTP 状态与解码后的信封。
func callOfficial(t *testing.T, srv *Server, method string, body any) (int, map[string]any) {
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
	srv.serveOfficialAgentAPI(rec, req, method)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
	}
	return rec.Code, out
}

// assertFourFieldError 钉住错误信封的四个字段（H8 的契约）。
func assertFourFieldError(t *testing.T, code int, out map[string]any, wantStatus int, wantCode string) {
	t.Helper()
	if code != wantStatus {
		t.Fatalf("状态码 %d，期望 %d（body=%v）", code, wantStatus, out)
	}
	obj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("错误信封缺 error 对象：%v", out)
	}
	for _, key := range []string{"code", "message", "hint", "retryable"} {
		if _, has := obj[key]; !has {
			t.Fatalf("错误信封缺 %s：%v", key, obj)
		}
	}
	if got := obj["code"]; got != wantCode {
		t.Fatalf("错误码 %v，期望 %s（message=%v）", got, wantCode, obj["message"])
	}
	if msg, _ := obj["message"].(string); strings.TrimSpace(msg) == "" {
		t.Fatalf("错误 message 是空的")
	}
	if hint, _ := obj["hint"].(string); strings.TrimSpace(hint) == "" {
		t.Fatalf("错误 hint 是空的（每个错误都要有下一步）")
	}
}

func TestOfficialAgentMethodsAreStable(t *testing.T) {
	methods := OfficialAgentMethods()
	if len(methods) != 24 {
		t.Fatalf("官方 agent 方法应是 24 个，实际 %d", len(methods))
	}
	seen := map[Method]bool{}
	for _, m := range methods {
		if seen[m] {
			t.Fatalf("方法重复：%s", m)
		}
		seen[m] = true
		if !IsOfficialAgentMethod(string(m)) {
			t.Fatalf("%s 没被 IsOfficialAgentMethod 认出来（路由会漏）", m)
		}
		if strings.Contains(string(m), "/") {
			t.Fatalf("%s 含 '/'：本仓方法名用点号", m)
		}
	}
	// 官方命名空间逐个点名，防止"少写一个也没人发现"。
	want := []string{
		"goals.get", "goals.create", "goals.edit", "goals.pause", "goals.resume",
		"goals.complete", "goals.clear",
		"commands.list", "commands.execute",
		"subagents.prompt", "subagents.interruptByParent",
		"userQuestions.answer", "userQuestions.attachWait",
		"agentPresets.read", "agentPresets.select",
		"skills.list", "fileReferences.list", "fileUploads.upload",
		"messageFeedback.list", "messageFeedback.put", "messageFeedback.delete",
		"sessionFeedback.record", "sessionReferenceResolver.candidates",
		"toolResults.watch",
	}
	got := make([]string, 0, len(methods))
	for _, m := range methods {
		got = append(got, string(m))
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("方法集合不一致\n got=%v\nwant=%v", got, want)
	}
	for _, other := range []string{"session.list", "goals.get2", "goals", ""} {
		if IsOfficialAgentMethod(other) {
			t.Fatalf("%q 不该被当成官方 agent 方法", other)
		}
	}
}

// 每个方法在"宿主没实现"时都必须是四字段 501，并点出是哪一个方法。
func TestOfficialAgentUnimplementedIsLoudPerMethod(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{})
	bodies := officialMinimalBodies()
	for _, method := range OfficialAgentMethods() {
		code, out := callOfficial(t, srv, string(method), bodies[method])
		assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
		msg := out["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, string(method)) {
			t.Fatalf("%s 的未实现错误没点出方法名：%q", method, msg)
		}
	}
}

// 没挂实现时也不许装成功。
func TestOfficialAgentNotAttachedIsNotImplemented(t *testing.T) {
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	for _, method := range OfficialAgentMethods() {
		code, out := callOfficial(t, srv, string(method), officialMinimalBodies()[method])
		assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	}
}

// officialMinimalBodies 是每个方法一份"形状合法但可能语义无效"的最小入参。
// 它只用来驱动"未实现"路径，不参与真实语义断言。
func officialMinimalBodies() map[Method]any {
	ref := map[string]any{"id": "goal-1", "revision": 1}
	return map[Method]any{
		MethodGoalsGet:      map[string]any{"agentId": "s1"},
		MethodGoalsCreate:   map[string]any{"agentId": "s1", "request": map[string]any{"objective": "做完"}},
		MethodGoalsEdit:     map[string]any{"agentId": "s1", "ref": ref, "request": map[string]any{"objective": "改"}},
		MethodGoalsPause:    map[string]any{"agentId": "s1", "ref": ref},
		MethodGoalsResume:   map[string]any{"agentId": "s1", "ref": ref},
		MethodGoalsComplete: map[string]any{"agentId": "s1", "ref": ref},
		MethodGoalsClear:    map[string]any{"agentId": "s1", "ref": ref},
		MethodCommandsList:  map[string]any{"agentId": "s1"},
		MethodCommandsExecute: map[string]any{
			"agentId": "s1", "line": "/help", "submittedAttachments": []any{},
		},
		MethodSubagentsPrompt: map[string]any{"request": map[string]any{
			"requestId": "r1", "parentSessionId": "p1", "childSessionId": "c1",
			"mode": "continuable", "delivery": "queue",
			"content": []any{map[string]any{"type": "text", "text": "跑一下"}},
		}},
		MethodSubagentsInterruptByParent: map[string]any{
			"childSessionId": "c1", "parentSessionId": "p1", "mode": "continuable",
		},
		MethodUserQuestionsAnswer: map[string]any{
			"agentId": "s1", "callId": "call-1",
			"answer": map[string]any{"answers": []any{
				map[string]any{"id": "q1", "selected": []any{"A"}},
			}},
		},
		MethodUserQuestionsAttachWait: map[string]any{"agentId": "s1", "callId": "call-1"},
		MethodAgentPresetsRead:        map[string]any{"agentPreset": "default"},
		MethodAgentPresetsSelect:      map[string]any{"agentId": "s1", "agentPreset": "default"},
		MethodSkillsList:              map[string]any{"request": map[string]any{"sessionId": "s1"}},
		MethodFileReferencesList:      map[string]any{"agentId": "s1", "query": ""},
		MethodFileUploadsUpload: map[string]any{
			"agentId": "s1", "request": map[string]any{"data": "aGk=", "name": "hi.txt"},
		},
		MethodMessageFeedbackList: map[string]any{"request": map[string]any{"sessionId": "s1"}},
		MethodMessageFeedbackPut: map[string]any{"request": map[string]any{
			"sessionId": "s1", "messageId": "m1", "rating": "positive", "ifVersion": nil,
		}},
		MethodMessageFeedbackDelete: map[string]any{"request": map[string]any{
			"sessionId": "s1", "messageId": "m1", "ifVersion": "v1",
		}},
		MethodSessionFeedbackRecord: map[string]any{"request": map[string]any{
			"sessionId": "s1", "text": "还行", "category": "other",
		}},
		MethodSessionReferenceResolverCandidates: map[string]any{"agentId": "s1", "query": ""},
		// toolResults.watch 是 `mode:'stream'`：官方 wire 是**flat** args
		// `{sessionId, seq}`（typert.host.js 的 jsonSchema），不是 `{request:{…}}`。
		MethodToolResultsWatch: map[string]any{"sessionId": "s1", "seq": 3},
	}
}

// 参数缺失 / 未知字段 / 类型不对都必须 400 且点出字段名（不许静默取默认值）。
func TestOfficialAgentParamsFailLoud(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{})
	cases := []struct {
		name   string
		method Method
		body   string
		含      string
	}{
		{"缺会话", MethodGoalsGet, `{}`, "agentId"},
		{"未知字段", MethodGoalsGet, `{"agentId":"s1","nope":1}`, "nope"},
		{"会话类型错", MethodGoalsGet, `{"agentId":123}`, "agentId"},
		{"会话空串", MethodGoalsGet, `{"agentId":""}`, "空串"},
		{"不是对象", MethodGoalsGet, `[]`, "JSON 对象"},
		{"null 体", MethodGoalsGet, `null`, "null"},
		{"空体", MethodGoalsGet, ``, "空"},
		{"缺 ref", MethodGoalsPause, `{"agentId":"s1"}`, "ref"},
		{"ref 缺 revision", MethodGoalsPause, `{"agentId":"s1","ref":{"id":"g"}}`, "revision"},
		{"ref revision 非正", MethodGoalsPause, `{"agentId":"s1","ref":{"id":"g","revision":0}}`, "正整数"},
		{"ref 未知字段", MethodGoalsPause, `{"agentId":"s1","ref":{"id":"g","revision":1,"x":2}}`, "x"},
		{"create 缺 objective", MethodGoalsCreate, `{"agentId":"s1","request":{}}`, "objective"},
		{"create 目标为空", MethodGoalsCreate, `{"agentId":"s1","objective":""}`, "空串"},
		{"request 与平铺冲突", MethodGoalsCreate, `{"agentId":"s1","request":{"objective":"a"},"objective":"b"}`, "不能同时给"},
		{"edit 没给任何字段", MethodGoalsEdit, `{"agentId":"s1","ref":{"id":"g","revision":1},"request":{}}`, "至少"},
		{"execute 缺 line", MethodCommandsExecute, `{"agentId":"s1"}`, "line"},
		{"execute 未知字段", MethodCommandsExecute, `{"agentId":"s1","line":"/x","附件":[]}`, "附件"},
		{"子代理 mode 错", MethodSubagentsInterruptByParent, `{"childSessionId":"c","parentSessionId":"p","mode":"cold"}`, "continuable"},
		{"子代理 prompt 缺 content", MethodSubagentsPrompt,
			`{"request":{"requestId":"r","parentSessionId":"p","childSessionId":"c","mode":"continuable","delivery":"queue"}}`, "content"},
		{"子代理 content 类型错", MethodSubagentsPrompt,
			`{"request":{"requestId":"r","parentSessionId":"p","childSessionId":"c","mode":"continuable","delivery":"queue","content":[{"type":"video"}]}}`, "text 或 image"},
		{"答案缺 answers", MethodUserQuestionsAnswer, `{"agentId":"s1","callId":"c","answer":{}}`, "answers"},
		{"答案空数组", MethodUserQuestionsAnswer, `{"agentId":"s1","callId":"c","answer":{"answers":[]}}`, "空数组"},
		{"preset 空串", MethodAgentPresetsRead, `{"agentPreset":""}`, "空串"},
		{"引用缺 query", MethodFileReferencesList, `{"agentId":"s1"}`, "query"},
		{"上传缺 data", MethodFileUploadsUpload, `{"agentId":"s1","request":{"name":"a"}}`, "data"},
		{"上传 data 空", MethodFileUploadsUpload, `{"agentId":"s1","request":{"data":""}}`, "空串"},
		{"反馈 rating 非法", MethodMessageFeedbackPut,
			`{"request":{"sessionId":"s1","messageId":"m","rating":"meh","ifVersion":null}}`, "positive"},
		{"反馈缺 ifVersion", MethodMessageFeedbackPut,
			`{"request":{"sessionId":"s1","messageId":"m","rating":"positive"}}`, "ifVersion"},
		{"反馈 category 非法", MethodMessageFeedbackPut,
			`{"request":{"sessionId":"s1","messageId":"m","rating":"positive","category":"weird","ifVersion":null}}`, "枚举"},
		{"删除缺 ifVersion", MethodMessageFeedbackDelete, `{"request":{"sessionId":"s1","messageId":"m"}}`, "ifVersion"},
		{"会话反馈 category 非法", MethodSessionFeedbackRecord,
			`{"request":{"sessionId":"s1","category":"weird"}}`, "枚举"},
		// seq 是 **durable 事件序号**（0 基），0 是合法值；负数才非法。
		{"watch seq 为负", MethodToolResultsWatch, `{"sessionId":"s1","seq":-1}`, "不能为负"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := callOfficial(t, srv, string(tc.method), tc.body)
			assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
			msg := out["error"].(map[string]any)["message"].(string)
			if !strings.Contains(msg, tc.含) {
				t.Fatalf("%s 的错误没点出「%s」：%q", tc.method, tc.含, msg)
			}
		})
	}
}

// goals.* 的结果必须逐字带官方 GoalView 的字段。
func TestOfficialAgentGoalsViewShape(t *testing.T) {
	var gotSession string
	view := OfficialGoalView{
		ID: "goal-1", Revision: 3, Objective: "把接口对齐", Phase: "active",
		MaxGoalRounds: 10, RoundsStarted: 2, CreatedAt: 1000, UpdatedAt: 2000,
		Activation: "armed",
	}
	srv := officialTestServer(t, OfficialAgentAPI{
		GoalGet: func(sessionID string) (OfficialGoalGetResult, error) {
			gotSession = sessionID
			v := view
			return OfficialGoalGetResult{Goal: &v}, nil
		},
		GoalCreate: func(sessionID string, req OfficialGoalCreateRequest) (OfficialGoalRef, error) {
			if req.Objective != "把接口对齐" || req.MaxGoalRounds == nil || *req.MaxGoalRounds != 7 {
				t.Fatalf("create 收到 %+v", req)
			}
			return OfficialGoalRef{ID: "goal-1", Revision: 1}, nil
		},
		GoalEdit: func(sessionID string, ref OfficialGoalRef, req OfficialGoalEditRequest) (OfficialGoalView, error) {
			if ref.ID != "goal-1" || ref.Revision != 3 {
				t.Fatalf("edit 的 ref 是 %+v", ref)
			}
			if req.Objective == nil || *req.Objective != "改一下" || req.MaxGoalRounds != nil {
				t.Fatalf("edit 收到 %+v", req)
			}
			return view, nil
		},
		GoalPause:    func(string, OfficialGoalRef) (OfficialGoalView, error) { return view, nil },
		GoalResume:   func(string, OfficialGoalRef) (OfficialGoalView, error) { return view, nil },
		GoalComplete: func(string, OfficialGoalRef) (OfficialGoalView, error) { return view, nil },
		GoalClear: func(string, OfficialGoalRef) (OfficialGoalRef, error) {
			return OfficialGoalRef{ID: "goal-1", Revision: 4}, nil
		},
	})

	code, out := callOfficial(t, srv, "goals.get", map[string]any{"agentId": "s1"})
	if code != http.StatusOK {
		t.Fatalf("goals.get 状态 %d: %v", code, out)
	}
	if gotSession != "s1" {
		t.Fatalf("goals.get 收到的会话是 %q", gotSession)
	}
	goal := out["result"].(map[string]any)["goal"].(map[string]any)
	for _, key := range []string{
		"id", "revision", "objective", "phase", "maxGoalRounds",
		"roundsStarted", "createdAt", "updatedAt", "activation",
	} {
		if _, has := goal[key]; !has {
			t.Fatalf("GoalView 缺字段 %s：%v", key, goal)
		}
	}
	if goal["activation"] != "armed" {
		t.Fatalf("activation 应原样透传，实际 %v", goal["activation"])
	}

	code, out = callOfficial(t, srv, "goals.create", map[string]any{
		"agentId": "s1", "request": map[string]any{"objective": "把接口对齐", "maxGoalRounds": 7},
	})
	if code != http.StatusOK {
		t.Fatalf("goals.create 状态 %d: %v", code, out)
	}
	ref := out["result"].(map[string]any)["ref"].(map[string]any)
	if ref["id"] != "goal-1" || ref["revision"].(float64) != 1 {
		t.Fatalf("create 的 ref 形状不对：%v", ref)
	}

	code, out = callOfficial(t, srv, "goals.edit", map[string]any{
		"agentId": "s1", "ref": map[string]any{"id": "goal-1", "revision": 3},
		"request": map[string]any{"objective": "改一下"},
	})
	if code != http.StatusOK {
		t.Fatalf("goals.edit 状态 %d: %v", code, out)
	}

	code, out = callOfficial(t, srv, "goals.clear", map[string]any{
		"agentId": "s1", "ref": map[string]any{"id": "goal-1", "revision": 3},
	})
	if code != http.StatusOK {
		t.Fatalf("goals.clear 状态 %d: %v", code, out)
	}
	if out["result"].(map[string]any)["revision"].(float64) != 4 {
		t.Fatalf("clear 应回 tombstone ref（revision 比被清的快照大一）：%v", out)
	}
}

// 没有当前 goal 时回 {"goal":null}（官方是 undefined；本仓信封不接受 null 结果）。
func TestOfficialAgentGoalsGetWithoutGoal(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{
		GoalGet: func(string) (OfficialGoalGetResult, error) { return OfficialGoalGetResult{}, nil },
	})
	code, out := callOfficial(t, srv, "goals.get", map[string]any{"agentId": "s1"})
	if code != http.StatusOK {
		t.Fatalf("goals.get 状态 %d: %v", code, out)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("result 不是对象：%v", out)
	}
	if v, has := result["goal"]; !has || v != nil {
		t.Fatalf("没有 goal 时应该是 {\"goal\":null}，实际 %v", result)
	}
}

// 宿主返回的类型化错误必须映射成正确的状态与稳定码，不许一律 500。
func TestOfficialAgentErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"未实现", ErrOfficialNotImplemented, http.StatusNotImplemented, "not_implemented"},
		{"没有会话", ErrNoSuchSession, http.StatusBadRequest, "no_such_session"},
		{"持久化失败", ErrStatePersistence, http.StatusInternalServerError, "internal"},
		{"版本冲突", ErrGoalTransition, http.StatusConflict, "revision_conflict"},
		{"未知错误", errors.New("炸了"), http.StatusInternalServerError, "internal"},
		{"显式 400", officialBadRequest("参数不对"), http.StatusBadRequest, "bad_request"},
		{"显式 404", officialNotFound("没有"), http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := officialTestServer(t, OfficialAgentAPI{
				GoalGet: func(string) (OfficialGoalGetResult, error) {
					return OfficialGoalGetResult{}, tc.err
				},
			})
			code, out := callOfficial(t, srv, "goals.get", map[string]any{"agentId": "s1"})
			assertFourFieldError(t, code, out, tc.status, tc.code)
		})
	}
}

// A/B 会话不串：Transport 必须把调用方的会话 id 原样递给宿主，不许拿"当前会话"顶上。
func TestOfficialAgentSessionsDoNotCross(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	srv := officialTestServer(t, OfficialAgentAPI{
		GoalGet: func(sessionID string) (OfficialGoalGetResult, error) {
			mu.Lock()
			seen[sessionID]++
			mu.Unlock()
			v := OfficialGoalView{ID: "goal-" + sessionID, Revision: 1, Activation: "disarmed"}
			return OfficialGoalGetResult{Goal: &v}, nil
		},
	})
	code, out := callOfficial(t, srv, "goals.get", map[string]any{"agentId": "A"})
	if code != http.StatusOK {
		t.Fatalf("A 状态 %d: %v", code, out)
	}
	code, out = callOfficial(t, srv, "goals.get", map[string]any{"agentId": "B"})
	if code != http.StatusOK {
		t.Fatalf("B 状态 %d: %v", code, out)
	}
	if seen["A"] != 1 || seen["B"] != 1 {
		t.Fatalf("两个会话被串了：%v", seen)
	}
}

// 会话 id 的别名（agentId / sessionId / id）都认，但互相冲突要拒。
func TestOfficialAgentSessionIDAliases(t *testing.T) {
	var seen []string
	srv := officialTestServer(t, OfficialAgentAPI{
		GoalGet: func(sessionID string) (OfficialGoalGetResult, error) {
			seen = append(seen, sessionID)
			return OfficialGoalGetResult{}, nil
		},
	})
	for _, body := range []map[string]any{
		{"agentId": "s1"}, {"sessionId": "s2"}, {"id": "s3"},
	} {
		code, out := callOfficial(t, srv, "goals.get", body)
		if code != http.StatusOK {
			t.Fatalf("%v 被拒了：%d %v", body, code, out)
		}
	}
	if strings.Join(seen, ",") != "s1,s2,s3" {
		t.Fatalf("别名解析结果 %v", seen)
	}
	code, out := callOfficial(t, srv, "goals.get", map[string]any{"agentId": "a", "sessionId": "b"})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
}

// commands.execute 的"语法/名字未解析"用 null 表达（官方 undefined）。
func TestOfficialAgentCommandsExecuteUndefined(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{
		CommandsExecute: func(string, string, []OfficialSubmittedAttachment) (*OfficialCommandExecution, error) {
			return nil, nil
		},
	})
	code, out := callOfficial(t, srv, "commands.execute", map[string]any{"agentId": "s1", "line": "/nope"})
	if code != http.StatusOK {
		t.Fatalf("状态 %d: %v", code, out)
	}
	// 官方这里回 undefined；本仓信封不接受 null，所以是**空 result 对象**，
	// 客户端据此与"真执行了"（有 commandId/result）区分。
	result, ok := out["result"].(map[string]any)
	if !ok || len(result) != 0 {
		t.Fatalf("没有载荷时应是空 result 对象，实际 %v", out)
	}
}

// 命令清单/引用候选的空结果必须是 []，不是 null（null 会被出口拦成 500）。
func TestOfficialAgentEmptyListsAreArrays(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{
		CommandsList: func(string) ([]OfficialCommand, error) { return nil, nil },
		FileReferencesList: func(string, string) ([]OfficialFileReference, error) {
			return nil, nil
		},
		SessionReferenceCandidates: func(string, string) ([]OfficialSessionReferenceCandidate, error) {
			return nil, nil
		},
	})
	for _, tc := range []struct {
		method Method
		body   any
	}{
		{MethodCommandsList, map[string]any{"agentId": "s1"}},
		{MethodFileReferencesList, map[string]any{"agentId": "s1", "query": ""}},
		{MethodSessionReferenceResolverCandidates, map[string]any{"agentId": "s1", "query": ""}},
	} {
		code, out := callOfficial(t, srv, string(tc.method), tc.body)
		if code != http.StatusOK {
			t.Fatalf("%s 状态 %d: %v", tc.method, code, out)
		}
		arr, ok := out["result"].([]any)
		if !ok || len(arr) != 0 {
			t.Fatalf("%s 的空结果是 %v，期望 []", tc.method, out["result"])
		}
	}
}

// 反馈类是 ok/value | ok/error 的 union：两种形状都要能被原样编码。
func TestOfficialAgentFeedbackUnionShapes(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{
		MessageFeedbackList: func(string) (OfficialMessageFeedbackListResult, error) {
			return OfficialMessageFeedbackListResult{OK: true, Value: &OfficialMessageFeedbackValue{
				Items: []OfficialMessageFeedbackItem{{
					MessageID: "m1", Rating: "positive", Version: "v1",
					CreatedAt: 1, UpdatedAt: 2,
				}},
			}}, nil
		},
		MessageFeedbackPut: func(req OfficialMessageFeedbackPutRequest) (OfficialMessageFeedbackPutResult, error) {
			if req.IfVersion != nil {
				t.Fatalf("ifVersion=null 应该解析成 nil，实际 %q", *req.IfVersion)
			}
			return OfficialMessageFeedbackPutResult{OK: false, Error: &OfficialFeedbackFailure{
				Code: "version-conflict", Current: json.RawMessage("null"),
			}}, nil
		},
		MessageFeedbackDelete: func(req OfficialMessageFeedbackDeleteRequest) (OfficialMessageFeedbackDeleteResult, error) {
			if req.IfVersion != "v1" {
				t.Fatalf("ifVersion 没透传：%q", req.IfVersion)
			}
			return OfficialMessageFeedbackDeleteResult{OK: true, Value: &OfficialMessageFeedbackDeleteValue{Absent: true}}, nil
		},
		SessionFeedbackRecord: func(req OfficialSessionFeedbackRequest) (OfficialSessionFeedbackResult, error) {
			return OfficialSessionFeedbackResult{OK: false, Error: &OfficialFeedbackFailure{
				Code: "session-not-found", SessionID: req.SessionID,
			}}, nil
		},
	})

	code, out := callOfficial(t, srv, "messageFeedback.list", map[string]any{
		"request": map[string]any{"sessionId": "s1"},
	})
	if code != http.StatusOK {
		t.Fatalf("list 状态 %d: %v", code, out)
	}
	list := out["result"].(map[string]any)
	if list["ok"] != true {
		t.Fatalf("list 的 ok 应为 true：%v", list)
	}
	item := list["value"].(map[string]any)["items"].([]any)[0].(map[string]any)
	for _, key := range []string{"messageId", "rating", "version", "createdAt", "updatedAt"} {
		if _, has := item[key]; !has {
			t.Fatalf("反馈项缺 %s：%v", key, item)
		}
	}

	code, out = callOfficial(t, srv, "messageFeedback.put", map[string]any{"request": map[string]any{
		"sessionId": "s1", "messageId": "m1", "rating": "positive", "ifVersion": nil,
	}})
	if code != http.StatusOK {
		t.Fatalf("put 状态 %d: %v", code, out)
	}
	put := out["result"].(map[string]any)
	if put["ok"] != false {
		t.Fatalf("put 的 ok 应为 false：%v", put)
	}
	errObj := put["error"].(map[string]any)
	if errObj["code"] != "version-conflict" {
		t.Fatalf("put 的错误码 %v", errObj["code"])
	}
	if cur, has := errObj["current"]; !has || cur != nil {
		t.Fatalf("version-conflict 必须带 current:null，实际 %v", errObj)
	}

	code, out = callOfficial(t, srv, "messageFeedback.delete", map[string]any{"request": map[string]any{
		"sessionId": "s1", "messageId": "m1", "ifVersion": "v1",
	}})
	if code != http.StatusOK {
		t.Fatalf("delete 状态 %d: %v", code, out)
	}
	if abs := out["result"].(map[string]any)["value"].(map[string]any)["absent"]; abs != true {
		t.Fatalf("delete 的 value.absent 应为 true：%v", out)
	}

	code, out = callOfficial(t, srv, "sessionFeedback.record", map[string]any{"request": map[string]any{
		"sessionId": "s9", "text": "还行",
	}})
	if code != http.StatusOK {
		t.Fatalf("sessionFeedback 状态 %d: %v", code, out)
	}
	if got := out["result"].(map[string]any)["error"].(map[string]any)["code"]; got != "session-not-found" {
		t.Fatalf("sessionFeedback 的错误码 %v", got)
	}
}

// 上传回执与文件引用字段逐条对齐官方。
func TestOfficialAgentFileUploadShape(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{
		FileUploadsUpload: func(sessionID string, req OfficialFileUploadRequest) (OfficialFileUploadValue, error) {
			if sessionID != "s1" || req.Data != "aGk=" || req.Name != "hi.txt" {
				t.Fatalf("upload 收到 %q %+v", sessionID, req)
			}
			return OfficialFileUploadValue{
				ReceiptID: "rcpt-1",
				File:      OfficialUploadedFile{AttachmentID: "sha", Name: "hi.txt", Bytes: 2},
			}, nil
		},
	})
	code, out := callOfficial(t, srv, "fileUploads.upload", map[string]any{
		"agentId": "s1", "request": map[string]any{"data": "aGk=", "name": "hi.txt"},
	})
	if code != http.StatusOK {
		t.Fatalf("状态 %d: %v", code, out)
	}
	result := out["result"].(map[string]any)
	if result["receiptId"] != "rcpt-1" {
		t.Fatalf("receiptId 没透传：%v", result)
	}
	file := result["file"].(map[string]any)
	for _, key := range []string{"attachmentId", "name", "bytes"} {
		if _, has := file[key]; !has {
			t.Fatalf("file 缺 %s：%v", key, file)
		}
	}
}

// toolResults.watch 是 `mode:'stream'`：args 是**官方 flat 形状**
// `{sessionId, seq}`（typert.host.js 的 jsonSchema），不是 L1 的
// `{"request":{…}}` 包壳——官方 wire 上参数名就是那个对象本身。
func TestOfficialAgentToolResultsWatchShape(t *testing.T) {
	srv := officialTestServer(t, OfficialAgentAPI{
		ToolResultsWatch: func(sessionID string, seq int64) (OfficialToolResultWatch, error) {
			if sessionID != "s1" || seq != 7 {
				t.Errorf("watch 收到 %q %d", sessionID, seq)
			}
			return OfficialToolResultWatch{Available: false}, nil
		},
	})
	rec, _ := runStream(t, func(w http.ResponseWriter, r *http.Request) {
		srv.serveOfficialAgentAPI(w, r, "toolResults.watch")
	}, `{"sessionId":"s1","seq":7}`)
	frames := rec.waitFrames(t, 1)
	if code := rec.status(); code != http.StatusOK {
		t.Fatalf("状态 %d", code)
	}
	if frames[0]["available"] != false {
		t.Fatalf("available 应为 false：%v", frames[0])
	}
	if _, has := frames[0]["value"]; has {
		t.Fatalf("available=false 时不该带 value：%v", frames[0])
	}

	// L1 的 `{"request":{…}}` 包壳在官方 wire 上是**未知字段** → 400（fail-loud）。
	rec2, _ := runStream(t, func(w http.ResponseWriter, r *http.Request) {
		srv.serveOfficialAgentAPI(w, r, "toolResults.watch")
	}, `{"request":{"sessionId":"s1","seq":7}}`)
	if code := rec2.waitStatus(t); code != http.StatusBadRequest {
		t.Fatalf("包壳 args 应 400（官方 wire 是 flat），实际 %d", code)
	}
}
