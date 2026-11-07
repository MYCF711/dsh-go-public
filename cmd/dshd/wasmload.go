package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
	"dshgo/internal/wasmhost"
)

// 这一层做两件事：**装载校验** 与 **装配成工具**。
//
// manifest 里写 `backend = "wasm"` 的模块，装配前会真的被 wasmhost 装起来：
//   - 文件不存在 / 不是 wasm / 编译不过 → 装配失败，错误里带路径与 wasmhost 的原始原因；
//   - 写了 digest 但和文件内容对不上 → 装配失败。这正是 manifest 校验存在的意义：
//     内容被换掉要在**启动时**炸，而不是等第一次调用才发现；
//   - 通过校验的模块登记进装配结果。
//
// 写不写工具能力键（`provides = ["tool"]` 或 `["tool:<工具名>"]`，多个就多个键），
// 决定它到底提不提供能力：
//   - **写了** → 生成一个 kernel.Module，其 Register 往工具表里注册这一行声明的
//     **全部** spi.Tool（一个模块可以挂多个工具：`[[module.tool]]` 数组）。
//     名字/描述/schema/caps/超时全部来自 manifest，不从客人取。模型能看见它们、
//     能调它们，这就是"wasm 插件真的提供了工具"的落点。
//     多个 wasm 工具共存时，每个工具一个能力键（理由见 wasmToolCap）。
//   - **没写** → 行为与这一层落地之前完全一致：Provides 为空、Register 为 nil、
//     不进工具表，只能 `dshd wasm call` 手动调。这条"不变"是硬要求：
//     没声明要提供工具的人，不该因为升级宿主就多出一个模型可见的工具。
//
// 为什么不把 digest 塞进 kernel.Module：kernel 的红线是"不识业务"，
// 也不该为了一次显示多一个字段；模块的身份（路径 / 摘要 / 导出）留在这一层。

// wasmToolCap 是 wasm 后端在 manifest 里认的能力名（单数，不是内核服务键 "tools"）。
//
// 为什么是 `"tool"` 而不是内核服务键 `"tools"`（复数）：这是两个概念。
// "tools" 是**别人提供的服务**（工具目录），wasm 模块要 `Requires` 它才能注册；
// "tool" 是**本模块要贡献的东西**。混成一个名字会让模块看起来像在提供工具目录，
// 而它其实只是往里加一行。
//
// 为什么还要有 `"tool:<工具名>"` 这种写法：kernel 的"一个能力只允许一个提供者"
// 是按**字符串**判的（internal/kernel/assemble.go:64），而 Apply 会把 manifest 里
// 的 provides 原样复制到每个模块上（internal/kernel/module.go:167）——
// 装载层没有任何钩子能在 Apply 之后改写它。所以两个 wasm 工具（不管是两个模块，
// 还是同一个模块里的两个工具）想同时存在，能力键就必须本来就是两个不同的字符串。
// "tool" 是单数场景的缩写（全署只允许出现一次）；"tool:<工具名>" 是多个工具时的写法：
// 它把"哪个模块贡献了哪个工具"直接写进能力名，kernel 的判重于是自然成立，
// 不需要内核认识 wasm、也不需要给它开特例。
const wasmToolCap = "tool"

// wasmToolCapPrefix 是"能力键带上工具名"的前缀，拼出来的键必须与同一行的 tool_name 一致。
const wasmToolCapPrefix = wasmToolCap + ":"

// wasmLoaded 是本次装配里真正装起来的一个 wasm 模块。
//
// module 句柄留在这里而不是用完就关：装载意味着它确实能被调用。
// 提供工具的模块，Register 闭包里直接握着同一个 *wasmhost.Module。
type wasmLoaded struct {
	id      string
	path    string
	digest  string
	exports []string
	module  *wasmhost.Module
	// tools 是它注册进工具表的工具名（按 manifest 顺序）；空表示不提供能力。
	//
	// 一个模块可以有多个工具：wasm 侧的"一个工具"不是一个模块，而是
	// 模块里的一个**分支**（客人按请求信封的 tool 字段分流）。
	tools []string
}

var (
	wasmMu      sync.Mutex
	wasmCurrent []wasmLoaded
)

// wasmEntry 是 manifest 里 wasm 关心的那几个字段。
//
// 为什么不把 digest 加到 kernel.ModuleEntry：kernel 的红线是"不识业务"，
// digest 是 **wasm 文件内容**的身份，属于装载层；kernel 只需要 id/backend，
// 不需要知道这条路径后面躺着一个 .wasm。
//
// 代价是同一份 manifest 在这一层再解析一遍——只解析 [[module]] 行，
// 读的是同一份字节，不存在"两份真相"。
type wasmEntry struct {
	ID       string   `toml:"id"`
	Backend  string   `toml:"backend"`
	Path     string   `toml:"path"`
	Digest   string   `toml:"digest"`
	Enabled  *bool    `toml:"enabled"`
	Provides []string `toml:"provides"`

	// 以下五个是**旧的单工具写法**（一个模块一个工具），只在 provides 含工具键时有意义。
	//
	// 为什么不从客人那里问：spi.Tool 的 Name/Description/Schema/Caps 是
	// **稳定性契约**（R4），审批与沙箱也靠 Caps 判定（AGENTS §2）。
	// 让客人自己声明"我只会读"，等于让它自己给自己发许可证。
	// 所以这些只有 manifest 一个来源，改动要走显式审查。
	ToolName        string   `toml:"tool_name"`
	ToolDescription string   `toml:"tool_description"`
	ToolSchema      string   `toml:"tool_schema"`
	ToolCaps        []string `toml:"tool_caps"`
	TimeoutMS       int      `toml:"timeout_ms"`

	// Tool 是**多工具写法**：`[[module.tool]]` 数组，一项一个工具。
	//
	// 与上面那五个字段二选一：两处都写就启动即炸（见 newWasmToolSpecs），
	// 宿主不会替你猜哪个优先。旧写法一个字都不用改，继续有效。
	Tool []wasmToolDecl `toml:"tool"`
}

// wasmToolDecl 是 `[[module.tool]]` 的一项。
//
// 字段名与 spill 的写法一致（name/description/schema/caps），唯一的例外是
// timeout_ms 可以逐项写：同一个模块里两个工具的超时未必一样。
type wasmToolDecl struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Schema      string   `toml:"schema"`
	Caps        []string `toml:"caps"`
	TimeoutMS   int      `toml:"timeout_ms"`
}

type wasmManifest struct {
	Modules []wasmEntry `toml:"module"`
}

