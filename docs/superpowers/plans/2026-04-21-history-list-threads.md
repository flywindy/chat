# GetThreadsList Implementation Plan

> ⚠️ **SUPERSEDED** — This plan was replaced by `2026-04-23-history-list-threads-redesign.md` before implementation. The shipped API is `GetThreadParentMessages` (not `GetThreadsList`), unread filtering uses `GetUnreadThreadRooms(ctx, roomID, account, ...)` (account-based, not parent-ID-driven), and `Subscription.ThreadUnread` was never added. **Do not execute this plan.**

> **For agentic workers:** This plan is superseded — see above. Do not implement.

**Goal:** Add a `GetThreadsList` NATS endpoint to `history-service` that returns thread-parent messages for a room sorted by last activity, with `all`/`following`/`unread` filter types and offset+limit pagination.

**Architecture:** MongoDB `thread_rooms` collection drives pagination, sorting (`threadLastMessage`), and filtering (`replyAccounts`, `threadParentId`). Cassandra `messages_by_id` bulk-hydrates the parent message bodies via a new `GetMessagesByIDs` method. The two result sets are joined in-memory into `ThreadParentMessage` response entities. Schema migrations: remove `tcount` from Cassandra (thread counts move to `thread_rooms.threadCount` in MongoDB), extend `pkg/model.ThreadRoom` with full thread metadata, add `ThreadUnread []string` to `Subscription`.

**Tech Stack:** Go 1.25, NATS request/reply via `pkg/natsrouter`, MongoDB (`go.mongodb.org/mongo-driver/v2`), Cassandra (`github.com/gocql/gocql`), `go.uber.org/mock` (mockgen), `github.com/stretchr/testify`, `testcontainers-go`.

---

## File Map

| File | Action | Responsibility |
|------|--------|---------------|
| `pkg/model/threadroom.go` | Modify | Updated `ThreadRoom` struct with full thread metadata and index docs |
| `pkg/model/subscription.go` | Modify | Add `ThreadUnread []string` field |
| `pkg/model/cassandra/message.go` | Modify | Remove `TCount` field |
| `pkg/model/model_test.go` | Modify | Update `TestThreadRoomJSON` for new struct; add `threadUnread` to `TestSubscriptionJSON` |
| `docs/cassandra_message_model.md` | Modify | Remove `tcount` from both table schemas |
| `docker-local/cassandra/init/10-table-messages_by_room.cql` | Modify | Remove `tcount INT` column |
| `docker-local/cassandra/init/13-table-messages_by_id.cql` | Modify | Remove `tcount INT` column |
| `history-service/internal/cassrepo/repository.go` | Modify | Remove `tcount` from `baseColumns`/`baseScanDest`; add `GetMessagesByIDs` |
| `history-service/internal/cassrepo/integration_test.go` | Modify | Remove `tcount` from CREATE TABLE DDL; add `TestRepository_GetMessagesByIDs` |
| `history-service/internal/mongorepo/pagination.go` | Create | `OffsetPageRequest`, `OffsetPage[T]`, `NewOffsetPageRequest`, `Paginate[T]` |
| `history-service/internal/mongorepo/pagination_test.go` | Create | Unit tests for all pagination helpers |
| `history-service/internal/mongorepo/subscription.go` | Modify | Add `GetSubscriptionForThreads` method |
| `history-service/internal/mongorepo/integration_test.go` | Modify | Add `TestSubscriptionRepo_GetSubscriptionForThreads*` and all `TestThreadRoomRepo_*` tests |
| `history-service/internal/mongorepo/threadroom.go` | Create | `ThreadRoomRepo` with `GetThreadRooms`, `GetFollowingThreadRooms`, `GetUnreadThreadRooms`, `EnsureIndexes` |
| `history-service/internal/models/threads.go` | Create | `GetThreadsListRequest/Response`, `ThreadParentMessage`, `ListThreadsType` constants |
| `pkg/subject/subject.go` | Modify | Add `MsgThreadParentPattern`, `MsgThreadParentWildcard` |
| `history-service/internal/service/service.go` | Modify | Add `ThreadRoomRepository` interface; extend `MessageRepository` + `SubscriptionRepository`; 3-arg `New`; register new handler in `RegisterHandlers` |
| `history-service/internal/service/mocks/mock_repository.go` | Regenerate | Run `make generate SERVICE=history-service` after interface changes |
| `history-service/internal/service/messages_test.go` | Modify | Update `newService` to return 4 values; add `_` to all existing callers |
| `history-service/internal/service/threads.go` | Create | `GetThreadsList` handler implementation |
| `history-service/internal/service/threads_test.go` | Create | Unit tests for `GetThreadsList` covering all filter types + error paths |
| `history-service/cmd/main.go` | Modify | Wire `ThreadRoomRepo`, update `service.New` call, call `threadRoomRepo.EnsureIndexes` |

---

## Task 1: Update ThreadRoom model + test

**Files:**
- Modify: `pkg/model/threadroom.go`
- Modify: `pkg/model/model_test.go:43-55`

- [ ] **Step 1: Update the test to use new field names** (causes compile error — red phase)

Replace `TestThreadRoomJSON` in `pkg/model/model_test.go`:

```go
func TestThreadRoomJSON(t *testing.T) {
	tr := model.ThreadRoom{
		ID:                    "tr-1",
		RoomID:                "r1",
		ThreadParentID:        "msg-parent",
		ThreadParentCreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		TShow:                 true,
		ThreadCount:           5,
		ThreadLastMessage:     time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
		ReplyAccounts:         []string{"alice", "bob"},
		SiteID:                "site-a",
		CreatedAt:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:             time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(&tr)
	require.NoError(t, err)
	var dst model.ThreadRoom
	require.NoError(t, json.Unmarshal(data, &dst))
	assert.Equal(t, tr, dst)
}
```

Note: `roundTrip` cannot be used here because `[]string` (the `ReplyAccounts` field) is not comparable. Use explicit marshal/unmarshal + `assert.Equal` (which uses `reflect.DeepEqual`).

- [ ] **Step 2: Verify the compile error (red)**

Run: `make test SERVICE=pkg/model`
Expected: FAIL — `model.ThreadRoom` composite literal uses field names (`ParentMessageID`, `LastMsgAt`, `LastMsgID`) that no longer exist

- [ ] **Step 3: Rewrite `pkg/model/threadroom.go`**

