# BotDM Re-subscribe Upsert + `DisableNotification` Field — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Re-creating a botDM for a user who already has a subscription must refresh `DisableNotification → false`, `IsSubscribed → true`, and `JoinedAt → new acceptedAt` while preserving runtime state (`LastSeenAt`, `HasMention`, `ThreadUnread`, `Alert`) and identity (`_id`, `u`).

**Architecture:** Add `DisableNotification bool` to `model.Subscription`. Add a new `BulkUpsertSubscriptions` store method that issues a `BulkWrite` of `UpdateOneModel` upserts keyed on `(roomId, u.account)`, with `$set` carrying the three re-activation fields and `$setOnInsert` carrying identity + zero-value runtime defaults. Wire the two botDM call sites in `room-worker` (async `processCreateRoom` botDM branch and sync `processSyncCreateDM` botDM branch) to the new method. Mirror the sync path's post-write re-read into the async path so the in-memory subs handed to `finishCreateRoom` carry persisted `_id`/`JoinedAt`. Channel-room, regular-DM, and add-member paths are untouched.

**Tech Stack:** Go 1.25, MongoDB (`go.mongodb.org/mongo-driver/v2`), `go.uber.org/mock` (mockgen), `stretchr/testify`, `testcontainers-go` for integration tests. All commands wrapped via root `Makefile`.

**Reference spec:** `docs/superpowers/specs/2026-05-19-botdm-resubscribe-upsert-design.md`

---

## File Structure

**Modify:**
- `pkg/model/subscription.go` — add `DisableNotification` field
- `pkg/model/model_test.go` — extend Subscription round-trip case
- `room-worker/store.go` — declare `BulkUpsertSubscriptions` on interface
- `room-worker/store_mongo.go` — implement `BulkUpsertSubscriptions`
- `room-worker/handler.go` — switch botDM branches to upsert + add async re-read
- `room-worker/handler_test.go` — update botDM unit tests for new expectations
- `room-worker/integration_test.go` — add re-join refresh + regression tests
- `room-worker/mock_store_test.go` — regenerated via `make generate` (do not hand-edit)

**No new files.**

---

## Task 1: Add `DisableNotification` field to `model.Subscription`

**Files:**
- Modify: `pkg/model/subscription.go` (around line 41)
- Modify: `pkg/model/model_test.go` (the `TestSubscriptionJSON` "with optional fields set" case, around line 453)

- [ ] **Step 1: Write the failing test**

Edit `pkg/model/model_test.go`. In the `TestSubscriptionJSON` "with optional fields set" subtest, add `DisableNotification: true,` immediately after the existing `Alert: true,` line, so the round-trip covers the new field:

```go
s := model.Subscription{
    ID:                  "s1",
    User:                model.SubscriptionUser{ID: "u1", Account: "alice"},
    RoomID:              "r1",
    RoomType:            model.RoomTypeChannel,
    SiteID:              "site-a",
    Roles:               []model.Role{model.RoleOwner},
    HistorySharedSince:  &hss,
    JoinedAt:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
    LastSeenAt:          &lsa,
    HasMention:          true,
    ThreadUnread:        []string{"parent-1", "parent-2"},
    Alert:               true,
    DisableNotification: true,
}
```

Also extend `TestSubscriptionJSON_ThreadUnreadOmittedAlertAlwaysPresent` to assert `disableNotification` is always present in JSON even when false:

```go
disableVal, hasDisable := raw["disableNotification"]
assert.True(t, hasDisable, "disableNotification must be present in JSON even when false")
assert.Equal(t, false, disableVal)
```

(Add these two lines next to the existing `alertVal, hasAlert := raw["alert"]` block in the same test.)

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=pkg/model`
Expected: FAIL — `DisableNotification` is undefined on `model.Subscription`, and the round-trip would fail anyway because the field doesn't exist.

- [ ] **Step 3: Add the field**

Edit `pkg/model/subscription.go`. In the `Subscription` struct (around line 27), insert the field immediately after `Alert`:

```go
Alert               bool `json:"alert" bson:"alert"`
DisableNotification bool `json:"disableNotification" bson:"disableNotification"`
```

No `omitempty` — matches the convention of `Alert`/`HasMention`.

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/model`
Expected: PASS — both Subscription round-trip and the "alert/disableNotification always present" assertion succeed.

- [ ] **Step 5: Commit**

```bash
git add pkg/model/subscription.go pkg/model/model_test.go
git commit -m "feat(model): add DisableNotification field to Subscription"
```

---

## Task 2: Declare `BulkUpsertSubscriptions` on the store interface + regenerate mocks

**Files:**
- Modify: `room-worker/store.go` (around line 38, next to `BulkCreateSubscriptions`)
- Regenerated: `room-worker/mock_store_test.go`

- [ ] **Step 1: Add the interface method**

Edit `room-worker/store.go`. Immediately after the existing `BulkCreateSubscriptions` line (line 38) inside the `SubscriptionStore` interface, add:

```go
// BulkUpsertSubscriptions inserts each sub and, on a (roomId, u.account)
// collision with an existing document, refreshes the re-activation fields
// (DisableNotification → false, IsSubscribed, JoinedAt) while preserving
// the existing document's runtime state (LastSeenAt, HasMention,
// ThreadUnread, Alert) and identity (_id, u). Intended for botDM
// re-creation paths only; channel/DM/add-member paths must continue to
// use BulkCreateSubscriptions for safe redelivery idempotency.
BulkUpsertSubscriptions(ctx context.Context, subs []*model.Subscription) error
```

