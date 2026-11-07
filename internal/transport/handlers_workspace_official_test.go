package transport

// 官方桌面端 workspace / workspaceFiles / directoryPicker 16 个方法的**契约测试**。
//
// 这些测试只打本包的 `serveOfficialWorkspaceAPI`（Lead 接线后由 /api 路由调它），
// 因此不依赖 server.go 的分发表——路由接线是 Lead 的验收项，这里用
// `IsOfficialWorkspaceMethod` 钉住"方法名集合"，避免新增方法从路由漏下去。
//
// 真实持久化、真实文件系统边界、真实会话归属在 `cmd/dshd/serve_workspace_api_test.go`
// （那一侧用 `transport.ServeOfficialWorkspace` 走同一条 transport 代码路径）。
// 本文件只证明：参数形状、官方 zod 字段、四字段错误、A/B 隔离。

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// officialWorkspaceTestServer 造一个挂了官方 workspace 实现的 Server。
func officialWorkspaceTestServer(t *testing.T, api OfficialWorkspaceAPI, picker OfficialDirectoryPickerAPI) *Server {
	t.Helper()
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.AttachOfficialWorkspaceAPI(api)
	srv.AttachOfficialDirectoryPickerAPI(picker)
	t.Cleanup(srv.DetachOfficialWorkspaceAPI)
	t.Cleanup(srv.DetachOfficialDirectoryPickerAPI)
	return srv
}

// callOfficialWorkspace 打一次方法，返回 HTTP 状态与解码后的信封。
func callOfficialWorkspace(t *testing.T, srv *Server, method string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case nil:
		raw = nil
	case string:
		raw = []byte(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		raw = encoded
	}
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.serveOfficialWorkspaceAPI(rec, req, method)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
	}
	return rec.Code, out
}

// officialWorkspaceResult 取出 {"result": …} 里的载荷。
func officialWorkspaceResult(t *testing.T, method string, out map[string]any) map[string]any {
	t.Helper()
	raw, ok := out["result"]
	if !ok {
		t.Fatalf("%s 的响应没有 result：%v", method, out)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s 的 result 不是对象：%v", method, raw)
	}
	return obj
}

// officialWorkspaceErrorCode 取错误信封里的 message（官方码写在 message 前缀里）。
func officialWorkspaceErrorMessage(t *testing.T, out map[string]any) string {
	t.Helper()
	obj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("错误信封缺 error 对象：%v", out)
	}
	msg, _ := obj["message"].(string)
	return msg
}

// TestOfficialWorkspaceMethodSet 钉住 16 个方法名与路由判定。
func TestOfficialWorkspaceMethodSet(t *testing.T) {
	methods := OfficialWorkspaceMethods()
	want := []string{
		"workspace.create", "workspace.rename", "workspace.delete",
		"workspace.insertBefore", "workspace.insertSessionBefore",
		"workspace.pinSession", "workspace.unpinSession",
		"workspace.archiveSession", "workspace.unarchiveSession",
		"workspaceFiles.list", "workspaceFiles.read", "workspaceFiles.readBytes",
		"workspaceFiles.stat", "workspaceFiles.changes",
		"directoryPicker.list", "directoryPicker.createDirectory",
	}
	if len(methods) != len(want) {
		t.Fatalf("官方 workspace 方法应是 %d 个，实际 %d：%v", len(want), len(methods), methods)
	}
	seen := map[Method]bool{}
	for i, m := range methods {
		if string(m) != want[i] {
			t.Fatalf("第 %d 个方法名是 %q，期望 %q（顺序固定）", i, m, want[i])
		}
		if seen[m] {
			t.Fatalf("方法重复：%s", m)
		}
		seen[m] = true
		if !IsOfficialWorkspaceMethod(string(m)) {
			t.Fatalf("%s 没被 IsOfficialWorkspaceMethod 认出来（路由会漏）", m)
		}
		if strings.Contains(string(m), "/") {
			t.Fatalf("%s 含 '/'：本仓方法名用点号", m)
		}
	}
	// 官方既有方法不得被这一族吞掉：workspace.list / workspace.init 是宿主独有面。
	for _, other := range []string{"workspace.list", "workspace.init", "session.list", "fs.read"} {
		if IsOfficialWorkspaceMethod(other) {
			t.Fatalf("%s 不属于官方 workspace 族，不该被认领", other)
		}
	}
}

