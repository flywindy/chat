# user-presence-service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a `user-presence-service` that tracks per-user presence (online/away/busy/offline + manual override) and serves it cross-site over the NATS gateway, backed by per-site Valkey with a deterministic sweeper for offline/away decay.

**Architecture:** Each site owns its local users' presence in local Valkey. Clients heartbeat to `chat.user.{account}.event.presence.heartbeat`; the service maintains a per-account connections hash, computes an effective status via an atomic Lua recompute, and publishes changes to `chat.user.presence.{siteID}.state.{account}`. A sweeper goroutine drives time-based decay using a `presence:sweep` ZSET. Initial state is served by a batch request/reply (`chat.user.presence.{siteID}.query.batch`). Cross-site works because all users + services share the single `chatapp` NATS account that spans the gateway.

**Tech Stack:** Go 1.25, NATS (core pub/sub + `pkg/natsrouter` request/reply), Valkey cluster via `redis/go-redis/v9` + Lua, `caarlos0/env`, `log/slog`, `go.uber.org/mock`, `testify`, `testcontainers` via `pkg/testutil`.

Reference spec: `docs/superpowers/specs/2026-06-01-user-presence-service-design.md`.

---

## File Structure

| File | Responsibility |
|---|---|
| `pkg/model/presence.go` | `PresenceStatus`/`Activity` enums, `ManualStatus`, `Heartbeat`, `ByeRequest`, `ManualStatusRequest/Response`, `PresenceQuery`, `PresenceState`, `PresenceQueryResponse` |
| `pkg/subject/subject.go` (modify) | presence subject builders + natsrouter patterns |
| `auth-service/handler.go` (modify) | add presence pub/sub to signed JWT |
| `user-presence-service/store.go` | `PresenceStore` interface + `StatusChange` + mockgen directive |
| `user-presence-service/store_valkey.go` | Valkey impl: recompute Lua, conns hash, manual, status, sweep ZSET |
| `user-presence-service/handler.go` | heartbeat / bye / manual-set / batch-query handlers + publish-on-change |
| `user-presence-service/sweeper.go` | sweep loop |
| `user-presence-service/main.go` | config, wiring, route registration, sweeper start, graceful shutdown |
| `user-presence-service/*_test.go` | unit + integration tests |
| `user-presence-service/deploy/*` | Dockerfile, docker-compose.yml, azure-pipelines.yml |
| `docs/client-api.md` (modify) | new subjects + schemas |
| `docker-local/compose.services.yaml` (modify) | add service for `make up` |

---

## Task 1: Model types

**Files:**
- Create: `pkg/model/presence.go`
- Test: `pkg/model/model_test.go` (modify — add round-trip cases)

