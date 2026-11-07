package transport

// 官方桌面端 0.2.2 的 forgePanel.*（19）与 liveshareShares.*（8）：共 27 个方法。
//
// # 契约来源（只读镜像，未改安装目录）
//
// 安装目录里没有这两个包（官方镜像 %TEMP%/official-api-src 只镜像了 19 个
// "@deepseek-ai/dsh-*" 包），所以这两个 typert.host.js 是从 app.asar
// **只读解出来**的，落在：
//
//	%TEMP%/official-api-src/dsh/node_modules/@deepseek-ai/dsh-experimental-client-ui-forge/lib/typert.host.js
//	%TEMP%/official-api-src/dsh/node_modules/@deepseek-ai/dsh-experimental-liveshare/lib/typert.host.js
//
// 解包器只读 asar（dataOffset = 8 + headerSize，header JSON 长 readUInt32LE(12)），
// 不写安装目录。逐方法位置（typert.host.js 行号 / 官方 src 行号）：
//
//	forgePanel/branches           556 / packages/experimental/client-ui-forge/src/index.ts:188
//	forgePanel/cancelWorktree     591 / …/client-ui-forge/src/index.ts:449
//	forgePanel/changeRequestChecks 625 / …/index.ts:285
//	forgePanel/changeRequestDiff  694 / …/index.ts:308
//	forgePanel/changeRequestText  763 / …/index.ts:352
//	forgePanel/continueInProject  849 / …/index.ts:530
//	forgePanel/dismiss            883 / …/index.ts:216
//	forgePanel/follow             934 / …/index.ts:158
//	forgePanel/localDiff          970 / …/index.ts:246
//	forgePanel/localText         1022 / …/index.ts:268
//	forgePanel/markReady         1091 / …/index.ts:390
//	forgePanel/merge             1142 / …/index.ts:409
//	forgePanel/refresh           1227 / …/index.ts:229
//	forgePanel/requestWorktree   1262 / …/index.ts:430
//	forgePanel/restoreWorktree   1313 / …/index.ts:499
//	forgePanel/retryWorktree     1347 / …/index.ts:478
//	forgePanel/stopCreation      1381 / …/index.ts:465
//	forgePanel/submitReview      1415 / …/index.ts:368
//	forgePanel/track             1484 / …/index.ts:201
//
//	liveshareShares/audit            232 / …/liveshare/src/host/share-remote.ts:132
//	liveshareShares/create           279 / …/share-remote.ts:57
//	liveshareShares/identity         311 / …/share-remote.ts:122
//	liveshareShares/setCapabilities  373 / …/share-remote.ts:90
//	liveshareShares/setRelay         405 / …/share-remote.ts:114
//	liveshareShares/stop             437 / …/share-remote.ts:72
//	liveshareShares/testPublish      469 / …/share-remote.ts:147
//	liveshareShares/updatePermissions 516 / …/share-remote.ts:80
//
// 官方 wire 名与参数（typert 的 `parameters[].wire`，**位置参数**；本仓 L1 收成
// 一个裸 JSON 对象，键就是 wire 名）：
//
//	branches / cancelWorktree / continueInProject / follow / refresh /
//	restoreWorktree / retryWorktree / stopCreation          → sessionId
//	changeRequestChecks                                    → sessionId, url, headSha
//	changeRequestDiff / dismiss / markReady / track         → sessionId, url
//	changeRequestText                                      → sessionId, url, commit, path
//	localDiff                                              → sessionId, scope
//	localText                                              → sessionId, scope, path
//	merge                                                  → sessionId, url, method, headSha
//	requestWorktree                                        → sessionId, from（string|null）
//	submitReview                                           → sessionId, url, request（union）
//	liveshareShares.*                                      → request（对象，字段见下表）
//
// # 为什么这 27 个全部是"结构化未实现"，而不是本地实现
//
// 官方把这些能力放在两个**独立部署面**上，本仓没有任何一份真值：
//
//   - forgePanel 的 inspection/worktree 依赖官方 `worktrees` 服务、forge registry、
//     GitHub provider 与 `gh auth login` 登录态；`localDiff`/`localText` 还要求
//     "用会话的 sandbox 模式执行 git"（官方 README §Inspections 与 §localDiff：
//     read-only/workspace-write 必须走沙箱后端）。本仓唯一的 git 读取在
//     modules/tool/repohere（工具层），Transport 按红线 R7 不 import modules、
//     也不在本层 fork git；`s.opt` 里没有注入 git/sandbox/worktree 服务的字段，
//     而 server.go 不在本任务可写范围。硬做只能自造"分支列表/补丁文本"。
//   - liveshareShares 的每一条都要连 Liveshare relay：owner 凭据
//     （owner-credential.json / LIVESHARE_OWNER_CREDENTIAL_FILE）、relay 地址
//     （owner-shares.json）、OIDC 身份（LIVESHARE_OWNER_PROVIDER_ID/TENANT_ID/
//     SUBJECT_ID）与 relay 侧的审计/注销接口。这些服务"separately owned"
//     （官方 README §Boundaries），本仓没有 relay 端口，也没有 owner 状态文件。
//
// 所以按任务口径第 3 条：**分不清"真没有"和"没后端"时，返回未实现**，
// 不用空数组/空对象冒充"没有数据"，也不返回假的 void 成功。
//
// # 边界
//
// 做：官方参数名的 fail-loud 校验（缺字段/未知字段/类型/枚举都 400）、
// 四字段错误契约、逐方法"缺什么服务与凭据"的可读说明。
//
// 不做：不返回任何成功载荷、不缓存、不写盘、不 import 任何服务实现。
//
// # 接线（Lead 的验收项）
//
// server.go 的 handleAPI 里，在既有 family 判断之后、`switch Method(name)` 之前加：
//
//	if IsOfficialForgeLiveshareMethod(name) {
//		s.serveOfficialForgeLiveshareAPI(w, r, name)
//		return
//	}
//
// 加之前这 27 个名字落到 switch 的 default：501 + 通用 "not implemented"
// （能调，但既不说缺什么、也不校验参数）。加之后回逐方法的 501/400。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// ---------------------------------------------------------------------------
// 27 个方法
// ---------------------------------------------------------------------------

