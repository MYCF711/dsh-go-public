package pluginmgr

// 插件/runtime 兼容性模型（官方 `evaluatePluginCompatibility` 的宿主侧实现）。
//
// # 官网原件
//
// app.asar → dsh-app-boot/lib/index.js:1129-1160：
//
//	function evaluatePluginCompatibility(manifest, exemptions = {}, runtimeVersion = getDshRuntimeVersion()) {
//	    const requirements = unmetRequirements(fields, runtimeVersion);
//	    if (Object.keys(requirements).length === 0) return undefined;
//	    const key = `${name}@${version}`;
//	    return { name, version, runtimeVersion, requirements,
//	             exempted: (exemptions[key] ?? []).includes(runtimeVersion) };
//	}
//
// 判据（同上 :1090-1119）：
//   - manifest 自己有 `engines.dsh`（字符串范围）就**只看它**，
//     peerDependencies 一律忽略；不满足 → {"engines.dsh": range}；
//   - 否则逐个看 `peerDependencies` 里名字是 `@deepseek-ai/dsh` 或
//     `@deepseek-ai/dsh-*` 的键，全部满足才算兼容；
//   - peer 的 `workspace:^|~|*` 指"当前 runtime"；`engines.dsh` 上的 workspace:
//     一律不满足（官方原文：including any workspace: `engines.dsh`, are unmet）；
//   - 预发布版本**参与**比较（官方：Prereleases participate in ranges），
//     所以本仓的 `0.2.2-go.1` 在 `^0.2.0` 里是满足的。
//
// # 本仓包的最小诚实体现在哪
//
// 本仓的包格式是 `dsh.local-plugin@1`（plugin.json），官方是 npm package.json。
// 这里只加两个**可选**字段：`engines` 与 `peerDependencies`，语义与官方逐条一致。
// 没有这两个字段的包（现在全部既有包）行为一个字节都不变——不满足判据的函数
// 在"没有要求"时返回 nil，不是"默认不兼容"。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// HostRuntimeVersion 是本宿主的 runtime 版本，用来判 `engines.dsh` / peer 范围。
//
// 为什么是一个可覆盖的变量而不是常量：`cmd/dshd/main.go:69` 与
// `cmd/dshtui/brand.go:27` 各自有 `version` 常量（0.2.2-go.1）。库包不能 import
// main，所以在装配期把 main 的版本写进来是唯一不重复定义的做法；没有注入时用
// 这个默认值（与 main 的常量一致）。
var HostRuntimeVersion = "0.2.2-go.1"

// Engines 对应官方 package.json 的 `engines`。目前只认 `dsh` 一个键。
type Engines struct {
	// DSH 是 dsh runtime 的 SemVer 范围（`^0.2.0`、`>=0.2.2 <0.3.0` 之类）。
	DSH string `json:"dsh,omitempty"`
}

// CompatibilityIssue 是"不兼容"的结构化结果（官方同名字段）。
//
// Exempted 为 true 表示**仍然不兼容**，但这个精确版本 + 这个 runtime 上有用户
// 显式授权（版本豁免）：调用方据此放行，并且要如实带上风险提示。
type CompatibilityIssue struct {
	Name           string
	Version        string
	RuntimeVersion string
	Requirements   map[string]string
	Exempted       bool
}

// Key 返回官方豁免键 `name@version`（本仓的 name 就是 manifest.id）。
func (i *CompatibilityIssue) Key() string {
	if i == nil {
		return ""
	}
	return i.Name + "@" + i.Version
}

// Requires 返回稳定的要求摘要（错误文案与测试都用它，避免 map 顺序抖动）。
func (i *CompatibilityIssue) Requires() string {
	if i == nil || len(i.Requirements) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(i.Requirements))
	for key := range i.Requirements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", key, i.Requirements[key]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// EvaluateCompatibility 判定一个包在当前 runtime 上兼不兼容。
