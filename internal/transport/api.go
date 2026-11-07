// Package transport 是宿主线与界面线唯一的握手点。
//
// 本文件只放契约：方法名与 JSON 形状。不放任何网络代码。
// 界面只认这里的字符串，改了就是破坏契约——先改快照，再两边各改各的。
package transport

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Method 是 /api 上的一个方法名。界面只认这些字符串，不许改。
type Method string

const (
	MethodSessionList    Method = "session.list"
	MethodSessionLoad    Method = "session.load"
	MethodSessionHistory Method = "session.history"
	MethodSessionPrompt  Method = "session.prompt"
	MethodSessionCancel  Method = "session.cancel"
	// MethodSessionApprove 回一次审批（H13）。请求走 WS，应答走 HTTP，与 prompt/cancel 一致。
	MethodSessionApprove Method = "session.approve"
	MethodSessionCreate  Method = "session.create"
	MethodSessionDelete  Method = "session.delete"
	MethodSessionFork    Method = "session.fork"
	// 三个 session operation 是独立 additive 扩展；不得加入 canonical 25。
	MethodSessionRewindPreview  Method = "session.rewind.preview"
	MethodSessionRewindApply    Method = "session.rewind.apply"
	MethodSessionTranscriptPage Method = "session.transcript.page"
	// metadata/init 是独立 additive 扩展；不得加入 canonical 25。
	MethodSessionRename   Method = "session.rename"
	MethodSessionColorGet Method = "session.color.get"
	MethodSessionColorSet Method = "session.color.set"
	MethodWorkspaceInit   Method = "workspace.init"
	// MethodSessionCompact 让客户端能主动压一次（界面上的"压缩"，官方 /compact）。
	MethodSessionCompact Method = "session.compact"
	MethodWorkspaceList  Method = "workspace.list"
	// MethodJobsList / MethodJobsOutput 是**后台任务**（bash 的
	// `run_in_background: true` 起出来的那批）。底层 `spi.Job` 早就实现了
	// List/Output/Kill，只是 Transport 上一直没有这两个方法，界面就看不到它们。
	MethodJobsList   Method = "jobs.list"
	MethodJobsOutput Method = "jobs.output"
	// MethodJobsStop 是 additive 任务动作；不得加入 canonical 25 或复制 jobs.output。
	MethodJobsStop Method = "jobs.stop"
	// MethodScheduleList 列定时任务。存储层（internal/schedule）早就有了，
	// `dshd schedule list` 一直在用；缺的只是 Transport 上这个方法，
	// 所以界面一直看不到"这个工作区有哪些定时任务"。
	MethodScheduleList Method = "schedule.list"
	// 日程 mutation 是独立 additive 扩展；list 的方法名、顺序与读语义保持不变。
	MethodScheduleDelete Method = "schedule.delete"
	MethodScheduleClear  Method = "schedule.clear"
	// MethodSessionSetModel 给会话设一个"下一轮用哪个模型"的覆盖。
	//
	// 为什么需要：`session.load` 里的 model 是**上一轮实际用的**（只读投影），
	// 而界面上的模型选择器要能改。没有这个方法，那个下拉就只能画死。
	// 覆盖存在 `<home>/state/session-model.json`（**不碰 v4**：R5 说读路径不写盘）。
	MethodSessionSetModel Method = "session.setModel"
	// MethodSessionSetPermission 给会话设一个"下一轮用什么审批策略"的覆盖。
	//
	// 和 setModel 同构：`load` 里的 permission 是只读投影，界面要能改就得有这个方法。
	// 覆盖存 `<home>/state/session-permission.json`（**不碰 v4**）。
	MethodSessionSetPermission Method = "session.setPermission"
	// MethodModelsList 问网关有哪些模型（OpenAI 兼容的 GET /v1/models）。
	//
	// 为什么必须由宿主转发：网关地址与凭据只在宿主这一侧（R7 界面不直连模型出口）。
	// 界面要模型下拉就只能走这里——或者在界面里硬编码模型名，那会过期、且会骗人。
	MethodModelsList Method = "models.list"
	// MethodTerminalOpen/Input/Output 是一条**长连接 shell** 的三个动作。
	//
	// 和 `jobs.*` 的区别：job 是"跑一条命令、有结束"；terminal 是"一直开着、
	// 用户随时敲"。底层 `modules/exec/bash` 的 `session` 本来就是持久 PTY，
	// 只是一直没有"开一个留住它"的入口。
	// MethodSessionSetGoal 设/清这条会话的 goal（一句话：现在在做什么）。
	//
	// 和 setModel/setPermission 同构，只是值是一段自由文本。
	// 界面上的 GoalBar 就停靠在 composer 上方——它在回答"我在干嘛"，
	// 而这个问题每时每刻都在被问。
	MethodSessionSetGoal Method = "session.setGoal"
	// effort/goal 扩展独立于 canonical 25；只新增方法，不重排既有方法。
	MethodSessionSetEffort  Method = "session.setEffort"
	MethodSessionGoalGet    Method = "session.goal.get"
	MethodSessionGoalPause  Method = "session.goal.pause"
	MethodSessionGoalResume Method = "session.goal.resume"
	MethodTerminalOpen      Method = "terminal.open"
	MethodTerminalInput     Method = "terminal.input"
	MethodTerminalOutput    Method = "terminal.output"
	MethodFSRead            Method = "fs.read"
	MethodFSWrite           Method = "fs.write"
	MethodConfigGet         Method = "config.get"
	MethodConfigSet         Method = "config.set"
	// 官方 ConfigForm 的 describe/mutate 与宿主显式表单入口都是 additive
	// 扩展，绝不加入 SessionMethods 的 canonical 25。
	MethodSettingsDescribe     Method = "settings.describe"
	MethodSettingsMutate       Method = "settings.mutate"
	MethodSettingsFormDescribe Method = "settings.form.describe"
	MethodSettingsFormGet      Method = "settings.form.get"
	MethodSettingsFormSet      Method = "settings.form.set"
	MethodSettingsFormReset    Method = "settings.form.reset"
	// 持久模型目录管理是独立 additive 扩展。既有 MethodModelsList 仍是
	// canonical 网关投影，不能改名或 repurposed 成目录存储。
	MethodModelCatalogList      Method = "models.catalog.list"
	MethodModelCatalogUpsert    Method = "models.catalog.upsert"
	MethodModelCatalogReorder   Method = "models.catalog.reorder"
	MethodModelCatalogTest      Method = "models.catalog.test"
	MethodModelCatalogSetActive Method = "models.catalog.set-active"
	// 本地附件是独立 additive 扩展；用户显式给出的 path 就是本次读取授权。
	// 它们不进入 canonical 25，也不改变旧 session.prompt{text} 的形状。
	MethodAttachmentPut Method = "attachment.put"
	MethodAttachmentGet Method = "attachment.get"
)

