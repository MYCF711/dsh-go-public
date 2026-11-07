package main

// `--script`：把一串"人本来会敲的键"回放给**真宿主**，并把每一轮的帧与屏幕快照
// 一起交出来。
//
// 为什么需要它：单测能验 view() 的形状，`--send` 能验单轮链路，但
// "多轮 + 工具 + 审批交错、连着跑下来屏幕还对不对"不是任何**一层**的性质，
// 是**连跑**的性质：
//
//   - 第二轮要带着第一轮的上下文（进程模型下一轮一个子进程，断了就是断了）；
//   - 审批那一下答完，工具才真的执行，结果帧才出现；
//   - 工具结果进了 lines（也打了折叠标记）之后，折叠/展开仍然是对的。
//
// 这三件事各自都能在单测里"看起来对"，而它们串起来的错只会在连跑时露出来。
//
// 它不改默认行为：只有显式给 --script 才走这条路，界面与 --send 的路径没动。
// 键走的也是界面那条路——`handleRune` → `submit` / `answerApproval`——
// 所以"按 y 批准"在这里和在终端里调用的是同一个函数，而不是另写一条捷径。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// scriptPlan 是一份回放脚本。
//
//	{
//	  "size": [100, 40],
//	  "turnTimeoutMs": 300000,
//	  "steps": [
//	    {"kind": "send", "text": "…", "label": "t1", "approve": "allow_once"},
//	    {"kind": "key", "key": "ctrl+t", "label": "t2-expanded"},
//	    {"kind": "snapshot", "label": "final"}
//	  ]
//	}
type scriptPlan struct {
	// Size 是屏幕的 [列, 行]。**不从终端取**：脚本要的是可比的屏幕，
	// 24 行和 40 行会裁掉不同内容，断言就跟着机器变。
	Size []int `json:"size"`
	// TurnTimeoutMs 是单轮上限。模型有时慢，但慢不是没有上限的理由——
	// 没有上限的探针挂住时，人看到的是"卡住了"，而失败信息什么都没有。
	TurnTimeoutMs int          `json:"turnTimeoutMs"`
	Steps         []scriptStep `json:"steps"`
}

// scriptStep 是一步。三种 kind：
//
//	send     发一句话并等这一轮结束（approve 非空时，收到审批就用对应的键应答）
//	key      敲一个特殊键（ctrl+t / ctrl+f / esc / pgup / pgdn / enter）
//	snapshot 只拍一张屏（用来验"这一刻屏幕上是什么"）
type scriptStep struct {
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
	Key     string `json:"key,omitempty"`
	Label   string `json:"label,omitempty"`
	Approve string `json:"approve,omitempty"`
	// Cancel 只对 send 有效：审批出现时**不按键**，改发 `session.cancel`。
	//
	// 为什么需要它：宿主取消挂起审批这条链（H13 尾巴）只有在"审批还在屏幕上"
	// 的那一刻才看得见效果。和 approve 互斥——一次审批只能有一种结算。
	Cancel bool `json:"cancel,omitempty"`
}

// scriptReport 是交给断言层（scripts/tui-session-e2e.py）的全部事实。
//
// 只放**事实**，不放结论：哪一帧、什么类型、屏幕长什么样。
// 断言写在脚本里，这里不替它判断对错——否则"驱动器写绿了"就成了通过。
type scriptReport struct {
	SessionID string         `json:"sessionId"`
	Width     int            `json:"width"`
	Height    int            `json:"height"`
	Turns     []turnRecord   `json:"turns"`
	Frames    []frameRecord  `json:"frames"`
	Screens   []screenRecord `json:"screens"`
	Keys      []keyRecord    `json:"keys"`
	// Cancels 记的是"哪一轮在审批挂着时发了 session.cancel"。
	// 不记这条，屏幕快照里那句"审批已取消"就分不清是驱动器按的取消键、
	// 还是宿主自己超时——两者都会让气泡消失，但事实完全不同。
	Cancels []cancelRecord `json:"cancels"`
	// Error 非空 = 驱动器自己半路停了（断流/超时）。这时**下面的事实仍然有效**，
	// 断言层可以拿它说清"停在哪一轮"，而不是只报一个"驱动失败"。
	Error string `json:"error,omitempty"`
}

