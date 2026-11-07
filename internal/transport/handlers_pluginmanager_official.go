package transport

// 本文件是官方桌面端 0.2.2-rc.1「pluginManager 侧」12 个 typert 方法的 L1
// 契约实现（官方源：@deepseek-ai/dsh-plugin-manager/lib/typert.host.js）。
//
// 对齐口径与 `handlers_agent_official.go` 一致：
//   - **L1**：参数名与结果字段按官方 zod 契约；wire 仍走本仓既有的
//     `POST /api/<method>` + 裸参数对象 + `{"result":…}` / 四字段 error。
//   - **L2**（`remote.mux` + typert Connection envelope）未实现，所以本文不
//     声称与官方桌面端 wire 兼容。
//
// 本文件只做协议翻译与**参数形状校验**：真实状态（已安装包、保留版本、启用位、
// 安装事务、包内文档）由宿主经 `OfficialPluginManagerAPI` 注入，实现在
// `internal/pluginmgr/official`。某个字段为 nil 表示这个能力在**当前宿主**上
// 没有真实支撑：回四字段 `not_implemented` 并点名缺什么，绝不返回假数据。
//
// ## 与官方语义的显式映射（必须与 RESULT 的映射表一致）
//
//   - 官方「bundle」= 本仓可安装单元 `dsh.local-plugin@1` 包，名字即包 id。
//     本仓的包**没有** patch rows，所以 `inspect` 如实地报 `bundle:false`；
//     `installBundle` 仍接受它——本仓没有第二层可安装单元。
//   - 本仓一包一插件，因此 `setBundleEnabled` 与 `setPluginEnabled` 写的是
//     **同一个**持久化启用位（同一 id 空间），不是两个独立开关。
//   - `application` 只取 applied/cancelled/failed：本仓没有运行时插件加载器，
//     所以每条 applied 结果都带一条 `warnings` 明确写出"只落了 profile 状态"。
//     不会报 `restart-required`（重启也不会加载），也不报 `overridden`。
//   - `waitForInstall` 官方的 `ChangeResult | null` 在本仓写成
//     `{"active":false}`：本仓 `/api` 出口不允许 `{"result":null}`（会把 null
//     变成 500），`active` 是客户端可判定、且不会伪装成"有载荷"的写法。
//   - 需要 registry / pnpm / git / tarball 的能力（inspect 的三种非本地规格、
//     listVersions 的 registry 查询、updateBundle 的在线下载、
//     installBundle 的构建审批与 registry 选项）一律 501 并点名缺哪个能力。
//
// 真实状态、持久化、A/B 隔离与取消回滚由 `internal/pluginmgr/official` 的
// 适配器测试覆盖；本文件用假宿主钉住协议。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// 官方方法名。字符串必须与官方 `@deepseek-ai/dsh-plugin-manager` 的
// `createTypertRemoteInfo` 键逐字对应（官方 `'pluginManager/inspect'`；
// 本仓把 `/` 换成 `.`，与既有 58 个方法同形）。
const (
	MethodPluginManagerListVersions Method = "pluginManager.listVersions"
	// MethodPluginManagerListVersionExemptions 官方签名：
	// `@Remote listVersionExemptions(): { exemptions: Record<string, string[]>; warnings: string[] }`
	// （typert.host.js:1277-1278 / 结果 schema 178-183）。**零参数**。
	MethodPluginManagerListVersionExemptions Method = "pluginManager.listVersionExemptions"
	MethodPluginManagerInspect               Method = "pluginManager.inspect"
	MethodPluginManagerInstallBundle         Method = "pluginManager.installBundle"
	MethodPluginManagerUpdateBundle          Method = "pluginManager.updateBundle"
	MethodPluginManagerRemoveBundle          Method = "pluginManager.removeBundle"
	MethodPluginManagerSetBundleEnabled      Method = "pluginManager.setBundleEnabled"
	MethodPluginManagerSetPluginEnabled      Method = "pluginManager.setPluginEnabled"
	MethodPluginManagerSetVersionExemption   Method = "pluginManager.setVersionExemption"
	MethodPluginManagerReadBundleDetail      Method = "pluginManager.readBundleDetail"
	MethodPluginManagerWaitForInstall        Method = "pluginManager.waitForInstall"
	MethodPluginManagerCancelInstall         Method = "pluginManager.cancelInstall"
)

// OfficialPluginManagerMethods 按固定顺序返回本文件实现的 12 个方法。
//
// 它给快照/文档/门禁用：新增或删除一个方法必须显式改这里，不能让
// "方法悄悄多了一个"从路由表里漏过去。
func OfficialPluginManagerMethods() []Method {
	return []Method{
		MethodPluginManagerCancelInstall,
		MethodPluginManagerInspect,
		MethodPluginManagerInstallBundle,
		MethodPluginManagerListVersionExemptions,
		MethodPluginManagerListVersions,
		MethodPluginManagerReadBundleDetail,
		MethodPluginManagerRemoveBundle,
		MethodPluginManagerSetBundleEnabled,
		MethodPluginManagerSetPluginEnabled,
		MethodPluginManagerSetVersionExemption,
		MethodPluginManagerUpdateBundle,
		MethodPluginManagerWaitForInstall,
	}
}