// AttachmentMethods 是本地附件的独立扩展面，绝不加入 canonical 25。
func AttachmentMethods() []Method {
	return []Method{MethodAttachmentPut, MethodAttachmentGet}
}

// TaskActionMethods 是独立任务 mutation 扩展面。jobs.output 已属于 canonical 25，
// 这里只列新增方法，避免任何快照/门禁把它重复计数。
func TaskActionMethods() []Method {
	return []Method{MethodScheduleDelete, MethodScheduleClear, MethodJobsStop}
}

// ModelCatalogMethods 是独立模型目录扩展面，绝不加入 canonical 25。
func ModelCatalogMethods() []Method {
	return []Method{
		MethodModelCatalogList, MethodModelCatalogUpsert, MethodModelCatalogReorder,
		MethodModelCatalogTest, MethodModelCatalogSetActive,
	}
}

// SettingsMethods 是独立设置中心扩展面。它故意与 SessionMethods 分开：
// canonical 25 的方法集、顺序和快照都不能因新增设置能力而变化。
func SettingsMethods() []Method {
	return []Method{
		MethodSettingsDescribe, MethodSettingsMutate,
		MethodSettingsFormDescribe, MethodSettingsFormGet,
		MethodSettingsFormSet, MethodSettingsFormReset,
	}
}

// EffortGoalMethods 是 W144 的独立 additive 扩展面，绝不加入 canonical 25。
func EffortGoalMethods() []Method {
	return []Method{
		MethodSessionSetEffort,
		MethodSessionGoalGet, MethodSessionGoalPause, MethodSessionGoalResume,
	}
}

