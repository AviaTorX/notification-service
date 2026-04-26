// Package queue is a thin facade over Asynq so the rest of the code can
// enqueue notifications without learning Asynq's API surface.
package queue

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

const (
	TaskSendNotification = "notification:send"

	QueueDefault   = "default"
	QueueScheduled = "scheduled"
)

// SendPayload is the JSON body Asynq carries. IdempotencyKey is only used
// by Asynq's `Unique` option to collapse accidental duplicates — real
// HTTP-level idempotency is handled separately in the idempotency package.
type SendPayload struct {
	NotificationID uuid.UUID `json:"notification_id"`
}

type Enqueuer struct {
	Client *asynq.Client
}

func NewEnqueuer(redisAddr string) *Enqueuer {
	return &Enqueuer{
		Client: asynq.NewClient(asynq.RedisClientOpt{Addr: redisAddr}),
	}
}

func (e *Enqueuer) Close() error { return e.Client.Close() }

// EnqueueOptions shapes a single `notification:send` task handoff to
// Asynq. ProcessAt=zero means "process immediately on the default
// queue"; a non-zero future value is honoured by routing to the
// `scheduled` queue instead. Retries=0 falls back to this package's
// default of 8 (see defaultInt below).
type EnqueueOptions struct {
	ProcessAt time.Time
	Retries   int
}

func (e *Enqueuer) EnqueueSend(p SendPayload, opts EnqueueOptions) (*asynq.TaskInfo, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}

	aopts := []asynq.Option{
		// TaskID gives us live-queue dedupe: if the same notification is
		// already pending/scheduled/active in Redis, a second Enqueue
		// returns asynq.ErrTaskIDConflict instead of creating a
		// duplicate. The reconciler depends on this — it classifies
		// ErrTaskIDConflict as "already_queued", not an error.
		asynq.TaskID(p.NotificationID.String()),
		asynq.MaxRetry(defaultInt(opts.Retries, 8)),
	}

	if !opts.ProcessAt.IsZero() && time.Until(opts.ProcessAt) > 0 {
		aopts = append(aopts, asynq.ProcessAt(opts.ProcessAt), asynq.Queue(QueueScheduled))
	} else {
		aopts = append(aopts, asynq.Queue(QueueDefault))
	}

	task := asynq.NewTask(TaskSendNotification, body, aopts...)
	return e.Client.Enqueue(task)
}

func defaultInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}
