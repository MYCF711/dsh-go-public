package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/llm"
	"dshgo/internal/transport"
)

type failingGoalJournalPersistence struct{ err error }

func (p *failingGoalJournalPersistence) AppendJournalFrame(string, transport.ServerFrame) error {
	return p.err
}
func (p *failingGoalJournalPersistence) ResetJournal(string) error { return p.err }

func openGoalStoreForTest(t *testing.T, home string, journals *transport.Journals, pushed *[]transport.ServerFrame) *goalOverrideStore {
	t.Helper()
	store := newGoalOverrideStore(home)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	store.journal = &goalChangeJournal{journals: journals, pub: func(_ string, frame transport.ServerFrame) {
		if pushed != nil {
			*pushed = append(*pushed, frame)
		}
	}}
	return store
}

func TestGoalStoreLegacyMigrationABRestartAndCommittedOperations(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"session-a":"legacy A","session-b":"legacy B"}`
	path := filepath.Join(home, "state", "session-goal.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	journals := transport.NewJournals(nil)
	var pushed []transport.ServerFrame
	store := newGoalOverrideStore(home)
	ids := []string{"goal-native-a", "goal-native-b"}
	store.newID = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	store.journal = &goalChangeJournal{journals: journals, pub: func(_ string, frame transport.ServerFrame) {
		pushed = append(pushed, frame)
	}}
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	for sessionID, wantID := range map[string]string{"session-a": "goal-native-a", "session-b": "goal-native-b"} {
		snapshot, err := store.Snapshot(sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Goal == nil || snapshot.Goal.ID != wantID || snapshot.Goal.Revision != 1 || snapshot.RoundsStarted != nil {
			t.Fatalf("legacy %s snapshot=%+v", sessionID, snapshot)
		}
	}
	if len(pushed) != 0 {
		t.Fatalf("启动迁移不得伪造 change：%+v", pushed)
	}

	restarted := openGoalStoreForTest(t, home, journals, &pushed)
	before, err := restarted.Snapshot("session-a")
	if err != nil || before.Goal.ID != "goal-native-a" {
		t.Fatalf("restart lost native id: %+v err=%v", before, err)
	}
	if err := restarted.Set("session-a", "edited A"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Pause("session-a"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Resume("session-a"); err != nil {
		t.Fatal(err)
	}
	after, _ := restarted.Snapshot("session-a")
	other, _ := restarted.Snapshot("session-b")
	if after.Goal.ID != before.Goal.ID || after.Goal.Revision != 4 || after.Goal.Objective != "edited A" || after.Goal.Phase != "active" {
		t.Fatalf("committed A=%+v", after.Goal)
	}
	if other.Goal.Revision != 1 || other.Goal.Objective != "legacy B" {
		t.Fatalf("A migration changed B=%+v", other.Goal)
	}
	if len(pushed) != 3 {
		t.Fatalf("edit/pause/resume frames=%d want=3", len(pushed))
	}
	for i, operation := range []string{"edit", "pause", "resume"} {
		var change transport.GoalChange
		if err := json.Unmarshal(pushed[i].Data, &change); err != nil {
			t.Fatal(err)
		}
		if change.Operation != operation || change.Goal == nil || change.Goal.Revision != int64(i+2) || pushed[i].Seq != i+1 {
			t.Fatalf("frame[%d]=%+v change=%+v", i, pushed[i], change)
		}
	}
	frames, last, _ := journals.Get("session-a").Since(0, 0)
	if last != 3 || !reflect.DeepEqual(frames, pushed) {
		t.Fatalf("WS/history not same journal: frames=%+v pushed=%+v", frames, pushed)
	}
}

func TestGoalStoreStateAndJournalFailuresAreLoudAndRollback(t *testing.T) {
	home := t.TempDir()
	journals := transport.NewJournals(nil)
	var pushed []transport.ServerFrame
	store := openGoalStoreForTest(t, home, journals, &pushed)
	if err := store.Set("session-a", "persisted"); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Snapshot("session-a")
	originalPath := store.path
	blocked := filepath.Join(home, "blocked-goal-target")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	store.path = blocked
	err := store.Set("session-a", "must not commit")
	if !errors.Is(err, transport.ErrStatePersistence) {
		t.Fatalf("state flush error=%v", err)
	}
	store.path = originalPath
	after, _ := store.Snapshot("session-a")
	if after.Goal.Revision != before.Goal.Revision || after.Goal.Objective != before.Goal.Objective || len(pushed) != 1 {
		t.Fatalf("failed state flush changed snapshot/journal: before=%+v after=%+v pushed=%+v", before.Goal, after.Goal, pushed)
	}

	// journal 持久化失败必须回滚刚写入的 goal 文件，不占 Transport seq，也不推 WS。
	failure := &failingGoalJournalPersistence{err: errors.New("journal disk full")}
	failingJournals := transport.NewJournals(nil)
	failingJournals.SetPersistence(failure)
	store.journal = &goalChangeJournal{journals: failingJournals, pub: func(_ string, frame transport.ServerFrame) {
		pushed = append(pushed, frame)
	}}
	err = store.Set("session-a", "journal must roll back")
	if !errors.Is(err, transport.ErrStatePersistence) {
		t.Fatalf("journal flush error=%v", err)
	}
	after, _ = store.Snapshot("session-a")
	if after.Goal.Revision != before.Goal.Revision || after.Goal.Objective != before.Goal.Objective || len(pushed) != 1 || failingJournals.Get("session-a").LastSeq() != 0 {
		t.Fatalf("journal failure was not atomic: after=%+v pushed=%+v last=%d", after.Goal, pushed, failingJournals.Get("session-a").LastSeq())
	}
	failure.err = nil
	if err := store.Set("session-a", "retry after repair"); err != nil {
		t.Fatal(err)
	}
	if got := failingJournals.Get("session-a").LastSeq(); got != 1 {
		t.Fatalf("retry consumed a gap: lastSeq=%d", got)
	}
}

func TestGoalStoreCorruptSnapshotAndIllegalTransitionsStayUnchanged(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "state", "session-goal.json")
	corrupt := []byte(`{"version":1,"goals":{"session-a":{"goal":{"id":"g","revision":1,"objective":"x","phase":"paused","maxGoalRounds":0,"blockedReason":{"code":"bad","message":"bad"}}}}}`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	store := newGoalOverrideStore(home)
	if err := store.Load(); err == nil {
		t.Fatal("invalid phase/blockedReason combination loaded")
	}
	store.journal = &goalChangeJournal{journals: transport.NewJournals(nil)}
	if err := store.Resume("session-a"); !errors.Is(err, transport.ErrStatePersistence) {
		t.Fatalf("corrupt store mutation error=%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(raw, corrupt) {
		t.Fatalf("corrupt file overwritten: %q err=%v", raw, err)
	}

	nullHome := t.TempDir()
	nullStore := newGoalOverrideStore(nullHome)
	if err := os.MkdirAll(filepath.Dir(nullStore.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nullStore.path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := nullStore.Load(); err == nil {
		t.Fatal("JSON null must not migrate into an empty goal store")
	}
	raw, err = os.ReadFile(nullStore.path)
	if err != nil || string(raw) != "null" {
		t.Fatalf("null legacy file overwritten: %q err=%v", raw, err)
	}

	home = t.TempDir()
	store = openGoalStoreForTest(t, home, transport.NewJournals(nil), nil)
	if err := store.Pause("missing"); !errors.Is(err, transport.ErrGoalTransition) {
		t.Fatalf("pause missing error=%v", err)
	}
	if err := store.Set("session-a", "active"); err != nil {
		t.Fatal(err)
	}
	if err := store.Resume("session-a"); !errors.Is(err, transport.ErrGoalTransition) {
		t.Fatalf("resume active error=%v", err)
	}
	if err := store.Pause("session-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Pause("session-a"); !errors.Is(err, transport.ErrGoalTransition) {
		t.Fatalf("pause paused error=%v", err)
	}
	snapshot, _ := store.Snapshot("session-a")
	if snapshot.Goal.Phase != "paused" || snapshot.Goal.Revision != 2 {
		t.Fatalf("illegal transitions changed snapshot=%+v", snapshot.Goal)
	}
}

func writeEffortTestCatalog(t *testing.T, home string, revision int64, modelID string, efforts map[string]any, baseURL ...string) int64 {
	t.Helper()
	catalog := llm.OpenModelCatalog(home)
	model := map[string]any{"id": modelID, "reasoningEfforts": efforts}
	if len(baseURL) > 0 {
		model["baseURL"] = baseURL[0]
	}
	raw, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	view, err := catalog.Upsert(raw, nil, &revision)
	if err != nil {
		t.Fatal(err)
	}
	return view.Revision
}

func TestEffortStoreFailureCorruptionAndNoV4Writes(t *testing.T) {
	home := t.TempDir()
	store := newEffortOverrideStore(home)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	writeEffortTestCatalog(t, home, 0, "model-a", map[string]any{"off": nil, "deep": "wire-a"})
	if err := setServeEffort(store, home, "session-a", "deep", "model-a"); err != nil {
		t.Fatal(err)
	}
	path := createModelWireSession(t, home, t.TempDir(), "session-v4")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := setServeEffort(store, home, "session-v4", "deep", "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := setServeEffort(store, home, "session-v4", "", "model-a"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("set/clear wrote v4: err=%v", err)
	}

	if err := setServeEffort(store, home, "session-a", "wire-a", "model-a"); !errors.Is(err, transport.ErrEffortInvalid) {
		t.Fatalf("wire used as key error=%v", err)
	}
	selection, _ := store.Selection("session-a")
	if selection != "deep" {
		t.Fatalf("invalid key replaced old selection=%q", selection)
	}
	originalPath := store.path
	blocked := filepath.Join(home, "blocked-effort-target")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	store.path = blocked
	if err := store.Set("session-a", "off"); !errors.Is(err, transport.ErrStatePersistence) {
		t.Fatalf("flush error=%v", err)
	}
	store.path = originalPath
	selection, _ = store.Selection("session-a")
	if selection != "deep" {
		t.Fatalf("failed flush changed memory=%q", selection)
	}
	reopened := newEffortOverrideStore(home)
	if err := reopened.Load(); err != nil {
		t.Fatal(err)
	}
	selection, _ = reopened.Selection("session-a")
	if selection != "deep" {
		t.Fatalf("failed flush changed disk=%q", selection)
	}

	corruptHome := t.TempDir()
	corrupt := newEffortOverrideStore(corruptHome)
	if err := os.MkdirAll(filepath.Dir(corrupt.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt.path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Load(); err == nil {
		t.Fatal("corrupt effort store loaded")
	}
	if err := corrupt.Set("session-a", ""); !errors.Is(err, transport.ErrStatePersistence) {
		t.Fatalf("corrupt store clear error=%v", err)
	}
	raw, err := os.ReadFile(corrupt.path)
	if err != nil || string(raw) != "not-json" {
		t.Fatalf("corrupt store overwritten: %q err=%v", raw, err)
	}
}

func TestDefaultLoopRunnerEffortWireOffRunContentAndProvider(t *testing.T) {
	home := t.TempDir()
	revision := writeEffortTestCatalog(t, home, 0, "model-a", map[string]any{"off": nil, "deep": "wire-a", "low": "low"})
	writeEffortTestCatalog(t, home, revision, "provider-model", map[string]any{"deep": "wire-provider"}, "http://127.0.0.1:1/v1")
	efforts := newEffortOverrideStore(home)
	if err := efforts.Load(); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-a", "deep"); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-b", "off"); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", capture)
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	base := &loopRunner{home: home, policy: "deny", maxSteps: 6, model: "model-a", reasoning: "host-default"}
	runner := &modelOverrideLoopRunner{base: base}

	assertRun := func(sessionID, wantModel, wantReasoning string) {
		t.Helper()
		if err := runner.Run(context.Background(), sessionID, "turn-"+sessionID, "next"); err != nil {
			t.Fatal(err)
		}
		model, args := readLoopModelArg(t, capture)
		if model != wantModel || !containsArgPair(args, "--reasoning", wantReasoning) {
			t.Fatalf("%s argv model=%q reasoning-want=%q args=%v", sessionID, model, wantReasoning, args)
		}
	}
	assertRun("session-a", "model-a", "wire-a")
	assertRun("session-b", "model-a", "") // off:null 必须显式空，不继承 host-default。
	if err := runner.RunContent(context.Background(), "session-a", "turn-content", []transport.ContentBlock{{Type: "text", Text: "content"}}); err != nil {
		t.Fatal(err)
	}
	_, args := readLoopModelArg(t, capture)
	if !containsArgPair(args, "--reasoning", "wire-a") {
		t.Fatalf("RunContent lost effort: %v", args)
	}
	var hasContent bool
	for _, arg := range args {
		if arg == "--prompt-content-file" {
			hasContent = true
		}
	}
	if !hasContent {
		t.Fatalf("RunContent flattened or lost content: %v", args)
	}

	models := newModelOverrideStore(home)
	if err := models.Set("session-a", "provider-model"); err != nil {
		t.Fatal(err)
	}
	assertRun("session-a", "provider-model", "wire-provider")
	_, args = readLoopModelArg(t, capture)
	if !containsArgPair(args, "--base", "http://127.0.0.1:1/v1") {
		t.Fatalf("provider route lost base: %v", args)
	}

	// catalog 改 wire 后下一轮必须重新映射，而不是复用 set 时缓存。
	revision = writeEffortTestCatalog(t, home, 2, "provider-model", map[string]any{"deep": "wire-new"}, "http://127.0.0.1:1/v1")
	_ = revision
	assertRun("session-a", "provider-model", "wire-new")
	if base.reasoning != "host-default" {
		t.Fatalf("per-turn effort polluted shared base=%q", base.reasoning)
	}

	// 父进程显式空默认也必须传空 argv，不能让 child 重新继承 max。
	t.Setenv("DSHD_REASONING_EFFORT", "max")
	emptyDefault := &modelOverrideLoopRunner{base: &loopRunner{
		home: home, policy: "deny", maxSteps: 6, model: "model-a", reasoning: "",
	}}
	if err := emptyDefault.Run(context.Background(), "session-c", "turn-empty-default", "next"); err != nil {
		t.Fatal(err)
	}
	_, args = readLoopModelArg(t, capture)
	if !containsArgPair(args, "--reasoning", "") {
		t.Fatalf("explicit empty host default was not forwarded: %v", args)
	}
}

func TestDefaultLoopRunnerPersistsActualEffortAfterRun(t *testing.T) {
	home := t.TempDir()
	writeEffortTestCatalog(t, home, 0, "model-a", map[string]any{"deep": "wire-a"})
	createModelWireSession(t, home, t.TempDir(), "session-a")
	efforts := newEffortOverrideStore(home)
	if err := efforts.Load(); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-a", "deep"); err != nil {
		t.Fatal(err)
	}
	// 不再用只捕获 argv 的假子进程证明落盘：这里经同一 init 入口执行真实
	// cmdRun，并同时核对线上 wire 与 v4 中当次请求的 catalog selection。
	t.Setenv("DSHD_STEER_HELPER", "1")
	t.Setenv("DSHD_STEER_MANIFEST", writeMinimalSteerManifest(t))
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	base, requests, _ := newModelRequestRecorder(t)
	t.Setenv("DSHD_LLM_BASE", base)
	runner := &modelOverrideLoopRunner{base: &loopRunner{
		home: home, policy: "deny", maxSteps: 6, model: "model-a",
	}, recordEffort: true}
	if err := runner.Run(context.Background(), "session-a", "turn-effort", "next"); err != nil {
		t.Fatal(err)
	}
	if got := (<-requests).Reasoning; got != "wire-a" {
		t.Fatalf("真实请求 reasoning=%q want=wire-a", got)
	}
	actual, err := newSessionSource(home, "").Effort("session-a")
	if err != nil || actual != "deep" {
		t.Fatalf("default loop actual effort=%q err=%v", actual, err)
	}
}

func TestModelRunnerEffortOffABAndClearRestoresHostDefault(t *testing.T) {
	home := t.TempDir()
	writeEffortTestCatalog(t, home, 0, "model-a", map[string]any{"off": nil, "deep": "wire-a"})
	createModelWireSession(t, home, t.TempDir(), "session-a")
	createModelWireSession(t, home, t.TempDir(), "session-b")
	efforts := newEffortOverrideStore(home)
	if err := efforts.Load(); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-a", "off"); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-b", "deep"); err != nil {
		t.Fatal(err)
	}
	base, requests, _ := newModelRequestRecorder(t)
	runner := &modelRunner{home: home, base: base, model: "model-a", reasoning: "host-default", recordEffort: true}
	if got := runModelAndReadRequest(t, runner, requests, "session-a", "off"); got.Reasoning != "" {
		t.Fatalf("off request reasoning=%q", got.Reasoning)
	}
	if got := runModelAndReadRequest(t, runner, requests, "session-b", "deep"); got.Reasoning != "wire-a" {
		t.Fatalf("B request reasoning=%q", got.Reasoning)
	}
	src := newSessionSource(home, "")
	actual, err := src.Effort("session-b")
	if err != nil || actual != "deep" {
		path, _ := src.resolve("session-b")
		lg, _ := v4.ReadFile(path)
		for _, event := range lg.Events {
			t.Logf("v4 seq=%d type=%s data=%s", event.Seq, event.Type, event.Data)
		}
		t.Fatalf("persisted actual effort=%q err=%v", actual, err)
	}
	if err := efforts.Set("session-a", ""); err != nil {
		t.Fatal(err)
	}
	if got := runModelAndReadRequest(t, runner, requests, "session-a", "default"); got.Reasoning != "host-default" {
		t.Fatalf("clear did not restore host default=%q", got.Reasoning)
	}
}

func TestRunnerRejectsStaleEffortBeforeSpawnOrRequest(t *testing.T) {
	home := t.TempDir()
	writeEffortTestCatalog(t, home, 0, "model-a", map[string]any{"deep": "wire-a"})
	writeEffortTestCatalog(t, home, 1, "model-b", map[string]any{"off": nil, "low": "low"})
	efforts := newEffortOverrideStore(home)
	if err := efforts.Load(); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-a", "deep"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := resolveServeEffort(home, "session-a", "model-b", "host-default"); err == nil || !strings.Contains(err.Error(), "deep") {
		t.Fatalf("stale model/key error=%v", err)
	}
}

// 合并回归：W144 的 effort 必须覆盖 W138 的首步与真实 steer 后续步，
// 同时 W145 checkpoint 仍只锚定原始 user，不被同 turn 的 steer 改写。
func TestModelRunnerSteerEffortAndW145CheckpointCoexist(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	path := createModelWireSession(t, home, cwd, "session-steer-effort")
	writeEffortTestCatalog(t, home, 0, "model-a", map[string]any{"deep": "wire-a"})
	efforts := newEffortOverrideStore(home)
	if err := efforts.Load(); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-steer-effort", "deep"); err != nil {
		t.Fatal(err)
	}

	type wireRequest struct {
		Reasoning string             `json:"reasoning_effort"`
		Messages  []steerWireMessage `json:"messages"`
	}
	requests := make(chan wireRequest, 2)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解码 steer+effort 请求: %v", err)
			return
		}
		requests <- req
		select {
		case <-firstStarted:
		default:
			close(firstStarted)
			<-releaseFirst
		}
		writeSteerTextSSE(w, "first-answer")
	}))
	defer gateway.Close()

	runner := &modelRunner{home: home, base: gateway.URL + "/v1", model: "model-a", recordEffort: true}
	runDone := make(chan error, 1)
	go func() {
		runDone <- runner.Run(context.Background(), "session-steer-effort", "turn-steer-effort", "original-with-effort")
	}()
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("首个 effort 模型请求未开始")
	}
	if err := runner.Steer(context.Background(), "session-steer-effort", "turn-steer-effort", "steer-with-effort"); err != nil {
		t.Fatalf("在飞 steer 未接管: %v", err)
	}
	close(releaseFirst)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("steer+effort turn 未收工")
	}

	first, second := <-requests, <-requests
	if first.Reasoning != "wire-a" || second.Reasoning != "wire-a" {
		t.Fatalf("首步/steer步 reasoning=%q/%q，要 wire-a/wire-a", first.Reasoning, second.Reasoning)
	}
	if len(first.Messages) != 1 || steerContentString(t, first.Messages[0].Content) != "original-with-effort" {
		t.Fatalf("首步消息=%+v", first.Messages)
	}
	want := []string{"original-with-effort", "first-answer", "steer-with-effort"}
	if len(second.Messages) != len(want) {
		t.Fatalf("steer 后续步消息=%+v", second.Messages)
	}
	for i := range want {
		if got := steerContentString(t, second.Messages[i].Content); got != want[i] {
			t.Fatalf("steer 后续步消息[%d]=%q，要 %q", i, got, want[i])
		}
	}
	if effort, err := newSessionSource(home, "").Effort("session-steer-effort"); err != nil || effort != "deep" {
		t.Fatalf("实际 effort 投影=%q err=%v", effort, err)
	}

	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var originalAnchor, steerAnchor int64 = -1, -1
	for _, event := range log.Events {
		if event.Type != "user/message" {
			continue
		}
		text, _ := textOf(event.Data, false)
		switch text {
		case "original-with-effort":
			originalAnchor = event.Seq
		case "steer-with-effort":
			steerAnchor = event.Seq
		}
	}
	if originalAnchor < 0 || steerAnchor < 0 {
		t.Fatalf("原始/steer user 锚点缺失: original=%d steer=%d", originalAnchor, steerAnchor)
	}
	if _, err := serveRewindPreview(home, "session-steer-effort", strconv.FormatInt(originalAnchor, 10)); err != nil {
		t.Fatalf("原始 user 的 W145 checkpoint 不可读: %v", err)
	}
	if _, err := serveRewindPreview(home, "session-steer-effort", strconv.FormatInt(steerAnchor, 10)); err == nil {
		t.Fatal("steer user 不应改写/新增 W145 checkpoint 锚点")
	}
}
