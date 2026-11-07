package schedule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeVersionFixture(t *testing.T, dir, sessionID, doc string) string {
	t.Helper()
	p := filepath.Join(dir, sessionID+".json")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 历史文件没有 version，也没有 allow_writes：按 legacy 读取，权限零值必须保持只读。
func TestLoadLegacyWithoutVersion(t *testing.T) {
	dir := t.TempDir()
	writeVersionFixture(t, dir, "legacy", `{"schedules":[{"id":"sch-legacy","kind":"every","every_seconds":300,"next_at":"2026-11-03T05:00:00+08:00"}]}`)

	items, err := Load(dir, "legacy")
	if err != nil {
		t.Fatalf("无 version 的历史文件必须可读: %v", err)
	}
	if len(items) != 1 || items[0].ID != "sch-legacy" {
		t.Fatalf("legacy 内容读错: %+v", items)
	}
	if items[0].AllowWrites {
		t.Fatal("历史文件缺少 allow_writes 时必须保持 false，不能迁移成可写")
	}
}

func TestLoadCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	writeVersionFixture(t, dir, "current", `{"version":1,"schedules":[{"id":"sch-current","kind":"daily","daily_at":"09:30","next_at":"2026-11-04T09:30:00+08:00"}]}`)

	items, err := Load(dir, "current")
	if err != nil {
		t.Fatalf("当前 version=1 必须可读: %v", err)
	}
	if len(items) != 1 || items[0].ID != "sch-current" {
		t.Fatalf("当前版本内容读错: %+v", items)
	}
}

func TestLoadRejectsUnsupportedVersions(t *testing.T) {
	for _, version := range []int{0, 999} {
		t.Run(fmt.Sprintf("version=%d", version), func(t *testing.T) {
			dir := t.TempDir()
			writeVersionFixture(t, dir, "unsupported", fmt.Sprintf(`{"version":%d,"schedules":[]}`, version))
			if _, err := Load(dir, "unsupported"); err == nil {
				t.Fatalf("version=%d 不在兼容白名单，必须 fail-loud", version)
			}
		})
	}
}

// 高层写路径必须先load成功；版本不认识时连Advance也不能覆盖原文件。
func TestAdvanceUnsupportedVersionPreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	before := []byte(`{"version":999,"schedules":[{"id":"sch-future","kind":"every","every_seconds":300,"next_at":"2026-11-03T05:00:00+08:00"}]}`)
	p := writeVersionFixture(t, dir, "future", string(before))
	dues := []Due{{Item: Item{ID: "sch-future"}, Next: at("2026-11-03T05:05:00+08:00")}}

	n, err := Advance(dir, "future", dues, at("2026-11-03T05:00:00+08:00"))
	if err == nil {
		t.Error("未来版本必须拒绝推进")
	}
	if n != 0 {
		t.Errorf("未来版本推进条数 = %d，期望 0", n)
	}
	after, readErr := os.ReadFile(p)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Errorf("拒绝未来版本后原文件被覆盖:\n前: %s\n后: %s", before, after)
	}
}

// legacy只在下一次真实save时升级；写回的永远是当前版本，且不放大旧权限。
func TestAdvanceLegacyWritesCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	writeVersionFixture(t, dir, "upgrade", `{"schedules":[{"id":"sch-legacy","kind":"every","every_seconds":300,"next_at":"2026-11-03T05:00:00+08:00"}]}`)
	items, err := Load(dir, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	now := at("2026-11-03T05:00:00+08:00")
	dues := DueAt(items, now)
	if len(dues) != 1 {
		t.Fatalf("测试前提失败：应有一条到期任务，得到 %+v", dues)
	}
	if n, err := Advance(dir, "upgrade", dues, now); err != nil || n != 1 {
		t.Fatalf("legacy 推进失败: n=%d err=%v", n, err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "upgrade.json"))
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatal(err)
	}
	if header.Version != 1 {
		t.Fatalf("save 后 version=%d，期望当前版本 1；文件=%s", header.Version, raw)
	}
	back, err := Load(dir, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].AllowWrites {
		t.Fatalf("legacy 升级后权限必须仍为 false: %+v", back)
	}
}

func TestCorruptJSONFailsWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	before := []byte(`{"schedules":[`)
	p := writeVersionFixture(t, dir, "corrupt", string(before))
	if _, err := Load(dir, "corrupt"); err == nil {
		t.Fatal("损坏 JSON 不得被当成空任务表")
	}
	dues := []Due{{Item: Item{ID: "sch-corrupt"}, Next: at("2026-11-03T05:05:00+08:00")}}
	if n, err := Advance(dir, "corrupt", dues, at("2026-11-03T05:00:00+08:00")); err == nil || n != 0 {
		t.Fatalf("损坏 JSON 不得推进或重置: n=%d err=%v", n, err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("损坏 JSON 被覆盖:\n前: %q\n后: %q", before, after)
	}
}
