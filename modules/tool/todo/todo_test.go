package todo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func run(t *testing.T, tool *Tool, raw string) (interface{}, error) {
	t.Helper()
	res, err := tool.Exec(context.Background(), json.RawMessage(raw))
	return res, err
}

// 整份替换：第二次给 2 项，就只剩 2 项（不是追加）。
func TestReplacesWholeList(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(),
		json.RawMessage(`{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"in_progress"},{"content":"c","status":"completed"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(data)
	if d.Total != 3 || d.Counts.InProgress != 1 || d.Counts.Completed != 1 || d.Counts.Pending != 1 {
		t.Fatalf("计数不对: %+v", d)
	}

	res, err = tool.Exec(context.Background(), json.RawMessage(`{"todos":[{"content":"只留这项","status":"in_progress"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	d = res.Data.(data)
	if d.Total != 1 || len(d.Todos) != 1 {
		t.Fatalf("应该整份替换成 1 项，实际 %+v", d)
	}
	tool.mu.Lock()
	cur := len(tool.cur)
	tool.mu.Unlock()
	if cur != 1 {
		t.Fatalf("内部状态也要替换，实际 %d 项", cur)
	}
}

// 清空：显式传 []。
func TestClearWithEmptyArray(t *testing.T) {
	tool := &Tool{}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"todos":[{"content":"a","status":"pending"}]}`)); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"todos":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Data.(data); d.Total != 0 {
		t.Fatalf("应该清空，实际 %+v", d)
	}
}

// 漏 todos 字段 ≠ 清空：必须报错，否则"忘了传"会静默把清单抹掉。
func TestMissingTodosIsError(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("缺 todos 必须报错")
	}
	if !strings.Contains(res.Hint, "清空就传") {
		t.Fatalf("Hint 要说清怎么清空: %q", res.Hint)
	}
}

// 非法 status / 空 content / 超长：都要带 Hint 说下一步。
func TestValidationHints(t *testing.T) {
	tool := &Tool{}
	cases := []struct{ raw, want string }{
		{`{"todos":[{"content":"a","status":"doing"}]}`, "status"},
		{`{"todos":[{"content":"  ","status":"pending"}]}`, "content"},
	}
	for _, c := range cases {
		res, err := tool.Exec(context.Background(), json.RawMessage(c.raw))
		if err == nil {
			t.Fatalf("%s 应该报错", c.raw)
		}
		if !strings.Contains(res.Hint, c.want) {
			t.Fatalf("Hint 要提「%s」，实际 %q", c.want, res.Hint)
		}
	}

	// 超过 50 项
	var big []string
	for i := 0; i < 51; i++ {
		big = append(big, `{"content":"x","status":"pending"}`)
	}
	raw := `{"todos":[` + strings.Join(big, ",") + `]}`
	res, err := tool.Exec(context.Background(), json.RawMessage(raw))
	if err == nil || !strings.Contains(res.Hint, "最多") {
		t.Fatalf("超长要报错并说上限: %v %q", err, res.Hint)
	}
}

// 多项 in_progress 时给一句提醒（不是错误）。
func TestWarnsWhenTooManyInProgress(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"todos":[
		{"content":"a","status":"in_progress"},{"content":"b","status":"in_progress"},
		{"content":"c","status":"pending"},{"content":"d","status":"pending"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Hint, "一次只留一项") {
		t.Fatalf("应提醒一次只做一件: %q", res.Hint)
	}
}
