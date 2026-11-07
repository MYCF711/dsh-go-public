package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

// wasmFixture 是 internal/wasmhost 的纯 wasm 夹具：60 字节，导出 add / spin。
// 跨包引用而不是复制一份：两个包看到同一份字节，"摘要对不上"才有意义。
// 测试的 cwd 是 cmd/dshd，所以要往上两级。
const wasmFixture = "../../internal/wasmhost/testdata/fixture.wasm"

// wasmToolFixture 是 Go reactor 夹具：导出 _initialize / alloc / call，实现工具 ABI。
//
// 两个夹具的分工：纯 wasm 那个验"装载 / 调用 / 超时打断"；
// Go 这个验"真的是一个工具"——没有它，_initialize 与 ABI 往返都测不到
// （见 internal/wasmhost/abi_test.go 与 docs/wasm-tool-abi.md §1.3）。
const wasmToolFixture = "../../internal/wasmhost/testdata/toolplugin.wasm"

// TestWasmModuleAssemblesAndShowsInGraph：合法 wasm 模块能装配，
// 装配结果里 backend 是 wasm、Provides 为空，且 `modules graph` / `list`
// 能看到它并显示 digest 前 12 位。
func TestWasmModuleAssemblesAndShowsInGraph(t *testing.T) {
	resetWasm(t)
	fixture := fixturePath(t)
	digest := sha256hexFile(t, fixture)
	logs := captureLogs(t)

	mp := writeManifest(t, `[profile]
name = "wasm"

[[module]]
id      = "wasm-probe"
backend = "wasm"
path    = "`+filepath.ToSlash(fixture)+`"
digest  = "`+digest+`"
`)
	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("合法的 wasm 模块必须能装配，实际报错：%v", err)
	}
	defer closeLoadedWasm(context.Background())

	var got *kernel.Module
	for i := range mods {
		if mods[i].ID == "wasm-probe" {
			got = &mods[i]
		}
	}
	if got == nil {
		t.Fatal("装配结果里没有 wasm-probe")
	}
	if got.Backend != kernel.BackendWasm {
		t.Fatalf("backend 应当是 wasm，实际 %q", got.Backend)
	}
	// 这条是"不许假装它提供能力"的落点：装配结果里必须真的是空的，
	// 而不是靠日志里一句话搪塞。
	if len(got.Provides) != 0 {
		t.Fatalf("wasm 模块现在不提供任何能力，Provides 应当为空，实际 %v", got.Provides)
	}
	if got.Register != nil {
		t.Fatal("还没做工具契约映射，wasm 模块不该有 Register")
	}

	list := loadedWasm()
	if len(list) != 1 {
		t.Fatalf("登记的 wasm 模块应当正好 1 个，实际 %d", len(list))
	}
	if list[0].digest != digest {
		t.Fatalf("登记的摘要应当是文件内容的 sha256：%s vs %s", list[0].digest, digest)
	}

	var graph bytes.Buffer
	if err := writeModules(&graph, "graph", mods); err != nil {
		t.Fatalf("modules graph 失败：%v", err)
	}
	g := graph.String()
	// 路径分隔符在 Windows 上是反斜杠，输出比对前统一成正斜杠：
	// 这里要钉的是"打出了哪个文件"，不是分隔符长什么样。
	gSlash := strings.ReplaceAll(g, `\`, "/")
	for _, want := range []string{"wasm-probe", "wasm", digest[:12], filepath.ToSlash(fixture)} {
		if !strings.Contains(gSlash, want) {
			t.Fatalf("modules graph 输出里应当有 %q：\n%s", want, g)
		}
	}

	var listing bytes.Buffer
	if err := writeModules(&listing, "list", mods); err != nil {
		t.Fatalf("modules list 失败：%v", err)
	}
	for _, want := range []string{"wasm-probe", "wasm", digest[:12]} {
		if !strings.Contains(listing.String(), want) {
			t.Fatalf("modules list 输出里应当有 %q：\n%s", want, listing.String())
		}
	}

	// 日志必须明说"它不提供能力"，而不是让人从空 PROVIDES 列里猜。
	if !strings.Contains(logs.String(), "不提供任何能力") {
		t.Fatalf("日志里必须明说 wasm 模块不提供任何能力：\n%s", logs.String())
	}
}

// TestWasmDigestMismatchFails：manifest 里的 digest 与实际文件不符 → 装配失败。
// 这正是 manifest 校验的意义：内容被换掉要在启动时炸。
func TestWasmDigestMismatchFails(t *testing.T) {
	resetWasm(t)
	fixture := fixturePath(t)
	real := sha256hexFile(t, fixture)
	wrong := strings.Repeat("0", 64)
	if wrong == real {
		t.Fatal("夹具摘要不可能是全 0，测试前提被破坏了")
	}

	mp := writeManifest(t, `[profile]
name = "wasm"

[[module]]
id      = "wasm-probe"
backend = "wasm"
path    = "`+filepath.ToSlash(fixture)+`"
digest  = "`+wrong+`"
`)
	if _, err := loadMods(mp, t.TempDir(), 0, nil); err == nil {
		t.Fatal("digest 不匹配必须让装配失败，不能装作没看见")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "digest") && !strings.Contains(msg, "摘要") {
			t.Fatalf("错误里要指出是摘要校验失败：%v", err)
		}
		if !strings.Contains(msg, "wasm-probe") {
			t.Fatalf("错误里要点名是哪个模块：%v", err)
		}
		if !strings.Contains(msg, real) {
			t.Fatalf("错误里要给出实际摘要，便于核对：%v", err)
		}
	}
	// 失败后不能留下"已登记"的假象。
	if got := loadedWasm(); len(got) != 0 {
		t.Fatalf("装配失败后不该留下登记的 wasm 模块：%v", got)
	}
}

// TestWasmMissingFileFails：文件不存在 → 装配失败，错误里带路径。
func TestWasmMissingFileFails(t *testing.T) {
	resetWasm(t)
	missing := filepath.Join(t.TempDir(), "no-such-module.wasm")

	mp := writeManifest(t, `[profile]
name = "wasm"

[[module]]
id      = "wasm-gone"
backend = "wasm"
path    = "`+filepath.ToSlash(missing)+`"
`)
	_, err := loadMods(mp, t.TempDir(), 0, nil)
	if err == nil {
		t.Fatal("路径不存在必须让装配失败")
	}
	if !strings.Contains(err.Error(), "no-such-module.wasm") {
		t.Fatalf("错误里要带路径：%v", err)
	}
	if !strings.Contains(err.Error(), "wasm-gone") {
		t.Fatalf("错误里要点名是哪个模块：%v", err)
	}
}

// TestWasmNonWasmFileFails：文件在，但不是 wasm → 报错要带 wasmhost 的原始原因（魔数）。
func TestWasmNonWasmFileFails(t *testing.T) {
	resetWasm(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake.wasm")
	if err := os.WriteFile(fake, []byte("这不是 wasm，只是一段文本"), 0o644); err != nil {
		t.Fatal(err)
	}

	mp := writeManifest(t, `[profile]
name = "wasm"

[[module]]
id      = "wasm-fake"
backend = "wasm"
path    = "`+filepath.ToSlash(fake)+`"
`)
	_, err := loadMods(mp, t.TempDir(), 0, nil)
	if err == nil {
		t.Fatal("非 wasm 文件必须让装配失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "0asm") {
		t.Fatalf("错误里要保留 wasmhost 的原始原因（魔数 \\0asm）：%v", err)
	}
	if !strings.Contains(msg, "fake.wasm") {
		t.Fatalf("错误里要带路径：%v", err)
	}
}

// TestWasmProvidesRejected：manifest 给 wasm 模块写 provides = ["compaction"] → 直接拒绝。
//
// 现在 wasm 只认 provides = ["tool"]（一个模块一个工具）。写别的东西不静默忽略，
// 是因为"写了 provides 却被丢掉"等于配置说谎：读 manifest 的人会以为这条已经生效。
func TestWasmProvidesRejected(t *testing.T) {
	resetWasm(t)
	fixture := fixturePath(t)

	mp := writeManifest(t, `[profile]
name = "wasm"

[[module]]
id       = "wasm-probe"
backend  = "wasm"
path     = "`+filepath.ToSlash(fixture)+`"
provides = ["compaction"]
`)
	_, err := loadMods(mp, t.TempDir(), 0, nil)
	if err == nil {
		t.Fatal(`wasm 只认 provides = ["tool"]，写 compaction 必须报错`)
	}
	if !strings.Contains(err.Error(), "provides") || !strings.Contains(err.Error(), "tool") {
		t.Fatalf(`错误里要写清只认 provides = ["tool"]：%v`, err)
	}
	if !strings.Contains(err.Error(), "compaction") {
		t.Fatalf("错误里要点名是哪个能力不认识：%v", err)
	}
}

// TestWasmToolProvidesOneTool 是这一层的落点：provides = ["tool"] 的 wasm 模块
// 真的往工具表里加了一个工具。
//
// 一条用例连着钉四件事，因为它们缺一条都不算"真的提供了一个工具"：
//   - 装配时 Register 真的跑过（工具表里查得到）；
//   - 名字/描述/schema 来自 manifest（不是从客人那里问的）；
//   - Exec 走 ABI 拿到结果（InvokeTool 真的调到了客人）；
//   - `modules graph` 的 wasm 小节把工具名打出来了（人能看见它提供了什么）。
func TestWasmToolProvidesOneTool(t *testing.T) {
	resetWasm(t)
	fixture := toolFixturePath(t)

	mp := writeManifest(t, `[profile]
name = "wasm-tool"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id       = "wasm-echo"
backend  = "wasm"
path     = "`+filepath.ToSlash(fixture)+`"
provides = ["tool"]
tool_name        = "echo"
tool_description = "把 text 原样回显（wasm 夹具工具）"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}'''
timeout_ms       = 2000
`)
	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("带工具声明的 wasm 模块必须能装配，实际：%v", err)
	}

	var got *kernel.Module
	for i := range mods {
		if mods[i].ID == "wasm-echo" {
			got = &mods[i]
		}
	}
	if got == nil {
		t.Fatal("装配结果里没有 wasm-echo")
	}
	if got.Register == nil {
		t.Fatal(`provides = ["tool"] 的模块必须有 Register，否则工具根本进不了表`)
	}
	// Provides 来自 manifest（Apply 会把它原样复制上来），所以装配结果里是 ["tool"]。
	// 这正是 `modules graph` 的 PROVIDES 列能看到它的原因：**能力来源只有一个**（manifest），
	// 装载层不能替它编一个。注意 "tool" 不是服务键：真正的注册靠 Requires "tools"
	// 注入工具表，两件事不许混。
	if !containsStr(got.Provides, "tool") {
		t.Fatalf(`装配结果里应当保留 manifest 写的 provides = ["tool"]，实际 %v`, got.Provides)
	}
	if !containsStr(got.Requires, "tools") {
		t.Fatalf("要注册进工具表就必须 Requires \"tools\"，实际 %v", got.Requires)
	}

	reg := assembleForTest(t, mods)
	tool, ok := reg.Get("echo")
	if !ok {
		var names []string
		for _, x := range reg.Tools() {
			names = append(names, x.Name())
		}
		t.Fatalf("工具表里没有 echo，实际有 %v", names)
	}
	if tool.Description() != "把 text 原样回显（wasm 夹具工具）" {
		t.Fatalf("Description 必须来自 manifest，实际 %q", tool.Description())
	}
	if !json.Valid(tool.Schema()) || !strings.Contains(string(tool.Schema()), `"text"`) {
		t.Fatalf("Schema 必须是 manifest 里那段 JSON，实际 %s", tool.Schema())
	}
	if tool.Caps().NeedsApproval() {
		t.Fatalf("没写 tool_caps 就该是全 false（沙箱强制它碰不到外面），实际 %+v", tool.Caps())
	}

	// Exec 真的走了一次 ABI。
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"text":"你好"}`))
	if err != nil {
		t.Fatalf("Exec 失败：%v", err)
	}
	if !strings.Contains(res.Text, "你好") {
		t.Fatalf("结果正文应当包含传进去的 text，实际 %q", res.Text)
	}

	// 人能看见它提供了什么：graph 的 wasm 小节要打出工具名与 requires。
	var graph bytes.Buffer
	if err := writeModules(&graph, "graph", mods); err != nil {
		t.Fatalf("modules graph 失败：%v", err)
	}
	g := graph.String()
	for _, want := range []string{"wasm-echo", "echo", "tools"} {
		if !strings.Contains(g, want) {
			t.Fatalf("modules graph 里应当能看到 %q：\n%s", want, g)
		}
	}
}

// TestWasmToolBusinessErrorGivesHint：客人回 isError:true → Exec 返回 error + Hint。
//
// 这是 R3 的落点：失败必须 `Result{Hint:…}, err`。
// 模型能看懂的关键在 Hint 与 err 文本里——只写 "tool failed" 等于没说。
func TestWasmToolBusinessErrorGivesHint(t *testing.T) {
	resetWasm(t)
	tool := loadWasmTool(t, "echo", "")

	res, err := tool.Exec(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("isError:true 必须让 Exec 返回 error（否则 v4 里 isError 会是 false）")
	}
	if res.Hint == "" {
		t.Fatal("R3：失败必须带 Hint，不能空 Result 丢提示")
	}
	if !strings.Contains(res.Hint, "text") {
		t.Fatalf("Hint 应当来自客人（怎么补参数），实际 %q", res.Hint)
	}
	if res.Text == "" {
		t.Fatal("工具失败也要把正文带出来，模型才知道发生了什么")
	}
	// err 的文本会被投影成 "Error: …" 给模型看，必须带客人说的内容。
	if !strings.Contains(err.Error(), "空输入") {
		t.Fatalf("err 里应当有客人的 summary（模型看到的 Error 行），实际 %v", err)
	}
}

// TestWasmToolTrapGivesHint：客人 trap → Exec 返回 error + 「别原样重试」的 Hint。
//
// 与上一条的区别就是本轮的核心区分：trap 不是业务失败，Hint 不能暗示"改参数再试"。
func TestWasmToolTrapGivesHint(t *testing.T) {
	resetWasm(t)
	tool := loadWasmTool(t, "echo", "")

	res, err := tool.Exec(context.Background(), json.RawMessage(`{"boom":true}`))
	if err == nil {
		t.Fatal("客人 trap 必须让 Exec 返回 error")
	}
	if res.Hint == "" {
		t.Fatal("trap 也必须给 Hint（R3）")
	}
	if !strings.Contains(res.Hint, "trap") {
		t.Fatalf("Hint 要说清是插件崩了，实际 %q", res.Hint)
	}
	if !strings.Contains(res.Hint, "不要原样重试") {
		t.Fatalf("trap 的 Hint 必须劝阻「原样重试」，实际 %q", res.Hint)
	}
}

// TestWasmWithoutProvidesKeepsToolTable：没写 provides = ["tool"] 的 wasm 模块，
// 工具表**逐字节不变**。
//
// 为什么用马上比对而不是"数量相等"：工具表是模型侧缓存前缀的一部分（R4），
// 多一个字、顺序变一下都算契约变化。用字节比对才不会漏掉"只多了个空格"。
func TestWasmWithoutProvidesKeepsToolTable(t *testing.T) {
	resetWasm(t)
	fixture := toolFixturePath(t)

	base := `[profile]
name = "wasm-noop"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id      = "tool-nowline"
backend = "static"
`
	withWasm := base + `
[[module]]
id      = "wasm-silent"
backend = "wasm"
path    = "` + filepath.ToSlash(fixture) + `"
`
	before := toolTableBytes(t, writeManifest(t, base))
	after := toolTableBytes(t, writeManifest(t, withWasm))

	if !bytes.Equal(before, after) {
		t.Fatalf("没写 provides 的 wasm 模块不该改变工具表：\n--- 之前 ---\n%s\n--- 之后 ---\n%s", before, after)
	}
	// 顺带把"它确实被装载了"钉住：不然上面那句可能只是因为两边都没装。
	if got := loadedWasm(); len(got) != 1 || got[0].id != "wasm-silent" {
		t.Fatalf("wasm-silent 应当被装载并登记，实际 %+v", got)
	}
	if got := loadedWasm(); len(got[0].tools) != 0 {
		t.Fatalf("没写 provides 的模块不该有工具名，实际 %v", got[0].tools)
	}
}

// TestWasmDisabledSkipped：enabled = false 的 wasm 模块连文件都不读——
// 一个关掉的模块不该因为路径坏了把启动拖下水。
func TestWasmDisabledSkipped(t *testing.T) {
	resetWasm(t)
	missing := filepath.Join(t.TempDir(), "never-touched.wasm")

	mp := writeManifest(t, `[profile]
name = "wasm"

[[module]]
id      = "wasm-off"
backend = "wasm"
path    = "`+filepath.ToSlash(missing)+`"
enabled = false
`)
	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("禁用的 wasm 模块不该被装载：%v", err)
	}
	for _, m := range mods {
		if m.ID == "wasm-off" {
			t.Fatal("禁用的模块不该出现在装配结果里")
		}
	}
	if got := loadedWasm(); len(got) != 0 {
		t.Fatalf("禁用的模块不该被登记：%v", got)
	}
}

// TestWasmToolManifestValidation：工具声明写错 → 装配时炸，并且说清缺什么。
//
// 为什么全都必填、且必须在装配期炸：这些错误在运行时的表现是
// "模型不会用这个工具"或"审批判错档"，从现象根本归因不到 manifest 少了一行。
func TestWasmToolManifestValidation(t *testing.T) {
	resetWasm(t)
	fixture := filepath.ToSlash(toolFixturePath(t))

	head := `[profile]
name = "wasm-tool"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id       = "wasm-echo"
backend  = "wasm"
path     = "` + fixture + `"
provides = ["tool"]
`
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "缺 tool_name",
			body: "tool_description = \"x\"\ntool_schema = '''{\"type\":\"object\"}'''\n",
			want: "tool_name",
		},
		{
			name: "缺 tool_description",
			body: "tool_name = \"echo\"\ntool_schema = '''{\"type\":\"object\"}'''\n",
			want: "tool_description",
		},
		{
			name: "缺 tool_schema",
			body: "tool_name = \"echo\"\ntool_description = \"x\"\n",
			want: "tool_schema",
		},
		{
			name: "schema 不是对象",
			body: "tool_name = \"echo\"\ntool_description = \"x\"\ntool_schema = '''[\"a\"]'''\n",
			want: "JSON 对象",
		},
		{
			name: "schema 不是 JSON",
			body: "tool_name = \"echo\"\ntool_description = \"x\"\ntool_schema = '''{nope}'''\n",
			want: "JSON 对象",
		},
		{
			name: "tool_caps 拼错",
			body: "tool_name = \"echo\"\ntool_description = \"x\"\n" +
				"tool_schema = '''{\"type\":\"object\"}'''\ntool_caps = [\"write\"]\n",
			want: "tool_caps",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetWasm(t)
			mp := writeManifest(t, head+c.body)
			_, err := loadMods(mp, t.TempDir(), 0, nil)
			if err == nil {
				t.Fatalf("%s 必须让装配失败", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里要点出 %q：%v", c.want, err)
			}
			if !strings.Contains(err.Error(), "wasm-echo") {
				t.Fatalf("错误里要点名模块：%v", err)
			}
			// 校验失败不能留下"已装载"的句柄。
			if got := loadedWasm(); len(got) != 0 {
				t.Fatalf("装配失败后不该留下已装载的 wasm 模块：%v", got)
			}
		})
	}
}