- [ ] **Step 2: Regenerate mocks**

Run: `make generate SERVICE=room-worker`
Expected: `room-worker/mock_store_test.go` updated to include `BulkUpsertSubscriptions` and matching `EXPECT()` helper. Do not hand-edit the file.

- [ ] **Step 3: Confirm Red state, do not commit**

Run: `make build SERVICE=room-worker`
Expected: FAIL with `*MongoStore does not implement SubscriptionStore (missing method BulkUpsertSubscriptions)`. That's the desired Red — Task 3 adds the implementation. Interface + mock + impl are one logical change and ship in Task 3's commit. Do not commit anything from this task in isolation.

---

## Task 3: Implement `BulkUpsertSubscriptions` on `MongoStore`

**Files:**
- Modify: `room-worker/store_mongo.go` (immediately after `BulkCreateSubscriptions`, around line 335)
- Modify: `room-worker/integration_test.go` (append a new test)

This task is integration-test-driven. The unit-test surface for `MongoStore` would be a Mongo mock and add no real signal; the real correctness gates are the Mongo upsert semantics, which only an integration test against a real container can verify.

- [ ] **Step 1: Write the failing integration test**

Append to `room-worker/integration_test.go`:

```go
func TestMongoStore_BulkUpsertSubscriptions_Integration(t *testing.T) {
    ctx := context.Background()
    db := setupMongo(t)
    store := NewMongoStore(db)

    oldJoinedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
    newJoinedAt := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
    lastSeen := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

    t.Run("update branch refreshes re-activation fields and preserves runtime state", func(t *testing.T) {
        mustInsertSub(t, db, &model.Subscription{
            ID:                  "existing-id-1",
            User:                model.SubscriptionUser{ID: "u_alice", Account: "alice"},
            RoomID:              "room-update-1",
            SiteID:              "site-A",
            RoomType:            model.RoomTypeBotDM,
            Name:                "helper.bot",
            IsSubscribed:        false,
            DisableNotification: true,
            JoinedAt:            oldJoinedAt,
            LastSeenAt:          &lastSeen,
            HasMention:          true,
            Alert:               true,
            ThreadUnread:        []string{"parent-1"},
        })

        // Caller-supplied sub uses a different _id; upsert MUST keep the existing _id.
        newSub := &model.Subscription{
            ID:           "caller-supplied-id-DIFFERENT",
            User:         model.SubscriptionUser{ID: "u_alice", Account: "alice"},
            RoomID:       "room-update-1",
            SiteID:       "site-A",
            RoomType:     model.RoomTypeBotDM,
            Name:         "helper.bot",
            IsSubscribed: true,
            JoinedAt:     newJoinedAt,
        }
        require.NoError(t, store.BulkUpsertSubscriptions(ctx, []*model.Subscription{newSub}))

        got, err := store.GetSubscription(ctx, "alice", "room-update-1")
        require.NoError(t, err)

        // Identity preserved.
        assert.Equal(t, "existing-id-1", got.ID, "_id must be preserved on update")

        // Re-activation fields refreshed.
        assert.False(t, got.DisableNotification, "DisableNotification must be cleared")
        assert.True(t, got.IsSubscribed, "IsSubscribed must be refreshed")
        assert.True(t, got.JoinedAt.Equal(newJoinedAt), "JoinedAt must be updated; got %v want %v", got.JoinedAt, newJoinedAt)

        // Runtime state preserved.
        assert.True(t, got.HasMention, "HasMention must be preserved")
        assert.True(t, got.Alert, "Alert must be preserved")
        require.NotNil(t, got.LastSeenAt)
        assert.True(t, got.LastSeenAt.Equal(lastSeen), "LastSeenAt must be preserved")
        assert.Equal(t, []string{"parent-1"}, got.ThreadUnread, "ThreadUnread must be preserved")
    })

    t.Run("insert branch initialises identity and zero-value runtime defaults", func(t *testing.T) {
        newSub := &model.Subscription{
            ID:           "fresh-insert-id",
            User:         model.SubscriptionUser{ID: "u_bob", Account: "bob"},
            RoomID:       "room-insert-1",
            SiteID:       "site-A",
            RoomType:     model.RoomTypeBotDM,
            Name:         "helper.bot",
            IsSubscribed: true,
            JoinedAt:     newJoinedAt,
        }
        require.NoError(t, store.BulkUpsertSubscriptions(ctx, []*model.Subscription{newSub}))

        got, err := store.GetSubscription(ctx, "bob", "room-insert-1")
        require.NoError(t, err)

        assert.Equal(t, "fresh-insert-id", got.ID)
        assert.Equal(t, "u_bob", got.User.ID)
        assert.Equal(t, "site-A", got.SiteID)
        assert.Equal(t, model.RoomTypeBotDM, got.RoomType)
        assert.Equal(t, "helper.bot", got.Name)
        assert.True(t, got.IsSubscribed)
        assert.False(t, got.DisableNotification)
        assert.True(t, got.JoinedAt.Equal(newJoinedAt))
        assert.False(t, got.HasMention)
        assert.False(t, got.Alert)
        assert.Nil(t, got.LastSeenAt)
        assert.Empty(t, got.ThreadUnread)
    })

    t.Run("empty slice is a no-op", func(t *testing.T) {
        require.NoError(t, store.BulkUpsertSubscriptions(ctx, nil))
        require.NoError(t, store.BulkUpsertSubscriptions(ctx, []*model.Subscription{}))
    })
}
```

