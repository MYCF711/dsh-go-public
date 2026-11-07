package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// serveDelete 删掉一个会话目录（整个 id 目录，不只是一份文件）。
// 有 live turn 时先拒掉 pending approval、请求取消并等 runner 真收工；
// 整个等待与删除区间都由删除屏障挡住新轮，避免 RemoveAll 与 v4/工具写入并发。
func serveDelete(home, sessionID string, broker *approvalBroker, prompts *promptService) error {
	if sessionID == "" {
		return fmt.Errorf("会话 id 为空")
	}
	path, err := session.FindByID(home, sessionID)
	if err != nil {
		// "找不到"必须和"删失败"分开：界面看到 404 才知道该换个 id，
		// 看到 500 只会以为服务坏了、反复重试同一个不存在的会话。
		return fmt.Errorf("%w: %v", transport.ErrNoSuchSession, err)
	}

	if prompts != nil {
		release, err := prompts.beginDelete(sessionID)
		if err != nil {
			return fmt.Errorf("会话正在删除: %w", err)
		}
		defer release()
		if err := cancelServeTurn(broker, prompts, sessionID); err != nil && !errors.Is(err, ErrNoSuchTurn) {
			if errors.Is(err, ErrCancelTimeout) {
				return fmt.Errorf("删除前取消未收工，会话保留且可重试: %w", err)
			}
			return fmt.Errorf("删除前取消失败，会话未删除: %w", err)
		}
	}

	// 等待期间若被外部删除，仍要报不存在；RemoveAll 对不存在返回 nil，
	// 不能直接拿它冒充本次删除成功。
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %v", transport.ErrNoSuchSession, err)
		}
		return fmt.Errorf("删除前检查会话失败: %w", err)
	}
	// 删的是这个会话的**目录**（内含 session.v4.jsonl.zstd 与 .lock）：
	// 只删文件会留下空目录，列表下次还会扫到它并报"坏会话"。
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		return fmt.Errorf("删除会话目录失败: %w", err)
	}
	return nil
}

// serveFork 保留旧的无参数整会话 fork。历史 title 形参原本就不生效，
// 新能力只能经 serveForkWithOptions 进入，避免旧调用悄悄改变语义。
func serveFork(home, sessionID, title string) (string, error) {
	res, err := serveForkWithOptions(home, sessionID, nil, nil)
	return res.SessionID, err
}

// withSessionOperationOptions 是 cmdServe 与真实 HTTP 测试共用的唯一接线。
// 这样 restart/A/B 验收走的不是另写一套测试专用 handler。
func withSessionOperationOptions(home string, opt transport.Options) transport.Options {
	metadata := newSessionMetadataService(home)
	opt.RenameSession = metadata.Rename
	opt.GetSessionColor = metadata.Color
	opt.SetSessionColor = metadata.SetColor
	opt.WorkspaceInit = metadata.WorkspaceInit
	opt.ForkAnchored = func(sessionID string, anchor, title *string) (transport.ForkResult, error) {
		return serveForkWithOptions(home, sessionID, anchor, title)
	}
	opt.RewindPreview = func(sessionID, anchor string) (transport.RewindPreview, error) {
		return serveRewindPreview(home, sessionID, anchor)
	}
	opt.RewindApply = func(sessionID, anchor, mode string) (transport.RewindResult, error) {
		return serveRewindApply(home, sessionID, anchor, mode)
	}
	opt.TranscriptPage = func(sessionID, cursor string, limit int) (transport.TranscriptPageResult, error) {
		return serveTranscriptPage(home, sessionID, cursor, limit)
	}
	// 官方 0.2.2 #78 session-checkpoint-policy：模型请求**之前**先落一份前缀快照。
	// 必须放在最后包：四个 prompt 入口都在上面这个 Options 里，晚包才不会漏。
	// 这里只补 durability 时机，rewind 的 preview/apply 一个字都没动。
	return withDurableCheckpointOptions(home, opt)
}

