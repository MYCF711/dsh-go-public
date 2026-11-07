package transport

// 官方流式方法里**不属于会话事件流**的两条：`workspaceFiles.changes` 与
// `toolResults.watch`。它们各有自己的真实数据源，所以各自一台小帧机
// （会话事件流那四条共用 official_stream.go 的 officialSessionStream）。
//
// # workspaceFiles.changes（官方 workspace-files/typert.host.js:160）
//
// 官方 `mode:'stream'`，参数 `(workspaceFileScope, path)`，wire 字段
// `workspaceFileScopeId` + `path`；帧是 `{kind:"ready"}` 之后每个失效
// `{kind:"change", change:{absolutePath, version} | {absolutePath, absent:true}}`。
//
// 数据源是 `OfficialWorkspaceAPI.FilesStat` 的**真实版本令牌**（宿主用真 stat
// 派生，客户端从不解析）。官方用 fs watch，本宿主没有 watch 入口，所以用
// **轮询 stat 比对 version**：语义完全一致（版本变了就是一次失效），只是延迟
// 从"内核通知"变成"一个轮询间隔"。这是显式差异，登记在 RESULT 里。
//
// 目录目标：官方支持"目录的直接子项"。本宿主 `FilesStat` 只对**普通文件**成立
// （目录回 `workspaceFiles/not-a-file`），而 `FilesList` 的结果里**没有目录绝对
// 路径**（官方帧必填 `absolutePath`）。所以目录目标明确回官方自己的
// `workspace-file/watch-unsupported` 并点名缺什么——**不拿工作区相对路径冒充
// 绝对路径**，那会让客户端把两套路径混用。
//
// # toolResults.watch（官方 dsh-api-remotes/lib/client.js:17415）
//
// 参数 `(sessionId, seq)`，帧是 `{available:false}` 或 `{available:true, value}`。
// 数据源是 `OfficialAgentAPI.ToolResultsWatch`：它读真实 durable `tool/result`
// 事件（cmd/dshd/serve_agent_api.go:1640），**冷读（结果已被淘汰/会话不在）
// 就回 available:false**，打开新流观察的是"当前这个实例"。
//
// 本宿主没有"结果被淘汰时通知"的机制，所以同样是轮询 + 去重：值变了才发一帧。
// 未接线（api.ToolResultsWatch == nil）时明确 501 点名缺什么，不返回假
// `{available:false}` 冒充"已实现但暂时没有"——两者对客户端的含义完全不同。

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// officialWorkspaceChange 是官方 `change` 的两种形状（version 与 absent 互斥）。
type officialWorkspaceChange struct {
	AbsolutePath string `json:"absolutePath"`
	Version      string `json:"version,omitempty"`
	Absent       bool   `json:"absent,omitempty"`
}

// officialWorkspaceWatchFrame 是 `workspaceFiles.changes` 的一帧。
type officialWorkspaceWatchFrame struct {
	Kind   string                   `json:"kind"`
	Change *officialWorkspaceChange `json:"change,omitempty"`
}

// officialWorkspaceChangesStream 是"跟随一个文件目标"的帧机。
type officialWorkspaceChangesStream struct {
	api       OfficialWorkspaceAPI
	sessionID string
	path      string

	opened bool
	// absolutePath 是**首次成功 stat** 拿到的绝对路径：目标后来消失时，
	// `absent` 帧仍然要带它（路径身份不因为文件没了而变）。
	absolutePath string
	version      string
	absent       bool
}

var _ OfficialFrameStream = (*officialWorkspaceChangesStream)(nil)

func (s *officialWorkspaceChangesStream) Close() {}

func (s *officialWorkspaceChangesStream) Next(ctx context.Context) (json.RawMessage, bool, *OfficialStreamFault) {
	if !s.opened {
		return s.open()
	}
	for {
		frame, more, fault := s.step()
		if fault != nil {
			return nil, false, fault
		}
		if more {
			return frame, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, true, nil
		case <-time.After(officialStreamPollInterval):
		}
	}
}

