package llm

// 真实模型发现（官方 `llm/discoverModels` 的端点探测部分）。
//
// # 为什么在 llm 包而不是 transport
//
// Transport 只做协议翻译，**不发网络请求**（R7 的精神：没人能从一个 HTTP
// 处理器里看出模型出口长什么样）。真正的探测走这里，由宿主注入。
//
// # 与 `(*Client).ListModels` 的关系
//
// `ListModels` 是本仓**既有 58 个方法**用的：它只要 `id`/`owned_by`。
// 官方 `discoverModels` 不一样——它要按**调用方给的一次性 baseURL + 一次性
// 凭据**去问一个**草稿里的**端点，还要把端点自愿公开的元数据（名称、上下文
// 窗口、输出上限、输入模态）原样带回去。两者共用"OpenAI 兼容 `GET /models`"
// 这同一个事实，但**不能**互相替代：
//
//   - 用 `ListModels` 实现发现会丢掉 name/contextWindow/maxTokens/inputModalities，
//     界面上就是"模型都在，但元数据全空"；
//   - 用发现实现 `ListModels` 会把一次性凭据带进既有方法的作用域。
//
// # 失败就是失败
//
// 和 `ListModels` 同一条规矩：**不**回空列表。空列表在界面上等于"这个端点一个
// 模型都没有"，而真实原因可能是端点没起、凭据过期、或根本不是 OpenAI 兼容
// 端点——三者该做的事完全不同。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DiscoveredModel 是一条**被端点公开**的模型元数据。
//
// 除 ID 外全部是"知道才有值"：官方 `ModelDiscovery` 的说明是
// "absent means unknown"，所以零值在这里表示**不知道**，不是一个默认值。
type DiscoveredModel struct {
	ID              string
	Name            string
	ContextWindow   int64
	MaxTokens       int64
	InputModalities []string
}

// discoverTimeout 是单次模型发现的墙钟上限。
//
// 调用方（Transport）传进来的是 HTTP 请求的 ctx，它**没有 deadline**：
// 一个挂住的端点会把那个 HTTP 请求永久占住。这里兜一个上限。
const discoverTimeout = 15 * time.Second

// discoverResponseBodyLimit 是失败时读回的错误正文上限（只要片段，不要整页）。
const discoverResponseBodyLimit = 512

// discoverDefaultUserAgent 与 `(*Client).post` 里的兜底逐字一致。
//
// 为什么不抽成一个常量给两边用：`client.go` 正在被并行写者改（`M` 状态），
// 为了一个字符串去动它会把两边的改动搅在一起。等那条线收口后再合并成一处。
const discoverDefaultUserAgent = "dshd (dsh-go)"

// discoverEntry 是 `/models` 的一条原始记录。
//
// 字段名按"各家网关实际怎么写"收：官方 OpenAI 形状只有 id/object/created/
// owned_by，元数据是**扩展**，而且不同网关拼法不同（`context_window` vs
// `context_length` vs OpenRouter 的 `architecture.context_length`）。这里全部
// 收进来，取值时按**优先级**挑第一个真值——不猜、不合成。
type discoverEntry struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	ContextWindow    float64 `json:"context_window,omitempty"`
	ContextLength    float64 `json:"context_length,omitempty"`
	MaxContextTokens float64 `json:"max_context_tokens,omitempty"`

	MaxTokens       float64 `json:"max_tokens,omitempty"`
	MaxOutputTokens float64 `json:"max_output_tokens,omitempty"`

	InputModalities []string `json:"input_modalities,omitempty"`
	Modalities      []string `json:"modalities,omitempty"`

	Architecture *struct {
		InputModalities []string `json:"input_modalities,omitempty"`
		ContextLength   float64  `json:"context_length,omitempty"`
	} `json:"architecture,omitempty"`

	TopProvider *struct {
		MaxCompletionTokens float64 `json:"max_completion_tokens,omitempty"`
	} `json:"top_provider,omitempty"`
}

