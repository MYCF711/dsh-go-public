package read

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/modules/fs/local"
)

func TestReadRendersDSHStyle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("第一行\n第二行\n第三行\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	tool := &Tool{fs: fsys}
	args, _ := json.Marshal(map[string]any{"file_path": "a.txt", "limit": 2})
	res, err := tool.Exec(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text == "" {
		t.Fatal("read 应该给出渲染好的 Text")
	}
	for _, want := range []string{"<path>a.txt</path>", "<type>file</type>", "<content>", "1: 第一行", "2: 第二行", "</content>"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("缺 %q：\n%s", want, res.Text)
		}
	}
	// 行号必须和 offset/limit 对齐，读第 2 行时前缀是 2。
	args, _ = json.Marshal(map[string]any{"file_path": "a.txt", "offset": 2, "limit": 1})
	res, err = tool.Exec(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "2: 第二行") {
		t.Fatalf("offset 没生效：\n%s", res.Text)
	}
}
