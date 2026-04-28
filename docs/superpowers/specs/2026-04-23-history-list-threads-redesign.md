# History Service — `GetThreadsList` Redesign

**Date:** 2026-04-23
**Status:** Draft
**Supersedes (partially):** `2026-04-21-history-list-threads-design.md`

## 1. Context & Scope

The `GetThreadsList` endpoint was specified and partially implemented per `2026-04-21-history-list-threads-design.md`. Two subsequent decisions require a redesign of the data model, the unread-threads query, and the mongorepo abstraction:

1. **PM decision (2026-04-22):** the endpoint returns plain parent `Message` objects. The `ThreadParentMessage` envelope (enriched with `ThreadRoom` metadata) is dropped. Frontend does not consume the extra fields.
2. **Unread semantics review (2026-04-23):** `Subscription.threadUnread` — a user-scoped list of unread `parentMessageId` values — is redundant with `thread_subscriptions`. The latter is already maintained by `message-worker` and carries a `lastSeenAt` timestamp per thread. Comparing `threadRoom.lastMsgAt > threadSubscription.lastSeenAt` is the canonical definition of "unread thread".
3. **Efficiency requirement:** the unread query must not collection-scan `thread_subscriptions`. The relevant index on `thread_subscriptions` that the history-service relies on is `{threadRoomId, userAccount}` (owned by message-worker, also ensured at startup by history-service). A query keyed by `{userAccount, roomId}` has no supporting index. The redesign drives the query from `threadRooms` and joins into `thread_subscriptions` via `threadRoomId` — leveraging the existing index.

### In scope

- Remove `Subscription.ThreadUnread` from `pkg/model/subscription.go` and from `SubscriptionRepository.GetHistorySharedSince`.
- Replace the `ThreadParentMessage` response with plain `[]Message`; remove the envelope type.
- Replace the current `GetUnreadThreadRooms(ctx, roomID, parentIDs, ...)` signature with `GetUnreadThreadRooms(ctx, roomID, account, accessSince, ...)` implemented as a MongoDB aggregation pipeline.
- Introduce a pipeline abstraction in `mongorepo`: `Aggregate`, `AggregatePaged` (via `$facet`), and a `pipelines.go` file for pipeline builder functions.
- Convert `OffsetPage[T]` from n+1 / `HasMore` semantics to a real `Total int64` produced by `$facet`.
- Align `threadRooms.EnsureIndexes` with the new unread query's access pattern (no new indexes required — existing two `thread_rooms` compound indexes remain correct; the `parentMessageId` index listed in earlier drafts was not shipped).
- Regenerate mocks; update handler/service unit tests; keep integration tests green.

### Out of scope

- Writer-side maintenance of `threadRooms` and `thread_subscriptions` (owned by `message-worker`).
- Indexing `thread_subscriptions` by `{userAccount, roomId}` — deferred; the aggregation pattern does not require it.
- Cassandra schema — unchanged from the prior spec.
- Behaviour of the `all` and `following` filters — unchanged, same repo methods, only the response shape flattens.

## 2. Data Model Changes

### 2.1 `pkg/model/subscription.go` — remove `ThreadUnread`

The `ThreadUnread []string` field is removed. The struct returns to its pre-2026-04-21 shape:

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

Unread thread state is no longer tracked per-subscription. It is derived at query time from `threadRooms.lastMsgAt` vs `thread_subscriptions.lastSeenAt` (section 4).

`pkg/model/model_test.go::TestSubscriptionJSON` drops the `ThreadUnread` fixture field from both the input and the expected JSON.

### 2.2 `pkg/model/threadroom.go` — no field changes

`ThreadRoom` retains its current shape (no `ThreadCount`, already removed in `27a223f`). The doc-comment index list stays accurate for the new query:

- `{roomId:1, lastMsgAt:-1, threadParentCreatedAt:1}` — `all`
- `{roomId:1, replyAccounts:1, lastMsgAt:-1, threadParentCreatedAt:1}` — `following`

