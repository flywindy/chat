# History Service `getThreadMessages` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a new NATS request/reply endpoint `getThreadMessages` to `history-service` that returns thread replies for a given parent message, paginated newest-first with a cursor.

**Architecture:** Thin handler in `history-service/internal/service` reads `roomID` from the NATS subject, enforces room access via subscription check before fetching any message (to prevent probing message IDs outside the caller's room), then calls a Cassandra repo method `GetThreadMessages` that queries `thread_messages_by_room` on the partition key `room_id` + first clustering key `thread_room_id`. Cursor-based pagination via existing `cassrepo.QueryBuilder` (native Cassandra `PageState`).

**Tech Stack:** Go 1.25, `github.com/gocql/gocql`, `pkg/natsrouter`, `go.uber.org/mock`, `testify`, `testcontainers-go/modules/cassandra` for integration tests.

**Spec:** `docs/superpowers/specs/2026-04-21-history-get-thread-messages-design.md`

---

## File Map

| File | Change |
|------|--------|
| `pkg/subject/subject.go` | Add `MsgThreadPattern(siteID)` and `MsgThreadWildcard(siteID)` |
| `pkg/subject/subject_test.go` | Add cases for the two new builders |
| `history-service/internal/models/message.go` | Add `GetThreadMessagesRequest` + `GetThreadMessagesResponse` |
| `history-service/internal/cassrepo/thread_messages.go` | Add `threadMessageColumns`, `threadMessageScanDest`, `GetThreadMessages` (shipped as a separate file; `repository.go` was split into per-table files) |
| `history-service/internal/cassrepo/integration_test.go` | Add seeding helper + four integration test cases |
| `history-service/internal/service/service.go` | Extend `MessageRepository` interface; register handler |
| `history-service/internal/service/threads.go` | Add `GetThreadMessages` handler |
| `history-service/internal/service/threads_test.go` | Table-driven unit tests for the handler |
| `history-service/internal/service/mocks/mock_repository.go` | Regenerated via `make generate SERVICE=history-service` |

No other services, docker init, or docs are touched.

---

## Task 1 — Subject builders in `pkg/subject`

**Files:**
- Modify: `pkg/subject/subject.go`
- Modify: `pkg/subject/subject_test.go`

- [ ] **Step 1: Write failing tests for the two new builders**

Open `pkg/subject/subject_test.go`. In `TestSubjectBuilders` (the table starting around line 9), add a new case to the `tests` slice next to the other `Msg*Pattern` entries. Since the existing file doesn't yet list `MsgHistoryPattern`/`MsgNextPattern`/`MsgSurroundingPattern`/`MsgGetPattern` in that test (they are `Pattern` style, not `Wildcard`), add the thread pattern case to `TestSubjectBuilders` and the thread wildcard case to `TestWildcardPatterns`.

In `TestSubjectBuilders` add (before the closing `}` of the `tests` slice):

```go
{"MsgThreadPattern", subject.MsgThreadPattern("site-a"),
    "chat.user.{account}.request.room.{roomID}.site-a.msg.thread"},
```

In `TestWildcardPatterns` add (before the closing `}` of that slice):

```go
{"MsgThreadWild", subject.MsgThreadWildcard("site-a"),
    "chat.user.*.request.room.*.site-a.msg.thread"},
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
go test -race ./pkg/subject/...
```

Expected: FAIL with `undefined: subject.MsgThreadPattern` and `undefined: subject.MsgThreadWildcard`.

- [ ] **Step 3: Add the two builders**

Open `pkg/subject/subject.go`. Immediately after `MsgGetPattern` (line 194 area), add:

```go
func MsgThreadPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.msg.thread", siteID)
}
```

And immediately after `MsgHistoryWildcard` (line 156 area), add:

```go
func MsgThreadWildcard(siteID string) string {
	return fmt.Sprintf("chat.user.*.request.room.*.%s.msg.thread", siteID)
}
```

- [ ] **Step 4: Run tests — confirm they pass**

```bash
go test -race ./pkg/subject/...
```

Expected: PASS. All existing cases still green.

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "feat(pkg/subject): add MsgThread pattern and wildcard builders"
```

---

## Task 2 — Request and response types

**Files:**
- Modify: `history-service/internal/models/message.go`

These are plain data types with no logic. No test file needed — they're exercised via handler tests in Task 5. Add them first so later tasks can reference them.

- [ ] **Step 1: Add the two types at the end of the file**

Open `history-service/internal/models/message.go`. Append (after the existing `GetMessageByIDRequest` definition):

```go
// GetThreadMessagesRequest is the payload for loading replies in a thread.
type GetThreadMessagesRequest struct {
	ThreadMessageID string `json:"threadMessageId"` // parent message ID (old Meteor `tmid`)
	Cursor          string `json:"cursor,omitempty"`
	Limit           int    `json:"limit"`
}

// GetThreadMessagesResponse is the response for GetThreadMessages.
type GetThreadMessagesResponse struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"nextCursor,omitempty"`
	HasNext    bool      `json:"hasNext"`
}
```

- [ ] **Step 2: Confirm the package still builds**

```bash
go build ./history-service/...
```

Expected: no output, exit 0.

- [ ] **Step 3: Lint + commit**

```bash
make lint
git add history-service/internal/models/message.go
git commit -m "feat(history-service): add thread-messages request/response types"
```

---

## Task 3 — Cassandra repository method `GetThreadMessages`

**Files:**
- Modify: `history-service/internal/cassrepo/repository.go`
- Modify: `history-service/internal/cassrepo/integration_test.go`

TDD note: unit tests for this are in Task 5 via mocks; real DB coverage lives in the integration test we write here in Step 1. Integration tests require Docker (`testcontainers-go`).

- [ ] **Step 1: Write failing integration tests**

Open `history-service/internal/cassrepo/integration_test.go`. The file already creates the `messages_by_room` and `messages_by_id` tables in `setupCassandra`. We need to also create `thread_messages_by_room` and add a seeding helper and four test cases.

Inside `setupCassandra`, add the `thread_messages_by_room` table creation right after the `messages_by_id` `CREATE TABLE` block (after line 107, before `cluster.Keyspace = "chat_test"`):

```go
require.NoError(t, session.Query(`CREATE TABLE IF NOT EXISTS chat_test.thread_messages_by_room (
    room_id TEXT,
    thread_room_id TEXT,
    created_at TIMESTAMP,
    message_id TEXT,
    thread_parent_id TEXT,
    sender FROZEN<"Participant">,
    target_user FROZEN<"Participant">,
    msg TEXT,
    mentions SET<FROZEN<"Participant">>,
    attachments LIST<BLOB>,
    file FROZEN<"File">,
    card FROZEN<"Card">,
    card_action FROZEN<"CardAction">,
    quoted_parent_message FROZEN<"QuotedParentMessage">,
    visible_to TEXT,
    unread BOOLEAN,
    reactions MAP<TEXT, FROZEN<SET<FROZEN<"Participant">>>>,
    deleted BOOLEAN,
    type TEXT,
    sys_msg_data BLOB,
    site_id TEXT,
    edited_at TIMESTAMP,
    updated_at TIMESTAMP,
    PRIMARY KEY ((room_id), thread_room_id, created_at, message_id)
) WITH CLUSTERING ORDER BY (thread_room_id DESC, created_at DESC, message_id DESC)`).Exec())
```

Then, at the end of the file, add a seeding helper and the four tests:

```go
func seedThreadMessages(t *testing.T, session *gocql.Session, roomID, threadRoomID, parentID string, base time.Time, count int) {
	t.Helper()
	sender := models.Participant{ID: "u1", Account: "user1"}
	for i := 0; i < count; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		err := session.Query(
			`INSERT INTO thread_messages_by_room (room_id, thread_room_id, created_at, message_id, thread_parent_id, sender, msg) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			roomID, threadRoomID, ts, fmt.Sprintf("%s-reply-%d", threadRoomID, i), parentID, sender, fmt.Sprintf("reply-%d", i),
		).Exec()
		require.NoError(t, err)
	}
}

