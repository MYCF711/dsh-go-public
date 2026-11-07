package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

func TestHistoryFromWrittenSession(t *testing.T) {
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/h", "s-hist", "standard")
	if err != nil {
		t.Fatal(err)
	}
	must := func(ev string, d any) {
		t.Helper()
		if err := l.Append(ev, d); err != nil {
			t.Fatal(err)
		}
	}
	must("turn/start", TurnStartData(1))
	must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "数一下 .go 文件"}}))
	must("assistant/message", AssistantMessageEvent(1, 1,
		[]map[string]any{ToolCallBlock("c1", "glob", `{"pattern":"**/*.go"}`)}, "tool_calls", nil))
	must("tool/call", ToolCallEvent(1, 1, "c1", "glob", `{"pattern":"**/*.go"}`))
	must("tool/result", ToolResultEvent(1, 1, "c1", []spi.Block{{Type: "text", Text: "68 个文件"}}, false, "/tmp/h", []int64{2}))
	must("assistant/message", AssistantMessageEvent(1, 1,
		[]map[string]any{TextBlock("一共 68 个")}, "stop", nil))
	must("turn/end", TurnEndData(1, "completed", ""))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	hist := History(back)
	if len(hist) != 4 {
		t.Fatalf("历史条数 = %d，期望 4（user/assistant/tool/assistant）：%+v", len(hist), hist)
	}
	if hist[0].Role != "user" || hist[0].Content != "数一下 .go 文件" {
		t.Fatalf("第 0 条 = %+v", hist[0])
	}
	if hist[1].Role != "assistant" || len(hist[1].ToolCalls) != 1 || hist[1].ToolCalls[0].Function.Name != "glob" {
		t.Fatalf("第 1 条 = %+v", hist[1])
	}
	if hist[2].Role != "tool" || hist[2].ToolCallID != "c1" || hist[2].Content != "68 个文件" {
		t.Fatalf("第 2 条 = %+v", hist[2])
	}
	if hist[3].Content != "一共 68 个" {
		t.Fatalf("第 3 条 = %+v", hist[3])
	}
}

func TestOpenSessionAppendsToSameFile(t *testing.T) {
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/h", "s-open", "standard")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("turn/start", TurnStartData(1)); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := OpenSession(home, "/tmp/h", "s-open")
	if err != nil {
		t.Fatal(err)
	}
	if l2.Seq() != 1 {
		t.Fatalf("续写的起始 seq = %d，期望 1", l2.Seq())
	}
	if err := l2.Append("turn/end", TurnEndData(1, "completed", "")); err != nil {
		t.Fatal(err)
	}
	l2.Close()

	back, err := v4.ReadFile(l2.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Events) != 2 {
		t.Fatalf("事件数 = %d，期望 2", len(back.Events))
	}
	if back.Events[1].Seq != 1 {
		t.Fatalf("追加事件的 seq = %d，期望 1", back.Events[1].Seq)
	}
}

func TestOpenSessionRejectsMissing(t *testing.T) {
	if _, err := OpenSession(t.TempDir(), "/tmp/h", "nope"); err == nil {
		t.Fatal("不存在的会话必须报错")
	}
}

// TestHistoryRealCorpus 拿真实 DSH 会话派生历史：能不能读回"人话"。
func TestHistoryRealCorpus(t *testing.T) {
	root := os.Getenv("DSH_HOME")
	if root == "" {
		root = `D:\DeepSeek Harness\home`
	}
	sessions := filepath.Join(root, "sessions")
	if _, err := os.Stat(sessions); err != nil {
		t.Skip("本机没有真实会话目录")
	}
	tested := 0
	err := filepath.Walk(sessions, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".zstd") || tested >= 3 {
			return nil
		}
		log, err := v4.ReadFile(p)
		if err != nil {
			return nil
		}
		hist := History(log)
		if len(hist) == 0 {
			return nil
		}
		tested++
		roles := map[string]int{}
		for _, m := range hist {
			roles[m.Role]++
		}
		if roles["user"] == 0 || roles["assistant"] == 0 {
			t.Fatalf("%s 派生历史缺角色: %+v", filepath.Base(p), roles)
		}
		t.Logf("%s：%d 条历史 user=%d assistant=%d tool=%d",
			filepath.Base(filepath.Dir(p)), len(hist), roles["user"], roles["assistant"], roles["tool"])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tested == 0 {
		t.Skip("没有可用的真实会话")
	}
}
