package sandbox

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkspaceCommandPolicy(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)

	inside, err := p.EvaluateCommand(CommandRequest{
		Call: testCall("session-A", "inside"), Command: "pwd", Cwd: f.workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, inside, ActionAllow, ReasonWorkspaceCommand, ModeWorkspaceWrite)
	if !inside.ExistingApprovalPreserved || inside.Approval != nil {
		t.Fatalf("workspace command must leave existing tool approval in place: %+v", inside)
	}

	sameMode, err := p.EvaluateCommand(CommandRequest{
		Call:    Call{SessionID: "session-A", CallID: "same-mode", SandboxPermissions: PermissionWorkspaceWrite},
		Command: "pwd", Cwd: f.workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, sameMode, ActionAllow, ReasonWorkspaceCommand, ModeWorkspaceWrite)
	if sameMode.Approval != nil {
		t.Fatalf("workspace-write is not an upgrade: %+v", sameMode)
	}

	outside, err := p.EvaluateCommand(CommandRequest{
		Call: testCall("session-A", "outside"), Command: "pwd", Cwd: f.outside,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, outside, ActionDeny, ReasonCommandOutsideWorkspace, ModeWorkspaceWrite)
	if !outside.EscalationEligible {
		t.Fatalf("outside command should be eligible for one-shot escalation: %+v", outside)
	}
}

func TestCommandEscalationRequiresApprovalBeforeExecution(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	req := CommandRequest{
		Call:    Call{SessionID: "session-A", CallID: "command-1", SandboxPermissions: PermissionDangerFullAccess},
		Command: "pwd", Cwd: f.outside,
	}
	runs := 0
	executeIfAllowed := func(d Decision) {
		if d.Action == ActionAllow {
			runs++
		}
	}

	pending, err := p.EvaluateCommand(req)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, pending, ActionNeedsApproval, ReasonEscalationRequired, ModeWorkspaceWrite)
	executeIfAllowed(pending)
	if runs != 0 {
		t.Fatal("shell started before escalation approval")
	}
	if pending.Approval == nil || pending.Approval.Operation != OperationExecute || pending.Approval.CallID != "command-1" {
		t.Fatalf("command approval = %+v", pending.Approval)
	}

	deniedReq := req
	deniedReq.Escalation = pending.Approval.Decide(false)
	denied, err := p.EvaluateCommand(deniedReq)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, denied, ActionDeny, ReasonEscalationDenied, ModeWorkspaceWrite)
	executeIfAllowed(denied)
	if runs != 0 {
		t.Fatal("shell started after denied escalation")
	}

	approvedReq := req
	approvedReq.Escalation = pending.Approval.Decide(true)
	approved, err := p.EvaluateCommand(approvedReq)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, approved, ActionAllow, ReasonDangerFullAccess, ModeWorkspaceWrite)
	if approved.EffectiveMode != ModeDangerFullAccess || !approved.ExistingApprovalPreserved {
		t.Fatalf("approved command = %+v", approved)
	}
	executeIfAllowed(approved)
	if runs != 1 {
		t.Fatalf("approved command runs = %d", runs)
	}

	next := req
	next.CallID = "command-2"
	nextDecision, err := p.EvaluateCommand(next)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, nextDecision, ActionNeedsApproval, ReasonEscalationRequired, ModeWorkspaceWrite)
	executeIfAllowed(nextDecision)
	if runs != 1 {
		t.Fatal("later command inherited one-shot approval")
	}
}

func TestDangerCommandAllowsButDoesNotClaimOSIsolation(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeDangerFullAccess)
	d, err := p.EvaluateCommand(CommandRequest{
		Call: testCall("session-A", "danger"), Command: "pwd", Cwd: f.outside,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, d, ActionAllow, ReasonDangerFullAccess, ModeDangerFullAccess)
	if !d.ExistingApprovalPreserved {
		t.Fatalf("existing command approval was erased: %+v", d)
	}
	if d.Enforcement != EnforcementPolicyOnlyNoOSIsolation {
		t.Fatalf("danger command claims unsupported isolation: %+v", d)
	}
}

func TestCommandRejectsMissingInputAndUnknownPermission(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	base := CommandRequest{Call: testCall("session-A", "command"), Command: "pwd", Cwd: f.workspace}
	for _, tt := range []struct {
		name   string
		mutate func(*CommandRequest)
		want   error
	}{
		{name: "missing command", mutate: func(r *CommandRequest) { r.Command = "" }, want: ErrMissingContext},
		{name: "missing cwd", mutate: func(r *CommandRequest) { r.Cwd = "" }, want: ErrMissingContext},
		{name: "unknown permission", mutate: func(r *CommandRequest) { r.SandboxPermissions = "read-only" }, want: ErrUnknownPermission},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			if _, err := p.EvaluateCommand(req); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tt.want)
			}
		})
	}
}

func TestCommandApprovalIsBoundToCanonicalCwd(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	req := CommandRequest{
		Call:    Call{SessionID: "session-A", CallID: "command", SandboxPermissions: PermissionDangerFullAccess},
		Command: "pwd", Cwd: f.outside,
	}
	pending, err := p.EvaluateCommand(req)
	if err != nil {
		t.Fatal(err)
	}
	req.Cwd = filepath.Join(f.outside, "other")
	req.Escalation = pending.Approval.Decide(true)
	if _, err := p.EvaluateCommand(req); !errors.Is(err, ErrApprovalBinding) {
		t.Fatalf("error = %v, want ErrApprovalBinding", err)
	}
}