```go
package model

import "time"

// ThreadRoom represents a thread in the thread_rooms MongoDB collection.
// Sorted by ThreadLastMessage for efficient listing; supports three query patterns
// via compound indexes:
//   - {roomId:1, threadLastMessage:-1}                               — all threads
//   - {roomId:1, replyAccounts:1, threadLastMessage:-1}              — following filter
//   - {roomId:1, threadParentId:1, threadLastMessage:-1}             — unread filter
type ThreadRoom struct {
	ID                    string    `json:"id"                    bson:"_id"`
	RoomID                string    `json:"roomId"                bson:"roomId"`
	ThreadParentID        string    `json:"threadParentId"        bson:"threadParentId"`
	ThreadParentCreatedAt time.Time `json:"threadParentCreatedAt" bson:"threadParentCreatedAt"`
	TShow                 bool      `json:"tshow"                 bson:"tshow"`
	ThreadCount           int       `json:"threadCount"           bson:"threadCount"`
	ThreadLastMessage     time.Time `json:"threadLastMessage"     bson:"threadLastMessage"`
	ReplyAccounts         []string  `json:"replyAccounts"         bson:"replyAccounts"`
	SiteID                string    `json:"siteId"                bson:"siteId"`
	CreatedAt             time.Time `json:"createdAt"             bson:"createdAt"`
	UpdatedAt             time.Time `json:"updatedAt"             bson:"updatedAt"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/model`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/model/threadroom.go pkg/model/model_test.go
git commit -m "feat(model): replace ThreadRoom with full thread metadata struct"
```

---

## Task 2: Add ThreadUnread to Subscription

**Files:**
- Modify: `pkg/model/subscription.go`
- Modify: `pkg/model/model_test.go:319-344`

