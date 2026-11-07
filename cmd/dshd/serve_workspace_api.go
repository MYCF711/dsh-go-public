package main

// 官方桌面端 0.2.2-rc.1 的 workspace / workspaceFiles / directoryPicker 16 个方法的
// **宿主侧真实实现**。
//
// # 分工
//
// 契约、参数校验、官方字段、四字段错误都在
// `internal/transport/handlers_workspace_official.go`；本文件只提供 transport 拿不到
// 的真实数据（红线 R7：transport 不读盘、不认识 registry、不认识 v4 会话）：
//
//	workspace.*        真 registry 文件 `<home>/state/workspaces.json` + 真 v4 会话归属
//	workspaceFiles.*   真文件系统，且受"会话 cwd = 工作区根"约束（含符号链接逃逸检查）
//	directoryPicker.*  真目录浏览与真建目录（不返回假目录列表）
//
// # 数据来源与边界（真实性声明）
//
//   - **工作区列表**来自本文件持久化的 registry 文件（`<home>/state/workspaces.json`，
//     version=1）。它不是内存表：进程重启后 create 幂等命中、标题、顺序、
//     pin/archive 集合都还在。
//   - **会话归属**来自真实 v4 会话存储（`session.ListSessions(home, path)`，按
//     projectKey 定位到该目录），手动顺序来自 registry 的 `manualSessionIds`。
//     归档的会话不参与分组（官方"Hide one known Session from Workspace grouping
//     surfaces"），但仍留在真实存储里。
//   - **active / stop** 来自真实轮次服务（`promptService`）：`active` 是"这一会话
//     此刻真有 live turn 或排队项"；`stop` 走 `CancelPlacement(interrupt)`，
//     与 `session.cancel` 同一条路径。
//   - **workspaceFiles** 的根是**该会话日志头里的 cwd**（不是进程 cwd）。读路径
//     一律先做词法边界检查，再对真实解析后的路径（EvalSymlinks）复核一次，
//     因此 `..`、绝对路径越界、符号链接逃逸都出不去工作区根。
//   - **directoryPicker** 的 home 是宿主账户 home（`os.UserHomeDir`，测试可注入），
//     列的是真实目录项；建目录真的 Mkdir，已存在就报 `directory-picker/exists`。
//
// # 明确未做的部分（不假装做了）
//
//   - `archiveSession` 的 `stopActivity` 只停**当前轮**；官方还包含
//     "subagent descendants / owned background jobs / active schedules"，
//     那三类本仓没有统一的停止入口，这里不谎称停了（登记在 RESULT）。
//   - `workspaceFiles.changes` 是官方 stream，transport 层如实 501。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

const (
	// officialWorkspaceStoreVersion 是 registry 文件的格式版本。
	officialWorkspaceStoreVersion = 1
	// officialWorkspaceEntryCap 是 workspaceFiles.list 一层的最多项数（官方
	// "configured entry cap"）。
	officialWorkspaceEntryCap = 512
	// officialWorkspaceMaxLines 是 workspaceFiles.read 一页最多行数
	// （官方 `maxLines`：limit "Defaults to, and may not exceed"）。
	officialWorkspaceMaxLines = 2000
	// officialWorkspaceMaxBytes 是一次读的字节上限（官方 `maxFileBytes`/`maxBytes`）。
	officialWorkspaceMaxBytes = 4 << 20
	// officialDirectoryEntryCap 是 directoryPicker.list 一层的最多项数。
	officialDirectoryEntryCap = 512
)

// officialWorkspaceRecord 是一个工作区在 registry 里的持久记录。
//
// 时间用毫秒整数存：ISO-8601 是**出口形状**，盘上存字符串会让"同一瞬间两种写法"
// 变成两个不同的值。
type officialWorkspaceRecord struct {
	WorkspaceID      string   `json:"workspaceId"`
	Path             string   `json:"path"`
	Title            string   `json:"title"`
	CreatedAtMS      int64    `json:"createdAtMs"`
	UpdatedAtMS      int64    `json:"updatedAtMs"`
	ManualSessionIDs []string `json:"manualSessionIds,omitempty"`
}

// officialWorkspaceDocument 是 registry 文件的顶层形状。
//
// `Order` 是**唯一**的顺序真相（官方 registry 的显示顺序）；`Workspaces` 是
// id → 记录。archived/pinned 是 registry-global 集合，跨工作区共享。
type officialWorkspaceDocument struct {
	Version            int                                 `json:"version"`
	Order              []string                            `json:"order"`
	Workspaces         map[string]*officialWorkspaceRecord  `json:"workspaces"`
	ArchivedSessionIDs []string                            `json:"archivedSessionIds"`
	PinnedSessionIDs   []string                            `json:"pinnedSessionIds"`
}

// officialWorkspaceHost 是 workspace 族 16 个方法的真实宿主实现。
//
// 一把锁管 registry 的"读-改-写-落盘"整段：官方 controller 用 `enqueue` 把变更
// 串行化，这里用互斥锁达到同样效果（不会有两次 create 同时落盘互相覆盖）。
type officialWorkspaceHost struct {
	home string
	src  *sessionSource
	// active/stop 是真实轮次能力；prompts 为 nil（无 serve 循环）时一律"没有活动"。
	active func(sessionID string) bool
	stop   func(sessionID string) error
	now    func() time.Time
	newID  func() (string, error)

	// 上限与 home 可在测试里覆盖；0 表示取默认值。
	dirCap     int
	lineCap    int
	byteCap    int64
	pickerHome string

	mu     sync.Mutex
	path   string
	doc    officialWorkspaceDocument
	loaded bool
}

