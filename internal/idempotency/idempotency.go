// Package idempotency implements the Stripe-style Idempotency-Key contract.
//
// On a repeated request with the same key + same route, return the cached
// response. If the cached request body differs from the new one, we return
// 409 Conflict — protecting callers from accidental key reuse across
// distinct operations.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	Client *redis.Client
	TTL    time.Duration
}

// New builds a cache that owns its own Redis client. Production callers
// should use NewWithClient to share a single pool.
func New(addr string, ttl time.Duration) *Cache {
	return NewWithClient(redis.NewClient(&redis.Options{Addr: addr}), ttl)
}

func NewWithClient(c *redis.Client, ttl time.Duration) *Cache {
	return &Cache{Client: c, TTL: ttl}
}

// Close is a no-op when the client is shared; owner closes it.
func (c *Cache) Close() error { return nil }

type Record struct {
	Status      int             `json:"status"`
	Body        json.RawMessage `json:"body"`
	RequestHash string          `json:"request_hash"`
}

var ErrConflict = errors.New("idempotency key reused with different request body")

func key(apiKey, route, idempotencyKey string) string {
	return fmt.Sprintf("idem:%s:%s:%s", apiKey, route, idempotencyKey)
}

func HashRequest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (c *Cache) Get(ctx context.Context, apiKey, route, idemKey, requestHash string) (*Record, error) {
	v, err := c.Client.Get(ctx, key(apiKey, route, idemKey)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(v, &r); err != nil {
		return nil, err
	}
	if r.RequestHash != requestHash {
		return nil, ErrConflict
	}
	return &r, nil
}

func (c *Cache) Set(ctx context.Context, apiKey, route, idemKey, requestHash string, status int, body []byte) error {
	payload, err := json.Marshal(Record{Status: status, Body: body, RequestHash: requestHash})
	if err != nil {
		return err
	}
	return c.Client.Set(ctx, key(apiKey, route, idemKey), payload, c.TTL).Err()
}
