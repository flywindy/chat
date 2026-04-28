# History Service — `GetThreadsList` Endpoint Design

**Date:** 2026-04-21
**Status:** Draft

## Summary

Add a NATS request/reply endpoint to `history-service` that returns the list of thread-parent messages for a given room, with three filter modes (`all`, `following`, `unread`), sorted by most-recent thread activity and paginated with offset+limit.

This ports the old Meteor `Messages.find({threadCount: {$exists: true}}, {sort: {ThreadLastMessage: -1}})` query to the current polyglot stack. Because the sort key (`threadLastMessage`) and filter data (`replyAccounts`, `threadUnread`) live in MongoDB, **MongoDB `thread_rooms` drives the query**; Cassandra provides bulk hydration of the parent message bodies. The two stores are joined in the service layer.

This PR also carries the required upstream schema changes: removing `tcount` from Cassandra (it moves permanently to `thread_rooms`), updating `pkg/model/threadroom.go` to the full agreed shape, and adding `ThreadUnread` to `pkg/model/subscription.go`.

## Scope

Covers: new NATS endpoint registered by `history-service`; updated `ThreadRoom` and `Subscription` pkg models; `tcount` removal from Cassandra schema, Go structs, integration test seeds, and docker-local DDL; new `mongorepo/pagination.go` and `mongorepo/threadroom.go`; new Cassandra `GetMessagesByIDs` bulk method; new subject builders in `pkg/subject`; full unit and integration test coverage; post-implementation architecture reviews of `mongorepo` and `cassrepo`; Makefile audit.

Out of scope: the writer-side services that populate and maintain `thread_rooms` (separate repo / separate PR); MongoDB index DDL at the ops/deployment level (noted in the model file); `ALTER TABLE DROP COLUMN tcount` for existing Cassandra clusters (ops step, after writer stops writing the column).

## Data Model Changes

### 1. `pkg/model/threadroom.go` — full shape + index notes

Replace the existing stub with the full agreed schema. Index guidance lives as a doc comment adjacent to the struct so it travels with the model into the shared package.

```go
// ThreadRoom is the MongoDB document that stores thread metadata for a single
// thread inside a room. It is the source of truth for mutable thread state
// (reply count, last activity, participant list) that has been decoupled from
// the Cassandra message row to avoid high-frequency Cassandra tombstones.
//
// MongoDB collection: thread_rooms
//
// Recommended indexes:
//
//   // all-threads query (type=all): sort by last activity within a room
//   { roomId: 1, threadLastMessage: -1 }
//
//   // following query (type=following): filter by participant + sort
//   { roomId: 1, replyAccounts: 1, threadLastMessage: -1 }  // multikey on replyAccounts array
//
//   // unread query (type=unread): filter by parent message IDs from subscription.threadUnread
//   { roomId: 1, threadParentId: 1, threadLastMessage: -1 }
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

Fields removed from the old stub: `LastMsgAt`, `LastMsgID` — replaced by `ThreadLastMessage`.
Fields added vs old stub: `ThreadParentCreatedAt`, `TShow`, `ThreadCount`, `ThreadLastMessage`, `ReplyAccounts`.

`ThreadParentCreatedAt` and `TShow` are write-once copies of the same fields on the Cassandra parent message. They are duplicated here so that consumers needing only thread metadata never need to cross into Cassandra.

### 2. `pkg/model/subscription.go` — add `ThreadUnread`

```go
type Subscription struct {
    // ... existing fields unchanged ...
    ThreadUnread []string `json:"threadUnread,omitempty" bson:"threadUnread,omitempty"`
}
```

`ThreadUnread` is a list of `threadParentId` values (parent message IDs) representing threads the user has unread activity in, within this room. Maintained by the writer service (out of scope). Empty slice and nil are equivalent.

### 3. `pkg/model/cassandra/message.go` — remove `TCount`

Remove the `TCount int` field. This data now lives exclusively in `thread_rooms.threadCount`.

```diff
- TCount int `cql:"tcount"`
```

`TShow` and `ThreadParentCreatedAt` remain on the Cassandra row — they are read-only after message creation and are needed by every message-fetching endpoint without a MongoDB round-trip.

### 4. `docs/cassandra_message_model.md` — schema updates

In `messages_by_room`:
- Remove `tcount INT` column
- Update `thread_room_id` comment: remove the "todo in future" note; document as: `// links to thread_rooms._id in MongoDB; "N/A" for non-thread messages`

