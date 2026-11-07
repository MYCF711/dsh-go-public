// Package team 提供会话之间的最小通信闭环：留话、等话、看有谁用过信箱。
//
// 存储故意做成纯文本：一个会话一个 .jsonl、一个 .cursor 整数。
// 不用数据库的理由是——出事时人能直接打开文件看懂并修好，
// 而"两个 agent 互相等不到"这种问题，恰恰是最需要人肉排查的。
package team

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

const (
	kindSend = "send_message"
	kindWait = "wait_agent"
	kindList = "list_agents"

	defaultWaitSeconds = 60
	maxWaitSeconds     = 300
	// 轮询间隔：500ms 足够让"对话感"不塌，又不至于把磁盘读爆。
	pollEvery = 500 * time.Millisecond
	// 一次最多吐多少条，剩下的靠 cursor 留到下次——输出必须有界。
	maxMessagesPerWait = 50
	maxTextBytes       = 32 * 1024
)

// Tool 的三个形态共用一个 struct，靠 kind 分派；这样 Module() 只需注册三次。
type Tool struct {
	box  *box
	kind string
}

func (t *Tool) Name() string { return t.kind }

// Description 只讲"什么时候用"，不讲实现；<=120 字由 desc-len.py 把关。
func (t *Tool) Description() string {
	switch t.kind {
	case kindSend:
		return "给另一个会话留一条消息，等它用 wait_agent 取走；要叫醒别人时用。"
	case kindWait:
		return "取自己信箱里的新消息，有就立刻返回，超时返回空列表；接手别人的活之前用。"
	default:
		return "列出用过信箱的会话和各自消息数，看谁在等你，或核对会话 id 有没有写错。"
	}
}

func (t *Tool) Schema() json.RawMessage {
	switch t.kind {
	case kindSend:
		return json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"目标会话 id，例如 acp-aaa"},"target":{"type":"string","description":"同 agent_id，DSH 的写法"},"message":{"type":"string","description":"消息正文"}},"required":["message"],"additionalProperties":false}`)
	case kindWait:
		return json.RawMessage(`{"type":"object","properties":{"timeout_seconds":{"type":"integer","description":"最多等多少秒，默认 60，上限 300"}},"additionalProperties":false}`)
	default:
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	}
}

// Caps：只有 send_message 会写盘；wait/list 只读文件系统。
func (t *Tool) Caps() spi.Caps {
	if t.kind == kindSend {
		return spi.Caps{Writes: true}
	}
	return spi.Caps{}
}

type sendArgs struct {
	To string `json:"agent_id"`
	// Target 是 DSH 的另一种写法（对齐测试报「DSH 要求但我们不认 target」）。
	// 两个都收：模型按 DSH 的 schema 写 target 时不该失败。
	Target string `json:"target"`
	Text   string `json:"message"`
}

type waitArgs struct {
	TimeoutSeconds int `json:"timeout_seconds"`
}

// message 是信箱里的一行。字段名就是落盘格式，别随便改——旧文件还躺在盘上。
type message struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Text string `json:"message"`
	At   string `json:"at"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	switch t.kind {
	case kindSend:
		return t.send(ctx, raw)
	case kindWait:
		return t.wait(ctx, raw)
	case kindList:
		return t.list(ctx, raw)
	}
	// 走到这里说明接线把 kind 写错了，不是模型的锅，所以 Hint 指向人。
	return spi.Result{Hint: "未知的工具分支，这是接线错误：别重复调用，告诉维护者检查 Module() 的 kind"},
		fmt.Errorf("team: 未知 kind %q", t.kind)
}

// ---- send_message ----