// readWasmEntries 只把 manifest 里的 [[module]] 行读出来。文件不存在 = 没有 wasm 模块。
func readWasmEntries(manifestPath string) ([]wasmEntry, error) {
	if strings.TrimSpace(manifestPath) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 没有 manifest 不是错误（与 kernel.LoadManifest 一致）
		}
		return nil, fmt.Errorf("读 manifest %s 失败: %w", manifestPath, err)
	}
	var f wasmManifest
	if err := toml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析 manifest %s 的 wasm 段失败: %w", manifestPath, err)
	}
	return f.Modules, nil
}

// loadWasmModules 装载并校验 manifest 里所有 backend = "wasm" 的模块，
// 返回要追加进候选列表的 kernel.Module。
//
// 返回的模块必须在 Apply **之前**并进候选：Apply 按 id 命中，
// 不在候选里的 id 会被判成"没有编译进二进制"——那正是这次要拆掉的那堵墙。
func loadWasmModules(manifestPath string) ([]kernel.Module, error) {
	entries, err := readWasmEntries(manifestPath)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		replaceWasm(nil)
		return nil, nil
	}
	base := manifestDirOf(manifestPath)
	ctx := context.Background()

	var (
		mods   []kernel.Module
		loaded []wasmLoaded
	)
	// fail 统一收尾：装到一半失败的模块也要关掉，不留 runtime、不留文件句柄。
	fail := func(err error) ([]kernel.Module, error) {
		closeWasmModules(loaded)
		return nil, err
	}

	// 第一遍：只校验 manifest，一个字节的 wasm 都不读。
	//
	// 为什么拆成两遍：一个 wasm 工具模块要编译 4.4 MB 的 Go 产物（~0.9 s），
	// 而"两个模块的能力键撞了""tool_name 写错了""两个模块用了同一个工具名"
	// 都是几微秒就能查出来的事。
	// 以前这两件事挤在一个循环里，于是第二行的错要等第一行编译完才报——
	// 而且多工具场景下这条错**必然**会发生（见 wasmToolCap 的注释）。
	//
	// 这一遍里有两张表，各管一件事，范围都是"本次装载"：
	//   - capOwner：能力键（"tool" / "tool:<名>"）的唯一性——kernel 按字符串判重；
	//   - toolOwner：**工具名**的唯一性——工具表按名字存、模型按名字调，
	//     这一条以前要等 spi.ToolsKey 的 Register 才炸（见 claimWasmToolNames）。
	type decl struct {
		specs   []*wasmToolSpec
		capKeys []string
	}
	decls := make(map[int]decl, len(entries))
	capOwner := make(map[string]string, len(entries))         // 能力键 → 模块 id
	toolOwner := make(map[string]wasmToolClaim, len(entries)) // 工具名 → 第一次声明它的那一处
	for i, e := range entries {
		if kernel.Backend(e.Backend) != kernel.BackendWasm || e.ID == "" {
			continue
		}
		if e.Enabled != nil && !*e.Enabled {
			continue
		}
		capKeys, unknown := splitWasmProvides(e.Provides)
		if len(unknown) > 0 {
			return fail(fmt.Errorf(
				"模块 %q 声明了 backend = \"wasm\" 和 provides = %v，但 wasm 后端只认 "+
					"provides = [\"tool\"]（一个工具）或 provides = [\"tool:<工具名>\", …] "+
					"（一个模块一个键，多工具就多个键）："+
					"%v 这些能力还没有对应实现。"+
					"在它们落地之前写进 manifest，就是在骗下一个读 manifest 的人",
				e.ID, e.Provides, unknown))
		}
		if len(capKeys) == 0 {
			// 声明了 `[[module.tool]]` 却没写 provides：配置说谎，必须炸。
			//
			// 为什么不"顺手帮你补一个 tool:<名字>"：能力的唯一来源是 manifest
			// （§8.4），装载层替它编一个，`modules graph` 上就会出现一行
			// manifest 里根本没写的键——省几行 TOML，弄丢一条判据。
			//
			// 老写法（只写 tool_name、不写 provides）保持历史行为不变：
			// 装载、但不提供能力（§8.6 第 4 条要求工具表逐字节不变）。
			if len(e.Tool) > 0 {
				return fail(noWasmToolCapErr(e))
			}
			continue
		}
		// 先校验工具声明，再装载 4 MiB 的 wasm：manifest 写错了没必要先编译一遍。
		specs, serr := newWasmToolSpecs(e)
		if serr != nil {
			return fail(serr)
		}
		if cerr := checkWasmTools(e.ID, capKeys, specs, e.Provides); cerr != nil {
			return fail(cerr)
		}
		// 工具名判重排在能力键判重**之前**：能力键是工具名的投影（"tool:<名>"），
		// 名字撞了才是根因。两个模块直接抄了同一段 [[module]] 时，两条错都会成立，
		// 先报"同名工具"这一条，用户改一处（改名字）就能同时解决两条。
		if derr := claimWasmToolNames(manifestPath, e, i+1, specs, toolOwner); derr != nil {
			return fail(derr)
		}
		for _, key := range capKeys {
			if prev, dup := capOwner[key]; dup {
				return fail(dupWasmToolCapErr(prev, e.ID, key))
			}
			capOwner[key] = e.ID
		}
		decls[i] = decl{specs: specs, capKeys: capKeys}
	}

	for i, e := range entries {
		if kernel.Backend(e.Backend) != kernel.BackendWasm {
			continue
		}
		if e.ID == "" {
			// 缺 id 的报错归 Apply，这里不抢它的台词。
			continue
		}
		// enabled = false 就是真的关掉：连文件都不读。
		// 一个被禁用的模块不该因为路径坏了把整个启动拖下水。
		//
		// 但 id 仍要登记进候选：Apply 是"先按 id 找模块、再判断 enabled"，
		// 不登记的话它会把"我写了这行、这次关掉"报成"没编译进二进制"。
		if e.Enabled != nil && !*e.Enabled {
			mods = append(mods, kernel.Module{ID: e.ID, Backend: kernel.BackendWasm})
			continue
		}
		specs := decls[i].specs

		p := strings.TrimSpace(e.Path)
		if p == "" {
			return fail(fmt.Errorf(
				"模块 %q 声明了 backend = \"wasm\"，但没有 path：wasm 后端必须给出 .wasm 文件路径", e.ID))
		}
		if !filepath.IsAbs(p) {
			// 相对路径按 manifest 所在目录解析，不是按当前工作目录：
			// 否则同一条命令换个 cwd 跑，指到的就是另一个文件。
			p = filepath.Join(base, p)
		}
		// 再取绝对路径：报错和 `modules graph` 里都能一眼看出到底读了哪个文件，
		// 而不是一个要靠 cwd 去补全的相对路径。
		if abs, aerr := filepath.Abs(p); aerr == nil {
			p = abs
		}

		m, err := wasmhost.Load(ctx, e.ID, p)
		if err != nil {
			return fail(fmt.Errorf("装配 wasm 模块 %q 失败（%s）: %w", e.ID, p, err))
		}
		actual := m.Digest()
		exports := m.Exports()
		entry := wasmLoaded{id: e.ID, path: p, digest: actual, exports: exports, module: m}
		// router 是"一个模块上的多个工具共用同一个实例"时的那个句柄：
		// 它拿着模块，也拿着那把"设工具名 + 调用"必须原子的锁。
		router := &wasmToolRouter{mod: m}
		if len(specs) > 0 {
			// 超时是**执行期**参数。多工具模块里每个工具可以有自己的 timeout_ms，
			// 所以模块级超时取其中最大的那个当安全上限，真正的 per-tool deadline
			// 由 Exec 用 ctx 收得更紧（见 wasmTool.Exec）。实例重建之后这两个参数
			// 继续生效（它们是宿主侧状态，不在客人里）。
			m.SetTimeout(maxWasmToolTimeout(specs))
			// 单工具模块顺手把模块级工具名设成它：`dshd wasm call` 这类不经过
			// spi.Tool 的入口仍然需要"这个模块默认是谁"。多工具模块不设——
			// 每次 Exec 都会在路由锁里把名字写对，模块级那个默认值没有意义。
			if len(specs) == 1 {
				m.SetToolName(specs[0].name)
			}
			for _, sp := range specs {
				entry.tools = append(entry.tools, sp.name)
			}
		}
		// 先登记再校验：任何一处校验失败，fail 都会把它一起关掉。
		loaded = append(loaded, entry)

		if want := strings.ToLower(strings.TrimSpace(e.Digest)); want != "" {
			if len(want) != 64 {
				return fail(fmt.Errorf(
					"模块 %q 的 digest 不是 64 位十六进制 sha256（写的是 %q，长度 %d）",
					e.ID, e.Digest, len(want)))
			}
			if _, herr := hex.DecodeString(want); herr != nil {
				return fail(fmt.Errorf(
					"模块 %q 的 digest 不是十六进制（写的是 %q）: %v", e.ID, e.Digest, herr))
			}
			if want != actual {
				return fail(fmt.Errorf(
					"模块 %q 的 digest 不匹配（摘要校验失败）：manifest 写的是 %s，文件 %s 实际的 sha256 是 %s。"+
						"内容被换过就该在启动时炸，而不是等第一次调用才发现",
					e.ID, want, p, actual))
			}
		}

		if len(specs) > 0 {
			mods = append(mods, kernel.Module{
				ID:      e.ID,
				Backend: kernel.BackendWasm,
				// Requires 必须有 "tools"：工具目录是**别人**提供的服务，
				// 没有它，Register 里的 Inject 会失败；拓扑排序也靠它保证顺序。
				//
				// Provides 这里不写：能力的唯一来源是 manifest，Apply 会把
				// e.Provides（"tool" 或 "tool:<工具名>" 一个或多个）原样复制上来——
				// `modules graph` 的 PROVIDES 列因此能看到这一行，而 kernel 的
				// "一个能力一个提供者"也按这些字符串自然成立（多个工具 = 多个不同的键）。
				// 注意这些键都不是服务键：真正的注册靠下面这个 Register，
				// 它 Inject 的是 spi 的 "tools" 服务。两件事别混。
				Requires: []string{spi.ToolsKey.String()},
				// 一个 Register 注册**这个模块声明的全部工具**：
				// kernel 的一个模块只有一个 Register 钩子，而 wasm 的一个模块
				// 现在可以有多个工具。工具表里看到的是多个独立的 spi.Tool。
				Register: wasmToolRegister(specs, router),
			})
			slog.Info("wasm 模块已装载，并注册工具（模型可见）",
				"module", e.ID, "tools", strings.Join(entry.tools, ","),
				"provides", strings.Join(decls[i].capKeys, ","), "path", p,
				"digest", shortDigest(actual), "exports", strings.Join(exports, ","),
				"timeout", maxWasmToolTimeout(specs))
			continue
		}

		mods = append(mods, kernel.Module{
			ID:      e.ID,
			Backend: kernel.BackendWasm,
			// Provides 必须为空：它不提供能力这件事要在**装配结果里**就是真的，
			// 不能只靠日志里一句话。
		})

		slog.Warn("wasm 模块已装载并通过校验，但**不提供任何能力**：不进工具表、模型调不到，只能 `dshd wasm call` 手动调"+
			"（要让它提供工具：单工具在 manifest 里写 provides = [\"tool\"] 并给出 tool_name / tool_description / tool_schema；"+
			"多工具写 provides = [\"tool:<工具名>\", …] 并为每个工具写一节 [[module.tool]]）",
			"module", e.ID, "path", p, "digest", shortDigest(actual),
			"exports", strings.Join(exports, ","))
	}

	replaceWasm(loaded)
	return mods, nil
}