// DiscoverModels 问**这条 client 指向的**端点有哪些模型。
//
// baseURL 可以写成四种形状，都会被规范化：
//
//	http://127.0.0.1:3425/v1        → http://127.0.0.1:3425/v1/models
//	http://127.0.0.1:3425/v1/       → 同上
//	http://127.0.0.1:3425/v1/models → 原样
//	http://127.0.0.1:3425           → http://127.0.0.1:3425/models
//
// 凭据是**一次性**的（官方说明："the harness never stores it"）：它只出现在
// 这一次请求头里，不进任何缓存、也不进错误消息（`RedactCredential` 兜底）。
func (c *Client) DiscoverModels(ctx context.Context) ([]DiscoveredModel, error) {
	if c == nil {
		return nil, fmt.Errorf("模型发现需要一条 client（nil）")
	}
	endpoint, err := discoverEndpoint(c.BaseURL)
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造模型发现请求失败: %w", err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = discoverDefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("探测 %s 失败: %s", endpoint, RedactCredential(err.Error(), c.APIKey))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, discoverResponseBodyLimit))
		return nil, fmt.Errorf("%s 回 %d：%s",
			endpoint, resp.StatusCode, RedactCredential(string(body), c.APIKey))
	}

	// 用指针接 `data`，把两种"空"分开（和 ListModels 同一条理由）：
	//   data: []    = 端点好好的，一个模型都没挂
	//   data: null / 没有 data = 响应形状不对（打到了别的服务）
	var doc struct {
		Data *[]discoverEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s 的响应不是合法 JSON: %w", endpoint, err)
	}
	if doc.Data == nil {
		return nil, fmt.Errorf("%s 的响应里没有 data 数组（这不是「没有模型」，"+
			"是响应形状不对）：确认它是 OpenAI 兼容端点（GET /models）", endpoint)
	}

	out := make([]DiscoveredModel, 0, len(*doc.Data))
	for i, entry := range *doc.Data {
		if strings.TrimSpace(entry.ID) == "" {
			// 官方结果里 id 是**必填**：拿不到 id 就没法代表这一条。
			// 静默丢掉会让用户以为"这个模型不存在"，所以响亮报错。
			return nil, fmt.Errorf("%s 的第 %d 条模型没有 id（官方契约 id 必填）", endpoint, i)
		}
		out = append(out, DiscoveredModel{
			ID:              entry.ID,
			Name:            entry.Name,
			ContextWindow:   firstPositive(entry.ContextWindow, entry.ContextLength, entry.MaxContextTokens, architectureContextLength(entry)),
			MaxTokens:       firstPositive(entry.MaxTokens, entry.MaxOutputTokens, topProviderMaxCompletionTokens(entry)),
			InputModalities: normalizeInputModalities(entry.InputModalities, entry.Modalities, architectureInputModalities(entry)),
		})
	}
	return out, nil
}

// discoverEndpoint 把调用方给的 baseURL 规范化成 `/models` 端点。
func discoverEndpoint(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("baseURL 是空串")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("baseURL 不是合法 URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("baseURL 必须是 http/https，收到 %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("baseURL 缺 host")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(path, "/models") {
		path += "/models"
	}
	parsed.Path = path
	// 查询串与片段对 `GET /models` 没有意义，带上只会让不同调用方拿到不同缓存。
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// firstPositive 取第一个 > 0 的值；全都没有就是 0（= 不知道）。
func firstPositive(values ...float64) int64 {
	for _, value := range values {
		if value > 0 {
			return int64(value)
		}
	}
	return 0
}

func architectureContextLength(entry discoverEntry) float64 {
	if entry.Architecture == nil {
		return 0
	}
	return entry.Architecture.ContextLength
}

func architectureInputModalities(entry discoverEntry) []string {
	if entry.Architecture == nil {
		return nil
	}
	return entry.Architecture.InputModalities
}

func topProviderMaxCompletionTokens(entry discoverEntry) float64 {
	if entry.TopProvider == nil {
		return 0
	}
	return entry.TopProvider.MaxCompletionTokens
}

// normalizeInputModalities 把各家的模态写法收敛到官方枚举 `text` / `image`。
//
// 官方 schema 只认这两个值。别的（audio/video/document…）**没法表达**，
// 只能丢掉——但绝不能编成 text（那会让界面以为端点接受纯文本输入）。
// 全都被丢掉时返回 nil（= 不知道），不是空数组。
func normalizeInputModalities(groups ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, group := range groups {
		for _, raw := range group {
			switch strings.ToLower(strings.TrimSpace(raw)) {
			case "text":
				if !seen["text"] {
					seen["text"] = true
					out = append(out, "text")
				}
			case "image":
				if !seen["image"] {
					seen["image"] = true
					out = append(out, "image")
				}
			}
		}
	}
	return out
}
