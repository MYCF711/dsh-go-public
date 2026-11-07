package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"dshgo/internal/transport"
)

const goalStoreVersion = 1

// goalRecord 把 snapshot 与 channel 持有的 roundsStarted 分开持久化。
// nil roundsStarted 是旧扁平文件迁移后的“未知”，不是伪造的 0。
type goalRecord struct {
	Goal          *transport.GoalSnapshot `json:"goal"`
	RoundsStarted *int                    `json:"roundsStarted,omitempty"`
}

type goalDocument struct {
	Version int                   `json:"version"`
	Goals   map[string]goalRecord `json:"goals"`
}

// goalChangeJournal 是唯一由真实状态迁移调用的 goal/change 写入口。
type goalChangeJournal struct {
	journals *transport.Journals
	pub      func(topic string, frame transport.ServerFrame)
}

func (j *goalChangeJournal) append(sessionID, operation string, record goalRecord) error {
	if j == nil || j.journals == nil {
		return fmt.Errorf("%w: goal/change journal 未接线", transport.ErrStatePersistence)
	}
	data, err := json.Marshal(transport.GoalChange{
		Kind: "goal/change", Version: 1, Operation: operation,
		Goal: cloneGoalSnapshot(record.Goal), RoundsStarted: cloneInt(record.RoundsStarted),
	})
	if err != nil {
		return fmt.Errorf("%w: 序列化 goal/change: %v", transport.ErrStatePersistence, err)
	}
	if _, err := emitFrameResult(j.journals, j.pub, sessionID, "goal/change", data); err != nil {
		return fmt.Errorf("%w: 追加 goal/change: %v", transport.ErrStatePersistence, err)
	}
	return nil
}

// goalOverrideStore 保留旧类型名与 Get/Set 调用面，但文件与内存已是真实 snapshot。
// 所有迁移串行；state 与 journal 任一提交失败都不会返回成功 ACK。
type goalOverrideStore struct {
	mu      sync.Mutex
	path    string
	m       map[string]goalRecord
	loadErr error
	journal *goalChangeJournal
	newID   func() (string, error)
}

func newGoalOverrideStore(home string) *goalOverrideStore {
	return &goalOverrideStore{
		path: filepath.Join(home, "state", "session-goal.json"),
		m:    map[string]goalRecord{},
		newID: func() (string, error) {
			id, err := serveID()
			if err != nil {
				return "", err
			}
			return "goal-" + id, nil
		},
	}
}

func (s *goalOverrideStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.loadErr = nil
			return nil
		}
		s.loadErr = fmt.Errorf("读会话 goal store %s: %w", s.path, err)
		return s.loadErr
	}

	// 旧格式是 map[sessionID]objective。迁移在 Load 内同步落盘，保证真实
	// goal id/revision 跨 restart 稳定；迁移不是一次 UI change，不发事件。
	var legacy map[string]string
	if err := json.Unmarshal(raw, &legacy); err == nil {
		if legacy == nil {
			s.loadErr = fmt.Errorf("旧 goal store %s 顶层是 null，不是 map", s.path)
			return s.loadErr
		}
		next := make(map[string]goalRecord, len(legacy))
		sessionIDs := make([]string, 0, len(legacy))
		for sessionID := range legacy {
			sessionIDs = append(sessionIDs, sessionID)
		}
		sort.Strings(sessionIDs) // 迁移 id 分配可重放，不受 Go map 遍历随机性影响。
		for _, sessionID := range sessionIDs {
			objective := legacy[sessionID]
			if sessionID == "" || objective == "" {
				s.loadErr = fmt.Errorf("旧 goal store %s 含空 session id/objective", s.path)
				return s.loadErr
			}
			id, err := s.newID()
			if err != nil {
				s.loadErr = fmt.Errorf("迁移旧 goal store 生成 id: %w", err)
				return s.loadErr
			}
			next[sessionID] = goalRecord{Goal: &transport.GoalSnapshot{
				ID: id, Revision: 1, Objective: objective, Phase: "active", MaxGoalRounds: 0,
			}}
		}
		if err := writeGoalDocument(s.path, goalDocument{Version: goalStoreVersion, Goals: next}); err != nil {
			s.loadErr = fmt.Errorf("迁移旧 goal store %s 落盘失败: %w", s.path, err)
			return s.loadErr
		}
		s.m = next
		s.loadErr = nil
		return nil
	}

	var doc goalDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		s.loadErr = fmt.Errorf("解析 goal store %s: %w", s.path, err)
		return s.loadErr
	}
	if doc.Version != goalStoreVersion || doc.Goals == nil {
		s.loadErr = fmt.Errorf("goal store %s version/goals 非法：version=%d", s.path, doc.Version)
		return s.loadErr
	}
	for sessionID, record := range doc.Goals {
		if err := validateGoalRecord(sessionID, record); err != nil {
			s.loadErr = fmt.Errorf("goal store %s: %w", s.path, err)
			return s.loadErr
		}
	}
	s.m = cloneGoalMap(doc.Goals)
	s.loadErr = nil
	return nil
}

