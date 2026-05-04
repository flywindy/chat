# Message-worker thread subscription outbox events implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replicate `ThreadSubscription` writes (parent author, replier, mentionees) from message-worker on the room's home site to each affected user's home site via the existing OUTBOX/INBOX federation.

> **⚠️ Post-implementation correction.** Task 5 below describes
> `ThreadSubscription.SiteID` as the **owner's** home site. That is **wrong**:
> `SiteID` is the **room's** home site (matching `Subscription.SiteID`),
> preserved unchanged across federation. Owner-site routing is handled
> transiently via an explicit `ownerSiteID` argument to
> `publishThreadSubOutboxIfRemote` — never written onto the subscription.
> Plan body is preserved as-written for traceability; canonical design lives
> in [`../specs/2026-04-28-message-worker-thread-subscription-outbox-design.md`](../specs/2026-04-28-message-worker-thread-subscription-outbox-design.md).
> Correction commit: `1c3915c`. See "Post-implementation correction" section
> at the end for the full diff summary.

**Architecture:** Mirror room-worker's pattern. Message-worker gains a `publish PublishFunc` field; after every local Insert/Upsert/MarkThreadSubscriptionMention it emits a `thread_subscription_upserted` outbox event when the subscription owner's site differs from the room's site. Inbox-worker dispatches the new event type to a new `UpsertThreadSubscription` store method that uses MongoDB `$max` on `hasMention` for monotonic mention-flag merge.

**Tech Stack:** Go 1.25, NATS JetStream (`github.com/nats-io/nats.go/jetstream`), MongoDB (`go.mongodb.org/mongo-driver/v2`), `github.com/hmchangw/chat/pkg/{model,subject,idgen,mention,userstore,stream}`, `go.uber.org/mock`, `stretchr/testify`, `testcontainers-go`.

**Spec:** `docs/superpowers/specs/2026-04-28-message-worker-thread-subscription-outbox-design.md`

---

## File map

**Modify:**
- `pkg/model/event.go` — new `OutboxThreadSubscriptionUpserted` const; new `SiteID` field on `Participant`
- `pkg/model/model_test.go` — round-trip test for new event-type tag and `Participant.SiteID`
- `pkg/mention/mention.go` — propagate `SiteID` from looked-up `User` onto `Participant`
- `pkg/mention/mention_test.go` — extend `TestResolve` to assert `SiteID`
- `message-worker/handler.go` — `Handler` gains `siteID` + `publish` fields; new `publishThreadSubOutboxIfRemote` helper; owner-site semantic for `ThreadSubscription.SiteID`; parent-user lookup; publish wiring in three paths
- `message-worker/handler_test.go` — new test cases for cross-site publishes
- `message-worker/main.go` — wire `cfg.SiteID` and JetStream publish closure into `NewHandler`
- `inbox-worker/handler.go` — new dispatch case + handler for `thread_subscription_upserted`; extend `InboxStore` interface
- `inbox-worker/handler_test.go` — extend stub store with `threadSubscriptions`; new dispatch tests
- `inbox-worker/main.go` — `mongoInboxStore` gains `threadSubCol` + `UpsertThreadSubscription` method
- `inbox-worker/integration_test.go` — new integration tests for upsert insert path + monotonic `hasMention` merge

**No new files.** All work fits in existing files.

---

## Task 1: Add `OutboxThreadSubscriptionUpserted` constant

**Files:**
- Modify: `pkg/model/event.go:79-84`
- Modify (test): `pkg/model/model_test.go` (append a new test)

- [ ] **Step 1.1: Write the failing model test**

Append to `pkg/model/model_test.go` (after the existing `TestOutboxEventJSON`):

```go
func TestOutboxEventJSON_ThreadSubscriptionUpserted(t *testing.T) {
	src := model.OutboxEvent{
		Type:       model.OutboxThreadSubscriptionUpserted,
		SiteID:     "site-a",
		DestSiteID: "site-b",
		Payload:    []byte(`{"id":"sub-1","threadRoomId":"tr-1"}`),
		Timestamp:  1735689600000,
	}
	data, err := json.Marshal(&src)
	require.NoError(t, err)

	var dst model.OutboxEvent
	require.NoError(t, json.Unmarshal(data, &dst))
	if !reflect.DeepEqual(src, dst) {
		t.Errorf("round-trip mismatch:\n  got  %+v\n  want %+v", dst, src)
	}
	if dst.Type != "thread_subscription_upserted" {
		t.Errorf("Type = %q, want thread_subscription_upserted", dst.Type)
	}
}
```

- [ ] **Step 1.2: Run test to verify it fails**

Run: `make test SERVICE=pkg/model 2>&1 | tail -30`

Expected: compile error `undefined: model.OutboxThreadSubscriptionUpserted`.

- [ ] **Step 1.3: Add the constant**

In `pkg/model/event.go`, replace the existing const block (line ~81-84):

```go
const (
	OutboxMemberAdded                OutboxEventType = "member_added"
	OutboxMemberRemoved              OutboxEventType = "member_removed"
	OutboxThreadSubscriptionUpserted OutboxEventType = "thread_subscription_upserted"
)
```

- [ ] **Step 1.4: Run test to verify it passes**

Run: `make test SERVICE=pkg/model`

Expected: all `pkg/model` tests pass including the new one.

- [ ] **Step 1.5: Commit**

```bash
git add pkg/model/event.go pkg/model/model_test.go
git commit -m "model: add OutboxThreadSubscriptionUpserted event type constant"
```

---

## Task 2: Add `SiteID` to `model.Participant` and propagate from `mention.Resolve`

**Files:**
- Modify: `pkg/model/event.go` (the `Participant` struct, around line 105-110)
- Modify: `pkg/model/model_test.go` (extend `TestParticipantJSON`)
- Modify: `pkg/mention/mention.go:77-84`
- Modify: `pkg/mention/mention_test.go` (extend `TestResolve`)

- [ ] **Step 2.1: Write the failing model test for `Participant.SiteID`**

Replace the `TestParticipantJSON` function in `pkg/model/model_test.go` with the version below (it adds two new sub-tests; keep the existing two):

```go
func TestParticipantJSON(t *testing.T) {
	t.Run("with userID", func(t *testing.T) {
		p := model.Participant{
			UserID:      "u1",
			Account:     "alice",
			ChineseName: "愛麗絲",
			EngName:     "Alice Wang",
		}
		roundTrip(t, &p, &model.Participant{})
	})

	t.Run("without userID omitted", func(t *testing.T) {
		p := model.Participant{
			Account:     "bob",
			ChineseName: "鮑勃",
			EngName:     "Bob Chen",
		}
		data, err := json.Marshal(p)
		require.NoError(t, err)

		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		_, hasUserID := raw["userId"]
		assert.False(t, hasUserID, "userId should be omitted when empty")

		var dst model.Participant
		require.NoError(t, json.Unmarshal(data, &dst))
		assert.Equal(t, p, dst)
	})

	t.Run("with siteID round-trips", func(t *testing.T) {
		p := model.Participant{
			UserID:      "u1",
			Account:     "alice",
			SiteID:      "site-a",
			ChineseName: "愛麗絲",
			EngName:     "Alice Wang",
		}
		roundTrip(t, &p, &model.Participant{})
	})

	t.Run("siteID omitted when empty", func(t *testing.T) {
		p := model.Participant{
			UserID:  "u1",
			Account: "alice",
			EngName: "Alice Wang",
		}
		data, err := json.Marshal(p)
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		_, hasSiteID := raw["siteId"]
		assert.False(t, hasSiteID, "siteId should be omitted when empty")
	})
}
```

- [ ] **Step 2.2: Run test to verify it fails**

Run: `make test SERVICE=pkg/model`

Expected: compile error `unknown field SiteID in struct literal of type model.Participant`.

- [ ] **Step 2.3: Add the `SiteID` field**

In `pkg/model/event.go`, replace the `Participant` struct definition with:

```go
// Participant represents a user with display name info for client rendering.
type Participant struct {
	UserID      string `json:"userId,omitempty" bson:"userId,omitempty"`
	Account     string `json:"account" bson:"account"`
	SiteID      string `json:"siteId,omitempty" bson:"siteId,omitempty"`
	ChineseName string `json:"chineseName" bson:"chineseName"`
	EngName     string `json:"engName" bson:"engName"`
}
```

- [ ] **Step 2.4: Run model tests to verify they pass**

Run: `make test SERVICE=pkg/model`

Expected: all `pkg/model` tests pass.

- [ ] **Step 2.5: Write the failing mention test**

Replace the "single mention resolved" and "multiple mentions resolved" cases in `TestResolve` (`pkg/mention/mention_test.go`) — change the user fixtures and `wantParts` to include `SiteID`. Use this diff (replace the two named test cases inline):

```go
		bobUser := model.User{ID: "u-bob", Account: "bob", SiteID: "site-b", EngName: "Bob Chen", ChineseName: "鮑勃"}
		aliceUser := model.User{ID: "u-alice", Account: "alice", SiteID: "site-a", EngName: "Alice Wang", ChineseName: "愛麗絲"}
```

(Update both `bobUser` and `aliceUser` declarations at the top of `TestResolve` — add `SiteID` field.)

Then update `wantParts` for these cases:

```go
		{
			name:         "single mention resolved",
			content:      "hey @bob",
			lookupUsers:  []model.User{bobUser},
			wantAccounts: []string{"bob"},
			wantParts: []model.Participant{
				{UserID: "u-bob", Account: "bob", SiteID: "site-b", EngName: "Bob Chen", ChineseName: "鮑勃"},
			},
		},
		{
			name:         "multiple mentions resolved",
			content:      "@alice and @bob",
			lookupUsers:  []model.User{aliceUser, bobUser},
			wantAccounts: []string{"alice", "bob"},
			wantParts: []model.Participant{
				{UserID: "u-alice", Account: "alice", SiteID: "site-a", EngName: "Alice Wang", ChineseName: "愛麗絲"},
				{UserID: "u-bob", Account: "bob", SiteID: "site-b", EngName: "Bob Chen", ChineseName: "鮑勃"},
			},
		},
```

The other cases (`@all only`, `@all and individual`, `unresolved account skipped`, `lookup error — partial result`) are unchanged — `@all` Participants intentionally have no `SiteID`.

- [ ] **Step 2.6: Run test to verify it fails**

Run: `make test SERVICE=pkg/mention`

Expected: `Resolve` returns Participants with empty `SiteID` — test diffs report missing `site-a` / `site-b`.

- [ ] **Step 2.7: Propagate SiteID in `mention.Resolve`**

In `pkg/mention/mention.go`, replace the loop that builds `result.Participants` from `users` (lines ~77-84):

