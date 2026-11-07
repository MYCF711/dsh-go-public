package pluginmgr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type transaction struct {
	Schema        string        `json:"schema"`
	ID            string        `json:"id"`
	Operation     string        `json:"operation"`
	PluginID      string        `json:"pluginId"`
	TargetVersion string        `json:"targetVersion,omitempty"`
	Phase         string        `json:"phase"`
	Before        *pluginRecord `json:"before,omitempty"`
	After         *pluginRecord `json:"after,omitempty"`
}

func (m *Manager) txDir(id string) string { return filepath.Join(m.root, "transactions", id) }

func writeTransaction(m *Manager, tx *transaction) error {
	tx.Schema = txSchema
	raw, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return internalError(err)
	}
	raw = append(raw, '\n')
	dir := m.txDir(tx.ID)
	tmp, err := os.CreateTemp(dir, ".transaction-*.tmp")
	if err != nil {
		return internalError(err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return internalError(err)
	}
	if err := os.Rename(name, filepath.Join(dir, "transaction.json")); err != nil {
		return internalError(err)
	}
	return nil
}

func (m *Manager) beginTransaction(operation, id string, before, after *pluginRecord) (*transaction, error) {
	random, err := randomID()
	if err != nil {
		return nil, internalError(err)
	}
	tx := &transaction{ID: random, Operation: operation, PluginID: id, Phase: "prepared", Before: before, After: after}
	if after != nil {
		tx.TargetVersion = after.Version
	} else if before != nil {
		tx.TargetVersion = before.Version
	}
	if err := os.MkdirAll(m.txDir(tx.ID), 0o700); err != nil {
		return nil, internalError(err)
	}
	if err := writeTransaction(m, tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func (m *Manager) setPhase(tx *transaction, phase string) error {
	tx.Phase = phase
	if err := writeTransaction(m, tx); err != nil {
		return err
	}
	if m.afterPhase != nil {
		if err := m.afterPhase(phase); err != nil {
			return internalError(err)
		}
	}
	return nil
}

func (m *Manager) finishTransaction(tx *transaction) {
	// A cleanup failure leaves a committed journal. Reconcile will finish it;
	// the already committed operation is not misreported as a failed update.
	_ = os.RemoveAll(m.txDir(tx.ID))
}

func (m *Manager) rollbackInstallTransaction(tx *transaction) error {
	if tx.After != nil && reflect.DeepEqual(m.state.Plugins[tx.PluginID], tx.After) {
		m.finishTransaction(tx)
		return nil
	}
	if tx.Operation != "restore" && tx.TargetVersion != "" {
		if tx.Before == nil || tx.Before.Version != tx.TargetVersion {
			if err := os.RemoveAll(m.packagePath(tx.PluginID, tx.TargetVersion)); err != nil {
				return internalError(err)
			}
		}
	}
	if err := os.RemoveAll(m.txDir(tx.ID)); err != nil {
		return internalError(err)
	}
	return nil
}

// List verifies the current package before returning its persistent projection.
func (m *Manager) List() ([]PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := sortedPluginIDs(m.state)
	out := make([]PluginInfo, 0, len(ids))
	for _, id := range ids {
		info, err := m.pluginInfoLocked(id)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func (m *Manager) Install(source string) (PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.installLocked(source, false, nil)
}

// installLocked 是 install 的唯一实现：enable 与 control 只为官方
// `pluginManager.installBundle` 的 enabled/requestId 服务；nil control 表示
// 这次安装不可取消（既有 plugin.install 就是这条路）。
//
// 启用位与安装写在同一份 state 里、由同一个事务提交：不会出现"装上了但
// 启用位另写一次、中途失败"的半截状态。
func (m *Manager) installLocked(source string, enable bool, control *installControl) (PluginInfo, error) {
	if err := control.checkpoint(); err != nil {
		return PluginInfo{}, err
	}
	manifest, absolute, err := m.validateSourceLocked(source)
	if err != nil {
		return PluginInfo{}, err
	}
	// 兼容性闸门（官方 installBundle 的同一步）：不兼容且没有精确豁免就不装。
	if err := m.checkCompatibilityLocked(manifest); err != nil {
		return PluginInfo{}, err
	}
	if m.state.Plugins[manifest.ID] != nil {
		return PluginInfo{}, errorf("duplicate_id", http.StatusConflict, false,
			"使用 plugin.update 安装更高 SemVer，或先 uninstall", "插件 id %s 已安装，拒绝重复安装", manifest.ID)
	}
	record := &pluginRecord{ID: manifest.ID, Version: manifest.Version, Enabled: enable,
		Versions: map[string]versionSource{manifest.Version: {Source: absolute}}}
	if err := m.installVersionLocked("install", manifest, absolute, nil, record, control); err != nil {
		return PluginInfo{}, err
	}
	return m.pluginInfoLocked(manifest.ID)
}

func (m *Manager) Update(id, source string) (PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.state.Plugins[id]
	if before == nil {
		return PluginInfo{}, notFound(id)
	}
	manifest, absolute, err := m.validateSourceLocked(source)
	if err != nil {
		return PluginInfo{}, err
	}
	if manifest.ID != id {
		return PluginInfo{}, badRequest("更新包 id=%s 与目标 id=%s 不一致", manifest.ID, id)
	}
	// 更新同样过兼容性闸门：装一个不兼容的新版本比装新包更危险（它会顶掉能用的版本）。
	if err := m.checkCompatibilityLocked(manifest); err != nil {
		return PluginInfo{}, err
	}
	newVersion, _ := parseSemver(manifest.Version)
	oldVersion, _ := parseSemver(before.Version)
	if compareSemver(newVersion, oldVersion) <= 0 {
		return PluginInfo{}, errorf("version_conflict", http.StatusConflict, false,
			"更新包版本必须大于当前版本；旧版请用 plugin.restore", "更新版本 %s 必须大于当前 %s", manifest.Version, before.Version)
	}
	if _, err := os.Stat(m.packagePath(id, manifest.Version)); err == nil {
		return PluginInfo{}, errorf("version_conflict", http.StatusConflict, false,
			"保留版本只能 restore，不能覆盖；请换用新版本", "版本目录 %s@%s 已存在，拒绝覆盖", id, manifest.Version)
	} else if !os.IsNotExist(err) {
		return PluginInfo{}, internalError(err)
	}
	after := cloneState(&persistentState{Schema: stateSchema, Plugins: map[string]*pluginRecord{id: before}}).Plugins[id]
	after.Version = manifest.Version
	after.PreviousVersion = before.Version
	after.Versions[manifest.Version] = versionSource{Source: absolute}
	if err := m.installVersionLocked("update", manifest, absolute, before, after, nil); err != nil {
		return PluginInfo{}, err
	}
	return m.pluginInfoLocked(id)
}

func (m *Manager) validateSourceLocked(source string) (Manifest, string, error) {
	manifest, err := readAndValidatePackage(source)
	if err != nil {
		return Manifest{}, "", err
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return Manifest{}, "", internalError(err)
	}
	rel, err := filepath.Rel(m.root, absolute)
	if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return Manifest{}, "", badRequest("包源目录不能位于管理器 root 内: %s", absolute)
	}
	return manifest, absolute, nil
}

func (m *Manager) installVersionLocked(operation string, sourceManifest Manifest, source string, before, after *pluginRecord, control *installControl) (retErr error) {
	if err := control.checkpoint(); err != nil {
		return err
	}
	tx, err := m.beginTransaction(operation, sourceManifest.ID, before, after)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			if rollbackErr := m.rollbackInstallTransaction(tx); retErr == nil && rollbackErr != nil {
				retErr = rollbackErr
			}
		}
	}()
	staging := filepath.Join(m.txDir(tx.ID), "staging")
	if err := copyPackage(source, staging, sourceManifest, control.checkpoint); err != nil {
		return err
	}
	copiedManifest, err := readAndValidatePackage(staging)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(copiedManifest, sourceManifest) {
		return manifestError("复制期间 manifest 或 payload 发生变化")
	}
	control.enter("staged")
	if err := m.setPhase(tx, "staged"); err != nil {
		return err
	}
	if err := control.checkpoint(); err != nil {
		return err
	}
	target := m.packagePath(sourceManifest.ID, sourceManifest.Version)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return internalError(err)
	}
	if err := os.Rename(staging, target); err != nil {
		return internalError(fmt.Errorf("原子安装版本目录失败: %w", err))
	}
	control.enter("installed")
	if err := m.setPhase(tx, "installed"); err != nil {
		return err
	}
	if err := control.checkpoint(); err != nil {
		return err
	}
	next := cloneState(m.state)
	next.Plugins[sourceManifest.ID] = after
	// committing 是"太晚"的边界：从这里开始，取消不再回滚已就位的版本目录，
	// 否则会与正在落盘的状态互相踩。
	control.enter("committing")
	if err := m.setPhase(tx, "committing"); err != nil {
		return err
	}
	if err := m.saveStateLocked(next); err != nil {
		return err
	}
	m.state = next
	if m.afterPhase != nil {
		if err := m.afterPhase("state_committed"); err != nil {
			return internalError(err)
		}
	}
	control.enter("state_committed")
	success = true
	m.finishTransaction(tx)
	return nil
}