// --- 缺口 2：多个 wasm 工具模块同时存在 ---

// twoToolManifest 造一个"两个 wasm 工具模块"的 manifest。
//
// 两个模块指向**同一个**夹具文件：这一层要证明的是"一个 manifest 能挂多个
// wasm 工具模块"，不是"能装多个不同文件"——用同一个文件反而排除掉
// "文件不同所以没撞上"这种解释。
//
// 能力键写 "tool:<tool_name>"（不是两个都写 "tool"）：kernel 按字符串判重
// （internal/kernel/assemble.go:64），而 Apply 会把 manifest 的 provides 原样
// 复制到每个模块上（internal/kernel/module.go:167），装载层没有钩子能改写它。
// 所以"两个模块同时存在"的唯一诚实做法，就是它们的能力键本来就是两个字符串。
func twoToolManifest(t *testing.T, capA, capB, nameA, nameB string) string {
	t.Helper()
	fixture := filepath.ToSlash(toolFixturePath(t))
	return `[profile]
name = "wasm-two-tools"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id       = "wasm-` + nameA + `"
backend  = "wasm"
path     = "` + fixture + `"
provides = ["` + capA + `"]
tool_name        = "` + nameA + `"
tool_description = "第一个 wasm 夹具工具（` + nameA + `）"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}'''

[[module]]
id       = "wasm-` + nameB + `"
backend  = "wasm"
path     = "` + fixture + `"
provides = ["` + capB + `"]
tool_name        = "` + nameB + `"
tool_description = "第二个 wasm 夹具工具（` + nameB + `）"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}'''
`
}

