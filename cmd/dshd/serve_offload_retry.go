package main

// 官方插件 80（`@deepseek-ai/dsh-compaction-image-offload`）的
// `agent/request-error → {kind:"retry"}` 编排在**本宿主请求路径**上的落地。
//
// # 官方那两行钩子（app.asar: dsh-compaction-image-offload/lib/index.js:88-96）
//
//	ctx.on("agent/request-error", ({agent, failure, signal}, next) => {
//	    if (failure.code !== IMAGE_OFFLOAD_REQUIRED_CODE || failure.offloadImages === undefined || signal.aborted) return next();
//	    if (!offloadOldestImages(agent.session, agent.session.surface.nodes, failure.offloadImages)) return next();
//	    return Promise.resolve({kind: "retry"});
//	});
//	ctx.on("compaction/summary-error", ...同一条判据，摘要请求也走同一份决策...);
//
// 官方是"路由自己算预算、超了就抛 IMAGE_OFFLOAD_REQUIRED（带 offloadImages 张数）"，
// 本宿主走的是 OpenAI 兼容网关：预算由**上游**判，我们只能从 4xx 的失败体里认。
// 所以判据落成两条：
//  1. 失败体里明确有 `IMAGE_OFFLOAD_REQUIRED`（含或不含 offloadImages）→ 照官方的张数，
//     没给张数就按本仓预算（session.DefaultImageOffloadBudget）算；
//  2. 4xx + 失败体同时出现 "image" 与预算类词（too large / too many / exceed /
//     payload / limit…）→ 视为同一类失败，至少降最旧的一张。
//
// # 只降级一次（防抖）
//
// 一次 turn 只允许一次降级重试：`offloadRetrier` 用掉就置位。第二次失败**如实上报**，
// 不再降级、不再重试——无限降级会把"图太多"变成"图片一张张消失"，用户根本看不出
// 发生了什么；而且每次重试都是一次真实计费请求。
//
// # 决策落在哪里
//
// 降级本身只改**这一次请求**里的图片段（换成官方占位文本）。落盘时在本轮
// user/message 之后追加一条 `image/offload` 决策（见 persistModelEvents），
// durable 块一个字节都不改——v4 是 append-only，重新 Surface 之后最旧的几张
// 已经是占位文本，模型仍然能按附件 id 取回原件。
//
// # compaction/summary-error 为什么没有对应接线
//
// 本宿主摘要请求是**纯文本**的（`internal/compact/compact.go:163-168` 只拼
// `llm.Message{Content: …}`，没有 image 段），所以 IMAGE_OFFLOAD_REQUIRED 在摘要
// 路径上不可达——那条钩子在这里没有可触发的输入，接上去只会是一段永不执行的代码。
// 这条差异记在 RESULT 的"未验项/差异"里，不假装接上了。

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// imageOffloadRequiredCode 是官方 `IMAGE_OFFLOAD_REQUIRED_CODE`
// （dsh-llm/lib/index.js:236）。
const imageOffloadRequiredCode = "IMAGE_OFFLOAD_REQUIRED"

// imageBudgetMarkers 是"上游在说图片/请求体超预算"的措辞。
//
// 只按 4xx + image + 这些词认定：`invalid image format` 一类**不会**命中，
// 否则一次坏图会被误判成"超预算"，把用户其它图白白降级掉。
var imageBudgetMarkers = []string{
	"too large", "too many", "exceed", "exceeded", "payload", "entity too large",
	"budget", "limit", "maximum", "too big",
}

// offloadImagesForError 判定一次请求失败是不是"图片预算超限、降级可解"。
//
// 返回 (张数, 是否命中)：张数为 0 表示"命中但上游没给张数"，调用方按预算自己算。
func offloadImagesForError(err error) (int, bool) {
	var gateway *llm.GatewayError
	if !errors.As(err, &gateway) {
		return 0, false
	}
	if gateway.Kind != llm.GatewayStatus || gateway.Code < 400 || gateway.Code > 499 {
		return 0, false
	}
	if count, ok := explicitOffloadCount(gateway.Body); ok {
		return count, true
	}
	lower := strings.ToLower(gateway.Body)
	if !strings.Contains(lower, "image") {
		return 0, false
	}
	for _, marker := range imageBudgetMarkers {
		if strings.Contains(lower, marker) {
			return 0, true
		}
	}
	return 0, false
}

