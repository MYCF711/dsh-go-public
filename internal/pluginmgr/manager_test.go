package pluginmgr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestPackage(t *testing.T, dir, id, version string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("private test package " + id + " " + version + " — not executable")
	sum := sha256.Sum256(payload)
	manifest := map[string]any{
		"schema": "dsh.local-plugin@1", "id": id, "version": version,
		"kind": "wasm", "entry": "plugin.wasm",
		"files": map[string]string{"plugin.wasm": hex.EncodeToString(sum[:])},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func onlyPlugin(t *testing.T, m *Manager) PluginInfo {
	t.Helper()
	plugins, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 {
		t.Fatalf("want one plugin, got %+v", plugins)
	}
	return plugins[0]
}

func TestLifecyclePersistsAndRestores(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "state", "host-plugins")
	v1 := writeTestPackage(t, filepath.Join(home, "private", "v1"), "acme.audit", "1.0.0")
	v2 := writeTestPackage(t, filepath.Join(home, "private", "v2"), "acme.audit", "2.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.List(); err != nil || len(got) != 0 {
		t.Fatalf("initial list = %+v, %v", got, err)
	}
	p, err := m.Install(v1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled || p.Version != "1.0.0" || p.Source != v1 || p.Kind != "wasm" || p.Entry != "plugin.wasm" {
		t.Fatalf("bad install projection: %+v", p)
	}
	m, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if p = onlyPlugin(t, m); p.Enabled || p.Version != "1.0.0" {
		t.Fatalf("install did not survive restart: %+v", p)
	}
	if _, err = m.Enable("acme.audit"); err != nil {
		t.Fatal(err)
	}
	m, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if p = onlyPlugin(t, m); !p.Enabled {
		t.Fatalf("enable did not survive restart: %+v", p)
	}
	p, err = m.Update("acme.audit", v2)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "2.0.0" || p.PreviousVersion != "1.0.0" || !p.Enabled || p.Source != v2 {
		t.Fatalf("bad update: %+v", p)
	}
	if _, err = m.Disable("acme.audit"); err != nil {
		t.Fatal(err)
	}
	p, err = m.Restore("acme.audit", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "1.0.0" || p.PreviousVersion != "2.0.0" || p.Enabled || p.Source != v1 {
		t.Fatalf("bad restore: %+v", p)
	}
	m, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if p = onlyPlugin(t, m); p.Version != "1.0.0" || p.Enabled {
		t.Fatalf("restore/disable not persistent: %+v", p)
	}
	if err = m.Uninstall("acme.audit"); err != nil {
		t.Fatal(err)
	}
	m, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.List(); err != nil || len(got) != 0 {
		t.Fatalf("uninstall not persistent: %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "packages", "acme.audit")); !os.IsNotExist(err) {
		t.Fatalf("package directory remains after uninstall: %v", err)
	}
}

func TestFailedUpdateKeepsOldVersion(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	v1 := writeTestPackage(t, filepath.Join(home, "v1"), "acme.audit", "1.0.0")
	v2 := writeTestPackage(t, filepath.Join(home, "v2"), "acme.audit", "2.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(v1); err != nil {
		t.Fatal(err)
	}
	// Corrupt the declared payload after constructing an otherwise valid v2 manifest.
	if err = os.WriteFile(filepath.Join(v2, "plugin.wasm"), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Update("acme.audit", v2); err == nil {
		t.Fatal("hash mismatch update unexpectedly succeeded")
	}
	if p := onlyPlugin(t, m); p.Version != "1.0.0" || p.Source != v1 {
		t.Fatalf("failed update switched current version: %+v", p)
	}
	m, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if p := onlyPlugin(t, m); p.Version != "1.0.0" {
		t.Fatalf("failed update was not atomic after restart: %+v", p)
	}
}

func TestUpdateCommitFailureRollsBackStagedPackage(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	v1 := writeTestPackage(t, filepath.Join(home, "v1"), "acme.audit", "1.0.0")
	v2 := writeTestPackage(t, filepath.Join(home, "v2"), "acme.audit", "2.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(v1); err != nil {
		t.Fatal(err)
	}
	m.afterPhase = func(phase string) error {
		if phase == "committing" {
			return fmt.Errorf("injected state write failure")
		}
		return nil
	}
	if _, err = m.Update("acme.audit", v2); err == nil {
		t.Fatal("injected commit failure unexpectedly succeeded")
	}
	m.afterPhase = nil
	if p := onlyPlugin(t, m); p.Version != "1.0.0" || p.Source != v1 {
		t.Fatalf("commit failure switched current version: %+v", p)
	}
	if _, err = os.Stat(m.packagePath("acme.audit", "2.0.0")); !os.IsNotExist(err) {
		t.Fatalf("uncommitted staged version remains: %v", err)
	}
	m, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if p := onlyPlugin(t, m); p.Version != "1.0.0" {
		t.Fatalf("commit failure survived restart as current: %+v", p)
	}
}

func TestRejectedPackagesFailLoud(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		version string
		mutate  func(t *testing.T, dir string)
	}{
		{name: "path escape", id: "acme.escape", version: "1.0.0", mutate: func(t *testing.T, dir string) {
			rewriteManifest(t, dir, func(m map[string]any) { m["entry"] = "../outside" })
		}},
		{name: "backslash path", id: "acme.escape", version: "1.0.0", mutate: func(t *testing.T, dir string) {
			rewriteManifest(t, dir, func(m map[string]any) { m["entry"] = `..\outside` })
		}},
		{name: "illegal version", id: "acme.version", version: "01.0.0"},
		{name: "missing manifest", id: "acme.bad", version: "1.0.0", mutate: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "plugin.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unlisted payload", id: "acme.bad", version: "1.0.0", mutate: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("unhashed"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing payload", id: "acme.bad", version: "1.0.0", mutate: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "plugin.wasm")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink payload", id: "acme.bad", version: "1.0.0", mutate: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "plugin.wasm")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "plugin.json"), filepath.Join(dir, "plugin.wasm")); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			pkg := writeTestPackage(t, filepath.Join(home, "pkg"), tc.id, tc.version)
			if tc.mutate != nil {
				tc.mutate(t, pkg)
			}
			m, err := Open(filepath.Join(home, "manager"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.Install(pkg); err == nil {
				t.Fatal("bad package unexpectedly installed")
			}
			if got, listErr := m.List(); listErr != nil || len(got) != 0 {
				t.Fatalf("bad install changed state: %+v, %v", got, listErr)
			}
		})
	}

	t.Run("duplicate manifest key", func(t *testing.T) {
		home := t.TempDir()
		pkg := writeTestPackage(t, filepath.Join(home, "pkg"), "acme.bad", "1.0.0")
		raw, err := os.ReadFile(filepath.Join(pkg, "plugin.json"))
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw[:1], append([]byte(`"id":"acme.other",`), raw[1:]...)...)
		if err = os.WriteFile(filepath.Join(pkg, "plugin.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := Open(filepath.Join(home, "manager"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = m.Install(pkg); err == nil || !strings.Contains(err.Error(), "重复") {
			t.Fatalf("duplicate JSON key was not loud: %v", err)
		}
	})
}

func rewriteManifest(t *testing.T, dir string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, "plugin.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateIDAndVersionAreRejected(t *testing.T) {
	home := t.TempDir()
	m, err := Open(filepath.Join(home, "manager"))
	if err != nil {
		t.Fatal(err)
	}
	v1 := writeTestPackage(t, filepath.Join(home, "one"), "acme.same", "1.0.0")
	duplicate := writeTestPackage(t, filepath.Join(home, "two"), "acme.same", "1.1.0")
	if _, err = m.Install(v1); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(duplicate); err == nil {
		t.Fatal("duplicate id unexpectedly installed")
	} else {
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Code != "duplicate_id" {
			t.Fatalf("duplicate id error = %#v", err)
		}
	}
	if _, err = m.Update("acme.same", v1); err == nil {
		t.Fatal("non-increasing update unexpectedly succeeded")
	}
}

func TestABHomeIsolation(t *testing.T) {
	base := t.TempDir()
	homeA := filepath.Join(base, "home-a")
	homeB := filepath.Join(base, "home-b")
	pkgA := writeTestPackage(t, filepath.Join(base, "private-a"), "acme.shared", "1.0.0")
	pkgB := writeTestPackage(t, filepath.Join(base, "private-b"), "acme.shared", "3.0.0")
	a, err := Open(filepath.Join(homeA, "state", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(filepath.Join(homeB, "state", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Install(pkgA); err != nil {
		t.Fatal(err)
	}
	if got, err := b.List(); err != nil || len(got) != 0 {
		t.Fatalf("home B observed home A install: %+v, %v", got, err)
	}
	if _, err = b.Install(pkgB); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Enable("acme.shared"); err != nil {
		t.Fatal(err)
	}
	pa := onlyPlugin(t, a)
	pb := onlyPlugin(t, b)
	if pa.Version != "1.0.0" || !pa.Enabled || pa.Source != pkgA || pb.Version != "3.0.0" || pb.Enabled || pb.Source != pkgB {
		t.Fatalf("A/B state leaked: A=%+v B=%+v", pa, pb)
	}
}

func TestReconcileAfterRealProcessCrash(t *testing.T) {
	for _, tc := range []struct {
		name, operation, phase, wantVersion string
		wantInstalled                       bool
	}{
		{name: "update before commit rolls back", operation: "update", phase: "installed", wantVersion: "1.0.0", wantInstalled: true},
		{name: "update after commit stays new", operation: "update", phase: "state_committed", wantVersion: "2.0.0", wantInstalled: true},
		{name: "uninstall before commit restores package", operation: "uninstall", phase: "package_removed", wantVersion: "1.0.0", wantInstalled: true},
		{name: "uninstall after commit finishes removal", operation: "uninstall", phase: "state_committed", wantInstalled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, "manager")
			v1 := writeTestPackage(t, filepath.Join(home, "v1"), "acme.crash", "1.0.0")
			v2 := writeTestPackage(t, filepath.Join(home, "v2"), "acme.crash", "2.0.0")
			m, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.Install(v1); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestPluginManagerCrashHelper$")
			cmd.Env = append(os.Environ(),
				"GO_WANT_PLUGINMGR_CRASH=1", "PLUGINMGR_ROOT="+root,
				"PLUGINMGR_OPERATION="+tc.operation, "PLUGINMGR_PHASE="+tc.phase,
				"PLUGINMGR_ID=acme.crash", "PLUGINMGR_SOURCE="+v2)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "crash-phase="+tc.phase) {
				t.Fatalf("helper did not crash at requested phase: %v\n%s", err, output)
			}
			m, err = Open(root)
			if err != nil {
				t.Fatalf("restart reconcile: %v", err)
			}
			plugins, err := m.List()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantInstalled {
				if len(plugins) != 0 {
					t.Fatalf("uncommitted uninstall left state: %+v", plugins)
				}
				return
			}
			if len(plugins) != 1 || plugins[0].Version != tc.wantVersion {
				t.Fatalf("reconcile version = %+v, want %s", plugins, tc.wantVersion)
			}
			pkgDir := filepath.Join(root, "packages", "acme.crash", plugins[0].Version, "plugin.wasm")
			if _, err = os.Stat(pkgDir); err != nil {
				t.Fatalf("current package was silently lost: %v", err)
			}
			actions, err := m.Reconcile()
			if err != nil {
				t.Fatal(err)
			}
			if actions.Recovered != 0 {
				t.Fatalf("reconcile was not idempotent: %+v", actions)
			}
		})
	}
}

func TestPluginManagerCrashHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PLUGINMGR_CRASH") != "1" {
		return
	}
	m, err := Open(os.Getenv("PLUGINMGR_ROOT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(90)
	}
	phase := os.Getenv("PLUGINMGR_PHASE")
	m.afterPhase = func(got string) error {
		if got == phase {
			fmt.Fprintln(os.Stderr, "crash-phase="+got)
			os.Exit(86)
		}
		return nil
	}
	switch os.Getenv("PLUGINMGR_OPERATION") {
	case "update":
		_, err = m.Update(os.Getenv("PLUGINMGR_ID"), os.Getenv("PLUGINMGR_SOURCE"))
	case "uninstall":
		err = m.Uninstall(os.Getenv("PLUGINMGR_ID"))
	default:
		err = fmt.Errorf("unknown helper operation")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(91)
	}
	os.Exit(0)
}
