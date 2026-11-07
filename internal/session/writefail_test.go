package session

import (
	"os"
	"strings"
	"testing"

	"dshgo/codecs/v4"
)

// 写到一半写不下去了（磁盘满、句柄被拔、权限被改）：
//
//	① 必须**报错**，不能假装写成功
//	② 已经写进去的事件一条都不能坏
//	③ 报错之后这份 Log 不能被当成还能用（seq 不能漂）
func TestAppendFailsLoudlyAndKeepsHistory(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	id := NewID()

	lg, err := CreateSession(home, cwd, id, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := lg.Append("user/message", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	path := lg.Path
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	// ① 关掉之后再写：必须报错
	if err := lg.Append("user/message", map[string]any{"after": "close"}); err == nil {
		t.Fatal("句柄已关还写成功——这种'静默丢失'最难查")
	} else if msg := err.Error(); !strings.Contains(msg, "关闭") &&
		!strings.Contains(msg, "写") && !strings.Contains(msg, "closed") {
		t.Fatalf("错误要说清是写不进去（实际是 %q）", msg)
	}

	// ② 前面 3 条必须完好
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("写失败不该把已有历史搞坏: %v", err)
	}
	if len(back.Events) != 3 {
		t.Fatalf("应该是 3 条（写失败那条不算），实际 %d 条", len(back.Events))
	}
	if back.TailTruncatedAt != 0 {
		t.Fatalf("写失败没留下半帧，不该报尾部截断（实际偏移 %d）", back.TailTruncatedAt)
	}

	// ③ 重新打开接着写：seq 必须从 3 起，不能重复
	again, err := OpenSessionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Append("user/message", map[string]any{"after": "reopen"}); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Events) != 4 {
		t.Fatalf("应该是 4 条，实际 %d 条", len(final.Events))
	}
	seen := map[int64]bool{}
	for _, ev := range final.Events {
		if seen[ev.Seq] {
			t.Fatalf("重复 seq %d——写失败之后 seq 漂了", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}

// 会话文件变成只读：resume 失败要**说清三件事**——
// 能读（不是文件坏了）、打不开写入、已有事件不受影响。
func TestReadOnlySessionExplainsItself(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	id := NewID()

	lg, err := CreateSession(home, cwd, id, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := lg.Append("user/message", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	path := lg.Path
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	// 只读：GNU 语义下 0o444；Windows 上对应只读属性
	if err := os.Chmod(path, 0o444); err != nil {
		t.Skip("这个平台改不了只读属性:", err)
	}
	defer os.Chmod(path, 0o644)

	_, err = OpenSessionAt(path)
	if err == nil {
		t.Skip("当前用户绕过只读属性，无法模拟")
	}
	msg := err.Error()
	for _, want := range []string{"能读", "打不开写入", "不受影响"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("要说清「%s」，实际: %v", want, msg)
		}
	}

	// 失败不能留下锁，也不能弄坏历史
	if _, statErr := os.Stat(path + ".lock"); statErr == nil {
		t.Fatal("失败之后不该留下锁文件")
	}
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("只读不影响读: %v", err)
	}
	if len(back.Events) != 2 {
		t.Fatalf("历史应该是 2 条，实际 %d 条", len(back.Events))
	}
}
