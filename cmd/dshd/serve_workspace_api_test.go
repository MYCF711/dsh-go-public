package main

// 官方 workspace / workspaceFiles / directoryPicker 的**宿主侧**测试：证明这些
// 方法读的是真东西（真 registry 文件、真 v4 会话、真文件系统），不是夹具。
//
// 分两层（两半都由本任务交付，Lead 接线只是把两半接起来）：
//
//	internal/transport/handlers_workspace_official_test.go  官方 zod 契约与四字段错误
//	本文件                                                  真实持久化 / 真实边界 / A/B
//
// 为什么不 POST /api/workspace.create：那 16 个 case 在 server.go 里，本轮写作用域
// 不允许改它。这里直接打 `transport.ServeOfficialWorkspace`——它就是 Lead 接线后
// /api 路由会调用的同一个函数，所以参数校验、官方字段、错误信封都走真实代码路径。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// officialWorkspaceHarness 是一套"真 home + 真 host + 真 transport 分派"的夹具。
type officialWorkspaceHarness struct {
	home   string
	host   *officialWorkspaceHost
	api    transport.OfficialWorkspaceAPI
	picker transport.OfficialDirectoryPickerAPI
}

func newOfficialWorkspaceHarness(t *testing.T) *officialWorkspaceHarness {
	t.Helper()
	home := t.TempDir()
	return newOfficialWorkspaceHarnessAt(t, home)
}

// newOfficialWorkspaceHarnessAt 复用同一个 home（模拟"进程重启后再读一遍磁盘"）。
func newOfficialWorkspaceHarnessAt(t *testing.T, home string) *officialWorkspaceHarness {
	t.Helper()
	host := newOfficialWorkspaceHost(home, nil, nil)
	if host == nil {
		t.Fatal("newOfficialWorkspaceHost 返回 nil")
	}
	return &officialWorkspaceHarness{home: home, host: host, api: host.workspaceAPI(), picker: host.directoryPickerAPI()}
}

// call 打一次方法，返回 HTTP 状态与信封。
func (h *officialWorkspaceHarness) call(t *testing.T, method string, body any) (int, map[string]any) {
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
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	transport.ServeOfficialWorkspace(rec, req, method, h.api, h.picker)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
	}
	return rec.Code, out
}

// ok 打一次方法并要求 200，返回 result 对象。
func (h *officialWorkspaceHarness) ok(t *testing.T, method string, body any) map[string]any {
	t.Helper()
	code, out := h.call(t, method, body)
	if code != http.StatusOK {
		t.Fatalf("%s 应 200，实际 %d（%v）", method, code, out)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s 的 result 不是对象：%v", method, out)
	}
	return res
}

// fail 打一次方法并要求指定状态码，返回错误 message（官方码写在 message 里）。
func (h *officialWorkspaceHarness) fail(t *testing.T, method string, body any, wantStatus int) string {
	t.Helper()
	code, out := h.call(t, method, body)
	if code != wantStatus {
		t.Fatalf("%s 应 %d，实际 %d（%v）", method, wantStatus, code, out)
	}
	obj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s 的错误信封缺 error 对象：%v", method, out)
	}
	for _, key := range []string{"code", "message", "hint", "retryable"} {
		if _, has := obj[key]; !has {
			t.Fatalf("%s 的错误信封缺 %s：%v", method, key, obj)
		}
	}
	msg, _ := obj["message"].(string)
	return msg
}

// officialWorkspaceView 取 result.workspace。
func officialWorkspaceView(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	ws, ok := res["workspace"].(map[string]any)
	if !ok {
		t.Fatalf("结果缺 workspace 对象：%v", res)
	}
	return ws
}

