package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hmchangw/chat/pkg/model"
)

const keyPrefix = "presence:"
const sweepKey = keyPrefix + "sweep"

func connsKey(account string) string  { return keyPrefix + "{" + account + "}:conns" }
func manualKey(account string) string { return keyPrefix + "{" + account + "}:manual" }
func statusKey(account string) string { return keyPrefix + "{" + account + "}:status" }

// Each connection is stored in the conns hash as field=connID,
// value="<away01>:<lastSeenMs>" where away01 is "0" (active) or "1" (inactive).
// The scripts are split per operation (per review): ping is a cheap liveness
// refresh that skips recompute for known connections, while activity/bye/manual/
// sweep recompute the aggregate via the shared computeLua tail.

// luaHeader binds now/stale from ARGV[1]/ARGV[2] for every script.
const luaHeader = `
local now   = tonumber(ARGV[1])
local stale = tonumber(ARGV[2])
`

// computeLua prunes stale connections, derives availability (online if any
// active connection, away if all inactive, offline if none), overlays the
// manual override (appear_offline -> offline always; other overrides apply only
// while live, else offline), CAS-writes the materialized status, and returns
// {changed(0/1), effective, nextDeadlineMs(-1 if none)}.
// KEYS[1]=conns hash  KEYS[2]=manual  KEYS[3]=status
const computeLua = `
local conns = redis.call('HGETALL', KEYS[1])
local anyLive, anyActive = false, false
local nextDeadline = -1
for i = 1, #conns, 2 do
  local field, val = conns[i], conns[i+1]
  local sep = string.find(val, ':')
  local away = string.sub(val, 1, sep - 1)
  local lastSeen = tonumber(string.sub(val, sep + 1))
  if now - lastSeen > stale then
    redis.call('HDEL', KEYS[1], field)
  else
    anyLive = true
    if away == '0' then anyActive = true end
    local d = lastSeen + stale
    if nextDeadline == -1 or d < nextDeadline then nextDeadline = d end
  end
end

local effective
if not anyLive then effective = 'offline'
elseif anyActive then effective = 'online'
else effective = 'away' end

local manual = redis.call('GET', KEYS[2])
if type(manual) == 'string' and manual ~= '' then
  if manual == 'appear_offline' then
    effective = 'offline'
  elseif anyLive then
    effective = manual
  else
    effective = 'offline'
  end
end

local prev = redis.call('GET', KEYS[3])
local changed = 0
if prev ~= effective then
  redis.call('SET', KEYS[3], effective)
  changed = 1
end
return {changed, effective, nextDeadline}
`

// pingScript refreshes a connection's lastSeen + TTL. For an already-known
// connection it skips recompute and returns changed=0 with an empty status —
// the caller never publishes on an unchanged ping, so it doesn't read the
// status, saving a GET round trip. Only a brand-new connection (the
// offline->online edge) runs the full recompute.
// ARGV[3]=connID  ARGV[4]=conns_ttl_ms
var pingScript = redis.NewScript(luaHeader + `
local connID = ARGV[3]
if redis.call('HEXISTS', KEYS[1], connID) == 1 then
  local v = redis.call('HGET', KEYS[1], connID)
  local sep = string.find(v, ':')
  local away = string.sub(v, 1, sep - 1)
  redis.call('HSET', KEYS[1], connID, away .. ':' .. now)
  redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[4]))
  return {0, '', now + stale}
end
redis.call('HSET', KEYS[1], connID, '0:' .. now)
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[4]))
` + computeLua)

// activityScript upserts a connection with the given away flag, then recomputes.
// ARGV[3]=connID  ARGV[4]=away01  ARGV[5]=conns_ttl_ms
var activityScript = redis.NewScript(luaHeader + `
redis.call('HSET', KEYS[1], ARGV[3], ARGV[4] .. ':' .. now)
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[5]))
` + computeLua)

// byeScript drops a connection, then recomputes (shared logic with sweep).
// ARGV[3]=connID
var byeScript = redis.NewScript(luaHeader + `
redis.call('HDEL', KEYS[1], ARGV[3])
` + computeLua)

// manualScript sets the manual override string, or clears it when ARGV[3] is
// the empty string, then recomputes.  ARGV[3]=status
var manualScript = redis.NewScript(luaHeader + `
if ARGV[3] == '' then
  redis.call('DEL', KEYS[2])
else
  redis.call('SET', KEYS[2], ARGV[3])
end
` + computeLua)

// sweepScript only prunes stale connections and recomputes (no mutation).
var sweepScript = redis.NewScript(luaHeader + computeLua)

type valkeyStore struct {
	c        *redis.ClusterClient
	staleMs  int64
	connsTTL int64 // ms
}

// ClusterConfig holds Valkey cluster connection config.
type ClusterConfig struct {
	Addrs    []string
	Password string
}

// NewValkeyStore dials the cluster, pings it, and returns a PresenceStore.
func NewValkeyStore(cfg ClusterConfig, staleThreshold, connsTTL time.Duration) (PresenceStore, error) {
	c := redis.NewClusterClient(&redis.ClusterOptions{Addrs: cfg.Addrs, Password: cfg.Password})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		if closeErr := c.Close(); closeErr != nil {
			slog.Warn("valkey cluster close after failed connect", "error", closeErr)
		}
		return nil, fmt.Errorf("valkey cluster connect: %w", err)
	}
	return newValkeyStoreFromClient(c, staleThreshold, connsTTL), nil
}

