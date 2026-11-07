package schedule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeStoreFixture(t *testing.T, dir, sessionID, doc string) string {
	t.Helper()
	p := filepath.Join(dir, sessionID+".json")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStoreLoadsLegacyAndCurrentVersions(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "无version的历史形状",
			doc:  `{"schedules":[{"id":"sch-legacy","title":"旧任务","prompt":"只读","kind":"every","every_seconds":300,"next_at":"2026-11-03T05:00:00+08:00"}]}`,
		},
		{
			name: "当前version=1",
			doc:  `{"version":1,"schedules":[{"id":"sch-current","title":"当前任务","prompt":"只读","kind":"every","every_seconds":300,"next_at":"2026-11-03T05:00:00+08:00"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeStoreFixture(t, dir, "s", tt.doc)
			items, err := (&store{dir: dir}).load("s")
			if err != nil {
				t.Fatalf("兼容文件必须可读: %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("任务数=%d，期望 1", len(items))
			}
			if items[0].AllowWrites {
				t.Fatal("缺少 allow_writes 的旧数据必须保持 false")
			}
		})
	}
}

// 从legacy读出后，下一次save升级为当前版本；缺失权限不能被迁移放大。
func TestStoreSaveLegacyWritesCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	writeStoreFixture(t, dir, "legacy", `{"schedules":[{"id":"sch-legacy","title":"旧任务","prompt":"只读","kind":"every","every_seconds":300,"next_at":"2026-11-03T05:00:00+08:00"}]}`)
	st := &store{dir: dir}
	items, err := st.load("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.save("legacy", items); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "legacy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version   int `json:"version"`
		Schedules []struct {
			AllowWrites bool `json:"allow_writes"`
		} `json:"schedules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 {
		t.Fatalf("save 后 version=%d，期望当前版本 1；文件=%s", doc.Version, raw)
	}
	if len(doc.Schedules) != 1 || doc.Schedules[0].AllowWrites {
		t.Fatalf("legacy 升级后 allow_writes 必须为 false: %s", raw)
	}
}

// 通过真实工具写入口验证：load失败之后不能继续save覆盖未知版本。
func TestCreateUnsupportedVersionPreservesOriginal(t *testing.T) {
	for _, version := range []int{0, 999} {
		t.Run(fmt.Sprintf("version=%d", version), func(t *testing.T) {
			dir := t.TempDir()
			before := []byte(fmt.Sprintf(`{"version":%d,"schedules":[]}`, version))
			p := writeStoreFixture(t, dir, "s", string(before))
			res, err := mk(dir, kindCreate).Exec(ctxWith("s"), json.RawMessage(`{"title":"新任务","prompt":"只读","kind":"every","every_seconds":300}`))
			if err == nil {
				t.Errorf("version=%d 不在白名单，create 必须失败", version)
			}
			if res.Hint == "" {
				t.Error("失败必须保留给调用方的 Hint")
			}
			after, readErr := os.ReadFile(p)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Errorf("未知版本被 create 覆盖:\n前: %s\n后: %s", before, after)
			}
		})
	}
}

func TestStoreRejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	writeStoreFixture(t, dir, "future", `{"version":999,"schedules":[]}`)
	if _, err := (&store{dir: dir}).load("future"); err == nil {
		t.Fatal("未来版本必须 fail-loud，不能被 json.Unmarshal 当未知字段忽略")
	}
}

func TestStoreCorruptJSONFailsWithoutOverwrite(t *testing.T) {
	for _, tt := range []struct {
		name string
		doc  string
	}{
		{name: "截断JSON", doc: `{"schedules":[`},
		{name: "空白文件不是legacy", doc: " \n\t"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			before := []byte(tt.doc)
			p := writeStoreFixture(t, dir, "s", tt.doc)
			if _, err := (&store{dir: dir}).load("s"); err == nil {
				t.Fatal("损坏或空白 JSON 不得被静默当成空任务表")
			}
			_, err := mk(dir, kindCreate).Exec(ctxWith("s"), json.RawMessage(`{"title":"新任务","prompt":"只读","kind":"every","every_seconds":300}`))
			if err == nil {
				t.Fatal("损坏 JSON 上的 create 必须失败，不能重置原文件")
			}
			after, readErr := os.ReadFile(p)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("损坏 JSON 被覆盖:\n前: %q\n后: %q", before, after)
			}
		})
	}
}
