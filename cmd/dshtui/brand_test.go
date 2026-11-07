package main

import (
	"strings"
	"testing"
)

// `--version` 的输出必须以产品名开头：脚本和人都是先认这个名字再看版本。
func TestVersionLineStartsWithProductName(t *testing.T) {
	line := versionLine()
	if !strings.HasPrefix(line, productName+" ") {
		t.Fatalf("版本行要以 %q 开头：%q", productName, line)
	}
	if version == "" {
		t.Fatal("版本号不能为空——--version 会打印一个没有版本的行")
	}
	if !strings.Contains(line, version) {
		t.Fatalf("版本行里没有版本号 %q：%q", version, line)
	}
}

// 版本号形如 `<上游>-go.<N>`。格式错了不会有人报错，只会在发布件里出现
// 一个和 CHANGELOG 对不上的号。
func TestVersionFormat(t *testing.T) {
	if !strings.Contains(version, "-go.") {
		t.Fatalf("版本号应形如 0.2.1-go.30，得到 %q", version)
	}
}

// `--version` 是**只读**的：返回 0、只写 stdout、不碰 stderr、不连宿主。
//
// 这条是真判据而不是形式：以前没有这个开关，任何想读版本号的脚本都得
// 起一遍界面（等于要一个终端、一个宿主、一个 token）。run 拆出来之后，
// "没连宿主"是可以断言的——连了会在这里挂住或报错。
func TestVersionFlagIsReadOnly(t *testing.T) {
	var out, errb strings.Builder
	code := run([]string{"--version"}, &out, &errb)
	if code != 0 {
		t.Fatalf("--version 应返回 0，得到 %d（stderr=%q）", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != versionLine() {
		t.Fatalf("--version 只该打印一行版本：%q", out.String())
	}
	if !strings.Contains(out.String(), productName) {
		t.Fatalf("--version 的输出里要有 %q：%q", productName, out.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("--version 不该往 stderr 写东西：%q", errb.String())
	}
}

// `--help` 第一行 = 产品名 + 版本。以前这里连名字都没有，只有 flag 包的默认 Usage。
func TestUsageFirstLineIsProduct(t *testing.T) {
	var b strings.Builder
	usage(&b)
	first := strings.SplitN(b.String(), "\n", 2)[0]
	if !strings.Contains(first, productName) {
		t.Fatalf("--help 第一行要出现 %q：%q", productName, first)
	}
	if !strings.Contains(first, version) {
		t.Fatalf("--help 第一行要出现版本号：%q", first)
	}
}

// 走真命令行：--help 由 flag 包打印选项表，所以"选项真的在"只能从 run 这层验。
func TestHelpListsFlags(t *testing.T) {
	var out, errb strings.Builder
	code := run([]string{"--help"}, &out, &errb)
	if code != 0 {
		t.Fatalf("--help 应返回 0，得到 %d", code)
	}
	help := out.String() + errb.String()
	if !strings.Contains(help, productName) {
		t.Fatalf("--help 里要有产品名：%q", help)
	}
	for _, want := range []string{"-version", "-addr", "-token", "-send", "-dump-splash"} {
		if !strings.Contains(help, want) {
			t.Fatalf("--help 里应有 %s，实际：%q", want, help)
		}
	}
}

// 标题序列逐字节钉住：OSC 0 用 BEL 收尾。写成 `\x1b]0;x\x1b\\`（ST）也对，
// 但两种混用会让"测试通过而终端不认"，所以只留一种。
func TestOSCTitleBytes(t *testing.T) {
	if got, want := oscTitle(productName), "\x1b]0;DSH TUI GO\x07"; got != want {
		t.Fatalf("标题序列：得到 %q，期望 %q", got, want)
	}
}
