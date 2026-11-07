package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

// buildLog 写一个会话文件并读回来。
func buildLog(t *testing.T, appendFn func(l *Log)) *v4.Log {
	t.Helper()
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/surface", "s-surface", "standard")
	if err != nil {
		t.Fatal(err)
	}
	appendFn(l)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func TestSurfaceHidesCompactedRange(t *testing.T) {
	log := buildLog(t, func(l *Log) {
		must := func(ev string, d any) {
			t.Helper()
			if err := l.Append(ev, d); err != nil {
				t.Fatal(err)
			}
		}
		must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "第一问"}})) // seq 0
		must("assistant/message", AssistantMessageEvent(1, 1,                       // seq 1
			[]map[string]any{TextBlock("第一答")}, "stop", nil))
		must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "第二问"}})) // seq 2
		// seq 3：把 0..1 压成摘要
		must("compaction/summary", map[string]any{
			"compactionId":       "c1",
			"summary":            []map[string]any{TextBlock("（摘要）第一问第一答")},
			"shadowedRange":      map[string]any{"start": 0, "end": 1},
			"shadowedSeqs":       []int64{0, 1},
			"shadowedTokenCount": 42,
			"provider":           "magpie",
			"model":              "m",
		})
		must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "第三问"}})) // seq 4
	})

	surface := Surface(log)
	if len(surface) != 3 {
		t.Fatalf("surface 节点数 = %d，期望 3（第二问 + 摘要 + 第三问）：%+v", len(surface), surface)
	}
	// 替身必须出现在**它遮蔽的那段起点**，也就是最前面——
	// 追加到末尾会让模型以为摘要是最新发生的事。
	if !surface[0].Summary || surface[0].Source != "compact-checkpoint" {
		t.Fatalf("节点 0 应是压缩摘要: %+v", surface[0])
	}
	if !strings.Contains(surface[0].Message.Content, "摘要") {
		t.Fatalf("摘要内容不对: %q", surface[0].Message.Content)
	}
	if surface[1].Message.Content != "第二问" {
		t.Fatalf("节点 1 = %+v", surface[1])
	}
	if surface[2].Message.Content != "第三问" {
		t.Fatalf("节点 2 = %+v", surface[2])
	}

	// 日志本身不能少一条（append-only）。
	if len(log.Events) != 5 {
		t.Fatalf("日志事件数 = %d，期望 5（遮蔽只发生在派生期）", len(log.Events))
	}
}

func TestSurfaceKeepsSystemMessage(t *testing.T) {
	log := buildLog(t, func(l *Log) {
		if err := l.Append("system/message", map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{"role": "system", "content": []map[string]any{TextBlock("你是 dsh-go")}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := l.Append("user/message", UserMessage([]spi.Block{{Type: "text", Text: "hi"}})); err != nil {
			t.Fatal(err)
		}
	})
	hist := History(log)
	if len(hist) != 2 || hist[0].Role != "system" || hist[1].Role != "user" {
		t.Fatalf("历史 = %+v", hist)
	}
}

func TestSurfaceHonorsReplaceOp(t *testing.T) {
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/surface", "s-replace", "standard")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("user/message", UserMessage([]spi.Block{{Type: "text", Text: "原文"}})); err != nil {
		t.Fatal(err)
	}
	// 手工追加一条带 replace 的 user/message
	w, err := v4.OpenAppend(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(v4.Event{
		Type: "user/message", Seq: 1, Time: 1,
		Data:      []byte(`{"content":[{"type":"text","text":"替换后"}],"role":"user","id":"x","surfaceOp":"append"}`),
		SurfaceOp: []byte(`{"op":"replace","startSeq":0,"endSeq":0}`),
	}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	l.Close()

	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	surface := Surface(back)
	if len(surface) != 1 || surface[0].Message.Content != "替换后" {
		t.Fatalf("replace 没生效: %+v", surface)
	}
}

// TestSurfaceRealCorpus 在真实压缩过的会话上验证：节点变少、摘要出现、日志不短。
func TestSurfaceRealCorpus(t *testing.T) {
	root := os.Getenv("DSH_HOME")
	if root == "" {
		root = `D:\DeepSeek Harness\home`
	}
	sessions := filepath.Join(root, "sessions")
	if _, err := os.Stat(sessions); err != nil {
		t.Skip("本机没有真实会话目录")
	}
	checked := 0
	err := filepath.Walk(sessions, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".zstd") || checked >= 40 {
			return nil
		}
		log, err := v4.ReadFile(p)
		if err != nil {
			return nil
		}
		eligible, summaries, prunes, checkpointMsgs := 0, 0, 0, 0
		for _, ev := range log.Events {
			switch ev.Type {
			case "system/message", "developer/message", "user/message", "assistant/message", "tool/result":
				eligible++
				// 真实语料里，DSH 自己的压缩/fork 也会用
				// source.kind = compact-checkpoint 的 user/message 来放摘要替身。
				// 必须解析 source.kind，不能用「正文里含 compact-checkpoint 字样」判。
				// 实测踩过：有的消息正文在讨论这个协议（第 10,340 字符处），
				// source.kind 是 plugin:hindsight；用子串判会误算成替身，
				// 于是「有替身却没有可见摘要」这条断言误报。
				if ev.Type == "user/message" && sourceKind(ev.Data) == "compact-checkpoint" {
					checkpointMsgs++
				}
			case "compaction/summary":
				summaries++
			case "compaction/prune":
				prunes++
			}
		}
		compacted := summaries + prunes
		if compacted == 0 {
			return nil
		}
		checked++
		surface := Surface(log)
		summaryNodes := 0
		for _, n := range surface {
			if n.Summary {
				summaryNodes++
			}
		}
		if len(surface) > eligible+summaryNodes {
			t.Fatalf("%s: surface=%d 超过上限 %d", filepath.Base(p), len(surface), eligible+summaryNodes)
		}
		// 压缩会**级联**：后一次压缩把上一次的摘要节点也遮蔽掉，
		// 所以可见摘要节点数通常远小于压缩次数（真实语料里 9 次压缩 → 1 个可见摘要）。
		// 替身有两个来源：compaction/summary 事件，以及 source.kind = compact-checkpoint
		// 的 user/message（真实语料里 DSH 自己就这么写）。prune 是纯裁剪，不留替身。
		if summaryNodes > summaries+checkpointMsgs {
			t.Errorf("%s: 可见摘要 %d 多于替身来源 %d（summary %d + checkpoint 消息 %d）",
				filepath.Base(p), summaryNodes, summaries+checkpointMsgs, summaries, checkpointMsgs)
		}
		if summaries+checkpointMsgs > 0 && summaryNodes == 0 {
			t.Errorf("%s: 有 %d 个替身来源却没有任何可见摘要", filepath.Base(p), summaries+checkpointMsgs)
		}
		t.Logf("%s：可派生 %d 条 → surface %d 条（summary %d / prune %d / checkpoint 消息 %d，可见摘要 %d）",
			filepath.Base(filepath.Dir(p)), eligible, len(surface), summaries, prunes, checkpointMsgs, summaryNodes)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Skip("没有找到压缩过的会话")
	}
}

func bytesContains(b []byte, sub string) bool {
	return bytes.Contains(b, []byte(sub))
}

// sourceKind 取 user/message 的 source.kind；没有就返回空串。
func sourceKind(data []byte) string {
	var d struct {
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return ""
	}
	return d.Source.Kind
}
