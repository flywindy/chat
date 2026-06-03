# Daily-IM Load Scenario — Find N

**Status:** Draft
**Owner:** hmchang
**Date:** 2026-05-27

## 1. Goal

Add a `loadgen daily` subcommand that simulates N users running the chat
system as their primary IM throughout a workday, ramps N geometrically,
and reports the largest N for which all SLO signals held over a sustained
hold window.

The output answers: *"How many concurrent daily-IM users can a single-site
deployment sustain before something breaks, and what breaks first?"*

## 2. Scope

**In scope (single-site only):**
- Message send + receive (frontdoor path through `message-gatekeeper`)
- History scrolling, room-list refresh, read-receipts, mentions
- Mute toggle, room create, member add, threaded replies
- Latency, error-rate, JetStream-pending, and service-error SLO signals
- Hybrid receiver: real `nats.Conn` per user up to a cap, multiplexed
  pool above the cap
- One-time JWT mint per user via `auth-service` at activation

**Out of scope (separate PRs):**
- Reconnect / presence storms (covered in a separate scenario PR)
- Cross-site federation (OUTBOX/INBOX) capacity
- All-hands rooms (>2k members)
- Per-message auth-service load
- CI regression gating — invoked manually, like existing `loadgen`

## 3. Failure Definition (what "breaks" means)

N is the largest step in the ramp where **none** of these tripped over
the hold window:

| Signal | Threshold | Source |
|---|---|---|
| `p95_latency_ms` (publish→receive) | > 500 | In-process histogram, correlated via `RoomEvent.LastMsgID` |
| `p99_latency_ms` | > 1000 | Same |
| `consumer_pending_growth` | end-of-hold pending > start + 1000 for any durable | NATS `/jsz?consumers=true`, polled every 5s |
| `error_rate` | > 0.1% of attempted ops | Failed publishes + `natsutil.ReplyError` 4xx/5xx + JetStream Nak/Term |
| `service_error_increase` | any counter delta > 0 | Prometheus scrape of each service's `/metrics` (`slog_errors_total`, `panic_total`) |

Durables watched are discovered at startup from `/jsz` (not hard-coded):
`message-worker`, `broadcast-worker`, `notification-worker`,
`inbox-worker`, `room-worker`, `search-sync-worker`.

SLO is evaluated over the **middle 60% of the hold window** to keep the
diurnal-envelope rate roughly stationary during measurement.

## 4. User Behavior Model

Each simulated user is a small state machine. A workday is compressed
into the hold window (default 180s = 3 min). Per-day action counts get
scaled by `holdSeconds / 28800` (8-hour workday) and dispatched as a
Poisson process under a diurnal envelope.

**Per-user-day budget (preset `daily-heavy`, headline):**

| Action | Per day | NATS subject / RPC |
|---|---|---|
| Send message (incl. ~⅓ threaded replies) | 60 | `chat.user.{acct}.room.{room}.{site}.msg.send` |
| Receive broadcast | derived (~2400/day at fan-out ~40) | subscribe to `chat.room.{room}.event.message` |
| Read receipt (one per room-visit) | 25 | `chat.user.{acct}.request.read-receipt` |
| Scroll history | 3 | `chat.user.{acct}.request.history.fetch` |
| Room-list refresh | 5 | `chat.user.{acct}.request.room.list` |
| Member add | 0.5 | `chat.user.{acct}.request.room.members.add` |
| Room create | 0.2 | `chat.user.{acct}.request.room.create` |
| Mute toggle | 0.2 | `chat.user.{acct}.request.mute.toggle` |

**Burstiness:** send actions cluster — when a user "fires," they emit
3–8 messages in a 20–60s burst, then go quiet. Implemented as a two-state
Markov chain (idle ↔ active) per user, transition probabilities chosen so
the stationary fraction of active users matches the diurnal envelope.

**Diurnal envelope:** `rateMultiplier(t) = 0.4 + 0.6 * peakShape(t)`,
where `peakShape` is two Gaussians centered at the 1/3 and 2/3 marks of
the hold window, normalized to peak at 1.0. Effect: rate is ~40% of mean
at the edges, ~150% mid-window.

**Presets:**

| preset | DMs | small (5–20) | medium (50–200) | large (500–2000) | rooms/user |
|---|---|---|---|---|---|
| `daily-light` | 15 | 10 | 5 | 2 | ~32 |
| `daily-heavy` | 25 | 20 | 8 | 3 | ~56 |
| `daily-power` | 40 | 30 | 10 | 3 | ~83 |

Room sizes within each band follow Zipf so the long tail is realistic.

## 5. Fixtures

Reuse the existing `loadgen seed` plumbing; add a new fixture builder
for the daily presets.

The seed step provisions:
- Users in MongoDB (`users` collection), IDs derived from
  `fnv(seed, "user", i)` — idempotent
- Rooms + memberships in MongoDB (`rooms`, `subscriptions`), same
  derivation
- Per-room AES-256-GCM key in Valkey (reuses `pkg/roomkeystore`, same
  as existing `loadgen seed`)