// officialWorkspaceStrings 取一个 JSON 字符串数组。
func officialWorkspaceStrings(t *testing.T, res map[string]any, key string) []string {
	t.Helper()
	raw, ok := res[key]
	if !ok {
		t.Fatalf("结果缺 %s：%v", key, res)
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s 不是数组（可能是 null）：%v", key, raw)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// newOfficialWorkspaceDir 建一个真实目录并返回它的规范路径。
func newOfficialWorkspaceDir(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %s: %v", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("取绝对路径 %s: %v", dir, err)
	}
	return filepath.Clean(abs)
}

// newRealSession 造一份真实 v4 会话（cwd 就是它的工作区根）。
func newRealSession(t *testing.T, home, cwd, id string) {
	t.Helper()
	lg, err := session.CreateSession(home, cwd, id, "standard")
	if err != nil {
		t.Fatalf("建真实会话 %s: %v", id, err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("关闭会话 %s: %v", id, err)
	}
}

// TestOfficialWorkspaceRegistryPersistsAcrossReload 9 个 workspace 方法的真实语义：
// 真 registry 文件、幂等 create、真排序、真重命名/删除、真 pin/archive 集合。
func TestOfficialWorkspaceRegistryPersistsAcrossReload(t *testing.T) {
	h := newOfficialWorkspaceHarness(t)
	base := t.TempDir()
	dirA := newOfficialWorkspaceDir(t, base, "alpha")
	dirB := newOfficialWorkspaceDir(t, base, "beta")

	// create A：created=true，title 取目录名，path 是规范绝对路径。
	res := h.ok(t, "workspace.create", map[string]any{"path": dirA})
	if created, _ := res["created"].(bool); !created {
		t.Fatalf("第一次 create 应 created=true：%v", res)
	}
	wsA := officialWorkspaceView(t, res)
	idA, _ := wsA["workspaceId"].(string)
	if idA == "" {
		t.Fatalf("workspaceId 不能为空：%v", wsA)
	}
	if got, _ := wsA["path"].(string); got != dirA {
		t.Fatalf("path 不是规范绝对路径：%q（期望 %q）", got, dirA)
	}
	if got, _ := wsA["title"].(string); got != filepath.Base(dirA) {
		t.Fatalf("title 应取目录名：%q", got)
	}
	for _, key := range []string{"createdAt", "updatedAt"} {
		ts, _ := wsA[key].(string)
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Fatalf("%s 不是真实 ISO-8601 时间：%q（%v）", key, ts, err)
		}
	}

	// registry 必须真的落盘（不是内存里的一张表）。
	registryPath := filepath.Join(h.home, "state", "workspaces.json")
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("registry 文件没落盘（%s）: %v", registryPath, err)
	}
	if !strings.Contains(string(raw), idA) {
		t.Fatalf("registry 文件里没有 %s：%s", idA, raw)
	}

	// 幂等：同一个目录再 create → created=false 且 id 不变。
	res = h.ok(t, "workspace.create", map[string]any{"path": dirA})
	if created, _ := res["created"].(bool); created {
		t.Fatalf("第二次 create 应 created=false：%v", res)
	}
	if got, _ := officialWorkspaceView(t, res)["workspaceId"].(string); got != idA {
		t.Fatalf("幂等 create 换了 id：%q → %q", idA, got)
	}

	// 不存在的目录 → 400 workspace/invalid-path（官方码在 message 里）。
	msg := h.fail(t, "workspace.create", map[string]any{"path": filepath.Join(base, "missing")}, http.StatusBadRequest)
	if !strings.Contains(msg, "workspace/invalid-path") {
		t.Fatalf("不存在的目录应报 workspace/invalid-path：%q", msg)
	}
	if _, err := os.Stat(filepath.Join(base, "missing")); !os.IsNotExist(err) {
		t.Fatalf("create 失败却建出了目录：%v", err)
	}

	// create B，然后 insertBefore(B, A) → 顺序 [B, A]。
	res = h.ok(t, "workspace.create", map[string]any{"path": dirB})
	idB, _ := officialWorkspaceView(t, res)["workspaceId"].(string)
	res = h.ok(t, "workspace.insertBefore", map[string]any{"workspaceId": idB, "beforeWorkspaceId": idA})
	if order := officialWorkspaceStrings(t, res, "workspaceIds"); !equalStrings(order, []string{idB, idA}) {
		t.Fatalf("insertBefore 顺序不对：%v", order)
	}
	// 省略 anchor = 移到末尾。
	res = h.ok(t, "workspace.insertBefore", map[string]any{"workspaceId": idB})
	if order := officialWorkspaceStrings(t, res, "workspaceIds"); !equalStrings(order, []string{idA, idB}) {
		t.Fatalf("insertBefore(省略 anchor) 顺序不对：%v", order)
	}
	// 未知 anchor → 404 workspace/not-found。
	msg = h.fail(t, "workspace.insertBefore", map[string]any{"workspaceId": idA, "beforeWorkspaceId": "ws-nope"}, http.StatusNotFound)
	if !strings.Contains(msg, "workspace/not-found") {
		t.Fatalf("未知 anchor 应报 workspace/not-found：%q", msg)
	}

	// rename：空白 title 400；重名 400 workspace/name-conflict；正常改名落盘。
	_ = h.fail(t, "workspace.rename", map[string]any{"workspaceId": idA, "title": "   "}, http.StatusBadRequest)
	msg = h.fail(t, "workspace.rename", map[string]any{"workspaceId": idA, "title": filepath.Base(dirB)}, http.StatusBadRequest)
	if !strings.Contains(msg, "workspace/name-conflict") {
		t.Fatalf("重名应报 workspace/name-conflict：%q", msg)
	}
	res = h.ok(t, "workspace.rename", map[string]any{"workspaceId": idA, "title": "  改名后  "})
	wsA = officialWorkspaceView(t, res)
	if got, _ := wsA["title"].(string); got != "改名后" {
		t.Fatalf("rename 应 trim 后落盘：%q", got)
	}
	if created, _ := wsA["createdAt"].(string); created == "" {
		t.Fatalf("rename 丢了 createdAt：%v", wsA)
	}
	if updated, _ := wsA["updatedAt"].(string); updated == wsA["createdAt"] {
		t.Fatalf("rename 后 updatedAt 必须变：%v", wsA)
	}
	msg = h.fail(t, "workspace.rename", map[string]any{"workspaceId": "ws-nope", "title": "x"}, http.StatusNotFound)
	if !strings.Contains(msg, "workspace/not-found") {
		t.Fatalf("未知 workspace 应报 workspace/not-found：%q", msg)
	}

	// pin / unpin：最新在前；未知会话 404 session/not-found；归档后不可 pin。
	realSession := "11111111-1111-1111-1111-111111111111"
	newRealSession(t, h.home, dirA, realSession)
	res = h.ok(t, "workspace.pinSession", map[string]any{"sessionId": realSession})
	if pinned := officialWorkspaceStrings(t, res, "pinnedSessionIds"); !equalStrings(pinned, []string{realSession}) {
		t.Fatalf("pinSession 集合不对：%v", pinned)
	}
	msg = h.fail(t, "workspace.pinSession", map[string]any{"sessionId": "00000000-0000-0000-0000-000000000000"}, http.StatusNotFound)
	if !strings.Contains(msg, "session/not-found") {
		t.Fatalf("未知会话应报 session/not-found：%q", msg)
	}

	// archive / unarchive：归档集最新在前；unarchive 幂等。
	res = h.ok(t, "workspace.archiveSession", map[string]any{"sessionId": realSession})
	if set := officialWorkspaceStrings(t, res, "archivedSessionIds"); !equalStrings(set, []string{realSession}) {
		t.Fatalf("archiveSession 集合不对：%v", set)
	}
	msg = h.fail(t, "workspace.pinSession", map[string]any{"sessionId": realSession}, http.StatusBadRequest)
	if !strings.Contains(msg, "gateway/bad-request") {
		t.Fatalf("已归档会话不可 pin，应报 gateway/bad-request：%q", msg)
	}
	res = h.ok(t, "workspace.unarchiveSession", map[string]any{"sessionId": realSession})
	if set := officialWorkspaceStrings(t, res, "archivedSessionIds"); len(set) != 0 {
		t.Fatalf("unarchiveSession 后集合应为空：%v", set)
	}
	res = h.ok(t, "workspace.unarchiveSession", map[string]any{"sessionId": realSession})
	if set := officialWorkspaceStrings(t, res, "archivedSessionIds"); len(set) != 0 {
		t.Fatalf("unarchive 必须幂等：%v", set)
	}
	res = h.ok(t, "workspace.unpinSession", map[string]any{"sessionId": realSession})
	if set := officialWorkspaceStrings(t, res, "pinnedSessionIds"); len(set) != 0 {
		t.Fatalf("unpin 后集合应为空：%v", set)
	}
	_ = h.ok(t, "workspace.unpinSession", map[string]any{"sessionId": realSession})

	// 进程重启（同一个 home 新 host）：顺序、标题、pin/archive 全部还在。
	h2 := newOfficialWorkspaceHarnessAt(t, h.home)
	res = h2.ok(t, "workspace.create", map[string]any{"path": dirA})
	if created, _ := res["created"].(bool); created {
		t.Fatalf("重启后 create 同一目录不应再 create：%v", res)
	}
	if got, _ := officialWorkspaceView(t, res)["workspaceId"].(string); got != idA {
		t.Fatalf("重启后 workspaceId 变了：%q → %q", idA, got)
	}
	if got, _ := officialWorkspaceView(t, res)["title"].(string); got != "改名后" {
		t.Fatalf("重启后 title 丢了：%q", got)
	}
	res = h2.ok(t, "workspace.insertBefore", map[string]any{"workspaceId": idB})
	if order := officialWorkspaceStrings(t, res, "workspaceIds"); !equalStrings(order, []string{idA, idB}) {
		t.Fatalf("重启后顺序丢了：%v", order)
	}

	// delete：只摘注册（目录与会话保留）。
	res = h2.ok(t, "workspace.delete", map[string]any{"workspaceId": idB})
	if deleted, _ := res["deleted"].(bool); !deleted {
		t.Fatalf("delete 结果应是 {deleted:true}：%v", res)
	}
	if _, err := os.Stat(dirB); err != nil {
		t.Fatalf("delete 不该删目录：%v", err)
	}
	if s, err := os.Stat(filepath.Join(h.home, "sessions")); err != nil || !s.IsDir() {
		t.Fatalf("delete 不该删会话：%v", err)
	}
	msg = h2.fail(t, "workspace.delete", map[string]any{"workspaceId": idB}, http.StatusNotFound)
	if !strings.Contains(msg, "workspace/not-found") {
		t.Fatalf("重复 delete 应报 workspace/not-found：%q", msg)
	}

	// 再重启一次：delete 也落盘了。
	h3 := newOfficialWorkspaceHarnessAt(t, h.home)
	res = h3.ok(t, "workspace.create", map[string]any{"path": dirB})
	if created, _ := res["created"].(bool); !created {
		t.Fatalf("delete 之后同目录应可重新 create：%v", res)
	}
}

