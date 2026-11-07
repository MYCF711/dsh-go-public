package transport

// 官方桌面端 media / market 侧 21 个方法的契约测试。
//
// 这些测试只打本包的 `ServeOfficialMediaMarketAPI`（Lead 接线后由 /api 路由调它），
// 因此不依赖 server.go 的分发表——**路由接线是 Lead 的验收项**，测试里用
// `IsOfficialMediaMarketMethod` + 逐条 POST 钉住"方法名集合与方法级结论"，避免
// 新增方法从路由漏下去、也避免某个方法悄悄从"未实现"变成"假成功"。
//
// 覆盖的判据（对应 RESULT 的三列表）：
//   - `speech.*` 4 个：501 且**不含任何转写文本**（不许伪造 ASR 结果）；
//   - `cotTranslation.translate`：真的把文本送出去，返回模型真实输出（用本地假
//     SSE 网关验证管线；空输出/网关失败一律报错，不拿原文顶替）；
//   - `officeToPdf.render`：501 并点名 LibreOffice 依赖；
//   - `pluginMarket.*` 3 个 + `pluginRecommendations.describe`：501 并点名缺的索引
//     服务；参数严格性（缺必填/未知字段/范围）回 400；
//   - `dynamicCordisRunner.*` 11 个：501 且消息里写明 R6（明确不做，不是欠账）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// officialMediaMarketMethodNames 是官方清单里的 21 个名字（逐字来自
// docs/evidence/official-desktop-api-methods-0.2.2-rc.1.txt）。
var officialMediaMarketMethodNames = []string{
	"cotTranslation.translate",
	"dynamicCordisRunner.getClientCode",
	"dynamicCordisRunner.invoke",
	"dynamicCordisRunner.reportClientGuardFailure",
	"dynamicCordisRunner.reportRenderFailure",
	"dynamicCordisRunner.resolveInspectQuery",
	"dynamicCordisRunner.resolveRequestRun",
	"dynamicCordisRunner.runHostHalf",
	"dynamicCordisRunner.settleUserRun",
	"dynamicCordisRunner.stopFromPanel",
	"dynamicCordisRunner.syncInspectManifest",
	"dynamicCordisRunner.undefineFromPanel",
	"officeToPdf.render",
	"pluginMarket.readEntryDetail",
	"pluginMarket.search",
	"pluginMarket.setIndexes",
	"pluginRecommendations.describe",
	"speech.cancelPreparation",
	"speech.configure",
	"speech.prepare",
	"speech.transcribe",
}

// mmTestServer 造一个只用于本文件的 Server（不启动监听，直接调 handler）。
func mmTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mmServe 打一次本组方法，返回状态码与原始正文。
func mmServe(t *testing.T, s *Server, method, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeOfficialMediaMarketAPI(rec, req)
	return rec.Code, rec.Body.String()
}

type mmErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Hint      string `json:"hint"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

// mmErrorOf 解出四字段错误体，并断言"只有 error、没有 result"。
func mmErrorOf(t *testing.T, body string) mmErrorBody {
	t.Helper()
	var out mmErrorBody
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("错误体不是 JSON: %v（正文=%s）", err, body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("错误体不是 JSON 对象: %v", err)
	}
	if _, hasResult := raw["result"]; hasResult {
		t.Fatalf("错误响应里不许出现 result（会变成假成功）：%s", body)
	}
	if out.Error.Code == "" || out.Error.Message == "" || out.Error.Hint == "" {
		t.Fatalf("四字段错误体缺字段：%s", body)
	}
	return out
}

// ---------------------------------------------------------------------------
// 方法集合：21 个一个不多一个不少
// ---------------------------------------------------------------------------

func TestOfficialMarketMethodSetCoversAll21(t *testing.T) {
	got := OfficialMediaMarketMethods()
	if len(got) != 21 {
		t.Fatalf("方法数必须是 21，拿到 %d", len(got))
	}
	gotNames := make([]string, 0, len(got))
	for _, m := range got {
		gotNames = append(gotNames, string(m))
		if !IsOfficialMediaMarketMethod(string(m)) {
			t.Fatalf("%s 不在 IsOfficialMediaMarketMethod 里", m)
		}
	}
	want := append([]string(nil), officialMediaMarketMethodNames...)
	sort.Strings(gotNames)
	sort.Strings(want)
	if strings.Join(gotNames, ",") != strings.Join(want, ",") {
		t.Fatalf("方法集与官方清单不一致：\n got=%v\nwant=%v", gotNames, want)
	}
	// 重复断言：任何一个都不能被算两次。
	seen := map[string]bool{}
	for _, name := range gotNames {
		if seen[name] {
			t.Fatalf("方法重复：%s", name)
		}
		seen[name] = true
	}
	// 不属本组的方法必须 false（否则会抢走别人的路由）。
	for _, other := range []string{"pluginManager.inspect", "schedule.list", "speech", "speech.", "pluginMarket"} {
		if IsOfficialMediaMarketMethod(other) {
			t.Fatalf("%q 不该被本组认领", other)
		}
	}
	// 接错路由要当场 404，不能给"空成功"。
	code, body := mmServe(t, mmTestServer(t), "pluginManager.inspect", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("非本组方法要 404，拿到 %d（%s）", code, body)
	}
}

// ---------------------------------------------------------------------------
// speech.*：未实现，且绝不伪造转写
// ---------------------------------------------------------------------------

func TestOfficialSpeechPrepareTranscribeConfigureCancelUnimplemented(t *testing.T) {
	s := mmTestServer(t)

	cases := []struct {
		method string
		body   string
		want   string // 消息里必须出现的片段
	}{
		{"speech.prepare", `{"providerId":"sensevoice"}`, "speech.prepare"},
		{"speech.prepare", `{"providerId":"sensevoice","options":{"downloadSource":"huggingface"}}`, "downloadSource"},
		{"speech.transcribe", `{"request":{"audioBase64":"AAAAAAAA"}}`, "speech.transcribe"},
		{"speech.transcribe", `{"audioBase64":"AAAAAAAA","language":"zh-CN"}`, "speech.transcribe"},
		{"speech.configure", `{"patch":{"language":"zh-CN"}}`, "speech.configure"},
		{"speech.configure", `{}`, "speech.configure"},
		{"speech.cancelPreparation", `{"providerId":"sensevoice"}`, "speech.cancelPreparation"},
	}
	for _, tc := range cases {
		code, body := mmServe(t, s, tc.method, tc.body)
		if code != http.StatusNotImplemented {
			t.Fatalf("%s%s 要 501，拿到 %d（%s）", tc.method, tc.body, code, body)
		}
		e := mmErrorOf(t, body)
		if e.Error.Code != "not_implemented" {
			t.Fatalf("%s 的 code 要 not_implemented，拿到 %q", tc.method, e.Error.Code)
		}
		if e.Error.Retryable {
			t.Fatalf("%s 未实现不该标成可重试", tc.method)
		}
		if !strings.Contains(e.Error.Message, tc.want) {
			t.Fatalf("%s 的消息没说清缺什么（缺 %q）：%s", tc.method, tc.want, e.Error.Message)
		}
	}
}

// 转写**绝不许**出现任何 "text" 字段：编一段文字冒充识别结果是最坏的失败方式。
func TestOfficialSpeechTranscribeNeverFabricates(t *testing.T) {
	s := mmTestServer(t)

	code, body := mmServe(t, s, "speech.transcribe", `{"request":{"audioBase64":"QUJDREVG"}}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("要 501，拿到 %d", code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("正文不是 JSON: %v", err)
	}
	if _, ok := raw["text"]; ok {
		t.Fatalf("转写未实现却回了 text 字段（伪造）：%s", body)
	}
	if _, ok := raw["audioSeconds"]; ok {
		t.Fatalf("转写未实现却回了 audioSeconds（伪造）：%s", body)
	}
	if _, ok := raw["inferenceSeconds"]; ok {
		t.Fatalf("转写未实现却回了 inferenceSeconds（伪造）：%s", body)
	}

	// 参数严格性：缺音频 / 未知字段 / providerId 类型错 → 400（而不是 501）。
	for _, bad := range []struct {
		body string
		why  string
	}{
		{`{}`, "缺 audioBase64"},
		{`{"request":{}}`, "缺 audioBase64"},
		{`{"request":{"audioBase64":""}}`, "空音频"},
		{`{"request":{"audioBase64":"AA","nope":1}}`, "未知字段"},
		{`{"request":{"audioBase64":"AA","providerId":7}}`, "providerId 类型错"},
		{`{"request":{"audioBase64":"AA"},"audioBase64":"AA"}`, "两处同名"},
	} {
		code, body := mmServe(t, s, "speech.transcribe", bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d（%s）", bad.why, code, body)
		}
		if e := mmErrorOf(t, body); e.Error.Code != "bad_request" {
			t.Fatalf("%s 的 code 要 bad_request，拿到 %q", bad.why, e.Error.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// cotTranslation.translate：真实现
// ---------------------------------------------------------------------------

// mmFakeGateway 起一个本地 SSE 网关。reply 为空时只发 [DONE]（模拟"没有内容"）。
func mmFakeGateway(t *testing.T, reply string, calls *int32, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"gateway says no"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if reply != "" {
			raw, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{"content": reply}}},
			})
			write(string(raw))
		}
		write(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		write(`[DONE]`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(cotGatewayBaseEnv, srv.URL+"/v1")
	t.Setenv(cotGatewayKeyEnv, "test-key-123")
	t.Setenv(cotGatewayModelEnv, "test/model")
	return srv
}

func TestOfficialCotTranslationTranslateRealGateway(t *testing.T) {
	s := mmTestServer(t)
	var calls int32
	mmFakeGateway(t, "Hello, world.", &calls, http.StatusOK)

	code, body := mmServe(t, s, "cotTranslation.translate",
		`{"request":{"text":"你好，世界。","targetLanguage":"en"}}`)
	if code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d（%s）", code, body)
	}
	var out struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("正文不是 JSON: %v（%s）", err, body)
	}
	if out.Result != "Hello, world." {
		t.Fatalf("result 必须是模型真实输出，拿到 %q", out.Result)
	}
	if out.Result == "你好，世界。" {
		t.Fatalf("result 等于原文：拿原文冒充译文")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("应当只打一次网关，实际 %d 次", got)
	}

	// 平铺写法（不带 request 包裹）与显式 deepseek-official 都要能用。
	for _, payload := range []string{
		`{"text":"你好","targetLanguage":"en"}`,
		`{"request":{"text":"你好","targetLanguage":"en","provider":"deepseek-official","sourceLanguage":"zh-CN"}}`,
		`{"request":{"text":"你好","targetLanguage":"en","provider":"deepseek-account","sessionId":"s-1"}}`,
	} {
		code, body := mmServe(t, s, "cotTranslation.translate", payload)
		if code != http.StatusOK {
			t.Fatalf("要 200，拿到 %d（%s）payload=%s", code, body, payload)
		}
	}

	// 空文的译文就是空文：不打网关，也不编内容。
	before := atomic.LoadInt32(&calls)
	code, body = mmServe(t, s, "cotTranslation.translate", `{"request":{"text":"","targetLanguage":"en"}}`)
	if code != http.StatusOK {
		t.Fatalf("空文要 200，拿到 %d（%s）", code, body)
	}
	out.Result = "sentinel"
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("正文不是 JSON: %v", err)
	}
	if out.Result != "" {
		t.Fatalf("空文的译文必须是空串，拿到 %q", out.Result)
	}
	if after := atomic.LoadInt32(&calls); after != before {
		t.Fatalf("空文不该打网关：从 %d 变成 %d", before, after)
	}
}

