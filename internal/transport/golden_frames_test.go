package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenManifest 是宿主与 UI 共用的 H14 契约夹具。framesFile 中只有正 seq
// 帧进入 journal；pong/session/reset 是控制样本，不占水位。
type goldenManifest struct {
	Schema string `json:"schema"`
	HTTP   struct {
		HistoryRequest json.RawMessage `json:"historyRequest"`
		StaleEpoch     struct {
			Request      json.RawMessage `json:"request"`
			Status       int             `json:"status"`
			Error        goldenError     `json:"error"`
			JournalEpoch int64           `json:"journalEpoch"`
			LastSeq      int             `json:"lastSeq"`
		} `json:"staleEpoch"`
	} `json:"http"`
	Sessions []*goldenSession `json:"sessions"`
}

type goldenError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
	Retryable bool   `json:"retryable"`
}

type goldenSession struct {
	ID               string `json:"id"`
	JournalEpoch     int64  `json:"journalEpoch"`
	FramesFile       string `json:"framesFile"`
	LiveSeq          []int  `json:"liveSeq"`
	PendingReplaySeq []int  `json:"pendingReplaySeq"`

	Frames   []ServerFrame `json:"-"`
	Controls []ServerFrame `json:"-"`
}

func loadGoldenContract(t *testing.T) *goldenManifest {
	t.Helper()
	raw, err := os.ReadFile("testdata/contract-golden.json")
	if err != nil {
		t.Fatalf("读共享 manifest：%v", err)
	}
	var g goldenManifest
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("共享 manifest 不是合法 JSON：%v", err)
	}
	if g.Schema != "dsh/transport-contract-golden@1" {
		t.Fatalf("未知 golden schema：%q", g.Schema)
	}
	if len(g.Sessions) < 2 {
		t.Fatalf("两会话隔离至少需要两个会话，实际 %d", len(g.Sessions))
	}
	seen := map[string]bool{}
	for _, s := range g.Sessions {
		if s.ID == "" || seen[s.ID] || s.JournalEpoch <= 0 {
			t.Fatalf("会话 id/epoch 非法：%+v", s)
		}
		seen[s.ID] = true
		frames := readGoldenFrames(t, s.FramesFile)
		for _, f := range frames {
			if f.Seq == 0 {
				s.Controls = append(s.Controls, f)
			} else {
				s.Frames = append(s.Frames, f)
			}
		}
	}
	return &g
}

func readGoldenFrames(t *testing.T, name string) []ServerFrame {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读共享帧 %s：%v", name, err)
	}
	var out []ServerFrame
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("%s 第 %d 行不是 JSON 对象：%v", name, i+1, err)
		}
		for key := range envelope {
			switch key {
			case "seq", "type", "data", "time":
			default:
				t.Fatalf("%s 第 %d 行含非 canonical 信封键 %q", name, i+1, key)
			}
		}
		for _, key := range []string{"seq", "type", "time"} {
			if _, ok := envelope[key]; !ok {
				t.Fatalf("%s 第 %d 行缺 %s", name, i+1, key)
			}
		}
		var f ServerFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("%s 第 %d 行不是 ServerFrame：%v", name, i+1, err)
		}
		out = append(out, f)
	}
	return out
}

func (s *goldenSession) frameBySeq(t *testing.T, seq int) ServerFrame {
	t.Helper()
	for _, f := range s.Frames {
		if f.Seq == seq {
			return f
		}
	}
	t.Fatalf("%s 没有 seq=%d", s.ID, seq)
	return ServerFrame{}
}

// 白名单里的 type 才许出现（含 v4 原词与显式登记的 Transport 事件）。
var allowedTypes = map[string]bool{
	"turn/start": true, "turn/end": true, "turn/done": true, "turn/error": true,
	"user/message": true, "assistant/message": true, "tool/call": true, "tool/result": true,
	"system/message": true, "developer/message": true,
	"approval/request": true, "approval/timeout": true, "approval/cancel": true,
	"session/reset": true, "pong": true,
	"session/title": true, "session/color": true,
	"goal/change": true,
}

