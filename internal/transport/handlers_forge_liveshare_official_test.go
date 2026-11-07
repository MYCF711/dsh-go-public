package transport

// 官方 0.2.2 桌面端 forgePanel.*（19）与 liveshareShares.*（8）共 27 个方法的契约测试。
//
// 这些测试只打本包的 `serveOfficialForgeLiveshareAPI`（Lead 接线后由 /api 路由调它），
// 因此不依赖 server.go 的分发表——**路由接线是 Lead 的验收项**；测试里用
// IsOfficialForgeLiveshareMethod 钉住"方法名集合"，避免新增方法从路由漏下去。
//
// 另外有一条 HTTP 级用例（走 s.ServeHTTP）证明这 27 个名字**当前就能调**：
// 未接线时它们落到 handleAPI 的 default（501 通用未实现），接线后落在本文件
// （501 逐方法 + 参数 400）。两种都不是 404，也都不会 panic。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// forgeOfficialTestServer 造一个不挂任何宿主实现的最小 Server。
func forgeOfficialTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// callForgeOfficial 打一次方法，返回 HTTP 状态与解码后的信封。
func callForgeOfficial(t *testing.T, srv *Server, method string, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.serveOfficialForgeLiveshareAPI(rec, req, method)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
	}
	return rec.Code, out
}

// forgeGoodBodies 是 27 个方法**按官方 wire 名**写的最小合法请求体。
//
// 故意不按本文件的参数表生成：那样"表写错了"和"测试写错了"会一起错，
// 测试就只剩自我一致。这里的键名逐条抄自官方 typert 的 parameters[].wire。
func forgeGoodBodies() map[Method]string {
	return map[Method]string{
		MethodForgePanelBranches:            `{"sessionId":"s-1"}`,
		MethodForgePanelCancelWorktree:      `{"sessionId":"s-1"}`,
		MethodForgePanelChangeRequestChecks: `{"sessionId":"s-1","url":"https://github.com/o/r/pull/1","headSha":"9f2c"}`,
		MethodForgePanelChangeRequestDiff:   `{"sessionId":"s-1","url":"https://github.com/o/r/pull/1"}`,
		MethodForgePanelChangeRequestText:   `{"sessionId":"s-1","url":"https://github.com/o/r/pull/1","commit":"9f2c","path":"src/a.go"}`,
		MethodForgePanelContinueInProject:   `{"sessionId":"s-1"}`,
		MethodForgePanelDismiss:             `{"sessionId":"s-1","url":"https://github.com/o/r/pull/1"}`,
		MethodForgePanelFollow:              `{"sessionId":"s-1"}`,
		MethodForgePanelLocalDiff:           `{"sessionId":"s-1","scope":{"kind":"branch"}}`,
		MethodForgePanelLocalText:           `{"sessionId":"s-1","scope":{"kind":"staged"},"path":"src/a.go"}`,
		MethodForgePanelMarkReady:           `{"sessionId":"s-1","url":"https://github.com/o/r/pull/1"}`,
		MethodForgePanelMerge:               `{"sessionId":"s-1","url":"https://github.com/o/r/pull/1","method":"squash","headSha":"9f2c"}`,
		MethodForgePanelRefresh:             `{"sessionId":"s-1"}`,
		MethodForgePanelRequestWorktree:     `{"sessionId":"s-1","from":null}`,
		MethodForgePanelRestoreWorktree:     `{"sessionId":"s-1"}`,
		MethodForgePanelRetryWorktree:       `{"sessionId":"s-1"}`,
		MethodForgePanelStopCreation:        `{"sessionId":"s-1"}`,
		MethodForgePanelSubmitReview:        `{"sessionId":"s-1","url":"https://github.com/o/r/pull/1","request":{"headSha":"9f2c","kind":"comment","comment":{"path":"src/a.go","side":"R","startLine":3,"endLine":3,"body":"nit"}}}`,
		MethodForgePanelTrack:               `{"sessionId":"s-1","url":"https://github.com/o/r/pull/1"}`,

		MethodLiveshareSharesAudit:             `{"request":{"shareId":"sh-1"}}`,
		MethodLiveshareSharesCreate:            `{"request":{"sessionId":"s-1"}}`,
		MethodLiveshareSharesIdentity:          `{"request":{}}`,
		MethodLiveshareSharesSetCapabilities:   `{"request":{"shareId":"sh-1","capabilities":[]}}`,
		MethodLiveshareSharesSetRelay:          `{"request":{"relayUrl":null}}`,
		MethodLiveshareSharesStop:              `{"request":{"shareId":"sh-1"}}`,
		MethodLiveshareSharesTestPublish:       `{"request":{"sessionId":"s-1"}}`,
		MethodLiveshareSharesUpdatePermissions: `{"request":{"shareId":"sh-1","sessionMode":"view"}}`,
	}
}