func copyPackage(source, staging string, manifest Manifest, check func() error) error {
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return internalError(err)
	}
	copyOne := func(name string, wantHash string) error {
		if err := check(); err != nil {
			return err
		}
		from := filepath.Join(source, filepath.FromSlash(name))
		to := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return internalError(err)
		}
		input, err := os.Open(from)
		if err != nil {
			return internalError(err)
		}
		defer input.Close()
		output, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return internalError(err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
		if syncErr := output.Sync(); copyErr == nil {
			copyErr = syncErr
		}
		if closeErr := output.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return internalError(copyErr)
		}
		if wantHash != "" {
			if got := hex.EncodeToString(hash.Sum(nil)); got != wantHash {
				return errorf("hash_mismatch", 422, false, "重新生成私有包及哈希", "复制文件 %s 时 SHA-256 改变", name)
			}
		}
		return nil
	}
	rawManifest, err := os.ReadFile(filepath.Join(source, "plugin.json"))
	if err != nil {
		return internalError(err)
	}
	manifestHash := sha256.Sum256(rawManifest)
	if err := copyOne("plugin.json", hex.EncodeToString(manifestHash[:])); err != nil {
		return err
	}
	for _, name := range sortedFileNames(manifest.Files) {
		if err := copyOne(name, manifest.Files[name]); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) setEnabled(id string, enabled bool) (PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.state.Plugins[id]
	if record == nil {
		return PluginInfo{}, notFound(id)
	}
	if _, err := m.pluginInfoLocked(id); err != nil {
		return PluginInfo{}, err
	}
	if record.Enabled == enabled {
		return m.pluginInfoLocked(id)
	}
	next := cloneState(m.state)
	next.Plugins[id].Enabled = enabled
	if err := m.saveStateLocked(next); err != nil {
		return PluginInfo{}, err
	}
	m.state = next
	return m.pluginInfoLocked(id)
}

