package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"dshgo/internal/session"
)

// 审批的"父子通道"（H13）。

// 为什么需要它：`dshd serve` 起子进程跑一轮时带 --policy ask，
// 但子进程没有终端——没人能回答那个 [y/N]。
// 于是子进程把"要问什么"写到 stderr 的一行，父进程解析后经 WS 问界面，
// 再把决定写回子进程 stdin 的一行。
//
// 为什么走 stderr/stdin 而不新开 fd：
//   - Windows 上继承额外 fd 麻烦，而 stderr/stdin 本来就通着；
//   - stderr 已经承担"给人看的诊断"，加一个行首标记就能解析；
//   - **不碰 stdout**——那里只有一行 ready 握手（H11 是进程级验过的契约）。
const (
	approvalRequestTag  = "@@APPROVAL@@ "
	approvalResponseTag = "@@APPROVAL-RESP@@ "
	// jobsOutReqTag / jobsOutRespTag 是「父进程问某个后台任务的输出」这一对。
	//
	// **方向和审批相反**：审批是子→父问、父→子答；这里是父→子问、子→父答。
	// 同一个 stdin，两个方向，所以必须先有一个**唯一的读端**——见 stdinDispatcher。
	jobsOutReqTag  = "@@JOBS-OUT@@ "
	jobsOutRespTag = "@@JOBS-OUT-RESP@@ "
)

// stdinDispatcher 是子进程侧**唯一**的 stdin 读端。
//
// # 为什么必须有它
//
// 在这个文件之前，`markerAsk` 每次审批都新建一个 bufio.Reader 去读 stdin 的一行
// ——**一次一个 reader**。单次审批没问题，但：
//   - 连续两次审批时，第一次的 reader 会预读并丢掉第二次的那一行（缓冲丢失）
//   - 而且 `jobs.output` 要的是同一根 stdin 上的另一类请求
//
// 两个读者抢一根流是拿不准的：谁先 Read 谁赢，输的那个永远等不到。
// 所以改成一个常驻 goroutine 读完再按标签分发。
var (
	stdinOnce  sync.Once
	approvalCh = make(chan string, 1)  // 审批应答（一次一条，缓冲 1 够）
	jobsOutCh  = make(chan string, 4)  // 任务输出请求（允许几秒内积压几条）
	steerReqCh = make(chan string, 16) // 同一根 stdin 上的当前 turn steer，保序交给唯一 inbox
)

func startStdinDispatcher() {
	stdinOnce.Do(func() {
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			// content 文本上限 1 MiB，但 JSON 转义与 steer envelope 会放大；
			// 附件字节不进 stdin。8 MiB 上限保住最坏块顺序且仍有明确背压边界。
			sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
			for sc.Scan() {
				line := sc.Text()
				switch {
				case strings.HasPrefix(line, approvalResponseTag):
					// **满了就丢**：审批应答只会有一份，丢一条说明调用方逻辑错了，
					// 阻塞在这里反而会卡死整个分发。
					select {
					case approvalCh <- line:
					default:
					}
				case strings.HasPrefix(line, jobsOutReqTag):
					select {
					case jobsOutCh <- line:
					default:
					}
				case strings.HasPrefix(line, steerIPCRequestTag):
					// steer 不能静默丢弃：背压挡住读端比父进程等一个永不到的 ACK 好。
					steerReqCh <- line
				}
				// 其它行（诊断输出、手敲的东西）一律忽略——这里不是人读的终端。
			}
			// **EOF 必须关掉 channel**：stdin 断了（父进程死了、管道关了）
			// 之后再也不会有应答，等待方要立刻知道"等不到了"，而不是永久阻塞。
			// 这一条以前由 ReadString 返回 error 隐式保证；换成 channel 之后
			// 得显式做，否则 markerAsk 会把整轮挂死（实测：测试直接卡住）。
			close(approvalCh)
			close(jobsOutCh)
			close(steerReqCh)
		}()
	})
}

// markerAsk 是没有终端、但有宿主在旁边时的审批通道。
//
// 读不到应答（管道断了、父进程死了）一律**拒绝**：
// 没人能回答的时候，"默认同意"是最坏的选择。
func markerAsk(ctx context.Context, sessionID, tool, args string) (bool, error) {
	req, err := json.Marshal(map[string]any{
		"requestId": "perm-" + session.NewID(),
		"sessionId": sessionID,
		"tool":      tool,
		"args":      args,
	})
	if err != nil {
		return false, nil
	}
	fmt.Fprintf(os.Stderr, "%s%s\n", approvalRequestTag, req)

	// **走共享分发，不自己 new reader**：见 stdinDispatcher 的注释
	// （每次新建 reader 会预读并丢掉下一行；而 jobs.output 也在用同一根 stdin）。
	startStdinDispatcher()
	var line string
	select {
	case l, ok := <-approvalCh:
		if !ok {
			return false, nil // stdin 关了：没人能回答，一律拒绝
		}
		line = l
	case <-ctx.Done():
		return false, nil // 父进程超时：当作没允许，不阻塞这一轮
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, approvalResponseTag) {
		return false, nil
	}
	var resp struct {
		Allow bool `json:"allow"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(line, approvalResponseTag)), &resp) != nil {
		return false, nil
	}
	return resp.Allow, nil
}

// approvalViaStdio 决定这一轮要不要走父子通道：
// 宿主明确开了这个开关才走，否则**不动**原来的终端行为。
func approvalViaStdio() bool {
	return os.Getenv("DSHD_APPROVAL_STDIO") == "1"
}