// TestOfficialForgeLiveshareMethodsAreStable 钉住 27 个方法名本身。
func TestOfficialForgeLiveshareMethodsAreStable(t *testing.T) {
	methods := OfficialForgeLiveshareMethods()
	if len(methods) != 27 {
		t.Fatalf("官方 forgePanel/liveshareShares 方法应是 27 个，实际 %d", len(methods))
	}
	seen := map[Method]bool{}
	for _, m := range methods {
		if seen[m] {
			t.Fatalf("方法重复：%s", m)
		}
		seen[m] = true
		if !IsOfficialForgeLiveshareMethod(string(m)) {
			t.Fatalf("%s 没被 IsOfficialForgeLiveshareMethod 认出来（路由会漏）", m)
		}
		if strings.Contains(string(m), "/") {
			t.Fatalf("%s 含 '/'：本仓方法名用点号", m)
		}
	}

	// 官方命名空间逐个点名（顺序与 docs/evidence/…-0.2.2-rc.1.txt 一致），
	// 防止"少写一个也没人发现"。
	want := []string{
		"forgePanel.branches", "forgePanel.cancelWorktree", "forgePanel.changeRequestChecks",
		"forgePanel.changeRequestDiff", "forgePanel.changeRequestText", "forgePanel.continueInProject",
		"forgePanel.dismiss", "forgePanel.follow", "forgePanel.localDiff", "forgePanel.localText",
		"forgePanel.markReady", "forgePanel.merge", "forgePanel.refresh",
		"forgePanel.requestWorktree", "forgePanel.restoreWorktree", "forgePanel.retryWorktree",
		"forgePanel.stopCreation", "forgePanel.submitReview", "forgePanel.track",
		"liveshareShares.audit", "liveshareShares.create", "liveshareShares.identity",
		"liveshareShares.setCapabilities", "liveshareShares.setRelay", "liveshareShares.stop",
		"liveshareShares.testPublish", "liveshareShares.updatePermissions",
	}
	got := make([]string, 0, len(methods))
	for _, m := range methods {
		got = append(got, string(m))
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, "|") != strings.Join(sorted, "|") {
		t.Fatalf("方法集合不一致：\n got=%v\nwant=%v", got, sorted)
	}

	// 不在本组口径里的官方名字必须**不被**本族认领：它们各自有别的归属，
	// 认领了会把一条本该回"未实现"的流式方法伪装成本族的一条。
	for _, out := range []string{
		"forgePanel.changeRequestStates", // 官方 stream，本组 19 条不含
		"liveshareShares.list",           // 官方 list，本组 8 条不含
		"liveshareShares.relay",
		"liveshareShares.capabilities",
		"liveshareShares.testRelay",
		"forgePanel.unknown",
		"",
	} {
		if IsOfficialForgeLiveshareMethod(out) {
			t.Fatalf("%q 不属于本文件的 27 个方法，却被认领了", out)
		}
	}
}