- [ ] **Step 2: Run the integration test to verify it fails**

Run: `make test-integration SERVICE=room-worker`
Expected: COMPILE FAIL — `BulkUpsertSubscriptions` is not defined on `*MongoStore`.

- [ ] **Step 3: Implement `BulkUpsertSubscriptions`**

Edit `room-worker/store_mongo.go`. Immediately after `BulkCreateSubscriptions` (after line 335), add:

```go
// BulkUpsertSubscriptions upserts each sub keyed on (roomId, u.account).
// On collision with an existing document, $set refreshes the three
// re-activation fields (disableNotification → false, isSubscribed,
// joinedAt) and leaves runtime fields (lastSeenAt, hasMention,
// threadUnread, alert) untouched. On insert, $setOnInsert initialises
// identity (_id, u, roomId, siteId, roomType, name, roles) plus
// hasMention/alert zero values. Used exclusively by botDM creation
// paths — see store.go for the interface comment.
func (s *MongoStore) BulkUpsertSubscriptions(ctx context.Context, subs []*model.Subscription) error {
    if len(subs) == 0 {
        return nil
    }
    models := make([]mongo.WriteModel, 0, len(subs))
    for _, sub := range subs {
        filter := bson.M{"roomId": sub.RoomID, "u.account": sub.User.Account}
        update := bson.M{
            "$set": bson.M{
                "disableNotification": false,
                "isSubscribed":        sub.IsSubscribed,
                "joinedAt":            sub.JoinedAt,
            },
            "$setOnInsert": bson.M{
                "_id":        sub.ID,
                "u":          sub.User,
                "roomId":     sub.RoomID,
                "siteId":     sub.SiteID,
                "roomType":   sub.RoomType,
                "name":       sub.Name,
                "roles":      sub.Roles,
                "hasMention": false,
                "alert":      false,
            },
        }
        models = append(models, mongo.NewUpdateOneModel().
            SetFilter(filter).
            SetUpdate(update).
            SetUpsert(true))
    }
    opts := options.BulkWrite().SetOrdered(false)
    if _, err := s.subscriptions.BulkWrite(ctx, models, opts); err != nil {
        return fmt.Errorf("bulk upsert %d subscriptions: %w", len(subs), err)
    }
    return nil
}
```

- [ ] **Step 4: Run the integration test to verify it passes**

Run: `make test-integration SERVICE=room-worker`
Expected: PASS for `TestMongoStore_BulkUpsertSubscriptions_Integration` (all three subtests).

- [ ] **Step 5: Run lint**

Run: `make lint`
Expected: PASS — no formatting or vet errors.

- [ ] **Step 6: Commit**

```bash
git add room-worker/store.go room-worker/store_mongo.go room-worker/mock_store_test.go room-worker/integration_test.go
git commit -m "feat(room-worker): add BulkUpsertSubscriptions store method

Adds a botDM-only upsert path: on (roomId, u.account) collision, refreshes
DisableNotification → false, IsSubscribed, and JoinedAt while preserving
the existing document's _id and runtime state (LastSeenAt, HasMention,
ThreadUnread, Alert). Existing BulkCreateSubscriptions is unchanged —
channel/DM/add-member paths keep their safe insert-only contract."
```

---

## Task 4: Wire `processCreateRoom` botDM branch to upsert + re-read canonical subs

**Files:**
- Modify: `room-worker/handler.go` (the botDM branch in `processCreateRoom`, around line 1158-1166)
- Modify: `room-worker/handler_test.go` (`TestProcessCreateRoom_BotDM_HasIsSubscribed`, around line 2073)

- [ ] **Step 1: Update the unit test to assert the new call shape (Red)**

Edit `room-worker/handler_test.go`. Replace the body of `TestProcessCreateRoom_BotDM_HasIsSubscribed` (lines 2073-2114) with:

```go
func TestProcessCreateRoom_BotDM_HasIsSubscribed(t *testing.T) {
    h, mockStore, getPublished := newCreateRoomTestHandler(t)
    ctx := natsutil.WithRequestID(context.Background(), testRequestID)

    requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
    bot := &model.User{ID: "u_bot", Account: "helper.bot", SiteID: "site-A"}

    mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
    mockStore.EXPECT().GetUser(gomock.Any(), "helper.bot").Return(bot, nil)
    mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)

    var capturedSubs []*model.Subscription
    mockStore.EXPECT().BulkUpsertSubscriptions(gomock.Any(), gomock.Any()).
        DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
            capturedSubs = subs
            return nil
        })

    // After upsert, handler re-reads canonical sub pair via FindDMSubscription.
    // Return the same in-memory subs (no dup-key collision in this happy path).
    mockStore.EXPECT().FindDMSubscription(gomock.Any(), "alice", "helper.bot").
        DoAndReturn(func(_ context.Context, _, _ string) (*model.Subscription, error) {
            return capturedSubs[0], nil
        })
    mockStore.EXPECT().FindDMSubscription(gomock.Any(), "helper.bot", "alice").
        DoAndReturn(func(_ context.Context, _, _ string) (*model.Subscription, error) {
            return capturedSubs[1], nil
        })

    mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-bot-1").Return(nil)

    body := makeCreateRoomBody(t, &model.CreateRoomRequest{
        RoomID: "room-bot-1", RequesterAccount: "alice",
        Users:     []string{"helper.bot"},
        Timestamp: time.Now().UnixMilli(),
    })
    require.NoError(t, h.processCreateRoom(ctx, body))

    require.Len(t, capturedSubs, 2)

    // human side (alice): Name = bot's account, IsSubscribed = true
    humanSub := capturedSubs[0]
    assert.Equal(t, "u_alice", humanSub.User.ID)
    assert.Equal(t, bot.Account, humanSub.Name)
    assert.True(t, humanSub.IsSubscribed)

    // bot side: Name = requester's account, IsSubscribed = false
    botSub := capturedSubs[1]
    assert.Equal(t, "u_bot", botSub.User.ID)
    assert.Equal(t, requester.Account, botSub.Name)
    assert.False(t, botSub.IsSubscribed)

    assert.Empty(t, messagesCanonical(getPublished(), "site-A"), "botDM must emit no sys-messages")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test SERVICE=room-worker`
Expected: FAIL — handler still calls `BulkCreateSubscriptions`, but the mock now expects `BulkUpsertSubscriptions` + two `FindDMSubscription` calls. gomock reports unexpected `BulkCreateSubscriptions` call.

- [ ] **Step 3: Wire the handler**

Edit `room-worker/handler.go`. Replace the botDM/DM dispatch block (lines 1155-1171, the `switch roomType` case `model.RoomTypeDM, model.RoomTypeBotDM:`) with:

```go
switch roomType {
case model.RoomTypeDM, model.RoomTypeBotDM:
    var subs []*model.Subscription
    if roomType == model.RoomTypeBotDM {
        subs = buildBotDMSubs(requester, counterpart, room, acceptedAt)
        if err := h.store.BulkUpsertSubscriptions(ctx, subs); err != nil {
            return fmt.Errorf("bulk upsert subs: %w", err)
        }
        // Upsert may have hit an existing row whose _id/JoinedAt differ
        // from the in-memory pair. Re-read so finishCreateRoom's
        // subscription.update / MemberAddEvent fan-out carries persisted
        // values. Mirrors the sync DM path.
        requesterSub, err := h.store.FindDMSubscription(ctx, requester.Account, counterpart.Account)
        if err != nil {
            return fmt.Errorf("find requester sub after upsert: %w", err)
        }
        counterpartSub, err := h.store.FindDMSubscription(ctx, counterpart.Account, requester.Account)
        if err != nil {
            return fmt.Errorf("find counterpart sub after upsert: %w", err)
        }
        subs = []*model.Subscription{requesterSub, counterpartSub}
    } else {
        subs = buildDMSubs(requester, counterpart, room, acceptedAt)
        if err := h.store.BulkCreateSubscriptions(ctx, subs); err != nil {
            return fmt.Errorf("bulk create subs: %w", err)
        }
    }
    return h.finishCreateRoom(ctx, &req, room, requester, []model.User{*requester, *counterpart}, subs, requestID, now)
case model.RoomTypeChannel:
    return h.processCreateRoomChannel(ctx, &req, room, requester, requestID, acceptedAt, now)
default:
    return newPermanent("unknown room type %q", roomType)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `make test SERVICE=room-worker`
Expected: PASS — `TestProcessCreateRoom_BotDM_HasIsSubscribed` and all sibling tests pass. In particular `TestProcessCreateRoom_DM_BuildsTwoSubs` MUST still pass (regression guard that regular DMs stay on `BulkCreateSubscriptions`).

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "feat(room-worker): use BulkUpsertSubscriptions on async botDM path

processCreateRoom's botDM branch now upserts subscriptions so muted/
inactive botDM rooms are reactivated when the user re-opens them, and
re-reads the canonical sub pair via FindDMSubscription so downstream
events carry persisted _id/JoinedAt. Regular-DM branch is unchanged."
```

---

## Task 5: Wire `processSyncCreateDM` botDM branch to upsert

**Files:**
- Modify: `room-worker/handler.go` (the sync DM bulk-create at lines 1561-1570)
- Modify: `room-worker/handler_test.go` — `TestHandleSyncCreateDM_BotDM_RequesterSubIsSubscribedTrue` at line 2771 (botDM branch — change expectation). Do NOT touch `TestHandleSyncCreateDM_DM_PersistsSubsAndReturnsRequester` (line 2705), `TestHandleSyncCreateDM_ReturnsCanonicalPersistedSub` (line 2807), or `TestHandleSyncCreateDM_BulkCreateSubsTransientError` (line 2985) — all three exercise the regular-DM branch (`RoomType: model.RoomTypeDM`) and serve as the regression guard that the DM branch still uses `BulkCreateSubscriptions`.

- [ ] **Step 1: Update the botDM unit test to expect `BulkUpsertSubscriptions` (Red)**

Edit `room-worker/handler_test.go`. In `TestHandleSyncCreateDM_BotDM_RequesterSubIsSubscribedTrue`, change line 2783 from:

```go
store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
```

to:

```go
store.EXPECT().BulkUpsertSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
```

