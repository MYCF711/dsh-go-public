package pluginmgr

// 兼容性模型与版本豁免的**真实磁盘**测试。
//
// 这一份只测库层：范围语义（官方 dsh-app-boot 的判据）、compatibility.json 的
// 形状/原子写/坏文件处理、以及 install/update 的闸门与豁免的交互。协议层
// （/api/pluginManager.setVersionExemption）在 internal/pluginmgr/official 的
// adapter_test.go 里，用同一个真实管理器。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 范围语义：覆盖官方插件实际用的写法，且"不合法就报错"而不是猜。
func TestSatisfiesRangeSemantics(t *testing.T) {
	cases := []struct {
		version string
		expr    string
		want    bool
		wantErr bool
	}{
		{"0.2.2-go.1", "^0.2.0", true, false},
		{"0.2.2-go.1", "^0.2.2-go.0", true, false},
		{"0.2.2-go.1", "~0.2.0", true, false},
		{"0.2.2-go.1", "0.2.2-go.1", true, false},
		{"0.2.2-go.1", "=0.2.2-go.1", true, false},
		{"0.2.2-go.1", "0.2.2", false, false},   // 预发布不满足"发布版"精确匹配
		{"0.2.2-go.1", ">=0.2.2-go.1", true, false},
		{"0.2.2-go.1", ">0.2.2", false, false},
		{"0.2.2-go.1", "<0.3.0", true, false},
		{"0.2.2-go.1", "0.2.x", true, false},
		{"0.2.2-go.1", "0.3.x", false, false},
		{"0.2.2-go.1", "^9.0.0", false, false},
		{"0.2.2-go.1", ">=9.0.0 <10.0.0", false, false},
		{"0.2.2-go.1", "^9.0.0 || ^0.2.0", true, false},
		{"0.2.2-go.1", "0.1.0 - 0.3.0", true, false},
		{"0.2.2-go.1", "*", true, false},
		{"0.2.2-go.1", "x", true, false},
		{"0.2.2-go.1", "workspace:^", false, true}, // engines 上 workspace: 一律不满足
		{"0.2.2-go.1", "不是范围", false, true},
		{"0.2.2-go.1", "", false, true},
	}
	for _, tc := range cases {
		got, err := SatisfiesRange(tc.version, tc.expr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q on %q 必须报错，实际 ok=%v", tc.version, tc.expr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q on %q 不该报错：%v", tc.version, tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q on %q = %v，要 %v", tc.version, tc.expr, got, tc.want)
		}
	}
}

