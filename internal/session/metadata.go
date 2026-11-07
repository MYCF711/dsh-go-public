package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dshgo/codecs/v4"
)

const sessionColorFileName = "session-color.json"

// sessionMetadataTransaction 是单向提交协议的可恢复 intent：先落本记录，
// 再追加 v4，最后提交读索引。记录成功后保留；读索引带同一 eventSeq 即表示
// 已完成。若进程在 v4 与索引之间退出，读取可凭它找回真实 v4 值，且绝不写盘。
type sessionMetadataTransaction struct {
	Version  int             `json:"version"`
	EventSeq int64           `json:"eventSeq"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}

type sessionMetadataIndex struct {
	Version  int     `json:"version"`
	EventSeq *int64  `json:"eventSeq,omitempty"`
	Title    *string `json:"title,omitempty"`
	Source   string  `json:"source,omitempty"`
	Color    *string `json:"color,omitempty"`
}

func metadataNames(eventType string) (indexName, transactionName string, err error) {
	switch eventType {
	case "session/title":
		return "session-title.json", "session-title.txn.json", nil
	case "session/color":
		return sessionColorFileName, "session-color.txn.json", nil
	default:
		return "", "", fmt.Errorf("未登记的 session metadata 类型 %q", eventType)
	}
}

func metadataPaths(sessionPath, eventType string) (string, string, error) {
	indexName, transactionName, err := metadataNames(eventType)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Dir(sessionPath)
	return filepath.Join(dir, indexName), filepath.Join(dir, transactionName), nil
}

// appendSessionMetadata 在真正 Append 前先修复上一份已入 v4、但未入索引的
// intent，防止后一次 mutation 覆盖恢复线索。任何一步失败都不发成功 ACK。
func appendSessionMetadata(l *Log, eventType string, data any) error {
	if l == nil {
		return fmt.Errorf("%s 需要打开的会话日志", eventType)
	}
	if err := repairSessionMetadata(l.Path, eventType); err != nil {
		return fmt.Errorf("恢复上一份 %s 索引失败: %w", eventType, err)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("序列化 %s: %w", eventType, err)
	}
	_, transactionPath, err := metadataPaths(l.Path, eventType)
	if err != nil {
		return err
	}
	tx := sessionMetadataTransaction{Version: 1, EventSeq: l.Seq(), Type: eventType, Data: payload}
	txRaw, err := json.Marshal(tx)
	if err != nil {
		return fmt.Errorf("序列化 %s intent: %w", eventType, err)
	}
	if err := writeSessionMetadataIndex(transactionPath, txRaw); err != nil {
		return fmt.Errorf("写 %s intent 失败（v4 尚未追加）: %w", eventType, err)
	}
	if err := l.Append(eventType, json.RawMessage(payload)); err != nil {
		return fmt.Errorf("写 %s 失败: %w", eventType, err)
	}
	index, err := metadataIndexFromTransaction(tx)
	if err != nil {
		return err
	}
	indexRaw, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("序列化 %s 索引: %w", eventType, err)
	}
	indexPath, _, _ := metadataPaths(l.Path, eventType)
	if err := writeSessionMetadataIndex(indexPath, indexRaw); err != nil {
		return fmt.Errorf("提交 %s 索引失败（intent 已保留，可从 v4 恢复）: %w", eventType, err)
	}
	return nil
}

// AppendSessionColor 追加官方 session/color 事件，并在返回前原子提交同目录
// 读索引。空串是一次真实的 clear，不会退化为删除其他会话的值或进程内 Map。
func AppendSessionColor(l *Log, color string) error {
	return appendSessionMetadata(l, "session/color", map[string]any{"color": color})
}

// ReadSessionColor 从指定会话自己的索引/intent 读 accent；未设置时返回空串。
// 已有但损坏且无法由 v4 恢复的索引必须报错，不能伪装成主题默认值。
func ReadSessionColor(sessionPath string) (string, error) {
	current, present, err := readSessionMetadata(sessionPath, "session/color")
	if err != nil {
		return "", err
	}
	if !present {
		return "", nil
	}
	return *current.Color, nil
}

func readMetadataIndex(sessionPath, eventType string) (*sessionMetadataIndex, bool, error) {
	indexPath, _, err := metadataPaths(sessionPath, eventType)
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(indexPath)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc sessionMetadataIndex
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, err
	}
	if doc.Version != 1 {
		return nil, false, fmt.Errorf("metadata 索引版本 %d 无效", doc.Version)
	}
	switch eventType {
	case "session/title":
		if doc.Title == nil {
			return nil, false, fmt.Errorf("title 索引缺 title")
		}
		if doc.Source == "" {
			doc.Source = "user" // W145 初版索引唯一来源就是显式用户标题。
		}
		if doc.Source != "user" && doc.Source != "auto" {
			return nil, false, fmt.Errorf("title 索引 source=%q 无效", doc.Source)
		}
	case "session/color":
		if doc.Color == nil {
			return nil, false, fmt.Errorf("color 索引缺 color")
		}
	}
	return &doc, true, nil
}

func readMetadataTransaction(sessionPath, eventType string) (*sessionMetadataTransaction, bool, error) {
	_, path, err := metadataPaths(sessionPath, eventType)
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var tx sessionMetadataTransaction
	if err := json.Unmarshal(raw, &tx); err != nil {
		return nil, false, err
	}
	if tx.Version != 1 || tx.Type != eventType || tx.EventSeq < 0 || !json.Valid(tx.Data) {
		return nil, false, fmt.Errorf("%s intent 版本/类型/eventSeq/data 无效", eventType)
	}
	return &tx, true, nil
}

func metadataIndexFromTransaction(tx sessionMetadataTransaction) (*sessionMetadataIndex, error) {
	var data struct {
		Title  *string `json:"title"`
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
		Color *string `json:"color"`
	}
	if err := json.Unmarshal(tx.Data, &data); err != nil {
		return nil, err
	}
	seq := tx.EventSeq
	doc := &sessionMetadataIndex{Version: 1, EventSeq: &seq, Title: data.Title, Color: data.Color, Source: data.Source.Kind}
	switch tx.Type {
	case "session/title":
		if doc.Title == nil || (doc.Source != "user" && doc.Source != "auto") {
			return nil, fmt.Errorf("session/title intent 缺 title/source")
		}
	case "session/color":
		if doc.Color == nil {
			return nil, fmt.Errorf("session/color intent 缺 color")
		}
	default:
		return nil, fmt.Errorf("未知 metadata intent %q", tx.Type)
	}
	return doc, nil
}

func sameMetadataValue(doc *sessionMetadataIndex, tx sessionMetadataTransaction) (bool, error) {
	fromTx, err := metadataIndexFromTransaction(tx)
	if err != nil {
		return false, err
	}
	if doc.EventSeq == nil || *doc.EventSeq != tx.EventSeq || doc.Version != fromTx.Version {
		return false, nil
	}
	switch tx.Type {
	case "session/title":
		return doc.Title != nil && *doc.Title == *fromTx.Title && doc.Source == fromTx.Source, nil
	case "session/color":
		return doc.Color != nil && *doc.Color == *fromTx.Color, nil
	}
	return false, nil
}

func metadataTransactionInV4(sessionPath string, tx sessionMetadataTransaction) (bool, error) {
	log, err := v4.ReadFile(sessionPath)
	if err != nil {
		return false, err
	}
	for _, event := range log.Events {
		if event.Seq == tx.EventSeq && event.Type == tx.Type && bytes.Equal(event.Data, tx.Data) {
			return true, nil
		}
	}
	return false, nil
}

// readSessionMetadata 只在 index 落后于 intent 时读 v4；正常路径仍是有界小索引。
// 这是只读恢复：不把修复写回磁盘，写回只发生在下一次真实 mutation 的开头。
func readSessionMetadata(sessionPath, eventType string) (*sessionMetadataIndex, bool, error) {
	index, indexPresent, indexErr := readMetadataIndex(sessionPath, eventType)
	tx, txPresent, txErr := readMetadataTransaction(sessionPath, eventType)
	if txErr != nil {
		return nil, false, txErr
	}
	if !txPresent {
		return index, indexPresent, indexErr
	}
	if indexErr == nil && indexPresent {
		same, err := sameMetadataValue(index, *tx)
		if err != nil {
			return nil, false, err
		}
		if same || (index.EventSeq != nil && *index.EventSeq > tx.EventSeq) {
			return index, true, nil
		}
	}
	matched, err := metadataTransactionInV4(sessionPath, *tx)
	if err != nil {
		return nil, false, err
	}
	if matched {
		recovered, err := metadataIndexFromTransaction(*tx)
		return recovered, true, err
	}
	if indexErr != nil {
		return nil, false, indexErr
	}
	return index, indexPresent, nil
}

// repairSessionMetadata 只由下一次写路径调用。恢复失败时拒绝覆盖旧 intent。
func repairSessionMetadata(sessionPath, eventType string) error {
	tx, present, err := readMetadataTransaction(sessionPath, eventType)
	if err != nil || !present {
		return err
	}
	index, indexPresent, indexErr := readMetadataIndex(sessionPath, eventType)
	if indexErr == nil && indexPresent {
		same, err := sameMetadataValue(index, *tx)
		if err != nil {
			return err
		}
		if same || (index.EventSeq != nil && *index.EventSeq > tx.EventSeq) {
			return nil
		}
	}
	matched, err := metadataTransactionInV4(sessionPath, *tx)
	if err != nil || !matched {
		return err
	}
	recovered, err := metadataIndexFromTransaction(*tx)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(recovered)
	if err != nil {
		return err
	}
	indexPath, _, _ := metadataPaths(sessionPath, eventType)
	return writeSessionMetadataIndex(indexPath, raw)
}

// writeSessionMetadataIndex 在同目录写临时文件、Sync、Close 后再原子替换。
// 随机临时名避免写者共享固定 .tmp；目录项断电提交仍需平台特定 dir fsync。
func writeSessionMetadataIndex(path string, raw []byte) (retErr error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".metadata-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	n, err := tmp.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmpPath, path)
}
