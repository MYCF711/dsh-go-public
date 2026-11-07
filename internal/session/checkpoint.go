package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dshgo/codecs/v4"
)

// 本文件是官方 0.2.2 插件 #78 `@deepseek-ai/dsh-session-checkpoint-policy`
// 的 Go 实现：**模型请求发出之前**，把这一轮要发出去的会话前缀落成一份
// 可复核的耐久快照，请求之后的补写一律不算。
//
// 官方原件（asar 只读解出，0.2.2-rc.1）：
//
//	dsh/node_modules/@deepseek-ai/dsh-session-checkpoint-policy/lib/index.js:26-31
//	  async function afterCheckpoint(ctx, session, next) {
//	      await ctx.sessions.flush(session);   // ① 先把完整请求前缀刷到耐久存储
//	      yield* next();                       // ② 再让下游适配器发第一个 chunk
//	  }
//
//	同文件 :55-57  “Checkpoint failures are fail-closed at the model and tool
//	side-effect boundaries: the downstream adapter or tool body is not invoked.”
//
// 官方 flush 的真身：dsh-session/lib/index.js:1970-1994 派发 `session/flush`，
// 任一 listener 失败即抛；dsh-session-persistence-jsonl/lib/index.js:426-434
// 的实现是 drain 缓冲 + fsync，失败时把批次按序留在缓冲里“retry and reject loudly”。
//
// 我方 v4 已经是**一帧一 fsync**（codecs/v4/write.go:162-165），所以这里缺的
// 不是“再刷一次盘”，而是“请求前那段前缀到底完整没有、之后有没有被改写”这件事
// 没有耐久结论。因此快照 = 独立侧车：
//
//	<home>/durability-checkpoints/<会话段>/<已覆盖事件数>/checkpoint.json
//
// 侧车记下此刻前缀的字节范围（sizeBytes）+ sha256 + 覆盖事件数 + 这次请求的身份；
// 侧车只**读** v4，绝不回写、绝不改写原始事件（R5：v4 append-only，读路径不写盘）。

const (
	// checkpointFormatVersion 是侧车格式版本；读到别的版本一律当损坏，不猜。
	checkpointFormatVersion = 1
	// checkpointMaxFileBytes 是侧车自身大小上限（防手写/损坏文件把内存吃满）。
	checkpointMaxFileBytes = 1 << 20
	// CheckpointRequestTextLimit 是侧车里保留的请求正文上限。
	//
	// 为什么保留正文而不是只留摘要：请求在飞的时候崩溃，这一轮的 user/message
	// 可能还没落进 v4（宿主 in-process runner 就是请求后才写盘）。只留摘要的话，
	// 崩溃后能证明“有过一次请求”，却不知道人到底说了什么。8 KiB 足够认出这一条，
	// 又不至于让每条轮次都在 home 里存一份长文；超过就截断并记 Truncated。
	CheckpointRequestTextLimit = 8 << 10
)

// ErrNoDurableCheckpoint 表示这条会话没有任何请求前快照。
//
// 调用方必须把它和“快照损坏”分开：没有快照是**没做过 checkpoint**（旧版本
// 或还没跑过一轮），损坏是**做过的快照对不上现在的字节**——后者更严重。
var ErrNoDurableCheckpoint = errors.New("durable checkpoint: 没有请求前快照")

// ErrDurableCheckpointCorrupt 表示快照存在但不可信（不是合法 JSON、身份对不上、
// 或者记下的字节范围与磁盘现状不一致）。
var ErrDurableCheckpointCorrupt = errors.New("durable checkpoint: 快照不可信")

// CheckpointRequest 是“这一轮要发出去的模型请求”的有界身份。
//
// 它不参与重放，也不进 v4；唯一用途是崩溃后回答“在飞的是哪一条请求”。
type CheckpointRequest struct {
	Kind string `json:"kind"` // "text" 或 "blocks"
	// SHA256 是请求载荷的完整摘要；Bytes 是载荷长度。
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	// Text 是载荷的有界头部（≤ CheckpointRequestTextLimit 字节，截在 UTF-8 边界）。
	Text string `json:"text,omitempty"`
	// Truncated 为真表示 Text 只是头部，完整内容见 SHA256/Bytes。
	Truncated bool `json:"truncated,omitempty"`
}

