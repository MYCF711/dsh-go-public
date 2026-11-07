package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"dshgo/internal/modes"
	"dshgo/internal/sandbox"
	"dshgo/internal/session"
)

func newSandboxTestRegistry(t *testing.T) *modes.Registry {
	t.Helper()
	reg, err := modes.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

var sandboxSessionSeq atomic.Int64

// createSandboxSession 建一份真实 v4 会话（header.agentPreset = preset）。
func createSandboxSession(t *testing.T, home, preset string) string {
	t.Helper()
	id := sanitizeSandboxID(fmt.Sprintf("sess-%s-%s-%d", t.Name(), preset, sandboxSessionSeq.Add(1)))
	lg, err := session.CreateSession(home, t.TempDir(), id, preset)
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	return id
}

func sanitizeSandboxID(id string) string {
	out := make([]rune, 0, len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func mustSandboxStore(t *testing.T, home string) *sandboxModeStore {
	t.Helper()
	store := newSandboxModeStore(home)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	return store
}

// 三档真值：preset → runtime policy，四种 id 都落到明确的档位。
func TestSandboxPresetMappingIsTrueValue(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	cases := map[string]sandbox.Mode{
		modes.DefaultID:      sandbox.ModeWorkspaceWrite,
		"workspace-write":    sandbox.ModeWorkspaceWrite,
		"read-only":          sandbox.ModeReadOnly,
		"danger-full-access": sandbox.ModeDangerFullAccess,
	}
	for preset, want := range cases {
		id := createSandboxSession(t, home, preset)
		src, err := resolveSandboxModeForSession(home, id, reg, store)
		if err != nil {
			t.Fatalf("preset %q: %v", preset, err)
		}
		if src.PresetID != preset || src.PresetMode != want || src.Effective != want {
			t.Fatalf("preset %q 解析 = %+v，期望 mode %s", preset, src, want)
		}
	}
	if _, err := sandboxModeForPresetID("nope", reg); err == nil {
		t.Fatal("未知 preset 必须 fail-loud")
	}
	if _, err := sandboxModeForPresetID("", reg); !errors.Is(err, sandbox.ErrMissingContext) {
		t.Fatalf("空 preset 必须 fail-loud，得到 %v", err)
	}
}

// session.setPermission 必须返回并持久化真实 sandbox mode；更宽一律拒绝。
func TestSandboxSetPermissionPersistsRealMode(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	approvals := newPermissionOverrideStore(home)
	id := createSandboxSession(t, home, modes.DefaultID)

	res, err := sandboxSetPermission(home, id, "read-only", reg, store, approvals)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != sandbox.ModeReadOnly || res.Effective != sandbox.ModeReadOnly || !res.Persisted {
		t.Fatalf("setPermission(read-only) = %+v", res)
	}
	if res.Enforcement != string(sandbox.EnforcementPolicyOnlyNoOSIsolation) {
		t.Fatalf("enforcement 必须是显式的 policy-only: %+v", res)
	}

	// 持久化后换一份 store（模拟重启）仍能读到真值。
	reloaded := mustSandboxStore(t, home)
	src, err := resolveSandboxModeForSession(home, id, reg, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if src.Effective != sandbox.ModeReadOnly || src.Override != sandbox.ModeReadOnly {
		t.Fatalf("重开后的档位 = %+v，期望 read-only", src)
	}

	// 放宽被拒绝：workspace-write preset 不能经 setPermission 升到 danger。
	if _, err := sandboxSetPermission(home, id, "workspace-write", reg, reloaded, approvals); err != nil {
		t.Fatalf("workspace-write preset 下把档位调回 workspace-write 应允许（与 preset 相等）: %v", err)
	}
	if _, err := sandboxSetPermission(home, id, "danger-full-access", reg, reloaded, approvals); !sandboxErrIsWidenDenied(err) {
		t.Fatalf("setPermission 放宽到 danger 必须拒绝，得到 %v", err)
	}

	// 清空覆盖 = 回到 preset 真值。
	cleared, err := sandboxSetPermission(home, id, "", reg, reloaded, approvals)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Persisted || cleared.Mode != sandbox.ModeWorkspaceWrite {
		t.Fatalf("清空后 = %+v，期望回到 workspace-write 且未持久化", cleared)
	}

	// 未知取值 fail-loud。
	if _, err := sandboxSetPermission(home, id, "yolo", reg, reloaded, approvals); !errors.Is(err, sandbox.ErrUnknownMode) {
		t.Fatalf("未知 permission 必须 fail-loud，得到 %v", err)
	}
}

// read-only 会话不允许被 setPermission 升到任何更宽的档位。
func TestSandboxReadOnlySessionCannotWiden(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	approvals := newPermissionOverrideStore(home)
	id := createSandboxSession(t, home, "read-only")

	for _, value := range []string{"workspace-write", "danger-full-access"} {
		if _, err := sandboxSetPermission(home, id, value, reg, store, approvals); !sandboxErrIsWidenDenied(err) {
			t.Fatalf("read-only 会话 setPermission(%q) 必须拒绝，得到 %v", value, err)
		}
	}
	src, err := resolveSandboxModeForSession(home, id, reg, store)
	if err != nil {
		t.Fatal(err)
	}
	if src.Effective != sandbox.ModeReadOnly {
		t.Fatalf("read-only 会话被放宽成了 %s", src.Effective)
	}
}

// 旧审批值仍然可设，但返回值里的 mode 必须是真实 sandbox 档位。
func TestSandboxLegacyApprovalValueStillReturnsRealMode(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	approvals := newPermissionOverrideStore(home)
	id := createSandboxSession(t, home, "read-only")

	res, err := sandboxSetPermission(home, id, "ask", reg, store, approvals)
	if err != nil {
		t.Fatal(err)
	}
	if res.Approval != "ask" {
		t.Fatalf("审批值没回显: %+v", res)
	}
	if res.Mode != sandbox.ModeReadOnly || res.Effective != sandbox.ModeReadOnly {
		t.Fatalf("审批值不能改 sandbox 档位: %+v", res)
	}
	if got := approvals.Get(id); got != "ask" {
		t.Fatalf("审批覆盖没落盘: %q", got)
	}
	if _, err := sandboxSetPermission(home, id, "block", reg, store, approvals); !errors.Is(err, sandbox.ErrUnknownMode) {
		t.Fatalf("未知审批值必须 fail-loud，得到 %v", err)
	}
}

// preset 的持久选择优先于 header；两处都没有必须 fail-loud。
func TestSandboxPresetOverrideBeatsHeaderAndMissingFailsLoud(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	id := createSandboxSession(t, home, modes.DefaultID)

	path := sandboxPresetStatePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"version": 1, "sessions": map[string]string{id: "read-only"}}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := resolveSandboxModeForSession(home, id, reg, store)
	if err != nil {
		t.Fatal(err)
	}
	if src.PresetID != "read-only" || src.Effective != sandbox.ModeReadOnly {
		t.Fatalf("preset 覆盖没生效: %+v", src)
	}

	// 未知 preset id 也必须 fail-loud（不能静默回 default）。
	doc["sessions"] = map[string]string{id: "nope"}
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSandboxModeForSession(home, id, reg, store); err == nil {
		t.Fatal("未知 preset 必须 fail-loud")
	}
}

// 重开/恢复：read-only 仍是 read-only（store 与 header 两条来源都验）。
func TestSandboxResumeKeepsReadOnly(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	id := createSandboxSession(t, home, "read-only")

	// 第一次解析（进程内）。
	first, err := resolveSandboxModeForSession(home, id, reg, store)
	if err != nil {
		t.Fatal(err)
	}
	// 新 store（模拟重启/新进程）。
	second, err := resolveSandboxModeForSession(home, id, reg, mustSandboxStore(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if first.Effective != sandbox.ModeReadOnly || second.Effective != sandbox.ModeReadOnly {
		t.Fatalf("恢复后权限变大: first=%+v second=%+v", first, second)
	}
}

// fork：父会话被收窄过，孩子必须跟着收窄；孩子本身更窄时保持孩子的。
func TestSandboxForkNeverWidens(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	approvals := newPermissionOverrideStore(home)

	parent := createSandboxSession(t, home, modes.DefaultID)
	child := createSandboxSession(t, home, modes.DefaultID)
	if _, err := sandboxSetPermission(home, parent, "read-only", reg, store, approvals); err != nil {
		t.Fatal(err)
	}
	if err := sandboxAdoptOnFork(home, parent, child, reg, store); err != nil {
		t.Fatal(err)
	}
	childMode, err := resolveSandboxModeForSession(home, child, reg, store)
	if err != nil {
		t.Fatal(err)
	}
	if childMode.Effective != sandbox.ModeReadOnly {
		t.Fatalf("fork 孩子权限变大: %+v", childMode)
	}

	// 反向：父 danger、孩子 read-only → 孩子保持 read-only。
	widenParent := createSandboxSession(t, home, "danger-full-access")
	roChild := createSandboxSession(t, home, "read-only")
	if err := sandboxAdoptOnFork(home, widenParent, roChild, reg, store); err != nil {
		t.Fatal(err)
	}
	roMode, err := resolveSandboxModeForSession(home, roChild, reg, store)
	if err != nil {
		t.Fatal(err)
	}
	if roMode.Effective != sandbox.ModeReadOnly {
		t.Fatalf("孩子自己的 read-only 被父会话放大: %+v", roMode)
	}
}

// A/B 隔离：A 的覆盖不落到 B，两个 store 实例读同一份盘也各自解析。
func TestSandboxABIsolation(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	approvals := newPermissionOverrideStore(home)
	a := createSandboxSession(t, home, modes.DefaultID)
	b := createSandboxSession(t, home, modes.DefaultID)

	if _, err := sandboxSetPermission(home, a, "read-only", reg, store, approvals); err != nil {
		t.Fatal(err)
	}
	srcA, err := resolveSandboxModeForSession(home, a, reg, store)
	if err != nil {
		t.Fatal(err)
	}
	srcB, err := resolveSandboxModeForSession(home, b, reg, store)
	if err != nil {
		t.Fatal(err)
	}
	if srcA.Effective != sandbox.ModeReadOnly {
		t.Fatalf("A 的收窄没生效: %+v", srcA)
	}
	if srcB.Effective != sandbox.ModeWorkspaceWrite {
		t.Fatalf("A 的设置污染了 B: %+v", srcB)
	}
}

// 子进程环境：只产出 DSHD_SANDBOX_MODE，不 setenv 到父进程。
func TestSandboxChildEnv(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	id := createSandboxSession(t, home, "read-only")
	env, err := sandboxWriteChildEnv(home, id, reg, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != sandbox.EnvMode+"=read-only" {
		t.Fatalf("child env = %v", env)
	}
	if os.Getenv(sandbox.EnvMode) != "" {
		t.Fatal("helper 把档位写进了父进程环境（A/B 会互相污染）")
	}
	if _, err := sandboxChildEnv("yolo"); !errors.Is(err, sandbox.ErrUnknownMode) {
		t.Fatalf("未知档位必须 fail-loud，得到 %v", err)
	}
}

// 持久文件里的未知档位必须 fail-loud，不能按默认档静默跑。
func TestSandboxStoreRejectsUnknownPersistedValue(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "state", "session-sandbox.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"sessions":{"s":"yolo"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newSandboxModeStore(home)
	if err := store.Load(); !errors.Is(err, sandbox.ErrUnknownMode) {
		t.Fatalf("未知持久档位必须 fail-loud，得到 %v", err)
	}
	// 损坏 JSON 同理。
	if err := os.WriteFile(path, []byte(`{"version":2,"sessions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := newSandboxModeStore(home).Load(); err == nil {
		t.Fatal("非法 version 必须 fail-loud")
	}
}

// 响应形状：Lead 接 session.setPermission 时直接用这几个字段。
func TestSandboxPermissionResultShape(t *testing.T) {
	res := sandboxPermissionResult{
		Action: string(sandbox.ActionAllow), Mode: sandbox.ModeReadOnly, Effective: sandbox.ModeReadOnly,
		Preset: "read-only", Persisted: true, Enforcement: string(sandbox.EnforcementPolicyOnlyNoOSIsolation),
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mode", "effective_mode", "preset", "persisted", "enforcement", "action"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("响应缺字段 %q: %s", key, raw)
		}
	}
	if got["mode"] != "read-only" || got["enforcement"] != "policy-only-no-os-isolation" {
		t.Fatalf("响应字段值不对: %s", raw)
	}
}

// 切 preset = 重新定基线：旧的收窄覆盖必须清掉，否则会出现"比新 preset 更宽"的非法组合。
func TestSandboxPresetSelectClearsStaleOverride(t *testing.T) {
	home := t.TempDir()
	reg := newSandboxTestRegistry(t)
	store := mustSandboxStore(t, home)
	approvals := newPermissionOverrideStore(home)
	id := createSandboxSession(t, home, modes.DefaultID)
	if _, err := sandboxSetPermission(home, id, "read-only", reg, store, approvals); err != nil {
		t.Fatal(err)
	}
	// 模拟 agentPresetSelect 把 preset 改成 read-only（写 state/session-preset.json）。
	path := sandboxPresetStatePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "sessions": map[string]string{id: "read-only"}})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧覆盖是 workspace-write（比新 preset 宽），不清理会让 resolve fail-loud。
	if err := store.Set(id, sandbox.ModeWorkspaceWrite); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSandboxModeForSession(home, id, reg, store); !errors.Is(err, sandbox.ErrWidenDenied) {
		t.Fatalf("更宽的旧覆盖应 fail-loud，得到 %v", err)
	}
	if err := sandboxOnPresetSelected(id, "read-only", reg, store); err != nil {
		t.Fatal(err)
	}
	src, err := resolveSandboxModeForSession(home, id, reg, store)
	if err != nil || src.Effective != sandbox.ModeReadOnly || src.Override != "" {
		t.Fatalf("切 preset 后 = %+v, %v；期望 read-only 且无覆盖", src, err)
	}
	if err := sandboxOnPresetSelected(id, "nope", reg, store); err == nil {
		t.Fatal("未知 preset 必须 fail-loud")
	}
}