// explicitOffloadCount 从失败体里认官方那个 code 与张数。
//
// 上游可能把它放在 `{error:{code,offloadImages}}`，也可能直接放在顶层
// （OpenAI 兼容网关常见 `{code, message}`）。两种都认；认到 code 但没张数
// 就回 (0,true)，交给预算算。
func explicitOffloadCount(body string) (int, bool) {
	if strings.TrimSpace(body) == "" {
		return 0, false
	}
	var payload struct {
		Code          string `json:"code"`
		OffloadImages int    `json:"offloadImages"`
		Error         struct {
			Code          string `json:"code"`
			Type          string `json:"type"`
			OffloadImages int    `json:"offloadImages"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err == nil {
		if payload.Error.OffloadImages > 0 {
			return payload.Error.OffloadImages, true
		}
		if payload.OffloadImages > 0 {
			return payload.OffloadImages, true
		}
		if payload.Error.Code == imageOffloadRequiredCode || payload.Error.Type == imageOffloadRequiredCode ||
			payload.Code == imageOffloadRequiredCode {
			return 0, true
		}
	}
	// 非 JSON 的失败体（网关直接回一段文本）也认 code 本身。
	if strings.Contains(body, imageOffloadRequiredCode) {
		return 0, true
	}
	return 0, false
}

// offloadRetrier 是"一次 turn 一次降级"的防抖开关。
//
// 它不是并发结构：一个 turn 的 step 循环本来就是串行的。
type offloadRetrier struct {
	used bool
}

// tryOffloadRetry 在请求失败后决定是否降级重试。
//
// 参数：
//
//	err          这一轮的失败（原样，不脱敏；判断只看 status/body）
//	initial      本轮首条用户输入（图片在 message.Parts 里）
//	history      请求历史（in-place 改 initialIndex 处的那条，重试就用新历史）
//	initialIndex 本轮首条用户消息在 history 里的下标（-1 = 不在历史里，不改历史）
//
// 返回 (降级后的输入, 被降级的图片序号, 是否真的重试)。序号是**消息内深度优先**
// 的图片出现序号（从 0 起），与官方 `image/offload` 的 imageIndexes 同口径。
func (r *offloadRetrier) tryOffloadRetry(err error, initial modelTurnInput, history []llm.Message, initialIndex int) (modelTurnInput, []int, bool) {
	if r.used {
		return initial, nil, false
	}
	count, required := offloadImagesForError(err)
	if !required {
		return initial, nil, false
	}
	degraded, indexes := degradePromptImages(initial, count)
	if len(indexes) == 0 {
		// 这一轮没有可降级的图片：把原始失败原样交回去，不假装能自愈。
		return initial, nil, false
	}
	r.used = true
	if initialIndex >= 0 && initialIndex < len(history) {
		history[initialIndex] = degraded.message
	}
	return degraded, indexes, true
}

// degradePromptImages 把本轮最旧的 count 张图换成官方占位文本。
//
// 只动 `message.Parts`（有序段）：图片按出现顺序与 durable 块里的图片一一对应，
// 所以第 k 个图片段就是第 k 张 durable 图片——占位文案要的附件 id 直接从它取。
// count<=0 时按本仓预算算；算出来还是 0 就降 1 张（上游已经说超预算了）。
func degradePromptImages(initial modelTurnInput, count int) (modelTurnInput, []int) {
	images := make([]session.ContentBlock, 0, 4)
	for _, block := range initial.durable {
		if session.IsImageBlock(block) {
			images = append(images, block)
		}
	}
	if len(images) == 0 || len(initial.message.Parts) == 0 {
		return initial, nil
	}
	if count <= 0 {
		count = imageOffloadPrefixCount(images)
	}
	if count <= 0 {
		count = 1
	}
	if count > len(images) {
		count = len(images)
	}
	parts := make([]llm.MessagePart, len(initial.message.Parts))
	copy(parts, initial.message.Parts)
	imageIndex := 0
	degradedCount := 0
	for i, part := range parts {
		if part.Type != llm.PartImageURL {
			continue
		}
		if imageIndex >= count {
			break
		}
		parts[i] = llm.TextPart(session.OffloadedImageText(images[imageIndex], nil))
		imageIndex++
		degradedCount++
	}
	if degradedCount == 0 {
		return initial, nil
	}
	degraded := initial
	degraded.message = initial.message
	degraded.message.Parts = parts
	degraded.message.Images = nil
	indexes := make([]int, degradedCount)
	for i := range indexes {
		indexes[i] = i
	}
	return degraded, indexes
}

// imageOffloadPrefixCount 按本仓默认预算算"还得降几张"。
//
// 走的是官方 `offloadedImagePrefixCount` 的移植（session.OffloadedImagePrefixCount），
// 预算也是官方 pi-ai 形状的默认值——与 surface 侧 `RequiredImageOffload` 同一口径。
func imageOffloadPrefixCount(images []session.ContentBlock) int {
	budget := session.DefaultImageOffloadBudget()
	lengths := make([]int64, 0, len(images))
	for _, block := range images {
		lengths = append(lengths, session.RepresentedBytes(block.ByteSize, budget.Representation))
	}
	return session.OffloadedImagePrefixCount(lengths, budget)
}

// emitImageOffloadRetry 把"这一轮正在降级重试"如实推给客户端。
//
// 官方事件名是 `llm/retry`（SessionEventMap）。这里推的是**同名的真实事件**：
// 客户端据此知道"你刚看到的半截回答被打断了，宿主降级后重发了一次"，
// 而不是自己猜为什么同一轮出现了两次 step/start。
func emitImageOffloadRetry(journals *transport.Journals, pub func(string, transport.ServerFrame),
	sessionID string, turn, step, images int) {
	data, err := json.Marshal(map[string]any{
		"turn": turn, "step": step, "kind": "image-offload", "offloadImages": images,
	})
	if err != nil {
		return
	}
	emitFrame(journals, pub, sessionID, "llm/retry", data)
}

// offloadTargets 把"这一轮降级了哪些图片序号"铺成官方 image/offload 的 targets。
//
// 送达目标是本轮 user/message 的 seq（落盘时才分配，见 persistModelEvents）。
func offloadTargets(userSeq int64, indexes []int) []session.ImageOffloadTarget {
	if userSeq < 0 || len(indexes) == 0 {
		return nil
	}
	sorted := make([]int, len(indexes))
	copy(sorted, indexes)
	sort.Ints(sorted)
	return []session.ImageOffloadTarget{{Seq: userSeq, ImageIndexes: sorted}}
}