// newOfficialWorkspaceHost 造一个真实宿主。home 为空表示"没有可用 home"，
// 此时返回 nil（调用方据此不接线 → 方法回 501，而不是拿进程默认目录顶上）。
func newOfficialWorkspaceHost(home string, src *sessionSource, prompts *promptService) *officialWorkspaceHost {
	if strings.TrimSpace(home) == "" {
		return nil
	}
	h := &officialWorkspaceHost{
		home: home,
		src:  src,
		now:  time.Now,
		newID: func() (string, error) {
			id, err := serveID()
			if err != nil {
				return "", err
			}
			return "ws-" + id, nil
		},
		path: filepath.Join(home, "state", "workspaces.json"),
	}
	if h.src == nil {
		h.src = newSessionSource(home, "")
	}
	if prompts != nil {
		h.active = func(sessionID string) bool {
			running, queued := prompts.officialSnapshot(sessionID)
			return running || len(queued) > 0
		}
		h.stop = func(sessionID string) error {
			// 与 session.cancel 同一条真实路径：requested 之后不等待。
			_, err := prompts.CancelPlacement(sessionID, "interrupt", "")
			if err != nil && !isNoSuchTurn(err) {
				return err
			}
			return nil
		}
	}
	return h
}

// ---------------------------------------------------------------------------
// registry 持久化
// ---------------------------------------------------------------------------

func (h *officialWorkspaceHost) emptyDocumentLocked() officialWorkspaceDocument {
	return officialWorkspaceDocument{
		Version:            officialWorkspaceStoreVersion,
		Order:              []string{},
		Workspaces:         map[string]*officialWorkspaceRecord{},
		ArchivedSessionIDs: []string{},
		PinnedSessionIDs:   []string{},
	}
}

// ensureLoadedLocked 首次用时读盘。**坏文件必须报错**：把读不动的 registry 当成空
// registry，会让下一次 save 覆盖掉用户真实的工作区列表。
func (h *officialWorkspaceHost) ensureLoadedLocked() error {
	if h.loaded {
		return nil
	}
	raw, err := os.ReadFile(h.path)
	if err != nil {
		if os.IsNotExist(err) {
			h.doc = h.emptyDocumentLocked()
			h.loaded = true
			return nil
		}
		return fmt.Errorf("读工作区注册表 %s: %w", h.path, err)
	}
	var doc officialWorkspaceDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("解析工作区注册表 %s: %w", h.path, err)
	}
	if doc.Version != officialWorkspaceStoreVersion {
		return fmt.Errorf("工作区注册表 %s 的 version=%d，本宿主只认 %d", h.path, doc.Version, officialWorkspaceStoreVersion)
	}
	if doc.Workspaces == nil {
		return fmt.Errorf("工作区注册表 %s 缺 workspaces", h.path)
	}
	// Order 里认不出的 id 直接丢掉（它们没有记录，画不出来），但顺序保持。
	order := make([]string, 0, len(doc.Order))
	seen := map[string]bool{}
	for _, id := range doc.Order {
		if doc.Workspaces[id] == nil || seen[id] {
			continue
		}
		seen[id] = true
		order = append(order, id)
	}
	// 记录存在但 Order 漏了它（手改过的文件）：补到末尾，别把工作区弄丢。
	for id := range doc.Workspaces {
		if seen[id] {
			continue
		}
		order = append(order, id)
	}
	doc.Order = order
	h.doc = doc
	h.loaded = true
	return nil
}

// saveLocked 原子落盘（临时文件 + rename）：半截文件比旧文件更糟。
func (h *officialWorkspaceHost) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return fmt.Errorf("建 state 目录: %w", err)
	}
	body, err := json.MarshalIndent(h.doc, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化工作区注册表: %w", err)
	}
	body = append(body, '\n')
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("写临时 registry %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, h.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 registry %s: %w", h.path, err)
	}
	return nil
}

// withDocument 在锁内加载并执行一段读逻辑。
func (h *officialWorkspaceHost) withDocument(fn func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.ensureLoadedLocked(); err != nil {
		return transport.OfficialWorkspaceError("workspace/internal", "%v", err)
	}
	return fn()
}

// withDocumentWrite 在锁内加载、执行、落盘。fn 返回错误时**不落盘**。
func (h *officialWorkspaceHost) withDocumentWrite(fn func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.ensureLoadedLocked(); err != nil {
		return transport.OfficialWorkspaceError("workspace/internal", "%v", err)
	}
	// 变更前的快照：落盘失败要能回滚内存，否则内存与磁盘从此不一致。
	backup := h.cloneDocumentLocked()
	if err := fn(); err != nil {
		h.doc = backup
		return err
	}
	if err := h.saveLocked(); err != nil {
		h.doc = backup
		return transport.OfficialWorkspaceError("workspace/internal", "工作区变更没能落盘: %v", err)
	}
	return nil
}

// cloneDocumentLocked 深拷贝当前 doc（切片要复制，map 要重建）。
func (h *officialWorkspaceHost) cloneDocumentLocked() officialWorkspaceDocument {
	out := officialWorkspaceDocument{
		Version:            h.doc.Version,
		Order:              append([]string(nil), h.doc.Order...),
		Workspaces:         make(map[string]*officialWorkspaceRecord, len(h.doc.Workspaces)),
		ArchivedSessionIDs: append([]string(nil), h.doc.ArchivedSessionIDs...),
		PinnedSessionIDs:   append([]string(nil), h.doc.PinnedSessionIDs...),
	}
	for id, rec := range h.doc.Workspaces {
		if rec == nil {
			continue
		}
		copied := *rec
		copied.ManualSessionIDs = append([]string(nil), rec.ManualSessionIDs...)
		out.Workspaces[id] = &copied
	}
	return out
}

// ---------------------------------------------------------------------------
// 投影：真实会话 + 手动顺序
// ---------------------------------------------------------------------------

// viewLocked 把一条记录投影成官方 `WorkspaceView`。
func (h *officialWorkspaceHost) viewLocked(rec *officialWorkspaceRecord) transport.OfficialWorkspace {
	return transport.OfficialWorkspace{
		WorkspaceID: rec.WorkspaceID,
		Path:        rec.Path,
		Title:       rec.Title,
		SessionIDs:  h.sessionIDsLocked(rec),
		CreatedAt:   officialISO8601(rec.CreatedAtMS),
		UpdatedAt:   officialISO8601(rec.UpdatedAtMS),
	}
}

