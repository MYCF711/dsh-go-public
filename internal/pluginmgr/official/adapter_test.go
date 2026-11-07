package official_test

// 官方 pluginManager 方法的**真实状态**端到端测试。
//
// 这里的每一个断言都读真实磁盘：临时 home 下的 `pluginmgr` 根、真实
// `dsh.local-plugin@1` 包目录、真实 `state.json`、真实事务日志。适配器不
// 伪造任何状态：装/卸/启停都走 `internal/pluginmgr` 的既有事务，重开管理器
// 之后状态必须还在；A/B 两个 home 各管各的。
//
// 走的是 `/api` 的同一个入口（`Server.ServeOfficialPluginManagerAPI`），
// 因此参数解析、四字段错误、结果形状都在链上。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/pluginmgr"
	"dshgo/internal/pluginmgr/official"
	"dshgo/internal/transport"
)

// packageFixture 是一个可安装的私有包目录（可选带 README.md）。
func packageFixture(t *testing.T, dir, id, version string, readme []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("private test package " + id + " " + version)
	sum := sha256.Sum256(payload)
	files := map[string]string{"plugin.wasm": hex.EncodeToString(sum[:])}
	if readme != nil {
		readmeSum := sha256.Sum256(readme)
		files["README.md"] = hex.EncodeToString(readmeSum[:])
	}
	manifest := map[string]any{
		"schema": "dsh.local-plugin@1", "id": id, "version": version,
		"kind": "wasm", "entry": "plugin.wasm", "files": files,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if readme != nil {
		if err := os.WriteFile(filepath.Join(dir, "README.md"), readme, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type fixture struct {
	home    string
	root    string
	manager *pluginmgr.Manager
	server  *transport.Server
}

func newFixture(t *testing.T, home string) *fixture {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	root := filepath.Join(home, "plugins")
	manager, err := pluginmgr.Open(root)
	if err != nil {
		t.Fatalf("pluginmgr.Open: %v", err)
	}
	srv, err := transport.NewServer(transport.Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.AttachOfficialPluginManagerAPI(official.New(manager))
	t.Cleanup(srv.DetachOfficialPluginManagerAPI)
	return &fixture{home: home, root: root, manager: manager, server: srv}
}

// reopen 模拟"重开 server"：同一个 home 重新 Open + 重新挂接。
func (f *fixture) reopen(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, f.home)
}

func (f *fixture) call(t *testing.T, method string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case nil:
		raw = nil
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		raw = encoded
	}
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	f.server.ServeOfficialPluginManagerAPI(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON：%q", method, rec.Body.String())
		}
	}
	return rec.Code, out
}

func (f *fixture) result(t *testing.T, method string, body any) map[string]any {
	t.Helper()
	code, out := f.call(t, method, body)
	if code != http.StatusOK {
		t.Fatalf("%s 状态 %d，期望 200：%v", method, code, out)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s 成功信封缺 result：%v", method, out)
	}
	return result
}

func (f *fixture) fourField(t *testing.T, method string, body any, wantStatus int, wantCode string) map[string]any {
	t.Helper()
	code, out := f.call(t, method, body)
	if code != wantStatus {
		t.Fatalf("%s 状态 %d，期望 %d：%v", method, code, wantStatus, out)
	}
	obj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s 缺四字段错误：%v", method, out)
	}
	for _, key := range []string{"code", "message", "hint", "retryable"} {
		if _, has := obj[key]; !has {
			t.Fatalf("%s 错误缺 %s：%v", method, key, obj)
		}
	}
	if obj["code"] != wantCode {
		t.Fatalf("%s 错误码 %v，期望 %s：%v", method, obj["code"], wantCode, obj)
	}
	if msg, _ := obj["message"].(string); strings.TrimSpace(msg) == "" {
		t.Fatalf("%s 错误消息是空的", method)
	}
	if _, has := out["result"]; has {
		t.Fatalf("%s 错误信封里出现了 result", method)
	}
	return obj
}

func installedIDs(t *testing.T, m *pluginmgr.Manager) map[string]pluginmgr.PluginInfo {
	t.Helper()
	list, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := map[string]pluginmgr.PluginInfo{}
	for _, info := range list {
		out[info.ID] = info
	}
	return out
}

// inspect 只做真实本地包校验，并把"没有 bundle patch"如实报成 bundle:false。
func TestInspectRealPackageAndRefusals(t *testing.T) {
	f := newFixture(t, "")
	pkg := packageFixture(t, filepath.Join(f.home, "private", "v1"), "acme.audit", "1.0.0", nil)

	result := f.result(t, "pluginManager.inspect", map[string]any{"spec": pkg})
	if result["status"] != "accepted" || result["kind"] != "path" {
		t.Fatalf("inspect 结果不对：%v", result)
	}
	if result["name"] != "acme.audit" || result["version"] != "1.0.0" {
		t.Fatalf("inspect 身份不对：%v", result)
	}
	if result["bundle"] != false {
		t.Fatalf("本仓包没有 dsh.bundle patch，bundle 必须是 false：%v", result)
	}
	if registry, has := result["registry"]; !has || registry != nil {
		t.Fatalf("registry 必须显式 null：%v", result)
	}

	// 目录不存在 → refused/not-a-package（不是 200 "accepted"）。
	result = f.result(t, "pluginManager.inspect", map[string]any{"spec": filepath.Join(f.home, "nope")})
	if result["status"] != "refused" || result["problem"] != "not-a-package" {
		t.Fatalf("缺目录的 inspect 结果不对：%v", result)
	}

	// 校验失败的真实包（manifest 缺 hash）→ refused，理由来自真实校验。
	broken := filepath.Join(f.home, "private", "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.json"), []byte(`{"schema":"dsh.local-plugin@1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result = f.result(t, "pluginManager.inspect", map[string]any{"spec": broken})
	if result["status"] != "refused" || result["problem"] != "not-a-package" {
		t.Fatalf("坏包的 inspect 结果不对：%v", result)
	}

	// 装过之后再 inspect → refused/already-installed（真实状态）。
	if _, err := f.manager.Install(pkg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	result = f.result(t, "pluginManager.inspect", map[string]any{"spec": pkg})
	if result["status"] != "refused" || result["problem"] != "already-installed" {
		t.Fatalf("已安装的 inspect 结果不对：%v", result)
	}
}

// 非本地路径形态（registry/name、git、tarball）需要 registry/pnpm/git 客户端，
// 本仓一个都没有 → 必须是四字段 501，不许假装 accepted。
func TestInspectAndInstallRefuseUnsupportedSpecForms(t *testing.T) {
	f := newFixture(t, "")
	for _, spec := range []string{
		"acme.bundle",
		"acme.bundle@1.2.3",
		"@scope/bundle",
		"git+https://example.com/acme.git",
		"https://example.com/acme-1.0.0.tgz",
	} {
		obj := f.fourField(t, "pluginManager.inspect", map[string]any{"spec": spec},
			http.StatusNotImplemented, "not_implemented")
		if msg, _ := obj["message"].(string); !strings.Contains(msg, "registry") && !strings.Contains(msg, "pnpm") {
			t.Fatalf("%q 的 501 没说清缺哪个能力：%q", spec, msg)
		}
		f.fourField(t, "pluginManager.installBundle", map[string]any{"spec": spec},
			http.StatusNotImplemented, "not_implemented")
	}
	if got := installedIDs(t, f.manager); len(got) != 0 {
		t.Fatalf("被拒的 spec 居然装了东西：%+v", got)
	}
}

// 真实的坏包不能被当成"装好了"：校验失败 → failed/invalid-spec，状态不变。
func TestInstallBundleInvalidPackageFailsLoud(t *testing.T) {
	f := newFixture(t, "")
	broken := filepath.Join(f.home, "private", "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.json"), []byte(`{"schema":"dsh.local-plugin@1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result := f.result(t, "pluginManager.installBundle", map[string]any{"spec": broken})
	if result["changed"] != false || result["application"] != "failed" {
		t.Fatalf("坏包的结果不对：%v", result)
	}
	errObj, _ := result["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid-spec" {
		t.Fatalf("坏包的错误码不对：%v", result)
	}
	if diag, _ := errObj["diagnostic"].(string); strings.TrimSpace(diag) == "" {
		t.Fatalf("坏包的错误没有诊断信息（界面无法说明为什么）：%v", result)
	}
	if got := installedIDs(t, f.manager); len(got) != 0 {
		t.Fatalf("坏包居然装了东西：%+v", got)
	}
}

func TestInstallBundleIsRealAndSurvivesRestart(t *testing.T) {
	f := newFixture(t, "")
	pkg := packageFixture(t, filepath.Join(f.home, "private", "v1"), "acme.audit", "1.0.0", nil)

	result := f.result(t, "pluginManager.installBundle", map[string]any{
		"spec": pkg, "options": map[string]any{"enabled": true, "requestId": "req-install-1"},
	})
	if result["changed"] != true || result["application"] != "applied" {
		t.Fatalf("installBundle 结果不对：%v", result)
	}
	if result["stage"] != "install" || result["target"] != pkg {
		t.Fatalf("installBundle 的 stage/target 不对：%v", result)
	}
	if result["enabled"] != true || result["bundle"] != "acme.audit" || result["version"] != "1.0.0" {
		t.Fatalf("installBundle 没回报真实安装结果：%v", result)
	}
	warnings, _ := result["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatalf("必须显式警告「宿主没有运行时加载器」：%v", result)
	}

	info := installedIDs(t, f.manager)["acme.audit"]
	if info.Version != "1.0.0" || !info.Enabled || info.Source != pkg {
		t.Fatalf("真实状态不对：%+v", info)
	}
	// 重开（同一 home 的新 server）后仍在。
	reopened := f.reopen(t)
	info = installedIDs(t, reopened.manager)["acme.audit"]
	if info.Version != "1.0.0" || !info.Enabled {
		t.Fatalf("重开后状态丢了：%+v", info)
	}
	// 重复安装 → 官方码 already-installed，且不改状态。
	result = reopened.result(t, "pluginManager.installBundle", map[string]any{"spec": pkg})
	if result["application"] != "failed" || result["changed"] != false {
		t.Fatalf("重复安装的结果不对：%v", result)
	}
	errObj, _ := result["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "already-installed" {
		t.Fatalf("重复安装的错误码不对：%v", result)
	}
	if info := installedIDs(t, reopened.manager)["acme.audit"]; info.Version != "1.0.0" {
		t.Fatalf("重复安装改了真实状态：%+v", info)
	}
}

// A/B 两个 home 不串：各自的包、启用位、存在性都独立。
func TestHomesAreIsolated(t *testing.T) {
	a := newFixture(t, "")
	b := newFixture(t, "")
	pkg := packageFixture(t, filepath.Join(a.home, "private", "v1"), "acme.audit", "1.0.0", nil)

	if result := a.result(t, "pluginManager.installBundle", map[string]any{"spec": pkg}); result["changed"] != true {
		t.Fatalf("A 装失败：%v", result)
	}
	if got := installedIDs(t, a.manager); len(got) != 1 {
		t.Fatalf("A 的状态不对：%+v", got)
	}
	if got := installedIDs(t, b.manager); len(got) != 0 {
		t.Fatalf("A 的安装串到了 B：%+v", got)
	}
	// B 里同一个包仍是"可安装"的：A 的已安装不该影响 B 的 inspect。
	if result := b.result(t, "pluginManager.inspect", map[string]any{"spec": pkg}); result["status"] != "accepted" {
		t.Fatalf("B 的 inspect 被 A 影响：%v", result)
	}
	// B 卸 A 的东西必须失败（真实不存在的包），A 不受影响。
	result := b.result(t, "pluginManager.removeBundle", map[string]any{"name": "acme.audit"})
	if result["changed"] != false {
		t.Fatalf("B 卸掉了不存在的包：%v", result)
	}
	if got := installedIDs(t, a.manager); len(got) != 1 {
		t.Fatalf("B 的操作影响了 A：%+v", got)
	}
}

func TestRemoveBundleIsReal(t *testing.T) {
	f := newFixture(t, "")
	pkg := packageFixture(t, filepath.Join(f.home, "private", "v1"), "acme.audit", "1.0.0", nil)
	f.result(t, "pluginManager.installBundle", map[string]any{"spec": pkg})

	result := f.result(t, "pluginManager.removeBundle", map[string]any{"name": "acme.audit"})
	if result["changed"] != true || result["stage"] != "remove" || result["application"] != "applied" {
		t.Fatalf("removeBundle 结果不对：%v", result)
	}
	if _, err := os.Stat(filepath.Join(f.root, "packages", "acme.audit", "1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("卸载后包目录还在：%v", err)
	}
	reopened := f.reopen(t)
	if got := installedIDs(t, reopened.manager); len(got) != 0 {
		t.Fatalf("重开后卸载没生效：%+v", got)
	}
	// 再卸一次：真实的"不可移除"。
	result = reopened.result(t, "pluginManager.removeBundle", map[string]any{"name": "acme.audit"})
	if result["changed"] != false {
		t.Fatalf("重复卸载报成功：%v", result)
	}
	errObj, _ := result["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "not-removable" {
		t.Fatalf("重复卸载的错误码不对：%v", result)
	}
}

// 本仓一包一插件：setBundleEnabled 与 setPluginEnabled 写的是同一个持久化启用位。
func TestEnableBitsAreRealAndPersisted(t *testing.T) {
	f := newFixture(t, "")
	pkg := packageFixture(t, filepath.Join(f.home, "private", "v1"), "acme.audit", "1.0.0", nil)
	f.result(t, "pluginManager.installBundle", map[string]any{"spec": pkg})

	// 官方 installBundle 的 enabled 缺省是 true，所以装完就是启用的。
	if info := installedIDs(t, f.manager)["acme.audit"]; !info.Enabled {
		t.Fatalf("installBundle 的 enabled 缺省应为 true：%+v", info)
	}
	result := f.result(t, "pluginManager.setPluginEnabled", map[string]any{"id": "acme.audit", "enabled": false})
	if result["changed"] != true || result["enabled"] != false || result["stage"] != "enable" {
		t.Fatalf("setPluginEnabled 结果不对：%v", result)
	}
	if info := installedIDs(t, f.manager)["acme.audit"]; info.Enabled {
		t.Fatalf("禁用没落到真实状态：%+v", info)
	}
	// 同值再设一次 → changed:false（真实状态没动）。
	result = f.result(t, "pluginManager.setPluginEnabled", map[string]any{"id": "acme.audit", "enabled": false})
	if result["changed"] != false {
		t.Fatalf("同值设置仍报 changed：%v", result)
	}
	// setBundleEnabled 写的是同一位（本仓一包一插件）。
	result = f.result(t, "pluginManager.setBundleEnabled", map[string]any{"name": "acme.audit", "enabled": true})
	if result["changed"] != true || result["enabled"] != true {
		t.Fatalf("setBundleEnabled 结果不对：%v", result)
	}
	if info := installedIDs(t, f.manager)["acme.audit"]; !info.Enabled {
		t.Fatalf("启用没落到真实状态：%+v", info)
	}
	reopened := f.reopen(t)
	if info := installedIDs(t, reopened.manager)["acme.audit"]; !info.Enabled {
		t.Fatalf("启用没持久化：%+v", info)
	}
	// 未知 id：官方码 unknown-plugin，不许改写别的包。
	result = reopened.result(t, "pluginManager.setPluginEnabled", map[string]any{"id": "nope.gone", "enabled": true})
	if result["changed"] != false {
		t.Fatalf("未知 id 报成功了：%v", result)
	}
	errObj, _ := result["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "unknown-plugin" {
		t.Fatalf("未知 id 的错误码不对：%v", result)
	}
}

// updateBundle 只在"目标版本已经在本机 store 里"时是真实可做的；否则没有下载源。
func TestUpdateBundleToRetainedVersionOnly(t *testing.T) {
	f := newFixture(t, "")
	v2 := packageFixture(t, filepath.Join(f.home, "private", "v2"), "acme.audit", "2.0.0", nil)
	v3 := packageFixture(t, filepath.Join(f.home, "private", "v3"), "acme.audit", "3.0.0", nil)
	if _, err := f.manager.Install(v2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Update("acme.audit", v3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Restore("acme.audit", "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if info := installedIDs(t, f.manager)["acme.audit"]; info.Version != "2.0.0" {
		t.Fatalf("准备状态不对：%+v", info)
	}

	result := f.result(t, "pluginManager.updateBundle", map[string]any{
		"name": "acme.audit", "version": "3.0.0", "options": map[string]any{"requestId": "req-update"},
	})
	if result["from"] != "2.0.0" || result["to"] != "3.0.0" || result["step"] != "install" {
		t.Fatalf("updateBundle 的三段式不对：%v", result)
	}
	inner, _ := result["result"].(map[string]any)
	if inner == nil || inner["changed"] != true || inner["application"] != "applied" {
		t.Fatalf("updateBundle.result 不对：%v", result)
	}
	if info := installedIDs(t, f.manager)["acme.audit"]; info.Version != "3.0.0" {
		t.Fatalf("更新没落到真实状态：%+v", info)
	}
	reopened := f.reopen(t)
	if info := installedIDs(t, reopened.manager)["acme.audit"]; info.Version != "3.0.0" {
		t.Fatalf("更新没持久化：%+v", info)
	}

	// 本机没有这个版本 → 无下载源，必须 501，且状态不变。
	f.fourField(t, "pluginManager.updateBundle", map[string]any{
		"name": "acme.audit", "version": "9.9.9",
	}, http.StatusNotImplemented, "not_implemented")
	if info := installedIDs(t, reopened.manager)["acme.audit"]; info.Version != "3.0.0" {
		t.Fatalf("失败的更新改了状态：%+v", info)
	}

	// 目标版本不新于当前 → invalid-spec（官方的 check 阶段拒绝），不改状态。
	result = reopened.result(t, "pluginManager.updateBundle", map[string]any{
		"name": "acme.audit", "version": "2.0.0",
	})
	if result["step"] != "check" {
		t.Fatalf("旧版本的 update 应在 check 阶段被拒：%v", result)
	}
	inner, _ = result["result"].(map[string]any)
	errObj, _ := inner["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid-spec" || inner["changed"] != false {
		t.Fatalf("旧版本的 update 结果不对：%v", result)
	}

	// 未知包 → 同样是 check 阶段的 invalid-spec，不是 200 的"成功"。
	result = reopened.result(t, "pluginManager.updateBundle", map[string]any{
		"name": "nope.gone", "version": "1.0.0",
	})
	inner, _ = result["result"].(map[string]any)
	errObj, _ = inner["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid-spec" {
		t.Fatalf("未知包的 update 结果不对：%v", result)
	}
}

// listVersions 的真实答案：本仓包来自本地目录，因此永远是 not-updatable。
func TestListVersionsRefusesNotUpdatable(t *testing.T) {
	f := newFixture(t, "")
	pkg := packageFixture(t, filepath.Join(f.home, "private", "v1"), "acme.audit", "1.0.0", nil)
	f.result(t, "pluginManager.installBundle", map[string]any{"spec": pkg})

	result := f.result(t, "pluginManager.listVersions", map[string]any{"name": "acme.audit"})
	if result["status"] != "refused" || result["problem"] != "not-updatable" {
		t.Fatalf("listVersions 结果不对：%v", result)
	}
	reason, _ := result["reason"].(string)
	if !strings.Contains(reason, "acme.audit") {
		t.Fatalf("拒绝理由没点名包：%q", reason)
	}
	if attempts, ok := result["attempts"].([]any); !ok || len(attempts) != 0 {
		t.Fatalf("attempts 必须是空数组：%v", result)
	}
	// 未知包同样 refused（不查 registry，就没有版本可列）。
	result = f.result(t, "pluginManager.listVersions", map[string]any{"name": "nope.gone"})
	if result["status"] != "refused" || result["problem"] != "not-updatable" {
		t.Fatalf("未知包的 listVersions 结果不对：%v", result)
	}
	// 指定 registry = 要问网络 → 501（本仓没有 registry 客户端）。
	f.fourField(t, "pluginManager.listVersions", map[string]any{
		"name": "acme.audit", "options": map[string]any{"registry": "https://registry.example"},
	}, http.StatusNotImplemented, "not_implemented")
}

// readBundleDetail 读的是已安装包里的真实 README.md。
func TestReadBundleDetailReadsRealReadme(t *testing.T) {
	f := newFixture(t, "")
	readme := []byte("\ufeff---\ntitle: 内网审计\n---\n# 内网审计\n\n它真的会读这个文件。\n")
	withReadme := packageFixture(t, filepath.Join(f.home, "private", "v1"), "acme.audit", "1.0.0", readme)
	without := packageFixture(t, filepath.Join(f.home, "private", "v2"), "acme.plain", "1.0.0", nil)
	f.result(t, "pluginManager.installBundle", map[string]any{"spec": withReadme})
	f.result(t, "pluginManager.installBundle", map[string]any{"spec": without})

	result := f.result(t, "pluginManager.readBundleDetail", map[string]any{
		"name": "acme.audit", "language": "zh-CN",
	})
	if result["status"] != "found" || result["origin"] != "readme" || result["path"] != "README.md" {
		t.Fatalf("readBundleDetail 结果不对：%v", result)
	}
	markdown, _ := result["markdown"].(string)
	if strings.HasPrefix(markdown, "\ufeff") || strings.HasPrefix(markdown, "---") {
		t.Fatalf("BOM/front matter 没剥掉：%q", markdown)
	}
	if !strings.Contains(markdown, "# 内网审计") {
		t.Fatalf("README 内容不对：%q", markdown)
	}

	// 没有 README 的包 → none（不是错误，也不是空 markdown）。
	result = f.result(t, "pluginManager.readBundleDetail", map[string]any{
		"name": "acme.plain", "language": "en",
	})
	if result["status"] != "none" || len(result) != 1 {
		t.Fatalf("无 README 的结果不对：%v", result)
	}

	// 未知包 → 四字段 404（官方抛 unknown-bundle）。
	f.fourField(t, "pluginManager.readBundleDetail", map[string]any{
		"name": "nope.gone", "language": "zh-CN",
	}, http.StatusNotFound, "not_found")
}

// waitForInstall / cancelInstall 映射的是 pluginmgr 的真实安装作业：
// 完成的作业不保留结果（官方 null 语义），没有 id → not-running。
func TestWaitAndCancelMapRealJobState(t *testing.T) {
	f := newFixture(t, "")
	pkg := packageFixture(t, filepath.Join(f.home, "private", "v1"), "acme.audit", "1.0.0", nil)
	f.result(t, "pluginManager.installBundle", map[string]any{
		"spec": pkg, "options": map[string]any{"requestId": "req-done"},
	})

	result := f.result(t, "pluginManager.waitForInstall", map[string]any{"requestId": "req-done"})
	if result["active"] != false {
		t.Fatalf("已完成的作业不该仍 active：%v", result)
	}
	result = f.result(t, "pluginManager.waitForInstall", map[string]any{"requestId": "never"})
	if result["active"] != false {
		t.Fatalf("未知 requestId 的 wait 结果不对：%v", result)
	}
	result = f.result(t, "pluginManager.cancelInstall", map[string]any{"requestId": "req-done"})
	if result["status"] != "not-running" {
		t.Fatalf("已完成作业的 cancel 结果不对：%v", result)
	}
	result = f.result(t, "pluginManager.cancelInstall", map[string]any{"requestId": "never"})
	if result["status"] != "not-running" {
		t.Fatalf("未知 requestId 的 cancel 结果不对：%v", result)
	}
}

// 版本豁免：真实持久化 + 重开可读 + 撤销，全部读真实磁盘。
func TestSetVersionExemptionPersistsAndReopens(t *testing.T) {
	f := newFixture(t, "")
	result := f.result(t, "pluginManager.setVersionExemption", map[string]any{
		"packageVersion": "acme.future@1.0.0", "runtimeVersion": pluginmgr.HostRuntimeVersion,
		"enabled": true, "acceptRisk": true,
	})
	if result["changed"] != true || result["application"] != "applied" || result["stage"] != "enable" {
		t.Fatalf("授权结果不对：%v", result)
	}
	warnings, _ := result["warnings"].([]any)
	joined := ""
	for _, warning := range warnings {
		joined += warning.(string) + "\n"
	}
	if !strings.Contains(joined, "compatibility.json") {
		t.Fatalf("必须说清豁免落在哪个文件：%v", warnings)
	}

	// 文件落在管理器 root 下，形状是官方那个裸对象。
	raw, err := os.ReadFile(filepath.Join(f.root, "compatibility.json"))
	if err != nil {
		t.Fatalf("豁免文件必须真的写出来：%v", err)
	}
	var shape map[string][]string
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("豁免文件必须是 {name@version: [dsh 版本]}：%v\n%s", err, raw)
	}
	if versions := shape["acme.future@1.0.0"]; len(versions) != 1 || versions[0] != pluginmgr.HostRuntimeVersion {
		t.Fatalf("文件内容不对：%v", shape)
	}

	// 重开：同一个 home 重新 Open，仍读得到。
	reopened := f.reopen(t)
	exemptions, _ := reopened.manager.VersionExemptions()
	if versions := exemptions["acme.future@1.0.0"]; len(versions) != 1 || versions[0] != pluginmgr.HostRuntimeVersion {
		t.Fatalf("重开之后读不到豁免：%v", exemptions)
	}

	// 撤销：通过 /api 撤销之后键真的消失。
	revoked := reopened.result(t, "pluginManager.setVersionExemption", map[string]any{
		"packageVersion": "acme.future@1.0.0", "runtimeVersion": pluginmgr.HostRuntimeVersion,
		"enabled": false,
	})
	if revoked["changed"] != true {
		t.Fatalf("撤销必须 changed=true：%v", revoked)
	}
	after, _ := reopened.manager.VersionExemptions()
	if _, ok := after["acme.future@1.0.0"]; ok {
		t.Fatalf("撤销之后不该还有这个键：%v", after)
	}

	// 前置条件走契约层：授权必须 acceptRisk=true，且只能给当前 runtime。
	f.fourField(t, "pluginManager.setVersionExemption", map[string]any{
		"packageVersion": "acme.future@1.0.0", "runtimeVersion": pluginmgr.HostRuntimeVersion,
		"enabled": true,
	}, http.StatusBadRequest, "bad_request")
	f.fourField(t, "pluginManager.setVersionExemption", map[string]any{
		"packageVersion": "acme.future@1.0.0", "runtimeVersion": "0.1.0",
		"enabled": true, "acceptRisk": true,
	}, http.StatusBadRequest, "bad_request")
}

// 豁免被 install/update 的兼容性闸门消费：不兼容的包在没有豁免时拒装，
// 授权之后能装上，而且结果里带一条真实的风险警告。
func TestVersionExemptionGatesInstall(t *testing.T) {
	f := newFixture(t, "")
	pkg := packageFixtureWithEngines(t, filepath.Join(f.home, "private", "future"), "acme.future", "1.0.0", "^9.0.0")

	result := f.result(t, "pluginManager.installBundle", map[string]any{"spec": pkg})
	if result["application"] != "failed" {
		t.Fatalf("不兼容的包必须拒装：%v", result)
	}
	failure, _ := result["error"].(map[string]any)
	if failure == nil || failure["code"] != "incompatible-version" {
		t.Fatalf("拒装码必须是官方 incompatible-version：%v", result)
	}
	if diagnostic, _ := failure["diagnostic"].(string); !strings.Contains(diagnostic, "^9.0.0") {
		t.Fatalf("诊断必须点出没满足的要求：%q", diagnostic)
	}
	if got := installedIDs(t, f.manager); len(got) != 0 {
		t.Fatalf("拒装之后不许留下安装记录：%v", got)
	}

	// 授权之后同一份包能装上，并且带一条"靠豁免装上的"警告。
	if _, err := f.manager.SetVersionExemption("acme.future@1.0.0", pluginmgr.HostRuntimeVersion, true, true); err != nil {
		t.Fatalf("授权：%v", err)
	}
	result = f.result(t, "pluginManager.installBundle", map[string]any{"spec": pkg})
	if result["application"] != "applied" || result["changed"] != true {
		t.Fatalf("授权之后必须装上：%v", result)
	}
	warnings, _ := result["warnings"].([]any)
	joined := ""
	for _, warning := range warnings {
		joined += warning.(string) + "\n"
	}
	if !strings.Contains(joined, "豁免") || !strings.Contains(joined, "acme.future@1.0.0") {
		t.Fatalf("必须如实说明这个版本是靠豁免装上的：%v", warnings)
	}
	if got := installedIDs(t, f.manager); len(got) != 1 {
		t.Fatalf("装上的包不对：%v", got)
	} else if _, ok := got["acme.future"]; !ok {
		t.Fatalf("装上的包必须是 acme.future：%v", got)
	}

	// 兼容的包（engines 满足）不需要任何豁免。
	ok := packageFixtureWithEngines(t, filepath.Join(f.home, "private", "ok"), "acme.ok", "1.0.0", "^0.2.0")
	result = f.result(t, "pluginManager.installBundle", map[string]any{"spec": ok})
	if result["application"] != "applied" {
		t.Fatalf("兼容的包必须能装上：%v", result)
	}
}

// packageFixtureWithEngines 是一个带 engines.dsh 的可安装包目录。
func packageFixtureWithEngines(t *testing.T, dir, id, version, enginesDSH string) string {
	t.Helper()
	pkg := packageFixture(t, dir, id, version, nil)
	raw, err := os.ReadFile(filepath.Join(pkg, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["engines"] = map[string]string{"dsh": enginesDSH}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "plugin.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return pkg
}

// 需要 pnpm 的选项（构建审批/registry）不能静默忽略。
func TestInstallBundleRefusesPnpmOnlyOptions(t *testing.T) {
	f := newFixture(t, "")
	pkg := packageFixture(t, filepath.Join(f.home, "private", "v1"), "acme.audit", "1.0.0", nil)

	f.fourField(t, "pluginManager.installBundle", map[string]any{
		"spec": pkg, "options": map[string]any{"approvedBuilds": []string{"esbuild"}},
	}, http.StatusNotImplemented, "not_implemented")
	f.fourField(t, "pluginManager.installBundle", map[string]any{
		"spec": pkg, "options": map[string]any{"registry": "https://registry.example"},
	}, http.StatusNotImplemented, "not_implemented")
	if got := installedIDs(t, f.manager); len(got) != 0 {
		t.Fatalf("被拒的选项居然装了东西：%+v", got)
	}
	// saveExact 在本仓是"本来就满足"的语义（状态里记的就是精确版本目录），接受。
	result := f.result(t, "pluginManager.installBundle", map[string]any{
		"spec": pkg, "options": map[string]any{"saveExact": true},
	})
	if result["changed"] != true {
		t.Fatalf("saveExact 安装结果不对：%v", result)
	}
}

// 版本豁免的**读侧**：读真实 compatibility.json、重开可读、撤销后消失。
// 两个键在官方契约里都是必填，所以没有文件时也必须回 `{}` / `[]`。
func TestListVersionExemptionsReadsRealFileAndReopens(t *testing.T) {
	f := newFixture(t, "")

	result := f.result(t, "pluginManager.listVersionExemptions", map[string]any{})
	exemptions, ok := result["exemptions"].(map[string]any)
	if !ok || len(exemptions) != 0 {
		t.Fatalf("缺文件时必须回空对象 exemptions：%v", result)
	}
	if warnings, ok := result["warnings"].([]any); !ok || len(warnings) != 0 {
		t.Fatalf("缺文件时 warnings 必须是空数组：%v", result)
	}

	// 授权（真实写盘）之后读得到。
	_ = f.result(t, "pluginManager.setVersionExemption", map[string]any{
		"packageVersion": "acme.future@1.0.0", "runtimeVersion": pluginmgr.HostRuntimeVersion,
		"enabled": true, "acceptRisk": true,
	})
	result = f.result(t, "pluginManager.listVersionExemptions", map[string]any{})
	exemptions, _ = result["exemptions"].(map[string]any)
	versions, _ := exemptions["acme.future@1.0.0"].([]any)
	if len(versions) != 1 || versions[0] != pluginmgr.HostRuntimeVersion {
		t.Fatalf("授权之后必须读得到：%v", result)
	}
	if warnings, _ := result["warnings"].([]any); len(warnings) != 0 {
		t.Fatalf("合法文件不该有 warnings：%v", result)
	}

	// 重开（同一个 home 重新 Open + 重新挂接）仍然读得到。
	reopened := f.reopen(t)
	result = reopened.result(t, "pluginManager.listVersionExemptions", map[string]any{})
	exemptions, _ = result["exemptions"].(map[string]any)
	if versions, _ := exemptions["acme.future@1.0.0"].([]any); len(versions) != 1 {
		t.Fatalf("重开之后读不到豁免：%v", result)
	}

	// 撤销之后键真的消失。
	_ = reopened.result(t, "pluginManager.setVersionExemption", map[string]any{
		"packageVersion": "acme.future@1.0.0", "runtimeVersion": pluginmgr.HostRuntimeVersion, "enabled": false,
	})
	result = reopened.result(t, "pluginManager.listVersionExemptions", map[string]any{})
	exemptions, _ = result["exemptions"].(map[string]any)
	if _, ok := exemptions["acme.future@1.0.0"]; ok {
		t.Fatalf("撤销之后不该还有这个键：%v", result)
	}
}

// 坏文件按**既有语义**降级：读侧 200 + 空豁免 + warnings（不抛错）；写侧仍然
// 拒绝改写，让用户先手工修。单条记录非法只跳过那一条，其余仍然生效。
func TestListVersionExemptionsDegradesOnCorruptFile(t *testing.T) {
	f := newFixture(t, "")
	path := filepath.Join(f.root, "compatibility.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := f.result(t, "pluginManager.listVersionExemptions", map[string]any{})
	exemptions, ok := result["exemptions"].(map[string]any)
	if !ok || len(exemptions) != 0 {
		t.Fatalf("坏文件必须降级成空 exemptions（不是 500）：%v", result)
	}
	warnings, _ := result["warnings"].([]any)
	joined := ""
	for _, warning := range warnings {
		joined += warning.(string) + "\n"
	}
	if !strings.Contains(joined, "compatibility.json") {
		t.Fatalf("warnings 必须点名坏文件：%v", result)
	}

	// 单条记录非法：合法记录仍然生效，非法那条只进 warnings。
	raw := `{"acme.ok@1.0.0": ["` + pluginmgr.HostRuntimeVersion + `"], "bad-key": ["nope"]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	result = f.result(t, "pluginManager.listVersionExemptions", map[string]any{})
	exemptions, _ = result["exemptions"].(map[string]any)
	if _, ok := exemptions["acme.ok@1.0.0"]; !ok {
		t.Fatalf("合法记录必须仍然生效：%v", result)
	}
	if _, ok := exemptions["bad-key"]; ok {
		t.Fatalf("非法记录不该被接受：%v", result)
	}
	if warnings, _ := result["warnings"].([]any); len(warnings) != 1 {
		t.Fatalf("非法记录必须进 warnings：%v", result)
	}

	// 写侧拒绝改写（既有语义：文件被读者拒过就不可重写）。
	f.fourField(t, "pluginManager.setVersionExemption", map[string]any{
		"packageVersion": "acme.future@1.0.0", "runtimeVersion": pluginmgr.HostRuntimeVersion,
		"enabled": true, "acceptRisk": true,
	}, http.StatusBadRequest, "bad_request")
}
