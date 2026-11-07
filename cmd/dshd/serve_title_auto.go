package main

// 官方 0.2.2 #14 `session-title-llm` / `session-title-first-prompt-llm`：
// 首条人类消息之后，用**一次独立、无工具、不落历史**的模型调用生成会话标题。
//
// # 官方证据（app.asar 内的真实包，逐条对齐）
//
//   - `node_modules/@deepseek-ai/dsh-session-title-first-prompt-llm/lib/index.js`
//     system prompt 原文、`Generate the session title from this JSON array of human messages:` 帧、
//     `titleFromOutput`（首个非空行 + 去整行 1–3 个 `*` 包裹）、
//     `messageSeqs` 记录来源消息、finish 分类（tool-calls / max-tokens 都算失败）。
//   - `node_modules/@deepseek-ai/dsh-session-title/lib/types/normalize.js`
//     `normalizeSessionTitle`：清 OSC/CSI/ESC、C0/C1 控制字符、方向与不可见字符，
//     `\s+` → 单空格，trim，按 UTF-8 字节上限截断且不拆码点。
//   - `node_modules/@deepseek-ai/dsh-session-title/lib/index.js`
//     触发条件：`!header.isSeeded && header.origin !== "subagent" && count === 1 && 当前无 title`；
//     用户显式标题会 supersede 在飞的自动生成（`source.kind === "user"` 永不被覆盖）。
//   - `node_modules/@deepseek-ai/dsh-session-title-llm/lib/index.js`
//     共享执行控制（maxInputBytes / maxOutputTokens / timeoutMs）与
//     「标题请求与主对话请求相互独立、不进入对话历史」的硬约束。
//
// # 为什么这个文件在 cmd/dshd，而不是 internal/*
//
// 官方把标题生成挂在 session-title 服务上，触发点在主请求身份记录之后。宿主这里
// 唯一可挂的"这一轮真收工"位置是 `modelOverrideLoopRunner`（cmdServe 的默认 runner），
// 而 v4 子进程（`dshd run`）里的 agent loop 不持有 Transport journal/索引写口。
// 于是：**父进程每轮收工后**在后台跑一次独立调用，写 v4 + 索引 + journal 帧。
//
// # 三条硬边界
//
//  1. 只有"首轮已正常收口 + 当前没有任何标题"才生成；用户显式标题永远优先
//     （写前在会话写锁内**重读**，用户标题在场就放弃）；
//  2. 失败/超时/空标题一律**不写**、不重试、不阻塞主 turn（Kick 是后台 goroutine，
//     本函数的 Run 只被测试与它同步调用）；
//  3. 标题调用不写任何 user/assistant/turn 事件，因此不进模型历史、不进 token 计量；
//     v4 只多一条 `session/title`。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"dshgo/codecs/v4"
	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// 官方 session-title-llm 的共享执行控制与 first-prompt 的长度目标都**没有库默认值**，
// 由装配方显式给。这里取官方 0.2.2 包内随附配置里出现的那组值：
// `maxInputBytes: 4096`、`maxOutputTokens: 64`、`targetWords: 5`、`targetCjkCharacters: 10`；
// 官方标题服务随附配置有 `maxTitleBytes: 80` 与 `120` 两档，这里取较宽的一档
// （标题是给人看的，120 字节仍远小于侧栏宽度，且不拆码点）。
//
// timeoutMs 官方同样必填：这里取 20s。标题调用是后台任务，超时只意味着"这一格
// 不写标题"，不重试；健康调用（输入 ≤4KB、输出 ≤64 token）远小于它。
const (
	autoTitleMaxInputBytes   = 4096
	autoTitleMaxOutputTokens = 64
	autoTitleMaxBytes        = 120
	autoTitleTargetWords     = 5
	autoTitleTargetCJK       = 10
	autoTitleTimeout         = 20 * time.Second
)

