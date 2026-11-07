package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type memoryJournalPersistence struct {
	frames map[string][]ServerFrame
	resets int
	err    error
}

func (p *memoryJournalPersistence) AppendJournalFrame(sessionID string, frame ServerFrame) error {
	if p.err != nil {
		return p.err
	}
	frame.Data = append(json.RawMessage(nil), frame.Data...)
	p.frames[sessionID] = append(p.frames[sessionID], frame)
	return nil
}

func (p *memoryJournalPersistence) ResetJournal(string) error {
	if p.err != nil {
		return p.err
	}
	p.resets++
	return nil
}

func TestJournalPersistenceRestoresRawFramesWithNewEpoch(t *testing.T) {
	p := &memoryJournalPersistence{frames: map[string][]ServerFrame{}}
	first := NewJournals(func() int64 { return 101 })
	first.SetPersistence(p)
	raw := json.RawMessage("{\n  \"message\": {\"content\": [{\"type\":\"reasoning\",\"text\":\"原样\"}]}\n}")
	for _, tc := range []struct {
		typ  string
		data json.RawMessage
		time int64
	}{
		{"assistant/message", raw, 1001},
		{"approval/request", json.RawMessage(`{"requestId":"perm-a"}`), 1002},
	} {
		if _, err := first.Append("session-a", tc.typ, tc.data, tc.time); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.Append("session-b", "assistant/message", json.RawMessage(`{"text":"只属于 B"}`), 2001); err != nil {
		t.Fatal(err)
	}

	second := NewJournals(func() int64 { return 202 })
	if err := second.Restore("session-a", p.frames["session-a"]); err != nil {
		t.Fatal(err)
	}
	if err := second.Restore("session-b", p.frames["session-b"]); err != nil {
		t.Fatal(err)
	}
	a := second.Get("session-a")
	if a.Epoch() != 202 || a.Epoch() == first.Get("session-a").Epoch() || a.LastSeq() != 2 {
		t.Fatalf("恢复必须换新 epoch 并续旧水位：old=%d new=%d last=%d", first.Get("session-a").Epoch(), a.Epoch(), a.LastSeq())
	}
	got, last, epoch := a.Since(0, 0)
	if last != 2 || epoch != 202 || !reflect.DeepEqual(got, p.frames["session-a"]) {
		t.Fatalf("原始帧恢复失真：got=%+v want=%+v last=%d epoch=%d", got, p.frames["session-a"], last, epoch)
	}
	if !bytes.Equal(got[0].Data, raw) {
		t.Fatalf("data 字节必须逐字保留：got=%q want=%q", got[0].Data, raw)
	}
	b, _, _ := second.Get("session-b").Since(0, 0)
	if len(b) != 1 || !bytes.Contains(b[0].Data, []byte("只属于 B")) || bytes.Contains(b[0].Data, []byte("perm-a")) {
		t.Fatalf("A/B journal 不得串联：%+v", b)
	}
	next, err := second.Append("session-a", "turn/done", json.RawMessage(`{"turnId":"turn-1"}`), 1003)
	if err != nil || next.Seq != 3 || next.Time != 1003 {
		t.Fatalf("恢复后的新帧必须续号且不改 time：frame=%+v err=%v", next, err)
	}
}

func TestJournalPersistenceFailureDoesNotConsumeSeq(t *testing.T) {
	p := &memoryJournalPersistence{frames: map[string][]ServerFrame{}, err: errors.New("disk full")}
	js := NewJournals(func() int64 { return 7 })
	js.SetPersistence(p)
	frame, err := js.Append("s", "assistant/message", json.RawMessage(`{"text":"x"}`), 123)
	if err == nil || frame.Seq != 0 || frame.Type != "" || frame.Data != nil || frame.Time != 0 {
		t.Fatalf("持久化失败必须显式返回且不得发帧：frame=%+v err=%v", frame, err)
	}
	if got := js.Get("s").LastSeq(); got != 0 {
		t.Fatalf("写盘失败不得消耗 Transport seq：%d", got)
	}
	win := js.Get("s").Window(0, 0)
	if len(win.Frames) != 0 || win.LastSeq != 0 || !win.Covered {
		t.Fatalf("写盘失败不得留下内存半帧：%+v", win)
	}
}

func TestJournalRestoreRejectsFabricatedOrBrokenSequence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []ServerFrame
	}{
		{"gap", []ServerFrame{{Seq: 2, Type: "turn/done", Time: 1}}},
		{"duplicate", []ServerFrame{{Seq: 1, Type: "turn/done", Time: 1}, {Seq: 1, Type: "turn/done", Time: 2}}},
		{"missing-time", []ServerFrame{{Seq: 1, Type: "turn/done", Time: 0}}},
		{"missing-type", []ServerFrame{{Seq: 1, Time: 1}}},
		{"bad-data", []ServerFrame{{Seq: 1, Type: "turn/done", Data: json.RawMessage(`{`), Time: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := NewJournal(9, 0)
			if err := j.Restore(tc.frames); err == nil {
				t.Fatalf("坏记录不得被修复成编造的历史：%+v", tc.frames)
			}
			if j.LastSeq() != 0 {
				t.Fatalf("失败恢复不得部分修改 journal：%+v", j)
			}
		})
	}
}
