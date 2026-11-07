package transport

// 本文件是官方桌面端 0.2.2-rc.1「workspace / workspaceFiles / directoryPicker」
// 16 个 typert 方法的 L1 语义实现。
//
// 对齐口径（见 docs/official-desktop-api-parity.md §2）：
//   - **L1**：参数与结果**字段**按官方 zod 契约；wire 仍走本仓既有的
//     `POST /api/<method>` + 裸参数对象 + `{"result":…}` / 四字段 error。
//   - **L2**（`remote.mux` 多路复用 + typert Connection envelope）**未实现**，
//     所以本文不声称与官方桌面端 wire 兼容。
//
// 契约原件：
//   - `@deepseek-ai/dsh-api-workspace-controller/lib/typert.host.js:32-240`（参数/结果 zod）
//     与 `:245-665`（descriptor：`workspace/create`、`rename`、`delete`、
//     `insertBefore`、`insertSessionBefore`、`pinSession`、`unpinSession`、
//     `archiveSession`、`unarchiveSession`、`directoryPicker/list`、
//     `directoryPicker/createDirectory`）
//   - `@deepseek-ai/dsh-api-workspace-files/lib/typert.host.js:23-155`（参数/结果 zod）
//     与 `:157-456`（descriptor：`workspaceFiles/list|read|readBytes|stat|changes`）
//
// 本文件只做协议翻译与形状校验：**不读盘、不认识 registry 文件、不认识 v4 会话**
// （红线 R7）。所有真实状态由宿主经 `OfficialWorkspaceAPI` /
// `OfficialDirectoryPickerAPI` 注入；某个能力宿主没接线时回**四字段的
// not_implemented** 并点名方法，绝不返回假数据、假目录列表、假 watch 帧。
//
// 接线的唯一入口是 `IsOfficialWorkspaceMethod` + `(*Server).serveOfficialWorkspaceAPI`：
// `cmd/dshd` 侧 `AttachOfficialWorkspaceAPI` / `AttachOfficialDirectoryPickerAPI`
// 注入实现（见 `cmd/dshd/serve_workspace_api.go`），`internal/transport/server.go`
// 的 /api 路由在既有 switch 之前调用一次即可（见 RESULT 的接线清单）。
//
// # 与官方 wire 的两处显式差异（都写进 RESULT，不静默）
//
//  1. `workspaceFiles/readBytes`：官方 result 的 `data` 是 `Uint8Array`（typert 有
//     二进制 encode）。本仓 L1 的 JSON 只能表达字符串，因此 `data` 是 **base64**，
//     并额外给 `encoding:"base64"` 明确标注——不把 base64 冒充成官方原地字节。
//  2. `workspaceFiles/changes`：官方是 `mode:'stream'`（typert.host.js:157-164）。
//     HTTP 一次性响应无法承载持续 watch，因此如实回 501 并说明实时面只能走
//     每会话 WS journal；**不返回假的 `ready` 帧**。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

// 官方方法名。字符串必须与官方 `createTypertRemoteInfo` 里 `'workspace/create'`
// 这类键逐字对应（本仓把 `/` 换成 `.`，与既有 58 个方法同形）。
const (
	MethodWorkspaceCreate              Method = "workspace.create"
	MethodWorkspaceRename              Method = "workspace.rename"
	MethodWorkspaceDelete              Method = "workspace.delete"
	MethodWorkspaceInsertBefore        Method = "workspace.insertBefore"
	MethodWorkspaceInsertSessionBefore Method = "workspace.insertSessionBefore"
	MethodWorkspacePinSession          Method = "workspace.pinSession"
	MethodWorkspaceUnpinSession        Method = "workspace.unpinSession"
	MethodWorkspaceArchiveSession      Method = "workspace.archiveSession"
	MethodWorkspaceUnarchiveSession    Method = "workspace.unarchiveSession"

	MethodWorkspaceFilesList      Method = "workspaceFiles.list"
	MethodWorkspaceFilesRead      Method = "workspaceFiles.read"
	MethodWorkspaceFilesReadBytes Method = "workspaceFiles.readBytes"
	MethodWorkspaceFilesStat      Method = "workspaceFiles.stat"
	MethodWorkspaceFilesChanges   Method = "workspaceFiles.changes"

	MethodDirectoryPickerList            Method = "directoryPicker.list"
	MethodDirectoryPickerCreateDirectory Method = "directoryPicker.createDirectory"
)

// OfficialWorkspaceMethods 按命名空间分组返回本文件实现的 16 个方法（顺序固定）。
//
// 它给快照/文档/门禁用：新增或删除一个方法必须显式改这里，不能让
// "方法悄悄多了一个"从路由表里漏过去。
func OfficialWorkspaceMethods() []Method {
	return []Method{
		MethodWorkspaceCreate, MethodWorkspaceRename, MethodWorkspaceDelete,
		MethodWorkspaceInsertBefore, MethodWorkspaceInsertSessionBefore,
		MethodWorkspacePinSession, MethodWorkspaceUnpinSession,
		MethodWorkspaceArchiveSession, MethodWorkspaceUnarchiveSession,
		MethodWorkspaceFilesList, MethodWorkspaceFilesRead, MethodWorkspaceFilesReadBytes,
		MethodWorkspaceFilesStat, MethodWorkspaceFilesChanges,
		MethodDirectoryPickerList, MethodDirectoryPickerCreateDirectory,
	}
}