func validateGoalRecord(sessionID string, record goalRecord) error {
	if sessionID == "" || record.Goal == nil {
		return fmt.Errorf("session %q 缺 goal snapshot", sessionID)
	}
	goal := record.Goal
	if goal.ID == "" || goal.Revision <= 0 || goal.Objective == "" || goal.MaxGoalRounds < 0 {
		return fmt.Errorf("session %q goal 标识/revision/objective/maxGoalRounds 非法", sessionID)
	}
	switch goal.Phase {
	case "active", "paused", "complete":
		if goal.BlockedReason != nil {
			return fmt.Errorf("session %q 非 blocked phase 不得带 blockedReason", sessionID)
		}
	case "blocked":
		if goal.BlockedReason == nil || goal.BlockedReason.Code == "" || goal.BlockedReason.Message == "" {
			return fmt.Errorf("session %q blocked goal 缺真实 blockedReason", sessionID)
		}
	default:
		return fmt.Errorf("session %q goal phase=%q 非法", sessionID, goal.Phase)
	}
	if record.RoundsStarted != nil && *record.RoundsStarted < 0 {
		return fmt.Errorf("session %q roundsStarted 为负数", sessionID)
	}
	if goal.Budget != nil {
		if goal.Budget.TokensUsed < 0 || goal.Budget.TimeUsedSeconds < 0 ||
			(goal.Budget.TokenBudget != nil && *goal.Budget.TokenBudget < 0) {
			return fmt.Errorf("session %q budget 含负数", sessionID)
		}
	}
	return nil
}

// Get 保留 session.load 的扁平投影；goal.get 使用下面的 Snapshot，绝不反造字段。
func (s *goalOverrideStore) Get(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil || s.m[sessionID].Goal == nil {
		return ""
	}
	return s.m[sessionID].Goal.Objective
}

func (s *goalOverrideStore) Snapshot(sessionID string) (transport.GoalResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return transport.GoalResult{}, fmt.Errorf("%w: %v", transport.ErrStatePersistence, s.loadErr)
	}
	record, exists := s.m[sessionID]
	if !exists {
		return transport.GoalResult{Goal: nil}, nil
	}
	return transport.GoalResult{Goal: cloneGoalSnapshot(record.Goal), RoundsStarted: cloneInt(record.RoundsStarted)}, nil
}

// Set 设 goal；空串 = clear。首次 create，替换 edit，原生 id/max/rounds 保留。
func (s *goalOverrideStore) Set(sessionID, objective string) error {
	return s.transition(sessionID, "set", objective)
}

func (s *goalOverrideStore) Pause(sessionID string) error {
	return s.transition(sessionID, "pause", "")
}

func (s *goalOverrideStore) Resume(sessionID string) error {
	return s.transition(sessionID, "resume", "")
}

