// Package valkeyutil provides thin connection + JSON helpers around the
// Valkey (Redis-compatible) client. Modeled on pkg/mongoutil so services
// get a one-call Connect + Disconnect pair plus typed get/set helpers for
// the common JSON-over-Valkey pattern.
//
// The underlying client is go-redis/v9 — Valkey is wire-compatible with
// Redis so no Valkey-specific driver is needed.
package valkeyutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client is the interface exposed by Connect. Tests can substitute their
// own implementation without depending on go-redis directly.
type Client interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	Close() error
}

// ErrCacheMiss is returned by Get and GetJSON when the key does not exist.
var ErrCacheMiss = errors.New("valkey: cache miss")

type redisClient struct {
	c *redis.Client
}

// Connect dials Valkey/Redis, verifies connectivity with PING, and returns
// a Client. Follows the same `fmt.Errorf("… connect: %w", err)` wrapping
// style used by pkg/mongoutil so upstream logs are consistent.
func Connect(ctx context.Context, addr, password string) (Client, error) {
	c := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		// Close the half-constructed client on the ping-failure path so
		// repeated connect failures (e.g. startup retry loops against
		// an unreachable addr) don't leak internal go-redis pool state.
		if closeErr := c.Close(); closeErr != nil {
			slog.Warn("valkey close after failed connect", "error", closeErr)
		}
		return nil, fmt.Errorf("valkey connect: %w", err)
	}
	slog.Info("connected to Valkey", "addr", addr)
	return &redisClient{c: c}, nil
}

// Disconnect closes the client and logs any failure at ERROR.
func Disconnect(client Client) {
	if client == nil {
		return
	}
	if err := client.Close(); err != nil {
		slog.Error("valkey disconnect failed", "error", err)
	}
}

func (r *redisClient) Get(ctx context.Context, key string) (string, error) {
	val, err := r.c.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrCacheMiss
	}
	if err != nil {
		return "", fmt.Errorf("valkey get: %w", err)
	}
	return val, nil
}

func (r *redisClient) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if err := r.c.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("valkey set: %w", err)
	}
	return nil
}

func (r *redisClient) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := r.c.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("valkey del: %w", err)
	}
	return nil
}

func (r *redisClient) Close() error {
	return r.c.Close()
}

// GetJSON reads `key` from Valkey and unmarshals the stored JSON into
// `out`. Returns ErrCacheMiss (wrapped) if the key is not set so callers
// can `errors.Is` it; all other failures (transport, malformed JSON) wrap
// as "valkey get json: …".
func GetJSON(ctx context.Context, client Client, key string, out any) error {
	raw, err := client.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("valkey get json: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("valkey get json: unmarshal: %w", err)
	}
	return nil
}

// SetJSONWithTTL marshals `value` to JSON and stores it under `key` with
// the given TTL. Zero ttl stores the key without expiry.
func SetJSONWithTTL(ctx context.Context, client Client, key string, value any, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("valkey set json: marshal: %w", err)
	}
	if err := client.Set(ctx, key, string(data), ttl); err != nil {
		return fmt.Errorf("valkey set json: %w", err)
	}
	return nil
}
