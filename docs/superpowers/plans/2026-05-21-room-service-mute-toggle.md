# Room-Service `mute.toggle` RPC — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a per-user `mute.toggle` RPC to `room-service` that flips `Subscription.DisableNotifications` for the requester in a single room, with cross-site federation so the user's home-site mirror stays consistent. Field is renamed from `DisableNotification` (singular) to `DisableNotifications` (plural) as part of the same plan.

**Architecture:** Follow the **`message.read` pattern** (Pattern B) — `room-service` performs the Mongo write inline (atomic aggregation-pipeline `FindOneAndUpdate` so the toggle is single-round-trip and concurrency-safe), publishes a `SubscriptionUpdateEvent` to `chat.user.{account}.event.subscription.update` for client UI fan-out, and (when `users.siteId != h.siteID`) publishes a new `subscription_mute_toggled` outbox event so `inbox-worker` mirrors the change on the user's home site. No canonical event, no `room-worker` involvement, no key rotation.

**Tech Stack:** Go 1.25 · NATS (core + JetStream) · MongoDB driver v2 · `go.uber.org/mock` · `stretchr/testify`.

---

## Cross-site stale-data assessment

The codebase replicates subscriptions per-site: the **room's home site** holds a sub doc for every member (used by `notification-worker` to fan out new-message notifications); the **user's home site** holds a parallel copy (used by `user-service` to render the sidebar / list-rooms view). Outbox→Inbox keeps the user-site copy in sync with writes that happen on the room's site.

For `mute.toggle`:
- **Notification correctness is safe even without cross-site sync.** The mute RPC lands on the room's home site (the subject's `{siteID}` is the room's site) and `room-service` writes on the room's site. `notification-worker` consumes `MESSAGES_CANONICAL_{siteID}` on the room's site and reads subscriptions from the room's site Mongo. So the mute is honored for notifications the moment the local write commits.
- **User's home-site copy needs cross-site sync.** When the client later refetches subscriptions via `user-service` (which reads from the user's home-site Mongo), it would see the stale value and the UI toggle would visibly snap back. The outbox event closes this gap.
- **Notification dispatch in `notification-worker` does not yet check `DisableNotifications`.** The current `handler.go` blindly publishes to every subscriber except the sender. Wiring the mute into the notification check is **out of scope for this plan** — call it out in the docs update so the next PR owns that follow-up.

No prod or staging environment exists yet, so the field rename does not need a Mongo data migration — local dev databases get recreated on every `docker-compose up`. If/when an environment with persisted data appears, ops can run a one-shot `updateMany` to copy `disableNotification` → `disableNotifications`; the plan itself contains no migration step.

---

## File map

| File | What it does | Action |
|------|--------------|--------|
| `pkg/model/subscription.go` | `Subscription.DisableNotification` field | Rename → `DisableNotifications` |
| `pkg/model/model_test.go` | Model round-trip tests | Rename field references + JSON key |
| `room-worker/integration_test.go` | botDM resubscribe tests | Rename field references |
| `pkg/model/event.go` | New outbox event type + payload struct | Add `OutboxSubscriptionMuteToggled` + `SubscriptionMuteToggledEvent` |
| `pkg/model/model_test.go` | Round-trip for new payload | Add test |
| `pkg/subject/subject.go` | Subject builders | Add `MuteToggle` + `MuteToggleWildcard` |
| `pkg/subject/subject_test.go` | Subject tests | Add cases |
| `room-service/store.go` | Store interface | Add `ToggleSubscriptionMute` method |
| `room-service/store_mongo.go` | Mongo impl | Implement `ToggleSubscriptionMute` |
| `room-service/integration_test.go` | Mongo integration | Add test for new method |
| `room-service/mock_store_test.go` | Generated mocks | Regenerate |
| `room-service/main.go` | Wire up handler | Add `publishCore` closure parameter |
| `room-service/handler.go` | New RPC handler | Add `natsMuteToggle` + `handleMuteToggle`; register subject |
| `room-service/handler_test.go` | Handler unit tests | Add table-driven tests |
| `inbox-worker/handler.go` | Cross-site mirror | Add `subscription_mute_toggled` case + handler |
| `inbox-worker/handler_test.go` | Handler unit tests | Add case |
| `inbox-worker/store.go` (or wherever `InboxStore` is defined) | Add `UpdateSubscriptionMute` | Add method to interface + Mongo impl |
| `docs/client-api.md` | Public API docs | Document new RPC + triggered events |

Historical specs/plans under `docs/superpowers/{specs,plans}/2026-05-19-botdm-*` describe the old `DisableNotification` name — **do not retroactively rewrite them**; they are historical records.

---

## Task 0: Rename `DisableNotification` → `DisableNotifications`

**Files:**
- Modify: `pkg/model/subscription.go:42`
- Modify: `pkg/model/model_test.go:470,522,523`
- Modify: `room-worker/integration_test.go:1288,1320,1350,1351,1365,1399,1428,1429`

- [ ] **Step 1: Update model test expectations first (TDD: red against old name)**

Edit `pkg/model/model_test.go`. Replace the three occurrences:

Line ~470 (in `TestSubscriptionJSON` "with optional fields set"):
```go
DisableNotifications: true,
```

