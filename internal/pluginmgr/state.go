package pluginmgr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Manager is safe for concurrent callers in one process. Cross-process writers
// are deliberately outside this minimal slice; transactions make crashes loud.
type Manager struct {
	mu         sync.Mutex
	root       string
	state      *persistentState
	afterPhase func(string) error

	// jobs 是**在飞**安装作业的登记表（官方 requestId 语义）。
	// 它用独立的锁：安装作业持着 mu 干活时，wait/cancel 仍然必须能被观察到，
	// 否则"取消一次安装"会先卡在自己的互斥量上。
	jobsMu sync.Mutex
	jobs   map[string]*InstallJob

	// compatMu 守卫 compatibility.json（版本豁免）的读-改-写。它与 mu 分开：
	// 安装闸门在持 mu 时读豁免，而 setVersionExemption 只碰文件，不需要等安装。
	compatMu sync.Mutex
}

// Open loads only the explicitly supplied root and reconciles interrupted work.
// It never consults a user-home default, environment variable or remote source.
func Open(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, badRequest("管理器 root 不能为空")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, internalError(err)
	}
	for _, dir := range []string{absolute, filepath.Join(absolute, "packages"), filepath.Join(absolute, "transactions")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, internalError(fmt.Errorf("创建管理器目录 %s: %w", dir, err))
		}
	}
	m := &Manager{root: absolute, jobs: map[string]*InstallJob{}}
	m.mu.Lock()
	err = m.loadStateLocked()
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := m.Reconcile(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateCurrentLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) loadStateLocked() error {
	raw, err := os.ReadFile(filepath.Join(m.root, "state.json"))
	if os.IsNotExist(err) {
		m.state = emptyState()
		return nil
	}
	if err != nil {
		return internalError(fmt.Errorf("读取插件状态失败: %w", err))
	}
	state := emptyState()
	if err := decodeStrict(raw, state); err != nil {
		return errorf("state_corrupt", 422, false,
			"修复 state.json；不要删除事务目录，先调用 plugin.reconcile 检查", "插件状态损坏: %v", err)
	}
	if state.Schema != stateSchema || state.Plugins == nil {
		return errorf("state_corrupt", 422, false, "恢复备份或修复 state.json", "未知/空插件状态 schema %q", state.Schema)
	}
	for id, record := range state.Plugins {
		if err := validateRecord(id, record); err != nil {
			return errorf("state_corrupt", 422, false, "修复 state.json 后重启 reconcile", "%v", err)
		}
	}
	m.state = state
	return nil
}

func validateRecord(id string, record *pluginRecord) error {
	if record == nil || id == "" || record.ID != id {
		return fmt.Errorf("插件记录 id 不一致: map=%q record=%+v", id, record)
	}
	if err := validateID(id); err != nil {
		return err
	}
	if _, err := parseSemver(record.Version); err != nil {
		return fmt.Errorf("插件 %s 当前版本非法: %w", id, err)
	}
	if len(record.Versions) == 0 {
		return fmt.Errorf("插件 %s 没有版本来源", id)
	}
	for version, source := range record.Versions {
		if _, err := parseSemver(version); err != nil || source.Source == "" {
			return fmt.Errorf("插件 %s 版本来源非法: %q %+v", id, version, source)
		}
	}
	if _, ok := record.Versions[record.Version]; !ok {
		return fmt.Errorf("插件 %s 当前版本 %s 没有来源", id, record.Version)
	}
	if record.PreviousVersion != "" {
		if _, ok := record.Versions[record.PreviousVersion]; !ok {
			return fmt.Errorf("插件 %s previousVersion %s 不在保留版本里", id, record.PreviousVersion)
		}
	}
	return nil
}

func (m *Manager) saveStateLocked(next *persistentState) error {
	if err := validateRecordSet(next); err != nil {
		return internalError(err)
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return internalError(err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(m.root, ".state-*.tmp")
	if err != nil {
		return internalError(fmt.Errorf("创建状态临时文件失败: %w", err))
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return internalError(fmt.Errorf("写入插件状态失败: %w", err))
	}
	if err := os.Rename(tmpName, filepath.Join(m.root, "state.json")); err != nil {
		return internalError(fmt.Errorf("原子替换插件状态失败: %w", err))
	}
	return nil
}

func validateRecordSet(state *persistentState) error {
	if state == nil || state.Schema != stateSchema || state.Plugins == nil {
		return fmt.Errorf("拒绝写入空/未知状态")
	}
	for id, record := range state.Plugins {
		if err := validateRecord(id, record); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) validateCurrentLocked() error {
	for id, record := range m.state.Plugins {
		if _, err := m.recordInfoLocked(id, record); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) recordInfoLocked(id string, record *pluginRecord) (PluginInfo, error) {
	info := PluginInfo{}
	manifest, err := readAndValidatePackage(m.packagePath(id, record.Version))
	if err != nil {
		return info, errorf("package_corrupt", 422, false,
			"保留现场并检查 packages 与 transactions；不要静默删除插件", "已安装插件 %s@%s 校验失败: %v", id, record.Version, err)
	}
	if manifest.ID != id || manifest.Version != record.Version {
		return info, errorf("package_corrupt", 422, false, "修复包目录身份后重试", "已安装包身份不符：state=%s@%s manifest=%s@%s", id, record.Version, manifest.ID, manifest.Version)
	}
	return PluginInfo{
		ID: id, Version: record.Version, Kind: manifest.Kind, Entry: manifest.Entry,
		Enabled: record.Enabled, Source: record.Versions[record.Version].Source,
		PreviousVersion: record.PreviousVersion,
	}, nil
}

func (m *Manager) packagePath(id, version string) string {
	return filepath.Join(m.root, "packages", id, version)
}

func (m *Manager) pluginInfoLocked(id string) (PluginInfo, error) {
	record := m.state.Plugins[id]
	if record == nil {
		return PluginInfo{}, notFound(id)
	}
	return m.recordInfoLocked(id, record)
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func sortedPluginIDs(state *persistentState) []string {
	ids := make([]string, 0, len(state.Plugins))
	for id := range state.Plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
