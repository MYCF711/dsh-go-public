package session

import (
	"strings"
	"testing"

	"dshgo/codecs/v4"
)

// 同一份会话被两个进程同时 resume：第二个必须被**明确拒绝**。
//
// 为什么不能放行：两边各自"读到 N 条 → 从 N 接着编号"，
// 写出来的日志里会出现**重复 seq**（实测过：9 条事件 3 个重复）。
// seq 是 surface 与压缩遮蔽的排序依据，重复意味着顺序不确定——
// 这比"打不开"糟糕得多。所以宁可拒绝，也不要写出一份顺序可疑的历史。
func TestSecondWriterIsRefused(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	id := NewID()

	first, err := CreateSession(home, cwd, id, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := first.Append("user/message", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	a, err := OpenSession(home, cwd, id)
	if err != nil {
		t.Fatal(err)
	}

	// 第二个句柄必须被拒，而且要说清下一步
	_, err = OpenSession(home, cwd, id)
	if err == nil {
		t.Fatal("同一份会话不允许两个写入者")
	}
	if !strings.Contains(err.Error(), "另一个进程") || !strings.Contains(err.Error(), ".lock") {
		t.Fatalf("拒绝时要说清原因与锁文件位置: %v", err)
	}

	// 关掉之后就能续写了
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := OpenSession(home, cwd, id)
	if err != nil {
		t.Fatalf("前一个写入者关掉后应该能续写: %v", err)
	}
	path := b.Path
	if err := b.Append("user/message", map[string]any{"after": "reopen"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读写完之后读不回来: %v", err)
	}
	seen := map[int64]bool{}
	for _, ev := range raw.Events {
		if seen[ev.Seq] {
			t.Fatalf("出现重复 seq %d（%d 条事件）", ev.Seq, len(raw.Events))
		}
		seen[ev.Seq] = true
	}
	if len(raw.Events) != 4 {
		t.Fatalf("应该是 3 + 1 条事件，实际 %d 条", len(raw.Events))
	}
}

// OpenSessionAt 也必须抢锁——它是 ACP resume 与 CLI compact 的入口，
// 漏了就等于给"同一份会话两个写入者"开后门（实测踩过）。
func TestOpenSessionAtAlsoClaimsLock(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	id := NewID()

	lg, err := CreateSession(home, cwd, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("user/message", map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	path, err := ResolvePath(home, cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	first, err := OpenSessionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessionAt(path); err == nil {
		t.Fatal("第二个 OpenSessionAt 必须被锁挡住")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenSessionAt(path)
	if err != nil {
		t.Fatalf("关掉之后应该能再开: %v", err)
	}
	again.Close()
}
