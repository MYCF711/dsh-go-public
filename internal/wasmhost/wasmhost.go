// Package wasmhost 是真的能把一个 wasm 模块装载起来并调用它的那一层。
//
// 边界（现在只做这一层）：
//   - 能装载、能按名字调导出函数、能卸载、能给出内容摘要；
//   - 工具契约的跨边界映射在 abi.go：`alloc` + `call` 两个导出，
//     args 与结果都是线性内存里的一段 JSON（形状见 docs/wasm-tool-abi.md §3）；
//   - 仍然不接 manifest：谁把这些包装成 spi.Tool 是装配层的事
//     （cmd/dshd/wasmload.go）。kernel 不认识 wasm。
//
// 为什么单独一个包而不是塞进 kernel：kernel 的红线是"不识业务"，
// 而 wazero 是一个有重量的运行时依赖。放在这里，kernel 编译期不背它。
package wasmhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// wasmMagic 是 wasm 的二进制的头四字节（\0asm）。
// 单独检查它而不是"编译失败就当不是 wasm"：错误要能一眼看出是"文件给错了"，
// 还是"文件是对的但里面有问题"。
var wasmMagic = []byte{0x00, 0x61, 0x73, 0x6d}

// Module 是一个已装载的 wasm 模块。
//
// 用法：Load → Call / InvokeTool（可多次）→ Close。Close 幂等。
//
// **实例会死**：客人 trap、超时、取消之后，这个实例就不能再用了
// （超时/取消时 wazero 直接把 module 关掉，实测 IsClosed=true；
// trap 时 wazero **不**关，但客人是 panic 出来的，Go 运行时状态没人担保——
// 见 docs/wasm-tool-abi.md §3.3/§3.4）。死了就该丢，让它下次调用重建，
// 而不是把"这个模块永远调不动"留给用户重启宿主。
//
// 重建靠的是**常驻的编译产物**（compiled）：重新 InstantiateModule 只要 2–4 ms，
// 而重新读文件 + 编译 4.4 MB 的 Go 模块要 ~0.9 s（§3.5 实测）。
// 所以这里把 compiled 与装载参数（reactor / tool / timeout）一起留在 Module 上，
// 重建时一个字节都不用再去磁盘拿——文件在运行期间被换掉也不会静默改变行为
// （内容身份由 digest 在装载时钉死）。
type Module struct {
	name   string
	path   string
	digest string

	// tool 是写进 ABI 请求信封的工具名，默认等于模块名。
	// 它是**装载层参数**，不是客人状态：重建实例后必须原样继续用。
	tool string
	// timeout 是单次 InvokeTool 的 deadline（0 = DefaultTimeout）。
	timeout time.Duration

	runtime  wazero.Runtime
	compiled wazero.CompiledModule // 常驻：实例重建的唯一凭据
	// reactor 说的是这个模块有没有 _initialize（重建时要再跑一次）。
	// 不重跑它，Go 客人一进导出函数就 runtime.notInitialized（unreachable）。
	reactor bool

	mod api.Module
	// log 保留**当前实例**最近几 KiB 的 stdout/stderr，trap 时拼进错误。
	// 重建时换一个新的：上一个实例的输出属于上一次失败，混进来只会误导。
	log *outputLog

	// dead 是"当前实例不可信了"。不靠 IsClosed 一个信号：
	// trap 之后 IsClosed 仍是 false（实测），但那个实例不该再用。
	dead       bool
	deadReason string
	// rebuildErr 是**上一次重建**失败的原因（成功就清空）。
	// 与 deadReason 分开存：错一次不该把"实例为什么死"这句话改写成
	// "实例已失效（实例已失效（…））"——排查的人需要的是两条独立的事实。
	rebuildErr string
	// gen 是实例代数（装载时 1，每重建一次 +1），rebuilds 是自动重建次数。
	// 两个都只用于诊断与测试：并发用例靠 Rebuilds() 证明"只重建一次"。
	gen      int64
	rebuilds int64
	// onRebuild 只有测试会设置：让"重建发生了"这件事可观察、可放慢。
	onRebuild func()

	mu     sync.Mutex
	closed bool
}

// RebuildTimeout 是一次自动重建的上限。
//
// 为什么要有上限：实例化本身只要 2–4 ms，但 `_initialize` 里跑的是插件作者的代码。
// 没有上限，一个卡住的 _initialize 会把"从坏状态里恢复"变成一次永久等待——
// 正是这次要消灭的那种故障。
const RebuildTimeout = 30 * time.Second

