// Package unlag 提供 `unlag_refresh` 工具：界面看起来卡住时，按一下刷新。
//
// 契约的关键只有一条：**同一个 request_id 只生效一次**。
// 模型重复调用不是错误，但它必须得到"已经刷过了"这个回答——
// 否则它会以为卡住没解决，继续刷，把本来没坏的东西刷成反复重建。
package unlag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

const name = "unlag_refresh"

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "request_id": {
      "type": "string",
      "description": "这次刷新的名字；同一个名字只生效一次。不传就用会话 id"
    }
  },
  "additionalProperties": false
}`)

// store 记"谁在什么时候刷过"。落盘而不是只放内存：
// serve 可能重启，重启后模型再刷同一个 request_id 应当还是"已经刷过了"。
type store struct {
	dir string
	mu  sync.Mutex
}

type entry struct {
	FirstAt string `json:"first_at"`
}

func (s *store) path() string { return filepath.Join(s.dir, "unlag.json") }

func (s *store) load() (map[string]entry, error) {
	b, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]entry{}, nil
		}
		return nil, err
	}
	m := map[string]entry{}
	if len(b) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *store) save(m map[string]entry) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(), b, 0o644)
}

// Tool 实现 spi.Tool。
type Tool struct{ st *store }

func (t *Tool) Name() string { return name }

func (t *Tool) Description() string {
	return "界面看起来卡住或不刷新时按一下；同一个 request_id 只生效一次，重复调用会告诉你已经刷过。"
}

func (t *Tool) Schema() json.RawMessage { return schema }

// Caps 全 false：这个工具不写工作区、不执行、不联网。
// 它只动 <home>/state 下自己的一个小文件，那是宿主状态，不是用户文件。
func (t *Tool) Caps() spi.Caps { return spi.Caps{} }

func (t *Tool) Exec(ctx context.Context, args json.RawMessage) (spi.Result, error) {
	var a struct {
		RequestID string `json:"request_id"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return spi.Result{Hint: `参数不合法：期望 {"request_id": "…"}，request_id 也可以不传`},
				fmt.Errorf("unlag_refresh 参数不是 JSON 对象: %w", err)
		}
	}

	sid := spi.SessionFrom(ctx)
	if a.RequestID == "" {
		a.RequestID = sid
	}
	if a.RequestID == "" {
		a.RequestID = "default"
	}
	key := sid + "::" + a.RequestID

	t.st.mu.Lock()
	defer t.st.mu.Unlock()

	m, err := t.st.load()
	if err != nil {
		return spi.Result{Hint: fmt.Sprintf("读不了刷新记录（%s）；确认 <home>/state 可写", t.st.path())},
			fmt.Errorf("读刷新记录失败: %w", err)
	}

	if first, ok := m[key]; ok {
		return spi.Result{
			Data:    map[string]any{"refreshed": false, "first_at": first.FirstAt},
			Summary: fmt.Sprintf("已经刷过了（第一次在 %s），没有重复生效", first.FirstAt),
		}, nil
	}

	now := time.Now().Format(time.RFC3339)
	m[key] = entry{FirstAt: now}
	if err := t.st.save(m); err != nil {
		return spi.Result{Hint: "写不了刷新记录；确认 <home>/state 可写"},
			fmt.Errorf("写刷新记录失败: %w", err)
	}
	return spi.Result{
		Data:    map[string]any{"refreshed": true, "first_at": now},
		Summary: fmt.Sprintf("已刷新（request_id=%s）", a.RequestID),
	}, nil
}

// Module 返回装配用的模块描述。dir 是状态目录（接线时传 <home>/state/unlag）。
func Module(dir string) kernel.Module {
	st := &store{dir: dir}
	return kernel.Module{
		ID:       "tool-unlag",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{st: st})
		},
	}
}
