# room-service (+ room-worker RPC) natsrouter Migration

## Summary

Move every NATS **request/reply RPC** in `room-service` (20) and the one in
`room-worker` (`natsServerCreateDM`) off raw `nc.QueueSubscribe` + hand-written
wrappers onto `pkg/natsrouter`. This unblocks per-message concurrency (today each
subscription dispatches serially) and centralizes marshal/unmarshal, error
replies, panic recovery, request-ID, and logging — matching `search-service` /
`history-service`.

Transport + plumbing swap only: subjects, request/response JSON, error
envelopes, and business semantics are preserved. room-worker's JetStream
consumer is **out of scope** (correct pattern, already error-model-conformant).

## Goals

- All request/reply RPCs registered through `natsrouter` (typed handlers, auto
  marshal/unmarshal, centralized error reply).
- Per-message concurrency replacing serial dispatch.
- Handlers conform to `pkg/errcode`: named-constructor errors with a `reason`
  where the frontend branches, raw `fmt.Errorf` for infra, no log-and-return.
- Strict request-ID preserved (reject missing/invalid — never mint).

## Non-Goals

- No change to room-worker's JetStream consumer (`HandleJetStreamMsg`; already
  uses `errcode.Permanent`/`IsPermanent` → `Ack`/`Nak`).
- No change to subject **values**, request/response **schemas**, federation
  payloads, or business logic.
- No admission-control cap (see Decisions).

## Background

### Current (room-service)

`RegisterCRUD(nc)` does `nc.QueueSubscribe(subj, "room-service", h.natsXxx)` ×18.
Each `natsXxx(m otelnats.Msg)`:
1. `wrappedCtx(m)` — strict request-ID via `natsutil.RequireRequestID`.
2. `handleXxx(ctx, m.Msg.Subject, m.Msg.Data)` — core parses subject + unmarshals body.
3. Error → `errnats.Reply`; success → `m.Msg.Respond` / `natsutil.ReplyJSON`.

nats.go dispatches each subscription's callback serially → every RPC is a
per-subject bottleneck.

### Target (typed `Register`; middleware per `natsrouter.Default()`)

```go
router := natsrouter.New(nc, "room-service")
router.Use(natsrouter.Recovery(), natsrouter.RequireRequestID(), natsrouter.Logging())
handler.Register(router)
// shutdown: router.Shutdown(ctx) before nc.Drain()
```

Handlers become `func (h *Handler) xxx(c *natsrouter.Context, req ReqT) (*RespT, error)`.
`Register` unmarshals, calls the handler, replies via `c.ReplyJSON` /
`errnats.Reply`, one goroutine per message.

`search-service` is the reference for the typed `Register` shape only; it uses
the **minting** `RequestID()` outermost (`search-service/main.go:168-170`).
room-service needs **strict** `RequireRequestID()` with `Recovery` outermost, per
`natsrouter.Default()` (`router.go:106`) / history-service.

## Decisions (confirmed)

1. **Strict request-ID** as a reusable `pkg/natsrouter` middleware
   `RequireRequestID()`, mirroring the minting `RequestID()`.
2. **Subject params via `{name}` Pattern builders + `c.Param`**, consistent with
   the other natsrouter services.
3. **Unbounded concurrency** (`natsrouter.New`, no `WithMaxConcurrency`), matching
   `search-service`; per-handler timeout is the backpressure lever (§6.4).
4. **Scope includes room-worker's single RPC** (`natsServerCreateDM`); JetStream
   consumer untouched.

## Design

### 6.1 `pkg/natsrouter` — `RequireRequestID()` middleware

New in `middleware.go`, alongside `RequestID()`:

```go
// Strict X-Request-ID: reject missing/non-UUID with BadRequest+Abort; never mint.
func RequireRequestID() HandlerFunc {
    return func(c *Context) {
        var headers nats.Header
        var subj string
        if c.Msg != nil {
            headers, subj = c.Msg.Header, c.Msg.Subject
        }
        ctx, id, err := natsutil.RequireRequestID(c.ctx, headers, subj)
        if err != nil {
            // c.Msg is set in production; guard the nil-Msg unit-test context.
            if c.Msg != nil {
                errnats.Reply(c, c.Msg, err) // BadRequest, reason RequestIDRequired
            }
            c.Abort()
            return
        }
        c.Set(requestIDKey, id)
        c.SetContext(ctx)
        c.WithLogValues("request_id", id)
        c.Next()
    }
}
```

Tests (`middleware_test.go`): valid UUID passes + stamps id; missing/invalid
replies BadRequest + aborts.

