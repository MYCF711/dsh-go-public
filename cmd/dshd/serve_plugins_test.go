package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dshgo/internal/pluginmgr"
)

func writePluginTransportPackage(t *testing.T, dir, version string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("private HTTP/CLI fixture " + version)
	sum := sha256.Sum256(payload)
	manifest := map[string]any{
		"schema": "dsh.local-plugin@1", "id": "acme.audit", "version": version,
		"kind": "wasm", "entry": "plugin.wasm",
		"files": map[string]string{"plugin.wasm": hex.EncodeToString(sum[:])},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "plugin.wasm"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPluginContractSnapshotIsAdditive(t *testing.T) {
	read := func(name string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "transport", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err = json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	canonical := read("api.snapshot.json")
	if got := len(canonical["methods"].([]any)); got != 25 {
		t.Fatalf("canonical method count changed: %d", got)
	}
	extension := read("plugins.snapshot.json")
	if got := len(extension["methods"].([]any)); got != 8 {
		t.Fatalf("plugin method count = %d", got)
	}
	for _, method := range extension["methods"].([]any) {
		for _, old := range canonical["methods"].([]any) {
			if method == old {
				t.Fatalf("plugin extension rewrote canonical method %v", method)
			}
		}
	}
	httpContract := extension["http"].(map[string]any)
	if httpContract["bareParams"] != true || !reflect.DeepEqual(httpContract["error"], []any{"code", "message", "hint", "retryable"}) {
		t.Fatalf("bad additive HTTP contract: %+v", httpContract)
	}
	ui := extension["ui"].(map[string]any)
	if ui["implemented"] != false || ui["mayFabricateFields"] != false {
		t.Fatalf("UI boundary was falsified: %+v", ui)
	}
}

func TestPluginHTTPGoldenLifecycle(t *testing.T) {
	home := t.TempDir()
	v1 := writePluginTransportPackage(t, filepath.Join(home, "private-v1"), "1.0.0")
	v2 := writePluginTransportPackage(t, filepath.Join(home, "private-v2"), "2.0.0")
	manager, err := pluginmgr.Open(filepath.Join(home, "manager"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newPluginHTTPHandler(manager))
	defer server.Close()

	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "transport", "testdata", "plugins-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Schema string `json:"schema"`
		Steps  []struct {
			Name     string          `json:"name"`
			Method   string          `json:"method"`
			Request  json.RawMessage `json:"request"`
			Status   int             `json:"status"`
			Response json.RawMessage `json:"response"`
		} `json:"steps"`
	}
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if golden.Schema != "dsh/transport-plugin-golden@1" || len(golden.Steps) < 12 {
		t.Fatalf("bad golden manifest: %s steps=%d", golden.Schema, len(golden.Steps))
	}
	expand := func(raw json.RawMessage) any {
		t.Helper()
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		var walk func(any) any
		walk = func(v any) any {
			switch x := v.(type) {
			case string:
				if x == "{{PACKAGE_V1}}" {
					return v1
				}
				if x == "{{PACKAGE_V2}}" {
					return v2
				}
			case []any:
				for i := range x {
					x[i] = walk(x[i])
				}
			case map[string]any:
				for k := range x {
					x[k] = walk(x[k])
				}
			}
			return v
		}
		return walk(v)
	}
	for _, step := range golden.Steps {
		t.Run(step.Name, func(t *testing.T) {
			body, err := json.Marshal(expand(step.Request))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Post(server.URL+"/api/"+step.Method, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			gotRaw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != step.Status {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, step.Status, gotRaw)
			}
			var got any
			if err = json.Unmarshal(gotRaw, &got); err != nil {
				t.Fatalf("non-JSON response: %s", gotRaw)
			}
			want := expand(step.Response)
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				t.Fatalf("response mismatch\n got=%s\nwant=%s", gotJSON, wantJSON)
			}
			if step.Status >= 400 {
				top := got.(map[string]any)
				errorObject := top["error"].(map[string]any)
				if len(top) != 1 || len(errorObject) != 4 {
					t.Fatalf("bad error envelope: %+v", got)
				}
			}
		})
	}
}

func TestPluginHTTPRejectsMalformedAndWrappedParams(t *testing.T) {
	manager, err := pluginmgr.Open(filepath.Join(t.TempDir(), "manager"))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"params":{}}`, `{"unknown":1}`, `{`, `null`, `[]`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/plugin.list", strings.NewReader(body))
		newPluginHTTPHandler(manager).ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q status=%d: %s", body, rec.Code, rec.Body.String())
		}
		var response struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Error) != 4 || response.Error["code"] != "bad_request" || response.Error["hint"] == "" {
			t.Fatalf("body %q bad error: %+v", body, response)
		}
	}
}

func TestPluginCLIRealProcessLifecycle(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	v1 := writePluginTransportPackage(t, filepath.Join(home, "private-v1"), "1.0.0")
	v2 := writePluginTransportPackage(t, filepath.Join(home, "private-v2"), "2.0.0")
	run := func(wantExit int, args ...string) map[string]any {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		argv := append([]string{"-test.run=^TestPluginCLIHelper$", "--", "--root", root}, args...)
		cmd := exec.Command(exe, argv...)
		cmd.Env = append(os.Environ(), "GO_WANT_PLUGIN_CLI=1")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err = cmd.Run()
		exit := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exit = exitErr.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if exit != wantExit {
			t.Fatalf("CLI %v exit=%d want=%d stdout=%s stderr=%s", args, exit, wantExit, stdout.String(), stderr.String())
		}
		raw := stdout.Bytes()
		if wantExit != 0 {
			raw = stderr.Bytes()
		}
		var envelope map[string]any
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("CLI %v non-JSON: %s / %s", args, stdout.String(), stderr.String())
		}
		return envelope
	}
	if got := run(0, "list"); !reflect.DeepEqual(got["result"], map[string]any{"plugins": []any{}}) {
		t.Fatalf("initial CLI list: %+v", got)
	}
	run(0, "install", "--path", v1)
	run(0, "enable", "--id", "acme.audit")
	updated := run(0, "update", "--id", "acme.audit", "--path", v2)
	plugin := updated["result"].(map[string]any)["plugin"].(map[string]any)
	if plugin["version"] != "2.0.0" || plugin["enabled"] != true || plugin["source"] != v2 {
		t.Fatalf("CLI update: %+v", plugin)
	}
	run(0, "disable", "--id", "acme.audit")
	restored := run(0, "restore", "--id", "acme.audit", "--version", "1.0.0")
	plugin = restored["result"].(map[string]any)["plugin"].(map[string]any)
	if plugin["version"] != "1.0.0" || plugin["enabled"] != false || plugin["source"] != v1 {
		t.Fatalf("CLI restore: %+v", plugin)
	}
	run(0, "reconcile")
	run(0, "uninstall", "--id", "acme.audit")
	failure := run(1, "enable", "--id", "acme.audit")
	errorObject := failure["error"].(map[string]any)
	if len(failure) != 1 || len(errorObject) != 4 || errorObject["code"] != "not_found" {
		t.Fatalf("CLI four-field error: %+v", failure)
	}
}

func TestPluginCLIHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PLUGIN_CLI") != "1" {
		return
	}
	sep := -1
	for i, arg := range os.Args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		os.Exit(92)
	}
	os.Exit(runPluginsCLI(os.Args[sep+1:], os.Stdout, os.Stderr))
}
