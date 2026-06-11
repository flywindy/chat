# user-presence-service — Design Spec

**Date:** 2026-06-01
**Status:** Approved for planning
**Branch:** `claude/ecstatic-meitner-SV87S`

## 1. Purpose

A new service that tracks and serves per-user **presence** — whether a user is
online, away, busy, or offline — across the federated multi-site chat system.
Clients query a user's current presence and subscribe to live changes. Each
site owns the presence of its local users; cross-site presence is served
directly over the NATS gateway (no replication).

## 2. Goals / Non-Goals

### Goals
- Track an **effective** presence status per account: `online`, `away`, `busy`, `offline`.
- Support a **manual override** (e.g. "busy", "appear offline") layered on top of
  connection-derived availability.
- Serve **initial state** (batch query, ≤ `PRESENCE_BATCH_MAX` accounts) and **live updates** (pub/sub).
- Work **cross-site**: a client on site A sees presence for a user homed on site B,
  directly, with no per-site mediation service.
- Detect offline/away reliably even when a client disconnects silently.

### Non-Goals (v1)
- **No durable manual status.** Manual override lives in Valkey only; loss on a
  Valkey flush/eviction is acceptable.
- **No `$SYS` connection-event integration.** NATS `$SYS` `CONNECT`/`DISCONNECT`
  events are a future optimization for the online/offline half (they cannot
  express "away"); v1 uses app-level heartbeats, which are self-contained and
  also yield "away".
- **No keyspace-notification (`__keyevent@*__:expired`) dependency.** Replaced by
  a deterministic sweeper (see §7). Keyspace-expiry's per-node, lossy, late
  behavior makes it unsuitable as a system of record.
- **No per-viewer presence filtering.** A user's effective status is global
  (one value broadcast to all watchers); `appear_offline` is applied uniformly
  before publish.
- **No cross-site presence replication** (no OUTBOX/INBOX). Presence is ephemeral
  pub/sub and rides the gateway directly.

## 3. Cross-Site Model (why this works)

The deployment runs a **single NATS account (`chatapp`)** for both end users and
backend services (`docker-local/setup.sh`: `auth-service` signs user JWTs with
the `chatapp` account key; the `backend` user has `--allow-pub/sub ">"`). Sites
are joined by **NATS gateways** (`nats-site*.conf`). NATS gateways propagate
subject interest across clusters **within an account**, and presence is
**ephemeral pub/sub** (it needs no persistence, unlike room messages which ride
OUTBOX→INBOX).

Consequences:
- A client on site A subscribing to `chat.user.presence.{siteB}.state.{account}`
  receives publishes from site B's presence-service directly over the gateway.
