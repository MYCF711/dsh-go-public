package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"dshgo/codecs/v4"
	"dshgo/internal/agent"
	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

type independentSideRunner interface {
	Run(context.Context, []llm.Message, string, func(string) error) (string, error)
}

// sideQueryService 只登记旁路 request identity；它不经过 promptService，也不持有
// 父会话 journal、queue 或 turn lock。每次调用重新读取源 v4 的只读快照。
type sideQueryService struct {
	home      string
	model     string
	base      string
	reasoning string
	src       *sessionSource

	newRunner func(model, base, credential string) independentSideRunner
	mu        sync.Mutex
	active    map[string]map[string]context.CancelFunc
	closed    bool
}

func newSideQueryService(home, model, base, reasoning string, src *sessionSource) *sideQueryService {
	service := &sideQueryService{home: home, model: model, base: base, reasoning: reasoning, src: src, active: map[string]map[string]context.CancelFunc{}}
	service.newRunner = func(model, base, credential string) independentSideRunner {
		chat := llm.New(base, credential)
		chat.UserAgent = "dshd/" + version + " side-query"
		return &agent.SideQueryRunner{Chat: chat, Model: model, Reasoning: reasoning}
	}
	return service
}

func (s *sideQueryService) Run(ctx context.Context, sessionID, requestID, prompt string, onText func(string) error) (string, error) {
	if s == nil || s.src == nil {
		return "", errors.New("sideQuery 独立 runner 未接线")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("sideQuery 已关闭")
	}
	bySource := s.active[sessionID]
	if bySource == nil {
		bySource = map[string]context.CancelFunc{}
		s.active[sessionID] = bySource
	}
	if _, exists := bySource[requestID]; exists {
		s.mu.Unlock()
		return "", fmt.Errorf("%w: %s/%s", transport.ErrSideQueryExists, sessionID, requestID)
	}
	bySource[requestID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(bySource, requestID)
		if len(bySource) == 0 {
			delete(s.active, sessionID)
		}
		s.mu.Unlock()
	}()

	path, err := s.src.resolve(sessionID)
	if err != nil {
		return "", err
	}
	back, err := v4.ReadFile(path)
	if err != nil {
		return "", err
	}
	history := session.History(back)
	model, err := resolveServeModel(s.home, sessionID, s.model)
	if err != nil {
		return "", err
	}
	provider, routed, err := resolveServeProvider(s.home, model)
	if err != nil {
		return "", err
	}
	base, credential := s.base, envOr("DSHD_LLM_KEY", "")
	if routed {
		base, credential = provider.BaseURL, provider.Credential()
	}
	runner := s.newRunner(model, base, credential)
	if runner == nil {
		return "", errors.New("sideQuery runner factory 返回 nil")
	}
	return runner.Run(ctx, history, prompt, onText)
}

func (s *sideQueryService) Cancel(sessionID, requestID string) error {
	if s == nil || s.src == nil {
		return errors.New("sideQuery 未接线")
	}
	if _, err := s.src.resolve(sessionID); err != nil {
		return err
	}
	s.mu.Lock()
	cancel := s.active[sessionID][requestID]
	s.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("%w: %s/%s", transport.ErrNoSuchSideQuery, sessionID, requestID)
	}
	cancel()
	return nil
}

func (s *sideQueryService) CloseSession(sessionID string) error {
	if s == nil || s.src == nil {
		return errors.New("sideQuery 未接线")
	}
	if _, err := s.src.resolve(sessionID); err != nil {
		return err
	}
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.active[sessionID]))
	for _, cancel := range s.active[sessionID] {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return nil
}

func (s *sideQueryService) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	var cancels []context.CancelFunc
	for _, bySource := range s.active {
		for _, cancel := range bySource {
			cancels = append(cancels, cancel)
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
