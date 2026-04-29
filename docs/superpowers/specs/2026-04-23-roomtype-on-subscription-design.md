# Design: RoomType on Subscription

## Summary

Add a `RoomType` field to `model.Subscription` so that every subscription record
carries the kind of room it belongs to. Add the new `RoomTypeBotDM` and
`RoomTypeDiscussion` constants to align the `RoomType` enum with the product's
current taxonomy: `dm`, `channel`, `botDM`, `discussion`. The legacy
`RoomTypeGroup = "group"` constant was already removed by PR #118 — this change
does not redo that work.

## Motivation

Today, any consumer that needs to know whether a subscription belongs to a DM,
channel, bot DM, or discussion must look up the room document. Denormalising
the room type onto the subscription lets consumers decide how to handle events
(UI rendering, notification routing, fan-out strategy) using only the
subscription payload. The rename from `group` to `channel` follows the
product's current naming.

## Scope

### In scope

1. `pkg/model/room.go` — add `RoomTypeBotDM` and `RoomTypeDiscussion` constants.
2. `pkg/model/subscription.go` — new `RoomType` field.
3. All production code that creates a `model.Subscription` — populate the new
   field.
4. The two `room-worker` "removed" event payloads — populate the new field on
   the partial `Subscription` literal carried by `SubscriptionUpdateEvent`.
5. Unit tests updated accordingly.

### Out of scope

- Data migration/backfill of existing subscriptions. Old Mongo documents keep
  `RoomType: ""` until they are rewritten by normal traffic or a separate
  backfill script (not part of this PR).
- New fan-out behaviour for `botDM` or `discussion` in `broadcast-worker` —
  those types fall through to the existing default warning branch.
- Any UI/client changes.

## Design

### 1. `pkg/model/room.go`

```go
const (
    RoomTypeChannel    RoomType = "channel"
    RoomTypeDM         RoomType = "dm"
    RoomTypeBotDM      RoomType = "botDM"
    RoomTypeDiscussion RoomType = "discussion"
)
```

- Add `RoomTypeBotDM` and `RoomTypeDiscussion`.
- `RoomTypeChannel` and `RoomTypeDM` are unchanged. `RoomTypeGroup` was already
  removed by PR #118.

### 2. `pkg/model/subscription.go`

Add `RoomType` to the `Subscription` struct between `RoomID` and `SiteID`:

```go
type Subscription struct {
    ID                 string           `json:"id" bson:"_id"`
    User               SubscriptionUser `json:"u" bson:"u"`
    RoomID             string           `json:"roomId" bson:"roomId"`
    RoomType           RoomType         `json:"roomType" bson:"roomType"`
    SiteID             string           `json:"siteId" bson:"siteId"`
    Roles              []Role           `json:"roles" bson:"roles"`
    HistorySharedSince *time.Time       `json:"historySharedSince,omitempty" bson:"historySharedSince,omitempty"`
    JoinedAt           time.Time        `json:"joinedAt" bson:"joinedAt"`
    LastSeenAt         time.Time        `json:"lastSeenAt" bson:"lastSeenAt"`
    HasMention         bool             `json:"hasMention" bson:"hasMention"`
}
```

### 3. Subscription creation sites

| Site | Source of `RoomType` |
|---|---|
| `room-service/handler.go` `handleCreateRoom` (owner subscription) | `req.Type` (the type the room is being created as) |
| `room-worker/handler.go` `processAddMembers` | Hardcoded `RoomTypeChannel`. Member-management ops (add/remove/role) only apply to channel rooms — room-service rejects them for any other kind. |
| `inbox-worker/handler.go` `handleMemberAdded` | Hardcoded `RoomTypeChannel`. Cross-site `member_added` events only originate from channel rooms. |

`processInvite` is intentionally absent: PR #118 ("Remove-member /
role-update hardening", merged Apr 24 to main) removed the single-user
invite flow entirely. The `.member.invite` subject is no longer wired up
in `HandleJetStreamMsg`, and the function does not exist on main.

### 4. Partial `Subscription` payloads on removed events

In `room-worker/handler.go` the "removed" variants of `SubscriptionUpdateEvent`
construct a partial `Subscription` literal. Hardcode `RoomType` to
`RoomTypeChannel` at both sites:

- `processRemoveIndividual`: stamp `RoomType: model.RoomTypeChannel` on the
  partial Subscription literal.
- `processRemoveOrg`: same — every per-account event in the loop carries
  `RoomTypeChannel`.

The hardcode mirrors `processAddMembers` and rests on the same invariant:
room-service is the single entry point for member-management requests and
rejects every kind except channel before they reach the room-worker stream.
No runtime `GetRoom` lookup is required.

### 5. `RoomTypeGroup` → `RoomTypeChannel` renames

PR #118 already removed `RoomTypeGroup` from `pkg/model/room.go`, renamed
`broadcast-worker`'s switch case + `publishGroupEvent` → `publishChannelEvent`,
and updated `room-service`'s role-update guard plus the `errRoomTypeGuard`
sentinel message. No additional renames are required in this change.

### 6. Store interfaces

No new store methods required. `room-worker` already has `GetRoom` in its
`SubscriptionStore` interface.

## Test plan

Following the repository's TDD rules: write failing tests, then implement.

- `pkg/model/model_test.go`
  - Update `TestRoomTypeValues`: assert the new set of constants.
  - Add a `Subscription` round-trip test case that sets `RoomType` and
    confirms it survives JSON and BSON marshal/unmarshal.
- `room-service/handler_test.go`
  - `TestCreateRoom`: assert the subscription captured by the store mock has
    `RoomType` equal to the request's `Type`.
- `room-worker/handler_test.go`
  - `processAddMembers`: assert each created subscription has
    `RoomType: RoomTypeChannel`.
  - `processRemoveIndividual`: assert the published `SubscriptionUpdateEvent`
    payload has `Subscription.RoomType` equal to the fetched room's type.
  - `processRemoveOrg`: same assertion across the per-account events.
- `inbox-worker/handler_test.go`
  - `handleMemberAdded`: assert each created subscription has
    `RoomType: RoomTypeChannel`.
- Run `make generate` to refresh mocks (no interface changes expected, but run
  for safety).
- `make lint`, `make test`, `make test-integration` must pass.

## Commit strategy

Five commits on branch `claude/add-roomtype-subscription-Uqow3`, each leaving
the tree green:

1. Spec doc.
2. `pkg/model`: add the new constants and the `RoomType` field, plus model tests.
3. `room-service`: populate `RoomType` on the CreateRoom owner subscription.
4. `room-worker`: populate `RoomType` on `processAddMembers`,
   `processRemoveIndividual`, and `processRemoveOrg`.
5. `inbox-worker`: populate `RoomType` on the cross-site `member_added` handler.

## Risks

- **Old subscriptions without `roomType`.** Returned as `RoomType: ""`. No
  code currently reads the field, so this is harmless until a consumer starts
  relying on it. A future backfill job (out of scope) can populate old
  records.
- **Client code consuming `SubscriptionUpdateEvent.Subscription.RoomType` on
  removed events.** Covered by populating the field from the fetched room; if
  the fetch fails the field is empty, same as before the change.
- **Silent data drift** if a future creation site forgets to set
  `RoomType`. Mitigated by the test plan above, which asserts `RoomType` on
  every known creation path.
