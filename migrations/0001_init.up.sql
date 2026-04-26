-- Users recognised by the service. Minimal on purpose; a real deployment would
-- integrate with an identity provider. `email` / `slack_user_id` can be null so
-- a user can exist without opting into a given channel.
CREATE TABLE users (
    id              UUID PRIMARY KEY,
    email           TEXT,
    slack_user_id   TEXT,
    slack_webhook   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX users_email_uk        ON users (LOWER(email))      WHERE email IS NOT NULL;
CREATE UNIQUE INDEX users_slack_user_uk   ON users (slack_user_id)     WHERE slack_user_id IS NOT NULL;

-- Per-user opt-in / opt-out for each channel. Missing row = channel enabled
-- (safe default for non-marketing traffic). Explicit row with enabled=false
-- is an opt-out.
CREATE TABLE user_channel_preferences (
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel     TEXT        NOT NULL CHECK (channel IN ('email','slack','in_app')),
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, channel)
);

-- Named, reusable template. `required_variables` is a declaration used at
-- send-time to reject requests missing inputs (fail fast, before enqueue).
-- `default_channels` is applied when the create-notification request does
-- not specify channels explicitly.
CREATE TABLE templates (
    id                  UUID PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT,
    subject             TEXT,                   -- rendered as Go text/template (email only)
    body_text           TEXT NOT NULL,          -- rendered as Go text/template
    body_html           TEXT,                   -- rendered as Go html/template (email only)
    default_channels    TEXT[] NOT NULL DEFAULT '{}',
    required_variables  TEXT[] NOT NULL DEFAULT '{}',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Each create-notification request becomes one row. `idempotency_key` is the
-- client-supplied token scoped per API caller; a repeated request with the
-- same key returns the stored notification rather than creating a new one.
-- `status` is the parent-level rollup; per-channel status lives in
-- notification_deliveries.
CREATE TABLE notifications (
    id                  UUID PRIMARY KEY,
    idempotency_key     TEXT,
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    template_id         UUID REFERENCES templates(id) ON DELETE SET NULL,
    channels            TEXT[] NOT NULL,                -- resolved channel list
    variables           JSONB NOT NULL DEFAULT '{}'::jsonb,
    scheduled_for       TIMESTAMPTZ,                    -- null == send now
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending','queued','processing','sent','partially_sent','failed','dlq','cancelled')),
    attempts            INT NOT NULL DEFAULT 0,
    last_error          TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at             TIMESTAMPTZ
);
CREATE UNIQUE INDEX notifications_idem_uk ON notifications (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX notifications_user_created_idx ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_scheduled_idx ON notifications (scheduled_for) WHERE scheduled_for IS NOT NULL AND status IN ('pending','queued');

-- One row per (notification, channel). The worker updates the channel-level
-- row as it delivers, and rolls up the parent notification status once all
-- channels are terminal.
CREATE TABLE notification_deliveries (
    id               UUID PRIMARY KEY,
    notification_id  UUID NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    channel          TEXT NOT NULL CHECK (channel IN ('email','slack','in_app')),
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','sent','failed','skipped')),
    attempts         INT NOT NULL DEFAULT 0,
    provider_id      TEXT,
    error            TEXT,
    sent_at          TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (notification_id, channel)
);
CREATE INDEX notification_deliveries_status_idx ON notification_deliveries (status);

-- In-app notifications are stored here so the user's client can pull them.
-- Kept separate from `notifications` because (a) many notifications span
-- multiple channels, (b) in-app items have their own read/unread lifecycle,
-- (c) we may eventually prune old rows on a different retention policy.
CREATE TABLE in_app_notifications (
    id              UUID PRIMARY KEY,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notification_id UUID REFERENCES notifications(id) ON DELETE SET NULL,
    title           TEXT NOT NULL,
    body            TEXT NOT NULL,
    data            JSONB NOT NULL DEFAULT '{}'::jsonb,
    read_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX in_app_user_unread_idx ON in_app_notifications (user_id, created_at DESC) WHERE read_at IS NULL;
CREATE INDEX in_app_user_all_idx    ON in_app_notifications (user_id, created_at DESC);

-- Seed one user + three starter templates so reviewers can curl something
-- interesting the moment the stack is up.
INSERT INTO users (id, email, slack_user_id, slack_webhook)
VALUES ('00000000-0000-0000-0000-000000000001',
        'demo@example.com',
        'U_DEMO',
        'http://slack-mock:9000/webhook');

INSERT INTO templates (id, name, description, subject, body_text, body_html, default_channels, required_variables)
VALUES
  ('10000000-0000-0000-0000-000000000001',
   'welcome',
   'Sent when a user signs up.',
   'Welcome to {{.product}}, {{.name}}!',
   'Hi {{.name}}, welcome to {{.product}}. Let us know if you need anything.',
   '<p>Hi <b>{{.name}}</b>, welcome to <b>{{.product}}</b>.</p>',
   ARRAY['email','in_app'],
   ARRAY['name','product']),
  ('10000000-0000-0000-0000-000000000002',
   'password_reset',
   'Password reset email.',
   'Reset your password',
   'Use this code to reset your password: {{.code}}. It expires in {{.ttl_minutes}} minutes.',
   '<p>Use this code to reset your password: <code>{{.code}}</code>. It expires in {{.ttl_minutes}} minutes.</p>',
   ARRAY['email'],
   ARRAY['code','ttl_minutes']),
  ('10000000-0000-0000-0000-000000000003',
   'deploy_succeeded',
   'Notify a Slack channel when a deploy finishes.',
   NULL,
   ':rocket: Deploy {{.deploy_id}} of *{{.service}}* succeeded in {{.duration}}.',
   NULL,
   ARRAY['slack','in_app'],
   ARRAY['deploy_id','service','duration']);