// autoTitleSysPrompt 逐字来自官方 first-prompt provider 的 systemPrompt()。
func autoTitleSysPrompt() string {
	return strings.Join([]string{
		"Create a concise title for an AI coding-assistant session from the supplied human messages.",
		"Return only the title on one line, **in plain text of natural language**, with no quotes, prefix, explanation, Markdown, XML, or terminal control codes. No code is allowed.",
		"Use the language of the messages.",
		fmt.Sprintf("Aim for about %d words in non-CJK languages or %d CJK characters.", autoTitleTargetWords, autoTitleTargetCJK),
		"If the messages give little to name, still return a short best-effort title, such as Greeting, instead of explaining.",
	}, "\n")
}

// autoTitleMessage 是官方 `collectSessionTitleMessages` 的元素形状：`{seq, text}`。
//
// seq 留着是**官方帧输入的一部分**（用户文本进 JSON，不拼进结构分隔符）；
// 写进 `session/title` 的 messageSeqs 按本任务契约固定为 `[]`，见 commit()。
type autoTitleMessage struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

// autoTitleInput 逐字对齐官方 `frameMessages()`：整条输入是一个 JSON 数组。
func autoTitleInput(messages []autoTitleMessage) string {
	raw, err := json.Marshal(messages)
	if err != nil {
		// 元素只有数字与字符串，实际不可能失败；真失败了也不能把半截文字当帧发出去。
		raw = []byte("[]")
	}
	return "Generate the session title from this JSON array of human messages:\n" + string(raw)
}

// ---------- 官方 normalize 的移植 ----------

// 这五条正则逐条对应官方 normalize.js 里的同名常量。Go 的 RE2 没有反向引用，
// 但官方这几条都不需要；字符类与量词保持一字不差。
var (
	autoTitleOSC         = regexp.MustCompile(`(?:\x1b\]|` + "\u009d" + `)[^\x07\x1b]*(?:\x07|\x1b\\|$)`)
	autoTitleCSI         = regexp.MustCompile(`(?:\x1b\[|` + "\u009b" + `)[0-?]*[ -/]*[@-~]`)
	autoTitleESC         = regexp.MustCompile(`\x1b[@-_]`)
	autoTitleControl     = regexp.MustCompile("[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\u009f]")
	autoTitleDirectional = regexp.MustCompile("[\u200b\u200e\u200f\u202a-\u202e\u2060-\u2064\u2066-\u206f\ufeff]")
	autoTitleSpace       = regexp.MustCompile(`\s+`)
)

// autoTitleCleanText 是官方 cleanTitleText()：清控制序列 → 归一空白 → trim。
func autoTitleCleanText(input string) string {
	input = autoTitleOSC.ReplaceAllString(input, "")
	input = autoTitleCSI.ReplaceAllString(input, "")
	input = autoTitleESC.ReplaceAllString(input, "")
	input = autoTitleControl.ReplaceAllString(input, "")
	input = autoTitleDirectional.ReplaceAllString(input, "")
	input = autoTitleSpace.ReplaceAllString(input, " ")
	return strings.TrimSpace(input)
}

// autoTitleTruncateUTF8 是官方 truncateTitleUtf8()：按字节上限取最长前缀，
// 且绝不切开一个码点（否则 JSON/索引里会留下半个 rune）。
func autoTitleTruncateUTF8(input string, maxBytes int) string {
	if maxBytes <= 0 || len(input) <= maxBytes {
		return input
	}
	var b strings.Builder
	used := 0
	for _, r := range input {
		n := utf8.RuneLen(r)
		if n < 0 {
			n = 1
		}
		if used+n > maxBytes {
			break
		}
		b.WriteRune(r)
		used += n
	}
	return b.String()
}

// normalizeAutoTitle 是官方 normalizeSessionTitle()：清洗 → 加权截断 → trimEnd。
// 空串返回空串，由调用方当作"这次不写标题"。
func normalizeAutoTitle(input string, maxBytes int) string {
	return strings.TrimRight(autoTitleTruncateUTF8(autoTitleCleanText(input), maxBytes), " ")
}

