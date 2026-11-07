package main

// 官方 0.2.2-rc.1 的 job.* / terminal.* 在**宿主侧**的真实实现。
//
// # 复用的是什么（不是新造一套）
//
//   - job.*：既有 jobs 服务（`jobActionBroker`）——它已经有会话围栏（跨会话 403）、
//     实例/PID 映像校验、"已结束 409"、保留输出目录 `<home>/state/jobs/<sid>/<id>.json`，
//     以及子进程 IPC（本文一个字都不重写那套判断）。
//   - terminal.*：既有 `terminal.open/input/output` 背后的 `*bash.Service` 持久 shell 会话
//     （同一时刻一条命令、Windows Job Object / Unix 进程组归属、CloseTerminal 回收整棵树）。
//     本文件只是在它上面加"身份/标题/尺寸/保留"这层官方元数据。
//
// # 为什么不是 PTY
//
// 官方 terminal 是 PTY（有屏幕模型、cols/rows 真正生效、attach 有 snapshot.screen 渲染）。
// 本仓 `modules/exec/bash` 的 terminal 是**管道式持久 shell**（stdin/stdout 管道），
// 没有 PTY，因此：
//   - cols/rows 只作为会话元数据记录，不做真实窗口尺寸（没有 SIGWINCH）；
//   - attach 的 screen 是**原始输出尾部**，不是 ANSI 渲染后的屏幕；
//   - environment.scrollback 如实报 0（没有行滚动缓冲）。
// 这三条都登记在 RESULT 的未验/未实现清单里，不假装成 PTY。
//
// # 缺能力怎么办
//
// 返回 transport.ErrJTOfficialUnsupported（→ 501 not_implemented）+ 说清缺什么。
// 例如 `terminal.create` 指定了本宿主起不来的 shell（底层模块写死 bash 引导）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dshgo/internal/transport"
	"dshgo/modules/exec/bash"
)

const (
	// officialTerminalMaxInputBytes 是**本层 Input 强制**的单次写入上限。
	// 它同时也是 environment.maxInputBytes 的真实来源（不是编出来的数）。
	officialTerminalMaxInputBytes = 64 * 1024
	// officialTerminalMaxScreenBytes 是快照/保留文本的上限（有界输出）。
	officialTerminalMaxScreenBytes = 256 * 1024
	// officialTerminalMaxCols / MaxRows 是 create 接受的尺寸上限（超出即截断，不报错）。
	officialTerminalMaxCols = 500
	officialTerminalMaxRows = 200
	// officialFollowPageBytes 是 job.follow 单帧携带的字节上限。
	officialFollowPageBytes = 64 * 1024
	// officialFollowMaxReadBytes 与执行层 1 MiB 保留窗口对齐：再往前读也读不到。
	officialFollowMaxReadBytes = 1024 * 1024
	// officialFollowMaxPages 是翻页上限（rune 游标 → 字节窗口的换算成本上限）。
	officialFollowMaxPages = 64
	// officialTerminalMaxIDRunes 是终端身份的字符上限（官方 id 由调用方给）。
	officialTerminalMaxIDRunes = 120
	// officialRetainedPerSession 是每个会话保留布局的条数上限（有界池）。
	officialRetainedPerSession = 16
	// officialLabelMaxRunes 是 job 标签（命令首行）的字符上限。
	officialLabelMaxRunes = 200
)

// ---------------------------------------------------------------------------
// job.*：包在既有 jobs 服务之上
// ---------------------------------------------------------------------------

// officialJobService 把既有 jobs 服务翻译成官方 job.* 的字段。
//
// 三个回调都由 serve.go 提供（roster 读 lr.jobs，output/stop 走 jobActionBroker），
// 本文件不自己找进程、不自己读盘。
type officialJobService struct {
	roster func(sessionID string) []transport.JobInfo
	output func(ctx context.Context, sessionID, id string, cursor, limit int, wait time.Duration) (transport.JobOutput, error)
	stop   func(ctx context.Context, sessionID, id string) (transport.JobStopResult, error)
}

// JobList 是官方 job/list：只列**这条会话能看到的**（自己的 + 未归属的）。
func (s *officialJobService) JobList(sessionID string) (transport.OfficialJobListFrame, error) {
	if s == nil || s.roster == nil {
		return transport.OfficialJobListFrame{}, fmt.Errorf("%w: 这个宿主没有接 jobs 清单", transport.ErrJTOfficialUnsupported)
	}
	if !validJobActionPart(sessionID) {
		return transport.OfficialJobListFrame{}, fmt.Errorf("%w: 非法 sessionId", transport.ErrJTOfficialBadParam)
	}
	infos := s.roster(sessionID)
	views := make([]transport.OfficialJobView, 0, len(infos))
	for _, info := range infos {
		// 归属围栏：别人的任务不出现在这条会话的清单里。
		if info.SessionID != "" && info.SessionID != sessionID {
			continue
		}
		views = append(views, officialJobView(info))
	}
	return transport.OfficialJobListFrame{Type: "rows", Jobs: views}, nil
}

// officialJobView 是 JobInfo → JobView 的唯一换算点。
//
// 已知缺口（都写在这里，不靠文档口头约定）：
//   - startedAt 恒为 0：既有 jobs 服务不暴露登记时间（spi.JobInfo 没有这个字段）。
//     官方要求它是 number，0 表示"不知道"，比编一个假时间诚实。
//   - finishedAt 不出现：同上。
//   - kind 恒为 "shell"：本宿主的后台任务都是 bash 执行层起的。
//   - status 只能分辨 running / completed / failed：killed 与 stopping 需要
//     产物自报的生命周期状态，执行层没有（停掉就是 done + 非零退出码）。
func officialJobView(info transport.JobInfo) transport.OfficialJobView {
	view := transport.OfficialJobView{
		ID:        info.ID,
		Kind:      "shell",
		Label:     officialJobLabel(info.Command),
		Status:    "running",
		StartedAt: 0,
		Output:    transport.OfficialJobOutput{Total: int64(info.Bytes)},
	}
	if info.SessionID != "" {
		view.Owner = info.SessionID
	}
	if info.Done {
		if info.ExitCode == 0 {
			view.Status = "completed"
		} else {
			view.Status = "failed"
		}
		view.Detail = fmt.Sprintf("exit code: %d", info.ExitCode)
	}
	return view
}

