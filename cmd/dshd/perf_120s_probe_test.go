package main

// HOST-PERF-120S 的进程级合成负载。
//
// 这个文件只在 go test -c 产物里生效：DSHD_PERF_120S=1 且 argv[1]=serve 时，
// 直接调用产品自己的 cmdServe；argv[1]=run 时，loopRunner 重 exec 同一个测试
// 二进制，这里追加一轮确定的 v4 事件。因而 HTTP/WS、journal、follow、approval、
// 持久化与进程树全是真实实现，只有模型/工具副作用由合成事件替代。普通 go test
// 与 go build 不进入这两条分支。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/spi"
)

const perf120SEnv = "DSHD_PERF_120S"

func init() {
	if os.Getenv(perf120SEnv) != "1" || len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "serve":
		if err := cmdServe(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "PERF120S serve 失败: %v\n", err)
			os.Exit(90)
		}
		os.Exit(0)
	case "run":
		if err := runPerf120SChild(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "PERF120S child 失败: %v\n", err)
			os.Exit(91)
		}
		os.Exit(0)
	}
}

func runPerf120SChild(args []string) error {
	home, sid, prompt := "", "", ""
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--home":
			home = args[i+1]
		case "--session":
			sid = args[i+1]
		case "--prompt":
			prompt = args[i+1]
		}
	}
	if home == "" || sid == "" || !strings.HasPrefix(prompt, "PERF120S ") {
		return fmt.Errorf("缺 --home/--session 或 PERF120S prompt")
	}
	var marker string
	var iteration int
	if _, err := fmt.Sscanf(prompt, "PERF120S marker=%s iteration=%d", &marker, &iteration); err != nil {
		return fmt.Errorf("解析合成轮次: %w", err)
	}
	path, err := newSessionSource(home, "").resolve(sid)
	if err != nil {
		return err
	}
	lg, err := session.OpenSessionAt(path)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = lg.Close()
		}
	}()
	appendEvent := func(typ string, data any) error {
		// 让 follow 的 200ms tick 与多会话调度真实交错；不是一次性批量 drain。
		time.Sleep(90 * time.Millisecond)
		return lg.Append(typ, data)
	}
	payload := strings.Repeat("负载-XYZ🙂", 256)
	if err := appendEvent("turn/start", session.TurnStartData(iteration)); err != nil {
		return err
	}
	if err := appendEvent("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: marker + " " + payload}})); err != nil {
		return err
	}
	if err := appendEvent("assistant/message", session.AssistantMessageEvent(iteration, 1,
		[]map[string]any{session.ReasoningBlock(marker + " reasoning-1 " + payload)}, "stop", nil)); err != nil {
		return err
	}
	if err := appendEvent("assistant/message", session.AssistantMessageEvent(iteration, 2,
		[]map[string]any{session.TextBlock(marker + " text-1 " + payload)}, "stop", nil)); err != nil {
		return err
	}
	callID := "call-" + marker
	argsJSON := string(mustPerf120SJSON(map[string]any{"path": marker + ".txt", "content": payload}))
	if err := appendEvent("tool/call", session.ToolCallEvent(iteration, 3, callID, "write", argsJSON)); err != nil {
		return err
	}
	allowed := true
	if os.Getenv("DSHD_APPROVAL_STDIO") == "1" {
		requestID := "approval-" + marker
		request := string(mustPerf120SJSON(map[string]any{
			"requestId": requestID, "sessionId": sid, "tool": "write",
			"args": map[string]any{"path": marker + ".txt", "content": payload},
		}))
		fmt.Fprintf(os.Stderr, "%s%s\n", approvalRequestTag, request)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return fmt.Errorf("读审批应答 %s: %w", requestID, err)
		}
		if !strings.HasPrefix(line, approvalResponseTag) {
			return fmt.Errorf("审批应答缺前缀: %q", line)
		}
		var response struct {
			RequestID string `json:"requestId"`
			Allow     bool   `json:"allow"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, approvalResponseTag))), &response); err != nil {
			return err
		}
		if response.RequestID != requestID {
			return fmt.Errorf("审批串流：期望 %s，收到 %s", requestID, response.RequestID)
		}
		allowed = response.Allow
	}
	if err := appendEvent("tool/result", session.ToolResultEvent(iteration, 3, callID,
		[]spi.Block{{Type: "text", Text: marker + " tool-result " + payload}}, !allowed, "", nil)); err != nil {
		return err
	}
	if err := appendEvent("assistant/message", session.AssistantMessageEvent(iteration, 4,
		[]map[string]any{session.ReasoningBlock(marker + " reasoning-2 " + payload)}, "stop", nil)); err != nil {
		return err
	}
	if err := appendEvent("assistant/message", session.AssistantMessageEvent(iteration, 5,
		[]map[string]any{session.TextBlock(marker + " text-2 " + payload)}, "stop", nil)); err != nil {
		return err
	}
	if err := appendEvent("turn/end", session.TurnEndData(iteration, "completed", "")); err != nil {
		return err
	}
	if err := lg.Close(); err != nil {
		return err
	}
	closed = true
	// 保证至少一次 live follow，而不是所有帧都靠子进程退出后的最终 drain。
	time.Sleep(260 * time.Millisecond)
	return nil
}

func mustPerf120SJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