- [ ] **Step 1: Write failing round-trip test cases.** In `pkg/model/model_test.go`, add presence types to the existing `roundTrip` table (follow the file's existing pattern; one entry per type: `Heartbeat`, `ManualStatus`, `ManualStatusRequest`, `ManualStatusResponse`, `PresenceQuery`, `PresenceState`, `PresenceQueryResponse`).

- [ ] **Step 2: Run, verify fail.** Run: `make test SERVICE=pkg/model` (or `go test ./pkg/model/...`). Expected: compile failure (types undefined).

- [ ] **Step 3: Create `pkg/model/presence.go`:**

```go
package model

// PresenceStatus is a user's effective or manual presence value.
type PresenceStatus string

const (
	StatusOnline        PresenceStatus = "online"
	StatusAway          PresenceStatus = "away"
	StatusBusy          PresenceStatus = "busy"
	StatusOffline       PresenceStatus = "offline"
	StatusAppearOffline PresenceStatus = "appear_offline" // manual-only
	StatusNone          PresenceStatus = ""               // no manual override / clear
)

// Activity is a single connection's client-reported interaction state.
type Activity string

const (
	ActivityActive Activity = "active"
	ActivityIdle   Activity = "idle"
)

// Heartbeat is published by a client (fire-and-forget) to refresh a
// connection's liveness and report its current activity.
type Heartbeat struct {
	ConnID    string   `json:"connId"    bson:"connId"`
	Activity  Activity `json:"activity"  bson:"activity"`
	Timestamp int64    `json:"timestamp" bson:"timestamp"`
}

// ByeRequest is a best-effort client signal on disconnect (tab close).
type ByeRequest struct {
	ConnID    string `json:"connId"    bson:"connId"`
	Timestamp int64  `json:"timestamp" bson:"timestamp"`
}

// ManualStatusRequest sets or clears a user's manual override. Status ""
// (StatusNone) clears it.
type ManualStatusRequest struct {
	Status    PresenceStatus `json:"status"    bson:"status"`
	Timestamp int64          `json:"timestamp" bson:"timestamp"`
}

// ManualStatus is the stored manual override (Valkey value).
type ManualStatus struct {
	Account string         `json:"account" bson:"account"`
	Status  PresenceStatus `json:"status"  bson:"status"`
	SetAt   int64          `json:"setAt"   bson:"setAt"`
}

// ManualStatusResponse is the reply to a manual-set request.
type ManualStatusResponse struct {
	Account   string         `json:"account"   bson:"account"`
	Status    PresenceStatus `json:"status"    bson:"status"`
	SetAt     int64          `json:"setAt"     bson:"setAt"`
	Effective PresenceStatus `json:"effective" bson:"effective"`
}

// PresenceQuery is a batch initial-state request body.
type PresenceQuery struct {
	Accounts []string `json:"accounts" bson:"accounts"`
}

// PresenceState is one user's published effective status.
type PresenceState struct {
	Account   string         `json:"account"   bson:"account"`
	SiteID    string         `json:"siteId"    bson:"siteId"`
	Status    PresenceStatus `json:"status"    bson:"status"`
	Timestamp int64          `json:"timestamp" bson:"timestamp"`
}

// PresenceQueryResponse is the batch-query reply.
type PresenceQueryResponse struct {
	States    []PresenceState `json:"states"    bson:"states"`
	Timestamp int64           `json:"timestamp" bson:"timestamp"`
}
```

- [ ] **Step 4: Run, verify pass.** Run: `go test ./pkg/model/...`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add pkg/model/presence.go pkg/model/model_test.go
git commit -m "feat(model): add presence domain types"
```

---

## Task 2: Subject builders

**Files:**
- Modify: `pkg/subject/subject.go`
- Test: `pkg/subject/subject_test.go`

- [ ] **Step 1: Write failing tests.** In `subject_test.go`, assert exact strings:
```go
func TestPresenceSubjects(t *testing.T) {
	assert.Equal(t, "chat.user.{account}.event.presence.heartbeat", PresenceHeartbeatPattern())
	assert.Equal(t, "chat.user.{account}.event.presence.bye", PresenceByePattern())
	assert.Equal(t, "chat.user.{account}.request.presence.manual.set", PresenceManualSetPattern())
	assert.Equal(t, "chat.user.presence.site-a.query.batch", PresenceQueryBatch("site-a"))
	assert.Equal(t, "chat.user.presence.site-a.state.alice", PresenceState("site-a", "alice"))
}
```

- [ ] **Step 2: Run, verify fail.** Run: `go test ./pkg/subject/...`. Expected: undefined functions.

- [ ] **Step 3: Add builders** to `pkg/subject/subject.go` (near the other natsrouter patterns):
```go
// --- presence ---

// PresenceHeartbeatPattern is the natsrouter pattern the presence service
// registers for client heartbeats. {account} is extracted per request.
func PresenceHeartbeatPattern() string { return "chat.user.{account}.event.presence.heartbeat" }

// PresenceByePattern is the natsrouter pattern for best-effort disconnects.
func PresenceByePattern() string { return "chat.user.{account}.event.presence.bye" }

// PresenceManualSetPattern is the natsrouter pattern for manual-override set/clear.
func PresenceManualSetPattern() string { return "chat.user.{account}.request.presence.manual.set" }

// PresenceQueryBatch is the concrete (per-site, literal) subject the owning
// site's presence service subscribes to for batch initial-state queries.
func PresenceQueryBatch(siteID string) string {
	return fmt.Sprintf("chat.user.presence.%s.query.batch", siteID)
}

// PresenceState is the live-state subject the owning site publishes a user's
// effective status to; clients subscribe to it (possibly cross-site).
func PresenceState(siteID, account string) string {
	return fmt.Sprintf("chat.user.presence.%s.state.%s", siteID, account)
}
```

- [ ] **Step 4: Run, verify pass.** Run: `go test ./pkg/subject/...`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "feat(subject): add presence subject builders"
```

---

## Task 3: auth-service JWT presence permissions

**Files:**
- Modify: `auth-service/handler.go` (`signNATSJWT`)
- Test: `auth-service/handler_test.go`

- [ ] **Step 1: Write failing test.** Add a test asserting the signed JWT's permissions include presence read/query. Decode the JWT and assert `chat.user.presence.*.state.>` is in Sub.Allow and `chat.user.presence.*.query.>` is in Pub.Allow. (Follow existing JWT-decoding test patterns in `handler_test.go`; if none, parse via `jwt.DecodeUserClaims`.)

- [ ] **Step 2: Run, verify fail.** Run: `go test ./auth-service/...`. Expected: assertion fail.

- [ ] **Step 3: Add the two rules** in `signNATSJWT`, after the existing allows:
```go
	// Presence: read anyone's live state and publish batch queries (but never
	// publish state — the literal token "state" vs "query" keeps the query
	// pub-rule from matching the state subject, so clients can't forge presence).
	uc.Sub.Allow.Add("chat.user.presence.*.state.>")
	uc.Pub.Allow.Add("chat.user.presence.*.query.>")
```

- [ ] **Step 4: Run, verify pass.** Run: `go test ./auth-service/...`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add auth-service/handler.go auth-service/handler_test.go
git commit -m "feat(auth): grant clients presence read + query permissions"
```

---

## Task 4: Service skeleton — store interface + mocks

**Files:**
- Create: `user-presence-service/store.go`
- Create: `user-presence-service/doc.go` (package comment, optional)
- Generated: `user-presence-service/mock_store_test.go`

- [ ] **Step 1: Create `user-presence-service/store.go`:**
```go
package main

import (
	"context"
	"time"

	"github.com/hmchangw/chat/pkg/model"
)

//go:generate mockgen -destination=mock_store_test.go -package=main . PresenceStore

// StatusChange is an account whose effective status was (re)computed.
type StatusChange struct {
	Account   string
	Effective model.PresenceStatus
}

// PresenceStore is the Valkey-backed presence state. All mutating methods
// return whether the account's effective status changed (so the caller can
// publish only on change) plus the new effective status.
type PresenceStore interface {
	// RecordHeartbeat upserts a connection's activity + lastSeen, prunes stale
	// connections, recomputes effective status, and reschedules the account in
	// the sweep index.
	RecordHeartbeat(ctx context.Context, account, connID string, activity model.Activity) (changed bool, effective model.PresenceStatus, err error)

	// RemoveConnection drops a connection (graceful bye) and recomputes.
	RemoveConnection(ctx context.Context, account, connID string) (changed bool, effective model.PresenceStatus, err error)

	// SetManual sets (or clears, when status == StatusNone) the manual override
	// and recomputes. setAt is the millis the override was set.
	SetManual(ctx context.Context, account string, status model.PresenceStatus, setAt int64) (changed bool, effective model.PresenceStatus, err error)

	// BatchGet returns the materialized effective status for each account
	// (StatusOffline for accounts with no record).
	BatchGet(ctx context.Context, accounts []string) (map[string]model.PresenceStatus, error)

	// Sweep recomputes every account whose sweep deadline is <= now,
	// reschedules or removes it, and returns the accounts whose status changed.
	Sweep(ctx context.Context, now time.Time) ([]StatusChange, error)

	Close() error
}
```

- [ ] **Step 2: Generate mocks.** Run: `make generate SERVICE=user-presence-service`. Expected: `mock_store_test.go` created. (This requires the package to compile — `store.go` alone compiles since it only references stdlib + model.)

- [ ] **Step 3: Verify it built.** Run: `go vet ./user-presence-service/...`. Expected: no errors (mock present).

- [ ] **Step 4: Commit.**
```bash
git add user-presence-service/store.go user-presence-service/mock_store_test.go
git commit -m "feat(presence): add PresenceStore interface + generated mock"
```

---

## Task 5: Valkey store implementation (Lua recompute)

**Files:**
- Create: `user-presence-service/store_valkey.go`
- Test: `user-presence-service/integration_test.go` (+ `main_test.go` for TestMain)

This is the load-bearing component. Keys are `{account}`-hash-tagged so the
recompute Lua touches them in one slot. The sweep ZSET is a separate, per-site
global key.

- [ ] **Step 1: Create `user-presence-service/store_valkey.go`:**
```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hmchangw/chat/pkg/model"
)