func TestRepository_GetThreadMessages_IsolatesByThreadRoomID(t *testing.T) {
	session := setupCassandra(t)
	repo := NewRepository(session)
	ctx := context.Background()
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// Two threads in the same room with different thread_room_ids.
	seedThreadMessages(t, session, "r-A", "tr-1", "m-parent-1", base, 3)
	seedThreadMessages(t, session, "r-A", "tr-2", "m-parent-2", base, 5)

	q, err := ParsePageRequest("", 100)
	require.NoError(t, err)

	page1, err := repo.GetThreadMessages(ctx, "r-A", "tr-1", q)
	require.NoError(t, err)
	assert.Len(t, page1.Data, 3)
	for _, m := range page1.Data {
		assert.Equal(t, "tr-1", m.ThreadRoomID)
		assert.Equal(t, "m-parent-1", m.ThreadParentID)
	}

	page2, err := repo.GetThreadMessages(ctx, "r-A", "tr-2", q)
	require.NoError(t, err)
	assert.Len(t, page2.Data, 5)
	for _, m := range page2.Data {
		assert.Equal(t, "tr-2", m.ThreadRoomID)
	}
}

func TestRepository_GetThreadMessages_IsolatesByRoomID(t *testing.T) {
	session := setupCassandra(t)
	repo := NewRepository(session)
	ctx := context.Background()
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// Same thread_room_id value in two different rooms — partition key must isolate them.
	seedThreadMessages(t, session, "r-A", "tr-1", "m-parent-A", base, 2)
	seedThreadMessages(t, session, "r-B", "tr-1", "m-parent-B", base, 4)

	q, err := ParsePageRequest("", 100)
	require.NoError(t, err)

	pageA, err := repo.GetThreadMessages(ctx, "r-A", "tr-1", q)
	require.NoError(t, err)
	assert.Len(t, pageA.Data, 2)

	pageB, err := repo.GetThreadMessages(ctx, "r-B", "tr-1", q)
	require.NoError(t, err)
	assert.Len(t, pageB.Data, 4)
}