// engines.dsh 单独决定；peerDependencies 只在没有 engines 时看。
func TestEvaluateCompatibilityRules(t *testing.T) {
	runtime := HostRuntimeVersion
	incompatible := Manifest{Schema: ManifestSchema, ID: "acme.future", Version: "1.0.0",
		Engines: &Engines{DSH: "^9.0.0"}}
	issue, err := EvaluateCompatibility(incompatible, nil, runtime)
	if err != nil {
		t.Fatalf("EvaluateCompatibility: %v", err)
	}
	if issue == nil || issue.Exempted {
		t.Fatalf("^9.0.0 必须判为不兼容且未豁免：%+v", issue)
	}
	if issue.Key() != "acme.future@1.0.0" || issue.Requirements["engines.dsh"] != "^9.0.0" {
		t.Fatalf("不兼容结果字段不对：%+v", issue)
	}
	// 同一份包 + 精确 runtime 上的豁免 → Exempted 为真（仍然不兼容，但被放行）。
	issue, err = EvaluateCompatibility(incompatible, map[string][]string{"acme.future@1.0.0": {runtime}}, runtime)
	if err != nil {
		t.Fatalf("EvaluateCompatibility: %v", err)
	}
	if issue == nil || !issue.Exempted {
		t.Fatalf("精确豁免必须让 Exempted 为真：%+v", issue)
	}
	// 别的 runtime 上的豁免不算（官方 includes 精确相等）。
	issue, err = EvaluateCompatibility(incompatible, map[string][]string{"acme.future@1.0.0": {"0.1.0"}}, runtime)
	if err != nil {
		t.Fatalf("EvaluateCompatibility: %v", err)
	}
	if issue == nil || issue.Exempted {
		t.Fatalf("别的 runtime 的豁免不算：%+v", issue)
	}

	// engines 满足 → 兼容；此时 peer 完全不看（官方口径）。
	compatible := Manifest{Schema: ManifestSchema, ID: "acme.ok", Version: "1.0.0",
		Engines:          &Engines{DSH: "^0.2.0"},
		PeerDependencies: map[string]string{"@deepseek-ai/dsh": "^9.0.0"}}
	if issue, err := EvaluateCompatibility(compatible, nil, runtime); err != nil || issue != nil {
		t.Fatalf("engines 满足就该兼容（peer 忽略）：issue=%+v err=%v", issue, err)
	}

	// 没有 engines 时看 peer：非 dsh 名字的 peer 不参与。
	peerOnly := Manifest{Schema: ManifestSchema, ID: "acme.peer", Version: "1.0.0",
		PeerDependencies: map[string]string{
			"@deepseek-ai/dsh":      "^9.0.0",
			"@deepseek-ai/dsh-llm":  "workspace:^",
			"react":                 "^18.0.0",
			"@deepseek-ai/dsh-core": "不是范围",
		}}
	issue, err = EvaluateCompatibility(peerOnly, nil, runtime)
	if err != nil {
		t.Fatalf("EvaluateCompatibility(peer): %v", err)
	}
	if issue == nil {
		t.Fatal("不满足的 dsh peer 必须判为不兼容")
	}
	if _, ok := issue.Requirements["@deepseek-ai/dsh"]; !ok {
		t.Fatalf("缺 @deepseek-ai/dsh 要求：%+v", issue.Requirements)
	}
	if _, ok := issue.Requirements["@deepseek-ai/dsh-core"]; !ok {
		t.Fatalf("非法范围在 peer 上算不满足（官方口径）：%+v", issue.Requirements)
	}
	if _, ok := issue.Requirements["@deepseek-ai/dsh-llm"]; ok {
		t.Fatalf("peer 的 workspace:^ 指当前 runtime，必须满足：%+v", issue.Requirements)
	}
	if _, ok := issue.Requirements["react"]; ok {
		t.Fatalf("非 dsh peer 不参与兼容性：%+v", issue.Requirements)
	}

	// engines 写坏了是错误（官方抛），不是"不兼容"。
	broken := Manifest{Schema: ManifestSchema, ID: "acme.bad", Version: "1.0.0", Engines: &Engines{DSH: "不是范围"}}
	if _, err := EvaluateCompatibility(broken, nil, runtime); err == nil {
		t.Fatal("engines.dsh 非法必须报错")
	}
}