- A request/reply batch query addressed to a remote site's subject is routed to
  the owning site and replied back across the gateway (same mechanism
  `room-service`'s `memberlist_client` already uses).
- Each site's presence-service **owns its local users only**: their connection
  state in *local* Valkey, computes their effective status, publishes changes.
  No presence data crosses sites except as live pub/sub or query replies.

Since the user collection is identical on every site, a client always knows any
account's home `siteID` and addresses the owning site directly.

## 4. Status Model (orthogonal)

Two stored inputs resolve to one published **effective** status.

**Availability** (derived from per-connection activity the client reports via
`activity`):
- `online` — ≥ 1 live connection reporting active
- `away` — ≥ 1 live connection, all reporting inactive (`away`)
- `offline` — no live connections

**Manual override** (`presence:{account}:manual`, stored as the plain status
string): one of `none` · `online` · `away` · `busy` · `appear_offline`.
Lifetime: **persists until the user clears it** (v1 — no auto-expiry).

**Resolution → effective status.** Precedence ladder, first match wins (mirrors
the legacy client):

1. **No live connections → `offline`** — beats any manual override.
2. Manual `appear_offline` → `offline`; manual `away` → `away`.
3. Manual `online` → `online`; manual `busy` → `busy`.
4. Any other manual override → that status.
5. All live connections inactive → `away`.
6. Otherwise → `online`.

Rationale: an override only "shows" while the user is actually connected (you
cannot be `busy` with every tab closed); `appear_offline` always resolves to
`offline`. Consumers receive only the resolved effective status and never
perform resolution themselves.

## 5. NATS Subjects & Permissions

### Client → service (inbound; already covered by existing `chat.user.{self}.>` JWT scope)

| Purpose | Subject | Pattern |
|---|---|---|
| Hello — init a connection (fire-and-forget) | `chat.user.{account}.event.presence.{siteID}.hello` | service subscribes `chat.user.*.event.presence.{ownSiteID}.hello` (queue group) |
| Ping — liveness (fire-and-forget, ~15s) | `chat.user.{account}.event.presence.{siteID}.ping` | service subscribes `chat.user.*.event.presence.{ownSiteID}.ping` (queue group) |
| Activity — active/inactive (fire-and-forget) | `chat.user.{account}.event.presence.{siteID}.activity` | service subscribes `chat.user.*.event.presence.{ownSiteID}.activity` (queue group) |
| Graceful disconnect (best-effort, on tab close) | `chat.user.{account}.event.presence.{siteID}.bye` | service subscribes `chat.user.*.event.presence.{ownSiteID}.bye` (queue group) |
| Set / clear manual override (req/rep) | `chat.user.{account}.request.presence.{siteID}.manual.set` | service subscribes `chat.user.*.request.presence.{ownSiteID}.manual.set` (queue group) |

Writes are **self-namespaced** (`{account}` JWT-pinned), so a client can only
act as itself. They also carry the user's **home `{siteID}`**, so the message
routes to the presence service that owns the user's state regardless of which
site the client is connected to (rather than relying on queue-group locality).
The `connId` is **client-generated** per connection and reused across
hello/ping/activity/bye. `online` vs `away` is derived server-side by
aggregating each connection's last-reported activity (any active ⇒ online, all
inactive ⇒ away); ping is a pure liveness refresh and does not recompute except
when it first creates a connection.

### Service ↔ client (cross-user read; requires the one auth-service change)

| Purpose | Subject | Direction |
|---|---|---|
| Live state | `chat.user.presence.state.{account}` | service publishes; clients subscribe |
| Batch query (≤ `PRESENCE_BATCH_MAX` accounts) | `chat.user.presence.{siteID}.query.batch` | clients request; own local site replies + fans out to peers |

The live-state subject omits `siteID` — it is a global per-user broadcast, so a
watcher subscribes knowing only the account, without resolving the user's home
site first.

### auth-service change (the only client-permission change)
Add to the signed user JWT in `signNATSJWT`:
- **Sub-allow:** `chat.user.presence.state.*` — clients can subscribe to any user's live state.
- **Pub-allow:** `chat.user.presence.*.query.batch` — clients can publish batch queries.

Writes (hello/ping/activity/bye/manual) live under the user's own
`chat.user.{account}.>` namespace, already granted. Clients can **read** anyone's
presence but cannot **publish** state (the literal token `state` vs `query`
prevents the query pub-rule from matching the state subject). Replies arrive on
`_INBOX.>` (already allowed).

> Touches a client-facing auth surface → update `docs/client-api.md` in the
> implementation PR (request/response schema, error cases, events).

### Service subscriptions (backend creds, full perms)
- `chat.user.*.event.presence.{ownSiteID}.hello` (queue group `user-presence-service`)
- `chat.user.*.event.presence.{ownSiteID}.ping` (queue group)
- `chat.user.*.event.presence.{ownSiteID}.activity` (queue group)
- `chat.user.*.event.presence.{ownSiteID}.bye` (queue group)
- `chat.user.*.request.presence.{ownSiteID}.manual.set` (queue group)
- `chat.user.presence.{ownSiteID}.query.batch` (queue group) — client entry; resolves home sites and fans out
- `chat.server.request.presence.{ownSiteID}.query.batch` (queue group) — server-to-server fan-out leaf (local lookup only)

Publishes `chat.user.presence.state.{account}` on every effective-status change.

`pkg/subject` gains builder functions + wildcard patterns for all of the above
(no raw `fmt.Sprintf` at call sites). Account-token validators reject empty
tokens and NATS wildcards, consistent with existing builders.

### Subscription lifecycle (client contract)

A watcher holds a **long-lived** subscription to
`chat.user.presence.state.{account}` for each account it cares about, for as
long as it is interested (the session, or while a contact/room is in view). The
batch query (§5) is only the initial snapshot; the subscription carries every
change after that.

