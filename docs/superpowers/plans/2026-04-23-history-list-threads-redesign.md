# History Service — GetThreadsList Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Redesign the unread-threads query to use a MongoDB aggregation pipeline with `$facet`-based pagination, remove `Subscription.ThreadUnread`, and replace `HasMore` with `Total int64` across the mongorepo layer.

**Architecture:** All three thread-list filter modes (`all`, `following`, `unread`) go through a new `AggregatePaged` method on `Collection[T]` that appends a `$facet` stage for accurate total counts. Pipeline builder functions in `pipelines.go` encode query logic; repo methods stay trivially thin. The unread query drives from `threadRooms` and `$lookup`s into `threadSubscriptions`, leveraging the existing `{threadRoomId}` index.

**Tech Stack:** Go 1.25, `go.mongodb.org/mongo-driver/v2`, `go.uber.org/mock`, `github.com/stretchr/testify`, `testcontainers-go`.

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `pkg/model/subscription.go` | Modify | Remove `ThreadUnread []string` field |
| `pkg/model/model_test.go` | Modify | Drop `ThreadUnread` from `TestSubscriptionJSON` fixture |
| `history-service/internal/mongorepo/collection.go` | Modify | Add `Aggregate`, `AggregatePaged`, `facetResult[T]`, `countResult` |
| `history-service/internal/mongorepo/pipelines.go` | **Create** | `allThreadsPipeline`, `followingThreadsPipeline`, `unreadThreadsPipeline` |
| `history-service/internal/mongorepo/pagination.go` | Modify | `HasMore bool` → `Total int64`; remove `Paginate()` and `paginateOptions()` |
| `history-service/internal/mongorepo/pagination_test.go` | Modify | Remove `Paginate` tests; add `TestEmptyPage` |
| `history-service/internal/mongorepo/threadroom.go` | Modify | Rewrite all three repo methods to use `AggregatePaged`; add `threadSubscriptions` field; fix `insertThreadRoom` collection name in comment; update `EnsureIndexes` comment |
| `history-service/internal/mongorepo/threadroom_test.go` | Modify | Fix `insertThreadRoom` helper to use `"threadRooms"`; rewrite unread tests with new scenarios; replace `HasMore` assertions with `Total` |
| `history-service/internal/mongorepo/subscription.go` | Modify | Drop `[]string` from `GetSubscriptionForThreads` return; drop `threadUnread` projection |
| `history-service/internal/mongorepo/subscription_test.go` | Modify | Add `Aggregate`/`AggregatePaged` integration tests; update `GetSubscriptionForThreads` tests |
| `history-service/internal/service/service.go` | Modify | Update `SubscriptionRepository` and `ThreadRoomRepository` interface signatures |
| `history-service/internal/service/mocks/mock_repository.go` | Regenerate | `make generate SERVICE=history-service` |
| `history-service/internal/models/threads.go` | Modify | `HasMore bool` → `Total int64` in `GetThreadsListResponse` |
| `history-service/internal/service/threads.go` | Modify | Update handler to use new signatures; propagate `Total` |
| `history-service/internal/service/threads_test.go` | Modify | Update all mock expectations and assertions |

---

## Task 1: Remove `Subscription.ThreadUnread` from model test

**Context:** `pkg/model/subscription.go` has a `ThreadUnread []string` field that will be removed in Task 4. The `TestSubscriptionJSON` fixture currently round-trips this field. We update the test now so the model change in Task 4 does not require revisiting this file. The struct field itself stays until Task 4.

**Files:**
- Modify: `pkg/model/model_test.go` (lines 325–351 — `TestSubscriptionJSON`)

- [ ] **Step 1: Update `TestSubscriptionJSON` to remove `ThreadUnread`**

In `pkg/model/model_test.go`, replace the `TestSubscriptionJSON` function body. The current fixture has `ThreadUnread: []string{"parent-1", "parent-2"}`. Remove that line. The rest of the function is unchanged.

Replace this:
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
```

With this (drop `ThreadUnread` line only):
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
	}
```

- [ ] **Step 2: Run tests and confirm they pass**

```bash
make test SERVICE=pkg/model
```

Expected: `ok  github.com/hmchangw/chat/pkg/model`

- [ ] **Step 3: Commit**

```bash
git add pkg/model/model_test.go
git commit -m "test(model): remove ThreadUnread fixture from TestSubscriptionJSON"
```

---

## Task 2: Add `Total` to `OffsetPage[T]` + add `Aggregate` / `AggregatePaged` to `Collection[T]`

**Context:** `AggregatePaged` will return `OffsetPage[T]{Data: data, Total: total}` so `Total int64` must exist on the struct before `AggregatePaged` compiles. We add it alongside the existing `HasMore bool` — purely additive. `HasMore` is removed in Task 6 once all callers have migrated. `Aggregate` is the simpler non-paginated variant. Both methods wrap cursor handling so no repo or test code ever touches a cursor.

**Files:**
- Modify: `history-service/internal/mongorepo/pagination.go` (add `Total int64` to `OffsetPage[T]`)
- Modify: `history-service/internal/mongorepo/collection.go` (add `Aggregate`, `AggregatePaged`, private types)
- Modify: `history-service/internal/mongorepo/subscription_test.go` (add integration tests)

- [ ] **Step 1: Write failing integration tests for `Aggregate` and `AggregatePaged`**

Append the following test functions to `history-service/internal/mongorepo/subscription_test.go` (it already has `//go:build integration`, `setupMongo`, and `testDoc` type defined — add below the existing tests):

