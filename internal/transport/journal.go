package transport

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Journal 是**每会话一条**的 Transport 序号域（H14）。
//
// 为什么必须有它：在这之前 seq 有三个互不相干的来源——
//   - 事件帧用 v4 的 `ev.Seq`（那是对外表 v4 日志的序号）
//   - 结局帧用全局 `serveTurnSeq`（所有会话共用一个计数器）
//   - 客户端又按自己收到的顺序数
//
// 三套序号放在一起，客户端「发现断号」的判断就是假的：要么永远不缺，要么永远缺。
// 契约（docs/transport-contract.md §2）要求的是：**每个会话一条 journal，
// 事件只在追加时分配一次单调 seq，WS 与 session.history 读同一份。**
//
// 所以这个类型只做一件事：给一条会话的 Transport 帧发号、存着、按区间取。
type Journal struct {
	mu       sync.Mutex
	epoch    int64
	seq      int
	frames   []ServerFrame
	capacity int
	// waiters 是"等下一帧"的广播表（见 Wait）。每个等待者一个 channel，
	// 追加时**关闭并摘掉**：关闭是广播，摘掉保证不会二次关闭。
	waiters map[chan struct{}]struct{}
}

// SinceNothing 是 sinceSeq 的"调用方什么都没拿"哨兵值。
//
// 为什么需要它：Window 的判据是 `Seq > sinceSeq`（排他），所以"从头开始"不能用 0
// ——0 表示"我已经有第 0 帧了"，第 0 帧就永远取不到。官方 `session.wait` 的
// `afterSeq` 用的是同一个约定（`afterSeq ?? -1`，且显式要求 `>= -1`），
// 本仓三条面（WS `sub`、mux 流、官方 follow）统一用 -1，不各自发明。
const SinceNothing = -1

// NewJournal 造一条空 journal。
//
// capacity 是内存里保留多少帧供 history 补洞；超出就从头丢（丢最老的）。
// 有上限是刻意的：**补洞是有限度的**，超出范围时客户端应该重新 load 首屏，
// 而不是指望宿主把整场会话一直留在内存里。
func NewJournal(epoch int64, capacity int) *Journal {
	if capacity <= 0 {
		capacity = 4096
	}
	return &Journal{epoch: epoch, capacity: capacity}
}

// Epoch 是这条 journal 的世代号。
//
// 宿主重启后内存 journal 由持久化记录重建，seq 可以续旧水位，但 epoch
// 必须换新。客户端仍可能拿着上一进程的世代；世代号跟着 load/history/sub
// 走，不匹配就要求重建基线，不能把两个进程的客户端状态静默混用。
func (j *Journal) Epoch() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.epoch
}

// LastSeq 是已分配到的最大 seq（0 表示还没发过帧）。
func (j *Journal) LastSeq() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq
}

// Append 给一帧发号并入账，返回**带 seq 的**那一帧。
//
// 发号与入账在同一把锁里：分开做的话，两个 goroutine 可能拿到相同号码，
// 或者客户端从 WS 收到的顺序与 history 里的顺序不一致。
// 生产写路径使用 Journals.Append，把同一把锁里的持久化也纳入原子步骤。
func (j *Journal) Append(typ string, data json.RawMessage, t int64) ServerFrame {
	f, _ := j.append(typ, data, t, nil)
	return f
}

// append 先提交可选的持久化记录，再推进内存水位。任何写盘错误都不占 seq，
// 更不会给 WS 一个重启后无法恢复的“成功”帧。
func (j *Journal) append(typ string, data json.RawMessage, t int64, persist func(ServerFrame) error) (ServerFrame, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f := ServerFrame{Seq: j.seq + 1, Type: typ, Data: data, Time: t}
	if persist != nil {
		if err := persist(f); err != nil {
			return ServerFrame{}, err
		}
	}
	j.seq = f.Seq
	j.frames = append(j.frames, f)
	j.trimLocked()
	j.broadcastLocked()
	return f, nil
}

// broadcastLocked 关掉并摘掉所有等待者的 channel。
//
// 关闭而非发送：等待者不需要"一条消息"，只需要"水位变了，回去读 Window"。
// 关掉之后等待者必须重新 Wait 才会被下一次追加唤醒——所以这里只有已注册的
// channel，不会往一个没人读的 channel 上阻塞。
func (j *Journal) broadcastLocked() {
	if len(j.waiters) == 0 {
		return
	}
	for ch := range j.waiters {
		close(ch)
	}
	j.waiters = nil
}

