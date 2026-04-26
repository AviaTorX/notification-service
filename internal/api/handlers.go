package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/avinash-shinde/notification-service/internal/apperr"
	"github.com/avinash-shinde/notification-service/internal/idempotency"
	"github.com/avinash-shinde/notification-service/internal/observability"
	"github.com/avinash-shinde/notification-service/internal/queue"
	"github.com/avinash-shinde/notification-service/internal/ratelimit"
	"github.com/avinash-shinde/notification-service/internal/store"
	"github.com/avinash-shinde/notification-service/internal/templates"
)

// ---- shared types ----

type Deps struct {
	Store       *store.Store
	Enqueuer    *queue.Enqueuer
	RateLimiter *ratelimit.Limiter
	Idempotency *idempotency.Cache
	APIKey      string
}

type createNotificationReq struct {
	UserID         string         `json:"user_id"`
	TemplateName   string         `json:"template_name,omitempty"`
	TemplateID     string         `json:"template_id,omitempty"`
	Channels       []string       `json:"channels,omitempty"`
	Variables      map[string]any `json:"variables,omitempty"`
	ScheduledFor   *time.Time     `json:"scheduled_for,omitempty"`
}

type notificationResp struct {
	ID             uuid.UUID         `json:"id"`
	Status         string            `json:"status"`
	UserID         uuid.UUID         `json:"user_id"`
	TemplateID     *uuid.UUID        `json:"template_id,omitempty"`
	Channels       []string          `json:"channels"`
	Variables      map[string]any    `json:"variables"`
	ScheduledFor   *time.Time        `json:"scheduled_for,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	Deliveries     []deliveryResp    `json:"deliveries,omitempty"`
}

type deliveryResp struct {
	Channel    string     `json:"channel"`
	Status     string     `json:"status"`
	Attempts   int        `json:"attempts"`
	ProviderID *string    `json:"provider_id,omitempty"`
	Error      *string    `json:"error,omitempty"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
}

// ---- create notification ----

