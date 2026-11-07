package transport

// 本文件是官方桌面端 0.2.2-rc.1「media / market 侧」21 个 typert 方法的 L1
// 契约实现（官方源：@deepseek-ai/dsh-experimental-api-speech-to-text /
// dsh-experimental-client-ui-cot-translation / dsh-office-to-pdf /
// dsh-plugin-market / dsh-plugin-recommendations / dsh-cordis-host-runner 的
// `lib/typert.host.js`，从 app.asar 只读解；方法清单见
// docs/evidence/official-desktop-api-methods-0.2.2-rc.1.txt）。
//
// 对齐口径与 `handlers_agent_official.go` / `handlers_pluginmanager_official.go`
// 一致：
//   - **L1**：参数名、必填/可选、结果字段按官方 zod 契约；wire 仍走本仓既有的
//     `POST /api/<method>` + 裸参数对象 + 结果对象 / 四字段 error。
//   - **L2**（`remote.mux` + typert Connection envelope）未实现，因此本文不声称
//     与官方桌面端 wire 兼容。
//
// 本文件只做协议翻译与**参数形状校验**，不读盘、不伪造任何结果。21 个方法逐条
// 结论（与 RESULT 的三列表一致）：
//
//  1. `cotTranslation.translate` —— **真实现**：把文本送真实模型网关
//     （`dshgo/internal/llm`，与 `cmd/dshd` 同一套 env：`DSHD_LLM_BASE` /
//     `DSHD_LLM_KEY` / `DSHD_LLM_MODEL` / `DSH_GO_MODEL`）做一次翻译，返回模型
//     真实输出。官方另有 `google`/`bing` 两个匿名翻译端点，本仓不代理第三方
//     匿名服务，这两个 provider 明确 501。
//  2. `speech.*` 4 个 —— 未实现：本仓没有语音识别引擎（ASR 模型 + 预处理/VAD +
//     模型下载与加载），所以 `prepare`/`configure`/`cancelPreparation` 没有可作用
//     的资源，`transcribe` 更不能编一段文字冒充转写结果。
//  3. `officeToPdf.render` —— 未实现：本仓没有 Office 渲染器（官方是
//     libreoffice-kit + 后台转换队列 + 字体缺失检测）。不拿文本抽取冒充 PDF。
//  4. `pluginMarket.*` 3 个 + `pluginRecommendations.describe` —— 未实现：市场
//     三个方法需要「已配置的第三方索引源 + HTTP 取回索引文档 + 索引文档 schema
//     校验 + 模糊匹配/排序/分页 + 安装状态计算」，本仓只有**已安装**插件目录
//     （`internal/pluginmgr`），没有市场索引存储、没有索引源设置项、没有
//     npm 文件 CDN / Git 提交读取器；recommendations 依赖的"已记录推荐卡"在官方
//     是模型侧工具产物，本仓整条链路都不存在。
//  5. `dynamicCordisRunner.*` 11 个 —— 未实现，且**按红线 R6 明确不做**（宿主不嵌
//     JS 运行时，不 fork/vendor dsc）。这 11 个方法没有一个只是数据搬运：每个都以
//     "在宿主里执行 Cordis 插件 JS 并结算那次运行"为语义，宿主侧没有合法执行路径。
//     这不是能力欠账，是设计边界。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"dshgo/internal/llm"
)

// 官方方法名。字符串必须与官方 `createTypertRemoteInfo` 的键逐字对应
// （官方 `'speech/prepare'`；本仓把 `/` 换成 `.`，与既有 58 个方法同形）。
const (
	MethodSpeechPrepare           Method = "speech.prepare"
	MethodSpeechTranscribe        Method = "speech.transcribe"
	MethodSpeechConfigure         Method = "speech.configure"
	MethodSpeechCancelPreparation Method = "speech.cancelPreparation"

	MethodCotTranslationTranslate Method = "cotTranslation.translate"

	MethodOfficeToPdfRender Method = "officeToPdf.render"

	MethodPluginMarketSearch          Method = "pluginMarket.search"
	MethodPluginMarketReadEntryDetail Method = "pluginMarket.readEntryDetail"
	MethodPluginMarketSetIndexes      Method = "pluginMarket.setIndexes"

	MethodPluginRecommendationsDescribe Method = "pluginRecommendations.describe"

	MethodDynamicCordisRunnerGetClientCode            Method = "dynamicCordisRunner.getClientCode"
	MethodDynamicCordisRunnerInvoke                   Method = "dynamicCordisRunner.invoke"
	MethodDynamicCordisRunnerReportClientGuardFailure Method = "dynamicCordisRunner.reportClientGuardFailure"
	MethodDynamicCordisRunnerReportRenderFailure      Method = "dynamicCordisRunner.reportRenderFailure"
	MethodDynamicCordisRunnerResolveInspectQuery      Method = "dynamicCordisRunner.resolveInspectQuery"
	MethodDynamicCordisRunnerResolveRequestRun        Method = "dynamicCordisRunner.resolveRequestRun"
	MethodDynamicCordisRunnerRunHostHalf              Method = "dynamicCordisRunner.runHostHalf"
	MethodDynamicCordisRunnerSettleUserRun            Method = "dynamicCordisRunner.settleUserRun"
	MethodDynamicCordisRunnerStopFromPanel            Method = "dynamicCordisRunner.stopFromPanel"
	MethodDynamicCordisRunnerSyncInspectManifest      Method = "dynamicCordisRunner.syncInspectManifest"
	MethodDynamicCordisRunnerUndefineFromPanel        Method = "dynamicCordisRunner.undefineFromPanel"
)

