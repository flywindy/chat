# Message Quoting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add reply-with-quote support to the chat pipeline. Users send `quotedParentMessageId` on `SendMessageRequest`; gatekeeper resolves the parent via history-service's existing `GetMessageByID` RPC, projects it into `cassandra.QuotedParentMessage`, and embeds the snapshot on the canonical `MessageEvent`. message-worker persists it to all relevant Cassandra tables.

**Architecture:** Gatekeeper does a synchronous NATS request/reply against `history-service` to fetch the parent (no Cassandra dependency added to gatekeeper). On any failure (not found, timeout, forbidden, thread-context mismatch, etc.) the entire send fails — gatekeeper replies the error to the client, acks the JetStream message, and does NOT publish to MESSAGES_CANONICAL. (The original soft-fail design was reversed during PR review — see the spec's "Hard-fail policy" section.) The snapshot rides on the canonical event so broadcast-worker and notification-worker see it without re-fetching.

**Tech Stack:** Go 1.25 · NATS / JetStream · Cassandra (gocql) · MongoDB · `caarlos0/env` · `go.uber.org/mock` · `stretchr/testify`

**Reference spec:** `docs/superpowers/specs/2026-04-27-message-quoting-design.md`

---

## Task 0: Extend `cassandra.QuotedParentMessage` UDT with thread fields

**Goal:** Add `thread_parent_id` and `thread_parent_created_at` to the `QuotedParentMessage` UDT — Go struct, single-source-of-truth doc, local-dev DDL, and the hand-rolled UDT in history-service's testcontainers helper. Cover the new fields in the existing `cassandra` package round-trip tests. Lands first because every later task references the new fields in fixtures or projections.

**Files:**
- Modify: `pkg/model/cassandra/message.go`
- Modify: `pkg/model/cassandra/message_test.go`
- Modify: `docs/cassandra_message_model.md`
- Modify: `docker-local/cassandra/init/06-udt-quoted_parent_message.cql`
- Modify: `history-service/internal/cassrepo/integration_test.go`

- [ ] **Step 1: Extend `TestQuotedParentMessage_JSON` to populate and assert the new fields**

In `pkg/model/cassandra/message_test.go`, replace `TestQuotedParentMessage_JSON` (currently at line 79) with:

```go
func TestQuotedParentMessage_JSON(t *testing.T) {
	threadParent := time.Date(2026, 1, 14, 9, 0, 0, 0, time.UTC)
	q := QuotedParentMessage{
		MessageID:             "m1",
		RoomID:                "r1",
		Sender:                Participant{ID: "u1", Account: "alice"},
		CreatedAt:             time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
		Msg:                   "original message",
		Mentions:              []Participant{{ID: "u2", Account: "bob"}},
		Attachments:           [][]byte{[]byte("file1")},
		MessageLink:           "https://chat.example.com/r1/m1",
		ThreadParentID:        "thread-parent-uuid",
		ThreadParentCreatedAt: &threadParent,
	}
	got := roundTrip(t, q)
	assert.Equal(t, "thread-parent-uuid", got.ThreadParentID)
	require.NotNil(t, got.ThreadParentCreatedAt)
	assert.Equal(t, threadParent, *got.ThreadParentCreatedAt)
}
```

Also update `TestQuotedParentMessage_JSON_Minimal` (currently at line 93) to assert the new fields are zero/nil when omitted:

```go
func TestQuotedParentMessage_JSON_Minimal(t *testing.T) {
	q := QuotedParentMessage{
		MessageID: "m1",
		RoomID:    "r1",
		Sender:    Participant{ID: "u1"},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	got := roundTrip(t, q)
	assert.Empty(t, got.Msg)
	assert.Nil(t, got.Mentions)
	assert.Nil(t, got.Attachments)
	assert.Empty(t, got.MessageLink)
	assert.Empty(t, got.ThreadParentID)
	assert.Nil(t, got.ThreadParentCreatedAt)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `make test SERVICE=pkg/model/cassandra`

Expected: FAIL — `QuotedParentMessage` has no field `ThreadParentID` or `ThreadParentCreatedAt`. Compilation error.

- [ ] **Step 3: Add the two fields to the Go struct**

In `pkg/model/cassandra/message.go`, replace the `QuotedParentMessage` struct (currently lines 46-56) with:

```go
// QuotedParentMessage maps to the Cassandra "QuotedParentMessage" UDT.
//
// ThreadParentID and ThreadParentCreatedAt capture the parent message's
// thread context (when the parent was itself a thread reply). Used by
// gatekeeper to enforce same-thread-context quoting and by clients to
// render thread-aware quote previews.
type QuotedParentMessage struct {
	MessageID             string        `json:"messageId"                       cql:"message_id"`
	RoomID                string        `json:"roomId"                          cql:"room_id"`
	Sender                Participant   `json:"sender"                          cql:"sender"`
	CreatedAt             time.Time     `json:"createdAt"                       cql:"created_at"`
	Msg                   string        `json:"msg,omitempty"                   cql:"msg"`
	Mentions              []Participant `json:"mentions,omitempty"              cql:"mentions"`
	Attachments           [][]byte      `json:"attachments,omitempty"           cql:"attachments"`
	MessageLink           string        `json:"messageLink,omitempty"           cql:"message_link"`
	ThreadParentID        string        `json:"threadParentId,omitempty"        cql:"thread_parent_id"`
	ThreadParentCreatedAt *time.Time    `json:"threadParentCreatedAt,omitempty" cql:"thread_parent_created_at"`
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `make test SERVICE=pkg/model/cassandra`

Expected: PASS — both extended tests, plus `TestMessage_JSON` and `TestMessage_JSON_Minimal` (which embed a `QuotedParentMessage`).

- [ ] **Step 5: Update single-source-of-truth doc**

In `docs/cassandra_message_model.md`, replace the existing `QuotedParentMessage` UDT block (lines 46-56) with:

```cql
CREATE TYPE IF NOT EXISTS "QuotedParentMessage"(
  message_id               TEXT,
  room_id                  TEXT,
  sender                   FROZEN<"Participant">,
  created_at               TIMESTAMP,
  msg                      TEXT,
  mentions                 SET<FROZEN<"Participant">>,
  attachments              LIST<BLOB>,
  message_link             TEXT,
  thread_parent_id         TEXT,
  thread_parent_created_at TIMESTAMP
);
```

- [ ] **Step 6: Update local-dev DDL**

Replace `docker-local/cassandra/init/06-udt-quoted_parent_message.cql` with:

```cql
CREATE TYPE IF NOT EXISTS chat."QuotedParentMessage" (
  message_id               TEXT,
  room_id                  TEXT,
  sender                   FROZEN<"Participant">,
  created_at               TIMESTAMP,
  msg                      TEXT,
  mentions                 SET<FROZEN<"Participant">>,
  attachments              LIST<BLOB>,
  message_link             TEXT,
  thread_parent_id         TEXT,
  thread_parent_created_at TIMESTAMP
);
```

- [ ] **Step 7: Sync the hand-rolled UDT in history-service's testcontainers helper**

In `history-service/internal/cassrepo/integration_test.go`, locate the `CREATE TYPE IF NOT EXISTS chat_test."QuotedParentMessage"` statement (currently at line 42) and replace it with:

```go
		`CREATE TYPE IF NOT EXISTS chat_test."QuotedParentMessage" (message_id TEXT, room_id TEXT, sender FROZEN<"Participant">, created_at TIMESTAMP, msg TEXT, mentions SET<FROZEN<"Participant">>, attachments LIST<BLOB>, message_link TEXT, thread_parent_id TEXT, thread_parent_created_at TIMESTAMP)`,
```

- [ ] **Step 8: Run the full unit test suite to confirm nothing else broke**

Run: `make test`

Expected: PASS — appending fields to the struct (with `omitempty`) is non-breaking. No other packages should regress.

- [ ] **Step 9: Run lint**

Run: `make lint`

Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add pkg/model/cassandra/message.go pkg/model/cassandra/message_test.go docs/cassandra_message_model.md docker-local/cassandra/init/06-udt-quoted_parent_message.cql history-service/internal/cassrepo/integration_test.go
git commit -m "feat(cassandra): add thread-context fields to QuotedParentMessage UDT

Adds thread_parent_id and thread_parent_created_at to the
QuotedParentMessage UDT — Go struct, single-source-of-truth doc, and the
local-dev DDL. Also syncs the hand-rolled UDT statement in
history-service's testcontainers integration test.

These fields capture the parent message's thread context (set when the
parent is itself a thread reply). Used by message-gatekeeper to enforce
same-thread-context quoting and by clients to render thread-aware quote
previews.

Production environments require a separate ALTER TYPE migration before
deploying the new gatekeeper — see the design spec for the runbook."
```

---

## Task 1: Add quote fields to wire model

**Goal:** Extend `model.SendMessageRequest` with the new client-supplied field, and `model.Message` with the snapshot field. Verify JSON round-trip.

**Files:**
- Modify: `pkg/model/message.go`
- Modify: `pkg/model/model_test.go`

- [ ] **Step 1: Write failing tests for the new fields**

Append the following two test functions to the bottom of `pkg/model/model_test.go` (just before the `roundTrip` helper at line 1248):

```go
func TestSendMessageRequest_QuotedParentMessageID_JSON(t *testing.T) {
	t.Run("with quotedParentMessageId", func(t *testing.T) {
		r := model.SendMessageRequest{
			ID:                    "msg-uuid-1",
			Content:               "great point!",
			RequestID:             "req-1",
			QuotedParentMessageID: "parent-msg-uuid",
		}
		data, err := json.Marshal(&r)
		require.NoError(t, err)

		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		assert.Equal(t, "parent-msg-uuid", raw["quotedParentMessageId"])

		var dst model.SendMessageRequest
		require.NoError(t, json.Unmarshal(data, &dst))
		assert.Equal(t, "parent-msg-uuid", dst.QuotedParentMessageID)
	})

	t.Run("quotedParentMessageId omitted when empty", func(t *testing.T) {
		r := model.SendMessageRequest{
			ID:        "msg-uuid-1",
			Content:   "hello",
			RequestID: "req-1",
		}
		data, err := json.Marshal(&r)
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		_, present := raw["quotedParentMessageId"]
		assert.False(t, present, "quotedParentMessageId should be omitted when empty")
	})
}

func TestMessage_QuotedParentMessage_JSON(t *testing.T) {
	parentTS := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	threadParentTS := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("populated snapshot round-trips", func(t *testing.T) {
		m := model.Message{
			ID: "m1", RoomID: "r1", UserID: "u1", UserAccount: "alice",
			Content:   "great point!",
			CreatedAt: now,
			QuotedParentMessage: &cassandra.QuotedParentMessage{
				MessageID: "parent-msg-uuid",
				RoomID:    "r1",
				Sender: cassandra.Participant{
					ID:      "u-bob",
					EngName: "Bob Chen",
					Account: "bob",
				},
				CreatedAt:             parentTS,
				Msg:                   "the original message",
				Mentions:              []cassandra.Participant{{ID: "u-carol", Account: "carol", EngName: "Carol Lee"}},
				MessageLink:           "http://localhost:3000/r1/parent-msg-uuid",
				ThreadParentID:        "thread-parent-uuid",
				ThreadParentCreatedAt: &threadParentTS,
			},
		}
		data, err := json.Marshal(&m)
		require.NoError(t, err)

		var dst model.Message
		require.NoError(t, json.Unmarshal(data, &dst))
		require.NotNil(t, dst.QuotedParentMessage)
		assert.Equal(t, "parent-msg-uuid", dst.QuotedParentMessage.MessageID)
		assert.Equal(t, "r1", dst.QuotedParentMessage.RoomID)
		assert.Equal(t, "the original message", dst.QuotedParentMessage.Msg)
		assert.Equal(t, "bob", dst.QuotedParentMessage.Sender.Account)
		assert.Equal(t, "Bob Chen", dst.QuotedParentMessage.Sender.EngName)
		assert.Equal(t, parentTS, dst.QuotedParentMessage.CreatedAt.UTC())
		assert.Equal(t, "http://localhost:3000/r1/parent-msg-uuid", dst.QuotedParentMessage.MessageLink)
		require.Len(t, dst.QuotedParentMessage.Mentions, 1)
		assert.Equal(t, "carol", dst.QuotedParentMessage.Mentions[0].Account)
		assert.Equal(t, "thread-parent-uuid", dst.QuotedParentMessage.ThreadParentID)
		require.NotNil(t, dst.QuotedParentMessage.ThreadParentCreatedAt)
		assert.Equal(t, threadParentTS, dst.QuotedParentMessage.ThreadParentCreatedAt.UTC())
	})

	t.Run("quotedParentMessage omitted when nil", func(t *testing.T) {
		m := model.Message{
			ID: "m1", RoomID: "r1", UserID: "u1", UserAccount: "alice",
			Content:   "hello",
			CreatedAt: now,
		}
		data, err := json.Marshal(&m)
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		_, present := raw["quotedParentMessage"]
		assert.False(t, present, "quotedParentMessage should be omitted when nil")
	})
}
```

Add the new import to the top of the file (in the existing `import` block):

```go
"github.com/hmchangw/chat/pkg/model/cassandra"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test SERVICE=pkg/model`

Expected: FAIL — `model.Message` has no field `QuotedParentMessage`; `model.SendMessageRequest` has no field `QuotedParentMessageID`. Compilation error.

- [ ] **Step 3: Add the new fields to `pkg/model/message.go`**

Replace the entire file with:

```go
package model

import (
	"time"

	"github.com/hmchangw/chat/pkg/model/cassandra"
)

type Message struct {
	ID                           string                         `json:"id"                                     bson:"_id"`
	RoomID                       string                         `json:"roomId"                                 bson:"roomId"`
	UserID                       string                         `json:"userId"                                 bson:"userId"`
	UserAccount                  string                         `json:"userAccount"                            bson:"userAccount"`
	Content                      string                         `json:"content"                                bson:"content"`
	Mentions                     []Participant                  `json:"mentions,omitempty"                     bson:"mentions,omitempty"`
	CreatedAt                    time.Time                      `json:"createdAt"                              bson:"createdAt"`
	ThreadParentMessageID        string                         `json:"threadParentMessageId,omitempty"        bson:"threadParentMessageId,omitempty"`
	ThreadParentMessageCreatedAt *time.Time                     `json:"threadParentMessageCreatedAt,omitempty" bson:"threadParentMessageCreatedAt,omitempty"`
	TShow                        bool                           `json:"tshow,omitempty"                        bson:"tshow,omitempty"`
	Type                         string                         `json:"type,omitempty"                         bson:"type,omitempty"`
	SysMsgData                   []byte                         `json:"sysMsgData,omitempty"                   bson:"sysMsgData,omitempty"`
	QuotedParentMessage          *cassandra.QuotedParentMessage `json:"quotedParentMessage,omitempty"          bson:"quotedParentMessage,omitempty"`
}

type SendMessageRequest struct {
	ID                           string `json:"id"`
	Content                      string `json:"content"`
	RequestID                    string `json:"requestId"`
	ThreadParentMessageID        string `json:"threadParentMessageId,omitempty"`
	ThreadParentMessageCreatedAt *int64 `json:"threadParentMessageCreatedAt,omitempty"`
	QuotedParentMessageID        string `json:"quotedParentMessageId,omitempty"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `make test SERVICE=pkg/model`

Expected: PASS — all model tests including the two new ones.

- [ ] **Step 5: Run full test suite to confirm no regressions**

Run: `make test`

Expected: PASS — no other package broke. (Adding an `omitempty` field is a non-breaking change.)

- [ ] **Step 6: Commit**

```bash
git add pkg/model/message.go pkg/model/model_test.go
git commit -m "feat(pkg/model): add quoted-parent fields to message + send-request

Adds QuotedParentMessageID to SendMessageRequest (client input) and
QuotedParentMessage *cassandra.QuotedParentMessage to Message (snapshot
that rides on the canonical event). Both fields are omitempty so existing
clients and downstream services see no behavior change."
```

---

## Task 2: Add `subject.MsgGet` concrete-subject helper

**Goal:** Provide a concrete-subject builder so gatekeeper can publish on history-service's `GetMessageByID` subject. Sits next to the existing `MsgGetPattern` (which is the natsrouter pattern variant the receiver uses).

**Files:**
- Modify: `pkg/subject/subject.go`
- Modify: `pkg/subject/subject_test.go`

- [ ] **Step 1: Write the failing test**

Open `pkg/subject/subject_test.go` and add a new test case to the `tests` table inside `TestSubjectBuilders` (the slice starts at line 11). Insert this entry just before the closing `}` of the slice (before line 81):

```go
{"MsgGet", subject.MsgGet("alice", "r1", "site-a"),
	"chat.user.alice.request.room.r1.site-a.msg.get"},
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test SERVICE=pkg/subject`

Expected: FAIL — `subject.MsgGet undefined`. Compilation error.

- [ ] **Step 3: Add the helper to `pkg/subject/subject.go`**

Insert the following function immediately after `MsgGetPattern` (currently at line 276):

```go
// MsgGet returns the concrete subject for issuing a GetMessageByID request to
// history-service on behalf of a given user/room. Pair with MsgGetPattern,
// which is the natsrouter pattern used by history-service to register the
// handler.
func MsgGet(account, roomID, siteID string) string {
	return fmt.Sprintf("chat.user.%s.request.room.%s.%s.msg.get", account, roomID, siteID)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `make test SERVICE=pkg/subject`

Expected: PASS — including the new `MsgGet` row.

- [ ] **Step 5: Commit**

```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "feat(pkg/subject): add MsgGet concrete-subject helper

Pairs with the existing MsgGetPattern (used by history-service's
natsrouter Register). Callers like message-gatekeeper need the concrete
form to publish a request on behalf of a specific user+room+site."
```

---

## Task 3: Add `ParentMessageFetcher` interface, regen mocks, wire handler branch + tests

> **Post-PR-review note:** the soft-fail design captured below was reversed during PR review — `resolveQuoteSnapshot` now returns `(*snapshot, error)` and every failure mode (RPC error, thread-context mismatch, buggy `(nil, nil)` fetcher) returns a wrapped error. The existing `HandleJetStreamMsg` path then replies the error to the sender's response subject and acks. The five "quote dropped, message ships" test cases were renamed to `request fails …` and use `wantErr: true`. The body of this task below shows the original step-by-step instructions for historical reference — see `docs/superpowers/specs/2026-04-27-message-quoting-design.md` ("Hard-fail policy" section) and `message-gatekeeper/handler.go::resolveQuoteSnapshot` for the current behavior.

**Goal:** Define the consumer-side interface message-gatekeeper will use to fetch the quoted parent. Add a new branch to `processMessage` that, when `req.QuotedParentMessageID` is non-empty, calls the fetcher and either embeds the snapshot on the message or soft-fails (warn + drop). Cover the new path with table-driven tests using a generated mock.

**Files:**
- Modify: `message-gatekeeper/store.go`
- Modify: `message-gatekeeper/handler.go`
- Modify: `message-gatekeeper/handler_test.go`
- Generated (do not edit by hand): `message-gatekeeper/mock_store_test.go`

- [ ] **Step 1: Add the interface to `message-gatekeeper/store.go`**

Replace the entire file with:

```go
package main

import (
	"context"
	"errors"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/model/cassandra"
)

//go:generate mockgen -destination=mock_store_test.go -package=main . Store,ParentMessageFetcher

// errNotSubscribed is returned when the user is not subscribed to the room.
var errNotSubscribed = errors.New("not subscribed")

type Store interface {
	GetSubscription(ctx context.Context, account, roomID string) (*model.Subscription, error)
}

// ParentMessageFetcher resolves a quoted parent message into a snapshot
// suitable for embedding on the new message's canonical event. Implementations
// should treat any failure (not found, RPC timeout, forbidden, etc.) as a
// reason to return an error — the handler soft-fails on every error and ships
// the message without the quote.
type ParentMessageFetcher interface {
	FetchQuotedParent(ctx context.Context, account, roomID, siteID, messageID string) (*cassandra.QuotedParentMessage, error)
}
```

- [ ] **Step 2: Regenerate mocks**

Run: `make generate SERVICE=message-gatekeeper`

Expected: `mock_store_test.go` is regenerated and now contains a `MockParentMessageFetcher` type alongside the existing `MockStore`.

Verify with: `grep -c "MockParentMessageFetcher" message-gatekeeper/mock_store_test.go`

Expected: a number ≥ 1.

- [ ] **Step 3: Write the failing handler tests**

Append the following test function to the bottom of `message-gatekeeper/handler_test.go`:

```go
func TestHandler_ProcessMessage_WithQuote(t *testing.T) {
	validID := uuid.New().String()
	validContent := "great point!"
	validSiteID := "site-a"
	validRoomID := "room-1"
	validAccount := "alice"
	parentMessageID := uuid.New().String()

	sub := &model.Subscription{
		User:   model.SubscriptionUser{ID: "u1", Account: validAccount},
		RoomID: validRoomID,
		Roles:  []model.Role{model.RoleMember},
	}

	threadID := "thread-parent-uuid"
	threadParentTS := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

	// mainRoomSnapshot represents a parent that lives in the main room
	// (not inside any thread): ThreadParentID == "".
	mainRoomSnapshot := &cassandra.QuotedParentMessage{
		MessageID:   parentMessageID,
		RoomID:      validRoomID,
		Sender:      cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt:   time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		Msg:         "the original message",
		MessageLink: "http://localhost:3000/" + validRoomID + "/" + parentMessageID,
	}

	// inThreadSnapshot represents a parent that is itself a reply inside thread T.
	inThreadSnapshot := &cassandra.QuotedParentMessage{
		MessageID:             parentMessageID,
		RoomID:                validRoomID,
		Sender:                cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt:             time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		Msg:                   "a reply inside thread T",
		MessageLink:           "http://localhost:3000/" + validRoomID + "/" + parentMessageID,
		ThreadParentID:        threadID,
		ThreadParentCreatedAt: &threadParentTS,
	}

	// inDifferentThreadSnapshot is in thread T'.
	inDifferentThreadSnapshot := &cassandra.QuotedParentMessage{
		MessageID:             parentMessageID,
		RoomID:                validRoomID,
		Sender:                cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt:             time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		Msg:                   "a reply inside thread T'",
		MessageLink:           "http://localhost:3000/" + validRoomID + "/" + parentMessageID,
		ThreadParentID:        "different-thread-uuid",
		ThreadParentCreatedAt: &threadParentTS,
	}

	tests := []struct {
		name          string
		buildData     func() []byte
		setupStore    func(s *MockStore)
		setupFetcher  func(f *MockParentMessageFetcher)
		setupPub      func() (publishFunc, *[]publishedMsg)
		wantErr       bool
		assertMessage func(t *testing.T, msg model.Message)
	}{
		{
			name: "main-room msg quoting main-room parent — snapshot embedded",
			buildData: func() []byte {
				req := model.SendMessageRequest{
					ID:                    validID,
					Content:               validContent,
					QuotedParentMessageID: parentMessageID,
				}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(mainRoomSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				require.NotNil(t, msg.QuotedParentMessage)
				assert.Equal(t, parentMessageID, msg.QuotedParentMessage.MessageID)
				assert.Equal(t, "the original message", msg.QuotedParentMessage.Msg)
				assert.Equal(t, "bob", msg.QuotedParentMessage.Sender.Account)
				assert.Equal(t, mainRoomSnapshot.MessageLink, msg.QuotedParentMessage.MessageLink)
				assert.Empty(t, msg.QuotedParentMessage.ThreadParentID)
			},
		},
		{
			name: "fetcher error — quote dropped, message still published",
			buildData: func() []byte {
				req := model.SendMessageRequest{
					ID:                    validID,
					Content:               validContent,
					QuotedParentMessageID: parentMessageID,
				}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(nil, fmt.Errorf("history response error: not found"))
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				assert.Nil(t, msg.QuotedParentMessage, "quote must be dropped on fetcher error")
				assert.Equal(t, validContent, msg.Content, "message body must still ship")
			},
		},
		{
			name: "thread T msg quoting another reply in thread T — snapshot embedded",
			buildData: func() []byte {
				return []byte(fmt.Sprintf(
					`{"id":%q,"content":%q,"requestId":"req-1","threadParentMessageId":%q,"threadParentMessageCreatedAt":%d,"quotedParentMessageId":%q}`,
					validID, validContent, threadID, threadParentTS.UnixMilli(), parentMessageID,
				))
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(inThreadSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				assert.Equal(t, threadID, msg.ThreadParentMessageID)
				require.NotNil(t, msg.QuotedParentMessage)
				assert.Equal(t, parentMessageID, msg.QuotedParentMessage.MessageID)
				assert.Equal(t, threadID, msg.QuotedParentMessage.ThreadParentID)
			},
		},
		{
			name: "main-room msg quoting a thread reply — quote dropped (cross-thread)",
			buildData: func() []byte {
				req := model.SendMessageRequest{
					ID:                    validID,
					Content:               validContent,
					QuotedParentMessageID: parentMessageID,
				}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(inThreadSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				assert.Nil(t, msg.QuotedParentMessage, "thread-context mismatch must drop the quote")
				assert.Equal(t, validContent, msg.Content)
			},
		},
		{
			name: "thread T msg quoting main-room parent — quote dropped (strict)",
			buildData: func() []byte {
				return []byte(fmt.Sprintf(
					`{"id":%q,"content":%q,"requestId":"req-1","threadParentMessageId":%q,"threadParentMessageCreatedAt":%d,"quotedParentMessageId":%q}`,
					validID, validContent, threadID, threadParentTS.UnixMilli(), parentMessageID,
				))
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(mainRoomSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				assert.Equal(t, threadID, msg.ThreadParentMessageID)
				assert.Nil(t, msg.QuotedParentMessage, "thread-T cannot quote main-room parent under strict rule")
			},
		},
		{
			name: "thread T msg quoting reply in different thread T' — quote dropped",
			buildData: func() []byte {
				return []byte(fmt.Sprintf(
					`{"id":%q,"content":%q,"requestId":"req-1","threadParentMessageId":%q,"threadParentMessageCreatedAt":%d,"quotedParentMessageId":%q}`,
					validID, validContent, threadID, threadParentTS.UnixMilli(), parentMessageID,
				))
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(inDifferentThreadSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				assert.Equal(t, threadID, msg.ThreadParentMessageID)
				assert.Nil(t, msg.QuotedParentMessage, "different thread context must drop the quote")
			},
		},
		{
			name: "no quote field — fetcher not called",
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: validContent}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				// no EXPECT — gomock will fail the test if FetchQuotedParent is called
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				assert.Nil(t, msg.QuotedParentMessage)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := NewMockStore(ctrl)
			fetcher := NewMockParentMessageFetcher(ctrl)
			tc.setupStore(store)
			tc.setupFetcher(fetcher)

			pub, publishedPtr := tc.setupPub()

			h := &Handler{
				store:         store,
				publish:       pub,
				siteID:        validSiteID,
				parentFetcher: fetcher,
			}

			data, err := h.processMessage(context.Background(), validAccount, validRoomID, validSiteID, tc.buildData())

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, data)

			var msg model.Message
			require.NoError(t, json.Unmarshal(data, &msg))
			tc.assertMessage(t, msg)

			// Also verify the snapshot reaches the canonical event.
			require.NotNil(t, publishedPtr)
			require.Len(t, *publishedPtr, 1)
			var evt model.MessageEvent
			require.NoError(t, json.Unmarshal((*publishedPtr)[0].data, &evt))
			if msg.QuotedParentMessage != nil {
				require.NotNil(t, evt.Message.QuotedParentMessage)
				assert.Equal(t, msg.QuotedParentMessage.MessageID, evt.Message.QuotedParentMessage.MessageID)
			} else {
				assert.Nil(t, evt.Message.QuotedParentMessage)
			}
		})
	}
}
```

Add the `cassandra` import to the existing `import` block at the top of `handler_test.go`:

```go
"github.com/hmchangw/chat/pkg/model/cassandra"
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `make test SERVICE=message-gatekeeper`

Expected: FAIL — `Handler` has no field `parentFetcher`; `processMessage` does nothing with the new field. Compilation error.

- [ ] **Step 5: Wire the handler — add field, constructor arg, and the new branch**

In `message-gatekeeper/handler.go`:

(a) Add the `cassandra` import to the existing `import` block:

```go
"github.com/hmchangw/chat/pkg/model/cassandra"
```

(b) Add the field to the `Handler` struct (replace lines 42-47):

```go
type Handler struct {
	store         Store
	publish       publishFunc
	reply         replyFunc
	siteID        string
	parentFetcher ParentMessageFetcher
}
```

(c) Update `NewHandler` (replace lines 49-52):

```go
// NewHandler constructs a new Handler with the given dependencies.
func NewHandler(store Store, publish publishFunc, reply replyFunc, siteID string, parentFetcher ParentMessageFetcher) *Handler {
	return &Handler{store: store, publish: publish, reply: reply, siteID: siteID, parentFetcher: parentFetcher}
}
```

(d) Add the quote-resolution branch inside `processMessage`. Find this block (around line 160):

```go
	msg := model.Message{
		ID:                           req.ID,
		RoomID:                       roomID,
		UserID:                       sub.User.ID,
		UserAccount:                  sub.User.Account,
		Content:                      req.Content,
		CreatedAt:                    now,
		ThreadParentMessageID:        req.ThreadParentMessageID,
		ThreadParentMessageCreatedAt: threadParentCreatedAt,
	}
```

Replace it with:

```go
	var quotedSnapshot *cassandra.QuotedParentMessage
	if req.QuotedParentMessageID != "" {
		snap, err := h.parentFetcher.FetchQuotedParent(ctx, account, roomID, siteID, req.QuotedParentMessageID)
		switch {
		case err != nil:
			slog.Warn("quoted parent unavailable, dropping quote",
				"quotedParentMessageId", req.QuotedParentMessageID,
				"roomId", roomID,
				"error", err)
		case snap.ThreadParentID != req.ThreadParentMessageID:
			// Strict same-conversation-context rule: main↔main, threadT↔threadT.
			// Anything else (cross-thread, main↔thread, thread↔main) drops the quote.
			slog.Warn("quoted parent has different thread context, dropping quote",
				"quotedParentMessageId", req.QuotedParentMessageID,
				"roomId", roomID,
				"newMessageThread", req.ThreadParentMessageID,
				"parentThread", snap.ThreadParentID)
		default:
			quotedSnapshot = snap
		}
	}

	msg := model.Message{
		ID:                           req.ID,
		RoomID:                       roomID,
		UserID:                       sub.User.ID,
		UserAccount:                  sub.User.Account,
		Content:                      req.Content,
		CreatedAt:                    now,
		ThreadParentMessageID:        req.ThreadParentMessageID,
		ThreadParentMessageCreatedAt: threadParentCreatedAt,
		QuotedParentMessage:          quotedSnapshot,
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `make test SERVICE=message-gatekeeper`

Expected: PASS — both the existing `TestHandler_ProcessMessage` table and the new `TestHandler_ProcessMessage_WithQuote` table all pass.

- [ ] **Step 7: Run the full test suite to confirm nothing else broke**

Run: `make test`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add message-gatekeeper/store.go message-gatekeeper/handler.go message-gatekeeper/handler_test.go message-gatekeeper/mock_store_test.go
git commit -m "feat(gatekeeper): add ParentMessageFetcher and quote branch in handler

Adds the consumer-side ParentMessageFetcher interface and a branch in
processMessage that calls it when SendMessageRequest carries
quotedParentMessageId. On success the snapshot is embedded on
model.Message.QuotedParentMessage; on any error the quote is dropped
silently (warn-logged) and the message ships as today.

The fetcher implementation lands in a follow-up commit."
```

---

## Task 4: Implement the history-service fetcher (`fetcher_history.go`)

**Goal:** Provide the production implementation of `ParentMessageFetcher` that issues a synchronous NATS request to history-service's `GetMessageByID`, decodes the natsrouter response envelope, and projects the returned `cassandra.Message` into a `cassandra.QuotedParentMessage`. Test against an in-process NATS server.

**Files:**
- Create: `message-gatekeeper/fetcher_history.go`
- Create: `message-gatekeeper/fetcher_history_test.go`

- [ ] **Step 1: Write the failing test**

Create `message-gatekeeper/fetcher_history_test.go` with the following content:

```go
package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/model/cassandra"
	"github.com/hmchangw/chat/pkg/subject"
)

func startTestNATS(t *testing.T) *otelnats.Conn {
	t.Helper()
	opts := &natsserver.Options{Port: -1}
	ns, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second), "nats server did not become ready")
	t.Cleanup(ns.Shutdown)

	nc, err := otelnats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

func TestHistoryParentFetcher_FetchQuotedParent(t *testing.T) {
	const (
		account   = "alice"
		roomID    = "room-1"
		siteID    = "site-a"
		messageID = "parent-msg-uuid"
		baseURL   = "http://localhost:3000"
	)
	parentCreatedAt := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	threadParentCreatedAt := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

	t.Run("happy path — returns projected snapshot with thread context and messageLink", func(t *testing.T) {
		nc := startTestNATS(t)

		parent := cassandra.Message{
			MessageID:             messageID,
			RoomID:                roomID,
			Sender:                cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
			CreatedAt:             parentCreatedAt,
			Msg:                   "a reply inside thread T",
			Mentions:              []cassandra.Participant{{ID: "u-carol", Account: "carol", EngName: "Carol Lee"}},
			ThreadParentID:        "thread-parent-uuid",
			ThreadParentCreatedAt: &threadParentCreatedAt,
		}

		// Stand up a stub responder on the exact subject the fetcher should publish on.
		_, err := nc.Subscribe(subject.MsgGet(account, roomID, siteID), func(ctx context.Context, msg *otelnats.Msg) {
			data, _ := json.Marshal(parent)
			_ = msg.Respond(data)
		})
		require.NoError(t, err)

		fetcher := newHistoryParentFetcher(nc, baseURL)
		got, err := fetcher.FetchQuotedParent(context.Background(), account, roomID, siteID, messageID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, messageID, got.MessageID)
		assert.Equal(t, roomID, got.RoomID)
		assert.Equal(t, "a reply inside thread T", got.Msg)
		assert.Equal(t, "bob", got.Sender.Account)
		assert.Equal(t, parentCreatedAt, got.CreatedAt.UTC())
		require.Len(t, got.Mentions, 1)
		assert.Equal(t, "carol", got.Mentions[0].Account)
		assert.Equal(t, baseURL+"/"+roomID+"/"+messageID, got.MessageLink)
		assert.Equal(t, "thread-parent-uuid", got.ThreadParentID)
		require.NotNil(t, got.ThreadParentCreatedAt)
		assert.Equal(t, threadParentCreatedAt, got.ThreadParentCreatedAt.UTC())
	})

	t.Run("history returns natsrouter error envelope — returns error", func(t *testing.T) {
		nc := startTestNATS(t)

		_, err := nc.Subscribe(subject.MsgGet(account, roomID, siteID), func(ctx context.Context, msg *otelnats.Msg) {
			data, _ := json.Marshal(model.ErrorResponse{Error: "message not found"})
			_ = msg.Respond(data)
		})
		require.NoError(t, err)

		fetcher := newHistoryParentFetcher(nc, baseURL)
		got, err := fetcher.FetchQuotedParent(context.Background(), account, roomID, siteID, messageID)
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "message not found")
	})

	t.Run("no responder — returns error", func(t *testing.T) {
		nc := startTestNATS(t)
		// Intentionally no subscriber: nc.Request must fail with "no responders".

		fetcher := newHistoryParentFetcher(nc, baseURL)
		got, err := fetcher.FetchQuotedParent(context.Background(), account, roomID, siteID, messageID)
		require.Error(t, err)
		assert.Nil(t, got)
	})
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test SERVICE=message-gatekeeper`

Expected: FAIL — `newHistoryParentFetcher undefined`. Compilation error.

- [ ] **Step 3: Create `message-gatekeeper/fetcher_history.go`**

Create the file with the following content:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/model/cassandra"
	"github.com/hmchangw/chat/pkg/subject"
)

// historyRequestTimeout bounds the synchronous request to history-service.
// Matches the standard NATS Go client default. Hardcoded — promote to env
// var only if ops needs to tune it.
const historyRequestTimeout = 2 * time.Second

// historyParentFetcher implements ParentMessageFetcher by issuing a NATS
// request to history-service's GetMessageByID handler. The base URL is used
// to build messageLink; it is injected so unit tests can supply any value.
type historyParentFetcher struct {
	nc          *otelnats.Conn
	chatBaseURL string
}

func newHistoryParentFetcher(nc *otelnats.Conn, chatBaseURL string) *historyParentFetcher {
	return &historyParentFetcher{nc: nc, chatBaseURL: chatBaseURL}
}

// getMessageByIDRequest mirrors history-service's request shape on the wire.
// We duplicate the small struct rather than cross-import a service-internal
// package.
type getMessageByIDRequest struct {
	MessageID string `json:"messageId"`
}

// FetchQuotedParent issues a NATS request to history-service's GetMessageByID
// handler at subject.MsgGet(account, roomID, siteID). On a successful reply,
// projects the returned cassandra.Message into a cassandra.QuotedParentMessage
// snapshot. Any error (NATS timeout, no responder, natsrouter error envelope,
// unmarshal failure) is wrapped and returned — the caller treats every error
// as a soft-fail signal.
func (f *historyParentFetcher) FetchQuotedParent(
	ctx context.Context,
	account, roomID, siteID, messageID string,
) (*cassandra.QuotedParentMessage, error) {
	reqBytes, err := json.Marshal(getMessageByIDRequest{MessageID: messageID})
	if err != nil {
		return nil, fmt.Errorf("marshal GetMessageByID request: %w", err)
	}

	subj := subject.MsgGet(account, roomID, siteID)
	msg, err := f.nc.Request(ctx, subj, reqBytes, historyRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("history request: %w", err)
	}

	// natsrouter encodes errors as {"error":"...","code":"..."}. Detect that
	// shape first; a successful Message has no top-level "error" field, so
	// this can't false-positive on a real response.
	var errEnv model.ErrorResponse
	if jsonErr := json.Unmarshal(msg.Data, &errEnv); jsonErr == nil && errEnv.Error != "" {
		return nil, fmt.Errorf("history response error: %s", errEnv.Error)
	}

	var parent cassandra.Message
	if err := json.Unmarshal(msg.Data, &parent); err != nil {
		return nil, fmt.Errorf("unmarshal parent message: %w", err)
	}

	return &cassandra.QuotedParentMessage{
		MessageID:             parent.MessageID,
		RoomID:                parent.RoomID,
		Sender:                parent.Sender,
		CreatedAt:             parent.CreatedAt,
		Msg:                   parent.Msg,
		Mentions:              parent.Mentions,
		MessageLink:           fmt.Sprintf("%s/%s/%s", f.chatBaseURL, parent.RoomID, parent.MessageID),
		ThreadParentID:        parent.ThreadParentID,
		ThreadParentCreatedAt: parent.ThreadParentCreatedAt,
	}, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `make test SERVICE=message-gatekeeper`

Expected: PASS — all three subtests of `TestHistoryParentFetcher_FetchQuotedParent` plus the existing `TestHandler_ProcessMessage*` suites.

- [ ] **Step 5: Run lint to make sure the new file conforms**

Run: `make lint`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add message-gatekeeper/fetcher_history.go message-gatekeeper/fetcher_history_test.go
git commit -m "feat(gatekeeper): implement history-service quoted-parent fetcher

Issues nc.Request(ctx, subject.MsgGet(...), ..., 2s) and projects the
returned cassandra.Message into a cassandra.QuotedParentMessage snapshot,
populating MessageLink from the injected chatBaseURL. Distinguishes a
natsrouter error envelope (returns error) from a real Message reply
(returns snapshot). Tested against an in-process NATS server."
```

---

## Task 5: Wire fetcher into `main.go`, add `CHAT_BASE_URL` config

**Goal:** Add the new env var, construct the fetcher with the existing NATS connection, and pass it to `NewHandler`. Update the local docker-compose so the dev stack runs with the new variable.

**Files:**
- Modify: `message-gatekeeper/main.go`
- Modify: `message-gatekeeper/deploy/docker-compose.yml`

- [ ] **Step 1: Add the config field to `message-gatekeeper/main.go`**

Replace the entire `config` struct (lines 23-30) with:

```go
type config struct {
	NatsURL       string `env:"NATS_URL,required"`
	NatsCredsFile string `env:"NATS_CREDS_FILE" envDefault:""`
	SiteID        string `env:"SITE_ID,required"`
	MongoURI      string `env:"MONGO_URI,required"`
	MongoDB       string `env:"MONGO_DB"        envDefault:"chat"`
	MaxWorkers    int    `env:"MAX_WORKERS"     envDefault:"100"`
	ChatBaseURL   string `env:"CHAT_BASE_URL"   envDefault:"http://localhost:3000"`
}
```

- [ ] **Step 2: Construct the fetcher and pass it to `NewHandler`**

Find this block (around line 67-74):

```go
	store := NewMongoStore(db)
	pub := func(ctx context.Context, subj string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
		return js.Publish(ctx, subj, data, opts...)
	}
	reply := func(ctx context.Context, subj string, data []byte) error {
		return nc.Publish(ctx, subj, data)
	}
	handler := NewHandler(store, pub, reply, cfg.SiteID)
```

Replace it with:

```go
	store := NewMongoStore(db)
	pub := func(ctx context.Context, subj string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
		return js.Publish(ctx, subj, data, opts...)
	}
	reply := func(ctx context.Context, subj string, data []byte) error {
		return nc.Publish(ctx, subj, data)
	}
	parentFetcher := newHistoryParentFetcher(nc, cfg.ChatBaseURL)
	handler := NewHandler(store, pub, reply, cfg.SiteID, parentFetcher)
```

- [ ] **Step 3: Update `message-gatekeeper/deploy/docker-compose.yml`**

Replace the `environment` block (under the `message-gatekeeper` service) with:

```yaml
    environment:
      - NATS_URL=nats://nats:4222
      - NATS_CREDS_FILE=/etc/nats/backend.creds
      - SITE_ID=site-local
      - MONGO_URI=mongodb://mongodb:27017
      - MONGO_DB=chat
      - CHAT_BASE_URL=http://localhost:3000
```

- [ ] **Step 4: Verify the binary still builds**

Run: `make build SERVICE=message-gatekeeper`

Expected: a binary at `bin/message-gatekeeper`. No compile errors.

- [ ] **Step 5: Run the full unit test suite**

Run: `make test`

Expected: PASS. (`main.go` is exercised at compile time only — no dedicated unit test.)

- [ ] **Step 6: Run lint**

Run: `make lint`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add message-gatekeeper/main.go message-gatekeeper/deploy/docker-compose.yml
git commit -m "feat(gatekeeper): wire history-parent fetcher + CHAT_BASE_URL config

Adds CHAT_BASE_URL env var (defaults to http://localhost:3000 to match
the chat-frontend dev port) and constructs historyParentFetcher with the
existing NATS connection, passing it into NewHandler. The local docker
compose file picks up the new variable explicitly so the dev stack does
not silently fall back to the default."
```

---

## Task 6: Persist `quoted_parent_message` in message-worker INSERTs + handler test

**Goal:** Extend `SaveMessage` and `SaveThreadMessage` in `message-worker/store_cassandra.go` to bind the `quoted_parent_message` UDT column. Verify via the handler test that a snapshot on the canonical event reaches the store unchanged.

**Files:**
- Modify: `message-worker/store_cassandra.go`
- Modify: `message-worker/handler_test.go`

- [ ] **Step 1: Write the failing handler test**

Append the following test function to the bottom of `message-worker/handler_test.go`:

```go
func TestHandler_ProcessMessage_Quote(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	user := &model.User{
		ID:          "u-1",
		Account:     "alice",
		SiteID:      "site-a",
		EngName:     "Alice Wang",
		ChineseName: "愛麗絲",
	}
	expectedSender := cassParticipant{
		ID:          user.ID,
		EngName:     user.EngName,
		CompanyName: user.ChineseName,
		Account:     "alice",
	}

	snapshot := &cassandra.QuotedParentMessage{
		MessageID:   "parent-msg-uuid",
		RoomID:      "r1",
		Sender:      cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt:   time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC),
		Msg:         "the original message",
		MessageLink: "http://localhost:3000/r1/parent-msg-uuid",
	}

	quotedMsg := model.Message{
		ID:                  "msg-quote-1",
		RoomID:              "r1",
		UserID:              "u-1",
		UserAccount:         "alice",
		Content:             "great point!",
		CreatedAt:           now,
		QuotedParentMessage: snapshot,
	}
	quotedEvt := model.MessageEvent{Message: quotedMsg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	quotedData, _ := json.Marshal(quotedEvt)

	t.Run("quote snapshot reaches SaveMessage unchanged", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := NewMockStore(ctrl)
		userStore := NewMockUserStore(ctrl)
		threadStore := NewMockThreadStore(ctrl)

		userStore.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
		store.EXPECT().
			SaveMessage(gomock.Any(), &quotedMsg, &expectedSender, "site-a").
			DoAndReturn(func(_ context.Context, m *model.Message, _ *cassParticipant, _ string) error {
				require.NotNil(t, m.QuotedParentMessage, "QuotedParentMessage must be forwarded")
				assert.Equal(t, "parent-msg-uuid", m.QuotedParentMessage.MessageID)
				assert.Equal(t, "the original message", m.QuotedParentMessage.Msg)
				return nil
			})

		h := NewHandler(store, userStore, threadStore)
		err := h.processMessage(context.Background(), quotedData)
		require.NoError(t, err)
	})
}
```

Add the `cassandra` import to the existing `import` block at the top of `handler_test.go`:

```go
"github.com/hmchangw/chat/pkg/model/cassandra"
```

- [ ] **Step 2: Run the test to verify it passes already (compile-level)**

Run: `make test SERVICE=message-worker`

Expected: PASS — the new test passes today because the existing `SaveMessage` mock is invoked with whatever `*model.Message` the handler builds, and the handler does not strip `QuotedParentMessage`. (The pointer flows through.) **This test guards against future regressions** — make sure it actually runs and passes by inspecting the test name in the output.

If for any reason the test fails at this point, stop and investigate before proceeding.

- [ ] **Step 3: Write the failing intent for the store change**

Open `message-worker/store_cassandra.go` and locate the `SaveMessage` function (currently at line 63). The two INSERT statements do NOT bind `quoted_parent_message`. We will extend both to bind `msg.QuotedParentMessage`. Without integration tests against a real Cassandra, the unit-test red phase here is effectively a code review check: confirm by reading the file that today's INSERTs omit the column.

Run: `grep -n "quoted_parent_message" message-worker/store_cassandra.go`

Expected: no output (column is not bound today).

- [ ] **Step 4: Extend `SaveMessage` to bind `quoted_parent_message`**

In `message-worker/store_cassandra.go`, replace the `SaveMessage` function (currently lines 63-81) with:

```go
// SaveMessage inserts msg into both messages_by_room and messages_by_id.
// updated_at is set to msg.CreatedAt (equals created_at on first insert — not yet edited).
// If either insert fails the error is returned immediately; JetStream will redeliver the message.
func (s *CassandraStore) SaveMessage(ctx context.Context, msg *model.Message, sender *cassParticipant, siteID string) error {
	if err := s.cassSession.Query(
		`INSERT INTO messages_by_room (room_id, created_at, message_id, sender, msg, site_id, updated_at, mentions, type, sys_msg_data, tshow, quoted_parent_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.RoomID, msg.CreatedAt, msg.ID, sender, msg.Content, siteID, msg.CreatedAt, toMentionSet(msg.Mentions), msg.Type, msg.SysMsgData, msg.TShow, msg.QuotedParentMessage,
	).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("insert messages_by_room %s: %w", msg.ID, err)
	}

	if err := s.cassSession.Query(
		`INSERT INTO messages_by_id (message_id, created_at, room_id, sender, msg, site_id, updated_at, mentions, type, sys_msg_data, tshow, quoted_parent_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.CreatedAt, msg.RoomID, sender, msg.Content, siteID, msg.CreatedAt, toMentionSet(msg.Mentions), msg.Type, msg.SysMsgData, msg.TShow, msg.QuotedParentMessage,
	).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("insert messages_by_id %s: %w", msg.ID, err)
	}

	return nil
}
```

- [ ] **Step 5: Extend `SaveThreadMessage` to bind `quoted_parent_message`**

In `message-worker/store_cassandra.go`, replace the `SaveThreadMessage` function (currently lines 88-115) with:

```go
// SaveThreadMessage inserts a thread reply into messages_by_id and thread_messages_by_room,
// then increments tcount on the parent message in both messages_by_id and messages_by_room.
// threadRoomID is the ID of the ThreadRoom document that anchors this thread (created or
// fetched by handleThreadRoomAndSubscriptions before this call).
// If any operation fails the error is returned immediately; JetStream will redeliver the message.
func (s *CassandraStore) SaveThreadMessage(ctx context.Context, msg *model.Message, sender *cassParticipant, siteID string, threadRoomID string) error {
	if err := s.cassSession.Query(
		`INSERT INTO messages_by_id
		 (message_id, created_at, room_id, sender, msg, site_id, updated_at, mentions,
		  thread_room_id, thread_parent_id, thread_parent_created_at, type, sys_msg_data, tshow, quoted_parent_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.CreatedAt, msg.RoomID, sender, msg.Content, siteID, msg.CreatedAt, toMentionSet(msg.Mentions),
		threadRoomID, msg.ThreadParentMessageID, msg.ThreadParentMessageCreatedAt, msg.Type, msg.SysMsgData, msg.TShow, msg.QuotedParentMessage,
	).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("insert messages_by_id %s: %w", msg.ID, err)
	}

	if err := s.cassSession.Query(
		`INSERT INTO thread_messages_by_room
		 (room_id, thread_room_id, created_at, message_id, thread_parent_id, sender, msg, site_id, updated_at, mentions, type, sys_msg_data, quoted_parent_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.RoomID, threadRoomID, msg.CreatedAt, msg.ID, msg.ThreadParentMessageID,
		sender, msg.Content, siteID, msg.CreatedAt, toMentionSet(msg.Mentions), msg.Type, msg.SysMsgData, msg.QuotedParentMessage,
	).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("insert thread_messages_by_room %s: %w", msg.ID, err)
	}

	if err := s.incrementParentTcount(ctx, msg); err != nil {
		return err
	}

	return nil
}
```

- [ ] **Step 6: Verify the unit tests still pass**

Run: `make test SERVICE=message-worker`

Expected: PASS — including the new `TestHandler_ProcessMessage_Quote` and all existing tests.

- [ ] **Step 7: Run lint**

Run: `make lint`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add message-worker/store_cassandra.go message-worker/handler_test.go
git commit -m "feat(message-worker): persist quoted_parent_message UDT on insert

Extends SaveMessage and SaveThreadMessage to bind msg.QuotedParentMessage
into the existing quoted_parent_message column on messages_by_room,
messages_by_id, and thread_messages_by_room. gocql marshals nil as a null
UDT, so messages without a quote write null and existing behavior is
preserved. Adds a handler test that asserts the snapshot reaches the
store unchanged from the canonical event."
```