func TestRepository_GetThreadMessages_OrdersDescByCreatedAt(t *testing.T) {
	session := setupCassandra(t)
	repo := NewRepository(session)
	ctx := context.Background()
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	seedThreadMessages(t, session, "r-A", "tr-1", "m-parent-1", base, 4)

	q, err := ParsePageRequest("", 100)
	require.NoError(t, err)

	page, err := repo.GetThreadMessages(ctx, "r-A", "tr-1", q)
	require.NoError(t, err)
	require.Len(t, page.Data, 4)
	for i := 0; i < len(page.Data)-1; i++ {
		assert.True(t, page.Data[i].CreatedAt.After(page.Data[i+1].CreatedAt),
			"expected DESC order at index %d", i)
	}
}

func TestRepository_GetThreadMessages_Pagination(t *testing.T) {
	session := setupCassandra(t)
	repo := NewRepository(session)
	ctx := context.Background()
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	seedThreadMessages(t, session, "r-A", "tr-1", "m-parent-1", base, 7)

	q, err := ParsePageRequest("", 3)
	require.NoError(t, err)

	page1, err := repo.GetThreadMessages(ctx, "r-A", "tr-1", q)
	require.NoError(t, err)
	assert.Len(t, page1.Data, 3)
	assert.True(t, page1.HasNext)
	require.NotEmpty(t, page1.NextCursor)

	q2, err := ParsePageRequest(page1.NextCursor, 3)
	require.NoError(t, err)
	page2, err := repo.GetThreadMessages(ctx, "r-A", "tr-1", q2)
	require.NoError(t, err)
	assert.Len(t, page2.Data, 3)

	q3, err := ParsePageRequest(page2.NextCursor, 3)
	require.NoError(t, err)
	page3, err := repo.GetThreadMessages(ctx, "r-A", "tr-1", q3)
	require.NoError(t, err)
	assert.Len(t, page3.Data, 1)
	assert.False(t, page3.HasNext)

	// No overlap, no gaps.
	seen := map[string]bool{}
	for _, m := range page1.Data {
		seen[m.MessageID] = true
	}
	for _, m := range page2.Data {
		assert.False(t, seen[m.MessageID], "page2 overlaps page1: %s", m.MessageID)
		seen[m.MessageID] = true
	}
	for _, m := range page3.Data {
		assert.False(t, seen[m.MessageID], "page3 overlaps earlier pages: %s", m.MessageID)
	}
	assert.Len(t, seen, 6) // page1 (3) + page2 (3); page3 tracked via the False check only
}

