package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestW149MCPHelperProcess 是由真实 dshd 拉起的独立 stdio MCP server。
// 协议 stdout 只能有 JSON-RPC；日志写单独文件，测试框架输出由 os.Exit 阻断。
func TestW149MCPHelperProcess(t *testing.T) {
	if os.Getenv("DSHD_W149_MCP_HELPER") != "1" {
		return
	}
	logPath := os.Getenv("DSHD_W149_MCP_LOG")
	record := func(method, cursor string) {
		raw, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "method": method, "cursor": cursor})
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.Write(append(raw, '\n'))
			_ = file.Close()
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  map[string]any  `json:"params"`
			JSONRPC string          `json:"jsonrpc"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if request.Method == "notifications/initialized" {
			continue
		}
		record(request.Method, fmt.Sprint(request.Params["cursor"]))
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch request.Method {
		case "initialize":
			response["result"] = map[string]any{
				"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "w149-fixture", "version": "1"},
			}
		case "tools/list":
			if fmt.Sprint(request.Params["cursor"]) == "page-2" {
				response["result"] = map[string]any{"tools": []any{map[string]any{"name": "second", "description": "second tool"}}}
			} else {
				response["result"] = map[string]any{
					"tools": []any{map[string]any{"name": "first", "description": "first tool"}}, "nextCursor": "page-2",
				}
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unknown method"}
		}
		raw, _ := json.Marshal(response)
		if _, err := os.Stdout.Write(append(raw, '\n')); err != nil {
			os.Exit(3)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	os.Exit(0)
}

type w149MCPLogRow struct {
	PID    int    `json:"pid"`
	Method string `json:"method"`
	Cursor string `json:"cursor"`
}

func readW149MCPLog(t *testing.T, path string) []w149MCPLogRow {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var rows []w149MCPLogRow
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var row w149MCPLogRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func writeW149MCPConfig(t *testing.T, home string, enabled bool, logPath string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"preserveTopLevel": map[string]any{"keep": true},
		"mcpServers": map[string]any{
			"docs": map[string]any{
				"command": os.Args[0], "args": []string{"-test.run=^TestW149MCPHelperProcess$"},
				"env":     map[string]string{"DSHD_W149_MCP_HELPER": "1", "DSHD_W149_MCP_LOG": logPath},
				"enabled": enabled, "preserveServerField": "keep",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "mcp.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func w149MCPList(t *testing.T, client *http.Client, base, sid string) map[string]any {
	t.Helper()
	result := postPluginGlue(t, client, base, "mcp.list", map[string]any{"id": sid}, http.StatusOK)["result"].(map[string]any)
	servers, ok := result["servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("servers=%+v", result["servers"])
	}
	return servers[0].(map[string]any)
}

func requireW149MCPState(t *testing.T, row map[string]any, status string, toolCount any) {
	t.Helper()
	if row["name"] != "docs" || row["status"] != status {
		t.Fatalf("MCP row=%+v want status=%s", row, status)
	}
	if toolCount == nil {
		if _, exists := row["toolCount"]; exists {
			t.Fatalf("%s 不应残留 toolCount: %+v", status, row)
		}
	} else if row["toolCount"] != toolCount {
		t.Fatalf("toolCount=%v want=%v row=%+v", row["toolCount"], toolCount, row)
	}
}

func TestW149RealDSHDMCPLifecycleABAndRestart(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	homeA, homeB := t.TempDir(), t.TempDir()
	logA := filepath.Join(t.TempDir(), "mcp-a.jsonl")
	writeW149MCPConfig(t, homeA, true, logA)
	writeW149MCPConfig(t, homeB, false, filepath.Join(t.TempDir(), "mcp-b.jsonl"))
	const sidA, sidB = "session-w149-mcp-a", "session-w149-mcp-b"
	createServeLifecycleSession(t, homeA, sidA)
	createServeLifecycleSession(t, homeB, sidB)
	decoy := t.TempDir()
	serverA := startPluginGlueServer(t, exe, homeA, decoy)
	serverB := startPluginGlueServer(t, exe, homeB, decoy)
	clientA := connectPluginGlueServer(t, serverA)
	clientB := connectPluginGlueServer(t, serverB)

	requireW149MCPState(t, w149MCPList(t, clientA, serverA.base, sidA), "connected", float64(2))
	requireW149MCPState(t, w149MCPList(t, clientB, serverB.base, sidB), "disabled", nil)
	initial := readW149MCPLog(t, logA)
	if len(initial) != 3 || initial[0].Method != "initialize" || initial[1].Method != "tools/list" || initial[2].Cursor != "page-2" {
		t.Fatalf("initialize/tools pagination log=%+v", initial)
	}
	postPluginGlue(t, clientA, serverA.base, "mcp.list", map[string]any{"id": "session-missing"}, http.StatusNotFound)
	postPluginGlue(t, clientA, serverA.base, "mcp.reconnect", map[string]any{"id": sidA, "name": "unknown"}, http.StatusNotFound)

	postPluginGlue(t, clientA, serverA.base, "mcp.reconnect", map[string]any{"id": sidA, "name": "docs"}, http.StatusOK)
	reconnected := readW149MCPLog(t, logA)
	if len(reconnected) != 6 || reconnected[3].PID == initial[0].PID {
		t.Fatalf("reconnect 未创建新一代真实进程: before=%+v after=%+v", initial, reconnected)
	}
	requireW149MCPState(t, w149MCPList(t, clientA, serverA.base, sidA), "connected", float64(2))

	postPluginGlue(t, clientA, serverA.base, "mcp.toggle", map[string]any{"id": sidA, "name": "docs", "enabled": false}, http.StatusOK)
	requireW149MCPState(t, w149MCPList(t, clientA, serverA.base, sidA), "disabled", nil)
	rawConfig, err := os.ReadFile(filepath.Join(homeA, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		PreserveTopLevel map[string]any `json:"preserveTopLevel"`
		MCPServers       map[string]struct {
			Enabled             bool   `json:"enabled"`
			PreserveServerField string `json:"preserveServerField"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(rawConfig, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.MCPServers["docs"].Enabled || persisted.MCPServers["docs"].PreserveServerField != "keep" || persisted.PreserveTopLevel["keep"] != true {
		t.Fatalf("toggle 持久化覆盖其它字段: %s", rawConfig)
	}
	beforeDisabledRestart := len(readW149MCPLog(t, logA))
	serverA.stop()
	serverA = startPluginGlueServer(t, exe, homeA, decoy)
	clientA = connectPluginGlueServer(t, serverA)
	requireW149MCPState(t, w149MCPList(t, clientA, serverA.base, sidA), "disabled", nil)
	if got := len(readW149MCPLog(t, logA)); got != beforeDisabledRestart {
		t.Fatalf("disabled restart 又拉起 MCP: before=%d after=%d", beforeDisabledRestart, got)
	}

	postPluginGlue(t, clientA, serverA.base, "mcp.toggle", map[string]any{"id": sidA, "name": "docs", "enabled": true}, http.StatusOK)
	requireW149MCPState(t, w149MCPList(t, clientA, serverA.base, sidA), "connected", float64(2))
	beforeEnabledRestart := readW149MCPLog(t, logA)
	serverA.stop()
	serverA = startPluginGlueServer(t, exe, homeA, decoy)
	clientA = connectPluginGlueServer(t, serverA)
	requireW149MCPState(t, w149MCPList(t, clientA, serverA.base, sidA), "connected", float64(2))
	afterEnabledRestart := readW149MCPLog(t, logA)
	if len(afterEnabledRestart) != len(beforeEnabledRestart)+3 || afterEnabledRestart[len(beforeEnabledRestart)].PID == beforeEnabledRestart[0].PID {
		t.Fatalf("enabled restart 未恢复真实连接: before=%+v after=%+v", beforeEnabledRestart, afterEnabledRestart)
	}
	// A 的整段动作不得改 B 的真实禁用状态。
	requireW149MCPState(t, w149MCPList(t, clientB, serverB.base, sidB), "disabled", nil)
	t.Log("W149_MCP_SUMMARY OK=12 FAIL=0")
}

func TestW149MCPManagerCloseReclaimsFixture(t *testing.T) {
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "mcp-close.jsonl")
	writeW149MCPConfig(t, home, true, logPath)
	manager := openMCPManager(home)
	manager.StartAll()
	rows, err := manager.List()
	if err != nil || len(rows) != 1 || rows[0].Status != "connected" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	conn := manager.servers["docs"].conn
	if conn == nil || conn.cmd == nil || conn.cmd.Process == nil {
		t.Fatal("connected server 没有真实子进程")
	}
	manager.Close()
	select {
	case <-conn.done:
	case <-time.After(5 * time.Second):
		t.Fatal("manager.Close 未等待 MCP 子进程退出")
	}
	if manager.servers["docs"].conn != nil {
		t.Fatal("Close 后仍持有连接")
	}
}
