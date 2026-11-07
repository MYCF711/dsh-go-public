package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"dshgo/internal/spi"
)

// 生产接线用的环境契约。
//
// `dshd serve` 每一轮起一个 `dshd run` 子进程：子进程只服务一个会话、只跑一轮，
// 所以"这个会话当前是哪一档"由父进程在 spawn 时放进环境变量，子进程在装配期
// 解析。模块不读全局可变状态，也不接受"当前权限"单例——A/B 两个会话是两个
// 进程（或两份 Gate），各自解析各自的环境。
const (
	// EnvMode 是一个子进程的真实 sandbox 档位，只接受三档字面量。
	EnvMode = "DSHD_SANDBOX_MODE"
	// EnvWorkspaceRoot 是 canonical 工作区根；缺省用工具自己的 root。
	EnvWorkspaceRoot = "DSHD_SANDBOX_WORKSPACE"
	// EnvHomeDir 是显式 `~` 通道使用的宿主 home；缺省用 os.UserHomeDir。
	EnvHomeDir = "DSHD_SANDBOX_HOME"
	// EnvExtraReadRoots 是工作区外仍允许读的根（filepath.SplitList 分隔）。
	EnvExtraReadRoots = "DSHD_SANDBOX_EXTRA_READ"
)

var (
	// ErrReadOnlyWrite 表示 read-only 档拒绝一切写入（含工作区内新建）。
	ErrReadOnlyWrite = errors.New("read-only 会话拒绝一切写操作")
	// ErrWriteOutsideWorkspace 表示 workspace-write 档的 canonical 写边界越界。
	ErrWriteOutsideWorkspace = errors.New("写路径越出 canonical 工作区")
	// ErrNeedsApproval 表示一次性升级尚未获批，工具不得执行。
	ErrNeedsApproval = errors.New("sandbox 一次性升级需要审批")
	// ErrDenied 表示策略层明确拒绝，工具不得执行。
	ErrDenied = errors.New("sandbox 策略拒绝")
	// ErrPolicyUnwired 表示策略层没有接线，调用方却请求了 sandbox_permissions。
	ErrPolicyUnwired = errors.New("sandbox 策略层未接线")
	// ErrWidenDenied 表示请求的档位比会话 preset 更宽，必须换 preset 而不是改权限。
	ErrWidenDenied = errors.New("沙箱档位不得比会话 preset 更宽")
	// ErrOutsideReadBoundary 表示 canonical 路径越出读边界（FS Resolve 用）。
	ErrOutsideReadBoundary = errors.New("路径越出沙盒读边界")
)

// ParseMode 严格解析三档。空值、未知值和带空白的别名都 fail-loud，
// 绝不静默回落到某一档。
func ParseMode(raw string) (Mode, error) {
	// 不做 TrimSpace：带空白、大小写别名、空值都算未知取值。
	// 容忍"看起来像"的值会让磁盘/环境里的半截值静默生效。
	switch Mode(raw) {
	case ModeReadOnly:
		return ModeReadOnly, nil
	case ModeWorkspaceWrite:
		return ModeWorkspaceWrite, nil
	case ModeDangerFullAccess:
		return ModeDangerFullAccess, nil
	}
	if raw == "" {
		return "", fmt.Errorf("%w: 空值（可选：read-only, workspace-write, danger-full-access）", ErrUnknownMode)
	}
	return "", fmt.Errorf("%w: %q（可选：read-only, workspace-write, danger-full-access）", ErrUnknownMode, raw)
}

// ParsePermission 严格解析工具的单次 sandbox_permissions。空值表示不请求变化。
func ParsePermission(raw string) (Permission, error) {
	switch Permission(raw) {
	case "":
		return "", nil
	case PermissionWorkspaceWrite:
		return PermissionWorkspaceWrite, nil
	case PermissionDangerFullAccess:
		return PermissionDangerFullAccess, nil
	}
	return "", fmt.Errorf("%w: %q（可选：workspace-write, danger-full-access）", ErrUnknownPermission, raw)
}

var modeRank = map[Mode]int{
	ModeReadOnly:         0,
	ModeWorkspaceWrite:   1,
	ModeDangerFullAccess: 2,
}