// IsOfficialWorkspaceMethod 判断一个 /api 路径名是否属于本文件的 16 个方法。
//
// 注意 `workspace.list` / `workspace.init` 是**宿主独有**面（canonical 25 与
// metadata init 扩展），不属于官方 workspace 族，这里必须认领不到。
func IsOfficialWorkspaceMethod(name string) bool {
	switch Method(name) {
	case MethodWorkspaceCreate, MethodWorkspaceRename, MethodWorkspaceDelete,
		MethodWorkspaceInsertBefore, MethodWorkspaceInsertSessionBefore,
		MethodWorkspacePinSession, MethodWorkspaceUnpinSession,
		MethodWorkspaceArchiveSession, MethodWorkspaceUnarchiveSession,
		MethodWorkspaceFilesList, MethodWorkspaceFilesRead, MethodWorkspaceFilesReadBytes,
		MethodWorkspaceFilesStat, MethodWorkspaceFilesChanges,
		MethodDirectoryPickerList, MethodDirectoryPickerCreateDirectory:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 官方的数据结构（字段名/可选性逐条照 zod 契约）
// ---------------------------------------------------------------------------

// OfficialWorkspace 是官方 `WorkspaceView`（workspaceController 的
// `sessionIds` 是"按手动顺序记在这个工作区名下的会话"）。
type OfficialWorkspace struct {
	WorkspaceID string   `json:"workspaceId"`
	Path        string   `json:"path"`
	Title       string   `json:"title"`
	SessionIDs  []string `json:"sessionIds"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

// OfficialWorkspaceCreateValue 是 `workspace/create` 的结果：`created` 区分
// "这次真的注册了"与"幂等命中已存在的注册"。
type OfficialWorkspaceCreateValue struct {
	Workspace OfficialWorkspace `json:"workspace"`
	Created   bool              `json:"created"`
}

// OfficialWorkspaceValue 是 rename / insertSessionBefore 的结果。
type OfficialWorkspaceValue struct {
	Workspace OfficialWorkspace `json:"workspace"`
}

// OfficialWorkspaceDeleteValue 是 `workspace/delete` 的结果（`deleted` 是 literal true）。
type OfficialWorkspaceDeleteValue struct {
	Deleted bool `json:"deleted"`
}

// OfficialWorkspaceOrderValue 是 `workspace/insertBefore` 的结果：**完整**顺序。
type OfficialWorkspaceOrderValue struct {
	WorkspaceIDs []string `json:"workspaceIds"`
}

// OfficialWorkspacePinValue 是 pin/unpin 的结果：完整的 pin 集，最新在前。
type OfficialWorkspacePinValue struct {
	PinnedSessionIDs []string `json:"pinnedSessionIds"`
}

// OfficialWorkspaceArchiveValue 是 archive/unarchive 的结果：完整的归档集。
type OfficialWorkspaceArchiveValue struct {
	ArchivedSessionIDs []string `json:"archivedSessionIds"`
}

// OfficialWorkspaceDirectoryEntry 是 `workspaceFiles/list` 的一行。
//
// `Type` 只取 `file` / `directory` / `other`（官方 zod 是三个 literal 的 union）；
// `Size` 只在"普通文件且后端报得出大小"时出现，所以它是指针。
type OfficialWorkspaceDirectoryEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size *int64 `json:"size,omitempty"`
}

// OfficialWorkspaceDirectoryListing 是 `workspaceFiles/list` 的结果。
//
// `Path` 是**工作区相对路径**（根是空串），子项路径 = 它 + `/` + name。
type OfficialWorkspaceDirectoryListing struct {
	Path      string                          `json:"path"`
	Entries   []OfficialWorkspaceDirectoryEntry `json:"entries"`
	Truncated bool                            `json:"truncated"`
}

// OfficialWorkspaceFileStat 是 stat/read/readBytes 共同的头部字段。
//
// `Version` 是"不透明的新鲜度令牌"，客户端不许解析；宿主用真实 stat 派生。
type OfficialWorkspaceFileStat struct {
	AbsolutePath string `json:"absolutePath"`
	Version      string `json:"version"`
	Bytes        *int64 `json:"bytes,omitempty"`

	// IsDirectory 标记这次 stat 命中的是**目录**。它只服务
	// `workspaceFiles.changes` 的目录目标：官方 watch 允许目标是"文件或目录"
	// （`Watch one file or a directory's direct entries`），而目录帧要的正是
	// `{absolutePath, version}`。它**不进 wire**（json:"-"），`workspaceFiles.stat`
	// 仍按官方契约拒绝非普通文件——见 officialWorkspaceFilesStat 的拒绝，
	// 客户端不会拿到"目录被 stat 成文件"的答复。
	IsDirectory bool `json:"-"`
}

// OfficialWorkspaceFileText 是 `workspaceFiles/read` 的结果。
type OfficialWorkspaceFileText struct {
	Offset       int64  `json:"offset"`
	Text         string `json:"text"`
	Lines        int64  `json:"lines"`
	EOF          bool   `json:"eof"`
	AbsolutePath string `json:"absolutePath"`
	Version      string `json:"version"`
	Bytes        *int64 `json:"bytes,omitempty"`
}

// OfficialWorkspaceFileBytes 是 `workspaceFiles/readBytes` 的结果。
//
// **显式差异**：官方 `data` 是 `Uint8Array`；本仓 L1 JSON 用 base64 表达，
// 并用 `encoding:"base64"` 标注。客户端不许把 `data` 当二进制裸串。
type OfficialWorkspaceFileBytes struct {
	Offset       int64  `json:"offset"`
	Data         string `json:"data"`
	Encoding     string `json:"encoding"`
	EOF          bool   `json:"eof"`
	AbsolutePath string `json:"absolutePath"`
	Version      string `json:"version"`
	Bytes        *int64 `json:"bytes,omitempty"`
}

// OfficialWorkspaceFileRange 是 read 的行窗口（官方 `WorkspaceFileRange`）。
//
// HasOffset/HasLimit 保留"没给"与"给了默认值"的区别：官方 offset 默认 1、
// limit 默认（且不得超过）宿主的 maxLines，宿主需要知道自己是被省略还是被显式给值。
type OfficialWorkspaceFileRange struct {
	Offset    int64
	Limit     int64
	HasOffset bool
	HasLimit  bool
}

// OfficialWorkspaceByteRange 是 readBytes 的字节窗口（官方 `WorkspaceByteRange`）。
type OfficialWorkspaceByteRange struct {
	Offset *int64
	Length *int64
}

// OfficialWorkspaceByteReadOptions 是 readBytes 的可选参数
// （官方 `WorkspaceByteReadOptions`：`range` + `baseFile`）。
type OfficialWorkspaceByteReadOptions struct {
	Range    *OfficialWorkspaceByteRange
	BaseFile *string
}

// OfficialDirectoryItem 是 directoryPicker 的 crumb/entry 同一形状：
// `name` / `path` / `hidden`（官方 `DirectoryListing` 的两个数组共用它）。
type OfficialDirectoryItem struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Hidden bool   `json:"hidden"`
}

// OfficialDirectoryListing 是 `directoryPicker/list` 的结果。
type OfficialDirectoryListing struct {
	Path      string                `json:"path"`
	Home      string                `json:"home"`
	Crumbs    []OfficialDirectoryItem `json:"crumbs"`
	Entries   []OfficialDirectoryItem `json:"entries"`
	Truncated bool                  `json:"truncated"`
}

// ---------------------------------------------------------------------------
// 挂载点
// ---------------------------------------------------------------------------

// OfficialWorkspaceAPI 是宿主侧的真实实现集合。**每个字段都可以为 nil**：
// 为 nil 表示这个能力在**当前宿主**上没有真实支撑，调用回 not_implemented
// 并点名方法——不是静默成功，也不是假数据。
//
// 为什么用"每方法一个函数"而不是一个 interface：这些方法分属两个官方包
// （workspace-controller / workspace-files），真实来源也不同（registry 文件 /
// v4 会话存储 / 真实文件系统），硬塞进一个 interface 只会逼调用方写空实现。
type OfficialWorkspaceAPI struct {
	Create              func(path string) (OfficialWorkspaceCreateValue, error)
	Rename              func(workspaceID, title string) (OfficialWorkspace, error)
	Delete              func(workspaceID string) error
	InsertBefore        func(workspaceID, beforeWorkspaceID string) ([]string, error)
	InsertSessionBefore func(workspaceID, sessionID, beforeSessionID string) (OfficialWorkspace, error)
	PinSession          func(sessionID string) ([]string, error)
	UnpinSession        func(sessionID string) ([]string, error)
	ArchiveSession      func(sessionID string, stopActivity bool) ([]string, error)
	UnarchiveSession    func(sessionID string) ([]string, error)

	FilesList      func(sessionID, path string) (OfficialWorkspaceDirectoryListing, error)
	FilesRead      func(sessionID, path string, r OfficialWorkspaceFileRange) (OfficialWorkspaceFileText, error)
	FilesReadBytes func(sessionID, path string, o OfficialWorkspaceByteReadOptions) (OfficialWorkspaceFileBytes, error)
	// FilesStat 是 stat 与 workspaceFiles.changes **共用**的目标 stat：
	// 普通文件返回 absolutePath/version/bytes；目录返回 absolutePath/version 并置
	// IsDirectory（目录目标只在 changes 里合法，unary stat 由 transport 层拒绝）。
	// 目标不存在时回 workspaceFiles/not-found（changes 据此发 absent 帧）。
	FilesStat func(sessionID, path string) (OfficialWorkspaceFileStat, error)
}

// OfficialDirectoryPickerAPI 是官方 `directoryPicker` 的浏览后端
// （官方原生选择器在桌面端；L1 只实现 browse 两个动词）。
type OfficialDirectoryPickerAPI struct {
	// List 列一层目录；path 为空表示宿主账户的 home（官方 acceptsUndefined:true）。
	List func(path string) (OfficialDirectoryListing, error)
	// CreateDirectory 在一个**绝对存在的父目录**下建一个子目录，返回新目录绝对路径。
	CreateDirectory func(path, name string) (string, error)
}

// officialWorkspaceMount 把两份实现挂到一个 Server 上。
//
// 为什么不用 `Options` 加字段：Options 在 server.go（本任务的写作用域之外，
// 由 Lead 统一接线）。按 *Server 索引的意义与 agent 侧那张表一致：两个 Server
// （A/B）各挂各的，互不可见，也不会出现进程级单例被共享。
var officialWorkspaceMount = struct {
	mu     sync.RWMutex
	api    map[*Server]OfficialWorkspaceAPI
	picker map[*Server]OfficialDirectoryPickerAPI
}{
	api:    map[*Server]OfficialWorkspaceAPI{},
	picker: map[*Server]OfficialDirectoryPickerAPI{},
}

// AttachOfficialWorkspaceAPI 给这个 Server 挂上官方 workspace 族的真实实现。
// 必须发生在服务开始接受 /api 调用之前（serve 的装配阶段）。
func (s *Server) AttachOfficialWorkspaceAPI(api OfficialWorkspaceAPI) {
	if s == nil {
		return
	}
	officialWorkspaceMount.mu.Lock()
	defer officialWorkspaceMount.mu.Unlock()
	officialWorkspaceMount.api[s] = api
}

// DetachOfficialWorkspaceAPI 摘掉实现（测试收尾用；生产不需要调）。
func (s *Server) DetachOfficialWorkspaceAPI() {
	if s == nil {
		return
	}
	officialWorkspaceMount.mu.Lock()
	defer officialWorkspaceMount.mu.Unlock()
	delete(officialWorkspaceMount.api, s)
	delete(officialWorkspaceMount.picker, s)
}

// AttachOfficialDirectoryPickerAPI 给这个 Server 挂上 directoryPicker 浏览后端。
func (s *Server) AttachOfficialDirectoryPickerAPI(picker OfficialDirectoryPickerAPI) {
	if s == nil {
		return
	}
	officialWorkspaceMount.mu.Lock()
	defer officialWorkspaceMount.mu.Unlock()
	officialWorkspaceMount.picker[s] = picker
}

// DetachOfficialDirectoryPickerAPI 摘掉 directoryPicker 后端（测试收尾用）。
func (s *Server) DetachOfficialDirectoryPickerAPI() {
	if s == nil {
		return
	}
	officialWorkspaceMount.mu.Lock()
	defer officialWorkspaceMount.mu.Unlock()
	delete(officialWorkspaceMount.picker, s)
}

func (s *Server) officialWorkspaceMounts() (OfficialWorkspaceAPI, OfficialDirectoryPickerAPI) {
	if s == nil {
		return OfficialWorkspaceAPI{}, OfficialDirectoryPickerAPI{}
	}
	officialWorkspaceMount.mu.RLock()
	defer officialWorkspaceMount.mu.RUnlock()
	return officialWorkspaceMount.api[s], officialWorkspaceMount.picker[s]
}

// ---------------------------------------------------------------------------
// 错误契约
// ---------------------------------------------------------------------------

// OfficialWorkspaceFault 是宿主侧的"官方错误"：`Code` 用官方词表
// （`workspace/not-found`、`workspace/session-active`、`directory-picker/exists`…），
// `Message` 是给人看的那句话。
//
// 为什么需要它：本仓的四字段 code 是**闭合集合**（见 server.go 的 errorHint /
// retryableFor），不能因为官方有 12 个码就长 12 个新码。所以官方码放在 message
// 前缀里（客户端按官方码分支），HTTP 状态与四字段 code 由下表统一映射。
type OfficialWorkspaceFault struct {
	Code    string
	Message string
}

func (f *OfficialWorkspaceFault) Error() string {
	if f == nil {
		return ""
	}
	if f.Message == "" {
		return f.Code
	}
	return f.Code + ": " + f.Message
}

// OfficialWorkspaceError 造一条官方错误（宿主侧唯一构造点）。
func OfficialWorkspaceError(code, format string, args ...any) error {
	return &OfficialWorkspaceFault{Code: code, Message: fmt.Sprintf(format, args...)}
}

// officialWorkspaceFaultStatus 是官方码 → (HTTP 状态, 四字段 code) 的**唯一**映射表。
//
// 判据是"客户端下一步怎么做"：
//   - 改输入才能过（非法路径/重名/不能移动/目录不可读…）→ 400 bad_request
//   - 目标不在了（工作区/会话/文件）                            → 404 not_found
//   - 真实状态不允许（会话还在跑）                              → 409 revision_conflict
//   - 宿主没这个能力                                            → 501 not_implemented
//   - 其它（宿主自己的 bug）                                    → 500 internal
func officialWorkspaceFaultStatus(code string) (status int, fourcode string) {
	switch code {
	case "workspace/not-found", "session/not-found", "workspaceFiles/not-found":
		return http.StatusNotFound, "not_found"
	case "workspace/session-active":
		return http.StatusConflict, "revision_conflict"
	case "workspace/unsupported", "not_implemented":
		return http.StatusNotImplemented, "not_implemented"
	case "workspace/internal", "gateway/internal", "internal":
		return http.StatusInternalServerError, "internal"
	}
	// 其余官方码（invalid-path / name-conflict / move-invalid / gateway/bad-request /
	// directory-picker/* / workspaceFiles/outside-workspace …）都是**输入问题**。
	return http.StatusBadRequest, "bad_request"
}

// officialWorkspaceFault 把宿主错误翻成 `*officialError`（本包唯一的四字段出口
// writeOfficialError 认得它）。
func officialWorkspaceFault(err error) error {
	if err == nil {
		return nil
	}
	// 运输层自己造的错误（参数校验/未接线）已经是 typed 的，原样透传。
	var typed *officialError
	if errors.As(err, &typed) {
		return err
	}
	// 宿主也可以直接给"通用哨兵"，按通用语义映射（与 agent 侧一致）。
	switch {
	case errors.Is(err, ErrOfficialNotImplemented):
		return officialNotImplemented("%s", err.Error())
	case errors.Is(err, ErrOfficialConflict):
		return officialRevisionConflict("%s", err.Error())
	case errors.Is(err, ErrOfficialBadInput):
		return officialBadRequest("%s", err.Error())
	case errors.Is(err, ErrNoSuchSession):
		return officialNoSuchSession(err.Error())
	}
	var fault *OfficialWorkspaceFault
	if errors.As(err, &fault) {
		status, code := officialWorkspaceFaultStatus(fault.Code)
		return &officialError{status: status, code: code, msg: fault.Error()}
	}
	// 认不出的错误一律按宿主内部问题报：它绝不能被渲染成成功。
	return officialInternal("%s", err.Error())
}

// ---------------------------------------------------------------------------
// 分发
// ---------------------------------------------------------------------------

// ServeOfficialWorkspace 处理本文件的 16 个方法。
//
// 它是纯函数（实现从参数进来，不从 *Server 上取），因此宿主侧测试可以直接打它，
// 而 `(*Server).serveOfficialWorkspaceAPI` 只是"取挂载 + 调它"的一层壳。
// 认证、POST 校验、失败日志都已经在 handleAPI 里做过。
func ServeOfficialWorkspace(w http.ResponseWriter, r *http.Request, name string, api OfficialWorkspaceAPI, picker OfficialDirectoryPickerAPI) {
	if !IsOfficialWorkspaceMethod(name) {
		writeJSON(w, http.StatusNotImplemented, errorBodyFor("not_implemented",
			fmt.Sprintf("不是官方 workspace 族的方法：%q", name)))
		return
	}
	params, err := decodeOfficialParams(r)
	if err != nil {
		writeOfficialError(w, err)
		return
	}
	result, err := routeOfficialWorkspaceMethod(api, picker, Method(name), params)
	if err != nil {
		writeOfficialError(w, officialWorkspaceFault(err))
		return
	}
	if result == nil {
		// 成功但无载荷时给出空对象，而不是 null：本仓出口不接受 {"result":null}。
		result = map[string]any{}
	}
	writeJSON(w, http.StatusOK, result)
}

// serveOfficialWorkspaceAPI 处理本文件的 16 个方法（Lead 接线入口）。
//
// 调用方（server.go 的 /api 路由）只需在既有 switch 之前判断
// `IsOfficialWorkspaceMethod(name)` 并调用它。
func (s *Server) serveOfficialWorkspaceAPI(w http.ResponseWriter, r *http.Request, name string) {
	// 流式方法（`workspaceFiles.changes`）走 NDJSON 长连接：**先于** unary 路由，
	// 因为它的结果不是"一次响应一个 result"，而是"ready + 一串 change 帧"。
	// 参数/能力错误仍在写响应头之前返回（四字段 + 状态码），不是 200 之后报错。
	if IsOfficialStreamMethod(name) {
		s.serveOfficialStream(w, r, name)
		return
	}
	api, picker := s.officialWorkspaceMounts()
	ServeOfficialWorkspace(w, r, name, api, picker)
}

// routeOfficialWorkspaceMethod 是大 switch，放在本文件里而不是 server.go：
// 官方方法还会继续加，路由表却是冻结的。
func routeOfficialWorkspaceMethod(api OfficialWorkspaceAPI, picker OfficialDirectoryPickerAPI, method Method, p *officialParams) (any, error) {
	switch method {
	case MethodWorkspaceCreate:
		return officialWorkspaceCreate(api, p)
	case MethodWorkspaceRename:
		return officialWorkspaceRename(api, p)
	case MethodWorkspaceDelete:
		return officialWorkspaceDelete(api, p)
	case MethodWorkspaceInsertBefore:
		return officialWorkspaceInsertBefore(api, p)
	case MethodWorkspaceInsertSessionBefore:
		return officialWorkspaceInsertSessionBefore(api, p)
	case MethodWorkspacePinSession, MethodWorkspaceUnpinSession:
		return officialWorkspacePin(api, method, p)
	case MethodWorkspaceArchiveSession:
		return officialWorkspaceArchive(api, p)
	case MethodWorkspaceUnarchiveSession:
		return officialWorkspaceUnarchive(api, p)
	case MethodWorkspaceFilesList:
		return officialWorkspaceFilesList(api, p)
	case MethodWorkspaceFilesRead:
		return officialWorkspaceFilesRead(api, p)
	case MethodWorkspaceFilesReadBytes:
		return officialWorkspaceFilesReadBytes(api, p)
	case MethodWorkspaceFilesStat:
		return officialWorkspaceFilesStat(api, p)
	case MethodWorkspaceFilesChanges:
		return officialWorkspaceFilesChanges(p)
	case MethodDirectoryPickerList:
		return officialDirectoryPickerList(picker, p)
	case MethodDirectoryPickerCreateDirectory:
		return officialDirectoryPickerCreateDirectory(picker, p)
	}
	return nil, officialNotImplemented("未知的官方 workspace 方法 %q", method)
}

// ---------------------------------------------------------------------------
// 参数解析
// ---------------------------------------------------------------------------

// officialPickObject 决定本次调用用"嵌套对象"还是"平铺字段"，并返回解析器。
//
// 官方 wire 的参数是一个位置参数对象（`request` / `range` / `options`），本仓
// 既有调用方习惯把它们平铺在顶层。两种写法都认；同一个字段两种写法都给 → 400
// （不猜哪个是真的）。返回的 object 只装本次实际给出的那批键，`finish()`
// 负责拒掉不认识的字段——官方 strictCodec 是 `additionalProperties:false`。
func officialPickObject(p *officialParams, name string, flatKeys []string) (*officialObject, error) {
	nested, hasNested, err := p.object(name)
	if err != nil {
		return nil, err
	}
	if !hasNested {
		flat := make(map[string]json.RawMessage, len(flatKeys))
		for _, key := range flatKeys {
			if raw, ok := p.take(key); ok {
				flat[key] = raw
			}
		}
		return newOfficialObject("参数", flat), nil
	}
	for _, key := range flatKeys {
		if p.has(key) {
			return nil, officialBadRequest("%s 与顶级的 %s 不能同时给（不猜哪个是真的）", name, key)
		}
	}
	return newOfficialObject(name, nested), nil
}

// officialWorkspaceIDParam 取必填的 workspaceId（非空）。
func officialWorkspaceIDParam(o *officialObject, owner string) (string, error) {
	id, has, err := o.str("workspaceId")
	if err != nil {
		return "", err
	}
	if !has {
		return "", officialBadRequest("缺 %s（工作区 id）", owner)
	}
	if id == "" {
		return "", officialBadRequest("%s 是空串", owner)
	}
	return id, nil
}

// officialSessionIDParam 取必填的 sessionId（非空）。
func officialSessionIDParam(o *officialObject, owner string) (string, error) {
	id, has, err := o.str("sessionId")
	if err != nil {
		return "", err
	}
	if !has {
		return "", officialBadRequest("缺 %s（会话 id）", owner)
	}
	if id == "" {
		return "", officialBadRequest("%s 是空串", owner)
	}
	return id, nil
}

// officialFileScope 取 workspaceFiles 的 scope（官方是 lookup 参数，wire 名
// `workspaceFileScopeId`）。scope 不成立就没有工作区根，不能拿进程默认目录顶上。
func officialFileScope(p *officialParams) (string, error) {
	id, _, err := p.strAlias("workspaceFileScopeId", "sessionId", "agentId")
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", officialBadRequest("workspaceFileScopeId 是空串（它是会话身份，不能缺）")
	}
	return id, nil
}

