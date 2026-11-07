package main

// 官方 account / llm.discoverModels 在**宿主机**这一侧的真实实现测试。
//
// 全部用真实临时 home、真实文件、真实 httptest 端点：不 mock 存储，也不
// mock HTTP。
//
// 覆盖：
//   - signOut 真删本地账号授权，回真实 AccountView（幂等）；
//   - getUnnotifiedBonuses / ackBonusNotified 读写的**是同一份**本地授权，
//     回执落盘后未读清单真的变短；
//   - 没有账号授权时回官方定义的 null / false（不是错误、也不是假数据）；
//   - 账号对不上 / 记录不存在时 ack 回 false 且**不写盘**；
//   - A/B 两个 home 不串；
//   - 需要官方云账号的六个能力**故意留 nil**（Transport 会回四字段 501）；
//   - llm.discoverModels 真探测 `/v1/models` 并回官方形状。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/transport"
)

// accountHome 造一个临时 home 并建好 state 目录。
func accountHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o755); err != nil {
		t.Fatalf("建 state 目录: %v", err)
	}
	return home
}

// accountGrantPathOf 是测试侧算出的授权文件路径（与实现用同一个文件名）。
func accountGrantPathOf(home string) string {
	return filepath.Join(home, "state", accountGrantFileName)
}

// writeAccountGrant 往 home 里放一份账号授权（模拟"平台同步下来的"）。
func writeAccountGrant(t *testing.T, home, accountID string, bonuses []transport.OfficialAccountBonus, acked []string) {
	t.Helper()
	doc := accountGrantDocument{
		Version:       accountGrantVersion,
		AccountID:     accountID,
		Bonuses:       bonuses,
		AckedOrderIDs: acked,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("序列化授权: %v", err)
	}
	if err := os.WriteFile(accountGrantPathOf(home), raw, 0o600); err != nil {
		t.Fatalf("写授权: %v", err)
	}
}

func testAccountClient() transport.OfficialAccountClientMetadata {
	return transport.OfficialAccountClientMetadata{
		Version: "0.2.2-rc.1", Locale: "zh-CN", TimezoneOffsetSeconds: 28800,
	}
}

// ---------------------------------------------------------------------------
// 绑定面：只绑真有支撑的四个
// ---------------------------------------------------------------------------

// TestAccountOfficialAPIOnlyBindsRealCapabilities 钉住"不许假装"的那条线：
// 需要官方云账号的四个 + 本仓明确不做/没有运行时的两个，**必须**是 nil。
//
// 为什么用 nil 而不是回一个空成功：Transport 对 nil 回官方形状的四字段 501
// 并说清缺哪个外部服务。绑一个"回 null 的实现"会让界面以为能力存在。
func TestAccountOfficialAPIOnlyBindsRealCapabilities(t *testing.T) {
	api := newAccountOfficialAPI(accountOfficialDeps{home: accountHome(t)})
	mustNil := map[string]bool{
		"account.getProfile":      api.AccountGetProfile == nil,
		"account.getBalance":      api.AccountGetBalance == nil,
		"account.startSignIn":     api.AccountStartSignIn == nil,
		"account.cancelSignIn":    api.AccountCancelSignIn == nil,
		"productAnalytics.report": api.ProductAnalyticsReport == nil,
		"actionCards.act":         api.ActionCardAct == nil,
	}
	for method, ok := range mustNil {
		if !ok {
			t.Fatalf("%s 不该被绑定（本宿主没有真实支撑，必须让 Transport 回 501）", method)
		}
	}
	mustBound := map[string]bool{
		"account.getUnnotifiedBonuses": api.AccountGetUnnotifiedBonuses != nil,
		"account.ackBonusNotified":     api.AccountAckBonusNotified != nil,
		"account.signOut":              api.AccountSignOut != nil,
		"llm.discoverModels":           api.LLMDiscoverModels != nil,
	}
	for method, ok := range mustBound {
		if !ok {
			t.Fatalf("%s 必须有真实实现", method)
		}
	}
}

// ---------------------------------------------------------------------------
// account.signOut
// ---------------------------------------------------------------------------

