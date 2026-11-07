// Package approval 记住"这个会话里，这个工具已经批准过了"。
//
// 作用域刻意做成「进程内 + 按会话」：
//   - 不落盘：resume 之后要重新批准。宁可多问一次，也不要让一个旧会话带着
//     别人留下的授权继续改文件；
//   - 按工具：批准 `read` 不等于批准 `shell`。
package approval

import "sync"

// Memory 是审批记忆。
type Memory struct {
	mu sync.RWMutex
	m  map[string]map[string]bool // sessionID → tool → 已永远允许
}

// New 造一个空的记忆。
func New() *Memory {
	return &Memory{m: map[string]map[string]bool{}}
}

// Allowed 判断这个会话里的这个工具是否已经被永久批准。
func (x *Memory) Allowed(session, tool string) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	set, ok := x.m[session]
	if !ok {
		return false
	}
	return set[tool]
}

// Remember 记下"以后这个工具都允许"。
func (x *Memory) Remember(session, tool string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.m == nil {
		x.m = map[string]map[string]bool{}
	}
	set, ok := x.m[session]
	if !ok {
		set = map[string]bool{}
		x.m[session] = set
	}
	set[tool] = true
}

// Forget 清掉一个会话的记忆（会话关闭时调用）。
func (x *Memory) Forget(session string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.m, session)
}

// Snapshot 返回某个会话已经记住的工具（诊断用）。
func (x *Memory) Snapshot(session string) []string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	set := x.m[session]
	out := make([]string, 0, len(set))
	for tool := range set {
		out = append(out, tool)
	}
	return out
}
