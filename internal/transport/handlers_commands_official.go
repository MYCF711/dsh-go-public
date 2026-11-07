package transport

// 本文件是官方 `commands.*`（`@deepseek-ai/dsh-commands` 的
// `CommandRuntime.list` / `CommandRuntime.execute`）在**宿主侧的真实命令注册表**。
//
// 为什么注册表落在 transport 而不是 cmd/dshd：
//   - 注册表本身是**纯逻辑**——名字解析、scoped shadowing 的等价物、生命周期
//     事件、结果归一化、附件准入——它不碰盘、不认识会话日志（红线 R7）；
//   - 真实状态（会话日志追加、compaction、模型覆盖）由宿主经
//     OfficialCommandLifecycle 与各命令自己的 handler 注入；
//   - 于是 A/B 两个 Server 各挂各的注册表，不存在进程级单例（R2）。
//
// 与官方的逐条对应（lib/index.js）：
//   - `parseCommand`：`/^\/([a-z][a-z0-9_-]*)(?=$|[\t\n\r ])/u`，name + rawInput；
//     Go 的 RE2 没有 lookahead，这里手写等价判定（见 parseOfficialCommand）。
//   - `normalizeDefinition`：name 必须是 lowercase 命令名、description 非空、
//     handler 必填、input.attachments 只能是 bool。
//   - `normalizeResult`：kind 只取 success/error；error 的 text 必须**非空且
//     非全空白**；sourceEventSeq 只许非负安全整数。
//   - `mintCommandId`：`cmd-<instanceToken>-<seq>`，instanceToken 每进程一份。
//   - `execute` 的生命周期：`command/run` 在 handler 之前追加，`command/done`
//     在结算之后追加；admission miss（语法/名字不解析）**什么都不写**。
//   - 附件准入在 execute 里做，不在 composer：命令没声明 `input.attachments`、
//     没有附件存储、解码失败，都结算成 `kind:"error"` 的**结果**（HTTP 200），
//     不是 transport 错误。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// 定义与调用
// ---------------------------------------------------------------------------

// OfficialCommandHandler 跑一条命令。**不许**返回假数据：拿不到真东西就
// 返回 error，由 execute 收敛成 transport 错误（不是假成功）。
type OfficialCommandHandler func(ctx context.Context, inv OfficialCommandInvocation) (OfficialCommandOutcome, error)

// OfficialCommandDefinition 是一条可注册命令（官方 `CommandDefinition`）。
type OfficialCommandDefinition struct {
	// DefinitionID 是插件自有的稳定身份；没有就留空（官方可选）。
	DefinitionID string
	// Name 是不带斜杠的小写命令名（官方 `/^[a-z][a-z0-9_-]*$/`）。
	Name string
	// Description 是发现界面用的一句话摘要，**必须非空**。
	Description string
	// Input 是可选的自由输入提示与附件声明。
	Input *OfficialCommandInput
	// Handler 是直接 UI 处理函数，**必须非空**。
	Handler OfficialCommandHandler
}

// OfficialCommandInvocation 是一次调用（官方 `CommandInvocation`）。
type OfficialCommandInvocation struct {
	CommandID string
	SessionID string
	RawInput  string
	// Attachments 是**已准入**的附件；空切片表示这次调用没带附件。
	Attachments []OfficialCommandAttachment
}

// OfficialCommandAttachment 是准入后的附件（图片已解码、文件已解析）。
//
// 与 wire 上的 OfficialSubmittedAttachment 分开：wire 上的 `data` 是 base64，
// 准入后是能直接交给工具/模型的字节；混成同一个类型会让"没校验的字符串"
// 一路溜进 handler。
type OfficialCommandAttachment struct {
	Type      string
	MediaType string
	Name      string
	// Bytes 只在 Type=="image" 时有值（解码后的原始字节）。
	Bytes []byte
	// ReceiptID 只在 Type=="file" 时有值（受控存储里的回执）。
	ReceiptID string
}

// OfficialCommandLifecycle 是宿主注入的**durable 生命周期追加**口。
//
// 官方 `appendLifecycle` 往会话日志直接追加 `command/run` / `command/done`
// （log-only，无 turn 包裹）。transport 不写盘（R7），所以由宿主提供实现；
// 返回帧上的真实时间/序号由宿主决定，transport 只消费 error。
type OfficialCommandLifecycle func(sessionID, eventType string, data any) (seq int64, err error)

