package session

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"dshgo/codecs/v4"
)

// 本文件是官方 0.2.2-rc.1 插件 `@deepseek-ai/dsh-compaction-image-offload`
// 的宿主侧移植（`docs/official-plugins-022-map.md` 第 80 项，MISSING → DONE）。
//
// 为什么需要它：附件链（W79–W98）已经能把真实图片内联进请求，但**没有任何预算
// 概念**。图片随轮次线性累积，每轮请求重放全部历史图片，迟早撞上路由的请求体
// 上限——那时候整轮 4xx 硬失败，用户看到的是"这个会话再也发不出消息了"。
// 官方的做法不是拒发，而是**把最旧的那几张永久降级成占位文本 + 附件指针**，
// 然后重试；模型仍然知道那里曾经有图、也知道按哪个 id 取回原件。
//
// 官方语义逐条对上原件（file:line 见下），预算与文案一字不改：
//   - 事件：`image/offload`，data 恰好一个键 `targets`；每个 target 恰好
//     `seq` + `imageIndexes` 两个键（lib/index.js:42、lib/types/projection.js:16-45）。
//   - 选择：按**模型请求顺序**从最旧开始；只挑 `user/message` 与 `tool/result`，
//     assistant 节点是模型自己的输出，永不被选（lib/index.js:19、lib/types/image-offload.js:19）。
//   - 序号：`imageIndexes` 是**消息内深度优先的图片出现序号**，并且**计到已退役的
//     那些**——只跳过 `offloaded === true` 的（lib/types/image-offload.js:29-35）。
//   - 预算：`offloadedImagePrefixCount`（dsh-llm/lib/types/content.js:183-204）——
//     数量超限与字节超限各自按 quantum 上取整，再取两者更靠后的那个前缀。
//   - 文案：`offloadedImageText`（dsh-llm/lib/types/content.js:73-79）。
//
// 与官方的差异（全部写进 RESULT，这里只标注）：
//  1. 官方内容块是 `type:"image"` + 嵌套 `attachment{attachmentId,name,mediaType,
//     width,height}`；本仓 v4 的图片块是 `type:"attachment"` + 扁平
//     `mime/attachmentId/...`（internal/session/blocks.go:12-30），所以
//     "是不是图片"由 MIME 判定，不按块类型名。
//  2. 官方在 Session 提交前就把非法 target 拒掉（抛错）；本仓读路径不写盘、
//     也不能因为一条脏事件让整份历史读不出来，所以 `Surface` 侧整条丢弃，
//     `ParseImageOffloadTargets` 保留严格校验给写入端与测试。

// ImageOffloadEventType 是官方插件写下的决策事件类型。
//
// 它属于 `MESSAGE_PROJECTION_EVENT_TYPES = {'image/offload'}`：不是 surface
// 事件（不改 surfaceOp、不占 surface 位置），但会改变模型可见内容
// （research/21-dsh-session-v4.md:189）。
const ImageOffloadEventType = "image/offload"

// 官方 pi-ai 路由的默认预算（dsh-llm-pi-ai/lib/index.js:954,1079,1136，
// 以及 dsh-llm/lib/types/content.js:189-190 的 quantum 缺省 1）。
//
// 为什么用这一组：DSH-GO 走的就是 OpenAI 兼容的 chat/completions + 内联
// base64 data URL（llm.MessagePart/ImagePart），与 pi-ai 路由同一形状；
// deepseek Messages 路由的 Files/base64 双预算是另一条形状，对照值见下面
// DeepSeek* 那组常量。
const (
	// DefaultMaxRequestImageBytes 是单个请求里全部图片 base64 后的总字节上限。
	DefaultMaxRequestImageBytes = 20 * 1024 * 1024
	// DefaultMaxRequestImages 是单个请求里的图片张数上限。
	DefaultMaxRequestImages = 50
	// DefaultImageOffloadCountQuantum 是每次退役的张数量子（官方 min(10, maxImages)）。
	DefaultImageOffloadCountQuantum = 10
	// DefaultImageOffloadByteQuantum 是每次退役的字节量子（pi-ai 未设，官方缺省 1）。
	DefaultImageOffloadByteQuantum = 1
)

