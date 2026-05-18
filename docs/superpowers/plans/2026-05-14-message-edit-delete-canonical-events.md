# Message Edit/Delete Canonical Events Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring message edits and soft-deletes onto the existing `MESSAGES_CANONICAL_<siteID>` JetStream stream (subjects `.updated` and `.deleted`) so `search-sync-worker` keeps the ES index in sync and `broadcast-worker` fans out per-user live events that federate to remote-site clients via the existing per-user-account NATS bridge.

**Architecture:** History-service publishes `model.MessageEvent` (with `Event: EventUpdated` or `EventDeleted`) to the canonical stream after each successful Cassandra mutation. Message-worker adds a `FilterSubject: .created` so it stops at the create flow. Broadcast-worker grows two dispatch arms on the canonical stream and emits per-user `RoomEvent` with new types `RoomEventMessageEdited` / `RoomEventMessageDeleted`. Search-sync-worker requires **no code change** — its existing payload-driven dispatch (`buildMessageAction`) already maps `EventUpdated` → ES index replace and `EventDeleted` → ES delete.

**Tech Stack:** Go 1.25.9, NATS JetStream (`github.com/nats-io/nats.go/jetstream`), `pkg/natsrouter`, `pkg/subject`, `pkg/model`, `gocql` for Cassandra (via existing `history-service` writer), `searchengine.BulkAction` (via existing `search-sync-worker`), testify, mockgen.

**Spec:** `docs/superpowers/specs/2026-05-14-message-edit-delete-canonical-events-design.md`

**Branch:** `claude/search-message-editedat-updatedat` (already checked out — current HEAD `b64fd20`).

---

## File Structure

**Modified (per-phase):**

| Phase | Files |
|---|---|
| A. RoomEvent types | `pkg/model/event.go`, `pkg/model/model_test.go` |
| B. history-service produces canonical | `history-service/internal/service/messages.go`, `history-service/internal/service/messages_test.go` |
| C. message-worker filters | `message-worker/main.go`, `message-worker/main_test.go` (or wherever `buildConsumerConfig` is tested) |
| D. broadcast-worker dispatch | `broadcast-worker/handler.go`, `broadcast-worker/handler_test.go` |
| E. integration verification | (run existing `search-sync-worker/integration_test.go`; add new broadcast-worker integration test if missing) |
| F. PR description | GitHub PR #182 |

**No changes required:**
- `pkg/subject/` — `MsgCanonicalUpdated`, `MsgCanonicalDeleted`, `MsgCanonicalWildcard` all already exist.
- `search-sync-worker/` — `buildMessageAction` already dispatches by `evt.Event` (search-sync-worker/messages.go:114-138). `FilterSubjects` already returns `nil` (subscribes to full wildcard).
- `inbox-worker/` — message content does not flow cross-site via INBOX/OUTBOX (D12, D13).

---

## Pre-flight

- [ ] **Step 1: Confirm branch + clean tree**

Run:
```bash
git branch --show-current
git status
```
Expected: branch is `claude/search-message-editedat-updatedat`; working tree clean.

- [ ] **Step 2: Confirm baseline tests pass**

Run:
```bash
make test
```
Expected: all packages PASS (any failure indicates pre-existing trouble unrelated to this plan).

---

## Phase A — `pkg/model`: add RoomEvent variants

The `broadcast-worker` emissions need two new `RoomEvent.Type` constants and slim payload sub-structs that ride alongside the existing `RoomEvent` envelope.

### Task A1: Add `RoomEventMessageEdited` / `RoomEventMessageDeleted` constants + payload structs

**Files:**
- Modify: `pkg/model/event.go`
- Modify: `pkg/model/model_test.go`

- [ ] **Step 1: Locate the existing `RoomEvent` definition**

Run:
```bash
grep -n "RoomEventNewMessage\|type RoomEvent\b\|type RoomEventType" /home/user/chat/pkg/model/event.go
```
You should see `RoomEventType` enum (string-typed) and the `RoomEventNewMessage` constant in a `const (...)` block (~line 137). Read 30 lines around it so you understand the existing pattern (envelope `RoomEvent` struct, what fields it carries, how the `Type` discriminator works).

- [ ] **Step 2: Write failing round-trip tests**

Append to `pkg/model/model_test.go`:

```go
func TestRoomEventMessageEditedJSON(t *testing.T) {
	editedAt := time.Date(2026, 5, 14, 12, 5, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 5, 14, 12, 5, 0, 0, time.UTC)
	evt := model.RoomEvent{
		Type:   model.RoomEventMessageEdited,
		RoomID: "r1",
		MessageEdited: &model.MessageEditedPayload{
			MessageID:  "msg-uuid",
			NewContent: "hello (edited)",
			EditedBy:   "alice",
			EditedAt:   editedAt,
			UpdatedAt:  updatedAt,
		},
	}
	roundTrip(t, &evt, &model.RoomEvent{})
}

func TestRoomEventMessageDeletedJSON(t *testing.T) {
	deletedAt := time.Date(2026, 5, 14, 12, 10, 0, 0, time.UTC)
	evt := model.RoomEvent{
		Type:   model.RoomEventMessageDeleted,
		RoomID: "r1",
		MessageDeleted: &model.MessageDeletedPayload{
			MessageID: "msg-uuid",
			DeletedBy: "alice",
			DeletedAt: deletedAt,
			UpdatedAt: deletedAt,
		},
	}
	roundTrip(t, &evt, &model.RoomEvent{})
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run:
```bash
go test -race -run 'TestRoomEventMessage(Edited|Deleted)JSON' ./pkg/model/...
```
Expected: FAIL with `undefined: model.RoomEventMessageEdited`, `undefined: model.RoomEventMessageDeleted`, `undefined: model.MessageEditedPayload`, `undefined: model.MessageDeletedPayload`, and `RoomEvent.MessageEdited` / `RoomEvent.MessageDeleted` field references unknown.

- [ ] **Step 4: Add the constants and payload sub-types**

In `pkg/model/event.go`, find the `const (...)` block around line 137 with `RoomEventNewMessage RoomEventType = "new_message"`. Extend it:

```go
const (
	RoomEventNewMessage      RoomEventType = "new_message"
	RoomEventMessageEdited   RoomEventType = "message_edited"
	RoomEventMessageDeleted  RoomEventType = "message_deleted"
)
```

(If other RoomEventType constants exist in the same block, leave them in place — only add the two new ones, keeping the existing ordering.)

Then add the payload types near the other RoomEvent-related types in `event.go`:

```go
// MessageEditedPayload is the slim sub-payload carried by RoomEvent when
// Type == RoomEventMessageEdited. Generated by broadcast-worker from a
// canonical .updated event.
type MessageEditedPayload struct {
	MessageID  string    `json:"messageId"`
	NewContent string    `json:"newContent,omitempty"`
	EditedBy   string    `json:"editedBy"`
	EditedAt   time.Time `json:"editedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// MessageDeletedPayload is the slim sub-payload carried by RoomEvent when
// Type == RoomEventMessageDeleted. Generated by broadcast-worker from a
// canonical .deleted event.
type MessageDeletedPayload struct {
	MessageID string    `json:"messageId"`
	DeletedBy string    `json:"deletedBy"`
	DeletedAt time.Time `json:"deletedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
```

Then add the two new optional pointers to the `RoomEvent` struct. Locate the existing struct (search for `type RoomEvent struct` in the same file). After the existing fields, before the closing brace, add:

```go
	// MessageEdited is set when Type == RoomEventMessageEdited. Nil otherwise.
	MessageEdited *MessageEditedPayload `json:"messageEdited,omitempty"`
	// MessageDeleted is set when Type == RoomEventMessageDeleted. Nil otherwise.
	MessageDeleted *MessageDeletedPayload `json:"messageDeleted,omitempty"`
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test -race -run 'TestRoomEventMessage(Edited|Deleted)JSON' ./pkg/model/...
```
Expected: PASS.

Also run the full model package to confirm no regression in other RoomEvent tests:
```bash
go test -race ./pkg/model/...
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/model/event.go pkg/model/model_test.go
git commit -m "feat(model): add RoomEventMessageEdited / RoomEventMessageDeleted types"
```

---

## Phase B — `history-service`: publish canonical on edit/delete, drop direct room.event

History-service already does the synchronous Cassandra write. We add a best-effort canonical publish after each successful write and remove the existing direct `chat.room.<roomID>.event` publish (broadcast-worker takes over).

History-service's `EventPublisher` interface is already in place: `Publish(ctx, subject, data) error` (`history-service/internal/service/service.go:type EventPublisher`). `s.publisher` is the same publisher today for the live RoomEvent — reuse it for the canonical publish.

### Task B1: Wire `siteID` into the canonical publish for `EditMessage`

The `EditMessage` handler is registered as `MsgEditPattern(siteID)`. The `siteID` is currently in scope at `RegisterHandlers(r, siteID)` but NOT on `HistoryService` as a field. We need it inside `EditMessage`. Two options: (a) thread `siteID` onto `HistoryService` as a field; (b) extract from the subject token in the handler. Option (a) is cleaner — matches the `room-worker` pattern where `siteID` is on the `Handler` struct.

**Files:**
- Modify: `history-service/internal/service/service.go`
- Modify: `history-service/main.go`
- Modify: `history-service/internal/service/messages.go`
- Modify: `history-service/internal/service/messages_test.go`

- [ ] **Step 1: Read the current `HistoryService` struct + constructor signature**

```bash
grep -B2 -A20 "type HistoryService struct\|func NewHistoryService" /home/user/chat/history-service/internal/service/service.go
```
Note the existing fields and the constructor's parameter list. You'll add `siteID string` as a field and a constructor parameter.

- [ ] **Step 2: Add `siteID` to `HistoryService`**

Edit `history-service/internal/service/service.go`. In the `HistoryService` struct, add the field (place adjacent to other simple string/duration fields like `historyFloor`):

```go
	siteID string
```

In the constructor `NewHistoryService` (or whatever the existing name is — check the file), add `siteID string` as a parameter and assign it: `siteID: siteID,` in the struct literal.

In `RegisterHandlers(r *natsrouter.Router, siteID string)`, the parameter is now redundant if the field is set. Either:
- Keep both (caller passes siteID twice; redundant but explicit), OR
- Drop the `siteID` parameter from `RegisterHandlers` and use `s.siteID` for the subject builders.

Choose to **drop the redundant parameter** for cleanliness. Update `RegisterHandlers`:

```go
func (s *HistoryService) RegisterHandlers(r *natsrouter.Router) {
	natsrouter.Register(r, subject.MsgHistoryPattern(s.siteID), s.LoadHistory)
	natsrouter.Register(r, subject.MsgNextPattern(s.siteID), s.LoadNextMessages)
	natsrouter.Register(r, subject.MsgSurroundingPattern(s.siteID), s.LoadSurroundingMessages)
	natsrouter.Register(r, subject.MsgGetPattern(s.siteID), s.GetMessageByID)
	natsrouter.Register(r, subject.MsgEditPattern(s.siteID), s.EditMessage)
	natsrouter.Register(r, subject.MsgDeletePattern(s.siteID), s.DeleteMessage)
	natsrouter.Register(r, subject.MsgThreadPattern(s.siteID), s.GetThreadMessages)
	natsrouter.Register(r, subject.MsgThreadParentPattern(s.siteID), s.GetThreadParentMessages)
}
```

- [ ] **Step 3: Update `history-service/main.go` to pass `siteID` to the constructor**

```bash
grep -n "NewHistoryService\|RegisterHandlers\|cfg.SiteID\|SiteID" /home/user/chat/history-service/main.go
```

Update the call site to pass `cfg.SiteID` to `NewHistoryService` and to call `RegisterHandlers(r)` (no second arg). If `cfg.SiteID` is not yet a config field on history-service's `Config` struct, add it (env var `SITE_ID`, `envDefault:"site-local"` matching the pattern in other services like search-service/main.go).

- [ ] **Step 4: Verify build + existing tests still pass**

```bash
go vet ./history-service/...
go test -race ./history-service/...
```
Expected: build clean, all existing tests PASS. (Existing tests may need to be updated if they constructed `HistoryService` directly — pass `siteID: "site-test"` or similar.)

- [ ] **Step 5: Commit**

```bash
git add history-service/internal/service/service.go history-service/main.go history-service/internal/service/messages_test.go
git commit -m "feat(history-service): thread siteID into HistoryService struct"
```

(If any test files in history-service needed the siteID injection update, include those in the same commit.)

### Task B2: Publish canonical `.updated` event after `EditMessage`'s Cassandra write; drop direct `chat.room.<roomID>.event`

**Files:**
- Modify: `history-service/internal/service/messages.go`
- Modify: `history-service/internal/service/messages_test.go`

- [ ] **Step 1: Write a failing test for the canonical-publish behavior**

Locate the existing `EditMessage` test(s) in `history-service/internal/service/messages_test.go`. There's likely a test that asserts the `publisher.Publish` was called with `subject.RoomEvent(roomID)` and a `MessageEditedEvent` payload. We're going to REPLACE that assertion with one that checks the canonical publish.

Append a new test:

```go
func TestEditMessage_PublishesCanonicalUpdatedEvent(t *testing.T) {
	// Construct a HistoryService with a fake EventPublisher; perform EditMessage;
	// assert the publisher received exactly one Publish call to MsgCanonicalUpdated
	// with a MessageEvent payload whose Event == EventUpdated and Message.EditedAt/UpdatedAt
	// are populated.
	pub := &fakeEventPublisher{}
	// ... reuse existing test scaffolding to build the service ...
	svc := newTestHistoryService(t, withPublisher(pub), withSiteID("site-a"))

	resp, err := svc.EditMessage(testCtx("alice", "r1"), models.EditMessageRequest{
		MessageID: "msg-1",
		NewMsg:    "updated content",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	require.Len(t, pub.calls, 1, "exactly one publish (canonical only, no direct room.event)")
	call := pub.calls[0]
	assert.Equal(t, subject.MsgCanonicalUpdated("site-a"), call.subject)

	var evt model.MessageEvent
	require.NoError(t, json.Unmarshal(call.data, &evt))
	assert.Equal(t, model.EventUpdated, evt.Event)
	assert.Equal(t, "msg-1", evt.Message.ID)
	assert.Equal(t, "r1", evt.Message.RoomID)
	assert.Equal(t, "updated content", evt.Message.Content)
	require.NotNil(t, evt.Message.EditedAt)
	require.NotNil(t, evt.Message.UpdatedAt)
	assert.Equal(t, "site-a", evt.SiteID)
	assert.NotZero(t, evt.Timestamp)
}

func TestEditMessage_PublishFailureDoesNotFailRPC(t *testing.T) {
	pub := &fakeEventPublisher{publishErr: errors.New("nats down")}
	svc := newTestHistoryService(t, withPublisher(pub), withSiteID("site-a"))
	resp, err := svc.EditMessage(testCtx("alice", "r1"), models.EditMessageRequest{
		MessageID: "msg-1",
		NewMsg:    "updated content",
	})
	require.NoError(t, err, "publish failure must not fail the RPC")
	require.NotNil(t, resp)
}
```

(The `fakeEventPublisher`, `newTestHistoryService`, `testCtx`, and `withPublisher`/`withSiteID` helpers either already exist in the file — read it to find them — or you'll add them. If the existing tests use a different fake-publisher pattern, follow it; the assertions above are what matter.)

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test -race -run 'TestEditMessage_PublishesCanonicalUpdatedEvent|TestEditMessage_PublishFailureDoesNotFailRPC' ./history-service/internal/service/...
```
Expected: FAIL because the current `EditMessage` publishes to `subject.RoomEvent(roomID)`, not `subject.MsgCanonicalUpdated(siteID)`, and the payload is `MessageEditedEvent`, not `MessageEvent{Event: EventUpdated}`.

- [ ] **Step 3: Replace the direct RoomEvent publish with the canonical publish in `EditMessage`**

Locate the existing publish block in `history-service/internal/service/messages.go` (around lines 366-393). It currently constructs a `MessageEditedEvent`, marshals it, and publishes to `subject.RoomEvent(roomID)`. Replace the entire post-Cassandra-write block (from `editedAtMs := editedAt.UnixMilli()` through the end of the existing publish error logging) with:

```go
	editedAtMs := editedAt.UnixMilli()

	// Build the canonical MessageEvent for downstream consumers (search-sync-worker
	// updates ES, broadcast-worker fans out per-user live events). The updated
	// Message carries EditedAt + UpdatedAt set to the same instant (matches the
	// Cassandra row written above).
	updatedMsg := *msg
	updatedMsg.Content = req.NewMsg
	updatedMsg.EditedAt = &editedAt
	updatedMsg.UpdatedAt = &editedAt
	canonicalEvt := model.MessageEvent{
		Event:     model.EventUpdated,
		Message:   updatedMsg,
		SiteID:    s.siteID,
		Timestamp: editedAtMs,
	}
	if payload, err := json.Marshal(&canonicalEvt); err == nil {
		if pubErr := s.publisher.Publish(c, subject.MsgCanonicalUpdated(s.siteID), payload); pubErr != nil {
			// Best-effort: log + continue. Cassandra is the source of truth and
			// is already updated; search index will be stale until the next
			// canonical publish for this message. Matches D2 in the spec.
			slog.Warn("edit: canonical publish failed",
				"error", pubErr, "messageID", req.MessageID, "roomID", roomID)
		}
	} else {
		slog.Warn("edit: canonical marshal failed",
			"error", err, "messageID", req.MessageID, "roomID", roomID)
	}
```

If `msg.UserAccount` / `msg.UserID` / other fields are not on the `msg` variable in scope (they should be — `msg` comes from `s.findMessage`), check the local variable shape and adjust the `updatedMsg` construction.

Also: the `encryptEditMsg` plaintext/encrypted-content logic (currently used for the live event payload) is no longer needed in this handler — broadcast-worker will do its own encryption choice when it builds the `RoomEvent`. Remove the `encryptEditMsg` call and its consumed `plainMsg`/`encMsg`/`encErr` variables from this handler (but leave the helper function itself in place — broadcast-worker may need it; we'll address that in Task D2).

- [ ] **Step 4: Run the new test + existing EditMessage tests**

```bash
go test -race -run 'TestEditMessage' ./history-service/internal/service/...
```
Expected: the new tests PASS. Any pre-existing test that asserted on the direct `RoomEvent(roomID)` publish or `MessageEditedEvent` payload will FAIL — that's expected. Locate those tests; they need to be **deleted or rewritten** to expect canonical publish behavior (the new tests above already cover the canonical-publish path, so the old tests are now redundant).

- [ ] **Step 5: Remove or rewrite obsolete tests**

Find old tests by searching:
```bash
grep -n "subject.RoomEvent\|MessageEditedEvent" /home/user/chat/history-service/internal/service/messages_test.go
```

For each test that asserted on the live RoomEvent publish from `EditMessage`: delete it (the new canonical-publish test covers the spec's contract; the old test asserted on the old behavior which is intentionally gone).

Re-run all history-service tests:
```bash
go test -race ./history-service/...
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add history-service/internal/service/messages.go history-service/internal/service/messages_test.go
git commit -m "feat(history-service): publish canonical .updated event on EditMessage, drop direct RoomEvent publish"
```

### Task B3: Publish canonical `.deleted` event after `DeleteMessage`'s Cassandra soft-delete; drop direct `chat.room.<roomID>.event`

**Files:**
- Modify: `history-service/internal/service/messages.go`
- Modify: `history-service/internal/service/messages_test.go`

- [ ] **Step 1: Write a failing test for the canonical .deleted publish**

Append to `messages_test.go`:

```go
func TestDeleteMessage_PublishesCanonicalDeletedEvent(t *testing.T) {
	pub := &fakeEventPublisher{}
	svc := newTestHistoryService(t, withPublisher(pub), withSiteID("site-a"))

	resp, err := svc.DeleteMessage(testCtx("alice", "r1"), models.DeleteMessageRequest{
		MessageID: "msg-1",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	require.Len(t, pub.calls, 1, "exactly one publish (canonical only, no direct room.event)")
	call := pub.calls[0]
	assert.Equal(t, subject.MsgCanonicalDeleted("site-a"), call.subject)

	var evt model.MessageEvent
	require.NoError(t, json.Unmarshal(call.data, &evt))
	assert.Equal(t, model.EventDeleted, evt.Event)
	assert.Equal(t, "msg-1", evt.Message.ID)
	assert.Equal(t, "r1", evt.Message.RoomID)
	require.NotNil(t, evt.Message.UpdatedAt, "deleted message must carry UpdatedAt = delete time")
	assert.Equal(t, "site-a", evt.SiteID)
	assert.NotZero(t, evt.Timestamp)
}

func TestDeleteMessage_AlreadyDeletedShortCircuit_DoesNotPublish(t *testing.T) {
	// The existing handler short-circuits on already-deleted messages to
	// prevent duplicate events. Preserve that behavior.
	pub := &fakeEventPublisher{}
	svc := newTestHistoryService(t,
		withPublisher(pub),
		withSiteID("site-a"),
		withExistingDeletedMessage("msg-1", "r1"),
	)
	_, err := svc.DeleteMessage(testCtx("alice", "r1"), models.DeleteMessageRequest{
		MessageID: "msg-1",
	})
	require.NoError(t, err)
	assert.Empty(t, pub.calls, "already-deleted short-circuit must not republish canonical")
}
```

(`withExistingDeletedMessage` is a fixture helper — follow the existing pattern in messages_test.go for setting up Cassandra-fixture rows; if no such helper exists, create one inline.)

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test -race -run 'TestDeleteMessage_PublishesCanonicalDeletedEvent|TestDeleteMessage_AlreadyDeletedShortCircuit_DoesNotPublish' ./history-service/internal/service/...
```
Expected: FAIL — current behavior publishes to `subject.RoomEvent(roomID)` with `MessageDeletedEvent`, not canonical.

- [ ] **Step 3: Replace the post-soft-delete publish block in `DeleteMessage`**

Locate `DeleteMessage` in `history-service/internal/service/messages.go` (function starts around line 407). After `actualDeletedAt, applied, err := s.msgWriter.SoftDeleteMessage(...)`, replace the existing publish-MessageDeletedEvent block (around line 464) with:

```go
	// Build canonical MessageEvent{Event: EventDeleted} for downstream consumers.
	// Carry only the fields needed for identification + timestamping; Content
	// is intentionally left empty (the message is being deleted).
	canonicalEvt := model.MessageEvent{
		Event: model.EventDeleted,
		Message: model.Message{
			ID:          msg.ID,
			RoomID:      msg.RoomID,
			UserID:      msg.UserID,
			UserAccount: msg.UserAccount,
			CreatedAt:   msg.CreatedAt,
			UpdatedAt:   &actualDeletedAt,
		},
		SiteID:    s.siteID,
		Timestamp: actualDeletedAt.UnixMilli(),
	}
	if payload, err := json.Marshal(&canonicalEvt); err == nil {
		if pubErr := s.publisher.Publish(c, subject.MsgCanonicalDeleted(s.siteID), payload); pubErr != nil {
			slog.Warn("delete: canonical publish failed",
				"error", pubErr, "messageID", req.MessageID, "roomID", roomID)
		}
	} else {
		slog.Warn("delete: canonical marshal failed",
			"error", err, "messageID", req.MessageID, "roomID", roomID)
	}
```

`actualDeletedAt` is the value `SoftDeleteMessage` returned (the row's stamped `updated_at`); `applied` indicates whether the delete actually changed state — the existing handler should already gate the publish on `applied` being true. Confirm by reading the surrounding code and preserve that gate (don't publish on no-op deletes — see the already-deleted test).

- [ ] **Step 4: Run new + existing tests**

```bash
go test -race ./history-service/...
```
Expected: new tests PASS; any tests that asserted on the old direct `MessageDeletedEvent` to `subject.RoomEvent(roomID)` will FAIL — delete them (same rationale as B2 step 5).

- [ ] **Step 5: Commit**

```bash
git add history-service/internal/service/messages.go history-service/internal/service/messages_test.go
git commit -m "feat(history-service): publish canonical .deleted event on DeleteMessage, drop direct RoomEvent publish"
```

### Task B4: Remove now-unused `MessageEditedEvent` / `MessageDeletedEvent` model types and `encryptEditMsg` helper (if unused)

The live event types in `history-service/internal/models/event.go` were the payload shape for the direct `chat.room.<roomID>.event` publish that B2/B3 removed. Check if anything else still depends on them.

**Files:**
- Modify: `history-service/internal/models/event.go`
- Modify: `history-service/internal/models/message_test.go` (delete tests for the removed types)
- Possibly modify: `history-service/internal/service/messages.go` (remove `encryptEditMsg` if no longer called)

- [ ] **Step 1: Find remaining references**

```bash
grep -rn "MessageEditedEvent\|MessageDeletedEvent" /home/user/chat --include="*.go"
```

If references remain in non-test code outside of history-service, STOP and report — there's another consumer we missed. Otherwise, proceed.

- [ ] **Step 2: Delete the types and their tests**

In `history-service/internal/models/event.go`: delete both type definitions (`MessageEditedEvent`, `MessageDeletedEvent`).

In `history-service/internal/models/message_test.go`: delete `TestMessageEditedEvent_JSON` and `TestMessageDeletedEvent_JSON` and any related test cases.

- [ ] **Step 3: Check `encryptEditMsg` usage**

```bash
grep -n "encryptEditMsg" /home/user/chat/history-service/internal/service/messages.go
```

If the only remaining reference is the function definition itself (no callers since `EditMessage` no longer uses it), delete the function. If broadcast-worker grows a need for this in Task D2, you'll re-introduce it there with the right structure.

- [ ] **Step 4: Build + test**

```bash
go vet ./history-service/...
go test -race ./history-service/...
```
Expected: clean build, all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add history-service/internal/models/event.go history-service/internal/models/message_test.go history-service/internal/service/messages.go
git commit -m "chore(history-service): remove obsolete MessageEditedEvent/MessageDeletedEvent types and encryptEditMsg helper"
```

---

## Phase C — `message-worker`: `FilterSubject: .created`

With history-service publishing `.updated` and `.deleted` to the same stream, message-worker's existing consumer would now receive them (its consumer has no subject filter today). Add an explicit filter to keep it strictly on the create flow.

### Task C1: Add `FilterSubject` to message-worker's canonical consumer config

**Files:**
- Modify: `message-worker/main.go`
- Modify: `message-worker/main_test.go` (or whichever `*_test.go` covers `buildConsumerConfig`)

- [ ] **Step 1: Locate `buildConsumerConfig` and its test**

```bash
grep -n "buildConsumerConfig" /home/user/chat/message-worker/*.go
```

- [ ] **Step 2: Write the failing test**

Either extend an existing test of `buildConsumerConfig`, or append a new test in the matching test file:

```go
func TestBuildConsumerConfig_FilterSubjectIsCreatedOnly(t *testing.T) {
	cfg := buildConsumerConfig(stream.ConsumerSettings{}) // pass defaults; the function's signature in this repo is just (s stream.ConsumerSettings)
	// The siteID isn't a parameter today — message-worker must be passing it in
	// when constructing the consumer. Verify the FilterSubject is set to the
	// .created subject for the configured site.
	t.Fatalf("see test below — the actual signature may need site injection. Replace this stub once the real signature is in scope.")
}
```

(This test is a placeholder. If `buildConsumerConfig` doesn't take `siteID` today, the actual implementation step below adds that — and then the test becomes:)

```go
func TestBuildConsumerConfig_FilterSubjectIsCreatedOnly(t *testing.T) {
	cfg := buildConsumerConfig(stream.ConsumerSettings{}, "site-a")
	assert.Equal(t, subject.MsgCanonicalCreated("site-a"), cfg.FilterSubject,
		"message-worker must only consume canonical .created subjects to avoid re-processing edits/deletes")
}
```

- [ ] **Step 3: Run test to verify it fails**

```bash
go test -race -run TestBuildConsumerConfig_FilterSubjectIsCreatedOnly ./message-worker/...
```
Expected: FAIL — either with "undefined" (if siteID isn't a parameter yet) or with the FilterSubject being empty.

- [ ] **Step 4: Update `buildConsumerConfig` to accept `siteID` and set `FilterSubject`**

In `message-worker/main.go`, change:

```go
func buildConsumerConfig(s stream.ConsumerSettings) jetstream.ConsumerConfig {
	cc := stream.DurableConsumerDefaults(s)
	cc.Durable = "message-worker"
	return cc
}
```

to:

```go
func buildConsumerConfig(s stream.ConsumerSettings, siteID string) jetstream.ConsumerConfig {
	cc := stream.DurableConsumerDefaults(s)
	cc.Durable = "message-worker"
	// Filter to .created only — history-service publishes .updated and .deleted
	// to the same stream, and they must NOT be re-processed by message-worker
	// (history-service already wrote Cassandra synchronously for those). See
	// spec D4 in docs/superpowers/specs/2026-05-14-message-edit-delete-canonical-events-design.md.
	cc.FilterSubject = subject.MsgCanonicalCreated(siteID)
	return cc
}
```

Update the call site in `message-worker/main.go` to pass `cfg.SiteID` (or whatever the local config var is called).

- [ ] **Step 5: Run test to verify it passes + full message-worker tests**

```bash
go test -race ./message-worker/...
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add message-worker/main.go message-worker/main_test.go
git commit -m "feat(message-worker): filter canonical consumer to .created only"
```

---

## Phase D — `broadcast-worker`: dispatch on .updated and .deleted with per-user fan-out

Broadcast-worker's current handler always treats input as a created-message event. We need to (1) widen the handler to dispatch on `evt.Event`, (2) handle `EventUpdated` and `EventDeleted` by building the corresponding `RoomEvent` and fanning out per-user.

### Task D1: Refactor `HandleMessage` to dispatch on `evt.Event`

**Files:**
- Modify: `broadcast-worker/handler.go`
- Modify: `broadcast-worker/handler_test.go`

- [ ] **Step 1: Write a failing test for the dispatch dispatcher**

In `broadcast-worker/handler_test.go`, add:

```go
func TestHandleMessage_DispatchesOnEventType(t *testing.T) {
	// Inject a fake publisher; verify that .updated payload causes the handler
	// to emit a RoomEventMessageEdited (not RoomEventNewMessage) and that
	// .deleted causes RoomEventMessageDeleted.
	cases := []struct {
		name       string
		event      model.EventType
		wantType   model.RoomEventType
	}{
		{"created", model.EventCreated, model.RoomEventNewMessage},
		{"updated", model.EventUpdated, model.RoomEventMessageEdited},
		{"deleted", model.EventDeleted, model.RoomEventMessageDeleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, pub := newTestHandler(t)
			payload := buildMessageEventPayload(t, tc.event, "msg-1", "r1", "alice")
			err := h.HandleMessage(context.Background(), payload)
			require.NoError(t, err)
			require.NotEmpty(t, pub.calls, "should publish at least one RoomEvent")
			assertAllPublishedRoomEventTypes(t, pub.calls, tc.wantType)
		})
	}
}
```

(`newTestHandler`, `buildMessageEventPayload`, and `assertAllPublishedRoomEventTypes` are helpers you'll add to the test file. For `newTestHandler` — read the existing tests in `broadcast-worker/handler_test.go` to find how the handler is constructed today; mirror that pattern. For `buildMessageEventPayload` — return a `[]byte` containing a marshaled `model.MessageEvent` with the requested `Event` field set. For the assertion — decode each published `data` as a `model.RoomEvent` and assert each one's `Type`.)

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test -race -run TestHandleMessage_DispatchesOnEventType ./broadcast-worker/...
```
Expected: FAIL — the current handler always emits `RoomEventNewMessage`.

- [ ] **Step 3: Refactor `HandleMessage` to switch on `evt.Event`**

In `broadcast-worker/handler.go`, the current `HandleMessage` starts at line 48. Refactor it to dispatch:

```go
func (h *Handler) HandleMessage(ctx context.Context, data []byte) error {
	var evt model.MessageEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		return fmt.Errorf("unmarshal message event: %w", err)
	}

	switch evt.Event {
	case "", model.EventCreated:
		return h.handleCreated(ctx, &evt)
	case model.EventUpdated:
		return h.handleUpdated(ctx, &evt)
	case model.EventDeleted:
		return h.handleDeleted(ctx, &evt)
	default:
		slog.Warn("unknown message event type, skipping", "event", evt.Event, "messageID", evt.Message.ID)
		return nil
	}
}

// handleCreated is the existing create-flow body of HandleMessage.
// (Move the contents of the original HandleMessage body here, starting from
// `msg := evt.Message` through the existing switch on room.Type.)
func (h *Handler) handleCreated(ctx context.Context, evt *model.MessageEvent) error {
	// ... existing body, lifted verbatim from HandleMessage ...
}

// handleUpdated will be filled in by Task D2.
func (h *Handler) handleUpdated(ctx context.Context, evt *model.MessageEvent) error {
	return fmt.Errorf("handleUpdated not yet implemented")
}

// handleDeleted will be filled in by Task D3.
func (h *Handler) handleDeleted(ctx context.Context, evt *model.MessageEvent) error {
	return fmt.Errorf("handleDeleted not yet implemented")
}
```

The empty `""` case in the switch preserves backwards-compat: existing canonical create publishes don't always set `Event` (the field is `omitempty`). Treat empty as `EventCreated`.

- [ ] **Step 4: Run dispatcher test (created arm passes; updated/deleted return stub errors — that's the next two tasks)**

```bash
go test -race -run TestHandleMessage_DispatchesOnEventType ./broadcast-worker/...
```
Expected: the `created` subtest PASSES; the `updated` and `deleted` subtests FAIL with "not yet implemented". That's expected — D2 and D3 fill them in.

Also run the broader handler tests to confirm no regression in the create path:
```bash
go test -race ./broadcast-worker/...
```
Expected: all PASS except the two stub-error tests.

- [ ] **Step 5: Commit (intermediate, stubbed)**

```bash
git add broadcast-worker/handler.go broadcast-worker/handler_test.go
git commit -m "refactor(broadcast-worker): dispatch HandleMessage on MessageEvent.Event"
```

### Task D2: Implement `handleUpdated` — per-user RoomEvent fan-out for edits

**Files:**
- Modify: `broadcast-worker/handler.go`
- Modify: `broadcast-worker/handler_test.go`

- [ ] **Step 1: Write specific behavioral tests for `handleUpdated`**

Append to `handler_test.go`:

```go
func TestHandleUpdated_ChannelRoom_FansOutPerUser(t *testing.T) {
	h, pub := newTestHandler(t)
	// Seed: room r1 is a channel with 3 subscribers: alice, bob, carol.
	// Verify all 3 receive a RoomEventMessageEdited on chat.user.<acct>.room.event.
	seedChannelRoom(t, h, "r1", []string{"alice", "bob", "carol"})

	edited := time.Date(2026, 5, 14, 12, 5, 0, 0, time.UTC)
	evt := model.MessageEvent{
		Event:     model.EventUpdated,
		SiteID:    "site-a",
		Timestamp: edited.UnixMilli(),
		Message: model.Message{
			ID:          "msg-1",
			RoomID:      "r1",
			UserID:      "u-alice",
			UserAccount: "alice",
			Content:     "updated content",
			CreatedAt:   time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
			EditedAt:    &edited,
			UpdatedAt:   &edited,
		},
	}
	data, _ := json.Marshal(&evt)
	require.NoError(t, h.HandleMessage(context.Background(), data))

	require.Len(t, pub.calls, 3, "per-user fan-out: one publish per subscriber")
	subjects := map[string]bool{}
	for _, c := range pub.calls {
		subjects[c.subject] = true
		var roomEvt model.RoomEvent
		require.NoError(t, json.Unmarshal(c.data, &roomEvt))
		assert.Equal(t, model.RoomEventMessageEdited, roomEvt.Type)
		require.NotNil(t, roomEvt.MessageEdited)
		assert.Equal(t, "msg-1", roomEvt.MessageEdited.MessageID)
		assert.Equal(t, "updated content", roomEvt.MessageEdited.NewContent)
		assert.Equal(t, "alice", roomEvt.MessageEdited.EditedBy)
		assert.True(t, roomEvt.MessageEdited.EditedAt.Equal(edited))
		assert.True(t, roomEvt.MessageEdited.UpdatedAt.Equal(edited))
	}
	assert.True(t, subjects[subject.UserRoomEvent("alice")])
	assert.True(t, subjects[subject.UserRoomEvent("bob")])
	assert.True(t, subjects[subject.UserRoomEvent("carol")])
}
```

(`seedChannelRoom` is a helper that primes the handler's `store` / `userStore` fakes so the lookup for room r1 returns a channel with the three subscribers. Mirror however the existing `.created` tests set this up.)

- [ ] **Step 2: Run test to verify it fails**

```bash
go test -race -run TestHandleUpdated_ChannelRoom_FansOutPerUser ./broadcast-worker/...
```
Expected: FAIL with "handleUpdated not yet implemented".

- [ ] **Step 3: Implement `handleUpdated`**

The general shape mirrors the existing channel-event publish path (`publishChannelEvent`). Adapt it to emit `RoomEventMessageEdited`. Replace the stub `handleUpdated`:

```go
func (h *Handler) handleUpdated(ctx context.Context, evt *model.MessageEvent) error {
	msg := evt.Message

	room, err := h.store.GetRoomByID(ctx, msg.RoomID)
	if err != nil {
		return fmt.Errorf("fetch room %s: %w", msg.RoomID, err)
	}

	subs, err := h.store.ListSubscriberAccounts(ctx, room.ID)
	if err != nil {
		return fmt.Errorf("list subscribers %s: %w", room.ID, err)
	}

	if msg.EditedAt == nil || msg.UpdatedAt == nil {
		return fmt.Errorf("edited event missing EditedAt or UpdatedAt: %s", msg.ID)
	}

	roomEvt := model.RoomEvent{
		Type:   model.RoomEventMessageEdited,
		RoomID: room.ID,
		MessageEdited: &model.MessageEditedPayload{
			MessageID:  msg.ID,
			NewContent: msg.Content,
			EditedBy:   msg.UserAccount,
			EditedAt:   *msg.EditedAt,
			UpdatedAt:  *msg.UpdatedAt,
		},
	}
	payload, err := json.Marshal(&roomEvt)
	if err != nil {
		return fmt.Errorf("marshal edited room event: %w", err)
	}

	for _, account := range subs {
		if err := h.pub.Publish(ctx, subject.UserRoomEvent(account), payload); err != nil {
			// Same per-subscriber error policy as the existing create path —
			// log and continue (one failed subscriber must not block the others).
			slog.Warn("publish edited event failed",
				"error", err, "account", account, "messageID", msg.ID, "roomID", room.ID)
		}
	}
	return nil
}
```

`GetRoomByID` and `ListSubscriberAccounts` may or may not exist on the existing `Store` interface — check `broadcast-worker/handler.go` for the existing interface methods used by the create path (`FetchAndUpdateRoom`, etc.). If a subscriber-lookup method doesn't exist, add one to the interface (and the mock store), modeled on whatever query the create path already issues to determine fan-out.

If the existing fan-out uses `FetchAndUpdateRoom` + a separate subscriber query, mirror that pattern. For now, the test above assumes a flat `ListSubscriberAccounts(ctx, roomID) ([]string, error)` exists — adjust to the real method name during implementation.

- [ ] **Step 4: Run tests to verify pass**

```bash
go test -race ./broadcast-worker/...
```
Expected: the new `TestHandleUpdated_ChannelRoom_FansOutPerUser` PASSES; the dispatcher test's `updated` sub-test now passes too; create tests still pass.

- [ ] **Step 5: Commit**

```bash
git add broadcast-worker/handler.go broadcast-worker/handler_test.go
git commit -m "feat(broadcast-worker): handle .updated canonical events, fan out RoomEventMessageEdited per-user"
```

### Task D3: Implement `handleDeleted` — per-user RoomEvent fan-out for deletes

Symmetric to D2.

**Files:**
- Modify: `broadcast-worker/handler.go`
- Modify: `broadcast-worker/handler_test.go`

- [ ] **Step 1: Write failing test**

Append:

```go
func TestHandleDeleted_ChannelRoom_FansOutPerUser(t *testing.T) {
	h, pub := newTestHandler(t)
	seedChannelRoom(t, h, "r1", []string{"alice", "bob", "carol"})

	deletedAt := time.Date(2026, 5, 14, 12, 10, 0, 0, time.UTC)
	evt := model.MessageEvent{
		Event:     model.EventDeleted,
		SiteID:    "site-a",
		Timestamp: deletedAt.UnixMilli(),
		Message: model.Message{
			ID:          "msg-1",
			RoomID:      "r1",
			UserID:      "u-alice",
			UserAccount: "alice",
			CreatedAt:   time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
			UpdatedAt:   &deletedAt,
		},
	}
	data, _ := json.Marshal(&evt)
	require.NoError(t, h.HandleMessage(context.Background(), data))

	require.Len(t, pub.calls, 3, "per-user fan-out: one publish per subscriber")
	for _, c := range pub.calls {
		var roomEvt model.RoomEvent
		require.NoError(t, json.Unmarshal(c.data, &roomEvt))
		assert.Equal(t, model.RoomEventMessageDeleted, roomEvt.Type)
		require.NotNil(t, roomEvt.MessageDeleted)
		assert.Equal(t, "msg-1", roomEvt.MessageDeleted.MessageID)
		assert.Equal(t, "alice", roomEvt.MessageDeleted.DeletedBy)
		assert.True(t, roomEvt.MessageDeleted.DeletedAt.Equal(deletedAt))
		assert.True(t, roomEvt.MessageDeleted.UpdatedAt.Equal(deletedAt))
	}
}
```

- [ ] **Step 2: Run + verify it fails**

```bash
go test -race -run TestHandleDeleted_ChannelRoom_FansOutPerUser ./broadcast-worker/...
```
Expected: FAIL with "handleDeleted not yet implemented".

- [ ] **Step 3: Implement `handleDeleted`**

```go
func (h *Handler) handleDeleted(ctx context.Context, evt *model.MessageEvent) error {
	msg := evt.Message

	room, err := h.store.GetRoomByID(ctx, msg.RoomID)
	if err != nil {
		return fmt.Errorf("fetch room %s: %w", msg.RoomID, err)
	}

	subs, err := h.store.ListSubscriberAccounts(ctx, room.ID)
	if err != nil {
		return fmt.Errorf("list subscribers %s: %w", room.ID, err)
	}

	if msg.UpdatedAt == nil {
		return fmt.Errorf("deleted event missing UpdatedAt: %s", msg.ID)
	}

	roomEvt := model.RoomEvent{
		Type:   model.RoomEventMessageDeleted,
		RoomID: room.ID,
		MessageDeleted: &model.MessageDeletedPayload{
			MessageID: msg.ID,
			DeletedBy: msg.UserAccount,
			DeletedAt: *msg.UpdatedAt,
			UpdatedAt: *msg.UpdatedAt,
		},
	}
	payload, err := json.Marshal(&roomEvt)
	if err != nil {
		return fmt.Errorf("marshal deleted room event: %w", err)
	}

	for _, account := range subs {
		if err := h.pub.Publish(ctx, subject.UserRoomEvent(account), payload); err != nil {
			slog.Warn("publish deleted event failed",
				"error", err, "account", account, "messageID", msg.ID, "roomID", room.ID)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests**

```bash
go test -race ./broadcast-worker/...
```
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add broadcast-worker/handler.go broadcast-worker/handler_test.go
git commit -m "feat(broadcast-worker): handle .deleted canonical events, fan out RoomEventMessageDeleted per-user"
```

### Task D4: DM rooms — confirm `handleUpdated`/`handleDeleted` also work for DM fan-out

The existing create path has a separate `publishDMEvents` (different subscriber-set semantics for DMs vs channels — DMs have exactly two members). Audit the new edit/delete paths to ensure they handle DMs correctly too.

**Files:**
- Possibly modify: `broadcast-worker/handler.go`
- Modify: `broadcast-worker/handler_test.go`

- [ ] **Step 1: Add DM tests**

```go
func TestHandleUpdated_DM_FansOutToBothMembers(t *testing.T) {
	h, pub := newTestHandler(t)
	seedDMRoom(t, h, "dm-r", []string{"alice", "bob"})

	edited := time.Now().UTC()
	evt := model.MessageEvent{
		Event:  model.EventUpdated,
		SiteID: "site-a",
		Message: model.Message{
			ID:          "msg-1",
			RoomID:      "dm-r",
			UserAccount: "alice",
			Content:     "updated",
			CreatedAt:   edited.Add(-5 * time.Minute),
			EditedAt:    &edited,
			UpdatedAt:   &edited,
		},
		Timestamp: edited.UnixMilli(),
	}
	data, _ := json.Marshal(&evt)
	require.NoError(t, h.HandleMessage(context.Background(), data))

	require.Len(t, pub.calls, 2, "DM fan-out: both members receive the edited event")
}
```

(Add an equivalent `TestHandleDeleted_DM_FansOutToBothMembers` mirroring D3's test.)

- [ ] **Step 2: Run tests**

```bash
go test -race -run 'TestHandleUpdated_DM|TestHandleDeleted_DM' ./broadcast-worker/...
```

If they PASS — the existing `ListSubscriberAccounts` (or whatever the real method is) already returns both DM members, and no change is needed. Commit a "tests: confirm DM fan-out" commit and proceed.

If they FAIL — the create path's DM logic isn't being exercised by `handleUpdated`/`handleDeleted`. In that case, refactor: extract a `fanOut(ctx, room, payload) error` helper that does the room-type-aware subscriber lookup (matching the existing `publishChannelEvent`/`publishDMEvents` split), and call it from all three handlers (`handleCreated`, `handleUpdated`, `handleDeleted`).

- [ ] **Step 3: Commit**

```bash
git add broadcast-worker/handler.go broadcast-worker/handler_test.go
git commit -m "test(broadcast-worker): confirm DM fan-out works for .updated and .deleted"
```

(Or, if a refactor was needed: `refactor(broadcast-worker): extract fanOut helper for shared per-user-event distribution`.)

---

## Phase E — Integration verification

### Task E1: Run the existing search-sync-worker integration test end-to-end

The integration test at `search-sync-worker/integration_test.go:330-400` already publishes `.created`/`.updated`/`.deleted` and verifies the ES end state. After Phase B, the same flow happens for real via history-service → JetStream → search-sync-worker.

**Files:**
- Run only — no code change.

- [ ] **Step 1: Run the test**

```bash
make test-integration SERVICE=search-sync-worker
```

Expected: the existing test PASSES end-to-end. If it fails:
- If Docker isn't available in the environment, this is the environment issue (skip; CI will catch it).
- If a real assertion fails, investigate — the most likely cause is a payload-shape mismatch between what history-service now publishes (Task B2/B3) and what the integration test's `loadTestEvents` fixtures expect. Reconcile.

- [ ] **Step 2: Commit only if a code fix was needed**

If the test was already passing, no commit. If a fix was required, commit it.

### Task E2: Add end-to-end test from history-service → canonical → search-sync-worker

If broadcast-worker / search-sync-worker integration tests cover the canonical flow but not the producer-side (history-service publish), consider adding a single integration test that ties the two ends. This is **optional** if the per-service tests in Phase B + E1 already cover the contract; skip if pre-existing coverage is sufficient.

**Files:**
- (Optional) Modify: `history-service/internal/service/integration_test.go`

- [ ] **Step 1: Survey existing integration coverage**

```bash
grep -n "func TestIntegration\|MsgCanonical" /home/user/chat/history-service/internal/service/integration_test.go
```

If the file already has integration tests that exercise the publisher with a real NATS testcontainer, extend those to assert the canonical subjects.

If existing integration tests use a fake publisher (no real NATS), an additional integration test is low-value — skip this task.

- [ ] **Step 2: Decide and commit (if added)**

```bash
git add history-service/internal/service/integration_test.go
git commit -m "test(history-service): integration coverage for canonical .updated/.deleted publishes"
```

---

## Phase F — Update PR #182 description

The PR currently describes only the schema-level changes (Message.EditedAt + UpdatedAt fields, ES index plumbing). Now it also covers the canonical publishing, message-worker filter, and broadcast-worker fan-out. Update the body so reviewers know the full scope.

### Task F1: Update PR description

**Files:**
- GitHub PR #182 (via `mcp__github__update_pull_request` or manually in the GitHub UI).

- [ ] **Step 1: Compose the revised description**

The body should cover:

- **Summary:** what the PR does — model schema changes for `Message.EditedAt`/`UpdatedAt`, wire `.updated`/`.deleted` canonical publishes from history-service, filter message-worker to `.created`, broadcast-worker fan-out for edits/deletes (cross-site clients now get live edit/delete notifications via the existing per-user-account bridge).
- **Spec reference:** `docs/superpowers/specs/2026-05-14-message-edit-delete-canonical-events-design.md`.
- **Breaking change call-out:** the legacy live event types `models.MessageEditedEvent` / `models.MessageDeletedEvent` and the direct `chat.room.<roomID>.event` publish are gone. Clients consuming `chat.user.<account>.room.event` need to handle two new `RoomEvent.Type` variants: `RoomEventMessageEdited` and `RoomEventMessageDeleted`.
- **Test plan:** lint, unit tests, integration tests, and a manual smoke against `docker-compose up` (if applicable).

- [ ] **Step 2: Push the description**

If `mcp__github__update_pull_request` is available, use it:

```text
update_pull_request(owner=hmchangw, repo=chat, pullNumber=182, title=…, body=<composed text>)
```

Otherwise, paste the body into the GitHub UI for PR #182.

- [ ] **Step 3: Commit (none — description-only change, no repo file)**

This task has no repo commit.

---

## Final verification

- [ ] **Step 1: Lint + tests across the repo**

```bash
make lint
make test
```
Expected: both clean (0 lint issues, all packages PASS).

- [ ] **Step 2: Push the branch**

```bash
git push origin claude/search-message-editedat-updatedat
```
Expected: push succeeds.

- [ ] **Step 3: Watch CI on PR #182**

Open <https://github.com/hmchangw/chat/pull/182/checks> and confirm all checks complete green (or address failures as they surface).

---

## Out of Scope (Future PRs)

- **Cleanup of `history-service/internal/models/event.go`** — if Task B4 deleted the live event types but anything else surfaces a dependency, those are separate follow-ups (the spec's §11 follow-up #4 covers this).
- **Reactions / pins** — same `.updated` canonical path with new `Event` enum variants or new `RoomEvent.Type` constants. Not in this PR.
- **Edit-trail UI** (`(edited)` badges, "edited at HH:MM" hover) — frontend PR; this work delivers the data into both the search response (PR #182's earlier commits) and the live event (this plan).
- **Analytics consumer on canonical** — drop-in addition; not addressed here.

---

## Summary

| Phase | Tasks | LOC change estimate |
|---|---|---|
| A. pkg/model RoomEvent variants | 1 task | ~50 lines code + ~40 lines tests |
| B. history-service canonical publishes | 4 tasks | ~120 lines code (mostly deletions + small additions) + ~150 lines tests |
| C. message-worker FilterSubject | 1 task | ~10 lines code + ~10 lines tests |
| D. broadcast-worker dispatch | 4 tasks | ~150 lines code + ~200 lines tests |
| E. Integration verification | 2 tasks | 0–~80 lines (optional) |
| F. PR description | 1 task | 0 code |
| **Total** | **13 tasks** | **~330 lines code + ~480 lines tests** |
