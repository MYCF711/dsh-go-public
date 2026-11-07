package main

// 官方桌面端 0.2.2-rc.1「账号 / 模型发现 / 产品分析 / 动作卡」10 个方法在
// **宿主机**这一侧的真实实现。
//
// # 分工（与 internal/transport/handlers_account_official.go 的边界）
//
// 契约、参数校验、官方字段、四字段错误都在 Transport 那一侧；本文件只提供
// Transport 拿不到的真实数据（红线 R7：Transport 不读盘、不碰凭据、不知道
// 账号服务长什么样）。
//
// # 只绑定**真有支撑**的能力
//
// `OfficialAccountAPI` 的每个回调为 nil 表示"当前宿主没有这个能力"，Transport
// 会回官方形状的四字段 501 并说清缺什么。本文件**故意**只绑四个：
//
//	account.getUnnotifiedBonuses  读本地账号授权里的赠送记录（见下）
//	account.ackBonusNotified      把"已读"写进本地账号授权
//	account.signOut               删掉本地账号授权，回真实状态
//	llm.discoverModels            真实探测 `GET <baseURL>/models`
//
// 其余六个**必须是 nil**，理由写在这里而不是编一个成功回包：
//
//	account.getProfile            要官方 DeepSeek Platform 账号服务；伪造 profile
//	                              （哪怕是空对象）就是让界面显示一个不存在的账号
//	account.getBalance            同上；余额是钱，编不得
//	account.startSignIn           要官方授权端点 + code 交换；假装成功等于骗用户"登上了"
//	account.cancelSignIn          同上；本宿主没有在跑的登录尝试
//	productAnalytics.report       **产品决策**：本仓明确不上报（DSH GO 是本地宿主）
//	actionCards.act               本仓没有 action card 运行时（没有 propose_action 工具、
//	                              没有 kind 注册表、没有卡片的 Session 投影）
//
// # 本地账号授权存储（`<home>/state/account-grant.json`）
//
// 官方 `account.*` 的语义都挂在"本地账号授权（account grant）"上：
// `getUnnotifiedBonuses` 在授权不存在时回 `null`、`ackBonusNotified` 回 `false`、
// `signOut` 的职责就是删掉它。本仓**没有**任何东西会写这个文件（登录未实现），
// 所以它是一个"真实但当前为空"的存储——这样 signOut 的"清本地凭据"是真删、
// getUnnotifiedBonuses 的 `null` 是**读出来的真值**而不是写死的常量。
//
// 等哪天 startSignIn 接上官方服务，它往这里写 grant + bonuses，这两个方法
// 立刻就是完整功能（不需要再改）。
//
// # 接线（Lead 在 cmd/dshd/serve.go 补一行）
//
//	srv.AttachOfficialAccountAPI(newAccountOfficialAPI(accountOfficialDeps{
//	    home: *home,
//	}))
//
// 以及 `internal/transport/server.go` 的 /api 路由在既有 switch **之前**加：
//
//	if IsOfficialAccountMethod(name) {
//		s.serveOfficialAccountAPI(w, r, name)
//		return
//	}

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dshgo/internal/llm"
	"dshgo/internal/transport"
)

// accountGrantVersion 是本地账号授权文档的格式版本。
const accountGrantVersion = 1

// accountGrantFileName 是本地账号授权在 home 里的位置。
//
// 与 `state/session-model.json`、`state/session-permission.json`、
// `state/session-preset.json` 同构：账号授权是**状态覆盖**，不是会话日志
// （R5：v4 是 append-only，登录态不属于任何一轮对话）。
const accountGrantFileName = "account-grant.json"

// accountOfficialDeps 是本文件需要的全部真实来源。
type accountOfficialDeps struct {
	// home 是宿主 home（账号授权落在 `<home>/state/account-grant.json`）。
	home string
}

// accountGrantDocument 是本地账号授权的持久形状。
//
// AccountID + Bonuses 是"官方平台同步下来的账号与赠送记录"的本地投影；
// AckedOrderIDs 是本机已经给用户看过（已回执）的那些 orderId。
type accountGrantDocument struct {
	Version       int                              `json:"version"`
	AccountID     string                           `json:"accountId"`
	Bonuses       []transport.OfficialAccountBonus `json:"bonuses"`
	AckedOrderIDs []string                         `json:"ackedOrderIds"`
}

// accountOfficialAPI 持有 home，并把真实实现绑定成 Transport 的回调。
type accountOfficialAPI struct {
	deps accountOfficialDeps
}