// 官方 deepseek Messages 路由的对照预算（llm-deepseek/lib/index.js:411-419, 210-213）。
// 本仓未接这条路由，保留数值只为"预算口径以官方原件为准"可核对。
const (
	// DeepSeekMaxRequestFilesBytes 是 raw（Files 引用）表示的单请求字节上限。
	DeepSeekMaxRequestFilesBytes = 128 * 1024 * 1024
	// DeepSeekMaxInlineRequestImageBytes 是 base64（内联回退）表示的字节上限。
	DeepSeekMaxInlineRequestImageBytes = 20 * 1024 * 1024
	// DeepSeekMaxImagesPerRequest 是单请求图片张数上限。
	DeepSeekMaxImagesPerRequest = 600
	// DeepSeekImageOffloadByteQuantum 是 raw 表示的退役字节量子。
	DeepSeekImageOffloadByteQuantum = 64 * 1024 * 1024
	// DeepSeekInlineImageOffloadByteQuantum 是 base64 表示的退役字节量子。
	DeepSeekInlineImageOffloadByteQuantum = 10 * 1024 * 1024
	// DeepSeekImageOffloadCountQuantum 是 deepseek 路由的张数量子。
	DeepSeekImageOffloadCountQuantum = 20
)

// 表示（representation）取值，与官方 `budget.representation` 一致。
const (
	// RepresentationRaw 按图片原始字节计量（Files/引用形状）。
	RepresentationRaw = "raw"
	// RepresentationBase64 按 base64 膨胀后的字节计量（内联形状）。
	RepresentationBase64 = "base64"
)

// ImageOffloadTarget 是官方 `ImageOffloadTarget`：一处消息事件 + 该消息内要退役的
// 深度优先图片序号。序号**严格递增**且非负（官方 projection.js:35-41）。
type ImageOffloadTarget struct {
	Seq          int64 `json:"seq"`
	ImageIndexes []int `json:"imageIndexes"`
}

// ImageOffloadData 是 `image/offload` 事件的 data 形状：**恰好**一个键 `targets`。
type ImageOffloadData struct {
	Targets []ImageOffloadTarget `json:"targets"`
}

// ImageAccess 是这张图片在当前工具执行世界里的只读路径。
//
// 对应官方 `resolveImageAttachmentAccess` 的返回值：拿不到（provider 没暴露映射）
// 就传 nil，占位文案走"没有可读路径"那一支。本仓的 attachment store 在
// cmd/dshd 侧，session 包不认识它，所以这两者之间只有这一个字段的契约。
type ImageAccess struct {
	// ReadonlyPath 是只读副本的绝对路径（模型可以自己 read 它）。
	ReadonlyPath string
}

// ImageOffloadBudget 是官方 `budget` 结构（count/byte 双预算 + 两个退役量子）。
// 零值表示"该维度不限制"，与官方 `undefined` 同义。
type ImageOffloadBudget struct {
	// Representation 是计量表示：RepresentationRaw 或 RepresentationBase64。
	Representation string
	// MaxBytes 是字节上限；0 = 不限制。
	MaxBytes int64
	// MaxImages 是张数上限；0 = 不限制。
	MaxImages int
	// ByteQuantum 是字节退役量子；0 = 1。
	ByteQuantum int64
	// CountQuantum 是张数退役量子；0 = 1。
	CountQuantum int
}

// DefaultImageOffloadBudget 返回本仓路由（pi-ai 形状）的官方默认预算。
func DefaultImageOffloadBudget() ImageOffloadBudget {
	return ImageOffloadBudget{
		Representation: RepresentationBase64,
		MaxBytes:       DefaultMaxRequestImageBytes,
		MaxImages:      DefaultMaxRequestImages,
		ByteQuantum:    DefaultImageOffloadByteQuantum,
		CountQuantum:   DefaultImageOffloadCountQuantum,
	}
}

// ImageExtension 是官方 `extension(mediaType)`（dsh-llm/lib/types/content.js:28-36）。
//
// 官方对未知类型走 `assertNever` 抛错；这里返回空串，由调用方省掉那句
// "Copy to a writable path ending in …"——附件 store 只收这四种图片
// （docs/attachments.md:9），空串在可达路径上不会出现。
func ImageExtension(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ""
}