const (
	MethodForgePanelBranches            Method = "forgePanel.branches"
	MethodForgePanelCancelWorktree      Method = "forgePanel.cancelWorktree"
	MethodForgePanelChangeRequestChecks Method = "forgePanel.changeRequestChecks"
	MethodForgePanelChangeRequestDiff   Method = "forgePanel.changeRequestDiff"
	MethodForgePanelChangeRequestText   Method = "forgePanel.changeRequestText"
	MethodForgePanelContinueInProject   Method = "forgePanel.continueInProject"
	MethodForgePanelDismiss             Method = "forgePanel.dismiss"
	MethodForgePanelFollow              Method = "forgePanel.follow"
	MethodForgePanelLocalDiff           Method = "forgePanel.localDiff"
	MethodForgePanelLocalText           Method = "forgePanel.localText"
	MethodForgePanelMarkReady           Method = "forgePanel.markReady"
	MethodForgePanelMerge               Method = "forgePanel.merge"
	MethodForgePanelRefresh             Method = "forgePanel.refresh"
	MethodForgePanelRequestWorktree     Method = "forgePanel.requestWorktree"
	MethodForgePanelRestoreWorktree     Method = "forgePanel.restoreWorktree"
	MethodForgePanelRetryWorktree       Method = "forgePanel.retryWorktree"
	MethodForgePanelStopCreation        Method = "forgePanel.stopCreation"
	MethodForgePanelSubmitReview        Method = "forgePanel.submitReview"
	MethodForgePanelTrack               Method = "forgePanel.track"

	MethodLiveshareSharesAudit             Method = "liveshareShares.audit"
	MethodLiveshareSharesCreate            Method = "liveshareShares.create"
	MethodLiveshareSharesIdentity          Method = "liveshareShares.identity"
	MethodLiveshareSharesSetCapabilities   Method = "liveshareShares.setCapabilities"
	MethodLiveshareSharesSetRelay          Method = "liveshareShares.setRelay"
	MethodLiveshareSharesStop              Method = "liveshareShares.stop"
	MethodLiveshareSharesTestPublish       Method = "liveshareShares.testPublish"
	MethodLiveshareSharesUpdatePermissions Method = "liveshareShares.updatePermissions"
)

// OfficialForgeLiveshareMethods 按官方命名空间分组返回这 27 个方法（顺序固定，
// 与 docs/evidence/official-desktop-api-methods-0.2.2-rc.1.txt 的字母序一致）。
//
// 它给快照/文档/门禁用：新增或删除一个方法必须显式改这里。
func OfficialForgeLiveshareMethods() []Method {
	return []Method{
		MethodForgePanelBranches, MethodForgePanelCancelWorktree,
		MethodForgePanelChangeRequestChecks, MethodForgePanelChangeRequestDiff,
		MethodForgePanelChangeRequestText, MethodForgePanelContinueInProject,
		MethodForgePanelDismiss, MethodForgePanelFollow,
		MethodForgePanelLocalDiff, MethodForgePanelLocalText,
		MethodForgePanelMarkReady, MethodForgePanelMerge,
		MethodForgePanelRefresh, MethodForgePanelRequestWorktree,
		MethodForgePanelRestoreWorktree, MethodForgePanelRetryWorktree,
		MethodForgePanelStopCreation, MethodForgePanelSubmitReview,
		MethodForgePanelTrack,
		MethodLiveshareSharesAudit, MethodLiveshareSharesCreate,
		MethodLiveshareSharesIdentity, MethodLiveshareSharesSetCapabilities,
		MethodLiveshareSharesSetRelay, MethodLiveshareSharesStop,
		MethodLiveshareSharesTestPublish, MethodLiveshareSharesUpdatePermissions,
	}
}

