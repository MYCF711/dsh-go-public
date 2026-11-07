package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

func makeServeJournalStoreSession(t *testing.T, home, cwd, id string) string {
	t.Helper()
	lg, err := session.CreateSession(home, cwd, id, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	return lg.Path
}

func TestServeJournalStoreCrossProcessRawRecovery(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	alphaPath := makeServeJournalStoreSession(t, home, cwd, "session-alpha")
	betaPath := makeServeJournalStoreSession(t, home, cwd, "session-beta")
	alphaV4Before, err := os.ReadFile(alphaPath)
	if err != nil {
		t.Fatal(err)
	}
	betaV4Before, err := os.ReadFile(betaPath)
	if err != nil {
		t.Fatal(err)
	}

	epochs := int64(500)
	first := transport.NewJournals(func() int64 { epochs++; return epochs })
	if _, err := restoreServeJournals(home, first); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage("{\n  \"message\": {\"content\": [{\"type\":\"reasoning\",\"text\":\"保留空白与中文\"}]}\n}")
	for _, f := range []transport.ServerFrame{
		{Type: "assistant/message", Data: raw, Time: 9101},
		{Type: "tool/call", Data: json.RawMessage(`{"callId":"call-a"}`), Time: 9102},
		{Type: "tool/result", Data: json.RawMessage(`{"message":{"toolCallId":"call-a"}}`), Time: 9103},
		{Type: "approval/request", Data: json.RawMessage(`{"requestId":"perm-a"}`), Time: 9104},
	} {
		got, err := first.Append("session-alpha", f.Type, f.Data, f.Time)
		if err != nil || got.Seq <= 0 || got.Time != f.Time || !bytes.Equal(got.Data, f.Data) {
			t.Fatalf("首进程追加失真：got=%+v err=%v", got, err)
		}
	}
	if _, err := first.Append("session-beta", "assistant/message", json.RawMessage(`{"text":"B 私有正文"}`), 9201); err != nil {
		t.Fatal(err)
	}
	oldAlphaEpoch := first.Get("session-alpha").Epoch()

	second := transport.NewJournals(func() int64 { epochs++; return epochs })
	if _, err := restoreServeJournals(home, second); err != nil {
		t.Fatal(err)
	}
	alpha := second.Get("session-alpha")
	got, last, epoch := alpha.Since(0, 0)
	if alpha.Epoch() == oldAlphaEpoch || last != 4 || epoch != alpha.Epoch() || len(got) != 4 {
		t.Fatalf("跨进程水位/世代错误：old=%d new=%d last=%d frames=%d", oldAlphaEpoch, alpha.Epoch(), last, len(got))
	}
	want, _, _ := first.Get("session-alpha").Since(0, 0)
	if !reflect.DeepEqual(got, want) || !bytes.Equal(got[0].Data, raw) {
		t.Fatalf("跨进程必须逐帧逐字恢复：got=%+v want=%+v", got, want)
	}
	for _, typ := range []string{"assistant/message", "tool/call", "tool/result", "approval/request"} {
		found := false
		for _, f := range got {
			found = found || f.Type == typ
		}
		if !found {
			t.Errorf("恢复缺原始类型 %s", typ)
		}
	}
	beta, betaLast, _ := second.Get("session-beta").Since(0, 0)
	if betaLast != 1 || len(beta) != 1 || !bytes.Contains(beta[0].Data, []byte("B 私有正文")) || bytes.Contains(beta[0].Data, []byte("perm-a")) {
		t.Fatalf("A/B 恢复串联：%+v last=%d", beta, betaLast)
	}

	next, err := second.Append("session-alpha", "turn/done", json.RawMessage(`{"turnId":"turn-after-restart"}`), 9105)
	if err != nil || next.Seq != 5 || next.Time != 9105 {
		t.Fatalf("恢复后追加必须续旧 seq 且不改 time：%+v err=%v", next, err)
	}
	third := transport.NewJournals(func() int64 { epochs++; return epochs })
	if _, err := restoreServeJournals(home, third); err != nil {
		t.Fatal(err)
	}
	finalFrames, finalLast, _ := third.Get("session-alpha").Since(0, 0)
	if finalLast != 5 || len(finalFrames) != 5 || finalFrames[4].Type != "turn/done" || finalFrames[4].Seq != 5 {
		t.Fatalf("第二次进程必须读到恢复后追加的帧：last=%d frames=%+v", finalLast, finalFrames)
	}

	alphaV4After, _ := os.ReadFile(alphaPath)
	betaV4After, _ := os.ReadFile(betaPath)
	if !bytes.Equal(alphaV4Before, alphaV4After) || !bytes.Equal(betaV4Before, betaV4After) {
		t.Fatal("Transport journal 恢复/追加不得重写 v4 一个字节")
	}
}

func TestServeJournalStoreResetAndReadOnlyRecovery(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	makeServeJournalStoreSession(t, home, cwd, "session-reset")
	js := transport.NewJournals(func() int64 { return 77 })
	if _, err := restoreServeJournals(home, js); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Append("session-reset", "turn/done", json.RawMessage(`{"turnId":"old"}`), 100); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(mustFindServeJournalSession(t, home, "session-reset")), serveJournalFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	reopened := transport.NewJournals(func() int64 { return 88 })
	if _, err := restoreServeJournals(home, reopened); err != nil {
		t.Fatal(err)
	}
	_ = reopened.Get("session-reset").Window(0, 0)
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("启动恢复与 history 读路径不得写 journal 文件")
	}
	if epoch := reopened.Reset("session-reset"); epoch != 88 || reopened.Get("session-reset").LastSeq() != 0 {
		t.Fatalf("reset 语义不得改变：epoch=%d last=%d", epoch, reopened.Get("session-reset").LastSeq())
	}

	afterReset := transport.NewJournals(func() int64 { return 99 })
	if _, err := restoreServeJournals(home, afterReset); err != nil {
		t.Fatal(err)
	}
	if got := afterReset.Get("session-reset").LastSeq(); got != 0 {
		t.Fatalf("reset 后的旧帧不得在下个进程复活：%d", got)
	}
}

