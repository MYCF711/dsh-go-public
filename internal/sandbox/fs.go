package sandbox

import "fmt"

// FSRequest 是一次 read/write/edit 类文件调用的策略输入。
// edit 在执行前以 OperationWrite 传入；本包不取代 FS 的读后写和防陈旧覆盖。
type FSRequest struct {
	Call
	Operation Operation `json:"operation"`
	Path      string    `json:"path"`
}

// EvaluateFS 判定工作区写、extra 只读、home 只读与显式危险全权。
// 它只返回决策，不读写文件；ActionAllow 后才可进入既有 FS 与工具审批管线。
func (p *Policy) EvaluateFS(req FSRequest) (Decision, error) {
	if req.Operation != OperationRead && req.Operation != OperationWrite {
		return Decision{}, fmt.Errorf("%w: FS 只接受 read/write，实际为 %q", ErrInvalidOperation, req.Operation)
	}
	return p.evaluate(req.Call, req.Operation, req.Path)
}