//
// 返回 nil 表示"完全满足"（官方 undefined）。不兼容时返回结构化结果，
// 其中 Exempted 由 exemptions 决定。engines.dsh 写坏了是**错误**（官方抛），
// 不是"不兼容"：静默当成不兼容会让用户去授权一个根本解析不了的范围。
func EvaluateCompatibility(manifest Manifest, exemptions map[string][]string, runtimeVersion string) (*CompatibilityIssue, error) {
	requirements, err := compatibilityRequirements(manifest, runtimeVersion)
	if err != nil {
		return nil, err
	}
	if len(requirements) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(manifest.ID) == "" || strings.TrimSpace(manifest.Version) == "" {
		return nil, fmt.Errorf("不兼容的包必须有 id 与 version 才能授权豁免")
	}
	key := manifest.ID + "@" + manifest.Version
	return &CompatibilityIssue{
		Name:           manifest.ID,
		Version:        manifest.Version,
		RuntimeVersion: runtimeVersion,
		Requirements:   requirements,
		Exempted:       hasExactExemption(exemptions[key], runtimeVersion),
	}, nil
}

// hasExactExemption 报告这一串 runtime 版本里有没有**精确**等于当前 runtime 的。
//
// 官方就是 `exemptions[key].includes(runtimeVersion)`：范围、前缀、空白都不算。
func hasExactExemption(versions []string, runtimeVersion string) bool {
	for _, version := range versions {
		if version == runtimeVersion {
			return true
		}
	}
	return false
}

// compatibilityRequirements 返回未满足的要求（空 map = 兼容）。
func compatibilityRequirements(manifest Manifest, runtimeVersion string) (map[string]string, error) {
	if _, err := parseSemver(runtimeVersion); err != nil {
		return nil, fmt.Errorf("runtime 版本 %q 不是 SemVer: %w", runtimeVersion, err)
	}
	if manifest.Engines != nil {
		rangeExpr := strings.TrimSpace(manifest.Engines.DSH)
		if rangeExpr == "" {
			return nil, fmt.Errorf("manifest engines.dsh 必须是字符串范围")
		}
		ok, err := SatisfiesRange(runtimeVersion, rangeExpr)
		if err != nil {
			return nil, fmt.Errorf("manifest engines.dsh=%q 不是合法范围: %w", rangeExpr, err)
		}
		if ok {
			return map[string]string{}, nil
		}
		return map[string]string{"engines.dsh": rangeExpr}, nil
	}
	unmet := map[string]string{}
	for name, rangeExpr := range manifest.PeerDependencies {
		if name != "@deepseek-ai/dsh" && !strings.HasPrefix(name, "@deepseek-ai/dsh-") {
			continue
		}
		expr := strings.TrimSpace(rangeExpr)
		switch expr {
		case "workspace:^", "workspace:~", "workspace:*":
			// 官方：peer 的 workspace 指"当前 runtime"，所以必然满足。
			continue
		}
		ok, err := SatisfiesRange(runtimeVersion, expr)
		if err != nil || !ok {
			// 官方的口径：peer 上空的/非法的范围都算不满足（不抛错）。
			unmet[name] = rangeExpr
		}
	}
	return unmet, nil
}