- **No server-side interest registry.** The presence-service never tracks who
  watches whom. On an effective-status change it publishes to the account's
  state subject; whoever is subscribed receives it. A publish with no
  subscribers is delivered to no one and is essentially free — so the service
  holds zero per-watcher state and does no per-watcher fan-out.
- **Interest-driven cross-site propagation.** When a watcher subscribes to a
  *remote* site's state subject, the NATS gateway propagates that interest to
  the owning site and the publishes flow back. Gateways are interest-aware: if
  no one remote is watching a given user, that user's publishes never cross the
  gateway — cross-site traffic exists only where there is real interest.
- **Subscribe-before-snapshot ordering.** The client subscribes to the state
  subject(s) *first*, then sends the batch query, so no transition can slip
  through the gap between snapshot and subscription.
- **Free cleanup.** Losing interest = unsubscribe; on disconnect NATS drops the
  client's subscriptions automatically. Nothing to clean up service-side.
- **Client-side scaling.** "Interested in N users" = N subscriptions on that
  client connection. For very large contact lists/rooms the frontend should
  subscribe only to currently-visible users and lean on the batch query
  on-demand for the rest. (Client concern, not a service one.)

## 6. Valkey Data Model

All per-account keys share a `{account}` **hash tag** so they land on one cluster
slot — required because the recompute touches them together in a single Lua
script (Lua is single-slot). The hash tag is **always `{account}`, never
`{connID}`** (tagging on connID would scatter an account's connections across
slots and break the atomic recompute).

| Key | Type | Contents | TTL |
|---|---|---|---|
| `presence:{account}:conns` | hash | field=`connID` → `"<active\|idle>:<lastSeenMs>"` | `PRESENCE_CONNS_TTL` (whole-key safety net, a few minutes) |
| `presence:{account}:manual` | string/json | `ManualStatus{Status, SetAt}` | none (persists until cleared) |
| `presence:{account}:status` | string/json | last-published effective status (for change-diff + fast reads) | none |
| `presence:sweep` | ZSET | member `{account}` → score = the account's next deadline (min over live conns of `lastSeenMs + staleThreshold`) | n/a (schedule index, not authoritative) |

Connections are kept in **one hash** (not separate `:conns:{connID}` keys):
recompute is a single `HGETALL` (Lua cannot `SCAN`), and per-key TTL is no longer
needed since the sweeper — not keyspace-expiry — drives decay.

A connection is **live** iff `now - lastSeenMs <= PRESENCE_STALE_THRESHOLD`
(computed staleness, not a Redis TTL).

## 7. Liveness & the Sweeper

Heartbeats are *positive* signals (new connection, `active`↔`idle`). The decisive
transitions are driven by the **absence** of signal (crash, network drop, lid
closed). The sweeper turns "no heartbeat for a while" into a deterministic event
without keyspace-notifications.

### On each heartbeat `(account, connID, activity)`

Atomic per-account Lua (slot `{account}`):

1. `HSET presence:{account}:conns connID "<activity>:<now>"`
2. Recompute effective status from the hash + `:manual`; compare to
   `presence:{account}:status`; write & return `changed` only if it differs.
3. Compute `nextDeadline` = min over the surviving live connections of
   `lastSeen + staleThreshold` (or `-1` when none remain), and return it.
4. If `changed`, publish `chat.user.presence.{site}.state.{account}`.

Then (outside the Lua, separate slot) reschedule the account in the sweep index:
`ZADD presence:sweep <nextDeadline> "{account}"`, or `ZREM presence:sweep "{account}"`
when `nextDeadline < 0`. The re-ADD overwrites the account's prior deadline.

> We re-push the deadline on **every** heartbeat so live accounts never enter
> the sweep's due-range — keeping sweep cost proportional to *churn*, not
> *population*.

### Sweep loop (every `PRESENCE_SWEEP_INTERVAL`, ~5s)

```text
now := nowMillis()
due := ZRANGEBYSCORE presence:sweep -inf now  (LIMIT 0 N)   // members are accounts
for each account in due:
    recompute(account)   // same atomic Lua: HGETALL, HDEL fields where now-lastSeen > threshold,
                         // read :manual, compare-and-set :status, return (changed, nextDeadline)
    reschedule(account)  // ZADD presence:sweep <nextDeadline> account, or ZREM when nextDeadline < 0
    if changed: publish chat.user.presence.{site}.state.{account}
```

