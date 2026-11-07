package edit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/modules/fs/local"
)

// 写前没读 / 读完变了这两类占真实 edit 失败的 85%（454 条样本：58.8% + 26.7%）。
//
// 所以这条提示的判据不是"措辞好听"，而是**它给出的下一步能不能照做**：
// 文件现在长什么样、重试会不会成功、找不到时该怎么办。
func TestStaleEditHintTellsWhatToDo(t *testing.T) {
	dir := t.TempDir()
	fsys, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.txt")

	put := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 情形一：old_string 在新内容里仍然唯一 → 明确说"再调一次就会成功"。
	put("line1\nTARGET\nline3\n")
	h := staleEditHint(fsys, args{FilePath: path, OldString: "TARGET"})
	if !strings.Contains(h, "仍然唯一") || !strings.Contains(h, "再调一次") {
		t.Fatalf("唯一时应当说重试就能成：%q", h)
	}

	// 情形二：找不到了 → 不许建议"重试"，要说"先 read 看现在是什么"。
	put("line1\nline2\n")
	h = staleEditHint(fsys, args{FilePath: path, OldString: "TARGET"})
	if strings.Contains(h, "再调一次") {
		t.Fatalf("找不到时不能建议重试：%q", h)
	}
	if !strings.Contains(h, "找不到") {
		t.Fatalf("要说清找不到：%q", h)
	}

	// 情形三：多处 → 报数字 + 给两种解法。
	put("X\nX\n")
	h = staleEditHint(fsys, args{FilePath: path, OldString: "X"})
	if !strings.Contains(h, "2 处") || !strings.Contains(h, "replace_all") {
		t.Fatalf("多处要报数字并给 replace_all：%q", h)
	}

	// 情形四：文件没了 → 不许建议"重试"。
	h = staleEditHint(fsys, args{FilePath: filepath.Join(dir, "gone.txt"), OldString: "X"})
	if !strings.Contains(h, "读不出来") && !strings.Contains(h, "路径还在") {
		t.Fatalf("读不到时要说清并指向路径：%q", h)
	}
}