func serveForkWithOptions(home, sessionID string, anchor, title *string) (transport.ForkResult, error) {
	if sessionID == "" {
		return transport.ForkResult{}, fmt.Errorf("会话 id 为空")
	}
	srcPath, err := newSessionSource(home, "").resolve(sessionID)
	if err != nil {
		return transport.ForkResult{}, err
	}
	src, err := v4.ReadFile(srcPath)
	if err != nil {
		return transport.ForkResult{}, fmt.Errorf("读源会话失败: %w", err)
	}
	newID := "session-" + session.NewID()
	var forked *session.Log
	if anchor == nil {
		forked, err = session.Fork(src, home, src.Header.Cwd, newID)
		if err == nil && title != nil {
			err = session.AppendSessionTitle(forked, *title)
		}
	} else {
		forked, err = session.ForkAt(src, home, src.Header.Cwd, newID, *anchor, title)
	}
	if err != nil {
		if forked != nil {
			_ = forked.Close()
		}
		if errors.Is(err, session.ErrInvalidAnchor) {
			return transport.ForkResult{}, fmt.Errorf("%w: %w", transport.ErrInvalidAnchor, err)
		}
		return transport.ForkResult{}, err
	}
	// 必须 Close 刷盘：否则新会话文件不完整，界面立刻 list 会看不到它。
	if cerr := forked.Close(); cerr != nil {
		return transport.ForkResult{}, fmt.Errorf("新会话落盘失败: %w", cerr)
	}
	return transport.ForkResult{SessionID: newID, Anchor: anchor, Title: title}, nil
}

// captureRewindCheckpointForLog 在用户事件已落盘、任何工具尚未执行时拍照。
// anchor 就是刚追加的 v4 seq；preview/apply 之后按同一身份找回这份字节。
// checkpoint 是可选能力：捕获失败必须响亮记录，但不能把普通对话轮次伪报成失败。
func captureRewindCheckpointForLog(home string, logw *session.Log) error {
	if logw == nil {
		return fmt.Errorf("rewind checkpoint 需要已打开的会话日志")
	}
	header, err := v4.ReadHeader(logw.Path)
	if err != nil {
		return fmt.Errorf("读取 checkpoint 会话身份失败: %w", err)
	}
	if err := session.CaptureRewindCheckpoint(home, header.ID, logw.Seq()-1, header.Cwd); err != nil {
		return fmt.Errorf("捕获 rewind checkpoint(seq=%d)失败: %w", logw.Seq()-1, err)
	}
	return nil
}

func readRewindSource(home, sessionID string) (*v4.Log, error) {
	if sessionID == "" {
		return nil, transport.ErrNoSuchSession
	}
	path, err := newSessionSource(home, "").resolve(sessionID)
	if err != nil {
		return nil, err
	}
	return v4.ReadFile(path)
}

func rewindPreviewOf(p session.RewindFilesPreview) *transport.RewindPreview {
	files := p.FilesChanged
	if files == nil {
		files = []string{}
	}
	return &transport.RewindPreview{FilesChanged: files, Insertions: p.Insertions, Deletions: p.Deletions}
}

func serveRewindPreview(home, sessionID, anchor string) (transport.RewindPreview, error) {
	src, err := readRewindSource(home, sessionID)
	if err != nil {
		return transport.RewindPreview{}, err
	}
	seq, err := session.ResolveUserAnchor(src, anchor)
	if err != nil {
		return transport.RewindPreview{}, fmt.Errorf("%w: %w", transport.ErrInvalidAnchor, err)
	}
	preview, err := session.PreviewRewindFiles(home, src.Header.ID, seq, src.Header.Cwd)
	if err != nil {
		return transport.RewindPreview{}, err
	}
	return *rewindPreviewOf(preview), nil
}

var rewindForkBefore = session.ForkBefore

func rewindRefusal(err error) (transport.RewindResult, error) {
	return transport.RewindResult{Kind: "refused", Reason: err.Error()}, nil
}

