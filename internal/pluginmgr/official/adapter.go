// Package official 把 `internal/pluginmgr` 的**真实**状态接到官方
// `pluginManager.*`（0.2.2-rc.1）的 L1 契约上。
//
// 为什么它是一个独立包，而不是塞进 `internal/transport` 或 `cmd/dshd`：
//
//   - `internal/transport` 的既有约定是"只做协议翻译，宿主状态靠注入"
//     （见 handlers_agent_official.go 顶部）。把 pluginmgr 的磁盘/事务知识
//     写进 Transport 会破坏那条边界；
//   - `cmd/dshd` 不在本次写作用域内，且放那里就无法用真实管理器做端到端测试；
//   - 于是这一层只做一件事：把官方契约与真实管理器**双向**映射，并让
//     `Cmd/dshd` 的接线退化成一行 `official.New(pluginManager)`。
//
// 真实性边界（每条都在 adapter_test.go 里有真实磁盘断言）：
//
//   - 装/卸走 `pluginmgr` 的既有事务（journal + staging rename + 回滚）；
//   - 启用位与安装写在同一份 `state.json`，重开管理器后仍在；
//   - 取消只在事务检查点生效，收工后 `state.json` 与 `packages/` 都回到原样；
//   - 需要 registry/pnpm/git/tarball 的形态一律 `transport.ErrOfficialNotImplemented`，
//     绝不假装装了一个下载不到的包；
//   - 兼容性模型与版本豁免是真的：`setVersionExemption` 把授权/撤销写进管理器
//     root 下的 `compatibility.json`（与官方同名同形状），install/update 在装之前
//     按 `engines.dsh` / peerDependencies 判一次，不兼容且没有精确豁免就拒装。
package official

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"dshgo/internal/pluginmgr"
	"dshgo/internal/transport"
)

// adapter 把官方契约翻译到真实管理器。它没有自己的状态：每次调用都读
// `pluginmgr` 的当前真实状态。
type adapter struct {
	manager *pluginmgr.Manager
}

// New 返回官方 pluginManager 的真实实现集合。
//
// `SetVersionExemption` 走 `pluginmgr.Manager.SetVersionExemption`：真实持久化
// （compatibility.json，重开管理器后仍可读），并被 install/update 的兼容性闸门
// 消费——这正是"豁免"在官方语义里的作用，不是一份没人读的死数据。
func New(manager *pluginmgr.Manager) transport.OfficialPluginManagerAPI {
	if manager == nil {
		return transport.OfficialPluginManagerAPI{}
	}
	a := &adapter{manager: manager}
	return transport.OfficialPluginManagerAPI{
		Inspect:             a.inspect,
		ListVersions:        a.listVersions,
		InstallBundle:       a.installBundle,
		UpdateBundle:        a.updateBundle,
		RemoveBundle:        a.removeBundle,
		SetBundleEnabled:    a.setEnabled,
		SetPluginEnabled:    a.setEnabled,
		SetVersionExemption: a.setVersionExemption,
		// 读与写是同一份 compatibility.json：listVersionExemptions 是
		// setVersionExemption 的读侧，不是另一份状态。
		ListVersionExemptions: a.listVersionExemptions,
		ReadBundleDetail:      a.readBundleDetail,
		WaitForInstall:        a.waitForInstall,
		CancelInstall:         a.cancelInstall,
	}
}

// docCandidates 是包内长文档的候选名（官方先看 locale 的 meta.detail，再看
// README；本仓 manifest 没有 meta.detail，所以只有 README 这一档）。
var docCandidates = []string{"README.md", "readme.md", "Readme.md", "README.markdown"}

// readmePrefix 是"不是 README"的判定：官方 likewise 只在没有 locale 文档时
// 才回落到 README，回落到 README 时 origin 就是 `readme`。

