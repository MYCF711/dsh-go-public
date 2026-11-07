// Package github 提供 `github` 工具：按 path 调 GitHub REST API（https://api.github.com/<path>）。
//
// 边界感：
//   - 只拼 https://api.github.com，不接受完整 URL——避免变成"任意域名代理"
//   - 写方法（POST/PATCH/PUT/DELETE）必须显式 confirm，否则直接拒
//   - 令牌按 GITHUB_TOKEN → GH_TOKEN → `gh auth token` 取；**永远不进 Result**
//   - 没有令牌就只允许 GET（公共读），写操作明确报错并说清要配什么
//   - 非 2xx 不报"成功但空"：把状态码和一小段正文一起回去
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "GitHub API 路径，不带前导斜杠，如 repos/o/r/issues"},
    "method": {"type": "string", "enum": ["GET", "POST", "PATCH", "PUT", "DELETE"], "description": "HTTP 方法，默认 GET；写方法必须带 confirm"},
    "body": {"type": "string", "description": "写操作的 JSON 请求体（字符串形式）"},
    "confirm": {"type": "boolean", "description": "写操作必须为 true，否则直接拒绝"}
  },
  "required": ["path"],
  "additionalProperties": false
}`)

const (
	// 输出上限。超过的部分不取——响应可以无限长，模型上下文不行。
	maxBytes = 200000
	// 非 2xx 时回给模型的正文片段（够看清错误信息就行）。
	maxErrSnippet = 500
	apiBase       = "https://api.github.com/"
	// 只用于 User-Agent；与宿主版本解耦，避免把版本号写进 Schema 契约。
	versionish = "0.2.2"
)

// writeMethods 是会改远端状态的 HTTP 方法。
var writeMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPatch:  true,
	http.MethodPut:    true,
	http.MethodDelete: true,
}

type Tool struct {
	Client *http.Client
}

func (t *Tool) Name() string { return "github" }
func (t *Tool) Caps() spi.Caps {
	// Caps 是静态的，做不到"GET 只读、POST 写"：一律按会写来算。
	// 代价是读也要审批——这一点写在 Description 里，别让模型以为读是免费的。
	return spi.Caps{Network: true, Writes: true}
}
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "调 GitHub REST API（path 如 repos/o/r/issues）。写方法要 confirm 与令牌。"
}

type args struct {
	Path    string `json:"path"`
	Method  string `json:"method"`
	Body    string `json:"body"`
	Confirm bool   `json:"confirm"`
}

type data struct {
	Method        string `json:"method"`
	Path          string `json:"path"`
	Status        int    `json:"status"`
	Bytes         int    `json:"bytes"`
	Truncated     bool   `json:"truncated"`
	Authenticated bool   `json:"authenticated"`
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
		return spi.Result{Hint: `参数不合法：期望 {"path": "repos/o/r/issues", "method": "GET"}`}, err
	}

	path := strings.TrimSpace(a.Path)
	path = strings.TrimLeft(path, "/")
	if path == "" {
		return spi.Result{Hint: `path 不能为空：给 API 路径，不带前导斜杠，如 "repos/o/r/issues"`},
			fmt.Errorf("path 为空")
	}
	if strings.Contains(path, "://") {
		return spi.Result{Hint: `path 只填 API 路径（如 repos/o/r/issues），不要给完整 URL——域名固定是 api.github.com`},
			fmt.Errorf("path 不应含协议头")
	}

	method := strings.ToUpper(strings.TrimSpace(a.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
	default:
		return spi.Result{Hint: `method 只支持 GET/POST/PATCH/PUT/DELETE；不填默认 GET`},
			fmt.Errorf("不支持的方法: %s", method)
	}
	isWrite := writeMethods[method]

	if isWrite && !a.Confirm {
		return spi.Result{Hint: fmt.Sprintf(`%s 会改远端状态，必须同时给 "confirm": true；不确定时先用 GET 看一眼`, method)},
			fmt.Errorf("写操作缺少 confirm")
	}

	body := strings.TrimSpace(a.Body)
	if body != "" && !json.Valid([]byte(body)) {
		return spi.Result{Hint: "body 不是合法 JSON：写请求体请传 JSON 字符串，如 {\"title\":\"x\"}（注意在参数里整体转义）"},
			fmt.Errorf("body 不是合法 JSON")
	}
	if body != "" && !isWrite {
		return spi.Result{Hint: fmt.Sprintf("body 只用于写方法（POST/PATCH/PUT/DELETE）；%s 不需要请求体", method)},
			fmt.Errorf("GET 不接受请求体")
	}

	token := resolveToken(ctx)

	// 公共读不需要令牌；写操作没有令牌就一定失败，早说清楚比让 GitHub 回 401 好。
	if isWrite && token == "" {
		return spi.Result{Hint: `写操作需要令牌：设置环境变量 GITHUB_TOKEN 或 GH_TOKEN，或先跑一次 gh auth login（工具会用 gh auth token）`},
			fmt.Errorf("缺少 GitHub 令牌")
	}

	url := apiBase + path
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return spi.Result{Hint: "请求构造失败：检查 path 是否含有非法字符"}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "dsh-go/"+versionish)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// 令牌只在这里出现一次，绝不进 Result / Summary / Hint。
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := t.client().Do(req)
	if err != nil {
		return spi.Result{Hint: "连不上 api.github.com：确认网络可达；代理环境要配 HTTPS_PROXY"},
			fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if readErr != nil {
		return spi.Result{Hint: "读到一半断了：重试一次，或者缩小查询范围（加 per_page / 收紧 path）"},
			fmt.Errorf("读正文失败: %w", readErr)
	}
	truncated := len(rawBody) > maxBytes
	if truncated {
		rawBody = rawBody[:maxBytes]
	}
	text := strings.ToValidUTF8(string(rawBody), "\uFFFD")
	authenticated := token != ""

	meta := data{
		Method:        method,
		Path:          path,
		Status:        resp.StatusCode,
		Bytes:         len(rawBody),
		Truncated:     truncated,
		Authenticated: authenticated,
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := text
		if rs := []rune(snippet); len(rs) > maxErrSnippet {
			snippet = string(rs[:maxErrSnippet]) + "…"
		}
		hint := "非 2xx：先看状态码与正文里的 message——404 是 repo/路径不对，401 是令牌无效，403 多为无权限或限流"
		switch {
		case resp.StatusCode == 401:
			hint = "401：令牌无效或已过期；重新设置 GITHUB_TOKEN / GH_TOKEN，或重新 gh auth login"
		case resp.StatusCode == 403:
			hint = "403：权限不足或被限流；看正文 message 与 X-RateLimit-* 头，必要时配令牌提高额度"
		case resp.StatusCode == 404:
			hint = "404：path 写错了（repos/<owner>/<repo>/…），或者该仓库/资源不存在或私有"
		case resp.StatusCode == 422:
			hint = "422：参数校验失败，看正文里的 errors 字段改 body"
		case resp.StatusCode >= 500:
			// 实测踩过：api.github.com 抽风时回 502 且**正文为空**，这时
			// "看正文里的 message" 是句空话，模型会连着重试两次再换工具。
			hint = "5xx：这是 GitHub 那边的问题（正文常常是空的），不是你写错了。别连着重试——隔一会儿再来，或用 gh CLI 试试是不是同一现象"
		}
		return spi.Result{
			Summary: fmt.Sprintf("HTTP %d %s /%s", resp.StatusCode, method, path),
			Text:    snippet,
			Data:    meta,
			Hint:    hint,
		}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	summary := fmt.Sprintf("HTTP %d %s /%s（%d 字节", resp.StatusCode, method, path, len(rawBody))
	if truncated {
		summary += "，已截断"
	}
	summary += "）"

	return spi.Result{
		Summary: summary,
		Text:    text,
		Data:    meta,
		Hint:    truncHint(truncated),
	}, nil
}

func truncHint(truncated bool) string {
	if truncated {
		return fmt.Sprintf("响应超过 %d 字节已截断：用 per_page/since/page 缩小范围，或换更具体的 path，别指望一次取全", maxBytes)
	}
	return ""
}

// resolveToken 按 GITHUB_TOKEN → GH_TOKEN → `gh auth token` 的顺序取令牌。
// 全部失败返回空串（调用方按"匿名公共读"处理）。令牌只在内存里过一手。
func resolveToken(ctx context.Context) string {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	// gh 可能没装、可能没登录：失败就当没有令牌，不打断请求。
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-github",
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