// IsOfficialPluginManagerMethod 判断一个 /api 路径名是否属于本文件的 12 个方法。
//
// 路由侧只需要这一次判断（**不要**在 server.go 里摊开 12 个 case：那样每加一个
// 官方方法都要动一次早已冻结的路由表）。
func IsOfficialPluginManagerMethod(name string) bool {
	switch Method(name) {
	case MethodPluginManagerCancelInstall, MethodPluginManagerInspect,
		MethodPluginManagerInstallBundle, MethodPluginManagerListVersionExemptions,
		MethodPluginManagerListVersions,
		MethodPluginManagerReadBundleDetail, MethodPluginManagerRemoveBundle,
		MethodPluginManagerSetBundleEnabled, MethodPluginManagerSetPluginEnabled,
		MethodPluginManagerSetVersionExemption, MethodPluginManagerUpdateBundle,
		MethodPluginManagerWaitForInstall:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 挂载点
// ---------------------------------------------------------------------------

// OfficialPluginManagerAPI 是宿主侧的真实实现集合。**每个字段都可以为 nil**：
// 为 nil 表示这个能力在**当前宿主**上没有真实支撑，调用回 not_implemented
// 并说清缺什么——不是静默成功，也不是假数据。
//
// 用法（Lead 接线，见 RESULT）：
//
//	srv.AttachOfficialPluginManagerAPI(official.New(pluginManager))
type OfficialPluginManagerAPI struct {
	Inspect             func(OfficialInspectParams) (OfficialPluginSpecInspection, error)
	ListVersions        func(OfficialListVersionsParams) (OfficialVersionList, error)
	InstallBundle       func(OfficialInstallBundleParams) (OfficialChangeResult, error)
	UpdateBundle        func(OfficialUpdateBundleParams) (OfficialUpdateResult, error)
	RemoveBundle        func(OfficialBundleNameParams) (OfficialChangeResult, error)
	SetBundleEnabled    func(OfficialSetEnabledParams) (OfficialChangeResult, error)
	SetPluginEnabled    func(OfficialSetEnabledParams) (OfficialChangeResult, error)
	SetVersionExemption func(OfficialVersionExemptionParams) (OfficialChangeResult, error)
	// ListVersionExemptions 与 SetVersionExemption 读写**同一份**
	// compatibility.json：授权之后重开管理器仍然读得到，坏文件按既有语义
	// 降级成"没有豁免 + warnings"（见 internal/pluginmgr/exemption.go）。
	ListVersionExemptions func() (OfficialVersionExemptions, error)
	ReadBundleDetail      func(OfficialReadBundleDetailParams) (OfficialBundleDetail, error)
	WaitForInstall        func(OfficialRequestIDParams) (OfficialInstallWait, error)
	CancelInstall         func(OfficialRequestIDParams) (OfficialInstallCancellation, error)
}

// officialPluginManagerMissing 是"字段为 nil 时缺的到底是什么"的唯一说法。
//
// 每个方法都必须有一句**可执行的**缺失说明：客户端据此决定是禁用入口还是
// 提示用户，而不是对着一句 "not implemented" 猜。
var officialPluginManagerMissing = map[Method]string{
	MethodPluginManagerInspect:               "宿主没有 registry/pnpm/git/tarball 解析器（本仓只有本地目录包）",
	MethodPluginManagerListVersions:          "宿主没有 registry 客户端（pnpm view 等价物），也没有本地发布版本源",
	MethodPluginManagerInstallBundle:         "宿主没有 bundle 下载源（registry/pnpm/git/tarball），也没有安装事务",
	MethodPluginManagerUpdateBundle:          "宿主没有 registry 下载源，也没有可原子切版的事务",
	MethodPluginManagerRemoveBundle:          "宿主没有可回滚的卸载事务",
	MethodPluginManagerSetBundleEnabled:      "宿主没有持久化的启用位",
	MethodPluginManagerSetPluginEnabled:      "宿主没有持久化的启用位",
	MethodPluginManagerSetVersionExemption:   "宿主没有挂接版本豁免存储（compatibility.json）；本仓的 internal/pluginmgr/official 已实现，缺的是装配",
	MethodPluginManagerListVersionExemptions: "宿主没有挂接版本豁免存储（compatibility.json）；本仓的 internal/pluginmgr/official 已实现，缺的是装配",
	MethodPluginManagerReadBundleDetail:      "宿主没有已安装包目录，读不到包内文档",
	MethodPluginManagerWaitForInstall:        "宿主没有安装作业登记表",
	MethodPluginManagerCancelInstall:         "宿主没有可取消的安装作业",
}

// officialPluginManagerMount 把一份实现挂到一个 Server 上。
//
// 按 *Server 索引而不是全局单例：A/B 两个宿主各挂各的（两个 home、两份状态），
// 互不可见。实现方用完可以 Detach（测试里都 defer 了）。
var officialPluginManagerMount = struct {
	mu sync.RWMutex
	m  map[*Server]OfficialPluginManagerAPI
}{m: map[*Server]OfficialPluginManagerAPI{}}

// AttachOfficialPluginManagerAPI 给这个 Server 挂上官方 pluginManager 的真实实现。
// 必须发生在服务开始接受 /api 调用之前（serve 的装配阶段）。
func (s *Server) AttachOfficialPluginManagerAPI(api OfficialPluginManagerAPI) {
	if s == nil {
		return
	}
	officialPluginManagerMount.mu.Lock()
	defer officialPluginManagerMount.mu.Unlock()
	officialPluginManagerMount.m[s] = api
}

// DetachOfficialPluginManagerAPI 摘掉实现（测试收尾用；生产不需要调）。
func (s *Server) DetachOfficialPluginManagerAPI() {
	if s == nil {
		return
	}
	officialPluginManagerMount.mu.Lock()
	defer officialPluginManagerMount.mu.Unlock()
	delete(officialPluginManagerMount.m, s)
}

func (s *Server) officialPluginManagerAPI() (OfficialPluginManagerAPI, bool) {
	if s == nil {
		return OfficialPluginManagerAPI{}, false
	}
	officialPluginManagerMount.mu.RLock()
	defer officialPluginManagerMount.mu.RUnlock()
	api, ok := officialPluginManagerMount.m[s]
	return api, ok
}

// ServeOfficialPluginManagerAPI 是这 11 个方法在 `/api` 上的唯一入口。
//
// 调用方（server.go 的 handleAPI）只需在既有 switch 之前判断
// `IsOfficialPluginManagerMethod(name)` 并调用它；认证、`POST` 校验、失败日志
// 都已经在 handleAPI 里做过了。
func (s *Server) ServeOfficialPluginManagerAPI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/")
	if !IsOfficialPluginManagerMethod(name) {
		// 接错路由就当场失败：宁可 404，也不要让一个非官方方法走到这里
		// 拿到"空成功"。
		writeJSON(w, http.StatusNotFound, errorBodyFor("not_found",
			fmt.Sprintf("%q 不是官方 pluginManager 方法", name)))
		return
	}
	s.serveOfficialPluginManagerAPI(w, r, name)
}

func (s *Server) serveOfficialPluginManagerAPI(w http.ResponseWriter, r *http.Request, name string) {
	api, ok := s.officialPluginManagerAPI()
	if !ok {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
			"官方 pluginManager API 未接线：宿主没有 AttachOfficialPluginManagerAPI"))
		return
	}
	params, err := decodeOfficialParams(r)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	result, err := routeOfficialPluginManagerMethod(api, Method(name), params)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	if result == nil {
		// 本仓成功出口不接受 nil（会被写成 500）。这些方法都有载荷，
		// 走到这里说明宿主返回了零值：用空对象表达"没有载荷"，
		// 而不是让客户端把 null 当成功。
		result = map[string]any{}
	}
	writeJSON(w, http.StatusOK, result)
}

