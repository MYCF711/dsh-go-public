package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dshgo/internal/spi"
)

type fakeRunner struct {
	res spi.SubagentResult
	err error
	got spi.SubagentRequest
}

func (f *fakeRunner) RunSub(ctx context.Context, req spi.SubagentRequest) (spi.SubagentResult, error) {
	f.got = req
	return f.res, f.err
}

// 正常：只要结果，不要中间步骤；Data 带步数与字数。
func TestReturnsOnlyResult(t *testing.T) {
	svc := &fakeRunner{res: spi.SubagentResult{Text: "结论：42", Steps: 4}}
	tool := &Tool{svc: svc}
	res, err := tool.Exec(context.Background(),
		json.RawMessage(`{"description":"查一件事","prompt":"自包含的任务描述"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "结论：42" {
		t.Fatalf("正文要是子代理的结论: %q", res.Text)
	}
	if d := res.Data.(data); d.Steps != 4 || d.Description != "查一件事" {
		t.Fatalf("Data 不对: %+v", d)
	}
	if svc.got.Prompt != "自包含的任务描述" {
		t.Fatalf("prompt 没传下去: %+v", svc.got)
	}
}

// 没装运行器：明确报错，不假装"派了"。
func TestNoRunnerIsLoud(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"description":"x","prompt":"y"}`))
	if err == nil {
		t.Fatal("没有运行器必须报错")
	}
	if !strings.Contains(res.Hint, "bash") {
		t.Fatalf("Hint 要给退路: %q", res.Hint)
	}
}

// 空 prompt / 空 description：都要说清。
func TestValidation(t *testing.T) {
	tool := &Tool{svc: &fakeRunner{}}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"description":"x","prompt":"  "}`)); err == nil {
		t.Fatal("空 prompt 要报错")
	}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"description":"","prompt":"y"}`))
	if err == nil || !strings.Contains(res.Hint, "3-5 个词") {
		t.Fatalf("空 description 要报错并说格式: %v %q", err, res.Hint)
	}
}

// requestGolden 是"模型没给 continuable"时宿主收到的请求的**逐字节**形状。
//
// 为什么钉死到这个程度：`continuable` 会让宿主真的多写一条 durable 子会话，
// 而这条子会话会出现在 session.list 里、会被 subagents.prompt 认领。所以
// "省略时什么也没多传"不能靠读代码确认，必须有断言——任何顺手塞进去的字段
// （哪怕是个 false 之外的零值变化）都会在这里红，而不是在真机上表现成
// "子代理莫名其妙可续问"。
const requestGolden = `{"Prompt":"自包含的任务描述","Description":"查一件事",` +
	`"Cwd":"","InheritSession":"","Inherit":"",` +
	`"Continuable":false,"ChildSessionID":"","ParentSessionID":""}`

// TestContinuableOmittedKeepsRequestShapeByteForByte 钉住默认关闭。
//
// 两个方向一起钉：
//  1. 结构体逐字段（reflect.DeepEqual）——只有 description/prompt 有值；
//  2. JSON 逐字节（requestGolden）——连字段顺序和零值都在契约里。
func TestContinuableOmittedKeepsRequestShapeByteForByte(t *testing.T) {
	svc := &fakeRunner{res: spi.SubagentResult{Text: "结论：42", Steps: 4}}
	tool := &Tool{svc: svc}
	if _, err := tool.Exec(context.Background(),
		json.RawMessage(`{"description":"查一件事","prompt":"自包含的任务描述"}`)); err != nil {
		t.Fatal(err)
	}
	want := spi.SubagentRequest{Prompt: "自包含的任务描述", Description: "查一件事"}
	if !reflect.DeepEqual(svc.got, want) {
		t.Fatalf("不传 continuable 时请求形状变了（R4 契约）：\n got=%#v\nwant=%#v", svc.got, want)
	}
	raw, err := json.Marshal(svc.got)
	if err != nil {
		t.Fatalf("编请求: %v", err)
	}
	if string(raw) != requestGolden {
		t.Fatalf("不传 continuable 时请求不再逐字节等价：\n got=%s\nwant=%s", raw, requestGolden)
	}
}

// TestContinuablePassesThrough 钉住透传：true 要原样到达宿主。
//
// 这条是"模型发得起可续问委派"的唯一证据面——宿主侧（subRunner.RunSub →
// durable spawn）已有自己的测试，工具层不透传的话那些实现永远收不到 true。
func TestContinuablePassesThrough(t *testing.T) {
	svc := &fakeRunner{res: spi.SubagentResult{Text: "好", Steps: 1}}
	tool := &Tool{svc: svc}
	if _, err := tool.Exec(context.Background(),
		json.RawMessage(`{"description":"查一件事","prompt":"自包含的任务描述","continuable":true}`)); err != nil {
		t.Fatal(err)
	}
	if !svc.got.Continuable {
		t.Fatalf("continuable 没有透传到宿主: %#v", svc.got)
	}
	// 显式 false 与省略同形：不能因为"给了这个键"就变成另一种语义。
	if _, err := tool.Exec(context.Background(),
		json.RawMessage(`{"description":"查一件事","prompt":"自包含的任务描述","continuable":false}`)); err != nil {
		t.Fatal(err)
	}
	if svc.got.Continuable {
		t.Fatalf("显式 false 被改成了 true: %#v", svc.got)
	}
	raw, err := json.Marshal(svc.got)
	if err != nil {
		t.Fatalf("编请求: %v", err)
	}
	if string(raw) != requestGolden {
		t.Fatalf("显式 false 的请求形状与省略不同：\n got=%s\nwant=%s", raw, requestGolden)
	}
}

// TestSchemaExposesContinuableAsOptionalBoolean 钉住"模型看得见这个开关"。
//
// 只有真的出现在 schema 里，模型才发得出来；而它一旦进了 required，
// 老的调用方（不给这个键）就会被参数校验打回——那正是"不传时形状不变"的反面。
func TestSchemaExposesContinuableAsOptionalBoolean(t *testing.T) {
	var s struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}
	prop, ok := s.Properties["continuable"]
	if !ok {
		t.Fatal("schema 里没有 continuable：模型永远发起不了可续问委派")
	}
	if prop.Type != "boolean" {
		t.Fatalf("continuable 类型 = %q，期望 boolean", prop.Type)
	}
	for _, name := range s.Required {
		if name == "continuable" {
			t.Fatal("continuable 不能是必填：老调用方不传它，必须仍然合法")
		}
	}
}

// 子代理失败：错误要传上来，不能吞掉说"完成"。
func TestChildFailurePropagates(t *testing.T) {
	tool := &Tool{svc: &fakeRunner{err: errors.New("子代理没有产出正文")}}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"description":"x","prompt":"y"}`))
	if err == nil {
		t.Fatal("子代理失败必须报错")
	}
	if !strings.Contains(res.Hint, "自己直接做") {
		t.Fatalf("Hint 要给退路: %q", res.Hint)
	}
}