// IsImageBlock 判断一个 v4 内容块是不是图片出现位置。
//
// 差异点 1：官方按 `block.type === 'image'` 认；本仓的图片是
// `type:"attachment"` + `mime:"image/*"`，所以按 MIME 认。`type:"image"` 这一支
// 只为读官方语料兼容（本仓不产出它，块上仍是扁平字段）。
func IsImageBlock(b ContentBlock) bool {
	switch b.Type {
	case "attachment", "image":
		return strings.HasPrefix(b.MIME, "image/")
	}
	return false
}

// jsonQuote 是官方 `quoted()`（JSON.stringify）的 Go 等价物。
func jsonQuote(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(raw)
}

// imageIdentity 是官方 `imageIdentity(ref)`（content.js:24-27）。
func imageIdentity(b ContentBlock) string {
	if b.Name == "" {
		return b.AttachmentID
	}
	return jsonQuote(b.Name) + " (" + b.AttachmentID + ")"
}

// normalizedAccessText 是官方 `normalizedAccessText(ref, access)`（content.js:37-41）。
func normalizedAccessText(b ContentBlock, access *ImageAccess) string {
	text := " Normalized copy (read-only; may be resized or re-encoded): " +
		jsonQuote(access.ReadonlyPath) +
		" (" + strconv.Itoa(b.Width) + "x" + strconv.Itoa(b.Height) + "px, " + b.MIME + ")." +
		" Source dimensions, format, and byte size may differ."
	if ext := ImageExtension(b.MIME); ext != "" {
		text += " Copy to a writable path ending in " + ext + " before editing."
	}
	return text
}

// OffloadedImageText 是官方 `offloadedImageText(ref, access)`（content.js:73-79）：
// 退役一张图片后模型看到的那段稳定占位。**必须**带附件身份（id），
// 模型就是靠它取回原件的。
func OffloadedImageText(b ContentBlock, access *ImageAccess) string {
	identity := "image omitted to fit request image limits; " + imageIdentity(b) + "."
	if access == nil {
		return "[" + identity + " No local normalized image path is available;" +
			" ask the user to attach it again if needed.]"
	}
	return "[" + identity + normalizedAccessText(b, access) + "]"
}

// Base64Length 是官方 `base64Length(bytes)`（content.js:141-143）：含 padding 的
// base64 长度。整数算术，不做浮点。
func Base64Length(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + 2) / 3 * 4
}

// RepresentedBytes 把一张图片的原始字节折成它在预算里占的字节数。
func RepresentedBytes(n int64, representation string) int64 {
	if representation == RepresentationBase64 {
		return Base64Length(n)
	}
	return n
}

// OffloadedImagePrefixCount 是官方 `offloadedImagePrefixCount(lengths, budget)`
// （content.js:183-204）的逐行移植：给定"按最旧到最新排好的每张图字节数"，
// 返回要退役的前缀张数。
//
// 两处反直觉但官方如此：
//   - 数量超限与字节超限**各自**按 quantum 上取整，取更靠后的那个前缀；
//   - 当 quantum 为 1 时判据是 `removedBytes >= removeBytes`，否则是
//     `removedBytes > removeBytes`——差一个字节，官方就是这么写的。
func OffloadedImagePrefixCount(lengths []int64, budget ImageOffloadBudget) int {
	var total int64
	for _, n := range lengths {
		total += n
	}
	var excessCount int
	if budget.MaxImages > 0 {
		if n := len(lengths) - budget.MaxImages; n > 0 {
			excessCount = n
		}
	}
	var excessBytes int64
	if budget.MaxBytes > 0 {
		if n := total - budget.MaxBytes; n > 0 {
			excessBytes = n
		}
	}
	if excessCount == 0 && excessBytes == 0 {
		return 0
	}
	countQuantum := budget.CountQuantum
	if countQuantum <= 0 {
		countQuantum = 1
	}
	byteQuantum := budget.ByteQuantum
	if byteQuantum <= 0 {
		byteQuantum = 1
	}
	removeCount := 0
	if excessCount > 0 {
		removeCount = (excessCount+countQuantum-1)/countQuantum*countQuantum
	}
	var removeBytes int64
	if excessBytes > 0 {
		removeBytes = (excessBytes+byteQuantum-1)/byteQuantum*byteQuantum
	}
	// 走一遍：累计到两种目标都满足为止。
	count := 0
	var removedBytes int64
	for _, imageBytes := range lengths {
		byteTargetMet := removeBytes == 0 ||
			(byteQuantum == 1 && removedBytes >= removeBytes) ||
			(byteQuantum != 1 && removedBytes > removeBytes)
		if count >= removeCount && byteTargetMet {
			break
		}
		removedBytes += imageBytes
		count++
	}
	return count
}

