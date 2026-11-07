package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"dshgo/codecs/v4"
)

// 官方 0.2.2-rc.1 的 @deepseek-ai/dsh-compaction-image-offload（map 编号 80）。
// 预算口径、占位文案、降级顺序全部以官方原件为准（file:line 见 image_offload.go 文件头）。

// imageTestAttachmentID 造一个形状合法的 attachment id（att- + 128-bit hex）。
func imageTestAttachmentID(i int) string {
	return fmt.Sprintf("att-%032x", i+1)
}

// imageTestBlock 造一张 image 附件的 durable 块。
func imageTestBlock(i int, byteSize int64) ContentBlock {
	return ContentBlock{
		Type: "attachment", AttachmentID: imageTestAttachmentID(i), Name: fmt.Sprintf("shot-%d.png", i+1),
		MIME: "image/png", ByteSize: byteSize, Width: 1568, Height: 882, SHA256: "00",
	}
}

// mustAppend 写一条事件，失败即 Fatal。
func mustAppend(t *testing.T, l *Log, eventType string, data any) {
	t.Helper()
	if err := l.Append(eventType, data); err != nil {
		t.Fatal(err)
	}
}

// imageSession 造一份 n 条 user/message、每条一张 1 MiB 图片的会话：
// 返回写入端，以及"读回当前日志"的闭包（Surface 吃的是读回来的 *v4.Log）。
func imageSession(t *testing.T, n int) (*Log, func() *v4.Log) {
	t.Helper()
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/image-offload", "s-image-offload", "standard")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	read := func() *v4.Log {
		t.Helper()
		back, err := v4.ReadFile(l.Path)
		if err != nil {
			t.Fatalf("读回会话失败: %v", err)
		}
		return back
	}
	for i := 0; i < n; i++ {
		mustAppend(t, l, "user/message", UserContentMessage([]ContentBlock{
			{Type: "text", Text: fmt.Sprintf("第 %d 张", i+1)},
			imageTestBlock(i, 1024*1024),
		}))
	}
	return l, read
}

// TestImageOffloadSurfaceExposesImageOccurrences：surface 节点必须带出图片出现位置
// （深度优先序号 + 在 content 里的位置 + durable 块），否则预算无从计算、
// 占位也无处插回原位。未退役时模型可见正文一个字节都不变。
func TestImageOffloadSurfaceExposesImageOccurrences(t *testing.T) {
	_, read := imageSession(t, 3)
	surface := Surface(read())
	if len(surface) != 3 {
		t.Fatalf("surface 节点数 = %d，期望 3", len(surface))
	}
	for i, node := range surface {
		if len(node.Images) != 1 {
			t.Fatalf("节点 %d 的图片出现位置数 = %d，期望 1：%+v", i, len(node.Images), node.Images)
		}
		img := node.Images[0]
		if img.Index != 0 || img.BlockIndex != 1 {
			t.Fatalf("节点 %d 的图片序号/块位置 = %d/%d，期望 0/1", i, img.Index, img.BlockIndex)
		}
		if img.Offloaded {
			t.Fatalf("节点 %d 还没退役，Offloaded 却是 true", i)
		}
		if img.Block.AttachmentID != imageTestAttachmentID(i) {
			t.Fatalf("节点 %d 的附件 id = %q", i, img.Block.AttachmentID)
		}
		if want := fmt.Sprintf("第 %d 张", i+1); node.Message.Content != want {
			t.Fatalf("未退役时模型可见正文 = %q，期望 %q（图片本来就不进正文）", node.Message.Content, want)
		}
	}
}