// officialFilePath 取 workspaceFiles 的必填 path（可以为空串：空 = 工作区根）。
//
// 这些方法的 path 是 typert 的**位置参数**（顶层），不裹在对象里，所以直从这里取。
func officialFilePath(p *officialParams) (string, error) {
	path, has, err := p.takeStr("path")
	if err != nil {
		return "", err
	}
	if !has {
		return "", officialBadRequest("缺 path（工作区内的路径；根目录给空串）")
	}
	return path, nil
}

// ---------------------------------------------------------------------------
// workspace.*
// ---------------------------------------------------------------------------

func officialWorkspaceCreate(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	o, err := officialPickObject(p, "request", []string{"path"})
	if err != nil {
		return nil, err
	}
	path, has, err := o.str("path")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !has {
		return nil, officialBadRequest("缺 path（要注册的目录绝对路径）")
	}
	if strings.TrimSpace(path) == "" {
		return nil, officialBadRequest("path 是空串")
	}
	if api.Create == nil {
		return nil, officialNotImplemented("workspace.create 未接线：宿主没有工作区注册表（workspace registry）")
	}
	return api.Create(path)
}

func officialWorkspaceRename(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	o, err := officialPickObject(p, "request", []string{"workspaceId", "title"})
	if err != nil {
		return nil, err
	}
	workspaceID, err := officialWorkspaceIDParam(o, "request.workspaceId")
	if err != nil {
		return nil, err
	}
	title, hasTitle, err := o.str("title")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasTitle {
		return nil, officialBadRequest("缺 request.title")
	}
	// 官方在 controller 里 trim 后判空（`gateway/bad-request`），这里先拒，
	// 免得把"改成一个看不见的名字"落到磁盘上。
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, officialBadRequest("title 是空串（官方要求非空白标题）")
	}
	if api.Rename == nil {
		return nil, officialNotImplemented("workspace.rename 未接线：宿主没有工作区注册表")
	}
	ws, err := api.Rename(workspaceID, title)
	if err != nil {
		return nil, err
	}
	return OfficialWorkspaceValue{Workspace: officialNormalizeWorkspace(ws)}, nil
}

