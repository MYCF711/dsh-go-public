package schedule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound 表示会话文件或指定 id 不存在。重复删除也走这里，绝不伪造 deleted=true。
	ErrNotFound = errors.New("schedule not found")
	// ErrRevisionConflict 表示调用方读到的文件已被别的写入者换掉。
	ErrRevisionConflict = errors.New("schedule revision conflict")
)

// mutationMu 串行化本进程内所有日程读改写。目录锁不能假装成跨进程事务；
// expectedRevision 另提供内容哈希 fence，批量清理由全量预检+失败回滚保护。
var mutationMu sync.Mutex

// LockMutations / UnlockMutations 把包内那把读改写锁交给**跨包**写者共用。
//
// 为什么必须共用一把：`schedule.delete`（本包 Delete）与 `schedule.edit`
// （cmd/dshd 的官方实现）做的是同一件事——读整份文件、改一条、原子替换。
// 两把锁各管一半就等于两个写者互相看不见，后写的那个会把先写的改动整条覆盖掉。
//
// 这一对是给宿主侧调用者用的；包内函数（Delete/ClearEnded/Advance）自己已经加锁，
// 持锁期间再调它们会死锁——调用方只能在持锁期间做自己的读改写并直接落盘。
func LockMutations() { mutationMu.Lock() }

// UnlockMutations 释放 LockMutations 拿到的锁。
func UnlockMutations() { mutationMu.Unlock() }

// DeleteResult 是一条真实删除完成后的持久状态。
type DeleteResult struct {
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	Deleted   bool   `json:"deleted"`
	Remaining int    `json:"remaining"`
	Revision  string `json:"revision"`
}

// ClearResult 是官方 clear-ended/deleteInactive 对应的持久结果。
// Revisions 只含实际改写的会话文件，供调用方核对 readback。
type ClearResult struct {
	Deleted   int               `json:"deleted"`
	Revisions map[string]string `json:"revisions"`
}

type mutationDocument struct {
	path    string
	session string
	raw     []byte
	mode    os.FileMode
	body    map[string]json.RawMessage
	entries []json.RawMessage
}

// Delete 原子删除一个会话文件里的指定 id。expectedRevision 非空时是旧文件字节的 SHA-256；
// 空串表示调用方没有做乐观锁。任何读、校验或写失败都不会把失败报成删除成功。
func Delete(dir, sessionID, id, expectedRevision string) (DeleteResult, error) {
	mutationMu.Lock()
	defer mutationMu.Unlock()
	if err := validateMutationSession(sessionID); err != nil {
		return DeleteResult{}, err
	}
	if strings.TrimSpace(id) == "" {
		return DeleteResult{}, fmt.Errorf("schedule id 不能为空")
	}
	doc, err := readMutationDocument(dir, sessionID)
	if err != nil {
		return DeleteResult{}, err
	}
	if expectedRevision != "" && revisionBytes(doc.raw) != expectedRevision {
		return DeleteResult{}, fmt.Errorf("%w: session=%s want=%s got=%s", ErrRevisionConflict, sessionID, expectedRevision, revisionBytes(doc.raw))
	}
	idx := -1
	for i, raw := range doc.entries {
		var item Item
		if err := json.Unmarshal(raw, &item); err != nil {
			return DeleteResult{}, fmt.Errorf("解析定时任务 %s 第 %d 条失败: %w", doc.path, i, err)
		}
		if item.ID != id {
			continue
		}
		if idx >= 0 {
			return DeleteResult{}, fmt.Errorf("定时任务文件 %s 有重复 id %q，拒绝猜测删除", doc.path, id)
		}
		idx = i
	}
	if idx < 0 {
		return DeleteResult{}, fmt.Errorf("%w: session=%s id=%s", ErrNotFound, sessionID, id)
	}
	entries := append(append([]json.RawMessage(nil), doc.entries[:idx]...), doc.entries[idx+1:]...)
	raw, err := marshalMutationDocument(doc.body, entries)
	if err != nil {
		return DeleteResult{}, err
	}
	if err := writeFileAtomic(doc.path, raw, doc.mode); err != nil {
		return DeleteResult{}, fmt.Errorf("原子替换 %s 失败，旧文件保持不动: %w", doc.path, err)
	}
	return DeleteResult{SessionID: sessionID, ID: id, Deleted: true, Remaining: len(entries), Revision: revisionBytes(raw)}, nil
}