const sweepKey = "presence:sweep"

func connsKey(account string) string  { return "presence:{" + account + "}:conns" }
func manualKey(account string) string { return "presence:{" + account + "}:manual" }
func statusKey(account string) string { return "presence:{" + account + "}:status" }

// recomputeScript optionally applies a mutation, prunes stale connections,
// derives availability + the manual override, compare-and-sets the
// materialized status, and returns {changed(0/1), effective, nextDeadlineMs(-1 if none)}.
//
// KEYS[1]=conns hash  KEYS[2]=manual  KEYS[3]=status
// ARGV[1]=now_ms  ARGV[2]=stale_ms  ARGV[3]=op(hb|bye|manual|recompute)
// ARGV[4]=arg1 (connID for hb/bye; manual JSON for manual)
// ARGV[5]=arg2 (activity for hb)  ARGV[6]=conns_ttl_ms
var recomputeScript = redis.NewScript(`
local connsKey, manualKey, statusKey = KEYS[1], KEYS[2], KEYS[3]
local now   = tonumber(ARGV[1])
local stale = tonumber(ARGV[2])
local op    = ARGV[3]

if op == 'hb' then
  redis.call('HSET', connsKey, ARGV[4], ARGV[5] .. ':' .. ARGV[1])
  redis.call('PEXPIRE', connsKey, tonumber(ARGV[6]))
elseif op == 'bye' then
  redis.call('HDEL', connsKey, ARGV[4])
elseif op == 'manual' then
  if ARGV[4] == '' then
    redis.call('DEL', manualKey)
  else
    redis.call('SET', manualKey, ARGV[4])
  end
end

local conns = redis.call('HGETALL', connsKey)
local anyLive, anyActive = false, false
local nextDeadline = -1
for i = 1, #conns, 2 do
  local field, val = conns[i], conns[i+1]
  local sep = string.find(val, ':')
  local activity = string.sub(val, 1, sep - 1)
  local lastSeen = tonumber(string.sub(val, sep + 1))
  if now - lastSeen > stale then
    redis.call('HDEL', connsKey, field)
  else
    anyLive = true
    if activity == 'active' then anyActive = true end
    local d = lastSeen + stale
    if nextDeadline == -1 or d < nextDeadline then nextDeadline = d end
  end
end

local effective
if not anyLive then effective = 'offline'
elseif anyActive then effective = 'online'
else effective = 'away' end

local manual = redis.call('GET', manualKey)
if type(manual) == 'string' then
  local ok, m = pcall(cjson.decode, manual)
  if ok and m.status and m.status ~= '' then
    if m.status == 'appear_offline' then
      effective = 'offline'
    elseif anyLive then
      effective = m.status
    else
      effective = 'offline'
    end
  end
end

local prev = redis.call('GET', statusKey)
local changed = 0
if prev ~= effective then
  redis.call('SET', statusKey, effective)
  changed = 1
end
return {changed, effective, nextDeadline}
`)

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

