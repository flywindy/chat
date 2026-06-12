# Load test: large-room bot scaling (find max room size)

## Goal

Answer one operational question on a single site:

> **With a bot sending at a fixed RPS, what is the largest room size the
> system can sustain before a real signal breaks — and what breaks
> first?**

Motivated by a production pattern where bots frequently send messages to
rooms with hundreds to thousands of members. Reuses the existing
`tools/loadgen` binary, fixtures, metrics server, ramp/verdict skeleton
(from `daily.go` / `maxrps_*.go`), and Prometheus/Grafana overlay.

The ramp variable is **room size**; the bot **send rate is an operator
flag** (`--rate`). The scenario warms up, holds at each size step while a
bot sender drives `--rate` msgs/sec into rooms of that size, evaluates SLO
signals, and reports the largest size where everything held plus the first
signal that tripped.

## Background: where room size actually costs

Established by reading the pipeline (`broadcast-worker/handler.go`,
`notification-worker/handler.go`, `message-worker`, room-service). For
**channel** rooms the message hot path is *not* uniformly O(room size):

| Stage | Cost vs room size | Why |
|-------|-------------------|-----|
| broadcast-worker | **O(1)** | Publishes one event to `chat.room.{roomID}.event`; NATS core does delivery fan-out, not the worker. |
| NATS delivery | O(live conns) | Invisible unless the test holds N real subscribed member connections (deferred to v2). |
| notification-worker | **O(N)** | `GetMembers` + per-member filter loop + bulk presence RPC over all accounts + batched emits. The real "large room ⇒ more work per message" engine. |
| room-service reads | O(N) | member-list / read-receipt loads, bounded by `maxRoomSize`. |

So "large rooms kill the message path" here is primarily a
**notification-worker** story (server-side O(N) per message), which is
measurable *without* live member connections via its JetStream consumer
backlog.

## Single room vs multiple rooms

One bot hammering **one** room concentrates load onto single data-layer
keys that multiple rooms spread:

- **Cassandra hot partition.** `message-worker` writes `messages_by_room`,
  partition key `(room_id, bucket)`. One room's messages in a window land
  on one partition → one replica set absorbs every write. Pessimistic.
- **Mongo room-document contention.** `broadcast-worker.UpdateRoomLastMessage`
  writes the single room doc on every message → write contention. Pessimistic.

Conversely one room is **optimistic** on cache: `notification-worker`'s
member-list cache (roomsubcache) and `roommetacache` are room-keyed, so a
single room stays always-warm, hiding the cache churn a real multi-room
bot fleet causes.

The two layouts answer different questions, so room count is a knob,
`--rooms-per-size` (default **4**). The total `--rate` is split evenly
across the K rooms at each step. `--rooms-per-size=1` deliberately probes
the single-room hot-partition / hot-document limit.

## Non-goals (v1)

- **Pattern 2 — create-and-blast.** Dynamic room create → bulk-add ~100 →
  immediate cold-cache send. Deferred to v2 (see below).
- **Live N-connection NATS-delivery pool.** Measuring NATS core delivery
  fan-out to real member connections. Deferred to v2; the daily scenario's
  `directPool` / multiplex pool is the reuse path.
- **Cross-site federation** (OUTBOX / INBOX). Single site only.
- **CI regression gate.** Invoked manually for capacity work.
- **Auth benchmark.** Uses shared `backend.creds` like existing loadgen.
- **Cross-machine absolute-number comparisons.** Within-machine A/B only.

## Architecture

A new subcommand in the `daily` / `max-rps` family:

```
loadgen max-room-size --preset=<name> --rate=<rps> \
    [--sizes=100,500,1000,2000,5000] [--rooms-per-size=4] \
    [--reads=<rps>] [--warmup=60s] [--hold=180s] [--cooldown=30s] \
    [--stop-on-trip=true] [--seed=42] [--csv=<path>]
```

Reuses the existing NATS connect, Mongo/Valkey wiring, metrics server,
percentile/CSV writers, signal handling, consumer-lag sampler, and the
per-step ramp loop + verdict structure from `daily.go`.

### Step protocol (per size S in `--sizes`)

1. Select the K seeded rooms of size S (K = `--rooms-per-size`).
2. **Warmup** (`--warmup`): bot sender publishes at `--rate` split across
   the K rooms; samples discarded (`Collector.Reset` at hold start).
3. **Hold** (`--hold`): keep publishing at `--rate`; if `--reads>0`, a read
   driver issues room-service member-list / load-history reads against the
   same rooms at `--reads` rps. Poll JetStream consumer pending at hold
   start and end.
