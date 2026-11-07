package session

import "strings"

// sessionIDPrefix 是界面（session.list）看到的会话 id 前缀。
//
// 磁盘上的目录名与日志 header 里的 id 都是裸 uuid，不带这个前缀。
const sessionIDPrefix = "session-"

// NormalizeID 把界面传来的会话 id 变成磁盘上找得着的那个。
//
// 界面看到的是 "session-<uuid>"（列表里就长这样），而目录名与 header 里是裸 "<uuid>"。
// 两种写法都必须认——否则界面把列表里的 id 原样传回来就会 404，
// 而这个 404 看起来像"会话不存在"，极难查。
//
// 规则：
//   - 有 "session-" 前缀就去掉
//   - 已经是裸 uuid 就原样返回
//   - 空串返回空串（由调用方判空，这里不报错）
//
// 只做前缀剥离，不去解析/校验 uuid 格式：认不出来的写法原样返回，
// 让调用方按 CandidateIDs 的顺序自己去找。
func NormalizeID(id string) string {
	if id == "" {
		return ""
	}
	if !strings.HasPrefix(id, sessionIDPrefix) {
		return id
	}
	bare := strings.TrimPrefix(id, sessionIDPrefix)
	if bare == "" {
		// 只有前缀本身："session-" 不是一个可用的裸 id，
		// 剥掉会得到空串，反而把一次"找不到"变成"不知道要找什么"。
		return id
	}
	return bare
}

// CandidateIDs 返回"先试哪个、再试哪个"的顺序，调用方按顺序找。
//
// 为什么不是只返回一个：真实语料里两种命名都存在过，
// 猜错了就是找不到，所以两种都给，谁先命中用谁。
//
//	"session-x" -> ["x", "session-x"]   // 先试去掉前缀的
//	"x"         -> ["x"]                // 不重复添加同一个
//	""          -> nil                  // 由调用方判空
func CandidateIDs(id string) []string {
	if id == "" {
		return nil
	}
	normalized := NormalizeID(id)
	if normalized == id {
		return []string{id}
	}
	return []string{normalized, id}
}
