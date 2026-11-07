package main

import (
	"sort"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// untitled 是 Summary 为空时的兜底标题。
// 界面上"标题栏空着"和"这个会话什么也没有"长得一模一样，用占位符把两者分开。
const untitled = "(无标题)"

// listSessions 是 session.list 的宿主实现。
//
// 它只做一件事：把 internal/session 已经算好的 Listing 转成界面认识的形状。
// 不要在 transport 里加第二个"读磁盘"的入口——列表只有一份真相。
func listSessions(home, cwd string) ([]transport.SessionSummary, error) {
	listings, err := session.ListSessions(home, cwd)
	if err != nil {
		return nil, err
	}

	// 先按时间排、再转字符串：UpdatedAt 出去是 RFC3339，带着时区偏移，
	// 直接比字符串会被 "+08:00"/"Z" 混排骗过去。time.Time 没有这个问题。
	sort.SliceStable(listings, func(i, j int) bool {
		return listings[i].UpdatedAt.After(listings[j].UpdatedAt)
	})

	out := make([]transport.SessionSummary, 0, len(listings))
	for _, l := range listings {
		title := l.Summary
		if l.Title != nil {
			title = *l.Title
		} else if title == "" {
			title = untitled
		}
		out = append(out, transport.SessionSummary{
			ID:            l.SessionID,
			Title:         title,
			TitleExplicit: l.Title != nil,
			TitleSource:   l.TitleSource,
			CWD:           l.Cwd,
			UpdatedAt:     l.UpdatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}
