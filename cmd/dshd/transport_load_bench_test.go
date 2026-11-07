package main

// 这个文件只回答一个问题：session.load 的首屏到底花了多少时间。
//
// 现状是「先全量映射、再切尾巴」：sessionSource.Messages 把整个会话读成
// []transport.Message，然后 transport.LoadSession 才用 SelectPage 取最后 limit 条。
// 换句话说，只要会话有 N 条消息，首屏就要为 N 条付出解析 + 分配 + 排序的代价。
//
// 这里**不断言快慢**（机器差异会让这种断言变成噪声），只把两个数字摆出来：
// 全量映射多少条、多少毫秒；首屏 50 条多少毫秒。
// 要不要优化，看这两个数字的比值——如果首屏耗时随会话变长线性增长，
// 那就说明代价确实在「全量」那一侧，而不是在「切 50 条」这一侧。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/transport"
)

// TestLoadCost 量 session.load 的首屏代价。
//
// 样本从环境变量 DSH_TEST_SESSION 给（一个真实的 session.v4.jsonl.zstd 路径）；
// 没设就跳过——这个测试依赖真实语料，没有语料时它什么也证明不了，
// 与其在 CI 里造一个假样本得出一个假数字，不如明确跳过。
func TestLoadCost(t *testing.T) {
	path := os.Getenv("DSH_TEST_SESSION")
	if path == "" {
		t.Skip("没设 DSH_TEST_SESSION，跳过：这条测试要真实会话才有意义")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("DSH_TEST_SESSION 指向的文件读不到：%v", err)
	}

	home, sessionID := splitSessionPath(t, path)
	src := newSessionSource(home, "")

	// ── 第一次：全量映射 ─────────────────────────────────────────────
	startAll := time.Now()
	all, err := src.Messages(sessionID)
	allCost := time.Since(startAll)
	if err != nil {
		t.Fatalf("全量映射失败（home=%s id=%s）：%v", home, sessionID, err)
	}

	// ── 第二次：session.load 的首屏 50 条 ───────────────────────────
	// 注意它内部会把 Messages 再跑一遍：这里量的是**首屏这个动作的总代价**，
	// 包含重复读日志。这正是要暴露的点，不能偷偷复用上一轮的结果。
	startPage := time.Now()
	page, err := transport.LoadSession(src, sessionID, 50)
	pageCost := time.Since(startPage)
	if err != nil {
		t.Fatalf("LoadSession 失败：%v", err)
	}

	t.Logf("全量映射：%d 条消息，%.2f ms", len(all), ms(allCost))
	t.Logf("首屏 50 条：%d 条（total=%d, truncated=%v），%.2f ms",
		len(page.Messages), page.Total, page.Truncated, ms(pageCost))

	// ── 两条硬断言（与机器快慢无关，只保证样本有意义）────────────────
	if len(all) < 50 {
		t.Fatalf("样本只有 %d 条消息（<50），切不出首屏，换一个长会话再量", len(all))
	}
	if len(page.Messages) > 50 {
		t.Fatalf("首屏返回了 %d 条（>50），LoadSession 的 limit 没生效", len(page.Messages))
	}
}

func ms(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

// splitSessionPath 从一个会话文件路径反推出 (home, sessionID)。
//
// 布局是 <home>/sessions/<projectKey>/<seg>/session.v4.jsonl.zstd，
// 所以 home 是「sessions 的上一级」，sessionID 就是 <seg>。
//
// 这里刻意不猜：如果 <seg> 不是 id 的原样（说明它被 ~XXXX 转义过），
// 直接用它会立刻被 EncodeSegment 再转义一次，resolve 必然找不到文件——
// 那种情况下报错信息会指向「会话不存在」，与真实原因差得很远。
// 所以先自证 EncodeSegment(seg)==seg，不成立就跳过并说清楚。
func splitSessionPath(t *testing.T, path string) (home, sessionID string) {
	t.Helper()

	segDir := filepath.Dir(path)
	projDir := filepath.Dir(segDir)
	sessionsDir := filepath.Dir(projDir)
	home = filepath.Dir(sessionsDir)

	if filepath.Base(sessionsDir) != "sessions" {
		t.Fatalf("路径不像 <home>/sessions/<project>/<id>/session.v4.jsonl.zstd：%s", path)
	}

	sessionID = filepath.Base(segDir)
	enc, err := v4.EncodeSegment(sessionID)
	if err != nil {
		t.Fatalf("从路径推出的会话 id %q 不合法：%v", sessionID, err)
	}
	if enc != sessionID {
		t.Skipf("路径里的目录名 %q 是转义过的（原样应为 %q），"+
			"这条测试没法从路径反解出 id，跳过", sessionID, enc)
	}
	return home, sessionID
}