- [ ] **Step 1: Add `ThreadUnread` to `TestSubscriptionJSON`** (red — field doesn't exist yet)

In `pkg/model/model_test.go`, update `TestSubscriptionJSON` to include the new field. Replace the `s := model.Subscription{...}` literal so it includes `ThreadUnread`:

```go
func TestSubscriptionJSON(t *testing.T) {
	hss := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := model.Subscription{
		ID:                 "s1",
		User:               model.SubscriptionUser{ID: "u1", Account: "alice"},
		RoomID:             "r1",
		SiteID:             "site-a",
		Roles:              []model.Role{model.RoleOwner},
		HistorySharedSince: &hss,
		JoinedAt:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		LastSeenAt:         time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		HasMention:         true,
		ThreadUnread:       []string{"parent-1", "parent-2"},
	}

	data, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var dst model.Subscription
	if err := json.Unmarshal(data, &dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(s, dst) {
		t.Errorf("round-trip mismatch:\n  got  %+v\n  want %+v", dst, s)
	}
}
```

- [ ] **Step 2: Verify the compile error (red)**

Run: `make test SERVICE=pkg/model`
Expected: FAIL — `model.Subscription` has no field `ThreadUnread`

- [ ] **Step 3: Add `ThreadUnread` to `pkg/model/subscription.go`**

Add the field at the end of the `Subscription` struct (after `HasMention`):

```go
type Subscription struct {
	ID                 string           `json:"id" bson:"_id"`
	User               SubscriptionUser `json:"u" bson:"u"`
	RoomID             string           `json:"roomId" bson:"roomId"`
	SiteID             string           `json:"siteId" bson:"siteId"`
	Roles              []Role           `json:"roles" bson:"roles"`
	HistorySharedSince *time.Time       `json:"historySharedSince,omitempty" bson:"historySharedSince,omitempty"`
	JoinedAt           time.Time        `json:"joinedAt" bson:"joinedAt"`
	LastSeenAt         time.Time        `json:"lastSeenAt" bson:"lastSeenAt"`
	HasMention         bool             `json:"hasMention" bson:"hasMention"`
	ThreadUnread       []string         `json:"threadUnread,omitempty" bson:"threadUnread,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/model`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/model/subscription.go pkg/model/model_test.go
git commit -m "feat(model): add ThreadUnread field to Subscription"
```

---

## Task 3: Remove TCount from Cassandra message model + DDL

`tcount` is being retired from Cassandra. Thread counts now live in `thread_rooms.threadCount` in MongoDB. This task removes the field from the Go struct, the schema docs, and the local-dev DDL files. Cassandra tables in production are forward-compatible (missing column is fine for reads; write omissions default to null/0).

**Files:**
- Modify: `pkg/model/cassandra/message.go:72`
- Modify: `docs/cassandra_message_model.md`
- Modify: `docker-local/cassandra/init/10-table-messages_by_room.cql`
- Modify: `docker-local/cassandra/init/13-table-messages_by_id.cql`

- [ ] **Step 1: Remove `TCount` from `pkg/model/cassandra/message.go`**

Delete this line from the `Message` struct:

```go
TCount                int                      `json:"tcount,omitempty"`
```

The struct should go directly from `TShow` to `ThreadParentID`:

```go
TShow                 bool                     `json:"tshow,omitempty"`
ThreadParentID        string                   `json:"threadParentId,omitempty"`
```

- [ ] **Step 2: Remove `tcount` from `docs/cassandra_message_model.md`**

In the `messages_by_room` table block, delete:
```
  tcount INT, // message reply thread count
```

In the `messages_by_id` table block, delete:
```
  tcount INT, // message reply thread count
```

- [ ] **Step 3: Remove `tcount` from `docker-local/cassandra/init/10-table-messages_by_room.cql`**

Delete the line:
```sql
  tcount                   INT,     // reply thread count
```

- [ ] **Step 4: Remove `tcount` from `docker-local/cassandra/init/13-table-messages_by_id.cql`**

Delete the line:
```sql
  tcount                   INT, // reply thread count
```

- [ ] **Step 5: Verify compilation**

Run: `make test SERVICE=pkg/model`
Expected: PASS (no reference to `TCount` in pkg/model tests)

- [ ] **Step 6: Commit**

```bash
git add pkg/model/cassandra/message.go docs/cassandra_message_model.md \
    docker-local/cassandra/init/10-table-messages_by_room.cql \
    docker-local/cassandra/init/13-table-messages_by_id.cql
git commit -m "feat(schema): remove tcount from Cassandra — thread counts move to MongoDB thread_rooms"
```

---

## Task 4: Fix cassrepo after TCount removal

`baseColumns` and `baseScanDest` in `cassrepo/repository.go` reference `tcount`. The integration test DDL also includes `tcount INT` in its CREATE TABLE statements. This task removes all three references so the codebase compiles and tests pass.

**Files:**
- Modify: `history-service/internal/cassrepo/repository.go:14-19,65-76`
- Modify: `history-service/internal/cassrepo/integration_test.go:60-65,88-93`

- [ ] **Step 1: Verify the compile failure (red)**

Run: `make test SERVICE=history-service`
Expected: FAIL — `m.TCount undefined (type models.Message has no field or method TCount)`

- [ ] **Step 2: Remove `tcount` from `baseColumns` in `history-service/internal/cassrepo/repository.go`**

Replace the `baseColumns` constant:

```go
const baseColumns = "room_id, created_at, message_id, thread_room_id, sender, target_user, " +
	"msg, mentions, attachments, file, card, card_action, tshow, " +
	"thread_parent_id, thread_parent_created_at, quoted_parent_message, " +
	"visible_to, unread, reactions, deleted, " +
	"type, sys_msg_data, site_id, edited_at, updated_at"
```

- [ ] **Step 3: Remove `&m.TCount` from `baseScanDest` in the same file**

Replace `baseScanDest`:

```go
func baseScanDest(m *models.Message) []any {
	return []any{
		&m.RoomID, &m.CreatedAt, &m.MessageID, &m.ThreadRoomID,
		&m.Sender, &m.TargetUser, &m.Msg,
		&m.Mentions, &m.Attachments, &m.File,
		&m.Card, &m.CardAction, &m.TShow,
		&m.ThreadParentID, &m.ThreadParentCreatedAt, &m.QuotedParentMessage,
		&m.VisibleTo, &m.Unread, &m.Reactions,
		&m.Deleted, &m.Type, &m.SysMsgData,
		&m.SiteID, &m.EditedAt, &m.UpdatedAt,
	}
}
```

- [ ] **Step 4: Remove `tcount` from the integration test CREATE TABLE DDL**

In `history-service/internal/cassrepo/integration_test.go`, find the two `CREATE TABLE` statements inside `setupCassandra`. Remove the `tcount INT,` line from each:

In `messages_by_room` CREATE TABLE (around line 60): remove `tcount INT,`

In `messages_by_id` CREATE TABLE (around line 90): remove `tcount INT,`

The `messages_by_room` CREATE TABLE should go from `card_action` directly to `tshow`:
```sql
		card_action FROZEN<"CardAction">,
		tshow BOOLEAN,
		thread_parent_id TEXT,
```

The `messages_by_id` CREATE TABLE similarly:
```sql
		card_action FROZEN<"CardAction">,
		tshow BOOLEAN,
		thread_parent_id TEXT,
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `make test SERVICE=history-service`
Expected: PASS

Run: `make test-integration SERVICE=history-service`
Expected: PASS (Cassandra container starts, tables created without tcount, all existing tests pass)

- [ ] **Step 6: Commit**

```bash
git add history-service/internal/cassrepo/repository.go \
    history-service/internal/cassrepo/integration_test.go
git commit -m "fix(cassrepo): remove tcount column reference after schema migration"
```

---

## Task 5: Add GetMessagesByIDs to cassrepo

The `GetThreadsList` handler needs to bulk-fetch parent messages by ID from `messages_by_id` after getting the list from MongoDB. This adds that method to `cassrepo`.

**Files:**
- Modify: `history-service/internal/cassrepo/repository.go` (append at end)
- Modify: `history-service/internal/cassrepo/integration_test.go` (append new test functions)

- [ ] **Step 1: Write the failing integration test**

Append to `history-service/internal/cassrepo/integration_test.go` (inside the `//go:build integration` file):

```go
func TestRepository_GetMessagesByIDs(t *testing.T) {
	session := setupCassandra(t)
	repo := NewRepository(session)
	ctx := context.Background()

	sender := models.Participant{ID: "u1", Account: "alice"}
	ts1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	require.NoError(t, session.Query(
		`INSERT INTO messages_by_id (message_id, room_id, created_at, sender, msg) VALUES (?, ?, ?, ?, ?)`,
		"m-batch-1", "r1", ts1, sender, "hello",
	).Exec())
	require.NoError(t, session.Query(
		`INSERT INTO messages_by_id (message_id, room_id, created_at, sender, msg) VALUES (?, ?, ?, ?, ?)`,
		"m-batch-2", "r1", ts2, sender, "world",
	).Exec())

	msgs, err := repo.GetMessagesByIDs(ctx, []string{"m-batch-1", "m-batch-2"})
	require.NoError(t, err)
	assert.Len(t, msgs, 2)
	ids := []string{msgs[0].MessageID, msgs[1].MessageID}
	assert.ElementsMatch(t, []string{"m-batch-1", "m-batch-2"}, ids)
}

func TestRepository_GetMessagesByIDs_Empty(t *testing.T) {
	session := setupCassandra(t)
	repo := NewRepository(session)
	ctx := context.Background()

	msgs, err := repo.GetMessagesByIDs(ctx, []string{})
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

func TestRepository_GetMessagesByIDs_MissingID(t *testing.T) {
	session := setupCassandra(t)
	repo := NewRepository(session)
	ctx := context.Background()

	sender := models.Participant{ID: "u1", Account: "alice"}
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, session.Query(
		`INSERT INTO messages_by_id (message_id, room_id, created_at, sender, msg) VALUES (?, ?, ?, ?, ?)`,
		"m-exists", "r1", ts, sender, "hi",
	).Exec())

	msgs, err := repo.GetMessagesByIDs(ctx, []string{"m-exists", "m-missing"})
	require.NoError(t, err)
	assert.Len(t, msgs, 1)
	assert.Equal(t, "m-exists", msgs[0].MessageID)
}
```

- [ ] **Step 2: Run to verify it fails (red)**

Run: `make test-integration SERVICE=history-service`
Expected: FAIL — `repo.GetMessagesByIDs undefined`

- [ ] **Step 3: Implement `GetMessagesByIDs` in `history-service/internal/cassrepo/repository.go`**

Append after `GetThreadMessages`:

```go
// GetMessagesByIDs returns messages from messages_by_id for the given message IDs.
// Missing IDs are silently omitted. Order is not guaranteed.
// Returns an empty slice (not nil) when messageIDs is empty.
func (r *Repository) GetMessagesByIDs(ctx context.Context, messageIDs []string) ([]models.Message, error) {
	if len(messageIDs) == 0 {
		return []models.Message{}, nil
	}
	iter := r.session.Query(
		messageByIDQuery+` WHERE message_id IN ?`,
		messageIDs,
	).WithContext(ctx).Iter()
	messages := make([]models.Message, 0, len(messageIDs))
	for {
		var m models.Message
		if !iter.Scan(messageByIDScanDest(&m)...) {
			break
		}
		messages = append(messages, m)
	}
	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("querying messages by IDs: %w", err)
	}
	return messages, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make test-integration SERVICE=history-service`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add history-service/internal/cassrepo/repository.go \
    history-service/internal/cassrepo/integration_test.go
git commit -m "feat(cassrepo): add GetMessagesByIDs for bulk parent message hydration"
```

---

## Task 6: Add mongorepo pagination helpers

MongoDB thread-room queries use offset+limit pagination (unlike Cassandra which uses cursor-based page state). This task creates reusable helpers in the `mongorepo` package.

**Files:**
- Create: `history-service/internal/mongorepo/pagination.go`
- Create: `history-service/internal/mongorepo/pagination_test.go`

- [ ] **Step 1: Write the failing tests**

Create `history-service/internal/mongorepo/pagination_test.go`:

```go
package mongorepo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewOffsetPageRequest_Defaults(t *testing.T) {
	p := NewOffsetPageRequest(0, 0)
	assert.Equal(t, int64(0), p.Offset)
	assert.Equal(t, int64(20), p.Limit)
}

func TestNewOffsetPageRequest_Custom(t *testing.T) {
	p := NewOffsetPageRequest(10, 30)
	assert.Equal(t, int64(10), p.Offset)
	assert.Equal(t, int64(30), p.Limit)
}

func TestNewOffsetPageRequest_LimitCapped(t *testing.T) {
	p := NewOffsetPageRequest(0, 200)
	assert.Equal(t, int64(100), p.Limit)
}

func TestPaginate_FewerThanLimit(t *testing.T) {
	data := []int{1, 2, 3}
	page := Paginate(data, 5)
	assert.False(t, page.HasMore)
	assert.Equal(t, []int{1, 2, 3}, page.Data)
}

func TestPaginate_ExactlyLimit(t *testing.T) {
	data := []int{1, 2, 3}
	page := Paginate(data, 3)
	assert.False(t, page.HasMore)
	assert.Len(t, page.Data, 3)
}

func TestPaginate_MoreThanLimit(t *testing.T) {
	data := []int{1, 2, 3, 4}
	page := Paginate(data, 3)
	assert.True(t, page.HasMore)
	assert.Equal(t, []int{1, 2, 3}, page.Data)
}

func TestPaginate_Empty(t *testing.T) {
	data := []int{}
	page := Paginate(data, 5)
	assert.False(t, page.HasMore)
	assert.Empty(t, page.Data)
}
```

- [ ] **Step 2: Run to verify it fails (red)**

Run: `make test SERVICE=history-service`
Expected: FAIL — `NewOffsetPageRequest undefined` / `Paginate undefined`

- [ ] **Step 3: Create `history-service/internal/mongorepo/pagination.go`**

```go
package mongorepo

// OffsetPageRequest holds offset+limit pagination parameters for MongoDB queries.
type OffsetPageRequest struct {
	Offset int64
	Limit  int64
}

// OffsetPage is the result of a paginated MongoDB query.
type OffsetPage[T any] struct {
	Data    []T
	HasMore bool
}

// NewOffsetPageRequest creates an OffsetPageRequest.
// Default limit: 20. Maximum limit: 100.
func NewOffsetPageRequest(offset, limit int) OffsetPageRequest {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return OffsetPageRequest{Offset: int64(offset), Limit: int64(limit)}
}

// paginateOptions returns QueryOptions that fetch Limit+1 items starting at Offset.
// The extra item is used to detect HasMore — it is trimmed by Paginate before returning.
func (p OffsetPageRequest) paginateOptions() []QueryOption {
	return []QueryOption{WithSkip(p.Offset), WithLimit(p.Limit + 1)}
}

// Paginate trims data to limit items and sets HasMore if the query returned limit+1.
func Paginate[T any](data []T, limit int64) OffsetPage[T] {
	hasMore := int64(len(data)) > limit
	if hasMore {
		data = data[:limit]
	}
	return OffsetPage[T]{Data: data, HasMore: hasMore}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make test SERVICE=history-service`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add history-service/internal/mongorepo/pagination.go \
    history-service/internal/mongorepo/pagination_test.go
git commit -m "feat(mongorepo): add offset+limit pagination helpers"
```

---

## Task 7: Add GetSubscriptionForThreads to mongorepo

The thread list endpoint needs two fields from the subscription: `historySharedSince` (access lower bound) and `threadUnread` (IDs of unread parent messages). This is a targeted projection that avoids pulling the full document.

**Files:**
- Modify: `history-service/internal/mongorepo/subscription.go`
- Modify: `history-service/internal/mongorepo/integration_test.go`

- [ ] **Step 1: Write the failing integration test**

Append to `history-service/internal/mongorepo/integration_test.go`:

```go
func TestSubscriptionRepo_GetSubscriptionForThreads(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	hss := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.Collection("subscriptions").InsertOne(ctx, model.Subscription{
		ID:                 "s-thr",
		User:               model.SubscriptionUser{ID: "u1", Account: "alice"},
		RoomID:             "r1", SiteID: "site-local",
		Roles:              []model.Role{model.RoleMember},
		JoinedAt:           hss,
		HistorySharedSince: &hss,
		ThreadUnread:       []string{"p1", "p2"},
	})
	require.NoError(t, err)

	gotHSS, tunread, subscribed, err := repo.GetSubscriptionForThreads(ctx, "alice", "r1")
	require.NoError(t, err)
	assert.True(t, subscribed)
	require.NotNil(t, gotHSS)
	assert.Equal(t, hss.UTC(), gotHSS.UTC())
	assert.Equal(t, []string{"p1", "p2"}, tunread)
}

func TestSubscriptionRepo_GetSubscriptionForThreads_NotSubscribed(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	_, _, subscribed, err := repo.GetSubscriptionForThreads(ctx, "alice", "r-none")
	require.NoError(t, err)
	assert.False(t, subscribed)
}

func TestSubscriptionRepo_GetSubscriptionForThreads_NilThreadUnread(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	_, err := db.Collection("subscriptions").InsertOne(ctx, model.Subscription{
		ID:       "s-no-tu",
		User:     model.SubscriptionUser{ID: "u2", Account: "bob"},
		RoomID:   "r2", SiteID: "site-local",
		Roles:    []model.Role{model.RoleMember},
		JoinedAt: time.Now(),
	})
	require.NoError(t, err)

	_, tunread, subscribed, err := repo.GetSubscriptionForThreads(ctx, "bob", "r2")
	require.NoError(t, err)
	assert.True(t, subscribed)
	assert.Nil(t, tunread)
}
```

- [ ] **Step 2: Run to verify it fails (red)**

Run: `make test-integration SERVICE=history-service`
Expected: FAIL — `repo.GetSubscriptionForThreads undefined`

- [ ] **Step 3: Implement `GetSubscriptionForThreads` in `history-service/internal/mongorepo/subscription.go`**

Append after `GetHistorySharedSince`:

```go
// GetSubscriptionForThreads returns thread-relevant fields from a subscription.
// Returns (historySharedSince, threadUnread, subscribed=true, nil) when subscribed.
// Returns (nil, nil, subscribed=false, nil) when the user is not subscribed to the room.
func (r *SubscriptionRepo) GetSubscriptionForThreads(ctx context.Context, account, roomID string) (*time.Time, []string, bool, error) {
	sub, err := r.subscriptions.FindOne(ctx,
		bson.M{"u.account": account, "roomId": roomID},
		WithProjection(bson.M{"historySharedSince": 1, "threadUnread": 1, "_id": 0}),
	)
	if err != nil {
		return nil, nil, false, err
	}
	if sub == nil {
		return nil, nil, false, nil
	}
	return sub.HistorySharedSince, sub.ThreadUnread, true, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make test-integration SERVICE=history-service`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add history-service/internal/mongorepo/subscription.go \
    history-service/internal/mongorepo/integration_test.go
git commit -m "feat(mongorepo): add GetSubscriptionForThreads projection query"
```

---

## Task 8: Create ThreadRoomRepo

This is the MongoDB repository for the `thread_rooms` collection. It provides three queries (all threads, following, unread) plus index creation. The collection is owned by `message-worker`; this service reads from it.

**Files:**
- Create: `history-service/internal/mongorepo/threadroom.go`
- Modify: `history-service/internal/mongorepo/integration_test.go` (append tests)

- [ ] **Step 1: Write the failing integration tests**

Append to `history-service/internal/mongorepo/integration_test.go`:

```go
func insertThreadRoom(t *testing.T, db *mongo.Database, tr model.ThreadRoom) {
	t.Helper()
	_, err := db.Collection("thread_rooms").InsertOne(context.Background(), tr)
	require.NoError(t, err)
}

func TestThreadRoomRepo_GetThreadRooms(t *testing.T) {
	db := setupMongo(t)
	repo := NewThreadRoomRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-1", RoomID: "r1", ThreadParentID: "p1", ThreadLastMessage: base.Add(3 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-2", RoomID: "r1", ThreadParentID: "p2", ThreadLastMessage: base.Add(1 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-3", RoomID: "r1", ThreadParentID: "p3", ThreadLastMessage: base.Add(2 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-other", RoomID: "r2", ThreadParentID: "p4", ThreadLastMessage: base.Add(4 * time.Hour), CreatedAt: base, UpdatedAt: base})

	// Page 1: limit 2, sorted newest first
	page, err := repo.GetThreadRooms(ctx, "r1", nil, NewOffsetPageRequest(0, 2))
	require.NoError(t, err)
	assert.True(t, page.HasMore)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "tr-1", page.Data[0].ID)
	assert.Equal(t, "tr-3", page.Data[1].ID)

	// Page 2
	page2, err := repo.GetThreadRooms(ctx, "r1", nil, NewOffsetPageRequest(2, 2))
	require.NoError(t, err)
	assert.False(t, page2.HasMore)
	require.Len(t, page2.Data, 1)
	assert.Equal(t, "tr-2", page2.Data[0].ID)

	// accessSince filter — only threads after base
	afterBase := base
	page3, err := repo.GetThreadRooms(ctx, "r1", &afterBase, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Len(t, page3.Data, 3)
}

func TestThreadRoomRepo_GetFollowingThreadRooms(t *testing.T) {
	db := setupMongo(t)
	repo := NewThreadRoomRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-1", RoomID: "r1", ThreadParentID: "p1", ThreadLastMessage: base.Add(2 * time.Hour), ReplyAccounts: []string{"alice", "bob"}, CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-2", RoomID: "r1", ThreadParentID: "p2", ThreadLastMessage: base.Add(1 * time.Hour), ReplyAccounts: []string{"bob"}, CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-3", RoomID: "r1", ThreadParentID: "p3", ThreadLastMessage: base.Add(3 * time.Hour), ReplyAccounts: []string{"alice"}, CreatedAt: base, UpdatedAt: base})

	page, err := repo.GetFollowingThreadRooms(ctx, "r1", "alice", nil, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.False(t, page.HasMore)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "tr-3", page.Data[0].ID)
	assert.Equal(t, "tr-1", page.Data[1].ID)
}

func TestThreadRoomRepo_GetUnreadThreadRooms(t *testing.T) {
	db := setupMongo(t)
	repo := NewThreadRoomRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-1", RoomID: "r1", ThreadParentID: "p1", ThreadLastMessage: base.Add(2 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-2", RoomID: "r1", ThreadParentID: "p2", ThreadLastMessage: base.Add(3 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-3", RoomID: "r1", ThreadParentID: "p3", ThreadLastMessage: base.Add(1 * time.Hour), CreatedAt: base, UpdatedAt: base})

	page, err := repo.GetUnreadThreadRooms(ctx, "r1", []string{"p1", "p2"}, nil, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.False(t, page.HasMore)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "tr-2", page.Data[0].ID)
	assert.Equal(t, "tr-1", page.Data[1].ID)

	// Empty parentIDs → empty result, no error
	empty, err := repo.GetUnreadThreadRooms(ctx, "r1", []string{}, nil, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Empty(t, empty.Data)
	assert.False(t, empty.HasMore)
}
```

Note: the `integration_test.go` file already imports `"go.mongodb.org/mongo-driver/v2/mongo"` — it is used by the `setupMongo` helper. Add `"github.com/hmchangw/chat/pkg/model"` if it isn't already present (it is, from `TestSubscriptionRepo_GetSubscription`).

- [ ] **Step 2: Run to verify it fails (red)**

Run: `make test-integration SERVICE=history-service`
Expected: FAIL — `NewThreadRoomRepo undefined`

- [ ] **Step 3: Create `history-service/internal/mongorepo/threadroom.go`**

```go
package mongorepo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/hmchangw/chat/pkg/model"
)

const threadRoomsCollection = "thread_rooms"

// threadRoomSort is the canonical sort order: newest thread activity first, stable by ID.
var threadRoomSort = bson.D{{Key: "threadLastMessage", Value: -1}, {Key: "_id", Value: 1}}

// ThreadRoomRepo reads from the thread_rooms MongoDB collection.
type ThreadRoomRepo struct {
	threadRooms *Collection[model.ThreadRoom]
}

// NewThreadRoomRepo creates a new thread room repository.
func NewThreadRoomRepo(db *mongo.Database) *ThreadRoomRepo {
	return &ThreadRoomRepo{
		threadRooms: NewCollection[model.ThreadRoom](db.Collection(threadRoomsCollection)),
	}
}

// EnsureIndexes creates the compound indexes required for efficient thread queries.
// Safe to call on every startup — MongoDB's CreateMany is idempotent for existing indexes.
func (r *ThreadRoomRepo) EnsureIndexes(ctx context.Context) error {
	_, err := r.threadRooms.Raw().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "roomId", Value: 1}, {Key: "threadLastMessage", Value: -1}}},
		{Keys: bson.D{{Key: "roomId", Value: 1}, {Key: "replyAccounts", Value: 1}, {Key: "threadLastMessage", Value: -1}}},
		{Keys: bson.D{{Key: "roomId", Value: 1}, {Key: "threadParentId", Value: 1}, {Key: "threadLastMessage", Value: -1}}},
	})
	if err != nil {
		return fmt.Errorf("ensure thread_rooms indexes: %w", err)
	}
	return nil
}

