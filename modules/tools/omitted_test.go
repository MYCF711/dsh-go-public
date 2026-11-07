package tools

import (
	"strings"
	"testing"
)

// 带行号的输出被截断时，说明里必须给出**可复制的 offset/limit**。
// "中间省略 N 字符" 这种话模型用不上，它要么再试探一次，要么放弃。
func TestOmittedNoticeGivesExactOffsets(t *testing.T) {
	var head, tail strings.Builder
	for i := 1; i <= 100; i++ {
		head.WriteString(itoa(i) + ": 行内容\n")
	}
	for i := 900; i <= 1000; i++ {
		tail.WriteString(itoa(i) + ": 行内容\n")
	}

	got := omittedNotice(head.String(), tail.String(), 12345, "/tmp/x/artifacts/a.json")
	if !strings.Contains(got, "第 101–899 行") {
		t.Fatalf("要说清省略了哪几行: %q", got)
	}
	if !strings.Contains(got, "read offset=101 limit=799") {
		t.Fatalf("要给出可复制的取回参数: %q", got)
	}
	if !strings.Contains(got, "/tmp/x/artifacts/a.json") {
		t.Fatalf("要带全文指针: %q", got)
	}
}

// 没有行号（例如 bash 的原始输出）时退回按字符说明，不能瞎编行号。
func TestOmittedNoticeFallsBackWithoutLineNumbers(t *testing.T) {
	got := omittedNotice("첫 줄\n둘째 줄\n", "끝 줄\n", 999, "/p")
	if strings.Contains(got, "行已省略") {
		t.Fatalf("没有行号就不该说行: %q", got)
	}
	if !strings.Contains(got, "999") || !strings.Contains(got, "/p") {
		t.Fatalf("要保留字符数与指针: %q", got)
	}
}
