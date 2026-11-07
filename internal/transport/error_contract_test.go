package transport

// H8：**整个 HTTP 出口只有一个错误体构造**，而且每条错误路径都交出
// `{error:{code,message,hint,retryable}}` 四字段。
//
// 为什么这条测试是"扫全仓"的，而不是逐个 handler 补一条：
// 2026-11-03 的界面审计实测发现 `stale_epoch` 那个 409 是**手写** JSON、
// 少 `retryable`——而当时 `TestWriteJSONEncodeFailureHasFourFields` 之类的
// 单点测试全是绿的。单点测试只证明"我测的那一条对"，证明不了"没有第二条
// 出口漏了字段"。这里改成从外面把所有出口打一遍：
//
//  1. 三台服务器覆盖三类分支：一个回调都没接（501 档）、每个回调都失败
//     （4xx/5xx 档）、带 journal（`stale_epoch` 409 档）；
//  2. 每个 method 用空 body / 坏 JSON / 带指向不存在的 id 的 body 各打一次；
//  3. 不走 /api 的出口也要打：未知路径、错动词、`/connect` 错 token、
//     `/ws` 没 cookie、`/ws` 有 cookie 但握手头不全、`/debug/stats` 没 cookie。
//
// 每条 4xx/5xx 响应统一断言：Content-Type 是 JSON、`error` 是对象、
// **恰好**四个键、四个值都非空、`retryable` 是布尔且取值与本文件里那张
// 独立的语义表一致。`retryable` 的判据在实现里只有一处；这张表是它的
// **另一份独立说法**——实现改了判据，这里会红，而不是跟着一起改。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// errorEnvelopeKeys 是错误对象的**全部**键。多一个少一个都算契约漂移。
var errorEnvelopeKeys = []string{"code", "message", "hint", "retryable"}

// retryableSemantics 是"这条错误该不该重试"的语义表。
//
// 判断依据是**客户端的下一步**，不是 HTTP 状态码：
//   - 改请求才能过（参数错、鉴权、未接线、找不到东西）→ 不可重试
//   - 等一等/换个基线再来就过（宿主忙、水位过期、宿主自己出错）→ 可重试
//
// 这张表故意不 import 实现里的判断函数：两边独立，才有对照价值。
var retryableSemantics = map[string]bool{
	// ---- 改请求/改客户端才能过 ----
	"unauthorized":      false, // 先 /connect 换 cookie
	"not_found":         false, // 路径、方法或目标写错了
	"not_implemented":   false, // 宿主没接这条线，重试一万次也一样
	"bad_request":       false, // 参数错：必须改参数
	"revision_conflict": false, // 先读最新 revision，不能盲重试覆盖
	"job_finished":      false, // 已结束不能重复停止
	"cross_session":     false, // 不能越 home/会话操作
	"bad_handshake":     false, // 握手头不全：改客户端
	"no_such_session":   false, // 会话不在：先 session.list
	"no_such_request":   false, // 审批已应答/已超时
	"bad_option":        false, // optionId 写错了
	"encode_failure":    false, // 宿主序列化出 bug，重试还是同一个 bug
	"empty_result":      false, // 宿主没给载荷（宿主缺陷）
	"error":             false, // 叫不出名字的宿主错误：不自动重试
	// ---- 等一等/换基线再来就过 ----
	"internal":    true, // 宿主自己出错：同样的请求再发一次可能就好了
	"busy":        true, // 上一轮还没结束：等 turn/done 再发
	"stale_epoch": true, // 水位/世代过期：丢旧水位重新 load
}

