package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/codecs/v4"
)

// doctor 的每一项都必须给出**实测到的东西**，不是"正常/异常"。
// 出问题时人最需要知道"到底哪里、什么值"。
func TestDoctorReportsConcreteDetail(t *testing.T) {
	home := t.TempDir()
	rep := runDoctor(home, "http://127.0.0.1:1/v1")

	if len(rep.Results) < 5 {
		t.Fatalf("应该至少 5 项检查，实际 %d 项", len(rep.Results))
	}
	for _, c := range rep.Results {
		if strings.TrimSpace(c.Detail) == "" {
			t.Fatalf("%s 没给出实测细节", c.Name)
		}
		// 失败必须带下一步
		if !c.OK && !c.Warn && c.Next == "" {
			t.Fatalf("%s 失败了却没给下一步", c.Name)
		}
	}

	var buf bytes.Buffer
	rep.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, "dshd doctor") {
		t.Fatalf("报告要有标题: %q", out)
	}
	// 网关不通时报告里要有 ✗ 和那条 base
	if !strings.Contains(out, "✗") || !strings.Contains(out, "127.0.0.1:1") {
		t.Fatalf("网关不通要在报告里可见: %q", out)
	}
	if !rep.Failed() {
		t.Fatal("网关不通是致命项")
	}
}

func TestDoctorHomeIsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkHomeWritable(f)
	if c.OK {
		t.Fatal("home 是文件时必须报错")
	}
	if !strings.Contains(c.Detail, "文件") || c.Next == "" {
		t.Fatalf("要说清是文件并给下一步: %+v", c)
	}
}