// OfficialMediaMarketMethods 按固定顺序返回本文件实现的 21 个方法。
//
// 它给快照/文档/门禁用：新增或删除一个方法必须显式改这里，不能让"方法悄悄少了
// 一个"从路由表里漏过去。
func OfficialMediaMarketMethods() []Method {
	return []Method{
		MethodCotTranslationTranslate,
		MethodDynamicCordisRunnerGetClientCode,
		MethodDynamicCordisRunnerInvoke,
		MethodDynamicCordisRunnerReportClientGuardFailure,
		MethodDynamicCordisRunnerReportRenderFailure,
		MethodDynamicCordisRunnerResolveInspectQuery,
		MethodDynamicCordisRunnerResolveRequestRun,
		MethodDynamicCordisRunnerRunHostHalf,
		MethodDynamicCordisRunnerSettleUserRun,
		MethodDynamicCordisRunnerStopFromPanel,
		MethodDynamicCordisRunnerSyncInspectManifest,
		MethodDynamicCordisRunnerUndefineFromPanel,
		MethodOfficeToPdfRender,
		MethodPluginMarketReadEntryDetail,
		MethodPluginMarketSearch,
		MethodPluginMarketSetIndexes,
		MethodPluginRecommendationsDescribe,
		MethodSpeechCancelPreparation,
		MethodSpeechConfigure,
		MethodSpeechPrepare,
		MethodSpeechTranscribe,
	}
}

// IsOfficialMediaMarketMethod 判断一个 /api 路径名是否属于本文件的 21 个方法。
//
// 路由侧只需要这一次判断（**不要**在 server.go 里摊开 21 个 case：官方方法还会
// 继续加，而那张路由表是冻结的）。
func IsOfficialMediaMarketMethod(name string) bool {
	switch Method(name) {
	case MethodCotTranslationTranslate,
		MethodDynamicCordisRunnerGetClientCode,
		MethodDynamicCordisRunnerInvoke,
		MethodDynamicCordisRunnerReportClientGuardFailure,
		MethodDynamicCordisRunnerReportRenderFailure,
		MethodDynamicCordisRunnerResolveInspectQuery,
		MethodDynamicCordisRunnerResolveRequestRun,
		MethodDynamicCordisRunnerRunHostHalf,
		MethodDynamicCordisRunnerSettleUserRun,
		MethodDynamicCordisRunnerStopFromPanel,
		MethodDynamicCordisRunnerSyncInspectManifest,
		MethodDynamicCordisRunnerUndefineFromPanel,
		MethodOfficeToPdfRender,
		MethodPluginMarketReadEntryDetail,
		MethodPluginMarketSearch,
		MethodPluginMarketSetIndexes,
		MethodPluginRecommendationsDescribe,
		MethodSpeechCancelPreparation,
		MethodSpeechConfigure,
		MethodSpeechPrepare,
		MethodSpeechTranscribe:
		return true
	}
	return false
}

// ServeOfficialMediaMarketAPI 是这 21 个方法在 `/api` 上的唯一入口。
//
// 调用方（server.go 的 handleAPI）只需在既有前缀判断之前加一条：
//
//	if IsOfficialMediaMarketMethod(name) {
//	        s.ServeOfficialMediaMarketAPI(w, r)
//	        return
//	}
//
// 认证、`POST` 校验与失败日志都已经在 handleAPI 里做过了。
func (s *Server) ServeOfficialMediaMarketAPI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/")
	if !IsOfficialMediaMarketMethod(name) {
		// 接错路由就当场失败：宁可 404，也不要让一个非本组方法走到这里拿到"空成功"。
		writeJSON(w, http.StatusNotFound, errorBodyFor("not_found",
			fmt.Sprintf("%q 不是官方 media/market 方法", name)))
		return
	}
	params, err := decodeOfficialParams(r)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	result, err := routeOfficialMediaMarketMethod(r, Method(name), params)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	if result == nil {
		// 本仓成功出口不接受 nil（会被写成 500）。本组唯一有载荷的方法是
		// `cotTranslation.translate`（字符串），走到这里说明实现返回了零值：
		// 用空对象表达"没有载荷"，而不是让客户端把 null 当成功。
		result = map[string]any{}
	}
	writeJSON(w, http.StatusOK, result)
}