// splitWasmProvides 把 manifest 的 provides 拆成「工具能力键」与「不认识的能力」两半。
//
// 认两种写法（理由见 wasmToolCap 的注释）：
//   - "tool"            —— 缩写：这个模块贡献它那**一个**工具。全部署只允许出现一次；
//   - "tool:<工具名>" —— 一个工具一个键；单工具模块也可以用它，
//     一个模块挂多个工具时必须每个工具各写一个。
//
// 返回值保持 manifest 顺序（去重留给 checkWasmTools 报错，不在这里悄悄吃掉）：
// 同一行里写两次同一个键是笔误，报出来比替用户合并更有用。
func splitWasmProvides(provides []string) (capKeys []string, unknown []string) {
	for _, raw := range provides {
		c := strings.TrimSpace(raw)
		switch {
		case c == wasmToolCap, strings.HasPrefix(c, wasmToolCapPrefix):
			capKeys = append(capKeys, c)
		default:
			unknown = append(unknown, c)
		}
	}
	return capKeys, unknown
}

// noWasmToolCapErr 是"声明了 [[module.tool]] 但 provides 里一个工具键都没有"的报法。
//
// 为什么必须炸而不是"顺手帮你补一个键"：能力的唯一来源是 manifest（§8.4）。
// 装载层替它编一个，`modules graph` 的 PROVIDES 列就会出现一行 manifest 里
// 根本没写的键；省下的只是几行 TOML，弄丢的是一条判据。
func noWasmToolCapErr(e wasmEntry) error {
	names := make([]string, 0, len(e.Tool))
	keys := make([]string, 0, len(e.Tool))
	for _, d := range e.Tool {
		names = append(names, strings.TrimSpace(d.Name))
		keys = append(keys, wasmToolCapPrefix+strings.TrimSpace(d.Name))
	}
	return fmt.Errorf(
		"模块 %q 声明了 %d 个工具（[[module.tool]]：%s），但 provides 里没有任何工具能力键："+
			"能力键就是\"哪个模块提供了哪个工具\"，必须显式写出来，宿主不会替你补。"+
			"请加一行：provides = [%s]",
		e.ID, len(e.Tool), strings.Join(names, "、"), quoteJoin(keys))
}