// Wait 返回一个**在下一次追加时关闭**的 channel。
//
// 用途：把"轮询"换成"推送"。调用方拿到的 channel 只保证一件事——
// 关闭时水位可能变了；它必须自己回去读 Window 并重新 Wait。
//
// 用法（缺一帧都不能丢，所以必须在读完之后再注册）：
//
//	for {
//	    frames := j.Window(since, 0).Frames
//	    if len(frames) > 0 { … }
//	    ch := j.Wait()          // 先注册
//	    if again := j.Window(since, 0).Frames; len(again) > 0 { continue } // 再查一次
//	    select { case <-ch: case <-ctx.Done(): return }
//	}
//
// 只注册不等待的调用方会积一份 channel——这不会泄漏：追加时统一关闭并清空。
func (j *Journal) Wait() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.waiters == nil {
		j.waiters = map[chan struct{}]struct{}{}
	}
	ch := make(chan struct{})
	j.waiters[ch] = struct{}{}
	return ch
}

func (j *Journal) trimLocked() {
	if len(j.frames) > j.capacity {
		// 丢最老的：补洞只保证最近一段，更早的让客户端重新 load。
		excess := len(j.frames) - j.capacity
		j.frames = append([]ServerFrame(nil), j.frames[excess:]...)
	}
}

// Restore 在启动阶段把持久化的原始帧装进一个空 journal。epoch 仍由新进程
// 分配；seq/type/data/time 全部来自记录，不用 v4 seq 或当前墙钟补造。
func (j *Journal) Restore(frames []ServerFrame) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.seq != 0 || len(j.frames) != 0 {
		return fmt.Errorf("transport: 只能恢复到空 journal（当前 lastSeq=%d）", j.seq)
	}
	restored := make([]ServerFrame, len(frames))
	for i, f := range frames {
		if f.Seq != i+1 {
			return fmt.Errorf("transport: 恢复帧 seq 不连续：第 %d 帧为 %d", i+1, f.Seq)
		}
		if f.Type == "" {
			return fmt.Errorf("transport: 恢复帧 seq=%d 没有 type", f.Seq)
		}
		if f.Time <= 0 {
			return fmt.Errorf("transport: 恢复帧 seq=%d 没有原始 time", f.Seq)
		}
		if len(f.Data) > 0 && !json.Valid(f.Data) {
			return fmt.Errorf("transport: 恢复帧 seq=%d 的 data 不是合法 JSON", f.Seq)
		}
		f.Data = append(json.RawMessage(nil), f.Data...)
		restored[i] = f
	}
	j.seq = len(restored)
	j.frames = restored
	j.trimLocked()
	return nil
}

// Window 是一次补洞窗口的**原子快照**：帧、水位、世代、有没有缺口。
//
// 为什么要有它：handler 里分开调 LastSeq/Covers/Since 会拿到三个时刻的状态。
// 恰好在两次调用之间挤掉一帧时，"检查通过、取帧却少一帧"——又变成看不出缺口的短数组。
// 一次拿全，判据与数据就来自同一个瞬间。
type Window struct {
	Frames  []ServerFrame
	LastSeq int
	Epoch   int64
	// Oldest 是还留在内存里的最早一帧；0 表示空 journal。
	Oldest int
	// Covered 为 false 表示 sinceSeq 之后有一段已经被容量挤掉，补不回来。
	Covered bool
}

// Window 取 seq > sinceSeq 的帧（最多 limit 条）并给出完整水位快照。
//
// **不做投影、不抽 text**：history 要的是原始帧，客户端自己折叠。
// 抽过 text 的 messages 只适合首屏，用它补洞会丢掉 reasoning/tool/approval。
//
// # sinceSeq 的排他/包含语义（固定下来，不许再各处自己解释）
//
//   - `sinceSeq >= 0`：**排他**下界 = "我已经收到第 sinceSeq 帧了"，返回 (sinceSeq, last]。
//   - `sinceSeq == SinceNothing(-1)`：**"我什么都没拿"**，返回 [oldest, last]。
//     这是"从头开始"的唯一写法——写 0 会漏掉第 0 帧（它表示"第 0 帧我有了"）。
//
// Covered 的判据同样按这两条分支算。以前只有 `sinceSeq >= Oldest-1` 一条，
// 于是 -1 一律被判成"补不回来"：明明内存里 1..N 全在，调用方拿到的却是
// "有缺口，重新 load 首屏"——一个**假缺口**，界面表现为莫名其妙地整屏重建。
func (j *Journal) Window(sinceSeq, limit int) Window {
	j.mu.Lock()
	defer j.mu.Unlock()
	if limit <= 0 || limit > j.capacity {
		limit = j.capacity
	}
	// 空集也要是 []，不能是 null：客户端按数组解，"null" 会变成 nil，
	// 而 nil 在多数语言里一 for 就炸或直接跳过——界面表现为"什么都没有"。
	w := Window{
		Frames:  make([]ServerFrame, 0, 8),
		LastSeq: j.seq,
		Epoch:   j.epoch,
		Covered: true,
	}
	if n := len(j.frames); n > 0 {
		w.Oldest = j.frames[0].Seq
		if sinceSeq < 0 {
			// "我什么都没拿"：只有内存里从**第一帧**起都在，才算全覆盖。
			w.Covered = w.Oldest <= 1
		} else {
			// 保留区间是 [oldest, last]，客户端要 (sinceSeq, last]。
			// sinceSeq < oldest-1 ⇒ oldest 之前那段已经被挤掉了，补不回来。
			w.Covered = sinceSeq >= w.Oldest-1
		}
	}
	for _, f := range j.frames {
		if f.Seq > sinceSeq {
			w.Frames = append(w.Frames, f)
			if len(w.Frames) >= limit {
				break
			}
		}
	}
	return w
}

