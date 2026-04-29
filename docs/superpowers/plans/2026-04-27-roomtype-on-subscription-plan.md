# RoomType on Subscription Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `RoomType` field to `model.Subscription`, add the new `RoomTypeBotDM` and `RoomTypeDiscussion` constants, and populate `RoomType` at every subscription creation site and on the partial Subscription payloads carried by removed `SubscriptionUpdateEvent`s.

**Architecture:** Denormalise the room kind onto the subscription document so downstream consumers (frontend room-list categorization, notification rules, future per-type handling) can route on `Subscription.RoomType` without an extra room lookup. Producers either read the type from the room (CreateRoom uses `req.Type`) or hardcode `RoomTypeChannel` for paths that are channel-only by upstream invariant — member-management ops (add/remove/role-update) and cross-site `member_added` events.

**Tech Stack:** Go 1.25, MongoDB driver v2 (`go.mongodb.org/mongo-driver/v2`), `go.uber.org/mock` (mockgen), `stretchr/testify` (assertions), NATS JetStream consumers via `pkg/idgen` and `pkg/subject`.

**Spec:** `docs/superpowers/specs/2026-04-23-roomtype-on-subscription-design.md`.

---

## Prerequisites

- Branch off `origin/main` after PR #118 ("Remove-member / role-update hardening") has merged. PR #118 already removed `RoomTypeGroup`, deleted `processInvite`, renamed `broadcast-worker`'s `publishGroupEvent` → `publishChannelEvent`, and updated `room-service`'s role-update guard. Do not redo any of that.
- All commands assume the repo root `/home/user/chat`. Use the `Makefile` targets — never raw `go` commands.
- Pre-commit hook runs lint + tests; commits fail if either fails.

## File Structure

| File | Responsibility | Touched in |
|---|---|---|
| `pkg/model/room.go` | Add new RoomType constants. | Task 1 |
| `pkg/model/subscription.go` | Add `RoomType` field on `Subscription`. | Task 1 |
| `pkg/model/model_test.go` | Assert new constants; round-trip the new field. | Task 1 |
| `room-service/handler.go` | Stamp `req.Type` onto the auto-created owner sub. | Task 2 |
| `room-service/handler_test.go` | Assert captured sub carries `RoomType`. | Task 2 |
| `room-worker/handler.go` | Hardcode `RoomTypeChannel` on `processAddMembers`, `processRemoveIndividual`, and `processRemoveOrg` Subscription literals. | Task 3 |
| `room-worker/handler_test.go` | Add `RoomType` assertions on `TestHandler_ProcessAddMembers` and `TestHandler_ProcessRemoveMember_SelfLeave_IndividualOnly`. | Task 3 |
| `inbox-worker/handler.go` | Hardcode `RoomTypeChannel` on cross-site `member_added` sub. | Task 4 |
| `inbox-worker/handler_test.go` | Assert created sub has `RoomType: RoomTypeChannel`. | Task 4 |

No store interfaces change. `room-worker`'s `SubscriptionStore` already has `GetRoom`; no `make generate` needed.

## Tasks

1. **`pkg/model` foundation** — new constants + new `Subscription.RoomType` field + model tests. Backward-compatible: existing call sites keep compiling because the new field defaults to `""`.
2. **`room-service` CreateRoom** — owner subscription carries `req.Type`.
3. **`room-worker`** — three sub literals updated, all hardcoding `RoomTypeChannel`:
   - `processAddMembers`: per-account sub creation.
   - `processRemoveIndividual`: partial sub literal carried by the "removed" `SubscriptionUpdateEvent`.
   - `processRemoveOrg`: same, in the per-account event loop.

   Member-management ops only apply to channel rooms — room-service rejects them for any other kind, so the hardcode is safe.
4. **`inbox-worker`** — cross-site `handleMemberAdded` hardcodes `RoomTypeChannel`. The originating site's `member.add` is gated by room-service to channels only, so any cross-site `member_added` event is always for a channel room.

---

### Task 1: `pkg/model` foundation