func (a *adapter) inspect(p transport.OfficialInspectParams) (transport.OfficialPluginSpecInspection, error) {
	spec := strings.TrimSpace(p.Spec)
	if p.Registry != nil {
		return transport.OfficialPluginSpecInspection{}, registryUnsupported()
	}
	kind := pluginmgr.SourceKind(spec)
	if kind != "path" {
		return transport.OfficialPluginSpecInspection{}, formUnsupported(kind)
	}
	if _, err := os.Stat(spec); err != nil {
		return transport.OfficialInspectionRefused("not-a-package", fmt.Sprintf("路径不存在或不可读: %v", err)), nil
	}
	manifest, err := pluginmgr.ValidateSource(spec)
	if err != nil {
		// 校验理由来自真实的 manifest/路径/hash 检查，原样带给界面。
		return transport.OfficialInspectionRefused("not-a-package", err.Error()), nil
	}
	if _, installed, err := a.manager.Bundle(manifest.ID); err != nil {
		return transport.OfficialPluginSpecInspection{}, hostError(err)
	} else if installed {
		return transport.OfficialInspectionRefused("already-installed",
			fmt.Sprintf("%s 已经装过了", manifest.ID)), nil
	}
	// 本仓包没有 `dsh.bundle` patch rows，所以 bundle 如实报 false。
	return transport.OfficialInspectionAccepted("path", manifest.ID, manifest.Version, false), nil
}

func (a *adapter) listVersions(p transport.OfficialListVersionsParams) (transport.OfficialVersionList, error) {
	if p.Registry != nil {
		return transport.OfficialVersionList{}, registryUnsupported()
	}
	state, ok, err := a.manager.Bundle(p.Name)
	if err != nil {
		return transport.OfficialVersionList{}, hostError(err)
	}
	if !ok {
		// 没装就没有"已安装版本"可作基准；本仓也没有 registry 可问。
		return transport.OfficialVersionsRefused("not-updatable",
			fmt.Sprintf("%s 不是已安装的 profile 依赖（本仓的包只从本地目录安装）", p.Name)), nil
	}
	if pluginmgr.SourceIsRegistrySpec(state.Source) {
		// 数据模型上不该出现；真出现了也不能假装查了 registry。
		return transport.OfficialVersionList{}, registryUnsupported()
	}
	return transport.OfficialVersionsRefused("not-updatable",
		fmt.Sprintf("%s@%s 来自%s，不是 registry 依赖；本仓没有 registry 客户端，也没有本地发布版本源",
			state.Name, state.Version, pluginmgr.DescribeSource(state.Source))), nil
}

func (a *adapter) installBundle(p transport.OfficialInstallBundleParams) (transport.OfficialChangeResult, error) {
	if p.Registry != nil {
		return transport.OfficialChangeResult{}, registryUnsupported()
	}
	if len(p.ApprovedBuilds) > 0 {
		return transport.OfficialChangeResult{}, fmt.Errorf(
			"%w: approvedBuilds 需要 pnpm 的构建脚本审批；本仓不跑 pnpm，也没有 allowBuilds 设置可写",
			transport.ErrOfficialNotImplemented)
	}
	spec := strings.TrimSpace(p.Spec)
	if kind := pluginmgr.SourceKind(spec); kind != "path" {
		return transport.OfficialChangeResult{}, formUnsupported(kind)
	}
	// 官方默认 enabled=true（`options?.enabled !== false`）。
	enable := true
	if p.Enabled != nil {
		enable = *p.Enabled
	}
	requests := pluginmgr.InstallOptions{Enable: enable}
	if p.RequestID != nil {
		requests.RequestID = *p.RequestID
	}
	job, err := a.manager.StartInstall(spec, requests)
	if err != nil {
		return transport.OfficialChangeResult{}, hostError(err)
	}
	outcome := job.Wait()
	result := a.changeFromOutcome(outcome, "install", spec)
	if outcome.Application == "applied" {
		result.Bundle = outcome.Info.ID
		result.Version = outcome.Info.Version
		if warning := a.exemptionWarningFor(outcome.Info.ID + "@" + outcome.Info.Version); warning != "" {
			result.Warnings = append(result.Warnings, warning)
		}
	}
	return result, nil
}

