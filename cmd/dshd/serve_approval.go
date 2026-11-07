package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"dshgo/internal/transport"
)

// approvalBroker 是审批的**父进程半边**（H13）。
//
// 链路：子进程 stderr 上的一行 `@@APPROVAL@@ {json}`
//
//	→ 这里解析并发 `approval/request`（进 journal，可重放）
//	→ 界面 POST session.approve
//	→ Resolve 把决定写回子进程 stdin 的一行 `@@APPROVAL-RESP@@ {json}`
//
// 为什么不做成"父进程直接执行工具"：工具跑在子进程里，父进程没有那次调用的上下文。
// 父进程只当**传话的**，判定仍然发生在真正执行的地方。
type approvalBroker struct {
	mu sync.Mutex
	// pending 是"已经问出去、还没人回答"的请求（requestId → 那条请求）。
	//
	// 负载、应答通道、取消信号装在**同一张表**里：三张表各自增删时，迟早出现
	// "应答通道删了、负载还留着"这种半死状态，而它会被重放成一条永远答不了的问题——
	// 界面上看着像卡住了，宿主这边却已经没有等待者。
	pending map[string]*pendingAsk
	stdin   io.WriteCloser
	// writeMu 串行化对子进程 stdin 的写。
	//
	// 为什么必须有：审批可以有**两个**来源同时结算——客户端点了允许、
	// 或超时那条 goroutine 到期。两边都会调 reply 往同一根 stdin 写一行 JSON，
	// 不串行就会写出交错的半行，子进程解析失败，而现象是"审批偶尔没生效"。
	writeMu sync.Mutex
	emit    func(sessionID, typ string, data json.RawMessage)
	// jobsOut 是"父进程问任务输出"的等待表：请求 id → 应答 channel。
	//
	// 和审批的 pending 同构但不共用：审批是**子进程问、父进程答**（数据是 bool），
	// 这里是**父进程问、子进程答**（数据是一段输出）。方向相反，混在一起会乱。
	jobsOutPending map[string]chan string
	// onJobs 收到子进程推上来的后台任务清单（`@@JOBS@@` 行）。
	// nil = 这个宿主不关心（老测试）。
	onJobs func(jobs []transport.JobInfo)
	// onJobsForSession 保留原全局 last-snapshot list 语义，同时给每行补真实归属。
	onJobsForSession func(sessionID string, jobs []transport.JobInfo)
	actions          *jobActionBroker

	// timeout 到了没人答 → 按**拒绝**结算。没人回答时"默认同意"是最坏的选择。
	timeout time.Duration

	// allowed 记「这个会话里这个工具以后都允许」。
	//
	// 边界说清楚：**只在内存里、只在这个 serve 进程内有效**，进程退出即失效——
	// 和 --policy 的生命周期一致。不落盘是刻意的：一次"以后都允许"若跨进程生效，
	// 用户换了会话、换了项目还会被静默放行，那不是他能预期的。
	allowed map[string]map[string]bool
}

// pendingPayload 是一条挂起审批的原始请求。
type pendingPayload struct {
	sessionID string
	tool      string
	data      json.RawMessage
}

// pendingAsk 是一条**还挂着、等回答**的审批。
type pendingAsk struct {
	payload pendingPayload
	// ch 收决定：true = 允许。带 1 格缓冲，所以结算方永远不会因为
	// 等待者还没跑到 select 而卡住。
	ch chan bool
	// stop 在宿主**主动取消**这条请求时关闭，让超时那条 goroutine 立刻收工。
	//
	// 不关的后果不是"多一条日志"：每轮都会起一个**新子进程**，它的 requestId
	// 又从 perm-1 开始编，于是取消后第 120 秒，那行 `allow:false` 会写进
	// 下一个子进程的 stdin，命中它这一轮某个真实请求——用户看到的是
	// "我明明没答，它自己拒了"。
	stop chan struct{}
}

func newApprovalBroker(stdin io.WriteCloser, emit func(sessionID, typ string, data json.RawMessage)) *approvalBroker {
	return &approvalBroker{
		pending: map[string]*pendingAsk{},
		allowed: map[string]map[string]bool{},
		stdin:   stdin,
		emit:    emit,
		timeout: approvalTimeout(),
	}
}

