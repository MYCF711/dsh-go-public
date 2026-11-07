package transport

import (
	"errors"
	"io"
	"testing"
	"time"
)

// idleConn 会一直阻塞在 ReadClientFrame 上（模拟半开连接），并实现 Pinger/Closer。
type idleConn struct {
	pings  int
	closed chan struct{}
}

func (c *idleConn) ReadClientFrame() (ClientFrame, error) {
	<-c.closed
	return ClientFrame{}, io.ErrClosedPipe
}
func (c *idleConn) WriteServerFrame(ServerFrame) error { return nil }
func (c *idleConn) Ping() error                        { c.pings++; return nil }
func (c *idleConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// 长时间没有客户端帧 → 必须自己判定死亡并返回 ErrConnIdle，
// 而不是一直阻塞在 ReadClientFrame 上（半开连接不会报错）。
func TestServeConnReturnsIdleAfterSilence(t *testing.T) {
	oldPing, oldIdle := pingEvery, idleLimit
	pingEvery, idleLimit = 20*time.Millisecond, 50*time.Millisecond
	defer func() { pingEvery, idleLimit = oldPing, oldIdle }()

	c := &idleConn{closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- ServeConn(c, NewHub()) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrConnIdle) {
			t.Fatalf("要 ErrConnIdle，拿到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时了还没返回——说明心跳没起作用")
	}
}

// 内存管道那种连接没有 Pinger：不许凭空加一个空实现来"看起来验过了"。
func TestHeartbeatSkipsNonPinger(t *testing.T) {
	c := &idleConn{closed: make(chan struct{})}
	var got bool
	heartbeat(struct{ Conn }{c}, 10*time.Millisecond, 10*time.Millisecond, func() { got = true })
	time.Sleep(40 * time.Millisecond)
	if got {
		t.Fatal("没有 Ping 能力的连接不该被判定超时")
	}
}
