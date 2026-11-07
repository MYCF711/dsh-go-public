package transport

// 官方桌面端 0.2.2-rc.1 的 account / llm.discoverModels / productAnalytics.report /
// actionCards.act 共 10 个方法的 L1 契约测试。
//
// # 契约来源（只读镜像）
//
//   - account.*           dsh-api-account-controller/lib/typert.host.js
//   - llm.discoverModels  dsh-llm/lib/typert.host.js
//   - productAnalytics.*  dsh-client-product-analytics/lib/typert.host.js
//   - actionCards.act     dsh-action-cards/lib/typert.host.js
//
// 后三个包不在 `/tmp/official-api-src` 镜像里（镜像只有 20 个包），从
// `app.asar` 只读解出（sha256 5fa8a6b2…，见 RESULT 的"契约来源"）。
//
// # 为什么只打 handler、不打整条 /api 路由
//
// 本包的分发表在 `server.go`，本任务的写作用域不含它（Lead 统一接线）。
// 所以测试直接调 `(*Server).serveOfficialAccountAPI`——它收到的 `*http.Request`
// 与真实路由完全同形，参数校验、四字段错误、成功信封都是同一份代码。
// 路由接线本身在 RESULT 的"接线清单"里，由 Lead 验收。
//
// # 钉住的东西
//
//   - 方法名集合（10 个，点号，与官方 wire 逐字对应）；
//   - 没接线时**每个方法**都是四字段 501，且 message 点出方法名与缺的外部服务；
//   - 参数缺失/未知字段/类型不对/枚举越界一律 400（fail-loud，不静默兜底）；
//   - 两个 Server 各挂各的实现（A/B 隔离）；
//   - `llm.discoverModels` 的结果是官方数组形状，不是自造的包装。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// accountTestServer 造一个挂了 account 实现的最小 Server。
func accountTestServer(t *testing.T, api OfficialAccountAPI) *Server {
	t.Helper()
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.AttachOfficialAccountAPI(api)
	t.Cleanup(srv.DetachOfficialAccountAPI)
	return srv
}

// callOfficialAccount 打一次 account 组的方法，返回 HTTP 状态与解码后的信封。
func callOfficialAccount(t *testing.T, srv *Server, method string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case nil:
		raw = nil
	case string:
		raw = []byte(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		raw = encoded
	}
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.serveOfficialAccountAPI(rec, req, method)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
	}
	return rec.Code, out
}

// accountClient 是官方 `AccountClientMetadata` 的合法最小实例。
func accountClient() map[string]any {
	return map[string]any{
		"version":               "0.2.2-rc.1",
		"locale":                "zh-CN",
		"timezoneOffsetSeconds": float64(28800),
	}
}

// accountMinimalBodies 是 10 个方法各自"形状合法"的最小入参。
//
// 它只驱动"未实现"与"参数契约"两条路径，不参与真实语义断言
// （真实语义在 cmd/dshd 的宿主实现里测）。
func accountMinimalBodies() map[Method]any {
	return map[Method]any{
		MethodAccountGetProfile:           map[string]any{"client": accountClient()},
		MethodAccountGetBalance:           map[string]any{"client": accountClient()},
		MethodAccountGetUnnotifiedBonuses: map[string]any{"client": accountClient()},
		MethodAccountAckBonusNotified: map[string]any{
			"accountId": "acc-1", "orderId": "order-1", "client": accountClient(),
		},
		MethodAccountStartSignIn: map[string]any{
			"client": accountClient(), "callbackOrigin": "http://127.0.0.1:43210", "loginSource": "desktop",
		},
		MethodAccountCancelSignIn: map[string]any{"attemptId": "attempt-1"},
		MethodAccountSignOut:      map[string]any{"client": accountClient()},
		MethodLLMDiscoverModels: map[string]any{
			"settingsNs": "llm",
			"request":    map[string]any{"baseURL": "http://127.0.0.1:1/v1"},
		},
		MethodProductAnalyticsReport: map[string]any{
			"event": map[string]any{
				"eventName":  "desktop_app_launch",
				"attributes": map[string]any{},
				"timestamp":  float64(1730000000000),
			},
		},
		MethodActionCardsAct: map[string]any{
			"agentId": "s1", "callId": "call-1", "verb": "dismiss", "payload": map[string]any{},
		},
	}
}

