package transport

// PageMode 决定这次取历史是"首屏"还是"补洞"。
type PageMode int

const (
	// PageTail 取最新一屏：给 session.load 用。
	PageTail PageMode = iota
	// PageSince 取 seq > sinceSeq 的那些：给 session.history 用。
	PageSince
)

// Page 是一次取历史的结果。
type Page struct {
	Messages  []Message `json:"messages"`
	Total     int       `json:"total"`     // 这个会话一共有多少条
	Truncated bool      `json:"truncated"` // 还有更早的没给（只有 PageTail 可能为 true）
	OldestSeq int       `json:"oldestSeq"` // 本页第一条的 seq；空页为 0
	NewestSeq int       `json:"newestSeq"` // 本页最后一条的 seq；空页为 0
}

// SelectPage 从一整个会话的消息里切出一页。
//
// 两条不可动摇的约定：
//
//	PageTail：返回**最后** limit 条；条数不够时全给。
//	          本来还有更早的没给 → Truncated=true。
//	PageSince：返回**所有** Seq > sinceSeq 的；limit>0 时最多 limit 条。
//	          Truncated 恒为 false —— 补洞没有"更早的没给"这回事。
//
// limit <= 0 表示"不限"。all 假定已按 Seq 升序。
//
// 这个前提**不在这里校验**，两条理由都实测过：
//  1. 183 个真实会话的 seq 全部严格递增（回退 0 处、重复 0 处）；
//  2. 保证它的机制在别处——同一份会话只允许一个写入者（<会话文件>.lock）。
//     没有写锁时两个进程各从 len(events) 编号，会写出重复 seq。
//
// 也就是说，去掉写锁之后这个前提才可能不成立。那时该报错还是该排序
// 是个产品决定，不该由这个纯函数偷偷替调用方决定。
func SelectPage(all []Message, mode PageMode, sinceSeq int, limit int) Page {
	total := len(all)

	var picked []Message

	switch mode {
	case PageSince:
		start := len(all)
		for i, m := range all {
			if m.Seq > sinceSeq {
				start = i
				break
			}
		}
		picked = all[start:]
		if limit > 0 && len(picked) > limit {
			picked = picked[:limit]
		}

		p := Page{
			Messages:  append([]Message(nil), picked...),
			Total:     total,
			Truncated: false, // 补洞没有"更早的没给"
		}
		if n := len(picked); n > 0 {
			p.OldestSeq = picked[0].Seq
			p.NewestSeq = picked[n-1].Seq
		} else {
			p.Messages = []Message{}
		}
		return p

	case PageTail:
		fallthrough
	default:
		start := 0
		truncated := false
		if limit > 0 && len(all) > limit {
			start = len(all) - limit
			truncated = true
		}
		picked = all[start:]

		p := Page{
			Messages:  append([]Message(nil), picked...),
			Total:     total,
			Truncated: truncated,
		}
		if n := len(picked); n > 0 {
			p.OldestSeq = picked[0].Seq
			p.NewestSeq = picked[n-1].Seq
		} else {
			p.Messages = []Message{}
		}
		return p
	}
}
