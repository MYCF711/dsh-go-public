package main

// 官方桌面端 agent 侧 24 个方法在**宿主机**这一侧的真实实现。
//
// 规矩（与 internal/transport/handlers_agent_official.go 的分工）：
//   - Transport 只做协议与形状；本文件才碰真实状态（goal 存储、modes、
//     skill 目录、会话日志、上传/反馈的持久化）。
//   - **没有真实支撑就明确未实现**（transport.ErrOfficialNotImplemented），
//     绝不返回编出来的数据、也绝不"假执行"。
//   - goal 复用既有 `serve_goal_override.go` 的 versioned snapshot 存储
//     （同一个 `*goalOverrideStore`、同一个 `state/session-goal.json`、
//     同一个 `goal/change` journal）——**没有第二份 goal 存储**。
//
// 接线（Lead 在 cmd/dshd/serve.go 补一行）：
//
//	srv.AttachOfficialAgentAPI(newAgentOfficialAPI(agentOfficialDeps{
//	    home: *home, goals: goalOverrides, journals: journals, src: src,
//	    submit: <Options.Prompt 用的那个提交函数>,
//	}))

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"

	"dshgo/codecs/v4"
	"dshgo/internal/modes"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// agentFeedbackNoteMaxBytes 是一条消息反馈备注的字节上限。
//
// 官方 `MessageFeedbackPutResult` 的 `note-too-large` 会回 maxBytes/actualBytes，
// 所以这个数必须是一个**真实生效**的阈值，不能只为回包写死一个常数。
const agentFeedbackNoteMaxBytes = 4096

// agentUploadMaxBytes 是单次 fileUploads.upload 解码后的字节上限。
//
// 25 MiB：足够放一张截图或一份文档，又不至于把 home 目录写爆。
const agentUploadMaxBytes = 25 << 20

// agentReferenceLimit 是 @ 引用与会话引用候选的返回上限（官方"configured limit"）。
const agentReferenceLimit = 50

// agentSessionReferenceLimit 是会话引用候选的上限。
const agentSessionReferenceLimit = 20

// agentOfficialDeps 是本文件需要的全部真实状态来源。
type agentOfficialDeps struct {
	home     string
	goals    *goalOverrideStore
	journals *transport.Journals
	src      *sessionSource
	// submit 是真实 prompt 入口（userQuestions.answer 把答案当用户消息送进会话）。
	// 为 nil 时 userQuestions.answer 明确未实现。
	submit func(sessionID, text string) (transport.PromptResult, error)
	// submitPlacement 是**带 placement** 的同一个 prompt 入口
	// （queue → PlacementFollowup、steer → PlacementSteer）。
	//
	// 为什么单独一个字段：subagents.prompt 的 delivery 是 queue / steer 两道，
	// 而 submit 只有 turn 一道。没有它时 queue 只能退成 turn、steer 只能响亮失败；
	// 有了它两道才是宿主 promptService 里真实的那两道（不是第二份队列）。
	submitPlacement func(sessionID, text, placement string) (transport.PromptResult, error)
	// interrupt 是"取消某个会话当前轮"的真实通道（subagents.interruptByParent
	// 靠它）。构造点在 serve.go（本任务禁改路径），所以生产里可能为 nil——
	// 为 nil 时 interruptByParent 回**响亮 501**并说清缺哪一行接线，
	// 绝不假装取消信号被接受。
	interrupt func(sessionID string) error
}

// agentOfficialAPI 持有进程内（**不持久化**）的 goal activation。
//
// 官方的 `activation` 明确写着 "Process-local continuation eligibility; never
// persisted"，所以它放在内存里是对的：新进程里所有 goal 都是 disarmed。
type agentOfficialAPI struct {
	deps agentOfficialDeps

	mu         sync.Mutex
	activation map[string]string // sessionID → armed|disarmed
	goalTimes  map[string]agentGoalTimes

	// 下面三项是**真实**能力对象，懒建（第一次用到时装配）：
	//   commandsRegistry  commands.list / commands.execute 的真注册表
	//   children          subagents.* 的可续问子会话地址表
	//   questionWaits     userQuestions.attachWait 的真实前台等待表
	commandsRegistry *transport.OfficialCommandRegistry
	commandsErr      error
	children         *transport.OfficialChildRegistry
	questionWaits    *transport.OfficialQuestionWaits
}

// agentGoalTimes 是某个 goal id 的创建/最后变更时间（epoch 毫秒）。
type agentGoalTimes struct {
	CreatedAt int64
	UpdatedAt int64
}

// newAgentOfficialAPI 把真实依赖装配成 Transport 要的 24 个回调。
func newAgentOfficialAPI(deps agentOfficialDeps) transport.OfficialAgentAPI {
	return newAgentOfficialAPICore(deps).bind()
}

// newAgentOfficialAPICore 造出持有进程内状态的那个对象（测试要拿它看 activation）。
func newAgentOfficialAPICore(deps agentOfficialDeps) *agentOfficialAPI {
	return &agentOfficialAPI{
		deps:       deps,
		activation: map[string]string{},
		goalTimes:  map[string]agentGoalTimes{},
	}
}

// bind 把方法表交给 Transport。每个方法都绑定到同一个宿主对象，所以
// activation / 时间缓存这类进程内状态在 24 个方法之间是共享的。
func (a *agentOfficialAPI) bind() transport.OfficialAgentAPI {
	return transport.OfficialAgentAPI{
		GoalGet:      a.goalGet,
		GoalCreate:   a.goalCreate,
		GoalEdit:     a.goalEdit,
		GoalPause:    a.goalPause,
		GoalResume:   a.goalResume,
		GoalComplete: a.goalComplete,
		GoalClear:    a.goalClear,

		CommandsList:    a.commandsList,
		CommandsExecute: a.commandsExecute,

		SubagentsPrompt:            a.subagentsPrompt,
		SubagentsInterruptByParent: a.subagentsInterruptByParent,

		UserQuestionsAnswer:     a.userQuestionsAnswer,
		UserQuestionsAttachWait: a.userQuestionsAttachWait,

		AgentPresetRead:   a.agentPresetRead,
		AgentPresetSelect: a.agentPresetSelect,

		SkillsList:         a.skillsList,
		FileReferencesList: a.fileReferencesList,
		FileUploadsUpload:  a.fileUploadsUpload,

		MessageFeedbackList:   a.messageFeedbackList,
		MessageFeedbackPut:    a.messageFeedbackPut,
		MessageFeedbackDelete: a.messageFeedbackDelete,

		SessionFeedbackRecord:      a.sessionFeedbackRecord,
		SessionReferenceCandidates: a.sessionReferenceCandidates,
		ToolResultsWatch:           a.toolResultsWatch,
	}
}

// ---------------------------------------------------------------------------
// 通用小工具
// ---------------------------------------------------------------------------

// agentWriteDoc 原子写一份 JSON 文档（同目录 tmp + rename）。
func agentWriteDoc(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// agentReadDoc 读一份 JSON 文档；文件不存在返回 found=false（不是错误）。
func agentReadDoc(path string, dst any) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return false, fmt.Errorf("%s 是空文件", path)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return false, fmt.Errorf("解析 %s: %w", path, err)
	}
	return true, nil
}

// agentSessionPath 解析会话的 v4 路径；不存在时返回 transport.ErrNoSuchSession。
func (a *agentOfficialAPI) agentSessionPath(sessionID string) (string, error) {
	if a.deps.src == nil {
		return "", fmt.Errorf("%w: 宿主没有接会话来源", transport.ErrOfficialNotImplemented)
	}
	path, err := a.deps.src.resolve(sessionID)
	if err != nil {
		return "", err
	}
	return path, nil
}

// agentSessionExists 判断会话在磁盘上真实存在。
func (a *agentOfficialAPI) agentSessionExists(sessionID string) bool {
	path, err := a.agentSessionPath(sessionID)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// agentReadEvents 读一个会话的 durable 事件。
func (a *agentOfficialAPI) agentReadEvents(sessionID string) ([]v4.Event, error) {
	path, err := a.agentSessionPath(sessionID)
	if err != nil {
		return nil, err
	}
	log, err := v4.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读会话 %s 的 v4 日志: %w", sessionID, err)
	}
	return log.Events, nil
}

// agentEnsureSession 把"会话不存在"变成 go 里可识别的一个错误。
func (a *agentOfficialAPI) agentEnsureSession(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: 会话 id 是空串", transport.ErrNoSuchSession)
	}
	if !a.agentSessionExists(sessionID) {
		return transport.ErrNoSuchSession
	}
	return nil
}

