# DESIGN.md — notifd

This is the design doc for `notifd`, a multi-channel notification service (email, Slack, in-app) written in Go. I wrote it as I'd write an internal RFC: what I built, why I built it that way, what I deliberately left out, and what I would change at scale. The `README.md` is the "how do I run it" doc; this one is the "why does it look like this" doc.

## 1. The problem I was solving

The brief asked for a notification service that can send across email, Slack, and in-app channels; support scheduled delivery; and support templates — with explicit encouragement to make my own design calls.

A one-page brief like this is really asking two questions: can you translate vague requirements into a coherent system, and do you know what "production-shaped" actually means for this genre of service? I took both as in-scope. Anyone can wire three APIs together; what I wanted to demonstrate is that I know where the sharp edges are (provider outages, retry herds, silent duplicates, template drift under load) and that I write code that holds up under them.

Non-goals I decided on explicitly:

- **Real Slack OAuth install.** The codebase ships a mock webhook server so the reviewer can run the full flow without touching a Slack workspace. The adapter interface means a real implementation is a single-file swap.
- **SMS and push channels.** The adapter pattern makes them cheap to add — fewer than 100 lines each — but including them now adds surface without proving anything new.
- **WebSocket push for in-app.** MVP is a REST pull endpoint. That's how Slack, Linear, and Notion expose their in-app feeds, and the path to WS is well-understood.
- **Multi-tenancy and RBAC.** Single tenant with a static API key. If a real deployment needed multi-tenancy I would add a `tenant_id` column to every table + route scoping; that's ~2 hours of work and I'd rather not fake it.
- **A management UI.** The stack already gives reviewers three UIs for free — Mailpit for email, Asynqmon for queues, slack-mock's `/received` for Slack — and a one-file static HTML demo for the in-app feed.

## 2. Architecture

```
                ┌───────────────────── HTTP ──────────────────────┐
                ▼                                                 │
         ┌─────────────┐     INSERT      ┌──────────────┐         │
         │  Fiber API  │────────────────▶│  PostgreSQL  │         │
         │             │◀──── SELECT ────│              │         │
         └──────┬──────┘                 └──────────────┘         │
                │ Enqueue(job, delay, retry)       ▲              │
                ▼                                  │ UPDATE       │
          ┌──────────┐        Dequeue       ┌──────┴─────────┐    │
          │  Redis   │◀──────────────────▶ │   Worker       │    │
          │ (Asynq)  │    DLQ on permanent │   (dispatcher) │    │
          └──────────┘                     └────────┬───────┘    │
                                                    │            │
                                                    ▼            │
                                    ┌────────┬────────┬────────┐ │
                                    │ Email  │ Slack  │ In-app │ │
                                    └────────┴────────┴────────┘ │
                                                                 │
                   Reconciler sweeps rows stuck at               │
                   `queued`/`processing` older than 30s ─────────┘
```

One binary, two subcommands: `notifd api` and `notifd worker`. They share the same `internal/` packages but scale independently. The API is stateless. The worker is a stateless Asynq consumer and also runs a small reconciler loop that recovers lost enqueues.

I picked this shape because the alternative — API doing the delivery itself — makes the API's p99 a function of the slowest provider. Decoupling via a durable queue moves that problem where it belongs and gives me retries, scheduling, and DLQ for free.

## 3. Data model

Seven tables. Every column is deliberate; nothing speculative.

| Table | Purpose |
|---|---|
| `users` | Identity plus per-channel addresses (email, slack_user_id, slack_webhook). |
| `user_channel_preferences` | Per-user opt-in/out per channel. A missing row means enabled. |
| `templates` | Name, subject/body_text/body_html, `default_channels[]`, `required_variables[]`. |
| `notifications` | Parent row. Rolled-up status, attempt counter, `scheduled_for`, `idempotency_key`, and a snapshot of the template content at create time. |
| `notification_deliveries` | **One row per (notification, channel)** — the fan-out ledger with per-channel status, provider id, and error. |
| `in_app_notifications` | The user-facing in-app feed. Kept separate from `notifications` because it has its own read/unread lifecycle and its own retention curve. |
| `schema_migrations` | golang-migrate bookkeeping. |

### Key invariants enforced in SQL

