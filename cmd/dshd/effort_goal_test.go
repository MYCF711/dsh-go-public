package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// W144 先红探针只使用线上 JSON，不引用待新增的 Go 类型：旧宿主必须在
// setEffort / goal.get 处真实失败，而不是靠“测试编译不过”冒充失败分支。
func TestEffortGoalRealDSHDSetEffortCatalogLoadABRestartAndRunner(t *testing.T) {
	seen := make(chan struct {
		Model     string `json:"model"`
		Reasoning string `json:"reasoning_effort"`
	}, 8)
	gateway := newEffortGoalGateway(t, seen)
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "1")
	t.Setenv("DSHD_LLM_BASE", gateway)
	t.Setenv("DSHD_LLM_KEY", "")

	exe := buildPluginGlueDSHD(t)
	home := t.TempDir()
	decoy := t.TempDir()
	server := startPluginGlueServer(t, exe, home, decoy)
	client := connectPluginGlueServer(t, server)
	postPluginGlue(t, client, server.base, "models.catalog.upsert", map[string]any{
		"model": map[string]any{
			"id":               "effort-model",
			"reasoningEfforts": map[string]any{"off": nil, "low": "low", "deep": "wire-deep"},
		},
		"expectedRevision": 0,
	}, http.StatusOK)
	postPluginGlue(t, client, server.base, "models.catalog.set-active", map[string]any{
		"id": "effort-model", "expectedRevision": 0,
	}, http.StatusOK)
	sessionA := effortGoalCreateSession(t, client, server.base)
	sessionB := effortGoalCreateSession(t, client, server.base)

	postPluginGlue(t, client, server.base, "session.setEffort", map[string]any{
		"id": sessionA, "effort": "deep",
	}, http.StatusOK)
	assertEffortSelection(t, client, server.base, sessionA, "deep")
	postPluginGlue(t, client, server.base, "session.setEffort", map[string]any{
		"id": sessionA, "effort": "wire-deep", // wire value 不是 key，必须拒绝且不覆盖 deep。
	}, http.StatusBadRequest)
	postPluginGlue(t, client, server.base, "session.setEffort", map[string]any{
		"id": sessionA, "effort": nil,
	}, http.StatusBadRequest)
	assertEffortSelection(t, client, server.base, sessionA, "deep")
	postPluginGlue(t, client, server.base, "session.setEffort", map[string]any{
		"id": sessionB, "effort": "low",
	}, http.StatusOK)
	assertEffortSelection(t, client, server.base, sessionB, "low")

	effortGoalPromptAndAssert(t, client, server, sessionA, "prove deep", seen, "effort-model", "wire-deep")
	assertActualEffort(t, client, server.base, sessionA, "deep")
	effortGoalPromptAndAssert(t, client, server, sessionB, "prove B low", seen, "effort-model", "low")
	assertActualEffort(t, client, server.base, sessionB, "low")
	assertActualEffort(t, client, server.base, sessionA, "deep")
	server.stop()
	server = startPluginGlueServer(t, exe, home, decoy)
	client = connectPluginGlueServer(t, server)
	assertEffortSelection(t, client, server.base, sessionA, "deep")
	assertEffortSelection(t, client, server.base, sessionB, "low")
	effortGoalPromptAndAssert(t, client, server, sessionA, "prove restarted deep", seen, "effort-model", "wire-deep")
	postPluginGlue(t, client, server.base, "session.setEffort", map[string]any{
		"id": sessionA, "effort": "",
	}, http.StatusOK)
	assertEffortSelection(t, client, server.base, sessionA, "")
	assertEffortSelection(t, client, server.base, sessionB, "low")
	assertActualEffort(t, client, server.base, sessionA, "deep")
	effortGoalPromptAndAssert(t, client, server, sessionA, "prove cleared", seen, "effort-model", "")
	assertActualEffort(t, client, server.base, sessionA, "")
	assertActualEffort(t, client, server.base, sessionB, "low")
	t.Log("EFFORT_REAL_SUMMARY OK=1 FAIL=0")
}

