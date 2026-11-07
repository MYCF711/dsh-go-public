package pluginmgr

// 版本豁免的真实持久化（官方 `setProfileVersionExemption` 的宿主侧实现）。
//
// # 官方原件
//
// app.asar → dsh-app-boot/lib/index.js:1159-1300：
//
//	PROFILE_COMPATIBILITY_FILENAME = "compatibility.json"
//	// 文件形状：{ "<name>@<精确版本>": ["<精确 dsh 版本>", …] }
//	readProfileCompatibility(dir)  // 缺文件=没有豁免；读不了/坏 JSON=没有豁免 + warnings + rewritable:false
//	setProfileVersionExemption(dir, packageVersion, runtimeVersion, enabled, acceptRisk)
//	// 1. 两边都必须是精确身份（name@version / 精确 SemVer，含 build metadata）
//	// 2. 授权必须 acceptRisk:true
//	// 3. 授权必须指定**当前** runtime（撤销可以指定历史版本）
//	// 4. 文件不可重写（被读者拒过）→ 拒绝写入，让用户先手工修
//	// 5. 写入是原子的（writeFileAtomic），权限 0600
//
// 本文件逐条对上：文件名、JSON 形状、键/值校验、缺文件=无豁免、坏文件=无豁免+
// 警告且不可重写、授权三条前置、原子写、0600。**没有**照搬的是"写到 profile 目录"：
// 本仓的插件管理器有自己的 root（`state.json` 所在目录），豁免与安装状态放在一起
// 才不会出现"豁免在一个 home、包装在另一个 home"的错位。
//
// # 与 install/update 的交互
//
// 官方在 install/update 前调用 `evaluatePluginCompatibility(manifest, exemptions)`
// 并在 !exempted 时拒装（`incompatible-version`）。本仓的同一处是
// `installLocked`/`Update` 里的 `checkCompatibilityLocked`：不兼容且没有豁免就
// 拒绝，有豁免就放行（调用方负责把风险提示带给用户）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// compatibilityFileName 与官方同名同形状（不是一个本仓私有格式）。
const compatibilityFileName = "compatibility.json"

// packageNamePattern 是 npm 包名（官方 PACKAGE_NAME 的同一份形状）。
var packageNamePattern = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)

// compatibilityState 是"读到的豁免 + 读的时候发现的问题"。
type compatibilityState struct {
	// Exemptions 是**被接受**的记录：精确 name@version → 精确 runtime 版本列表。
	Exemptions map[string][]string
	// Warnings 是每条被跳过的记录/文件问题；官方原样报给调用方。
	Warnings []string
	// Rewritable 为 false 表示文件本身不可重写（用户必须手工修）。
	Rewritable bool
}

// compatibilityPath 返回这个管理器 root 下的豁免文件路径。
func (m *Manager) compatibilityPath() string {
	return filepath.Join(m.root, compatibilityFileName)
}

// readCompatibility 读豁免文件。缺文件 = 没有豁免（官方同一分支）。
//
// 读不了/不是合法 JSON/顶层不是对象 → 没有豁免 + warning + rewritable:false。
// 单条记录非法只跳过那一条，其余仍然生效（官方："rejected records are skipped
// while the remaining valid ones still apply"）。
func (m *Manager) readCompatibility() compatibilityState {
	out := compatibilityState{Exemptions: map[string][]string{}, Rewritable: true}
	path := m.compatibilityPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out
		}
		out.Rewritable = false
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s 读不出来（%v）；按没有豁免处理", path, err))
		return out
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		out.Rewritable = false
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s 不是合法 JSON（%v）；按没有豁免处理", path, err))
		return out
	}
	if value == nil {
		out.Rewritable = false
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s 顶层必须是对象：{name@version: [dsh 版本]}", path))
		return out
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name, version, ok := splitPackageVersion(key)
		if !ok {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %q 不是精确的 name@version 键；这条记录被忽略", path, key))
			continue
		}
		_ = name
		_ = version
		var versions []string
		if err := json.Unmarshal(value[key], &versions); err != nil || !isExactVersionList(versions) {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %s 必须是一串精确 dsh 版本；这条记录被忽略", path, key))
			continue
		}
		out.Exemptions[key] = versions
	}
	if len(out.Warnings) > 0 {
		out.Rewritable = false
	}
	return out
}

