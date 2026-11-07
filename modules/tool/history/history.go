// Package history 提供 `history_search` 工具：在**压缩前的原始日志**里按关键词回查。
//
// 压缩把细节换成摘要之后，模型需要一个"翻旧账"的入口。日志是 append-only，
// 原文永远在——这个工具就是那条回去的路。
package history

import (
	"context"
	"encoding/json"
	"fmt"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "要回查的关键词（字面匹配，大小写敏感）"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 50, "description": "最多返回多少条，默认 10"}
  },
  "required": ["query"],
  "additionalProperties": false
}`)

type Tool struct{ svc spi.HistorySearch }

func (t *Tool) Name() string            { return "history_search" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "在原始日志里按关键词回查（含已被压缩遮蔽的内容）；想找回「之前那段细节」时用。"
}

type args struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type data struct {
	Query     string           `json:"query"`
	Count     int              `json:"count"`
	Hits      []spi.HistoryHit `json:"hits"`
	Truncated bool             `json:"truncated"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"query": "关键词", "limit"?: 10}`}, err
	}
	if a.Query == "" {
		return spi.Result{Hint: "query 不能为空；给一个具体的词或路径片段"}, fmt.Errorf("query 为空")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	hits, err := t.svc.Search(a.Query, limit+1) // 多取一条用来判断是否截断
	if err != nil {
		return spi.Result{Hint: "当前没有活动会话，或者会话文件读不了；先发一条消息再回查"}, err
	}
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}

	summary := fmt.Sprintf("命中 %d 条「%s」", len(hits), a.Query)
	if truncated {
		summary += "（已截断）"
	}
	hint := ""
	if len(hits) == 0 {
		hint = "没命中：换个更短的词，或者先 read/grep 相关文件"
	}
	return spi.Result{
		Summary: summary,
		Data:    data{Query: a.Query, Count: len(hits), Hits: hits, Truncated: truncated},
		Hint:    hint,
	}, nil
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-history",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools", "history"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			svc, err := kernel.Inject(s, spi.HistoryKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{svc: svc})
		},
	}
}
