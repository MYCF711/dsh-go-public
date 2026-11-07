package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type pluginGlueCLIResult struct {
	exit   int
	stdout string
	stderr string
}

// TestPluginGlueRealDSHD builds the product main package, not a test helper.
// Every CLI action and every HTTP lifecycle step below crosses a real process or
// socket boundary and uses only temporary homes.
func TestPluginGlueRealDSHD(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	t.Run("CLI full lifecycle and explicit root", func(t *testing.T) {
		testPluginGlueCLI(t, exe)
	})
	t.Run("authenticated serve lifecycle startup reconcile and AB homes", func(t *testing.T) {
		testPluginGlueServe(t, exe)
	})
}

func buildPluginGlueDSHD(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "dshd.exe")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build real dshd: %v\n%s", err, output)
	}
	return exe
}

func pluginGlueEnvironment(decoyHome string) []string {
	return append(os.Environ(),
		"HOME="+decoyHome,
		"USERPROFILE="+decoyHome,
		"DSHD_HOME="+decoyHome,
		"DSHD_TIMING=",
	)
}

func runPluginGlueCLI(t *testing.T, exe, decoyHome string, args ...string) pluginGlueCLIResult {
	t.Helper()
	argv := append([]string{"plugins"}, args...)
	cmd := exec.Command(exe, argv...)
	cmd.Env = pluginGlueEnvironment(decoyHome)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exit = exitErr.ExitCode()
		} else {
			t.Fatalf("run real dshd %v: %v", argv, err)
		}
	}
	return pluginGlueCLIResult{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}

func callPluginGlueCLI(t *testing.T, exe, decoyHome string, wantExit int, args ...string) map[string]any {
	t.Helper()
	got := runPluginGlueCLI(t, exe, decoyHome, args...)
	if got.exit != wantExit {
		t.Fatalf("dshd plugins %v exit=%d want=%d stdout=%s stderr=%s", args, got.exit, wantExit, got.stdout, got.stderr)
	}
	raw := got.stdout
	if wantExit != 0 {
		raw = got.stderr
		if got.stdout != "" {
			t.Fatalf("failed CLI wrote stdout: %q", got.stdout)
		}
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("dshd plugins %v non-JSON: %v stdout=%q stderr=%q", args, err, got.stdout, got.stderr)
	}
	if wantExit != 0 {
		assertPluginGlueError(t, envelope, "")
	} else if len(envelope) != 1 || envelope["result"] == nil {
		t.Fatalf("success envelope is not exactly result: %+v", envelope)
	}
	return envelope
}

