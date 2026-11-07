package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/spi"
)

func mkSession(t *testing.T, home, cwd, id, userText, assistantText string) string {
	t.Helper()
	l, err := CreateSession(home, cwd, id, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("user/message", UserMessage([]spi.Block{{Type: "text", Text: userText}})); err != nil {
		t.Fatal(err)
	}
	if err := l.Append("assistant/message", AssistantMessageEvent(1, 1,
		[]map[string]any{TextBlock(assistantText)}, "stop", nil)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	return l.Path
}

func TestListSessionsAllProjects(t *testing.T) {
	home := t.TempDir()
	mkSession(t, home, "/tmp/proj-a", "a-1", "项目 A 的第一句", "A 的回复")
	mkSession(t, home, "/tmp/proj-b", "b-1", "项目 B 的第一句", "B 的回复")

	got, err := ListSessions(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应该列出 2 个会话，得到 %d", len(got))
	}
	for _, l := range got {
		if !l.Readable {
			t.Fatalf("这几份都得读得动: %+v", l)
		}
		// 只读首帧之后事件数是未知的（-1）——列会话不再为了数事件把整份日志解一遍。
		// 这条断言原本钉的是"读得动且有内容"，现在改钉"要么不知道，要么真数到了"。
		if l.Events != -1 && l.Events != 2 {
			t.Fatalf("%s 事件数 = %d", l.SessionID, l.Events)
		}
		if l.Preset != "read-only" {
			t.Fatalf("%s 的 preset = %q", l.SessionID, l.Preset)
		}
		if !strings.Contains(l.Summary, "回复") {
			t.Fatalf("摘要应该优先取助手消息，得到 %q", l.Summary)
		}
	}
}

func TestListSessionsFilterByProject(t *testing.T) {
	home := t.TempDir()
	mkSession(t, home, "/tmp/proj-a", "a-1", "A", "A 回复")
	mkSession(t, home, "/tmp/proj-b", "b-1", "B", "B 回复")

	got, err := ListSessions(home, "/tmp/proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "a-1" {
		t.Fatalf("按项目过滤后 = %+v", got)
	}
}

func TestListSessionsEmptyHome(t *testing.T) {
	got, err := ListSessions(t.TempDir(), "")
	if err != nil {
		t.Fatalf("没有 sessions 目录不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("应该是空列表，得到 %d", len(got))
	}
}

func TestListSessionsSummaryFallsBackToUser(t *testing.T) {
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/p", "u-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("user/message", UserMessage([]spi.Block{{Type: "text", Text: "只有用户消息"}})); err != nil {
		t.Fatal(err)
	}
	l.Close()

	got, err := ListSessions(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Summary, "只有用户消息") {
		t.Fatalf("没有助手消息时应回落到用户消息，得到 %+v", got)
	}
}

// 同一个 session id 出现在多个项目目录下（换机器 / 换挂载点会让 projectKey 变）：
// 列表里必须**合并成一条**，并标出有几份。
//
// 实测踩过：不给 cwd 列会话时，同一份会话出现两行同样的 id——
// 面板上看不出区别，按 id 删除又会报「多处命中」。
func TestListSessionsDedupesSameIDAcrossProjects(t *testing.T) {
	home := t.TempDir()
	cwdA := t.TempDir()

	id := NewID()
	lg, err := CreateSession(home, cwdA, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("user/message", map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	path := lg.Path
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	// 复制到另一个项目目录下
	src := filepath.Dir(path)
	dst := filepath.Join(home, "sessions", "--other-project--", filepath.Base(src))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}

	list, err := ListSessions(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应该合并成 1 条，实际 %d 条: %+v", len(list), list)
	}
	if list[0].SessionID != id {
		t.Fatalf("id 不对: %s", list[0].SessionID)
	}
	if list[0].Copies != 2 {
		t.Fatalf("要标出有 2 份，实际 %d", list[0].Copies)
	}
}

// 只有一份时不该冒出 copies 字段——那是噪声。
func TestListSessionsSingleHasNoCopies(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	lg, err := CreateSession(home, cwd, NewID(), "")
	if err != nil {
		t.Fatal(err)
	}
	lg.Close()

	list, err := ListSessions(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Copies != 0 {
		t.Fatalf("单份不该有 Copies: %+v", list)
	}
}