4. Evaluate SLO signals → `PASS` / `TRIP` / `INCONCLUSIVE`.
5. **Cooldown** (`--cooldown`): drain so consumers catch up before the next
   step.
6. `--stop-on-trip=true` stops the ramp at the first TRIP (not on
   INCONCLUSIVE).

Final ANSWER = largest size S with verdict PASS, plus the first failing
signal on the step that tripped.

## Load drivers

### Bot sender (headline)

- One (or a few) publisher connection(s) send channel messages via the
  normal front-door subject
  `chat.user.{account}.room.{roomID}.{siteID}.msg.send` at `--rate`,
  round-robin across the step's K rooms.
- The bot account is seeded as **room owner** of every room so it clears
  message-gatekeeper's `LargeRoomThreshold` restriction on large-room sends
  (the documented daily-scenario block). No re-config required.
- E2E latency is correlated via `RoomEvent.LastMsgID` on a single
  subscription per room (the existing messages-workload trick) — no live
  member pool needed.

### Read driver (optional, `--reads`, default 0)

- Low-rate room-service reads (member-list / load-history) against the same
  large rooms, so read p95/p99 vs size is visible. Covers the read-side
  concern. Off by default to keep the headline run focused on send fan-out.

## SLO signals and verdicts

A step verdict is `PASS`, `TRIP`, or `INCONCLUSIVE`.

**TRIP** if any of:

- **notification-worker consumer pending grew by > `--slo-pending-growth`
  (default 1000)** over the hold — the headline O(N) signal. **Unlike the
  daily scenario, notification-worker is NOT exempt here**: it is the thing
  this scenario hunts. broadcast-worker and message-worker durables are
  also gated on the same growth threshold.
- E2E publish→broadcast `p95 > --slo-p95` (default 500ms) or
  `p99 > --slo-p99` (default 1000ms) — correlated via `RoomEvent.LastMsgID`.
- room-service read `p95`/`p99` over the same thresholds — only when
  `--reads>0`.
- `error_rate > --slo-error-rate` (default 0.001) — failed publishes,
  request timeouts, gatekeeper 4xx/5xx.
- any gated durable that existed at hold-start was missing at hold-end
  (consumer crashed).