Lines ~522-523:
```go
disableVal, hasDisable := raw["disableNotifications"]
assert.True(t, hasDisable, "disableNotifications must be present in JSON even when false")
```

- [ ] **Step 2: Run model tests — expect FAIL**

Run: `make test SERVICE=pkg/model`

Expected: FAIL — the struct still defines `DisableNotification`, the test now references `DisableNotifications`.

- [ ] **Step 3: Rename the field on `model.Subscription`**

Edit `pkg/model/subscription.go:42`:

```go
DisableNotifications bool             `json:"disableNotifications" bson:"disableNotifications"`
```

- [ ] **Step 4: Rename in room-worker integration test**

Edit `room-worker/integration_test.go`. Use `replace_all` on the exact identifier `DisableNotification` → `DisableNotifications`. The comments at lines 1288 and 1365 mention the field name — update those too.

- [ ] **Step 5: Run all tests — expect PASS**

Run: `make test`

Expected: PASS — no remaining references to the old field name in compiled code.

- [ ] **Step 6: Search for any stragglers**

Run: `grep -rn "DisableNotification\b\|disableNotification\b" --include="*.go" .`

Expected: zero hits (the `\b` boundary makes sure `DisableNotifications` doesn't match). Anything that comes up: fix it.

- [ ] **Step 7: Commit**

```bash
git add pkg/model/subscription.go pkg/model/model_test.go room-worker/integration_test.go
git commit -m "refactor(model): rename Subscription.DisableNotification → DisableNotifications"
```

---

## Task 1: Add `MuteToggle` subject builders

**Files:**
- Modify: `pkg/subject/subject.go`
- Modify: `pkg/subject/subject_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `pkg/subject/subject_test.go` (after the existing `TestMessageRead*` block around line 320):

```go
func TestMuteToggle(t *testing.T) {
	got := subject.MuteToggle("alice", "r1", "site-a")
	want := "chat.user.alice.request.room.r1.site-a.mute.toggle"
	if got != want {
		t.Errorf("MuteToggle: got %q, want %q", got, want)
	}
}

func TestMuteToggleWildcard(t *testing.T) {
	got := subject.MuteToggleWildcard("site-a")
	want := "chat.user.*.request.room.*.site-a.mute.toggle"
	if got != want {
		t.Errorf("MuteToggleWildcard: got %q, want %q", got, want)
	}
}

func TestMuteToggle_ParseUserRoomSubject(t *testing.T) {
	subj := subject.MuteToggle("alice", "r1", "site-a")
	account, roomID, ok := subject.ParseUserRoomSubject(subj)
	if !ok || account != "alice" || roomID != "r1" {
		t.Errorf("ParseUserRoomSubject(%q) = (%q,%q,%v), want (alice,r1,true)", subj, account, roomID, ok)
	}
}
```

- [ ] **Step 2: Run — expect FAIL**

Run: `make test SERVICE=pkg/subject`

Expected: FAIL with "undefined: subject.MuteToggle" / "MuteToggleWildcard".

- [ ] **Step 3: Implement the builders**

Append to `pkg/subject/subject.go` after `MessageReadReceiptWildcard` (around line 362):

```go
// MuteToggle returns the concrete subject for the per-user mute.toggle RPC.
// Pair with MuteToggleWildcard for room-service's QueueSubscribe.
func MuteToggle(account, roomID, siteID string) string {
	return fmt.Sprintf("chat.user.%s.request.room.%s.%s.mute.toggle", account, roomID, siteID)
}

// MuteToggleWildcard is the per-site subscription pattern for the mute.toggle RPC.
func MuteToggleWildcard(siteID string) string {
	return fmt.Sprintf("chat.user.*.request.room.*.%s.mute.toggle", siteID)
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `make test SERVICE=pkg/subject`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "feat(subject): add MuteToggle subject builders"
```

---

## Task 2: Add `MuteToggleResponse` and outbox event types

**Files:**
- Modify: `pkg/model/event.go`
- Modify: `pkg/model/model_test.go`

- [ ] **Step 1: Write a failing round-trip test for the new types**

Append to `pkg/model/model_test.go` (near the end of the file, after the existing event round-trip tests):

```go
func TestMuteToggleResponseJSON(t *testing.T) {
	src := model.MuteToggleResponse{
		Status:               "ok",
		DisableNotifications: true,
	}
	data, err := json.Marshal(src)
	require.NoError(t, err)

	var dst model.MuteToggleResponse
	require.NoError(t, json.Unmarshal(data, &dst))
	assert.Equal(t, src, dst)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Equal(t, "ok", raw["status"])
	assert.Equal(t, true, raw["disableNotifications"])
}

func TestSubscriptionMuteToggledEventJSON(t *testing.T) {
	src := model.SubscriptionMuteToggledEvent{
		Account:              "alice",
		RoomID:               "r1",
		DisableNotifications: true,
		Timestamp:            1234567890,
	}
	data, err := json.Marshal(src)
	require.NoError(t, err)

	var dst model.SubscriptionMuteToggledEvent
	require.NoError(t, json.Unmarshal(data, &dst))
	assert.Equal(t, src, dst)
}

func TestOutboxSubscriptionMuteToggledConst(t *testing.T) {
	assert.Equal(t, model.OutboxEventType("subscription_mute_toggled"), model.OutboxSubscriptionMuteToggled)
}
```

- [ ] **Step 2: Run — expect FAIL**

Run: `make test SERVICE=pkg/model`

Expected: FAIL with "undefined: model.MuteToggleResponse" and "undefined: model.SubscriptionMuteToggledEvent" / "OutboxSubscriptionMuteToggled".

- [ ] **Step 3: Add the types**

Append to `pkg/model/event.go` after `RoomKeyEnsureResponse` (around line 211):

```go
// MuteToggleResponse is the sync reply for the mute.toggle RPC. DisableNotifications
// carries the resulting value of the toggle (post-flip).
type MuteToggleResponse struct {
	Status               string `json:"status"`
	DisableNotifications bool   `json:"disableNotifications"`
}

// SubscriptionMuteToggledEvent is the OutboxEvent.Payload for type
// "subscription_mute_toggled". Sent from a room's home site to the user's home
// site whenever a user flips DisableNotifications via the mute.toggle RPC; the
// destination updates its local subscription mirror.
type SubscriptionMuteToggledEvent struct {
	Account              string `json:"account"              bson:"account"`
	RoomID               string `json:"roomId"               bson:"roomId"`
	DisableNotifications bool   `json:"disableNotifications" bson:"disableNotifications"`
	Timestamp            int64  `json:"timestamp"            bson:"timestamp"`
}
```

Add the new outbox event-type const inside the existing `const ( … OutboxSubscriptionRead … )` block (line ~81):

```go
const (
	OutboxMemberAdded                OutboxEventType = "member_added"
	OutboxMemberRemoved              OutboxEventType = "member_removed"
	OutboxSubscriptionRead           OutboxEventType = "subscription_read"
	OutboxSubscriptionMuteToggled    OutboxEventType = "subscription_mute_toggled"
	OutboxThreadSubscriptionUpserted OutboxEventType = "thread_subscription_upserted"
)
```

- [ ] **Step 4: Run — expect PASS**

Run: `make test SERVICE=pkg/model`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/model/event.go pkg/model/model_test.go
git commit -m "feat(model): add MuteToggleResponse + SubscriptionMuteToggledEvent"
```

---

## Task 3: Add `ToggleSubscriptionMute` to `RoomStore` interface

**Files:**
- Modify: `room-service/store.go`

- [ ] **Step 1: Add the method to the interface**

Edit `room-service/store.go`. Add to the `RoomStore` interface (after `UpdateSubscriptionRead`, around line 75):

```go
// ToggleSubscriptionMute atomically flips disableNotifications on the
// subscription keyed by (roomID, account) using a single aggregation-pipeline
// FindOneAndUpdate so concurrent toggles cannot deadlock or read-modify-write
// race. Returns the resulting value of disableNotifications post-flip.
// Returns model.ErrSubscriptionNotFound (wrapped) when no subscription matches.
ToggleSubscriptionMute(ctx context.Context, roomID, account string) (bool, error)
```

- [ ] **Step 2: Verify it compiles (no test yet — implementation comes next)**

Run: `go build ./room-service/...`

Expected: PASS — the interface change compiles even before the impl exists, because nothing yet binds a struct against the new method. (`MongoStore` is a concrete type and only validated against the interface at handler-construction time, which is still ok because we'll add the impl in the next task.) If the build does break here, that's fine — the next task adds the method.

- [ ] **Step 3: Commit**

```bash
git add room-service/store.go
git commit -m "feat(room-service): declare ToggleSubscriptionMute in RoomStore"
```

---

## Task 4: Implement `ToggleSubscriptionMute` on `MongoStore`

**Files:**
- Modify: `room-service/store_mongo.go`
- Modify: `room-service/integration_test.go`

- [ ] **Step 1: Write a failing integration test**

Open `room-service/integration_test.go` and append the following test. Use the existing `testutil.MongoDB(t, ...)` helper pattern that other tests in the file already use; mirror their `package main` + `//go:build integration` tag.

```go
func TestMongoStore_ToggleSubscriptionMute(t *testing.T) {
	db := testutil.MongoDB(t, "room-svc-mute")
	store := NewMongoStore(db)
	ctx := context.Background()

	sub := &model.Subscription{
		ID:                   idgen.GenerateUUIDv7(),
		User:                 model.SubscriptionUser{ID: "u1", Account: "alice"},
		RoomID:               "r1",
		RoomType:             model.RoomTypeChannel,
		SiteID:               "site-a",
		Roles:                []model.Role{model.RoleMember},
		JoinedAt:             time.Now().UTC(),
		DisableNotifications: false,
	}
	require.NoError(t, store.CreateSubscription(ctx, sub))

	// First toggle: false → true.
	got, err := store.ToggleSubscriptionMute(ctx, "r1", "alice")
	require.NoError(t, err)
	assert.True(t, got)

	// Verify persisted.
	persisted, err := store.GetSubscription(ctx, "alice", "r1")
	require.NoError(t, err)
	assert.True(t, persisted.DisableNotifications)

	// Second toggle: true → false.
	got, err = store.ToggleSubscriptionMute(ctx, "r1", "alice")
	require.NoError(t, err)
	assert.False(t, got)

	// Not-found case.
	_, err = store.ToggleSubscriptionMute(ctx, "missing", "alice")
	assert.ErrorIs(t, err, model.ErrSubscriptionNotFound)
}
```

If `idgen`, `time`, or `model.ErrSubscriptionNotFound` aren't already imported in this file, add them — check the existing import block.

- [ ] **Step 2: Run the integration test — expect FAIL**

Run: `make test-integration SERVICE=room-service`

Expected: FAIL — `store.ToggleSubscriptionMute` undefined.

- [ ] **Step 3: Implement on `MongoStore`**

Append to `room-service/store_mongo.go` after `UpdateSubscriptionRead` (around line 679):

```go
// ToggleSubscriptionMute atomically flips disableNotifications via an
// aggregation-pipeline FindOneAndUpdate. The $ifNull guard treats a missing
// field as false so legacy documents toggle to true on the first call.
// Returns the post-flip value, or model.ErrSubscriptionNotFound when no
// subscription matches.
func (s *MongoStore) ToggleSubscriptionMute(ctx context.Context, roomID, account string) (bool, error) {
	filter := bson.M{"roomId": roomID, "u.account": account}
	update := mongo.Pipeline{
		bson.D{{Key: "$set", Value: bson.M{
			"disableNotifications": bson.M{"$not": bson.M{
				"$ifNull": bson.A{"$disableNotifications", false},
			}},
		}}},
	}
	opts := options.FindOneAndUpdate().
		SetReturnDocument(options.After).
		SetProjection(bson.M{"_id": 0, "disableNotifications": 1})

	var result struct {
		DisableNotifications bool `bson:"disableNotifications"`
	}
	err := s.subscriptions.FindOneAndUpdate(ctx, filter, update, opts).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, fmt.Errorf("toggle mute for %q in room %q: %w", account, roomID, model.ErrSubscriptionNotFound)
		}
		return false, fmt.Errorf("toggle mute for %q in room %q: %w", account, roomID, err)
	}
	return result.DisableNotifications, nil
}
```

- [ ] **Step 4: Run the integration test — expect PASS**

Run: `make test-integration SERVICE=room-service`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add room-service/store_mongo.go room-service/integration_test.go
git commit -m "feat(room-service): implement ToggleSubscriptionMute on MongoStore"
```

---

## Task 5: Regenerate mocks

**Files:**
- Modify: `room-service/mock_store_test.go` (auto-generated; never hand-edit)

- [ ] **Step 1: Regenerate**

Run: `make generate SERVICE=room-service`

- [ ] **Step 2: Verify the new method appears**

Run: `grep -n ToggleSubscriptionMute room-service/mock_store_test.go`

Expected: at least one hit for the mock method.

- [ ] **Step 3: Run unit tests to confirm nothing else broke**

Run: `make test SERVICE=room-service`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add room-service/mock_store_test.go
git commit -m "chore(room-service): regenerate mocks for ToggleSubscriptionMute"
```

---

## Task 6: Wire `publishCore` into the handler

`room-service` currently only injects `publishToStream` (JetStream). The mute handler needs a core-NATS publish path for `chat.user.{account}.event.subscription.update`. Add a second injected closure following the room-worker shape.

**Files:**
- Modify: `room-service/handler.go`
- Modify: `room-service/main.go`

- [ ] **Step 1: Add the field + constructor parameter on `Handler`**

Edit `room-service/handler.go`. In the `Handler` struct (around line 30), add after `publishToStream`:

```go
publishCore     func(ctx context.Context, subj string, data []byte) error
```

In `NewHandler` (around line 43), add the new parameter as the last argument, before the closing `)`:

```go
func NewHandler(store RoomStore, keyStore RoomKeyStore, memberListClient MemberListClient, msgReader MessageReader, siteID string, maxRoomSize, maxBatchSize int, memberListTimeout time.Duration, publishToStream func(context.Context, string, []byte) error, publishCore func(context.Context, string, []byte) error) *Handler {
	return &Handler{
		store:             store,
		keyStore:          keyStore,
		memberListClient:  memberListClient,
		msgReader:         msgReader,
		siteID:            siteID,
		maxRoomSize:       maxRoomSize,
		maxBatchSize:      maxBatchSize,
		memberListTimeout: memberListTimeout,
		publishToStream:   publishToStream,
		publishCore:       publishCore,
	}
}
```

- [ ] **Step 2: Wire it in `main.go`**

Edit `room-service/main.go`. Replace the `handler := NewHandler(...)` block (line 122) with:

```go
handler := NewHandler(store, keyStore, memberListClient, cassReader, cfg.SiteID, cfg.MaxRoomSize, cfg.MaxBatchSize, cfg.MemberListTimeout,
	func(ctx context.Context, subj string, data []byte) error {
		if _, err := js.PublishMsg(ctx, natsutil.NewMsg(ctx, subj, data)); err != nil {
			return fmt.Errorf("publish to %q: %w", subj, err)
		}
		return nil
	},
	func(ctx context.Context, subj string, data []byte) error {
		if err := nc.PublishMsg(ctx, natsutil.NewMsg(ctx, subj, data)); err != nil {
			return fmt.Errorf("publish core to %q: %w", subj, err)
		}
		return nil
	},
)
```

- [ ] **Step 3: Build the service**

Run: `make build SERVICE=room-service`

Expected: PASS — the new param is plumbed end-to-end.

- [ ] **Step 4: Update existing handler tests that construct `Handler{}` literally**

Many tests in `handler_test.go` construct `&Handler{...}` with named fields and never set `publishCore`. That's fine: nil is acceptable when the test path doesn't exercise it. No code change required here. Run the tests to confirm:

Run: `make test SERVICE=room-service`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add room-service/handler.go room-service/main.go
git commit -m "feat(room-service): inject publishCore alongside publishToStream"
```

---

## Task 7: Implement `handleMuteToggle` and register the RPC

**Files:**
- Modify: `room-service/handler.go`
- Modify: `room-service/handler_test.go`

- [ ] **Step 1: Write failing handler unit tests**

Append to `room-service/handler_test.go`:

```go
func TestHandler_MuteToggle_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)

	store.EXPECT().
		GetSubscription(gomock.Any(), "alice", "r1").
		Return(&model.Subscription{
			ID:     "s1",
			User:   model.SubscriptionUser{ID: "u1", Account: "alice"},
			RoomID: "r1",
			SiteID: "site-a",
		}, nil)
	store.EXPECT().
		ToggleSubscriptionMute(gomock.Any(), "r1", "alice").
		Return(true, nil)
	store.EXPECT().
		GetUserSiteID(gomock.Any(), "alice").
		Return("site-a", nil) // same site → no outbox publish

	var coreSubjects []string
	var coreBodies [][]byte
	h := &Handler{
		store:  store,
		siteID: "site-a",
		publishToStream: func(_ context.Context, _ string, _ []byte) error {
			t.Fatal("publishToStream must not be called for same-site mute toggle")
			return nil
		},
		publishCore: func(_ context.Context, subj string, data []byte) error {
			coreSubjects = append(coreSubjects, subj)
			coreBodies = append(coreBodies, data)
			return nil
		},
	}

	subj := subject.MuteToggle("alice", "r1", "site-a")
	resp, err := h.handleMuteToggle(context.Background(), subj, nil)
	require.NoError(t, err)

	var got model.MuteToggleResponse
	require.NoError(t, json.Unmarshal(resp, &got))
	assert.Equal(t, "ok", got.Status)
	assert.True(t, got.DisableNotifications)

	require.Len(t, coreSubjects, 1)
	assert.Equal(t, subject.SubscriptionUpdate("alice"), coreSubjects[0])

	var evt model.SubscriptionUpdateEvent
	require.NoError(t, json.Unmarshal(coreBodies[0], &evt))
	assert.Equal(t, "mute_toggled", evt.Action)
	assert.True(t, evt.Subscription.DisableNotifications)
	assert.Equal(t, "alice", evt.Subscription.User.Account)
}

func TestHandler_MuteToggle_CrossSitePublishesOutbox(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)

	store.EXPECT().
		GetSubscription(gomock.Any(), "alice", "r1").
		Return(&model.Subscription{
			User: model.SubscriptionUser{ID: "u1", Account: "alice"},
			RoomID: "r1", SiteID: "site-a",
		}, nil)
	store.EXPECT().
		ToggleSubscriptionMute(gomock.Any(), "r1", "alice").
		Return(true, nil)
	store.EXPECT().
		GetUserSiteID(gomock.Any(), "alice").
		Return("site-b", nil)

	var streamSubj string
	var streamData []byte
	h := &Handler{
		store: store, siteID: "site-a",
		publishToStream: func(_ context.Context, s string, d []byte) error {
			streamSubj = s
			streamData = d
			return nil
		},
		publishCore: func(_ context.Context, _ string, _ []byte) error { return nil },
	}

	subj := subject.MuteToggle("alice", "r1", "site-a")
	_, err := h.handleMuteToggle(context.Background(), subj, nil)
	require.NoError(t, err)

	assert.Equal(t, subject.Outbox("site-a", "site-b", model.OutboxSubscriptionMuteToggled), streamSubj)

	var outbox model.OutboxEvent
	require.NoError(t, json.Unmarshal(streamData, &outbox))
	assert.Equal(t, model.OutboxSubscriptionMuteToggled, outbox.Type)
	assert.Equal(t, "site-a", outbox.SiteID)
	assert.Equal(t, "site-b", outbox.DestSiteID)

	var payload model.SubscriptionMuteToggledEvent
	require.NoError(t, json.Unmarshal(outbox.Payload, &payload))
	assert.Equal(t, "alice", payload.Account)
	assert.Equal(t, "r1", payload.RoomID)
	assert.True(t, payload.DisableNotifications)
	assert.NotZero(t, payload.Timestamp)
}

