package transport

import (
	"encoding/json"
	"strings"
	"testing"
)

// 交接文档里给的三种客户端帧，一个都不能被拒。
func TestParseClientFrameAcceptsContract(t *testing.T) {
	cases := []struct {
		raw       string
		wantT     string
		wantTopic string
		wantSince int
	}{
		{`{"t":"sub","topic":"session:abc","sinceSeq":123}`, "sub", "session:abc", 123},
		{`{"t":"sub","topic":"session:abc"}`, "sub", "session:abc", 0},
		{`{"t":"unsub","topic":"session:abc"}`, "unsub", "session:abc", 0},
		{`{"t":"ping"}`, "ping", "", 0},
	}
	for _, c := range cases {
		got, err := ParseClientFrame([]byte(c.raw))
		if err != nil {
			t.Fatalf("%s 应当接受，却报 %v", c.raw, err)
		}
		if got.T != c.wantT || got.Topic != c.wantTopic || got.SinceSeq != c.wantSince {
			t.Fatalf("%s → %+v", c.raw, got)
		}
	}
}

// 不认识的帧绝不能被当 ping 放过：客户端会以为自己订阅成功了。
func TestParseClientFrameRejectsUnknown(t *testing.T) {
	bad := []string{
		``,
		`not json`,
		`{"t":"subscribe","topic":"session:abc"}`, // 拼错一个字母就静默不生效最糟
		`{"t":"sub"}`,                        // 缺 topic
		`{"t":"sub","topic":""}`,             // 空 topic
		`{"t":"sub","topic":"sessions:abc"}`, // 前缀不对
		`{"t":"sub","topic":"session:"}`,     // id 为空
		`{"t":"unsub"}`,                      // 缺 topic
	}
	for _, raw := range bad {
		if _, err := ParseClientFrame([]byte(raw)); err == nil {
			t.Fatalf("%q 应当被拒，却通过了", raw)
		}
	}
}

// seq 是客户端判连续性的依据，不许造 0。
func TestNewServerFrameRejectsZeroSeq(t *testing.T) {
	if _, err := NewServerFrame(0, "user/message", nil); err == nil {
		t.Fatal("seq=0 应当被拒")
	}
	if _, err := NewServerFrame(1, "", nil); err == nil {
		t.Fatal("空 type 应当被拒")
	}
	f, err := NewServerFrame(7, "user/message", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Seq != 7 || f.Type != "user/message" {
		t.Fatalf("%+v", f)
	}
}

// H2：流帧必须带墙钟时间，否则界面流式期间算不出 tok/s。
func TestServerFrameCarriesTime(t *testing.T) {
	f := ServerFrame{Seq: 3, Type: "assistant/message", Time: 1793683941007}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"time":1793683941007`) {
		t.Fatalf("帧里没有 time：%s", b)
	}
}