**Files:**
- Modify: `pkg/model/room.go` (the `RoomType` const block)
- Modify: `pkg/model/subscription.go` (the `Subscription` struct)
- Modify: `pkg/model/model_test.go` (`TestSubscriptionJSON`, `TestRoomTypeValues`)

This task is independently buildable: every existing call site keeps compiling because the new `Subscription.RoomType` field defaults to `""`.

- [ ] **Step 1.1: Update `TestSubscriptionJSON` to set the new field**

  Edit `pkg/model/model_test.go`. Find the `TestSubscriptionJSON` function and add `RoomType: model.RoomTypeChannel` between `RoomID` and `SiteID` so the round-trip exercises the new field:

  ```go
  func TestSubscriptionJSON(t *testing.T) {
      hss := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
      s := model.Subscription{
          ID:                 "s1",
          User:               model.SubscriptionUser{ID: "u1", Account: "alice"},
          RoomID:             "r1",
          RoomType:           model.RoomTypeChannel,
          SiteID:             "site-a",
          Roles:              []model.Role{model.RoleOwner},
          HistorySharedSince: &hss,
          JoinedAt:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
          LastSeenAt:         time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
          HasMention:         true,
      }
      // (rest of the test unchanged — it already round-trips s through JSON)
  }
  ```

- [ ] **Step 1.2: Update `TestRoomTypeValues` to assert the four constants**

  Replace the existing `TestRoomTypeValues` body with:

  ```go
  func TestRoomTypeValues(t *testing.T) {
      if model.RoomTypeChannel != "channel" {
          t.Errorf("RoomTypeChannel = %q", model.RoomTypeChannel)
      }
      if model.RoomTypeDM != "dm" {
          t.Errorf("RoomTypeDM = %q", model.RoomTypeDM)
      }
      if model.RoomTypeBotDM != "botDM" {
          t.Errorf("RoomTypeBotDM = %q", model.RoomTypeBotDM)
      }
      if model.RoomTypeDiscussion != "discussion" {
          t.Errorf("RoomTypeDiscussion = %q", model.RoomTypeDiscussion)
      }
  }
  ```

- [ ] **Step 1.3: Run the tests — expect a build failure**

  ```bash
  make test SERVICE=pkg/model
  ```

  Expected output (verbatim):
  ```text
  pkg/model/model_test.go:387:3: unknown field RoomType in struct literal of type model.Subscription
  pkg/model/model_test.go:416:11: undefined: model.RoomTypeBotDM
  pkg/model/model_test.go:419:11: undefined: model.RoomTypeDiscussion
  FAIL	github.com/hmchangw/chat/pkg/model [build failed]
  ```

- [ ] **Step 1.4: Add the new constants in `pkg/model/room.go`**

  Replace the existing `const ( ... )` block with:

  ```go
  const (
      RoomTypeChannel    RoomType = "channel"
      RoomTypeDM         RoomType = "dm"
      RoomTypeBotDM      RoomType = "botDM"
      RoomTypeDiscussion RoomType = "discussion"
  )
  ```

- [ ] **Step 1.5: Add the `RoomType` field on `Subscription`**

  Edit `pkg/model/subscription.go`. Insert the new field between `RoomID` and `SiteID`:

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

- [ ] **Step 1.6: Run the tests — expect green**

  ```bash
  make test SERVICE=pkg/model
  ```

  Expected:
  ```text
  ok  	github.com/hmchangw/chat/pkg/model	1.0xxs
  ok  	github.com/hmchangw/chat/pkg/model/cassandra	(cached)
  ```

- [ ] **Step 1.7: Lint clean**

  ```bash
  make lint
  ```

  Expected: `0 issues.`

- [ ] **Step 1.8: Commit**

  ```bash
  git add pkg/model/
  git commit -m "model: add RoomType on Subscription and botDM/discussion constants"
  ```

---

### Task 2: `room-service` CreateRoom owner subscription

**Files:**
- Modify: `room-service/handler.go` (the `handleCreateRoom` function — the `sub := model.Subscription{...}` literal)
- Modify: `room-service/handler_test.go` (`TestHandler_CreateRoom`)

Depends on Task 1 (`Subscription.RoomType` must exist).

