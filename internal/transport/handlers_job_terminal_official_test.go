package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件钉住官方 0.2.2-rc.1 的 job.*（3）与 terminal.*（8）共 11 个方法。
//
// 契约来源（只读镜像，见 docs/official-desktop-api-parity.md）：
//   job-controller/lib/typert.host.js:73   job/list  parameter {sessionId}
//   job-controller/lib/typert.host.js:77   job/list  result {type:'rows', jobs:[JobView]}
//   job-controller/lib/typert.host.js:64   job/kill  parameter {sessionId, jobId}
//   job-controller/lib/typert.host.js:69   job/kill  result {outcome:'requested'|'already-finished'}
//   job-controller/lib/typert.host.js:7    job/follow  parameter {sessionId?, jobId, from?}
//   job-controller/lib/typert.host.js:13   job/follow  result opened|output|status 帧
//   terminal-controller/lib/typert.host.js:82   terminal/create request {shellPath?, id, cols, rows}
//   terminal-controller/lib/typert.host.js:89   terminal/create result WebTerminalInfo
//   terminal-controller/lib/typert.host.js:107  terminal/environment result（6 个数字字段）
//   terminal-controller/lib/typert.host.js:22   terminal/attach result snapshot|output|state|ack
//
// 为什么测试直接调 handler 而不是走 handleAPI：路由的 case 由 Lead 在
// internal/transport/server.go 里补（本任务禁止改那个文件）。TestOfficialJobTerminal_JobTerminalRouteStatus
// 单独盯住"路由是否已经接上"这件事——它不会因为没接线而变红，但接线后会自动升级成形状断言。

// int64Ptr 给帧里的可选数字字段造指针（nil = 字段不出现）。
func int64Ptr(v int64) *int64 { return &v }

// fakeOfficialJobTerminal 是协议层的替身：只回答"handler 把参数传对了吗、结果怎么编码"。
// 真实 PTY 与真实 jobs 服务在 cmd/dshd/serve_terminal_api_test.go 里验。
type fakeOfficialJobTerminal struct {
	listFrame    OfficialJobListFrame
	listErr      error
	listSessions []string

	killValue    OfficialJobKillValue
	killErr      error
	killCalls    []string
	killCtxSeen  bool
	followFrame  OfficialJobFollowFrame
	followErr    error
	followFrom   []int64
	followScopes []string

	createInfo  OfficialWebTerminalInfo
	createErr   error
	createReqs  []OfficialTerminalCreateRequest
	attachSnap  OfficialTerminalSnapshot
	attachErr   error
	terminals   []OfficialWebTerminalInfo
	listTErr    error
	closeErr    error
	closeIDs    []string
	env         OfficialTerminalEnvironment
	envErr      error
	shells      []OfficialTerminalShell
	shellsErr   error
	renameErr   error
	renameCalls []string
	retainErr   error
	retainIDs   []string
}

func (f *fakeOfficialJobTerminal) JobList(sessionID string) (OfficialJobListFrame, error) {
	f.listSessions = append(f.listSessions, sessionID)
	return f.listFrame, f.listErr
}

func (f *fakeOfficialJobTerminal) JobKill(ctx context.Context, sessionID, jobID string) (OfficialJobKillValue, error) {
	if ctx == nil {
		panic("handler 必须传非 nil ctx")
	}
	f.killCtxSeen = true
	f.killCalls = append(f.killCalls, sessionID+"/"+jobID)
	return f.killValue, f.killErr
}

func (f *fakeOfficialJobTerminal) JobFollow(ctx context.Context, sessionID, jobID string, from int64) (OfficialJobFollowFrame, error) {
	f.followScopes = append(f.followScopes, sessionID+"/"+jobID)
	f.followFrom = append(f.followFrom, from)
	return f.followFrame, f.followErr
}

func (f *fakeOfficialJobTerminal) TerminalCreate(ctx context.Context, sessionID string, req OfficialTerminalCreateRequest) (OfficialWebTerminalInfo, error) {
	f.createReqs = append(f.createReqs, req)
	return f.createInfo, f.createErr
}

func (f *fakeOfficialJobTerminal) TerminalAttach(ctx context.Context, sessionID, id string) (OfficialTerminalSnapshot, error) {
	return f.attachSnap, f.attachErr
}

func (f *fakeOfficialJobTerminal) TerminalList(sessionID string) ([]OfficialWebTerminalInfo, error) {
	return f.terminals, f.listTErr
}

