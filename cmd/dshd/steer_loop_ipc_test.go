package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// 生产子进程入口：loopRunner 重 exec 当前测试二进制时，直接执行真实 cmdRun，
// 不走 testing 的 fake runner。它仍使用同一 stdin/stderr IPC 与 v4 写锁。
func init() {
	if os.Getenv("DSHD_STEER_HELPER") != "1" || len(os.Args) < 2 || os.Args[1] != "run" {
		return
	}
	args := os.Args[2:]
	if manifest := os.Getenv("DSHD_STEER_MANIFEST"); manifest != "" {
		args = append(args, "--manifest", manifest, "--workspace-bytes", "0", "--auto-compact", "0")
	}
	if err := cmdRun(args); err != nil {
		fmt.Fprintf(os.Stderr, "steer helper cmdRun: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type steerPublished struct {
	topic string
	frame transport.ServerFrame
}

func writeMinimalSteerManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dsh.toml")
	if err := os.WriteFile(path, []byte("[[module]]\nid = \"tools\"\nbackend = \"static\"\n\n[[module]]\nid = \"exec\"\nbackend = \"static\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoopRunnerProductionIPCSteerFIFOContentAndABIsolation(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	pathA := seedTurnSession(t, home, cwd, "session-loop-a", func(*session.Log) error { return nil })
	pathB := seedTurnSession(t, home, cwd, "session-loop-b", func(*session.Log) error { return nil })
	manifest := writeMinimalSteerManifest(t)
	t.Setenv("DSHD_STEER_HELPER", "1")
	t.Setenv("DSHD_STEER_MANIFEST", manifest)
	t.Setenv("DSHD_LLM_MODEL", "m")

	type record struct {
		sid string
		n   int
		req steerWireRequest
	}
	records := make(chan record, 8)
	firstStarted := map[string]chan struct{}{"session-loop-a": make(chan struct{}), "session-loop-b": make(chan struct{})}
	releaseFirst := map[string]chan struct{}{"session-loop-a": make(chan struct{}), "session-loop-b": make(chan struct{})}
	var startOnce = map[string]*sync.Once{"session-loop-a": {}, "session-loop-b": {}}
	var mu sync.Mutex
	calls := map[string]int{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req steerWireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解码子进程请求: %v", err)
			return
		}
		raw, _ := json.Marshal(req)
		sid := ""
		switch {
		case strings.Contains(string(raw), "A-original"):
			sid = "session-loop-a"
		case strings.Contains(string(raw), "B-original"):
			sid = "session-loop-b"
		default:
			t.Errorf("请求不属于 A/B: %s", raw)
			return
		}
		mu.Lock()
		calls[sid]++
		n := calls[sid]
		mu.Unlock()
		records <- record{sid: sid, n: n, req: req}
		if n == 1 {
			startOnce[sid].Do(func() { close(firstStarted[sid]) })
			select {
			case <-releaseFirst[sid]:
			case <-r.Context().Done():
				return
			}
		}
		writeSteerTextSSE(w, fmt.Sprintf("%s-answer-%d", sid, n))
	}))
	defer gateway.Close()
	t.Setenv("DSHD_LLM_BASE", gateway.URL+"/v1")

	actions := newJobActionBroker(home)
	broker := newApprovalBroker(nil, nil)
	broker.actions = actions
	published := make(chan steerPublished, 128)
	base := &loopRunner{home: home, policy: "allow", maxSteps: 8, broker: broker, jobActions: actions,
		journals: transport.NewJournals(nil), pub: func(topic string, f transport.ServerFrame) {
			published <- steerPublished{topic: topic, frame: f}
		}}
	p := newPromptService(&modelOverrideLoopRunner{base: base})
	liveA, err := p.Submit(context.Background(), "session-loop-a", "A-original")
	if err != nil {
		t.Fatal(err)
	}
	liveB, err := p.Submit(context.Background(), "session-loop-b", "B-original")
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"session-loop-a", "session-loop-b"} {
		select {
		case <-firstStarted[sid]:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s 第一模型调用未开始", sid)
		}
	}
	for _, text := range []string{"A-steer-1", "A-steer-2"} {
		got, err := p.SubmitPlacement(context.Background(), "session-loop-a", text, "steer")
		if err != nil || !got.Accepted || got.TurnID != liveA.TurnID {
			t.Fatalf("A steer receipt=%+v err=%v，要 turnID=%q", got, err, liveA.TurnID)
		}
	}
	imagePath := filepath.Join(cwd, "b-pixel.png")
	writeImageFixture(t, imagePath, "png", 1, 1)
	attachment, err := newAttachmentStore(home).Put(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	referencePath := filepath.Join(cwd, "b-reference.txt")
	if err := os.WriteFile(referencePath, []byte("B reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks := []transport.ContentBlock{
		{Type: "text", Text: "B-BEFORE"},
		{Type: "attachment", AttachmentID: attachment.ID},
		{Type: "text", Text: "B-AFTER"},
		{Type: "reference", ReferenceType: "file", ReferenceID: "ref-" + strings.Repeat("b", 32), Path: referencePath},
	}
	gotB, err := p.SubmitBlocksPlacement(context.Background(), "session-loop-b", blocks, "steer")
	if err != nil || !gotB.Accepted || gotB.TurnID != liveB.TurnID {
		t.Fatalf("B content steer receipt=%+v err=%v，要 turnID=%q", gotB, err, liveB.TurnID)
	}
	close(releaseFirst["session-loop-a"])
	close(releaseFirst["session-loop-b"])

	doneTurns := map[string]int{}
	turnStarts := map[string]int{}
	for len(doneTurns) < 2 {
		select {
		case item := <-published:
			if item.frame.Type == "turn/start" {
				turnStarts[item.topic]++
			}
			if item.frame.Type != "turn/done" {
				continue
			}
			var data struct {
				TurnID string `json:"turnId"`
			}
			if err := json.Unmarshal(item.frame.Data, &data); err != nil {
				t.Fatal(err)
			}
			want := liveA.TurnID
			if item.topic == "session:session-loop-b" {
				want = liveB.TurnID
			}
			if data.TurnID != want {
				t.Fatalf("turn/done topic=%s turnID=%q，要 %q", item.topic, data.TurnID, want)
			}
			doneTurns[item.topic]++
		case <-time.After(15 * time.Second):
			t.Fatalf("等 A/B turn/done 超时: %+v", doneTurns)
		}
	}
	if doneTurns["session:session-loop-a"] != 1 || doneTurns["session:session-loop-b"] != 1 ||
		turnStarts["session:session-loop-a"] != 1 || turnStarts["session:session-loop-b"] != 1 {
		t.Fatalf("steer 不得新 Run/新 turn: done=%v starts=%v", doneTurns, turnStarts)
	}

	second := map[string]steerWireRequest{}
	for len(second) < 2 {
		rec := <-records
		if rec.n == 2 {
			second[rec.sid] = rec.req
		}
	}
	a := second["session-loop-a"]
	var aTexts []string
	for _, msg := range a.Messages {
		var text string
		if json.Unmarshal(msg.Content, &text) == nil && (msg.Role == "user" || msg.Role == "assistant") {
			aTexts = append(aTexts, text)
		}
	}
	wantA := []string{"A-original", "session-loop-a-answer-1", "A-steer-1", "A-steer-2"}
	if fmt.Sprint(aTexts) != fmt.Sprint(wantA) {
		t.Fatalf("A 第二请求顺序=%v，要 %v；wire=%+v", aTexts, wantA, a.Messages)
	}
	b := second["session-loop-b"]
	if len(b.Messages) == 0 {
		t.Fatal("B 没有第二请求")
	}
	var bParts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(b.Messages[len(b.Messages)-1].Content, &bParts); err != nil {
		t.Fatalf("B content steer parts: %v", err)
	}
	if len(bParts) != 4 || bParts[0].Text != "B-BEFORE" || bParts[1].Type != "image_url" ||
		bParts[2].Text != "B-AFTER" || !strings.Contains(bParts[3].Text, "ref-"+strings.Repeat("b", 32)) {
		t.Fatalf("B content steer 顺序/附件/引用丢失: %+v", bParts)
	}
	typesA, _ := steerEventSummary(t, pathA)
	wantAEvents := []string{"turn/start", "user/message", "assistant/message", "user/message", "user/message", "assistant/message", "turn/end"}
	if fmt.Sprint(typesA) != fmt.Sprint(wantAEvents) {
		t.Fatalf("A v4=%v，要 %v", typesA, wantAEvents)
	}
	typesB, usersB := steerEventSummary(t, pathB)
	wantBEvents := []string{"turn/start", "user/message", "assistant/message", "user/message", "assistant/message", "turn/end"}
	if fmt.Sprint(typesB) != fmt.Sprint(wantBEvents) || len(usersB) != 2 {
		t.Fatalf("B v4=%v users=%d，要 %v / 2", typesB, len(usersB), wantBEvents)
	}
	var durable struct {
		Content []session.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(usersB[1], &durable); err != nil || len(durable.Content) != 4 ||
		durable.Content[1].AttachmentID != attachment.ID || durable.Content[3].ReferenceID != "ref-"+strings.Repeat("b", 32) {
		t.Fatalf("B durable content 不保真: %+v err=%v", durable.Content, err)
	}
	for sid, live := range map[string]transport.PromptResult{"session-loop-a": liveA, "session-loop-b": liveB} {
		result, err := p.SubmitPlacement(context.Background(), sid, "too-late", "steer")
		if !errors.Is(err, ErrSteerLate) || result.Accepted {
			t.Fatalf("%s 收工后 steer 未明确拒绝: result=%+v err=%v (live=%s)", sid, result, err, live.TurnID)
		}
	}
}

func TestLoopRunnerProductionIPCSteerBeforeRunRegistration(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	sid := "session-loop-early"
	turnID := "turn-loop-early"
	path := seedTurnSession(t, home, cwd, sid, func(*session.Log) error { return nil })
	t.Setenv("DSHD_STEER_HELPER", "1")
	t.Setenv("DSHD_STEER_MANIFEST", writeMinimalSteerManifest(t))
	t.Setenv("DSHD_LLM_MODEL", "m")
	requests := make(chan steerWireRequest, 2)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req steerWireRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		requests <- req
		writeSteerTextSSE(w, "answer")
	}))
	defer gateway.Close()
	// 早到分支同时走 catalog routed provider，覆盖 runProviderLoop 的同一 IPC 代次。
	upsertProviderForTest(t, home, "model-steer", gateway.URL+"/v1", "PROVIDER_STEER_KEY", "provider-secret", 0)
	setSessionProviderModel(t, home, sid, "model-steer")
	actions := newJobActionBroker(home)
	broker := newApprovalBroker(nil, nil)
	broker.actions = actions
	base := &loopRunner{home: home, policy: "allow", maxSteps: 8, broker: broker, jobActions: actions}
	runner := &modelOverrideLoopRunner{base: base}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	steerDone := make(chan error, 1)
	go func() { steerDone <- runner.Steer(ctx, sid, turnID, "early-steer") }()
	select {
	case err := <-steerDone:
		t.Fatalf("子进程尚未注册时 steer 过早返回: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := runner.Run(context.Background(), sid, turnID, "original"); err != nil {
		t.Fatal(err)
	}
	if err := <-steerDone; err != nil {
		t.Fatalf("子进程注册后早到 steer 未接管: %v", err)
	}
	req := <-requests
	var texts []string
	for _, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(msg.Content, &text) == nil {
			texts = append(texts, text)
		}
	}
	if fmt.Sprint(texts) != fmt.Sprint([]string{"original", "early-steer"}) {
		t.Fatalf("首步请求应包含 original→early-steer: %+v", req.Messages)
	}
	types, _ := steerEventSummary(t, path)
	want := []string{"turn/start", "user/message", "user/message", "assistant/message", "turn/end"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("早到 IPC steer 不得另开 turn: events=%v，要 %v", types, want)
	}
}