- [ ] **Step 2.1: Add the RoomType assertion to `TestHandler_CreateRoom`**

  Edit `room-service/handler_test.go`. The test already captures the auto-created subscription via `var capturedSub *model.Subscription`. Append a `RoomType` assertion at the bottom of the test, just before the closing brace:

  ```go
  if capturedSub != nil && capturedSub.RoomType != model.RoomTypeChannel {
      t.Errorf("expected owner subscription RoomType=%q, got %q", model.RoomTypeChannel, capturedSub.RoomType)
  }
  ```

  The request fixture in this test already uses `Type: model.RoomTypeChannel`, so the assertion is internally consistent.

- [ ] **Step 2.2: Run the test — expect red**

  ```bash
  make test SERVICE=room-service
  ```

  Expected:
  ```text
  --- FAIL: TestHandler_CreateRoom (0.00s)
      handler_test.go:51: expected owner subscription RoomType="channel", got ""
  ```

- [ ] **Step 2.3: Stamp `req.Type` onto the owner subscription**

  Edit `room-service/handler.go` inside `handleCreateRoom`. The block `sub := model.Subscription{...}` builds the auto-created owner sub. Add `RoomType: req.Type,` between `RoomID` and `SiteID`:

  ```go
  // Auto-create owner subscription
  sub := model.Subscription{
      ID:                 idgen.GenerateID(),
      User:               model.SubscriptionUser{ID: req.CreatedBy, Account: req.CreatedByAccount},
      RoomID:             room.ID,
      RoomType:           req.Type,
      SiteID:             req.SiteID,
      Roles:              []model.Role{model.RoleOwner},
      HistorySharedSince: &now,
      JoinedAt:           now,
  }
  ```

- [ ] **Step 2.4: Run the test — expect green**

  ```bash
  make test SERVICE=room-service
  ```

  Expected: `ok  	github.com/hmchangw/chat/room-service	1.0xxs`

- [ ] **Step 2.5: Lint clean**

  ```bash
  make lint
  ```

  Expected: `0 issues.`

- [ ] **Step 2.6: Commit**

  ```bash
  git add room-service/
  git commit -m "room-service: populate Subscription.RoomType on CreateRoom"
  ```

---

### Task 3: `room-worker` — add and remove paths

**Files:**
- Modify: `room-worker/handler.go`
  - `processAddMembers` — `sub := &model.Subscription{...}` literal inside the per-account loop
  - `processRemoveIndividual` — the partial `model.Subscription{...}` literal inside the `SubscriptionUpdateEvent`
  - `processRemoveOrg` — the partial `model.Subscription{...}` literal inside the per-account event loop
- Modify: `room-worker/handler_test.go`
  - `TestHandler_ProcessAddMembers`
  - `TestHandler_ProcessRemoveMember_SelfLeave_IndividualOnly`

Depends on Task 1. No store interface changes; no `make generate` needed.

**Design notes:**

All three sub literals hardcode `RoomTypeChannel`. Member-management ops
(`member.add`, `member.remove`, `member.role-update`) only apply to channel
rooms — room-service rejects them for any other kind before they reach the
room-worker stream. Hardcoding mirrors `processAddMembers` and avoids a
runtime `GetRoom` round-trip.

- [ ] **Step 3.1: Update `TestHandler_ProcessAddMembers` — assert RoomType on every created sub**

  Inside the existing `BulkCreateSubscriptions` `DoAndReturn` callback, add a per-sub assertion:

  ```go
  store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).DoAndReturn(
      func(_ context.Context, subs []*model.Subscription) error {
          assert.Len(t, subs, 2)
          for _, s := range subs {
              assert.Equal(t, "site-a", s.SiteID)
              assert.Equal(t, model.RoomTypeChannel, s.RoomType)
              assert.Equal(t, []model.Role{model.RoleMember}, s.Roles)
              require.NotNil(t, s.HistorySharedSince)
              assert.Equal(t, s.JoinedAt, *s.HistorySharedSince)
          }
          return nil
      })
  ```

