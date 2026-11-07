package main

import (
	"testing"
)

// W149 先红：真实产品进程里没有 MCP 配置时必须给真实空目录，而不是 501。
func TestW149RealDSHDEmptyMCPList(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	home := t.TempDir()
	const sid = "session-w149-empty-mcp"
	createServeLifecycleSession(t, home, sid)
	server := startPluginGlueServer(t, exe, home, t.TempDir())
	client := connectPluginGlueServer(t, server)
	envelope := postPluginGlue(t, client, server.base, "mcp.list", map[string]any{"id": sid}, 200)
	result := envelope["result"].(map[string]any)
	servers, ok := result["servers"].([]any)
	if !ok || len(servers) != 0 {
		t.Fatalf("无 MCP 配置的 servers=%#v，要真实空数组", result["servers"])
	}
}
