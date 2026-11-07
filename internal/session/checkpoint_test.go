package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"dshgo/codecs/v4"
)

// 这些测试钉 internal/session 这一层的 durability checkpoint 语义：
// 前缀只读派生、原子发布、同一轮只落一份、失败响亮且回滚、恢复时逐字节复核。
// 时机（请求前 vs 请求后）在 cmd/dshd/serve_checkpoint_test.go 里，用真进程 + kill 钉。

// seedCheckpointSession 造一份真实 v4 会话并返回路径（事件 seq 从 0 连续）。
func seedCheckpointSession(t *testing.T, home, id string, events int) string {
	t.Helper()
	cwd := t.TempDir()
	lg, err := CreateSession(home, cwd, id, "default")
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	for i := 0; i < events; i++ {
		if err := lg.Append("turn/start", TurnStartData(i+1)); err != nil {
			lg.Close()
			t.Fatalf("写事件 %d: %v", i, err)
		}
	}
	path := lg.Path
	if err := lg.Close(); err != nil {
		t.Fatalf("关会话: %v", err)
	}
	return path
}

func readFileOrFail(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return data
}

func checkpointPath(t *testing.T, home, sessionID string, covered int64) string {
	t.Helper()
	dir, err := CheckpointDir(home, sessionID, covered)
	if err != nil {
		t.Fatalf("算侧车目录: %v", err)
	}
	return filepath.Join(dir, "checkpoint.json")
}

// TestDurableCheckpointCapturesPrefixAndIsReadOnly 钉两件事：
// ① 快照记的就是那一刻的前缀（字节数、sha256、事件数）；
// ② 写快照不改 v4 一个字节（append-only，读路径不写盘）。
func TestDurableCheckpointCapturesPrefixAndIsReadOnly(t *testing.T) {
	home := t.TempDir()
	id := "session-ckpt-unit"
	path := seedCheckpointSession(t, home, id, 3)
	before := readFileOrFail(t, path)

	req := NewCheckpointRequest("text", []byte("请先落盘再请求"))
	cp, err := WriteDurableCheckpoint(home, path, &req)
	if err != nil {
		t.Fatalf("写快照: %v", err)
	}
	if after := readFileOrFail(t, path); !bytes.Equal(before, after) {
		t.Fatalf("写快照改动了 v4：%d → %d 字节", len(before), len(after))
	}
	if cp.SessionID != id || cp.CoveredEvents != 3 || cp.LastSeq != 2 {
		t.Fatalf("快照身份不对: %+v", cp)
	}
	if cp.SizeBytes != int64(len(before)) || !bytesEqualHex(before, cp.SHA256) {
		t.Fatalf("快照没有锁定那一刻的前缀: size=%d sha=%s", cp.SizeBytes, cp.SHA256)
	}
	if cp.Request == nil || cp.Request.Kind != "text" || cp.Request.Text != "请先落盘再请求" {
		t.Fatalf("请求身份不对: %+v", cp.Request)
	}

	back, err := ReadDurableCheckpoint(home, id, 3)
	if err != nil {
		t.Fatalf("读回快照: %v", err)
	}
	if back.SHA256 != cp.SHA256 || back.CoveredEvents != cp.CoveredEvents || back.SessionPath != cp.SessionPath {
		t.Fatalf("读回的快照与写入不一致: %+v vs %+v", back, cp)
	}
	// 不存在的锚点必须是「没有」，不能是「损坏」——调用方靠这个区分。
	if _, err := ReadDurableCheckpoint(home, id, 99); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("不存在的锚点应返回 os.ErrNotExist，实际 %v", err)
	}
}

// TestDurableCheckpointIdempotentForSamePrefix 钉「同一轮只落一份」：
// 重复调用不重写字节；同一锚点前缀变了就响亮失败，不悄悄覆盖。
func TestDurableCheckpointIdempotentForSamePrefix(t *testing.T) {
	home := t.TempDir()
	id := "session-ckpt-idem"
	path := seedCheckpointSession(t, home, id, 2)
	target := checkpointPath(t, home, id, 2)

	first, err := WriteDurableCheckpoint(home, path, &CheckpointRequest{Kind: "text", SHA256: strings.Repeat("a", 64), Bytes: 1})
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	bytes1 := readFileOrFail(t, target)
	second, err := WriteDurableCheckpoint(home, path, &CheckpointRequest{Kind: "text", SHA256: strings.Repeat("a", 64), Bytes: 2})
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if bytes2 := readFileOrFail(t, target); !bytes.Equal(bytes1, bytes2) {
		t.Fatalf("同一轮重复调用重写了侧车")
	}
	if second.SHA256 != first.SHA256 || second.CapturedAt != first.CapturedAt {
		t.Fatalf("第二次应原样返回第一份，实际 %+v", second)
	}
}