func (t *Tool) send(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a sendArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"agent_id":"<会话 id>","message":"<正文>"}`}, err
	}
	self := spi.SessionFrom(ctx)
	// target 是 DSH schema 里的名字；agent_id 是我们的。两者都给时以 agent_id 为准。
	if a.To == "" {
		a.To = a.Target
	}
	if self == "" {
		return spi.Result{Hint: "这个工具要在会话里跑：发件人靠当前会话 id 署名，没有会话就没法发"}, errors.New("team: 缺少会话上下文")
	}
	to := strings.TrimSpace(a.To)
	if to == "" {
		return spi.Result{Hint: `to 不能为空：填目标会话 id；不知道 id 就先调 list_agents 查`}, errors.New("team: to 为空")
	}
	if to == self {
		return spi.Result{Hint: "to 就是自己：你在给自己写信，没人会来收。换成对方会话 id，或直接往下做"}, errors.New("team: to 等于 self")
	}
	if err := checkID(to); err != nil {
		return spi.Result{Hint: "to 里有非法字符（路径分隔符或 ..）：会话 id 形如 acp-aaa，用 list_agents 看真名"}, err
	}
	if strings.TrimSpace(a.Text) == "" {
		return spi.Result{Hint: "text 不能为空：把要对方做的事一句话写清，别只发个招呼"}, errors.New("team: text 为空")
	}
	if len(a.Text) > maxTextBytes {
		return spi.Result{Hint: fmt.Sprintf("正文超过 %d 字节：拆成两条发，或把长内容写成文件再在消息里给路径", maxTextBytes)},
			fmt.Errorf("team: 正文过大（%d 字节）", len(a.Text))
	}

	if err := t.box.ensure(); err != nil {
		return spi.Result{Hint: "信箱目录建不起来：检查该路径的权限和磁盘空间，别当作已发出"}, err
	}
	path := t.box.mailPath(to)

	// 编号沿用目标文件已有的行数：一个会话一个文件，顺序天然递增。
	count, err := countLines(path)
	if err != nil {
		return spi.Result{Hint: "读不了对方的信箱文件：它可能被别的进程占用或权限不对，稍后重试一次"}, err
	}
	msg := message{
		ID:   fmt.Sprintf("msg-%04d", count+1),
		From: self,
		Text: a.Text,
		At:   time.Now().Format(time.RFC3339),
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return spi.Result{Hint: "消息序列化失败，这是内部错误：别重试，告诉维护者"}, err
	}
	// O_APPEND + 单次 Write：两个会话同时发也不会把彼此的半行插在一起。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return spi.Result{Hint: "打开信箱失败：检查目录权限，确认 <home>/state/mail 可写后重试"}, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return spi.Result{Hint: "写入没成功，消息可能只写了一半：重试前先用 list_agents 看条数确认"}, err
	}

	return spi.Result{
		Summary: fmt.Sprintf("已发给 %s（%s）", to, msg.ID),
		Data:    map[string]any{"id": msg.ID, "agent_id": to, "at": msg.At},
		Text:    fmt.Sprintf("已投递：%s → %s。对方下次调 wait_agent 就能取到；它不会主动醒，别指望立刻有回应。", msg.ID, to),
	}, nil
}

// ---- wait_agent ----

func (t *Tool) wait(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a waitArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"timeout_seconds": 60}；不想调就不传`}, err
	}
	self := spi.SessionFrom(ctx)
	if self == "" {
		return spi.Result{Hint: "这个工具要在会话里跑：信箱按当前会话 id 找，没有会话就无箱可取"}, errors.New("team: 缺少会话上下文")
	}
	secs := a.TimeoutSeconds
	if secs <= 0 {
		secs = defaultWaitSeconds
	}
	if secs > maxWaitSeconds {
		secs = maxWaitSeconds
	}
	deadline := time.Now().Add(time.Duration(secs) * time.Second)

	for {
		msgs, total, err := t.box.readMessages(self)
		if err != nil {
			return spi.Result{Hint: "读信箱失败：检查该文件是否被删或权限变了，别当作没有消息"}, err
		}
		cursor := t.box.readCursor(self)
		if cursor > total {
			// 文件被人工改小过：游标越界就当从头再读，总比永远收不到强。
			cursor = total
		}
		if total > cursor {
			take := total - cursor
			if take > maxMessagesPerWait {
				take = maxMessagesPerWait
			}
			got := msgs[cursor : cursor+take]
			next := cursor + take
			if err := t.box.writeCursor(self, next); err != nil {
				return spi.Result{Hint: "游标写不下去：这批消息会重复取到，重试一次；仍失败就修目录权限"}, err
			}
			rest := total - next
			hint := ""
			if rest > 0 {
				hint = fmt.Sprintf("还剩 %d 条没取：再调一次 wait_agent（timeout_seconds 给 1）继续拿", rest)
			}
			return spi.Result{
				Summary: fmt.Sprintf("收到 %d 条新消息", len(got)),
				Data: map[string]any{
					"session_id": self,
					"count":      len(got),
					"messages":   got,
					"cursor":     next,
					"remaining":  rest,
				},
				Text: renderMessages(got),
				Hint: hint,
			}, nil
		}

		select {
		case <-ctx.Done():
			return spi.Result{Hint: "等待被取消（本轮结束或超时）：没有取到新消息，别据此判断对方没回"}, ctx.Err()
		case <-time.After(pollEvery):
		}
		if time.Now().After(deadline) {
			// 超时不是错误：它就是"暂时没人找你"这个事实。
			return spi.Result{
				Summary: fmt.Sprintf("等了 %d 秒，没有新消息", secs),
				Data: map[string]any{
					"session_id": self,
					"count":      0,
					"messages":   []message{},
					"timeout":    true,
				},
				Text: "超时，没有新消息。",
				Hint: "先干别的、过一会儿再来等；想让对方回话就用 send_message 明确要它回一条",
			}, nil
		}
	}
}

// ---- list_agents ----