// SessionMethods 是 U0 第一版要兑现的全部方法（顺序固定，快照按它排）。
func SessionMethods() []Method {
	return []Method{
		MethodSessionList, MethodSessionLoad, MethodSessionHistory,
		MethodSessionPrompt, MethodSessionCancel, MethodSessionApprove, MethodSessionCreate,
		MethodSessionDelete, MethodSessionFork, MethodSessionCompact, MethodWorkspaceList,
		MethodJobsList, MethodJobsOutput, MethodScheduleList,
		MethodSessionSetModel, MethodSessionSetPermission, MethodModelsList,
		MethodSessionSetGoal,
		MethodTerminalOpen, MethodTerminalInput, MethodTerminalOutput,
		MethodFSRead, MethodFSWrite, MethodConfigGet, MethodConfigSet,
	}
}

// SessionOperationMethods 是 W145 独立扩展面。session.fork 本来就在 canonical
// 25 中，因此这里只列新增的三个 method，避免快照重复计数。
func SessionOperationMethods() []Method {
	return []Method{MethodSessionRewindPreview, MethodSessionRewindApply, MethodSessionTranscriptPage}
}

// MetadataInitMethods 是 session metadata 与 workspace.init 的独立扩展面。
// 它只注册新能力，不改变 canonical 25 的数量、顺序或快照。
func MetadataInitMethods() []Method {
	return []Method{MethodSessionRename, MethodSessionColorGet, MethodSessionColorSet, MethodWorkspaceInit}
}

// WorkspaceInitFile 是宿主实际处理过的一份模板文件。failed 必须带真实错误；
// created/unchanged 不允许伪造 error，调用方据此保留部分成功的完整事实。
type WorkspaceInitFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// WorkspaceInitResult 至少包含一项真实处理结果；空 files 不是成功的 no-op。
type WorkspaceInitResult struct {
	Files []WorkspaceInitFile `json:"files"`
}

var (
	ErrInvalidAnchor = errors.New("invalid user anchor")
	ErrInvalidCursor = errors.New("invalid durable transcript cursor")
)

// ForkResult 只在请求实际带 anchor/title 时回显对应指针；nil 必须省略，
// 因而旧的无参数 fork 仍只有 sessionId，显式空 title 也不会被吞掉。
type ForkResult struct {
	SessionID string  `json:"sessionId"`
	Anchor    *string `json:"anchor,omitempty"`
	Title     *string `json:"title,omitempty"`
}

// RewindPreview 是一次真实 checkpoint dry-run 或 restore 的文件统计。
type RewindPreview struct {
	FilesChanged []string `json:"filesChanged"`
	Insertions   int      `json:"insertions"`
	Deletions    int      `json:"deletions"`
}

// RewindResult 的 partial outcome 只允许 both 在文件恢复成功后、对话 fork
// 失败时产生；此时 SessionID 必须为空，不能冒充新会话已创建。
type RewindResult struct {
	Kind              string         `json:"kind"`
	SessionID         string         `json:"sessionId,omitempty"`
	Reason            string         `json:"reason,omitempty"`
	Files             *RewindPreview `json:"files,omitempty"`
	ConversationError string         `json:"conversationError,omitempty"`
}

