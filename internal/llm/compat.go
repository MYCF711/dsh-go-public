package llm

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 这一层是 W184 的「严格兼容 + 可选字段协商」。
//
// 背景：不少 OpenAI 兼容网关按 OpenAPI schema 严格校验请求体，多一个字段、少一个
// 必须字段、拼写换过名，都直接 400/422 拒掉。四个标准字段（model / messages /
// tools / stream）在哪儿都必须能过；真正各家不同的是可选字段：
//
//   - max_tokens                  : OpenAI 现行文档已由 max_completion_tokens 取代，
//                                   老端点/vLLM 只认 max_tokens；
//   - stream_options.include_usage: OpenAI 认，但一些自建网关/代理当成未知参数；
//   - reasoning_effort            : 只有推理模型/部分渠道认，非推理端点上常被拒。
//
// 策略（三层，从严到宽）：
//
//  1. 默认请求**逐字节保持原样**（禁止无谓漂移）：没有任何能力信息时，发出去的
//     JSON 与历史实现完全一致，见 TestDefaultWireBodyIsByteIdentical。
//  2. 能力已知时直接走兼容形状：官方端点（api.openai.com）用 max_completion_tokens；
//     以及本进程里曾经被某个严格网关实际接受过的形状（按 base+model 记忆 5 分钟）。
//  3. 仍然被拒时，只在 400/422 且响应体**明确**指向某个我们真的发出去的可选字段时，
//     做一次最小回退：只改那一个字段（去掉，或换成 OpenAI 官方拼写）。401/403/404/
//     429/5xx 与普通参数错误（值越界、messages 结构不对）一律原样失败，不重试。
//
// 每个可选字段只能单向收缩一次，因此最多 4 次请求必然收敛（见 maxWireAttempts）。

const (
	maxTokensFieldLegacy     = "max_tokens"
	maxTokensFieldCompletion = "max_completion_tokens"

	// maxWireAttempts 是一条请求最多发几次：初始 1 次 + 最多 3 次可选字段回退。
	//
	// 三个可选字段（stream_options / reasoning_effort / max_tokens 拼写）各自
	// 只允许收缩一次，且已试过的形状不再重试（见 attempted），所以这里是一个
	// 上界而不是"重试到成功"：不存在死循环。
	maxWireAttempts = 4

	// maxErrorBodyBytes 是错误响应体的读取上限：够解析出"哪个参数不被支持"，
	// 又不至于把几 MB 的 HTML 错误页读进内存。
	maxErrorBodyBytes = 8 << 10

	// maxErrorSnippetRunes 是交给 GatewayError.Body（进而可能进日志/Transport）
	// 的诊断片段上限。留原始措辞，因为下一个动作只能从这句话里看出来。
	maxErrorSnippetRunes = 600
)

// wireProfile 是一条请求**实际发出**的可选字段形状。
//
// 零值不可用：用 defaultWireProfile()。三个字段都可比较，可以直接当 map key。
type wireProfile struct {
	maxTokensField       string // max_tokens（默认）或 max_completion_tokens
	includeStreamOptions bool   // 是否发 stream_options.include_usage
	includeReasoning     bool   // 是否发 reasoning_effort
}

// defaultWireProfile 是历史行为：与本次改动之前的请求体逐字节一致。
func defaultWireProfile() wireProfile {
	return wireProfile{
		maxTokensField:       maxTokensFieldLegacy,
		includeStreamOptions: true,
		includeReasoning:     true,
	}
}

// wireSent 记录这条请求**真的**发出去的可选字段。
//
// 协商只允许动真实发出的字段：网关抱怨一个我们没发的字段（或者是站点的通用措辞），
// 说明问题不在请求形状上，重试只会浪费一次请求。
type wireSent struct {
	maxTokens     bool
	streamOptions bool
	reasoning     bool
}

func sentFields(req Request, profile wireProfile) wireSent {
	return wireSent{
		maxTokens:     req.MaxTokens > 0,
		streamOptions: profile.includeStreamOptions && req.StreamOptions != nil,
		reasoning:     profile.includeReasoning && req.Reasoning != "",
	}
}