// officialJobLabel 把命令压成一行标签（官方 label 是"one-line label"）。
func officialJobLabel(cmd string) string {
	line := cmd
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > officialLabelMaxRunes {
		runes := []rune(line)
		line = string(runes[:officialLabelMaxRunes])
	}
	return line
}

// JobKill 是官方 job/kill：复用既有 Stop（围栏/映像校验/409 都在那里）。
func (s *officialJobService) JobKill(ctx context.Context, sessionID, jobID string) (transport.OfficialJobKillValue, error) {
	if s == nil || s.stop == nil {
		return transport.OfficialJobKillValue{}, fmt.Errorf("%w: 这个宿主没有接 jobs 停止动作", transport.ErrJTOfficialUnsupported)
	}
	if !validJobActionPart(sessionID) || !validJobActionPart(jobID) {
		return transport.OfficialJobKillValue{}, fmt.Errorf("%w: 非法 sessionId/jobId", transport.ErrJTOfficialBadParam)
	}
	if _, err := s.stop(ctx, sessionID, jobID); err != nil {
		// 错误原样上抛：transport 层按 sentinel 翻 403/404/409/500。
		// **不在这里把"已结束"翻译成 already-finished**：本仓契约要求 409，
		// 而且客户端据此才知道该去读保留输出，而不是以为停成功了。
		return transport.OfficialJobKillValue{}, err
	}
	return transport.OfficialJobKillValue{Outcome: "requested"}, nil
}

// JobFollow 是官方 job/follow 的**一次性读取**：一页字节窗口，或一个终态投影。
//
// 偏移是**绝对字节**（官方语义）。绝对总量的锚点是既有 jobs 服务报的
// JobInfo.Bytes（写出的总字节），保留窗口的起点由这次读取的真实字节数反推：
// earliest = max(0, 锚点 - 本次读到的字节数)。
func (s *officialJobService) JobFollow(ctx context.Context, sessionID, jobID string, from int64) (transport.OfficialJobFollowFrame, error) {
	if s == nil || s.output == nil {
		return transport.OfficialJobFollowFrame{}, fmt.Errorf("%w: 这个宿主没有接 jobs 输出", transport.ErrJTOfficialUnsupported)
	}
	if !validJobActionPart(sessionID) || !validJobActionPart(jobID) {
		return transport.OfficialJobFollowFrame{}, fmt.Errorf("%w: 非法 sessionId/jobId", transport.ErrJTOfficialBadParam)
	}

	// 不等待：官方是"有新输出就推"，一次性读取没有等待窗口。
	// 客户端按 next 轮询（这条路径不会假装在推）。
	text, done, exitCode, truncated, err := s.readAll(ctx, sessionID, jobID)
	if err != nil {
		return transport.OfficialJobFollowFrame{}, err
	}

	anchor := int64(len(text)) // 兜底：清单里没有这一行时，锚点就是本次读到的字节数
	if s.roster != nil {
		for _, info := range s.roster(sessionID) {
			if info.ID == jobID && (info.SessionID == "" || info.SessionID == sessionID) {
				if int64(info.Bytes) > anchor {
					anchor = int64(info.Bytes)
				}
				break
			}
		}
	}
	earliest := anchor - int64(len(text))
	if earliest < 0 {
		earliest = 0
	}

	// 已经排干且已结算 → 终态投影（官方 status 帧）。
	if from >= anchor {
		if done {
			info := transport.JobInfo{ID: jobID, SessionID: sessionID, Done: true, ExitCode: exitCode, Bytes: int(anchor)}
			view := officialJobView(info)
			return transport.OfficialJobFollowFrame{Type: "status", Job: &view}, nil
		}
		// 还没新输出：回一帧空 chunks（合法帧），客户端按 next 继续轮询。
		next := anchor
		return transport.OfficialJobFollowFrame{Type: "output", Chunks: []transport.OfficialJobChunk{}, Next: &next}, nil
	}

	frame := transport.OfficialJobFollowFrame{Type: "output"}
	if from < earliest {
		// 请求的位置已经被保留窗口淘汰：如实标 lossy + gapBefore。
		frame.Lossy = true
		from = earliest
	}
	offset := int(from - earliest)
	if offset > len(text) {
		offset = len(text)
	}
	end := offset + officialFollowPageBytes
	if end > len(text) {
		end = len(text)
	}
	chunk := transport.OfficialJobChunk{At: from, Text: text[offset:end]}
	if frame.Lossy {
		chunk.GapBefore = true
	}
	next := from + int64(len(chunk.Text))
	frame.Chunks = []transport.OfficialJobChunk{chunk}
	frame.Next = &next
	_ = truncated
	return frame, nil
}

// readAll 按既有分页把保留输出读回来（有界：最多 1 MiB / 64 页）。
func (s *officialJobService) readAll(ctx context.Context, sessionID, jobID string) (string, bool, int, bool, error) {
	var sb strings.Builder
	cursor := 0
	done := false
	exitCode := 0
	truncated := false
	for page := 0; page < officialFollowMaxPages; page++ {
		out, err := s.output(ctx, sessionID, jobID, cursor, transport.MaxJobOutputLimit, 0)
		if err != nil {
			return "", false, 0, false, err
		}
		sb.WriteString(out.Output)
		cursor = out.NextCursor
		done = out.Done
		exitCode = out.ExitCode
		truncated = truncated || out.Truncated
		if !out.HasMore || sb.Len() >= officialFollowMaxReadBytes {
			break
		}
	}
	text := sb.String()
	if len(text) > officialFollowMaxReadBytes {
		text = text[len(text)-officialFollowMaxReadBytes:]
	}
	return text, done, exitCode, truncated, nil
}

