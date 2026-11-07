package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"dshgo/codecs/v4"
	"dshgo/internal/modes"
	"dshgo/internal/sandbox"
	"dshgo/internal/session"
)

// 生产接线：会话 preset → 真实 runtime sandbox 档位。
//
// 这一层只做策略，不做 OS 隔离：
//   - 档位落在 `state/session-sandbox.json`（与 session-model/session-permission 同构）；
//   - 每一次 `dshd run` 子进程由父进程把**该会话的真实档位**放进
//     DSHD_SANDBOX_MODE（见 sandboxChildEnv），子进程的 bash/pwsh/FS 按它判定；
//   - 没有"当前权限"单例：A/B 两个会话各自解析各自的值，fork 显式继承。
//
// 未接线前（本文件只有 helper，serve.go/main.go 的调用由 Lead 加），生产行为
// 还是旧的：bash/pwsh 不经过策略闸门，FS 只按 workspace-write 的旧边界走。
// 本文件与测试证明的是"接上之后的三档真值"，不是"已经接了"。

// sandboxModeDocument 是 `<home>/state/session-sandbox.json` 的形状。
type sandboxModeDocument struct {
	Version  int               `json:"version"`
	Sessions map[string]string `json:"sessions"`
}

// sandboxModeStore 是每会话真实 sandbox 档位的持久覆盖。
//
// 只存"比 preset 更窄或相等"的显式选择；更宽的请求在写入前就被拒绝
// （见 sandboxSetPermission），所以重开/恢复不可能因这份文件变大。
type sandboxModeStore struct {
	mu   sync.Mutex
	path string
	m    map[string]sandbox.Mode
}

func newSandboxModeStore(home string) *sandboxModeStore {
	return &sandboxModeStore{
		path: filepath.Join(home, "state", "session-sandbox.json"),
		m:    map[string]sandbox.Mode{},
	}
}

// Load 读盘。文件不存在不算错；损坏或未知档位 fail-loud（绝不按默认档静默跑）。
func (s *sandboxModeStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc sandboxModeDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if doc.Version != 1 || doc.Sessions == nil {
		return fmt.Errorf("%s version/sessions 非法（期望 version=1 且 sessions 对象）", s.path)
	}
	next := make(map[string]sandbox.Mode, len(doc.Sessions))
	for sessionID, value := range doc.Sessions {
		mode, err := sandbox.ParseMode(value)
		if err != nil {
			return fmt.Errorf("%s 会话 %q: %w", s.path, sessionID, err)
		}
		next[sessionID] = mode
	}
	s.m = next
	return nil
}

// Get 返回会话的持久覆盖；第二个返回值表示有没有显式设置过。
func (s *sandboxModeStore) Get(sessionID string) (sandbox.Mode, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", false, fmt.Errorf("%w: session id 为空", sandbox.ErrMissingContext)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mode, ok := s.m[sessionID]
	return mode, ok, nil
}

// Set 写入一个已验证的档位并原子落盘。
func (s *sandboxModeStore) Set(sessionID string, mode sandbox.Mode) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("%w: session id 为空", sandbox.ErrMissingContext)
	}
	if err := sandbox.ValidateMode(mode); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sessionID] = mode
	return s.flushLocked()
}

// Clear 清除覆盖（回到 preset 的档位）。
func (s *sandboxModeStore) Clear(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("%w: session id 为空", sandbox.ErrMissingContext)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[sessionID]; !ok {
		return nil
	}
	delete(s.m, sessionID)
	return s.flushLocked()
}

