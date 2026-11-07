// Package readimage 提供 `read_image` 工具：把一张图片交给模型"看"。
//
// 关键不在读文件，而在**图片怎么到达模型**：OpenAI 兼容接口的 tool 消息不支持
// 图片，只有 user 消息能带内容块。所以这个工具把图放进 `Result.Images`，
// 由 agent 循环紧跟一条 user 消息送出去（见 internal/agent/loop.go）。
// 走通这条链之前，"读图成功"只是一句空话——模型看不见图。
package readimage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {"type": "string", "description": "图片文件路径（相对当前工作目录）"}
  },
  "required": ["file_path"],
  "additionalProperties": false
}`)

// 单张图上限。base64 会膨胀 4/3，而模型那边还有自己的大小限制——
// 与其让它 400，不如这里先拦下并说清怎么办。
const maxBytes = 4 << 20

type Tool struct{}

func (t *Tool) Name() string            { return "read_image" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "读取一张图片交给模型看（PNG/JPEG/WebP/GIF）。要看图时用，不要自己造缩略图。"
}

type args struct {
	FilePath string `json:"file_path"`
}

type data struct {
	Path  string `json:"path"`
	MIME  string `json:"mime"`
	Bytes int    `json:"bytes"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"file_path": "图片路径"}`}, err
	}
	p := strings.TrimSpace(a.FilePath)
	if p == "" {
		return spi.Result{Hint: "file_path 不能为空：给一个图片文件的路径"}, fmt.Errorf("file_path 为空")
	}

	st, err := os.Stat(p)
	if err != nil {
		return spi.Result{Hint: "文件不存在：先用 glob 确认路径（注意扩展名与相对目录）"},
			fmt.Errorf("文件不存在: %s", p)
	}
	if st.IsDir() {
		return spi.Result{Hint: "这是目录不是图片：指定到具体文件"}, fmt.Errorf("是目录: %s", p)
	}
	if st.Size() > maxBytes {
		return spi.Result{Hint: fmt.Sprintf("图片 %d 字节，超过上限 %d：先缩小再读，别整张塞进来", st.Size(), maxBytes)},
			fmt.Errorf("图片过大: %d 字节", st.Size())
	}
	if st.Size() == 0 {
		return spi.Result{Hint: "空文件：确认写入/下载成功了再读"}, fmt.Errorf("空文件: %s", p)
	}

	buf, err := os.ReadFile(p)
	if err != nil {
		return spi.Result{Hint: "读不动这个文件：确认权限，或用 bash ls -l 看看"}, err
	}
	// 按内容判类型，不信扩展名——改过名的 .png 到处都是。
	mime := http.DetectContentType(buf)
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return spi.Result{Hint: fmt.Sprintf("这不是支持的图片（识别为 %s）：只收 PNG/JPEG/WebP/GIF", mime)},
			fmt.Errorf("不支持的图片类型: %s", mime)
	}

	return spi.Result{
		Summary: fmt.Sprintf("图片 %s（%s，%d 字节）", filepath.Base(p), mime, len(buf)),
		Data:    data{Path: filepath.ToSlash(p), MIME: mime, Bytes: len(buf)},
		Images:  []spi.Image{{MIME: mime, DataBase64: base64.StdEncoding.EncodeToString(buf)}},
	}, nil
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-readimage",
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
