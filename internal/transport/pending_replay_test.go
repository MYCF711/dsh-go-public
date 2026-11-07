package transport

import (
	"encoding/json"
	"testing"
)

// H13：订阅时要把该会话还挂着的审批重放给新连接，且**不带 seq**。
//
// 带 seq 会让客户端以为历史里多了一条新事件，进而重复渲染同一个气泡；
// 不带 seq 才能表达"这不是新事件，是仍然有效的状态"。
func TestSubscribeReplaysPendingApprovals(t *testing.T) {
	h := NewHub()
	payload := json.RawMessage(`{"requestId":"perm-1","sessionId":"s1"}`)
	h.SetPendingReplay(func(sessionID string) []ServerFrame {
		if sessionID != "s1" {
			return nil
		}
		return []ServerFrame{{Type: "approval/request", Data: payload}}
	})

	sub := &recordingSub{}
	conn := &scriptedConn{sub: sub, frames: []ClientFrame{{T: FrameSub, Topic: "session:s1"}}}
	_ = ServeConn(conn, h)

	var found bool
	for _, f := range sub.frames {
		if f.Type == "approval/request" {
			found = true
			if f.Seq != 0 {
				t.Fatalf("重放的审批不该带 seq，实际 %d", f.Seq)
			}
			if string(f.Data) != string(payload) {
				t.Fatalf("负载要原样重放：%s", f.Data)
			}
		}
	}
	if !found {
		t.Fatalf("断线重连后必须看到挂起的审批，否则子进程会一直等")
	}
}