// ValidateMode 报告一个档位是否是三档之一。
func ValidateMode(mode Mode) error {
	if _, ok := modeRank[mode]; !ok {
		return fmt.Errorf("%w: %q（可选：read-only, workspace-write, danger-full-access）", ErrUnknownMode, mode)
	}
	return nil
}

// MostRestrictive 返回两个合法档位中更窄的那个。
// 用于"恢复/fork 后权限不得变大"：任何一方更窄，结果就更窄。
func MostRestrictive(a, b Mode) (Mode, error) {
	if err := ValidateMode(a); err != nil {
		return "", err
	}
	if err := ValidateMode(b); err != nil {
		return "", err
	}
	if modeRank[a] <= modeRank[b] {
		return a, nil
	}
	return b, nil
}

// Boundary 是 FS 侧的路径策略：只有 mode + canonical 工作区，没有会话身份。
//
// 为什么不是 Policy：`spi.FS` 的 Read/Write/CheckWritable 都没有 ctx，拿不到
// 会话 id；而生产上每一轮是一个独立子进程，档位在进程级就是确定的。Boundary
// 只回答"这个 canonical 路径能不能写"，因此可以被 FS 在每次写前同步调用。
type Boundary struct {
	mode      Mode
	workspace string
}

// NewBoundary 校验档位并 canonicalize 工作区根。未知档位 fail-loud。
func NewBoundary(mode Mode, workspaceRoot string) (*Boundary, error) {
	if err := ValidateMode(mode); err != nil {
		return nil, err
	}
	workspace, err := canonicalRoot("workspace", workspaceRoot)
	if err != nil {
		return nil, err
	}
	return &Boundary{mode: mode, workspace: workspace}, nil
}

// Mode 返回这个 boundary 的档位。
func (b *Boundary) Mode() Mode {
	if b == nil {
		return ""
	}
	return b.mode
}

// WorkspaceRoot 返回 canonical 工作区根。
func (b *Boundary) WorkspaceRoot() string {
	if b == nil {
		return ""
	}
	return b.workspace
}

// CheckWrite 在真正写盘前判定 canonical 写边界。
//
//   - read-only：一律拒绝，包括工作区内新建；
//   - workspace-write：只允许 canonical workspace 内部；
//   - danger-full-access：不虚构路径沙盒，但调用方仍须保留读后写/防陈旧覆盖。
//
// 返回的 canonical 路径来自 core 的 canonicalPath：已存在目标与"经同一父目录
// 新建"使用同一条规则，符号链接/reparse point 会被解析后再判断。
func (b *Boundary) CheckWrite(rawPath string) (string, error) {
	if b == nil {
		return "", fmt.Errorf("%w: nil boundary", ErrMissingContext)
	}
	path := rawPath
	if !filepath.IsAbs(filepath.FromSlash(path)) {
		path = filepath.Join(b.workspace, filepath.FromSlash(path))
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	switch b.mode {
	case ModeReadOnly:
		return canonical, fmt.Errorf("%w: %q（工作区内新建也不允许）", ErrReadOnlyWrite, rawPath)
	case ModeWorkspaceWrite:
		if !withinRoot(b.workspace, canonical) {
			return canonical, fmt.Errorf("%w: %q canonical=%q workspace=%q",
				ErrWriteOutsideWorkspace, rawPath, canonical, b.workspace)
		}
	case ModeDangerFullAccess:
		// 明确不提供路径沙盒；只由显式选择该档的会话/调用进入。
	default:
		return canonical, fmt.Errorf("%w: %q", ErrUnknownMode, b.mode)
	}
	return canonical, nil
}

// BoundaryFromEnv 从进程环境解析 FS 边界。
//
// 环境里没有 DSHD_SANDBOX_MODE 时回落到 workspace-write：这与 FS 既有的
// "只写工作区"边界一致，是收窄而不是放宽，因此不需要靠 fail-loud 拒绝启动。
// 一旦设了环境变量，任何未知值都 fail-loud。
func BoundaryFromEnv(workspaceRoot string) (*Boundary, error) {
	mode := ModeWorkspaceWrite
	if raw := strings.TrimSpace(os.Getenv(EnvMode)); raw != "" {
		parsed, err := ParseMode(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvMode, err)
		}
		mode = parsed
	}
	workspace := strings.TrimSpace(os.Getenv(EnvWorkspaceRoot))
	if workspace == "" {
		workspace = workspaceRoot
	}
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("%w: 工作区根为空", ErrMissingContext)
	}
	return NewBoundary(mode, workspace)
}