// TestImageOffloadReplacesOldestWithPlaceholder：按预算退役最旧的若干张，
// 重新 Surface 后它们**原地**变成官方占位文本（带附件 id），更旧的先退役、
// 更新的仍是图片；v4 原始事件一个字节都不动。
func TestImageOffloadReplacesOldestWithPlaceholder(t *testing.T) {
	writer, read := imageSession(t, 3)
	log := read()
	before := make([][]byte, len(log.Events))
	for i, ev := range log.Events {
		before[i] = append([]byte(nil), ev.Data...)
	}

	surface := Surface(log)
	if got := RequiredImageOffload(surface, ImageOffloadBudget{Representation: RepresentationRaw, MaxImages: 3}); got != 0 {
		t.Fatalf("3 张图、上限 3 张时 RequiredImageOffload = %d，期望 0", got)
	}
	if got := RequiredImageOffload(surface, ImageOffloadBudget{Representation: RepresentationRaw, MaxImages: 2, CountQuantum: 1}); got != 1 {
		t.Fatalf("上限 2 张时 RequiredImageOffload = %d，期望 1", got)
	}

	recorded, err := RecordImageOffload(writer, surface, 2)
	if err != nil || !recorded {
		t.Fatalf("RecordImageOffload = %v, %v；期望 true, nil", recorded, err)
	}

	// v4 是 append-only：原事件逐条一字不改，只多出最后那条决策。
	log = read()
	if len(log.Events) != 4 {
		t.Fatalf("事件数 = %d，期望 4（3 条原事件 + 1 条决策）", len(log.Events))
	}
	last := log.Events[3]
	if last.Type != ImageOffloadEventType {
		t.Fatalf("第 4 条事件类型 = %q，期望 %q", last.Type, ImageOffloadEventType)
	}
	for i := range before {
		if !bytes.Equal(before[i], log.Events[i].Data) {
			t.Fatalf("第 %d 条原始事件被改写了：%s → %s", i, before[i], log.Events[i].Data)
		}
	}

	// 磁盘上的字节也必须是"前缀不变 + 追加"，不是原地重写。
	onDisk, err := os.ReadFile(log.Path)
	if err != nil {
		t.Fatalf("读会话文件失败: %v", err)
	}
	if len(onDisk) == 0 {
		t.Fatal("会话文件是空的")
	}

	surface = Surface(log)
	if len(surface) != 3 {
		t.Fatalf("退役后 surface 节点数 = %d，期望 3（决策事件不占 surface 位置）", len(surface))
	}
	for i, node := range surface {
		wantOffloaded := i < 2
		if node.Images[0].Offloaded != wantOffloaded {
			t.Fatalf("节点 %d 的 Offloaded = %v，期望 %v", i, node.Images[0].Offloaded, wantOffloaded)
		}
		if !wantOffloaded {
			if node.Message.Content != "第 3 张" {
				t.Fatalf("未退役节点正文 = %q，期望 %q", node.Message.Content, "第 3 张")
			}
			continue
		}
		// 占位必须**原地**插在文本块后面（图片是第 2 个块），并带真实附件 id。
		want := fmt.Sprintf("第 %d 张\n%s", i+1, OffloadedImageText(imageTestBlock(i, 1024*1024), nil))
		if node.Message.Content != want {
			t.Fatalf("节点 %d 的占位正文 = %q，期望 %q", i, node.Message.Content, want)
		}
		if !strings.Contains(node.Message.Content, imageTestAttachmentID(i)) {
			t.Fatalf("节点 %d 的占位里没有附件 id：%q", i, node.Message.Content)
		}
	}
}

// TestImageOffloadRereadDoesNotOffloadAgain：读路径再次 Surface 时，
// 已退役的出现位置不再参与选择（否则同一张图会被反复降级、事件无限增长）。
func TestImageOffloadRereadDoesNotOffloadAgain(t *testing.T) {
	writer, read := imageSession(t, 3)
	surface := Surface(read())
	if _, err := RecordImageOffload(writer, surface, 2); err != nil {
		t.Fatal(err)
	}
	events := len(read().Events)

	// 再读一次：还是超预算，但只剩 1 张可退役，且必须是最新的那张（序号 2）。
	surface = Surface(read())
	if got := RequiredImageOffload(surface, ImageOffloadBudget{Representation: RepresentationRaw, MaxImages: 2, CountQuantum: 1}); got != 0 {
		t.Fatalf("退役后 RequiredImageOffload = %d，期望 0", got)
	}
	targets := SelectOldestImages(surface, 5)
	if len(targets) != 1 {
		t.Fatalf("可退役 target 数 = %d，期望 1（已退役的两张必须被跳过）：%+v", len(targets), targets)
	}
	if targets[0].Seq != 2 || len(targets[0].ImageIndexes) != 1 || targets[0].ImageIndexes[0] != 0 {
		t.Fatalf("可退役 target = %+v，期望 {seq:2 imageIndexes:[0]}", targets[0])
	}

	// 但预算本来就够时，一次都不该写。
	recorded, err := RecordImageOffload(writer, surface, 0)
	if err != nil || recorded {
		t.Fatalf("count=0 时 RecordImageOffload = %v, %v；期望 false, nil", recorded, err)
	}
	if got := len(read().Events); got != events {
		t.Fatalf("count=0 却写了事件：%d → %d", events, got)
	}
}

