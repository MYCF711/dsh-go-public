package transport

// 本文件是官方桌面端 0.2.2-rc.1「账号 / 模型发现 / 产品分析 / 动作卡」10 个
// typert 方法的 L1 语义实现。
//
// # 契约来源（逐条可核对）
//
//	account.*             dsh-api-account-controller/lib/typert.host.js
//	llm.discoverModels    dsh-llm/lib/typert.host.js
//	productAnalytics.*    dsh-client-product-analytics/lib/typert.host.js
//	actionCards.act       dsh-action-cards/lib/typert.host.js
//
// 前一个包在 `/tmp/official-api-src` 镜像里；后三个**不在**镜像里（镜像只有
// 20 个包），从 `app.asar` 只读解出（sha256 5fa8a6b2…）。具体 file:line 见
// RESULT 的"逐方法三列表"。
//
// # 对齐口径（见 docs/official-desktop-api-parity.md §2）
//
//   - **L1**：参数与结果**字段**按官方 zod 契约；wire 仍走本仓既有的
//     `POST /api/<method>` + 裸参数对象 + `{"result":…}` / 四字段 error。
//   - **L2**（`remote.mux` + typert Connection envelope）**未实现**，所以本文
//     不声称与官方桌面端 wire 兼容。
//
// # 本文件只做协议翻译与形状校验
//
// 不读盘、不碰凭据、不认识账号服务（红线 R7）。真实状态一律由宿主经
// `OfficialAccountAPI` 注入；某个能力宿主没接线时回**四字段的 not_implemented**
// 并写清**缺哪个外部服务**——绝不返回编出来的 profile / 余额，也绝不假装
// 登录成功。
//
// # 关于 hint 字段（一处显式约定）
//
// 本仓的四字段错误里 `hint` 由 `errorHint(code)` **按码**给出（server.go 是
// 冻结的，加不了按方法分支）。所以"缺哪个外部服务"这句只能落在 `message`
// 上——`TestOfficialAccountNeedsOfficialServiceHint` 就是它的守卫。要让
// `hint` 也带方法级文案，得先给 `errorHint` 加分支，那是 Lead 的写作用域。
//
// # 接线
//
// 唯一入口是 `IsOfficialAccountMethod` + `(*Server).serveOfficialAccountAPI`。
// `cmd/dshd` 侧 `AttachOfficialAccountAPI` 注入实现，`internal/transport/server.go`
// 的 `/api` 路由在既有 switch **之前**调用一次（接线清单见 RESULT）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
)

// 官方方法名。字符串必须与官方 `createTypertRemoteInfo` 里的
// `'account/getProfile'` 这类键逐字对应（本仓把 `/` 换成 `.`，与既有 68 个
// 官方方法同形）。
const (
	MethodAccountAckBonusNotified     Method = "account.ackBonusNotified"
	MethodAccountCancelSignIn         Method = "account.cancelSignIn"
	MethodAccountGetBalance           Method = "account.getBalance"
	MethodAccountGetProfile           Method = "account.getProfile"
	MethodAccountGetUnnotifiedBonuses Method = "account.getUnnotifiedBonuses"
	MethodAccountSignOut              Method = "account.signOut"
	MethodAccountStartSignIn          Method = "account.startSignIn"

	MethodLLMDiscoverModels Method = "llm.discoverModels"

	MethodProductAnalyticsReport Method = "productAnalytics.report"

	MethodActionCardsAct Method = "actionCards.act"
)

// 官方 `SignInAttempt.phase` 的完整枚举（见 account-controller 的
// cancelSignIn/getState 结果 schema）。
const (
	OfficialSignInPhaseInitializing   = "initializing"
	OfficialSignInPhaseWaitingBrowser = "waiting-browser"
	OfficialSignInPhaseExchanging     = "exchanging"
	OfficialSignInPhaseCommitting     = "committing"
	OfficialSignInPhaseSucceeded      = "succeeded"
	OfficialSignInPhaseCancelled      = "cancelled"
	OfficialSignInPhaseExpired        = "expired"
	OfficialSignInPhaseFailed         = "failed"
)

// OfficialSignInPhases 给宿主做白名单校验（值不在里面就是宿主自己编的）。
func OfficialSignInPhases() []string {
	return []string{
		OfficialSignInPhaseInitializing, OfficialSignInPhaseWaitingBrowser,
		OfficialSignInPhaseExchanging, OfficialSignInPhaseCommitting,
		OfficialSignInPhaseSucceeded, OfficialSignInPhaseCancelled,
		OfficialSignInPhaseExpired, OfficialSignInPhaseFailed,
	}
}