- **Idempotency:** partial unique index on `notifications (idempotency_key) WHERE idempotency_key IS NOT NULL`. The Redis cache is a fast path; this index is the source of truth.
- **Template immutability on a live queue:** `notifications.template_id` has `ON DELETE RESTRICT`, plus the notification row carries a snapshot of the template content. A caller who accepts a 202 is guaranteed the message they saw is the message that gets delivered, regardless of later edits or deletes.
- **Status enum hygiene:** the `notifications.status` CHECK allows exactly the six values the Go code writes: `queued`, `processing`, `sent`, `partially_sent`, `dlq`, `cancelled`. Drift between the schema and the writer has bitten me before, so I keep them tight.

### Indexes sized to the access patterns

- `notifications_stuck_idx ON (updated_at, id) WHERE status IN ('queued','processing')` — the reconciler's partial index. Without it, the reconciler scan would be O(total rows) every 30 seconds.
- `notifications_scheduled_idx ON (scheduled_for) WHERE scheduled_for IS NOT NULL AND status = 'queued'` — keeps the scheduled-row lookup cheap.
- `in_app_user_all_idx ON (user_id, created_at DESC, id DESC)` and its `WHERE read_at IS NULL` partial twin — the trailing `id DESC` lets the keyset cursor be served directly from the index with no sort node.

### Status rollup

The worker writes one row per channel in `notification_deliveries` as it delivers. Once every channel has reached a terminal state, the parent `notifications.status` rolls up to one of:

- `sent` — every enabled channel delivered.
- `partially_sent` — some delivered, some permanently failed. I treat this as first-class rather than lying "sent" when Slack succeeded and SMTP hard-bounced.
- `dlq` — nothing delivered; retries exhausted.
- `cancelled` — the user opted out of every requested channel.

While retries are still pending, `partially_sent` is a *transient* state and the task keeps coming back until every channel is terminal.

## 4. API

The full route table is in the README. A few design calls worth surfacing here:

- **`POST /v1/notifications` returns 202 Accepted**, not 200. Delivery is asynchronous by design and returning 200 would lie about what happened.
- **Channel resolution precedence:** explicit `channels[]` on the request wins first, then the template's `default_channels`, otherwise 400. After resolution, user preferences filter the set. An empty final set is `cancelled` — not `failed`. The user asked for this.
- **Required-variable validation runs at the API boundary**, not at render time. The template declares what it needs; the handler rejects missing variables with a specific 400 instead of surfacing a template-engine error three layers deep.
- **`Idempotency-Key` follows the Stripe contract:** same key + same body hash = cached response; same key + different body = **409 Conflict**. TTL 24h, cache keyed by `(api_key, route, key)` in Redis. The Postgres partial unique index on the key is the final authority for cross-request deduplication.
- **Auth** is a static `X-API-Key` compared with `crypto/subtle.ConstantTimeCompare` over SHA-256 digests, so response timing doesn't leak the key. Production would swap this for OIDC or mTLS.

## 5. Templates

- Go stdlib `text/template` for plain text and Slack bodies; `html/template` for email HTML, so `{{.name}}` containing `<script>...` can't XSS the recipient.
- `missingkey=error` is set globally. Referencing an undeclared variable crashes the render instead of silently emitting `<no value>` — which is exactly why I validate `required_variables` at the API layer.
- **Upsert validates syntax for every slot.** Both `text/template` and `html/template` parsers are invoked during `POST /v1/templates` so a broken template can't be persisted and then DLQ'd at send time. The two parsers don't have the same grammar — `html/template` catches issues (unclosed context boundaries, bad actions inside attribute values) that `text/template` does not.
- **Parsed templates are cached**, keyed by SHA-256 of the source. Re-parsing on every send is CPU waste the service doesn't need to pay.
- **Template snapshot on accept.** The API copies `subject`, `body_text`, `body_html`, `template_name`, and `required_variables` into the notification row at create time. The worker renders from the snapshot. Combined with `ON DELETE RESTRICT` on `template_id`, a template edit or delete can never change the content of an already-accepted notification.

## 6. Scheduling

- **Instant send** (no `scheduled_for`): `asynq.Enqueue` onto the `default` queue. Worker picks it up immediately.
- **Delayed send** (`scheduled_for` in the future): `asynq.ProcessAt(t)` onto the `scheduled` queue. Asynq stores the task in a Redis ZSET keyed by fire-time; its internal scheduler moves it into the ready set at the right moment.
- Jobs survive process restart because Redis runs with AOF persistence.
- `asynq.TaskID(notificationID)` gives us live-queue dedup: if the same notification is enqueued twice while the first task is still pending, the second call returns `asynq.ErrTaskIDConflict` rather than creating a duplicate. The reconciler depends on this — it classifies `ErrTaskIDConflict` as `already_queued`, not an error.

