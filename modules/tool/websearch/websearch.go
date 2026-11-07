// Package websearch 提供 `web_search` 工具：联网搜索，返回摘要答案 + 来源链接。
//
// 它自己不联网——把这件事交给 `spi.SearchProvider`。理由：搜索没有公认的公共端点，
// 每家请求/响应形状都不一样。做成 SPI 之后，"换搜索后端"是换一个插件，
// 不用动这个工具。
//
// **没配置后端时明确报错**，不返回"搜过了，没结果"：后者会让模型以为
// 互联网上真的没有，而不是"这个宿主没接搜索"。这个区别在实际用起来差得很远。
package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "queries": {
      "type": "array",
      "description": "1–4 条查询；结果会合并去重",
      "items": {"type": "string"}
    }
  },
  "required": ["queries"],
  "additionalProperties": false
}`)

const (
	maxQueries = 4
	maxResults = 8
)

type Tool struct{ svc spi.SearchProvider }

func (t *Tool) Name() string            { return "web_search" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{Network: true} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "联网搜索，返回摘要答案与来源链接。要知道「网上现在怎么说」时用。"
}

type args struct {
	Queries []string `json:"queries"`
}

type data struct {
	Queries []string           `json:"queries"`
	Answer  string             `json:"answer,omitempty"`
	Sources []spi.SearchSource `json:"sources"`
	Count   int                `json:"count"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"queries": ["…"]}`}, err
	}
	if len(a.Queries) == 0 {
		return spi.Result{Hint: "queries 不能为空：给至少一条具体的搜索词"}, fmt.Errorf("没有查询词")
	}
	if len(a.Queries) > maxQueries {
		return spi.Result{Hint: fmt.Sprintf("一次最多 %d 条查询：把次要的合并进第一条", maxQueries)},
			fmt.Errorf("查询过多: %d", len(a.Queries))
	}
	clean := make([]string, 0, len(a.Queries))
	for i, q := range a.Queries {
		q = strings.TrimSpace(q)
		if q == "" {
			return spi.Result{Hint: fmt.Sprintf("第 %d 条查询是空的", i+1)}, fmt.Errorf("空查询")
		}
		clean = append(clean, q)
	}
	if t.svc == nil {
		return spi.Result{Hint: "宿主没有装搜索后端；要么配 DSHD_SEARCH_URL，要么改用 web_fetch 直接取某个网址"},
			fmt.Errorf("没有搜索服务")
	}

	res, err := t.svc.Search(ctx, clean, maxResults)
	if err != nil {
		// 后端没配 与 后端连不上 是两回事，别混成一句"搜索失败"。
		if strings.Contains(err.Error(), "没有配置搜索服务") {
			return spi.Result{Hint: "这个宿主没配搜索后端：设 DSHD_SEARCH_URL 指向一个 SearxNG 风格端点，或者改用 web_fetch 直接取具体网址"},
				err
		}
		return spi.Result{Hint: "搜索后端出错：先用 web_fetch 直接取一个你信得过的网址，别在这里反复重试"}, err
	}

	var b strings.Builder
	if res.Answer != "" {
		b.WriteString(res.Answer)
		b.WriteString("\n\n")
	}
	for i, s := range res.Sources {
		fmt.Fprintf(&b, "%d. %s <%s>", i+1, s.Title, s.URL)
		if s.Snippet != "" {
			fmt.Fprintf(&b, "\n   %s", oneLine(s.Snippet))
		}
		b.WriteString("\n")
	}

	summary := fmt.Sprintf("命中 %d 条来源", len(res.Sources))
	if res.Answer != "" {
		summary = "有摘要答案，" + summary
	}
	hint := ""
	if len(res.Sources) == 0 {
		hint = "零命中：换个更常见的说法，或者直接用 web_fetch 打开你想到的那个网址"
	}
	return spi.Result{
		Summary: summary,
		Text:    strings.TrimSpace(b.String()),
		Data:    data{Queries: clean, Answer: res.Answer, Sources: res.Sources, Count: len(res.Sources)},
		Hint:    hint,
	}, nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 160 {
		return string([]rune(s)[:160]) + "…"
	}
	return s
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-websearch",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools", "search"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			svc, err := kernel.Inject(s, spi.SearchKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{svc: svc})
		},
	}
}