// SatisfiesRange 判断 version 是否落在 npm 风格的范围里。
//
// 支持的子集（覆盖官方插件实际使用的写法，超出就**报错**而不是猜）：
//
//	* / x / X                 任意版本
//	1.2.3        =1.2.3       精确
//	1.2 / 1                   部分版本（npm 语义：1.2 = >=1.2.0 <1.3.0）
//	^1.2.3  ~1.2.3            caret / tilde（0.x 的 caret 按 npm 收窄）
//	>=1.2  >1.2  <=1.2  <1.2  比较符
//	1.2.3 - 2.3.4             连字符区间
//	A || B                    或；空格分隔 = 且
//
// 预发布参与比较（0.2.2-go.1 与 0.2.2 是两个不同版本），与官方一致。
func SatisfiesRange(version, expression string) (bool, error) {
	current, err := parseSemver(version)
	if err != nil {
		return false, fmt.Errorf("版本 %q 不是 SemVer: %w", version, err)
	}
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return false, fmt.Errorf("范围不能为空")
	}
	if strings.HasPrefix(expression, "workspace:") {
		// 官方：engines.dsh 上的 workspace: 一律不满足。
		return false, fmt.Errorf("workspace:%s 不是版本范围", strings.TrimPrefix(expression, "workspace:"))
	}
	for _, alternative := range strings.Split(expression, "||") {
		ok, err := satisfiesConjunction(current, alternative)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// satisfiesConjunction 判一个"空格分隔的且"（也接受连字符区间）。
func satisfiesConjunction(current semVersion, alternative string) (bool, error) {
	alternative = strings.TrimSpace(alternative)
	if alternative == "" {
		return false, fmt.Errorf("范围里有空的选项")
	}
	if alternative == "*" || alternative == "x" || alternative == "X" {
		return true, nil
	}
	if left, right, ok := splitHyphenRange(alternative); ok {
		low, err := parsePartial(left)
		if err != nil {
			return false, err
		}
		high, err := parsePartial(right)
		if err != nil {
			return false, err
		}
		return compareSemver(current, low.lower()) >= 0 && compareSemver(current, high.upperExclusive()) < 0, nil
	}
	for _, token := range strings.Fields(alternative) {
		ok, err := satisfiesComparator(current, token)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// splitHyphenRange 拆 `A - B`（两端都不为空才是连字符区间）。
func splitHyphenRange(alternative string) (string, string, bool) {
	parts := strings.Split(alternative, " - ")
	if len(parts) != 2 {
		return "", "", false
	}
	left, right := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if left == "" || right == "" {
		return "", "", false
	}
	return left, right, true
}

// satisfiesComparator 判单个比较符。
func satisfiesComparator(current semVersion, token string) (bool, error) {
	operator := ""
	rest := token
	for _, candidate := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(token, candidate) {
			operator = candidate
			rest = strings.TrimSpace(token[len(candidate):])
			break
		}
	}
	if rest == "" {
		return false, fmt.Errorf("比较符 %q 缺版本", token)
	}
	if rest == "*" || rest == "x" || rest == "X" {
		switch operator {
		case "", "=", ">=", "<=", "^", "~":
			return true, nil
		case ">":
			return false, nil
		case "<":
			return false, nil
		}
	}
	partial, err := parsePartial(rest)
	if err != nil {
		return false, err
	}
	low := partial.lower()
	high := partial.upperExclusive()
	switch operator {
	case "", "=":
		if partial.hasPatch {
			return compareSemver(current, low) == 0, nil
		}
		return compareSemver(current, low) >= 0 && compareSemver(current, high) < 0, nil
	case ">=":
		return compareSemver(current, low) >= 0, nil
	case ">":
		if partial.hasPatch {
			return compareSemver(current, low) > 0, nil
		}
		return compareSemver(current, high) >= 0, nil
	case "<":
		return compareSemver(current, low) < 0, nil
	case "<=":
		if partial.hasPatch {
			return compareSemver(current, low) <= 0, nil
		}
		return compareSemver(current, high) < 0, nil
	case "^":
		return compareSemver(current, low) >= 0 && compareSemver(current, partial.caretUpper()) < 0, nil
	case "~":
		return compareSemver(current, low) >= 0 && compareSemver(current, partial.tildeUpper()) < 0, nil
	}
	return false, fmt.Errorf("不认识的比较符 %q", operator)
}

// partialVersion 是可能缺 minor/patch 的版本（`1` / `1.2` / `1.2.3`）。
type partialVersion struct {
	major, minor, patch uint64
	hasMinor, hasPatch  bool
	pre                 []string
	build               string
}

// parsePartial 解一个可能缺段的版本；prerelease/build 仍按 SemVer 校验。
func parsePartial(text string) (partialVersion, error) {
	var out partialVersion
	value := strings.TrimSpace(text)
	if strings.HasPrefix(value, "v") {
		value = value[1:]
	}
	if value == "" {
		return out, fmt.Errorf("版本不能为空")
	}
	if index := strings.IndexByte(value, '+'); index >= 0 {
		out.build = value[index+1:]
		value = value[:index]
		if out.build == "" || !validIdentifiers(out.build) {
			return out, fmt.Errorf("非法 build metadata")
		}
	}
	if index := strings.IndexByte(value, '-'); index >= 0 {
		pre := value[index+1:]
		value = value[:index]
		if !validIdentifiers(pre) {
			return out, fmt.Errorf("非法 prerelease")
		}
		out.pre = strings.Split(pre, ".")
		for _, part := range out.pre {
			if isDigits(part) && len(part) > 1 && part[0] == '0' {
				return out, fmt.Errorf("prerelease 数字有前导零")
			}
		}
	}
	segments := strings.Split(value, ".")
	if len(segments) > 3 {
		return out, fmt.Errorf("版本段数超过 3")
	}
	numbers := make([]uint64, 0, 3)
	for _, segment := range segments {
		// `x` / `X` / `*` 是通配段（npm：`0.2.x` 等价于 `0.2` 的部分版本）。
		// 通配段之后的所有段都只能是通配（`0.x.3` 非法，与 npm 一致）。
		if segment == "x" || segment == "X" || segment == "*" {
			break
		}
		if !isDigits(segment) || len(segment) > 1 && segment[0] == '0' {
			return out, fmt.Errorf("版本段 %q 不是合法数字", segment)
		}
		number, err := strconv.ParseUint(segment, 10, 64)
		if err != nil {
			return out, err
		}
		numbers = append(numbers, number)
	}
	out.major = numbers[0]
	if len(numbers) > 1 {
		out.minor = numbers[1]
		out.hasMinor = true
	}
	if len(numbers) > 2 {
		out.patch = numbers[2]
		out.hasPatch = true
	}
	return out, nil
}

// lower 是这一段的**下界**（缺段按 0 补）。
func (p partialVersion) lower() semVersion {
	return semVersion{core: [3]uint64{p.major, p.minor, p.patch}, pre: p.pre}
}

// upperExclusive 是"这一段覆盖的最后一个版本之后"（部分版本的比较用）。
func (p partialVersion) upperExclusive() semVersion {
	switch {
	case !p.hasMinor:
		return semVersion{core: [3]uint64{p.major + 1, 0, 0}}
	case !p.hasPatch:
		return semVersion{core: [3]uint64{p.major, p.minor + 1, 0}}
	default:
		return semVersion{core: [3]uint64{p.major, p.minor, p.patch + 1}}
	}
}

// caretUpper 是 `^` 的上界（npm 语义：0.x 收窄到下一个不破坏兼容的段）。
func (p partialVersion) caretUpper() semVersion {
	switch {
	case p.major > 0:
		return semVersion{core: [3]uint64{p.major + 1, 0, 0}}
	case p.hasMinor && p.minor > 0:
		return semVersion{core: [3]uint64{0, p.minor + 1, 0}}
	case p.hasMinor && p.hasPatch:
		return semVersion{core: [3]uint64{0, 0, p.patch + 1}}
	case p.hasMinor:
		return semVersion{core: [3]uint64{0, 1, 0}}
	default:
		return semVersion{core: [3]uint64{1, 0, 0}}
	}
}

// tildeUpper 是 `~` 的上界（有 minor 就锁 minor，否则锁 major）。
func (p partialVersion) tildeUpper() semVersion {
	if p.hasMinor {
		return semVersion{core: [3]uint64{p.major, p.minor + 1, 0}}
	}
	return semVersion{core: [3]uint64{p.major + 1, 0, 0}}
}
