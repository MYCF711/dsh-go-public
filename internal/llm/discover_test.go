package llm

// 模型发现（官方 `llm/discoverModels` 的探测部分）的契约测试。
//
// 全部打真实 httptest 端点：不 mock HTTP 客户端，也不手写响应字符串。
// 覆盖：URL 规范化、元数据映射（含各家网关的别名）、失败响亮、一次性凭据
// 不外泄、模态枚举收敛。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// discoverModelsServer 造一个假的 OpenAI 兼容端点，并记录被请求的路径。
type discoverModelsServer struct {
	*httptest.Server
	path   string
	auth   string
	ua     string
	status int
	body   string
}

func newDiscoverModelsServer(t *testing.T, status int, body string) *discoverModelsServer {
	t.Helper()
	fake := &discoverModelsServer{status: status, body: body}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.path = r.URL.Path
		fake.auth = r.Header.Get("Authorization")
		fake.ua = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fake.status)
		_, _ = w.Write([]byte(fake.body))
	}))
	t.Cleanup(fake.Close)
	return fake
}

// TestDiscoverModelsMapsOfficialFields：端点公开的元数据必须原样落到官方字段上。
func TestDiscoverModelsMapsOfficialFields(t *testing.T) {
	fake := newDiscoverModelsServer(t, http.StatusOK, `{
      "object": "list",
      "data": [
        {"id": "deepseek-chat", "name": "DeepSeek Chat",
         "context_window": 131072, "max_tokens": 8192,
         "input_modalities": ["text", "image"]},
        {"id": "second-model", "context_length": 65536, "max_output_tokens": 4096}
      ]
    }`)
	models, err := New(fake.URL+"/v1", "sk-one-shot").DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("条数 %d，期望 2", len(models))
	}
	first := models[0]
	if first.ID != "deepseek-chat" || first.Name != "DeepSeek Chat" {
		t.Fatalf("id/name 映射错：%+v", first)
	}
	if first.ContextWindow != 131072 || first.MaxTokens != 8192 {
		t.Fatalf("contextWindow/maxTokens 映射错：%+v", first)
	}
	if len(first.InputModalities) != 2 || first.InputModalities[0] != "text" || first.InputModalities[1] != "image" {
		t.Fatalf("inputModalities 映射错：%+v", first.InputModalities)
	}
	// 别名：context_length / max_output_tokens 也要认。
	second := models[1]
	if second.ContextWindow != 65536 || second.MaxTokens != 4096 {
		t.Fatalf("别名没认出来：%+v", second)
	}
	// 端点没给的可选字段必须是零值（= 不知道），不能编。
	if second.Name != "" || second.InputModalities != nil {
		t.Fatalf("未知字段被编造：%+v", second)
	}
}

// TestDiscoverModelsOpenRouterArchitecture：OpenRouter 把元数据放在嵌套的
// architecture/top_provider 里，也要认。
func TestDiscoverModelsOpenRouterArchitecture(t *testing.T) {
	fake := newDiscoverModelsServer(t, http.StatusOK, `{"data":[{
      "id": "vendor/model",
      "architecture": {"input_modalities": ["text", "image", "audio"], "context_length": 200000},
      "top_provider": {"max_completion_tokens": 64000}
    }]}`)
	models, err := New(fake.URL+"/v1", "").DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if models[0].ContextWindow != 200000 || models[0].MaxTokens != 64000 {
		t.Fatalf("嵌套元数据没认出来：%+v", models[0])
	}
	// audio 不在官方枚举里：只能丢掉，绝不能编成 text。
	if got := strings.Join(models[0].InputModalities, ","); got != "text,image" {
		t.Fatalf("模态收敛错：%q（期望 text,image）", got)
	}
}

// TestDiscoverModelsEndpointNormalization 钉住 baseURL 的四种写法。
func TestDiscoverModelsEndpointNormalization(t *testing.T) {
	cases := []struct {
		base     string
		wantPath string
	}{
		{"http://HOST/v1", "/v1/models"},
		{"http://HOST/v1/", "/v1/models"},
		{"http://HOST/v1/models", "/v1/models"},
		{"http://HOST", "/models"},
		{"http://HOST/", "/models"},
	}
	for _, tc := range cases {
		t.Run(tc.base, func(t *testing.T) {
			fake := newDiscoverModelsServer(t, http.StatusOK, `{"data":[]}`)
			// 用真端点的 host 替换占位符，保持路径形状。
			parsed, err := url.Parse(fake.URL)
			if err != nil {
				t.Fatalf("解析测试端点: %v", err)
			}
			base := strings.Replace(tc.base, "http://HOST", "http://"+parsed.Host, 1)
			if _, err := New(base, "").DiscoverModels(context.Background()); err != nil {
				t.Fatalf("DiscoverModels(%q): %v", base, err)
			}
			if fake.path != tc.wantPath {
				t.Fatalf("请求路径 %q，期望 %q", fake.path, tc.wantPath)
			}
		})
	}
}