```go
		for i := range users {
			result.Participants = append(result.Participants, model.Participant{
				UserID:      users[i].ID,
				Account:     users[i].Account,
				SiteID:      users[i].SiteID,
				ChineseName: users[i].ChineseName,
				EngName:     users[i].EngName,
			})
		}
```

- [ ] **Step 2.8: Run tests to verify they pass**

Run: `make test SERVICE=pkg/mention && make test SERVICE=pkg/model`

Expected: both packages pass.

- [ ] **Step 2.9: Verify no other consumer broke**

Run: `make test`

Expected: full unit test suite passes. (`Participant.SiteID` is omitempty and additive; existing fixtures that don't set it serialize identically.)

- [ ] **Step 2.10: Commit**

```bash
git add pkg/model/event.go pkg/model/model_test.go pkg/mention/mention.go pkg/mention/mention_test.go
git commit -m "model+mention: add SiteID to Participant, propagate from Resolve"
```

---

## Task 3: Add `siteID` and `publish` fields to message-worker `Handler`

This is a compile-only refactor. No new behavior — we just plumb the fields through `NewHandler` and update existing tests/main wiring. Behavior changes start in Task 4.

**Files:**
- Modify: `message-worker/handler.go:19-27` (Handler struct + NewHandler)
- Modify: `message-worker/handler_test.go` (every `NewHandler(...)` call)
- Modify: `message-worker/main.go:95` (the `NewHandler(...)` call site)
- Modify: `message-worker/integration_test.go` (every `NewHandler(...)` call site, if any)

- [ ] **Step 3.1: Update `Handler` struct and `NewHandler` constructor**

In `message-worker/handler.go`, replace lines 19-27 with:

```go
// PublishFunc publishes data; non-empty msgID sets Nats-Msg-Id for JetStream stream-level dedup.
// Mirrors room-worker's PublishFunc signature so message-worker can plug into the same publish closure.
type PublishFunc func(ctx context.Context, subj string, data []byte, msgID string) error

type Handler struct {
	store       Store
	userStore   userstore.UserStore
	threadStore ThreadStore
	siteID      string
	publish     PublishFunc
}

func NewHandler(store Store, userStore userstore.UserStore, threadStore ThreadStore, siteID string, publish PublishFunc) *Handler {
	return &Handler{
		store:       store,
		userStore:   userStore,
		threadStore: threadStore,
		siteID:      siteID,
		publish:     publish,
	}
}
```

- [ ] **Step 3.2: Update existing `NewHandler` call sites in tests**

In `message-worker/handler_test.go`, find every `NewHandler(mockStore, mockUserStore, mockThreadStore)` call (3 sites — `TestHandler_ProcessMessage`, `TestHandler_HandleThreadRoomAndSubscriptions`, `TestHandler_HandleJetStreamMsg`) and replace each with:

```go
				h := NewHandler(mockStore, mockUserStore, mockThreadStore, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error {
					return nil
				})
```

Use exactly `"site-a"` — every existing test fixture uses that as both `User.SiteID` and `evt.SiteID`, so a no-op publish closure will be exercised but no remote-publish branch is taken yet (we add that in Task 4+).

- [ ] **Step 3.3: Update `main.go` `NewHandler` call**

In `message-worker/main.go` replace the line `handler := NewHandler(store, us, threadStore)` (~line 95) with:

```go
	handler := NewHandler(store, us, threadStore, cfg.SiteID, func(ctx context.Context, subj string, data []byte, msgID string) error {
		if msgID == "" {
			return nc.Publish(ctx, subj, data)
		}
		_, err := js.Publish(ctx, subj, data, jetstream.WithMsgID(msgID))
		return err
	})
```

- [ ] **Step 3.4: Update integration test `NewHandler` call sites if any**

Run: `grep -n "NewHandler(" message-worker/integration_test.go`

If results, update each call to pass `"site-a"` as siteID and a no-op publish closure (same shape as Step 3.2). If no results, skip this step.

- [ ] **Step 3.5: Verify build + unit tests**

Run: `make lint && make test SERVICE=message-worker`

Expected: pass. All existing tests still green — behavior unchanged.

- [ ] **Step 3.6: Commit**

```bash
git add message-worker/handler.go message-worker/handler_test.go message-worker/main.go message-worker/integration_test.go
git commit -m "message-worker: plumb siteID and PublishFunc into Handler"
```

---

## Task 4: Implement `publishThreadSubOutboxIfRemote` helper

The helper centralizes the local-vs-remote decision, payload marshalling, dedup-ID derivation, and publish call. It's used by Tasks 6, 7, 8.

**Files:**
- Modify: `message-worker/handler.go` (append helper near the bottom)
- Modify: `message-worker/handler_test.go` (append a new test function)

- [ ] **Step 4.1: Write the failing helper test**

Append to `message-worker/handler_test.go` (new top-level function, before the `fakeJSMsg` declaration):

```go
func TestHandler_PublishThreadSubOutboxIfRemote(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	baseSub := &model.ThreadSubscription{
		ID:              "sub-1",
		ParentMessageID: "pm-1",
		RoomID:          "r1",
		ThreadRoomID:    "tr-1",
		UserID:          "u-bob",
		UserAccount:     "bob",
		SiteID:          "site-b",
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	t.Run("same site — no publish", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		var called bool
		h := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), NewMockThreadStore(ctrl), "site-b",
			func(_ context.Context, _ string, _ []byte, _ string) error {
				called = true
				return nil
			})

		err := h.publishThreadSubOutboxIfRemote(context.Background(), baseSub, "msg-1")
		require.NoError(t, err)
		assert.False(t, called, "publish must not be called when sub.SiteID == h.siteID")
	})

	t.Run("empty siteID — skip with warn, no publish", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		var called bool
		h := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), NewMockThreadStore(ctrl), "site-a",
			func(_ context.Context, _ string, _ []byte, _ string) error {
				called = true
				return nil
			})

		emptySub := *baseSub
		emptySub.SiteID = ""
		err := h.publishThreadSubOutboxIfRemote(context.Background(), &emptySub, "msg-1")
		require.NoError(t, err)
		assert.False(t, called, "publish must not be called when sub.SiteID is empty")
	})

	t.Run("remote site — publishes with expected subject and dedup ID", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		var captured struct {
			subj    string
			data    []byte
			msgID   string
			callCnt int
		}
		h := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), NewMockThreadStore(ctrl), "site-a",
			func(_ context.Context, subj string, data []byte, msgID string) error {
				captured.subj = subj
				captured.data = data
				captured.msgID = msgID
				captured.callCnt++
				return nil
			})

		err := h.publishThreadSubOutboxIfRemote(context.Background(), baseSub, "msg-1")
		require.NoError(t, err)
		require.Equal(t, 1, captured.callCnt)
		assert.Equal(t, "outbox.site-a.to.site-b.thread_subscription_upserted", captured.subj)
		assert.NotEmpty(t, captured.msgID, "dedup ID must be set")

		// Same inputs → same dedup ID (stable across redeliveries).
		var second string
		h2 := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), NewMockThreadStore(ctrl), "site-a",
			func(_ context.Context, _ string, _ []byte, msgID string) error {
				second = msgID
				return nil
			})
		require.NoError(t, h2.publishThreadSubOutboxIfRemote(context.Background(), baseSub, "msg-1"))
		assert.Equal(t, captured.msgID, second, "dedup ID must be deterministic for the same (threadRoomID, userID, msgID) seed")

		// Different msgID → different dedup ID.
		var third string
		h3 := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), NewMockThreadStore(ctrl), "site-a",
			func(_ context.Context, _ string, _ []byte, msgID string) error {
				third = msgID
				return nil
			})
		require.NoError(t, h3.publishThreadSubOutboxIfRemote(context.Background(), baseSub, "msg-2"))
		assert.NotEqual(t, captured.msgID, third)

		// Payload is an OutboxEvent whose inner Payload decodes back to the ThreadSubscription.
		var outer model.OutboxEvent
		require.NoError(t, json.Unmarshal(captured.data, &outer))
		assert.Equal(t, model.OutboxThreadSubscriptionUpserted, outer.Type)
		assert.Equal(t, "site-a", outer.SiteID)
		assert.Equal(t, "site-b", outer.DestSiteID)
		assert.Greater(t, outer.Timestamp, int64(0))

		var inner model.ThreadSubscription
		require.NoError(t, json.Unmarshal(outer.Payload, &inner))
		assert.Equal(t, *baseSub, inner)
	})

	t.Run("publish error returned", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		boom := errors.New("publish boom")
		h := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), NewMockThreadStore(ctrl), "site-a",
			func(_ context.Context, _ string, _ []byte, _ string) error {
				return boom
			})

		err := h.publishThreadSubOutboxIfRemote(context.Background(), baseSub, "msg-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
	})
}
```

- [ ] **Step 4.2: Run test to verify it fails**

Run: `make test SERVICE=message-worker`

Expected: compile error `h.publishThreadSubOutboxIfRemote undefined`.

- [ ] **Step 4.3: Implement the helper**

Append to `message-worker/handler.go` (after `markThreadMentions`, at end of file). Add `subject` to imports if not already present.

```go
// publishThreadSubOutboxIfRemote publishes a thread_subscription_upserted
// outbox event when sub.SiteID is a remote site. Same-site or empty SiteID
// is a no-op (empty SiteID logs a warning — it indicates a caller bug).
//
// The dedup-ID seed is (threadRoomID, userID, msgID): msg.ID is unique per
// reply, and (msg.ID, userID) is unique within a reply, so the seed is stable
// across MESSAGES_CANONICAL redeliveries and JetStream stream-level dedup
// absorbs duplicates within the dedup window.
func (h *Handler) publishThreadSubOutboxIfRemote(ctx context.Context, sub *model.ThreadSubscription, msgID string) error {
	if sub.SiteID == "" {
		slog.Warn("thread subscription has empty SiteID, skipping outbox publish",
			"threadRoomID", sub.ThreadRoomID, "userID", sub.UserID, "msgID", msgID)
		return nil
	}
	if sub.SiteID == h.siteID {
		return nil
	}

	payload, err := json.Marshal(sub)
	if err != nil {
		return fmt.Errorf("marshal thread subscription: %w", err)
	}
	outbox := model.OutboxEvent{
		Type:       model.OutboxThreadSubscriptionUpserted,
		SiteID:     h.siteID,
		DestSiteID: sub.SiteID,
		Payload:    payload,
		Timestamp:  time.Now().UTC().UnixMilli(),
	}
	data, err := json.Marshal(outbox)
	if err != nil {
		return fmt.Errorf("marshal outbox event: %w", err)
	}
	dedupID := idgen.DeriveID(fmt.Sprintf("thread-sub-outbox:%s:%s:%s", sub.ThreadRoomID, sub.UserID, msgID))
	subj := subject.Outbox(h.siteID, sub.SiteID, model.OutboxThreadSubscriptionUpserted)
	if err := h.publish(ctx, subj, data, dedupID); err != nil {
		return fmt.Errorf("publish thread subscription outbox to %s: %w", sub.SiteID, err)
	}
	return nil
}
```

If the file's import block doesn't already import `"github.com/hmchangw/chat/pkg/subject"`, add it.

- [ ] **Step 4.4: Run tests to verify they pass**

Run: `make test SERVICE=message-worker`

Expected: pass.

- [ ] **Step 4.5: Commit**

```bash
git add message-worker/handler.go message-worker/handler_test.go
git commit -m "message-worker: add publishThreadSubOutboxIfRemote helper"
```

---

## Task 5: Switch `ThreadSubscription.SiteID` to owner-site, lookup parent siteID, warn-and-skip on parent user not found

Today `buildThreadSubscription` is called with the message-event siteID (the room's home site). For cross-site replication the field must reflect the **subscription owner's** home site. This task threads the owner siteID through the three callers and adds a parent-user lookup. No publishing yet — that's Tasks 6/7/8.

**Files:**
- Modify: `message-worker/handler.go:131-204` (handleFirstThreadReply, handleSubsequentThreadReply, buildThreadSubscription, markThreadMentions)
- Modify: `message-worker/handler.go:50-73` (carry replier user through to thread reply path)
- Modify: `message-worker/handler_test.go` (add `FindUserByID` mock expectation for parents in every thread-message test case)

- [ ] **Step 5.1: Update `buildThreadSubscription` to receive owner siteID explicitly**

In `message-worker/handler.go`, replace the existing `buildThreadSubscription` (around line 209-222):

```go
// buildThreadSubscription constructs a ThreadSubscription for (threadRoomID, userID).
// ownerSiteID is the home site of the subscription's owner — NOT the room's site —
// so the document round-trips correctly through the OUTBOX/INBOX federation.
// lastSeenAt is always nil; the field is owned by user-action paths, not message-worker.
func (h *Handler) buildThreadSubscription(msg *model.Message, threadRoomID, userID, userAccount, ownerSiteID string, now time.Time) *model.ThreadSubscription {
	return &model.ThreadSubscription{
		ID:              idgen.GenerateID(),
		ParentMessageID: msg.ThreadParentMessageID,
		RoomID:          msg.RoomID,
		ThreadRoomID:    threadRoomID,
		UserID:          userID,
		UserAccount:     userAccount,
		SiteID:          ownerSiteID,
		LastSeenAt:      nil,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}
```

(Only the parameter name changes from `siteID` → `ownerSiteID`; existing callers stop here being correct because they currently pass the message-event siteID. The next steps fix the callers.)

- [ ] **Step 5.2: Carry replier `*model.User` into the thread-reply path**

The current `processMessage` (lines ~57-92) discards `user` after building `cassParticipant`. We need its `SiteID` inside `handleFirstThreadReply` / `handleSubsequentThreadReply` / `markThreadMentions`. Update `processMessage` to pass `user` through.

Replace lines 75-92 of `message-worker/handler.go` with:

```go
	if evt.Message.ThreadParentMessageID != "" {
		// Resolve (or create) the thread room first so we have the threadRoomID
		// before persisting the message to Cassandra.
		threadRoomID, err := h.handleThreadRoomAndSubscriptions(ctx, &evt.Message, evt.SiteID, user)
		if err != nil {
			return fmt.Errorf("handle thread room and subscriptions: %w", err)
		}
		if err := h.markThreadMentions(ctx, &evt.Message, threadRoomID, evt.SiteID); err != nil {
			return fmt.Errorf("mark thread mentions: %w", err)
		}
		if err := h.store.SaveThreadMessage(ctx, &evt.Message, sender, evt.SiteID, threadRoomID); err != nil {
			return fmt.Errorf("save thread message: %w", err)
		}
	} else {
		if err := h.store.SaveMessage(ctx, &evt.Message, sender, evt.SiteID); err != nil {
			return fmt.Errorf("save message: %w", err)
		}
	}
```

- [ ] **Step 5.3: Update `handleThreadRoomAndSubscriptions` and the two reply handlers**

Replace `handleThreadRoomAndSubscriptions`, `handleFirstThreadReply`, `handleSubsequentThreadReply`, and add `lookupOwnerSiteID` in `message-worker/handler.go` with the versions below. The `replier *model.User` arg flows in; the new `lookupOwnerSiteID` helper resolves the parent's home site via `userStore.FindUserByID` with warn-and-skip on `userstore.ErrUserNotFound` and propagation on other errors.

```go
// handleThreadRoomAndSubscriptions creates the ThreadRoom on first reply and
// inserts ThreadSubscriptions for the parent author and replier. On subsequent
// replies it upserts both subscriptions and bumps the room's last-message pointer.
// It returns the threadRoomID so the caller can pass it to SaveThreadMessage.
//
// `replier` may be nil for system messages with no real user (rare in thread
// paths); subscriptions for the replier are skipped in that case.
func (h *Handler) handleThreadRoomAndSubscriptions(ctx context.Context, msg *model.Message, eventSiteID string, replier *model.User) (string, error) {
	now := msg.CreatedAt

	threadRoom := model.ThreadRoom{
		ID:              idgen.GenerateID(),
		ParentMessageID: msg.ThreadParentMessageID,
		RoomID:          msg.RoomID,
		SiteID:          eventSiteID,
		LastMsgAt:       now,
		LastMsgID:       msg.ID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	err := h.threadStore.CreateThreadRoom(ctx, &threadRoom)
	switch {
	case err == nil:
		return threadRoom.ID, h.handleFirstThreadReply(ctx, msg, threadRoom.ID, replier, now)
	case errors.Is(err, errThreadRoomExists):
		return h.handleSubsequentThreadReply(ctx, msg, replier, now)
	default:
		return "", fmt.Errorf("create thread room: %w", err)
	}
}

// handleFirstThreadReply runs after the thread room has just been created.
// It inserts subscriptions for the parent author (looked up via userStore for
// the parent's home site) and, if distinct, for the replier (using the
// replier's home site).
func (h *Handler) handleFirstThreadReply(ctx context.Context, msg *model.Message, threadRoomID string, replier *model.User, now time.Time) error {
	parentSender, err := h.store.GetMessageSender(ctx, msg.ThreadParentMessageID)
	if err != nil {
		if errors.Is(err, errMessageNotFound) {
			slog.Warn("thread reply parent not found — skipping subscription creation",
				"parentMessageID", msg.ThreadParentMessageID,
				"replyID", msg.ID)
			return nil
		}
		return fmt.Errorf("get parent message sender: %w", err)
	}

	parentSiteID, err := h.lookupOwnerSiteID(ctx, parentSender.ID, "first-reply parent")
	if err != nil {
		return fmt.Errorf("lookup parent owner site: %w", err)
	}
	if parentSiteID != "" {
		if err := h.threadStore.InsertThreadSubscription(ctx,
			h.buildThreadSubscription(msg, threadRoomID, parentSender.ID, parentSender.Account, parentSiteID, now),
		); err != nil {
			return fmt.Errorf("insert parent author thread subscription: %w", err)
		}
	}

	if replier != nil && msg.UserID != parentSender.ID {
		if err := h.threadStore.InsertThreadSubscription(ctx,
			h.buildThreadSubscription(msg, threadRoomID, msg.UserID, msg.UserAccount, replier.SiteID, now),
		); err != nil {
			return fmt.Errorf("insert replier thread subscription: %w", err)
		}
	}

	return nil
}

// handleSubsequentThreadReply runs when CreateThreadRoom reported an existing room.
// Upserts subscriptions for both the parent author and the replier (idempotent
// on redelivery), then bumps the room's last-message pointer. Returns the
// existing thread room ID so the caller can pass it to SaveThreadMessage.
func (h *Handler) handleSubsequentThreadReply(ctx context.Context, msg *model.Message, replier *model.User, now time.Time) (string, error) {
	existingRoom, err := h.threadStore.GetThreadRoomByParentMessageID(ctx, msg.ThreadParentMessageID)
	if err != nil {
		return "", fmt.Errorf("get existing thread room: %w", err)
	}

	parentSender, err := h.store.GetMessageSender(ctx, msg.ThreadParentMessageID)
	switch {
	case err == nil:
		parentSiteID, lookupErr := h.lookupOwnerSiteID(ctx, parentSender.ID, "subsequent-reply parent")
		if lookupErr != nil {
			return "", fmt.Errorf("lookup parent owner site: %w", lookupErr)
		}
		if parentSiteID != "" {
			if err := h.threadStore.UpsertThreadSubscription(ctx,
				h.buildThreadSubscription(msg, existingRoom.ID, parentSender.ID, parentSender.Account, parentSiteID, now),
			); err != nil {
				return "", fmt.Errorf("upsert parent author thread subscription: %w", err)
			}
		}
		if replier != nil && msg.UserID != parentSender.ID {
			if err := h.threadStore.UpsertThreadSubscription(ctx,
				h.buildThreadSubscription(msg, existingRoom.ID, msg.UserID, msg.UserAccount, replier.SiteID, now),
			); err != nil {
				return "", fmt.Errorf("upsert replier thread subscription: %w", err)
			}
		}
	case errors.Is(err, errMessageNotFound):
		slog.Warn("thread reply parent not found — skipping parent subscription upsert",
			"parentMessageID", msg.ThreadParentMessageID,
			"replyID", msg.ID)
		if replier != nil {
			if err := h.threadStore.UpsertThreadSubscription(ctx,
				h.buildThreadSubscription(msg, existingRoom.ID, msg.UserID, msg.UserAccount, replier.SiteID, now),
			); err != nil {
				return "", fmt.Errorf("upsert replier thread subscription: %w", err)
			}
		}
	default:
		return "", fmt.Errorf("get parent message sender: %w", err)
	}

	if err := h.threadStore.UpdateThreadRoomLastMessage(ctx, existingRoom.ID, msg.ID, now); err != nil {
		return "", fmt.Errorf("update thread room last message: %w", err)
	}

	return existingRoom.ID, nil
}

// lookupOwnerSiteID resolves a user's home site by ID.
// Returns ("", nil) when the user is not found (logs a warning) so callers
// can skip that user gracefully — parallels the errMessageNotFound branch
// already in this file. Other DB errors are returned for the caller to NAK on.
func (h *Handler) lookupOwnerSiteID(ctx context.Context, userID, role string) (string, error) {
	user, err := h.userStore.FindUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, userstore.ErrUserNotFound) {
			slog.Warn("owner user not found — skipping thread subscription",
				"userID", userID, "role", role)
			return "", nil
		}
		return "", fmt.Errorf("lookup user %s: %w", userID, err)
	}
	return user.SiteID, nil
}
```

- [ ] **Step 5.4: Update existing `markThreadMentions` to use `Participant.SiteID`**

Replace the existing `markThreadMentions` (around line 227-243 of `handler.go`) with:

```go
// markThreadMentions flips hasMention=true on the thread subscription of every
// @account mentionee in msg (auto-creating the subscription if absent). The
// sender is excluded, and @all is ignored at the thread level. The
// subscription's SiteID is the mentionee's home site (carried on Participant).
func (h *Handler) markThreadMentions(ctx context.Context, msg *model.Message, threadRoomID, eventSiteID string) error {
	for i := range msg.Mentions {
		p := &msg.Mentions[i]
		if p.Account == "all" {
			continue
		}
		if p.UserID == msg.UserID {
			continue
		}
		ownerSiteID := p.SiteID
		if ownerSiteID == "" {
			// Defensive: should not happen since mention.Resolve populates SiteID
			// for resolved users. Fall back to event site so the local mark still
			// happens; outbox publish (Task 8) will skip on empty SiteID anyway.
			slog.Warn("mentionee participant has empty SiteID, falling back to event site",
				"account", p.Account, "userID", p.UserID, "msgID", msg.ID)
			ownerSiteID = eventSiteID
		}
		sub := h.buildThreadSubscription(msg, threadRoomID, p.UserID, p.Account, ownerSiteID, msg.CreatedAt)
		sub.HasMention = true
		if err := h.threadStore.MarkThreadSubscriptionMention(ctx, sub); err != nil {
			return fmt.Errorf("mark thread subscription mention for user %s: %w", p.UserID, err)
		}
	}
	return nil
}
```

- [ ] **Step 5.5: Update existing handler tests with `FindUserByID` parent expectation**

In `message-worker/handler_test.go`, every thread-message test case in `TestHandler_ProcessMessage` and `TestHandler_HandleThreadRoomAndSubscriptions` now does an extra `userStore.FindUserByID(parentSender.ID)` lookup. Add the mock expectation just after the `GetMessageSender` mock in each thread test case.

For `TestHandler_ProcessMessage`, the affected cases are: `"thread message — calls SaveThreadMessage not SaveMessage"`, `"thread message save error — NAK after user lookup"`, `"thread reply mentioning non-participant — marks that user's subscription"`, `"thread reply where sender self-mentions — no MarkThreadSubscriptionMention call"`, `"thread reply with @all only — no MarkThreadSubscriptionMention call"`, `"thread reply with @all + @bob — only bob marked"`, `"thread reply mentioning non-participant — MarkThreadSubscriptionMention error is propagated"`. After each `store.EXPECT().GetMessageSender(...)` line, insert:

```go
					us.EXPECT().FindUserByID(gomock.Any(), "u-parent").
						Return(&model.User{ID: "u-parent", Account: "parent-user", SiteID: "site-a"}, nil)
```

For `TestHandler_HandleThreadRoomAndSubscriptions`, every case that calls `GetMessageSender` and returns a non-error/non-`errMessageNotFound` parentSender needs the same `FindUserByID` mock added. The `errMessageNotFound` cases skip the lookup entirely (the function returns before calling it).

Also: replace `h.handleThreadRoomAndSubscriptions(context.Background(), tt.msg, tt.siteID)` with `h.handleThreadRoomAndSubscriptions(context.Background(), tt.msg, tt.siteID, &model.User{ID: tt.msg.UserID, Account: tt.msg.UserAccount, SiteID: "site-a"})` to thread the replier through (the function signature changed in Step 5.3).

For each affected test in `TestHandler_ProcessMessage`, the `processMessage` entry point already calls `FindUserByID` for the replier — that mock is already set. Nothing more to add on the replier path.

- [ ] **Step 5.6: Add new test cases for parent user-not-found and DB-error**

Append two new test cases in `TestHandler_HandleThreadRoomAndSubscriptions`:

```go
		{
			name:   "first reply — parent user not found in userStore — warn-and-skip parent, still inserts replier",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").Return(parentSender, nil)
			},
			extraUserStoreSetup: func(us *MockUserStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-parent").
					Return(nil, fmt.Errorf("wrap: %w", userstore.ErrUserNotFound))
			},
			expectReplierInsert: true,
		},
		{
			name:   "first reply — parent user lookup DB error — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").Return(parentSender, nil)
			},
			extraUserStoreSetup: func(us *MockUserStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-parent").
					Return(nil, errors.New("mongo: connection refused"))
			},
			wantErr: true,
		},
```

To use `extraUserStoreSetup`, extend the test struct definition near the top of `TestHandler_HandleThreadRoomAndSubscriptions`:

```go
	tests := []struct {
		name                string
		msg                 *model.Message
		siteID              string
		setupMocks          func(store *MockStore, ts *MockThreadStore)
		extraUserStoreSetup func(us *MockUserStore)
		expectReplierInsert bool
		wantErr             bool
	}{
```

And in the run loop, after `tt.setupMocks(...)`, call:

```go
			if tt.extraUserStoreSetup != nil {
				tt.extraUserStoreSetup(mockUserStore)
			}
			if tt.expectReplierInsert {
				mockThreadStore.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
			}
```

Add `"github.com/hmchangw/chat/pkg/userstore"` to the test file's imports if not already present.

- [ ] **Step 5.7: Add `errors` import if missing**

Verify `message-worker/handler.go` imports `"github.com/hmchangw/chat/pkg/userstore"` (it already does — used by `Handler.userStore`). No new imports needed beyond what Task 4 added (`subject`).

- [ ] **Step 5.8: Run tests to verify they pass**

Run: `make generate SERVICE=message-worker && make test SERVICE=message-worker`

Expected: pass. (`make generate` regenerates mocks if any interface signature changed.)

- [ ] **Step 5.9: Commit**

```bash
git add message-worker/handler.go message-worker/handler_test.go message-worker/mock_store_test.go message-worker/mock_userstore_test.go
git commit -m "message-worker: ThreadSubscription.SiteID = owner site; lookup parent siteID"
```

---

## Task 6: Wire outbox publish into `handleFirstThreadReply`

After each `InsertThreadSubscription` succeeds, call `publishThreadSubOutboxIfRemote`. The helper short-circuits same-site, so local-only setups remain quiet.

**Files:**
- Modify: `message-worker/handler.go` (handleFirstThreadReply body — only the post-insert lines)
- Modify: `message-worker/handler_test.go` (new test cases)

- [ ] **Step 6.1: Write the failing test for first-reply remote publish**

Append to `message-worker/handler_test.go` (a new top-level test, not part of the existing tables — having a dedicated function keeps the publish-recording wiring isolated):

```go
func TestHandler_FirstReply_OutboxPublishes(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	parentSender := &cassParticipant{ID: "u-parent", Account: "parent-user"}
	parentUserAtA := &model.User{ID: "u-parent", Account: "parent-user", SiteID: "site-a"}
	parentUserAtC := &model.User{ID: "u-parent", Account: "parent-user", SiteID: "site-c"}

	type publishCall struct {
		subj  string
		data  []byte
		msgID string
	}

	tests := []struct {
		name              string
		replierSite       string
		parentUser        *model.User
		wantPublishToSite map[string]int // destSite → expected count
	}{
		{
			name:              "both local — no publish",
			replierSite:       "site-a",
			parentUser:        parentUserAtA,
			wantPublishToSite: map[string]int{},
		},
		{
			name:              "replier remote — one publish to replier site",
			replierSite:       "site-b",
			parentUser:        parentUserAtA,
			wantPublishToSite: map[string]int{"site-b": 1},
		},
		{
			name:              "parent remote — one publish to parent site",
			replierSite:       "site-a",
			parentUser:        parentUserAtC,
			wantPublishToSite: map[string]int{"site-c": 1},
		},
		{
			name:              "both remote, different sites — two publishes",
			replierSite:       "site-b",
			parentUser:        parentUserAtC,
			wantPublishToSite: map[string]int{"site-b": 1, "site-c": 1},
		},
		{
			name:              "both remote, same site — two publishes to that site",
			replierSite:       "site-b",
			parentUser:        &model.User{ID: "u-parent", Account: "parent-user", SiteID: "site-b"},
			wantPublishToSite: map[string]int{"site-b": 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := NewMockStore(ctrl)
			us := NewMockUserStore(ctrl)
			ts := NewMockThreadStore(ctrl)

			store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").Return(parentSender, nil)
			us.EXPECT().FindUserByID(gomock.Any(), "u-parent").Return(tt.parentUser, nil)
			ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
			ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)

			var calls []publishCall
			h := NewHandler(store, us, ts, "site-a", func(_ context.Context, subj string, data []byte, msgID string) error {
				calls = append(calls, publishCall{subj: subj, data: data, msgID: msgID})
				return nil
			})

			replier := &model.User{ID: "u-replier", Account: "replier", SiteID: tt.replierSite}
			msg := &model.Message{
				ID:                    "msg-reply",
				RoomID:                "r1",
				UserID:                "u-replier",
				UserAccount:           "replier",
				CreatedAt:             now,
				ThreadParentMessageID: "msg-parent",
			}

			err := h.handleFirstThreadReply(context.Background(), msg, "tr-1", replier, now)
			require.NoError(t, err)

			gotByDest := map[string]int{}
			for _, c := range calls {
				var outer model.OutboxEvent
				require.NoError(t, json.Unmarshal(c.data, &outer))
				assert.Equal(t, model.OutboxThreadSubscriptionUpserted, outer.Type)
				gotByDest[outer.DestSiteID]++
			}
			assert.Equal(t, tt.wantPublishToSite, gotByDest)
		})
	}
}

func TestHandler_FirstReply_OutboxPublishError_NAKs(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	us := NewMockUserStore(ctrl)
	ts := NewMockThreadStore(ctrl)

	store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
		Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
	us.EXPECT().FindUserByID(gomock.Any(), "u-parent").
		Return(&model.User{ID: "u-parent", Account: "parent-user", SiteID: "site-c"}, nil)
	ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
	// Replier insert never reached because parent-publish fails first.

	boom := errors.New("publish boom")
	h := NewHandler(store, us, ts, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error {
		return boom
	})

	msg := &model.Message{
		ID: "msg-reply", RoomID: "r1", UserID: "u-replier", UserAccount: "replier",
		CreatedAt: now, ThreadParentMessageID: "msg-parent",
	}
	err := h.handleFirstThreadReply(context.Background(), msg,
		"tr-1", &model.User{ID: "u-replier", SiteID: "site-b"}, now)
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}
```

- [ ] **Step 6.2: Run tests to verify they fail**

Run: `make test SERVICE=message-worker -run TestHandler_FirstReply_Outbox`

Expected: FAIL — current handler never calls publish.

- [ ] **Step 6.3: Wire the publish call into `handleFirstThreadReply`**

In `message-worker/handler.go`, replace the body of `handleFirstThreadReply` (the version from Task 5.3) so that each successful insert is followed by an outbox publish:

```go
func (h *Handler) handleFirstThreadReply(ctx context.Context, msg *model.Message, threadRoomID string, replier *model.User, now time.Time) error {
	parentSender, err := h.store.GetMessageSender(ctx, msg.ThreadParentMessageID)
	if err != nil {
		if errors.Is(err, errMessageNotFound) {
			slog.Warn("thread reply parent not found — skipping subscription creation",
				"parentMessageID", msg.ThreadParentMessageID,
				"replyID", msg.ID)
			return nil
		}
		return fmt.Errorf("get parent message sender: %w", err)
	}

	parentSiteID, err := h.lookupOwnerSiteID(ctx, parentSender.ID, "first-reply parent")
	if err != nil {
		return fmt.Errorf("lookup parent owner site: %w", err)
	}
	if parentSiteID != "" {
		parentSub := h.buildThreadSubscription(msg, threadRoomID, parentSender.ID, parentSender.Account, parentSiteID, now)
		if err := h.threadStore.InsertThreadSubscription(ctx, parentSub); err != nil {
			return fmt.Errorf("insert parent author thread subscription: %w", err)
		}
		if err := h.publishThreadSubOutboxIfRemote(ctx, parentSub, msg.ID); err != nil {
			return err
		}
	}

	if replier != nil && msg.UserID != parentSender.ID {
		replierSub := h.buildThreadSubscription(msg, threadRoomID, msg.UserID, msg.UserAccount, replier.SiteID, now)
		if err := h.threadStore.InsertThreadSubscription(ctx, replierSub); err != nil {
			return fmt.Errorf("insert replier thread subscription: %w", err)
		}
		if err := h.publishThreadSubOutboxIfRemote(ctx, replierSub, msg.ID); err != nil {
			return err
		}
	}

	return nil
}
```

- [ ] **Step 6.4: Run tests to verify they pass**

Run: `make test SERVICE=message-worker`

Expected: pass — new tests green and existing tests still green (existing fixtures use `site-a` everywhere so no remote publishes fire).

- [ ] **Step 6.5: Commit**

```bash
git add message-worker/handler.go message-worker/handler_test.go
git commit -m "message-worker: emit outbox event on first-reply ThreadSubscription inserts"
```

---

## Task 7: Wire outbox publish into `handleSubsequentThreadReply`

Same pattern as Task 6 but on the upsert path. Subsequent replies always upsert both subscriptions (idempotent on redelivery), so we publish after each successful upsert.

**Files:**
- Modify: `message-worker/handler.go` (handleSubsequentThreadReply body)
- Modify: `message-worker/handler_test.go` (new test cases)

- [ ] **Step 7.1: Write the failing test for subsequent-reply remote publish**

Append to `message-worker/handler_test.go`:

```go
func TestHandler_SubsequentReply_OutboxPublishes(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	parentSender := &cassParticipant{ID: "u-parent", Account: "parent-user"}
	parentUserAtA := &model.User{ID: "u-parent", Account: "parent-user", SiteID: "site-a"}
	parentUserAtC := &model.User{ID: "u-parent", Account: "parent-user", SiteID: "site-c"}

	tests := []struct {
		name              string
		replierSite       string
		parentUser        *model.User
		wantPublishToSite map[string]int
	}{
		{
			name:              "both local — no publish",
			replierSite:       "site-a",
			parentUser:        parentUserAtA,
			wantPublishToSite: map[string]int{},
		},
		{
			name:              "replier remote — one publish",
			replierSite:       "site-b",
			parentUser:        parentUserAtA,
			wantPublishToSite: map[string]int{"site-b": 1},
		},
		{
			name:              "parent remote — one publish",
			replierSite:       "site-a",
			parentUser:        parentUserAtC,
			wantPublishToSite: map[string]int{"site-c": 1},
		},
		{
			name:              "both remote, different sites — two publishes",
			replierSite:       "site-b",
			parentUser:        parentUserAtC,
			wantPublishToSite: map[string]int{"site-b": 1, "site-c": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := NewMockStore(ctrl)
			us := NewMockUserStore(ctrl)
			ts := NewMockThreadStore(ctrl)

			ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
				Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
			store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").Return(parentSender, nil)
			us.EXPECT().FindUserByID(gomock.Any(), "u-parent").Return(tt.parentUser, nil)
			ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
			ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
			ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-existing", "msg-reply", now).Return(nil)

			var publishedDests []string
			h := NewHandler(store, us, ts, "site-a", func(_ context.Context, _ string, data []byte, _ string) error {
				var outer model.OutboxEvent
				if err := json.Unmarshal(data, &outer); err != nil {
					return err
				}
				publishedDests = append(publishedDests, outer.DestSiteID)
				return nil
			})

			replier := &model.User{ID: "u-replier", Account: "replier", SiteID: tt.replierSite}
			msg := &model.Message{
				ID:                    "msg-reply",
				RoomID:                "r1",
				UserID:                "u-replier",
				UserAccount:           "replier",
				CreatedAt:             now,
				ThreadParentMessageID: "msg-parent",
			}

			roomID, err := h.handleSubsequentThreadReply(context.Background(), msg, replier, now)
			require.NoError(t, err)
			assert.Equal(t, "tr-existing", roomID)

			gotByDest := map[string]int{}
			for _, d := range publishedDests {
				gotByDest[d]++
			}
			assert.Equal(t, tt.wantPublishToSite, gotByDest)
		})
	}
}

func TestHandler_SubsequentReply_OutboxPublishError_NAKs(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	us := NewMockUserStore(ctrl)
	ts := NewMockThreadStore(ctrl)

	ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
		Return(&model.ThreadRoom{ID: "tr-1"}, nil)
	store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
		Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
	us.EXPECT().FindUserByID(gomock.Any(), "u-parent").
		Return(&model.User{ID: "u-parent", SiteID: "site-c"}, nil)
	ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)

	boom := errors.New("publish boom")
	h := NewHandler(store, us, ts, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error {
		return boom
	})

	msg := &model.Message{
		ID: "msg-reply", RoomID: "r1", UserID: "u-replier", UserAccount: "replier",
		CreatedAt: now, ThreadParentMessageID: "msg-parent",
	}
	_, err := h.handleSubsequentThreadReply(context.Background(), msg,
		&model.User{ID: "u-replier", SiteID: "site-b"}, now)
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}
```

- [ ] **Step 7.2: Run tests to verify they fail**

Run: `make test SERVICE=message-worker -run TestHandler_SubsequentReply_Outbox`

Expected: FAIL — no publish happens yet on the upsert path.

- [ ] **Step 7.3: Wire the publish call into `handleSubsequentThreadReply`**

Replace `handleSubsequentThreadReply` in `message-worker/handler.go` with:

```go
func (h *Handler) handleSubsequentThreadReply(ctx context.Context, msg *model.Message, replier *model.User, now time.Time) (string, error) {
	existingRoom, err := h.threadStore.GetThreadRoomByParentMessageID(ctx, msg.ThreadParentMessageID)
	if err != nil {
		return "", fmt.Errorf("get existing thread room: %w", err)
	}

	parentSender, err := h.store.GetMessageSender(ctx, msg.ThreadParentMessageID)
	switch {
	case err == nil:
		parentSiteID, lookupErr := h.lookupOwnerSiteID(ctx, parentSender.ID, "subsequent-reply parent")
		if lookupErr != nil {
			return "", fmt.Errorf("lookup parent owner site: %w", lookupErr)
		}
		if parentSiteID != "" {
			parentSub := h.buildThreadSubscription(msg, existingRoom.ID, parentSender.ID, parentSender.Account, parentSiteID, now)
			if err := h.threadStore.UpsertThreadSubscription(ctx, parentSub); err != nil {
				return "", fmt.Errorf("upsert parent author thread subscription: %w", err)
			}
			if err := h.publishThreadSubOutboxIfRemote(ctx, parentSub, msg.ID); err != nil {
				return "", err
			}
		}
		if replier != nil && msg.UserID != parentSender.ID {
			replierSub := h.buildThreadSubscription(msg, existingRoom.ID, msg.UserID, msg.UserAccount, replier.SiteID, now)
			if err := h.threadStore.UpsertThreadSubscription(ctx, replierSub); err != nil {
				return "", fmt.Errorf("upsert replier thread subscription: %w", err)
			}
			if err := h.publishThreadSubOutboxIfRemote(ctx, replierSub, msg.ID); err != nil {
				return "", err
			}
		}
	case errors.Is(err, errMessageNotFound):
		slog.Warn("thread reply parent not found — skipping parent subscription upsert",
			"parentMessageID", msg.ThreadParentMessageID,
			"replyID", msg.ID)
		if replier != nil {
			replierSub := h.buildThreadSubscription(msg, existingRoom.ID, msg.UserID, msg.UserAccount, replier.SiteID, now)
			if err := h.threadStore.UpsertThreadSubscription(ctx, replierSub); err != nil {
				return "", fmt.Errorf("upsert replier thread subscription: %w", err)
			}
			if err := h.publishThreadSubOutboxIfRemote(ctx, replierSub, msg.ID); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("get parent message sender: %w", err)
	}

	if err := h.threadStore.UpdateThreadRoomLastMessage(ctx, existingRoom.ID, msg.ID, now); err != nil {
		return "", fmt.Errorf("update thread room last message: %w", err)
	}

	return existingRoom.ID, nil
}
```

- [ ] **Step 7.4: Run tests to verify they pass**

Run: `make test SERVICE=message-worker`

Expected: pass.

- [ ] **Step 7.5: Commit**

```bash
git add message-worker/handler.go message-worker/handler_test.go
git commit -m "message-worker: emit outbox event on subsequent-reply ThreadSubscription upserts"
```

---

## Task 8: Wire outbox publish into `markThreadMentions`

After `MarkThreadSubscriptionMention` succeeds for a remote mentionee, publish the outbox event so the mentionee's home site upserts a `hasMention=true` subscription. Local mentionees: no publish.

**Files:**
- Modify: `message-worker/handler.go` (markThreadMentions body)
- Modify: `message-worker/handler_test.go` (new test cases)

- [ ] **Step 8.1: Write the failing test for mention publish**

Append to `message-worker/handler_test.go`:

```go
func TestHandler_MarkThreadMentions_OutboxPublishes(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name              string
		mentionees        []model.Participant
		wantPublishToSite map[string]int
	}{
		{
			name:              "no mentions — no publish",
			mentionees:        nil,
			wantPublishToSite: map[string]int{},
		},
		{
			name: "local mentionee — mark only, no publish",
			mentionees: []model.Participant{
				{UserID: "u-bob", Account: "bob", SiteID: "site-a"},
			},
			wantPublishToSite: map[string]int{},
		},
		{
			name: "remote mentionee — mark and publish",
			mentionees: []model.Participant{
				{UserID: "u-bob", Account: "bob", SiteID: "site-b"},
			},
			wantPublishToSite: map[string]int{"site-b": 1},
		},
		{
			name: "two remote mentionees in different sites — two publishes",
			mentionees: []model.Participant{
				{UserID: "u-bob", Account: "bob", SiteID: "site-b"},
				{UserID: "u-carol", Account: "carol", SiteID: "site-c"},
			},
			wantPublishToSite: map[string]int{"site-b": 1, "site-c": 1},
		},
		{
			name: "@all is skipped — no mark, no publish",
			mentionees: []model.Participant{
				{Account: "all", EngName: "all"},
			},
			wantPublishToSite: map[string]int{},
		},
		{
			name: "sender self-mention is skipped",
			mentionees: []model.Participant{
				{UserID: "u-sender", Account: "sender", SiteID: "site-b"},
			},
			wantPublishToSite: map[string]int{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			ts := NewMockThreadStore(ctrl)

			expectedMarks := 0
			for _, p := range tt.mentionees {
				if p.Account == "all" {
					continue
				}
				if p.UserID == "u-sender" {
					continue
				}
				expectedMarks++
			}
			ts.EXPECT().MarkThreadSubscriptionMention(gomock.Any(), gomock.Any()).
				Times(expectedMarks).Return(nil)

			var publishedDests []string
			h := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), ts, "site-a",
				func(_ context.Context, _ string, data []byte, _ string) error {
					var outer model.OutboxEvent
					if err := json.Unmarshal(data, &outer); err != nil {
						return err
					}
					publishedDests = append(publishedDests, outer.DestSiteID)
					return nil
				})

			msg := &model.Message{
				ID:                    "msg-reply",
				RoomID:                "r1",
				UserID:                "u-sender",
				UserAccount:           "sender",
				CreatedAt:             now,
				ThreadParentMessageID: "msg-parent",
				Mentions:              tt.mentionees,
			}
			err := h.markThreadMentions(context.Background(), msg, "tr-1", "site-a")
			require.NoError(t, err)

			gotByDest := map[string]int{}
			for _, d := range publishedDests {
				gotByDest[d]++
			}
			assert.Equal(t, tt.wantPublishToSite, gotByDest)
		})
	}
}

func TestHandler_MarkThreadMentions_OutboxPublishError_NAKs(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ctrl := gomock.NewController(t)
	ts := NewMockThreadStore(ctrl)
	ts.EXPECT().MarkThreadSubscriptionMention(gomock.Any(), gomock.Any()).Return(nil)

	boom := errors.New("publish boom")
	h := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), ts, "site-a",
		func(_ context.Context, _ string, _ []byte, _ string) error { return boom })

	msg := &model.Message{
		ID: "msg-reply", RoomID: "r1", UserID: "u-sender", UserAccount: "sender",
		CreatedAt: now, ThreadParentMessageID: "msg-parent",
		Mentions: []model.Participant{{UserID: "u-bob", Account: "bob", SiteID: "site-b"}},
	}
	err := h.markThreadMentions(context.Background(), msg, "tr-1", "site-a")
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}

func TestHandler_MarkThreadMentions_HasMentionInPayload(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ctrl := gomock.NewController(t)
	ts := NewMockThreadStore(ctrl)
	ts.EXPECT().MarkThreadSubscriptionMention(gomock.Any(), gomock.Any()).Return(nil)

	var captured []byte
	h := NewHandler(NewMockStore(ctrl), NewMockUserStore(ctrl), ts, "site-a",
		func(_ context.Context, _ string, data []byte, _ string) error {
			captured = data
			return nil
		})

	msg := &model.Message{
		ID: "msg-reply", RoomID: "r1", UserID: "u-sender", UserAccount: "sender",
		CreatedAt: now, ThreadParentMessageID: "msg-parent",
		Mentions: []model.Participant{{UserID: "u-bob", Account: "bob", SiteID: "site-b"}},
	}
	require.NoError(t, h.markThreadMentions(context.Background(), msg, "tr-1", "site-a"))

	var outer model.OutboxEvent
	require.NoError(t, json.Unmarshal(captured, &outer))
	var sub model.ThreadSubscription
	require.NoError(t, json.Unmarshal(outer.Payload, &sub))
	assert.True(t, sub.HasMention, "outbox-emitted ThreadSubscription must carry HasMention=true")
	assert.Equal(t, "u-bob", sub.UserID)
	assert.Equal(t, "site-b", sub.SiteID)
}
```

- [ ] **Step 8.2: Run tests to verify they fail**

Run: `make test SERVICE=message-worker -run TestHandler_MarkThreadMentions_Outbox`

Expected: FAIL — `markThreadMentions` doesn't publish yet.

- [ ] **Step 8.3: Wire the publish call into `markThreadMentions`**

Replace `markThreadMentions` in `message-worker/handler.go` with:

```go
func (h *Handler) markThreadMentions(ctx context.Context, msg *model.Message, threadRoomID, eventSiteID string) error {
	for i := range msg.Mentions {
		p := &msg.Mentions[i]
		if p.Account == "all" {
			continue
		}
		if p.UserID == msg.UserID {
			continue
		}
		ownerSiteID := p.SiteID
		if ownerSiteID == "" {
			slog.Warn("mentionee participant has empty SiteID, falling back to event site",
				"account", p.Account, "userID", p.UserID, "msgID", msg.ID)
			ownerSiteID = eventSiteID
		}
		sub := h.buildThreadSubscription(msg, threadRoomID, p.UserID, p.Account, ownerSiteID, msg.CreatedAt)
		sub.HasMention = true
		if err := h.threadStore.MarkThreadSubscriptionMention(ctx, sub); err != nil {
			return fmt.Errorf("mark thread subscription mention for user %s: %w", p.UserID, err)
		}
		if err := h.publishThreadSubOutboxIfRemote(ctx, sub, msg.ID); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 8.4: Run tests to verify they pass**

Run: `make test SERVICE=message-worker`

Expected: pass.

- [ ] **Step 8.5: Commit**

```bash
git add message-worker/handler.go message-worker/handler_test.go
git commit -m "message-worker: emit outbox event on mention-marked ThreadSubscriptions"
```

---

## Task 9: Wire JetStream publish closure in `message-worker/main.go`

Task 3 plumbed `siteID` and a no-op publish closure into the test fixtures. The `main.go` change in Step 3.3 already added the JetStream-backed closure. This task verifies the wiring end-to-end and confirms `bootstrap.go` is unchanged for OUTBOX (per spec — ops/IaC owns OUTBOX).

**Files:**
- Verify: `message-worker/main.go:95-105`
- Verify: `message-worker/bootstrap.go` (no changes)
- Modify: `message-worker/main.go` (only if Step 3.3 wasn't applied)

- [ ] **Step 9.1: Confirm `main.go` JetStream closure is in place**

Read `message-worker/main.go` and verify the `NewHandler` call passes the publish closure exactly as below:

```go
	handler := NewHandler(store, us, threadStore, cfg.SiteID, func(ctx context.Context, subj string, data []byte, msgID string) error {
		if msgID == "" {
			return nc.Publish(ctx, subj, data)
		}
		_, err := js.Publish(ctx, subj, data, jetstream.WithMsgID(msgID))
		return err
	})
```

If missing or different, apply Step 3.3 from Task 3 verbatim.

- [ ] **Step 9.2: Confirm `bootstrap.go` is unchanged for OUTBOX**

Read `message-worker/bootstrap.go` and verify the only stream it bootstraps is `MESSAGES_CANONICAL`. There must be NO reference to `stream.Outbox(...)` or `OUTBOX_`.

Per the spec ("Stream ownership"), OUTBOX is owned by ops/IaC. Adding it here would diverge from room-worker's pattern. Leave the file alone.

- [ ] **Step 9.3: Build the binary**

Run: `make build SERVICE=message-worker`

Expected: succeeds. (Confirms imports and types compile end-to-end with the new publish closure.)

- [ ] **Step 9.4: Run lint + full unit suite**

Run: `make lint && make test`

Expected: full repo passes.

- [ ] **Step 9.5: Commit (if Step 9.1 made any change)**

If Step 9.1 was a no-op (Task 3 already covered it), skip the commit. Otherwise:

```bash
git add message-worker/main.go
git commit -m "message-worker: wire JetStream publish closure with Nats-Msg-Id"
```

---

## Task 10: Inbox-worker dispatch case for `thread_subscription_upserted`

Inbox-worker needs to recognize the new event type, decode the payload, and call a new store method. This task adds the interface method, the dispatch case, the handler function, and unit-test coverage. The Mongo implementation comes in Task 11.

**Files:**
- Modify: `inbox-worker/handler.go` (extend `InboxStore` interface; add dispatch case + handler)
- Modify: `inbox-worker/handler_test.go` (extend `stubInboxStore`; new dispatch tests)

- [ ] **Step 10.1: Extend the `InboxStore` interface**

In `inbox-worker/handler.go`, add `UpsertThreadSubscription` to the interface:

```go
// InboxStore abstracts the data store operations needed by the inbox worker.
type InboxStore interface {
	CreateSubscription(ctx context.Context, sub *model.Subscription) error
	BulkCreateSubscriptions(ctx context.Context, subs []*model.Subscription) error
	UpsertRoom(ctx context.Context, room *model.Room) error
	UpdateSubscriptionRoles(ctx context.Context, account, roomID string, roles []model.Role) error
	DeleteSubscriptionsByAccounts(ctx context.Context, roomID string, accounts []string) error
	FindUsersByAccounts(ctx context.Context, accounts []string) ([]model.User, error)
	UpsertThreadSubscription(ctx context.Context, sub *model.ThreadSubscription) error
}
```

- [ ] **Step 10.2: Extend `stubInboxStore` with thread-subscription support**

In `inbox-worker/handler_test.go`, extend `stubInboxStore`:

Add a field to the struct (after `users`):

```go
	threadSubs []model.ThreadSubscription
```

Add a method (anywhere among the other stub methods):

```go
func (s *stubInboxStore) UpsertThreadSubscription(_ context.Context, sub *model.ThreadSubscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.threadSubs {
		if s.threadSubs[i].ThreadRoomID == sub.ThreadRoomID && s.threadSubs[i].UserID == sub.UserID {
			// Monotonic hasMention merge — never clear true→false.
			if sub.HasMention {
				s.threadSubs[i].HasMention = true
			}
			s.threadSubs[i].UpdatedAt = sub.UpdatedAt
			return nil
		}
	}
	s.threadSubs = append(s.threadSubs, *sub)
	return nil
}

func (s *stubInboxStore) getThreadSubs() []model.ThreadSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]model.ThreadSubscription, len(s.threadSubs))
	copy(cp, s.threadSubs)
	return cp
}
```

- [ ] **Step 10.3: Write the failing dispatch tests**

Append to `inbox-worker/handler_test.go`:

```go
func TestHandleEvent_ThreadSubscriptionUpserted_Insert(t *testing.T) {
	store := &stubInboxStore{}
	pub := &mockPublisher{}
	h := NewHandler(store, pub)

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	sub := model.ThreadSubscription{
		ID:              "sub-1",
		ParentMessageID: "pm-1",
		RoomID:          "r1",
		ThreadRoomID:    "tr-1",
		UserID:          "u-bob",
		UserAccount:     "bob",
		SiteID:          "site-b",
		HasMention:      false,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	subData, err := json.Marshal(sub)
	require.NoError(t, err)

	evt := model.OutboxEvent{
		Type:       "thread_subscription_upserted",
		SiteID:     "site-a",
		DestSiteID: "site-b",
		Payload:    subData,
		Timestamp:  now.UnixMilli(),
	}
	evtData, _ := json.Marshal(evt)

	require.NoError(t, h.HandleEvent(context.Background(), evtData))

	got := store.getThreadSubs()
	require.Len(t, got, 1)
	assert.Equal(t, sub, got[0])
	assert.Empty(t, pub.getRecords(), "no client-facing publish on thread sub upsert")
}

func TestHandleEvent_ThreadSubscriptionUpserted_MonotonicHasMention(t *testing.T) {
	store := &stubInboxStore{}
	pub := &mockPublisher{}
	h := NewHandler(store, pub)

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	mentionSub := model.ThreadSubscription{
		ID: "sub-1", ParentMessageID: "pm-1", RoomID: "r1", ThreadRoomID: "tr-1",
		UserID: "u-bob", UserAccount: "bob", SiteID: "site-b",
		HasMention: true, CreatedAt: now, UpdatedAt: now,
	}
	mentionData, _ := json.Marshal(mentionSub)
	mentionEvt, _ := json.Marshal(model.OutboxEvent{
		Type: "thread_subscription_upserted", SiteID: "site-a", DestSiteID: "site-b",
		Payload: mentionData, Timestamp: now.UnixMilli(),
	})
	require.NoError(t, h.HandleEvent(context.Background(), mentionEvt))

	// Second event for same (threadRoomID, userID) with HasMention=false must NOT clear it.
	plainSub := mentionSub
	plainSub.HasMention = false
	plainSub.UpdatedAt = now.Add(time.Minute)
	plainData, _ := json.Marshal(plainSub)
	plainEvt, _ := json.Marshal(model.OutboxEvent{
		Type: "thread_subscription_upserted", SiteID: "site-a", DestSiteID: "site-b",
		Payload: plainData, Timestamp: plainSub.UpdatedAt.UnixMilli(),
	})
	require.NoError(t, h.HandleEvent(context.Background(), plainEvt))

	got := store.getThreadSubs()
	require.Len(t, got, 1)
	assert.True(t, got[0].HasMention, "hasMention must remain true after a non-mention event")
}

func TestHandleEvent_ThreadSubscriptionUpserted_InvalidPayload(t *testing.T) {
	store := &stubInboxStore{}
	pub := &mockPublisher{}
	h := NewHandler(store, pub)

	evt := model.OutboxEvent{
		Type: "thread_subscription_upserted", SiteID: "site-a", DestSiteID: "site-b",
		Payload: []byte("not json"),
	}
	evtData, _ := json.Marshal(evt)

	require.Error(t, h.HandleEvent(context.Background(), evtData))
	assert.Empty(t, store.getThreadSubs())
}

func TestHandleEvent_ThreadSubscriptionUpserted_StoreError(t *testing.T) {
	store := &errorThreadSubStore{stubInboxStore: &stubInboxStore{}}
	pub := &mockPublisher{}
	h := NewHandler(store, pub)

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	sub := model.ThreadSubscription{
		ID: "sub-1", ThreadRoomID: "tr-1", UserID: "u-bob", SiteID: "site-b",
		CreatedAt: now, UpdatedAt: now,
	}
	subData, _ := json.Marshal(sub)
	evtData, _ := json.Marshal(model.OutboxEvent{
		Type: "thread_subscription_upserted", SiteID: "site-a", DestSiteID: "site-b",
		Payload: subData, Timestamp: now.UnixMilli(),
	})

	err := h.HandleEvent(context.Background(), evtData)
	require.Error(t, err)
}

type errorThreadSubStore struct {
	*stubInboxStore
}

func (s *errorThreadSubStore) UpsertThreadSubscription(_ context.Context, _ *model.ThreadSubscription) error {
	return fmt.Errorf("boom")
}
```

- [ ] **Step 10.4: Run tests to verify they fail**

Run: `make test SERVICE=inbox-worker -run TestHandleEvent_ThreadSubscriptionUpserted`

Expected: compile error or unknown-event-type warn (handler.go has no case for the new type yet).

- [ ] **Step 10.5: Add the dispatch case + handler**

In `inbox-worker/handler.go`, add a new case in the `switch evt.Type` block of `HandleEvent`:

```go
	case "thread_subscription_upserted":
		return h.handleThreadSubscriptionUpserted(ctx, &evt)
```

Then append the handler function (after `handleRoleUpdated`):

```go
// handleThreadSubscriptionUpserted upserts a ThreadSubscription on the local
// site when message-worker on another site reports that a user (parent author,
// replier, or mentionee) is participating in a thread. The Mongo store layer
// is responsible for the monotonic hasMention merge — see store impl.
func (h *Handler) handleThreadSubscriptionUpserted(ctx context.Context, evt *model.OutboxEvent) error {
	var sub model.ThreadSubscription
	if err := json.Unmarshal(evt.Payload, &sub); err != nil {
		return fmt.Errorf("unmarshal thread_subscription_upserted payload: %w", err)
	}
	if err := h.store.UpsertThreadSubscription(ctx, &sub); err != nil {
		return fmt.Errorf("upsert thread subscription (threadRoomID %q, userID %q): %w",
			sub.ThreadRoomID, sub.UserID, err)
	}
	return nil
}
```

- [ ] **Step 10.6: Run tests to verify they pass**

Run: `make test SERVICE=inbox-worker`

Expected: pass.

- [ ] **Step 10.7: Commit**

```bash
git add inbox-worker/handler.go inbox-worker/handler_test.go
git commit -m "inbox-worker: dispatch thread_subscription_upserted to UpsertThreadSubscription"
```

---

## Task 11: Mongo `UpsertThreadSubscription` implementation in inbox-worker

Concrete implementation backing the interface added in Task 10. Uses `$setOnInsert` for first-event fields, `$set` for `updatedAt`, and `$max` on `hasMention` for monotonic merge. Includes ensuring the `(threadRoomId, userId)` unique index exists at startup.

**Files:**
- Modify: `inbox-worker/main.go` (add `threadSubCol` field; add `UpsertThreadSubscription` method; ensure index at startup)

- [ ] **Step 11.1: Add `threadSubCol` to `mongoInboxStore` and wire it in `main`**

In `inbox-worker/main.go`, replace the `mongoInboxStore` struct definition with:

```go
// mongoInboxStore implements InboxStore using MongoDB.
type mongoInboxStore struct {
	subCol       *mongo.Collection
	roomCol      *mongo.Collection
	userCol      *mongo.Collection
	threadSubCol *mongo.Collection
}
```

In `main()`, replace the store construction (around the existing `&mongoInboxStore{...}` literal) with:

```go
	store := &mongoInboxStore{
		subCol:       db.Collection("subscriptions"),
		roomCol:      db.Collection("rooms"),
		userCol:      db.Collection("users"),
		threadSubCol: db.Collection("threadSubscriptions"),
	}
	if err := store.ensureIndexes(ctx); err != nil {
		slog.Error("ensure indexes failed", "error", err)
		os.Exit(1)
	}
```

- [ ] **Step 11.2: Add the `ensureIndexes` and `UpsertThreadSubscription` methods**

Append to `inbox-worker/main.go` (after the existing `BulkCreateSubscriptions` method, before `func main()`):

```go
// ensureIndexes creates the unique index on (threadRoomId, userId) used by
// UpsertThreadSubscription. The index name and shape match what message-worker
// creates in its own threadStoreMongo so both services agree on the natural
// key for thread subscriptions.
func (s *mongoInboxStore) ensureIndexes(ctx context.Context) error {
	if _, err := s.threadSubCol.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "threadRoomId", Value: 1}, {Key: "userId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return fmt.Errorf("ensure threadSubscriptions (threadRoomId,userId) index: %w", err)
	}
	return nil
}

