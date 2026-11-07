// Package webfetch 提供 `web_fetch` 工具：取一个 HTTP(S) URL 的正文，解码成文本。
//
// 边界感很重要：
//   - 只接受 http/https（file:// 之类直接拒，避免把本地文件当"网页"读）
//   - 有超时、有大小上限——网页可以无限大，模型上下文不行
//   - 非 2xx 不报"成功但空"，而是把状态码和一小段正文一起回去
package webfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "要取的 HTTP(S) URL"}
  },
  "required": ["url"],
  "additionalProperties": false
}`)

const (
	// 正文上限。超出的部分不取——网页可以无限长，模型上下文不行。
	maxBytes = 512 * 1024
	// 非 2xx 时回给模型的正文片段（够看清错误页就行）。
	maxErrSnippet = 500
)

type Tool struct {
	Client *http.Client
}

func (t *Tool) Name() string            { return "web_fetch" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{Network: true} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "取一个 HTTP(S) 网页的正文并解码成文本。要看具体某个链接的内容时用。"
}

type args struct {
	URL string `json:"url"`
}

type data struct {
	URL         string `json:"url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       int    `json:"bytes"`
	Truncated   bool   `json:"truncated"`
}

func (t *Tool) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	// 超时由 ctx 控制；这里只兜底，防止服务端一直吐字节。
	return &http.Client{Timeout: 30 * time.Second}
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"url": "https://…"}`}, err
	}
	target := strings.TrimSpace(a.URL)
	if target == "" {
		return spi.Result{Hint: "url 不能为空：给完整地址，含 https://"}, fmt.Errorf("url 为空")
	}
	u, err := url.Parse(target)
	if err != nil {
		return spi.Result{Hint: "URL 解析不了：检查是否有空格或中文未转义"}, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return spi.Result{Hint: fmt.Sprintf("只支持 http/https（给的是 %q）；本地文件用 read", u.Scheme)},
			fmt.Errorf("不支持的协议: %s", u.Scheme)
	}
	if u.Host == "" {
		return spi.Result{Hint: "URL 里没有主机名：写成 https://example.com/path 这种形式"},
			fmt.Errorf("缺主机名")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return spi.Result{Hint: "请求构造失败：检查 URL 是否合法"}, err
	}
	// 有些站点对没有 UA 的请求直接 403。
	req.Header.Set("User-Agent", "dsh-go/"+versionish+"")

	resp, err := t.client().Do(req)
	if err != nil {
		return spi.Result{Hint: "连不上：确认域名拼写、网络可达；内网地址可能被策略挡住"},
			fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if readErr != nil {
		return spi.Result{Hint: "读到一半断了：重试一次，或者换一个来源"},
			fmt.Errorf("读正文失败: %w", readErr)
	}
	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}
	text := strings.ToValidUTF8(string(body), "\uFFFD")
	ct := resp.Header.Get("Content-Type")

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := text
		if len([]rune(snippet)) > maxErrSnippet {
			snippet = string([]rune(snippet)[:maxErrSnippet]) + "…"
		}
		hint := "非 2xx：先看状态码——404 是地址不对，403 多半要登录/被挡，5xx 是对方的问题"
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			hint = "被拒绝：这个页面可能要登录或有反爬；换官方的原始地址（如 raw.githubusercontent.com）"
		}
		return spi.Result{
			Summary: fmt.Sprintf("HTTP %d（%d 字节）", resp.StatusCode, len(body)),
			Text:    snippet,
			Data:    data{URL: u.String(), Status: resp.StatusCode, ContentType: ct, Bytes: len(body)},
			Hint:    hint,
		}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	summary := fmt.Sprintf("HTTP %d（%d 字节", resp.StatusCode, len(body))
	if truncated {
		summary += "，已截断"
	}
	summary += "）"

	return spi.Result{
		Summary: summary,
		Text:    text,
		Data:    data{URL: u.String(), Status: resp.StatusCode, ContentType: ct, Bytes: len(body), Truncated: truncated},
		Hint:    truncHint(truncated),
	}, nil
}

func truncHint(truncated bool) string {
	if truncated {
		return fmt.Sprintf("正文超过 %d 字节已截断：需要更多就换更具体的地址（锚点/API），别指望一次取全", maxBytes)
	}
	return ""
}

// versionish 只用于 User-Agent；版本号由宿主在装配时不注入，这里给个稳定值。
const versionish = "0.2.2"

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-webfetch",
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