### The outbox-lite reconciler

The write path is INSERT then enqueue, with no distributed transaction between Postgres and Redis. If Redis is unreachable for the enqueue call, the Postgres row is already committed. Left unattended, that's a lost-work bug.

My fix is a reconciler goroutine inside the worker that sweeps rows matching:

```
status IN ('queued','processing')
AND updated_at < NOW() - stuck_after
AND (scheduled_for IS NULL OR scheduled_for <= NOW() + stuck_after)
```

every 30 seconds and re-enqueues them. The last clause is load-bearing — without it, every future-scheduled row would match the stale window and the reconciler would try to re-enqueue it on every tick, hammering the `enqueue_error` counter with false positives. Instant sends and near-term scheduled sends are both recovered; far-future sends are left alone until they're due.

Asynq's TaskID dedup makes picking up a row that actually made it into Redis safe: `EnqueueSend` returns `ErrTaskIDConflict`, which I explicitly classify as `already_queued` rather than an error.

Sweeps paginate with a compound `(updated_at, id)` keyset cursor and keep draining until the result set is empty, `MaxPerTick` is hit, or the context fires. That bounds a single tick's work while still letting a multi-minute Redis outage drain quickly once it's over.

This is a lighter-weight version of a full transactional outbox. I accept up to one sweep-interval of drift on the pathological "commit succeeded, Redis down" edge case in exchange for not having to scan a second outbox table. The trade-off is called out here so a future reader knows exactly where the correctness seam is.

## 7. Retries, failure classification, and DLQ

Retry behaviour is driven by typed errors in `internal/apperr`:

| Error kind | Example | Worker behaviour |
|---|---|---|
| `Invalid` / `NotFound` / `Unauthorized` | 400/401/404 at the API boundary | Rejected before enqueue — never retried. |
| `Permanent` | SMTP 5xx, Slack 4xx, malformed payload, template-syntax error, pg FK/CHECK/unique violation | `asynq.SkipRetry` → moved to the archived set (DLQ) immediately. |
| `Transient` | SMTP timeout, Slack 5xx/429, Redis blip, pg transient error | Asynq retries with exponential backoff + jitter. |

Retry delays are computed by `retryDelayWithJitter`: exponential `2^n` seconds with ±50% uniform jitter, hard-capped to `[1s, 10m]` post-jitter, and `n` clamped to `[0, 30]` to avoid `time.Duration` overflow. The jitter is not optional — without it, every task at retry count `n` lands at the same wall-clock second, producing a stampede the moment the provider comes back up. Default `MaxRetry` is 8.

### Partial-failure retry semantics

When one channel succeeds and another transiently fails, the task must be retried — but it must not re-send the channel that already succeeded. The dispatcher handles this by:

1. Loading existing `notification_deliveries` on every run and building the work set as `enabled_channels \ {ch : delivery[ch].status == 'sent'}`.
2. Calling the channel adapters only for that work set.
3. If any remaining failure is transient, returning a plain error so Asynq retries with backoff. `partially_sent` is a *transient* parent state in that case — the task keeps coming back until every channel is terminal.
4. If the remaining failures are all permanent, writing a final `partially_sent` (some channels delivered) or `dlq` (none did) and stopping.

The same-channel-twice case is an invariant I care enough about to lock in with a unit test that creates a fake delivery ledger pre-marked `sent` and asserts the adapter isn't called on retry.

## 8. Idempotency

Clients retry on network errors. I don't want a double send. Two layers:

**Fast path — Redis cache.** On `POST /v1/notifications` with an `Idempotency-Key` header, I look up `(api_key, route, key)` in Redis. A hit with the same body hash replays the cached response. A hit with a different body hash returns 409 Conflict; reusing a key for a different request is a client bug worth surfacing.

**Source of truth — Postgres unique index.** The cache-miss path is not atomic. Two concurrent requests with the same key can both pass the cache check and race into the insert. The partial unique index on `notifications.idempotency_key` (key alone — this is safe today because only `POST /v1/notifications` persists notifications; if a second write-route ever uses the same key space I would widen the index before shipping) ensures the second insert trips `23505 unique_violation`. The store maps that to `ErrIdempotencyKeyReused` and the handler recovers by fetching the winning row and returning it as a 202 replay. The caller sees a consistent response either way; no 500 leaks from the race.

## 9. Rate limiting

Per-`(user_id, channel)` fixed-window counter in Redis. One-minute buckets, Lua-free `INCR + EXPIRE`. Default 60 requests per minute.