---

## Task 7: Integration test — round-trip `quoted_parent_message` through Cassandra

**Goal:** Prove via testcontainers that a populated `*cassandra.QuotedParentMessage` written by `SaveMessage` (and `SaveThreadMessage`) reads back intact from Cassandra. Also verifies the open question from the spec — that gocql marshals a nil pointer as a null UDT.

**Files:**
- Modify: `message-worker/integration_test.go`

- [ ] **Step 1: Extend the test schema in `setupCassandra` to include the UDT and column**

Open `message-worker/integration_test.go` and locate the `stmts` slice inside `setupCassandra` (currently starting at line 40).

Insert the following CREATE TYPE statement just after the `Participant` UDT statement (currently at line 42), as the next element of `stmts`:

```go
		`CREATE TYPE IF NOT EXISTS chat_test."QuotedParentMessage" (message_id TEXT, room_id TEXT, sender FROZEN<"Participant">, created_at TIMESTAMP, msg TEXT, mentions SET<FROZEN<"Participant">>, attachments LIST<BLOB>, message_link TEXT, thread_parent_id TEXT, thread_parent_created_at TIMESTAMP)`,
```

Then add `quoted_parent_message FROZEN<"QuotedParentMessage">,` as a new column inside each of the three table definitions (`messages_by_room`, `messages_by_id`, `thread_messages_by_room`). Insert it right after the `sys_msg_data BLOB,` line in each table.

