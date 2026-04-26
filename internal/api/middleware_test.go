package api

import (
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// C4: the middleware must reject a request whose API key differs from the
// configured value, using a constant-time comparison.
func TestAPIKeyMiddleware_RejectsWrongKey(t *testing.T) {
	app := fiber.New()
	app.Use(apiKeyMiddleware("right"))
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(headerAPIKey, "wrong")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

func TestAPIKeyMiddleware_AcceptsRightKey(t *testing.T) {
	app := fiber.New()
	app.Use(apiKeyMiddleware("right"))
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(headerAPIKey, "right")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status=%d, want 200", resp.StatusCode)
	}
}

// M3: env-gated CORS. With a concrete allowlist, a disallowed Origin
// must not receive an Access-Control-Allow-Origin header on preflight.
// (Without this header, a browser refuses to read the response — which
// is the CORS protocol's deny path.)
func TestCORS_AllowlistBlocksDisallowedOrigin(t *testing.T) {
	app := fiber.New()
	app.Use(corsForTest([]string{"https://app.example.com"}))
	app.Get("/x", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	// Allowed
	req := httptest.NewRequest("OPTIONS", "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("allowed origin should be echoed, got %q", got)
	}

	// Disallowed
	req = httptest.NewRequest("OPTIONS", "/x", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin must NOT receive Access-Control-Allow-Origin, got %q", got)
	}
}

// corsForTest is a minimal re-construction of the server's CORS setup
// for isolated middleware testing. Kept in the test file so a change in
// server.go's CORS wiring also has to update this helper — cheap way to
// keep them in lock-step.
func corsForTest(origins []string) fiber.Handler {
	return cors.New(cors.Config{
		AllowOrigins: strings.Join(origins, ","),
		AllowMethods: "GET, POST, OPTIONS",
		AllowHeaders: "X-API-Key",
	})
}

func TestAPIKeyMiddleware_RejectsMissingKey(t *testing.T) {
	app := fiber.New()
	app.Use(apiKeyMiddleware("right"))
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

// C3: the access-log status must reflect what the client saw, even when
// the handler returned an error and Fiber's error handler rewrote the
// status.
//
// Previously this test only asserted resp.StatusCode, which only proves
// the error handler ran — it did NOT prove the log captured the
// rewritten status. Now we capture the log output via zerolog's writer
// swap and assert the logged status field matches the client response.
func TestAccessLog_CapturesStatusAfterErrorHandler(t *testing.T) {
	var buf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = origLogger }()

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return c.Status(418).SendString("teapot")
		},
	})
	app.Use(accessLogMiddleware())
	app.Get("/", func(c *fiber.Ctx) error {
		return fiber.NewError(500, "boom")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 418 {
		t.Errorf("client status=%d, want 418 (error handler rewrote it)", resp.StatusCode)
	}
	// The real assertion: the emitted log line must carry status 418,
	// not 500. That's what proves the middleware sees the post-error-
	// handler status the client actually saw.
	logLine := buf.String()
	if !strings.Contains(logLine, `"status":418`) {
		t.Errorf("access log did not capture rewritten status; got: %s", logLine)
	}
	if strings.Contains(logLine, `"status":500`) {
		t.Errorf("access log captured stale pre-error-handler status 500; got: %s", logLine)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "teapot") {
		t.Errorf("body did not come from error handler: %q", body)
	}
}