func (s *valkeyStore) recompute(ctx context.Context, account, op, arg1, arg2 string, now int64) (bool, model.PresenceStatus, int64, error) {
	res, err := recomputeScript.Run(ctx, s.c,
		[]string{connsKey(account), manualKey(account), statusKey(account)},
		strconv.FormatInt(now, 10), strconv.FormatInt(s.staleMs, 10), op, arg1, arg2, strconv.FormatInt(s.connsTTL, 10),
	).Slice()
	if err != nil {
		return false, "", 0, fmt.Errorf("recompute %q: %w", account, err)
	}
	changed := res[0].(int64) == 1
	effective := model.PresenceStatus(res[1].(string))
	nextDeadline := res[2].(int64)
	return changed, effective, nextDeadline, nil
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

func (s *valkeyStore) RecordHeartbeat(ctx context.Context, account, connID string, activity model.Activity) (bool, model.PresenceStatus, error) {
	now := time.Now().UTC().UnixMilli()
	changed, eff, next, err := s.recompute(ctx, account, "hb", connID, string(activity), now)
	if err != nil {
		return false, "", err
	}
	if err := s.reschedule(ctx, account, next); err != nil {
		return false, "", err
	}
	return changed, eff, nil
}

func (s *valkeyStore) RemoveConnection(ctx context.Context, account, connID string) (bool, model.PresenceStatus, error) {
	now := time.Now().UTC().UnixMilli()
	changed, eff, next, err := s.recompute(ctx, account, "bye", connID, "", now)
	if err != nil {
		return false, "", err
	}
	if err := s.reschedule(ctx, account, next); err != nil {
		return false, "", err
	}
	return changed, eff, nil
}

func (s *valkeyStore) SetManual(ctx context.Context, account string, status model.PresenceStatus, setAt int64) (bool, model.PresenceStatus, error) {
	now := time.Now().UTC().UnixMilli()
	var arg string
	if status != model.StatusNone {
		data, err := json.Marshal(model.ManualStatus{Account: account, Status: status, SetAt: setAt})
		if err != nil {
			return false, "", fmt.Errorf("marshal manual status: %w", err)
		}
		arg = string(data)
	}
	changed, eff, next, err := s.recompute(ctx, account, "manual", arg, "", now)
	if err != nil {
		return false, "", err
	}
	if err := s.reschedule(ctx, account, next); err != nil {
		return false, "", err
	}
	return changed, eff, nil
}

func (s *valkeyStore) BatchGet(ctx context.Context, accounts []string) (map[string]model.PresenceStatus, error) {
	out := make(map[string]model.PresenceStatus, len(accounts))
	if len(accounts) == 0 {
		return out, nil
	}
	pipe := s.c.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(accounts))
	for _, a := range accounts {
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
	accounts, err := s.c.ZRangeByScore(ctx, sweepKey, &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(nowMs, 10), Offset: 0, Count: 500,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("sweep zrangebyscore: %w", err)
	}
	var changes []StatusChange
	for _, a := range accounts {
		changed, eff, next, rerr := s.recompute(ctx, a, "recompute", "", "", nowMs)
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
```

- [ ] **Step 2: Create `user-presence-service/main_test.go`:**
```go
//go:build integration

package main

import (
	"testing"

	"github.com/hmchangw/chat/pkg/testutil"
)

func TestMain(m *testing.M) { testutil.RunTests(m) }
```

- [ ] **Step 3: Write failing integration test** `user-presence-service/integration_test.go`:
```go
//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/testutil"
)

func newTestStore(t *testing.T) *valkeyStore {
	t.Helper()
	c := testutil.StartValkeyCluster(t) // per-test: store owns Close via Close()
	return newValkeyStoreFromClient(c, 45*time.Second, 5*time.Minute)
}

func TestValkeyStore_HeartbeatLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// First active heartbeat: offline -> online (changed).
	changed, eff, err := st.RecordHeartbeat(ctx, "alice", "c1", model.ActivityActive)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, model.StatusOnline, eff)

	// Same activity again: no change.
	changed, _, err = st.RecordHeartbeat(ctx, "alice", "c1", model.ActivityActive)
	require.NoError(t, err)
	assert.False(t, changed)

	// Idle: online -> away.
	changed, eff, err = st.RecordHeartbeat(ctx, "alice", "c1", model.ActivityIdle)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, model.StatusAway, eff)

	// Bye: away -> offline.
	changed, eff, err = st.RemoveConnection(ctx, "alice", "c1")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, model.StatusOffline, eff)
}

func TestValkeyStore_ManualOverride(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, _, err := st.RecordHeartbeat(ctx, "bob", "c1", model.ActivityActive)
	require.NoError(t, err)

	// appear_offline wins over online.
	changed, eff, err := st.SetManual(ctx, "bob", model.StatusAppearOffline, time.Now().UnixMilli())
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, model.StatusOffline, eff)

	// Clear -> back to online.
	changed, eff, err = st.SetManual(ctx, "bob", model.StatusNone, time.Now().UnixMilli())
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, model.StatusOnline, eff)
}

func TestValkeyStore_SweepDecay(t *testing.T) {
	c := testutil.StartValkeyCluster(t)
	st := newValkeyStoreFromClient(c, 10*time.Millisecond, time.Minute) // tiny threshold
	ctx := context.Background()

	_, _, err := st.RecordHeartbeat(ctx, "carol", "c1", model.ActivityActive)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond) // let the connection go stale

	changes, err := st.Sweep(ctx, time.Now())
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "carol", changes[0].Account)
	assert.Equal(t, model.StatusOffline, changes[0].Effective)
}