func TestEffortGoalRealDSHDDefaultLoopRunnerOffAndClearEndToEnd(t *testing.T) {
	seen := make(chan struct {
		Model     string `json:"model"`
		Reasoning string `json:"reasoning_effort"`
	}, 4)
	gateway := newEffortGoalGateway(t, seen)
	t.Setenv("DSHD_SERVE_MODEL_RUNNER", "")
	t.Setenv("DSHD_REASONING_EFFORT", "max")
	t.Setenv("DSHD_LLM_BASE", gateway)
	t.Setenv("DSHD_LLM_KEY", "")
	exe := buildPluginGlueDSHD(t)
	server := startPluginGlueServer(t, exe, t.TempDir(), t.TempDir())
	client := connectPluginGlueServer(t, server)
	postPluginGlue(t, client, server.base, "models.catalog.upsert", map[string]any{
		"model": map[string]any{
			"id": "effort-model", "reasoningEfforts": map[string]any{"off": nil, "max": "max"},
		},
		"expectedRevision": 0,
	}, http.StatusOK)
	postPluginGlue(t, client, server.base, "models.catalog.set-active", map[string]any{
		"id": "effort-model", "expectedRevision": 0,
	}, http.StatusOK)
	sessionID := effortGoalCreateSession(t, client, server.base)
	postPluginGlue(t, client, server.base, "session.setEffort", map[string]any{
		"id": sessionID, "effort": "off",
	}, http.StatusOK)
	effortGoalPromptAndAssert(t, client, server, sessionID, "default loop off", seen, "effort-model", "")
	assertActualEffort(t, client, server.base, sessionID, "off")
	postPluginGlue(t, client, server.base, "session.setEffort", map[string]any{
		"id": sessionID, "effort": "",
	}, http.StatusOK)
	effortGoalPromptAndAssert(t, client, server, sessionID, "default loop cleared", seen, "effort-model", "max")
	assertActualEffort(t, client, server.base, sessionID, "max")
	t.Log("DEFAULT_LOOP_EFFORT_REAL_SUMMARY OK=1 FAIL=0")
}

func TestEffortGoalRealDSHDPersistenceFailuresHaveFourFieldErrors(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	home := t.TempDir()
	state := home + string(os.PathSeparator) + "state"
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	// 文件路径被目录占据：Load 真实失败且 sticky，后续 API 不得回 ok:true。
	if err := os.Mkdir(state+string(os.PathSeparator)+"session-effort.json", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state+string(os.PathSeparator)+"session-goal.json", 0o755); err != nil {
		t.Fatal(err)
	}
	server := startPluginGlueServer(t, exe, home, t.TempDir())
	client := connectPluginGlueServer(t, server)
	sessionID := effortGoalCreateSession(t, client, server.base)
	for _, probe := range []struct {
		method string
		params map[string]any
	}{
		{"session.setEffort", map[string]any{"id": sessionID, "effort": ""}},
		{"session.goal.get", map[string]any{"id": sessionID}},
		{"session.goal.pause", map[string]any{"id": sessionID}},
		{"session.setGoal", map[string]any{"id": sessionID, "goal": "must not commit"}},
	} {
		envelope := postPluginGlue(t, client, server.base, probe.method, probe.params, http.StatusInternalServerError)
		assertPluginGlueError(t, envelope, "internal")
	}
	t.Log("EFFORT_GOAL_PERSISTENCE_FAILURE_SUMMARY OK=4 FAIL=0")
}

