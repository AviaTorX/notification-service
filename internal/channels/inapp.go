package channels

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/avinash-shinde/notification-service/internal/apperr"
	"github.com/avinash-shinde/notification-service/internal/store"
)

// InAppAdapter persists the message to `in_app_notifications` so the client
// can pull it via GET /v1/users/:id/in-app-notifications.
type InAppAdapter struct {
	Store *store.Store
}

func NewInAppAdapter(s *store.Store) *InAppAdapter { return &InAppAdapter{Store: s} }

func (a *InAppAdapter) Name() string { return "in_app" }

func (a *InAppAdapter) Send(ctx context.Context, msg Message) (Result, error) {
	userID, err := uuid.Parse(msg.UserID)
	if err != nil {
		return Result{}, apperr.Permanent("invalid user id", err)
	}
	var notifID *uuid.UUID
	if msg.NotificationID != "" {
		if id, err := uuid.Parse(msg.NotificationID); err == nil {
			notifID = &id
		}
	}
	title := msg.Subject
	if title == "" {
		title = msg.TemplateName
	}
	n, err := a.Store.CreateInApp(ctx, store.CreateInAppParams{
		UserID:         userID,
		NotificationID: notifID,
		Title:          title,
		Body:           msg.BodyText,
		Data:           map[string]any{"template": msg.TemplateName},
	})
	if err != nil {
		return Result{}, classifyPGError("persist in-app notification", err)
	}
	return Result{ProviderID: "inapp:" + n.ID.String()}, nil
}

// classifyPGError maps a Postgres driver error to a retryable or
// permanent failure. Classifying FK / CHECK / unique violations as
// permanent prevents a doomed retry loop on "user_id deleted mid-flight"
// or "schema disagrees with writer."
func classifyPGError(context string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503", // foreign_key_violation
			"23505", // unique_violation
			"23514", // check_violation
			"22P02": // invalid_text_representation (bad UUID, bad JSONB)
			return apperr.Permanent(context+" (pg "+pgErr.Code+")", err)
		}
	}
	return apperr.Transient(context, err)
}