// GetThreadRooms returns paginated thread rooms for a room, sorted by newest activity first.
// If accessSince is non-nil, only threads with ThreadLastMessage strictly after that time are included.
func (r *ThreadRoomRepo) GetThreadRooms(ctx context.Context, roomID string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[model.ThreadRoom], error) {
	filter := bson.M{"roomId": roomID}
	if accessSince != nil {
		filter["threadLastMessage"] = bson.M{"$gt": *accessSince}
	}
	opts := append([]QueryOption{WithSort(threadRoomSort)}, req.paginateOptions()...)
	items, err := r.threadRooms.FindMany(ctx, filter, opts...)
	if err != nil {
		return OffsetPage[model.ThreadRoom]{}, fmt.Errorf("querying thread rooms: %w", err)
	}
	return Paginate(items, req.Limit), nil
}

// GetFollowingThreadRooms returns paginated thread rooms where the given account appears in ReplyAccounts.
func (r *ThreadRoomRepo) GetFollowingThreadRooms(ctx context.Context, roomID, account string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[model.ThreadRoom], error) {
	filter := bson.M{"roomId": roomID, "replyAccounts": account}
	if accessSince != nil {
		filter["threadLastMessage"] = bson.M{"$gt": *accessSince}
	}
	opts := append([]QueryOption{WithSort(threadRoomSort)}, req.paginateOptions()...)
	items, err := r.threadRooms.FindMany(ctx, filter, opts...)
	if err != nil {
		return OffsetPage[model.ThreadRoom]{}, fmt.Errorf("querying following thread rooms: %w", err)
	}
	return Paginate(items, req.Limit), nil
}