### 6.2 `pkg/subject` — Pattern builders

Add a `{name}`-placeholder `*Pattern` builder for each of the 17 wildcard
room-service subjects, each rendering the **same** concrete subject as its
`*Wildcard` sibling (which stays for publishers). Example:

```go
// → chat.user.{account}.request.room.{roomID}.<siteID>.member.role-update
func MemberRoleUpdatePattern(siteID string) string { ... }
```

Each builder names every variable token (`{account}`, `{roomID}`, `{orgID}` …);
`{siteID}` stays concrete. Each gets a `subject_test.go` case. The 3 concrete
subjects (`RoomsInfoBatchSubscribe`, `RoomKeyEnsure`, `RoomRestricted`) and
room-worker's `RoomCreateDMSync` need no builder.

### 6.3 `pkg/model` — typed status replies + rename request

Replace ad-hoc `map[string]string{"status":…}` returns with typed structs
(CLAUDE.md: no bare maps):

```go
type StatusReply struct {
    Status string `json:"status"` // "ok" | "accepted"
}
type StatusWithRequestReply struct {
    Status    string `json:"status"`
    RequestID string `json:"requestId"`
}
```

Add `RoomRenameRequest` (rename uses an inline `struct{ NewName string }`;
`Register` needs a named type):

```go
type RoomRenameRequest struct {
    NewName string `json:"newName"`
}
```

`NewName`-only (no server-ignored `RoomID`), keeping the body byte-identical.
Status replies are content-identical to today (JSON key order isn't part of the
contract). Add `model_test.go` round-trips.

### 6.4 `room-service/main.go` wiring

- Middleware order `Recovery` → `RequireRequestID` → `Logging`, per
  `natsrouter.Default()` (`router.go:106`) / history-service (not search-service,
  which puts `RequestID` outermost). `Logging` reads `request_id` on success; on
  reject `RequireRequestID` aborts, so the rejection is logged once by
  `errnats.Reply` → `Classify`, not `Logging`. room-service is NATS-only (no
  HTTP/`/metrics`/`/healthz`).
  ```go
  router := natsrouter.New(nc, "room-service")
  router.Use(natsrouter.Recovery(), natsrouter.RequireRequestID(), natsrouter.Logging())
  router.Use(natsrouter.HandlerTimeout(cfg.RequestTimeout)) // see below
  handler.Register(router)
  ```
- **Recommended:** `HandlerTimeout(cfg.RequestTimeout)` via new `REQUEST_TIMEOUT`
  env (default `10s`) — the backpressure lever under unbounded concurrency. (Open
  for confirmation.)
- Replace `handler.RegisterCRUD(nc)` with `handler.Register(router)`.
- Add `router.Shutdown(ctx)` as the first shutdown hook, before `nc.Drain()`.

### 6.5 `room-service/handler.go` conversion

Delete `RegisterCRUD`, the 20 `natsXxx`/`NatsHandleXxx` wrappers, and
`wrappedCtx`. Add:

```go
func (h *Handler) Register(r *natsrouter.Router) {
    natsrouter.Register(r, subject.RoomCreatePattern(h.siteID), h.createRoom)
    natsrouter.RegisterNoBody(r, subject.MuteTogglePattern(h.siteID), h.muteToggle)
    // … 20 total …
}
```

Three flavors (see inventory):
- **`Register[Req,Resp]`** — router unmarshals; handler reads params via
  `c.Param`, returns `(*Resp, error)`.
- **`RegisterNoBody[Resp]`** — body-less; params via `c.Param`.
- **`RegisterNoBody[Resp]` + manual optional unmarshal** — `listMembers` /
  `getRoomKey` tolerate an empty body today; `Register` rejects empty bytes (a
  `{}` is fine). Keep the `if len(c.Msg.Data) > 0 { json.Unmarshal(...) }` guard.

Cores reshape from `handleXxx(ctx, subj, data)` to (params, typed req).
Subject-parse error branches disappear (a matched subscription always yields
params). `requestId`-echoing responses read it via
`natsutil.RequestIDFromContext(c.Context())`.

**Observability parity:** spans (`trace.SpanFromContext`) and metrics
(`roomkeymetrics.*.Add`) live in the preserved cores
(`handler.go:365,380,1099,1359,1904,1996,2126,2179,2195`); the consumer span
flows via `m.Context()` → `acquireContext` (`router.go:211`) as today. Guardrail:
thread the natsrouter `*Context` / `c.Context()` into cores — never a bare ctx,
or spans vanish.