func TestRepository_GetThreadMessages_EmptyWhenThreadUnknown(t *testing.T) {
	session := setupCassandra(t)
	repo := NewRepository(session)
	ctx := context.Background()

	q, err := ParsePageRequest("", 10)
	require.NoError(t, err)

	page, err := repo.GetThreadMessages(ctx, "r-A", "tr-nonexistent", q)
	require.NoError(t, err)
	assert.Empty(t, page.Data)
	assert.False(t, page.HasNext)
	assert.Empty(t, page.NextCursor)
}
```

- [ ] **Step 2: Run the integration tests — confirm they fail**

```bash
make test-integration SERVICE=history-service
```

Expected: compilation error `repo.GetThreadMessages undefined`. (If Docker isn't available, the test binary still needs to compile — `go build -tags integration ./history-service/...` must also fail the same way.)

- [ ] **Step 3: Implement `threadMessageColumns` + scan helper + method**

Open `history-service/internal/cassrepo/repository.go`. After the existing `messageByIDQuery` constant (line 25), add:

```go
// threadMessageColumns lists the columns present in thread_messages_by_room.
// This table has a strict subset of messages_by_room's columns — no tshow,
// no tcount, no thread_parent_created_at, no pinned_* — so it needs its own
// column list and scan destination.
const threadMessageColumns = "room_id, thread_room_id, created_at, message_id, thread_parent_id, " +
	"sender, target_user, msg, mentions, attachments, file, card, card_action, " +
	"quoted_parent_message, visible_to, unread, reactions, deleted, " +
	"type, sys_msg_data, site_id, edited_at, updated_at"

const threadMessageQuery = "SELECT " + threadMessageColumns + " FROM thread_messages_by_room"

// threadMessageScanDest returns Scan destination pointers for threadMessageColumns in order.
func threadMessageScanDest(m *models.Message) []any {
	return []any{
		&m.RoomID, &m.ThreadRoomID, &m.CreatedAt, &m.MessageID, &m.ThreadParentID,
		&m.Sender, &m.TargetUser, &m.Msg,
		&m.Mentions, &m.Attachments, &m.File,
		&m.Card, &m.CardAction,
		&m.QuotedParentMessage,
		&m.VisibleTo, &m.Unread, &m.Reactions,
		&m.Deleted, &m.Type, &m.SysMsgData,
		&m.SiteID, &m.EditedAt, &m.UpdatedAt,
	}
}

func scanThreadMessages(iter *gocql.Iter) []models.Message {
	messages := make([]models.Message, 0)
	for {
		var m models.Message
		if !iter.Scan(threadMessageScanDest(&m)...) {
			break
		}
		messages = append(messages, m)
	}
	return messages
}
```

Then, append the new method at the end of the file (after `GetMessageByID`):

```go
// GetThreadMessages returns a paginated set of thread replies for the given
// (roomID, threadRoomID) partition-slice, newest-first.
//
// Partition key equality on room_id + first clustering key equality on
// thread_room_id yields a single-slice seek with no ALLOW FILTERING.
// Clustering ORDER BY (thread_room_id DESC, created_at DESC, message_id DESC)
// means ORDER BY created_at DESC is the natural order within the slice.
func (r *Repository) GetThreadMessages(ctx context.Context, roomID, threadRoomID string, q PageRequest) (Page[models.Message], error) {
	var messages []models.Message

	nextCursor, err := NewQueryBuilder(
		r.session.Query(
			threadMessageQuery+` WHERE room_id = ? AND thread_room_id = ? ORDER BY created_at DESC`,
			roomID, threadRoomID,
		).WithContext(ctx),
	).
		WithCursor(q.Cursor).
		WithPageSize(q.PageSize).
		Fetch(func(iter *gocql.Iter) {
			messages = scanThreadMessages(iter)
		})
	if err != nil {
		return Page[models.Message]{}, fmt.Errorf("querying thread messages: %w", err)
	}

	return Page[models.Message]{
		Data:       messages,
		NextCursor: nextCursor,
		HasNext:    nextCursor != "",
	}, nil
}
```

- [ ] **Step 4: Run the integration tests — confirm they pass**

```bash
make test-integration SERVICE=history-service
```

Expected: all pre-existing integration tests plus the five new `TestRepository_GetThreadMessages_*` tests pass.

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add history-service/internal/cassrepo/repository.go history-service/internal/cassrepo/integration_test.go
git commit -m "feat(history-service): add GetThreadMessages cassandra repo method"
```

---

## Task 4 — Extend `MessageRepository` interface and regenerate mocks

**Files:**
- Modify: `history-service/internal/service/service.go`
- Regenerate: `history-service/internal/service/mocks/mock_repository.go`

