package backend

import (
	"context"
	"time"
)

// Phase 5 only consumes durable auto-delta scheduler slots / queues runs.
// There is intentionally NO Scanner HTTP/page worker until Phase 6/7.
// This loop runs only while App.Run is serving; EIO startup at 07:00 never
// calls today's 05:00 slot, and does not catch up missed days.
func (a *App) scannerScheduleLoop(ctx context.Context) {
	if a.Scanner == nil {
		return
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		if a.Logger != nil {
			a.Logger.Errorf("scanner scheduler timezone unavailable: %v", err)
		}
		return
	}
	for {
		now := time.Now().In(location)
		next := time.Date(now.Year(), now.Month(), now.Day(), 5, 0, 0, 0, location)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		instant := time.Now()
		// If the host was asleep/stalled past this minute, the slot was missed.
		local := instant.In(location)
		if local.Hour() != 5 || local.Minute() != 0 {
			continue
		}
		for _, up := range a.ConfigStore.Snapshot().Upstream {
			if up.ID == "" {
				continue
			}
			reason, err := a.scannerDailySlot(up.ID, instant)
			if err != nil {
				if a.Logger != nil {
					a.Logger.Warnf("scanner auto slot for source %s: %v", up.ID, err)
				}
				continue
			}
			if reason == "queued" && a.Logger != nil {
				a.Logger.Infof("scanner queued daily delta run for upstream %s", up.ID)
			}
		}
	}
}