func TestEffortGoalRealDSHDGoalSnapshotJournalRestartAndReconnect(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	home := t.TempDir()
	decoy := t.TempDir()
	server := startPluginGlueServer(t, exe, home, decoy)
	client := connectPluginGlueServer(t, server)
	sessionA := effortGoalCreateSession(t, client, server.base)
	sessionB := effortGoalCreateSession(t, client, server.base)

	assertGoalSnapshot(t, client, server.base, sessionA, nil, 0, -1)
	postPluginGlue(t, client, server.base, "session.goal.pause", map[string]any{"id": sessionA}, http.StatusBadRequest)
	postPluginGlue(t, client, server.base, "session.setGoal", map[string]any{
		"id": sessionA, "goal": "  finish the adapter  ",
	}, http.StatusOK)
	created := assertGoalSnapshot(t, client, server.base, sessionA, nil, 0, 1)
	goalID := created["id"].(string)
	if goalID == "" || created["objective"] != "finish the adapter" || created["phase"] != "active" || created["maxGoalRounds"] != float64(0) {
		t.Fatalf("create snapshot=%+v", created)
	}
	assertGoalSnapshot(t, client, server.base, sessionB, nil, 0, -1)

	postPluginGlue(t, client, server.base, "session.goal.pause", map[string]any{"id": sessionA}, http.StatusOK)
	paused := assertGoalSnapshot(t, client, server.base, sessionA, nil, 0, 2)
	if paused["id"] != goalID || paused["phase"] != "paused" {
		t.Fatalf("pause snapshot=%+v", paused)
	}
	postPluginGlue(t, client, server.base, "session.goal.pause", map[string]any{"id": sessionA}, http.StatusBadRequest)
	postPluginGlue(t, client, server.base, "session.goal.resume", map[string]any{"id": sessionA}, http.StatusOK)
	assertGoalSnapshot(t, client, server.base, sessionA, nil, 0, 3)
	postPluginGlue(t, client, server.base, "session.goal.resume", map[string]any{"id": sessionA}, http.StatusBadRequest)
	postPluginGlue(t, client, server.base, "session.setGoal", map[string]any{
		"id": sessionA, "goal": "finish and verify the adapter",
	}, http.StatusOK)
	edited := assertGoalSnapshot(t, client, server.base, sessionA, nil, 0, 4)
	if edited["id"] != goalID || edited["objective"] != "finish and verify the adapter" {
		t.Fatalf("edit snapshot=%+v", edited)
	}
	assertGoalOperations(t, client, server.base, sessionA, []string{"create", "pause", "resume", "edit"})
	assertGoalOperations(t, client, server.base, sessionB, []string{})

	server.stop()
	server = startPluginGlueServer(t, exe, home, decoy)
	client = connectPluginGlueServer(t, server)
	restarted := assertGoalSnapshot(t, client, server.base, sessionA, nil, 0, 4)
	if restarted["id"] != goalID {
		t.Fatalf("restart changed native goal id: before=%s after=%+v", goalID, restarted)
	}
	assertGoalOperations(t, client, server.base, sessionA, []string{"create", "pause", "resume", "edit"})
	// journalEpoch=0 是契约里的首次订阅；不能经 float64 转 random int64 epoch。
	ws := dialEffortGoalWS(t, server, client, sessionA, 2, 0)
	first := readEffortGoalWSFrame(t, ws)
	second := readEffortGoalWSFrame(t, ws)
	if first["seq"] != float64(3) || second["seq"] != float64(4) {
		t.Fatalf("reconnect replay frames=%+v %+v", first, second)
	}
	postPluginGlue(t, client, server.base, "session.setGoal", map[string]any{
		"id": sessionA, "goal": "",
	}, http.StatusOK)
	live := readEffortGoalWSFrame(t, ws)
	assertGoalChangeFrame(t, live, 5, "clear")
	_ = ws.Close()
	assertGoalSnapshot(t, client, server.base, sessionA, nil, 0, -1)
	postPluginGlue(t, client, server.base, "session.setGoal", map[string]any{
		"id": sessionA, "goal": "", // 既有 clear 幂等：第二次成功且不得新增 change。
	}, http.StatusOK)

	ws = dialEffortGoalWS(t, server, client, sessionA, 4, 0)
	replayed := readEffortGoalWSFrame(t, ws)
	assertGoalChangeFrame(t, replayed, 5, "clear")
	_ = ws.Close()
	assertGoalOperations(t, client, server.base, sessionA, []string{"create", "pause", "resume", "edit", "clear"})
	assertGoalOperations(t, client, server.base, sessionB, []string{})
	t.Log("GOAL_REAL_SUMMARY OK=1 FAIL=0")
}

func newEffortGoalGateway(t *testing.T, seen chan<- struct {
	Model     string `json:"model"`
	Reasoning string `json:"reasoning_effort"`
}) string {
	t.Helper()
	return startTestSSEGateway(t, func(request struct {
		Model     string `json:"model"`
		Reasoning string `json:"reasoning_effort"`
	}) {
		seen <- request
	})
}

