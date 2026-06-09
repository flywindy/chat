# Relax the X-Request-ID requirement for server-to-server RPCs

**Date:** 2026-06-08
**Scope:** `RoomsInfoBatch` (room-service) and `RoomCreateDMSync` (room-worker)
**Status:** Approved design — ready for implementation plan

## 1. Problem

`room-service` and `room-worker` both install the **strict** `natsrouter.RequireRequestID()`
middleware globally via `router.Use(...)`:

- `room-service/main.go:184` — `Recovery(), RequireRequestID(), Logging()`
- `room-worker/main.go:154` — `Recovery(), RequireRequestID(), Logging()`

The global middleware rejects **every** request whose `X-Request-ID` header is
missing or not a valid hyphenated UUID (`errcode.BadRequest`, reason
`RequestIDRequired`). That requirement was introduced for **dedup-critical**
handlers — those that derive a JetStream `Nats-Msg-Id` or a canonical message ID
from the request ID, where a server-side mint would break client-retry
deduplication (see `docs/error-handling.md` §3a).

Because the policy is applied service-wide, it is also forced onto
**server-to-server RPCs** that do not need it:

| Subject | Service | Why it doesn't need strict |
|---------|---------|----------------------------|
| `chat.server.request.room.{siteID}.info.batch` (`RoomsInfoBatch`) | room-service | Read-only batch lookup; request ID is logging-only. |
| `chat.server.request.room.{siteID}.create.dm` (`RoomCreateDMSync`) | room-worker | Writes are naturally idempotent; the cross-site outbox dedup can key on a deterministic payload seed instead of the request ID. |

Both subjects live in the `chat.server.request.…` namespace, which
`docs/client-api.md:58` explicitly places **outside** the client API surface.
Every caller of these RPCs is another backend service that today is forced to
forward a stable `X-Request-ID` purely to satisfy the remote gate.

**Goal:** these two RPCs accept calls without an `X-Request-ID` header (minting
one for logging when absent), while every user-facing, dedup-critical room-service
handler keeps the strict requirement.

## 2. Non-goals

- `RoomKeyEnsure` (`chat.server.request.room.{siteID}.key.ensure`) is also
  server-to-server but is **out of scope** for this change; it stays strict.
  (Future follow-up could relax it the same way.)
- room-worker's **async ROOMS-stream consume loop** (`room-worker/main.go:253`),
  its `messageDedupSeed` helper, and `processRoomRename`'s strict
  `X-Request-ID` check are a separate path — async member events published by
  room-service's still-strict user-facing handlers. **Untouched.** The
  "room-service rejects missing/malformed X-Request-ID at publish time" comment
  there remains accurate.
- No change to `OutboxDedupID` semantics for any other caller. Strict
  user-facing paths keep request-ID-derived dedup.
- No `docs/client-api.md` change (these subjects are not client-facing).

## 3. Approach

Two independent pieces, plus a small shared-library addition.

### 3.1 `pkg/natsrouter` — route groups (generic per-route middleware)

`natsrouter` currently supports only **global** middleware (`router.Use`); every
route registered via `addRoute` gets `r.middleware` prepended
(`pkg/natsrouter/router.go:164-168`). There is no way to exempt a single route
from a globally-installed middleware. We add a gin-style route group so a
**subset** of routes can carry additional middleware on top of the base chain.

- Introduce an unexported interface so the `Register*` helpers can target either
  a router or a group:

  ```go
  // registrar is satisfied by *Router and *Group (both in-package).
  type registrar interface {
      addRoute(pattern string, handlers []HandlerFunc)
  }
  ```

- Change the first parameter of `Register`, `RegisterNoBody`, and `RegisterVoid`
  from `r *Router` to `r registrar`. All existing call sites pass a `*Router`,
  which satisfies the interface — **no other call site changes**.