func testPluginGlueCLI(t *testing.T, exe string) {
	home := t.TempDir()
	root := filepath.Join(home, "plugins")
	decoyHome := filepath.Join(t.TempDir(), "must-not-be-read")
	packages := t.TempDir()
	v1 := writePluginTransportPackage(t, filepath.Join(packages, "v1"), "1.0.0")
	v2 := writePluginTransportPackage(t, filepath.Join(packages, "v2"), "2.0.0")

	help := runPluginGlueCLI(t, exe, decoyHome, "--help")
	if help.exit != 0 || help.stderr != "" || !strings.Contains(help.stdout, "dshd plugins --root <dir>") || !strings.Contains(help.stdout, "reconcile") {
		t.Fatalf("plugins --help exit=%d stdout=%q stderr=%q", help.exit, help.stdout, help.stderr)
	}
	missingRoot := callPluginGlueCLI(t, exe, decoyHome, 1, "list")
	assertPluginGlueError(t, missingRoot, "bad_request")
	if _, err := os.Stat(decoyHome); !os.IsNotExist(err) {
		t.Fatalf("CLI without --root consulted/created environment home %s: err=%v", decoyHome, err)
	}
	unknown := callPluginGlueCLI(t, exe, decoyHome, 1, "--root", root, "unknown")
	assertPluginGlueError(t, unknown, "bad_request")

	initial := callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "list")
	assertPluginGlueList(t, initial, 0)
	installed := pluginGluePlugin(t, callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "install", "--path", v1))
	if installed["version"] != "1.0.0" || installed["enabled"] != false || installed["source"] != v1 {
		t.Fatalf("CLI install returned wrong truth: %+v", installed)
	}
	enabled := pluginGluePlugin(t, callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "enable", "--id", "acme.audit"))
	if enabled["enabled"] != true {
		t.Fatalf("CLI enable did not persist truth: %+v", enabled)
	}
	disabled := pluginGluePlugin(t, callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "disable", "--id", "acme.audit"))
	if disabled["enabled"] != false {
		t.Fatalf("CLI disable did not persist truth: %+v", disabled)
	}
	updated := pluginGluePlugin(t, callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "update", "--id", "acme.audit", "--path", v2))
	if updated["version"] != "2.0.0" || updated["previousVersion"] != "1.0.0" || updated["source"] != v2 {
		t.Fatalf("CLI update returned wrong truth: %+v", updated)
	}
	restored := pluginGluePlugin(t, callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "restore", "--id", "acme.audit", "--version", "1.0.0"))
	if restored["version"] != "1.0.0" || restored["previousVersion"] != "2.0.0" || restored["source"] != v1 {
		t.Fatalf("CLI restore returned wrong truth: %+v", restored)
	}
	reconciled := callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "reconcile")
	result := reconciled["result"].(map[string]any)
	if result["recovered"] != float64(0) || len(result["actions"].([]any)) != 0 {
		t.Fatalf("CLI clean reconcile: %+v", result)
	}
	uninstalled := callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "uninstall", "--id", "acme.audit")
	if uninstalled["result"].(map[string]any)["uninstalled"] != true {
		t.Fatalf("CLI uninstall: %+v", uninstalled)
	}
	assertPluginGlueList(t, callPluginGlueCLI(t, exe, decoyHome, 0, "--root", root, "list"), 0)
	missingPlugin := callPluginGlueCLI(t, exe, decoyHome, 1, "--root", root, "enable", "--id", "acme.audit")
	assertPluginGlueError(t, missingPlugin, "not_found")
	if _, err := os.Stat(decoyHome); !os.IsNotExist(err) {
		t.Fatalf("real CLI lifecycle touched environment home %s: err=%v", decoyHome, err)
	}
	t.Log("PLUGIN_GLUE_CLI_SUMMARY OK=13 FAIL=0")
}

func pluginGluePlugin(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result object: %+v", envelope)
	}
	plugin, ok := result["plugin"].(map[string]any)
	if !ok {
		t.Fatalf("missing plugin object: %+v", envelope)
	}
	return plugin
}

func assertPluginGlueList(t *testing.T, envelope map[string]any, want int) []any {
	t.Helper()
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing list result: %+v", envelope)
	}
	plugins, ok := result["plugins"].([]any)
	if !ok || len(plugins) != want {
		t.Fatalf("plugin list count=%d want=%d: %+v", len(plugins), want, envelope)
	}
	return plugins
}

func assertPluginGlueError(t *testing.T, envelope map[string]any, wantCode string) {
	t.Helper()
	if len(envelope) != 1 {
		t.Fatalf("error envelope has extra keys: %+v", envelope)
	}
	errorObject, ok := envelope["error"].(map[string]any)
	if !ok || len(errorObject) != 4 {
		t.Fatalf("error is not exactly four fields: %+v", envelope)
	}
	for _, key := range []string{"code", "message", "hint"} {
		if errorObject[key] == "" || errorObject[key] == nil {
			t.Fatalf("error %s is empty: %+v", key, errorObject)
		}
	}
	if _, ok := errorObject["retryable"].(bool); !ok {
		t.Fatalf("retryable is not boolean: %+v", errorObject)
	}
	if wantCode != "" && errorObject["code"] != wantCode {
		t.Fatalf("error code=%v want=%s: %+v", errorObject["code"], wantCode, errorObject)
	}
}

