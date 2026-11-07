package main

// 官方 0.2.2 #14 `session-title-llm`（first-prompt）的宿主验收。
//
// 纪律：
//   - 只用**进程内可控假网关**（httptest）验证，绝不真调 magpie/生产网关：
//     `DSHD_LLM_BASE` 指到假网关，并断言它收到的请求条数与形状；
//   - 走**生产接线**：`modelOverrideLoopRunner`（cmdServe 唯一的默认 runner）、
//     真 v4 文件、真索引、真 Transport journal，不另写一条测试专用实现；
//   - 「首轮之后有没有 session/title」只看 `session.list` 的真实行（title/titleSource），
//     不读测试自己的影子状态。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// autoTitleEvent 是直接写进 v4 的一条事件（测试夹具用）。
type autoTitleEvent struct {
	typ  string
	data any
}

// autoTitleSession 建一份 v4 日志并写入给定事件。
//
// 不复用 session.CreateSession：seeded 头（官方 first-prompt provider 会跳过）
// 只有 v4.Create 能造出来，而"跳过 seeded"正是要钉的其中一格。
func autoTitleSession(t *testing.T, home, id string, seeded bool, events []autoTitleEvent) string {
	t.Helper()
	cwd := filepath.Join(home, "workspace-"+id)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	seg, err := v4.EncodeSegment(id)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "sessions", v4.ProjectKey(cwd), seg, "session.v4.jsonl.zstd")
	w, err := v4.Create(path, v4.Header{
		Type: "session", Version: 4, ID: id, CreatedAt: time.Now().UnixMilli(),
		Cwd: cwd, IsSeeded: seeded, AgentPreset: "standard",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range events {
		raw, err := json.Marshal(ev.data)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Append(v4.Event{Type: ev.typ, Seq: int64(i), Time: time.Now().UnixMilli(), Data: raw}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// autoTitleTurn 复刻真实写入顺序：turn/start → user/message → assistant/message → turn/end。
func autoTitleTurn(turn int, prompt string) []autoTitleEvent {
	return []autoTitleEvent{
		{"turn/start", session.TurnStartData(turn)},
		{"user/message", session.UserMessage([]spi.Block{{Type: "text", Text: prompt}})},
		{"assistant/message", session.AssistantMessageEvent(turn, 1, []map[string]any{session.TextBlock("ok")}, "stop", nil)},
		{"turn/end", session.TurnEndData(turn, "completed", "")},
	}
}

// autoTitleSeed 建一份"首轮已正常收口"的会话（官方触发条件的最小现场）。
func autoTitleSeed(t *testing.T, home, id, prompt string) string {
	t.Helper()
	events := []autoTitleEvent{{"system/message", session.SystemMessageData(1, 1, "system")}}
	events = append(events, autoTitleTurn(1, prompt)...)
	return autoTitleSession(t, home, id, false, events)
}

// autoTitleGatewayRequest 是假网关收到的请求体（只声明真正断言的字段）。
type autoTitleGatewayRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
	Tools  json.RawMessage
	// MaxTokens 是 max_tokens；兼容形状下可能改名，测试按"两个都为 0 就不对"判。
	MaxTokens           int `json:"max_tokens"`
	MaxCompletionTokens int `json:"max_completion_tokens"`
	Messages            []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (r autoTitleGatewayRequest) text(i int) string {
	if i < 0 || i >= len(r.Messages) {
		return ""
	}
	var s string
	if err := json.Unmarshal(r.Messages[i].Content, &s); err == nil {
		return s
	}
	return string(r.Messages[i].Content)
}

type autoTitleGateway struct {
	server *httptest.Server
	reply  func(autoTitleGatewayRequest) (content string, status int, delay time.Duration)

	mu   sync.Mutex
	reqs []autoTitleGatewayRequest
}

func newAutoTitleGateway(t *testing.T, reply func(autoTitleGatewayRequest) (string, int, time.Duration)) *autoTitleGateway {
	t.Helper()
	g := &autoTitleGateway{reply: reply}
	if g.reply == nil {
		// 模型不听话时的真实形状：星号包裹 + 第二行解释。官方只取首行并去掉整行星号。
		g.reply = func(autoTitleGatewayRequest) (string, int, time.Duration) {
			return "**Fix the login redirect**\nHere is why this title fits.", http.StatusOK, 0
		}
	}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var req autoTitleGatewayRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			http.Error(w, "bad request json", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.reqs = append(g.reqs, req)
		g.mu.Unlock()
		content, status, delay := g.reply(req)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status != http.StatusOK {
			http.Error(w, "gateway refused", status)
			return
		}
		autoTitleWriteSSE(w, content)
	}))
	t.Cleanup(g.server.Close)
	return g
}

func autoTitleWriteSSE(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	payload, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"delta":         map[string]any{"content": content},
			"finish_reason": "stop",
		}},
	})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
}

