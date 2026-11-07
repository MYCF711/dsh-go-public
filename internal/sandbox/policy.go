// Package sandbox 实现会话级文件与命令策略判定。
//
// 本包不是 OS 沙盒：不启动进程、不授予工具审批，也不把 Windows Job Object、
// Unix process group 或审批管线当成文件/网络隔离。调用方必须在每次工具调用前
// 求值，并且只有 ActionAllow 才能进入既有审批与执行管线。所有一次性升级都只
// 存在于本次请求中；包内没有可写的全局模式或跨调用授权表。
package sandbox

import (
	"errors"
	"fmt"
	"strings"
)

// Mode 是会话的真实沙盒策略档位。
type Mode string

const (
	ModeReadOnly         Mode = "read-only"
	ModeWorkspaceWrite   Mode = "workspace-write"
	ModeDangerFullAccess Mode = "danger-full-access"
)

// Permission 是工具单次调用声明的 sandbox_permissions。空值表示不请求变化。
type Permission string

const (
	PermissionWorkspaceWrite   Permission = "workspace-write"
	PermissionDangerFullAccess Permission = "danger-full-access"
)

// Action 是策略层的执行闸门结果。
type Action string

const (
	ActionAllow         Action = "allow"
	ActionDeny          Action = "deny"
	ActionNeedsApproval Action = "needs-approval"
)

// Reason 是稳定、机器可读的判定原因。
type Reason string

const (
	ReasonWorkspaceRead           Reason = "workspace-read"
	ReasonWorkspaceWrite          Reason = "workspace-write"
	ReasonWorkspaceCommand        Reason = "workspace-command"
	ReasonExtraRead               Reason = "extra-read"
	ReasonHomeRead                Reason = "home-read"
	ReasonDangerFullAccess        Reason = "danger-full-access"
	ReasonReadOnlyWrite           Reason = "read-only-write-denied"
	ReasonReadOnlyExecute         Reason = "read-only-execute-denied"
	ReasonReadOnlyEscalation      Reason = "read-only-escalation-denied"
	ReasonExtraReadOnly           Reason = "extra-read-root-is-read-only"
	ReasonHomeReadOnly            Reason = "home-is-read-only"
	ReasonOutsideWorkspace        Reason = "outside-workspace-write"
	ReasonOutsideRead             Reason = "outside-read-boundary"
	ReasonCommandOutsideWorkspace Reason = "command-outside-workspace"
	ReasonEscalationRequired      Reason = "sandbox-upgrade-required"
	ReasonEscalationDenied        Reason = "sandbox-upgrade-denied"
)

// Enforcement 明确策略实际提供的隔离层级。
type Enforcement string

const (
	// EnforcementPolicyOnlyNoOSIsolation 表示只有工具前置策略，没有 OS 文件或网络隔离。
	EnforcementPolicyOnlyNoOSIsolation Enforcement = "policy-only-no-os-isolation"
)

// Operation 是被闸门的操作类型。
type Operation string

const (
	OperationRead    Operation = "read"
	OperationWrite   Operation = "write"
	OperationExecute Operation = "execute"
)

// ApprovalKind 标识需要审批的对象，避免与既有工具审批混为一谈。
type ApprovalKind string

const (
	ApprovalKindSandboxUpgrade ApprovalKind = "sandbox-upgrade"
)

var (
	ErrUnknownMode       = errors.New("未知 sandbox mode")
	ErrUnknownPermission = errors.New("未知 sandbox_permissions")
	ErrMissingContext    = errors.New("缺失 sandbox 策略上下文")
	ErrSessionMismatch   = errors.New("调用会话与策略会话不一致")
	ErrInvalidOperation  = errors.New("操作类型不适用于该策略入口")
	ErrInvalidRoot       = errors.New("sandbox 根目录无效")
	ErrInvalidPath       = errors.New("sandbox 路径无效")
	ErrPathOutsideHome   = errors.New("显式 home 路径越界")
	ErrApprovalBinding   = errors.New("一次性升级审批与调用不匹配")
)

// Config 是一个会话的不可变策略上下文。New 会 canonicalize 所有根目录。
type Config struct {
	SessionID      string
	Mode           Mode
	WorkspaceRoot  string
	ExtraReadRoots []string
	HomeDir        string
}

// Policy 不含可变授权状态，可安全并发处理同一会话的独立调用。
// 不同会话必须各自持有 Policy，禁止通过全局“当前权限”共享。
type Policy struct {
	sessionID string
	mode      Mode
	resolver  resolver
}