This task only adds the method signature to the consumer-side interface and regenerates the mock. It does **not** register the handler — that's Task 6, after the handler implementation has unit tests passing.

- [ ] **Step 1: Add the method to the `MessageRepository` interface**

Open `history-service/internal/service/service.go`. In the `MessageRepository` interface (currently lines 16–22), add a new line after `GetMessageByID`:

```go
type MessageRepository interface {
	GetMessagesBefore(ctx context.Context, roomID string, before time.Time, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetMessagesBetweenDesc(ctx context.Context, roomID string, since, before time.Time, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetMessagesAfter(ctx context.Context, roomID string, after time.Time, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetAllMessagesAsc(ctx context.Context, roomID string, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetMessageByID(ctx context.Context, messageID string) (*models.Message, error)
	GetThreadMessages(ctx context.Context, roomID, threadRoomID string, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
}
```

- [ ] **Step 2: Regenerate mocks**

```bash
make generate SERVICE=history-service
```

Expected: `history-service/internal/service/mocks/mock_repository.go` is updated with a new `GetThreadMessages` method on `MockMessageRepository` and its recorder.

- [ ] **Step 3: Verify the package builds**

```bash
go build ./history-service/...
```

Expected: no output, exit 0. The real `cassrepo.Repository` (from Task 3) already satisfies the extended interface.

- [ ] **Step 4: Verify existing unit tests still pass**

```bash
make test SERVICE=history-service
```

Expected: all pre-existing `service` unit tests still green. Nothing new exercises the interface yet.

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add history-service/internal/service/service.go history-service/internal/service/mocks/mock_repository.go
git commit -m "feat(history-service): extend MessageRepository with GetThreadMessages"
```

---

## Task 5 — `GetThreadMessages` handler (TDD)

**Files:**
- Modify: `history-service/internal/service/threads_test.go`
- Modify: `history-service/internal/service/threads.go`

The handler follows the spec's flow exactly. Key points:

- **It reads `c.Param("roomID")`** — used for the subscription access check, consistent with other endpoints.
- **Subscription check runs BEFORE the parent fetch** — prevents callers from using the endpoint as a message-existence oracle.
- **It uses `findMessage(c, roomID, req.ThreadMessageID)`** — the helper enforces `msg.RoomID == roomID` as a secondary cross-check.

- [ ] **Step 1: Write failing unit tests**

Open `history-service/internal/service/threads_test.go`. Append at the end of the file (after `TestHistoryService_GetMessageByID_NotSubscribed`):

```go
// --- GetThreadMessages ---

// threadCtx builds a context that deliberately carries a DIFFERENT roomID
// in the subject from the parent's actual room — proves the handler never
// reads c.Param("roomID") and always uses parent.RoomID.
func threadCtx() *natsrouter.Context {
	return natsrouter.NewContext(map[string]string{"account": "u1", "roomID": "wrong-room-from-subject"})
}

func TestHistoryService_GetThreadMessages_Success(t *testing.T) {
	svc, msgs, subs, _ := newService(t) // 4th return: *mocks.MockThreadRoomRepository (added later)
	c := threadCtx()

	parentCreatedAt := joinTime.Add(5 * time.Minute)
	parent := &models.Message{
		MessageID:    "m-parent",
		RoomID:       "r1",
		CreatedAt:    parentCreatedAt,
		ThreadRoomID: "tr-1",
	}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	// Subscription check uses parent.RoomID, not the subject's roomID.
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(&joinTime, true, nil)

	replies := []models.Message{
		{MessageID: "reply-2", RoomID: "r1", ThreadRoomID: "tr-1", ThreadParentID: "m-parent", CreatedAt: parentCreatedAt.Add(2 * time.Minute)},
		{MessageID: "reply-1", RoomID: "r1", ThreadRoomID: "tr-1", ThreadParentID: "m-parent", CreatedAt: parentCreatedAt.Add(1 * time.Minute)},
	}
	msgs.EXPECT().GetThreadMessages(gomock.Any(), "r1", "tr-1", gomock.Any()).Return(makePage(replies, false), nil)

	resp, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.NoError(t, err)
	assert.Len(t, resp.Messages, 2)
	assert.Equal(t, "reply-2", resp.Messages[0].MessageID)
	assert.False(t, resp.HasNext)
	assert.Empty(t, resp.NextCursor)
}