// ---------------------------------------------------------------------------
// terminal.*：包在真实持久 shell 会话之上
// ---------------------------------------------------------------------------

// officialTerminalEntry 是一条官方终端身份。
//
// term 是真实会话（nil = 已关闭/已随宿主重启消失）；retained + screen 是
// `terminal.retain` 的**真实效果**：身份与屏幕尾部落盘，宿主重启后 list 仍能看到。
type officialTerminalEntry struct {
	id        string
	sessionID string
	title     string
	shell     transport.OfficialTerminalShell
	cwd       string
	cols      int
	rows      int

	term     *bash.Terminal
	state    string
	exitCode *int
	errMsg   string

	closed   bool
	retained bool
	screen   string
	sequence int64
	holders  int

	createdAt time.Time
	lastUsed  time.Time
}

func (e *officialTerminalEntry) key() string { return e.sessionID + "\x00" + e.id }

// officialTerminalAPI 是 terminal.* 的唯一实现。
//
// 状态分成两处，各管一件事：
//   - 内存 entries：进程内的真实会话与元数据；
//   - `<home>/state/terminals/<sessionId>.json`：布局（id/标题/shell/尺寸）+
//     **被 retain 的那几条的屏幕尾部**。没有 retain 的条目重启后不落盘：
//     进程已经不存在，留一个点不开的空壳只会骗人。
type officialTerminalAPI struct {
	mu       sync.Mutex
	home     string
	bash     *bash.Service
	cwdOf    func(sessionID string) (string, error)
	stateDir string

	entries   map[string]*officialTerminalEntry
	closedIDs map[string]bool

	// unattended 是"没人挂着就回收"的阈值（官方 TerminalRetention.unattendedTimeoutMs）。
	// 0 = 不回收；retain 的持有者（holders>0）或 retained 的身份永不被回收。
	unattended time.Duration
	now        func() time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// newOfficialTerminalAPI 起终端层。svc 必须是 terminal.open 用的**同一个** Service
// （否则 official 列表与既有终端是两套实例）。
func newOfficialTerminalAPI(home string, svc *bash.Service, cwdOf func(string) (string, error), unattended time.Duration) (*officialTerminalAPI, error) {
	if svc == nil {
		return nil, fmt.Errorf("official terminal：必须复用 terminal.open 的 bash.Service")
	}
	if cwdOf == nil {
		cwdOf = func(string) (string, error) { return "", nil }
	}
	a := &officialTerminalAPI{
		home:       home,
		bash:       svc,
		cwdOf:      cwdOf,
		stateDir:   filepath.Join(home, "state", "terminals"),
		entries:    map[string]*officialTerminalEntry{},
		closedIDs:  map[string]bool{},
		unattended: unattended,
		now:        time.Now,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if err := os.MkdirAll(a.stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("建终端状态目录失败: %w", err)
	}
	a.loadLayouts()
	if unattended > 0 {
		go a.reclaimLoop()
	} else {
		close(a.done)
	}
	return a, nil
}

// Close 停掉回收协程并关掉本层启动的**全部**终端（只关自己记录的，不扫别人的）。
func (a *officialTerminalAPI) Close() {
	a.stopOnce.Do(func() {
		close(a.stop)
		if a.unattended > 0 {
			<-a.done
		}
	})
	a.mu.Lock()
	targets := make([]*officialTerminalEntry, 0, len(a.entries))
	for _, e := range a.entries {
		targets = append(targets, e)
	}
	a.mu.Unlock()
	for _, e := range targets {
		a.closeEntry(e)
	}
}

// envInt 读整型环境变量（负/非法值走默认）。
func officialUnattendedFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("DSHD_TERMINAL_UNATTENDED_IDLE"))
	if raw == "" {
		return 30 * time.Minute
	}
	if raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 30 * time.Minute
	}
	return d
}

// reclaimLoop 是无人值守回收：没人挂着（holders==0）、没被 retain、且空闲超过阈值的
// 终端会被真正关掉（回收进程树）。这正是官方 retain 要保护的东西——
// 没有这条循环，retain 就成了不产生任何效果的空动作。
func (a *officialTerminalAPI) reclaimLoop() {
	defer close(a.done)
	interval := a.unattended / 4
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
			a.reclaimOnce()
		}
	}
}

func (a *officialTerminalAPI) reclaimOnce() {
	a.mu.Lock()
	var victims []*officialTerminalEntry
	for _, e := range a.entries {
		if e.closed || e.retained || e.holders > 0 || e.state != "running" || e.term == nil {
			continue
		}
		if a.now().Sub(e.lastUsed) >= a.unattended {
			victims = append(victims, e)
		}
	}
	a.mu.Unlock()
	for _, e := range victims {
		e.closed = true
		e.retained = false
		a.closeEntry(e)
	}
}