func TestHandler_MuteToggle_NotRoomMember(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)

	store.EXPECT().
		GetSubscription(gomock.Any(), "alice", "r1").
		Return(nil, model.ErrSubscriptionNotFound)

	h := &Handler{
		store: store, siteID: "site-a",
		publishToStream: func(_ context.Context, _ string, _ []byte) error { return nil },
		publishCore:     func(_ context.Context, _ string, _ []byte) error { return nil },
	}

	subj := subject.MuteToggle("alice", "r1", "site-a")
	_, err := h.handleMuteToggle(context.Background(), subj, nil)
	assert.ErrorIs(t, err, errNotRoomMember)
}

func TestHandler_MuteToggle_InvalidSubject(t *testing.T) {
	h := &Handler{
		siteID:          "site-a",
		publishToStream: func(_ context.Context, _ string, _ []byte) error { return nil },
		publishCore:     func(_ context.Context, _ string, _ []byte) error { return nil },
	}
	_, err := h.handleMuteToggle(context.Background(), "garbage.subject", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid mute-toggle subject")
}

func TestHandler_MuteToggle_StoreError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)

	store.EXPECT().
		GetSubscription(gomock.Any(), "alice", "r1").
		Return(&model.Subscription{
			User: model.SubscriptionUser{ID: "u1", Account: "alice"}, RoomID: "r1",
		}, nil)
	store.EXPECT().
		ToggleSubscriptionMute(gomock.Any(), "r1", "alice").
		Return(false, fmt.Errorf("db down"))

	h := &Handler{
		store: store, siteID: "site-a",
		publishToStream: func(_ context.Context, _ string, _ []byte) error { return nil },
		publishCore:     func(_ context.Context, _ string, _ []byte) error { return nil },
	}
	subj := subject.MuteToggle("alice", "r1", "site-a")
	_, err := h.handleMuteToggle(context.Background(), subj, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "toggle subscription mute")
}
```

- [ ] **Step 2: Run — expect FAIL**

Run: `make test SERVICE=room-service`

Expected: FAIL — `handleMuteToggle` undefined.

- [ ] **Step 3: Implement `handleMuteToggle` and `natsMuteToggle`**

Append to `room-service/handler.go` (at the bottom, after `handleEnsureRoomKey`):

```go
func (h *Handler) natsMuteToggle(m otelnats.Msg) {
	ctx := wrappedCtx(m)
	resp, err := h.handleMuteToggle(ctx, m.Msg.Subject, m.Msg.Data)
	if err != nil {
		slog.Error("mute toggle failed", "error", err, "subject", m.Msg.Subject)
		natsutil.ReplyError(m.Msg, sanitizeError(err))
		return
	}
	if err := m.Msg.Respond(resp); err != nil {
		slog.Error("failed to respond to mute toggle", "error", err)
	}
}