// TestWasmTwoToolModulesCoexist 是缺口 2 的落点：一个 manifest 里两个 wasm 工具模块，
// **两个工具都进工具表、都调得通**。
//
// 这一条同时钉三件事，缺任何一条都不算"多个 wasm 工具能用"：
//   - 装配不报"能力被两个模块同时提供"（这正是改造前必然发生的失败）；
//   - 工具表里两个名字都在，且各自的身份（描述）没有被串台；
//   - 两次 Exec 都真的跨到客人那边（同一个 .wasm 文件、两个不同实例）。
func TestWasmTwoToolModulesCoexist(t *testing.T) {
	resetWasm(t)
	mp := writeManifest(t, twoToolManifest(t, "tool:echo", "tool:hello", "echo", "hello"))

	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("两个 wasm 工具模块必须能同时装配，实际报错：%v", err)
	}

	// 装配结果里两个模块的能力键必须不同——相同时 kernel 的判重必然炸。
	caps := map[string]string{}
	for _, m := range mods {
		if m.Backend != kernel.BackendWasm {
			continue
		}
		if len(m.Provides) != 1 {
			t.Fatalf("wasm 模块 %q 应当正好提供一个能力键，实际 %v", m.ID, m.Provides)
		}
		if m.Register == nil {
			t.Fatalf("wasm 模块 %q 有工具声明就必须有 Register", m.ID)
		}
		caps[m.ID] = m.Provides[0]
	}
	if caps["wasm-echo"] == caps["wasm-hello"] || caps["wasm-echo"] == "" || caps["wasm-hello"] == "" {
		t.Fatalf("两个 wasm 模块的能力键必须各不相同，实际 %v", caps)
	}

	// graph 要能一眼看出谁提供了哪个工具（能力键里带工具名就是为了这个）。
	var graph bytes.Buffer
	if err := writeModules(&graph, "graph", mods); err != nil {
		t.Fatalf("modules graph 失败：%v", err)
	}
	for _, want := range []string{"wasm-echo", "wasm-hello", "tool:echo", "tool:hello", "tools"} {
		if !strings.Contains(graph.String(), want) {
			t.Fatalf("modules graph 里应当能看到 %q：\n%s", want, graph.String())
		}
	}

	reg := assembleForTest(t, mods)
	for _, name := range []string{"echo", "hello"} {
		if _, ok := reg.Get(name); !ok {
			var names []string
			for _, x := range reg.Tools() {
				names = append(names, x.Name())
			}
			t.Fatalf("工具表里没有 %q，实际有 %v", name, names)
		}
	}
	if got, _ := reg.Get("echo"); got.Description() != "第一个 wasm 夹具工具（echo）" {
		t.Fatalf("两个工具的 manifest 声明串台了：echo 的描述是 %q", got.Description())
	}
	if got, _ := reg.Get("hello"); got.Description() != "第二个 wasm 夹具工具（hello）" {
		t.Fatalf("两个工具的 manifest 声明串台了：hello 的描述是 %q", got.Description())
	}

	// 两个工具都要真的调得通：装配成功只证明注册了，不证明能用。
	cases := []struct{ name, text string }{
		{"echo", "第一个"},
		{"hello", "第二个"},
	}
	for _, c := range cases {
		tool, _ := reg.Get(c.name)
		res, err := tool.Exec(context.Background(), json.RawMessage(`{"text":"`+c.text+`"}`))
		if err != nil {
			t.Fatalf("工具 %q 执行失败：%v", c.name, err)
		}
		if !strings.Contains(res.Text, c.text) {
			t.Fatalf("工具 %q 的结果里应当有 %q，实际 %q", c.name, c.text, res.Text)
		}
	}

	// 两个模块都登记在 wasm 小节里，且各自记着自己的工具名。
	list := loadedWasm()
	if len(list) != 2 {
		t.Fatalf("应当登记 2 个 wasm 模块，实际 %d：%+v", len(list), list)
	}
	tools := map[string]string{}
	for _, x := range list {
		tools[x.id] = strings.Join(x.tools, ",")
	}
	if tools["wasm-echo"] != "echo" || tools["wasm-hello"] != "hello" {
		t.Fatalf("wasm 小节里的工具名对不上：%v", tools)
	}
}

