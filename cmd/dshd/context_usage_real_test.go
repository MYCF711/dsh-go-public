package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func waitW149NaturalTurn(t *testing.T, client *http.Client, base, sid string, sinceSeq int, turnID string) {
	t.Helper()
	if turnID == "" {
		t.Fatal("session.prompt 未返回 turnId")
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		history := postPluginGlue(t, client, base, "session.history", map[string]any{"id": sid, "sinceSeq": sinceSeq, "limit": 1000}, http.StatusOK)
		frames := history["result"].(map[string]any)["frames"].([]any)
		for _, raw := range frames {
			frame := raw.(map[string]any)
			data, _ := frame["data"].(map[string]any)
			if data["turnId"] != turnID {
				continue
			}
			if frame["type"] == "turn/error" {
				t.Fatalf("turn %s failed: %+v", turnID, frame)
			}
			if frame["type"] == "turn/done" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 turn %s 超时: %+v", turnID, frames)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func promptW149NaturalTurn(t *testing.T, client *http.Client, base, sid, prompt string) {
	t.Helper()
	history := postPluginGlue(t, client, base, "session.history", map[string]any{"id": sid, "sinceSeq": 0, "limit": 1000}, http.StatusOK)["result"].(map[string]any)
	sinceSeq := 0
	if watermark, exists := history["lastSeq"]; exists {
		sinceSeq = int(watermark.(float64))
	}
	result := postPluginGlue(t, client, base, "session.prompt", map[string]any{"id": sid, "text": prompt}, http.StatusOK)["result"].(map[string]any)
	waitW149NaturalTurn(t, client, base, sid, sinceSeq, result["turnId"].(string))
}

func TestW149RealDSHDContextUsageFromNaturalPromptAndRestart(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var request struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
			http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
			return
		}
		var prompt string
		_ = json.Unmarshal(request.Messages[len(request.Messages)-1].Content, &prompt)
		w.Header().Set("Content-Type", "text/event-stream")
		usage := ""
		switch prompt {
		case "first A":
			usage = `,"usage":{"prompt_tokens":70,"completion_tokens":30,"total_tokens":100}`
		case "first B":
			usage = `,"usage":{"prompt_tokens":140,"completion_tokens":60,"total_tokens":200}`
		}
		content, _ := json.Marshal("answer " + prompt)
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]%s}`+"\n\n", content, usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(gateway.Close)
	t.Setenv("DSHD_LLM_BASE", gateway.URL+"/v1")
	t.Setenv("DSHD_LLM_KEY", "")
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "")
	t.Setenv("DSHD_CONTEXT_WINDOW", "")

	exe := buildPluginGlueDSHD(t)
	home := t.TempDir()
	server := startPluginGlueServer(t, exe, home, t.TempDir())
	client := connectPluginGlueServer(t, server)
	create := func() string {
		return postPluginGlue(t, client, server.base, "session.create", map[string]any{}, http.StatusOK)["result"].(map[string]any)["sessionId"].(string)
	}
	sidA, sidB := create(), create()
	for _, item := range []struct {
		sid, prompt string
	}{{sidA, "first A"}, {sidB, "first B"}} {
		promptW149NaturalTurn(t, client, server.base, item.sid, item.prompt)
	}
	readUsage := func(sid string, want float64) map[string]any {
		t.Helper()
		envelope := postPluginGlue(t, client, server.base, "context.usage", map[string]any{"id": sid, "detail": "full"}, http.StatusOK)
		result := envelope["result"].(map[string]any)
		categories := result["categories"].([]any)
		if result["used"] != want || len(categories) != 1 {
			t.Fatalf("sid=%s usage=%+v", sid, result)
		}
		category := categories[0].(map[string]any)
		if category["name"] != "context" || category["tokens"] != want || category["kind"] != "used" {
			t.Fatalf("sid=%s 假分类: %+v", sid, result)
		}
		for _, key := range []string{"sections", "files", "skills", "tools", "max"} {
			if _, exists := result[key]; exists {
				t.Fatalf("没有真实计量却返回 %s: %+v", key, result)
			}
		}
		return result
	}
	beforeA := readUsage(sidA, 100)
	readUsage(sidB, 200)

	server.stop()
	server = startPluginGlueServer(t, exe, home, t.TempDir())
	client = connectPluginGlueServer(t, server)
	afterA := readUsage(sidA, 100)
	if !reflect.DeepEqual(beforeA, afterA) {
		t.Fatalf("restart 后 usage 改变: before=%+v after=%+v", beforeA, afterA)
	}
	readUsage(sidB, 200)

	promptW149NaturalTurn(t, client, server.base, sidA, "second A")
	failure := postPluginGlue(t, client, server.base, "context.usage", map[string]any{"id": sidA, "detail": "full"}, http.StatusInternalServerError)
	if !strings.Contains(fmt.Sprint(failure), "最近 assistant") {
		t.Fatalf("第二轮缺 usage 却回退第一轮: %+v", failure)
	}
	readUsage(sidB, 200)
	t.Log("W149_CONTEXT_REAL_SUMMARY OK=9 FAIL=0")
}