// TestOfficialAccountMethodsAreStable 钉住方法集合：少一个、多一个、拼错都红。
func TestOfficialAccountMethodsAreStable(t *testing.T) {
	methods := OfficialAccountMethods()
	if len(methods) != 10 {
		t.Fatalf("官方 account 组应是 10 个方法，实际 %d", len(methods))
	}
	seen := map[Method]bool{}
	for _, m := range methods {
		if seen[m] {
			t.Fatalf("方法重复：%s", m)
		}
		seen[m] = true
		if !IsOfficialAccountMethod(string(m)) {
			t.Fatalf("%s 没被 IsOfficialAccountMethod 认出来（路由会漏）", m)
		}
		if strings.Contains(string(m), "/") {
			t.Fatalf("%s 含 '/'：本仓方法名用点号", m)
		}
	}
	want := []string{
		"account.ackBonusNotified",
		"account.cancelSignIn",
		"account.getBalance",
		"account.getProfile",
		"account.getUnnotifiedBonuses",
		"account.signOut",
		"account.startSignIn",
		"actionCards.act",
		"llm.discoverModels",
		"productAnalytics.report",
	}
	got := make([]string, 0, len(methods))
	for _, m := range methods {
		got = append(got, string(m))
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("方法集合与官方清单不一致\n got=%v\nwant=%v", got, want)
	}
	// 不是本组的方法一个都不许被吞进来（尤其不能被 account 前缀误吞）。
	for _, other := range []string{
		"account", "account.getState", "account.watch", "account.watchExpiry",
		"account.hasRunningAccountTasks", "accounts.getProfile", "llm.listProviders",
		"productAnalytics.watchPolicy", "actionCards.view", "",
	} {
		if IsOfficialAccountMethod(other) {
			t.Fatalf("%q 不该被当成 account 组方法", other)
		}
	}
}

// TestOfficialAccountUnimplementedIsLoudPerMethod：一个实现都没挂时，
// 10 个方法**逐个**必须是四字段 501，且 message 里点出方法名。
// 这是"不允许留空、静默成功"的机器可判据。
func TestOfficialAccountUnimplementedIsLoudPerMethod(t *testing.T) {
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	bodies := accountMinimalBodies()
	for _, method := range OfficialAccountMethods() {
		code, out := callOfficialAccount(t, srv, string(method), bodies[method])
		assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
		msg := out["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, string(method)) {
			t.Fatalf("%s 的未实现错误没点出方法名：%q", method, msg)
		}
	}
}

// TestOfficialAccountEmptyAPIIsNotImplemented：挂了空实现（每个回调都是 nil）
// 与"没挂"同形——能力不存在就是 501，不是 200 空对象。
func TestOfficialAccountEmptyAPIIsNotImplemented(t *testing.T) {
	srv := accountTestServer(t, OfficialAccountAPI{})
	bodies := accountMinimalBodies()
	for _, method := range OfficialAccountMethods() {
		code, out := callOfficialAccount(t, srv, string(method), bodies[method])
		assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	}
}

// TestOfficialAccountNeedsOfficialServiceHint：需要官方云账号的四个方法，
// message 必须说清"需要官方账号服务/凭据"，且**不许**伪造 profile / 余额 /
// 登录成功。
//
// 为什么钉 hint 文案：hint 字段由 errorHint(code) 按码给出（server.go 是
// 冻结的，加不了按方法分支），所以"缺什么"只能落在 message 上。这条测试
// 就是那个约定的守卫——改掉这句话会红。
func TestOfficialAccountNeedsOfficialServiceHint(t *testing.T) {
	srv := accountTestServer(t, OfficialAccountAPI{})
	bodies := accountMinimalBodies()
	for _, method := range []Method{
		MethodAccountGetProfile,
		MethodAccountGetBalance,
		MethodAccountStartSignIn,
		MethodAccountCancelSignIn,
	} {
		code, out := callOfficialAccount(t, srv, string(method), bodies[method])
		assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
		msg := out["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, "官方账号服务") {
			t.Fatalf("%s 的未实现错误没说清缺官方账号服务：%q", method, msg)
		}
		if strings.Contains(msg, "signed-in") || strings.Contains(msg, `"ready"`) {
			t.Fatalf("%s 的未实现错误里出现了成功字样：%q", method, msg)
		}
	}
}

