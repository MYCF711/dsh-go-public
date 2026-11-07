package transport

// 官方 `commands.*` 的**真实注册表**契约测试。
//
// 这里钉的是官方语义（lib/index.js 的 CommandRuntime）：
//   - 名字解析与 `undefined` 出口；
//   - 生命周期 `command/run` / `command/done` 的顺序与内容；
//   - 结果归一化（success/error、sourceEventSeq）；
//   - 附件准入（策略拒绝是**结果**，形状错是 **400**）；
//   - 没接生命周期口时**响亮**未实现。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// commandRecorder 记下生命周期帧。
type commandRecorder struct {
	mu     sync.Mutex
	events []string
	data   []map[string]any
}

func (r *commandRecorder) hook(sessionID, eventType string, data any) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventType)
	if m, ok := data.(map[string]any); ok {
		r.data = append(r.data, m)
	} else {
		r.data = append(r.data, nil)
	}
	return int64(len(r.events)), nil
}

func (r *commandRecorder) snapshot() ([]string, []map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...), append([]map[string]any(nil), r.data...)
}

func newTestCommandRegistry(t *testing.T, rec *commandRecorder) *OfficialCommandRegistry {
	t.Helper()
	registry := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{
		Lifecycle:     rec.hook,
		InstanceToken: "testtok",
	})
	if _, err := registry.Register(OfficialCommandDefinition{
		Name: "help", Description: "列出命令",
		Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			return OfficialCommandOutcome{Kind: "success", Text: "ok"}, nil
		},
	}); err != nil {
		t.Fatalf("注册 help: %v", err)
	}
	return registry
}

func TestOfficialCommandParse(t *testing.T) {
	cases := []struct {
		line     string
		name     string
		rawInput string
		ok       bool
	}{
		{"/help", "help", "", true},
		{"/help me", "help", " me", true},
		{"/help\tnow", "help", "\tnow", true},
		{"/a-b_c1 x", "a-b_c1", " x", true},
		{"/Help", "", "", false},
		{"/help!", "", "", false},
		{"/", "", "", false},
		{"help", "", "", false},
		{"", "", "", false},
		{"/1help", "", "", false},
	}
	for _, tc := range cases {
		name, raw, ok := parseOfficialCommand(tc.line)
		if ok != tc.ok {
			t.Fatalf("%q 的 ok=%v，期望 %v", tc.line, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if name != tc.name || raw != tc.rawInput {
			t.Fatalf("%q 解析成 (%q,%q)，期望 (%q,%q)", tc.line, name, raw, tc.name, tc.rawInput)
		}
	}
}

func TestOfficialCommandRegistryListSorted(t *testing.T) {
	rec := &commandRecorder{}
	registry := newTestCommandRegistry(t, rec)
	for _, name := range []string{"goal", "feedback"} {
		if _, err := registry.Register(OfficialCommandDefinition{
			Name: name, Description: name + " 描述",
			Input: &OfficialCommandInput{Hint: "hint"},
			Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
				return OfficialCommandOutcome{Kind: "success"}, nil
			},
		}); err != nil {
			t.Fatalf("注册 %s: %v", name, err)
		}
	}
	list := registry.List()
	if len(list) != 3 {
		t.Fatalf("应该列出 3 条命令，实际 %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Fatalf("没有按 name 升序：%v", list)
		}
	}
	for _, cmd := range list {
		if cmd.Name == "goal" {
			if cmd.Input == nil || cmd.Input.Hint != "hint" {
				t.Fatalf("input 没带出来：%+v", cmd)
			}
		}
	}
	// 注销必须**只**删自己那份。
	dispose, err := registry.Register(OfficialCommandDefinition{
		Name: "temp", Description: "临时",
		Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			return OfficialCommandOutcome{Kind: "success"}, nil
		},
	})
	if err != nil {
		t.Fatalf("注册 temp: %v", err)
	}
	dispose()
	if _, ok := registry.Find("temp"); ok {
		t.Fatal("dispose 之后 temp 还在")
	}
	if _, ok := registry.Find("help"); !ok {
		t.Fatal("dispose 把别的命令删掉了")
	}
}