func routeOfficialMediaMarketMethod(r *http.Request, method Method, p *officialParams) (any, error) {
	switch method {
	case MethodSpeechPrepare:
		return officialSpeechPrepare(p)
	case MethodSpeechTranscribe:
		return officialSpeechTranscribe(p)
	case MethodSpeechConfigure:
		return officialSpeechConfigure(p)
	case MethodSpeechCancelPreparation:
		return officialSpeechCancelPreparation(p)
	case MethodCotTranslationTranslate:
		return officialCotTranslationTranslate(r, p)
	case MethodOfficeToPdfRender:
		return officialOfficeToPdfRender(p)
	case MethodPluginMarketSearch:
		return officialPluginMarketSearch(p)
	case MethodPluginMarketReadEntryDetail:
		return officialPluginMarketReadEntryDetail(p)
	case MethodPluginMarketSetIndexes:
		return officialPluginMarketSetIndexes(p)
	case MethodPluginRecommendationsDescribe:
		return officialPluginRecommendationsDescribe(p)
	}
	if spec, ok := officialCordisSpecs[method]; ok {
		return officialCordisRefused(method, spec, p)
	}
	return nil, officialNotImplemented("%s 未实现", method)
}

// ---------------------------------------------------------------------------
// 参数读取：官方"位置对象参数"的两种写法
// ---------------------------------------------------------------------------

// mmView 给"一个对象型位置参数"（`request` / `query` / `options` / `patch`）
// 提供统一读取口。官方这些参数是一个对象，而本仓 wire 是一个裸 JSON 对象，
// 因此两种写法都认：
//
//	{"request":{"text":"…","targetLanguage":"zh-CN"}}   // 嵌套（贴着官方 wire）
//	{"text":"…","targetLanguage":"zh-CN"}                // 平铺（本仓既有习惯）
//
// 同一个名字两处都给 → 400（不猜哪个是真的，与 handlers_agent_official.go 对
// `ref`/`request` 的处理一致）。对象参数与同级标量参数（`speech.prepare` 的
// providerId 旁边是 options、`pluginMarket.search` 的 url 旁边是 query）也能
// 一起读：嵌套对象里的键优先，顶层同名的键仍然认。
type mmView struct {
	owner string
	top   *officialParams
	nest  *officialParams // nil = 这个参数没有嵌套写法
}

// mmOpen 打开一个对象型位置参数。没有给这个参数时返回"只有顶层"的视图。
func mmOpen(p *officialParams, owner string) (*mmView, error) {
	view := &mmView{owner: owner, top: p}
	raw, ok := p.take(owner)
	if !ok {
		return view, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, officialBadRequest("%s 必须是 JSON 对象", owner)
	}
	if m == nil {
		return nil, officialBadRequest("%s 是 null：要么不给，要么给一个对象", owner)
	}
	view.nest = &officialParams{m: m, used: map[string]bool{}}
	return view, nil
}

// mmTop 是"这个参数没有对象包裹、字段直接躺在顶层"的视图。
func mmTop(p *officialParams) *mmView { return &mmView{top: p} }

func (v *mmView) take(name string) (json.RawMessage, bool, error) {
	var nested, outer bool
	var nestedRaw, outerRaw json.RawMessage
	if v.nest != nil {
		nestedRaw, nested = v.nest.take(name)
	}
	outerRaw, outer = v.top.take(name)
	switch {
	case nested && outer:
		return nil, false, officialBadRequest("%s 与 %s.%s 不能同时给", name, v.owner, name)
	case nested:
		return nestedRaw, true, nil
	case outer:
		return outerRaw, true, nil
	}
	return nil, false, nil
}

// finish 把两处"没用到的键"都当未知字段拒掉（官方 strictCodec 是
// additionalProperties:false）。
func (v *mmView) finish() error {
	if v.nest != nil {
		if err := v.nest.finish(); err != nil {
			// 把"哪个对象里"说清楚：只报 不认识的字段 "x" 时，调用方分不出是
			// 顶层还是 request/query 里的。
			if msg := err.Error(); strings.HasPrefix(msg, "不认识的字段 ") {
				return officialBadRequest("%s 里%s", v.owner, msg)
			}
			return err
		}
	}
	if err := v.top.finish(); err != nil {
		return err
	}
	return nil
}

