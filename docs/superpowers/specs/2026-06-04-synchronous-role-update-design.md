# Synchronous Member Role Update in room-service

## Summary

Collapse the asynchronous member-role-update flow into a single synchronous
request/reply handled entirely in `room-service`. Previously `room-service`
validated the request, published a canonical message to the `ROOMS` stream,
and replied `{"status":"accepted"}`; `room-worker.processRoleUpdate` later
consumed that message, mutated Mongo, and emitted the events. Role changes are
infrequent and cheap, so the JetStream hop bought little and cost a round-trip
plus a duplicate code path.

Architecturally mirrors `mute.toggle` / `favorite.toggle`: `room-service`
mutates the subscription atomically, publishes the `subscription.update` event
to the target, replicates cross-site via the OUTBOX, and replies synchronously.
`room-worker` no longer participates in role updates.

## Subject

- Concrete: `chat.user.{account}.request.room.{roomID}.{siteID}.member.role-update`
- Wildcard: `chat.user.*.request.room.*.{siteID}.member.role-update`

Unchanged from the async design. `{siteID}` is the room's origin site (the site
that owns the room and runs the handling `room-service`). Queue group:
`room-service`. Subject parsing reuses `subject.ParseUserRoomSubject`.

## Wire Format

Request body — unchanged (`pkg/model`, `UpdateRoleRequest`):

```go
type UpdateRoleRequest struct {
    RoomID    string     `json:"roomId"`    // optional; server derives from subject
    Account   string     `json:"account"`   // target whose role changes
    NewRole   model.Role `json:"newRole"`   // "owner" (promote) | "member" (demote)
    Timestamp int64      `json:"timestamp"` // server-set; client omits
}
```

Success reply changes from `{"status":"accepted"}` to:

```json
{ "status": "ok" }
```

There is no `AsyncJobResult` and no `chat.user.{requester}.response.{requestID}`
event — role updates never emitted one.

## Handler Flow

`room-service/handler.go` — `handleUpdateRole(ctx, subj, data)`:

1. Parse subject → `(requester, roomID)`; unmarshal `UpdateRoleRequest`; reject
   a body `roomId` that disagrees with the subject.
2. **Validation (unchanged):** `newRole ∈ {owner, member}` (`errInvalidRole`);
   room is a channel (`errRoomTypeGuard`); requester is an owner
   (`errOnlyOwners`); load target via `GetSubscriptionWithMembership`
   (`errTargetNotMember`); promote→target not already owner (`errAlreadyOwner`);
   demote→target is owner (`errNotOwner`); promote→target has an individual
   membership (`errPromoteRequiresIndividual`); last-owner self-demote guard
   (`errCannotDemoteLast`).
3. `sub, err := h.store.SetOwnerRole(ctx, roomID, req.Account, req.NewRole == model.RoleOwner)`.
   - `errors.Is(err, model.ErrSubscriptionNotFound)` → `errTargetNotMember`
     (defensive: target removed between validate and mutate).
4. Publish `SubscriptionUpdateEvent` (`Action: "role_updated"`, `Subscription: *sub`)
   on `subject.SubscriptionUpdate(req.Account)`. **Publish failure here is
   non-fatal** — slog.Error and continue; the DB write is the source of truth.
   (Shared with mute/favorite via the `publishSubscriptionUpdate` helper.)
5. `userSiteID, err := h.store.GetUserSiteID(ctx, req.Account)`.
6. If `userSiteID != "" && userSiteID != h.siteID`: wrap the same marshaled
   `SubscriptionUpdateEvent` in `OutboxEvent{Type: "role_updated", …}` and
   publish to `subject.Outbox(h.siteID, userSiteID, "role_updated")` via
   `publishToStream`. Failure here **is** fatal.
7. Reply `{"status":"ok"}`.

Idempotency: not a toggle. The validation guards reject re-promote/re-demote, so
a retried RPC after success returns `errAlreadyOwner`/`errNotOwner` rather than a
second success — identical to the async design's validation.

## Errors

Reuses existing room-service role sentinels — no new error variables:
`errInvalidRole`, `errRoomTypeGuard`, `errOnlyOwners`, `errTargetNotMember`,
`errAlreadyOwner`, `errNotOwner`, `errPromoteRequiresIndividual`,
`errCannotDemoteLast`. All routed through `sanitizeError` before reaching the
client. A `SetOwnerRole` store/infra failure wraps to `internal` at the boundary.