func TestOfficialCommandRegistryRejectsBadDefinitions(t *testing.T) {
	registry := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{Lifecycle: func(string, string, any) (int64, error) { return 0, nil }})
	handler := func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
		return OfficialCommandOutcome{Kind: "success"}, nil
	}
	for name, def := range map[string]OfficialCommandDefinition{
		"空名":     {Name: "", Description: "d", Handler: handler},
		"带斜杠":    {Name: "/help", Description: "d", Handler: handler},
		"大写":     {Name: "Help", Description: "d", Handler: handler},
		"空描述":    {Name: "help", Description: "", Handler: handler},
		"nil 处理": {Name: "help", Description: "d"},
	} {
		if _, err := registry.Register(def); err == nil {
			t.Fatalf("%s 竟然注册成功了", name)
		}
	}
	if _, err := registry.Register(OfficialCommandDefinition{Name: "help", Description: "d", Handler: handler}); err != nil {
		t.Fatalf("合法定义被拒: %v", err)
	}
	if _, err := registry.Register(OfficialCommandDefinition{Name: "help", Description: "d", Handler: handler}); err == nil {
		t.Fatal("重名竟然注册成功了")
	}
}

func TestOfficialCommandExecuteLifecycle(t *testing.T) {
	rec := &commandRecorder{}
	registry := newTestCommandRegistry(t, rec)

	exec, err := registry.Execute(context.Background(), "s1", "/help 参数", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if exec == nil {
		t.Fatal("已注册命令回了 undefined")
	}
	if exec.CommandID != "cmd-testtok-1" {
		t.Fatalf("commandId 形状不对：%q", exec.CommandID)
	}
	if exec.Result.Kind != "success" || exec.Result.Text != "ok" {
		t.Fatalf("结果不对：%+v", exec.Result)
	}
	events, data := rec.snapshot()
	if len(events) != 2 || events[0] != "command/run" || events[1] != "command/done" {
		t.Fatalf("生命周期不对：%v", events)
	}
	if data[0]["commandId"] != exec.CommandID || data[0]["name"] != "help" || data[0]["args"] != " 参数" {
		t.Fatalf("command/run 内容不对：%v", data[0])
	}
	if data[1]["commandId"] != exec.CommandID || data[1]["kind"] != "success" || data[1]["text"] != "ok" {
		t.Fatalf("command/done 内容不对：%v", data[1])
	}
}

func TestOfficialCommandExecuteUnknownIsUndefined(t *testing.T) {
	rec := &commandRecorder{}
	registry := newTestCommandRegistry(t, rec)

	for _, line := range []string{"/nope", "help", "/", "/HELP", "/help!"} {
		exec, err := registry.Execute(context.Background(), "s1", line, nil)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if exec != nil {
			t.Fatalf("%q 应该回 undefined，实际 %+v", line, exec)
		}
	}
	if events, _ := rec.snapshot(); len(events) != 0 {
		t.Fatalf("admission miss 不该记账，实际 %v", events)
	}
}

func TestOfficialCommandExecuteAttachments(t *testing.T) {
	rec := &commandRecorder{}
	registry := newTestCommandRegistry(t, rec)

	// 命令没声明 attachments → 结算成 error **结果**（200）。
	exec, err := registry.Execute(context.Background(), "s1", "/help", []OfficialSubmittedAttachment{
		{Type: "file", ReceiptID: "r1"},
	})
	if err != nil {
		t.Fatalf("不接受附件应该是结果错，不是 transport 错：%v", err)
	}
	if exec == nil || exec.Result.Kind != "error" || !strings.Contains(exec.Result.Text, "does not accept attachments") {
		t.Fatalf("应该结算成 error 结果，实际 %+v", exec)
	}

	// 声明了 attachments 但没有附件存储 → 同样是 error 结果。
	registry2 := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{Lifecycle: rec.hook})
	if _, err := registry2.Register(OfficialCommandDefinition{
		Name: "shot", Description: "带图",
		Input: &OfficialCommandInput{Hint: "图", Attachments: boolPtr(true)},
		Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			return OfficialCommandOutcome{Kind: "success"}, nil
		},
	}); err != nil {
		t.Fatalf("注册 shot: %v", err)
	}
	exec, err = registry2.Execute(context.Background(), "s1", "/shot", []OfficialSubmittedAttachment{
		{Type: "image", MediaType: "image/png", Data: "aGk="},
	})
	if err != nil {
		t.Fatalf("没有附件存储应该是结果错：%v", err)
	}
	if exec == nil || exec.Result.Kind != "error" || !strings.Contains(exec.Result.Text, "no attachment store") {
		t.Fatalf("应该结算成 error 结果，实际 %+v", exec)
	}

	// 声明了 attachments 且**有**附件存储时，形状错（未知 type）→ transport 400。
	registry3 := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{
		Lifecycle:   rec.hook,
		Attachments: stubAttachmentStore{},
	})
	if _, err := registry3.Register(OfficialCommandDefinition{
		Name: "shot", Description: "带图",
		Input: &OfficialCommandInput{Hint: "图", Attachments: boolPtr(true)},
		Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			return OfficialCommandOutcome{Kind: "success"}, nil
		},
	}); err != nil {
		t.Fatalf("注册 shot: %v", err)
	}
	if _, err := registry3.Execute(context.Background(), "s1", "/shot", []OfficialSubmittedAttachment{{Type: "audio"}}); err == nil {
		t.Fatal("未知附件 type 竟然不是 400")
	}
	// image 的 data 不是规范 base64 → 400。
	if _, err := registry3.Execute(context.Background(), "s1", "/shot", []OfficialSubmittedAttachment{
		{Type: "image", MediaType: "image/png", Data: "不是 base64"},
	}); err == nil {
		t.Fatal("非法 base64 竟然不是 400")
	}
	// 合法图片 → handler 拿到真实字节。
	var seen []OfficialCommandAttachment
	registry4 := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{
		Lifecycle:   rec.hook,
		Attachments: stubAttachmentStore{},
	})
	if _, err := registry4.Register(OfficialCommandDefinition{
		Name: "shot", Description: "带图",
		Input: &OfficialCommandInput{Hint: "图", Attachments: boolPtr(true)},
		Handler: func(_ context.Context, inv OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			seen = inv.Attachments
			return OfficialCommandOutcome{Kind: "success", Text: "收到图"}, nil
		},
	}); err != nil {
		t.Fatalf("注册 shot: %v", err)
	}
	exec, err = registry4.Execute(context.Background(), "s1", "/shot", []OfficialSubmittedAttachment{
		{Type: "image", MediaType: "image/png", Data: "aGk=", Name: "a.png"},
	})
	if err != nil || exec == nil || exec.Result.Kind != "success" {
		t.Fatalf("合法图片应该成功：%v %+v", err, exec)
	}
	if len(seen) != 1 || string(seen[0].Bytes) != "hi" || seen[0].Name != "a.png" {
		t.Fatalf("handler 拿到的准入附件不对：%+v", seen)
	}
}

