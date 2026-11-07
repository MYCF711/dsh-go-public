package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"

	"dshgo/internal/pluginmgr"
)

// pluginParams is the union of the additive bare-parameter shapes. The HTTP
// layer checks the per-method key whitelist before calling this dispatcher.
type pluginParams struct {
	ID      string `json:"id,omitempty"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

type pluginListResult struct {
	Plugins []pluginmgr.PluginInfo `json:"plugins"`
}

type pluginResult struct {
	Plugin pluginmgr.PluginInfo `json:"plugin"`
}

type pluginUninstallResult struct {
	ID          string `json:"id"`
	Uninstalled bool   `json:"uninstalled"`
}

// pluginEnvelope is the only HTTP/CLI envelope constructor. Error is the typed
// four-field object; no call site can accidentally omit hint or retryable.
type pluginEnvelope struct {
	Result any              `json:"result,omitempty"`
	Error  *pluginmgr.Error `json:"error,omitempty"`
}

var pluginMethodParams = map[string][]string{
	"plugin.list":      {},
	"plugin.install":   {"path"},
	"plugin.enable":    {"id"},
	"plugin.disable":   {"id"},
	"plugin.uninstall": {"id"},
	"plugin.update":    {"id", "path"},
	"plugin.restore":   {"id", "version"},
	"plugin.reconcile": {},
}

func callPluginMethod(manager *pluginmgr.Manager, method string, params pluginParams) (any, error) {
	switch method {
	case "plugin.list":
		plugins, err := manager.List()
		if err != nil {
			return nil, err
		}
		return pluginListResult{Plugins: plugins}, nil
	case "plugin.install":
		plugin, err := manager.Install(params.Path)
		return pluginResult{Plugin: plugin}, err
	case "plugin.enable":
		plugin, err := manager.Enable(params.ID)
		return pluginResult{Plugin: plugin}, err
	case "plugin.disable":
		plugin, err := manager.Disable(params.ID)
		return pluginResult{Plugin: plugin}, err
	case "plugin.uninstall":
		if err := manager.Uninstall(params.ID); err != nil {
			return nil, err
		}
		return pluginUninstallResult{ID: params.ID, Uninstalled: true}, nil
	case "plugin.update":
		plugin, err := manager.Update(params.ID, params.Path)
		return pluginResult{Plugin: plugin}, err
	case "plugin.restore":
		plugin, err := manager.Restore(params.ID, params.Version)
		return pluginResult{Plugin: plugin}, err
	case "plugin.reconcile":
		return manager.Reconcile()
	default:
		return nil, pluginmgr.AsError(fmt.Errorf("unknown plugin method %s", method))
	}
}

// newPluginHTTPHandler serves only the additive /api/plugin.* methods.
// Production wiring must mount it behind the existing Transport authentication;
// these tests intentionally exercise this adapter directly over a real socket.
func newPluginHTTPHandler(manager *pluginmgr.Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		allowed, ok := pluginMethodParams[method]
		if r.Method != http.MethodPost || !ok || "/api/"+method != r.URL.Path {
			writePluginError(w, errorPluginHTTP("not_found", http.StatusNotFound, "检查 /api/plugin.* 路径与 POST 方法", "未知插件 Transport 方法"))
			return
		}
		var params pluginParams
		if err := decodePluginParams(r, method, allowed, &params); err != nil {
			writePluginError(w, err)
			return
		}
		result, err := callPluginMethod(manager, method, params)
		if err != nil {
			writePluginError(w, err)
			return
		}
		writePluginJSON(w, http.StatusOK, pluginEnvelope{Result: result})
	})
}

func errorPluginHTTP(code string, status int, hint, message string) *pluginmgr.Error {
	return &pluginmgr.Error{Code: code, Status: status, Hint: hint, Message: message, Retryable: code == "internal"}
}

func decodePluginParams(r *http.Request, method string, allowed []string, target *pluginParams) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return errorPluginHTTP("bad_request", http.StatusBadRequest, "直接发送该方法定义的参数对象", "读取参数失败: "+err.Error())
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errorPluginHTTP("bad_request", http.StatusBadRequest, "直接发送该方法定义的参数对象", method+" 参数必须是 JSON 裸对象")
	}
	if _, wrapped := fields["params"]; wrapped {
		return errorPluginHTTP("bad_request", http.StatusBadRequest, "直接发送该方法定义的参数对象", method+" 参数必须是裸对象，不接受 params 包装")
	}
	whitelist := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		whitelist[key] = true
	}
	for key := range fields {
		if !whitelist[key] {
			return errorPluginHTTP("bad_request", http.StatusBadRequest, "只发送快照中此 method 列出的参数", method+" 不接受参数 "+key)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errorPluginHTTP("bad_request", http.StatusBadRequest, "检查 id/path/version 都必须是字符串", method+" 参数类型错误: "+err.Error())
	}
	return nil
}

func writePluginError(w http.ResponseWriter, err error) {
	apiErr := pluginmgr.AsError(err)
	if apiErr.Status == 0 {
		apiErr.Status = http.StatusInternalServerError
	}
	writePluginJSON(w, apiErr.Status, pluginEnvelope{Error: apiErr})
}

func writePluginJSON(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		fallback := errorPluginHTTP("internal", http.StatusInternalServerError, "检查宿主序列化错误", err.Error())
		raw, _ = json.Marshal(pluginEnvelope{Error: fallback})
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}

// runPluginsCLI is the real CLI adapter used by the process-level tests. It has
// no default root: integrating it into main must pass an explicit temporary or
// product home subtree, never an implicit real-user plugin directory.
func runPluginsCLI(argv []string, stdout, stderr io.Writer) int {
	rootFlags := flag.NewFlagSet("plugins", flag.ContinueOnError)
	rootFlags.SetOutput(io.Discard)
	root := rootFlags.String("root", "", "插件管理器根目录（显式必填）")
	if err := rootFlags.Parse(argv); err != nil || *root == "" || rootFlags.NArg() == 0 {
		return writePluginCLIError(stderr, errorPluginHTTP("bad_request", http.StatusBadRequest,
			"用法：plugins --root <dir> <list|install|enable|disable|uninstall|update|restore|reconcile> [参数]", "CLI 需要显式 --root 与 action"))
	}
	action := rootFlags.Arg(0)
	method := "plugin." + action
	if _, ok := pluginMethodParams[method]; !ok {
		return writePluginCLIError(stderr, errorPluginHTTP("bad_request", http.StatusBadRequest,
			"action 只能是 list/install/enable/disable/uninstall/update/restore/reconcile", "未知插件 action: "+action))
	}
	flags := flag.NewFlagSet("plugins "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	id := flags.String("id", "", "插件 id")
	path := flags.String("path", "", "本地私有包目录")
	version := flags.String("version", "", "restore 的保留版本")
	if err := flags.Parse(rootFlags.Args()[1:]); err != nil || flags.NArg() != 0 {
		return writePluginCLIError(stderr, errorPluginHTTP("bad_request", http.StatusBadRequest, "检查 action 的 --id/--path/--version 旗标", "CLI 参数解析失败"))
	}
	manager, err := pluginmgr.Open(*root)
	if err != nil {
		return writePluginCLIError(stderr, err)
	}
	result, err := callPluginMethod(manager, method, pluginParams{ID: *id, Path: *path, Version: *version})
	if err != nil {
		return writePluginCLIError(stderr, err)
	}
	writePluginCLIJSON(stdout, pluginEnvelope{Result: result})
	return 0
}

func writePluginCLIError(writer io.Writer, err error) int {
	apiErr := pluginmgr.AsError(err)
	writePluginCLIJSON(writer, pluginEnvelope{Error: apiErr})
	return 1
}

func writePluginCLIJSON(writer io.Writer, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		fallback := errorPluginHTTP("internal", http.StatusInternalServerError, "检查宿主序列化错误", err.Error())
		raw, _ = json.Marshal(pluginEnvelope{Error: fallback})
	}
	_, _ = writer.Write(append(raw, '\n'))
}
