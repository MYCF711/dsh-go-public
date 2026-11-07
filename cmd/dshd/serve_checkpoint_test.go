package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// 本文件钉的是官方 #78 `session-checkpoint-policy` 的 durability 时机：
// **模型请求发出之前**，会话请求前缀必须已经落成一份可恢复快照。
//
// 官方原件（0.2.2-rc.1，asar 只读解出）：
//   dsh/node_modules/@deepseek-ai/dsh-session-checkpoint-policy/lib/index.js:26-31
//     async function afterCheckpoint(ctx, session, next) {
//         await ctx.sessions.flush(session);   // 先落盘
//         yield* next();                       // 再让适配器发请求
//     }
// 判定：flush 拒绝 ⇒ 适配器根本不派发（fail-closed），见同文件 :55-57。
//
// 我方对应物是 <home>/durability-checkpoints/<会话段>/<已覆盖事件数>/checkpoint.json：
// 只读派生 + 独立侧车，绝不回写 v4。

const (
	checkpointCrashChildEnv  = "DSHD_CHECKPOINT_CRASH_CHILD"
	checkpointCrashHomeEnv   = "DSHD_CHECKPOINT_CRASH_HOME"
	checkpointCrashSIDEnv    = "DSHD_CHECKPOINT_CRASH_SID"
	checkpointCrashMarkEnv   = "DSHD_CHECKPOINT_CRASH_MARKER"
	checkpointCrashPrefixEnv = "DSHD_CHECKPOINT_CRASH_PREFIX"

	checkpointCrashPrompt = "上一轮的回答我看过了，接着说"
)

// checkpointDir 是测试侧独立算出的侧车路径：只要产品真的按契约落盘，
// 这里就该命中；产品没落盘时它必须报「没有」。
func checkpointDir(t *testing.T, home, sessionID string, coveredEvents int64) string {
	t.Helper()
	seg, err := v4.EncodeSegment(sessionID)
	if err != nil {
		t.Fatalf("会话段编码失败: %v", err)
	}
	return filepath.Join(home, "durability-checkpoints", seg, strconv.FormatInt(coveredEvents, 10))
}