// TestOfficialForgeLiveshareAllReturnNotImplemented 是这 27 个方法的主判据：
// 合法参数 → 501 + not_implemented + 四字段 + 说清缺什么；没有 result。
func TestOfficialForgeLiveshareAllReturnNotImplemented(t *testing.T) {
	bodies := forgeGoodBodies()
	if len(bodies) != 27 {
		t.Fatalf("测试数据应覆盖 27 个方法，实际 %d", len(bodies))
	}
	srv := forgeOfficialTestServer(t)
	for _, method := range OfficialForgeLiveshareMethods() {
		body, ok := bodies[method]
		if !ok {
			t.Fatalf("%s 没有合法的测试请求体", method)
		}
		status, out := callForgeOfficial(t, srv, string(method), body)
		if status != http.StatusNotImplemented {
			t.Fatalf("%s 状态码 %d，期望 501（body=%v）", method, status, out)
		}
		assertFourFieldError(t, status, out, http.StatusNotImplemented, "not_implemented")
		if _, has := out["result"]; has {
			t.Fatalf("%s 回了 result：未实现不许带成功载荷（%v）", method, out)
		}
		obj := out["error"].(map[string]any)
		msg, _ := obj["message"].(string)
		if !strings.Contains(msg, string(method)) {
			t.Fatalf("%s 的错误正文没点名方法：%q", method, msg)
		}
		// 正文必须说清"需要什么"，否则界面只能显示"未实现"，用户不知道等谁。
		if !strings.Contains(msg, "需要") {
			t.Fatalf("%s 的错误正文没说清缺什么：%q", method, msg)
		}
		if retryable, ok := obj["retryable"].(bool); !ok || retryable {
			t.Fatalf("%s 的 retryable 应为 false（缺服务不是重试能解决的）：%v", method, obj["retryable"])
		}
	}
}

