package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// reply 只做一件事：把**已经定好**的 allow 写回子进程 stdin。
// 它不做判定（判定在 decisionOf 与 acp.Run），但写出去的东西必须能被子进程解出来。
//
// 这也是 scripts/perm-gates.json 里把它从 exempt 转成"有测试钉着"的那条：
// 判据是"写出去的一行必须是合法 JSON，且 allow 就是传进来的那个"。
func TestApprovalReplyWritesParseableLine(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	b := &approvalBroker{stdin: pw}

	go func() {
		b.reply("perm-1", true)
		b.reply("perm-2", false)
		_ = pw.Close()
	}()

	sc := bufio.NewScanner(pr)
	var got []map[string]any
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), approvalResponseTag)
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("写出去的行不是合法 JSON：%q（%v）", line, err)
		}
		got = append(got, m)
	}
	if len(got) != 2 {
		t.Fatalf("应当写两行，实际 %d", len(got))
	}
	if got[0]["requestId"] != "perm-1" || got[0]["allow"] != true {
		t.Fatalf("第一行不对：%v", got[0])
	}
	if got[1]["requestId"] != "perm-2" || got[1]["allow"] != false {
		t.Fatalf("第二行不对：%v", got[1])
	}
}

// slowWriter 把每次写拆成**逐字节**、每字节让一下。
//
// 为什么不用 io.Pipe 直接验：短写在 pipe 上常常一次就完成，两行根本撞不上，
// 测试会假绿（实测：去掉 writeMu 它照样过）。拆成逐字节 + 主动让出，
// 才真正模拟"两个来源同时写一根 stdin"的竞争窗口。
type slowWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *slowWriter) Write(p []byte) (int, error) {
	for _, c := range p {
		w.mu.Lock()
		w.buf.WriteByte(c)
		w.mu.Unlock()
		runtime.Gosched()
	}
	return len(p), nil
}

func (w *slowWriter) Close() error { return nil }

// 并发写不能写出交错的半行。
//
// 现实中审批有两个来源同时结算：客户端点了允许、或超时那条 goroutine 到期。
// 没有 writeMu 时两行会绞在一起，子进程解析失败，现象是"审批偶尔没生效"。
func TestApprovalReplyIsSerialized(t *testing.T) {
	sw := &slowWriter{}
	b := &approvalBroker{stdin: sw}

	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.reply("perm-x", true)
		}()
	}
	wg.Wait()

	sc := bufio.NewScanner(strings.NewReader(sw.buf.String()))
	lines := 0
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), approvalResponseTag)
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("第 %d 行被写坏了（并发没串行化）：%q", lines+1, line)
		}
		lines++
	}
	if lines != n {
		t.Fatalf("应当有 %d 行，实际 %d（有行被吞或粘在一起）", n, lines)
	}
}