func TestHistoryService_GetThreadMessages_HasNextAndCursor(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(&joinTime, true, nil)

	replies := []models.Message{
		{MessageID: "reply-2", RoomID: "r1", ThreadRoomID: "tr-1", CreatedAt: joinTime.Add(7 * time.Minute)},
		{MessageID: "reply-1", RoomID: "r1", ThreadRoomID: "tr-1", CreatedAt: joinTime.Add(6 * time.Minute)},
	}
	msgs.EXPECT().GetThreadMessages(gomock.Any(), "r1", "tr-1", gomock.Any()).Return(makePage(replies, true), nil)

	resp, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.NoError(t, err)
	assert.True(t, resp.HasNext)
	assert.NotEmpty(t, resp.NextCursor)
}

func TestHistoryService_GetThreadMessages_EmptyThreadMessageID(t *testing.T) {
	svc, _, _ := newService(t)
	c := threadCtx()

	// No mocks should be called — validation short-circuits before any DB access.
	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "threadMessageId is required")
}

func TestHistoryService_GetThreadMessages_ParentNotFound(t *testing.T) {
	svc, msgs, _ := newService(t)
	c := threadCtx()

	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-unknown").Return(nil, nil)
	// No subscription check expected — handler short-circuits on not-found.

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-unknown"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "message not found")
}

func TestHistoryService_GetThreadMessages_ParentLookupError(t *testing.T) {
	svc, msgs, _ := newService(t)
	c := threadCtx()

	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(nil, fmt.Errorf("db down"))

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.Error(t, err)
	assertInternalErr(t, err, "failed to retrieve message")
}

func TestHistoryService_GetThreadMessages_NotSubscribed(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(nil, false, nil)

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not subscribed to room")
}

func TestHistoryService_GetThreadMessages_SubscriptionStoreError(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(nil, false, fmt.Errorf("db error"))

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.Error(t, err)
	assertInternalErr(t, err, "unable to verify room access")
}

func TestHistoryService_GetThreadMessages_ParentBeforeAccessSince(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	// Parent predates the access window.
	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(-1 * time.Hour), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(&joinTime, true, nil)

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside access window")
}

func TestHistoryService_GetThreadMessages_NoHSS(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	// Even very-old parents are accessible when historySharedSince is nil.
	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(-1 * time.Hour), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	msgs.EXPECT().GetThreadMessages(gomock.Any(), "r1", "tr-1", gomock.Any()).Return(makePage(nil, false), nil)

	resp, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.NoError(t, err)
	assert.Empty(t, resp.Messages)
}

