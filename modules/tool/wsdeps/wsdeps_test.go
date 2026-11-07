package wsdeps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 正常路径：这台机器上有 python（本仓开发机就有），返回里必须给出路径与版本，
// 而且**库的键名是包名**（python-docx 而不是 docx），模型照着装包才对得上。
func TestExecReportsPython(t *testing.T) {
	res, err := (&Tool{}).Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Skipf("这台机器没有 python/node/pnpm，跳过：%v", err)
	}
	if res.Text == "" {
		t.Fatalf("Text 不能空——模型看的是它")
	}
	var d deps
	if err := json.Unmarshal([]byte(res.Text), &d); err != nil {
		t.Fatalf("Text 不是合法 JSON：%v", err)
	}
	if !d.Python.OK {
		t.Fatalf("python 应当可用：%s", res.Text)
	}
	if d.Python.Path == "" || d.Python.Version == "" {
		t.Fatalf("路径与版本都要给：%s", res.Text)
	}

	// 解出来的库名必须是**安装名**，不是 import 名。
	for name := range d.Libraries {
		switch name {
		case "docx", "PIL", "pptx":
			t.Fatalf("库名 %q 是 import 名；模型照着它去 pip 会装错包", name)
		}
	}
	// 摘要里要有版本号，否则 TUI 那一行等于没说。
	if !strings.Contains(res.Summary, d.Python.Version) {
		t.Fatalf("摘要没带上 python 版本：%q", res.Summary)
	}
}

// 找不到东西时：**不许编**。整项省略或进 missing，且 Summary 要说实话。
func TestNoFakeVersionsWhenMissing(t *testing.T) {
	res, err := (&Tool{}).Exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Skip("三样都没有的情况下才走这条；本机有 python，跳过")
	}
	var d deps
	_ = json.Unmarshal([]byte(res.Text), &d)

	// node 不存在时 Node.Path 必须为空、Version 也必须为空。
	if d.Node.Path == "" && d.Node.Version != "" {
		t.Fatalf("没有 node 却给了版本号 %q —— 编数据比没数据更糟", d.Node.Version)
	}
	if d.Pnpm.Path == "" && d.Pnpm.Version != "" {
		t.Fatalf("没有 pnpm 却给了版本号 %q", d.Pnpm.Version)
	}
	for _, name := range d.Missing {
		if v, ok := d.Libraries[name]; ok {
			t.Fatalf("%s 在 missing 里却又有版本 %q", name, v)
		}
	}
}

// Caps 必须承认它会起子进程：审批与沙箱不按工具名猜（AGENTS §2）。
func TestCapsDeclareExecutes(t *testing.T) {
	if !(&Tool{}).Caps().Executes {
		t.Fatal("这个工具会起 python/node 子进程，Executes 必须是 true")
	}
}

// 描述长度上限（门禁同一口径：按 rune 数 ≤120）。
func TestDescriptionLength(t *testing.T) {
	n := len([]rune((&Tool{}).Description()))
	if n > 120 {
		t.Fatalf("Description %d 字，超过 120", n)
	}
}