// TestOfficialWorkspaceSessionsComeFromRealStore 会话归属必须来自真实 v4 会话存储，
// 手动排序落盘，归档的会话不参与分组，A/B 工作区不串。
func TestOfficialWorkspaceSessionsComeFromRealStore(t *testing.T) {
	h := newOfficialWorkspaceHarness(t)
	base := t.TempDir()
	dirA := newOfficialWorkspaceDir(t, base, "wsA")
	dirB := newOfficialWorkspaceDir(t, base, "wsB")

	// 真会话：A 下 3 条，B 下 1 条。
	idsA := []string{
		"aaaaaaaa-0000-0000-0000-000000000001",
		"aaaaaaaa-0000-0000-0000-000000000002",
		"aaaaaaaa-0000-0000-0000-000000000003",
	}
	for _, id := range idsA {
		newRealSession(t, h.home, dirA, id)
		time.Sleep(10 * time.Millisecond) // UpdatedAt 的先后决定默认顺序
	}
	idB := "bbbbbbbb-0000-0000-0000-000000000001"
	newRealSession(t, h.home, dirB, idB)

	res := h.ok(t, "workspace.create", map[string]any{"path": dirA})
	wsA := officialWorkspaceView(t, res)
	idWA, _ := wsA["workspaceId"].(string)
	sessions := officialWorkspaceStrings(t, wsA, "sessionIds")
	if len(sessions) != 3 {
		t.Fatalf("A 工作区应有 3 条真实会话，实际 %v", sessions)
	}
	// 默认顺序：最近更新的在前。
	if !equalStrings(sessions, []string{idsA[2], idsA[1], idsA[0]}) {
		t.Fatalf("默认会话顺序应按最近更新倒序：%v", sessions)
	}

	res = h.ok(t, "workspace.create", map[string]any{"path": dirB})
	if got := officialWorkspaceStrings(t, officialWorkspaceView(t, res), "sessionIds"); !equalStrings(got, []string{idB}) {
		t.Fatalf("B 工作区会话串了：%v", got)
	}

	// 手动排序：把最早那条移到最前。
	res = h.ok(t, "workspace.insertSessionBefore", map[string]any{
		"workspaceId": idWA, "sessionId": idsA[0], "beforeSessionId": idsA[2],
	})
	if got := officialWorkspaceStrings(t, officialWorkspaceView(t, res), "sessionIds"); !equalStrings(got, []string{idsA[0], idsA[2], idsA[1]}) {
		t.Fatalf("insertSessionBefore 顺序不对：%v", got)
	}
	// 省略 anchor = 移到末尾。
	res = h.ok(t, "workspace.insertSessionBefore", map[string]any{"workspaceId": idWA, "sessionId": idsA[0]})
	if got := officialWorkspaceStrings(t, officialWorkspaceView(t, res), "sessionIds"); !equalStrings(got, []string{idsA[2], idsA[1], idsA[0]}) {
		t.Fatalf("insertSessionBefore(省略 anchor) 顺序不对：%v", got)
	}
	// 不属于本工作区的会话 → 400 workspace/move-invalid（A/B 不串）。
	msg := h.fail(t, "workspace.insertSessionBefore", map[string]any{
		"workspaceId": idWA, "sessionId": idB,
	}, http.StatusBadRequest)
	if !strings.Contains(msg, "workspace/move-invalid") {
		t.Fatalf("跨工作区会话应报 workspace/move-invalid：%q", msg)
	}

	// 重启后手动顺序还在。
	h2 := newOfficialWorkspaceHarnessAt(t, h.home)
	res = h2.ok(t, "workspace.create", map[string]any{"path": dirA})
	if got := officialWorkspaceStrings(t, officialWorkspaceView(t, res), "sessionIds"); !equalStrings(got, []string{idsA[2], idsA[1], idsA[0]}) {
		t.Fatalf("重启后手动顺序丢了：%v", got)
	}

	// 归档：会话从分组里消失，归档集里有它。
	res = h2.ok(t, "workspace.archiveSession", map[string]any{"sessionId": idsA[1]})
	if got := officialWorkspaceStrings(t, res, "archivedSessionIds"); !equalStrings(got, []string{idsA[1]}) {
		t.Fatalf("归档集不对：%v", got)
	}
	res = h2.ok(t, "workspace.create", map[string]any{"path": dirA})
	if got := officialWorkspaceStrings(t, officialWorkspaceView(t, res), "sessionIds"); !equalStrings(got, []string{idsA[2], idsA[0]}) {
		t.Fatalf("归档的会话不该出现在分组里：%v", got)
	}
	// 未知会话归档 → 404 session/not-found。
	msg = h2.fail(t, "workspace.archiveSession", map[string]any{"sessionId": "ffffffff-0000-0000-0000-000000000000"}, http.StatusNotFound)
	if !strings.Contains(msg, "session/not-found") {
		t.Fatalf("未知会话归档应报 session/not-found：%q", msg)
	}
}

