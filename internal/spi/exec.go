package spi

import (
	"errors"
	"time"
)

// Exec 是命令执行接缝。按 R6：**真实 shell 进程**，不做进程内解释。
// CwdFollower 是 Exec 的可选能力：换掉"新会话的默认工作目录"。
//
// 和 FS 的 RootFollower 同一套思路：客户端带来的 cwd 要两边一起生效，
// 否则同一个会话里 read 在 B、bash 在 A。
type CwdFollower interface {
	SetCwd(dir string) error
}

type Exec interface {
	// Run 在持久 shell 里执行一条命令。
	//
	// idle 是「多久没有任何输出就转为后台任务」。返回的 JobID 非空表示已转后台，
	// 此时 Result 里是到目前为止的输出，任务继续跑，用 Job 服务取后续输出。
	Run(cmd string, idle time.Duration) (ExecResult, error)
}

// ExecResult 是一条命令的结果。
type ExecResult struct {
	Output    string `json:"output"` // stdout 与 stderr 合流（M1 的取舍，见 CHANGELOG）
	ExitCode  int    `json:"exit_code"`
	Cwd       string `json:"cwd"`
	JobID     string `json:"job_id,omitempty"`
	MovedToBg bool   `json:"moved_to_bg,omitempty"`
}

// Job 是后台任务表。
type Job interface {
	Output(id string, wait time.Duration) (JobOutput, error)
	Kill(id string) error
	List() []JobInfo
}

// JobOutput 是一次取输出的结果。
type JobOutput struct {
	ID       string `json:"id"`
	Output   string `json:"output"`
	Done     bool   `json:"done"`
	ExitCode int    `json:"exit_code"`
	Cwd      string `json:"cwd"`
}

// JobInfo 是任务清单里的一行。
type JobInfo struct {
	ID       string `json:"id"`
	Command  string `json:"command"`
	Done     bool   `json:"done"`
	ExitCode int    `json:"exit_code"`
	Bytes    int    `json:"bytes"`

	// OwnerID 隔离不同 Service 实例；任务 id 中也含这个不可猜测的实例代次。
	// SessionScope 是装配时的 kernel Scope 名；裸 NewService 没有外部会话 id，故可为空。
	OwnerID      string `json:"owner_id"`
	SessionScope string `json:"session_scope,omitempty"`
	RootPID      int    `json:"root_pid"`
	RootImage    string `json:"root_image"`
	// CreationID 是 OS 给出的进程创建代次（Windows FILETIME / Linux proc starttime /
	// Darwin sysctl start time），用来识别 PID 复用，不是进程名或自行生成的时间戳。
	CreationID string `json:"creation_id"`
	// ProcessTree 说明可验证的进程树归属机制；ProcessGroupID 仅 Unix 非零。
	ProcessTree    string `json:"process_tree"`
	ProcessGroupID int    `json:"process_group_id,omitempty"`
}

const (
	// JobDefaultPageLimit 是 OutputPage 的默认字节上限。
	JobDefaultPageLimit = 64 * 1024
	// JobMaxPageLimit 限制单次响应，防止调用方用超大 limit 制造内存/响应洪水。
	JobMaxPageLimit = 256 * 1024
	// JobMaxPageWait 限制一次长轮询；调用方可用 nextCursor 继续等。
	JobMaxPageWait = 30 * time.Second
)

var (
	ErrJobNotFound       = errors.New("没有任务")
	ErrJobWrongInstance  = errors.New("后台任务属于另一个 exec 服务实例")
	ErrJobFinished       = errors.New("后台任务已经结束或已请求停止")
	ErrJobProcessExited  = errors.New("后台任务根进程已经退出")
	ErrJobProcessReused  = errors.New("后台任务根 PID 的创建代次或映像不匹配")
	ErrJobInvalidCursor  = errors.New("后台任务输出游标无效")
	ErrServiceClosed     = errors.New("exec 服务已经关闭")
	ErrProcessTreeUnsafe = errors.New("无法验证进程树归属")
)

// JobPager 是有界增量输出能力。Job 保持原接口，调用方可按需断言这个扩展。
// cursor/limit 都按字节计；cursor 为 0 从头读，limit 为 0 用默认值。
type JobPager interface {
	OutputPage(id string, cursor int64, limit int, wait time.Duration) (JobOutputPage, error)
}

// JobController 是安全停止与真实元数据能力。跨实例 id 必须返回 ErrJobWrongInstance。
type JobController interface {
	Stop(id string) error
	Metadata(id string) (JobMetadata, error)
}

// JobOutputPage 是一页原始输出。Total 是产生过的总字节数（含被保留窗口淘汰的前缀），
// Offset/NextCursor 都是绝对字节游标。Truncated 表示请求的 cursor 之前已有字节被淘汰。
type JobOutputPage struct {
	ID         string `json:"id"`
	Output     string `json:"output"`
	Offset     int64  `json:"offset"`
	NextCursor int64  `json:"next_cursor"`
	Total      int64  `json:"total"`
	Truncated  bool   `json:"truncated"`
	Done       bool   `json:"done"`
	ExitCode   int    `json:"exit_code"`
	Cwd        string `json:"cwd,omitempty"`
}

// JobMetadata 是某任务此刻的真实快照；输出保留范围也明确暴露，不假装有完整历史。
type JobMetadata struct {
	JobInfo
	Cwd            string `json:"cwd,omitempty"`
	RetainedOffset int64  `json:"retained_offset"`
	Total          int64  `json:"total"`
	Truncated      bool   `json:"truncated"`
}