// TestOfficialWorkspaceNotAttachedIsNotImplemented 没接线时 16 个方法都必须
// 明确 501（四字段），不许静默成功、不许拿假数据顶上。
func TestOfficialWorkspaceNotAttachedIsNotImplemented(t *testing.T) {
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// 每个方法给一份**形状合法**的参数：未接线的判定必须发生在参数通过之后，
	// 否则"没接线"会被参数错误盖住，客户端以为是自己传错了。
	absParent := filepath.Join(os.TempDir(), "official-workspace-parent")
	valid := map[string]string{
		"workspace.create":              `{"path":"x"}`,
		"workspace.rename":              `{"workspaceId":"w","title":"t"}`,
		"workspace.delete":              `{"workspaceId":"w"}`,
		"workspace.insertBefore":        `{"workspaceId":"w"}`,
		"workspace.insertSessionBefore": `{"workspaceId":"w","sessionId":"s"}`,
		"workspace.pinSession":          `{"sessionId":"s"}`,
		"workspace.unpinSession":        `{"sessionId":"s"}`,
		"workspace.archiveSession":      `{"sessionId":"s"}`,
		"workspace.unarchiveSession":    `{"sessionId":"s"}`,
		"workspaceFiles.list":           `{"workspaceFileScopeId":"s","path":""}`,
		"workspaceFiles.read":           `{"workspaceFileScopeId":"s","path":"a.txt","range":{}}`,
		"workspaceFiles.readBytes":      `{"workspaceFileScopeId":"s","path":"a.bin","options":{}}`,
		"workspaceFiles.stat":           `{"workspaceFileScopeId":"s","path":"a.txt"}`,
		"workspaceFiles.changes":        `{"workspaceFileScopeId":"s","path":"a.txt"}`,
		"directoryPicker.list":          `{}`,
	}
	createDirBody, err := json.Marshal(map[string]any{"path": absParent, "name": "n"})
	if err != nil {
		t.Fatalf("序列化 createDirectory 请求: %v", err)
	}
	valid["directoryPicker.createDirectory"] = string(createDirBody)
	for _, m := range OfficialWorkspaceMethods() {
		body, ok := valid[string(m)]
		if !ok {
			t.Fatalf("%s 没有配一份合法参数（测试自己的漏项）", m)
		}
		code, out := callOfficialWorkspace(t, srv, string(m), body)
		if code != http.StatusNotImplemented {
			t.Fatalf("%s 未接线时应回 501，实际 %d（body=%v）", m, code, out)
		}
		assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
		if msg := officialWorkspaceErrorMessage(t, out); !strings.Contains(msg, string(m)) {
			t.Fatalf("%s 的未接线错误没点名方法：%q", m, msg)
		}
	}
}