// RequiredImageOffload 是官方 `requiredImageOffload(messages, budget, versionBytes)`
// （content.js:215-226）：这条派生请求还要再退役多少张最旧的图片才能在预算内。
// 已经在 surface 上标了 `Offloaded` 的出现位置不计入（它们现在是占位文本）。
func RequiredImageOffload(surface []SurfaceMsg, budget ImageOffloadBudget) int {
	lengths := make([]int64, 0, 8)
	for _, node := range surface {
		for _, img := range node.Images {
			if img.Offloaded {
				continue
			}
			lengths = append(lengths, RepresentedBytes(img.Block.ByteSize, budget.Representation))
		}
	}
	return OffloadedImagePrefixCount(lengths, budget)
}

// SelectOldestImages 是官方 `offloadOldestImages` 的**选择**部分
// （lib/types/image-offload.js:14-44）：按模型请求顺序从最旧开始，
// 挑 count 个还没退役的出现位置，按消息聚成 target。
//
// 跳过 assistant 节点的方式是"Surface 上只有 user/tool 节点带 Images"
// （见 surfaceImages），不是在这里再判一次类型。
func SelectOldestImages(surface []SurfaceMsg, count int) []ImageOffloadTarget {
	if count <= 0 {
		return nil
	}
	var targets []ImageOffloadTarget
	for _, node := range surface {
		if count == 0 {
			break
		}
		var indexes []int
		for _, img := range node.Images {
			if count == 0 {
				break
			}
			if img.Offloaded {
				continue
			}
			indexes = append(indexes, img.Index)
			count--
		}
		if len(indexes) > 0 {
			targets = append(targets, ImageOffloadTarget{Seq: node.Seq, ImageIndexes: indexes})
		}
	}
	return targets
}

// RecordImageOffload 是官方 `offloadOldestImages` 的**落盘**部分：选中后追加一条
// `image/offload` 决策事件。返回是否真的还有可退役的出现位置。
//
// 为什么是 append 而不是改写：v4 是 append-only，被"降级"的那些事件一个字节都
// 不动；退役只发生在派生投影里。撤销降级的唯一手段就是那条事件本身不存在。
func RecordImageOffload(log *Log, surface []SurfaceMsg, count int) (bool, error) {
	targets := SelectOldestImages(surface, count)
	if len(targets) == 0 {
		return false, nil
	}
	if err := log.Append(ImageOffloadEventType, ImageOffloadData{Targets: targets}); err != nil {
		return false, err
	}
	return true, nil
}

// OffloadExcessImages 是请求路径的入口：先按预算算"还要退役几张"，再落一条决策。
// 返回本次退役的张数（0 = 本来就够，什么都没写）。
//
// 调用方的正确用法是"失败即降级、降级即重试"：拿这条函数改写日志后**重新**
// Surface 一次，新投影里最旧的若干张已经变成占位文本，请求就重新在预算内。
func OffloadExcessImages(log *Log, surface []SurfaceMsg, budget ImageOffloadBudget) (int, error) {
	count := RequiredImageOffload(surface, budget)
	if count <= 0 {
		return 0, nil
	}
	recorded, err := RecordImageOffload(log, surface, count)
	if err != nil {
		return 0, err
	}
	if !recorded {
		return 0, nil
	}
	return count, nil
}