// scanErrorResponse 检查一条响应；是错误响应就逐项断言，返回它的 code。
// 不是错误响应（<400）时返回空串——成功路径不归这条测试管。
func scanErrorResponse(t *testing.T, where string, status int, hdr http.Header, body []byte) string {
	t.Helper()
	if status < 400 {
		return ""
	}

	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s：错误响应 Content-Type=%q，不是 JSON（纯文本正文客户端连解析都进不去）：%s",
			where, ct, body)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Errorf("%s：错误响应不是 JSON：%v（正文 %q）", where, err, body)
		return ""
	}
	raw, ok := top["error"]
	if !ok {
		t.Errorf("%s：顶层没有 error 键：%s", where, body)
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Errorf("%s：error 不是对象：%s", where, body)
		return ""
	}

	// 恰好四个键：多出来的键同样会让客户端特判（而且没人给它写测试）。
	if len(obj) != len(errorEnvelopeKeys) {
		t.Errorf("%s：error 里应当恰好 %d 个键，实际 %d 个：%s",
			where, len(errorEnvelopeKeys), len(obj), body)
	}

	var code, message, hint string
	for _, k := range errorEnvelopeKeys {
		v, ok := obj[k]
		if !ok {
			t.Errorf("%s：error 缺 %q（四字段之一）：%s", where, k, body)
			continue
		}
		switch k {
		case "code":
			_ = json.Unmarshal(v, &code)
		case "message":
			_ = json.Unmarshal(v, &message)
		case "hint":
			_ = json.Unmarshal(v, &hint)
		}
	}
	// retryable 必须**真的是布尔**：字符串 "true"、数字 1、缺省都算错。
	var retryable bool
	if v, ok := obj["retryable"]; ok {
		if err := json.Unmarshal(v, &retryable); err != nil {
			t.Errorf("%s：retryable 不是布尔（%s）：%s", where, v, body)
		}
	}

	if code == "" {
		t.Errorf("%s：code 为空——客户端没法按码分支：%s", where, body)
	}
	if message == "" {
		t.Errorf("%s：message 为空——界面只能显示一个空错误：%s", where, body)
	}
	if hint == "" {
		t.Errorf("%s：hint 为空——契约要求每个错误都说下一步怎么做：%s", where, body)
	}

	want, known := retryableSemantics[code]
	if !known {
		t.Errorf("%s：code=%q 不在 retryableSemantics 里——加新码必须同时说清它能不能重试",
			where, code)
	} else if retryable != want {
		t.Errorf("%s：code=%q 的 retryable=%v，语义要求 %v：%s", where, code, retryable, want, body)
	}
	return code
}

// failingOptions 让每个回调都失败：这样每个 method 的 4xx/5xx 分支都会被走到。
//
// 失败消息刻意分成三类：能映射成稳定码的（"没有这个会话"/"上一轮还没结束"）、
// 认得出来的（"…失败：…"）、以及**映射不出来的**（随便一句宿主原文）——
// 第三类必须落到兜底码，而兜底码同样要四字段、有 hint、不可重试。
func failingOptions(j *Journals) Options {
	hostErr := errors.New("磁盘只读，这一下没写成")
	return Options{
		Journals: j,
		List: func() ([]SessionSummary, error) {
			return nil, hostErr
		},
		Load: func(id string, limit int) (LoadResult, error) {
			if id == "boom" {
				return LoadResult{}, hostErr
			}
			return LoadResult{}, errors.New("没有这个会话")
		},
		History: func(id string, sinceSeq, limit int) (Page, error) {
			return Page{}, errors.New("没有这个会话")
		},
		Compact: func(id string, keep int) (CompactResult, error) {
			return CompactResult{}, hostErr
		},
		Prompt: func(id, text string) (PromptResult, error) {
			if id == "boom" {
				return PromptResult{}, hostErr
			}
			return PromptResult{}, errors.New("上一轮还没结束")
		},
		Delete: func(id string) error {
			if id == "boom" {
				return hostErr
			}
			return ErrNoSuchSession
		},
		Fork: func(id, title string) (string, error) {
			if id == "boom" {
				return "", hostErr
			}
			return "", ErrNoSuchSession
		},
		WorkspaceList: func(path string) ([]WorkspaceEntry, error) {
			return nil, hostErr
		},
		JobsList: func() []JobInfo { return nil },
		JobsOutput: func(id string, wait time.Duration) (JobOutput, error) {
			return JobOutput{}, errors.New("no such job: " + id)
		},
		ScheduleList: func() []ScheduleItem { return nil },
		ScheduleDelete: func(sessionID, id, expectedRevision string) (ScheduleDeleteResult, error) {
			return ScheduleDeleteResult{}, ErrNoSuchSchedule
		},
		ScheduleClear: func() (ScheduleClearResult, error) {
			return ScheduleClearResult{}, hostErr
		},
		JobsStop: func(sessionID, id string) (JobStopResult, error) {
			return JobStopResult{}, ErrNoSuchJob
		},
		SetSessionModel: func(id, model string) error {
			return errors.New("模型 id 不认识")
		},
		SetSessionPermission: func(id, perm string) error {
			return errors.New("permission 只能是 deny/ask/allow/auto")
		},
		ModelsList: func(ctx context.Context) ([]ModelEntry, error) {
			return nil, hostErr
		},
		SetSessionGoal: func(id, goal string) error {
			return errors.New("goal 写盘失败")
		},
		TerminalOpen: func(cwd string) (string, error) {
			return "", hostErr
		},
		TerminalInput: func(id, data string) error {
			return errors.New("这个终端已经关了")
		},
		TerminalOutput: func(id string, since int) (TerminalOutput, error) {
			return TerminalOutput{}, errors.New("这个终端已经关了")
		},
		FSRead: func(path string) (FSContent, error) {
			if path == "/" {
				return FSContent{}, hostErr
			}
			return FSContent{}, ErrNoSuchFile
		},
		FSWrite: func(path, content string) error {
			if path == "/" {
				return hostErr
			}
			return ErrNoSuchFile
		},
		ConfigGet: func() (map[string]string, error) {
			return nil, hostErr
		},
		ConfigSet: func(kv map[string]string) error {
			return hostErr
		},
		Cancel: func(id string) error {
			return errors.New("没有在飞的一轮")
		},
		Approve: func(requestID, optionID string) error {
			switch requestID {
			case "bad-option":
				return ErrBadOption
			case "boom":
				return hostErr
			}
			return ErrNoSuchRequest
		},
		Create: func(mode string) (string, error) {
			return "", errors.New("mode 不认识")
		},
	}
}