// stubAttachmentStore 是一个空实现（本测试只验证准入形状，不用真存储）。
type stubAttachmentStore struct{}

func (stubAttachmentStore) Put(string) (Attachment, error) { return Attachment{}, nil }

func (stubAttachmentStore) Get(string) (AttachmentContent, error) {
	return AttachmentContent{}, nil
}

func TestOfficialCommandExecuteHandlerFailure(t *testing.T) {
	rec := &commandRecorder{}
	registry := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{Lifecycle: rec.hook})
	if _, err := registry.Register(OfficialCommandDefinition{
		Name: "boom", Description: "炸",
		Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			return OfficialCommandOutcome{}, errors.New("底层坏了")
		},
	}); err != nil {
		t.Fatalf("注册 boom: %v", err)
	}
	if _, err := registry.Execute(context.Background(), "s1", "/boom", nil); err == nil {
		t.Fatal("handler 失败竟然成功了")
	}
	events, data := rec.snapshot()
	if len(events) != 2 || events[1] != "command/done" || data[1]["kind"] != "error" {
		t.Fatalf("handler 失败的生命周期不对：%v %v", events, data)
	}
	if !strings.Contains(data[1]["text"].(string), "底层坏了") {
		t.Fatalf("command/done 没带 handler 的错误：%v", data[1])
	}
}

