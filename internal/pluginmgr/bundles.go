package pluginmgr

// 只读投影：官方 `pluginManager.*` 的读方法要的是"真实已安装包 + 保留版本 +
// 来源 spec + 启用位 + 包内文档"，这些都在 `state.json` 与 `packages/<id>/<version>/`
// 里，不需要任何新存储。本文件只读，不写盘、不改状态（R5：读路径不写盘）。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// scopedPackagePattern 是 npm 作用域包名（`@scope/name`、`@scope/name@1.2.3`）。
// 它含 `/`，但绝不是相对路径——不先认出来，registry 规格会被误判成本地目录。
var scopedPackagePattern = regexp.MustCompile(`^@[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*(@[^\s]+)?$`)

// BundleState 是一个已安装包的真实投影（含保留版本与每版来源）。
//
// 官方把一个**包**叫 bundle；本仓可安装单元就是 `dsh.local-plugin@1` 包，
// 因此 pluginManager.* 的 bundle 名 == 包 id（见 RESULT 的映射表）。
type BundleState struct {
	Name            string
	Version         string
	Enabled         bool
	Kind            string
	Entry           string
	Source          string
	PreviousVersion string
	// Versions 是保留版本，按 SemVer 升序；VersionSources 是每版的来源 spec。
	Versions       []string
	VersionSources map[string]string
}