// 豁免文件的形状、原子写、幂等与撤销——全部读真实磁盘。
func TestVersionExemptionPersistsAndReopens(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	manager, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, warnings := manager.VersionExemptions(); len(got) != 0 || len(warnings) != 0 {
		t.Fatalf("新 root 不该有豁免：%v %v", got, warnings)
	}

	changed, err := manager.SetVersionExemption("acme.future@1.0.0", HostRuntimeVersion, true, true)
	if err != nil {
		t.Fatalf("SetVersionExemption: %v", err)
	}
	if !changed.Changed {
		t.Fatal("第一次授权必须 changed=true")
	}
	if len(changed.Warnings) == 0 {
		t.Fatal("必须如实说明本仓没有运行时加载器")
	}

	// 文件是官方同形状：顶层就是 {name@version: [dsh 版本]}，没有私有 schema 包装。
	raw, err := os.ReadFile(filepath.Join(root, compatibilityFileName))
	if err != nil {
		t.Fatalf("豁免文件必须落在管理器 root 下：%v", err)
	}
	var fileShape map[string][]string
	if err := json.Unmarshal(raw, &fileShape); err != nil {
		t.Fatalf("豁免文件形状不对（官方是裸对象）：%v\n%s", err, raw)
	}
	if versions := fileShape["acme.future@1.0.0"]; len(versions) != 1 || versions[0] != HostRuntimeVersion {
		t.Fatalf("文件内容不对：%v", fileShape)
	}
	// 权限只在 POSIX 上有意义：Windows 的 os.Stat 一律报 0666，断它等于断测试环境。
	if info, err := os.Stat(filepath.Join(root, compatibilityFileName)); err != nil {
		t.Fatalf("豁免文件必须存在：%v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("豁免文件权限必须是 0600：%v", info.Mode().Perm())
	}

	// 幂等：再授权一次不重复写、也不谎报 changed。
	again, err := manager.SetVersionExemption("acme.future@1.0.0", HostRuntimeVersion, true, true)
	if err != nil {
		t.Fatalf("重复授权：%v", err)
	}
	if again.Changed {
		t.Fatal("同一授权写两次不该 changed=true")
	}

	// 重开管理器：仍然读得到（"重开可读"就是这个意思）。
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	exemptions, warnings := reopened.VersionExemptions()
	if len(warnings) != 0 {
		t.Fatalf("正常文件不该有警告：%v", warnings)
	}
	if versions := exemptions["acme.future@1.0.0"]; len(versions) != 1 || versions[0] != HostRuntimeVersion {
		t.Fatalf("重开之后读不到豁免：%v", exemptions)
	}

	// 撤销：键消失，文件回到 {}。
	revoked, err := reopened.SetVersionExemption("acme.future@1.0.0", HostRuntimeVersion, false, false)
	if err != nil {
		t.Fatalf("撤销：%v", err)
	}
	if !revoked.Changed {
		t.Fatal("撤销必须 changed=true")
	}
	raw, err = os.ReadFile(filepath.Join(root, compatibilityFileName))
	if err != nil {
		t.Fatal(err)
	}
	var after map[string][]string
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("撤销之后文件必须为空对象：%v", after)
	}
}

// 前置条件：身份必须精确、授权必须接受风险、授权只能给当前 runtime。
func TestVersionExemptionPreconditions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	manager, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name                                       string
		packageVersion, runtimeVersion             string
		enabled, acceptRisk                        bool
	}{
		{"packageVersion 不是 name@version", "acme.future", HostRuntimeVersion, true, true},
		{"版本是范围", "acme.future@^1.0.0", HostRuntimeVersion, true, true},
		{"runtime 不是精确版本", "acme.future@1.0.0", "^0.2.0", true, true},
		{"授权不接受风险", "acme.future@1.0.0", HostRuntimeVersion, true, false},
		{"授权给别的 runtime", "acme.future@1.0.0", "0.1.0", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := manager.SetVersionExemption(tc.packageVersion, tc.runtimeVersion, tc.enabled, tc.acceptRisk); err == nil {
				t.Fatalf("必须拒绝：%+v", tc)
			}
		})
	}
	// 撤销不要求 acceptRisk，也不要求当前 runtime（官方：撤销可以点名历史版本）。
	if _, err := manager.SetVersionExemption("acme.future@1.0.0", "0.1.0", false, false); err != nil {
		t.Fatalf("撤销历史 runtime 不该报错：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, compatibilityFileName)); err == nil {
		t.Fatal("什么都没改到时不该写出空文件（幂等无变更）")
	}
}

// 坏文件：按"没有豁免"处理 + 警告 + 拒绝改写（官方 rewritable:false）。
func TestVersionExemptionRejectsCorruptFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, compatibilityFileName)
	if err := os.WriteFile(path, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	exemptions, warnings := manager.VersionExemptions()
	if len(exemptions) != 0 || len(warnings) != 1 {
		t.Fatalf("坏文件必须按无豁免处理并报一条警告：%v %v", exemptions, warnings)
	}
	if _, err := manager.SetVersionExemption("acme.future@1.0.0", HostRuntimeVersion, true, true); err == nil {
		t.Fatal("坏文件必须先修好，不许被覆盖")
	}
	// 文件内容必须没被我们改过。
	raw, _ := os.ReadFile(path)
	if string(raw) != "{ 不是 json" {
		t.Fatalf("拒绝改写之后文件被动过了：%q", raw)
	}
}