// 尾部半帧是**警告**不是错误：前面的内容照常可用。
func TestDoctorTornSessionIsWarning(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "--p--", "s1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 造一份真实会话再砍掉尾巴
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	w, err := v4.Create(path, v4.Header{Type: "session", Version: 4, ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(v4.Event{Type: "user/message", Data: []byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b[:len(b)-6], 0o644); err != nil {
		t.Fatal(err)
	}

	c := checkSessionsReadable(home)
	if !c.OK || !c.Warn {
		t.Fatalf("尾部半帧应该是「可读但有警告」，实际 %+v", c)
	}
	if !strings.Contains(c.Detail, "半帧") {
		t.Fatalf("要说清是什么问题: %q", c.Detail)
	}
}

// 网关返回 500 时，报告里要出现状态码。
func TestDoctorGatewayStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := checkGateway(srv.URL + "/v1")
	if c.OK {
		t.Fatal("500 必须报错")
	}
	if !strings.Contains(c.Detail, "500") {
		t.Fatalf("要报出状态码: %q", c.Detail)
	}
}

// 网关正常时，报告里要出现模型数——那是"真的连上了"的证据。
func TestDoctorGatewayOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer srv.Close()

	c := checkGateway(srv.URL + "/v1")
	if !c.OK {
		t.Fatalf("应该连上: %+v", c)
	}
	if !strings.Contains(c.Detail, "2 个模型") {
		t.Fatalf("要报出模型数: %q", c.Detail)
	}
}

// 警告要出现在**结尾那行**，而且它自己那行要是 !——
// 结尾说"上面 ! 那行"，那行就不能显示成 ✓。
func TestDoctorWarningIsVisibleInSummary(t *testing.T) {
	rep := doctorReport{Home: "x", Results: []checkResult{
		{Name: "a", OK: true, Detail: "fine"},
		{Name: "c", OK: true, Warn: true, Detail: "尾巴坏了但能读", Next: "不用管"},
	}}
	var buf bytes.Buffer
	rep.Render(&buf)
	out := buf.String()

	if strings.Contains(out, "一切正常") {
		t.Fatalf("有警告时不能打「一切正常」: %q", out)
	}
	if !strings.Contains(out, "值得看一眼") {
		t.Fatalf("结尾要说清有几项值得看: %q", out)
	}
	if !strings.Contains(out, "! c") {
		t.Fatalf("警告那行要用 ! 打头，才能和结尾对上号: %q", out)
	}
	if rep.Failed() {
		t.Fatal("警告不是致命项，Failed 应为 false")
	}
}

// 出事之后第一件事是翻日志。doctor 要能把"哪份日志、最后一条错误原文"指出来，
// 而不是让人自己去找。
func TestDoctorPointsAtLastLogError(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"level":"INFO","msg":"会话开始","session":"s1"}`,
		`{"level":"WARN","msg":"工具调用失败","tool":"read","error":"/x 不存在"}`,
	}
	path := filepath.Join(dir, "dshd-2026-11-03.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := checkRecentLog(home)
	if !c.OK || !c.Warn {
		t.Fatalf("有警告的日志应该是「可读但有警告」: %+v", c)
	}
	if !strings.Contains(c.Detail, "1 条警告") {
		t.Fatalf("要说清几条警告: %q", c.Detail)
	}
	if !strings.Contains(c.Next, "/x 不存在") {
		t.Fatalf("要带出最后一条错误原文: %q", c.Next)
	}
	if !strings.Contains(c.Next, "dshd-2026-11-03.log") {
		t.Fatalf("要指到文件: %q", c.Next)
	}
}

// 没有日志时不该报警——第一次跑本来就没有。
func TestDoctorNoLogYet(t *testing.T) {
	c := checkRecentLog(t.TempDir())
	if !c.OK || c.Warn {
		t.Fatalf("没有日志是正常的: %+v", c)
	}
}

// logs 被一个文件占了：这**不是**"还没有日志"，是"永远写不了"。
// 原来两种情况都说成"还没有日志（跑一次就会写）"，而后者恰恰是出事时最该知道的。
func TestDoctorLogsBlockedByFile(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "logs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := checkRecentLog(home)
	if c.OK {
		t.Fatal("logs 是文件是致命项，不能报 OK")
	}
	if !strings.Contains(c.Detail, "文件") {
		t.Fatalf("要说清是被文件占了: %q", c.Detail)
	}
	if !strings.Contains(c.Next, "删掉") {
		t.Fatalf("要给下一步: %q", c.Next)
	}

	// 打开日志也要给出同样的解释，而不是 mkdir 的原始错误
	if _, err := openDiag(home); err == nil {
		t.Fatal("logs 是文件时 openDiag 必须失败")
	} else if !strings.Contains(err.Error(), "文件") {
		t.Fatalf("openDiag 的错要说得清: %v", err)
	}
}

// 拿不到日志时只警告、不中断——记不了日志不该让活干不下去。
func TestDiagOrWarnDoesNotCrash(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "logs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := diagOrWarn(home); d != nil {
		t.Fatal("拿不到日志应该返回 nil")
	}
}

// 当天的日志文件本身只读（盘满/被改权限）：打开要失败、要能说清，
// 但**不能因此影响干活**——记不了日志不是不干活的理由。
func TestDiagFailsWhenLogFileReadOnly(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dshd-"+time.Now().Format("2006-01-02")+".log")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Skip("这个平台改不了只读属性:", err)
	}
	defer os.Chmod(path, 0o644)

	if d, err := openDiag(home); err == nil {
		d.Close()
		t.Skip("当前用户绕过只读属性，无法模拟")
	} else if !strings.Contains(err.Error(), "日志") {
		t.Fatalf("错误要说清是日志的问题: %v", err)
	}

	// 关键：拿不到日志只是警告，函数返回 nil 让调用方继续跑
	if d := diagOrWarn(home); d != nil {
		t.Fatal("拿不到日志应该返回 nil")
	}
}

// 体检要能提前说出"wasm 模块装不起来"。
//
// 为什么值得单独一条：wasm 装载失败**只在启动时**报一次，跑起来之后就没人看了，
// 而它的失败形态是"模块静默不生效"，跟配置写错一个样。
func TestDoctorChecksWasmModules(t *testing.T) {
	dir := t.TempDir()

	// ① 没有 wasm → 明确说"没有"，不能含糊成"已装载"
	plain := filepath.Join(dir, "plain.toml")
	if err := os.WriteFile(plain, []byte("[[module]]\nid = \"tool-todo\"\nbackend = \"static\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkWasmModules(plain); !c.OK || !strings.Contains(c.Detail, "没有 wasm") {
		t.Fatalf("没有 wasm 时应当如实说，实际 ok=%v detail=%q", c.OK, c.Detail)
	}

	// ② 指向不存在的文件 → 红，且错误里带路径
	broken := filepath.Join(dir, "broken.toml")
	body := "[[module]]\nid = \"wasm-x\"\nbackend = \"wasm\"\npath = \"nope.wasm\"\n"
	if err := os.WriteFile(broken, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkWasmModules(broken)
	if c.OK {
		t.Fatal("装不起来的 wasm 模块必须报红")
	}
	if !strings.Contains(c.Detail, "nope.wasm") {
		t.Fatalf("错误里要带路径：%q", c.Detail)
	}
}

// 体检要说清"这台机器能不能搜"，而不是等模型每调一次失败一次。
//
// 判据来自真语料：web_search 的失败率 91%（61/67），失败原因永远是同一句
// "宿主没有装搜索后端"——那是环境，不是代码。所以这一项**不许报红**：
// 不配搜索照样能用（web_fetch 直取网址），报红会让所有没配的人都以为坏了。
func TestDoctorReportsSearchBackend(t *testing.T) {
	t.Setenv("DSHD_SEARCH_URL", "")
	c := checkSearchBackend()
	if !c.OK {
		t.Fatal("没配搜索后端不该报红：不配也能用，只是搜不了")
	}
	if !strings.Contains(c.Detail, "web_fetch") && !strings.Contains(c.Detail, "搜不了") {
		t.Fatalf("要说清后果与替代：%q", c.Detail)
	}

	t.Setenv("DSHD_SEARCH_URL", "https://searx.example/")
	if c := checkSearchBackend(); !c.OK || !strings.Contains(c.Detail, "searx.example") {
		t.Fatalf("配了就要显示配到哪：%+v", c)
	}
}