func (s *officialWorkspaceChangesStream) open() (json.RawMessage, bool, *OfficialStreamFault) {
	if s.api.FilesStat == nil {
		return nil, false, officialStreamNotImplemented(MethodWorkspaceFilesChanges,
			"宿主没有真实文件系统读入口（OfficialWorkspaceAPI.FilesStat 未接线）")
	}
	stat, err := s.api.FilesStat(s.sessionID, s.path)
	if err != nil {
		return nil, false, s.faultOf(err, true)
	}
	s.opened = true
	s.absolutePath = stat.AbsolutePath
	s.version = stat.Version
	// ready 的语义是"watch 已经生效"：这里指首帧 stat 成功、版本已经记下。
	// 之后任何版本变化都会是一帧 change。
	raw, fault := marshalStreamFrame(s.kind(), officialWorkspaceWatchFrame{Kind: "ready"})
	return raw, false, fault
}

func (s *officialWorkspaceChangesStream) step() (json.RawMessage, bool, *OfficialStreamFault) {
	stat, err := s.api.FilesStat(s.sessionID, s.path)
	if err != nil {
		// 目标没了：这是官方的 `absent` 帧，不是流错误。
		if isWorkspaceCode(err, "workspaceFiles/not-found") {
			if s.absent {
				return nil, false, nil
			}
			s.absent = true
			raw, fault := marshalStreamFrame(s.kind(), officialWorkspaceWatchFrame{
				Kind: "change",
				Change: &officialWorkspaceChange{
					AbsolutePath: s.absolutePath,
					Absent:       true,
				},
			})
			return raw, true, fault
		}
		// 其它错误（不可读、越界…）是**流失败**：官方 `follower.fail(error)`
		// 会把整条流打死，不继续发看不清的帧。
		return nil, false, s.faultOf(err, false)
	}
	// 目标回来了：先清掉 absent，再按版本变化发 change。
	s.absent = false
	if stat.Version == s.version && stat.AbsolutePath == s.absolutePath {
		return nil, false, nil
	}
	s.version = stat.Version
	s.absolutePath = stat.AbsolutePath
	raw, fault := marshalStreamFrame(s.kind(), officialWorkspaceWatchFrame{
		Kind: "change",
		Change: &officialWorkspaceChange{
			AbsolutePath: stat.AbsolutePath,
			Version:      stat.Version,
		},
	})
	return raw, true, fault
}

func (s *officialWorkspaceChangesStream) kind() Method { return MethodWorkspaceFilesChanges }

// faultOf 把宿主错误翻成流故障（目录目标单独一条：官方有专门码）。
func (s *officialWorkspaceChangesStream) faultOf(err error, opening bool) *OfficialStreamFault {
	switch {
	case isWorkspaceCode(err, "workspaceFiles/not-a-file"):
		return &OfficialStreamFault{Code: "workspace-file/watch-unsupported", Status: 501,
			Message: "workspaceFiles.changes 的目录目标未实现：官方要求目录帧带 absolutePath，" +
				"而本宿主 FilesList 只给工作区相对路径（不拿相对路径冒充绝对路径）；" +
				"目录 watch 来源见 RESULT 的接线清单"}
	case isWorkspaceCode(err, "workspaceFiles/not-found"):
		return &OfficialStreamFault{Code: "workspace-file/not-found", Status: 404,
			Message: "workspaceFiles.changes 的目标不存在：" + err.Error()}
	case errors.Is(err, ErrNoSuchSession):
		return officialStreamNotFound(MethodWorkspaceFilesChanges, err.Error())
	}
	status, code := 500, "workspace/internal"
	if typed := workspaceCodeOf(err); typed != "" {
		status, code = officialWorkspaceFaultStatus(typed)
	}
	_ = opening
	return &OfficialStreamFault{Code: code, Status: status, Message: err.Error()}
}