// GetUnreadThreadRooms returns paginated thread rooms whose ThreadParentID is in parentIDs.
// parentIDs is the ThreadUnread slice from the user's subscription.
// Returns an empty page immediately when parentIDs is empty.
func (r *ThreadRoomRepo) GetUnreadThreadRooms(ctx context.Context, roomID string, parentIDs []string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[model.ThreadRoom], error) {
	if len(parentIDs) == 0 {
		return OffsetPage[model.ThreadRoom]{Data: []model.ThreadRoom{}, HasMore: false}, nil
	}
	filter := bson.M{"roomId": roomID, "threadParentId": bson.M{"$in": parentIDs}}
	if accessSince != nil {
		filter["threadLastMessage"] = bson.M{"$gt": *accessSince}
	}
	opts := append([]QueryOption{WithSort(threadRoomSort)}, req.paginateOptions()...)
	items, err := r.threadRooms.FindMany(ctx, filter, opts...)
	if err != nil {
		return OffsetPage[model.ThreadRoom]{}, fmt.Errorf("querying unread thread rooms: %w", err)
	}
	return Paginate(items, req.Limit), nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make test-integration SERVICE=history-service`
Expected: PASS (all existing tests + new ThreadRoomRepo tests)

- [ ] **Step 5: Commit**

```bash
git add history-service/internal/mongorepo/threadroom.go \
    history-service/internal/mongorepo/integration_test.go
git commit -m "feat(mongorepo): add ThreadRoomRepo with GetThreadRooms, GetFollowingThreadRooms, GetUnreadThreadRooms"
```

---

## Task 9: Add subject builders + request/response types

Two small additions before touching the service layer: the NATS subject pattern (in `pkg/subject`) and the handler request/response types (in `history-service/internal/models`).

**Files:**
- Modify: `pkg/subject/subject.go` (append at end of file)
- Create: `history-service/internal/models/threads.go`

- [ ] **Step 1: Add subject builders to `pkg/subject/subject.go`**

Append two functions after `MsgThreadPattern`:

```go
func MsgThreadParentPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.msg.thread.parent", siteID)
}

