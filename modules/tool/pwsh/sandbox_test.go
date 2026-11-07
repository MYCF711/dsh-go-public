package pwsh

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/sandbox"
	"dshgo/internal/spi"
)

func gateFor(t *testing.T, mode sandbox.Mode, workspace string) *sandbox.Gate {
	t.Helper()
	g, err := sandbox.NewGate(sandbox.ProcessConfig{Mode: mode, WorkspaceRoot: workspace, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func ctxWithSession(id string) context.Context {
	return spi.WithSession(context.Background(), id)
}

func pwshArgs(t *testing.T, command, workdir string, extra map[string]any) json.RawMessage {
	t.Helper()
	args := map[string]any{"command": command}
	if workdir != "" {
		args["workdir"] = workdir
	}
	for k, v := range extra {
		args[k] = v
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// countSpawns 把 pwsh 的进程构造点换成计数桩，返回"启动了几次"的探针。
func countSpawns(t *testing.T) *int {
	t.Helper()
	old := execCommandContext
	calls := 0
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		calls++
		return old(ctx, name, arg...)
	}
	t.Cleanup(func() { execCommandContext = old })
	return &calls
}

// read-only：策略拒绝发生在进程构造之前——零进程、marker 不存在。
func TestSandboxReadOnlyDeniesBeforeProcess(t *testing.T) {
	spawns := countSpawns(t)
	ws := t.TempDir()
	marker := filepath.Join(ws, "pwsh-must-not-exist.txt")
	tool := &Tool{gate: gateFor(t, sandbox.ModeReadOnly, ws)}
	_, err := tool.Exec(ctxWithSession("session-ro"), pwshArgs(t,
		"Set-Content -Path '"+strings.ReplaceAll(marker, `\`, "/")+"' -Value ran", "", nil))
	if !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("read-only 命令必须被策略拒绝，得到 %v", err)
	}
	if *spawns != 0 {
		t.Fatalf("deny 之后仍然构造了 %d 次进程", *spawns)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("deny 之后 PowerShell 还是启动了（marker 存在）")
	}
}

func TestSandboxNeedsApprovalDoesNotStartProcess(t *testing.T) {
	ws := t.TempDir()
	marker := filepath.Join(ws, "pwsh-unapproved.txt")
	tool := &Tool{gate: gateFor(t, sandbox.ModeWorkspaceWrite, ws)}
	spawns := countSpawns(t)
	_, err := tool.Exec(ctxWithSession("session-ww"), pwshArgs(t,
		"Set-Content -Path '"+strings.ReplaceAll(marker, `\`, "/")+"' -Value ran", "",
		map[string]any{"sandbox_permissions": "danger-full-access"}))
	if !errors.Is(err, sandbox.ErrNeedsApproval) {
		t.Fatalf("未审批升级应 needs-approval，得到 %v", err)
	}
	if *spawns != 0 {
		t.Fatalf("needs-approval 之后仍然构造了 %d 次进程", *spawns)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("needs-approval 之后 PowerShell 还是启动了")
	}
	if tool.lastDecision == nil || tool.lastDecision.Approval == nil || !tool.lastDecision.Approval.OneShot {
		t.Fatalf("结果缺少一次性审批需求: %+v", tool.lastDecision)
	}
}

// 已批准的一次性升级只在当前调用生效；后续调用重新要求审批。
func TestSandboxApprovedOneShotOnlyAppliesToThatCall(t *testing.T) {
	if _, _, err := resolveShell(); err != nil {
		t.Skip("本机没有 PowerShell：", err)
	}
	ws := t.TempDir()
	outside := t.TempDir()
	gate := gateFor(t, sandbox.ModeWorkspaceWrite, ws)
	tool := &Tool{gate: gate}
	ctx := ctxWithSession("session-ww")

	first := filepath.Join(outside, "pwsh-approved.txt")
	second := filepath.Join(outside, "pwsh-second.txt")
	command := "Set-Content -Path '"
	pending, err := gate.EvaluateCommand(ctx, command, outside, sandbox.PermissionDangerFullAccess)
	if err != nil || pending.Action != sandbox.ActionNeedsApproval || pending.Approval == nil {
		t.Fatalf("未审批判定 = %+v, %v", pending, err)
	}
	approvedCtx := sandbox.WithEscalation(ctx, pending.Approval.Decide(true))
	if _, err := tool.Exec(approvedCtx, pwshArgs(t, command+strings.ReplaceAll(first, `\`, "/")+"' -Value ok",
		outside, map[string]any{"sandbox_permissions": "danger-full-access"})); err != nil {
		t.Fatalf("已批准的一次性升级应执行: %v", err)
	}
	if _, statErr := os.Stat(first); statErr != nil {
		t.Fatalf("已批准调用没有真正执行: %v", statErr)
	}
	_, err = tool.Exec(ctx, pwshArgs(t, command+strings.ReplaceAll(second, `\`, "/")+"' -Value no",
		outside, map[string]any{"sandbox_permissions": "danger-full-access"}))
	if !errors.Is(err, sandbox.ErrNeedsApproval) {
		t.Fatalf("后续调用必须重新审批，得到 %v", err)
	}
	if _, statErr := os.Stat(second); statErr == nil {
		t.Fatal("一次性批准漂移到了后续调用")
	}
}

func TestSandboxUnwiredToolRefusesEscalation(t *testing.T) {
	ws := t.TempDir()
	marker := filepath.Join(ws, "pwsh-unwired.txt")
	tool := &Tool{}
	_, err := tool.Exec(ctxWithSession("session-x"), pwshArgs(t,
		"Set-Content -Path '"+strings.ReplaceAll(marker, `\`, "/")+"' -Value ran", "",
		map[string]any{"sandbox_permissions": "danger-full-access"}))
	if !errors.Is(err, sandbox.ErrPolicyUnwired) {
		t.Fatalf("未接线时显式升级必须 fail-loud，得到 %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("未接线时仍执行了带 sandbox_permissions 的调用")
	}
}

func TestSandboxUnknownPermissionFailsLoud(t *testing.T) {
	tool := &Tool{gate: gateFor(t, sandbox.ModeWorkspaceWrite, t.TempDir())}
	_, err := tool.Exec(ctxWithSession("session-x"), pwshArgs(t, "Get-Date", "", map[string]any{"sandbox_permissions": "yolo"}))
	if !errors.Is(err, sandbox.ErrUnknownPermission) {
		t.Fatalf("未知 sandbox_permissions 必须 fail-loud，得到 %v", err)
	}
}

func TestSandboxNewToolFailsLoudOnUnknownEnv(t *testing.T) {
	t.Setenv(sandbox.EnvMode, "yolo")
	if _, err := newTool(); !errors.Is(err, sandbox.ErrUnknownMode) {
		t.Fatalf("未知环境档位必须 fail-loud，得到 %v", err)
	}
}