func officialISO8601(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// sessionIDsLocked 给出"这个工作区名下的会话"，按官方语义：
// 手动顺序在前（仍然真实存在的那些），其余按最近更新倒序，归档的不参与。
func (h *officialWorkspaceHost) sessionIDsLocked(rec *officialWorkspaceRecord) []string {
	derived := h.derivedSessionIDs(rec.Path)
	present := make(map[string]bool, len(derived))
	for _, id := range derived {
		present[id] = true
	}
	archived := make(map[string]bool, len(h.doc.ArchivedSessionIDs))
	for _, id := range h.doc.ArchivedSessionIDs {
		archived[id] = true
	}
	seen := make(map[string]bool, len(derived))
	out := make([]string, 0, len(derived))
	for _, id := range rec.ManualSessionIDs {
		if !present[id] || archived[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range derived {
		if archived[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// derivedSessionIDs 从**真实 v4 会话存储**里取这个目录下的会话（最近更新在前）。
//
// 用 `session.ListSessions(home, path)` 而不是自己扫目录：会话列表只有一份真相，
// projectKey 的换算也只该有一份实现。
func (h *officialWorkspaceHost) derivedSessionIDs(workspacePath string) []string {
	if h.src == nil {
		return nil
	}
	listings, err := session.ListSessions(h.home, workspacePath)
	if err != nil {
		return nil
	}
	kept := make([]session.Listing, 0, len(listings))
	for _, l := range listings {
		if strings.TrimSpace(l.SessionID) == "" {
			continue
		}
		kept = append(kept, l)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		return kept[i].UpdatedAt.After(kept[j].UpdatedAt)
	})
	out := make([]string, 0, len(kept))
	for _, l := range kept {
		out = append(out, l.SessionID)
	}
	return out
}

// knownSession 报告这个会话是否真的存在（按 id 定位真实日志文件）。
func (h *officialWorkspaceHost) knownSession(sessionID string) bool {
	if h.src == nil || strings.TrimSpace(sessionID) == "" {
		return false
	}
	_, err := h.src.resolve(sessionID)
	return err == nil
}

// ---------------------------------------------------------------------------
// 路径工具
// ---------------------------------------------------------------------------

// officialCanonicalPath 是路径的规范形式（去 `\\?\`、绝对化、Clean）。
func officialCanonicalPath(path string) string {
	p := v4.NormalizeCwd(strings.TrimSpace(path))
	if abs, err := filepath.Abs(filepath.FromSlash(p)); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// officialSamePath 判两条路径是否指同一目录（Windows 大小写不敏感）。
func officialSamePath(a, b string) bool {
	ca, cb := officialCanonicalPath(a), officialCanonicalPath(b)
	if ca == cb {
		return true
	}
	return strings.EqualFold(ca, cb)
}

// officialPathWithin 判 target 是否在 root 之下（含 root 本身）。
//
// 自己做前缀比较而不是只靠 filepath.Rel：Windows 的盘符与大小写、以及
// `C:\a` vs `C:\ab` 这种"前缀相同但不是子目录"的坑都要挡住。
func officialPathWithin(root, target string) bool {
	r, t := filepath.Clean(root), filepath.Clean(target)
	if r == t {
		return true
	}
	prefix := r
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if len(t) < len(prefix) {
		return false
	}
	// 逐字符比较，大小写在所有平台上都忽略：宁可多拒一个"大小写不同的路径"，
	// 也不要放过一个真实的越界。
	return strings.EqualFold(t[:len(prefix)], prefix)
}

// officialResolveWithin 把一个工作区路径解析成绝对路径与工作区相对路径。
//
// allowEmpty 决定空 path 合法与否：`workspaceFiles.list` 用空串表示工作区根，
// 而 read/stat/readBytes 指向的必须是文件，空串一律拒。
func officialResolveWithin(root, path, base string, allowEmpty bool) (string, string, error) {
	cleaned := strings.TrimSpace(path)
	var target string
	switch {
	case cleaned == "":
		if !allowEmpty {
			return "", "", transport.OfficialWorkspaceError("workspaceFiles/not-a-file", "path 是空串（这里要一个文件）")
		}
		target = root
	case filepath.IsAbs(cleaned):
		target = filepath.Clean(filepath.FromSlash(v4.NormalizeCwd(cleaned)))
	case strings.ContainsRune(cleaned, 0):
		return "", "", transport.OfficialWorkspaceError("workspaceFiles/outside-workspace", "path 含 NUL")
	default:
		if base == "" {
			base = root
		}
		target = filepath.Clean(filepath.Join(base, filepath.FromSlash(cleaned)))
	}
	if !officialPathWithin(root, target) {
		return "", "", transport.OfficialWorkspaceError("workspaceFiles/outside-workspace",
			"路径 %q 跑出工作区根 %q（工作区边界是硬约束）", path, root)
	}
	// 词法边界过了还要按**真实解析后**的路径再核一次：符号链接/联接点会把
	// 一个看起来在里面的路径指到外面去。
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		if resolvedTarget, terr := filepath.EvalSymlinks(target); terr == nil {
			if !officialPathWithin(resolvedRoot, resolvedTarget) {
				return "", "", transport.OfficialWorkspaceError("workspaceFiles/outside-workspace",
					"路径 %q 经符号链接解析后跑出工作区根（%q）", path, resolvedRoot)
			}
		}
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", "", transport.OfficialWorkspaceError("workspaceFiles/outside-workspace", "算不出相对路径: %v", err)
	}
	if rel == "." {
		rel = ""
	}
	return target, filepath.ToSlash(rel), nil
}

// officialResolvedAbsolute 给出"官方 absolutePath"：符号链接解析后的绝对路径。
func officialResolvedAbsolute(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// officialVersionOf 用真实 stat 派生不透明的新鲜度令牌（客户端不许解析）。
func officialVersionOf(st os.FileInfo) string {
	return fmt.Sprintf("%x-%x", st.ModTime().UnixNano(), st.Size())
}

// officialSplitLines 按官方语义切行：去掉 CRLF 的 `\r`，末尾换行不算一行。
func officialSplitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(content), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// ---------------------------------------------------------------------------
// workspace.* 的真实实现
// ---------------------------------------------------------------------------

// workspaceAPI 把真实能力装成 transport 认的那份函数表。
func (h *officialWorkspaceHost) workspaceAPI() transport.OfficialWorkspaceAPI {
	return transport.OfficialWorkspaceAPI{
		Create:              h.createWorkspace,
		Rename:              h.renameWorkspace,
		Delete:              h.deleteWorkspace,
		InsertBefore:        h.insertWorkspaceBefore,
		InsertSessionBefore: h.insertSessionBefore,
		PinSession:          h.pinSession,
		UnpinSession:        h.unpinSession,
		ArchiveSession:      h.archiveSession,
		UnarchiveSession:    h.unarchiveSession,

		FilesList:      h.filesList,
		FilesRead:      h.filesRead,
		FilesReadBytes: h.filesReadBytes,
		FilesStat:      h.filesStat,
	}
}

// directoryPickerAPI 把真实目录浏览装成 transport 认的那份函数表。
func (h *officialWorkspaceHost) directoryPickerAPI() transport.OfficialDirectoryPickerAPI {
	return transport.OfficialDirectoryPickerAPI{
		List:            h.pickerList,
		CreateDirectory: h.pickerCreateDirectory,
	}
}

func (h *officialWorkspaceHost) createWorkspace(path string) (transport.OfficialWorkspaceCreateValue, error) {
	canonical := officialCanonicalPath(path)
	info, err := os.Stat(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			return transport.OfficialWorkspaceCreateValue{}, transport.OfficialWorkspaceError(
				"workspace/invalid-path", "不能在 %q 建工作区：目录不存在", path)
		}
		return transport.OfficialWorkspaceCreateValue{}, transport.OfficialWorkspaceError(
			"workspace/invalid-path", "不能在 %q 建工作区：%v", path, err)
	}
	if !info.IsDir() {
		return transport.OfficialWorkspaceCreateValue{}, transport.OfficialWorkspaceError(
			"workspace/invalid-path", "%q 不是目录", path)
	}

	var out transport.OfficialWorkspaceCreateValue
	err = h.withDocumentWrite(func() error {
		// 幂等命中：同一个真实目录只注册一次。
		for _, id := range h.doc.Order {
			rec := h.doc.Workspaces[id]
			if rec != nil && officialSamePath(rec.Path, canonical) {
				out = transport.OfficialWorkspaceCreateValue{Workspace: h.viewLocked(rec), Created: false}
				return nil
			}
		}
		id, iderr := h.newID()
		if iderr != nil {
			return transport.OfficialWorkspaceError("workspace/internal", "生成 workspaceId 失败: %v", iderr)
		}
		now := h.now().UnixMilli()
		rec := &officialWorkspaceRecord{
			WorkspaceID: id,
			Path:        canonical,
			Title:       officialWorkspaceTitle(canonical),
			CreatedAtMS: now,
			UpdatedAtMS: now,
		}
		h.doc.Workspaces[id] = rec
		h.doc.Order = append(h.doc.Order, id)
		out = transport.OfficialWorkspaceCreateValue{Workspace: h.viewLocked(rec), Created: true}
		return nil
	})
	if err != nil {
		return transport.OfficialWorkspaceCreateValue{}, err
	}
	return out, nil
}

// officialWorkspaceTitle 是新建工作区的初始标题：目录名。
func officialWorkspaceTitle(path string) string {
	base := filepath.Base(filepath.Clean(path))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return path
	}
	return base
}

func (h *officialWorkspaceHost) renameWorkspace(workspaceID, title string) (transport.OfficialWorkspace, error) {
	var out transport.OfficialWorkspace
	err := h.withDocumentWrite(func() error {
		rec := h.doc.Workspaces[workspaceID]
		if rec == nil {
			return transport.OfficialWorkspaceError("workspace/not-found", "没有这个工作区：%q", workspaceID)
		}
		if rec.Title == title {
			out = h.viewLocked(rec)
			return nil
		}
		for _, id := range h.doc.Order {
			if id == workspaceID {
				continue
			}
			other := h.doc.Workspaces[id]
			if other != nil && other.Title == title {
				return transport.OfficialWorkspaceError("workspace/name-conflict",
					"工作区标题 %q 已被 %q 占用", title, id)
			}
		}
		rec.Title = title
		rec.UpdatedAtMS = h.now().UnixMilli()
		out = h.viewLocked(rec)
		return nil
	})
	if err != nil {
		return transport.OfficialWorkspace{}, err
	}
	return out, nil
}

func (h *officialWorkspaceHost) deleteWorkspace(workspaceID string) error {
	return h.withDocumentWrite(func() error {
		if h.doc.Workspaces[workspaceID] == nil {
			return transport.OfficialWorkspaceError("workspace/not-found", "没有这个工作区：%q", workspaceID)
		}
		delete(h.doc.Workspaces, workspaceID)
		order := make([]string, 0, len(h.doc.Order))
		for _, id := range h.doc.Order {
			if id != workspaceID {
				order = append(order, id)
			}
		}
		h.doc.Order = order
		return nil
	})
}

func (h *officialWorkspaceHost) insertWorkspaceBefore(workspaceID, beforeWorkspaceID string) ([]string, error) {
	var out []string
	err := h.withDocumentWrite(func() error {
		if h.doc.Workspaces[workspaceID] == nil {
			return transport.OfficialWorkspaceError("workspace/not-found", "没有这个工作区：%q", workspaceID)
		}
		if beforeWorkspaceID != "" {
			if beforeWorkspaceID == workspaceID {
				return transport.OfficialWorkspaceError("workspace/move-invalid",
					"不能在它自己前面插入：%q", workspaceID)
			}
			if h.doc.Workspaces[beforeWorkspaceID] == nil {
				return transport.OfficialWorkspaceError("workspace/not-found", "锚点工作区不存在：%q", beforeWorkspaceID)
			}
		}
		rest := make([]string, 0, len(h.doc.Order))
		for _, id := range h.doc.Order {
			if id != workspaceID {
				rest = append(rest, id)
			}
		}
		if beforeWorkspaceID == "" {
			rest = append(rest, workspaceID)
			h.doc.Order = rest
			out = append([]string(nil), rest...)
			return nil
		}
		next := make([]string, 0, len(rest)+1)
		for _, id := range rest {
			if id == beforeWorkspaceID {
				next = append(next, workspaceID)
			}
			next = append(next, id)
		}
		h.doc.Order = next
		out = append([]string(nil), next...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (h *officialWorkspaceHost) insertSessionBefore(workspaceID, sessionID, beforeSessionID string) (transport.OfficialWorkspace, error) {
	var out transport.OfficialWorkspace
	err := h.withDocumentWrite(func() error {
		rec := h.doc.Workspaces[workspaceID]
		if rec == nil {
			return transport.OfficialWorkspaceError("workspace/not-found", "没有这个工作区：%q", workspaceID)
		}
		current := h.sessionIDsLocked(rec)
		if !officialContains(current, sessionID) {
			return transport.OfficialWorkspaceError("workspace/move-invalid",
				"会话 %q 不在工作区 %q 的手动顺序里（它不属于这个工作区，或已被归档）", sessionID, workspaceID)
		}
		if beforeSessionID != "" {
			if beforeSessionID == sessionID {
				return transport.OfficialWorkspaceError("workspace/move-invalid", "会话不能在它自己前面：%q", sessionID)
			}
			if !officialContains(current, beforeSessionID) {
				return transport.OfficialWorkspaceError("workspace/move-invalid",
					"锚点会话 %q 不在工作区 %q 里", beforeSessionID, workspaceID)
			}
		}
		rest := make([]string, 0, len(current))
		for _, id := range current {
			if id != sessionID {
				rest = append(rest, id)
			}
		}
		next := make([]string, 0, len(rest)+1)
		if beforeSessionID == "" {
			next = append(rest, sessionID)
		} else {
			for _, id := range rest {
				if id == beforeSessionID {
					next = append(next, sessionID)
				}
				next = append(next, id)
			}
		}
		rec.ManualSessionIDs = next
		rec.UpdatedAtMS = h.now().UnixMilli()
		out = h.viewLocked(rec)
		return nil
	})
	if err != nil {
		return transport.OfficialWorkspace{}, err
	}
	return out, nil
}

func (h *officialWorkspaceHost) pinSession(sessionID string) ([]string, error) {
	var out []string
	err := h.withDocumentWrite(func() error {
		if !h.knownSession(sessionID) {
			return transport.OfficialWorkspaceError("session/not-found", "没有这个会话：%q", sessionID)
		}
		if officialContains(h.doc.ArchivedSessionIDs, sessionID) {
			return transport.OfficialWorkspaceError("gateway/bad-request",
				"会话 %q 已归档，先 unarchive 再 pin", sessionID)
		}
		h.doc.PinnedSessionIDs = officialMoveToFront(h.doc.PinnedSessionIDs, sessionID)
		out = append([]string(nil), h.doc.PinnedSessionIDs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func (h *officialWorkspaceHost) unpinSession(sessionID string) ([]string, error) {
	var out []string
	err := h.withDocumentWrite(func() error {
		h.doc.PinnedSessionIDs = officialRemove(h.doc.PinnedSessionIDs, sessionID)
		out = append([]string(nil), h.doc.PinnedSessionIDs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// archiveSession 把会话加进 registry-global 归档集。
//
// 活动会话默认拒绝（官方 `workspace/session-active`）；`stopActivity` 为真时
// **先请求停止**（不等待），再落盘归档。
func (h *officialWorkspaceHost) archiveSession(sessionID string, stopActivity bool) ([]string, error) {
	if !h.knownSession(sessionID) {
		return nil, transport.OfficialWorkspaceError("session/not-found", "没有这个会话：%q", sessionID)
	}
	if h.active != nil && h.active(sessionID) {
		if !stopActivity {
			return nil, transport.OfficialWorkspaceError("workspace/session-active",
				"会话 %q 还在跑（turn 或排队项未结束）：先 session.cancel，或带 stopActivity=true 再归档", sessionID)
		}
		if h.stop != nil {
			// 官方语义：停止请求先发出去，且**不等待**它完成；失败不改归档结果，
			// 但不能静默——打到 stderr 供排查。
			if err := h.stop(sessionID); err != nil {
				fmt.Fprintf(os.Stderr, "transport: workspace.archiveSession 请求停止 %s 失败（归档照做）: %v\n", sessionID, err)
			}
		}
	}
	var out []string
	err := h.withDocumentWrite(func() error {
		if !officialContains(h.doc.ArchivedSessionIDs, sessionID) {
			h.doc.ArchivedSessionIDs = append([]string{sessionID}, h.doc.ArchivedSessionIDs...)
		}
		out = append([]string(nil), h.doc.ArchivedSessionIDs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func (h *officialWorkspaceHost) unarchiveSession(sessionID string) ([]string, error) {
	var out []string
	err := h.withDocumentWrite(func() error {
		h.doc.ArchivedSessionIDs = officialRemove(h.doc.ArchivedSessionIDs, sessionID)
		out = append([]string(nil), h.doc.ArchivedSessionIDs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// officialContains 是线性查找（集合都很小：几十条）。
func officialContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// officialRemove 去掉全部匹配项（保持其余顺序）。
func officialRemove(list []string, want string) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item == want {
			continue
		}
		out = append(out, item)
	}
	return out
}

// officialMoveToFront 把 want 提到最前（已存在则移动，不重复）。
func officialMoveToFront(list []string, want string) []string {
	out := make([]string, 0, len(list)+1)
	out = append(out, want)
	for _, item := range list {
		if item == want {
			continue
		}
		out = append(out, item)
	}
	return out
}

// ---------------------------------------------------------------------------
// workspaceFiles.* 的真实实现
// ---------------------------------------------------------------------------

// workspaceRootForSession 取该会话的工作区根：**会话日志头里的 cwd**。
//
// 不用进程 cwd：多会话可能属于不同工作区，进程 cwd 只是"当前这个"。
func (h *officialWorkspaceHost) workspaceRootForSession(sessionID string) (string, error) {
	if h.src == nil {
		return "", transport.OfficialWorkspaceError("workspace/unsupported", "宿主没有会话数据源，给不出工作区根")
	}
	cwd, err := h.src.Cwd(sessionID)
	if err != nil {
		return "", transport.OfficialWorkspaceError("session/not-found", "会话 %q 不存在或读不出 cwd：%v", sessionID, err)
	}
	if strings.TrimSpace(cwd) == "" {
		return "", transport.OfficialWorkspaceError("session/not-found", "会话 %q 的日志头没有 cwd，无法确定工作区根", sessionID)
	}
	root := officialCanonicalPath(cwd)
	if st, serr := os.Stat(root); serr != nil || !st.IsDir() {
		return "", transport.OfficialWorkspaceError("workspaceFiles/outside-workspace",
			"会话 %q 的工作区根 %q 不是一个存在的目录：%v", sessionID, root, serr)
	}
	return root, nil
}

func (h *officialWorkspaceHost) filesList(sessionID, path string) (transport.OfficialWorkspaceDirectoryListing, error) {
	root, err := h.workspaceRootForSession(sessionID)
	if err != nil {
		return transport.OfficialWorkspaceDirectoryListing{}, err
	}
	abs, rel, err := officialResolveWithin(root, path, root, true)
	if err != nil {
		return transport.OfficialWorkspaceDirectoryListing{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return transport.OfficialWorkspaceDirectoryListing{}, transport.OfficialWorkspaceError(
				"workspaceFiles/not-found", "目录不存在：%q", path)
		}
		return transport.OfficialWorkspaceDirectoryListing{}, transport.OfficialWorkspaceError(
			"workspaceFiles/unreadable", "读不了 %q：%v", path, err)
	}
	if !st.IsDir() {
		return transport.OfficialWorkspaceDirectoryListing{}, transport.OfficialWorkspaceError(
			"workspaceFiles/not-a-directory", "%q 不是目录", path)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return transport.OfficialWorkspaceDirectoryListing{}, transport.OfficialWorkspaceError(
			"workspaceFiles/unreadable", "列不动 %q：%v", path, err)
	}
	capN := h.entryCap()
	out := make([]transport.OfficialWorkspaceDirectoryEntry, 0, len(entries))
	truncated := false
	for _, entry := range entries {
		if len(out) >= capN {
			truncated = true
			break
		}
		child := filepath.Join(abs, entry.Name())
		// 官方："A symlink reports the type of its destination" —— 用 Stat 跟随。
		info, ierr := os.Stat(child)
		item := transport.OfficialWorkspaceDirectoryEntry{Name: entry.Name(), Type: "other"}
		switch {
		case ierr != nil:
			item.Type = "other"
		case info.IsDir():
			item.Type = "directory"
		case info.Mode().IsRegular():
			item.Type = "file"
			size := info.Size()
			item.Size = &size
		}
		out = append(out, item)
	}
	return transport.OfficialWorkspaceDirectoryListing{Path: rel, Entries: out, Truncated: truncated}, nil
}

// filesRead 读一页文本。官方语义：offset 从 1 开始，lines=0 表示这一页越过末行，
// eof 表示"这一页包含了文件的最后一行"。
func (h *officialWorkspaceHost) filesRead(sessionID, path string, r transport.OfficialWorkspaceFileRange) (transport.OfficialWorkspaceFileText, error) {
	root, err := h.workspaceRootForSession(sessionID)
	if err != nil {
		return transport.OfficialWorkspaceFileText{}, err
	}
	abs, _, err := officialResolveWithin(root, path, root, false)
	if err != nil {
		return transport.OfficialWorkspaceFileText{}, err
	}
	st, err := h.statRegularFile(abs, path)
	if err != nil {
		return transport.OfficialWorkspaceFileText{}, err
	}
	if st.Size() > h.byteCapValue() {
		return transport.OfficialWorkspaceFileText{}, transport.OfficialWorkspaceError(
			"workspaceFiles/too-large", "文件 %d 字节超过一页读的上限 %d；用 workspaceFiles.readBytes 的 range 分窗读", st.Size(), h.byteCapValue())
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return transport.OfficialWorkspaceFileText{}, transport.OfficialWorkspaceError(
			"workspaceFiles/unreadable", "读不了 %q：%v", path, err)
	}
	offset := int64(1)
	if r.HasOffset {
		offset = r.Offset
	}
	limit := int64(h.lineCapValue())
	if r.HasLimit {
		if r.Limit > int64(h.lineCapValue()) {
			return transport.OfficialWorkspaceFileText{}, transport.OfficialWorkspaceError(
				"workspaceFiles/too-large", "range.limit=%d 超过一页上限 %d", r.Limit, h.lineCapValue())
		}
		limit = r.Limit
	}
	lines := officialSplitLines(content)
	total := int64(len(lines))
	start := offset - 1
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := lines[start:end]
	size := st.Size()
	return transport.OfficialWorkspaceFileText{
		Offset:       offset,
		Text:         strings.Join(page, "\n"),
		Lines:        int64(len(page)),
		EOF:          end >= total,
		AbsolutePath: officialResolvedAbsolute(abs),
		Version:      officialVersionOf(st),
		Bytes:        &size,
	}, nil
}

// filesReadBytes 读整文件或一个字节窗口。官方 data 是 Uint8Array；L1 用 base64。
func (h *officialWorkspaceHost) filesReadBytes(sessionID, path string, o transport.OfficialWorkspaceByteReadOptions) (transport.OfficialWorkspaceFileBytes, error) {
	root, err := h.workspaceRootForSession(sessionID)
	if err != nil {
		return transport.OfficialWorkspaceFileBytes{}, err
	}
	base := root
	if o.BaseFile != nil {
		// baseFile 本身也必须落在工作区内，否则它会把相对路径指向外面。
		baseAbs, _, berr := officialResolveWithin(root, *o.BaseFile, root, false)
		if berr != nil {
			return transport.OfficialWorkspaceFileBytes{}, berr
		}
		base = filepath.Dir(baseAbs)
	}
	abs, _, err := officialResolveWithin(root, path, base, false)
	if err != nil {
		return transport.OfficialWorkspaceFileBytes{}, err
	}
	st, err := h.statRegularFile(abs, path)
	if err != nil {
		return transport.OfficialWorkspaceFileBytes{}, err
	}
	size := st.Size()
	offset := int64(0)
	length := size
	hasWindow := false
	if o.Range != nil {
		if o.Range.Offset != nil {
			offset = *o.Range.Offset
			hasWindow = true
		}
		if o.Range.Length != nil {
			length = *o.Range.Length
			hasWindow = true
			if length > h.byteCapValue() {
				return transport.OfficialWorkspaceFileBytes{}, transport.OfficialWorkspaceError(
					"workspaceFiles/too-large", "options.range.length=%d 超过一次读的上限 %d", length, h.byteCapValue())
			}
		} else if size > h.byteCapValue() {
			return transport.OfficialWorkspaceFileBytes{}, transport.OfficialWorkspaceError(
				"workspaceFiles/too-large", "文件 %d 字节超过一次读的上限 %d；给 options.range 分窗读", size, h.byteCapValue())
		}
	} else if size > h.byteCapValue() {
		return transport.OfficialWorkspaceFileBytes{}, transport.OfficialWorkspaceError(
			"workspaceFiles/too-large", "文件 %d 字节超过一次读的上限 %d；给 options.range 分窗读", size, h.byteCapValue())
	}
	if offset > size {
		offset = size
	}
	if offset+length > size {
		length = size - offset
	}
	if length < 0 {
		length = 0
	}
	f, err := os.Open(abs)
	if err != nil {
		return transport.OfficialWorkspaceFileBytes{}, transport.OfficialWorkspaceError(
			"workspaceFiles/unreadable", "打不开 %q：%v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
		return transport.OfficialWorkspaceFileBytes{}, transport.OfficialWorkspaceError(
			"workspaceFiles/unreadable", "定位 %q 失败：%v", path, serr)
	}
	buf := make([]byte, length)
	read, rerr := io.ReadFull(f, buf)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		return transport.OfficialWorkspaceFileBytes{}, transport.OfficialWorkspaceError(
			"workspaceFiles/unreadable", "读 %q 失败：%v", path, rerr)
	}
	buf = buf[:read]
	_ = hasWindow
	return transport.OfficialWorkspaceFileBytes{
		Offset:       offset,
		Data:         base64.StdEncoding.EncodeToString(buf),
		Encoding:     "base64",
		EOF:          offset+int64(read) >= size,
		AbsolutePath: officialResolvedAbsolute(abs),
		Version:      officialVersionOf(st),
		Bytes:        &size,
	}, nil
}

// filesStat 是 `workspaceFiles.stat` 与 `workspaceFiles.changes` 共用的目标 stat。
//
// 官方 `workspaceFiles.stat` 只认普通文件；而 `workspaceFiles.changes` 的目标
// 按官方可以是文件也可以是**目录**（`Watch one file or a directory's direct
// entries`），目录帧要的是 `{absolutePath, version}`。两者共用同一个宿主入口：
// 目录返回 absolutePath/version 并置 IsDirectory，由 transport 层保证 unary stat
// 继续拒目录（见 internal/transport/handlers_workspace_official.go）。
//
// 空 path 在这里是**工作区根**：官方 `fs.resolve(path, {cwd})` 就是这么解析的，
// changes 允许 watch 根目录；unary stat 的空串拒绝同样由 transport 层按官方
// 契约完成（根是目录，会被判成目录目标拒掉）。
func (h *officialWorkspaceHost) filesStat(sessionID, path string) (transport.OfficialWorkspaceFileStat, error) {
	root, err := h.workspaceRootForSession(sessionID)
	if err != nil {
		return transport.OfficialWorkspaceFileStat{}, err
	}
	abs, _, err := officialResolveWithin(root, path, root, true)
	if err != nil {
		return transport.OfficialWorkspaceFileStat{}, err
	}
	// 目标身份只看最后一段（中间目录的符号链接已由 officialResolveWithin 按
	// 解析后的真实路径核过边界）；最后一段是符号链接一律拒——不借链接改变
	// "这个路径指向谁"。
	lst, err := h.statEntry(abs, path)
	if err != nil {
		return transport.OfficialWorkspaceFileStat{}, err
	}
	if lst.IsDir() {
		// 目录版本 = 目录自身的 stat 令牌（mtime+size）。NTFS/ext4 在直接子项
		// 增/删/改名时更新目录 mtime，子项内容变化不更新——正好是官方
		// "目录的直接子项"这条 watch 的失效面。
		return transport.OfficialWorkspaceFileStat{
			AbsolutePath: officialResolvedAbsolute(abs),
			Version:      officialVersionOf(lst),
			IsDirectory:  true,
		}, nil
	}
	if !lst.Mode().IsRegular() {
		return transport.OfficialWorkspaceFileStat{}, transport.OfficialWorkspaceError(
			"workspaceFiles/not-a-file", "%q 不是普通文件", path)
	}
	size := lst.Size()
	return transport.OfficialWorkspaceFileStat{
		AbsolutePath: officialResolvedAbsolute(abs),
		Version:      officialVersionOf(lst),
		Bytes:        &size,
	}, nil
}

// statEntry 是 stat/read/readBytes 共用的前置检查：存在性 + 拒绝符号链接。
// 目录/普通文件的判定留给调用方（unary stat 不认目录，changes 认）。
func (h *officialWorkspaceHost) statEntry(abs, path string) (os.FileInfo, error) {
	lst, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, transport.OfficialWorkspaceError("workspaceFiles/not-found", "文件不存在：%q", path)
		}
		return nil, transport.OfficialWorkspaceError("workspaceFiles/unreadable", "读不了 %q：%v", path, err)
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return nil, transport.OfficialWorkspaceError("workspaceFiles/symlink-refused",
			"%q 是符号链接：官方 read 不用链接冒充文件内容", path)
	}
	return lst, nil
}

// statRegularFile 是 read/readBytes/stat 共用的前置检查：
// 必须是**普通文件**，且 `read` 家族拒绝符号链接（官方"`read` still refuses a symlink"）。
func (h *officialWorkspaceHost) statRegularFile(abs, path string) (os.FileInfo, error) {
	lst, err := h.statEntry(abs, path)
	if err != nil {
		return nil, err
	}
	if lst.IsDir() {
		return nil, transport.OfficialWorkspaceError("workspaceFiles/not-a-file", "%q 是目录，不是文件", path)
	}
	if !lst.Mode().IsRegular() {
		return nil, transport.OfficialWorkspaceError("workspaceFiles/not-a-file", "%q 不是普通文件", path)
	}
	return lst, nil
}

func (h *officialWorkspaceHost) entryCap() int {
	if h.dirCap > 0 {
		return h.dirCap
	}
	return officialWorkspaceEntryCap
}

func (h *officialWorkspaceHost) lineCapValue() int {
	if h.lineCap > 0 {
		return h.lineCap
	}
	return officialWorkspaceMaxLines
}

func (h *officialWorkspaceHost) byteCapValue() int64 {
	if h.byteCap > 0 {
		return h.byteCap
	}
	return officialWorkspaceMaxBytes
}

// ---------------------------------------------------------------------------
// directoryPicker.* 的真实实现
// ---------------------------------------------------------------------------

// pickerHomeDir 是浏览器的"Home"根（官方 `home` 字段）。
func (h *officialWorkspaceHost) pickerHomeDir() string {
	if strings.TrimSpace(h.pickerHome) != "" {
		return officialCanonicalPath(h.pickerHome)
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return officialCanonicalPath(home)
}

func (h *officialWorkspaceHost) pickerList(path string) (transport.OfficialDirectoryListing, error) {
	home := h.pickerHomeDir()
	target := home
	if strings.TrimSpace(path) != "" {
		target = officialCanonicalPath(path)
	}
	if target == "" {
		return transport.OfficialDirectoryListing{}, transport.OfficialWorkspaceError(
			"directory-picker/unreadable", "宿主说不清自己的 home 目录，也没有给 path")
	}
	st, err := os.Stat(target)
	if err != nil {
		return transport.OfficialDirectoryListing{}, transport.OfficialWorkspaceError(
			"directory-picker/unreadable", "列不动 %q：%v", target, err)
	}
	if !st.IsDir() {
		return transport.OfficialDirectoryListing{}, transport.OfficialWorkspaceError(
			"directory-picker/unreadable", "%q 不是目录", target)
	}
	raw, err := os.ReadDir(target)
	if err != nil {
		return transport.OfficialDirectoryListing{}, transport.OfficialWorkspaceError(
			"directory-picker/unreadable", "列不动 %q：%v", target, err)
	}
	capN := h.dirCap
	if capN <= 0 {
		capN = officialDirectoryEntryCap
	}
	entries := make([]transport.OfficialDirectoryItem, 0, len(raw))
	truncated := false
	for _, entry := range raw {
		child := filepath.Join(target, entry.Name())
		info, ierr := os.Stat(child)
		if ierr != nil || !info.IsDir() {
			// 官方："Direct child directories" —— 文件与不可读项都不出现。
			continue
		}
		if len(entries) >= capN {
			truncated = true
			break
		}
		entries = append(entries, transport.OfficialDirectoryItem{
			Name: entry.Name(),
			Path: child,
			// 官方：hidden 是宿主平台约定。本仓按 dot 前缀判（POSIX 约定，
			// Windows 上也用同一判据，避免同一份列表两套标准）。
			Hidden: strings.HasPrefix(entry.Name(), "."),
		})
	}
	return transport.OfficialDirectoryListing{
		Path:      target,
		Home:      home,
		Crumbs:    officialDirectoryCrumbs(target),
		Entries:   entries,
		Truncated: truncated,
	}, nil
}

// officialDirectoryCrumbs 造"从文件系统根到所列目录"的完整祖先链。
//
// 官方：每个 crumb 都是跳转目标，根 crumb 的 name 是它的完整路径
// （Windows 上是 `C:\`，POSIX 上是 `/`），crumb 的 hidden 永远是 false。
func officialDirectoryCrumbs(target string) []transport.OfficialDirectoryItem {
	clean := filepath.Clean(target)
	var chain []string
	for cur := clean; ; {
		chain = append(chain, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	out := make([]transport.OfficialDirectoryItem, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		path := chain[i]
		name := filepath.Base(path)
		if i == len(chain)-1 {
			// 根：name 就是完整路径本身。
			name = path
		}
		out = append(out, transport.OfficialDirectoryItem{Name: name, Path: path, Hidden: false})
	}
	return out
}

func (h *officialWorkspaceHost) pickerCreateDirectory(path, name string) (string, error) {
	parent := officialCanonicalPath(path)
	if !filepath.IsAbs(parent) {
		return "", transport.OfficialWorkspaceError("gateway/bad-request",
			"path 必须是绝对路径（给的是 %q）", path)
	}
	// name 的逐字校验在 transport 层已经做过一遍；这里再校一次（纵深防御：
	// 宿主也可能被别的调用方直接使用）。
	if strings.TrimSpace(name) == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", transport.OfficialWorkspaceError("gateway/bad-request",
			"name 必须是单个非空白路径段（给的是 %q）", name)
	}
	st, err := os.Stat(parent)
	if err != nil {
		return "", transport.OfficialWorkspaceError("directory-picker/create-failed",
			"父目录不存在或读不了 %q：%v", parent, err)
	}
	if !st.IsDir() {
		return "", transport.OfficialWorkspaceError("directory-picker/create-failed",
			"父目录 %q 不是目录", parent)
	}
	target := filepath.Join(parent, name)
	if _, err := os.Lstat(target); err == nil {
		return "", transport.OfficialWorkspaceError("directory-picker/exists",
			"%q 已经存在", target)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return "", transport.OfficialWorkspaceError("directory-picker/create-failed",
			"建目录 %q 失败：%v", target, err)
	}
	return target, nil
}

// ---------------------------------------------------------------------------
// 接线
// ---------------------------------------------------------------------------

// attachOfficialWorkspace 在 serve 的装配阶段把真实实现挂到 srv 上。
//
// 这是 Lead 接线要用的一行：`serve.go` 造好 Server 之后、开始服务之前调它。
// prompts 可以为 nil（不带 serve 循环的入口），此时"没有活动会话"是真实结论。
func attachOfficialWorkspace(srv *transport.Server, home string, src *sessionSource, prompts *promptService) *officialWorkspaceHost {
	host := newOfficialWorkspaceHost(home, src, prompts)
	if host == nil || srv == nil {
		return host
	}
	srv.AttachOfficialWorkspaceAPI(host.workspaceAPI())
	srv.AttachOfficialDirectoryPickerAPI(host.directoryPickerAPI())
	return host
}

// 编译期钉住：这两个接口形状必须与 transport 的期望一致。
var (
	_ = transport.OfficialWorkspaceAPI{}
	_ = transport.OfficialDirectoryPickerAPI{}
)