// TerminalCreate 起一条**真实** shell 会话并登记官方身份。
//
// 官方幂等：同一会话里同一 id 再 create，参数一致就回同一条（不新起进程）；
// 参数不一致即 400——静默忽略差异会让界面以为尺寸/ shell 已生效。
func (a *officialTerminalAPI) TerminalCreate(ctx context.Context, sessionID string, req transport.OfficialTerminalCreateRequest) (transport.OfficialWebTerminalInfo, error) {
	if !validJobActionPart(sessionID) {
		return transport.OfficialWebTerminalInfo{}, fmt.Errorf("%w: 非法 sessionId", transport.ErrJTOfficialBadParam)
	}
	id := strings.TrimSpace(req.ID)
	switch {
	case id == "":
		return transport.OfficialWebTerminalInfo{}, fmt.Errorf("%w: 缺 id", transport.ErrJTOfficialBadParam)
	case utf8.RuneCountInString(id) > officialTerminalMaxIDRunes:
		return transport.OfficialWebTerminalInfo{}, fmt.Errorf("%w: id 最多 %d 个字符",
			transport.ErrJTOfficialBadParam, officialTerminalMaxIDRunes)
	case !validJobActionPart(id):
		return transport.OfficialWebTerminalInfo{}, fmt.Errorf("%w: id 含非法字符（路径分隔符/控制字符）",
			transport.ErrJTOfficialBadParam)
	}
	cols, rows := clampTerminalSize(req.Cols, req.Rows)
	if cols <= 0 || rows <= 0 {
		return transport.OfficialWebTerminalInfo{}, fmt.Errorf("%w: cols/rows 必须是正整数", transport.ErrJTOfficialBadParam)
	}
	shell, err := resolveOfficialShell(req.ShellPath)
	if err != nil {
		return transport.OfficialWebTerminalInfo{}, err
	}
	cwd := a.sessionCwd(sessionID)

	key := sessionID + "\x00" + id
	a.mu.Lock()
	if a.closedIDs[key] {
		a.mu.Unlock()
		// 官方：closed identity 不再可用（terminal/unavailable）。
		return transport.OfficialWebTerminalInfo{}, fmt.Errorf("%w: %s 已在本会话关闭", transport.ErrJTOfficialTerminalGone, id)
	}
	if existing := a.entries[key]; existing != nil && !existing.closed {
		info := existing.info()
		sameShape := existing.cols == cols && existing.rows == rows &&
			strings.EqualFold(existing.shell.Path, shell.Path)
		a.mu.Unlock()
		if !sameShape {
			return transport.OfficialWebTerminalInfo{}, fmt.Errorf(
				"%w: 终端 %s 已存在且参数不同（现在 %d×%d %s），要换参数请先 terminal.close",
				transport.ErrJTOfficialBadParam, id, existing.cols, existing.rows, existing.shell.Path)
		}
		return info, nil
	}
	a.mu.Unlock()

	term, err := a.bash.OpenTerminal(cwd)
	if err != nil {
		return transport.OfficialWebTerminalInfo{}, fmt.Errorf("起终端失败：%w", err)
	}
	if !shell.isDefault {
		if err := a.execIntoTargetShell(term, shell); err != nil {
			_ = a.bash.CloseTerminal(term.ID())
			return transport.OfficialWebTerminalInfo{}, err
		}
	}

	now := a.now()
	entry := &officialTerminalEntry{
		id: id, sessionID: sessionID,
		title: defaultTerminalTitle(shell, id),
		shell: transport.OfficialTerminalShell{Path: shell.Path, Args: shell.Args, Name: shell.Name},
		cwd:   cwd, cols: cols, rows: rows,
		term: term, state: "running",
		createdAt: now, lastUsed: now,
	}
	a.mu.Lock()
	a.entries[entry.key()] = entry
	a.mu.Unlock()
	a.persistSession(sessionID)
	return entry.info(), nil
}

// TerminalAttach 回一帧真实快照（官方首帧）。
func (a *officialTerminalAPI) TerminalAttach(ctx context.Context, sessionID, id string) (transport.OfficialTerminalSnapshot, error) {
	entry, err := a.lookup(sessionID, id, "terminal.attach")
	if err != nil {
		return transport.OfficialTerminalSnapshot{}, err
	}
	screen, sequence, dead := a.screenOf(entry)
	writable := entry.state == "running" && !dead
	if dead {
		a.markExited(entry)
	}
	return transport.OfficialTerminalSnapshot{
		Type:     "snapshot",
		Sequence: sequence,
		Screen:   screen,
		Info:     entry.info(),
		Writable: writable,
		OneShot:  true,
	}, nil
}

// TerminalList 列这条会话的**真实存活**终端 + 被 retain 的（含重启后恢复的布局）。
func (a *officialTerminalAPI) TerminalList(sessionID string) ([]transport.OfficialWebTerminalInfo, error) {
	if !validJobActionPart(sessionID) {
		return nil, fmt.Errorf("%w: 非法 sessionId", transport.ErrJTOfficialBadParam)
	}
	a.mu.Lock()
	entries := make([]*officialTerminalEntry, 0, len(a.entries))
	for _, e := range a.entries {
		if e.sessionID == sessionID && !e.closed {
			entries = append(entries, e)
		}
	}
	a.mu.Unlock()
	// 顺手把已经退出的标出来（真实状态，不是猜的）。
	for _, e := range entries {
		if e.state == "running" && e.term != nil {
			a.screenOf(e) // 内部会把 dead 标进 state
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].createdAt.Equal(entries[j].createdAt) {
			return entries[i].id < entries[j].id
		}
		return entries[i].createdAt.Before(entries[j].createdAt)
	})
	out := make([]transport.OfficialWebTerminalInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.info())
	}
	return out, nil
}

// TerminalClose 真关：关会话 → 回收整棵进程树；重复 close 成功（官方语义）。
func (a *officialTerminalAPI) TerminalClose(ctx context.Context, sessionID, id string) error {
	if !validJobActionPart(sessionID) {
		return fmt.Errorf("%w: 非法 sessionId", transport.ErrJTOfficialBadParam)
	}
	key := sessionID + "\x00" + id
	a.mu.Lock()
	if a.closedIDs[key] {
		a.mu.Unlock()
		return nil // 官方：repeated closes succeed
	}
	entry := a.entries[key]
	if entry == nil {
		a.mu.Unlock()
		// 不是本会话的终端要报 403 而不是 404：既不能操作，也不该被当成"没有"。
		if _, err := a.lookup(sessionID, id, "terminal.close"); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s", transport.ErrJTOfficialTerminalGone, id)
	}
	entry.closed = true
	delete(a.entries, key)
	a.closedIDs[key] = true
	a.mu.Unlock()

	a.closeEntry(entry)
	a.persistSession(sessionID)
	return nil
}