For example, the `messages_by_room` table (currently lines 43-56) should become:

```go
		`CREATE TABLE IF NOT EXISTS chat_test.messages_by_room (
			room_id               TEXT,
			created_at            TIMESTAMP,
			message_id            TEXT,
			sender                FROZEN<"Participant">,
			msg                   TEXT,
			site_id               TEXT,
			updated_at            TIMESTAMP,
			mentions              SET<FROZEN<"Participant">>,
			tcount                INT,
			type                  TEXT,
			sys_msg_data          BLOB,
			quoted_parent_message FROZEN<"QuotedParentMessage">,
			PRIMARY KEY ((room_id), created_at, message_id)
		) WITH CLUSTERING ORDER BY (created_at DESC, message_id DESC)`,
```

Apply the same `quoted_parent_message FROZEN<"QuotedParentMessage">,` addition (placed after `sys_msg_data BLOB,`) to `messages_by_id` and `thread_messages_by_room`.

- [ ] **Step 2: Add a new round-trip test for `SaveMessage` with a populated snapshot**

Append the following test function to the bottom of `message-worker/integration_test.go`:

```go
func TestCassandraStore_SaveMessage_WithQuotedParent(t *testing.T) {
	cassSession := setupCassandra(t)
	store := NewCassandraStore(cassSession)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	parentCreatedAt := now.Add(-time.Hour).Truncate(time.Millisecond)
	threadParentCreatedAt := now.Add(-2 * time.Hour).Truncate(time.Millisecond)

	sender := &cassParticipant{
		ID: "u-1", EngName: "Alice Wang", CompanyName: "愛麗絲", Account: "alice",
	}
	snapshot := &cassandra.QuotedParentMessage{
		MessageID: "parent-msg-uuid",
		RoomID:    "r-1",
		Sender:    cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt: parentCreatedAt,
		Msg:       "the original message",
		Mentions: []cassandra.Participant{
			{ID: "u-carol", Account: "carol", EngName: "Carol Lee"},
		},
		MessageLink:           "http://localhost:3000/r-1/parent-msg-uuid",
		ThreadParentID:        "thread-parent-uuid",
		ThreadParentCreatedAt: &threadParentCreatedAt,
	}
	msg := &model.Message{
		ID:                  "m-quote-1",
		RoomID:              "r-1",
		UserID:              "u-1",
		UserAccount:         "alice",
		Content:             "great point!",
		CreatedAt:           now,
		QuotedParentMessage: snapshot,
	}

	require.NoError(t, store.SaveMessage(ctx, msg, sender, "site-a"))

	t.Run("messages_by_room round-trips QuotedParentMessage including thread context", func(t *testing.T) {
		var got cassandra.QuotedParentMessage
		err := cassSession.Query(
			`SELECT quoted_parent_message FROM messages_by_room WHERE room_id = ? AND created_at = ? AND message_id = ?`,
			"r-1", now, "m-quote-1",
		).Scan(&got)
		require.NoError(t, err)
		assert.Equal(t, "parent-msg-uuid", got.MessageID)
		assert.Equal(t, "r-1", got.RoomID)
		assert.Equal(t, "the original message", got.Msg)
		assert.Equal(t, "bob", got.Sender.Account)
		assert.Equal(t, "Bob Chen", got.Sender.EngName)
		assert.Equal(t, parentCreatedAt, got.CreatedAt.UTC().Truncate(time.Millisecond))
		assert.Equal(t, "http://localhost:3000/r-1/parent-msg-uuid", got.MessageLink)
		require.Len(t, got.Mentions, 1)
		assert.Equal(t, "carol", got.Mentions[0].Account)
		assert.Equal(t, "thread-parent-uuid", got.ThreadParentID)
		require.NotNil(t, got.ThreadParentCreatedAt)
		assert.Equal(t, threadParentCreatedAt, got.ThreadParentCreatedAt.UTC().Truncate(time.Millisecond))
	})

	t.Run("messages_by_id round-trips QuotedParentMessage including thread context", func(t *testing.T) {
		var got cassandra.QuotedParentMessage
		err := cassSession.Query(
			`SELECT quoted_parent_message FROM messages_by_id WHERE message_id = ? AND created_at = ?`,
			"m-quote-1", now,
		).Scan(&got)
		require.NoError(t, err)
		assert.Equal(t, "parent-msg-uuid", got.MessageID)
		assert.Equal(t, "the original message", got.Msg)
		assert.Equal(t, "thread-parent-uuid", got.ThreadParentID)
		require.NotNil(t, got.ThreadParentCreatedAt)
		assert.Equal(t, threadParentCreatedAt, got.ThreadParentCreatedAt.UTC().Truncate(time.Millisecond))
	})
}

func TestCassandraStore_SaveMessage_NilQuotedParent(t *testing.T) {
	cassSession := setupCassandra(t)
	store := NewCassandraStore(cassSession)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	sender := &cassParticipant{ID: "u-1", Account: "alice"}
	msg := &model.Message{
		ID:          "m-no-quote",
		RoomID:      "r-1",
		UserID:      "u-1",
		UserAccount: "alice",
		Content:     "plain message",
		CreatedAt:   now,
		// QuotedParentMessage intentionally nil — verifies gocql marshals nil
		// pointer as a null UDT (the open question from the spec).
	}

	require.NoError(t, store.SaveMessage(ctx, msg, sender, "site-a"))

	var got *cassandra.QuotedParentMessage
	err := cassSession.Query(
		`SELECT quoted_parent_message FROM messages_by_id WHERE message_id = ? AND created_at = ?`,
		"m-no-quote", now,
	).Scan(&got)
	require.NoError(t, err)
	assert.Nil(t, got, "nil pointer must round-trip as null UDT")
}

func TestCassandraStore_SaveThreadMessage_WithQuotedParent(t *testing.T) {
	cassSession := setupCassandra(t)
	store := NewCassandraStore(cassSession)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	parentCreatedAt := now.Add(-time.Hour).Truncate(time.Millisecond)

	sender := &cassParticipant{ID: "u-1", Account: "alice"}
	snapshot := &cassandra.QuotedParentMessage{
		MessageID: "parent-msg-uuid",
		RoomID:    "r-1",
		Sender:    cassandra.Participant{ID: "u-bob", Account: "bob"},
		CreatedAt: parentCreatedAt,
		Msg:       "original",
	}
	msg := &model.Message{
		ID:                    "m-thread-quote",
		RoomID:                "r-1",
		UserID:                "u-1",
		UserAccount:           "alice",
		Content:               "thread reply with quote",
		CreatedAt:             now,
		ThreadParentMessageID: "thread-parent-uuid",
		QuotedParentMessage:   snapshot,
	}

	const threadRoomID = "tr-quote-1"
	require.NoError(t, store.SaveThreadMessage(ctx, msg, sender, "site-a", threadRoomID))

	t.Run("thread_messages_by_room round-trips QuotedParentMessage", func(t *testing.T) {
		var got cassandra.QuotedParentMessage
		err := cassSession.Query(
			`SELECT quoted_parent_message FROM thread_messages_by_room WHERE room_id = ? AND thread_room_id = ? AND created_at = ? AND message_id = ?`,
			"r-1", threadRoomID, now, "m-thread-quote",
		).Scan(&got)
		require.NoError(t, err)
		assert.Equal(t, "parent-msg-uuid", got.MessageID)
		assert.Equal(t, "original", got.Msg)
	})

	t.Run("messages_by_id round-trips QuotedParentMessage for thread message", func(t *testing.T) {
		var got cassandra.QuotedParentMessage
		err := cassSession.Query(
			`SELECT quoted_parent_message FROM messages_by_id WHERE message_id = ? AND created_at = ?`,
			"m-thread-quote", now,
		).Scan(&got)
		require.NoError(t, err)
		assert.Equal(t, "parent-msg-uuid", got.MessageID)
	})
}
```

