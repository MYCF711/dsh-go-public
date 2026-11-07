package transport

import (
	"encoding/json"
	"testing"
)

// 非 TailSourcer 的旧 Source 路径也要带 effort；否则性能优化分支不同、线上字段就不同。
type loadEffortSource struct{ effort string }

func (s loadEffortSource) Messages(string) ([]Message, error) { return []Message{}, nil }
func (s loadEffortSource) Effort(string) (string, error)      { return s.effort, nil }

func TestLoadSessionWithoutTailCarriesOptionalEffort(t *testing.T) {
	res, err := LoadSession(loadEffortSource{effort: "high"}, "session-plain", 10)
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
	if got := wire["effort"]; got != "high" {
		t.Fatalf("普通 LoadSession 路径 effort=%#v，线上=%s", got, raw)
	}
	if got := len(SessionMethods()); got != 25 {
		t.Fatalf("additive effort 不得改变 canonical 25，实际 %d", got)
	}
}
