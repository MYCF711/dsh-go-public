package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"dshgo/internal/transport"
)

// 这一层是「发号 + 推送」的唯一入口（H14）。
//
// 以前 seq 有三个来源：事件帧用 v4 `ev.Seq`、结局帧用全局 `serveTurnSeq`、
// 客户端按收到的顺序自己数。三者混用之后，客户端"发现断号"的判断是假的。
//
// 现在统一成：**每会话一条 Transport journal**，任何要推给界面的帧都先过
// `emit`，由 journal 发号并记账；WS 推送与 `session.history` 读的是同一份。

// emitFrame 是三个 runner 共用的发号+推送入口，保留旧调用方的签名。
// 持久化失败必须留在宿主 stderr；返回零帧且不发布，不能假装已入账。
func emitFrame(journals *transport.Journals, pub func(topic string, f transport.ServerFrame),
	sessionID, typ string, data json.RawMessage) transport.ServerFrame {
	f, err := emitFrameResult(journals, pub, sessionID, typ, data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Transport journal 追加失败（session=%s type=%s）：%v\n", sessionID, typ, err)
	}
	return f
}

// emitFrameResult 把写盘错误显式交给需要处理它的调用方。
// journals 为 nil 时退化成"只推不记账"（老测试不关心序号也不用改）。
func emitFrameResult(journals *transport.Journals, pub func(topic string, f transport.ServerFrame),
	sessionID, typ string, data json.RawMessage) (transport.ServerFrame, error) {
	return emitFrameAtResult(journals, pub, sessionID, typ, data, time.Now().UnixMilli())
}

// emitFrameAt 给已有真实时间的事件发号。v4 follow/drain 必须走这里，
// 不能把 200ms 轮询到事件的墙钟冒充 step/start 或首个 token 的发生时间。
func emitFrameAt(journals *transport.Journals, pub func(topic string, f transport.ServerFrame),
	sessionID, typ string, data json.RawMessage, t int64) transport.ServerFrame {
	f, err := emitFrameAtResult(journals, pub, sessionID, typ, data, t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Transport journal 追加失败（session=%s type=%s）：%v\n", sessionID, typ, err)
	}
	return f
}

func emitFrameAtResult(journals *transport.Journals, pub func(topic string, f transport.ServerFrame),
	sessionID, typ string, data json.RawMessage, t int64) (transport.ServerFrame, error) {
	if t <= 0 {
		return transport.ServerFrame{}, fmt.Errorf("事件 %s 缺真实时间戳", typ)
	}
	f := transport.ServerFrame{Type: typ, Data: data, Time: t}
	if journals != nil {
		// 发号、原始帧持久化与入账在同一把会话锁里完成；写盘失败不占 seq，
		// 也不向 WS 发布一个重启后会消失的假成功帧。
		var err error
		f, err = journals.Append(sessionID, typ, data, t)
		if err != nil {
			return transport.ServerFrame{}, err
		}
	}
	if pub != nil {
		pub("session:"+sessionID, f)
	}
	return f, nil
}

// emitTurnFrame 推一轮的结局（turn/done 或 turn/error）。
//
// 它现在也进 journal：否则客户端补洞时拿到的事件流里没有"这一轮结束了"，
// 会一直停在"正在思考"——这正是在 H2 那轮踩过的坑。
func emitTurnFrame(journals *transport.Journals, pub func(topic string, f transport.ServerFrame),
	sessionID, turnID string, err error) {
	typ := TurnDone
	var payload any = turnDoneData{TurnID: turnID}
	if err != nil {
		typ = TurnError
		payload = turnErrorData{TurnID: turnID, Error: err.Error()}
	}
	data, mErr := json.Marshal(payload)
	if mErr != nil {
		// 负载全是字符串，实际不可能失败；真失败了帧本身也比什么都不发强。
		data = nil
	}
	emitFrame(journals, pub, sessionID, typ, data)
}

// emit 是 loopRunner 上的便捷写法。
func (r *loopRunner) emit(sessionID, typ string, data json.RawMessage) transport.ServerFrame {
	return emitFrame(r.journals, r.pub, sessionID, typ, data)
}

// emitTurn 是 loopRunner 上的便捷写法。
func (r *loopRunner) emitTurn(sessionID, turnID string, err error) {
	emitTurnFrame(r.journals, r.pub, sessionID, turnID, err)
}
