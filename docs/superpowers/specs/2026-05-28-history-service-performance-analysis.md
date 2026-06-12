# History-Service Read Performance — Analysis

**Date:** 2026-05-28
**Author:** session (code-grounded analysis)
**Status:** Findings #2 and #4 implemented on `claude/history-service-performance-HklKj`. Finding #1 split out to its own PR on `claude/history-service-readcache-HklKj`. Findings #5 and #7 (token-aware) split out to `claude/history-service-cass-perf-HklKj`. See *Implementation status* below. #6 and #8 remain open; #3 deferred.

## Scope

- **Reads only.** The eight read/query handlers registered in `service.go:99` (`LoadHistory`, `LoadNextMessages`, `LoadSurroundingMessages`, `GetMessageByID`, `GetThreadMessages`, `GetThreadParentMessages`). The write paths (`EditMessage`, `DeleteMessage`, and the worker-side persist) are out of scope.
- **Method:** static reasoning from the code, the Cassandra/MongoDB access patterns, and round-trip counting. No live load test was run. Findings that hinge on real traffic distribution are flagged **measure-first**.
- **Driven by:** a proactive "where are the wins" pass, not a reported symptom.

## Summary

Ranked by effort-adjusted value. Gaps in numbering (`#3`) are intentional — see *Deferred*.