// New 校验并构造会话策略。未知 mode、缺失上下文和无效根目录都 fail-loud。
func New(cfg Config) (*Policy, error) {
	if strings.TrimSpace(cfg.SessionID) == "" {
		return nil, fmt.Errorf("%w: session id 为空", ErrMissingContext)
	}
	switch cfg.Mode {
	case ModeReadOnly, ModeWorkspaceWrite, ModeDangerFullAccess:
	default:
		return nil, fmt.Errorf("%w: %q（可选：read-only, workspace-write, danger-full-access）", ErrUnknownMode, cfg.Mode)
	}
	r, err := newResolver(cfg)
	if err != nil {
		return nil, err
	}
	return &Policy{sessionID: cfg.SessionID, mode: cfg.Mode, resolver: r}, nil
}

// Call 是每次工具调用的策略输入。Escalation 只能由真实审批管线填入，
// 不得从模型提供的 JSON 反序列化；sandbox_permissions 本身不是审批凭证。
type Call struct {
	SessionID          string              `json:"session_id"`
	CallID             string              `json:"call_id"`
	SandboxPermissions Permission          `json:"sandbox_permissions,omitempty"`
	Escalation         *EscalationApproval `json:"-"`
}

// ApprovalRequest 是 ActionNeedsApproval 时返回的结构化一次性审批需求。
// 它绑定会话、调用、操作和 canonical path，防止授权漂移到另一处。
type ApprovalRequest struct {
	Kind       ApprovalKind `json:"kind"`
	OneShot    bool         `json:"one_shot"`
	SessionID  string       `json:"session_id"`
	CallID     string       `json:"call_id"`
	Permission Permission   `json:"permission"`
	Operation  Operation    `json:"operation"`
	Path       string       `json:"path"`
	Reason     Reason       `json:"reason"`
}

// EscalationApproval 是审批管线对特定 ApprovalRequest 的答复。
// Approved=false 是明确拒绝；nil 指针才是尚未审批。
type EscalationApproval struct {
	ApprovalRequest
	Approved bool `json:"approved"`
}

// Decide 把结构化审批需求变成同样绑定范围的审批答复；不写入任何全局状态。
func (a ApprovalRequest) Decide(approved bool) *EscalationApproval {
	return &EscalationApproval{ApprovalRequest: a, Approved: approved}
}

// Decision 是一次调用的完整策略结果。
type Decision struct {
	Action              Action           `json:"action"`
	Reason              Reason           `json:"reason"`
	Mode                Mode             `json:"mode"`
	EffectiveMode       Mode             `json:"effective_mode"`
	RequestedPermission Permission       `json:"requested_permission,omitempty"`
	SessionID           string           `json:"session_id"`
	CallID              string           `json:"call_id"`
	Operation           Operation        `json:"operation"`
	Path                string           `json:"path"`
	CanonicalPath       string           `json:"canonical_path,omitempty"`
	Approval            *ApprovalRequest `json:"approval,omitempty"`
	EscalationEligible  bool             `json:"escalation_eligible,omitempty"`
	// ExistingApprovalPreserved=true 只表示本判定不取代既有工具审批，
	// 不表示工具已经获批；write/execute 仍须进入原审批管线。
	ExistingApprovalPreserved bool        `json:"existing_approval_preserved,omitempty"`
	Enforcement               Enforcement `json:"enforcement"`
}

