// Package ratelimit implements a per-minute fixed-window counter in Redis.
// Keyed on (user_id, channel) to protect a single recipient from being
// flooded; the API layer rejects enqueue attempts that would exceed the
// budget.
//
// Fixed-window (via time.Now().Truncate(Window)) is Lua-free and
// branch-free on the hot path; the worst-case error is a 2× burst across
// a minute boundary, which is acceptable for recipient-protection. A
// sliding window or token bucket is the right upgrade when this trade-off
// stops being acceptable — both have precedents under `golang.org/x`.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Limiter struct {
	Client    *redis.Client
	PerMinute int
	Window    time.Duration
}

// New builds a limiter that owns its own Redis client. Used by tests /
// callers that don't share Redis. Production callers use NewWithClient to
// reuse a single pool across limiter + idempotency + healthz.
func New(addr string, perMinute int) *Limiter {
	return NewWithClient(redis.NewClient(&redis.Options{Addr: addr}), perMinute)
}

func NewWithClient(c *redis.Client, perMinute int) *Limiter {
	return &Limiter{Client: c, PerMinute: perMinute, Window: time.Minute}
}

// Close is a no-op when the client is shared with other subsystems.
// The owner of the shared client is responsible for closing it.
func (l *Limiter) Close() error { return nil }

// Allow returns (allowed, remaining, error). If `PerMinute` is zero or
// negative, limiter is disabled (always allow).
func (l *Limiter) Allow(ctx context.Context, userID, channel string) (bool, int, error) {
	if l.PerMinute <= 0 {
		return true, 0, nil
	}
	bucket := time.Now().UTC().Truncate(l.Window).Unix()
	key := fmt.Sprintf("rl:%s:%s:%d", userID, channel, bucket)
	pipe := l.Client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, l.Window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, 0, err
	}
	count := int(incr.Val())
	remaining := l.PerMinute - count
	if remaining < 0 {
		remaining = 0
	}
	return count <= l.PerMinute, remaining, nil
}
