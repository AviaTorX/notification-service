package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/avinash-shinde/notification-service/internal/config"
	"github.com/avinash-shinde/notification-service/internal/idempotency"
	"github.com/avinash-shinde/notification-service/internal/queue"
	"github.com/avinash-shinde/notification-service/internal/ratelimit"
	"github.com/avinash-shinde/notification-service/internal/store"
)

// isPermissiveCORS reports whether the configured allowlist means
// "let any origin through." Empty or a single "*" entry both map to
// permissive — everything else is treated as a concrete allowlist.
func isPermissiveCORS(origins []string) bool {
	if len(origins) == 0 {
		return true
	}
	if len(origins) == 1 && origins[0] == "*" {
		return true
	}
	return false
}

// Run wires dependencies and blocks until ctx is cancelled.
//
// Dependency sharing: rate limiter and idempotency cache share a single
// Redis client. Asynq is the intentional exception — it manages its own
// connection pool internally and the public API only accepts a
// `RedisClientOpt`; sharing a go-redis client with Asynq is not
// supported upstream.
func Run(ctx context.Context, cfg config.Config) error {
	st, err := store.New(ctx, cfg.PostgresDSN, store.PoolOptions{
		MaxConns: int32(cfg.PostgresMaxConns),
		MinConns: int32(cfg.PostgresMinConns),
	})
	if err != nil {
		return err
	}
	defer st.Close()

	// One Redis client, shared across subsystems that talk go-redis.
	sharedRedis := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		PoolSize: cfg.RedisPoolSize,
	})
	defer sharedRedis.Close()

	enq := queue.NewEnqueuer(cfg.RedisAddr)
	defer enq.Close()

	limiter := ratelimit.NewWithClient(sharedRedis, cfg.RateLimitPerMinute)
	idem := idempotency.NewWithClient(sharedRedis, cfg.IdempotencyTTL)

	app := fiber.New(fiber.Config{
		AppName:               "notifd-api",
		DisableStartupMessage: true,
		ReadTimeout:           10 * time.Second,
		WriteTimeout:          20 * time.Second,
		ErrorHandler:          errorHandler,
	})

	// Middleware ordering contract — DO NOT REORDER without updating
	// the access-log / metrics interaction comment in middleware.go:
	//   recover     → turns handler panics into structured 500s so the
	//                 following middlewares still see a response.
	//   cors        → allowed-origin handling + preflight short-circuit;
	//                 preflights intentionally skip the stack below.
	//   requestID   → mint or honor X-Request-ID for downstream logs.
	//   metrics     → records http_request_duration_seconds keyed by
	//                 the Fiber route template (not raw path).
	//   accessLog   → invokes the error handler inline and captures the
	//                 post-rewrite status; returns nil so Fiber does
	//                 not run the error handler a second time.
	app.Use(recover.New(recover.Config{EnableStackTrace: true}))
	// CORS is env-gated via CORS_ALLOW_ORIGINS:
	//   - "*" (default) or empty   → permissive, any origin honored.
	//     Required for `web/in-app-demo.html` opened via file:// (Origin:
	//     null) and for frictionless development against the API.
	//   - comma-separated origins  → strict allowlist. Production MUST
	//     set this; safety-in-depth even though every mutating route is
	//     still gated by X-API-Key. AllowCredentials stays pinned to
	//     false so a future switch to cookie-based auth cannot silently
	//     widen this into a CSRF hole.
	corsCfg := cors.Config{
		AllowCredentials: false,
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization, X-API-Key, Idempotency-Key, X-Request-ID",
		AllowMethods:     "GET, POST, PUT, DELETE, OPTIONS",
		ExposeHeaders:    "X-Request-ID, Idempotent-Replay",
		MaxAge:           600,
	}
	if isPermissiveCORS(cfg.CORSAllowOrigins) {
		log.Warn().Msg("CORS is permissive (any origin). Set CORS_ALLOW_ORIGINS to a concrete list for production.")
		// Fiber's cors middleware consults AllowOriginsFunc only when
		// AllowOrigins does not match. Setting AllowOrigins to "" and
		// a true-returning Func keeps every origin honored.
		corsCfg.AllowOrigins = ""
		corsCfg.AllowOriginsFunc = func(origin string) bool { return true }
	} else {
		// AllowOrigins is the authoritative check in Fiber v2's cors —
		// a comma-separated list; origins not in it get an empty
		// Access-Control-Allow-Origin header (browsers block the
		// response). Leaving AllowOriginsFunc unset avoids the
		// middleware's "either-or" trap where a Func that returns false
		// can still echo the origin if AllowOrigins matches.
		corsCfg.AllowOrigins = strings.Join(cfg.CORSAllowOrigins, ",")
		log.Info().Strs("origins", cfg.CORSAllowOrigins).Msg("CORS allowlist active")
	}
	app.Use(cors.New(corsCfg))
	app.Use(requestIDMiddleware())
	app.Use(metricsMiddleware())
	app.Use(accessLogMiddleware())

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/readyz", func(c *fiber.Ctx) error {
		rctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
		defer cancel()
		if err := st.Pool.Ping(rctx); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "db_down", "error": err.Error()})
		}
		if err := sharedRedis.Ping(rctx).Err(); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "redis_down", "error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ready"})
	})
	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	Mount(app, &Deps{
		Store:       st,
		Enqueuer:    enq,
		RateLimiter: limiter,
		Idempotency: idem,
		APIKey:      cfg.APIKey,
	})

	errCh := make(chan error, 1)
	go func() {
		log.Info().Str("addr", cfg.APIAddr).Msg("api listening")
		if err := app.Listen(cfg.APIAddr); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info().Msg("api shutdown requested")
		return app.Shutdown()
	case err := <-errCh:
		return err
	}
}