// autoTitleFromOutput 是官方 titleFromOutput()：
// 取首个非空行；整行被 1–3 个 `*` 包裹时剥掉包裹；内层再出现同一串 `*` 视为
// 模型本意（不剥），首/尾是空白也不剥（官方正则要求 `\S` 开头结尾）。
func autoTitleFromOutput(text string) string {
	line := ""
	for _, item := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			line = trimmed
			break
		}
	}
	for _, marker := range []string{"***", "**", "*"} {
		if !strings.HasPrefix(line, marker) || !strings.HasSuffix(line, marker) || len(line) < 2*len(marker) {
			continue
		}
		inner := line[len(marker) : len(line)-len(marker)]
		if inner == "" || strings.Contains(inner, marker) {
			continue
		}
		first, _ := utf8.DecodeRuneInString(inner)
		last, _ := utf8.DecodeLastRuneInString(inner)
		if unicode.IsSpace(first) || unicode.IsSpace(last) {
			continue
		}
		return inner
	}
	return line
}

// ---------- 触发条件 ----------

// autoTitleUserMessage 是官方 `sessionTitleUserMessageOf()`：只认 `source.kind=="user"`
// 的 user/message，文本按 text 块以 "\n" 连接，normalize 之后为空的不算。
func autoTitleUserMessage(ev v4.Event) (autoTitleMessage, bool) {
	if ev.Type != "user/message" {
		return autoTitleMessage{}, false
	}
	var data struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return autoTitleMessage{}, false
	}
	if data.Source.Kind != "user" {
		return autoTitleMessage{}, false
	}
	texts := make([]string, 0, len(data.Content))
	for _, block := range data.Content {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if normalizeAutoTitle(text, autoTitleMaxInputBytes) == "" {
		return autoTitleMessage{}, false
	}
	return autoTitleMessage{Seq: ev.Seq, Text: text}, true
}

