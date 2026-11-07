package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Conn 是一条连接的读写面。WS 的真实实现以后补，测试用内存管道。
//
// 读那一面：ServeConn 是唯一的调用者，所以实现不必为并发读做准备。
// 写那一面：Hub 的推送路径和本文件的 pong 都会写，**串行由 ServeConn 的写锁负责**，
// 实现本身不必并发安全（真去写 socket 的实现最好自己再兜一层）。
type Conn interface {
	// ReadClientFrame 阻塞等下一帧；对端关闭时返回 io.EOF（或包着它的错误）。
	ReadClientFrame() (ClientFrame, error)
	// WriteServerFrame 写一帧；写失败表示连接已坏。
	WriteServerFrame(ServerFrame) error
}

// connSubscriber 把一条连接适配成 Hub 的 Subscriber。
//
// 写锁不是可选项：Hub.Publish 在锁外推送（见 hub.go 的锁纪律），
// 所以多个 Publish 可以并发进同一个 Push；而"写一帧"对一条连接来说不可交错——
// 两帧的字节交叠在客户端那边就是一段坏 JSON，且**看起来像网络故障**，极难查。
type connSubscriber struct {
	conn Conn

	// mu 只串行化 socket 写字节；eventMu 串行化事件顺序与 replay→live 交接。
	mu      sync.Mutex
	eventMu sync.Mutex

	// replaying 期间 Push 只进缓冲区，不碰 socket。这样即使 Conn 的实现
	// 在 WriteServerFrame 里同步触发 Append+Publish，也不会自锁或插队。
	stateMu      sync.Mutex
	replaying    bool
	replayBuffer []ServerFrame
	deliveredSeq int
	journal      *Journal
	epoch        int64
}

// 钉住契约：连接订阅者就是 Hub 要的那种订阅者。
var _ Subscriber = (*connSubscriber)(nil)

// Push 把一帧写给客户端。返回 error 表示连接已坏，Hub 会把它从所有 topic 上摘掉。
func (s *connSubscriber) Push(frame ServerFrame) error {
	// 快路径不拿 eventMu：重放期间只缓冲，避免 Conn 写回调里的同步 Publish 自锁。
	s.stateMu.Lock()
	if s.replaying {
		s.replayBuffer = append(s.replayBuffer, frame)
		s.stateMu.Unlock()
		return nil
	}
	s.stateMu.Unlock()

	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	// 等 eventMu 时可能恰好开始了一次重复 sub；再次检查，仍只缓冲。
	s.stateMu.Lock()
	if s.replaying {
		s.replayBuffer = append(s.replayBuffer, frame)
		s.stateMu.Unlock()
		return nil
	}
	if s.journal != nil {
		s.stateMu.Unlock()
		// Publish 只负责提醒“journal 可能有新帧”。并发 emit 的 Publish 顺序
		// 不保证与 Append 一致，必须读 journal 补到最新，不能按到达先后发号。
		return s.catchUpLocked()
	}
	s.stateMu.Unlock()
	// 无 journal 是老调用方：保持 Hub 原样透传，不发明去重或重放语义。
	return s.write(frame)
}

// beginReplay 打开交接闸。重复 sub 不许把本连接已经交付的水位倒退；
// rebuild 是例外：reset 明确宣告旧水位作废，改用当前 journal 的基线。
func (s *connSubscriber) beginReplay(journal *Journal, epoch int64, sinceSeq int, rebuild bool) {
	s.eventMu.Lock()
	s.stateMu.Lock()
	if rebuild || sinceSeq > s.deliveredSeq {
		s.deliveredSeq = sinceSeq
	}
	s.journal = journal
	s.epoch = epoch
	s.replaying = true
	s.replayBuffer = nil
	s.stateMu.Unlock()
	s.eventMu.Unlock()
}