// str 取可选字符串。
func (v *mmView) str(name string) (string, bool, error) {
	raw, ok, err := v.take(name)
	if err != nil || !ok {
		return "", false, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, officialBadRequest("%s 必须是字符串", name)
	}
	return s, true, nil
}

// strValue 取必填字符串；空串由调用方决定收不收。
func (v *mmView) strValue(name string) (string, error) {
	s, ok, err := v.str(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", officialBadRequest("缺 %s", name)
	}
	return s, nil
}

// strNonEmpty 取必填字符串，并拒绝空串。
func (v *mmView) strNonEmpty(name string) (string, error) {
	s, err := v.strValue(name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", officialBadRequest("%s 是空串", name)
	}
	return s, nil
}

// object 要求这个键（若给）是 JSON 对象；内部字段不在这里读。
func (v *mmView) object(name string, required bool) error {
	raw, ok, err := v.take(name)
	if err != nil {
		return err
	}
	if !ok {
		if required {
			return officialBadRequest("缺 %s", name)
		}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return officialBadRequest("%s 必须是 JSON 对象", name)
	}
	if obj == nil {
		return officialBadRequest("%s 是 null：要么不给，要么给一个对象", name)
	}
	return nil
}

// strings 取字符串数组（元素必须是字符串，允许空数组）。
func (v *mmView) strings(name string, required bool) ([]string, error) {
	raw, ok, err := v.take(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		if required {
			return nil, officialBadRequest("缺 %s", name)
		}
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, officialBadRequest("%s 必须是 JSON 数组", name)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		var s string
		if err := json.Unmarshal(item, &s); err != nil {
			return nil, officialBadRequest("%s[%d] 必须是字符串", name, i)
		}
		out = append(out, s)
	}
	return out, nil
}

// number 取可选数字。
func (v *mmView) number(name string) (float64, bool, error) {
	raw, ok, err := v.take(name)
	if err != nil || !ok {
		return 0, false, err
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false, officialBadRequest("%s 必须是数字", name)
	}
	return f, true, nil
}

// intNonNegative 取必填的非负整数（官方 offset / limit 是整数语义）。
func (v *mmView) intNonNegative(name string, min, max int64) (int64, error) {
	f, ok, err := v.number(name)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, officialBadRequest("缺 %s", name)
	}
	if f != math.Trunc(f) {
		return 0, officialBadRequest("%s 必须是整数", name)
	}
	n := int64(f)
	if n < min || (max > 0 && n > max) {
		if max > 0 {
			return 0, officialBadRequest("%s 必须在 %d 到 %d 之间", name, min, max)
		}
		return 0, officialBadRequest("%s 不能小于 %d", name, min)
	}
	return n, nil
}

// boolean 取可选布尔。
func (v *mmView) boolean(name string) (bool, bool, error) {
	raw, ok, err := v.take(name)
	if err != nil || !ok {
		return false, false, err
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false, officialBadRequest("%s 必须是布尔", name)
	}
	return b, true, nil
}

// ---------------------------------------------------------------------------
// speech.*（4）—— 本仓没有语音识别引擎
// ---------------------------------------------------------------------------

// 官方 `speech.prepare(providerId: string, options?: {downloadSource?: string}) → void`。
func officialSpeechPrepare(p *officialParams) (any, error) {
	view, err := mmOpen(p, "options")
	if err != nil {
		return nil, err
	}
	providerID, err := view.strNonEmpty("providerId")
	if err != nil {
		return nil, err
	}
	downloadSource, _, err := view.str("downloadSource")
	if err != nil {
		return nil, err
	}
	if err := view.finish(); err != nil {
		return nil, err
	}
	if downloadSource != "" {
		return nil, officialNotImplemented(
			"speech.prepare 未接线：本仓没有语音识别引擎，没有可准备的资源（providerId=%q downloadSource=%q 需要 ASR 模型文件、预处理器/VAD、模型下载源与加载器；宿主侧四样都没有）",
			providerID, downloadSource)
	}
	return nil, officialNotImplemented(
		"speech.prepare 未接线：本仓没有语音识别引擎，没有可准备的资源（providerId=%q 需要 ASR 模型文件、预处理器/VAD、模型下载源与加载器；宿主侧四样都没有）",
		providerID)
}

