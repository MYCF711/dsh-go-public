package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type policyFixture struct {
	workspace string
	extra     string
	home      string
	outside   string
}

func newPolicyFixture(t *testing.T) policyFixture {
	t.Helper()
	base := t.TempDir()
	f := policyFixture{
		workspace: filepath.Join(base, "workspace"),
		extra:     filepath.Join(base, "extra"),
		home:      filepath.Join(base, "home"),
		outside:   filepath.Join(base, "outside"),
	}
	for _, dir := range []string{f.workspace, f.extra, f.home, f.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("创建测试目录 %s: %v", dir, err)
		}
	}
	return f
}

func (f policyFixture) policy(t *testing.T, session string, mode Mode) *Policy {
	t.Helper()
	p, err := New(Config{
		SessionID:      session,
		Mode:           mode,
		WorkspaceRoot:  f.workspace,
		ExtraReadRoots: []string{f.extra},
		HomeDir:        f.home,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func testCall(session, id string) Call {
	return Call{SessionID: session, CallID: id}
}

func TestNewRejectsUnknownModeAndMissingContext(t *testing.T) {
	f := newPolicyFixture(t)
	valid := Config{
		SessionID:      "session-A",
		Mode:           ModeWorkspaceWrite,
		WorkspaceRoot:  f.workspace,
		ExtraReadRoots: []string{f.extra},
		HomeDir:        f.home,
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   error
	}{
		{name: "unknown mode", mutate: func(c *Config) { c.Mode = "default" }, want: ErrUnknownMode},
		{name: "empty mode", mutate: func(c *Config) { c.Mode = "" }, want: ErrUnknownMode},
		{name: "missing session", mutate: func(c *Config) { c.SessionID = "" }, want: ErrMissingContext},
		{name: "missing workspace", mutate: func(c *Config) { c.WorkspaceRoot = "" }, want: ErrMissingContext},
		{name: "missing home", mutate: func(c *Config) { c.HomeDir = "" }, want: ErrMissingContext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			if _, err := New(cfg); !errors.Is(err, tt.want) {
				t.Fatalf("New error = %v, want errors.Is(%v)", err, tt.want)
			}
		})
	}
}

func TestEvaluateRejectsInvalidContextAndPermission(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)

	var nilPolicy *Policy
	_, err := nilPolicy.EvaluateFS(FSRequest{
		Call: testCall("session-A", "call-nil"), Operation: OperationRead, Path: "file.txt",
	})
	if !errors.Is(err, ErrMissingContext) {
		t.Fatalf("nil policy error = %v", err)
	}

	base := FSRequest{Call: testCall("session-A", "call-1"), Operation: OperationWrite, Path: "file.txt"}
	tests := []struct {
		name   string
		mutate func(*FSRequest)
		want   error
	}{
		{name: "missing session", mutate: func(r *FSRequest) { r.SessionID = "" }, want: ErrMissingContext},
		{name: "missing call", mutate: func(r *FSRequest) { r.CallID = "" }, want: ErrMissingContext},
		{name: "wrong session", mutate: func(r *FSRequest) { r.SessionID = "session-B" }, want: ErrSessionMismatch},
		{name: "unknown permission", mutate: func(r *FSRequest) { r.SandboxPermissions = "default" }, want: ErrUnknownPermission},
		{name: "invalid operation", mutate: func(r *FSRequest) { r.Operation = OperationExecute }, want: ErrInvalidOperation},
		{name: "grant without request", mutate: func(r *FSRequest) {
			r.Escalation = (&ApprovalRequest{
				Kind: ApprovalKindSandboxUpgrade, OneShot: true, SessionID: "session-A", CallID: "call-1",
				Permission: PermissionDangerFullAccess, Operation: OperationWrite, Path: "file.txt",
			}).Decide(true)
		}, want: ErrApprovalBinding},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			if _, err := p.EvaluateFS(req); !errors.Is(err, tt.want) {
				t.Fatalf("EvaluateFS error = %v, want errors.Is(%v)", err, tt.want)
			}
		})
	}
}