## Store

### Interface (`room-service/store.go`)

```go
// SetOwnerRole atomically grants (makeOwner=true) or revokes (makeOwner=false)
// the owner role on the subscription keyed by (roomID, account) via a single
// FindOneAndUpdate. Other roles (e.g. member) are retained. Returns the
// updated subscription, or model.ErrSubscriptionNotFound (wrapped) when no match.
SetOwnerRole(ctx context.Context, roomID, account string, makeOwner bool) (*model.Subscription, error)
```

`GetUserSiteID` already exists — reused as-is.

### Mongo implementation (`room-service/store_mongo.go`)

An aggregation-pipeline `FindOneAndUpdate` (`ReturnDocument: After`) sharing the
`findOneAndUpdateSub` helper with `ToggleSubscriptionMute`/`ToggleSubscriptionFavorite`.
The `$set` recomputes `roles` with **order-preserving** expressions:

```go
// promote: append "owner" only when absent
{$cond: {if: {$in: ["owner", roles]}, then: roles, else: {$concatArrays: [roles, ["owner"]]}}}
// demote: drop "owner", then re-add "member" if absent
withoutOwner := {$filter: {input: roles, cond: {$ne: ["$$this", "owner"]}}}
{$cond: {if: {$in: ["member", withoutOwner]}, then: withoutOwner, else: {$concatArrays: [withoutOwner, ["member"]]}}}
```

Two key properties:

1. **Atomic** — single round-trip, no read-modify-write race.
2. **Empty-array safe on demote.** A **channel creator is seeded `roles: ["owner"]`**
   (`room-worker` `processCreateRoom`), so a plain `$filter` would demote it to
   `[]`. The `$cond` guard re-adds `member` — exactly like the worker's old
   `AddRole(member)` before `RemoveRole(owner)` — so the result is always `["member"]`.

`mongo.ErrNoDocuments` is wrapped with `model.ErrSubscriptionNotFound`.

### Indexes

No new indexes. The existing `(roomId, "u.account")` unique index covers the lookup.

## Cross-Site Federation

The publish *site* moves from `room-worker` to `room-service`; the wire contract
is byte-identical, so `inbox-worker` is untouched.

### Outbox publish (room-service)

Subject: `outbox.{roomSite}.to.{userSite}.role_updated`. The `OutboxEvent`'s
inner `Payload` is the JSON-encoded `SubscriptionUpdateEvent` (the full event,
not a compact struct — `inbox-worker.handleRoleUpdated` decodes exactly that).
JetStream stream: `OUTBOX_{roomSite}`.

### Inbox handler (inbox-worker) — unchanged

`inbox-worker.handleRoleUpdated` already decodes a `SubscriptionUpdateEvent` and
calls `UpdateSubscriptionRoles(account, roomID, roles)` (an idempotent absolute
`$set`). No code change.

### Dedup ID dropped

The async path passed a per-message `natsutil.OutboxDedupID` to guard against
JetStream redelivery of the canonical `ROOMS` message. That redelivery no longer
exists — the handler runs once per request, and a retry stops at the validation
guards before any publish. Cross-site duplicate delivery stays harmless because
`UpdateSubscriptionRoles` is idempotent. This matches the mute/favorite outbox
publishes, which also pass an empty msgID.

### Stream ownership

No `bootstrap.go` change. `OUTBOX`/`INBOX` are owned by ops/IaC.

## room-worker cutover