func (a *adapter) updateBundle(p transport.OfficialUpdateBundleParams) (transport.OfficialUpdateResult, error) {
	if p.Registry != nil {
		return transport.OfficialUpdateResult{}, registryUnsupported()
	}
	state, ok, err := a.manager.Bundle(p.Name)
	if err != nil {
		return transport.OfficialUpdateResult{}, hostError(err)
	}
	if !ok {
		return updateRefused("", p.Name, p.Version, "invalid-spec",
			fmt.Sprintf("%s 不是已安装的 profile 依赖", p.Name)), nil
	}
	from := state.Version
	compare, err := pluginmgr.CompareVersions(p.Version, from)
	if err != nil {
		// 版本串本身不是 SemVer：官方的 check 阶段就拒，什么都不改。
		return updateRefused(from, p.Name, p.Version, "invalid-spec",
			fmt.Sprintf("%q 不是 SemVer 版本", p.Version)), nil
	}
	if compare <= 0 {
		return updateRefused(from, p.Name, p.Version, "invalid-spec",
			fmt.Sprintf("%s 不新于已安装的 %s", p.Version, from)), nil
	}
	if _, ok := state.VersionSources[p.Version]; !ok {
		// 目标版本不在本机 store：下载它需要 registry/pnpm，本仓没有。
		return transport.OfficialUpdateResult{}, fmt.Errorf(
			"%w: 本机 store 里没有 %s@%s，下载它需要 registry/pnpm 客户端",
			transport.ErrOfficialNotImplemented, p.Name, p.Version)
	}
	requests := pluginmgr.InstallOptions{}
	if p.RequestID != nil {
		requests.RequestID = *p.RequestID
	}
	job, err := a.manager.StartUpdate(p.Name, p.Version, requests)
	if err != nil {
		return transport.OfficialUpdateResult{}, hostError(err)
	}
	outcome := job.Wait()
	result := a.changeFromOutcome(outcome, "install", p.Name)
	if warning := a.exemptionWarningFor(p.Name + "@" + p.Version); warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}
	return transport.OfficialUpdateResult{
		From:   from,
		To:     p.Version,
		Step:   "install",
		Result: result,
	}, nil
}

func updateRefused(from, name, version, code, diagnostic string) transport.OfficialUpdateResult {
	return transport.OfficialUpdateResult{
		From: from, To: version, Step: "check",
		Result: transport.OfficialFailedChange("install", name, code, diagnostic),
	}
}

func (a *adapter) removeBundle(p transport.OfficialBundleNameParams) (transport.OfficialChangeResult, error) {
	if _, ok, err := a.manager.Bundle(p.Name); err != nil {
		return transport.OfficialChangeResult{}, hostError(err)
	} else if !ok {
		// 官方把"不可移除"放在结果里（ManagementFailure），不是抛错。
		return transport.OfficialFailedChange("remove", p.Name, "not-removable",
			fmt.Sprintf("没有已安装的包 %s", p.Name)), nil
	}
	if err := a.manager.Uninstall(p.Name); err != nil {
		var apiErr *pluginmgr.Error
		if errors.As(err, &apiErr) {
			return transport.OfficialFailedChange("remove", p.Name, mapChangeCode(apiErr.Code), apiErr.Message), nil
		}
		return transport.OfficialChangeResult{}, hostError(err)
	}
	return transport.OfficialAppliedChange("remove", p.Name, false), nil
}

// setEnabled 同时服务 setBundleEnabled 与 setPluginEnabled：本仓一包一插件，
// 两个官方方法写的是同一个持久化启用位（同一 id 空间）。
func (a *adapter) setEnabled(p transport.OfficialSetEnabledParams) (transport.OfficialChangeResult, error) {
	state, ok, err := a.manager.Bundle(p.Name)
	if err != nil {
		return transport.OfficialChangeResult{}, hostError(err)
	}
	if !ok {
		return transport.OfficialFailedChange("enable", p.Name, "unknown-plugin",
			fmt.Sprintf("没有已安装的包 %s", p.Name)), nil
	}
	result := transport.OfficialAppliedChange("enable", p.Name, p.Enabled)
	if state.Enabled == p.Enabled {
		result.Changed = false
		return result, nil
	}
	var info pluginmgr.PluginInfo
	if p.Enabled {
		info, err = a.manager.Enable(p.Name)
	} else {
		info, err = a.manager.Disable(p.Name)
	}
	if err != nil {
		var apiErr *pluginmgr.Error
		if errors.As(err, &apiErr) {
			return transport.OfficialFailedChange("enable", p.Name, mapChangeCode(apiErr.Code), apiErr.Message), nil
		}
		return transport.OfficialChangeResult{}, hostError(err)
	}
	enabled := info.Enabled
	result.Changed = true
	result.Enabled = &enabled
	return result, nil
}

