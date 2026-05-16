package roomkeystore

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisAdapter wraps *redis.Client to satisfy hashCommander.
type redisAdapter struct {
	c *redis.Client
}

func (a *redisAdapter) hset(ctx context.Context, key string, pub, priv string) error {
	return a.c.HSet(ctx, key, "pub", pub, "priv", priv, "ver", "0").Err()
}

func (a *redisAdapter) hsetWithVersion(ctx context.Context, key string, pub, priv string, version int) error {
	return a.c.HSet(ctx, key, "pub", pub, "priv", priv, "ver", strconv.Itoa(version)).Err()
}

func (a *redisAdapter) hgetall(ctx context.Context, key string) (map[string]string, error) {
	return a.c.HGetAll(ctx, key).Result()
}

// rotateScript atomically reads the current key, copies it to the previous
// slot with a grace-period TTL, increments the version, and writes the new key as current.
// Returns the new version number.
// This runs as a single Lua script so no other client can interleave.
var rotateScript = redis.NewScript(`
local currentKey = KEYS[1]
local prevKey    = KEYS[2]
local newPub     = ARGV[1]
local newPriv    = ARGV[2]
local graceSec   = tonumber(ARGV[3])

local cur = redis.call('HGETALL', currentKey)
if #cur == 0 then
    return redis.error_reply('no current key')
end

local curVer = tonumber(redis.call('HGET', currentKey, 'ver')) or 0
local newVer = curVer + 1

redis.call('DEL', prevKey)
redis.call('HSET', prevKey, unpack(cur))
redis.call('EXPIRE', prevKey, graceSec)

redis.call('HSET', currentKey, 'pub', newPub, 'priv', newPriv, 'ver', tostring(newVer))
return newVer
`)

func (a *redisAdapter) rotatePipeline(ctx context.Context, currentKey, prevKey string, pub, priv string, gracePeriod time.Duration) (int, error) {
	graceSec := int(gracePeriod.Seconds())
	if graceSec < 1 {
		graceSec = 1
	}
	result, err := rotateScript.Run(ctx, a.c, []string{currentKey, prevKey}, pub, priv, graceSec).Int()
	if err != nil && strings.Contains(err.Error(), "no current key") {
		return 0, ErrNoCurrentKey
	}
	return result, err
}

func (a *redisAdapter) deletePipeline(ctx context.Context, currentKey, prevKey string) error {
	return a.c.Del(ctx, currentKey, prevKey).Err()
}

// hgetallMany issues HGETALL for every key in a single pipeline and returns
// one map per input key (in the same order). A missing hash yields an empty
// map rather than an error, matching go-redis v9 HGetAll semantics.
func (a *redisAdapter) hgetallMany(ctx context.Context, keys []string) ([]map[string]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	pipe := a.c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(keys))
	for i, k := range keys {
		cmds[i] = pipe.HGetAll(ctx, k)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([]map[string]string, len(keys))
	for i, c := range cmds {
		m, err := c.Result()
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

func (a *redisAdapter) closeClient() error {
	return a.c.Close()
}

// NewValkeyStore creates a valkeyStore, pings Valkey to verify connectivity, and returns it.
func NewValkeyStore(cfg Config) (RoomKeyStore, error) {
	c := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("valkey connect: %w", err)
	}
	return &valkeyStore{client: &redisAdapter{c: c}, closer: c, gracePeriod: cfg.GracePeriod}, nil
}