// TestAccountSignOutRemovesGrant：真删文件，回真实状态。
func TestAccountSignOutRemovesGrant(t *testing.T) {
	home := accountHome(t)
	writeAccountGrant(t, home, "acc-1", nil, nil)
	api := newAccountOfficialAPI(accountOfficialDeps{home: home})

	view, err := api.AccountSignOut(context.Background(), testAccountClient())
	if err != nil {
		t.Fatalf("signOut: %v", err)
	}
	if view.Status != transport.OfficialAccountStatusSignedOut {
		t.Fatalf("status=%q，期望 signed-out", view.Status)
	}
	if view.Attempt != nil {
		t.Fatalf("attempt 必须是 nil（没有登录尝试），实际 %+v", view.Attempt)
	}
	if view.Links.UsageURL != "" || view.Links.TopUpURL != "" {
		t.Fatalf("links 不该编造 URL：%+v", view.Links)
	}
	if _, statErr := os.Stat(accountGrantPathOf(home)); !os.IsNotExist(statErr) {
		t.Fatalf("授权文件还在（stat err=%v）", statErr)
	}
}

// TestAccountSignOutIsIdempotent：没有授权时也是 signed-out，不报错。
func TestAccountSignOutIsIdempotent(t *testing.T) {
	home := accountHome(t)
	api := newAccountOfficialAPI(accountOfficialDeps{home: home})
	for i := 0; i < 3; i++ {
		view, err := api.AccountSignOut(context.Background(), testAccountClient())
		if err != nil {
			t.Fatalf("第 %d 次 signOut: %v", i+1, err)
		}
		if view.Status != transport.OfficialAccountStatusSignedOut {
			t.Fatalf("第 %d 次 status=%q", i+1, view.Status)
		}
	}
}

// TestAccountSignOutNeedsHome：没有 home 是明确的未实现，不是静默成功。
func TestAccountSignOutNeedsHome(t *testing.T) {
	api := newAccountOfficialAPI(accountOfficialDeps{})
	if _, err := api.AccountSignOut(context.Background(), testAccountClient()); !errors.Is(err, transport.ErrOfficialNotImplemented) {
		t.Fatalf("没有 home 时应回未实现，实际 %v", err)
	}
}

// TestAccountSignOutDoesNotTouchCredentials：官方明确"不删 API key"。
func TestAccountSignOutDoesNotTouchCredentials(t *testing.T) {
	home := accountHome(t)
	credential := filepath.Join(home, "state", "credentials.json")
	if err := os.WriteFile(credential, []byte(`{"DSH_KEY":"x"}`), 0o600); err != nil {
		t.Fatalf("写凭据: %v", err)
	}
	writeAccountGrant(t, home, "acc-1", nil, nil)
	api := newAccountOfficialAPI(accountOfficialDeps{home: home})
	if _, err := api.AccountSignOut(context.Background(), testAccountClient()); err != nil {
		t.Fatalf("signOut: %v", err)
	}
	if _, statErr := os.Stat(credential); statErr != nil {
		t.Fatalf("signOut 动了 API 凭据（官方明确不删）：%v", statErr)
	}
}

// ---------------------------------------------------------------------------
// account.getUnnotifiedBonuses / ackBonusNotified
// ---------------------------------------------------------------------------

// TestAccountBonusesWithoutGrantIsOfficialNull：没登录时回官方定义的 null。
func TestAccountBonusesWithoutGrantIsOfficialNull(t *testing.T) {
	api := newAccountOfficialAPI(accountOfficialDeps{home: accountHome(t)})
	list, err := api.AccountGetUnnotifiedBonuses(context.Background(), testAccountClient())
	if err != nil {
		t.Fatalf("getUnnotifiedBonuses: %v", err)
	}
	if list != nil {
		t.Fatalf("没有账号授权时应回 nil（官方 null），实际 %+v", list)
	}
	acked, err := api.AccountAckBonusNotified(context.Background(), "acc-1", "order-1", testAccountClient())
	if err != nil {
		t.Fatalf("ackBonusNotified: %v", err)
	}
	if acked {
		t.Fatalf("没有账号授权时 ack 必须回 false（官方：account absent）")
	}
}