func (f *fakeOfficialJobTerminal) TerminalClose(ctx context.Context, sessionID, id string) error {
	f.closeIDs = append(f.closeIDs, sessionID+"/"+id)
	return f.closeErr
}

func (f *fakeOfficialJobTerminal) TerminalEnvironment(ctx context.Context, sessionID string) (OfficialTerminalEnvironment, error) {
	return f.env, f.envErr
}

func (f *fakeOfficialJobTerminal) TerminalShells(ctx context.Context, sessionID string) ([]OfficialTerminalShell, error) {
	return f.shells, f.shellsErr
}

func (f *fakeOfficialJobTerminal) TerminalRename(ctx context.Context, sessionID, id, title string) error {
	f.renameCalls = append(f.renameCalls, sessionID+"/"+id+"/"+title)
	return f.renameErr
}

func (f *fakeOfficialJobTerminal) TerminalRetain(ctx context.Context, sessionID, id string) error {
	f.retainIDs = append(f.retainIDs, sessionID+"/"+id)
	return f.retainErr
}

func newFakeOfficialJobTerminal() *fakeOfficialJobTerminal {
	return &fakeOfficialJobTerminal{}
}

// officialPost 直接进 handler（不经 handleAPI 的 switch），并在必要时断言四字段错误体。
func officialPost(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// errEnvelope 解析四字段错误体；不是错误体就 Fatal。
func errEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误体不是 JSON：%v（body=%s）", err, rec.Body.String())
	}
	if env.Error == nil {
		t.Fatalf("没有 error 字段（body=%s）", rec.Body.String())
	}
	for _, k := range []string{"code", "message", "hint", "retryable"} {
		if _, ok := env.Error[k]; !ok {
			t.Fatalf("错误体缺四字段里的 %q：%s", k, rec.Body.String())
		}
	}
	if hint, _ := env.Error["hint"].(string); strings.TrimSpace(hint) == "" {
		t.Fatalf("hint 为空：客户端不知道下一步该做什么（body=%s）", rec.Body.String())
	}
	return env.Error
}

// ---------------------------------------------------------------------------
// 路由现状：官方 11 个方法现在**没有** case（Lead 待补）。
// ---------------------------------------------------------------------------