// quoteJoin 把能力键拼成能直接粘回 TOML 的模样：["tool:echo", "tool:upper"]。
func quoteJoin(keys []string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = `"` + k + `"`
	}
	return strings.Join(q, ", ")
}

// checkWasmTools 校验「provides 里的工具键」与「声明的工具」是不是同一批东西。
//
// 为什么必须校验：能力键会进 `modules graph`，而工具名会进模型看到的工具表。
// 两处不一致时，图和表就各说各话——而这种错在 manifest 里只是几个字符，
// 到运行期完全看不出来。所以：**对不上就启动即炸**，并把两边都列出来。
//
// 语义（三种写法的边界）：
//   - provides = ["tool"]：这个模块只有**一个**工具，它就是这个键；
//   - provides = ["tool:echo","tool:upper"]：每个键对应 [[module.tool]] 里的一项；
//   - 两种混写（"tool" 与 "tool:x" 同时出现）：自相矛盾，报错而不是取其一。
func checkWasmTools(id string, capKeys []string, specs []*wasmToolSpec, provides []string) error {
	seen := make(map[string]bool, len(capKeys))
	for _, k := range capKeys {
		if seen[k] {
			return fmt.Errorf(
				"模块 %q 的 provides 里 %q 写了两次：能力键必须独一无二（它标识\"哪个模块提供了哪个工具\"）",
				id, k)
		}
		seen[k] = true
	}

	// 声明的工具名（按顺序，便于报错时照抄）。
	names := make([]string, 0, len(specs))
	declared := make(map[string]bool, len(specs))
	for _, s := range specs {
		names = append(names, s.name)
		declared[s.name] = true
	}

	bare := seen[wasmToolCap]
	named := make(map[string]bool, len(capKeys))
	for _, k := range capKeys {
		if k == wasmToolCap {
			continue
		}
		named[strings.TrimSpace(strings.TrimPrefix(k, wasmToolCapPrefix))] = true
	}

	// "tool" 是单工具缩写：配多个工具、或与 "tool:x" 同时出现，都是自相矛盾。
	if bare && len(named) > 0 {
		return fmt.Errorf(
			"模块 %q 的 provides = %v 同时写了缩写 %q 与 %q：一个能力键要么是单工具的缩写，"+
				"要么写出工具名，不许两种混着来。多工具请一个工具一个键：provides = [%s]",
			id, provides, wasmToolCap, wasmToolCapPrefix+"…", quoteJoin(capKeysOf(names)))
	}
	if bare && len(specs) != 1 {
		return fmt.Errorf(
			"模块 %q 写了 provides = [\"tool\"]，但声明了 %d 个工具（%s）：%q 是"+
				"「这个模块只有一个工具」的缩写，多工具必须一个工具一个键。请改成 provides = [%s]",
			id, len(specs), strings.Join(names, "、"), wasmToolCap, quoteJoin(capKeysOf(names)))
	}

	// 两边的差集都要报：只报一边，另一边就要人自己反推。
	var noTool, noKey []string
	for _, k := range capKeys {
		if k == wasmToolCap {
			continue
		}
		n := strings.TrimSpace(strings.TrimPrefix(k, wasmToolCapPrefix))
		if n == "" {
			return fmt.Errorf(
				"模块 %q 的 provides 里写了 %q：冒号后面必须写出工具名（例：provides = [\"tool:echo\"]，"+
					"与 [[module.tool]].name 一致）；只写个前缀等于让读者自己去猜哪个模块提供了什么", id, k)
		}
		if !declared[n] {
			noTool = append(noTool, k)
		}
	}
	if !bare {
		for _, s := range specs {
			if !named[s.name] {
				noKey = append(noKey, s.name)
			}
		}
	}
	if len(noTool) > 0 || len(noKey) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "模块 %q 的 provides 与声明的工具对不上（这一行声明了 %d 个工具：%s）",
			id, len(specs), strings.Join(names, "、"))
		if len(noTool) > 0 {
			fmt.Fprintf(&b, "；能力键 %s 没有对应的工具声明", quoteJoin(noTool))
		}
		if len(noKey) > 0 {
			fmt.Fprintf(&b, "；工具 %s 在 provides 里没有对应的能力键", quoteJoin(noKey))
		}
		fmt.Fprintf(&b, "。正确写法：provides = [%s]（冒号后与 [[module.tool]].name 逐个对齐）",
			quoteJoin(capKeysOf(names)))
		return errors.New(b.String())
	}
	return nil
}

// capKeysOf 把工具名翻成它们**应该**有的能力键，用于报错时给出可照抄的一行。
func capKeysOf(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, wasmToolCapPrefix+n)
	}
	return out
}

// wasmToolClaim 是全局工具名表里的一条：**谁**在 manifest 的**哪里**声明了这个名字。
//
// 为什么记 module / item 而不只记模块 id：报错要能指到"改哪一行"。
// 跨模块撞名时，人要找的是 `[[module]] id = "x"` 那一段里的 tool_name / [[module.tool]]；
// 同一个模块里撞名时，要找的是第几项 [[module.tool]]——只写一句模块 id 等于什么也没说，
// 而"改哪一行"恰恰是这条错误唯一比 Register 多出来的东西。
type wasmToolClaim struct {
	id     string
	module int    // 1 起的 [[module]] 序号（manifest 文件里的顺序，与装载顺序一致）
	item   int    // 1 起的 [[module.tool]] 项号；0 = 旧写法（tool_name 那几行）
	name   string // 声明的工具名
}

// section 是"在 manifest 的哪一节"，可照着找。
func (c wasmToolClaim) section() string {
	return fmt.Sprintf("第 %d 个 [[module]]（id = %q）", c.module, c.id)
}