Add the `cassandra` import to the existing `import` block at the top of `integration_test.go`:

```go
"github.com/hmchangw/chat/pkg/model/cassandra"
```

(There is already a `"github.com/testcontainers/testcontainers-go/modules/cassandra"` import — this is a different package. The Go compiler will complain about the import name clash. To resolve, alias the testcontainers import: change the existing line `"github.com/testcontainers/testcontainers-go/modules/cassandra"` to `tccassandra "github.com/testcontainers/testcontainers-go/modules/cassandra"`, and update the single call site `cassandra.Run(ctx, "cassandra:5")` in `setupCassandra` to `tccassandra.Run(ctx, "cassandra:5")`.)

- [ ] **Step 2.5: Apply the import alias rename**

In `message-worker/integration_test.go`:

- Replace the existing import line `"github.com/testcontainers/testcontainers-go/modules/cassandra"` with `tccassandra "github.com/testcontainers/testcontainers-go/modules/cassandra"`.
- Add `"github.com/hmchangw/chat/pkg/model/cassandra"` to the same import block.
- Replace `container, err := cassandra.Run(ctx, "cassandra:5")` (the only usage in `setupCassandra`, currently at line 27) with `container, err := tccassandra.Run(ctx, "cassandra:5")`.

- [ ] **Step 3: Run integration tests**