// newAccountOfficialAPI 把真实依赖装配成 Transport 要的账号组回调。
//
// **只绑真有支撑的四个**；其余留 nil，让 Transport 回官方形状的 501
// 并说清缺哪个外部服务（见文件头的清单）。
func newAccountOfficialAPI(deps accountOfficialDeps) transport.OfficialAccountAPI {
	api := &accountOfficialAPI{deps: deps}
	return transport.OfficialAccountAPI{
		AccountGetUnnotifiedBonuses: api.getUnnotifiedBonuses,
		AccountAckBonusNotified:     api.ackBonusNotified,
		AccountSignOut:              api.signOut,
		LLMDiscoverModels:           api.discoverModels,
	}
}

// grantPath 是本地账号授权的绝对路径。
func (a *accountOfficialAPI) grantPath() (string, error) {
	if strings.TrimSpace(a.deps.home) == "" {
		return "", fmt.Errorf("%w: 宿主没有 home，无处放账号授权", transport.ErrOfficialNotImplemented)
	}
	return filepath.Join(a.deps.home, "state", accountGrantFileName), nil
}

// loadGrant 读本地账号授权。
//
// 返回 found=false 表示**没有授权**（未登录）——这是正常状态，不是错误。
// 文件存在但读不出来/形状不对时**必须报错**：把损坏的授权当成"未登录"会让
// 用户以为自己的账号被登出了（而真实原因是磁盘坏了）。
func (a *accountOfficialAPI) loadGrant() (accountGrantDocument, bool, error) {
	path, err := a.grantPath()
	if err != nil {
		return accountGrantDocument{}, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return accountGrantDocument{}, false, nil
		}
		return accountGrantDocument{}, false, fmt.Errorf("%w: 读 %s: %v", transport.ErrStatePersistence, path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return accountGrantDocument{}, false, fmt.Errorf("%w: %s 是空文件", transport.ErrStatePersistence, path)
	}
	var doc accountGrantDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return accountGrantDocument{}, false, fmt.Errorf("%w: 解析 %s: %v", transport.ErrStatePersistence, path, err)
	}
	if doc.Version != accountGrantVersion {
		return accountGrantDocument{}, false, fmt.Errorf(
			"%w: %s 的 version=%d，本宿主只认 %d",
			transport.ErrStatePersistence, path, doc.Version, accountGrantVersion)
	}
	if strings.TrimSpace(doc.AccountID) == "" {
		return accountGrantDocument{}, false, fmt.Errorf(
			"%w: %s 缺 accountId（授权存在却不知道属于谁）", transport.ErrStatePersistence, path)
	}
	return doc, true, nil
}