func MsgThreadParentWildcard(siteID string) string {
	return fmt.Sprintf("chat.user.*.request.room.*.%s.msg.thread.parent", siteID)
}
```

- [ ] **Step 2: Create `history-service/internal/models/threads.go`**

```go
package models

import (
	"time"

	"github.com/hmchangw/chat/pkg/model/cassandra"
)

// ListThreadsType controls which threads are returned.
type ListThreadsType string

const (
	ListThreadsAll       ListThreadsType = ""
	ListThreadsFollowing ListThreadsType = "following"
	ListThreadsUnread    ListThreadsType = "unread"
)

// GetThreadsListRequest is the NATS request payload for listing threads in a room.
type GetThreadsListRequest struct {
	Type   ListThreadsType `json:"type"`
	Offset int             `json:"offset"`
	Limit  int             `json:"limit"`
}

// ThreadParentMessage is a Cassandra parent message enriched with MongoDB thread metadata.
type ThreadParentMessage struct {
	cassandra.Message
	ThreadCount       int       `json:"threadCount"`
	ThreadLastMessage time.Time `json:"threadLastMessage"`
	ReplyAccounts     []string  `json:"replyAccounts,omitempty"`
}

// GetThreadsListResponse is the NATS response for GetThreadsList.
type GetThreadsListResponse struct {
	Threads []ThreadParentMessage `json:"threads"`
	HasMore bool                  `json:"hasMore"`
}
```

- [ ] **Step 3: Verify compilation**

Run: `make test SERVICE=history-service`
Expected: PASS

Run: `make test SERVICE=pkg/subject`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/subject/subject.go history-service/internal/models/threads.go
git commit -m "feat(subject,models): add MsgThreadParentPattern and GetThreadsList types"
```

---

## Task 10: Update service interfaces, regenerate mocks, update newService helper

The service layer needs three interface changes: `MessageRepository` gets `GetMessagesByIDs`, `SubscriptionRepository` gets `GetSubscriptionForThreads`, and a new `ThreadRoomRepository` interface is added. After updating the interfaces, mocks must be regenerated and `newService` in the existing test helper must be updated.

**Files:**
- Modify: `history-service/internal/service/service.go`
- Regenerate: `history-service/internal/service/mocks/mock_repository.go`
- Modify: `history-service/internal/service/messages_test.go`

- [ ] **Step 1: Write the failing test (red)**

In `messages_test.go`, update `newService` to return 4 values. This causes a compile error until the service `New` function is updated:

```go
func newService(t *testing.T) (*service.HistoryService, *mocks.MockMessageRepository, *mocks.MockSubscriptionRepository, *mocks.MockThreadRoomRepository) {
	ctrl := gomock.NewController(t)
	msgs := mocks.NewMockMessageRepository(ctrl)
	subs := mocks.NewMockSubscriptionRepository(ctrl)
	threadRooms := mocks.NewMockThreadRoomRepository(ctrl)
	return service.New(msgs, subs, threadRooms), msgs, subs, threadRooms
}
```

Also update all existing callers in `messages_test.go` — they use `svc, msgs, subs := newService(t)`. Add `_` to capture the unused 4th return:

```bash
sed -i 's/svc, msgs, subs := newService(t)/svc, msgs, subs, _ := newService(t)/g' \
    history-service/internal/service/messages_test.go
```

Some callers only use `svc` and `subs`, or just `svc` — update those too:

```bash
sed -i 's/svc, _, subs := newService(t)/svc, _, subs, _ := newService(t)/g' \
    history-service/internal/service/messages_test.go
```

- [ ] **Step 2: Verify the compile error (red)**

Run: `make test SERVICE=history-service`
Expected: FAIL — `mocks.MockThreadRoomRepository undefined` / `service.New` argument count mismatch

- [ ] **Step 3: Rewrite `history-service/internal/service/service.go`**