// TestOfficialProductAnalyticsReportRefusesByPolicy：本仓明确不上报。
//
// 这条不是"没接线"，是产品决策：上报端点在官方是 Platform 采集服务，本仓
// 一个字节都不发。message 必须说清"本仓明确不上报"，否则调用方会以为只是
// 暂时没实现。
func TestOfficialProductAnalyticsReportRefusesByPolicy(t *testing.T) {
	srv := accountTestServer(t, OfficialAccountAPI{})
	code, out := callOfficialAccount(t, srv, string(MethodProductAnalyticsReport),
		accountMinimalBodies()[MethodProductAnalyticsReport])
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	msg := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "不上报") {
		t.Fatalf("productAnalytics.report 的错误没说清本仓不上报：%q", msg)
	}
}

// TestOfficialActionCardsActNeedsRuntime：本仓没有 action card 运行时。
func TestOfficialActionCardsActNeedsRuntime(t *testing.T) {
	srv := accountTestServer(t, OfficialAccountAPI{})
	code, out := callOfficialAccount(t, srv, string(MethodActionCardsAct),
		accountMinimalBodies()[MethodActionCardsAct])
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	msg := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "action card") {
		t.Fatalf("actionCards.act 的错误没说清缺 action card 运行时：%q", msg)
	}
}

