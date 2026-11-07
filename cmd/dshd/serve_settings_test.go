package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/internal/settingsform"
)

func openSettingsStores(t *testing.T, home, baseModel, basePolicy string) (*serveSettingsStore, *modelOverrideStore, *permissionOverrideStore) {
	t.Helper()
	models := newModelOverrideStore(home)
	if err := models.Load(); err != nil {
		t.Fatal(err)
	}
	permissions := newPermissionOverrideStore(home)
	if err := permissions.Load(); err != nil {
		t.Fatal(err)
	}
	return newServeSettingsStore(baseModel, basePolicy, models, permissions), models, permissions
}

func setDefaultForTest(t *testing.T, store *serveSettingsStore, namespace, field, value string, revision int64) {
	t.Helper()
	svc := settingsform.New(store)
	_, err := svc.Mutate(namespace, []settingsform.PathOp{{Op: "set", Path: []string{field}, Value: value}}, &revision)
	if err != nil {
		t.Fatal(err)
	}
}

func resetDefaultForTest(t *testing.T, store *serveSettingsStore, namespace string, revision int64) {
	t.Helper()
	if _, err := settingsform.New(store).Reset(namespace, nil, &revision); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsDefaultsReachExistingTurnRunnerAcrossABResetAndReopen(t *testing.T) {
	home := t.TempDir()
	capture := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", capture)
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	store, models, permissions := openSettingsStores(t, home, "base-model", "allow")

	ids := []string{"session-a", "session-b", "session-c"}
	create := func(string) (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	createWithDefaults := createWithSettingsDefaults(create, store)

	setDefaultForTest(t, store, settingsform.NamespaceModel, settingsform.FieldDefaultModel, "model-A", 0)
	setDefaultForTest(t, store, settingsform.NamespacePermission, settingsform.FieldDefaultPreset, "deny", 0)
	sessionA, err := createWithDefaults("")
	if err != nil || sessionA != "session-a" {
		t.Fatalf("create A=%q err=%v", sessionA, err)
	}
	setDefaultForTest(t, store, settingsform.NamespaceModel, settingsform.FieldDefaultModel, "model-B", 1)
	setDefaultForTest(t, store, settingsform.NamespacePermission, settingsform.FieldDefaultPreset, "ask", 1)
	sessionB, err := createWithDefaults("")
	if err != nil || sessionB != "session-b" {
		t.Fatalf("create B=%q err=%v", sessionB, err)
	}
	resetDefaultForTest(t, store, settingsform.NamespaceModel, 2)
	resetDefaultForTest(t, store, settingsform.NamespacePermission, 2)
	sessionC, err := createWithDefaults("")
	if err != nil || sessionC != "session-c" {
		t.Fatalf("create C=%q err=%v", sessionC, err)
	}

	base := &loopRunner{home: home, policy: "allow", maxSteps: 6, model: "base-model"}
	runner := &modelOverrideLoopRunner{base: base, permissions: permissions}
	assertRun := func(sid, wantModel, wantPolicy string) {
		t.Helper()
		if err := runner.Run(context.Background(), sid, "turn-"+sid, "next-prompt"); err != nil {
			t.Fatalf("Run(%s): %v", sid, err)
		}
		model, args := readLoopModelArg(t, capture)
		if model != wantModel || !containsArgPair(args, "--policy", wantPolicy) {
			t.Fatalf("%s argv model=%q policy-want=%q args=%v", sid, model, wantPolicy, args)
		}
	}
	assertRun(sessionA, "model-A", "deny")
	assertRun(sessionB, "model-B", "ask")
	assertRun(sessionC, "base-model", "allow")

	// Reopen both existing files; the original runner still resolves session A/B/C.
	store, models, permissions = openSettingsStores(t, home, "base-model", "allow")
	_ = store
	_ = models
	runner = &modelOverrideLoopRunner{base: base, permissions: permissions}
	assertRun(sessionA, "model-A", "deny")
	assertRun(sessionB, "model-B", "ask")
	assertRun(sessionC, "base-model", "allow")
	if got := settingsform.New(store); got == nil {
		t.Fatal("reopened service is nil")
	}
	t.Log("SETTINGS_RUNNER_SUMMARY OK=1 FAIL=0")
}

func TestSettingsStoreFailedFlushRollsBackMemory(t *testing.T) {
	home := t.TempDir()
	store, models, _ := openSettingsStores(t, home, "base-model", "allow")
	setDefaultForTest(t, store, settingsform.NamespaceModel, settingsform.FieldDefaultModel, "persisted-model", 0)

	// Rename a file over an existing directory fails on Windows and POSIX. Only
	// this temporary test store is redirected; no shared path is touched.
	blocked := filepath.Join(home, "blocked-target")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	models.path = blocked
	_, err := store.Write(settingsform.NamespaceModel, map[string]any{settingsform.FieldDefaultModel: "must-not-appear"}, nil)
	if err == nil {
		t.Fatal("flush over directory unexpectedly succeeded")
	}
	doc, readErr := store.Read(settingsform.NamespaceModel)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := effectiveSettingsValue(doc, settingsform.FieldDefaultModel); got != "persisted-model" || doc.Revision != 1 {
		t.Fatalf("failed flush changed readable state: value=%q revision=%d", got, doc.Revision)
	}
}

func TestSettingsStoreStartupReadFailureIsNotDefaultSuccess(t *testing.T) {
	home := t.TempDir()
	store, _, _ := openSettingsStores(t, home, "base-model", "allow")
	store.setLoadErrors(errors.New("session-model.json: invalid JSON"), nil)
	_, err := settingsform.New(store).Get(settingsform.NamespaceModel)
	if err == nil || settingsform.AsError(err).Code != "internal" {
		t.Fatalf("startup corruption returned fake default: %v", err)
	}
	_, err = store.Write(settingsform.NamespaceModel, map[string]any{settingsform.FieldDefaultModel: "new"}, nil)
	if err == nil || settingsform.AsError(err).Code != "internal" {
		t.Fatalf("startup corruption allowed write: %v", err)
	}
}

func TestSettingsStoreRejectsCorruptReservedMetadata(t *testing.T) {
	home := t.TempDir()
	_, models, _ := openSettingsStores(t, home, "base-model", "allow")
	if err := os.MkdirAll(filepath.Dir(models.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(models.path, []byte(`{"\u0000settingsFormRevision":"broken"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, _, _ := openSettingsStores(t, home, "base-model", "allow")
	_, err := settingsform.New(store).Get(settingsform.NamespaceModel)
	if err == nil || settingsform.AsError(err).Code != "internal" {
		t.Fatalf("corrupt revision error=%v", err)
	}
}

type settingsGolden struct {
	Accepted []struct {
		Method   string         `json:"method"`
		Params   map[string]any `json:"params"`
		Status   int            `json:"status"`
		Current  string         `json:"current"`
		Revision int64          `json:"revision"`
	} `json:"accepted"`
	Rejected []struct {
		Name   string         `json:"name"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
		Status int            `json:"status"`
		Error  string         `json:"error"`
	} `json:"rejected"`
}

func readSettingsGolden(t *testing.T) settingsGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "transport", "testdata", "settings-form-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden settingsGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Accepted) == 0 || len(golden.Rejected) == 0 {
		t.Fatalf("empty golden: %+v", golden)
	}
	return golden
}

func settingsEnvelopeCurrent(t *testing.T, envelope map[string]any) (string, int64) {
	t.Helper()
	result := envelope["result"].(map[string]any)
	revision := int64(result["revision"].(float64))
	if fields, ok := result["fields"].([]any); ok {
		field := fields[0].(map[string]any)
		return field["current"].(string), revision
	}
	values := result["value"].(map[string]any)
	for _, value := range values {
		return value.(string), revision
	}
	t.Fatalf("no current value in %+v", envelope)
	return "", 0
}

func logSettingsEvidence(t *testing.T, label string, envelope map[string]any) {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s %s", label, raw)
}

func TestSettingsFormRealDSHDAuthenticatedGoldenABRestartReset(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	decoy := filepath.Join(t.TempDir(), "decoy-home")
	homeA := filepath.Join(t.TempDir(), "home-a")
	homeB := filepath.Join(t.TempDir(), "home-b")
	serverA := startPluginGlueServer(t, exe, homeA, decoy)
	serverB := startPluginGlueServer(t, exe, homeB, decoy)

	anonymous := &http.Client{Timeout: postTimeoutForSettings}
	for _, method := range []string{"settings.describe", "settings.mutate", "settings.form.describe", "settings.form.get", "settings.form.set", "settings.form.reset"} {
		envelope := postPluginGlue(t, anonymous, serverA.base, method, map[string]any{}, http.StatusUnauthorized)
		assertPluginGlueError(t, envelope, "unauthorized")
	}
	clientA := connectPluginGlueServer(t, serverA)
	clientB := connectPluginGlueServer(t, serverB)

	described := postPluginGlue(t, clientA, serverA.base, "settings.describe", map[string]any{}, http.StatusOK)
	logSettingsEvidence(t, "REAL_DESCRIBE", described)
	result := described["result"].(map[string]any)
	if len(result["namespaces"].([]any)) != 2 || len(result["sections"].([]any)) != 2 || len(result["errors"].([]any)) != 0 {
		t.Fatalf("bad real describe: %+v", result)
	}
	initialGet := postPluginGlue(t, clientA, serverA.base, "settings.form.get", map[string]any{"ns": "permission"}, http.StatusOK)
	if current, revision := settingsEnvelopeCurrent(t, initialGet); current != "allow" || revision != 0 {
		t.Fatalf("initial get current=%q revision=%d", current, revision)
	}
	logSettingsEvidence(t, "REAL_GET_INITIAL", initialGet)
	golden := readSettingsGolden(t)
	for _, accepted := range golden.Accepted {
		envelope := postPluginGlue(t, clientA, serverA.base, accepted.Method, accepted.Params, accepted.Status)
		current, revision := settingsEnvelopeCurrent(t, envelope)
		if current != accepted.Current || revision != accepted.Revision {
			t.Fatalf("accepted %s current=%q revision=%d, want %q/%d", accepted.Method, current, revision, accepted.Current, accepted.Revision)
		}
		logSettingsEvidence(t, "GOLDEN_ACCEPT", envelope)
	}
	for _, rejected := range golden.Rejected {
		params := rejected.Params
		if rejected.Name == "out of range" {
			params["value"] = strings.Repeat("x", 257)
		}
		envelope := postPluginGlue(t, clientA, serverA.base, rejected.Method, params, rejected.Status)
		assertPluginGlueError(t, envelope, rejected.Error)
		logSettingsEvidence(t, "GOLDEN_REJECT_"+rejected.Name, envelope)
	}

	// Set both again after the golden reset; these are the values that must survive restart.
	permissionSet := postPluginGlue(t, clientA, serverA.base, "settings.form.set", map[string]any{
		"ns": "permission", "path": []string{"defaultPreset"}, "value": "deny", "expectedRevision": 2,
	}, http.StatusOK)
	modelSet := postPluginGlue(t, clientA, serverA.base, "settings.mutate", map[string]any{
		"ns": "model", "ops": []any{map[string]any{"op": "set", "path": []string{"defaultModel"}, "value": "settings-model-A"}}, "expectedRevision": 2,
	}, http.StatusOK)
	logSettingsEvidence(t, "REAL_SET_PERMISSION", permissionSet)
	logSettingsEvidence(t, "REAL_SET_MODEL", modelSet)

	conflict := postPluginGlue(t, clientA, serverA.base, "settings.form.set", map[string]any{
		"ns": "permission", "path": []string{"defaultPreset"}, "value": "allow", "expectedRevision": 2,
	}, http.StatusConflict)
	assertPluginGlueError(t, conflict, "revision_conflict")
	logSettingsEvidence(t, "REAL_REVISION_CONFLICT", conflict)

	permissionB := postPluginGlue(t, clientB, serverB.base, "settings.form.get", map[string]any{"ns": "permission"}, http.StatusOK)
	if current, revision := settingsEnvelopeCurrent(t, permissionB); current != "allow" || revision != 0 {
		t.Fatalf("home B changed: current=%q revision=%d", current, revision)
	}

	created := postPluginGlue(t, clientA, serverA.base, "session.create", map[string]any{}, http.StatusOK)
	sessionID := created["result"].(map[string]any)["sessionId"].(string)
	loaded := postPluginGlue(t, clientA, serverA.base, "session.load", map[string]any{"id": sessionID, "tailMessages": 1}, http.StatusOK)
	if got := loaded["result"].(map[string]any)["permission"]; got != "deny" {
		t.Fatalf("new session runtime permission=%v, want deny", got)
	}
	modelRaw, err := os.ReadFile(filepath.Join(homeA, "state", "session-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	var modelValues map[string]string
	if err := json.Unmarshal(modelRaw, &modelValues); err != nil {
		t.Fatal(err)
	}
	if modelValues[sessionID] != "settings-model-A" {
		t.Fatalf("new session model storage=%q", modelValues[sessionID])
	}
	t.Logf("REAL_NEW_SESSION id=%s permission=deny model=settings-model-A", sessionID)

	serverA.stop()
	serverA = startPluginGlueServer(t, exe, homeA, decoy)
	clientA = connectPluginGlueServer(t, serverA)
	afterRestart := postPluginGlue(t, clientA, serverA.base, "settings.form.get", map[string]any{"ns": "model"}, http.StatusOK)
	if current, revision := settingsEnvelopeCurrent(t, afterRestart); current != "settings-model-A" || revision != 3 {
		t.Fatalf("restart model current=%q revision=%d", current, revision)
	}
	logSettingsEvidence(t, "REAL_AFTER_RESTART", afterRestart)

	resetPermission := postPluginGlue(t, clientA, serverA.base, "settings.form.reset", map[string]any{"ns": "permission", "expectedRevision": 3}, http.StatusOK)
	resetModel := postPluginGlue(t, clientA, serverA.base, "settings.mutate", map[string]any{
		"ns": "model", "ops": []any{map[string]any{"op": "unset", "path": []string{"defaultModel"}}}, "expectedRevision": 3,
	}, http.StatusOK)
	if current, _ := settingsEnvelopeCurrent(t, resetPermission); current != "allow" {
		t.Fatalf("reset permission=%q", current)
	}
	if current, _ := settingsEnvelopeCurrent(t, resetModel); current != "group/deepseek-v4.1-flash-1017" {
		t.Fatalf("reset model=%q", current)
	}
	logSettingsEvidence(t, "REAL_RESET_PERMISSION", resetPermission)
	logSettingsEvidence(t, "REAL_RESET_MODEL", resetModel)

	serverA.stop()
	serverA = startPluginGlueServer(t, exe, homeA, decoy)
	clientA = connectPluginGlueServer(t, serverA)
	afterResetRestart := postPluginGlue(t, clientA, serverA.base, "settings.form.get", map[string]any{"ns": "permission"}, http.StatusOK)
	if current, revision := settingsEnvelopeCurrent(t, afterResetRestart); current != "allow" || revision != 4 {
		t.Fatalf("reset restart current=%q revision=%d", current, revision)
	}
	logSettingsEvidence(t, "REAL_AFTER_RESET_RESTART", afterResetRestart)

	// A real dshd modelRunner must put the persisted new-session default on the
	// actual OpenAI-compatible request. The endpoint is local and answers one
	// ordinary SSE assistant message; no external model or GUI is involved.
	seenModel := make(chan string, 1)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seenModel <- request.Model
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(gateway.Close)
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "1")
	t.Setenv("DSHD_LLM_BASE", gateway.URL+"/v1")
	t.Setenv("DSHD_LLM_KEY", "")
	homeModel := filepath.Join(t.TempDir(), "home-model-runtime")
	modelServer := startPluginGlueServer(t, exe, homeModel, decoy)
	modelClient := connectPluginGlueServer(t, modelServer)
	postPluginGlue(t, modelClient, modelServer.base, "settings.form.set", map[string]any{
		"ns": "model", "path": []string{"defaultModel"}, "value": "runtime-settings-model", "expectedRevision": 0,
	}, http.StatusOK)
	created = postPluginGlue(t, modelClient, modelServer.base, "session.create", map[string]any{}, http.StatusOK)
	runtimeSession := created["result"].(map[string]any)["sessionId"].(string)
	postPluginGlue(t, modelClient, modelServer.base, "session.prompt", map[string]any{"id": runtimeSession, "text": "prove model"}, http.StatusOK)
	select {
	case got := <-seenModel:
		if got != "runtime-settings-model" {
			t.Fatalf("runtime request model=%q", got)
		}
		t.Logf("REAL_RUNTIME_MODEL session=%s model=%s", runtimeSession, got)
	case <-time.After(15 * time.Second):
		t.Fatalf("model request not received; serve stderr=%s", modelServer.stderr.String())
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		history := postPluginGlue(t, modelClient, modelServer.base, "session.history", map[string]any{"id": runtimeSession, "sinceSeq": 0, "limit": 100}, http.StatusOK)
		frames := history["result"].(map[string]any)["frames"].([]any)
		done := false
		for _, raw := range frames {
			frame := raw.(map[string]any)
			if frame["type"] == "turn/error" {
				t.Fatalf("runtime model turn failed: %+v", frame)
			}
			done = done || frame["type"] == "turn/done"
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn/done not received; frames=%+v stderr=%s", frames, modelServer.stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Log("REAL_RUNTIME_TURN_DONE OK=1 FAIL=0")
	t.Log("SETTINGS_FORM_REAL_SUMMARY OK=1 FAIL=0")
}

const postTimeoutForSettings = 15 * time.Second