```go
// --- Collection[T].Aggregate integration tests ---

func TestCollection_Aggregate_ReturnsMatchingDocs(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("agg_docs"))

	_, err := db.Collection("agg_docs").InsertMany(ctx, []interface{}{
		testDoc{ID: "d1", Name: "Alice", Age: 30},
		testDoc{ID: "d2", Name: "Bob", Age: 25},
		testDoc{ID: "d3", Name: "Charlie", Age: 35},
	})
	require.NoError(t, err)

	pipeline := bson.A{
		bson.D{{Key: "$match", Value: bson.M{"age": bson.M{"$gte": 30}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "age", Value: 1}}}},
	}

	results, err := col.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "d1", results[0].ID) // Alice, age 30
	assert.Equal(t, "d3", results[1].ID) // Charlie, age 35
}

func TestCollection_Aggregate_Empty_ReturnsEmptySlice(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("agg_docs_empty"))

	pipeline := bson.A{bson.D{{Key: "$match", Value: bson.M{"name": "Nobody"}}}}

	results, err := col.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	assert.NotNil(t, results)
	assert.Empty(t, results)
}

// --- Collection[T].AggregatePaged integration tests ---

func TestCollection_AggregatePaged_ReturnsDataAndTotal(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("paged_docs"))

	_, err := db.Collection("paged_docs").InsertMany(ctx, []interface{}{
		testDoc{ID: "d1", Name: "A", Age: 1},
		testDoc{ID: "d2", Name: "B", Age: 2},
		testDoc{ID: "d3", Name: "C", Age: 3},
		testDoc{ID: "d4", Name: "D", Age: 4},
		testDoc{ID: "d5", Name: "E", Age: 5},
	})
	require.NoError(t, err)

	pipeline := bson.A{
		bson.D{{Key: "$sort", Value: bson.D{{Key: "age", Value: 1}}}},
	}

	// Page 1: offset 0, limit 2 — expects total=5, data=[d1,d2]
	page, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(0, 2))
	require.NoError(t, err)
	assert.Equal(t, int64(5), page.Total)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "d1", page.Data[0].ID)
	assert.Equal(t, "d2", page.Data[1].ID)

	// Page 2: offset 2, limit 2 — expects total=5, data=[d3,d4]
	page2, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(2, 2))
	require.NoError(t, err)
	assert.Equal(t, int64(5), page2.Total)
	require.Len(t, page2.Data, 2)
	assert.Equal(t, "d3", page2.Data[0].ID)
	assert.Equal(t, "d4", page2.Data[1].ID)
}

func TestCollection_AggregatePaged_EmptyCollection(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("paged_docs_empty"))

	pipeline := bson.A{bson.D{{Key: "$match", Value: bson.M{"name": "Nobody"}}}}

	page, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Equal(t, int64(0), page.Total)
	assert.NotNil(t, page.Data)
	assert.Empty(t, page.Data)
}
```

- [ ] **Step 2: Run integration tests and confirm they FAIL**

```bash
make test-integration SERVICE=history-service
```

Expected: compilation error — `col.Aggregate` and `col.AggregatePaged` are undefined.

- [ ] **Step 3: Add `Total int64` to `OffsetPage[T]` in `pagination.go`**

In `history-service/internal/mongorepo/pagination.go`, replace the `OffsetPage` struct:

```go
// OffsetPage is the result of a paginated MongoDB query.
type OffsetPage[T any] struct {
	Data    []T
	HasMore bool  // set by Paginate via n+1; removed in Task 6 once all callers use AggregatePaged
	Total   int64 // set by AggregatePaged via $facet
}
```

No other changes to `pagination.go` in this step.

- [ ] **Step 4: Add `Aggregate`, `AggregatePaged`, `facetResult[T]`, and `countResult` to `collection.go`**

Append the following to `history-service/internal/mongorepo/collection.go` (after the `Raw()` method). The existing imports (`context`, `errors`, `fmt`, `bson`, `mongo`) are already present; no new imports are needed:

```go
// Aggregate runs the pipeline and decodes every result row into []T.
// Returns an empty slice (not nil) when no documents match.
// The pipeline encodes all query logic — sort, filter, project — so no
// QueryOption is accepted here.
func (c *Collection[T]) Aggregate(ctx context.Context, pipeline bson.A) ([]T, error) {
	cursor, err := c.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("aggregating %s: %w", c.name, err)
	}
	var results []T
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decoding %s aggregate: %w", c.name, err)
	}
	if results == nil {
		results = []T{}
	}
	return results, nil
}

// AggregatePaged appends a $facet stage to the pipeline that produces a
// paginated data branch (skip + limit) and a total-count branch.
// Returns OffsetPage[T] with accurate Total from the count branch.
func (c *Collection[T]) AggregatePaged(ctx context.Context, pipeline bson.A, req OffsetPageRequest) (OffsetPage[T], error) {
	facet := bson.D{{Key: "$facet", Value: bson.M{
		"data": bson.A{
			bson.D{{Key: "$skip", Value: req.Offset}},
			bson.D{{Key: "$limit", Value: req.Limit}},
		},
		"total": bson.A{
			bson.D{{Key: "$count", Value: "count"}},
		},
	}}}
	full := append(pipeline, facet)

	cursor, err := c.col.Aggregate(ctx, full)
	if err != nil {
		return OffsetPage[T]{}, fmt.Errorf("aggregating %s: %w", c.name, err)
	}
	var wrapper []facetResult[T]
	if err := cursor.All(ctx, &wrapper); err != nil {
		return OffsetPage[T]{}, fmt.Errorf("decoding %s facet: %w", c.name, err)
	}
	if len(wrapper) == 0 {
		return EmptyPage[T](), nil
	}
	data := wrapper[0].Data
	if data == nil {
		data = []T{}
	}
	var total int64
	if len(wrapper[0].Total) > 0 {
		total = wrapper[0].Total[0].Count
	}
	return OffsetPage[T]{Data: data, Total: total}, nil
}

// facetResult decodes the single document emitted by the $facet stage.
type facetResult[T any] struct {
	Data  []T           `bson:"data"`
	Total []countResult `bson:"total"`
}

type countResult struct {
	Count int64 `bson:"count"`
}
```

- [ ] **Step 5: Run integration tests and confirm they PASS**

```bash
make test-integration SERVICE=history-service
```

Expected: the four new `TestCollection_Aggregate_*` and `TestCollection_AggregatePaged_*` tests pass. (Existing tests also pass.)