// TestWasmTwoToolsWithPlainToolKeyFails：两个模块都写 provides = ["tool"] 时，
// **装载层**必须给出能照着改的错误，而不是 kernel 那句"能力被两个模块同时提供"。
//
// 这条是缺口 2 的代价，必须钉住：用户不会去读 kernel 源码，
// 他只知道自己在 manifest 里写了两行一模一样的 provides。
func TestWasmTwoToolsWithPlainToolKeyFails(t *testing.T) {
	resetWasm(t)
	mp := writeManifest(t, twoToolManifest(t, "tool", "tool", "echo", "hello"))

	_, err := loadMods(mp, t.TempDir(), 0, nil)
	if err == nil {
		t.Fatal(`两个 wasm 工具模块都写 provides = ["tool"] 必须报错（kernel 只允许一个提供者）`)
	}
	msg := err.Error()
	for _, want := range []string{"tool:echo", "tool:<tool_name>", "wasm-echo", "wasm-hello"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误里要有 %q（要能照着改 manifest）：\n%s", want, msg)
		}
	}
	// 校验就该在装载前失败：不留半装好的句柄。
	if got := loadedWasm(); len(got) != 0 {
		t.Fatalf("失败后不该留下已装载的模块：%+v", got)
	}
}

// TestWasmToolCapKeyMustMatchToolName：能力键冒号后必须与 tool_name 一致。
//
// 不校验的话，`modules graph`（能力键）与模型看到的工具表（工具名）会各说各话，
// 而这种错在 manifest 里只是几个字符。
func TestWasmToolCapKeyMustMatchToolName(t *testing.T) {
	resetWasm(t)
	mp := writeManifest(t, twoToolManifest(t, "tool:eho", "tool:hello", "echo", "hello"))

	_, err := loadMods(mp, t.TempDir(), 0, nil)
	if err == nil {
		t.Fatal("能力键与 tool_name 不一致必须报错")
	}
	msg := err.Error()
	for _, want := range []string{"tool:eho", `"echo"`, "wasm-echo"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误里要有 %q：\n%s", want, msg)
		}
	}
}

// TestWasmToolRebuildsAfterTrapThroughExec 把缺口 1 拉到工具层再验一遍：
// **同一个 spi.Tool 句柄**先炸一次、再成功一次。
//
// 为什么在 cmd/dshd 也测：wasmhost 的用例证明的是"Module 会重建"，
// 这里证明的是"Register 闭包里握着的那个 Module 就是被重建的那个"——
// 如果 wasmTool 持有的是别的副本（比如按值拷贝），工具层就永远调不动了。
func TestWasmToolRebuildsAfterTrapThroughExec(t *testing.T) {
	resetWasm(t)
	tool := loadWasmTool(t, "echo", "")

	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"boom":true}`)); err == nil {
		t.Fatal("客人 trap 必须让 Exec 返回 error")
	}

	res, err := tool.Exec(context.Background(), json.RawMessage(`{"text":"自愈之后"}`))
	if err != nil {
		t.Fatalf("trap 之后同一个工具必须还能调（实例应当自动重建），实际：%v", err)
	}
	if !strings.Contains(res.Text, "自愈之后") {
		t.Fatalf("重建之后应当真的调到客人，实际 %q", res.Text)
	}
}

// --- 一个模块多个工具（`[[module.tool]]`） ---

// multiToolHead 造"一个 wasm 模块（可能）挂多个工具"的 manifest 前半段。
//
// 与上一轮 §9.2 的"两个模块共用一个文件"不是一回事：那里是两个 [[module]] 行
// （两份编译产物、两个实例、两个能力键），这里只有**一个** [[module]] 行：
// 一个实例、多个 spi.Tool，靠请求信封的 tool 字段分流。
// 用同一个 fixture 文件，也就排除了"文件不同所以没混到一起"这种解释。
//
// provides 传空串 = 故意不写 provides 那一行（验"声明了工具却没写能力键"）。
func multiToolHead(t *testing.T, provides string) string {
	t.Helper()
	return multiToolHeadWith(t, provides, "")
}

// multiToolHeadWith 允许在**模块级**多插几行（比如旧的单工具字段，用来验混写）。
//
// 注意 TOML 的位置语义：模块级的键必须写在第一节 `[[module.tool]]` **之前**，
// 写在后面就属于那个子表了。这既是写法约束，也是混写最容易出现的位置。
func multiToolHeadWith(t *testing.T, provides, extra string) string {
	t.Helper()
	prov := ""
	if strings.TrimSpace(provides) != "" {
		prov = "provides = [" + provides + "]\n"
	}
	return `[profile]