| Rank | # | Finding | Layer | Gain | Effort | Risk | Status |
|------|---|---------|-------|------|--------|------|--------|
| 1 | 1 | No caching of per-request Mongo reads | Mongo | High | Med | Med (access-check correctness) | ✅ Done (separate PR: `claude/history-service-readcache-HklKj`) |
| 2 | 7 | Cassandra client not token-aware / no compression | Driver (shared pkg) | Low–Med per read, repo-wide aggregate | Low | Low, but repo-wide blast radius | ✅ Done (separate PR: `claude/history-service-cass-perf-HklKj`; token-aware only, compression not pursued) |
| 3 | 4 | Per-row reflection rebuild in `structScan` | Cassandra scan | Low | Low | Low | ✅ Done |
| 4 | 2 | Access check serialized ahead of the parallel fan-out | Service | Low–Med (shrinks if #1 lands) | Low–Med | Low–Med | ✅ Done (LoadHistory, LoadNextMessages) |
| 5 | 5 | `GetMessagesByIDs` multi-partition `IN` | Cassandra | Low–Med | Med | Low–Med | ✅ Done (separate PR: `claude/history-service-cass-perf-HklKj`) |
| 6 | 6 | Offset pagination (`$skip` + per-page `$count`) | Mongo | Low (deep paging only) | Med (API change) | Low | Open (measure offsets) |
| 7 | 8 | Wide-row over-fetch (all 25 columns every read) | Cassandra | Low–Med | Low–Med | Low (API-constrained) | Open (API-constrained) |

## Implementation status (2026-05-28)

Implemented on `claude/history-service-performance-HklKj`, TDD throughout; `make lint` clean repo-wide and `make test` green under `-race`. Integration tests (testcontainers) not run locally — run in CI before merge.

| # | Commit / PR | Notes |
|---|-------------|-------|
| 4 | `a006d7e` | Per-type cql-tag→field-index map cached in a `sync.Map`. Benchmark on the full column set: 4813→908 ns/op, 5→2 allocs/op. |
| 2 | `e180d88` | `checkAccessAndRoomTimes` runs the access check and room-times resolve concurrently in LoadHistory/LoadNextMessages; access error keeps precedence. Cassandra-interleaved handlers intentionally keep the access check first. |
| 7 | separate PR — branch `claude/history-service-cass-perf-HklKj` | `cassutil` sets `TokenAwareHostPolicy(RoundRobinHostPolicy())`. Compression deliberately left out of scope. Repo-wide. |
| 5 | separate PR — branch `claude/history-service-cass-perf-HklKj` | `IN` replaced with bounded-concurrency token-aware point reads via a unit-tested `fetchByIDs`; input order preserved, missing omitted. |
| 1 | separate PR — branch `claude/history-service-readcache-HklKj` | New `readcache` (LRU+TTL+singleflight). Subscription cache positives-only, default 2m; room metadata default 10s (lastMsgAt volatility). Env-tunable, `size`/`ttl` 0 disables, `Stats()` exposed. |

**Cross-cutting prerequisite:** the service emits no DB-level performance instrumentation today (per-handler latency comes only from `natsrouter.Logging()`). Several findings can't be prioritized confidently without it — see *Instrument before optimizing*.

## The read hot path

Every read handler follows the same opening sequence before it touches message data:

1. **`getAccessSince`** (`utils.go:14`) → `subscriptions.GetHistorySharedSince` → a Mongo `FindOne({u.account, roomId})` projecting `historySharedSince` (`subscription.go:32`). Returns the access window (or "not subscribed"). **Serial, on the critical path** — the handler blocks on it before doing anything else.
2. **`resolveRoomTimesOrError`** (`room_times.go:21`) → resolves `(lastMsgAt, createdAt)` for the bucket-walk bounds. Uses client-supplied `meta` hints when present and sane; otherwise falls back to `rooms.GetRoomTimes` → `FindByID` projecting `{lastMsgAt, createdAt}` (`room.go:46`). A mixed/ inconsistent hint pair can trigger a **second** `GetRoomTimes` ("consistency refetch", `room_times.go:119`). **Serial** for the walk handlers.
3. **Message read + auxiliary reads**, then redaction (`redactUnavailableQuotes`).

### Per-request cost, by handler

"Mongo RTs" counts round-trips on the **critical path**; parenthetical reads run in parallel.

| Handler | Mongo RTs (no client hint) | Mongo RTs (valid hint) | Cassandra |
|---------|---------------------------|------------------------|-----------|
| `LoadHistory` | access + roomTimes serial, **+ minUserLastSeenAt in parallel** with the page read | access serial (+ minUserLastSeenAt parallel) | bucket walk (`GetMessagesBefore`/`BetweenDesc`) |
| `LoadNextMessages` | access + roomTimes (serial) | access (serial) | bucket walk (`GetMessagesAfter`/`AllAsc`) |
| `LoadSurroundingMessages` | access + roomTimes (serial) | access (serial) | `GetMessageByID` + **two parallel** walks (before/after) |
| `GetMessageByID` | access (serial) | access (serial) | single `GetMessageByID` |
| `GetThreadMessages` | access serial, **then** roomTimes ∥ `findMessage` (WaitGroup, `threads.go:51`) | same | single-partition thread slice |
| `GetThreadParentMessages` | access (serial) | access (serial) | `GetMessagesByIDs` (`IN`) hydration |

**The shape of the problem:** for the common `LoadHistory` with no hint, **two Mongo round-trips run in series** (`getAccessSince`, then `GetRoomTimes`) *before* the Cassandra page read even starts. `LoadHistory` already parallelizes the page read against `GetMinUserLastSeenAt` via `errgroup` (`messages.go:67`), but the two opening Mongo reads are not folded into that fan-out. These two reads are the cheapest, highest-confidence latency to remove.

## Findings

### #1 — No caching of per-request Mongo reads  ·  Mongo  ·  **High value**  ·  ✅ Implemented (separate PR — branch `claude/history-service-readcache-HklKj`)

**Observation.** None of the three per-request Mongo reads is cached. Each history read re-fetches from Mongo:
- `getAccessSince` — subscription lookup, **every** read (`utils.go:15`).
- `GetRoomTimes` — room `lastMsgAt`/`createdAt`, on every read without a valid client hint (`room_times.go:98`), occasionally twice (consistency refetch).
- `GetMinUserLastSeenAt` — room read receipt floor, every `LoadHistory` (`messages.go:84`).

History reads are a high-frequency, user-facing endpoint, so this is steady Mongo QPS spent re-reading slow-changing data.

**Precedent already in the repo.** `pkg/roommetacache` is a process-local **LRU + TTL + singleflight** cache (`roommetacache.go`), already used by `message-gatekeeper` and `broadcast-worker` to take room metadata off their per-message hot path. It exposes a generic `WrapStore` and hit/miss `Stats`, and is backed by the design rationale in `docs/superpowers/specs/2026-05-18-message-pipeline-mongo-caching-design.md`. `pkg/roomsubcache` (Valkey-backed, member-list shape) also exists but is currently **imported by nothing**. History-service uses neither.

**Per-field analysis** — these fields differ sharply in how safe they are to cache:

| Field | Volatility | Caching risk | Verdict |
|-------|-----------|--------------|---------|
| subscription `subscribed` / `historySharedSince` | changes on join/leave/role change | **Security-sensitive**: a stale "subscribed" lets a removed member keep reading; a stale window exposes the wrong history range | short TTL **+** event-driven invalidation on membership change; do not cache aggressively |
| room `createdAt` | immutable after creation | none | cache aggressively |
| room `lastMsgAt` | changes on every new message | a stale-low value sets the walk *ceiling* too low, hiding the very newest messages from page 1 — but those are exactly what real-time delivery / client hints already supply | cacheable with a short TTL; lowest-risk because clients pass hints and `clockSkewTolerance` exists |
| room `minUserLastSeenAt` | changes as members read | advisory hint surfaced to clients; failure is already non-fatal (`messages.go:84-90`) | cacheable with short TTL, safe |

**Expected gain.** On a cache hit, removes **1–2 serial Mongo round-trips from the critical path** (large p50/p99 win because they currently block the Cassandra read) and sharply cuts Mongo read QPS for the whole service.

**Options / tradeoffs.**
- **Reuse the `roommetacache` pattern** (process-local LRU+TTL+singleflight) for the room fields. Pro: zero network hop, proven in-repo. Con: per-instance — no cross-instance eviction, so invalidation is TTL-only.
- **Valkey-shared cache** (like `roomsubcache`) for the subscription/access entry, so invalidation events evict cluster-wide. Pro: coherent across instances. Con: adds a network hop (cheaper than Mongo, still non-zero).
- Pragmatic split: process-local LRU for `createdAt`/`lastMsgAt`/`minUserLastSeenAt`; a short-TTL + event-invalidated cache (Valkey or local) for the access check.

**Caveats.** The access check is the only security-sensitive one and dictates the design — get its TTL/invalidation right and the rest is mechanical. This is the single highest-value item.

### #7 — Cassandra client not token-aware / no compression  ·  shared driver pkg  ·  **Low–Med, broad reach**  ·  ✅ Implemented (separate PR — branch `claude/history-service-cass-perf-HklKj`; token-aware only, compression not pursued)

**Observation.** `cassutil.buildCluster` (`pkg/cassutil/cass.go`) sets keyspace, `LocalQuorum`, a 10 s timeout, and `NumConns`, but **no `HostSelectionPolicy`**. gocql therefore defaults to `RoundRobinHostPolicy` — **not token-aware**. Every query may land on a coordinator that is *not* a replica for the partition, which then forwards to a replica: an extra network hop and avoidable coordinator load on each request. No compressor is set either.

**Why it matters here.** Every history read is keyed on a single partition (`messages_by_room` by `(room_id, bucket)`, `messages_by_id` by `message_id`, `thread_messages_by_thread` by `thread_room_id`). Token-aware routing — `gocql.TokenAwareHostPolicy(gocql.RoundRobinHostPolicy())` — would send each of these straight to a replica, removing the coordinator hop.

**Expected gain.** Small per read (one fewer hop), but it applies to **every Cassandra read and write across every service** that uses `cassutil`, so the aggregate cluster-load reduction is the real prize. `SnappyCompressor` is a secondary lever for the wide message rows (CPU-for-bandwidth trade).

**Risk / caveats.** `cassutil` is a **shared package** — changing it touches `message-worker`, `search-sync-worker`, and others. Token-aware round-robin is strictly better for partition-keyed access, so risk is low, but the change should be validated across all consumers and is best owned as a deliberate shared-infra change rather than a history-service-local tweak.

### #4 — Per-row reflection rebuild in `structScan`  ·  Cassandra scan  ·  **Low, cheap**  ·  ✅ Implemented (`a006d7e`)

**Observation.** `structScan` (`utils.go:130`) calls `buildScanValues` (`utils.go:99`) **once per row**. `buildScanValues` reflects over all ~25–27 struct fields to rebuild the `cql`-tag → field map every single row. A 100-row page rebuilds that map 100 times.

**Expected gain.** Pure CPU/allocation reduction. Build the tag→field-index mapping **once per type** (e.g. a `sync.Map` keyed by `reflect.Type`) and per row only index into the fields. The win is real but small relative to network RTT — message reads are I/O-bound, not CPU-bound.

**Risk.** Minimal; localized to one helper, fully covered by `utils_test.go`. A good "while we're in here" cleanup, not a headline.

### #2 — Access check serialized ahead of the parallel fan-out  ·  Service  ·  **Low–Med (conditional)**  ·  ✅ Implemented (`e180d88`)

**Observation.** In every handler, `getAccessSince` runs and returns *before* any other work (`messages.go:30` precedes the `errgroup` at `:67`). The subscription round-trip is fully serial in front of room-times resolution and the Cassandra page read.

**Option.** Fold the access check into the parallel fan-out: launch the Cassandra page read, the access check, and the room reads concurrently, then **enforce authorization and redaction after** results return — discarding the message data if the caller turns out unauthorized.

**Tradeoffs.**
- Pro: removes the access-check RT from the critical path on the common (authorized) path.
- Con: performs a Cassandra read for callers who turn out unauthorized — wasted cluster work on the rare path, and a theoretical timing side-channel (the data itself is never returned).
- **Interaction with #1:** if caching lands, a cached access check is nearly free and no longer worth parallelizing. So #2 mainly matters either *instead of* #1 or for cache-miss latency. Don't pursue both for the same goal.

### #5 — `GetMessagesByIDs` multi-partition `IN`  ·  Cassandra  ·  **Low–Med (measure N)**  ·  ✅ Implemented (separate PR — branch `claude/history-service-cass-perf-HklKj`)

**Observation.** `GetMessagesByIDs` issues `... WHERE message_id IN ?` (`messages_by_id.go:33`). `message_id` is the partition key, so `IN` is a **scatter-gather across N partitions** funnelled through one coordinator. It's used by `GetThreadParentMessages` to hydrate thread parents (`threads.go:212`); N is the number of distinct thread rooms on the page (deduped), bounded by the thread-list page size (default 20, max 100).

**Expected gain.** For N≈20, modest. For N near 100 it's a 100-partition fan-out on a single coordinator. Alternative: bounded-concurrency **parallel single-partition point reads** (each token-aware, hitting its own replica), which spreads load off one coordinator.

**Confidence.** Measure-first: the value depends entirely on real thread-list page sizes. If lists are short, leave it.

### #6 — Offset pagination on thread lists  ·  Mongo  ·  **Low (deep paging only)**

**Observation.** `AggregatePaged` (`pkg/mongoutil/collection.go:74`) appends a `$facet` with `$skip`/`$limit` for the page **and a `$count` over the full match on every call**. Used for thread-parent lists (`GetThreadParentMessages`, `threads.go:178`). The compound indexes in `threadroom.go` (`EnsureIndexes`) back the match+sort, so shallow pages are index-served, but `$skip` is still O(offset) and the full re-`$count` is repeated per page.

**Expected gain.** Negligible for shallow offsets (the common case); degrades for rooms with many threads under deep paging. A keyset/range cursor on `(lastMsgAt, threadParentCreatedAt)` would remove the skip cost but is an **API change** (the current contract is offset-based, `req.Offset`). Computing `total` once rather than per page is a smaller independent saving.

**Confidence.** Measure-first: depends on observed offset depth. Likely low priority.

### #8 — Wide-row over-fetch  ·  Cassandra  ·  **Low–Med (API-constrained)**

**Observation.** Reads always `SELECT` the full column set — `baseColumns` is 25 columns (`messages_by_room.go:13`), and `messages_by_id` adds `pinned_at`/`pinned_by` (27). Several can be large: `msg` (up to the 20 KB content cap), `attachments`, `file`, `card`, the `reactions` MAP, `sys_msg_data`, `visible_to`. Every read pays the full deserialization + wire cost regardless of what the endpoint needs.

**Expected gain.** Projecting narrower column sets for endpoints with a genuinely narrower need would cut deserialization (compounding with #4) and payload size. **Constrained by the client API contract** — most endpoints return the full `Message` shape, so this only pays off where a specific path needs less (e.g. an existence/header check). Lowest priority; recorded as a lever, not a recommendation.

## Instrument before optimizing

The service emits **no DB-level performance signal** today — `cassrepo` has no spans or metrics (the walker's `walked` counter is a throwaway local), and per-handler latency comes only from `natsrouter.Logging()`. Before investing in the measure-first items (#5, #6, and the deferred #3), add:

- a **`buckets_walked` histogram** per read (unblocks #3),
- **per-call Mongo and Cassandra timing** (quantifies #1's payoff and #2's critical-path share),
- **cache hit/miss counters** if #1 lands (`roommetacache` already exposes `Stats`),
- **`IN` fan-out width / thread-list offset** distributions (size #5 and #6).

This is cheap, low-risk, and converts several "gut value" ratings into evidence.

## Recommended sequence

1. **Instrument** the DB calls (prerequisite, low effort). — _still open; readcache exposes `Stats()`, but DB-call timing / `buckets_walked` metrics are not yet wired._
2. ✅ **#1 caching** — done in separate PR (branch `claude/history-service-readcache-HklKj`).
3. ✅ **#7 token-aware driver** — done in separate PR (branch `claude/history-service-cass-perf-HklKj`). ✅ **#4 scan-map cache** — done (`a006d7e`).
4. ✅ **#2** done (`e180d88`); ✅ **#5** done in separate PR (branch `claude/history-service-cass-perf-HklKj`); **#6** still gated on offset-depth metrics.
5. **#8** only if a narrow-projection endpoint appears. — open.

## Deferred / future investigation

- **#3 — sequential bucket-walk read amplification.** `fillPage` (`walker.go:90`) queries one partition per bucket, strictly serially, up to `MESSAGE_READ_MAX_BUCKETS=122`. Sparse-but-active rooms and empty-range walks can cost many serial Cassandra round-trips for a single page (worst case ~122). Existing service-layer clamps (cap `before` at `lastMsgAt+1ms`; floor-clamp to `historyFloor`; reject out-of-range cursor buckets) already trim the common dead-room case. Candidate fixes — speculative parallel prefetch, a per-room non-empty-bucket index, adaptive bucket sizing — are non-trivial and interact with cursor semantics. **Not worth analyzing until the `buckets_walked` histogram shows how deep real reads actually walk.** Deferred at the team's direction.
