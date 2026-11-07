package unlag

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"dshgo/internal/spi"
)

// 同一个 request_id 只生效一次——这是这个工具唯一真正重要的语义。
func TestSameRequestIDOnlyOnce(t *testing.T) {
	tool := &Tool{st: &store{dir: t.TempDir()}}
	ctx := spi.WithSession(context.Background(), "s1")

	r1, err := tool.Exec(ctx, json.RawMessage(`{"request_id":"demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Data.(map[string]any)["refreshed"] != true {
		t.Fatalf("第一次应当是 refreshed=true，拿到 %+v", r1.Data)
	}

	r2, err := tool.Exec(ctx, json.RawMessage(`{"request_id":"demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Data.(map[string]any)["refreshed"] != false {
		t.Fatalf("第二次应当是 refreshed=false，拿到 %+v", r2.Data)
	}
}

// 不传 request_id 就用会话 id：同一个会话反复点，只生效一次。
func TestFallsBackToSessionID(t *testing.T) {
	tool := &Tool{st: &store{dir: t.TempDir()}}
	ctx := spi.WithSession(context.Background(), "s1")
	for i := 0; i < 3; i++ {
		r, err := tool.Exec(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := i == 0
		if r.Data.(map[string]any)["refreshed"] != want {
			t.Fatalf("第 %d 次 refreshed 应当是 %v", i+1, want)
		}
	}
}

// 落盘而不是只放内存：重启之后同一个 request_id 仍然是"已经刷过了"。
func TestPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	ctx := spi.WithSession(context.Background(), "s1")
	if _, err := (&Tool{st: &store{dir: dir}}).Exec(ctx, json.RawMessage(`{"request_id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	r, err := (&Tool{st: &store{dir: filepath.Clean(dir)}}).Exec(ctx, json.RawMessage(`{"request_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Data.(map[string]any)["refreshed"] != false {
		t.Fatal("换一个实例读同一个目录，应当还是已经刷过了")
	}
}

// 不同会话用同一个 request_id 是两回事，不许互相消。
func TestDifferentSessionsAreIndependent(t *testing.T) {
	tool := &Tool{st: &store{dir: t.TempDir()}}
	a, _ := tool.Exec(spi.WithSession(context.Background(), "s1"), json.RawMessage(`{"request_id":"x"}`))
	b, _ := tool.Exec(spi.WithSession(context.Background(), "s2"), json.RawMessage(`{"request_id":"x"}`))
	if a.Data.(map[string]any)["refreshed"] != true || b.Data.(map[string]any)["refreshed"] != true {
		t.Fatal("不同会话的同一个 request_id 应当各自生效")
	}
}
