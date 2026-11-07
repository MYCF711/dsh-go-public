package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"dshgo/internal/llm"
	"dshgo/internal/transport"
)

// effortOverrideStore 只保存 catalog level key，不保存它的 wire value。
// 每轮都按当时的 model+catalog 重新映射，切模型或修改目录后不会发出陈旧 wire。
type effortOverrideStore struct {
	mu      sync.Mutex
	path    string
	m       map[string]string
	loadErr error
}

func newEffortOverrideStore(home string) *effortOverrideStore {
	return &effortOverrideStore{
		path: filepath.Join(home, "state", "session-effort.json"),
		m:    map[string]string{},
	}
}

func (s *effortOverrideStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.loadErr = nil
			return nil
		}
		s.loadErr = fmt.Errorf("读会话 effort 覆盖 %s: %w", s.path, err)
		return s.loadErr
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		s.loadErr = fmt.Errorf("解析会话 effort 覆盖 %s: %w", s.path, err)
		return s.loadErr
	}
	if values == nil {
		values = map[string]string{}
	}
	for sessionID, effort := range values {
		if sessionID == "" || effort == "" {
			s.loadErr = fmt.Errorf("会话 effort 覆盖 %s 含空 session id/level", s.path)
			return s.loadErr
		}
	}
	s.m = values
	s.loadErr = nil
	return nil
}

func (s *effortOverrideStore) Selection(sessionID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return "", fmt.Errorf("%w: %v", transport.ErrStatePersistence, s.loadErr)
	}
	return s.m[sessionID], nil
}

func (s *effortOverrideStore) Set(sessionID, effort string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id 为空", transport.ErrEffortInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("%w: 拒绝覆盖损坏的 effort store: %v", transport.ErrStatePersistence, s.loadErr)
	}
	next := make(map[string]string, len(s.m)+1)
	for id, value := range s.m {
		next[id] = value
	}
	if effort == "" {
		delete(next, sessionID)
	} else {
		next[sessionID] = effort
	}
	if err := writeEffortOverrideDocument(s.path, next); err != nil {
		return fmt.Errorf("%w: 写 %s: %v", transport.ErrStatePersistence, s.path, err)
	}
	s.m = next
	return nil
}

func writeEffortOverrideDocument(path string, values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// catalogEffortWire 返回 (wire, keyDeclared, error)。off:null 是已声明且 wire
// 为空，不能与“没有这个 key”或“无会话覆盖”合并成同一种状态。
func catalogEffortWire(home, modelID, level string, catalogs ...*llm.ModelCatalogStore) (string, bool, error) {
	catalog := (*llm.ModelCatalogStore)(nil)
	if len(catalogs) > 0 {
		catalog = catalogs[0]
	}
	if catalog == nil {
		catalog = llm.OpenModelCatalog(home)
	}
	view, err := catalog.List()
	if err != nil {
		return "", false, fmt.Errorf("%w: 读取模型目录: %v", transport.ErrStatePersistence, err)
	}
	for _, raw := range view.Models {
		var model struct {
			ID               string          `json:"id"`
			ReasoningEfforts json.RawMessage `json:"reasoningEfforts"`
		}
		if err := json.Unmarshal(raw, &model); err != nil {
			return "", false, fmt.Errorf("解析模型目录行: %w", err)
		}
		if model.ID != modelID {
			continue
		}
		trimmed := bytes.TrimSpace(model.ReasoningEfforts)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("false")) {
			return "", false, nil
		}
		var levels map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &levels); err != nil {
			return "", false, fmt.Errorf("模型 %q 的 reasoningEfforts 不是 object: %w", modelID, err)
		}
		wireRaw, exists := levels[level]
		if !exists {
			return "", false, nil
		}
		if bytes.Equal(bytes.TrimSpace(wireRaw), []byte("null")) {
			if level != "off" {
				return "", false, fmt.Errorf("模型 %q 的 level %q 非法 null wire", modelID, level)
			}
			return "", true, nil
		}
		var wire string
		if err := json.Unmarshal(wireRaw, &wire); err != nil || wire == "" {
			return "", false, fmt.Errorf("模型 %q 的 level %q wire 不是非空 string", modelID, level)
		}
		return wire, true, nil
	}
	return "", false, nil
}

// setServeEffort 先按“下一轮生效模型”校验，再写独立 store。空串只负责清除，
// 不要求旧 catalog 仍然存在；损坏 store 不会被一次 clear 静默覆盖。
func setServeEffort(store *effortOverrideStore, home, sessionID, effort, defaultModel string, catalogs ...*llm.ModelCatalogStore) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id 为空", transport.ErrEffortInvalid)
	}
	if effort == "" {
		return store.Set(sessionID, "")
	}
	model, err := resolveServeModel(home, sessionID, defaultModel)
	if err != nil {
		return fmt.Errorf("%w: 解析当前模型: %v", transport.ErrStatePersistence, err)
	}
	_, declared, err := catalogEffortWire(home, model, effort, catalogs...)
	if err != nil {
		return err
	}
	if !declared {
		return fmt.Errorf("%w: 模型 %q 未声明 level %q", transport.ErrEffortInvalid, model, effort)
	}
	return store.Set(sessionID, effort)
}

// resolveServeEffort 在每轮重新读取覆盖与 catalog。selected=true 表示确有覆盖，
// 即使 wire==""（off:null）也必须覆盖 host 默认，不能退回继承环境变量。
func resolveServeEffort(home, sessionID, modelID, fallback string, catalogs ...*llm.ModelCatalogStore) (wire, level string, selected bool, err error) {
	store := newEffortOverrideStore(home)
	if err := store.Load(); err != nil {
		return "", "", false, err
	}
	selection, err := store.Selection(sessionID)
	if err != nil {
		return "", "", false, err
	}
	if selection == "" {
		// host fallback 是 wire，不天然是 catalog key。只有它本身也是已声明
		// key、且该 key 的映射正好就是实际 wire 时，才能把 key 记入 v4；
		// 例如 deep→wire-a 时，fallback=wire-a 不得伪记成 deep。
		level := ""
		if fallback != "" {
			mapped, declared, err := catalogEffortWire(home, modelID, fallback, catalogs...)
			if err != nil {
				return "", "", false, err
			}
			if declared && mapped == fallback {
				level = fallback
			}
		}
		return fallback, level, false, nil
	}
	wire, declared, err := catalogEffortWire(home, modelID, selection, catalogs...)
	if err != nil {
		return "", "", false, err
	}
	if !declared {
		return "", "", false, fmt.Errorf("会话 %s 的 effort level %q 已不是当前模型 %q 的 reasoningEfforts key", sessionID, selection, modelID)
	}
	return wire, selection, true, nil
}