// TestOfficialWorkspaceArchiveActiveRequiresStopActivity 活动会话必须先停：
// 真轮次在跑时（active 回调为真），无 stopActivity 拒绝且集合不变；给了就真请求停止。
func TestOfficialWorkspaceArchiveActiveRequiresStopActivity(t *testing.T) {
	h := newOfficialWorkspaceHarness(t)
	dir := newOfficialWorkspaceDir(t, t.TempDir(), "active")
	id := "cccccccc-0000-0000-0000-000000000001"
	newRealSession(t, h.home, dir, id)

	// 把"这一条正在跑"注入宿主（生产上来自 promptService）。
	var stops []string
	h.host.active = func(sessionID string) bool { return sessionID == id }
	h.host.stop = func(sessionID string) error { stops = append(stops, sessionID); return nil }
	h.api = h.host.workspaceAPI()

	msg := h.fail(t, "workspace.archiveSession", map[string]any{"sessionId": id}, http.StatusConflict)
	if !strings.Contains(msg, "workspace/session-active") {
		t.Fatalf("活动会话应报 workspace/session-active：%q", msg)
	}
	if len(stops) != 0 {
		t.Fatalf("被拒绝时不得请求停止：%v", stops)
	}
	// 归档集必须没变。
	h.host.mu.Lock()
	archived := append([]string(nil), h.host.doc.ArchivedSessionIDs...)
	h.host.mu.Unlock()
	if len(archived) != 0 {
		t.Fatalf("被拒绝时归档集不得变化：%v", archived)
	}

	res := h.ok(t, "workspace.archiveSession", map[string]any{"sessionId": id, "stopActivity": true})
	if got := officialWorkspaceStrings(t, res, "archivedSessionIds"); !equalStrings(got, []string{id}) {
		t.Fatalf("带 stopActivity 应完成归档：%v", got)
	}
	if len(stops) != 1 || stops[0] != id {
		t.Fatalf("stopActivity=true 必须先请求停止：%v", stops)
	}
}