type frameRecord struct {
	Turn int             `json:"turn"`
	Seq  int             `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type turnRecord struct {
	Label     string `json:"label"`
	Index     int    `json:"index"`
	Status    string `json:"status"` // done | error | timeout | closed
	Frames    int    `json:"frames"`
	ElapsedMs int64  `json:"elapsedMs"`
}

type screenRecord struct {
	Label string   `json:"label"`
	At    string   `json:"at"` // after-turn | approval | answered | canceled | key | timeout | manual
	Turn  int      `json:"turn"`
	Lines []string `json:"lines"`
}

type keyRecord struct {
	Label string `json:"label"`
	Key   string `json:"key"`
	Turn  int    `json:"turn"`
	When  string `json:"when"` // approval | step
}

// cancelRecord 是一次"人在审批挂着时取消了这一轮"的记录。
// Err 非空表示那次 session.cancel 本身失败了（宿主要么已经收工，要么不认这条会话）。
type cancelRecord struct {
	Label string `json:"label"`
	Turn  int    `json:"turn"`
	Err   string `json:"err,omitempty"`
}

// approvalKeys 是"审批这一问该敲哪个键"，与 handleRune 里那三行**同义**：
// y = allow_once、a = allow_always、n = reject_once。
//
// 写成表而不是 switch，是为了让"选项 → 键"只有一处定义：
// 两处各写一遍的话，迟早有一处把 a 和 n 弄反，而那正是"点了允许却被拒"的形状。
var approvalKeys = map[string]rune{
	"allow_once":   'y',
	"allow_always": 'a',
	"reject_once":  'n',
}

// specialKeys 是 key 步支持的名字（和 handleSpecial/readKeys 认的名字一致）。
var specialKeys = map[string]string{
	"esc":  "esc",
	"pgup": "pgup",
	"pgdn": "pgdn",
	"up":   "up",
	"down": "down",
	"home": "home",
	"end":  "end",
}

// keyRunes 是"按一下就是某个控制字符"的键（走 handleRune）。
//
// 和 specialKeys 分开：那半边走 handleSpecial（方向/翻页），
// 这半边走 handleRune（Ctrl+T/Ctrl+F/回车）。混在一起会让
// "Ctrl+T 到底调没调 toggleToolResults"变成猜的。
var keyRunes = map[string]rune{
	"ctrl+o": 15,
	"ctrl+t": 20,
	"ctrl+f": 6,
	"enter":  13,
}

func knownKey(name string) bool {
	if _, ok := keyRunes[name]; ok {
		return true
	}
	_, ok := specialKeys[name]
	return ok
}

func readPlan(path string) (*scriptPlan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读脚本失败: %w", err)
	}
	var p scriptPlan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("脚本不是合法 JSON: %w", err)
	}
	if len(p.Steps) == 0 {
		return nil, fmt.Errorf("脚本没有 steps")
	}
	for i, st := range p.Steps {
		switch st.Kind {
		case "send":
			if strings.TrimSpace(st.Text) == "" {
				return nil, fmt.Errorf("第 %d 步 send 没有 text", i+1)
			}
			// 正文里的换行会变成回车 = 半句话就被发出去。
			// 这不是"容错一下"的事：发出去的是**半句**，而屏幕上看着像完整的一句。
			if strings.ContainsAny(st.Text, "\r\n") {
				return nil, fmt.Errorf("第 %d 步 send 的 text 含换行（会被当成回车提前发送）", i+1)
			}
			if st.Approve != "" {
				if _, ok := approvalKeys[st.Approve]; !ok {
					return nil, fmt.Errorf("第 %d 步 approve=%q 不认识（可选 allow_once/allow_always/reject_once）",
						i+1, st.Approve)
				}
			}
			if st.Cancel && st.Approve != "" {
				// 一次审批只有一种结算：又按键又取消，驱动器自己就说不清屏幕该显示哪种。
				return nil, fmt.Errorf("第 %d 步 approve 与 cancel 只能选一个（一次审批只有一种结算）", i+1)
			}
		case "key":
			if !knownKey(st.Key) {
				return nil, fmt.Errorf("第 %d 步 key=%q 不认识（可选 %s）", i+1, st.Key,
					strings.Join(keyNames(), "/"))
			}
		case "snapshot":
		default:
			return nil, fmt.Errorf("第 %d 步 kind=%q 不认识（send/key/snapshot）", i+1, st.Kind)
		}
		if st.Cancel && st.Kind != "send" {
			// 只有 send 会等审批出现；key/snapshot 步写 cancel 是脚本作者以为它会发，
			// 实际什么都不做——这种"写了不跑"的脚本比报错更浪费时间。
			return nil, fmt.Errorf("第 %d 步 cancel 只对 send 有效（kind=%q）", i+1, st.Kind)
		}
	}
	return &p, nil
}

func keyNames() []string {
	out := make([]string, 0, len(specialKeys)+len(keyRunes))
	for k := range keyRunes {
		out = append(out, k)
	}
	for k := range specialKeys {
		out = append(out, k)
	}
	return out
}

// runScript 回放整份脚本。返回非 nil 表示**驱动**没跑完；报告仍然会写出去。
func runScript(addr, token, planPath, outPath string) error {
	plan, err := readPlan(planPath)
	if err != nil {
		return err
	}
	rep := &scriptReport{}
	// 失败时最需要的是现场：所以报告在 return 之前一定落盘，不靠"成功才写"。
	defer func() {
		if outPath != "" {
			if raw, mErr := json.MarshalIndent(rep, "", "  "); mErr == nil {
				if wErr := os.WriteFile(outPath, append(raw, '\n'), 0o644); wErr != nil {
					fmt.Fprintln(os.Stderr, "dshtui: 写报告失败:", wErr)
				}
			}
		}
	}()

	a, err := newApp(addr, token)
	if err != nil {
		rep.Error = err.Error()
		return err
	}
	w, h := 100, 40
	if len(plan.Size) == 2 && plan.Size[0] > 0 && plan.Size[1] > 0 {
		w, h = plan.Size[0], plan.Size[1]
	}
	// 输出接到 io.Discard：脚本模式不画屏，屏幕内容靠快照交出去。
	a.scr = &Screen{out: io.Discard, width: w, height: h}
	rep.Width, rep.Height = w, h

	if err := a.newSession(); err != nil {
		rep.Error = err.Error()
		return err
	}
	rep.SessionID = a.sid

	timeout := time.Duration(plan.TurnTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	frames := a.frames
	events := a.connectionEvents
	turn := 0
	for _, st := range plan.Steps {
		switch st.Kind {
		case "send":
			turn++
			start := time.Now()
			startFrames := len(rep.Frames)
			// 打字 + 回车：和人做的是同一件事。
			for _, r := range st.Text {
				a.handleRune(r)
			}
			a.handleRune(13)

			rec := turnRecord{Label: st.Label, Index: turn}
			timer := time.NewTimer(timeout)
			done := false
			// cancelSent 保证一轮只取消一次：approval/request 可能因重放再来一遍，
			// 每来一条就发一次 session.cancel，第二次只会拿到"没有可取消的轮次"。
			cancelSent := false
			// processFrame 只处理已经在 app 中落定的帧。HTTP accepted 前被暂存的帧
			// 会在 promptResults 分支释放后走同一个入口，审批应答与终局判断不重写。
			processFrame := func(f ServerFrame) {
				if f.Type == "approval/request" {
					// 先拍"问题在屏幕上"的那一帧，再按 y。
					rep.Screens = append(rep.Screens,
						snapshotOf(a, st.Label+"-approval", "approval", turn))
					if st.Approve != "" {
						r := approvalKeys[st.Approve]
						a.handleRune(r)
						rep.Keys = append(rep.Keys, keyRecord{
							Label: st.Label, Key: string(r), Turn: turn, When: "approval",
						})
						rep.Screens = append(rep.Screens,
							snapshotOf(a, st.Label+"-answered", "answered", turn))
					} else if st.Cancel && !cancelSent {
						// 不按键，直接取消这一轮：这是"审批还挂在屏幕上、
						// 宿主那边已经把它收了"的那条路径。屏幕快照要等
						// approval/cancel 帧到了再拍——那一刻才是真的结算完。
						cancelSent = true
						_, cErr := a.cl.post("session.cancel", map[string]any{"id": a.sid})
						crec := cancelRecord{Label: st.Label, Turn: turn}
						if cErr != nil {
							crec.Err = cErr.Error()
						}
						rep.Cancels = append(rep.Cancels, crec)
					}
				}
				if f.Type == "approval/cancel" && cancelSent {
					rep.Screens = append(rep.Screens,
						snapshotOf(a, st.Label+"-canceled", "canceled", turn))
				}
				// **只认 turn/done / turn/error**：turn/end 是"这一步结束"，
				// 在 turn/done 之前就到。停在 turn/end 会漏掉真正的终局。
				if f.Type == "turn/done" || f.Type == "turn/error" {
					if a.activeTurnID != "" && frameTurnID(f.Data) != "" && frameTurnID(f.Data) != a.activeTurnID {
						return // app 已忽略的旧终局，驱动器也不能拿它收轮
					}
					rec.Status = "done"
					if f.Type == "turn/error" {
						rec.Status = "error"
					}
					done = true
				}
			}
			for !done {
				select {
				case f, ok := <-frames:
					if !ok {
						// 流断了：重连归 client，这里如实停下——不能假装还在跑。
						timer.Stop()
						rec.Status = "closed"
						rec.Frames = len(rep.Frames) - startFrames
						rec.ElapsedMs = time.Since(start).Milliseconds()
						rep.Turns = append(rep.Turns, rec)
						rep.Error = fmt.Sprintf("第 %d 轮流断了（订阅没了）", turn)
						return fmt.Errorf("%s", rep.Error)
					}
					buffered := a.pendingSend != nil
					a.handleFrame(f)
					rep.Frames = append(rep.Frames, frameRecord{
						Turn: turn, Seq: f.Seq, Type: f.Type, Data: f.Data,
					})
					if !buffered {
						processFrame(f)
					}
				case ev, ok := <-events:
					if !ok {
						events = nil
						continue
					}
					a.handleConnectionEvent(ev)
				case result := <-a.promptResults:
					if a.pendingSend == nil || result.id != a.pendingSend.id {
						continue // 旧提交的迟到结果不属于这一轮
					}
					buffered := a.handlePromptResult(result)
					if result.err != nil || !result.ack.Accepted {
						timer.Stop()
						rec.Status = "error"
						rec.Frames = len(rep.Frames) - startFrames
						rec.ElapsedMs = time.Since(start).Milliseconds()
						rep.Turns = append(rep.Turns, rec)
						rep.Error = a.err
						rep.Screens = append(rep.Screens, snapshotOf(a, st.Label+"-send-error", "error", turn))
						return fmt.Errorf("%s", rep.Error)
					}
					for _, f := range buffered {
						processFrame(f)
						if done {
							break
						}
					}
				case <-timer.C:
					rec.Status = "timeout"
					rec.Frames = len(rep.Frames) - startFrames
					rec.ElapsedMs = time.Since(start).Milliseconds()
					rep.Turns = append(rep.Turns, rec)
					rep.Screens = append(rep.Screens,
						snapshotOf(a, st.Label+"-timeout", "timeout", turn))
					rep.Error = fmt.Sprintf("第 %d 轮 %v 内没等到 turn/done", turn, timeout)
					return fmt.Errorf("%s", rep.Error)
				}
			}
			timer.Stop()
			rec.Frames = len(rep.Frames) - startFrames
			rec.ElapsedMs = time.Since(start).Milliseconds()
			rep.Turns = append(rep.Turns, rec)
			// 这一轮的屏幕（此刻这一轮的内容正在屏幕底部，还没被下一轮顶掉）。
			rep.Screens = append(rep.Screens, snapshotOf(a, st.Label, "after-turn", turn))

		case "key":
			if r, ok := keyRunes[st.Key]; ok {
				a.handleRune(r)
			} else {
				a.handleSpecial(st.Key)
			}
			rep.Keys = append(rep.Keys, keyRecord{Label: st.Label, Key: st.Key, Turn: turn, When: "step"})
			rep.Screens = append(rep.Screens, snapshotOf(a, st.Label, "key", turn))

		case "snapshot":
			rep.Screens = append(rep.Screens, snapshotOf(a, st.Label, "manual", turn))
		}
	}
	return nil
}

// snapshotOf 拍一张"现在屏幕上是什么"。
//
// 行尾空格裁掉（fitWidth 会补到终端宽度）：断言比较的是内容，
// 而一排尾随空格在 JSON 里既占地方又会让 diff 看不出重点。
func snapshotOf(a *app, label, at string, turn int) screenRecord {
	lines := a.view()
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = strings.TrimRight(ln, " ")
	}
	return screenRecord{Label: label, At: at, Turn: turn, Lines: out}
}