- [ ] **Step 6: Run unit tests to confirm no regression**

```bash
make test SERVICE=history-service
```

Expected: `ok  github.com/hmchangw/chat/history-service`

- [ ] **Step 7: Commit**

```bash
git add history-service/internal/mongorepo/pagination.go \
        history-service/internal/mongorepo/collection.go \
        history-service/internal/mongorepo/subscription_test.go
git commit -m "feat(mongorepo): add Total to OffsetPage and Aggregate/AggregatePaged to Collection[T]"
```

---

## Task 3: Create `mongorepo/pipelines.go`

**Context:** Pipeline builder functions encode the MongoDB aggregation logic for each filter mode. They return `bson.A` ready to pass to `AggregatePaged`. They never embed `$skip`, `$limit`, or `$count` — that is `AggregatePaged`'s job. `threadRoomSort` referenced here is the package-level variable already defined in `threadroom.go`.

**Files:**
- Create: `history-service/internal/mongorepo/pipelines.go`

- [ ] **Step 1: Create `pipelines.go` with all three builder functions**

Create `history-service/internal/mongorepo/pipelines.go` with this exact content:

```go
package mongorepo

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// allThreadsPipeline matches every thread room in the given room, sorted by
// most recent activity. Optionally restricts to parents created at/after
// accessSince (used when the user joined mid-history).
func allThreadsPipeline(roomID string, accessSince *time.Time) bson.A {
	match := bson.M{"roomId": roomID}
	if accessSince != nil {
		match["threadParentCreatedAt"] = bson.M{"$gte": *accessSince}
	}
	return bson.A{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$sort", Value: threadRoomSort}},
	}
}

// followingThreadsPipeline matches thread rooms where account appears in
// replyAccounts (i.e. the user has replied in the thread).
func followingThreadsPipeline(roomID, account string, accessSince *time.Time) bson.A {
	match := bson.M{"roomId": roomID, "replyAccounts": account}
	if accessSince != nil {
		match["threadParentCreatedAt"] = bson.M{"$gte": *accessSince}
	}
	return bson.A{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$sort", Value: threadRoomSort}},
	}
}

// unreadThreadsPipeline returns thread rooms that are unread for userAccount:
//  1. Match thread rooms in the room (uses {roomId, lastMsgAt, threadParentCreatedAt} index).
//  2. $lookup into threadSubscriptions by threadRoomId (uses {threadRoomId, userId} index
//     leading field), filtered to the requesting user.
//  3. Keep only threads where the user has a subscription.
//  4. Keep only threads where lastMsgAt > lastSeenAt OR lastSeenAt is nil
//     (nil = never opened = always unread).
//  5. Drop the joined sub field before returning.
func unreadThreadsPipeline(roomID, userAccount string, accessSince *time.Time) bson.A {
	matchRoom := bson.M{"roomId": roomID}
	if accessSince != nil {
		matchRoom["threadParentCreatedAt"] = bson.M{"$gte": *accessSince}
	}
	return bson.A{
		bson.D{{Key: "$match", Value: matchRoom}},
		bson.D{{Key: "$lookup", Value: bson.M{
			"from": "threadSubscriptions",
			"let":  bson.M{"tr": "$_id"},
			"pipeline": bson.A{
				bson.D{{Key: "$match", Value: bson.M{
					"$expr":       bson.M{"$eq": bson.A{"$threadRoomId", "$$tr"}},
					"userAccount": userAccount,
				}}},
				bson.D{{Key: "$project", Value: bson.M{"lastSeenAt": 1, "_id": 0}}},
			},
			"as": "sub",
		}}},
		// user must have a threadSubscription for this thread
		bson.D{{Key: "$match", Value: bson.M{"sub": bson.M{"$ne": bson.A{}}}}},
		// thread is unread when lastSeenAt is nil OR lastMsgAt > lastSeenAt
		bson.D{{Key: "$match", Value: bson.M{
			"$expr": bson.M{"$or": bson.A{
				bson.M{"$eq": bson.A{bson.M{"$arrayElemAt": bson.A{"$sub.lastSeenAt", 0}}, nil}},
				bson.M{"$gt": bson.A{"$lastMsgAt", bson.M{"$arrayElemAt": bson.A{"$sub.lastSeenAt", 0}}}},
			}},
		}}},
		bson.D{{Key: "$project", Value: bson.M{"sub": 0}}},
		bson.D{{Key: "$sort", Value: threadRoomSort}},
	}
}
```

- [ ] **Step 2: Verify the package compiles**

```bash
make build SERVICE=history-service
```

Expected: binary builds successfully with no errors.

- [ ] **Step 3: Commit**

```bash
git add history-service/internal/mongorepo/pipelines.go
git commit -m "feat(mongorepo): add pipeline builder functions for thread room queries"
```

---

## Task 4: Rewrite `GetThreadRooms` and `GetFollowingThreadRooms` to use `AggregatePaged`

**Context:** These two methods keep their exact signatures — only the implementation changes. They switch from `FindMany` + `Paginate` to `AggregatePaged` + a pipeline builder. The integration tests are updated to assert `page.Total` instead of `page.HasMore`. `GetUnreadThreadRooms` is not touched here — it changes in Task 5 where the full interface + service-layer cascade is handled atomically.

**Files:**
- Modify: `history-service/internal/mongorepo/threadroom.go`
- Modify: `history-service/internal/mongorepo/threadroom_test.go`

- [ ] **Step 1: Rewrite `threadroom.go` — constructor, `GetThreadRooms`, `GetFollowingThreadRooms`**

In `history-service/internal/mongorepo/threadroom.go`:

1. Add `threadSubscriptions *Collection[model.ThreadSubscription]` to the struct and constructor:

```go
type ThreadRoomRepo struct {
	threadRooms         *Collection[model.ThreadRoom]
	threadSubscriptions *Collection[model.ThreadSubscription]
}

func NewThreadRoomRepo(db *mongo.Database) *ThreadRoomRepo {
	return &ThreadRoomRepo{
		threadRooms:         NewCollection[model.ThreadRoom](db.Collection(threadRoomsCollection)),
		threadSubscriptions: NewCollection[model.ThreadSubscription](db.Collection("threadSubscriptions")),
	}
}
```

