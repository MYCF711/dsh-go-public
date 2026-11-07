package main

// 官方 `subagents.*` 的**宿主侧真实语义**（L1 HOST-SUBAGENT-CONTINUABLE）。
//
// 形状与参数校验在 `internal/transport/handlers_subagents_official_test.go`，
// 这里测的是四件必须真的事：
//
//	1. 没 spawn 过的 child → 400 `no_such_session`，**绝不**回 messageId；
//	2. 别的进程 spawn 的 child → 认领后真的投得进去（200 + messageId），
//	   且回执帧真的落进子会话 v4 日志；
//	3. 父 id 不匹配 → 403 `cross_session`（不许变成"没这个会话"）；
//	4. 投递失败 → 不回 messageId；同一 requestId 重投 → 同一个 messageId 且只投一次。
//
// 全程真 HTTP（真 transport.Server + 一次性 token 换 cookie）+ 真 v4 会话，
// 不 mock 协议层：状态码与 code 就是从线上读回来的那几个字节。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/agent"
	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// subagentE2E 是一套"真 home + 真 transport.Server + 真官方 API 对象"的宿主。
type subagentE2E struct {
	t      *testing.T
	home   string
	base   string
	client *http.Client
	core   *agentOfficialAPI
	close  func() error

	// 投递通道的观测点（真实宿主里是 promptService；这里只记"有没有真投"）。
	delivered  []string
	placement  []string
	deliverErr error
	interrupts []string
}

// newSubagentE2E 起一套真宿主：官方 API 挂在同一个 Server 上，走同一条 cookie 门。
func newSubagentE2E(t *testing.T, tune func(*agentOfficialDeps)) *subagentE2E {
	t.Helper()
	return newSubagentE2EAt(t, t.TempDir(), tune)
}

// newSubagentE2EAt 在**给定的 home** 上再起一套宿主。同 home 起两套 = 模拟
// "两个进程看同一份 v4 日志"：认领必须从盘上认，而不是从前一个进程的内存里认。
func newSubagentE2EAt(t *testing.T, home string, tune func(*agentOfficialDeps)) *subagentE2E {
	t.Helper()
	e := &subagentE2E{t: t, home: home}
	deps := agentOfficialDeps{
		home: home,
		src:  newSessionSource(home, ""),
		submit: func(sessionID, text string) (transport.PromptResult, error) {
			if e.deliverErr != nil {
				return transport.PromptResult{}, e.deliverErr
			}
			e.delivered = append(e.delivered, sessionID+"|"+text)
			e.placement = append(e.placement, string(transport.PlacementFollowup))
			return transport.PromptResult{Accepted: true, TurnID: "turn-1"}, nil
		},
		interrupt: func(sessionID string) error {
			e.interrupts = append(e.interrupts, sessionID)
			return nil
		},
	}
	if tune != nil {
		tune(&deps)
	}
	e.core = newAgentOfficialAPICore(deps)

	opts := transport.Options{
		Source: deps.src,
		List:   func() ([]transport.SessionSummary, error) { return listSessions(home, "") },
		Load: func(id string, limit int) (transport.LoadResult, error) {
			return transport.LoadSession(deps.src, id, limit)
		},
	}
	srv, err := transport.NewServer(opts)
	if err != nil {
		t.Fatalf("建 transport server: %v", err)
	}
	srv.AttachOfficialAgentAPI(e.core.bind())
	base, err := srv.Start()
	if err != nil {
		_ = srv.Close()
		t.Fatalf("启动 transport server: %v", err)
	}
	e.base, e.close = base, srv.Close
	jar, err := cookiejar.New(nil)
	if err != nil {
		_ = srv.Close()
		t.Fatalf("cookie jar: %v", err)
	}
	e.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := e.client.Get(base + "/connect?token=" + srv.Token())
	if err != nil {
		_ = srv.Close()
		t.Fatalf("换 cookie: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		_ = srv.Close()
		t.Fatalf("connect status=%d", resp.StatusCode)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return e
}

// newSession 建一条真实父会话（`subagents.prompt` 会先校验父会话存在）。
func (e *subagentE2E) newSession(id string) string {
	e.t.Helper()
	if id == "" {
		id = session.NewID()
	}
	lg, err := session.CreateSession(e.home, e.home, id, "default")
	if err != nil {
		e.t.Fatalf("建会话 %q: %v", id, err)
	}
	if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "派活"}})); err != nil {
		e.t.Fatalf("写会话 %q: %v", id, err)
	}
	if err := lg.Close(); err != nil {
		e.t.Fatalf("关会话 %q: %v", id, err)
	}
	return id
}