// createNotification implements the full POST /v1/notifications flow.
//
// Idempotency is enforced in two layers:
//  1. **Fast path — Redis cache.** Same body hash → replay the cached
//     response. Different body hash → 409 Conflict.
//  2. **Source of truth — Postgres unique index on idempotency_key.** Two
//     concurrent requests that both miss the cache can still race into the
//     insert; the second one trips `23505 unique_violation`, which the
//     store surfaces as `ErrIdempotencyKeyReused`. We handle that by
//     re-fetching the existing row and returning it as a 202 replay.
//     This closes the race the reviewer flagged as Blocker B3.
func createNotification(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		body := c.Body()
		var req createNotificationReq
		if err := json.Unmarshal(body, &req); err != nil {
			return apperr.Invalid("invalid json body: " + err.Error())
		}
		userID, err := uuid.Parse(req.UserID)
		if err != nil {
			return apperr.Invalid("user_id must be a uuid")
		}

		idemKey := c.Get(headerIdempKey)
		requestHash := idempotency.HashRequest(body)
		route := "POST:/v1/notifications"

		// Fast path: replay from the Redis cache when possible.
		if idemKey != "" {
			cached, err := d.Idempotency.Get(c.Context(), d.APIKey, route, idemKey, requestHash)
			if err != nil {
				if errors.Is(err, idempotency.ErrConflict) {
					return apperr.Conflict("idempotency key reused with different request body")
				}
				return err
			}
			if cached != nil {
				return writeReplay(c, cached.Status, cached.Body)
			}
		}

		user, err := d.Store.GetUser(c.Context(), userID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apperr.NotFound("user not found")
			}
			return err
		}

		var tmpl *store.Template
		switch {
		case req.TemplateID != "":
			id, err := uuid.Parse(req.TemplateID)
			if err != nil {
				return apperr.Invalid("template_id must be a uuid")
			}
			tmpl, err = d.Store.GetTemplate(c.Context(), id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return apperr.NotFound("template not found")
				}
				return err
			}
		case req.TemplateName != "":
			tmpl, err = d.Store.GetTemplateByName(c.Context(), req.TemplateName)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return apperr.NotFound("template not found: " + req.TemplateName)
				}
				return err
			}
		}

		// Determine channels: explicit -> template default -> error.
		channels := req.Channels
		if len(channels) == 0 && tmpl != nil {
			channels = append(channels, tmpl.DefaultChannels...)
		}
		if len(channels) == 0 {
			return apperr.Invalid("no channels specified and template has no defaults")
		}
		for _, ch := range channels {
			if !store.ValidChannel(ch) {
				return apperr.Invalid("invalid channel: " + ch)
			}
		}

		// Validate template variables up-front.
		if tmpl != nil {
			if miss := templates.Validate(tmpl.RequiredVariables, req.Variables); len(miss) > 0 {
				return apperr.Invalid("missing required variables: " + strings.Join(miss, ","))
			}
		}

		// Rate limit per (user, channel).
		//
		// Policy: fail-OPEN on Redis errors. A Redis outage is already
		// a critical incident; converting it into a total API outage by
		// letting the limiter surface 500s is worse than permitting a
		// burst above the configured budget for the outage window.
		// Operators watch `rate_limit_backend_errors_total` to notice
		// that the limiter is degraded — distinct from legitimate
		// 429s, which bump `rate_limit_rejections_total`.
		for _, ch := range channels {
			allowed, _, err := d.RateLimiter.Allow(c.Context(), userID.String(), ch)
			if err != nil {
				observability.RateLimitBackendErrors.WithLabelValues(ch).Inc()
				log.Warn().Err(err).Str("channel", ch).Str("user_id", userID.String()).
					Msg("rate limiter backend error; failing open")
				continue
			}
			if !allowed {
				observability.RateLimitRejections.WithLabelValues(ch).Inc()
				return apperr.RateLimited("rate limit exceeded for channel " + ch)
			}
		}

		// Build params including template snapshot. Content is frozen at
		// create time so the delivered message is deterministic even if the
		// template is later edited or deleted.
		p := buildCreateParams(user.ID, tmpl, channels, req, idemKey)

		n, err := d.Store.CreateNotification(c.Context(), p)
		if err != nil {
			if errors.Is(err, store.ErrIdempotencyKeyReused) && idemKey != "" {
				// Another concurrent request with the same key won the
				// insert race. Fetch its row and replay.
				existing, ferr := d.Store.GetNotificationByIdempotencyKey(c.Context(), idemKey)
				if ferr != nil {
					return fmt.Errorf("idempotency recovery fetch: %w", ferr)
				}
				return writeReplayFromRow(c, existing)
			}
			return err
		}
		observability.NotificationsCreated.WithLabelValues(templateNameOr(tmpl, "ad_hoc")).Inc()

		// Enqueue. Failure here is tolerated: the worker-side reconciler
		// (`worker.Reconciler`) will pick up any row stuck at `queued` for
		// longer than its scan interval and re-enqueue it. We still log +
		// count so operators notice sustained enqueue failure.
		var at time.Time
		if p.ScheduledFor != nil {
			at = *p.ScheduledFor
		}
		if _, eerr := d.Enqueuer.EnqueueSend(queue.SendPayload{NotificationID: n.ID}, queue.EnqueueOptions{
			ProcessAt: at,
		}); eerr != nil {
			observability.EnqueueFailures.WithLabelValues("api").Inc()
			log.Warn().Err(eerr).Str("notification_id", n.ID.String()).
				Msg("enqueue failed; reconciler will retry")
			// Do NOT return an error to the caller: the row is durably
			// persisted with status=queued and will be picked up by the
			// reconciler. Returning 500 here would let the caller think the
			// work was lost, when it wasn't.
		}

		resp := renderNotification(n, nil)
		raw, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		status := fiber.StatusAccepted
		if idemKey != "" {
			if err := d.Idempotency.Set(c.Context(), d.APIKey, route, idemKey, requestHash, status, raw); err != nil {
				// Correctness is preserved by the Postgres partial-unique
				// index on idempotency_key — a subsequent retry with the
				// same key will trip 23505 and hit the replay path. But
				// the Redis cache is the fast-path; degradation should be
				// visible to operators rather than silent.
				observability.IdempotencyCacheWriteErrors.Inc()
				log.Warn().Err(err).
					Str("request_id", rid(c)).
					Str("idempotency_key", idemKey).
					Msg("idempotency cache write failed; PG unique index remains source of truth")
			}
		}
		c.Status(status)
		c.Set("Content-Type", "application/json")
		return c.Send(raw)
	}
}