func (h *Handler) handleMuteToggle(ctx context.Context, subj string, _ []byte) ([]byte, error) {
	account, roomID, ok := subject.ParseUserRoomSubject(subj)
	if !ok {
		return nil, fmt.Errorf("invalid mute-toggle subject: %s", subj)
	}

	sub, err := h.store.GetSubscription(ctx, account, roomID)
	switch {
	case errors.Is(err, model.ErrSubscriptionNotFound):
		return nil, errNotRoomMember
	case err != nil:
		return nil, fmt.Errorf("get subscription: %w", err)
	}

	newVal, err := h.store.ToggleSubscriptionMute(ctx, roomID, account)
	if err != nil {
		if errors.Is(err, model.ErrSubscriptionNotFound) {
			return nil, errNotRoomMember
		}
		return nil, fmt.Errorf("toggle subscription mute: %w", err)
	}

	now := time.Now().UTC()

	updatedSub := *sub
	updatedSub.DisableNotifications = newVal
	subEvt := model.SubscriptionUpdateEvent{
		UserID:       sub.User.ID,
		Subscription: updatedSub,
		Action:       "mute_toggled",
		Timestamp:    now.UnixMilli(),
	}
	subEvtData, err := json.Marshal(subEvt)
	if err != nil {
		return nil, fmt.Errorf("marshal subscription update event: %w", err)
	}
	if err := h.publishCore(ctx, subject.SubscriptionUpdate(account), subEvtData); err != nil {
		slog.Error("subscription update publish failed", "error", err, "account", account)
		// Non-fatal — the DB write is the source of truth; clients will reconcile on next refetch.
	}

	userSiteID, err := h.store.GetUserSiteID(ctx, account)
	if err != nil {
		slog.Warn("get user siteId failed; skipping outbox", "error", err, "account", account)
		return json.Marshal(model.MuteToggleResponse{Status: "ok", DisableNotifications: newVal})
	}
	if userSiteID != "" && userSiteID != h.siteID {
		payload := model.SubscriptionMuteToggledEvent{
			Account:              account,
			RoomID:               roomID,
			DisableNotifications: newVal,
			Timestamp:            now.UnixMilli(),
		}
		payloadData, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal mute-toggled payload: %w", err)
		}
		outbox := model.OutboxEvent{
			Type:       model.OutboxSubscriptionMuteToggled,
			SiteID:     h.siteID,
			DestSiteID: userSiteID,
			Payload:    payloadData,
			Timestamp:  now.UnixMilli(),
		}
		outboxData, err := json.Marshal(outbox)
		if err != nil {
			return nil, fmt.Errorf("marshal outbox event: %w", err)
		}
		if err := h.publishToStream(ctx, subject.Outbox(h.siteID, userSiteID, model.OutboxSubscriptionMuteToggled), outboxData); err != nil {
			return nil, fmt.Errorf("publish mute-toggled outbox: %w", err)
		}
	}

	return json.Marshal(model.MuteToggleResponse{Status: "ok", DisableNotifications: newVal})
}
```

- [ ] **Step 4: Register the subject in `RegisterCRUD`**

Edit `room-service/handler.go`. Inside `RegisterCRUD` (around line 63), append a new subscription before the closing `return nil`:

```go
if _, err := nc.QueueSubscribe(subject.MuteToggleWildcard(h.siteID), queue, h.natsMuteToggle); err != nil {
    return fmt.Errorf("subscribe mute toggle: %w", err)
}
```

- [ ] **Step 5: Run — expect PASS**

Run: `make test SERVICE=room-service`

Expected: PASS — all five new handler tests green.

- [ ] **Step 6: Run lint**

Run: `make lint`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add room-service/handler.go room-service/handler_test.go
git commit -m "feat(room-service): add mute.toggle RPC handler"
```

