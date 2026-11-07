package llm

// 0.2.2 对未声明容量的自定义模型使用十进制默认值，不是二进制 MiB/KiB。
const (
	DefaultCustomContextWindow = 1_000_000
	DefaultCustomMaxOutput     = 128_000
)

// ModelCapacity 是模型的上下文窗口与单次最大输出 token 数。
//
// 零值字段表示调用方没有显式声明该字段；它不是 Transport 的容量字段，
// 因而不会把 Transport 的 0=未知改成猜测值。
type ModelCapacity struct {
	ContextWindow int
	MaxOutput     int
}

// builtInModelCapacity 只列当前宿主已有的内置已知模型。
//
// 本仓没有上游 pi-ai 模型目录，models.list 也只有 id/owned_by；因此不能把
// 网关返回的任意名字冒充成“已知容量”。其余非空 --model 自由文本在当前
// 声明面里就是未声明自定义模型。这里只保存兼容基线，不做模型路由。
func builtInModelCapacity(modelID string) (ModelCapacity, bool) {
	switch modelID {
	case "group/deepseek-v4.1-flash-1017":
		return ModelCapacity{
			ContextWindow: 128_000,
			MaxOutput:     0, // 保持原行为：常规请求不新增 max_tokens。
		}, true
	default:
		return ModelCapacity{}, false
	}
}

// ResolveModelCapacity 是宿主唯一的模型容量解析点。
//
// 显式声明的正值逐字段优先，包括较小端点的 32K/8K 等覆盖。只有非内置、
// 非空的自由文本模型才补 1M/128K 默认值；内置模型保留原容量，空模型 ID
// 仍表示未知。函数不缓存任何模型或会话状态，交错解析不会互相串值。
func ResolveModelCapacity(modelID string, declared ModelCapacity) ModelCapacity {
	capacity, known := builtInModelCapacity(modelID)
	if !known {
		if modelID == "" {
			return declared
		}
		capacity = ModelCapacity{
			ContextWindow: DefaultCustomContextWindow,
			MaxOutput:     DefaultCustomMaxOutput,
		}
	}
	if declared.ContextWindow > 0 {
		capacity.ContextWindow = declared.ContextWindow
	}
	if declared.MaxOutput > 0 {
		capacity.MaxOutput = declared.MaxOutput
	}
	return capacity
}
