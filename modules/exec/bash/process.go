package bash

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"dshgo/internal/spi"
)

// processIdentity 全部来自 OS 查询；Active=false 与查询错误不是一回事。
type processIdentity struct {
	PID            int
	Image          string
	CreationID     string
	Active         bool
	ProcessTree    string
	ProcessGroupID int
}

type processTreeNative interface {
	terminate() error
	release() error
	terminateOnClose(validRoot bool) bool
}

// processControl 的锁同时串行化 Stop/Close 与测试中的身份快照。
// native 的句柄只服务本次启动的树，绝不按镜像名或全局枚举杀进程。
type processControl struct {
	mu       sync.Mutex
	identity processIdentity
	native   processTreeNative
	closed   bool
}

func (c *processControl) metadata() processIdentity {
	if c == nil {
		return processIdentity{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.identity
}

func sameProcessImage(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func validateProcessIdentity(want, got processIdentity) error {
	if !got.Active {
		return spi.ErrJobProcessExited
	}
	if want.PID != got.PID || want.CreationID == "" || want.CreationID != got.CreationID ||
		want.Image == "" || !sameProcessImage(want.Image, got.Image) {
		return fmt.Errorf("%w: pid=%d", spi.ErrJobProcessReused, want.PID)
	}
	return nil
}

func (c *processControl) stop() error {
	if c == nil || c.native == nil {
		return spi.ErrProcessTreeUnsafe
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return spi.ErrJobFinished
	}
	got, err := queryProcessIdentity(c.identity.PID)
	if err != nil {
		if errors.Is(err, spi.ErrJobProcessExited) {
			return err
		}
		return fmt.Errorf("%w: 查询 root PID %d: %v", spi.ErrProcessTreeUnsafe, c.identity.PID, err)
	}
	if err := validateProcessIdentity(c.identity, got); err != nil {
		return err
	}
	if err := c.native.terminate(); err != nil {
		return fmt.Errorf("终止已核对的进程树: %w", err)
	}
	c.closed = true
	if err := c.native.release(); err != nil {
		return fmt.Errorf("进程树已终止，释放归属句柄: %w", err)
	}
	return nil
}

// close 是 Dispose 的强制收工。Windows 的 Job 句柄不依赖 PID，可在 root 已退出时
// 仍终止该 Job 里残留的自有子进程；Unix 只有 root 身份仍匹配时才对 PGID 发信号，
// 避免一个已经消失的 PGID 被复用后误伤别人。
func (c *processControl) close() {
	if c == nil || c.native == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	valid := false
	if got, err := queryProcessIdentity(c.identity.PID); err == nil {
		valid = validateProcessIdentity(c.identity, got) == nil
	}
	if c.native.terminateOnClose(valid) {
		_ = c.native.terminate()
	}
	_ = c.native.release()
	c.closed = true
}
