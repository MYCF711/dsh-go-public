package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"
)

type envModelRequest struct {
	Model     string `json:"model"`
	Reasoning string `json:"reasoning_effort"`
	Stream    bool   `json:"stream"`
}

func envModelPromptAndAssert(t *testing.T, client *http.Client, server *pluginGlueServer, sessionID, prompt string,
	requests <-chan envModelRequest, wantModel, wantReasoning string) {
	t.Helper()
	accepted := postEffortGoalPromptWhenReady(t, client, server.base, sessionID, prompt)
	turnID := accepted["result"].(map[string]any)["turnId"].(string)
	select {
	case request := <-requests:
		if request.Model != wantModel || request.Reasoning != wantReasoning {
			t.Fatalf("runner request=%+v want model=%q reasoning=%q", request, wantModel, wantReasoning)
		}
		t.Logf("ENV_MODEL_RUNTIME session=%s model=%s reasoning=%s", sessionID, request.Model, request.Reasoning)
	case <-time.After(15 * time.Second):
		t.Fatalf("runner request timeout session=%s stderr=%s", sessionID, server.stderr.String())
	}
	waitEffortGoalTurn(t, client, server.base, sessionID, turnID)
}

func newEnvModelGateway(t *testing.T) (string, <-chan envModelRequest, *atomic.Int32) {
	t.Helper()
	requests := make(chan envModelRequest, 16)
	var discoveries atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			discoveries.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[`+
				`{"id":"env-model-a","display_name":"Env A","owned_by":"provider-a","context_window":1000000,"max_output_tokens":128000,"modalities":{"input":["text","image"]},"supported_reasoning_levels":[{"effort":"xhigh"},{"effort":"max"}]},`+
				`{"id":"env-model-b","display_name":"Env B","owned_by":"provider-b","context_window":2000000,"max_output_tokens":64000,"modalities":{"input":["text"]},"supported_reasoning_levels":[{"effort":"low"}]}`+
				`]}`)
		case "/v1/chat/completions":
			var request envModelRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			requests <- request
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(gateway.Close)
	return gateway.URL, requests, &discoveries
}

func assertEnvCatalogEntry(t *testing.T, result map[string]any, id string, contextWindow float64, levels ...string) {
	t.Helper()
	models := result["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models=%+v want one %s", models, id)
	}
	model := models[0].(map[string]any)
	if model["id"] != id || model["contextWindow"] != contextWindow || model["reasoningEfforts"] == nil {
		t.Fatalf("model=%+v want id=%s contextWindow=%v", model, id, contextWindow)
	}
	efforts := model["reasoningEfforts"].(map[string]any)
	if len(efforts) != len(levels) {
		t.Fatalf("reasoningEfforts=%+v want=%v", efforts, levels)
	}
	for _, level := range levels {
		if efforts[level] != level {
			t.Fatalf("reasoningEfforts[%q]=%v want exact provider wire %q", level, efforts[level], level)
		}
	}
	for _, forbidden := range []string{"off", "low", "high"} {
		if _, exists := efforts[forbidden]; exists {
			found := false
			for _, level := range levels {
				found = found || level == forbidden
			}
			if !found {
				t.Fatalf("fabricated effort %q in %+v", forbidden, efforts)
			}
		}
	}
	active := result["active"].(map[string]any)
	if active["id"] != id {
		t.Fatalf("active=%+v want %s", active, id)
	}
}

func TestEnvModelCatalogRealDSHDABEffortRestartAndExistingEmptyCatalog(t *testing.T) {
	gateway, requests, discoveries := newEnvModelGateway(t)
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "1")
	t.Setenv("DSHD_LLM_BASE", gateway+"/v1")
	t.Setenv("DSHD_LLM_KEY", "")
	t.Setenv("DSHD_REASONING_EFFORT", "")
	exe := buildPluginGlueDSHD(t)

	t.Setenv("DSHD_LLM_MODEL", "env-model-a")
	homeA := t.TempDir()
	serverA := startPluginGlueServer(t, exe, homeA, t.TempDir())
	clientA := connectPluginGlueServer(t, serverA)
	initialA := modelCatalogResult(t, postPluginGlue(t, clientA, serverA.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	assertEnvCatalogEntry(t, initialA, "env-model-a", 1000000, "xhigh", "max")
	logModelCatalogEvidence(t, "ENV_MODEL_CATALOG_A", map[string]any{"result": initialA})

	t.Setenv("DSHD_LLM_MODEL", "env-model-b")
	homeB := t.TempDir()
	serverB := startPluginGlueServer(t, exe, homeB, t.TempDir())
	clientB := connectPluginGlueServer(t, serverB)
	initialB := modelCatalogResult(t, postPluginGlue(t, clientB, serverB.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	assertEnvCatalogEntry(t, initialB, "env-model-b", 2000000, "low")
	logModelCatalogEvidence(t, "ENV_MODEL_CATALOG_B", map[string]any{"result": initialB})

	sessionA := effortGoalCreateSession(t, clientA, serverA.base)
	sessionB := effortGoalCreateSession(t, clientB, serverB.base)
	postPluginGlue(t, clientA, serverA.base, "session.setEffort", map[string]any{"id": sessionA, "effort": "max"}, http.StatusOK)
	postPluginGlue(t, clientB, serverB.base, "session.setEffort", map[string]any{"id": sessionB, "effort": "low"}, http.StatusOK)
	assertEffortSelection(t, clientA, serverA.base, sessionA, "max")
	assertEffortSelection(t, clientB, serverB.base, sessionB, "low")
	envModelPromptAndAssert(t, clientA, serverA, sessionA, "env A max", requests, "env-model-a", "max")
	envModelPromptAndAssert(t, clientB, serverB, sessionB, "env B low", requests, "env-model-b", "low")
	assertActualEffort(t, clientA, serverA.base, sessionA, "max")
	assertActualEffort(t, clientB, serverB.base, sessionB, "low")

	serverA.stop()
	t.Setenv("DSHD_LLM_MODEL", "env-model-a")
	serverA = startPluginGlueServer(t, exe, homeA, t.TempDir())
	clientA = connectPluginGlueServer(t, serverA)
	assertEffortSelection(t, clientA, serverA.base, sessionA, "max")
	envModelPromptAndAssert(t, clientA, serverA, sessionA, "env A restarted max", requests, "env-model-a", "max")
	for _, home := range []string{homeA, homeB} {
		if _, err := os.Stat(filepath.Join(home, "state", "model-catalog.json")); !os.IsNotExist(err) {
			t.Fatalf("ephemeral catalog became persistent at %s: %v", home, err)
		}
	}

	homeWithEmptyCatalog := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeWithEmptyCatalog, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeWithEmptyCatalog, "state", "model-catalog.json"), []byte(`{"version":1,"revision":7,"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := discoveries.Load()
	serverC := startPluginGlueServer(t, exe, homeWithEmptyCatalog, t.TempDir())
	clientC := connectPluginGlueServer(t, serverC)
	existing := modelCatalogResult(t, postPluginGlue(t, clientC, serverC.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	if existing["revision"] != float64(7) || len(existing["models"].([]any)) != 0 || discoveries.Load() != before {
		t.Fatalf("existing empty catalog triggered fallback: %+v discoveries=%d->%d", existing, before, discoveries.Load())
	}
	logModelCatalogEvidence(t, "ENV_MODEL_CATALOG_EXISTING_EMPTY", map[string]any{"result": existing})
	t.Log("ENV_MODEL_CATALOG_REAL_SUMMARY OK=12 FAIL=0")
}

func TestEnvModelCatalogImplicitDefaultDoesNotDiscover(t *testing.T) {
	gateway, _, discoveries := newEnvModelGateway(t)
	t.Setenv("DSHD_LLM_BASE", gateway+"/v1")
	t.Setenv("DSHD_LLM_KEY", "")
	t.Setenv("DSHD_LLM_MODEL", "")
	t.Setenv("DSH_GO_MODEL", "")
	exe := buildPluginGlueDSHD(t)
	home := t.TempDir()
	server := startPluginGlueServer(t, exe, home, t.TempDir())
	client := connectPluginGlueServer(t, server)
	result := modelCatalogResult(t, postPluginGlue(t, client, server.base, "models.catalog.list", map[string]any{}, http.StatusOK))
	if len(result["models"].([]any)) != 0 || discoveries.Load() != 0 {
		t.Fatalf("implicit default triggered discovery: result=%+v discoveries=%d", result, discoveries.Load())
	}
	if _, err := os.Stat(filepath.Join(home, "state", "model-catalog.json")); !os.IsNotExist(err) {
		t.Fatalf("implicit default created catalog: %v", err)
	}
	t.Log("ENV_MODEL_IMPLICIT_DEFAULT_SUMMARY OK=2 FAIL=0")
}

func TestEnvModelCatalogRealDSHDDefaultLoopEffort(t *testing.T) {
	gateway, requests, _ := newEnvModelGateway(t)
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "")
	t.Setenv("DSHD_LLM_BASE", gateway+"/v1")
	t.Setenv("DSHD_LLM_KEY", "")
	t.Setenv("DSHD_LLM_MODEL", "env-model-a")
	t.Setenv("DSHD_REASONING_EFFORT", "")
	exe := buildPluginGlueDSHD(t)
	server := startPluginGlueServer(t, exe, t.TempDir(), t.TempDir())
	client := connectPluginGlueServer(t, server)
	sessionID := effortGoalCreateSession(t, client, server.base)
	postPluginGlue(t, client, server.base, "session.setEffort", map[string]any{"id": sessionID, "effort": "xhigh"}, http.StatusOK)
	assertEffortSelection(t, client, server.base, sessionID, "xhigh")
	envModelPromptAndAssert(t, client, server, sessionID, "default loop env xhigh", requests, "env-model-a", "xhigh")
	assertActualEffort(t, client, server.base, sessionID, "xhigh")
	t.Log("ENV_MODEL_DEFAULT_LOOP_SUMMARY OK=4 FAIL=0")
}
