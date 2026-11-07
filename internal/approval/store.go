package approval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Store 把"这个会话已经批准过哪些工具"落到**我们自己的状态目录**。
//
// 为什么不写进 v4 会话日志：`approval/policy` 事件在 DSH 里的形状是
// `{"policy": "never", "source": ...}` —— **没有工具维度**。往里加 `tool` 键
// 属于自造字段，一旦 DSH 解析更严就会把整份日志当成坏的。
// 所以授权状态放旁边：会话日志保持 100% DSH 干净，我们的状态另存。
//
// 默认关闭：只有显式打开（`--remember-approvals`）才写盘——
// 让"跨进程记住授权"成为用户的选择，而不是默认行为。
type Store struct {
	Dir string // 例如 <home>/state/approvals
}

type record struct {
	Allowed   []string `json:"allowed"`
	UpdatedAt string   `json:"updatedAt"`
}

func (s *Store) path(sessionID string) (string, error) {
	name, err := safeName(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, name+".json"), nil
}

// Save 覆盖写一个会话的授权清单。
func (s *Store) Save(sessionID string, tools []string) error {
	if s.Dir == "" {
		return fmt.Errorf("没有配置状态目录")
	}
	p, err := s.path(sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	sorted := append([]string(nil), tools...)
	sort.Strings(sorted)
	b, err := json.MarshalIndent(record{Allowed: sorted, UpdatedAt: time.Now().Format(time.RFC3339)}, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p) // 原子替换
}

// Load 读回一个会话的授权清单；文件不存在返回空，不算错误。
func (s *Store) Load(sessionID string) ([]string, error) {
	if s.Dir == "" {
		return nil, nil
	}
	p, err := s.path(sessionID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		// 坏文件不该让整个会话起不来：当作没记过，顺便说清楚。
		return nil, fmt.Errorf("授权记录 %s 解析失败（按未授权处理）: %w", p, err)
	}
	return r.Allowed, nil
}

// Delete 删除一个会话的授权记录（会话关闭时调用）。
func (s *Store) Delete(sessionID string) error {
	if s.Dir == "" {
		return nil
	}
	p, err := s.path(sessionID)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// safeName 把会话 id 变成安全的文件名。
func safeName(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("会话 id 为空")
	}
	out := make([]rune, 0, len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	name := string(out)
	if name == "." || name == ".." {
		return "", fmt.Errorf("非法会话 id: %q", id)
	}
	return name, nil
}