// wireRequest 是真正序列化的请求体。字段顺序与历史 Request 结构体一致，
// 多出来的 max_completion_tokens 只在非默认形状下出现且 omitempty，所以默认
// 形状的输出与 json.Marshal(Request) 逐字节相同。
type wireRequest struct {
	Model               string         `json:"model"`
	Messages            []Message      `json:"messages"`
	Tools               []ToolDef      `json:"tools,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       *StreamOptions `json:"stream_options,omitempty"`
	MaxTokens           int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	Reasoning           string         `json:"reasoning_effort,omitempty"`
}

// marshalWireRequest 按 profile 拼请求体。model/messages/tools/stream 是标准字段，
// 任何 profile 下都原样保留。
func marshalWireRequest(req Request, profile wireProfile) ([]byte, error) {
	wire := wireRequest{
		Model:    req.Model,
		Messages: req.Messages,
		Tools:    req.Tools,
		Stream:   req.Stream,
	}
	if profile.includeStreamOptions {
		wire.StreamOptions = req.StreamOptions
	}
	if req.MaxTokens > 0 {
		if profile.maxTokensField == maxTokensFieldCompletion {
			wire.MaxCompletionTokens = req.MaxTokens
		} else {
			wire.MaxTokens = req.MaxTokens
		}
	}
	if profile.includeReasoning {
		wire.Reasoning = req.Reasoning
	}
	return json.Marshal(wire)
}

// fallbackMarkers 是"这个**参数**有问题"的措辞标记：不认识、不支持、不允许、
// 必须改名。刻意只收 schema 级词组，**不含** "invalid" / "too large" /
// "out of range" / 单独的 "unknown"：值越界（max_tokens too large）或
// "Unknown error" 这种泛化文案不是形状问题，那种 400 必须原样失败。
var fallbackMarkers = []string{
	"unknown parameter", "unknown_parameter", "unknown field", "unknown_field",
	"unknown argument", "unknown_argument", "unknown property", "unknown_property",
	"unrecognized request argument", "unrecognized parameter", "unrecognized_parameter",
	"unrecognized field", "unrecognized_field", "unrecognized argument",
	"unrecognised parameter", "unrecognised field",
	"unsupported parameter", "unsupported_parameter", "unsupported field",
	"not supported", "not support", "does not support", "doesn't support",
	"isn't supported", "not allowed", "not permitted",
	"extra field", "extra_forbidden", "additional propert",
	"no longer supported", "must use", "renamed", "not accepted",
}

// otherSpelling 是同义但官方已改名的字段（max_tokens ↔ max_completion_tokens）。
// 只有这种"有官方替代拼写"的字段才允许改名回退；stream_options 与
// reasoning_effort 没有替代字段，端点不认就只能去掉。
func otherSpelling(field string) string {
	switch field {
	case maxTokensFieldLegacy:
		return maxTokensFieldCompletion
	case maxTokensFieldCompletion:
		return maxTokensFieldLegacy
	default:
		return ""
	}
}

// blamesField 报告响应体是否**明确指向**这个字段：字段名出现，并且
// "不认识/不支持/必须改名"的措辞也在同一段正文里。rename 的理由还有第二种
// 写法——"请改用 <另一个拼写> instead"，所以替代拼写+instead 也算。
//
// 只看"全文字段名 + 全文标记"不够：一张 HTML 错误页里同时出现 "unknown" 和
// "max_tokens" 不该触发回退，那会变成盲目重试。
func blamesField(text, field string) bool {
	if !strings.Contains(text, field) {
		return false
	}
	if containsAny(text, fallbackMarkers) {
		return true
	}
	if replacement := otherSpelling(field); replacement != "" {
		return strings.Contains(text, replacement) && strings.Contains(text, "instead")
	}
	return false
}

// negotiateWire 判断这次失败能不能靠改一个可选字段解决。
//
// 只有同时满足三条才回退，否则返回 ok=false、调用方原样报错：
//  1. 状态码是 400 或 422（认证、路由、限流、上游故障都不是形状问题）；
//  2. 响应体里有"不认识/不支持/要改名"的措辞；
//  3. 被点名的字段确实是我们这条请求发出去的。
//
// 每次只动一个字段，返回的 change 是给人看的说明，会进最终错误体。
func negotiateWire(profile wireProfile, sent wireSent, status int, body string) (wireProfile, string, bool) {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return wireProfile{}, "", false
	}
	text := strings.ToLower(body)
	switch {
	case sent.streamOptions && (blamesField(text, "stream_options") || blamesField(text, "include_usage")):
		next := profile
		next.includeStreamOptions = false
		return next, "去掉 stream_options（端点不认这个可选字段；本轮 usage 可能不再上报）", true
	case sent.reasoning && (blamesField(text, "reasoning_effort") || blamesField(text, "reasoning effort")):
		next := profile
		next.includeReasoning = false
		return next, "去掉 reasoning_effort（端点不认这个可选字段；本轮没有推理强度）", true
	case sent.maxTokens && profile.maxTokensField == maxTokensFieldLegacy && blamesField(text, maxTokensFieldLegacy):
		next := profile
		next.maxTokensField = maxTokensFieldCompletion
		return next, "max_tokens → max_completion_tokens（改用 OpenAI 官方现行拼写）", true
	case sent.maxTokens && profile.maxTokensField == maxTokensFieldCompletion && blamesField(text, maxTokensFieldCompletion):
		next := profile
		next.maxTokensField = maxTokensFieldLegacy
		return next, "max_completion_tokens → max_tokens（该端点只认旧拼写）", true
	}
	return wireProfile{}, "", false
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// knownEndpointCompat 是"能力已知"的静态表：命中就不必先撞一次 400。
//
// 只收官方文档写明的差异，且只影响可选字段；未命中的端点一律返回默认形状，
// 保证默认请求逐字节不变（自有网关 127.0.0.1:3425 就走这条路）。
func knownEndpointCompat(baseURL string) (wireProfile, bool) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return wireProfile{}, false
	}
	if !strings.EqualFold(parsed.Hostname(), "api.openai.com") {
		return wireProfile{}, false
	}
	// OpenAI 官方的 chat/completions 已用 max_completion_tokens 取代 max_tokens
	// （推理模型只认前者），stream_options 与 reasoning_effort 都支持。
	profile := defaultWireProfile()
	profile.maxTokensField = maxTokensFieldCompletion
	return profile, true
}

// capabilityTTL 是被严格网关接受过的形状在进程里的记忆时长。
//
// 不设永久记忆：网关/渠道换配置后最多 5 分钟就回到默认形状重新试探，
// 不会因为一次 400 就永久降级（比如 usage 一直报不出来）。
const capabilityTTL = 5 * time.Minute

type capabilityEntry struct {
	profile wireProfile
	expires time.Time
}

// capabilityCache 按 (baseURL, model) 记住被真正接受过的形状。
//
// 为什么是进程级而不是 Client 级：宿主大量调用点都是
// `llm.New(base, key).Stream(...)`（每轮一个新 Client），只挂在 Client 上的话
// 严格网关每轮都要先吃一个 400。键里带 model，避免同一端点上的两个模型互相污染。
var capabilityCache = struct {
	mu      sync.Mutex
	entries map[string]capabilityEntry
}{entries: map[string]capabilityEntry{}}

func capabilityKey(baseURL, model string) string {
	return strings.TrimSuffix(baseURL, "/") + "|" + model
}

func cachedCapability(baseURL, model string) (wireProfile, bool) {
	key := capabilityKey(baseURL, model)
	capabilityCache.mu.Lock()
	defer capabilityCache.mu.Unlock()
	entry, ok := capabilityCache.entries[key]
	if !ok {
		return wireProfile{}, false
	}
	if time.Now().After(entry.expires) {
		delete(capabilityCache.entries, key)
		return wireProfile{}, false
	}
	return entry.profile, true
}

func rememberCapability(baseURL, model string, profile wireProfile) {
	key := capabilityKey(baseURL, model)
	now := time.Now()
	capabilityCache.mu.Lock()
	defer capabilityCache.mu.Unlock()
	// 顺手清过期项：条目数只跟端点×模型数有关，一次遍历就很便宜。
	for candidate, entry := range capabilityCache.entries {
		if now.After(entry.expires) {
			delete(capabilityCache.entries, candidate)
		}
	}
	capabilityCache.entries[key] = capabilityEntry{profile: profile, expires: now.Add(capabilityTTL)}
}

// resetCapabilityCache 只给测试用：避免用例之间的记忆互相影响。
func resetCapabilityCache() {
	capabilityCache.mu.Lock()
	defer capabilityCache.mu.Unlock()
	capabilityCache.entries = map[string]capabilityEntry{}
}

// requestProfile 是这条请求的起始形状：先看学过什么，再看官方静态表，最后默认。
func (c *Client) requestProfile(model string) wireProfile {
	if profile, ok := cachedCapability(c.BaseURL, model); ok {
		return profile
	}
	if profile, ok := knownEndpointCompat(c.BaseURL); ok {
		return profile
	}
	return defaultWireProfile()
}

// errorSnippet 把网关错误体变成可以交给客户端/日志的片段。
//
// 顺序是「先清洗 credential，再按字符边界截断」：反过来的话，正好被截断在中间的
// credential 会漏出去半截。原始措辞要留着——"Unrecognized request argument
// supplied: stream_options" 这句话本身就是下一个动作。
func errorSnippet(raw []byte, credentials ...string) string {
	text := RedactCredential(strings.TrimSpace(string(raw)), credentials...)
	runes := []rune(text)
	if len(runes) > maxErrorSnippetRunes {
		return string(runes[:maxErrorSnippetRunes]) + "…"
	}
	return text
}