// TranscriptPageResult 页内 oldest-first，页面整体从最新向最旧前进。
// NextCursor 可在 hasOlder=false 时仍存在，用于显式读取稳定的耗尽空页。
type TranscriptPageResult struct {
	Messages   []Message `json:"messages"`
	HasOlder   bool      `json:"hasOlder"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// CompactResult 是 session.compact 的返回值。
//
// Compacted=false 表示"现在不需要压"，**不是错误**：会话还没长到该压的时候，
// 这是正常结果；把它当错误会让界面弹一个红框告诉用户"操作失败"。
type CompactResult struct {
	Compacted bool  `json:"compacted"`
	From      int64 `json:"from,omitempty"`
	To        int64 `json:"to,omitempty"`
	Shadowed  int   `json:"shadowed,omitempty"`
	Summary   int   `json:"summaryChars,omitempty"`
}

// Message 是 session.load 里的一条消息。
// Role 必须是 user / assistant / tool 之一；tool 专属字段（Tool/Args/Summary
// 与 CallID/Turn/Step，以及 W190 的 IsError/Meta/Structured）只在 Role=="tool"
// 时出现。
type Message struct {
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	Text    string `json:"text"`
	Tool    string `json:"tool,omitempty"`
	Args    string `json:"args,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Time 是这条消息对应事件的墙钟时间（epoch 毫秒）。
	// 界面要算 tok/s：输出 token ÷ 相邻两条助手消息的时间差。
	// 没有它就算不出速率——这是 H2，2026-11-03 界面线提的。
	Time int64 `json:"time,omitempty"`
	// Usage 是助手消息带的用量（H1）。键与 v4 的 usage 一致，界面直接读。
	// nil 表示这次没有用量信息——**不许编 0**，那样界面会显示"0 tok/s"。
	Usage *UsageMsg `json:"usage,omitempty"`
	// View 是工具结果的渲染声明（H3），原样来自 spi.ViewSpec。
	// 缺省不出现——界面见到 `kind` 才选渲染器，没有就退回「工具名 + 摘要」。
	View json.RawMessage `json:"view,omitempty"`
	// Reasoning 是这条助手消息的思考正文（可选，2026-11-04 加，**只加不改**）。
	//
	// 为什么必须有：v4 的 assistant/message 里 `type=reasoning` 的块不只是一句
	// "它想过"——模型想了、却没有输出正文时，它就是这次回答的**全部内容**。
	// 旧投影只取 text 块，读回来是一条空消息；而 serve 推的 assistant/message 帧
	// 本来就带 reasoning（serve_turn.go 只在正文为空时才带）。断线重连/重开
	// 走 session.load 时没有它，等于把这条回答丢了。
	//
	// 空串表示"这条没有思考块"，或"有正文，不必再带思考"——与推帧同形：
	// 有正文时思考是草稿，不是已经给用户的回答。老客户端忽略未知键即可。
	Reasoning string `json:"reasoning,omitempty"`
	// V4Seq 是持久 v4 事件信封的 Event.Seq（MSGID-IDENTITY，只加不改）。
	// 指针非 nil 的 0 是合法身份；nil 只用于非 v4/未知来源。它不是 Transport journal seq。
	V4Seq *int64 `json:"v4Seq,omitempty"`
	// MessageID 只透传 v4 data 已有的持久消息 id；当前 assistant 没有时省略，不生成。
	MessageID string `json:"messageId,omitempty"`
	// Content 是有序内容块（ATTACHMENTS-REFERENCES，只加不改）。Text 仍是旧客户端
	// 使用的纯文本投影；附件/引用只在这里出现，绝不把文件名拼进 Text 冒充。
	Content []ContentBlock `json:"content,omitempty"`
	// CallID/Turn/Step 是 tool 结果与它那次调用的关联身份
	// （TOOL-IDENTITY-LOAD / W187，只加不改）。
	//
	// 为什么必须有：界面把 load 里 `role=="tool"` 的消息恢复成 tool.result 事件，
	// 判据在 TUI `view-spec.ts` 的 `dshGoLoadToolResult`——`callId` 非空、
	// `turn`/`step` 都是有限数字，缺一项就返回 undefined。没有这三个字段，
	// 断线重连/重开走 `session.load` 就等于把工具结果整条丢掉（W182 真机红灯）。
	//
	// **无真值就整个省略**（指针 nil 才省略）：不许写 0——0 是合法数字，
	// 界面会拿它当"第 0 轮第 0 步"去恢复一条编出来的工具结果。
	// 它们只在 `Role=="tool"` 时出现；user/assistant 不带。
	CallID string `json:"callId,omitempty"`
	Turn   *int   `json:"turn,omitempty"`
	Step   *int   `json:"step,omitempty"`
	// IsError 表示这条工具结果是**失败**（TOOL-RESULT-ENVELOPE / W190，只加不改）。
	//
	// 为什么必须有：界面把 load 里 `role=="tool"` 的消息恢复成 `tool.result`
	// 事件，成败就是事件上的 `isError`（TUI `view-spec.ts` 里
	// `message.isError === true || message.error !== undefined`）。W187 只补了
	// `callId/turn/step`，于是失败结果**能**恢复成事件、却被画成成功——那比
	// 丢消息更糟：用户会以为命令真的跑通了、失败的命令是成功的。
	//
	// **只输出真值**：`omitempty` 让 `false` 整个省略。缺键与 `false` 都表示
	// "不是失败"，老客户端忽略未知键即可；而 `true` 是唯一需要额外携带的信息，
	// 所以不写 `"isError":false`——无真值时多给一个键就是编。
	IsError bool `json:"isError,omitempty"`
	// Meta 是 durable `tool/result.meta` 的**原样透传**（W190）。
	//
	// 真实 v4 里它就是 `{"cwd": "…"}`（`session.ToolResultEvent` 写的），
	// 与 WS 帧上的 `data.meta` 是同一份字节。缺键/`null` 时**整个省略**，
	// 不编空对象——界面靠 `undefined` 才知道"这次没有元数据"。
	Meta json.RawMessage `json:"meta,omitempty"`
	// Structured 是 durable `tool/result.structured` 的原样透传（W190）。
	//
	// 形状由工具自己定，宿主**不解释**它（也不因为不认识就丢掉）；
	// 只有日志里真有这个键才出现，缺键/`null` 整个省略。
	Structured json.RawMessage `json:"structured,omitempty"`
}