// approvalTimeout 是"没人回答就按拒绝算"的等待时长。
//
// 默认 120s；`DSHD_APPROVAL_TIMEOUT_SEC` 可以覆盖。
// 为什么要可配：**超时这条路径默认永远跑不到**（两分钟没人答才算数），
// 如果没有调小的办法，它就只能是"看起来实现了"。
func approvalTimeout() time.Duration {
	if v := os.Getenv("DSHD_APPROVAL_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 120 * time.Second
}

// useStdin 换成本轮子进程的 stdin（每轮一个子进程，管道是新的）。
func (b *approvalBroker) useStdin(w io.WriteCloser) {
	b.mu.Lock()
	b.stdin = w
	b.mu.Unlock()
}

// setEmit 挂上推送出口（由 serve 在启动时注入，闭包捕获 loopRunner）。
func (b *approvalBroker) setEmit(fn func(sessionID, typ string, data json.RawMessage)) {
	b.mu.Lock()
	b.emit = fn
	b.mu.Unlock()
}

// Watch 逐行扫子进程的 stderr，遇到审批请求就登记并推给界面。
//
// **必须逐行**：stderr 之前是 bytes.Buffer（跑完才读），那样审批要等到轮次结束——
// 而审批的意义就是"跑到一半停下来问"。这里是唯一的改动理由。
func (b *approvalBroker) Watch(r io.Reader, alsoTo io.Writer) {
	b.WatchSession("", r, alsoTo)
}

func (b *approvalBroker) WatchSession(sessionID string, r io.Reader, alsoTo io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		// 原样留给运维日志：审批行也要能在 serve 的 stderr 里看到，
		// 不然出了事分不清"没问"和"问了没人答"。
		if alsoTo != nil {
			fmt.Fprintln(alsoTo, line)
		}
		// 后台任务清单也走这条 stderr 通道（见 main.go 的 emitJobs）。
		// **先认它再认审批**：两个标记前缀不同，顺序不影响正确性，
		// 但把新的放前面，读代码的人一眼看到"这条通道不止审批一件事"。
		if b.actions != nil && b.actions.deliver(line) {
			continue
		}
		if respID, payload, ok := parseJobsOutResp(line); ok {
			b.deliverJobsOut(respID, payload)
			continue
		}
		if jobs, ok := parseJobsLine(line); ok {
			if b.onJobsForSession != nil {
				b.onJobsForSession(sessionID, jobs)
			} else if b.onJobs != nil {
				b.onJobs(jobs)
			}
			continue
		}
		req, ok := parseApprovalLine(line)
		if !ok {
			continue
		}
		b.handle(req)
	}
}