I chose fixed-window over sliding-window because at this scale the 2× cross-boundary burst is acceptable for recipient-protection. `golang.org/x/time/rate` is the upgrade path when that trade-off stops being acceptable.

**Failure policy is fail-open.** If Redis is unreachable the limiter returns an error; the handler logs WARN, bumps `rate_limit_backend_errors_total{channel}`, and permits the request. A Redis incident is already a critical incident — promoting it into a total API outage by returning 5xx is worse than permitting a brief burst above the configured budget. The distinct metric (vs. `rate_limit_rejections_total`, which tracks legitimate 429s) means an operator can tell the two apart.

A per-provider rate limiter — SendGrid/SES account QPS — belongs inside the channel adapter. I haven't built it because the mock adapters don't rate-limit, but it's called out as the obvious next step.

## 10. Observability

- **Structured JSON logs** via zerolog. Every request carries `request_id` (honouring the caller's `X-Request-ID` header or minting one). Every worker log line carries `notification_id`.
- **Prometheus metrics** at `/metrics` on the API (`:8080`) and on the worker (`:9100`, exposed as `:9101` on the host). The ones that matter:
  - `http_request_duration_seconds{method,route,status}` histogram + `http_requests_total` counter. Route label is the Fiber route template, not the raw path, so cardinality is bounded. `/metrics`, `/healthz`, and `/readyz` are explicitly excluded from the histogram so K8s probes don't dominate the bottom buckets.
  - `notifications_created_total{type}`, `notifications_delivered_total{channel,status}`, `notifications_failed_total{channel,reason}`.
  - `channel_send_duration_seconds{channel}` histogram — per-channel latency.
  - `template_render_errors_total{template}`.
  - `rate_limit_rejections_total{channel}` (429s) and `rate_limit_backend_errors_total{channel}` (Redis down, failing open).
  - `idempotency_cache_write_errors_total` — Redis `Set` failures after a successful API response. Correctness is preserved by the PG unique index; this metric surfaces cache-path degradation.
  - `store_write_errors_total{op}` — dispatcher status/delivery writes that failed but didn't block task completion. The reconciler will eventually re-drive stuck rows; the counter makes the drift legible.
  - `notifications_reconciled_total{result}` — one of `ok`, `already_queued`, `enqueue_error`, `list_error`.
  - `asynq_queue_size{queue,state}` gauge published by the worker every 10s from Asynq's Inspector, for HPA scaling on queue depth.
- **Asynqmon** at `:8090` gives a queue/DLQ/retry dashboard for free.

Distributed tracing (OpenTelemetry) is the obvious next step. I left it out because faking tracing — spans that don't propagate across boundaries — is worse than not claiming it.

## 11. Security posture

- **AuthN.** Static `X-API-Key` compared in constant time via SHA-256 digests fed to `crypto/subtle.ConstantTimeCompare`. A remote attacker cannot recover the key by measuring response timing. Production swap: OIDC for human actors, mTLS or a JWT-with-rotation for service-to-service.
- **AuthZ.** Single-tenant. No RBAC. Multi-tenancy means adding `tenant_id` to every table plus middleware that scopes it.
- **HTML injection.** `html/template` auto-escapes HTML bodies. Plain-text templates deliberately do not escape — that's the correct default for plain text.
- **CORS.** Env-gated via `CORS_ALLOW_ORIGINS`. Default is `*` (permissive, required for the `file://` in-app demo); production must set a concrete allowlist. `AllowCredentials` is pinned to `false` explicitly so flipping to cookie-based auth later can't silently widen this into a CSRF hole.
- **Secrets.** 12-factor, env-var only. `.env.example` is committed; `.env` is in `.gitignore`. Real deployments source from a secrets manager.
- **PII at rest.** Email addresses and in-app bodies sit in Postgres. Retention and deletion policies are not implemented; at real volume I'd add a nightly job that archives and deletes notifications older than N days.

## 12. Scale and deployment

### Capacity estimate

A round-number scenario to anchor the numbers: 10M users × 10 notifications/day = 100M/day ≈ 1.2K/s average, ~12K/s peak. Row size ~1KB → 100 GB/day raw for `notifications` + `notification_deliveries`; hot retention 30 days, archive older. Peak queue depth: 12K enq/s × average 2s delivery ≈ 24K in-flight. At `WORKER_CONCURRENCY=20` that's around 1,200 worker pods, or fewer with higher per-pod concurrency.

### Per-tier scaling

| Tier | Approach |
|---|---|
| **API** | Stateless, behind an L7 LB. N replicas scaled on CPU + in-flight requests. |
| **Worker** | Stateless; Asynq partitions work by task. Autoscale on **queue depth** — CPU lags because workers are almost entirely blocked on provider I/O. |
| **Postgres** | Vertical first. Then read replicas for analytics. Then partition `notifications` and `notification_deliveries` by month. Then shard on `tenant_id`. |
| **Redis** | Primary/replica for HA. Separate instances for queue vs. cache/rate-limit — very different failure semantics. |
| **External providers** | Per-provider token bucket + fallback provider (e.g. SendGrid → SES on sustained error-rate). |

### When I would re-platform

- **Redis → Kafka:** sustained ≥50K msg/s, or when a second consumer group (analytics, audit) needs the same stream. The adapter interface stays identical; the enqueuer implementation changes.
- **Single region → multi-region active/active:** when regulation or latency requires it. Per-region DB + queue; cross-region dedup via idempotency keys.
- **Monolith adapters → per-channel services:** only when one channel's volume or latency profile drags down the others. Not before.

### Deployment

Local: `docker compose up`. One command, no accounts.

Production sketch (not built, but the shape is clear):

- Kubernetes. One `Deployment` for API (HPA on CPU + request rate), one for worker (HPA on `asynq_queue_size` via the Prometheus Adapter; manifests in `deploy/k8s/`).
- Managed Postgres (RDS / Cloud SQL) and managed Redis (ElastiCache / Upstash).
- CI/CD: GitHub Actions → multi-arch image build → push → ArgoCD or `kubectl apply`.
- Secrets via an external secrets manager, injected at deploy time.
- Config: 12-factor, env-var only.

### SLOs I'd commit to

- API availability (non-5xx rate): **99.9%** (43 min/mo error budget).
- p99 instant-send end-to-end: **< 5 s**.
- Scheduled-send drift: **p99 < 30 s** from the scheduled time.
- DLQ depth alert: >0.1% of daily volume.

## 13. What I would do with more time (ranked)

1. **Migrate the SMTP adapter from `gopkg.in/gomail.v2` to `github.com/wneessen/go-mail`.** gomail v2 has no context support and no dial-timeout knob, which forces the two-goroutine race pattern currently in `internal/channels/email.go`. `wneessen/go-mail` accepts a `context.Context` natively; migrating would eliminate the "background goroutine outlives the call until the OS TCP timeout" caveat I document in that file.
2. **OpenTelemetry tracing** across API + worker + adapters. Biggest operational win for debugging production incidents.
3. **WebSocket push for in-app.** REST pull is fine for MVP; WS is the natural next step.
4. **Provider abstraction for email and Slack** — pluggable SendGrid / SES / Postmark; real Slack OAuth install + Block Kit bodies.
5. **Multi-tenancy + RBAC** — `tenant_id` column on every table; API-keys table with scopes.
6. **OpenAPI spec + generated typed clients.**
7. **Timezone-aware quiet hours** and per-user delivery windows.
8. **Template A/B testing** — versioned templates, traffic splits, per-variant metrics.
9. **Localization** — template variants keyed by locale.
10. **PII retention + deletion jobs.**

## 14. Trade-offs I want to call out

- **One Redis cluster for queue + cache + rate-limit.** That's a single point of failure in the current config. Production separates them.
- **No tracing today.** Logs + metrics are enough for this scale; they won't be at 10×.
- **Slack is mocked, not real.** Submitting with a real workspace install is two extra hours and adds zero insight to the design. The mock captures the adapter contract.
- **No admin UI.** Asynqmon + Mailpit + slack-mock give reviewers a live picture without one.
- **Fixed-window rate limiter.** Edge-of-window bursts can reach 2× the limit briefly. Acceptable at the traffic profile the service is sized for.
- **Gomail.v2 dial cancellation is best-effort.** See `internal/channels/email.go` for the detailed explanation and the migration path.

## 15. Why not Kafka / RabbitMQ instead of Redis+Asynq?

A reasonable question. My answer, in one line: Asynq gives me durable delayed jobs, exponential retries with jitter, DLQ semantics, unique-task dedup, and per-queue priority in a single Go library with a real admin UI (Asynqmon). RabbitMQ needs a plugin for delayed messages; Kafka handles none of the retry/DLQ semantics out of the box. For the problem at hand — scheduling, retries, DLQ — Asynq is the right tool. At ≥50K msg/s or when I need pub/sub fan-out to multiple independent consumer groups, I would move to Kafka and keep the adapter interface identical.