// startTestSSEGateway 只返回本地、确定性的最小 SSE；断言的仍是 runner 真发出的 JSON。
func startTestSSEGateway(t *testing.T, onRequest func(struct {
	Model     string `json:"model"`
	Reasoning string `json:"reasoning_effort"`
})) string {
	t.Helper()
	server := &http.Server{Addr: "127.0.0.1:0"}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var request struct {
			Model     string                 `json:"model"`
			Reasoning string                 `json:"reasoning_effort"`
			Messages  []autoTitleWireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// 自动标题（官方 #14）是首轮收口后的额外真实模型调用，不带主 turn 的
		// reasoning 覆盖；本探针断言主 turn 的 effort wire，必须把它排除在计数之外。
		if !isAutoTitleWireRequest(request.Messages) {
			onRequest(struct {
				Model     string `json:"model"`
				Reasoning string `json:"reasoning_effort"`
			}{Model: request.Model, Reasoning: request.Reasoning})
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String() + "/v1"
}

func effortGoalCreateSession(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	envelope := postPluginGlue(t, client, base, "session.create", map[string]any{}, http.StatusOK)
	return envelope["result"].(map[string]any)["sessionId"].(string)
}

func assertEffortSelection(t *testing.T, client *http.Client, base, sessionID, want string) {
	t.Helper()
	envelope := postPluginGlue(t, client, base, "session.load", map[string]any{
		"id": sessionID, "tailMessages": 1,
	}, http.StatusOK)
	result := envelope["result"].(map[string]any)
	selection, exists := result["effortSelection"]
	if want == "" {
		if exists {
			t.Fatalf("session=%s effortSelection 应省略，实际 %+v", sessionID, result)
		}
		return
	}
	if !exists || selection != want {
		t.Fatalf("session=%s effortSelection=%v exists=%v want=%q；load=%+v", sessionID, selection, exists, want, result)
	}
}

func assertActualEffort(t *testing.T, client *http.Client, base, sessionID, want string) {
	t.Helper()
	envelope := postPluginGlue(t, client, base, "session.load", map[string]any{
		"id": sessionID, "tailMessages": 1,
	}, http.StatusOK)
	result := envelope["result"].(map[string]any)
	effort, exists := result["effort"]
	if want == "" {
		if exists {
			t.Fatalf("session=%s 实际 effort 应未知/省略，实际 %+v", sessionID, result)
		}
		return
	}
	if !exists || effort != want {
		t.Fatalf("session=%s 实际 effort=%v exists=%v want=%q；load=%+v", sessionID, effort, exists, want, result)
	}
}

func effortGoalPromptAndAssert(t *testing.T, client *http.Client, server *pluginGlueServer, sessionID, prompt string,
	seen <-chan struct {
		Model     string `json:"model"`
		Reasoning string `json:"reasoning_effort"`
	}, wantModel, wantReasoning string) {
	t.Helper()
	accepted := postEffortGoalPromptWhenReady(t, client, server.base, sessionID, prompt)
	turnID := accepted["result"].(map[string]any)["turnId"].(string)
	select {
	case request := <-seen:
		if request.Model != wantModel || request.Reasoning != wantReasoning {
			t.Fatalf("runner request=%+v want model=%q reasoning=%q", request, wantModel, wantReasoning)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("runner request timeout session=%s stderr=%s", sessionID, server.stderr.String())
	}
	waitEffortGoalTurn(t, client, server.base, sessionID, turnID)
}

func postEffortGoalPromptWhenReady(t *testing.T, client *http.Client, base, sessionID, prompt string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"id": sessionID, "text": prompt})
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := client.Post(base+"/api/session.prompt", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			var envelope map[string]any
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatal(err)
			}
			return envelope
		}
		// turn/done 先入 journal，promptService 随后才释放 busy；只等这条真实、
		// 可重试的交接，不重试其它错误，也不接受 409 冒充 prompt 成功。
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		if resp.StatusCode != http.StatusConflict || failure.Error.Code != "busy" || time.Now().After(deadline) {
			t.Fatalf("session.prompt status=%d body=%s", resp.StatusCode, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitEffortGoalTurn(t *testing.T, client *http.Client, base, sessionID, turnID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		envelope := postPluginGlue(t, client, base, "session.history", map[string]any{
			"id": sessionID, "sinceSeq": 0, "limit": 1000,
		}, http.StatusOK)
		frames := envelope["result"].(map[string]any)["frames"].([]any)
		for _, raw := range frames {
			frame := raw.(map[string]any)
			if frame["type"] != "turn/done" && frame["type"] != "turn/error" {
				continue
			}
			data := frame["data"].(map[string]any)
			if data["turnId"] != turnID {
				continue
			}
			if frame["type"] == "turn/error" {
				t.Fatalf("turn %s failed: %+v", turnID, frame)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s completion timeout; frames=%+v", turnID, frames)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// assertGoalSnapshot 用 revision=-1 表示必须没有 goal；有 goal 时同时钉住 roundsStarted=0。
func assertGoalSnapshot(t *testing.T, client *http.Client, base, sessionID string, _ any, _ int, revision int) map[string]any {
	t.Helper()
	envelope := postPluginGlue(t, client, base, "session.goal.get", map[string]any{"id": sessionID}, http.StatusOK)
	result := envelope["result"].(map[string]any)
	rawGoal, exists := result["goal"]
	if !exists {
		t.Fatalf("goal.get 缺 goal 键：%+v", result)
	}
	if revision < 0 {
		if rawGoal != nil {
			t.Fatalf("session=%s 应无 goal，实际 %+v", sessionID, result)
		}
		return nil
	}
	goal, ok := rawGoal.(map[string]any)
	if !ok {
		t.Fatalf("goal 不是 snapshot：%+v", result)
	}
	if goal["revision"] != float64(revision) || result["roundsStarted"] != float64(0) {
		t.Fatalf("goal revision/roundsStarted=%+v result=%+v", goal, result)
	}
	if _, hasBudget := goal["budget"]; hasBudget {
		t.Fatalf("宿主未计量 budget，不得补零：%+v", goal)
	}
	return goal
}

func assertGoalOperations(t *testing.T, client *http.Client, base, sessionID string, want []string) {
	t.Helper()
	envelope := postPluginGlue(t, client, base, "session.history", map[string]any{
		"id": sessionID, "sinceSeq": 0, "limit": 100,
	}, http.StatusOK)
	frames := envelope["result"].(map[string]any)["frames"].([]any)
	if len(frames) != len(want) {
		t.Fatalf("session=%s history frames=%d want=%d：%+v", sessionID, len(frames), len(want), frames)
	}
	for i, operation := range want {
		assertGoalChangeFrame(t, frames[i].(map[string]any), i+1, operation)
	}
}

func assertGoalChangeFrame(t *testing.T, frame map[string]any, seq int, operation string) {
	t.Helper()
	if frame["type"] != "goal/change" || frame["seq"] != float64(seq) || frame["time"] == nil {
		t.Fatalf("goal frame envelope=%+v want seq=%d", frame, seq)
	}
	data := frame["data"].(map[string]any)
	if data["kind"] != "goal/change" || data["version"] != float64(1) || data["operation"] != operation {
		t.Fatalf("goal frame data=%+v want operation=%s", data, operation)
	}
	if operation == "round" {
		t.Fatalf("普通 set/clear/pause/resume 不得伪造 round：%+v", frame)
	}
	if operation == "clear" {
		if goal, exists := data["goal"]; exists && goal != nil {
			t.Fatalf("clear 不得携带 snapshot：%+v", data)
		}
		return
	}
	goal, ok := data["goal"].(map[string]any)
	if !ok || goal["id"] == "" || goal["revision"] == nil || goal["objective"] == "" || goal["phase"] == "" || goal["maxGoalRounds"] == nil {
		t.Fatalf("%s 必须带完整 snapshot：%+v", operation, data)
	}
}

type effortGoalWS struct {
	conn net.Conn
	br   *bufio.Reader
}

func (w *effortGoalWS) Close() error { return w.conn.Close() }

func dialEffortGoalWS(t *testing.T, server *pluginGlueServer, client *http.Client, sessionID string, since int, epoch int64) *effortGoalWS {
	t.Helper()
	u, err := url.Parse(server.base)
	if err != nil {
		t.Fatal(err)
	}
	cookies := client.Jar.Cookies(u)
	if len(cookies) == 0 {
		t.Fatal("authenticated client has no cookie")
	}
	conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nCookie: %s=%s\r\n\r\n",
		u.Host, key, cookies[0].Name, cookies[0].Value)
	br := bufio.NewReader(conn)
	response, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	// 101 的 Body 拥有底层连接；Close 会把刚升级的 WS 一起关掉。
	if response.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		t.Fatalf("WS status=%d", response.StatusCode)
	}
	want := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(want[:]) {
		conn.Close()
		t.Fatal("bad Sec-WebSocket-Accept")
	}
	payload, _ := json.Marshal(map[string]any{
		"t": "sub", "topic": "session:" + sessionID, "sinceSeq": since, "journalEpoch": epoch,
	})
	if err := writeEffortGoalClientFrame(conn, payload); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return &effortGoalWS{conn: conn, br: br}
}

func writeEffortGoalClientFrame(conn net.Conn, payload []byte) error {
	if len(payload) >= 126 {
		return fmt.Errorf("test client payload too large: %d", len(payload))
	}
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81, byte(0x80 | len(payload))}
	frame = append(frame, mask[:]...)
	for i, value := range payload {
		frame = append(frame, value^mask[i%4])
	}
	_, err := conn.Write(frame)
	return err
}

func readEffortGoalWSFrame(t *testing.T, ws *effortGoalWS) map[string]any {
	t.Helper()
	if err := ws.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	first, err := ws.br.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	second, err := ws.br.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	if first != 0x81 || second&0x80 != 0 {
		t.Fatalf("unexpected WS frame header: %#x %#x", first, second)
	}
	length := uint64(second & 0x7f)
	switch length {
	case 126:
		var value uint16
		if err := binary.Read(ws.br, binary.BigEndian, &value); err != nil {
			t.Fatal(err)
		}
		length = uint64(value)
	case 127:
		if err := binary.Read(ws.br, binary.BigEndian, &length); err != nil {
			t.Fatal(err)
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(ws.br, payload); err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&frame); err != nil {
		t.Fatalf("WS payload=%q err=%v", payload, err)
	}
	return frame
}