func autoTitleTurnEndKind(ev v4.Event) string {
	var data struct {
		Reason struct {
			Kind string `json:"kind"`
		} `json:"reason"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return ""
	}
	return data.Reason.Kind
}

// autoTitlePrompt 判定"这一格现在该不该生成标题"，并返回首条人类消息。
//
// 条件是官方 first-prompt 触发的宿主版：
//   - 头不是 seeded、不是子代理（本宿主用 DelegationDepth 表达；ParentSession 是 fork）；
//   - 只有一条合格用户消息（steer 出第二条也不算，和官方 count===1 一致）；
//   - 当前日志里**没有任何** session/title（含用户显式标题、fork 带过来的标题）；
//   - 恰好一轮 turn 已收口，且收口原因是 completed / max-tokens。
//
// 任一条不满足就返回 false：调用方只把它当"跳过"，不重试、不写任何东西。
func autoTitlePrompt(log *v4.Log) (autoTitleMessage, bool) {
	if log == nil || log.Header.IsSeeded || log.Header.DelegationDepth > 0 {
		return autoTitleMessage{}, false
	}
	var (
		first        autoTitleMessage
		users        int
		starts, ends int
		open         bool
		lastEnd      string
	)
	for _, ev := range log.Events {
		switch ev.Type {
		case "session/title":
			// 已有任何标题（user / auto / fork 带过来的）都不再补。
			return autoTitleMessage{}, false
		case "turn/start":
			starts++
			open = true
		case "turn/end":
			ends++
			open = false
			lastEnd = autoTitleTurnEndKind(ev)
		case "user/message":
			if msg, ok := autoTitleUserMessage(ev); ok {
				users++
				if users == 1 {
					first = msg
				}
			}
		}
	}
	if users != 1 || starts != 1 || ends != 1 || open {
		return autoTitleMessage{}, false
	}
	switch lastEnd {
	case "completed", "max-tokens":
		// 这一轮真的产出了回答（max-tokens 只是被截断，不是失败）。
	case "":
		// 老日志的 turn/end 可能没有 reason：按"已收口"接受，不因格式差异丢掉标题。
	default:
		return autoTitleMessage{}, false
	}
	if first.Text == "" {
		return autoTitleMessage{}, false
	}
	return first, true
}

// ---------- 独立模型调用 ----------

// autoTitleCall 是一次性的标题请求。没有任何工具、没有主 turn 的身份、
// 没有 follow/journal 写口——它只能返回一段文本。
type autoTitleCall struct {
	model      string
	base       string
	credential string
	input      string
}

func (c autoTitleCall) generate(ctx context.Context) (string, error) {
	if strings.TrimSpace(c.input) == "" {
		return "", errors.New("session-title: 标题输入为空")
	}
	chat := llm.New(c.base, c.credential)
	chat.UserAgent = "dshd/" + version + " session-title"
	resp, err := chat.Stream(ctx, llm.Request{
		Model: c.model,
		Messages: []llm.Message{
			{Role: "system", Content: autoTitleSysPrompt()},
			{Role: "user", Content: c.input},
		},
		// 独立上限：不继承主对话的 max tokens（官方同款约束）。
		MaxTokens: autoTitleMaxOutputTokens,
	}, nil)
	if err != nil {
		return "", redactServeProviderError(err, c.credential)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if resp == nil {
		return "", errors.New("session-title: 模型返回 nil response")
	}
	if len(resp.ToolCalls) > 0 || resp.FinishReason == "tool_calls" {
		return "", errors.New("session-title: 标题输出只能是文本（模型给出了工具调用）")
	}
	if resp.FinishReason == "length" {
		return "", fmt.Errorf("session-title: 标题输出撞上 maxOutputTokens=%d", autoTitleMaxOutputTokens)
	}
	title := normalizeAutoTitle(autoTitleFromOutput(resp.Content), autoTitleMaxBytes)
	if title == "" {
		return "", errors.New("session-title: 模型没有给出标题")
	}
	return title, nil
}

// ---------- 耐久写：v4 + intent + 读索引 ----------

// 下面两个结构逐字对应 internal/session 里**未导出**的 intent/索引形状。
// 那一侧写 user 标题的实现（AppendSessionTitle → appendSessionMetadata）不在本任务
// 写作用域内，而 auto 标题必须落进同一套只读恢复协议，所以在这里按同一形状重写一遍。
// 任何一侧改形状，另一侧都会立刻读不出来——这是刻意让它们互相约束。
type autoTitleTransaction struct {
	Version  int             `json:"version"`
	EventSeq int64           `json:"eventSeq"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}

type autoTitleIndex struct {
	Version  int     `json:"version"`
	EventSeq *int64  `json:"eventSeq,omitempty"`
	Title    *string `json:"title,omitempty"`
	Source   string  `json:"source,omitempty"`
}

func autoTitleIndexPath(sessionPath string) string {
	return filepath.Join(filepath.Dir(sessionPath), "session-title.json")
}

func autoTitleTransactionPath(sessionPath string) string {
	return filepath.Join(filepath.Dir(sessionPath), "session-title.txn.json")
}

// autoTitleIndexPresent 是便宜的前置门：有索引就说明这个会话已经有标题，
// 直接跳过而不必解压整份 v4。索引损坏也当"有标题"跳过——auto 不该去动它。
func autoTitleIndexPresent(sessionPath string) bool {
	_, err := os.Stat(autoTitleIndexPath(sessionPath))
	return err == nil
}

// writeAutoTitleIndex 与 internal/session.writeSessionMetadataIndex 同一算法：
// 同目录随机临时名 → Sync → Close → 原子 rename。不共享临时名，避免并发写者互相踩。
func writeAutoTitleIndex(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auto-title-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// ---------- 服务 ----------

// autoTitleService 是进程内的自动标题写口。
//
// 字段全部显式（home/journal/pub/超时），测试因此能构造短超时或空 journal；
// 生产由 `modelOverrideLoopRunner.autoTitleService()` 懒建一份。
type autoTitleService struct {
	home     string
	journals *transport.Journals
	pub      func(topic string, f transport.ServerFrame)
	timeout  time.Duration

	mu     sync.Mutex
	busy   map[string]bool
	closed map[string]bool // 已判定"这一格不会再有标题"（有界，见 markDone）
}

func newAutoTitleService(home string, journals *transport.Journals, pub func(string, transport.ServerFrame)) *autoTitleService {
	return &autoTitleService{
		home: home, journals: journals, pub: pub, timeout: autoTitleTimeout,
		busy: map[string]bool{}, closed: map[string]bool{},
	}
}

// autoTitleClosedMemoLimit 是"已判定无标题"缓存的上界：只是一层性能记忆，
// 满了就整体清空，绝不为了它无限长。判据本身永远靠 v4 现场重算。
const autoTitleClosedMemoLimit = 512

func (s *autoTitleService) isClosed(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed[sessionID]
}

func (s *autoTitleService) markClosed(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.closed) >= autoTitleClosedMemoLimit {
		s.closed = map[string]bool{}
	}
	s.closed[sessionID] = true
}

func (s *autoTitleService) begin(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[sessionID] {
		return false
	}
	s.busy[sessionID] = true
	return true
}

func (s *autoTitleService) end(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, sessionID)
}