// routeOfficialPluginManagerMethod 是一个大 switch，放在本文件里而不是
// server.go：官方方法还会继续加，路由表却是冻结的。
func routeOfficialPluginManagerMethod(api OfficialPluginManagerAPI, method Method, p *officialParams) (any, error) {
	switch method {
	case MethodPluginManagerInspect:
		return officialPluginInspect(api, p)
	case MethodPluginManagerListVersions:
		return officialPluginListVersions(api, p)
	case MethodPluginManagerInstallBundle:
		return officialPluginInstallBundle(api, p)
	case MethodPluginManagerUpdateBundle:
		return officialPluginUpdateBundle(api, p)
	case MethodPluginManagerRemoveBundle:
		return officialPluginRemoveBundle(api, p)
	case MethodPluginManagerSetBundleEnabled:
		return officialPluginSetEnabled(api, method, p)
	case MethodPluginManagerSetPluginEnabled:
		return officialPluginSetEnabled(api, method, p)
	case MethodPluginManagerSetVersionExemption:
		return officialPluginSetVersionExemption(api, p)
	case MethodPluginManagerListVersionExemptions:
		return officialPluginListVersionExemptions(api, p)
	case MethodPluginManagerReadBundleDetail:
		return officialPluginReadBundleDetail(api, p)
	case MethodPluginManagerWaitForInstall:
		return officialPluginWaitForInstall(api, p)
	case MethodPluginManagerCancelInstall:
		return officialPluginCancelInstall(api, p)
	}
	return nil, officialNotImplemented("%s 未实现", method)
}

func officialPluginMissing(method Method) error {
	text := officialPluginManagerMissing[method]
	if text == "" {
		text = "宿主没有这个能力的真实支撑"
	}
	return officialNotImplemented("%s 未接线：%s", method, text)
}

// ---------------------------------------------------------------------------
// 参数读取：顶层参数 + 官方 `options?` 对象
// ---------------------------------------------------------------------------

// pmParams 是"顶层裸参数 + 可选 options 对象"的统一读取口。
//
// 官方这些方法的签名是位置参数，其中 options 是一个可选对象。本仓 wire 是
// 一个裸 JSON 对象，因此两种写法都认：
//
//	{"spec":"…","options":{"enabled":true}}
//	{"spec":"…","enabled":true}
//
// 同一个键两处都给 → 400（不猜哪个是真的，与 handlers_agent_official.go
// 对 `ref`/`request` 的处理一致）。
type pmParams struct {
	owner  string
	top    *officialParams
	nested *officialParams
}

