package runtime

import (
	"context"
	"github.com/flagforge/flagforge/internal/eventbus"
	"github.com/flagforge/flagforge/internal/observability"
	"github.com/flagforge/flagforge/internal/repository"
	"github.com/flagforge/flagforge/internal/streaming"
	"log/slog"
	"sync"
	"time"
)

// Start gives every worker the same cancellation owner. The returned function joins them.
func Start(ctx context.Context, r *repository.Repository, bus *eventbus.Redis, hub *streaming.Hub, m *observability.Metrics, poll time.Duration, batch int) func() {
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() {
		for ctx.Err() == nil {
			if err := bus.Subscribe(ctx, hub.Publish); err != nil && ctx.Err() == nil {
				slog.Warn("redis subscription interrupted")
			}
			if !pause(ctx, time.Second) {
				return
			}
		}
	})
	run(func() {
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				work, stop := context.WithTimeout(ctx, 5*time.Second)
				_, err := r.PublishOutbox(work, bus, batch)
				if err != nil && ctx.Err() == nil {
					m.OutboxFailures.Inc()
					slog.Warn("outbox batch deferred")
				}
				var pending int64
				if r.Pool.QueryRow(work, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending) == nil {
					m.Pending.Set(float64(pending))
				}
				stop()
			}
		}
	})
	run(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for i := 0; i < 100; i++ {
					work, stop := context.WithTimeout(ctx, 5*time.Second)
					ran, err := r.RunSchedule(work)
					stop()
					if err != nil {
						slog.Warn("scheduler transaction failed")
						break
					}
					if !ran {
						break
					}
				}
			}
		}
	})
	return wg.Wait
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