func (p *Policy) evaluate(call Call, operation Operation, rawPath string) (Decision, error) {
	if p == nil {
		return Decision{}, fmt.Errorf("%w: nil policy", ErrMissingContext)
	}
	if strings.TrimSpace(call.SessionID) == "" || strings.TrimSpace(call.CallID) == "" {
		return Decision{}, fmt.Errorf("%w: session_id 与 call_id 均必填", ErrMissingContext)
	}
	if call.SessionID != p.sessionID {
		return Decision{}, fmt.Errorf("%w: policy=%q call=%q", ErrSessionMismatch, p.sessionID, call.SessionID)
	}
	switch p.mode {
	case ModeReadOnly, ModeWorkspaceWrite, ModeDangerFullAccess:
	default:
		return Decision{}, fmt.Errorf("%w: %q", ErrUnknownMode, p.mode)
	}
	switch operation {
	case OperationRead, OperationWrite, OperationExecute:
	default:
		return Decision{}, fmt.Errorf("%w: %q", ErrInvalidOperation, operation)
	}
	switch call.SandboxPermissions {
	case "", PermissionWorkspaceWrite, PermissionDangerFullAccess:
	default:
		return Decision{}, fmt.Errorf("%w: %q（可选：workspace-write, danger-full-access）", ErrUnknownPermission, call.SandboxPermissions)
	}
	if strings.TrimSpace(rawPath) == "" {
		return Decision{}, fmt.Errorf("%w: path/cwd 为空", ErrMissingContext)
	}
	canonical, err := p.resolver.resolve(rawPath)
	if err != nil {
		return Decision{}, err
	}

	d := Decision{
		Mode: p.mode, EffectiveMode: p.mode,
		RequestedPermission: call.SandboxPermissions,
		SessionID:           call.SessionID, CallID: call.CallID,
		Operation: operation, Path: rawPath, CanonicalPath: canonical,
		ExistingApprovalPreserved: operation == OperationWrite || operation == OperationExecute,
		Enforcement:               EnforcementPolicyOnlyNoOSIsolation,
	}

	// read-only 是硬上限：任何显式权限请求都不能放大它。
	if p.mode == ModeReadOnly && call.SandboxPermissions != "" {
		d.Action, d.Reason = ActionDeny, ReasonReadOnlyEscalation
		return d, nil
	}
	if call.Escalation != nil && (p.mode != ModeWorkspaceWrite || call.SandboxPermissions != PermissionDangerFullAccess) {
		return Decision{}, fmt.Errorf("%w: 只有 workspace-write 的 danger-full-access 请求可携带升级审批", ErrApprovalBinding)
	}

	if p.mode == ModeWorkspaceWrite && call.SandboxPermissions == PermissionDangerFullAccess {
		if call.Escalation == nil {
			d.Action, d.Reason = ActionNeedsApproval, ReasonEscalationRequired
			d.EscalationEligible = true
			d.Approval = &ApprovalRequest{
				Kind: ApprovalKindSandboxUpgrade, OneShot: true,
				SessionID: call.SessionID, CallID: call.CallID,
				Permission: PermissionDangerFullAccess, Operation: operation,
				Path: canonical, Reason: ReasonEscalationRequired,
			}
			return d, nil
		}
		if err := validateEscalation(call.Escalation, call, operation, canonical); err != nil {
			return Decision{}, err
		}
		if !call.Escalation.Approved {
			d.Action, d.Reason = ActionDeny, ReasonEscalationDenied
			return d, nil
		}
		d.Action, d.Reason = ActionAllow, ReasonDangerFullAccess
		d.EffectiveMode = ModeDangerFullAccess
		return d, nil
	}

	switch p.mode {
	case ModeDangerFullAccess:
		// danger-full-access 只解除本策略的路径边界；既有工具审批仍原样保留。
		d.Action, d.Reason = ActionAllow, ReasonDangerFullAccess
		return d, nil
	case ModeReadOnly:
		switch operation {
		case OperationWrite:
			d.Action, d.Reason = ActionDeny, ReasonReadOnlyWrite
			return d, nil
		case OperationExecute:
			d.Action, d.Reason = ActionDeny, ReasonReadOnlyExecute
			return d, nil
		}
	}

	class := p.resolver.classify(canonical)
	// 独立 home 只经显式 ~ 通道开放；若 home 与 workspace/extra 重合，
	// classify 已先归入那些原有通道，普通绝对路径的语义保持不变。
	if class == pathHome && !explicitHomePath(rawPath) {
		class = pathOutside
	}
	switch operation {
	case OperationRead:
		switch class {
		case pathWorkspace:
			d.Action, d.Reason = ActionAllow, ReasonWorkspaceRead
		case pathExtra:
			d.Action, d.Reason = ActionAllow, ReasonExtraRead
		case pathHome:
			d.Action, d.Reason = ActionAllow, ReasonHomeRead
		default:
			d.Action, d.Reason = ActionDeny, ReasonOutsideRead
			d.EscalationEligible = p.mode == ModeWorkspaceWrite
		}
	case OperationWrite:
		switch class {
		case pathWorkspace:
			d.Action, d.Reason = ActionAllow, ReasonWorkspaceWrite
		case pathExtra:
			d.Action, d.Reason = ActionDeny, ReasonExtraReadOnly
		case pathHome:
			d.Action, d.Reason = ActionDeny, ReasonHomeReadOnly
		default:
			d.Action, d.Reason = ActionDeny, ReasonOutsideWorkspace
		}
		if d.Action == ActionDeny {
			d.EscalationEligible = p.mode == ModeWorkspaceWrite
		}
	case OperationExecute:
		if class == pathWorkspace {
			d.Action, d.Reason = ActionAllow, ReasonWorkspaceCommand
		} else {
			d.Action, d.Reason = ActionDeny, ReasonCommandOutsideWorkspace
			d.EscalationEligible = p.mode == ModeWorkspaceWrite
		}
	}
	return d, nil
}

func validateEscalation(got *EscalationApproval, call Call, operation Operation, canonical string) error {
	if got == nil {
		return fmt.Errorf("%w: nil approval", ErrApprovalBinding)
	}
	a := got.ApprovalRequest
	if a.Kind != ApprovalKindSandboxUpgrade || !a.OneShot ||
		a.SessionID != call.SessionID || a.CallID != call.CallID ||
		a.Permission != PermissionDangerFullAccess || a.Operation != operation ||
		a.Path != canonical || a.Reason != ReasonEscalationRequired {
		return fmt.Errorf("%w: session/call/permission/operation/path 必须与原审批需求完全一致", ErrApprovalBinding)
	}
	return nil
}
