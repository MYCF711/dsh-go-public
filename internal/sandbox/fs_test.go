package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWorkspaceFSDefaultBoundaries(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)

	writeInside := filepath.Join(f.workspace, "new", "deep", "file.txt")
	d, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "workspace-write"), Operation: OperationWrite, Path: writeInside,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, d, ActionAllow, ReasonWorkspaceWrite, ModeWorkspaceWrite)
	if d.EffectiveMode != ModeWorkspaceWrite || !d.ExistingApprovalPreserved {
		t.Fatalf("workspace write = %+v", d)
	}

	for _, tt := range []struct {
		name   string
		path   string
		reason Reason
	}{
		{name: "extra is read only", path: filepath.Join(f.extra, "file.txt"), reason: ReasonExtraReadOnly},
		{name: "home is read only", path: "~/file.txt", reason: ReasonHomeReadOnly},
		{name: "outside", path: filepath.Join(f.outside, "file.txt"), reason: ReasonOutsideWorkspace},
		{name: "prefix sibling", path: f.workspace + "-evil/file.txt", reason: ReasonOutsideWorkspace},
		{name: "relative escape", path: "../outside/file.txt", reason: ReasonOutsideWorkspace},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, err := p.EvaluateFS(FSRequest{
				Call: testCall("session-A", "write-"+tt.name), Operation: OperationWrite, Path: tt.path,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertDecision(t, d, ActionDeny, tt.reason, ModeWorkspaceWrite)
			if !d.EscalationEligible {
				t.Fatalf("workspace write denial should offer explicit danger escalation: %+v", d)
			}
		})
	}

	for _, tt := range []struct {
		name   string
		path   string
		reason Reason
	}{
		{name: "workspace", path: "file.txt", reason: ReasonWorkspaceRead},
		{name: "extra", path: filepath.Join(f.extra, "file.txt"), reason: ReasonExtraRead},
		{name: "home tilde", path: "~/file.txt", reason: ReasonHomeRead},
	} {
		t.Run("read "+tt.name, func(t *testing.T) {
			d, err := p.EvaluateFS(FSRequest{
				Call: testCall("session-A", "read-"+tt.name), Operation: OperationRead, Path: tt.path,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertDecision(t, d, ActionAllow, tt.reason, ModeWorkspaceWrite)
			if d.ExistingApprovalPreserved {
				t.Fatalf("read must not invent a write/tool approval: %+v", d)
			}
		})
	}

	outsideRead, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "outside-read"), Operation: OperationRead, Path: filepath.Join(f.outside, "file.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, outsideRead, ActionDeny, ReasonOutsideRead, ModeWorkspaceWrite)
	if !outsideRead.EscalationEligible {
		t.Fatalf("outside read should require explicit danger escalation: %+v", outsideRead)
	}

	// 与 modules/fs/local 一致：独立 home 只能经显式 ~ 打开，不能用普通绝对路径绕入。
	absoluteHome, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "absolute-home-read"), Operation: OperationRead, Path: filepath.Join(f.home, "file.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, absoluteHome, ActionDeny, ReasonOutsideRead, ModeWorkspaceWrite)
}

func TestWorkspacePermissionIsNotAnEscalation(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	d, err := p.EvaluateFS(FSRequest{
		Call:      Call{SessionID: "session-A", CallID: "same-mode", SandboxPermissions: PermissionWorkspaceWrite},
		Operation: OperationWrite, Path: "file.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, d, ActionAllow, ReasonWorkspaceWrite, ModeWorkspaceWrite)
	if d.EffectiveMode != ModeWorkspaceWrite || d.Approval != nil {
		t.Fatalf("workspace-write must not request escalation: %+v", d)
	}
}

func TestExplicitHomeCannotEscapeThroughDotDot(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	_, err := p.EvaluateFS(FSRequest{
		Call: testCall("session-A", "home-escape"), Operation: OperationRead, Path: "~/../outside/file.txt",
	})
	if !errors.Is(err, ErrPathOutsideHome) {
		t.Fatalf("error = %v, want ErrPathOutsideHome", err)
	}
}

func TestCanonicalBoundaryFollowsExistingAndFutureSymlinkPaths(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	link := filepath.Join(f.workspace, "link")
	makeDirectoryLink(t, link, f.outside)

	for _, name := range []string{"existing", "future"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(link, "file.txt")
			if name == "future" {
				path = filepath.Join(link, "not-created", "deep", "file.txt")
			}
			d, err := p.EvaluateFS(FSRequest{
				Call: testCall("session-A", name), Operation: OperationWrite, Path: path,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertDecision(t, d, ActionDeny, ReasonOutsideWorkspace, ModeWorkspaceWrite)
			rel, err := filepath.Rel(f.outside, d.CanonicalPath)
			if err != nil || rel == ".." || filepath.IsAbs(rel) {
				t.Fatalf("canonical path %q did not resolve through link to %q (rel=%q err=%v)", d.CanonicalPath, f.outside, rel, err)
			}
		})
	}
}

func TestDangerEscalationCanCrossCanonicalLinkOnlyForApprovedCall(t *testing.T) {
	f := newPolicyFixture(t)
	p := f.policy(t, "session-A", ModeWorkspaceWrite)
	link := filepath.Join(f.workspace, "link")
	makeDirectoryLink(t, link, f.outside)
	req := FSRequest{
		Call:      Call{SessionID: "session-A", CallID: "link-write", SandboxPermissions: PermissionDangerFullAccess},
		Operation: OperationWrite, Path: filepath.Join(link, "new", "file.txt"),
	}
	pending, err := p.EvaluateFS(req)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Action != ActionNeedsApproval {
		t.Fatalf("pending = %+v", pending)
	}
	req.Escalation = pending.Approval.Decide(true)
	allowed, err := p.EvaluateFS(req)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, allowed, ActionAllow, ReasonDangerFullAccess, ModeWorkspaceWrite)
	if allowed.EffectiveMode != ModeDangerFullAccess {
		t.Fatalf("link escalation = %+v", allowed)
	}
}

func TestNewRejectsNonDirectoryRoots(t *testing.T) {
	f := newPolicyFixture(t)
	file := filepath.Join(f.workspace, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{
		SessionID: "session-A", Mode: ModeWorkspaceWrite,
		WorkspaceRoot: file, HomeDir: f.home,
	})
	if !errors.Is(err, ErrInvalidRoot) {
		t.Fatalf("error = %v, want ErrInvalidRoot", err)
	}
}

func makeDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("创建目录符号链接: %v", err)
		}
		return
	}
	// Windows 目录联接不需要开发者模式或提权；os.Symlink 失败时用它覆盖 reparse point。
	if err := os.Symlink(target, link); err == nil {
		return
	}
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("创建 Windows directory junction 失败: %v (%s)", err, output)
	}
}