// DurableCheckpoint 是请求前快照的内容（侧车 checkpoint.json 的形状）。
type DurableCheckpoint struct {
	Version   int    `json:"version"`
	SessionID string `json:"sessionId"`
	// CoveredEvents 是快照覆盖的事件数（= 最后一帧 seq + 1，空会话为 0）。
	// 它同时是侧车目录名，所以“同一轮”= 同一 CoveredEvents，重复调用不会落第二份。
	CoveredEvents int64 `json:"coveredEvents"`
	// LastSeq 是最后一帧的 seq（CoveredEvents-1）；空会话为 -1。
	LastSeq int64 `json:"lastSeq"`
	// SessionPath 是 v4 会话文件的绝对路径；SessionID 取自文件 header，不是调用方传的串。
	SessionPath string `json:"sessionPath"`
	// SizeBytes/SHA256 锁定“请求前的前缀”：崩溃后按这两个值复核字节有没有被改写。
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
	Frames    int    `json:"frames"`
	CapturedAt int64 `json:"capturedAt"`
	Request   *CheckpointRequest `json:"request,omitempty"`
}

// RecoveredPrefix 是崩溃重启后能拿回来的东西：只读派生的 v4 帧 + 这次请求的身份。
//
// 它不改写 v4，也不“补写”缺失的事件：调用方拿到的是**当时确实已落盘**的那一段。
type RecoveredPrefix struct {
	Checkpoint DurableCheckpoint
	Header     v4.Header
	Events     []v4.Event
	// PrefixBytes 是复核通过的字节数（= Checkpoint.SizeBytes）。
	PrefixBytes int64
	// ExtraBytes 是快照之后又长出来的字节；TailTruncatedAt 非零表示那段尾部是半帧
	// （杀进程时正在写）。两者都**不影响**前缀：前缀已经单独复核过。
	ExtraBytes      int64
	TailTruncatedAt int
}

// NewCheckpointRequest 把请求载荷压成有界身份（摘要 + 长度 + 有界头部）。
func NewCheckpointRequest(kind string, payload []byte) CheckpointRequest {
	sum := sha256.Sum256(payload)
	head := payload
	truncated := false
	if len(head) > CheckpointRequestTextLimit {
		head = head[:CheckpointRequestTextLimit]
		truncated = true
	}
	// 截断只许落在合法 UTF-8 边界上，否则侧车 JSON 里会出现半个汉字。
	for len(head) > 0 && !utf8.Valid(head) {
		head = head[:len(head)-1]
	}
	return CheckpointRequest{
		Kind:      kind,
		SHA256:    hex.EncodeToString(sum[:]),
		Bytes:     int64(len(payload)),
		Text:      string(head),
		Truncated: truncated,
	}
}

// CheckpointDir 返回某会话某锚点的侧车目录（导出给运维/测试核对磁盘产物）。
func CheckpointDir(home, sessionID string, coveredEvents int64) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("durable checkpoint 需要 home")
	}
	if coveredEvents < 0 {
		return "", fmt.Errorf("durable checkpoint 的 coveredEvents 必须非负（实际 %d）", coveredEvents)
	}
	seg, err := v4.EncodeSegment(sessionID)
	if err != nil {
		return "", fmt.Errorf("编码 durable checkpoint 会话段: %w", err)
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("解析 durable checkpoint home: %w", err)
	}
	return filepath.Join(absHome, "durability-checkpoints", seg, strconv.FormatInt(coveredEvents, 10)), nil
}