Run: `make test-integration SERVICE=message-worker`

Expected: PASS. New test functions:
- `TestCassandraStore_SaveMessage_WithQuotedParent` — confirms `messages_by_room` and `messages_by_id` round-trip the snapshot.
- `TestCassandraStore_SaveMessage_NilQuotedParent` — confirms nil pointer round-trips as null UDT (resolves the open question).
- `TestCassandraStore_SaveThreadMessage_WithQuotedParent` — confirms thread tables round-trip the snapshot.
- All existing integration tests continue to pass.

Note: integration tests require Docker. If Docker is unavailable, document the failure and ensure the change is verified before merge.

- [ ] **Step 4: Run unit tests + lint as final guardrails**

Run: `make test && make lint`

Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
git add message-worker/integration_test.go
git commit -m "test(message-worker): integration tests for quoted_parent_message UDT

Extends the testcontainers schema with the QuotedParentMessage UDT and
the quoted_parent_message column on messages_by_room, messages_by_id,
and thread_messages_by_room. Adds three new round-trip tests:

- SaveMessage with populated snapshot reads back intact from both
  messages_by_room and messages_by_id.
- SaveMessage with nil snapshot writes a null UDT (resolves the open
  question from the spec — gocql does marshal a nil pointer as null).
- SaveThreadMessage with populated snapshot persists to both
  thread_messages_by_room and messages_by_id.

