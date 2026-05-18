# Consolidate Cross-Site Room-Creation Federation Event — Design

**Date:** 2026-05-12
**Status:** Draft
**Services:** `room-worker`, `inbox-worker`, `search-sync-worker` (test-only)
**Related specs:**
- `2026-05-01-federated-room-origin-site-mv-fix-design.md` (PR #145 — added cross-site `member_added` for add-members)
- `2026-05-11-create-room-origin-site-mv-fix-design.md` (PR #169 — added cross-site `member_added` for room creation, leaving `room_created` in place)

## Problem

Today every room-creation publishes two cross-site OUTBOX events per remote site:

| Event | Consumer on remote site | Purpose |
|---|---|---|
| `outbox.{origin}.to.{remote}.room_created` | `inbox-worker.handleRoomCreated` | Build correctly-shaped `Subscription` rows (DM/botDM/channel) |
| `outbox.{origin}.to.{remote}.member_added` | `search-sync-worker` (MV update) and `inbox-worker.handleMemberAdded` (dup-key no-op for fresh rooms) | Search index update |

The redundancy is a layering artifact: PR #142 (Vinayak, `feat(create-room): DM, botDM, and channel creation across sites`) introduced `room_created` as the cross-site federation event before the `member_added` lane existed end-to-end. PR #145 then added the `member_added` cross-site event for the add-members path, and PR #169 extended `member_added` to the create path. At that point `room_created` became redundant for everything except the per-room-type sub-shape logic inside `inbox-worker.handleMemberAdded`, which is hardcoded to `RoomType: channel`.

### Two concrete consequences

1. **2× outbound publish per remote site per room creation.** `room-worker.finishCreateRoom` emits both events for every remote member's site.
2. **Latent search bug in `search-sync-worker/spotlight.go`**: it writes `RoomType: string(evt.RoomType)` into the spotlight ES doc, but today's `MemberAddEvent` wire payload doesn't carry `RoomType` — so for every room indexed via `member_added` since PR #145, the spotlight doc's `roomType` field has been the empty string. Room typeahead UI loses type info. This consolidation incidentally heals the bug because room-worker will start populating `RoomType` on every `member_added` publish.

## Goals

- One cross-site federation event per room creation. `member_added` does double duty: drives sub creation in `inbox-worker` and MV updates in `search-sync-worker`.
- `inbox-worker.handleMemberAdded` builds the right `Subscription` shape for every room type (channel, DM, botDM), reusing the existing per-type helpers (`subscriptionName`, `rolesForType`, `subscriptionIsSubscribed`) that `handleRoomCreated` uses today.
- Full removal of the `room_created` event type, its model, its handler, and its tests. No shim, no transition flag.
- No subject or stream changes — same wire lanes, fewer wire formats.

## Non-Goals

- **Backward compatibility for cross-tree consumers of `RoomCreatedOutbox` / `OutboxTypeRoomCreated`.** Grep confirms no out-of-tree consumer exists today.
- **Bridging in-flight `room_created` events during the deploy window.** Both ends ship in the same release per site; any straggler federated event arriving at a new `inbox-worker` pod hits the existing `default` case in the event-type switch and logs `"unknown event type, skipping"`. The corresponding `member_added` for the same room arrives moments later and does the right thing.
- **Refactoring `MemberRemoveEvent`** the same way. Remove is already type-agnostic in `inbox-worker.handleMemberRemoved`; only `member_added` needs the per-type sub-shape logic.
- **Search-sync-worker code changes.** Zero structural change required. No new tests added; existing spotlight coverage already asserts `roomType` via `baseInboxMemberEvent`.

## Design

### Wire format

Extend `model.MemberAddEvent` with two `omitempty` fields:

```go
type MemberAddEvent struct {
    Type               string   `json:"type"`
    RoomID             string   `json:"roomId"`
    RoomName           string   `json:"roomName"`
    Accounts           []string `json:"accounts"`
    SiteID             string   `json:"siteId"`
    JoinedAt           int64    `json:"joinedAt"`
    HistorySharedSince *int64   `json:"historySharedSince,omitempty"`
    Timestamp          int64    `json:"timestamp"`

    // NEW
    RoomType         RoomType `json:"roomType,omitempty"`
    RequesterAccount string   `json:"requesterAccount,omitempty"`
}
```

- `RoomType`: drives `inbox-worker.handleMemberAdded`'s sub-shape branch and `search-sync-worker.spotlight.go`'s ES doc population. Zero value (`""`) is treated as `RoomTypeChannel` for backward compatibility with any in-flight publishes during deploy.
- `RequesterAccount`: only meaningful when `RoomType` is `DM` or `BotDM` — used by `subscriptionName` to compute the counterpart account for the recipient sub's `Name`. Empty/unused for channels.

The two added fields are `omitempty` so wire payloads from older publishers and from add-member operations (which don't need the fields) stay unchanged.

### Publisher side — `room-worker/handler.go`

Two publish call sites populate the new fields:

1. **`finishCreateRoom`** — the per-remote-site `member_added` OUTBOX publish (added in #169) sets `RoomType: room.Type` and `RequesterAccount: requester.Account`. The origin-local INBOX publish does the same for consistency (search-sync-worker on the origin reads it).
2. **`finishCreateRoom`** — delete the per-remote-site `room_created` OUTBOX publish entirely (the block constructing `RoomCreatedOutbox`).
3. **`processAddMembers`** — both publish sites (local INBOX + per-remote-site OUTBOX) get `RoomType: room.Type` and `RequesterAccount: req.RequesterAccount`. Channels only, so `RequesterAccount` won't actually be read on the consumer side, but keeping it consistent across all `member_added` emissions reduces special-case surface in publisher code.

### Consumer side — `inbox-worker/handler.go`

`handleMemberAdded` dispatches on `event.RoomType` and uses the existing helpers, refactored to take primitives so they're callable from both old-and-removed `handleRoomCreated` and the new `handleMemberAdded` path:

**Before** (lives in handler.go around line 227):
```go
func subscriptionName(d *model.RoomCreatedOutbox, u *model.User) string { ... }
func subscriptionIsSubscribed(d *model.RoomCreatedOutbox, u *model.User) bool { ... }
```

**After** (primitives, no struct dependency):
```go
func subscriptionName(roomType model.RoomType, roomName, requesterAccount string, u *model.User) string { ... }
func subscriptionIsSubscribed(roomType model.RoomType, u *model.User) bool { ... }
```

`rolesForType(t model.RoomType) []Role` already takes a primitive — unchanged.

`handleMemberAdded` post-refactor:

```go
func (h *Handler) handleMemberAdded(ctx context.Context, evt *model.OutboxEvent) error {
    var event model.MemberAddEvent
    if err := json.Unmarshal(evt.Payload, &event); err != nil {
        return fmt.Errorf("unmarshal member_added payload: %w", err)
    }

    roomType := event.RoomType
    if roomType == "" {
        roomType = model.RoomTypeChannel  // backward-compat for older publishers
    }

    users, err := h.store.FindUsersByAccounts(ctx, event.Accounts)
    if err != nil {
        return fmt.Errorf("find users by accounts: %w", err)
    }
    userMap := make(map[string]model.User, len(users))
    for i := range users {
        userMap[users[i].Account] = users[i]
    }

    joinedAt := time.UnixMilli(event.JoinedAt).UTC()
    var historySharedSince *time.Time
    if event.HistorySharedSince != nil && *event.HistorySharedSince > 0 {
        t := time.UnixMilli(*event.HistorySharedSince).UTC()
        historySharedSince = &t
    }

    subs := make([]*model.Subscription, 0, len(event.Accounts))
    for _, account := range event.Accounts {
        u, ok := userMap[account]
        if !ok {
            slog.Warn("user not found for account", "account", account)
            continue
        }
        sub := &model.Subscription{
            ID:                 idgen.GenerateUUIDv7(),
            User:               model.SubscriptionUser{ID: u.ID, Account: u.Account},
            RoomID:             event.RoomID,
            RoomType:           roomType,
            SiteID:             event.SiteID,
            Roles:              rolesForType(roomType),
            Name:               subscriptionName(roomType, event.RoomName, event.RequesterAccount, &u),
            IsSubscribed:       subscriptionIsSubscribed(roomType, &u),
            HistorySharedSince: historySharedSince,
            JoinedAt:           joinedAt,
        }
        subs = append(subs, sub)
    }

    if err := h.store.BulkCreateSubscriptions(ctx, subs); err != nil {
        if !mongo.IsDuplicateKeyError(err) {
            return fmt.Errorf("bulk create subscriptions: %w", err)
        }
    }
    return nil
}
```

The old `handleRoomCreated` function is **deleted**. The `case model.MessageTypeRoomCreated:` arm in `HandleEvent`'s switch is also deleted.

### Removal sweep

- `pkg/model/event.go`:
  - Delete `type RoomCreatedOutbox struct`.
  - Delete `OutboxTypeRoomCreated` constant (cross-site OUTBOX event type).
  - **Do not delete** `MessageTypeRoomCreated` constant — it's a distinct constant for the channel-create system-message type ("alice created the room"), used by `room-worker`'s `publishChannelSysMessages` and unrelated to federation. The two constants happen to share the string value `"room_created"` but live on different code paths.
- `inbox-worker/handler.go`:
  - Delete `handleRoomCreated`.
  - Delete the `case model.MessageTypeRoomCreated:` arm.
  - Refactor `subscriptionName` / `subscriptionIsSubscribed` to take primitives.
- `inbox-worker/handler_test.go`: delete `TestHandleRoomCreated*` cases (5 of them per grep). Replace with new `TestHandleMemberAdded_DM*` / `TestHandleMemberAdded_BotDM*` cases.
- `inbox-worker/integration_test.go`: delete `TestHandleRoomCreatedPersistsRemoteSubs` and `TestHandleRoomCreatedDM_PersistsRemoteCounterpartSub` (2 cases). Replace with `TestHandleMemberAdded_Channel_PersistsRemoteSubs` and `TestHandleMemberAdded_DM_PersistsRemoteCounterpartSub` going through the public `HandleEvent` entry point.
- `room-worker/handler.go::finishCreateRoom`: delete the per-remote-site `room_created` publish block. Add `RoomType` + `RequesterAccount` to the existing `member_added` publishes (both local-INBOX and cross-site OUTBOX).
- `room-worker/handler.go::processAddMembers`: add `RoomType` + `RequesterAccount` to all three publish sites (local INBOX, cross-site OUTBOX, the older add-members publish if separate).
- `room-worker/handler_test.go`: assert `RoomType` is populated on every `member_added` publish capture; delete any test that asserted on the `room_created` outbox publishes.
- `room-worker/integration_test.go`: `TestProcessCreateRoomChannel_OutboxPerRemoteSite` and `TestProcessCreateRoomDM_OutboxToCounterpartSite` updated to assert only one outbox per dest site (the `member_added`), drop the `room_created` assertions.

### Search-sync-worker impact

Zero structural change. `model.InboxMemberEvent` (the struct `search-sync-worker.parseMemberEvent` decodes into) already has `RoomType` — it's the publisher side that didn't populate it. Once `room-worker` starts setting `RoomType` on every `member_added` publish, the existing `string(evt.RoomType)` write at `spotlight.go:120` produces the correct value for the first time.

No new search-sync-worker tests needed. The existing `TestSpotlightCollection_BuildAction_MemberAdded` already asserts `assert.Equal(t, "channel", doc["roomType"])` via `baseInboxMemberEvent()` (which sets `RoomType: model.RoomTypeChannel`), so the regression guard for the latent spotlight-roomType-empty bug is already in place.

### Idempotency

The unique index on `subscriptions.(roomId, u.account)` keeps concurrent and redelivered creates idempotent. `handleMemberAdded` swallows `mongo.IsDuplicateKeyError` (see the bulk-create branch in the code example above) and continues, so JetStream replays of `member_added` after a crashed prior delivery do not nak-loop. Non-duplicate errors propagate so JetStream retries until success or `MaxDeliver` is hit.

## Testing

Unit + integration coverage, per spec.

### `inbox-worker/handler_test.go`

New tests covering per-room-type sub-shape via `handleMemberAdded`:

- `TestHandleMemberAdded_Channel_BuildsChannelSub` (regression — covers the previous behavior, with `RoomType` field set explicitly on the event).
- `TestHandleMemberAdded_Channel_DefaultsWhenRoomTypeEmpty` — backward-compat: event with empty `RoomType` is treated as channel (older publishers).
- `TestHandleMemberAdded_DM_BuildsRecipientSubWithCounterpartName` — asserts `Name = RequesterAccount`, `Roles = nil`, `IsSubscribed = false`.
- `TestHandleMemberAdded_BotDM_BuildsBotSub` — asserts `IsSubscribed` depends on `isBot(u.Account)`.
- `TestHandleMemberAdded_DuplicateKey_IsIdempotent` — bulk-create returns dup-key; handler returns nil.

Delete `TestHandleRoomCreated*` cases (5 total per current `grep`).

### `inbox-worker/integration_test.go`

Delete `TestHandleRoomCreatedPersistsRemoteSubs` and `TestHandleRoomCreatedDM_PersistsRemoteCounterpartSub`. Replace with `TestHandleMemberAdded_Channel_PersistsRemoteSubs` and `TestHandleMemberAdded_DM_PersistsRemoteCounterpartSub` — both go through the public `HandleEvent` entry point with `member_added` payloads carrying the new `RoomType` + `RequesterAccount` fields, and assert the persisted Subscription rows have the right shape.

### `room-worker/handler_test.go`

Update existing PR #169 tests to assert `RoomType` and `RequesterAccount` are populated on the captured publishes. New test:

- `TestProcessCreateRoom_DM_OutboxCarriesRoomTypeAndRequester` — DM create across sites, captures the cross-site `member_added` publish, asserts inner `RoomType = DM` and `RequesterAccount = alice`.

### `room-worker/integration_test.go`

- `TestProcessCreateRoomChannel_OutboxPerRemoteSite` — drop the `room_created` outbox assertions. Assert exactly one outbox per dest site (`member_added`), with `RoomType=channel` on the payload.
- `TestProcessCreateRoomDM_OutboxToCounterpartSite` — same shape. Assert `RoomType=DM`, `RequesterAccount=alice` on the payload.

### `search-sync-worker/spotlight_test.go`

No new tests. The existing `TestSpotlightCollection_BuildAction_MemberAdded` already asserts `doc["roomType"] == "channel"` via `baseInboxMemberEvent()`, so the regression guard for the latent spotlight-roomType-empty bug is in place without code change.

## Rollout

Single PR, both publisher and consumer changes shipped together.

**Pre-production context**: cross-site federation is not yet integrated end-to-end in any deployed environment. No live cross-site DM traffic exists today, so the theoretical mixed-version DM-sub-malformation window during a mid-deploy creation has no real-world incidence. This rollout plan is therefore straightforward — when federation does go live in prod, both this PR and its schema extension will already be in place everywhere.

### Deploy order (defensive, not strictly required)

For belt-and-suspenders correctness in case federation traffic does materialize during a rolling deploy, prefer deploying `room-worker` ahead of `inbox-worker` per site. This ensures any new `member_added` events on the wire already carry `RoomType` + `RequesterAccount` before the consumer side starts dispatching on those fields.

In-flight `room_created` events that arrive at the new `inbox-worker` (because they were redelivered from before its deploy) hit the existing `default` case in the event-type switch → `slog.Warn("unknown event type, skipping")` + ack. No nak-loop, no behavior regression.

### Per-site verification after deploy

1. Create a federated DM (alice@s1 → bob@s2). Verify on s2 the recipient's `Subscription` doc has `Name = "alice"`, `Roles = nil`, `IsSubscribed = false`.
2. Create a federated channel with one cross-site member. Verify on the remote site the member's `Subscription` doc has `Name = roomName`, `Roles = ["member"]`, `IsSubscribed = false`.
3. Query the spotlight ES index for the new rooms — confirm `roomType` is `"channel"` / `"dm"` / `"botDM"` (not empty).
4. `nats stream info OUTBOX_{site}` should show `member_added` events flowing at room-creation rate; `room_created` subject should report zero new messages.

## Observability

- **Logs**: one new `slog.Warn` per straggler `room_created` event during the deploy window — expected, transient. After deploy completes, zero.
- **Metrics**: existing JetStream metrics on `OUTBOX_{site}` will show `member_added` subject throughput rise (it now carries create-time too), `room_created` subject throughput drop to zero.
- **Traces**: unchanged — `member_added` was already on the trace path for create via PR #169.

## Risks

- **Mixed-version deploy window** (theoretical, no real-world incidence today). Cross-site federation is not yet integrated end-to-end, so no DM federation traffic exists during a rolling deploy of this PR. If federation traffic did flow during the window, a DM creation initiated by a new `room-worker` pod could be consumed by an old `inbox-worker` pod's channel-hardcoded `handleMemberAdded` and produce a recipient sub with `Name=""` (empty counterpart name). The defect would be bounded to the deploy duration and recoverable via either a manual script that re-derives `Name` from the room's home-site sub, or by churn (any later add-member re-emits with correct schema). Not blocking under current operational reality.
- **Removal of `RoomCreatedOutbox` is a one-way ratchet.** No out-of-tree consumer exists today (`grep` confirms). If we ever want to reintroduce the dedicated event, the cost is just adding the type back; the lane is unchanged.