```go
package service

import (
	"context"
	"time"

	"github.com/hmchangw/chat/history-service/internal/cassrepo"
	"github.com/hmchangw/chat/history-service/internal/models"
	"github.com/hmchangw/chat/history-service/internal/mongorepo"
	pkgmodel "github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/subject"
)

//go:generate mockgen -destination=mocks/mock_repository.go -package=mocks . MessageRepository,SubscriptionRepository,ThreadRoomRepository

// MessageRepository defines Cassandra-backed message operations.
type MessageRepository interface {
	GetMessagesBefore(ctx context.Context, roomID string, before time.Time, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetMessagesBetweenDesc(ctx context.Context, roomID string, since, before time.Time, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetMessagesAfter(ctx context.Context, roomID string, after time.Time, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetAllMessagesAsc(ctx context.Context, roomID string, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetMessageByID(ctx context.Context, messageID string) (*models.Message, error)
	GetThreadMessages(ctx context.Context, roomID, threadRoomID string, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
	GetMessagesByIDs(ctx context.Context, messageIDs []string) ([]models.Message, error)
}

// SubscriptionRepository defines MongoDB-backed subscription lookups.
type SubscriptionRepository interface {
	GetHistorySharedSince(ctx context.Context, account, roomID string) (*time.Time, bool, error)
	GetSubscriptionForThreads(ctx context.Context, account, roomID string) (*time.Time, []string, bool, error)
}

// ThreadRoomRepository defines MongoDB-backed thread room queries.
type ThreadRoomRepository interface {
	GetThreadRooms(ctx context.Context, roomID string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
	GetFollowingThreadRooms(ctx context.Context, roomID, account string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
	GetUnreadThreadRooms(ctx context.Context, roomID string, parentIDs []string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
}

// HistoryService handles message history queries. Transport-agnostic.
type HistoryService struct {
	messages      MessageRepository
	subscriptions SubscriptionRepository
	threadRooms   ThreadRoomRepository
}

// New creates a HistoryService with the given repositories.
func New(msgs MessageRepository, subs SubscriptionRepository, threadRooms ThreadRoomRepository) *HistoryService {
	return &HistoryService{messages: msgs, subscriptions: subs, threadRooms: threadRooms}
}

// RegisterHandlers wires all NATS endpoints for the history service.
// Panics if any subscription fails (startup-only, fatal if broken).
func (s *HistoryService) RegisterHandlers(r *natsrouter.Router, siteID string) {
	natsrouter.Register(r, subject.MsgHistoryPattern(siteID), s.LoadHistory)
	natsrouter.Register(r, subject.MsgNextPattern(siteID), s.LoadNextMessages)
	natsrouter.Register(r, subject.MsgSurroundingPattern(siteID), s.LoadSurroundingMessages)
	natsrouter.Register(r, subject.MsgGetPattern(siteID), s.GetMessageByID)
	natsrouter.Register(r, subject.MsgThreadPattern(siteID), s.GetThreadMessages)
	natsrouter.Register(r, subject.MsgThreadParentPattern(siteID), s.GetThreadsList)
}
```

- [ ] **Step 4: Regenerate mocks**

Run: `make generate SERVICE=history-service`
Expected: `history-service/internal/service/mocks/mock_repository.go` is regenerated with `MockThreadRoomRepository` added

- [ ] **Step 5: Run tests to verify they pass**

Run: `make test SERVICE=history-service`
Expected: PASS (all existing tests compile and pass with the new `newService` signature)

- [ ] **Step 6: Commit**

```bash
git add history-service/internal/service/service.go \
    history-service/internal/service/mocks/mock_repository.go \
    history-service/internal/service/messages_test.go
git commit -m "feat(service): add ThreadRoomRepository interface, extend MessageRepository and SubscriptionRepository, update New to 3 args"
```

---

## Task 11: Implement GetThreadsList handler + unit tests

The handler wires everything together: subscription check → MongoDB thread room query (with filter type) → Cassandra bulk fetch → join.

**Files:**
- Create: `history-service/internal/service/threads_test.go`
- Create: `history-service/internal/service/threads.go`

- [ ] **Step 1: Write the failing tests**

Create `history-service/internal/service/threads_test.go`:

```go
package service_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/hmchangw/chat/history-service/internal/models"
	"github.com/hmchangw/chat/history-service/internal/mongorepo"
	pkgmodel "github.com/hmchangw/chat/pkg/model"
)

var threadBase = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

func makeThreadRooms() []pkgmodel.ThreadRoom {
	return []pkgmodel.ThreadRoom{
		{ID: "tr-1", RoomID: "r1", ThreadParentID: "p1", ThreadLastMessage: threadBase.Add(2 * time.Hour), ThreadCount: 5, ReplyAccounts: []string{"alice"}},
		{ID: "tr-2", RoomID: "r1", ThreadParentID: "p2", ThreadLastMessage: threadBase.Add(1 * time.Hour), ThreadCount: 3, ReplyAccounts: []string{"bob"}},
	}
}

func makeCassMessages() []models.Message {
	return []models.Message{
		{MessageID: "p1", RoomID: "r1", Msg: "parent 1"},
		{MessageID: "p2", RoomID: "r1", Msg: "parent 2"},
	}
}

func makeThreadPage(hasMore bool) mongorepo.OffsetPage[pkgmodel.ThreadRoom] {
	return mongorepo.OffsetPage[pkgmodel.ThreadRoom]{Data: makeThreadRooms(), HasMore: hasMore}
}

/// --- GetThreadsList: all filter ---

func TestHistoryService_GetThreadsList_All(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(makeThreadPage(false), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.InAnyOrder([]string{"p1", "p2"})).Return(makeCassMessages(), nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Type: models.ListThreadsAll, Limit: 20})
	require.NoError(t, err)
	assert.Len(t, resp.Threads, 2)
	assert.False(t, resp.HasMore)
	assert.Equal(t, "p1", resp.Threads[0].MessageID)
	assert.Equal(t, 5, resp.Threads[0].ThreadCount)
	assert.Equal(t, threadBase.Add(2*time.Hour), resp.Threads[0].ThreadLastMessage)
}

func TestHistoryService_GetThreadsList_HasMore(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(makeThreadPage(true), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(makeCassMessages(), nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 2})
	require.NoError(t, err)
	assert.True(t, resp.HasMore)
}

// --- GetThreadsList: following filter ---

func TestHistoryService_GetThreadsList_Following(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, true, nil)
	threadRooms.EXPECT().GetFollowingThreadRooms(gomock.Any(), "r1", "u1", nil, gomock.Any()).Return(makeThreadPage(false), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(makeCassMessages(), nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Type: models.ListThreadsFollowing, Limit: 20})
	require.NoError(t, err)
	assert.Len(t, resp.Threads, 2)
}

// --- GetThreadsList: unread filter ---

func TestHistoryService_GetThreadsList_Unread(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	unread := []string{"p1", "p2"}
	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, unread, true, nil)
	threadRooms.EXPECT().GetUnreadThreadRooms(gomock.Any(), "r1", unread, nil, gomock.Any()).Return(makeThreadPage(false), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(makeCassMessages(), nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Type: models.ListThreadsUnread, Limit: 20})
	require.NoError(t, err)
	assert.Len(t, resp.Threads, 2)
}

// --- GetThreadsList: not subscribed ---

func TestHistoryService_GetThreadsList_NotSubscribed(t *testing.T) {
	svc, _, subs, _ := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, false, nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.NoError(t, err)
	assert.Empty(t, resp.Threads)
	assert.False(t, resp.HasMore)
}

// --- GetThreadsList: empty thread list (no Cassandra call) ---

func TestHistoryService_GetThreadsList_EmptyThreads(t *testing.T) {
	svc, _, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(
		mongorepo.OffsetPage[pkgmodel.ThreadRoom]{Data: []pkgmodel.ThreadRoom{}, HasMore: false}, nil,
	)
	// GetMessagesByIDs must NOT be called when thread list is empty

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.NoError(t, err)
	assert.Empty(t, resp.Threads)
	assert.False(t, resp.HasMore)
}

// --- GetThreadsList: error paths ---

func TestHistoryService_GetThreadsList_SubscriptionError(t *testing.T) {
	svc, _, subs, _ := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, false, fmt.Errorf("db error"))

	_, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.Error(t, err)
	assertInternalErr(t, err, "unable to verify room access")
}

func TestHistoryService_GetThreadsList_ThreadRoomError(t *testing.T) {
	svc, _, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(
		mongorepo.OffsetPage[pkgmodel.ThreadRoom]{}, fmt.Errorf("mongo down"),
	)

	_, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.Error(t, err)
	assertInternalErr(t, err, "failed to load thread list")
}

func TestHistoryService_GetThreadsList_CassandraError(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(makeThreadPage(false), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("cassandra down"))

	_, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.Error(t, err)
	assertInternalErr(t, err, "failed to load thread parent messages")
}

func TestHistoryService_GetThreadsList_MissingParentIgnored(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(makeThreadPage(false), nil)
	// Only return p1 — p2 is missing (e.g. deleted)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(
		[]models.Message{{MessageID: "p1", RoomID: "r1", Msg: "parent 1"}}, nil,
	)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.NoError(t, err)
	assert.Len(t, resp.Threads, 1)
	assert.Equal(t, "p1", resp.Threads[0].MessageID)
}
```