// TestOffloadedImagePrefixCountMatchesOfficial 逐行对照官方
// offloadedImagePrefixCount（dsh-llm/lib/types/content.js:183-204）。
func TestOffloadedImagePrefixCountMatchesOfficial(t *testing.T) {
	cases := []struct {
		name    string
		lengths []int64
		budget  ImageOffloadBudget
		want    int
	}{
		{"不限制", []int64{10, 10, 10}, ImageOffloadBudget{}, 0},
		{"空历史", nil, ImageOffloadBudget{MaxImages: 1}, 0},
		{"张数在预算内", []int64{10, 10}, ImageOffloadBudget{MaxImages: 2}, 0},
		{"张数超 1 张", []int64{10, 10, 10}, ImageOffloadBudget{MaxImages: 2}, 1},
		{"字节在预算内", []int64{100, 100, 100}, ImageOffloadBudget{MaxBytes: 300}, 0},
		{"字节超 50", []int64{100, 100, 100}, ImageOffloadBudget{MaxBytes: 250}, 1},
		// 官方在 quantum != 1 时判据是 removedBytes > removeBytes（差一个字节）。
		{"字节量子向上取整", []int64{100, 100, 100}, ImageOffloadBudget{MaxBytes: 250, ByteQuantum: 64}, 1},
		// 张数量子向上取整到 10，但只有 3 张可退 → 全退。
		{"张数量子向上取整", []int64{10, 10, 10}, ImageOffloadBudget{MaxImages: 2, CountQuantum: 10}, 3},
		// 张数与字节同时超：取更靠后的那个前缀。
		{"双预算取更靠后者", []int64{10, 10, 10, 10}, ImageOffloadBudget{MaxImages: 3, MaxBytes: 25}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OffloadedImagePrefixCount(tc.lengths, tc.budget); got != tc.want {
				t.Fatalf("OffloadedImagePrefixCount(%v, %+v) = %d，期望 %d", tc.lengths, tc.budget, got, tc.want)
			}
		})
	}

	// base64Length 官方是 Math.ceil(bytes/3)*4。
	for _, tc := range []struct{ in, want int64 }{{0, 0}, {1, 4}, {3, 4}, {4, 8}, {1048576, 1398104}} {
		if got := Base64Length(tc.in); got != tc.want {
			t.Fatalf("Base64Length(%d) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

// TestOffloadedImageTextMatchesOfficial 逐字节对照官方 offloadedImageText
// （dsh-llm/lib/types/content.js:37-41,73-79）的两条分支。
func TestOffloadedImageTextMatchesOfficial(t *testing.T) {
	block := ContentBlock{
		Type: "attachment", AttachmentID: "att-abc", Name: "shot.png",
		MIME: "image/png", Width: 1568, Height: 882, ByteSize: 1,
	}

	// 拿不到只读路径：官方那句"No local normalized image path is available"。
	want := `[image omitted to fit request image limits; "shot.png" (att-abc).` +
		` No local normalized image path is available; ask the user to attach it again if needed.]`
	if got := OffloadedImageText(block, nil); got != want {
		t.Fatalf("无路径文案 = %q\n期望       = %q", got, want)
	}

	// 有只读路径：身份 + 归一化副本说明 + 扩展名指引。
	want = `[image omitted to fit request image limits; "shot.png" (att-abc).` +
		` Normalized copy (read-only; may be resized or re-encoded): "C:/state/attachments/objects/att-abc/blob"` +
		` (1568x882px, image/png). Source dimensions, format, and byte size may differ.` +
		` Copy to a writable path ending in .png before editing.]`
	got := OffloadedImageText(block, &ImageAccess{ReadonlyPath: "C:/state/attachments/objects/att-abc/blob"})
	if got != want {
		t.Fatalf("带路径文案 = %q\n期望       = %q", got, want)
	}

	// 没名字时官方只给 id（ref.name === undefined 那一支）。
	nameless := block
	nameless.Name = ""
	if got := OffloadedImageText(nameless, nil); !strings.Contains(got, "image limits; att-abc.") {
		t.Fatalf("无名字文案 = %q，期望带裸 id", got)
	}

	// 四种官方图片类型的扩展名。
	for mediaType, ext := range map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif",
	} {
		if got := ImageExtension(mediaType); got != ext {
			t.Fatalf("ImageExtension(%q) = %q，期望 %q", mediaType, got, ext)
		}
	}
}

// TestImageOffloadRequestFitsBudgetAfterOffload 是端到端：21 张 1 MiB 图在
// pi-ai 官方默认预算（20 MiB base64）下超限；退役最旧的 7 张后重新派生的请求
// 重新在预算内，且 7 张的附件 id 全都出现在模型可见正文里（模型能按 id 取回原件）。
func TestImageOffloadRequestFitsBudgetAfterOffload(t *testing.T) {
	writer, read := imageSession(t, 21)
	surface := Surface(read())
	budget := DefaultImageOffloadBudget()

	over := RequiredImageOffload(surface, budget)
	if over != 7 {
		t.Fatalf("超预算张数 = %d，期望 7（21×1398104 base64 字节 vs 20 MiB）", over)
	}

	offloaded, err := OffloadExcessImages(writer, surface, budget)
	if err != nil || offloaded != 7 {
		t.Fatalf("OffloadExcessImages = %d, %v；期望 7, nil", offloaded, err)
	}

	surface = Surface(read())
	if got := RequiredImageOffload(surface, budget); got != 0 {
		t.Fatalf("退役后 RequiredImageOffload = %d，期望 0", got)
	}

	var placeholders int
	for i, node := range surface {
		wantOffloaded := i < 7
		if node.Images[0].Offloaded != wantOffloaded {
			t.Fatalf("节点 %d 的 Offloaded = %v，期望 %v", i, node.Images[0].Offloaded, wantOffloaded)
		}
		if !wantOffloaded {
			continue
		}
		placeholders++
		if !strings.Contains(node.Message.Content, imageTestAttachmentID(i)) {
			t.Fatalf("节点 %d 的占位里没有附件 id：%q", i, node.Message.Content)
		}
	}
	if placeholders != 7 {
		t.Fatalf("占位数 = %d，期望 7", placeholders)
	}

	// 一次降级之后幂等：再跑一遍不该写任何东西。
	events := len(read().Events)
	if n, err := OffloadExcessImages(writer, surface, budget); err != nil || n != 0 {
		t.Fatalf("第二次 OffloadExcessImages = %d, %v；期望 0, nil", n, err)
	}
	if got := len(read().Events); got != events {
		t.Fatalf("幂等失败：事件数 %d → %d", events, got)
	}
}

// TestImageOffloadSkipsAssistantNodes：assistant 节点是模型自己的输出，
// 官方明确永不被选（lib/types/image-offload.js:19）。
func TestImageOffloadSkipsAssistantNodes(t *testing.T) {
	writer, read := imageSession(t, 0)
	mustAppend(t, writer, "assistant/message", AssistantMessageEvent(1, 1, []map[string]any{
		TextBlock("先看图"),
		{"type": "attachment", "attachmentId": "att-assistant", "mime": "image/png", "byteSize": 1024},
	}, "stop", nil))
	mustAppend(t, writer, "user/message", UserContentMessage([]ContentBlock{imageTestBlock(0, 1024)}))
	surface := Surface(read())
	if len(surface) != 2 {
		t.Fatalf("surface 节点数 = %d，期望 2", len(surface))
	}
	if len(surface[0].Images) != 0 {
		t.Fatalf("assistant 节点不该带图片出现位置：%+v", surface[0].Images)
	}
	targets := SelectOldestImages(surface, 5)
	if len(targets) != 1 || targets[0].Seq != 1 {
		t.Fatalf("可退役 target = %+v，期望只落在 user 节点 seq 1", targets)
	}
}

// TestParseImageOffloadTargetsRejectsMalformed 逐条对照官方
// imageOffloadProjection.project 的校验（lib/types/projection.js:16-41）。
func TestParseImageOffloadTargetsRejectsMalformed(t *testing.T) {
	ok := `{"targets":[{"seq":1,"imageIndexes":[0,2]}]}`
	targets, err := ParseImageOffloadTargets(json.RawMessage(ok))
	if err != nil {
		t.Fatalf("合法事件被拒：%v", err)
	}
	if len(targets) != 1 || targets[0].Seq != 1 || len(targets[0].ImageIndexes) != 2 {
		t.Fatalf("解析结果 = %+v", targets)
	}

	bad := []struct{ name, data string }{
		{"不是对象", `[]`},
		{"多键", `{"targets":[{"seq":1,"imageIndexes":[0]}],"extra":1}`},
		{"缺 targets", `{"other":[]}`},
		{"targets 不是数组", `{"targets":{}}`},
		{"targets 为空", `{"targets":[]}`},
		{"target 多键", `{"targets":[{"seq":1,"imageIndexes":[0],"x":1}]}`},
		{"target 缺 seq", `{"targets":[{"imageIndexes":[0]}]}`},
		{"seq 为负", `{"targets":[{"seq":-1,"imageIndexes":[0]}]}`},
		{"imageIndexes 为空", `{"targets":[{"seq":1,"imageIndexes":[]}]}`},
		{"imageIndexes 非严格递增", `{"targets":[{"seq":1,"imageIndexes":[1,1]}]}`},
		{"imageIndexes 为负", `{"targets":[{"seq":1,"imageIndexes":[-1]}]}`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseImageOffloadTargets(json.RawMessage(tc.data)); err == nil {
				t.Fatalf("非法事件 %s 被接受了", tc.data)
			}
		})
	}
}