An account that received a fresh heartbeat has already been `ZADD`-ed forward
(ZADD overwrites), so it won't be due — no false drop. When all of an account's
connections are stale, recompute `HDEL`s them and the aggregate flips
(`online`→`away`, or →`offline` if none remain live), and the account is removed
from the index.

### Fast paths (skip the sweeper)
- **Graceful `bye`**: `HDEL` that connID + immediate recompute → instant offline when it fires (best-effort; sweeper is the backstop).
- **`active`↔`idle`**: carried by the heartbeat itself → immediate recompute.

### Reliability properties
- **Absence detected by time, not by a notification that may never arrive.**
- **Self-healing across restarts:** the ZSET lives in Valkey; a restarted service resumes popping due members and reconciles. Nothing is lost.
- **Authoritative recompute:** the ZSET may drift from the hash harmlessly — recompute reads the hash (the truth); worst case is a slightly-late/redundant sweep.
- **HA-safe with no leader:** if multiple replicas sweep, the compare-and-set in the recompute Lua means only the replica that actually flips `:status` publishes; others compute the same value and stay quiet (duplicate *work*, never duplicate *events*).

### Overhead
- **+1 cheap `ZADD` per heartbeat** (O(log N)); ~6.7k ZADD/s at 100k users/15s — comfortable.
- **Sweep cost ∝ disconnect rate, not population** — live connections stay out of the due-range; an idle window is essentially one empty ranged read.
- **One concentration point:** `presence:sweep` is a single slot (all ZADDs + the sweep read). Fine at moderate scale; the scale lever is sharding into K `{shard}`-tagged ZSETs split across replicas (post-v1).

### Tuning knobs (config)
- `PRESENCE_HEARTBEAT_INTERVAL` — client contract, ~15s
- `PRESENCE_STALE_THRESHOLD` — ~45s (≈ 3× interval; tolerates 2 dropped pings)
- `PRESENCE_SWEEP_INTERVAL` — ~5s
- `PRESENCE_CONNS_TTL` — whole-hash safety-net TTL, a few minutes

Offline-detection latency ≈ `staleThreshold + sweepInterval` ≈ ~50s (typical for chat presence).

## 8. Heartbeat Rate / Load

- NATS transport is **not** the bottleneck (small messages; a server handles
  hundreds-of-thousands–millions/sec). 100k users @15s ≈ 6.7k msg/s.
- Real cost is per-heartbeat Valkey work + service CPU, bounded by **no-op
  publish suppression** (a steady-state heartbeat changes nothing → zero
  fan-out).
- **Client guidance:** a **SharedWorker** sharing one NATS connection + one
  heartbeat across a browser's tabs collapses N-tab amplification and makes
  `active` = "any tab active". (Frontend concern; noted, not built here.)

## 9. Service Shape (follows repo conventions)

Flat `user-presence-service/` at repo root:

| File | Responsibility |
|---|---|
| `main.go` | `caarlos0/env` config; `natsutil.Connect`; `valkeyutil.ConnectCluster`; OTel tracer/meter; subscriptions; start sweeper goroutine; `shutdown.Wait` |
| `handler.go` | heartbeat / manual-set / bye / batch-query handlers; effective-status resolution; publish-on-change |
| `sweeper.go` | sweep loop + scheduling |
| `store.go` | `PresenceStore` interface + `//go:generate mockgen` directive |
| `store_valkey.go` | Valkey impl: recompute Lua, conns hash, manual, status, sweep ZSET |
| `handler_test.go` | unit tests, mocked store (table-driven; happy/error/edge) |
| `integration_test.go` | `//go:build integration`; `testutil.SharedValkeyCluster` + `testutil.NATS`; `TestMain` → `testutil.RunTests` |
| `mock_store_test.go` | generated (never hand-edited) |
| `deploy/Dockerfile` | multi-stage `golang:1.25.10-alpine` → `alpine:3.21`, non-root |
| `deploy/docker-compose.yml` | local dev wiring (NATS creds, Valkey addrs) |
| `deploy/azure-pipelines.yml` | CI/CD |