// Kick 起一个后台 goroutine：主 turn 已经把 turn/done 推出去了，标题调用
// 绝不能挡在它前面。失败只留一行可读的宿主 stderr（凭据已脱敏），不重试。
func (s *autoTitleService) Kick(sessionID, model, base, credential string) {
	if s == nil || s.home == "" || sessionID == "" {
		return
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = autoTitleTimeout
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if _, err := s.Run(ctx, sessionID, model, base, credential); err != nil {
			fmt.Fprintf(os.Stderr, "自动标题未写入（会话 %s）：%v\n", sessionID, err)
		}
	}()
}

// Run 是同步入口（测试与 Kick 共用）。返回 (标题, nil) 表示写成功；
// ("", nil) 表示"这一格按规则不生成"（已有标题 / 不是首轮收口 / 并发中）；
// ("", err) 表示真实失败，调用方只记日志、不重试。
func (s *autoTitleService) Run(ctx context.Context, sessionID, model, base, credential string) (string, error) {
	if s == nil || s.home == "" {
		return "", errors.New("session-title: 服务未接线")
	}
	if strings.TrimSpace(sessionID) == "" {
		return "", errors.New("session-title: 会话 id 为空")
	}
	if s.isClosed(sessionID) {
		return "", nil
	}
	path, err := newSessionSource(s.home, "").resolve(sessionID)
	if err != nil {
		return "", fmt.Errorf("session-title: 解析会话 %s: %w", sessionID, err)
	}
	if autoTitleIndexPresent(path) {
		s.markClosed(sessionID)
		return "", nil
	}
	if !s.begin(sessionID) {
		return "", nil
	}
	defer s.end(sessionID)

	// 一次调用一个总时限：Kick 与测试的同步调用共用同一道门，超时就"这一格不写标题"。
	// 调用方 ctx 更早到期时以它为准（WithTimeout 取更早的 deadline）。
	if timeout := s.timeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	log, err := v4.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("session-title: 读会话 %s: %w", sessionID, err)
	}
	first, ok := autoTitlePrompt(log)
	if !ok {
		s.markClosed(sessionID)
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// 输入上界：超了按码点截断，而不是整次放弃——官方在超限时直接拒绝，
	// 那会让"首条消息是一大段粘贴"的会话永远没有标题。
	input := autoTitleTruncateUTF8(autoTitleInput([]autoTitleMessage{first}), autoTitleMaxInputBytes)
	title, err := autoTitleCall{model: model, base: base, credential: credential, input: input}.generate(ctx)
	if err != nil {
		return "", err
	}
	written, err := s.commit(sessionID, path, title)
	if err != nil {
		return "", err
	}
	if !written {
		// 提交点发现用户已经写了标题（或不再满足触发条件）：按"跳过"返回，
		// **不能**把生成结果报成成功——调用方会以为它落了盘。
		return "", nil
	}
	return title, nil
}