// ---------------------------------------------------------------------------
// goals.*（复用既有 versioned snapshot 存储，不新造第二份）
// ---------------------------------------------------------------------------

// agentGoalOperation 是一次官方语义的 goal 迁移。
type agentGoalOperation struct {
	op        string // create / edit / pause / resume / complete / clear
	ref       transport.OfficialGoalRef
	objective *string
	maxRounds *int
}

// agentGoalOutcome 是迁移结果 + 这次迁移自己的真实时间。
type agentGoalOutcome struct {
	goal      *transport.GoalSnapshot
	rounds    *int
	createdAt int64
	updatedAt int64
	tombstone transport.OfficialGoalRef
}

// activation 读写进程内的 continuation 资格。
func (a *agentOfficialAPI) agentActivation(sessionID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.activation[sessionID]; ok {
		return v
	}
	return "disarmed"
}

func (a *agentOfficialAPI) agentSetActivation(sessionID, value string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activation[sessionID] = value
}

func (a *agentOfficialAPI) agentRememberGoalTimes(goalID string, times agentGoalTimes) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.goalTimes[goalID] = times
}

// agentGoalView 把一个存储记录翻成官方 GoalView。
//
// createdAt/updatedAt 必须来自**真实记录**：这里直接问 goal/change 帧
// （每次迁移都写过一帧，帧上的 time 就是那次迁移的墙钟）。找不到帧就报错，
// 不拿 now 顶上——那会让"这个 goal 是刚才建的"变成假话。
func (a *agentOfficialAPI) agentGoalView(sessionID string, record goalRecord) (transport.OfficialGoalView, error) {
	if record.Goal == nil {
		return transport.OfficialGoalView{}, fmt.Errorf("%w: 会话 %s 没有当前 goal", transport.ErrOfficialConflict, sessionID)
	}
	times, err := a.agentGoalTimesOf(sessionID, record.Goal.ID)
	if err != nil {
		return transport.OfficialGoalView{}, err
	}
	view := transport.OfficialGoalView{
		ID:            record.Goal.ID,
		Revision:      record.Goal.Revision,
		Objective:     record.Goal.Objective,
		Phase:         record.Goal.Phase,
		MaxGoalRounds: record.Goal.MaxGoalRounds,
		RoundsStarted: 0,
		CreatedAt:     times.CreatedAt,
		UpdatedAt:     times.UpdatedAt,
		Activation:    a.agentActivation(sessionID),
	}
	if record.RoundsStarted != nil {
		view.RoundsStarted = *record.RoundsStarted
	}
	if record.Goal.BlockedReason != nil {
		view.BlockedReason = &transport.OfficialGoalBlockedReason{
			Code:    record.Goal.BlockedReason.Code,
			Message: record.Goal.BlockedReason.Message,
		}
	}
	return view, nil
}

// agentGoalTimesOf 取一个 goal 的创建/最后变更时间。
//
// 先看进程内缓存（本进程刚迁移过就知道），再回落到 durable 的
// Transport journal 文件（`transport-journal.v1.jsonl`，它是 append-only 的
// 完整记录，不像内存 journal 那样有 4096 帧的容量上限）。
func (a *agentOfficialAPI) agentGoalTimesOf(sessionID, goalID string) (agentGoalTimes, error) {
	if goalID == "" {
		return agentGoalTimes{}, fmt.Errorf("%w: goal id 是空串", transport.ErrStatePersistence)
	}
	a.mu.Lock()
	cached, ok := a.goalTimes[goalID]
	a.mu.Unlock()
	if ok && cached.CreatedAt > 0 {
		return cached, nil
	}
	times, err := a.agentScanGoalTimes(sessionID, goalID)
	if err != nil {
		return agentGoalTimes{}, err
	}
	a.agentRememberGoalTimes(goalID, times)
	return times, nil
}

// agentScanGoalTimes 从 durable journal 文件里还原一个 goal 的时间。
func (a *agentOfficialAPI) agentScanGoalTimes(sessionID, goalID string) (agentGoalTimes, error) {
	v4Path, err := a.agentSessionPath(sessionID)
	if err != nil {
		return agentGoalTimes{}, err
	}
	journalPath := filepath.Join(filepath.Dir(v4Path), serveJournalFileName)
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return agentGoalTimes{}, agentGoalTimesMissing(sessionID, goalID, journalPath)
		}
		return agentGoalTimes{}, fmt.Errorf("%w: 读 %s: %v", transport.ErrStatePersistence, journalPath, err)
	}
	var times agentGoalTimes
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// 用真实的记录类型：Data 在磁盘上是 base64（`[]byte` 的标准编码），
		// 用 json.RawMessage 解会把它当成字符串而丢掉每一帧。
		var rec serveJournalRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// 尾部半帧（断电）不是错误：前面已经读到的仍然是真值。
			continue
		}
		if rec.Kind != serveJournalFrame || rec.Type != "goal/change" || rec.Time <= 0 {
			continue
		}
		var change transport.GoalChange
		if err := json.Unmarshal(rec.Data, &change); err != nil || change.Goal == nil {
			continue
		}
		if change.Goal.ID != goalID {
			continue
		}
		if times.CreatedAt == 0 || rec.Time < times.CreatedAt {
			times.CreatedAt = rec.Time
		}
		if rec.Time > times.UpdatedAt {
			times.UpdatedAt = rec.Time
		}
	}
	if times.CreatedAt == 0 {
		return agentGoalTimes{}, agentGoalTimesMissing(sessionID, goalID, journalPath)
	}
	return times, nil
}

// agentGoalTimesMissing 是"有 goal、没有迁移历史"时的响亮错误。
//
// 它只发生在一种真实情况：旧扁平 `session-goal.json` 在 Load 里被迁移成
// snapshot（迁移不是一次 UI 变更，所以没写 goal/change 帧）。那种 goal 的
// 创建时间**确实不存在**，本文件不编一个。
func agentGoalTimesMissing(sessionID, goalID, journalPath string) error {
	return fmt.Errorf("%w: goal %q（会话 %s）在 %s 里没有 goal/change 记录，创建/更新时间无法还原"+
		"（旧扁平格式迁移出来的 goal 没有这段历史）",
		transport.ErrStatePersistence, goalID, sessionID, journalPath)
}