Notes:
- **Consumer pattern:** heartbeats/queries use **core-NATS `QueueSubscribe`**
  (ephemeral, not JetStream — presence is not persisted). No streams, so no
  `bootstrap.go`.
- **Graceful shutdown order:** stop sweeper → `nc.Drain()` → close Valkey →
  flush OTel.
- **`pkg/model`** gains `PresenceStatus` (typed enum), `ManualStatus`,
  `Heartbeat`, `PresenceState`, and a `PresenceEvent` wrapper with the required
  `Timestamp int64 json:"timestamp"` (ms) convention; all model structs carry
  both `json` and `bson` tags. Added to `pkg/model/model_test.go` round-trip.
- **No new third-party dependencies** (`redis/go-redis/v9` and NATS libs already present).
- **Logging:** `log/slog` JSON; request/correlation ID extracted from NATS
  headers and propagated via `context.Context`; never log full presence payloads
  beyond status + account.

## 10. Config (env)

| Var | Default | Notes |
|---|---|---|
| `NATS_URL` | `nats://localhost:4222` | |
| `NATS_CREDS_FILE` | `""` | backend creds |
| `SITE_ID` | `site-local` | owning site |
| `VALKEY_ADDRS` | — (**required**) | cluster seed nodes, csv |
| `VALKEY_PASSWORD` | `""` | |
| `PRESENCE_BATCH_MAX` | `100` | max accounts per batch query |
| `PRESENCE_HEARTBEAT_INTERVAL` | `15s` | client contract (informational/served to clients) |
| `PRESENCE_STALE_THRESHOLD` | `45s` | liveness window |
| `PRESENCE_SWEEP_INTERVAL` | `5s` | sweep tick |
| `PRESENCE_CONNS_TTL` | `5m` | whole-hash safety-net TTL |

`SCREAMING_SNAKE_CASE`; required values (e.g. `VALKEY_ADDRS`) fail fast at
startup; non-critical values get `envDefault`.

## 11. Error Handling & Edge Cases

- Batch query > `PRESENCE_BATCH_MAX` → `natsutil.ReplyError` with a sanitized
  `model.ErrorResponse` ("batch exceeds max of N accounts"); no truncation.
- Unknown / never-seen account in a query → return `offline` (default), not an error.
- Valkey errors → wrap with context (`fmt.Errorf("...: %w", err)`), surface via
  OTel metrics; never leak raw internal errors to clients.
- `appear_offline` with live connections → effective `offline`, but connections
  are still tracked and swept so clearing the override reflects real availability
  immediately.
- Manual override referencing an account with no connections → effective
  `offline` (override only shows while connected, except it's moot here).
- Duplicate sweeps across replicas → idempotent via compare-and-set publish guard.

## 12. Testing (TDD, per CLAUDE.md)

- **Red→Green→Refactor** for every unit; tests precede implementation.
- **Unit** (`handler_test.go`): table-driven over the resolution matrix (every
  row of §4), heartbeat active/idle transitions, manual set/clear, batch-size
  validation, query of unknown accounts, publish-on-change vs suppression.
  Inject the publish function as a field so tests capture output with no real
  NATS. Mock `PresenceStore`.
- **Integration** (`integration_test.go`, `//go:build integration`): real
  Valkey via `testutil.SharedValkeyCluster` (+ `t.Cleanup(FlushValkey)`) and
  `testutil.NATS`; exercise the recompute Lua, sweeper decay (online→away→offline
  by withholding heartbeats), and cross-account hash-tag colocation. `TestMain`
  → `testutil.RunTests`.
- **Coverage:** ≥ 80% floor, target 90%+ on handler + store; cover error paths
  and boundaries.

## 13. Open Items (carry into the plan)

- Exact `PresenceEvent` payload fields beyond `{account, siteID, status, since,
  timestamp}` (include an `isManual` flag?) — finalize during planning.
- ~~Sweep ZSET member delimiter~~ — resolved: members are the bare `account`
  scored by the account's next deadline (min over live conns), so no delimiter
  or per-connection member is needed.
- `docs/client-api.md` update content (new subjects, batch schema, manual-set
  request/reply, error cases) — produced with the implementation PR.
