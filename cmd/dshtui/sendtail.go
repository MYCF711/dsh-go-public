package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"time"
)

// sendAndTail 是最小闭环：建会话 → 订阅 → 发消息 → 把帧按行打出来。
//
// 它同时是**链路自检**：界面还没做，但 Transport 这几步任何一处不对，
// 这里先红，而不是等界面画了半天才发现收不到帧。
func sendAndTail(addr, token, text string) error {
	if token == "" {
		return fmt.Errorf("缺 --token（宿主 ready 行里的 token，一次性）")
	}
	c, err := newClient(addr, token)
	if err != nil {
		return err
	}
	sid, err := c.createSession()
	if err != nil {
		return err
	}
	fmt.Printf("会话 %s\n", sid)

	frames, closeFn, err := c.Subscribe(sid, 0)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := c.prompt(sid, text); err != nil {
		return err
	}

	// 退出条件：看到 turn/done 或 turn/error，或 60 秒没动静。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	deadline := time.After(60 * time.Second)
	start := time.Now()
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return fmt.Errorf("流断了（%s）", time.Since(start).Round(time.Millisecond))
			}
			line := ""
			if len(f.Data) > 0 {
				line = summarize(f.Data)
			}
			fmt.Printf("%3d %-18s %s\n", f.Seq, f.Type, line)
			if f.Type == "turn/done" || f.Type == "turn/error" {
				fmt.Printf("用时 %s\n", time.Since(start).Round(time.Millisecond))
				return nil
			}
		case <-sig:
			return fmt.Errorf("被中断")
		case <-deadline:
			return fmt.Errorf("60 秒没等到 turn/done——宿主没跑完，或订阅没收到帧")
		}
	}
}

// summarize 把一帧的 data 压成一行：正文取前 60 个字，别的给个键名。
func summarize(data json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return tail(data)
	}
	for _, k := range []string{"text", "content", "message", "name", "title"} {
		if v, ok := m[k].(string); ok && v != "" {
			return trunc(v, 60)
		}
	}
	b, _ := json.Marshal(m)
	return trunc(string(b), 80)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
