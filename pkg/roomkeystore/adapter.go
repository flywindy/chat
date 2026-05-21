package roomkeystore

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// clusterAdapter wraps *redis.ClusterClient to satisfy hashCommander.
type clusterAdapter struct {
	c *redis.ClusterClient
}

func (a *clusterAdapter) hset(ctx context.Context, key string, priv string) error {
	return a.c.HSet(ctx, key, "priv", priv, "ver", "0").Err()
}

func (a *clusterAdapter) hsetWithVersion(ctx context.Context, key string, priv string, version int) error {
	return a.c.HSet(ctx, key, "priv", priv, "ver", strconv.Itoa(version)).Err()
}

func (a *clusterAdapter) hgetall(ctx context.Context, key string) (map[string]string, error) {
	return a.c.HGetAll(ctx, key).Result()
}

// rotateScript atomically reads the current key, copies it to the previous
// slot with a grace-period TTL, increments the version, and writes the new key as current.
// Returns the new version number.
// This runs as a single Lua script so no other client can interleave.
var rotateScript = redis.NewScript(`
local currentKey = KEYS[1]
local prevKey    = KEYS[2]
local newPriv    = ARGV[1]
local graceSec   = tonumber(ARGV[2])

local cur = redis.call('HGETALL', currentKey)
if #cur == 0 then
    return redis.error_reply('no current key')
end

local curVer = tonumber(redis.call('HGET', currentKey, 'ver')) or 0
local newVer = curVer + 1

redis.call('DEL', prevKey)
redis.call('HSET', prevKey, unpack(cur))
redis.call('EXPIRE', prevKey, graceSec)

redis.call('HSET', currentKey, 'priv', newPriv, 'ver', tostring(newVer))
return newVer
`)

func (a *clusterAdapter) rotatePipeline(ctx context.Context, currentKey, prevKey string, priv string, gracePeriod time.Duration) (int, error) {
	graceSec := int(gracePeriod.Seconds())
	if graceSec < 1 {
		graceSec = 1
	}
	result, err := rotateScript.Run(ctx, a.c, []string{currentKey, prevKey}, priv, graceSec).Int()
	if isLuaNoCurrentKeyErr(err) {
		return 0, ErrNoCurrentKey
	}
	return result, err
}

// isLuaNoCurrentKeyErr reports whether err is the sentinel the rotate Lua
// script emits via redis.error_reply('no current key'). go-redis surfaces
// Lua error_reply values as untyped errors whose message equals the Lua
// string verbatim — typed error wrapping is not available at this boundary.
func isLuaNoCurrentKeyErr(err error) bool {
	return err != nil && err.Error() == "no current key"
}

func (a *clusterAdapter) deletePipeline(ctx context.Context, currentKey, prevKey string) error {
	return a.c.Del(ctx, currentKey, prevKey).Err()
}

// hgetallMany issues HGETALL for every key in a single pipeline and returns
// one map per input key (in the same order). A missing hash yields an empty
// map rather than an error, matching go-redis v9 HGetAll semantics.
func (a *clusterAdapter) hgetallMany(ctx context.Context, keys []string) ([]map[string]string, error) {
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

func (a *clusterAdapter) closeClient() error {
	return a.c.Close()
}

// ClusterConfig holds connection config for a Valkey cluster deployment.
// Addrs is a list of seed node addresses; go-redis ClusterClient discovers
// all nodes automatically via CLUSTER SLOTS. One address is sufficient but
// listing all masters is more robust against seed-node downtime at connect time.
type ClusterConfig struct {
	Addrs       []string
	Password    string
	GracePeriod time.Duration
}

// NewValkeyClusterStore creates a valkeyStore backed by a Valkey cluster,
// pings the cluster to verify connectivity, and returns it.
func NewValkeyClusterStore(cfg ClusterConfig) (RoomKeyStore, error) {
	c := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:    cfg.Addrs,
		Password: cfg.Password,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		if closeErr := c.Close(); closeErr != nil {
			slog.Warn("valkey cluster close after failed connect", "error", closeErr)
		}
		return nil, fmt.Errorf("valkey cluster connect: %w", err)
	}
	return &valkeyStore{client: &clusterAdapter{c: c}, closer: c, gracePeriod: cfg.GracePeriod}, nil
}

// NewValkeyClusterStoreFromClient wraps a pre-built *redis.ClusterClient as a
// RoomKeyStore. Intended for tests that inject a client configured with a
// ClusterSlots override (testcontainer port-mapping workaround).
func NewValkeyClusterStoreFromClient(c *redis.ClusterClient, gracePeriod time.Duration) RoomKeyStore {
	return &valkeyStore{client: &clusterAdapter{c: c}, closer: c, gracePeriod: gracePeriod}
}