// isExactVersionList 要求每个元素都是精确 SemVer（范围/前缀/空白都不算）。
func isExactVersionList(versions []string) bool {
	if versions == nil {
		return false
	}
	for _, version := range versions {
		if !isExactVersion(version) {
			return false
		}
	}
	return true
}

// isExactVersion 判断是不是"精确"SemVer：解析得出来，且原串与规范化写法一致
// （官方 `value === parsed.version + build` 的同一判据）。
func isExactVersion(value string) bool {
	parsed, err := parseSemver(value)
	if err != nil {
		return false
	}
	canonical := fmt.Sprintf("%d.%d.%d", parsed.core[0], parsed.core[1], parsed.core[2])
	if len(parsed.pre) > 0 {
		canonical += "-" + strings.Join(parsed.pre, ".")
	}
	if index := strings.IndexByte(value, '+'); index >= 0 {
		canonical += value[index:]
	}
	return value == canonical
}

// splitPackageVersion 拆 `name@version`：分隔符必须存在、两边都非空且都合法。
//
// 用 LastIndex（官方同样 lastIndexOf('@')），这样 `@scope/pkg@1.2.3` 也对。
func splitPackageVersion(packageVersion string) (name, version string, ok bool) {
	at := strings.LastIndex(packageVersion, "@")
	if at <= 0 || at == len(packageVersion)-1 {
		return "", "", false
	}
	name, version = packageVersion[:at], packageVersion[at+1:]
	if !packageNamePattern.MatchString(name) || !isExactVersion(version) {
		return "", "", false
	}
	return name, version, true
}

// VersionExemptions 返回当前 profile 里被接受的豁免（只读，重开管理器后仍可读）。
func (m *Manager) VersionExemptions() (map[string][]string, []string) {
	m.compatMu.Lock()
	defer m.compatMu.Unlock()
	state := m.readCompatibility()
	out := make(map[string][]string, len(state.Exemptions))
	for key, versions := range state.Exemptions {
		out[key] = append([]string(nil), versions...)
	}
	return out, append([]string(nil), state.Warnings...)
}

// ExemptionChanged 报告一次 SetVersionExemption 是否真的改了文件。
type ExemptionChanged struct {
	Changed bool
	// Warnings 是持久化之后仍然成立的事实（例如本仓没有运行时加载器）。
	Warnings []string
}

// SetVersionExemption 授权/撤销一个**精确**版本豁免。
//
// 前置条件全部照官方：
//   - packageVersion 必须是精确 `name@version`；
//   - runtimeVersion 必须是精确 SemVer；
//   - 授权（enabled）必须 acceptRisk=true；
//   - 授权必须指向**当前** runtime（撤销可以指向历史版本）；
//   - 文件被读者拒过（不可重写）→ 报错让用户先修。
func (m *Manager) SetVersionExemption(packageVersion, runtimeVersion string, enabled, acceptRisk bool) (ExemptionChanged, error) {
	var out ExemptionChanged
	name, version, ok := splitPackageVersion(strings.TrimSpace(packageVersion))
	if !ok {
		return out, badRequest("packageVersion 必须是精确的「包名@版本」，收到 %q", packageVersion)
	}
	_ = name
	_ = version
	runtimeVersion = strings.TrimSpace(runtimeVersion)
	if !isExactVersion(runtimeVersion) {
		return out, badRequest("runtimeVersion 必须是精确 SemVer，收到 %q", runtimeVersion)
	}
	if enabled && !acceptRisk {
		return out, badRequest("授权不兼容版本必须显式接受风险（acceptRisk=true）：它可能让宿主崩溃或丢数据")
	}
	if enabled && runtimeVersion != HostRuntimeVersion {
		return out, badRequest("只能为当前 runtime（%s）授权，收到 %q", HostRuntimeVersion, runtimeVersion)
	}

	m.compatMu.Lock()
	defer m.compatMu.Unlock()
	current := m.readCompatibility()
	if !current.Rewritable {
		return out, errorf("compatibility_corrupt", 422, false,
			"先手工修好 compatibility.json（或删掉它），再重试",
			"%s 有无法接受的记录，拒绝改写：%s", compatibilityFileName, strings.Join(current.Warnings, "；"))
	}
	versions := current.Exemptions[packageVersion]
	before := append([]string(nil), versions...)
	if enabled {
		merged := append([]string(nil), versions...)
		found := false
		for _, existing := range merged {
			if existing == runtimeVersion {
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, runtimeVersion)
		}
		sort.Slice(merged, func(i, j int) bool { return compareVersionStrings(merged[i], merged[j]) < 0 })
		current.Exemptions[packageVersion] = merged
	} else {
		retained := make([]string, 0, len(versions))
		for _, existing := range versions {
			if existing != runtimeVersion {
				retained = append(retained, existing)
			}
		}
		if len(retained) == 0 {
			delete(current.Exemptions, packageVersion)
		} else {
			current.Exemptions[packageVersion] = retained
		}
	}
	if sameVersionList(before, current.Exemptions[packageVersion]) {
		// 幂等：同一授权写两次不重复写盘，也不谎报 changed。
		return out, nil
	}
	if err := m.writeCompatibility(current.Exemptions); err != nil {
		return out, err
	}
	out.Changed = true
	out.Warnings = append(out.Warnings,
		"本仓没有运行时插件加载器：豁免已持久化（下次 install/update 该精确版本时生效），不会热重载已装插件")
	return out, nil
}