// ProcessConfig 是一个进程（生产上=一个会话的一轮）的策略上下文。
type ProcessConfig struct {
	Mode           Mode
	WorkspaceRoot  string
	HomeDir        string
	ExtraReadRoots []string
}

// Gate 是命令类工具的策略闸门：会话 id 从每次调用的 ctx 取，档位来自进程配置。
//
// Gate 不含可变授权状态。一次性升级审批只从 ctx 读（见 WithEscalation），
// 工具参数里的 sandbox_permissions 本身不是凭证。
type Gate struct {
	cfg ProcessConfig

	mu       sync.Mutex
	policies map[string]*Policy
}

// NewGate 校验进程策略上下文。未知档位、空/无效根都 fail-loud。
func NewGate(cfg ProcessConfig) (*Gate, error) {
	if err := ValidateMode(cfg.Mode); err != nil {
		return nil, err
	}
	if _, err := canonicalRoot("workspace", cfg.WorkspaceRoot); err != nil {
		return nil, err
	}
	if _, err := canonicalRoot("home", cfg.HomeDir); err != nil {
		return nil, err
	}
	for _, root := range cfg.ExtraReadRoots {
		if _, err := canonicalRoot("extra read", root); err != nil {
			return nil, err
		}
	}
	return &Gate{cfg: cfg, policies: make(map[string]*Policy)}, nil
}

// Mode 返回闸门的进程级档位。
func (g *Gate) Mode() Mode {
	if g == nil {
		return ""
	}
	return g.cfg.Mode
}

func (g *Gate) policyFor(sessionID string) (*Policy, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.policies[sessionID]; ok {
		return p, nil
	}
	p, err := New(Config{
		SessionID:      sessionID,
		Mode:           g.cfg.Mode,
		WorkspaceRoot:  g.cfg.WorkspaceRoot,
		ExtraReadRoots: append([]string(nil), g.cfg.ExtraReadRoots...),
		HomeDir:        g.cfg.HomeDir,
	})
	if err != nil {
		return nil, err
	}
	g.policies[sessionID] = p
	return p, nil
}

// NextCallID 生成一次工具调用的稳定身份。
//
// 它只在进程内唯一；审批绑定真正依赖的是 ApprovalRequest 里的 session/call/
// operation/canonical path，而不是这个字符串本身的可猜测性。
var callSeq atomic.Uint64

// NextCallID 生成形如 `<prefix>-<pid>-<seq>` 的调用标识。
func NextCallID(prefix string) string {
	if strings.TrimSpace(prefix) == "" {
		prefix = "call"
	}
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), callSeq.Add(1))
}

type escalationCtxKey struct{}

// WithEscalation 把审批管线对某次一次性升级请求的答复绑到 ctx 上。
// 只有真实审批结果才允许调用；模型参数不得伪造这个值。
func WithEscalation(ctx context.Context, approval *EscalationApproval) context.Context {
	if ctx == nil || approval == nil {
		return ctx
	}
	return context.WithValue(ctx, escalationCtxKey{}, approval)
}

// EscalationFrom 取本次调用携带的一次性升级答复；没有返回 nil。
func EscalationFrom(ctx context.Context) *EscalationApproval {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(escalationCtxKey{}).(*EscalationApproval)
	return a
}

func callFromContext(ctx context.Context, prefix string, permission Permission) (Call, error) {
	sessionID := strings.TrimSpace(spi.SessionFrom(ctx))
	if sessionID == "" {
		return Call{}, fmt.Errorf("%w: 工具调用缺少会话上下文（spi.WithSession）", ErrMissingContext)
	}
	call := Call{
		SessionID:          sessionID,
		CallID:             NextCallID(prefix),
		SandboxPermissions: permission,
		Escalation:         EscalationFrom(ctx),
	}
	if call.Escalation != nil {
		// 重试的是一次已审批调用：把绑定参数对齐到审批需求本身，
		// 由 validateEscalation 逐项核对（session/call/permission/operation/path）。
		call.CallID = call.Escalation.CallID
	}
	return call, nil
}