- Shared `backend.creds` for publishing (already in repo)

**Constraint:** the fixture set is sized for the *maximum* N in the
ramp. Each step **activates** a subset of pre-seeded users; we do not
re-seed between steps. Seed once at the start of a sweep, run the
full ramp, teardown at the end.

`loadgen teardown --preset=daily-heavy` drops the seeded MongoDB
collections and per-room Valkey keys, matching the existing `teardown`
shape.

## 6. Receiver Architecture (hybrid)

Two pools inside the loadgen process:

- **Direct pool** — first `--max-direct-users` users (default 20000).
  Each owns its own `nats.Conn` and per-room `Subscribe`. Realistic
  per-user connection cost.
- **Multiplex pool** — remaining users share a fixed-size pool of
  `--multiplex-pool-size` (default 200) connections. A dispatcher
  goroutine per shared conn routes incoming broadcasts to per-user
  inbox channels via a `roomID → []userID` map.

Users never move between pools mid-run.

**Latency correlation:** each broadcast carries `RoomEvent.LastMsgID`.
Publish records `messageID → publishTime` into a `sync.Map`; receive
reads-and-deletes, emits a latency sample. A TTL janitor evicts
entries older than 10s and caps the map at 1M entries (oldest evicted
on overflow). Anything not received within 10s counts toward
`error_rate`.

**Multiplex dispatcher backpressure:** non-blocking send to per-user
inbox channels — `select { case ch <- msg: default: drop+count }`.
Dropped messages count toward `error_rate`.

**Sharding ceiling:** at startup, loadgen computes the projected
connection count as `min(N_max, max_direct_users) + multiplex_pool_size`
and refuses to start if it exceeds `--max-conns-per-process`
(default 25000). With the defaults this allows N up to 100k+ in a
single process (20000 direct + 200 multiplex = 20200 conns regardless
of N). Multi-pod sharding (raising the user ceiling further by
splitting the user-ID space across pods) is a future PR.

## 7. Ramp Protocol

**Config (CLI flags):**

| Flag | Default | Notes |
|---|---|---|
| `--steps` | `1k,2k,5k,10k,20k,50k,100k` | Comma-sep N values, in order |
| `--warmup` | `60s` | Per-step ramp-up + settle; SLO not evaluated |
| `--hold` | `180s` | Per-step steady-state window; SLO evaluated over middle 60% |
| `--cooldown` | `30s` | Drain in-flight before next step |
| `--stop-on-trip` | `true` | Stop on first trip; final N = previous step |
| `--max-direct-users` | `20000` | Cap on direct-pool size |
| `--multiplex-pool-size` | `200` | Shared conns in multiplex pool |
| `--max-conns-per-process` | `25000` | Safety ceiling |

**Per-step lifecycle:**

1. **Warmup** — activate `N_step - N_prev` additional users at a
   rate-limited 500 users/sec (to avoid spinning up tens of thousands of
   goroutines instantly). Each new user picks pool (direct vs multiplex),
   mints its JWT (cached for the run), opens conn / registers interest,
   starts its state machine. SLO counters reset at end of warmup.
2. **Hold** — apply diurnal envelope to per-user rate. Collect latency
   samples. Poll `/jsz` every 5s. Scrape service `/metrics` every 15s.
3. **Evaluate** — compute verdict (Section 3). Append result to CSV.
4. If tripped and `--stop-on-trip`: report
   `N = previous-step` and stop. Else: cooldown, next step.

Users persist across steps — capacity planning asks "can we sustain N,"
not "can we onboard N from zero." This also avoids re-subscribe churn
dominating the warmup.

**Single-step mode:** `--steps=10000` runs one step. Useful for tighter
manual sweeps around the breakpoint after a coarse run.

## 8. Output

Per-step result struct (one row per step in CSV, also rendered to console):

```go
type StepResult struct {
    N                     int
    StartedAt             time.Time
    HoldDuration          time.Duration
    P50LatencyMs          float64
    P95LatencyMs          float64
    P99LatencyMs          float64
    ErrorRate             float64
    AttemptedOps          int64
    FailedOps             int64
    ConsumerPending       map[string]ConsumerPendingDelta // durable -> start/end/delta
    ServiceErrorIncreases map[string]int64                 // service -> delta
    LoadgenSelfMetrics    SelfMetrics                      // GC p99, goroutines, CPU%
    Tripped               bool
    Inconclusive          bool     // see Section 11 risks
    TrippedReasons        []string // e.g. "p95=612 > 500"
}
```

**Console summary at end of run:**

```
N        p50    p95    p99    err%    worst-pending-delta             verdict
1000     12     45     89     0.00%   broadcast-worker +12             PASS
2000     14     58     112    0.00%   broadcast-worker +34             PASS
5000     22     94     180    0.01%   broadcast-worker +180            PASS
10000    38     210    430    0.02%   broadcast-worker +890            PASS
20000    71     480    980    0.04%   broadcast-worker +1240           TRIP

ANSWER: N = 10000 (last passing step)
        Next limit: broadcast-worker consumer (pending growth)
```