func TestValkeyStore_BatchGet(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, _, err := st.RecordHeartbeat(ctx, "dave", "c1", model.ActivityActive)
	require.NoError(t, err)

	got, err := st.BatchGet(ctx, []string{"dave", "neverseen"})
	require.NoError(t, err)
	assert.Equal(t, model.StatusOnline, got["dave"])
	assert.Equal(t, model.StatusOffline, got["neverseen"])
}
```
> Note: tests use `StartValkeyCluster` (per-test) because the store calls
> `Close()` on the underlying client — per CLAUDE.md, the per-test mode is
> required when the wrapper owns `Close()`. We do not call `st.Close()` here
> (the `t.Cleanup` from `StartValkeyCluster` closes the client), so the wrapper
> is fine; if a test needs `Close()`, it still uses the per-test client.

- [ ] **Step 4: Run, verify it passes.** Run: `make test-integration SERVICE=user-presence-service`. Expected: PASS (requires Docker).

- [ ] **Step 5: Commit.**
```bash
git add user-presence-service/store_valkey.go user-presence-service/integration_test.go user-presence-service/main_test.go
git commit -m "feat(presence): Valkey store with atomic recompute Lua + sweep index"
```

---

## Task 6: Handler

**Files:**
- Create: `user-presence-service/handler.go`
- Test: `user-presence-service/handler_test.go`

- [ ] **Step 1: Create `user-presence-service/handler.go`:**
```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/pkg/subject"
)

// PublishFunc publishes data to a subject (core NATS).
type PublishFunc func(ctx context.Context, subj string, data []byte) error

// Handler holds presence dependencies.
type Handler struct {
	store    PresenceStore
	publish  PublishFunc
	siteID   string
	batchMax int
	now      func() time.Time
}

// NewHandler builds a Handler.
func NewHandler(store PresenceStore, publish PublishFunc, siteID string, batchMax int) *Handler {
	return &Handler{store: store, publish: publish, siteID: siteID, batchMax: batchMax, now: time.Now}
}

// publishState publishes a user's effective status to its state subject.
func (h *Handler) publishState(ctx context.Context, account string, status model.PresenceStatus) {
	if err := publishState(ctx, h.publish, h.siteID, account, status, h.now()); err != nil {
		slog.Error("publish presence state failed", "error", err, "account", account)
	}
}

// Heartbeat handles a fire-and-forget client heartbeat.
func (h *Handler) Heartbeat(c *natsrouter.Context, req model.Heartbeat) error {
	account := c.Param("account")
	if account == "" || req.ConnID == "" {
		return fmt.Errorf("heartbeat: missing account or connId")
	}
	activity := req.Activity
	if activity != model.ActivityActive && activity != model.ActivityIdle {
		activity = model.ActivityActive
	}
	changed, eff, err := h.store.RecordHeartbeat(c, account, req.ConnID, activity)
	if err != nil {
		return fmt.Errorf("record heartbeat: %w", err)
	}
	if changed {
		h.publishState(c, account, eff)
	}
	return nil
}

// Bye handles a best-effort disconnect.
func (h *Handler) Bye(c *natsrouter.Context, req model.ByeRequest) error {
	account := c.Param("account")
	if account == "" || req.ConnID == "" {
		return fmt.Errorf("bye: missing account or connId")
	}
	changed, eff, err := h.store.RemoveConnection(c, account, req.ConnID)
	if err != nil {
		return fmt.Errorf("remove connection: %w", err)
	}
	if changed {
		h.publishState(c, account, eff)
	}
	return nil
}

// SetManual sets or clears the manual override and returns the new state.
func (h *Handler) SetManual(c *natsrouter.Context, req model.ManualStatusRequest) (*model.ManualStatusResponse, error) {
	account := c.Param("account")
	if account == "" {
		return nil, natsrouter.ErrBadRequest("missing account")
	}
	switch req.Status {
	case model.StatusNone, model.StatusOnline, model.StatusAway, model.StatusBusy, model.StatusAppearOffline:
	default:
		return nil, natsrouter.ErrBadRequest("invalid manual status")
	}
	setAt := h.now().UTC().UnixMilli()
	changed, eff, err := h.store.SetManual(c, account, req.Status, setAt)
	if err != nil {
		return nil, fmt.Errorf("set manual: %w", err)
	}
	if changed {
		h.publishState(c, account, eff)
	}
	return &model.ManualStatusResponse{Account: account, Status: req.Status, SetAt: setAt, Effective: eff}, nil
}

// QueryBatch returns the current effective status for up to batchMax accounts.
func (h *Handler) QueryBatch(c *natsrouter.Context, req model.PresenceQuery) (*model.PresenceQueryResponse, error) {
	if len(req.Accounts) == 0 {
		return &model.PresenceQueryResponse{States: []model.PresenceState{}, Timestamp: h.now().UTC().UnixMilli()}, nil
	}
	if len(req.Accounts) > h.batchMax {
		return nil, natsrouter.ErrBadRequest(fmt.Sprintf("batch exceeds max of %d accounts", h.batchMax))
	}
	statuses, err := h.store.BatchGet(c, req.Accounts)
	if err != nil {
		return nil, fmt.Errorf("batch get: %w", err)
	}
	now := h.now().UTC().UnixMilli()
	states := make([]model.PresenceState, 0, len(statuses))
	for _, account := range req.Accounts {
		states = append(states, model.PresenceState{
			Account: account, SiteID: h.siteID, Status: statuses[account], Timestamp: now,
		})
	}
	return &model.PresenceQueryResponse{States: states, Timestamp: now}, nil
}