// ParseImageOffloadTargets 是官方 `imageOffloadProjection.project` 的**校验**部分
// （lib/types/projection.js:14-45）逐条移植，供写入端与测试用。
//
// 官方在这里抛错的每一条都保留：data 必须是对象且**恰好**一个键 `targets`；
// 每个 target 必须**恰好** `seq` + `imageIndexes` 两个键；seq 必须是非负安全整数；
// imageIndexes 非空、非负、**严格递增**。
//
// 官方另外两条（"seq 必须是当前 surface 节点"、"源事件必须是 user/message 或
// tool/result"）需要 surface 上下文，在 applyImageOffload 里判。
func ParseImageOffloadTargets(data json.RawMessage) ([]ImageOffloadTarget, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("image/offload: data 必须是 JSON 对象: %w", err)
	}
	if len(obj) != 1 {
		return nil, fmt.Errorf("image/offload: data 必须只含一个键 targets（实际 %d 个）", len(obj))
	}
	rawTargets, ok := obj["targets"]
	if !ok {
		return nil, fmt.Errorf("image/offload: data 必须只含一个键 targets")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(rawTargets, &raws); err != nil {
		return nil, fmt.Errorf("image/offload: targets 必须是数组: %w", err)
	}
	if len(raws) == 0 {
		return nil, fmt.Errorf("image/offload: targets 不能为空数组")
	}
	out := make([]ImageOffloadTarget, 0, len(raws))
	for _, raw := range raws {
		var t map[string]json.RawMessage
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("image/offload: target 必须是 JSON 对象: %w", err)
		}
		if len(t) != 2 {
			return nil, fmt.Errorf("image/offload: 每个 target 必须恰好含 seq 与 imageIndexes 两个键（实际 %d 个）", len(t))
		}
		rawSeq, ok := t["seq"]
		if !ok {
			return nil, fmt.Errorf("image/offload: target 缺 seq")
		}
		var seq int64
		if err := json.Unmarshal(rawSeq, &seq); err != nil || seq < 0 {
			return nil, fmt.Errorf("image/offload: target seq 必须是非负整数")
		}
		rawIndexes, ok := t["imageIndexes"]
		if !ok {
			return nil, fmt.Errorf("image/offload: target 缺 imageIndexes")
		}
		var indexes []int
		if err := json.Unmarshal(rawIndexes, &indexes); err != nil {
			return nil, fmt.Errorf("image/offload: imageIndexes 必须是数组: %w", err)
		}
		if len(indexes) == 0 {
			return nil, fmt.Errorf("image/offload: imageIndexes 不能为空数组")
		}
		previous := -1
		for _, index := range indexes {
			if index < 0 {
				return nil, fmt.Errorf("image/offload: imageIndexes 必须是非负安全整数")
			}
			if index <= previous {
				return nil, fmt.Errorf("image/offload: imageIndexes 必须严格递增")
			}
			previous = index
		}
		out = append(out, ImageOffloadTarget{Seq: seq, ImageIndexes: indexes})
	}
	return out, nil
}

// ImageOffloadTargets 按日志顺序回放全部 image/offload 决策。
//
// 单条事件非法时**整条丢弃**（官方在 Session 提交前就拒绝它；读路径不写盘，
// 也不能因为一条脏事件让整份历史读不出来）。合法事件按 seq 顺序拼接，
// 后写的决策看得到先写的退役标记（官方 project 的 `context.messages` 累积语义）。
func ImageOffloadTargets(log *v4.Log) []ImageOffloadTarget {
	if log == nil {
		return nil
	}
	var out []ImageOffloadTarget
	for _, ev := range log.Events {
		if ev.Type != ImageOffloadEventType {
			continue
		}
		targets, err := ParseImageOffloadTargets(ev.Data)
		if err != nil {
			continue
		}
		out = append(out, targets...)
	}
	return out
}

// imageOffloadIndex 把全部决策摊平成 seq → 已退役的深度优先图片序号。
//
// 同一条消息可能出现在多条决策里（一次请求退役几张、下一轮再退役几张），
// 这里按日志顺序合并；重复的序号是幂等的（`markOffloadedImages` 只置位）。
func imageOffloadIndex(log *v4.Log) map[int64][]int {
	targets := ImageOffloadTargets(log)
	if len(targets) == 0 {
		return nil
	}
	out := make(map[int64][]int, len(targets))
	for _, target := range targets {
		out[target.Seq] = append(out[target.Seq], target.ImageIndexes...)
	}
	return out
}

// SurfaceImage 是 surface 节点里**一个**图片出现位置的投影视图。
//
// 为什么保留整块：模型要按 id 取回原件（占位文案里就是 id），而字节预算要用
// 真实 ByteSize 算——两者都在 durable 块上，别处拿不到。
type SurfaceImage struct {
	// Index 是消息内深度优先的图片序号（从 0 起，**只数图片块**）。
	Index int
	// BlockIndex 是它在 content 数组里的位置（占位文本要插回原位）。
	BlockIndex int
	// Block 是原始 durable 块（attachment 形状）。
	Block ContentBlock
	// Offloaded 为 true 表示这个出现位置已被 image/offload 退役成占位文本。
	Offloaded bool
}