// writeCompatibility 原子写豁免文件（0600，与官方同一权限）。
func (m *Manager) writeCompatibility(exemptions map[string][]string) error {
	if exemptions == nil {
		exemptions = map[string][]string{}
	}
	raw, err := json.MarshalIndent(exemptions, "", "  ")
	if err != nil {
		return internalError(fmt.Errorf("序列化版本豁免失败: %w", err))
	}
	raw = append(raw, '\n')
	path := m.compatibilityPath()
	staging := path + ".tmp"
	if err := os.WriteFile(staging, raw, 0o600); err != nil {
		return internalError(fmt.Errorf("写版本豁免临时文件失败: %w", err))
	}
	if err := os.Rename(staging, path); err != nil {
		_ = os.Remove(staging)
		return internalError(fmt.Errorf("原子替换版本豁免文件失败: %w", err))
	}
	return nil
}

// checkCompatibilityLocked 是 install/update 的兼容性闸门。
//
// 不兼容且没有精确豁免 → 拒绝安装（官方 incompatible-version）。有豁免 → 放行，
// 由调用方把风险提示带给用户。**没有**兼容性要求的包一律放行（返回 nil）。
func (m *Manager) checkCompatibilityLocked(manifest Manifest) error {
	exemptions, _ := m.VersionExemptions()
	issue, err := EvaluateCompatibility(manifest, exemptions, HostRuntimeVersion)
	if err != nil {
		return manifestError("兼容性判定失败: %v", err)
	}
	if issue == nil || issue.Exempted {
		return nil
	}
	return errorf("incompatible_version", 409, false,
		fmt.Sprintf("更新插件，或为 %s 在 dsh %s 上授权精确版本豁免（pluginManager.setVersionExemption，需 acceptRisk=true）",
			issue.Key(), issue.RuntimeVersion),
		"%s@%s 与 dsh %s 不兼容（要求 %s）；运行它可能崩溃或丢数据",
		issue.Name, issue.Version, issue.RuntimeVersion, issue.Requires())
}

// CompatibilityOf 返回一个已安装/待安装包的兼容性结论（含是否已豁免）。
//
// 适配器用它给 ChangeResult 补一条真实警告：装上了、但它是**靠豁免**装上的。
func (m *Manager) CompatibilityOf(manifest Manifest) (*CompatibilityIssue, error) {
	exemptions, _ := m.VersionExemptions()
	return EvaluateCompatibility(manifest, exemptions, HostRuntimeVersion)
}

// compareVersionStrings 给豁免列表排序用；解析不出来的按字典序垫底。
func compareVersionStrings(left, right string) int {
	leftVersion, leftErr := parseSemver(left)
	rightVersion, rightErr := parseSemver(right)
	if leftErr != nil || rightErr != nil {
		return strings.Compare(left, right)
	}
	return compareSemver(leftVersion, rightVersion)
}

// sameVersionList 判两份列表是否逐项相同（顺序敏感：写盘前我们已经排过序）。
func sameVersionList(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