func TestOfficialCommandExecuteNormalizesResult(t *testing.T) {
	rec := &commandRecorder{}
	registry := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{Lifecycle: rec.hook})
	if _, err := registry.Register(OfficialCommandDefinition{
		Name: "blank", Description: "空错误",
		Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			return OfficialCommandOutcome{Kind: "error", Text: "   "}, nil
		},
	}); err != nil {
		t.Fatalf("注册 blank: %v", err)
	}
	if _, err := registry.Execute(context.Background(), "s1", "/blank", nil); err == nil {
		t.Fatal("空 error text 竟然通过了归一化")
	}

	registry2 := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{Lifecycle: rec.hook})
	if _, err := registry2.Register(OfficialCommandDefinition{
		Name: "weird", Description: "未知 kind",
		Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			return OfficialCommandOutcome{Kind: "pending"}, nil
		},
	}); err != nil {
		t.Fatalf("注册 weird: %v", err)
	}
	if _, err := registry2.Execute(context.Background(), "s1", "/weird", nil); err == nil {
		t.Fatal("未知 result kind 竟然通过了归一化")
	}
}

// 没有生命周期口时 commands.execute 必须**响亮**未实现（不能只执行不记账）。
func TestOfficialCommandExecuteWithoutLifecycleIs501(t *testing.T) {
	registry := NewOfficialCommandRegistry(OfficialCommandRegistryOptions{})
	if _, err := registry.Register(OfficialCommandDefinition{
		Name: "help", Description: "d",
		Handler: func(context.Context, OfficialCommandInvocation) (OfficialCommandOutcome, error) {
			return OfficialCommandOutcome{Kind: "success"}, nil
		},
	}); err != nil {
		t.Fatalf("注册: %v", err)
	}
	if _, err := registry.Execute(context.Background(), "s1", "/help", nil); !errors.Is(err, ErrOfficialNotImplemented) {
		t.Fatalf("应该是未实现，实际 %v", err)
	}
}

// 走真实 /api 出口：commands.list / commands.execute 的形状与四字段错误。
func TestOfficialCommandWire(t *testing.T) {
	rec := &commandRecorder{}
	registry := newTestCommandRegistry(t, rec)
	srv := officialTestServer(t, OfficialAgentAPI{
		CommandsList: func(string) ([]OfficialCommand, error) { return registry.List(), nil },
		CommandsExecute: func(sessionID, line string, attachments []OfficialSubmittedAttachment) (*OfficialCommandExecution, error) {
			return registry.Execute(context.Background(), sessionID, line, attachments)
		},
	})

	code, out := callOfficial(t, srv, "commands.list", map[string]any{"agentId": "s1"})
	if code != http.StatusOK {
		t.Fatalf("commands.list: %d %v", code, out)
	}
	list, ok := out["result"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("commands.list 结果不对：%v", out)
	}

	code, out = callOfficial(t, srv, "commands.execute", map[string]any{"agentId": "s1", "line": "/help"})
	if code != http.StatusOK {
		t.Fatalf("commands.execute: %d %v", code, out)
	}
	exec, ok := out["result"].(map[string]any)
	if !ok || exec["commandId"] == "" {
		t.Fatalf("commands.execute 结果不对：%v", out)
	}

	// 未知名字 → 官方 undefined（本仓空 result 对象）。
	code, out = callOfficial(t, srv, "commands.execute", map[string]any{"agentId": "s1", "line": "/nope"})
	if code != http.StatusOK {
		t.Fatalf("未知名字应该 200：%d %v", code, out)
	}
	if result, ok := out["result"].(map[string]any); !ok || len(result) != 0 {
		t.Fatalf("未知名字应该是空 result 对象：%v", out)
	}

	// 缺 line → 400 四字段。
	code, out = callOfficial(t, srv, "commands.execute", map[string]any{"agentId": "s1"})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
}

func boolPtr(v bool) *bool { return &v }