name = "wasm-multi-tool"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id      = "wasm-text"
backend = "wasm"
path    = "` + filepath.ToSlash(toolFixturePath(t)) + `"
` + prov + extra
}

// toolItem 是 `[[module.tool]]` 的一节。extra 用来插 timeout_ms / caps 这类可选项。
func toolItem(name, desc, extra string) string {
	return `
[[module.tool]]
name        = "` + name + `"
description = "` + desc + `"
schema      = '''{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}'''
` + extra
}

// TestWasmModuleServesTwoTools 是这一轮的落点之一：**一个** wasm 模块声明**两个**
// 工具，工具表里就该出现两个名字。
//
// 一条用例钉五件事，缺一条都不算"一个模块真能提供多个工具"：
//   - 装配结果里只有一个 wasm 模块（不是把文件挂了两遍）；
//   - 它有两个能力键（kernel 按字符串判重，两个工具 = 两个键）；
//   - 一个 Register 把两个工具都注册进了表；
//   - 两个工具的身份（描述/能力声明）没有串台；
//   - `modules graph` 的 TOOL 列把两个名字都打出来了（人能看见它提供了什么）。
func TestWasmModuleServesTwoTools(t *testing.T) {
	resetWasm(t)
	mp := writeManifest(t, multiToolHead(t, `"tool:echo", "tool:upper"`)+
		toolItem("echo", "把 text 原样回显（wasm 夹具 · 一号）", "timeout_ms = 2000\n")+
		toolItem("upper", "把 text 里的字母转成大写（wasm 夹具 · 二号）", "timeout_ms = 5000\n"))

	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("一个模块挂两个工具必须能装配，实际：%v", err)
	}

	wasmMods := 0
	var text *kernel.Module
	for i := range mods {
		if mods[i].Backend != kernel.BackendWasm {
			continue
		}
		wasmMods++
		text = &mods[i]
	}
	if wasmMods != 1 {
		t.Fatalf("应当是 **一个** wasm 模块挂两个工具，实际装配出 %d 个", wasmMods)
	}
	if len(text.Provides) != 2 || !containsStr(text.Provides, "tool:echo") || !containsStr(text.Provides, "tool:upper") {
		t.Fatalf(`两个工具就该有两个能力键（tool:echo / tool:upper），实际 %v`, text.Provides)
	}
	if text.Register == nil {
		t.Fatal("有工具声明的模块必须有 Register")
	}

	reg := assembleForTest(t, mods)
	for _, name := range []string{"echo", "upper"} {
		if _, ok := reg.Get(name); !ok {
			var names []string
			for _, x := range reg.Tools() {
				names = append(names, x.Name())
			}
			t.Fatalf("工具表里没有 %q，实际有 %v", name, names)
		}
	}
	if got, _ := reg.Get("echo"); got.Description() != "把 text 原样回显（wasm 夹具 · 一号）" {
		t.Fatalf("两个工具的声明串台了：echo 的描述是 %q", got.Description())
	}
	if got, _ := reg.Get("upper"); got.Description() != "把 text 里的字母转成大写（wasm 夹具 · 二号）" {
		t.Fatalf("两个工具的声明串台了：upper 的描述是 %q", got.Description())
	}

	// 两个工具都真的调得通（不同 timeout_ms 也要能共存）。
	for _, c := range []struct{ name, text string }{
		{"echo", "一号"},
		{"upper", "二号"},
	} {
		tool, _ := reg.Get(c.name)
		res, err := tool.Exec(context.Background(), json.RawMessage(`{"text":"`+c.text+`"}`))
		if err != nil {
			t.Fatalf("工具 %q 执行失败：%v", c.name, err)
		}
		if res.Text == "" {
			t.Fatalf("工具 %q 没有结果正文", c.name)
		}
	}

	// 登记的模块仍然是一个，但它记着两个工具名。
	list := loadedWasm()
	if len(list) != 1 {
		t.Fatalf("登记的 wasm 模块应当只有 1 个，实际 %d：%+v", len(list), list)
	}
	if strings.Join(list[0].tools, ",") != "echo,upper" {
		t.Fatalf("登记的工具名应当是 echo,upper（按 manifest 顺序），实际 %v", list[0].tools)
	}

	// 人能看见它提供了什么：graph 的 TOOL 列必须两个都打。
	var graph bytes.Buffer
	if err := writeModules(&graph, "graph", mods); err != nil {
		t.Fatalf("modules graph 失败：%v", err)
	}
	g := graph.String()
	for _, want := range []string{"wasm-text", "tool:echo", "tool:upper", "echo,upper"} {
		if !strings.Contains(g, want) {
			t.Fatalf("modules graph 里应当能看到 %q：\n%s", want, g)
		}
	}
}

// TestWasmTwoToolsRouteByToolField 是这一轮的核心：两个工具走**同一个模块**，
// 但逻辑各自不同——证明宿主真的把工具名放进了请求信封的 tool 字段，
// 而不是"注册了两个名字、其实都跑同一段逻辑"。
//
// 为什么判据是输出而不是"调用没报错"：夹具里 upper 只比 echo 多一个
// `req.Tool == "upper"` 的条件，所以"没路由"的表现恰恰是**成功**返回
// echo 的结果。只断言 err == nil 的测试在这里会全绿，等于什么也没证明。
//
// 三次调用按 echo → upper → echo 交叉，是因为工具名在 ABI 层是**模块级状态**
// （SetToolName）：如果宿主只在装载时设一次、或每次设了却没和调用串起来，
// 第三次就会拿到上一次的名字——那正是最难看的一种 bug（结果对、身份错）。
func TestWasmTwoToolsRouteByToolField(t *testing.T) {
	resetWasm(t)
	mp := writeManifest(t, multiToolHead(t, `"tool:echo", "tool:upper"`)+
		toolItem("echo", "原样回显", "")+
		toolItem("upper", "转大写", ""))

	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	reg := assembleForTest(t, mods)
	echoTool, ok := reg.Get("echo")
	if !ok {
		t.Fatal("工具表里没有 echo")
	}
	upTool, ok := reg.Get("upper")
	if !ok {
		t.Fatal("工具表里没有 upper")
	}

	args := json.RawMessage(`{"text":"Hi 你好"}`)
	cases := []struct {
		tool spi.Tool
		want string
	}{
		{echoTool, "echo:Hi 你好"},
		{upTool, "upper:HI 你好"}, // 路由没生效的话这里会是 echo:Hi 你好
		{echoTool, "echo:Hi 你好"},
		{upTool, "upper:HI 你好"},
	}
	for i, c := range cases {
		res, err := c.tool.Exec(context.Background(), args)
		if err != nil {
			t.Fatalf("第 %d 次调用（%s）失败：%v", i+1, c.tool.Name(), err)
		}
		if res.Text != c.want {
			t.Fatalf("第 %d 次调用 %q 的正文应当是 %q，实际 %q。"+
				"这通常意味着宿主没把工具名写进请求信封的 tool 字段（两个工具跑到同一个分支上了）",
				i+1, c.tool.Name(), c.want, res.Text)
		}
	}
}

// TestWasmLegacySingleToolFormStillWorks 钉住"旧写法一个字都不用改"。
//
// 旧写法（tool_name / tool_description / tool_schema / tool_caps）与新的
// `[[module.tool]]` 是两条入口，共享同一份校验与同一份运行时；
// 这一条把旧入口从 manifest 一路走到 Exec，证明它不是"还能解析"而是"还能用"。
func TestWasmLegacySingleToolFormStillWorks(t *testing.T) {
	resetWasm(t)
	fixture := toolFixturePath(t)

	mp := writeManifest(t, `[profile]