// decl 是"用哪种写法声明的"，直接给出那一行长什么样。
func (c wasmToolClaim) decl() string {
	if c.item == 0 {
		// 旧写法：一个模块一个工具，写成模块级的 tool_name。
		return fmt.Sprintf("tool_name = %q", c.name)
	}
	// 新写法：这一名字写在 [[module.tool]] 的第几项里。
	return fmt.Sprintf("[[module.tool]] 第 %d 项（name = %q）", c.item, c.name)
}

// claimWasmToolNames 把这一行声明的工具名登记进**本次装载范围**的工具名表。
//
// 为什么必须在这一层查（而不是等 spi.ToolsKey 的 Register）：工具名进的是**同一张**
// 工具表——模型按名字调、注册表按名字存，所以两个模块声明同名工具时，第二处 Register
// 必然失败。但那条错误的原话是"wasm 工具 X 注册失败"，既不说是哪两个模块撞的，
// 也不说 manifest 里该改哪一行；而用户手上只有 manifest。在这里查，代价是几微秒，
// 换来的是"两个模块 id + 工具名 + 改哪一处 + 怎么改"一句说得全。
//
// 为什么同一个模块里的重复也走这一条：本来是 `[[module.tool]]` 写了两遍的笔误，
// 但撞的就是名字，报法没理由分叉；而"第几项"这种位置信息只该在一处维护。
// 也正因为这样，newWasmToolSpecs 不再自己查重名（原来那一份报错已并入这里）：
// 同一个笔误出现两种文案，用户还得自己判断哪条更权威。
//
// 范围是"本次装载"：调用方每次装载都新建表。这与工具表的语义一致——
// 一次装配一张表，跨装配的同名（比如换了 manifest 的两次启动）互不相干。
func claimWasmToolNames(manifestPath string, e wasmEntry, module int, specs []*wasmToolSpec, owner map[string]wasmToolClaim) error {
	for i, sp := range specs {
		item := 0
		if len(e.Tool) > 0 {
			// 只有新写法才有"第几项"：旧写法一行就是一个工具。
			item = i + 1
		}
		cur := wasmToolClaim{id: e.ID, module: module, item: item, name: sp.name}
		if prev, dup := owner[sp.name]; dup {
			return dupWasmToolNameErr(manifestPath, prev, cur)
		}
		owner[sp.name] = cur
	}
	return nil
}

// dupWasmToolNameErr 是"同一个工具名被声明了两次"的报法。
//
// 三件事必须说清，缺一条用户就得自己去猜：
//   - **哪两处撞了**（模块 id，以及同一模块时是第几项）；
//   - **改哪个文件/哪一行**（manifest 路径 + 第几个 [[module]] + 哪种写法）；
//   - **怎么改**（改名，并让 provides 里的能力键跟着改——那两处必须逐个对齐）。
//
// 为什么不说"注册失败"就完事：工具名是模型看到的身份，重名时模型分不清该调谁，
// 这不是"能不能跑"，是装配该不该继续；而能被这个名字影响的只有读 manifest 的人。
func dupWasmToolNameErr(manifestPath string, prev, cur wasmToolClaim) error {
	file := strings.TrimSpace(manifestPath)
	if file == "" {
		// 没有 manifest 文件就没有 wasm 模块，走不到这里；留着是为了不让路径为空。
		file = "dsh.toml"
	}
	// 同一个模块里的两项撞了：说"模块 A 与 A 都声明了"是废话，直接给"第几项"。
	if prev.id == cur.id {
		return fmt.Errorf(
			"模块 %q 里工具 %q 出现了两次：%s与 %s。同一个模块里的工具名也必须唯一"+
				"（模型按名字调、工具表按名字存，第二处登记必然失败）。"+
				"改其中一项的 name，并把 provides 里重复的那个 %q 跟着改"+
				"（位置：manifest %s，%s）",
			cur.id, cur.name, prev.decl(), cur.decl(),
			wasmToolCapPrefix+cur.name, file, cur.section())
	}
	return fmt.Errorf(
		"模块 %q 与 %q 都声明了工具 %q：工具名在装配范围内必须唯一"+
			"（模型按名字调、工具表按名字存，第二处登记必然失败）。"+
			"改其中一个的名字，并把两者的能力键改成两个不同的值——"+
			"例如 provides = [%q] 与 [%q]，并同步 tool_name / [[module.tool]].name。"+
			"位置：manifest %s：%s的 %s；%s的 %s",
		prev.id, cur.id, cur.name,
		wasmToolCapPrefix+cur.name, wasmToolCapPrefix+cur.name+"2",
		file, prev.section(), prev.decl(), cur.section(), cur.decl())
}

// dupWasmToolCapErr 是"两个 wasm 模块争同一个能力键"的报法。
//
// 为什么这条错误值得单独写一段：kernel 的原话是"能力 %q 被两个模块同时提供"，
// 它说的是**症状**；而这里能说的是**改法**。多工具部署下这条错误必然会出现，
// 让用户自己去 kernel 的报错里反推 provides 该写什么，是把成本推给了最不了解内核的人。
func dupWasmToolCapErr(prevID, id, capKey string) error {
	if capKey == wasmToolCap {
		return fmt.Errorf(
			"模块 %q 与 %q 都写了 provides = [\"tool\"]：kernel 只允许一个能力一个提供者，"+
				"所以 %q 这种缩写全署只能用一个。要同时挂多个 wasm 工具，请让每个模块写各自的能力键："+
				"provides = [\"tool:<tool_name>\"]（例：provides = [\"tool:echo\"]，冒号后必须与同一行的 "+
				"tool_name 一致）——能力键就是\"哪个模块提供了哪个工具\"，两个模块贡献的不是同一个东西",
			prevID, id, wasmToolCap)
	}
	return fmt.Errorf(
		"模块 %q 与 %q 都写了能力键 %q：能力键必须独一无二（它标识\"哪个模块提供了哪个工具\"）。"+
			"两个不同的工具请用各自的工具名，例如 provides = [\"tool:echo\"] 与 provides = [\"tool:hello\"]",
		prevID, id, capKey)
}

