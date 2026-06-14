# Reactions Post-Merge Follow-ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the three approved follow-ups from PR #258 review as one commit each on branch `claude/reactions-followups`: (1) drop dead `updated_at` touch + batch reaction writes via `gocql.UnloggedBatch`; (2) move reaction author-notification from `notification-worker` to `broadcast-worker` and narrow `notification-worker` consumer's `FilterSubjects` to `{MsgCanonicalCreated}` only; (3) add singular `FindUserByAccount` to `UserStore` and use it in the reaction handler.

**Architecture:** Three independent commits on a single branch. No wire-format change. Item 1 reduces Cassandra RTTs (Add 2→1, Remove 4→1) using the established `cassrepo/pin.go` `UnloggedBatch` precedent. Item 2 consolidates all reaction wire effects into `broadcast-worker.handleReacted` and removes the `Publisher` infrastructure from `notification-worker`. Item 3 adds the singular method alongside `FindUsersByAccounts` (which has genuine batch callers in `broadcast-worker`/`room-service`) and uses typed `ErrUserNotFound` branching.

**Tech Stack:** Go 1.25; `gocql` for Cassandra; `nats.go` + JetStream; `go.uber.org/mock` for mocks; `stretchr/testify` for assertions; testcontainers for Cassandra integration tests via `pkg/testutil`.

**Spec:** `docs/superpowers/specs/2026-06-10-reactions-post-merge-followups-design.md`

---

## File Structure

### Item 1 — Cassandra reactions batch
- **Modify** `history-service/internal/cassrepo/reactions.go` — replace 3-statement Add and 6-statement Remove with one `UnloggedBatch` each; delete three `touchUpdatedAt*` consts.
- **Modify** `history-service/internal/cassrepo/reactions_integration_test.go` — flip Remove-side `updated_at` assertions; add Add-side `updated_at == reactedAt` regression.
- **Modify** `docs/specs/message-reactions.md` §2.5 — update mirror-consistency story.
- **Modify** `docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md` §6 — note the batched-write shape and cite `cassrepo/pin.go` precedent.

### Item 2 — Move reaction author-notification to broadcast-worker
- **Modify** `broadcast-worker/handler.go` — extend `handleReacted` to publish author notification after the room fan-out.
- **Modify** `broadcast-worker/handler_test.go` — port the four reaction-policy tests; add publish-failure regression.
- **Modify** `notification-worker/handler.go` — delete `EventReacted` switch branch, `handleReaction`, `Publisher` interface, `ReactionPub` field on `HandlerDeps`, `natsPublisher` wrapper.
- **Modify** `notification-worker/main.go` — drop `ReactionPub:` line from `NewHandler` deps; narrow `cc.FilterSubjects` to `{MsgCanonicalCreated(siteID)}`.
- **Modify** `notification-worker/handler_test.go` — delete the four `TestHandleMessage_Reaction_*` tests + `mockReactionPub` / `reactionPubRecord` / `reactHandler` helpers.
- **Modify** `notification-worker/consumer_config_test.go` — `TestBuildConsumerConfig` filter assertion: 1 element instead of 2.
- **Modify** `docs/specs/message-reactions.md` §10 — move author-notification responsibility from notification-worker to broadcast-worker row.
- **Modify** `docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md` §3.2/§3.3 — redraw consumer table.
- **Modify** `docs/superpowers/specs/2026-06-04-notification-worker-system-message-allowlist-design.md` — append follow-up note: filter narrowed further to `{created}`.

### Item 3 — `FindUserByAccount` singular
- **Modify** `pkg/userstore/userstore.go` — add `FindUserByAccount` to `UserStore` interface; implement on `mongoStore`.
- **Modify** `pkg/userstore/cache.go` — add `FindUserByAccount` on `Cache` with prefixed singleflight key.
- **Modify** `pkg/userstore/cache_test.go` — add 4 cache tests; extend `fakeStore` with `FindUserByAccount` impl.
- **Modify** `pkg/userstore/integration_test.go` — add 2 mongoStore tests (hit + miss → `ErrUserNotFound`).
- **Modify** `history-service/internal/service/service.go` — add `FindUserByAccount` to the service-level `UserStore` interface.
- **Modify** `history-service/internal/service/reactions.go` — swap call site; add `userstore` import and `errors.Is(err, userstore.ErrUserNotFound)` branch.
- **Modify** `history-service/internal/service/reactions_test.go` — rewrite 8 `FindUsersByAccounts` mock expectations to `FindUserByAccount`; add `aliceUserPtr()` helper.
- **Regenerate** `history-service/internal/service/mocks/mock_repository.go` (and any other UserStore mocks) via `make generate`.

---

## Pre-flight (once, before any task)

- [ ] **Step 0.1: Confirm branch state**

```bash
cd /home/user/chat
git status --short
git branch --show-current
git log --oneline -3
```

Expected: clean tree; branch = `claude/reactions-followups`; HEAD = `21ba4d2e docs: spec for reactions post-merge follow-up changes`.

- [ ] **Step 0.2: Confirm baseline tests are green before any changes**

```bash
make test SERVICE=history-service
make test SERVICE=notification-worker
make test SERVICE=broadcast-worker
go test -race ./pkg/userstore/...
```

Expected: all pass. If any fail without our changes, stop and investigate before continuing.

---

## Task 1: Drop `updated_at` touch on RemoveReaction and batch the writes

**Files:**
- Modify: `history-service/internal/cassrepo/reactions.go`
- Modify: `history-service/internal/cassrepo/reactions_integration_test.go`
- Modify: `docs/specs/message-reactions.md` §2.5
- Modify: `docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md` §6

---

### Step 1.1: Flip the Remove-side integration assertions (Red)

The current `TestRepository_RemoveReaction_TopLevel` (lines 263–279) asserts `updated_at` is bumped on Remove. After the change the row's `updated_at` should *not* be bumped by Remove — Add still bumps it. Flip the assertion to expect the value at the *add* time (i.e. unchanged by the remove).

- [ ] **Step 1.1.a: Edit `reactions_integration_test.go`** — replace the Remove-side `updated_at` assertions in `TestRepository_RemoveReaction_TopLevel`.

Find this block (lines 263–279):

```go
	// Verify updated_at was bumped on Remove (regression guard for the
	// '_ = updatedAt' bug where RemoveReaction silently discarded the
	// timestamp and left updated_at frozen at the add time).
	var gotUpdatedAt time.Time
	require.NoError(t, repo.session.Query(
		`SELECT updated_at FROM messages_by_id WHERE message_id = ? AND created_at = ?`,
		msgID, createdAt,
	).Scan(&gotUpdatedAt))
	assert.WithinDuration(t, removedAt, gotUpdatedAt, time.Second,
		"messages_by_id.updated_at must reflect the remove time")

	require.NoError(t, repo.session.Query(
		`SELECT updated_at FROM messages_by_room WHERE room_id = ? AND bucket = ? AND created_at = ? AND message_id = ?`,
		roomID, bucketSizer.Of(createdAt), createdAt, msgID,
	).Scan(&gotUpdatedAt))
	assert.WithinDuration(t, removedAt, gotUpdatedAt, time.Second,
		"messages_by_room.updated_at must reflect the remove time")
```

Replace with:

```go
	// updated_at MUST NOT be bumped by Remove. The Remove path is a
	// per-cell DELETE only; touching updated_at would be dead work since
	// no consumer reads it for reaction freshness (broadcast events build
	// timestamps from the canonical event, search-sync skips reactions,
	// pagination uses created_at, no NATS ETag semantics). The row's
	// updated_at therefore reflects the Add (addedAt), not the Remove.
	var gotUpdatedAt time.Time
	require.NoError(t, repo.session.Query(
		`SELECT updated_at FROM messages_by_id WHERE message_id = ? AND created_at = ?`,
		msgID, createdAt,
	).Scan(&gotUpdatedAt))
	assert.WithinDuration(t, addedAt, gotUpdatedAt, time.Second,
		"messages_by_id.updated_at must reflect the add (not the remove)")

	require.NoError(t, repo.session.Query(
		`SELECT updated_at FROM messages_by_room WHERE room_id = ? AND bucket = ? AND created_at = ? AND message_id = ?`,
		roomID, bucketSizer.Of(createdAt), createdAt, msgID,
	).Scan(&gotUpdatedAt))
	assert.WithinDuration(t, addedAt, gotUpdatedAt, time.Second,
		"messages_by_room.updated_at must reflect the add (not the remove)")
```

