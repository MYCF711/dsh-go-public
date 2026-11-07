package main

import (
	"encoding/json"
	"testing"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// session.load 的 effort 只能来自这条会话自己的最新配置记录。
// 测试刻意只按线上 JSON 断言：旧实现没有 Effort 字段时应当行为红，而不是编译红。
func TestLoadEffortUsesLatestSessionRecord(t *testing.T) {
	t.Setenv("DSHD_REASONING_EFFORT", "max") // 进程默认不得冒充会话真值。
	home := t.TempDir()
	cwd := t.TempDir()
	const sid = "session-effort-latest"
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		events := []struct {
			typ  string
			data any
		}{
			{"model/selection", map[string]any{"provider": "magpie", "model": "model-a", "reasoningEffort": "high"}},
			{"request/header", map[string]any{"header": map[string]any{"config": map[string]any{
				"provider": "magpie", "model": "model-a", "reasoningEffort": "max",
			}}, "reason": "initial"}},
			{"request/context", map[string]any{"provider": "magpie", "model": "model-a", "contextWindow": 1000000}},
			{"model/selection", map[string]any{"provider": "magpie", "model": "model-a", "reasoningEffort": "xhigh"}},
		}
		for _, ev := range events {
			if err := lg.Append(ev.typ, ev.data); err != nil {
				return err
			}
		}
		return nil
	})

	wire := loadWireForEffortTest(t, home, cwd, sid)
	if got := wire["effort"]; got != "xhigh" {
		t.Fatalf("session.load effort = %#v，期望最新会话记录 xhigh；线上=%s", got, mustJSONForEffortTest(wire))
	}
	if got := wire["model"]; got != "model-a" {
		t.Fatalf("原 model 投影被改变：%#v", got)
	}
}

func TestLoadEffortOldAndUnknownRecordsStayUnknown(t *testing.T) {
	t.Setenv("DSHD_REASONING_EFFORT", "max")
	for _, tc := range []struct {
		name   string
		events []struct {
			typ  string
			data any
		}
	}{
		{name: "旧v4只有request/context没有effort", events: []struct {
			typ  string
			data any
		}{{"request/context", map[string]any{"provider": "magpie", "model": "model-old"}}}},
		{name: "旧选择没有effort字段", events: []struct {
			typ  string
			data any
		}{{"model/selection", map[string]any{"provider": "magpie", "model": "model-old"}}}},
		{name: "最新未知枚举不得回退旧max", events: []struct {
			typ  string
			data any
		}{
			{"request/header", map[string]any{"header": map[string]any{"config": map[string]any{"reasoningEffort": "max"}}}},
			{"model/selection", map[string]any{"provider": "magpie", "model": "model-x", "reasoningEffort": "medium"}},
		}},
		{name: "空值不是off也不得回退", events: []struct {
			typ  string
			data any
		}{
			{"model/selection", map[string]any{"reasoningEffort": "high"}},
			{"request/header", map[string]any{"header": map[string]any{"config": map[string]any{"reasoningEffort": ""}}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cwd := t.TempDir()
			const sid = "session-effort-unknown"
			seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
				for _, ev := range tc.events {
					if err := lg.Append(ev.typ, ev.data); err != nil {
						return err
					}
				}
				return nil
			})
			wire := loadWireForEffortTest(t, home, cwd, sid)
			if got, exists := wire["effort"]; exists {
				t.Fatalf("无记录/未知枚举必须省略 effort 让界面显示 ?，实际 %#v；线上=%s", got, mustJSONForEffortTest(wire))
			}
		})
	}
}

func TestLoadEffortKnownEnumProjection(t *testing.T) {
	for _, effort := range []string{"off", "low", "high", "xhigh", "max"} {
		t.Run(effort, func(t *testing.T) {
			home := t.TempDir()
			cwd := t.TempDir()
			const sid = "session-effort-enum"
			seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
				return lg.Append("request/header", map[string]any{"header": map[string]any{
					"config": map[string]any{"reasoningEffort": effort},
				}})
			})
			wire := loadWireForEffortTest(t, home, cwd, sid)
			if got := wire["effort"]; got != effort {
				t.Fatalf("已知枚举 %q 投影为 %#v", effort, got)
			}
		})
	}
}

func loadWireForEffortTest(t *testing.T, home, cwd, sid string) map[string]any {
	t.Helper()
	res, err := transport.LoadSession(newSessionSource(home, cwd), sid, 10)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func mustJSONForEffortTest(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