### 6.6 `room-worker` — `natsServerCreateDM`

Add a small router for the one RPC (JS consumer unchanged):

```go
router := natsrouter.New(nc, "room-worker")
router.Use(natsrouter.Recovery(), natsrouter.RequireRequestID(), natsrouter.Logging())
natsrouter.Register(router, subject.RoomCreateDMSync(cfg.SiteID), handler.serverCreateDM)
```

Add `router.Shutdown(ctx)` before `nc.Drain()` (after the JS iterator stop +
`wg.Wait()`). Delete `natsServerCreateDM` + `requireDedupRequestID` (middleware
replaces it). Reshape `handleSyncCreateDM(ctx, data)` → `serverCreateDM(c, req)
(*model.SyncCreateDMReply, error)`. Concrete subject → no builder / `c.Param`.
Already collision/retry-safe, so concurrent dispatch is fine.

### 6.7 Error-propagation conformance pass

Handlers mostly already return `errcode.*` with reasons + raw `fmt.Errorf` for
infra. The migration removes manual `errnats.Reply` sites. Audit each handler for:
- Client-error-as-internal bugs fixed for free: `roomRename` / `roomRestricted`
  return `fmt.Errorf("invalid request: %w", err)` on a bad body today (→
  `internal`); the router now replies `BadRequest`.
- No `slog.*` before returning the same error (router classifies+logs once).
- `WithCause` only on infra errors, never raw client payloads.

## Per-RPC inventory

| # | RPC | Pattern/subject builder | Flavor | Request | Response | Params |
|---|-----|------------------------|--------|---------|----------|--------|
| 1 | create | `RoomCreatePattern` | Register | `CreateRoomRequest` | `CreateRoomReply` | account |
| 2 | rooms-info-batch | `RoomsInfoBatchSubscribe` (concrete) | Register | `RoomsInfoBatchRequest` | `RoomsInfoBatchResponse` | — |
| 3 | role-update | `MemberRoleUpdatePattern` | Register | `UpdateRoleRequest` | `StatusReply{ok}` | account, roomID |
| 4 | remove-member | `MemberRemovePattern` | Register | `RemoveMemberRequest` | `StatusReply{accepted}` | account, roomID |
| 5 | add-members | `MemberAddPattern` | Register | `AddMembersRequest` | `StatusReply{accepted}` | account, roomID |
| 6 | message-read | `MessageReadPattern` | NoBody | — | `StatusReply{accepted}` | account, roomID |
| 7 | read-receipt | `MessageReadReceiptPattern` | Register | `ReadReceiptRequest` | `ReadReceiptResponse` | account, roomID |
| 8 | thread-read | `MessageThreadReadPattern` | Register | `MessageThreadReadRequest` | `StatusReply{accepted}` | account, roomID |
| 9 | list-members | `MemberListPattern` | NoBody + optional body | `ListRoomMembersRequest` (optional) | `ListRoomMembersResponse` | account, roomID |
| 10 | list-org-members | `OrgMembersPattern` | NoBody | — | `ListOrgMembersResponse` | account, orgID |
| 11 | ensure-room-key | `RoomKeyEnsure` (concrete) | Register | `RoomKeyEnsureRequest` | `RoomKeyEnsureResponse` | — |
| 12 | get-room-key | `RoomKeyGetPattern` | NoBody + optional body | `RoomKeyGetRequest` (optional) | `RoomKeyGetResponse` | account, roomID |
| 13 | mute-toggle | `MuteTogglePattern` | NoBody | — | `MuteToggleResponse` | account, roomID |
| 14 | favorite-toggle | `FavoriteTogglePattern` | NoBody | — | `FavoriteToggleResponse` | account, roomID |
| 15 | room-rename | `RoomRenamePattern` | Register | `RoomRenameRequest` (new) | `StatusWithRequestReply{accepted}` | account, roomID |
| 16 | room-restricted | `RoomRestricted` (concrete) | Register | `RoomRestrictedRequest` | `StatusWithRequestReply{ok}` | — |
| 17 | app-tabs | `RoomAppTabsPattern` | NoBody | — | `GetRoomAppTabsResponse` | account, roomID |
| 18 | app-cmd-menu | `RoomAppCmdMenuPattern` | NoBody | — | `GetRoomAppCommandMenuResponse` | account, roomID |
| 19 | member-statuses | `MemberStatusesPattern` | NoBody + optional body | `ListMemberStatusesRequest` (optional) | `ListMemberStatusesResponse` | account, roomID |
| 20 | mentionable | `MentionableSubscriptionsPattern` | NoBody + optional body | `MentionableSubscriptionsRequest` (optional) | `MentionableSubscriptionsResponse` | account, roomID |
| 21 | server-create-dm (room-worker) | `RoomCreateDMSync` (concrete) | Register | `SyncCreateDMRequest` | `SyncCreateDMReply` | — |