func TestReadOnlyDeniesWritesExecutionAndEscalation(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeReadOnly)

	read, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "read"), Operation: OperationRead, Path: "file.txt",
	})
	if err != nil || read.Action != ActionAllow {
		t.Fatalf("read = %+v, err=%v", read, err)
	}

	write, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "write"), Operation: OperationWrite, Path: "file.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, write, ActionDeny, ReasonReadOnlyWrite, ModeReadOnly)
	if write.EscalationEligible || write.Approval != nil {
		t.Fatalf("read-only write unexpectedly offers escalation: %+v", write)
	}

	for _, permission := range []Permission{PermissionWorkspaceWrite, PermissionDangerFullAccess} {
		t.Run("write upgrade "+string(permission), func(t *testing.T) {
			d, err := p.EvaluateFS(FSRequest{
				Call:      Call{SessionID: "session-A", CallID: "write-" + string(permission), SandboxPermissions: permission},
				Operation: OperationWrite, Path: "file.txt",
			})
			if err != nil {
				t.Fatal(err)
			}
			assertDecision(t, d, ActionDeny, ReasonReadOnlyEscalation, ModeReadOnly)
			if d.Approval != nil || d.EscalationEligible {
				t.Fatalf("read-only upgrade must be denied without approval: %+v", d)
			}
		})
		t.Run("execute upgrade "+string(permission), func(t *testing.T) {
			d, err := p.EvaluateCommand(CommandRequest{
				Call:    Call{SessionID: "session-A", CallID: "exec-" + string(permission), SandboxPermissions: permission},
				Command: "pwd", Cwd: f.workspace,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertDecision(t, d, ActionDeny, ReasonReadOnlyEscalation, ModeReadOnly)
		})
	}

	execute, err := p.EvaluateCommand(CommandRequest{
		Call: testCall("session-A", "execute"), Command: "pwd", Cwd: f.workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, execute, ActionDeny, ReasonReadOnlyExecute, ModeReadOnly)

	outsideRead, err := p.EvaluateFS(FSRequest{
		Call:      Call{SessionID: "session-A", CallID: "outside-read", SandboxPermissions: PermissionDangerFullAccess},
		Operation: OperationRead, Path: filepath.Join(f.outside, "secret.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, outsideRead, ActionDeny, ReasonReadOnlyEscalation, ModeReadOnly)
}

func TestDangerFullAccessAllowsWithoutReplacingToolApproval(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeDangerFullAccess)
	outside := filepath.Join(f.outside, "new", "file.txt")

	write, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "write"), Operation: OperationWrite, Path: outside,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, write, ActionAllow, ReasonDangerFullAccess, ModeDangerFullAccess)
	if !write.ExistingApprovalPreserved || write.Approval != nil {
		t.Fatalf("danger mode must not consume or replace existing tool approval: %+v", write)
	}
	if write.Enforcement != EnforcementPolicyOnlyNoOSIsolation {
		t.Fatalf("enforcement = %q", write.Enforcement)
	}

	execute, err := p.EvaluateCommand(CommandRequest{
		Call:    Call{SessionID: "session-A", CallID: "exec", SandboxPermissions: PermissionWorkspaceWrite},
		Command: "pwd", Cwd: f.outside,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, execute, ActionAllow, ReasonDangerFullAccess, ModeDangerFullAccess)
	if !execute.ExistingApprovalPreserved || execute.Approval != nil {
		t.Fatalf("danger command approval semantics = %+v", execute)
	}
}

func TestWorkspaceOneShotEscalationIsBoundToSingleCall(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	outside := filepath.Join(f.outside, "new", "file.txt")
	req := FSRequest{
		Call:      Call{SessionID: "session-A", CallID: "call-1", SandboxPermissions: PermissionDangerFullAccess},
		Operation: OperationWrite,
		Path:      outside,
	}

	plain, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "plain"), Operation: OperationWrite, Path: outside,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, plain, ActionDeny, ReasonOutsideWorkspace, ModeWorkspaceWrite)
	if !plain.EscalationEligible {
		t.Fatalf("outside write should be eligible for explicit escalation: %+v", plain)
	}

	pending, err := p.EvaluateFS(req)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, pending, ActionNeedsApproval, ReasonEscalationRequired, ModeWorkspaceWrite)
	if pending.EffectiveMode != ModeWorkspaceWrite || pending.Approval == nil {
		t.Fatalf("pending escalation = %+v", pending)
	}
	approval := pending.Approval
	if approval.Kind != ApprovalKindSandboxUpgrade || !approval.OneShot ||
		approval.SessionID != "session-A" || approval.CallID != "call-1" ||
		approval.Permission != PermissionDangerFullAccess || approval.Operation != OperationWrite ||
		approval.Path != pending.CanonicalPath || approval.Reason != ReasonEscalationRequired {
		t.Fatalf("structured approval mismatch: %+v", approval)
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"action":"needs-approval"`, `"reason":"sandbox-upgrade-required"`,
		`"mode":"workspace-write"`, `"effective_mode":"workspace-write"`,
		`"kind":"sandbox-upgrade"`, `"one_shot":true`, `"session_id":"session-A"`, `"call_id":"call-1"`,
	} {
		if !strings.Contains(string(encoded), fragment) {
			t.Errorf("decision JSON %s does not contain %s", encoded, fragment)
		}
	}

	runs := 0
	runIfAllowed := func(d Decision) {
		if d.Action == ActionAllow {
			runs++
		}
	}
	runIfAllowed(pending)
	if runs != 0 {
		t.Fatal("unapproved escalation executed")
	}

	deniedReq := req
	deniedReq.Escalation = approval.Decide(false)
	denied, err := p.EvaluateFS(deniedReq)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, denied, ActionDeny, ReasonEscalationDenied, ModeWorkspaceWrite)
	runIfAllowed(denied)
	if runs != 0 {
		t.Fatal("denied escalation executed")
	}

	approvedReq := req
	approvedReq.Escalation = approval.Decide(true)
	approved, err := p.EvaluateFS(approvedReq)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, approved, ActionAllow, ReasonDangerFullAccess, ModeWorkspaceWrite)
	if approved.EffectiveMode != ModeDangerFullAccess || approved.Approval != nil || !approved.ExistingApprovalPreserved {
		t.Fatalf("approved one-shot = %+v", approved)
	}
	runIfAllowed(approved)
	if runs != 1 {
		t.Fatalf("approved call runs = %d, want 1", runs)
	}

	next := req
	next.CallID = "call-2"
	nextDecision, err := p.EvaluateFS(next)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, nextDecision, ActionNeedsApproval, ReasonEscalationRequired, ModeWorkspaceWrite)
	if nextDecision.Approval.CallID != "call-2" {
		t.Fatalf("next call inherited approval: %+v", nextDecision)
	}

	after, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "after"), Operation: OperationWrite, Path: outside,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, after, ActionDeny, ReasonOutsideWorkspace, ModeWorkspaceWrite)
	runIfAllowed(nextDecision)
	runIfAllowed(after)
	if runs != 1 {
		t.Fatalf("one-shot polluted later calls: runs=%d", runs)
	}
}

func TestEscalationApprovalCannotCrossSessionCallPathOrOperation(t *testing.T) {
	f := newPolicyFixture(t)
	a := f.policy(t, "session-A", ModeWorkspaceWrite)
	b := f.policy(t, "session-B", ModeWorkspaceWrite)
	outside := filepath.Join(f.outside, "file.txt")
	base := FSRequest{
		Call:      Call{SessionID: "session-A", CallID: "call-1", SandboxPermissions: PermissionDangerFullAccess},
		Operation: OperationWrite, Path: outside,
	}
	pending, err := a.EvaluateFS(base)
	if err != nil {
		t.Fatal(err)
	}
	grant := pending.Approval.Decide(true)

	tests := []struct {
		name string
		p    *Policy
		req  FSRequest
	}{
		{name: "other session", p: b, req: func() FSRequest {
			r := base
			r.SessionID = "session-B"
			return r
		}()},
		{name: "other call", p: a, req: func() FSRequest {
			r := base
			r.CallID = "call-2"
			return r
		}()},
		{name: "other path", p: a, req: func() FSRequest {
			r := base
			r.Path = filepath.Join(f.outside, "other.txt")
			return r
		}()},
		{name: "other operation", p: a, req: func() FSRequest {
			r := base
			r.Operation = OperationRead
			return r
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			req.Escalation = grant
			if _, err := tt.p.EvaluateFS(req); !errors.Is(err, ErrApprovalBinding) {
				t.Fatalf("error = %v, want ErrApprovalBinding", err)
			}
		})
	}
}

func TestConcurrentCallsDoNotShareOneShotState(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	const count = 32
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("call-%d", i)
			req := FSRequest{
				Call:      Call{SessionID: "session-A", CallID: id, SandboxPermissions: PermissionDangerFullAccess},
				Operation: OperationWrite, Path: filepath.Join(f.outside, id+".txt"),
			}
			pending, err := p.EvaluateFS(req)
			if err != nil {
				errs <- err
				return
			}
			if pending.Action != ActionNeedsApproval || pending.Approval == nil || pending.Approval.CallID != id {
				errs <- fmt.Errorf("%s pending = %+v", id, pending)
				return
			}
			req.Escalation = pending.Approval.Decide(true)
			allowed, err := p.EvaluateFS(req)
			if err != nil {
				errs <- err
				return
			}
			if allowed.Action != ActionAllow || allowed.EffectiveMode != ModeDangerFullAccess {
				errs <- fmt.Errorf("%s allowed = %+v", id, allowed)
				return
			}
			later, err := p.EvaluateFS(FSRequest{
				Call: testCall("session-A", id+"-later"), Operation: OperationWrite, Path: req.Path,
			})
			if err != nil {
				errs <- err
				return
			}
			if later.Action != ActionDeny {
				errs <- fmt.Errorf("%s later = %+v", id, later)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func assertDecision(t *testing.T, got Decision, action Action, reason Reason, mode Mode) {
	t.Helper()
	if got.Action != action || got.Reason != reason || got.Mode != mode {
		t.Fatalf("decision = %+v, want action=%q reason=%q mode=%q", got, action, reason, mode)
	}
	if got.Enforcement != EnforcementPolicyOnlyNoOSIsolation {
		t.Fatalf("decision enforcement = %q, want explicit non-OS policy", got.Enforcement)
	}
}