// apiProbe 是打一次 /api 请求的描述。
type apiProbe struct {
	name   string
	method string // HTTP 方法；空 = POST
	path   string
	body   string
}

// badApiBodies 是每个 method 都要吃的三种坏 body。
//
// 为什么三种：只有 `{}` 的话，凡是"解码失败/缺字段"先返回的 handler
// 都在第一道门就把请求挡了，后面真正调用宿主的那一半永远走不到。
var badApiBodies = []struct {
	name string
	body string
}{
	{"空 body", `{}`},
	{"坏 JSON", `{`},
	// 这一份把所有 method 的参数名都填上，但值指向不存在的东西：
	// 于是分支从"缺参数"推进到"宿主说没有/宿主失败"。
	{"参数齐全但不存在", `{"id":"boom","sessionId":"boom","requestId":"boom",` +
		`"optionId":"boom","path":"/nope.txt","content":"x","cwd":"",` +
		`"text":"","sinceSeq":5,"limit":1,"journalEpoch":999,` +
		`"values":{"a":"b"},"mode":"nope","goal":"g","model":"m","perm":"ask"}`},
}

// TestEveryHTTPErrorHasFourFieldEnvelope 是扫全仓的那条测试。
func TestEveryHTTPErrorHasFourFieldEnvelope(t *testing.T) {
	js := NewJournals(func() int64 { return 7 })
	js.Get("scan-session") // 先建出这个会话的 journal（epoch=7）

	servers := []struct {
		name string
		opts Options
	}{
		{"未接线（501 档）", Options{}},
		{"宿主全失败（4xx/5xx 档）", failingOptions(nil)},
		{"带 journal（stale_epoch 档）", failingOptions(js)},
	}

	// 每个 method 至少要被"造出过一次错误"，否则这条扫描对它形同虚设。
	sawError := map[Method]bool{}
	// 扫了多少、断言了多少：只在最后打一行。这两个数一起掉，说明扫描退化了
	// （比如某个回调不再失败），而"没有断言到东西"的测试和没写是一样的。
	requests, errs := 0, 0

	// 专项探针：上面三种 body 覆盖不到的几条分支。
	extraProbes := []apiProbe{
		{"approve 的 optionId 不认识", "", "/api/session.approve", `{"requestId":"bad-option","optionId":"yolo"}`},
		{"history 水位比当前还大", "", "/api/session.history", `{"sessionId":"scan-session","sinceSeq":99}`},
		{"fs.read 文件不存在", "", "/api/fs.read", `{"path":"nope.txt"}`},
		{"fs.write 文件不存在", "", "/api/fs.write", `{"path":"nope.txt","content":""}`},
		{"jobs.output 任务不存在", "", "/api/jobs.output", `{"id":"nope"}`},
		{"jobs.stop 任务不存在", "", "/api/jobs.stop", `{"sessionId":"s","id":"nope"}`},
		{"schedule.delete 任务不存在", "", "/api/schedule.delete", `{"sessionId":"s","id":"nope"}`},
		{"schedule.clear 存储失败", "", "/api/schedule.clear", `{}`},
		{"terminal.input 终端不存在", "", "/api/terminal.input", `{"id":"nope","data":"ls"}`},
		{"terminal.output 终端不存在", "", "/api/terminal.output", `{"id":"nope"}`},
		{"config.set values 为空", "", "/api/config.set", `{"values":{}}`},
	}

	for _, srv := range servers {
		s, err := NewServer(srv.opts)
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.cookies["scan-cookie"] = struct{}{}
		s.mu.Unlock()

		do := func(p apiProbe) {
			m := p.method
			if m == "" {
				m = http.MethodPost
			}
			req := httptest.NewRequest(m, p.path, strings.NewReader(p.body))
			req.AddCookie(&http.Cookie{Name: cookieName, Value: "scan-cookie"})
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			requests++
			if rec.Code >= 400 {
				errs++
			}
			where := srv.name + " / " + p.name
			code := scanErrorResponse(t, where, rec.Code, rec.Header(), rec.Body.Bytes())
			if code == "" && rec.Code < 400 {
				return
			}
			if strings.HasPrefix(p.path, "/api/") {
				sawError[Method(strings.TrimPrefix(p.path, "/api/"))] = true
			}
		}

		for _, m := range SessionMethods() {
			path := "/api/" + string(m)
			for _, b := range badApiBodies {
				do(apiProbe{name: string(m) + " " + b.name, path: path, body: b.body})
			}
		}
		for _, p := range extraProbes {
			do(p)
		}

		// 不走 /api 的出口：路由、认证、握手都在这里。
		noCookie := []apiProbe{
			{"未知路径", http.MethodGet, "/nope", ""},
			{"错动词", http.MethodGet, "/api/session.list", ""},
			{"没 cookie 打 /api", "", "/api/session.list", `{}`},
			{"错 token 换 cookie", http.MethodGet, "/connect?token=wrong", ""},
			{"没 cookie 升级 WS", http.MethodGet, "/ws", ""},
			{"没 cookie 看 debug", http.MethodGet, "/debug/stats", ""},
		}
		for _, p := range noCookie {
			m := p.method
			if m == "" {
				m = http.MethodPost
			}
			req := httptest.NewRequest(m, p.path, strings.NewReader(p.body))
			// 故意不带 cookie
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			requests++
			if rec.Code >= 400 {
				errs++
			}
			scanErrorResponse(t, srv.name+" / "+p.name, rec.Code, rec.Header(), rec.Body.Bytes())
		}

		// 带 cookie 但握手头不全：这条以前是 http.Error 的纯文本正文，
		// 四字段一个都没有（客户端只能在"升级失败"上瞎猜）。
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: "scan-cookie"})
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		requests++
		if rec.Code >= 400 {
			errs++
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s：缺握手头要 400，拿到 %d", srv.name, rec.Code)
		}
		if code := scanErrorResponse(t, srv.name+" / WS 握手头不全", rec.Code, rec.Header(), rec.Body.Bytes()); code != "bad_handshake" {
			t.Errorf("%s：WS 握手失败应当是 bad_handshake，实际 %q", srv.name, code)
		}
	}

	// 覆盖度：少一个 method 就说明这条扫描漏了它（不是"它没有错误分支"）。
	for _, m := range SessionMethods() {
		if !sawError[m] {
			t.Errorf("method %s 一次错误响应都没造出来——扫描对它形同虚设，补一条探针", m)
		}
	}
	t.Logf("扫了 %d 次请求，其中 %d 条错误响应逐个查了四字段", requests, errs)
	if errs < 100 {
		t.Fatalf("只造出 %d 条错误响应——某个回调不再失败了？扫描退化成空转", errs)
	}
}

