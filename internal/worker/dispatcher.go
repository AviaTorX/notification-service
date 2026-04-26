// Package worker consumes notification:send tasks from Asynq, renders the
// template, fans out to the channel adapters, records per-channel status,
// and rolls up the parent notification status.
//
// The dispatcher is written to be safely re-driven: if the same task is
// dequeued twice (duplicate enqueue, worker crash mid-flight, retry after a
// transient failure), it will not re-send channels that are already marked
// `sent` in notification_deliveries. That guarantee complements the
// outbox-style reconciler in reconciler.go which handles lost enqueues.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/avinash-shinde/notification-service/internal/apperr"
	"github.com/avinash-shinde/notification-service/internal/channels"
	"github.com/avinash-shinde/notification-service/internal/observability"
	"github.com/avinash-shinde/notification-service/internal/queue"
	"github.com/avinash-shinde/notification-service/internal/store"
	"github.com/avinash-shinde/notification-service/internal/templates"
)

// DispatcherStore is the narrow interface the dispatcher needs from the
// persistence layer. Kept as an interface so tests can provide an
// in-memory fake without standing up Postgres.
type DispatcherStore interface {
	GetNotification(ctx context.Context, id uuid.UUID) (*store.Notification, error)
	GetUser(ctx context.Context, id uuid.UUID) (*store.User, error)
	EnabledChannels(ctx context.Context, userID uuid.UUID, candidate []string) ([]string, error)
	UpdateNotificationStatus(ctx context.Context, id uuid.UUID, status string, lastErr *string) error
	EnsureDelivery(ctx context.Context, notificationID uuid.UUID, channel string) (*store.Delivery, error)
	UpdateDelivery(ctx context.Context, notificationID uuid.UUID, channel, status string, providerID, errMsg *string) error
	ListDeliveries(ctx context.Context, notificationID uuid.UUID) ([]store.Delivery, error)
}

// ChannelLookup is the narrow interface the dispatcher needs from the
// channel registry — kept minimal so tests can plug in stubs.
type ChannelLookup interface {
	Get(name string) (channels.Adapter, error)
}

type Dispatcher struct {
	Store    DispatcherStore
	Channels ChannelLookup
}

func NewDispatcher(s DispatcherStore, r ChannelLookup) *Dispatcher {
	return &Dispatcher{Store: s, Channels: r}
}