- Add the group type and constructor:

  ```go
  // Group carries middleware that runs after the base router's middleware
  // for every route registered on it. Created via (*Router).Group.
  type Group struct {
      router     *Router
      middleware []HandlerFunc
  }

  // Group returns a route group whose registered routes run mw after the
  // router's global middleware (and before the route handler).
  func (r *Router) Group(mw ...HandlerFunc) *Group {
      return &Group{router: r, middleware: mw}
  }

  func (g *Group) addRoute(pattern string, handlers []HandlerFunc) {
      all := make([]HandlerFunc, 0, len(g.middleware)+len(handlers))
      all = append(all, g.middleware...)
      all = append(all, handlers...)
      g.router.addRoute(pattern, all) // router prepends the global chain
  }
  ```

  Resulting handler chain for a grouped route:
  `global… + group… + handler`. For a base-router route: `global… + handler`.

Nested groups are not needed (YAGNI) — only `(*Router).Group` is added.

### 3.2 `room-service` — base mints, strict group keeps the requirement

`room-service/main.go`:

```go
router := natsrouter.New(nc, "room-service")
router.Use(natsrouter.Recovery(), natsrouter.RequestID(), natsrouter.Logging()) // mint baseline
handler.Register(router)
```

`room-service/handler.go` `Register(r *natsrouter.Router)`:

```go
func (h *Handler) Register(r *natsrouter.Router) {
    strict := r.Group(natsrouter.RequireRequestID())

    // All existing routes move onto the strict group, unchanged otherwise:
    natsrouter.RegisterNoBody(strict, subject.MuteTogglePattern(h.siteID), h.muteToggle)
    // ... every current route except RoomsInfoBatch ...
    natsrouter.Register(strict, subject.RoomCreatePattern(h.siteID), h.createRoom)

    // Server-to-server, mint-only (relaxed):
    natsrouter.Register(r, subject.RoomsInfoBatchSubscribe(h.siteID), h.roomsInfoBatch)
}
```

Effect:

- Every route still gets a request ID (minted when absent) via the base
  `RequestID()` — logging/tracing correlation is preserved everywhere.
- Only `RoomsInfoBatch` tolerates a missing/absent/malformed header.
- All user-facing dedup-critical routes still reject a missing header exactly as
  before (the strict group re-applies `RequireRequestID`, which inspects the
  inbound header regardless of the minted ctx value).

**Accepted minor behavior change:** with `RequireRequestID` now *inside*
`Logging` (it used to be outside), a rejected strict request emits **two** INFO
log lines — the `errcode.Classify` line from `errnats.Reply` (as before) **plus**
the standard `Logging` "nats request" line (new). This is negligible
(low-volume 4xx) and arguably an improvement (the rejected request now also
appears in the normal request log). It will be called out in the implementation
notes so reviewers expect it.

### 3.3 `room-worker` — global swap + payload-derived outbox dedup

The room-worker router registers a **single** route, `serverCreateDM`
(`room-worker/main.go:155`), so no group is needed — swap the global middleware:

```go
router.Use(natsrouter.Recovery(), natsrouter.RequestID(), natsrouter.Logging())
```

The handler itself has no internal request-ID gate (the old
`TestHandleSyncCreateDM_MissingRequestID` was retired), so the swap alone
removes the requirement. One safety change is required so minting cannot
reintroduce duplicate cross-site events:

`publishSyncDMOutbox` (`room-worker/handler.go:1905`) currently dedups via:

```go
payloadSeed := fmt.Sprintf("%s:%s:%d", room.ID, requester.Account, room.CreatedAt.UnixMilli())
... natsutil.OutboxDedupID(ctx, other.SiteID, payloadSeed)
```

`OutboxDedupID` *prefers* the ctx request ID and only falls back to
`payloadSeed` when ctx carries **no** request ID. Once we mint, ctx always
carries an ID, so the deterministic fallback would never fire and a caller
retrying without a stable request ID could emit duplicate `member_added` /
`room_created` outbox events.

Change this one call site to derive the dedup ID **directly** from the
deterministic payload seed, independent of the request ID:

```go
// Dedup keys on intrinsic room identity (stable across retries and re-subscribes),
// not on the request ID, so a minted/absent X-Request-ID can't break dedup.
dedupID := payloadSeed + ":" + other.SiteID
return h.publish(ctx, subject.Outbox(...), eData, dedupID)
```

