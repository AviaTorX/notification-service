# notifd — a multi-channel notification service

Production-shaped reference implementation of a **notification service** that delivers the same message across **email, Slack, and in-app**, with **templates, scheduling, retries, idempotency, rate limiting, preferences, and observability** out of the box.

Written in **Go 1.22** with **GoFiber, PostgreSQL, Redis, and Asynq**. Everything — including a fake SMTP server and a Slack webhook mock — boots with a single command: `docker compose up`.

> **If you are reviewing this**: read [`DESIGN.md`](DESIGN.md). It is the RFC for the service and answers 90% of "why did you do X" questions.

---

## Quickstart (5 minutes)

```bash
# 1. Prereqs: Docker + docker compose. Nothing else.
cp .env.example .env

# 2. Bring up the full stack (API, worker, Postgres, Redis, Mailpit, Slack mock, Asynqmon).
docker compose up -d --build

# 3. Walk through every feature end-to-end.
bash scripts/demo.sh
```

**What opens up:**

| URL | What it is |
|-----|------------|
| http://localhost:8080 | API (`/v1/*`, `/healthz`, `/readyz`, `/metrics`) |
| http://localhost:9101/metrics | **Worker** Prometheus metrics — `asynq_queue_size`, per-channel delivery latency, reconciler counters |
| http://localhost:8025 | **Mailpit** — emails the service "sends" land here |
| http://localhost:9000/received | **Slack mock** — every Slack webhook POST is captured here |
| http://localhost:8090 | **Asynqmon** — queue / DLQ / retries UI |
| localhost:55432 | Postgres (DSN in `.env`) |
| localhost:56379 | Redis (queue + rate limit + idempotency cache) |

If you want a visual in-app feed, open `web/in-app-demo.html` directly in a browser — one static file, no build step, polls the API.

---

## Smoke test (copy-paste)

`USER` below is the seeded demo user (see `migrations/0001_init.up.sql`). Pick any other name for the variable — `UID` is reserved on zsh.

```bash
KEY=dev-api-key-change-me
USER=00000000-0000-0000-0000-000000000001

# Instant delivery to email + in-app (per `welcome` template defaults)
curl -sf -X POST http://localhost:8080/v1/notifications \
  -H "X-API-Key: $KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-$(date +%s)" \
  -d "{\"user_id\":\"$USER\",\"template_name\":\"welcome\",\"variables\":{\"name\":\"Avinash\",\"product\":\"NotifCo\"}}" | jq

# 3 seconds from now
FUTURE=$(python3 -c 'import datetime; print((datetime.datetime.now(datetime.UTC)+datetime.timedelta(seconds=3)).strftime("%Y-%m-%dT%H:%M:%SZ"))')
curl -sf -X POST http://localhost:8080/v1/notifications \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d "{\"user_id\":\"$USER\",\"template_name\":\"welcome\",\"variables\":{\"name\":\"Later\",\"product\":\"NotifCo\"},\"scheduled_for\":\"$FUTURE\"}" | jq

# Inspect in-app feed
curl -sf http://localhost:8080/v1/users/$USER/in-app-notifications -H "X-API-Key: $KEY" | jq

# Watch Mailpit at http://localhost:8025, Asynqmon at http://localhost:8090.
```

Full walk-through of every notable path (instant, scheduled, idempotency replay, conflict, Slack path, opt-out, missing-variable rejection):

```bash
bash scripts/demo.sh
# or: make demo
```

### Clicking through the demo in Bruno

If you'd rather see the API exercised via a UI, open `bruno/notifd/` with [Bruno](https://www.usebruno.com/). Collection includes four folders (Health, Notifications, Templates, Users) with 23 requests total, auto-captured IDs between requests, and assertions on every response. See [`bruno/README.md`](bruno/README.md) for the full tour.

### Creating your own template and user via the API

The migration seeds a demo user and three system templates (`welcome`, `password_reset`, `deploy_succeeded`). To register your own:

```bash
# Create a template
curl -sf -X POST http://localhost:8080/v1/templates \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{
    "name": "security_alert",
    "description": "Notifies a user of a suspicious login.",
    "subject": "Suspicious sign-in from {{.location}}",
    "body_text": "Hi {{.name}}, we noticed a sign-in from {{.location}} at {{.time}}.",
    "body_html": "<p>Hi <b>{{.name}}</b>, we noticed a sign-in from <b>{{.location}}</b> at {{.time}}.</p>",
    "default_channels": ["email", "in_app"],
    "required_variables": ["name", "location", "time"]
  }' | jq

# Create a user
curl -sf -X POST http://localhost:8080/v1/users \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{
    "email": "alice@example.com",
    "slack_webhook": "http://slack-mock:9000/webhook"
  }' | jq
```