// IsOfficialForgeLiveshareMethod 判断一个 /api 路径名是否属于本文件的 27 个方法。
//
// 路由侧只需要这一次判断（不要在 server.go 里摊开 27 个 case）。
func IsOfficialForgeLiveshareMethod(name string) bool {
	_, ok := forgeLiveshareSpecOf(Method(name))
	return ok
}

// ---------------------------------------------------------------------------
// 参数表（官方 wire 名 + 形状）
// ---------------------------------------------------------------------------

// forgeParamKind 是参数校验需要区分的几种形状。
//
// 只列这几种：其余形状（数组、数字）这 27 个方法根本不收；多一种就多一处
// "看起来校验了"的空转。
type forgeParamKind int

const (
	// forgeParamString：必填、非空字符串（sessionId/url/headSha/commit/path）。
	forgeParamString forgeParamKind = iota
	// forgeParamNullableString：必填，可以是 null（`from`、`relayUrl`）。
	forgeParamNullableString
	// forgeParamScope：`{kind: branch|uncommitted|unstaged|staged}`。
	forgeParamScope
	// forgeParamMergeMethod：merge|squash|rebase。
	forgeParamMergeMethod
	// forgeParamReviewRequest：submitReview 的 union（comment | review）。
	forgeParamReviewRequest
	// forgeParamObject：liveshareShares 的 `request` 对象。
	forgeParamObject
)

// forgeParam 是一个官方位置参数在 L1 上的样子。
type forgeParam struct {
	wire string
	kind forgeParamKind
	// inner 是 `request` 对象里**必填**的字段（其余键按官方
	// additionalProperties:true 放过）。
	inner []forgeParam
}

// forgeMethodSpec 是一个方法"要什么参数"与"缺什么服务"的静态描述。
//
// 它同时是：参数校验的输入、未实现错误的正文来源、报告里那 27 行的数据源。
// 三者用同一份表，避免文档说需要 A、代码说需要 B。
type forgeMethodSpec struct {
	method Method
	// official 是官方契约位置（typert.host.js 行号 / src file:line）。
	official string
	params   []forgeParam
	// need 是"要真实现必须有的服务与凭据"。
	need string
	// note 是本仓为什么现在给不出真值（一句话，带证据口径）。
	note string
}

