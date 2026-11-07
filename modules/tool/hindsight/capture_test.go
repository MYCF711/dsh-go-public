package hindsight

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 正常路径：写进去的那一页必须能被**同一包的读/搜**找回来。
//
// 这条比"文件存在"强：倡议页的意义就是以后能被搜到，
// 只验落盘成功的话，标签或标题拼错一样绿。
func TestCaptureWritesSearchablePage(t *testing.T) {
	dir := t.TempDir()
	tool := &captureTool{dir: dir}

	res, err := tool.Exec(context.Background(), json.RawMessage(
		`{"title":"统一验证规矩","summary":"把磁盘上那些会自己腐烂的断言挂到一条命令上"}`))
	if err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if res.Summary == "" || res.Data == nil {
		t.Fatalf("要有摘要和结构化数据：%+v", res)
	}

	// 文件真的在盘上
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), suffix) {
		t.Fatalf("应当恰好写一个 .json 页：%v", entries)
	}

	// 用 search 找回来
	searcher := &searchTool{dir: dir}
	out, err := searcher.Exec(context.Background(), json.RawMessage(`{"query":"验证规矩"}`))
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	if !strings.Contains(out.Text, "统一验证规矩") {
		t.Fatalf("刚记的倡议搜不到：%s", out.Text)
	}

	// 标签必须是 initiative：列表/检索靠它分类
	raw, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("页不是合法 JSON：%v", err)
	}
	if len(doc.Tags) != 1 || doc.Tags[0] != "initiative" {
		t.Fatalf("标签应是 [initiative]，实际 %v", doc.Tags)
	}
}

// 缺参数：必须报错，而且**必须带 Hint**（R3：不许空 Result 丢 Hint）。
func TestCaptureRequiresTitleAndSummary(t *testing.T) {
	tool := &captureTool{dir: t.TempDir()}
	for _, body := range []string{
		`{"summary":"只有说明"}`,
		`{"title":"只有标题"}`,
		`{"title":"  ","summary":"标题是空白"}`,
	} {
		res, err := tool.Exec(context.Background(), json.RawMessage(body))
		if err == nil {
			t.Fatalf("%s：应当报错", body)
		}
		if res.Hint == "" {
			t.Fatalf("%s：失败必须带 Hint", body)
		}
	}
}

// Caps：会建目录、写文件，必须声明 Writes（否则 deny 策略下会被静默放行一次写盘）。
func TestCaptureCapsDeclareWrites(t *testing.T) {
	if !(&captureTool{}).Caps().Writes {
		t.Fatal("capture 会写盘，Caps.Writes 必须为真")
	}
}

// 描述长度上限（门禁同一口径：按 rune ≤120）。
func TestCaptureDescriptionLength(t *testing.T) {
	if n := len([]rune((&captureTool{}).Description())); n > 120 {
		t.Fatalf("Description %d 字，超过 120", n)
	}
}