// setVersionExemption 授权/撤销一个精确版本豁免。
//
// 官方语义（dsh-plugin-manager/lib/index.js:2085-2090）：先写 profile 的
// compatibility.json，再 reload 活体插件。本仓没有运行时插件加载器，所以第二半
// 如实折成 warning，而不是假装 reload 过。
func (a *adapter) setVersionExemption(p transport.OfficialVersionExemptionParams) (transport.OfficialChangeResult, error) {
	acceptRisk := p.AcceptRisk != nil && *p.AcceptRisk
	changed, err := a.manager.SetVersionExemption(p.PackageVersion, p.RuntimeVersion, p.Enabled, acceptRisk)
	if err != nil {
		return transport.OfficialChangeResult{}, hostError(err)
	}
	result := transport.OfficialAppliedChange("enable", p.PackageVersion, p.Enabled)
	result.Changed = changed.Changed
	result.Warnings = append(result.Warnings, "豁免已持久化到插件管理器的 compatibility.json（重开仍可读）")
	result.Warnings = append(result.Warnings, changed.Warnings...)
	return result, nil
}

// listVersionExemptions 读 profile 里已接受的精确版本豁免。
//
// 官方语义（dsh-plugin-manager/lib/index.js:2071-2077）：直接读
// compatibility.json，把**被接受**的 `name@version → [dsh 版本]` 与读者拒掉的
// 记录/文件问题一起返回。坏文件不抛错：exemptions 为空、warnings 如实带出，
// 让界面提示用户先手工修（本仓 `pluginmgr.readCompatibility` 同一条分支）。
func (a *adapter) listVersionExemptions() (transport.OfficialVersionExemptions, error) {
	exemptions, warnings := a.manager.VersionExemptions()
	return transport.OfficialVersionExemptions{
		Exemptions: exemptions,
		Warnings:   warnings,
	}, nil
}

// exemptionWarningFor 报告"这个精确版本是靠豁免装上的"。
//
// 返回空串表示没有豁免，不加噪音；有豁免就如实带上风险——官方同样把它渲染成
// 一条用户可见的警告（pluginCompatibilityWarning）。
func (a *adapter) exemptionWarningFor(packageVersion string) string {
	exemptions, _ := a.manager.VersionExemptions()
	for _, runtimeVersion := range exemptions[packageVersion] {
		if runtimeVersion == pluginmgr.HostRuntimeVersion {
			return fmt.Sprintf("%s 与 dsh %s 不兼容，是按精确版本豁免安装的：运行它可能崩溃或丢数据",
				packageVersion, pluginmgr.HostRuntimeVersion)
		}
	}
	return ""
}

func (a *adapter) readBundleDetail(p transport.OfficialReadBundleDetailParams) (transport.OfficialBundleDetail, error) {
	path, raw, ok, err := a.manager.ReadDocument(p.Name, docCandidates)
	if err != nil {
		var apiErr *pluginmgr.Error
		if errors.As(err, &apiErr) && apiErr.Code == "not_found" {
			return transport.OfficialBundleDetail{}, transport.OfficialNotFound(
				"没有这个 bundle：%q（先调 installBundle 或核对 id）", p.Name)
		}
		return transport.OfficialBundleDetail{}, hostError(err)
	}
	if !ok {
		// 装上了但没有 README：官方的回答就是 none，不是错误。
		return transport.OfficialBundleDetail{Status: "none"}, nil
	}
	return transport.OfficialBundleDetail{
		Status: "found", Origin: "readme", Path: path,
		Markdown: stripDocumentPrefix(raw),
	}, nil
}