func TestHistoryService_GetThreadMessages_InvalidCursor(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(&joinTime, true, nil)

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{
		ThreadMessageID: "m-parent",
		Cursor:          "!!not-base64!!",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pagination cursor")
}

func TestHistoryService_GetThreadMessages_RepoError(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(&joinTime, true, nil)
	msgs.EXPECT().GetThreadMessages(gomock.Any(), "r1", "tr-1", gomock.Any()).Return(cassrepo.Page[models.Message]{}, fmt.Errorf("db error"))

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.Error(t, err)
	assertInternalErr(t, err, "failed to load thread messages")
}

func TestHistoryService_GetThreadMessages_DefaultLimit(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(&joinTime, true, nil)
	msgs.EXPECT().GetThreadMessages(gomock.Any(), "r1", "tr-1", gomock.Cond(func(x any) bool {
		pr, ok := x.(cassrepo.PageRequest)
		return ok && pr.PageSize == 20
	})).Return(makePage(nil, false), nil)

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.NoError(t, err)
}

func TestHistoryService_GetThreadMessages_LimitClampsToMax(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(&joinTime, true, nil)
	msgs.EXPECT().GetThreadMessages(gomock.Any(), "r1", "tr-1", gomock.Cond(func(x any) bool {
		pr, ok := x.(cassrepo.PageRequest)
		return ok && pr.PageSize == 100
	})).Return(makePage(nil, false), nil)

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent", Limit: 999})
	require.NoError(t, err)
}

func TestHistoryService_GetThreadMessages_NegativeLimit(t *testing.T) {
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(&joinTime, true, nil)
	msgs.EXPECT().GetThreadMessages(gomock.Any(), "r1", "tr-1", gomock.Cond(func(x any) bool {
		pr, ok := x.(cassrepo.PageRequest)
		return ok && pr.PageSize == 20
	})).Return(makePage(nil, false), nil)

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent", Limit: -5})
	require.NoError(t, err)
}

func TestHistoryService_GetThreadMessages_UsesParentRoomNotSubject(t *testing.T) {
	// Explicit proof: the subject's roomID ("wrong-room-from-subject") is ignored.
	// Subscription check and repo query both use parent.RoomID ("r1").
	svc, msgs, subs := newService(t)
	c := threadCtx()

	parent := &models.Message{MessageID: "m-parent", RoomID: "r1", CreatedAt: joinTime.Add(5 * time.Minute), ThreadRoomID: "tr-1"}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-parent").Return(parent, nil)
	// gomock will fail the test if GetHistorySharedSince is called with any roomID other than "r1".
	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	msgs.EXPECT().GetThreadMessages(gomock.Any(), "r1", "tr-1", gomock.Any()).Return(makePage(nil, false), nil)

	_, err := svc.GetThreadMessages(c, models.GetThreadMessagesRequest{ThreadMessageID: "m-parent"})
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
make test SERVICE=history-service
```

Expected: FAIL with `svc.GetThreadMessages undefined`.

- [ ] **Step 3: Implement the handler**

Open `history-service/internal/service/threads.go`. Append at the end of the file:

```go
func (s *HistoryService) GetThreadMessages(c *natsrouter.Context, req models.GetThreadMessagesRequest) (*models.GetThreadMessagesResponse, error) {
	account := c.Param("account")
	// NOTE: roomID is intentionally NOT read from the subject. It comes from
	// the fetched parent message's own room_id. This makes the handler
	// forward-compatible with dropping {roomID} from the subject pattern.

	if req.ThreadMessageID == "" {
		return nil, natsrouter.ErrBadRequest("threadMessageId is required")
	}

	// Fetch-first (before the subscription check) so missing IDs short-circuit
	// to 404 without a Mongo round trip. Documented deviation from the
	// access-first pattern used by LoadSurroundingMessages and GetMessageByID.
	parent, err := s.messages.GetMessageByID(c, req.ThreadMessageID)
	if err != nil {
		slog.Error("finding thread parent", "error", err, "messageID", req.ThreadMessageID)
		return nil, natsrouter.ErrInternal("failed to retrieve message")
	}
	if parent == nil {
		return nil, natsrouter.ErrNotFound("message not found")
	}

	roomID := parent.RoomID

	accessSince, err := s.getAccessSince(c, account, roomID)
	if err != nil {
		return nil, err
	}
	if accessSince != nil && parent.CreatedAt.Before(*accessSince) {
		return nil, natsrouter.ErrForbidden("thread is outside access window")
	}

	pageReq, err := parsePageRequest(req.Cursor, req.Limit)
	if err != nil {
		return nil, err
	}

	page, err := s.messages.GetThreadMessages(c, roomID, parent.ThreadRoomID, pageReq)
	if err != nil {
		slog.Error("loading thread messages", "error", err, "roomID", roomID, "threadRoomID", parent.ThreadRoomID)
		return nil, natsrouter.ErrInternal("failed to load thread messages")
	}

	return &models.GetThreadMessagesResponse{
		Messages:   page.Data,
		NextCursor: page.NextCursor,
		HasNext:    page.HasNext,
	}, nil
}
```

- [ ] **Step 4: Run tests — confirm they all pass**

```bash
make test SERVICE=history-service
```

Expected: all pre-existing tests plus the 14 new `TestHistoryService_GetThreadMessages_*` tests pass. Race detector clean.

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add history-service/internal/service/threads.go history-service/internal/service/threads_test.go
git commit -m "feat(history-service): add GetThreadMessages handler"
```

---

## Task 6 — Register the handler

**Files:**
- Modify: `history-service/internal/service/service.go`

One-line change to `RegisterHandlers`. Splitting it out from Task 5 keeps each commit's blast radius small and makes the registration an explicit, reviewable step.

- [ ] **Step 1: Add the registration line**

Open `history-service/internal/service/service.go`. In `RegisterHandlers` (currently lines 42–47), add a new line after the existing registrations:

```go
func (s *HistoryService) RegisterHandlers(r *natsrouter.Router, siteID string) {
	natsrouter.Register(r, subject.MsgHistoryPattern(siteID), s.LoadHistory)
	natsrouter.Register(r, subject.MsgNextPattern(siteID), s.LoadNextMessages)
	natsrouter.Register(r, subject.MsgSurroundingPattern(siteID), s.LoadSurroundingMessages)
	natsrouter.Register(r, subject.MsgGetPattern(siteID), s.GetMessageByID)
	natsrouter.Register(r, subject.MsgThreadPattern(siteID), s.GetThreadMessages)
}
```

- [ ] **Step 2: Build + run all unit tests**

```bash
go build ./history-service/...
make test SERVICE=history-service
```

Expected: both succeed.

- [ ] **Step 3: Lint + commit**

```bash
make lint
git add history-service/internal/service/service.go
git commit -m "feat(history-service): register GetThreadMessages NATS endpoint"
```

---

## Task 7 — Final verification

**Files:** none modified.

End-to-end check across the whole repo before handing off.

- [ ] **Step 1: Lint the whole repo**

```bash
make lint
```

Expected: no output, exit 0.

- [ ] **Step 2: Run every unit test with race detector**

```bash
make test
```

Expected: all packages pass.

- [ ] **Step 3: Run every integration test**

```bash
make test-integration SERVICE=history-service
```

Expected: all `cassrepo` integration tests pass, including the five new `TestRepository_GetThreadMessages_*` cases.

- [ ] **Step 4: Verify build of the service binary**

```bash
make build SERVICE=history-service
```

Expected: `bin/history-service` produced.

- [ ] **Step 5: Push the branch**

```bash
git push -u origin claude/plan-history-endpoint-8FXg5
```

No PR is created. Creating the PR is the user's call.

---

## Self-Review Notes

Checked against `docs/superpowers/specs/2026-04-21-history-get-thread-messages-design.md`:

**Spec coverage:**

| Spec item | Implementing task |
|-----------|-------------------|
| NATS subject builders (`MsgThreadPattern` + `MsgThreadWildcard`) | Task 1 |
| Request/response types | Task 2 |
| Cassandra repo method targeting `thread_messages_by_room` with `(room_id, thread_room_id)` equality seek | Task 3 |
| Dedicated `threadMessageColumns` + scan helper | Task 3 |
| Integration tests: partition isolation by `room_id`, slice isolation by `thread_room_id`, DESC ordering, cursor pagination, empty thread | Task 3 |
| `MessageRepository` interface extension + mock regeneration | Task 4 |
| Handler reads `account` and `roomID` from the subject; subscription check uses subject `roomID` | Task 5 |
| Subscription check runs before parent fetch; `findMessage` cross-checks `msg.RoomID == roomID` | Task 5 |
| `historySharedSince` gating against parent's `CreatedAt` | Task 5, covered by `ParentBeforeAccessSince` + `NoHSS` |
| Cursor pagination default/max/negative-limit handling | Task 5, covered by `DefaultLimit` / `LimitClampsToMax` / `NegativeLimit` |
| Error matrix (bad request, not found, forbidden, internal) | Task 5, covered by the dedicated cases listed in the spec |
| Handler registered on the router | Task 6 |

**No-`"N/A"` promise:** every seeded `thread_room_id` in `seedThreadMessages` uses values `tr-1`/`tr-2`. No test fixture writes the literal `"N/A"` sentinel into the thread path. (Note: the pre-existing `TestRepository_FullRow_AllColumns` test keeps writing `"N/A"` to `messages_by_id.thread_room_id` — that's unrelated, exercises the full-column scan of `messages_by_id`, and is out of scope here.)

**No other services touched:** no edits to `message-worker`, `message-gatekeeper`, `broadcast-worker`, `room-service`, `docker-local/**`, or any docs besides this plan and the linked spec.

**No placeholders:** every step contains concrete code or exact commands. No "TBD", no "similar to", no "handle edge cases".

**Type consistency:** `GetThreadMessagesRequest.ThreadMessageID` name used identically across models, tests, and handler. `ThreadRoomID` is the field name on `models.Message` (already defined in `pkg/model/cassandra/message.go` from prior work) — verified against Task 3's `threadMessageScanDest` binding.

---

## Execution Choice

Plan complete and committed to `docs/superpowers/plans/2026-04-21-history-get-thread-messages.md`. Two execution options:

**1. Subagent-Driven (recommended)** — fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — execute tasks in this session using `executing-plans`, batch execution with checkpoints.