func TestServeJournalStoreTornTailAndCorruption(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	makeServeJournalStoreSession(t, home, cwd, "session-tail")
	js := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, js); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Append("session-tail", "turn/done", json.RawMessage(`{"turnId":"complete"}`), 123); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(mustFindServeJournalSession(t, home, "session-tail")), serveJournalFileName)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(original, []byte(`{"version":1,"kind":"frame"`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, recovered); err != nil {
		t.Fatalf("只忽略未提交的 torn tail，完整前缀应可恢复：%v", err)
	}
	frames, last, _ := recovered.Get("session-tail").Since(0, 0)
	if last != 1 || len(frames) != 1 || frames[0].Time != 123 {
		t.Fatalf("torn tail 前的完整帧失真：last=%d frames=%+v", last, frames)
	}

	corrupt := append(original, []byte("not-json\n")...)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreServeJournals(home, transport.NewJournals(nil)); err == nil || !strings.Contains(err.Error(), "第 2 行") {
		t.Fatalf("换行终结的损坏中间记录必须 fail loud：%v", err)
	}
}

func TestServeJournalStoreTornTailRepairedOnlyOnNextAppend(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	makeServeJournalStoreSession(t, home, cwd, "session-tail-append")
	first := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, first); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append("session-tail-append", "turn/done", json.RawMessage(`{"turnId":"committed"}`), 123); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(mustFindServeJournalSession(t, home, "session-tail-append")), serveJournalFileName)
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append([]byte(nil), committed...), []byte(`{"version":1,"kind":"frame"`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	torn, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	second := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, second); err != nil {
		t.Fatal(err)
	}
	afterRead, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRead, torn) {
		t.Fatal("只读启动恢复不得截断或改写 torn tail")
	}
	if _, err := second.Append("session-tail-append", "turn/done", json.RawMessage(`{"turnId":"after-tail"}`), 124); err != nil {
		t.Fatalf("下一条已提交追加应只截去未提交尾巴：%v", err)
	}
	afterAppend, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterAppend[:len(committed)], committed) {
		t.Fatal("修复 torn tail 不得改写任何已提交前缀字节")
	}

	third := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, third); err != nil {
		t.Fatalf("torn tail 后追加的记录必须能被下一进程恢复：%v", err)
	}
	frames, last, _ := third.Get("session-tail-append").Since(0, 0)
	if last != 2 || len(frames) != 2 || frames[0].Time != 123 || frames[1].Time != 124 || !bytes.Contains(frames[1].Data, []byte("after-tail")) {
		t.Fatalf("torn tail 前后已提交帧失真：last=%d frames=%+v", last, frames)
	}
}

func mustFindServeJournalSession(t *testing.T, home, id string) string {
	t.Helper()
	path, err := session.FindByID(home, id)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