// Handle is the Asynq task handler.
//
// Terminal-state handling:
//   - `sent` / `cancelled` / `dlq` → no work to do, return nil.
//   - `partially_sent` is NOT terminal here because per-channel retries may
//     still land us back on this task; instead we recompute which channels
//     remain pending and retry only those.
//   - `queued` / `processing` → re-run and pick up whichever channels are
//     still unsent.
//
// Retry signalling to Asynq:
//   - Any remaining transient failure → plain error, Asynq retries with
//     exponential backoff + jitter.
//   - Only permanent failures → `asynq.SkipRetry`, moves to archived set.
//   - Everything delivered → nil.
func (d *Dispatcher) Handle(ctx context.Context, t *asynq.Task) error {
	var p queue.SendPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("%w: bad payload: %v", asynq.SkipRetry, err)
	}
	logger := log.With().Str("notification_id", p.NotificationID.String()).Logger()

	n, err := d.Store.GetNotification(ctx, p.NotificationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			logger.Warn().Msg("notification gone; skipping")
			return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
		}
		return fmt.Errorf("load notification: %w", err)
	}

	// Skip only truly terminal rows. `partially_sent` falls through so we
	// can recompute pending channels.
	switch n.Status {
	case "sent", "cancelled", "dlq":
		logger.Info().Str("status", n.Status).Msg("notification already terminal; skipping")
		return nil
	}

	noteWriteErr("update_status", n.ID, d.Store.UpdateNotificationStatus(ctx, n.ID, "processing", nil))

	user, err := d.Store.GetUser(ctx, n.UserID)
	if err != nil {
		return d.permanent(ctx, n.ID, fmt.Errorf("load user: %w", err))
	}

	// Resolve channel set: explicit request -> preference-filtered.
	candidate := n.Channels
	enabled, err := d.Store.EnabledChannels(ctx, n.UserID, candidate)
	if err != nil {
		return fmt.Errorf("load preferences: %w", err)
	}
	if len(enabled) == 0 {
		noteWriteErr("update_status", n.ID, d.Store.UpdateNotificationStatus(ctx, n.ID, "cancelled", ptr("no channels enabled for user")))
		logger.Warn().Msg("no channels after preference filter; cancelling")
		return nil
	}

	// Render from snapshot. The snapshot was frozen at create time so the
	// delivered message is deterministic even if the template was edited
	// or deleted since.
	rendered, err := d.render(n)
	if err != nil {
		observability.TemplateRenderErrors.WithLabelValues(snapshotName(n)).Inc()
		return d.permanent(ctx, n.ID, err)
	}

	// Skip channels already marked `sent` in notification_deliveries so
	// a retry of a partially-successful task doesn't duplicate sends.
	existing, err := d.Store.ListDeliveries(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("load deliveries: %w", err)
	}
	sentBefore := map[string]bool{}
	for _, dr := range existing {
		if dr.Status == "sent" {
			sentBefore[dr.Channel] = true
		}
	}

	work := make([]string, 0, len(enabled))
	for _, ch := range enabled {
		if !sentBefore[ch] {
			work = append(work, ch)
		}
	}
	alreadySent := len(enabled) - len(work)

	newlySent, failed := d.fanOut(ctx, logger, n, user, rendered, work)
	totalSent := alreadySent + newlySent

	// Roll up parent status.
	hasTransient := anyTransient(failed)
	joined := strings.Join(failed, "; ")
	switch {
	case totalSent == len(enabled):
		noteWriteErr("update_status", n.ID, d.Store.UpdateNotificationStatus(ctx, n.ID, "sent", nil))
		return nil
	case hasTransient:
		// At least one transient failure remains → keep retrying. We hold
		// the parent in `partially_sent` if some channels succeeded, or
		// `queued` if none did — either way Asynq gets a real error so it
		// retries with backoff. (B1: partially_sent is NOT the end state.)
		rollup := "queued"
		if totalSent > 0 {
			rollup = "partially_sent"
		}
		noteWriteErr("update_status", n.ID, d.Store.UpdateNotificationStatus(ctx, n.ID, rollup, ptr(joined)))
		return fmt.Errorf("transient failures remain: %s", joined)
	case totalSent > 0:
		// Some succeeded, the rest permanently failed — final state.
		noteWriteErr("update_status", n.ID, d.Store.UpdateNotificationStatus(ctx, n.ID, "partially_sent", ptr(joined)))
		return nil
	default:
		// All permanent failures, nothing sent → DLQ.
		noteWriteErr("update_status", n.ID, d.Store.UpdateNotificationStatus(ctx, n.ID, "dlq", ptr(joined)))
		return fmt.Errorf("%w: all permanent: %s", asynq.SkipRetry, joined)
	}
}

// render builds the content from the snapshot columns. Falls back to the
// ad-hoc path (pulling body from variables) only when no snapshot exists,
// which is the case for template-less notifications.
func (d *Dispatcher) render(n *store.Notification) (templates.Rendered, error) {
	if n.BodyTextSnapshot == nil {
		// Ad-hoc notifications: grab subject/body from variables.
		return templates.Render(templates.Spec{
			Name:     "ad_hoc",
			Subject:  strValue(n.Variables, "subject"),
			BodyText: strValue(n.Variables, "body_text"),
			BodyHTML: strValue(n.Variables, "body_html"),
		}, n.Variables)
	}
	spec := templates.Spec{
		Name:              snapshotName(n),
		BodyText:          *n.BodyTextSnapshot,
		RequiredVariables: n.RequiredVariablesSnapshot,
	}
	if n.SubjectSnapshot != nil {
		spec.Subject = *n.SubjectSnapshot
	}
	if n.BodyHTMLSnapshot != nil {
		spec.BodyHTML = *n.BodyHTMLSnapshot
	}
	return templates.Render(spec, n.Variables)
}

// fanOut delivers rendered content across `work` channels concurrently
// and returns (newlySent, failed). Per-channel latency stacks in
// parallel rather than sequentially — p99 = max(channel latencies), not
// sum — which matters when email (SMTP dial + TLS handshake) sits in
// the same notification as a fast in-app insert.
//
// Each channel's work is a goroutine; results land in a per-channel slot
// in a fixed-size slice so we don't touch shared state outside the slot.
// Database writes inside each goroutine are independent rows in
// notification_deliveries, so there's no contention. The parent
// notification status is rolled up by the caller after fan-out returns.
//
// `failed` entries are tagged "<channel>:T:<msg>" for transient and
// "<channel>:P:<msg>" for permanent so anyTransient() can classify the
// composite result.
func (d *Dispatcher) fanOut(
	ctx context.Context,
	logger zerolog.Logger,
	n *store.Notification,
	user *store.User,
	rendered templates.Rendered,
	work []string,
) (int, []string) {
	type outcome struct {
		sent bool
		tag  string // "P" or "T" for failures; empty on success
		msg  string // failure detail
	}
	outcomes := make([]outcome, len(work))

	var wg sync.WaitGroup
	for i, chName := range work {
		i, chName := i, chName
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i] = d.deliverOne(ctx, logger, n, user, rendered, chName)
		}()
	}
	wg.Wait()

	sent := 0
	failed := []string{}
	for i, o := range outcomes {
		if o.sent {
			sent++
			continue
		}
		failed = append(failed, fmt.Sprintf("%s:%s:%s", work[i], o.tag, o.msg))
	}
	return sent, failed
}