**Artifacts:**
- `tools/loadgen/deploy/results/daily-<preset>-<timestamp>.csv` — one
  row per step
- Grafana dashboards (already wired in `tools/loadgen/deploy/`) cover
  live observation during the run

## 9. Implementation Layout

New files in `tools/loadgen/`, all `package main`:

| File | Purpose |
|---|---|
| `daily.go` | `runDaily(cfg dailyConfig) error` — top-level control loop |
| `daily_user.go` | `userState` + state machine (idle/active Markov, action picker) |
| `daily_pool.go` | `directPool` + `multiplexPool` + dispatcher routing |
| `daily_envelope.go` | Diurnal envelope (`rateMultiplier(elapsed, holdDuration) float64`) |
| `daily_actions.go` | One function per op: `sendMessage`, `scrollHistory`, `refreshRoomList`, `readReceipt`, `muteToggle`, `roomCreate`, `memberAdd` |
| `daily_seed.go` | Fixture builder for `daily-light` / `daily-heavy` / `daily-power` |
| `daily_verdict.go` | `evaluateStep(samples, durableState) StepResult` |
| `daily_report.go` | Console table + CSV emit |
| `*_test.go` | Unit tests per source file |
| `daily_integration_test.go` | One integration test: tiny preset (N=50) for 30s against testcontainers NATS+Mongo+Valkey, asserts a passing verdict |

**New subcommand wiring in `main.go`:**

```go
case "daily":
    cfg := parseDailyConfig(os.Args[2:])
    return runDaily(cfg)
```

**Reused without modification:**
- `tools/loadgen/seed.go`, `tools/loadgen/preset.go` (extended, not
  rewritten — `daily-light/heavy/power` join the existing
  `small/medium/large/realistic` set)
- `tools/loadgen/metrics.go` (latency histogram, error counters)
- `tools/loadgen/deploy/` Makefile + docker-compose overlay — one new
  target: `make run-daily PRESET=daily-heavy`
- `pkg/roomkeystore`, `pkg/subject`, `pkg/model`, `pkg/idgen`,
  `pkg/natsutil`

**TDD:** every action handler, the envelope, the verdict evaluator, and
the pool dispatcher are unit-tested as pure-ish functions. The control
loop and pool wiring are exercised by the single integration test.
Target ≥80% per CLAUDE.md.

## 10. Auth & Inject Path

- **Auth:** shared NATS `backend.creds` for publishing (existing
  loadgen pattern). Each simulated user mints one JWT via `auth-service`
  at activation, cached for the run. No per-message auth.
- **Inject:** frontdoor — publish to
  `chat.user.{acct}.room.{room}.{site}.msg.send` so `message-gatekeeper`
  validates. (The existing `--inject=canonical` shortcut is not exposed
  on `daily`; the whole point is to measure the full pipeline.)

## 11. Risks & Mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| Loadgen-as-bottleneck (CPU/GC on load box dominates measured latency) | High at N≥20k | Print loadgen self-metrics (GC pause p99, goroutine count, CPU%) per step. If GC pause > 50ms or CPU > 80% during hold, mark step **INCONCLUSIVE** instead of PASS/TRIP. |
| Memory blowup from latency correlation map | Medium | TTL janitor evicts entries > 10s old; hard cap at 1M entries; oldest evicted on overflow. |
| Fixture seed at N=100k taking minutes | High | Already idempotent — first run pays the cost, subsequent runs are no-ops. Document `make seed-daily-power` as one-time per environment. |
| Diurnal envelope makes per-step rate non-stationary | Medium | Evaluate SLO over middle 60% of hold (skip first 20% + last 20%). |
| Multiplex pool dispatcher contention | Medium | Per-shared-conn dispatcher goroutine, non-blocking send to per-user inbox channels; drops count toward `error_rate`. |
| Encryption (Valkey) overhead on receive | Low | Loadgen never decrypts — only reads `LastMsgID` from cleartext envelope, same as existing `loadgen run`. |
| Auth-service unintentionally in loop | Low | One JWT mint per user at activation, cached. |
| State pollution between runs | Medium | `loadgen teardown --preset=daily-heavy` drops Mongo collections and Valkey keys. |
| Hitting `--max-conns-per-process` ceiling | Low (only if operator raises `--max-direct-users` above the cap) | Hard-fail at startup with a clear error; multi-pod sharding is a future PR. |

## 12. Open Questions

None at design time. Implementation may surface tuning questions
(exact Markov transition probabilities, exact Zipf parameters for
room-size bands) which will be decided during plan execution and
documented in code comments where the constant is defined.

## 13. Success Criteria

1. `loadgen daily --preset=daily-heavy` runs to completion on a single
   developer box and produces a `StepResult` CSV + console summary.
2. The verdict logic correctly identifies a tripped step in the
   integration test (which injects a fault by capping the test NATS
   server's outbound bandwidth).
3. Coverage ≥ 80% per CLAUDE.md.
4. `make lint`, `make test`, `make test-integration SERVICE=loadgen`,
   `make sast` all pass.
5. A team member who has never seen the tool can run it from the
   README's quick-start section and get a number for N.
