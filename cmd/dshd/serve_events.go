package main

// 这一文件只剩"事件类型常量 + 负载形状 + follow 进度"。
// 发号与推送已经搬到 serve_journal.go（H14：seq 只有 journal 一个来源）。

// turnEvent 是一轮结束时推给界面的事件类型（WS 的 type 字段）。
//
// 只有两种，因为它们只回答一个问题：「这一轮结束了没有、成没成」。
// 中间过程（assistant/tool/usage 那些）不在这里造，那是会话日志的事。
const (
	TurnDone  = "turn/done"
	TurnError = "turn/error"
)

//
// 为什么不复用会话日志的 seq：日志 seq 是**持久事实**（落进 v4 文件、
// 参与 surface 与遮蔽的排序），而这里的 seq 只是这条连接上的送达顺序，
// 一次 serve 重启就该从 1 重来。两者一旦混用，界面就会拿不连续的日志
// seq 去补历史，补出来的是别的东西。

// turnDoneData 是 turn/done 的负载。字段名是界面契约，别改。
type turnDoneData struct {
	TurnID string `json:"turnId"`
}

// turnErrorData 是 turn/error 的负载。
//
// Error 是**错误原文**：不做归纳、不换成人话。界面要能把它原样显示给用户，
// 我们这一层一旦开始"翻译"，用户看到的就是二手信息，
// 而真正的形状（哪一步、哪个文件、哪个状态码）恰恰是最难复现的部分。
type turnErrorData struct {
	TurnID string `json:"turnId"`
	Error  string `json:"error"`
}

// followProgress 把 follow 走到了哪里交回来。只在 fw.Wait() 之后读，所以不需要锁。
type followProgress struct {
	offset  int64
	lastSeq int64
	path    string
}