// OfficialCommandRegistryOptions 装配一个注册表。
type OfficialCommandRegistryOptions struct {
	// Lifecycle 为 nil 时，`commands.execute` 明确未实现（不能只执行不记账：
	// 官方对 `command/run` 追加失败是 fail-loud 的）。
	Lifecycle OfficialCommandLifecycle
	// Attachments 是受控附件存储；nil = 没接（带附件的调用结算成 error 结果）。
	Attachments AttachmentStore
	// InstanceToken 覆盖 commandId 的实例段（测试钉形状用）；空则随机。
	InstanceToken string
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

// OfficialCommandRegistry 是真实、可插拔、可列出的命令注册表。
//
// 并发：Register 与 Execute 可并发；Execute 内部先取定义快照再跑 handler，
// 因此执行期间注册/注销新命令不会让这次调用看到半个定义。
type OfficialCommandRegistry struct {
	mu    sync.RWMutex
	defs  map[string]OfficialCommandDefinition
	opts  OfficialCommandRegistryOptions
	token string
	seq   atomic.Uint64
}

// NewOfficialCommandRegistry 造一个空注册表。
func NewOfficialCommandRegistry(opts OfficialCommandRegistryOptions) *OfficialCommandRegistry {
	token := opts.InstanceToken
	if token == "" {
		var b [4]byte
		if _, err := rand.Read(b[:]); err == nil {
			token = hex.EncodeToString(b[:])
		} else {
			// 拿不到随机源也不该让宿主起不来：退化成固定段，唯一性由 seq 保证。
			token = "00000000"
		}
	}
	return &OfficialCommandRegistry{defs: map[string]OfficialCommandDefinition{}, opts: opts, token: token}
}

// Register 注册一条命令，返回**精确**的注销函数（官方 `commands.register()`）。
//
// 校验是 fail-loud 的：名字不合法、description 空、handler 为 nil、重名，
// 全部返回 error 而不是静默忽略——静默忽略会让"我注册了但它没出现"变成
// 一个只能靠猜的 bug。
func (r *OfficialCommandRegistry) Register(def OfficialCommandDefinition) (func(), error) {
	if r == nil {
		return nil, fmt.Errorf("commands.register: 注册表为 nil")
	}
	if err := def.validate(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.defs[def.Name]; dup {
		return nil, fmt.Errorf("commands.register: 命令 %q 已经注册过（同名只能有一份定义）", def.Name)
	}
	r.defs[def.Name] = def
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			// 只删**自己那一份**：别人重注册过同名命令时，注销不能把人家的删掉。
			if cur, ok := r.defs[def.Name]; ok && cur.DefinitionID == def.DefinitionID && cur.Description == def.Description {
				delete(r.defs, def.Name)
			}
		})
	}, nil
}

func (d OfficialCommandDefinition) validate() error {
	if !isOfficialCommandName(d.Name) {
		return fmt.Errorf("commands.register: 命令名 %q 不合法（官方是 /^[a-z][a-z0-9_-]*$/，不带斜杠）", d.Name)
	}
	if d.Description == "" {
		return fmt.Errorf("commands.register: 命令 %q 的 description 不能为空（发现界面要显示它）", d.Name)
	}
	if d.Handler == nil {
		return fmt.Errorf("commands.register: 命令 %q 没有 handler（注册了也不能执行）", d.Name)
	}
	return nil
}