// closeEntry 关掉真实会话（回收进程树）并把状态落成终态。
func (a *officialTerminalAPI) closeEntry(entry *officialTerminalEntry) {
	entry.closed = true
	if entry.term != nil {
		native := entry.term.ID()
		if err := a.bash.CloseTerminal(native); err != nil {
			// 已经没了不算失败；记下来供排障。
			entry.errMsg = err.Error()
		}
		entry.term = nil
	}
	if entry.state == "running" {
		entry.state = "exited"
	}
	if entry.retained {
		a.persistSession(entry.sessionID)
	}
}

// TerminalEnvironment 报**本层真实生效**的限制（每个数都能在代码里找到强制点）。
func (a *officialTerminalAPI) TerminalEnvironment(ctx context.Context, sessionID string) (transport.OfficialTerminalEnvironment, error) {
	if !validJobActionPart(sessionID) {
		return transport.OfficialTerminalEnvironment{}, fmt.Errorf("%w: 非法 sessionId", transport.ErrJTOfficialBadParam)
	}
	return transport.OfficialTerminalEnvironment{
		Cwd:              a.sessionCwd(sessionID),
		MaxInputBytes:    officialTerminalMaxInputBytes,
		MaxBufferedBytes: officialTerminalMaxScreenBytes,
		MaxCols:          officialTerminalMaxCols,
		MaxRows:          officialTerminalMaxRows,
		// 没有 PTY 也就没有行滚动缓冲：如实报 0，不编一个 1000。
		Scrollback: 0,
	}, nil
}

// TerminalShells 是**真实探测**：默认 shell 排第一，其余按可执行名去重。
//
// 每一项带 supported：只有默认 shell 能真正起（底层模块写死 bash 引导），
// 其余装了也起不来——给客户端一个可灰掉的标记，比让用户点了报错好。
func (a *officialTerminalAPI) TerminalShells(ctx context.Context, sessionID string) ([]transport.OfficialTerminalShell, error) {
	if !validJobActionPart(sessionID) {
		return nil, fmt.Errorf("%w: 非法 sessionId", transport.ErrJTOfficialBadParam)
	}
	return discoverOfficialShells()
}

// TerminalRename 改标题并落盘（真实持久化：宿主重启后 list 仍是新标题）。
func (a *officialTerminalAPI) TerminalRename(ctx context.Context, sessionID, id, title string) error {
	entry, err := a.lookup(sessionID, id, "terminal.rename")
	if err != nil {
		return err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("%w: title 不能为空", transport.ErrJTOfficialBadParam)
	}
	a.mu.Lock()
	entry.title = title
	entry.lastUsed = a.now()
	a.mu.Unlock()
	a.persistSession(sessionID)
	return nil
}

// TerminalRetain 让身份"被保留"：持有期间不被无人值守回收，屏幕尾部与布局落盘。
func (a *officialTerminalAPI) TerminalRetain(ctx context.Context, sessionID, id string) error {
	entry, err := a.lookup(sessionID, id, "terminal.retain")
	if err != nil {
		return err
	}
	screen, sequence, _ := a.screenOf(entry)
	a.mu.Lock()
	entry.holders++
	entry.retained = true
	if sequence > entry.sequence {
		entry.sequence = sequence
	}
	if screen != "" {
		entry.screen = screen
	}
	entry.lastUsed = a.now()
	a.mu.Unlock()
	a.persistSession(sessionID)
	return nil
}

// Input 是本层对既有 terminal.input 的等价实现（若 Lead 接线，写入就有上限）。
//
// 它把 environment.maxInputBytes 变成真的强制点：超限直接拒绝，不静默截断。
func (a *officialTerminalAPI) Input(sessionID, id, data string) error {
	entry, err := a.lookup(sessionID, id, "terminal.input")
	if err != nil {
		return err
	}
	if len(data) > officialTerminalMaxInputBytes {
		return fmt.Errorf("%w: 单次写入 %d 字节超过上限 %d",
			transport.ErrJTOfficialBadParam, len(data), officialTerminalMaxInputBytes)
	}
	a.mu.Lock()
	term := entry.term
	state := entry.state
	entry.lastUsed = a.now()
	a.mu.Unlock()
	if term == nil || state != "running" {
		return fmt.Errorf("%w: %s 已不在运行", transport.ErrJTOfficialTerminalGone, id)
	}
	if err := term.Input(data); err != nil {
		a.markExited(entry)
		return fmt.Errorf("%w: %v", transport.ErrJTOfficialTerminalGone, err)
	}
	return nil
}

// Output 是本层对既有 terminal.output 的等价实现（增量读，游标是字节）。
func (a *officialTerminalAPI) Output(sessionID, id string, since int) (transport.TerminalOutput, error) {
	entry, err := a.lookup(sessionID, id, "terminal.output")
	if err != nil {
		return transport.TerminalOutput{}, err
	}
	a.mu.Lock()
	term := entry.term
	if term != nil {
		entry.lastUsed = a.now()
	}
	a.mu.Unlock()
	if term == nil {
		return transport.TerminalOutput{Output: "", Next: since, Dead: true}, nil
	}
	out, next, dead := term.Output(since)
	if dead {
		a.markExited(entry)
	}
	return transport.TerminalOutput{Output: string(out), Next: next, Dead: dead}, nil
}