In `messages_by_id`:
- Same two changes as above

`thread_messages_by_room` and `pinned_messages_by_room`: no change.

### 5. `docker-local/cassandra/init/` DDL files

Apply matching changes to `10-table-messages_by_room.cql` and `13-table-messages_by_id.cql`: remove `tcount INT`, update the `thread_room_id` comment.

### 6. `history-service/internal/cassrepo/` — tcount ripple

- `repository.go`: remove `tcount` from `baseColumns` constant and `baseScanDest` scan destinations.
- `integration_test.go`: remove `tcount` from any `INSERT` statements in seed helpers and full-row round-trip tests (the existing `TestRepository_FullRow_AllColumns` inserts `tcount` — update or remove that column from the insert and the assertion).

### 7. Makefile audit

After all changes, verify `make lint`, `make test`, and `make build SERVICE=history-service` all pass. Check whether any Makefile target references `tcount` or the old subject name.

## NATS Subject

| Operation | Pattern | Queue Group |
|-----------|---------|-------------|
| List thread parents | `chat.user.{account}.request.room.{roomID}.{siteID}.msg.thread.parent` | `history-service` |

New builders in `pkg/subject`:

```go
func MsgThreadParentPattern(siteID string) string {
    return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.msg.thread.parent", siteID)
}

func MsgThreadParentWildcard(siteID string) string {
    return fmt.Sprintf("chat.user.*.request.room.*.%s.msg.thread.parent", siteID)
}
```

`{roomID}` is read by this handler — it is the primary room filter, unlike `GetThreadMessages` which ignores it.

## Request / Response Types

Added to `history-service/internal/models/threads.go` (new file):

```go
// ListThreadsType controls which subset of threads is returned.
type ListThreadsType string

const (
    ListThreadsAll       ListThreadsType = ""          // default: all threads in room
    ListThreadsFollowing ListThreadsType = "following" // threads user has replied in
    ListThreadsUnread    ListThreadsType = "unread"    // threads with unread activity
)

// GetThreadsListRequest is the payload for listing threads in a room.
type GetThreadsListRequest struct {
    Type   ListThreadsType `json:"type"`
    Offset int             `json:"offset"` // default 0
    Limit  int             `json:"limit"`  // default 20, max 100
}

// ThreadParentMessage is the response item: the parent message from Cassandra
// enriched with live thread metadata from MongoDB thread_rooms.
type ThreadParentMessage struct {
    Message                                  // embedded Cassandra message row
    ThreadCount       int       `json:"threadCount"`
    ThreadLastMessage time.Time `json:"threadLastMessage"`
    ReplyAccounts     []string  `json:"replyAccounts"`
}

// GetThreadsListResponse is the response for GetThreadsList.
type GetThreadsListResponse struct {
    Threads []ThreadParentMessage `json:"threads"`
    HasMore bool                  `json:"hasMore"`
}
```

`ThreadParentMessage` is defined here in `internal/models` (not in `pkg`) because it is a service-specific response shape that combines data from two stores; it has no use outside this service.

## Handler Flow

`(s *HistoryService) GetThreadsList(c *natsrouter.Context, req GetThreadsListRequest) (*GetThreadsListResponse, error)`:

1. `account := c.Param("account")`, `roomID := c.Param("roomID")`.

2. Fetch subscription: `accessSince, threadUnread, subscribed, err := s.subscriptions.GetSubscriptionForThreads(c, account, roomID)`.
   - Implementation uses projection `{historySharedSince: 1, threadUnread: 1, _id: 0}` — only these two fields are decoded, no full document fetch.
   - On error: log full error internally + return `ErrInternal("unable to verify room access")`.
   - If not subscribed: return `ErrForbidden("not subscribed to room")`.

3. Build `pageReq` via `mongorepo.ParsePageRequest(req.Offset, req.Limit)` (defaults/clamping: default 20, max 100, negative offset treated as 0).

