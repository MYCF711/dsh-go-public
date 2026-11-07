package main

import (
	"encoding/json"

	"dshgo/codecs/v4"
)

// v4SeqPtr 复制一个持久事件序号。不能取 range 变量 ev 的字段地址：
// 那会让多条 load 消息共享同一个指针，后一条把前一条的身份覆盖掉。
func v4SeqPtr(seq int64) *int64 { return &seq }

// persistentMessageID 只读 v4 data 已经持久化的消息 id。当前 user 在顶层 id，
// assistant 构造器没有 id；没有就返回空串，绝不按时间、正文或 Transport seq 生成。
func persistentMessageID(ev v4.Event) string {
	if ev.Type != "user/message" && ev.Type != "assistant/message" {
		return ""
	}
	var d struct {
		MessageID string `json:"messageId"`
		ID        string `json:"id"`
		Message   *struct {
			MessageID string `json:"messageId"`
			ID        string `json:"id"`
		} `json:"message"`
	}
	if json.Unmarshal(ev.Data, &d) != nil {
		return ""
	}
	for _, id := range []string{d.MessageID, d.ID} {
		if id != "" {
			return id
		}
	}
	if d.Message != nil {
		for _, id := range []string{d.Message.MessageID, d.Message.ID} {
			if id != "" {
				return id
			}
		}
	}
	return ""
}

// projectV4MessageIdentity 把持久 v4 信封的身份加入派生 Transport data。
// v4 文件与旧 Transport journal 都不会在这里被打开或改写；只有新派生帧获得字段。
// user/assistant/step/delta 之外的类型、非对象或损坏 data 原样返回，由客户端走既有 scoped fallback。
func projectV4MessageIdentity(ev v4.Event) json.RawMessage {
	switch ev.Type {
	case "user/message", "assistant/message",
		"step/start", "step/end", "assistant/text", "assistant/reasoning":
		// step/delta 与消息一样有持久 v4 信封；重放必须能按 v4Seq 去重。
	default:
		return ev.Data
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(ev.Data, &data) != nil || data == nil {
		return ev.Data
	}
	seq, _ := json.Marshal(ev.Seq)
	data["v4Seq"] = seq
	if id := persistentMessageID(ev); id != "" {
		raw, _ := json.Marshal(id)
		data["messageId"] = raw
	}
	out, err := json.Marshal(data)
	if err != nil {
		return ev.Data
	}
	return out
}
