package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/agent"
	"dshgo/internal/modes"
	"dshgo/internal/session"
)

// 这个文件钉住 D1：**开了的 turn 一定有结局**。
//
// 判据不是"进程退出码非 0"——那是当场能看见的信号；只读日志的人（回放、UI、
// history_search、事后取证）看不见退出码，只能看见日志。日志里一个 turn 开了
// 却没闭合，消费者无从判断"这一轮到底跑完没有"，只能各自猜一个默认值。
// 所以这一份测试只读 v4 文件，断言三件事：闭合了、恰好一条、kind 说的是实话。

// d1TurnEnd 是一条 turn/end 里消费者真正读得到的字段。
type d1TurnEnd struct {
	Seq    int64
	Kind   string
	Detail string // error.message 或 aborted 的 reason
}

// d1Closure 是"这份日志里 turn 开合得对不对"的读数。
type d1Closure struct {
	Starts int
	Ends   []d1TurnEnd
}

func d1ReadClosure(t *testing.T, path string) d1Closure {
	t.Helper()
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读会话日志 %s 失败: %v", path, err)
	}
	var c d1Closure
	for _, ev := range back.Events {
		switch ev.Type {
		case "turn/start":
			c.Starts++
		case "turn/end":
			var d struct {
				Reason struct {
					Kind  string `json:"kind"`
					Error *struct {
						Name    string `json:"name"`
						Message string `json:"message"`
					} `json:"error"`
					Reason string `json:"reason"`
				} `json:"reason"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatalf("turn/end 不是合法 JSON: %v", err)
			}
			e := d1TurnEnd{Seq: ev.Seq, Kind: d.Reason.Kind}
			switch {
			case d.Reason.Error != nil:
				e.Detail = d.Reason.Error.Message
			case d.Reason.Reason != "":
				e.Detail = d.Reason.Reason
			}
			c.Ends = append(c.Ends, e)
		}
	}
	return c
}

// d1AssertClosed 钉住"闭合了、只闭合一次、kind 说实话"。
//
// 三条都是消费者会踩的坑：没闭合 = 悬挂；闭合两次 = 数错轮次；
// kind 写 completed = 把失败伪装成跑完了。
func d1AssertClosed(t *testing.T, path, wantKind, wantDetail string) {
	t.Helper()
	c := d1ReadClosure(t, path)
	if c.Starts == 0 {
		t.Fatalf("日志里连 turn/start 都没有，测试没走到那一轮：%s", path)
	}
	if len(c.Ends) != 1 {
		t.Fatalf("turn/start %d 条、turn/end %d 条：这一轮必须**恰好**闭合一次"+
			"（悬挂和重复都会让消费者数错）：%v", c.Starts, len(c.Ends), c.Ends)
	}
	got := c.Ends[0]
	if got.Kind != wantKind {
		t.Fatalf("turn/end 的 kind = %q，期望 %q（完整 reason: %+v）", got.Kind, wantKind, got)
	}
	if wantKind == "completed" || wantKind == "max-tokens" {
		return // 成功路径不要求原因文本
	}
	if got.Detail == "" {
		t.Fatalf("失败结局必须留下原因（error.message / aborted.reason），实际空：%+v", got)
	}
	if wantDetail != "" && !strings.Contains(got.Detail, wantDetail) {
		t.Fatalf("结局原因里应当能看出 %q，实际 %q", wantDetail, got.Detail)
	}
}

// d1Workspace 把进程切到一个只有 tools 模块的临时工作区，并预建一个空会话。
//
// 为什么 chdir + 最小 manifest：cmdRun 不传 --manifest 时读的是相对的
// `dsh.toml`（同 schedule_test.go 的做法）。只装 tools 模块，测试进程就不会
// 把真 bash/fs/网络工具装进来，也就不会碰用户目录。
func d1Workspace(t *testing.T, sid string) (home, cwd string) {
	t.Helper()
	base := t.TempDir()
	manifest := "# D1 测试用最小 manifest：cmdRun 只需要 ToolsKey / ToolRunnerKey\n" +
		"[[module]]\nid = \"tools\"\n"
	if err := os.WriteFile(filepath.Join(base, "dsh.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	// cmdRun 会把全局 rulesPath 重置成默认值；还原，免得影响别的测试。
	oldRules := *rulesPath
	t.Cleanup(func() { *rulesPath = oldRules })

	home = t.TempDir()
	l, err := session.CreateSession(home, base, sid, "default")
	if err != nil {
		t.Fatalf("预建会话失败: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return home, base
}

// d1SSE 起一个假网关；每次请求把请求序号交给 write，测试自己决定流长什么样。
func d1SSE(t *testing.T, write func(w http.ResponseWriter, n int)) string {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, int(atomic.AddInt32(&n, 1)))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// d1Text 是最小的一段完整流：一句正文 + 指定 finish_reason。
func d1Text(content, finish string) func(http.ResponseWriter, int) {
	return func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
			`{"choices":[{"delta":{"content":%q},"finish_reason":%q}]}`, content, finish))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// d1ToolCall 是"模型一直要调工具"的流：每一步都请求同一个工具，永不收工。
func d1ToolCall(tool string) func(http.ResponseWriter, int) {
	return func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function",`+
				`"function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, tool))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// d1Run 用给定网关跑一次 `run`，返回它的错误。
func d1Run(t *testing.T, home, sid, base string, extra ...string) error {
	t.Helper()
	argv := []string{
		"--home", home, "--session", sid,
		"--prompt", "做点事", "--policy", "allow", "--base", base,
	}
	return cmdRun(append(argv, extra...))
}

// TestRunClosesTurnOnEveryExit 是 D1 的主证据：run 的每一种结束方式都在日志里闭合。
//
// 「工具失败」不在这张表里，这是有意的：工具失败被记成 tool/result{isError:true}
// 之后循环继续，turn 仍会正常收工——它不是一条"让 turn 失败的路径"。
func TestRunClosesTurnOnEveryExit(t *testing.T) {
	cases := []struct {
		name    string
		stream  func(http.ResponseWriter, int)
		extra   []string
		wantErr bool
		kind    string
		detail  string
	}{
		{
			name: "网关 500：模型调用失败",
			stream: func(w http.ResponseWriter, _ int) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, "upstream boom")
			},
			wantErr: true, kind: "error", detail: "500",
		},
		{
			name: "200 但流是空的：空响应",
			stream: func(w http.ResponseWriter, _ int) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
			},
			wantErr: true, kind: "error", detail: "空响应",
		},
		{
			name:    "步数用满仍未收敛",
			stream:  d1ToolCall("nope"),
			extra:   []string{"--max-steps", "1"},
			wantErr: true, kind: "error", detail: "用满",
		},
		{
			name:   "正常收敛",
			stream: d1Text("好", "stop"),
			kind:   "completed",
		},
		{
			name:   "被 max_tokens 截断",
			stream: d1Text("半句", "length"),
			kind:   "max-tokens",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, cwd := d1Workspace(t, "d1-run")
			base := d1SSE(t, c.stream)
			err := d1Run(t, home, "d1-run", base, c.extra...)
			if c.wantErr && err == nil {
				t.Fatal("这条路径应当失败；没失败说明测试根本没走到那个分支")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("这条路径不该失败: %v", err)
			}
			path, perr := session.SessionPath(home, cwd, "d1-run")
			if perr != nil {
				t.Fatal(perr)
			}
			d1AssertClosed(t, path, c.kind, c.detail)
		})
	}
}

// d1ACPLogPath 取 ACP 那边这一轮的会话文件。
func d1ACPLogPath(t *testing.T, r *acpRunner, sid string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.logs[sid]
	if l == nil {
		t.Fatalf("ACP 没有为会话 %s 建日志", sid)
	}
	return l.Path
}

// TestACPPromptClosesTurnOnEveryExit 是 D1 在 ACP 那一侧的证据。
//
// ACP 的失败不是靠退出码表达的（进程还活着，下一个 prompt 还要继续用），
// 所以日志是**唯一**留下"这一轮怎么了"的地方。
func TestACPPromptClosesTurnOnEveryExit(t *testing.T) {
	builtin, err := modes.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	quiet := func(agent.Event) error { return nil }

	t.Run("网关 500：模型调用失败", func(t *testing.T) {
		r, _, _ := newPromptHarness(t, d1SSE(t, func(w http.ResponseWriter, _ int) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "upstream boom")
		}), modes.DefaultID, builtin)
		_, _, perr := r.Prompt(context.Background(), "d1-acp-500", t.TempDir(), "做点事", quiet)
		if perr == nil {
			t.Fatal("网关 500 时 Prompt 应当返回错误")
		}
		d1AssertClosed(t, d1ACPLogPath(t, r, "d1-acp-500"), "error", "500")
	})

	t.Run("步数用满仍未收敛", func(t *testing.T) {
		r, _, _ := newPromptHarness(t, d1SSE(t, d1ToolCall("w")), modes.DefaultID, builtin)
		_, _, perr := r.Prompt(context.Background(), "d1-acp-steps", t.TempDir(), "做点事", quiet)
		if perr == nil {
			t.Fatal("步数用满时 Prompt 应当返回错误")
		}
		d1AssertClosed(t, d1ACPLogPath(t, r, "d1-acp-steps"), "error", "用满")
	})

	t.Run("取消：Prompt 回 cancelled 且没有 error", func(t *testing.T) {
		gotReq := make(chan struct{}, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case gotReq <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)

		r, _, _ := newPromptHarness(t, srv.URL, modes.DefaultID, builtin)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cwd := t.TempDir()
		type result struct {
			stop string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			stop, _, perr := r.Prompt(ctx, "d1-acp-cancel", cwd, "做点事", quiet)
			done <- result{stop: stop, err: perr}
		}()
		<-gotReq
		cancel()
		res := <-done
		if res.err != nil {
			t.Fatalf("取消不是错误（Prompt 的约定是回 cancelled）: %v", res.err)
		}
		if res.stop != "cancelled" {
			t.Fatalf("stopReason = %q，期望 cancelled", res.stop)
		}
		d1AssertClosed(t, d1ACPLogPath(t, r, "d1-acp-cancel"), "aborted", "取消")
	})
}