// EvaluateCommand 在启动 shell 之前判定一次命令调用。
// 返回 ActionNeedsApproval 时调用方不得启动进程，也不得留下 job。
func (g *Gate) EvaluateCommand(ctx context.Context, command, cwd string, permission Permission) (Decision, error) {
	if g == nil {
		return Decision{}, fmt.Errorf("%w: nil gate", ErrMissingContext)
	}
	call, err := callFromContext(ctx, "cmd", permission)
	if err != nil {
		return Decision{}, err
	}
	p, err := g.policyFor(call.SessionID)
	if err != nil {
		return Decision{}, err
	}
	return p.EvaluateCommand(CommandRequest{Call: call, Command: command, Cwd: cwd})
}

// EvaluateFS 在写/读之前判定一次文件调用。
// 当前生产 write/edit/read 工具没有 ctx（spi.FS 签名），FS 侧走 Boundary；
// 这个入口供将来带 ctx 的调用方（以及需要一次性升级的文件调用）使用。
func (g *Gate) EvaluateFS(ctx context.Context, operation Operation, path string, permission Permission) (Decision, error) {
	if g == nil {
		return Decision{}, fmt.Errorf("%w: nil gate", ErrMissingContext)
	}
	call, err := callFromContext(ctx, "fs", permission)
	if err != nil {
		return Decision{}, err
	}
	p, err := g.policyFor(call.SessionID)
	if err != nil {
		return Decision{}, err
	}
	return p.EvaluateFS(FSRequest{Call: call, Operation: operation, Path: path})
}

// GateFromEnv 从进程环境解析命令策略闸门。
//
// 环境里没有 DSHD_SANDBOX_MODE 时返回 (nil, nil)：这表示**策略层尚未接线**，
// 旧行为保持可用；但任何显式 sandbox_permissions 请求都会 fail-loud（见工具层），
// 不会静默当作放行。设了未知值则返回错误，模块装配期就炸。
func GateFromEnv(fallbackWorkspace string) (*Gate, error) {
	raw := strings.TrimSpace(os.Getenv(EnvMode))
	if raw == "" {
		return nil, nil
	}
	mode, err := ParseMode(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvMode, err)
	}
	workspace := strings.TrimSpace(os.Getenv(EnvWorkspaceRoot))
	if workspace == "" {
		workspace = fallbackWorkspace
	}
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("%w: 工作区根为空", ErrMissingContext)
	}
	home := strings.TrimSpace(os.Getenv(EnvHomeDir))
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("%w: 解析宿主 home: %v", ErrMissingContext, err)
		}
	}
	var extra []string
	if rawExtra := strings.TrimSpace(os.Getenv(EnvExtraReadRoots)); rawExtra != "" {
		extra = filepath.SplitList(rawExtra)
	}
	return NewGate(ProcessConfig{
		Mode: mode, WorkspaceRoot: workspace, HomeDir: home, ExtraReadRoots: extra,
	})
}

// Error 把非 allow 判定变成可 errors.Is 的错误。
// 调用方仍必须把完整 Decision 放进结构化结果，不能只回一句话。
func (d Decision) Error() error {
	switch d.Action {
	case ActionDeny:
		return fmt.Errorf("%w: reason=%s path=%q", ErrDenied, d.Reason, d.CanonicalPath)
	case ActionNeedsApproval:
		return fmt.Errorf("%w: reason=%s path=%q", ErrNeedsApproval, d.Reason, d.CanonicalPath)
	case ActionAllow:
		return nil
	default:
		return fmt.Errorf("%w: 未知 action %q", ErrDenied, d.Action)
	}
}

// CanonicalPath 暴露 core 的 canonical 规则：解析最深已存在祖先的符号链接/
// reparse point，再拼回尚不存在的后缀。FS 用它对齐 extra read 根与只读比较。
func CanonicalPath(path string) (string, error) {
	return canonicalPath(path)
}

// CompareModes 按宽松程度比较两档：a 更宽返回正数，更窄返回负数，相同返回 0。
// 调用方用它做"权限不得变大"的单调性检查。
func CompareModes(a, b Mode) (int, error) {
	if err := ValidateMode(a); err != nil {
		return 0, err
	}
	if err := ValidateMode(b); err != nil {
		return 0, err
	}
	return modeRank[a] - modeRank[b], nil
}
