package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 打标记：写进 agents.json，而且**不动别的字段**。
//
// 这条是核心——中断只是加一个时间戳，如果实现里重新构造 agent 记录，
// 会把 description/context 这些悄悄清空，而那种丢失在列表里看不出来。
func TestInterruptMarksAgentAndKeepsOtherFields(t *testing.T) {
	dir := t.TempDir()
	seed := &spawnFile{Agents: []spawnAgent{{
		Name: "alice", Description: "查资料", Context: "fork", CreatedAt: "2026-11-03T10:00:00+08:00",
	}}}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, spawnFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &interruptTool{dir: dir}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"target":"alice"}`))
	if err != nil {
		t.Fatalf("标记失败：%v", err)
	}
	if res.Summary == "" || res.Data == nil {
		t.Fatalf("要有摘要与结构化数据：%+v", res)
	}
	// 正文必须说清"这是标记，不是杀进程"——不然模型会以为队友真的停了。
	if !strings.Contains(res.Text, "不是进程") && !strings.Contains(res.Text, "标记") {
		t.Fatalf("正文要说明这是标记：%s", res.Text)
	}

	f, err := loadSpawnFile(filepath.Join(dir, spawnFileName))
	if err != nil {
		t.Fatal(err)
	}
	a := f.find("alice")
	if a == nil {
		t.Fatal("alice 不见了")
	}
	if a.InterruptedAt == "" {
		t.Fatal("没写 interrupted_at")
	}
	if a.Description != "查资料" || a.Context != "fork" || a.CreatedAt == "" {
		t.Fatalf("别的字段被改坏了：%+v", a)
	}
}

// 不存在的队友：报错 + Hint，且**不许顺手建一个**。
func TestInterruptUnknownAgentDoesNotCreateIt(t *testing.T) {
	dir := t.TempDir()
	tool := &interruptTool{dir: dir}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"target":"nobody"}`))
	if err == nil {
		t.Fatal("不存在的队友应当报错")
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint")
	}
	if _, statErr := os.Stat(filepath.Join(dir, spawnFileName)); statErr == nil {
		t.Fatal("不该因为一次失败的 interrupt 建出 agents.json")
	}
}

// 缺参数：报错 + Hint。
func TestInterruptRequiresName(t *testing.T) {
	res, err := (&interruptTool{dir: t.TempDir()}).Exec(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("缺 name 应当报错")
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint")
	}
}

// Caps：改 agents.json 就是写盘。
func TestInterruptCapsDeclareWrites(t *testing.T) {
	if !(&interruptTool{}).Caps().Writes {
		t.Fatal("interrupt 会写 agents.json，Caps.Writes 必须为真")
	}
}

func TestInterruptDescriptionLength(t *testing.T) {
	if n := len([]rune((&interruptTool{}).Description())); n > 120 {
		t.Fatalf("Description %d 字，超过 120", n)
	}
}
