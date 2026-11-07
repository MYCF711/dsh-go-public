package ask

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 核心契约：必须让循环停下来等人。不设 Stop 就是自问自答。
func TestSetsStop(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(
		`{"questions":[{"id":"q1","header":"选方向","question":"走哪条？",
		"options":[{"label":"A（推荐）","description":"快"},{"label":"B"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop {
		t.Fatal("ask_user_question 必须设 Stop=true")
	}
	for _, want := range []string{"走哪条？", "q1", "1) A（推荐）", "【选方向】"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("正文要含 %q: %q", want, res.Text)
		}
	}
	d := res.Data.(data)
	if !d.AwaitingAnswer || d.Count != 1 || d.Questions[0].ID != "q1" {
		t.Fatalf("Data 不对: %+v", d)
	}
	if !strings.Contains(res.Hint, "不要自己编答案") {
		t.Fatalf("Hint 要提醒不要自答: %q", res.Hint)
	}
}

// 一次问多个；没有选项也可以。
func TestMultipleQuestionsWithoutOptions(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(
		`{"questions":[{"id":"q1","question":"项目叫什么？"},{"id":"q2","question":"放哪个盘？"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(data)
	if d.Count != 2 {
		t.Fatalf("应该两个问题: %+v", d)
	}
	if !strings.Contains(res.Summary, "2 个问题") {
		t.Fatalf("摘要要说几个: %q", res.Summary)
	}
}

// 坏输入都要带 Hint：缺 id、id 重复、空问题、只有一个选项、太多问题。
func TestValidationHints(t *testing.T) {
	tool := &Tool{}
	cases := []struct{ raw, want string }{
		{`{"questions":[{"question":"q"}]}`, "没有 id"},
		{`{"questions":[{"id":"a","question":"1"},{"id":"a","question":"2"}]}`, "重复"},
		{`{"questions":[{"id":"a","question":"  "}]}`, "没有 question"},
		{`{"questions":[{"id":"a","question":"q","options":[{"label":"只一个"}]}]}`, "只有一个选项"},
		{`{"questions":[]}`, "空的"},
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

	// 超过 4 个问题
	q := `{"id":"q%d","question":"x"}`
	var parts []string
	for i := 0; i < 5; i++ {
		parts = append(parts, strings.Replace(q, "%d", string(rune('0'+i)), 1))
	}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"questions":[`+strings.Join(parts, ",")+`]}`))
	if err == nil || !strings.Contains(res.Hint, "最多") {
		t.Fatalf("问题过多要报错: %v %q", err, res.Hint)
	}
}