// finishReplay 在订阅已经挂上之后交付重放帧，再把 journal 补到最新。
// 缓冲只记录“有 Publish 来过”；实际帧永远从 journal 取，因此缓冲里只有
// seq=N+1、seq=N 的 Publish 迟到时，也不会漏掉 N。
func (s *connSubscriber) finishReplay(frames []ServerFrame) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	for _, f := range frames {
		if err := s.writeEventLocked(f); err != nil {
			return err
		}
	}
	for {
		if err := s.catchUpLocked(); err != nil {
			return err
		}
		s.stateMu.Lock()
		win := s.journal.Window(s.deliveredSeq, 0)
		if !win.Covered || win.Epoch != s.epoch {
			s.stateMu.Unlock()
			return fmt.Errorf("transport: journal 在重放收尾时换代或滑窗")
		}
		if len(s.replayBuffer) == 0 && len(win.Frames) == 0 {
			// “无新 Publish + journal 已补齐 + 关闭重放”在同一把锁里完成。
			// 之后的 Push 会走实时 catchUp，不存在两侧都不送的空窗。
			s.replaying = false
			s.stateMu.Unlock()
			return nil
		}
		s.replayBuffer = nil
		s.stateMu.Unlock()
	}
}

// catchUpLocked 从当前交付水位读同一 journal，直到暂时没有新帧。
// 调用方持有 eventMu，所以并发 Push 不会把两份 catch-up 交错写到 socket。
func (s *connSubscriber) catchUpLocked() error {
	for {
		s.stateMu.Lock()
		journal, epoch, since := s.journal, s.epoch, s.deliveredSeq
		s.stateMu.Unlock()
		if journal == nil {
			return nil
		}
		win := journal.Window(since, 0)
		if !win.Covered || win.Epoch != epoch {
			return fmt.Errorf("transport: journal 在实时补洞时换代或滑窗")
		}
		if len(win.Frames) == 0 {
			return nil
		}
		for _, f := range win.Frames {
			if err := s.writeEventLocked(f); err != nil {
				return err
			}
		}
	}
}

// writeEventLocked 的调用方持有 eventMu；seq<=0 的控制状态不参与水位。
func (s *connSubscriber) writeEventLocked(frame ServerFrame) error {
	s.stateMu.Lock()
	if frame.Seq > 0 && frame.Seq <= s.deliveredSeq {
		s.stateMu.Unlock()
		return nil
	}
	s.stateMu.Unlock()
	if err := s.write(frame); err != nil {
		return err
	}
	if frame.Seq > 0 {
		s.stateMu.Lock()
		s.deliveredSeq = frame.Seq
		s.stateMu.Unlock()
	}
	return nil
}

