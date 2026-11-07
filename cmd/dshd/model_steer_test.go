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

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

type steerWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type steerWireRequest struct {
	Messages []steerWireMessage `json:"messages"`
}

func steerContentString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatalf("content 不是字符串: %s (%v)", raw, err)
	}
	return text
}

func writeSteerTextSSE(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\n", text)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func waitSteerFrame(t *testing.T, frames <-chan transport.ServerFrame, typ string) transport.ServerFrame {
	t.Helper()
	for {
		select {
		case f := <-frames:
			if f.Type == typ {
				return f
			}
		case <-time.After(8 * time.Second):
			t.Fatalf("等待 %s 超时", typ)
		}
	}
}

func steerEventSummary(t *testing.T, path string) (types []string, userData []json.RawMessage) {
	t.Helper()
	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range log.Events {
		switch ev.Type {
		case "turn/start", "turn/end", "user/message", "assistant/message":
			types = append(types, ev.Type)
			if ev.Type == "user/message" {
				userData = append(userData, ev.Data)
			}
		}
	}
	return types, userData
}

func TestModelRunnerSteerWaitsForRunRegistration(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	path := seedTurnSession(t, home, cwd, "session-model-early", func(*session.Log) error { return nil })
	requests := make(chan steerWireRequest, 2)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req steerWireRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		requests <- req
		writeSteerTextSSE(w, "answer")
	}))
	defer gateway.Close()
	r := &modelRunner{home: home, base: gateway.URL + "/v1", model: "m"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	steerDone := make(chan error, 1)
	go func() { steerDone <- r.Steer(ctx, "session-model-early", "turn-early", "early-steer") }()
	select {
	case err := <-steerDone:
		t.Fatalf("Run 尚未注册时 steer 过早返回: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := r.Run(context.Background(), "session-model-early", "turn-early", "original"); err != nil {
		t.Fatal(err)
	}
	if err := <-steerDone; err != nil {
		t.Fatalf("Run 注册后早到 steer 未接管: %v", err)
	}
	req := <-requests
	if len(req.Messages) != 2 || steerContentString(t, req.Messages[0].Content) != "original" ||
		steerContentString(t, req.Messages[1].Content) != "early-steer" {
		t.Fatalf("首步边界应接管早到 steer: %+v", req.Messages)
	}
	types, _ := steerEventSummary(t, path)
	want := []string{"turn/start", "user/message", "user/message", "assistant/message", "turn/end"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("早到 steer 不得另开 turn: events=%v，要 %v", types, want)
	}
}

func TestModelRunnerSteerSameTurnFIFOAndLate(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	path := seedTurnSession(t, home, cwd, "session-model-steer", func(*session.Log) error { return nil })
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	requests := make(chan steerWireRequest, 4)
	var once sync.Once
	var mu sync.Mutex
	calls := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req steerWireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解码请求: %v", err)
			return
		}
		requests <- req
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			once.Do(func() { close(firstStarted) })
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
			writeSteerTextSSE(w, "assistant-1")
			return
		}
		writeSteerTextSSE(w, fmt.Sprintf("assistant-%d", n))
	}))
	defer gateway.Close()

	frames := make(chan transport.ServerFrame, 32)
	r := &modelRunner{home: home, base: gateway.URL + "/v1", model: "m",
		pub: func(_ string, f transport.ServerFrame) { frames <- f }}
	p := newPromptService(r)
	live, err := p.Submit(context.Background(), "session-model-steer", "original")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("第一模型调用未开始")
	}
	for _, text := range []string{"steer-1", "steer-2"} {
		steerCtx, cancel := context.WithCancel(context.Background())
		got, err := p.SubmitPlacement(steerCtx, "session-model-steer", text, "steer")
		cancel() // 临时提交 ctx 不能成为已接管消息的生命周期。
		if err != nil || !got.Accepted || got.TurnID != live.TurnID {
			t.Fatalf("steer receipt=%+v err=%v，要同一 turnID=%q", got, err, live.TurnID)
		}
	}
	close(releaseFirst)
	done := waitSteerFrame(t, frames, "turn/done")
	var doneData struct {
		TurnID string `json:"turnId"`
	}
	if err := json.Unmarshal(done.Data, &doneData); err != nil || doneData.TurnID != live.TurnID {
		t.Fatalf("turn/done 身份=%s err=%v，要 %q", done.Data, err, live.TurnID)
	}

	first := <-requests
	second := <-requests
	if len(first.Messages) != 1 || steerContentString(t, first.Messages[0].Content) != "original" {
		t.Fatalf("第一请求=%+v", first.Messages)
	}
	want := []string{"original", "assistant-1", "steer-1", "steer-2"}
	if len(second.Messages) != len(want) {
		t.Fatalf("第二请求=%+v", second.Messages)
	}
	for i := range want {
		if got := steerContentString(t, second.Messages[i].Content); got != want[i] {
			t.Fatalf("第二请求第 %d 条=%q，要 %q；全部=%+v", i, got, want[i], second.Messages)
		}
	}
	types, _ := steerEventSummary(t, path)
	wantTypes := []string{"turn/start", "user/message", "assistant/message", "user/message", "user/message", "assistant/message", "turn/end"}
	if fmt.Sprint(types) != fmt.Sprint(wantTypes) {
		t.Fatalf("v4 顺序=%v，要 %v", types, wantTypes)
	}
	select {
	case extra := <-requests:
		t.Fatalf("不得有第三模型调用/新 Run: %+v", extra)
	default:
	}
	if result, err := p.SubmitPlacement(context.Background(), "session-model-steer", "too-late", "steer"); !errors.Is(err, ErrSteerLate) || result.Accepted {
		t.Fatalf("收工后的 steer 必须明确过晚且不 accepted: result=%+v err=%v", result, err)
	}
}

