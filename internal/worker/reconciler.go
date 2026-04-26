package worker

import (
	"context"
	"errors"
	"time"

	"github.com/hibiken/asynq"
	"github.com/rs/zerolog/log"

	"github.com/avinash-shinde/notification-service/internal/observability"
	"github.com/avinash-shinde/notification-service/internal/queue"
	"github.com/avinash-shinde/notification-service/internal/store"
)

// ReconcilerStore is the narrow interface the reconciler needs from the
// persistence layer. Extracted so tests can substitute a fake without
// standing up Postgres.
type ReconcilerStore interface {
	ListStuckQueued(ctx context.Context, p store.StuckQueuedParams) ([]store.Notification, store.StuckCursor, error)
}

// ReconcilerEnqueuer is the narrow interface the reconciler needs from
// the queue facade. Same motivation as ReconcilerStore.
type ReconcilerEnqueuer interface {
	EnqueueSend(queue.SendPayload, queue.EnqueueOptions) (*asynq.TaskInfo, error)
}

// Reconciler sweeps the `notifications` table for rows stuck at `queued`
// or `processing` for longer than `StuckAfter` and re-enqueues them.
//
// Design notes:
//   - The sweep is periodic, not event-driven. A true outbox would use
//     LISTEN/NOTIFY or CDC; for this service's scale, a 30s scan is
//     massively simpler and accepts up to `StuckAfter + Interval` drift
//     on the lost-enqueue edge case.
//   - The sweep paginates with a compound (updated_at, id) keyset cursor
//     so a Redis outage that stacked up many thousands of rows can still
//     drain across multiple batches inside a single tick, up to
//     MaxPerTick. Subsequent ticks pick up where the previous one
//     stopped.
//   - Asynq's `TaskID(notificationID)` is a live-queue de-dupe. If a
//     row is already queued/scheduled in Redis, re-enqueue returns
//     `asynq.ErrTaskIDConflict`; we count that as an `already_queued`
//     outcome (not an error) because the task is actually fine. Real
//     errors still bump `enqueue_error`.
type Reconciler struct {
	Store      ReconcilerStore
	Enqueuer   ReconcilerEnqueuer
	Interval   time.Duration
	StuckAfter time.Duration
	BatchLimit int
	// MaxPerTick caps a single tick's total work so a catastrophic
	// backlog doesn't starve other responsibilities on the same worker.
	// Subsequent ticks pick up where this one stopped.
	MaxPerTick int
}

func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	stuckAfter := r.StuckAfter
	if stuckAfter <= 0 {
		stuckAfter = 30 * time.Second
	}
	batch := r.BatchLimit
	if batch <= 0 {
		batch = 1000
	}
	maxPerTick := r.MaxPerTick
	if maxPerTick <= 0 {
		maxPerTick = 50000
	}

	log.Info().
		Dur("interval", interval).
		Dur("stuck_after", stuckAfter).
		Int("batch_size", batch).
		Int("max_per_tick", maxPerTick).
		Msg("reconciler started")

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("reconciler shutting down")
			return
		case <-t.C:
			r.sweep(ctx, stuckAfter, batch, maxPerTick)
		}
	}
}

// sweep drains stuck notifications via keyset pagination until either
// (a) no more rows remain, (b) we hit maxPerTick, or (c) ctx fires.
func (r *Reconciler) sweep(ctx context.Context, stuckAfter time.Duration, batch, maxPerTick int) {
	var cursor store.StuckCursor
	processed := 0
	start := time.Now()
	for processed < maxPerTick {
		select {
		case <-ctx.Done():
			return
		default:
		}
		rows, next, err := r.Store.ListStuckQueued(ctx, store.StuckQueuedParams{
			OlderThan: stuckAfter,
			Limit:     batch,
			After:     cursor,
		})
		if err != nil {
			observability.Reconciled.WithLabelValues("list_error").Inc()
			log.Warn().Err(err).Msg("reconciler: ListStuckQueued failed")
			return
		}
		if len(rows) == 0 {
			if processed > 0 {
				log.Info().
					Int("count", processed).
					Dur("elapsed", time.Since(start)).
					Msg("reconciler: sweep drained")
			}
			return
		}
		for _, n := range rows {
			// Honor shutdown mid-batch so a large page can't delay
			// graceful termination by up to BatchLimit re-enqueues.
			select {
			case <-ctx.Done():
				return
			default:
			}
			r.reenqueueOne(n)
			processed++
		}
		if next.IsZero() {
			log.Info().
				Int("count", processed).
				Dur("elapsed", time.Since(start)).
				Msg("reconciler: sweep drained")
			return
		}
		cursor = next
	}
	log.Warn().
		Int("count", processed).
		Int("max_per_tick", maxPerTick).
		Msg("reconciler: sweep hit MaxPerTick cap; will continue on next tick")
}

// reenqueueOne handles a single row. Split out so the Asynq-dedup path
// is readable and unit-testable without wiring up the whole sweep loop.
func (r *Reconciler) reenqueueOne(n store.Notification) {
	var at time.Time
	if n.ScheduledFor != nil && time.Now().Before(*n.ScheduledFor) {
		at = *n.ScheduledFor
	}
	_, err := r.Enqueuer.EnqueueSend(
		queue.SendPayload{NotificationID: n.ID},
		queue.EnqueueOptions{ProcessAt: at},
	)
	switch {
	case err == nil:
		observability.Reconciled.WithLabelValues("ok").Inc()
	case errors.Is(err, asynq.ErrTaskIDConflict):
		// Task is already live in Redis (scheduled, pending, active,
		// or retrying). That is the correct state; it is not a failure
		// for the reconciler to discover it.
		observability.Reconciled.WithLabelValues("already_queued").Inc()
	default:
		observability.Reconciled.WithLabelValues("enqueue_error").Inc()
		log.Warn().Err(err).Str("notification_id", n.ID.String()).
			Msg("reconciler: re-enqueue failed")
	}
}