func (g *autoTitleGateway) base() string { return g.server.URL + "/v1" }

func (g *autoTitleGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.reqs)
}

func (g *autoTitleGateway) request(i int) autoTitleGatewayRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reqs[i]
}

// autoTitleRunner 用生产默认接线造一个 runner。
//
// DSHD_MODEL_WIRE_ARGV 让 serve_loop_test.go 的 init() 在子进程里拦下 `run ...`：
// 测试不起真模型、不留子进程，只走"这一轮成功收口"这条真实路径。
func autoTitleRunner(t *testing.T, home, gatewayBase string, journals *transport.Journals, pub func(string, transport.ServerFrame)) *modelOverrideLoopRunner {
	t.Helper()
	t.Setenv("DSHD_MODEL_WIRE_ARGV", filepath.Join(t.TempDir(), "argv.json"))
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	t.Setenv("DSHD_LLM_BASE", gatewayBase)
	return &modelOverrideLoopRunner{base: &loopRunner{
		home: home, policy: "deny", maxSteps: 6, model: "auto-title-model",
		journals: journals, pub: pub,
	}}
}

func autoTitleEvents(t *testing.T, path string) []v4.Event {
	t.Helper()
	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读会话日志: %v", err)
	}
	return log.Events
}

func autoTitleEventTypes(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, ev := range autoTitleEvents(t, path) {
		out = append(out, ev.Type)
	}
	return out
}