// ContentBlock 是 session.prompt、user/message、load/history 共用的有序块。
// attachment 块线上只需 attachmentId；宿主解析真实 store 后补齐元数据。
// reference 块只接受真实 file path 或 sessionId，referenceId 由宿主生成并持久化。
type ContentBlock struct {
	Type          string `json:"type"`
	Text          string `json:"text,omitempty"`
	AttachmentID  string `json:"attachmentId,omitempty"`
	Name          string `json:"name,omitempty"`
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

// Attachment 是一份真实本地文件的可持久元数据。Width/Height 只对图片非零。
type Attachment struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ByteSize int64  `json:"byteSize"`
	MIME     string `json:"mime"`
	SHA256   string `json:"sha256"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// AttachmentContent 是宿主适配层返回值；Data 不直接 JSON 序列化，
// server 在唯一出口编码成 dataBase64，避免 UI 读到伪造/截断的另一份字节。
type AttachmentContent struct {
	Attachment Attachment `json:"attachment"`
	Data       []byte     `json:"-"`
}

// AttachmentStore 由宿主实现。transport 只经 cookie 后的方法调用它，不自行读盘。
type AttachmentStore interface {
	Put(path string) (Attachment, error)
	Get(id string) (AttachmentContent, error)
}

// UsageMsg 是一条助手消息的用量。字段名照 v4 的 usage 原样搬，不另起一套。
type UsageMsg struct {
	InputTokens     int `json:"inputTokens,omitempty"`
	OutputTokens    int `json:"outputTokens,omitempty"`
	TotalTokens     int `json:"totalTokens,omitempty"`
	CacheReadTokens int `json:"cacheReadTokens,omitempty"`
}

// SessionSummary 是 session.list 的一行。Title 要能看懂（一句话摘要）。
type SessionSummary struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	TitleExplicit bool   `json:"titleExplicit,omitempty"`
	TitleSource   string `json:"titleSource,omitempty"`
	CWD           string `json:"cwd"`
	UpdatedAt     string `json:"updatedAt"`
}

// GoalBlockedReason 只在 phase=blocked 时出现。
type GoalBlockedReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// GoalBudget 只承载宿主真实计量的预算；当前宿主未计量时整个字段省略。
type GoalBudget struct {
	TokensUsed      int64  `json:"tokensUsed"`
	TokenBudget     *int64 `json:"tokenBudget"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
}

// GoalSnapshot 是宿主持久化的真实目标状态，不由扁平 objective 临时伪造。
type GoalSnapshot struct {
	ID            string             `json:"id"`
	Revision      int64              `json:"revision"`
	Objective     string             `json:"objective"`
	Phase         string             `json:"phase"`
	MaxGoalRounds int                `json:"maxGoalRounds"`
	BlockedReason *GoalBlockedReason `json:"blockedReason,omitempty"`
	Budget        *GoalBudget        `json:"budget,omitempty"`
}

