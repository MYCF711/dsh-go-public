package schedule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeMutationFixture(t *testing.T, dir, sessionID string, items ...map[string]any) []byte {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(map[string]any{"version": 1, "schedules": items, "preserved": "top-level"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, sessionID+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

func mutationItem(id, status string) map[string]any {
	return map[string]any{
		"id": id, "title": id, "prompt": "keep", "kind": "daily", "daily_at": "09:00",
		"next_at": "2099-01-02T09:00:00+08:00", "status": status,
		"created_at": "2026-01-01T00:00:00+08:00", "unknown": map[string]any{"keep": true},
	}
}

func revisionOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestDeletePersistsAcrossReopenAndRepeatedDelete(t *testing.T) {
	dir := t.TempDir()
	before := writeMutationFixture(t, dir, "session-a", mutationItem("a1", "running"), mutationItem("a2", "ended"))
	result, err := Delete(dir, "session-a", "a1", revisionOf(before))
	if err != nil {
		t.Fatal(err)
	}
	if result.Remaining != 1 || result.Revision == "" || result.Revision == revisionOf(before) {
		t.Fatalf("bad delete result: %+v", result)
	}
	items, err := Load(dir, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "a2" {
		t.Fatalf("重开后记录不对：%+v", items)
	}
	after, err := os.ReadFile(filepath.Join(dir, "session-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["preserved"]) != `"top-level"` {
		t.Fatalf("顶层未知字段被丢：%s", after)
	}
	var entries []map[string]any
	if err := json.Unmarshal(doc["schedules"], &entries); err != nil {
		t.Fatal(err)
	}
	if entries[0]["unknown"] == nil || entries[0]["created_at"] == "" {
		t.Fatalf("保留条目的字段被丢：%+v", entries[0])
	}
	if _, err := Delete(dir, "session-a", "a1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除必须 not_found，得到 %v", err)
	}
}

func TestDeleteRevisionConflictAndCorruptionDoNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	before := writeMutationFixture(t, dir, "s", mutationItem("a1", "running"))
	if _, err := Delete(dir, "s", "a1", "stale"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("revision 冲突必须可识别，得到 %v", err)
	}
	assertFileBytes(t, filepath.Join(dir, "s.json"), before)

	corrupt := []byte("{broken\n")
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Delete(dir, "broken", "a1", ""); err == nil {
		t.Fatal("损坏文件不能删除成功")
	}
	assertFileBytes(t, filepath.Join(dir, "broken.json"), corrupt)
	if _, err := Delete(dir, "missing", "a1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知文件/id 必须 not_found，得到 %v", err)
	}
}

func TestClearEndedReadbackABAndEmpty(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeMutationFixture(t, dirA, "one", mutationItem("ended", "ended"), mutationItem("active", "running"))
	writeMutationFixture(t, dirB, "one", mutationItem("b-ended", "ended"))
	result, err := ClearEnded(dirA, time.Date(2026, 11, 5, 0, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || len(result.Revisions) != 1 || result.Revisions["one"] == "" {
		t.Fatalf("clear result: %+v", result)
	}
	items, err := Load(dirA, "one")
	if err != nil || len(items) != 1 || items[0].ID != "active" {
		t.Fatalf("活跃任务被清或回读失败：items=%+v err=%v", items, err)
	}
	other, err := Load(dirB, "one")
	if err != nil || len(other) != 1 || other[0].ID != "b-ended" {
		t.Fatalf("A home 的 clear 污染 B home：items=%+v err=%v", other, err)
	}
	empty, err := ClearEnded(filepath.Join(dirA, "missing"), time.Now())
	if err != nil || empty.Deleted != 0 || len(empty.Revisions) != 0 {
		t.Fatalf("空目录应是 deleted=0：%+v err=%v", empty, err)
	}
}

func TestClearEndedPreflightAndWriteFailureRollback(t *testing.T) {
	t.Run("损坏文件阻止任何写入", func(t *testing.T) {
		dir := t.TempDir()
		before := writeMutationFixture(t, dir, "a", mutationItem("a1", "ended"))
		corrupt := []byte("{bad")
		if err := os.WriteFile(filepath.Join(dir, "z.json"), corrupt, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ClearEnded(dir, time.Now()); err == nil {
			t.Fatal("批量清理遇到坏文件必须失败")
		}
		assertFileBytes(t, filepath.Join(dir, "a.json"), before)
		assertFileBytes(t, filepath.Join(dir, "z.json"), corrupt)
	})

	t.Run("第二个原子写失败回滚第一个", func(t *testing.T) {
		dir := t.TempDir()
		beforeA := writeMutationFixture(t, dir, "a", mutationItem("a1", "ended"))
		beforeB := writeMutationFixture(t, dir, "b", mutationItem("b1", "ended"))
		if err := os.Mkdir(filepath.Join(dir, "b.json.tmp"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := ClearEnded(dir, time.Now()); err == nil {
			t.Fatal("注入第二文件写失败必须返回错误")
		}
		assertFileBytes(t, filepath.Join(dir, "a.json"), beforeA)
		assertFileBytes(t, filepath.Join(dir, "b.json"), beforeB)
	})
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("文件被失败操作改变：%s\ngot=%q\nwant=%q", path, got, want)
	}
}
