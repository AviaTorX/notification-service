package idempotency

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// gateRedis returns a Redis client pointed at REDIS_ADDR or skips the test.
// We do NOT spin up a test container here because the project already runs
// Redis via docker-compose for the E2E suite — reusing it keeps the unit
// test surface fast and deterministic.
func gateRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := firstNonEmpty("TEST_REDIS_ADDR", "localhost:56379")
	c := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable at %s (set TEST_REDIS_ADDR or run docker-compose): %v", addr, err)
	}
	return c
}

func firstNonEmpty(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

func TestCache_GetSet_Roundtrip(t *testing.T) {
	client := gateRedis(t)
	defer client.Close()

	c := &Cache{Client: client, TTL: time.Minute}
	ctx := context.Background()
	apiKey, route, key := "apikey", "POST:/v1/foo", "run-1-" + time.Now().Format(time.RFC3339Nano)
	reqHash := HashRequest([]byte(`{"a":1}`))

	got, err := c.Get(ctx, apiKey, route, key, reqHash)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("cache miss should return (nil, nil)")
	}

	want := []byte(`{"id":"abc"}`)
	if err := c.Set(ctx, apiKey, route, key, reqHash, 202, want); err != nil {
		t.Fatal(err)
	}
	got, err = c.Get(ctx, apiKey, route, key, reqHash)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || string(got.Body) != string(want) || got.Status != 202 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestCache_Get_ConflictOnDifferentBody(t *testing.T) {
	client := gateRedis(t)
	defer client.Close()

	c := &Cache{Client: client, TTL: time.Minute}
	ctx := context.Background()
	apiKey, route, key := "apikey", "POST:/v1/foo", "conflict-" + time.Now().Format(time.RFC3339Nano)
	origHash := HashRequest([]byte(`{"a":1}`))
	altHash := HashRequest([]byte(`{"a":2}`))

	if err := c.Set(ctx, apiKey, route, key, origHash, 202, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	_, err := c.Get(ctx, apiKey, route, key, altHash)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestCache_TTLExpires(t *testing.T) {
	client := gateRedis(t)
	defer client.Close()

	c := &Cache{Client: client, TTL: 500 * time.Millisecond}
	ctx := context.Background()
	apiKey, route, key := "apikey", "POST:/v1/foo", "ttl-" + time.Now().Format(time.RFC3339Nano)
	h := HashRequest([]byte(`{}`))
	if err := c.Set(ctx, apiKey, route, key, h, 200, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	got, err := c.Get(ctx, apiKey, route, key, h)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("record should have expired")
	}
}