// publishState marshals + publishes a PresenceState. Package-level so the
// sweeper can reuse it.
func publishState(ctx context.Context, publish PublishFunc, siteID, account string, status model.PresenceStatus, now time.Time) error {
	st := model.PresenceState{Account: account, SiteID: siteID, Status: status, Timestamp: now.UTC().UnixMilli()}
	data, err := natsutil.MarshalResponse(st)
	if err != nil {
		return fmt.Errorf("marshal presence state: %w", err)
	}
	if err := publish(ctx, subject.PresenceState(siteID, account), data); err != nil {
		return fmt.Errorf("publish presence state: %w", err)
	}
	return nil
}
```
> `natsrouter.ErrBadRequest` returns a `*RouteError` that `replyErr` serializes
> to the client (verified in `pkg/natsrouter/errors.go` / `register.go`).

- [ ] **Step 2: Write `user-presence-service/handler_test.go`** (unit, mocked store; table-driven). Cover: heartbeat active→online publishes; heartbeat no-change suppresses publish; missing connID errors; bye publishes on change; manual invalid status → bad request; manual valid → response + publish; querybatch over limit → bad request; querybatch maps statuses (incl. missing→offline default from store map); querybatch empty → empty states. Use a fake publish capturing calls:
```go
package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
)

type capturedPublish struct{ subjects []string; payloads [][]byte }

func (c *capturedPublish) fn() PublishFunc {
	return func(_ context.Context, subj string, data []byte) error {
		c.subjects = append(c.subjects, subj)
		c.payloads = append(c.payloads, data)
		return nil
	}
}

func fixedNow() func() time.Time {
	t := time.Unix(1700000000, 0).UTC()
	return func() time.Time { return t }
}

func TestHandler_Heartbeat_PublishesOnChange(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockPresenceStore(ctrl)
	cap := &capturedPublish{}
	h := NewHandler(store, cap.fn(), "site-a", 100)
	h.now = fixedNow()

	store.EXPECT().RecordHeartbeat(gomock.Any(), "alice", "c1", model.ActivityActive).
		Return(true, model.StatusOnline, nil)

	c := natsrouter.NewContext(map[string]string{"account": "alice"})
	err := h.Heartbeat(c, model.Heartbeat{ConnID: "c1", Activity: model.ActivityActive})
	require.NoError(t, err)
	require.Len(t, cap.subjects, 1)
	assert.Equal(t, "chat.user.presence.site-a.state.alice", cap.subjects[0])
}

func TestHandler_Heartbeat_SuppressesWhenUnchanged(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockPresenceStore(ctrl)
	cap := &capturedPublish{}
	h := NewHandler(store, cap.fn(), "site-a", 100)
	store.EXPECT().RecordHeartbeat(gomock.Any(), "alice", "c1", model.ActivityActive).
		Return(false, model.StatusOnline, nil)
	c := natsrouter.NewContext(map[string]string{"account": "alice"})
	require.NoError(t, h.Heartbeat(c, model.Heartbeat{ConnID: "c1", Activity: model.ActivityActive}))
	assert.Empty(t, cap.subjects)
}

func TestHandler_QueryBatch_OverLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockPresenceStore(ctrl)
	h := NewHandler(store, (&capturedPublish{}).fn(), "site-a", 2)
	c := natsrouter.NewContext(nil)
	_, err := h.QueryBatch(c, model.PresenceQuery{Accounts: []string{"a", "b", "c"}})
	require.Error(t, err)
	var re *natsrouter.RouteError
	require.ErrorAs(t, err, &re)
}

// ... add: Bye publish-on-change, SetManual invalid + valid, QueryBatch mapping,
// missing-connId error, empty-accounts.
```

- [ ] **Step 3: Run, verify pass.** Run: `make test SERVICE=user-presence-service`. Expected: PASS.

- [ ] **Step 4: Commit.**
```bash
git add user-presence-service/handler.go user-presence-service/handler_test.go
git commit -m "feat(presence): heartbeat/bye/manual/query handlers with publish-on-change"
```

---

## Task 7: Sweeper

**Files:**
- Create: `user-presence-service/sweeper.go`
- Test: `user-presence-service/sweeper_test.go`

- [ ] **Step 1: Create `user-presence-service/sweeper.go`:**
```go
package main

import (
	"context"
	"log/slog"
	"time"
)

// Sweeper periodically recomputes accounts whose sweep deadline has passed
// and publishes any resulting status changes.
type Sweeper struct {
	store    PresenceStore
	publish  PublishFunc
	siteID   string
	interval time.Duration
	now      func() time.Time
}

// NewSweeper builds a Sweeper.
func NewSweeper(store PresenceStore, publish PublishFunc, siteID string, interval time.Duration) *Sweeper {
	return &Sweeper{store: store, publish: publish, siteID: siteID, interval: interval, now: time.Now}
}

// Run ticks until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.tick(ctx); err != nil {
				slog.Error("presence sweep failed", "error", err)
			}
		}
	}
}

// tick runs one sweep and publishes changes.
func (s *Sweeper) tick(ctx context.Context) error {
	changes, err := s.store.Sweep(ctx, s.now())
	if err != nil {
		return err
	}
	for _, ch := range changes {
		if perr := publishState(ctx, s.publish, s.siteID, ch.Account, ch.Effective, s.now()); perr != nil {
			slog.Error("publish swept state failed", "error", perr, "account", ch.Account)
		}
	}
	return nil
}
```

- [ ] **Step 2: Write `user-presence-service/sweeper_test.go`** (unit, mocked store): `tick` publishes one state event per returned change; `tick` surfaces store error; `tick` with no changes publishes nothing.
```go
package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/hmchangw/chat/pkg/model"
)