// newValkeyStoreFromClient wraps a pre-built client (tests inject a
// ClusterSlots-override client).
func newValkeyStoreFromClient(c *redis.ClusterClient, staleThreshold, connsTTL time.Duration) *valkeyStore {
	return &valkeyStore{c: c, staleMs: staleThreshold.Milliseconds(), connsTTL: connsTTL.Milliseconds()}
}

// run executes one presence script and parses its {changed, effective,
// nextDeadline} reply. now/stale are prepended as ARGV[1]/ARGV[2]; callers pass
// any op-specific args after.
func (s *valkeyStore) run(ctx context.Context, script *redis.Script, account string, now int64, args ...any) (bool, model.PresenceStatus, int64, error) {
	argv := append([]any{strconv.FormatInt(now, 10), strconv.FormatInt(s.staleMs, 10)}, args...)
	res, err := script.Run(ctx, s.c,
		[]string{connsKey(account), manualKey(account), statusKey(account)}, argv...,
	).Slice()
	if err != nil {
		return false, "", 0, fmt.Errorf("presence script %q: %w", account, err)
	}
	if len(res) != 3 {
		return false, "", 0, fmt.Errorf("presence script %q: unexpected result arity %d", account, len(res))
	}
	changed, _ := res[0].(int64)
	effective, _ := res[1].(string)
	nextDeadline, _ := res[2].(int64)
	return changed == 1, model.PresenceStatus(effective), nextDeadline, nil
}

// reschedule updates the sweep ZSET for an account based on its next deadline.
func (s *valkeyStore) reschedule(ctx context.Context, account string, nextDeadline int64) error {
	if nextDeadline < 0 {
		if err := s.c.ZRem(ctx, sweepKey, account).Err(); err != nil {
			return fmt.Errorf("sweep zrem %q: %w", account, err)
		}
		return nil
	}
	if err := s.c.ZAdd(ctx, sweepKey, redis.Z{Score: float64(nextDeadline), Member: account}).Err(); err != nil {
		return fmt.Errorf("sweep zadd %q: %w", account, err)
	}
	return nil
}

// mutate runs a mutating script and reschedules the account's sweep deadline.
func (s *valkeyStore) mutate(ctx context.Context, account string, script *redis.Script, args ...any) (bool, model.PresenceStatus, error) {
	now := time.Now().UTC().UnixMilli()
	changed, eff, next, err := s.run(ctx, script, account, now, args...)
	if err != nil {
		return false, "", err
	}
	if err := s.reschedule(ctx, account, next); err != nil {
		return false, "", err
	}
	return changed, eff, nil
}

func (s *valkeyStore) Ping(ctx context.Context, account, connID string) (bool, model.PresenceStatus, error) {
	return s.mutate(ctx, account, pingScript, connID, strconv.FormatInt(s.connsTTL, 10))
}

func (s *valkeyStore) SetActivity(ctx context.Context, account, connID string, away bool) (bool, model.PresenceStatus, error) {
	flag := "0"
	if away {
		flag = "1"
	}
	return s.mutate(ctx, account, activityScript, connID, flag, strconv.FormatInt(s.connsTTL, 10))
}

func (s *valkeyStore) RemoveConnection(ctx context.Context, account, connID string) (bool, model.PresenceStatus, error) {
	return s.mutate(ctx, account, byeScript, connID)
}

func (s *valkeyStore) SetManual(ctx context.Context, account string, status model.PresenceStatus) (bool, model.PresenceStatus, error) {
	// Stored as the plain status string (per review) — the script only needs the
	// status, so there is no JSON to decode in Lua.
	return s.mutate(ctx, account, manualScript, string(status))
}

func (s *valkeyStore) BatchGet(ctx context.Context, accounts []string) (map[string]model.PresenceStatus, error) {
	out := make(map[string]model.PresenceStatus, len(accounts))
	if len(accounts) == 0 {
		return out, nil
	}
	pipe := s.c.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(accounts))
	for _, a := range accounts {
		if _, seen := cmds[a]; seen {
			continue
		}
		cmds[a] = pipe.Get(ctx, statusKey(a))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("batch get: %w", err)
	}
	for a, cmd := range cmds {
		v, err := cmd.Result()
		if err == redis.Nil || v == "" {
			out[a] = model.StatusOffline
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("batch get %q: %w", a, err)
		}
		out[a] = model.PresenceStatus(v)
	}
	return out, nil
}

func (s *valkeyStore) Sweep(ctx context.Context, now time.Time) ([]StatusChange, error) {
	nowMs := now.UTC().UnixMilli()
	accounts, err := s.c.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key: sweepKey, ByScore: true, Start: "-inf", Stop: strconv.FormatInt(nowMs, 10), Offset: 0, Count: 500,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("sweep zrange: %w", err)
	}
	var changes []StatusChange
	for _, a := range accounts {
		changed, eff, next, rerr := s.run(ctx, sweepScript, a, nowMs)
		if rerr != nil {
			return changes, rerr
		}
		if rerr := s.reschedule(ctx, a, next); rerr != nil {
			return changes, rerr
		}
		if changed {
			changes = append(changes, StatusChange{Account: a, Effective: eff})
		}
	}
	return changes, nil
}

func (s *valkeyStore) Close() error { return s.c.Close() }