Leave the rest of the test (FindDMSubscription x2, CreateRoom, FindUsersByAccounts, assertions) unchanged.

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test SERVICE=room-worker`
Expected: FAIL — handler still calls `BulkCreateSubscriptions` in the sync botDM branch; mock expects `BulkUpsertSubscriptions`.

- [ ] **Step 3: Wire the handler**

Edit `room-worker/handler.go`. Replace the sync-DM bulk-create dispatch (lines 1561-1570) with:

```go
// validateSyncCreateDMShape already gated this to {dm, botDM}.
var subs []*model.Subscription
if req.RoomType == model.RoomTypeBotDM {
    subs = buildBotDMSubs(requester, other, room, acceptedAt)
    if err := h.store.BulkUpsertSubscriptions(ctx, subs); err != nil {
        return nil, fmt.Errorf("bulk upsert subs: %w", err)
    }
} else {
    subs = buildDMSubs(requester, other, room, acceptedAt)
    if err := h.store.BulkCreateSubscriptions(ctx, subs); err != nil {
        return nil, fmt.Errorf("bulk create subs: %w", err)
    }
}
```

The existing `FindDMSubscription` re-read at lines 1574-1582 stays — it now serves both branches correctly. The `publishSubscriptionUpdates` and outbox calls below it are unchanged.

- [ ] **Step 4: Run the test to verify it passes**

Run: `make test SERVICE=room-worker`
Expected: PASS — sync botDM test, sync regular-DM tests, and all other handler tests pass.

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "feat(room-worker): use BulkUpsertSubscriptions on sync botDM path

processSyncCreateDM's botDM branch now upserts subscriptions so muted/
inactive cross-site botDM rooms are reactivated on re-create. The
existing post-write FindDMSubscription re-read handles the canonical
sub fetch unchanged. Regular-DM branch is unchanged."
```

---

## Task 6: End-to-end integration test — botDM re-join refresh via `processCreateRoom`

**Files:**
- Modify: `room-worker/integration_test.go`

- [ ] **Step 1: Write the failing integration test**

Append to `room-worker/integration_test.go`:

```go
// TestProcessCreateRoom_BotDM_ReSubscribe_Integration verifies the
// end-to-end re-join refresh: pre-seed a muted/inactive botDM
// subscription, then run processCreateRoom for the same (room, user)
// pair and assert the canonical row's mute/active state was refreshed
// and runtime fields were preserved.
func TestProcessCreateRoom_BotDM_ReSubscribe_Integration(t *testing.T) {
    ctx := context.Background()
    db := setupMongo(t)
    store := NewMongoStore(db)

    mustInsertUser(t, db, &model.User{
        ID: "u_alice", Account: "alice", SiteID: "site-A",
        EngName: "Alice", ChineseName: "爱丽丝",
    })
    mustInsertUser(t, db, &model.User{
        ID: "u_helper_bot", Account: "helper.bot", SiteID: "site-A",
    })

    roomID := idgen.BuildDMRoomID("u_alice", "u_helper_bot")
    oldJoinedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
    lastSeen := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

    // Pre-seed: muted, inactive, with unread mention + runtime state.
    mustInsertRoom(t, db, &model.Room{
        ID: roomID, Type: model.RoomTypeBotDM, SiteID: "site-A",
        CreatedBy: "u_alice", CreatedAt: oldJoinedAt, UpdatedAt: oldJoinedAt,
        UIDs:     []string{"u_alice", "u_helper_bot"},
        Accounts: []string{"alice", "helper.bot"},
    })
    mustInsertSub(t, db, &model.Subscription{
        ID:                  "existing-human-sub",
        User:                model.SubscriptionUser{ID: "u_alice", Account: "alice"},
        RoomID:              roomID,
        SiteID:              "site-A",
        RoomType:            model.RoomTypeBotDM,
        Name:                "helper.bot",
        IsSubscribed:        false,
        DisableNotification: true,
        JoinedAt:            oldJoinedAt,
        LastSeenAt:          &lastSeen,
        HasMention:          true,
        Alert:               true,
    })
    mustInsertSub(t, db, &model.Subscription{
        ID:           "existing-bot-sub",
        User:         model.SubscriptionUser{ID: "u_helper_bot", Account: "helper.bot"},
        RoomID:       roomID,
        SiteID:       "site-A",
        RoomType:     model.RoomTypeBotDM,
        Name:         "alice",
        IsSubscribed: false,
        JoinedAt:     oldJoinedAt,
    })

    h := newIntegrationHandler(t, store, "site-A")
    const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
    ctx = natsutil.WithRequestID(ctx, reqID)

    body, err := json.Marshal(model.CreateRoomRequest{
        RoomID:           roomID,
        Users:            []string{"helper.bot"},
        RequesterID:      "u_alice",
        RequesterAccount: "alice",
        Timestamp:        time.Now().UTC().UnixMilli(),
    })
    require.NoError(t, err)
    require.NoError(t, h.processCreateRoom(ctx, body))

    // Human sub: identity preserved, re-activation refreshed, runtime preserved.
    human, err := store.GetSubscription(ctx, "alice", roomID)
    require.NoError(t, err)
    assert.Equal(t, "existing-human-sub", human.ID, "_id preserved")
    assert.False(t, human.DisableNotification, "DisableNotification cleared")
    assert.True(t, human.IsSubscribed, "IsSubscribed refreshed")
    assert.False(t, human.JoinedAt.Equal(oldJoinedAt), "JoinedAt updated to acceptedAt")
    assert.True(t, human.HasMention, "HasMention preserved")
    assert.True(t, human.Alert, "Alert preserved")
    require.NotNil(t, human.LastSeenAt)
    assert.True(t, human.LastSeenAt.Equal(lastSeen), "LastSeenAt preserved")

    // Bot sub: identity preserved, IsSubscribed stays false (bot-side semantics).
    botSub, err := store.GetSubscription(ctx, "helper.bot", roomID)
    require.NoError(t, err)
    assert.Equal(t, "existing-bot-sub", botSub.ID, "_id preserved")
    assert.False(t, botSub.IsSubscribed, "bot side stays IsSubscribed=false")
    assert.False(t, botSub.DisableNotification, "bot side DisableNotification cleared (idempotent no-op)")

    // Only 2 subs total — no duplicates created.
    subCount, err := db.Collection("subscriptions").CountDocuments(ctx, bson.M{"roomId": roomID})
    require.NoError(t, err)
    assert.Equal(t, int64(2), subCount, "no duplicate subs after re-create")
}
```