Authorization on the two list endpoints:
- **list-members** (room, row 9): reads `account` for the membership check
  (`GetSubscription` → `errNotRoomMember`, `handler.go:470-481`) — `{account}`
  load-bearing.
- **list-org-members** (row 10): reads only `orgID`, no requester check
  (`handler.go:454-467`). **Reviewed/decided:** it's a section/department
  directory lookup (`orgID` = user `SectID`/`DeptID`, `store.go:73`) with no room
  context, intentionally readable by any authenticated caller — no gate added.
  `{account}` still named for uniformity.

## Wire compatibility

- **Subjects:** unchanged (Pattern builders render byte-identical subjects).
- **Success bodies:** unchanged content; `map → struct` only changes key order
  (not part of the contract).
- **Error envelopes:** codes/reasons unchanged except two deltas: (1)
  unmarshal-failure text `"invalid request"` → `"invalid request payload"`, same
  `bad_request`; (2) `roomRename`/`roomRestricted` bad body now `bad_request`
  instead of `internal` (a fix).
- **Empty-body callers** of `list-members` / `get-room-key` keep working via the
  optional-unmarshal flavor.
- `docs/client-api.md`: **must** update the rename malformed-body error
  (`client-api.md:622`: `"invalid request"` → `"invalid request payload"`,
  `internal` → `bad_request`). Per CLAUDE.md, also verify the request-ID-required
  note for the migrated RPCs (today only create §211, rename §608 — pre-existing
  gap). Schemas/events otherwise unchanged.

## Testing (TDD)

Red → Green per unit, suite green throughout.
- `pkg/natsrouter`: `RequireRequestID` tests (valid / missing / invalid → abort).
- `pkg/subject`: rendered-pattern assertion per builder.
- `pkg/model`: round-trips for `StatusReply`, `StatusWithRequestReply`,
  `RoomRenameRequest`.
- `room-service` / `room-worker` handler tests: build `*Context` via
  `natsrouter.NewContext(map[...])`, call `h.xxx(ctx, req)`, assert on `*Resp`.
  Beyond signature updates: (a) **delete** the ~12 `*_InvalidSubject` cases
  (`handler_test.go:884,1825,1997,2431,2940,3376,3742,4158,4381,4879,5403,5537`)
  — branches now unreachable; (b) **move** the `TestWrappedCtx_*` trio
  (`handler_test.go:2351-2402`) and `TestRequireDedupRequestID`
  (`handler_test.go:4345`) into `pkg/natsrouter`; (c) malformed-body cases covered
  once by `Register` tests. Add empty-body tests for list-members / get-room-key.
- Integration tests: subjects/replies unchanged → should pass as-is; fix only
  what wiring touches.
- `make generate` if store interfaces changed (none expected), then `make lint` +
  `make test` (race).

## Sequencing (single branch, small commits)

1. `pkg/natsrouter`: `RequireRequestID()` + tests.
2. `pkg/subject`: Pattern builders + tests.
3. `pkg/model`: status replies + `RoomRenameRequest` + tests.
4. room-service handlers in groups (toggles/reads → list/get → mutations →
   create), tests green.
5. room-service `main.go` cutover; delete `RegisterCRUD`/wrappers/`wrappedCtx`.
6. room-worker: migrate `natsServerCreateDM`.
7. Final `make lint` + `make test`; diff `docs/client-api.md`.

## Risks & mitigations

- **Silent dedup break** from minting → use `RequireRequestID()` (decision 1);
  middleware tests.
- **Empty-body regression** (list-members/get-room-key) → optional-body flavor +
  tests.
- **Unbounded goroutines** flooding deps → `HandlerTimeout` (§6.4); revisit
  `WithMaxConcurrency` if needed.
- **Ordering no longer FIFO per subject** → mutations use atomic Mongo ops / dedup
  by request-id; create + sync-DM are collision/retry-safe.
- **Large test-file churn** (`handler_test.go` ~228 KB) → mechanical `NewContext`
  updates, group-by-group.

## Out of scope

room-worker JetStream consumer; subject values; request/response schemas;
federation payloads; room-worker async error model (already conformant).