// TestOfficialAccountParamContract 是全组的 fail-loud 参数契约：
// 缺必填、未知字段、类型不对、枚举越界**一律 400**，不许静默兜底。
func TestOfficialAccountParamContract(t *testing.T) {
	srv := accountTestServer(t, OfficialAccountAPI{})
	cases := []struct {
		name   string
		method Method
		body   any
	}{
		{"空请求体", MethodAccountGetProfile, ""},
		{"null 请求体", MethodAccountGetProfile, "null"},
		{"数组请求体", MethodAccountGetProfile, "[]"},
		{"缺 client", MethodAccountGetProfile, map[string]any{}},
		{"client 不是对象", MethodAccountGetProfile, map[string]any{"client": "x"}},
		{"client 缺 version", MethodAccountGetProfile, map[string]any{"client": map[string]any{
			"locale": "zh-CN", "timezoneOffsetSeconds": float64(0),
		}}},
		{"client 缺 locale", MethodAccountGetProfile, map[string]any{"client": map[string]any{
			"version": "1", "timezoneOffsetSeconds": float64(0),
		}}},
		{"client 缺 timezoneOffsetSeconds", MethodAccountGetProfile, map[string]any{"client": map[string]any{
			"version": "1", "locale": "zh-CN",
		}}},
		{"client 的 timezoneOffsetSeconds 不是数字", MethodAccountGetProfile, map[string]any{"client": map[string]any{
			"version": "1", "locale": "zh-CN", "timezoneOffsetSeconds": "28800",
		}}},
		{"client 带未知字段", MethodAccountGetProfile, map[string]any{"client": map[string]any{
			"version": "1", "locale": "zh-CN", "timezoneOffsetSeconds": float64(0), "token": "x",
		}}},
		{"顶层未知字段", MethodAccountGetProfile, map[string]any{
			"client": accountClient(), "extra": 1,
		}},
		{"ackBonusNotified 缺 accountId", MethodAccountAckBonusNotified, map[string]any{
			"orderId": "o", "client": accountClient(),
		}},
		{"ackBonusNotified 缺 orderId", MethodAccountAckBonusNotified, map[string]any{
			"accountId": "a", "client": accountClient(),
		}},
		{"ackBonusNotified 的 accountId 是空串", MethodAccountAckBonusNotified, map[string]any{
			"accountId": "", "orderId": "o", "client": accountClient(),
		}},
		{"startSignIn 缺 callbackOrigin", MethodAccountStartSignIn, map[string]any{
			"client": accountClient(), "loginSource": "web",
		}},
		{"startSignIn 的 callbackOrigin 不是 URL", MethodAccountStartSignIn, map[string]any{
			"client": accountClient(), "callbackOrigin": "not a url", "loginSource": "web",
		}},
		{"startSignIn 的 callbackOrigin 用了 file 协议", MethodAccountStartSignIn, map[string]any{
			"client": accountClient(), "callbackOrigin": "file:///tmp/x", "loginSource": "web",
		}},
		{"startSignIn 缺 loginSource", MethodAccountStartSignIn, map[string]any{
			"client": accountClient(), "callbackOrigin": "http://127.0.0.1:1",
		}},
		{"startSignIn 的 loginSource 越界", MethodAccountStartSignIn, map[string]any{
			"client": accountClient(), "callbackOrigin": "http://127.0.0.1:1", "loginSource": "mobile",
		}},
		{"cancelSignIn 缺 attemptId", MethodAccountCancelSignIn, map[string]any{}},
		{"cancelSignIn 的 attemptId 是空串", MethodAccountCancelSignIn, map[string]any{"attemptId": ""}},
		{"discoverModels 缺 settingsNs", MethodLLMDiscoverModels, map[string]any{}},
		{"discoverModels 的 settingsNs 是空串", MethodLLMDiscoverModels, map[string]any{"settingsNs": ""}},
		{"discoverModels 的 request 不是对象", MethodLLMDiscoverModels, map[string]any{
			"settingsNs": "llm", "request": "x",
		}},
		{"discoverModels 的 request 带未知字段", MethodLLMDiscoverModels, map[string]any{
			"settingsNs": "llm", "request": map[string]any{"endpoint": "x"},
		}},
		{"discoverModels 的 baseURL 不是字符串", MethodLLMDiscoverModels, map[string]any{
			"settingsNs": "llm", "request": map[string]any{"baseURL": 1},
		}},
		{"report 缺 event", MethodProductAnalyticsReport, map[string]any{}},
		{"report 的 event 不是对象", MethodProductAnalyticsReport, map[string]any{"event": "x"}},
		{"report 的 eventName 是空串", MethodProductAnalyticsReport, map[string]any{
			"event": map[string]any{"eventName": "", "timestamp": float64(1)},
		}},
		{"report 缺 timestamp", MethodProductAnalyticsReport, map[string]any{
			"event": map[string]any{"eventName": "desktop_app_launch"},
		}},
		{"report 的 timestamp 不是数字", MethodProductAnalyticsReport, map[string]any{
			"event": map[string]any{"eventName": "desktop_app_launch", "timestamp": "1"},
		}},
		{"act 缺 agentId", MethodActionCardsAct, map[string]any{"callId": "c", "verb": "dismiss"}},
		{"act 缺 callId", MethodActionCardsAct, map[string]any{"agentId": "s", "verb": "dismiss"}},
		{"act 缺 verb", MethodActionCardsAct, map[string]any{"agentId": "s", "callId": "c"}},
		{"act 的 verb 是空串", MethodActionCardsAct, map[string]any{
			"agentId": "s", "callId": "c", "verb": "",
		}},
		{"act 的 payload 不是对象", MethodActionCardsAct, map[string]any{
			"agentId": "s", "callId": "c", "verb": "dismiss", "payload": "x",
		}},
		{"act 顶层未知字段", MethodActionCardsAct, map[string]any{
			"agentId": "s", "callId": "c", "verb": "dismiss", "extra": 1,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := callOfficialAccount(t, srv, string(tc.method), tc.body)
			assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
		})
	}
}

