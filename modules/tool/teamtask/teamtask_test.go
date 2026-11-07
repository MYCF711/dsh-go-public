package teamtask

import (
	"context"
	"encoding/json"
	"testing"

	"dshgo/internal/spi"
)

func mk(dir string, kind string) *Tool { return &Tool{b: &board{dir: dir}, kind: kind} }

// claim 不给 owner 时用会话 id 认领：身份在 ctx 里，不必再问一遍。
//
// 这条测试的由来：压缩工具表后（参数说明被去掉），模型 claim 时漏填 owner，
// 被工具拒掉，白白多跑两轮。让工具有合理默认，比把要求写在说明里可靠。
func TestClaimDefaultsToSessionID(t *testing.T) {
	dir := t.TempDir()
	ctx := spi.WithSession(context.Background(), "session-abc")
	res, err := mk(dir, kindCreate).Exec(ctx, json.RawMessage(`{"subject":"X","description":"Y"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := taskID(t, res)
	if _, err := mk(dir, kindUpdate).Exec(ctx, json.RawMessage(`{"task_id":"`+id+`","action":"claim"}`)); err != nil {
		t.Fatalf("claim 不带 owner 应当认领会话 id: %v", err)
	}
	got, _ := mk(dir, kindGet).Exec(ctx, json.RawMessage(`{"task_id":"`+id+`"}`))
	// Data 里放的是结构体指针，走一遍 JSON 再断言，别依赖具体类型。
	raw, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Task struct {
			Owner string `json:"owner"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Task.Owner != "session-abc" {
		t.Fatalf("owner = %q，期望 session-abc", out.Task.Owner)
	}
}

// 既没有 owner 也没有会话 id 时，仍然要拒绝并说清怎么办。
func TestClaimWithoutOwnerOrSessionRejected(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	res, err := mk(dir, kindCreate).Exec(ctx, json.RawMessage(`{"subject":"X","description":"Y"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mk(dir, kindUpdate).Exec(ctx, json.RawMessage(`{"task_id":"`+taskID(t, res)+`","action":"claim"}`)); err == nil {
		t.Fatal("没有身份来源时必须拒绝")
	}
}

func taskID(t *testing.T, res spi.Result) string {
	t.Helper()
	m, _ := res.Data.(map[string]any)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("拿不到任务 id: %#v", res.Data)
	}
	return id
}