func TestLoopRunnerProductionIPCSteerSecondStepErrorAndCancel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "第二模型调用错误"},
		{name: "第二模型调用取消", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			sid := "session-loop-error"
			turnID := "turn-loop-error"
			path := seedTurnSession(t, home, cwd, sid, func(*session.Log) error { return nil })
			t.Setenv("DSHD_STEER_HELPER", "1")
			t.Setenv("DSHD_STEER_MANIFEST", writeMinimalSteerManifest(t))
			t.Setenv("DSHD_LLM_MODEL", "m")
			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			secondStarted := make(chan struct{})
			releaseSecond := make(chan struct{})
			var mu sync.Mutex
			calls := 0
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				if n == 1 {
					close(firstStarted)
					<-releaseFirst
					writeSteerTextSSE(w, "first-answer")
					return
				}
				close(secondStarted)
				if tc.cancel {
					select {
					case <-r.Context().Done():
					case <-releaseSecond:
					}
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, "controlled-child-second-error")
			}))
			defer gateway.Close()
			t.Setenv("DSHD_LLM_BASE", gateway.URL+"/v1")

			actions := newJobActionBroker(home)
			broker := newApprovalBroker(nil, nil)
			broker.actions = actions
			r := &loopRunner{home: home, policy: "allow", maxSteps: 8, broker: broker, jobActions: actions}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runDone := make(chan error, 1)
			go func() { runDone <- r.Run(ctx, sid, turnID, "original") }()
			select {
			case <-firstStarted:
			case <-time.After(10 * time.Second):
				t.Fatal("子进程第一模型调用未开始")
			}
			if err := r.Steer(context.Background(), sid, turnID, "child-steer"); err != nil {
				t.Fatal(err)
			}
			close(releaseFirst)
			select {
			case <-secondStarted:
			case <-time.After(10 * time.Second):
				t.Fatal("steer 没有触发同一子进程的第二模型调用")
			}
			if tc.cancel {
				cancel()
			}
			if err := <-runDone; err == nil {
				t.Fatal("第二模型调用错误/取消必须真实返回失败")
			}
			if tc.cancel {
				close(releaseSecond)
			}
			if err := r.Steer(context.Background(), sid, turnID, "late-after-error"); !errors.Is(err, ErrSteerLate) {
				t.Fatalf("失败收工后 steer 必须 ErrSteerLate，实际 %v", err)
			}
			types, users := steerEventSummary(t, path)
			want := []string{"turn/start", "user/message", "assistant/message", "user/message", "turn/end"}
			if fmt.Sprint(types) != fmt.Sprint(want) || len(users) != 2 {
				t.Fatalf("失败/取消不得写第二回答或新 turn: events=%v users=%d，要 %v / 2", types, len(users), want)
			}
		})
	}
}