func (s *goalOverrideStore) transition(sessionID, action, objective string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id 为空", transport.ErrGoalTransition)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("%w: %v", transport.ErrStatePersistence, s.loadErr)
	}
	old := cloneGoalMap(s.m)
	current, exists := old[sessionID]
	if action == "set" && objective == "" && !exists {
		return nil // 旧 setGoal(clear) 幂等：无迁移就不写 state、不发 change。
	}
	if s.journal == nil {
		return fmt.Errorf("%w: goal/change journal 未接线", transport.ErrStatePersistence)
	}
	nextRecord := cloneGoalRecord(current)
	operation := action

	switch action {
	case "set":
		if objective == "" {
			operation = "clear"
			nextRecord = goalRecord{Goal: nil, RoundsStarted: cloneInt(current.RoundsStarted)}
		} else if !exists {
			operation = "create"
			id, err := s.newID()
			if err != nil {
				return fmt.Errorf("%w: 生成 goal id: %v", transport.ErrStatePersistence, err)
			}
			zero := 0
			nextRecord = goalRecord{Goal: &transport.GoalSnapshot{
				ID: id, Revision: 1, Objective: objective, Phase: "active", MaxGoalRounds: 0,
			}, RoundsStarted: &zero}
		} else {
			operation = "edit"
			if current.Goal.Revision == math.MaxInt64 {
				return fmt.Errorf("%w: goal revision 已溢出", transport.ErrGoalTransition)
			}
			nextRecord.Goal = cloneGoalSnapshot(current.Goal)
			nextRecord.Goal.Revision++
			nextRecord.Goal.Objective = objective
		}
	case "pause":
		if !exists || current.Goal.Phase != "active" {
			return fmt.Errorf("%w: 只有 active goal 才能 pause", transport.ErrGoalTransition)
		}
		if current.Goal.Revision == math.MaxInt64 {
			return fmt.Errorf("%w: goal revision 已溢出", transport.ErrGoalTransition)
		}
		nextRecord.Goal = cloneGoalSnapshot(current.Goal)
		nextRecord.Goal.Revision++
		nextRecord.Goal.Phase = "paused"
	case "resume":
		if !exists || current.Goal.Phase != "paused" {
			return fmt.Errorf("%w: 只有 paused goal 才能 resume", transport.ErrGoalTransition)
		}
		if current.Goal.Revision == math.MaxInt64 {
			return fmt.Errorf("%w: goal revision 已溢出", transport.ErrGoalTransition)
		}
		nextRecord.Goal = cloneGoalSnapshot(current.Goal)
		nextRecord.Goal.Revision++
		nextRecord.Goal.Phase = "active"
	default:
		return fmt.Errorf("%w: 未知 operation %q", transport.ErrGoalTransition, action)
	}

	next := cloneGoalMap(old)
	if operation == "clear" {
		delete(next, sessionID)
	} else {
		next[sessionID] = nextRecord
	}
	if err := writeGoalDocument(s.path, goalDocument{Version: goalStoreVersion, Goals: next}); err != nil {
		return fmt.Errorf("%w: 写 goal store %s: %v", transport.ErrStatePersistence, s.path, err)
	}
	s.m = next
	if err := s.journal.append(sessionID, operation, nextRecord); err != nil {
		// state 已写、journal 未提交：恢复到迁移前，不能留下无事件的“成功状态”。
		if rollbackErr := writeGoalDocument(s.path, goalDocument{Version: goalStoreVersion, Goals: old}); rollbackErr != nil {
			s.loadErr = fmt.Errorf("journal 提交失败 %v；goal state 回滚也失败 %v", err, rollbackErr)
			return fmt.Errorf("%w: %v", transport.ErrStatePersistence, s.loadErr)
		}
		s.m = old
		return err
	}
	return nil
}

func writeGoalDocument(path string, doc goalDocument) error {
	if doc.Goals == nil {
		doc.Goals = map[string]goalRecord{}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
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

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneGoalSnapshot(goal *transport.GoalSnapshot) *transport.GoalSnapshot {
	if goal == nil {
		return nil
	}
	copy := *goal
	if goal.BlockedReason != nil {
		reason := *goal.BlockedReason
		copy.BlockedReason = &reason
	}
	if goal.Budget != nil {
		budget := *goal.Budget
		if goal.Budget.TokenBudget != nil {
			tokenBudget := *goal.Budget.TokenBudget
			budget.TokenBudget = &tokenBudget
		}
		copy.Budget = &budget
	}
	return &copy
}

func cloneGoalRecord(record goalRecord) goalRecord {
	return goalRecord{Goal: cloneGoalSnapshot(record.Goal), RoundsStarted: cloneInt(record.RoundsStarted)}
}

func cloneGoalMap(values map[string]goalRecord) map[string]goalRecord {
	out := make(map[string]goalRecord, len(values))
	for sessionID, record := range values {
		out[sessionID] = cloneGoalRecord(record)
	}
	return out
}
