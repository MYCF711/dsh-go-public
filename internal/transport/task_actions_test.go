package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func taskActionRequest(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "task-action-test"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func newTaskActionServer(t *testing.T, opt Options) *Server {
	t.Helper()
	s, err := NewServer(opt)
	if err != nil {
		t.Fatal(err)
	}
	s.cookies["task-action-test"] = struct{}{}
	return s
}

// 旧实现会忽略 cursor/limit 并回整段；这条必须在分页实现前真实变红。
func TestJobsOutputCursorAndLimitAreEnforced(t *testing.T) {
	s := newTaskActionServer(t, Options{JobsOutput: func(id string, wait time.Duration) (JobOutput, error) {
		return JobOutput{ID: id, Output: "abcdef", Done: true}, nil
	}})
	rec := taskActionRequest(t, s, "jobs.output", `{"id":"j1","cursor":2,"limit":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Result["output"] != "cd" || got.Result["cursor"] != float64(2) || got.Result["nextCursor"] != float64(4) || got.Result["hasMore"] != true || got.Result["total"] != float64(6) {
		t.Fatalf("分页不对：%+v", got.Result)
	}
}

func TestJobsOutputCursorUsesRunesWithoutSplittingUTF8(t *testing.T) {
	s := newTaskActionServer(t, Options{JobsOutput: func(id string, wait time.Duration) (JobOutput, error) {
		return JobOutput{ID: id, Output: "甲🙂乙", Done: true}, nil
	}})
	rec := taskActionRequest(t, s, "jobs.output", `{"id":"j1","cursor":1,"limit":1}`)
	var got struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Result["output"] != "🙂" || got.Result["total"] != float64(3) || got.Result["nextCursor"] != float64(2) {
		t.Fatalf("UTF-8 被切坏或游标单位漂移：%+v", got.Result)
	}
}

func TestJobsOutputTruncatedRetentionKeepsRuneContract(t *testing.T) {
	s := newTaskActionServer(t, Options{JobsOutputScoped: func(sessionID, id string, cursor, limit int, wait time.Duration) (JobOutput, error) {
		return PageJobOutput(JobOutput{ID: id, SessionID: sessionID, Output: "甲🙂乙", Done: true, Truncated: true}, cursor, limit), nil
	}})
	rec := taskActionRequest(t, s, "jobs.output", `{"sessionId":"s1","id":"j1","cursor":1,"limit":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Result["output"] != "🙂" || got.Result["total"] != float64(3) || got.Result["nextCursor"] != float64(2) || got.Result["truncated"] != true {
		t.Fatalf("截断窗口混用了字节 total 或丢失 truncated：%+v", got.Result)
	}
}

func TestJobsOutputRejectsCursorBeyondCurrentOutput(t *testing.T) {
	s := newTaskActionServer(t, Options{JobsOutput: func(id string, wait time.Duration) (JobOutput, error) {
		return JobOutput{ID: id, Output: "abc"}, nil
	}})
	rec := taskActionRequest(t, s, "jobs.output", `{"id":"j1","cursor":4,"limit":2}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cursor 越界 status=%d body=%s", rec.Code, rec.Body.String())
	}
	scanErrorResponse(t, "jobs.output cursor", rec.Code, rec.Header(), rec.Body.Bytes())
}

func TestTaskActionArgumentsFailBeforeCapability(t *testing.T) {
	s := newTaskActionServer(t, Options{})
	for _, tc := range []struct {
		method string
		body   string
	}{
		{"schedule.delete", `{}`},
		{"schedule.clear", `{"params":{}}`},
		{"jobs.stop", `{}`},
	} {
		rec := taskActionRequest(t, s, tc.method, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 旧实现/错误参数 status=%d want=400 body=%s", tc.method, rec.Code, rec.Body.String())
		}
		scanErrorResponse(t, tc.method, rec.Code, rec.Header(), rec.Body.Bytes())
	}
}

func TestTaskActionsAdditiveSnapshot(t *testing.T) {
	raw, err := os.ReadFile("testdata/task-actions.snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Canonical int                        `json:"canonicalMethodsUnchanged"`
		Methods   map[string]json.RawMessage `json:"methods"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Canonical != 25 || len(SessionMethods()) != 25 {
		t.Fatalf("canonical 25 被改：snapshot=%d code=%d", snapshot.Canonical, len(SessionMethods()))
	}
	if len(snapshot.Methods) != 4 || len(TaskActionMethods()) != 3 {
		t.Fatalf("additive 方法面漂移：snapshot=%d new=%d", len(snapshot.Methods), len(TaskActionMethods()))
	}
	for _, method := range TaskActionMethods() {
		if snapshot.Methods[string(method)] == nil {
			t.Errorf("snapshot 缺 %s", method)
		}
	}
	for _, method := range []string{"schedule.delete", "schedule.clear", "jobs.output", "jobs.stop"} {
		if snapshot.Methods[method] == nil {
			t.Errorf("snapshot 缺 %s", method)
		}
	}
}

type taskActionGoldenCase struct {
	Name     string         `json:"name"`
	Method   string         `json:"method"`
	Body     map[string]any `json:"body"`
	Status   int            `json:"status"`
	Response map[string]any `json:"response"`
}

type taskActionGoldenError struct {
	kind error
	text string
}

func (e *taskActionGoldenError) Error() string { return e.text }
func (e *taskActionGoldenError) Unwrap() error { return e.kind }

func TestTaskActionsGoldenOverHTTP(t *testing.T) {
	raw, err := os.ReadFile("testdata/task-actions-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []taskActionGoldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 8 {
		t.Fatalf("golden 夹具退化：%d", len(cases))
	}
	s := newTaskActionServer(t, Options{
		ScheduleDelete: func(sessionID, id, expectedRevision string) (ScheduleDeleteResult, error) {
			switch id {
			case "missing":
				return ScheduleDeleteResult{}, &taskActionGoldenError{ErrNoSuchSchedule, "missing schedule"}
			case "conflict":
				return ScheduleDeleteResult{}, &taskActionGoldenError{ErrRevisionConflict, "stale revision"}
			default:
				return ScheduleDeleteResult{SessionID: sessionID, ID: id, Deleted: true, Remaining: 1, Revision: "rev-new"}, nil
			}
		},
		ScheduleClear: func() (ScheduleClearResult, error) {
			return ScheduleClearResult{Deleted: 1, Revisions: map[string]string{"session-a": "rev-clear"}, Schedules: []ScheduleItem{}}, nil
		},
		JobsOutputScoped: func(sessionID, id string, cursor, limit int, wait time.Duration) (JobOutput, error) {
			return PageJobOutput(JobOutput{ID: id, SessionID: sessionID, Output: "abcdef"}, cursor, limit), nil
		},
		JobsStop: func(sessionID, id string) (JobStopResult, error) {
			if sessionID == "session-b" {
				return JobStopResult{}, &taskActionGoldenError{ErrCrossSession, "wrong owner"}
			}
			if id == "ended" {
				return JobStopResult{}, &taskActionGoldenError{ErrJobFinished, "already ended"}
			}
			return JobStopResult{ID: id, SessionID: sessionID, Stopped: true, Done: true, ExitCode: -1}, nil
		},
	})
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			body, _ := json.Marshal(tc.Body)
			rec := taskActionRequest(t, s, tc.Method, string(body))
			if rec.Code != tc.Status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.Status, rec.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.Response) {
				t.Fatalf("响应不合约：\ngot=%+v\nwant=%+v", got, tc.Response)
			}
		})
	}
}