// wasmToolRegister 返回把**这个模块声明的全部工具**注册进工具表的 Register。
//
// 为什么 Register 要闭包握着模块句柄：装配层只能给 *kernel.Scope（R2），
// 句柄不能放进 kernel；闭包是唯一既守住 R2 又能拿到句柄的形状。
//
// 为什么一个 Register 注册多个工具：kernel 的一个模块只有一个 Register 钩子，
// 而 wasm 的一个模块现在可以有多个工具（manifest 的 `[[module.tool]]` 数组）。
// 工具表里看到的是多个**独立**的 spi.Tool：各自的名字/描述/schema/caps/超时，
// 各自出现在模型面前、各自独立审批；共用的只有那个 wasm 实例（见 wasmToolRouter）。
func wasmToolRegister(specs []*wasmToolSpec, r *wasmToolRouter) func(*kernel.Scope) error {
	tools := make([]*wasmTool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, &wasmTool{
			router:  r,
			name:    spec.name,
			desc:    spec.desc,
			schema:  spec.schema,
			caps:    spec.caps,
			timeout: spec.timeout,
		})
	}
	return func(s *kernel.Scope) error {
		reg, err := kernel.Inject(s, spi.ToolsKey)
		if err != nil {
			return err
		}
		for _, t := range tools {
			if err := reg.Register(t); err != nil {
				return fmt.Errorf("wasm 工具 %q 注册失败: %w", t.Name(), err)
			}
		}
		return nil
	}
}

// wasmToolSpec 是 manifest 里对一个 wasm 工具的全部声明。
//
// 每个字段都**只能**从 manifest 来：工具的 Name/Description/Schema/Caps 是
// 稳定性契约（R4），审批与沙箱也靠 Caps 判定（AGENTS §2）——
// 让客人自己声明"我只会读"，等于让它自己给自己发许可证。
type wasmToolSpec struct {
	name    string
	desc    string
	schema  json.RawMessage
	caps    spi.Caps
	timeout time.Duration
}

// newWasmToolSpecs 把 manifest 里这一行的工具声明校验并组装成一组 spec。
//
// 两种写法（二选一，混写报错）：
//   - 旧写法：tool_name / tool_description / tool_schema / tool_caps / timeout_ms，一个工具；
//   - 新写法：`[[module.tool]]` 数组，一项一个工具（每项 name/description/schema/caps[/timeout_ms]）。
//
// 为什么混写要**启动即炸**而不是猜优先级：两种写法都在回答"这个模块有哪些工具"，
// 同时出现时谁赢都是猜；猜错的表现是"模型少了一个工具"或"某个工具带着别人的
// description 出现在模型面前"，运行期完全查不出来。manifest 是唯一事实来源，
// 那就让它自己说清楚，而不是让宿主替它选一个。
func newWasmToolSpecs(e wasmEntry) ([]*wasmToolSpec, error) {
	if hasLegacyToolFields(e) && len(e.Tool) > 0 {
		return nil, fmt.Errorf(
			"模块 %q 同时写了单工具字段（tool_name / tool_description / tool_schema / tool_caps）"+
				"和 [[module.tool]] 数组：两种写法不能混用，宿主不会替你猜哪个优先。"+
				"只有一个工具就删掉 [[module.tool]] 只留 tool_name 那几行；"+
				"有多个工具就删掉 tool_* 那一组，每个工具写一节 [[module.tool]]", e.ID)
	}

	if len(e.Tool) == 0 {
		// 旧写法：逐字段等价于 [[module.tool]] 里的唯一一项。
		// 这条路径必须与改造前**完全一致**——既有 manifest 一个字都不用改。
		spec, err := newWasmToolSpec(e.ID,
			wasmToolDecl{
				Name:        e.ToolName,
				Description: e.ToolDescription,
				Schema:      e.ToolSchema,
				Caps:        e.ToolCaps,
			},
			e.TimeoutMS,
			"tool_name / tool_description / tool_schema 这一组")
		if err != nil {
			return nil, err
		}
		return []*wasmToolSpec{spec}, nil
	}

	// 这里**不查重名**：同一个模块里两项同名与两个模块撞名是同一件事（工具名撞了），
	// 报法也共用一条——见 claimWasmToolNames。两处各写一份的话，同一个笔误会得到
	// 两种文案，而"第几项""哪个文件"这类位置信息也会分散在两个地方维护。
	specs := make([]*wasmToolSpec, 0, len(e.Tool))
	for i, d := range e.Tool {
		where := fmt.Sprintf("[[module.tool]] 第 %d 项", i+1)
		timeoutMS := d.TimeoutMS
		if timeoutMS == 0 {
			// 模块级 timeout_ms 是多工具写法的**默认值**：一项一个写法太啰嗦，
			// 而"这一行所有工具共用一个超时"是常见情况。0 = 没写。
			timeoutMS = e.TimeoutMS
		}
		spec, err := newWasmToolSpec(e.ID, d, timeoutMS, where)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// hasLegacyToolFields 判断这一行是不是用了旧的单工具写法。
//
// 只看 tool_* 那几个字段，**不看** timeout_ms：timeout_ms 在多工具写法里
// 仍然合法（当默认值用），它不构成"两种写法混用"。
func hasLegacyToolFields(e wasmEntry) bool {
	return strings.TrimSpace(e.ToolName) != "" ||
		strings.TrimSpace(e.ToolDescription) != "" ||
		strings.TrimSpace(e.ToolSchema) != "" ||
		len(e.ToolCaps) > 0
}

// newWasmToolSpec 校验并组装**一个**工具声明。任何一处不对都在装配时炸。
//
// where 说明它写在 manifest 的哪一处（"tool_name …" 还是 "[[module.tool]] 第 2 项"）：
// 报错时点名，用户不用猜是哪一项写错了；两种写法也因此共用同一份校验。
//
// 为什么全都必填：漏了工具名就是模型看到一个空名字的工具；
// 漏了描述就是模型不知道什么时候该用它；漏了 schema 就是参数没法校验。
// 这些错误在运行时表现为"模型不会用这个工具"，极难归因，所以放在启动路径上失败。
func newWasmToolSpec(id string, d wasmToolDecl, timeoutMS int, where string) (*wasmToolSpec, error) {
	name := strings.TrimSpace(d.Name)
	if name == "" {
		return nil, fmt.Errorf(
			`模块 %q 的 %s 没有工具名（tool_name / name）：工具名是模型看到的名字，必须显式给出`+
				`（旧写法例：tool_name = "echo"；多工具例：[[module.tool]] 里 name = "echo"）`, id, where)
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return nil, fmt.Errorf(
			"模块 %q 的工具名 %q 里有空白字符（%s）：工具名要能被模型原样写进 tool_calls，不能带空格",
			id, name, where)
	}

	desc := strings.TrimSpace(d.Description)
	if desc == "" {
		return nil, fmt.Errorf(
			"模块 %q 的工具 %q 没有描述（tool_description / description）：Description 是"+
				"模型看到的「什么时候用它」，缺了它模型只能靠猜，几乎不会正确调用", id, name)
	}

	raw := strings.TrimSpace(d.Schema)
	if raw == "" {
		return nil, fmt.Errorf(
			`模块 %q 的工具 %q 没有参数 schema（tool_schema / schema）：schema 必须写在 manifest 里`+
				`（例：tool_schema = '''{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}'''）`,
			id, name)
	}
	// 必须是**对象**：工具参数永远是一个 JSON 对象，顶层不能是数组或标量。
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil || probe == nil {
		return nil, fmt.Errorf(
			"模块 %q 的工具 %q 的 schema 不是合法的 JSON 对象：%v（原文 %q）", id, name, err, raw)
	}

	caps, err := parseWasmCaps(d.Caps)
	if err != nil {
		return nil, fmt.Errorf("模块 %q 的工具 %q: %w", id, name, err)
	}

	timeout := wasmhost.DefaultTimeout
	if timeoutMS < 0 {
		return nil, fmt.Errorf("模块 %q 的 %s 的 timeout_ms = %d，不能是负数", id, where, timeoutMS)
	}
	if timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}

	return &wasmToolSpec{
		name: name, desc: desc,
		schema:  json.RawMessage(raw),
		caps:    caps,
		timeout: timeout,
	}, nil
}

// maxWasmToolTimeout 是给模块级 SetTimeout 的那一个值：取全部工具里最大的超时。
//
// 为什么取最大：模块级 timeout 是"这次调用最长能跑多久"的安全上限，
// 而每个工具真正的 deadline 由 Exec 用自己的 ctx 收得更紧（见 wasmTool.Exec）。
// 取最小会把超时长的工具冤枉掉，只有取最大才只是兜底。
func maxWasmToolTimeout(specs []*wasmToolSpec) time.Duration {
	var max time.Duration
	for _, s := range specs {
		if s.timeout > max {
			max = s.timeout
		}
	}
	if max <= 0 {
		return wasmhost.DefaultTimeout
	}
	return max
}

// wasmToolRouter 是"一个模块上有多个工具"时的那把路由锁。
//
// 请求信封里的 tool 字段是宿主**唯一**告诉客人"你现在是哪个工具"的方式
// （docs/wasm-tool-abi.md §3.2；协议早就为这一天留好了这个字段，本轮一个字节没改）。
// 但它在 abi 层是**模块级状态**（Module.SetToolName），没有 per-call 参数——
// 所以"写名字 + 调用"必须原子：两个工具并发时，A 写完名字、B 又写一遍，
// A 就会以 B 的身份跑起来（输出看起来正常，只是答非所问，最难查）。
//
// 这把锁的代价是零：一个实例本来就只能跑一个 call（§3.4，同步导出不能重入），
// 同一个模块上的两个工具本来也得串行；锁只是把"串行"从实例扩到"写名字 + 调用"。
type wasmToolRouter struct {
	mod *wasmhost.Module
	mu  sync.Mutex
}

// invokeTool 在锁里写工具名，并按**这个工具**自己的超时调一次客人。
//
// 为什么超时要在这里再收一次：模块级 SetTimeout 取的是全部工具里最大的那个
// （见 maxWasmToolTimeout），所以超时短的工具必须自己把 ctx 卡紧，
// 否则它会拿到别人的长超时——manifest 写的 timeout_ms 就成了摆设。
func (r *wasmToolRouter) invokeTool(ctx context.Context, name string, timeout time.Duration, args []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mod.SetToolName(name)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return r.mod.InvokeTool(ctx, args)
}

// parseWasmCaps 把 manifest 的 tool_caps 翻成 spi.Caps。
//
// 默认全 false 不是客人的自我介绍，而是**沙箱强制**的：宿主不给客人任何
// FS preopen、不传 args/env、只挂 wasi_snapshot_preview1 的最小面。
// manifest 仍然可以声明更严的能力（比如一个算完要人批的工具写 destructive），
// 那只是让审批更早介入，不是说客人真能碰到文件系统。
func parseWasmCaps(names []string) (spi.Caps, error) {
	var c spi.Caps
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "":
			// TOML 里多写一个空串不算错，忽略。
		case "writes":
			c.Writes = true
		case "executes":
			c.Executes = true
		case "network":
			c.Network = true
		case "destructive":
			c.Destructive = true
		default:
			return c, fmt.Errorf(
				"tool_caps 里有不认识的能力 %q：只认 writes / executes / network / destructive"+
					"（与 spi.Caps 的四个字段一一对应，拼错会让审批判错档）", n)
		}
	}
	return c, nil
}

