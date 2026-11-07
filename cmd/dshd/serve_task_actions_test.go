package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"dshgo/internal/schedule"
)

func writeServeTaskSchedule(t *testing.T, home, sessionID string, items ...map[string]any) string {
	t.Helper()
	dir := filepath.Join(home, "state", "schedules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(map[string]any{"version": 1, "schedules": items}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, sessionID+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	revision, err := schedule.Revision(dir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func serveTaskItem(id, status string) map[string]any {
	return map[string]any{"id": id, "title": id, "prompt": "p", "kind": "daily", "daily_at": "09:00", "next_at": "2099-01-01T09:00:00+08:00", "status": status}
}

func TestTaskActionsRealDSHDReadyTokenScheduleAB(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	decoy := filepath.Join(t.TempDir(), "decoy-home")
	homeA := filepath.Join(t.TempDir(), "home-a")
	homeB := filepath.Join(t.TempDir(), "home-b")
	revA := writeServeTaskSchedule(t, homeA, "session-a", serveTaskItem("active-a", "running"), serveTaskItem("ended-a", "ended"))
	writeServeTaskSchedule(t, homeB, "session-b", serveTaskItem("active-b", "running"), serveTaskItem("ended-b", "ended"))

	serverA := startPluginGlueServer(t, exe, homeA, decoy)
	serverB := startPluginGlueServer(t, exe, homeB, decoy)
	anonymous := &http.Client{}
	for _, method := range []string{"schedule.delete", "schedule.clear", "jobs.output", "jobs.stop"} {
		envelope := postPluginGlue(t, anonymous, serverA.base, method, map[string]any{}, http.StatusUnauthorized)
		assertPluginGlueError(t, envelope, "unauthorized")
	}
	clientA := connectPluginGlueServer(t, serverA)
	clientB := connectPluginGlueServer(t, serverB)

	listed := postPluginGlue(t, clientA, serverA.base, "schedule.list", map[string]any{}, http.StatusOK)
	result := listed["result"].(map[string]any)
	rows := result["schedules"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["sessionId"] != "session-a" || rows[0].(map[string]any)["revision"] == "" {
		t.Fatalf("schedule.list 缺少删除所需真值：%+v", listed)
	}
	deleted := postPluginGlue(t, clientA, serverA.base, "schedule.delete", map[string]any{"sessionId": "session-a", "id": "active-a", "expectedRevision": revA}, http.StatusOK)
	if deleted["result"].(map[string]any)["deleted"] != true {
		t.Fatalf("delete response: %+v", deleted)
	}
	postPluginGlue(t, clientA, serverA.base, "schedule.delete", map[string]any{"sessionId": "session-a", "id": "active-a"}, http.StatusNotFound)
	postPluginGlue(t, clientA, serverA.base, "schedule.delete", map[string]any{"sessionId": "session-a", "id": "ended-a", "expectedRevision": "stale"}, http.StatusConflict)
	postPluginGlue(t, clientA, serverA.base, "schedule.clear", map[string]any{"params": map[string]any{}}, http.StatusBadRequest)
	cleared := postPluginGlue(t, clientA, serverA.base, "schedule.clear", map[string]any{}, http.StatusOK)
	clearResult := cleared["result"].(map[string]any)
	if clearResult["deleted"] != float64(1) || len(clearResult["schedules"].([]any)) != 0 {
		t.Fatalf("clear-ended/readback 不对：%+v", cleared)
	}

	// A 的删除与清空不能碰 B；随后 B 自己的 clear 只清 B 的 ended。
	itemsB, err := schedule.Load(filepath.Join(homeB, "state", "schedules"), "session-b")
	if err != nil || len(itemsB) != 2 {
		t.Fatalf("A 操作污染 B：items=%+v err=%v", itemsB, err)
	}
	clearedB := postPluginGlue(t, clientB, serverB.base, "schedule.clear", map[string]any{}, http.StatusOK)
	if clearedB["result"].(map[string]any)["deleted"] != float64(1) {
		t.Fatalf("B clear: %+v", clearedB)
	}
	itemsB, err = schedule.Load(filepath.Join(homeB, "state", "schedules"), "session-b")
	if err != nil || len(itemsB) != 1 || itemsB[0].ID != "active-b" {
		t.Fatalf("B 重开回读不对：items=%+v err=%v", itemsB, err)
	}
}