// TestOfficialForgeLiveshareParamsFailLoud 钉住参数错误：必须是 400 bad_request。
//
// 这是"先校验参数再回 501"的意义：GUI 拼错参数时得到的应该是一条能改的错误，
// 而不是一个看起来"等宿主实现就好"的未实现。
func TestOfficialForgeLiveshareParamsFailLoud(t *testing.T) {
	srv := forgeOfficialTestServer(t)
	cases := []struct {
		name   string
		method Method
		body   string
		want   string // 错误正文里必须出现的片段
	}{
		{"缺 sessionId", MethodForgePanelBranches, `{}`, "缺 sessionId"},
		{"sessionId 空串", MethodForgePanelBranches, `{"sessionId":""}`, "空串"},
		{"sessionId 类型错", MethodForgePanelBranches, `{"sessionId":7}`, "必须是字符串"},
		{"未知顶层字段", MethodForgePanelBranches, `{"sessionId":"s-1","branch":"main"}`, "不认识的字段"},
		{"请求体不是对象", MethodForgePanelBranches, `[]`, "JSON 对象"},
		{"请求体为空", MethodForgePanelBranches, ``, "请求体为空"},
		{"changeRequestChecks 缺 url", MethodForgePanelChangeRequestChecks, `{"sessionId":"s-1","headSha":"9f2c"}`, "缺 url"},
		{"changeRequestText 缺 path", MethodForgePanelChangeRequestText, `{"sessionId":"s-1","url":"u","commit":"c"}`, "缺 path"},
		{"localDiff 缺 scope", MethodForgePanelLocalDiff, `{"sessionId":"s-1"}`, "缺 scope"},
		{"localDiff scope 非对象", MethodForgePanelLocalDiff, `{"sessionId":"s-1","scope":"branch"}`, "必须是 JSON 对象"},
		{"localDiff scope 无 kind", MethodForgePanelLocalDiff, `{"sessionId":"s-1","scope":{}}`, "scope 缺 kind"},
		{"localDiff scope kind 非法", MethodForgePanelLocalDiff, `{"sessionId":"s-1","scope":{"kind":"worktree"}}`, "scope.kind 只能是"},
		{"localText 缺 path", MethodForgePanelLocalText, `{"sessionId":"s-1","scope":{"kind":"staged"}}`, "缺 path"},
		{"merge method 非法", MethodForgePanelMerge, `{"sessionId":"s-1","url":"u","method":"rebase-merge","headSha":"c"}`, "method 只能是"},
		{"merge 缺 headSha", MethodForgePanelMerge, `{"sessionId":"s-1","url":"u","method":"merge"}`, "缺 headSha"},
		{"requestWorktree from 类型错", MethodForgePanelRequestWorktree, `{"sessionId":"s-1","from":3}`, "字符串或 null"},
		{"submitReview 缺 request", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u"}`, "缺 request"},
		{"submitReview 缺 headSha", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u","request":{"kind":"comment"}}`, "缺 headSha"},
		{"submitReview kind 非法", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u","request":{"headSha":"c","kind":"reaction"}}`, "kind 只能是"},
		{"submitReview comment 缺 side", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u","request":{"headSha":"c","kind":"comment","comment":{"path":"a","body":"b","startLine":1,"endLine":1}}}`, "缺 side"},
		{"submitReview side 非法", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u","request":{"headSha":"c","kind":"comment","comment":{"path":"a","body":"b","side":"M","startLine":1,"endLine":1}}}`, "side 只能是"},
		{"submitReview review 缺 event", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u","request":{"headSha":"c","kind":"review","body":"","comments":[]}}`, "缺 event"},
		{"submitReview review event 非法", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u","request":{"headSha":"c","kind":"review","event":"lgtm","body":"","comments":[]}}`, "event 只能是"},
		{"submitReview review 缺 comments", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u","request":{"headSha":"c","kind":"review","event":"approve","body":""}}`, "缺 comments"},
		{"liveshare 缺 request", MethodLiveshareSharesStop, `{}`, "缺 request"},
		{"liveshare request 为 null", MethodLiveshareSharesStop, `{"request":null}`, "不能是 null"},
		{"liveshare request 缺 shareId", MethodLiveshareSharesStop, `{"request":{}}`, "缺 shareId"},
		{"liveshare shareId 类型错", MethodLiveshareSharesAudit, `{"request":{"shareId":12}}`, "必须是字符串"},
		{"liveshare create 缺 sessionId", MethodLiveshareSharesCreate, `{"request":{}}`, "缺 sessionId"},
		{"liveshare setRelay 缺 relayUrl", MethodLiveshareSharesSetRelay, `{"request":{}}`, "缺 relayUrl"},
		{"liveshare setRelay 类型错", MethodLiveshareSharesSetRelay, `{"request":{"relayUrl":7}}`, "字符串或 null"},
		{"liveshare 未知顶层字段", MethodLiveshareSharesStop, `{"request":{"shareId":"sh-1"},"force":true}`, "不认识的字段"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := callForgeOfficial(t, srv, string(tc.method), tc.body)
			assertFourFieldError(t, status, out, http.StatusBadRequest, "bad_request")
			obj := out["error"].(map[string]any)
			msg, _ := obj["message"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("错误正文 %q 没包含 %q", msg, tc.want)
			}
			if _, has := out["result"]; has {
				t.Fatalf("参数错不许带 result：%v", out)
			}
		})
	}
}

// TestOfficialForgeLiveshareAcceptableVariants 钉住"官方允许的那几种写法也要收"：
// null 的 from / relayUrl、review 分支、capabilities 空数组，都不能被误判成 400。
func TestOfficialForgeLiveshareAcceptableVariants(t *testing.T) {
	srv := forgeOfficialTestServer(t)
	cases := []struct {
		name   string
		method Method
		body   string
	}{
		{"requestWorktree from=字符串", MethodForgePanelRequestWorktree, `{"sessionId":"s-1","from":"main"}`},
		{"setRelay relayUrl=字符串", MethodLiveshareSharesSetRelay, `{"request":{"relayUrl":"https://relay.example"}}`},
		{"submitReview review 分支", MethodForgePanelSubmitReview, `{"sessionId":"s-1","url":"u","request":{"headSha":"c","kind":"review","event":"approve","body":"","comments":[]}}`},
		{"identity 空 request", MethodLiveshareSharesIdentity, `{"request":{}}`},
		{"identity 带 relay", MethodLiveshareSharesIdentity, `{"request":{"relay":"https://relay.example"}}`},
		{"updatePermissions 带 ttl", MethodLiveshareSharesUpdatePermissions, `{"request":{"shareId":"sh-1","ttlMinutes":null,"allowsFork":false}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := callForgeOfficial(t, srv, string(tc.method), tc.body)
			if status != http.StatusNotImplemented {
				t.Fatalf("合法变体应回 501，实际 %d（body=%v）", status, out)
			}
			assertFourFieldError(t, status, out, http.StatusNotImplemented, "not_implemented")
		})
	}
}

// TestOfficialForgeLiveshareCallableThroughHTTP 证明这 27 个名字**走真实路由也能调**。
//
// 未接线时它们落到 handleAPI 的 default（501 通用未实现），接线后落本文件
// （501 逐方法）。两条路都不是 404、都不是 panic，所以这条用例在接线前后都成立。
func TestOfficialForgeLiveshareCallableThroughHTTP(t *testing.T) {
	srv := forgeOfficialTestServer(t)
	cookie := srv.MintInternalCookie()
	if cookie == "" {
		t.Fatalf("MintInternalCookie 返回空值")
	}
	bodies := forgeGoodBodies()
	for _, method := range OfficialForgeLiveshareMethods() {
		req := httptest.NewRequest(http.MethodPost, "/api/"+string(method),
			strings.NewReader(bodies[method]))
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s 回了 404：这些方法必须返回确定状态", method)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
		obj, ok := out["error"].(map[string]any)
		if !ok {
			t.Fatalf("%s 没有回错误信封（不许静默成功）：%v", method, out)
		}
		if code := obj["code"]; code != "not_implemented" {
			t.Fatalf("%s 的错误码 %v，期望 not_implemented（body=%v）", method, code, out)
		}
		for _, key := range []string{"code", "message", "hint", "retryable"} {
			if _, has := obj[key]; !has {
				t.Fatalf("%s 的错误信封缺 %s：%v", method, key, obj)
			}
		}
	}
}

// TestOfficialForgeLiveshareHomeIsolation 钉住 A/B home 隔离：
// 这 27 个方法不读盘、不落盘，两个不同 home 的响应必须逐字节一致。
//
// 它们将来接上真服务时，这条用例就是"别把 A home 的 share/绑定串到 B home"的基线。
func TestOfficialForgeLiveshareHomeIsolation(t *testing.T) {
	run := func(home string) map[string]string {
		t.Helper()
		t.Setenv("DSH_HOME", home)
		srv := forgeOfficialTestServer(t)
		bodies := forgeGoodBodies()
		got := make(map[string]string, len(bodies))
		for _, method := range OfficialForgeLiveshareMethods() {
			status, out := callForgeOfficial(t, srv, string(method), bodies[method])
			if status != http.StatusNotImplemented {
				t.Fatalf("%s 状态码 %d，期望 501", method, status)
			}
			raw, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("%s 序列化响应失败: %v", method, err)
			}
			got[string(method)] = string(raw)
		}
		return got
	}
	dirA := t.TempDir()
	dirB := t.TempDir()
	a := run(dirA)
	b := run(dirB)
	for method, bodyA := range a {
		if bodyA != b[method] {
			t.Fatalf("%s 在 A/B 两个 home 下响应不同：\nA=%s\nB=%s", method, bodyA, b[method])
		}
	}
	// 隔离的另一半：不许偷偷建目录/写状态文件。
	for _, home := range []string{dirA, dirB} {
		entries, err := os.ReadDir(home)
		if err != nil {
			t.Fatalf("读 home %s 失败: %v", home, err)
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, filepath.Join(home, e.Name()))
			}
			t.Fatalf("home %s 被写入了内容（未实现的方法不许落盘）：%v", home, names)
		}
	}
}

// TestOfficialForgeLiveshareDeterministic 钉住"确定状态"：同一请求两次必须逐字节一致。
func TestOfficialForgeLiveshareDeterministic(t *testing.T) {
	srv := forgeOfficialTestServer(t)
	body := `{"sessionId":"s-1","scope":{"kind":"unstaged"},"path":"src/a.go"}`
	status1, out1 := callForgeOfficial(t, srv, string(MethodForgePanelLocalText), body)
	raw1, _ := json.Marshal(out1)
	status2, out2 := callForgeOfficial(t, srv, string(MethodForgePanelLocalText), body)
	raw2, _ := json.Marshal(out2)
	if status1 != status2 || !bytes.Equal(raw1, raw2) {
		t.Fatalf("同一请求两次结果不同：%d/%d\n%s\n%s", status1, status2, raw1, raw2)
	}
}

// TestOfficialForgeLiveshareNotFamilyNameRejected 钉住接线判断与本文件一致：
// 不属于这一族的方法名必须被明确拒绝（501 + 点名），而不是落成 404。
func TestOfficialForgeLiveshareNotFamilyNameRejected(t *testing.T) {
	srv := forgeOfficialTestServer(t)
	status, out := callForgeOfficial(t, srv, "forgePanel.changeRequestStates", `[]`)
	assertFourFieldError(t, status, out, http.StatusNotImplemented, "not_implemented")
	obj := out["error"].(map[string]any)
	msg, _ := obj["message"].(string)
	if !strings.Contains(msg, "不是官方 forgePanel/liveshareShares 族") {
		t.Fatalf("错误正文没说明这一族不认领：%q", msg)
	}
}
