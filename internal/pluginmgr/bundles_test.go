package pluginmgr

// 只读投影（bundles.go）的判据测试：来源形态分类、保留版本投影、包内文档读取。
//
// 这些判据决定官方 `pluginManager.inspect/listVersions/updateBundle` 的**答案**，
// 所以必须钉在真实 state/包目录上，而不是在适配器里猜。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceKindClassification(t *testing.T) {
	registry := []string{
		"acme.bundle", "acme.bundle@1.0.0", "acme.bundle@^1.2.0",
		"@scope/name", "@scope/name@1.2.3",
	}
	for _, spec := range registry {
		if got := SourceKind(spec); got != "registry" {
			t.Fatalf("%q 判成 %q，期望 registry（作用域包名含 /，不是相对路径）", spec, got)
		}
	}
	local := []string{"./pkg", "../pkg", "some/dir", "/tmp/pkg", `C:\tmp\pkg`, "D:/tmp/pkg"}
	for _, spec := range local {
		if got := SourceKind(spec); got != "path" {
			t.Fatalf("%q 判成 %q，期望 path", spec, got)
		}
	}
	if got := SourceKind("git+https://example.com/a.git"); got != "git" {
		t.Fatalf("git 规格判成 %q", got)
	}
	if got := SourceKind("https://example.com/a-1.0.0.tgz"); got != "tarball" {
		t.Fatalf("tarball 规格判成 %q", got)
	}
	if SourceIsRegistrySpec("/tmp/pkg") {
		t.Fatal("本地目录被当成 registry 依赖：updatable 会误报")
	}
	if !SourceIsRegistrySpec("acme.bundle@1.0.0") {
		t.Fatal("registry 规格没被识别")
	}
}

func TestBundlesProjectRetainedVersions(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	v1 := writeTestPackage(t, filepath.Join(home, "v1"), "acme.audit", "1.0.0")
	v2 := writeTestPackage(t, filepath.Join(home, "v2"), "acme.audit", "2.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(v1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update("acme.audit", v2); err != nil {
		t.Fatal(err)
	}

	state, ok, err := m.Bundle("acme.audit")
	if err != nil || !ok {
		t.Fatalf("Bundle: ok=%v err=%v", ok, err)
	}
	if state.Version != "2.0.0" || state.PreviousVersion != "1.0.0" {
		t.Fatalf("当前/上一版投影不对：%+v", state)
	}
	if len(state.Versions) != 2 || state.Versions[0] != "1.0.0" || state.Versions[1] != "2.0.0" {
		t.Fatalf("保留版本必须按 SemVer 升序：%+v", state.Versions)
	}
	if state.VersionSources["1.0.0"] != v1 || state.Source != v2 {
		t.Fatalf("来源投影不对：%+v", state)
	}
	if _, ok, err := m.Bundle("nope.gone"); ok || err != nil {
		t.Fatalf("未安装的包必须 ok=false 且不报错：ok=%v err=%v", ok, err)
	}
	all, err := m.Bundles()
	if err != nil || len(all) != 1 || all[0].Name != "acme.audit" {
		t.Fatalf("Bundles 投影不对：%v %+v", err, all)
	}
	if cmp, err := CompareVersions("2.0.0", "1.0.0"); err != nil || cmp != 1 {
		t.Fatalf("CompareVersions(2.0.0,1.0.0)=%d,%v", cmp, err)
	}
	if _, err := CompareVersions("2.0", "1.0.0"); err == nil {
		t.Fatal("非法版本比较没有报错")
	}
}

func TestReadDocumentOnlyReadsRegisteredFiles(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	// 带 README 的包（README 必须登记在 manifest.files 里才能被接受）。
	dir := writeTestPackage(t, filepath.Join(home, "v1"), "acme.docs", "1.0.0")
	readme := []byte("# 文档\n真实内容\n")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), readme, 0o644); err != nil {
		t.Fatal(err)
	}
	registerFile(t, dir, "README.md", readme)
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(dir); err != nil {
		t.Fatal(err)
	}
	path, raw, ok, err := m.ReadDocument("acme.docs", []string{"README.md"})
	if err != nil || !ok || path != "README.md" {
		t.Fatalf("ReadDocument: path=%q ok=%v err=%v", path, ok, err)
	}
	if !strings.Contains(string(raw), "真实内容") {
		t.Fatalf("读到的文档不对：%q", raw)
	}
	// 没登记的路径一律读不到（不把包目录变成任意文件读取口）。
	if _, _, ok, err := m.ReadDocument("acme.docs", []string{"plugin.json", "../state.json"}); err != nil || ok {
		t.Fatalf("未登记文件被读到了：ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := m.ReadDocument("nope.gone", []string{"README.md"}); err == nil || ok {
		t.Fatal("未知包的 ReadDocument 必须报 not_found")
	}
}

func TestValidateSourceIsTheInstallJudge(t *testing.T) {
	home := t.TempDir()
	good := writeTestPackage(t, filepath.Join(home, "good"), "acme.audit", "1.0.0")
	manifest, err := ValidateSource(good)
	if err != nil || manifest.ID != "acme.audit" || manifest.Version != "1.0.0" {
		t.Fatalf("ValidateSource: %+v %v", manifest, err)
	}
	broken := filepath.Join(home, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.json"), []byte(`{"schema":"dsh.local-plugin@1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateSource(broken); err == nil {
		t.Fatal("坏 manifest 通过了 ValidateSource：inspect 会先说 accepted、install 再失败")
	}
	if _, err := ValidateSource(filepath.Join(home, "absent")); err == nil {
		t.Fatal("不存在的目录通过了 ValidateSource")
	}
}

// registerFile 把一个文件登记进 packageFixture 的 manifest（重算 SHA-256）。
func registerFile(t *testing.T, dir, name string, payload []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := decodeStrict(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	files, _ := manifest["files"].(map[string]any)
	sum := sha256.Sum256(payload)
	files[name] = hex.EncodeToString(sum[:])
	manifest["files"] = files
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}
