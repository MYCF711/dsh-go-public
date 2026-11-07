package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

// buildForkSrc 造一个带 system/user/assistant/tool 的源会话。
func buildForkSrc(t *testing.T, home string) *v4.Log {
	t.Helper()
	l, err := CreateSession(home, "/tmp/src", "src-1", "standard")
	if err != nil {
		t.Fatal(err)
	}
	must := func(ev string, d any) {
		t.Helper()
		if err := l.Append(ev, d); err != nil {
			t.Fatal(err)
		}
	}
	must("system/message", SystemMessageData(1, 1, "你是 dsh-go"))
	must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "数文件"}}))
	must("assistant/message", AssistantMessageEvent(1, 1,
		[]map[string]any{ToolCallBlock("c1", "glob", `{"pattern":"*.go"}`)}, "tool_calls", nil))
	must("tool/call", ToolCallEvent(1, 1, "c1", "glob", `{"pattern":"*.go"}`))
	must("tool/result", ToolResultEvent(1, 1, "c1", []spi.Block{{Type: "text", Text: "68 个"}}, false, "/tmp/src", []int64{2}))
	must("assistant/message", AssistantMessageEvent(1, 1, []map[string]any{TextBlock("一共 68 个")}, "stop", nil))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func TestForkInheritsSurface(t *testing.T) {
	home := t.TempDir()
	src := buildForkSrc(t, home)

	forked, err := Fork(src, home, "/tmp/forked", "fork-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := forked.Close(); err != nil {
		t.Fatal(err)
	}

	back, err := v4.ReadFile(forked.Path)
	if err != nil {
		t.Fatalf("fork 产物读不回来: %v", err)
	}
	if back.Header.ID != "fork-1" {
		t.Fatalf("新会话 id = %q", back.Header.ID)
	}
	if back.Header.ParentSession != "src-1" {
		t.Fatalf("parentSession = %q，期望 src-1", back.Header.ParentSession)
	}
	if back.Header.Cwd != "/tmp/forked" {
		t.Fatalf("cwd = %q", back.Header.Cwd)
	}

	want := Surface(src)
	got := Surface(back)
	if len(got) != len(want) {
		t.Fatalf("surface 节点数 %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Message.Role != want[i].Message.Role {
			t.Fatalf("第 %d 个节点角色 %q，期望 %q", i, got[i].Message.Role, want[i].Message.Role)
		}
		if got[i].Message.Content != want[i].Message.Content {
			t.Fatalf("第 %d 个节点内容 %q，期望 %q", i, got[i].Message.Content, want[i].Message.Content)
		}
	}
	// 工具调用也要跟过去，否则 assistant 消息会指向不存在的 call。
	if len(got[2].Message.ToolCalls) != 1 || got[2].Message.ToolCalls[0].Function.Name != "glob" {
		t.Fatalf("工具调用没继承: %+v", got[2].Message.ToolCalls)
	}
}

func TestForkDoesNotTouchSource(t *testing.T) {
	home := t.TempDir()
	src := buildForkSrc(t, home)
	before, err := os.ReadFile(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	forked, err := Fork(src, home, "/tmp/f2", "fork-2")
	if err != nil {
		t.Fatal(err)
	}
	forked.Close()

	after, err := os.ReadFile(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("源文件被改了：%d → %d 字节", len(before), len(after))
	}
}

func TestForkCompactedSourceKeepsCheckpoint(t *testing.T) {
	home := t.TempDir()
	src := buildForkSrc(t, home)
	l, err := OpenSessionAt(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Compact(l, src.Events, Span{Start: 1, End: 2},
		[]spi.Block{{Type: "text", Text: "（摘要）用户想数文件"}}, CompactionMeta{}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	compacted, err := v4.ReadFile(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	forked, err := Fork(compacted, home, "/tmp/f3", "fork-3")
	if err != nil {
		t.Fatal(err)
	}
	forked.Close()

	back, err := v4.ReadFile(forked.Path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range Surface(back) {
		if strings.Contains(n.Message.Content, "摘要") && n.Source == "compact-checkpoint" {
			found = true
		}
	}
	if !found {
		t.Fatal("源会话的压缩摘要没有以 compact-checkpoint 身份跟到新会话")
	}
}

func TestForkRejectsBadIDs(t *testing.T) {
	home := t.TempDir()
	src := buildForkSrc(t, home)
	if _, err := Fork(src, home, "/tmp/x", ""); err == nil {
		t.Fatal("空 id 必须报错")
	}
	if _, err := Fork(src, home, "/tmp/x", "src-1"); err == nil {
		t.Fatal("与源会话同 id 必须报错")
	}
}

func TestForkWritesToExpectedPath(t *testing.T) {
	home := t.TempDir()
	src := buildForkSrc(t, home)
	forked, err := Fork(src, home, "/tmp/p", "fork-path")
	if err != nil {
		t.Fatal(err)
	}
	forked.Close()
	want := filepath.Join(home, "sessions", "--tmp-p--", "fork-path", "session.v4.jsonl.zstd")
	got := filepath.ToSlash(forked.Path)
	if got != filepath.ToSlash(want) {
		t.Fatalf("路径 = %s，期望 %s", got, want)
	}
}

// TestForkInheritsAgentPreset 锁死一条安全规则：
// 子会话必须继承父会话的模式，否则从 read-only fork 出来会变成 default（权限变大）。
func TestForkInheritsAgentPreset(t *testing.T) {
	home := t.TempDir()
	src := buildForkSrc(t, home)
	src.Header.AgentPreset = "read-only"

	forked, err := Fork(src, home, "/tmp/f-mode", "fork-mode")
	if err != nil {
		t.Fatal(err)
	}
	forked.Close()

	back, err := v4.ReadFile(forked.Path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Header.AgentPreset != "read-only" {
		t.Fatalf("子会话 agentPreset = %q，期望 read-only", back.Header.AgentPreset)
	}
	if back.Header.ParentSession != "src-1" {
		t.Fatalf("parentSession = %q", back.Header.ParentSession)
	}
}