// waitAutoTitle 轮询真实 `session.list` 行，直到出现 auto 标题。
// 判据只用 titleSource=="auto"：没有标题时 list 会拿摘要冒充 title，不能拿它当成功。
func waitAutoTitle(t *testing.T, home, sessionID, path string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		rows, err := listSessions(home, "")
		if err != nil {
			t.Fatalf("session.list: %v", err)
		}
		for _, row := range rows {
			if row.ID != sessionID {
				continue
			}
			if row.TitleSource == "auto" {
				return row.Title
			}
			if time.Now().After(deadline) {
				t.Fatalf("首轮正常收口后没有 auto 标题：list 行=%+v；v4 事件类型=%v",
					row, autoTitleEventTypes(t, path))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("session.list 里根本没有会话 %s", sessionID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAutoTitleFirstTurnWritesDurableAutoTitle 是这一格的先红用例：
// 首轮 turn 正常结束后必须出现官方形状的 session/title（source=auto），
// 并且重启后 session.list 逐字回读同一个标题。
func TestAutoTitleFirstTurnWritesDurableAutoTitle(t *testing.T) {
	home := t.TempDir()
	const sessionID = "session-auto-title"
	path := autoTitleSeed(t, home, sessionID, "please fix the login redirect")

	before := autoTitleEvents(t, path)
	beforeTypes := autoTitleEventTypes(t, path)

	gateway := newAutoTitleGateway(t, nil)
	journals := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, journals); err != nil {
		t.Fatal(err)
	}
	var pubMu sync.Mutex
	var published []transport.ServerFrame
	pub := func(topic string, f transport.ServerFrame) {
		pubMu.Lock()
		published = append(published, f)
		pubMu.Unlock()
	}
	runner := autoTitleRunner(t, home, gateway.base(), journals, pub)
	if err := runner.Run(context.Background(), sessionID, "turn-1", "please fix the login redirect"); err != nil {
		t.Fatalf("首轮正常收口不该失败: %v", err)
	}

	title := waitAutoTitle(t, home, sessionID, path, 3*time.Second)
	if title != "Fix the login redirect" {
		t.Fatalf("标题=%q，要 %q（首行 + 去整行星号 + 空白归一）", title, "Fix the login redirect")
	}

	// 一次、且只有一次的独立调用：模型/两段消息/输出上限/无工具。
	if got := gateway.count(); got != 1 {
		t.Fatalf("标题调用次数=%d，要恰好 1 次", got)
	}
	req := gateway.request(0)
	if req.Model != "auto-title-model" {
		t.Fatalf("标题调用 model=%q，要和本轮同一模型", req.Model)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("标题调用消息=%+v，要 [system,user] 两段", req.Messages)
	}
	if !strings.Contains(req.text(1), "please fix the login redirect") {
		t.Fatalf("标题输入没有首条用户消息：%q", req.text(1))
	}
	if len(req.Tools) != 0 && string(req.Tools) != "null" {
		t.Fatalf("标题调用带了工具表：%s", req.Tools)
	}
	if req.MaxTokens == 0 && req.MaxCompletionTokens == 0 {
		t.Fatalf("标题调用没有输出上限：%+v", req)
	}

	// v4 只多了**一条** session/title，形状与官方一致（messageSeqs 为空数组）。
	after := autoTitleEvents(t, path)
	if len(after) != len(before)+1 {
		t.Fatalf("v4 事件数 %d → %d，只许多一条 session/title；类型=%v", len(before), len(after), autoTitleEventTypes(t, path))
	}
	var titleData struct {
		Title       string  `json:"title"`
		MessageSeqs []int64 `json:"messageSeqs"`
		Source      struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	last := after[len(after)-1]
	if last.Type != "session/title" {
		t.Fatalf("最后一条 v4 事件=%q，要 session/title", last.Type)
	}
	if err := json.Unmarshal(last.Data, &titleData); err != nil {
		t.Fatal(err)
	}
	if titleData.Title != title || titleData.MessageSeqs == nil || len(titleData.MessageSeqs) != 0 || titleData.Source.Kind != "auto" {
		t.Fatalf("session/title 形状=%+v（要 title=%q、messageSeqs=[]、source.kind=auto）", titleData, title)
	}

	// 不污染主 turn 的模型历史：投影逐字相同，且没有新增 user/assistant 事件。
	if got := historyForModel(autoTitleReadLog(t, path)); !reflect.DeepEqual(got, historyForModel(autoTitleReadLogBefore(t, before))) {
		t.Fatalf("标题调用污染了模型历史:\n前=%+v\n后=%+v", historyForModel(autoTitleReadLogBefore(t, before)), got)
	}
	for _, typ := range autoTitleEventTypes(t, path) {
		switch typ {
		case "session/title", "system/message", "turn/start", "turn/end":
		default:
			if !autoTitleContains(beforeTypes, typ) {
				t.Fatalf("标题调用写了额外事件 %q", typ)
			}
		}
	}

	// turn/done 先于标题帧：标题调用不能挡住主 turn 的收尾。
	// 索引先落盘、帧后发，所以这里等的是"帧真的到了"，不是再等一次索引。
	titleAt, turnDoneAt := waitAutoTitleFrame(t, &pubMu, &published, 5*time.Second)
	if turnDoneAt < 0 || titleAt < 0 || turnDoneAt > titleAt {
		t.Fatalf("推送顺序 turn/done=%d session/title=%d（%+v）", turnDoneAt, titleAt, published)
	}

	// 重启后 session.list 逐字回读；Transport journal 里的原始帧也还在。
	srv := startMetadataInitHTTPServer(t, home)
	defer srv.close()
	row := metadataSessionByID(t, srv, sessionID)
	if row["title"] != title || row["titleSource"] != "auto" || row["titleExplicit"] != true {
		t.Fatalf("重启后 session.list 行=%+v", row)
	}
	var history transport.HistoryResult
	if err := json.Unmarshal(callW145HTTP(t, srv, "session.history", map[string]any{"id": sessionID, "sinceSeq": 0}, http.StatusOK), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Frames) != 2 || history.Frames[0].Type != TurnDone || history.Frames[1].Type != "session/title" {
		t.Fatalf("重启后 journal 帧=%+v", history.Frames)
	}
	var journalTitle struct {
		Title  string `json:"title"`
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(history.Frames[1].Data, &journalTitle); err != nil {
		t.Fatal(err)
	}
	if journalTitle.Title != title || journalTitle.Source.Kind != "auto" {
		t.Fatalf("journal 里的标题帧=%+v", journalTitle)
	}
}

// waitAutoTitleFrame 等标题帧真的被推出去，并返回 (标题帧下标, turn/done 帧下标)。
func waitAutoTitleFrame(t *testing.T, mu *sync.Mutex, published *[]transport.ServerFrame, within time.Duration) (titleAt, turnDoneAt int) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		mu.Lock()
		titleAt, turnDoneAt = -1, -1
		for i, frame := range *published {
			switch frame.Type {
			case "session/title":
				if titleAt < 0 {
					titleAt = i
				}
			case TurnDone:
				if turnDoneAt < 0 {
					turnDoneAt = i
				}
			}
		}
		mu.Unlock()
		if titleAt >= 0 {
			return titleAt, turnDoneAt
		}
		if time.Now().After(deadline) {
			return -1, turnDoneAt
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func autoTitleReadLog(t *testing.T, path string) *v4.Log {
	t.Helper()
	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读会话日志: %v", err)
	}
	return log
}

// autoTitleReadLogBefore 把"标题写入之前"的事件快照包成一份只读 Log，用来比对投影。
func autoTitleReadLogBefore(t *testing.T, events []v4.Event) *v4.Log {
	t.Helper()
	return &v4.Log{
		Header: v4.Header{Type: "session", Version: 4, ID: "before"},
		Events: events,
	}
}

func autoTitleContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ---------- 跳过与优先级 ----------

// autoTitleDirect 直接构造生产服务（同步调用）：跳过/失败这两类判据必须能
// 在没有 goroutine 竞争的前提下断言"一次请求都没发"。
func autoTitleDirect(home, gatewayBase string, timeout time.Duration) (*autoTitleService, string) {
	service := newAutoTitleService(home, nil, nil)
	service.timeout = timeout
	return service, gatewayBase
}

func autoTitleListRow(t *testing.T, home, sessionID string) transport.SessionSummary {
	t.Helper()
	rows, err := listSessions(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == sessionID {
			return row
		}
	}
	t.Fatalf("session.list 里没有 %s", sessionID)
	return transport.SessionSummary{}
}

func autoTitleTitleEvents(t *testing.T, path string) []v4.Event {
	t.Helper()
	var out []v4.Event
	for _, ev := range autoTitleEvents(t, path) {
		if ev.Type == "session/title" {
			out = append(out, ev)
		}
	}
	return out
}

// TestAutoTitleSkipsWhenSessionAlreadyHasAnyTitle：已有的标题（用户显式 / fork 带过来的）
// 一律让 auto 退出；判据是"一次模型调用都不发"，不是"发了但没覆盖"。
func TestAutoTitleSkipsWhenSessionAlreadyHasAnyTitle(t *testing.T) {
	home := t.TempDir()
	const sessionID = "session-auto-title-user-wins"
	path := autoTitleSeed(t, home, sessionID, "please fix the login redirect")

	logw, err := session.OpenSessionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendSessionTitle(logw, "  用户显式标题 — 原样  "); err != nil {
		t.Fatal(err)
	}
	if err := logw.Close(); err != nil {
		t.Fatal(err)
	}

	gateway := newAutoTitleGateway(t, nil)
	service, base := autoTitleDirect(home, gateway.base(), time.Second)
	title, err := service.Run(context.Background(), sessionID, "auto-title-model", base, "")
	if err != nil || title != "" {
		t.Fatalf("已有用户标题时 Run=(%q,%v)，要 (\"\",nil) 的静默跳过", title, err)
	}
	if got := gateway.count(); got != 0 {
		t.Fatalf("已有标题仍发了 %d 次标题调用", got)
	}
	if events := autoTitleTitleEvents(t, path); len(events) != 1 {
		t.Fatalf("session/title 事件数=%d，只该有用户那一条", len(events))
	}
	row := autoTitleListRow(t, home, sessionID)
	if row.Title != "  用户显式标题 — 原样  " || row.TitleSource != "user" || !row.TitleExplicit {
		t.Fatalf("用户标题被改写：%+v", row)
	}
}

// TestAutoTitleUserRenameBeatsInFlightGeneration：用户标题在**自动生成在飞时**落盘，
// auto 必须放弃写入。这里用网关闸门精确制造这个交错：
// 标题请求已发出 → 用户 rename 走同一把会话写锁落盘 → 放行网关 → auto 的提交点重读。
func TestAutoTitleUserRenameBeatsInFlightGeneration(t *testing.T) {
	home := t.TempDir()
	const sessionID = "session-auto-title-race"
	path := autoTitleSeed(t, home, sessionID, "please fix the login redirect")

	released := make(chan struct{})
	gateway := newAutoTitleGateway(t, func(autoTitleGatewayRequest) (string, int, time.Duration) {
		<-released
		return "Auto Generated Title", http.StatusOK, 0
	})
	service, base := autoTitleDirect(home, gateway.base(), 5*time.Second)

	type outcome struct {
		title string
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		title, err := service.Run(context.Background(), sessionID, "auto-title-model", base, "")
		done <- outcome{title: title, err: err}
	}()

	// 等标题调用真的到了网关，再让用户改名 —— 此时 auto 还没拿到写锁。
	deadline := time.Now().Add(3 * time.Second)
	for gateway.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("标题调用没有到达假网关")
		}
		time.Sleep(5 * time.Millisecond)
	}
	metadata := newSessionMetadataService(home)
	if err := metadata.Rename(sessionID, "用户赢了这个标题", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(released)
	got := <-done

	if got.err != nil {
		t.Fatalf("在飞写情绪（用户已落盘）不设报错，拿到 %v", got.err)
	}
	if got.title != "" {
		t.Fatalf("用户已写标题，auto 不该报成功，拿到 %q", got.title)
	}
	events := autoTitleTitleEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("session/title 事件数=%d，只该有用户那一条：%+v", len(events), events)
	}
	row := autoTitleListRow(t, home, sessionID)
	if row.Title != "用户赢了这个标题" || row.TitleSource != "user" {
		t.Fatalf("最终标题=%+v，要用户那条", row)
	}
}

// TestAutoTitleUserRenameWinsAfterAutoTitle：auto 先落盘、用户随后改名 —— 用户仍然赢，
// 且之后的调用不再产生第二条 auto 标题。
func TestAutoTitleUserRenameWinsAfterAutoTitle(t *testing.T) {
	home := t.TempDir()
	const sessionID = "session-auto-title-rename-later"
	path := autoTitleSeed(t, home, sessionID, "please fix the login redirect")
	gateway := newAutoTitleGateway(t, nil)
	service, base := autoTitleDirect(home, gateway.base(), 3*time.Second)
	title, err := service.Run(context.Background(), sessionID, "auto-title-model", base, "")
	if err != nil || title != "Fix the login redirect" {
		t.Fatalf("首次自动标题=(%q,%v)", title, err)
	}
	if row := autoTitleListRow(t, home, sessionID); row.Title != title || row.TitleSource != "auto" {
		t.Fatalf("自动标题行=%+v", row)
	}
	metadata := newSessionMetadataService(home)
	if err := metadata.Rename(sessionID, "更准的手工标题", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if row := autoTitleListRow(t, home, sessionID); row.Title != "更准的手工标题" || row.TitleSource != "user" {
		t.Fatalf("改名后行=%+v", row)
	}
	// 再走一次真实入口：已有标题，不该再发调用，也不该再写第二条。
	if again, err := service.Run(context.Background(), sessionID, "auto-title-model", base, ""); err != nil || again != "" {
		t.Fatalf("已有标题后再跑=(%q,%v)，要静默跳过", again, err)
	}
	if got := gateway.count(); got != 1 {
		t.Fatalf("标题调用次数=%d，改名之后不该再发", got)
	}
	if events := autoTitleTitleEvents(t, path); len(events) != 2 {
		t.Fatalf("session/title 事件数=%d，要 1 auto + 1 user", len(events))
	}
}

// ---------- 非首轮 / seeded / 非正常收口 ----------

func TestAutoTitleSkipsNonFirstTurnAndSeededSessions(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		seeded    bool
		turns     []string
	}{
		{name: "second-turn", sessionID: "session-auto-title-turn2", turns: []string{"first prompt", "second prompt"}},
		{name: "seeded", sessionID: "session-auto-title-seeded", seeded: true, turns: []string{"seeded first prompt"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			events := []autoTitleEvent{{"system/message", session.SystemMessageData(1, 1, "system")}}
			for i, prompt := range tc.turns {
				events = append(events, autoTitleTurn(i+1, prompt)...)
			}
			path := autoTitleSession(t, home, tc.sessionID, tc.seeded, events)
			gateway := newAutoTitleGateway(t, nil)
			service, base := autoTitleDirect(home, gateway.base(), time.Second)
			title, err := service.Run(context.Background(), tc.sessionID, "auto-title-model", base, "")
			if err != nil || title != "" {
				t.Fatalf("不满足触发条件时 Run=(%q,%v)，要 (\"\",nil)", title, err)
			}
			if got := gateway.count(); got != 0 {
				t.Fatalf("不该发标题调用，实际 %d 次", got)
			}
			if got := autoTitleTitleEvents(t, path); len(got) != 0 {
				t.Fatalf("不该写 session/title，实际 %+v", got)
			}
		})
	}
}

// 首轮被取消/报错（turn/end 不是 completed 也不是 max-tokens）不算"正常收口"。
func TestAutoTitleSkipsAbortedFirstTurn(t *testing.T) {
	home := t.TempDir()
	const sessionID = "session-auto-title-aborted"
	path := autoTitleSession(t, home, sessionID, false, []autoTitleEvent{
		{"system/message", session.SystemMessageData(1, 1, "system")},
		{"turn/start", session.TurnStartData(1)},
		{"user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "cancel me"}})},
		{"turn/end", session.TurnEndData(1, "aborted", "用户取消")},
	})
	gateway := newAutoTitleGateway(t, nil)
	service, base := autoTitleDirect(home, gateway.base(), time.Second)
	if title, err := service.Run(context.Background(), sessionID, "auto-title-model", base, ""); err != nil || title != "" {
		t.Fatalf("被取消的首轮 Run=(%q,%v)，要 (\"\",nil)", title, err)
	}
	if got := gateway.count(); got != 0 {
		t.Fatalf("被取消的首轮不该发标题调用，实际 %d 次", got)
	}
	if got := autoTitleTitleEvents(t, path); len(got) != 0 {
		t.Fatalf("不该写 session/title，实际 %+v", got)
	}
}

// ---------- 失败不写、不重试 ----------

func TestAutoTitleFailureWritesNothingAndDoesNotRetry(t *testing.T) {
	cases := []struct {
		name    string
		reply   func(autoTitleGatewayRequest) (string, int, time.Duration)
		timeout time.Duration
		// maxCalls 是这一格允许的模型请求数上界：5xx/429/连接错误会走宿主统一的
		// llm-retry（官方 #20，1 首发 + 3 重试），空回答与超时都是一次即止。
		maxCalls int
	}{
		{
			name: "gateway-500",
			reply: func(autoTitleGatewayRequest) (string, int, time.Duration) {
				return "", http.StatusInternalServerError, 0
			},
			timeout:  3 * time.Second,
			maxCalls: 4,
		},
		{
			name: "empty-content",
			reply: func(autoTitleGatewayRequest) (string, int, time.Duration) {
				return "   \n\t ", http.StatusOK, 0
			},
			timeout:  3 * time.Second,
			maxCalls: 1,
		},
		{
			name: "timeout",
			reply: func(autoTitleGatewayRequest) (string, int, time.Duration) {
				return "Too Late Title", http.StatusOK, 5 * time.Second
			},
			timeout:  80 * time.Millisecond,
			maxCalls: 1,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			sessionID := "session-auto-title-" + tc.name
			path := autoTitleSeed(t, home, sessionID, "please fix the login redirect")
			gateway := newAutoTitleGateway(t, tc.reply)
			service, base := autoTitleDirect(home, gateway.base(), tc.timeout)
			start := time.Now()
			title, err := service.Run(context.Background(), sessionID, "auto-title-model", base, "")
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("失败必须报错，拿到 title=%q", title)
			}
			if title != "" {
				t.Fatalf("失败不该返回标题，拿到 %q", title)
			}
			if tc.name == "timeout" && elapsed > 2*time.Second {
				t.Fatalf("超时门没生效：耗时 %s", elapsed)
			}
			if got := gateway.count(); got < 1 || got > tc.maxCalls {
				t.Fatalf("标题调用次数=%d，要在 1..%d 之间（有界，不无界重试）", got, tc.maxCalls)
			}
			if got := autoTitleTitleEvents(t, path); len(got) != 0 {
				t.Fatalf("失败仍写了 session/title：%+v", got)
			}
			if _, statErr := os.Stat(autoTitleIndexPath(path)); !os.IsNotExist(statErr) {
				t.Fatalf("失败仍写了索引：%v", statErr)
			}
			row := autoTitleListRow(t, home, sessionID)
			if row.TitleSource != "" || row.TitleExplicit {
				t.Fatalf("失败后 list 行=%+v", row)
			}
		})
	}
}

// ---------- 不阻塞主 turn / 并发不串 ----------

// TestAutoTitleDoesNotBlockMainTurn：标题调用挂在网关上不动时，主 turn 的 Run
// 必须立刻返回、turn/done 帧必须已经推出去。
func TestAutoTitleDoesNotBlockMainTurn(t *testing.T) {
	home := t.TempDir()
	const sessionID = "session-auto-title-nonblock"
	path := autoTitleSeed(t, home, sessionID, "please fix the login redirect")
	gateway := newAutoTitleGateway(t, func(autoTitleGatewayRequest) (string, int, time.Duration) {
		return "", http.StatusOK, time.Hour // 只由客户端取消来结束
	})
	journals := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, journals); err != nil {
		t.Fatal(err)
	}
	var pubMu sync.Mutex
	var published []transport.ServerFrame
	runner := autoTitleRunner(t, home, gateway.base(), journals, func(_ string, f transport.ServerFrame) {
		pubMu.Lock()
		published = append(published, f)
		pubMu.Unlock()
	})
	start := time.Now()
	if err := runner.Run(context.Background(), sessionID, "turn-1", "please fix the login redirect"); err != nil {
		t.Fatalf("主 turn 收口失败: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("标题调用挡住了主 turn：Run 耗时 %s", elapsed)
	}
	pubMu.Lock()
	turnDone := false
	for _, frame := range published {
		if frame.Type == TurnDone {
			turnDone = true
		}
	}
	pubMu.Unlock()
	if !turnDone {
		t.Fatalf("主 turn 的 turn/done 没有推出去：%+v", published)
	}
	// 放开挂住的标题调用：连接被关掉后它只会失败，且不写任何东西。
	gateway.server.CloseClientConnections()
	if got := autoTitleTitleEvents(t, path); len(got) != 0 {
		t.Fatalf("挂住超时的标题调用仍写了事件：%+v", got)
	}
}

// TestAutoTitleConcurrentSessionsKeepOwnTitles：A/B 两个会话首轮同时收口，
// 标题各写各的，绝不互相覆盖，也不串输入。
func TestAutoTitleConcurrentSessionsKeepOwnTitles(t *testing.T) {
	home := t.TempDir()
	a, b := "session-auto-title-a", "session-auto-title-b"
	pathA := autoTitleSeed(t, home, a, "alpha: fix the login redirect")
	pathB := autoTitleSeed(t, home, b, "beta: rename the sidebar")
	gateway := newAutoTitleGateway(t, func(req autoTitleGatewayRequest) (string, int, time.Duration) {
		time.Sleep(30 * time.Millisecond) // 强制两个会话的标题调用重叠
		input := req.text(1)
		switch {
		case strings.Contains(input, "alpha"):
			return "Alpha Login Redirect", http.StatusOK, 0
		case strings.Contains(input, "beta"):
			return "Beta Sidebar Rename", http.StatusOK, 0
		default:
			return "", http.StatusBadRequest, 0
		}
	})
	journals := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, journals); err != nil {
		t.Fatal(err)
	}
	runner := autoTitleRunner(t, home, gateway.base(), journals, nil)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, item := range []struct{ id, prompt string }{
		{a, "alpha: fix the login redirect"},
		{b, "beta: rename the sidebar"},
	} {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runner.Run(context.Background(), item.id, "turn-"+item.id, item.prompt); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发首轮失败: %v", err)
	}
	if title := waitAutoTitle(t, home, a, pathA, 5*time.Second); title != "Alpha Login Redirect" {
		t.Fatalf("A 标题=%q", title)
	}
	if title := waitAutoTitle(t, home, b, pathB, 5*time.Second); title != "Beta Sidebar Rename" {
		t.Fatalf("B 标题=%q", title)
	}
	for _, item := range []struct{ id, path, want string }{
		{a, pathA, "Alpha Login Redirect"},
		{b, pathB, "Beta Sidebar Rename"},
	} {
		events := autoTitleTitleEvents(t, item.path)
		if len(events) != 1 {
			t.Fatalf("%s 的 session/title 事件数=%d，要 1", item.id, len(events))
		}
		var data struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(events[0].Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Title != item.want {
			t.Fatalf("%s 写进了别人的标题：%q", item.id, data.Title)
		}
		if row := autoTitleListRow(t, home, item.id); row.Title != item.want || row.TitleSource != "auto" {
			t.Fatalf("%s 的 list 行=%+v", item.id, row)
		}
	}
	if got := gateway.count(); got != 2 {
		t.Fatalf("标题调用次数=%d，要 2（每个会话一次）", got)
	}
}

// TestAutoTitleSameSessionConcurrentRunsWriteOnce：同一会话被并发触发两次（例如
// 一轮收口 + 一轮 steer 收口挨得太近），只允许一次调用、一条标题事件。
func TestAutoTitleSameSessionConcurrentRunsWriteOnce(t *testing.T) {
	home := t.TempDir()
	const sessionID = "session-auto-title-same"
	path := autoTitleSeed(t, home, sessionID, "please fix the login redirect")
	gateway := newAutoTitleGateway(t, func(autoTitleGatewayRequest) (string, int, time.Duration) {
		return "Only One Title", http.StatusOK, 40 * time.Millisecond
	})
	service, base := autoTitleDirect(home, gateway.base(), 5*time.Second)
	var wg sync.WaitGroup
	titles := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			title, err := service.Run(context.Background(), sessionID, "auto-title-model", base, "")
			if err != nil {
				errs <- err
			}
			titles <- title
		}()
	}
	wg.Wait()
	close(errs)
	close(titles)
	for err := range errs {
		t.Fatalf("并发同会话不该报错: %v", err)
	}
	won := 0
	for title := range titles {
		if title == "Only One Title" {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("并发同会话产生 %d 个成功标题，要恰好 1 个", won)
	}
	if events := autoTitleTitleEvents(t, path); len(events) != 1 {
		t.Fatalf("session/title 事件数=%d，要 1", len(events))
	}
	if got := gateway.count(); got != 1 {
		t.Fatalf("标题调用次数=%d，要 1", got)
	}
}

// ---------- 官方 prompt / 规范化 ----------

func TestAutoTitlePromptMatchesOfficialShape(t *testing.T) {
	home := t.TempDir()
	const sessionID = "session-auto-title-prompt"
	autoTitleSeed(t, home, sessionID, "please fix the login redirect")
	gateway := newAutoTitleGateway(t, nil)
	service, base := autoTitleDirect(home, gateway.base(), 3*time.Second)
	if _, err := service.Run(context.Background(), sessionID, "auto-title-model", base, ""); err != nil {
		t.Fatal(err)
	}
	req := gateway.request(0)
	system := req.text(0)
	for _, want := range []string{
		"Create a concise title for an AI coding-assistant session from the supplied human messages.",
		"Return only the title on one line",
		"Use the language of the messages.",
		"Aim for about 5 words in non-CJK languages or 10 CJK characters.",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt 缺官方原文片段 %q：%q", want, system)
		}
	}
	input := req.text(1)
	if !strings.HasPrefix(input, "Generate the session title from this JSON array of human messages:\n[") {
		t.Fatalf("输入帧不是官方 JSON 数组形状：%q", input)
	}
	var messages []struct {
		Seq  int64  `json:"seq"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(input, "\n", 2)[1]), &messages); err != nil {
		t.Fatalf("输入帧不是合法 JSON：%v (%q)", err, input)
	}
	if len(messages) != 1 || messages[0].Seq != 2 || messages[0].Text != "please fix the login redirect" {
		t.Fatalf("输入帧内容=%+v，要首条用户消息（seq=2）", messages)
	}
}

func TestAutoTitleNormalizeAndBounds(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		bytes int
		want  string
	}{
		{name: "newlines-and-tabs", in: "\n 多行\n标题\t  ", bytes: autoTitleMaxBytes, want: "多行 标题"},
		{name: "sgr-color", in: "\x1b[31mRed\x1b[0m title", bytes: autoTitleMaxBytes, want: "Red title"},
		{name: "osc-title", in: "\x1b]0;fake\x07real title", bytes: autoTitleMaxBytes, want: "real title"},
		{name: "bidi-override", in: "\u202eabc\u202c def", bytes: autoTitleMaxBytes, want: "abc def"},
		{name: "zero-width", in: "a\u200bb\ufeffc", bytes: autoTitleMaxBytes, want: "abc"},
		{name: "cjk-byte-bound", in: strings.Repeat("中", 100), bytes: 120, want: strings.Repeat("中", 40)},
		{name: "ascii-byte-bound", in: "abcdef", bytes: 3, want: "abc"},
		{name: "empty-after-clean", in: " \n\t\x1b[0m ", bytes: autoTitleMaxBytes, want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAutoTitle(tc.in, tc.bytes)
			if got != tc.want {
				t.Fatalf("normalizeAutoTitle(%q,%d)=%q，要 %q", tc.in, tc.bytes, got, tc.want)
			}
			if len(got) > tc.bytes {
				t.Fatalf("结果 %d 字节超过上限 %d", len(got), tc.bytes)
			}
		})
	}
	// 截断不拆码点：每个结果都必须是合法 UTF-8。
	for _, maxBytes := range []int{1, 2, 3, 4, 5} {
		got := normalizeAutoTitle("汉字标题", maxBytes)
		if !utf8.ValidString(got) {
			t.Fatalf("maxBytes=%d 截出了半个码点: %q", maxBytes, got)
		}
	}
}

func TestAutoTitleFromOutputMatchesOfficial(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{name: "first-non-empty-line", in: "\n\nFix the login redirect\ncommentary", want: "Fix the login redirect"},
		{name: "strong-wrap", in: "**Fix the login redirect**\nexplanation", want: "Fix the login redirect"},
		{name: "single-star-wrap", in: "*Fix it*", want: "Fix it"},
		{name: "triple-star-wrap", in: "***Fix it***", want: "Fix it"},
		{name: "inner-marker-kept", in: "**a**b**", want: "**a**b**"},
		{name: "padded-wrap-kept", in: "** Fix it **", want: "** Fix it **"},
		{name: "empty", in: "  \n\t\n", want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := autoTitleFromOutput(tc.in); got != tc.want {
				t.Fatalf("autoTitleFromOutput(%q)=%q，要 %q", tc.in, got, tc.want)
			}
		})
	}
}