// WriteDurableCheckpoint 在**发起模型请求之前**落一份前缀快照。
//
// 语义（与官方的逐条对照见 docs 与任务 RESULT）：
//   - 只读派生：整份 v4 读一次，前缀的 size/sha 与事件数都从这一次读里算出来；
//   - 前缀必须完整：尾部有半帧就拒绝（半个前缀不能当 checkpoint）；
//   - 幂等：同一 (会话, 覆盖事件数) 只落一份，已存在则复核身份后原样返回，不改字节；
//   - 并发：同进程按会话串行（A/B 会话互不影响），跨进程靠同名 rename 的原子发布；
//   - 失败响亮：任何一步失败都返回错误，并清掉自己的临时文件；不静默半写。
//
// 代价：每次调用整份读一遍 v4（前缀 sha256 必须覆盖全部前缀字节）。
// 一轮一次、十倍于消息文本的读取量级；不接受就先别叫它 durability。
func WriteDurableCheckpoint(home, sessionPath string, req *CheckpointRequest) (DurableCheckpoint, error) {
	if strings.TrimSpace(sessionPath) == "" {
		return DurableCheckpoint{}, fmt.Errorf("durable checkpoint 需要会话文件路径")
	}
	absPath, err := filepath.Abs(sessionPath)
	if err != nil {
		return DurableCheckpoint{}, fmt.Errorf("解析会话文件路径: %w", err)
	}
	// ① 只读派生（不加写锁、不改文件）：读一次，算前缀与事件数。
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return DurableCheckpoint{}, fmt.Errorf("durable checkpoint 读会话文件: %w", err)
	}
	cp, err := checkpointFromBytes(absPath, raw, req)
	if err != nil {
		return DurableCheckpoint{}, err
	}

	dir, err := CheckpointDir(home, cp.SessionID, cp.CoveredEvents)
	if err != nil {
		return DurableCheckpoint{}, err
	}
	// ② 同进程内按会话串行：同一轮的并发调用只允许一个发布。
	unlock := lockCheckpointSession(absPath)
	defer unlock()

	if existing, err := readCheckpointFile(dir); err == nil {
		if err := sameCheckpointAnchor(existing, cp); err != nil {
			return DurableCheckpoint{}, err
		}
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return DurableCheckpoint{}, err
	}
	// ③ 原子发布：tmp 写完 fsync 才 rename，失败清 tmp。
	if err := publishCheckpoint(dir, cp); err != nil {
		return DurableCheckpoint{}, err
	}
	return cp, nil
}

// checkpointFromBytes 是“会话字节 → 快照”的唯一判据。
func checkpointFromBytes(absPath string, raw []byte, req *CheckpointRequest) (DurableCheckpoint, error) {
	lg, err := v4.Read(raw)
	if err != nil {
		return DurableCheckpoint{}, fmt.Errorf("durable checkpoint 解析会话前缀: %w", err)
	}
	if lg.Header.ID == "" {
		return DurableCheckpoint{}, fmt.Errorf("%w: 会话 header 没有 id", ErrDurableCheckpointCorrupt)
	}
	if lg.TailTruncatedAt != 0 {
		return DurableCheckpoint{}, fmt.Errorf(
			"%w: 会话文件尾部有半帧（offset=%d）——半个前缀不能当请求前快照", ErrDurableCheckpointCorrupt, lg.TailTruncatedAt)
	}
	for i, ev := range lg.Events {
		if ev.Seq != int64(i) {
			return DurableCheckpoint{}, fmt.Errorf(
				"%w: 第 %d 个事件 seq=%d（seq 必须文件内连续）", ErrDurableCheckpointCorrupt, i, ev.Seq)
		}
	}
	sum := sha256.Sum256(raw)
	cp := DurableCheckpoint{
		Version:       checkpointFormatVersion,
		SessionID:     lg.Header.ID,
		CoveredEvents: int64(len(lg.Events)),
		LastSeq:       int64(len(lg.Events)) - 1,
		SessionPath:   absPath,
		SizeBytes:     int64(len(raw)),
		SHA256:        hex.EncodeToString(sum[:]),
		Frames:        lg.Frames,
		CapturedAt:    time.Now().UnixMilli(),
	}
	if cp.SizeBytes == 0 {
		return DurableCheckpoint{}, fmt.Errorf("%w: 会话文件是空的", ErrDurableCheckpointCorrupt)
	}
	if req != nil {
		normalized := *req
		if normalized.Kind != "text" && normalized.Kind != "blocks" {
			return DurableCheckpoint{}, fmt.Errorf("durable checkpoint 的请求种类必须是 text/blocks（实际 %q）", normalized.Kind)
		}
		if len(normalized.SHA256) != sha256.Size*2 || normalized.Bytes < 0 {
			return DurableCheckpoint{}, fmt.Errorf("durable checkpoint 的请求身份不合法（sha256=%q bytes=%d）", normalized.SHA256, normalized.Bytes)
		}
		cp.Request = &normalized
	}
	return cp, nil
}