- [ ] **Step 3.2: Update `TestHandler_ProcessRemoveMember_SelfLeave_IndividualOnly` — assert RoomType on the published event payload**

  After the `assert.True(t, subjSet[subject.MemberEvent(roomID)], ...)` line, add:

  ```go
  for _, p := range published {
      if p.subj != subject.SubscriptionUpdate(account) {
          continue
      }
      var evt model.SubscriptionUpdateEvent
      require.NoError(t, json.Unmarshal(p.data, &evt))
      assert.Equal(t, model.RoomTypeChannel, evt.Subscription.RoomType, "subscription update should carry RoomType")
  }
  ```

  This single assertion covers the contract for both `processRemoveIndividual` and `processRemoveOrg` — the shape of the partial Subscription literal is identical at both sites.

- [ ] **Step 3.3: Run tests — expect red**

  ```bash
  make test SERVICE=room-worker
  ```

  Expected failures: `TestHandler_ProcessAddMembers` (`RoomType = ""`) and `TestHandler_ProcessRemoveMember_SelfLeave_IndividualOnly` (`RoomType = ""`).

- [ ] **Step 3.4: Hardcode `RoomTypeChannel` on `processAddMembers` subs**

  In `room-worker/handler.go` find the literal inside `processAddMembers`:

  ```go
  // RoomType is fixed to channel: room-service rejects member.add for
  // any other room kind.
  sub := &model.Subscription{
      ID:       idgen.GenerateID(),
      User:     model.SubscriptionUser{ID: user.ID, Account: user.Account},
      RoomID:   req.RoomID,
      RoomType: model.RoomTypeChannel,
      SiteID:   room.SiteID,
      Roles:    []model.Role{model.RoleMember},
      JoinedAt: acceptedAt,
  }
  ```

- [ ] **Step 3.5: Hardcode `RoomTypeChannel` on the partial Subscription in `processRemoveIndividual`**

  Inside `processRemoveIndividual`, replace the `Subscription:` field of the `SubscriptionUpdateEvent` with:

  ```go
  // Subscription update event. RoomType is fixed to channel: room-service
  // rejects member.remove for any other room kind.
  subEvt := model.SubscriptionUpdateEvent{
      UserID: user.ID,
      Subscription: model.Subscription{
          RoomID:   req.RoomID,
          RoomType: model.RoomTypeChannel,
          User:     model.SubscriptionUser{ID: user.ID, Account: req.Account},
      },
      Action:    "removed",
      Timestamp: now.UnixMilli(),
  }
  ```

- [ ] **Step 3.6: Hardcode `RoomTypeChannel` on the partial Subscription in `processRemoveOrg`**

  Inside `processRemoveOrg`'s per-account loop, replace the `Subscription:` field with:

  ```go
  subEvt := model.SubscriptionUpdateEvent{
      Subscription: model.Subscription{
          RoomID:   req.RoomID,
          RoomType: model.RoomTypeChannel,
          User:     model.SubscriptionUser{Account: m.Account},
      },
      Action:    "removed",
      Timestamp: now.UnixMilli(),
  }
  ```

- [ ] **Step 3.7: Run tests — expect green**

  ```bash
  make test SERVICE=room-worker
  ```

  Expected: `ok  	github.com/hmchangw/chat/room-worker	1.0xxs`

- [ ] **Step 3.8: Lint clean**

  ```bash
  make lint
  ```

  Expected: `0 issues.`

- [ ] **Step 3.9: Commit**

  ```bash
  git add room-worker/
  git commit -m "room-worker: populate Subscription.RoomType on add and remove"
  ```

---

### Task 4: `inbox-worker` cross-site member_added

**Files:**
- Modify: `inbox-worker/handler.go` (the `sub := &model.Subscription{...}` literal inside `handleMemberAdded`)
- Modify: `inbox-worker/handler_test.go` (`TestHandleEvent_MemberAdded`)

Depends on Task 1.

Cross-site `member_added` events only fire for rooms that support add-member (channels and discussions). Hardcoding `RoomTypeChannel` mirrors `room-worker.processAddMembers` so every persisted subscription carries a non-empty `RoomType`.