// Since 是 Window 的简化版（只要帧/水位/世代的老调用方用）。
func (j *Journal) Since(sinceSeq, limit int) (frames []ServerFrame, lastSeq int, epoch int64) {
	w := j.Window(sinceSeq, limit)
	return w.Frames, w.LastSeq, w.Epoch
}

// Reset 换一个世代号并清空历史帧，返回新的 epoch。
//
// 什么时候用：宿主内存态丢了、或客户端拿着一个比当前 lastSeq 还大的水位来要补洞——
// 后者说明两边对不上，继续"按 seq 补"只会补出错位的数据。
func (j *Journal) Reset(newEpoch int64) int64 {
	epoch, _ := j.reset(newEpoch, nil)
	return epoch
}

func (j *Journal) reset(newEpoch int64, persist func() error) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if persist != nil {
		if err := persist(); err != nil {
			return j.epoch, err
		}
	}
	j.epoch = newEpoch
	j.seq = 0
	j.frames = nil
	return j.epoch, nil
}

// JournalPersistence 由宿主实现；transport 不认识文件系统。记录在 journal
// 锁内同步提交，保证同会话的 seq 分配、磁盘顺序与 history/WS 顺序一致。
type JournalPersistence interface {
	AppendJournalFrame(sessionID string, frame ServerFrame) error
	ResetJournal(sessionID string) error
}

// Journals 是按会话 id 索引的 journal 表。
type Journals struct {
	mu sync.Mutex
	m  map[string]*Journal
	// epochOf 生成新 journal 的世代号；测试里可以换掉，生产用单调时钟。
	epochOf     func() int64
	persistence JournalPersistence
}

// NewJournals 造一张空表。epochOf 为 nil 时用 LastSeq 式的推导（调用方注入更有意义）。
func NewJournals(epochOf func() int64) *Journals {
	if epochOf == nil {
		var n int64
		epochOf = func() int64 { n++; return n }
	}
	return &Journals{m: map[string]*Journal{}, epochOf: epochOf}
}

// Get 取（必要时创建）某个会话的 journal。
func (js *Journals) Get(sessionID string) *Journal {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.getLocked(sessionID)
}

func (js *Journals) getLocked(sessionID string) *Journal {
	if j, ok := js.m[sessionID]; ok {
		return j
	}
	j := NewJournal(js.epochOf(), 0)
	js.m[sessionID] = j
	return j
}

// SetPersistence 在启动恢复完成后接上写路径。调用必须发生在服务并发追加前。
func (js *Journals) SetPersistence(p JournalPersistence) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.persistence = p
}

// Restore 恢复一个会话的原始帧；不写持久化文件，也不沿用旧 epoch。
func (js *Journals) Restore(sessionID string, frames []ServerFrame) error {
	return js.Get(sessionID).Restore(frames)
}

// Append 是生产写入口：磁盘提交、内存发号在同一会话锁内依次完成。
func (js *Journals) Append(sessionID, typ string, data json.RawMessage, t int64) (ServerFrame, error) {
	js.mu.Lock()
	j := js.getLocked(sessionID)
	p := js.persistence
	js.mu.Unlock()
	if p == nil {
		return j.append(typ, data, t, nil)
	}
	return j.append(typ, data, t, func(f ServerFrame) error {
		return p.AppendJournalFrame(sessionID, f)
	})
}

// Reset 给某个会话换世代（宿主侧主动重建基线时用）。持久化 reset 标记
// 先落盘，旧帧才不会在下个进程复活。旧签名不变；写失败时保持原 epoch/水位。
func (js *Journals) Reset(sessionID string) int64 {
	js.mu.Lock()
	j := js.getLocked(sessionID)
	p := js.persistence
	newEpoch := js.epochOf()
	js.mu.Unlock()
	if p == nil {
		return j.Reset(newEpoch)
	}
	epoch, _ := j.reset(newEpoch, func() error { return p.ResetJournal(sessionID) })
	return epoch
}
