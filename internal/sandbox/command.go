package sandbox

import (
	"fmt"
	"strings"
)

// CommandRequest 是 bash/pwsh 单次调用的策略输入。
// Command 只用于确认调用上下文完整；核心不解析 shell 文本、不推测文件或网络行为。
type CommandRequest struct {
	Call
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
}

// EvaluateCommand 在启动 shell 前返回 allow/deny/needs-approval。
// workspace-write 默认只允许工作区 cwd；危险全权升级未获批前不会返回 allow。
// 本函数不启动进程，也不提供 OS 隔离。生产接线必须在 ActionAllow 后仍执行
// 既有工具审批，并使用原进程树回收机制；Job Object/process group 不是路径沙盒。
func (p *Policy) EvaluateCommand(req CommandRequest) (Decision, error) {
	if strings.TrimSpace(req.Command) == "" {
		return Decision{}, fmt.Errorf("%w: command 为空", ErrMissingContext)
	}
	if strings.TrimSpace(req.Cwd) == "" {
		return Decision{}, fmt.Errorf("%w: cwd 为空", ErrMissingContext)
	}
	return p.evaluate(req.Call, OperationExecute, req.Cwd)
}