name = "wasm-legacy"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id       = "wasm-echo"
backend  = "wasm"
path     = "`+filepath.ToSlash(fixture)+`"
provides = ["tool"]
tool_name        = "echo"
tool_description = "把 text 原样回显（旧写法）"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}'''
tool_caps        = ["writes"]
timeout_ms       = 2000
`)
	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("旧的单工具写法必须继续有效，实际报错：%v", err)
	}

	var got *kernel.Module
	for i := range mods {
		if mods[i].ID == "wasm-echo" {
			got = &mods[i]
		}
	}
	if got == nil || got.Register == nil {
		t.Fatal("旧写法的模块必须照旧注册工具")
	}
	if len(got.Provides) != 1 || got.Provides[0] != "tool" {
		t.Fatalf(`旧写法的能力键仍然是 "tool"（不是 tool:echo），实际 %v`, got.Provides)
	}

	reg := assembleForTest(t, mods)
	tool, ok := reg.Get("echo")
	if !ok {
		t.Fatal("工具表里没有 echo")
	}
	if tool.Description() != "把 text 原样回显（旧写法）" {
		t.Fatalf("描述必须来自 manifest，实际 %q", tool.Description())
	}
	// tool_caps 也照旧生效：审批靠它判档。
	if !tool.Caps().Writes || !tool.Caps().NeedsApproval() {
		t.Fatalf("旧写法的 tool_caps = [\"writes\"] 必须照旧生效，实际 %+v", tool.Caps())
	}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"text":"旧写法"}`))
	if err != nil {
		t.Fatalf("旧写法的工具必须调得动：%v", err)
	}
	if res.Text != "echo:旧写法" {
		t.Fatalf("结果不对：%q", res.Text)
	}

	// 单工具模块仍然把模块级工具名设成它：`dshd wasm call` 这类不经过
	// spi.Tool 的入口要用这个默认值。
	list := loadedWasm()
	if len(list) != 1 || strings.Join(list[0].tools, ",") != "echo" {
		t.Fatalf("登记的工具名应当是 echo，实际 %+v", list)
	}
	if name := list[0].module.ToolName(); name != "echo" {
		t.Fatalf("单工具模块应当把模块级工具名设成 echo（wasm call 用），实际 %q", name)
	}
}

// TestWasmMixedToolFormsRejected：旧的单工具字段与 `[[module.tool]]` 混写 →
// **启动即炸**，而且不许猜优先级。
//
// 为什么必须炸：两种写法都在回答"这个模块有哪些工具"。谁赢都是猜，
// 猜错的表现是"模型少了一个工具"或"某个工具带着别人的 description 出现在
// 模型面前"——运行期完全看不出来，只能靠人回头读 manifest。
func TestWasmMixedToolFormsRejected(t *testing.T) {
	resetWasm(t)
	// 混写的现实形态：本来是一个单工具模块，想再加一个工具，于是往模块级
	// 补了 tool_name，同时又写了 [[module.tool]]。
	mp := writeManifest(t, multiToolHeadWith(t, `"tool:echo", "tool:upper"`,
		`tool_name        = "upper"
tool_description = "也写在模块上（这就是混写）"
tool_schema      = '''{"type":"object"}'''
`)+toolItem("echo", "原样回显", "")+toolItem("upper", "转大写", ""))

	_, err := loadMods(mp, t.TempDir(), 0, nil)
	if err == nil {
		t.Fatal("单工具字段与 [[module.tool]] 混写必须报错，不许猜优先级")
	}
	msg := err.Error()
	for _, want := range []string{"wasm-text", "tool_name", "[[module.tool]]", "不能混用"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误里要有 %q（要能照着改 manifest）：\n%s", want, msg)
		}
	}
	if got := loadedWasm(); len(got) != 0 {
		t.Fatalf("校验失败不能留下已装载的句柄：%+v", got)
	}
}

// TestWasmToolCapKeysMustMatchDeclaredTools：能力键与声明的工具必须**一一对上**。
//
// 能力键会进 `modules graph`，工具名会进模型看到的工具表；两处不一致时，
// 图和表各说各话，而这种错在 manifest 里只是几个字符。
// 所以：少一个键、多一个键、多工具还写单数缩写 "tool"、缩写与具名混写，
// 全部在启动时炸，并给出能照抄的一行 provides。
func TestWasmToolCapKeysMustMatchDeclaredTools(t *testing.T) {
	items := toolItem("echo", "原样回显", "") + toolItem("upper", "转大写", "")
	cases := []struct {
		name     string
		provides string
		want     []string
	}{
		{
			name:     "少一个键",
			provides: `"tool:echo"`,
			want:     []string{"wasm-text", "tool:upper", "没有对应的能力键"},
		},
		{
			name:     "多一个键",
			provides: `"tool:echo", "tool:upper", "tool:ghost"`,
			want:     []string{"wasm-text", "tool:ghost", "没有对应的工具声明"},
		},
		{
			name:     "两个工具却写单数缩写 tool",
			provides: `"tool"`,
			want:     []string{"wasm-text", "tool", "tool:echo", "tool:upper"},
		},
		{
			name:     "缩写与具名混写",
			provides: `"tool", "tool:echo"`,
			want:     []string{"wasm-text", "缩写"},
		},
		{
			name:     "声明了工具却没写 provides",
			provides: "",
			want:     []string{"wasm-text", "provides", "tool:echo"},
		},
		{
			name:     "同一个键写两次",
			provides: `"tool:echo", "tool:echo", "tool:upper"`,
			want:     []string{"wasm-text", "tool:echo", "两次"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetWasm(t)
			mp := writeManifest(t, multiToolHead(t, c.provides)+items)
			_, err := loadMods(mp, t.TempDir(), 0, nil)
			if err == nil {
				t.Fatalf("%s 必须让装配失败", c.name)
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误里要有 %q：\n%s", want, err)
				}
			}
			if got := loadedWasm(); len(got) != 0 {
				t.Fatalf("校验失败不能留下已装载的句柄：%+v", got)
			}
		})
	}
}

// TestWasmMultiToolDeclValidation：`[[module.tool]]` 每一项自己的必填项
// 也要在装配期炸，并且点名是第几项。
//
// 与旧写法共用同一份校验（newWasmToolSpec），但"第几项"必须说出来：
// 一个模块里写着五节 [[module.tool]] 时，"某个工具没有 description"
// 这句话等于没说。
func TestWasmMultiToolDeclValidation(t *testing.T) {
	cases := []struct {
		name  string
		items string
		want  []string
	}{
		{
			name:  "第二项没有 name",
			items: toolItem("echo", "原样回显", "") + "\n[[module.tool]]\ndescription = \"x\"\nschema = '''{\"type\":\"object\"}'''\n",
			want:  []string{"wasm-text", "第 2 项", "name"},
		},
		{
			name:  "第二项没有 description",
			items: toolItem("echo", "原样回显", "") + "\n[[module.tool]]\nname = \"upper\"\nschema = '''{\"type\":\"object\"}'''\n",
			want:  []string{"wasm-text", "upper", "description"},
		},
		{
			name:  "schema 不是对象",
			items: toolItem("echo", "原样回显", "") + "\n[[module.tool]]\nname = \"upper\"\ndescription = \"x\"\nschema = '''[\"a\"]'''\n",
			want:  []string{"wasm-text", "upper", "JSON 对象"},
		},
		{
			name:  "caps 拼错",
			items: toolItem("echo", "原样回显", "") + "\n[[module.tool]]\nname = \"upper\"\ndescription = \"x\"\nschema = '''{\"type\":\"object\"}'''\ncaps = [\"write\"]\n",
			want:  []string{"wasm-text", "upper", "caps"},
		},
		{
			name:  "同一个模块里工具重名",
			items: toolItem("echo", "一号", "") + toolItem("echo", "二号", ""),
			want:  []string{"wasm-text", "echo", "唯一"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetWasm(t)
			mp := writeManifest(t, multiToolHead(t, `"tool:echo"`)+c.items)
			_, err := loadMods(mp, t.TempDir(), 0, nil)
			if err == nil {
				t.Fatalf("%s 必须让装配失败", c.name)
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误里要有 %q：\n%s", want, err)
				}
			}
			if got := loadedWasm(); len(got) != 0 {
				t.Fatalf("校验失败不能留下已装载的句柄：%+v", got)
			}
		})
	}
}

