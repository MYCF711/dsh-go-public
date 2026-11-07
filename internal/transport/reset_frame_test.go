package transport

import (
	"encoding/json"
	"io"
	"testing"
)

// H14：客户端带着过期世代来订阅，服务端必须主动发 session/reset。
//
// 为什么必须服务端发：宿主重启后 journal 换了世代，客户端**无从得知**——
// 它只是继续按老水位收，收到的每一帧看起来都合法，但已经错位。
// 这种"看起来对、其实错"的状态是这类协议里最难查的。
func TestSubscribeWithStaleEpochEmitsSessionReset(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s1")
	j.Append("text", nil, 1)
	realEpoch := j.Epoch()

	h := NewHub()
	h.SetJournals(js)

	sub := &recordingSub{}
	conn := &scriptedConn{
		sub:    sub,
		frames: []ClientFrame{{T: FrameSub, Topic: "session:s1", SinceSeq: 0, JournalEpoch: realEpoch + 7}},
	}
	_ = ServeConn(conn, h)

	if len(sub.frames) == 0 {
		t.Fatalf("过期世代必须收到 session/reset")
	}
	f := sub.frames[0]
	if f.Type != "session/reset" {
		t.Fatalf("第一帧应是 session/reset，拿到 %q", f.Type)
	}
	if f.Seq != 0 {
		t.Fatalf("reset 不带 seq（它不是事件流的一帧），实际 %d", f.Seq)
	}
	var payload struct {
		JournalEpoch int64 `json:"journalEpoch"`
		LastSeq      int   `json:"lastSeq"`
	}
	if err := json.Unmarshal(f.Data, &payload); err != nil {
		t.Fatalf("reset 载荷不是 JSON：%v", err)
	}
	if payload.JournalEpoch != realEpoch {
		t.Fatalf("reset 要带**当前**世代 %d，实际 %d", realEpoch, payload.JournalEpoch)
	}
	if payload.LastSeq != 1 {
		t.Fatalf("reset 要带当前水位 1，实际 %d", payload.LastSeq)
	}
}

// 世代对得上时不许发 reset：多发一条会让客户端白白重建一次基线（长会话会闪）。
func TestSubscribeWithMatchingEpochIsQuiet(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s1")
	j.Append("text", nil, 1)

	h := NewHub()
	h.SetJournals(js)

	sub := &recordingSub{}
	conn := &scriptedConn{
		sub:    sub,
		frames: []ClientFrame{{T: FrameSub, Topic: "session:s1", SinceSeq: 0, JournalEpoch: j.Epoch()}},
	}
	_ = ServeConn(conn, h)

	for _, f := range sub.frames {
		if f.Type == "session/reset" {
			t.Fatalf("世代一致时不该发 reset：%+v", f)
		}
	}
}

// recordingSub 记下服务端写回来的每一帧。
type recordingSub struct {
	frames []ServerFrame
}

func (r *recordingSub) write(f ServerFrame) error {
	r.frames = append(r.frames, f)
	return nil
}

// scriptedConn 依次吐出预设的客户端帧，读完就 EOF（相当于对端正常关闭）。
type scriptedConn struct {
	sub    *recordingSub
	frames []ClientFrame
	i      int
}

func (c *scriptedConn) ReadClientFrame() (ClientFrame, error) {
	if c.i >= len(c.frames) {
		return ClientFrame{}, io.EOF
	}
	f := c.frames[c.i]
	c.i++
	return f, nil
}

func (c *scriptedConn) WriteServerFrame(f ServerFrame) error { return c.sub.write(f) }