- [ ] **Step 2: Run to verify it fails (red)**

Run: `make test SERVICE=history-service`
Expected: FAIL — `svc.GetThreadsList undefined`

- [ ] **Step 3: Create `history-service/internal/service/threads.go`**

```go
package service

import (
	"log/slog"

	"github.com/hmchangw/chat/history-service/internal/models"
	"github.com/hmchangw/chat/history-service/internal/mongorepo"
	pkgmodel "github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
)

// GetThreadsList returns a paginated list of thread parent messages for a room.
// NATS subject: chat.user.{account}.request.room.{roomID}.{siteID}.msg.thread.parent
func (s *HistoryService) GetThreadsList(c *natsrouter.Context, req models.GetThreadsListRequest) (*models.GetThreadsListResponse, error) {
	account := c.Param("account")
	roomID := c.Param("roomID")

	accessSince, threadUnread, subscribed, err := s.subscriptions.GetSubscriptionForThreads(c, account, roomID)
	if err != nil {
		slog.Error("checking subscription for threads", "error", err, "account", account, "roomID", roomID)
		return nil, natsrouter.ErrInternal("unable to verify room access")
	}
	if !subscribed {
		return &models.GetThreadsListResponse{Threads: []models.ThreadParentMessage{}, HasMore: false}, nil
	}

	pageReq := mongorepo.NewOffsetPageRequest(req.Offset, req.Limit)

	var threadPage mongorepo.OffsetPage[pkgmodel.ThreadRoom]
	switch req.Type {
	case models.ListThreadsFollowing:
		threadPage, err = s.threadRooms.GetFollowingThreadRooms(c, roomID, account, accessSince, pageReq)
	case models.ListThreadsUnread:
		threadPage, err = s.threadRooms.GetUnreadThreadRooms(c, roomID, threadUnread, accessSince, pageReq)
	default:
		threadPage, err = s.threadRooms.GetThreadRooms(c, roomID, accessSince, pageReq)
	}
	if err != nil {
		slog.Error("loading thread list", "error", err, "roomID", roomID, "type", req.Type)
		return nil, natsrouter.ErrInternal("failed to load thread list")
	}

	if len(threadPage.Data) == 0 {
		return &models.GetThreadsListResponse{Threads: []models.ThreadParentMessage{}, HasMore: false}, nil
	}

	parentIDs := make([]string, len(threadPage.Data))
	for i, tr := range threadPage.Data {
		parentIDs[i] = tr.ThreadParentID
	}

	cassMessages, err := s.messages.GetMessagesByIDs(c, parentIDs)
	if err != nil {
		slog.Error("loading thread parent messages", "error", err, "roomID", roomID)
		return nil, natsrouter.ErrInternal("failed to load thread parent messages")
	}

	msgByID := make(map[string]models.Message, len(cassMessages))
	for _, m := range cassMessages {
		msgByID[m.MessageID] = m
	}

	threads := make([]models.ThreadParentMessage, 0, len(threadPage.Data))
	for _, tr := range threadPage.Data {
		msg, ok := msgByID[tr.ThreadParentID]
		if !ok {
			continue
		}
		threads = append(threads, models.ThreadParentMessage{
			Message:           msg,
			ThreadCount:       tr.ThreadCount,
			ThreadLastMessage: tr.ThreadLastMessage,
			ReplyAccounts:     tr.ReplyAccounts,
		})
	}

	return &models.GetThreadsListResponse{Threads: threads, HasMore: threadPage.HasMore}, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make test SERVICE=history-service`
Expected: PASS (all unit tests including new threads_test.go)

- [ ] **Step 5: Commit**

```bash
git add history-service/internal/service/threads.go \
    history-service/internal/service/threads_test.go
git commit -m "feat(service): implement GetThreadsList handler with all/following/unread filters"
```

---

## Task 12: Wire everything in main.go

Connect `ThreadRoomRepo` to the service and call `EnsureIndexes` on startup.

**Files:**
- Modify: `history-service/cmd/main.go`

- [ ] **Step 1: Update `history-service/cmd/main.go`**

Replace the three lines that create `mongoRepo` and `svc`:

```go
// OLD:
mongoRepo := mongorepo.NewSubscriptionRepo(mongoClient.Database(cfg.Mongo.DB))
svc := service.New(cassRepo, mongoRepo)

// NEW:
subRepo := mongorepo.NewSubscriptionRepo(mongoClient.Database(cfg.Mongo.DB))
threadRoomRepo := mongorepo.NewThreadRoomRepo(mongoClient.Database(cfg.Mongo.DB))
if err := threadRoomRepo.EnsureIndexes(ctx); err != nil {
    slog.Error("ensure thread_rooms indexes failed", "error", err)
    os.Exit(1)
}
svc := service.New(cassRepo, subRepo, threadRoomRepo)
```

- [ ] **Step 2: Verify compilation**

Run: `make build SERVICE=history-service`
Expected: binary builds without errors

- [ ] **Step 3: Run all unit tests**

Run: `make test SERVICE=history-service`
Expected: PASS

- [ ] **Step 4: Run lint**

Run: `make lint`
Expected: no errors

- [ ] **Step 5: Commit**

```bash
git add history-service/cmd/main.go
git commit -m "feat(history-service): wire ThreadRoomRepo and register GetThreadsList endpoint"
```

---

## Task 13: Final verification

- [ ] **Step 1: Run all unit tests**

Run: `make test`
Expected: PASS — all packages including `pkg/model`, `pkg/subject`, `history-service`

- [ ] **Step 2: Run integration tests for history-service**

Run: `make test-integration SERVICE=history-service`
Expected: PASS — Cassandra + MongoDB containers start, all tests pass

- [ ] **Step 3: Run lint on the full repo**

Run: `make lint`
Expected: no errors

- [ ] **Step 4: Push the branch**

```bash
git push -u origin claude/add-history-nats-endpoint-0OVot
```

---