// buildCreateParams assembles the persistence params including the template
// snapshot (frozen at request time).
func buildCreateParams(userID uuid.UUID, tmpl *store.Template, channels []string, req createNotificationReq, idemKey string) store.CreateNotificationParams {
	p := store.CreateNotificationParams{
		ID:        uuid.New(),
		UserID:    userID,
		Channels:  channels,
		Variables: req.Variables,
	}
	if idemKey != "" {
		k := idemKey
		p.IdempotencyKey = &k
	}
	if req.ScheduledFor != nil {
		p.ScheduledFor = req.ScheduledFor
	}
	if tmpl != nil {
		tid := tmpl.ID
		p.TemplateID = &tid
		// Copy snapshot strings instead of taking &tmpl.Name etc. The
		// CreateNotification call is on the same goroutine as tmpl's
		// load, so aliasing happens to be safe today — but a future
		// engineer swapping the template fetcher for a shared cache
		// would turn aliasing into a cross-request mutation bug. The
		// cost of copying is four strings; the cost of aliasing is a
		// race condition that bisects to a helper.
		name := tmpl.Name
		body := tmpl.BodyText
		p.TemplateNameSnapshot = &name
		p.BodyTextSnapshot = &body
		if tmpl.Subject != nil {
			subj := *tmpl.Subject
			p.SubjectSnapshot = &subj
		}
		if tmpl.BodyHTML != nil {
			html := *tmpl.BodyHTML
			p.BodyHTMLSnapshot = &html
		}
		if len(tmpl.RequiredVariables) > 0 {
			rv := make([]string, len(tmpl.RequiredVariables))
			copy(rv, tmpl.RequiredVariables)
			p.RequiredVariablesSnapshot = rv
		}
	}
	return p
}

func writeReplay(c *fiber.Ctx, status int, body []byte) error {
	c.Status(status)
	c.Set("Content-Type", "application/json")
	c.Set("Idempotent-Replay", "true")
	return c.Send(body)
}

func writeReplayFromRow(c *fiber.Ctx, n *store.Notification) error {
	raw, err := json.Marshal(renderNotification(n, nil))
	if err != nil {
		return err
	}
	return writeReplay(c, fiber.StatusAccepted, raw)
}

func getNotification(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return apperr.Invalid("id must be a uuid")
		}
		n, err := d.Store.GetNotification(c.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apperr.NotFound("notification not found")
			}
			return err
		}
		deliveries, err := d.Store.ListDeliveries(c.Context(), n.ID)
		if err != nil {
			return err
		}
		return c.JSON(renderNotification(n, deliveries))
	}
}

// ---- templates CRUD ----

type templateReq struct {
	Name              string   `json:"name"`
	Description       *string  `json:"description,omitempty"`
	Subject           *string  `json:"subject,omitempty"`
	BodyText          string   `json:"body_text"`
	BodyHTML          *string  `json:"body_html,omitempty"`
	DefaultChannels   []string `json:"default_channels,omitempty"`
	RequiredVariables []string `json:"required_variables,omitempty"`
}

func listTemplates(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ts, err := d.Store.ListTemplates(c.Context())
		if err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(ts))
		for _, t := range ts {
			out = append(out, templateToMap(t))
		}
		return c.JSON(out)
	}
}