// outputLog 是客人 stdout/stderr 的环形缓冲（只留最后 4 KiB）。
//
// 为什么不是 io.Discard：上一轮 spike 的根因就丢在这里——Go 运行时自己把
// "runtime: wasmexport function called before runtime initialization /
// call _initialize first" 写到 stderr，而 Load 接的是 nil，于是宿主只剩
// 一句 "wasm error: unreachable"，把「少跑一个函数」查成了「不支持 Go 产物」。
// 客人的话可以很长，但**最后一句通常就是原因**。
type outputLog struct {
	mu  sync.Mutex
	buf []byte
}

const outputLogCap = 4096

func (l *outputLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > outputLogCap {
		l.buf = l.buf[len(l.buf)-outputLogCap:]
	}
	return len(p), nil
}

func (l *outputLog) tail() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(string(l.buf))
}

// Load 从文件装载一个 wasm 模块。
//
// 五件事都必须发生，否则调用方会得到一个"看起来能用"的东西：
//   - 文件不存在 / 不是 wasm → 明确报错，错误里带路径与原因；
//   - 算出内容的 sha256（manifest 校验要用）；
//   - 装 WASI 导入：Go / TinyGo 的 wasip1 产物都导入 wasi_snapshot_preview1，
//     不实例化它连模块都装不上（实测：module[wasi_snapshot_preview1] not instantiated）；
//   - 按导出表判形态并跑对的 start：有 `_initialize` 就是 reactor，必须跑它，
//     否则任何 //go:wasmexport 函数一进就被 runtime.notInitialized 打断（unreachable）；
//     只有 `_start` 的是 command 构建，明确报错让人重编，而不是装上一个调不动的模块；
//   - 任何一步失败都清理干净，不留 runtime、不留文件句柄。
func Load(ctx context.Context, name, path string) (m *Module, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 wasm 失败（%s）: %w", path, err)
	}
	if len(raw) < 4 || string(raw[:4]) != string(wasmMagic) {
		return nil, fmt.Errorf("%s 不是 wasm 模块：文件不以 \\0asm 开头", path)
	}
	sum := sha256.Sum256(raw)

	// CloseOnContextDone：ctx 取消时能把正在跑的 wasm 打断，
	// 而不是把宿主的 goroutine 永久钉在一个死循环里（夹具里 spin() 就是干这个的）。
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	ok := false
	defer func() {
		if !ok {
			_ = rt.Close(context.Background())
		}
	}()

	// 只挂 wasi_snapshot_preview1 的最小面；**不**挂任何 FS preopen、
	// 不传 args/env。隔离因此是被强制出来的，不是靠客人自我介绍。
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return nil, fmt.Errorf("实例化 wasi_snapshot_preview1 失败（%s）: %w", path, err)
	}

	compiled, err := rt.CompileModule(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("编译 wasm 失败（%s）: %w", path, err)
	}

	// 形态判定必须自己做：配错的 start 名 wazero **不会报错**，静默跳过，
	// 然后调用时才 trap（实测见 docs/wasm-tool-abi.md §1.2(c)）。
	exports := compiled.ExportedFunctions()
	_, hasInit := exports["_initialize"]
	if !hasInit {
		if _, hasStart := exports["_start"]; hasStart {
			return nil, fmt.Errorf("%s 是 command 形态的 wasm（只导出 _start、没有 _initialize）："+
				"这类模块跑完 main 就 proc_exit，宿主调不到任何导出函数。"+
				"Go 客人请用 GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared 重编成 reactor", path)
		}
	}

	log := &outputLog{}
	cfg := wazero.NewModuleConfig().
		WithName(instanceName(1)).
		WithStdout(log).WithStderr(log)
	if hasInit {
		// reactor：先把 Go/TinyGo 运行时拉起来，再允许调用导出。
		cfg = cfg.WithStartFunctions("_initialize")
	}
	mod, err := rt.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		if hasInit {
			// _initialize 失败的错误里不带客人 stderr，补上——原因常常就在那儿。
			if tail := log.tail(); tail != "" {
				return nil, fmt.Errorf("实例化 wasm 失败（%s）: %w\n客人输出: %s", path, err, tail)
			}
		}
		return nil, fmt.Errorf("实例化 wasm 失败（%s）: %w", path, err)
	}

	ok = true
	return &Module{
		name: name, path: path, digest: hex.EncodeToString(sum[:]),
		tool: name, timeout: DefaultTimeout,
		runtime: rt, compiled: compiled, reactor: hasInit,
		mod: mod, log: log, gen: 1,
	}, nil
}