// lookup 取一条属于该会话的终端；不是本会话的一律 403，不泄漏存在性。
func (a *officialTerminalAPI) lookup(sessionID, id, method string) (*officialTerminalEntry, error) {
	if !validJobActionPart(sessionID) {
		return nil, fmt.Errorf("%w: 非法 sessionId", transport.ErrJTOfficialBadParam)
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: %s 缺 id", transport.ErrJTOfficialBadParam, method)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := a.entries[sessionID+"\x00"+id]
	if entry == nil || entry.closed {
		// 别人的终端：先看它在不在别的会话里。
		for _, other := range a.entries {
			if other.id == id && other.sessionID != sessionID && !other.closed {
				return nil, fmt.Errorf("%w: 终端 %s 属于另一个会话", transport.ErrCrossSession, id)
			}
		}
		return nil, fmt.Errorf("%w: %s", transport.ErrJTOfficialTerminalGone, id)
	}
	return entry, nil
}

// screenOf 取有界输出尾部与真实序号（已写出的字节数，单调）。
func (a *officialTerminalAPI) screenOf(entry *officialTerminalEntry) (string, int64, bool) {
	a.mu.Lock()
	term := entry.term
	a.mu.Unlock()
	if term == nil {
		return entry.screen, entry.sequence, entry.state != "running"
	}
	// since = 尾部窗口起点：一次只取有界的一段，不把整段历史搬进来。
	_, total, dead := term.Output(1 << 30)
	start := 0
	if total > officialTerminalMaxScreenBytes {
		start = total - officialTerminalMaxScreenBytes
	}
	raw, next, dead2 := term.Output(start)
	dead = dead || dead2
	a.mu.Lock()
	if int64(next) > entry.sequence {
		entry.sequence = int64(next)
	}
	if dead {
		entry.state = "exited"
	}
	a.mu.Unlock()
	return string(raw), int64(next), dead
}

// markExited 记下真实退出（退出码拿不到：bash 模块不导出，官方允许 null）。
func (a *officialTerminalAPI) markExited(entry *officialTerminalEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry.state == "running" {
		entry.state = "exited"
	}
}

// info 是 entry → 官方 WebTerminalInfo 的唯一换算点。
func (e *officialTerminalEntry) info() transport.OfficialWebTerminalInfo {
	info := transport.OfficialWebTerminalInfo{
		ID:       e.id,
		Title:    e.title,
		Shell:    e.shell,
		Cwd:      e.cwd,
		Cols:     e.cols,
		Rows:     e.rows,
		State:    e.state,
		ExitCode: e.exitCode,
		Error:    e.errMsg,
	}
	if info.Shell.Args == nil {
		info.Shell.Args = []string{}
	}
	return info
}

// sessionCwd 取会话工作目录（v4 头部记的那个）；不知道就回空串，不猜。
func (a *officialTerminalAPI) sessionCwd(sessionID string) string {
	if a.cwdOf == nil {
		return ""
	}
	cwd, err := a.cwdOf(sessionID)
	if err != nil {
		return ""
	}
	return cwd
}

// clampTerminalSize 把尺寸夹进 environment 报的 maxCols/maxRows。
func clampTerminalSize(cols, rows int) (int, int) {
	if cols > officialTerminalMaxCols {
		cols = officialTerminalMaxCols
	}
	if rows > officialTerminalMaxRows {
		rows = officialTerminalMaxRows
	}
	return cols, rows
}

func defaultTerminalTitle(shell officialShell, id string) string {
	if name := shell.Name; name != "" {
		return name
	}
	return id
}

// execIntoTargetShell 让会话真正变成**用户选的那个 shell**。
//
// 底层 Service 只会起 bash（写死），但它起的 bash 继承了 stdin/stdout 管道与
// Job Object 归属。往它的 stdin 写一条 `exec <目标>`，bash 会被目标进程替换：
// PID 不变、管道不变、进程树归属不变，后续输入由目标 shell 自己解释。
//
// 静默失败的风险由调用方兜住：写完等一小会，会话死了就说明 exec 失败（起不来）。
func (a *officialTerminalAPI) execIntoTargetShell(term *bash.Terminal, shell officialShell) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("%w: 本宿主只支持默认 shell（%s）；指定其它 shell 的引导只在 Windows 上实现",
			transport.ErrJTOfficialUnsupported, shell.Path)
	}
	line, err := shellExecLine(shell)
	if err != nil {
		return err
	}
	if err := term.Input(line); err != nil {
		return fmt.Errorf("把会话切到 %s 失败：%w", shell.Path, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if _, _, dead := term.Output(1 << 30); dead {
			return fmt.Errorf("%w: 指定的 shell 起不来（%s）：会话已退出", transport.ErrJTOfficialBadParam, shell.Path)
		}
	}
	return nil
}

