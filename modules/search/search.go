// Package search 提供默认的搜索后端。
//
// 本机没有可用的公共搜索端点（DSH 自己的 web_search 在这台机器上同样是报错的，
// 真实会话里子代理被明确告知"web_search 不可用，改用 curl"）。所以这里的默认实现
// **不假装能搜**：没有配置就直接报错，并说清要配什么。
//
// 配置方式（环境变量）：
//
//	DSHD_SEARCH_URL   搜索端点，SearxNG 风格的 GET，含 {query} 占位
//	                  例如 http://127.0.0.1:8080/search?q={query}&format=json
//
// 响应按 {results:[{title,url,content}], answers?:[...]} 解析（SearxNG 的 json 格式）。
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

// Provider 是按 DSHD_SEARCH_URL 工作的实现。
type Provider struct {
	// Endpoint 是带 {query} 占位的 URL；空表示没配置。
	Endpoint string
	Client   *http.Client
}

func (p *Provider) Search(ctx context.Context, queries []string, limit int) (spi.SearchResult, error) {
	if strings.TrimSpace(p.Endpoint) == "" {
		return spi.SearchResult{}, fmt.Errorf("没有配置搜索服务")
	}
	if len(queries) == 0 {
		return spi.SearchResult{}, fmt.Errorf("没有查询词")
	}
	if limit <= 0 {
		limit = 8
	}

	var merged spi.SearchResult
	seen := map[string]bool{}
	for _, q := range queries {
		one, err := p.query(ctx, q)
		if err != nil {
			return spi.SearchResult{}, err
		}
		if merged.Answer == "" {
			merged.Answer = one.Answer
		}
		for _, s := range one.Sources {
			if seen[s.URL] {
				continue
			}
			seen[s.URL] = true
			merged.Sources = append(merged.Sources, s)
			if len(merged.Sources) >= limit {
				break
			}
		}
		if len(merged.Sources) >= limit {
			break
		}
	}
	return merged, nil
}

func (p *Provider) query(ctx context.Context, q string) (spi.SearchResult, error) {
	target := strings.ReplaceAll(p.Endpoint, "{query}", url.QueryEscape(q))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return spi.SearchResult{}, fmt.Errorf("搜索请求构造失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c := p.Client
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return spi.SearchResult{}, fmt.Errorf("搜索服务连不上: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return spi.SearchResult{}, fmt.Errorf("读搜索响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return spi.SearchResult{}, fmt.Errorf("搜索服务返回 %d", resp.StatusCode)
	}

	var raw struct {
		Answer  string   `json:"answer"`
		Answers []string `json:"answers"`
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return spi.SearchResult{}, fmt.Errorf("搜索响应不是预期的 JSON: %w", err)
	}

	out := spi.SearchResult{Answer: raw.Answer}
	if out.Answer == "" && len(raw.Answers) > 0 {
		out.Answer = raw.Answers[0]
	}
	for _, r := range raw.Results {
		out.Sources = append(out.Sources, spi.SearchSource{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// Module 装配默认搜索后端。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "search",
		Backend:  kernel.BackendStatic,
		Provides: []string{"search"},
		Register: func(s *kernel.Scope) error {
			return kernel.Provide(s, spi.SearchKey, spi.SearchProvider(
				&Provider{Endpoint: os.Getenv("DSHD_SEARCH_URL")}))
		},
	}
}