// GoalResult 是 session.goal.get 的 result。roundsStarted 用指针区分真实 0 与未知。
type GoalResult struct {
	Goal          *GoalSnapshot `json:"goal"`
	RoundsStarted *int          `json:"roundsStarted,omitempty"`
}

// GoalChange 是同一 Transport journal 中 goal/change 帧的 data。
type GoalChange struct {
	Kind          string        `json:"kind"`
	Version       int           `json:"version"`
	Operation     string        `json:"operation"`
	Goal          *GoalSnapshot `json:"goal,omitempty"`
	RoundsStarted *int          `json:"roundsStarted,omitempty"`
	Round         *int          `json:"round,omitempty"`
}

// LoadResult 是 session.load 的返回值。
type LoadResult struct {
	Messages  []Message `json:"messages"`
	Total     int       `json:"total"`
	Truncated bool      `json:"truncated"`
	// Model 是这条会话最近一次请求用的模型 id（H4）。
	// **空字符串表示不知道**，界面就该整段不画——不许拿我们的默认值冒充，
	// 那样用户会以为"这个会话是用默认模型跑的"，而它可能不是。
	Model string `json:"model,omitempty"`
	// Goal 是这条会话当前在做的事（一句话）。空 = 没设过，界面不画 GoalBar。
	Goal string `json:"goal,omitempty"`
	// Permission 是当前审批策略（deny/ask/allow/auto，H5）。
	// **不是 mode**：mode 决定模型能看见哪些工具，权限决定写操作要不要问人。
	Permission string `json:"permission,omitempty"`
	// ContentWidth 是会话内容列的宽度（逻辑像素，H12）。
	//
	// 为什么由宿主给而不是界面自己算：界面在槽位里只拿得到整窗宽度，
	// 而官方气泡上限是 `min(列宽*0.702, 82%)`——没有列宽这个数就只能瞎猜比例。
	// **0 表示不知道**（omitempty），界面沿用自己那套；不许填一个"看着像"的默认值。
	ContentWidth int `json:"contentWidth,omitempty"`
	// UsageTotal 是**整个会话**的累计用量（H1 的顶层合计）。
	//
	// 与逐条 usage 的关系：逐条是"那一轮花了多少"，这个是"这个会话一共花了多少"。
	// **tail 切页不许改变它**——切页只影响返回哪几条消息，不影响全会话统计。
	// 键名与逐条一致（inputTokens/outputTokens），另加 cacheReadTokens 累计。
	UsageTotal *UsageMsg `json:"usageTotal,omitempty"`
	// ContextTokens 是**最近一条** assistant 的 totalTokens，也就是"当前上下文占多少"。
	// 它跟累计值是两件事：累计一直涨，上下文会随压缩回落。
	ContextTokens int `json:"contextTokens,omitempty"`
	// ContextWindow 是这条会话模型的上限（token）。
	// **0 表示不知道**：界面据此决定"画进度条"还是"只报数字"。
	// 不许填一个"看着像"的默认值——那会让百分比永远算错。
	ContextWindow int `json:"contextWindow,omitempty"`
	// JournalEpoch/LastSeq 让客户端拿到"我现在该从哪个水位接着收"（H14）。
	// load 只是首屏投影，**不能用来补洞**；但它必须告诉客户端 journal 在哪。
	JournalEpoch int64 `json:"journalEpoch,omitempty"`
	LastSeq      int   `json:"lastSeq,omitempty"`
	// CWD 是这条会话的工作目录（绝对路径，Windows 风格）。
	//
	// 为什么必须由宿主给：`workspace.list` 只回 `{name,isDir,size}`（没有 path），
	// 界面要读某个文件就得拼「工作区根 + name」——而界面不知道根在哪，
	// 于是 `fs.read` 永远调不出来。空字符串表示不知道（omitempty），
	// **不许填当前进程 cwd 冒充**：多会话可能属于不同工作区。
	CWD string `json:"cwd,omitempty"`
	// Effort 是这条会话最新配置记录里的推理强度（additive，只加不改）。
	// 空串表示旧会话无记录、记录缺字段或枚举未知；不得拿本进程 --reasoning
	// 或模型名冒充。老客户端忽略此键，新客户端据此显示真值或 `?`。
	Effort string `json:"effort,omitempty"`
	// EffortSelection 是持久化的会话覆盖 key；没有覆盖时省略。
	// 它与 Effort（最近实际请求记录）分离，也不回退到进程 --reasoning。
	EffortSelection string `json:"effortSelection,omitempty"`
}