func officialWorkspaceDelete(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	o, err := officialPickObject(p, "request", []string{"workspaceId"})
	if err != nil {
		return nil, err
	}
	workspaceID, err := officialWorkspaceIDParam(o, "request.workspaceId")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.Delete == nil {
		return nil, officialNotImplemented("workspace.delete 未接线：宿主没有工作区注册表")
	}
	if err := api.Delete(workspaceID); err != nil {
		return nil, err
	}
	return OfficialWorkspaceDeleteValue{Deleted: true}, nil
}

func officialWorkspaceInsertBefore(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	o, err := officialPickObject(p, "request", []string{"workspaceId", "beforeWorkspaceId"})
	if err != nil {
		return nil, err
	}
	workspaceID, err := officialWorkspaceIDParam(o, "request.workspaceId")
	if err != nil {
		return nil, err
	}
	before, hasBefore, err := o.str("beforeWorkspaceId")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	// 官方 `beforeWorkspaceId` 是可选的：省略 = 移到末尾。
	if hasBefore && before == "" {
		return nil, officialBadRequest("beforeWorkspaceId 是空串（要移到末尾就省略这个字段）")
	}
	if api.InsertBefore == nil {
		return nil, officialNotImplemented("workspace.insertBefore 未接线：宿主没有工作区注册表")
	}
	order, err := api.InsertBefore(workspaceID, before)
	if err != nil {
		return nil, err
	}
	if order == nil {
		order = []string{}
	}
	return OfficialWorkspaceOrderValue{WorkspaceIDs: order}, nil
}