// Bundles 返回全部已安装包的真实投影（按 id 排序，与 List 一致）。
func (m *Manager) Bundles() ([]BundleState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := sortedPluginIDs(m.state)
	out := make([]BundleState, 0, len(ids))
	for _, id := range ids {
		state, err := m.bundleStateLocked(id)
		if err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	return out, nil
}

// Bundle 返回单个包的真实投影；没安装时 ok=false（不是错误）。
func (m *Manager) Bundle(name string) (BundleState, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Plugins[name] == nil {
		return BundleState{}, false, nil
	}
	state, err := m.bundleStateLocked(name)
	if err != nil {
		return BundleState{}, false, err
	}
	return state, true, nil
}

func (m *Manager) bundleStateLocked(id string) (BundleState, error) {
	record := m.state.Plugins[id]
	info, err := m.recordInfoLocked(id, record)
	if err != nil {
		return BundleState{}, err
	}
	sources := make(map[string]string, len(record.Versions))
	versions := make([]string, 0, len(record.Versions))
	for version, source := range record.Versions {
		sources[version] = source.Source
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool {
		left, leftErr := parseSemver(versions[i])
		right, rightErr := parseSemver(versions[j])
		if leftErr != nil || rightErr != nil {
			return versions[i] < versions[j]
		}
		return compareSemver(left, right) < 0
	})
	return BundleState{
		Name: id, Version: record.Version, Enabled: record.Enabled,
		Kind: info.Kind, Entry: info.Entry, Source: sources[record.Version],
		PreviousVersion: record.PreviousVersion,
		Versions:        versions, VersionSources: sources,
	}, nil
}

// ReadDocument 读取已安装包**当前版本**里的一个文档。
//
// candidates 按顺序尝试（例如 README.md / readme.md）；只接受 manifest 里
// 登记过 hash 的文件——这样"读文档"不会变成读包目录里的任意文件。
// 返回包内的 `/` 分隔路径、内容与是否找到；文档超过 1 MiB 视为太大（fail-loud，
// 与 manifest 的大小边界同量级）。
func (m *Manager) ReadDocument(name string, candidates []string) (string, []byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.state.Plugins[name]
	if record == nil {
		return "", nil, false, notFound(name)
	}
	// 先按真实校验确认当前包完好（读路径也要对状态负责）。
	if _, err := m.recordInfoLocked(name, record); err != nil {
		return "", nil, false, err
	}
	manifest, err := readAndValidatePackage(m.packagePath(name, record.Version))
	if err != nil {
		return "", nil, false, err
	}
	for _, candidate := range candidates {
		if _, ok := manifest.Files[candidate]; !ok {
			continue
		}
		path := filepath.Join(m.packagePath(name, record.Version), filepath.FromSlash(candidate))
		info, err := os.Stat(path)
		if err != nil {
			return "", nil, false, internalError(err)
		}
		if info.Size() > 1<<20 {
			return "", nil, false, errorf("document_too_large", 422, false,
				"拆小包内文档；本仓只读 ≤1MiB 的文档", "包 %s@%s 的 %s 有 %d 字节，超过上限",
				name, record.Version, candidate, info.Size())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", nil, false, internalError(err)
		}
		return candidate, raw, true, nil
	}
	return "", nil, false, nil
}

// ValidateSource 是安装前对**本地目录包**的真实校验（manifest、路径、hash、
// 未登记文件…），不写任何状态。
//
// 它给官方 `pluginManager.inspect` 用：inspect 必须给出与 install 完全一致的
// 判定，否则界面会"检查通过、安装失败"。
func ValidateSource(source string) (Manifest, error) {
	return readAndValidatePackage(source)
}

// SourceIsLocalPath 判据：这个 spec 是不是一个本地目录（本仓唯一能装的形态）。
//
// 只有"看起来像路径"的字符串才会被 stat：一个 registry 名（`acme.bundle`）
// 绝不能被当成同名的相对目录去 stat，否则 inspect 会把 cwd 里的巧合目录
// 说成"已接受的本地包"。
func SourceIsLocalPath(spec string) bool {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../") ||
		strings.HasPrefix(trimmed, ".\\") || strings.HasPrefix(trimmed, "..\\") {
		return true
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "\\") {
		return true
	}
	// Windows 盘符（C:\ / C:/），或 UNC（\\server\share）。
	if len(trimmed) >= 3 && trimmed[1] == ':' && (trimmed[2] == '\\' || trimmed[2] == '/') {
		return true
	}
	return strings.ContainsAny(trimmed, "/\\")
}

// SourceKind 把 spec 归到官方的四种形态之一：path / registry / git / tarball。
// 它只看字符串形态，不 stat、不联网；判定"存不存在"是调用方的事。
func SourceKind(spec string) string {
	trimmed := strings.TrimSpace(spec)
	switch {
	case scopedPackagePattern.MatchString(trimmed):
		// `@scope/name` 是 registry 名，不是 `./@scope/name` 这样的相对目录。
		return "registry"
	case strings.HasPrefix(trimmed, "git+"), strings.HasPrefix(trimmed, "git://"),
		strings.HasPrefix(trimmed, "github:"), strings.HasPrefix(trimmed, "gitlab:"),
		strings.HasSuffix(trimmed, ".git"):
		return "git"
	case strings.HasPrefix(trimmed, "http://"), strings.HasPrefix(trimmed, "https://"):
		if strings.HasSuffix(trimmed, ".tgz") || strings.HasSuffix(trimmed, ".tar.gz") ||
			strings.HasSuffix(trimmed, ".tar") {
			return "tarball"
		}
		return "registry"
	case strings.HasSuffix(trimmed, ".tgz") || strings.HasSuffix(trimmed, ".tar.gz"):
		return "tarball"
	case SourceIsLocalPath(trimmed):
		return "path"
	default:
		return "registry"
	}
}

// SourceIsRegistrySpec 判据：spec 是否是一个 registry 依赖（name 或 name@range）。
//
// 本仓的包一律从本地目录安装，来源抵押在 `state.json` 的 `versions[].source`
// 里；判断"能不能 updatable"就是看这个来源是不是 registry spec。
func SourceIsRegistrySpec(source string) bool {
	return SourceKind(source) == "registry"
}

// CompareVersions 比较两个 SemVer：-1 左小、0 相等、1 左大。
// 非法版本返回错误（不猜）。
func CompareVersions(left, right string) (int, error) {
	leftVersion, err := parseSemver(left)
	if err != nil {
		return 0, fmt.Errorf("非法版本 %q: %w", left, err)
	}
	rightVersion, err := parseSemver(right)
	if err != nil {
		return 0, fmt.Errorf("非法版本 %q: %w", right, err)
	}
	return compareSemver(leftVersion, rightVersion), nil
}

// DescribeSource 给诊断用：把来源压成一句可读的话（不泄露本机绝对路径之外的信息）。
func DescribeSource(source string) string {
	if source == "" {
		return "（来源缺失）"
	}
	if SourceIsRegistrySpec(source) {
		return source
	}
	if !SourceIsLocalPath(source) {
		return fmt.Sprintf("%s（%s）", source, SourceKind(source))
	}
	return "本地目录 " + source
}
