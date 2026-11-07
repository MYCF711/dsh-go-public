package spi

// HistoryHit 是一条命中。
type HistoryHit struct {
	Seq     int64  `json:"seq"`
	Type    string `json:"type"`    // 事件类型
	Role    string `json:"role"`    // user / assistant / tool / system
	Excerpt string `json:"excerpt"` // 命中位置前后的一小段
	// Hidden 表示这条已经被压缩遮蔽（不在 surface 上了），但原文还在。
	Hidden bool `json:"hidden"`
}

// HistorySearch 在会话的原始日志里按关键词检索。
//
// 检索范围是**全部事件**，包括已被压缩遮蔽的那些——这正是它的价值。
type HistorySearch interface {
	Search(query string, limit int) ([]HistoryHit, error)
}
