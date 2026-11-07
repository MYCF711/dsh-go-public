// Package modes 是"模式（preset）"：决定一次会话里模型能看到哪些工具。
//
// PLAN §6 的三条硬规矩：
//  1. **内置模式只读**（`Builtin = true`）——用户层 patch 命中即报错，不静默覆盖；
//  2. 工具可见性由模式决定，**模式在一次会话内不变** → 工具表稳定，不击穿前缀缓存；
//  3. 装卸类工具只在用户自建模式下可见（内置模式一律不给）。
package modes

import (
	"fmt"

	"dshgo/internal/sandbox"
)

// Mode 是一个模式。
type Mode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Builtin     bool   `json:"builtin"`
	// Tools 是白名单；为空表示"全部可见"。
	Tools []string `json:"tools,omitempty"`
	// Approval 是该模式的审批倾向（空 = 沿用宿主配置）。
	Approval string `json:"approval,omitempty"`
	// Sandbox 是该模式对应的真实 runtime sandbox 档位：
	// read-only / workspace-write / danger-full-access。
	//
	// 空 = workspace-write（安全默认：只收窄，不放宽）。显式写了未知值，
	// 装配期 LoadDir 与 SandboxMode 都会 fail-loud，不静默降级。
	Sandbox string `json:"sandbox,omitempty"`
}

// SandboxMode 把会话 mode 映射成 runtime sandbox 档位。
//
// 映射是**真值**，不是显示值：工具、FS、命令闸门都按它判定。
// 未知 sandbox 字段给出 ErrUnknownMode，绝不回落到某一档。
func (m Mode) SandboxMode() (sandbox.Mode, error) {
	raw := m.Sandbox
	if raw == "" {
		return sandbox.ModeWorkspaceWrite, nil
	}
	mode, err := sandbox.ParseMode(raw)
	if err != nil {
		return "", fmt.Errorf("模式 %q 的 sandbox=%q 无效: %w", m.ID, m.Sandbox, err)
	}
	return mode, nil
}

// Allows 判断某个工具在这个模式下是否可见。
func (m Mode) Allows(tool string) bool {
	if len(m.Tools) == 0 {
		return true
	}
	for _, t := range m.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// DefaultID 是默认模式。
const DefaultID = "default"

// Builtins 返回编译进二进制的内置模式（只读）。
func Builtins() []Mode {
	return []Mode{
		{
			ID:          DefaultID,
			Name:        "Default",
			Description: "全部工具可用；写入限制在 canonical 工作区内（workspace-write 档）。",
			Builtin:     true,
			Sandbox:     string(sandbox.ModeWorkspaceWrite),
		},
		{
			ID:          "read-only",
			Name:        "Read only",
			Description: "只能读：read / glob / grep / history_search。用来审阅代码或做排查。",
			Builtin:     true,
			Tools:       []string{"read", "glob", "grep", "history_search"},
			Approval:    "deny",
			Sandbox:     string(sandbox.ModeReadOnly),
		},
		{
			ID:          "workspace-write",
			Name:        "Workspace write",
			Description: "全部工具可用；只允许写 canonical 工作区，命令 cwd 也必须在工作区内。",
			Builtin:     true,
			Sandbox:     string(sandbox.ModeWorkspaceWrite),
		},
		{
			ID:          "danger-full-access",
			Name:        "Danger full access",
			Description: "全部工具可用；不虚构路径沙盒，写/执行可越出工作区（仍保留审批与读后写）。",
			Builtin:     true,
			Sandbox:     string(sandbox.ModeDangerFullAccess),
		},
	}
}

// Lookup 按 id 找内置模式。
func Lookup(id string) (Mode, bool) {
	if id == "" {
		id = DefaultID
	}
	for _, m := range Builtins() {
		if m.ID == id {
			return m, true
		}
	}
	return Mode{}, false
}

// Resolve 返回模式；未知模式返回错误（**不静默回落到 default**）。
func Resolve(id string) (Mode, error) {
	m, ok := Lookup(id)
	if !ok {
		return Mode{}, fmt.Errorf("未知模式 %q（可选：%s）", id, IDs())
	}
	return m, nil
}

// IDs 列出所有内置模式 id。
func IDs() string {
	ms := Builtins()
	out := ""
	for i, m := range ms {
		if i > 0 {
			out += ", "
		}
		out += m.ID
	}
	return out
}

// OverrideBuiltin 检查"用户层是否试图改写内置模式"。命中即报错（PLAN §6）。
func OverrideBuiltin(id string) error {
	if m, ok := Lookup(id); ok && m.Builtin {
		return fmt.Errorf("模式 %q 是内置的，只读；想改就从它复制一份新 id", id)
	}
	return nil
}
