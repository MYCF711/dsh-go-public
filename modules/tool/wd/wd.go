// Package wd 提供 `working_directory` 工具：看/换当前工作目录。
//
// 为什么需要它：模型经常要确认"我在哪个目录"。以前只能靠 bash 里的 `pwd`，
// 但 bash 有自己的持久会话，`cd` 只影响那条会话——**别的工具看不到**。
// 这里改的是**进程的** cwd，影响 read/write/glob/grep 与新开的 bash 会话。
//
// 但它**改不了已经开着的那条 bash 会话**（那是另一份状态）。这一点必须写在
// 返回里，否则模型会以为"我切了目录，bash 也切了"——然后在下一次调用里困惑。
package wd

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
    "path": {"type": "string", "description": "要切换到的目录（相对当前目录或绝对路径）；省略则只报告当前目录"}
  },
  "additionalProperties": false
}`)

type Tool struct{}

func (t *Tool) Name() string            { return "working_directory" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "查看或切换当前工作目录（影响 read/write/glob/grep 与新开的 bash）"
}

type args struct {
	Path string `json:"path"`
}

type data struct {
	Path     string `json:"path"`
	Changed  bool   `json:"changed"`
	Parent   string `json:"parent,omitempty"`
	BashNote string `json:"bash_note,omitempty"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"path"?: "目录"}`}, err
	}

	cur, err := os.Getwd()
	if err != nil {
		return spi.Result{Hint: "取不到当前目录；用 bash 的 pwd 看看"}, err
	}

	if strings.TrimSpace(a.Path) == "" {
		return spi.Result{
			Summary: "当前目录: " + filepath.ToSlash(cur),
			Data:    data{Path: filepath.ToSlash(cur)},
		}, nil
	}

	target := a.Path
	if !filepath.IsAbs(target) {
		target = filepath.Join(cur, target)
	}
	st, statErr := os.Stat(target)
	if statErr != nil {
		return spi.Result{Hint: "目录不存在；先 glob 或 bash ls 确认名字"},
			fmt.Errorf("不是目录: %s", a.Path)
	}
	if !st.IsDir() {
		return spi.Result{Hint: "这是文件不是目录；要读它用 read"}, fmt.Errorf("不是目录: %s", a.Path)
	}
	if err := os.Chdir(target); err != nil {
		return spi.Result{Hint: "切换失败（权限或路径问题）；确认目录可进入"}, err
	}

	now, _ := os.Getwd()
	// 已经开着的那条 bash 会话有自己的 cwd，切不到——必须说清。
	note := "已经开着的 bash 会话保留它自己的 cwd；要在那边也切，执行 bash `cd <目录>`"
	return spi.Result{
		Summary: "已切到: " + filepath.ToSlash(now),
		Data: data{
			Path:     filepath.ToSlash(now),
			Changed:  true,
			Parent:   filepath.ToSlash(filepath.Dir(now)),
			BashNote: note,
		},
		Hint: note,
	}, nil
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-wd",
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