// officialToolResultFrame 是 `toolResults.watch` 的一帧（官方 available 判别式）。
type officialToolResultFrame struct {
	Available bool            `json:"available"`
	Value     json.RawMessage `json:"value,omitempty"`
}

// officialToolResultStream 是"观察一条已提交工具结果"的帧机。
type officialToolResultStream struct {
	api       OfficialAgentAPI
	sessionID string
	seq       int64

	opened bool
	// last 是上一次交付的载荷：只有**变化**才发一帧（官方的 watch 是变更流，
	// 不是心跳流）。
	last []byte
}

var _ OfficialFrameStream = (*officialToolResultStream)(nil)

func (s *officialToolResultStream) Close() {}

func (s *officialToolResultStream) Next(ctx context.Context) (json.RawMessage, bool, *OfficialStreamFault) {
	if !s.opened {
		if s.api.ToolResultsWatch == nil {
			return nil, false, officialStreamNotImplemented(MethodToolResultsWatch,
				"宿主没有 durable 工具结果读取（OfficialAgentAPI.ToolResultsWatch 未接线）")
		}
		s.opened = true
		frame, more, fault := s.sample()
		if fault != nil {
			return nil, false, fault
		}
		_ = more
		return frame, false, nil
	}
	for {
		frame, more, fault := s.sample()
		if fault != nil {
			return nil, false, fault
		}
		if more {
			return frame, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, true, nil
		case <-time.After(officialStreamPollInterval):
		}
	}
}

// sample 读一次真实来源，变化才产出帧。
//
// 首帧**永远**产出一帧（"打开新流观察的是当前实例"）：客户端据此知道
// "我看的是哪个 seq 的哪个可用状态"，而不是等一个可能永远不来的变化。
func (s *officialToolResultStream) sample() (json.RawMessage, bool, *OfficialStreamFault) {
	watch, err := s.api.ToolResultsWatch(s.sessionID, s.seq)
	if err != nil {
		return nil, false, officialStreamFaultOf(MethodToolResultsWatch, err)
	}
	frame := officialToolResultFrame{Available: watch.Available}
	if watch.Available {
		if len(watch.Value) == 0 {
			// available:true 却没有 value 是宿主 bug：官方 schema 要求 value 必填。
			return nil, false, officialStreamInternal(MethodToolResultsWatch,
				"宿主报 available:true 却没给 value（官方 available:true 分支 value 必填）")
		}
		frame.Value = append(json.RawMessage(nil), watch.Value...)
	}
	raw, merr := json.Marshal(frame)
	if merr != nil {
		return nil, false, officialStreamInternal(MethodToolResultsWatch, "序列化工具结果帧失败：%v", merr)
	}
	if s.last != nil && string(s.last) == string(raw) {
		return nil, false, nil
	}
	s.last = raw
	return raw, true, nil
}

// marshalStreamFrame 是流帧的唯一序列化点（失败一律 internal，不留半帧）。
//
// 只返回帧与故障：**"有没有新帧"是调用方的语义**（open 是 opening，
// step 是增量），不能由序列化函数替它回答——这里曾经返回 `more=false`，
// 结果每个 change 帧都被当成"没有新数据"丢掉，流永远只剩 ready。
func marshalStreamFrame(method Method, frame any) (json.RawMessage, *OfficialStreamFault) {
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, officialStreamInternal(method, "序列化流帧失败：%v", err)
	}
	return raw, nil
}

// workspaceCodeOf 取宿主错误的官方码（不是 OfficialWorkspaceFault 就返回空串）。
func workspaceCodeOf(err error) string {
	var fault *OfficialWorkspaceFault
	if errors.As(err, &fault) && fault != nil {
		return fault.Code
	}
	return ""
}

// isWorkspaceCode 判断宿主错误是不是某个官方码。
func isWorkspaceCode(err error, code string) bool {
	return workspaceCodeOf(err) == code
}