type pluginGlueServer struct {
	cmd      *exec.Cmd
	base     string
	token    string
	stderr   bytes.Buffer
	done     chan struct{}
	waitErr  error
	stopOnce sync.Once
}

func startPluginGlueServer(t *testing.T, exe, home, decoyHome string) *pluginGlueServer {
	t.Helper()
	cmd := exec.Command(exe, "serve", "--home", home, "--addr", "127.0.0.1:0")
	cmd.Env = pluginGlueEnvironment(decoyHome)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	server := &pluginGlueServer{cmd: cmd, done: make(chan struct{})}
	cmd.Stderr = &server.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start real dshd serve: %v", err)
	}
	go func() {
		server.waitErr = cmd.Wait()
		close(server.done)
	}()
	t.Cleanup(server.stop)

	type readResult struct {
		line string
		err  error
	}
	read := make(chan readResult, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		read <- readResult{line: line, err: err}
	}()
	select {
	case got := <-read:
		if got.err != nil {
			server.stop()
			t.Fatalf("read ready: %v stderr=%s", got.err, server.stderr.String())
		}
		if !strings.HasPrefix(got.line, "ready ") {
			server.stop()
			t.Fatalf("bad ready line %q stderr=%s", got.line, server.stderr.String())
		}
		var ready readyLine
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(got.line, "ready "))), &ready); err != nil {
			server.stop()
			t.Fatalf("bad ready JSON: %v line=%q", err, got.line)
		}
		server.base, server.token = ready.URL, ready.Token
	case <-server.done:
		t.Fatalf("serve exited before ready: %v stderr=%s", server.waitErr, server.stderr.String())
	case <-time.After(20 * time.Second):
		server.stop()
		t.Fatalf("serve ready timeout stderr=%s", server.stderr.String())
	}
	if server.base == "" || server.token == "" || !strings.HasPrefix(server.base, "http://127.0.0.1:") {
		server.stop()
		t.Fatalf("bad ready values: base=%q token-empty=%v", server.base, server.token == "")
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return server
}

func (s *pluginGlueServer) stop() {
	s.stopOnce.Do(func() {
		select {
		case <-s.done:
		default:
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill() // only the child started by this test
			}
			<-s.done
		}
	})
}

func connectPluginGlueServer(t *testing.T, server *pluginGlueServer) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(server.base + "/connect?token=" + url.QueryEscape(server.token))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatalf("connect status=%d cookies=%d body=%s", resp.StatusCode, len(resp.Cookies()), body)
	}
	return client
}

func postPluginGlue(t *testing.T, client *http.Client, base, method string, params any, wantStatus int) map[string]any {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(base+"/api/"+method, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", method, err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s status=%d want=%d body=%s", method, resp.StatusCode, wantStatus, body)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("POST %s non-JSON: %v body=%s", method, err, body)
	}
	if wantStatus >= 400 {
		assertPluginGlueError(t, envelope, "")
	} else if len(envelope) != 1 || envelope["result"] == nil {
		t.Fatalf("POST %s success envelope=%+v", method, envelope)
	}
	return envelope
}

