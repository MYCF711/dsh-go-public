package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// MSGID-IDENTITY：load 与 WS 必须投影同一个持久 v4 Event.Seq；同文不是去重键。
// 测试只走既有入口并用线上 JSON 断言，所以旧实现是行为红，不是编译红。
func TestLoadCarriesPersistentV4MessageIdentityAcrossReopen(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	const sid = "message-identity-load"
	user1 := session.UserMessage([]spi.Block{{Type: "text", Text: "同文用户"}})
	path := seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", user1); err != nil {
			return err
		}
		return lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.TextBlock("同文助手")}, "stop", nil))
	})

	readWire := func() []map[string]json.RawMessage {
		t.Helper()
		msgs, err := newSessionSource(home, cwd).Messages(sid)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]map[string]json.RawMessage, 0, len(msgs))
		for _, m := range msgs {
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			out = append(out, wire)
		}
		return out
	}
	beforeWire := readWire()
	beforeLog, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeRaw := make([]json.RawMessage, len(beforeLog.Events))
	for i, ev := range beforeLog.Events {
		beforeRaw[i] = append(json.RawMessage(nil), ev.Raw...)
	}

	// 模拟另一进程续写：同文两条是全新事件，不能复用第一条身份，也不能重写旧字节。
	logw, err := session.OpenSessionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	user2 := session.UserMessage([]spi.Block{{Type: "text", Text: "同文用户"}})
	if err := logw.Append("user/message", user2); err != nil {
		t.Fatal(err)
	}
	if err := logw.Append("assistant/message", session.AssistantMessageEvent(2, 1,
		[]map[string]any{session.TextBlock("同文助手")}, "stop", nil)); err != nil {
		t.Fatal(err)
	}
	if err := logw.Close(); err != nil {
		t.Fatal(err)
	}

	afterLog, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterLog.Events) != 4 || len(beforeRaw) != 2 {
		t.Fatalf("续写事件数不对：before=%d after=%d", len(beforeRaw), len(afterLog.Events))
	}
	for i := range beforeRaw {
		if !bytes.Equal(beforeRaw[i], afterLog.Events[i].Raw) {
			t.Fatalf("旧 v4 事件 %d 被重写：before=%s after=%s", i, beforeRaw[i], afterLog.Events[i].Raw)
		}
	}

	afterWire := readWire()
	if len(beforeWire) != 2 || len(afterWire) != 4 {
		t.Fatalf("同文消息必须全部保留：before=%d after=%d wire=%+v", len(beforeWire), len(afterWire), afterWire)
	}
	wantUsers := []string{user1["id"].(string), user2["id"].(string)}
	for i, wire := range afterWire {
		var identity struct {
			V4Seq     *int64 `json:"v4Seq"`
			MessageID string `json:"messageId"`
		}
		if err := json.Unmarshal(wire["v4Seq"], &identity.V4Seq); err != nil || identity.V4Seq == nil {
			t.Fatalf("load 第 %d 条缺 v4Seq（seq=0 也必须出现）：%s err=%v", i, wire["v4Seq"], err)
		}
		if *identity.V4Seq != int64(i) {
			t.Fatalf("load 第 %d 条 v4Seq=%d，期望持久 Event.Seq=%d", i, *identity.V4Seq, i)
		}
		_ = json.Unmarshal(wire["messageId"], &identity.MessageID)
		if i%2 == 0 {
			if identity.MessageID != wantUsers[i/2] {
				t.Fatalf("user messageId=%q，应透传持久 id=%q", identity.MessageID, wantUsers[i/2])
			}
		} else if _, invented := wire["messageId"]; invented {
			t.Fatalf("assistant 没有持久 message id，不得编造：%s", wire["messageId"])
		}
	}
	for i := 0; i < 2; i++ {
		for _, key := range []string{"v4Seq", "messageId"} {
			if !bytes.Equal(beforeWire[i][key], afterWire[i][key]) {
				t.Fatalf("续写前后第 %d 条 %s 不稳定：before=%s after=%s", i, key, beforeWire[i][key], afterWire[i][key])
			}
		}
	}
}

func TestFollowProjectsSameV4IdentityAsLoadWithoutInventingAssistantID(t *testing.T) {
	frames, path := runFakeChildTurn(t, "message-identity-follow", 0, true)
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bySeq := map[int64]v4.Event{}
	for _, ev := range back.Events {
		bySeq[ev.Seq] = ev
	}

	content := 0
	for _, f := range frames {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(f.Data, &top); err != nil {
			t.Fatal(err)
		}
		if f.Type != "user/message" && f.Type != "assistant/message" {
			if _, changed := top["v4Seq"]; changed {
				t.Fatalf("%s 不属于本轮内容身份投影范围：%s", f.Type, f.Data)
			}
			continue
		}
		content++
		var identity struct {
			V4Seq     *int64 `json:"v4Seq"`
			MessageID string `json:"messageId"`
		}
		if err := json.Unmarshal(f.Data, &identity); err != nil || identity.V4Seq == nil {
			t.Fatalf("WS %s 缺 v4Seq：%s err=%v", f.Type, f.Data, err)
		}
		ev, ok := bySeq[*identity.V4Seq]
		if !ok || ev.Type != f.Type {
			t.Fatalf("WS Transport seq=%d 的 v4Seq=%d 找不到同类持久事件", f.Seq, *identity.V4Seq)
		}
		if int64(f.Seq) == *identity.V4Seq {
			t.Fatalf("夹具必须区分 Transport seq 与 v4Seq，二者却都是 %d", f.Seq)
		}
		switch f.Type {
		case "user/message":
			var source struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(ev.Data, &source); err != nil || source.ID == "" || identity.MessageID != source.ID {
				t.Fatalf("user messageId=%q 未透传持久 id=%q err=%v", identity.MessageID, source.ID, err)
			}
		case "assistant/message":
			if identity.MessageID != "" {
				t.Fatalf("assistant 没有持久 message id，不得生成 %q", identity.MessageID)
			}
		}
	}
	if content != 2 {
		t.Fatalf("本轮应有 user/assistant 各一帧，实际 %d：%+v", content, frames)
	}
}