// agentGoalTransact 是**唯一**的官方 goal 迁移入口。
//
// 它和既有 `goalOverrideStore.transition` 用同一份内存 map、同一个文件、
// 同一条 goal/change journal；区别只是支持官方语义（ref 校验、edit、
// complete、clear tombstone）。
func (a *agentOfficialAPI) agentGoalTransact(sessionID string, operation agentGoalOperation) (agentGoalOutcome, error) {
	store := a.deps.goals
	if store == nil {
		return agentGoalOutcome{}, fmt.Errorf("%w: 宿主没有 goal 存储", transport.ErrOfficialNotImplemented)
	}
	if sessionID == "" {
		return agentGoalOutcome{}, fmt.Errorf("%w: 会话 id 是空串", transport.ErrNoSuchSession)
	}
	if !a.agentSessionExists(sessionID) {
		return agentGoalOutcome{}, transport.ErrNoSuchSession
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadErr != nil {
		return agentGoalOutcome{}, fmt.Errorf("%w: %v", transport.ErrStatePersistence, store.loadErr)
	}
	if store.journal == nil || store.journal.journals == nil {
		return agentGoalOutcome{}, fmt.Errorf("%w: goal/change journal 未接线", transport.ErrStatePersistence)
	}

	before := cloneGoalMap(store.m)
	current, exists := before[sessionID]
	next := cloneGoalMap(before)
	outcome := agentGoalOutcome{}
	now := time.Now().UnixMilli()

	// 上一次迁移的真实时间（用来把 updatedAt 做成单调）。
	prevTimes := agentGoalTimes{}
	var prevTimesErr error
	if exists && current.Goal != nil {
		prevTimes, prevTimesErr = a.agentGoalTimesOfLocked(sessionID, current.Goal.ID)
	}
	if prevTimes.UpdatedAt > now {
		now = prevTimes.UpdatedAt
	}

	switch operation.op {
	case "create":
		if exists && current.Goal != nil && current.Goal.Phase != "complete" {
			return agentGoalOutcome{}, fmt.Errorf("%w: 会话 %s 还有未完成的 goal %q（phase=%s）",
				transport.ErrGoalTransition, sessionID, current.Goal.ID, current.Goal.Phase)
		}
		if operation.objective == nil || *operation.objective == "" {
			return agentGoalOutcome{}, fmt.Errorf("%w: objective 是空串", transport.ErrGoalTransition)
		}
		id, err := store.newID()
		if err != nil {
			return agentGoalOutcome{}, fmt.Errorf("%w: 生成 goal id: %v", transport.ErrStatePersistence, err)
		}
		maxRounds := 0
		if operation.maxRounds != nil {
			maxRounds = *operation.maxRounds
		}
		zero := 0
		record := goalRecord{
			Goal: &transport.GoalSnapshot{
				ID: id, Revision: 1, Objective: *operation.objective,
				Phase: "active", MaxGoalRounds: maxRounds,
			},
			RoundsStarted: &zero,
		}
		next[sessionID] = record
		outcome = agentGoalOutcome{goal: record.Goal, rounds: record.RoundsStarted, createdAt: now, updatedAt: now}

	case "edit", "pause", "resume", "complete":
		if !exists || current.Goal == nil {
			return agentGoalOutcome{}, fmt.Errorf("%w: 会话 %s 没有当前 goal", transport.ErrGoalTransition, sessionID)
		}
		// 非 create 的迁移要保住 createdAt：拿不到它就不动（否则回包会编一个假时间）。
		if prevTimesErr != nil {
			return agentGoalOutcome{}, prevTimesErr
		}
		if operation.ref.ID != current.Goal.ID || operation.ref.Revision != current.Goal.Revision {
			return agentGoalOutcome{}, fmt.Errorf("%w: goal ref %q@%d 已过期，当前是 %q@%d",
				transport.ErrGoalTransition, operation.ref.ID, operation.ref.Revision,
				current.Goal.ID, current.Goal.Revision)
		}
		if current.Goal.Revision == int64(^uint64(0)>>1) {
			return agentGoalOutcome{}, fmt.Errorf("%w: goal revision 已溢出", transport.ErrGoalTransition)
		}
		updated := cloneGoalSnapshot(current.Goal)
		updated.Revision = current.Goal.Revision + 1
		switch operation.op {
		case "edit":
			if operation.objective != nil {
				if *operation.objective == "" {
					return agentGoalOutcome{}, fmt.Errorf("%w: objective 是空串", transport.ErrGoalTransition)
				}
				updated.Objective = *operation.objective
			}
			if operation.maxRounds != nil {
				updated.MaxGoalRounds = *operation.maxRounds
			}
		case "pause":
			if current.Goal.Phase != "active" {
				return agentGoalOutcome{}, fmt.Errorf("%w: 只有 active goal 才能 pause，当前是 %s",
					transport.ErrGoalTransition, current.Goal.Phase)
			}
			updated.Phase = "paused"
		case "resume":
			switch current.Goal.Phase {
			case "active", "paused", "blocked":
			default:
				return agentGoalOutcome{}, fmt.Errorf("%w: %s 的 goal 不能 resume",
					transport.ErrGoalTransition, current.Goal.Phase)
			}
			rounds := 0
			if current.RoundsStarted != nil {
				rounds = *current.RoundsStarted
			}
			// maxGoalRounds=0 在本仓既有存储里表示"没有上限"（session.setGoal 建出来的
			// goal 就是 0），所以只有 >0 时才做预算判定。
			if updated.MaxGoalRounds > 0 && rounds >= updated.MaxGoalRounds {
				return agentGoalOutcome{}, fmt.Errorf("%w: goal 已用满 %d 轮预算，先提高 maxGoalRounds 再 resume",
					transport.ErrGoalTransition, updated.MaxGoalRounds)
			}
			updated.Phase = "active"
		case "complete":
			switch current.Goal.Phase {
			case "active", "paused", "blocked":
			default:
				return agentGoalOutcome{}, fmt.Errorf("%w: %s 的 goal 不能 complete（官方语义：已完成再 complete 是错，不是静默成功）",
					transport.ErrGoalTransition, current.Goal.Phase)
			}
			updated.Phase = "complete"
		}
		record := goalRecord{Goal: updated, RoundsStarted: cloneInt(current.RoundsStarted)}
		next[sessionID] = record
		createdAt := prevTimes.CreatedAt
		if createdAt == 0 {
			createdAt = now
		}
		outcome = agentGoalOutcome{goal: updated, rounds: record.RoundsStarted, createdAt: createdAt, updatedAt: now}

	case "clear":
		if !exists || current.Goal == nil {
			return agentGoalOutcome{}, fmt.Errorf("%w: 会话 %s 没有当前 goal，无可清除", transport.ErrGoalTransition, sessionID)
		}
		if prevTimesErr != nil {
			return agentGoalOutcome{}, prevTimesErr
		}
		if operation.ref.ID != current.Goal.ID || operation.ref.Revision != current.Goal.Revision {
			return agentGoalOutcome{}, fmt.Errorf("%w: goal ref %q@%d 已过期，当前是 %q@%d",
				transport.ErrGoalTransition, operation.ref.ID, operation.ref.Revision,
				current.Goal.ID, current.Goal.Revision)
		}
		delete(next, sessionID)
		outcome = agentGoalOutcome{
			goal: nil,
			tombstone: transport.OfficialGoalRef{
				ID: current.Goal.ID, Revision: current.Goal.Revision + 1,
			},
			createdAt: prevTimes.CreatedAt, updatedAt: now,
		}

	default:
		return agentGoalOutcome{}, fmt.Errorf("%w: 未知 goal 操作 %q", transport.ErrGoalTransition, operation.op)
	}

	// 先落 state，再落 journal；journal 失败要把 state 回滚（与既有 transition 同规矩）。
	if err := writeGoalDocument(store.path, goalDocument{Version: goalStoreVersion, Goals: next}); err != nil {
		return agentGoalOutcome{}, fmt.Errorf("%w: 写 goal store %s: %v", transport.ErrStatePersistence, store.path, err)
	}
	frameTime, err := a.agentAppendGoalChange(store, sessionID, operation.op, next[sessionID], now)
	if err != nil {
		if rollbackErr := writeGoalDocument(store.path, goalDocument{Version: goalStoreVersion, Goals: before}); rollbackErr != nil {
			store.loadErr = fmt.Errorf("journal 提交失败 %v；goal state 回滚也失败 %v", err, rollbackErr)
			return agentGoalOutcome{}, fmt.Errorf("%w: %v", transport.ErrStatePersistence, store.loadErr)
		}
		return agentGoalOutcome{}, err
	}
	store.m = next
	outcome.updatedAt = frameTime
	if outcome.createdAt == 0 {
		outcome.createdAt = frameTime
	}
	if outcome.goal != nil {
		a.agentRememberGoalTimes(outcome.goal.ID, agentGoalTimes{
			CreatedAt: outcome.createdAt, UpdatedAt: frameTime,
		})
	}
	switch operation.op {
	case "create", "resume":
		a.agentSetActivation(sessionID, "armed")
	case "pause", "complete", "clear":
		a.agentSetActivation(sessionID, "disarmed")
	}
	return outcome, nil
}

// agentGoalTimesOfLocked 在已持有 store 锁时仍可安全调用（缓存路径不碰 store）。
func (a *agentOfficialAPI) agentGoalTimesOfLocked(sessionID, goalID string) (agentGoalTimes, error) {
	a.mu.Lock()
	cached, ok := a.goalTimes[goalID]
	a.mu.Unlock()
	if ok && cached.CreatedAt > 0 {
		return cached, nil
	}
	return a.agentScanGoalTimes(sessionID, goalID)
}

// agentAppendGoalChange 追加一帧 goal/change 并回传帧上的真实时间。
//
// 它刻意不复用 `goalChangeJournal.append`（那个不返回值）：updatedAt 必须
// 等于**帧上的时间**，否则重启后从 journal 还原出来的值和这次回包对不上。
func (a *agentOfficialAPI) agentAppendGoalChange(store *goalOverrideStore, sessionID, operation string, record goalRecord, at int64) (int64, error) {
	data, err := json.Marshal(transport.GoalChange{
		Kind: "goal/change", Version: 1, Operation: operation,
		Goal: cloneGoalSnapshot(record.Goal), RoundsStarted: cloneInt(record.RoundsStarted),
	})
	if err != nil {
		return 0, fmt.Errorf("%w: 序列化 goal/change: %v", transport.ErrStatePersistence, err)
	}
	frame, err := emitFrameAtResult(store.journal.journals, store.journal.pub, sessionID, "goal/change", data, at)
	if err != nil {
		return 0, fmt.Errorf("%w: 追加 goal/change: %v", transport.ErrStatePersistence, err)
	}
	return frame.Time, nil
}

// agentCurrentGoal 读当前 goal 记录（不存在返回 exists=false）。
func (a *agentOfficialAPI) agentCurrentGoal(sessionID string) (goalRecord, bool, error) {
	store := a.deps.goals
	if store == nil {
		return goalRecord{}, false, fmt.Errorf("%w: 宿主没有 goal 存储", transport.ErrOfficialNotImplemented)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadErr != nil {
		return goalRecord{}, false, fmt.Errorf("%w: %v", transport.ErrStatePersistence, store.loadErr)
	}
	record, exists := store.m[sessionID]
	return cloneGoalRecord(record), exists, nil
}

func (a *agentOfficialAPI) goalGet(sessionID string) (transport.OfficialGoalGetResult, error) {
	if err := a.agentEnsureSession(sessionID); err != nil {
		return transport.OfficialGoalGetResult{}, err
	}
	record, exists, err := a.agentCurrentGoal(sessionID)
	if err != nil {
		return transport.OfficialGoalGetResult{}, err
	}
	if !exists || record.Goal == nil {
		return transport.OfficialGoalGetResult{Goal: nil}, nil
	}
	view, err := a.agentGoalView(sessionID, record)
	if err != nil {
		return transport.OfficialGoalGetResult{}, err
	}
	return transport.OfficialGoalGetResult{Goal: &view}, nil
}

func (a *agentOfficialAPI) goalCreate(sessionID string, req transport.OfficialGoalCreateRequest) (transport.OfficialGoalRef, error) {
	objective := req.Objective
	outcome, err := a.agentGoalTransact(sessionID, agentGoalOperation{
		op: "create", objective: &objective, maxRounds: req.MaxGoalRounds,
	})
	if err != nil {
		return transport.OfficialGoalRef{}, err
	}
	return transport.OfficialGoalRef{ID: outcome.goal.ID, Revision: outcome.goal.Revision}, nil
}

func (a *agentOfficialAPI) goalEdit(sessionID string, ref transport.OfficialGoalRef, req transport.OfficialGoalEditRequest) (transport.OfficialGoalView, error) {
	outcome, err := a.agentGoalTransact(sessionID, agentGoalOperation{
		op: "edit", ref: ref, objective: req.Objective, maxRounds: req.MaxGoalRounds,
	})
	if err != nil {
		return transport.OfficialGoalView{}, err
	}
	return a.agentGoalView(sessionID, goalRecord{Goal: outcome.goal, RoundsStarted: outcome.rounds})
}

func (a *agentOfficialAPI) goalPause(sessionID string, ref transport.OfficialGoalRef) (transport.OfficialGoalView, error) {
	return a.agentGoalSimpleTransition(sessionID, "pause", ref)
}

func (a *agentOfficialAPI) goalResume(sessionID string, ref transport.OfficialGoalRef) (transport.OfficialGoalView, error) {
	return a.agentGoalSimpleTransition(sessionID, "resume", ref)
}

func (a *agentOfficialAPI) goalComplete(sessionID string, ref transport.OfficialGoalRef) (transport.OfficialGoalView, error) {
	return a.agentGoalSimpleTransition(sessionID, "complete", ref)
}

func (a *agentOfficialAPI) agentGoalSimpleTransition(sessionID, op string, ref transport.OfficialGoalRef) (transport.OfficialGoalView, error) {
	outcome, err := a.agentGoalTransact(sessionID, agentGoalOperation{op: op, ref: ref})
	if err != nil {
		return transport.OfficialGoalView{}, err
	}
	return a.agentGoalView(sessionID, goalRecord{Goal: outcome.goal, RoundsStarted: outcome.rounds})
}

func (a *agentOfficialAPI) goalClear(sessionID string, ref transport.OfficialGoalRef) (transport.OfficialGoalRef, error) {
	outcome, err := a.agentGoalTransact(sessionID, agentGoalOperation{op: "clear", ref: ref})
	if err != nil {
		return transport.OfficialGoalRef{}, err
	}
	return outcome.tombstone, nil
}

// ---------------------------------------------------------------------------
// commands.*
// ---------------------------------------------------------------------------

// commandRegistry 懒建宿主真实命令注册表。
//
// 注册表本身住在 `serve_commands_api.go`（**真实** handler：/help 列注册表、
// /goal 读真实 goal 记录、/feedback 落 durable 反馈），这里只做一次性装配。
func (a *agentOfficialAPI) commandRegistry() (*transport.OfficialCommandRegistry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.commandsRegistry != nil || a.commandsErr != nil {
		return a.commandsRegistry, a.commandsErr
	}
	registry, err := newHostCommandRegistry(a)
	if err != nil {
		a.commandsErr = fmt.Errorf("%w: 装配命令注册表失败: %v", transport.ErrStatePersistence, err)
		return nil, a.commandsErr
	}
	a.commandsRegistry = registry
	return registry, nil
}

// commandsList 列出宿主**真实注册过**的命令（官方 `commands.list`）。
func (a *agentOfficialAPI) commandsList(sessionID string) ([]transport.OfficialCommand, error) {
	if err := a.agentEnsureSession(sessionID); err != nil {
		return nil, err
	}
	registry, err := a.commandRegistry()
	if err != nil {
		return nil, err
	}
	return registry.List(), nil
}

// commandsExecute 走真实注册表执行一条命令（官方 `commands.execute`）。
//
// 返回 (nil, nil) 表示**语法/名字不解析**（官方 `undefined`）：没有进入
// handler，也没有写任何生命周期帧。
func (a *agentOfficialAPI) commandsExecute(sessionID, line string, attachments []transport.OfficialSubmittedAttachment) (*transport.OfficialCommandExecution, error) {
	if a.deps.src == nil {
		return nil, fmt.Errorf("%w: 宿主没有接会话来源", transport.ErrOfficialNotImplemented)
	}
	if err := a.agentEnsureSession(sessionID); err != nil {
		return nil, err
	}
	registry, err := a.commandRegistry()
	if err != nil {
		return nil, err
	}
	return registry.Execute(context.Background(), sessionID, line, attachments)
}

// ---------------------------------------------------------------------------
// subagents.*
// ---------------------------------------------------------------------------

// childRegistry 懒建"可续问子会话"的真实地址表。
//
// 真实能力住在 `serve_subagents_api.go`：子会话是 durable v4 会话、父子关系
// 落 `subagent/spawn` 帧、投递走宿主 prompt 通道、回执落 `subagent/message` 帧。
func (a *agentOfficialAPI) childRegistry() *transport.OfficialChildRegistry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.children == nil {
		a.children = newHostChildRegistry(a.deps)
	}
	return a.children
}

// subagentsPrompt 把一条浏览器消息真的投进子会话 inbox（官方 `subagents.prompt`）。
//
// 没有登记过的子会话 → no_such_session；父 id 不匹配 → cross_session 403。
// **绝不**在没有真实子会话时回一个编的 messageId。
func (a *agentOfficialAPI) subagentsPrompt(req transport.OfficialSubagentPromptRequest) (transport.OfficialSubagentPromptReceipt, error) {
	if err := a.agentEnsureSession(req.ParentSessionID); err != nil {
		return transport.OfficialSubagentPromptReceipt{}, err
	}
	// 子代理工具在 `dshd run` 子进程里 spawn，本进程只认盘上的父子帧
	// （见 serve_subagents_api.go 的 adoptChild）。
	a.adoptChild(req.ChildSessionID)
	return a.childRegistry().Prompt(context.Background(), req)
}

// subagentsInterruptByParent 按父授权中断子会话当前轮（官方 `interruptByParent`）。
func (a *agentOfficialAPI) subagentsInterruptByParent(childSessionID, parentSessionID string) (transport.OfficialAccepted, error) {
	if err := a.agentEnsureSession(parentSessionID); err != nil {
		return transport.OfficialAccepted{}, err
	}
	a.adoptChild(childSessionID)
	return a.childRegistry().InterruptByParent(childSessionID, parentSessionID)
}

// ---------------------------------------------------------------------------
// userQuestions.*
// ---------------------------------------------------------------------------

// agentOpenQuestionCall 判断一个 ask_user_question 调用是否仍然"开着"。
//
// 真实依据是 durable 日志：一条 `tool/result` 带着该 callId、工具是
// ask_user_question、structured 里 awaiting_answer=true；并且它之后**没有**
// 任何 user/message（本宿主里 ask 结束本轮，人的下一条用户消息就是回答，
// 也就把这个问题关掉了）。
func (a *agentOfficialAPI) agentOpenQuestionCall(sessionID, callID string) (bool, error) {
	events, err := a.agentReadEvents(sessionID)
	if err != nil {
		return false, err
	}
	calls := indexToolCalls(events)
	info, isAsk := calls[callID]
	if !isAsk || info.name != "ask_user_question" {
		return false, fmt.Errorf("%w: 会话 %s 里没有 callId=%s 的 ask_user_question 调用",
			transport.ErrOfficialBadInput, sessionID, callID)
	}
	resultIndex := -1
	for i, ev := range events {
		if ev.Type != "tool/result" {
			continue
		}
		var d struct {
			Message struct {
				ToolCallID string `json:"toolCallId"`
			} `json:"message"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			continue
		}
		if d.Message.ToolCallID == callID {
			resultIndex = i
			break
		}
	}
	if resultIndex < 0 {
		return false, fmt.Errorf("%w: callId=%s 还没有 tool/result（问题还没问出来）",
			transport.ErrOfficialBadInput, callID)
	}
	for _, ev := range events[resultIndex+1:] {
		if ev.Type == "user/message" {
			return false, nil
		}
	}
	return true, nil
}

// userQuestionsAnswer 把答案作为**真实用户消息**送进会话，并回报问题是否还开着。
func (a *agentOfficialAPI) userQuestionsAnswer(sessionID, callID string, answer transport.OfficialQuestionAnswer) (bool, error) {
	if err := a.agentEnsureSession(sessionID); err != nil {
		return false, err
	}
	if a.deps.submit == nil {
		return false, fmt.Errorf("%w: 宿主没有把答案送进会话的 prompt 通道", transport.ErrOfficialNotImplemented)
	}
	open, err := a.agentOpenQuestionCall(sessionID, callID)
	if err != nil {
		return false, err
	}
	if !open {
		// 官方：答案在问题已经关掉之后到达，不投递、回 false（不是错误）。
		return false, nil
	}
	text := agentRenderQuestionAnswer(answer)
	if text == "" {
		return false, fmt.Errorf("%w: 答案渲染成空串", transport.ErrOfficialBadInput)
	}
	if _, err := a.deps.submit(sessionID, text); err != nil {
		return false, fmt.Errorf("把答案送进会话失败: %w", err)
	}
	return true, nil
}

// agentRenderQuestionAnswer 把结构化答案渲染成一条可读的用户消息。
func agentRenderQuestionAnswer(answer transport.OfficialQuestionAnswer) string {
	var b strings.Builder
	for _, item := range answer.Answers {
		b.WriteString("回答 ")
		b.WriteString(item.ID)
		b.WriteString("：")
		switch {
		case len(item.Selected) > 0 && strings.TrimSpace(item.Custom) != "":
			b.WriteString(strings.Join(item.Selected, "、"))
			b.WriteString("（补充：")
			b.WriteString(strings.TrimSpace(item.Custom))
			b.WriteString("）")
		case len(item.Selected) > 0:
			b.WriteString(strings.Join(item.Selected, "、"))
		case strings.TrimSpace(item.Custom) != "":
			b.WriteString(strings.TrimSpace(item.Custom))
		default:
			b.WriteString("（没有选择）")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// userQuestionsAttachWait 回一帧**真实**剩余时长（官方 `attachWait`，stream）。
//
// 真实来源见 `serve_questions_api.go`：deadline = durable `tool/result` 帧时间
// + 宿主前台等待窗口。返回 (nil, nil) 表示"不发帧"（wait 已结束）。
func (a *agentOfficialAPI) userQuestionsAttachWait(sessionID, callID string) (*transport.OfficialQuestionWait, error) {
	return a.userQuestionsAttachWaitHost(sessionID, callID)
}

// ---------------------------------------------------------------------------
// agentPresets.*
// ---------------------------------------------------------------------------

// agentPresetRoster 汇总内置模式与用户自建模式（`<home>/modes/*.toml`）。
func (a *agentOfficialAPI) agentPresetRoster() ([]modes.Mode, error) {
	out := append([]modes.Mode{}, modes.Builtins()...)
	if a.deps.home != "" {
		user, err := modes.LoadDir(filepath.Join(a.deps.home, "modes"))
		if err != nil {
			return nil, err
		}
		out = append(out, user...)
	}
	return out, nil
}

// agentPresetRead 返回一个 preset 的真实声明正文。
//
// 用户自建模式回**磁盘上的 TOML 原文**；内置模式回它编译期声明的字段。
// 官方这里的 content 是"子插件 entry-list YAML"——本仓的 preset 是
// "工具可见性模式"，不是插件清单，所以正文形式就是 TOML/内置声明（已登记差异）。
func (a *agentOfficialAPI) agentPresetRead(presetID string) (transport.OfficialAgentPresetDocument, error) {
	roster, err := a.agentPresetRoster()
	if err != nil {
		return transport.OfficialAgentPresetDocument{}, err
	}
	var found *modes.Mode
	for i := range roster {
		if roster[i].ID == presetID {
			found = &roster[i]
			break
		}
	}
	if found == nil {
		return transport.OfficialAgentPresetDocument{}, fmt.Errorf("%w: 没有 id=%q 的 preset（内置：%s）",
			transport.ErrOfficialBadInput, presetID, modes.IDs())
	}
	doc := transport.OfficialAgentPresetDocument{
		AgentPreset: found.ID, Name: found.Name, Description: found.Description,
	}
	if !found.Builtin {
		raw, err := a.agentUserPresetFile(presetID)
		if err != nil {
			return transport.OfficialAgentPresetDocument{}, err
		}
		doc.Content = raw
		return doc, nil
	}
	// 内置模式：把编译期声明原样列出来（不编造不存在的字段）。
	var b strings.Builder
	b.WriteString("id: " + found.ID + "\n")
	b.WriteString("name: " + found.Name + "\n")
	b.WriteString("description: " + found.Description + "\n")
	b.WriteString("builtin: true\n")
	if len(found.Tools) == 0 {
		b.WriteString("tools: []  # 空 = 全部工具可见\n")
	} else {
		b.WriteString("tools:\n")
		for _, t := range found.Tools {
			b.WriteString("  - " + t + "\n")
		}
	}
	if found.Approval != "" {
		b.WriteString("approval: " + found.Approval + "\n")
	}
	doc.Content = b.String()
	return doc, nil
}

// agentUserPresetFile 在 `<home>/modes` 里找到声明了这个 id 的 toml 原文。
func (a *agentOfficialAPI) agentUserPresetFile(presetID string) (string, error) {
	dir := filepath.Join(a.deps.home, "modes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("%w: 读 preset 目录 %s: %v", transport.ErrOfficialBadInput, dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("读 %s: %w", path, err)
		}
		var head struct {
			ID string `toml:"id"`
		}
		if err := toml.Unmarshal(raw, &head); err != nil {
			return "", fmt.Errorf("解析 %s: %w", path, err)
		}
		if head.ID == presetID {
			return string(raw), nil
		}
	}
	return "", fmt.Errorf("%w: preset %q 没有磁盘声明（不是用户自建模式）", transport.ErrOfficialBadInput, presetID)
}

// agentPresetStatePath 是"这个会话选了哪个 preset"的持久覆盖。
//
// 与 `state/session-model.json`、`state/session-permission.json` 同构：
// v4 append-only（R5）不能改 header，所以选择落在 state 覆盖里，重启可读。
func (a *agentOfficialAPI) agentPresetStatePath() string {
	return filepath.Join(a.deps.home, "state", "session-preset.json")
}

type agentPresetDocument struct {
	Version  int               `json:"version"`
	Sessions map[string]string `json:"sessions"`
}

func (a *agentOfficialAPI) agentLoadPresetState() (agentPresetDocument, error) {
	var doc agentPresetDocument
	found, err := agentReadDoc(a.agentPresetStatePath(), &doc)
	if err != nil {
		return agentPresetDocument{}, fmt.Errorf("%w: %v", transport.ErrStatePersistence, err)
	}
	if !found {
		return agentPresetDocument{Version: 1, Sessions: map[string]string{}}, nil
	}
	if doc.Version != 1 || doc.Sessions == nil {
		return agentPresetDocument{}, fmt.Errorf("%w: %s version/sessions 非法",
			transport.ErrStatePersistence, a.agentPresetStatePath())
	}
	return doc, nil
}

// agentSessionPreset 读会话当前选中的 preset（没有覆盖时回默认 preset）。
func (a *agentOfficialAPI) agentSessionPreset(sessionID string) (string, error) {
	doc, err := a.agentLoadPresetState()
	if err != nil {
		return "", err
	}
	if value, ok := doc.Sessions[sessionID]; ok && value != "" {
		return value, nil
	}
	return modes.DefaultID, nil
}

func (a *agentOfficialAPI) agentPresetSelect(sessionID, presetID string) (string, error) {
	if err := a.agentEnsureSession(sessionID); err != nil {
		return "", err
	}
	// preset 必须存在于真实清单里（内置 + `<home>/modes/*.toml`）；
	// modes.Resolve 只看内置，用它校验会把用户自建模式全部拒掉。
	roster, rosterErr := a.agentPresetRoster()
	if rosterErr != nil {
		return "", rosterErr
	}
	var mode modes.Mode
	var found bool
	for _, candidate := range roster {
		if candidate.ID == presetID {
			mode, found = candidate, true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("%w: 没有 id=%q 的 preset（可选：%s）",
			transport.ErrOfficialBadInput, presetID, agentPresetIDs(roster))
	}
	doc, err := a.agentLoadPresetState()
	if err != nil {
		return "", err
	}
	doc.Sessions[sessionID] = mode.ID
	if err := agentWriteDoc(a.agentPresetStatePath(), doc); err != nil {
		return "", fmt.Errorf("%w: 写 %s: %v", transport.ErrStatePersistence, a.agentPresetStatePath(), err)
	}
	return mode.ID, nil
}

// agentPresetIDs 把可选 preset id 排成一行，给"未知 preset"的错误用。
func agentPresetIDs(roster []modes.Mode) string {
	ids := make([]string, 0, len(roster))
	for _, mode := range roster {
		ids = append(ids, mode.ID)
	}
	return strings.Join(ids, ", ")
}

// ---------------------------------------------------------------------------
// skills.list
// ---------------------------------------------------------------------------

// agentSkillRoots 与 `skill.Module(filepath.Join(home, "skills"))` 的查找顺序一致：
// 主目录在前，`DSHD_SKILLS_EXTRA`（`;` 分隔）追加在后。
func (a *agentOfficialAPI) agentSkillRoots() []string {
	var out []string
	if a.deps.home != "" {
		out = append(out, filepath.Join(a.deps.home, "skills"))
	}
	for _, p := range strings.Split(os.Getenv("DSHD_SKILLS_EXTRA"), ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (a *agentOfficialAPI) skillsList(sessionID string) (transport.OfficialSkillCatalog, error) {
	if err := a.agentEnsureSession(sessionID); err != nil {
		return transport.OfficialSkillCatalog{}, err
	}
	// modelInvocable 的真实判据：当前 preset 的工具表里还有没有 `skill` 工具。
	// preset 选择落在 state 覆盖里；读不到就按默认 preset 算。
	modelInvocable := true
	if preset, err := a.agentSessionPreset(sessionID); err == nil {
		if mode, ok := modes.Lookup(preset); ok {
			modelInvocable = mode.Allows("skill")
		}
	}
	seen := map[string]bool{}
	catalog := transport.OfficialSkillCatalog{Skills: []transport.OfficialSkill{}}
	for _, root := range a.agentSkillRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return transport.OfficialSkillCatalog{}, fmt.Errorf("读 skill 目录 %s: %w", root, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || seen[entry.Name()] {
				continue
			}
			path := filepath.Join(root, entry.Name(), "SKILL.md")
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			seen[entry.Name()] = true
			meta := agentSkillFrontmatter(path)
			catalog.Skills = append(catalog.Skills, transport.OfficialSkill{
				Path: path, Name: entry.Name(),
				Description: meta["description"], WhenToUse: meta["when_to_use"],
				ModelInvocable: modelInvocable,
			})
		}
	}
	sort.Slice(catalog.Skills, func(i, j int) bool { return catalog.Skills[i].Name < catalog.Skills[j].Name })
	return catalog, nil
}

// agentSkillFrontmatter 只读 SKILL.md 顶部的 `---` 块。取不到就是空串，不猜。
func agentSkillFrontmatter(path string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.SplitN(text, "\n", 60)
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return out
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key != "" && value != "" {
			out[key] = value
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// fileReferences.list
// ---------------------------------------------------------------------------

func (a *agentOfficialAPI) fileReferencesList(sessionID, query string) ([]transport.OfficialFileReference, error) {
	if a.deps.src == nil {
		return nil, fmt.Errorf("%w: 宿主没有接会话来源", transport.ErrOfficialNotImplemented)
	}
	cwd, err := a.deps.src.Cwd(sessionID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cwd) == "" {
		return nil, fmt.Errorf("%w: 会话 %s 没有工作目录，列不出引用候选",
			transport.ErrOfficialNotImplemented, sessionID)
	}
	// 官方：query 是 `@` 或 `@"` 之后的路径文本。
	text := strings.TrimPrefix(query, `"`)
	dirPart, prefix := splitAgentReferenceQuery(text)

	base := cwd
	if dirPart != "" {
		base = filepath.FromSlash(dirPart)
		if !filepath.IsAbs(base) {
			base = filepath.Join(cwd, base)
		}
	}
	base = filepath.Clean(base)
	if !filepath.IsAbs(base) {
		return nil, fmt.Errorf("%w: 引用路径 %q 解析后不是绝对路径", transport.ErrOfficialBadInput, query)
	}
	if rel, relErr := filepath.Rel(cwd, base); relErr == nil && strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("%w: 引用路径 %q 逃出工作目录 %s", transport.ErrOfficialBadInput, query, cwd)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return []transport.OfficialFileReference{}, nil
		}
		return nil, fmt.Errorf("列目录 %s: %w", base, err)
	}
	lower := strings.ToLower(prefix)
	out := make([]transport.OfficialFileReference, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if lower != "" && !strings.HasPrefix(strings.ToLower(name), lower) {
			continue
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		}
		out = append(out, transport.OfficialFileReference{
			Path: agentJoinReference(dirPart, name), Kind: kind,
		})
		if len(out) >= agentReferenceLimit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// splitAgentReferenceQuery 把 `src/han` 拆成 ("src/", "han")；没有分隔符时目录为空。
func splitAgentReferenceQuery(text string) (string, string) {
	if idx := strings.LastIndexAny(text, `/\`); idx >= 0 {
		return text[:idx+1], text[idx+1:]
	}
	return "", text
}

// agentJoinReference 拼回用户视角的路径（统一用 `/`，Windows 的提示框也认）。
func agentJoinReference(dirPart, name string) string {
	if dirPart == "" {
		return name
	}
	return strings.TrimSuffix(strings.ReplaceAll(dirPart, `\`, `/`), "/") + "/" + name
}

// ---------------------------------------------------------------------------
// fileUploads.upload
// ---------------------------------------------------------------------------

func (a *agentOfficialAPI) agentUploadDir() string {
	return filepath.Join(a.deps.home, "state", "uploads")
}

// agentUploadName 把显示名洗成一个安全的叶子名（**从不当作路径**）。
func agentUploadName(raw string) string {
	name := strings.TrimSpace(raw)
	if idx := strings.LastIndexAny(name, `/\`); idx >= 0 {
		name = name[idx+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case strings.ContainsRune(`<>:"|?*`, r):
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "upload"
	}
	if len([]rune(name)) > 100 {
		name = string([]rune(name)[:100])
	}
	return name
}

func (a *agentOfficialAPI) fileUploadsUpload(sessionID string, req transport.OfficialFileUploadRequest) (transport.OfficialFileUploadValue, error) {
	if err := a.agentEnsureSession(sessionID); err != nil {
		return transport.OfficialFileUploadValue{}, err
	}
	if a.deps.home == "" {
		return transport.OfficialFileUploadValue{}, fmt.Errorf("%w: 宿主没有 home，无处落盘",
			transport.ErrOfficialNotImplemented)
	}
	// base64：只认标准编码；缺 padding 的写法也接受（很多客户端会去掉）。
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(req.Data)
		if err != nil {
			return transport.OfficialFileUploadValue{}, fmt.Errorf("%w: data 不是合法 base64", transport.ErrOfficialBadInput)
		}
	}
	if len(data) == 0 {
		return transport.OfficialFileUploadValue{}, fmt.Errorf("%w: 解码后是 0 字节", transport.ErrOfficialBadInput)
	}
	if len(data) > agentUploadMaxBytes {
		return transport.OfficialFileUploadValue{}, fmt.Errorf("%w: 文件 %d 字节，超过上限 %d",
			transport.ErrOfficialBadInput, len(data), agentUploadMaxBytes)
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	dir := a.agentUploadDir()
	blobPath := filepath.Join(dir, "blobs", digest)
	if _, statErr := os.Stat(blobPath); statErr != nil {
		if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
			return transport.OfficialFileUploadValue{}, fmt.Errorf("%w: 建上传目录: %v", transport.ErrStatePersistence, err)
		}
		if err := os.WriteFile(blobPath, data, 0o600); err != nil {
			return transport.OfficialFileUploadValue{}, fmt.Errorf("%w: 写上传文件: %v", transport.ErrStatePersistence, err)
		}
	}

	receipt, err := serveID()
	if err != nil {
		return transport.OfficialFileUploadValue{}, fmt.Errorf("%w: 生成上传回执: %v", transport.ErrStatePersistence, err)
	}
	receiptID := "rcpt-" + receipt
	name := agentUploadName(req.Name)
	record := struct {
		Version    int    `json:"version"`
		ReceiptID  string `json:"receiptId"`
		SessionID  string `json:"sessionId"`
		Name       string `json:"name"`
		Bytes      int64  `json:"bytes"`
		SHA256     string `json:"sha256"`
		Attachment string `json:"attachmentId"`
		CreatedAt  int64  `json:"createdAt"`
	}{
		Version: 1, ReceiptID: receiptID, SessionID: sessionID, Name: name,
		Bytes: int64(len(data)), SHA256: digest, Attachment: digest,
		CreatedAt: time.Now().UnixMilli(),
	}
	// 回执记录带**接收方会话**：官方的回执只在它那个 Agent 作用域里有效。
	if err := agentWriteDoc(filepath.Join(dir, receiptID+".json"), record); err != nil {
		return transport.OfficialFileUploadValue{}, fmt.Errorf("%w: 写上传回执: %v", transport.ErrStatePersistence, err)
	}
	return transport.OfficialFileUploadValue{
		ReceiptID: receiptID,
		File: transport.OfficialUploadedFile{
			AttachmentID: digest, Name: name, Bytes: int64(len(data)),
		},
	}, nil
}

// ---------------------------------------------------------------------------
// messageFeedback.*
// ---------------------------------------------------------------------------

// agentFeedbackDocument 是消息反馈的持久文档（按会话 + 消息 id 存）。
type agentFeedbackDocument struct {
	Version  int                                                         `json:"version"`
	Sessions map[string]map[string]transport.OfficialMessageFeedbackItem `json:"sessions"`
}

func (a *agentOfficialAPI) agentFeedbackPath() string {
	return filepath.Join(a.deps.home, "state", "message-feedback.json")
}

func (a *agentOfficialAPI) agentLoadFeedback() (agentFeedbackDocument, error) {
	var doc agentFeedbackDocument
	found, err := agentReadDoc(a.agentFeedbackPath(), &doc)
	if err != nil {
		return agentFeedbackDocument{}, fmt.Errorf("%w: %v", transport.ErrStatePersistence, err)
	}
	if !found {
		return agentFeedbackDocument{Version: 1, Sessions: map[string]map[string]transport.OfficialMessageFeedbackItem{}}, nil
	}
	if doc.Version != 1 || doc.Sessions == nil {
		return agentFeedbackDocument{}, fmt.Errorf("%w: %s version/sessions 非法",
			transport.ErrStatePersistence, a.agentFeedbackPath())
	}
	for sessionID, items := range doc.Sessions {
		for messageID, item := range items {
			if sessionID == "" || messageID == "" || item.Version == "" {
				return agentFeedbackDocument{}, fmt.Errorf("%w: %s 含空会话/消息/版本",
					transport.ErrStatePersistence, a.agentFeedbackPath())
			}
			if item.MessageID != messageID {
				return agentFeedbackDocument{}, fmt.Errorf("%w: %s 里键 %q 与 item.messageId %q 不一致",
					transport.ErrStatePersistence, a.agentFeedbackPath(), messageID, item.MessageID)
			}
		}
	}
	return doc, nil
}

func (a *agentOfficialAPI) messageFeedbackList(sessionID string) (transport.OfficialMessageFeedbackListResult, error) {
	if !a.agentSessionExists(sessionID) {
		return transport.OfficialMessageFeedbackListResult{
			OK: false, Error: &transport.OfficialFeedbackFailure{Code: "session-not-found", SessionID: sessionID},
		}, nil
	}
	doc, err := a.agentLoadFeedback()
	if err != nil {
		return transport.OfficialMessageFeedbackListResult{}, err
	}
	items := []transport.OfficialMessageFeedbackItem{}
	for _, item := range doc.Sessions[sessionID] {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].MessageID < items[j].MessageID })
	return transport.OfficialMessageFeedbackListResult{
		OK: true, Value: &transport.OfficialMessageFeedbackValue{Items: items},
	}, nil
}

// agentNewFeedbackVersion 生成一个只用于相等比较的版本令牌。
func agentNewFeedbackVersion() (string, error) {
	raw, err := serveID()
	if err != nil {
		return "", err
	}
	return "fb-" + strings.ReplaceAll(raw, "-", ""), nil
}

func (a *agentOfficialAPI) messageFeedbackPut(req transport.OfficialMessageFeedbackPutRequest) (transport.OfficialMessageFeedbackPutResult, error) {
	if !a.agentSessionExists(req.SessionID) {
		return transport.OfficialMessageFeedbackPutResult{
			OK: false, Error: &transport.OfficialFeedbackFailure{Code: "session-not-found", SessionID: req.SessionID},
		}, nil
	}
	if strings.TrimSpace(req.Note) == "" && req.Note != "" {
		return transport.OfficialMessageFeedbackPutResult{
			OK: false, Error: &transport.OfficialFeedbackFailure{Code: "note-blank"},
		}, nil
	}
	if len(req.Note) > agentFeedbackNoteMaxBytes {
		maxBytes, actual := int64(agentFeedbackNoteMaxBytes), int64(len(req.Note))
		return transport.OfficialMessageFeedbackPutResult{
			OK: false, Error: &transport.OfficialFeedbackFailure{
				Code: "note-too-large", MaxBytes: &maxBytes, ActualBytes: &actual,
			},
		}, nil
	}

	doc, err := a.agentLoadFeedback()
	if err != nil {
		return transport.OfficialMessageFeedbackPutResult{}, err
	}
	if doc.Sessions[req.SessionID] == nil {
		doc.Sessions[req.SessionID] = map[string]transport.OfficialMessageFeedbackItem{}
	}
	current, exists := doc.Sessions[req.SessionID][req.MessageID]

	// 乐观并发：ifVersion=null 要求"当前不存在"；否则要求版本相等。
	if req.IfVersion == nil {
		if exists {
			return agentFeedbackConflict(current, true), nil
		}
	} else if !exists || current.Version != *req.IfVersion {
		return agentFeedbackConflict(current, exists), nil
	}

	now := time.Now().UnixMilli()
	item := transport.OfficialMessageFeedbackItem{
		MessageID: req.MessageID, Rating: req.Rating, Note: req.Note,
		Category: req.Category, CreatedAt: now, UpdatedAt: now,
	}
	if exists {
		// 完全相同的写入是 no-op：版本与 updatedAt 都不动（官方语义）。
		if current.Rating == req.Rating && current.Note == req.Note && current.Category == req.Category {
			return transport.OfficialMessageFeedbackPutResult{OK: true, Value: &current}, nil
		}
		item.CreatedAt = current.CreatedAt
		item.UpdatedAt = now
		if item.UpdatedAt < current.UpdatedAt {
			item.UpdatedAt = current.UpdatedAt
		}
	}
	version, err := agentNewFeedbackVersion()
	if err != nil {
		return transport.OfficialMessageFeedbackPutResult{}, fmt.Errorf("%w: %v", transport.ErrStatePersistence, err)
	}
	item.Version = version
	doc.Sessions[req.SessionID][req.MessageID] = item
	if err := agentWriteDoc(a.agentFeedbackPath(), doc); err != nil {
		return transport.OfficialMessageFeedbackPutResult{}, fmt.Errorf("%w: 写消息反馈: %v", transport.ErrStatePersistence, err)
	}
	return transport.OfficialMessageFeedbackPutResult{OK: true, Value: &item}, nil
}

// agentFeedbackConflict 造一个带 current 的 version-conflict。
// current 为 nil 时必须显式写成 `null`（官方 union 里它是 null|item）。
func agentFeedbackConflict(current transport.OfficialMessageFeedbackItem, exists bool) transport.OfficialMessageFeedbackPutResult {
	failure := &transport.OfficialFeedbackFailure{Code: "version-conflict"}
	if exists {
		raw, err := json.Marshal(current)
		if err == nil {
			failure.Current = raw
		}
	} else {
		failure.Current = json.RawMessage("null")
	}
	return transport.OfficialMessageFeedbackPutResult{OK: false, Error: failure}
}

func (a *agentOfficialAPI) messageFeedbackDelete(req transport.OfficialMessageFeedbackDeleteRequest) (transport.OfficialMessageFeedbackDeleteResult, error) {
	if !a.agentSessionExists(req.SessionID) {
		return transport.OfficialMessageFeedbackDeleteResult{
			OK: false, Error: &transport.OfficialFeedbackFailure{Code: "session-not-found", SessionID: req.SessionID},
		}, nil
	}
	doc, err := a.agentLoadFeedback()
	if err != nil {
		return transport.OfficialMessageFeedbackDeleteResult{}, err
	}
	current, exists := doc.Sessions[req.SessionID][req.MessageID]
	if !exists {
		// 官方：已经是"不存在"就是稳定后置条件，不报冲突。
		return transport.OfficialMessageFeedbackDeleteResult{
			OK: true, Value: &transport.OfficialMessageFeedbackDeleteValue{Absent: true},
		}, nil
	}
	if current.Version != req.IfVersion {
		raw, marshalErr := json.Marshal(current)
		failure := &transport.OfficialFeedbackFailure{Code: "version-conflict"}
		if marshalErr == nil {
			failure.Current = raw
		}
		return transport.OfficialMessageFeedbackDeleteResult{OK: false, Error: failure}, nil
	}
	delete(doc.Sessions[req.SessionID], req.MessageID)
	if len(doc.Sessions[req.SessionID]) == 0 {
		delete(doc.Sessions, req.SessionID)
	}
	if err := agentWriteDoc(a.agentFeedbackPath(), doc); err != nil {
		return transport.OfficialMessageFeedbackDeleteResult{}, fmt.Errorf("%w: 写消息反馈: %v", transport.ErrStatePersistence, err)
	}
	return transport.OfficialMessageFeedbackDeleteResult{
		OK: true, Value: &transport.OfficialMessageFeedbackDeleteValue{Absent: true},
	}, nil
}

// ---------------------------------------------------------------------------
// sessionFeedback.record
// ---------------------------------------------------------------------------

// agentSessionFeedbackDocument 是会话级反馈的追加日志。
type agentSessionFeedbackDocument struct {
	Version int                        `json:"version"`
	Items   []agentSessionFeedbackItem `json:"items"`
}

type agentSessionFeedbackItem struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Text      string `json:"text,omitempty"`
	Category  string `json:"category,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

func (a *agentOfficialAPI) agentSessionFeedbackPath() string {
	return filepath.Join(a.deps.home, "state", "session-feedback.json")
}

func (a *agentOfficialAPI) sessionFeedbackRecord(req transport.OfficialSessionFeedbackRequest) (transport.OfficialSessionFeedbackResult, error) {
	if !a.agentSessionExists(req.SessionID) {
		return transport.OfficialSessionFeedbackResult{
			OK: false, Error: &transport.OfficialFeedbackFailure{Code: "session-not-found", SessionID: req.SessionID},
		}, nil
	}
	var doc agentSessionFeedbackDocument
	found, err := agentReadDoc(a.agentSessionFeedbackPath(), &doc)
	if err != nil {
		return transport.OfficialSessionFeedbackResult{}, fmt.Errorf("%w: %v", transport.ErrStatePersistence, err)
	}
	if !found {
		doc = agentSessionFeedbackDocument{Version: 1}
	}
	if doc.Version != 1 {
		return transport.OfficialSessionFeedbackResult{}, fmt.Errorf("%w: %s version 非法",
			transport.ErrStatePersistence, a.agentSessionFeedbackPath())
	}
	id, err := serveID()
	if err != nil {
		return transport.OfficialSessionFeedbackResult{}, fmt.Errorf("%w: %v", transport.ErrStatePersistence, err)
	}
	doc.Items = append(doc.Items, agentSessionFeedbackItem{
		ID: "sf-" + id, SessionID: req.SessionID,
		Text: req.Text, Category: req.Category,
		CreatedAt: time.Now().UnixMilli(),
	})
	if err := agentWriteDoc(a.agentSessionFeedbackPath(), doc); err != nil {
		return transport.OfficialSessionFeedbackResult{}, fmt.Errorf("%w: 写会话反馈: %v", transport.ErrStatePersistence, err)
	}
	return transport.OfficialSessionFeedbackResult{
		OK: true, Value: &transport.OfficialSessionFeedbackRecorded{Recorded: true},
	}, nil
}

// ---------------------------------------------------------------------------
// sessionReferenceResolver.candidates
// ---------------------------------------------------------------------------

func (a *agentOfficialAPI) sessionReferenceCandidates(sessionID, query string) ([]transport.OfficialSessionReferenceCandidate, error) {
	if err := a.agentEnsureSession(sessionID); err != nil {
		return nil, err
	}
	selfCwd, _ := a.deps.src.Cwd(sessionID)
	listings, err := session.ListSessions(a.deps.home, "")
	if err != nil {
		return nil, fmt.Errorf("列会话: %w", err)
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	out := make([]transport.OfficialSessionReferenceCandidate, 0, len(listings))
	for _, listing := range listings {
		if listing.SessionID == sessionID || !listing.Readable {
			continue
		}
		label := listing.SessionID
		if listing.Title != nil && strings.TrimSpace(*listing.Title) != "" {
			label = strings.TrimSpace(*listing.Title)
		} else if strings.TrimSpace(listing.Summary) != "" {
			label = strings.TrimSpace(listing.Summary)
		}
		if needle != "" {
			haystack := strings.ToLower(listing.SessionID + " " + listing.Cwd + " " + label)
			if !strings.Contains(haystack, needle) {
				continue
			}
		}
		sameWorkspace := listing.Cwd != "" && selfCwd != "" && listing.Cwd == selfCwd
		out = append(out, transport.OfficialSessionReferenceCandidate{
			Mention:       "@[" + agentMentionLabel(label) + "](dsh-session:" + listing.SessionID + ")",
			SessionID:     listing.SessionID,
			Label:         label,
			DisplayTitle:  label,
			Cwd:           listing.Cwd,
			SameWorkspace: sameWorkspace,
			CreatedAt:     listing.Created,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SameWorkspace != out[j].SameWorkspace {
			return out[i].SameWorkspace
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	if len(out) > agentSessionReferenceLimit {
		out = out[:agentSessionReferenceLimit]
	}
	return out, nil
}

// agentMentionLabel 清掉 mention 里会破坏 `@[label](…)` 语法的字符。
func agentMentionLabel(label string) string {
	label = strings.ReplaceAll(label, "]", ")")
	label = strings.ReplaceAll(label, "[", "(")
	label = strings.ReplaceAll(label, "\n", " ")
	label = strings.TrimSpace(label)
	if label == "" {
		return "session"
	}
	runes := []rune(label)
	if len(runes) > 60 {
		return string(runes[:60])
	}
	return label
}

// ---------------------------------------------------------------------------
// toolResults.watch（L1：一次性读取，不假装流式）
// ---------------------------------------------------------------------------

func (a *agentOfficialAPI) toolResultsWatch(sessionID string, seq int64) (transport.OfficialToolResultWatch, error) {
	if err := a.agentEnsureSession(sessionID); err != nil {
		return transport.OfficialToolResultWatch{}, err
	}
	events, err := a.agentReadEvents(sessionID)
	if err != nil {
		return transport.OfficialToolResultWatch{}, err
	}
	for _, ev := range events {
		if ev.Seq != seq {
			continue
		}
		if ev.Type != "tool/result" {
			// 官方：cold read 取不到就回 available:false，不报错。
			return transport.OfficialToolResultWatch{Available: false}, nil
		}
		data, ok := loadToolResultOf(ev.Data)
		if !ok {
			return transport.OfficialToolResultWatch{Available: false}, nil
		}
		calls := indexToolCalls(events)
		value := map[string]any{
			"seq":     ev.Seq,
			"time":    ev.Time,
			"callId":  data.CallID,
			"isError": data.IsError,
			"text":    data.Text,
		}
		if info, has := calls[data.CallID]; has {
			value["tool"] = info.name
			value["arguments"] = info.args
		}
		if data.Turn != nil {
			value["turn"] = *data.Turn
		}
		if data.Step != nil {
			value["step"] = *data.Step
		}
		if len(data.Meta) > 0 {
			value["meta"] = data.Meta
		}
		if len(data.Structured) > 0 {
			value["structured"] = data.Structured
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return transport.OfficialToolResultWatch{}, fmt.Errorf("%w: 序列化工具结果: %v", transport.ErrStatePersistence, err)
		}
		return transport.OfficialToolResultWatch{Available: true, Value: raw}, nil
	}
	return transport.OfficialToolResultWatch{Available: false}, nil
}