4. **Query thread_rooms from MongoDB** based on type:
   - `ListThreadsAll` / `""`: `s.threadRooms.GetThreadRooms(c, roomID, accessSince, pageReq)`
   - `ListThreadsFollowing`: `s.threadRooms.GetFollowingThreadRooms(c, roomID, account, accessSince, pageReq)`
   - `ListThreadsUnread`: if `len(threadUnread) == 0`, return empty response immediately. Otherwise: `s.threadRooms.GetUnreadThreadRooms(c, roomID, threadUnread, accessSince, pageReq)`
   - Unknown type: return `ErrBadRequest("invalid type")`.
   - On repo error: log full error internally + return `ErrInternal("failed to list threads")`.

5. If `page.Data` is empty, return `&GetThreadsListResponse{Threads: []ThreadParentMessage{}, HasMore: false}`.

6. **Bulk-fetch parent messages from Cassandra**: extract `ThreadParentID` from each `ThreadRoom`, call `s.messages.GetMessagesByIDs(c, parentIDs)` → `map[string]*models.Message`.
   - On error: log full error internally + return `ErrInternal("failed to load thread messages")`.

7. **Combine** into `[]ThreadParentMessage`: for each `ThreadRoom`, look up the parent message in the map. If the message is missing (data inconsistency): log error + return `ErrInternal("data inconsistency: thread parent message missing")` — fail the whole page, do not silently skip.

8. Return `&GetThreadsListResponse{Threads: combined, HasMore: page.HasMore}`.

**`accessSince` application:** pushed into the MongoDB query as `threadParentCreatedAt >= accessSince`. This matches the semantics of other endpoints (gate by when the parent was created, not by last activity) and avoids a post-hydration filter loop.

## Repositories

### `mongorepo/pagination.go` — new file

Offset+limit pagination helper for MongoDB, analogous to `cassrepo/utils.go` PageState approach.

```go
package mongorepo

const (
    defaultPageSize = 20
    maxPageSize     = 100
)

// PageRequest holds offset+limit pagination parameters for MongoDB queries.
type PageRequest struct {
    Offset int64
    Limit  int64
}

// Page is a generic paginated result from MongoDB.
type Page[T any] struct {
    Data    []T
    HasMore bool
}

// ParsePageRequest builds a PageRequest with defaults and clamping applied.
// Negative offset is treated as 0. Limit <= 0 defaults to 20, max 100.
func ParsePageRequest(offset, limit int) PageRequest {
    if offset < 0 {
        offset = 0
    }
    if limit <= 0 {
        limit = defaultPageSize
    }
    if limit > maxPageSize {
        limit = maxPageSize
    }
    return PageRequest{Offset: int64(offset), Limit: int64(limit)}
}

// queryOptions returns WithSkip + WithLimit(limit+1) so the caller can detect HasMore.
func (p PageRequest) queryOptions() []QueryOption {
    return []QueryOption{
        WithSkip(p.Offset),
        WithLimit(p.Limit + 1), // fetch one extra to detect HasMore
    }
}

// Paginate trims the +1 sentinel from results and sets HasMore.
func Paginate[T any](data []T, limit int64) Page[T] {
    if int64(len(data)) > limit {
        return Page[T]{Data: data[:limit], HasMore: true}
    }
    return Page[T]{Data: data, HasMore: false}
}
```

### `mongorepo/threadroom.go` — new file

```go
type ThreadRoomRepo struct {
    threadRooms *Collection[model.ThreadRoom]
}

func NewThreadRoomRepo(db *mongo.Database) *ThreadRoomRepo

// GetThreadRooms returns all threads in a room sorted by last activity.
// accessSince filters by threadParentCreatedAt >= accessSince when non-nil.
func (r *ThreadRoomRepo) GetThreadRooms(ctx context.Context, roomID string, accessSince *time.Time, req PageRequest) (Page[model.ThreadRoom], error)

// GetFollowingThreadRooms returns threads where account appears in replyAccounts.
func (r *ThreadRoomRepo) GetFollowingThreadRooms(ctx context.Context, roomID, account string, accessSince *time.Time, req PageRequest) (Page[model.ThreadRoom], error)

// GetUnreadThreadRooms returns threads whose threadParentId is in parentIDs.
func (r *ThreadRoomRepo) GetUnreadThreadRooms(ctx context.Context, roomID string, parentIDs []string, accessSince *time.Time, req PageRequest) (Page[model.ThreadRoom], error)
```