// TestRetryableDecisionCoversEveryCode 钉住"码 → 能不能重试"这层映射本身。
//
// 上面那条测的是**出口**；这条测的是**映射表**：任何一条消息映射出来的码，
// 都必须在语义表里有说法、都有非空 hint。加新码忘了分类，这里红。
func TestRetryableDecisionCoversEveryCode(t *testing.T) {
	msgs := []string{
		"unauthorized",
		"not found",
		"not implemented",
		"没有这个会话",
		"上一轮还没结束",
		"no_such_request",
		"bad_option",
		"响应序列化失败",
		"响应是空值：handler 没有给出 result",
		"缺参数：要 requestId 与 optionId",
		"随便一句宿主原文，谁也认不出来",
	}
	for _, msg := range msgs {
		code := errorCode(msg)
		if _, ok := retryableSemantics[code]; !ok {
			t.Errorf("errorCode(%q)=%q 没有在 retryableSemantics 里分类", msg, code)
		}
		if hint := errorHint(code); hint == "" {
			t.Errorf("code=%q 没有 hint：客户端拿到它不知道下一步做什么", code)
		}
	}

	// 语义表里每一条都要能说清（防手滑写反）。
	if !retryableSemantics["stale_epoch"] {
		t.Error("stale_epoch 必须可重试：丢旧水位重新 load 就能继续")
	}
	if retryableSemantics["bad_request"] {
		t.Error("参数错不可重试：重发同一个请求还是同一个 400")
	}
}