// TestAccountAckBonusNotifiedPersists：回执真的落盘，未读清单真的变短。
func TestAccountAckBonusNotifiedPersists(t *testing.T) {
	home := accountHome(t)
	writeAccountGrant(t, home, "acc-1", []transport.OfficialAccountBonus{
		{OrderID: "order-1", ExpiresAt: "", Message: "第一条"},
		{OrderID: "order-2", ExpiresAt: "", Message: "第二条"},
	}, nil)
	api := newAccountOfficialAPI(accountOfficialDeps{home: home})

	before, err := api.AccountGetUnnotifiedBonuses(context.Background(), testAccountClient())
	if err != nil {
		t.Fatalf("getUnnotifiedBonuses: %v", err)
	}
	if before == nil || len(before.Bonuses) != 2 || before.AccountID != "acc-1" {
		t.Fatalf("初始未读清单不对：%+v", before)
	}

	ok, err := api.AccountAckBonusNotified(context.Background(), "acc-1", "order-1", testAccountClient())
	if err != nil {
		t.Fatalf("ackBonusNotified: %v", err)
	}
	if !ok {
		t.Fatalf("已知记录的回执应回 true")
	}

	// 落盘：重新造一个宿主（模拟重启）也要看到回执。
	reopened := newAccountOfficialAPI(accountOfficialDeps{home: home})
	after, err := reopened.AccountGetUnnotifiedBonuses(context.Background(), testAccountClient())
	if err != nil {
		t.Fatalf("重开后的 getUnnotifiedBonuses: %v", err)
	}
	if after == nil || len(after.Bonuses) != 1 || after.Bonuses[0].OrderID != "order-2" {
		t.Fatalf("回执没落盘：%+v", after)
	}

	// 幂等：再回执一次仍然是 true，且不会重复追加。
	again, err := reopened.AccountAckBonusNotified(context.Background(), "acc-1", "order-1", testAccountClient())
	if err != nil {
		t.Fatalf("重复回执: %v", err)
	}
	if !again {
		t.Fatalf("重复回执应仍回 true")
	}
	raw, err := os.ReadFile(accountGrantPathOf(home))
	if err != nil {
		t.Fatalf("读授权: %v", err)
	}
	var doc accountGrantDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析授权: %v", err)
	}
	if len(doc.AckedOrderIDs) != 1 {
		t.Fatalf("回执列表重复追加：%v", doc.AckedOrderIDs)
	}
}

// TestAccountAckBonusNotifiedRefusesUnknown：账号/记录对不上回 false 且不写盘。
func TestAccountAckBonusNotifiedRefusesUnknown(t *testing.T) {
	cases := []struct {
		name      string
		accountID string
		orderID   string
	}{
		{"账号对不上", "acc-other", "order-1"},
		{"记录不存在", "acc-1", "order-999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := accountHome(t)
			writeAccountGrant(t, home, "acc-1", []transport.OfficialAccountBonus{
				{OrderID: "order-1", ExpiresAt: "", Message: "第一条"},
			}, nil)
			before, err := os.ReadFile(accountGrantPathOf(home))
			if err != nil {
				t.Fatalf("读授权: %v", err)
			}
			api := newAccountOfficialAPI(accountOfficialDeps{home: home})
			ok, err := api.AccountAckBonusNotified(context.Background(), tc.accountID, tc.orderID, testAccountClient())
			if err != nil {
				t.Fatalf("ackBonusNotified: %v", err)
			}
			if ok {
				t.Fatalf("对不上的回执应回 false")
			}
			after, err := os.ReadFile(accountGrantPathOf(home))
			if err != nil {
				t.Fatalf("读授权: %v", err)
			}
			if string(before) != string(after) {
				t.Fatalf("被拒的回执改了磁盘（%s → %s）", before, after)
			}
		})
	}
}

// TestAccountGrantCorruptIsLoud：授权文件坏了要报错，不能当成"未登录"。
//
// 把损坏的授权读成"没登录"会让用户以为账号被登出了，而真实原因是磁盘坏了。
func TestAccountGrantCorruptIsLoud(t *testing.T) {
	for _, body := range []string{"", "   ", "{", `{"version":1}`, `{"version":99,"accountId":"a"}`} {
		t.Run(body, func(t *testing.T) {
			home := accountHome(t)
			if err := os.WriteFile(accountGrantPathOf(home), []byte(body), 0o600); err != nil {
				t.Fatalf("写坏授权: %v", err)
			}
			api := newAccountOfficialAPI(accountOfficialDeps{home: home})
			if _, err := api.AccountGetUnnotifiedBonuses(context.Background(), testAccountClient()); err == nil {
				t.Fatalf("坏授权 %q 被当成未登录", body)
			}
			if _, err := api.AccountAckBonusNotified(context.Background(), "a", "o", testAccountClient()); err == nil {
				t.Fatalf("坏授权 %q 被当成 false", body)
			}
		})
	}
}