// TestDurableCheckpointRejectsTornPrefix：尾部有半帧（被杀时正在写）时，
// 不允许把半个前缀当 checkpoint —— 那会让「已落盘」变成谎话。
func TestDurableCheckpointRejectsTornPrefix(t *testing.T) {
	home := t.TempDir()
	id := "session-ckpt-torn"
	path := seedCheckpointSession(t, home, id, 2)
	appendTornTail(t, path)

	_, err := WriteDurableCheckpoint(home, path, nil)
	if err == nil {
		t.Fatalf("尾部有半帧仍然写成功")
	}
	if !errors.Is(err, ErrDurableCheckpointCorrupt) {
		t.Fatalf("错误应可判为 ErrDurableCheckpointCorrupt，实际 %v", err)
	}
	if _, statErr := os.Stat(checkpointPath(t, home, id, 2)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("失败时留下了侧车（静默半写）: %v", statErr)
	}
}

// TestDurableCheckpointPublishFailureRollsBack：建目录都失败时必须响亮失败，
// 并且不留 tmp、不留空目录。
func TestDurableCheckpointPublishFailureRollsBack(t *testing.T) {
	home := t.TempDir()
	id := "session-ckpt-rollback"
	path := seedCheckpointSession(t, home, id, 1)
	// 用普通文件占住侧车根：MkdirAll 必然失败，与权限位无关。
	rootFile := filepath.Join(home, "durability-checkpoints")
	if err := os.WriteFile(rootFile, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteDurableCheckpoint(home, path, nil); err == nil {
		t.Fatalf("建目录失败却返回成功")
	}
	if err := filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(p) == ".tmp" || filepath.Base(p) == "checkpoint.json" {
			t.Fatalf("失败后留下半写产物: %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDurableCheckpointSessionsDoNotCross：A/B 会话各自一份，互不覆盖。
func TestDurableCheckpointSessionsDoNotCross(t *testing.T) {
	home := t.TempDir()
	pathA := seedCheckpointSession(t, home, "session-ckpt-a", 2)
	pathB := seedCheckpointSession(t, home, "session-ckpt-b", 2)
	if pathA == pathB {
		t.Fatalf("两个会话不应共用一个 v4 文件")
	}
	cpA, err := WriteDurableCheckpoint(home, pathA, nil)
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	cpB, err := WriteDurableCheckpoint(home, pathB, nil)
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if cpA.SessionID != "session-ckpt-a" || cpB.SessionID != "session-ckpt-b" {
		t.Fatalf("A/B 串了: %+v %+v", cpA, cpB)
	}
	if cpA.SHA256 == cpB.SHA256 {
		t.Fatalf("两份会话内容不同，前缀摘要不该相同")
	}
	if _, err := ReadDurableCheckpoint(home, "session-ckpt-a", 2); err != nil {
		t.Fatalf("B 写完之后 A 的快照读不出来: %v", err)
	}
	listA, err := ListDurableCheckpoints(home, "session-ckpt-a")
	if err != nil || len(listA) != 1 || listA[0].SessionID != "session-ckpt-a" {
		t.Fatalf("A 的快照清单不对: %+v err=%v", listA, err)
	}
}

// TestDurableCheckpointConcurrentSameTurn：同一轮的并发调用只允许落一份，
// 且每个调用要么拿到同一份快照、要么拿到可读的错误。
func TestDurableCheckpointConcurrentSameTurn(t *testing.T) {
	home := t.TempDir()
	id := "session-ckpt-concurrent"
	path := seedCheckpointSession(t, home, id, 4)
	var wg sync.WaitGroup
	results := make([]DurableCheckpoint, 16)
	errs := make([]error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := NewCheckpointRequest("text", []byte("并发 "+strconv.Itoa(i)))
			results[i], errs[i] = WriteDurableCheckpoint(home, path, &req)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发调用失败: %v", i, err)
		}
		if results[i].SHA256 != results[0].SHA256 {
			t.Fatalf("第 %d 个并发调用拿到的不是同一份快照", i)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(checkpointPath(t, home, id, 4)))
	if err != nil {
		t.Fatalf("列侧车目录: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "checkpoint.json" {
		t.Fatalf("并发后应只有一份侧车，实际 %v", entries)
	}
}

// TestRecoverDurableCheckpointIgnoresTailTear 钉「杀进程后重启」：
// 快照之后又半写了几个字节，前缀仍必须逐字节复核通过并完整读出来。
func TestRecoverDurableCheckpointIgnoresTailTear(t *testing.T) {
	home := t.TempDir()
	id := "session-ckpt-recover"
	path := seedCheckpointSession(t, home, id, 3)
	if _, err := WriteDurableCheckpoint(home, path, nil); err != nil {
		t.Fatalf("写快照: %v", err)
	}
	appendTornTail(t, path)

	got, err := RecoverDurableCheckpoint(home, id)
	if err != nil {
		t.Fatalf("恢复: %v", err)
	}
	if got.Checkpoint.CoveredEvents != 3 || len(got.Events) != 3 {
		t.Fatalf("恢复出的事件数不对: %d / %d", got.Checkpoint.CoveredEvents, len(got.Events))
	}
	if got.PrefixBytes != got.Checkpoint.SizeBytes {
		t.Fatalf("复核通过的字节数 %d 与快照 %d 不一致", got.PrefixBytes, got.Checkpoint.SizeBytes)
	}
	if got.ExtraBytes == 0 || got.TailTruncatedAt == 0 {
		t.Fatalf("快照之后的半帧没被报出来: extra=%d tornAt=%d", got.ExtraBytes, got.TailTruncatedAt)
	}
	if got.Header.ID != id {
		t.Fatalf("恢复出的 header id = %q", got.Header.ID)
	}
}

// TestRecoverDurableCheckpointWithoutSnapshot：没有快照必须是 ErrNoDurableCheckpoint，
// 不能伪装成「恢复成功、零帧」。
func TestRecoverDurableCheckpointWithoutSnapshot(t *testing.T) {
	home := t.TempDir()
	id := "session-ckpt-nosnap"
	seedCheckpointSession(t, home, id, 1)
	if _, err := RecoverDurableCheckpoint(home, id); !errors.Is(err, ErrNoDurableCheckpoint) {
		t.Fatalf("没有快照时应返回 ErrNoDurableCheckpoint，实际 %v", err)
	}
}

// TestRecoverDurableCheckpointDetectsPrefixRewrite：请求前的前缀被改写以后，
// 快照必须报损坏，而不是拿改过的字节冒充「已落盘」。
func TestRecoverDurableCheckpointDetectsPrefixRewrite(t *testing.T) {
	home := t.TempDir()
	id := "session-ckpt-rewrite"
	path := seedCheckpointSession(t, home, id, 3)
	if _, err := WriteDurableCheckpoint(home, path, nil); err != nil {
		t.Fatalf("写快照: %v", err)
	}
	data := readFileOrFail(t, path)
	if len(data) < 32 {
		t.Fatalf("会话文件太短: %d", len(data))
	}
	tampered := append([]byte(nil), data...)
	tampered[20] ^= 0xFF // 前缀内部翻一个字节：长度不变，内容变了
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("改写会话: %v", err)
	}
	if _, err := RecoverDurableCheckpoint(home, id); !errors.Is(err, ErrDurableCheckpointCorrupt) {
		t.Fatalf("前缀被改写却没报损坏: %v", err)
	}
	// 快照本身也不许被这次恢复改写。
	if again := readFileOrFail(t, checkpointPath(t, home, id, 3)); len(again) == 0 {
		t.Fatalf("侧车被删了")
	}
}

// TestCheckpointRequestHeadIsBoundedAndValidUTF8：超长请求只留合法 UTF-8 头部，
// 完整身份仍在 sha256/bytes 里。
func TestCheckpointRequestHeadIsBoundedAndValidUTF8(t *testing.T) {
	payload := bytes.Repeat([]byte("落盘"), CheckpointRequestTextLimit) // 远超上限
	req := NewCheckpointRequest("text", payload)
	if len(req.Text) > CheckpointRequestTextLimit {
		t.Fatalf("头部 %d 字节超过上限 %d", len(req.Text), CheckpointRequestTextLimit)
	}
	if !req.Truncated {
		t.Fatalf("超长载荷必须标记 Truncated")
	}
	if req.Bytes != int64(len(payload)) || !bytesEqualHex(payload, req.SHA256) {
		t.Fatalf("完整身份丢了: bytes=%d sha=%s", req.Bytes, req.SHA256)
	}
	if !utf8.ValidString(req.Text) {
		t.Fatalf("头部不是合法 UTF-8（截断切在汉字中间）")
	}
}

// appendTornTail 在 v4 尾巴上补 3 个字节：小于一个帧头，读起来就是「半帧」。
func appendTornTail(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("打开会话追加: %v", err)
	}
	if _, err := f.Write([]byte{0x28, 0xB5, 0x2F}); err != nil {
		_ = f.Close()
		t.Fatalf("写半帧: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关会话: %v", err)
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("半帧后会话应仍可读（尾部截断）: %v", err)
	}
	if lg.TailTruncatedAt == 0 {
		t.Fatalf("3 字节尾巴没有被识别成半帧")
	}
}