2. Rewrite `GetThreadRooms` (signature unchanged):

```go
func (r *ThreadRoomRepo) GetThreadRooms(ctx context.Context, roomID string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[model.ThreadRoom], error) {
	page, err := r.threadRooms.AggregatePaged(ctx, allThreadsPipeline(roomID, accessSince), req)
	if err != nil {
		return OffsetPage[model.ThreadRoom]{}, fmt.Errorf("querying thread rooms: %w", err)
	}
	return page, nil
}
```

3. Rewrite `GetFollowingThreadRooms` (signature unchanged):

```go
func (r *ThreadRoomRepo) GetFollowingThreadRooms(ctx context.Context, roomID, account string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[model.ThreadRoom], error) {
	page, err := r.threadRooms.AggregatePaged(ctx, followingThreadsPipeline(roomID, account, accessSince), req)
	if err != nil {
		return OffsetPage[model.ThreadRoom]{}, fmt.Errorf("querying following thread rooms: %w", err)
	}
	return page, nil
}
```

4. Also update the `EnsureIndexes` comment for the third index — it's retained for parent-lookup callers, not the unread query:

```go
// GetUnreadThreadRooms (aggregation drives from first compound index, not this one)
{Keys: bson.D{
    {Key: "roomId", Value: 1},
    {Key: "parentMessageId", Value: 1},
    {Key: "lastMsgAt", Value: -1},
    {Key: "threadParentCreatedAt", Value: 1},
}},
```

Leave `GetUnreadThreadRooms` **unchanged** — it is rewritten in Task 5.

- [ ] **Step 2: Build to confirm compilation**

```bash
make build SERVICE=history-service
```

Expected: binary builds with no errors.

- [ ] **Step 3: Rewrite `threadroom_test.go` integration tests for `all` and `following`**

In `history-service/internal/mongorepo/threadroom_test.go`:

1. Fix `insertThreadRoom` helper — it currently writes to `"thread_rooms"` but the repo reads from `"threadRooms"`:

```go
func insertThreadRoom(t *testing.T, db *mongo.Database, tr model.ThreadRoom) {
	t.Helper()
	_, err := db.Collection("threadRooms").InsertOne(context.Background(), tr)
	require.NoError(t, err)
}
```

2. Replace `TestThreadRoomRepo_GetThreadRooms` with:

```go
func TestThreadRoomRepo_GetThreadRooms(t *testing.T) {
	db := setupMongo(t)
	repo := NewThreadRoomRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 4 rooms in r1: tr-old, tr-1, tr-2, tr-3 (sorted by lastMsgAt desc: tr-old first)
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-old", RoomID: "r1", ParentMessageID: "p0", ThreadParentCreatedAt: base.Add(-1 * time.Hour), LastMsgAt: base.Add(5 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-1", RoomID: "r1", ParentMessageID: "p1", ThreadParentCreatedAt: base.Add(1 * time.Hour), LastMsgAt: base.Add(3 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-2", RoomID: "r1", ParentMessageID: "p2", ThreadParentCreatedAt: base.Add(2 * time.Hour), LastMsgAt: base.Add(1 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-3", RoomID: "r1", ParentMessageID: "p3", ThreadParentCreatedAt: base.Add(3 * time.Hour), LastMsgAt: base.Add(2 * time.Hour), CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-other", RoomID: "r2", ParentMessageID: "p4", ThreadParentCreatedAt: base.Add(1 * time.Hour), LastMsgAt: base.Add(4 * time.Hour), CreatedAt: base, UpdatedAt: base})

	// Page 1: limit 2 — gets tr-old (5h) and tr-1 (3h); total is 4
	page, err := repo.GetThreadRooms(ctx, "r1", nil, NewOffsetPageRequest(0, 2))
	require.NoError(t, err)
	assert.Equal(t, int64(4), page.Total)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "tr-old", page.Data[0].ID)
	assert.Equal(t, "tr-1", page.Data[1].ID)

	// Page 2: offset 2, limit 2 — gets tr-3 (2h) and tr-2 (1h); total still 4
	page2, err := repo.GetThreadRooms(ctx, "r1", nil, NewOffsetPageRequest(2, 2))
	require.NoError(t, err)
	assert.Equal(t, int64(4), page2.Total)
	require.Len(t, page2.Data, 2)

	// accessSince = base: excludes tr-old (threadParentCreatedAt = base-1h < base)
	afterBase := base
	page3, err := repo.GetThreadRooms(ctx, "r1", &afterBase, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Equal(t, int64(3), page3.Total)
	for _, tr := range page3.Data {
		assert.NotEqual(t, "tr-old", tr.ID)
	}
}
```

3. Replace `TestThreadRoomRepo_GetFollowingThreadRooms` with:

```go
func TestThreadRoomRepo_GetFollowingThreadRooms(t *testing.T) {
	db := setupMongo(t)
	repo := NewThreadRoomRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-1", RoomID: "r1", ParentMessageID: "p1", ThreadParentCreatedAt: base.Add(1 * time.Hour), LastMsgAt: base.Add(2 * time.Hour), ReplyAccounts: []string{"alice", "bob"}, CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-2", RoomID: "r1", ParentMessageID: "p2", ThreadParentCreatedAt: base.Add(2 * time.Hour), LastMsgAt: base.Add(1 * time.Hour), ReplyAccounts: []string{"bob"}, CreatedAt: base, UpdatedAt: base})
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-3", RoomID: "r1", ParentMessageID: "p3", ThreadParentCreatedAt: base.Add(3 * time.Hour), LastMsgAt: base.Add(3 * time.Hour), ReplyAccounts: []string{"alice"}, CreatedAt: base, UpdatedAt: base})

	page, err := repo.GetFollowingThreadRooms(ctx, "r1", "alice", nil, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Equal(t, int64(2), page.Total)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "tr-3", page.Data[0].ID) // lastMsgAt 3h > 2h
	assert.Equal(t, "tr-1", page.Data[1].ID)
}
```