func officialWorkspaceInsertSessionBefore(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	o, err := officialPickObject(p, "request", []string{"workspaceId", "sessionId", "beforeSessionId"})
	if err != nil {
		return nil, err
	}
	workspaceID, err := officialWorkspaceIDParam(o, "request.workspaceId")
	if err != nil {
		return nil, err
	}
	sessionID, err := officialSessionIDParam(o, "request.sessionId")
	if err != nil {
		return nil, err
	}
	before, hasBefore, err := o.str("beforeSessionId")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if hasBefore && before == "" {
		return nil, officialBadRequest("beforeSessionId 是空串（要移到末尾就省略这个字段）")
	}
	if api.InsertSessionBefore == nil {
		return nil, officialNotImplemented("workspace.insertSessionBefore 未接线：宿主没有工作区注册表")
	}
	ws, err := api.InsertSessionBefore(workspaceID, sessionID, before)
	if err != nil {
		return nil, err
	}
	return OfficialWorkspaceValue{Workspace: officialNormalizeWorkspace(ws)}, nil
}

// officialWorkspacePin 处理 pinSession / unpinSession（两者只差一个动作）。
func officialWorkspacePin(api OfficialWorkspaceAPI, method Method, p *officialParams) (any, error) {
	o, err := officialPickObject(p, "request", []string{"sessionId"})
	if err != nil {
		return nil, err
	}
	sessionID, err := officialSessionIDParam(o, "request.sessionId")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if method == MethodWorkspacePinSession {
		if api.PinSession == nil {
			return nil, officialNotImplemented("workspace.pinSession 未接线：宿主没有工作区注册表")
		}
		pinned, err := api.PinSession(sessionID)
		if err != nil {
			return nil, err
		}
		return OfficialWorkspacePinValue{PinnedSessionIDs: nonNilStrings(pinned)}, nil
	}
	if api.UnpinSession == nil {
		return nil, officialNotImplemented("workspace.unpinSession 未接线：宿主没有工作区注册表")
	}
	pinned, err := api.UnpinSession(sessionID)
	if err != nil {
		return nil, err
	}
	return OfficialWorkspacePinValue{PinnedSessionIDs: nonNilStrings(pinned)}, nil
}

