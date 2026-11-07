package write

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/modules/fs/local"
)

func TestWriteRendersDSHStyle(t *testing.T) {
	dir := t.TempDir()
	fsys, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	tool := &Tool{fs: fsys}

	args, _ := json.Marshal(map[string]any{"file_path": "a.txt", "content": "hello"})
	res, err := tool.Exec(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<path>a.txt</path>", "<type>file</type>", "<content>", "Created file", "</content>"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("缺 %q：\n%s", want, res.Text)
		}
	}

	// 覆盖已有文件 → Updated file
	args, _ = json.Marshal(map[string]any{"file_path": "a.txt", "content": "hello2"})
	res, err = tool.Exec(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Updated file") {
		t.Fatalf("覆盖已存在文件应显示 Updated file：\n%s", res.Text)
	}
}