func TestModelRunnerSteerContentPreservesPartsAndDurableOrder(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	path := seedTurnSession(t, home, cwd, "session-model-content-steer", func(*session.Log) error { return nil })
	imagePath := filepath.Join(cwd, "pixel.png")
	writeImageFixture(t, imagePath, "png", 1, 1)
	attachment, err := newAttachmentStore(home).Put(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	referencePath := filepath.Join(cwd, "reference.txt")
	if err := os.WriteFile(referencePath, []byte("reference-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks := []transport.ContentBlock{
		{Type: "text", Text: "BEFORE"},
		{Type: "attachment", AttachmentID: attachment.ID},
		{Type: "text", Text: "AFTER"},
		{Type: "reference", ReferenceType: "file", ReferenceID: "ref-" + strings.Repeat("a", 32), Path: referencePath},
	}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	requests := make(chan steerWireRequest, 2)
	var once sync.Once
	var calls int
	var mu sync.Mutex
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req steerWireRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		requests <- req
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			once.Do(func() { close(firstStarted) })
			<-releaseFirst
			writeSteerTextSSE(w, "first-answer")
			return
		}
		writeSteerTextSSE(w, "content-answer")
	}))
	defer gateway.Close()
	r := &modelRunner{home: home, base: gateway.URL + "/v1", model: "m"}
	runDone := make(chan error, 1)
	go func() {
		runDone <- r.Run(context.Background(), "session-model-content-steer", "turn-content", "original")
	}()
	<-firstStarted
	if err := r.SteerContent(context.Background(), "session-model-content-steer", "turn-content", blocks); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	<-requests
	second := <-requests
	if len(second.Messages) != 3 {
		t.Fatalf("content steer 第二请求=%+v", second.Messages)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(second.Messages[2].Content, &parts); err != nil {
		t.Fatalf("steer content 不是有序 parts: %s (%v)", second.Messages[2].Content, err)
	}
	if len(parts) != 4 || parts[0].Type != "text" || parts[0].Text != "BEFORE" ||
		parts[1].Type != "image_url" || !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") ||
		parts[2].Type != "text" || parts[2].Text != "AFTER" ||
		parts[3].Type != "text" || !strings.Contains(parts[3].Text, "ref-"+strings.Repeat("a", 32)) {
		t.Fatalf("text→image→text→reference 顺序/身份丢失: %+v", parts)
	}
	_, users := steerEventSummary(t, path)
	if len(users) != 2 {
		t.Fatalf("user 事件数=%d", len(users))
	}
	var durable struct {
		Content []session.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(users[1], &durable); err != nil {
		t.Fatal(err)
	}
	if len(durable.Content) != 4 || durable.Content[0].Text != "BEFORE" ||
		durable.Content[1].AttachmentID != attachment.ID || durable.Content[2].Text != "AFTER" ||
		durable.Content[3].ReferenceID != "ref-"+strings.Repeat("a", 32) {
		t.Fatalf("durable blocks 保真失败: %+v", durable.Content)
	}
}

func TestModelRunnerSteerSecondStepErrorAndCancelWriteNoPartialTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "第二模型调用错误"},
		{name: "第二模型调用取消", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			path := seedTurnSession(t, home, cwd, "session-model-error", func(*session.Log) error { return nil })
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
				fmt.Fprint(w, "controlled-second-step-error")
			}))
			defer gateway.Close()
			frames := make(chan transport.ServerFrame, 16)
			r := &modelRunner{home: home, base: gateway.URL + "/v1", model: "m",
				pub: func(_ string, f transport.ServerFrame) { frames <- f }}
			p := newPromptService(r)
			if _, err := p.Submit(context.Background(), "session-model-error", "original"); err != nil {
				t.Fatal(err)
			}
			<-firstStarted
			if _, err := p.SubmitPlacement(context.Background(), "session-model-error", "steer-not-partial", "steer"); err != nil {
				t.Fatal(err)
			}
			close(releaseFirst)
			<-secondStarted
			if tc.cancel {
				if err := p.Cancel("session-model-error"); err != nil {
					t.Fatal(err)
				}
				close(releaseSecond)
			} else {
				waitSteerFrame(t, frames, "turn/error")
			}
			types, _ := steerEventSummary(t, path)
			if len(types) != 0 {
				raw, _ := os.ReadFile(path)
				t.Fatalf("第二步失败/取消不得留下半套 turn 或 steer 历史: events=%v\n%s", types, raw)
			}
		})
	}
}