func serveRewindApply(home, sessionID, anchor, mode string) (transport.RewindResult, error) {
	if mode != "conversation" && mode != "files" && mode != "both" {
		return transport.RewindResult{}, fmt.Errorf("rewind mode 必须是 conversation/files/both")
	}
	src, err := readRewindSource(home, sessionID)
	if err != nil {
		return transport.RewindResult{}, err
	}
	seq, err := session.ResolveUserAnchor(src, anchor)
	if err != nil {
		return rewindRefusal(err)
	}
	// both 必须先证明对话可切，再动文件；非法/首个 anchor 不会留下半完成。
	if mode != "files" {
		if _, err := session.ValidateForkBefore(src, anchor); err != nil {
			return rewindRefusal(err)
		}
	}

	var files *transport.RewindPreview
	if mode != "conversation" {
		restored, err := session.RestoreRewindFiles(home, src.Header.ID, seq, src.Header.Cwd)
		if err != nil {
			return transport.RewindResult{}, err
		}
		files = rewindPreviewOf(restored)
		if mode == "files" {
			return transport.RewindResult{Kind: "rewound", SessionID: sessionID, Files: files}, nil
		}
	}

	newID := "session-" + session.NewID()
	forked, err := rewindForkBefore(src, home, src.Header.Cwd, newID, anchor)
	if err == nil {
		err = forked.Close()
	}
	if err != nil {
		if files != nil {
			// 文件已真实恢复；对话失败必须保留 partial outcome，不能回传新 id 冒充全成功。
			return transport.RewindResult{Kind: "rewound", Files: files, ConversationError: err.Error()}, nil
		}
		return transport.RewindResult{}, err
	}
	return transport.RewindResult{Kind: "rewound", SessionID: newID, Files: files}, nil
}

type durableTranscriptCursor struct {
	Version   int    `json:"v"`
	SessionID string `json:"sessionId"`
	Before    *int64 `json:"before"`
}

func encodeTranscriptCursor(sessionID string, before int64) (string, error) {
	raw, err := json.Marshal(durableTranscriptCursor{Version: 1, SessionID: sessionID, Before: &before})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeTranscriptCursor(value, sessionID string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, fmt.Errorf("%w: encoding", transport.ErrInvalidCursor)
	}
	var cur durableTranscriptCursor
	if err := json.Unmarshal(raw, &cur); err != nil || cur.Version != 1 || cur.SessionID != sessionID || cur.Before == nil || *cur.Before < 0 {
		return 0, fmt.Errorf("%w: session/version/boundary", transport.ErrInvalidCursor)
	}
	return *cur.Before, nil
}

// serveTranscriptPage 直接读 v4 durable 事件并复用 session.load 的 messagesOf
// 投影。cursor 只携带会话身份与页首 v4 seq，不读 Transport journal/history。
func serveTranscriptPage(home, sessionID, cursor string, limit int) (transport.TranscriptPageResult, error) {
	if limit <= 0 || limit > 200 {
		return transport.TranscriptPageResult{}, fmt.Errorf("transcript.page limit 必须在 1..200")
	}
	source := newSessionSource(home, "")
	path, err := source.resolve(sessionID)
	if err != nil {
		return transport.TranscriptPageResult{}, err
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		return transport.TranscriptPageResult{}, err
	}
	messages, err := source.messagesOf(lg.Events)
	if err != nil {
		return transport.TranscriptPageResult{}, err
	}
	before := int64(-1)
	if cursor != "" {
		before, err = decodeTranscriptCursor(cursor, lg.Header.ID)
		if err != nil {
			return transport.TranscriptPageResult{}, err
		}
		known := false
		for _, msg := range messages {
			if int64(msg.Seq) == before {
				known = true
				break
			}
		}
		if !known {
			return transport.TranscriptPageResult{}, fmt.Errorf("%w: durable message no longer exists", transport.ErrInvalidCursor)
		}
	}
	eligible := messages
	if before >= 0 {
		eligible = make([]transport.Message, 0, len(messages))
		for _, msg := range messages {
			if int64(msg.Seq) < before {
				eligible = append(eligible, msg)
			}
		}
	}
	if len(eligible) == 0 {
		return transport.TranscriptPageResult{Messages: []transport.Message{}, HasOlder: false, NextCursor: cursor}, nil
	}
	start := len(eligible) - limit
	if start < 0 {
		start = 0
	}
	page := append([]transport.Message(nil), eligible[start:]...)
	next, err := encodeTranscriptCursor(lg.Header.ID, int64(page[0].Seq))
	if err != nil {
		return transport.TranscriptPageResult{}, err
	}
	return transport.TranscriptPageResult{Messages: page, HasOlder: start > 0, NextCursor: next}, nil
}

