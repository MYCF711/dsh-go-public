package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/llm"
)

// History 从 v4 事件里派生出**模型可见的历史**。
//
// 这是 surface 派生的最小版本（M4 会把它换成正投影）：
//   - 只有 5 类事件参与派生，其它一律跳过；
//   - `user/message` → user；`assistant/message` → assistant（含 tool-call 块）；
//   - `tool/result` → role:"tool"，带 toolCallId；
//   - `system/message` 目前不进历史（系统提示由装配期给），但要认得它。
func History(log *v4.Log) []llm.Message {
	surface := Surface(log)
	out := make([]llm.Message, 0, len(surface))
	for _, node := range surface {
		out = append(out, node.Message)
	}
	return out
}

func userMessage(raw json.RawMessage) (llm.Message, bool) {
	var d struct {
		Content []ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return llm.Message{}, false
	}
	return llm.Message{Role: "user", Content: joinText(d.Content)}, true
}

func assistantMessage(raw json.RawMessage) (llm.Message, bool) {
	// 取块是单点（ContentBlocks）：message.content 优先、顶层 content 兜底。
	// 只认前者的后果很隐蔽：**历史里少几条助手消息**，看起来像"那次模型没回答"，
	// 没人会怀疑到投影这一层。
	blocks, ok := ContentBlocks(raw, PreferNested)
	if !ok {
		return llm.Message{}, false
	}
	msg := llm.Message{Role: "assistant", Content: joinText(blocks)}
	for _, b := range blocks {
		if b.Type == "tool-call" {
			msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: llm.Func{Name: b.Name, Arguments: b.Arguments},
			})
		}
	}
	return msg, true
}

func toolMessage(raw json.RawMessage) (llm.Message, bool) {
	var d struct {
		Message struct {
			ToolCallID string         `json:"toolCallId"`
			Content    []ContentBlock `json:"content"`
			IsError    bool           `json:"isError"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return llm.Message{}, false
	}
	if d.Message.ToolCallID == "" {
		return llm.Message{}, false
	}
	return llm.Message{
		Role:       "tool",
		ToolCallID: d.Message.ToolCallID,
		Content:    joinText(d.Message.Content),
	}, true
}

func joinText(blocks []ContentBlock) string {
	out := ""
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += b.Text
	}
	return out
}

// OpenSession 打开一个已存在的会话（校验 header 后以 append 模式续写）。
func OpenSession(home, cwd, id string) (*Log, error) {
	path, err := SessionPath(home, cwd, id)
	if err != nil {
		return nil, err
	}
	return openSessionWriter(path, id)
}

// claimWriter 保证**同一时刻只有一个进程在写一份会话**。
//
// 为什么必须有：两个进程各自"读到 N 条 → 从 N 接着编号"，
// 写出来的日志里会**出现重复 seq**（实测：9 条事件 3 个重复）。
// seq 是 surface 与压缩遮蔽的排序依据，重复意味着顺序不确定——
// 这比"拒绝打开"糟糕得多。所以宁可明确拒绝。
func claimWriter(path string) error {
	lock := path + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("会话正被另一个进程写入（锁文件 %s）；"+
				"确认没有别的 dshd 在用这份会话后删掉它再 resume", lock)
		}
		return fmt.Errorf("建写锁失败: %w", err)
	}
	_, writeErr := fmt.Fprintf(f, "pid=%d\nat=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	closeErr := f.Close()
	if writeErr != nil {
		if removeErr := os.Remove(lock); removeErr != nil && !os.IsNotExist(removeErr) {
			writeErr = errors.Join(writeErr, fmt.Errorf("清理写锁失败: %w", removeErr))
		}
		return fmt.Errorf("写锁文件失败: %w", writeErr)
	}
	if closeErr != nil {
		if removeErr := os.Remove(lock); removeErr != nil && !os.IsNotExist(removeErr) {
			closeErr = errors.Join(closeErr, fmt.Errorf("清理写锁失败: %w", removeErr))
		}
		return fmt.Errorf("关闭锁文件失败: %w", closeErr)
	}
	return nil
}

func releaseWriter(path string) error {
	err := os.Remove(path + ".lock")
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func joinSessionUnlockError(err error, path string) error {
	if releaseErr := releaseWriter(path); releaseErr != nil {
		return errors.Join(err, fmt.Errorf("释放会话写锁 %s: %w", path+".lock", releaseErr))
	}
	return err
}

// SessionPath 按 v4 的目录规则算出会话文件路径。
func SessionPath(home, cwd, id string) (string, error) {
	seg, err := v4.EncodeSegment(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sessions", v4.ProjectKey(cwd), seg, "session.v4.jsonl.zstd"), nil
}

// OpenSessionAt 直接按文件路径续写（CLI 用：文件可能不在 <home>/sessions 布局里）。
func OpenSessionAt(path string) (*Log, error) {
	return openSessionWriter(path, "")
}

// openSessionWriter 是 OpenSession/OpenSessionAt 共用的唯一快照与加锁顺序。
// 必须先抢锁再读事件快照；否则锁前 ReadFile 会按旧 len(events) 发出重复 seq。
func openSessionWriter(path, expectedID string) (*Log, error) {
	if err := claimWriter(path); err != nil {
		return nil, err
	}
	back, err := v4.ReadFile(path)
	if err != nil {
		return nil, joinSessionUnlockError(err, path)
	}
	if expectedID != "" && back.Header.ID != expectedID {
		err := fmt.Errorf("文件里的会话 id 是 %q，与请求的 %q 不符", back.Header.ID, expectedID)
		return nil, joinSessionUnlockError(err, path)
	}
	w, err := v4.OpenAppend(path)
	if err != nil {
		// 这时候**文件是能读的**（上面 ReadFile 已经成功了），只是打不开写入。
		openErr := fmt.Errorf("会话能读、但打不开写入（%s）：%w；"+
			"文件只读/被占用/磁盘满都会这样。已有 %d 条事件不受影响，可以先用 `dshd session info --in` 查看",
			path, err, len(back.Events))
		return nil, joinSessionUnlockError(openErr, path)
	}
	lg := &Log{w: w, Path: path, seq: int64(len(back.Events)), now: time.Now}
	lg.lockPath = path + ".lock"
	return lg, nil
}
