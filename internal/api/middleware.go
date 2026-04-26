package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/avinash-shinde/notification-service/internal/observability"
)

const (
	headerAPIKey    = "X-API-Key"
	headerIdempKey  = "Idempotency-Key"
	headerRequestID = "X-Request-ID"
	ctxRequestIDKey = "request_id"
)

// apiKeyMiddleware enforces the configured API key using a constant-time
// SHA-256 comparison so a remote attacker cannot recover the expected key
// via response-time side channels.
//
// Production swap: OIDC for human actors, mTLS or JWT-with-rotation for
// service-to-service.
func apiKeyMiddleware(expected string) fiber.Handler {
	expectedSum := sha256.Sum256([]byte(expected))
	return func(c *fiber.Ctx) error {
		got := c.Get(headerAPIKey)
		if got == "" {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid or missing api key")
		}
		gotSum := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(gotSum[:], expectedSum[:]) != 1 {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid or missing api key")
		}
		return c.Next()
	}
}

// requestIDMiddleware attaches an X-Request-ID to every request (honours a
// caller-supplied value so upstream tracing stays coherent).
func requestIDMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		rid := c.Get(headerRequestID)
		if rid == "" {
			rid = uuid.NewString()
		}
		c.Locals(ctxRequestIDKey, rid)
		c.Set(headerRequestID, rid)
		return c.Next()
	}
}

// accessLogMiddleware emits a structured log line per request.
//
// In Fiber's middleware model, a handler that returns an error hands that
// error back up the middleware stack BEFORE Fiber dispatches to the
// configured ErrorHandler. That means reading `c.Response().StatusCode()`
// right after `c.Next()` returns captures whatever status the handler
// last set, NOT the status the client will receive if an ErrorHandler
// rewrites it.
//
// To log the status the client actually sees, we invoke the app's
// ErrorHandler ourselves when c.Next() returns non-nil, then read the
// status and swallow the error (Fiber's internal finalizer won't run the
// ErrorHandler a second time when we return nil).
//
// Method/path/requestID are captured BEFORE c.Next() because Fiber
// returns zero-copy strings backed by the fasthttp request buffer, which
// is reused by the pool after the response is written — the same
// fasthttp trap fixed in metricsMiddleware.
func accessLogMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		method := string(append([]byte{}, c.Method()...))
		path := string(append([]byte{}, c.Path()...))
		requestID := rid(c)

		err := c.Next()
		if err != nil {
			// Dispatch through the app-level error handler so the status
			// we log matches the status the client sees. errorHandler
			// never returns an error itself (it always writes a JSON
			// body and returns nil), but check anyway.
			if eh := c.App().ErrorHandler; eh != nil {
				_ = eh(c, err)
			}
		}
		status := c.Response().StatusCode()

		ev := log.Info()
		if err != nil || status >= 400 {
			ev = log.Warn()
			if err != nil {
				ev = ev.Err(err)
			}
		}
		ev.Str("request_id", requestID).
			Str("method", method).
			Str("path", path).
			Int("status", status).
			Msg("http")
		// Return nil because we already invoked the error handler. If we
		// returned err, Fiber would run the error handler a second time
		// and overwrite our response body.
		return nil
	}
}

func rid(c *fiber.Ctx) string {
	if v, ok := c.Locals(ctxRequestIDKey).(string); ok {
		return v
	}
	return ""
}

// metricsMiddleware records http_request_duration_seconds and
// http_requests_total keyed by the Fiber route template (not the raw
// path) so metric cardinality stays bounded regardless of user IDs.
//
// Skips /metrics, /healthz, /readyz to keep the latency histogram a
// real picture of business-traffic p95/p99: K8s probes dominate any
// interesting percentile otherwise at ~0.1ms per probe × thousands
// per minute per pod. /metrics is also excluded to avoid
// scrape-observes-itself feedback on the default registerer.
//
// Method and path are COPIED before c.Next() because Fiber returns
// zero-copy strings backed by the fasthttp request buffer, and that
// buffer is reused by the pool after the response is written. Reading
// c.Method() after the response renders would silently give us bytes
// from an unrelated subsequent request ("GETT" in dev — two requests'
// buffers overlapping).
func metricsMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		path := c.Path()
		switch path {
		case "/metrics", "/healthz", "/readyz":
			return c.Next()
		}
		method := string(append([]byte{}, c.Method()...))
		start := time.Now()
		err := c.Next()
		status := strconv.Itoa(c.Response().StatusCode())
		route := c.Route().Path
		if route == "" {
			route = "unmatched"
		}
		observability.HTTPRequestDuration.WithLabelValues(method, route, status).
			Observe(time.Since(start).Seconds())
		observability.HTTPRequestsTotal.WithLabelValues(method, route, status).Inc()
		return err
	}
}