---

## Task 8: Add `subscription_mute_toggled` handler to `inbox-worker`

**Files:**
- Modify: `inbox-worker/handler.go`
- Modify: `inbox-worker/handler_test.go`
- Modify: `inbox-worker/store.go` (interface) and `inbox-worker/store_mongo.go` (impl) — confirm exact filenames first via `ls inbox-worker/`
- Modify: `inbox-worker/mock_store_test.go` (regenerated)

- [ ] **Step 1: Locate the store files**

Run: `ls inbox-worker/`

Confirm the file names that hold the `InboxStore` interface and its Mongo impl (they may not exactly match `store.go` / `store_mongo.go`). Adjust the subsequent steps' paths accordingly.

- [ ] **Step 2: Add the store interface method**

In the file that defines `InboxStore` (the interface block is in `inbox-worker/handler.go:18-32`), append the new method to the interface:

```go
// UpdateSubscriptionMute sets disableNotifications on the subscription keyed
// by (roomID, account). Missing-subscription is a silent no-op so federation
// races during membership delete don't surface errors.
UpdateSubscriptionMute(ctx context.Context, roomID, account string, disableNotifications bool) error
```

- [ ] **Step 3: Add the dispatch case**

In the `HandleEvent` switch (around line 51), add:

```go
case "subscription_mute_toggled":
    return h.handleSubscriptionMuteToggled(ctx, &evt)
```