// post 发一次官方方法，返回 (HTTP 状态码, 原样 JSON)。
func (e *subagentE2E) post(method string, params any) (int, []byte) {
	e.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		e.t.Fatalf("编参数: %v", err)
	}
	resp, err := e.client.Post(e.base+"/api/"+method, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		e.t.Fatalf("POST %s: %v", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("读 %s 的响应: %v", method, err)
	}
	return resp.StatusCode, body
}

// promptRequest 是官方 SubagentPromptRequest 的最小合法载荷。
func promptRequest(requestID, parentID, childID, delivery string) map[string]any {
	return map[string]any{"request": map[string]any{
		"requestId":       requestID,
		"parentSessionId": parentID,
		"childSessionId":  childID,
		"mode":            "continuable",
		"delivery":        delivery,
		"content":         []map[string]any{{"type": "text", "text": "继续说说刚才那个结论"}},
	}}
}

// decode 把线上 JSON 解成 {error.code, result.messageId} 两个关心的字段。
func decode(t *testing.T, raw []byte) (code string, messageID string) {
	t.Helper()
	var got struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
		Result *struct {
			MessageID string `json:"messageId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解响应 %s: %v", string(raw), err)
	}
	if got.Error != nil {
		code = got.Error.Code
	}
	if got.Result != nil {
		messageID = got.Result.MessageID
	}
	return code, messageID
}

// ---------------------------------------------------------------------------
// 1. 未登记 → no_such_session（红线的落点）
// ---------------------------------------------------------------------------

// TestSubagentPromptUnspawnedChildIsNoSuchSession 钉住"没有地址就没有 messageId"。
//
// 两种"没地址"都算：从来没 spawn 过（会话文件都不存在），以及
// 会话存在但**没有父子帧**（普通会话不许被人当成谁的孩子去投喂）。
func TestSubagentPromptUnspawnedChildIsNoSuchSession(t *testing.T) {
	e := newSubagentE2E(t, nil)
	parent := e.newSession("")

	status, raw := e.post("subagents.prompt", promptRequest("req-1", parent, "child-never-spawned", "queue"))
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d 期望 400；body=%s", status, raw)
	}
	if code, messageID := decode(t, raw); code != "no_such_session" || messageID != "" {
		t.Fatalf("code=%q messageId=%q（都必须为空/ no_such_session）；body=%s", code, messageID, raw)
	}
	if len(e.delivered) != 0 {
		t.Fatalf("没登记的 child 被投递了: %v", e.delivered)
	}

	// 普通会话（无父子帧）同样不许被投。
	plain := e.newSession("")
	status, raw = e.post("subagents.prompt", promptRequest("req-2", parent, plain, "queue"))
	if status != http.StatusBadRequest {
		t.Fatalf("普通会话 status=%d 期望 400；body=%s", status, raw)
	}
	if code, messageID := decode(t, raw); code != "no_such_session" || messageID != "" {
		t.Fatalf("普通会话被认成子会话: code=%q messageId=%q", code, messageID)
	}
}

// ---------------------------------------------------------------------------
// 2. 别的进程 spawn 的 child → 认领后可投
// ---------------------------------------------------------------------------

// TestSubagentPromptAdoptsChildSpawnedElsewhere 是这次接线的核心用例。
//
// 真实链路里 child 是 `dshd run` 子进程 spawn 的（子代理工具在那里执行），
// 而 `subagents.prompt` 由 serve 进程处理——两个进程之间只有 v4 日志。
// 所以这里**不往内存注册表塞任何东西**，只让生产 spawn 函数把盘上的事实写出来，
// 再走真 HTTP 投递：能 200 就说明"跨进程认领"真的通了。
func TestSubagentPromptAdoptsChildSpawnedElsewhere(t *testing.T) {
	e := newSubagentE2E(t, nil)
	parent := e.newSession("")
	rec, err := (agent.ChildSpawn{Home: e.home, ParentSessionID: parent, Cwd: e.home}).
		Spawn(agent.ChildSpawnOptions{ChildSessionID: "child-live-1", Prompt: "数文件", Answer: "三个"})
	if err != nil {
		t.Fatalf("生产 spawn: %v", err)
	}

	status, raw := e.post("subagents.prompt", promptRequest("req-ok", parent, rec.ChildSessionID, "queue"))
	if status != http.StatusOK {
		t.Fatalf("status=%d 期望 200；body=%s", status, raw)
	}
	code, messageID := decode(t, raw)
	if code != "" || messageID == "" {
		t.Fatalf("code=%q messageId=%q（成功必须带 messageId）；body=%s", code, messageID, raw)
	}
	if len(e.delivered) != 1 || !strings.HasPrefix(e.delivered[0], rec.ChildSessionID+"|") {
		t.Fatalf("投递没落到子会话上: %v", e.delivered)
	}
	if !strings.Contains(e.delivered[0], "继续说说刚才那个结论") {
		t.Fatalf("投递正文不是那条续问: %q", e.delivered[0])
	}

	// 回执必须真落进子会话 v4 日志（`subagent/message` 帧），不是一个只在内存里的数。
	path, err := session.FindByID(e.home, rec.ChildSessionID)
	if err != nil {
		t.Fatalf("找子会话: %v", err)
	}
	if !logHasEvent(t, path, "subagent/message") {
		t.Fatal("子会话日志里没有 subagent/message 回执帧")
	}
}

// TestSubagentPromptAdoptionDoesNotTruncateHistory 钉住"认领 ≠ 重建"。
//
// 第二次投递要让服务端再认一次人（新进程 / 另一条连接），这条路径绝不能
// 重新 CreateSession——v4.Create 打开即截断，子代理的历史会被清空。
func TestSubagentPromptAdoptionDoesNotTruncateHistory(t *testing.T) {
	e := newSubagentE2E(t, nil)
	parent := e.newSession("")
	if _, err := (agent.ChildSpawn{Home: e.home, ParentSessionID: parent, Cwd: e.home}).
		Spawn(agent.ChildSpawnOptions{ChildSessionID: "child-keep", Prompt: "做事", Answer: "做完了"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	before := logEvents(t, mustFind(t, e.home, "child-keep"))

	// 第一个进程投一次（会认领一次）。
	if status, raw := e.post("subagents.prompt", promptRequest("req-a", parent, "child-keep", "queue")); status != http.StatusOK {
		t.Fatalf("第一次投递 status=%d body=%s", status, raw)
	}
	// 同一个 home 上再起一套宿主（= 另一个进程 / 重启后的 serve），它没有任何
	// 内存状态，只能从盘上认——这条路径绝不许重建会话。
	other := newSubagentE2EAt(t, e.home, nil)
	if status, raw := other.post("subagents.prompt", promptRequest("req-b", parent, "child-keep", "queue")); status != http.StatusOK {
		t.Fatalf("第二个进程投递 status=%d body=%s", status, raw)
	}
	after := logEvents(t, mustFind(t, e.home, "child-keep"))
	if len(after) < len(before) {
		t.Fatalf("认领把历史截断了: %d → %d 帧", len(before), len(after))
	}
	for i := range before {
		if before[i].Type != after[i].Type || before[i].Seq != after[i].Seq {
			t.Fatalf("第 %d 帧被改写: %+v → %+v", i, before[i], after[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 3. 父 id 不匹配 → 403 cross_session
// ---------------------------------------------------------------------------

func TestSubagentPromptCrossSessionIsForbidden(t *testing.T) {
	e := newSubagentE2E(t, nil)
	parentA := e.newSession("")
	parentB := e.newSession("")
	if _, err := (agent.ChildSpawn{Home: e.home, ParentSessionID: parentA, Cwd: e.home}).
		Spawn(agent.ChildSpawnOptions{ChildSessionID: "child-A", Prompt: "x", Answer: "y"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	status, raw := e.post("subagents.prompt", promptRequest("req-cross", parentB, "child-A", "queue"))
	if status != http.StatusForbidden {
		t.Fatalf("status=%d 期望 403；body=%s", status, raw)
	}
	if code, messageID := decode(t, raw); code != "cross_session" || messageID != "" {
		t.Fatalf("code=%q messageId=%q；body=%s", code, messageID, raw)
	}
	if len(e.delivered) != 0 {
		t.Fatalf("跨会话投递被放行了: %v", e.delivered)
	}

	// 同一个 child，换回真父就通——说明 403 来自归属判定，不是"这条 child 坏了"。
	if status, raw := e.post("subagents.prompt", promptRequest("req-same", parentA, "child-A", "queue")); status != http.StatusOK {
		t.Fatalf("真父投递 status=%d body=%s", status, raw)
	}
}

// ---------------------------------------------------------------------------
// 4. 投递失败不回 messageId；同 requestId 幂等
// ---------------------------------------------------------------------------

func TestSubagentPromptDeliveryFailureNeverReturnsMessageID(t *testing.T) {
	e := newSubagentE2E(t, nil)
	e.deliverErr = errSubagentDeliver
	parent := e.newSession("")
	if _, err := (agent.ChildSpawn{Home: e.home, ParentSessionID: parent, Cwd: e.home}).
		Spawn(agent.ChildSpawnOptions{ChildSessionID: "child-fail", Prompt: "x", Answer: "y"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	status, raw := e.post("subagents.prompt", promptRequest("req-fail", parent, "child-fail", "queue"))
	if status == http.StatusOK {
		t.Fatalf("投递失败却回了 200: %s", raw)
	}
	if code, messageID := decode(t, raw); messageID != "" {
		t.Fatalf("投递失败却回了 messageId=%q（code=%q）；body=%s", messageID, code, raw)
	}
	if !strings.Contains(string(raw), "投递失败") && !strings.Contains(string(raw), errSubagentDeliver.Error()) {
		t.Fatalf("失败原因没说出来: %s", raw)
	}
}

func TestSubagentPromptIsIdempotentPerRequestID(t *testing.T) {
	e := newSubagentE2E(t, nil)
	parent := e.newSession("")
	if _, err := (agent.ChildSpawn{Home: e.home, ParentSessionID: parent, Cwd: e.home}).
		Spawn(agent.ChildSpawnOptions{ChildSessionID: "child-idem", Prompt: "x", Answer: "y"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	req := promptRequest("req-same-id", parent, "child-idem", "queue")
	status1, raw1 := e.post("subagents.prompt", req)
	status2, raw2 := e.post("subagents.prompt", req)
	if status1 != http.StatusOK || status2 != http.StatusOK {
		t.Fatalf("两次都要 200：%d/%s %d/%s", status1, raw1, status2, raw2)
	}
	_, id1 := decode(t, raw1)
	_, id2 := decode(t, raw2)
	if id1 == "" || id1 != id2 {
		t.Fatalf("同一 requestId 必须回同一个 messageId：%q vs %q", id1, id2)
	}
	if len(e.delivered) != 1 {
		t.Fatalf("同一 requestId 只许投一次，实投 %d 次: %v", len(e.delivered), e.delivered)
	}
}

// ---------------------------------------------------------------------------
// 5. interruptByParent：真父 accepted，假父 403，未登记 400
// ---------------------------------------------------------------------------

func TestSubagentInterruptByParentUsesRealAuthority(t *testing.T) {
	e := newSubagentE2E(t, nil)
	parentA := e.newSession("")
	parentB := e.newSession("")
	if _, err := (agent.ChildSpawn{Home: e.home, ParentSessionID: parentA, Cwd: e.home}).
		Spawn(agent.ChildSpawnOptions{ChildSessionID: "child-int", Prompt: "x", Answer: "y"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	params := func(child, parent string) map[string]any {
		return map[string]any{"childSessionId": child, "parentSessionId": parent, "mode": "continuable"}
	}

	status, raw := e.post("subagents.interruptByParent", params("child-int", parentA))
	if status != http.StatusOK {
		t.Fatalf("真父中断 status=%d body=%s", status, raw)
	}
	if len(e.interrupts) != 1 || e.interrupts[0] != "child-int" {
		t.Fatalf("取消通道没被真调到: %v", e.interrupts)
	}

	status, raw = e.post("subagents.interruptByParent", params("child-int", parentB))
	if status != http.StatusForbidden {
		t.Fatalf("假父中断 status=%d 期望 403；body=%s", status, raw)
	}
	if code, _ := decode(t, raw); code != "cross_session" {
		t.Fatalf("code=%q 期望 cross_session；body=%s", code, raw)
	}

	status, raw = e.post("subagents.interruptByParent", params("child-nope", parentA))
	if status != http.StatusBadRequest {
		t.Fatalf("未登记中断 status=%d 期望 400；body=%s", status, raw)
	}
	if code, _ := decode(t, raw); code != "no_such_session" {
		t.Fatalf("code=%q 期望 no_such_session；body=%s", code, raw)
	}
}

// TestSubagentInterruptWhenChildIsIdleIsAccepted 钉住官方取消语义的**真实**落点。
//
// 这里用的取消通道是**宿主真的那个** promptService（不是替身），所以"没有在飞的一轮"
// 是它真的会回的错误（ErrNoSuchTurn）。官方语义：accepted 只表示取消信号被接受，
// "目标本来就静止"是真实状态、不是失败——把它当 500 回出去，界面会把"已经停了"
// 显示成"取消失败"。
func TestSubagentInterruptWhenChildIsIdleIsAccepted(t *testing.T) {
	idle := newPromptService(subagentNoopRunner{})
	e := newSubagentE2E(t, func(deps *agentOfficialDeps) {
		deps.interrupt = func(sessionID string) error {
			_, err := idle.CancelPlacement(sessionID, "interrupt", "")
			return err
		}
	})
	parent := e.newSession("")
	if _, err := (agent.ChildSpawn{Home: e.home, ParentSessionID: parent, Cwd: e.home}).
		Spawn(agent.ChildSpawnOptions{ChildSessionID: "child-idle", Prompt: "x", Answer: "y"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	status, raw := e.post("subagents.interruptByParent", map[string]any{
		"childSessionId": "child-idle", "parentSessionId": parent, "mode": "continuable",
	})
	if status != http.StatusOK {
		t.Fatalf("空闲子会话中断 status=%d 期望 200（没有在飞的一轮不是失败）；body=%s", status, raw)
	}
	if !strings.Contains(string(raw), `"accepted":true`) {
		t.Fatalf("空闲子会话中断没回 accepted:true；body=%s", raw)
	}
}

// ---------------------------------------------------------------------------
// 6. 生产 spawn 调用点：subRunner.RunSub + Continuable
// ---------------------------------------------------------------------------

// TestSubagentContinuableSpawnFromRunner 走的是**生产调用点**：
// `spi.SubagentRunner` 的唯一实现 subRunner。它跑完委派后必须留下一条
// durable 子会话，而这条子会话必须马上能被 `subagents.prompt` 找到。
func TestSubagentContinuableSpawnFromRunner(t *testing.T) {
	e := newSubagentE2E(t, nil)
	parent := e.newSession("")
	r := &subRunner{
		chat:     subagentFakeChat{reply: "文件一共三个"},
		tools:    subagentEmptyRegistry{},
		model:    "test-model",
		maxSteps: 2,
		timeout:  10 * time.Second,
		home:     e.home,
	}
	ctx := spi.WithSession(context.Background(), parent)
	res, err := r.RunSub(ctx, spi.SubagentRequest{
		Prompt: "数一下 sessions 目录里有几个文件", Description: "数文件", Continuable: true,
	})
	if err != nil {
		t.Fatalf("RunSub: %v", err)
	}
	if res.ChildSessionID == "" {
		t.Fatal("Continuable 委派没有回 ChildSessionID")
	}
	if res.Text != "文件一共三个" {
		t.Fatalf("正文不对: %q", res.Text)
	}
	// 盘上必须真有这条子会话，而且归属就是父会话。
	rec, ok, err := agent.FindChild(e.home, res.ChildSessionID)
	if err != nil || !ok {
		t.Fatalf("spawn 后的子会话认不回来: ok=%v err=%v", ok, err)
	}
	if rec.ParentSessionID != parent {
		t.Fatalf("归属不对: %+v", rec)
	}
	// 委派往返必须进了子会话历史（续问的前提）。
	for _, want := range []string{"数一下 sessions 目录里有几个文件", "文件一共三个"} {
		if !logContainsText(t, mustFind(t, e.home, res.ChildSessionID), want) {
			t.Fatalf("子会话历史里没有 %q", want)
		}
	}
	// 紧接着就能投——这就是"真接到生产"该有的样子。
	status, raw := e.post("subagents.prompt", promptRequest("req-after-spawn", parent, res.ChildSessionID, "queue"))
	if status != http.StatusOK {
		t.Fatalf("spawn 之后投递 status=%d body=%s", status, raw)
	}
	if _, messageID := decode(t, raw); messageID == "" {
		t.Fatalf("没回 messageId: %s", raw)
	}
}

// TestSubagentContinuableWithoutParentOrHomeFailsLoudly 钉住"不假装可续问"。
//
// 没有父就是没有授权边界、没有 home 就落不了盘——两种都必须是错误，
// 而不是回一个指向空气的 child id。
func TestSubagentContinuableWithoutParentOrHomeFailsLoudly(t *testing.T) {
	e := newSubagentE2E(t, nil)
	r := &subRunner{
		chat: subagentFakeChat{reply: "好"}, tools: subagentEmptyRegistry{},
		model: "m", maxSteps: 1, timeout: 5 * time.Second, home: e.home,
	}
	if _, err := r.RunSub(context.Background(), spi.SubagentRequest{Prompt: "做点事", Continuable: true}); err == nil {
		t.Fatal("ctx 上没有会话身份时必须失败")
	}
	// 有父但没有 home：同样不许回 child id。
	r.home = ""
	ctx := spi.WithSession(context.Background(), e.newSession(""))
	if _, err := r.RunSub(ctx, spi.SubagentRequest{Prompt: "做点事", Continuable: true}); err == nil {
		t.Fatal("没有 home 时必须失败")
	}
	// 非 Continuable：老行为一个字节都不变，不建任何会话。
	r.home = e.home
	res, err := r.RunSub(ctx, spi.SubagentRequest{Prompt: "做点事"})
	if err != nil {
		t.Fatalf("普通委派不该失败: %v", err)
	}
	if res.ChildSessionID != "" {
		t.Fatalf("非 Continuable 却回了 child id: %q", res.ChildSessionID)
	}
}

// ---------------------------------------------------------------------------
// 7. 真机探针的播种口
// ---------------------------------------------------------------------------

// TestSubagentRealMachineSeedChild 是**真机探针的播种口**，不是产品测试。
//
// 为什么需要它：真机上 `subagents.prompt` 要有一个"别的进程 spawn 出来的 child"
// 才有地址，而正常 spawn 由模型触发子代理工具完成（要模型、要时序）。为了不把
// 真机验证绑在模型抖动上，这里用**生产 spawn 函数**往指定 home 里播一个 child，
// 之后整条 HTTP 链路都是真的。
//
// 默认跳过：没设 DSHD_SUBAGENT_SEED_HOME 时它什么都不做，也不污染别的测试。
func TestSubagentRealMachineSeedChild(t *testing.T) {
	home := strings.TrimSpace(os.Getenv("DSHD_SUBAGENT_SEED_HOME"))
	if home == "" {
		t.Skip("真机探针专用：设置 DSHD_SUBAGENT_SEED_HOME 才跑")
	}
	parent := strings.TrimSpace(os.Getenv("DSHD_SUBAGENT_SEED_PARENT"))
	child := strings.TrimSpace(os.Getenv("DSHD_SUBAGENT_SEED_CHILD"))
	rec, err := (agent.ChildSpawn{Home: home, ParentSessionID: parent, Cwd: home}).Spawn(
		agent.ChildSpawnOptions{
			ChildSessionID: child,
			Prompt:         "数一下 sessions 目录里有几个会话",
			Answer:         "一共一个（父会话）",
		})
	if err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	t.Logf("spawned child=%s parent=%s cwd=%s", rec.ChildSessionID, rec.ParentSessionID, rec.Cwd)
}

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// errSubagentDeliver 是"宿主投递通道拒绝"的真实错误面。
var errSubagentDeliver = &subagentDeliverError{}

type subagentDeliverError struct{}

func (*subagentDeliverError) Error() string { return "prompt 通道正忙（测试注入）" }

// subagentNoopRunner 是"永远不会真的跑一轮"的 turnRunner：
// 用它造出的 promptService 是**真空闲**的，取消它必然回 ErrNoSuchTurn。
type subagentNoopRunner struct{}

func (subagentNoopRunner) Run(ctx context.Context, sessionID, turnID, text string) error { return nil }

// subagentFakeChat 是不发网络的模型替身：只回一句正文，不产生工具调用。
type subagentFakeChat struct{ reply string }

func (c subagentFakeChat) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event) error) (*llm.Response, error) {
	return &llm.Response{Content: c.reply, FinishReason: "stop"}, nil
}

// subagentEmptyRegistry 是空工具表：RunSub 会把它包进 noSubagent。
type subagentEmptyRegistry struct{}

func (subagentEmptyRegistry) Tools() []spi.Tool { return nil }
func (subagentEmptyRegistry) Get(string) (spi.Tool, bool) {
	return nil, false
}
func (subagentEmptyRegistry) Register(spi.Tool) error { return nil }

// readV4Log 读一份会话日志（跨进程/重启读的是同一份东西）。
func readV4Log(path string) (*v4.Log, error) { return v4.ReadFile(path) }

func mustFind(t *testing.T, home, id string) string {
	t.Helper()
	path, err := session.FindByID(home, id)
	if err != nil {
		t.Fatalf("按 id 找会话 %q: %v", id, err)
	}
	return path
}

func logEvents(t *testing.T, path string) []struct {
	Type string
	Seq  int64
} {
	t.Helper()
	log, err := readV4Log(path)
	if err != nil {
		t.Fatalf("读会话 %s: %v", path, err)
	}
	out := make([]struct {
		Type string
		Seq  int64
	}, 0, len(log.Events))
	for _, ev := range log.Events {
		out = append(out, struct {
			Type string
			Seq  int64
		}{Type: ev.Type, Seq: ev.Seq})
	}
	return out
}

func logHasEvent(t *testing.T, path, eventType string) bool {
	t.Helper()
	log, err := readV4Log(path)
	if err != nil {
		t.Fatalf("读会话 %s: %v", path, err)
	}
	for _, ev := range log.Events {
		if ev.Type == eventType {
			return true
		}
	}
	return false
}

func logContainsText(t *testing.T, path, want string) bool {
	t.Helper()
	log, err := readV4Log(path)
	if err != nil {
		t.Fatalf("读会话 %s: %v", path, err)
	}
	for _, ev := range log.Events {
		if strings.Contains(string(ev.Data), want) || strings.Contains(string(ev.Raw), want) {
			return true
		}
	}
	return false
}