`room.CreatedAt` is stable across retries: on a NATS redelivery / client retry,
`CreateRoom` returns a duplicate-key error and the reconcile path sets
`room = existing`, so `room.CreatedAt` is the original creation time. The seed is
therefore identical across attempts. (`OutboxDedupID` is unchanged for all other
callers.)

`publishSubscriptionUpdates` is verified during implementation to carry no
request-ID-derived `Nats-Msg-Id` (subscription-update events are
non-JetStream-deduped); the request ID there is logging-only.

### 3.4 Docs

`docs/error-handling.md` §3a ("Strict — reject missing/malformed"):

- Remove `room-worker.natsServerCreateDM` from the strict-callers list.
- Narrow "every handler in `room-service`" to "every **user-facing** handler in
  `room-service` (the strict route group); the `chat.server.*` `RoomsInfoBatch`
  read RPC mints instead."
- Note that `RoomCreateDMSync` now mints and its outbox dedup is
  payload-derived.

No `docs/client-api.md` change (server-internal subjects).

## 4. Testing (TDD — Red → Green → Refactor)

### `pkg/natsrouter` (new `Group`)
- Grouped route runs base + group middleware in order, then the handler.
- A route registered on `r.Group(RequireRequestID())` rejects a missing
  `X-Request-ID` (BadRequest, reason `RequestIDRequired`); a route on the base
  router (with `RequestID()` global) mints and passes for the same header-less
  message.
- `Register`, `RegisterNoBody`, and `RegisterVoid` all accept a `*Group`.

### `room-service`
- `RoomsInfoBatch` succeeds with **no** `X-Request-ID` header and returns the
  batch response.
- A strict route (e.g. `addMembers`) still rejects a missing header.
- Update `room-service/integration_test.go` where it builds a `RequireRequestID`
  router + sends a generated reqID for the batch path, to also cover the
  header-less case.

### `room-worker`
- `serverCreateDM` succeeds with **no** `X-Request-ID` header (mint path).
- Cross-site outbox dedup ID is **stable** across two calls that supply
  differing / absent request IDs (asserts payload-derived dedup) and **differs**
  for a different room/requester.
- Retire or update any test asserting the old router-level rejection for the
  DM-sync subject (the generic gate is covered by `pkg/natsrouter` tests).

All packages keep ≥80% coverage; run with `-race` via the Makefile.

## 5. Files touched

| File | Change |
|------|--------|
| `pkg/natsrouter/register.go` | `Register*` take `registrar` interface |
| `pkg/natsrouter/router.go` (or new `group.go`) | `registrar` interface, `Group` type, `(*Router).Group` |
| `pkg/natsrouter/group_test.go` (new) / `register_test.go` | Group + registrar tests |
| `room-service/main.go` | `RequireRequestID()` → `RequestID()` baseline |
| `room-service/handler.go` | strict group; `RoomsInfoBatch` on base router |
| `room-service/integration_test.go` / `handler_test.go` | header-less `RoomsInfoBatch` + strict-route rejection tests |
| `room-worker/main.go` | `RequireRequestID()` → `RequestID()` |
| `room-worker/handler.go` | `publishSyncDMOutbox` payload-derived dedup |
| `room-worker/handler_test.go` / `integration_test.go` | header-less DM-sync + payload-derived dedup tests |
| `docs/error-handling.md` | §3a strict-callers list update |

## 6. Risks & mitigations

- **Duplicate cross-site events on retry** → mitigated by payload-derived outbox
  dedup (§3.3); `room.CreatedAt` is stable across retries via the duplicate-key
  reconcile path.
- **Accidentally relaxing a dedup-critical room-service route** → the strict
  group keeps every existing route strict; only `RoomsInfoBatch` is moved to the
  base router. Tests assert a strict route still rejects.
- **`registrar` interface change breaks other natsrouter consumers** → the
  change is parameter-type-widening; every existing call passes `*Router`, which
  satisfies the interface. `search-service` / `history-service` registrations
  compile unchanged.
- **Double log line on rejected strict requests** → accepted, documented in §3.2.