// instanceName 是这个实例在 wazero store 里的名字。
//
// 为什么带代数：死掉的实例可能还挂在 store 里（trap 之后 wazero 并不关它），
// 用同一个名字再实例化会撞 "module already instantiated"——
// 那就把"重建"变成了新的坑。名字只是诊断标识，客人不 import 自己。
func instanceName(gen int64) string {
	if gen <= 1 {
		return "plugin"
	}
	return fmt.Sprintf("plugin#%d", gen)
}

// Rebuilds 返回这个模块自动重建实例的次数（诊断用；并发用例靠它断言"只重建一次"）。
func (m *Module) Rebuilds() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rebuilds
}

// InstanceGen 返回当前实例的代数（装载时 1，每重建一次 +1）。
func (m *Module) InstanceGen() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

// deadReasonOr 给"实例为什么不能用"一句人话；有记录就用记录。
// 调用方必须持有 m.mu。
func (m *Module) deadReasonOr(fallback string) string {
	if s := strings.TrimSpace(m.deadReason); s != "" {
		if m.rebuildErr != "" {
			// 上一次重建也失败过：这句话对下一个读错误的人有用——
			// 它说明"不是第一次了"，别把希望押在重试上。
			return s + "；之后自动重建也失败过（" + m.rebuildErr + "）"
		}
		return s
	}
	if m.mod != nil && m.mod.IsClosed() {
		return "实例已被 wazero 关闭（超时、取消或客人自己 proc_exit）"
	}
	return fallback
}

// ensureLiveLocked 保证 m.mod 是一个还能调的实例；不能就**重建一次**。
//
// 调用方必须持有 m.mu。并发语义就落在"必须持锁"这一条上：
// 两个 goroutine 同时撞上死实例时，先到的那个重建、后到的那个在锁外等，
// 拿到的是重建好的实例再顺手判一次——所以是"只重建一次"，
// 而不是"两个都重建、把其中一个实例漏在 store 里"。
func (m *Module) ensureLiveLocked(ctx context.Context) error {
	if m.closed {
		return fmt.Errorf("模块 %s 已卸载，不能再调用", m.name)
	}
	if m.mod != nil && !m.dead && !m.mod.IsClosed() {
		return nil
	}
	why := m.deadReasonOr("实例状态未知")
	// 死掉的实例先收干净：它自己不会回来，留着白占线性内存（Go 产物 3.5 MiB）。
	if m.mod != nil {
		_ = m.mod.Close(context.Background())
		m.mod = nil
	}
	if m.runtime == nil || m.compiled == nil {
		return fmt.Errorf(
			"wasm 模块 %q 的实例已失效（%s），而且装载时的编译产物已经释放，**无法重建**：只能重启宿主",
			m.name, why)
	}
	return m.rebuildLocked(ctx, why)
}

// rebuildLocked 用常驻的编译产物重新实例化一次（并再跑一遍 _initialize）。
//
// 失败时把**两句话**都写进错误：实例为什么死、重建为什么也失败。
// 只写"重建失败"等于把排查起点从"插件的哪个错误"挪到"宿主的哪一层"，那是在帮倒忙。
func (m *Module) rebuildLocked(ctx context.Context, why string) error {
	rctx, cancel := context.WithTimeout(ctx, RebuildTimeout)
	defer cancel()

	gen := m.gen + 1
	log := &outputLog{}
	cfg := wazero.NewModuleConfig().
		WithName(instanceName(gen)).
		WithStdout(log).WithStderr(log)
	if m.reactor {
		// 重建必须重跑 _initialize：Go 客人的运行时状态是每个实例各自的。
		cfg = cfg.WithStartFunctions("_initialize")
	}

	mod, err := m.runtime.InstantiateModule(rctx, m.compiled, cfg)
	if err != nil {
		cause := err
		if tail := log.tail(); tail != "" {
			cause = fmt.Errorf("%w\n客人输出: %s", err, tail)
		}
		m.dead = true
		m.deadReason = why           // 原话保留：不把两层失败拼成一句绕口令
		m.rebuildErr = cause.Error() // 下一次再失败时把这条也带上
		return fmt.Errorf(
			"wasm 模块 %q 的实例已失效（%s），**自动重建也失败了**: %v。"+
				"这个模块现在调不动——先按上面的原因修插件或环境再重启宿主；"+
				"原样重试不会成功", m.name, why, cause)
	}

	m.mod = mod
	m.log = log
	m.dead = false
	m.deadReason = ""
	m.rebuildErr = ""
	m.gen = gen
	m.rebuilds++
	if m.onRebuild != nil {
		m.onRebuild()
	}
	return nil
}