// 官方 `speech.transcribe(request: {audioBase64, providerId?, language?}) →
// {text, audioSeconds, inferenceSeconds}`。
//
// **绝不伪造**：没有 ASR 引擎就不返回任何 text —— 编一段文字冒充转写结果，
// 比明确报未实现糟糕得多（用户会把它当真的听到的话去用）。
func officialSpeechTranscribe(p *officialParams) (any, error) {
	view, err := mmOpen(p, "request")
	if err != nil {
		return nil, err
	}
	audio, err := view.strNonEmpty("audioBase64")
	if err != nil {
		return nil, err
	}
	if _, _, err := view.str("providerId"); err != nil {
		return nil, err
	}
	if _, _, err := view.str("language"); err != nil {
		return nil, err
	}
	if err := view.finish(); err != nil {
		return nil, err
	}
	_ = audio // 只用于"参数确实给了音频"；宿主没有解码器，不解析它
	return nil, officialNotImplemented(
		"speech.transcribe 未接线：本仓没有语音识别引擎（没有 ASR 模型、没有音频解码、没有 VAD、没有推理管线）。拒绝返回任何转写文本")
}

// 官方 `speech.configure(patch?: {providerId?, language?}) → void`。
func officialSpeechConfigure(p *officialParams) (any, error) {
	view, err := mmOpen(p, "patch")
	if err != nil {
		return nil, err
	}
	if _, _, err := view.str("providerId"); err != nil {
		return nil, err
	}
	if _, _, err := view.str("language"); err != nil {
		return nil, err
	}
	if err := view.finish(); err != nil {
		return nil, err
	}
	return nil, officialNotImplemented(
		"speech.configure 未接线：本仓没有语音识别配置（没有 provider 选择、没有语言偏好落点，也没有消费这些设置的识别引擎）")
}

// 官方 `speech.cancelPreparation(providerId: string) → void`。
func officialSpeechCancelPreparation(p *officialParams) (any, error) {
	providerID, err := mmTop(p).strNonEmpty("providerId")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	return nil, officialNotImplemented(
		"speech.cancelPreparation 未接线：本仓没有语音准备任务（providerId=%q 没有对应的下载/加载作业可取消）",
		providerID)
}

// ---------------------------------------------------------------------------
// cotTranslation.translate —— 唯一的真实现
// ---------------------------------------------------------------------------

// 官方 provider 取值（zod `z.union([literal("google"), literal("bing"),
// literal("deepseek-account"), literal("deepseek-official")])`）。
const (
	cotProviderGoogle     = "google"
	cotProviderBing       = "bing"
	cotProviderDSAccount  = "deepseek-account"
	cotProviderDSOfficial = "deepseek-official"
)

// 翻译调用的上限与网关连接信息。
//
// env 键与默认值与 `cmd/dshd/serve.go` 完全一致（那里也是
// `envOr("DSHD_LLM_BASE", "http://127.0.0.1:3425/v1")` 与 `envOr("DSHD_LLM_MODEL",
// "group/deepseek-v4.1-flash-1017")`），这样"宿主跑哪个网关/哪个模型"和"翻译用
// 哪个网关/哪个模型"不会各说一套。
const (
	cotTranslationTimeout  = 90 * time.Second
	cotTranslationMaxChars = 100000

	cotGatewayBaseEnv    = "DSHD_LLM_BASE"
	cotGatewayKeyEnv     = "DSHD_LLM_KEY"
	cotGatewayModelEnv   = "DSHD_LLM_MODEL"
	cotGatewayLegacyEnv  = "DSH_GO_MODEL"
	cotGatewayDefaultURL = "http://127.0.0.1:3425/v1"
	cotGatewayDefaultMdl = "group/deepseek-v4.1-flash-1017"
)

// officialCotTranslationTranslate 实现官方 `cotTranslation.translate`。
//
// 官方语义：把一段文本交给选定的翻译 provider，返回译文（`result` 是裸字符串）。
//
// 本仓的映射（与 RESULT 的映射表一致）：
//   - `deepseek-official` / `deepseek-account` / 省略 → 走**真实模型网关**
//     （`dshgo/internal/llm`，与 `cmd/dshd` 完全同一套 env），返回模型真实输出；
//   - `google` / `bing` → 501：本仓不代理第三方匿名翻译端点；
//   - 模型输出空文本 → 报错，**不拿原文顶替**（拿原文冒充译文比报错更糟）。
func officialCotTranslationTranslate(r *http.Request, p *officialParams) (any, error) {
	view, err := mmOpen(p, "request")
	if err != nil {
		return nil, err
	}
	text, err := view.strValue("text")
	if err != nil {
		return nil, err
	}
	target, err := view.strNonEmpty("targetLanguage")
	if err != nil {
		return nil, err
	}
	source, _, err := view.str("sourceLanguage")
	if err != nil {
		return nil, err
	}
	provider, hasProvider, err := view.str("provider")
	if err != nil {
		return nil, err
	}
	if _, _, err := view.str("sessionId"); err != nil {
		return nil, err
	}
	if err := view.finish(); err != nil {
		return nil, err
	}

	switch {
	case !hasProvider || provider == "" || provider == cotProviderDSOfficial || provider == cotProviderDSAccount:
		// 走真实网关（见下）。
	case provider == cotProviderGoogle || provider == cotProviderBing:
		return nil, officialNotImplemented(
			"cotTranslation.translate 未接线：provider=%q 是第三方匿名翻译端点，本仓不代理匿名翻译服务；改用 deepseek-official / deepseek-account（走本机模型网关）",
			provider)
	default:
		return nil, officialBadRequest(
			"provider=%q 不是官方取值（google / bing / deepseek-account / deepseek-official）", provider)
	}

	if len([]rune(text)) > cotTranslationMaxChars {
		return nil, officialBadRequest("text 超过 %d 个字符上限", cotTranslationMaxChars)
	}
	if text == "" {
		// 空文的译文就是空文：不需要打模型，也不能算"编内容"。
		return "", nil
	}
	return cotTranslateViaGateway(r.Context(), text, target, strings.TrimSpace(source))
}

