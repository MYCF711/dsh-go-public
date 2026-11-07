package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// serveJournalFileName 与 v4 同目录，但它是 Transport 派生存储，不是第二份
// 会话事实源。serveDelete 删除会话目录时会一并清理；fork 不复制它。
const serveJournalFileName = "transport-journal.v1.jsonl"

const (
	serveJournalVersion = 1
	serveJournalFrame   = "frame"
	serveJournalReset   = "reset"
)

// serveJournalRecord 的 Data 用 base64（encoding/json 对 []byte 的标准编码），
// 而不是 json.RawMessage。后者在 Marshal 时会压缩/转义内部 JSON，无法保证
// “原始 data 字节逐字恢复”。nil data 仍省略，恢复后仍是 nil。
type serveJournalRecord struct {
	Version   int    `json:"version"`
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
	Seq       int    `json:"seq,omitempty"`
	Type      string `json:"type,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Time      int64  `json:"time,omitempty"`
}

type serveJournalTail struct {
	committedBytes int64
	observedBytes  int64
}

type serveJournalStore struct {
	home string
	mu   sync.Mutex
	// paths 缓存 sessionID → v4 路径；journal 固定在它的旁目录。
	paths map[string]string
	// tails 只记录未换行终结/未成功追加的尾巴。启动恢复绝不截断它；
	// 下一次写路径会先核对文件未被外部改动，再只截去这段未提交字节。
	tails map[string]serveJournalTail
}

// restoreServeJournals 是 cmdServe 启动阶段唯一的恢复入口。它只读已有文件；
// load/history/subscribe 不经过这里，也不会为了“修复”而写盘。
func restoreServeJournals(home string, journals *transport.Journals) (*serveJournalStore, error) {
	if journals == nil {
		return nil, fmt.Errorf("恢复 Transport journal：journals 为 nil")
	}
	s := &serveJournalStore{home: home, paths: map[string]string{}, tails: map[string]serveJournalTail{}}
	matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", serveJournalFileName))
	if err != nil {
		return nil, fmt.Errorf("扫描 Transport journal：%w", err)
	}
	for _, journalPath := range matches {
		v4Path := filepath.Join(filepath.Dir(journalPath), "session.v4.jsonl.zstd")
		head, err := v4.ReadHeader(v4Path)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 对应 v4 header：%w", journalPath, err)
		}
		if head.ID == "" {
			return nil, fmt.Errorf("%s 对应 v4 header 没有 session id", journalPath)
		}
		s.mu.Lock()
		_, duplicate := s.paths[head.ID]
		s.mu.Unlock()
		if duplicate {
			return nil, fmt.Errorf("会话 %s 有多份 Transport journal，拒绝猜测哪份有效", head.ID)
		}
		frames, committedBytes, observedBytes, err := readServeJournalFile(journalPath, head.ID)
		if err != nil {
			return nil, err
		}
		if err := journals.Restore(head.ID, frames); err != nil {
			return nil, fmt.Errorf("恢复 %s：%w", journalPath, err)
		}
		s.mu.Lock()
		s.paths[head.ID] = v4Path
		if observedBytes > committedBytes {
			s.tails[head.ID] = serveJournalTail{committedBytes: committedBytes, observedBytes: observedBytes}
		}
		s.mu.Unlock()
	}
	journals.SetPersistence(s)
	return s, nil
}

// readServeJournalFile 只接受换行终结的已提交记录。最后一个未终结片段是
// 进程强杀/断电留下的 torn tail：它从未向 Append 返回成功，恢复时只读跳过；
// 任何完整坏行或序号断裂都 fail loud，绝不补造 time/seq/data。
// committedBytes/observedBytes 交给下一次写路径核对并截去未提交尾巴，启动本身不写。
func readServeJournalFile(path, sessionID string) ([]transport.ServerFrame, int64, int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []transport.ServerFrame{}, 0, 0, nil
		}
		return nil, 0, 0, fmt.Errorf("读取 Transport journal %s：%w", path, err)
	}
	frames := make([]transport.ServerFrame, 0)
	lines := bytes.Split(raw, []byte{'\n'})
	var committedBytes int64
	for i, line := range lines {
		lineNumber := i + 1
		terminated := i < len(lines)-1
		if !terminated {
			break
		}
		committedBytes += int64(len(line) + 1)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec serveJournalRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, 0, 0, fmt.Errorf("%s 第 %d 行不是已提交的合法记录：%w", path, lineNumber, err)
		}
		if rec.Version != serveJournalVersion || rec.SessionID != sessionID {
			return nil, 0, 0, fmt.Errorf("%s 第 %d 行 version/sessionId 不符：version=%d sessionId=%q", path, lineNumber, rec.Version, rec.SessionID)
		}
		switch rec.Kind {
		case serveJournalReset:
			frames = make([]transport.ServerFrame, 0)
		case serveJournalFrame:
			wantSeq := len(frames) + 1
			if rec.Seq != wantSeq {
				return nil, 0, 0, fmt.Errorf("%s 第 %d 行 seq=%d，应为 %d", path, lineNumber, rec.Seq, wantSeq)
			}
			if rec.Type == "" || rec.Time <= 0 || (len(rec.Data) > 0 && !json.Valid(rec.Data)) {
				return nil, 0, 0, fmt.Errorf("%s 第 %d 行缺原始 type/time 或 data 非法", path, lineNumber)
			}
			frames = append(frames, transport.ServerFrame{
				Seq: rec.Seq, Type: rec.Type, Data: append(json.RawMessage(nil), rec.Data...), Time: rec.Time,
			})
		default:
			return nil, 0, 0, fmt.Errorf("%s 第 %d 行 kind=%q 未登记", path, lineNumber, rec.Kind)
		}
	}
	return frames, committedBytes, int64(len(raw)), nil
}

func (s *serveJournalStore) AppendJournalFrame(sessionID string, frame transport.ServerFrame) error {
	if frame.Seq <= 0 || frame.Type == "" || frame.Time <= 0 || (len(frame.Data) > 0 && !json.Valid(frame.Data)) {
		return fmt.Errorf("拒绝持久化不完整 Transport 帧：%+v", frame)
	}
	return s.appendRecord(serveJournalRecord{
		Version: serveJournalVersion, Kind: serveJournalFrame, SessionID: sessionID,
		Seq: frame.Seq, Type: frame.Type, Data: []byte(frame.Data), Time: frame.Time,
	})
}

func (s *serveJournalStore) ResetJournal(sessionID string) error {
	return s.appendRecord(serveJournalRecord{
		Version: serveJournalVersion, Kind: serveJournalReset, SessionID: sessionID,
	})
}

func (s *serveJournalStore) appendRecord(rec serveJournalRecord) error {
	if rec.SessionID == "" {
		return fmt.Errorf("Transport journal 缺 session id")
	}
	v4Path, err := s.sessionPath(rec.SessionID)
	if err != nil {
		return err
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化 Transport journal：%w", err)
	}
	line = append(line, '\n')
	path := filepath.Join(filepath.Dir(v4Path), serveJournalFileName)
	if tail, ok := s.journalTail(rec.SessionID); ok {
		// Windows 的 append-only 句柄不允许 Truncate（O_RDWR 也一样），所以
		// 修复使用独立的临时读写句柄，随后仍以 O_APPEND 打开正式写路径。
		repair, openErr := os.OpenFile(path, os.O_RDWR, 0)
		if openErr != nil {
			return fmt.Errorf("打开 Transport journal 尾巴修复句柄 %s：%w", path, openErr)
		}
		st, statErr := repair.Stat()
		if statErr != nil {
			_ = repair.Close()
			return fmt.Errorf("检查 Transport journal %s：%w", path, statErr)
		}
		// 启动只读恢复之后，文件若已被别的写者改动就 fail loud；绝不能
		// 按旧 offset 截掉别人刚提交的内容。核对通过也只去掉未提交尾巴。
		if st.Size() != tail.observedBytes {
			_ = repair.Close()
			return fmt.Errorf("Transport journal %s 在恢复后被外部改动：恢复时 %d 字节，现在 %d 字节", path, tail.observedBytes, st.Size())
		}
		truncateErr := repair.Truncate(tail.committedBytes)
		if truncateErr == nil {
			truncateErr = repair.Sync()
		}
		closeErr := repair.Close()
		if truncateErr != nil {
			return fmt.Errorf("截去 Transport journal 未提交尾巴 %s：%w", path, truncateErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭 Transport journal 尾巴修复句柄 %s：%w", path, closeErr)
		}
		// 修复已成功，但正式追加尚未开始；若后面失败，下次可幂等重试，
		// 不能继续保留修复前的 observedBytes 造成假的外部改动冲突。
		s.rememberTail(rec.SessionID, serveJournalTail{committedBytes: tail.committedBytes, observedBytes: tail.committedBytes})
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开 Transport journal %s：%w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("检查 Transport journal %s：%w", path, err)
	}
	committedBytes := st.Size()
	n, writeErr := f.Write(line)
	if writeErr == nil && n != len(line) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	observedBytes := committedBytes + int64(n)
	if after, statErr := f.Stat(); statErr == nil {
		observedBytes = after.Size()
	} else if writeErr == nil {
		writeErr = statErr
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		// Write/Sync/Close 任一失败都没有向 journal.Append 返回成功。记下
		// 未提交区间，下一次写先回到已提交前缀，避免把新 JSON 粘到半行后。
		if observedBytes > committedBytes {
			s.rememberTail(rec.SessionID, serveJournalTail{committedBytes: committedBytes, observedBytes: observedBytes})
		}
		if writeErr != nil {
			return fmt.Errorf("追加 Transport journal %s：%w", path, writeErr)
		}
		return fmt.Errorf("关闭 Transport journal %s：%w", path, closeErr)
	}
	s.clearTail(rec.SessionID)
	return nil
}

func (s *serveJournalStore) journalTail(sessionID string) (serveJournalTail, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tail, ok := s.tails[sessionID]
	return tail, ok
}

func (s *serveJournalStore) rememberTail(sessionID string, tail serveJournalTail) {
	s.mu.Lock()
	s.tails[sessionID] = tail
	s.mu.Unlock()
}

func (s *serveJournalStore) clearTail(sessionID string) {
	s.mu.Lock()
	delete(s.tails, sessionID)
	s.mu.Unlock()
}

func (s *serveJournalStore) sessionPath(sessionID string) (string, error) {
	s.mu.Lock()
	path := s.paths[sessionID]
	s.mu.Unlock()
	if path != "" {
		return path, nil
	}
	path, err := session.FindByID(s.home, sessionID)
	if err != nil {
		return "", fmt.Errorf("定位会话 %s 的 Transport journal：%w", sessionID, err)
	}
	s.mu.Lock()
	if existing := s.paths[sessionID]; existing != "" {
		path = existing
	} else {
		s.paths[sessionID] = path
	}
	s.mu.Unlock()
	return path, nil
}