// pmNewParams 解析顶层参数，并在 `options` 出现时把它拆成嵌套对象。
func pmNewParams(p *officialParams, owner string) (*pmParams, error) {
	params := &pmParams{owner: owner, top: p}
	raw, ok := p.take(owner)
	if !ok {
		return params, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, officialBadRequest("%s 必须是 JSON 对象: %v", owner, err)
	}
	if m == nil {
		return nil, officialBadRequest("%s 是 null：要么不给，要么给一个对象", owner)
	}
	params.nested = &officialParams{m: m, used: map[string]bool{}}
	return params, nil
}

func (pm *pmParams) raw(name string) (json.RawMessage, bool, error) {
	var nestedRaw, topRaw json.RawMessage
	var hasNested, hasTop bool
	if pm.nested != nil {
		nestedRaw, hasNested = pm.nested.take(name)
	}
	topRaw, hasTop = pm.top.take(name)
	switch {
	case hasNested && hasTop:
		return nil, false, officialBadRequest("%s.%s 与顶层 %s 同时出现：不猜哪个是真的", pm.owner, name, name)
	case hasNested:
		return nestedRaw, true, nil
	case hasTop:
		return topRaw, true, nil
	}
	return nil, false, nil
}

// finish 在解析结束时调用：嵌套对象与顶层都不许有没用到的键。
func (pm *pmParams) finish() error {
	if pm.nested != nil {
		if err := pm.nested.finish(); err != nil {
			return officialBadRequest("%s 里%s", pm.owner, strings.TrimPrefix(err.Error(), "不认识的字段 "))
		}
	}
	return pm.top.finish()
}

func (pm *pmParams) requiredString(name string) (string, error) {
	value, has, err := pm.optionalString(name)
	if err != nil {
		return "", err
	}
	if !has {
		return "", officialBadRequest("缺 %s", name)
	}
	return value, nil
}

func (pm *pmParams) optionalString(name string) (string, bool, error) {
	raw, has, err := pm.raw(name)
	if err != nil || !has {
		return "", has, err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, officialBadRequest("%s 必须是字符串", name)
	}
	return value, true, nil
}

// nonEmptyString 取必填且去掉空白后非空的字符串。
func (pm *pmParams) nonEmptyString(name string) (string, error) {
	value, err := pm.requiredString(name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", officialBadRequest("%s 是空串", name)
	}
	return value, nil
}

// optionalNullableString 区分"没给"（has=false）、"给了 null"（ptr=nil, has=true）
// 与"给了字符串"——官方的 `registry?: string | null` 就是这三种。
func (pm *pmParams) optionalNullableString(name string) (*string, bool, error) {
	raw, has, err := pm.raw(name)
	if err != nil || !has {
		return nil, has, err
	}
	if string(bytesTrimSpace(raw)) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, officialBadRequest("%s 必须是字符串或 null", name)
	}
	if strings.TrimSpace(value) == "" {
		return nil, false, officialBadRequest("%s 是空串：要么给 registry URL，要么给 null", name)
	}
	return &value, true, nil
}

func (pm *pmParams) optionalBool(name string) (*bool, bool, error) {
	raw, has, err := pm.raw(name)
	if err != nil || !has {
		return nil, has, err
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, officialBadRequest("%s 必须是布尔", name)
	}
	return &value, true, nil
}

func (pm *pmParams) requiredBool(name string) (bool, error) {
	value, has, err := pm.optionalBool(name)
	if err != nil {
		return false, err
	}
	if !has || value == nil {
		return false, officialBadRequest("缺 %s", name)
	}
	return *value, nil
}

func (pm *pmParams) optionalStringArray(name string) ([]string, bool, error) {
	raw, has, err := pm.raw(name)
	if err != nil || !has {
		return nil, has, err
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false, officialBadRequest("%s 必须是字符串数组", name)
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, false, officialBadRequest("%s 里有空串", name)
		}
	}
	return values, true, nil
}

// optionalRequestID 取可选的 requestId：给了就必须非空（空串没法命名作业）。
func (pm *pmParams) optionalRequestID(name string) (*string, error) {
	value, has, err := pm.optionalString(name)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, nil
	}
	if strings.TrimSpace(value) == "" {
		return nil, officialBadRequest("%s 是空串：要么不给，要么给一个能命名安装作业的 id", name)
	}
	return &value, nil
}

func bytesTrimSpace(raw json.RawMessage) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

// ---------------------------------------------------------------------------
// 参数校验
// ---------------------------------------------------------------------------

var (
	// pmSemverPattern 与 pluginmgr 的 parseSemver 同口径（核心三段 + 可选 pre/build）。
	pmSemverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	// pmLanguagePattern 是官方 `language`（如 zh-CN / en / pt-BR）的形状。
	pmLanguagePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
)

// pmNpmFileSources 是官方允许的 npm 文件 CDN 白名单。
var pmNpmFileSources = map[string]bool{"jsdelivr": true, "unpkg": true, "npmmirror": true}

// ---------------------------------------------------------------------------
// 官方数据结构（字段名/可选性逐条照 zod 契约）
// ---------------------------------------------------------------------------