// cotGatewayConfig 解析本机模型网关连接信息（env 优先，默认值跟宿主一致）。
func cotGatewayConfig() (base, key, model string) {
	base = strings.TrimSpace(os.Getenv(cotGatewayBaseEnv))
	if base == "" {
		base = cotGatewayDefaultURL
	}
	key = strings.TrimSpace(os.Getenv(cotGatewayKeyEnv))
	model = strings.TrimSpace(os.Getenv(cotGatewayModelEnv))
	if model == "" {
		model = strings.TrimSpace(os.Getenv(cotGatewayLegacyEnv))
	}
	if model == "" {
		model = cotGatewayDefaultMdl
	}
	return base, key, model
}

// cotTranslatePrompt 只给"翻译"这一件事：目标语言 + 原文。
//
// 提示词按"输出即成品"写：客户端会把 result 原样贴回界面，多一句解释就是脏数据。
func cotTranslatePrompt(text, target, source string) (system, user string) {
	var b strings.Builder
	b.WriteString("You are a translation engine. Translate the user's text into the language identified by the BCP 47 tag ")
	b.WriteString(target)
	b.WriteString(". Output ONLY the translated text: no quotes, no preamble, no explanations, no notes. ")
	b.WriteString("Preserve line breaks and inline code verbatim. If the text is already entirely in that language, return it unchanged.")
	if source != "" {
		b.WriteString(" The source text is expected to be in the BCP 47 language ")
		b.WriteString(source)
		b.WriteString("; translate from it even if it looks otherwise.")
	}
	return b.String(), text
}