The `{roomId, parentMessageId, lastMsgAt, threadParentCreatedAt}` index mentioned in earlier drafts was not created — none of the shipped query methods require it.

### 2.3 `pkg/model/threadsubscription.go` — no changes

`ThreadSubscription` is owned and maintained by `message-worker`. The history-service reads it via aggregation `$lookup` and never writes it. Nil `LastSeenAt` is treated as "never seen" (always unread) — see section 4.

### 2.4 `ThreadParentMessage` — removed entirely

The `ThreadParentMessage` envelope type introduced in the 2026-04-21 spec is removed from `pkg/model`. The endpoint returns plain `[]Message`. Any test fixtures and references in `pkg/model/model_test.go` are deleted.

## 3. Endpoint Response Shape

The request shape is unchanged. The response returns plain `Message` rows.

### 3.1 Request (unchanged)

NATS subject: `chat.user.{account}.request.room.{roomID}.{siteID}.msg.thread.parent`

```go
// Shipped as GetThreadParentMessagesRequest with field Filter instead of Type.
type GetThreadParentMessagesRequest struct {
    Filter models.ThreadFilter `json:"filter"` // "", "all", "following", "unread"
    Offset int                 `json:"offset"`
    Limit  int                 `json:"limit"`
}
```

`models.ThreadFilter` constants: `ThreadFilterAll = "all"`, `ThreadFilterFollowing = "following"`, `ThreadFilterUnread = "unread"`. An empty `filter` field is treated as `"all"` by the handler.

### 3.2 Response

```go
// Shipped as GetThreadParentMessagesResponse with field ParentMessages instead of Threads.
type GetThreadParentMessagesResponse struct {
    ParentMessages []Message `json:"parentMessages"`
    Total          int64     `json:"total"`
}
```

Changes from the prior revision:

- `ParentMessages` (was `Threads`) is `[]Message` — flat list of hydrated parent message rows.
- `HasMore bool` is **replaced by** `Total int64` — the MongoDB aggregation computes the full match count via `$facet` (section 5). `HasMore` is derivable client-side as `offset + len(parentMessages) < total`.

### 3.3 Behaviour

- Not subscribed to the room → `ErrForbidden("not subscribed to room")` (403). The handler calls `s.getAccessSince(c, account, roomID)` which returns `ErrForbidden` for non-subscribers; see `TestHistoryService_GetThreadParentMessages_NotSubscribed`.
- Subscribed, zero matches → `{ParentMessages: [], Total: 0}`.
- MongoDB sort order (`lastMsgAt` desc, tie-break `threadParentCreatedAt` asc) is preserved through Cassandra bulk hydration. A parent missing in Cassandra is silently skipped (does not decrement `Total`; `Total` always reflects the MongoDB match count, not the hydrated count).
- Invalid `Filter` → NATS `BadRequest` reply.
- Internal error loading subscription / thread rooms / parent messages → sanitized `Internal` reply; full error logged server-side.

### 3.4 Sort & tie-break rationale

MongoDB sort key: `{lastMsgAt: -1, threadParentCreatedAt: 1}`. `threadParentCreatedAt` provides a deterministic tie-break when two threads have identical `lastMsgAt` (typical for freshly-created threads whose `lastMsgAt` equals the parent creation time). The tie-break field is duplicated onto `threadRooms` at creation time by `message-worker` and does not require a Cassandra lookup at read time.

## 4. Unread Threads Query Redesign

### 4.1 Definition of "unread thread"

A thread is unread for user `U` in room `R` when **both** hold:

1. `U` has a `threadSubscription` for the thread (implies `U` has participated in or was mentioned in the thread — the only writer of `thread_subscriptions` is `message-worker`, which creates them on participation).
2. `threadRoom.lastMsgAt > threadSubscription.lastSeenAt` **or** `threadSubscription.lastSeenAt` is nil (the user has a subscription but has never opened the thread).

Consequently, a user with no `threadSubscription` for a given thread is **not** notified of unread activity there. This matches the product decision: "no subscription = did not post = no notification". Threads the user passively sees in the room do not count as unread.

