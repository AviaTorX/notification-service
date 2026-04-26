package worker

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"

	"github.com/avinash-shinde/notification-service/internal/channels"
	"github.com/avinash-shinde/notification-service/internal/config"
	"github.com/avinash-shinde/notification-service/internal/observability"
	"github.com/avinash-shinde/notification-service/internal/queue"
	"github.com/avinash-shinde/notification-service/internal/store"
)

// Run wires the worker dependencies, starts the Asynq server + the
// enqueue-reliability reconciler + the queue-depth exporter, and blocks
// until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config) error {
	st, err := store.New(ctx, cfg.PostgresDSN, store.PoolOptions{
		MaxConns: int32(cfg.PostgresMaxConns),
		MinConns: int32(cfg.PostgresMinConns),
	})
	if err != nil {
		return err
	}
	defer st.Close()

	registry := channels.NewRegistry(
		channels.NewEmailAdapter(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPFrom),
		channels.NewSlackAdapter(cfg.SlackWebhookURL),
		channels.NewInAppAdapter(st),
	)

	d := NewDispatcher(st, registry)

	enq := queue.NewEnqueuer(cfg.RedisAddr)
	defer enq.Close()
	reconciler := &Reconciler{
		Store:      st,
		Enqueuer:   enq,
		Interval:   cfg.ReconcilerInterval,
		StuckAfter: cfg.ReconcilerStuck,
		BatchLimit: cfg.ReconcilerBatch,
		// MaxPerTick = 50x batch by default: keeps a single tick from
		// starving its interval, but a deep backlog is drained quickly.
		MaxPerTick: cfg.ReconcilerBatch * 50,
	}
	go reconciler.Run(ctx)

	// Queue-depth exporter. The K8s HPA scales on
	// asynq_queue_size{queue="default"} via the Prometheus Adapter;
	// this goroutine keeps that gauge fresh.
	queueInspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: cfg.RedisAddr})
	defer queueInspector.Close()
	go publishQueueDepth(ctx, queueInspector)

	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.RedisAddr},
		asynq.Config{
			Concurrency: cfg.WorkerConcurrency,
			Queues: map[string]int{
				queue.QueueDefault:   6,
				queue.QueueScheduled: 4,
			},
			RetryDelayFunc: retryDelayWithJitter,
			IsFailure: func(err error) bool { return err != nil },
			ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, t *asynq.Task, err error) {
				retried, _ := asynq.GetRetryCount(ctx)
				max, _ := asynq.GetMaxRetry(ctx)
				log.Error().Err(err).Int("retry", retried).Int("max", max).
					Str("type", t.Type()).Msg("task errored")
			}),
			Logger: asynqZerologAdapter{},
		},
	)

	mux := asynq.NewServeMux()
	mux.HandleFunc(queue.TaskSendNotification, d.Handle)

	metricsSrv := &http.Server{Addr: ":9100", Handler: promhttp.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info().Str("addr", ":9100").Msg("worker metrics listening")
		_ = metricsSrv.ListenAndServe()
	}()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Run(mux); err != nil {
			errCh <- err
		}
	}()

	log.Info().Int("concurrency", cfg.WorkerConcurrency).Msg("worker up")

	select {
	case <-ctx.Done():
		log.Info().Msg("worker shutdown requested")
		srv.Shutdown()
		_ = metricsSrv.Shutdown(context.Background())
		return nil
	case err := <-errCh:
		_ = metricsSrv.Shutdown(context.Background())
		return err
	}
}

// publishQueueDepth scrapes Asynq's queue stats every 10s and publishes
// them as gauges so the K8s HPA can scale worker pods on queue depth
// via the Prometheus Adapter.
func publishQueueDepth(ctx context.Context, insp *asynq.Inspector) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	queues := []string{queue.QueueDefault, queue.QueueScheduled}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, q := range queues {
				info, err := insp.GetQueueInfo(q)
				if err != nil {
					continue
				}
				observability.QueueSize.WithLabelValues(q, "pending").Set(float64(info.Pending))
				observability.QueueSize.WithLabelValues(q, "active").Set(float64(info.Active))
				observability.QueueSize.WithLabelValues(q, "scheduled").Set(float64(info.Scheduled))
				observability.QueueSize.WithLabelValues(q, "retry").Set(float64(info.Retry))
				observability.QueueSize.WithLabelValues(q, "archived").Set(float64(info.Archived))
			}
		}
	}
}

// asynqZerologAdapter forwards Asynq's internal log lines through
// zerolog at the appropriate level.
type asynqZerologAdapter struct{}

func (asynqZerologAdapter) Debug(args ...interface{}) { log.Debug().Msg(fmt.Sprint(args...)) }
func (asynqZerologAdapter) Info(args ...interface{})  { log.Info().Msg(fmt.Sprint(args...)) }
func (asynqZerologAdapter) Warn(args ...interface{})  { log.Warn().Msg(fmt.Sprint(args...)) }
func (asynqZerologAdapter) Error(args ...interface{}) { log.Error().Msg(fmt.Sprint(args...)) }
func (asynqZerologAdapter) Fatal(args ...interface{}) { log.Fatal().Msg(fmt.Sprint(args...)) }

// retryDelayWithJitter returns an exponential backoff with ±50% jitter
// applied. Under a real provider outage, deterministic exponential
// backoff makes every retry at retry count `n` land at the same
// wall-clock second, producing a stampede when the provider comes back
// up. Jitter spreads those retries over time.
//
// Contract (what operators can rely on):
//
//   - Returned delay is always in [1s, 10m], inclusive.
//   - For the un-saturated range, delay ≈ base ± 50% where
//     base = min(2^n * 1s, 10m). Jitter is uniform in [base/2, 1.5*base]
//     then clamped to [1s, 10m] so the hard ceiling holds even after
//     the upper jitter swing.
//   - `n` is clamped to [0, 30] up front. Beyond that, `1<<n` overflows
//     time.Duration (int64 ns), so the raw base can go negative or
//     zero and rand.Int63n would panic. Asynq ships MaxRetry=8, but we
//     don't rely on that invariant — any caller bumping MaxRetry or
//     un-archiving a DLQ'd task must not crash the worker.
//
// math/rand is used because jitter doesn't need cryptographic quality.
// The global source's internal mutex is fine at the retry-rate this
// service runs; if that becomes hot, switch to math/rand/v2.
func retryDelayWithJitter(n int, _ error, _ *asynq.Task) time.Duration {
	if n < 0 {
		n = 0
	}
	if n > 30 {
		n = 30 // 2^30 seconds ≈ 34 years — well past the 10m cap anyway
	}
	const cap = 10 * time.Minute
	base := time.Duration(1<<uint(n)) * time.Second
	if base > cap {
		base = cap
	}
	// Uniform in [base/2, 1.5*base).
	jitterRange := time.Duration(rand.Int63n(int64(base))) // [0, base)
	delay := base/2 + jitterRange
	if delay > cap {
		delay = cap
	}
	if delay < time.Second {
		delay = time.Second
	}
	return delay
}
