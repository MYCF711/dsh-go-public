package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"dshgo/internal/llm"
)

// 会话级模型覆盖。
//
// # 为什么存在
//
// `session.load` 回的 `model` 是**上一轮实际用的**（从 v4 的 request/context 事件读），
// 它是只读投影。而界面上要有一个"这个会话用哪个模型"的选择器——那就得能改。
//
// # 为什么存 JSON 不写 v4
//
// R5 说 v4 是 append-only、**读路径不写盘**。模型选择是界面状态，不是会话内容，
// 写进日志会污染重放。所以单独一个 `<home>/state/session-model.json`。
//
// # 并发
//
// 一个进程一个文件；读写都在 `modelOverrideStore.mu` 下。写用临时文件 + rename，
// 避免半个 JSON 留在盘上（断电时读端会拿到"文件坏了"）。
type modelOverrideStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func newModelOverrideStore(home string) *modelOverrideStore {
	return &modelOverrideStore{
		path: filepath.Join(home, "state", "session-model.json"),
		m:    map[string]string{},
	}
}

// Load 读盘（启动时调一次，文件不存在不算错）。
func (s *modelOverrideStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读会话模型覆盖 %s: %w", s.path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("解析会话模型覆盖 %s: %w", s.path, err) // 文件坏了要说，不静默丢
	}
	if m != nil {
		s.m = m
	}
	return nil
}

// Get 取一个会话的覆盖（空串 = 没设过）。
func (s *modelOverrideStore) Get(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[sessionID]
}

// Set 设一个覆盖。**空 model = 清除**（回到宿主默认）。
func (s *modelOverrideStore) Set(sessionID, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if model == "" {
		delete(s.m, sessionID)
	} else {
		s.m[sessionID] = model
	}
	return s.flushLocked()
}

// flushLocked 原子写盘：临时文件 + rename。
func (s *modelOverrideStore) flushLocked() error {
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

// resolveServeModel 在每一轮开始时解析覆盖。重新读这份小 JSON，是为了让
// 默认 loopRunner 与 modelRunner 有完全一致的 set/clear/restart 语义：
// setModel 成功落盘后的下一轮一定看到同一个值，损坏文件也一定明确失败。
func resolveServeModel(home, sessionID, fallback string) (string, error) {
	overrides := newModelOverrideStore(home)
	if err := overrides.Load(); err != nil {
		return "", err
	}
	if model := overrides.Get(sessionID); model != "" {
		return model, nil
	}
	return fallback, nil
}

// resolveServeProvider 在每轮重新读取 catalog，两个 runner 因而看到同一份
// SetModel/重开后的配置。目录损坏与已声明 provider 的坏配置绝不回退；只有
// 普通 model id（catalog 未收录或条目三个 provider 字段全缺）保留 legacy。
func resolveServeProvider(home, model string) (llm.ModelProviderConfig, bool, error) {
	provider, err := llm.OpenModelCatalog(home).ResolveProvider(model)
	if llm.ModelCatalogErrorCode(err) == "not_found" {
		return llm.ModelProviderConfig{}, false, nil
	}
	if err != nil {
		return llm.ModelProviderConfig{}, false, fmt.Errorf("解析模型 %q 的 provider 配置: %w", model, err)
	}
	if provider.BaseURL == "" {
		return llm.ModelProviderConfig{}, false, nil
	}
	return provider, true, nil
}

func redactServeProviderError(err error, credential string) error {
	if err == nil {
		return nil
	}
	message := llm.RedactCredential(err.Error(), credential)
	if message == err.Error() {
		return err
	}
	return errors.New(message)
}