func testPluginGlueServe(t *testing.T, exe string) {
	decoyHome := filepath.Join(t.TempDir(), "serve-must-not-use-env-home")
	homeA := filepath.Join(t.TempDir(), "home-a")
	homeB := filepath.Join(t.TempDir(), "home-b")
	packages := t.TempDir()
	v1 := writePluginTransportPackage(t, filepath.Join(packages, "v1"), "1.0.0")
	v2 := writePluginTransportPackage(t, filepath.Join(packages, "v2"), "2.0.0")

	serverA := startPluginGlueServer(t, exe, homeA, decoyHome)
	serverB := startPluginGlueServer(t, exe, homeB, decoyHome)
	anonymous := &http.Client{Timeout: 15 * time.Second}
	for _, method := range []string{"plugin.list", "plugin.install", "plugin.enable", "plugin.disable", "plugin.update", "plugin.restore", "plugin.reconcile", "plugin.uninstall"} {
		envelope := postPluginGlue(t, anonymous, serverA.base, method, map[string]any{}, http.StatusUnauthorized)
		assertPluginGlueError(t, envelope, "unauthorized")
	}
	clientA := connectPluginGlueServer(t, serverA)
	clientB := connectPluginGlueServer(t, serverB)
	assertPluginGlueList(t, postPluginGlue(t, clientA, serverA.base, "plugin.list", map[string]any{}, http.StatusOK), 0)
	assertPluginGlueList(t, postPluginGlue(t, clientB, serverB.base, "plugin.list", map[string]any{}, http.StatusOK), 0)

	installedA := pluginGluePlugin(t, postPluginGlue(t, clientA, serverA.base, "plugin.install", map[string]any{"path": v1}, http.StatusOK))
	installedB := pluginGluePlugin(t, postPluginGlue(t, clientB, serverB.base, "plugin.install", map[string]any{"path": v2}, http.StatusOK))
	if installedA["version"] != "1.0.0" || installedB["version"] != "2.0.0" || installedA["enabled"] != false || installedB["enabled"] != false {
		t.Fatalf("AB install crossed homes: A=%+v B=%+v", installedA, installedB)
	}
	enabled := pluginGluePlugin(t, postPluginGlue(t, clientA, serverA.base, "plugin.enable", map[string]any{"id": "acme.audit"}, http.StatusOK))
	if enabled["enabled"] != true {
		t.Fatalf("HTTP enable: %+v", enabled)
	}
	disabled := pluginGluePlugin(t, postPluginGlue(t, clientA, serverA.base, "plugin.disable", map[string]any{"id": "acme.audit"}, http.StatusOK))
	if disabled["enabled"] != false {
		t.Fatalf("HTTP disable: %+v", disabled)
	}
	updated := pluginGluePlugin(t, postPluginGlue(t, clientA, serverA.base, "plugin.update", map[string]any{"id": "acme.audit", "path": v2}, http.StatusOK))
	if updated["version"] != "2.0.0" || updated["previousVersion"] != "1.0.0" || updated["enabled"] != false {
		t.Fatalf("HTTP update: %+v", updated)
	}
	restored := pluginGluePlugin(t, postPluginGlue(t, clientA, serverA.base, "plugin.restore", map[string]any{"id": "acme.audit", "version": "1.0.0"}, http.StatusOK))
	if restored["version"] != "1.0.0" || restored["previousVersion"] != "2.0.0" || restored["source"] != v1 {
		t.Fatalf("HTTP restore: %+v", restored)
	}
	pluginsB := assertPluginGlueList(t, postPluginGlue(t, clientB, serverB.base, "plugin.list", map[string]any{}, http.StatusOK), 1)
	pluginB := pluginsB[0].(map[string]any)
	if pluginB["version"] != "2.0.0" || pluginB["enabled"] != false || pluginB["source"] != v2 {
		t.Fatalf("home B changed during home A lifecycle: %+v", pluginB)
	}
	wrapped := postPluginGlue(t, clientA, serverA.base, "plugin.list", map[string]any{"params": map[string]any{}}, http.StatusBadRequest)
	assertPluginGlueError(t, wrapped, "bad_request")

	oldBase := serverA.base
	transactionDir := seedPluginGlueStartupRestore(t, filepath.Join(homeA, "plugins"))
	serverA.stop()
	parsed, _ := url.Parse(oldBase)
	conn, err := net.DialTimeout("tcp", parsed.Host, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("listener %s still accepts connections after child stop", parsed.Host)
	}

	serverA = startPluginGlueServer(t, exe, homeA, decoyHome)
	if _, err := os.Stat(transactionDir); !os.IsNotExist(err) {
		t.Fatalf("serve startup did not reconcile interrupted restore: dir=%s err=%v", transactionDir, err)
	}
	clientA = connectPluginGlueServer(t, serverA)
	pluginsA := assertPluginGlueList(t, postPluginGlue(t, clientA, serverA.base, "plugin.list", map[string]any{}, http.StatusOK), 1)
	pluginA := pluginsA[0].(map[string]any)
	if pluginA["version"] != "1.0.0" || pluginA["enabled"] != false || pluginA["source"] != v1 {
		t.Fatalf("listener restart lost state: %+v", pluginA)
	}
	reconciled := postPluginGlue(t, clientA, serverA.base, "plugin.reconcile", map[string]any{}, http.StatusOK)["result"].(map[string]any)
	if reconciled["recovered"] != float64(0) || len(reconciled["actions"].([]any)) != 0 {
		t.Fatalf("post-startup reconcile is not clean: %+v", reconciled)
	}

	// The explicit CLI root and the serve-derived home root are the same on-disk
	// manager. This is a real second process reading the serve-persisted state.
	fromCLI := callPluginGlueCLI(t, exe, decoyHome, 0, "--root", filepath.Join(homeA, "plugins"), "list")
	pluginsFromCLI := assertPluginGlueList(t, fromCLI, 1)
	if pluginsFromCLI[0].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("CLI and serve do not share home plugin root: %+v", fromCLI)
	}
	uninstalled := postPluginGlue(t, clientA, serverA.base, "plugin.uninstall", map[string]any{"id": "acme.audit"}, http.StatusOK)
	if uninstalled["result"].(map[string]any)["uninstalled"] != true {
		t.Fatalf("HTTP uninstall: %+v", uninstalled)
	}
	assertPluginGlueList(t, postPluginGlue(t, clientA, serverA.base, "plugin.list", map[string]any{}, http.StatusOK), 0)
	assertPluginGlueList(t, callPluginGlueCLI(t, exe, decoyHome, 0, "--root", filepath.Join(homeA, "plugins"), "list"), 0)
	assertPluginGlueList(t, postPluginGlue(t, clientB, serverB.base, "plugin.list", map[string]any{}, http.StatusOK), 1)
	missing := postPluginGlue(t, clientA, serverA.base, "plugin.enable", map[string]any{"id": "acme.audit"}, http.StatusNotFound)
	assertPluginGlueError(t, missing, "not_found")

	serverA.stop()
	serverB.stop()
	if _, err := os.Stat(decoyHome); !os.IsNotExist(err) {
		t.Fatalf("serve lifecycle touched environment home %s: err=%v", decoyHome, err)
	}
	t.Log("PLUGIN_GLUE_SERVE_SUMMARY OK=26 FAIL=0 (8 unauthenticated + 16 authenticated + 2 cross-process CLI)")
}

// seedPluginGlueStartupRestore writes a valid, uncommitted restore journal over
// an already-persisted v1 state. A real serve startup must reconcile it to the
// before state and remove the journal without changing either package version.
func seedPluginGlueStartupRestore(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	before := state.Plugins["acme.audit"]
	if len(before) == 0 {
		t.Fatalf("no acme.audit state in %s", raw)
	}
	var after map[string]any
	if err := json.Unmarshal(before, &after); err != nil {
		t.Fatal(err)
	}
	after["version"] = "2.0.0"
	after["previousVersion"] = "1.0.0"
	transaction := map[string]any{
		"schema":        "dsh.local-plugin-transaction@1",
		"id":            "glue-startup-reconcile",
		"operation":     "restore",
		"pluginId":      "acme.audit",
		"targetVersion": "2.0.0",
		"phase":         "committing",
		"before":        before,
		"after":         after,
	}
	encoded, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "transactions", "glue-startup-reconcile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transaction.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (r pluginGlueCLIResult) String() string {
	return fmt.Sprintf("exit=%d stdout=%s stderr=%s", r.exit, r.stdout, r.stderr)
}
