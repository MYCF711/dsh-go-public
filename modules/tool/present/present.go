// Package present 提供 `present` 工具：把**已存在**的文件声明为交付物。
//
// 它不复制内容、不移动文件、不生成任何东西——只是"点名"：告诉人该打开哪几个。
// 契约照抄上游（research/20-dsh-tool-schemas.json）：一次最多 4 个。
//
// 为什么要校验存在性：模型很容易在路径上想当然（拼错目录、忘了扩展名）。
// 不校验的话它会说"已交付"，人打开却是空的——**声明的价值全在真实**。
package present

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "array",
      "description": "通常 1-2 个最重要的交付物；一次最多 4 个",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "已存在文件的路径（相对当前工作目录）"},
          "description": {"type": "string", "description": "给人看的一句话说明"}
        },
        "required": ["path"],
        "additionalProperties": false
      }
    }
  },
  "required": ["files"],
  "additionalProperties": false
}`)

const maxFiles = 4

type File struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
	Bytes       int64  `json:"bytes"`
}

type args struct {
	Files []struct {
		Path        string `json:"path"`
		Description string `json:"description"`
	} `json:"files"`
}

type data struct {
	Files []File `json:"files"`
	Count int    `json:"count"`
}

type Tool struct{}

func (t *Tool) Name() string            { return "present" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "把已存在的文件声明为交付物（不复制内容）。人需要单独打开某个文件时用。"
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"files": [{"path": "…", "description"?: "…"}]}`}, err
	}
	if a.Files == nil {
		return spi.Result{Hint: `必须给 files：这只是"声明"，没有"清空"语义`},
			fmt.Errorf("缺 files")
	}
	if len(a.Files) == 0 {
		return spi.Result{Hint: "一次至少给 1 个文件；没有交付物就别调这个工具"},
			fmt.Errorf("files 为空")
	}
	if len(a.Files) > maxFiles {
		return spi.Result{Hint: fmt.Sprintf("一次最多 %d 个：先给最重要的那几个，其余在正文里说", maxFiles)},
			fmt.Errorf("文件过多: %d", len(a.Files))
	}

	out := make([]File, 0, len(a.Files))
	for i, f := range a.Files {
		p := strings.TrimSpace(f.Path)
		if p == "" {
			return spi.Result{Hint: fmt.Sprintf("第 %d 个没给 path", i+1)},
				fmt.Errorf("第 %d 个 path 为空", i+1)
		}
		st, err := os.Stat(p)
		if err != nil {
			return spi.Result{Hint: fmt.Sprintf("%s 不存在：先用 glob 确认路径（注意扩展名与相对目录）", p)},
				fmt.Errorf("文件不存在: %s", p)
		}
		if st.IsDir() {
			return spi.Result{Hint: fmt.Sprintf("%s 是目录：交付要指定到具体文件", p)},
				fmt.Errorf("是目录不是文件: %s", p)
		}
		if st.Size() == 0 {
			// 空文件也算"存在"，但值得说一句——多半是写失败了。
			return spi.Result{Hint: fmt.Sprintf("%s 是空文件：确认写入成功了再交付", p)},
				fmt.Errorf("空文件: %s", p)
		}
		out = append(out, File{
			Path:        filepath.ToSlash(p),
			Description: strings.TrimSpace(f.Description),
			Bytes:       st.Size(),
		})
	}

	summary := fmt.Sprintf("交付 %d 个文件", len(out))
	if len(out) == 1 {
		summary += "：" + filepath.Base(out[0].Path)
	} else {
		names := make([]string, 0, len(out))
		for _, f := range out {
			names = append(names, filepath.Base(f.Path))
		}
		summary += "：" + strings.Join(names, "、")
	}

	return spi.Result{
		Summary: summary,
		Data:    data{Files: out, Count: len(out)},
	}, nil
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-present",
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