// TestDiscoverModelsRejectsBadBaseURL：不是 http(s) 的 baseURL 一律拒，
// 且**不**发请求（file:// 探出去等于读本地文件）。
func TestDiscoverModelsRejectsBadBaseURL(t *testing.T) {
	for _, base := range []string{"", "   ", "file:///etc/passwd", "ftp://h/x", "not a url", "http://"} {
		t.Run(base, func(t *testing.T) {
			if _, err := New(base, "").DiscoverModels(context.Background()); err == nil {
				t.Fatalf("baseURL %q 被接受了", base)
			}
		})
	}
}

// TestDiscoverModelsNon200IsLoud：非 200 是错误，不是空列表。
func TestDiscoverModelsNon200IsLoud(t *testing.T) {
	fake := newDiscoverModelsServer(t, http.StatusUnauthorized, `{"error":"bad key"}`)
	_, err := New(fake.URL+"/v1", "sk-secret-value").DiscoverModels(context.Background())
	if err == nil {
		t.Fatalf("401 被当成成功")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误里没有状态码：%v", err)
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("一次性凭据泄漏进错误消息：%v", err)
	}
}

// TestDiscoverModelsMissingDataIsLoud：`data` 不存在（打到了别的服务）
// 是错误，不是"没有模型"。
func TestDiscoverModelsMissingDataIsLoud(t *testing.T) {
	fake := newDiscoverModelsServer(t, http.StatusOK, `{"object":"list"}`)
	_, err := New(fake.URL+"/v1", "").DiscoverModels(context.Background())
	if err == nil {
		t.Fatalf("缺 data 被当成空列表")
	}
	if !strings.Contains(err.Error(), "data") {
		t.Fatalf("错误没说清缺 data：%v", err)
	}
}

// TestDiscoverModelsEmptyListIsEmptySlice：真·空列表要能区分出来。
func TestDiscoverModelsEmptyListIsEmptySlice(t *testing.T) {
	fake := newDiscoverModelsServer(t, http.StatusOK, `{"data":[]}`)
	models, err := New(fake.URL+"/v1", "").DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if models == nil || len(models) != 0 {
		t.Fatalf("空列表应是长度为 0 的非 nil 切片，实际 %#v", models)
	}
}

// TestDiscoverModelsEntryWithoutIDIsLoud：官方 id 必填，缺了不能静默丢掉。
func TestDiscoverModelsEntryWithoutIDIsLoud(t *testing.T) {
	fake := newDiscoverModelsServer(t, http.StatusOK, `{"data":[{"id":""},{"id":"ok"}]}`)
	_, err := New(fake.URL+"/v1", "").DiscoverModels(context.Background())
	if err == nil {
		t.Fatalf("缺 id 的条目被静默接受")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Fatalf("错误没说清缺 id：%v", err)
	}
}

// TestDiscoverModelsSendsOneShotCredential：一次性凭据只出现在这一次请求头，
// 且 UA 必须署自己的名。
func TestDiscoverModelsSendsOneShotCredential(t *testing.T) {
	fake := newDiscoverModelsServer(t, http.StatusOK, `{"data":[]}`)
	if _, err := New(fake.URL+"/v1", "sk-one-shot").DiscoverModels(context.Background()); err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if fake.auth != "Bearer sk-one-shot" {
		t.Fatalf("Authorization=%q，期望 Bearer sk-one-shot", fake.auth)
	}
	if fake.ua == "" || fake.ua == "Go-http-client/1.1" {
		t.Fatalf("UA 没署自己的名：%q", fake.ua)
	}
}

// TestDiscoverModelsNilClientIsSafe：nil client 不能 panic。
func TestDiscoverModelsNilClientIsSafe(t *testing.T) {
	var client *Client
	if _, err := client.DiscoverModels(context.Background()); err == nil {
		t.Fatalf("nil client 应该是错误")
	}
}

// TestDiscoverModelsRespectsContextCancel：调用方取消必须打断探测。
func TestDiscoverModelsRespectsContextCancel(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() {
		close(block)
		srv.Close()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(srv.URL+"/v1", "").DiscoverModels(ctx); err == nil {
		t.Fatalf("已取消的 ctx 没有打断探测")
	}
}

// TestDiscoverEndpointShape 是纯函数级断言：规范化结果可被再次解析。
func TestDiscoverEndpointShape(t *testing.T) {
	got, err := discoverEndpoint("http://h:1/v1/?x=1#frag")
	if err != nil {
		t.Fatalf("discoverEndpoint: %v", err)
	}
	if got != "http://h:1/v1/models" {
		t.Fatalf("规范化结果 %q，期望 http://h:1/v1/models", got)
	}
	if _, err := url.Parse(got); err != nil {
		t.Fatalf("规范化结果不是合法 URL: %v", err)
	}
}