// forgeLiveshareSpecs 是这 27 个方法的唯一参数/缺口表。
func forgeLiveshareSpecs() []forgeMethodSpec {
	const github = "GitHub provider（官方 dsh-experimental-forge 的 github 实现）与 gh 登录态（`gh auth login` 或 GITHUB_TOKEN）"
	const worktree = "官方 worktrees 服务（@deepseek-ai/dsh-worktree）与 worktree 请求投影 forgeWorktree"
	const binding = "forgeSession 绑定存储（Session ↔ change request URL）"
	const localGit = "本机 git 读取服务（Session 的 cwd + sandbox 模式 + 受控 git 调用），本仓只在 modules/tool/repohere 里有 git，Transport 按 R7 不 import modules"
	const relay = "Liveshare relay 地址（owner-shares.json 里保存的地址）+ owner 凭据（owner-credential.json / LIVESHARE_OWNER_CREDENTIAL_FILE）+ OIDC 身份（LIVESHARE_OWNER_PROVIDER_ID / TENANT_ID / SUBJECT_ID）"
	const relayOwned = "relay 与它的审计/注销接口属于单独部署的 Liveshare server，不在本仓"

	sessionOnly := []forgeParam{{wire: "sessionId", kind: forgeParamString}}
	withURL := []forgeParam{
		{wire: "sessionId", kind: forgeParamString},
		{wire: "url", kind: forgeParamString},
	}
	withHead := []forgeParam{
		{wire: "sessionId", kind: forgeParamString},
		{wire: "url", kind: forgeParamString},
		{wire: "headSha", kind: forgeParamString},
	}
	withScope := []forgeParam{
		{wire: "sessionId", kind: forgeParamString},
		{wire: "scope", kind: forgeParamScope},
	}
	withScopePath := []forgeParam{
		{wire: "sessionId", kind: forgeParamString},
		{wire: "scope", kind: forgeParamScope},
		{wire: "path", kind: forgeParamString},
	}
	shareID := []forgeParam{{wire: "request", kind: forgeParamObject, inner: []forgeParam{
		{wire: "shareId", kind: forgeParamString},
	}}}
	shareSession := []forgeParam{{wire: "request", kind: forgeParamObject, inner: []forgeParam{
		{wire: "sessionId", kind: forgeParamString},
	}}}

	return []forgeMethodSpec{
		{
			method:   MethodForgePanelBranches,
			official: "typert.host.js:556 / client-ui-forge/src/index.ts:188",
			params:   sessionOnly,
			need:     "本机 git 分支枚举（symbolic-ref --short HEAD + for-each-ref，按最近提交排序）以及 " + worktree,
			note:     localGit,
		},
		{
			method:   MethodForgePanelCancelWorktree,
			official: "typert.host.js:591 / client-ui-forge/src/index.ts:449",
			params:   sessionOnly,
			need:     "worktree 请求记录与等待中的 inbox hold（撤回请求后放行消息；本方法官方口径本身不需要 worktrees 服务，但需要那条记录）",
			note:     "本仓没有 plugin:client-ui-forge/worktree 这条会话记录，也就没有可撤回的请求；返回 success 会把\"没有这个功能\"说成\"已经撤回\"",
		},
		{
			method:   MethodForgePanelChangeRequestChecks,
			official: "typert.host.js:625 / client-ui-forge/src/index.ts:285",
			params:   withHead,
			need:     github + "；checks 必须是 code host 在 headSha 上算出的完整快照",
			note:     "本仓没有任何 code host 客户端，checks 只能靠合并状态/常量编出来",
		},
		{
			method:   MethodForgePanelChangeRequestDiff,
			official: "typert.host.js:694 / client-ui-forge/src/index.ts:308",
			params:   withURL,
			need:     github + " 的 diff 读取（按 binding 最新 status 的 head commit 取补丁）",
			note:     "本仓没有 provider，也没有 change request 绑定，编不出 host 计算的补丁",
		},
		{
			method:   MethodForgePanelChangeRequestText,
			official: "typert.host.js:763 / client-ui-forge/src/index.ts:352",
			params: []forgeParam{
				{wire: "sessionId", kind: forgeParamString},
				{wire: "url", kind: forgeParamString},
				{wire: "commit", kind: forgeParamString},
				{wire: "path", kind: forgeParamString},
			},
			need: github + " 的单文件读取（在 commit 上取 path，用来补 diff 的未变更行）",
			note: "本仓既没有 provider 也没有远端 commit，本地读只会把当前工作树当成那个 commit",
		},
		{
			method:   MethodForgePanelContinueInProject,
			official: "typert.host.js:849 / client-ui-forge/src/index.ts:530",
			params:   sessionOnly,
			need:     "working-directory 服务与 Agent maintenance（把会话从 worktree 移回源 checkout）",
			note:     "本仓没有 worktree，也没有 maintenance 通道；回 success 等于假装搬迁成功",
		},
		{
			method:   MethodForgePanelDismiss,
			official: "typert.host.js:883 / client-ui-forge/src/index.ts:216",
			params:   withURL,
			need:     binding + "（按 URL 隐藏一条绑定，别的 URL 不改会话）",
			note:     "本仓没有绑定存储；空绑定下 success 与\"这一条本来就不存在\"无法区分",
		},
		{
			method:   MethodForgePanelFollow,
			official: "typert.host.js:934 / client-ui-forge/src/index.ts:158",
			params:   sessionOnly,
			need:     "forge panel 状态流（ForgePanelState：先当前状态再后续状态）+ session query + forge registry",
			note:     "本仓没有 panel 状态机；开一条只发空帧的流就是假流式",
		},
		{
			method:   MethodForgePanelLocalDiff,
			official: "typert.host.js:970 / client-ui-forge/src/index.ts:246",
			params:   withScope,
			need:     localGit + "；还要 fork point（worktree baseCommit，或远端默认分支的 merge-base）与 ctx.fs 读未跟踪文件",
			note:     "本仓 Transport 不跑 git、没有 sandbox 模式信息，branch scope 连 fork point 都定不出来",
		},
		{
			method:   MethodForgePanelLocalText,
			official: "typert.host.js:1022 / client-ui-forge/src/index.ts:268",
			params:   withScopePath,
			need:     localGit + "（Staged 读 index 的 `show :<path>`，其余读工作树）+ 路径必须落在 checkout 内",
			note:     "本仓没有 checkout 边界与 index 读取；直接读盘会把未跟踪/被忽略的文件也当成真值",
		},
		{
			method:   MethodForgePanelMarkReady,
			official: "typert.host.js:1091 / client-ui-forge/src/index.ts:390",
			params:   withURL,
			need:     github + " 的写操作（draft → ready for review）",
			note:     "本仓没有 provider 写通道；回 success 就是伪造一次远端状态变更",
		},
		{
			method:   MethodForgePanelMerge,
			official: "typert.host.js:1142 / client-ui-forge/src/index.ts:409",
			params: []forgeParam{
				{wire: "sessionId", kind: forgeParamString},
				{wire: "url", kind: forgeParamString},
				{wire: "method", kind: forgeParamMergeMethod},
				{wire: "headSha", kind: forgeParamString},
			},
			need: github + " 的合并写操作（merge/squash/rebase，且必须校验 headSha 未移动）",
			note: "本仓没有 provider；本地合并会把远端 PR 状态与本地分支混为一谈",
		},
		{
			method:   MethodForgePanelRefresh,
			official: "typert.host.js:1227 / client-ui-forge/src/index.ts:229",
			params:   sessionOnly,
			need:     "forge inspection 调度器（重跑 git 检查 + 登录探测 + 轮询所有绑定）",
			note:     "本仓没有 inspection 与轮询；回 success 等于说\"刚刷新过\"而没有任何真值刷新",
		},
		{
			method:   MethodForgePanelRequestWorktree,
			official: "typert.host.js:1262 / client-ui-forge/src/index.ts:430",
			params: []forgeParam{
				{wire: "sessionId", kind: forgeParamString},
				{wire: "from", kind: forgeParamNullableString},
			},
			need:     worktree + "（分支命名 dsh-<session 前 8 位>-<4 位随机>，建在仓库 .agents/worktrees/ 下）",
			note:     "本仓没有 worktree 服务，也没有\"下一轮首步创建\"的 pre-step 钩子",
		},
		{
			method:   MethodForgePanelRestoreWorktree,
			official: "typert.host.js:1313 / client-ui-forge/src/index.ts:499",
			params:   sessionOnly,
			need:     worktree + " 与 working-directory 服务（按记录目录重新 checkout，分支没了就从起始 commit 重建）",
			note:     "本仓没有 worktree 记录；回 success 会让界面以为目录回来了",
		},
		{
			method:   MethodForgePanelRetryWorktree,
			official: "typert.host.js:1347 / client-ui-forge/src/index.ts:478",
			params:   sessionOnly,
			need:     worktree + " 与等待中消息的 hold（用新分支名重试）",
			note:     "本仓没有等待消息的 hold；retry 只能编一个\"在跑\"的状态",
		},
		{
			method:   MethodForgePanelStopCreation,
			official: "typert.host.js:1381 / client-ui-forge/src/index.ts:465",
			params:   sessionOnly,
			need:     "worktree 创建任务与 inbox hold（停止创建但保留请求）",
			note:     "本仓没有可停的创建任务；回 success 与\"本来就没在创建\"无法区分",
		},
		{
			method:   MethodForgePanelSubmitReview,
			official: "typert.host.js:1415 / client-ui-forge/src/index.ts:368",
			params: []forgeParam{
				{wire: "sessionId", kind: forgeParamString},
				{wire: "url", kind: forgeParamString},
				{wire: "request", kind: forgeParamReviewRequest},
			},
			need: github + " 的评论/评审写操作（必须显式带上 headSha，head 变了要拒）",
			note: "本仓没有 provider 写通道；回 success 就是伪造一次已发布的评审",
		},
		{
			method:   MethodForgePanelTrack,
			official: "typert.host.js:1484 / client-ui-forge/src/index.ts:201",
			params:   withURL,
			need:     "forge registry（识别 URL 属于哪个 code host）+ " + binding,
			note:     "本仓没有 registry，认不出 URL 就没法建立绑定；可识别子集不能冒充全部",
		},

		{
			method:   MethodLiveshareSharesAudit,
			official: "typert.host.js:232 / liveshare/src/host/share-remote.ts:132",
			params:   shareID,
			need:     relay + "；审计记录本身由 Liveshare server 持久化，" + relayOwned,
			note:     "没有 relay 就没有审计源；返回空数组会被读成\"没有审计事件\"",
		},
		{
			method:   MethodLiveshareSharesCreate,
			official: "typert.host.js:279 / liveshare/src/host/share-remote.ts:57",
			params:   shareSession,
			need:     relay + "；还要 owner 状态文件（默认 $DSH_HOME/liveshare/owner-shares.json）做注册与记录",
			note:     "没有 relay 注册就没有 shareId 与链接；本地编一个 shareId 就是假数据",
		},
		{
			method:   MethodLiveshareSharesIdentity,
			official: "typert.host.js:311 / liveshare/src/host/share-remote.ts:122",
			params: []forgeParam{{wire: "request", kind: forgeParamObject}},
			need:     relay + "（Relay-verified 身份；显示名由凭据给出，本地配置不能覆盖）",
			note:     "身份来自 relay 验证，本仓没有该凭据，返回任何 login 都是伪造",
		},
		{
			method:   MethodLiveshareSharesSetCapabilities,
			official: "typert.host.js:373 / liveshare/src/host/share-remote.ts:90",
			params: []forgeParam{{wire: "request", kind: forgeParamObject, inner: []forgeParam{
				{wire: "shareId", kind: forgeParamString},
			}}},
			need:     relay + " 的端点授权写回（插件声明 readable/writable）",
			note:     "没有 share 记录与 relay，写授权无处落盘；回 success 会让 owner 以为已收窄暴露面",
		},
		{
			method:   MethodLiveshareSharesSetRelay,
			official: "typert.host.js:405 / liveshare/src/host/share-remote.ts:114",
			params: []forgeParam{{wire: "request", kind: forgeParamObject, inner: []forgeParam{
				{wire: "relayUrl", kind: forgeParamNullableString},
			}}},
			need:     relay + " 的持久化（owner-shares.json 里的 relay 地址）",
			note:     "本仓没有 owner 状态文件；回 success 等于假装地址已保存",
		},
		{
			method:   MethodLiveshareSharesStop,
			official: "typert.host.js:437 / liveshare/src/host/share-remote.ts:72",
			params:   shareID,
			need:     relay + " 上的 share 注销与 fork 撤销",
			note:     "没有 relay 就无法注销；本地\"忘了这条\"会让 viewer 仍连得上",
		},
		{
			method:   MethodLiveshareSharesTestPublish,
			official: "typert.host.js:469 / liveshare/src/host/share-remote.ts:147",
			params:   shareSession,
			need:     relay + "（注册一个 throwaway share 来验证本机是否有发布权）",
			note:     "无 relay 时\"能发布\"只能是编的；任务口径明确禁止 testPublish 不发就报成功",
		},
		{
			method:   MethodLiveshareSharesUpdatePermissions,
			official: "typert.host.js:516 / liveshare/src/host/share-remote.ts:80",
			params: []forgeParam{{wire: "request", kind: forgeParamObject, inner: []forgeParam{
				{wire: "shareId", kind: forgeParamString},
			}}},
			need:     relay + " 的权限写回（tier write/view、allowsFork、ttlMinutes）",
			note:     "本仓没有 share 记录；回 success 会让 owner 以为只读已生效，实际没有分享层",
		},
	}
}