func upsertTemplate(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req templateReq
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return apperr.Invalid("invalid body: " + err.Error())
		}
		if req.Name == "" || req.BodyText == "" {
			return apperr.Invalid("name and body_text are required")
		}
		for _, ch := range req.DefaultChannels {
			if !store.ValidChannel(ch) {
				return apperr.Invalid("invalid default channel: " + ch)
			}
		}
		// Compile-check every template slot on upsert. Subject and body_text
		// are text/template; body_html goes through html/template — the two
		// parsers have different grammars, and html/template catches issues
		// (unclosed context boundaries, bad attribute values) that
		// text/template does not. Rejecting here means a bad template can't
		// be persisted and later DLQ'd at send time.
		if req.Subject != nil {
			if err := templates.ValidateTextSyntax("subject", *req.Subject); err != nil {
				return apperr.Invalid("subject template invalid: " + err.Error())
			}
		}
		if err := templates.ValidateTextSyntax("body_text", req.BodyText); err != nil {
			return apperr.Invalid("body_text template invalid: " + err.Error())
		}
		if req.BodyHTML != nil {
			if err := templates.ValidateHTMLSyntax("body_html", *req.BodyHTML); err != nil {
				return apperr.Invalid("body_html template invalid: " + err.Error())
			}
		}
		t, err := d.Store.UpsertTemplate(c.Context(), store.UpsertTemplateParams{
			Name:              req.Name,
			Description:       req.Description,
			Subject:           req.Subject,
			BodyText:          req.BodyText,
			BodyHTML:          req.BodyHTML,
			DefaultChannels:   req.DefaultChannels,
			RequiredVariables: req.RequiredVariables,
		})
		if err != nil {
			return err
		}
		return c.Status(fiber.StatusOK).JSON(templateToMap(*t))
	}
}

func getTemplate(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return apperr.Invalid("id must be a uuid")
		}
		t, err := d.Store.GetTemplate(c.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apperr.NotFound("template not found")
			}
			return err
		}
		return c.JSON(templateToMap(*t))
	}
}

func deleteTemplate(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return apperr.Invalid("id must be a uuid")
		}
		if err := d.Store.DeleteTemplate(c.Context(), id); err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				return apperr.NotFound("template not found")
			case errors.Is(err, store.ErrStillReferenced):
				return apperr.Conflict("template is still referenced by notifications; cannot delete")
			default:
				return err
			}
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// ---- user preferences ----

type prefReq struct {
	Channel string `json:"channel"`
	Enabled bool   `json:"enabled"`
}

func getPreferences(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return apperr.Invalid("id must be a uuid")
		}
		ps, err := d.Store.ListPreferences(c.Context(), id)
		if err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(ps))
		for _, p := range ps {
			out = append(out, map[string]any{
				"channel":    p.Channel,
				"enabled":    p.Enabled,
				"updated_at": p.UpdatedAt,
			})
		}
		return c.JSON(out)
	}
}

func putPreference(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return apperr.Invalid("id must be a uuid")
		}
		var req prefReq
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return apperr.Invalid("invalid body: " + err.Error())
		}
		if !store.ValidChannel(req.Channel) {
			return apperr.Invalid("invalid channel: " + req.Channel)
		}
		if err := d.Store.UpsertPreference(c.Context(), id, req.Channel, req.Enabled); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// ---- users (minimal) ----

type userReq struct {
	ID           string  `json:"id,omitempty"`
	Email        *string `json:"email,omitempty"`
	SlackUserID  *string `json:"slack_user_id,omitempty"`
	SlackWebhook *string `json:"slack_webhook,omitempty"`
}

func upsertUser(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req userReq
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return apperr.Invalid("invalid body: " + err.Error())
		}
		var id uuid.UUID
		if req.ID != "" {
			parsed, err := uuid.Parse(req.ID)
			if err != nil {
				return apperr.Invalid("id must be a uuid")
			}
			id = parsed
		} else {
			id = uuid.New()
		}
		u, err := d.Store.UpsertUser(c.Context(), store.UpsertUserParams{
			ID:           id,
			Email:        req.Email,
			SlackUserID:  req.SlackUserID,
			SlackWebhook: req.SlackWebhook,
		})
		if err != nil {
			return err
		}
		return c.Status(fiber.StatusOK).JSON(map[string]any{
			"id":            u.ID,
			"email":         u.Email,
			"slack_user_id": u.SlackUserID,
			"slack_webhook": u.SlackWebhook,
			"created_at":    u.CreatedAt,
			"updated_at":    u.UpdatedAt,
		})
	}
}