// wasmTool 是 spi.Tool 在 wasm 上的实现。
//
// 身份全部来自 manifest，只有 Exec 真的跨到客人那边。
//
// 多个工具共用一个 wasm 实例（一个模块一条 4.4 MB 的编译产物、一个常驻实例），
// 所以"我是谁"必须每次调用跟着请求走：router 负责把工具名写进信封的 tool 字段。
type wasmTool struct {
	router  *wasmToolRouter
	name    string
	desc    string
	schema  json.RawMessage
	caps    spi.Caps
	timeout time.Duration
}

func (t *wasmTool) Name() string            { return t.name }
func (t *wasmTool) Description() string     { return t.desc }
func (t *wasmTool) Schema() json.RawMessage { return t.schema }
func (t *wasmTool) Caps() spi.Caps          { return t.caps }

// Exec 走一次 ABI，并把三层失败收成模型能看懂的两类。
//
// 分层（与 wasmhost 的约定一一对应）：
//   - 客人 trap / 协议错 / 超时 → 插件坏了，不是参数错。Hint 明说"别原样重试"；
//   - 结果信封 isError:true → **业务失败**：Hint 直接来自客人，
//     客人没写就由这里兜一条可行动的，绝不留空。
//
// R3：失败必须 `Result{Hint:…}, err`，不许返回空 Result 把 Hint 丢掉——
// 模型拿不到 Hint 就只能瞎猜，一次失败会变成三四次无效重试。
func (t *wasmTool) Exec(ctx context.Context, args json.RawMessage) (spi.Result, error) {
	// 工具名与超时都在这一次调用里当场交给客人：
	// 同一个模块上的另一个工具可能就是刚被调完的那个（见 wasmToolRouter）。
	out, err := t.router.invokeTool(ctx, t.name, t.timeout, args)
	if err != nil {
		return spi.Result{
				Summary: fmt.Sprintf("wasm 插件 %s 执行失败", t.name),
				Hint:    hintForWasmError(err),
			},
			fmt.Errorf("wasm 工具 %q 执行失败: %w", t.name, err)
	}

	rep, perr := wasmhost.ParseReply(out)
	if perr != nil {
		return spi.Result{
				Summary: fmt.Sprintf("wasm 插件 %s 返回了无法识别的结果", t.name),
				Hint:    "插件返回的信封不符合 ABI（宿主侧问题）：不要重试，把这条错误报告给用户",
			},
			fmt.Errorf("wasm 工具 %q 的结果信封不合法: %w", t.name, perr)
	}

	if rep.IsError {
		hint := strings.TrimSpace(rep.Hint)
		if hint == "" {
			// 客人没给 hint 也要给模型一条可行动的：这正是 R3 要拦的形态。
			hint = "工具报告失败但没给 hint：按 summary/text 的说法改参数再试；同样的参数重试不会有不同结果"
		}
		summary := strings.TrimSpace(rep.Summary)
		if summary == "" {
			summary = "工具报告失败"
		}
		// err 的文本会被 agent 层投影成 "Error: …" 直接给模型看，
		// 所以它必须带上客人说的正文，而不是一句 "tool failed"。
		msg := summary
		if text := strings.TrimSpace(rep.Text); text != "" && text != summary {
			msg = summary + "：" + text
		}
		return spi.Result{Summary: summary, Text: rep.Text, Hint: hint}, errors.New(msg)
	}

	// 成功：Text 是客人渲染好的正文，projection 会原样用它。
	return spi.Result{Summary: rep.Summary, Text: rep.Text}, nil
}

