package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/internal/llm"
)

type modelCatalogGolden struct {
	Schema   string `json:"schema"`
	Accepted []struct {
		Name           string         `json:"name"`
		Method         string         `json:"method"`
		Params         map[string]any `json:"params"`
		Status         int            `json:"status"`
		Revision       int64          `json:"revision"`
		Order          []string       `json:"order"`
		TestOK         bool           `json:"testOK"`
		TestModel      string         `json:"testModel"`
		ActiveModel    string         `json:"activeModel"`
		ActiveRevision int64          `json:"activeRevision"`
	} `json:"accepted"`
	Rejected []struct {
		Name   string         `json:"name"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
		Status int            `json:"status"`
		Error  string         `json:"error"`
	} `json:"rejected"`
}

func readModelCatalogGolden(t *testing.T) modelCatalogGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "transport", "testdata", "model-catalog-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden modelCatalogGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if golden.Schema != "dshgo.model-catalog.golden@1" || len(golden.Accepted) != 6 || len(golden.Rejected) < 13 {
		t.Fatalf("bad model catalog golden: schema=%q accepted=%d rejected=%d", golden.Schema, len(golden.Accepted), len(golden.Rejected))
	}
	return golden
}

func expandModelCatalogValue(value any, baseURL string) any {
	switch typed := value.(type) {
	case string:
		if typed == "{{BASE_URL}}" {
			return baseURL
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = expandModelCatalogValue(typed[i], baseURL)
		}
	case map[string]any:
		for key := range typed {
			typed[key] = expandModelCatalogValue(typed[key], baseURL)
		}
	}
	return value
}

func modelCatalogResult(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result: %+v", envelope)
	}
	return result
}

func assertModelCatalogOrder(t *testing.T, result map[string]any, want []string) {
	t.Helper()
	rawModels, ok := result["models"].([]any)
	if !ok || len(rawModels) != len(want) {
		t.Fatalf("models=%v want order=%v", result["models"], want)
	}
	for i, id := range want {
		model := rawModels[i].(map[string]any)
		if model["id"] != id {
			t.Fatalf("model[%d].id=%v want=%s", i, model["id"], id)
		}
	}
}

func logModelCatalogEvidence(t *testing.T, label string, envelope map[string]any) {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "catalog-test-secret") || strings.Contains(string(raw), "wrong-secret") {
		t.Fatalf("credential echoed in %s: %s", label, raw)
	}
	t.Logf("%s %s", label, raw)
}

func waitModelCatalogTurn(t *testing.T, client *http.Client, base, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		history := postPluginGlue(t, client, base, "session.history", map[string]any{"id": sessionID, "sinceSeq": 0, "limit": 1000}, http.StatusOK)
		frames := modelCatalogResult(t, history)["frames"].([]any)
		for _, raw := range frames {
			frame := raw.(map[string]any)
			if frame["type"] == "turn/error" {
				t.Fatalf("turn failed: %+v", frame)
			}
			if frame["type"] == "turn/done" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn/done timeout session=%s frames=%+v", sessionID, frames)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestModelCatalogRealDSHDAuthenticatedGoldenABRestartAndRuntime(t *testing.T) {
	const secret = "catalog-test-secret"
	seenModels := make(chan string, 16)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":"received %s"}`, r.Header.Get("Authorization"))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"gateway-only","owned_by":"gateway"}]}`)
		case "/v1/chat/completions":
			var request struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if request.Stream {
				seenModels <- request.Model
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"local-response","model":%q,"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`, request.Model)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(gateway.Close)
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "1")
	t.Setenv("DSHD_LLM_BASE", gateway.URL+"/v1")
	t.Setenv("DSHD_LLM_KEY", secret)

	exe := buildPluginGlueDSHD(t)
	decoy := filepath.Join(t.TempDir(), "decoy-home")
	homeA := filepath.Join(t.TempDir(), "home-a")
	homeB := filepath.Join(t.TempDir(), "home-b")
	serverA := startPluginGlueServer(t, exe, homeA, decoy)
	serverB := startPluginGlueServer(t, exe, homeB, decoy)
	methods := []string{"models.catalog.list", "models.catalog.upsert", "models.catalog.reorder", "models.catalog.test", "models.catalog.set-active"}
	anonymous := &http.Client{Timeout: postTimeoutForSettings}
	for _, method := range methods {
		envelope := postPluginGlue(t, anonymous, serverA.base, method, map[string]any{}, http.StatusUnauthorized)
		assertPluginGlueError(t, envelope, "unauthorized")
	}
	clientA := connectPluginGlueServer(t, serverA)
	clientB := connectPluginGlueServer(t, serverB)
	initialA := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	initialB := modelCatalogResult(t, postPluginGlue(t, clientB, serverB.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	for name, result := range map[string]map[string]any{"A": initialA, "B": initialB} {
		if result["revision"] != float64(0) || len(result["models"].([]any)) != 0 {
			t.Fatalf("initial %s=%+v", name, result)
		}
		active := result["active"].(map[string]any)
		if active["id"] != "group/deepseek-v4.1-flash-1017" || active["revision"] != float64(0) {
			t.Fatalf("initial active %s=%+v", name, active)
		}
	}

	oldSession := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)
	golden := readModelCatalogGolden(t)
	for _, accepted := range golden.Accepted {
		params := expandModelCatalogValue(accepted.Params, gateway.URL+"/v1").(map[string]any)
		envelope := postPluginGlue(t, clientA, serverA.base, accepted.Method, params, accepted.Status)
		logModelCatalogEvidence(t, "MODEL_CATALOG_ACCEPT_"+accepted.Name, envelope)
		result := modelCatalogResult(t, envelope)
		if accepted.TestOK {
			if result["ok"] != true || result["model"] != accepted.TestModel || result["id"] != accepted.TestModel {
				t.Fatalf("test result=%+v", result)
			}
			continue
		}
		if result["revision"] != float64(accepted.Revision) {
			t.Fatalf("%s revision=%v want=%d", accepted.Name, result["revision"], accepted.Revision)
		}
		if accepted.Method == "models.catalog.set-active" {
			if result["activeModel"] != accepted.ActiveModel || result["activeRevision"] != float64(accepted.ActiveRevision) {
				t.Fatalf("set-active result=%+v", result)
			}
		} else {
			assertModelCatalogOrder(t, result, accepted.Order)
		}
	}

	for _, rejected := range golden.Rejected {
		params := expandModelCatalogValue(rejected.Params, gateway.URL+"/v1").(map[string]any)
		envelope := postPluginGlue(t, clientA, serverA.base, rejected.Method, params, rejected.Status)
		assertPluginGlueError(t, envelope, rejected.Error)
		logModelCatalogEvidence(t, "MODEL_CATALOG_REJECT_"+rejected.Name, envelope)
	}
	wrongCredential := postPluginGlue(t, clientA, serverA.base, "models.catalog.test", map[string]any{"id": "model-a", "credential": "wrong-secret"}, http.StatusBadGateway)
	assertPluginGlueError(t, wrongCredential, "upstream_error")
	logModelCatalogEvidence(t, "MODEL_CATALOG_REJECT_wrong credential", wrongCredential)
	emptyCatalog := postPluginGlue(t, clientB, serverB.base, "models.catalog.reorder", map[string]any{"ids": []string{}, "expectedRevision": 0}, http.StatusConflict)
	assertPluginGlueError(t, emptyCatalog, "empty_catalog")
	logModelCatalogEvidence(t, "MODEL_CATALOG_REJECT_empty catalog", emptyCatalog)

	settings := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "settings.form.get", map[string]any{"ns": "model"}, http.StatusOK))
	field := settings["fields"].([]any)[0].(map[string]any)
	if field["current"] != "model-a" || settings["revision"] != float64(1) {
		t.Fatalf("W61 default after set-active=%+v", settings)
	}
	afterRejected := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	if afterRejected["revision"] != float64(4) {
		t.Fatalf("rejected writes changed revision: %+v", afterRejected)
	}
	assertModelCatalogOrder(t, afterRejected, []string{"model-b", "model-a"})
	modelA := afterRejected["models"].([]any)[1].(map[string]any)
	if modelA["name"] != "Alpha Two" || modelA["futureScalar"] != float64(42) || modelA["futureField"] == nil || modelA["reasoningEfforts"] == nil || modelA["baseURL"] != gateway.URL+"/v1" {
		t.Fatalf("full fields did not round trip: %+v", modelA)
	}
	credential := afterRejected["credentials"].(map[string]any)["LOCAL_MODEL_CATALOG_KEY"].(map[string]any)
	if credential["configured"] != true || credential["writable"] != true {
		t.Fatalf("credential status=%+v", credential)
	}

	// The original canonical models.list remains the gateway projection, not a
	// silently repurposed persisted-catalog method.
	canonical := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "models.list", map[string]any{}, http.StatusOK))
	canonicalModels := canonical["models"].([]any)
	if len(canonicalModels) != 1 || canonicalModels[0].(map[string]any)["id"] != "gateway-only" {
		t.Fatalf("canonical models.list changed: %+v", canonical)
	}

	newSession := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)
	overridesRaw, err := os.ReadFile(filepath.Join(homeA, "state", "session-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	var overrides map[string]string
	if err := json.Unmarshal(overridesRaw, &overrides); err != nil {
		t.Fatal(err)
	}
	if overrides[oldSession] != "group/deepseek-v4.1-flash-1017" || overrides[newSession] != "model-a" {
		t.Fatalf("old/new session defaults retroactive or missing: old=%q new=%q", overrides[oldSession], overrides[newSession])
	}
	for _, expected := range []struct {
		session string
		model   string
	}{{oldSession, "group/deepseek-v4.1-flash-1017"}, {newSession, "model-a"}} {
		postPluginGlue(t, clientA, serverA.base, "session.prompt", map[string]any{"id": expected.session, "text": "prove active model"}, http.StatusOK)
		select {
		case got := <-seenModels:
			if got != expected.model {
				t.Fatalf("runtime session=%s model=%q want=%q", expected.session, got, expected.model)
			}
			t.Logf("MODEL_CATALOG_RUNTIME session=%s model=%s", expected.session, got)
		case <-time.After(15 * time.Second):
			t.Fatalf("model request timeout session=%s stderr=%s", expected.session, serverA.stderr.String())
		}
		waitModelCatalogTurn(t, clientA, serverA.base, expected.session)
	}

	serverA.stop()
	serverA = startPluginGlueServer(t, exe, homeA, decoy)
	clientA = connectPluginGlueServer(t, serverA)
	reopened := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	if reopened["revision"] != float64(4) || reopened["active"].(map[string]any)["id"] != "model-a" || reopened["active"].(map[string]any)["revision"] != float64(1) {
		t.Fatalf("reopened catalog=%+v", reopened)
	}
	assertModelCatalogOrder(t, reopened, []string{"model-b", "model-a"})
	logModelCatalogEvidence(t, "MODEL_CATALOG_REOPENED", map[string]any{"result": reopened})
	reopenedSession := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "session.create", map[string]any{}, http.StatusOK))["sessionId"].(string)
	for _, expected := range []struct {
		session string
		model   string
	}{{oldSession, "group/deepseek-v4.1-flash-1017"}, {reopenedSession, "model-a"}} {
		postPluginGlue(t, clientA, serverA.base, "session.prompt", map[string]any{"id": expected.session, "text": "prove reopened active model"}, http.StatusOK)
		select {
		case got := <-seenModels:
			if got != expected.model {
				t.Fatalf("reopened runtime session=%s model=%q want=%q", expected.session, got, expected.model)
			}
			t.Logf("MODEL_CATALOG_REOPENED_RUNTIME session=%s model=%s", expected.session, got)
		case <-time.After(15 * time.Second):
			t.Fatalf("reopened model request timeout session=%s stderr=%s", expected.session, serverA.stderr.String())
		}
		waitModelCatalogTurn(t, clientA, serverA.base, expected.session)
	}
	finalB := modelCatalogResult(t, postPluginGlue(t, clientB, serverB.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	if finalB["revision"] != float64(0) || len(finalB["models"].([]any)) != 0 || finalB["active"].(map[string]any)["id"] != "group/deepseek-v4.1-flash-1017" {
		t.Fatalf("home B changed: %+v", finalB)
	}
	t.Log("MODEL_CATALOG_GOLDEN_SUMMARY OK=6 FAIL=0")
	t.Log("MODEL_CATALOG_REJECT_SUMMARY OK=15 FAIL=0")
	t.Log("MODEL_CATALOG_RUNTIME_SUMMARY OK=4 FAIL=0")
}

func TestModelCatalogSetActiveReachesLoopRunnerWithoutRetroactiveRewrite(t *testing.T) {
	home := t.TempDir()
	capture := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", capture)
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	settings, _, permissions := openSettingsStores(t, home, "base-model", "allow")
	catalog := llm.OpenModelCatalog(home)
	httpServer := httptest.NewServer(newModelCatalogHTTPHandler(catalog, settings))
	t.Cleanup(httpServer.Close)
	post := func(method string, params any, want int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(params)
		resp, err := http.Post(httpServer.URL+"/api/"+method, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s status=%d body=%s", method, resp.StatusCode, body)
		}
		var envelope map[string]any
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope
	}
	post("models.catalog.upsert", map[string]any{"model": map[string]any{"id": "loop-active"}, "expectedRevision": 0}, http.StatusOK)
	ids := []string{"session-old", "session-new", "session-reopened"}
	create := func(string) (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	oldSession, err := createWithSettingsDefaults(create, settings)("")
	if err != nil {
		t.Fatal(err)
	}
	post("models.catalog.set-active", map[string]any{"id": "loop-active", "expectedRevision": 0}, http.StatusOK)
	newSession, err := createWithSettingsDefaults(create, settings)("")
	if err != nil {
		t.Fatal(err)
	}
	base := &loopRunner{home: home, policy: "allow", maxSteps: 6, model: "base-model"}
	runner := &modelOverrideLoopRunner{base: base, permissions: permissions}
	for _, expected := range []struct{ session, model string }{{oldSession, "base-model"}, {newSession, "loop-active"}} {
		if err := runner.Run(context.Background(), expected.session, "turn-"+expected.session, "next-prompt"); err != nil {
			t.Fatal(err)
		}
		model, _ := readLoopModelArg(t, capture)
		if model != expected.model {
			t.Fatalf("loop runner session=%s model=%q want=%q", expected.session, model, expected.model)
		}
		t.Logf("MODEL_CATALOG_LOOP_RUNTIME session=%s model=%s", expected.session, model)
	}

	reopenedSettings, _, reopenedPermissions := openSettingsStores(t, home, "base-model", "allow")
	reopenedCatalog := llm.OpenModelCatalog(home)
	view, err := reopenedCatalog.List()
	if err != nil || view.Revision != 1 {
		t.Fatalf("reopened catalog=%+v err=%v", view, err)
	}
	reopenedSession, err := createWithSettingsDefaults(create, reopenedSettings)("")
	if err != nil {
		t.Fatal(err)
	}
	reopenedRunner := &modelOverrideLoopRunner{base: base, permissions: reopenedPermissions}
	if err := reopenedRunner.Run(context.Background(), reopenedSession, "turn-reopened", "next-prompt"); err != nil {
		t.Fatal(err)
	}
	model, _ := readLoopModelArg(t, capture)
	if model != "loop-active" {
		t.Fatalf("reopened loop model=%q", model)
	}
	t.Log("MODEL_CATALOG_LOOP_SUMMARY OK=3 FAIL=0")
}