// 官方 `AccountView.status` 的枚举（只有两个值）。
const (
	OfficialAccountStatusSignedOut        = "signed-out"
	OfficialAccountStatusCredentialStored = "credential-stored"
)

// 官方 `SignInAttempt.errorCode` 的枚举。
const (
	OfficialSignInErrorExpired    = "expired"
	OfficialSignInErrorNoResponse = "no-response"
	OfficialSignInErrorNetwork    = "network"
	OfficialSignInErrorProtocol   = "protocol"
	OfficialSignInErrorStorage    = "storage"
)

// 官方 `ActionCardResult.status` 的四个可判别值。
const (
	OfficialActionCardAccepted    = "accepted"
	OfficialActionCardSettled     = "settled"
	OfficialActionCardRefused     = "refused"
	OfficialActionCardUnconfirmed = "unconfirmed"
)

// OfficialAccountMethods 按固定顺序返回本文件实现的 10 个方法。
//
// 它给快照/文档/门禁用：新增或删除一个方法必须显式改这里，不能让
// "方法悄悄多了一个"从路由表里漏过去。
func OfficialAccountMethods() []Method {
	return []Method{
		MethodAccountGetProfile,
		MethodAccountGetBalance,
		MethodAccountGetUnnotifiedBonuses,
		MethodAccountAckBonusNotified,
		MethodAccountStartSignIn,
		MethodAccountCancelSignIn,
		MethodAccountSignOut,
		MethodLLMDiscoverModels,
		MethodProductAnalyticsReport,
		MethodActionCardsAct,
	}
}