func TestSweeper_TickPublishesChanges(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockPresenceStore(ctrl)
	cap := &capturedPublish{}
	sw := NewSweeper(store, cap.fn(), "site-a", 0)
	sw.now = fixedNow()
	store.EXPECT().Sweep(gomock.Any(), gomock.Any()).
		Return([]StatusChange{{Account: "alice", Effective: model.StatusOffline}}, nil)
	require.NoError(t, sw.tick(context.Background()))
	require.Len(t, cap.subjects, 1)
	assert.Equal(t, "chat.user.presence.site-a.state.alice", cap.subjects[0])
}

func TestSweeper_TickSurfacesError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockPresenceStore(ctrl)
	sw := NewSweeper(store, (&capturedPublish{}).fn(), "site-a", 0)
	store.EXPECT().Sweep(gomock.Any(), gomock.Any()).Return(nil, errors.New("boom"))
	require.Error(t, sw.tick(context.Background()))
}
```

- [ ] **Step 3: Run, verify pass.** Run: `make test SERVICE=user-presence-service`. Expected: PASS.

- [ ] **Step 4: Commit.**
```bash
git add user-presence-service/sweeper.go user-presence-service/sweeper_test.go
git commit -m "feat(presence): sweeper for time-based offline/away decay"
```

---

## Task 8: main.go wiring

**Files:**
- Create: `user-presence-service/main.go`

- [ ] **Step 1: Create `user-presence-service/main.go`:**
```go
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/nats-io/nats.go"

	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/pkg/otelutil"
	"github.com/hmchangw/chat/pkg/shutdown"
	"github.com/hmchangw/chat/pkg/subject"
)

type NATSConfig struct {
	URL       string `env:"URL,required"`
	CredsFile string `env:"CREDS_FILE" envDefault:""`
}

type ValkeyConfig struct {
	Addrs    []string `env:"ADDRS,required" envSeparator:","`
	Password string   `env:"PASSWORD"        envDefault:""`
}

type PresenceConfig struct {
	BatchMax          int           `env:"BATCH_MAX"          envDefault:"100"`
	HeartbeatInterval time.Duration `env:"HEARTBEAT_INTERVAL" envDefault:"15s"`
	StaleThreshold    time.Duration `env:"STALE_THRESHOLD"    envDefault:"45s"`
	SweepInterval     time.Duration `env:"SWEEP_INTERVAL"     envDefault:"5s"`
	ConnsTTL          time.Duration `env:"CONNS_TTL"          envDefault:"5m"`
}