(Place the case after `subscription_read` to keep related cases together.)

- [ ] **Step 4: Add the handler method**

Append after `handleSubscriptionRead` (around line 200):

```go
// handleSubscriptionMuteToggled mirrors a mute toggle from the room's home
// site onto the user's home-site subscription copy. Missing-subscription is
// a silent no-op (the store enforces this) so a federation race between a
// membership delete and a mute toggle does not error the consumer.
func (h *Handler) handleSubscriptionMuteToggled(ctx context.Context, evt *model.OutboxEvent) error {
    var e model.SubscriptionMuteToggledEvent
    if err := json.Unmarshal(evt.Payload, &e); err != nil {
        return fmt.Errorf("unmarshal subscription_mute_toggled payload: %w", err)
    }
    if err := h.store.UpdateSubscriptionMute(ctx, e.RoomID, e.Account, e.DisableNotifications); err != nil {
        return fmt.Errorf("update subscription mute for %q in room %q: %w", e.Account, e.RoomID, err)
    }
    return nil
}
```

- [ ] **Step 5: Write a failing unit test**

Append to `inbox-worker/handler_test.go`:

```go
func TestHandler_SubscriptionMuteToggled(t *testing.T) {
    ctrl := gomock.NewController(t)
    store := NewMockInboxStore(ctrl)

    store.EXPECT().
        UpdateSubscriptionMute(gomock.Any(), "r1", "alice", true).
        Return(nil)

    h := NewHandler(store)

    payload, err := json.Marshal(model.SubscriptionMuteToggledEvent{
        Account: "alice", RoomID: "r1", DisableNotifications: true, Timestamp: 12345,
    })
    require.NoError(t, err)
    evt, err := json.Marshal(model.OutboxEvent{
        Type: model.OutboxSubscriptionMuteToggled, SiteID: "site-a", DestSiteID: "site-b",
        Payload: payload, Timestamp: 12345,
    })
    require.NoError(t, err)

    require.NoError(t, h.HandleEvent(context.Background(), evt))
}
```

