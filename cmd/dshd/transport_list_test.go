package main

import (
	"os"
	"testing"
	"time"
)

// TestListSessionsGolden 拿**真实语料**跑 listSessions，钉住界面真正依赖的几条：
//   - 至少 1 条会话（否则面板是空的，转成空表也"通过"了，什么也没证明）
//   - 每条 ID 非空、Title 非空（空标题在界面上就是一行看不见的条目）
//   - Title 长度 ≤ 120（一行放得下；这条是界面的排版约束，不是数据的性质）
//   - 按 UpdatedAt 从新到旧
//
// 语料不在仓库里（只读引用 DSH_TEST_HOME），CI 上没有就 Skip——
// 这不是"测不到"，是"语料不该进仓库"。全程只读，不写这个 home。
func TestListSessionsGolden(t *testing.T) {
	home := os.Getenv("DSH_TEST_HOME")
	if home == "" {
		t.Skip("没设 DSH_TEST_HOME，跳过真实语料 golden（CI 上本来就没有这份语料）")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("DSH_TEST_HOME 指的地方读不了: %v", err)
	}

	// cwd 传空 = 不按项目过滤，把所有项目都列出来，样本最大。
	got, err := listSessions(home, "")
	if err != nil {
		t.Fatalf("listSessions(%q, \"\") 出错: %v", home, err)
	}
	if len(got) == 0 {
		t.Fatalf("语料里有 0 条会话——要么 home 指错了，要么 ListSessions 退化了")
	}
	t.Logf("%d 条会话", len(got))

	// 排序必须由 listSessions 保证，不能让测试"碰巧"过：
	// 这里自己解析回 time.Time 比，而不是比字符串（RFC3339 带时区偏移，字典序不可靠）。
	prev := time.Time{}
	placeholders := 0
	for i, s := range got {
		if s.ID == "" {
			t.Fatalf("第 %d 条会话 ID 是空串", i)
		}
		if s.Title == "" {
			t.Fatalf("会话 %s 的 Title 是空串——界面会渲染成一行看不见的条目", s.ID)
		}
		if s.Title == "(无标题)" {
			placeholders++
		}
		// 长度按**字符**数算：摘要里有中文，按字节算会把 40 个汉字的标题判成超长。
		if n := len([]rune(s.Title)); n > 120 {
			t.Fatalf("会话 %s 的 Title %d 字符，超过界面一行的上限 120：%q", s.ID, n, s.Title)
		}

		ts, err := time.Parse(time.RFC3339, s.UpdatedAt)
		if err != nil {
			t.Fatalf("会话 %s 的 UpdatedAt=%q 不是 RFC3339: %v", s.ID, s.UpdatedAt, err)
		}
		if i > 0 && ts.After(prev) {
			t.Fatalf("第 %d 条（%s，%s）比上一条（%s）更新——没有按时间倒序",
				i, s.ID, s.UpdatedAt, prev.Format(time.RFC3339))
		}
		prev = ts
	}

	t.Logf("最新一条: %s | %s", got[0].Title, got[0].UpdatedAt)
	// 占位符不算"能看懂"。原来的断言只要求非空串，而 "(无标题)" 恰好能骗过它——
	// 界面左栏会整整一屏都是这四个字，测试却是绿的。
	if placeholders*2 > len(got) {
		t.Fatalf("%d/%d 条会话的标题是占位符——摘要生成坏了，不是「这些会话没内容」",
			placeholders, len(got))
	}

}