type Config struct {
	SiteID   string         `env:"SITE_ID,required"`
	NATS     NATSConfig     `envPrefix:"NATS_"`
	Valkey   ValkeyConfig   `envPrefix:"VALKEY_"`
	Presence PresenceConfig `envPrefix:"PRESENCE_"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := env.ParseAs[Config]()
	if err != nil {
		slog.Error("parse config", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	tracerShutdown, err := otelutil.InitTracer(ctx, "user-presence-service")
	if err != nil {
		slog.Error("init tracer failed", "error", err)
		os.Exit(1)
	}

	store, err := NewValkeyStore(
		ClusterConfig{Addrs: cfg.Valkey.Addrs, Password: cfg.Valkey.Password},
		cfg.Presence.StaleThreshold, cfg.Presence.ConnsTTL,
	)
	if err != nil {
		slog.Error("valkey connect failed", "error", err)
		os.Exit(1)
	}

	nc, err := natsutil.Connect(cfg.NATS.URL, cfg.NATS.CredsFile)
	if err != nil {
		slog.Error("nats connect failed", "error", err)
		os.Exit(1)
	}

	publish := func(ctx context.Context, subj string, data []byte) error {
		return nc.PublishMsg(ctx, natsutil.NewMsg(ctx, subj, data))
	}

	handler := NewHandler(store, publish, cfg.SiteID, cfg.Presence.BatchMax)

	router := natsrouter.Default(nc, "user-presence-service")
	natsrouter.RegisterVoid(router, subject.PresenceHeartbeatPattern(), handler.Heartbeat)
	natsrouter.RegisterVoid(router, subject.PresenceByePattern(), handler.Bye)
	natsrouter.Register(router, subject.PresenceManualSetPattern(), handler.SetManual)
	natsrouter.Register(router, subject.PresenceQueryBatch(cfg.SiteID), handler.QueryBatch)

	sweeper := NewSweeper(store, publish, cfg.SiteID, cfg.Presence.SweepInterval)
	sweepCtx, stopSweep := context.WithCancel(ctx)
	go sweeper.Run(sweepCtx)

	slog.Info("user-presence-service running", "site", cfg.SiteID, "valkey", cfg.Valkey.Addrs)

	shutdown.Wait(ctx, 25*time.Second,
		func(_ context.Context) error { stopSweep(); return nil },
		func(ctx context.Context) error { return router.Shutdown(ctx) },
		func(ctx context.Context) error { return nc.Drain() },
		func(_ context.Context) error { return store.Close() },
		func(ctx context.Context) error { return tracerShutdown(ctx) },
	)
}

var _ = nats.Header{} // keep nats import if unused after edits; remove if not needed
```
> Remove the trailing `var _ = nats.Header{}` line and the `nats` import if the
> build reports it unused (it is a guard for editing convenience only).

- [ ] **Step 2: Build.** Run: `make build SERVICE=user-presence-service`. Expected: binary at `bin/user-presence-service`, no errors. Fix any unused import.

- [ ] **Step 3: Full unit test + vet.** Run: `make test SERVICE=user-presence-service && go vet ./user-presence-service/...`. Expected: PASS.

- [ ] **Step 4: Commit.**
```bash
git add user-presence-service/main.go
git commit -m "feat(presence): wire config, routes, sweeper, graceful shutdown"
```

---

## Task 9: Deploy files

**Files:**
- Create: `user-presence-service/deploy/Dockerfile`
- Create: `user-presence-service/deploy/docker-compose.yml`
- Create: `user-presence-service/deploy/azure-pipelines.yml`

- [ ] **Step 1: Dockerfile** (mirror room-worker):
```dockerfile
FROM golang:1.25.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY pkg/ pkg/
COPY user-presence-service/ user-presence-service/
RUN CGO_ENABLED=0 go build -o /user-presence-service ./user-presence-service/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=builder /user-presence-service /user-presence-service
USER app
ENTRYPOINT ["/user-presence-service"]
```

- [ ] **Step 2: docker-compose.yml:**
```yaml
name: user-presence-service

services:
  user-presence-service:
    build:
      context: ../..
      dockerfile: user-presence-service/deploy/Dockerfile
    environment:
      - NATS_URL=nats://nats:4222
      - NATS_CREDS_FILE=/etc/nats/backend.creds
      - SITE_ID=site-local
      - VALKEY_ADDRS=valkey:6379
      - PRESENCE_HEARTBEAT_INTERVAL=15s
      - PRESENCE_STALE_THRESHOLD=45s
      - PRESENCE_SWEEP_INTERVAL=5s
    volumes:
      - ../../docker-local/backend.creds:/etc/nats/backend.creds:ro
    networks:
      - chat-local

networks:
  chat-local:
    external: true
```

- [ ] **Step 3: azure-pipelines.yml** (mirror room-service, swap `SERVICE_NAME: user-presence-service` and the `paths.include` entries to `user-presence-service/`).

- [ ] **Step 4: Commit.**
```bash
git add user-presence-service/deploy/
git commit -m "build(presence): add Dockerfile, compose, azure pipeline"
```

---

## Task 10: Docs + local compose registration

**Files:**
- Modify: `docs/client-api.md`
- Modify: `docker-local/compose.services.yaml`

- [ ] **Step 1: Update `docs/client-api.md`** — add a "Presence" section documenting:
  - Heartbeat publish subject + `Heartbeat` body (fire-and-forget, ~15s, `activity: active|idle`).
  - Bye publish subject + `ByeRequest` body.
  - Manual set request/reply subject + `ManualStatusRequest`/`ManualStatusResponse`, valid `status` values, error on invalid.
  - Batch query request/reply subject `chat.user.presence.{siteID}.query.batch` + `PresenceQuery` (≤100 accounts; error on overflow) → `PresenceQueryResponse`.
  - Live state subscription subject `chat.user.presence.{siteID}.state.{account}` + `PresenceState`.
  - The subscribe-before-snapshot ordering note.
  (Match the existing structure/style of `docs/client-api.md`.)

- [ ] **Step 2: Register in `docker-local/compose.services.yaml`** — add a `user-presence-service` service block consistent with the existing entries (same image build context, `NATS_URL`, `SITE_ID`, `VALKEY_ADDRS`, `backend.creds` mount, network). Mirror an existing Valkey-using service entry.

- [ ] **Step 3: Validate compose syntax.** Run: `docker compose -f docker-local/compose.services.yaml config -q`. Expected: no error. (If Docker unavailable, skip and note.)

- [ ] **Step 4: Commit.**
```bash
git add docs/client-api.md docker-local/compose.services.yaml
git commit -m "docs(presence): document client API + register local compose service"
```

---

## Task 11: Final verification

- [ ] **Step 1: Lint.** Run: `make lint`. Fix any findings.
- [ ] **Step 2: Format.** Run: `make fmt`.
- [ ] **Step 3: Full unit tests.** Run: `make test`. Expected: PASS.
- [ ] **Step 4: Integration tests (if Docker).** Run: `make test-integration SERVICE=user-presence-service`. Expected: PASS.
- [ ] **Step 5: SAST.** Run: `make sast`. Address medium+ findings (the Lua script + conversions are the likely flags; justify with `// #nosec` only for genuine false positives).
- [ ] **Step 6: Commit any lint/fmt fixups.**
```bash
git add -A
git commit -m "chore(presence): lint, format, sast fixups"
```
- [ ] **Step 7: Push.** `git push -u origin claude/ecstatic-meitner-SV87S` (retry with backoff on network error).

---

## Self-Review Notes

- **Spec coverage:** §3 cross-site (Task 3 JWT + literal per-site query subject), §4 status model (Task 5 Lua resolution + Task 1 enums), §5 subjects/permissions (Tasks 2,3), §6 Valkey model incl. `{account}` hash tags (Task 5), §7 sweeper (Tasks 5,7), §8 load (15s default Task 8), §9 service shape (Tasks 4–9), §10 config (Task 8), §11 error cases (Task 6), §12 testing (every task TDD).
- **Type consistency:** `PresenceStore` method signatures in Task 4 match call sites in Tasks 5–8; `PublishFunc`, `StatusChange`, `publishState` signatures consistent across handler/sweeper; subject strings identical between builder (Task 2) and assertions (Tasks 5,6,7).
- **Open items from spec §13:** `PresenceState` payload finalized as `{account, siteId, status, timestamp}` (no `isManual` in v1 — effective status already encodes the override); sweep ZSET member is the bare `account` (min-deadline scheduling, no delimiter needed); `docs/client-api.md` covered in Task 10.
