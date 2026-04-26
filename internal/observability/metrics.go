package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	NotificationsCreated = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "notifications_created_total", Help: "Notifications accepted by the API."},
		[]string{"type"},
	)
	NotificationsDelivered = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "notifications_delivered_total", Help: "Per-channel deliveries by terminal status."},
		[]string{"channel", "status"},
	)
	NotificationsFailed = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "notifications_failed_total", Help: "Per-channel permanent failures."},
		[]string{"channel", "reason"},
	)
	TemplateRenderErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "template_render_errors_total", Help: "Template rendering errors."},
		[]string{"template"},
	)
	ChannelSendDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "channel_send_duration_seconds",
			Help:    "Channel adapter send latency.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		},
		[]string{"channel"},
	)
	RateLimitRejections = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "rate_limit_rejections_total", Help: "Rate-limit rejections."},
		[]string{"channel"},
	)
	EnqueueFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "enqueue_failures_total", Help: "Enqueue attempts that returned an error at the API layer."},
		[]string{"source"},
	)
	Reconciled = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "notifications_reconciled_total", Help: "Notifications re-enqueued by the worker-side reconciler."},
		[]string{"result"},
	)
	// HTTP server histograms. Route is the Fiber route template
	// (e.g. "/v1/notifications/:id") so cardinality stays bounded.
	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "API HTTP request latency.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14), // 1ms .. ~16s
		},
		[]string{"method", "route", "status"},
	)
	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "http_requests_total", Help: "API HTTP request count."},
		[]string{"method", "route", "status"},
	)
	// Asynq queue-depth gauges published by the worker so Prometheus can
	// scrape them and the Prometheus Adapter can surface them to the K8s
	// HPA external-metrics API.
	QueueSize = promauto.NewGaugeVec(
		prometheus.GaugeOpts{Name: "asynq_queue_size", Help: "Asynq queue size by state."},
		[]string{"queue", "state"},
	)
	RateLimitBackendErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rate_limit_backend_errors_total",
			Help: "Rate limiter Redis backend errors. Service fails open on these — permit the request — so this is the signal an operator watches to know the limiter is degraded.",
		},
		[]string{"channel"},
	)
	// StoreWriteErrors tracks dispatcher status / delivery writes that
	// failed but did NOT block task completion. The task keeps running
	// because the channel delivery already succeeded on the wire; we
	// still want the counter + log so operators can catch DB drift.
	StoreWriteErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "store_write_errors_total",
			Help: "Non-blocking store writes that failed in the worker (dispatcher status rollup + per-channel delivery updates). Reconciler will eventually re-drive stuck rows; this metric makes the drift legible.",
		},
		[]string{"op"}, // op = update_status | update_delivery | ensure_delivery
	)
	// IdempotencyCacheWriteErrors tracks failures of the Redis Idempotency.Set
	// after a successful API response. Correctness is preserved by the
	// Postgres partial-unique index on idempotency_key (a subsequent retry
	// with the same key trips 23505 and hits the replay path), but the
	// Redis fast-path is degraded and operators should know.
	IdempotencyCacheWriteErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "idempotency_cache_write_errors_total",
			Help: "Idempotency-Key Redis Set failures after a successful API response. PG unique index is the source of truth; this metric surfaces cache-path degradation.",
		},
	)
)