// forgeLiveshareSpecOf 在表里查一个方法。27 条线性扫，比多一张 map 好维护。
func forgeLiveshareSpecOf(m Method) (forgeMethodSpec, bool) {
	for _, spec := range forgeLiveshareSpecs() {
		if spec.method == m {
			return spec, true
		}
	}
	return forgeMethodSpec{}, false
}

// ---------------------------------------------------------------------------
// 分发
// ---------------------------------------------------------------------------

// serveOfficialForgeLiveshareAPI 处理本文件的 27 个方法（Lead 的接线入口）。
//
// 调用方（server.go 的 /api 路由）只需在既有 family 判断之后判断
// `IsOfficialForgeLiveshareMethod(name)` 并调用它；认证、POST 校验、失败日志
// 都已经在 handleAPI 里做过了。
func (s *Server) serveOfficialForgeLiveshareAPI(w http.ResponseWriter, r *http.Request, name string) {
	ServeOfficialForgeLiveshare(w, r, name)
}

// ServeOfficialForgeLiveshare 是纯函数形态的分发入口（没有 *Server 的宿主与
// 测试都用它）：参数校验完就回逐方法的未实现错误。
func ServeOfficialForgeLiveshare(w http.ResponseWriter, r *http.Request, name string) {
	spec, ok := forgeLiveshareSpecOf(Method(name))
	if !ok {
		// 走到这里说明接线判断和本文件不一致：明确说"不是这一族"，
		// 不要落成 404（404 会被读成"这个宿主没有这个方法"）。
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
			fmt.Sprintf("不是官方 forgePanel/liveshareShares 族的方法：%q", name)))
		return
	}
	params, err := decodeOfficialParams(r)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	if err := validateForgeLiveshareParams(params, spec); err != nil {
		writeOfficialError(w, err)
		return
	}
	// 参数已按官方 wire 校验完；这一族在本宿主上没有真值，只能如实说缺什么。
	writeOfficialError(w, fmt.Errorf("%w：%s 需要 %s。%s；按契约不以空结果/假成功代替",
		ErrOfficialNotImplemented, spec.method, spec.need, spec.note))
}

