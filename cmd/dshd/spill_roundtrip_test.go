package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/kernel"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// 大输出被外置之后，Hint 会说「用 read 读全文」。
// 那句话必须是真的：read 要能读到 artifact 路径。
func TestSpilledArtifactIsReadable(t *testing.T) {
	home := t.TempDir()
	wd := t.TempDir()

	k := kernel.New("spilltest")
	defer k.Root().Dispose()
	if _, err := kernel.Assemble(k.Root(), builtinModules(wd, home, 0, nil)); err != nil {
		t.Fatal(err)
	}
	runner, err := kernel.Inject(k.Root(), spi.ToolRunnerKey)
	if err != nil {
		t.Fatal(err)
	}

	// 造一个必然被外置的输出：超过 MaxChars(4000)
	big := strings.Repeat("x", 9000)
	if err := os.WriteFile(filepath.Join(wd, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := runner.Run(context.Background(), "read", json.RawMessage(`{"file_path":"big.txt"}`))
	if err != nil {
		t.Fatal(err)
	}

	// 找指针：优先 Truncated.Pointer，其次 Hint 里提到的路径
	ptr := ""
	if res.Truncated != nil {
		ptr = res.Truncated.Pointer
	}
	if ptr == "" {
		t.Fatalf("9000 字符的输出没被外置（MaxChars 默认 4000）: Summary=%q Hint=%q", res.Summary, res.Hint)
	}
	if !strings.Contains(ptr, "artifacts") {
		t.Fatalf("artifact 应该落在 home/artifacts 下，实际 %q", ptr)
	}

	// 关键：read 能不能读到它
	args, _ := json.Marshal(map[string]string{"file_path": ptr})
	back, err := runner.Run(context.Background(), "read", args)
	if err != nil {
		t.Fatalf("Hint 说能读，实际读不了: %v", err)
	}
	if !strings.Contains(back.Text, "xxxx") {
		t.Fatalf("读回来的内容不对: %.80s", back.Text)
	}
}

// 外置之后，**模型看到的那段文本**必须也是有界的。
// 只把 Data 换掉、Text 还是全文的话，spill 等于没做。
func TestSpillBoundsWhatModelSees(t *testing.T) {
	home := t.TempDir()
	wd := t.TempDir()

	k := kernel.New("spillbound")
	defer k.Root().Dispose()
	if _, err := kernel.Assemble(k.Root(), builtinModules(wd, home, 0, nil)); err != nil {
		t.Fatal(err)
	}
	runner, err := kernel.Inject(k.Root(), spi.ToolRunnerKey)
	if err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("y", 9000)
	if err := os.WriteFile(filepath.Join(wd, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := runner.Run(context.Background(), "read", json.RawMessage(`{"file_path":"big.txt"}`))
	if err != nil {
		t.Fatal(err)
	}

	blocks := session.ProjectResult(res)
	var seen int
	for _, b := range blocks {
		seen += len([]rune(b.Text))
	}
	if seen > 4000 {
		t.Fatalf("模型会看到 %d 字符，而阈值是 4000 —— spill 只换了 Data，没管 Text", seen)
	}
}
