package bash

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/internal/sandbox"
	"dshgo/internal/spi"
)

func gateFor(t *testing.T, mode sandbox.Mode, workspace string) *sandbox.Gate {
	t.Helper()
	home := t.TempDir()
	g, err := sandbox.NewGate(sandbox.ProcessConfig{Mode: mode, WorkspaceRoot: workspace, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func toolFor(g *sandbox.Gate, svc *Service) *shellTool {
	return &shellTool{svc: svc, budget: 10 * time.Second, gate: g}
}

func sessionArgs(t *testing.T, command string, extra map[string]any) json.RawMessage {
	t.Helper()
	args := map[string]any{"command": command, "description": "sandbox probe"}
	for k, v := range extra {
		args[k] = v
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ctxWithSession(id string) context.Context {
	return spi.WithSession(context.Background(), id)
}

// read-only：判定发生在任何进程动作之前——shell 会话没建、job 表为空、文件没写。
func TestSandboxReadOnlyDeniesBeforeProcess(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	marker := filepath.Join(ws, "must-not-exist.txt")
	svc := NewService(ws)
	defer svc.Close()
	tool := toolFor(gateFor(t, sandbox.ModeReadOnly, ws), svc)

	_, err := tool.Exec(ctxWithSession("session-ro"), sessionArgs(t,
		"printf ran > '"+strings.ReplaceAll(marker, `\`, "/")+"'", nil))
	if !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("read-only 命令必须被策略拒绝，得到 %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("deny 之后进程还是启动了（marker 存在）")
	}
	if svc.sess != nil {
		t.Fatal("deny 之后仍然建立了 bash 会话（进程已启动）")
	}
	if jobs := svc.List(); len(jobs) != 0 {
		t.Fatalf("deny 之后 job 表非空: %+v", jobs)
	}
}

// 未审批的 danger-full-access：不得启动进程，也不得留下 job；结果里要有结构化审批需求。
func TestSandboxNeedsApprovalDoesNotStartProcess(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	marker := filepath.Join(ws, "unapproved.txt")
	svc := NewService(ws)
	defer svc.Close()
	tool := toolFor(gateFor(t, sandbox.ModeWorkspaceWrite, ws), svc)

	for _, background := range []bool{false, true} {
		res, err := tool.Exec(ctxWithSession("session-ww"), sessionArgs(t,
			"printf ran > '"+strings.ReplaceAll(marker, `\`, "/")+"'",
			map[string]any{"sandbox_permissions": "danger-full-access", "run_in_background": background}))
		if !errors.Is(err, sandbox.ErrNeedsApproval) {
			t.Fatalf("background=%v 未审批升级应 needs-approval，得到 %v", background, err)
		}
		data, ok := res.Data.(map[string]any)
		if !ok {
			t.Fatalf("拒绝结果缺结构化 Data: %+v", res.Data)
		}
		decision, ok := data["sandbox"].(*sandbox.Decision)
		if !ok || decision.Approval == nil || !decision.Approval.OneShot {
			t.Fatalf("拒绝结果缺少一次性审批需求: %+v", data["sandbox"])
		}
		if strings.TrimSpace(res.Hint) == "" {
			t.Fatal("拒绝结果必须给 Hint")
		}
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("needs-approval 之后进程还是启动了（marker 存在）")
	}
	if svc.sess != nil {
		t.Fatal("needs-approval 之后仍然建立了 bash 会话")
	}
	if jobs := svc.List(); len(jobs) != 0 {
		t.Fatalf("needs-approval 之后 job 表非空: %+v", jobs)
	}
}

// 已批准的一次性升级：只对这一次调用生效；下一次同参数调用重新 needs-approval。
func TestSandboxApprovedOneShotOnlyAppliesToThatCall(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	outside := t.TempDir()
	gate := gateFor(t, sandbox.ModeWorkspaceWrite, ws)
	// 命令 cwd 就是会话 cwd：要越界执行，会话本身必须开在工作区外。
	svc := NewService(outside)
	defer svc.Close()
	tool := toolFor(gate, svc)
	ctx := ctxWithSession("session-ww")

	command := "printf approved > '"
	first := filepath.Join(outside, "approved.txt")
	second := filepath.Join(outside, "second.txt")

	pending, err := gate.EvaluateCommand(ctx, command, outside, sandbox.PermissionDangerFullAccess)
	if err != nil || pending.Action != sandbox.ActionNeedsApproval || pending.Approval == nil {
		t.Fatalf("未审批判定 = %+v, %v", pending, err)
	}
	approvedCtx := sandbox.WithEscalation(ctx, pending.Approval.Decide(true))

	if _, err := tool.Exec(approvedCtx, sessionArgs(t, command+strings.ReplaceAll(first, `\`, "/")+"'",
		map[string]any{"sandbox_permissions": "danger-full-access"})); err != nil {
		t.Fatalf("已批准的一次性升级应执行: %v", err)
	}
	if _, statErr := os.Stat(first); statErr != nil {
		t.Fatalf("已批准调用没有真正执行: %v", statErr)
	}

	// 后续调用（没有审批）不得继承上一次的批准。
	_, err = tool.Exec(ctx, sessionArgs(t, command+strings.ReplaceAll(second, `\`, "/")+"'",
		map[string]any{"sandbox_permissions": "danger-full-access"}))
	if !errors.Is(err, sandbox.ErrNeedsApproval) {
		t.Fatalf("后续调用必须重新审批，得到 %v", err)
	}
	if _, statErr := os.Stat(second); statErr == nil {
		t.Fatal("一次性批准漂移到了后续调用")
	}
}

// workspace-write：cwd 在工作区外时 deny，零进程。
func TestSandboxCommandCwdOutsideWorkspaceDenied(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	outside := t.TempDir()
	svc := NewService(outside)
	defer svc.Close()
	tool := toolFor(gateFor(t, sandbox.ModeWorkspaceWrite, ws), svc)

	_, err := tool.Exec(ctxWithSession("session-ww"), sessionArgs(t, "pwd", nil))
	if !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("工作区外 cwd 必须 deny，得到 %v", err)
	}
	if svc.sess != nil {
		t.Fatal("deny 之后仍然建立了 bash 会话")
	}
}

// 策略层未接线：普通命令保持旧行为，但显式 sandbox_permissions 必须 fail-loud。
func TestSandboxUnwiredToolRefusesEscalation(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	marker := filepath.Join(ws, "unwired.txt")
	svc := NewService(ws)
	defer svc.Close()
	tool := toolFor(nil, svc)

	_, err := tool.Exec(ctxWithSession("session-x"), sessionArgs(t,
		"printf ran > '"+strings.ReplaceAll(marker, `\`, "/")+"'",
		map[string]any{"sandbox_permissions": "danger-full-access"}))
	if !errors.Is(err, sandbox.ErrPolicyUnwired) {
		t.Fatalf("未接线时显式升级必须 fail-loud，得到 %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("未接线时仍执行了带 sandbox_permissions 的调用")
	}
	if svc.sess != nil {
		t.Fatal("未接线时仍建立了 bash 会话")
	}
}

// 未知 sandbox_permissions 取值 fail-loud。
func TestSandboxUnknownPermissionFailsLoud(t *testing.T) {
	ws := t.TempDir()
	svc := NewService(ws)
	defer svc.Close()
	tool := toolFor(gateFor(t, sandbox.ModeWorkspaceWrite, ws), svc)
	_, err := tool.Exec(ctxWithSession("session-x"), sessionArgs(t, "true",
		map[string]any{"sandbox_permissions": "yolo"}))
	if !errors.Is(err, sandbox.ErrUnknownPermission) {
		t.Fatalf("未知 sandbox_permissions 必须 fail-loud，得到 %v", err)
	}
	if svc.sess != nil {
		t.Fatal("未知 permission 仍建立了 bash 会话")
	}
}

// workspace-write 工作区内命令正常放行（策略不是"一刀切拒绝"）。
func TestSandboxWorkspaceWriteAllowsInsideCommand(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	svc := NewService(ws)
	defer svc.Close()
	tool := toolFor(gateFor(t, sandbox.ModeWorkspaceWrite, ws), svc)
	res, err := tool.Exec(ctxWithSession("session-ww"), sessionArgs(t, "printf hi > inside.txt", nil))
	if err != nil {
		t.Fatalf("工作区内命令应允许: %v", err)
	}
	if res.Text == "" {
		t.Fatalf("命令结果为空: %+v", res)
	}
	if _, statErr := os.Stat(filepath.Join(ws, "inside.txt")); statErr != nil {
		t.Fatalf("命令没有真正执行: %v", statErr)
	}
}