All three share the same sort (`{threadLastMessage: -1, _id: 1}` — stable tiebreaker on `_id`), the same `accessSince` filter logic, and use `ParsePageRequest` + `Paginate` from `pagination.go`.

The `Collection.FindMany` helper already supports `WithSort`, `WithLimit`, `WithSkip` via `options.go` — no changes needed to `collection.go`.

### `SubscriptionRepository` — new method

Add to the interface in `service/service.go` and implement in `mongorepo/subscription.go`:

```go
// GetSubscriptionForThreads returns the historySharedSince lower bound and the
// threadUnread parent-ID list for a user's room subscription.
// Uses projection {historySharedSince: 1, threadUnread: 1, _id: 0} — no full document decode.
// Returns (nil, nil, false, nil) when not subscribed.
GetSubscriptionForThreads(ctx context.Context, account, roomID string) (accessSince *time.Time, threadUnread []string, subscribed bool, err error)
```

### `ThreadRoomRepository` interface — new, in `service/service.go`

```go
type ThreadRoomRepository interface {
    GetThreadRooms(ctx context.Context, roomID string, accessSince *time.Time, req mongorepo.PageRequest) (mongorepo.Page[model.ThreadRoom], error)
    GetFollowingThreadRooms(ctx context.Context, roomID, account string, accessSince *time.Time, req mongorepo.PageRequest) (mongorepo.Page[model.ThreadRoom], error)
    GetUnreadThreadRooms(ctx context.Context, roomID string, parentIDs []string, accessSince *time.Time, req mongorepo.PageRequest) (mongorepo.Page[model.ThreadRoom], error)
}
```

### `cassrepo/repository.go` — new `GetMessagesByIDs`

```go
// GetMessagesByIDs fetches multiple messages by ID from messages_by_id in parallel.
// Missing IDs are silently absent from the returned map (caller decides how to handle gaps).
// Returns a map of messageID → *Message.
func (r *Repository) GetMessagesByIDs(ctx context.Context, ids []string) (map[string]*models.Message, error)
```

Implementation fires one `messages_by_id` query per ID concurrently using `errgroup`, with a semaphore capped at `min(len(ids), 20)` to bound parallelism. Returns a map for O(1) join in the service layer.

### `HistoryService` wiring

- Add `threadRooms ThreadRoomRepository` field to `HistoryService`.
- Update `New(msgs, subs, threadRooms)` constructor.
- Register in `RegisterHandlers`: `natsrouter.Register(r, subject.MsgThreadParentPattern(siteID), s.GetThreadsList)`.
- Update `cmd/main.go` to construct and inject `mongorepo.NewThreadRoomRepo(db)`.

## Testing

### Unit tests (`internal/service/threads_test.go`)

Table-driven tests following the established `Test<Type>_<Method>_<Scenario>` naming pattern. Mock all three repository interfaces. Cover:

- `type=all`, happy path with HasMore true and false
- `type=following`, happy path
- `type=unread` with non-empty threadUnread
- `type=unread` with empty threadUnread → immediate empty response, no repo calls
- Unknown `type` → `ErrBadRequest`
- Not subscribed → `ErrForbidden`
- Subscription store error → `ErrInternal`
- `accessSince` passed through to repo correctly
- Thread rooms returned but Cassandra returns no message for one ID → `ErrInternal` (inconsistency)
- ThreadRoomRepo error → `ErrInternal`
- Cassandra bulk fetch error → `ErrInternal`
- Offset and limit defaults applied (0 offset, 20 limit when zero)
- Limit clamped to 100
- Response assembles `ThreadParentMessage` correctly (fields from both stores present)

### Integration tests (`mongorepo/threadroom_integration_test.go`)

`//go:build integration`, testcontainers MongoDB. Cover:

- Seed thread_rooms, verify pagination HasMore detection (fetch limit+1 sentinel)
- `GetFollowingThreadRooms` filters by `replyAccounts` correctly, excludes non-participant
- `GetUnreadThreadRooms` filters by `parentIDs` correctly
- `accessSince` correctly excludes thread parents older than the cutoff
- Sort order: `threadLastMessage DESC` verified across multiple documents

### Cassandra integration (`cassrepo/integration_test.go` — extend existing)

- `GetMessagesByIDs` returns map with all found messages
- `GetMessagesByIDs` with some IDs not present — missing keys absent from map, no error
- `GetMessagesByIDs` with empty input — returns empty map, no error
- Verify existing `TestRepository_FullRow_AllColumns` still passes after `tcount` removal from INSERT and scan

### `mongorepo/pagination_test.go` — new unit tests

- `ParsePageRequest`: defaults, clamping, negative offset
- `Paginate`: exact limit returns HasMore=false; limit+1 returns HasMore=true with trimmed slice

## Post-Implementation Reviews

### `mongorepo` architecture review

After all `mongorepo` files are written, review the package as a whole for:
- Consistent use of `Collection[T]` wrapper vs raw `*mongo.Collection`
- `QueryOption` usage — ensure `pagination.go` integrates cleanly with `options.go`
- No exported internals; `queryOptions()` on `PageRequest` stays unexported
- Error wrapping follows project convention (`fmt.Errorf("short description: %w", err)`)

### `cassrepo` package review

After `GetMessagesByIDs` is added, review `cassrepo` as a whole for:
- Goroutine lifecycle — `GetMessagesByIDs` fan-out must have a clear termination path; no leaks
- Consistent column constant usage — no ad-hoc column strings outside the declared constants
- `baseScanDest` correctness after `tcount` removal — column count must match scan destination count

## Out of Scope

- Writer-side: the service that creates/updates `thread_rooms` documents and maintains `replyAccounts`, `threadCount`, `threadLastMessage`, `threadUnread` on subscriptions.
- MongoDB index creation DDL — noted in the model struct comment; applied at deployment time.
- `ALTER TABLE DROP COLUMN tcount` on existing Cassandra clusters — ops step; safe to defer until after the writer stops writing the column.
- Removing `{roomID}` from history subject patterns — flagged as a future cleanup, not done here.
- Cursor-based pagination for this endpoint — offset+limit suits UI page navigation; cursor pagination can be added later if needed.

## Implementation Deltas

The shipped implementation diverges from this design doc in several places. The final design is documented in `docs/superpowers/specs/2026-04-23-history-list-threads-redesign.md`. Key divergences:

| Area | This doc | Shipped |
|------|----------|---------|
| Handler name | `GetThreadsList` / `GetThreadsListResponse` | `GetThreadParentMessages` / `GetThreadParentMessagesResponse` |
| Pagination types | `PageRequest` / `Page[T]` / `queryOptions()` helper | `OffsetPageRequest` / `OffsetPage[T]` / `AggregatePaged` flow |
| `GetUnreadThreadRooms` signature | `(ctx, roomID, parentIDs []string, ...)` — caller passes subscription-derived parent IDs | `(ctx, roomID, account, ...)` — repo does a `$lookup` against `threadSubscriptions` collection directly |
| Not-subscribed response | `ErrForbidden("not subscribed to room")` | Same — `getAccessSince` helper returns `ErrForbidden`; aligned in final implementation |
| Missing parent on hydrate | Fail the whole page with `ErrInternal` | Silently skip missing / wrong-room parents (defense-in-depth against data drift) |
| `tcount` / `TCount` removal | Specified for removal from Cassandra | `tcount` remains in `cassrepo/messages_by_room.go` `baseColumns` and scan destinations — Cassandra schema change deferred |
| `Subscription.ThreadUnread` | Used to pass parent IDs to `GetUnreadThreadRooms` | Superseded by `threadSubscriptions` collection join inside `unreadThreadsPipeline` |
| `GetMessagesByIDs` return type | `map[string]*models.Message` | `[]models.Message` — caller builds the map in the service layer |