func TestGoldenFramesMatchContract(t *testing.T) {
	g := loadGoldenContract(t)
	saw := map[string]bool{}
	for _, s := range g.Sessions {
		if len(s.Frames) == 0 {
			t.Fatalf("%s 没有 journal 帧", s.ID)
		}
		for i, f := range s.Frames {
			if !allowedTypes[f.Type] {
				t.Fatalf("%s 第 %d 帧 type=%q 不在白名单", s.ID, i+1, f.Type)
			}
			if f.Seq != i+1 {
				t.Fatalf("%s 第 %d 帧 seq=%d，应从 1 连续递增", s.ID, i+1, f.Seq)
			}
			if f.Time <= 0 || (len(f.Data) > 0 && !json.Valid(f.Data)) {
				t.Fatalf("%s seq=%d 缺 time 或 data 非法", s.ID, f.Seq)
			}
			saw[f.Type] = true
			if bytes.Contains(f.Data, []byte(`"reasoning"`)) {
				saw["reasoning"] = true
			}
			if bytes.Contains(f.Data, []byte(`"text"`)) {
				saw["text"] = true
			}
		}
		live := map[int]bool{}
		for _, seq := range s.LiveSeq {
			f := s.frameBySeq(t, seq)
			if live[seq] || f.Seq != seq {
				t.Fatalf("%s liveSeq 重复或不存在：%d", s.ID, seq)
			}
			live[seq] = true
		}
		for _, seq := range s.PendingReplaySeq {
			if f := s.frameBySeq(t, seq); f.Type != "approval/request" {
				t.Fatalf("%s pendingReplaySeq=%d 是 %s，不是 approval/request", s.ID, seq, f.Type)
			}
		}
		for _, f := range s.Controls {
			if f.Type != "pong" && f.Type != "session/reset" {
				t.Fatalf("%s 非法控制帧 %s", s.ID, f.Type)
			}
			if f.Time <= 0 {
				t.Fatalf("%s %s 缺 time", s.ID, f.Type)
			}
			saw[f.Type] = true
			if f.Type == "session/reset" {
				var d struct {
					Epoch   int64 `json:"journalEpoch"`
					LastSeq int   `json:"lastSeq"`
				}
				if err := json.Unmarshal(f.Data, &d); err != nil || d.Epoch != s.JournalEpoch || d.LastSeq != len(s.Frames) {
					t.Fatalf("%s reset 水位不符：%+v, err=%v", s.ID, d, err)
				}
			}
		}
	}
	for _, want := range []string{
		"text", "reasoning", "tool/call", "tool/result", "approval/request", "approval/timeout",
		"turn/done", "turn/error", "session/reset", "pong",
	} {
		if !saw[want] {
			t.Errorf("共享 golden 缺 %s 样本", want)
		}
	}
}

func TestGoldenFramesReplayInOrder(t *testing.T) {
	g := loadGoldenContract(t)
	for _, s := range g.Sessions {
		j := NewJournal(s.JournalEpoch, 0)
		for _, f := range s.Frames {
			got := j.Append(f.Type, f.Data, f.Time)
			if got.Seq != f.Seq || got.Time != f.Time || !bytes.Equal(got.Data, f.Data) {
				t.Fatalf("%s 重放失真：got=%+v want=%+v", s.ID, got, f)
			}
		}
		// 分批读取也必须拼回同一份原始 history，而不是 messages 投影。
		var replayed []ServerFrame
		for cursor := 0; ; {
			frames, last, epoch := j.Since(cursor, 2)
			if last != len(s.Frames) || epoch != s.JournalEpoch {
				t.Fatalf("%s history 水位错误：last=%d epoch=%d", s.ID, last, epoch)
			}
			if len(frames) == 0 {
				break
			}
			for _, f := range frames {
				replayed = append(replayed, f)
				cursor = f.Seq
			}
		}
		if len(replayed) != len(s.Frames) {
			t.Fatalf("%s 分批 history 得到 %d/%d 帧", s.ID, len(replayed), len(s.Frames))
		}
		for i := range replayed {
			if replayed[i].Seq != s.Frames[i].Seq || !bytes.Equal(replayed[i].Data, s.Frames[i].Data) {
				t.Fatalf("%s 第 %d 帧 history 与 golden 不同", s.ID, i+1)
			}
		}
		// pending approval 的重放是原负载的无 seq 状态帧，不再追加进 journal。
		for _, seq := range s.PendingReplaySeq {
			f := s.frameBySeq(t, seq)
			replay := ServerFrame{Type: f.Type, Data: f.Data, Time: f.Time}
			if replay.Seq != 0 || replay.Type != "approval/request" || !bytes.Equal(replay.Data, f.Data) {
				t.Fatalf("%s pending 重放失真", s.ID)
			}
		}
	}
}