func TestOfficialCotTranslationTranslateRefusals(t *testing.T) {
	s := mmTestServer(t)
	var calls int32
	mmFakeGateway(t, "Hello.", &calls, http.StatusOK)

	// 匿名端点明确 501（本仓不代理第三方匿名翻译）。
	for _, provider := range []string{"google", "bing"} {
		code, body := mmServe(t, s, "cotTranslation.translate",
			fmt.Sprintf(`{"request":{"text":"你好","targetLanguage":"en","provider":%q}}`, provider))
		if code != http.StatusNotImplemented {
			t.Fatalf("provider=%s 要 501，拿到 %d（%s）", provider, code, body)
		}
		e := mmErrorOf(t, body)
		if !strings.Contains(e.Error.Message, provider) {
			t.Fatalf("消息要说明是哪个 provider：%s", e.Error.Message)
		}
		if !strings.Contains(e.Error.Message, "匿名翻译") {
			t.Fatalf("消息要说清缺什么（第三方匿名翻译端点）：%s", e.Error.Message)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("被拒绝的 provider 不该打网关，实际 %d 次", got)
	}

	// 参数严格性。
	for _, bad := range []struct {
		body string
		why  string
	}{
		{`{}`, "缺 text/targetLanguage"},
		{`{"request":{"text":"你好"}}`, "缺 targetLanguage"},
		{`{"request":{"text":"你好","targetLanguage":""}}`, "targetLanguage 空串"},
		{`{"request":{"text":"你好","targetLanguage":"en","provider":"bogus"}}`, "provider 非法"},
		{`{"request":{"text":"你好","targetLanguage":"en","nope":1}}`, "未知字段"},
		{`{"request":{"text":"你好","targetLanguage":"en"},"text":"你好"}`, "两处同名"},
		{`{"request":"你好"}`, "request 不是对象"},
	} {
		code, body := mmServe(t, s, "cotTranslation.translate", bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d（%s）", bad.why, code, body)
		}
		if e := mmErrorOf(t, body); e.Error.Code != "bad_request" {
			t.Fatalf("%s 的 code 要 bad_request，拿到 %q", bad.why, e.Error.Code)
		}
	}
}

// 网关给了 200 但正文空 / 网关直接失败：都必须报错，**不许**回原文。
func TestOfficialCotTranslationTranslateNoFallbackToSource(t *testing.T) {
	s := mmTestServer(t)

	var empty int32
	mmFakeGateway(t, "", &empty, http.StatusOK)
	code, body := mmServe(t, s, "cotTranslation.translate", `{"request":{"text":"你好","targetLanguage":"en"}}`)
	if code == http.StatusOK {
		t.Fatalf("模型输出空文本时必须报错，不能给 200：%s", body)
	}
	if strings.Contains(body, `"result"`) {
		t.Fatalf("失败响应里不许出现 result：%s", body)
	}
	_ = mmErrorOf(t, body)

	var fail int32
	mmFakeGateway(t, "unused", &fail, http.StatusUnauthorized)
	code, body = mmServe(t, s, "cotTranslation.translate", `{"request":{"text":"你好","targetLanguage":"en"}}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("网关 401 要 500，拿到 %d（%s）", code, body)
	}
	e := mmErrorOf(t, body)
	if !strings.Contains(e.Error.Message, "调模型网关失败") {
		t.Fatalf("消息要说清是网关失败：%s", e.Error.Message)
	}
	if strings.Contains(e.Error.Message, "test-key-123") {
		t.Fatalf("错误消息里不许出现 api key：%s", e.Error.Message)
	}
	if strings.Contains(e.Error.Message, `"result"`) {
		t.Fatalf("失败响应里不许出现 result：%s", body)
	}
}

// ---------------------------------------------------------------------------
// officeToPdf.render：未实现
// ---------------------------------------------------------------------------

func TestOfficialOfficeRenderUnimplemented(t *testing.T) {
	s := mmTestServer(t)

	code, body := mmServe(t, s, "officeToPdf.render",
		`{"workspaceFileScopeId":"s-1","path":"/w/report.docx","priority":"foreground"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("要 501，拿到 %d（%s）", code, body)
	}
	e := mmErrorOf(t, body)
	if !strings.Contains(e.Error.Message, "LibreOffice") {
		t.Fatalf("消息要点名依赖（LibreOffice/libreoffice-kit）：%s", e.Error.Message)
	}
	if strings.Contains(body, `"data"`) || strings.Contains(body, `"eof"`) {
		t.Fatalf("未实现却回了渲染字节字段：%s", body)
	}

	// agentId / sessionId 两种既有写法都认。
	for _, payload := range []string{
		`{"agentId":"s-1","path":"/w/a.docx","priority":"background"}`,
		`{"sessionId":"s-1","path":"/w/a.docx","priority":"foreground"}`,
	} {
		if code, body := mmServe(t, s, "officeToPdf.render", payload); code != http.StatusNotImplemented {
			t.Fatalf("要 501，拿到 %d（%s）payload=%s", code, body, payload)
		}
	}

	// 参数严格性。
	for _, bad := range []struct {
		body string
		why  string
	}{
		{`{"path":"/w/a.docx","priority":"foreground"}`, "缺 workspaceFileScopeId"},
		{`{"workspaceFileScopeId":"","path":"/w/a.docx","priority":"foreground"}`, "scope 空串"},
		{`{"workspaceFileScopeId":"s-1","priority":"foreground"}`, "缺 path"},
		{`{"workspaceFileScopeId":"s-1","path":"/w/a.docx"}`, "缺 priority"},
		{`{"workspaceFileScopeId":"s-1","path":"/w/a.docx","priority":"now"}`, "priority 非法"},
		{`{"workspaceFileScopeId":"s-1","path":"/w/a.docx","priority":"foreground","x":1}`, "未知字段"},
	} {
		code, body := mmServe(t, s, "officeToPdf.render", bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d（%s）", bad.why, code, body)
		}
	}
}

// ---------------------------------------------------------------------------
// pluginMarket.* / pluginRecommendations.*：未实现 + 严格参数
// ---------------------------------------------------------------------------

func TestOfficialMarketSearchUnimplemented(t *testing.T) {
	s := mmTestServer(t)

	// 嵌套写法（贴着官方 wire）。
	code, body := mmServe(t, s, "pluginMarket.search",
		`{"url":"https://market.example/index.json","query":{"text":"lsp","sort":{"kind":"relevance"},"offset":0,"limit":50}}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("要 501，拿到 %d（%s）", code, body)
	}
	e := mmErrorOf(t, body)
	if !strings.Contains(e.Error.Message, "pluginMarket.search") {
		t.Fatalf("消息要点名方法：%s", e.Error.Message)
	}
	if !strings.Contains(e.Error.Message, "market.example") {
		t.Fatalf("消息要带上 url，便于排查：%s", e.Error.Message)
	}
	if strings.Contains(body, `"plugins"`) || strings.Contains(body, `"total"`) {
		t.Fatalf("未实现却回了搜索结果字段：%s", body)
	}

	// 平铺写法同样认。
	code, body = mmServe(t, s, "pluginMarket.search",
		`{"url":"https://market.example/index.json","sort":{"kind":"index"},"offset":0,"limit":1}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("平铺写法要 501，拿到 %d（%s）", code, body)
	}

	// 参数严格性。
	for _, bad := range []struct {
		body string
		why  string
	}{
		{`{"query":{"sort":{"kind":"index"},"offset":0,"limit":1}}`, "缺 url"},
		{`{"url":"u"}`, "缺 sort/offset/limit"},
		{`{"url":"u","sort":{"kind":"index"},"offset":0}`, "缺 limit"},
		{`{"url":"u","sort":{"kind":"index"},"offset":-1,"limit":1}`, "offset 为负"},
		{`{"url":"u","sort":{"kind":"index"},"offset":0,"limit":0}`, "limit 太小"},
		{`{"url":"u","sort":{"kind":"index"},"offset":0,"limit":101}`, "limit 太大"},
		{`{"url":"u","sort":{"kind":"index"},"offset":0.5,"limit":1}`, "offset 不是整数"},
		{`{"url":"u","sort":"index","offset":0,"limit":1}`, "sort 不是对象"},
		{`{"url":"u","sort":{"kind":"index"},"offset":0,"limit":1,"labels":["a",2]}`, "labels 元素不是字符串"},
		{`{"url":"u","sort":{"kind":"index"},"offset":0,"limit":1,"refresh":"yes"}`, "refresh 不是布尔"},
		{`{"url":"u","sort":{"kind":"index"},"offset":0,"limit":1,"nope":true}`, "未知字段"},
	} {
		code, body := mmServe(t, s, "pluginMarket.search", bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d（%s）", bad.why, code, body)
		}
	}
}

func TestOfficialMarketReadEntryDetailAndSetIndexes(t *testing.T) {
	s := mmTestServer(t)

	code, body := mmServe(t, s, "pluginMarket.readEntryDetail",
		`{"url":"https://m/x.json","name":"@acme/audit","version":"1.2.3","language":"zh-CN","options":{"refresh":true}}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("要 501，拿到 %d（%s）", code, body)
	}
	e := mmErrorOf(t, body)
	if !strings.Contains(e.Error.Message, "@acme/audit") {
		t.Fatalf("消息要带上条目名：%s", e.Error.Message)
	}
	if strings.Contains(body, `"status":"found"`) || strings.Contains(body, `"markdown"`) {
		t.Fatalf("未实现却回了文档：%s", body)
	}
	if code, body := mmServe(t, s, "pluginMarket.readEntryDetail",
		`{"url":"u","name":"n","version":"1","language":"zh"}`); code != http.StatusNotImplemented {
		t.Fatalf("不带 options 也要 501，拿到 %d（%s）", code, body)
	}
	for _, bad := range []struct {
		body string
		why  string
	}{
		{`{"name":"n","version":"1","language":"zh"}`, "缺 url"},
		{`{"url":"u","version":"1","language":"zh"}`, "缺 name"},
		{`{"url":"u","name":"n","language":"zh"}`, "缺 version"},
		{`{"url":"u","name":"n","version":"1"}`, "缺 language"},
		{`{"url":"u","name":"n","version":"1","language":"zh","options":{"npmFileSources":{"a":1}}}`, "npmFileSources 不是数组"},
		{`{"url":"u","name":"n","version":"1","language":"zh","options":{"refresh":"x"}}`, "refresh 不是布尔"},
		{`{"url":"u","name":"n","version":"1","language":"zh","junk":1}`, "未知字段"},
	} {
		code, body := mmServe(t, s, "pluginMarket.readEntryDetail", bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d（%s）", bad.why, code, body)
		}
	}

	code, body = mmServe(t, s, "pluginMarket.setIndexes", `{"urls":["https://a/i.json","https://b/i.json"]}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("要 501，拿到 %d（%s）", code, body)
	}
	e = mmErrorOf(t, body)
	if !strings.Contains(e.Error.Message, "2 个 url") {
		t.Fatalf("消息要说清几个 url 没落盘：%s", e.Error.Message)
	}
	for _, bad := range []struct{ body, why string }{
		{`{}`, "缺 urls"},
		{`{"urls":"https://a"}`, "urls 不是数组"},
		{`{"urls":[1]}`, "元素不是字符串"},
		{`{"urls":[],"x":1}`, "未知字段"},
	} {
		code, body := mmServe(t, s, "pluginMarket.setIndexes", bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d（%s）", bad.why, code, body)
		}
	}

	// describe：官方结果有 refused 分支，但本仓整条链路不存在 → 501（不假装"只是不可用"）。
	code, body = mmServe(t, s, "pluginRecommendations.describe", `{"agentId":"s-1","callId":"call-7"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("要 501，拿到 %d（%s）", code, body)
	}
	e = mmErrorOf(t, body)
	if !strings.Contains(e.Error.Message, "call-7") || !strings.Contains(e.Error.Message, "推荐") {
		t.Fatalf("消息要带上 callId 并说清缺推荐子系统：%s", e.Error.Message)
	}
	for _, bad := range []struct{ body, why string }{
		{`{"callId":"call-7"}`, "缺 agentId"},
		{`{"agentId":"s-1"}`, "缺 callId"},
		{`{"agentId":"s-1","callId":"call-7","x":1}`, "未知字段"},
	} {
		code, body := mmServe(t, s, "pluginRecommendations.describe", bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 要 400，拿到 %d（%s）", bad.why, code, body)
		}
	}
}

// ---------------------------------------------------------------------------
// dynamicCordisRunner.*：按 R6 明确不做
// ---------------------------------------------------------------------------

func TestOfficialCordisUnimplementedByR6(t *testing.T) {
	s := mmTestServer(t)

	cases := []struct{ method, body string }{
		{"dynamicCordisRunner.getClientCode", `{"agentId":"s-1","pluginId":"p","pluginRunId":"r"}`},
		{"dynamicCordisRunner.invoke", `{"pluginId":"p","pluginRunId":"r","method":"m","args":{}}`},
		{"dynamicCordisRunner.reportClientGuardFailure", `{"agentId":"s-1","pluginId":"p","pluginRunId":"r","failure":{}}`},
		{"dynamicCordisRunner.reportRenderFailure", `{"agentId":"s-1","pluginId":"p","pluginRunId":"r","failure":{}}`},
		{"dynamicCordisRunner.resolveInspectQuery", `{"agentId":"s-1","requestId":"q","resolution":"ok"}`},
		{"dynamicCordisRunner.resolveRequestRun", `{"requestId":"q"}`},
		{"dynamicCordisRunner.runHostHalf", `{"agentId":"s-1","pluginId":"p","packageId":"k","mode":"approve","requestId":"q","approveFutureVersions":true}`},
		{"dynamicCordisRunner.settleUserRun", `{"agentId":"s-1","pluginId":"p","resolution":{"kind":"done"}}`},
		{"dynamicCordisRunner.stopFromPanel", `{"agentId":"s-1","pluginId":"p"}`},
		{"dynamicCordisRunner.syncInspectManifest", `{"providers":[]}`},
		{"dynamicCordisRunner.undefineFromPanel", `{"agentId":"s-1","pluginId":"p"}`},
	}
	if len(cases) != 11 {
		t.Fatalf("R6 覆盖的 cordis 方法必须是 11 个，拿到 %d", len(cases))
	}
	for _, tc := range cases {
		code, body := mmServe(t, s, tc.method, tc.body)
		if code != http.StatusNotImplemented {
			t.Fatalf("%s 要 501，拿到 %d（%s）", tc.method, code, body)
		}
		e := mmErrorOf(t, body)
		if e.Error.Code != "not_implemented" {
			t.Fatalf("%s 的 code 要 not_implemented，拿到 %q", tc.method, e.Error.Code)
		}
		if !strings.Contains(e.Error.Message, "R6") {
			t.Fatalf("%s 的消息必须写明 R6（明确不做，不是欠账）：%s", tc.method, e.Error.Message)
		}
		if !strings.Contains(e.Error.Message, "JS 运行时") {
			t.Fatalf("%s 的消息要说清缺什么：%s", tc.method, e.Error.Message)
		}
	}
}

func TestOfficialCordisStrictParams(t *testing.T) {
	s := mmTestServer(t)

	for _, bad := range []struct{ method, body, why string }{
		{"dynamicCordisRunner.getClientCode", `{"pluginId":"p","pluginRunId":"r"}`, "缺 agentId"},
		{"dynamicCordisRunner.getClientCode", `{"agentId":"","pluginId":"p","pluginRunId":"r"}`, "agentId 空串"},
		{"dynamicCordisRunner.getClientCode", `{"agentId":7,"pluginId":"p","pluginRunId":"r"}`, "agentId 类型错"},
		{"dynamicCordisRunner.getClientCode", `{"agentId":"s","pluginId":"p","pluginRunId":"r","nope":1}`, "未知字段"},
		{"dynamicCordisRunner.getClientCode", `{"agentId":"s","pluginId":null,"pluginRunId":"r"}`, "null 必填"},
		{"dynamicCordisRunner.invoke", `{"pluginId":"p","pluginRunId":"r","method":"m"}`, "缺 args"},
		{"dynamicCordisRunner.syncInspectManifest", `{}`, "缺 providers"},
		{"dynamicCordisRunner.stopFromPanel", `{"agentId":"s"}`, "缺 pluginId"},
	} {
		code, body := mmServe(t, s, bad.method, bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s/%s 要 400，拿到 %d（%s）", bad.method, bad.why, code, body)
		}
		if e := mmErrorOf(t, body); e.Error.Code != "bad_request" {
			t.Fatalf("%s/%s 的 code 要 bad_request，拿到 %q", bad.method, bad.why, e.Error.Code)
		}
	}
}