func (t *Tool) list(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	entries, err := os.ReadDir(t.box.dir)
	if err != nil && !os.IsNotExist(err) {
		return spi.Result{Hint: "读不了信箱目录：检查 <home>/state/mail 的权限，别当作没有会话"}, err
	}

	type agent struct {
		SessionID    string `json:"session_id"`
		Messages     int    `json:"messages"`
		LastModified string `json:"last_modified"`
		Mailbox      string `json:"mailbox"`
	}
	agents := []agent{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(t.box.dir, e.Name())
		n, err := countLines(path)
		if err != nil {
			// 单个文件读坏了不该让整张表作废：条数记 -1，人一看就知道要修哪个。
			n = -1
		}
		mod := ""
		if info, err := e.Info(); err == nil {
			mod = info.ModTime().Format(time.RFC3339)
		}
		agents = append(agents, agent{
			SessionID:    strings.TrimSuffix(e.Name(), ".jsonl"),
			Messages:     n,
			LastModified: mod,
			Mailbox:      filepath.ToSlash(path),
		})
	}
	// 最近活跃的排前面：找"谁刚找过我"时不用自己扫表。
	sort.Slice(agents, func(i, j int) bool { return agents[i].LastModified > agents[j].LastModified })

	if len(agents) == 0 {
		return spi.Result{
			Summary: "信箱是空的",
			Data:    map[string]any{"agents": agents, "count": 0},
			Text:    "还没有任何会话用过信箱。",
			Hint:    "先 send_message 给对方会话 id 留一条，之后这里才会出现条目；id 不确定就问人",
		}, nil
	}

	var sb strings.Builder
	sb.WriteString("会话 id\t消息数\t最后修改\n")
	for _, a := range agents {
		sb.WriteString(fmt.Sprintf("%s\t%d\t%s\n", a.SessionID, a.Messages, a.LastModified))
	}
	return spi.Result{
		Summary: fmt.Sprintf("%d 个会话用过信箱", len(agents)),
		Data:    map[string]any{"agents": agents, "count": len(agents)},
		Text:    strings.TrimRight(sb.String(), "\n"),
	}, nil
}

// renderMessages 把消息拼成模型直接能读的正文；Text 非空时投影不再包 JSON。
func renderMessages(msgs []message) string {
	var sb strings.Builder
	for i, m := range msgs {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("[%s] %s @ %s\n%s\n", m.ID, m.From, m.At, m.Text))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ---- 存储 ----

type box struct{ dir string }

func (b *box) ensure() error {
	return os.MkdirAll(b.dir, 0o755)
}

func (b *box) mailPath(session string) string {
	return filepath.Join(b.dir, session+".jsonl")
}

func (b *box) cursorPath(session string) string {
	return filepath.Join(b.dir, session+".cursor")
}

// readMessages 读出整个信箱；文件不存在意味着"还没人给你写信"，不是错误。
func (b *box) readMessages(session string) ([]message, int, error) {
	f, err := os.Open(b.mailPath(session))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var out []message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			// 坏行也占一个序号：跳过它但要推进游标，否则会永远卡在同一行。
			m = message{ID: "msg-bad", From: "?", Text: line}
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	return out, len(out), nil
}

func (b *box) readCursor(session string) int {
	raw, err := os.ReadFile(b.cursorPath(session))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 0 {
		// 游标坏了就当没读过：宁可重复几条，也别静默丢消息。
		return 0
	}
	return n
}

func (b *box) writeCursor(session string, n int) error {
	if err := b.ensure(); err != nil {
		return err
	}
	return os.WriteFile(b.cursorPath(session), []byte(strconv.Itoa(n)+"\n"), 0o644)
}

// countLines 数一个信箱文件里的消息条数；文件不存在算 0 条。
func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// checkID 挡住能跳出信箱目录的会话 id：拼接路径前必须先过这一关。
func checkID(id string) error {
	if id == "" || id == "." || id == ".." {
		return fmt.Errorf("team: 非法会话 id %q", id)
	}
	if strings.ContainsAny(id, `/\:*?"<>|`) || strings.ContainsAny(id, "\r\n\t") {
		return fmt.Errorf("team: 非法会话 id %q", id)
	}
	return nil
}

// Module 返回装配用的模块描述。dir 是信箱目录（接线时传 <home>/state/mail）。
func Module(dir string) kernel.Module {
	b := &box{dir: dir}
	return kernel.Module{
		ID:       "tool-team",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			for _, kind := range []string{kindSend, kindWait, kindList} {
				if err := reg.Register(&Tool{box: b, kind: kind}); err != nil {
					return err
				}
			}
			// spawn_teammate 写的是 <home>/state/team/agents.json，而 Module 的入参
			// 是信箱目录 <home>/state/mail。**不能直接把 dir 传下去**——
			// 踩过：队友名单落进了 state/mail/agents.json，和收件箱混在一起。
			agentsDir := filepath.Join(filepath.Dir(dir), "team")
			if err := reg.Register(&spawnTool{dir: agentsDir}); err != nil {
				return err
			}
			// 中断标记写的是同一份 agents.json，所以也用 agentsDir。
			return reg.Register(&interruptTool{dir: agentsDir})
		},
	}
}
