package main

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

func appendW149ContextEvent(t *testing.T, lg *session.Log, kind string, data any) {
	t.Helper()
	if err := lg.Append(kind, data); err != nil {
		t.Fatal(err)
	}
}

func TestContextUsageFullPreservesOnlyMeasuredSnapshot(t *testing.T) {
	home := t.TempDir()
	const sid = "session-context-full"
	lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	appendW149ContextEvent(t, lg, "system/message", session.SystemMessageData(1, 1, "system text"))
	appendW149ContextEvent(t, lg, "request/context", map[string]any{
		"model": "model-a", "contextWindow": 1000,
		"contextUsage": map[string]any{
			"categories": []any{map[string]any{"name": "messages", "tokens": 100, "kind": "used"}},
			"sections":   []any{map[string]any{"name": "system", "tokens": 40}},
			"files":      []any{map[string]any{"path": "D:/work/AGENTS.md", "tokens": 10}},
			"skills":     []any{},
			"tools":      []any{map[string]any{"name": "search", "server": "docs", "tokens": 20}},
		},
	})
	appendW149ContextEvent(t, lg, "assistant/message", session.AssistantMessageEvent(1, 1,
		[]map[string]any{session.TextBlock("answer")}, "stop",
		map[string]any{"inputTokens": 70, "outputTokens": 30, "totalTokens": 100}))
	// 该记录位于选中 assistant 之后，不能倒灌进它的快照。
	appendW149ContextEvent(t, lg, "request/context", map[string]any{"contextWindow": 9999})
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := newSessionSource(home, "").ContextUsage(sid, "full")
	if err != nil {
		t.Fatal(err)
	}
	if got.Used != 100 || got.Max != 1000 || len(got.Categories) != 1 || got.Categories[0].Name != "messages" || got.Categories[0].Tokens != 100 {
		t.Fatalf("usage=%+v", got)
	}
	if got.Sections == nil || len(*got.Sections) != 1 || (*got.Sections)[0].Name != "system" || (*got.Sections)[0].Tokens != 40 {
		t.Fatalf("sections=%+v", got.Sections)
	}
	if got.Files == nil || (*got.Files)[0].Path != "D:/work/AGENTS.md" || got.Skills == nil || len(*got.Skills) != 0 {
		t.Fatalf("files/skills=%+v/%+v", got.Files, got.Skills)
	}
	if got.Tools == nil || len(*got.Tools) != 1 || (*got.Tools)[0].Server != "docs" || (*got.Tools)[0].Tokens != 20 {
		t.Fatalf("tools=%+v", got.Tools)
	}
}

func TestContextUsageFullDoesNotInventBreakdownOrReuseStaleUsage(t *testing.T) {
	home := t.TempDir()
	const sid = "session-context-aggregate"
	lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	appendW149ContextEvent(t, lg, "system/message", session.SystemMessageData(1, 1, "real system text"))
	appendW149ContextEvent(t, lg, "request/header", map[string]any{"header": map[string]any{"tools": []any{map[string]any{"name": "read"}}}})
	appendW149ContextEvent(t, lg, "assistant/message", session.AssistantMessageEvent(1, 1,
		[]map[string]any{session.TextBlock("answer")}, "stop",
		map[string]any{"inputTokens": 70, "outputTokens": 30, "totalTokens": 100}))
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := newSessionSource(home, "").ContextUsage(sid, "full")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Categories) != 1 || got.Categories[0].Name != "context" || got.Categories[0].Tokens != 100 || got.Max != 0 {
		t.Fatalf("无真实 breakdown 却伪造分类/max: %+v", got)
	}
	if got.Sections != nil || got.Files != nil || got.Skills != nil || got.Tools != nil {
		t.Fatalf("没有持久计量却伪造 details: %+v", got)
	}
}

func TestContextUsageFullRejectsLatestMissingUsageAndChangedSurface(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-usage", true: "changed-surface"}[changed], func(t *testing.T) {
			home := t.TempDir()
			const sid = "session-context-stale"
			lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
			if err != nil {
				t.Fatal(err)
			}
			appendW149ContextEvent(t, lg, "assistant/message", session.AssistantMessageEvent(1, 1,
				[]map[string]any{session.TextBlock("old")}, "stop", map[string]any{"totalTokens": 40}))
			if changed {
				appendW149ContextEvent(t, lg, "user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "new unmeasured input"}}))
			} else {
				appendW149ContextEvent(t, lg, "assistant/message", session.AssistantMessageEvent(2, 1,
					[]map[string]any{session.TextBlock("latest-without-usage")}, "stop", nil))
			}
			if err := lg.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := newSessionSource(home, "").ContextUsage(sid, "full"); err == nil || !strings.Contains(err.Error(), "context.usage") {
				t.Fatalf("陈旧 usage 被当成当前真值: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestContextUsageSnapshotStrictNullAndSafeIntegers(t *testing.T) {
	valid := `{"contextWindow":1000,"contextUsage":{"categories":[{"name":"messages","tokens":1,"kind":"used"}],"sections":[],"files":[],"skills":[],"tools":[]}}`
	if _, err := parseContextUsageSnapshot([]byte(valid)); err != nil {
		t.Fatalf("valid snapshot: %v", err)
	}
	invalid := []string{
		`null`,
		`{"contextWindow":null}`,
		`{"contextWindow":0}`,
		`{"contextWindow":9007199254740992}`,
		`{"contextUsage":null}`,
		`{"contextUsage":{"sections":null}}`,
		`{"contextUsage":{"sections":[{"name":"system"}]}}`,
		`{"contextUsage":{"sections":[{"name":"system","tokens":null}]}}`,
		`{"contextUsage":{"sections":[{"name":"system","tokens":-1}]}}`,
		`{"contextUsage":{"sections":[{"name":"system","tokens":9007199254740992}]}}`,
		`{"contextUsage":{"categories":[{"name":"messages","tokens":1}]}}`,
		`{"contextUsage":{"tools":[{"name":"read","tokens":1,"server":""}]}}`,
		`{"contextUsage":{"tools":[{"name":"read","tokens":1,"server":null}]}}`,
	}
	for _, raw := range invalid {
		if _, err := parseContextUsageSnapshot([]byte(raw)); err == nil {
			t.Errorf("非法快照被接受: %s", raw)
		}
	}
	if _, err := parseContextUsageSnapshot([]byte(`{"contextWindow":9007199254740991,"contextUsage":{"sections":[{"name":"system","tokens":9007199254740991}]}}`)); err != nil {
		t.Fatalf("最大 JS 安全整数应通过: %v", err)
	}
}

func TestContextUsageRejectsUnsafeTotalAndAcceptsMeasuredZero(t *testing.T) {
	for _, test := range []struct {
		usage map[string]any
		want  int
		bad   bool
	}{{map[string]any{"totalTokens": 0}, 0, false}, {map[string]any{"totalTokens": int64(9007199254740992)}, 0, true}} {
		lg := &v4.Log{Events: []v4.Event{{Type: "assistant/message", Data: []byte(`{"usage":` + mustW149JSON(t, test.usage) + `}`)}}}
		got, err := newSessionSource(t.TempDir(), "").contextUsageFromV4(lg)
		if test.bad && err == nil {
			t.Fatalf("unsafe total 被接受: %+v", got)
		}
		if !test.bad && (err != nil || got.Used != test.want || len(got.Categories) != 1 || got.Categories[0].Tokens != test.want) {
			t.Fatalf("measured zero: got=%+v err=%v", got, err)
		}
	}
}

func mustW149JSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
