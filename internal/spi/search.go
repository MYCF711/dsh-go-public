package spi

import "context"

// SearchProvider 是"网页搜索"这一能力的抽象。
//
// 为什么是接口而不是直接写死一个 HTTP 调用：搜索这件事没有公认的公共端点，
// 每家（SearxNG / Tavily / Brave / 自建）的请求与响应形状都不一样。
// 把它做成 SPI，"换一个搜索后端"就是换一个插件，不用改工具本身。
//
// **没有配置后端时，默认实现必须明确报错**——不能返回"搜过了，没结果"：
// 那会让模型以为互联网上真的没有，而不是"这个宿主没接搜索"。
type SearchProvider interface {
	// Search 把多个查询合并成一批结果。queries 至少一个。
	Search(ctx context.Context, queries []string, limit int) (SearchResult, error)
}

// SearchResult 是一次搜索的合并结果。
type SearchResult struct {
	// Answer 是搜索服务给的摘要答案；没有就是空。
	Answer string `json:"answer,omitempty"`
	// Sources 是来源列表，顺序即相关性顺序。
	Sources []SearchSource `json:"sources"`
}

// SearchSource 是一条来源。
type SearchSource struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}
