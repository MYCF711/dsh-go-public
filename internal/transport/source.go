package transport

import "errors"

// Source 是 transport 唯一的取数入口。
//
// 实现放在宿主那边（它才被允许读 v4 日志）；transport 自己**不 import 任何
// 会话日志的编解码包**（红线 R7），也不碰文件系统——这条线只做协议翻译。
type Source interface {
	// Messages 返回一个会话的全部消息，**必须按 Seq 升序**。
	Messages(sessionID string) ([]Message, error)
}

// PermissionSourcer 是**可选**能力：告诉协议层某个会话的审批策略。
//
// **这个字段原来是死的**：`LoadResult.Permission` 的注释写着「当前审批策略
// （deny/ask/allow/auto）」，但协议层从来没填过它（`res.Permission` 零命中），
// 所以界面上读到的永远是空串。注释说得越好听，越容易被当成"已经支持了"。
// GoalSourcer 是**可选**能力：告诉协议层某个会话当前在做什么。
//
// 和 Permission/Cwd 同款：`LoadResult` 有这个字段但可以没人填——
// 空串表示"这条会话没有设目标"，界面据此不画 GoalBar。
type GoalSourcer interface {
	Goal(sessionID string) (string, error)
}

type PermissionSourcer interface {
	Permission(sessionID string) (string, error)
}

// CwdSourcer 是**可选**能力：告诉协议层某个会话的工作目录。
// 不实现就不填，界面按"不知道"处理（不猜）。
type CwdSourcer interface {
	Cwd(sessionID string) (string, error)
}

var ErrNoSuchSession = errors.New("没有这个会话")

// ErrNotImplemented 表示这个宿主没实现某个可选能力。
// **要跟"没有这个会话"分开**：前者换个宿主能行，后者换参数才行。
var ErrNotImplemented = errors.New("这个宿主还没实现该能力")

// ErrNoSuchFile 由宿主在读文件时返回，协议层据此回 404。
// "文件不存在"和"读失败"在界面上长得一样，但前者该换路径、后者该重试。
var ErrNoSuchFile = errors.New("文件不存在")

// LoadSession 是 session.load 的实现：首屏，取最新一屏。
// TailSourcer 是 Source 的可选增强：只取最后 limit 条消息，同时给出总数。
//
// 与 SeqSourcer 同一个理由：`session.load` 只要首屏 50 条，
// 但"先投影全部再截尾"在 24k 事件的会话上要 480 ms。
type TailSourcer interface {
	MessagesTail(sessionID string, limit int) ([]Message, int, error)
}

// ErrNoSuchRequest 表示审批请求 id 不认识（已超时、已应答、或伪造）。
var ErrNoSuchRequest = errors.New("no_such_request")

// ErrBadOption 表示 optionId 不在这一轮的 options 里。
var ErrBadOption = errors.New("bad_option")

// UsageSourcer 是 Source 的可选增强：给出**整个会话**的累计用量与当前上下文占用。
//
// 为什么单独一个接口：tail 快路只读尾部，拿不到全会话统计；
// 把它交给宿主按需全量扫一次，比在 transport 里重读一遍文件更清楚。
type UsageSourcer interface {
	UsageTotal(sessionID string) (total UsageMsg, contextTokens int, err error)
}

// WindowSourcer 是 Source 的可选增强：给出这条会话模型的上限（token）。
// 说不出来就返回 0——界面会退化成"只报数字"，而不是画一根永远不准的进度条。
type WindowSourcer interface {
	ContextWindow(sessionID string) int
}

// ModelSourcer 是 Source 的可选增强：给出这条会话用的模型 id。
type ModelSourcer interface {
	Model(sessionID string) (string, error)
}

// EffortSourcer 是 Source 的可选增强：给出这条会话最新配置记录里的推理强度。
// 空串表示旧会话无记录、记录缺字段或枚举未知；不得由协议层补进程默认值。
type EffortSourcer interface {
	Effort(sessionID string) (string, error)
}

// EffortSelectionSourcer 给出独立 store 中的覆盖 key；空串只表示没有覆盖。
type EffortSelectionSourcer interface {
	EffortSelection(sessionID string) (string, error)
}

// Compactor 是 Source 的可选增强：能对会话做一次压缩（追加遮蔽事件）。
//
// 为什么是可选接口：压缩要读模型、要往日志追加，不是每个 Source 都能干
// （测试用的假 Source 就不该为了编译而实现它）。宿主没实现时，
// 协议层回一个**说清楚**的 501，而不是假装成功。
type Compactor interface {
	Compact(sessionID string, keep int) (CompactResult, error)
}

// DoCompact 是 session.compact 的实现。
func DoCompact(src Source, sessionID string, keep int) (CompactResult, error) {
	if src == nil {
		return CompactResult{}, errors.New("transport: DoCompact 需要 Source，传了 nil")
	}
	if sessionID == "" {
		return CompactResult{}, ErrNoSuchSession
	}
	c, ok := src.(Compactor)
	if !ok {
		return CompactResult{}, ErrNotImplemented
	}
	return c.Compact(sessionID, keep)
}

