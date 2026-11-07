package main

import (
	"encoding/json"
	"testing"

	"dshgo/internal/agent"
	"dshgo/internal/session"
)

// CI 七条第 6 条：工具表 ≤ 6000 token（PLAN.md §15）。
//
// 用本仓自己的估算器（ascii/4 + 非 ASCII 每字 1）而不是真分词器：
// 真分词要么联网、要么引入依赖。估算器与实测的偏差记录在 docs/ci-rules.md，
// **它是代理不是证明**——但比"没人管"强一个数量级。
func TestToolTableTokenBudget(t *testing.T) {
	table := buildToolTableMust(t)
	for i := range table {
		table[i].Parameters = agent.CompactSchema(table[i].Parameters)
	}
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	est := session.EstimateRunes(string(raw))
	t.Logf("工具表 %d 个，%d 字节，估算 %d token（上限 6000）", len(table), len(raw), est)
	if est > 6000 {
		t.Fatalf("工具表估算 %d token，超过 PLAN §15 的 6000；要么砍描述、要么把低频工具合成一个多 action 的",
			est)
	}
}
