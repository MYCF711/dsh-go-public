// Package read 是读文件工具：返回结构化结果而不是 shell 回显。
//
// 它同时承担「观测」职责：读过的文件才允许被 write/edit 修改（读后写策略在 FS 里）。
package read

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

// schema 是稳定契约：不含时间戳、绝对路径、随机 id。
var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {"type": "string", "description": "文件路径（正斜杠，相对工作区根）"},
    "offset": {"type": "integer", "minimum": 1, "description": "起始行号（1 基），默认 1"},
    "limit": {"type": "integer", "minimum": 1, "description": "最多返回多少行，默认 2000"}
  },
  "required": ["file_path"],
  "additionalProperties": false
}`)

const (
	defaultLimit = 2000
	maxLimit     = 2000
)

// Tool 实现 spi.Tool。
type Tool struct {
	fs spi.FS
}

func (t *Tool) Name() string   { return "read" }
func (t *Tool) Caps() spi.Caps { return spi.Caps{} }
func (t *Tool) Description() string {
	return "读文件（按行窗口）。改文件前必须先读；大文件用 offset/limit 分段。"
}

func (t *Tool) Schema() json.RawMessage { return schema }

type args struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

type payload struct {
	Path       string `json:"path"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	TotalLines int    `json:"total_lines"`
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不是合法 JSON：期望 {"file_path": "...", "offset"?: 1, "limit"?: 2000}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	if strings.TrimSpace(a.FilePath) == "" {
		return spi.Result{Hint: "file_path 不能为空；先用 glob 找到文件再读"},
			fmt.Errorf("file_path 为空")
	}
	if a.Offset <= 0 {
		a.Offset = 1
	}
	if a.Limit <= 0 {
		a.Limit = defaultLimit
	}
	if a.Limit > maxLimit {
		a.Limit = maxLimit
	}

	data, err := t.fs.Read(a.FilePath)
	if err != nil {
		return spi.Result{Hint: "确认路径存在且是文件；用 glob/grep 先定位，不要猜路径"},
			err
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	// 结尾换行会多切一个空元素，去掉它以免 total_lines 多 1。
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)

	// offset 越过文件末尾：**必须报错**。
	// 早先这里是静默返回空内容，模型会以为"文件这一段是空的"——
	// 一个错误答案，比报错糟糕得多。DSH 也是这么做的。
	if a.Offset > total && total > 0 {
		return spi.Result{
				Hint: fmt.Sprintf("这个文件只有 %d 行；要读末尾就用 offset=%d，要从头读就别传 offset", total, total),
			},
			fmt.Errorf("offset %d 超出范围：%q 只有 %d 行", a.Offset, a.FilePath, total)
	}

	start := a.Offset
	end := start + a.Limit - 1
	if end > total {
		end = total
	}
	var body string
	if start <= total {
		body = strings.Join(lines[start-1:end], "\n")
	}

	p := payload{
		Path:       strings.ReplaceAll(a.FilePath, "\\", "/"),
		StartLine:  start,
		EndLine:    end,
		TotalLines: total,
		Content:    body,
		Truncated:  end < total,
	}
	summary := fmt.Sprintf("%s 第 %d-%d 行（共 %d 行）", p.Path, start, end, total)

	// 模型可见正文对齐 DSH：<path>/<type>/<content> 包裹 + 行号前缀。
	var rendered strings.Builder
	rendered.WriteString("<path>" + p.Path + "</path>" + "\n")
	rendered.WriteString("<type>file</type>" + "\n")
	rendered.WriteString("<content>" + "\n")
	if start <= total {
		for i := start; i <= end; i++ {
			fmt.Fprintf(&rendered, "%d: %s\n", i, lines[i-1])
		}
	}
	rendered.WriteString("</content>")
	hint := ""
	if end < total {
		hint = fmt.Sprintf("还有 %d 行没读；用 offset=%d 继续", total-end, end+1)
	}
	return spi.Result{
		Summary: summary,
		Data:    p,
		Text:    rendered.String(),
		Hint:    hint,
		View: &spi.ViewSpec{
			Kind:  spi.ViewPlain,
			Title: p.Path,
			Badge: fmt.Sprintf("%d-%d/%d", start, end, total),
			Body:  body,
		},
	}, nil
}

// Module 返回装配用的模块描述：消费 "tools" 与 "fs"，不提供能力。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-read",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools", "fs"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			fsv, err := kernel.Inject(s, spi.FSKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{fs: fsv})
		},
	}
}