// saveGrant 原子写本地账号授权（同目录 tmp + rename）。
func (a *accountOfficialAPI) saveGrant(doc accountGrantDocument) error {
	path, err := a.grantPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%w: 建 state 目录: %v", transport.ErrStatePersistence, err)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: 序列化账号授权: %v", transport.ErrStatePersistence, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("%w: 写 %s: %v", transport.ErrStatePersistence, tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%w: 落盘 %s: %v", transport.ErrStatePersistence, path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// account.getUnnotifiedBonuses
// ---------------------------------------------------------------------------

// getUnnotifiedBonuses 返回"平台下发过、本机还没回执"的赠送记录。
//
// 没有本地账号授权时回 (nil, nil)：官方结果类型是 `... | null`，且说明里
// 写死 "null when the account grant is absent or changed"——对一个没登录的
// 宿主，`null` 就是**确定的真值**，不是空实现。
func (a *accountOfficialAPI) getUnnotifiedBonuses(
	_ context.Context, _ transport.OfficialAccountClientMetadata,
) (*transport.OfficialAccountBonusList, error) {
	doc, found, err := a.loadGrant()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	acked := make(map[string]bool, len(doc.AckedOrderIDs))
	for _, orderID := range doc.AckedOrderIDs {
		acked[orderID] = true
	}
	bonuses := make([]transport.OfficialAccountBonus, 0, len(doc.Bonuses))
	for _, bonus := range doc.Bonuses {
		if acked[bonus.OrderID] {
			continue
		}
		bonuses = append(bonuses, bonus)
	}
	return &transport.OfficialAccountBonusList{AccountID: doc.AccountID, Bonuses: bonuses}, nil
}

// ---------------------------------------------------------------------------
// account.ackBonusNotified
// ---------------------------------------------------------------------------

// ackBonusNotified 把一条赠送记录标成"已给用户看过"，并**真的落盘**。
//
// 返回 false 的两种真实情况（官方："false when the account is absent or
// changed"）：
//   - 没有本地账号授权（未登录）；
//   - 本地授权的账号/记录里没有这个 orderId（账号变了，或这条不是本机的）。
//
// 两种情况都**不**写盘：编一条不存在的回执只会让下次多出一条假的"已读"。
func (a *accountOfficialAPI) ackBonusNotified(
	_ context.Context, accountID, orderID string, _ transport.OfficialAccountClientMetadata,
) (bool, error) {
	doc, found, err := a.loadGrant()
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	// 账号必须对得上：不同账号的同名 orderId 不能互相污染回执。
	if doc.AccountID != accountID {
		return false, nil
	}
	known := false
	for _, bonus := range doc.Bonuses {
		if bonus.OrderID == orderID {
			known = true
			break
		}
	}
	if !known {
		return false, nil
	}
	for _, acked := range doc.AckedOrderIDs {
		if acked == orderID {
			// 幂等：重复回执不重复追加，但仍然回 true（平台侧已经是"已读"）。
			return true, nil
		}
	}
	doc.AckedOrderIDs = append(doc.AckedOrderIDs, orderID)
	if err := a.saveGrant(doc); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// account.signOut
// ---------------------------------------------------------------------------

// signOut 删掉本地账号授权，并回真实状态。
//
// 官方语义："Remove the local account grant and revoke it through Platform in
// the background, without deleting API keys."——本宿主没有 Platform 可调，
// 所以**只做本地那一半**（删文件），且明确不碰 API key（那是
// `credentials.unset` 的事）。
//
// 回包里的 `links` 是 Platform 的用量/充值页地址：本机没有 Platform 配置，
// 所以是**空串**（官方 AccountView 里两个字段都必填，空串 = 不知道），
// 不编一个看起来像真的 URL。
func (a *accountOfficialAPI) signOut(
	_ context.Context, _ transport.OfficialAccountClientMetadata,
) (transport.OfficialAccountView, error) {
	view := transport.OfficialAccountView{
		Status: transport.OfficialAccountStatusSignedOut,
		Links:  transport.OfficialAccountLinks{},
	}
	path, err := a.grantPath()
	if err != nil {
		return view, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return view, fmt.Errorf("%w: 删 %s: %v", transport.ErrStatePersistence, path, err)
	}
	// 删完再把状态读回来：不假设"删了就等于没了"（权限/挂载点都可能让它还在）。
	if _, found, readErr := a.loadGrant(); readErr != nil {
		return view, readErr
	} else if found {
		return view, fmt.Errorf("%w: %s 删不掉（还在）", transport.ErrStatePersistence, path)
	}
	return view, nil
}

// ---------------------------------------------------------------------------
// llm.discoverModels
// ---------------------------------------------------------------------------

// discoverModels 真实探测 `GET <baseURL>/models` 并回官方形状。
//
// 本仓**没有 provider 注册表**（官方那个 `settingsNs` 指向的"已注册的
// discovery"本仓不存在），所以 baseURL 必须由调用方给：缺了就 fail-loud，
// 不拿进程默认端点顶上（那会问错服务，还把别人的模型列表画出来）。
func (a *accountOfficialAPI) discoverModels(
	ctx context.Context, _ string, req transport.OfficialModelDiscoveryRequest,
) ([]transport.OfficialDiscoveredModel, error) {
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		provider := strings.TrimSpace(req.Provider)
		if provider == "" {
			return nil, fmt.Errorf(
				"%w: llm.discoverModels 缺 request.baseURL——本仓没有 provider 注册表，"+
					"无法从 provider 推出端点；请直接给 request.baseURL",
				transport.ErrOfficialBadInput)
		}
		return nil, fmt.Errorf(
			"%w: llm.discoverModels 无法解析 provider %q——本仓没有 provider 注册表"+
				"（只有模型目录，且它按模型 id 而不是 provider 名索引）；请直接给 request.baseURL",
			transport.ErrOfficialBadInput, provider)
	}
	client := llm.New(baseURL, req.APIKey)
	models, err := client.DiscoverModels(ctx)
	if err != nil {
		// 端点探测失败是**上游**的问题（不是我们的参数错）：按 internal 报，
		// 并保留原文（已由 llm 侧脱敏一次性凭据）。
		return nil, fmt.Errorf("%w: llm.discoverModels 探测 %s 失败: %v",
			transport.ErrStatePersistence, baseURL, err)
	}
	out := make([]transport.OfficialDiscoveredModel, 0, len(models))
	for _, model := range models {
		out = append(out, transport.OfficialDiscoveredModel{
			ID:              model.ID,
			Name:            model.Name,
			ContextWindow:   model.ContextWindow,
			MaxTokens:       model.MaxTokens,
			InputModalities: model.InputModalities,
		})
	}
	return out, nil
}