(Match imports/import style with existing inbox-worker tests.)

- [ ] **Step 6: Run — expect FAIL**

Run: `make test SERVICE=inbox-worker`

Expected: FAIL — `UpdateSubscriptionMute` mock doesn't exist yet.

- [ ] **Step 7: Implement on the Mongo impl**

In the inbox-worker Mongo impl file (likely `store_mongo.go`), add:

```go
// UpdateSubscriptionMute sets disableNotifications on the subscription keyed
// by (roomID, account). Missing-subscription is a silent no-op.
func (s *MongoStore) UpdateSubscriptionMute(ctx context.Context, roomID, account string, disableNotifications bool) error {
    _, err := s.subscriptions.UpdateOne(ctx,
        bson.M{"roomId": roomID, "u.account": account},
        bson.M{"$set": bson.M{"disableNotifications": disableNotifications}},
    )
    if err != nil {
        return fmt.Errorf("update subscription mute for %q in room %q: %w", account, roomID, err)
    }
    return nil
}
```

If the inbox-worker `MongoStore` struct uses a different field name for the subscriptions collection (e.g. `subs`, `Subscriptions`), match it — peek at one existing method like `UpdateSubscriptionRead` for the right receiver name.

- [ ] **Step 8: Regenerate mocks**

Run: `make generate SERVICE=inbox-worker`