// markDeadLocked 把当前实例判死：下一次调用会重建它。
//
// 判死只有两类理由（都是"这次调用之后实例不可信"）：客人 trap、
// 调用被超时/取消打断。**宿主协议错不在此列**：那是两边对不上，
// 重建一万次也还是对不上，把实例丢掉只会让下一个错误更难查。
//
// 调用方必须持有 m.mu。
func (m *Module) markDeadLocked(reason string) {
	if m.closed {
		return
	}
	m.dead = true
	m.deadReason = reason
}

// instanceFailureReason 判断一次调用失败是不是"实例坏了"，坏了就返回判死理由。
//
// 为什么必须区分：trap 之后 wazero 不关实例，宿主若不管，下次调用会拿到
// 一个 panic 过的 Go 运行时继续跑——那是"看起来能用"的最坏形态。
func instanceFailureReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrGuestTrap):
		return "上一次调用里客人 trap（插件崩溃）", true
	case errors.Is(err, context.DeadlineExceeded):
		return "上一次调用超时，wazero 把实例关掉了", true
	case errors.Is(err, context.Canceled):
		return "上一次调用被取消，wazero 把实例关掉了", true
	}
	return "", false
}

// stderrTail 返回客人最近的输出（trap 时拼进错误）。
func (m *Module) stderrTail() string {
	if m == nil || m.log == nil {
		return ""
	}
	return m.log.tail()
}

// Name / Path / Digest 是模块的身份信息。Digest 是内容的 sha256（小写十六进制）。
func (m *Module) Name() string   { return m.name }
func (m *Module) Path() string   { return m.path }
func (m *Module) Digest() string { return m.digest }

// Exports 返回模块导出的**函数**名，按字典序排序。
//
// 只列函数、不列内存/全局/表：这一层能做的事就是 Call，
// 列出来的应当正好是能调的。排序是为了输出稳定——
// 它会被 `dshd wasm inspect` 直接打到屏幕上，也会进将来的清单快照，
// 而 map 的遍历顺序每次都不一样。
//
// 已卸载的模块返回 nil（没有导出可说了），不报错：
// 这个方法描述的是"模块里有什么"，不是一次可能失败的操作。
func (m *Module) Exports() []string {
	// 导出表来自编译产物，重建实例不会改变它；但 m.mod 会被重建换掉，
	// 所以整段持锁读——否则 -race 会抓到"重建正在换指针"。
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.mod == nil {
		return nil
	}
	defs := m.mod.ExportedFunctionDefinitions()
	out := make([]string, 0, len(defs))
	for name := range defs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Call 调用一个导出函数。
//
// 找不到这个导出函数时**报错而不是返回零值**：把"没有这个函数"说成"调用成功、结果是 0"
// 是最难查的一类错误。用 ExportedFunction 先查，就是为了这个。
//
// 实例死了（上次 trap / 超时）会先重建一次再调，与 InvokeTool 走同一条路：
// 这条是给 `dshd wasm call` 用的诊断入口，它尤其不能"一次 trap 之后就永久坏掉"。
func (m *Module) Call(ctx context.Context, fn string, args ...uint64) ([]uint64, error) {
	// 一次只有一条调用能进实例：call 是同步导出，重入会踩客人自己的全局状态。
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureLiveLocked(ctx); err != nil {
		return nil, err
	}
	f := m.mod.ExportedFunction(fn)
	if f == nil {
		return nil, fmt.Errorf("模块 %s 没有导出函数 %q（可能是别的名字，或编译时没带 wasmexport）", m.name, fn)
	}
	res, err := f.Call(ctx, args...)
	if err != nil {
		wrapped := fmt.Errorf("调用 %s.%s 失败: %w", m.name, fn, err)
		if why, dead := instanceFailureReason(wrapped); dead {
			m.markDeadLocked(why)
		}
		return nil, wrapped
	}
	return res, nil
}

// Close 卸载模块并释放 runtime。可重复调用。
//
// 字段在锁里取快照、关闭动作在锁外做：关闭一个正在被调用的实例本来就是误用，
// 但"读字段"和"重建换指针"之间的数据竞争不该顺带存在。
func (m *Module) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	mod := m.mod
	rt := m.runtime
	m.mu.Unlock()

	var first error
	if mod != nil {
		if err := mod.Close(ctx); err != nil {
			first = err
		}
	}
	if rt != nil {
		if err := rt.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}
