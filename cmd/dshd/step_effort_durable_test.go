package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

func readStepEffortEvents(t *testing.T, path string) []v4.Event {
	t.Helper()
	lg, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读 v4: %v", err)
	}
	return lg.Events
}

func stepEventTypes(events []v4.Event) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

func requireOrderedEventTypes(t *testing.T, events []v4.Event, want ...string) {
	t.Helper()
	at := 0
	for _, event := range events {
		if at < len(want) && event.Type == want[at] {
			at++
		}
	}
	if at != len(want) {
		t.Fatalf("真实事件顺序缺少 %v；全部类型=%v", want, stepEventTypes(events))
	}
}

// 生产 serve 的默认 runner 就是 cmdRun。最终无工具步也必须有 step/end，
// reasoning/text 增量不能只留在完整 assistant/message 里。
func TestCmdRunPersistsRealStepStreamAndFinalStepEnd(t *testing.T) {
	home, _ := d1Workspace(t, "step-stream-run")
	base := d1SSE(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	if err := d1Run(t, home, "step-stream-run", base, "--reasoning", "high"); err != nil {
		t.Fatal(err)
	}
	path, err := session.ResolvePath(home, "", "step-stream-run")
	if err != nil {
		t.Fatal(err)
	}
	events := readStepEffortEvents(t, path)
	requireOrderedEventTypes(t, events,
		"turn/start", "user/message", "step/start", "assistant/reasoning", "assistant/text",
		"assistant/message", "step/end", "turn/end")
	for _, event := range events {
		if event.Type != "assistant/reasoning" && event.Type != "assistant/text" {
			continue
		}
		var data struct {
			Turn  int    `json:"turn"`
			Step  int    `json:"step"`
			Index int    `json:"index"`
			Text  string `json:"text"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Turn != 1 || data.Step != 1 || data.Model == "" || data.Text == "" {
			t.Fatalf("delta 身份/正文不完整：%s", event.Data)
		}
		wantIndex := 0
		if event.Type == "assistant/text" {
			wantIndex = 1
		}
		if data.Index != wantIndex {
			t.Fatalf("delta index=%d want=%d data=%s", data.Index, wantIndex, event.Data)
		}
	}
}

// follow/drain 必须保留 v4 信封时间；delta 也要有持久 v4Seq，断线重放才能去重，
// TTFT 不能用 200ms 轮询时的 time.Now 重新伪造。
func TestServeFollowPreservesV4TimeAndStepDeltaIdentity(t *testing.T) {
	home := t.TempDir()
	sid := "follow-step-time"
	lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("step/start", map[string]any{"turn": 1, "step": 1}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("assistant/text", map[string]any{"turn": 1, "step": 1, "text": "real", "index": 0}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	events := readStepEffortEvents(t, lg.Path)
	js := transport.NewJournals(nil)
	var pushed []transport.ServerFrame
	r := &loopRunner{home: home, journals: js, pub: func(_ string, frame transport.ServerFrame) {
		pushed = append(pushed, frame)
	}}
	prog := r.followBaseline(sid)
	prog.offset = 0
	done := make(chan struct{})
	close(done)
	r.follow(context.Background(), sid, done, &prog)
	if len(pushed) != len(events) {
		t.Fatalf("frames=%d events=%d", len(pushed), len(events))
	}
	for i, frame := range pushed {
		if frame.Time != events[i].Time {
			t.Fatalf("frame %d time=%d v4 time=%d", i, frame.Time, events[i].Time)
		}
		if frame.Type != "assistant/text" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(frame.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["v4Seq"] != float64(events[i].Seq) {
			t.Fatalf("delta v4Seq=%v want=%d data=%s", data["v4Seq"], events[i].Seq, frame.Data)
		}
	}
}

// modelRunner 的首步和真实 steer 后续步都要把当次请求的 selection 与流式 step
// 写入同一份 v4；不能只拿 override store 在回合结束后补一条。
func TestModelRunnerSteerPersistsEachRealStepAndEffort(t *testing.T) {
	home := t.TempDir()
	createModelWireSession(t, home, t.TempDir(), "session-step-steer")
	writeEffortTestCatalog(t, home, 0, "model-a", map[string]any{"deep": "wire-a"})
	efforts := newEffortOverrideStore(home)
	if err := efforts.Load(); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-step-steer", "deep"); err != nil {
		t.Fatal(err)
	}

	firstStarted := make(chan struct{})
	firstDeltaWritten := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requests int
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Reasoning string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解码请求: %v", err)
		}
		if body.Reasoning != "wire-a" {
			t.Errorf("第 %d 次真实请求 reasoning=%q", requests, body.Reasoning)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			close(firstStarted)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer-1\"},\"finish_reason\":null}]}\n\n")
			w.(http.Flusher).Flush()
			close(firstDeltaWritten)
			<-releaseFirst
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		} else {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\n", fmt.Sprintf("answer-%d", requests))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer gateway.Close()

	js := transport.NewJournals(nil)
	var frames []transport.ServerFrame
	live := make(chan transport.ServerFrame, 16)
	runner := &modelRunner{home: home, base: gateway.URL + "/v1", model: "model-a", recordEffort: true,
		journals: js, pub: func(_ string, frame transport.ServerFrame) {
			frames = append(frames, frame)
			live <- frame
		}}
	waitLive := func(want string) transport.ServerFrame {
		t.Helper()
		for {
			select {
			case frame := <-live:
				if frame.Type == want {
					return frame
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("等待 live %s 超时", want)
			}
		}
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- runner.Run(context.Background(), "session-step-steer", "turn-step-steer", "original")
	}()
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("首步没有发出")
	}
	waitLive("step/start")
	if err := runner.Steer(context.Background(), "session-step-steer", "turn-step-steer", "steer"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDeltaWritten:
	case <-time.After(5 * time.Second):
		t.Fatal("首 delta 未写入")
	}
	// 响应还未 finish 就必须已经看到真实 delta；此时 v4 仍遵守 W138 原子边界。
	waitLive("assistant/text")
	path, err := session.ResolvePath(home, "", "session-step-steer")
	if err != nil {
		t.Fatal(err)
	}
	if events := readStepEffortEvents(t, path); len(events) != 0 {
		t.Fatalf("首响应未收工时 v4 不应有半套事件：%v", stepEventTypes(events))
	}
	close(releaseFirst)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("steer turn 未收工")
	}
	events := readStepEffortEvents(t, path)
	var selections, starts, deltas, ends []v4.Event
	for _, event := range events {
		switch event.Type {
		case "model/selection":
			selections = append(selections, event)
		case "step/start":
			starts = append(starts, event)
		case "assistant/text", "assistant/reasoning":
			deltas = append(deltas, event)
		case "step/end":
			ends = append(ends, event)
		}
	}
	if requests != 2 || len(selections) != 2 || len(starts) != 2 || len(deltas) != 2 || len(ends) != 2 {
		t.Fatalf("requests=%d selections=%d starts=%d deltas=%d ends=%d types=%v",
			requests, len(selections), len(starts), len(deltas), len(ends), stepEventTypes(events))
	}
	requireOrderedEventTypes(t, events,
		"turn/start", "user/message", "model/selection", "step/start", "assistant/text", "assistant/message", "step/end",
		"user/message", "model/selection", "step/start", "assistant/text", "assistant/message", "step/end", "turn/end")
	for i, event := range selections {
		var data map[string]any
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(data["reasoningEffort"], "deep") || !strings.Contains(string(event.Data), "model-a") {
			t.Fatalf("selection[%d]=%s", i, event.Data)
		}
	}
	var liveStarts, liveDeltas, liveEnds int
	for _, frame := range frames {
		switch frame.Type {
		case "step/start":
			liveStarts++
		case "assistant/text", "assistant/reasoning":
			liveDeltas++
		case "step/end":
			liveEnds++
		}
	}
	if liveStarts != 2 || liveDeltas != 2 || liveEnds != 2 {
		t.Fatalf("live starts=%d deltas=%d ends=%d frames=%+v", liveStarts, liveDeltas, liveEnds, frames)
	}
}

// 200ms tick 路径与预关闭 done 的 drain 路径共用同一条时间/身份规则。
func TestServeFollowTickPreservesV4TimeAndStepDeltaIdentity(t *testing.T) {
	home := t.TempDir()
	sid := "follow-step-time-tick"
	lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("step/start", map[string]any{"turn": 1, "step": 1}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("assistant/text", map[string]any{"turn": 1, "step": 1, "text": "tick", "index": 0}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	events := readStepEffortEvents(t, lg.Path)
	pushed := make(chan transport.ServerFrame, 4)
	r := &loopRunner{home: home, journals: transport.NewJournals(nil), pub: func(_ string, frame transport.ServerFrame) {
		pushed <- frame
	}}
	prog := r.followBaseline(sid)
	prog.offset = 0
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		r.follow(ctx, sid, make(chan struct{}), &prog)
		close(returned)
	}()
	var frames []transport.ServerFrame
	for len(frames) < len(events) {
		select {
		case frame := <-pushed:
			frames = append(frames, frame)
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatalf("tick 只收到 %d/%d 帧", len(frames), len(events))
		}
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("follow 未随 ctx 收工")
	}
	for i, frame := range frames {
		if frame.Time != events[i].Time {
			t.Fatalf("tick frame %d time=%d v4 time=%d", i, frame.Time, events[i].Time)
		}
		if frame.Type == "assistant/text" {
			var data map[string]any
			if err := json.Unmarshal(frame.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["v4Seq"] != float64(events[i].Seq) {
				t.Fatalf("tick delta v4Seq=%v want=%d", data["v4Seq"], events[i].Seq)
			}
		}
	}
}

// 每次真实 HTTP 请求都对应一条 selection；这里不创建 override store，
// 因而两条记录只能来自 cmdRun 的实际请求 sink，不能由 UI 状态伪造。
func TestCmdRunRecordsSelectionForEveryActualModelStep(t *testing.T) {
	home, _ := d1Workspace(t, "step-effort-requests")
	var requests int
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Reasoning string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解码请求: %v", err)
		}
		if body.Reasoning != "wire-a" {
			t.Errorf("第 %d 次请求 reasoning=%q", requests, body.Reasoning)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"nope\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer gateway.Close()
	if err := d1Run(t, home, "step-effort-requests", gateway.URL,
		"--record-effort", "--effort-level", "deep", "--reasoning", "wire-a", "--max-steps", "2"); err != nil {
		t.Fatal(err)
	}
	path, err := session.ResolvePath(home, "", "step-effort-requests")
	if err != nil {
		t.Fatal(err)
	}
	events := readStepEffortEvents(t, path)
	var selections, starts, ends int
	for _, event := range events {
		switch event.Type {
		case "model/selection":
			selections++
			var data map[string]any
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["reasoningEffort"] != "deep" {
				t.Fatalf("selection=%s", event.Data)
			}
		case "step/start":
			starts++
		case "step/end":
			ends++
		}
	}
	if requests != 2 || selections != requests || starts != requests || ends != requests {
		t.Fatalf("requests=%d selections=%d starts=%d ends=%d types=%v", requests, selections, starts, ends, stepEventTypes(events))
	}
}

func TestServeFollowKeepsEffortSelectionOutOfTransportJournal(t *testing.T) {
	home := t.TempDir()
	sid := "follow-effort-internal"
	lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("model/selection", map[string]any{"model": "model-a", "reasoningEffort": "deep"}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("step/start", map[string]any{"turn": 1, "step": 1}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	js := transport.NewJournals(nil)
	var pushed []transport.ServerFrame
	r := &loopRunner{home: home, journals: js, pub: func(_ string, frame transport.ServerFrame) {
		pushed = append(pushed, frame)
	}}
	prog := r.followBaseline(sid)
	prog.offset = 0
	done := make(chan struct{})
	close(done)
	r.follow(context.Background(), sid, done, &prog)
	if len(pushed) != 1 || pushed[0].Type != "step/start" {
		t.Fatalf("selection 不应新增 WS/journal 帧：%+v", pushed)
	}
	if got := js.Get(sid).LastSeq(); got != 1 {
		t.Fatalf("journal lastSeq=%d want=1", got)
	}
}

func TestResolveServeEffortDoesNotInventCatalogLevelFromWire(t *testing.T) {
	home := t.TempDir()
	writeEffortTestCatalog(t, home, 0, "model-a", map[string]any{"off": nil, "deep": "wire-a", "high": "high"})
	for _, tc := range []struct {
		fallback  string
		wantWire  string
		wantLevel string
	}{
		{fallback: "wire-a", wantWire: "wire-a", wantLevel: ""},
		{fallback: "deep", wantWire: "deep", wantLevel: ""},
		{fallback: "high", wantWire: "high", wantLevel: "high"},
		{fallback: "", wantWire: "", wantLevel: ""},
	} {
		t.Run("fallback="+tc.fallback, func(t *testing.T) {
			wire, level, selected, err := resolveServeEffort(home, "session-a", "model-a", tc.fallback)
			if err != nil {
				t.Fatal(err)
			}
			if selected || wire != tc.wantWire || level != tc.wantLevel {
				t.Fatalf("wire=%q level=%q selected=%v want wire=%q level=%q selected=false", wire, level, selected, tc.wantWire, tc.wantLevel)
			}
		})
	}
	efforts := newEffortOverrideStore(home)
	if err := efforts.Load(); err != nil {
		t.Fatal(err)
	}
	if err := efforts.Set("session-a", "off"); err != nil {
		t.Fatal(err)
	}
	wire, level, selected, err := resolveServeEffort(home, "session-a", "model-a", "host-default")
	if err != nil || !selected || wire != "" || level != "off" {
		t.Fatalf("显式 off 必须保留 key+空 wire：wire=%q level=%q selected=%v err=%v", wire, level, selected, err)
	}
}