func (m *Manager) Enable(id string) (PluginInfo, error)  { return m.setEnabled(id, true) }
func (m *Manager) Disable(id string) (PluginInfo, error) { return m.setEnabled(id, false) }

func (m *Manager) Restore(id, version string) (PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restoreLocked(id, version, nil)
}

// restoreLocked 是 restore 的唯一实现。官方 `pluginManager.updateBundle` 在
// "目标版本已在本机 store"时就走这里：它改的是真实 current 指针，由事务包住，
// 失败不改旧 current。
func (m *Manager) restoreLocked(id, version string, control *installControl) (PluginInfo, error) {
	if err := control.checkpoint(); err != nil {
		return PluginInfo{}, err
	}
	before := m.state.Plugins[id]
	if before == nil {
		return PluginInfo{}, notFound(id)
	}
	if version == "" {
		version = before.PreviousVersion
	}
	if version == "" {
		return PluginInfo{}, badRequest("插件 %s 没有 previousVersion；请显式给 version", id)
	}
	if _, ok := before.Versions[version]; !ok {
		return PluginInfo{}, errorf("not_found", http.StatusNotFound, false,
			"plugin.list 查看 previousVersion，或安装一个更高版本", "插件 %s 没有保留版本 %s", id, version)
	}
	if version == before.Version {
		return m.pluginInfoLocked(id)
	}
	if _, err := readAndValidatePackage(m.packagePath(id, version)); err != nil {
		return PluginInfo{}, err
	}
	after := cloneState(&persistentState{Schema: stateSchema, Plugins: map[string]*pluginRecord{id: before}}).Plugins[id]
	after.Version = version
	after.PreviousVersion = before.Version
	tx, err := m.beginTransaction("restore", id, before, after)
	if err != nil {
		return PluginInfo{}, err
	}
	if err := control.checkpoint(); err != nil {
		_ = os.RemoveAll(m.txDir(tx.ID))
		return PluginInfo{}, err
	}
	next := cloneState(m.state)
	next.Plugins[id] = after
	control.enter("committing")
	if err := m.setPhase(tx, "committing"); err != nil {
		_ = os.RemoveAll(m.txDir(tx.ID))
		return PluginInfo{}, err
	}
	if err := m.saveStateLocked(next); err != nil {
		_ = os.RemoveAll(m.txDir(tx.ID))
		return PluginInfo{}, err
	}
	m.state = next
	control.enter("state_committed")
	if m.afterPhase != nil {
		if err := m.afterPhase("state_committed"); err != nil {
			return PluginInfo{}, internalError(err)
		}
	}
	m.finishTransaction(tx)
	return m.pluginInfoLocked(id)
}