### 4.2 Query strategy: drive from `threadRooms`, `$lookup` into `thread_subscriptions`

Two approaches were considered:

**Approach A (rejected):** two-query join. First `thread_subscriptions.Find({userAccount: U, roomId: R})`, then `threadRooms.Find({_id: $in: [...], lastMsgAt: $gt: ...})`. Rejected because `thread_subscriptions` has no `{userAccount, roomId}` index; the first query is a full collection scan globally.

**Approach B (chosen):** single aggregation pipeline starting from `threadRooms`. Match by `roomId` (uses `{roomId, lastMsgAt, threadParentCreatedAt}` index), `$lookup` into `thread_subscriptions` keyed by `threadRoomId` (uses `{threadRoomId, userAccount}` index — leading field hit), filter to rows where the subscription exists and `lastMsgAt > lastSeenAt`. Bounded by threads-in-room × participants-per-thread, rather than total subscriptions globally.

### 4.3 Pipeline stages

```text
[
  { $match: {
      roomId: <R>,
      threadParentCreatedAt: { $gte: <accessSince> }   // only when accessSince != nil
  }},
  { $lookup: {
      from: "thread_subscriptions",
      let:  { tr: "$_id" },
      pipeline: [
        { $match: {
            $expr: { $eq: ["$threadRoomId", "$$tr"] },
            userAccount: <U>
        }},
        { $project: { lastSeenAt: 1, _id: 0 } }
      ],
      as: "sub"
  }},
  { $match: { sub: { $ne: [] } } },                     // user has a subscription
  { $match: {
      $expr: {
        $gt: ["$lastMsgAt", { $arrayElemAt: ["$sub.lastSeenAt", 0] }]
      }
  }},
  { $project: { sub: 0 } },                             // drop the joined field
  { $sort: { lastMsgAt: -1, threadParentCreatedAt: 1 } },
  { $facet: { ... } }                                   // section 5: pagination + total
]
```

### 4.4 Nil `lastSeenAt` semantics

`ThreadSubscription.LastSeenAt` is `*time.Time`. Nil means the user has a subscription but has never opened the thread — **always counts as unread**. MongoDB's BSON comparison order places `null` below all typed values ([BSON comparison order](https://www.mongodb.com/docs/manual/reference/bson-type-comparison-order/)), so in aggregation expressions `{ $gt: [date, null] }` evaluates to `true` for any non-null date. The implementation therefore uses a single `$gt` stage — no explicit `$or`/`$eq: null` branch is required. The pipeline shown above in section 4.3 has been updated to reflect this simpler form.

### 4.5 Index usage

- Initial `$match` hits `{roomId:1, lastMsgAt:-1, threadParentCreatedAt:1}` — the `all` compound index. Room-scoped, with the sort order already encoded.
- `$lookup` inner `$match` hits `{threadRoomId:1, userAccount:1}` — leading-field prefix scan, owned by `message-worker` and also ensured by history-service at startup.
- No new indexes required. `thread_subscriptions.{userAccount, roomId}` remains unindexed; the aggregation does not need it.

### 4.6 What changes in the repository

`GetUnreadThreadRooms` signature changes from:

```go
GetUnreadThreadRooms(ctx, roomID string, parentIDs []string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[ThreadRoom], error)
```

to:

```go
GetUnreadThreadRooms(ctx, roomID, account string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[ThreadRoom], error)
```

The `parentIDs` slice is gone (the subscription-derived list no longer exists). The service layer no longer passes `threadUnread` into the repo. Unread queries reference `thread_subscriptions` by collection name inside a `$lookup` stage; no direct `*Collection` dependency is added to `ThreadRoomRepo` (section 6).

## 5. Mongorepo Pipeline Abstraction

### 5.1 Goals