// UpsertThreadSubscription inserts the subscription on first event for a
// (threadRoomId, userId) pair, and on subsequent events updates only
// updatedAt and (monotonically) hasMention. $setOnInsert pins the immutable
// fields on insert; $set always refreshes updatedAt; $max on hasMention
// guarantees a non-mention event never clears a prior mention=true.
//
// $setOnInsert and $max operate on disjoint fields (hasMention is set by $max
// only — never by $setOnInsert) so MongoDB doesn't reject the update with a
// "conflicting update operators" error.
func (s *mongoInboxStore) UpsertThreadSubscription(ctx context.Context, sub *model.ThreadSubscription) error {
	filter := bson.M{"threadRoomId": sub.ThreadRoomID, "userId": sub.UserID}
	update := bson.M{
		"$setOnInsert": bson.M{
			"_id":             sub.ID,
			"parentMessageId": sub.ParentMessageID,
			"roomId":          sub.RoomID,
			"threadRoomId":    sub.ThreadRoomID,
			"userId":          sub.UserID,
			"userAccount":     sub.UserAccount,
			"siteId":          sub.SiteID,
			"lastSeenAt":      sub.LastSeenAt,
			"createdAt":       sub.CreatedAt,
		},
		"$set": bson.M{"updatedAt": sub.UpdatedAt},
		"$max": bson.M{"hasMention": sub.HasMention},
	}
	if _, err := s.threadSubCol.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true)); err != nil {
		return fmt.Errorf("upsert thread subscription (threadRoomID %q, userID %q): %w",
			sub.ThreadRoomID, sub.UserID, err)
	}
	return nil
}
```

- [ ] **Step 11.3: Build and run unit tests**

Run: `make build SERVICE=inbox-worker && make test SERVICE=inbox-worker`

Expected: both pass. (The unit tests use the in-memory stub from Task 10; this task's Mongo code only matters in integration tests, added in Task 12.)

- [ ] **Step 11.4: Commit**

```bash
git add inbox-worker/main.go
git commit -m "inbox-worker: Mongo UpsertThreadSubscription with monotonic hasMention merge"
```

---

## Task 12: Integration tests for inbox-worker `UpsertThreadSubscription`

Two scenarios in real MongoDB via testcontainers: insert from empty, and monotonic-mention merge across two events.

**Files:**
- Modify: `inbox-worker/integration_test.go` (append two test functions)

- [ ] **Step 12.1: Append the integration tests**

Add to `inbox-worker/integration_test.go`:

```go
func TestInboxWorker_ThreadSubscriptionUpserted_Insert_Integration(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()

	store := &mongoInboxStore{
		subCol:       db.Collection("subscriptions"),
		roomCol:      db.Collection("rooms"),
		userCol:      db.Collection("users"),
		threadSubCol: db.Collection("threadSubscriptions"),
	}
	require.NoError(t, store.ensureIndexes(ctx))

	pub := &recordingPublisher{}
	handler := NewHandler(store, pub)

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	sub := model.ThreadSubscription{
		ID: "sub-1", ParentMessageID: "pm-1", RoomID: "r1", ThreadRoomID: "tr-1",
		UserID: "u-bob", UserAccount: "bob", SiteID: "site-b",
		HasMention: false, CreatedAt: now, UpdatedAt: now,
	}
	subData, _ := json.Marshal(sub)
	evtData, _ := json.Marshal(model.OutboxEvent{
		Type: "thread_subscription_upserted", SiteID: "site-a", DestSiteID: "site-b",
		Payload: subData, Timestamp: now.UnixMilli(),
	})

	require.NoError(t, handler.HandleEvent(ctx, evtData))

	var got model.ThreadSubscription
	err := db.Collection("threadSubscriptions").
		FindOne(ctx, bson.M{"threadRoomId": "tr-1", "userId": "u-bob"}).
		Decode(&got)
	require.NoError(t, err)
	assert.Equal(t, "sub-1", got.ID)
	assert.Equal(t, "site-b", got.SiteID)
	assert.False(t, got.HasMention)
	assert.True(t, got.CreatedAt.Equal(now))
	assert.True(t, got.UpdatedAt.Equal(now))

	// No client publishes for thread subscription upserts.
	assert.Empty(t, pub.subjects)
}