// TestAccountBonusHomesAreIsolated：A/B 两个 home 不串。
func TestAccountBonusHomesAreIsolated(t *testing.T) {
	homeA := accountHome(t)
	homeB := accountHome(t)
	writeAccountGrant(t, homeA, "acc-a", []transport.OfficialAccountBonus{
		{OrderID: "order-a", ExpiresAt: "", Message: "A"},
	}, nil)
	writeAccountGrant(t, homeB, "acc-b", []transport.OfficialAccountBonus{
		{OrderID: "order-b", ExpiresAt: "", Message: "B"},
	}, nil)
	apiA := newAccountOfficialAPI(accountOfficialDeps{home: homeA})
	apiB := newAccountOfficialAPI(accountOfficialDeps{home: homeB})

	listA, err := apiA.AccountGetUnnotifiedBonuses(context.Background(), testAccountClient())
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	listB, err := apiB.AccountGetUnnotifiedBonuses(context.Background(), testAccountClient())
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if listA.AccountID != "acc-a" || listB.AccountID != "acc-b" {
		t.Fatalf("A/B 串了：%q / %q", listA.AccountID, listB.AccountID)
	}
	// A 回执不能影响 B。
	if _, err := apiA.AccountAckBonusNotified(context.Background(), "acc-a", "order-a", testAccountClient()); err != nil {
		t.Fatalf("A ack: %v", err)
	}
	if after, _ := apiB.AccountGetUnnotifiedBonuses(context.Background(), testAccountClient()); after == nil || len(after.Bonuses) != 1 {
		t.Fatalf("A 的回执影响了 B：%+v", after)
	}
}

// ---------------------------------------------------------------------------
// llm.discoverModels
// ---------------------------------------------------------------------------

// TestAccountDiscoverModelsProbesEndpoint：真探测 `/v1/models`，回官方形状。
func TestAccountDiscoverModelsProbesEndpoint(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat","name":"DeepSeek Chat",
          "context_window":131072,"max_tokens":8192,"input_modalities":["text"]}]}`))
	}))
	defer srv.Close()

	api := newAccountOfficialAPI(accountOfficialDeps{home: accountHome(t)})
	models, err := api.LLMDiscoverModels(context.Background(), "llm", transport.OfficialModelDiscoveryRequest{
		BaseURL: srv.URL + "/v1", APIKey: "sk-one-shot",
	})
	if err != nil {
		t.Fatalf("discoverModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("探测路径 %q，期望 /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-one-shot" {
		t.Fatalf("一次性凭据没带上：%q", gotAuth)
	}
	if len(models) != 1 {
		t.Fatalf("条数 %d，期望 1", len(models))
	}
	got := models[0]
	if got.ID != "deepseek-chat" || got.Name != "DeepSeek Chat" ||
		got.ContextWindow != 131072 || got.MaxTokens != 8192 ||
		len(got.InputModalities) != 1 || got.InputModalities[0] != "text" {
		t.Fatalf("官方字段映射错：%+v", got)
	}
}

// TestAccountDiscoverModelsNeedsBaseURL：没有 provider 注册表时必须 fail-loud，
// 不拿进程默认端点顶上（那会问错服务，还把别人的模型列表画出来）。
func TestAccountDiscoverModelsNeedsBaseURL(t *testing.T) {
	api := newAccountOfficialAPI(accountOfficialDeps{home: accountHome(t)})
	for _, req := range []transport.OfficialModelDiscoveryRequest{
		{},
		{Provider: "deepseek"},
	} {
		if _, err := api.LLMDiscoverModels(context.Background(), "llm", req); !errors.Is(err, transport.ErrOfficialBadInput) {
			t.Fatalf("缺 baseURL 时应回 bad_request，实际 %v", err)
		}
	}
}

// TestAccountDiscoverModelsUpstreamFailureIsLoud：端点挂了是错误，不是空列表。
func TestAccountDiscoverModelsUpstreamFailureIsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	api := newAccountOfficialAPI(accountOfficialDeps{home: accountHome(t)})
	_, err := api.LLMDiscoverModels(context.Background(), "llm", transport.OfficialModelDiscoveryRequest{
		BaseURL: srv.URL + "/v1",
	})
	if err == nil {
		t.Fatalf("500 被当成空列表")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("错误里没有状态码：%v", err)
	}
}

// TestAccountDiscoverModelsEmptyResultIsEmptySlice：端点真没挂模型 → 空数组。
func TestAccountDiscoverModelsEmptyResultIsEmptySlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	api := newAccountOfficialAPI(accountOfficialDeps{home: accountHome(t)})
	models, err := api.LLMDiscoverModels(context.Background(), "llm", transport.OfficialModelDiscoveryRequest{
		BaseURL: srv.URL + "/v1",
	})
	if err != nil {
		t.Fatalf("discoverModels: %v", err)
	}
	if models == nil || len(models) != 0 {
		t.Fatalf("空结果应是长度 0 的非 nil 切片，实际 %#v", models)
	}
}