func (a *adapter) waitForInstall(p transport.OfficialRequestIDParams) (transport.OfficialInstallWait, error) {
	outcome, ok := a.manager.WaitInstall(p.RequestID)
	if !ok {
		// 官方的 null：没有（或已经收工）这个 requestId。
		return transport.OfficialInstallWait{Active: false}, nil
	}
	result := a.changeFromOutcome(outcome, outcome.Stage, outcome.Target)
	return transport.OfficialInstallWait{Active: true, Result: &result}, nil
}

func (a *adapter) cancelInstall(p transport.OfficialRequestIDParams) (transport.OfficialInstallCancellation, error) {
	return transport.OfficialInstallCancellation{Status: a.manager.CancelInstall(p.RequestID)}, nil
}

// changeFromOutcome 把真实作业结果翻译成官方 ChangeResult。
func (a *adapter) changeFromOutcome(outcome pluginmgr.InstallOutcome, stage, target string) transport.OfficialChangeResult {
	if stage == "" {
		stage = outcome.Stage
	}
	if target == "" {
		target = outcome.Target
	}
	switch outcome.Application {
	case "applied":
		enabled := false
		if outcome.Enabled != nil {
			enabled = *outcome.Enabled
		} else if outcome.Changed {
			enabled = outcome.Info.Enabled
		}
		result := transport.OfficialAppliedChange(stage, target, enabled)
		result.Bundle = outcome.Info.ID
		result.Version = outcome.Info.Version
		return result
	case "cancelled":
		return transport.OfficialCancelledChange(stage, target)
	default:
		code := "operation-error"
		diagnostic := "安装事务失败"
		if outcome.Failure != nil {
			code = mapChangeCode(outcome.Failure.Code)
			diagnostic = outcome.Failure.Message
		}
		return transport.OfficialFailedChange(stage, target, code, diagnostic)
	}
}

// mapChangeCode 把 pluginmgr 的稳定码映射到官方的 `ChangeResult.error.code` 枚举。
func mapChangeCode(code string) string {
	switch code {
	case "duplicate_id", "already_installed":
		return "already-installed"
	case "invalid_manifest", "hash_mismatch", "bad_request", "version_conflict", "state_corrupt",
		"package_corrupt", "transaction_corrupt":
		return "invalid-spec"
	case "incompatible_version":
		return "incompatible-version"
	case "not_found":
		return "unknown-plugin"
	case "duplicate_request":
		return "operation-error"
	case "document_too_large":
		return "operation-error"
	default:
		return "operation-error"
	}
}

// stripDocumentPrefix 去掉 BOM 与开头的 YAML front matter（官方同样剥这两样：
// 前端不应拿到 `---` 头）。
func stripDocumentPrefix(raw []byte) string {
	text := strings.TrimPrefix(string(raw), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---") {
		return text
	}
	lines := strings.Split(text, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return text
	}
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			return strings.TrimPrefix(strings.Join(lines[index+1:], "\n"), "\n")
		}
	}
	// 没有闭合的 front matter：原样返回（不猜它是不是文档内容）。
	return text
}

// hostError 把 pluginmgr 的四字段错误翻译成 official 错误，保住状态码。
//
// 一律塌成 500 会让客户端把"参数写错/状态冲突"当成"宿主崩了"（它就会重试，
// 或者去改参数却改错地方）。hint 由错误码统一给出，这里不带。
func hostError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *pluginmgr.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Status {
	case http.StatusBadRequest:
		return transport.OfficialBadRequest("%s", apiErr.Message)
	case http.StatusNotFound:
		return transport.OfficialNotFound("%s", apiErr.Message)
	case http.StatusConflict:
		return transport.OfficialConflict("%s", apiErr.Message)
	case http.StatusUnprocessableEntity:
		return transport.OfficialBadRequest("%s", apiErr.Message)
	default:
		return err
	}
}

func formUnsupported(kind string) error {
	return fmt.Errorf("%w: %q 形态的 spec 需要 registry/pnpm/git/tarball 客户端；本仓只接受本地目录包（kind=path）",
		transport.ErrOfficialNotImplemented, kind)
}

func registryUnsupported() error {
	return fmt.Errorf("%w: 本仓没有 registry 客户端（pnpm view 等价物），也没有任何 registry 配置可读",
		transport.ErrOfficialNotImplemented)
}