- [ ] **Step 2: Run the integration test to verify it passes**

Run: `make test-integration SERVICE=room-worker`
Expected: PASS for `TestProcessCreateRoom_BotDM_ReSubscribe_Integration`.

(This task does not have a separate Red step because the handler change in Task 4 already exists. The test exists to verify end-to-end correctness against real Mongo and lock in the behavior as a regression guard. If it fails, the bug is in Task 4's wiring — return to that task.)

- [ ] **Step 3: Commit**

```bash
git add room-worker/integration_test.go
git commit -m "test(room-worker): integration test for botDM re-subscribe refresh

End-to-end test that processCreateRoom on a muted/inactive botDM
refreshes DisableNotification/IsSubscribed/JoinedAt while preserving
the existing _id and runtime fields (HasMention, Alert, LastSeenAt)."
```

---

## Task 7: Regression integration test — regular DM does NOT upsert

**Files:**
- Modify: `room-worker/integration_test.go`

- [ ] **Step 1: Write the regression test**

Append to `room-worker/integration_test.go`:

```go
// TestProcessCreateRoom_DM_DoesNotUpsert_Integration locks in that
// processCreateRoom's regular-DM branch keeps its insert-only contract:
// a pre-existing regular-DM subscription's state (specifically
// DisableNotification = true and an old JoinedAt) must NOT be refreshed
// when processCreateRoom is replayed for the same (room, user) pair.
// This regression guard prevents accidental upsert wiring on the DM
// branch in future edits.
func TestProcessCreateRoom_DM_DoesNotUpsert_Integration(t *testing.T) {
    ctx := context.Background()
    db := setupMongo(t)
    store := NewMongoStore(db)

    mustInsertUser(t, db, &model.User{
        ID: "u_alice", Account: "alice", SiteID: "site-A",
        EngName: "Alice", ChineseName: "爱丽丝",
    })
    mustInsertUser(t, db, &model.User{
        ID: "u_bob", Account: "bob", SiteID: "site-A",
        EngName: "Bob", ChineseName: "鲍勃",
    })

    roomID := idgen.BuildDMRoomID("u_alice", "u_bob")
    oldJoinedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

    mustInsertRoom(t, db, &model.Room{
        ID: roomID, Type: model.RoomTypeDM, SiteID: "site-A",
        CreatedBy: "u_alice", CreatedAt: oldJoinedAt, UpdatedAt: oldJoinedAt,
        UIDs:     []string{"u_alice", "u_bob"},
        Accounts: []string{"alice", "bob"},
    })
    mustInsertSub(t, db, &model.Subscription{
        ID:                  "existing-alice-sub",
        User:                model.SubscriptionUser{ID: "u_alice", Account: "alice"},
        RoomID:              roomID,
        SiteID:              "site-A",
        RoomType:            model.RoomTypeDM,
        Name:                "bob",
        DisableNotification: true,
        JoinedAt:            oldJoinedAt,
    })
    mustInsertSub(t, db, &model.Subscription{
        ID:       "existing-bob-sub",
        User:     model.SubscriptionUser{ID: "u_bob", Account: "bob"},
        RoomID:   roomID,
        SiteID:   "site-A",
        RoomType: model.RoomTypeDM,
        Name:     "alice",
        JoinedAt: oldJoinedAt,
    })

    h := newIntegrationHandler(t, store, "site-A")
    const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
    ctx = natsutil.WithRequestID(ctx, reqID)

    body, err := json.Marshal(model.CreateRoomRequest{
        RoomID:           roomID,
        Users:            []string{"bob"},
        RequesterID:      "u_alice",
        RequesterAccount: "alice",
        Timestamp:        time.Now().UTC().UnixMilli(),
    })
    require.NoError(t, err)
    require.NoError(t, h.processCreateRoom(ctx, body))

    got, err := store.GetSubscription(ctx, "alice", roomID)
    require.NoError(t, err)
    assert.True(t, got.DisableNotification,
        "regular-DM path must NOT clear DisableNotification on re-create (insert-only contract)")
    assert.True(t, got.JoinedAt.Equal(oldJoinedAt),
        "regular-DM path must NOT refresh JoinedAt on re-create (insert-only contract)")
}
```

- [ ] **Step 2: Run the integration test to verify it passes**

Run: `make test-integration SERVICE=room-worker`
Expected: PASS — confirms the regular-DM branch keeps the pre-existing sub untouched, including the muted state and old `JoinedAt`.

- [ ] **Step 3: Commit**

```bash
git add room-worker/integration_test.go
git commit -m "test(room-worker): regression test for regular-DM insert-only contract

Locks in that processCreateRoom's regular-DM branch must NOT refresh a
pre-existing subscription's DisableNotification or JoinedAt — only the
botDM branch upserts. Guards against accidental upsert wiring spreading
to the regular-DM path in future edits."
```

---

## Task 8: Verification gate — full test + lint + SAST

**Files:** None.

- [ ] **Step 1: Regenerate all mocks (defensive)**

Run: `make generate`
Expected: no diff if Task 2 was committed correctly.

- [ ] **Step 2: Run full unit-test suite with race detector**

Run: `make test`
Expected: PASS across all services.

- [ ] **Step 3: Run integration tests for the touched service**

Run: `make test-integration SERVICE=room-worker`
Expected: PASS for all room-worker integration tests, including the two new ones.

- [ ] **Step 4: Run lint**

Run: `make lint`
Expected: PASS.

- [ ] **Step 5: Run SAST**

Run: `make sast`
Expected: PASS (no medium+ findings introduced by this change).

- [ ] **Step 6: Verify branch coverage on touched files**

Run:
```bash
go test -tags=integration -race -coverprofile=/tmp/cov.out ./room-worker/... \
  && go tool cover -func=/tmp/cov.out | grep -E "(BulkUpsertSubscriptions|processCreateRoom|processSyncCreateDM)"
```
Expected: coverage on `BulkUpsertSubscriptions` ≥90%; both modified handler branches exercised. If coverage on the new method is below 90%, add a missing test rather than padding numbers.

- [ ] **Step 7: Push**

```bash
git push -u origin claude/fix-room-notification-settings-RqyO1
```

---

## Spec Coverage Map

| Spec section | Implementing task(s) |
|---|---|
| §3.1 `DisableNotification` field on `model.Subscription` | Task 1 |
| §3.2 `newSub` signature unchanged (zero-value default) | Task 4 (implicit — no change made to `newSub`) |
| §3.3 `BulkUpsertSubscriptions` interface + mock | Task 2 |
| §3.3 `BulkUpsertSubscriptions` Mongo impl (`$set` / `$setOnInsert`) | Task 3 |
| §3.4 Async `processCreateRoom` botDM branch switch + re-read | Task 4 |
| §3.4 Sync `processSyncCreateDM` botDM branch switch | Task 5 |
| §3.5 Sync re-read (already present) — async re-read mirrors it | Task 4 |
| Testing §1 Model round-trip | Task 1 |
| Testing §2 Handler unit tests for botDM branches | Tasks 4, 5 |
| Testing §3 Integration test — re-join refresh | Task 6 |
| Testing §3 Integration test — fresh insert | Task 3 (subtest within store integration test) |
| Testing §3 Integration test — regular-DM regression | Task 7 |
| Testing §4 Coverage gate ≥90% | Task 8 |
| Out-of-scope: notification-worker filter, mute toggle endpoint, frontend UI | (deferred — explicitly out of scope per spec) |
| §3a `BulkCreateSubscriptions` → `$setOnInsert` upsert | Addendum Task A1 |
| §3b `FindDMSubscriptionPair` single-query re-read | Addendum Task A2 |

---

## Addendum — Post-Review Changes (commit `d4bb50c`)

Three review comments from `mliu33` on PR #202 triggered follow-up changes
after the original plan landed. Spec sections §3a and §3b cover the design;
the tasks below cover the implementation.

### Addendum Task A1: Convert `BulkCreateSubscriptions` to `$setOnInsert` upsert

**Files:**
- Modify: `room-worker/store_mongo.go` — `BulkCreateSubscriptions` impl
- Modify: `room-worker/handler_test.go` — drop the "non-dup-key" wording in
  the `BulkCreateSubsTransientError` comment; the test itself is unchanged
  (a transient error still surfaces as "internal error")

- [x] Replace the `InsertMany + SetOrdered(false) + IsDuplicateKeyError`
  swallow with a `BulkWrite` of `mongoutil.UpsertModel(filter, $setOnInsert)`
  models, one per sub, filter keyed on `(roomId, u.account)`. Use the same
  `options.BulkWrite().SetOrdered(false)` so partial collisions don't halt
  the batch.
- [x] No interface change — same signature, same semantics observed by
  callers. `BulkUpsertSubscriptions` (botDM refresh) stays untouched.
- [x] Confirm `TestProcessCreateRoom_DM_DoesNotUpsert_Integration` still
  passes — `$setOnInsert` is a no-op on collision, so the pre-seeded
  `DisableNotification = true` and old `JoinedAt` remain.

### Addendum Task A2: `FindDMSubscriptionPair` single-query re-read

**Files:**
- Modify: `room-worker/store.go` — declare on `SubscriptionStore` interface
- Modify: `room-worker/store_mongo.go` — Mongo impl
- Modify: `room-worker/handler.go` — `findDMSubscriptionPair` helper now
  wraps the new store method; both call sites pass `room.ID` +
  `requester.Account`
- Modify: `room-worker/handler_test.go` — every test that previously mocked
  two `FindDMSubscription` calls now mocks one `FindDMSubscriptionPair` call
  (returning both subs)
- Regenerate: `room-worker/mock_store_test.go` via `make generate`

- [x] Add interface method:
  ```go
  FindDMSubscriptionPair(ctx context.Context, roomID, requesterAccount string) (*model.Subscription, *model.Subscription, error)
  ```
- [x] Mongo impl: one `Find` on
  `{"roomId": roomID, "roomType": {"$in": [dm, botDM]}}`, decode into
  `[]model.Subscription`, partition by `u.account`. Return
  `model.ErrSubscriptionNotFound` if fewer than two results or if
  `requesterAccount` isn't among them.
- [x] Update handler helper signature from
  `findDMSubscriptionPair(ctx, requester, counterpart *model.User)` to
  `findDMSubscriptionPair(ctx, roomID, requesterAccount string)`.
- [x] Update both call sites (async `processCreateRoom` botDM branch, sync
  `handleSyncCreateDM` post-bulk re-read).
- [x] `make generate` → `make test SERVICE=room-worker` → `make lint`.

### Addendum verification

- [x] `make lint` — 0 issues
- [x] `make test SERVICE=room-worker` — pass
- [x] Commit + push: `d4bb50c refactor(room-worker): single-query DM sub
  pair + idempotent-upsert BulkCreate`

---

## Addendum 2 — Post-Review (user-service Owns Re-subscribe)

A second review round pivoted the design: **user-service** will own the
re-subscribe semantic via `SetUserAppSubscribe` (clears
`DisableNotification`, sets `IsSubscribed = true`) and only calls
`createRoomSync` when no subscription exists. That collapses room-worker's
re-subscribe code path: both DM and botDM share the same idempotent
insert, and the `BulkUpsertSubscriptions` refresh path is no longer
needed.

### Addendum Task B1: Remove `BulkUpsertSubscriptions`

**Files:**
- Modify: `room-worker/store.go` — delete interface method + doc
- Modify: `room-worker/store_mongo.go` — delete Mongo impl
- Regenerate: `room-worker/mock_store_test.go`
- Modify: `room-worker/handler.go` — both call sites switch to
  `BulkCreateSubscriptions`
- Modify: `room-worker/handler_test.go` — all
  `EXPECT().BulkUpsertSubscriptions` lines become `BulkCreateSubscriptions`
- Modify: `room-worker/integration_test.go` — delete
  `TestMongoStore_BulkUpsertSubscriptions_Integration`; invert
  `TestProcessCreateRoom_BotDM_ReSubscribe_Integration` into
  `TestProcessCreateRoom_BotDM_DoesNotUpsert_Integration`

- [x] Drop the if/else dispatch in `processCreateRoom` DM branch — both
  room types call `BulkCreateSubscriptions` followed by the canonical
  re-read via `FindDMSubscriptionPair`.
- [x] Same simplification in `handleSyncCreateDM`.
- [x] Remove the botDM-specific `joinedAt` carve-out on the dup-room path:
  `joinedAt = existing.CreatedAt` now applies to both DM and botDM (the
  previous `if req.RoomType == model.RoomTypeDM` gate is gone).
- [x] Invert the botDM integration test — it now asserts that a
  pre-seeded muted/inactive sub is **preserved** on redelivery (mirrors
  the existing DM regression guard).

### Addendum Task B2: Remove `FindDMSubscription`

**Files:**
- Modify: `room-worker/store.go` — delete interface method + doc
- Modify: `room-worker/store_mongo.go` — delete Mongo impl
- Regenerate: `room-worker/mock_store_test.go`

- [x] No call sites remain in `room-worker` after Addendum Task A2
  (`FindDMSubscriptionPair` covers the only consumer). Delete the
  interface method and the Mongo impl.

### Addendum Task B3: Tighten `FindDMSubscriptionPair`

**Files:**
- Modify: `room-worker/store_mongo.go`
- Modify: `room-worker/handler.go` — inline the helper (pure pass-through
  after we drop the redundant wrap; callers add their own context)

- [x] Count check: `len(subs) < 2` → `len(subs) != 2` — fail loudly if
  Mongo ever returns more than two subs for a DM/botDM room.
- [x] Stop double-wrapping the driver error. The store returns the raw
  error from `Find` / `cursor.All`; callers (handler) already add
  operational context (`"re-read DM subs after write: %w"`). Single
  wrap, not double.
- [x] Inline `findDMSubscriptionPair` helper — it was a pure pass-through
  after the wrap was removed.

### Addendum 2 verification

- [x] `make generate SERVICE=room-worker`
- [x] `make lint` — 0 issues
- [x] `make test SERVICE=room-worker` — pass

### Spec coverage update

| Spec section | Implementing task(s) |
|---|---|
| Post-Review Revision: `BulkUpsertSubscriptions` removed | Addendum Task B1 |
| Post-Review Revision: `FindDMSubscription` removed | Addendum Task B2 |
| Post-Review Revision: shared `joinedAt = existing.CreatedAt` for DM + botDM | Addendum Task B1 |
| Post-Review Revision: `FindDMSubscriptionPair` != 2 check + single error wrap | Addendum Task B3 |