- [ ] **Step 1.1.b: Add an Add-side regression test** — append to the same file (after the last existing test, before EOF):

```go
// TestRepository_AddReaction_BumpsUpdatedAt is a regression guard: Add still
// touches updated_at to reactedAt via the combined `SET reactions[?] = ?,
// updated_at = ?` statement (only Remove drops the touch).
func TestRepository_AddReaction_BumpsUpdatedAt(t *testing.T) {
	repo, bucketSizer, createdAt, key, reactor := reactionFixture(t)
	ctx := context.Background()

	sender := models.Participant{ID: "u-bob", Account: "bob"}
	roomID := "room-add-bumps"
	msgID := "m-add-bumps"

	require.NoError(t, repo.session.Query(
		`INSERT INTO messages_by_id (message_id, room_id, created_at, sender, msg, thread_parent_id) VALUES (?, ?, ?, ?, ?, ?)`,
		msgID, roomID, createdAt, sender, "hello", "",
	).Exec())
	require.NoError(t, repo.session.Query(
		`INSERT INTO messages_by_room (room_id, bucket, created_at, message_id, sender, msg, thread_parent_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		roomID, bucketSizer.Of(createdAt), createdAt, msgID, sender, "hello", "",
	).Exec())

	msg := &models.Message{MessageID: msgID, RoomID: roomID, CreatedAt: createdAt, Sender: sender}
	require.NoError(t, repo.AddReaction(ctx, msg, key, reactor))

	var got time.Time
	require.NoError(t, repo.session.Query(
		`SELECT updated_at FROM messages_by_id WHERE message_id = ? AND created_at = ?`,
		msgID, createdAt,
	).Scan(&got))
	assert.WithinDuration(t, reactor.ReactedAt, got, time.Second,
		"messages_by_id.updated_at must reflect the add time")

	require.NoError(t, repo.session.Query(
		`SELECT updated_at FROM messages_by_room WHERE room_id = ? AND bucket = ? AND created_at = ? AND message_id = ?`,
		roomID, bucketSizer.Of(createdAt), createdAt, msgID,
	).Scan(&got))
	assert.WithinDuration(t, reactor.ReactedAt, got, time.Second,
		"messages_by_room.updated_at must reflect the add time")
}
```

- [ ] **Step 1.1.c: Repeat the flip for the thread-reply Remove test**

Scan `reactions_integration_test.go` for `TestRepository_RemoveReaction_ThreadReply` and any other Remove-path test that asserts `updated_at` reflects `removedAt`. Flip every such assertion to expect `addedAt`. If a test does not exist for thread replies, skip — Step 1.5 verifies coverage.

- [ ] **Step 1.2: Run the integration tests — expect a RED**

```bash
cd /home/user/chat
make test-integration SERVICE=history-service
```

Expected: `TestRepository_RemoveReaction_TopLevel` (and any thread variant) now FAIL because the current code still bumps `updated_at` on Remove. `TestRepository_AddReaction_BumpsUpdatedAt` PASSES (existing code already bumps on Add).

Time-box: if integration setup takes >5min on the first run, that's normal (testcontainers pull). If it never starts, run `docker ps` to confirm Docker is up.

---

### Step 1.3: Replace `AddReaction` and `RemoveReaction` with `UnloggedBatch` (Green)

- [ ] **Step 1.3.a: Rewrite `history-service/internal/cassrepo/reactions.go`**

Open the file and replace its full contents with:

```go
package cassrepo

import (
	"context"
	"fmt"
	"time"

	"github.com/gocql/gocql"

	"github.com/hmchangw/chat/history-service/internal/models"
)

// v3 reactions storage: one map-cell per (emoji, user_account) per row.
// Writes hit messages_by_id (source of truth) and the room-or-thread mirror in
// one UnloggedBatch — transport grouping, not atomic; half-apply on coordinator
// failure is possible but the heal is natural: the canonical event will
// re-publish and downstream operations are idempotent (re-Add overwrites the
// same cell, re-Delete on a missing cell is a no-op). Precedent: pin.go.
// pinned_messages_by_room is NOT a reaction mirror — the pinned panel does not
// render reactions, so writing them there is dead work.
//
// Remove does NOT touch updated_at: no consumer reads it for reaction
// freshness (broadcast events derive timestamps from the canonical event,
// search-sync skips reactions, pagination keys off created_at, no NATS ETag
// semantics). Skipping the touch turns Remove from 4 RTTs into 1.

const (
	addReactionMsgByID   = `UPDATE messages_by_id SET reactions[?] = ?, updated_at = ? WHERE message_id = ? AND created_at = ?`
	addReactionMsgByRoom = `UPDATE messages_by_room SET reactions[?] = ?, updated_at = ? WHERE room_id = ? AND bucket = ? AND created_at = ? AND message_id = ?`
	addReactionThreadMsg = `UPDATE thread_messages_by_thread SET reactions[?] = ?, updated_at = ? WHERE thread_room_id = ? AND created_at = ? AND message_id = ?`

	removeReactionMsgByID   = `DELETE reactions[?] FROM messages_by_id WHERE message_id = ? AND created_at = ?`
	removeReactionMsgByRoom = `DELETE reactions[?] FROM messages_by_room WHERE room_id = ? AND bucket = ? AND created_at = ? AND message_id = ?`
	removeReactionThreadMsg = `DELETE reactions[?] FROM thread_messages_by_thread WHERE thread_room_id = ? AND created_at = ? AND message_id = ?`
)

// AddReaction writes one (emoji, user_account) map-cell to messages_by_id and
// the room-or-thread mirror in a single UnloggedBatch; idempotent re-writes
// overwrite the ReactorInfo.
//
//nolint:gocritic // hugeParam: reactor passed by value to match Sender/Mentions in models.Message
func (r *Repository) AddReaction(ctx context.Context, msg *models.Message, key models.ReactionKey, reactor models.ReactorInfo) error {
	if msg.ThreadParentID != "" && msg.ThreadRoomID == "" {
		return fmt.Errorf("react thread message %s: ThreadParentID %q is set but ThreadRoomID is empty", msg.MessageID, msg.ThreadParentID)
	}
	reactedAt := reactor.ReactedAt

	batch := r.session.NewBatch(gocql.UnloggedBatch).WithContext(ctx)
	batch.Query(addReactionMsgByID, key, reactor, reactedAt, msg.MessageID, msg.CreatedAt)
	if msg.ThreadParentID == "" {
		b := r.bucket.Of(msg.CreatedAt)
		batch.Query(addReactionMsgByRoom, key, reactor, reactedAt, msg.RoomID, b, msg.CreatedAt, msg.MessageID)
		if err := r.session.ExecuteBatch(batch); err != nil {
			return fmt.Errorf("add reaction on message %s in room %s via batch(messages_by_id, messages_by_room): %w", msg.MessageID, msg.RoomID, err)
		}
		return nil
	}
	batch.Query(addReactionThreadMsg, key, reactor, reactedAt, msg.ThreadRoomID, msg.CreatedAt, msg.MessageID)
	if err := r.session.ExecuteBatch(batch); err != nil {
		return fmt.Errorf("add reaction on thread message %s in thread %s via batch(messages_by_id, thread_messages_by_thread): %w", msg.MessageID, msg.ThreadRoomID, err)
	}
	return nil
}

// RemoveReaction deletes one (emoji, user_account) map-cell from messages_by_id
// and the room-or-thread mirror in a single UnloggedBatch; idempotent on an
// absent cell. updatedAt is unused — see file-level comment.
func (r *Repository) RemoveReaction(ctx context.Context, msg *models.Message, key models.ReactionKey, _ time.Time) error {
	if msg.ThreadParentID != "" && msg.ThreadRoomID == "" {
		return fmt.Errorf("unreact thread message %s: ThreadParentID %q is set but ThreadRoomID is empty", msg.MessageID, msg.ThreadParentID)
	}

	batch := r.session.NewBatch(gocql.UnloggedBatch).WithContext(ctx)
	batch.Query(removeReactionMsgByID, key, msg.MessageID, msg.CreatedAt)
	if msg.ThreadParentID == "" {
		b := r.bucket.Of(msg.CreatedAt)
		batch.Query(removeReactionMsgByRoom, key, msg.RoomID, b, msg.CreatedAt, msg.MessageID)
		if err := r.session.ExecuteBatch(batch); err != nil {
			return fmt.Errorf("remove reaction on message %s in room %s via batch(messages_by_id, messages_by_room): %w", msg.MessageID, msg.RoomID, err)
		}
		return nil
	}
	batch.Query(removeReactionThreadMsg, key, msg.ThreadRoomID, msg.CreatedAt, msg.MessageID)
	if err := r.session.ExecuteBatch(batch); err != nil {
		return fmt.Errorf("remove reaction on thread message %s in thread %s via batch(messages_by_id, thread_messages_by_thread): %w", msg.MessageID, msg.ThreadRoomID, err)
	}
	return nil
}
```

Key deltas vs original:
- Added `"github.com/gocql/gocql"` import.
- Deleted three `touchUpdatedAt*` constants.
- Both methods now build one `gocql.UnloggedBatch` and call `ExecuteBatch` once.
- `RemoveReaction`'s `updatedAt` parameter is now `_` (kept on signature for interface compatibility with `service.MessageWriter`).
- Error messages name both tables for on-call clarity on half-apply.

- [ ] **Step 1.3.b: Verify `service.MessageWriter` interface still matches**

The service-level `MessageWriter.RemoveReaction` signature must be unchanged. Check `history-service/internal/service/service.go` lines 44–46 — confirm the parameter list still says `updatedAt time.Time`. (It does today; we kept it in our new signature.)

```bash
grep -n "RemoveReaction" /home/user/chat/history-service/internal/service/service.go
```

Expected: matches the interface — `RemoveReaction(ctx context.Context, msg *models.Message, key models.ReactionKey, updatedAt time.Time) error`.

- [ ] **Step 1.4: Run integration tests — expect GREEN**

```bash
cd /home/user/chat
make test-integration SERVICE=history-service
```

Expected: all `TestRepository_*Reaction*` tests pass. The two flipped Remove tests, the new Add-bumps-updated_at regression, and the existing idempotency / pinned / thread tests must all be green.

- [ ] **Step 1.5: Run unit tests for the whole service**

```bash
cd /home/user/chat
make test SERVICE=history-service
```

Expected: pass. Service-layer reaction tests still pass because they don't observe Cassandra; they exercise the `MockMessageWriter`.

- [ ] **Step 1.6: Update `docs/specs/message-reactions.md` — Mirror consistency section**

Open `docs/specs/message-reactions.md` and find the section about mirror consistency (look for the heading containing "Mirror consistency" — heading number may have shifted). Replace the description of the 4-statement Remove sequence with the batched 2-statement story. Use this language:

> **Mirror consistency.** Each Add issues one `gocql.UnloggedBatch` containing
> `messages_by_id` plus the room-or-thread mirror; each Remove does the same
> with two `DELETE reactions[?] FROM …` statements. The batch is transport
> grouping (not atomic) — a coordinator failure can half-apply; the heal is
> automatic because re-published canonical events drive idempotent re-writes
> (Add overwrites the same map cell, Delete on a missing cell is a no-op).
> Remove does not touch `updated_at`: no downstream consumer reads it for
> reaction freshness. Precedent: `cassrepo/pin.go`.

If a "Reactions-specific retry caveat" subsection exists, tighten the prose to clarify the retry-flip wrinkle stems from the handler's read-then-decide-direction step and is unaffected by batching. If it doesn't exist, skip.

- [ ] **Step 1.7: Update `docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md` — MessageWriter section**

Find the section describing the `MessageWriter` store interface (search for "MessageWriter" heading). Add or update the `AddReaction` / `RemoveReaction` description to read:

> Both methods issue a single `gocql.UnloggedBatch` covering `messages_by_id`
> plus the room-or-thread mirror. `RemoveReaction` does **not** touch
> `updated_at`. Precedent: `history-service/internal/cassrepo/pin.go`.

- [ ] **Step 1.8: Lint and SAST**

```bash
cd /home/user/chat
make lint
make sast
```

Expected: clean. If `make sast` complains about `UnloggedBatch`, that's a false positive — but check the message: `pin.go` uses the same pattern, so unless `pin.go` is also flagged, our change shouldn't be either.

- [ ] **Step 1.9: Commit**

```bash
cd /home/user/chat
git add history-service/internal/cassrepo/reactions.go \
        history-service/internal/cassrepo/reactions_integration_test.go \
        docs/specs/message-reactions.md \
        docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md

git commit -m "$(cat <<'EOF'
cassrepo/reactions: drop updated_at touch and batch writes via UnloggedBatch

AddReaction and RemoveReaction now issue one gocql.UnloggedBatch covering
messages_by_id plus the room-or-thread mirror. Cuts RTTs:

- AddReaction: 2 → 1
- RemoveReaction: 4 → 1 (the three touchUpdatedAt* statements are dropped)

No downstream consumer reads reactions' updated_at for freshness; broadcast
events build timestamps from the canonical event (Message.UpdatedAt set by
the handler), search-sync-worker skips reactions, pagination keys off
created_at. Batch is transport grouping (not atomic); half-apply heal is
automatic via canonical event re-publish + per-cell idempotency. Precedent:
cassrepo/pin.go's UnloggedBatch over messages_by_id + pinned_messages_by_room.

Integration tests:
- Flipped Remove-side assertions: updated_at now reflects the Add (not the
  Remove). Added a regression test that Add still bumps updated_at.

Refs:
- https://github.com/hmchangw/chat/pull/258#discussion_r3366774917
EOF
)"
```

Expected: pre-commit hook runs `make lint` and `make test` and passes.

---

## Task 2: Move reaction author-notification to broadcast-worker; narrow notification-worker filter

**Conscious regression accepted (deep-review item #1):** Today the notification publish is retried via JetStream NAK on transient NATS failure. After this task, broadcast-worker swallows the publish error (room fan-out has already published; NAK would re-broadcast it). Net effect on transient NATS failure: author sees the reaction in their UI when they next open the room (data is in Cassandra) but misses the badge ping. This matches the pre-existing fail-open behavior in `publishMutation` for DM mutations (`broadcast-worker/handler.go:602–610`). User has approved this trade-off.

**Files:**
- Modify: `broadcast-worker/handler.go` — extend `handleReacted`
- Modify: `broadcast-worker/handler_test.go` — port 4 reaction tests + 1 publish-failure regression
- Modify: `notification-worker/handler.go` — delete `EventReacted` branch, `handleReaction`, `Publisher` interface, `ReactionPub`, `natsPublisher`
- Modify: `notification-worker/main.go` — drop `ReactionPub:` line; narrow `cc.FilterSubjects`
- Modify: `notification-worker/handler_test.go` — delete reaction tests + helpers
- Modify: `notification-worker/consumer_config_test.go` — update filter assertion
- Modify: `docs/specs/message-reactions.md` §10
- Modify: `docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md` §3.2/§3.3
- Modify: `docs/superpowers/specs/2026-06-04-notification-worker-system-message-allowlist-design.md` (append note)

---

### Step 2.1: Add broadcast-worker reaction-notification tests (Red)

The current `broadcast-worker/handler_test.go` covers `handleReacted` for room fan-out only (see `TestHandleReacted_ChannelRoomScopedPublish` at line 1159). Port the 4 policy tests from `notification-worker/handler_test.go` and add a publish-failure regression.

- [ ] **Step 2.1.a: Append five tests to `broadcast-worker/handler_test.go`**

Existing setup pattern (from `TestHandleReacted_ChannelRoomScopedPublish` at line 1159):
- `mockPublisher` struct (already in `handler_test.go:28`) with `.records []publishRecord`; `publishRecord{subject, data}`.
- `NewHandler(store, us, pub, keyStore, encrypt)` — `store` is `NewMockStore(ctrl)`, `us` is `NewMockUserStore(ctrl)`, `pub` is `&mockPublisher{}`, `keyStore` is `NewMockRoomKeyProvider(ctrl)`.

Place these tests right after `TestHandleReacted_MissingUpdatedAt_LogsAndDrops` (around line 1305). Mirror that file's style — explicit setup, no fixture helpers.

```go
// Reaction author-notification tests — moved from notification-worker as
// part of consolidating all reaction wire effects into broadcast-worker.
// Subject and payload are byte-identical to the prior notification-worker
// path (chat.user.{author}.notification, NotificationEvent{Type:"reaction"}).

// findPublishRecord returns the first record whose subject matches, or nil.
func findPublishRecord(records []publishRecord, subj string) *publishRecord {
	for i := range records {
		if records[i].subject == subj {
			return &records[i]
		}
	}
	return nil
}

func TestHandleReacted_Added_PublishesAuthorNotification(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	us := NewMockUserStore(ctrl)
	pub := &mockPublisher{}
	keyStore := NewMockRoomKeyProvider(ctrl)

	roomID := "r1"
	room := &model.Room{ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a"}
	store.EXPECT().GetRoom(gomock.Any(), roomID).Return(room, nil)

	reactedAt := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	evt := model.MessageEvent{
		Event:     model.EventReacted,
		SiteID:    "site-a",
		Timestamp: reactedAt.UnixMilli(),
		Message: model.Message{
			ID: "m1", RoomID: roomID, UserAccount: "bob",
			CreatedAt: reactedAt.Add(-time.Hour), UpdatedAt: &reactedAt,
		},
		ReactionDelta: &model.ReactionDelta{
			Shortcode: "thumbsup",
			Action:    model.ReactionActionAdded,
			Actor:     model.Participant{UserID: "u-alice", Account: "alice", EngName: "Alice"},
		},
	}
	data, err := json.Marshal(&evt)
	require.NoError(t, err)

	h := NewHandler(store, us, pub, keyStore, true)
	require.NoError(t, h.HandleMessage(context.Background(), data))

	// Two publishes expected: one room-scoped, one to the author.
	require.Len(t, pub.records, 2)
	notif := findPublishRecord(pub.records, subject.Notification("bob"))
	require.NotNil(t, notif, "author notification must be published on chat.user.bob.notification")
	var got model.NotificationEvent
	require.NoError(t, json.Unmarshal(notif.data, &got))
	assert.Equal(t, "reaction", got.Type)
	require.NotNil(t, got.ReactionDelta)
	assert.Equal(t, "thumbsup", got.ReactionDelta.Shortcode)
	assert.Equal(t, "alice", got.ReactionDelta.Actor.Account)
}

func TestHandleReacted_Removed_NoAuthorNotification(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	us := NewMockUserStore(ctrl)
	pub := &mockPublisher{}
	keyStore := NewMockRoomKeyProvider(ctrl)

	roomID := "r1"
	room := &model.Room{ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a"}
	store.EXPECT().GetRoom(gomock.Any(), roomID).Return(room, nil)

	reactedAt := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	evt := model.MessageEvent{
		Event: model.EventReacted, SiteID: "site-a", Timestamp: reactedAt.UnixMilli(),
		Message: model.Message{
			ID: "m1", RoomID: roomID, UserAccount: "bob",
			CreatedAt: reactedAt.Add(-time.Hour), UpdatedAt: &reactedAt,
		},
		ReactionDelta: &model.ReactionDelta{
			Shortcode: "thumbsup",
			Action:    model.ReactionActionRemoved,
			Actor:     model.Participant{Account: "alice"},
		},
	}
	data, err := json.Marshal(&evt)
	require.NoError(t, err)

	h := NewHandler(store, us, pub, keyStore, true)
	require.NoError(t, h.HandleMessage(context.Background(), data))

	assert.Nil(t, findPublishRecord(pub.records, subject.Notification("bob")),
		"Remove must not notify the author")
}

func TestHandleReacted_SelfReact_NoAuthorNotification(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	us := NewMockUserStore(ctrl)
	pub := &mockPublisher{}
	keyStore := NewMockRoomKeyProvider(ctrl)

	roomID := "r1"
	room := &model.Room{ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a"}
	store.EXPECT().GetRoom(gomock.Any(), roomID).Return(room, nil)

	reactedAt := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	evt := model.MessageEvent{
		Event: model.EventReacted, SiteID: "site-a", Timestamp: reactedAt.UnixMilli(),
		Message: model.Message{
			ID: "m1", RoomID: roomID, UserAccount: "alice",
			CreatedAt: reactedAt.Add(-time.Hour), UpdatedAt: &reactedAt,
		},
		ReactionDelta: &model.ReactionDelta{
			Shortcode: "thumbsup",
			Action:    model.ReactionActionAdded,
			Actor:     model.Participant{Account: "alice"},
		},
	}
	data, err := json.Marshal(&evt)
	require.NoError(t, err)

	h := NewHandler(store, us, pub, keyStore, true)
	require.NoError(t, h.HandleMessage(context.Background(), data))

	assert.Nil(t, findPublishRecord(pub.records, subject.Notification("alice")),
		"self-react must not notify the actor")
}

func TestHandleReacted_EmptyAuthor_NoAuthorNotification(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	us := NewMockUserStore(ctrl)
	pub := &mockPublisher{}
	keyStore := NewMockRoomKeyProvider(ctrl)

	roomID := "r1"
	room := &model.Room{ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a"}
	store.EXPECT().GetRoom(gomock.Any(), roomID).Return(room, nil)

	reactedAt := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	evt := model.MessageEvent{
		Event: model.EventReacted, SiteID: "site-a", Timestamp: reactedAt.UnixMilli(),
		Message: model.Message{
			ID: "m1", RoomID: roomID, UserAccount: "", // system message
			CreatedAt: reactedAt.Add(-time.Hour), UpdatedAt: &reactedAt,
		},
		ReactionDelta: &model.ReactionDelta{
			Shortcode: "thumbsup",
			Action:    model.ReactionActionAdded,
			Actor:     model.Participant{Account: "alice"},
		},
	}
	data, err := json.Marshal(&evt)
	require.NoError(t, err)

	h := NewHandler(store, us, pub, keyStore, true)
	require.NoError(t, h.HandleMessage(context.Background(), data))

	for _, rec := range pub.records {
		assert.NotEqual(t, subject.Notification(""), rec.subject,
			"system message (empty author) must not produce a user-notification publish")
	}
}

// Publish failure on the author notification must NOT propagate (the room
// fan-out has already gone out and is the primary effect). It must be logged.
// Use a failing publisher only on the notification subject; channel room
// publishes via the same pub, so a discriminating publisher is needed.
type partialFailPublisher struct {
	mu        sync.Mutex
	records   []publishRecord
	failSubj  string
	failErr   error
}

func (p *partialFailPublisher) Publish(_ context.Context, subj string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if subj == p.failSubj {
		return p.failErr
	}
	p.records = append(p.records, publishRecord{subject: subj, data: data})
	return nil
}

func TestHandleReacted_AuthorPublishFailure_RoomFanOutStillSucceeds(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	us := NewMockUserStore(ctrl)
	pub := &partialFailPublisher{
		failSubj: subject.Notification("bob"),
		failErr:  errors.New("nats down"),
	}
	keyStore := NewMockRoomKeyProvider(ctrl)

	roomID := "r1"
	room := &model.Room{ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a"}
	store.EXPECT().GetRoom(gomock.Any(), roomID).Return(room, nil)

	reactedAt := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	evt := model.MessageEvent{
		Event: model.EventReacted, SiteID: "site-a", Timestamp: reactedAt.UnixMilli(),
		Message: model.Message{
			ID: "m1", RoomID: roomID, UserAccount: "bob",
			CreatedAt: reactedAt.Add(-time.Hour), UpdatedAt: &reactedAt,
		},
		ReactionDelta: &model.ReactionDelta{
			Shortcode: "thumbsup",
			Action:    model.ReactionActionAdded,
			Actor:     model.Participant{Account: "alice"},
		},
	}
	data, err := json.Marshal(&evt)
	require.NoError(t, err)

	h := NewHandler(store, us, pub, keyStore, true)
	require.NoError(t, h.HandleMessage(context.Background(), data),
		"author-notify failure must not NAK the canonical event")
	require.NotNil(t, findPublishRecord(pub.records, subject.RoomEvent(roomID)),
		"room fan-out must still have published")
}
```

Imports: `sync` and `errors` are already imported in `handler_test.go`. `subject` is too. No new imports.

- [ ] **Step 2.2: Run broadcast-worker tests — expect RED on the five new tests**

```bash
cd /home/user/chat
make test SERVICE=broadcast-worker
```

Expected: the five new tests fail because `handleReacted` doesn't yet publish to the author subject. Existing tests pass.

---

### Step 2.3: Extend `handleReacted` in `broadcast-worker/handler.go` (Green)

- [ ] **Step 2.3.a: Edit `broadcast-worker/handler.go`** — replace the body of `handleReacted` (currently lines 537–578).

Find this block:

```go
	return h.publishMutation(ctx, room, model.RoomEventMessageReacted, msg.ID, &react)
}
```

Replace the `return` line with:

```go
	if err := h.publishMutation(ctx, room, model.RoomEventMessageReacted, msg.ID, &react); err != nil {
		return err
	}

	// Author notification: same subject + payload as the prior notification-worker
	// path. Only "added" actions on a different user's message notify; self-reacts
	// and system messages (empty UserAccount) are silent.
	if evt.ReactionDelta.Action != model.ReactionActionAdded {
		return nil
	}
	authorAccount := msg.UserAccount
	if authorAccount == "" || authorAccount == evt.ReactionDelta.Actor.Account {
		return nil
	}
	notif := model.NotificationEvent{
		Type:          "reaction",
		RoomID:        msg.RoomID,
		Message:       msg,
		ReactionDelta: evt.ReactionDelta,
		Timestamp:     time.Now().UTC().UnixMilli(),
	}
	data, err := json.Marshal(notif)
	if err != nil {
		// Marshal of a struct we just built shouldn't fail; if it does, the room
		// fan-out has already gone out — log and swallow.
		slog.ErrorContext(ctx, "marshal reaction author notification failed",
			"error", err,
			"messageID", msg.ID,
			"roomID", msg.RoomID,
			"siteID", evt.SiteID,
			"request_id", natsutil.RequestIDFromContext(ctx),
		)
		return nil
	}
	if err := h.pub.Publish(ctx, subject.Notification(authorAccount), data); err != nil {
		// Author-notify failure does not NAK; room fan-out is the primary effect
		// and has already published. JetStream redelivery would re-broadcast it.
		slog.ErrorContext(ctx, "publish reaction author notification failed",
			"error", err,
			"author", authorAccount,
			"messageID", msg.ID,
			"roomID", msg.RoomID,
			"siteID", evt.SiteID,
			"request_id", natsutil.RequestIDFromContext(ctx),
		)
		return nil
	}
	return nil
}
```

Verify `model.NotificationEvent` is what gets imported here — `broadcast-worker/handler.go` already imports `github.com/hmchangw/chat/pkg/model`. `subject.Notification` should already be reachable; `broadcast-worker/handler.go:20` imports `pkg/subject`. `time` is already imported. No new imports needed.

- [ ] **Step 2.3.b: Confirm `model.NotificationEvent` exists with the expected shape**

```bash
grep -n "type NotificationEvent struct" /home/user/chat/pkg/model/*.go
grep -A 10 "type NotificationEvent struct" /home/user/chat/pkg/model/*.go
```

Expected: the struct contains `Type`, `RoomID`, `Message`, `ReactionDelta`, `Timestamp` fields. If field names differ, adjust the literal accordingly.

- [ ] **Step 2.3.c: Confirm `subject.Notification` exists**

```bash
grep -n "func Notification" /home/user/chat/pkg/subject/*.go
```

Expected: `func Notification(account string) string` returns `chat.user.<account>.notification`.

- [ ] **Step 2.4: Run broadcast-worker tests — expect GREEN**

```bash
cd /home/user/chat
make test SERVICE=broadcast-worker
```

Expected: all five new tests pass alongside existing tests.

---

### Step 2.5: Strip reaction code from notification-worker (Red→Green merged)

This is pure deletion — no new tests. The TDD discipline applies in reverse: we run the existing reaction tests, delete the code they cover, watch them fail to compile, then delete them.

- [ ] **Step 2.5.a: Delete the four reaction tests and helpers from `notification-worker/handler_test.go`**

Open `notification-worker/handler_test.go` and find the comment `// --- Reaction notification path tests (separate from the push pipeline) ---` (around line 844). Delete from that comment line through the closing `}` of `TestHandleMessage_Reaction_MissingDelta_LogsAndDrops` (around line 959). That removes:
- `mockReactionPub` struct
- `reactionPubRecord` struct
- `(mockReactionPub).Publish` and `getRecords` methods
- `reactHandler` helper
- 4 test functions

- [ ] **Step 2.5.b: Edit `notification-worker/handler.go`** — delete the `EventReacted` branch.

Find lines 100–109:

```go
	// Reactions take a separate author-only path; the full push pipeline below
	// is for create events. Edits/deletes are silently dropped.
	switch evt.Event {
	case model.EventReacted:
		return h.handleReaction(ctx, &evt)
	case model.EventCreated, "":
		// fall through to push pipeline
	default:
		return nil
	}
```

Replace with:

```go
	// Edits/deletes/pins/unpins/reactions never push and are excluded at the
	// broker via FilterSubjects; the switch below is a defensive backstop for a
	// misconfigured broker. Reactions are handled entirely in broadcast-worker.
	switch evt.Event {
	case model.EventCreated, "":
		// fall through to push pipeline
	default:
		return nil
	}
```

- [ ] **Step 2.5.c: Delete `handleReaction` from `notification-worker/handler.go`**

Find lines 325–363 (the entire `handleReaction` function plus its preceding comment). Delete the whole block.

- [ ] **Step 2.5.d: Delete `Publisher` interface and `natsPublisher` wrapper from `notification-worker/handler.go`**

Find lines 22–30 (`natsPublisher` struct and its `Publish` method) and lines 46–51 (`Publisher` interface). Delete both.

Find the `HandlerDeps` struct (lines 53–64). Delete the `ReactionPub Publisher` field (line 60) so only the remaining fields stay.

- [ ] **Step 2.5.e: Audit notification-worker imports**

The `subject` import in `notification-worker/handler.go` (line 19) is still used by the push pipeline (`subject.Notification`, etc. for non-reaction notifications) — KEEP it. Audit three imports that may have become unused:

```bash
grep -n "nats\." /home/user/chat/notification-worker/handler.go
grep -n "errors\." /home/user/chat/notification-worker/handler.go
grep -n "natsutil\." /home/user/chat/notification-worker/handler.go
```

For each grep that returns no matches, delete the corresponding import line:
- no `nats.` matches → delete `"github.com/nats-io/nats.go"`
- no `errors.` matches → delete `"errors"`
- no `natsutil.` matches → delete `"github.com/hmchangw/chat/pkg/natsutil"` (the deleted `handleReaction` used `natsutil.RequestIDFromContext` + `natsutil.MarshalResponse`)

- [ ] **Step 2.5.f: Edit `notification-worker/main.go`** — drop the `ReactionPub:` line.

Find line 201:

```go
		ReactionPub:        natsPublisher{nc: nc.NatsConn()},
```

Delete that single line.

- [ ] **Step 2.5.g: Narrow `FilterSubjects` in `notification-worker/main.go`**

Find lines 350–357:

```go
func buildConsumerConfig(s stream.ConsumerSettings, siteID string) jetstream.ConsumerConfig {
	cc := stream.DurableConsumerDefaults(s)
	cc.Durable = "notification-worker"
	cc.FilterSubjects = []string{
		subject.MsgCanonicalCreated(siteID),
		subject.MsgCanonicalReacted(siteID),
	}
	return cc
}
```

Replace the `FilterSubjects` slice to drop `MsgCanonicalReacted`:

```go
func buildConsumerConfig(s stream.ConsumerSettings, siteID string) jetstream.ConsumerConfig {
	cc := stream.DurableConsumerDefaults(s)
	cc.Durable = "notification-worker"
	// FilterSubjects scopes the consumer to the only canonical event this worker
	// acts on — created (push fan-out). reacted moved to broadcast-worker;
	// updated/deleted/pinned/unpinned are excluded at the broker so they are
	// never delivered, unmarshaled, or acked here.
	cc.FilterSubjects = []string{
		subject.MsgCanonicalCreated(siteID),
	}
	return cc
}
```

- [ ] **Step 2.5.h: Update `notification-worker/consumer_config_test.go`**

Find lines 46–57 (the `"filters to created and reacted subjects only"` subtest). Replace with:

```go
	t.Run("filters to created subject only", func(t *testing.T) {
		cc := buildConsumerConfig(stream.ConsumerSettings{}, "site-a")

		// The worker only acts on created (push fan-out); reacted moved to
		// broadcast-worker as part of consolidating all reaction wire effects.
		// updated/deleted/pinned/unpinned are excluded at the broker.
		assert.ElementsMatch(t, []string{
			subject.MsgCanonicalCreated("site-a"),
		}, cc.FilterSubjects)
	})
```

- [ ] **Step 2.6: Run notification-worker tests — expect GREEN after deletions**

```bash
cd /home/user/chat
make test SERVICE=notification-worker
```

Expected: pass. The reaction tests no longer exist; the consumer_config test asserts the narrowed slice.

If a compile error mentions `Publisher` or `ReactionPub`, you missed a reference — grep:

```bash
grep -n "ReactionPub\|natsPublisher\|handleReaction\|Publisher" /home/user/chat/notification-worker/*.go
```

Fix any stragglers (in `bootstrap.go`, `integration_test.go`, etc.).

- [ ] **Step 2.7: Update docs**

Section numbers in these docs may have shifted since first authored — search by heading title, not number.

- `docs/specs/message-reactions.md` "Downstream consumers" section: replace the notification-worker row with this prose, or delete the row outright if the table is per-effect:
  > Author notification: published by **broadcast-worker** on
  > `chat.user.{authorAccount}.notification` carrying `NotificationEvent{Type:"reaction"}`.
  > Policy: action == added AND author != actor AND author != "". Failure
  > swallowed (log + return nil) since the room fan-out is the primary effect.

- `docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md` "Producers/Consumers" or "Consumer table" section: in the consumer table, move the reaction-notification responsibility from notification-worker's row to broadcast-worker's row.

- `docs/superpowers/specs/2026-06-04-notification-worker-system-message-allowlist-design.md`: append a "## Follow-up — 2026-06-10" section:
  > As of `claude/reactions-followups`, the durable consumer's `FilterSubjects` is
  > further narrowed to `{MsgCanonicalCreated}` only. `MsgCanonicalReacted`
  > moved to broadcast-worker. The PR #273 in-place narrowing pattern
  > (`CreateOrUpdateConsumer` on NATS 2.10+) covers the rollout — no cursor reset.

- [ ] **Step 2.8: Lint and SAST**

```bash
cd /home/user/chat
make lint
make sast
```

Expected: clean. If lint complains about unused imports in `notification-worker/handler.go`, finish the cleanup from Step 2.5.e.

- [ ] **Step 2.9: Commit**

```bash
cd /home/user/chat
git add broadcast-worker/handler.go \
        broadcast-worker/handler_test.go \
        notification-worker/handler.go \
        notification-worker/handler_test.go \
        notification-worker/main.go \
        notification-worker/consumer_config_test.go \
        docs/specs/message-reactions.md \
        docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md \
        docs/superpowers/specs/2026-06-04-notification-worker-system-message-allowlist-design.md

# Add testhelpers_test.go too if Step 2.1.b extended it:
git add broadcast-worker/testhelpers_test.go 2>/dev/null || true

git commit -m "$(cat <<'EOF'
broadcast-worker, notification-worker: consolidate reaction notification path

Moves the reaction author-notification publish from notification-worker into
broadcast-worker.handleReacted so all reaction wire effects (room fan-out +
author notification) live in one handler. Wire format is unchanged — same
subject (chat.user.{author}.notification) and payload
(NotificationEvent{Type:"reaction"}).

notification-worker is now exclusively the mobile-push pipeline. Its
HandlerDeps loses ReactionPub; the Publisher interface and natsPublisher
wrapper are deleted; the EventReacted switch branch and handleReaction
function are gone.

The durable consumer's FilterSubjects narrows further to {MsgCanonicalCreated}
only — same NATS 2.10+ in-place narrowing pattern as PR #273. updated/
deleted/pinned/unpinned/reacted are excluded at the broker.

Refs:
- https://github.com/hmchangw/chat/pull/258#discussion_r3367439840
- PR #273 (FilterSubjects narrowing precedent)
EOF
)"
```

---

## Task 3: Add `FindUserByAccount` singular to `UserStore`

**Files:**
- Modify: `pkg/userstore/userstore.go`
- Modify: `pkg/userstore/cache.go`
- Modify: `pkg/userstore/cache_test.go` (extend `fakeStore`, add 4 tests)
- Modify: `pkg/userstore/integration_test.go` (add 2 tests)
- Modify: `history-service/internal/service/service.go` (add method to interface)
- Modify: `history-service/internal/service/reactions.go` (swap call site)
- Modify: `history-service/internal/service/reactions_test.go` (rewrite mocks)
- Regenerate: `history-service/internal/service/mocks/mock_repository.go` (+ any other UserStore mocks)

---

### Step 3.1: Add `FindUserByAccount` to the store interface + mongoStore (Red→Green)

- [ ] **Step 3.1.a: Add a mongoStore integration test (Red)**

Open `pkg/userstore/integration_test.go`. Append:

```go
func TestMongoStore_FindUserByAccount_Hit(t *testing.T) {
	col := setupUserCollection(t)
	ctx := context.Background()

	_, err := col.InsertOne(ctx, model.User{
		ID: "u-alice", Account: "alice", SiteID: "site-a", EngName: "Alice",
	})
	require.NoError(t, err)

	store := userstore.NewMongoStore(col)
	got, err := store.FindUserByAccount(ctx, "alice")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "u-alice", got.ID)
	assert.Equal(t, "alice", got.Account)
	assert.Equal(t, "Alice", got.EngName)
}

func TestMongoStore_FindUserByAccount_NotFound(t *testing.T) {
	col := setupUserCollection(t)
	ctx := context.Background()

	store := userstore.NewMongoStore(col)
	got, err := store.FindUserByAccount(ctx, "nope")
	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorIs(t, err, userstore.ErrUserNotFound, "miss must return ErrUserNotFound")
}
```

If `setupUserCollection` doesn't exist, inspect existing tests in the same file and adapt their setup pattern (uses `testutil.MongoDB`).

- [ ] **Step 3.1.b: Run integration tests — expect RED**

```bash
cd /home/user/chat
make test-integration SERVICE=pkg/userstore || go test -tags=integration ./pkg/userstore/...
```

Expected: the two new tests fail (`FindUserByAccount` undefined).

- [ ] **Step 3.1.c: Add `FindUserByAccount` to the interface + mongoStore impl**

Edit `pkg/userstore/userstore.go`. Replace the file contents with:

```go
package userstore

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/hmchangw/chat/pkg/model"
)

// ErrUserNotFound is returned by FindUserByID / FindUserByAccount when no
// user matches the lookup key.
var ErrUserNotFound = errors.New("user not found")

// UserStore defines read operations for user records.
type UserStore interface {
	FindUserByID(ctx context.Context, id string) (*model.User, error)
	FindUserByAccount(ctx context.Context, account string) (*model.User, error)
	FindUsersByAccounts(ctx context.Context, accounts []string) ([]model.User, error)
}

// userProjection is the field set shared by FindUserByAccount and
// FindUsersByAccounts so the cache cross-populate path holds identical rows
// regardless of which method warmed it.
var userProjection = bson.M{"_id": 1, "account": 1, "siteId": 1, "engName": 1, "chineseName": 1, "employeeId": 1}

type mongoStore struct {
	col *mongo.Collection
}

// NewMongoStore returns a UserStore backed by the given MongoDB collection.
func NewMongoStore(col *mongo.Collection) UserStore {
	return &mongoStore{col: col}
}

// FindUserByID returns the user with the given ID.
// Returns ErrUserNotFound (wrapped) if no document matches.
func (s *mongoStore) FindUserByID(ctx context.Context, id string) (*model.User, error) {
	var u model.User
	if err := s.col.FindOne(ctx, bson.M{"_id": id}).Decode(&u); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("find user %s: %w", id, ErrUserNotFound)
		}
		return nil, fmt.Errorf("find user %s: %w", id, err)
	}
	return &u, nil
}

// FindUserByAccount returns the user whose account field matches.
// Returns ErrUserNotFound (wrapped) if no document matches.
func (s *mongoStore) FindUserByAccount(ctx context.Context, account string) (*model.User, error) {
	var u model.User
	opts := options.FindOne().SetProjection(userProjection)
	if err := s.col.FindOne(ctx, bson.M{"account": account}, opts).Decode(&u); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("find user by account %s: %w", account, ErrUserNotFound)
		}
		return nil, fmt.Errorf("find user by account %s: %w", account, err)
	}
	return &u, nil
}

// FindUsersByAccounts returns all users whose account field is in accounts.
func (s *mongoStore) FindUsersByAccounts(ctx context.Context, accounts []string) ([]model.User, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	filter := bson.M{"account": bson.M{"$in": accounts}}
	cursor, err := s.col.Find(ctx, filter, options.Find().SetProjection(userProjection))
	if err != nil {
		return nil, fmt.Errorf("find users by accounts: %w", err)
	}
	defer cursor.Close(ctx)
	var users []model.User
	if err := cursor.All(ctx, &users); err != nil {
		return nil, fmt.Errorf("decode users: %w", err)
	}
	return users, nil
}
```

Key changes:
- Added `FindUserByAccount` to the interface.
- Hoisted projection into shared `userProjection` var (same field set).
- Added `mongoStore.FindUserByAccount` using `FindOne` + same projection.
- `FindUsersByAccounts` now references the shared projection (DRY).

- [ ] **Step 3.1.d: Run the userstore integration tests — expect GREEN**

```bash
cd /home/user/chat
go test -tags=integration ./pkg/userstore/...
```

Expected: pass.

---

### Step 3.2: Add `FindUserByAccount` to the cache (Red→Green)

- [ ] **Step 3.2.a: Extend `fakeStore` in `pkg/userstore/cache_test.go`**

Append a method to `fakeStore`:

```go
func (f *fakeStore) FindUserByAccount(_ context.Context, account string) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byAccountCalls++
	if f.err != nil {
		return nil, f.err
	}
	if u, ok := f.usersByAccount[account]; ok {
		return u, nil
	}
	return nil, userstore.ErrUserNotFound
}
```

(`fakeStore` already counts `byAccountCalls` — this method shares the counter with `FindUsersByAccounts`, which is fine.)

- [ ] **Step 3.2.b: Add four cache tests**

Append to `cache_test.go`:

```go
func TestCache_FindUserByAccount_MissThenHit(t *testing.T) {
	store := newFakeStore(model.User{ID: "u1", Account: "alice"})
	c, err := userstore.NewCache(store, 10, time.Minute)
	require.NoError(t, err)

	got1, err := c.FindUserByAccount(context.Background(), "alice")
	require.NoError(t, err)
	require.NotNil(t, got1)
	assert.Equal(t, "u1", got1.ID)

	got2, err := c.FindUserByAccount(context.Background(), "alice")
	require.NoError(t, err)
	require.Equal(t, got1, got2, "second call must serve from cache")

	assert.Equal(t, 1, store.byAccountCalls, "second call must not hit the store")
}

func TestCache_FindUserByAccount_NotFoundIsUnwrapped(t *testing.T) {
	store := newFakeStore() // empty
	c, err := userstore.NewCache(store, 10, time.Minute)
	require.NoError(t, err)

	got, err := c.FindUserByAccount(context.Background(), "nope")
	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorIs(t, err, userstore.ErrUserNotFound, "miss must propagate ErrUserNotFound unwrapped")
}

func TestCache_FindUserByAccount_StoreErrorWrapped(t *testing.T) {
	store := newFakeStore()
	store.err = errors.New("mongo down")
	c, err := userstore.NewCache(store, 10, time.Minute)
	require.NoError(t, err)

	got, err := c.FindUserByAccount(context.Background(), "alice")
	assert.Nil(t, got)
	require.Error(t, err)
	assert.False(t, errors.Is(err, userstore.ErrUserNotFound),
		"non-not-found errors must NOT be classified as ErrUserNotFound")
	assert.Contains(t, err.Error(), "mongo down")
}

func TestCache_FindUserByAccount_CrossPopulatesByID(t *testing.T) {
	store := newFakeStore(model.User{ID: "u1", Account: "alice"})
	c, err := userstore.NewCache(store, 10, time.Minute)
	require.NoError(t, err)

	// Prime byAccount.
	_, err = c.FindUserByAccount(context.Background(), "alice")
	require.NoError(t, err)

	// FindUserByID for the same user must hit the cache (no store call).
	preByID := store.byIDCalls
	got, err := c.FindUserByID(context.Background(), "u1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, store.byIDCalls, preByID, "FindUserByID must serve from cross-populated entry")
}
```

- [ ] **Step 3.2.c: Run cache tests — expect RED**

```bash
cd /home/user/chat
go test -race ./pkg/userstore/...
```

Expected: the four new tests fail (`FindUserByAccount` undefined on Cache).

- [ ] **Step 3.2.d: Add `FindUserByAccount` to `pkg/userstore/cache.go`**

Insert after `FindUserByID` (between lines 94 and 96):

```go
// FindUserByAccount serves from the by-account LRU when hot, falls through to
// the store on miss. Singleflight key is prefixed with "account:" so it does
// not collide with FindUserByID's id-keyed group. On success populates both
// LRUs so a later FindUserByID hits the cache. ErrUserNotFound propagates
// unwrapped; missing entries are NOT negatively cached.
func (c *Cache) FindUserByAccount(ctx context.Context, account string) (*model.User, error) {
	if v, ok := c.byAccount.Get(account); ok {
		c.hits.Add(1)
		return v, nil
	}
	c.misses.Add(1)
	v, err, _ := c.sf.Do("account:"+account, func() (interface{}, error) {
		if cached, ok := c.byAccount.Get(account); ok {
			return cached, nil
		}
		u, err := c.store.FindUserByAccount(ctx, account)
		if err != nil {
			return nil, err
		}
		c.populate(u)
		return u, nil
	})
	if err != nil {
		c.loadErrs.Add(1)
		if errors.Is(err, ErrUserNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("find cached user by account %q: %w", account, err)
	}
	return v.(*model.User), nil
}
```

- [ ] **Step 3.2.e: Run cache tests — expect GREEN**

```bash
cd /home/user/chat
go test -race ./pkg/userstore/...
```

Expected: pass.

---

### Step 3.3: Widen the service-level `UserStore` interface and swap the call site (Red→Green)

- [ ] **Step 3.3.a: Update the service-level interface**

Edit `history-service/internal/service/service.go` lines 80–83:

```go
// UserStore resolves the calling user's full profile for ReactorInfo and the Participant on the canonical event.
type UserStore interface {
	FindUsersByAccounts(ctx context.Context, accounts []string) ([]pkgmodel.User, error)
}
```

Replace with:

```go
// UserStore resolves the calling user's full profile for ReactorInfo and the Participant on the canonical event.
type UserStore interface {
	FindUserByAccount(ctx context.Context, account string) (*pkgmodel.User, error)
	FindUsersByAccounts(ctx context.Context, accounts []string) ([]pkgmodel.User, error)
}
```

(Keeping `FindUsersByAccounts` because the broader `pkg/userstore.UserStore` keeps it — no callers in history-service use the batch form today, but the interface should expose what the implementation provides and what the test fixture exercises.)

Actually — since the service-level `UserStore` interface should reflect only what the *service* uses (CLAUDE.md: "Each service defines its own store interface in `store.go` with only the methods it needs"), and the service only uses `FindUserByAccount` after this change, narrow it:

```go
// UserStore resolves the calling user's full profile for ReactorInfo and the Participant on the canonical event.
type UserStore interface {
	FindUserByAccount(ctx context.Context, account string) (*pkgmodel.User, error)
}
```

Use the narrowed form. If a follow-up adds a batch consumer in history-service, it can re-add `FindUsersByAccounts` then.

- [ ] **Step 3.3.b: Edit `history-service/internal/service/reactions.go`** — swap the call site.

Find lines 53–61:

```go
	users, err := s.users.FindUsersByAccounts(c, []string{account})
	if err != nil {
		return nil, fmt.Errorf("react: resolve actor %s: %w", account, err)
	}
	if len(users) == 0 {
		slog.WarnContext(c, "react: actor not found", "account", account)
		return nil, fmt.Errorf("react: actor not found for account %s", account)
	}
	actor := users[0]
```

Replace with:

```go
	actor, err := s.users.FindUserByAccount(c, account)
	if err != nil {
		if errors.Is(err, userstore.ErrUserNotFound) {
			slog.WarnContext(c, "react: actor not found", "account", account)
			return nil, fmt.Errorf("react: actor not found for account %s", account)
		}
		return nil, fmt.Errorf("react: resolve actor %s: %w", account, err)
	}
```

Then change every subsequent `actor.X` field access — they were on a value (`model.User`), they're now on a pointer (`*model.User`). Go auto-derefs, so no changes needed at the access sites. Verify:

```bash
grep -n "actor\." /home/user/chat/history-service/internal/service/reactions.go
```

All accesses should be `actor.ID`, `actor.Account`, `actor.SiteID`, `actor.EngName`, `actor.ChineseName`. None should be `&actor` or `*actor`.

- [ ] **Step 3.3.c: Add the `userstore` import to reactions.go**

In the import block of `history-service/internal/service/reactions.go`, add:

```go
	"github.com/hmchangw/chat/pkg/userstore"
```

Keep it sorted alphabetically with the other `pkg/` imports.

- [ ] **Step 3.3.d: Regenerate mocks**

```bash
cd /home/user/chat
make generate SERVICE=history-service
```

Expected: `history-service/internal/service/mocks/mock_repository.go` is regenerated. The `MockUserStore` now has `FindUserByAccount` and no longer has `FindUsersByAccounts`.

If `make generate` regenerates other services' UserStore mocks (`broadcast-worker/mock_userstore_test.go`, `message-worker/mock_userstore_test.go`), that's expected — those mocks track the `pkg/userstore.UserStore` interface, which still has both methods.

- [ ] **Step 3.3.e: Run history-service unit tests — expect COMPILE FAILURE in `reactions_test.go`**

```bash
cd /home/user/chat
go test ./history-service/internal/service/...
```

Expected: compile errors in `reactions_test.go` — `MockUserStore.FindUsersByAccounts` no longer exists (because the service-level interface dropped it).

- [ ] **Step 3.3.f: Rewrite `history-service/internal/service/reactions_test.go` mock expectations**

There are 8 sites (from the earlier grep) that look like:

```go
f.users.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"u1"}).Return([]model.User{aliceUser()}, nil)
```

Rewrite each to:

```go
f.users.EXPECT().FindUserByAccount(gomock.Any(), "u1").Return(aliceUserPtr(), nil)
```

For the two failure-mode tests:

```go
// Was:
f.users.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"u1"}).Return(nil, errors.New("mongo down"))
// Becomes:
f.users.EXPECT().FindUserByAccount(gomock.Any(), "u1").Return(nil, errors.New("mongo down"))
```

```go
// Was:
f.users.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"u1"}).Return(nil, nil)
// (the "actor not found" test — returned empty slice without an error)
// Becomes:
f.users.EXPECT().FindUserByAccount(gomock.Any(), "u1").Return(nil, userstore.ErrUserNotFound)
```

- [ ] **Step 3.3.g: Add `aliceUserPtr()` helper in `reactions_test.go`**

Locate `aliceUser()` (the existing value helper) and add next to it:

```go
func aliceUserPtr() *model.User {
	u := aliceUser()
	return &u
}
```

- [ ] **Step 3.3.h: Add the `userstore` import to `reactions_test.go`**

```go
	"github.com/hmchangw/chat/pkg/userstore"
```

- [ ] **Step 3.3.i: Run history-service tests — expect GREEN**

```bash
cd /home/user/chat
make test SERVICE=history-service
```

Expected: pass.

---

### Step 3.4: Whole-repo verification

- [ ] **Step 3.4.a: Build everything**

```bash
cd /home/user/chat
go build ./...
```

Expected: clean. If `broadcast-worker` / `message-worker` / `room-service` fail to build because they don't yet implement `FindUserByAccount` on their UserStore mocks — but the production code in those services uses `pkg/userstore.UserStore`, which we already extended in Step 3.1.c. The auto-regenerated mocks from Step 3.3.d should cover them. If a mock is stale, rerun `make generate` (without `SERVICE=`) to regenerate all.

```bash
cd /home/user/chat
make generate
```

- [ ] **Step 3.4.b: Run all unit tests with race**

```bash
cd /home/user/chat
go test -race ./...
```

Expected: pass.

- [ ] **Step 3.4.c: Lint and SAST**

```bash
cd /home/user/chat
make lint
make sast
```

Expected: clean.

- [ ] **Step 3.4.d: Confirm no remaining single-element `FindUsersByAccounts` call sites**

```bash
grep -rn 'FindUsersByAccounts.*\[\]string{[^,}]*}' /home/user/chat \
  --include='*.go' \
  --exclude-dir=mocks \
  --exclude='*_test.go'
```

Expected: no matches. (Test files may legitimately use single-element slices for table-driven cases; we only police production code.)

- [ ] **Step 3.5: Commit**

```bash
cd /home/user/chat
git add pkg/userstore/userstore.go \
        pkg/userstore/cache.go \
        pkg/userstore/cache_test.go \
        pkg/userstore/integration_test.go \
        history-service/internal/service/service.go \
        history-service/internal/service/reactions.go \
        history-service/internal/service/reactions_test.go \
        history-service/internal/service/mocks/mock_repository.go

# Any other auto-regenerated UserStore mocks
git add broadcast-worker/mock_userstore_test.go message-worker/mock_userstore_test.go 2>/dev/null || true

git commit -m "$(cat <<'EOF'
userstore: add FindUserByAccount singular; use it in the reaction handler

The reaction handler was wrapping a 1-element batch (FindUsersByAccounts(
[]string{account}) + len()==0 + users[0]) to fetch one user by account.
Adds a dedicated singular method that returns *model.User and signals miss
via the existing ErrUserNotFound sentinel — same shape as FindUserByID.

Changes:
- pkg/userstore.UserStore gains FindUserByAccount; mongoStore implements
  it with FindOne + the shared userProjection.
- pkg/userstore.Cache.FindUserByAccount adds singleflight on the
  "account:"-prefixed key (separate group from FindUserByID's id key) and
  cross-populates both LRUs via the existing populate() helper.
- history-service service-level UserStore narrows to just
  FindUserByAccount (CLAUDE.md: "Each service defines its own store
  interface in store.go with only the methods it needs").
- The reaction handler swaps to errors.Is(err, ErrUserNotFound) branching.

Perf delta vs the prior 1-element FindUsersByAccounts is essentially zero —
Mongo's planner rewrites a single-element $in to an equality match. This
change ships on API-clarity and error-semantic grounds.

Refs:
- https://github.com/hmchangw/chat/pull/258#discussion_r3366804706
EOF
)"
```

---

## Pre-push verification

- [ ] **Step P.1: Confirm three commits on the branch**

```bash
cd /home/user/chat
git log --oneline origin/main..HEAD
```

Expected (top is HEAD):

```
<hash> userstore: add FindUserByAccount singular; use it in the reaction handler
<hash> broadcast-worker, notification-worker: consolidate reaction notification path
<hash> cassrepo/reactions: drop updated_at touch and batch writes via UnloggedBatch
21ba4d2e docs: spec for reactions post-merge follow-up changes
```

- [ ] **Step P.2: Rebase onto current origin/main**

```bash
cd /home/user/chat
git fetch origin main
git rebase origin/main
```

Resolve any conflicts. If the spec commit's docs changed under our feet, prefer the upstream version and re-apply any spec edits.

- [ ] **Step P.3: Full repo test + lint + sast one more time post-rebase**

```bash
cd /home/user/chat
go test -race ./...
make test-integration
make lint
make sast
```

Expected: all clean.

- [ ] **Step P.4: Delete `docs/reviews/` artifacts if any**

```bash
ls /home/user/chat/docs/reviews/ 2>/dev/null && rm -rf /home/user/chat/docs/reviews/*
```

Per CLAUDE.md "Before Committing": delete every file under `docs/reviews/` from the branch before push.

- [ ] **Step P.5: Push**

```bash
cd /home/user/chat
git push -u origin claude/reactions-followups
```

If the remote rejects because of a force-push concern after rebase, retry with `--force-with-lease`:

```bash
git push -u origin claude/reactions-followups --force-with-lease
```

Use plain `--force` only with explicit user authorization.

- [ ] **Step P.6: Do NOT open a PR**

Per environment instructions: "Do NOT create a pull request unless the user explicitly asks for one." Stop here and report the pushed branch + commit hashes back to the user.

- [ ] **Step P.7: Draft reply to mliu33 (Item 3 thread)**

The spec's Item 3 §"Reply to reviewer's perf framing" has the drafted text. Surface it to the user for review before posting. Do NOT post the GitHub comment without user approval.

---

## Out of scope (for clarity)

- CQL `//` comment syntax fixes (CodeRabbit nitpick — pre-existing, separate task).
- `slog.Error` for nil-delta in `CanonicalDedupID` (CodeRabbit nitpick — redundant with handler-side logs).
- Moving service interfaces from `service.go` to `store.go` (CodeRabbit nitpick — cross-service refactor for a separate PR).
- NFC test in `pkg/emoji/emoji_test.go` byte-identical literals — if trivially co-located with Item 3's commit, bundle; otherwise punt.

See `docs/superpowers/specs/2026-06-10-reactions-post-merge-followups-design.md` "Out of scope" section.
