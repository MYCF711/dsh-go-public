package pluginmgr

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

func transactionError(format string, args ...any) *Error {
	return errorf("transaction_corrupt", 422, false,
		"保留 transactions 现场并修复矛盾；不要删除 packages", format, args...)
}

// Reconcile finishes or rolls back every journal according to the actual state.
// It is idempotent: a second call reports no recovered transactions.
func (m *Manager) Reconcile() (ReconcileResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := ReconcileResult{Actions: []ReconcileAction{}}
	if err := m.loadStateLocked(); err != nil {
		return result, err
	}
	entries, err := os.ReadDir(filepath.Join(m.root, "transactions"))
	if err != nil {
		return result, internalError(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if !entry.IsDir() {
			return result, transactionError("transactions 下出现未知文件 %s", entry.Name())
		}
		dir := filepath.Join(m.root, "transactions", entry.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "transaction.json"))
		if err != nil {
			return result, transactionError("事务 %s 缺 transaction.json: %v", entry.Name(), err)
		}
		var tx transaction
		if err := decodeStrict(raw, &tx); err != nil {
			return result, transactionError("事务 %s 损坏: %v", entry.Name(), err)
		}
		if tx.Schema != txSchema || tx.ID != entry.Name() || tx.PluginID == "" {
			return result, transactionError("事务身份不符: dir=%s tx=%+v", entry.Name(), tx)
		}
		if err := validateID(tx.PluginID); err != nil {
			return result, transactionError("事务 %s: %v", tx.ID, err)
		}
		if tx.Before != nil {
			if err := validateRecord(tx.PluginID, tx.Before); err != nil {
				return result, transactionError("事务 %s before 损坏: %v", tx.ID, err)
			}
		}
		if tx.After != nil {
			if err := validateRecord(tx.PluginID, tx.After); err != nil {
				return result, transactionError("事务 %s after 损坏: %v", tx.ID, err)
			}
		}
		action, err := m.reconcileOneLocked(&tx)
		if err != nil {
			return result, err
		}
		result.Actions = append(result.Actions, ReconcileAction{
			TransactionID: tx.ID, Action: action, ID: tx.PluginID,
			Detail: fmt.Sprintf("operation=%s phase=%s", tx.Operation, tx.Phase),
		})
		result.Recovered++
	}
	return result, nil
}

func (m *Manager) reconcileOneLocked(tx *transaction) (string, error) {
	current := m.state.Plugins[tx.PluginID]
	committed := false
	switch tx.Operation {
	case "install", "update", "restore":
		committed = tx.After != nil && reflect.DeepEqual(current, tx.After)
	case "uninstall":
		committed = current == nil && tx.Before != nil
	default:
		return "", transactionError("事务 %s 操作未知 %q", tx.ID, tx.Operation)
	}
	packageDir := filepath.Join(m.root, "packages", tx.PluginID)
	trash := filepath.Join(m.txDir(tx.ID), "trash")
	if committed {
		if tx.Operation == "uninstall" {
			if _, err := os.Stat(packageDir); err == nil {
				return "", transactionError("已提交卸载 %s 却重新出现包目录，拒绝猜归属", tx.PluginID)
			} else if !os.IsNotExist(err) {
				return "", internalError(err)
			}
		} else {
			manifest, err := readAndValidatePackage(m.packagePath(tx.PluginID, tx.After.Version))
			if err != nil || manifest.ID != tx.PluginID || manifest.Version != tx.After.Version {
				return "", transactionError("已提交事务 %s 的当前包缺失/损坏: %v", tx.ID, err)
			}
		}
		if err := os.RemoveAll(m.txDir(tx.ID)); err != nil {
			return "", internalError(err)
		}
		return "committed", nil
	}

	// Not committed: the durable record must still be exactly the old one.
	if !reflect.DeepEqual(current, tx.Before) {
		return "", transactionError("事务 %s 状态既非 before 也非 after，拒绝自动删除", tx.ID)
	}
	switch tx.Operation {
	case "install", "update":
		if tx.After == nil {
			return "", transactionError("事务 %s 缺 after", tx.ID)
		}
		if err := os.RemoveAll(m.packagePath(tx.PluginID, tx.After.Version)); err != nil {
			return "", internalError(err)
		}
		_ = os.Remove(filepath.Join(m.root, "packages", tx.PluginID))
	case "restore":
		// No package directory changed; the old current pointer remains valid.
	case "uninstall":
		if _, err := os.Stat(trash); err == nil {
			if _, err := os.Stat(packageDir); err == nil {
				return "", transactionError("卸载回滚时 trash 与 packages 同时存在: %s", tx.PluginID)
			} else if !os.IsNotExist(err) {
				return "", internalError(err)
			}
			if err := os.MkdirAll(filepath.Dir(packageDir), 0o700); err != nil {
				return "", internalError(err)
			}
			if err := os.Rename(trash, packageDir); err != nil {
				return "", internalError(fmt.Errorf("恢复卸载包目录失败: %w", err))
			}
		} else if os.IsNotExist(err) {
			if _, statErr := os.Stat(packageDir); statErr != nil {
				return "", transactionError("未提交卸载 %s 同时缺 trash 与 packages，拒绝静默丢包", tx.PluginID)
			}
		} else {
			return "", internalError(err)
		}
	}
	if err := os.RemoveAll(m.txDir(tx.ID)); err != nil {
		return "", internalError(err)
	}
	return "rolled_back", nil
}
