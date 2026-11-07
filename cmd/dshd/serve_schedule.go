package main

import (
	"errors"
	"path/filepath"
	"time"

	"dshgo/internal/schedule"
	"dshgo/internal/transport"
)

// serveScheduleList 保留原 list 的容错语义：读失败对旧面板表现为空。
// mutation/readback 必须走 serveScheduleListStrict，不能沿用这里的吞错。
func serveScheduleList(home string) []transport.ScheduleItem {
	items, err := serveScheduleListStrict(home)
	if err != nil {
		return nil
	}
	return items
}

func serveScheduleListStrict(home string) ([]transport.ScheduleItem, error) {
	dir := filepath.Join(home, "state", "schedules")
	sessions, err := schedule.Sessions(dir)
	if err != nil {
		return nil, err
	}
	out := make([]transport.ScheduleItem, 0, len(sessions))
	for _, sid := range sessions {
		items, revision, err := schedule.LoadWithRevision(dir, sid)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			out = append(out, transport.ScheduleItem{
				ID: it.ID, SessionID: sid, Title: it.Title, Kind: it.Kind, NextAt: it.NextAt,
				EverySec: it.EverySec, DailyAt: it.DailyAt, WeeklyAt: it.WeeklyAt,
				Status: it.Status, EndedAt: it.EndedAt, Revision: revision,
			})
		}
	}
	return out, nil
}

func serveScheduleDelete(home, sessionID, id, expectedRevision string) (transport.ScheduleDeleteResult, error) {
	result, err := schedule.Delete(filepath.Join(home, "state", "schedules"), sessionID, id, expectedRevision)
	if errors.Is(err, schedule.ErrNotFound) {
		return transport.ScheduleDeleteResult{}, errors.Join(transport.ErrNoSuchSchedule, err)
	}
	if errors.Is(err, schedule.ErrRevisionConflict) {
		return transport.ScheduleDeleteResult{}, errors.Join(transport.ErrRevisionConflict, err)
	}
	if err != nil {
		return transport.ScheduleDeleteResult{}, err
	}
	return transport.ScheduleDeleteResult{
		SessionID: result.SessionID, ID: result.ID, Deleted: result.Deleted,
		Remaining: result.Remaining, Revision: result.Revision,
	}, nil
}

func serveScheduleClear(home string) (transport.ScheduleClearResult, error) {
	dir := filepath.Join(home, "state", "schedules")
	result, err := schedule.ClearEnded(dir, time.Now())
	if err != nil {
		return transport.ScheduleClearResult{}, err
	}
	// 官方 clear-ended 只有在权威 readback 完成后才算成功。
	readback, err := serveScheduleListStrict(home)
	if err != nil {
		return transport.ScheduleClearResult{}, err
	}
	return transport.ScheduleClearResult{Deleted: result.Deleted, Revisions: result.Revisions, Schedules: readback}, nil
}