// deliverOne is the single-channel body of fanOut, split out so each
// goroutine has its own stack of locals.
func (d *Dispatcher) deliverOne(
	ctx context.Context,
	logger zerolog.Logger,
	n *store.Notification,
	user *store.User,
	rendered templates.Rendered,
	chName string,
) (out struct {
	sent bool
	tag  string
	msg  string
}) {
	adapter, err := d.Channels.Get(chName)
	if err != nil {
		out.tag, out.msg = "P", err.Error()
		return
	}
	if _, err := d.Store.EnsureDelivery(ctx, n.ID, chName); err != nil {
		noteWriteErr("ensure_delivery", n.ID, err)
	}

	msg := buildMessage(n, user, rendered, chName)
	start := time.Now()
	res, err := adapter.Send(ctx, msg)
	observability.ChannelSendDuration.WithLabelValues(chName).Observe(time.Since(start).Seconds())

	if err != nil {
		tag := "P"
		if apperr.IsRetryable(err) {
			tag = "T"
		}
		errMsg := err.Error()
		noteWriteErr("update_delivery", n.ID, d.Store.UpdateDelivery(ctx, n.ID, chName, "failed", nil, &errMsg))
		if tag == "P" {
			observability.NotificationsFailed.WithLabelValues(chName, "permanent").Inc()
		} else {
			observability.NotificationsFailed.WithLabelValues(chName, "transient").Inc()
		}
		logger.Warn().Err(err).Str("channel", chName).Msg("delivery failed")
		out.tag, out.msg = tag, errMsg
		return
	}
	pid := res.ProviderID
	noteWriteErr("update_delivery", n.ID, d.Store.UpdateDelivery(ctx, n.ID, chName, "sent", &pid, nil))
	observability.NotificationsDelivered.WithLabelValues(chName, "sent").Inc()
	logger.Info().Str("channel", chName).Str("provider_id", pid).Msg("delivered")
	out.sent = true
	return
}

func (d *Dispatcher) permanent(ctx context.Context, id uuid.UUID, err error) error {
	msg := err.Error()
	noteWriteErr("update_status", id, d.Store.UpdateNotificationStatus(ctx, id, "dlq", &msg))
	return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
}

// noteWriteErr records a best-effort dispatcher store write that failed.
// We deliberately do NOT block the task on these: the channel network
// call already succeeded (or has nothing left to retry), so returning
// the error to Asynq would trigger an unnecessary retry. The counter +
// WARN log ensure an operator sees drift even though the in-memory
// outcome of the task is correct.
//
// For status writes that ARE load-bearing (e.g. the 'cancelled' path
// where we return nil), a future revision could surface the error so
// Asynq retries — but at the cost of re-resolving channel preferences
// repeatedly, so prefer "log loudly, reconciler will pick it up" until
// a real incident forces otherwise.
func noteWriteErr(op string, id uuid.UUID, err error) {
	if err == nil {
		return
	}
	observability.StoreWriteErrors.WithLabelValues(op).Inc()
	log.Warn().Err(err).
		Str("op", op).
		Str("notification_id", id.String()).
		Msg("dispatcher store write failed; task continues, reconciler may re-drive")
}

func buildMessage(n *store.Notification, u *store.User, r templates.Rendered, channel string) channels.Message {
	m := channels.Message{
		UserID:         n.UserID.String(),
		NotificationID: n.ID.String(),
		Subject:        r.Subject,
		BodyText:       r.BodyText,
		BodyHTML:       r.BodyHTML,
		TemplateName:   snapshotName(n),
	}
	switch channel {
	case "email":
		if u.Email != nil {
			m.ToEmail = *u.Email
		}
	case "slack":
		if u.SlackWebhook != nil {
			m.ToSlackURL = *u.SlackWebhook
		}
	}
	return m
}

// ---- helpers ----

func ptr[T any](v T) *T { return &v }

func strValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func snapshotName(n *store.Notification) string {
	if n == nil || n.TemplateNameSnapshot == nil {
		return "ad_hoc"
	}
	return *n.TemplateNameSnapshot
}

func anyTransient(failed []string) bool {
	for _, f := range failed {
		if strings.Contains(f, ":T:") {
			return true
		}
	}
	return false
}