// write 是这条连接**唯一**的字节写入口：Hub 推送、控制帧与 pong 都走它。
// envelope time 在这里兜底，原始 seq/data 不动；已有时间（尤其 journal 重放）原样保留。
func (s *connSubscriber) write(frame ServerFrame) error {
	if frame.Time <= 0 {
		frame.Time = time.Now().UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteServerFrame(frame)
}

// ServeConn 跑完一条连接，直到对端关闭或帧坏掉。
//
// 规矩（都是踩过的）：
//   - 第一帧必须是合法客户端帧；坏帧 → 立刻返回错误，别当 ping 放过
//   - sub：往 hub 订 topic；同一连接可以对同一 topic 重复 sub（以最后一次 sinceSeq 为准）
//   - unsub：退订；**退订后要真的收不到**
//   - ping：回一条 pong
//   - 连接结束（任何原因）→ 必须 hub.UnsubscribeAll(this)，否则 Hub 会一直往死连接里写
//
// 返回 nil 表示对端正常关闭。
func ServeConn(conn Conn, hub *Hub) error {
	if conn == nil {
		// 没有连接就没什么可读的；返回错误而不是在这里 nil 解引用。
		return errors.New("transport: ServeConn 收到 nil 连接")
	}
	if hub == nil {
		// 没给总线：订阅照收，但一条帧也发不出去（这条连接本身仍然能 ping/pong）。
		// 不 panic、也不假装"订上了会有人推"——Hub 那边的表只有这一条死记录，随连接结束消失。
		hub = NewHub()
	}

	sub := &connSubscriber{conn: conn}

	// 无论怎么退出（对端关闭、坏帧、写失败），都要把自己从 Hub 上摘干净。
	// 早退晚退都走 defer：留一条死连接在表里，之后每次 Publish 都要往它身上白费一次写，
	// 而且连接对象（连带它的缓冲）永远收不掉。
	defer hub.UnsubscribeAll(sub)

	// 心跳：只在连接实现了 Pinger 时启动（内存管道不需要）。
	// 读侧长时间没有帧 → 关掉连接把读解开，并返回 ErrConnIdle 而不是"use of closed connection"。
	var idle int32
	stop := heartbeat(conn, pingEvery, idleLimit, func() {
		atomic.StoreInt32(&idle, 1)
		if c, ok := conn.(io.Closer); ok {
			_ = c.Close()
		}
	})
	defer stop()

	first := true
	for {
		frame, err := conn.ReadClientFrame()
		if err != nil {
			if atomic.LoadInt32(&idle) == 1 {
				return ErrConnIdle
			}
			// 对端正常关闭：这条连接的活干完了，不算错误。
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		// 先验再动状态：坏帧必须在**任何**订阅变化之前被拦下，
		// 否则会出现"订阅改了、连接却已经该关了"的半截状态。
		if err := checkClientFrame(frame, first); err != nil {
			return err
		}
		first = false

		switch frame.T {
		case FrameSub:
			sessionID := topicSession(frame.Topic)
			if hub.journals == nil {
				// 老调用方没有 journal：保留只接新帧的行为，不伪造可重放历史。
				if err := replayPending(hub, sessionID, nil, sub); err != nil {
					return err
				}
				hub.Subscribe(frame.Topic, frame.SinceSeq, sub)
				break
			}

			journal := hub.journals.Get(sessionID)
			win := journal.Window(frame.SinceSeq, 0)
			rebuild := (frame.JournalEpoch != 0 && win.Epoch != frame.JournalEpoch) || !win.Covered
			replayBase := frame.SinceSeq
			if rebuild {
				replayBase = win.LastSeq
			}

			// 先开闸、发 reset，再挂订阅。此后所有实时帧只会进交接缓冲，
			// 不会插到 reset 或历史帧前面。
			sub.beginReplay(journal, win.Epoch, replayBase, rebuild)
			if rebuild {
				reset, _ := json.Marshal(map[string]any{
					"journalEpoch": win.Epoch,
					"lastSeq":      win.LastSeq,
				})
				if err := sub.write(ServerFrame{Type: "session/reset", Data: reset}); err != nil {
					return err
				}
			}
			hub.Subscribe(frame.Topic, replayBase, sub)

			// 订阅后再取一次快照：第一次 Window→Subscribe 之间追加的帧也因此
			// 进入重放集；第二次 Window 之后追加的帧已在 Hub 缓冲里，两侧无缝。
			latest := journal.Window(replayBase, 0)
			if !latest.Covered || latest.Epoch != win.Epoch {
				// 交接期间又换代/滑窗，不能拿错位数据充数。关连接让客户端
				// 重新 load/sub，比静默丢一段更可诊断。
				return fmt.Errorf("transport: journal 在订阅交接期间换代")
			}

			// pending 是无 seq 的状态帧；若同一 requestId 已在本次 journal
			// 重放集里，就不再额外制造第二份审批。
			if err := replayPending(hub, sessionID, latest.Frames, sub); err != nil {
				return err
			}
			if err := sub.finishReplay(latest.Frames); err != nil {
				return err
			}

		case FrameUnsub:
			hub.Unsubscribe(frame.Topic, sub)

		case FramePing:
			// pong 直接构造结构体、不走 NewServerFrame：后者的第一道检查是 seq <= 0 → ErrBadSeq，
			// 理由是 seq 是客户端判连续性的唯一依据，不许造 0。
			// 但 pong 根本不是事件流里的一帧（没有 data、不进历史、不参与重放），
			// "没有序号"正是它该有的样子——给它编一个假序号反而会污染客户端的连续性判断。
			if err := sub.write(ServerFrame{Type: "pong"}); err != nil {
				// 写不出去＝连接已坏，没必要再等下一次读。
				return err
			}

		default:
			// checkClientFrame 已经拦过；留这一支只是为了让 switch 穷尽，
			// 免得以后加了帧类型忘了在这里处理，静默地什么也不做。
			return fmt.Errorf("%w: t=%q", ErrUnknownFrameType, frame.T)
		}
	}
}

// replayPending 重放仍挂起的审批。journal 已包含同一请求时只交付 journal 那一份；
// 不在本次重放区间的 pending 仍按原规则以 seq=0 状态帧恢复。
func replayPending(hub *Hub, sessionID string, replayed []ServerFrame, sub *connSubscriber) error {
	if hub.pendingReplay == nil {
		return nil
	}
	for _, pf := range hub.pendingReplay(sessionID) {
		duplicate := false
		for _, jf := range replayed {
			if sameApprovalRequest(pf, jf) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if err := sub.write(pf); err != nil {
			return err
		}
	}
	return nil
}

func sameApprovalRequest(a, b ServerFrame) bool {
	if a.Type != "approval/request" || b.Type != "approval/request" {
		return false
	}
	var ad, bd struct {
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(a.Data, &ad) == nil && json.Unmarshal(b.Data, &bd) == nil && ad.RequestID != "" {
		return ad.RequestID == bd.RequestID
	}
	return string(a.Data) == string(b.Data)
}

// topicSession 从 "session:<id>" 里取会话 id；不是这个形状时原样返回，
// 反正 Get 只是拿一个 key（拿不到就是新建域，不影响正确性）。
func topicSession(topic string) string {
	const p = "session:"
	if len(topic) > len(p) && topic[:len(p)] == p {
		return topic[len(p):]
	}
	return topic
}

// checkClientFrame 拦下不该进状态机的帧。
//
// 正常路径上 ParseClientFrame 已经在字节层验过一遍了；但 Conn 的实现者未必走它
// （内存管道测试就会直接搓 ClientFrame），所以这里再验一次形状——
// 代价是一次 switch，而漏掉的代价是：一个 t 不认识的帧被当成 ping 放过之后，
// 客户端以为自己订阅成功了，然后一直等不到事件，**且两边都没有任何报错**。
func checkClientFrame(frame ClientFrame, first bool) error {
	// 第一帧单独提一句：它出错时客户端连"这条连接要怎么用"都还没说清，
	// 把这一点写进错误里，比笼统的"帧不合法"好查。
	where := "帧"
	if first {
		where = "第一帧"
	}

	switch frame.T {
	case FrameSub:
		if err := requireTopic(frame.Topic, frame.T); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		// 与 ParseClientFrame 同一条规矩：只支持 "session:<非空 id>"。
		if err := requireSessionTopic(frame.Topic); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if frame.SinceSeq < SinceNothing {
			return fmt.Errorf("%w: %s的 sinceSeq=%d 小于 %d；要「我什么都没拿」就省略该字段或写 %d",
				ErrBadSeq, where, frame.SinceSeq, SinceNothing, SinceNothing)
		}
		return nil

	case FrameUnsub:
		// unsub 只要求带 topic（不查前缀）：取消不掉比多挂一条更糟。
		if err := requireTopic(frame.Topic, frame.T); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		return nil

	case FramePing:
		// ping 不改变任何状态，topic / sinceSeq 带了也忽略。
		return nil

	case "":
		return fmt.Errorf("%w: %s缺 t 字段；可选 %s / %s / %s",
			ErrUnknownFrameType, where, FrameSub, FrameUnsub, FramePing)

	default:
		return fmt.Errorf("%w: %s的 t=%q；可选 %s / %s / %s",
			ErrUnknownFrameType, where, frame.T, FrameSub, FrameUnsub, FramePing)
	}
}