// ---- in-app feed ----

func listInApp(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return apperr.Invalid("id must be a uuid")
		}
		unread := c.Query("unread") == "true"
		limit, _ := strconv.Atoi(c.Query("limit"))

		// Compound cursor "<rfc3339nano>|<uuid>" so same-timestamp rows
		// still order deterministically. (E4.)
		var before store.InAppCursor
		if raw := c.Query("before"); raw != "" {
			parsed, perr := parseInAppCursor(raw)
			if perr != nil {
				return apperr.Invalid("before: " + perr.Error())
			}
			before = parsed
		}

		items, next, err := d.Store.ListInAppPage(c.Context(), store.ListInAppParams{
			UserID:     id,
			UnreadOnly: unread,
			Limit:      limit,
			Before:     before,
		})
		if err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(items))
		for _, n := range items {
			out = append(out, map[string]any{
				"id":              n.ID,
				"user_id":         n.UserID,
				"notification_id": n.NotificationID,
				"title":           n.Title,
				"body":            n.Body,
				"data":            n.Data,
				"read_at":         n.ReadAt,
				"created_at":      n.CreatedAt,
			})
		}
		resp := fiber.Map{"items": out}
		if !next.IsZero() {
			resp["next_cursor"] = formatInAppCursor(next)
		}
		return c.JSON(resp)
	}
}

// formatInAppCursor encodes a compound cursor as "<rfc3339nano>|<uuid>".
// Kept simple on purpose — opaque base64 is a better public contract, but
// for an API under X-API-Key this is readable and trivial to debug.
func formatInAppCursor(c store.InAppCursor) string {
	return c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
}

func parseInAppCursor(raw string) (store.InAppCursor, error) {
	tsPart, idPart, ok := strings.Cut(raw, "|")
	if !ok {
		return store.InAppCursor{}, fmt.Errorf("cursor must be <rfc3339nano>|<uuid>")
	}
	t, err := time.Parse(time.RFC3339Nano, tsPart)
	if err != nil {
		return store.InAppCursor{}, fmt.Errorf("cursor timestamp: %w", err)
	}
	id, err := uuid.Parse(idPart)
	if err != nil {
		return store.InAppCursor{}, fmt.Errorf("cursor id: %w", err)
	}
	return store.InAppCursor{CreatedAt: t, ID: id}, nil
}

func markInAppRead(d *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return apperr.Invalid("user id must be a uuid")
		}
		nid, err := uuid.Parse(c.Params("nid"))
		if err != nil {
			return apperr.Invalid("notification id must be a uuid")
		}
		if err := d.Store.MarkInAppRead(c.Context(), userID, nid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apperr.NotFound("in-app notification not found")
			}
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// ---- helpers ----

func renderNotification(n *store.Notification, d []store.Delivery) notificationResp {
	resp := notificationResp{
		ID:           n.ID,
		Status:       n.Status,
		UserID:       n.UserID,
		TemplateID:   n.TemplateID,
		Channels:     n.Channels,
		Variables:    n.Variables,
		ScheduledFor: n.ScheduledFor,
		CreatedAt:    n.CreatedAt,
	}
	for _, x := range d {
		resp.Deliveries = append(resp.Deliveries, deliveryResp{
			Channel:    x.Channel,
			Status:     x.Status,
			Attempts:   x.Attempts,
			ProviderID: x.ProviderID,
			Error:      x.Error,
			SentAt:     x.SentAt,
		})
	}
	return resp
}

func templateToMap(t store.Template) map[string]any {
	return map[string]any{
		"id":                 t.ID,
		"name":               t.Name,
		"description":        t.Description,
		"subject":            t.Subject,
		"body_text":          t.BodyText,
		"body_html":          t.BodyHTML,
		"default_channels":   t.DefaultChannels,
		"required_variables": t.RequiredVariables,
		"created_at":         t.CreatedAt,
		"updated_at":         t.UpdatedAt,
	}
}

func templateNameOr(t *store.Template, def string) string {
	if t == nil {
		return def
	}
	return t.Name
}