// hintForWasmError 把 wasmhost 的错误翻成模型能执行的下一步。
//
// 为什么不用一句话概括所有错误：这四类的**正确反应完全不同**——
// trap 要换做法、超时缩小输入、取消重发、协议错只能报人。
// 给错方向比不给更糟：模型会拿着一句"重试"把同一个坑踩三遍。
func hintForWasmError(err error) string {
	switch {
	case errors.Is(err, wasmhost.ErrGuestTrap):
		return "插件内部崩溃（wasm trap），不是你的参数错了：不要原样重试；换一种做法，或改用别的工具"
	case errors.Is(err, context.DeadlineExceeded):
		return "插件执行超时：输入可能太大，缩小范围再试；连续超时要报告给用户" +
			"（超时后这个实例已被关闭，下一次调用会自动重建，不用重启宿主）"
	case errors.Is(err, context.Canceled):
		return "这次调用被取消（会话被中断）：需要时重新发起，不要把它当成插件失败"
	case errors.Is(err, wasmhost.ErrProtocol):
		return "插件与宿主的 ABI 对不上（宿主侧问题）：不要重试，把这条错误报告给用户"
	default:
		return "wasm 插件调用失败：先按错误原文判断，别反复重试同样的参数"
	}
}

// replaceWasm 换掉"当前装配里的 wasm 模块"，并关掉上一批。
//
// 为什么敢直接关旧的：这些句柄没有任何出口——模块 Provides 为空、没有 Register，
// 拿不到模块指针的调用方不可能还在用它。
func replaceWasm(next []wasmLoaded) {
	wasmMu.Lock()
	old := wasmCurrent
	wasmCurrent = next
	wasmMu.Unlock()
	closeWasmModules(old)
}

// closeLoadedWasm 卸载本次装配装起来的全部 wasm 模块。可重复调用。
//
// 谁该调：打印完就退出的命令（`modules graph|list`）。进程活着的命令
// （serve / acp / run）不调——模块本来就该活到进程结束。
func closeLoadedWasm(ctx context.Context) {
	wasmMu.Lock()
	cur := wasmCurrent
	wasmCurrent = nil
	wasmMu.Unlock()
	closeWasmModulesWith(ctx, cur)
}

func closeWasmModules(list []wasmLoaded) {
	closeWasmModulesWith(context.Background(), list)
}

func closeWasmModulesWith(ctx context.Context, list []wasmLoaded) {
	for _, x := range list {
		if x.module == nil {
			continue
		}
		if err := x.module.Close(ctx); err != nil {
			slog.Debug("关闭 wasm 模块失败", "module", x.id, "err", err)
		}
	}
}

// loadedWasm 返回本次装配里装起来的 wasm 模块（按 manifest 顺序）。
func loadedWasm() []wasmLoaded {
	wasmMu.Lock()
	defer wasmMu.Unlock()
	out := make([]wasmLoaded, len(wasmCurrent))
	copy(out, wasmCurrent)
	return out
}

// writeWasmSection 把 wasm 模块单独打一节：backend、digest 前 12 位、导出的工具名。
//
// 为什么单独一节而不是塞进主表：主表的列是"装配后提供哪些**能力（服务键）**"，
// 而 wasm 模块的能力键（"tool" / "tool:<tool_name>"）**不是服务键**——
// 工具不是一个被别人 Require 的服务，它是往工具表里加一行。
// 混进主表，读的人会以为它提供了 "tools" 这个服务（那才是别人提供的服务）。
// TOOL 列就是这条区分：提供了工具的模块在这里显示工具名，
// 其余显示"（不提供）"，一眼能分出这两类。
//
// 一个模块可以挂多个工具（`[[module.tool]]`），所以这一列是**逗号分隔的一串**：
// 只显示第一个会让读的人以为另外几个没注册上。
func writeWasmSection(w io.Writer) {
	list := loadedWasm()
	if len(list) == 0 {
		return
	}
	fmt.Fprintf(w, "\n# wasm 后端（已装载 / 已校验；TOOL 列 = 注册进工具表的工具名（多个用逗号分隔），"+
		"空表示不提供能力、只能 `dshd wasm call` 手动调）\n")
	fmt.Fprintf(w, "%-22s %-9s %-12s %-18s %-22s %s\n", "MODULE", "BACKEND", "DIGEST", "EXPORTS", "TOOL", "PATH")
	for _, x := range list {
		exports := strings.Join(x.exports, ",")
		if exports == "" {
			exports = "（无导出函数）"
		}
		tool := strings.Join(x.tools, ",")
		if tool == "" {
			tool = "（不提供）"
		}
		fmt.Fprintf(w, "%-22s %-9s %-12s %-18s %-22s %s\n",
			x.id, string(kernel.BackendWasm), shortDigest(x.digest), exports, tool, x.path)
	}
}

// shortDigest 取 sha256 的前 12 位十六进制：够认出一个文件，又不至于把行撑爆。
func shortDigest(d string) string {
	if len(d) <= 12 {
		return d
	}
	return d[:12]
}

// manifestDirOf 是 manifest 里相对 path 的解析基准目录。
func manifestDirOf(manifestPath string) string {
	if strings.TrimSpace(manifestPath) == "" {
		return "."
	}
	d := filepath.Dir(manifestPath)
	if d == "" {
		return "."
	}
	return d
}