// HistoryResult 是 session.history 的返回值（H14）。
//
// **frames 是原始帧，不是投影后的 messages**：补洞要能恢复 reasoning/tool/approval，
// 而投影只保留 text。lastSeq/journalEpoch 一并给出，客户端据此判断水位是否可用。
type HistoryResult struct {
	Frames       []ServerFrame `json:"frames"`
	LastSeq      int           `json:"lastSeq"`
	JournalEpoch int64         `json:"journalEpoch"`
}

// SubmitPlacement 是 session.prompt 的可选落点。空串与 turn 保持旧语义；
// queue 是 followup 的宿主侧别名，线上结果仍只使用 canonical 的 PromptResult。
type SubmitPlacement string

const (
	PlacementTurn     SubmitPlacement = "turn"
	PlacementSteer    SubmitPlacement = "steer"
	PlacementFollowup SubmitPlacement = "followup"
	PlacementQueue    SubmitPlacement = "queue"
	PlacementNow      SubmitPlacement = "now"
)

var (
	// ErrEffortInvalid 表示 level 不是当前模型 catalog 声明的 reasoningEfforts key。
	ErrEffortInvalid = errors.New("effort 不在当前模型 reasoningEfforts")
	// ErrGoalTransition 表示目标不存在或当前 phase 不允许这次迁移。
	ErrGoalTransition = errors.New("非法 goal 状态迁移")
	// ErrStatePersistence 表示状态/journal 的真实读写提交失败；HTTP 必须响亮报错。
	ErrStatePersistence = errors.New("会话状态持久化失败")
	// ErrPlacementInvalid 表示调用方给了不认识的 placement/cause；HTTP 映射为 400。
	ErrPlacementInvalid = errors.New("invalid prompt placement")
	// ErrPlacementUnsupported 表示当前真实 runner 没有该能力；HTTP 映射为 501。
	// 尤其 busy steer 不能退成排队：只有 runner 确认注入当前轮才允许 accepted。
	ErrPlacementUnsupported = errors.New("prompt placement unsupported")
)

// NormalizeSubmitPlacement 校验并返回 canonical placement；空串保持旧 turn 语义。
func NormalizeSubmitPlacement(value string) (string, error) {
	switch SubmitPlacement(value) {
	case "", PlacementTurn:
		return string(PlacementTurn), nil
	case PlacementSteer:
		return string(PlacementSteer), nil
	case PlacementFollowup, PlacementQueue:
		return string(PlacementFollowup), nil
	case PlacementNow:
		return string(PlacementNow), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrPlacementInvalid, value)
	}
}

// PromptResult 是 session.prompt 的返回值：turn/排队/真实 steer 确认都立刻返回，
// 回答走 WS。queued followup 的 TurnID 是已保留、之后真实执行的未来轮次 id；
// busy steer 成功时 TurnID 是被注入的当前轮次 id。canonical 形状不新增字段。
type PromptResult struct {
	Accepted bool   `json:"accepted"`
	TurnID   string `json:"turnId"`
}

// CancelResult 是 additive cancel receipt。StillQueued/CancelledQueued 使用宿主
// TurnID（不是 TUI clientMessageId）；调用方若需要官方 CancelReceipt，必须按
// prompt ACK 的 TurnID 自行映射，不能在收到 ACK 前编造 confirmed 空队列。
type CancelResult struct {
	Cancelled       bool     `json:"cancelled"`
	TurnID          string   `json:"turnId,omitempty"`
	CancelledQueued []string `json:"cancelledQueued,omitempty"`
	StillQueued     []string `json:"stillQueued"`
	Outcome         string   `json:"outcome"`
}

// WorkspaceEntry 是 workspace.list 返回的一条。
//
// 只有三个字段：多一个都得由界面负责渲染，而界面现在只用得上这三个。
// 以后要加（比如修改时间）再显式改快照，不要"顺手多带点"。
type WorkspaceEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

// FSContent 是 fs.read 的返回：路径、正文、字节数。
type FSContent struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Size    int64  `json:"size"`
}