Leave `TestThreadRoomRepo_GetUnreadThreadRooms` **unchanged** — it is replaced in Task 5.

- [ ] **Step 4: Run integration tests and confirm they pass**

```bash
make test-integration SERVICE=history-service
```

Expected: all tests pass including the rewritten `GetThreadRooms` and `GetFollowingThreadRooms` tests.

- [ ] **Step 5: Run unit tests**

```bash
make test SERVICE=history-service
```

Expected: `ok  github.com/hmchangw/chat/history-service`

- [ ] **Step 6: Commit**

```bash
git add history-service/internal/mongorepo/threadroom.go \
        history-service/internal/mongorepo/threadroom_test.go
git commit -m "refactor(mongorepo): migrate GetThreadRooms and GetFollowingThreadRooms to AggregatePaged"
```

---

## Task 5: Atomic interface change — unread query, subscription, service layer

**Context:** All remaining signature changes must land in one commit because Go checks interface satisfaction at the wiring point in `main.go`. This task changes `GetUnreadThreadRooms` (removes `parentIDs`, adds `account`), drops `[]string` from `GetSubscriptionForThreads`, updates the service interfaces, regenerates mocks, and updates the service handler and all its tests. `Subscription.ThreadUnread` is also removed here since `subscription.go` is being touched anyway.

**Files:**
- Modify: `pkg/model/subscription.go`
- Modify: `history-service/internal/mongorepo/subscription.go`
- Modify: `history-service/internal/mongorepo/subscription_test.go`
- Modify: `history-service/internal/mongorepo/threadroom.go`
- Modify: `history-service/internal/mongorepo/threadroom_test.go`
- Modify: `history-service/internal/service/service.go`
- Regenerate: `history-service/internal/service/mocks/mock_repository.go`
- Modify: `history-service/internal/models/threads.go`
- Modify: `history-service/internal/service/threads.go`
- Modify: `history-service/internal/service/threads_test.go`

- [ ] **Step 1: Remove `ThreadUnread` from `pkg/model/subscription.go`**

Replace the `Subscription` struct in `pkg/model/subscription.go`:

```go
type Subscription struct {
	ID                 string           `json:"id"                           bson:"_id"`
	User               SubscriptionUser `json:"u"                            bson:"u"`
	RoomID             string           `json:"roomId"                       bson:"roomId"`
	SiteID             string           `json:"siteId"                       bson:"siteId"`
	Roles              []Role           `json:"roles"                        bson:"roles"`
	HistorySharedSince *time.Time       `json:"historySharedSince,omitempty" bson:"historySharedSince,omitempty"`
	JoinedAt           time.Time        `json:"joinedAt"                     bson:"joinedAt"`
	LastSeenAt         time.Time        `json:"lastSeenAt"                   bson:"lastSeenAt"`
	HasMention         bool             `json:"hasMention"                   bson:"hasMention"`
}
```

- [ ] **Step 2: Update `subscription.go` — drop `[]string` from `GetSubscriptionForThreads`**

In `history-service/internal/mongorepo/subscription.go`, replace `GetSubscriptionForThreads`:

```go
// GetSubscriptionForThreads returns the HistorySharedSince timestamp for thread access checks.
// Returns (nil, true, nil) when subscribed but no HSS is set.
// Returns (nil, false, nil) when not subscribed.
func (r *SubscriptionRepo) GetSubscriptionForThreads(ctx context.Context, account, roomID string) (*time.Time, bool, error) {
	sub, err := r.subscriptions.FindOne(ctx,
		bson.M{"u.account": account, "roomId": roomID},
		WithProjection(bson.M{"historySharedSince": 1, "_id": 0}),
	)
	if err != nil {
		return nil, false, err
	}
	if sub == nil {
		return nil, false, nil
	}
	return sub.HistorySharedSince, true, nil
}
```

- [ ] **Step 3: Update `subscription_test.go` — remove `ThreadUnread` integration tests**

In `history-service/internal/mongorepo/subscription_test.go`:

1. In `TestSubscriptionRepo_GetSubscriptionForThreads`, remove `ThreadUnread: []string{"p1", "p2"}` from the insert fixture, change the call to 3-return form, and drop the `tunread` assertion:

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
	})
	require.NoError(t, err)

	gotHSS, subscribed, err := repo.GetSubscriptionForThreads(ctx, "alice", "r1")
	require.NoError(t, err)
	assert.True(t, subscribed)
	require.NotNil(t, gotHSS)
	assert.Equal(t, hss.UTC(), gotHSS.UTC())
}
```

2. In `TestSubscriptionRepo_GetSubscriptionForThreads_NotSubscribed`, change to 3-return:

```go
func TestSubscriptionRepo_GetSubscriptionForThreads_NotSubscribed(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	_, subscribed, err := repo.GetSubscriptionForThreads(ctx, "alice", "r-none")
	require.NoError(t, err)
	assert.False(t, subscribed)
}
```

3. Delete `TestSubscriptionRepo_GetSubscriptionForThreads_NilThreadUnread` entirely — it tested the `[]string` return which no longer exists.

- [ ] **Step 4: Rewrite `GetUnreadThreadRooms` in `threadroom.go`**

Replace `GetUnreadThreadRooms` in `history-service/internal/mongorepo/threadroom.go`:

```go
// GetUnreadThreadRooms returns paginated thread rooms that have unread activity for
// account. A thread is unread when the user has a threadSubscription AND
// (threadRoom.lastMsgAt > sub.lastSeenAt OR sub.lastSeenAt is nil).
func (r *ThreadRoomRepo) GetUnreadThreadRooms(ctx context.Context, account, roomID string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[model.ThreadRoom], error) {
	page, err := r.threadRooms.AggregatePaged(ctx, unreadThreadsPipeline(roomID, account, accessSince), req)
	if err != nil {
		return OffsetPage[model.ThreadRoom]{}, fmt.Errorf("querying unread thread rooms: %w", err)
	}
	return page, nil
}
```

- [ ] **Step 5: Rewrite unread integration test in `threadroom_test.go`**

Replace `TestThreadRoomRepo_GetUnreadThreadRooms` in `history-service/internal/mongorepo/threadroom_test.go`. Also add the `insertThreadSubscription` helper and `timePtr` helper before the test:

```go
func insertThreadSubscription(t *testing.T, db *mongo.Database, ts model.ThreadSubscription) {
	t.Helper()
	_, err := db.Collection("threadSubscriptions").InsertOne(context.Background(), ts)
	require.NoError(t, err)
}

