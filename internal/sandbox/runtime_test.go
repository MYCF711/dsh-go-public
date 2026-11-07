package sandbox

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"dshgo/internal/spi"
)

func TestParseModeAndPermissionFailLoud(t *testing.T) {
	for _, raw := range []string{"read-only", "workspace-write", "danger-full-access"} {
		got, err := ParseMode(raw)
		if err != nil || string(got) != raw {
			t.Fatalf("ParseMode(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "default", "allow", "workspace_write", " workspace-write"} {
		if _, err := ParseMode(raw); !errors.Is(err, ErrUnknownMode) {
			t.Fatalf("ParseMode(%q) 应 fail-loud，得到 %v", raw, err)
		}
	}
	if p, err := ParsePermission(""); err != nil || p != "" {
		t.Fatalf("空 permission 应表示不请求变化: %q %v", p, err)
	}
	for _, raw := range []string{"workspace-write", "danger-full-access"} {
		if got, err := ParsePermission(raw); err != nil || string(got) != raw {
			t.Fatalf("ParsePermission(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"read-only", "yolo", "allow"} {
		if _, err := ParsePermission(raw); !errors.Is(err, ErrUnknownPermission) {
			t.Fatalf("ParsePermission(%q) 应 fail-loud，得到 %v", raw, err)
		}
	}
}

func TestMostRestrictiveAndCompare(t *testing.T) {
	pairs := []struct {
		a, b Mode
		want Mode
	}{
		{ModeDangerFullAccess, ModeReadOnly, ModeReadOnly},
		{ModeWorkspaceWrite, ModeWorkspaceWrite, ModeWorkspaceWrite},
		{ModeDangerFullAccess, ModeWorkspaceWrite, ModeWorkspaceWrite},
		{ModeReadOnly, ModeReadOnly, ModeReadOnly},
	}
	for _, tt := range pairs {
		got, err := MostRestrictive(tt.a, tt.b)
		if err != nil || got != tt.want {
			t.Fatalf("MostRestrictive(%s,%s) = %s, %v; want %s", tt.a, tt.b, got, err, tt.want)
		}
	}
	if _, err := MostRestrictive("yolo", ModeReadOnly); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("未知档位应 fail-loud: %v", err)
	}
	if n, err := CompareModes(ModeDangerFullAccess, ModeReadOnly); err != nil || n <= 0 {
		t.Fatalf("CompareModes = %d, %v", n, err)
	}
	if n, err := CompareModes(ModeReadOnly, ModeDangerFullAccess); err != nil || n >= 0 {
		t.Fatalf("CompareModes = %d, %v", n, err)
	}
}

func TestBoundaryThreeTiersAndCanonicalSymlink(t *testing.T) {
	f := newPolicyFixture(t)

	readOnly, err := NewBoundary(ModeReadOnly, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.CheckWrite(filepath.Join(f.workspace, "new", "deep", "file.txt")); !errors.Is(err, ErrReadOnlyWrite) {
		t.Fatalf("read-only 必须在写盘前拒绝（含工作区内新建），得到 %v", err)
	}

	workspaceWrite, err := NewBoundary(ModeWorkspaceWrite, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceWrite.CheckWrite("inside.txt"); err != nil {
		t.Fatalf("工作区内写应允许: %v", err)
	}
	if _, err := workspaceWrite.CheckWrite(filepath.Join(f.outside, "x.txt")); !errors.Is(err, ErrWriteOutsideWorkspace) {
		t.Fatalf("工作区外写必须拒绝: %v", err)
	}
	if _, err := workspaceWrite.CheckWrite(f.workspace + "-evil/x.txt"); !errors.Is(err, ErrWriteOutsideWorkspace) {
		t.Fatalf("前缀兄弟目录必须拒绝: %v", err)
	}

	link := filepath.Join(f.workspace, "link")
	makeDirectoryLink(t, link, f.outside)
	if _, err := workspaceWrite.CheckWrite(filepath.Join(link, "not-created", "deep", "file.txt")); !errors.Is(err, ErrWriteOutsideWorkspace) {
		t.Fatalf("符号链接指向工作区外时必须按 canonical 路径拒绝: %v", err)
	}

	danger, err := NewBoundary(ModeDangerFullAccess, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := danger.CheckWrite(filepath.Join(f.outside, "x.txt")); err != nil {
		t.Fatalf("danger-full-access 不虚构路径沙盒: %v", err)
	}
	if _, err := NewBoundary("yolo", f.workspace); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("未知档位必须 fail-loud: %v", err)
	}
}

func TestBoundaryFromEnvDefaultAndUnknown(t *testing.T) {
	f := newPolicyFixture(t)
	t.Setenv(EnvMode, "")
	b, err := BoundaryFromEnv(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode() != ModeWorkspaceWrite {
		t.Fatalf("缺省档位 = %s，期望 workspace-write（只收窄不放宽）", b.Mode())
	}
	t.Setenv(EnvMode, "read-only")
	b, err = BoundaryFromEnv(f.workspace)
	if err != nil || b.Mode() != ModeReadOnly {
		t.Fatalf("环境档位没生效: %v %v", b, err)
	}
	t.Setenv(EnvMode, "yolo")
	if _, err := BoundaryFromEnv(f.workspace); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("未知环境档位必须 fail-loud: %v", err)
	}
}

func newTestGate(t *testing.T, mode Mode, f policyFixture) *Gate {
	t.Helper()
	g, err := NewGate(ProcessConfig{
		Mode: mode, WorkspaceRoot: f.workspace, HomeDir: f.home,
		ExtraReadRoots: []string{f.extra},
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func sessionCtx(session string) context.Context {
	return spi.WithSession(context.Background(), session)
}

func TestGateRequiresSessionContextAndRejectsUnknownPermission(t *testing.T) {
	f := newPolicyFixture(t)
	g := newTestGate(t, ModeWorkspaceWrite, f)
	if _, err := g.EvaluateCommand(context.Background(), "true", f.workspace, ""); !errors.Is(err, ErrMissingContext) {
		t.Fatalf("缺会话上下文必须 fail-loud: %v", err)
	}
	if _, err := g.EvaluateCommand(sessionCtx("session-A"), "true", f.workspace, "yolo"); !errors.Is(err, ErrUnknownPermission) {
		t.Fatalf("未知 sandbox_permissions 必须 fail-loud: %v", err)
	}
	if _, err := g.EvaluateCommand(sessionCtx("session-A"), "", f.workspace, ""); !errors.Is(err, ErrMissingContext) {
		t.Fatalf("空命令必须 fail-loud: %v", err)
	}
}

func TestGateThreeTiersAndABIsolation(t *testing.T) {
	f := newPolicyFixture(t)
	roGate := newTestGate(t, ModeReadOnly, f)
	d, err := roGate.EvaluateCommand(sessionCtx("session-A"), "touch x", f.workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDeny || d.Reason != ReasonReadOnlyExecute {
		t.Fatalf("read-only 命令判定 = %+v", d)
	}

	wwGate := newTestGate(t, ModeWorkspaceWrite, f)
	d, err = wwGate.EvaluateCommand(sessionCtx("session-B"), "true", f.workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionAllow || d.Reason != ReasonWorkspaceCommand {
		t.Fatalf("workspace-write 工作区内命令判定 = %+v", d)
	}
	d, err = wwGate.EvaluateCommand(sessionCtx("session-B"), "true", f.outside, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDeny || d.Reason != ReasonCommandOutsideWorkspace {
		t.Fatalf("workspace-write 工作区外 cwd 判定 = %+v", d)
	}

	dangerGate := newTestGate(t, ModeDangerFullAccess, f)
	d, err = dangerGate.EvaluateCommand(sessionCtx("session-C"), "true", f.outside, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionAllow || d.Reason != ReasonDangerFullAccess {
		t.Fatalf("danger-full-access 命令判定 = %+v", d)
	}

	// A/B 隔离：同一个 Gate 里两个会话各自的 Policy 不共享可变状态。
	// read-only gate 永远 deny；workspace gate 永远 allow；互相不影响。
	if d, _ := roGate.EvaluateCommand(sessionCtx("session-B"), "true", f.workspace, ""); d.Action != ActionDeny {
		t.Fatalf("read-only gate 被 B 会话污染: %+v", d)
	}
	if d, _ := wwGate.EvaluateCommand(sessionCtx("session-A"), "true", f.workspace, ""); d.Action != ActionAllow {
		t.Fatalf("workspace gate 被 A 会话污染: %+v", d)
	}
}

func TestGateOneShotApprovalBinding(t *testing.T) {
	f := newPolicyFixture(t)
	g := newTestGate(t, ModeWorkspaceWrite, f)
	ctx := sessionCtx("session-A")
	d, err := g.EvaluateCommand(ctx, "true", f.outside, PermissionDangerFullAccess)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionNeedsApproval || d.Approval == nil || !d.Approval.OneShot {
		t.Fatalf("未审批的越界命令应 needs-approval: %+v", d)
	}
	approval := d.Approval.Decide(true)

	approved, err := g.EvaluateCommand(WithEscalation(ctx, approval), "true", f.outside, PermissionDangerFullAccess)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Action != ActionAllow || approved.EffectiveMode != ModeDangerFullAccess {
		t.Fatalf("已批准的一次性升级应 allow/danger: %+v", approved)
	}

	// 同一份批准不能漂到另一个 cwd：绑定 canonical path。
	if _, err := g.EvaluateCommand(WithEscalation(ctx, approval), "true", filepath.Join(f.outside, "other"), PermissionDangerFullAccess); !errors.Is(err, ErrApprovalBinding) {
		t.Fatalf("批准绑定到别的路径必须报 ErrApprovalBinding: %v", err)
	}
	// 也不能漂到另一个会话。
	if _, err := g.EvaluateCommand(spi.WithSession(WithEscalation(ctx, approval), "session-B"), "true", f.outside, PermissionDangerFullAccess); !errors.Is(err, ErrApprovalBinding) && !errors.Is(err, ErrSessionMismatch) {
		t.Fatalf("批准绑定到别的会话必须失败: %v", err)
	}
	// 后续调用不带批准 = 重新 needs-approval，证明没有跨调用记忆。
	again, err := g.EvaluateCommand(ctx, "true", f.outside, PermissionDangerFullAccess)
	if err != nil || again.Action != ActionNeedsApproval {
		t.Fatalf("一次批准不得污染后续调用: %+v %v", again, err)
	}
	// 未批准的显式拒绝。
	denied, err := g.EvaluateCommand(WithEscalation(ctx, d.Approval.Decide(false)), "true", f.outside, PermissionDangerFullAccess)
	if err != nil {
		t.Fatal(err)
	}
	if denied.Action != ActionDeny || denied.Reason != ReasonEscalationDenied {
		t.Fatalf("明确拒绝的升级判定 = %+v", denied)
	}
}

func TestGateFromEnv(t *testing.T) {
	f := newPolicyFixture(t)
	t.Setenv(EnvMode, "")
	g, err := GateFromEnv(f.workspace)
	if err != nil || g != nil {
		t.Fatalf("未接线时 GateFromEnv 应返回 (nil,nil): %v %v", g, err)
	}
	t.Setenv(EnvMode, "workspace-write")
	t.Setenv(EnvWorkspaceRoot, f.workspace)
	t.Setenv(EnvHomeDir, f.home)
	t.Setenv(EnvExtraReadRoots, f.extra)
	g, err = GateFromEnv("/nonexistent-should-not-be-used")
	if err != nil {
		t.Fatal(err)
	}
	if g.Mode() != ModeWorkspaceWrite {
		t.Fatalf("Gate 档位 = %s", g.Mode())
	}
	t.Setenv(EnvMode, "yolo")
	if _, err := GateFromEnv(f.workspace); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("未知环境档位必须 fail-loud: %v", err)
	}
}

func TestDecisionError(t *testing.T) {
	if err := (Decision{Action: ActionAllow}).Error(); err != nil {
		t.Fatalf("allow 不应有错误: %v", err)
	}
	if err := (Decision{Action: ActionDeny, Reason: ReasonReadOnlyExecute}).Error(); !errors.Is(err, ErrDenied) {
		t.Fatalf("deny 应包装 ErrDenied: %v", err)
	}
	if err := (Decision{Action: ActionNeedsApproval}).Error(); !errors.Is(err, ErrNeedsApproval) {
		t.Fatalf("needs-approval 应包装 ErrNeedsApproval: %v", err)
	}
}