// isOfficialCommandName 是官方正则 `/^[a-z][a-z0-9_-]*$/` 的逐字翻译。
func isOfficialCommandName(name string) bool {
	if name == "" {
		return false
	}
	if name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// List 返回 name 升序的不可变描述符（官方 `list()` 的排序口径）。
func (r *OfficialCommandRegistry) List() []OfficialCommand {
	if r == nil {
		return []OfficialCommand{}
	}
	r.mu.RLock()
	out := make([]OfficialCommand, 0, len(r.defs))
	for _, def := range r.defs {
		out = append(out, def.descriptor())
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (d OfficialCommandDefinition) descriptor() OfficialCommand {
	cmd := OfficialCommand{
		DefinitionID: d.DefinitionID,
		Name:         d.Name,
		Description:  d.Description,
	}
	if d.Input != nil {
		input := *d.Input
		cmd.Input = &input
	}
	return cmd
}

// Find 解析一条有效命令定义（官方 `find()`）。
func (r *OfficialCommandRegistry) Find(name string) (OfficialCommandDefinition, bool) {
	if r == nil {
		return OfficialCommandDefinition{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.defs[name]
	return def, ok
}

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

// Execute 解析并执行一条已知命令，不把它送给模型。
//
// 返回值语义与官方逐条对齐：
//   - 语法不解析 / 名字不存在 → (nil, nil)，即官方的 `undefined`；
//   - 已解析 → 追加 `command/run` → 附件准入 → handler → 归一化 →
//     追加 `command/done` → 返回 {commandId, result}；
//   - ctx 已取消 → 错误（官方 `abortError`）；
//   - 生命周期追加失败 → 错误（官方 `command/run` 追加失败 fail-loud；
//     成功路径上的 `command/done` 追加失败同样上抛，不吞成成功）。
func (r *OfficialCommandRegistry) Execute(ctx context.Context, sessionID, line string, attachments []OfficialSubmittedAttachment) (*OfficialCommandExecution, error) {
	name, rawInput, ok := parseOfficialCommand(line)
	if !ok {
		return nil, nil
	}
	def, found := r.Find(name)
	if !found {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.opts.Lifecycle == nil {
		return nil, officialNotImplemented("commands.execute 未接线：宿主没有 command 生命周期追加口（command/run 记账无处落盘）")
	}

	commandID := r.mint()
	runEvent := map[string]any{
		"commandId": commandID,
		"name":      name,
		"source":    map[string]any{"kind": "user"},
	}
	if rawInput != "" {
		runEvent["args"] = rawInput
	}
	if _, err := r.opts.Lifecycle(sessionID, "command/run", runEvent); err != nil {
		return nil, officialInternal("commands.execute: 追加 command/run 失败: %v", err)
	}

	settle := func(outcome OfficialCommandOutcome) (*OfficialCommandExecution, error) {
		doneEvent := map[string]any{"commandId": commandID, "kind": outcome.Kind}
		if outcome.Kind == "success" && outcome.Text != "" {
			doneEvent["text"] = outcome.Text
		} else if outcome.Kind == "error" {
			doneEvent["text"] = outcome.Text
		}
		if outcome.SourceEventSeq != nil {
			doneEvent["sourceEventSeq"] = *outcome.SourceEventSeq
		}
		if _, err := r.opts.Lifecycle(sessionID, "command/done", doneEvent); err != nil {
			return nil, officialInternal("commands.execute: 追加 command/done 失败: %v", err)
		}
		return &OfficialCommandExecution{CommandID: commandID, Result: outcome}, nil
	}

	// 附件准入：官方在这里（不是 composer）判"能不能带"。
	admitted, settleErr, err := r.admitAttachments(name, def, attachments)
	if err != nil {
		return nil, err
	}
	if settleErr != nil {
		return settle(*settleErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	outcome, handlerErr := def.Handler(ctx, OfficialCommandInvocation{
		CommandID: commandID, SessionID: sessionID, RawInput: rawInput, Attachments: admitted,
	})
	if handlerErr != nil {
		// 官方：handler 抛出的错误先记一条 `command/done`（kind:"error"，contained），
		// 再把原错误上抛——handler 自己的失败才是报告出来的那个失败。
		r.settleThrown(sessionID, commandID, handlerErr)
		return nil, officialInternal("命令 /%s 执行失败: %v", name, handlerErr)
	}
	normalized, err := normalizeOfficialCommandOutcome(name, outcome)
	if err != nil {
		r.settleThrown(sessionID, commandID, err)
		return nil, officialBadRequest("%v", err)
	}
	return settle(normalized)
}

// settleThrown 是 handler 抛错时的 contained 记账（追加失败只记日志、不遮蔽原错误）。
func (r *OfficialCommandRegistry) settleThrown(sessionID, commandID string, cause error) {
	if r.opts.Lifecycle == nil {
		return
	}
	text := cause.Error()
	if strings.TrimSpace(text) == "" {
		text = "命令执行失败（无错误文本）"
	}
	_, _ = r.opts.Lifecycle(sessionID, "command/done", map[string]any{
		"commandId": commandID, "kind": "error", "text": text,
	})
}

// admitAttachments 实现官方附件准入。
//
// 返回 (准入结果, 要结算的 error 结果, transport 错误)：
//   - 没带附件 → (nil, nil, nil)；
//   - 命令没声明 attachments → 结算 `/<name> does not accept attachments`；
//   - 没有附件存储 → 结算 `/<name>: attachments are unavailable because no
//     attachment store is composed`；
//   - 解码/形状不合法 → transport 400（wire 上的形状错，不是命令策略）。
func (r *OfficialCommandRegistry) admitAttachments(name string, def OfficialCommandDefinition, submitted []OfficialSubmittedAttachment) ([]OfficialCommandAttachment, *OfficialCommandOutcome, error) {
	if len(submitted) == 0 {
		return nil, nil, nil
	}
	if def.Input == nil || def.Input.Attachments == nil || !*def.Input.Attachments {
		return nil, &OfficialCommandOutcome{Kind: "error", Text: fmt.Sprintf("/%s does not accept attachments", name)}, nil
	}
	if r.opts.Attachments == nil {
		return nil, &OfficialCommandOutcome{
			Kind: "error",
			Text: fmt.Sprintf("/%s: attachments are unavailable because no attachment store is composed", name),
		}, nil
	}
	admitted := make([]OfficialCommandAttachment, 0, len(submitted))
	for i, item := range submitted {
		part := OfficialCommandAttachment{Type: item.Type, MediaType: item.MediaType, Name: item.Name}
		switch item.Type {
		case "image":
			raw, err := base64.StdEncoding.DecodeString(item.Data)
			if err != nil {
				return nil, nil, officialBadRequest("submittedAttachments[%d].data 不是规范 base64: %v", i, err)
			}
			if len(raw) == 0 {
				return nil, nil, officialBadRequest("submittedAttachments[%d] 解码后是空字节", i)
			}
			part.Bytes = raw
		case "file":
			if item.ReceiptID == "" {
				return nil, nil, officialBadRequest("submittedAttachments[%d].receiptId 是空串", i)
			}
			part.ReceiptID = item.ReceiptID
		default:
			return nil, nil, officialBadRequest("submittedAttachments[%d].type 必须是 image 或 file", i)
		}
		admitted = append(admitted, part)
	}
	return admitted, nil, nil
}

// mint 造下一个生命周期配对 id（官方 `mintCommandId`）。
func (r *OfficialCommandRegistry) mint() string {
	return fmt.Sprintf("cmd-%s-%d", r.token, r.seq.Add(1))
}

// isOfficialCommandNameChar 判断一个字节是否属于命令名字符集（`[a-z0-9_-]`）。
func isOfficialCommandNameChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-':
		return true
	}
	return false
}

// parseOfficialCommand 是官方 `parseCommand` 的逐字翻译。
//
// 官方正则：/^\/([a-z][a-z0-9_-]*)(?=$|[\t\n\r ])/u。RE2 不支持 lookahead，
// 所以手写：以 '/' 开头、名字段按官方字符集贪婪取、下一个字符必须是
// 串尾或 tab/LF/CR/空格 之一（**空格算分隔**，别的标点不算）。
func parseOfficialCommand(line string) (name, rawInput string, ok bool) {
	if len(line) < 2 || line[0] != '/' {
		return "", "", false
	}
	if c := line[1]; c < 'a' || c > 'z' {
		return "", "", false
	}
	i := 1
	for i < len(line) && isOfficialCommandNameChar(line[i]) {
		i++
	}
	if i < len(line) {
		switch line[i] {
		case '\t', '\n', '\r', ' ':
		default:
			return "", "", false
		}
	}
	return line[1:i], line[i:], true
}

// normalizeOfficialCommandOutcome 是官方 `normalizeResult` 的逐字翻译。
func normalizeOfficialCommandOutcome(name string, value OfficialCommandOutcome) (OfficialCommandOutcome, error) {
	switch value.Kind {
	case "success":
		if value.SourceEventSeq != nil && *value.SourceEventSeq < 0 {
			return OfficialCommandOutcome{}, fmt.Errorf("command %q success sourceEventSeq 必须是**非负**整数", name)
		}
		return value, nil
	case "error":
		if strings.TrimSpace(value.Text) == "" {
			return OfficialCommandOutcome{}, fmt.Errorf("command %q error text 必须非空（空错误等于静默失败）", name)
		}
		return value, nil
	default:
		return OfficialCommandOutcome{}, fmt.Errorf("command %q 返回了未知 result kind %q（只取 success/error）", name, value.Kind)
	}
}