func TestGoldenGapUsesHistoryNotReload(t *testing.T) {
	g := loadGoldenContract(t)
	s := g.Sessions[0]
	live := map[int]bool{}
	for _, seq := range s.LiveSeq {
		live[seq] = true
	}
	var missing []int
	for _, f := range s.Frames {
		if !live[f.Seq] {
			missing = append(missing, f.Seq)
		}
	}
	if fmt.Sprint(missing) != "[4 5]" {
		t.Fatalf("live 应故意缺 [4 5]，实际 %v", missing)
	}
	j := NewJournal(s.JournalEpoch, 0)
	for _, f := range s.Frames {
		j.Append(f.Type, f.Data, f.Time)
	}
	frames, last, epoch := j.Since(3, 2)
	if last != len(s.Frames) || epoch != s.JournalEpoch || len(frames) != 2 || frames[0].Seq != 4 || frames[1].Seq != 5 {
		t.Fatalf("history 未精确补齐 4..5：frames=%+v last=%d epoch=%d", frames, last, epoch)
	}
}

func TestGoldenHTTPBareParamsAndErrors(t *testing.T) {
	g := loadGoldenContract(t)
	s := g.Sessions[0]
	j := NewJournal(s.JournalEpoch, 0)
	for _, f := range s.Frames {
		j.Append(f.Type, f.Data, f.Time)
	}
	js := &Journals{m: map[string]*Journal{s.ID: j}}
	srv := newTestServer(t, Options{Journals: js})

	var bare map[string]json.RawMessage
	if err := json.Unmarshal(g.HTTP.HistoryRequest, &bare); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := bare["params"]; wrapped {
		t.Fatal("historyRequest 不得包 params")
	}
	for _, key := range []string{"id", "sinceSeq", "limit", "journalEpoch"} {
		if _, ok := bare[key]; !ok {
			t.Fatalf("historyRequest 缺 %s", key)
		}
	}
	rec := postJSON(t, srv, "/api/session.history", string(g.HTTP.HistoryRequest))
	if rec.Code != http.StatusOK {
		t.Fatalf("裸参数 history 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var success struct {
		Result HistoryResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &success); err != nil {
		t.Fatal(err)
	}
	if len(success.Result.Frames) != 2 || success.Result.Frames[0].Seq != 1 || success.Result.Frames[1].Seq != 2 ||
		success.Result.LastSeq != len(s.Frames) || success.Result.JournalEpoch != s.JournalEpoch {
		t.Fatalf("history 原始帧/水位不符：%+v", success.Result)
	}

	rec = postJSON(t, srv, "/api/session.history", string(g.HTTP.StaleEpoch.Request))
	if rec.Code != g.HTTP.StaleEpoch.Status {
		t.Fatalf("stale_epoch 状态=%d，期望 %d", rec.Code, g.HTTP.StaleEpoch.Status)
	}
	var failure struct {
		Error    goldenError `json:"error"`
		Epoch    int64       `json:"journalEpoch"`
		LastSeq  int         `json:"lastSeq"`
		RawError map[string]json.RawMessage
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(top["error"], &failure.RawError); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if len(failure.RawError) != 4 {
		t.Fatalf("错误对象必须恰好四字段，实际 %v", failure.RawError)
	}
	for _, key := range []string{"code", "message", "hint", "retryable"} {
		if _, ok := failure.RawError[key]; !ok {
			t.Errorf("错误对象缺 %s", key)
		}
	}
	if failure.Error != g.HTTP.StaleEpoch.Error || failure.Epoch != g.HTTP.StaleEpoch.JournalEpoch || failure.LastSeq != g.HTTP.StaleEpoch.LastSeq {
		t.Fatalf("stale_epoch 与共享 golden 不符：got=%+v epoch=%d last=%d", failure.Error, failure.Epoch, failure.LastSeq)
	}
}

// TestGoldenMessageIdentityTruth 冻结 MSGID-IDENTITY：v4Seq 来自持久事件信封，
// messageId 只透传已有持久 id；Transport seq 不得混进来。B 的同文消息必须同文不同身份。
func TestGoldenMessageIdentityTruth(t *testing.T) {
	g := loadGoldenContract(t)
	calls := map[string]map[string]bool{}
	sameText := map[string]map[string]int{}
	for _, s := range g.Sessions {
		calls[s.ID] = map[string]bool{}
		sameText[s.ID] = map[string]int{}
		v4Seen := map[int64]bool{}
		for _, f := range s.Frames {
			var d map[string]json.RawMessage
			if err := json.Unmarshal(f.Data, &d); err != nil {
				t.Fatal(err)
			}
			switch f.Type {
			case "user/message", "assistant/message":
				var identity struct {
					V4Seq     *int64 `json:"v4Seq"`
					MessageID string `json:"messageId"`
					ID        string `json:"id"`
					Content   []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
					Message struct {
						MessageID string `json:"messageId"`
						ID        string `json:"id"`
						Content   []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"message"`
				}
				if err := json.Unmarshal(f.Data, &identity); err != nil || identity.V4Seq == nil || *identity.V4Seq < 0 {
					t.Fatalf("%s seq=%d %s 缺真实 v4Seq（0 也合法）：%+v err=%v", s.ID, f.Seq, f.Type, identity, err)
				}
				if v4Seen[*identity.V4Seq] {
					t.Fatalf("%s 两条内容事件共用 v4Seq=%d", s.ID, *identity.V4Seq)
				}
				v4Seen[*identity.V4Seq] = true
				if f.Type == "user/message" {
					if identity.ID == "" || identity.MessageID != identity.ID {
						t.Fatalf("%s seq=%d user messageId=%q 必须透传已有 id=%q", s.ID, f.Seq, identity.MessageID, identity.ID)
					}
				} else {
					sourceID := identity.ID
					if sourceID == "" {
						sourceID = identity.Message.MessageID
					}
					if sourceID == "" {
						sourceID = identity.Message.ID
					}
					if identity.MessageID != "" && identity.MessageID != sourceID {
						t.Fatalf("%s seq=%d assistant messageId=%q 不是持久来源 id=%q", s.ID, f.Seq, identity.MessageID, sourceID)
					}
				}
				blocks := identity.Content
				if len(blocks) == 0 {
					blocks = identity.Message.Content
				}
				text := ""
				for _, b := range blocks {
					if b.Type == "text" {
						text += b.Text
					}
				}
				sameText[s.ID][f.Type+"\x00"+text]++
			case "turn/done", "turn/error":
				var identity struct {
					TurnID string `json:"turnId"`
				}
				if err := json.Unmarshal(f.Data, &identity); err != nil || identity.TurnID == "" {
					t.Fatalf("%s seq=%d 缺真实 turnId", s.ID, f.Seq)
				}
			case "tool/call":
				var identity struct {
					CallID string `json:"callId"`
				}
				if err := json.Unmarshal(f.Data, &identity); err != nil || identity.CallID == "" {
					t.Fatalf("%s seq=%d 缺真实 callId", s.ID, f.Seq)
				}
				calls[s.ID][identity.CallID] = true
			case "tool/result":
				if _, invented := d["callId"]; invented {
					t.Fatalf("%s seq=%d tool/result 顶层 callId 不是当前真实形状", s.ID, f.Seq)
				}
				var identity struct {
					Message struct {
						ToolCallID string `json:"toolCallId"`
					} `json:"message"`
				}
				if err := json.Unmarshal(f.Data, &identity); err != nil || !calls[s.ID][identity.Message.ToolCallID] {
					t.Fatalf("%s seq=%d message.toolCallId 无法配对：%q", s.ID, f.Seq, identity.Message.ToolCallID)
				}
			}
		}
	}
	for _, key := range []string{
		"user/message\x00SESSION-B-ONLY",
		"assistant/message\x00这是 session-b 的回答，不能串到 session-a。",
	} {
		if got := sameText["session-b"][key]; got != 2 {
			t.Fatalf("B 的同文样本 %q 应有两条独立身份，实际 %d", key, got)
		}
	}
}