// validateForgeLiveshareParams 按官方 wire 名做 fail-loud 校验。
//
// 为什么先校验再回 501：GUI 调错参数时必须拿到 400（能改），而不是一个
// "未实现"（会让人以为等宿主就行）。两种错因在界面上必须可分辨。
func validateForgeLiveshareParams(p *officialParams, spec forgeMethodSpec) error {
	for _, want := range spec.params {
		switch want.kind {
		case forgeParamString:
			s, err := p.str(want.wire)
			if err != nil {
				return err
			}
			if s == "" {
				return officialBadRequest("%s 是空串", want.wire)
			}
		case forgeParamNullableString:
			if err := checkForgeNullableString(p, want.wire); err != nil {
				return err
			}
		case forgeParamScope:
			if err := checkForgeScope(p); err != nil {
				return err
			}
		case forgeParamMergeMethod:
			if err := checkForgeEnum(p, want.wire, []string{"merge", "squash", "rebase"}); err != nil {
				return err
			}
		case forgeParamReviewRequest:
			if err := checkForgeReviewRequest(p); err != nil {
				return err
			}
		case forgeParamObject:
			obj, present, err := p.object(want.wire)
			if err != nil {
				return err
			}
			if !present {
				return officialBadRequest("缺 %s", want.wire)
			}
			if obj == nil {
				return officialBadRequest("%s 不能是 null", want.wire)
			}
			for _, field := range want.inner {
				if err := checkForgeObjectField(want.wire, obj, field); err != nil {
					return err
				}
			}
		default:
			return officialBadRequest("%s 的参数形状没有对应的校验规则", want.wire)
		}
	}
	// 未知顶层字段一律拒：官方的 parameters 是 strictCodec（additionalProperties:false），
	// 拼错一个键就该当场说清楚，而不是静默忽略后回 501。
	return p.finish()
}

