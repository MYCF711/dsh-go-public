package session

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

// TestCompactRealCorpus 是压缩的**批量回归**：拿真实会话的副本逐个压，验四条不变量。
//
// 单例验证（345 事件那个）说明"能压"，这里要说明"在各种真实形状上都压得对"。
// 原件一律只读——每个都先复制到临时目录再动。
func TestCompactRealCorpus(t *testing.T) {
	root := os.Getenv("DSH_HOME")
	if root == "" {
		root = `D:\DeepSeek Harness\home`
	}
	sessionsDir := filepath.Join(root, "sessions")
	if _, err := os.Stat(sessionsDir); err != nil {
		t.Skip("本机没有真实会话目录")
	}

	const limit = 15
	var files []string
	err := filepath.Walk(sessionsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".zstd") {
			return nil
		}
		// 太大的先跳过：批量回归要快，极端大小另有单测。
		if info.Size() > 4<<20 {
			return nil
		}
		files = append(files, p)
		if len(files) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("没有找到会话文件")
	}

	tmp := t.TempDir()
	passed, skipped, failed := 0, 0, 0

	for i, src := range files {
		name := filepath.Base(filepath.Dir(src))
		dst := filepath.Join(tmp, "case-"+string(rune('a'+i))+".zstd")
		if err := copyFile(src, dst); err != nil {
			t.Fatalf("%s: 复制失败: %v", name, err)
		}

		before, err := v4.ReadFile(dst)
		if err != nil {
			t.Errorf("%s: 读原件失败: %v", name, err)
			failed++
			continue
		}
		surfaceBefore := Surface(before)
		if len(surfaceBefore) < 10 {
			skipped++
			continue
		}

		// 压前半段，留最近 6 条。
		keep := 6
		cut := surfaceBefore[:len(surfaceBefore)-keep]
		span := Span{Start: cut[0].Seq, End: cut[len(cut)-1].Seq}

		l, err := OpenSessionAt(dst)
		if err != nil {
			t.Errorf("%s: 打开失败: %v", name, err)
			failed++
			continue
		}
		_, shadowed, err := Compact(l, before.Events, span,
			[]spi.Block{{Type: "text", Text: "（批量回归用的固定摘要）"}},
			CompactionMeta{Provider: "test", Model: "none"})
		if err != nil {
			l.Close()
			t.Errorf("%s: 压缩失败: %v", name, err)
			failed++
			continue
		}
		if err := l.Close(); err != nil {
			t.Errorf("%s: 关闭失败: %v", name, err)
			failed++
			continue
		}

		// —— 四条不变量 ——
		after, err := v4.ReadFile(dst)
		if err != nil {
			t.Errorf("%s: 压缩后读不回来: %v", name, err)
			failed++
			continue
		}
		if len(after.Events) != len(before.Events)+3 {
			t.Errorf("%s: 事件数 %d → %d，期望 +3", name, len(before.Events), len(after.Events))
		}
		seqs := make(map[int64]bool, len(after.Events))
		for _, ev := range after.Events {
			seqs[ev.Seq] = true
		}
		for _, s := range shadowed {
			if !seqs[s] {
				t.Errorf("%s: 被遮蔽的 seq %d 从日志里消失了（必须 append-only）", name, s)
			}
		}
		surfaceAfter := Surface(after)
		if len(surfaceAfter) >= len(surfaceBefore) {
			t.Errorf("%s: 压缩后 surface 没变少：%d → %d", name, len(surfaceBefore), len(surfaceAfter))
		}
		// 最近的尾巴必须留着（用户当前在谈的东西不能被压没）。
		hBefore, hAfter := History(before), History(after)
		if len(hAfter) == 0 {
			t.Errorf("%s: 压缩后历史空了", name)
		} else if hBefore[len(hBefore)-1].Content != hAfter[len(hAfter)-1].Content {
			t.Errorf("%s: 最后一条消息变了：%q → %q", name,
				trunc(hBefore[len(hBefore)-1].Content), trunc(hAfter[len(hAfter)-1].Content))
		}
		passed++
		t.Logf("%s: surface %d → %d（遮蔽 %d 条，事件 %d → %d）",
			name, len(surfaceBefore), len(surfaceAfter), len(shadowed),
			len(before.Events), len(after.Events))
	}

	t.Logf("批量回归：通过 %d / 跳过 %d / 失败 %d", passed, skipped, failed)
	if failed > 0 {
		t.Fatalf("%d 个会话的压缩回归失败", failed)
	}
	if passed == 0 {
		t.Skip("没有可压缩的样本")
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func trunc(s string) string {
	r := []rune(s)
	if len(r) <= 30 {
		return s
	}
	return string(r[:30]) + "…"
}