// checkpointOnDisk 是测试侧对侧车 JSON 的独立读法（不复用产品结构体）：
// 侧车是耐久产物，测试必须自己解得出字段，才算「磁盘上真的有这份东西」。
type checkpointOnDisk struct {
	Version       int    `json:"version"`
	SessionID     string `json:"sessionId"`
	CoveredEvents int64  `json:"coveredEvents"`
	LastSeq       int64  `json:"lastSeq"`
	SessionPath   string `json:"sessionPath"`
	SizeBytes     int64  `json:"sizeBytes"`
	SHA256        string `json:"sha256"`
	Frames        int    `json:"frames"`
	Request       *struct {
		Kind      string `json:"kind"`
		SHA256    string `json:"sha256"`
		Bytes     int64  `json:"bytes"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	} `json:"request"`
}

func readCheckpointOnDisk(t *testing.T, dir, why string) checkpointOnDisk {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatalf("%s，但磁盘上没有任何 durability 快照：%v（期望 %s）", why, err, dir)
	}
	var cp checkpointOnDisk
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatalf("快照不是合法 JSON: %v（原样 %q）", err, string(raw))
	}
	return cp
}

// verifyCheckpointPrefix 复核「已发请求前的帧完整可读」：快照记的字节范围
// 必须和磁盘现状逐字节一致，且这些帧必须能被 v4 解析器完整读出来。
func verifyCheckpointPrefix(t *testing.T, cp checkpointOnDisk, sessionBytes []byte) {
	t.Helper()
	if cp.SizeBytes <= 0 || cp.SizeBytes > int64(len(sessionBytes)) {
		t.Fatalf("快照记的字节范围 %d 越界（会话文件 %d 字节）", cp.SizeBytes, len(sessionBytes))
	}
	prefix := sessionBytes[:cp.SizeBytes]
	sum := sha256.Sum256(prefix)
	if got := hex.EncodeToString(sum[:]); got != cp.SHA256 {
		t.Fatalf("请求前的前缀已被改写：磁盘 sha256=%s，快照 sha256=%s", got, cp.SHA256)
	}
	log, err := v4.Read(prefix)
	if err != nil {
		t.Fatalf("请求前的帧读不出来: %v", err)
	}
	if log.TailTruncatedAt != 0 {
		t.Fatalf("请求前的帧有半帧尾巴（offset=%d），前缀不完整", log.TailTruncatedAt)
	}
	if int64(len(log.Events)) != cp.CoveredEvents {
		t.Fatalf("前缀事件数 %d 与快照 %d 不一致", len(log.Events), cp.CoveredEvents)
	}
	for i, ev := range log.Events {
		if ev.Seq != int64(i) {
			t.Fatalf("第 %d 个事件 seq=%d，前缀 seq 不连续", i, ev.Seq)
		}
	}
	if cp.LastSeq != cp.CoveredEvents-1 {
		t.Fatalf("lastSeq=%d 与 coveredEvents=%d 不自洽", cp.LastSeq, cp.CoveredEvents)
	}
}

// TestCheckpointRequestPrefixSurvivesKill 是「请求前已落盘」的唯一硬证据：
// 子进程把请求真发出去（假网关收到 POST 才写 in-flight 标记），然后整进程被杀；
// 父进程重启后必须能读出请求前的全部帧。
func TestCheckpointRequestPrefixSurvivesKill(t *testing.T) {
	if os.Getenv(checkpointCrashChildEnv) == "1" {
		runCheckpointCrashChild(t)
		return
	}

	home := t.TempDir()
	sid := "session-ckpt-crash"
	marker := filepath.Join(home, "in-flight.marker")
	prefixCopy := filepath.Join(home, "prefix.bin")

	cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointRequestPrefixSurvivesKill$", "-test.v")
	cmd.Env = append(os.Environ(),
		checkpointCrashChildEnv+"=1",
		checkpointCrashHomeEnv+"="+home,
		checkpointCrashSIDEnv+"="+sid,
		checkpointCrashMarkEnv+"="+marker,
		checkpointCrashPrefixEnv+"="+prefixCopy,
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("起子进程: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	markerText := ""
	deadline := time.Now().Add(60 * time.Second)
	for {
		if raw, err := os.ReadFile(marker); err == nil {
			markerText = strings.TrimSpace(string(raw))
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			killed = true
			t.Fatalf("子进程没有把模型请求发出去（等 in-flight 标记超时）。子进程输出：\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 请求**已经在飞**：此刻落盘的一切，都是「请求前」的东西。
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("杀子进程: %v", err)
	}
	killWait := cmd.Wait()
	killed = true

	before, err := os.ReadFile(prefixCopy)
	if err != nil {
		t.Fatalf("读子进程派发前拍下的会话字节: %v", err)
	}
	path, err := newSessionSource(home, "").resolve(sid)
	if err != nil {
		t.Fatalf("重启后找不到会话文件（杀进程后无法恢复）: %v\n子进程输出：\n%s", err, out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("重启后读会话文件: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("v4 会话文件被改写了：请求前 %d 字节，重启后 %d 字节（append-only 违规）", len(before), len(after))
	}

	why := fmt.Sprintf("模型请求已发出（in-flight 标记=%q），进程已被杀（wait=%v），重启后", markerText, killWait)
	// 种子内容：turn/start、user/message、assistant/message、turn/end = 4 帧。
	cp := readCheckpointOnDisk(t, checkpointDir(t, home, sid, 4), why)
	if cp.SessionID != sid {
		t.Fatalf("快照会话 id = %q，期望 %q", cp.SessionID, sid)
	}
	if cp.SessionPath != path {
		t.Fatalf("快照记的会话路径 = %q，期望 %q", cp.SessionPath, path)
	}
	if int64(len(before)) != cp.SizeBytes {
		t.Fatalf("快照字节数 %d 与会话长度 %d 不一致", cp.SizeBytes, len(before))
	}
	verifyCheckpointPrefix(t, cp, before)
	if cp.Request == nil {
		t.Fatalf("快照没有记录这次请求的身份（无法判断崩溃时在飞的是哪一条）")
	}
	if cp.Request.Kind != "text" || cp.Request.Text != checkpointCrashPrompt {
		t.Fatalf("快照记的请求 = %+v，期望 text=%q", *cp.Request, checkpointCrashPrompt)
	}
	wantSum := sha256.Sum256([]byte(checkpointCrashPrompt))
	if cp.Request.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("请求摘要 = %s，期望 %s", cp.Request.SHA256, hex.EncodeToString(wantSum[:]))
	}
}

// TestCheckpointPromptFailsClosedOnDiskFailure 钉 requirement 4：
// 快照落不下来就**不许**发起模型请求，并且不能留半写产物。
func TestCheckpointPromptFailsClosedOnDiskFailure(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	sid := "session-ckpt-diskfail"
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		return lg.Append("turn/start", session.TurnStartData(1))
	})
	// 用同名普通文件占住侧车根目录：MkdirAll 必然失败，且不依赖权限位，
	// 在 Windows/root 下都成立。
	if err := os.WriteFile(filepath.Join(home, "durability-checkpoints"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	dispatched := false
	opt := withSessionOperationOptions(home, transport.Options{
		Prompt: func(string, string) (transport.PromptResult, error) {
			dispatched = true
			return transport.PromptResult{Accepted: true, TurnID: "t"}, nil
		},
	})
	if _, err := opt.Prompt(sid, "落盘失败时必须挡住这一轮"); err == nil {
		t.Fatalf("快照写盘失败却返回成功（静默半写）")
	}
	if dispatched {
		t.Fatalf("快照写盘失败仍派发了模型请求（不是 fail-closed）")
	}
	// 回滚：不许留 *.tmp，也不许留半个 checkpoint.json。
	var leftovers []string
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(p) == ".tmp" || filepath.Base(p) == "checkpoint.json" {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Fatalf("失败后留下半写产物: %v", leftovers)
	}
}

// TestCheckpointIdempotentPerTurnAndSessionIsolation 钉 requirement 4：
// 同一轮重复调用只落一份（字节不许被改写）；A/B 会话不串。
func TestCheckpointIdempotentPerTurnAndSessionIsolation(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	calls := 0
	opt := withSessionOperationOptions(home, transport.Options{
		Prompt: func(string, string) (transport.PromptResult, error) {
			calls++
			return transport.PromptResult{Accepted: true, TurnID: "t"}, nil
		},
	})
	seedTurnSession(t, home, cwd, "session-ckpt-a", func(lg *session.Log) error {
		return lg.Append("turn/start", session.TurnStartData(1))
	})
	seedTurnSession(t, home, cwd, "session-ckpt-b", func(lg *session.Log) error {
		if err := lg.Append("turn/start", session.TurnStartData(1)); err != nil {
			return err
		}
		return lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "B 的前缀"}}))
	})
	dirA := checkpointDir(t, home, "session-ckpt-a", 1)
	dirB := checkpointDir(t, home, "session-ckpt-b", 2)

	if _, err := opt.Prompt("session-ckpt-a", "同一轮第一次"); err != nil {
		t.Fatalf("A 第一次 prompt: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dirA, "checkpoint.json"))
	if err != nil {
		t.Fatalf("A 没有快照: %v", err)
	}
	// 同一轮（v4 一点没长）重复调用：只落一份，字节不许被改写。
	if _, err := opt.Prompt("session-ckpt-a", "同一轮重复调用"); err != nil {
		t.Fatalf("A 重复 prompt: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dirA, "checkpoint.json"))
	if err != nil {
		t.Fatalf("A 重复调用后读快照: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("同一轮重复调用改写了快照\n第一次: %s\n第二次: %s", first, second)
	}
	entries, err := os.ReadDir(filepath.Dir(dirA))
	if err != nil || len(entries) != 1 || entries[0].Name() != "1" {
		t.Fatalf("同一轮应只有一份快照，实际 entries=%v err=%v", entries, err)
	}

	// A/B 不串：B 的快照归属 B，且不许动 A 的字节。
	if _, err := opt.Prompt("session-ckpt-b", "另一个会话"); err != nil {
		t.Fatalf("B prompt: %v", err)
	}
	cpB := readCheckpointOnDisk(t, dirB, "B 会话已受理请求")
	if cpB.SessionID != "session-ckpt-b" || cpB.CoveredEvents != 2 {
		t.Fatalf("B 的快照记错了身份: id=%q covered=%d", cpB.SessionID, cpB.CoveredEvents)
	}
	againA, err := os.ReadFile(filepath.Join(dirA, "checkpoint.json"))
	if err != nil || !bytes.Equal(first, againA) {
		t.Fatalf("B 的轮次改动了 A 的快照（err=%v）", err)
	}
	if calls != 3 {
		t.Fatalf("内层 prompt 应被调用 3 次，实际 %d", calls)
	}
}

// TestCheckpointNeverWritesV4 钉 requirement 2：快照是只读派生 + 独立侧车，
// 前后 v4 原始事件必须 bytes.Equal。
func TestCheckpointNeverWritesV4(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	sid := "session-ckpt-immutable"
	opt := withSessionOperationOptions(home, transport.Options{
		Prompt: func(string, string) (transport.PromptResult, error) {
			return transport.PromptResult{Accepted: true, TurnID: "t"}, nil
		},
	})
	path := seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		return lg.Append("turn/start", session.TurnStartData(1))
	})
	read := func() []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 v4: %v", err)
		}
		return b
	}

	before := read()
	if _, err := opt.Prompt(sid, "第一轮请求"); err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	if after := read(); !bytes.Equal(before, after) {
		t.Fatalf("快照路径改写了 v4：%d -> %d 字节", len(before), len(after))
	}

	// 模拟这一轮真正落盘的事件，再发下一轮：新锚点必然是新快照。
	lg, err := session.OpenSessionAt(path)
	if err != nil {
		t.Fatalf("打开会话写入: %v", err)
	}
	if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "第一轮请求"}})); err != nil {
		t.Fatalf("写用户消息: %v", err)
	}
	if err := lg.Append("turn/end", session.TurnEndData(1, "completed", "")); err != nil {
		t.Fatalf("写轮次结束: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("关会话: %v", err)
	}
	grown := read()
	if _, err := opt.Prompt(sid, "第二轮请求"); err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	if after := read(); !bytes.Equal(grown, after) {
		t.Fatalf("第二轮快照改写了 v4：%d -> %d 字节", len(grown), len(after))
	}
	// 两个不同锚点各自一份，覆盖事件数必须与当时的前缀一致。
	for _, covered := range []int64{1, 3} {
		cp := readCheckpointOnDisk(t, checkpointDir(t, home, sid, covered), "第 "+strconv.FormatInt(covered, 10)+" 帧的请求已受理")
		if cp.CoveredEvents != covered || cp.SessionID != sid {
			t.Fatalf("覆盖 %d 帧的快照记错了身份: %+v", covered, cp)
		}
	}
}

// runCheckpointCrashChild 在子进程里真实派发一次模型请求，然后挂住等父进程杀。
func runCheckpointCrashChild(t *testing.T) {
	home := os.Getenv(checkpointCrashHomeEnv)
	sid := os.Getenv(checkpointCrashSIDEnv)
	marker := os.Getenv(checkpointCrashMarkEnv)
	prefixCopy := os.Getenv(checkpointCrashPrefixEnv)
	if home == "" || sid == "" || marker == "" || prefixCopy == "" {
		t.Fatalf("子进程环境不全")
	}
	cwd := filepath.Join(home, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("建工作区: %v", err)
	}
	path := seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("turn/start", session.TurnStartData(1)); err != nil {
			return err
		}
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "上一轮的问题"}})); err != nil {
			return err
		}
		if err := lg.Append("assistant/message",
			session.AssistantMessageEvent(1, 1, []map[string]any{session.TextBlock("上一轮的回答")}, "stop", nil)); err != nil {
			return err
		}
		return lg.Append("turn/end", session.TurnEndData(1, "completed", ""))
	})

	// 假模型网关：收到 POST 才写标记 —— 标记存在就等于「请求真的发出去了」。
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(marker, []byte(r.Method+" "+r.URL.Path), 0o600); err != nil {
			t.Errorf("写 in-flight 标记: %v", err)
		}
		<-r.Context().Done() // 不产生任何响应：请求一直吊着，父进程在这时杀
	}))
	defer gateway.Close()

	runner := &modelRunner{home: home, base: gateway.URL + "/v1", model: "m"}
	opt := withSessionOperationOptions(home, transport.Options{
		Prompt: func(sessionID, text string) (transport.PromptResult, error) {
			if err := runner.Run(context.Background(), sessionID, "turn-ckpt", text); err != nil {
				return transport.PromptResult{}, err
			}
			return transport.PromptResult{Accepted: true, TurnID: "turn-ckpt"}, nil
		},
	})
	// 派发前先把会话字节原样拍一份，父进程用它证明 v4 没被快照路径改写。
	sessionBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读会话字节: %v", err)
	}
	if err := os.WriteFile(prefixCopy, sessionBytes, 0o600); err != nil {
		t.Fatalf("写派发前字节副本: %v", err)
	}
	if _, err := opt.Prompt(sid, checkpointCrashPrompt); err != nil {
		t.Fatalf("prompt 失败: %v", err)
	}
	// 走到这里说明请求返回了（网关没吊住）；父进程会看到标记并杀，这条只是兜底。
	<-time.After(30 * time.Second)
}