// OfficialChangeResult 是官方 `ChangeResult`：所有写方法的统一结果。
//
// 本仓不跑 pnpm，所以官方里那些只在一次 pnpm 运行后才有的键
// （packageResult / pendingBuilds / approvedBuilds / registries / failedAt /
// incompatible）在这里**不出现**——它们缺省即"没有发生过 pnpm 运行"，
// 不是被吞掉的失败。
type OfficialChangeResult struct {
	Changed       bool                 `json:"changed"`
	Application   string               `json:"application"`
	RestartReason string               `json:"restartReason,omitempty"`
	Stage         string               `json:"stage"`
	Target        string               `json:"target"`
	Enabled       *bool                `json:"enabled,omitempty"`
	Error         *OfficialChangeError `json:"error,omitempty"`
	Warnings      []string             `json:"warnings,omitempty"`
	Bundle        string               `json:"bundle,omitempty"`
	Version       string               `json:"version,omitempty"`
}

// OfficialChangeError 是官方 `ChangeResult.error`（管理性失败）。
type OfficialChangeError struct {
	Code       string `json:"code"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

// OfficialUpdateResult 是官方 `PluginUpdateResult`。
type OfficialUpdateResult struct {
	From    string                `json:"from"`
	To      string                `json:"to"`
	Step    string                `json:"step"`
	Result  OfficialChangeResult  `json:"result"`
	Restore *OfficialChangeResult `json:"restore,omitempty"`
}

// OfficialPluginSpecInspection 是官方 `PluginSpecInspection` 的联合：
// `accepted` 分支带 kind/bundle/registry，`refused` 分支带 problem/reason。
type OfficialPluginSpecInspection struct {
	Status  string `json:"status"`
	Kind    string `json:"kind,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	// Description 来自包清单；本仓 manifest 不声明它，因此通常缺省。
	Description string `json:"description,omitempty"`
	// Bundle 是 `null|false|true`：本仓包没有 patch，恒为 false。
	Bundle json.RawMessage `json:"bundle,omitempty"`
	// Registry 是 `null|string`：accepted 时必须显式出现。
	Registry   json.RawMessage   `json:"registry,omitempty"`
	Host       string            `json:"host,omitempty"`
	Problem    string            `json:"problem,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Registries []json.RawMessage `json:"registries,omitempty"`
}

// OfficialRegistryAttempt 是一次 registry 询问的记账（官方 attempts 元素）。
type OfficialRegistryAttempt struct {
	Registry json.RawMessage `json:"registry"`
	Problem  string          `json:"problem,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}

// OfficialIncompatiblePackage 是官方 `incompatible` 元素（版本被运行版本拒绝）。
type OfficialIncompatiblePackage struct {
	Name           string            `json:"name"`
	Version        string            `json:"version"`
	RuntimeVersion string            `json:"runtimeVersion"`
	Requirements   map[string]string `json:"requirements"`
}

// OfficialPublishedVersion 是官方版本列表的一行。
type OfficialPublishedVersion struct {
	Version      string                       `json:"version"`
	PublishedAt  string                       `json:"publishedAt,omitempty"`
	Tags         []string                     `json:"tags"`
	Prerelease   bool                         `json:"prerelease"`
	Deprecated   string                       `json:"deprecated,omitempty"`
	Bundle       bool                         `json:"bundle"`
	Incompatible *OfficialIncompatiblePackage `json:"incompatible,omitempty"`
}

// OfficialVersionList 是官方 `PluginVersionList` 的联合。
type OfficialVersionList struct {
	Status    string          `json:"status"`
	Installed string          `json:"installed,omitempty"`
	Registry  json.RawMessage `json:"registry,omitempty"`
	// Attempts 必须是数组（空数组表示"一个 registry 都没问"）。
	Attempts []OfficialRegistryAttempt  `json:"attempts"`
	Versions []OfficialPublishedVersion `json:"versions,omitempty"`
	Omitted  *int                       `json:"omitted,omitempty"`
	Problem  string                     `json:"problem,omitempty"`
	Reason   string                     `json:"reason,omitempty"`
}

// OfficialBundleDetail 是官方 `BundleDetail` 的联合（found | none）。
type OfficialBundleDetail struct {
	Status   string `json:"status"`
	Origin   string `json:"origin,omitempty"`
	Path     string `json:"path,omitempty"`
	Markdown string `json:"markdown,omitempty"`
}

// OfficialInstallWait 承载官方 `waitForInstall` 的 `ChangeResult | null`。
//
// **与官方 wire 的显式差异**：本仓成功出口不允许 `{"result":null}`（出口会把
// null 变成 500），所以"没有这个在飞安装"用 `{"active":false}` 表达；客户端
// 判 `active===false` 即官方的 null，且不会把空载荷当成功。
type OfficialInstallWait struct {
	Active bool                  `json:"active"`
	Result *OfficialChangeResult `json:"result,omitempty"`
}

// OfficialInstallCancellation 是官方 `PluginInstallCancellation`。
type OfficialInstallCancellation struct {
	Status string `json:"status"`
}

// OfficialVersionExemptions 是官方 `listVersionExemptions` 的结果：
//
//	{ exemptions: Record<string, string[]>; warnings: string[] }
//
// （typert.host.js:178-183；`parameters: []`，零参数。）
//
// 两个键在官方 zod 里都是**必填**，所以缺省时也必须编成 `{}` / `[]`，
// 不能是 null——客户端会把 null 当"没有这个字段"。**被读者拒掉的记录不进
// exemptions**，只在 warnings 里如实出现（官方同一分支：读不了/坏 JSON 不
// 抛错，回空豁免 + 警告，让用户先手工修）。
type OfficialVersionExemptions struct {
	Exemptions map[string][]string `json:"exemptions"`
	Warnings   []string            `json:"warnings"`
}

// ---------------------------------------------------------------------------
// 结果构造器（形状只在这里拼，方法体里不许手捏 map）
// ---------------------------------------------------------------------------

// 宿主适配器（internal/pluginmgr/official）在包外，拿不到本包的内部错误类型；
// 这三个薄封装让它能给出**精确**状态码，而不是把所有失败都塌成 500。
// 它们只委托既有的 `officialBadRequest/officialNotFound/officialRevisionConflict`，
// 不新造错误码。
func OfficialBadRequest(format string, args ...any) error {
	return officialBadRequest(format, args...)
}

func OfficialNotFound(format string, args ...any) error {
	return officialNotFound(format, args...)
}

func OfficialConflict(format string, args ...any) error {
	return officialRevisionConflict(format, args...)
}

// OfficialNoRuntimeLoaderWarning 是每条 applied 结果都带的真实边界说明。
//
// docs/host-plugin-manager.md §1 写明了本仓不执行也不动态加载包入口；把它写进
// 结果的 warnings，客户端就不会把 enabled=true 误读成"插件已经在跑"。
const OfficialNoRuntimeLoaderWarning = "本仓没有运行时插件加载器：本次只持久化了 profile 状态（state.json），包入口不会被加载或执行"

// OfficialAppliedChange 造一条"真实状态已改"的结果。
func OfficialAppliedChange(stage, target string, enabled bool) OfficialChangeResult {
	value := enabled
	return OfficialChangeResult{
		Changed: true, Application: "applied", Stage: stage, Target: target,
		Enabled: &value, Warnings: []string{OfficialNoRuntimeLoaderWarning},
	}
}

// OfficialCancelledChange 造一条"取消且已回滚"的结果。
func OfficialCancelledChange(stage, target string) OfficialChangeResult {
	return OfficialChangeResult{Changed: false, Application: "cancelled", Stage: stage, Target: target}
}

// OfficialFailedChange 造一条管理性失败（官方把这类失败放在结果里，而不是抛错）。
func OfficialFailedChange(stage, target, code, diagnostic string) OfficialChangeResult {
	return OfficialChangeResult{
		Changed: false, Application: "failed", Stage: stage, Target: target,
		Error: &OfficialChangeError{Code: code, Diagnostic: diagnostic},
	}
}

// OfficialInspectionAccepted 造 accepted 分支：
// bundle 如实报"有没有 patch"，registry 显式 null（本仓没有任何 registry）。
func OfficialInspectionAccepted(kind, name, version string, bundle bool) OfficialPluginSpecInspection {
	raw := json.RawMessage("false")
	if bundle {
		raw = json.RawMessage("true")
	}
	return OfficialPluginSpecInspection{
		Status: "accepted", Kind: kind, Name: name, Version: version,
		Bundle: raw, Registry: json.RawMessage("null"),
	}
}

// OfficialInspectionRefused 造 refused 分支。
func OfficialInspectionRefused(problem, reason string) OfficialPluginSpecInspection {
	return OfficialPluginSpecInspection{Status: "refused", Problem: problem, Reason: reason}
}

// OfficialVersionsRefused 造 listVersions 的 refused 分支（attempts 必为空数组）。
func OfficialVersionsRefused(problem, reason string) OfficialVersionList {
	return OfficialVersionList{
		Status: "refused", Problem: problem, Reason: reason,
		Attempts: []OfficialRegistryAttempt{},
	}
}

// OfficialNullableString 把 string 编成 `null|string`（空串即 null）。
func OfficialNullableString(value string) json.RawMessage {
	if value == "" {
		return json.RawMessage("null")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(encoded)
}

// OfficialIntPtr 取 int 指针（`omitted: 0` 也要显式出现）。
func OfficialIntPtr(value int) *int { return &value }

// ---------------------------------------------------------------------------
// 参数结构（wire 名 → 宿主入参）
// ---------------------------------------------------------------------------

// OfficialInspectParams 是 `inspect(spec, options?)` 的入参。
type OfficialInspectParams struct {
	Spec string
	// Registry 为 nil 表示"没指定/指定 null"，即用配置里的 registry。
	Registry *string
}

// OfficialListVersionsParams 是 `listVersions(name, options?)` 的入参。
type OfficialListVersionsParams struct {
	Name     string
	Registry *string
}

// OfficialInstallBundleParams 是 `installBundle(spec, options?)` 的入参。
type OfficialInstallBundleParams struct {
	Spec string
	// Enabled 为 nil 表示没给（官方默认 true，由宿主语义决定）。
	Enabled *bool
	// SaveExact 在本仓"本来就已经满足"（状态里记的是精确版本目录）。
	SaveExact      *bool
	RequestID      *string
	ApprovedBuilds []string
	Registry       *string
}

// OfficialUpdateBundleParams 是 `updateBundle(name, version, options?)` 的入参。
type OfficialUpdateBundleParams struct {
	Name      string
	Version   string
	Registry  *string
	RequestID *string
}

// OfficialBundleNameParams 是 `removeBundle(name)` 的入参。
type OfficialBundleNameParams struct {
	Name string
}

// OfficialSetEnabledParams 是 `setBundleEnabled(name, enabled)` 与
// `setPluginEnabled(id, enabled)` 的入参（两个官方方法各用各的键）。
type OfficialSetEnabledParams struct {
	Name    string
	Enabled bool
}

// OfficialVersionExemptionParams 是 `setVersionExemption(...)` 的入参。
type OfficialVersionExemptionParams struct {
	PackageVersion string
	RuntimeVersion string
	Enabled        bool
	AcceptRisk     *bool
}

// OfficialReadBundleDetailParams 是 `readBundleDetail(name, language, options?)` 的入参。
type OfficialReadBundleDetailParams struct {
	Name           string
	Language       string
	NpmFileSources []string
}

// OfficialRequestIDParams 是 `waitForInstall/cancelInstall(requestId)` 的入参。
type OfficialRequestIDParams struct {
	RequestID string
}

// ---------------------------------------------------------------------------
// 每个方法的参数解析
// ---------------------------------------------------------------------------

func officialPluginInspect(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	spec, err := pm.nonEmptyString("spec")
	if err != nil {
		return nil, err
	}
	registry, _, err := pm.optionalNullableString("registry")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if api.Inspect == nil {
		return nil, officialPluginMissing(MethodPluginManagerInspect)
	}
	return api.Inspect(OfficialInspectParams{Spec: spec, Registry: registry})
}

func officialPluginListVersions(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	name, err := pm.nonEmptyString("name")
	if err != nil {
		return nil, err
	}
	registry, _, err := pm.optionalNullableString("registry")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if api.ListVersions == nil {
		return nil, officialPluginMissing(MethodPluginManagerListVersions)
	}
	result, err := api.ListVersions(OfficialListVersionsParams{Name: name, Registry: registry})
	if err != nil {
		return nil, err
	}
	return normalizeVersionList(result), nil
}

// normalizeVersionList 把 nil 切片归一成空数组：官方契约里 `attempts` 与
// `tags` 是数组，缺键/null 会让客户端把它当"没有这个字段"。
func normalizeVersionList(list OfficialVersionList) OfficialVersionList {
	if list.Attempts == nil {
		list.Attempts = []OfficialRegistryAttempt{}
	}
	for i := range list.Versions {
		if list.Versions[i].Tags == nil {
			list.Versions[i].Tags = []string{}
		}
	}
	return list
}

func officialPluginInstallBundle(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	spec, err := pm.nonEmptyString("spec")
	if err != nil {
		return nil, err
	}
	enabled, _, err := pm.optionalBool("enabled")
	if err != nil {
		return nil, err
	}
	saveExact, _, err := pm.optionalBool("saveExact")
	if err != nil {
		return nil, err
	}
	requestID, err := pm.optionalRequestID("requestId")
	if err != nil {
		return nil, err
	}
	approvedBuilds, _, err := pm.optionalStringArray("approvedBuilds")
	if err != nil {
		return nil, err
	}
	registry, _, err := pm.optionalNullableString("registry")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if api.InstallBundle == nil {
		return nil, officialPluginMissing(MethodPluginManagerInstallBundle)
	}
	return api.InstallBundle(OfficialInstallBundleParams{
		Spec: spec, Enabled: enabled, SaveExact: saveExact, RequestID: requestID,
		ApprovedBuilds: approvedBuilds, Registry: registry,
	})
}

func officialPluginUpdateBundle(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	name, err := pm.nonEmptyString("name")
	if err != nil {
		return nil, err
	}
	version, err := pm.requiredString("version")
	if err != nil {
		return nil, err
	}
	if version == "" {
		return nil, officialBadRequest("version 是空串")
	}
	registry, _, err := pm.optionalNullableString("registry")
	if err != nil {
		return nil, err
	}
	requestID, err := pm.optionalRequestID("requestId")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if api.UpdateBundle == nil {
		return nil, officialPluginMissing(MethodPluginManagerUpdateBundle)
	}
	return api.UpdateBundle(OfficialUpdateBundleParams{
		Name: name, Version: version, Registry: registry, RequestID: requestID,
	})
}

func officialPluginRemoveBundle(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	name, err := pm.nonEmptyString("name")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if api.RemoveBundle == nil {
		return nil, officialPluginMissing(MethodPluginManagerRemoveBundle)
	}
	return api.RemoveBundle(OfficialBundleNameParams{Name: name})
}

// officialPluginSetEnabled 同时服务 setBundleEnabled(name) 与
// setPluginEnabled(id)：官方两个方法的消息形状相同，只有参数名不同。
func officialPluginSetEnabled(api OfficialPluginManagerAPI, method Method, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	nameKey := "name"
	handler := api.SetBundleEnabled
	if method == MethodPluginManagerSetPluginEnabled {
		nameKey = "id"
		handler = api.SetPluginEnabled
	}
	name, err := pm.nonEmptyString(nameKey)
	if err != nil {
		return nil, err
	}
	enabled, err := pm.requiredBool("enabled")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, officialPluginMissing(method)
	}
	return handler(OfficialSetEnabledParams{Name: name, Enabled: enabled})
}

func officialPluginSetVersionExemption(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	packageVersion, err := pm.nonEmptyString("packageVersion")
	if err != nil {
		return nil, err
	}
	at := strings.LastIndex(packageVersion, "@")
	if at <= 0 || at == len(packageVersion)-1 {
		return nil, officialBadRequest("packageVersion 必须是「包名@精确版本」，收到 %q", packageVersion)
	}
	if name, version := packageVersion[:at], packageVersion[at+1:]; strings.TrimSpace(name) == "" || !pmSemverPattern.MatchString(version) {
		return nil, officialBadRequest("packageVersion 的版本部分不是 SemVer：%q", packageVersion)
	}
	runtimeVersion, err := pm.nonEmptyString("runtimeVersion")
	if err != nil {
		return nil, err
	}
	enabled, err := pm.requiredBool("enabled")
	if err != nil {
		return nil, err
	}
	acceptRisk, _, err := pm.optionalBool("acceptRisk")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	// 官方要求"授权豁免"必须先接受风险（acceptRisk:true）；撤销不需要。
	// 这一步是**契约语义**校验，与宿主有没有这个能力无关，所以放在 501 之前。
	if enabled && (acceptRisk == nil || !*acceptRisk) {
		return nil, officialBadRequest(
			"授权 packageVersion=%s 在 runtimeVersion=%s 上运行必须显式 acceptRisk=true（接受崩溃与数据损失的风险）",
			packageVersion, runtimeVersion)
	}
	if api.SetVersionExemption == nil {
		return nil, officialPluginMissing(MethodPluginManagerSetVersionExemption)
	}
	return api.SetVersionExemption(OfficialVersionExemptionParams{
		PackageVersion: packageVersion, RuntimeVersion: runtimeVersion,
		Enabled: enabled, AcceptRisk: acceptRisk,
	})
}

// officialPluginListVersionExemptions 读当前 profile 里被接受的精确版本豁免。
//
// 官方是 `direct` 调用、零参数（typert.host.js:788-797），所以这里**不取任何
// 具名参数**：请求体给 `{}` 即可，多一个字段就是契约外的（strict 400）。
// 文件读不了/坏 JSON 不算这个方法失败——官方的读者把问题折进 warnings 并回
// 空豁免（`readProfileCompatibility`），本仓同一条语义。
func officialPluginListVersionExemptions(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.ListVersionExemptions == nil {
		return nil, officialPluginMissing(MethodPluginManagerListVersionExemptions)
	}
	result, err := api.ListVersionExemptions()
	if err != nil {
		return nil, err
	}
	return normalizeVersionExemptions(result), nil
}

// normalizeVersionExemptions 把 nil map/slice 归一成 `{}` / `[]`：官方 zod 里
// 两个键都是必填，缺键或 null 会让客户端把它当"没有这个字段"。
func normalizeVersionExemptions(value OfficialVersionExemptions) OfficialVersionExemptions {
	if value.Exemptions == nil {
		value.Exemptions = map[string][]string{}
	}
	if value.Warnings == nil {
		value.Warnings = []string{}
	}
	for key, versions := range value.Exemptions {
		if versions == nil {
			value.Exemptions[key] = []string{}
		}
	}
	return value
}

func officialPluginReadBundleDetail(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	name, err := pm.nonEmptyString("name")
	if err != nil {
		return nil, err
	}
	language, err := pm.nonEmptyString("language")
	if err != nil {
		return nil, err
	}
	if !pmLanguagePattern.MatchString(language) {
		return nil, officialBadRequest("language 必须是 zh-CN 这样的语言标签，收到 %q", language)
	}
	sources, _, err := pm.optionalStringArray("npmFileSources")
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		if !pmNpmFileSources[source] {
			return nil, officialBadRequest("npmFileSources 里有未知 CDN %q（只接受 jsdelivr/unpkg/npmmirror）", source)
		}
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if api.ReadBundleDetail == nil {
		return nil, officialPluginMissing(MethodPluginManagerReadBundleDetail)
	}
	return api.ReadBundleDetail(OfficialReadBundleDetailParams{
		Name: name, Language: language, NpmFileSources: sources,
	})
}

func officialPluginWaitForInstall(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	requestID, err := pm.nonEmptyString("requestId")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if api.WaitForInstall == nil {
		return nil, officialPluginMissing(MethodPluginManagerWaitForInstall)
	}
	return api.WaitForInstall(OfficialRequestIDParams{RequestID: requestID})
}

func officialPluginCancelInstall(api OfficialPluginManagerAPI, p *officialParams) (any, error) {
	pm, err := pmNewParams(p, "options")
	if err != nil {
		return nil, err
	}
	requestID, err := pm.nonEmptyString("requestId")
	if err != nil {
		return nil, err
	}
	if err := pm.finish(); err != nil {
		return nil, err
	}
	if api.CancelInstall == nil {
		return nil, officialPluginMissing(MethodPluginManagerCancelInstall)
	}
	return api.CancelInstall(OfficialRequestIDParams{RequestID: requestID})
}