func LoadSession(src Source, sessionID string, limit int) (LoadResult, error) {
	if src == nil {
		return LoadResult{}, errors.New("transport: LoadSession 需要 Source，传了 nil")
	}
	if sessionID == "" {
		return LoadResult{}, ErrNoSuchSession
	}
	if ts, ok := src.(TailSourcer); ok {
		msgs, total, err := ts.MessagesTail(sessionID, limit)
		if err != nil {
			return LoadResult{}, err
		}
		// total < 0 表示宿主这次没算（大会话走尾部快路）。
		// 此时 truncated 必须是 true——"可能还有"是唯一保守的解释。
		trunc := true
		if total >= 0 {
			trunc = total > len(msgs)
		}
		res := LoadResult{Messages: msgs, Total: total, Truncated: trunc}
		if us, ok := src.(UsageSourcer); ok {
			if tot, ctx, uerr := us.UsageTotal(sessionID); uerr == nil {
				if tot != (UsageMsg{}) {
					res.UsageTotal = &tot
				}
				res.ContextTokens = ctx
			}
		}
		if ws, ok := src.(WindowSourcer); ok {
			if w := ws.ContextWindow(sessionID); w > 0 {
				res.ContextWindow = w
			}
		}
		if ms, ok := src.(ModelSourcer); ok {
			if m, merr := ms.Model(sessionID); merr == nil {
				res.Model = m
			}
		}
		if es, ok := src.(EffortSourcer); ok {
			if effort, eerr := es.Effort(sessionID); eerr == nil && effort != "" {
				res.Effort = effort
			}
		}
		if es, ok := src.(EffortSelectionSourcer); ok {
			selection, eerr := es.EffortSelection(sessionID)
			if eerr != nil {
				return LoadResult{}, eerr
			}
			if selection != "" {
				res.EffortSelection = selection
			}
		}
		if gs, ok := src.(GoalSourcer); ok {
			if goal, gerr := gs.Goal(sessionID); gerr == nil && goal != "" {
				res.Goal = goal
			}
		}
		if ps, ok := src.(PermissionSourcer); ok {
			if perm, perr := ps.Permission(sessionID); perr == nil && perm != "" {
				res.Permission = perm
			}
		}
		if cs, ok := src.(CwdSourcer); ok {
			if cwd, cerr := cs.Cwd(sessionID); cerr == nil && cwd != "" {
				res.CWD = cwd
			}
		}
		return res, nil
	}

	all, err := src.Messages(sessionID)
	if err != nil {
		return LoadResult{}, err
	}
	page := SelectPage(all, PageTail, 0, limit)
	res := LoadResult{
		Messages:  page.Messages,
		Total:     page.Total,
		Truncated: page.Truncated,
	}
	if us, ok := src.(UsageSourcer); ok {
		if tot, ctx, uerr := us.UsageTotal(sessionID); uerr == nil {
			if tot != (UsageMsg{}) {
				res.UsageTotal = &tot
			}
			res.ContextTokens = ctx
		}
	}
	if ms, ok := src.(ModelSourcer); ok {
		if m, merr := ms.Model(sessionID); merr == nil {
			res.Model = m
		}
	}
	if es, ok := src.(EffortSourcer); ok {
		if effort, eerr := es.Effort(sessionID); eerr == nil && effort != "" {
			res.Effort = effort
		}
	}
	if es, ok := src.(EffortSelectionSourcer); ok {
		selection, eerr := es.EffortSelection(sessionID)
		if eerr != nil {
			return LoadResult{}, eerr
		}
		if selection != "" {
			res.EffortSelection = selection
		}
	}
	return res, nil
}

// HistorySession 是 session.history 的实现：按 seq 增量补洞。
// SeqSourcer 是 Source 的可选增强：只取 seq > sinceSeq 的消息。
//
// 为什么需要它：`session.history` 是界面断线补洞时反复调用的 method。
// 只实现 Messages 的源会**先从全量事件投影出所有消息、再按 seq 过滤**——
// 实测最大的那份会话（24k 事件）即使结果为空也要 425 ms。
// 实现这个接口就能在投影前先掐掉不需要的事件。
type SeqSourcer interface {
	MessagesSince(sessionID string, sinceSeq int) ([]Message, error)
}

func HistorySession(src Source, sessionID string, sinceSeq int, limit int) (Page, error) {
	if src == nil {
		return Page{}, errors.New("transport: HistorySession 需要 Source，传了 nil")
	}
	if sessionID == "" {
		return Page{}, ErrNoSuchSession
	}
	// 能增量取就增量取：这是界面轮询最频繁的一条路。
	if ss, ok := src.(SeqSourcer); ok {
		newer, err := ss.MessagesSince(sessionID, sinceSeq)
		if err != nil {
			return Page{}, err
		}
		return SelectPage(newer, PageSince, sinceSeq, limit), nil
	}

	all, err := src.Messages(sessionID)
	if err != nil {
		return Page{}, err
	}
	return SelectPage(all, PageSince, sinceSeq, limit), nil
}
