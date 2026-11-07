package session

import "encoding/json"

// ContentBlock 是 v4 内容块的最小形状；写入（ReasoningBlock / TextBlock /
// ToolCallBlock）与读取（ContentBlocks）共用这一个结构体。
//
// 为什么不各写一份：`internal/session` 与 `cmd/dshd` 以前各有一个字段一模一样的
// `block`，两边各自跟着 v4 漂——一边加了字段、另一边读回来的块就少一个，
// 而两端都不报错。`cmd/dshd` 侧现在是 `type block = session.ContentBlock` 的别名，
// 不再是第二份定义。
type ContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// 以下字段只用于用户 attachment/reference 块；原有 text/reasoning/tool-call
	// 的键与值保持不变。块在 content 数组中的位置就是持久顺序。
	AttachmentID  string `json:"attachmentId,omitempty"`
	ByteSize      int64  `json:"byteSize,omitempty"`
	MIME          string `json:"mime,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	ReferenceID   string `json:"referenceId,omitempty"`
	ReferenceType string `json:"referenceType,omitempty"`
	Path          string `json:"path,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
}

// Prefer 说明两个 content 位置里先看哪一个。
//
// 为什么次序要显式写出来、而不写死一种：v4 把助手内容落在 `message.content`，
// 但 user/message 的正文在**顶层** `content`——两个位置都有内容时，"先看哪个"
// 就是两条读路径的真实差别。统一成一种次序会在这种事件上改掉输出，而这类事件
// 恰恰最可能是脏数据，最不该悄悄换一种读法。
type Prefer int

const (
	// PreferNested：`message.content` 优先，`len==0` 时用顶层 `content`。
	// 这是 v4 的标准落法（assistant/message），三处读路径都用它。
	PreferNested Prefer = iota
	// PreferTop：顶层 `content` 优先，`len==0` 时用 `message.content`。
	// 这是 user/message 的落法（见 cmd/dshd/transport_source.go 的 textOf）。
	PreferTop
)

// ContentBlocks 取出一条消息的内容块——「内容块从哪取」这条规则**只有这一份**。
//
// 规则：`prefer` 指的位置优先，`len==0` 时用另一个位置兜底；JSON 解不开就当没有
// （ok=false），不返回错误——调用方（历史派生 / 界面投影 / 检索 / fork）要的是
// 一份投影，一条坏事件不该把整批读路径打断。
//
// 为什么两个位置都得认：真实语料把助手内容放在 `message.content`，但早期/别处
// 产生的 assistant/message 直接挂在顶层。只认一种的后果很隐蔽——那些会话在历史
// 与界面上"一条消息都没有"，看起来像会话坏了，不像读错了键。
//
// 为什么它是单点：这条规则以前在四处各写一遍（`ReasoningBlocks`、
// `assistantMessage`、`cmd/dshd/eventText`、`cmd/dshd/contentBlocks`），
// 各解一遍同一个匿名结构体、各写一遍同一个 len==0 兜底。改了一处、漏了另一处
// 这种事没有任何测试会红——两个读路径各自都有用例，只是用例形状不同。
// 现在 `scripts/single-source.py` 的 `content-block-locus` 形状看着它：
// 同一个函数里再摆一份"两个 `Content []… json:"content"`"就是第二份实现。
func ContentBlocks(data json.RawMessage, prefer Prefer) ([]ContentBlock, bool) {
	var d struct {
		Message struct {
			Content []ContentBlock `json:"content"`
		} `json:"message"`
		Content []ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, false
	}
	first, second := d.Message.Content, d.Content
	if prefer == PreferTop {
		first, second = second, first
	}
	if len(first) == 0 {
		first = second
	}
	return first, true
}