func timePtr(t time.Time) *time.Time { return &t }

func TestThreadRoomRepo_GetUnreadThreadRooms(t *testing.T) {
	db := setupMongo(t)
	repo := NewThreadRoomRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// tr-1: alice has sub, lastMsgAt (5h) > lastSeenAt (3h) → unread
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-1", RoomID: "r1", ParentMessageID: "p1",
		ThreadParentCreatedAt: base.Add(1 * time.Hour), LastMsgAt: base.Add(5 * time.Hour),
		CreatedAt: base, UpdatedAt: base})
	insertThreadSubscription(t, db, model.ThreadSubscription{ID: "ts-1", ThreadRoomID: "tr-1",
		RoomID: "r1", ParentMessageID: "p1", UserID: "u1", UserAccount: "alice",
		SiteID: "site-local", LastSeenAt: timePtr(base.Add(3 * time.Hour)),
		CreatedAt: base, UpdatedAt: base})

	// tr-2: alice has sub, lastMsgAt (2h) < lastSeenAt (4h) → read, excluded
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-2", RoomID: "r1", ParentMessageID: "p2",
		ThreadParentCreatedAt: base.Add(2 * time.Hour), LastMsgAt: base.Add(2 * time.Hour),
		CreatedAt: base, UpdatedAt: base})
	insertThreadSubscription(t, db, model.ThreadSubscription{ID: "ts-2", ThreadRoomID: "tr-2",
		RoomID: "r1", ParentMessageID: "p2", UserID: "u1", UserAccount: "alice",
		SiteID: "site-local", LastSeenAt: timePtr(base.Add(4 * time.Hour)),
		CreatedAt: base, UpdatedAt: base})

	// tr-3: alice has sub, nil lastSeenAt → always unread
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-3", RoomID: "r1", ParentMessageID: "p3",
		ThreadParentCreatedAt: base.Add(3 * time.Hour), LastMsgAt: base.Add(3 * time.Hour),
		CreatedAt: base, UpdatedAt: base})
	insertThreadSubscription(t, db, model.ThreadSubscription{ID: "ts-3", ThreadRoomID: "tr-3",
		RoomID: "r1", ParentMessageID: "p3", UserID: "u1", UserAccount: "alice",
		SiteID: "site-local", LastSeenAt: nil, CreatedAt: base, UpdatedAt: base})

	// tr-4: alice has NO sub → excluded
	insertThreadRoom(t, db, model.ThreadRoom{ID: "tr-4", RoomID: "r1", ParentMessageID: "p4",
		ThreadParentCreatedAt: base.Add(4 * time.Hour), LastMsgAt: base.Add(4 * time.Hour),
		CreatedAt: base, UpdatedAt: base})

	page, err := repo.GetUnreadThreadRooms(ctx, "alice", "r1", nil, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	// tr-1 (unread) and tr-3 (nil lastSeenAt); sorted lastMsgAt desc: tr-1 (5h) first
	assert.Equal(t, int64(2), page.Total)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "tr-1", page.Data[0].ID)
	assert.Equal(t, "tr-3", page.Data[1].ID)

	// accessSince excludes tr-1 (threadParentCreatedAt = 1h < 2h threshold)
	since := base.Add(2 * time.Hour)
	page2, err := repo.GetUnreadThreadRooms(ctx, "alice", "r1", &since, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Equal(t, int64(1), page2.Total)
	assert.Equal(t, "tr-3", page2.Data[0].ID)

	// user with no subscriptions → zero results
	pageNone, err := repo.GetUnreadThreadRooms(ctx, "nobody", "r1", nil, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Equal(t, int64(0), pageNone.Total)
	assert.Empty(t, pageNone.Data)
}
```

- [ ] **Step 6: Update service interfaces in `service/service.go`**

In `history-service/internal/service/service.go`, update both interface declarations:

```go
// SubscriptionRepository defines MongoDB-backed subscription lookups.
type SubscriptionRepository interface {
	GetHistorySharedSince(ctx context.Context, account, roomID string) (*time.Time, bool, error)
	GetSubscriptionForThreads(ctx context.Context, account, roomID string) (*time.Time, bool, error)
}

// ThreadRoomRepository defines MongoDB-backed thread room queries.
type ThreadRoomRepository interface {
	GetThreadRooms(ctx context.Context, roomID string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
	GetFollowingThreadRooms(ctx context.Context, roomID, account string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
	GetUnreadThreadRooms(ctx context.Context, account, roomID string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
}
```

- [ ] **Step 7: Regenerate mocks**

```bash
make generate SERVICE=history-service
```

Expected: `history-service/internal/service/mocks/mock_repository.go` is regenerated with updated signatures. Verify it compiles:

```bash
make build SERVICE=history-service
```

Expected: build fails because `service/threads.go` still uses old signatures. That is expected — fix it in the next step.

- [ ] **Step 8: Update `models/threads.go` — replace `HasMore` with `Total`**

In `history-service/internal/models/threads.go`, replace the response struct:

```go
// GetThreadsListResponse is the NATS response for GetThreadsList.
// Threads are ordered by most recent reply activity (MongoDB sort), returned as plain messages.
type GetThreadsListResponse struct {
	Threads []Message `json:"threads"`
	Total   int64     `json:"total"`
}
```

- [ ] **Step 9: Rewrite `service/threads.go`**

Replace the entire file `history-service/internal/service/threads.go` with:

```go
package service

import (
	"fmt"
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

	accessSince, subscribed, err := s.subscriptions.GetSubscriptionForThreads(c, account, roomID)
	if err != nil {
		slog.Error("checking subscription for threads", "error", err, "account", account, "roomID", roomID)
		return nil, natsrouter.ErrInternal("unable to verify room access")
	}
	if !subscribed {
		return &models.GetThreadsListResponse{Threads: []models.Message{}, Total: 0}, nil
	}

	pageReq := mongorepo.NewOffsetPageRequest(req.Offset, req.Limit)

	var threadPage mongorepo.OffsetPage[pkgmodel.ThreadRoom]
	switch req.Type {
	case models.ListThreadsAll:
		threadPage, err = s.threadRooms.GetThreadRooms(c, roomID, accessSince, pageReq)
	case models.ListThreadsFollowing:
		threadPage, err = s.threadRooms.GetFollowingThreadRooms(c, roomID, account, accessSince, pageReq)
	case models.ListThreadsUnread:
		threadPage, err = s.threadRooms.GetUnreadThreadRooms(c, account, roomID, accessSince, pageReq)
	default:
		return nil, natsrouter.ErrBadRequest(fmt.Sprintf("invalid thread list type: %q", req.Type))
	}
	if err != nil {
		slog.Error("loading thread list", "error", err, "roomID", roomID, "type", req.Type)
		return nil, natsrouter.ErrInternal("failed to load thread list")
	}

	if len(threadPage.Data) == 0 {
		return &models.GetThreadsListResponse{Threads: []models.Message{}, Total: threadPage.Total}, nil
	}

	parentIDs := make([]string, len(threadPage.Data))
	for i := range threadPage.Data {
		parentIDs[i] = threadPage.Data[i].ParentMessageID
	}

	cassMessages, err := s.messages.GetMessagesByIDs(c, parentIDs)
	if err != nil {
		slog.Error("loading thread parent messages", "error", err, "roomID", roomID)
		return nil, natsrouter.ErrInternal("failed to load thread parent messages")
	}

	msgByID := make(map[string]models.Message, len(cassMessages))
	for i := range cassMessages {
		msgByID[cassMessages[i].MessageID] = cassMessages[i]
	}

	// Preserve MongoDB sort order; silently skip parents missing in Cassandra.
	threads := make([]models.Message, 0, len(threadPage.Data))
	for i := range threadPage.Data {
		msg, ok := msgByID[threadPage.Data[i].ParentMessageID]
		if !ok {
			continue
		}
		threads = append(threads, msg)
	}

	return &models.GetThreadsListResponse{Threads: threads, Total: threadPage.Total}, nil
}
```

- [ ] **Step 10: Rewrite `service/threads_test.go`**

Replace the entire file `history-service/internal/service/threads_test.go` with:

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
	"github.com/hmchangw/chat/pkg/natsrouter"
)

var threadBase = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

func makeThreadRooms() []pkgmodel.ThreadRoom {
	return []pkgmodel.ThreadRoom{
		{ID: "tr-1", RoomID: "r1", ParentMessageID: "p1", LastMsgAt: threadBase.Add(2 * time.Hour), ReplyAccounts: []string{"alice"}},
		{ID: "tr-2", RoomID: "r1", ParentMessageID: "p2", LastMsgAt: threadBase.Add(1 * time.Hour), ReplyAccounts: []string{"bob"}},
	}
}

func makeCassMessages() []models.Message {
	return []models.Message{
		{MessageID: "p1", RoomID: "r1", Msg: "parent 1", TCount: 5},
		{MessageID: "p2", RoomID: "r1", Msg: "parent 2", TCount: 3},
	}
}

func makeThreadPage(total int64) mongorepo.OffsetPage[pkgmodel.ThreadRoom] {
	return mongorepo.OffsetPage[pkgmodel.ThreadRoom]{Data: makeThreadRooms(), Total: total}
}

// --- GetThreadsList: all filter ---

func TestHistoryService_GetThreadsList_All(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(makeThreadPage(2), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(makeCassMessages(), nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Type: models.ListThreadsAll, Limit: 20})
	require.NoError(t, err)
	assert.Len(t, resp.Threads, 2)
	assert.Equal(t, int64(2), resp.Total)
	assert.Equal(t, "p1", resp.Threads[0].MessageID)
	assert.Equal(t, 5, resp.Threads[0].TCount)
}

func TestHistoryService_GetThreadsList_Total(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	// Total=42 simulates a large result set with only 2 items on this page
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(makeThreadPage(42), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(makeCassMessages(), nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, int64(42), resp.Total)
}

// --- GetThreadsList: following filter ---

func TestHistoryService_GetThreadsList_Following(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	threadRooms.EXPECT().GetFollowingThreadRooms(gomock.Any(), "r1", "u1", nil, gomock.Any()).Return(makeThreadPage(2), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(makeCassMessages(), nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Type: models.ListThreadsFollowing, Limit: 20})
	require.NoError(t, err)
	assert.Len(t, resp.Threads, 2)
}

// --- GetThreadsList: unread filter ---

func TestHistoryService_GetThreadsList_Unread(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	threadRooms.EXPECT().GetUnreadThreadRooms(gomock.Any(), "u1", "r1", nil, gomock.Any()).Return(makeThreadPage(2), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(makeCassMessages(), nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Type: models.ListThreadsUnread, Limit: 20})
	require.NoError(t, err)
	assert.Len(t, resp.Threads, 2)
	assert.Equal(t, int64(2), resp.Total)
}

// --- GetThreadsList: not subscribed ---

func TestHistoryService_GetThreadsList_NotSubscribed(t *testing.T) {
	svc, _, subs, _ := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, false, nil)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.NoError(t, err)
	assert.Empty(t, resp.Threads)
	assert.Equal(t, int64(0), resp.Total)
}

// --- GetThreadsList: empty thread list (no Cassandra call) ---

func TestHistoryService_GetThreadsList_EmptyThreads(t *testing.T) {
	svc, _, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(
		mongorepo.OffsetPage[pkgmodel.ThreadRoom]{Data: []pkgmodel.ThreadRoom{}, Total: 0}, nil,
	)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.NoError(t, err)
	assert.Empty(t, resp.Threads)
	assert.Equal(t, int64(0), resp.Total)
}

// --- GetThreadsList: error paths ---

func TestHistoryService_GetThreadsList_SubscriptionError(t *testing.T) {
	svc, _, subs, _ := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, false, fmt.Errorf("db error"))

	_, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.Error(t, err)
	assertInternalErr(t, err, "unable to verify room access")
}

func TestHistoryService_GetThreadsList_ThreadRoomError(t *testing.T) {
	svc, _, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)
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

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(makeThreadPage(2), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("cassandra down"))

	_, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.Error(t, err)
	assertInternalErr(t, err, "failed to load thread parent messages")
}

func TestHistoryService_GetThreadsList_MissingParentIgnored(t *testing.T) {
	svc, msgs, subs, threadRooms := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	threadRooms.EXPECT().GetThreadRooms(gomock.Any(), "r1", nil, gomock.Any()).Return(makeThreadPage(2), nil)
	msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(
		[]models.Message{{MessageID: "p1", RoomID: "r1", Msg: "parent 1"}}, nil,
	)

	resp, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Limit: 20})
	require.NoError(t, err)
	assert.Len(t, resp.Threads, 1)
	assert.Equal(t, "p1", resp.Threads[0].MessageID)
	// Total is from MongoDB count, not hydrated count
	assert.Equal(t, int64(2), resp.Total)
}

func TestHistoryService_GetThreadsList_InvalidType(t *testing.T) {
	svc, _, subs, _ := newService(t)
	c := testContext()

	subs.EXPECT().GetSubscriptionForThreads(gomock.Any(), "u1", "r1").Return(nil, true, nil)

	_, err := svc.GetThreadsList(c, models.GetThreadsListRequest{Type: "bogus", Limit: 20})
	require.Error(t, err)
	var routeErr *natsrouter.RouteError
	require.ErrorAs(t, err, &routeErr)
	assert.Equal(t, natsrouter.CodeBadRequest, routeErr.Code)
}
```

Note: `time` import must be added to this file's import block.

- [ ] **Step 11: Run unit tests**

```bash
make test SERVICE=history-service
```

Expected: `ok  github.com/hmchangw/chat/history-service`

- [ ] **Step 12: Run integration tests**

```bash
make test-integration SERVICE=history-service
```

Expected: all tests pass including the rewritten unread scenario.

- [ ] **Step 13: Commit everything in this task**

```bash
git add pkg/model/subscription.go \
        history-service/internal/mongorepo/subscription.go \
        history-service/internal/mongorepo/subscription_test.go \
        history-service/internal/mongorepo/threadroom.go \
        history-service/internal/mongorepo/threadroom_test.go \
        history-service/internal/service/service.go \
        history-service/internal/service/mocks/mock_repository.go \
        history-service/internal/models/threads.go \
        history-service/internal/service/threads.go \
        history-service/internal/service/threads_test.go
git commit -m "feat(history): replace threadUnread with aggregation pipeline for unread threads"
```

---

## Task 6: Remove `HasMore` — final cleanup

**Context:** All callers now use `Total` from `AggregatePaged`. `HasMore` and the n+1 helper functions (`Paginate`, `paginateOptions`) are dead code. This task removes them and rewrites `pagination_test.go` to match the slimmer struct. After this task the migration is complete.

**Files:**
- Modify: `history-service/internal/mongorepo/pagination.go`
- Modify: `history-service/internal/mongorepo/pagination_test.go`

- [ ] **Step 1: Remove `HasMore`, `Paginate`, and `paginateOptions` from `pagination.go`**

Replace the entire content of `history-service/internal/mongorepo/pagination.go` with:

```go
package mongorepo

// OffsetPageRequest holds offset+limit pagination parameters for MongoDB queries.
type OffsetPageRequest struct {
	Offset int64
	Limit  int64
}

// OffsetPage is the result of a paginated MongoDB aggregation query.
type OffsetPage[T any] struct {
	Data  []T
	Total int64
}

// EmptyPage returns a zero-result page — non-nil Data so JSON marshals to [] rather than null.
func EmptyPage[T any]() OffsetPage[T] {
	return OffsetPage[T]{Data: []T{}}
}

// NewOffsetPageRequest creates an OffsetPageRequest.
// Default limit: 20. Maximum limit: 100. Negative offset is clamped to 0.
func NewOffsetPageRequest(offset, limit int) OffsetPageRequest {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return OffsetPageRequest{Offset: int64(offset), Limit: int64(limit)}
}
```

- [ ] **Step 2: Rewrite `pagination_test.go`**

Replace the entire content of `history-service/internal/mongorepo/pagination_test.go` with:

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

func TestNewOffsetPageRequest_NegativeOffset(t *testing.T) {
	p := NewOffsetPageRequest(-5, 20)
	assert.Equal(t, int64(0), p.Offset)
}

func TestEmptyPage(t *testing.T) {
	page := EmptyPage[int]()
	assert.NotNil(t, page.Data)
	assert.Empty(t, page.Data)
	assert.Equal(t, int64(0), page.Total)
}
```

- [ ] **Step 3: Run unit tests**

```bash
make test SERVICE=history-service
```

Expected: `ok  github.com/hmchangw/chat/history-service`

- [ ] **Step 4: Run integration tests**

```bash
make test-integration SERVICE=history-service
```

Expected: all pass.

- [ ] **Step 5: Run linter**

```bash
make lint
```

Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add history-service/internal/mongorepo/pagination.go \
        history-service/internal/mongorepo/pagination_test.go
git commit -m "refactor(mongorepo): remove HasMore and Paginate — all pagination now via AggregatePaged"
```


