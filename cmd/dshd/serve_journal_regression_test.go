package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// 同一个 loopRunner 连续两轮调用同名工具、同参数时，第二轮只能从新的 EOF 水位
// 继续，不得把第一轮的 tool/call/tool/result 再推一次。这里直接走 follow/drain，
// 不依赖模型，也不用手写 Transport seq。
func TestServeFollowAcrossTurnsDoesNotRepeatSameTool(t *testing.T) {
	home := t.TempDir()
	sid := "follow-two-tool-turns"
	lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	js := transport.NewJournals(nil)
	var pushed []transport.ServerFrame
	r := &loopRunner{home: home, journals: js, pub: func(_ string, f transport.ServerFrame) {
		pushed = append(pushed, f)
	}}
	for turn := 1; turn <= 2; turn++ {
		prog := r.followBaseline(sid)
		logw, err := session.OpenSessionAt(lg.Path)
		if err != nil {
			t.Fatal(err)
		}
		callID := fmt.Sprintf("call-%d", turn)
		if err := logw.Append("tool/call", session.ToolCallEvent(turn, 1, callID, "read", `{"path":"same.txt"}`)); err != nil {
			t.Fatal(err)
		}
		if err := logw.Append("tool/result", session.ToolResultEvent(turn, 1, callID,
			[]spi.Block{{Type: "text", Text: "same result"}}, false, "", nil)); err != nil {
			t.Fatal(err)
		}
		if err := logw.Close(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		close(done)
		r.follow(context.Background(), sid, done, &prog)
	}

	var gotTypes []string
	for i, f := range pushed {
		gotTypes = append(gotTypes, f.Type)
		if f.Seq != i+1 || f.Time <= 0 {
			t.Fatalf("跨轮帧必须同 journal、连续且带 time：i=%d frame=%+v", i, f)
		}
	}
	wantTypes := []string{"tool/call", "tool/result", "tool/call", "tool/result"}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("两轮同工具只能各出现一次：got=%v want=%v frames=%+v", gotTypes, wantTypes, pushed)
	}
	history, last, _ := js.Get(sid).Since(0, 0)
	if last != 4 || !reflect.DeepEqual(history, pushed) {
		t.Fatalf("WS 推送与 history 必须来自同一 journal：last=%d history=%+v pushed=%+v", last, history, pushed)
	}
}

// debug runner 的 assistant 与 turn/done 都要走同一 journal；连续两轮不能回到
// Seq=1，也不能只给 assistant 补 time 而漏掉终局帧。
func TestServeDebugRunnerAllFramesShareJournalAndTime(t *testing.T) {
	home := t.TempDir()
	sid := "debug-two-turn-journal"
	seedTurnSession(t, home, t.TempDir(), sid, func(*session.Log) error { return nil })
	base, requests := newWireRecorder(t, sseTextOnly)
	js := transport.NewJournals(nil)
	var pushed []transport.ServerFrame
	r := &modelRunner{home: home, base: base, model: "m", journals: js, pub: func(_ string, f transport.ServerFrame) {
		pushed = append(pushed, f)
	}}
	runTurnMessages(t, r, requests, sid, "第一轮")
	runTurnMessages(t, r, requests, sid, "第二轮")

	var types []string
	for i, f := range pushed {
		types = append(types, f.Type)
		if f.Seq != i+1 || f.Time <= 0 {
			t.Fatalf("debug 帧必须连续且 time>0：i=%d frame=%+v", i, f)
		}
	}
	want := []string{
		"step/start", "assistant/text", "assistant/message", "step/end", TurnDone,
		"step/start", "assistant/text", "assistant/message", "step/end", TurnDone,
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("debug 两轮帧形状错误：got=%v want=%v", types, want)
	}
	history, last, _ := js.Get(sid).Since(0, 0)
	if last != len(pushed) || !reflect.DeepEqual(history, pushed) {
		t.Fatalf("debug 推送未完整进入 history：last=%d history=%+v pushed=%+v", last, history, pushed)
	}
}

func TestServePendingApprovalReplayHasTimeAndOriginalIdentity(t *testing.T) {
	b := newApprovalBroker(&fakeStdin{}, nil)
	b.timeout = time.Hour
	b.handle(approvalReq{RequestID: "perm-time", SessionID: "s-time", Tool: "read", Args: json.RawMessage(`{"path":"x"}`)})
	t.Cleanup(func() { _ = b.Resolve("perm-time", "reject_once") })

	raw := b.Pending("s-time")
	frames := b.PendingFrames("s-time")
	if len(raw) != 1 || len(frames) != 1 {
		t.Fatalf("应恰好一条 pending：raw=%d frames=%d", len(raw), len(frames))
	}
	f := frames[0]
	if f.Type != "approval/request" || f.Seq != 0 || f.Time <= 0 || string(f.Data) != string(raw[0]) {
		t.Fatalf("pending 重放必须保留 data/requestId/seq=0 且 time>0：%+v", f)
	}
	var identity struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(f.Data, &identity); err != nil || identity.RequestID != "perm-time" {
		t.Fatalf("requestId 失真：%+v err=%v data=%s", identity, err, f.Data)
	}
	if !strings.Contains(string(f.Data), `"sessionId":"s-time"`) {
		t.Fatalf("原始 sessionId 未保留：%s", f.Data)
	}
}
