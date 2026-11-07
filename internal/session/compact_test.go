package session

import (
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

func buildTenEventLog(t *testing.T) (*Log, *v4.Log) {
	t.Helper()
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/compact", "s-compact", "standard")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := l.Append("user/message", UserMessage([]spi.Block{{Type: "text", Text: "问"}})); err != nil {
			t.Fatal(err)
		}
		if err := l.Append("assistant/message", AssistantMessageEvent(1, i+1,
			[]map[string]any{TextBlock("答")}, "stop", nil)); err != nil {
			t.Fatal(err)
		}
	}
	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	return l, back
}

func TestCompactHidesRangeAndAppends(t *testing.T) {
	l, back := buildTenEventLog(t)
	before := len(back.Events)

	id, shadowed, err := Compact(l, back.Events, Span{Start: 0, End: 3},
		[]spi.Block{{Type: "text", Text: "（前两轮摘要）"}},
		CompactionMeta{Provider: "magpie", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || !strings.HasPrefix(id, "cmp-") {
		t.Fatalf("compactionId = %q", id)
	}
	if len(shadowed) != 4 {
		t.Fatalf("被遮蔽 %d 条，期望 4：%v", len(shadowed), shadowed)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != before+3 {
		t.Fatalf("日志长度 %d → %d，期望 +3（start/summary/end）", before, len(after.Events))
	}
	// 日志一条不少：被遮蔽的事件还在文件里。
	seqs := map[int64]bool{}
	for _, ev := range after.Events {
		seqs[ev.Seq] = true
	}
	for s := int64(0); s < int64(before); s++ {
		if !seqs[s] {
			t.Fatalf("seq %d 从日志里消失了——日志必须 append-only", s)
		}
	}

	surface := Surface(after)
	hiddenNodes := 0
	checkpoints := 0
	for _, n := range surface {
		if n.Summary {
			checkpoints++
		}
		if n.Seq >= 0 && n.Seq <= 3 {
			hiddenNodes++
		}
	}
	if checkpoints != 1 {
		t.Fatalf("替身节点数 = %d，期望 1", checkpoints)
	}
	if hiddenNodes != 0 {
		t.Fatalf("被遮蔽区间仍在 surface 上：%d 个节点", hiddenNodes)
	}
	// 原本 8 个节点，遮蔽 4 个，加 1 个替身 = 5。
	if len(surface) != 5 {
		t.Fatalf("surface 节点数 = %d，期望 5", len(surface))
	}
}

// 摘要为空时不许遮蔽历史：被压掉的事件会换成一段空白，模型什么也看不到。
//
// 这是「有产出但产出里没有载荷」的会话版：compaction 事件写进去了、seq 动了、
// surface 也真的变小了——但变小之后的那份历史是空的，而且没有任何一处报错。
func TestCompactRejectsEmptySummary(t *testing.T) {
	cases := map[string][]spi.Block{
		"nil":       nil,
		"空切片":       {},
		"只有空白":      {{Type: "text", Text: " \n\t "}},
		"类型不是 text": {{Type: "reasoning", Text: "想了很久"}},
	}
	for name, summary := range cases {
		t.Run(name, func(t *testing.T) {
			l, back := buildTenEventLog(t)
			defer l.Close()
			before := l.Seq()
			if _, _, err := Compact(l, back.Events, Span{Start: 0, End: 3}, summary, CompactionMeta{}); err == nil {
				t.Fatalf("空摘要（%s）必须报错", name)
			}
			if l.Seq() != before {
				t.Fatalf("被拒绝的压缩不许写任何事件：seq %d → %d", before, l.Seq())
			}
		})
	}
}

// 区间里只有记账事件（tool/call 之类）：压完 surface 一个节点都没少，却报「压缩成功」。
//
// 这是"压缩后 surface 没变却说已压缩"的形状：CLI 会打印「遮蔽 N 条事件」，
// 而模型看到的历史原封不动，反而凭空多出一条摘要。压了等于没压（还更贵）。
func TestCompactRejectsRangeWithoutVisibleEvents(t *testing.T) {
	l, _ := buildTenEventLog(t)
	defer l.Close()
	if err := l.Append("tool/call", ToolCallEvent(1, 1, "c1", "read", `{}`)); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	seq := back.Events[len(back.Events)-1].Seq
	before := l.Seq()

	if _, _, err := Compact(l, back.Events, Span{Start: seq, End: seq},
		[]spi.Block{{Type: "text", Text: "一段摘要"}}, CompactionMeta{}); err == nil {
		t.Fatal("只有 tool/call 的区间不许报压缩成功：surface 根本不会变小")
	}
	if l.Seq() != before {
		t.Fatalf("被拒绝的压缩不许写事件：seq %d → %d", before, l.Seq())
	}
}

func TestCompactRejectsEmptyRange(t *testing.T) {
	l, back := buildTenEventLog(t)
	defer l.Close()
	if _, _, err := Compact(l, back.Events, Span{Start: 100, End: 200}, nil, CompactionMeta{}); err == nil {
		t.Fatal("区间里没有事件必须报错")
	}
	if _, _, err := Compact(l, back.Events, Span{Start: 5, End: 1}, nil, CompactionMeta{}); err == nil {
		t.Fatal("start > end 必须报错")
	}
}

func TestCompactTwiceCascades(t *testing.T) {
	l, back := buildTenEventLog(t)
	if _, _, err := Compact(l, back.Events, Span{Start: 0, End: 3},
		[]spi.Block{{Type: "text", Text: "第一次摘要"}}, CompactionMeta{}); err != nil {
		t.Fatal(err)
	}
	mid, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	// 第二次把"第一次的摘要"也压进去（真实语料里就是这样级联的）。
	var firstSummarySeq int64 = -1
	for _, ev := range mid.Events {
		if ev.Type == "compaction/summary" {
			firstSummarySeq = ev.Seq
		}
	}
	if firstSummarySeq < 0 {
		t.Fatal("找不到第一次的 summary")
	}
	if _, _, err := Compact(l, mid.Events, Span{Start: 0, End: firstSummarySeq},
		[]spi.Block{{Type: "text", Text: "第二次摘要"}}, CompactionMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	surface := Surface(after)
	checkpoints := 0
	for _, n := range surface {
		if n.Summary {
			checkpoints++
			if n.Message.Content != "第二次摘要" {
				t.Fatalf("可见摘要 = %q，期望最后一次", n.Message.Content)
			}
		}
	}
	if checkpoints != 1 {
		t.Fatalf("级联后可见摘要 = %d，期望 1（前一个被后一个遮蔽）", checkpoints)
	}
}