// serveWorkspaceList 列出当前工作区的顶层条目。
//
// 为什么只列顶层：界面左栏要的是"这个工作区里有什么项目/目录"，
// 递归会让一次调用扫几万个文件——那是 grep 工具的事，不是这个 method 的事。
func serveWorkspaceList(path string) ([]transport.WorkspaceEntry, error) {
	// path 为空就用进程工作目录（旧行为）；给了就列那个目录。
	// **不再用进程 cwd 冒充会话 cwd**：多会话可能属于不同工作区，
	// 界面传进来的是 session.load.cwd，那才是"这条会话自己的"。
	dir := path
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("取当前目录失败: %w", err)
		}
		dir = wd
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读目录失败: %w", err)
	}
	out := make([]transport.WorkspaceEntry, 0, len(ents))
	for _, e := range ents {
		ent := transport.WorkspaceEntry{Name: e.Name(), IsDir: e.IsDir()}
		if info, ierr := e.Info(); ierr == nil && !e.IsDir() {
			ent.Size = info.Size()
		}
		out = append(out, ent)
	}
	return out, nil
}

// fsReadLimit 是 fs.read 的单文件上限。界面要的是"预览"，不是"下载"。
const fsReadLimit = 4 << 20

// serveFSRead 读工作区里的一个文件。
//
// 越界检查在这里做：`..` 跑出去、指向目录、超过上限，分别给不同的错误——
// 界面看到 404"文件不存在"和 500"读不了"应该做不同的事。
func serveFSRead(path string) (transport.FSContent, error) {
	if strings.TrimSpace(path) == "" {
		return transport.FSContent{}, fmt.Errorf("path 为空")
	}
	abs, err := filepath.Abs(filepath.FromSlash(path))
	if err != nil {
		return transport.FSContent{}, fmt.Errorf("路径解析失败: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return transport.FSContent{}, transport.ErrNoSuchFile
		}
		return transport.FSContent{}, fmt.Errorf("读不了: %w", err)
	}
	if st.IsDir() {
		return transport.FSContent{}, fmt.Errorf("这是一个目录，不是文件")
	}
	if st.Size() > fsReadLimit {
		return transport.FSContent{}, fmt.Errorf("文件 %d 字节，超过 %d 的上限；先用 bash 看片段", st.Size(), fsReadLimit)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return transport.FSContent{}, fmt.Errorf("读失败: %w", err)
	}
	return transport.FSContent{Path: path, Content: string(b), Size: int64(len(b))}, nil
}

// serveFSWrite 写工作区里的一个文件。
//
// 不做 read-before-write：那是**工具层**的策略（模型改文件前必须先读）。
// 界面是人手在改，人已经知道自己在改什么——把工具的策略套到人身上只会挡路。
func serveFSWrite(path, content string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path 为空")
	}
	abs, err := filepath.Abs(filepath.FromSlash(path))
	if err != nil {
		return fmt.Errorf("路径解析失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("创建父目录失败: %w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写失败: %w", err)
	}
	return nil
}

// serveConfigGet 读界面配置：一份扁平的 key→string 表，落 <home>/state/ui-config.json。
//
// 为什么不复用 dsh.toml：那份是**模块清单**，改错一个 id 就会少一半工具；
// 界面偏好（主题、语言）跟它是两种东西，混在一起出一次事故就够受。
func serveConfigGet(home string) (map[string]string, error) {
	p := filepath.Join(home, "state", "ui-config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("读配置失败: %w", err)
	}
	m := map[string]string{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("配置文件坏了（%s）: %w", p, err)
		}
	}
	return m, nil
}

// serveConfigSet 只覆盖给出的 key，其它项保持不动——界面每次只改自己关心的那几个。
func serveConfigSet(home string, kv map[string]string) error {
	cur, err := serveConfigGet(home)
	if err != nil {
		return err
	}
	for k, v := range kv {
		cur[k] = v
	}
	p := filepath.Join(home, "state", "ui-config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("建 state 目录失败: %w", err)
	}
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return fmt.Errorf("写配置失败: %w", err)
	}
	return nil
}
