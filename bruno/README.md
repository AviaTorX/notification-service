# Bruno collection for notifd

A [Bruno](https://www.usebruno.com/) collection that walks through every
notable API path. Plain-text `.bru` files — commit-tracked, reviewable,
and importable into Bruno with zero setup.

## Opening it

1. Install Bruno (https://www.usebruno.com/downloads).
2. In Bruno: **Collections → Open Collection**.
3. Point it at `bruno/notifd/` in this repo.
4. Top-right environment dropdown → pick **Local**.

The environment sets:

| Var           | Default                                      | Notes                                    |
|---------------|----------------------------------------------|------------------------------------------|
| `baseUrl`     | `http://localhost:8080`                      | API origin                               |
| `apiKey`      | `dev-api-key-change-me`                      | Matches `.env.example`                   |
| `demoUser`    | `00000000-0000-0000-0000-000000000001`       | Seeded by migration 0001                 |
| `notificationId` | *(captured at runtime)*                   | Set by Notifications/01                  |
| `scheduledNotificationId` | *(captured at runtime)*           | Set by Notifications/05                  |
| `aliceUserId` | *(captured at runtime)*                      | Set by Users/01                          |
| `createdTemplateId` | *(captured at runtime)*                | Set by Templates/02                      |
| `inAppItemId` | *(captured at runtime)*                      | Set by Users/05                          |

## Running the demo in order

The folders are designed to be clicked top-to-bottom for a live walkthrough.

### 1. Health
Prove the stack is up. Open `/healthz`, `/readyz`, `/metrics`.

### 2. Notifications
The core flow. Nine requests:

1. **Create instant (welcome)** — 202 accepted, delivered in ~1s
2. **Idempotent replay (same key)** — same 202 with `Idempotent-Replay: true`
3. **Idempotency conflict (different body)** — 409
4. **Fetch by id** — status `sent`, per-channel deliveries visible
5. **Scheduled for +5s** — 202, `scheduled_for` echoed, status `queued`
6. **Fetch scheduled (after 5s)** — wait 5s between #5 and this one
7. **Slack-only (deploy_succeeded)** — routes to Slack + in-app, no email
8. **Missing required variable** — 400 with the missing name in the message
9. **List in-app feed** — cursor-paginated envelope

While you click, have these open in a browser:

- **Mailpit** `http://localhost:8025` — emails land here
- **Slack mock** `http://localhost:9000/received` — webhook POSTs captured
- **Asynqmon** `http://localhost:8090` — queue, scheduled, retry, DLQ

### 3. Templates
Five requests: list, create, reject-broken-HTML, fetch, delete-with-FK-protection.

### 4. Users
Six requests: upsert, list preferences, opt-out email, opt back in, list in-app feed, mark-as-read.

## Request features worth mentioning

- **Auto-captured IDs.** Post-response scripts stash IDs into environment
  variables so follow-up requests don't require copy-paste.
- **Tests on every request.** The `tests { }` block asserts status codes
  and response shape. Run the whole folder via Bruno's "Run Folder"
  action and watch every assertion pass.
- **Deterministic idempotency key** (`demo-welcome-001`) so the replay
  and conflict requests work immediately without coordination.
- **Pre-request scripts** compute `+5s` timestamps at request time so
  the scheduled-send request always points at the future.

## Running via CLI (CI-friendly)

Bruno has a CLI (`bru`) that runs collections headlessly:

```bash
npm install -g @usebruno/cli
cd bruno/notifd
bru run --env Local
```

Exit code reflects whether every test passed. Useful if you want to
gate CI on API contract tests.