// sameCheckpointAnchor 判定“磁盘上已有的这份快照能不能代表这一次请求”。
//
// 同一 (会话, 覆盖事件数) 认为是同一轮：前缀一模一样，请求身份允许不同
// （重试时文案可能改过），先到者赢 —— 要求是“同一轮只落一份”，不是“每次覆盖”。
func sameCheckpointAnchor(existing, want DurableCheckpoint) error {
	if existing.Version != want.Version {
		return fmt.Errorf("%w: 侧车版本 %d，期望 %d", ErrDurableCheckpointCorrupt, existing.Version, want.Version)
	}
	if existing.SessionID != want.SessionID {
		return fmt.Errorf("%w: 侧车会话 %q，期望 %q", ErrDurableCheckpointCorrupt, existing.SessionID, want.SessionID)
	}
	if existing.CoveredEvents != want.CoveredEvents || existing.LastSeq != want.LastSeq {
		return fmt.Errorf("%w: 侧车覆盖 %d 帧（last=%d），期望 %d 帧（last=%d）",
			ErrDurableCheckpointCorrupt, existing.CoveredEvents, existing.LastSeq, want.CoveredEvents, want.LastSeq)
	}
	if existing.SessionPath != want.SessionPath {
		return fmt.Errorf("%w: 侧车记的会话路径 %q，期望 %q", ErrDurableCheckpointCorrupt, existing.SessionPath, want.SessionPath)
	}
	if existing.SizeBytes != want.SizeBytes || existing.SHA256 != want.SHA256 {
		return fmt.Errorf("%w: 同一锚点的前缀已经变了（%d/%s ≠ %d/%s）：同一轮不允许落两份不同的快照",
			ErrDurableCheckpointCorrupt, existing.SizeBytes, existing.SHA256, want.SizeBytes, want.SHA256)
	}
	return nil
}

// publishCheckpoint 原子发布侧车：tmp → fsync → rename。任何失败都清掉自己的 tmp。
func publishCheckpoint(dir string, cp DurableCheckpoint) error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("durable checkpoint 序列化: %w", err)
	}
	createdDir, err := ensureCheckpointDir(dir)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf("checkpoint-%d-%d.tmp", os.Getpid(), time.Now().UnixNano()))
	cleanup := func() {
		_ = os.Remove(tmp)
		if createdDir {
			// 只删“自己建、且还是空”的目录：绝不 RemoveAll——那会连带删掉
			// 另一个进程刚发布好的快照。
			_ = os.Remove(dir)
			_ = os.Remove(filepath.Dir(dir))
		}
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return fmt.Errorf("durable checkpoint 建临时文件: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("durable checkpoint 写临时文件: %w", err)
	}
	// 先 fsync 再 rename：rename 之后读者看到的必须是已经落盘的字节。
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("durable checkpoint fsync 临时文件: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("durable checkpoint 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "checkpoint.json")); err != nil {
		// 跨进程并发发布：另一个进程已经抢到了。只要那边读得通，就算幂等成功。
		if rev, revErr := readCheckpointFile(dir); revErr == nil {
			_ = os.Remove(tmp)
			if err := sameCheckpointAnchor(rev, cp); err != nil {
				return err
			}
			return nil
		}
		cleanup()
		return fmt.Errorf("durable checkpoint 发布: %w", err)
	}
	if err := syncCheckpointDir(dir); err != nil {
		// 这里已经 rename 成功：侧车文件确实在了，只是目录项没拿到 fsync 确认。
		// 仍然**响亮失败**（调用方因此不派发模型请求）；文件本身自洽，
		// 下一次同锚点调用会读到它并当作幂等命中，不会因此产生第二份。
		cleanup()
		return fmt.Errorf("durable checkpoint fsync 目录: %w", err)
	}
	return nil
}

// ensureCheckpointDir 建目录，并告知“是不是本次创建的”（决定失败时能不能回滚）。
func ensureCheckpointDir(dir string) (bool, error) {
	if _, err := os.Lstat(dir); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("durable checkpoint 检查目录: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("durable checkpoint 建目录: %w", err)
	}
	// MkdirAll 期间别的进程可能也建过：以“最终是否由本次创建”为准要靠 Mkdir。
	// 这里保守返回 true：回滚只会删空目录，删不掉非空目录，最坏是无操作。
	return true, nil
}