- Delete `processRoleUpdate` and its `.member.role-update` dispatch case in
  `HandleJetStreamMsg`. A stray role-update message then hits the existing
  `default` branch (logged + Ack'd); rollout ordering prevents that in practice.
- Remove the now-dead `AddRole` from `SubscriptionStore` + `MongoStore` — its
  only caller was `processRoleUpdate`. **Keep `RemoveRole`** — still used by the
  remove-member dual-membership path (strip owner when an individual membership
  is removed but the user stays via org).
- `room-service` no longer publishes the canonical `member.role-update` message.

## Behavioural rules

1. **Reply is `{"status":"ok"}`, not the full subscription.** The requester is
   usually not the target, so returning the target's private subscription fields
   (mute, `lastSeenAt`, `threadUnread`) is avoided.
2. **`subscription.update` goes to the target only**, not the requester — same as
   the async design.
3. **No notification/broadcast change.** Role changes don't affect delivery.

## Client API Doc

Per CLAUDE.md §5, the same PR updates the Update-Member-Role section of
`docs/client-api.md`: async-job/`accepted` → synchronous/`ok`, validation and
mutation happen in `room-service`, `subscription.update` (`role_updated`) to the
target plus a cross-site outbox for remote targets. Event schema unchanged.

## Testing (TDD)

### Handler unit tests (`room-service/handler_test.go`)

Built with a mocked `RoomStore` and captured `publishCore`/`publishToStream`
closures.

| Test | Setup | Asserts |
|------|-------|---------|
| `TestHandler_UpdateRole_Success` | promote; `GetUserSiteID` → same site | reply `{"ok"}`; one core publish (`role_updated`) on the target's subject; **no** stream publish (`t.Error` guard). |
| `TestHandler_UpdateRole_Demote_Success` | demote of `[member,owner]` | reply `{"ok"}`; event roles `[member]`. |
| `TestHandler_UpdateRole_CrossSiteOutbox` | `GetUserSiteID` → different site | stream publish on `outbox.site-a.to.site-b.role_updated`; payload decodes to `SubscriptionUpdateEvent`. |
| `TestHandler_UpdateRole_SetOwnerRoleNotFound` | `SetOwnerRole` → `ErrSubscriptionNotFound` | `errTargetNotMember`; no publishes. |
| `TestHandler_UpdateRole_PublishCoreError_NonFatal` | `publishCore` fails | still replies `{"ok"}`. |
| validation suite | non-owner, DM room, invalid role, already-owner, demote-non-owner, last-owner, org-only promote | each returns its sentinel; no mutation. |

`make generate SERVICE=room-service` regenerates `mock_store_test.go` with `SetOwnerRole`.

### Integration test (`room-service/integration_test.go`)

`TestMongoStore_SetOwnerRole_Integration` (tag `integration`): seed `[member]`,
promote → `[member,owner]` (exact order), promote again idempotent, demote →
`[member]`, demote again idempotent, **seed an `[owner]`-only creator and demote
→ `[member]`** (the empty-array guard), missing-sub → `ErrSubscriptionNotFound`.
Uses `testutil.MongoDB(t, "room-svc-role")`.

### room-worker

Remove the `processRoleUpdate` unit + dispatch tests and the `AddRole`
integration coverage; repoint the role integration test to `RemoveRole`.

### Coverage

The new handler/store paths hit every branch above. Expected ≥90% on the new
code; the 80% package floor applies.

## Out of Scope

- Request subject, `UpdateRoleRequest` schema, and validation rules — unchanged.
- The `subscription.update` event schema and the cross-site `role_updated`
  contract — only the publish site moves.
- `model.AsyncJobOpRoomMemberRoleUpdate` — left as-is; role-update never emitted
  an `AsyncJobResult`, so the constant is orthogonal.
- `notification-worker` — caches only mute state; roles aren't cached, so no
  canonical member event is added.

## Rollout

No stream or IaC change. Deploy **`room-service` first** (stops producing the
canonical message, starts handling synchronously), then **`room-worker`** (case
removed) — so no in-flight role-update message is dropped during the window.

## Risks

- **Durability downgrade vs the async path.** The async design persisted the
  request on the `ROOMS` stream before mutating, surviving a `room-service` crash
  mid-operation via JetStream redelivery. The synchronous path has no such
  replay: a crash between the Mongo write and the event publish leaves the role
  changed but the real-time `subscription.update` unsent (clients reconcile on
  next refetch). Accepted — role updates are rare and the mute/favorite RPCs
  already make this trade.
- **Non-idempotent retries.** A retry after a successful change returns
  `errAlreadyOwner`/`errNotOwner`. Same observable behavior as the async
  validation; clients debounce.
- **Cross-site mirror lag.** Between the room-site write and the home-site mirror,
  a home-site read returns the old roles. Acceptable — the target's session got
  the `subscription.update` directly.