The existing testcontainers cassandra import is aliased to tccassandra
to avoid colliding with pkg/model/cassandra."
```

---

## Done — feature complete

After Task 7 commits, every spec requirement is implemented and tested:

| Spec requirement | Task |
|---|---|
| `QuotedParentMessage` UDT gains `thread_parent_id` + `thread_parent_created_at` (Go struct, single-source-of-truth doc, local-dev DDL, history-service inline UDT) | Task 0 |
| `QuotedParentMessageID` on `SendMessageRequest` | Task 1 |
| `QuotedParentMessage` on `Message` (uses `cassandra.QuotedParentMessage`) | Task 1 |
| `subject.MsgGet` concrete-subject helper | Task 2 |
| `ParentMessageFetcher` interface, hard-fail handler branch, **thread-context guard** (rejects cross-thread-room quotes) | Task 3 |
| `historyParentFetcher` implementation incl. `messageLink` and thread-context projection | Task 4 |
| `CHAT_BASE_URL` env var, default `http://localhost:3000` | Task 5 |
| `quoted_parent_message` persisted in `messages_by_room` / `messages_by_id` (regular + thread) and `thread_messages_by_room` | Task 6 |
| Integration round-trip incl. thread-context fields + nil-UDT verification | Task 7 |

**Production rollout reminder (3 strict steps):**

1. Run the two `ALTER TYPE` statements (see spec §"Cassandra schema migration") against each site's keyspace.
2. Deploy the new `message-worker` (so canonical events with snapshots are persisted into the new UDT fields).
3. Deploy the new `message-gatekeeper` (starts populating the snapshot field on canonical events).

Old binaries reading post-migration UDTs are unaffected (gocql tolerates extra UDT fields). Deploying gatekeeper before message-worker is not catastrophic — gocql ignores unspecified columns — but snapshots emitted in that window won't be persisted.

**Out of scope (deferred):**
- `attachments` field on the snapshot.
- `notification-worker` updates beyond the automatic snapshot propagation it gets via `Message`.
- Cassandra schema migration — the column exists in all four production tables already.