- Keep pipeline logic out of repo methods. Repo methods read as `pipeline := builder(...); return r.col.AggregatePaged(ctx, pipeline, req)`.
- Keep `$skip` / `$limit` / `$count` mechanics out of pipeline builders. Builders produce the filter/lookup/sort stages; the abstraction appends pagination via `$facet`.
- Single round-trip for paginated reads with accurate total count (no second count query).
- Uniform `OffsetPage[T]` shape across `all`, `following`, `unread` — all three migrate to the abstraction.

### 5.2 New file: `mongorepo/pipelines.go`

Home for pipeline-builder functions. Each takes business parameters and returns `bson.A`. Builders never append `$skip`, `$limit`, or `$count` — that is the abstraction's job.

```go
// pipelines.go

// allThreadsPipeline matches every thread room in a room, optionally
// restricted to parents created at/after accessSince.
func allThreadsPipeline(roomID string, accessSince *time.Time) bson.A {
    match := bson.M{"roomId": roomID}
    if accessSince != nil {
        match["threadParentCreatedAt"] = bson.M{"$gte": *accessSince}
    }
    return bson.A{
        bson.D{{Key: "$match", Value: match}},
        bson.D{{Key: "$sort",  Value: threadRoomSort}},
    }
}

// followingThreadsPipeline matches threads where the account is in replyAccounts.
func followingThreadsPipeline(roomID, account string, accessSince *time.Time) bson.A { ... }

// unreadThreadsPipeline: section 4.3 — drives from threadRooms, $lookup
// into thread_subscriptions, filters by lastMsgAt > sub.lastSeenAt.
func unreadThreadsPipeline(roomID, userAccount string, accessSince *time.Time) bson.A { ... }
```

`threadRoomSort` stays in `threadroom.go` (already defined there).

### 5.3 New method: `Collection[T].Aggregate`

For non-paginated aggregation results.

```go
// Aggregate runs the pipeline and decodes every result row into []T.
// Returns an empty slice (not nil) when no documents match.
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
```

No options parameter: the pipeline encodes everything. `cursor.All` is wrapped here so callers never touch a cursor.

### 5.4 New method: `Collection[T].AggregatePaged`

For paginated aggregation with total count, via `$facet`.

```go
// AggregatePaged appends a $facet stage that splits the pipeline into a
// paginated data branch and a total-count branch, then returns both as an
// OffsetPage[T].
func (c *Collection[T]) AggregatePaged(
    ctx context.Context,
    pipeline bson.A,
    req OffsetPageRequest,
) (OffsetPage[T], error) {
    facet := bson.D{{Key: "$facet", Value: bson.M{
        "data": bson.A{
            bson.D{{Key: "$skip",  Value: req.Offset}},
            bson.D{{Key: "$limit", Value: req.Limit}},
        },
        "total": bson.A{
            bson.D{{Key: "$count", Value: "count"}},
        },
    }}}
    full := append(bson.A{}, pipeline...) // copy to avoid aliasing caller's slice
    full = append(full, facet)

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

// facetResult decodes the output of the $facet stage. Kept unexported —
// only AggregatePaged constructs the pipeline that produces this shape.
type facetResult[T any] struct {
    Data  []T           `bson:"data"`
    Total []countResult `bson:"total"`
}

type countResult struct {
    Count int64 `bson:"count"`
}
```

`$facet` always emits exactly one output document (the two-branch summary). Decoding into `[]facetResult[T]` tolerates empty-collection edge cases without special-casing.

### 5.5 `OffsetPage[T]` & `OffsetPageRequest` changes

```go
// OffsetPage is the result of a paginated MongoDB query.
type OffsetPage[T any] struct {
    Data  []T
    Total int64
}
```

- `HasMore bool` **removed** — derivable from `req.Offset + int64(len(Data)) < Total`.
- `Total int64` **added** — full match count from `$facet` count branch.
- `EmptyPage[T]()` retained, now zero-Total.

`OffsetPageRequest` is unchanged. `paginateOptions()` and `Paginate()` helpers (n+1 trick) are **removed** — no longer used once all paginated reads go through `AggregatePaged`.

### 5.6 Migration of existing repo methods