// cotTranslateViaGateway 用既有 llm client 往真实网关发一次翻译请求。
//
// 返回模型真实输出；任何"没有真值"的情况（连不上、HTTP 错、空文本）都返回错误，
// 绝不回落到原文或空串。
func cotTranslateViaGateway(ctx context.Context, text, target, source string) (string, error) {
	base, key, model := cotGatewayConfig()
	system, user := cotTranslatePrompt(text, target, source)

	ctx, cancel := llm.Timeout(ctx, cotTranslationTimeout)
	defer cancel()

	resp, err := llm.New(base, key).Stream(ctx, llm.Request{
		Model: model,
		Messages: []llm.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}, nil)
	if err != nil {
		// 网关错误文本可能带回凭据：这里是唯一清洗点。
		return "", officialInternal("cotTranslation.translate 调模型网关失败（%s model=%s）：%s",
			base, model, llm.RedactCredential(err.Error(), key))
	}
	if resp == nil {
		return "", officialInternal("cotTranslation.translate 没有拿到网关响应（model=%s）", model)
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		return "", officialInternal(
			"cotTranslation.translate 的模型输出是空文本（model=%s finishReason=%q）：不拿原文顶替译文，请重试或换模型",
			model, resp.FinishReason)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// officeToPdf.render —— 本仓没有 Office 渲染器
// ---------------------------------------------------------------------------

// 官方 `officeToPdf.render(workspaceFileScopeId, path, priority:
// "foreground"|"background") → RenderedDocumentBytes{missingFonts, generation,
// offset, data, eof, absolutePath, version, bytes?}`。
func officialOfficeToPdfRender(p *officialParams) (any, error) {
	// 官方这个位置的 `name` 是 workspaceFileScope、wire 名是 workspaceFileScopeId；
	// 本仓会话 id 另有 agentId / sessionId 两种既有写法，三种都认。
	scope, _, err := p.strAlias("workspaceFileScopeId", "agentId", "sessionId")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(scope) == "" {
		return nil, officialBadRequest("workspaceFileScopeId 是空串")
	}
	path, err := mmTop(p).strNonEmpty("path")
	if err != nil {
		return nil, err
	}
	priority, err := mmTop(p).strNonEmpty("priority")
	if err != nil {
		return nil, err
	}
	if priority != "foreground" && priority != "background" {
		return nil, officialBadRequest("priority 只能是 foreground / background，拿到 %q", priority)
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	return nil, officialNotImplemented(
		"officeToPdf.render 未接线：本仓没有 Office 渲染器（官方是 LibreOffice（libreoffice-kit）+ 后台转换队列 + 字体缺失检测）。path=%q priority=%s；不拿文本抽取或空 PDF 冒充渲染结果",
		path, priority)
}

// ---------------------------------------------------------------------------
// pluginMarket.*（3）—— 本仓只有"已安装插件目录"，没有市场索引服务
// ---------------------------------------------------------------------------

// 官方 `pluginMarket.search(url, query: MarketQuery) → MarketSearchResult`。
func officialPluginMarketSearch(p *officialParams) (any, error) {
	view, err := mmOpen(p, "query")
	if err != nil {
		return nil, err
	}
	url, err := view.strNonEmpty("url")
	if err != nil {
		return nil, err
	}
	if _, _, err := view.str("text"); err != nil {
		return nil, err
	}
	if _, _, err := view.str("category"); err != nil {
		return nil, err
	}
	if _, err := view.strings("labels", false); err != nil {
		return nil, err
	}
	if err := view.object("sort", true); err != nil {
		return nil, err
	}
	if _, err := view.intNonNegative("offset", 0, 0); err != nil {
		return nil, err
	}
	if _, err := view.intNonNegative("limit", 1, 100); err != nil {
		return nil, err
	}
	if _, _, err := view.boolean("refresh"); err != nil {
		return nil, err
	}
	if _, _, err := view.number("fetchedAt"); err != nil {
		return nil, err
	}
	if err := view.finish(); err != nil {
		return nil, err
	}
	return nil, officialNotImplemented(
		"pluginMarket.search 未接线：本仓没有插件市场索引服务（索引源设置、索引文档取回与 schema 校验、模糊匹配/排序/分页、安装状态计算都没有）。url=%q", url)
}

// 官方 `pluginMarket.readEntryDetail(url, name, version, language, options?) →
// {status:"found",origin,markdown,document} | {status:"none"}`。
func officialPluginMarketReadEntryDetail(p *officialParams) (any, error) {
	view, err := mmOpen(p, "options")
	if err != nil {
		return nil, err
	}
	url, err := view.strNonEmpty("url")
	if err != nil {
		return nil, err
	}
	name, err := view.strNonEmpty("name")
	if err != nil {
		return nil, err
	}
	version, err := view.strNonEmpty("version")
	if err != nil {
		return nil, err
	}
	language, err := view.strNonEmpty("language")
	if err != nil {
		return nil, err
	}
	if _, err := view.strings("npmFileSources", false); err != nil {
		return nil, err
	}
	if _, _, err := view.boolean("refresh"); err != nil {
		return nil, err
	}
	if err := view.finish(); err != nil {
		return nil, err
	}
	return nil, officialNotImplemented(
		"pluginMarket.readEntryDetail 未接线：本仓没有市场索引、也没有 npm 文件 CDN / Git 提交读取器，读不到条目长文档。url=%q name=%q version=%q language=%q",
		url, name, version, language)
}

// 官方 `pluginMarket.setIndexes(urls: string[]) → void`。
func officialPluginMarketSetIndexes(p *officialParams) (any, error) {
	urls, err := mmTop(p).strings("urls", true)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	return nil, officialNotImplemented(
		"pluginMarket.setIndexes 未接线：本仓没有插件市场索引源设置项（没有可写位置、没有 plugin-market/changed 事件），拒绝假装写成功（%d 个 url 未落盘）", len(urls))
}

// ---------------------------------------------------------------------------
// pluginRecommendations.describe —— 本仓没有推荐卡记录
// ---------------------------------------------------------------------------

// 官方 `pluginRecommendations.describe(agentId, callId) →
// {status:"available",title,description,enabled} | {status:"refused",reason}`。
//
// 官方这个结果自带 `refused{reason:"unavailable"}`，看上去像"可以合法地拒绝"；
// 但那会假装"推荐子系统存在、只是这一刻不可用"。本仓整条链路（推荐卡是模型侧
// 工具产物 + 卡文本目录）都不存在，所以报 501 并点名缺什么。
func officialPluginRecommendationsDescribe(p *officialParams) (any, error) {
	agentID, err := p.sessionRef()
	if err != nil {
		return nil, err
	}
	callID, err := mmTop(p).strNonEmpty("callId")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	return nil, officialNotImplemented(
		"pluginRecommendations.describe 未接线：本仓没有插件推荐子系统（推荐卡记录与卡文本目录都不存在）。agentId=%q callId=%q",
		agentID, callID)
}

// ---------------------------------------------------------------------------
// dynamicCordisRunner.*（11）—— 按红线 R6 明确不做
// ---------------------------------------------------------------------------

// officialCordisSpec 是一个 cordis 方法的官方参数清单（wire 名）。
//
// required 里的键必须出现且非 null；optional 里的键可以不给。**内部形状不做
// 校验**：这些方法在本宿主上一律不执行（R6），深校验只会让"反正要 501"的调用
// 多绕一步、也不会让任何东西变得更安全；但顶层键名仍然严格（官方 strictCodec
// 是 additionalProperties:false），这样写错字段名依旧是 400 而不是 501。
type officialCordisSpec struct {
	required []string
	optional []string
}

var officialCordisSpecs = map[Method]officialCordisSpec{
	MethodDynamicCordisRunnerGetClientCode: {
		required: []string{"agentId", "pluginId", "pluginRunId"},
	},
	MethodDynamicCordisRunnerInvoke: {
		required: []string{"pluginId", "pluginRunId", "method", "args"},
	},
	MethodDynamicCordisRunnerReportClientGuardFailure: {
		required: []string{"agentId", "pluginId", "pluginRunId", "failure"},
	},
	MethodDynamicCordisRunnerReportRenderFailure: {
		required: []string{"agentId", "pluginId", "pluginRunId", "failure"},
	},
	MethodDynamicCordisRunnerResolveInspectQuery: {
		required: []string{"agentId", "requestId", "resolution"},
	},
	MethodDynamicCordisRunnerResolveRequestRun: {
		required: []string{"requestId"},
		optional: []string{"resolution"},
	},
	MethodDynamicCordisRunnerRunHostHalf: {
		required: []string{"agentId", "pluginId", "packageId", "mode", "requestId", "approveFutureVersions"},
	},
	MethodDynamicCordisRunnerSettleUserRun: {
		required: []string{"agentId", "pluginId"},
		optional: []string{"resolution"},
	},
	MethodDynamicCordisRunnerStopFromPanel: {
		required: []string{"agentId", "pluginId"},
	},
	MethodDynamicCordisRunnerSyncInspectManifest: {
		required: []string{"providers"},
	},
	MethodDynamicCordisRunnerUndefineFromPanel: {
		required: []string{"agentId", "pluginId"},
	},
}

// officialCordisRefused 校验顶层键名后，按 R6 给出"明确不做"的 501。
//
// 为什么不"尽量实现一部分"：这 11 个方法没有一个只是数据搬运 —— 每一个都以
// "在宿主里执行 Client/Host 插件 JS"为语义（取 Client 代码、跑 Host 半边、
// 结算一次运行、同步 inspect 清单…）。"宿主不嵌 JS 运行时"是 **R6**，所以它们是
// 设计边界，不是能力欠账。
func officialCordisRefused(method Method, spec officialCordisSpec, p *officialParams) (any, error) {
	agentID := ""
	for _, name := range spec.required {
		raw, ok := p.take(name)
		if !ok {
			return nil, officialBadRequest("缺 %s", name)
		}
		if string(raw) == "null" {
			return nil, officialBadRequest("%s 是 null：官方这个参数必填", name)
		}
		if name == "agentId" {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, officialBadRequest("agentId 必须是字符串")
			}
			if strings.TrimSpace(s) == "" {
				return nil, officialBadRequest("agentId 是空串")
			}
			agentID = s
		}
	}
	for _, name := range spec.optional {
		raw, ok := p.take(name)
		if !ok {
			continue
		}
		if string(raw) == "null" {
			return nil, officialBadRequest("%s 是 null：要么不给，要么给一个值", name)
		}
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if agentID != "" {
		return nil, officialNotImplemented(
			"%s 按红线 R6 明确不做：宿主不嵌 JS 运行时（也不 fork/vendor dsc），而这个方法以「在宿主里执行 Cordis 插件 JS 并结算那次运行」为语义（agentId=%q），没有任何合法执行路径。这不是能力欠账，是设计边界；需要动态插件执行请走独立进程方案",
			method, agentID)
	}
	return nil, officialNotImplemented(
		"%s 按红线 R6 明确不做：宿主不嵌 JS 运行时（也不 fork/vendor dsc），而这个方法以「在宿主里执行 Cordis 插件 JS 并结算那次运行」为语义，没有任何合法执行路径。这不是能力欠账，是设计边界；需要动态插件执行请走独立进程方案",
		method)
}