// IsOfficialAccountMethod 判断一个 /api 路径名是否属于本文件的 10 个方法。
//
// 路由侧只需要这一次判断（**不要**在 server.go 里摊开 10 个 case：那样
// 每加一个官方方法都要动一次早已冻结的路由表）。
func IsOfficialAccountMethod(name string) bool {
	switch Method(name) {
	case MethodAccountGetProfile, MethodAccountGetBalance,
		MethodAccountGetUnnotifiedBonuses, MethodAccountAckBonusNotified,
		MethodAccountStartSignIn, MethodAccountCancelSignIn, MethodAccountSignOut,
		MethodLLMDiscoverModels, MethodProductAnalyticsReport, MethodActionCardsAct:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 官方的数据结构（字段名/可选性逐条照 zod 契约）
// ---------------------------------------------------------------------------

// OfficialAccountClientMetadata 是官方 `AccountClientMetadata`。
//
// 三个字段**都必填**（官方 `required: ["version","locale","timezoneOffsetSeconds"]`），
// 且 `additionalProperties: false`——多一个键就是 400，不猜。
//
// timezoneOffsetSeconds 是 `z.number()`：**不是整数**（历史时区有 30/45 分钟
// 偏移，写成秒也不是整分钟），所以用 float64 接，不按"看起来像整数"拒掉。
type OfficialAccountClientMetadata struct {
	Version               string  `json:"version"`
	Locale                string  `json:"locale"`
	TimezoneOffsetSeconds float64 `json:"timezoneOffsetSeconds"`
}

// OfficialAccountLinks 是官方 `AccountView.links`（两个都必填，可以为空串）。
type OfficialAccountLinks struct {
	UsageURL string `json:"usageUrl"`
	TopUpURL string `json:"topUpUrl"`
}

// OfficialAccountAttempt 是一次浏览器登录尝试。
//
// 官方 `attempt` 是 `Attempt | null`：**没有尝试时必须是显式 null**，
// 不能省略（省略会让客户端分不清"没有尝试"和"字段丢了"）。
type OfficialAccountAttempt struct {
	ID           string `json:"id"`
	Phase        string `json:"phase"`
	AuthorizeURL string `json:"authorizeUrl,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	ErrorCode    string `json:"errorCode,omitempty"`
}

// OfficialAccountView 是官方 `AccountView`：安全账号投影。
type OfficialAccountView struct {
	Status  string                  `json:"status"`
	Links   OfficialAccountLinks    `json:"links"`
	Attempt *OfficialAccountAttempt `json:"attempt"`
}

// OfficialAccountProfileValue 是官方 `getProfile` 的 `value`。
//
// id/name/contact 三个字段**必填且可为 null**（官方是 `string | null`）：
// 用指针是为了区分"null"与"空串"——Platform 没给就是 null，不是 ""。
type OfficialAccountProfileValue struct {
	ID        *string `json:"id"`
	Name      *string `json:"name"`
	Contact   *string `json:"contact"`
	AvatarURL *string `json:"avatarUrl,omitempty"`
}

// OfficialAccountProfile 是官方 `getProfile` 的结果
// （`{status:"ready",value} | {status:"failed"}`）。
type OfficialAccountProfile struct {
	Status string                       `json:"status"`
	Value  *OfficialAccountProfileValue `json:"value,omitempty"`
}

// OfficialAccountWallet 是一个钱包余额（官方 `{currency:"CNY"|"USD", balance:string}`）。
//
// balance 是**字符串**：金额不该在传输层被浮点化（0.1+0.2 那类）。原样字符串。
type OfficialAccountWallet struct {
	Currency string `json:"currency"`
	Balance  string `json:"balance"`
}

// OfficialAccountBalance 是官方 `getBalance` 的结果
// （`{status:"ready",value,bonusWallets} | {status:"failed"}`）。
type OfficialAccountBalance struct {
	Status       string                  `json:"status"`
	Value        []OfficialAccountWallet `json:"value"`
	BonusWallets []OfficialAccountWallet `json:"bonusWallets"`
}

// OfficialAccountBonus 是一条未提醒的赠送记录。
//
// campaign/amount/currency/grantedAt 官方是 `z.json()`（**原样转发、不校验**），
// 所以用 RawMessage 接：本仓不认识它们的结构，也不该替 Platform 校验形状。
type OfficialAccountBonus struct {
	Title     string          `json:"title,omitempty"`
	OrderID   string          `json:"orderId"`
	Campaign  json.RawMessage `json:"campaign,omitempty"`
	Amount    json.RawMessage `json:"amount,omitempty"`
	Currency  json.RawMessage `json:"currency,omitempty"`
	GrantedAt json.RawMessage `json:"grantedAt,omitempty"`
	ExpiresAt string          `json:"expiresAt"`
	Message   string          `json:"message"`
}

// OfficialAccountBonusList 是官方 `getUnnotifiedBonuses` 的结果。
type OfficialAccountBonusList struct {
	AccountID string                 `json:"accountId"`
	Bonuses   []OfficialAccountBonus `json:"bonuses"`
}

// OfficialModelDiscoveryRequest 是官方 `LlmModelDiscoveryRequest`。
//
// 四个字段**全部可选**（官方 `required: []`）：一个 adapter 已经认识的 route
// 不需要 baseURL。本仓没有 provider 注册表，所以宿主那一侧缺 baseURL 时会
// 明确报错——不是静默回空列表。
type OfficialModelDiscoveryRequest struct {
	Provider string
	BaseURL  string
	API      string
	APIKey   string
}

// OfficialDiscoveredModel 是官方 `ModelDiscovery` 的一条。
//
// 除 id 外**全部可选**：官方写得很清楚 "absent means unknown"（
// inputModalities 的说明），所以没真值就不许出现，更不能填默认值。
type OfficialDiscoveredModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	ContextWindow   int64    `json:"contextWindow,omitempty"`
	MaxTokens       int64    `json:"maxTokens,omitempty"`
	InputModalities []string `json:"inputModalities,omitempty"`
}

// OfficialProductEvent 是官方 `ProductEvent` 的**判别字段**。
//
// 官方这个 union 有 20 多个变体（每个 eventName 一套 attributes）。本仓
// 明确不上报（见 productAnalyticsReport 的说明），所以这里只校验判别所需的
// 两个必填字段：eventName 与 timestamp。逐变体校验 attributes 形状属于
// "给一个永远 501 的方法写死 schema"，登记在 RESULT 的未验项里。
type OfficialProductEvent struct {
	EventName  string          `json:"eventName"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
	Timestamp  float64         `json:"timestamp"`
}

// OfficialActionCardDecision 是官方 `ActionCardDecision`。
type OfficialActionCardDecision struct {
	CallID string          `json:"callId"`
	Action string          `json:"action"`
	Phase  string          `json:"phase"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// OfficialActionCardResult 是官方 `ActionCardResult` 的四个可判别变体。
type OfficialActionCardResult struct {
	Status   string                      `json:"status"`
	Decision *OfficialActionCardDecision `json:"decision,omitempty"`
	Reason   string                      `json:"reason,omitempty"`
}

// ---------------------------------------------------------------------------
// 挂载点
// ---------------------------------------------------------------------------

// OfficialAccountAPI 是宿主侧的真实实现集合。**每个字段都可以为 nil**：
// 为 nil 表示这个能力在**当前宿主**上没有真实支撑，调用回 not_implemented
// 并说清缺什么——不是静默成功，也不是假数据。
//
// 为什么每个方法都带 ctx：`llm.discoverModels` 是一次**真实网络探测**
// （`GET <baseURL>/models`），必须能被调用方的取消/超时打断；账号类方法
// 将来接 Platform 时同理。
type OfficialAccountAPI struct {
	AccountGetProfile           func(ctx context.Context, client OfficialAccountClientMetadata) (*OfficialAccountProfile, error)
	AccountGetBalance           func(ctx context.Context, client OfficialAccountClientMetadata) (*OfficialAccountBalance, error)
	AccountGetUnnotifiedBonuses func(ctx context.Context, client OfficialAccountClientMetadata) (*OfficialAccountBonusList, error)
	AccountAckBonusNotified     func(ctx context.Context, accountID, orderID string, client OfficialAccountClientMetadata) (bool, error)
	AccountStartSignIn          func(ctx context.Context, client OfficialAccountClientMetadata, callbackOrigin, loginSource string) (OfficialAccountView, error)
	AccountCancelSignIn         func(ctx context.Context, attemptID string) (OfficialAccountView, error)
	AccountSignOut              func(ctx context.Context, client OfficialAccountClientMetadata) (OfficialAccountView, error)

	LLMDiscoverModels func(ctx context.Context, settingsNS string, req OfficialModelDiscoveryRequest) ([]OfficialDiscoveredModel, error)

	ProductAnalyticsReport func(ctx context.Context, event OfficialProductEvent) error

	ActionCardAct func(ctx context.Context, sessionID, callID, verb string, payload map[string]json.RawMessage) (OfficialActionCardResult, error)
}

// officialAccountMount 把一份实现挂到一个 Server 上。
//
// 为什么不用 `Options` 加字段：Options 在 server.go（本任务的写作用域之外，
// 由 Lead 统一接线）。这里用按 *Server 索引的表，隔离性与"每个 Server 一份
// 实现"完全一致：两个 Server（A/B）各挂各的，互不可见，也不会有进程级单例
// 被两个宿主共享。实现方用完可以 Detach 释放条目（测试里都 defer 了）。
var officialAccountMount = struct {
	mu sync.RWMutex
	m  map[*Server]OfficialAccountAPI
}{m: map[*Server]OfficialAccountAPI{}}

// AttachOfficialAccountAPI 给这个 Server 挂上官方账号组的真实实现。
// 必须发生在服务开始接受 /api 调用之前（serve 的装配阶段）。
func (s *Server) AttachOfficialAccountAPI(api OfficialAccountAPI) {
	if s == nil {
		return
	}
	officialAccountMount.mu.Lock()
	defer officialAccountMount.mu.Unlock()
	officialAccountMount.m[s] = api
}

// DetachOfficialAccountAPI 摘掉实现（测试收尾用；生产不需要调）。
func (s *Server) DetachOfficialAccountAPI() {
	if s == nil {
		return
	}
	officialAccountMount.mu.Lock()
	defer officialAccountMount.mu.Unlock()
	delete(officialAccountMount.m, s)
}

func (s *Server) officialAccountAPI() (OfficialAccountAPI, bool) {
	if s == nil {
		return OfficialAccountAPI{}, false
	}
	officialAccountMount.mu.RLock()
	defer officialAccountMount.mu.RUnlock()
	api, ok := officialAccountMount.m[s]
	return api, ok
}

// ---------------------------------------------------------------------------
// 分发
// ---------------------------------------------------------------------------

// serveOfficialAccountAPI 处理本文件的 10 个方法。
//
// 调用方（server.go 的 /api 路由）只需在既有 switch 之前判断
// `IsOfficialAccountMethod(name)` 并调用它；认证、`POST` 校验、失败日志都已经
// 在 handleAPI 里做过了。
func (s *Server) serveOfficialAccountAPI(w http.ResponseWriter, r *http.Request, name string) {
	api, ok := s.officialAccountAPI()
	if !ok {
		writeOfficialError(w, officialNotImplemented(
			"%s 未接线：宿主没有 AttachOfficialAccountAPI", name))
		return
	}
	params, err := decodeOfficialParams(r)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	result, err := routeOfficialAccountMethod(api, Method(name), r.Context(), params)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	if result == nil {
		// 官方 `productAnalytics.report` 的结果是 `void`，`actionCards.act` 等
		// 也允许无载荷。本仓出口不接受 `{"result":null}`（会变成 500），
		// 所以用**空 result 对象**表达"这次调用没有载荷"。
		// 该差异登记在 RESULT 的"与官方 wire 的显式差异"里。
		result = map[string]any{}
	}
	writeJSON(w, http.StatusOK, result)
}

// routeOfficialAccountMethod 是一个大 switch，放在本文件里而不是 server.go：
// 官方方法还会继续加，路由表却是冻结的。
func routeOfficialAccountMethod(api OfficialAccountAPI, method Method, ctx context.Context, p *officialParams) (any, error) {
	switch method {
	case MethodAccountGetProfile:
		return officialAccountGetProfile(ctx, api, p)
	case MethodAccountGetBalance:
		return officialAccountGetBalance(ctx, api, p)
	case MethodAccountGetUnnotifiedBonuses:
		return officialAccountGetUnnotifiedBonuses(ctx, api, p)
	case MethodAccountAckBonusNotified:
		return officialAccountAckBonusNotified(ctx, api, p)
	case MethodAccountStartSignIn:
		return officialAccountStartSignIn(ctx, api, p)
	case MethodAccountCancelSignIn:
		return officialAccountCancelSignIn(ctx, api, p)
	case MethodAccountSignOut:
		return officialAccountSignOut(ctx, api, p)
	case MethodLLMDiscoverModels:
		return officialLLMDiscoverModels(ctx, api, p)
	case MethodProductAnalyticsReport:
		return officialProductAnalyticsReport(ctx, api, p)
	case MethodActionCardsAct:
		return officialActionCardsAct(ctx, api, p)
	}
	return nil, officialNotImplemented("未知的官方 account 组方法 %q", method)
}

// ---------------------------------------------------------------------------
// 参数：AccountClientMetadata
// ---------------------------------------------------------------------------

// officialAccountClient 解析官方 `client` 参数（必填对象，严格模式）。
func officialAccountClient(p *officialParams) (OfficialAccountClientMetadata, error) {
	obj, ok, err := p.object("client")
	if err != nil {
		return OfficialAccountClientMetadata{}, err
	}
	if !ok {
		return OfficialAccountClientMetadata{}, officialBadRequest(
			"缺 client（官方 AccountClientMetadata：{\"version\":…,\"locale\":…,\"timezoneOffsetSeconds\":…}）")
	}
	o := newOfficialObject("client", obj)
	version, hasVersion, err := o.str("version")
	if err != nil {
		return OfficialAccountClientMetadata{}, err
	}
	locale, hasLocale, err := o.str("locale")
	if err != nil {
		return OfficialAccountClientMetadata{}, err
	}
	offset, hasOffset, err := o.numberFloat("timezoneOffsetSeconds")
	if err != nil {
		return OfficialAccountClientMetadata{}, err
	}
	if err := o.finish(); err != nil {
		return OfficialAccountClientMetadata{}, err
	}
	if !hasVersion || version == "" {
		return OfficialAccountClientMetadata{}, officialBadRequest("client.version 必填且非空")
	}
	if !hasLocale || locale == "" {
		return OfficialAccountClientMetadata{}, officialBadRequest("client.locale 必填且非空")
	}
	if !hasOffset {
		return OfficialAccountClientMetadata{}, officialBadRequest("client.timezoneOffsetSeconds 必填")
	}
	return OfficialAccountClientMetadata{
		Version: version, Locale: locale, TimezoneOffsetSeconds: offset,
	}, nil
}

// numberFloat 取一个浮点数字段。
//
// 官方 `timezoneOffsetSeconds` 是 `z.number()`（不是整数），所以不能复用
// `officialObject.number`（那个按"必须是整数"拒掉 1.5 这类合法输入）。
func (o *officialObject) numberFloat(name string) (float64, bool, error) {
	raw, ok := o.take(name)
	if !ok {
		return 0, false, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false, officialBadRequest("%s.%s 必须是数字", o.owner, name)
	}
	return f, true, nil
}

// ---------------------------------------------------------------------------
// account.getProfile / getBalance / getUnnotifiedBonuses
// ---------------------------------------------------------------------------

// 官方 `T | null` 三兄弟的回包键名。
//
// **与官方 wire 的一处显式差异**：本仓 `/api` 的成功信封不允许
// `{"result":null}`（出口会把 null 变成 500），因此"没有"用
// `{"profile":null}` / `{"balance":null}` / `{"bonuses":null}` 表达——
// 与既有 `session.goal.get` 的 `{"goal":null}` 同形。
const (
	officialAccountProfileKey = "profile"
	officialAccountBalanceKey = "balance"
	officialAccountBonusesKey = "bonuses"
)

func officialAccountGetProfile(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	client, err := officialAccountClient(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.AccountGetProfile == nil {
		return nil, officialNotImplemented(
			"account.getProfile 未实现：需要官方账号服务/凭据——profile 只能由官方 DeepSeek " +
				"Platform 账号授权取回，本宿主没有该服务（也不伪造 profile 数据）")
	}
	profile, err := api.AccountGetProfile(ctx, client)
	if err != nil {
		return nil, err
	}
	if profile == nil {
		return map[string]any{officialAccountProfileKey: nil}, nil
	}
	return map[string]any{officialAccountProfileKey: profile}, nil
}

func officialAccountGetBalance(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	client, err := officialAccountClient(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.AccountGetBalance == nil {
		return nil, officialNotImplemented(
			"account.getBalance 未实现：需要官方账号服务/凭据——余额（含赠送钱包）只能由官方 " +
				"DeepSeek Platform 账号服务查询，本宿主没有该服务（也不编造余额）")
	}
	balance, err := api.AccountGetBalance(ctx, client)
	if err != nil {
		return nil, err
	}
	if balance == nil {
		return map[string]any{officialAccountBalanceKey: nil}, nil
	}
	// 官方 ready 变体里 value/bonusWallets 都是**必填数组**：nil 要落成 []
	// （null 会让严格客户端把"没有钱包"读成"字段丢了"）。
	if balance.Value == nil {
		balance.Value = []OfficialAccountWallet{}
	}
	if balance.BonusWallets == nil {
		balance.BonusWallets = []OfficialAccountWallet{}
	}
	return map[string]any{officialAccountBalanceKey: balance}, nil
}

func officialAccountGetUnnotifiedBonuses(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	client, err := officialAccountClient(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.AccountGetUnnotifiedBonuses == nil {
		return nil, officialNotImplemented(
			"account.getUnnotifiedBonuses 未实现：需要官方账号服务/凭据——赠送记录由官方 " +
				"DeepSeek Platform 账号服务下发，本宿主没有该服务（也不编造赠送记录）")
	}
	bonuses, err := api.AccountGetUnnotifiedBonuses(ctx, client)
	if err != nil {
		return nil, err
	}
	if bonuses == nil {
		// 官方：账号授权不存在/已变化时回 null。这是**确定的真值**，不是空实现。
		return map[string]any{officialAccountBonusesKey: nil}, nil
	}
	if bonuses.Bonuses == nil {
		bonuses.Bonuses = []OfficialAccountBonus{}
	}
	return map[string]any{officialAccountBonusesKey: bonuses}, nil
}

// ---------------------------------------------------------------------------
// account.ackBonusNotified
// ---------------------------------------------------------------------------

func officialAccountAckBonusNotified(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	accountID, err := p.str("accountId")
	if err != nil {
		return nil, err
	}
	orderID, err := p.str("orderId")
	if err != nil {
		return nil, err
	}
	client, err := officialAccountClient(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if accountID == "" {
		return nil, officialBadRequest("accountId 是空串")
	}
	if orderID == "" {
		return nil, officialBadRequest("orderId 是空串")
	}
	if api.AccountAckBonusNotified == nil {
		return nil, officialNotImplemented(
			"account.ackBonusNotified 未实现：需要官方账号服务/凭据——已读回执要写到官方 " +
				"DeepSeek Platform 账号服务，本宿主没有该服务（也不假装记下了）")
	}
	return api.AccountAckBonusNotified(ctx, accountID, orderID, client)
}

// ---------------------------------------------------------------------------
// account.startSignIn / cancelSignIn / signOut
// ---------------------------------------------------------------------------

// officialLoopbackOrigin 校验官方 `callbackOrigin`（"browser-accessible
// loopback HTTP origin"）。
//
// 只做形状与协议校验：http/https + 有 host。**不**强制 host 必须是
// 127.0.0.1（自定义域解析到环回、或 IPv6 写法都合法），但 `file:`/`javascript:`
// 这类协议一定拒——把这种 origin 交给浏览器等于把回调打到本地文件上。
func officialLoopbackOrigin(raw string) error {
	if raw == "" {
		return officialBadRequest("callbackOrigin 是空串")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return officialBadRequest("callbackOrigin 不是合法 URL: %v", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return officialBadRequest("callbackOrigin 必须是 http/https 的环回 origin，收到 %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return officialBadRequest("callbackOrigin 缺 host（官方要一个浏览器可达的环回 origin）")
	}
	return nil
}

func officialAccountStartSignIn(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	client, err := officialAccountClient(p)
	if err != nil {
		return nil, err
	}
	callbackOrigin, err := p.str("callbackOrigin")
	if err != nil {
		return nil, err
	}
	loginSource, err := p.str("loginSource")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if err := officialLoopbackOrigin(callbackOrigin); err != nil {
		return nil, err
	}
	if loginSource != "web" && loginSource != "desktop" {
		return nil, officialBadRequest("loginSource 必须是 \"web\" 或 \"desktop\"，收到 %q", loginSource)
	}
	if api.AccountStartSignIn == nil {
		return nil, officialNotImplemented(
			"account.startSignIn 未实现：需要官方账号服务/凭据——浏览器登录要官方 DeepSeek " +
				"Platform 的授权端点与 code 交换，本宿主没有该服务（也不假装登录成功）")
	}
	return api.AccountStartSignIn(ctx, client, callbackOrigin, loginSource)
}

func officialAccountCancelSignIn(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	attemptID, err := p.str("attemptId")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if attemptID == "" {
		return nil, officialBadRequest("attemptId 是空串")
	}
	if api.AccountCancelSignIn == nil {
		return nil, officialNotImplemented(
			"account.cancelSignIn 未实现：需要官方账号服务/凭据——本宿主没有在跑的登录尝试" +
				"（登录本身要官方 DeepSeek Platform 账号服务）")
	}
	return api.AccountCancelSignIn(ctx, attemptID)
}

func officialAccountSignOut(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	client, err := officialAccountClient(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.AccountSignOut == nil {
		// 官方 signOut 的语义是"移除本地账号授权"（**不**删 API key）。
		// 本宿主没有本地账号授权存储时，真实状态就是 signed-out；
		// 但"没有实现"和"已经登出"必须能区分，所以这里**不**回成功——
		// 回成功会让客户端以为它真的清掉了什么。
		return nil, officialNotImplemented(
			"account.signOut 未接线：宿主没有账号授权存储（本仓没有官方账号登录，" +
				"本地没有可清掉的账号授权）；清 API key 请用 credentials.unset")
	}
	return api.AccountSignOut(ctx, client)
}

// ---------------------------------------------------------------------------
// llm.discoverModels
// ---------------------------------------------------------------------------

func officialLLMDiscoverModels(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	settingsNS, err := p.str("settingsNs")
	if err != nil {
		return nil, err
	}
	obj, hasRequest, err := p.object("request")
	if err != nil {
		return nil, err
	}
	req := OfficialModelDiscoveryRequest{}
	if hasRequest {
		o := newOfficialObject("request", obj)
		provider, _, err := o.str("provider")
		if err != nil {
			return nil, err
		}
		baseURL, _, err := o.str("baseURL")
		if err != nil {
			return nil, err
		}
		apiName, _, err := o.str("api")
		if err != nil {
			return nil, err
		}
		apiKey, _, err := o.str("apiKey")
		if err != nil {
			return nil, err
		}
		if err := o.finish(); err != nil {
			return nil, err
		}
		req = OfficialModelDiscoveryRequest{
			Provider: provider, BaseURL: baseURL, API: apiName, APIKey: apiKey,
		}
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if settingsNS == "" {
		return nil, officialBadRequest("settingsNs 是空串（官方要它来选注册的 discovery）")
	}
	if api.LLMDiscoverModels == nil {
		return nil, officialNotImplemented(
			"llm.discoverModels 未接线：宿主没有模型发现器（真实实现是 GET <baseURL>/models 探测）")
	}
	models, err := api.LLMDiscoverModels(ctx, settingsNS, req)
	if err != nil {
		return nil, err
	}
	if models == nil {
		// 官方结果是数组：空就是 []（"这个端点一个模型都没挂"是确定的真值）。
		models = []OfficialDiscoveredModel{}
	}
	for i, model := range models {
		if model.ID == "" {
			return nil, officialInternal("模型发现第 %d 条没有 id（宿主给了非法结果）", i)
		}
		for _, modality := range model.InputModalities {
			if modality != "text" && modality != "image" {
				return nil, officialInternal(
					"模型发现第 %d 条的 inputModalities 含官方枚举外的值 %q", i, modality)
			}
		}
	}
	return models, nil
}

// ---------------------------------------------------------------------------
// productAnalytics.report
// ---------------------------------------------------------------------------

func officialProductAnalyticsReport(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	obj, ok, err := p.object("event")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, officialBadRequest("缺 event（官方 ProductEvent）")
	}
	o := newOfficialObject("event", obj)
	eventName, hasName, err := o.str("eventName")
	if err != nil {
		return nil, err
	}
	rawAttributes, _ := o.take("attributes")
	rawTimestamp, hasTimestamp, err := o.numberFloat("timestamp")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasName || eventName == "" {
		return nil, officialBadRequest("event.eventName 必填且非空")
	}
	if !hasTimestamp {
		return nil, officialBadRequest("event.timestamp 必填（毫秒时间戳）")
	}
	event := OfficialProductEvent{
		EventName: eventName, Attributes: rawAttributes, Timestamp: rawTimestamp,
	}
	if api.ProductAnalyticsReport == nil {
		return nil, officialNotImplemented(
			"productAnalytics.report 未实现：本仓明确不上报——DSH GO 是本地宿主，产品分析事件" +
				"（desktop_app_launch 等）一个字节都不发往任何采集服务；这是产品决策，不是待接线")
	}
	if err := api.ProductAnalyticsReport(ctx, event); err != nil {
		return nil, err
	}
	// 官方结果是 `void`：真的上报了也没有载荷可回。
	return map[string]any{}, nil
}

// ---------------------------------------------------------------------------
// actionCards.act
// ---------------------------------------------------------------------------

func officialActionCardsAct(ctx context.Context, api OfficialAccountAPI, p *officialParams) (any, error) {
	sessionID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	callID, err := p.str("callId")
	if err != nil {
		return nil, err
	}
	verb, err := p.str("verb")
	if err != nil {
		return nil, err
	}
	obj, hasPayload, err := p.object("payload")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if callID == "" {
		return nil, officialBadRequest("callId 是空串")
	}
	if verb == "" {
		return nil, officialBadRequest("verb 是空串（官方只允许 \"dismiss\" 或 kind 自己的动词）")
	}
	var payload map[string]json.RawMessage
	if hasPayload {
		payload = obj
	}
	if api.ActionCardAct == nil {
		return nil, officialNotImplemented(
			"actionCards.act 未实现：本仓没有 action card 运行时——没有 propose_action 工具、"+
				"没有 kind 注册表、没有卡片的 Session 投影，%q 动词无处分发（也不假装记下了）", verb)
	}
	result, err := api.ActionCardAct(ctx, sessionID, callID, verb, payload)
	if err != nil {
		return nil, err
	}
	switch result.Status {
	case OfficialActionCardAccepted, OfficialActionCardSettled,
		OfficialActionCardRefused, OfficialActionCardUnconfirmed:
	default:
		return nil, officialInternal(
			"actionCards.act 收到宿主给的非法 status %q（官方枚举只有 accepted/settled/refused/unconfirmed）",
			result.Status)
	}
	if result.Status == OfficialActionCardSettled && result.Decision == nil {
		return nil, officialInternal("actionCards.act 的 settled 结果缺 decision（官方必填）")
	}
	if (result.Status == OfficialActionCardRefused || result.Status == OfficialActionCardUnconfirmed) &&
		result.Reason == "" {
		return nil, officialInternal(
			"actionCards.act 的 %s 结果缺 reason（官方必填）", result.Status)
	}
	return result, nil
}
