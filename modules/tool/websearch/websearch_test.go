package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dshgo/internal/spi"
)

type fakeSearch struct {
	res spi.SearchResult
	err error
	got []string
	lim int
}

func (f *fakeSearch) Search(ctx context.Context, q []string, limit int) (spi.SearchResult, error) {
	f.got = q
	f.lim = limit
	return f.res, f.err
}

// 正常：摘要 + 来源都进正文，Data 结构化。
func TestSearchOK(t *testing.T) {
	svc := &fakeSearch{res: spi.SearchResult{
		Answer: "答案是 42",
		Sources: []spi.SearchSource{
			{Title: "来源一", URL: "https://a.example", Snippet: "片段一"},
			{Title: "来源二", URL: "https://b.example"},
		},
	}}
	tool := &Tool{svc: svc}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"queries":["普遍性问题"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "答案是 42") || !strings.Contains(res.Text, "https://a.example") {
		t.Fatalf("正文要含摘要与链接: %q", res.Text)
	}
	d := res.Data.(data)
	if d.Count != 2 || d.Answer == "" {
		t.Fatalf("Data 不对: %+v", d)
	}
	if svc.lim != maxResults || len(svc.got) != 1 {
		t.Fatalf("要把查询与上限传下去: %v %d", svc.got, svc.lim)
	}
}

// 没装后端 ≠ 搜出来是空的。必须报错并指路。
func TestNoProviderIsLoud(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"queries":["x"]}`))
	if err == nil {
		t.Fatal("没有后端必须报错，不能返回空结果")
	}
	if !strings.Contains(res.Hint, "web_fetch") {
		t.Fatalf("Hint 要给出退路: %q", res.Hint)
	}
}

// 后端报"没配置"与后端连不上，说法要不一样。
func TestUnconfiguredVsUnreachable(t *testing.T) {
	unconf := &Tool{svc: &fakeSearch{err: errors.New("没有配置搜索服务")}}
	res, _ := unconf.Exec(context.Background(), json.RawMessage(`{"queries":["x"]}`))
	if !strings.Contains(res.Hint, "DSHD_SEARCH_URL") {
		t.Fatalf("没配置要提具体配置项: %q", res.Hint)
	}

	down := &Tool{svc: &fakeSearch{err: errors.New("搜索服务连不上: dial tcp")}}
	res, _ = down.Exec(context.Background(), json.RawMessage(`{"queries":["x"]}`))
	if strings.Contains(res.Hint, "DSHD_SEARCH_URL") {
		t.Fatalf("连不上不该让人去改配置: %q", res.Hint)
	}
	if !strings.Contains(res.Hint, "web_fetch") {
		t.Fatalf("连不上要给出退路: %q", res.Hint)
	}
}

// 零命中：不是错误，但要说"换个说法"。
func TestZeroHits(t *testing.T) {
	tool := &Tool{svc: &fakeSearch{res: spi.SearchResult{}}}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"queries":["不存在的东西"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Hint, "换个更常见的说法") {
		t.Fatalf("零命中要给下一步: %q", res.Hint)
	}
}

// 输入边界：空数组 / 空词 / 超过 4 条。
func TestInputLimits(t *testing.T) {
	tool := &Tool{svc: &fakeSearch{}}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"queries":[]}`)); err == nil {
		t.Fatal("空数组要报错")
	}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"queries":["  "]}`)); err == nil {
		t.Fatal("空词要报错")
	}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"queries":["a","b","c","d","e"]}`)); err == nil {
		t.Fatal("超过 4 条要报错")
	}
}
