// Package todo 提供 `todo_write` 工具：记录/更新任务清单。
//
// 契约照抄上游（research/20-dsh-tool-schemas.json）：**整份替换**，不是增量。
// 模型每次给完整的清单——这样"删掉一项"和"改一项"都不需要额外动词，
// 也不会出现"本地追加、上游以为替换"的分叉。
package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "完整的任务清单，替换掉之前的清单",
      "items": {
        "type": "object",
        "properties": {
          "content": {"type": "string", "description": "任务是什么——一句祈使句"},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed"],
                     "description": "pending（没开始）| in_progress（正在做）| completed（做完了）"}
        },
        "required": ["content", "status"],
        "additionalProperties": false
      }
    }
  },
  "required": ["todos"],
  "additionalProperties": false
}`)

// 上界：清单本来给人看，超过这个数就不再是"清单"而是噪声。
const maxItems = 50

type Item struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

type args struct {
	Todos []Item `json:"todos"`
}

type counts struct {
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Completed  int `json:"completed"`
}

type data struct {
	Todos  []Item `json:"todos"`
	Counts counts `json:"counts"`
	Total  int    `json:"total"`
}

type Tool struct {
	mu  sync.Mutex
	cur []Item
}

func (t *Tool) Name() string            { return "todo_write" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "记录或更新任务清单（整份替换）。多步骤工作时用它把计划摆出来、让人看得见进度。"
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"todos": [{"content": "…", "status": "pending|in_progress|completed"}]}`}, err
	}

	// 空清单是合法的（表示"清空"），但要显式传 []，不能漏字段。
	if a.Todos == nil {
		return spi.Result{Hint: `必须给 todos：清空就传 "todos": []`},
			fmt.Errorf("缺 todos")
	}
	if len(a.Todos) > maxItems {
		return spi.Result{Hint: fmt.Sprintf("清单最多 %d 项；把已完成的合并或删掉再写", maxItems)},
			fmt.Errorf("清单过长: %d 项", len(a.Todos))
	}

	var c counts
	clean := make([]Item, 0, len(a.Todos))
	for i, it := range a.Todos {
		content := strings.TrimSpace(it.Content)
		if content == "" {
			return spi.Result{Hint: fmt.Sprintf("第 %d 项没有 content；每项都要一句话说清要做什么", i+1)},
				fmt.Errorf("第 %d 项 content 为空", i+1)
		}
		switch it.Status {
		case "pending":
			c.Pending++
		case "in_progress":
			c.InProgress++
		case "completed":
			c.Completed++
		default:
			return spi.Result{Hint: fmt.Sprintf(`第 %d 项的 status 只能是 "pending" / "in_progress" / "completed"`, i+1)},
				fmt.Errorf("第 %d 项 status 非法: %q", i+1, it.Status)
		}
		clean = append(clean, Item{Content: content, Status: it.Status})
	}

	t.mu.Lock()
	t.cur = clean
	t.mu.Unlock()

	summary := fmt.Sprintf("%d 项", len(clean))
	if len(clean) > 0 {
		parts := make([]string, 0, 3)
		if c.InProgress > 0 {
			parts = append(parts, fmt.Sprintf("%d 进行中", c.InProgress))
		}
		if c.Pending > 0 {
			parts = append(parts, fmt.Sprintf("%d 待办", c.Pending))
		}
		if c.Completed > 0 {
			parts = append(parts, fmt.Sprintf("%d 完成", c.Completed))
		}
		summary += "（" + strings.Join(parts, " / ") + "）"
	}

	hint := ""
	if c.InProgress > 1 && len(clean) > 3 {
		hint = "同时进行中的有 " + fmt.Sprint(c.InProgress) + " 项；除非确实并行，否则一次只留一项 in_progress"
	}
	return spi.Result{
		Summary: summary,
		Data:    data{Todos: clean, Counts: c, Total: len(clean)},
		Hint:    hint,
	}, nil
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-todo",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{})
		},
	}
}