// shellExecLine 造 "exec '<msys path>' <args>\n"。
func shellExecLine(shell officialShell) (string, error) {
	path := msysPath(shell.Path)
	if path == "" {
		return "", fmt.Errorf("%w: shellPath 为空", transport.ErrJTOfficialBadParam)
	}
	quoted := "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	parts := []string{"exec", quoted}
	for _, arg := range shell.Args {
		parts = append(parts, "'"+strings.ReplaceAll(arg, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ") + "\n", nil
}

// msysPath 把 Windows 路径转成 MSYS bash 认的形式（C:\x\y.exe → /c/x/y.exe）。
func msysPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		drive := strings.ToLower(p[:1])
		rest := p[2:]
		if rest == "" {
			rest = "/"
		}
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		return "/" + drive + rest
	}
	return p
}

// officialShell 是探测结果 + 它是不是本宿主真正能起的那个。
type officialShell struct {
	transport.OfficialTerminalShell
	isDefault bool
}

// resolveOfficialShell 解析 create 的 shellPath：空 = 默认；指向默认 = 默认；
// 指向别的真实存在但本宿主起不来的 = 501（不是 400：参数没错，能力没到）。
func resolveOfficialShell(raw string) (officialShell, error) {
	def, err := officialDefaultShell()
	if err != nil {
		return officialShell{}, fmt.Errorf("%w: 找不到可用的默认 shell：%v", transport.ErrJTOfficialUnsupported, err)
	}
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	abs, err := resolveShellPath(raw)
	if err != nil {
		return officialShell{}, fmt.Errorf("%w: shellPath %q 不是可执行文件：%v", transport.ErrJTOfficialBadParam, raw, err)
	}
	if strings.EqualFold(abs, def.Path) {
		return def, nil
	}
	// 能探测到但起不来：底层模块写死 bash 引导，这里如实回 501。
	// （Windows 上 execIntoTargetShell 会真的切过去；这条分支留给非 Windows。）
	if runtime.GOOS != "windows" {
		return officialShell{}, fmt.Errorf("%w: 本宿主只支持默认 shell（%s），收到 %s",
			transport.ErrJTOfficialUnsupported, def.Path, abs)
	}
	return officialShell{
		OfficialTerminalShell: transport.OfficialTerminalShell{
			Path: abs, Args: shellArgsFor(abs), Name: filepath.Base(abs),
		},
	}, nil
}

// officialDefaultShell 必须和 modules/exec/bash 的 findBash **同一套优先级**：
// DSHD_BASH → PATH 里的 bash → Git for Windows 的两个常见位置。
//
// 这里重复了一份（模块那边没导出），所以 discoverOfficialShells 的测试会
// 用"起一个终端 + 跑一条命令回读"来证明默认 shell 真的能跑——不靠这份复制的正确性。
func officialDefaultShell() (officialShell, error) {
	candidates := []string{}
	if p := os.Getenv("DSHD_BASH"); p != "" {
		candidates = append(candidates, p)
	}
	if p, err := exec.LookPath("bash"); err == nil {
		candidates = append(candidates, p)
	}
	if runtime.GOOS == "windows" {
		candidates = append(candidates,
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files\Git\usr\bin\bash.exe`)
	}
	for _, c := range candidates {
		abs, err := resolveShellPath(c)
		if err != nil {
			continue
		}
		return officialShell{
			OfficialTerminalShell: transport.OfficialTerminalShell{
				Path: abs, Args: shellArgsFor(abs), Name: filepath.Base(abs),
			},
			isDefault: true,
		}, nil
	}
	return officialShell{}, fmt.Errorf("没有可用的 bash（装 Git for Windows / 加进 PATH / 设 DSHD_BASH）")
}

// resolveShellPath 把候选解析成绝对路径并确认它是可执行文件。
func resolveShellPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("空路径")
	}
	abs := raw
	if !filepath.IsAbs(abs) {
		found, err := exec.LookPath(raw)
		if err != nil {
			return "", err
		}
		abs = found
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是目录", abs)
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(abs), ".exe") &&
		!strings.EqualFold(filepath.Ext(abs), ".com") && !strings.EqualFold(filepath.Ext(abs), ".bat") {
		return "", fmt.Errorf("%s 不是 Windows 可执行文件", abs)
	}
	return abs, nil
}

// shellArgsFor 与官方 shells.js 的 profile() 对齐：
// cmd → []；pwsh/powershell → ["-NoLogo"]；其余 → ["-i"]。
//
// 本宿主的默认 shell 是 bash，但**不能**给它 -i：底层会话是 `bash --noprofile
// --norc -s`（非交互读 stdin），加 -i 会让提示符与作业控制进到输出里。
func shellArgsFor(path string) []string {
	kind := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	switch kind {
	case "cmd":
		return []string{}
	case "pwsh", "powershell":
		return []string{"-NoLogo"}
	case "bash", "sh", "zsh", "dash":
		return []string{}
	}
	return []string{"-i"}
}

// discoverOfficialShells 真实探测：默认 shell 第一，其余按可执行名去重。
func discoverOfficialShells() ([]transport.OfficialTerminalShell, error) {
	def, err := officialDefaultShell()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", transport.ErrJTOfficialUnsupported, err)
	}
	// supported 的判据与 create 完全一致：默认 shell 永远可起；非默认只在
	// Windows 上可起（用 bash 的 exec 引导切换），其它平台 create 会明确 501。
	// 两处判据必须同源，否则界面会把"点了必报错"的项画成可用。
	supportedFor := func(isDefault bool) bool { return isDefault || runtime.GOOS == "windows" }
	supported := supportedFor(true)
	out := []transport.OfficialTerminalShell{{
		Path: def.Path, Args: def.Args, Name: def.Name, Supported: &supported,
	}}
	seen := map[string]bool{strings.ToLower(def.Name): true}
	candidates := []string{"cmd.exe", "powershell.exe", "pwsh.exe", "bash.exe", "sh"}
	if runtime.GOOS != "windows" {
		candidates = []string{"sh", "bash", "zsh", "dash", "fish"}
	}
	for _, candidate := range candidates {
		abs, err := resolveShellPath(candidate)
		if err != nil {
			continue // 没装就是没装，不是错误
		}
		name := filepath.Base(abs)
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		flag := supportedFor(false)
		out = append(out, transport.OfficialTerminalShell{
			Path: abs, Args: shellArgsFor(abs), Name: name, Supported: &flag,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 布局落盘（rename/retain 的"真持久化"）
// ---------------------------------------------------------------------------

type persistedTerminalLayout struct {
	Version   int                         `json:"version"`
	SessionID string                      `json:"sessionId"`
	Terminals []persistedTerminalIdentity `json:"terminals"`
}

type persistedTerminalIdentity struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	ShellPath string   `json:"shellPath"`
	ShellName string   `json:"shellName"`
	ShellArgs []string `json:"shellArgs"`
	Cwd       string   `json:"cwd"`
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	Retained  bool     `json:"retained"`
	// Screen 只在 Retained 时写（有界尾部）：这是 retain 的真实产物。
	Screen  string `json:"screen,omitempty"`
	SavedAt int64  `json:"savedAt"`
}

func (a *officialTerminalAPI) layoutPath(sessionID string) string {
	return filepath.Join(a.stateDir, sessionID+".json")
}

// persistSession 写这条会话的布局：只写**值得保留**的条目。
//
// 规则（为什么不是全写）：
//   - retained：写（含屏幕尾部）——这就是 retain 的意义；
//   - 运行中但没 retain：不写。宿主重启后进程一定没了，写下来只会在列表里
//     留一个点不开的空壳；
//   - 已关闭：不写。
func (a *officialTerminalAPI) persistSession(sessionID string) {
	if !validJobActionPart(sessionID) {
		return
	}
	a.mu.Lock()
	layout := persistedTerminalLayout{Version: 1, SessionID: sessionID}
	for _, e := range a.entries {
		if e.sessionID != sessionID || e.closed || !e.retained {
			continue
		}
		layout.Terminals = append(layout.Terminals, persistedTerminalIdentity{
			ID: e.id, Title: e.title,
			ShellPath: e.shell.Path, ShellName: e.shell.Name, ShellArgs: e.shell.Args,
			Cwd: e.cwd, Cols: e.cols, Rows: e.rows,
			Retained: true, Screen: e.screen, SavedAt: a.now().UnixMilli(),
		})
	}
	path := a.layoutPath(sessionID)
	a.mu.Unlock()

	if len(layout.Terminals) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "transport: 删终端布局 %s 失败: %v\n", path, err)
		}
		return
	}
	sort.Slice(layout.Terminals, func(i, j int) bool { return layout.Terminals[i].ID < layout.Terminals[j].ID })
	if len(layout.Terminals) > officialRetainedPerSession {
		// 有界池：留最近保存的那些（SavedAt 大的在前）。
		sort.Slice(layout.Terminals, func(i, j int) bool { return layout.Terminals[i].SavedAt > layout.Terminals[j].SavedAt })
		layout.Terminals = layout.Terminals[:officialRetainedPerSession]
	}
	raw, err := json.MarshalIndent(layout, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "transport: 编终端布局失败: %v\n", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "transport: 写终端布局 %s 失败: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "transport: 提交终端布局 %s 失败: %v\n", path, err)
	}
}

// loadLayouts 恢复被 retain 过的身份：进程已经不在（宿主重启过），
// 所以状态如实是 exited、writable=false、exitCode=null，屏幕尾部就是当时留下的那份。
func (a *officialTerminalAPI) loadLayouts() {
	entries, err := os.ReadDir(a.stateDir)
	if err != nil {
		return
	}
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(a.stateDir, de.Name()))
		if err != nil {
			continue
		}
		var layout persistedTerminalLayout
		if err := json.Unmarshal(raw, &layout); err != nil {
			fmt.Fprintf(os.Stderr, "transport: 终端布局 %s 损坏，忽略: %v\n", de.Name(), err)
			continue
		}
		if !validJobActionPart(layout.SessionID) {
			continue
		}
		for _, t := range layout.Terminals {
			if !t.Retained || !validJobActionPart(t.ID) {
				continue
			}
			entry := &officialTerminalEntry{
				id: t.ID, sessionID: layout.SessionID, title: t.Title,
				shell: transport.OfficialTerminalShell{Path: t.ShellPath, Args: t.ShellArgs, Name: t.ShellName},
				cwd:   t.Cwd, cols: t.Cols, rows: t.Rows,
				state: "exited", retained: true, screen: t.Screen,
				errMsg:    "宿主重启：终端进程已不存在（保留的是布局与屏幕尾部）",
				createdAt: time.UnixMilli(t.SavedAt), lastUsed: time.UnixMilli(t.SavedAt),
			}
			a.entries[entry.key()] = entry
		}
	}
}

// ---------------------------------------------------------------------------
// 组装：给 Lead 的一行接线入口
// ---------------------------------------------------------------------------

// officialJobTerminalService 把 job.* 与 terminal.* 两半合成 transport 要的服务面。
type officialJobTerminalService struct {
	*officialJobService
	*officialTerminalAPI
}

// Close 释放终端层（关掉本层起的会话 + 停回收协程）。
func (s *officialJobTerminalService) Close() {
	if s != nil && s.officialTerminalAPI != nil {
		s.officialTerminalAPI.Close()
	}
}

// Handler 返回可以直接挂到 transport.Options 上的路由对象。
//
// Lead 的接线只需要三行：构造 → Options 上挂 Handler() → defer Close()。
func (s *officialJobTerminalService) Handler() *transport.OfficialJobTerminal {
	return &transport.OfficialJobTerminal{Svc: s}
}

// newOfficialJobTerminalService 组装官方 11 个方法的服务面。
//
// 参数全部来自既有 serve() 里的对象，本函数不新建任何执行层：
//   - svc：terminal.open 用的那个 *bash.Service（必须同一个）；
//   - jobs：既有 jobActionBroker（围栏/映像校验/409 都在里面）；
//   - roster：按会话给后台任务清单（serve.go 里从 lr.jobs 过滤）；
//   - cwdOf：会话工作目录（serve.go 里是 src.Cwd）。
func newOfficialJobTerminalService(
	home string,
	svc *bash.Service,
	jobs *jobActionBroker,
	roster func(sessionID string) []transport.JobInfo,
	cwdOf func(sessionID string) (string, error),
) (*officialJobTerminalService, error) {
	terminals, err := newOfficialTerminalAPI(home, svc, cwdOf, officialUnattendedFromEnv())
	if err != nil {
		return nil, err
	}
	jobsSide := &officialJobService{roster: roster}
	if jobs != nil {
		jobsSide.output = jobs.Output
		jobsSide.stop = jobs.Stop
	}
	return &officialJobTerminalService{officialJobService: jobsSide, officialTerminalAPI: terminals}, nil
}
