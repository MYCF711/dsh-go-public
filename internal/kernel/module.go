package kernel

import (
	"fmt"
	"log/slog"
	"os"
	"sort"

	toml "github.com/pelletier/go-toml/v2"
)

// Backend 是插件装载层（PLAN §3）。
type Backend string

const (
	// BackendStatic 编译进二进制，零装载开销——内核与官方默认实现用这层。
	BackendStatic Backend = "static"
	// BackendWasm 由 wazero 装载，用于频繁替换的纯逻辑。
	BackendWasm Backend = "wasm"
	// BackendProcess 由 go-plugin 装载，用于第三方 / 重依赖 / 崩溃隔离。
	BackendProcess Backend = "process"
)

// Module 是一个可装配的插件。默认实现与第三方插件形状完全相同（A3）。
type Module struct {
	ID       string
	Backend  Backend
	Provides []string       // 本模块提供的能力（服务键名）
	Requires []string       // 本模块依赖的能力
	Builtin  bool           // true = 内置模式，用户层不可覆盖
	Config   map[string]any // 来自 manifest 的配置
	Register func(*Scope) error
}

// Manifest 是 dsh.toml。
type Manifest struct {
	// Present 表示磁盘上真的有这个文件（决定 Apply 是"全开"还是"白名单"）。
	Present bool `toml:"-"`

	Profile struct {
		Name string `toml:"name"`
	} `toml:"profile"`
	Modules []ModuleEntry `toml:"module"`
}

// ModuleEntry 是 manifest 里的一行。
type ModuleEntry struct {
	ID       string         `toml:"id"`
	Backend  Backend        `toml:"backend"`
	Path     string         `toml:"path"`
	Enabled  *bool          `toml:"enabled"`
	Provides []string       `toml:"provides"`
	Requires []string       `toml:"requires"`
	Config   map[string]any `toml:"config"`
}

// implementedBackends 是**真的能被装载**的后端集合。
//
// wasm：manifest 声明它之后，装配前会由 cmd/dshd/wasmload.go 真的装载并校验
// （魔数 / 编译 / 可选 digest）。**装载不等于提供能力**——wasm 模块的 Provides
// 目前恒为空，不进工具表；那一步要等工具契约映射。
// process 目前只有常量声明，没有任何实现（PLAN §3.1 写的是门槛，不是完成）。
//
// 把它们里的任何一个漏掉，后果都是"我改了配置，却什么都没发生"——
// 这类静默失效最难查，因为它连个错误都没有。process 因此仍然在拒绝名单外。
var implementedBackends = map[Backend]bool{
	"":            true, // 未声明 = 编译进来的默认实现
	BackendStatic: true,
	BackendWasm:   true,
}

// LoadManifest 读 dsh.toml。文件不存在不算错（返回空 manifest）。
func LoadManifest(path string) (*Manifest, error) {
	m := &Manifest{}
	if path == "" {
		return m, nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// 没有 manifest 不是错误：二进制要能单独跑。
		return m, nil
	}
	m.Present = true
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 manifest %s 失败: %w", path, err)
	}
	if err := toml.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("解析 manifest %s 失败: %w", path, err)
	}
	return m, nil
}

// Apply 按 manifest 过滤、覆盖并排序待装配模块。
//
// 语义（对齐 DSH 的 applyEntryPatches）：按 id 命中；`enabled = false` 是禁用；
// config 是**整块替换**而不是 merge。manifest 里出现未编译进来的 id 是错误，
// 不是静默忽略——否则用户以为改了配置，其实什么都没发生。
func (m *Manifest) Apply(mods []Module) ([]Module, error) {
	byID := make(map[string]int, len(mods))
	known := make([]string, 0, len(mods))
	for i, mod := range mods {
		if mod.ID == "" {
			return nil, fmt.Errorf("模块 #%d 没有 ID", i)
		}
		if _, dup := byID[mod.ID]; dup {
			return nil, fmt.Errorf("模块 id %q 重复注册", mod.ID)
		}
		byID[mod.ID] = i
		known = append(known, mod.ID)
	}

	// 没有 manifest 文件 = 默认全开。manifest 在场时才当白名单用。
	if !m.Present {
		return mods, nil
	}

	// 白名单会**静默**丢掉没列出来的模块。踩过：新加一个内置模块、忘了写进
	// dsh.toml，于是工具表里少一个，没有任何提示——查了半天以为是注册失败。
	// 这里把"被白名单挡掉的"打一行日志，让人一眼看到。
	if len(m.Modules) > 0 {
		listed := make(map[string]bool, len(m.Modules))
		for _, e := range m.Modules {
			listed[e.ID] = true
		}
		for _, id := range known {
			if !listed[id] {
				slog.Warn("manifest 是白名单，这个模块没列出来，已跳过", "module", id)
			}
		}
	}

	seen := make(map[string]bool, len(m.Modules))
	out := make([]Module, 0, len(mods))
	for _, e := range m.Modules {
		if e.ID == "" {
			return nil, fmt.Errorf("manifest 里有一行 module 缺 id")
		}
		idx, ok := byID[e.ID]
		if !ok {
			sort.Strings(known)
			return nil, fmt.Errorf("manifest 指定的模块 %q 没有编译进二进制；可用模块: %v", e.ID, known)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("manifest 里模块 %q 出现多次", e.ID)
		}
		seen[e.ID] = true

		if e.Backend != "" && !implementedBackends[e.Backend] {
			return nil, fmt.Errorf(
				"模块 %q 声明的 backend %q 还没有实现（当前只有 static / wasm）；"+
					"要么去掉这一行用编译进来的实现，要么等这一层落地——"+
					"静默降级会让\"我改了配置却什么都没发生\"变成一类查不出来的故障",
				e.ID, e.Backend)
		}

		// enabled = false 是**真的关掉**。
		// 踩过：这个字段以前只解析不使用，于是 manifest 里写着 enabled=false，
		// 模块照样装配、工具照样在表里——"改了配置却什么都没发生"。
		if e.Enabled != nil && !*e.Enabled {
			continue
		}

		mod := mods[idx]
		if e.Backend != "" {
			mod.Backend = e.Backend
		}
		if len(e.Provides) > 0 {
			mod.Provides = e.Provides
		}
		if len(e.Requires) > 0 {
			mod.Requires = e.Requires
		}
		if e.Config != nil {
			mod.Config = e.Config // 整块替换，不 merge
		}
		out = append(out, mod)
	}
	return out, nil
}