// TestOfficialAccountViewShape：宿主给出真实 AccountView 时，回包逐字段
// 是官方形状（status/links/attempt，且 attempt 可为 null）。
func TestOfficialAccountViewShape(t *testing.T) {
	srv := accountTestServer(t, OfficialAccountAPI{
		AccountSignOut: func(_ context.Context, _ OfficialAccountClientMetadata) (OfficialAccountView, error) {
			return OfficialAccountView{
				Status: "signed-out",
				Links:  OfficialAccountLinks{UsageURL: "https://platform.example/usage", TopUpURL: "https://platform.example/topup"},
			}, nil
		},
	})
	code, out := callOfficialAccount(t, srv, string(MethodAccountSignOut),
		map[string]any{"client": accountClient()})
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200（body=%v）", code, out)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("回包没有 result 对象：%v", out)
	}
	if result["status"] != "signed-out" {
		t.Fatalf("status=%v，期望 signed-out", result["status"])
	}
	links, ok := result["links"].(map[string]any)
	if !ok {
		t.Fatalf("links 不是对象：%v", result)
	}
	if links["usageUrl"] != "https://platform.example/usage" || links["topUpUrl"] != "https://platform.example/topup" {
		t.Fatalf("links 字段名不对（官方是 usageUrl/topUpUrl）：%v", links)
	}
	// attempt 官方是 `Attempt | null`，必须出现且为 null（不是被省略）。
	if v, has := result["attempt"]; !has || v != nil {
		t.Fatalf("attempt 必须是显式 null，实际 has=%v v=%v", has, v)
	}
	// 官方 AccountView 不许出现 success/ok 这类自造字段。
	for _, bad := range []string{"ok", "success", "signedIn"} {
		if _, has := result[bad]; has {
			t.Fatalf("回包里出现了自造字段 %q：%v", bad, result)
		}
	}
}

// TestOfficialAccountNullableResultsAreWrapped：官方的 `T | null` 三兄弟
// （getProfile / getBalance / getUnnotifiedBonuses）在本仓 wire 上用
// `{"<name>": null}` 表达，因为 `{"result":null}` 会被出口变成 500
// （见 server.go writeJSON 的注释与既有的 `{"goal":null}` 先例）。
func TestOfficialAccountNullableResultsAreWrapped(t *testing.T) {
	srv := accountTestServer(t, OfficialAccountAPI{
		AccountGetProfile: func(_ context.Context, _ OfficialAccountClientMetadata) (*OfficialAccountProfile, error) {
			return nil, nil
		},
		AccountGetBalance: func(_ context.Context, _ OfficialAccountClientMetadata) (*OfficialAccountBalance, error) {
			return nil, nil
		},
		AccountGetUnnotifiedBonuses: func(_ context.Context, _ OfficialAccountClientMetadata) (*OfficialAccountBonusList, error) {
			return nil, nil
		},
	})
	for method, key := range map[Method]string{
		MethodAccountGetProfile:           "profile",
		MethodAccountGetBalance:           "balance",
		MethodAccountGetUnnotifiedBonuses: "bonuses",
	} {
		code, out := callOfficialAccount(t, srv, string(method), map[string]any{"client": accountClient()})
		if code != http.StatusOK {
			t.Fatalf("%s 状态码 %d，期望 200（body=%v）", method, code, out)
		}
		result, ok := out["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s 回包没有 result 对象：%v", method, out)
		}
		v, has := result[key]
		if !has {
			t.Fatalf("%s 回包缺 %q 键（null 必须是显式的）：%v", method, key, result)
		}
		if v != nil {
			t.Fatalf("%s 的 %q 应为 null，实际 %v", method, key, v)
		}
	}
}

// TestOfficialAccountAckBonusNotifiedBoolean：官方结果是裸 boolean。
func TestOfficialAccountAckBonusNotifiedBoolean(t *testing.T) {
	for _, want := range []bool{true, false} {
		srv := accountTestServer(t, OfficialAccountAPI{
			AccountAckBonusNotified: func(_ context.Context, accountID, orderID string, _ OfficialAccountClientMetadata) (bool, error) {
				if accountID != "acc-1" || orderID != "order-1" {
					return false, errors.New("参数没传到宿主")
				}
				return want, nil
			},
		})
		code, out := callOfficialAccount(t, srv, string(MethodAccountAckBonusNotified),
			accountMinimalBodies()[MethodAccountAckBonusNotified])
		if code != http.StatusOK {
			t.Fatalf("状态码 %d，期望 200（body=%v）", code, out)
		}
		if got, ok := out["result"].(bool); !ok || got != want {
			t.Fatalf("result=%v，期望裸布尔 %v（body=%v）", out["result"], want, out)
		}
	}
}

