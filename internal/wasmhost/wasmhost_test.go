package wasmhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const fixture = "testdata/fixture.wasm"

// 正常装载 + 调用导出函数。
func TestLoadAndCall(t *testing.T) {
	ctx := context.Background()
	m, err := Load(ctx, "fixture", fixture)
	if err != nil {
		t.Fatalf("装载失败：%v", err)
	}
	defer m.Close(ctx)

	got, err := m.Call(ctx, "add", 2, 3)
	if err != nil {
		t.Fatalf("调用 add 失败：%v", err)
	}
	if len(got) != 1 || got[0] != 5 {
		t.Fatalf("add(2,3) = %v，期望 5", got)
	}
}

// 文件不存在：报错要带路径，不然"哪个文件没了"得靠猜。
func TestLoadMissingFile(t *testing.T) {
	_, err := Load(context.Background(), "nope", "testdata/no-such.wasm")
	if err == nil {
		t.Fatal("不存在的文件必须报错")
	}
	if !contains(err.Error(), "no-such.wasm") {
		t.Fatalf("错误里要带路径：%v", err)
	}
}

// 不是 wasm：要明确说"魔数不对"，而不是"编译失败"。
func TestLoadNonWasm(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.wasm")
	if err := os.WriteFile(p, []byte("这不是 wasm，只是一段文本"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), "fake", p)
	if err == nil {
		t.Fatal("非 wasm 必须报错")
	}
	if !contains(err.Error(), "0asm") {
		t.Fatalf("要说清是魔数不对：%v", err)
	}
}

// Close 可重复调用。
func TestCloseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	m, err := Load(ctx, "fixture", fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatalf("第一次 Close 出错：%v", err)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatalf("第二次 Close 应当无害，实际：%v", err)
	}
	// 卸载之后调用要报错，而不是返回零值假装成功
	if _, err := m.Call(ctx, "add", 1, 1); err == nil {
		t.Fatal("已卸载的模块不该还能调用")
	}
}

// 摘要：同一文件两次装载一致，不同文件不同。
func TestDigest(t *testing.T) {
	ctx := context.Background()
	a, err := Load(ctx, "a", fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	b, err := Load(ctx, "b", fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)

	if a.Digest() != b.Digest() {
		t.Fatalf("同一文件两次装载摘要不同：%s vs %s", a.Digest(), b.Digest())
	}
	if len(a.Digest()) != 64 {
		t.Fatalf("sha256 应当是 64 个十六进制字符，实际 %d", len(a.Digest()))
	}

	// 摘要必须**就是文件内容的 sha256**：拿它跟独立算的比，
	// 而不是跟自己比——那只能证明它稳定，证明不了它是什么。
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256hex(raw)
	if a.Digest() != want {
		t.Fatalf("摘要不是文件内容的 sha256：%s vs %s", a.Digest(), want)
	}
}

// 调用不存在的导出函数：报错，不返回零值。
func TestCallUnknownExport(t *testing.T) {
	ctx := context.Background()
	m, err := Load(ctx, "fixture", fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx)
	if _, err := m.Call(ctx, "no_such_fn", 1); err == nil {
		t.Fatal("不存在的导出函数必须报错")
	}
}

// Exports 要列出**能调的函数**，并且按名字排序。
//
// 为什么同一个断言跑 20 遍：wazero 的 ExportedFunctionDefinitions() 返回 map，
// 而 Go 的 map 遍历顺序每次都不一样。只比一次的话，"没排序"的实现
// 有 1/2 的概率碰巧是对的（夹具两个导出名正好是升序），测试于是变成掷硬币；
// 跑 20 次，没排序的实现几乎必然（1-2⁻²⁰）在某一轮暴露。
func TestExports(t *testing.T) {
	ctx := context.Background()
	m, err := Load(ctx, "fixture", fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx)

	want := []string{"add", "spin"}
	for i := 0; i < 20; i++ {
		got := m.Exports()
		if len(got) != len(want) {
			t.Fatalf("第 %d 次 Exports() = %v，期望 %v", i+1, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("第 %d 次 Exports() = %v，期望 %v（且按名字排序）", i+1, got, want)
			}
		}
	}
}

// 卸载之后没有导出可说了：返回空，而不是残留旧名字。
func TestExportsAfterClose(t *testing.T) {
	ctx := context.Background()
	m, err := Load(ctx, "fixture", fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.Exports(); len(got) != 0 {
		t.Fatalf("已卸载的模块还有导出：%v", got)
	}
}

// 超时必须**真的打断执行**，而不是"ctx 到期了，Call 还在里面转"。
//
// 夹具里的 spin() 是一个 loop/br 0 的死循环：不调用任何函数、不碰内存，
// 没有任何自我结束的可能。唯一能让它停下来的，是 wazero 的
// CloseOnContextDone 在循环回边上检查 ctx。所以这个用例同时钉两件事：
//   - 返回的错误能被 errors.Is 认成 context.DeadlineExceeded；
//   - 它确实在超时附近返回了（宽限 10s 只为机器慢，真没打断会一直挂着）。
func TestCallTimeoutInterrupts(t *testing.T) {
	ctx := context.Background()
	m, err := Load(ctx, "fixture", fixture)
	if err != nil {
		t.Fatal(err)
	}
	// 用 Background 关闭：Call 超时之后 ctx 已经是过期的了。
	defer m.Close(context.Background())

	callCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = m.Call(callCtx, "spin")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("spin() 是死循环，超时后必须返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误要能认出是超时（context.DeadlineExceeded），实际：%v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("超时设的 200ms，却跑了 %s 才返回——ctx 没有真的打断执行", elapsed)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// sha256hex 独立算一遍，作为对照。
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