`GetThreadRooms` and `GetFollowingThreadRooms` move to `AggregatePaged` using the simple builders from 5.2. This is mechanical: the current `bson.M` filter plus sort options become a two-stage pipeline. `GetUnreadThreadRooms` uses the aggregation builder from section 4.3.

After migration, each method body collapses to:

```go
func (r *ThreadRoomRepo) GetThreadRooms(ctx context.Context, roomID string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[model.ThreadRoom], error) {
    return r.threadRooms.AggregatePaged(ctx, allThreadsPipeline(roomID, accessSince), req)
}
```

No more `filter := bson.M{...}`, no more `opts := append(...)`. The pipeline-builder owns the query shape; the method owns the collection wiring.

### 5.7 What is out of scope for the abstraction

- `WithProjection` integration with aggregation. Projection in aggregation is a `$project` pipeline stage, which belongs in the builder. `QueryOption` stays for `Find` operations only.
- Cursor-style / keyset pagination — offset is sufficient for the frontend's current needs.
- Generic `Aggregate` + external `Paginate` helper — `AggregatePaged` owns the `$facet` logic end-to-end; exposing it piecemeal would re-introduce the n+1 pattern we are removing.

## 6. Repository Interface & Index Updates

### 6.1 `SubscriptionRepository.GetHistorySharedSince`

```go
// Before (GetSubscriptionForThreads — name and signature)
GetSubscriptionForThreads(ctx context.Context, account, roomID string) (*time.Time, []string, bool, error)

// Shipped (renamed + return slice dropped)
GetHistorySharedSince(ctx context.Context, account, roomID string) (*time.Time, bool, error)
```

- Returned `[]string` slice (`Subscription.ThreadUnread`) removed.
- Method renamed from `GetSubscriptionForThreads` to `GetHistorySharedSince` to reflect its actual purpose.
- Projection in the mongo implementation drops `threadUnread: 1`, keeps `historySharedSince: 1`.
- Service layer call site drops the `threadUnread` variable.

### 6.2 `ThreadRoomRepository.GetUnreadThreadRooms`

```go
// Before
GetUnreadThreadRooms(ctx, roomID string, parentIDs []string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[ThreadRoom], error)

// After
GetUnreadThreadRooms(ctx, roomID, account string, accessSince *time.Time, req OffsetPageRequest) (OffsetPage[ThreadRoom], error)
```