type approvalReq struct {
	RequestID string          `json:"requestId"`
	SessionID string          `json:"sessionId"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
}

// parseJobsLine 解析 `@@JOBS@@ [{...}]`。
func parseJobsLine(line string) ([]transport.JobInfo, bool) {
	const tag = "@@JOBS@@ "
	if !strings.HasPrefix(line, tag) {
		return nil, false
	}
	var jobs []transport.JobInfo
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, tag)), &jobs); err != nil {
		return nil, false // 坏行当没看见：拒绝比"半个清单"好
	}
	return jobs, true
}

// parseJobsOutResp 解析 `@@JOBS-OUT-RESP@@ {"id":"job-1",...}`。
//
// 返回 (请求 id, 原始负载, 是否认得)。
func parseJobsOutResp(line string) (string, string, bool) {
	const tag = "@@JOBS-OUT-RESP@@ "
	if !strings.HasPrefix(line, tag) {
		return "", "", false
	}
	payload := strings.TrimPrefix(line, tag)
	var probe struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(payload), &probe) != nil || probe.ID == "" {
		return "", "", false // 坏行当没看见
	}
	return probe.ID, payload, true
}

// deliverJobsOut 把应答投给正在等它的那个 jobs.output 调用。
//
// **channel 缓冲 1，投递不阻塞**：没人等（超时走了）就丢掉——
// 阻塞在这里会让整个 stderr 分发停住，连带审批也收不到。
func (b *approvalBroker) deliverJobsOut(id, payload string) {
	b.mu.Lock()
	ch := b.jobsOutPending[id]
	delete(b.jobsOutPending, id)
	b.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- payload:
	default:
	}
}

// askJobsOut 发一条"查任务输出"的请求，等子进程回一条应答。
func (b *approvalBroker) askJobsOut(ctx context.Context, id string) (string, error) {
	b.mu.Lock()
	if b.stdin == nil {
		b.mu.Unlock()
		return "", fmt.Errorf("这条会话的子进程还没起来（没有 stdin）")
	}
	if b.jobsOutPending == nil {
		b.jobsOutPending = map[string]chan string{}
	}
	ch := make(chan string, 1)
	b.jobsOutPending[id] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.jobsOutPending, id)
		b.mu.Unlock()
	}()

	// **请求 id 带上任务 id**：子进程按它回，父进程按它找等待者。
	// 同一个任务并发查两次的话第二次会覆盖第一次的等待者——
	// 那种情况下第二次的结果更晚但更新，覆盖是可接受的（不比"给出旧输出"差）。
	fmt.Fprintf(b.stdin, "%s{\"id\":%q}\n", jobsOutReqTag, id)

	select {
	case payload := <-ch:
		return payload, nil
	case <-ctx.Done():
		return "", fmt.Errorf("等子进程回任务输出超时（%v）", ctx.Err())
	}
}

func parseApprovalLine(line string) (approvalReq, bool) {
	if !strings.HasPrefix(line, approvalRequestTag) {
		return approvalReq{}, false
	}
	var r approvalReq
	if json.Unmarshal([]byte(strings.TrimPrefix(line, approvalRequestTag)), &r) != nil {
		return approvalReq{}, false
	}
	if r.RequestID == "" {
		return approvalReq{}, false
	}
	return r, true
}

func (b *approvalBroker) handle(req approvalReq) {
	// 用户之前在这个会话里对**同一个工具**点过"以后都允许"：不再问，直接放行。
	//
	// 不问就不发 approval/request——界面不会闪出一个已经不需要回答的气泡。
	// 但要在 stderr 留一行：自动化放行必须能在日志里查得到，否则事后没人能解释
	// "这次为什么没问我"。
	if b.alwaysAllowed(req.SessionID, req.Tool) {
		fmt.Fprintf(os.Stderr, "审批：会话 %s 的 %s 早先已选择「以后都允许」，本轮自动放行\n",
			req.SessionID, req.Tool)
		b.reply(req.RequestID, true)
		return
	}

	ch := make(chan bool, 1)
	stop := make(chan struct{})

	// 形状与 ACP 的 session/request_permission 对齐（docs/h13-approval.md §契约）：
	// 界面两边显示同一套东西，用户在不同客户端看到的措辞才不会打架。
	payload, _ := json.Marshal(map[string]any{
		"requestId": req.RequestID,
		"sessionId": req.SessionID,
		// 超时**必须报给界面**，否则界面只能显示"等你批准"：
		// 人离开一会儿回来，命令已经被拒了，屏幕上却什么都没变——
		// 分不清"还没人答"和"早就超时了"。
		//
		// 两个字段都发，缺一不可：
		//
		//	timeoutMs  —— 总等待时长（就是下面 time.After 用的那个数，不是另编的）
		//	deadlineMs —— 绝对截止时刻（Unix 毫秒）
		//
		// 为什么要绝对时刻：断线重连时重放的是**当初这条负载**（见 pending）；
		// 只发时长的旧负载会被客户端当成"从现在起再等一整轮"，屏幕上说还有 90s，
		// 宿主 3s 后就拒了——那比不显示倒计时更坏。
		// 工具名单独给一个字段：`allow_always` 的粒度就是（会话 + 工具名），
		// 而 toolCall 里只有 title/kind/rawInput——**客户端看不到自己正在记住哪个工具**。
		// 只加不改：老客户端忽略多出来的键。
		"tool":       req.Tool,
		"timeoutMs":  b.timeout.Milliseconds(),
		"deadlineMs": time.Now().Add(b.timeout).UnixMilli(),
		"toolCall": map[string]any{
			"toolCallId": req.RequestID,
			"title":      approvalTitle(req.Tool, string(req.Args)),
			"kind":       approvalKind(req.Tool),
			"status":     "pending",
			"rawInput":   req.Args,
		},
		"options": []map[string]any{
			{"optionId": "allow_once", "name": "允许这一次", "kind": "allow_once"},
			{"optionId": "allow_always", "name": "这个会话里以后都允许", "kind": "allow_always"},
			{"optionId": "reject_once", "name": "拒绝", "kind": "reject_once"},
		},
	})

	// 登记必须在 emit **之前**：反过来的话，客户端秒答（或这一刻正好 cancel）
	// 会落在一个还没登记的 requestId 上，得到 404，而界面已经显示"已批准/已取消"。
	b.mu.Lock()
	if b.pending == nil { // 直接构造的 broker（老测试）也要能用
		b.pending = map[string]*pendingAsk{}
	}
	b.pending[req.RequestID] = &pendingAsk{
		payload: pendingPayload{sessionID: req.SessionID, tool: req.Tool, data: payload},
		ch:      ch,
		stop:    stop,
	}
	b.mu.Unlock()

	if b.emit != nil {
		b.emit(req.SessionID, "approval/request", payload)
	}

	// 超时按拒绝结算，并让界面知道"是超时，不是有人点了拒绝"。
	go func() {
		select {
		case allow := <-ch:
			b.reply(req.RequestID, allow)
		case <-stop:
			// 已经被 cancel 结算过了：这里**什么都不做**。
			// 再推一条超时、再写一行应答，都是在编造另一件事。
		case <-time.After(b.timeout):
			// 只有还能**认领**到这条请求的人才有权结算：cancel 已经把记录摘掉时，
			// 这里必须闭嘴——否则界面上先显示"已取消"，两分钟后又冒出一条"已超时"。
			if _, ok := b.claim(req.RequestID); !ok {
				return
			}
			if b.emit != nil {
				b.emit(req.SessionID, "approval/timeout", payload)
			}
			b.reply(req.RequestID, false)
		}
	}()
}

// Pending 返回这个会话上**还没被回答**的审批请求（原始负载）。
//
// 为什么需要重放：客户端可能断开重连、甚至整进程重启，那些气泡就没了；
// 而请求在宿主这边仍然挂着——子进程还在等。不重放的话，用户永远看不到那个问题，
// 子进程就一直等到超时被拒，看起来像"卡住了"。
func (b *approvalBroker) Pending(sessionID string) []json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]json.RawMessage, 0, len(b.pending))
	for _, ask := range b.pending {
		if ask.payload.sessionID == sessionID {
			out = append(out, ask.payload.data)
		}
	}
	return out
}

// PendingFrames 是 Pending 的帧形状版本，直接给 transport 用。
//
// 重放的帧**不带 seq**：它不是事件流里新的一帧（历史里已经有那条 approval/request），
// 只是把"仍然有效"的状态再摆一次；带号会让客户端以为又多了一条事件。
func (b *approvalBroker) PendingFrames(sessionID string) []transport.ServerFrame {
	raw := b.Pending(sessionID)
	out := make([]transport.ServerFrame, 0, len(raw))
	for _, d := range raw {
		out = append(out, transport.ServerFrame{
			Type: "approval/request", Data: d, Time: time.Now().UnixMilli(),
		})
	}
	return out
}

// Resolve 是 session.approve 的落点：把界面的决定交给等待中的那一轮。
func (b *approvalBroker) Resolve(requestID, optionID string) error {
	allow, ok := decisionOf(optionID)
	if !ok {
		return transport.ErrBadOption
	}
	ask, found := b.claim(requestID)
	if !found {
		// 已经答过、已经超时、被取消、或者是编的——四种都当作"这个请求不在了"。
		return transport.ErrNoSuchRequest
	}
	// "以后都允许"要真的记住：否则界面给了这个选项、用户点了，
	// 下次同一个工具还被问一遍——**选项在、语义空**比没这个选项更糟。
	if optionID == "allow_always" {
		b.markAlways(ask.payload.sessionID, ask.payload.tool)
	}
	ask.ch <- allow
	return nil
}

// claim 把一条还挂着的问题认领下来（一次性：认领过的就从表里没了）。
//
// 三条结算路径（界面回答、超时到期、cancel）都必须**先认领再动手**：
// 认领不到说明别人已经结算过，这时候什么都不做才是对的——否则界面上会
// 先看到"已取消"，两分钟后又冒出一条"已超时"。
func (b *approvalBroker) claim(id string) (*pendingAsk, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ask, ok := b.pending[id]
	if !ok {
		return nil, false
	}
	delete(b.pending, id)
	return ask, true
}

// alwaysAllowed 查「这个会话里这个工具已经选了以后都允许」。
func (b *approvalBroker) alwaysAllowed(sessionID, tool string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.allowed[sessionID]
	return ok && m[tool]
}

// markAlways 记下「以后都允许」。粒度是**会话 + 工具名**，不是工具 + 参数：
// 一次"以后都允许 bash"就代表用户接受这个会话里所有 bash，
// 细化到参数会变成"每次都问一个新的"，那正是用户点这个选项想避免的。
func (b *approvalBroker) markAlways(sessionID, tool string) {
	if sessionID == "" || tool == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.allowed[sessionID] == nil {
		b.allowed[sessionID] = map[string]bool{}
	}
	b.allowed[sessionID][tool] = true
}

// CancelSession 把这条会话上**所有还挂着**的审批按拒绝结算，并推 `approval/cancel`。
//
// 为什么 cancel 必须走到这里：`session.cancel` 只取消轮次的话，子进程被杀掉，
// 而那条请求在宿主表里还是"等你批准"——界面上留着一个永远答不上的气泡，
// journal 里没有结局帧，客户端重连还会把它当"还挂着"重放出来。
//
// 顺序是"先拒绝、后推帧"：拒绝写回子进程，工具**一定**不会执行；
// 哪怕子进程这一拍还没来得及死，它拿到的也是明确的拒绝。
//
// 返回结算了几条（0 = 这条会话上没有挂着的审批）。
func (b *approvalBroker) CancelSession(sessionID string) int {
	b.mu.Lock()
	var ids []string
	for id, ask := range b.pending {
		if ask.payload.sessionID == sessionID {
			ids = append(ids, id)
		}
	}
	victims := make([]*pendingAsk, 0, len(ids))
	for _, id := range ids {
		victims = append(victims, b.pending[id])
		delete(b.pending, id)
	}
	b.mu.Unlock()

	for i, ask := range victims {
		// 先让超时那条 goroutine 收工：它认领会失败，所以不会重复结算。
		close(ask.stop)
		b.reply(ids[i], false)
		if b.emit != nil {
			// 负载与 approval/request **同形**：客户端靠 requestId 收掉那个气泡，
			// 不需要第二条解码路径（approval/timeout 也是这么发的）。
			b.emit(sessionID, "approval/cancel", ask.payload.data)
		}
	}
	return len(victims)
}

// cancelTurn 是 `session.cancel` 的落点：**先**把挂着的审批按拒绝结算，再取消这一轮。
//
// 顺序不能反：先取消轮次会立刻杀掉子进程，那条审批就成了"永远等不到回答"的僵尸；
// 反过来先拒绝，子进程拿到的是明确的拒绝（工具不执行），然后再中止这一轮。
func cancelTurn(b *approvalBroker, p *promptService, sessionID string) error {
	rejected := 0
	if b != nil {
		rejected = b.CancelSession(sessionID)
	}
	err := p.Cancel(sessionID)
	if err != nil && rejected > 0 {
		// 审批确实被拒掉了（工具不会执行），只是这一轮在这一拍已经自己收工了。
		// 这种"差一拍"不该报成 409：界面会以为取消失败、再去点一次。
		return nil
	}
	return err
}

// reply 把决定写回子进程。写失败不影响判定本身（子进程读不到会按拒绝处理）。
func (b *approvalBroker) reply(requestID string, allow bool) {
	if b.stdin == nil {
		return
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	line, _ := json.Marshal(map[string]any{"requestId": requestID, "allow": allow})
	fmt.Fprintf(b.stdin, "%s%s\n", approvalResponseTag, line)
}

// decisionOf 把 optionId 翻成 true/false；不认识的返回 false,false。
//
// "不认识就当拒绝"在这里不成立：得让调用方明确报 400，
// 否则界面传了个错拼的 optionId，用户会以为"我点了允许但没生效"。
func decisionOf(optionID string) (allow bool, ok bool) {
	switch optionID {
	case "allow_once", "allow_always":
		return true, true
	case "reject_once":
		return false, true
	}
	return false, false
}
