package api

import (
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/avinash-shinde/notification-service/internal/apperr"
)

type errorBody struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// errorHandler converts apperr.Error to the right HTTP status + JSON body.
// Anything else becomes a 500 with a generic message; the full error is
// logged via the access-log middleware.
func errorHandler(c *fiber.Ctx, err error) error {
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return c.Status(fe.Code).JSON(errorBody{Error: fe.Message, RequestID: rid(c)})
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		code := statusForKind(ae.Kind)
		return c.Status(code).JSON(errorBody{Error: ae.Message, Code: codeForKind(ae.Kind), RequestID: rid(c)})
	}
	return c.Status(fiber.StatusInternalServerError).JSON(errorBody{Error: "internal error", RequestID: rid(c)})
}

func statusForKind(k apperr.Kind) int {
	switch k {
	case apperr.KindInvalid:
		return fiber.StatusBadRequest
	case apperr.KindNotFound:
		return fiber.StatusNotFound
	case apperr.KindConflict:
		return fiber.StatusConflict
	case apperr.KindUnauthorized:
		return fiber.StatusUnauthorized
	case apperr.KindRateLimited:
		return fiber.StatusTooManyRequests
	default:
		return fiber.StatusInternalServerError
	}
}

func codeForKind(k apperr.Kind) string {
	switch k {
	case apperr.KindInvalid:
		return "invalid_request"
	case apperr.KindNotFound:
		return "not_found"
	case apperr.KindConflict:
		return "conflict"
	case apperr.KindUnauthorized:
		return "unauthorized"
	case apperr.KindRateLimited:
		return "rate_limited"
	default:
		return "internal_error"
	}
}