- [ ] **Step 4.1: Add the RoomType assertion to `TestHandleEvent_MemberAdded`**

  In `inbox-worker/handler_test.go`, find the per-field assertion block on the captured `sub`. After the `Roles` assertion and before the `sub.ID == ""` check, add:

  ```go
  if sub.RoomType != model.RoomTypeChannel {
      t.Errorf("subscription RoomType = %q, want %q", sub.RoomType, model.RoomTypeChannel)
  }
  ```

- [ ] **Step 4.2: Run tests — expect red**

  ```bash
  make test SERVICE=inbox-worker
  ```

  Expected:
  ```text
  --- FAIL: TestHandleEvent_MemberAdded (0.00s)
      handler_test.go:222: subscription RoomType = "", want "channel"
  ```

- [ ] **Step 4.3: Hardcode `RoomTypeChannel` on the cross-site sub**

  In `inbox-worker/handler.go` find the literal inside `handleMemberAdded`. Add `RoomType: model.RoomTypeChannel,` between `RoomID` and `SiteID`:

  ```go
  sub := &model.Subscription{
      ID:                 idgen.GenerateID(),
      User:               model.SubscriptionUser{ID: user.ID, Account: user.Account},
      RoomID:             event.RoomID,
      RoomType:           model.RoomTypeChannel,
      SiteID:             event.SiteID,
      Roles:              []model.Role{model.RoleMember},
      HistorySharedSince: historySharedSince,
      JoinedAt:           joinedAt,
  }
  ```

- [ ] **Step 4.4: Run tests — expect green**

  ```bash
  make test SERVICE=inbox-worker
  ```

  Expected: `ok  	github.com/hmchangw/chat/inbox-worker	1.0xxs`

- [ ] **Step 4.5: Lint clean**

  ```bash
  make lint
  ```

  Expected: `0 issues.`

- [ ] **Step 4.6: Commit**

  ```bash
  git add inbox-worker/
  git commit -m "inbox-worker: populate RoomTypeChannel on cross-site member_added"
  ```

---

## Final verification

After all four tasks land, sweep the whole repo and push:

- [ ] **V.1: Full test sweep**

  ```bash
  make test
  ```

  Expected: every package shows `ok ...` (no `FAIL`). The `pkg/cassutil`, `pkg/oidc`, `pkg/otelutil`, `pkg/userstore`, etc. lines may show `[no test files]` — that's normal.

- [ ] **V.2: Full lint sweep**

  ```bash
  make lint
  ```

  Expected: `0 issues.`

- [ ] **V.3: Integration tests (where Docker is available)**

  ```bash
  make test-integration SERVICE=room-worker
  make test-integration SERVICE=room-service
  make test-integration SERVICE=inbox-worker
  ```

  These require Docker for testcontainers. Skip if not available locally — CI will exercise them.

- [ ] **V.4: Push to the feature branch**

  ```bash
  git push -u origin claude/add-roomtype-subscription-Uqow3
  ```

  If the branch already has the previous (pre-rebase) commits on the remote, force-push with lease:
  ```bash
  git push --force-with-lease origin claude/add-roomtype-subscription-Uqow3
  ```

## Risks and rollback

- **Old subscriptions without `roomType`.** Returned as `RoomType: ""`. No code in this branch reads the field, so old documents are harmless until a future consumer relies on it. A backfill job is out of scope.
- **A future room kind that supports member-management.** All three room-worker sites and inbox-worker hardcode `RoomTypeChannel` because room-service rejects member ops for non-channels today. If room-service is ever extended to allow `member.add`/`member.remove`/`member.role-update` on a different room kind (e.g., `discussion`), every hardcoded site must be revisited or the persisted `RoomType` will be wrong. Test assertions and the explanatory `// RoomType is fixed to channel: ...` comments at each site exist to surface this.
- **Future creation site forgets to set `RoomType`.** Mitigated by per-site test assertions in Tasks 2-4.
- **Rollback:** revert the four implementation commits in reverse order. Reverting only the model commit (Task 1) would break compilation of the call sites that use the new field — revert all four together if needed.