// checkForgeNullableString 校验"必填、可为 null 的字符串"。
func checkForgeNullableString(p *officialParams, name string) error {
	raw, ok := p.take(name)
	if !ok {
		return officialBadRequest("缺 %s", name)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return officialBadRequest("%s 必须是字符串或 null", name)
	}
	return nil
}

// checkForgeScope 校验 Scope：`{kind: branch|uncommitted|unstaged|staged}`。
//
// 对象内部的额外键放过（官方该对象是 additionalProperties:true），但 `kind`
// 必须在枚举里——否则界面拿到的是"未实现"，实际是参数写错了。
func checkForgeScope(p *officialParams) error {
	obj, present, err := p.object("scope")
	if err != nil {
		return err
	}
	if !present {
		return officialBadRequest("缺 scope")
	}
	if obj == nil {
		return officialBadRequest("scope 不能是 null")
	}
	raw, ok := obj["kind"]
	if !ok {
		return officialBadRequest("scope 缺 kind")
	}
	var kind string
	if err := json.Unmarshal(raw, &kind); err != nil {
		return officialBadRequest("scope.kind 必须是字符串")
	}
	for _, want := range []string{"branch", "uncommitted", "unstaged", "staged"} {
		if kind == want {
			return nil
		}
	}
	return officialBadRequest("scope.kind 只能是 branch/uncommitted/unstaged/staged，收到 %q", kind)
}

// checkForgeEnum 校验一个必填字符串字段的取值。
func checkForgeEnum(p *officialParams, name string, allowed []string) error {
	s, err := p.str(name)
	if err != nil {
		return err
	}
	for _, want := range allowed {
		if s == want {
			return nil
		}
	}
	return officialBadRequest("%s 只能是 %v，收到 %q", name, allowed, s)
}