- [ ] **Step 9: Run — expect PASS**

Run: `make test SERVICE=inbox-worker`

Expected: PASS.

- [ ] **Step 10: Lint**

Run: `make lint`

Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add inbox-worker/
git commit -m "feat(inbox-worker): mirror subscription_mute_toggled outbox event"
```

---

## Task 9: Document the new RPC in `docs/client-api.md`

**Files:**
- Modify: `docs/client-api.md`

- [ ] **Step 1: Insert the new section after Mark Messages Read**

Find the line `#### Mark Messages Read` (around line 735) and locate its closing `---` separator (around line 783). Insert the following section immediately before the next `####` block:

````markdown
#### Toggle Mute

**Subject:** `chat.user.{account}.request.room.{roomID}.{siteID}.mute.toggle`
**Reply subject:** auto-generated `_INBOX.>` (NATS request/reply)

Synchronous RPC. `room-service` flips `Subscription.disableNotifications` for the requester in a single atomic Mongo `FindOneAndUpdate`, replies with the resulting value, fans out a `subscription.update` event to the user's other client sessions, and (for cross-site users) publishes a `subscription_mute_toggled` outbox event so `inbox-worker` mirrors the change on the user's home site.

Idempotency: this is a toggle, not a set — every successful call flips the bit. Clients must debounce the user-visible action; redelivery of the same RPC will flip back.

##### Request body

The subject already carries `account` and `roomID`, so no body fields are required. Clients may send `{}` or omit the body entirely; any body content is ignored.

##### Success response

| Field                  | Type    | Notes |
|------------------------|---------|-------|
| `status`               | string  | Always `"ok"`. |
| `disableNotifications` | boolean | The resulting value of `Subscription.disableNotifications` after the flip. |

```json
{ "status": "ok", "disableNotifications": true }
```

##### Error response

See [Error envelope](#6-error-envelope-reference). Common errors:

- `"only room members can list members"` — the user has no subscription in the room (sentinel reused across membership-gated RPCs).
- `"invalid mute-toggle subject: …"` — the subject is malformed.

##### Triggered events — success path

**`chat.user.{account}.event.subscription.update`** — emitted once for the requester so other client sessions reconcile.

| Field          | Type   | Notes |
|----------------|--------|-------|
| `userId`       | string | The requester's internal user ID. |
| `subscription` | object | The `Subscription` record with the updated `disableNotifications`. |
| `action`       | string | `"mute_toggled"`. |
| `timestamp`    | number | Milliseconds since Unix epoch (UTC). |

##### Behaviour notes

- **Cross-site federation:** if the user's home site (`users.siteId`) differs from the handler's site, a `subscription_mute_toggled` outbox event is published to `outbox.{handlerSite}.to.{userSite}.subscription_mute_toggled` with payload `{account, roomId, disableNotifications, timestamp}`. The destination `inbox-worker` applies an unconditional `$set` on the local subscription mirror.
- **Notification delivery:** `notification-worker` does **not** yet consult `disableNotifications` before sending. End-to-end mute behaviour is wired only as far as the persisted flag; honouring it in fan-out is a follow-up.

---
````

- [ ] **Step 2: Commit**

```bash
git add docs/client-api.md
git commit -m "docs(client-api): document mute.toggle RPC"
```

---

## Task 10: Final verification

- [ ] **Step 1: Full suite**

Run: `make lint && make test && make test-integration SERVICE=room-service && make test-integration SERVICE=inbox-worker`

Expected: all green.

- [ ] **Step 2: SAST**

Run: `make sast`

Expected: PASS.

- [ ] **Step 3: Stale-reference scan**

Run: `grep -rn "DisableNotification\b\|disableNotification\b" --include="*.go" .`

Expected: zero hits (rename complete in production code; historical specs/plans intentionally untouched).

- [ ] **Step 4: Push the branch**

```bash
git push -u origin claude/explore-room-service-7tNlq
```

---

## Spec coverage self-review

| Requirement | Task |
|---|---|
| Rename `DisableNotification` → `DisableNotifications` everywhere in production code + active tests | Task 0 |
| New per-user `mute.toggle` RPC subject + parser | Task 1 |
| Public response model + outbox event type/payload | Task 2 |
| Atomic Mongo toggle that returns post-flip value | Tasks 3, 4 |
| Mocks for the new store method | Task 5 |
| Handler-side wiring for core-NATS publish | Task 6 |
| RPC handler: validates membership, writes, fans out subscription.update, publishes cross-site outbox | Task 7 |
| `inbox-worker` mirror handler + store method | Task 8 |
| `docs/client-api.md` covers new RPC + triggered events | Task 9 |
| All checks green | Task 10 |
| Cross-site stale-data analysis recorded | "Cross-site stale-data assessment" section above |
| Production Mongo migration of renamed field documented as ops follow-up | Same section above |
| `notification-worker` honouring `disableNotifications` documented as follow-up | Behaviour notes in Task 9 |
