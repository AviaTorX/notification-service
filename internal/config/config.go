package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIAddr            string
	APIKey             string
	PostgresDSN        string
	PostgresMaxConns   int
	PostgresMinConns   int
	RedisAddr          string
	RedisPoolSize      int
	SMTPHost           string
	SMTPPort           int
	SMTPUser           string
	SMTPPassword       string
	SMTPFrom           string
	SlackWebhookURL    string
	LogLevel           string
	WorkerConcurrency  int
	RateLimitPerMinute int
	IdempotencyTTL     time.Duration
	ReconcilerBatch    int
	ReconcilerInterval time.Duration
	ReconcilerStuck    time.Duration
	// CORSAllowOrigins is a comma-separated list of origins the API will
	// honor on preflight. The special value "*" (or an empty list) means
	// "permit any origin" — correct for dev where `web/in-app-demo.html`
	// is opened from `file://`, unsafe in production where it turns the
	// API's CORS posture into "anyone on the internet can fetch
	// responses with a valid X-API-Key". Production deployments MUST set
	// this to a concrete origin list.
	CORSAllowOrigins []string
}

// Load reads the config from process env vars. Malformed integers surface
// as a returned error with the env var name in the message; nothing
// panics.
func Load() (Config, error) {
	workerConc, err := envInt("WORKER_CONCURRENCY", 20)
	if err != nil {
		return Config{}, err
	}
	rateLimit, err := envInt("RATE_LIMIT_PER_MINUTE", 60)
	if err != nil {
		return Config{}, err
	}
	smtpPort, err := envInt("SMTP_PORT", 1025)
	if err != nil {
		return Config{}, err
	}
	pgMax, err := envInt("POSTGRES_MAX_CONNS", 20)
	if err != nil {
		return Config{}, err
	}
	pgMin, err := envInt("POSTGRES_MIN_CONNS", 2)
	if err != nil {
		return Config{}, err
	}
	redisPool, err := envInt("REDIS_POOL_SIZE", 20)
	if err != nil {
		return Config{}, err
	}
	reconBatch, err := envInt("RECONCILER_BATCH_SIZE", 1000)
	if err != nil {
		return Config{}, err
	}
	reconIntervalSec, err := envInt("RECONCILER_INTERVAL_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}
	reconStuckSec, err := envInt("RECONCILER_STUCK_AFTER_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}

	c := Config{
		APIAddr:            env("API_ADDR", ":8080"),
		APIKey:             env("API_KEY", ""),
		RedisAddr:          env("REDIS_ADDR", "redis:6379"),
		RedisPoolSize:      redisPool,
		SMTPHost:           env("SMTP_HOST", "mailhog"),
		SMTPPort:           smtpPort,
		SMTPUser:           env("SMTP_USER", ""),
		SMTPPassword:       env("SMTP_PASSWORD", ""),
		SMTPFrom:           env("SMTP_FROM", "notifications@example.com"),
		SlackWebhookURL:    env("SLACK_WEBHOOK_URL", "http://slack-mock:9000/webhook"),
		LogLevel:           env("LOG_LEVEL", "info"),
		WorkerConcurrency:  workerConc,
		RateLimitPerMinute: rateLimit,
		IdempotencyTTL:     24 * time.Hour,
		PostgresMaxConns:   pgMax,
		PostgresMinConns:   pgMin,
		ReconcilerBatch:    reconBatch,
		ReconcilerInterval: time.Duration(reconIntervalSec) * time.Second,
		ReconcilerStuck:    time.Duration(reconStuckSec) * time.Second,
		CORSAllowOrigins:   parseCSV(env("CORS_ALLOW_ORIGINS", "*")),
	}
	c.PostgresDSN = fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=%s",
		env("POSTGRES_USER", "notif"),
		env("POSTGRES_PASSWORD", "notifpass"),
		env("POSTGRES_HOST", "postgres"),
		env("POSTGRES_PORT", "5432"),
		env("POSTGRES_DB", "notifications"),
		env("POSTGRES_SSLMODE", "disable"),
	)
	if c.APIKey == "" {
		return c, errors.New("API_KEY is required")
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	return n, nil
}

// parseCSV splits "a,b, c" into ["a", "b", "c"], trimming whitespace
// and dropping empties. The single-value form "*" survives unchanged so
// the server layer can detect the permissive sentinel.
func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