// TestOfficialWorkspaceFilesRealFSBoundary workspaceFiles 走真实文件系统且受工作区根约束。
func TestOfficialWorkspaceFilesRealFSBoundary(t *testing.T) {
	h := newOfficialWorkspaceHarness(t)
	root := newOfficialWorkspaceDir(t, t.TempDir(), "root")
	other := newOfficialWorkspaceDir(t, t.TempDir(), "other")
	id := "dddddddd-0000-0000-0000-000000000001"
	newRealSession(t, h.home, root, id)

	textPath := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(textPath, []byte("l1\nl2\nl3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(root, "blob.bin")
	if err := os.WriteFile(binPath, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(other, "secret.txt")
	if err := os.WriteFile(outsidePath, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	// list：根目录 = 空 path，child type 只取 file/directory/other。
	res := h.ok(t, "workspaceFiles.list", map[string]any{"workspaceFileScopeId": id, "path": ""})
	if res["path"] != "" {
		t.Fatalf("根目录的 path 应是空串：%v", res)
	}
	if truncated, _ := res["truncated"].(bool); truncated {
		t.Fatalf("小目录不该 truncated：%v", res)
	}
	entries, _ := res["entries"].([]any)
	names := make([]string, 0, len(entries))
	types := map[string]string{}
	sizes := map[string]any{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		names = append(names, name)
		types[name], _ = entry["type"].(string)
		if size, has := entry["size"]; has {
			sizes[name] = size
		}
	}
	if !equalStrings(names, []string{"blob.bin", "notes.txt", "sub"}) {
		t.Fatalf("list 应按名字排序且只列直接子项：%v", names)
	}
	if types["blob.bin"] != "file" || types["sub"] != "directory" {
		t.Fatalf("entry type 不对：%v", types)
	}
	if sizes["blob.bin"] != float64(6) {
		t.Fatalf("文件项应有真实 size=6：%v", sizes)
	}
	if _, has := sizes["sub"]; has {
		t.Fatalf("目录项不该有 size：%v", sizes)
	}

	// 子目录的 path 是工作区相对路径（用 '/' 连接）。
	res = h.ok(t, "workspaceFiles.list", map[string]any{"workspaceFileScopeId": id, "path": "sub"})
	if res["path"] != "sub" {
		t.Fatalf("子目录 path 应是工作区相对路径：%v", res)
	}

	// read：分页语义（offset 从 1 开始，末行 eof）。
	res = h.ok(t, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": id, "path": "notes.txt", "range": map[string]any{"offset": 1, "limit": 2},
	})
	if res["offset"] != float64(1) || res["text"] != "l1\nl2" || res["lines"] != float64(2) || res["eof"] != false {
		t.Fatalf("read 首页不对：%v", res)
	}
	// 官方 absolutePath 是"符号链接解析后"的绝对路径（Windows 上 8.3 短名也会被展开），
	// 所以期望值独立地用 EvalSymlinks 算一遍。
	if got, _ := res["absolutePath"].(string); filepath.Clean(got) != resolvedOrSelf(textPath) {
		t.Fatalf("absolutePath 应是符号链接解析后的真实绝对路径：%q（期望 %q）", got, resolvedOrSelf(textPath))
	}
	if version, _ := res["version"].(string); version == "" {
		t.Fatalf("read 必须给 version：%v", res)
	}
	if res["bytes"] != float64(len("l1\nl2\nl3\n")) {
		t.Fatalf("read 的 bytes 应是完整文件大小：%v", res)
	}
	res = h.ok(t, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": id, "path": "notes.txt", "range": map[string]any{"offset": 3, "limit": 5},
	})
	if res["text"] != "l3" || res["lines"] != float64(1) || res["eof"] != true {
		t.Fatalf("read 末页不对：%v", res)
	}
	// 越过末行的页：lines=0、text 空。
	res = h.ok(t, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": id, "path": "notes.txt", "range": map[string]any{"offset": 99},
	})
	if res["text"] != "" || res["lines"] != float64(0) {
		t.Fatalf("越过末行的页应是空页：%v", res)
	}
	// limit 超过上限 → 400（官方：may not exceed maxLines）。
	h.fail(t, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": id, "path": "notes.txt", "range": map[string]any{"limit": 100000},
	}, http.StatusBadRequest)
	// 目录不是文件 → 400。
	h.fail(t, "workspaceFiles.read", map[string]any{
		"workspaceFileScopeId": id, "path": "sub", "range": map[string]any{},
	}, http.StatusBadRequest)

	// stat：version 随内容变化。
	res = h.ok(t, "workspaceFiles.stat", map[string]any{"workspaceFileScopeId": id, "path": "notes.txt"})
	versionBefore, _ := res["version"].(string)
	if res["bytes"] != float64(len("l1\nl2\nl3\n")) {
		t.Fatalf("stat 的 bytes 不对：%v", res)
	}
	if err := os.WriteFile(textPath, []byte("l1\nl2\nl3\nl4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = h.ok(t, "workspaceFiles.stat", map[string]any{"workspaceFileScopeId": id, "path": "notes.txt"})
	if versionAfter, _ := res["version"].(string); versionAfter == versionBefore {
		t.Fatalf("内容变了 version 却没变：%q", versionAfter)
	}
	// 目录不能 stat 成文件。
	h.fail(t, "workspaceFiles.stat", map[string]any{"workspaceFileScopeId": id, "path": "sub"}, http.StatusBadRequest)

	// readBytes：真实字节的 base64 + 窗口 + eof。
	res = h.ok(t, "workspaceFiles.readBytes", map[string]any{
		"workspaceFileScopeId": id, "path": "blob.bin",
		"options": map[string]any{"range": map[string]any{"offset": 2, "length": 3}},
	})
	data, _ := res["data"].(string)
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("readBytes 的 data 不是合法 base64：%q（%v）", data, err)
	}
	if string(decoded) != "cde" {
		t.Fatalf("readBytes 窗口内容不对：%q", decoded)
	}
	if res["offset"] != float64(2) || res["eof"] != false || res["encoding"] != "base64" {
		t.Fatalf("readBytes 字段不对：%v", res)
	}
	res = h.ok(t, "workspaceFiles.readBytes", map[string]any{
		"workspaceFileScopeId": id, "path": "blob.bin",
		"options": map[string]any{"range": map[string]any{"offset": 3, "length": 10}},
	})
	data, _ = res["data"].(string)
	if decoded, err = base64.StdEncoding.DecodeString(data); err != nil || string(decoded) != "def" {
		t.Fatalf("readBytes 越界窗口应截到 EOF：%q（%v）", data, err)
	}
	if res["eof"] != true {
		t.Fatalf("读到末尾时 eof 应为 true：%v", res)
	}
	// 越过 EOF 的窗口：空 data、eof=true。
	res = h.ok(t, "workspaceFiles.readBytes", map[string]any{
		"workspaceFileScopeId": id, "path": "blob.bin",
		"options": map[string]any{"range": map[string]any{"offset": 99}},
	})
	if res["data"] != "" || res["eof"] != true {
		t.Fatalf("越过 EOF 应给空 data + eof=true：%v", res)
	}
	// baseFile：相对路径从 baseFile 所在目录解析。
	res = h.ok(t, "workspaceFiles.readBytes", map[string]any{
		"workspaceFileScopeId": id, "path": "blob.bin",
		"options": map[string]any{"baseFile": "notes.txt"},
	})
	if res["eof"] != true {
		t.Fatalf("baseFile 解析后应读到整个 blob.bin：%v", res)
	}

	// 边界：`..` 逃逸、绝对路径越界、另一个工作区的文件，全部 400 且不泄漏内容。
	for _, path := range []string{
		"../secret.txt",
		"../other/secret.txt",
		filepath.ToSlash(outsidePath),
		outsidePath,
	} {
		msg := h.fail(t, "workspaceFiles.read", map[string]any{
			"workspaceFileScopeId": id, "path": path, "range": map[string]any{},
		}, http.StatusBadRequest)
		if !strings.Contains(msg, "workspaceFiles/outside-workspace") {
			t.Fatalf("越界读取 %q 应报 workspaceFiles/outside-workspace：%q", path, msg)
		}
	}
	// stat / list / readBytes 同样受边界约束。
	h.fail(t, "workspaceFiles.stat", map[string]any{"workspaceFileScopeId": id, "path": "../secret.txt"}, http.StatusBadRequest)
	h.fail(t, "workspaceFiles.list", map[string]any{"workspaceFileScopeId": id, "path": ".."}, http.StatusBadRequest)
	h.fail(t, "workspaceFiles.readBytes", map[string]any{
		"workspaceFileScopeId": id, "path": "../secret.txt", "options": map[string]any{},
	}, http.StatusBadRequest)

	// 不存在的文件 → 404 workspaceFiles/not-found。
	msg := h.fail(t, "workspaceFiles.stat", map[string]any{"workspaceFileScopeId": id, "path": "missing.txt"}, http.StatusNotFound)
	if !strings.Contains(msg, "workspaceFiles/not-found") {
		t.Fatalf("不存在的文件应报 workspaceFiles/not-found：%q", msg)
	}
	// 未知会话（scope 不成立）→ 404 session/not-found，不拿进程默认目录顶上。
	msg = h.fail(t, "workspaceFiles.list", map[string]any{
		"workspaceFileScopeId": "eeeeeeee-0000-0000-0000-000000000000", "path": "",
	}, http.StatusNotFound)
	if !strings.Contains(msg, "session/not-found") {
		t.Fatalf("未知会话应报 session/not-found：%q", msg)
	}

	// 符号链接逃逸：能建就必测（Windows 无权限时跳过）。
	linkPath := filepath.Join(root, "escape-link")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Logf("跳过符号链接逃逸断言（本机不允许建符号链接）：%v", err)
	} else {
		msg = h.fail(t, "workspaceFiles.read", map[string]any{
			"workspaceFileScopeId": id, "path": "escape-link", "range": map[string]any{},
		}, http.StatusBadRequest)
		if !strings.Contains(msg, "workspaceFiles/") {
			t.Fatalf("符号链接逃逸应被拒：%q", msg)
		}
	}
}

