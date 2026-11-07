package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// 会话级审批策略覆盖。
//
// 和 `modelOverrideStore` 同构（`serve_model_override.go`），差别在值的语义：
// model 是任意字符串，permission 只能是 deny/ask/allow/auto（白名单在
// `transport.handleSessionSetPermission` 里校，这一层不再重复）。
//
// # 为什么单独一个文件而不是和 model 合并成一个 store
//
// 合并会让"改模型"和"改权限"共享同一份 JSON 的并发路径——两个界面动作
// 同时写会互相覆盖对方的 key（读-改-写没有整体锁）。分开两个文件，
// 两个操作互不影响，出问题也容易定位（看一眼文件就知道是谁的锅）。
type permissionOverrideStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func newPermissionOverrideStore(home string) *permissionOverrideStore {
	return &permissionOverrideStore{
		path: filepath.Join(home, "state", "session-permission.json"),
		m:    map[string]string{},
	}
}

// Load 读盘（启动时调一次，文件不存在不算错）。
func (s *permissionOverrideStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m != nil {
		s.m = m
	}
	return nil
}

// Get 取一个会话的覆盖（空串 = 没设过）。
func (s *permissionOverrideStore) Get(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[sessionID]
}

// Set 设一个覆盖。**空 perm = 清除**（回到宿主默认）。
func (s *permissionOverrideStore) Set(sessionID, perm string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if perm == "" {
		delete(s.m, sessionID)
	} else {
		s.m[sessionID] = perm
	}
	return s.flushLocked()
}

// flushLocked 原子写盘：临时文件 + rename。
func (s *permissionOverrideStore) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
