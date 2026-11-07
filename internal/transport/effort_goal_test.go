package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestEffortGoalAdditiveHTTPMethodsAndErrorBranches(t *testing.T) {
	if len(SessionMethods()) != 25 || len(EffortGoalMethods()) != 4 {
		t.Fatalf("canonical/additive methods=%d/%d", len(SessionMethods()), len(EffortGoalMethods()))
	}
	var effortCalls, pauseCalls, resumeCalls int
	server := newTestServer(t, Options{
		SetSessionEffort: func(id, effort string) error {
			effortCalls++
			if id != "session-a" || (effort != "deep" && effort != "") {
				return ErrEffortInvalid
			}
			return nil
		},
		GoalGet: func(id string) (GoalResult, error) {
			if id == "persist-failure" {
				return GoalResult{}, ErrStatePersistence
			}
			zero := 0
			return GoalResult{Goal: &GoalSnapshot{
				ID: "goal-native", Revision: 1, Objective: "ship", Phase: "active", MaxGoalRounds: 0,
			}, RoundsStarted: &zero}, nil
		},
		GoalPause: func(string) error {
			pauseCalls++
			return nil
		},
		GoalResume: func(string) error {
			resumeCalls++
			return nil
		},
	})

	recorder := postJSON(t, server, "/api/session.setEffort", `{"id":"session-a","effort":"deep"}`)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "{\"result\":{\"ok\":true}}\n" {
		t.Fatalf("setEffort response=%d %s", recorder.Code, recorder.Body.String())
	}
	for _, body := range []string{
		`{"effort":"deep"}`, `{"id":"session-a"}`, `{"id":"session-a","effort":null}`,
		`{"id":"session-a","effort":3}`, `{"id":"session-a","effort":"wire-deep"}`, `{`,
	} {
		recorder = postJSON(t, server, "/api/session.setEffort", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid effort body=%s status=%d body=%s", body, recorder.Code, recorder.Body.String())
		}
		scanErrorResponse(t, "setEffort "+body, recorder.Code, recorder.Header(), recorder.Body.Bytes())
	}
	if effortCalls != 2 { // deep + wire-deep 进入 callback；其余必须在参数门拒绝。
		t.Fatalf("effort callback calls=%d", effortCalls)
	}
	recorder = postJSON(t, server, "/api/session.goal.get", `{"id":"session-a"}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"goal-native"`) || !strings.Contains(recorder.Body.String(), `"roundsStarted":0`) {
		t.Fatalf("goal.get response=%d %s", recorder.Code, recorder.Body.String())
	}
	for _, method := range []string{"session.goal.pause", "session.goal.resume"} {
		recorder = postJSON(t, server, "/api/"+method, `{"id":"session-a"}`)
		if recorder.Code != http.StatusOK || recorder.Body.String() != "{\"result\":{\"ok\":true}}\n" {
			t.Fatalf("%s response=%d %s", method, recorder.Code, recorder.Body.String())
		}
	}
	if pauseCalls != 1 || resumeCalls != 1 {
		t.Fatalf("pause/resume calls=%d/%d", pauseCalls, resumeCalls)
	}
	recorder = postJSON(t, server, "/api/session.goal.get", `{"id":"persist-failure"}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("goal persistence status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := scanErrorResponse(t, "goal persistence", recorder.Code, recorder.Header(), recorder.Body.Bytes()); code != "internal" {
		t.Fatalf("goal persistence code=%q", code)
	}
}

func TestEffortGoalNilCallbacksKeepClosedDispatch(t *testing.T) {
	server := newTestServer(t, Options{})
	for _, method := range EffortGoalMethods() {
		recorder := postJSON(t, server, "/api/"+string(method), `{"id":"session-a","effort":""}`)
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("%s status=%d body=%s", method, recorder.Code, recorder.Body.String())
		}
		scanErrorResponse(t, string(method), recorder.Code, recorder.Header(), recorder.Body.Bytes())
	}
}

type effortSelectionSource struct {
	selection string
	err       error
}

func (s effortSelectionSource) Messages(string) ([]Message, error) { return []Message{}, nil }
func (s effortSelectionSource) Effort(string) (string, error)      { return "high", nil }
func (s effortSelectionSource) EffortSelection(string) (string, error) {
	return s.selection, s.err
}

type effortSelectionTailSource struct{ effortSelectionSource }

func (s effortSelectionTailSource) MessagesTail(string, int) ([]Message, int, error) {
	return []Message{}, 0, nil
}

func TestLoadSessionEffortSelectionBothSourcePaths(t *testing.T) {
	for _, source := range []Source{
		effortSelectionSource{selection: "off"},
		effortSelectionTailSource{effortSelectionSource{selection: "off"}},
	} {
		result, err := LoadSession(source, "session-a", 10)
		if err != nil {
			t.Fatal(err)
		}
		if result.Effort != "high" || result.EffortSelection != "off" {
			t.Fatalf("effort/selection must remain independent: %+v", result)
		}
	}
	for _, source := range []Source{
		effortSelectionSource{},
		effortSelectionTailSource{effortSelectionSource{}},
	} {
		result, err := LoadSession(source, "session-a", 10)
		if err != nil || result.EffortSelection != "" {
			t.Fatalf("no override must omit selection: %+v err=%v", result, err)
		}
	}
	_, err := LoadSession(effortSelectionSource{err: errors.New("store corrupt")}, "session-a", 10)
	if err == nil {
		t.Fatal("corrupt selection store projected as no override")
	}
}

func TestEffortGoalAdditiveSnapshotLeavesCanonicalAlone(t *testing.T) {
	raw, err := os.ReadFile("testdata/effort-goal.snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Canonical int `json:"canonicalMethodsUnchanged"`
		Methods   []struct {
			Name string `json:"name"`
		} `json:"methods"`
		LoadResultKeys []string `json:"loadResultKeys"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Canonical != 25 || len(snapshot.Methods) != len(EffortGoalMethods()) {
		t.Fatalf("snapshot canonical/methods=%d/%d", snapshot.Canonical, len(snapshot.Methods))
	}
	for i, method := range EffortGoalMethods() {
		if snapshot.Methods[i].Name != string(method) {
			t.Fatalf("method[%d]=%q want=%q", i, snapshot.Methods[i].Name, method)
		}
	}
	for _, name := range SessionMethods() {
		for _, additive := range EffortGoalMethods() {
			if name == additive {
				t.Fatalf("additive method leaked into canonical: %q", name)
			}
		}
	}
	wantKeys := jsonKeys(LoadResult{})
	if !reflect.DeepEqual(snapshot.LoadResultKeys, wantKeys) {
		t.Fatalf("load keys=%v want=%v", snapshot.LoadResultKeys, wantKeys)
	}
}

func TestEffortGoalGoldenOperationsReplaySameJournal(t *testing.T) {
	raw, err := os.ReadFile("testdata/effort-goal-golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	journal := NewJournal(41, 0)
	wantOperations := []string{"create", "edit", "pause", "resume", "clear"}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != len(wantOperations) {
		t.Fatalf("golden lines=%d", len(lines))
	}
	for i, line := range lines {
		var frame ServerFrame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatal(err)
		}
		if !allowedTypes[frame.Type] || frame.Type != "goal/change" {
			t.Fatalf("unregistered goal frame: %+v", frame)
		}
		var change GoalChange
		if err := json.Unmarshal(frame.Data, &change); err != nil {
			t.Fatal(err)
		}
		if change.Kind != "goal/change" || change.Version != 1 || change.Operation != wantOperations[i] {
			t.Fatalf("change[%d]=%+v", i, change)
		}
		if change.Operation == "clear" {
			if change.Goal != nil {
				t.Fatalf("clear has snapshot: %+v", change)
			}
		} else if change.Goal == nil || change.Goal.ID == "" || change.Goal.Revision == 0 {
			t.Fatalf("%s missing full snapshot: %+v", change.Operation, change)
		}
		journal.Append(frame.Type, frame.Data, frame.Time)
	}
	frames, last, epoch := journal.Since(0, 0)
	if last != len(lines) || epoch != 41 || len(frames) != len(lines) {
		t.Fatalf("replay frames/last/epoch=%d/%d/%d", len(frames), last, epoch)
	}
	for i, frame := range frames {
		if frame.Seq != i+1 || !strings.Contains(string(frame.Data), `"operation":"`+wantOperations[i]+`"`) {
			t.Fatalf("replayed frame[%d]=%+v", i, frame)
		}
	}
}
