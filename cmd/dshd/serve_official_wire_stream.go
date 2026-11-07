package main

import (
	"context"
	"encoding/json"

	"dshgo/internal/officialwire"
	"dshgo/internal/transport"
)

// officialMuxStreamOpener 把官方 mux 的 open 帧接到官方帧机上。
//
// 为什么放在 cmd/dshd 而不是 internal/transport：transport 的生产代码已经
// import 了 officialwire，而 officialwire 的内部测试又 import transport；
// 把 opener 放进 transport 的生产文件会形成测试期 import 环。放在这里
// 两者都能 import，且生产/测试共用同一份实现。
// officialMuxStreamPageLimit 与 transport 侧的 officialStreamPageLimit 同值（256）。
const officialMuxStreamPageLimit = 256

type officialMuxStreamOpener struct {
	server *transport.Server
}

var _ officialwire.StreamOpener = officialMuxStreamOpener{}

// Open 把 mux 的 open 帧接到官方帧机上。
func (o officialMuxStreamOpener) Open(ctx context.Context, endpoint string, args json.RawMessage) (officialwire.Stream, *officialwire.Error) {
	// mux 的 endpoint 是官方斜杠形式（"session/events"），不是 L1 的点号方法名。
	if endpoint == "session/events" {
		// 本仓自有的尾部流（官方 session/events 是 unary 页，不是流）。
		var request struct {
			SessionID string `json:"sessionId"`
			SinceSeq  *int   `json:"sinceSeq"`
			Limit     *int   `json:"limit"`
		}
		if err := json.Unmarshal(args, &request); err != nil {
			return nil, officialwire.ArgumentsInvalid(endpoint, "args", "args 必须是对象：sessionId + 可选 sinceSeq/limit")
		}
		sinceSeq, limit := transport.SinceNothing, officialMuxStreamPageLimit
		if request.SinceSeq != nil {
			sinceSeq = *request.SinceSeq
		}
		if request.Limit != nil {
			limit = *request.Limit
		}
		stream, fault := o.server.OpenOfficialJournalTail(request.SessionID, sinceSeq, limit)
		if fault != nil {
			return nil, officialwire.NewError(fault.Code, fault.Message, map[string]any{"endpoint": endpoint})
		}
		return &officialMuxStream{stream: stream}, nil
	}
	stream, fault := o.server.OpenOfficialFrameStream(ctx, endpoint, args)
	if fault != nil {
		return nil, officialwire.NewError(fault.Code, fault.Message, map[string]any{"endpoint": endpoint})
	}
	return &officialMuxStream{stream: stream}, nil
}

// officialMuxStream 把 OfficialFrameStream 接成 officialwire.Stream。
type officialMuxStream struct {
	stream transport.OfficialFrameStream
}

func (m *officialMuxStream) Next(ctx context.Context) (officialwire.StreamItem, bool, *officialwire.Error) {
	frame, done, fault := m.stream.Next(ctx)
	if fault != nil {
		return officialwire.StreamItem{}, true,
			officialwire.NewError(fault.Code, fault.Message, map[string]any{"endpoint": ""})
	}
	if done {
		return officialwire.StreamItem{}, true, nil
	}
	return officialwire.StreamItem{Value: frame, HasValue: true}, false, nil
}

func (m *officialMuxStream) Close() { m.stream.Close() }
