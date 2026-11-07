package modes

import (
	"fmt"

	"dshgo/internal/spi"
)

// View 是"按模式裁剪过的工具表"。
//
// 它只做减法：**不能注册**（注册走真正的 registry）。
// 模型看到的就是这个视图，所以工具表在会话内稳定。
type View struct {
	inner spi.ToolRegistry
	mode  Mode
}

// Filter 用模式裁剪一个 registry。
func Filter(inner spi.ToolRegistry, m Mode) spi.ToolRegistry {
	if inner == nil {
		return nil
	}
	return &View{inner: inner, mode: m}
}

func (v *View) Register(spi.Tool) error {
	return fmt.Errorf("模式视图是只读的，不能注册工具")
}

func (v *View) Tools() []spi.Tool {
	all := v.inner.Tools()
	if len(v.mode.Tools) == 0 {
		return all
	}
	out := make([]spi.Tool, 0, len(all))
	for _, t := range all {
		if v.mode.Allows(t.Name()) {
			out = append(out, t)
		}
	}
	return out
}

func (v *View) Get(name string) (spi.Tool, bool) {
	if !v.mode.Allows(name) {
		return nil, false
	}
	return v.inner.Get(name)
}