func officialWorkspaceArchive(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	o, err := officialPickObject(p, "request", []string{"sessionId", "stopActivity"})
	if err != nil {
		return nil, err
	}
	sessionID, err := officialSessionIDParam(o, "request.sessionId")
	if err != nil {
		return nil, err
	}
	stop, _, err := o.boolean("stopActivity")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.ArchiveSession == nil {
		return nil, officialNotImplemented("workspace.archiveSession 未接线：宿主没有工作区注册表")
	}
	archived, err := api.ArchiveSession(sessionID, stop)
	if err != nil {
		return nil, err
	}
	return OfficialWorkspaceArchiveValue{ArchivedSessionIDs: nonNilStrings(archived)}, nil
}

func officialWorkspaceUnarchive(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	o, err := officialPickObject(p, "request", []string{"sessionId"})
	if err != nil {
		return nil, err
	}
	sessionID, err := officialSessionIDParam(o, "request.sessionId")
	if err != nil {
		return nil, err
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.UnarchiveSession == nil {
		return nil, officialNotImplemented("workspace.unarchiveSession 未接线：宿主没有工作区注册表")
	}
	archived, err := api.UnarchiveSession(sessionID)
	if err != nil {
		return nil, err
	}
	return OfficialWorkspaceArchiveValue{ArchivedSessionIDs: nonNilStrings(archived)}, nil
}

// ---------------------------------------------------------------------------
// workspaceFiles.*
// ---------------------------------------------------------------------------

func officialWorkspaceFilesList(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	scope, err := officialFileScope(p)
	if err != nil {
		return nil, err
	}
	path, err := officialFilePath(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.FilesList == nil {
		return nil, officialNotImplemented("workspaceFiles.list 未接线：宿主没有真实文件系统读入口")
	}
	listing, err := api.FilesList(scope, path)
	if err != nil {
		return nil, err
	}
	if listing.Entries == nil {
		listing.Entries = []OfficialWorkspaceDirectoryEntry{}
	}
	return listing, nil
}

func officialWorkspaceFilesRead(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	scope, err := officialFileScope(p)
	if err != nil {
		return nil, err
	}
	path, err := officialFilePath(p)
	if err != nil {
		return nil, err
	}
	// 官方 `range` 是**必填**对象参数（typert `z.object({...})`，无 acceptsUndefined）。
	rng, err := officialFileRange(p, "range", true)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.FilesRead == nil {
		return nil, officialNotImplemented("workspaceFiles.read 未接线：宿主没有真实文件系统读入口")
	}
	return api.FilesRead(scope, path, rng)
}

func officialWorkspaceFilesReadBytes(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	scope, err := officialFileScope(p)
	if err != nil {
		return nil, err
	}
	path, err := officialFilePath(p)
	if err != nil {
		return nil, err
	}
	// 官方 `options` 也是必填对象参数（`WorkspaceByteReadOptions`，可给 {}）。
	opts, err := officialByteReadOptions(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.FilesReadBytes == nil {
		return nil, officialNotImplemented("workspaceFiles.readBytes 未接线：宿主没有真实文件系统读入口")
	}
	value, err := api.FilesReadBytes(scope, path, opts)
	if err != nil {
		return nil, err
	}
	// 官方 `data` 是 Uint8Array；L1 用 base64，必须显式标注，绝不裸塞二进制。
	if value.Encoding == "" {
		value.Encoding = "base64"
	}
	return value, nil
}

func officialWorkspaceFilesStat(api OfficialWorkspaceAPI, p *officialParams) (any, error) {
	scope, err := officialFileScope(p)
	if err != nil {
		return nil, err
	}
	path, err := officialFilePath(p)
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if api.FilesStat == nil {
		return nil, officialNotImplemented("workspaceFiles.stat 未接线：宿主没有真实文件系统读入口")
	}
	stat, err := api.FilesStat(scope, path)
	if err != nil {
		return nil, err
	}
	// 官方 `workspaceFiles.stat` 只 stat 普通文件（目录回 not-regular-file）。
	// FilesStat 之所以能给出目录 stat，是给 `workspaceFiles.changes` 的目录目标
	// 用的；这条 unary 方法必须继续拒目录，不能把目录当文件报给客户端。
	if stat.IsDirectory {
		return nil, OfficialWorkspaceError("workspaceFiles/not-a-file",
			"%q 是目录，不是文件（workspaceFiles.stat 只 stat 普通文件；目录 watch 走 workspaceFiles.changes）", path)
	}
	return stat, nil
}

// officialWorkspaceFilesChanges 是官方 `mode:'stream'` 方法。
//
// **它不会走到这里**：`serveOfficialWorkspaceAPI` 在 unary 路由之前就用
// `IsOfficialStreamMethod` 把它交给 NDJSON 长连接（official_stream_face.go）。
// 保留这个 case 是为了让"路由表完整"这件事可读——方法在表里有一行，而不是
// 静默消失。真的被调到说明分派顺序被改坏了，所以明确报错而不是返回空。
func officialWorkspaceFilesChanges(p *officialParams) (any, error) {
	return nil, officialNotImplemented(
		"workspaceFiles.changes 是流式方法：分派应在 IsOfficialStreamMethod 分支" +
			"（NDJSON 长连接）被接走，走到 unary 路由说明分派顺序坏了")
}

// officialFileRange 解析 read 的行窗口 `range`（`{offset?, limit?}`）。
func officialFileRange(p *officialParams, name string, required bool) (OfficialWorkspaceFileRange, error) {
	raw, has := p.take(name)
	if !has {
		if required {
			return OfficialWorkspaceFileRange{}, officialBadRequest(
				"缺 %s（官方的 %s 参数必填；没有窗口就给 {}）", name, name)
		}
		return OfficialWorkspaceFileRange{}, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return OfficialWorkspaceFileRange{}, officialBadRequest("%s 必须是 JSON 对象", name)
	}
	o := newOfficialObject(name, obj)
	out := OfficialWorkspaceFileRange{}
	offset, hasOffset, err := o.number("offset")
	if err != nil {
		return OfficialWorkspaceFileRange{}, err
	}
	limit, hasLimit, err := o.number("limit")
	if err != nil {
		return OfficialWorkspaceFileRange{}, err
	}
	if err := o.finish(); err != nil {
		return OfficialWorkspaceFileRange{}, err
	}
	if hasOffset {
		if offset < 1 {
			return OfficialWorkspaceFileRange{}, officialBadRequest("%s.offset 从 1 开始（给的是 %d）", name, offset)
		}
		out.Offset, out.HasOffset = offset, true
	}
	if hasLimit {
		if limit < 1 {
			return OfficialWorkspaceFileRange{}, officialBadRequest("%s.limit 必须是正整数（给的是 %d）", name, limit)
		}
		out.Limit, out.HasLimit = limit, true
	}
	return out, nil
}

// officialByteReadOptions 解析 readBytes 的 `options`
// （`{range?:{offset?,length?}, baseFile?}`）。
func officialByteReadOptions(p *officialParams) (OfficialWorkspaceByteReadOptions, error) {
	raw, has := p.take("options")
	if !has {
		return OfficialWorkspaceByteReadOptions{}, officialBadRequest(
			"缺 options（官方的 options 参数必填；读整个文件就给 {}）")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return OfficialWorkspaceByteReadOptions{}, officialBadRequest("options 必须是 JSON 对象")
	}
	o := newOfficialObject("options", obj)
	out := OfficialWorkspaceByteReadOptions{}

	if rawRange, ok := o.take("range"); ok {
		var rangeObj map[string]json.RawMessage
		if err := json.Unmarshal(rawRange, &rangeObj); err != nil {
			return OfficialWorkspaceByteReadOptions{}, officialBadRequest("options.range 必须是 JSON 对象")
		}
		ro := newOfficialObject("options.range", rangeObj)
		offset, hasOffset, err := ro.number("offset")
		if err != nil {
			return OfficialWorkspaceByteReadOptions{}, err
		}
		length, hasLength, err := ro.number("length")
		if err != nil {
			return OfficialWorkspaceByteReadOptions{}, err
		}
		if err := ro.finish(); err != nil {
			return OfficialWorkspaceByteReadOptions{}, err
		}
		spec := &OfficialWorkspaceByteRange{}
		if hasOffset {
			if offset < 0 {
				return OfficialWorkspaceByteReadOptions{}, officialBadRequest("options.range.offset 不能为负")
			}
			value := offset
			spec.Offset = &value
		}
		if hasLength {
			if length < 0 {
				return OfficialWorkspaceByteReadOptions{}, officialBadRequest("options.range.length 不能为负")
			}
			value := length
			spec.Length = &value
		}
		out.Range = spec
	}
	base, hasBase, err := o.str("baseFile")
	if err != nil {
		return OfficialWorkspaceByteReadOptions{}, err
	}
	if err := o.finish(); err != nil {
		return OfficialWorkspaceByteReadOptions{}, err
	}
	if hasBase {
		if base == "" {
			return OfficialWorkspaceByteReadOptions{}, officialBadRequest("options.baseFile 是空串")
		}
		out.BaseFile = &base
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// directoryPicker.*
// ---------------------------------------------------------------------------

func officialDirectoryPickerList(picker OfficialDirectoryPickerAPI, p *officialParams) (any, error) {
	// 官方 `path` 是 `acceptsUndefined: true`：省略 = 列 home。
	path, hasPath, err := p.takeStr("path")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if hasPath {
		path = strings.TrimSpace(path)
	}
	if picker.List == nil {
		return nil, officialNotImplemented("directoryPicker.list 未接线：宿主没有目录浏览后端")
	}
	listing, err := picker.List(path)
	if err != nil {
		return nil, err
	}
	if listing.Crumbs == nil {
		listing.Crumbs = []OfficialDirectoryItem{}
	}
	if listing.Entries == nil {
		listing.Entries = []OfficialDirectoryItem{}
	}
	return listing, nil
}

func officialDirectoryPickerCreateDirectory(picker OfficialDirectoryPickerAPI, p *officialParams) (any, error) {
	path, hasPath, err := p.takeStr("path")
	if err != nil {
		return nil, err
	}
	name, hasName, err := p.takeStr("name")
	if err != nil {
		return nil, err
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	if !hasPath {
		return nil, officialBadRequest("缺 path（绝对存在的父目录）")
	}
	if !hasName {
		return nil, officialBadRequest("缺 name（单个非空白路径段）")
	}
	if err := officialDirectoryNameProblem(path, name); err != nil {
		return nil, err
	}
	if picker.CreateDirectory == nil {
		return nil, officialNotImplemented("directoryPicker.createDirectory 未接线：宿主没有目录浏览后端")
	}
	created, err := picker.CreateDirectory(path, name)
	if err != nil {
		return nil, err
	}
	if created == "" {
		return nil, officialInternal("directoryPicker.createDirectory 返回了空路径（官方结果必须是新目录绝对路径）")
	}
	return created, nil
}

// officialDirectoryNameProblem 是官方 zod refine 的逐字搬运：
// `name.trim() !== "" && name !== "." && name !== ".." && !/[/\\]/.test(name)`。
//
// 额外多一条：NUL 与 Windows 盘符（`C:`）也拒——它们会被宿主拼成一个逃出
// 父目录的路径。多拒一条是收紧不是放宽，不会让合法请求 400。
func officialDirectoryNameProblem(path, name string) error {
	if strings.TrimSpace(path) == "" {
		return officialBadRequest("path 是空串（要绝对存在的父目录）")
	}
	if !filepath.IsAbs(path) {
		return officialBadRequest("path 必须是绝对路径（给的是 %q）", path)
	}
	if strings.TrimSpace(name) == "" {
		return officialBadRequest("name 是空白串（官方要求单个非空白路径段）")
	}
	if name == "." || name == ".." {
		return officialBadRequest("name 不能是 %q", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return officialBadRequest("name 不能含路径分隔符（给的是 %q）", name)
	}
	if strings.ContainsRune(name, 0) {
		return officialBadRequest("name 不能含 NUL")
	}
	if strings.ContainsRune(name, ':') {
		return officialBadRequest("name 不能含 ':'（Windows 盘符会被拼成另一条路径）")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// nonNilStrings 把 nil 切片归一成空数组：官方结果里的集合字段是数组，
// 序列化成 null 会让客户端多一个特判分支。
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// officialNormalizeWorkspace 归一一个会话集合字段（同样是为了不让 null 上线）。
func officialNormalizeWorkspace(in OfficialWorkspace) OfficialWorkspace {
	in.SessionIDs = nonNilStrings(in.SessionIDs)
	return in
}