// commit 在会话写锁内做"重读 → 判定 → intent → v4 → 索引 → 发布"。
//
// 为什么必须在写锁里重读：用户 `session.rename` 走的是同一把 `session.Log` 写锁
// （`sessionMetadataService.mutate` 先 AppendSessionTitle 再提交 journal）。
// 于是两种顺序都只有一种结局——用户标题先落盘，我重读时看到它就放弃；
// 用户标题后到，它无条件追加并成为最新值（auto 永远不覆盖 user）。
func (s *autoTitleService) commit(sessionID, sessionPath, title string) (written bool, retErr error) {
	logw, err := session.OpenSessionAt(sessionPath)
	if err != nil {
		return false, fmt.Errorf("session-title: 打开会话 %s 的写入: %w", sessionID, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = logw.Close()
		}
	}()
	current, err := v4.ReadFile(sessionPath)
	if err != nil {
		return false, fmt.Errorf("session-title: 重读会话 %s: %w", sessionID, err)
	}
	if _, ok := autoTitlePrompt(current); !ok {
		// 拿锁这一瞬间用户已经写了标题（或这一轮不再满足条件）：静默放弃。
		s.markClosed(sessionID)
		return false, nil
	}
	if autoTitleIndexPresent(sessionPath) {
		s.markClosed(sessionID)
		return false, nil
	}

	data := map[string]any{
		"title":       title,
		"messageSeqs": []int64{},
		"source":      map[string]any{"kind": "auto"},
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return false, fmt.Errorf("session-title: 序列化标题: %w", err)
	}
	seq := logw.Seq()
	transaction := autoTitleTransaction{Version: 1, EventSeq: seq, Type: "session/title", Data: raw}
	transactionPath := autoTitleTransactionPath(sessionPath)
	if err := writeAutoTitleIndex(transactionPath, transaction); err != nil {
		return false, fmt.Errorf("session-title: 写 intent 失败（v4 尚未追加）: %w", err)
	}
	if err := logw.Append("session/title", json.RawMessage(raw)); err != nil {
		// intent 从未对应过 v4 事件，留着只会让下一次 mutation 多恢复一次无用记录。
		_ = os.Remove(transactionPath)
		return false, fmt.Errorf("session-title: 写 session/title 失败: %w", err)
	}
	indexSeq, indexTitle := seq, title
	index := autoTitleIndex{Version: 1, EventSeq: &indexSeq, Title: &indexTitle, Source: "auto"}
	if err := writeAutoTitleIndex(autoTitleIndexPath(sessionPath), index); err != nil {
		return false, fmt.Errorf("session-title: 提交标题索引失败（intent 已保留，可从 v4 恢复）: %w", err)
	}
	if err := logw.Close(); err != nil {
		closed = true
		return false, fmt.Errorf("session-title: 落盘会话 %s 的标题: %w", sessionID, err)
	}
	closed = true

	// 耐久事实成立后才发帧：客户端看到的标题永远等于重启后读到的标题。
	// 没有 journal 就没有合法 seq 域，宁可不推（session.list 仍会回读）。
	if s.journals != nil {
		emitFrameAt(s.journals, s.pub, sessionID, "session/title", raw, time.Now().UnixMilli())
	}
	return true, nil
}

// ---------- 与默认 runner 的唯一注册点 ----------

// autoTitleService 懒建并缓存到 runner 上：cmdServe 那侧的构造点不在本任务
// 写作用域内，而服务本身只是"home + journal 出口 + 两个小缓存"，不需要外部状态。
func (r *modelOverrideLoopRunner) autoTitleService() *autoTitleService {
	if r == nil || r.base == nil || r.base.home == "" {
		return nil
	}
	r.titlesMu.Lock()
	defer r.titlesMu.Unlock()
	if r.titles == nil {
		r.titles = newAutoTitleService(r.base.home, r.base.journals, r.base.pub)
	}
	return r.titles
}

// autoTitleRoute 与这一轮同一条路由：会话走 provider 覆盖时，标题调用也必须
// 打到同一个网关/凭据；否则会出现"回答在 A 网关、标题打到默认地址"的错位。
func autoTitleRoute(provider llm.ModelProviderConfig, routed bool) (base, credential string) {
	if routed {
		return provider.BaseURL, provider.Credential()
	}
	return envOr("DSHD_LLM_BASE", "http://127.0.0.1:3425/v1"), os.Getenv("DSHD_LLM_KEY")
}

// kickAutoTitle 是 Run/RunContent 唯一的收尾钩子：只有**这一轮正常收工**才触发，
// 且在后台跑。错误只进宿主 stderr，绝不改写这一轮的结局。
func (r *modelOverrideLoopRunner) kickAutoTitle(sessionID, model string, provider llm.ModelProviderConfig, routed bool) {
	service := r.autoTitleService()
	if service == nil {
		return
	}
	base, credential := autoTitleRoute(provider, routed)
	service.Kick(sessionID, model, base, credential)
}
