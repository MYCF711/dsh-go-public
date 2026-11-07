package v4

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadGoldenFile(t *testing.T) {
	l, err := ReadFile(filepath.Join("testdata", "small.v4.jsonl.zstd"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if l.Header.Type != "session" || l.Header.Version != 4 {
		t.Fatalf("header = %+v", l.Header)
	}
	if l.Header.ID == "" || l.Header.CreatedAt == 0 {
		t.Fatalf("header 缺字段: %+v", l.Header)
	}
	if l.Frames != 3 {
		t.Fatalf("帧数 = %d，期望 3（header 独立成帧 + 两个批次帧）", l.Frames)
	}
	if len(l.Events) != 4 {
		t.Fatalf("事件数 = %d，期望 4（header 之外的 4 行）", len(l.Events))
	}
	if l.Events[0].Type != "permission/preset" {
		t.Fatalf("首个事件 = %q", l.Events[0].Type)
	}
	for i, ev := range l.Events {
		if ev.Seq != int64(i) {
			t.Fatalf("第 %d 个事件的 seq = %d，期望 %d（seq 必须是文件内连续序号）", i, ev.Seq, i)
		}
		if len(ev.Raw) == 0 || !strings.HasPrefix(string(ev.Raw), "{") {
			t.Fatalf("第 %d 个事件没有保留原始行", i)
		}
	}
}

func TestHeaderRejectsExtraKeys(t *testing.T) {
	// 多一个键即非法：宽进会把 v5 的字段读成 v4。
	var h Header
	err := h.UnmarshalJSON([]byte(`{"type":"session","version":4,"id":"x","whatever":1}`))
	if err == nil || !strings.Contains(err.Error(), "白名单") {
		t.Fatalf("应拒绝白名单外的键，得到 %v", err)
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	if _, err := Read([]byte("not zstd at all")); err == nil {
		t.Fatal("非 zstd 内容必须报错")
	}
	if _, err := Read(nil); err == nil {
		t.Fatal("空文件必须报错")
	}
}

func TestCorruptTailIsReported(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "small.v4.jsonl.zstd"))
	if err != nil {
		t.Fatal(err)
	}
	broken := b[:len(b)-6] // 砍掉尾部

	// 契约（2026-11-03 改）：尾部半帧**不能泼掉前面的**，但也**不能静默**。
	// 原来这里是"必须报错"——那样一次断电就整份历史读不出来。
	log, err := Read(broken)
	if err != nil {
		t.Fatalf("尾部截断不该让整份日志读不出来: %v", err)
	}
	if log.TailTruncatedAt == 0 {
		t.Fatal("截断必须报出来，不能静默少读几条")
	}
	if len(log.Events) == 0 {
		t.Fatal("前面的完整帧应该留着")
	}
}

// TestReadRealCorpus 是本包最有价值的一条测试：拿 $DSH_HOME 里的真实会话全量解析。
// CI 上没有这些文件，自动跳过。
func TestReadRealCorpus(t *testing.T) {
	root := os.Getenv("DSH_HOME")
	if root == "" {
		root = `D:\DeepSeek Harness\home`
	}
	sessions := filepath.Join(root, "sessions")
	if _, err := os.Stat(sessions); err != nil {
		t.Skip("本机没有真实会话目录：", sessions)
	}

	var files, events, frames int
	var failures []string
	err := filepath.Walk(sessions, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".zstd") {
			return nil
		}
		files++
		l, err := ReadFile(p)
		if err != nil {
			failures = append(failures, p+": "+err.Error())
			return nil
		}
		events += len(l.Events)
		frames += l.Frames
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("真实语料：%d 个文件，%d 帧，%d 事件", files, frames, events)
	if files == 0 {
		t.Skip("没有找到会话文件")
	}
	if len(failures) > 0 {
		t.Fatalf("%d/%d 个文件解析失败，前 5 条：\n%s", len(failures), files, strings.Join(failures[:min(5, len(failures))], "\n"))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