**INCONCLUSIVE** (overrides PASS/TRIP; verdict can't be trusted) when:

- loadgen GC pause p99 > 50ms (load box under pressure),
- achieved publish rate fell more than `--rate-tolerance` (default 0.05)
  below `--rate` while SLO signals looked healthy (generator was the limit),
- pending poll failed at hold start/end due to ctx cancel,
- `ctx.Done()` fired during warmup or hold.

**PASS** otherwise.

## Fixtures & presets

New `BotRoomPreset`, distinct from messages / members presets:

```go
type BotRoomPreset struct {
    Name     string
    Users    int   // shared user pool drawn from for memberships
    Sizes    []int // room sizes to seed (one set of --rooms-per-size rooms each)
    MaxRooms int   // rooms-per-size ceiling the preset seeds for
    BotAccount string // owner/sender seeded into every room
}
```

Seeding writes, for each size S and each of `MaxRooms` replicas:

- one `Room` doc (`RoomTypeChannel`, `UserCount=S`),
- the bot owner subscription (`RoleOwner`) + `S-1` member subscriptions
  drawn deterministically from the shared user pool, written **directly to
  Mongo** — this bypasses room-service so `MAX_ROOM_SIZE` does not block
  creation of a 5k-member room,
- one per-room key in Valkey (encryption default-on), derived from `--seed`.

Presets:

| preset           | users | sizes                   | rooms/size seeded | use case                    |
|------------------|-------|-------------------------|-------------------|-----------------------------|
| `botroom-small`  | 300   | 50, 100, 200            | 4                 | smoke / dev                 |
| `botroom-medium` | 5500  | 100, 500, 1000, 2000, 5000 | 4              | default capacity run        |

`--sizes` and `--rooms-per-size` at run time must be a subset of / not
exceed what the preset seeded; the command refuses to start otherwise and
names the seeded ceiling (mirrors the members-workload preflight guards).

The `seed` and `teardown` subcommands gain `botroom` as a `--workload`
value alongside `messages` / `members` / `history`.

## Companion change: lift the members add-path ceiling past 1000

Separable task in the same PR, covering the add-members-at-scale concern:

- Raise `MAX_ROOM_SIZE` in `tools/loadgen/deploy/docker-compose.yml`
  (room-service service) from 1000 to a value above the new max size
  (e.g. 6000) so the members-capacity workload can grow rooms past 1000.
- Add a `members-capacity-xl` preset (pool sized above 1000, e.g.
  baseline 1, candidate pool ~5500) so `members-capacity --target-size`
  can reach ~5000.

No change to `members_generator.go` logic — the existing
`ValidateCapacityTarget` preflight already adapts to the larger pool.

## Observability

New Prometheus metrics on the existing registry:

- `loadgen_botroom_published_total{preset,phase,size}`
- `loadgen_botroom_publish_errors_total{reason=publish|gatekeeper|timeout}`
- `loadgen_botroom_e2e_latency_seconds{size}` (publish→broadcast histogram)
- `loadgen_botroom_read_latency_seconds{size}` (when `--reads>0`)
- existing `loadgen_consumer_pending{stream,consumer}` reused, sampled
  against broadcast-worker, message-worker, **and notification-worker**
  durables.

Summary printer: a per-step table
(`size | rooms | rate | e2e_p50/p95/p99 | read_p95 | err% |
worst-pending-delta | verdict`) plus the final ANSWER / `Next limit:` line,
and a `BOTTLENECK:`-style note naming the worst durable. CSV: one row per
step.

## Error handling

Three classes, distinct labels on `loadgen_botroom_publish_errors_total`:

- `publish` — NATS / gatekeeper publish failure → log + count, continue.
- `gatekeeper` — gatekeeper rejected the send (e.g. not-owner, restricted)
  → log first 5 verbatim, count rest. Should be zero given the bot is owner;
  non-zero signals a seeding or config problem.
- `timeout` — E2E broadcast not seen within the request timeout → count,
  mark missing in the collector.

Ramp abort is fatal-but-clean: cancel the run context, drain in-flight,
print the partial summary. Reuses the existing `signal.NotifyContext`.

## Testing

Per CLAUDE.md TDD (Red → Green → Refactor → Commit, ≥80% coverage):

- `botroom_test.go` — size-ramp driver: step ordering, rate split across K
  rooms, rate adherence with a synthetic clock, `--sizes`/`--rooms-per-size`
  subset-of-preset validation.
- `botroom_verdict_test.go` — verdict table: notification-worker-pending
  gating fires the TRIP, INCONCLUSIVE guards (GC pause, rate shortfall),
  missing-durable trip, read-latency gate only active when `--reads>0`.
- `seed_botroom_test.go` — fixture determinism for a given seed; bot owner
  present on every room; per-size member counts exact; pools disjoint.
- `botroom_integration_test.go` (`//go:build integration`) — testcontainers
  via `pkg/testutil`: NATS+JS, Mongo, Valkey, real room-service /
  broadcast-worker / notification-worker. Seeds `botroom-small`, runs a
  short 2-step ramp at a low rate, asserts zero gatekeeper errors,
  non-empty E2E samples, and a sane verdict.
- Flag validation: `--rate>0`, `--rooms-per-size≥1`, `--sizes` non-empty
  and within the preset's seeded sizes.

## File layout

```
tools/loadgen/
  botroom.go               # BotRoomPreset, size-ramp driver, step protocol
  botroom_test.go
  botroom_verdict.go       # SLO signals, verdict, summary printer
  botroom_verdict_test.go
  botroom_integration_test.go
  seed_botroom.go          # fixture builder, Seed/Teardown integration
  seed_botroom_test.go
  main.go                  # +max-room-size subcommand; +botroom on --workload
  metrics.go               # +botroom_* metric families
  preset.go / members.go   # +botroom presets; +members-capacity-xl preset
  deploy/docker-compose.yml # MAX_ROOM_SIZE 1000 -> 6000
  deploy/Makefile          # +seed-botroom, run-max-room-size, teardown-botroom
  README.md                # +large-room bot scenario section
```

## v2 follow-ups (documented, not built)

1. **Pattern 2 — create-and-blast.** A churn driver: bots create a fresh
   ~100-member room (room-service create), bulk-add the members, and
   immediately send, measuring the cold-cache penalty (roommetacache /
   roomsubcache misses) and/or max sustainable creation rate.
2. **Live N-connection NATS-delivery pool.** Stand up real subscribed
   member connections (reuse daily's `directPool` / multiplex) so NATS core
   delivery fan-out to N connections is measured, not just server-side
   O(N) work.
</content>
</invoke>
