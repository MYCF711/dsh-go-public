package spi

// FS 是文件系统接缝。所有工具都经它读写，这样两条策略只实现一次：
//
//  1. **路径归一**：一律正斜杠、相对工作区根，越界直接拒绝；
//  2. **读后写**（read-before-write）：文件存在时，必须先读过才能改；
//     读过之后文件又被别人改过，必须重新读——不允许盲覆盖。
//
// 工具自己不要实现这两条（AGENTS §3.7）。
// RootFollower 是 FS 的可选能力：把工作区根换成另一个目录。
//
// 客户端（ACP 的 session/new）可能带着自己的工作目录来，而 FS 的根是装配期定的。
// 实现这个接口的 FS 才能"跟着会话走"。
type RootFollower interface {
	SetRoot(root string) error
}

type FS interface {
	// Root 返回工作区根（正斜杠绝对路径）。
	Root() string
	// Resolve 把调用方给的路径归一成工作区内的绝对路径；越界返回错误。
	Resolve(path string) (string, error)
	// Read 读文件，并登记「已观测」；登记发生在读之前，宁可多要求一次重读。
	Read(path string) ([]byte, error)
	// CheckWritable 在写之前确认读后写策略。文件不存在时允许创建。
	//
	// op 是动词（"write" / "edit"），只影响报错措辞：
	// 真实语料里 edit 的失败 84% 来自这条策略，措辞要能让模型当场学会怎么改。
	CheckWritable(path, op string) error
	// Stat 查文件是否存在与大小（不改观测状态）。
	Stat(path string) (size int64, exists bool, err error)
	// Write 写文件（自动建父目录），成功后把新内容登记为已观测。
	Write(path string, data []byte) error
}