// TestOfficialJobTerminal_JobTerminalRouteStatus 不假设路由已经接上：
//   - 现在（未接线）打 /api/job.list 会落到 server.go 的 default → 501 not_implemented；
//     这条测试把这件事**记下来**并继续绿，而不是让它变成一条会过期的红。
//   - 接线之后同一个探针会自动升级成形状断言。
func TestOfficialJobTerminal_JobTerminalRouteStatus(t *testing.T) {
	s := newTestServer(t, Options{JobsList: func() []JobInfo { return nil }})
	rec := postJSON(t, s, "/api/job.list", `{"sessionId":"s1"}`)
	if rec.Code == http.StatusNotImplemented {
		code, _ := errEnvelope(t, rec)["code"].(string)
		if code != "not_implemented" {
			t.Fatalf("未接线时应当是 not_implemented，拿到 %q", code)
		}
		t.Logf("RED 基线：/api/job.list 当前未接线（%d %s）——Lead 补 server.go 的 case 后本测试自动转为形状断言", rec.Code, code)
		return
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("接线后要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["type"] != "rows" {
		t.Fatalf("接线后 job.list 的 result 必须是 {type:'rows',jobs:[…]}，拿到 %v", got)
	}
}

// TestExistingJobsListShapeUntouched 钉住"不删改既有方法"。
//
// 官方 job.list 与既有 jobs.list 必须**并存**：后者是 TUI 的生产依赖，
// 形状一个字段都不许动（这里连 "type" 这个新字段都不许冒出来）。
func TestExistingJobsListShapeUntouched(t *testing.T) {
	s := newTestServer(t, Options{JobsList: func() []JobInfo {
		return []JobInfo{{ID: "job-1", SessionID: "s1", Command: "sleep 1", Done: false, Bytes: 12}}
	}})
	rec := postJSON(t, s, "/api/jobs.list", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("既有 jobs.list 要 200，拿到 %d", rec.Code)
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if _, has := got["type"]; has {
		t.Fatalf("既有 jobs.list 不许长出官方专用字段 type：%v", got)
	}
	jobs, _ := got["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("既有 jobs.list 要回 1 行，拿到 %v", got)
	}
	row, _ := jobs[0].(map[string]any)
	for _, legacy := range []string{"id", "sessionId", "command", "done", "exit_code", "bytes"} {
		if _, ok := row[legacy]; !ok {
			t.Fatalf("既有 jobs.list 行少了旧字段 %q：%v", legacy, row)
		}
	}
	for _, officialOnly := range []string{"kind", "label", "status", "startedAt", "output"} {
		if _, ok := row[officialOnly]; ok {
			t.Fatalf("既有 jobs.list 行不许混入官方字段 %q：%v", officialOnly, row)
		}
	}
}

// ---------------------------------------------------------------------------
// job.list
// ---------------------------------------------------------------------------

func TestOfficialJobTerminal_JobListFrameShape(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.listFrame = OfficialJobListFrame{
		Type: "rows",
		Jobs: []OfficialJobView{
			{
				ID:        "job-7",
				Kind:      "shell",
				Label:     "npm test",
				Owner:     "s1",
				Status:    "running",
				StartedAt: 1730000000000,
				Output:    OfficialJobOutput{Total: 42},
			},
			{
				// owner/finishedAt/progress/detail/outputLimitBytes 都是可选字段：
				// 空值必须**不出现**，否则官方 zod（optional）也认，但界面会画出空标签。
				ID:        "job-8",
				Kind:      "shell",
				Label:     "true",
				Status:    "completed",
				StartedAt: 1730000000001,
				Output:    OfficialJobOutput{Total: 0},
			},
		},
	}
	h := (&OfficialJobTerminal{Svc: f}).HandleJobList

	rec := officialPost(t, h, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if len(f.listSessions) != 1 || f.listSessions[0] != "s1" {
		t.Fatalf("sessionId 没传到底：%v", f.listSessions)
	}
	var env struct {
		Result struct {
			Type string `json:"type"`
			Jobs []map[string]any
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Type != "rows" {
		t.Fatalf("type 必须是 rows，拿到 %q", env.Result.Type)
	}
	if len(env.Result.Jobs) != 2 {
		t.Fatalf("要回 2 行，拿到 %d", len(env.Result.Jobs))
	}
	for _, k := range []string{"id", "kind", "label", "status", "startedAt", "output"} {
		if _, ok := env.Result.Jobs[0][k]; !ok {
			t.Fatalf("JobView 缺必填字段 %q：%v", k, env.Result.Jobs[0])
		}
	}
	out, _ := env.Result.Jobs[0]["output"].(map[string]any)
	if out == nil {
		t.Fatalf("output 必须是对象：%v", env.Result.Jobs[0])
	}
	for _, k := range []string{"total", "earliest"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("output 缺必填字段 %q：%v", k, out)
		}
	}
	if _, ok := env.Result.Jobs[1]["owner"]; ok {
		t.Fatalf("未归属的 job 不许带 owner：%v", env.Result.Jobs[1])
	}
}

// 空清单必须是 []，不能是 null：官方 zod 是 z.array，null 会让客户端整段不画。
func TestOfficialJobTerminal_JobListEmptyIsArray(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.listFrame = OfficialJobListFrame{Type: "rows"}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobList, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"jobs":[]`) {
		t.Fatalf("空清单必须编成 []：%s", rec.Body.String())
	}
}

func TestOfficialJobTerminal_JobListParamErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空对象", `{}`},
		{"sessionId 空串", `{"sessionId":""}`},
		{"null", `null`},
		{"数组", `[]`},
		{"未知字段", `{"sessionId":"s1","bogus":true}`},
		{"类型错", `{"sessionId":42}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeOfficialJobTerminal()
			rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobList, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("要 400，拿到 %d：%s", rec.Code, rec.Body.String())
			}
			if code, _ := errEnvelope(t, rec)["code"].(string); code != "bad_request" {
				t.Fatalf("code 要是 bad_request，拿到 %q", code)
			}
			if len(f.listSessions) != 0 {
				t.Fatalf("参数不合法时不许打到底层服务：%v", f.listSessions)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// job.kill
// ---------------------------------------------------------------------------

func TestOfficialJobTerminal_JobKillOutcomes(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.killValue = OfficialJobKillValue{Outcome: "requested"}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobKill, `{"sessionId":"s1","jobId":"job-7"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if got := unwrapResult(t, rec.Body.Bytes()); got["outcome"] != "requested" {
		t.Fatalf("outcome 必须是 requested，拿到 %v", got)
	}
	if !f.killCtxSeen {
		t.Fatal("job.kill 必须带 ctx 到底层服务")
	}
	if len(f.killCalls) != 1 || f.killCalls[0] != "s1/job-7" {
		t.Fatalf("sessionId/jobId 没传对：%v", f.killCalls)
	}
}

// 错误映射是四字段契约的核心：同一个"任务已结束"，必须稳定落 409 job_finished，
// 而不是 500——客户端据此换分支（去 jobs.output 读保留输出），而不是重试。
func TestOfficialJobTerminal_JobKillErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"已结束", fmt.Errorf("%w: done", ErrJobFinished), http.StatusConflict, "job_finished"},
		{"跨会话", fmt.Errorf("%w: other", ErrCrossSession), http.StatusForbidden, "cross_session"},
		{"不存在", fmt.Errorf("%w: nope", ErrNoSuchJob), http.StatusNotFound, "not_found"},
		{"底层失败", errors.New("子进程没应答"), http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeOfficialJobTerminal()
			f.killErr = c.err
			rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobKill, `{"sessionId":"s1","jobId":"job-7"}`)
			if rec.Code != c.status {
				t.Fatalf("要 %d，拿到 %d：%s", c.status, rec.Code, rec.Body.String())
			}
			if code, _ := errEnvelope(t, rec)["code"].(string); code != c.code {
				t.Fatalf("code 要是 %q，拿到 %q", c.code, code)
			}
		})
	}
}

func TestOfficialJobTerminal_JobKillParamErrors(t *testing.T) {
	cases := []string{
		`{}`,
		`{"sessionId":"s1"}`,
		`{"jobId":"job-7"}`,
		`{"sessionId":"s1","jobId":""}`,
		`{"sessionId":"s1","jobId":"job-7","nope":1}`,
	}
	for _, body := range cases {
		f := newFakeOfficialJobTerminal()
		rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobKill, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d：%s", body, rec.Code, rec.Body.String())
		}
		errEnvelope(t, rec)
		if len(f.killCalls) != 0 {
			t.Fatalf("参数不合法时不许打到底层：%v", f.killCalls)
		}
	}
}

// 既有 jobs.stop 收的是 {sessionId,id}；官方收 {sessionId,jobId}。两边都要认，
// 免得界面按旧写法发过来被判 400（它不会去读 400 的 body）。
func TestOfficialJobTerminal_JobKillAcceptsIDAlias(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.killValue = OfficialJobKillValue{Outcome: "requested"}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobKill, `{"sessionId":"s1","id":"job-9"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("id 别名要能用，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if len(f.killCalls) != 1 || f.killCalls[0] != "s1/job-9" {
		t.Fatalf("别名没解析成 jobId：%v", f.killCalls)
	}
	// 两个都给且不一致 → 必须 fail-loud，不许猜哪个是真的。
	rec = officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobKill, `{"sessionId":"s1","jobId":"a","id":"b"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("jobId 与 id 冲突要 400，拿到 %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// job.follow
// ---------------------------------------------------------------------------

// 官方 job.follow 是流；本宿主 HTTP 侧**只做一次性读取**。
// 断言点：一帧、带 oneShot 标记、不出现任何流式头（SSE/chunked 伪装）。
func TestOfficialJobTerminal_JobFollowIsOneShotNotFakeStream(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.followFrame = OfficialJobFollowFrame{
		Type: "output",
		Chunks: []OfficialJobChunk{
			{At: 16, Text: "line-2\n"},
			{At: 23, Text: "line-3\n", GapBefore: true},
		},
		Next: int64Ptr(30),
	}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobFollow, `{"sessionId":"s1","jobId":"job-7","from":16}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Fatalf("HTTP 侧不许假装流式（Content-Type=%q）", ct)
	}
	if len(f.followFrom) != 1 || f.followFrom[0] != 16 {
		t.Fatalf("from 没传到底：%v", f.followFrom)
	}
	var env struct {
		Result struct {
			Type    string `json:"type"`
			Chunks  []map[string]any
			Next    *int64 `json:"next"`
			OneShot bool   `json:"oneShot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Type != "output" {
		t.Fatalf("type 要是 output，拿到 %q", env.Result.Type)
	}
	if len(env.Result.Chunks) != 2 {
		t.Fatalf("要 2 个 chunk，拿到 %d", len(env.Result.Chunks))
	}
	for _, k := range []string{"at", "text"} {
		if _, ok := env.Result.Chunks[0][k]; !ok {
			t.Fatalf("chunk 缺必填 %q：%v", k, env.Result.Chunks[0])
		}
	}
	if _, ok := env.Result.Chunks[1]["gapBefore"]; !ok {
		t.Fatalf("gapBefore=true 必须出现：%v", env.Result.Chunks[1])
	}
	if env.Result.Next == nil || *env.Result.Next != 30 {
		t.Fatalf("next 必须是 30，拿到 %v", env.Result.Next)
	}
	if !env.Result.OneShot {
		t.Fatal("一次性读取必须显式标 oneShot=true：不许冒充流")
	}
}

// 已结算且排干 → status 帧（终态投影）。
func TestOfficialJobTerminal_JobFollowStatusFrame(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.followFrame = OfficialJobFollowFrame{
		Type: "status",
		Job: &OfficialJobView{ID: "job-7", Kind: "shell", Label: "npm test",
			Status: "completed", StartedAt: 1730000000000,
			Output: OfficialJobOutput{Total: 30}},
	}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobFollow, `{"sessionId":"s1","jobId":"job-7"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			Type string         `json:"type"`
			Job  map[string]any `json:"job"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Type != "status" || env.Result.Job == nil {
		t.Fatalf("要 status 帧且带 job：%s", rec.Body.String())
	}
}

// from 带负数或非数字 → 400；jobId 缺失 → 400。
func TestOfficialJobTerminal_JobFollowParamErrors(t *testing.T) {
	for _, body := range []string{`{}`, `{"jobId":"j"}`, `{"sessionId":"s1","jobId":"j","from":-1}`,
		`{"sessionId":"s1","jobId":"j","from":"x"}`, `{"sessionId":"s1","jobId":"j","zzz":1}`} {
		f := newFakeOfficialJobTerminal()
		rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleJobFollow, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d：%s", body, rec.Code, rec.Body.String())
		}
		errEnvelope(t, rec)
		if len(f.followScopes) != 0 {
			t.Fatalf("参数不合法时不许打到底层：%v", f.followScopes)
		}
	}
}

// ---------------------------------------------------------------------------
// terminal.create / list / attach / close / environment / shells / rename / retain
// ---------------------------------------------------------------------------

func TestOfficialJobTerminal_TerminalCreateShape(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.createInfo = OfficialWebTerminalInfo{
		ID: "term-a", Title: "bash", Cwd: "/tmp",
		Shell: OfficialTerminalShell{Path: "/bin/bash", Args: []string{}, Name: "bash"},
		Cols:  120, Rows: 40, State: "running", ExitCode: nil,
	}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalCreate,
		`{"sessionId":"s1","id":"term-a","cols":120,"rows":40,"shellPath":"/bin/bash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if len(f.createReqs) != 1 {
		t.Fatalf("要打一次 create，拿到 %d", len(f.createReqs))
	}
	if got := f.createReqs[0]; got.ID != "term-a" || got.Cols != 120 || got.Rows != 40 || got.ShellPath != "/bin/bash" {
		t.Fatalf("create 参数没传对：%+v", got)
	}
	var env struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "title", "shell", "cwd", "cols", "rows", "state", "exitCode"} {
		if _, ok := env.Result[k]; !ok {
			t.Fatalf("WebTerminalInfo 缺必填字段 %q（exitCode 可以为 null 但必须存在）：%v", k, env.Result)
		}
	}
	shell, _ := env.Result["shell"].(map[string]any)
	for _, k := range []string{"path", "args", "name"} {
		if _, ok := shell[k]; !ok {
			t.Fatalf("TerminalShell 缺必填 %q：%v", k, shell)
		}
	}
}

// 官方把 create 的参数放在 request 里（agent 走 lookup）。两种形状都认，
// 免得官方客户端把 {request:{…}} 发过来时被当成"未知字段"。
func TestOfficialJobTerminal_TerminalCreateAcceptsNestedRequest(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.createInfo = OfficialWebTerminalInfo{ID: "term-b", State: "running", Shell: OfficialTerminalShell{Path: "x", Args: []string{}, Name: "x"}}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalCreate,
		`{"agentId":"s1","request":{"id":"term-b","cols":80,"rows":24}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("嵌套 request 要能用，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if len(f.createReqs) != 1 || f.createReqs[0].ID != "term-b" || f.createReqs[0].Cols != 80 {
		t.Fatalf("嵌套 request 没解析对：%+v", f.createReqs)
	}
}

func TestOfficialJobTerminal_TerminalCreateParamErrors(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"sessionId":"s1"}`,
		`{"sessionId":"s1","id":"t"}`,
		`{"sessionId":"s1","id":"t","cols":80}`,
		`{"sessionId":"s1","id":"","cols":80,"rows":24}`,
		`{"sessionId":"s1","id":"t","cols":0,"rows":24}`,
		`{"sessionId":"s1","id":"t","cols":80,"rows":24,"bogus":1}`,
		`{"sessionId":"s1","request":{"id":"t","cols":80,"rows":24},"bogus":1}`,
	} {
		f := newFakeOfficialJobTerminal()
		rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalCreate, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d：%s", body, rec.Code, rec.Body.String())
		}
		errEnvelope(t, rec)
		if len(f.createReqs) != 0 {
			t.Fatalf("参数不合法时不许起终端：%v", f.createReqs)
		}
	}
}

// terminal.list 的 result 是**数组**（不是 {terminals:[…]}）。
func TestOfficialJobTerminal_TerminalListIsArray(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.terminals = []OfficialWebTerminalInfo{{
		ID: "term-a", Title: "bash", State: "running",
		Shell: OfficialTerminalShell{Path: "/bin/bash", Args: []string{}, Name: "bash"},
		Cwd:   "/tmp", Cols: 120, Rows: 40,
	}}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalList, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("result 必须是数组：%v（body=%s）", err, rec.Body.String())
	}
	if len(env.Result) != 1 || env.Result[0]["id"] != "term-a" {
		t.Fatalf("要回 1 条 term-a：%v", env.Result)
	}

	f2 := newFakeOfficialJobTerminal()
	rec = officialPost(t, (&OfficialJobTerminal{Svc: f2}).HandleTerminalList, `{"sessionId":"s1"}`)
	if !strings.Contains(rec.Body.String(), `"result":[]`) {
		t.Fatalf("空清单必须编成 []：%s", rec.Body.String())
	}
}

func TestOfficialJobTerminal_TerminalAttachSnapshotShape(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.attachSnap = OfficialTerminalSnapshot{
		Type: "snapshot", Sequence: 128, Screen: "$ echo hi\nhi\n",
		Info:     OfficialWebTerminalInfo{ID: "term-a", Title: "bash", State: "running", Shell: OfficialTerminalShell{Path: "x", Args: []string{}, Name: "x"}},
		Writable: true,
	}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalAttach, `{"sessionId":"s1","id":"term-a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			Type     string         `json:"type"`
			Sequence int64          `json:"sequence"`
			Screen   string         `json:"screen"`
			Writable bool           `json:"writable"`
			Info     map[string]any `json:"info"`
			OneShot  bool           `json:"oneShot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Type != "snapshot" || env.Result.Sequence != 128 || env.Result.Info == nil || !env.Result.Writable {
		t.Fatalf("snapshot 帧字段不对：%s", rec.Body.String())
	}
	if !env.Result.OneShot {
		t.Fatal("attach 是一次性快照，必须标 oneShot=true")
	}

	for _, body := range []string{`{}`, `{"id":"term-a"}`, `{"sessionId":"s1","id":""}`} {
		rec := officialPost(t, (&OfficialJobTerminal{Svc: newFakeOfficialJobTerminal()}).HandleTerminalAttach, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d", body, rec.Code)
		}
	}
	fErr := newFakeOfficialJobTerminal()
	fErr.attachErr = fmt.Errorf("%w: term-a", ErrJTOfficialTerminalGone)
	rec = officialPost(t, (&OfficialJobTerminal{Svc: fErr}).HandleTerminalAttach, `{"sessionId":"s1","id":"term-a"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("终端不在了要 404，拿到 %d：%s", rec.Code, rec.Body.String())
	}
}

func TestOfficialJobTerminal_TerminalCloseResultAndGone(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalClose, `{"sessionId":"s1","id":"term-a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if len(f.closeIDs) != 1 || f.closeIDs[0] != "s1/term-a" {
		t.Fatalf("close 参数没传对：%v", f.closeIDs)
	}
	// 官方：repeated closes succeed。第二次同样 200。
	if rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalClose, `{"sessionId":"s1","id":"term-a"}`); rec.Code != http.StatusOK {
		t.Fatalf("重复 close 要 200（官方语义），拿到 %d", rec.Code)
	}

	fErr := newFakeOfficialJobTerminal()
	fErr.closeErr = fmt.Errorf("%w: term-z", ErrJTOfficialTerminalGone)
	rec = officialPost(t, (&OfficialJobTerminal{Svc: fErr}).HandleTerminalClose, `{"sessionId":"s1","id":"term-z"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不认识的终端要 404，拿到 %d：%s", rec.Code, rec.Body.String())
	}

	fCross := newFakeOfficialJobTerminal()
	fCross.closeErr = fmt.Errorf("%w: 别的会话", ErrCrossSession)
	rec = officialPost(t, (&OfficialJobTerminal{Svc: fCross}).HandleTerminalClose, `{"sessionId":"s1","id":"term-b"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨会话终端要 403，拿到 %d：%s", rec.Code, rec.Body.String())
	}
}

func TestOfficialJobTerminal_TerminalEnvironmentShape(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.env = OfficialTerminalEnvironment{
		Cwd: "/d/work", MaxInputBytes: 1 << 16, MaxBufferedBytes: 1 << 18,
		MaxCols: 500, MaxRows: 200, Scrollback: 0,
	}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalEnvironment, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"cwd", "maxInputBytes", "maxBufferedBytes", "maxCols", "maxRows", "scrollback"} {
		if _, ok := env.Result[k]; !ok {
			t.Fatalf("TerminalEnvironment 缺必填字段 %q：%v", k, env.Result)
		}
	}
	if env.Result["cwd"] != "/d/work" {
		t.Fatalf("cwd 要如实转回：%v", env.Result)
	}
}

func TestOfficialJobTerminal_TerminalShellsShape(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.shells = []OfficialTerminalShell{
		{Path: "C:/Program Files/Git/bin/bash.exe", Args: []string{}, Name: "bash.exe"},
		{Path: "C:/Windows/System32/cmd.exe", Args: []string{}, Name: "cmd.exe"},
	}
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalShells, `{"sessionId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("result 必须是数组：%v", err)
	}
	if len(env.Result) != 2 {
		t.Fatalf("要回 2 个 shell，拿到 %d", len(env.Result))
	}
	for _, k := range []string{"path", "args", "name"} {
		if _, ok := env.Result[0][k]; !ok {
			t.Fatalf("TerminalShell 缺必填 %q：%v", k, env.Result[0])
		}
	}
	if _, ok := env.Result[0]["args"].([]any); !ok {
		t.Fatalf("args 必须是数组：%v", env.Result[0]["args"])
	}
}

// rename 的 title：非空、≤120 字符（官方 index.ts:229 的契约）。
func TestOfficialJobTerminal_TerminalRenameValidation(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalRename,
		`{"sessionId":"s1","id":"term-a","title":"构建日志"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if len(f.renameCalls) != 1 || f.renameCalls[0] != "s1/term-a/构建日志" {
		t.Fatalf("rename 参数没传对：%v", f.renameCalls)
	}

	long := strings.Repeat("字", 121)
	cases := []string{
		`{"sessionId":"s1","id":"term-a","title":""}`,
		`{"sessionId":"s1","id":"term-a"}`,
		`{"sessionId":"s1","id":"term-a","title":"   "}`,
		`{"sessionId":"s1","id":"term-a","title":"` + long + `"}`,
		`{"sessionId":"s1","id":"term-a","title":"x","bogus":1}`,
	}
	f2 := newFakeOfficialJobTerminal()
	for _, body := range cases {
		rec := officialPost(t, (&OfficialJobTerminal{Svc: f2}).HandleTerminalRename, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d：%s", body, rec.Code, rec.Body.String())
		}
		errEnvelope(t, rec)
	}
	if len(f2.renameCalls) != 0 {
		t.Fatalf("参数不合法时不许打到底层：%v", f2.renameCalls)
	}
}

func TestOfficialJobTerminal_TerminalRetainFrame(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalRetain, `{"sessionId":"s1","id":"term-a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			Type    string `json:"type"`
			OneShot bool   `json:"oneShot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Type != "retained" {
		t.Fatalf("retain 的帧 type 必须是 retained，拿到 %q", env.Result.Type)
	}
	if !env.Result.OneShot {
		t.Fatal("retain 是一次性确认，必须标 oneShot=true（本宿主没有保留池流）")
	}
	if len(f.retainIDs) != 1 || f.retainIDs[0] != "s1/term-a" {
		t.Fatalf("retain 参数没传对：%v", f.retainIDs)
	}
}

// 未实现的能力必须回 501 + 明确原因，不能回假数据。
func TestOfficialJobTerminal_UnsupportedCapabilityIs501(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.retainErr = fmt.Errorf("%w: 保留池未实现", ErrJTOfficialUnsupported)
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalRetain, `{"sessionId":"s1","id":"term-a"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("未实现要 501，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if code, _ := errEnvelope(t, rec)["code"].(string); code != "not_implemented" {
		t.Fatalf("code 要是 not_implemented，拿到 %q", code)
	}
	if _, has := unwrapResultOK(t, rec.Body.Bytes()); has {
		t.Fatal("501 不许带 result（那就是假数据）")
	}
}

// unwrapResultOK 判断 body 里有没有 result 键。
func unwrapResultOK(t *testing.T, raw []byte) (any, bool) {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false
	}
	v, ok := env["result"]
	return v, ok
}

// 没接线（Svc=nil）时，11 个方法都要回 501，且错误体是四字段。
func TestOfficialJobTerminal_AllMethodsUnwiredIs501(t *testing.T) {
	h := &OfficialJobTerminal{}
	cases := []struct {
		method string
		fn     http.HandlerFunc
		body   string
	}{
		{"job.list", h.HandleJobList, `{"sessionId":"s1"}`},
		{"job.kill", h.HandleJobKill, `{"sessionId":"s1","jobId":"j"}`},
		{"job.follow", h.HandleJobFollow, `{"sessionId":"s1","jobId":"j"}`},
		{"terminal.create", h.HandleTerminalCreate, `{"sessionId":"s1","id":"t","cols":80,"rows":24}`},
		{"terminal.attach", h.HandleTerminalAttach, `{"sessionId":"s1","id":"t"}`},
		{"terminal.list", h.HandleTerminalList, `{"sessionId":"s1"}`},
		{"terminal.close", h.HandleTerminalClose, `{"sessionId":"s1","id":"t"}`},
		{"terminal.environment", h.HandleTerminalEnvironment, `{"sessionId":"s1"}`},
		{"terminal.shells", h.HandleTerminalShells, `{"sessionId":"s1"}`},
		{"terminal.rename", h.HandleTerminalRename, `{"sessionId":"s1","id":"t","title":"x"}`},
		{"terminal.retain", h.HandleTerminalRetain, `{"sessionId":"s1","id":"t"}`},
	}
	for _, c := range cases {
		rec := officialPost(t, c.fn, c.body)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s 未接线要 501，拿到 %d：%s", c.method, rec.Code, rec.Body.String())
		}
		err := errEnvelope(t, rec)
		if err["code"] != "not_implemented" {
			t.Fatalf("%s 的 code 要是 not_implemented，拿到 %v", c.method, err["code"])
		}
		if msg, _ := err["message"].(string); !strings.Contains(msg, c.method) {
			t.Fatalf("%s 的错误消息要点名方法（现在 %q）", c.method, msg)
		}
	}
}

// Handles/Serve 是给 Lead 的**单点接线**入口：server.go 的 default 分支里
// 一行就能把 11 个方法全部接上，不必写 11 个 case。
func TestOfficialJobTerminal_HandlesAndServeDispatch(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	f.listFrame = OfficialJobListFrame{Type: "rows", Jobs: []OfficialJobView{}}
	f.env = OfficialTerminalEnvironment{Cwd: "/d/work"}
	h := &OfficialJobTerminal{Svc: f}

	for _, m := range OfficialJobTerminalMethods() {
		if !h.Handles(string(m)) {
			t.Fatalf("Handles(%q) 要为 true", m)
		}
	}
	for _, notOurs := range []string{"jobs.list", "jobs.stop", "terminal.open", "session.list", ""} {
		if h.Handles(notOurs) {
			t.Fatalf("Handles(%q) 必须为 false（不许抢既有方法的路由）", notOurs)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/terminal.environment", strings.NewReader(`{"sessionId":"s1"}`))
	rec := httptest.NewRecorder()
	h.Serve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Serve 要按路径里的方法名分发，拿到 %d：%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/job.list", strings.NewReader(`{"sessionId":"s1"}`))
	rec = httptest.NewRecorder()
	h.Serve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Serve /api/job.list 要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}

	// 不认识的方法不许被 Serve 静默吞掉。
	req = httptest.NewRequest(http.MethodPost, "/api/jobs.list", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	h.Serve(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("Serve 遇到非官方方法要 501，拿到 %d", rec.Code)
	}
}

// 参数别名冲突必须 fail-loud：sessionId 与 agentId 同时给且不一致时，
// 猜任何一个都会把 A 会话的操作打到 B 会话上。
func TestOfficialJobTerminal_IdentityConflictFailsLoud(t *testing.T) {
	f := newFakeOfficialJobTerminal()
	rec := officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalList, `{"sessionId":"a","agentId":"b"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("身份冲突要 400，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	errEnvelope(t, rec)

	rec = officialPost(t, (&OfficialJobTerminal{Svc: f}).HandleTerminalList, `{"agentId":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("agentId 别名要能用，拿到 %d：%s", rec.Code, rec.Body.String())
	}
}