- `parentIDs []string` removed; replaced with `account string` (caller's NATS-authenticated identity).
- Implementation body described in sections 4.3 and 5.4.

### 6.3 `ThreadRoomRepo` struct — collection dependency

```go
type ThreadRoomRepo struct {
    threadRooms *Collection[model.ThreadRoom]
}

func NewThreadRoomRepo(db *mongo.Database) *ThreadRoomRepo {
    return &ThreadRoomRepo{
        threadRooms: NewCollection[model.ThreadRoom](db.Collection("thread_rooms")),
    }
}
```

Note: the unread aggregation references `thread_subscriptions` by collection name inside the `$lookup` stage — no direct `*Collection` field is needed on the struct. The collection name string embedded in the pipeline is sufficient.

### 6.4 Service-layer wiring

`GetThreadParentMessages` (`threads.go`):

- `accessSince, threadUnread, subscribed, err := s.subscriptions.GetSubscriptionForThreads(...)` was replaced with `accessSince, subscribed, err := s.subscriptions.GetHistorySharedSince(...)`.
- The `ThreadFilterUnread` switch case calls `s.threadRooms.GetUnreadThreadRooms(c, roomID, account, accessSince, pageReq)`.
- `threadPage.HasMore` was replaced with `threadPage.Total`, propagated to `GetThreadParentMessagesResponse.Total`.
- `len(threadUnread) == 0` early-return was removed — the aggregation handles "no subscriptions" naturally (returns zero rows + `Total: 0`).

### 6.5 Mocks

Regenerated via `make generate SERVICE=history-service`. Both mock interfaces (`SubscriptionRepository`, `ThreadRoomRepository`) pick up the signature changes. `history-service/internal/service/threads_test.go` was updated to drop the `threadUnread` slice from every `GetHistorySharedSince` return, and to pass `account` instead of `parentIDs` in every `GetUnreadThreadRooms` expectation.

### 6.6 Index strategy (unchanged from prior spec)

`ThreadRoomRepo.EnsureIndexes` creates two `thread_rooms` compound indexes:

- `{roomId:1, lastMsgAt:-1, threadParentCreatedAt:1}` — `all` + `unread` (outer `$match`)
- `{roomId:1, replyAccounts:1, lastMsgAt:-1, threadParentCreatedAt:1}` — `following`

`thread_subscriptions` indexes:

- `{threadRoomId:1, userAccount:1}` unique — **leading field drives the `$lookup` inner `$match`**; created by message-worker and also ensured at startup by history-service

`thread_subscriptions.{userAccount, roomId}` is intentionally **not** added. The aggregation does not need it; adding it would impose maintenance cost for no query benefit.

### 6.7 Test updates

- `pkg/model/model_test.go::TestSubscriptionJSON` — drop `ThreadUnread` fixture.
- `pkg/model/model_test.go` — remove any `ThreadParentMessage` references (already partially done in `b5d684d`).
- `history-service/internal/mongorepo/threadroom_test.go` — existing `FindMany`-based expectations rewritten to match aggregation-based queries. Integration test coverage should include: zero subscriptions, non-nil `lastSeenAt` in the past (unread), non-nil `lastSeenAt` in the future (read), nil `lastSeenAt` (always unread), mixed population across multiple threads.
- `history-service/internal/mongorepo/subscription_test.go` — integration test drops `ThreadUnread` expectations.
- `history-service/internal/service/threads_test.go` — all scenarios updated per 6.4 and 6.5.

---

---

## 7. QuotedParentMessage Schema Extension (post-ship)

### 7.1 Motivation

In the original implementation, `redactUnavailableQuotes` re-fetched thread parents from Cassandra at read time
to check whether the parent message predates the caller's `historySharedSince` window. This
was necessary because the `QuotedParentMessage` snapshot stored on a TShow reply could
have a stale `CreatedAt`. The Cassandra fetch was batched and deduplicated, but still
added latency and complexity to every history read.

### 7.2 Schema change — `QuotedParentMessage` UDT

Two fields added (set by message-worker at write time, never by history-service):

| Field | CQL type | JSON key | Purpose |
|-------|----------|----------|---------|
| `thread_parent_id` | `TEXT` | `threadParentId` | ID of the thread parent for this TShow reply |
| `thread_parent_created_at` | `TIMESTAMP` | `threadParentCreatedAt` | Actual `created_at` of that parent row |

Files touched: `pkg/model/cassandra/message.go`, `docker-local/cassandra/init/06-udt-quoted_parent_message.cql`, `docs/cassandra_message_model.md`.

### 7.3 Impact on `redactUnavailableQuotes`

The function is simplified to a single-pass loop with no Cassandra call:

```go
func redactUnavailableQuotes(msgs []models.Message, accessSince *time.Time) {
    if accessSince == nil { return }
    for i := range msgs {
        q := msgs[i].QuotedParentMessage
        if q == nil { continue }
        tshowParentInaccessible := msgs[i].TShow &&
            q.ThreadParentID != "" &&
            q.ThreadParentCreatedAt != nil &&
            q.ThreadParentCreatedAt.Before(*accessSince)
        if q.CreatedAt.Before(*accessSince) || tshowParentInaccessible {
            msgs[i].QuotedParentMessage = &models.QuotedParentMessage{Msg: UnavailableQuoteMsg}
        }
    }
}
```

The function is now package-level (no `*HistoryService` receiver, no `context.Context`).

### 7.4 Responsibility split

- **message-worker** populates `QuotedParentMessage.ThreadParentID` and `ThreadParentCreatedAt` when writing TShow replies. history-service does not write these fields.
- **history-service** reads them at query time for access-window enforcement.

**End of spec.**


