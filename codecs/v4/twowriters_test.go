package v4

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 两个宿主进程同时往一个会话追加帧：谁也别把谁的写没了。
//
// 实现上是 O_APPEND（不加锁），所以真正要验的是：
//   - 帧不会互相覆盖（O_APPEND 保证单个 write 的原子性）
//   - 读回来时每一帧都还能解，每个 writer 的每个序号都在
func TestTwoWritersDoNotLoseFrames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.v4.jsonl.zstd")

	first, err := Create(path, Header{Type: "session", Version: 4, ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	const perWriter = 20
	done := make(chan error, 2)
	write := func(writer int) {
		lg, err := OpenAppend(path)
		if err != nil {
			done <- err
			return
		}
		defer lg.Close()
		for i := 0; i < perWriter; i++ {
			payload, _ := json.Marshal(map[string]any{"writer": writer, "i": i})
			if err := lg.Append(Event{Type: "user/message", Data: payload}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}

	go write(1)
	go write(2)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("追加失败: %v", err)
		}
	}

	log, err := ReadFile(path)
	if err != nil {
		t.Fatalf("并发写完之后读不回来: %v", err)
	}
	if got, want := len(log.Events), perWriter*2; got != want {
		t.Fatalf("帧丢了：期望 %d 条事件，实际 %d", want, got)
	}

	seen := map[int]map[int]bool{1: {}, 2: {}}
	for _, e := range log.Events {
		var d struct {
			Writer int `json:"writer"`
			I      int `json:"i"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("事件解不出来: %v", err)
		}
		seen[d.Writer][d.I] = true
	}
	for _, w := range []int{1, 2} {
		if len(seen[w]) != perWriter {
			t.Fatalf("writer %d 只剩 %d 帧（期望 %d）", w, len(seen[w]), perWriter)
		}
	}
}

// 断电/磁盘满会留下**半帧**。这时读整份日志必须保住前面的部分——
// 为了最后半帧丢掉整个历史，是最坏的结果。
func TestTornTailKeepsEarlierEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.v4.jsonl.zstd")

	w, err := Create(path, Header{Type: "session", Version: 4, ID: "torn"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		payload, _ := json.Marshal(map[string]any{"i": i})
		if err := w.Append(Event{Type: "user/message", Data: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟写了一半：追加半截 zstd 帧（有 magic、没内容）
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x58}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	log, err := ReadFile(path)
	if err != nil {
		t.Fatalf("尾部半帧不该让整份日志读不出来: %v", err)
	}
	if len(log.Events) < 5 {
		t.Fatalf("前面的 5 条必须保住，实际只有 %d 条", len(log.Events))
	}
}