// TestOfficialDiscoverModelsShape：`llm.discoverModels` 的结果是**官方数组**，
// 每条至少带 id；可选字段只在宿主真的知道时才出现（不填默认值）。
func TestOfficialDiscoverModelsShape(t *testing.T) {
	srv := accountTestServer(t, OfficialAccountAPI{
		LLMDiscoverModels: func(_ context.Context, settingsNS string, req OfficialModelDiscoveryRequest) ([]OfficialDiscoveredModel, error) {
			if settingsNS != "llm" {
				return nil, officialBadRequest("settingsNs 没传到宿主：%q", settingsNS)
			}
			if req.BaseURL != "http://127.0.0.1:1/v1" {
				return nil, officialBadRequest("baseURL 没传到宿主：%q", req.BaseURL)
			}
			return []OfficialDiscoveredModel{
				{ID: "deepseek-chat", Name: "DeepSeek Chat", ContextWindow: 131072, MaxTokens: 8192,
					InputModalities: []string{"text"}},
				{ID: "bare-model"},
			}, nil
		},
	})
	code, out := callOfficialAccount(t, srv, string(MethodLLMDiscoverModels),
		accountMinimalBodies()[MethodLLMDiscoverModels])
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200（body=%v）", code, out)
	}
	items, ok := out["result"].([]any)
	if !ok {
		t.Fatalf("result 不是数组（官方结果是 ModelDiscovery[]）：%v", out)
	}
	if len(items) != 2 {
		t.Fatalf("条数 %d，期望 2", len(items))
	}
	first := items[0].(map[string]any)
	for _, key := range []string{"id", "name", "contextWindow", "maxTokens", "inputModalities"} {
		if _, has := first[key]; !has {
			t.Fatalf("官方字段 %q 丢了：%v", key, first)
		}
	}
	second := items[1].(map[string]any)
	// 可选字段没真值就不许出现（"absent means unknown"）。
	for _, key := range []string{"name", "contextWindow", "maxTokens", "inputModalities"} {
		if _, has := second[key]; has {
			t.Fatalf("未知的可选字段 %q 被编造成默认值：%v", key, second)
		}
	}
}

// TestOfficialAccountABIsolation：两个 Server 各挂各的实现，互不可见。
func TestOfficialAccountABIsolation(t *testing.T) {
	mk := func(status string) OfficialAccountAPI {
		return OfficialAccountAPI{
			AccountSignOut: func(_ context.Context, _ OfficialAccountClientMetadata) (OfficialAccountView, error) {
				return OfficialAccountView{Status: status, Links: OfficialAccountLinks{}}, nil
			},
		}
	}
	a := accountTestServer(t, mk("signed-out"))
	b := accountTestServer(t, mk("credential-stored"))

	check := func(srv *Server, want string) {
		t.Helper()
		code, out := callOfficialAccount(t, srv, string(MethodAccountSignOut),
			map[string]any{"client": accountClient()})
		if code != http.StatusOK {
			t.Fatalf("状态码 %d，期望 200（body=%v）", code, out)
		}
		result := out["result"].(map[string]any)
		if result["status"] != want {
			t.Fatalf("A/B 串了：status=%v，期望 %s", result["status"], want)
		}
	}
	check(a, "signed-out")
	check(b, "credential-stored")
	check(a, "signed-out")
}

// TestOfficialAccountNilServerIsSafe：nil Server 也不能 panic（Detach 幂等）。
func TestOfficialAccountNilServerIsSafe(t *testing.T) {
	var srv *Server
	srv.DetachOfficialAccountAPI()
	if _, ok := srv.officialAccountAPI(); ok {
		t.Fatalf("nil Server 不该有实现")
	}
	var api OfficialAccountAPI
	api = OfficialAccountAPI{}
	srv2 := accountTestServer(t, api)
	if _, ok := srv2.officialAccountAPI(); !ok {
		t.Fatalf("挂上去的实现读不回来")
	}
}

// TestOfficialAccountConcurrentAttachDetach：挂/摘是并发安全的（A/B 两个
// 宿主同时装配时不能互相踩 map）。
func TestOfficialAccountConcurrentAttachDetach(t *testing.T) {
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.AttachOfficialAccountAPI(OfficialAccountAPI{})
			_, _ = srv.officialAccountAPI()
			srv.DetachOfficialAccountAPI()
		}()
	}
	wg.Wait()
}
