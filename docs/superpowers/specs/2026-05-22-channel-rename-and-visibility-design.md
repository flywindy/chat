# Channel Rename and Visibility Design

**Date:** 2026-05-22 (revised 2026-05-25 after fresh-eyed review against
existing `room-service` / `room-worker` / `inbox-worker` code)
**Status:** Shipped, with post-merge divergences — see note below.

> **Post-merge divergences (PR #224 review):**
> * **Visibility → restricted, sync, server-internal.** The visibility RPC was renamed `room.restricted` and converted from an async-job (client → `chat.user.>` → ROOMS_ → room-worker → AsyncJobResult) to a **synchronous server-to-server** request/reply on `chat.server.request.room.{siteID}.restricted`. room-service does the work in-handler (Mongo writes, sys-message publish, outbox fan-out) and replies `{"status":"ok","requestId":<id>}` once the work is committed. No `AsyncJobResult`. The RPC is no longer part of the client-facing API surface.
> * **Per-subscription fan-out replaced by a single room-scoped sys message.** Both rename and restricted dropped the `SubscriptionUpdateEvent` per-subscription publish; they now publish ONE `room_renamed` / `room_restricted` system message to `chat.room.{roomID}.event` (via the existing canonical-message pipeline) and clients derive their local subscription state from that single event.
> * **`pkg/subject.RoomRename` no longer panics** on invalid account tokens — the validation check was removed (server callers are responsible for clean identities).
> * **`updateChannelRoom` no longer enforces `type=channel` at the store layer** — room-service validates upstream, so the store only handles room-not-found.
> * **Cross-site member fan-out for add-members ships only that site's homed accounts** instead of the full member list.

> Line-number references throughout this spec (e.g.
> `room-worker/handler.go:1203–1210`) are accurate as of the current
> `master`. They WILL drift once implementation lands and any of the
> referenced files are touched; treat them as anchors for review, not
> as code-search targets after the implementation PR merges.

## Overview

Two admin operations on channel rooms:

1. **Rename** — change `Room.Name` and the denormalized
   `Subscription.Name` on every subscription. Owner or platform admin.
2. **Visibility** — set `Room.Restricted` and `Room.ExternalAccess`,
   and (on the `false→true` restrict transition only) rewrite every
   subscription's roles to a canonical owner/member shape. Platform
   admin only.

Both flow through the existing two-service split:

- `room-service` validates synchronously, publishes the canonical
  request to `ROOMS_{siteID}`, replies `{"status":"accepted","requestId":<id>}`.
- `room-worker` (home site only) persists Mongo writes, emits a system
  message for rename, fans `SubscriptionUpdateEvent` per affected
  subscription, and publishes one outbox event per remote site that
  has members.
- `inbox-worker` (remote sites with federated members) mirrors the
  subscription mutation locally. **No event fan-out** — the home-site
  `room-worker` already published per-account events, and NATS
  supercluster routes them to the user wherever connected.

Platform additions:
- `User.Roles []UserRole` with `UserRoleAdmin` / `UserRoleUser` —
  platform-level admin gate.
- `Room.ExternalAccess bool` — external-member access flag (semantics
  out of scope). `Room.Restricted` already exists in
  `pkg/model/room.go:27` and is read today by
  `room-service.handleAddMembers`.
- `Subscription.Restricted` + `Subscription.ExternalAccess`
  denormalized fields joining the existing `Name` + `RoomType`
  denormalization. Lets a single `SubscriptionUpdateEvent` carry every
  client-facing room field; no `RoomMetadataUpdateEvent` is published.

Each worker grows its existing store interface
(`room-worker.SubscriptionStore`, `inbox-worker.InboxStore`) with the
new methods. No shared package — see §Store Methods.

## Scope

In scope:
- `User.Roles []UserRole` + the two constants.
- `Room.ExternalAccess bool`; `RenameRoomRequest` /
  `RoomVisibilityRequest` request types.
- `Subscription.Restricted` + `Subscription.ExternalAccess`
  (both `omitempty`).
- `MessageTypeRoomRenamed` + `RoomRenamedSysData`.
- `OutboxRoomRenamed` / `OutboxRoomVisibilityChanged` constants +
  payload types.
- `AsyncJobOpRoomRename` / `AsyncJobOpRoomVisibility` constants;
  `room-worker` publishes `AsyncJobResult` for both ops following the
  existing create/add/remove pattern.
- Four `pkg/subject` builders: `RoomRename` / `RoomRenameWildcard` /
  `RoomVisibility` / `RoomVisibilityWildcard`.
- `room-service` NATS RPCs `room.rename` / `room.visibility`.
- Canonical events on `ROOMS_{siteID}`.
- `room-worker` consumers for both.
- `inbox-worker` handlers for both — mirror only.
- Store-method additions on both workers (see §Store Methods).
- Port `errPermanent` + Ack-on-permanent from `room-worker` to
  `inbox-worker` (one-shot refactor — today `inbox-worker/main.go:298–308`
  NAKs on every error and would loop on poison messages).
- `RESTRICTED_ROOM_MIN_MEMBERS` env var on `room-service` (default `5`).
- `mock-user-service`: `"admin1"` → `[UserRoleAdmin]`; default to
  `[UserRoleUser]`.
- `docs/client-api.md`: request/response/error schemas for both RPCs.

Out of scope: DM / BotDM / discussion rooms; combined rename+visibility
RPC; `ListRoomMembers` bot/app enrichment; frontend wiring (existing
`SubscriptionUpdateEvent` listener picks up new payloads unchanged);
operational semantics of `ExternalAccess=false` for existing external
members (owned by a separate spec).

## Architecture

### Event Flow — Rename

```text
Client
  |  NATS request/reply: chat.user.{account}.request.room.{roomID}.{siteID}.room.rename
  v
room-service (room's home site)
  |  validate: subject parse, request unmarshal, payload vs subject equality,
  |            name validity, requesterUser fetch (rule #4),
  |            room exists, room.Type == Channel,
  |            isPlatformAdmin(user) OR (sub.Roles contains "owner")
  |  publish RenameRoomRequest to ROOMS stream (with X-Request-ID header)
  |  reply {"status":"accepted","requestId":<id>}
  v
ROOMS_{siteID} — chat.room.canonical.{siteID}.room.rename
  |
  v  consume (durable consumer: room-worker)
room-worker (room's home site)
  |  defer func() { publishAsyncJobResult(ctx, requesterAccount, AsyncJobOpRoomRename, roomID, err) }()
  |    — closure over named return `err`; vars populated post-unmarshal;
  |      short-circuits gracefully on unmarshal failure (empty requesterAccount).
  |  validate X-Request-ID (permanent on missing/invalid)
  |  1. h.store.UpdateRoomName(ctx, roomID, newName)
  |  2. h.store.UpdateSubscriptionNamesForRoom(ctx, roomID, newName)
  |  3. publishCanonical(sys message: MessageTypeRoomRenamed)
  |     — Nats-Msg-Id = idgen.MessageIDFromRequestID(requestID, "room_renamed")
  |     — broadcast-worker then fans out chat.room.{roomID}.event (one publish, reaches all)
  |  4. subs = h.store.ListByRoom(ctx, roomID)     // existing method
  |     for each: publish SubscriptionUpdateEvent{Action:"renamed", Subscription:sub}
  |       to subject.SubscriptionUpdate(sub.User.Account)
  |       — supercluster routes per-account regardless of user's home site
  |  5. accounts := extract(subs); users := h.store.FindUsersByAccounts(ctx, accounts)
  |     remoteSites := distinct(user.SiteID for user where user.SiteID != h.siteID)
  |     for each remoteSiteID in remoteSites: publish OutboxEvent{Type:OutboxRoomRenamed,
  |       payload:{roomID,newName,ts}} to outbox.{room.SiteID}.to.{remoteSiteID}.room_renamed
  |       — Nats-Msg-Id = natsutil.OutboxDedupID(ctx, remoteSiteID, payloadSeed)
  |     (same bucket-by-user.SiteID pattern as processAddMembers:1117–1124)
  |  6. ack (on permanent error: Ack with errPermanent; deferred publishAsyncJobResult
  |     reports the error to the requester before Ack)
  v
OUTBOX_{room.SiteID} → INBOX_{remoteSiteID}
       — wildcard subject transform outbox.*.to.{siteID}.> →
         chat.inbox.{siteID}.aggregate.> (owned by ops/IaC) already
         covers the new event-type suffix; no IaC change required.
  v
inbox-worker (remote site — Mongo replication only; no event fan-out)
  |  handleRoomRenamed:
  |    1. h.store.UpdateSubscriptionNamesForRoom(ctx, roomID, newName)
  |       — the {roomId:roomID} filter only matches the federated user
  |         subscription copies stored on this site
  |    2. (no SubscriptionUpdateEvent — supercluster already delivered from home site)
  |    3. (no UpdateRoomName — remote sites don't store Room docs for federated channels)
```

### Event Flow — Visibility

```text
Client
  |  NATS request/reply: chat.user.{account}.request.room.{roomID}.{siteID}.room.visibility
  v
room-service (room's home site)
  |  validate: subject parse, request unmarshal, payload vs subject equality,
  |            isPlatformAdmin(requesterUser),
  |            room exists, room.Type == Channel,
  |            if req.Restricted=true AND ownerAccount non-empty:
  |              ownerAccount has a subscription in the room (any role),
  |            if restricted transitioning false→true:
  |              ownerAccount non-empty (required to designate owner),
  |              room.UserCount >= RESTRICTED_ROOM_MIN_MEMBERS
  |  publish RoomVisibilityRequest to ROOMS stream (with X-Request-ID header)
  |  reply {"status":"accepted","requestId":<id>}
  v
ROOMS_{siteID} — chat.room.canonical.{siteID}.room.visibility
  |
  v  consume (durable consumer: room-worker)
room-worker (room's home site)
  |  defer func() { publishAsyncJobResult(ctx, requesterAccount, AsyncJobOpRoomVisibility, roomID, err) }()
  |  validate X-Request-ID (permanent on missing/invalid)
  |  1. h.store.UpdateRoomVisibility(ctx, roomID, restricted, externalAccess)
  |  2. h.store.ApplySubscriptionVisibility(ctx, roomID, restricted, externalAccess, ownerAccount)
  |     — single updateMany. Three branches keyed on (restricted, ownerAccount):
  |       (a) restricted=true, ownerAccount non-empty (transition):
  |           $set restricted+externalAccess AND $cond rewrites roles —
  |           u.account == ownerAccount becomes ["owner"], else ["member"].
  |       (b) restricted=true, ownerAccount empty (non-transition; admin
  |           toggling externalAccess on already-restricted room):
  |           $set restricted+externalAccess only; roles untouched.
  |       (c) restricted=false (unrestrict): $set restricted+externalAccess
  |           only; roles untouched.
  |  3. subs = h.store.ListByRoom(ctx, roomID)     // existing method
  |     for each: publish SubscriptionUpdateEvent{Action:"visibility_changed", Subscription:sub}
  |       to subject.SubscriptionUpdate(sub.User.Account)
  |       — supercluster routes per-account regardless of user's home site
  |  4. accounts := extract(subs); users := h.store.FindUsersByAccounts(ctx, accounts)
  |     remoteSites := distinct(user.SiteID for user where user.SiteID != h.siteID)
  |     for each remoteSiteID in remoteSites: publish OutboxEvent{
  |       Type:OutboxRoomVisibilityChanged,
  |       payload:{roomID, restricted, externalAccess, ownerAccount, ts}}
  |       to outbox.{room.SiteID}.to.{remoteSiteID}.room_visibility_changed
  |       — Nats-Msg-Id = natsutil.OutboxDedupID(ctx, remoteSiteID, payloadSeed)
  |  5. ack (deferred publishAsyncJobResult reports success/error to requester)
  v
OUTBOX_{room.SiteID} → INBOX_{remoteSiteID}   (same subject transform as rename)
  v
inbox-worker (remote site — Mongo replication only; no event fan-out)
  |  handleRoomVisibilityChanged:
  |    1. h.store.ApplySubscriptionVisibility(ctx, payload.RoomID, payload.Restricted,
  |       payload.ExternalAccess, payload.OwnerAccount)
  |       — same three branches as room-worker. OwnerAccount is load-bearing:
  |         if the designated owner's home site is this one, the $cond promotes
  |         their mirrored subscription copy to ["owner"].
  |    2. (no SubscriptionUpdateEvent — supercluster already delivered from home site)
  |    3. (no UpdateRoomVisibility — remote sites don't store Room docs for federated channels)
```

No system message on visibility change — the per-account
`SubscriptionUpdateEvent` carries the new flags directly.

## NATS Subjects

### Request/Reply (Client → room-service)

| Operation | Subject | Queue Group |
|-----------|---------|-------------|
| Rename | `chat.user.{account}.request.room.{roomID}.{siteID}.room.rename` | `room-service` |
| Visibility | `chat.user.{account}.request.room.{roomID}.{siteID}.room.visibility` | `room-service` |

Add four builders to `pkg/subject/subject.go` matching the existing
`MemberRoleUpdate` shape:

```go
func RoomRename(account, roomID, siteID string) string {
    return fmt.Sprintf("chat.user.%s.request.room.%s.%s.room.rename", account, roomID, siteID)
}
func RoomRenameWildcard(siteID string) string {
    return fmt.Sprintf("chat.user.*.request.room.*.%s.room.rename", siteID)
}
func RoomVisibility(account, roomID, siteID string) string {
    return fmt.Sprintf("chat.user.%s.request.room.%s.%s.room.visibility", account, roomID, siteID)
}
func RoomVisibilityWildcard(siteID string) string {
    return fmt.Sprintf("chat.user.*.request.room.*.%s.room.visibility", siteID)
}
```

The ROOMS stream's `chat.room.canonical.{siteID}.>` filter and the
OUTBOX→INBOX subject transform (`outbox.*.to.{siteID}.>` →
`chat.inbox.{siteID}.aggregate.>`) both wildcard over the event-type
suffix — no stream-config change.

### Outbox / Client Fan-out

| Subject | Payload | Purpose |
|---------|---------|---------|
| `chat.msg.canonical.{siteID}.created` | `Message{Type=room_renamed, SysMsgData=…}` | Sys message (rename only) |
| `subject.UserResponse(requesterAccount, requestID)` | `AsyncJobResult{RequestID, Operation, Status, Error, RoomID, Timestamp}` | Per-request success/error signal back to the requester (existing struct at `pkg/model/event.go:250`) |
| `subject.SubscriptionUpdate(account)` | `SubscriptionUpdateEvent` with `Action="renamed"` or `"visibility_changed"` | Per-affected-subscription event; payload's Subscription carries updated Name/Restricted/ExternalAccess/Roles |
| `outbox.{room.SiteID}.to.{remoteSiteID}.room_renamed` | `OutboxEvent` wrapping `RoomRenamedOutboxPayload` | Replicate rename cross-site |
| `outbox.{room.SiteID}.to.{remoteSiteID}.room_visibility_changed` | `OutboxEvent` wrapping `RoomVisibilityOutboxPayload` | Replicate visibility cross-site |

## Data Models

### `pkg/model/user.go`

```go
type UserRole string

const (
    UserRoleAdmin UserRole = "admin"
    UserRoleUser  UserRole = "user"
)
```

Append `Roles []UserRole \`json:"roles" bson:"roles"\`` to `User`.
Empty `Roles` reads as `["user"]`; only positive marker is `"admin"`.
`mock-user-service` writes explicit `["user"]` for fixture clarity.

### `pkg/model/room.go`

`Room.Restricted` is pre-existing (with `omitempty`). Add
`ExternalAccess` alongside it:

```go
ExternalAccess bool `json:"externalAccess,omitempty" bson:"externalAccess,omitempty"`
```

New request types:

```go
type RenameRoomRequest struct {
    RoomID    string `json:"roomId"    bson:"roomId"`
    NewName   string `json:"newName"   bson:"newName"`
    Account   string `json:"account"   bson:"account"`   // server-set; client SHOULD omit
    Timestamp int64  `json:"timestamp" bson:"timestamp"` // server-set
}

type RoomVisibilityRequest struct {
    RoomID         string `json:"roomId"                 bson:"roomId"`
    Restricted     bool   `json:"restricted"             bson:"restricted"`
    ExternalAccess bool   `json:"externalAccess"         bson:"externalAccess"`
    OwnerAccount   string `json:"ownerAccount,omitempty" bson:"ownerAccount,omitempty"` // when non-empty + Restricted=true, this account becomes sole owner (see §Validation rules 7–8)
    Account        string `json:"account"                bson:"account"`                // server-set
    Timestamp      int64  `json:"timestamp"              bson:"timestamp"`              // server-set
}
```

`room-service` sets `Account` from the subject and `Timestamp` to
`time.Now().UTC().UnixMilli()` before publishing; payload `RoomID` /
`Account` MUST equal subject-parsed values if non-empty (rule #2).
Visibility clients always send both flags (read-modify-write);
last-writer-wins on concurrent admin flips.

### `pkg/model/subscription.go`

```go
Restricted     bool `json:"restricted,omitempty"     bson:"restricted,omitempty"`
ExternalAccess bool `json:"externalAccess,omitempty" bson:"externalAccess,omitempty"`
```

`omitempty` keeps these absent from BSON unless set. Readers MUST treat
missing as `false`. Both fields are fast-path denormalization for client
payloads — **`Room` is the access-control source of truth**.

Maintained by `ApplySubscriptionVisibility` (this spec). Future
subscription-creation paths MAY seed from current `Room` values; out of
scope here (see §Known Limitations for the federation race this
creates).

Two well-established system facts the spec depends on (called out only
because the design wouldn't work without them):
- `Subscription.SiteID == Room.SiteID` always — subscriptions carry the
  room's home-site ID, not the user's. The user's home site lives on
  the `User` doc (`User.SiteID`).
- Every site's Mongo holds a `User` doc for every user it has data
  about. `room-service.GetUser` and the worker's
  `FindUsersByAccounts(accounts)` join `subscriptions ↔ users` against
  local Mongo without any cross-site round-trip. Existing
  user-replication paths maintain this.

### `pkg/model/message.go`

```go
const MessageTypeRoomRenamed = "room_renamed"

type RoomRenamedSysData struct {
    NewName   string `json:"newName"   bson:"newName"`
    ByAccount string `json:"byAccount" bson:"byAccount"`
}
```

Sys message content:
`fmt.Sprintf("%s renamed the channel to %q", req.Account, req.NewName)`.
No `oldName` — `req.NewName` is stable in the canonical event across
JetStream redeliveries, so the message reads identically on every
retry.

### `pkg/model/event.go`

Doc-comment updates: `SubscriptionUpdateEvent.Action` gains `"renamed"`
and `"visibility_changed"`. New typed constants (existing publishers
reference by symbol, e.g. `model.OutboxMemberRemoved`):

```go
OutboxRoomRenamed            OutboxEventType = "room_renamed"
OutboxRoomVisibilityChanged  OutboxEventType = "room_visibility_changed"
```

`inbox-worker.HandleEvent` switch dispatches on string literals today
(`case "member_added":`); keep that style for the new cases.

Outbox payloads:

```go
type RoomRenamedOutboxPayload struct {
    RoomID    string `json:"roomId"    bson:"roomId"`
    NewName   string `json:"newName"   bson:"newName"`
    Timestamp int64  `json:"timestamp" bson:"timestamp"`
}

type RoomVisibilityOutboxPayload struct {
    RoomID         string `json:"roomId"                 bson:"roomId"`
    Restricted     bool   `json:"restricted"             bson:"restricted"`
    ExternalAccess bool   `json:"externalAccess"         bson:"externalAccess"`
    OwnerAccount   string `json:"ownerAccount,omitempty" bson:"ownerAccount,omitempty"` // when non-empty + Restricted=true, this account becomes sole owner
    Timestamp      int64  `json:"timestamp"              bson:"timestamp"`
}
```

`RoomMetadataUpdateEvent` (defined in `pkg/model/event.go:23–30` but
dormant — no `OutboxEventType` constant references it) is NOT activated
by this PR.

Add two `AsyncJobResult` operation constants to the existing block in
`pkg/model/event.go:259–265`:

```go
AsyncJobOpRoomRename     = "room.rename"
AsyncJobOpRoomVisibility = "room.visibility"
```

These match the create/add/remove pattern: `room-worker` publishes
`AsyncJobResult{Status:"ok"|"error", Error:<sanitized>}` to
`subject.UserResponse(requesterAccount, requestID)` once the async
work lands, so the requester's client gets a success/failure signal
distinct from the downstream `SubscriptionUpdateEvent` fan-out.

## Store Methods

Each worker grows its existing store interface. The two methods that
exist in both (`UpdateSubscriptionNamesForRoom`,
`ApplySubscriptionVisibility`) have copy-paste-identical query bodies
across the two `store_mongo.go` files — a deliberate trade for keeping
the per-service store pattern and its mock surface intact.

### `room-worker.SubscriptionStore` additions

```go
// Match {_id: roomID, type: "channel"} on rooms; return existing
// ErrRoomNotFound / ErrNotChannelRoom sentinels.
UpdateRoomName(ctx context.Context, roomID, newName string) error
UpdateRoomVisibility(ctx context.Context, roomID string, restricted, externalAccess bool) error

// Single updateMany on subscriptions; filter {roomId: roomID}.
UpdateSubscriptionNamesForRoom(ctx context.Context, roomID, newName string) error

// Single updateMany on subscriptions. See §ApplySubscriptionVisibility.
ApplySubscriptionVisibility(ctx context.Context, roomID string, restricted, externalAccess bool, ownerAccount string) error
```

Two existing `SubscriptionStore` methods are reused without
modification:
- `ListByRoom(ctx, roomID)` — returns every subscription on the room's
  home site (subscriptions are room-scoped: `Subscription.SiteID ==
  Room.SiteID`). Drives the per-account `SubscriptionUpdateEvent`
  fan-out.
- `FindUsersByAccounts(ctx, accounts)` — returns the matching `User`
  records. Wrapped by the new `findRemoteSitesForAccounts` helper
  (see §Helpers), which buckets remote sites via `user.SiteID !=
  h.siteID`. Same pattern as the existing inline snippets in
  `processAddMembers:1117–1124` and `processRemoveOrg:706–712` — those
  are refactored to call the helper in the same PR. No new store
  method needed.

### `inbox-worker.InboxStore` additions

```go
UpdateSubscriptionNamesForRoom(ctx context.Context, roomID, newName string) error
ApplySubscriptionVisibility(ctx context.Context, roomID string, restricted, externalAccess bool, ownerAccount string) error
```

Same query bodies as the room-worker counterparts. On a remote site
the local Mongo only holds subscription copies for the federated users
whose home site is here, so the `{roomId: roomID}` filter naturally
scopes the updates correctly.

### `ApplySubscriptionVisibility` query body

One `updateMany`. Three branches keyed on `(restricted, ownerAccount)`:

**(a) Restrict transition** (`restricted=true`, `ownerAccount` non-empty)
— aggregation-pipeline update:

```js
db.subscriptions.updateMany(
    { roomId: roomID },
    [{ $set: {
        restricted:     true,
        externalAccess: <externalAccess>,
        roles: { $cond: { if: { $eq: ["$u.account", ownerAccount] }, then: ["owner"], else: ["member"] } }
    }}]
)
```

The `$cond` keys on `u.account`, so the admin's chosen `ownerAccount`
becomes the sole owner regardless of pre-restrict role — this
eliminates the validation→processing race where a concurrent demote
between rule #7 and the worker could leave zero owners.

**(b) Non-transition restricted update** (`restricted=true`,
`ownerAccount` empty — admin toggling `externalAccess` only) and
**(c) unrestrict** (`restricted=false`) — single `updateMany`, no role
rewrite:

```js
db.subscriptions.updateMany(
    { roomId: roomID },
    { $set: { restricted: <restricted>, externalAccess: <externalAccess> } }
)
```

Skipping the role rewrite on empty `ownerAccount` prevents wiping the
existing owner during a pure `externalAccess` toggle. All branches are
idempotent on retry.

## Helpers

- `room-service/helper.go` — add `isPlatformAdmin(u *model.User) bool`
  (true iff `UserRoleAdmin` ∈ `u.Roles`, nil-safe). Extend
  `sanitizeError`'s known-prefix list with: `"only owners or admins"`,
  `"only admins"`, `"owner account is not a member of this room"`,
  `"owner account is required when restricting a room"`,
  `"not enough members"`, `"invalid name"`, `"rename is only allowed in
  channel rooms"`, `"visibility change is only allowed in channel rooms"`.
- `room-worker` — add `publishSubscriptionEvents(ctx, subs, action)`
  publishing one `SubscriptionUpdateEvent` per subscription to
  `subject.SubscriptionUpdate(sub.User.Account)`. `inbox-worker` does
  NOT add this — it never publishes events.
- `room-worker` — add
  `findRemoteSitesForAccounts(ctx context.Context, accounts []string) ([]string, error)`:
  calls `h.store.FindUsersByAccounts(ctx, accounts)`, dedupes
  `user.SiteID` values where `user.SiteID != h.siteID`, returns the
  resulting slice (nil-safe on empty input). New `processRoomRename` /
  `processRoomVisibility` both call this; in the same PR, retrofit the
  existing inline snippets in `processAddMembers:1117–1124` and
  `processRemoveOrg:706–712` to call it too (mechanical replacement —
  reduces 4 inline copies to 1 helper).

## Validation Rules

Synchronous in `room-service`; on failure `natsutil.ReplyError` returns
the sanitized message and nothing is published.

### `room.rename`

| # | Check | Error |
|---|-------|-------|
| 1 | Parse `account` and `roomID` from subject | `"invalid rename subject"` |
| 2 | Unmarshal `RenameRoomRequest`; payload `RoomID`/`Account` must equal subject-parsed values if non-empty | `"invalid request"` |
| 3 | `newName` non-empty after `strings.TrimSpace`, ≤ 100 chars | `"invalid name"` |
| 4 | Fetch `requesterUser` via `GetUser(account)`. `errors.Is(err, ErrUserNotFound)` (sentinel wraps `mongo.ErrNoDocuments` in `store_mongo.go:673`) → `requesterUser = nil`, continue. Other errors return internal error (don't silently downgrade auth). | `"internal error"` on transient |
| 5 | Room must exist | `"room not found"` |
| 6 | `room.Type == RoomTypeChannel` | `"rename is only allowed in channel rooms"` |
| 7 | `isPlatformAdmin(requesterUser) \|\| (GetSubscription(account, roomID) succeeds AND hasRole(sub.Roles, RoleOwner))`. Admin check FIRST so admins without a subscription pass. | `"only owners or admins can rename a channel"` |

On success: marshal with subject-set `Account` and
`time.Now().UTC().UnixMilli()` `Timestamp`, publish to
`RoomCanonical(siteID, "room.rename")` with `X-Request-ID` propagated
via JetStream header, reply
`{"status":"accepted","requestId":<id>}`.

### `room.visibility`

| # | Check | Error |
|---|-------|-------|
| 1 | Parse `account` and `roomID` from subject | `"invalid visibility subject"` |
| 2 | Unmarshal `RoomVisibilityRequest`; payload `RoomID`/`Account` equality | `"invalid request"` |
| 3 | Fetch `requesterUser` via `GetUser(account)` (same sentinel handling as rename rule #4 — `ErrUserNotFound` → nil, transient → internal error). Then `isPlatformAdmin(requesterUser)`. | `"only admins can change room visibility"` ; `"internal error"` on transient |
| 4 | Room must exist | `"room not found"` |
| 5 | `room.Type == RoomTypeChannel` | `"visibility change is only allowed in channel rooms"` |
| 6 | Compute `isTransition := req.Restricted == true AND room.Restricted == false`. No check; used by rules 8 and 9. | — |
| 7 | If `req.Restricted == true` AND `OwnerAccount` non-empty: `(OwnerAccount, roomID)` subscription exists (any role — `$cond` promotes it to sole owner). Applies to BOTH transitions and non-transitions: if the admin sends `OwnerAccount` on an already-restricted room, that person becomes the new sole owner. | `"owner account is not a member of this room"` |
| 8 | If `isTransition`: `OwnerAccount` MUST be non-empty (a restricted room needs a designated owner) | `"owner account is required when restricting a room"` |
| 9 | If `isTransition`: `room.UserCount >= cfg.RestrictedRoomMinMembers` (existing denormalized counter; bots/apps excluded) | `"not enough members to restrict (need at least N)"` |

**`OwnerAccount` semantics.** The field is authoritative whenever
`Restricted=true`: if non-empty, the worker's branch (a) promotes
that account to sole owner regardless of prior role. If the admin
wants to toggle `ExternalAccess` on an already-restricted room WITHOUT
changing ownership, they must omit `OwnerAccount` (worker hits
branch (b), roles untouched). If the admin wants to change the
designated owner on an already-restricted room, they send the new
`OwnerAccount` (worker hits branch (a), rewrites all roles around the
new owner). On unrestrict (`Restricted=false`) `OwnerAccount` is
ignored — branch (c) only touches the flags.

On success: publish to `RoomCanonical(siteID, "room.visibility")`,
reply `{"status":"accepted","requestId":<id>}`. Op is idempotent —
re-applying current state is harmless.

## Processing (room-worker)

`HandleJetStreamMsg` dispatches by canonical subject trailing token:
`.room.rename` → `processRoomRename`, `.room.visibility` →
`processRoomVisibility`. Both processors share the signature
`(ctx context.Context, data []byte) (err error)` — the named return is
load-bearing for the deferred `publishAsyncJobResult` closure (see
below).

`requestID` comes from `natsutil.RequestIDFromContext(ctx)` (the
`X-Request-ID` header room-service stamped at publish). It's forwarded
onto every outbox publish for cross-site correlation and dedup. Both
processors guard with the existing pattern:

```go
requestID := natsutil.RequestIDFromContext(ctx)
if requestID == "" {
    return newPermanent("missing X-Request-ID")
}
if !idgen.IsValidUUID(requestID) {
    return newPermanent("invalid X-Request-ID: must be a hyphenated UUID")
}
```

— matching `processCreateRoom:1212–1218` and `processAddMembers:745–751`.
Without these guards the deterministic
`idgen.MessageIDFromRequestID(requestID, "room_renamed")` would
collapse to a degenerate ID, breaking sys-message dedup.

**AsyncJob defer pattern** — both processors use
`processCreateRoom`'s "declare early, populate after unmarshal" shape
(`room-worker/handler.go:1203–1210`). The defer is registered
immediately so it fires on every return path, including unmarshal
failures; `publishAsyncJobResult` short-circuits when the
`requesterAccount` is still empty (`handler.go:99–101`), so unmarshal
errors don't generate a misleading "ok" result. The closure captures
the named return `err` by reference so the final value reaches the
publish call:

```go
func (h *Handler) processRoomRename(ctx context.Context, data []byte) (err error) {
    var requesterAccount, roomID string
    defer func() {
        h.publishAsyncJobResult(ctx, requesterAccount, model.AsyncJobOpRoomRename, roomID, err)
    }()
    // ... requestID guards ...
    var req model.RenameRoomRequest
    if err = json.Unmarshal(data, &req); err != nil {
        return newPermanent("unmarshal rename: %s", err.Error())
    }
    requesterAccount = req.Account
    roomID = req.RoomID
    // ... rest of the processor ...
}
```

The `defer h.publishAsyncJobResult(...)` (positional, no closure) shape
would NOT work — Go evaluates defer arguments at registration time, so
`err` would be captured as `nil`.

**Error policy** (existing `room-worker` pattern,
`handler.go:30,124–140,203–222`): permanent failures (unmarshal,
`ErrRoomNotFound`, `ErrNotChannelRoom`, missing/invalid request ID)
wrap via `newPermanent` / `newPermanentAbsent` and `Ack`; transient
Mongo / NATS failures `Nak`.

**Duplicate `AsyncJobResult` on retry.** The deferred closure fires on
every return path — including transient failures that `Nak`. On
JetStream redelivery, if the retry succeeds, the defer fires again
with `Status:"ok"`. The client therefore receives both
`{Status:"error", ...}` followed by `{Status:"ok", ...}`. This is the
existing behavior of every async room-worker op (`processCreateRoom` /
`processAddMembers` / `processRemove*`); the frontend reducer keys on
`(requestID, Status)` and treats the latest result as authoritative.
Unit tests for the new processors MUST cover the error-then-ok
sequence to confirm the same pattern holds.

### `processRoomRename`

Signature `(ctx context.Context, data []byte) (err error)`.

1. Declare `var requesterAccount, roomID string` and register the
   deferred `publishAsyncJobResult` closure (see pattern above).
2. Validate `requestID` (see guards above; permanent on missing/invalid).
3. Unmarshal `RenameRoomRequest` (permanent on error — the defer fires
   with empty `requesterAccount` and short-circuits, so no stale "ok"
   result). On success, set `requesterAccount = req.Account` and
   `roomID = req.RoomID`.
4. `h.store.UpdateRoomName(ctx, req.RoomID, req.NewName)`. Permanent on
   `ErrRoomNotFound` / `ErrNotChannelRoom`.
5. `h.store.UpdateSubscriptionNamesForRoom(ctx, req.RoomID, req.NewName)`.
6. Build and publish sys message:
   - `ID = idgen.MessageIDFromRequestID(requestID, "room_renamed")`
     (deterministic across redeliveries).
   - `Type = MessageTypeRoomRenamed`,
     `SysMsgData = RoomRenamedSysData{NewName, ByAccount}`,
     `Content = fmt.Sprintf("%s renamed the channel to %q", req.Account, req.NewName)`,
     `CreatedAt = time.UnixMilli(req.Timestamp).UTC()`.
   - Call `publishCanonical(ctx, &msg, h.siteID, time.Now().UTC())` —
     wraps in `MessageEvent{Event:EventCreated, Message:msg,
     SiteID:h.siteID, Timestamp:now.UnixMilli()}` per existing
     `room-worker/handler.go:1571–1583` and dedups via
     `natsutil.CanonicalDedupID(&evt)` (resolves to `Message.ID` for
     `EventCreated`).
7. `subs := h.store.ListByRoom(ctx, req.RoomID)` — also feeds the
   remote-site derivation in step 8. Call
   `publishSubscriptionEvents(ctx, subs, "renamed")`.
8. Derive remote sites via the helper (see §Helpers):

   ```go
   accounts := make([]string, 0, len(subs))
   for _, sub := range subs {
       accounts = append(accounts, sub.User.Account)
   }
   remoteSites, err := h.findRemoteSitesForAccounts(ctx, accounts)
   if err != nil {
       return fmt.Errorf("find remote sites for outbox fan-out: %w", err)
   }
   ```

9. For each `remoteSiteID` in `remoteSites`, build:

   ```go
   evt := model.OutboxEvent{
       Type:       model.OutboxRoomRenamed,
       SiteID:     h.siteID,
       DestSiteID: remoteSiteID,
       Payload:    json.Marshal(RoomRenamedOutboxPayload{
           RoomID: req.RoomID, NewName: req.NewName, Timestamp: req.Timestamp,
       }),
       Timestamp:  time.Now().UTC().UnixMilli(),
   }
   ```

   Publish to `subject.Outbox(h.siteID, remoteSiteID,
   model.OutboxRoomRenamed)` with `Nats-Msg-Id =
   natsutil.OutboxDedupID(ctx, remoteSiteID, payloadSeed)` (existing
   helper reads `X-Request-ID` from ctx, falls back to `payloadSeed`).
   `payloadSeed = fmt.Sprintf("%s:%s:%d", req.RoomID, req.NewName, req.Timestamp)`.
10. **Ack.**

### `processRoomVisibility`

Signature `(ctx context.Context, data []byte) (err error)`. Same defer
pattern, same `requestID` guards, same error policy as
`processRoomRename`.

1. Declare `var requesterAccount, roomID string`; register deferred
   `publishAsyncJobResult` closure with
   `model.AsyncJobOpRoomVisibility`.
2. Validate `requestID` (permanent on missing/invalid).
3. Unmarshal `RoomVisibilityRequest` (permanent on error). Set
   `requesterAccount = req.Account`, `roomID = req.RoomID`.
4. `h.store.UpdateRoomVisibility(ctx, req.RoomID, req.Restricted, req.ExternalAccess)`.
   Permanent on `ErrRoomNotFound` / `ErrNotChannelRoom`.
5. `h.store.ApplySubscriptionVisibility(ctx, req.RoomID,
   req.Restricted, req.ExternalAccess, req.OwnerAccount)` — three
   branches per the function contract above.
6. `subs := h.store.ListByRoom(ctx, req.RoomID)` — also feeds the
   remote-site derivation in step 7. Call
   `publishSubscriptionEvents(ctx, subs, "visibility_changed")`.
7. Derive `remoteSites` via `h.findRemoteSitesForAccounts(ctx, accounts)`
   — same call shape as `processRoomRename` step 8.
8. For each `remoteSiteID`, build `OutboxEvent{Type:
   model.OutboxRoomVisibilityChanged, SiteID: h.siteID, DestSiteID:
   remoteSiteID, Payload: marshaled RoomVisibilityOutboxPayload,
   Timestamp: time.Now().UTC().UnixMilli()}`. Publish to
   `subject.Outbox(...)` with `Nats-Msg-Id = natsutil.OutboxDedupID(ctx,
   remoteSiteID, fmt.Sprintf("%s:%t:%t:%d", req.RoomID, req.Restricted,
   req.ExternalAccess, req.Timestamp))`.
9. **Ack.**

The two writes (room + subscriptions) are not transactional. Both are
idempotent (`$set` of fixed values; `$cond` rewrite to canonical
shape); JetStream redelivery converges.

## Cross-Site Handling (inbox-worker)

Two new cases in `HandleEvent`. Each only writes to local Mongo — no
Room doc writes (remote sites don't store Room docs for federated
channels), no event publishing (home-site already emitted; supercluster
routed). Matches the existing role-update inbox-worker pattern.

- `handleRoomRenamed`: `h.store.UpdateSubscriptionNamesForRoom(ctx,
  payload.RoomID, payload.NewName)`.
- `handleRoomVisibilityChanged`:
  `h.store.ApplySubscriptionVisibility(ctx, payload.RoomID,
  payload.Restricted, payload.ExternalAccess, payload.OwnerAccount)`.
  `OwnerAccount` is load-bearing here: if the designated owner's home
  site is remote, that remote site's `$cond` needs the account string
  to promote the mirrored subscription copy. Dropping it would leave
  that owner as `["member"]`.

**Error policy refactor (in scope for this PR):** today
`inbox-worker/main.go:298–308` NAKs on every handler error, which would
loop on poison messages. Two-file refactor:

1. **`inbox-worker/handler.go`** — port the `errPermanent` sentinel
   (`room-worker/handler.go:30`), the `permanentError` type with
   `Error()` / `Unwrap()` / `Is()`, and the `newPermanent(format,
   args...)` constructor (`room-worker/handler.go:124–140`). Then wrap
   unmarshal failures in `handleRoomRenamed` /
   `handleRoomVisibilityChanged` (and the existing handlers) with
   `newPermanent(...)`.
2. **`inbox-worker/main.go`** — the `cons.Consume` callback at lines
   298–308 currently does an unconditional `m.Nak()` on handler error.
   Change it to mirror `room-worker/handler.go:203–222`: if
   `errors.Is(err, errPermanent)` then `m.Ack()`, else `m.Nak()`. The
   dispatch loop lives in `main.go` (not `handler.go`) on the
   inbox-worker side — different from room-worker's layout.

## Migration

No backfill. New fields use `bson:",omitempty"` — readers MUST treat
missing as `false`. Already-restricted rooms' subscriptions stay
`Restricted=false` in BSON until the next visibility op writes them.
Access control stays on `Room.Restricted`; the subscription field is
denormalization only.

## Configuration

```go
// room-service config
RestrictedRoomMinMembers int `env:"RESTRICTED_ROOM_MIN_MEMBERS" envDefault:"5"`
```

`mock-user-service/handler.go`:
- `"admin1"` → `User.Roles=[UserRoleAdmin]`.
- All others → `User.Roles=[UserRoleUser]`.

## Observability

Standard slog INFO per request and per processed event with `op`,
`requester`, `roomID`, `requestID`. Validation rejects log at INFO with
`reason`. No audit collection.

## Design Decisions

| Decision | Rationale |
|----------|-----------|
| Denormalize `Restricted`/`ExternalAccess` onto `Subscription` | Single `SubscriptionUpdateEvent` carries every client-facing room field. Avoids a separate `RoomMetadataUpdateEvent` channel and per-subscriber metadata fan-out. |
| `$cond` keys on `u.account == ownerAccount` | Admin's chosen owner becomes the sole owner regardless of pre-restrict role; eliminates the validation→processing race where a concurrent demote leaves zero owners. |
| `OwnerAccount` is authoritative whenever `Restricted=true` | Sending `OwnerAccount=X` on a restricted room (transition OR non-transition) makes X the sole owner. Admin who wants to change ownership re-sends visibility with the new `OwnerAccount`; admin who wants to toggle `ExternalAccess` only omits `OwnerAccount` (worker hits branch (b), roles untouched). |
| Skip role rewrite on empty `ownerAccount` | Lets the admin toggle `externalAccess` on an already-restricted room without touching ownership. |
| One visibility RPC, both flags every call | Admin reads-modifies-writes both. Last-writer-wins is acceptable for admin ops. |
| Home-site emits all per-account events; `inbox-worker` only mutates Mongo | NATS supercluster routes `chat.user.{account}.event.*` regardless of user's home site. Matches the role-update pattern. |
| Per-worker store methods, no shared package | Two `updateMany` query bodies duplicated across two `store_mongo.go` files is cheaper than a new package + mock surface + integration-test target. Bodies are copy-paste identical. |
| `Room.UserCount` only (no apps) for min-members | Restriction is a human-membership gate; bots/apps shouldn't count toward the threshold. |
| No Mongo transaction around room + subscription writes | Both writes idempotent; JetStream retry converges. Mongo transactions require replica-set deployments. |
| Sys-message dedup via deterministic `Nats-Msg-Id` | `Message.ID = idgen.MessageIDFromRequestID(requestID, "room_renamed")` → `publishCanonical` uses it as `Nats-Msg-Id` for JetStream stream dedup. |
| Outbox dedup via `natsutil.OutboxDedupID(ctx, destSiteID, payloadSeed)` | Existing helper: `{requestID}:{destSiteID}` with `payloadSeed` fallback. Stable across worker retries. Same as `processRoleUpdate`'s pattern. |
| Per-account `SubscriptionUpdateEvent` not server-deduped | Published to core NATS (not a stream). Frontend reducer is idempotent — applying the same payload twice yields the same `state.subscriptions[roomId]`. |
| `oldName` not tracked on rename | Sys message uses `newName` only; `req.NewName` is stable in the canonical event, eliminating the JetStream-redelivery race that `findOneAndUpdate(returnDocument: "before")` would introduce. |
| Sys message for rename, not visibility | Rename changes user-facing metadata (channel title). Visibility is admin-side; the subscription update carries the new flags. |
| `User.Roles []UserRole` (not `bool`) | Forward-compatible; matches `Subscription.Roles` shape. Empty `Roles` reads as `["user"]`; only positive marker is `"admin"`. |
| Subscription role `"admin"` stays unused | Platform `User.Roles` covers admin auth; the subscription-level role enum doesn't need extending. |

## Known Limitations

- **Add-member during rename/visibility federation race.** A federated
  user added to a room after a rename or visibility outbox event has
  reached their home site gets a mirrored subscription seeded at
  add-member time — potentially stale. The rename/visibility events
  won't replay for them. Mitigations (out of scope here): (a) admin
  re-fires the op, or (b) add-member federation reads the room's
  current `Name` / `Restricted` / `ExternalAccess` / `OwnerAccount` and
  seeds the new subscription fields at insert time.

## TDD Order

1. `pkg/model` — `UserRole` + constants, `User.Roles`,
   `Room.ExternalAccess`, `Subscription.Restricted` +
   `Subscription.ExternalAccess`, `MessageTypeRoomRenamed`,
   `RoomRenamedSysData`, request types, outbox payload types,
   `OutboxRoomRenamed` + `OutboxRoomVisibilityChanged` constants,
   `AsyncJobOpRoomRename` + `AsyncJobOpRoomVisibility` constants,
   doc-comment extensions. Update `pkg/model/model_test.go`.
2. `pkg/subject` — four new builders + tests.
3. `room-worker.SubscriptionStore` additions — add method signatures
   to the interface, run `make generate SERVICE=room-worker` to
   regenerate `mock_store_test.go` (never hand-edit), write integration
   tests (TestContainers Mongo) covering `ErrRoomNotFound` /
   `ErrNotChannelRoom`, the three `ApplySubscriptionVisibility`
   branches, idempotent re-application. Implement.
4. `inbox-worker.InboxStore` additions — same flow:
   add to interface, `make generate SERVICE=inbox-worker`, write
   integration tests for `UpdateSubscriptionNamesForRoom` +
   `ApplySubscriptionVisibility`. Implement.
5. `inbox-worker` permanent-error refactor — port `errPermanent` +
   Ack-on-permanent from `room-worker/handler.go`; unit-test the
   dispatch loop's terminate behaviour.
6. `room-service` handler unit tests (red) — every validation rule for
   both RPCs, happy paths.
7. `room-service` handler implementation (green). Add `isPlatformAdmin`,
   extend `sanitizeError` prefix list. Rule #8 reads `room.UserCount`.
8. `room-service` integration test — end-to-end NATS request/reply with
   stream-publish assertions.
9. `room-worker` handler unit tests (red) — both processors. Coverage:
   - X-Request-ID guards (missing → permanent; invalid UUID → permanent).
   - Unmarshal failure → permanent, deferred `AsyncJobResult`
     short-circuits on empty `requesterAccount` (no stale "ok").
   - Store-error paths (`ErrRoomNotFound`, `ErrNotChannelRoom`) →
     permanent + `AsyncJobResult{Status:"error", Error:<sanitized>}`.
   - Transient store error → `AsyncJobResult{Status:"error"}`, then on
     simulated retry success → `AsyncJobResult{Status:"ok"}`
     (error-then-ok sequence, matches existing async-op behavior).
   - Happy path: `UpdateRoomName`/`UpdateRoomVisibility`,
     `UpdateSubscriptionNamesForRoom` / `ApplySubscriptionVisibility`,
     sys-message publish (rename only) with deterministic
     `Nats-Msg-Id`, `ListByRoom` + `publishSubscriptionEvents`,
     `FindUsersByAccounts` + bucketed outbox publishes per remote
     site, `AsyncJobResult{Status:"ok"}`.
10. `room-worker` handler implementation (green).
11. `room-worker` integration test — Mongo state, canonical sys
    message, subscription event fan-out, outbox publishes.
12. `inbox-worker` handler unit tests (red) — both new handlers; assert
    no event publishing.
13. `inbox-worker` handler implementation (green).
14. `inbox-worker` integration test.
15. `mock-user-service` — seed `admin1`; default to `[UserRoleUser]`.
16. `docs/client-api.md` — schemas under §3.1.
17. `make lint && make test && make test-integration && make sast`.
