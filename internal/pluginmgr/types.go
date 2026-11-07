// Package pluginmgr manages the task-scoped dsh.local-plugin@1 directory format.
// It copies and verifies packages only; it never loads or executes plugin code.
package pluginmgr

import (
	"errors"
	"fmt"
	"net/http"
)

const (
	ManifestSchema = "dsh.local-plugin@1"
	stateSchema    = "dsh.local-plugin-state@1"
	txSchema       = "dsh.local-plugin-transaction@1"
)

// PluginInfo is the additive Transport projection of one installed package.
type PluginInfo struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	Kind            string `json:"kind"`
	Entry           string `json:"entry"`
	Enabled         bool   `json:"enabled"`
	Source          string `json:"source"`
	PreviousVersion string `json:"previousVersion,omitempty"`
}

// ReconcileAction describes one recovery decision; it is never a fabricated success.
type ReconcileAction struct {
	TransactionID string `json:"transactionId"`
	Action        string `json:"action"`
	ID            string `json:"id"`
	Detail        string `json:"detail"`
}

// ReconcileResult reports all incomplete transactions processed in this call.
type ReconcileResult struct {
	Actions   []ReconcileAction `json:"actions"`
	Recovered int               `json:"recovered"`
}

// Error is the stable four-field error used by both HTTP and CLI adapters.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
	Retryable bool   `json:"retryable"`
	Status    int    `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func errorf(code string, status int, retryable bool, hint, format string, args ...any) *Error {
	return &Error{Code: code, Status: status, Retryable: retryable, Hint: hint, Message: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) *Error {
	return errorf("bad_request", http.StatusBadRequest, false, "检查裸参数中的 id/path/version", format, args...)
}

func notFound(id string) *Error {
	return errorf("not_found", http.StatusNotFound, false, "先调用 plugin.list 核对已安装 id", "插件 %s 不存在", id)
}

func internalError(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return errorf("internal", http.StatusInternalServerError, true, "可重试；持续失败时检查管理器目录与事务记录", "%v", err)
}

// AsError converts an arbitrary I/O failure without discarding its message.
func AsError(err error) *Error { return internalError(err) }

// Manifest is the complete restricted local package manifest.
//
// Engines / PeerDependencies 是**可选**的兼容性字段（见 compat.go）：它们的语义
// 与官方 package.json 的同名字段一致，只用来回答"这个包能不能在当前的 dsh
// runtime 上跑"。没有这两个字段的包（既有全部包）行为不变。
type Manifest struct {
	Schema           string            `json:"schema"`
	ID               string            `json:"id"`
	Version          string            `json:"version"`
	Kind             string            `json:"kind"`
	Entry            string            `json:"entry"`
	Files            map[string]string `json:"files"`
	Engines          *Engines          `json:"engines,omitempty"`
	PeerDependencies map[string]string `json:"peerDependencies,omitempty"`
}

type versionSource struct {
	Source string `json:"source"`
}

type pluginRecord struct {
	ID              string                   `json:"id"`
	Version         string                   `json:"version"`
	Enabled         bool                     `json:"enabled"`
	PreviousVersion string                   `json:"previousVersion,omitempty"`
	Versions        map[string]versionSource `json:"versions"`
}

type persistentState struct {
	Schema  string                   `json:"schema"`
	Plugins map[string]*pluginRecord `json:"plugins"`
}

func emptyState() *persistentState {
	return &persistentState{Schema: stateSchema, Plugins: map[string]*pluginRecord{}}
}

func cloneState(in *persistentState) *persistentState {
	out := emptyState()
	for id, record := range in.Plugins {
		copyRecord := *record
		copyRecord.Versions = make(map[string]versionSource, len(record.Versions))
		for version, source := range record.Versions {
			copyRecord.Versions[version] = source
		}
		out.Plugins[id] = &copyRecord
	}
	return out
}