// 单条记录非法只跳过那一条，其余仍然生效（官方原文同口径）。
func TestVersionExemptionSkipsOnlyBadRecords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	content := map[string]any{
		"acme.future@1.0.0":     []string{HostRuntimeVersion},
		"acme.range@^1.0.0":     []string{HostRuntimeVersion}, // 键不是精确版本
		"acme.badversions@1.0.0": []string{"^0.2.0"},          // 值不是精确版本
	}
	raw, _ := json.Marshal(content)
	if err := os.WriteFile(filepath.Join(root, compatibilityFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	exemptions, warnings := manager.VersionExemptions()
	if len(exemptions) != 1 {
		t.Fatalf("只该接受一条合法记录：%v", exemptions)
	}
	if _, ok := exemptions["acme.future@1.0.0"]; !ok {
		t.Fatalf("合法记录被丢了：%v", exemptions)
	}
	if len(warnings) != 2 {
		t.Fatalf("两条非法记录各一条警告，实际 %v", warnings)
	}
}

// install/update 的闸门：不兼容且无豁免就拒装；授权之后同一份包能装上。
func TestInstallGateUsesExemptions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	manager, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "pkg")
	pkg := writeEnginesPackage(t, source, "acme.future", "1.0.0", "^9.0.0")

	if _, err := manager.Install(pkg); err == nil {
		t.Fatal("不兼容且没有豁免时必须拒装")
	} else {
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Code != "incompatible_version" {
			t.Fatalf("拒绝安装必须回 incompatible_version，实际 %v", err)
		}
	}
	if installed, err := manager.installedIDsForTest(); err != nil || len(installed) != 0 {
		t.Fatalf("拒装之后不许留下任何安装记录：%v %v", installed, err)
	}

	// 授权之后同一份包能装上，并且 CompatibilityOf 报"已豁免"。
	if _, err := manager.SetVersionExemption("acme.future@1.0.0", HostRuntimeVersion, true, true); err != nil {
		t.Fatalf("授权：%v", err)
	}
	info, err := manager.Install(pkg)
	if err != nil {
		t.Fatalf("授权之后必须能装上：%v", err)
	}
	if info.ID != "acme.future" || info.Version != "1.0.0" {
		t.Fatalf("装上的包不对：%+v", info)
	}
	issue, err := manager.CompatibilityOf(Manifest{ID: "acme.future", Version: "1.0.0",
		Engines: &Engines{DSH: "^9.0.0"}})
	if err != nil {
		t.Fatalf("CompatibilityOf: %v", err)
	}
	if issue == nil || !issue.Exempted {
		t.Fatalf("装上之后 CompatibilityOf 必须报已豁免：%+v", issue)
	}

	// 更新同一份不兼容的新版本同样要过闸门：没有新版本豁免就拒。
	next := writeEnginesPackage(t, filepath.Join(t.TempDir(), "pkg2"), "acme.future", "2.0.0", "^9.0.0")
	if _, err := manager.Update("acme.future", next); err == nil {
		t.Fatal("不兼容的新版本没有豁免时必须拒绝更新")
	}
	if _, err := manager.SetVersionExemption("acme.future@2.0.0", HostRuntimeVersion, true, true); err != nil {
		t.Fatalf("授权 2.0.0：%v", err)
	}
	updated, err := manager.Update("acme.future", next)
	if err != nil {
		t.Fatalf("授权之后必须能更新：%v", err)
	}
	if updated.Version != "2.0.0" {
		t.Fatalf("更新后的版本不对：%+v", updated)
	}
}

// 没有兼容性要求的包（既有全部包）行为一个字节都不变。
func TestInstallGateIgnoresPackagesWithoutRequirements(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	manager, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := writeTestPackage(t, filepath.Join(t.TempDir(), "plain"), "acme.plain", "1.0.0")
	if _, err := manager.Install(pkg); err != nil {
		t.Fatalf("没有兼容性要求的包必须能装：%v", err)
	}
}

// writeEnginesPackage 造一个带 engines.dsh 的真实可安装包目录。
func writeEnginesPackage(t *testing.T, dir, id, version, enginesDSH string) string {
	t.Helper()
	pkg := writeTestPackage(t, dir, id, version)
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

// installedIDsForTest 读真实状态里的已安装 id（拒装之后必须为空）。
func (m *Manager) installedIDsForTest() ([]string, error) {
	infos, err := m.List()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.ID)
	}
	return ids, nil
}
