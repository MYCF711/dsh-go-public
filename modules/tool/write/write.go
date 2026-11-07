// Package write 是写文件工具：新建或**在读过之后**覆盖。
//
// 覆盖一个没读过的文件会被 FS 的读后写策略拒绝——盲改是在拿别人的修改赌博。
package write

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
    "file_path": {"type": "string", "description": "文件路径（正斜杠，相对工作区根）"},
    "content": {"type": "string", "description": "完整文件内容"}
  },
  "required": ["file_path", "content"],
  "additionalProperties": false
}`)

// Tool 实现 spi.Tool。
type Tool struct {
	fs spi.FS
}

func (t *Tool) Name() string   { return "write" }
func (t *Tool) Caps() spi.Caps { return spi.Caps{Writes: true} }
func (t *Tool) Description() string {
	return "新建或整体覆盖文件。覆盖已有文件前必须先 read。"
}
func (t *Tool) Schema() json.RawMessage { return schema }

type args struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type payload struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Created bool   `json:"created"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"file_path": "...", "content": "..."}`},
			fmt.Errorf("参数解析失败: %w", err)
	}
	if strings.TrimSpace(a.FilePath) == "" {
		return spi.Result{Hint: "file_path 不能为空"}, fmt.Errorf("file_path 为空")
	}

	size, exists, err := t.fs.Stat(a.FilePath)
	if err != nil {
		return spi.Result{Hint: "路径越界或不可访问；只允许写工作区内的文件"}, err
	}
	if err := t.fs.CheckWritable(a.FilePath, "write"); err != nil {
		return spi.Result{Hint: "覆盖已有文件前必须先 read 它；要新建文件就不用读"}, err
	}

	data := []byte(a.Content)
	if err := t.fs.Write(a.FilePath, data); err != nil {
		return spi.Result{Hint: "写入失败；确认路径可写、磁盘未满"}, err
	}

	p := payload{
		Path:    strings.ReplaceAll(a.FilePath, "\\", "/"),
		Bytes:   len(data),
		Created: !exists,
	}
	verb := "覆盖"
	if p.Created {
		verb = "新建"
	}
	summary := fmt.Sprintf("%s %s（%d 字节，原 %d 字节）", verb, p.Path, p.Bytes, size)
	action := "Updated file"
	if p.Created {
		action = "Created file"
	}
	text := "<path>" + p.Path + "</path>" + "\n" +
		"<type>file</type>" + "\n" +
		"<content>" + "\n" +
		action + "\n" +
		"</content>"
	return spi.Result{Summary: summary, Data: p, Text: text}, nil
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-write",
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