// syncCheckpointDir 让目录项也落盘（POSIX）。Windows 没有这条契约。
func syncCheckpointDir(dir string) error {
	if runtime.GOOS == "windows" {
		// 官方 JSONL 后端在 Windows 上同样放弃目录 fsync，改用
		// MoveFileExW(..., MOVEFILE_WRITE_THROUGH) 发布
		// （dsh-session-persistence-jsonl/lib/index.js:445-460）。Go 标准库没有那个开关，
		// 这里只能保证“文件本身已 fsync + 同名 rename 发布”，目录项交给 OS。
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// readCheckpointFile 读并校验侧车；不存在返回 os.ErrNotExist（调用方据此判“没做过”）。
func readCheckpointFile(dir string) (DurableCheckpoint, error) {
	var cp DurableCheckpoint
	raw, err := rewindReadLimited(filepath.Join(dir, "checkpoint.json"), checkpointMaxFileBytes)
	if err != nil {
		return cp, err
	}
	if err := json.Unmarshal(raw, &cp); err != nil {
		return cp, fmt.Errorf("%w: 侧车不是合法 JSON: %v", ErrDurableCheckpointCorrupt, err)
	}
	if err := validateCheckpoint(cp); err != nil {
		return DurableCheckpoint{}, err
	}
	return cp, nil
}

func validateCheckpoint(cp DurableCheckpoint) error {
	if cp.Version != checkpointFormatVersion {
		return fmt.Errorf("%w: 侧车版本 %d，期望 %d", ErrDurableCheckpointCorrupt, cp.Version, checkpointFormatVersion)
	}
	if cp.SessionID == "" || cp.SessionPath == "" {
		return fmt.Errorf("%w: 侧车缺会话身份", ErrDurableCheckpointCorrupt)
	}
	if cp.CoveredEvents < 0 || cp.LastSeq != cp.CoveredEvents-1 {
		return fmt.Errorf("%w: 覆盖事件数 %d 与 lastSeq %d 不自洽", ErrDurableCheckpointCorrupt, cp.CoveredEvents, cp.LastSeq)
	}
	if cp.SizeBytes <= 0 || cp.Frames < 1 {
		return fmt.Errorf("%w: 前缀字节数 %d / 帧数 %d 不合法", ErrDurableCheckpointCorrupt, cp.SizeBytes, cp.Frames)
	}
	if len(cp.SHA256) != sha256.Size*2 {
		return fmt.Errorf("%w: 前缀摘要不是 sha256", ErrDurableCheckpointCorrupt)
	}
	if _, err := hex.DecodeString(cp.SHA256); err != nil {
		return fmt.Errorf("%w: 前缀摘要不是十六进制: %v", ErrDurableCheckpointCorrupt, err)
	}
	if cp.Request != nil {
		if cp.Request.Kind != "text" && cp.Request.Kind != "blocks" {
			return fmt.Errorf("%w: 请求种类 %q 不认识", ErrDurableCheckpointCorrupt, cp.Request.Kind)
		}
		if len(cp.Request.SHA256) != sha256.Size*2 || cp.Request.Bytes < 0 {
			return fmt.Errorf("%w: 请求身份不合法", ErrDurableCheckpointCorrupt)
		}
	}
	return nil
}

// ReadDurableCheckpoint 读某会话某锚点的侧车。会话 id 的两种写法（带/不带
// "session-" 前缀）都认，与 sessionSource.resolve 的判据一致。
func ReadDurableCheckpoint(home, sessionID string, coveredEvents int64) (DurableCheckpoint, error) {
	var lastErr error
	for _, id := range CandidateIDs(sessionID) {
		dir, err := CheckpointDir(home, id, coveredEvents)
		if err != nil {
			lastErr = err
			continue
		}
		cp, err := readCheckpointFile(dir)
		if err == nil {
			return cp, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return DurableCheckpoint{}, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return DurableCheckpoint{}, lastErr
}

// ListDurableCheckpoints 列出某会话的全部快照，按覆盖事件数升序。
func ListDurableCheckpoints(home, sessionID string) ([]DurableCheckpoint, error) {
	seen := make(map[string]bool)
	out := make([]DurableCheckpoint, 0, 4)
	for _, id := range CandidateIDs(sessionID) {
		seg, err := v4.EncodeSegment(id)
		if err != nil {
			continue
		}
		absHome, err := filepath.Abs(home)
		if err != nil {
			return nil, fmt.Errorf("解析 durable checkpoint home: %w", err)
		}
		root := filepath.Join(absHome, "durability-checkpoints", seg)
		entries, err := os.ReadDir(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("列 durable checkpoint: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			cp, err := readCheckpointFile(filepath.Join(root, entry.Name()))
			if err != nil {
				return nil, err
			}
			if seen[cp.SessionPath+"/"+strconv.FormatInt(cp.CoveredEvents, 10)] {
				continue
			}
			seen[cp.SessionPath+"/"+strconv.FormatInt(cp.CoveredEvents, 10)] = true
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CoveredEvents != out[j].CoveredEvents {
			return out[i].CoveredEvents < out[j].CoveredEvents
		}
		return out[i].SessionPath < out[j].SessionPath
	})
	return out, nil
}

// RecoverDurableCheckpoint 是“杀进程后重启”的恢复入口：取最新一份快照，
// **只读**打开当时的会话文件，逐字节复核前缀，再把能读的帧交出来。
//
// 前缀之后的字节（快照之后又长出来的、或被杀时写一半的）单独报，
// 不允许因为它们否定前缀——那正是这份快照存在的理由。
func RecoverDurableCheckpoint(home, sessionID string) (RecoveredPrefix, error) {
	cps, err := ListDurableCheckpoints(home, sessionID)
	if err != nil {
		return RecoveredPrefix{}, err
	}
	if len(cps) == 0 {
		return RecoveredPrefix{}, fmt.Errorf("%w: 会话 %q", ErrNoDurableCheckpoint, sessionID)
	}
	cp := cps[len(cps)-1]
	raw, err := os.ReadFile(cp.SessionPath)
	if err != nil {
		return RecoveredPrefix{}, fmt.Errorf("durable checkpoint 恢复读会话文件: %w", err)
	}
	if int64(len(raw)) < cp.SizeBytes {
		return RecoveredPrefix{}, fmt.Errorf("%w: 会话文件 %d 字节短于快照记的 %d 字节（前缀被截断）",
			ErrDurableCheckpointCorrupt, len(raw), cp.SizeBytes)
	}
	prefix := raw[:cp.SizeBytes]
	sum := sha256.Sum256(prefix)
	if got := hex.EncodeToString(sum[:]); got != cp.SHA256 {
		return RecoveredPrefix{}, fmt.Errorf("%w: 前缀已被改写（磁盘 %s，快照 %s）", ErrDurableCheckpointCorrupt, got, cp.SHA256)
	}
	lg, err := v4.Read(prefix)
	if err != nil {
		return RecoveredPrefix{}, fmt.Errorf("%w: 前缀读不出来: %v", ErrDurableCheckpointCorrupt, err)
	}
	if lg.TailTruncatedAt != 0 || int64(len(lg.Events)) != cp.CoveredEvents {
		return RecoveredPrefix{}, fmt.Errorf("%w: 前缀有半帧或事件数 %d 与快照 %d 不符",
			ErrDurableCheckpointCorrupt, len(lg.Events), cp.CoveredEvents)
	}
	out := RecoveredPrefix{
		Checkpoint:  cp,
		Header:      lg.Header,
		Events:      lg.Events,
		PrefixBytes: cp.SizeBytes,
		ExtraBytes:  int64(len(raw)) - cp.SizeBytes,
	}
	if out.ExtraBytes > 0 {
		// 只作报告用：快照之后的尾部可能就是一个半帧，不影响前缀结论。
		if full, ferr := v4.Read(raw); ferr == nil {
			out.TailTruncatedAt = full.TailTruncatedAt
		}
	}
	return out, nil
}

// checkpointLocks 按会话文件串行化快照发布（键与轮次无关，键数 = 会话数）。
var checkpointLocks sync.Map

func lockCheckpointSession(sessionPath string) func() {
	value, _ := checkpointLocks.LoadOrStore(sessionPath, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// bytesEqualHex 是给调用方（测试/运维）复核前缀用的最小工具，
// 避免它们各自手写 sha256 比较。
func bytesEqualHex(data []byte, hexSum string) bool {
	sum := sha256.Sum256(data)
	return bytes.Equal([]byte(hex.EncodeToString(sum[:])), []byte(strings.ToLower(hexSum)))
}