func TestInboxWorker_ThreadSubscriptionUpserted_MonotonicMention_Integration(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()

	store := &mongoInboxStore{
		subCol:       db.Collection("subscriptions"),
		roomCol:      db.Collection("rooms"),
		userCol:      db.Collection("users"),
		threadSubCol: db.Collection("threadSubscriptions"),
	}
	require.NoError(t, store.ensureIndexes(ctx))

	handler := NewHandler(store, &recordingPublisher{})
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	// First event: HasMention=true.
	mentionSub := model.ThreadSubscription{
		ID: "sub-1", ParentMessageID: "pm-1", RoomID: "r1", ThreadRoomID: "tr-1",
		UserID: "u-bob", UserAccount: "bob", SiteID: "site-b",
		HasMention: true, CreatedAt: now, UpdatedAt: now,
	}
	mentionData, _ := json.Marshal(mentionSub)
	mentionEvt, _ := json.Marshal(model.OutboxEvent{
		Type: "thread_subscription_upserted", SiteID: "site-a", DestSiteID: "site-b",
		Payload: mentionData, Timestamp: now.UnixMilli(),
	})
	require.NoError(t, handler.HandleEvent(ctx, mentionEvt))

	// Second event: HasMention=false (later updatedAt). Must NOT clear the flag.
	plainSub := mentionSub
	plainSub.HasMention = false
	later := now.Add(time.Minute)
	plainSub.UpdatedAt = later
	plainData, _ := json.Marshal(plainSub)
	plainEvt, _ := json.Marshal(model.OutboxEvent{
		Type: "thread_subscription_upserted", SiteID: "site-a", DestSiteID: "site-b",
		Payload: plainData, Timestamp: later.UnixMilli(),
	})
	require.NoError(t, handler.HandleEvent(ctx, plainEvt))

	var got model.ThreadSubscription
	err := db.Collection("threadSubscriptions").
		FindOne(ctx, bson.M{"threadRoomId": "tr-1", "userId": "u-bob"}).
		Decode(&got)
	require.NoError(t, err)
	assert.True(t, got.HasMention, "hasMention must remain true after a non-mention event")
	assert.True(t, got.UpdatedAt.Equal(later), "updatedAt must advance to the later event's value")
	// _id and createdAt come from $setOnInsert and must remain from the first event.
	assert.Equal(t, "sub-1", got.ID)
	assert.True(t, got.CreatedAt.Equal(now))

	// Third event: HasMention=true again. Idempotent — still true, updatedAt advances.
	thirdSub := plainSub
	thirdSub.HasMention = true
	evenLater := later.Add(time.Minute)
	thirdSub.UpdatedAt = evenLater
	thirdData, _ := json.Marshal(thirdSub)
	thirdEvt, _ := json.Marshal(model.OutboxEvent{
		Type: "thread_subscription_upserted", SiteID: "site-a", DestSiteID: "site-b",
		Payload: thirdData, Timestamp: evenLater.UnixMilli(),
	})
	require.NoError(t, handler.HandleEvent(ctx, thirdEvt))

	require.NoError(t, db.Collection("threadSubscriptions").
		FindOne(ctx, bson.M{"threadRoomId": "tr-1", "userId": "u-bob"}).
		Decode(&got))
	assert.True(t, got.HasMention)
	assert.True(t, got.UpdatedAt.Equal(evenLater))
}
```

- [ ] **Step 12.2: Run integration tests**

Run: `make test-integration SERVICE=inbox-worker`

Expected: both new tests pass alongside the existing ones.

- [ ] **Step 12.3: Run the full integration suite to confirm no regressions**

Run: `make test-integration`

Expected: full integration suite passes (or skips on unavailable Docker — investigate if anything new fails).

- [ ] **Step 12.4: Commit**

```bash
git add inbox-worker/integration_test.go
git commit -m "inbox-worker: integration tests for UpsertThreadSubscription monotonic merge"
```

---

## Spec coverage map

| Spec section / requirement | Task(s) |
|---|---|
| `OutboxThreadSubscriptionUpserted` constant | Task 1 |
| `Participant.SiteID` field, propagation from `mention.Resolve` | Task 2 |
| `Handler.publish PublishFunc` + `siteID` field | Task 3 |
| `publishThreadSubOutboxIfRemote` helper, dedup ID seed, same-site no-op, empty-SiteID guard, error propagation | Task 4 |
| `ThreadSubscription.SiteID` = owner's site (not room's) | Task 5 |
| Parent siteID lookup via `userStore.FindUserByID`, warn-and-skip on `ErrUserNotFound`, propagate other errors | Tasks 5, 6, 7 |
| Outbox publish on first reply (parent + replier) | Task 6 |
| Outbox publish on subsequent reply (parent + replier) | Task 7 |
| Outbox publish on thread mention (mentionees only, with `HasMention=true` payload) | Task 8 |
| Skip @all and sender self-mention at thread level | Task 8 |
| `main.go` JetStream publish closure with `Nats-Msg-Id` | Tasks 3, 9 |
| `bootstrap.go` unchanged for OUTBOX (ops/IaC ownership) | Task 9 |
| Inbox-worker dispatch case for `thread_subscription_upserted` | Task 10 |
| `InboxStore.UpsertThreadSubscription` interface method | Task 10 |
| Mongo `$setOnInsert` + `$set` + `$max` upsert with monotonic `hasMention` | Task 11 |
| `(threadRoomId, userId)` unique index in inbox-worker | Task 11 |
| Integration tests: insert path + monotonic mention merge | Task 12 |
| Failure semantics: NAK on outbox publish error → JetStream redelivery → idempotent dedup | Tasks 4, 6, 7, 8 |
| Migration / rollout note (deploy inbox-worker before message-worker) | covered by spec; no code task needed |
| Non-goals: ThreadRoom replication, client UI events, delete events, @all propagation | none — explicitly excluded by spec |

---

## Execution handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-28-message-worker-thread-subscription-outbox.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — Execute tasks in this session using `superpowers:executing-plans`, batch execution with checkpoints.

Which approach?

---

## Post-implementation correction (review feedback)

After Task 5 landed, PR review (mliu33) flagged that `ThreadSubscription.SiteID`
should follow the same semantic as `Subscription.SiteID` — **the room's home
site**, a back-reference preserved across federation — not the owner's home
site. This plan as written above (Task 5 in particular) describes the
incorrect owner-site semantic.

**Corrected design** is captured in the spec at
`docs/superpowers/specs/2026-04-28-message-worker-thread-subscription-outbox-design.md`
("`ThreadSubscription.SiteID` semantic" + "Outbox routing" sections).

**Correction commit:** `1c3915c` — "ThreadSubscription.SiteID stays as room
site; route by owner site separately". Summary of differences vs. the plan
above:

- `buildThreadSubscription` is called with the **room's** site at all three
  call sites; `SiteID` field on the persisted document is the room's site.
- `publishThreadSubOutboxIfRemote` gained an explicit `ownerSiteID` parameter
  used only for the cross-site routing decision (`DestSiteID` + same-site
  short-circuit). Owner-site is resolved transiently at processing time —
  `replier.SiteID`, `lookupOwnerSiteID(parentSender.ID)`, or `Participant.SiteID`
  — and never written onto the subscription.
- `handleFirstThreadReply` / `handleSubsequentThreadReply` gain `eventSiteID`
  arg; `markThreadMentions` no longer needs the empty-SiteID fallback (the
  field on the sub is always the room's site).
- Tests assert `sub.SiteID == roomSite` after round-trip, not `ownerSite`.

The historical plan above stays as-written for traceability of how the work
unfolded. New readers should treat the spec as the canonical design.