// checkForgeReviewRequest 校验 submitReview 的 request union：
//
//	{kind:"comment", headSha, comment:{path,side,startLine,endLine,body}}
//	{kind:"review",  headSha, event:comment|approve|request-changes, body, comments:[…]}
//
// union 的分支按官方 zod 的 required 字段判定，不猜另一个分支。
func checkForgeReviewRequest(p *officialParams) error {
	obj, present, err := p.object("request")
	if err != nil {
		return err
	}
	if !present {
		return officialBadRequest("缺 request")
	}
	if obj == nil {
		return officialBadRequest("request 不能是 null")
	}
	headSha, ok := obj["headSha"]
	if !ok {
		return officialBadRequest("request 缺 headSha")
	}
	if _, err := forgeStringOf(headSha, "request.headSha"); err != nil {
		return err
	}
	rawKind, ok := obj["kind"]
	if !ok {
		return officialBadRequest("request 缺 kind")
	}
	kind, err := forgeStringOf(rawKind, "request.kind")
	if err != nil {
		return err
	}
	switch kind {
	case "comment":
		raw, ok := obj["comment"]
		if !ok {
			return officialBadRequest("request.kind=comment 时必须给 comment")
		}
		comment := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &comment); err != nil {
			return officialBadRequest("request.comment 必须是 JSON 对象")
		}
		if comment == nil {
			return officialBadRequest("request.comment 不能是 null")
		}
		for _, field := range []string{"path", "body"} {
			value, ok := comment[field]
			if !ok {
				return officialBadRequest("request.comment 缺 %s", field)
			}
			if _, err := forgeStringOf(value, "request.comment."+field); err != nil {
				return err
			}
		}
		for _, field := range []string{"side"} {
			value, ok := comment[field]
			if !ok {
				return officialBadRequest("request.comment 缺 %s", field)
			}
			side, err := forgeStringOf(value, "request.comment."+field)
			if err != nil {
				return err
			}
			if side != "L" && side != "R" {
				return officialBadRequest("request.comment.side 只能是 L 或 R，收到 %q", side)
			}
		}
		for _, field := range []string{"startLine", "endLine"} {
			value, ok := comment[field]
			if !ok {
				return officialBadRequest("request.comment 缺 %s", field)
			}
			var n json.Number
			if err := json.Unmarshal(value, &n); err != nil {
				return officialBadRequest("request.comment.%s 必须是数字", field)
			}
		}
		return nil
	case "review":
		rawEvent, ok := obj["event"]
		if !ok {
			return officialBadRequest("request 缺 event（kind=review 时必填）")
		}
		event, err := forgeStringOf(rawEvent, "request.event")
		if err != nil {
			return err
		}
		if !forgeStringIn(event, "comment", "approve", "request-changes") {
			return officialBadRequest("request.event 只能是 comment/approve/request-changes，收到 %q", event)
		}
		rawBody, ok := obj["body"]
		if !ok {
			return officialBadRequest("request 缺 body（kind=review 时必填，可以是空串）")
		}
		if _, err := forgeStringAllowEmpty(rawBody, "request.body"); err != nil {
			return err
		}
		rawComments, ok := obj["comments"]
		if !ok {
			return officialBadRequest("request 缺 comments（kind=review 时必填，可以是空数组）")
		}
		var comments []json.RawMessage
		if err := json.Unmarshal(rawComments, &comments); err != nil {
			return officialBadRequest("request.comments 必须是数组")
		}
		if comments == nil {
			return officialBadRequest("request.comments 不能是 null")
		}
		return nil
	default:
		return officialBadRequest("request.kind 只能是 comment 或 review，收到 %q", kind)
	}
}

// checkForgeObjectField 校验 `request` 对象里的一个必填字段。
func checkForgeObjectField(owner string, obj map[string]json.RawMessage, field forgeParam) error {
	raw, ok := obj[field.wire]
	if !ok {
		return officialBadRequest("%s 缺 %s", owner, field.wire)
	}
	switch field.kind {
	case forgeParamString:
		if _, err := forgeStringOf(raw, owner+"."+field.wire); err != nil {
			return err
		}
	case forgeParamNullableString:
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return officialBadRequest("%s.%s 必须是字符串或 null", owner, field.wire)
		}
	default:
		return officialBadRequest("%s.%s 的参数形状没有对应的校验规则", owner, field.wire)
	}
	return nil
}

// forgeStringOf 取一个字符串字段，空串与非字符串都算参数错。
func forgeStringOf(raw json.RawMessage, name string) (string, error) {
	s, err := forgeStringAllowEmpty(raw, name)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", officialBadRequest("%s 是空串", name)
	}
	return s, nil
}

// forgeStringAllowEmpty 取一个字符串字段，但**允许空串**。
//
// 用在官方契约里就是 `z.string()` 的字段上：评审摘要（review body）可以是空的，
// 而 url/shareId/sessionId 这类空串没有意义，必须当场拒。
func forgeStringAllowEmpty(raw json.RawMessage, name string) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", officialBadRequest("%s 必须是字符串", name)
	}
	return s, nil
}

// forgeStringIn 判断 v 是否等于 allowed 里的某一个。
func forgeStringIn(v string, allowed ...string) bool {
	for _, want := range allowed {
		if v == want {
			return true
		}
	}
	return false
}