func (m *Manager) Uninstall(id string) (retErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.state.Plugins[id]
	if before == nil {
		return notFound(id)
	}
	if _, err := m.pluginInfoLocked(id); err != nil {
		return err
	}
	tx, err := m.beginTransaction("uninstall", id, before, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// If state was actually committed, retain that successful state.
		if m.state.Plugins[id] == nil {
			committed = true
			m.finishTransaction(tx)
			return
		}
		trash := filepath.Join(m.txDir(tx.ID), "trash")
		if _, err := os.Stat(trash); err == nil {
			_ = os.MkdirAll(filepath.Join(m.root, "packages"), 0o700)
			_ = os.Rename(trash, filepath.Join(m.root, "packages", id))
		}
		_ = os.RemoveAll(m.txDir(tx.ID))
	}()
	packageDir := filepath.Join(m.root, "packages", id)
	trash := filepath.Join(m.txDir(tx.ID), "trash")
	if err := os.Rename(packageDir, trash); err != nil {
		return internalError(fmt.Errorf("移入卸载事务区失败: %w", err))
	}
	if err := m.setPhase(tx, "package_removed"); err != nil {
		return err
	}
	next := cloneState(m.state)
	delete(next.Plugins, id)
	if err := m.setPhase(tx, "committing"); err != nil {
		return err
	}
	if err := m.saveStateLocked(next); err != nil {
		return err
	}
	m.state = next
	if m.afterPhase != nil {
		if err := m.afterPhase("state_committed"); err != nil {
			return internalError(err)
		}
	}
	committed = true
	m.finishTransaction(tx)
	return nil
}
