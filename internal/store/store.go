// Package store is the data-access layer. Uses pgx/v5 directly — no ORM,
// no reflection, explicit SQL. The `Store` struct bundles the pool and
// exposes one method per use case so handlers and the worker talk to the
// database through a single well-defined surface.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
}

// PoolOptions controls the pgx connection pool size. Zero values fall
// back to a conservative default so callers that don't care get sensible
// behaviour; production callers should plumb env values through.
type PoolOptions struct {
	MaxConns int32
	MinConns int32
}

func New(ctx context.Context, dsn string, opts PoolOptions) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	} else {
		cfg.MaxConns = 20
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	} else {
		cfg.MinConns = 2
	}
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() {
	if s != nil && s.Pool != nil {
		s.Pool.Close()
	}
}

// ---------- Domain types ----------

// ValidChannel gates channel names at the API boundary. The Postgres
// CHECK constraint is the source of truth; this function must match it.
// If you add a channel, update both the CHECK and this function
// together — there is no compile-time coupling between them.
func ValidChannel(c string) bool {
	switch c {
	case "email", "slack", "in_app":
		return true
	}
	return false
}

type User struct {
	ID           uuid.UUID
	Email        *string
	SlackUserID  *string
	SlackWebhook *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Template struct {
	ID                uuid.UUID
	Name              string
	Description       *string
	Subject           *string
	BodyText          string
	BodyHTML          *string
	DefaultChannels   []string
	RequiredVariables []string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type Notification struct {
	ID                         uuid.UUID
	IdempotencyKey             *string
	UserID                     uuid.UUID
	TemplateID                 *uuid.UUID
	Channels                   []string
	Variables                  map[string]any
	ScheduledFor               *time.Time
	Status                     string
	Attempts                   int
	LastError                  *string
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
	SentAt                     *time.Time
	SubjectSnapshot            *string
	BodyTextSnapshot           *string
	BodyHTMLSnapshot           *string
	TemplateNameSnapshot       *string
	RequiredVariablesSnapshot  []string
}

type Delivery struct {
	ID             uuid.UUID
	NotificationID uuid.UUID
	Channel        string
	Status         string
	Attempts       int
	ProviderID     *string
	Error          *string
	SentAt         *time.Time
	UpdatedAt      time.Time
}

type ChannelPreference struct {
	UserID    uuid.UUID
	Channel   string
	Enabled   bool
	UpdatedAt time.Time
}

type InAppNotification struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	NotificationID *uuid.UUID
	Title          string
	Body           string
	Data           map[string]any
	ReadAt         *time.Time
	CreatedAt      time.Time
}

var (
	ErrNotFound        = errors.New("not found")
	ErrStillReferenced = errors.New("row is still referenced by other rows")
)

// ---------- Users ----------

func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	const q = `SELECT id, email, slack_user_id, slack_webhook, created_at, updated_at FROM users WHERE id = $1`
	row := s.Pool.QueryRow(ctx, q, id)
	var u User
	if err := row.Scan(&u.ID, &u.Email, &u.SlackUserID, &u.SlackWebhook, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

type UpsertUserParams struct {
	ID           uuid.UUID
	Email        *string
	SlackUserID  *string
	SlackWebhook *string
}

func (s *Store) UpsertUser(ctx context.Context, p UpsertUserParams) (*User, error) {
	const q = `
		INSERT INTO users (id, email, slack_user_id, slack_webhook)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		   SET email = EXCLUDED.email,
		       slack_user_id = EXCLUDED.slack_user_id,
		       slack_webhook = EXCLUDED.slack_webhook,
		       updated_at = NOW()
		RETURNING id, email, slack_user_id, slack_webhook, created_at, updated_at`
	var u User
	err := s.Pool.QueryRow(ctx, q, p.ID, p.Email, p.SlackUserID, p.SlackWebhook).
		Scan(&u.ID, &u.Email, &u.SlackUserID, &u.SlackWebhook, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ---------- Preferences ----------

func (s *Store) ListPreferences(ctx context.Context, userID uuid.UUID) ([]ChannelPreference, error) {
	const q = `SELECT user_id, channel, enabled, updated_at FROM user_channel_preferences WHERE user_id = $1`
	rows, err := s.Pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelPreference{}
	for rows.Next() {
		var p ChannelPreference
		if err := rows.Scan(&p.UserID, &p.Channel, &p.Enabled, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpsertPreference(ctx context.Context, userID uuid.UUID, channel string, enabled bool) error {
	const q = `
		INSERT INTO user_channel_preferences (user_id, channel, enabled)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, channel) DO UPDATE
		   SET enabled = EXCLUDED.enabled, updated_at = NOW()`
	_, err := s.Pool.Exec(ctx, q, userID, channel, enabled)
	return err
}

// EnabledChannels returns the set of channels the user has NOT opted out of.
// Missing preference rows are treated as enabled.
func (s *Store) EnabledChannels(ctx context.Context, userID uuid.UUID, candidate []string) ([]string, error) {
	prefs, err := s.ListPreferences(ctx, userID)
	if err != nil {
		return nil, err
	}
	disabled := map[string]struct{}{}
	for _, p := range prefs {
		if !p.Enabled {
			disabled[p.Channel] = struct{}{}
		}
	}
	out := make([]string, 0, len(candidate))
	for _, c := range candidate {
		if _, off := disabled[c]; off {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// ---------- Templates ----------

func (s *Store) GetTemplate(ctx context.Context, id uuid.UUID) (*Template, error) {
	const q = `SELECT id, name, description, subject, body_text, body_html, default_channels, required_variables, created_at, updated_at
	           FROM templates WHERE id = $1`
	return s.scanTemplateRow(s.Pool.QueryRow(ctx, q, id))
}

func (s *Store) GetTemplateByName(ctx context.Context, name string) (*Template, error) {
	const q = `SELECT id, name, description, subject, body_text, body_html, default_channels, required_variables, created_at, updated_at
	           FROM templates WHERE name = $1`
	return s.scanTemplateRow(s.Pool.QueryRow(ctx, q, name))
}

func (s *Store) ListTemplates(ctx context.Context) ([]Template, error) {
	const q = `SELECT id, name, description, subject, body_text, body_html, default_channels, required_variables, created_at, updated_at
	           FROM templates ORDER BY name`
	rows, err := s.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		t, err := s.scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

type row interface {
	Scan(dest ...any) error
}

func (s *Store) scanTemplateRow(r row) (*Template, error) {
	var t Template
	err := r.Scan(&t.ID, &t.Name, &t.Description, &t.Subject, &t.BodyText, &t.BodyHTML,
		&t.DefaultChannels, &t.RequiredVariables, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (s *Store) scanTemplate(rows pgx.Rows) (*Template, error) {
	var t Template
	if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.Subject, &t.BodyText, &t.BodyHTML,
		&t.DefaultChannels, &t.RequiredVariables, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

type UpsertTemplateParams struct {
	ID                uuid.UUID
	Name              string
	Description       *string
	Subject           *string
	BodyText          string
	BodyHTML          *string
	DefaultChannels   []string
	RequiredVariables []string
}

func (s *Store) UpsertTemplate(ctx context.Context, p UpsertTemplateParams) (*Template, error) {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.DefaultChannels == nil {
		p.DefaultChannels = []string{}
	}
	if p.RequiredVariables == nil {
		p.RequiredVariables = []string{}
	}
	const q = `
		INSERT INTO templates (id, name, description, subject, body_text, body_html, default_channels, required_variables)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (name) DO UPDATE
		   SET description = EXCLUDED.description,
		       subject = EXCLUDED.subject,
		       body_text = EXCLUDED.body_text,
		       body_html = EXCLUDED.body_html,
		       default_channels = EXCLUDED.default_channels,
		       required_variables = EXCLUDED.required_variables,
		       updated_at = NOW()
		RETURNING id, name, description, subject, body_text, body_html, default_channels, required_variables, created_at, updated_at`
	return s.scanTemplateRow(s.Pool.QueryRow(ctx, q,
		p.ID, p.Name, p.Description, p.Subject, p.BodyText, p.BodyHTML, p.DefaultChannels, p.RequiredVariables))
}

func (s *Store) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	cmd, err := s.Pool.Exec(ctx, `DELETE FROM templates WHERE id = $1`, id)
	if err != nil {
		// Postgres 23503 = foreign_key_violation. With ON DELETE
		// RESTRICT on notifications.template_id, this fires whenever a
		// template is still referenced by pending or historical
		// notifications. Collapse to a typed sentinel so the API
		// surfaces a clean 409 Conflict rather than a bare 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ErrStillReferenced
		}
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- Notifications ----------

// ErrIdempotencyKeyReused is returned by CreateNotification when a row with
// the same idempotency_key already exists. Callers should fetch the existing
// row via GetNotificationByIdempotencyKey and treat the result as a replay.
var ErrIdempotencyKeyReused = errors.New("idempotency key already used")

type CreateNotificationParams struct {
	ID                        uuid.UUID
	IdempotencyKey            *string
	UserID                    uuid.UUID
	TemplateID                *uuid.UUID
	Channels                  []string
	Variables                 map[string]any
	ScheduledFor              *time.Time
	SubjectSnapshot           *string
	BodyTextSnapshot          *string
	BodyHTMLSnapshot          *string
	TemplateNameSnapshot      *string
	RequiredVariablesSnapshot []string
}

const notificationColumns = `id, idempotency_key, user_id, template_id, channels, variables, scheduled_for,
		          status, attempts, last_error, created_at, updated_at, sent_at,
		          subject_snapshot, body_text_snapshot, body_html_snapshot,
		          template_name_snapshot, required_variables_snapshot`

func (s *Store) CreateNotification(ctx context.Context, p CreateNotificationParams) (*Notification, error) {
	if p.Variables == nil {
		p.Variables = map[string]any{}
	}
	if p.RequiredVariablesSnapshot == nil {
		// Column is NOT NULL — pgx passes literal NULL for a nil []string,
		// which trips the constraint. Empty slice is the right default.
		p.RequiredVariablesSnapshot = []string{}
	}
	vars, err := json.Marshal(p.Variables)
	if err != nil {
		return nil, fmt.Errorf("marshal variables: %w", err)
	}
	const q = `
		INSERT INTO notifications
		  (id, idempotency_key, user_id, template_id, channels, variables, scheduled_for, status,
		   subject_snapshot, body_text_snapshot, body_html_snapshot,
		   template_name_snapshot, required_variables_snapshot)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, 'queued', $8, $9, $10, $11, $12)
		RETURNING ` + notificationColumns
	n, err := s.scanNotificationRow(s.Pool.QueryRow(ctx, q,
		p.ID, p.IdempotencyKey, p.UserID, p.TemplateID, p.Channels, vars, p.ScheduledFor,
		p.SubjectSnapshot, p.BodyTextSnapshot, p.BodyHTMLSnapshot,
		p.TemplateNameSnapshot, p.RequiredVariablesSnapshot))
	if err != nil {
		// Postgres 23505 = unique_violation. Collapse it to a sentinel so
		// callers can surface a replay instead of a 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && p.IdempotencyKey != nil {
			return nil, ErrIdempotencyKeyReused
		}
		return nil, err
	}
	return n, nil
}

func (s *Store) GetNotification(ctx context.Context, id uuid.UUID) (*Notification, error) {
	const q = `SELECT ` + notificationColumns + ` FROM notifications WHERE id = $1`
	return s.scanNotificationRow(s.Pool.QueryRow(ctx, q, id))
}

func (s *Store) GetNotificationByIdempotencyKey(ctx context.Context, key string) (*Notification, error) {
	const q = `SELECT ` + notificationColumns + ` FROM notifications WHERE idempotency_key = $1`
	return s.scanNotificationRow(s.Pool.QueryRow(ctx, q, key))
}

// StuckQueuedParams parameterises the reconciler scan. `After` is a
// keyset cursor on (updated_at, id) so a single large outage window can
// be drained across multiple pages without re-scanning the leading rows.
type StuckQueuedParams struct {
	OlderThan time.Duration
	Limit     int
	After     StuckCursor
}

// StuckCursor is the keyset cursor for ListStuckQueued. Zero value means
// "start from the oldest matching row".
type StuckCursor struct {
	UpdatedAt time.Time
	ID        uuid.UUID
}

func (c StuckCursor) IsZero() bool { return c.UpdatedAt.IsZero() && c.ID == uuid.Nil }

// ListStuckQueued returns notifications stuck in 'queued' (or 'processing')
// older than `OlderThan`, paginated via a compound (updated_at, id) keyset
// cursor. Consumed by the worker-side reconciler to recover rows that
// were committed to Postgres but never enqueued (Redis outage mid-
// request) or whose worker crashed between claim and status update.
//
// `scheduled_for` filter: a row intentionally scheduled for the future
// is NOT stuck. Without this guard, every future-scheduled notification
// matches the 30s-stale window and gets re-enqueued on every tick — Asynq
// dedupes against the live scheduled-queue entry, so each re-enqueue
// returns an `ErrTaskIDConflict` that the reconciler would count as
// failure. We include rows whose scheduled_for is within one stuck-
// window of now so a row that SHOULD have fired but didn't (missed
// enqueue) is still recovered promptly.
//
// Partial index `notifications_stuck_idx` (migration 0004) backs this
// query so the scan stays O(stuck_count) rather than O(total_count).
func (s *Store) ListStuckQueued(ctx context.Context, p StuckQueuedParams) ([]Notification, StuckCursor, error) {
	if p.Limit <= 0 {
		p.Limit = 1000
	}
	args := []any{fmt.Sprintf("%d seconds", int(p.OlderThan.Seconds()))}
	q := `
		SELECT ` + notificationColumns + `
		FROM notifications
		WHERE status IN ('queued','processing')
		  AND updated_at < NOW() - $1::interval
		  AND (scheduled_for IS NULL OR scheduled_for <= NOW() + $1::interval)`
	if !p.After.IsZero() {
		args = append(args, p.After.UpdatedAt, p.After.ID)
		q += fmt.Sprintf(` AND (updated_at, id) > ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, p.Limit+1)
	q += fmt.Sprintf(` ORDER BY updated_at, id LIMIT $%d`, len(args))

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, StuckCursor{}, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		n, err := scanNotificationRows(rows)
		if err != nil {
			return nil, StuckCursor{}, err
		}
		out = append(out, *n)
	}
	if err := rows.Err(); err != nil {
		return nil, StuckCursor{}, err
	}
	var next StuckCursor
	if len(out) > p.Limit {
		last := out[p.Limit-1]
		next = StuckCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
		out = out[:p.Limit]
	}
	return out, next, nil
}

func (s *Store) scanNotificationRow(r row) (*Notification, error) {
	var n Notification
	var raw []byte
	err := r.Scan(&n.ID, &n.IdempotencyKey, &n.UserID, &n.TemplateID, &n.Channels, &raw, &n.ScheduledFor,
		&n.Status, &n.Attempts, &n.LastError, &n.CreatedAt, &n.UpdatedAt, &n.SentAt,
		&n.SubjectSnapshot, &n.BodyTextSnapshot, &n.BodyHTMLSnapshot,
		&n.TemplateNameSnapshot, &n.RequiredVariablesSnapshot)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &n.Variables); err != nil {
			return nil, fmt.Errorf("decode variables: %w", err)
		}
	} else {
		n.Variables = map[string]any{}
	}
	return &n, nil
}

func scanNotificationRows(rows pgx.Rows) (*Notification, error) {
	var n Notification
	var raw []byte
	if err := rows.Scan(&n.ID, &n.IdempotencyKey, &n.UserID, &n.TemplateID, &n.Channels, &raw, &n.ScheduledFor,
		&n.Status, &n.Attempts, &n.LastError, &n.CreatedAt, &n.UpdatedAt, &n.SentAt,
		&n.SubjectSnapshot, &n.BodyTextSnapshot, &n.BodyHTMLSnapshot,
		&n.TemplateNameSnapshot, &n.RequiredVariablesSnapshot); err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &n.Variables); err != nil {
			return nil, fmt.Errorf("decode variables: %w", err)
		}
	} else {
		n.Variables = map[string]any{}
	}
	return &n, nil
}

func (s *Store) UpdateNotificationStatus(ctx context.Context, id uuid.UUID, status string, lastErr *string) error {
	const q = `
		UPDATE notifications
		   SET status = $2, last_error = $3, attempts = attempts + 1, updated_at = NOW(),
		       sent_at = CASE WHEN $2 = 'sent' OR $2 = 'partially_sent' THEN NOW() ELSE sent_at END
		 WHERE id = $1`
	_, err := s.Pool.Exec(ctx, q, id, status, lastErr)
	return err
}

// ---------- Deliveries ----------

func (s *Store) EnsureDelivery(ctx context.Context, notificationID uuid.UUID, channel string) (*Delivery, error) {
	const q = `
		INSERT INTO notification_deliveries (id, notification_id, channel)
		VALUES ($1, $2, $3)
		ON CONFLICT (notification_id, channel) DO UPDATE SET updated_at = NOW()
		RETURNING id, notification_id, channel, status, attempts, provider_id, error, sent_at, updated_at`
	return s.scanDelivery(s.Pool.QueryRow(ctx, q, uuid.New(), notificationID, channel))
}

func (s *Store) UpdateDelivery(ctx context.Context, notificationID uuid.UUID, channel, status string, providerID *string, errMsg *string) error {
	const q = `
		UPDATE notification_deliveries
		   SET status = $3, attempts = attempts + 1, provider_id = COALESCE($4, provider_id),
		       error = $5,
		       sent_at = CASE WHEN $3 = 'sent' THEN NOW() ELSE sent_at END,
		       updated_at = NOW()
		 WHERE notification_id = $1 AND channel = $2`
	_, err := s.Pool.Exec(ctx, q, notificationID, channel, status, providerID, errMsg)
	return err
}

func (s *Store) ListDeliveries(ctx context.Context, notificationID uuid.UUID) ([]Delivery, error) {
	const q = `
		SELECT id, notification_id, channel, status, attempts, provider_id, error, sent_at, updated_at
		FROM notification_deliveries WHERE notification_id = $1 ORDER BY channel`
	rows, err := s.Pool.Query(ctx, q, notificationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.NotificationID, &d.Channel, &d.Status, &d.Attempts,
			&d.ProviderID, &d.Error, &d.SentAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) scanDelivery(r row) (*Delivery, error) {
	var d Delivery
	err := r.Scan(&d.ID, &d.NotificationID, &d.Channel, &d.Status, &d.Attempts,
		&d.ProviderID, &d.Error, &d.SentAt, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

// ---------- In-app notifications ----------

type CreateInAppParams struct {
	UserID         uuid.UUID
	NotificationID *uuid.UUID
	Title          string
	Body           string
	Data           map[string]any
}

func (s *Store) CreateInApp(ctx context.Context, p CreateInAppParams) (*InAppNotification, error) {
	if p.Data == nil {
		p.Data = map[string]any{}
	}
	data, err := json.Marshal(p.Data)
	if err != nil {
		return nil, err
	}
	const q = `
		INSERT INTO in_app_notifications (id, user_id, notification_id, title, body, data)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		RETURNING id, user_id, notification_id, title, body, data, read_at, created_at`
	row := s.Pool.QueryRow(ctx, q, uuid.New(), p.UserID, p.NotificationID, p.Title, p.Body, data)
	var n InAppNotification
	var raw []byte
	if err := row.Scan(&n.ID, &n.UserID, &n.NotificationID, &n.Title, &n.Body, &raw, &n.ReadAt, &n.CreatedAt); err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &n.Data); err != nil {
			return nil, fmt.Errorf("decode in-app data: %w", err)
		}
	}
	return &n, nil
}

// InAppCursor is a compound keyset cursor: pagination proceeds by strictly
// descending (CreatedAt, ID). A plain `created_at < before` cursor would
// silently skip rows sharing a timestamp during a burst of concurrent
// inserts; the ID tie-break eliminates that whole class of bug.
type InAppCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func (c InAppCursor) IsZero() bool { return c.CreatedAt.IsZero() && c.ID == uuid.Nil }

type ListInAppParams struct {
	UserID     uuid.UUID
	UnreadOnly bool
	Limit      int
	// Before is the compound keyset cursor; zero value means "start from
	// the newest row".
	Before InAppCursor
}

// ListInAppPage returns (items, nextCursor). nextCursor is set to the
// (created_at, id) of the last returned row when more rows exist beyond
// it, otherwise an InAppCursor zero value. Clients paginate by threading
// nextCursor back in as Before.
func (s *Store) ListInAppPage(ctx context.Context, p ListInAppParams) ([]InAppNotification, InAppCursor, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	args := []any{p.UserID}
	q := `
		SELECT id, user_id, notification_id, title, body, data, read_at, created_at
		FROM in_app_notifications
		WHERE user_id = $1`
	if p.UnreadOnly {
		q += ` AND read_at IS NULL`
	}
	if !p.Before.IsZero() {
		// Lexicographic tuple compare does the right thing: the next row
		// must be either strictly older, OR same created_at but a smaller
		// id. Equivalent to `(created_at < t) OR (created_at = t AND id < i)`.
		args = append(args, p.Before.CreatedAt, p.Before.ID)
		q += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	// Fetch limit+1 to detect whether a next page exists without a count().
	args = append(args, p.Limit+1)
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, InAppCursor{}, err
	}
	defer rows.Close()
	out := make([]InAppNotification, 0, p.Limit)
	for rows.Next() {
		var n InAppNotification
		var raw []byte
		if err := rows.Scan(&n.ID, &n.UserID, &n.NotificationID, &n.Title, &n.Body, &raw, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, InAppCursor{}, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &n.Data); err != nil {
				return nil, InAppCursor{}, fmt.Errorf("decode in-app data: %w", err)
			}
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, InAppCursor{}, err
	}
	var next InAppCursor
	if len(out) > p.Limit {
		last := out[p.Limit-1]
		next = InAppCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		out = out[:p.Limit]
	}
	return out, next, nil
}

func (s *Store) MarkInAppRead(ctx context.Context, userID, id uuid.UUID) error {
	cmd, err := s.Pool.Exec(ctx,
		`UPDATE in_app_notifications SET read_at = NOW() WHERE id = $1 AND user_id = $2 AND read_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
