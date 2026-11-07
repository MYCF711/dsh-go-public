package session

import (
	"encoding/json"
	"testing"

	"dshgo/internal/spi"
)

func TestProjectResultOrder(t *testing.T) {
	r := spi.Result{
		Summary:   "读到了 3 行",
		Data:      map[string]any{"total": 3},
		Truncated: &spi.Truncation{Kept: 10, Total: 99, Pointer: "/tmp/x.json"},
		Hint:      "继续用 offset=4",
	}
	blocks := ProjectResult(r)
	if len(blocks) != 4 {
		t.Fatalf("块数 = %d，期望 4（摘要/数据/截断/提示）", len(blocks))
	}
	if blocks[0].Text != "读到了 3 行" {
		t.Fatalf("块 0 = %q", blocks[0].Text)
	}
	if blocks[1].Text != `{"total":3}` {
		t.Fatalf("块 1 = %q，期望紧凑 JSON", blocks[1].Text)
	}
	if blocks[3].Text != "提示：继续用 offset=4" {
		t.Fatalf("块 3 = %q", blocks[3].Text)
	}
}

func TestProjectResultIsDeterministic(t *testing.T) {
	r := spi.Result{Summary: "s", Data: map[string]any{"b": 2, "a": 1}}
	a := Flatten(ProjectResult(r))
	b := Flatten(ProjectResult(r))
	if a != b {
		t.Fatalf("同一结果投影不稳定:\n%q\n%q", a, b)
	}
}

func TestProjectResultEmpty(t *testing.T) {
	blocks := ProjectResult(spi.Result{})
	if len(blocks) != 1 || blocks[0].Text != "（无输出）" {
		t.Fatalf("空结果应有占位块，得到 %+v", blocks)
	}
}

func TestProjectResultSkipsEmptyParts(t *testing.T) {
	blocks := ProjectResult(spi.Result{Data: json.RawMessage(`{"k":1}`)})
	if len(blocks) != 1 {
		t.Fatalf("只有数据时不该多出块，得到 %+v", blocks)
	}
	if blocks[0].Text != `{"k":1}` {
		t.Fatalf("块 = %q", blocks[0].Text)
	}
}