// ClearEnded 只删除 Host 明确判定为 ended 的记录：status="ended"，或带已到期 ended_at。
// running/pending/active 以及本仓 every/daily/weekly 周期任务都保留。now 只用于 ended_at，
// 绝不拿 next_at 已过冒充 ended（那可能只是一个还没被 Advance 的到期任务）。
func ClearEnded(dir string, now time.Time) (ClearResult, error) {
	mutationMu.Lock()
	defer mutationMu.Unlock()
	result := ClearResult{Revisions: map[string]string{}}
	sessions, err := Sessions(dir)
	if err != nil {
		return result, err
	}
	docs := make([]*mutationDocument, 0, len(sessions))
	changed := make([]bool, len(sessions))
	newRaw := make([][]byte, len(sessions))
	// 全量预检：任何文件损坏或条目无法判定，都必须在第一个写入前失败。
	for i, sessionID := range sessions {
		doc, err := readMutationDocument(dir, sessionID)
		if err != nil {
			return result, err
		}
		docs = append(docs, doc)
		kept := make([]json.RawMessage, 0, len(doc.entries))
		for j, raw := range doc.entries {
			var item Item
			if err := json.Unmarshal(raw, &item); err != nil {
				return result, fmt.Errorf("解析定时任务 %s 第 %d 条失败: %w", doc.path, j, err)
			}
			ended := item.Status == "ended"
			if !ended && item.EndedAt != "" {
				stamp, err := time.Parse(time.RFC3339, item.EndedAt)
				if err != nil {
					return result, fmt.Errorf("定时任务 %s 的 ended_at 无效: %w", item.ID, err)
				}
				ended = !stamp.After(now)
			}
			if ended {
				result.Deleted++
				changed[i] = true
				continue
			}
			kept = append(kept, raw)
		}
		if changed[i] {
			newRaw[i], err = marshalMutationDocument(doc.body, kept)
			if err != nil {
				return result, err
			}
		}
	}

	committed := make([]int, 0, len(docs))
	for i, doc := range docs {
		if !changed[i] {
			continue
		}
		if err := writeFileAtomic(doc.path, newRaw[i], doc.mode); err != nil {
			rollbackErr := rollbackMutationFiles(docs, committed)
			if rollbackErr != nil {
				return ClearResult{Revisions: map[string]string{}}, errors.Join(
					fmt.Errorf("写 %s 失败: %w", doc.path, err),
					fmt.Errorf("回滚已写日程文件也失败: %w", rollbackErr))
			}
			return ClearResult{Revisions: map[string]string{}}, fmt.Errorf("写 %s 失败，已回滚本次全部日程写入: %w", doc.path, err)
		}
		committed = append(committed, i)
		result.Revisions[doc.session] = revisionBytes(newRaw[i])
	}
	if result.Deleted == 0 {
		result.Revisions = map[string]string{}
	}
	return result, nil
}

func rollbackMutationFiles(docs []*mutationDocument, committed []int) error {
	var errs []error
	for i := len(committed) - 1; i >= 0; i-- {
		doc := docs[committed[i]]
		if err := writeFileAtomic(doc.path, doc.raw, doc.mode); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", doc.path, err))
		}
	}
	return errors.Join(errs...)
}

func readMutationDocument(dir, sessionID string) (*mutationDocument, error) {
	if err := validateMutationSession(sessionID); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sessionID+".json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: session=%s", ErrNotFound, sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("读定时任务文件 %s 失败: %w", path, err)
	}
	if err := ValidateScheduleVersion(raw); err != nil {
		return nil, fmt.Errorf("定时任务文件 %s 版本不兼容: %w", path, err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("定时任务文件 %s 不是合法 JSON 对象: %w", path, err)
	}
	if body == nil {
		return nil, fmt.Errorf("定时任务文件 %s 不是合法 JSON 对象", path)
	}
	entries := []json.RawMessage{}
	if schedules, ok := body["schedules"]; ok && string(schedules) != "null" {
		if err := json.Unmarshal(schedules, &entries); err != nil {
			return nil, fmt.Errorf("定时任务文件 %s 的 schedules 不是数组: %w", path, err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &mutationDocument{path: path, session: sessionID, raw: raw, mode: info.Mode().Perm(), body: body, entries: entries}, nil
}

func marshalMutationDocument(body map[string]json.RawMessage, entries []json.RawMessage) ([]byte, error) {
	if entries == nil {
		entries = []json.RawMessage{}
	}
	body["version"] = json.RawMessage(fmt.Sprint(currentScheduleVersion))
	schedules, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("序列化 schedules 失败: %w", err)
	}
	body["schedules"] = schedules
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化定时任务失败: %w", err)
	}
	return append(raw, '\n'), nil
}

// writeFileAtomic 使用独占临时文件、文件 Sync 和同目录 rename。已存在 .tmp 不删不覆盖，
// 因为无法证明它属于本次写入；调用方会收到失败并保留旧文件。
func writeFileAtomic(path string, raw []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		_ = f.Close()
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	removeTemp = false
	return nil
}

// Revision 返回当前持久文件的 SHA-256，供 schedule.list 与 expectedRevision 配对使用。
func Revision(dir, sessionID string) (string, error) {
	if err := validateMutationSession(sessionID); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(dir, sessionID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: session=%s", ErrNotFound, sessionID)
	}
	if err != nil {
		return "", err
	}
	return revisionBytes(raw), nil
}

func revisionBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateMutationSession(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" || sessionID == "." || sessionID == ".." ||
		strings.ContainsAny(sessionID, `/\\:*?"<>|`) || strings.ContainsAny(sessionID, "\r\n\t") {
		return fmt.Errorf("sessionId %q 为空或含路径分隔符/控制字符", sessionID)
	}
	return nil
}