// TestWasmSingleItemArrayWithBareToolKey：`[[module.tool]]` 只写一项时，
// `provides = ["tool"]`（单数缩写）仍然成立——数组写法与缩写不是互斥的。
//
// 为什么值得单独钉：文档里说"只挂一个工具时两种能力键都允许"，而缩写 `"tool"`
// 的语义是"这个模块只有一个工具"。这条把"数组 + 缩写"这个组合也拉到启动路径上验一遍，
// 免得读的人以为数组写法必须写 `tool:<名字>`。
func TestWasmSingleItemArrayWithBareToolKey(t *testing.T) {
	resetWasm(t)
	mp := writeManifest(t, multiToolHead(t, `"tool"`)+toolItem("echo", "数组写法但只有一个工具", ""))

	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf(`[[module.tool]] 只有一项时 provides = ["tool"] 应当成立，实际报错：%v`, err)
	}
	if len(loadedWasm()) != 1 || strings.Join(loadedWasm()[0].tools, ",") != "echo" {
		t.Fatalf("应当登记一个模块、工具名 echo，实际 %+v", loadedWasm())
	}
	reg := assembleForTest(t, mods)
	if _, ok := reg.Get("echo"); !ok {
		t.Fatal("工具表里没有 echo")
	}
}

// --- 工具名在装配范围内必须唯一（跨模块判重，§10.8 第 1 条） ---

// dupNameSentinel 是判重用例里故意写的**不存在**的 .wasm 文件名。
//
// 它的用处只有一个：证明这条校验发生在**读 wasm 文件之前**。如果先碰磁盘，
// 报错就会变成"读不到这个文件"——那既不是根因（名字撞了），也修不掉重名。
const dupNameSentinel = "never-read-by-dup-check.wasm"

// TestWasmDuplicateToolNameAcrossModulesFails：两个**不同模块**声明同一个工具名，
// 必须由装载期的 manifest 校验报出来，而不是等工具表 Register 时才失败。
//
// 改前这条错来自 spi.ToolsKey 的 Register（"wasm 工具 X 注册失败"）：它不说哪两个
// 模块撞了，也不说改哪一行——而会改这个的只有读 manifest 的人。所以判据按三条钉：
//   - 哪两处撞了：两个模块 id + 工具名；
//   - 改哪个文件/哪一行：manifest 路径 + 第几个 [[module]] + 那一行的写法；
//   - 在读 wasm 之前：两条 path 都不存在，错误里不该出现它们。
func TestWasmDuplicateToolNameAcrossModulesFails(t *testing.T) {
	resetWasm(t)
	mp := writeManifest(t, `[profile]
name = "wasm-dup-name"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id       = "wasm-a"
backend  = "wasm"
path     = "`+dupNameSentinel+`"
provides = ["tool:echo"]
tool_name        = "echo"
tool_description = "第一个模块的 echo"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}}}'''

[[module]]
id       = "wasm-b"
backend  = "wasm"
path     = "`+dupNameSentinel+`"
provides = ["tool"]
tool_name        = "echo"
tool_description = "第二个模块也想要 echo"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}}}'''
`)
	_, err := loadMods(mp, t.TempDir(), 0, nil)
	if err == nil {
		t.Fatal("两个模块声明同一个工具名必须让装配失败：工具表里只容得下一个 echo")
	}
	msg := err.Error()
	// 1) 谁跟谁撞了。
	for _, want := range []string{"wasm-a", "wasm-b", `"echo"`, "唯一"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误里要有 %q（要能看出是谁跟谁撞了）：\n%s", want, msg)
		}
	}
	// 2) 改哪个文件/哪一行（两条 path 都不同，所以这里只认 manifest 里的位置）。
	for _, want := range []string{mp, "第 2 个 [[module]]", "第 3 个 [[module]]", `tool_name = "echo"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误里要有 %q（要能照着找到那一行）：\n%s", want, msg)
		}
	}
	// 3) 必须停在 manifest 上：那两个 .wasm 根本不存在，先读文件就会报成读不到。
	if strings.Contains(msg, dupNameSentinel) {
		t.Fatalf("判重必须发生在读 .wasm 之前（否则根因会被\"读不到文件\"盖掉）：\n%s", msg)
	}
	if got := loadedWasm(); len(got) != 0 {
		t.Fatalf("校验失败不能留下已装载的句柄：%+v", got)
	}
}

// TestWasmDuplicateToolNameOtherFormsFails：新写法（[[module.tool]]）与新旧混用
// 撞名，也要走**同一条**判重——判的是名字，不是写法。
//
// 为什么要专门测新旧混用：那是真实的迁移形态（把老模块改成数组写法时，
// name 常常是照着 tool_name 抄的），而这恰恰是最容易被"两种写法各查各的"漏掉的一种。
func TestWasmDuplicateToolNameOtherFormsFails(t *testing.T) {
	head := `[profile]
name = "wasm-dup-name"

[[module]]
id      = "tools"
backend = "static"
`
	cases := []struct {
		name string
		body string
		// want 是"这条错的文案必须说清的事"：两处位置 + 工具名。
		want []string
	}{
		{
			name: "两处都写 [[module.tool]]",
			body: `
[[module]]
id       = "wasm-a"
backend  = "wasm"
path     = "` + dupNameSentinel + `"
provides = ["tool:echo"]
` + toolItem("echo", "模块 a 的 echo", "") + `
[[module]]
id       = "wasm-b"
backend  = "wasm"
path     = "` + dupNameSentinel + `"
provides = ["tool:echo"]
` + toolItem("echo", "模块 b 的 echo", ""),
			want: []string{"wasm-a", "wasm-b", `"echo"`, "第 2 个 [[module]]", "第 3 个 [[module]]",
				"[[module.tool]] 第 1 项"},
		},
		{
			name: "一处 tool_name、一处 [[module.tool]]",
			body: `
[[module]]
id       = "wasm-a"
backend  = "wasm"
path     = "` + dupNameSentinel + `"
provides = ["tool:echo"]
tool_name        = "echo"
tool_description = "旧写法的 echo"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}}}'''

[[module]]
id       = "wasm-b"
backend  = "wasm"
path     = "` + dupNameSentinel + `"
provides = ["tool"]
` + toolItem("echo", "新写法的 echo", ""),
			want: []string{"wasm-a", "wasm-b", `"echo"`, `tool_name = "echo"`,
				"[[module.tool]] 第 1 项"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetWasm(t)
			mp := writeManifest(t, head+c.body)
			_, err := loadMods(mp, t.TempDir(), 0, nil)
			if err == nil {
				t.Fatalf("%s：同名工具必须让装配失败", c.name)
			}
			msg := err.Error()
			for _, want := range append(c.want, mp) {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误里要有 %q：\n%s", want, msg)
				}
			}
			if strings.Contains(msg, dupNameSentinel) {
				t.Fatalf("判重必须发生在读 .wasm 之前：\n%s", msg)
			}
			if got := loadedWasm(); len(got) != 0 {
				t.Fatalf("校验失败不能留下已装载的句柄：%+v", got)
			}
		})
	}
}

// TestWasmDuplicateToolNameInsideModuleFails：**同一个模块**里 [[module.tool]]
// 写了两遍同名 → 同一条判重（不是另一套文案）。
//
// 为什么这条要单独立：它是唯一一种"模块 id 相同"的撞名，报错若照抄跨模块的句式，
// 会写出"模块 wasm-text 与 wasm-text 撞了"这种废话。位置信息必须落到"第几项"。
func TestWasmDuplicateToolNameInsideModuleFails(t *testing.T) {
	resetWasm(t)
	mp := writeManifest(t, multiToolHead(t, `"tool:echo"`)+
		toolItem("echo", "一号", "")+toolItem("echo", "二号", ""))

	_, err := loadMods(mp, t.TempDir(), 0, nil)
	if err == nil {
		t.Fatal("同一个模块里两个工具同名必须让装配失败（工具表按名字存，第二行存不进去）")
	}
	msg := err.Error()
	for _, want := range []string{"wasm-text", `"echo"`, "[[module.tool]] 第 1 项", "[[module.tool]] 第 2 项",
		"唯一", mp} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误里要有 %q（要能指到第几项）：\n%s", want, msg)
		}
	}
	// 同一个模块撞名时不许退化成"A 与 A"的句式。
	if strings.Contains(msg, `"wasm-text" 与 "wasm-text"`) {
		t.Fatalf("同一个模块里的重复不该说成两个模块撞了：\n%s", msg)
	}
	if got := loadedWasm(); len(got) != 0 {
		t.Fatalf("校验失败不能留下已装载的句柄：%+v", got)
	}
}

// TestWasmDistinctToolNamesAcrossFormsAssemble 是这条校验的**不误伤**用例：
// 两个模块各一个工具、名字不同（一个旧写法、一个 [[module.tool]]）→ 正常装配，
// 两个工具都在表里、都调得通。
//
// 为什么必须钉住：判重加在装载期，最怕的就是"顺手多拦一层"——
// 名字不同却被判成重复，等于把本来能跑的两个插件变成一个都用不了。
func TestWasmDistinctToolNamesAcrossFormsAssemble(t *testing.T) {
	resetWasm(t)
	fixture := filepath.ToSlash(toolFixturePath(t))

	mp := writeManifest(t, `[profile]
name = "wasm-two-names"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id       = "wasm-echo"
backend  = "wasm"
path     = "`+fixture+`"
provides = ["tool:echo"]
tool_name        = "echo"
tool_description = "旧写法的 echo"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}}}'''