// handwrittenError 是"绕过统一构造、手写错误体"的形态。
//
// 为什么用源码扫描而不是再补几十条行为测试：手写的那几处在**冷分支**上
// （只有世代过期、只有握手失败才会走到），行为测试很难把它们全盖住；
// 而"有没有第二种写法"是静态事实，扫一遍最便宜。
var handwrittenError = []struct {
	re  *regexp.Regexp
	why string
}{
	{
		regexp.MustCompile(`http\.Error\(`),
		"http.Error 写的是纯文本正文：四个字段一个都没有，客户端连 JSON 都解不动",
	},
	{
		regexp.MustCompile(`map\[string\](?:any|interface\{\})\{"error"`),
		"手写 error map：字段靠手抄，迟早少一个（H8 的 stale_epoch 就是这么漏的）",
	},
}

// errorBodyScanRoots 是**本契约的**出口所在的目录。
//
// `cmd/dshd-bridge` 不在里面：它说的是 ACP 信封（{ok:false,error:{code,message}}），
// 不是 Transport 契约，改它等于把两套协议混成一套。
var errorBodyScanRoots = []string{
	".",
	filepath.Join("..", "..", "cmd", "dshd"),
}

// TestNoHandwrittenErrorBodies 扫源码：错误体只能有一个构造点。
func TestNoHandwrittenErrorBodies(t *testing.T) {
	scanned := 0
	for _, root := range errorBodyScanRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("扫描根目录不存在：%s（%v）——静默跳过等于这条守卫不存在", root, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("读不了 %s：%v", root, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			// cmd/dshd 只扫 serve*：协议出口在 serve 那条线上，
			// 其余是 CLI 子命令（往 stdout 打表格，不是 HTTP 响应）。
			if strings.Contains(root, "cmd") && !strings.HasPrefix(name, "serve") {
				continue
			}
			path := filepath.Join(root, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读不了 %s：%v", path, err)
			}
			scanned++
			for i, line := range strings.Split(string(src), "\n") {
				// 注释里提到这些形态不算违规（本文件就在注释里提了）。
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				for _, h := range handwrittenError {
					if h.re.MatchString(line) {
						t.Errorf("%s:%d 手写错误体：%s\n    为什么不行：%s",
							filepath.ToSlash(path), i+1, strings.TrimSpace(line), h.why)
					}
				}
			}
		}
	}
	if scanned < 5 {
		t.Fatalf("只扫到 %d 个源文件——扫描根目录写错了？", scanned)
	}
}