---

## API surface

All write routes require `X-API-Key`. Optional `Idempotency-Key` on `POST /v1/notifications` gives Stripe-style replay semantics.

| Method | Path | Purpose |
|--------|------|---------|
| GET    | `/healthz` | Liveness |
| GET    | `/readyz` | Readiness (DB + Redis) |
| GET    | `/metrics` | Prometheus exposition |
| POST   | `/v1/notifications` | Create & enqueue (instant or scheduled) |
| GET    | `/v1/notifications/{id}` | Fetch status + per-channel deliveries |
| GET    | `/v1/templates` | List templates |
| POST   | `/v1/templates` | Upsert (create or update by name) |
| GET    | `/v1/templates/{id}` | Fetch a template |
| DELETE | `/v1/templates/{id}` | Delete |
| POST   | `/v1/users` | Upsert a user |
| GET    | `/v1/users/{id}/preferences` | List per-channel opt-ins |
| PUT    | `/v1/users/{id}/preferences` | Enable/disable a channel |
| GET    | `/v1/users/{id}/in-app-notifications[?unread=true&limit=N&before=<cursor>]` | Pull in-app feed |
| POST   | `/v1/users/{id}/in-app-notifications/{nid}/read` | Mark read |

### POST /v1/notifications — request shape

```json
{
  "user_id": "UUID",
  "template_name": "welcome",
  "template_id": "UUID (alternative)",
  "channels": ["email", "in_app"],
  "variables": {"name": "Ada", "product": "Widgets"},
  "scheduled_for": "2026-04-25T18:00:00Z"
}
```

Resolution order for channels: explicit `channels` → template `default_channels` → reject (400) if neither. User preferences filter the final set; an opt-out of every requested channel surfaces as `status=cancelled`.

Response is always `202 Accepted` with the notification row. Poll `GET /v1/notifications/{id}` for the per-channel delivery outcome, or watch Asynqmon.

---

## Tests

```bash
# Unit tests (no infra required)
go test ./...

# End-to-end (runs against the docker-compose stack)
docker compose up -d
E2E=1 go test ./tests/e2e/... -v
```

Covers: template rendering + variable validation, HTML auto-escape, error kind/retryability, SMTP error classification, idempotency hashing, dispatcher retry semantics (partial-failure retry, skip-already-sent, DLQ on permanent), reconciler (scheduled-row skip, TaskID-conflict handling, pagination), access-log status capture, CORS allowlist behaviour, rate-limit fail-open, instant + scheduled delivery, idempotent replay, key-reuse conflict, missing-variable rejection.

---

## Operating

```bash
make up           # build + start the stack
make down         # tear down incl. volumes
make logs         # tail api + worker
make test         # go test ./...
make demo         # run scripts/demo.sh
docker compose logs -f worker
```

### Where the moving parts live

```
cmd/notifd/              # one binary, two subcommands (`api`, `worker`)
internal/api/            # Fiber handlers, middleware, CORS, error mapping
internal/worker/         # Asynq consumer + dispatcher + reconciler + retry math
internal/channels/       # email (SMTP), slack (webhook), in-app (DB)
internal/templates/      # Go text/html template engine + parse cache + validation
internal/store/          # pgx-based data access (no ORM)
internal/queue/          # Asynq enqueuer facade
internal/ratelimit/      # Redis fixed-window limiter per (user, channel)
internal/idempotency/    # Stripe-style Idempotency-Key cache
internal/observability/  # zerolog + Prometheus metrics
internal/config/         # 12-factor env loading
internal/apperr/         # typed errors for transient/permanent classification
migrations/              # golang-migrate SQL (5 files)
mocks/slack/             # small Go HTTP server that logs webhook POSTs
deploy/k8s/              # minimal K8s manifests with HPA sketch
scripts/demo.sh          # live walk-through of every feature (curl)
bruno/notifd/            # Bruno API collection — same walkthrough via UI
tests/e2e/               # opt-in HTTP integration tests
web/in-app-demo.html     # zero-build static demo of the in-app feed
```

---

## See also

- **[DESIGN.md](DESIGN.md)** — full RFC: requirements, data model, API contract, retry/DLQ/idempotency strategy, reconciler, rate limiting, observability, security, scale + deployment (capacity estimate, per-tier scaling, graduation path to Kafka, K8s sketch, SLOs), trade-offs, and "what I'd do with more time".