// TestOfficialDirectoryPickerRealListAndCreate directoryPicker 走真实文件系统：
// 真 home、真 crumbs、按名字排序、hidden 标记、entry 上限与 truncation、真建目录。
func TestOfficialDirectoryPickerRealListAndCreate(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"a-dir", "b-dir", ".hidden-dir"} {
		if err := os.MkdirAll(filepath.Join(home, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		t.Fatal(err)
	}
	absHome = filepath.Clean(absHome)

	host := newOfficialWorkspaceHost(t.TempDir(), nil, nil)
	host.pickerHome = absHome // 生产上默认是 os.UserHomeDir()
	picker := host.directoryPickerAPI()

	call := func(t *testing.T, method string, body any) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/"+method, strings.NewReader(body.(string)))
		rec := httptest.NewRecorder()
		transport.ServeOfficialWorkspace(rec, req, method, host.workspaceAPI(), picker)
		var out map[string]any
		if rec.Body.Len() > 0 {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("%s 的响应不是 JSON：%q", method, rec.Body.String())
			}
		}
		return rec.Code, out
	}

	code, out := call(t, "directoryPicker.list", `{}`)
	if code != http.StatusOK {
		t.Fatalf("list 应 200，实际 %d（%v）", code, out)
	}
	res, _ := out["result"].(map[string]any)
	if got, _ := res["path"].(string); filepath.Clean(got) != absHome {
		t.Fatalf("省略 path 应列 home：%q（期望 %q）", got, absHome)
	}
	if got, _ := res["home"].(string); filepath.Clean(got) != absHome {
		t.Fatalf("home 字段不对：%q", got)
	}
	crumbs, _ := res["crumbs"].([]any)
	if len(crumbs) == 0 {
		t.Fatalf("crumbs 不能为空：%v", res)
	}
	last, _ := crumbs[len(crumbs)-1].(map[string]any)
	if got, _ := last["path"].(string); filepath.Clean(got) != absHome {
		t.Fatalf("最后一个 crumb 必须是所列目录：%v", last)
	}
	first, _ := crumbs[0].(map[string]any)
	rootPath, _ := first["path"].(string)
	if got, _ := first["name"].(string); got != rootPath {
		t.Fatalf("根 crumb 的 name 应是它的完整路径：%v", first)
	}
	if hidden, _ := first["hidden"].(bool); hidden {
		t.Fatalf("crumb 的 hidden 永远是 false：%v", first)
	}

	entries, _ := res["entries"].([]any)
	var names []string
	hiddenOf := map[string]bool{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		names = append(names, name)
		hiddenOf[name], _ = entry["hidden"].(bool)
	}
	if !equalStrings(names, []string{".hidden-dir", "a-dir", "b-dir"}) {
		t.Fatalf("entries 应只含目录且按名字排序：%v", names)
	}
	if !hiddenOf[".hidden-dir"] || hiddenOf["a-dir"] {
		t.Fatalf("hidden 标记不对：%v", hiddenOf)
	}
	if truncated, _ := res["truncated"].(bool); truncated {
		t.Fatalf("3 个目录不该 truncated：%v", res)
	}

	// entry 上限生效时 truncated=true，且缺的是名字排序的尾部。
	host.dirCap = 2
	code, out = call(t, "directoryPicker.list", `{}`)
	if code != http.StatusOK {
		t.Fatalf("list 应 200，实际 %d（%v）", code, out)
	}
	res, _ = out["result"].(map[string]any)
	entries, _ = res["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("上限 2 时应只给 2 项：%v", res)
	}
	if truncated, _ := res["truncated"].(bool); !truncated {
		t.Fatalf("被截断时 truncated 必须为 true：%v", res)
	}
	host.dirCap = 0

	// createDirectory：真建目录，返回真实绝对路径。
	code, out = call(t, "directoryPicker.createDirectory", `{"path":`+jsonQuote(absHome)+`,"name":"new-dir"}`)
	if code != http.StatusOK {
		t.Fatalf("createDirectory 应 200，实际 %d（%v）", code, out)
	}
	created, _ := out["result"].(string)
	if filepath.Clean(created) != filepath.Join(absHome, "new-dir") {
		t.Fatalf("createDirectory 返回路径不对：%q", created)
	}
	if st, err := os.Stat(created); err != nil || !st.IsDir() {
		t.Fatalf("createDirectory 没真的建出目录：%v", err)
	}
	// 已存在 → 400 directory-picker/exists。
	code, out = call(t, "directoryPicker.createDirectory", `{"path":`+jsonQuote(absHome)+`,"name":"new-dir"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("已存在应 400，实际 %d（%v）", code, out)
	}
	obj, _ := out["error"].(map[string]any)
	if msg, _ := obj["message"].(string); !strings.Contains(msg, "directory-picker/exists") {
		t.Fatalf("已存在应报 directory-picker/exists：%q", msg)
	}
	// 父目录不存在 → 400 directory-picker/create-failed 或 unreadable（不静默成功）。
	code, out = call(t, "directoryPicker.createDirectory", `{"path":`+jsonQuote(filepath.Join(absHome, "missing"))+`,"name":"x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("父目录不存在应 400，实际 %d（%v）", code, out)
	}
	// 相对 path → 400（官方要求绝对存在的父目录）。
	code, out = call(t, "directoryPicker.createDirectory", `{"path":"relative","name":"x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("相对 path 应 400，实际 %d（%v）", code, out)
	}
	// 列一个不存在的目录 → 400 directory-picker/unreadable。
	code, out = call(t, "directoryPicker.list", `{"path":`+jsonQuote(filepath.Join(absHome, "nope"))+`}`)
	if code != http.StatusBadRequest {
		t.Fatalf("列不存在的目录应 400，实际 %d（%v）", code, out)
	}
	obj, _ = out["error"].(map[string]any)
	if msg, _ := obj["message"].(string); !strings.Contains(msg, "directory-picker/unreadable") {
		t.Fatalf("不可读目录应报 directory-picker/unreadable：%q", msg)
	}
}

// resolvedOrSelf 是测试里独立算"符号链接解析后的绝对路径"（不复用被测实现）。
func resolvedOrSelf(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// jsonQuote 把一个路径编成 JSON 字符串字面量（测试里手拼 body 用）。
func jsonQuote(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// ---------------------------------------------------------------------------
// workspaceFiles.changes：目录目标
// ---------------------------------------------------------------------------

// nextChangesFrame 从一条 changes 流上等下一帧（超时即判失败）。
func nextChangesFrame(t *testing.T, stream transport.OfficialFrameStream) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, done, fault := stream.Next(ctx)
	if fault != nil {
		t.Fatalf("changes 流故障：%v（%s）", fault.Code, fault.Message)
	}
	if done {
		t.Fatalf("changes 流提前结束（等帧超时）")
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("changes 帧不是 JSON 对象：%q（%v）", string(raw), err)
	}
	return frame
}

// changesChangeOf 取一帧 change 的 change 对象，并钉住 kind。
func changesChangeOf(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	if frame["kind"] != "change" {
		t.Fatalf("帧不是 change：%v", frame)
	}
	change, ok := frame["change"].(map[string]any)
	if !ok {
		t.Fatalf("change 帧缺 change 对象：%v", frame)
	}
	return change
}

// TestOfficialWorkspaceChangesDirectoryTargetRealFS
//
// 目录目标必须真的产出 change 帧（官方帧：{absolutePath,version} /
// {absolutePath,absent:true}），而不是 501 workspace-file/watch-unsupported：
//
//   - absolutePath 是**符号链接解析后的绝对路径**，不是工作区相对路径；
//   - 目录直接子项增/删 → 目录版本变化 → 一帧带新版本的 change；
//   - 目录消失 → absent；目录回来 → 带新版本的 change；
//   - 文件目标仍走同一条流（尺寸/内容变化 → change），目录支持不改变文件语义。
func TestOfficialWorkspaceChangesDirectoryTargetRealFS(t *testing.T) {
	h := newOfficialWorkspaceHarness(t)
	root := newOfficialWorkspaceDir(t, t.TempDir(), "root")
	id := "dddddddd-0000-0000-0000-0000000000d1"
	newRealSession(t, h.home, root, id)

	watched := filepath.Join(root, "watched")
	if err := os.MkdirAll(watched, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(watched, "nested"), 0o755); err != nil {
		t.Fatalf("建子目录: %v", err)
	}

	journals := transport.NewJournals(nil)
	srv, err := transport.NewServer(transport.Options{Source: newSessionSource(h.home, ""), Journals: journals})
	if err != nil {
		t.Fatalf("transport.NewServer: %v", err)
	}
	defer srv.Close()
	srv.AttachOfficialWorkspaceAPI(h.api)
	defer srv.DetachOfficialWorkspaceAPI()

	open := func(path string) (transport.OfficialFrameStream, *transport.OfficialStreamFault) {
		return srv.OpenOfficialFrameStream(context.Background(), "workspaceFiles.changes",
			[]byte(`{"workspaceFileScopeId":`+jsonQuote(id)+`,"path":`+jsonQuote(path)+`}`))
	}

	stream, fault := open("watched")
	if fault != nil {
		t.Fatalf("目录目标必须能打开 changes 流，当前 fault=%d %s：%s",
			fault.Status, fault.Code, fault.Message)
	}
	defer stream.Close()

	head := nextChangesFrame(t, stream)
	if head["kind"] != "ready" {
		t.Fatalf("首帧必须是 ready（watch 已生效）：%v", head)
	}

	wantAbs := resolvedOrSelf(watched)
	if !filepath.IsAbs(wantAbs) {
		t.Fatalf("测试夹具的目录必须能算成绝对路径：%q", wantAbs)
	}
	// 目录的"绝对路径身份"：不是请求里那个相对路径。
	if wantAbs == "watched" || wantAbs == filepath.ToSlash("watched") {
		t.Fatalf("目录目标的绝对路径不能等于工作区相对路径：%q", wantAbs)
	}

	// 第 1 个直接子项 → 一帧 change（绝对路径 + 版本令牌）。
	if err := os.WriteFile(filepath.Join(watched, "a.txt"), []byte("1"), 0o644); err != nil {
		t.Fatalf("写子文件: %v", err)
	}
	first := changesChangeOf(t, nextChangesFrame(t, stream))
	if abs, _ := first["absolutePath"].(string); abs != wantAbs {
		t.Fatalf("目录帧的 absolutePath 必须是解析后的绝对路径 %q，实际 %q", wantAbs, abs)
	}
	if abs, _ := first["absolutePath"].(string); !filepath.IsAbs(abs) {
		t.Fatalf("目录帧的 absolutePath 不是绝对路径：%q（不拿相对路径冒充）", abs)
	}
	version1, _ := first["version"].(string)
	if version1 == "" {
		t.Fatalf("目录帧必须带非空 version：%v", first)
	}
	if _, has := first["absent"]; has {
		t.Fatalf("目录存在时不许带 absent：%v", first)
	}

	// 第 2 个直接子项 → 目录版本必须再变。
	if err := os.WriteFile(filepath.Join(watched, "b.txt"), []byte("2"), 0o644); err != nil {
		t.Fatalf("写第二个子文件: %v", err)
	}
	second := changesChangeOf(t, nextChangesFrame(t, stream))
	version2, _ := second["version"].(string)
	if version2 == "" || version2 == version1 {
		t.Fatalf("目录直接子项增加后 version 必须变化：%q → %q", version1, version2)
	}

	// 删掉一个直接子项 → 目录版本再变。
	if err := os.Remove(filepath.Join(watched, "a.txt")); err != nil {
		t.Fatalf("删子文件: %v", err)
	}
	third := changesChangeOf(t, nextChangesFrame(t, stream))
	version3, _ := third["version"].(string)
	if version3 == "" || version3 == version2 {
		t.Fatalf("目录直接子项删除后 version 必须变化：%q → %q", version2, version3)
	}

	// 目录消失 → absent 帧，路径身份不变。
	if err := os.RemoveAll(watched); err != nil {
		t.Fatalf("删目录: %v", err)
	}
	gone := changesChangeOf(t, nextChangesFrame(t, stream))
	if gone["absent"] != true {
		t.Fatalf("目录消失必须发 absent 帧：%v", gone)
	}
	if abs, _ := gone["absolutePath"].(string); abs != wantAbs {
		t.Fatalf("absent 帧仍要带原来的绝对路径 %q，实际 %q", wantAbs, abs)
	}
	if _, has := gone["version"]; has {
		t.Fatalf("absent 帧不许带 version：%v", gone)
	}

	// 目录回来 → 带新版本的 change（不是再发一次 absent）。
	if err := os.MkdirAll(watched, 0o755); err != nil {
		t.Fatalf("重建目录: %v", err)
	}
	back := changesChangeOf(t, nextChangesFrame(t, stream))
	version4, _ := back["version"].(string)
	if version4 == "" {
		t.Fatalf("目录回来后必须发带 version 的 change：%v", back)
	}
	if _, has := back["absent"]; has {
		t.Fatalf("目录回来后的 change 不许带 absent：%v", back)
	}

	// 文件目标回归：目录支持不改变"文件 watch"的语义。
	fileTarget := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(fileTarget, []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件目标: %v", err)
	}
	fileStream, fault := open("plain.txt")
	if fault != nil {
		t.Fatalf("文件目标必须仍能打开 changes 流：%v", fault)
	}
	defer fileStream.Close()
	if head := nextChangesFrame(t, fileStream); head["kind"] != "ready" {
		t.Fatalf("文件目标首帧必须是 ready：%v", head)
	}
	if err := os.WriteFile(fileTarget, []byte("xy"), 0o644); err != nil {
		t.Fatalf("改文件目标: %v", err)
	}
	fileChange := changesChangeOf(t, nextChangesFrame(t, fileStream))
	if abs, _ := fileChange["absolutePath"].(string); abs != resolvedOrSelf(fileTarget) {
		t.Fatalf("文件帧的 absolutePath 不对：%v", fileChange)
	}

	// 空 path = 工作区根，也是目录目标：changes 能 watch 根，而 unary stat
	// 仍按官方契约拒绝（根是目录，不是普通文件）。
	h.fail(t, "workspaceFiles.stat", map[string]any{"workspaceFileScopeId": id, "path": ""}, http.StatusBadRequest)
	rootStream, fault := open("")
	if fault != nil {
		t.Fatalf("工作区根（空 path）必须能打开 changes 流：%v", fault)
	}
	defer rootStream.Close()
	if head := nextChangesFrame(t, rootStream); head["kind"] != "ready" {
		t.Fatalf("根目录首帧必须是 ready：%v", head)
	}
	if err := os.WriteFile(filepath.Join(root, "root-new.txt"), []byte("r"), 0o644); err != nil {
		t.Fatalf("在根下建文件: %v", err)
	}
	rootChange := changesChangeOf(t, nextChangesFrame(t, rootStream))
	if abs, _ := rootChange["absolutePath"].(string); abs != resolvedOrSelf(root) {
		t.Fatalf("根目录帧的 absolutePath 必须是工作区根的绝对路径：%v", rootChange)
	}
	if version, _ := rootChange["version"].(string); version == "" {
		t.Fatalf("根目录帧必须带 version：%v", rootChange)
	}
}

// TestOfficialWorkspaceChangesDirectoryBoundary
//
// 目录目标不能成为越界/符号链接的口子：
//   - `..`、工作区外的绝对目录 → 400 workspaceFiles/outside-workspace；
//   - 指向工作区外的目录符号链接 → 400 outside-workspace；
//   - 指向工作区内的目录符号链接 → 400 symlink-refused（watch 不借链接改路径身份）；
//   - 拒绝都发生在开流之前（没有 200 + 假 ready）。
func TestOfficialWorkspaceChangesDirectoryBoundary(t *testing.T) {
	h := newOfficialWorkspaceHarness(t)
	root := newOfficialWorkspaceDir(t, t.TempDir(), "root")
	other := newOfficialWorkspaceDir(t, t.TempDir(), "other")
	id := "dddddddd-0000-0000-0000-0000000000d2"
	newRealSession(t, h.home, root, id)
	if err := os.MkdirAll(filepath.Join(other, "sub"), 0o755); err != nil {
		t.Fatalf("建工作区外目录: %v", err)
	}
	inside := filepath.Join(root, "inside")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("建工作区内目录: %v", err)
	}

	journals := transport.NewJournals(nil)
	srv, err := transport.NewServer(transport.Options{Source: newSessionSource(h.home, ""), Journals: journals})
	if err != nil {
		t.Fatalf("transport.NewServer: %v", err)
	}
	defer srv.Close()
	srv.AttachOfficialWorkspaceAPI(h.api)
	defer srv.DetachOfficialWorkspaceAPI()

	open := func(path string) (transport.OfficialFrameStream, *transport.OfficialStreamFault) {
		return srv.OpenOfficialFrameStream(context.Background(), "workspaceFiles.changes",
			[]byte(`{"workspaceFileScopeId":`+jsonQuote(id)+`,"path":`+jsonQuote(path)+`}`))
	}
	mustRefused := func(path, wantCode string) {
		t.Helper()
		stream, fault := open(path)
		if fault == nil {
			stream.Close()
			t.Fatalf("越界/链接目录 %q 必须被拒，实际打开了流", path)
		}
		if fault.Status != http.StatusBadRequest {
			t.Fatalf("%q 应 400，实际 %d（%s）", path, fault.Status, fault.Code)
		}
		if !strings.Contains(fault.Message, wantCode) {
			t.Fatalf("%q 应报 %s，实际：%s", path, wantCode, fault.Message)
		}
	}

	mustRefused("..", "workspaceFiles/outside-workspace")
	mustRefused(filepath.ToSlash(other), "workspaceFiles/outside-workspace")
	mustRefused(filepath.ToSlash(filepath.Join(other, "sub")), "workspaceFiles/outside-workspace")

	// 符号链接：能建就必测（Windows 无权限时跳过，与既有边界测试同口径）。
	if err := os.Symlink(other, filepath.Join(root, "link-out")); err != nil {
		t.Logf("跳过目录符号链接断言（本机不允许建符号链接）：%v", err)
	} else {
		mustRefused("link-out", "workspaceFiles/outside-workspace")
		if err := os.Symlink(inside, filepath.Join(root, "link-in")); err != nil {
			t.Logf("跳过工作区内目录符号链接断言：%v", err)
		} else {
			mustRefused("link-in", "workspaceFiles/symlink-refused")
		}
	}
}