// surfaceEventBlocks 取出一条事件的内容块，按各类型的落法选优先位置
// （user/message 在顶层，其余在 message.content；规则只在 blocks.go 的 ContentBlocks 里）。
func surfaceEventBlocks(ev v4.Event) ([]ContentBlock, bool) {
	prefer := PreferNested
	if ev.Type == "user/message" {
		prefer = PreferTop
	}
	return ContentBlocks(ev.Data, prefer)
}

// surfaceImages 取出一条事件里的图片出现位置（深度优先）。
//
// 只有 `user/message` 与 `tool/result` 有：官方明确 assistant 节点是模型自己的
// 输出、永不被选（lib/types/image-offload.js:19）。纯文本消息返回 nil——
// 不加就还是以前那份投影，一个字节都不多。
func surfaceImages(ev v4.Event) []SurfaceImage {
	switch ev.Type {
	case "user/message", "tool/result":
	default:
		return nil
	}
	blocks, ok := surfaceEventBlocks(ev)
	if !ok {
		return nil
	}
	var out []SurfaceImage
	imageIndex := 0
	for i, block := range blocks {
		if !IsImageBlock(block) {
			continue
		}
		out = append(out, SurfaceImage{Index: imageIndex, BlockIndex: i, Block: block})
		imageIndex++
	}
	return out
}

// markOffloadedImages 返回标了退役位的副本（不改传入切片：surface 节点可能被
// 多个读路径共享，就地改会把"这次请求的退役"泄漏到别人的投影里）。
//
// 官方 `offloadMessageImages` 对"选中的出现位置不存在"抛错；这里读路径只标
// 存在的那些——不存在的 target 在写入端已经由 ParseImageOffloadTargets 与
// 当前 surface 的一致性挡掉了。
func markOffloadedImages(images []SurfaceImage, indexes []int) []SurfaceImage {
	if len(images) == 0 || len(indexes) == 0 {
		return images
	}
	selected := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		selected[index] = true
	}
	out := make([]SurfaceImage, len(images))
	copy(out, images)
	for i := range out {
		if selected[out[i].Index] {
			out[i].Offloaded = true
		}
	}
	return out
}

// joinTextWithPlaceholders 是 `joinText` 的"带占位"版本：文本块照旧按 "\n"
// 拼接，被退役的图片出现位置**原地**换成官方占位文本。
//
// 为什么不是"把占位追加到末尾"：那会让模型以为占位是最后发生的事，
// 与 compaction 替身必须插回被遮蔽起点是同一个道理（surface.go:120-122）。
func joinTextWithPlaceholders(blocks []ContentBlock, images []SurfaceImage) string {
	byBlock := make(map[int]SurfaceImage, len(images))
	for _, img := range images {
		byBlock[img.BlockIndex] = img
	}
	out := ""
	add := func(text string) {
		if out != "" {
			out += "\n"
		}
		out += text
	}
	for i, block := range blocks {
		if img, ok := byBlock[i]; ok {
			if img.Offloaded {
				add(OffloadedImageText(img.Block, nil))
			}
			continue
		}
		if block.Type != "text" {
			continue
		}
		add(block.Text)
	}
	return out
}

// applyImageOffload 把一条决策投影到一个已派生的 surface 节点上。
//
// 官方两处「需要 surface 上下文」的校验在这里：target 必须指向**当前** surface
// 节点，且源事件必须是 `user/message` 或 `tool/result`（projection.js:29-33）。
func applyImageOffload(node SurfaceMsg, ev v4.Event, indexes []int) SurfaceMsg {
	if len(indexes) == 0 || len(node.Images) == 0 {
		return node
	}
	switch ev.Type {
	case "user/message", "tool/result":
	default:
		return node
	}
	marked := markOffloadedImages(node.Images, indexes)
	if !anyOffloaded(marked) {
		return node
	}
	blocks, ok := surfaceEventBlocks(ev)
	if !ok {
		return node
	}
	node.Images = marked
	node.Message.Content = joinTextWithPlaceholders(blocks, marked)
	return node
}

func anyOffloaded(images []SurfaceImage) bool {
	for _, img := range images {
		if img.Offloaded {
			return true
		}
	}
	return false
}
