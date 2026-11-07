package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/kernel"
)

const sample = `[profile]
name = "test"

[[module]]
id      = "tool-a"
backend = "static"

[[module]]
id      = "tool-b"
backend = "static"

[[module]]
id      = "tool-c"
backend = "static"
enabled = false
`

func newTool(t *testing.T) (*Tool, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "dsh.toml")
	if err := os.WriteFile(p, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Tool{manifest: p}, p
}

func exec(t *testing.T, tl *Tool, raw string) (string, error) {
	t.Helper()
	res, err := tl.Exec(context.Background(), json.RawMessage(raw))
	return res.Summary + "|" + res.Text, err
}

// list 要如实报出每个模块的开合状态。
func TestListShowsState(t *testing.T) {
	tl, _ := newTool(t)
	out, err := exec(t, tl, `{"action":"list"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "tool-a") || !strings.Contains(out, "tool-c") {
		t.Fatalf("没列出模块：%s", out)
	}
	if !strings.Contains(out, "off tool-c") {
		t.Fatalf("tool-c 是关着的，要标出来：%s", out)
	}
}

// disable 之后文件里必须真的多了 enabled = false，而且还能被 kernel 解析。
func TestDisableWritesAndStaysParseable(t *testing.T) {
	tl, p := newTool(t)
	if _, err := exec(t, tl, `{"action":"disable","id":"tool-a"}`); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "enabled = false") {
		t.Fatalf("没写进 enabled=false：%s", body)
	}
	if _, err := kernel.LoadManifest(p); err != nil {
		t.Fatalf("改完的 manifest 解析不过：%v", err)
	}
	// 再列一次：tool-a 应当是关的
	out, _ := exec(t, tl, `{"action":"list"}`)
	if !strings.Contains(out, "off tool-a") {
		t.Fatalf("disable 之后 tool-a 应当是 off：%s", out)
	}
}

// enable 把 enabled 行删掉（回到默认开），并留下 .bak。
func TestEnableRemovesFlagAndBacksUp(t *testing.T) {
	tl, p := newTool(t)
	if _, err := exec(t, tl, `{"action":"enable","id":"tool-c"}`); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	if strings.Contains(string(body), "enabled = false") {
		t.Fatalf("enable 之后不该还有 enabled=false：%s", body)
	}
	if _, err := os.Stat(p + ".bak"); err != nil {
		t.Fatal("没留备份：出事就没法回滚")
	}
}

// 不存在的 id 必须拒绝，并列出可选的——否则模型下一步还是瞎猜。
func TestUnknownIDIsRefused(t *testing.T) {
	tl, p := newTool(t)
	_, err := exec(t, tl, `{"action":"disable","id":"tool-zzz"}`)
	if err == nil {
		t.Fatal("不存在的 id 必须报错")
	}
	body, _ := os.ReadFile(p)
	if string(body) != sample {
		t.Fatal("失败的调用不该改动文件")
	}
}

// 重复操作要幂等：已经关着的再关一次，不许重复插入 enabled 行。
func TestDisableIsIdempotent(t *testing.T) {
	tl, p := newTool(t)
	if _, err := exec(t, tl, `{"action":"disable","id":"tool-a"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(t, tl, `{"action":"disable","id":"tool-a"}`); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	if n := strings.Count(string(body), "enabled = false"); n != 2 {
		// tool-c 本来就有一条，tool-a 只该多一条
		t.Fatalf("enabled=false 出现 %d 次，说明重复插入了：%s", n, body)
	}
}

// 结果里必须写明"下次启动生效"——本仓没有热重载，说已经生效是骗人。
func TestSaysRestartRequired(t *testing.T) {
	tl, _ := newTool(t)
	res, err := tl.Exec(context.Background(), json.RawMessage(`{"action":"disable","id":"tool-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "重启") {
		t.Fatalf("要说明下次启动生效：%s", res.Text)
	}
	m, _ := res.Data.(map[string]any)
	if m["restartRequired"] != true {
		t.Fatalf("Data.restartRequired 应当为 true：%v", m)
	}
}

// Caps：会改 dsh.toml，必须按写报（审批要能看到）。
func TestCapsDeclareWrites(t *testing.T) {
	if !(&Tool{}).Caps().Writes {
		t.Fatal("plugin 会改 manifest，Caps.Writes 必须为真")
	}
}

// enabled 行要紧挨着本块的内容，不能落在空行后面——
// 那样读起来像下一块的配置，人改文件时会看错。
func TestDisablePutsFlagInsideBlock(t *testing.T) {
	tl, p := newTool(t)
	if _, err := exec(t, tl, `{"action":"disable","id":"tool-a"}`); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	lines := strings.Split(string(body), "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "enabled = false" && i > 0 && strings.TrimSpace(lines[i-1]) == "backend = \"static\"" {
			return // 紧跟在 backend 后面，正确
		}
	}
	t.Fatalf("enabled=false 没紧跟在块内容后面：\n%s", body)
}

// 没有 manifest 不是错误：宿主没有白名单时就是"全开"，这是合法配置。
//
// 真机上（tool-live 探针用一个空 home）就是这种情况：
// 以前它返回 ERR，看起来像"插件系统坏了"，其实只是这台机器没建过这个文件。
func TestMissingManifestIsNotAnError(t *testing.T) {
	tl := &Tool{manifest: filepath.Join(t.TempDir(), "no-such.toml")}
	res, err := tl.Exec(context.Background(), json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("没有 manifest 不该报错：%v", err)
	}
	if !strings.Contains(res.Text, "默认全开") && !strings.Contains(res.Summary, "全开") {
		t.Fatalf("要说清这是'默认全开'：%s / %s", res.Summary, res.Text)
	}
}

// 但 enable/disable 没有 manifest 就没法改，得报错并指路。
func TestToggleWithoutManifestFails(t *testing.T) {
	tl := &Tool{manifest: filepath.Join(t.TempDir(), "no-such.toml")}
	if _, err := tl.Exec(context.Background(), json.RawMessage(`{"action":"disable","id":"tool-a"}`)); err == nil {
		t.Fatal("没有 manifest 时改不了，应当报错")
	}
}
