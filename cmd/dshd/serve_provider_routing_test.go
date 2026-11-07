package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/internal/llm"
	"dshgo/internal/transport"
)

const (
	providerTestSecretA  = "provider-a-stored-secret"
	providerTestSecretB  = "provider-b-stored-secret"
	providerTestFallback = "legacy-fallback-secret"
)

type providerWireRequest struct {
	Path            string
	Model           string
	AuthorizationOK bool
}

type providerWireEndpoint struct {
	label    string
	base     string
	requests chan providerWireRequest
}

func newProviderWireEndpoint(t *testing.T, label, expectedModel, expectedCredential string) *providerWireEndpoint {
	t.Helper()
	endpoint := &providerWireEndpoint{label: label, requests: make(chan providerWireRequest, 32)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                 `json:"model"`
			Messages []autoTitleWireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seen := providerWireRequest{
			Path: r.URL.Path, Model: request.Model,
			AuthorizationOK: r.Header.Get("Authorization") == "Bearer "+expectedCredential,
		}
		// 自动标题（官方 #14）是首轮收口后的额外真实模型调用；本探针断言的是主 turn
		// 走哪条 provider，标题请求不计入顺序，但仍要正常回 200，否则标题调用失败只是少一条标题。
		if !isAutoTitleWireRequest(request.Messages) {
			endpoint.requests <- seen
		}
		if r.URL.Path != "/v1/chat/completions" || request.Model != expectedModel || !seen.AuthorizationOK {
			http.Error(w, "provider wire mismatch", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	endpoint.base = server.URL + "/v1"
	return endpoint
}

func assertProviderWire(t *testing.T, got providerWireRequest, wantModel string) {
	t.Helper()
	if got.Path != "/v1/chat/completions" || got.Model != wantModel || !got.AuthorizationOK {
		t.Fatalf("provider wire mismatch: path=%q model=%q authorizationOK=%v wantModel=%q", got.Path, got.Model, got.AuthorizationOK, wantModel)
	}
}

func upsertProviderForTest(t *testing.T, home, id, base, ref, credential string, revision int64) {
	t.Helper()
	store := llm.OpenModelCatalog(home)
	model := map[string]any{"id": id}
	if base != "" {
		model["baseURL"] = base
	}
	if ref != "" {
		model["apiKeyEnv"] = ref
	}
	raw, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	var secret *string
	if credential != "" {
		secret = &credential
	}
	if _, err := store.Upsert(raw, secret, &revision); err != nil {
		t.Fatalf("upsert provider %s: %v", id, err)
	}
}

func setSessionProviderModel(t *testing.T, home, sessionID, model string) {
	t.Helper()
	overrides := newModelOverrideStore(home)
	if err := overrides.Load(); err != nil {
		t.Fatal(err)
	}
	if err := overrides.Set(sessionID, model); err != nil {
		t.Fatal(err)
	}
}

func TestModelRunnerCatalogProviderABLegacyAndReopen(t *testing.T) {
	home := t.TempDir()
	for _, sessionID := range []string{"legacy", "plain", "session-a", "session-b"} {
		createModelWireSession(t, home, t.TempDir(), sessionID)
	}
	endpointA := newProviderWireEndpoint(t, "A", "model-a", providerTestSecretA)
	endpointB := newProviderWireEndpoint(t, "B", "model-b", providerTestSecretB)
	fallback := newProviderWireEndpoint(t, "legacy", "legacy-model", providerTestFallback)
	upsertProviderForTest(t, home, "model-a", endpointA.base, "PROVIDER_A_KEY", providerTestSecretA, 0)
	upsertProviderForTest(t, home, "model-b", endpointB.base, "PROVIDER_B_KEY", providerTestSecretB, 1)
	upsertProviderForTest(t, home, "plain", "", "", "", 2)
	setSessionProviderModel(t, home, "session-a", "model-a")
	setSessionProviderModel(t, home, "session-b", "model-b")
	setSessionProviderModel(t, home, "legacy", "legacy-model")
	setSessionProviderModel(t, home, "plain", "plain")
	t.Setenv("DSHD_LLM_BASE", fallback.base)
	t.Setenv("DSHD_LLM_KEY", providerTestFallback)
	runner := &modelRunner{home: home, base: fallback.base, model: "legacy-model"}

	run := func(sessionID, prompt string) providerWireRequest {
		t.Helper()
		if err := runner.Run(context.Background(), sessionID, "turn-"+prompt, prompt); err != nil {
			t.Fatalf("Run(%s): %v", sessionID, err)
		}
		select {
		case got := <-endpointA.requests:
			return got
		case got := <-endpointB.requests:
			return got
		case got := <-fallback.requests:
			return got
		case <-time.After(5 * time.Second):
			t.Fatalf("no provider request for %s", sessionID)
			return providerWireRequest{}
		}
	}
	if got := run("legacy", "legacy-before-catalog"); got.Path != "/v1/chat/completions" || !got.AuthorizationOK {
		t.Fatalf("legacy fallback changed: %+v", got)
	}
	if got := run("session-a", "provider-a"); got.Model != "model-a" {
		t.Fatalf("A routed to wrong provider: %+v", got)
	} else {
		assertProviderWire(t, got, "model-a")
	}
	if got := run("session-b", "provider-b"); got.Model != "model-b" {
		t.Fatalf("B routed to wrong provider: %+v", got)
	} else {
		assertProviderWire(t, got, "model-b")
	}

	// Use a separate fallback recorder for the two legacy ids so model is checked
	// exactly without hiding a wrong-provider request behind a shared channel.
	plainRunnerHome := t.TempDir()
	createModelWireSession(t, plainRunnerHome, t.TempDir(), "plain")
	plainFallback := newProviderWireEndpoint(t, "plain-fallback", "plain", providerTestFallback)
	upsertProviderForTest(t, plainRunnerHome, "plain", "", "", "", 0)
	setSessionProviderModel(t, plainRunnerHome, "plain", "plain")
	plainRunner := &modelRunner{home: plainRunnerHome, base: plainFallback.base, model: "legacy-model"}
	if err := plainRunner.Run(context.Background(), "plain", "turn-plain", "plain"); err != nil {
		t.Fatal(err)
	}
	assertProviderWire(t, <-plainFallback.requests, "plain")

	runner = &modelRunner{home: home, base: fallback.base, model: "legacy-model"}
	if got := run("session-a", "provider-a-reopened"); got.Model != "model-a" {
		t.Fatalf("reopened A routed to wrong provider: %+v", got)
	} else {
		assertProviderWire(t, got, "model-a")
	}
	concurrentErrors := make(chan error, 2)
	go func() {
		concurrentErrors <- runner.Run(context.Background(), "session-a", "turn-concurrent-a", "concurrent-a")
	}()
	go func() {
		concurrentErrors <- runner.Run(context.Background(), "session-b", "turn-concurrent-b", "concurrent-b")
	}()
	for i := 0; i < 2; i++ {
		if err := <-concurrentErrors; err != nil {
			t.Fatalf("concurrent direct provider run: %v", err)
		}
	}
	assertProviderWire(t, <-endpointA.requests, "model-a")
	assertProviderWire(t, <-endpointB.requests, "model-b")
	if os.Getenv("DSHD_LLM_BASE") != fallback.base || os.Getenv("DSHD_LLM_KEY") != providerTestFallback {
		t.Fatal("provider routing mutated process-global DSHD_LLM_BASE/KEY")
	}
	select {
	case got := <-endpointB.requests:
		t.Fatalf("unexpected B request after reopen: model=%q", got.Model)
	default:
	}
	t.Log("PROVIDER_DIRECT_AB_REOPEN_SUMMARY OK=7 FAIL=0")
}

func TestModelRunnerProviderFailuresAreLoudAndRedacted(t *testing.T) {
	t.Run("wrong credential is redacted in error and turn frame", func(t *testing.T) {
		const secret = "wrong-provider-literal"
		home := t.TempDir()
		createModelWireSession(t, home, t.TempDir(), "session-a")
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"endpoint echoed %s"}`, secret)
		}))
		t.Cleanup(gateway.Close)
		upsertProviderForTest(t, home, "model-a", gateway.URL+"/v1", "PROVIDER_WRONG_KEY", secret, 0)
		setSessionProviderModel(t, home, "session-a", "model-a")
		var frames []transport.ServerFrame
		runner := &modelRunner{home: home, base: "http://127.0.0.1:1/v1", model: "legacy", pub: func(_ string, frame transport.ServerFrame) {
			frames = append(frames, frame)
		}}
		err := runner.Run(context.Background(), "session-a", "turn-wrong", "wrong credential")
		if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
			t.Fatalf("wrong credential error leaked or was not redacted: %v", err)
		}
		if len(frames) != 2 || frames[0].Type != "step/start" || frames[1].Type != TurnError {
			t.Fatalf("失败前真实 step 与 turn/error 帧序列不完整：%+v", frames)
		}
		for _, frame := range frames {
			if strings.Contains(string(frame.Data), secret) {
				t.Fatalf("frame leaked credential: %s", frame.Data)
			}
		}
		if !strings.Contains(string(frames[1].Data), "[redacted]") {
			t.Fatalf("turn/error credential redaction failed: %s", frames[1].Data)
		}
	})

	for _, test := range []struct {
		name       string
		model      string
		ref        string
		credential string
		corrupt    bool
		want       string
	}{
		{name: "missing baseURL", model: "missing-base", ref: "MISSING_BASE_KEY", credential: "configured-but-no-base", want: "baseURL"},
		{name: "missing credential", model: "missing-key", ref: "MISSING_RUNTIME_PROVIDER_KEY", want: "credential"},
		{name: "corrupt catalog", corrupt: true, want: "model-catalog.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			createModelWireSession(t, home, t.TempDir(), "session-a")
			fallback := newProviderWireEndpoint(t, "fallback", "legacy", providerTestFallback)
			if test.corrupt {
				path := filepath.Join(home, "state", "model-catalog.json")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				upsertProviderForTest(t, home, test.model, map[bool]string{true: "", false: fallback.base}[test.name == "missing baseURL"], test.ref, test.credential, 0)
			}
			setSessionProviderModel(t, home, "session-a", test.model)
			runner := &modelRunner{home: home, base: fallback.base, model: "legacy"}
			err := runner.Run(context.Background(), "session-a", "turn-invalid", "must not fall back")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid provider error=%v want substring=%q", err, test.want)
			}
			select {
			case request := <-fallback.requests:
				t.Fatalf("invalid provider silently fell back: model=%q", request.Model)
			default:
			}
		})
	}
}

func readProviderLoopEnvironment(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("child did not record environment: %v", err)
	}
	var env []string
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func providerEnvValue(env []string, key string) (string, int) {
	value := ""
	count := 0
	prefix := key + "="
	for _, entry := range env {
		if len(entry) >= len(prefix) && strings.EqualFold(entry[:len(prefix)], prefix) {
			value = strings.TrimPrefix(entry, prefix)
			count++
		}
	}
	return value, count
}

func TestLoopRunnerCatalogProviderArgvEnvironmentABReopenAndLegacy(t *testing.T) {
	home := t.TempDir()
	argvCapture := filepath.Join(t.TempDir(), "argv.json")
	envCapture := filepath.Join(t.TempDir(), "env.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", argvCapture)
	t.Setenv("DSHD_MODEL_WIRE_ENV", envCapture)
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	endpointA := newProviderWireEndpoint(t, "A", "model-a", providerTestSecretA)
	endpointB := newProviderWireEndpoint(t, "B", "model-b", providerTestSecretB)
	upsertProviderForTest(t, home, "model-a", endpointA.base, "PROVIDER_A_KEY", providerTestSecretA, 0)
	upsertProviderForTest(t, home, "model-b", endpointB.base, "PROVIDER_B_KEY", providerTestSecretB, 1)
	upsertProviderForTest(t, home, "plain", "", "", "", 2)
	for id, model := range map[string]string{"session-a": "model-a", "session-b": "model-b", "plain": "plain", "legacy": "legacy-model"} {
		setSessionProviderModel(t, home, id, model)
	}
	t.Setenv("DSHD_LLM_BASE", "http://legacy.invalid/v1")
	t.Setenv("DSHD_LLM_KEY", providerTestFallback)
	base := &loopRunner{home: home, policy: "deny", maxSteps: 6, model: "legacy-model"}
	runner := &modelOverrideLoopRunner{base: base}

	assertRun := func(sessionID, wantModel, wantBase, wantKey string, provider bool) {
		t.Helper()
		if err := runner.Run(context.Background(), sessionID, "turn-"+sessionID, "next-prompt"); err != nil {
			t.Fatalf("Run(%s): %v", sessionID, err)
		}
		model, args := readLoopModelArg(t, argvCapture)
		if model != wantModel {
			t.Fatalf("%s model=%q want=%q", sessionID, model, wantModel)
		}
		if provider {
			if !containsArgPair(args, "--base", wantBase) {
				t.Fatalf("%s missing --base provider endpoint: %v", sessionID, args)
			}
		} else {
			for _, arg := range args {
				if arg == "--base" {
					t.Fatalf("%s legacy model acquired --base: %v", sessionID, args)
				}
			}
		}
		joined := strings.Join(args, "\x00")
		for _, secret := range []string{providerTestSecretA, providerTestSecretB, providerTestFallback} {
			if strings.Contains(joined, secret) {
				t.Fatalf("%s credential appeared in argv", sessionID)
			}
		}
		env := readProviderLoopEnvironment(t, envCapture)
		if got, count := providerEnvValue(env, "DSHD_LLM_BASE"); got != wantBase || count != 1 {
			t.Fatalf("%s child DSHD_LLM_BASE count=%d matches=%v", sessionID, count, got == wantBase)
		}
		if got, count := providerEnvValue(env, "DSHD_LLM_KEY"); got != wantKey || count != 1 {
			t.Fatalf("%s child DSHD_LLM_KEY count=%d matches=%v", sessionID, count, got == wantKey)
		}
	}
	assertRun("session-a", "model-a", endpointA.base, providerTestSecretA, true)
	assertRun("session-b", "model-b", endpointB.base, providerTestSecretB, true)
	runner = &modelOverrideLoopRunner{base: base}
	assertRun("session-a", "model-a", endpointA.base, providerTestSecretA, true)
	assertRun("plain", "plain", "http://legacy.invalid/v1", providerTestFallback, false)
	assertRun("legacy", "legacy-model", "http://legacy.invalid/v1", providerTestFallback, false)
	if os.Getenv("DSHD_LLM_BASE") != "http://legacy.invalid/v1" || os.Getenv("DSHD_LLM_KEY") != providerTestFallback {
		t.Fatal("loop provider routing mutated process-global environment")
	}
	t.Log("PROVIDER_LOOP_ARGV_ENV_SUMMARY OK=5 FAIL=0")
}

func TestLoopRunnerInvalidProviderDoesNotSpawn(t *testing.T) {
	home := t.TempDir()
	upsertProviderForTest(t, home, "missing-key", "http://127.0.0.1:1/v1", "MISSING_LOOP_PROVIDER_KEY", "", 0)
	setSessionProviderModel(t, home, "session-a", "missing-key")
	t.Setenv("MISSING_LOOP_PROVIDER_KEY", "")
	argvCapture := filepath.Join(t.TempDir(), "must-not-spawn.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", argvCapture)
	t.Setenv("DSHD_MODEL_WIRE_ENV", filepath.Join(t.TempDir(), "must-not-spawn-env.json"))
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	runner := &modelOverrideLoopRunner{base: &loopRunner{home: home, policy: "deny", maxSteps: 6, model: "legacy"}}
	err := runner.Run(context.Background(), "session-a", "turn-missing", "must not spawn")
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("missing loop credential error=%v", err)
	}
	if _, statErr := os.Stat(argvCapture); !os.IsNotExist(statErr) {
		t.Fatalf("invalid provider spawned child: stat=%v", statErr)
	}
}

func TestProviderRoutingRealDSHDLoopABAndRestart(t *testing.T) {
	endpointA := newProviderWireEndpoint(t, "A", "model-a", providerTestSecretA)
	endpointB := newProviderWireEndpoint(t, "B", "model-b", providerTestSecretB)
	fallback := newProviderWireEndpoint(t, "legacy", "group/deepseek-v4.1-flash-1017", providerTestFallback)
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "")
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "")
	t.Setenv("DSHD_LLM_BASE", fallback.base)
	t.Setenv("DSHD_LLM_KEY", providerTestFallback)
	exe := buildPluginGlueDSHD(t)
	home := filepath.Join(t.TempDir(), "provider-home")
	server := startPluginGlueServer(t, exe, home, filepath.Join(t.TempDir(), "decoy-home"))
	client := connectPluginGlueServer(t, server)
	oldSession := modelCatalogResult(t, postPluginGlue(t, client, server.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)
	for _, item := range []struct {
		id, base, ref, secret string
		revision              int64
	}{{"model-a", endpointA.base, "PROVIDER_A_KEY", providerTestSecretA, 0}, {"model-b", endpointB.base, "PROVIDER_B_KEY", providerTestSecretB, 1}} {
		postPluginGlue(t, client, server.base, "models.catalog.upsert", map[string]any{
			"model":      map[string]any{"id": item.id, "api": "openai-completions", "baseURL": item.base, "apiKeyEnv": item.ref},
			"credential": item.secret, "expectedRevision": item.revision,
		}, http.StatusOK)
	}
	postPluginGlue(t, client, server.base, "models.catalog.set-active", map[string]any{"id": "model-a", "expectedRevision": 0}, http.StatusOK)
	sessionA := modelCatalogResult(t, postPluginGlue(t, client, server.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)
	postPluginGlue(t, client, server.base, "models.catalog.set-active", map[string]any{"id": "model-b", "expectedRevision": 1}, http.StatusOK)
	sessionB := modelCatalogResult(t, postPluginGlue(t, client, server.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)

	expectRequest := func(wantLabel, wantModel string) {
		t.Helper()
		var label string
		var got providerWireRequest
		select {
		case got = <-endpointA.requests:
			label = "A"
		case got = <-endpointB.requests:
			label = "B"
		case got = <-fallback.requests:
			label = "legacy"
		case <-time.After(15 * time.Second):
			t.Fatalf("provider request timeout want=%s stderr=%s", wantLabel, server.stderr.String())
		}
		if label != wantLabel {
			t.Fatalf("request routed to %s, want %s (model=%q)", label, wantLabel, got.Model)
		}
		assertProviderWire(t, got, wantModel)
	}
	prompt := func(sessionID, label, model string) {
		t.Helper()
		postPluginGlue(t, client, server.base, "session.prompt", map[string]any{"id": sessionID, "text": "prove " + label}, http.StatusOK)
		expectRequest(label, model)
		waitModelCatalogTurn(t, client, server.base, sessionID)
	}
	prompt(oldSession, "legacy", "group/deepseek-v4.1-flash-1017")
	prompt(sessionA, "A", "model-a")
	prompt(sessionB, "B", "model-b")

	server.stop()
	server = startPluginGlueServer(t, exe, home, filepath.Join(t.TempDir(), "decoy-home-reopened"))
	client = connectPluginGlueServer(t, server)
	reopenedB := modelCatalogResult(t, postPluginGlue(t, client, server.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)
	prompt(sessionA, "A", "model-a")
	prompt(reopenedB, "B", "model-b")

	concurrentA := modelCatalogResult(t, postPluginGlue(t, client, server.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)
	concurrentB := modelCatalogResult(t, postPluginGlue(t, client, server.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)
	postPluginGlue(t, client, server.base, "session.setModel", map[string]any{"id": concurrentA, "model": "model-a"}, http.StatusOK)
	postPluginGlue(t, client, server.base, "session.prompt", map[string]any{"id": concurrentA, "text": "concurrent provider A"}, http.StatusOK)
	postPluginGlue(t, client, server.base, "session.prompt", map[string]any{"id": concurrentB, "text": "concurrent provider B"}, http.StatusOK)
	seenConcurrent := map[string]bool{}
	for len(seenConcurrent) < 2 {
		select {
		case got := <-endpointA.requests:
			assertProviderWire(t, got, "model-a")
			seenConcurrent["A"] = true
		case got := <-endpointB.requests:
			assertProviderWire(t, got, "model-b")
			seenConcurrent["B"] = true
		case got := <-fallback.requests:
			t.Fatalf("concurrent provider request fell back to legacy: model=%q", got.Model)
		case <-time.After(15 * time.Second):
			t.Fatalf("concurrent provider timeout: seen=%v stderr=%s", seenConcurrent, server.stderr.String())
		}
	}
	waitModelCatalogTurn(t, client, server.base, concurrentA)
	waitModelCatalogTurn(t, client, server.base, concurrentB)
	t.Log("PROVIDER_REAL_LOOP_AB_RESTART_SUMMARY OK=7 FAIL=0")
}