// TestOfficialWorkspaceCreateIsIdempotentShape 官方 create 的两种参数写法与结果字段。
func TestOfficialWorkspaceCreateIsIdempotentShape(t *testing.T) {
	calls := 0
	api := OfficialWorkspaceAPI{
		Create: func(path string) (OfficialWorkspaceCreateValue, error) {
			calls++
			if path != "/tmp/ws-a" {
				t.Fatalf("宿主收到的 path 是 %q", path)
			}
			return OfficialWorkspaceCreateValue{
				Workspace: OfficialWorkspace{
					WorkspaceID: "ws-a", Path: path, Title: "ws-a",
					SessionIDs: []string{}, CreatedAt: "2026-11-07T00:00:00Z",
					UpdatedAt: "2026-11-07T00:00:00Z",
				},
				Created: calls == 1,
			}, nil
		},
	}
	srv := officialWorkspaceTestServer(t, api, OfficialDirectoryPickerAPI{})

	// 嵌套 request 写法（官方 wire 是对象参数）。
	code, out := callOfficialWorkspace(t, srv, "workspace.create", map[string]any{"request": map[string]any{"path": "/tmp/ws-a"}})
	if code != http.StatusOK {
		t.Fatalf("create 应 200，实际 %d（%v）", code, out)
	}
	res := officialWorkspaceResult(t, "workspace.create", out)
	if created, _ := res["created"].(bool); !created {
		t.Fatalf("第一次 create 应 created=true：%v", res)
	}
	ws, _ := res["workspace"].(map[string]any)
	for _, key := range []string{"workspaceId", "path", "title", "sessionIds", "createdAt", "updatedAt"} {
		if _, has := ws[key]; !has {
			t.Fatalf("create 结果缺 %s：%v", key, ws)
		}
	}
	if ids, ok := ws["sessionIds"].([]any); !ok || len(ids) != 0 {
		t.Fatalf("空工作区 sessionIds 应是空数组（不是 null）：%v", ws["sessionIds"])
	}

	// 平铺写法：同一个请求的另一种形状。
	code, out = callOfficialWorkspace(t, srv, "workspace.create", map[string]any{"path": "/tmp/ws-a"})
	if code != http.StatusOK {
		t.Fatalf("第二次 create 应 200，实际 %d（%v）", code, out)
	}
	res = officialWorkspaceResult(t, "workspace.create", out)
	if created, _ := res["created"].(bool); created {
		t.Fatalf("第二次 create 应 created=false：%v", res)
	}

	// 两种写法同时给 → 400（不猜哪个是真的）。
	code, out = callOfficialWorkspace(t, srv, "workspace.create", map[string]any{
		"request": map[string]any{"path": "/tmp/ws-a"}, "path": "/tmp/ws-b",
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")

	// 缺 path / 未知字段 / 非字符串 → 400，且**不得**打到宿主。
	before := calls
	for _, body := range []string{`{}`, `{"path":1}`, `{"path":"/tmp/ws-a","extra":1}`, `[]`, ``} {
		code, out = callOfficialWorkspace(t, srv, "workspace.create", body)
		if code != http.StatusBadRequest {
			t.Fatalf("非法 create 请求 %q 应 400，实际 %d（%v）", body, code, out)
		}
		assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	}
	if calls != before {
		t.Fatalf("非法请求打到了宿主（calls %d → %d）", before, calls)
	}
}

// TestOfficialWorkspaceMutationsForwardToHost 逐个钉住 8 个变更方法的参数与结果形状。
func TestOfficialWorkspaceMutationsForwardToHost(t *testing.T) {
	ws := OfficialWorkspace{
		WorkspaceID: "ws-a", Path: "/tmp/a", Title: "A",
		SessionIDs: []string{"s2", "s1"},
		CreatedAt:  "2026-11-07T00:00:00Z", UpdatedAt: "2026-11-07T00:00:01Z",
	}
	var got struct {
		renameTitle    string
		deleted        string
		orderTo        string
		orderBefore    string
		sessionTo      string
		sessionBefore  string
		pin            string
		unpin          string
		archived       string
		stopActivity   bool
		unarchivedCall string
	}
	api := OfficialWorkspaceAPI{
		Rename: func(workspaceID, title string) (OfficialWorkspace, error) {
			if workspaceID != "ws-a" {
				t.Fatalf("rename 收到的 workspaceId=%q", workspaceID)
			}
			got.renameTitle = title
			out := ws
			out.Title = title
			return out, nil
		},
		Delete: func(workspaceID string) error { got.deleted = workspaceID; return nil },
		InsertBefore: func(workspaceID, beforeWorkspaceID string) ([]string, error) {
			got.orderTo, got.orderBefore = workspaceID, beforeWorkspaceID
			return []string{"ws-b", "ws-a"}, nil
		},
		InsertSessionBefore: func(workspaceID, sessionID, beforeSessionID string) (OfficialWorkspace, error) {
			got.sessionTo, got.sessionBefore = sessionID, beforeSessionID
			out := ws
			out.SessionIDs = []string{"s1", "s2"}
			return out, nil
		},
		PinSession: func(sessionID string) ([]string, error) {
			got.pin = sessionID
			return []string{sessionID, "s0"}, nil
		},
		UnpinSession: func(sessionID string) ([]string, error) {
			got.unpin = sessionID
			return []string{"s0"}, nil
		},
		ArchiveSession: func(sessionID string, stopActivity bool) ([]string, error) {
			got.archived, got.stopActivity = sessionID, stopActivity
			return []string{sessionID}, nil
		},
		UnarchiveSession: func(sessionID string) ([]string, error) {
			got.unarchivedCall = sessionID
			return []string{}, nil
		},
	}
	srv := officialWorkspaceTestServer(t, api, OfficialDirectoryPickerAPI{})

	code, out := callOfficialWorkspace(t, srv, "workspace.rename", map[string]any{"workspaceId": "ws-a", "title": "新名字"})
	if code != http.StatusOK {
		t.Fatalf("rename 应 200，实际 %d（%v）", code, out)
	}
	if got.renameTitle != "新名字" {
		t.Fatalf("rename 没把 title 原样交给宿主：%q", got.renameTitle)
	}
	if title, _ := officialWorkspaceResult(t, "workspace.rename", out)["workspace"].(map[string]any)["title"].(string); title != "新名字" {
		t.Fatalf("rename 结果没回显新 title：%v", out)
	}
	// 空白 title 官方是 gateway/bad-request：运输层直接 400，不打宿主。
	code, out = callOfficialWorkspace(t, srv, "workspace.rename", map[string]any{"workspaceId": "ws-a", "title": "   "})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	if got.renameTitle != "新名字" {
		t.Fatalf("空白 title 打到了宿主：%q", got.renameTitle)
	}

	code, out = callOfficialWorkspace(t, srv, "workspace.delete", map[string]any{"workspaceId": "ws-a"})
	if code != http.StatusOK {
		t.Fatalf("delete 应 200，实际 %d（%v）", code, out)
	}
	if deleted, _ := officialWorkspaceResult(t, "workspace.delete", out)["deleted"].(bool); !deleted {
		t.Fatalf("delete 结果应是 {deleted:true}：%v", out)
	}
	if got.deleted != "ws-a" {
		t.Fatalf("delete 没把 workspaceId 交给宿主：%q", got.deleted)
	}

	code, out = callOfficialWorkspace(t, srv, "workspace.insertBefore", map[string]any{"workspaceId": "ws-a", "beforeWorkspaceId": "ws-b"})
	if code != http.StatusOK {
		t.Fatalf("insertBefore 应 200，实际 %d（%v）", code, out)
	}
	order, _ := officialWorkspaceResult(t, "workspace.insertBefore", out)["workspaceIds"].([]any)
	if len(order) != 2 || order[0] != "ws-b" || order[1] != "ws-a" {
		t.Fatalf("insertBefore 返回的完整顺序不对：%v", out)
	}
	if got.orderTo != "ws-a" || got.orderBefore != "ws-b" {
		t.Fatalf("insertBefore 参数转发不对：%+v", got)
	}
	// beforeWorkspaceId 省略 = 移到末尾（官方 optional anchor）。
	code, out = callOfficialWorkspace(t, srv, "workspace.insertBefore", map[string]any{"workspaceId": "ws-a"})
	if code != http.StatusOK {
		t.Fatalf("insertBefore 省略 anchor 应 200，实际 %d（%v）", code, out)
	}
	if got.orderBefore != "" {
		t.Fatalf("省略 anchor 时宿主应收到空串：%q", got.orderBefore)
	}

	code, out = callOfficialWorkspace(t, srv, "workspace.insertSessionBefore", map[string]any{
		"workspaceId": "ws-a", "sessionId": "s1", "beforeSessionId": "s2",
	})
	if code != http.StatusOK {
		t.Fatalf("insertSessionBefore 应 200，实际 %d（%v）", code, out)
	}
	ids, _ := officialWorkspaceResult(t, "workspace.insertSessionBefore", out)["workspace"].(map[string]any)["sessionIds"].([]any)
	if len(ids) != 2 || ids[0] != "s1" {
		t.Fatalf("insertSessionBefore 结果 sessionIds 顺序不对：%v", out)
	}
	if got.sessionTo != "s1" || got.sessionBefore != "s2" {
		t.Fatalf("insertSessionBefore 参数转发不对：%+v", got)
	}

	code, out = callOfficialWorkspace(t, srv, "workspace.pinSession", map[string]any{"sessionId": "s1"})
	if code != http.StatusOK {
		t.Fatalf("pinSession 应 200，实际 %d（%v）", code, out)
	}
	pinned, _ := officialWorkspaceResult(t, "workspace.pinSession", out)["pinnedSessionIds"].([]any)
	if len(pinned) != 2 || pinned[0] != "s1" {
		t.Fatalf("pinSession 结果应是最新在前：%v", out)
	}
	if got.pin != "s1" {
		t.Fatalf("pinSession 没把 sessionId 交给宿主：%q", got.pin)
	}

	code, out = callOfficialWorkspace(t, srv, "workspace.unpinSession", map[string]any{"sessionId": "s1"})
	if code != http.StatusOK {
		t.Fatalf("unpinSession 应 200，实际 %d（%v）", code, out)
	}
	if got.unpin != "s1" {
		t.Fatalf("unpinSession 没把 sessionId 交给宿主：%q", got.unpin)
	}

	code, out = callOfficialWorkspace(t, srv, "workspace.archiveSession", map[string]any{"sessionId": "s1"})
	if code != http.StatusOK {
		t.Fatalf("archiveSession 应 200，实际 %d（%v）", code, out)
	}
	if got.stopActivity {
		t.Fatalf("没传 stopActivity 时宿主不该收到 true")
	}
	archived, _ := officialWorkspaceResult(t, "workspace.archiveSession", out)["archivedSessionIds"].([]any)
	if len(archived) != 1 || archived[0] != "s1" {
		t.Fatalf("archiveSession 结果应是完整归档集：%v", out)
	}

	code, out = callOfficialWorkspace(t, srv, "workspace.unarchiveSession", map[string]any{"sessionId": "s1"})
	if code != http.StatusOK {
		t.Fatalf("unarchiveSession 应 200，实际 %d（%v）", code, out)
	}
	if got.unarchivedCall != "s1" {
		t.Fatalf("unarchiveSession 没把 sessionId 交给宿主：%q", got.unarchivedCall)
	}
	// 空集合必须序列化成 []，不是 null。
	if set, ok := officialWorkspaceResult(t, "workspace.unarchiveSession", out)["archivedSessionIds"].([]any); !ok || len(set) != 0 {
		t.Fatalf("空归档集应是 JSON 数组：%v", out)
	}
}

// TestOfficialWorkspaceArchiveActiveRequiresStopActivity 官方 `workspace/session-active`：
// 活动会话无 stopActivity 时拒绝（409 四字段），归档集不变；给了就真去请求停止。
func TestOfficialWorkspaceArchiveActiveRequiresStopActivity(t *testing.T) {
	active := true
	var stops []string
	api := OfficialWorkspaceAPI{
		ArchiveSession: func(sessionID string, stopActivity bool) ([]string, error) {
			if active && !stopActivity {
				return nil, OfficialWorkspaceError("workspace/session-active",
					"会话 %q 正在运行（turn + 2 个后台任务），先停或传 stopActivity=true", sessionID)
			}
			if stopActivity {
				stops = append(stops, sessionID)
			}
			return []string{sessionID, "s-old"}, nil
		},
	}
	srv := officialWorkspaceTestServer(t, api, OfficialDirectoryPickerAPI{})

	code, out := callOfficialWorkspace(t, srv, "workspace.archiveSession", map[string]any{"sessionId": "s1"})
	assertFourFieldError(t, code, out, http.StatusConflict, "revision_conflict")
	if msg := officialWorkspaceErrorMessage(t, out); !strings.Contains(msg, "workspace/session-active") {
		t.Fatalf("错误 message 必须带官方码 workspace/session-active：%q", msg)
	}
	if len(stops) != 0 {
		t.Fatalf("被拒绝时不得请求停止：%v", stops)
	}

	code, out = callOfficialWorkspace(t, srv, "workspace.archiveSession", map[string]any{"sessionId": "s1", "stopActivity": true})
	if code != http.StatusOK {
		t.Fatalf("带 stopActivity 应 200，实际 %d（%v）", code, out)
	}
	if len(stops) != 1 || stops[0] != "s1" {
		t.Fatalf("stopActivity=true 必须先请求停止：%v", stops)
	}
	set, _ := officialWorkspaceResult(t, "workspace.archiveSession", out)["archivedSessionIds"].([]any)
	if len(set) != 2 || set[0] != "s1" {
		t.Fatalf("归档集不对：%v", out)
	}
}

// TestOfficialWorkspaceErrorsAreFourField 官方各错误码到四字段信封的映射。
func TestOfficialWorkspaceErrorsAreFourField(t *testing.T) {
	cases := []struct {
		fault      string
		wantStatus int
		wantCode   string
	}{
		{"workspace/not-found", http.StatusNotFound, "not_found"},
		{"session/not-found", http.StatusNotFound, "not_found"},
		{"workspace/invalid-path", http.StatusBadRequest, "bad_request"},
		{"workspace/name-conflict", http.StatusBadRequest, "bad_request"},
		{"workspace/move-invalid", http.StatusBadRequest, "bad_request"},
		{"workspace/session-active", http.StatusConflict, "revision_conflict"},
		{"directory-picker/unreadable", http.StatusBadRequest, "bad_request"},
		{"directory-picker/exists", http.StatusBadRequest, "bad_request"},
		{"directory-picker/create-failed", http.StatusBadRequest, "bad_request"},
		{"workspaceFiles/outside-workspace", http.StatusBadRequest, "bad_request"},
	}
	for _, tc := range cases {
		api := OfficialWorkspaceAPI{
			Delete: func(string) error {
				return OfficialWorkspaceError(tc.fault, "宿主拒绝：%s", tc.fault)
			},
		}
		srv := officialWorkspaceTestServer(t, api, OfficialDirectoryPickerAPI{})
		code, out := callOfficialWorkspace(t, srv, "workspace.delete", map[string]any{"workspaceId": "ws-a"})
		assertFourFieldError(t, code, out, tc.wantStatus, tc.wantCode)
		if msg := officialWorkspaceErrorMessage(t, out); !strings.Contains(msg, tc.fault) {
			t.Fatalf("%s 的错误 message 丢了官方码：%q", tc.fault, msg)
		}
		// 兜底：认不出的错误必须是 500 internal，绝不能被渲染成成功。
		api.Delete = func(string) error { return errors.New("磁盘只读") }
		srv = officialWorkspaceTestServer(t, api, OfficialDirectoryPickerAPI{})
		code, out = callOfficialWorkspace(t, srv, "workspace.delete", map[string]any{"workspaceId": "ws-a"})
		assertFourFieldError(t, code, out, http.StatusInternalServerError, "internal")
	}
}

// TestOfficialWorkspaceFilesZodShapes 官方 workspaceFiles.list/read/stat 的字段与边界语义。
func TestOfficialWorkspaceFilesZodShapes(t *testing.T) {
	size := int64(42)
	var gotPath string
	api := OfficialWorkspaceAPI{
		FilesList: func(sessionID, path string) (OfficialWorkspaceDirectoryListing, error) {
			if sessionID != "s1" {
				t.Fatalf("scope（会话身份）没传到宿主：%q", sessionID)
			}
			gotPath = path
			return OfficialWorkspaceDirectoryListing{
				Path: "sub",
				Entries: []OfficialWorkspaceDirectoryEntry{
					{Name: "a.txt", Type: "file", Size: &size},
					{Name: "d", Type: "directory"},
				},
				Truncated: false,
			}, nil
		},
		FilesRead: func(sessionID, path string, r OfficialWorkspaceFileRange) (OfficialWorkspaceFileText, error) {
			if !r.HasOffset || r.Offset != 2 || !r.HasLimit || r.Limit != 1 {
				t.Fatalf("range 没解析对：%+v", r)
			}
			bytes := int64(99)
			return OfficialWorkspaceFileText{
				Offset: 2, Text: "second", Lines: 1, EOF: false,
				AbsolutePath: "/tmp/a/a.txt", Version: "v-1", Bytes: &bytes,
			}, nil
		},
		FilesStat: func(sessionID, path string) (OfficialWorkspaceFileStat, error) {
			return OfficialWorkspaceFileStat{AbsolutePath: "/tmp/a/a.txt", Version: "v-2", Bytes: &size}, nil
		},
	}
	srv := officialWorkspaceTestServer(t, api, OfficialDirectoryPickerAPI{})

	// scope 的官方 wire 名是 workspaceFileScopeId（typert lookup，wire 保留）。
	code, out := callOfficialWorkspace(t, srv, "workspaceFiles.list", map[string]any{
		"workspaceFileScopeId": "s1", "path": "sub",
	})
	if code != http.StatusOK {
		t.Fatalf("list 应 200，实际 %d（%v）", code, out)
	}
	if gotPath != "sub" {
		t.Fatalf("list 的 path 没交给宿主：%q", gotPath)
	}
	res := officialWorkspaceResult(t, "workspaceFiles.list", out)
	if res["path"] != "sub" {
		t.Fatalf("list 结果的 path 不对：%v", res)
	}
	if _, has := res["truncated"]; !has {
		t.Fatalf("list 结果缺 truncated：%v", res)
	}
	entries, _ := res["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries 数量不对：%v", res)
	}
	first, _ := entries[0].(map[string]any)
	if first["name"] != "a.txt" || first["type"] != "file" || first["size"] != float64(42) {
		t.Fatalf("第一项形状不对：%v", first)
	}
	if _, has := entries[1].(map[string]any)["size"]; has {
		t.Fatalf("目录项不该带 size：%v", entries[1])
	}

	// read：range 必填（官方对象参数），offset/limit 是整数。
	code, out = callOfficialWorkspace(t, srv, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": "s1", "path": "a.txt", "range": map[string]any{"offset": 2, "limit": 1},
	})
	if code != http.StatusOK {
		t.Fatalf("read 应 200，实际 %d（%v）", code, out)
	}
	res = officialWorkspaceResult(t, "workspaceFiles.read", out)
	for _, key := range []string{"offset", "text", "lines", "eof", "absolutePath", "version"} {
		if _, has := res[key]; !has {
			t.Fatalf("read 结果缺 %s：%v", key, res)
		}
	}
	if res["text"] != "second" || res["lines"] != float64(1) || res["eof"] != false {
		t.Fatalf("read 结果值不对：%v", res)
	}

	code, out = callOfficialWorkspace(t, srv, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": "s1", "path": "a.txt",
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	if msg := officialWorkspaceErrorMessage(t, out); !strings.Contains(msg, "range") {
		t.Fatalf("缺 range 的错误没说清缺什么：%q", msg)
	}

	code, out = callOfficialWorkspace(t, srv, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": "s1", "path": "a.txt", "range": map[string]any{"offset": 0},
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")

	code, out = callOfficialWorkspace(t, srv, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": "s1", "path": "a.txt", "range": map[string]any{"nope": 1},
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")

	code, out = callOfficialWorkspace(t, srv, "workspaceFiles.stat", map[string]any{
		"workspaceFileScopeId": "s1", "path": "a.txt",
	})
	if code != http.StatusOK {
		t.Fatalf("stat 应 200，实际 %d（%v）", code, out)
	}
	res = officialWorkspaceResult(t, "workspaceFiles.stat", out)
	if res["version"] != "v-2" || res["absolutePath"] != "/tmp/a/a.txt" {
		t.Fatalf("stat 结果不对：%v", res)
	}

	// 缺 scope / 未知字段 / 空 scope → 400（fail-loud，不拿进程默认值顶上）。
	for _, body := range []string{
		`{"path":"a.txt"}`,
		`{"workspaceFileScopeId":"","path":"a.txt"}`,
		`{"workspaceFileScopeId":"s1","path":"a.txt","extra":true}`,
		`{"workspaceFileScopeId":"s1"}`,
	} {
		method := "workspaceFiles.list"
		if strings.Contains(body, "a.txt") && !strings.Contains(body, "list") {
			method = "workspaceFiles.list"
		}
		code, out = callOfficialWorkspace(t, srv, method, body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 非法请求 %q 应 400，实际 %d（%v）", method, body, code, out)
		}
		assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	}
}

// TestOfficialWorkspaceFilesBytesAndChangesAreHonest
//
// bytes：官方 data 是 Uint8Array，L1 的 JSON 只能字符串——本仓用 base64 并显式标
// `encoding:"base64"`，不冒充二进制。
//
// changes：官方是 `mode:'stream'`，实现是 NDJSON 长连接（official_stream_sources.go）。
// 这个夹具**没有** FilesStat（宿主没接文件系统读入口），所以必须是
// 501 + 点名缺哪个能力——不是假 ready 帧。
func TestOfficialWorkspaceFilesBytesAndChangesAreHonest(t *testing.T) {
	bytesIn := int64(7)
	var gotOpts OfficialWorkspaceByteReadOptions
	api := OfficialWorkspaceAPI{
		FilesReadBytes: func(sessionID, path string, opts OfficialWorkspaceByteReadOptions) (OfficialWorkspaceFileBytes, error) {
			gotOpts = opts
			return OfficialWorkspaceFileBytes{
				Offset: 2, Data: "AQID", Encoding: "base64", EOF: true,
				AbsolutePath: "/tmp/a/a.bin", Version: "v-1", Bytes: &bytesIn,
			}, nil
		},
	}
	srv := officialWorkspaceTestServer(t, api, OfficialDirectoryPickerAPI{})

	code, out := callOfficialWorkspace(t, srv, "workspaceFiles.readBytes", map[string]any{
		"workspaceFileScopeId": "s1", "path": "a.bin",
		"options": map[string]any{"range": map[string]any{"offset": 2, "length": 3}, "baseFile": "a.bin"},
	})
	if code != http.StatusOK {
		t.Fatalf("readBytes 应 200，实际 %d（%v）", code, out)
	}
	res := officialWorkspaceResult(t, "workspaceFiles.readBytes", out)
	if res["data"] != "AQID" {
		t.Fatalf("readBytes 的 data 应是 base64 串：%v", res)
	}
	if res["encoding"] != "base64" {
		t.Fatalf("readBytes 必须显式标注 encoding=base64（官方是 Uint8Array）：%v", res)
	}
	for _, key := range []string{"offset", "eof", "absolutePath", "version"} {
		if _, has := res[key]; !has {
			t.Fatalf("readBytes 结果缺 %s：%v", key, res)
		}
	}
	if gotOpts.Range == nil || *gotOpts.Range.Offset != 2 || *gotOpts.Range.Length != 3 {
		t.Fatalf("range 没解析对：%+v", gotOpts)
	}
	if gotOpts.BaseFile == nil || *gotOpts.BaseFile != "a.bin" {
		t.Fatalf("baseFile 没解析对：%+v", gotOpts)
	}

	// options 必填（官方对象参数），省略即 400。
	code, out = callOfficialWorkspace(t, srv, "workspaceFiles.readBytes", map[string]any{
		"workspaceFileScopeId": "s1", "path": "a.bin",
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")

	// changes：宿主没有 FilesStat → 501 且**点名**缺的能力（不是假 ready 帧）。
	code, out = callOfficialWorkspace(t, srv, "workspaceFiles.changes", map[string]any{
		"workspaceFileScopeId": "s1", "path": "a.bin",
	})
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	msg := officialWorkspaceErrorMessage(t, out)
	if !strings.Contains(msg, "FilesStat") {
		t.Fatalf("changes 的未实现说明必须点名缺 FilesStat：%q", msg)
	}
}

// TestOfficialDirectoryPickerListAndCreate
//
// createDirectory 的 name 校验是官方 zod refine 的逐字搬运：非空白、不是 "."、
// 不是 ".."、不含 `/` 或 `\`。失败必须 400 且**不落盘**。
//
// 这里的路径用 os.TempDir() 拼：transport 要求 `path` 是**平台绝对路径**
// （Windows 上 "/home/u" 不是绝对路径），用写死的 POSIX 路径会把测试写死。
func TestOfficialDirectoryPickerListAndCreate(t *testing.T) {
	homeDir := filepath.Join(os.TempDir(), "official-picker-home")
	workDir := filepath.Join(homeDir, "work")
	var listPath string
	var gotParent, gotName string
	picker := OfficialDirectoryPickerAPI{
		List: func(path string) (OfficialDirectoryListing, error) {
			listPath = path
			return OfficialDirectoryListing{
				Path: homeDir, Home: homeDir,
				Crumbs: []OfficialDirectoryItem{
					{Name: filepath.VolumeName(homeDir) + string(filepath.Separator), Path: filepath.VolumeName(homeDir) + string(filepath.Separator)},
					{Name: homeDir, Path: homeDir},
				},
				Entries: []OfficialDirectoryItem{
					{Name: "work", Path: workDir},
					{Name: ".config", Path: filepath.Join(homeDir, ".config"), Hidden: true},
				},
				Truncated: false,
			}, nil
		},
		CreateDirectory: func(path, name string) (string, error) {
			gotParent, gotName = path, name
			return filepath.Join(path, name), nil
		},
	}
	srv := officialWorkspaceTestServer(t, OfficialWorkspaceAPI{}, picker)

	// path 省略 = 列 home（官方 acceptsUndefined:true）。
	code, out := callOfficialWorkspace(t, srv, "directoryPicker.list", `{}`)
	if code != http.StatusOK {
		t.Fatalf("list 应 200，实际 %d（%v）", code, out)
	}
	if listPath != "" {
		t.Fatalf("省略 path 时宿主应收到空串（列 home）：%q", listPath)
	}
	res := officialWorkspaceResult(t, "directoryPicker.list", out)
	for _, key := range []string{"path", "home", "crumbs", "entries", "truncated"} {
		if _, has := res[key]; !has {
			t.Fatalf("list 结果缺 %s：%v", key, res)
		}
	}
	crumbs, _ := res["crumbs"].([]any)
	if len(crumbs) != 2 {
		t.Fatalf("crumbs 数量不对：%v", res)
	}
	entry, _ := crumbs[0].(map[string]any)
	for _, key := range []string{"name", "path", "hidden"} {
		if _, has := entry[key]; !has {
			t.Fatalf("crumb 缺 %s：%v", key, entry)
		}
	}
	entries, _ := res["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries 数量不对：%v", res)
	}
	if hidden, _ := entries[1].(map[string]any)["hidden"].(bool); !hidden {
		t.Fatalf("隐藏项 hidden 应为 true：%v", entries[1])
	}

	// path 给了就原样交给宿主。
	code, out = callOfficialWorkspace(t, srv, "directoryPicker.list", map[string]any{"path": workDir})
	if code != http.StatusOK || listPath != workDir {
		t.Fatalf("list 的 path 没交给宿主：code=%d path=%q out=%v", code, listPath, out)
	}

	// createDirectory：成功时 result 是**字符串**（不是对象）。
	code, out = callOfficialWorkspace(t, srv, "directoryPicker.createDirectory", map[string]any{
		"path": homeDir, "name": "new",
	})
	if code != http.StatusOK {
		t.Fatalf("createDirectory 应 200，实际 %d（%v）", code, out)
	}
	if want := filepath.Join(homeDir, "new"); out["result"] != want {
		t.Fatalf("createDirectory 的 result 应是新目录绝对路径字符串：%v（期望 %q）", out, want)
	}
	if gotParent != homeDir || gotName != "new" {
		t.Fatalf("createDirectory 参数转发不对：%q %q", gotParent, gotName)
	}

	// 非法 name（官方 refine 逐条）+ 空 path + 相对 path：一律 400，且不打宿主。
	before := gotName
	// name 里带真反斜杠：用 json.Marshal 造，免得手写转义把用例写错。
	backslashJSON, berr := json.Marshal(map[string]any{"path": homeDir, "name": "a" + string(filepath.Separator) + "b"})
	if berr != nil {
		t.Fatalf("序列化反斜杠用例: %v", berr)
	}
	backslashBody := string(backslashJSON)
	bad := []string{
		`{"path":` + jsonQuotePath(homeDir) + `,"name":""}`,
		`{"path":` + jsonQuotePath(homeDir) + `,"name":"   "}`,
		`{"path":` + jsonQuotePath(homeDir) + `,"name":"."}`,
		`{"path":` + jsonQuotePath(homeDir) + `,"name":".."}`,
		`{"path":` + jsonQuotePath(homeDir) + `,"name":"a/b"}`,
		backslashBody,
		`{"path":"","name":"new"}`,
		`{"path":"relative/dir","name":"new"}`,
		`{"name":"new"}`,
		`{"path":` + jsonQuotePath(homeDir) + `}`,
	}
	for _, body := range bad {
		code, out = callOfficialWorkspace(t, srv, "directoryPicker.createDirectory", body)
		if code != http.StatusBadRequest {
			t.Fatalf("非法 createDirectory %q 应 400，实际 %d（%v）", body, code, out)
		}
		assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	}
	if gotName != before {
		t.Fatalf("非法 name 打到了宿主：%q", gotName)
	}

	// 没挂 picker 时明确 501（不是 400，也不是成功）。
	bare, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	code, out = callOfficialWorkspace(t, bare, "directoryPicker.list", `{}`)
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
}

// jsonQuotePath 把路径编成 JSON 字符串字面量（测试里手拼 body 用）。
func jsonQuotePath(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// TestOfficialWorkspaceTwoServersDoNotMix A/B 两个 Server 各挂各的实现，互不可见。
func TestOfficialWorkspaceTwoServersDoNotMix(t *testing.T) {
	mk := func(id string) OfficialWorkspaceAPI {
		return OfficialWorkspaceAPI{
			Create: func(path string) (OfficialWorkspaceCreateValue, error) {
				return OfficialWorkspaceCreateValue{
					Workspace: OfficialWorkspace{
						WorkspaceID: id, Path: path, Title: id,
						SessionIDs: []string{}, CreatedAt: "t", UpdatedAt: "t",
					},
					Created: true,
				}, nil
			},
		}
	}
	a := officialWorkspaceTestServer(t, mk("ws-a"), OfficialDirectoryPickerAPI{})
	b := officialWorkspaceTestServer(t, mk("ws-b"), OfficialDirectoryPickerAPI{})

	for _, tc := range []struct {
		srv  *Server
		want string
	}{{a, "ws-a"}, {b, "ws-b"}} {
		code, out := callOfficialWorkspace(t, tc.srv, "workspace.create", map[string]any{"path": "/tmp/x"})
		if code != http.StatusOK {
			t.Fatalf("create 应 200，实际 %d（%v）", code, out)
		}
		got, _ := officialWorkspaceResult(t, "workspace.create", out)["workspace"].(map[string]any)["workspaceId"].(string)
		if got != tc.want {
			t.Fatalf("A/B 串了：拿到 %q，期望 %q", got, tc.want)
		}
	}
	a.DetachOfficialWorkspaceAPI()
	code, out := callOfficialWorkspace(t, a, "workspace.create", map[string]any{"path": "/tmp/x"})
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	// B 仍然可用。
	code, out = callOfficialWorkspace(t, b, "workspace.create", map[string]any{"path": "/tmp/x"})
	if code != http.StatusOK {
		t.Fatalf("摘掉 A 之后 B 应仍可用，实际 %d（%v）", code, out)
	}
}

// ---------------------------------------------------------------------------
// workspaceFiles.changes 的目录目标（FilesStat 的目录分支）
// ---------------------------------------------------------------------------

// TestOfficialWorkspaceDirectoryTargetStatAndChanges
//
// 宿主用同一个 FilesStat 喂两个面：目录命中时置 IsDirectory。transport 必须
//
//  1. 让 unary `workspaceFiles.stat` 继续按官方契约拒绝目录（400 +
//     workspaceFiles/not-a-file），不把目录当文件报出去；
//  2. 让 `workspaceFiles.changes` 的目录目标产出官方帧——
//     {kind:"ready"} → {kind:"change",change:{absolutePath,version}} →
//     {kind:"change",change:{absolutePath,absent:true}}。
func TestOfficialWorkspaceDirectoryTargetStatAndChanges(t *testing.T) {
	dirAbs := filepath.Join(os.TempDir(), "official-watch-dir")
	if !filepath.IsAbs(dirAbs) {
		t.Fatalf("夹具的目录绝对路径不成立：%q", dirAbs)
	}
	var mu sync.Mutex
	version := "d1"
	absent := false
	dirStat := func(sessionID, path string) (OfficialWorkspaceFileStat, error) {
		if sessionID != "s1" || path != "dir" {
			t.Errorf("FilesStat 收到 %q %q（期望 s1 dir）", sessionID, path)
		}
		mu.Lock()
		defer mu.Unlock()
		if absent {
			return OfficialWorkspaceFileStat{}, OfficialWorkspaceError("workspaceFiles/not-found", "目录不存在：dir")
		}
		return OfficialWorkspaceFileStat{AbsolutePath: dirAbs, Version: version, IsDirectory: true}, nil
	}
	srv := officialWorkspaceTestServer(t, OfficialWorkspaceAPI{FilesStat: dirStat}, OfficialDirectoryPickerAPI{})

	// 1) unary stat：官方只认普通文件，目录必须 400 且点名 not-a-file。
	code, out := callOfficialWorkspace(t, srv, "workspaceFiles.stat", map[string]any{
		"workspaceFileScopeId": "s1", "path": "dir",
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
	if msg := officialWorkspaceErrorMessage(t, out); !strings.Contains(msg, "workspaceFiles/not-a-file") {
		t.Fatalf("目录 stat 必须报 workspaceFiles/not-a-file：%q", msg)
	}
	if _, has := out["result"]; has {
		t.Fatalf("目录 stat 不许有 result（不能把目录当文件）：%v", out)
	}

	// IsDirectory 只服务流内部，不进 wire：普通文件 stat 的 result 里不许出现它。
	srv.AttachOfficialWorkspaceAPI(OfficialWorkspaceAPI{
		FilesStat: func(string, string) (OfficialWorkspaceFileStat, error) {
			return OfficialWorkspaceFileStat{AbsolutePath: dirAbs, Version: "f1"}, nil
		},
	})
	code, out = callOfficialWorkspace(t, srv, "workspaceFiles.stat", map[string]any{
		"workspaceFileScopeId": "s1", "path": "dir",
	})
	if code != http.StatusOK {
		t.Fatalf("文件 stat 应 200，实际 %d（%v）", code, out)
	}
	res := officialWorkspaceResult(t, "workspaceFiles.stat", out)
	if _, has := res["isDirectory"]; has {
		t.Fatalf("IsDirectory 不该出现在 wire 上：%v", res)
	}
	// 2) changes：目录目标是官方允许的，必须真出帧（不是假 ready）。
	srv.AttachOfficialWorkspaceAPI(OfficialWorkspaceAPI{FilesStat: dirStat})
	rec, _ := runStream(t, func(w http.ResponseWriter, r *http.Request) {
		srv.serveOfficialWorkspaceAPI(w, r, "workspaceFiles.changes")
	}, `{"workspaceFileScopeId":"s1","path":"dir"}`)
	frames := rec.waitFrames(t, 1)
	if frames[0]["kind"] != "ready" {
		t.Fatalf("目录目标首帧必须是 ready：%v", frames[0])
	}

	mu.Lock()
	version = "d2"
	mu.Unlock()
	frames = rec.waitFrames(t, 2)
	if frames[1]["kind"] != "change" {
		t.Fatalf("目录版本变化必须发 change：%v", frames[1])
	}
	change, _ := frames[1]["change"].(map[string]any)
	if change["absolutePath"] != dirAbs || change["version"] != "d2" {
		t.Fatalf("目录 change 帧形状不对：%v", change)
	}
	if _, has := change["absent"]; has {
		t.Fatalf("目录存在时不许带 absent：%v", change)
	}

	mu.Lock()
	absent = true
	mu.Unlock()
	frames = rec.waitFrames(t, 3)
	change, _ = frames[2]["change"].(map[string]any)
	if change["absent"] != true || change["absolutePath"] != dirAbs {
		t.Fatalf("目录消失必须发 absent 帧：%v", frames[2])
	}
}