func (s *sandboxModeStore) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	doc := sandboxModeDocument{Version: 1, Sessions: make(map[string]string, len(s.m))}
	for sessionID, mode := range s.m {
		doc.Sessions[sessionID] = string(mode)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// sandboxPresetStatePath 与 serve_agent_api.go 的 agentPresetStatePath 是同一个文件。
// 这里只读，不写：preset 选择的唯一写者还是 agentPresetSelect。
func sandboxPresetStatePath(home string) string {
	return filepath.Join(home, "state", "session-preset.json")
}

// readSessionPresetOverrides 读 preset 覆盖文件。形状非法 fail-loud。
func readSessionPresetOverrides(home string) (map[string]string, error) {
	raw, err := os.ReadFile(sandboxPresetStatePath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var doc struct {
		Version  int               `json:"version"`
		Sessions map[string]string `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", sandboxPresetStatePath(home), err)
	}
	if doc.Version != 1 || doc.Sessions == nil {
		return nil, fmt.Errorf("%s version/sessions 非法", sandboxPresetStatePath(home))
	}
	return doc.Sessions, nil
}

// sandboxPresetIDForSession 返回会话当前 preset 的真值来源：
//  1. `state/session-preset.json` 的显式选择（界面选过的）；
//  2. 否则读 v4 header 的 agentPreset（建会话时写下的）；
//  3. 两处都空 = 没有记录 → fail-loud，不猜。
func sandboxPresetIDForSession(home, sessionID string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("%w: session id 为空", sandbox.ErrMissingContext)
	}
	overrides, err := readSessionPresetOverrides(home)
	if err != nil {
		return "", err
	}
	if preset := strings.TrimSpace(overrides[sessionID]); preset != "" {
		return preset, nil
	}
	path, err := session.FindByID(home, sessionID)
	if err != nil {
		return "", fmt.Errorf("%w: 会话 %s 没有 preset 覆盖，且找不到 v4 日志: %v",
			sandbox.ErrMissingContext, sessionID, err)
	}
	header, err := v4.ReadHeader(path)
	if err != nil {
		return "", fmt.Errorf("%w: 读 %s 的 header: %v", sandbox.ErrMissingContext, path, err)
	}
	if preset := strings.TrimSpace(header.AgentPreset); preset != "" {
		return preset, nil
	}
	return "", fmt.Errorf("%w: 会话 %s 的 preset 覆盖与 header.agentPreset 都是空的",
		sandbox.ErrMissingContext, sessionID)
}

// sandboxModeSource 是一次解析的完整证据：preset 是什么、覆盖是什么、最终档位是什么。
type sandboxModeSource struct {
	SessionID  string       `json:"session_id"`
	PresetID   string       `json:"preset_id"`
	PresetMode sandbox.Mode `json:"preset_mode"`
	Override   sandbox.Mode `json:"override,omitempty"`
	Effective  sandbox.Mode `json:"effective_mode"`
}

// resolveSandboxModeForSession 把"会话 preset + 持久覆盖"解析成一个真实档位。
//
// 单调性：覆盖只能比 preset 窄或相等。持久文件里出现更宽的覆盖（旧版本/手改）
// 一律 fail-loud，不静默放宽，也不静默丢弃。
func resolveSandboxModeForSession(home, sessionID string, presets *modes.Registry, store *sandboxModeStore) (sandboxModeSource, error) {
	if presets == nil {
		return sandboxModeSource{}, fmt.Errorf("%w: preset registry 为空", sandbox.ErrMissingContext)
	}
	if store == nil {
		return sandboxModeSource{}, fmt.Errorf("%w: sandbox store 为空", sandbox.ErrMissingContext)
	}
	presetID, err := sandboxPresetIDForSession(home, sessionID)
	if err != nil {
		return sandboxModeSource{}, err
	}
	mode, err := presets.Resolve(presetID)
	if err != nil {
		return sandboxModeSource{}, fmt.Errorf("会话 %s 的 preset %q 无法解析: %w", sessionID, presetID, err)
	}
	presetMode, err := mode.SandboxMode()
	if err != nil {
		return sandboxModeSource{}, err
	}
	src := sandboxModeSource{SessionID: sessionID, PresetID: presetID, PresetMode: presetMode, Effective: presetMode}
	override, ok, err := store.Get(sessionID)
	if err != nil {
		return sandboxModeSource{}, err
	}
	if !ok {
		return src, nil
	}
	src.Override = override
	cmp, err := sandbox.CompareModes(override, presetMode)
	if err != nil {
		return sandboxModeSource{}, err
	}
	if cmp > 0 {
		return sandboxModeSource{}, fmt.Errorf(
			"%w: 会话 %s 的持久覆盖 %s 比 preset %q（%s）更宽；拒绝恢复，请清掉 state/session-sandbox.json 里的这条",
			sandbox.ErrWidenDenied, sessionID, override, presetID, presetMode)
	}
	src.Effective = override
	return src, nil
}

// sandboxModeForPresetID 是建会话时的映射真值：未知 preset / 未知 sandbox 都 fail-loud。
func sandboxModeForPresetID(presetID string, presets *modes.Registry) (sandbox.Mode, error) {
	if presets == nil {
		return "", fmt.Errorf("%w: preset registry 为空", sandbox.ErrMissingContext)
	}
	if strings.TrimSpace(presetID) == "" {
		return "", fmt.Errorf("%w: preset id 为空", sandbox.ErrMissingContext)
	}
	mode, err := presets.Resolve(presetID)
	if err != nil {
		return "", err
	}
	return mode.SandboxMode()
}

// sandboxPermissionResult 是 session.setPermission 应该回给界面的真实值。
//
// 关键点：mode/effective_mode 是**真实 sandbox 档位**，不是审批值。
// 旧客户端传 deny/ask/allow/auto 时，approval 原样回显，但 mode 仍是当前真值，
// 界面不再需要自己猜"这个会话到底能不能写工作区"。
type sandboxPermissionResult struct {
	Action      string       `json:"action"` // allow / deny / needs-approval（本次设置动作的结论）
	Mode        sandbox.Mode `json:"mode"`
	Effective   sandbox.Mode `json:"effective_mode"`
	Preset      string       `json:"preset"`
	Persisted   bool         `json:"persisted"`
	Approval    string       `json:"approval,omitempty"`
	Enforcement string       `json:"enforcement"`
	Error       string       `json:"error,omitempty"`
}

// sandboxApprovalValue 是旧的审批值集合。它们仍然能设置，但不再被当作 sandbox 档位。
func sandboxApprovalValue(value string) bool {
	switch value {
	case "deny", "ask", "allow", "auto":
		return true
	}
	return false
}

// sandboxSetPermission 是 session.setPermission 的策略半边：
//   - "" → 清掉 sandbox 覆盖，返回 preset 真值；
//   - 三档之一 → 只允许"更窄或相等"，持久化并返回真实档位；
//   - deny/ask/allow/auto → 照旧写审批覆盖，同时返回当前真实 sandbox 档位；
//   - 其它任何值 → fail-loud，不猜、不降级。
func sandboxSetPermission(
	home, sessionID, value string,
	presets *modes.Registry,
	sandboxStore *sandboxModeStore,
	approvals *permissionOverrideStore,
) (sandboxPermissionResult, error) {
	if strings.TrimSpace(sessionID) == "" {
		return sandboxPermissionResult{}, fmt.Errorf("%w: id 为空：要改哪个会话", sandbox.ErrMissingContext)
	}
	current, err := resolveSandboxModeForSession(home, sessionID, presets, sandboxStore)
	if err != nil {
		return sandboxPermissionResult{}, err
	}
	result := sandboxPermissionResult{
		Action:      string(sandbox.ActionAllow),
		Mode:        current.Effective,
		Effective:   current.Effective,
		Preset:      current.PresetID,
		Enforcement: string(sandbox.EnforcementPolicyOnlyNoOSIsolation),
	}
	trimmed := strings.TrimSpace(value)
	switch {
	case trimmed == "":
		if err := sandboxStore.Clear(sessionID); err != nil {
			return sandboxPermissionResult{}, err
		}
		result.Persisted = false
		return result, nil

	case sandboxApprovalValue(trimmed):
		if approvals == nil {
			return sandboxPermissionResult{}, fmt.Errorf("%w: 审批覆盖 store 为空", sandbox.ErrMissingContext)
		}
		if err := approvals.Set(sessionID, trimmed); err != nil {
			return sandboxPermissionResult{}, err
		}
		result.Approval = trimmed
		return result, nil

	default:
		requested, err := sandbox.ParseMode(trimmed)
		if err != nil {
			return sandboxPermissionResult{}, fmt.Errorf("permission 只能是三档 sandbox（read-only / workspace-write / danger-full-access）或 deny/ask/allow/auto（或空串清除）: %w", err)
		}
		cmp, err := sandbox.CompareModes(requested, current.PresetMode)
		if err != nil {
			return sandboxPermissionResult{}, err
		}
		if cmp > 0 {
			return sandboxPermissionResult{}, fmt.Errorf(
				"%w: 会话 preset %q 是 %s；不能把权限改成更宽的 %s。要放宽请显式切换 preset（agentPresets.select），要在单次调用中越界请用工具参数 sandbox_permissions",
				sandbox.ErrWidenDenied, current.PresetID, current.PresetMode, requested)
		}
		if err := sandboxStore.Set(sessionID, requested); err != nil {
			return sandboxPermissionResult{}, err
		}
		result.Mode = requested
		result.Effective = requested
		result.Persisted = true
		return result, nil
	}
}

// sandboxAdoptOnFork 让 fork 出来的会话继承父会话的**有效**档位（只能更窄）。
//
// fork 的 header.agentPreset 是从源会话复制的，所以孩子默认是 preset 档；
// 如果父会话当时被 setPermission 收窄过，孩子必须跟着收窄，否则 fork 就是
// 一条"权限变大"的捷径。反过来，孩子自己的 preset 更窄时保持孩子的。
func sandboxAdoptOnFork(home, parentID, childID string, presets *modes.Registry, store *sandboxModeStore) error {
	parent, err := resolveSandboxModeForSession(home, parentID, presets, store)
	if err != nil {
		return fmt.Errorf("解析父会话 %s 的 sandbox 档位: %w", parentID, err)
	}
	child, err := resolveSandboxModeForSession(home, childID, presets, store)
	if err != nil {
		return fmt.Errorf("解析 fork 会话 %s 的 sandbox 档位: %w", childID, err)
	}
	effective, err := sandbox.MostRestrictive(parent.Effective, child.PresetMode)
	if err != nil {
		return err
	}
	if effective == child.PresetMode {
		return store.Clear(childID)
	}
	return store.Set(childID, effective)
}

// sandboxOnPresetSelected 是 agentPresets.select 成功后的清理口：
// 旧的 sandbox 覆盖是相对**旧 preset** 的收窄；换 preset 意味着重新定基线，
// 必须清掉，否则会留下"比新 preset 更宽"的非法组合（resolve 会 fail-loud）。
// 新 preset 本身也要在这里校验档位，避免把未知 sandbox 值带进下一轮。
func sandboxOnPresetSelected(sessionID, newPresetID string, presets *modes.Registry, store *sandboxModeStore) error {
	if _, err := sandboxModeForPresetID(newPresetID, presets); err != nil {
		return err
	}
	return store.Clear(sessionID)
}

// sandboxChildEnv 是子进程 spawn 时唯一需要的策略环境。
//
// 只在 childArgs 的 exec.Cmd.Env 上追加；不要 os.Setenv 到父进程——那会让
// A/B 两个会话共享一个"当前档位"，正是契约禁止的形状。
func sandboxChildEnv(mode sandbox.Mode) ([]string, error) {
	if err := sandbox.ValidateMode(mode); err != nil {
		return nil, err
	}
	return []string{sandbox.EnvMode + "=" + string(mode)}, nil
}

// sandboxWriteChildEnv 是 LoopRunner 侧的一行接线：解析该会话真实档位并
// 生成要追加到 exec.Cmd.Env 的环境。Lead 在 serve_tools.go 起子进程时调用。
func sandboxWriteChildEnv(home, sessionID string, presets *modes.Registry, store *sandboxModeStore) ([]string, error) {
	src, err := resolveSandboxModeForSession(home, sessionID, presets, store)
	if err != nil {
		return nil, err
	}
	return sandboxChildEnv(src.Effective)
}

// sandboxErrIsWidenDenied 给上层做错误分类用（HTTP 400 vs 500）。
func sandboxErrIsWidenDenied(err error) bool {
	return errors.Is(err, sandbox.ErrWidenDenied)
}