[[module]]
id       = "wasm-upper"
backend  = "wasm"
path     = "`+fixture+`"
provides = ["tool:upper"]
`+toolItem("upper", "新写法的 upper", ""))

	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("两个模块的工具名不同就该照常装配，实际报错：%v", err)
	}
	if got := loadedWasm(); len(got) != 2 {
		t.Fatalf("两个模块都该装载，实际 %+v", got)
	}

	reg := assembleForTest(t, mods)
	for _, name := range []string{"echo", "upper"} {
		if _, ok := reg.Get(name); !ok {
			var names []string
			for _, x := range reg.Tools() {
				names = append(names, x.Name())
			}
			t.Fatalf("工具表里没有 %q，实际有 %v", name, names)
		}
	}
	// 两个都要真的调得通：判重不该改变任何一条执行路径。
	for _, c := range []struct{ name, want string }{
		{"echo", "echo:Hi 你好"},
		{"upper", "upper:HI 你好"},
	} {
		tool, _ := reg.Get(c.name)
		res, err := tool.Exec(context.Background(), json.RawMessage(`{"text":"Hi 你好"}`))
		if err != nil {
			t.Fatalf("工具 %q 执行失败：%v", c.name, err)
		}
		if res.Text != c.want {
			t.Fatalf("工具 %q 的正文应当是 %q，实际 %q", c.name, c.want, res.Text)
		}
	}
}

// --- 测试辅助 ---

func fixturePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(wasmFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("夹具不在预期位置（%s）：%v", p, err)
	}
	return p
}

// toolFixturePath 是 Go reactor 夹具的绝对路径。
func toolFixturePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(wasmToolFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("工具夹具不在预期位置（%s）：%v\n跑 python scripts/build-wasm-fixture.py 生成", p, err)
	}
	return p
}

// assembleForTest 真的跑一遍装配，返回工具表——不直接 new 一个注册表：
// 那样就绕过了 Register，而"Register 真的被调到"正是本轮要证明的事。
func assembleForTest(t *testing.T, mods []kernel.Module) spi.ToolRegistry {
	t.Helper()
	k := kernel.New("test")
	root := k.Root()
	t.Cleanup(root.Dispose)
	if _, err := kernel.Assemble(root, mods); err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	reg, err := kernel.Inject(root, spi.ToolsKey)
	if err != nil {
		t.Fatalf("取工具表失败：%v", err)
	}
	return reg
}

// toolTableBytes 走完整链路（manifest → 装载 → 装配 → 工具表 JSON），返回字节。
func toolTableBytes(t *testing.T, manifest string) []byte {
	t.Helper()
	mods, err := loadMods(manifest, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	reg := assembleForTest(t, mods)
	table, err := buildToolTable(reg)
	if err != nil {
		t.Fatalf("工具表构建失败：%v", err)
	}
	out, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// loadWasmTool 走生产路径装一个 wasm 工具并取回来。
func loadWasmTool(t *testing.T, name, extra string) spi.Tool {
	t.Helper()
	fixture := toolFixturePath(t)
	mp := writeManifest(t, `[profile]
name = "wasm-tool"

[[module]]
id      = "tools"
backend = "static"

[[module]]
id       = "wasm-echo"
backend  = "wasm"
path     = "`+filepath.ToSlash(fixture)+`"
provides = ["tool"]
tool_name        = "`+name+`"
tool_description = "wasm 夹具工具"
tool_schema      = '''{"type":"object","properties":{"text":{"type":"string"}}}'''
`+extra)
	mods, err := loadMods(mp, t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	reg := assembleForTest(t, mods)
	tool, ok := reg.Get(name)
	if !ok {
		t.Fatalf("工具表里没有 %q", name)
	}
	return tool
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dsh.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha256hexFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// captureLogs 把 slog 默认 handler 换成 buffer，返回它以便断言日志内容。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// resetWasm 让每个用例从"没有已装载 wasm"开始，并在结束时收干净。
// 顺带把 slog 静音：Apply 的白名单告警每个用例 40+ 行，不该淹掉测试输出。
func resetWasm(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	closeLoadedWasm(context.Background())
	t.Cleanup(func() { closeLoadedWasm(context.Background()) })
}
