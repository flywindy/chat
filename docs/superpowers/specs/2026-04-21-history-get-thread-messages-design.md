# History Service — `getThreadMessages` Endpoint Design

**Date:** 2026-04-21
**Status:** Draft
**Derived from:** `2026-03-25-refactor-history-service-design.md`

## Summary

Add a NATS request/reply endpoint to `history-service` that returns the thread replies for a given thread-parent message, paginated newest-first. Ports the old Meteor `Messages.find({tmid: tmid}).sort({ts: -1})` query to the current stack: Cassandra as the store, cursor-based pagination matching the other history endpoints, and subscription-gated access via the existing `historySharedSince` helper.

The query targets the existing `thread_messages_by_room` table. The `thread_room_id` clustering column is the partition slice key, producing an efficient single-slice read with native Cassandra `PageState` pagination. `message-worker/handler.go` and `store_cassandra.go` create a real `ThreadRoom` UUID and stamp `thread_room_id` on the parent Cassandra row — the former `"N/A"` placeholder is no longer written. No Cassandra schema change; `message-worker` changes are included in the same PR.

## Scope

Covers a single new NATS request/reply endpoint registered by `history-service`: parent-message lookup, subscription access check, cursor-paginated read of `thread_messages_by_room`, and the corresponding request/response types, subject builders, repo method, unit tests, and integration tests.

Out of scope: schema changes to any Cassandra table; changes to the other three history endpoints; dropping `{roomID}` from history subjects (flagged as a future cleanup, not done here). The `message-worker` write path (creating `ThreadRoom` documents and stamping `thread_room_id` on parent rows) was brought in scope by this PR — see `message-worker/handler.go` and `store_cassandra.go`.

## NATS Subject

| Operation | Subject Pattern | Queue Group |
|-----------|-----------------|-------------|
| Get thread messages | `chat.user.{account}.request.room.{roomID}.{siteID}.msg.thread` | `history-service` |

Registered via new builders in `pkg/subject`:

```go
func MsgThreadPattern(siteID string) string {
    return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.msg.thread", siteID)
}

func MsgThreadWildcard(siteID string) string {
    return fmt.Sprintf("chat.user.*.request.room.*.%s.msg.thread", siteID)
}
```

`{roomID}` is present in the pattern for consistency with the three existing history endpoints. The handler reads it for the subscription access check — identical to the other endpoints. The parent message's own `room_id` is used as a secondary cross-check inside `findMessage`. When a future cleanup drops `{roomID}` from history subjects, this handler will require updating alongside the others.

## Request / Response Types

Added to `history-service/internal/models/message.go`:

```go
type GetThreadMessagesRequest struct {
    ThreadMessageID string `json:"threadMessageId"` // parent message ID
    Cursor          string `json:"cursor,omitempty"`
    Limit           int    `json:"limit"`
}

type GetThreadMessagesResponse struct {
    Messages   []Message `json:"messages"`
    NextCursor string    `json:"nextCursor,omitempty"`
    HasNext    bool      `json:"hasNext"`
}
```

- `ThreadMessageID` — required; the message ID of the thread parent (in old terminology, `tmid`).
- `Cursor` — empty for first page; base64-encoded Cassandra `PageState` for subsequent pages. Same encoding used by `LoadNextMessages`.
- `Limit` — default 20, clamped to max 100. Same defaults as other endpoints.
- `Messages` — thread replies only, ordered newest-first. The parent is **not** included; the client already has it.

## Handler Flow

`(s *HistoryService) GetThreadMessages(c *natsrouter.Context, req GetThreadMessagesRequest) (*GetThreadMessagesResponse, error)`:

1. `account := c.Param("account")`, `roomID := c.Param("roomID")`.
2. Reject empty `req.ThreadMessageID` with `ErrBadRequest("threadMessageId is required")`.
3. `accessSince, err := s.getAccessSince(c, account, roomID)` — subscription existence + `historySharedSince` lookup; returns `ErrForbidden` if the caller isn't subscribed. Runs before the Cassandra fetch so an unauthenticated caller cannot probe whether arbitrary message IDs exist.
4. Fetch the submitted message: `msg, err := s.messages.GetMessageByID(c, req.ThreadMessageID)`. On error: log + `ErrInternal`. On nil: `ErrNotFound("message not found")`.
5. If the fetched message is itself a reply (`msg.ThreadParentID != ""`), resolve the true parent by calling `findMessage(c, roomID, msg.ThreadParentID)`. The access check in step 6 always runs against the true parent.
6. If `accessSince != nil && parent.CreatedAt.Before(*accessSince)` → `ErrForbidden("thread is outside access window")`. Same gating semantics as `GetMessageByID` and `LoadSurroundingMessages`.
7. If `parent.ThreadRoomID == ""`, return `{messages: [], hasNext: false}` — no replies yet.
8. `pageReq, err := parsePageRequest(req.Cursor, req.Limit)` — existing helper; default 20, max 100.
9. `page, err := s.messages.GetThreadMessages(c, roomID, parent.ThreadRoomID, pageReq)`. On error: log + `ErrInternal("failed to load thread messages")`.
10. Return `&GetThreadMessagesResponse{Messages: page.Data, NextCursor: page.NextCursor, HasNext: page.HasNext}`.

**Ordering note.** The subscription check runs *before* the Cassandra message fetch, consistent with `GetMessageByID` and `LoadSurroundingMessages`. This prevents unauthenticated callers from using the endpoint as a message-existence oracle. The `roomID` used for the subscription check comes from the NATS subject parameter (not from the parent message), matching the other endpoints.

## Cassandra Repository

New method on `cassrepo.Repository`:

```go
func (r *Repository) GetThreadMessages(
    ctx context.Context, roomID, threadRoomID string, q PageRequest,
) (Page[models.Message], error)
```

Query:

```cql
SELECT <threadMessageColumns>
  FROM thread_messages_by_room
 WHERE room_id = ? AND thread_room_id = ?
 ORDER BY created_at DESC
```

Partition-key + first-clustering-key equality seek. No `ALLOW FILTERING`. `ORDER BY created_at DESC` matches the table's native clustering order. Paginated via the existing `NewQueryBuilder(q).WithCursor(q.Cursor).WithPageSize(q.PageSize).Fetch(...)` pattern so the cursor is the same opaque base64-`PageState` token the other endpoints use.

### Column selection

`thread_messages_by_room` has a different column set from `messages_by_room` (no `tshow`, `tcount`, `thread_parent_created_at`, `pinned_at`, `pinned_by`). A dedicated column list and scan-destination helper sits alongside the existing `baseColumns` / `baseScanDest`:

```go
const threadMessageColumns = "room_id, thread_room_id, created_at, message_id, thread_parent_id, " +
    "sender, target_user, msg, mentions, attachments, file, card, card_action, " +
    "quoted_parent_message, visible_to, unread, reactions, deleted, " +
    "type, sys_msg_data, site_id, edited_at, updated_at"

func threadMessageScanDest(m *models.Message) []any { /* parallel to baseScanDest */ }
```

Columns absent from `thread_messages_by_room` are simply left at the struct's zero values (`TShow=false`, `TCount=0`, `ThreadParentCreatedAt=nil`, `PinnedAt=nil`, `PinnedBy=nil`), which marshals out with `omitempty` so the wire payload stays clean.

### Repository interface extension

`service.MessageRepository` gains one method:

```go
GetThreadMessages(ctx context.Context, roomID, threadRoomID string, q cassrepo.PageRequest) (cassrepo.Page[models.Message], error)
```

`make generate SERVICE=history-service` regenerates `mocks/mock_repository.go`.

## Handler Registration

`RegisterHandlers` in `history-service/internal/service/service.go` gains one line:

```go
natsrouter.Register(r, subject.MsgThreadPattern(siteID), s.GetThreadMessages)
```

## The `thread_room_id` Writer — Status

This situation has been resolved in the same PR. `message-worker/handler.go` and `store_cassandra.go` now:

1. Create a `ThreadRoom` MongoDB document (UUID `_id`) on the first reply to a parent message.
2. Stamp `thread_room_id` on the parent Cassandra row (both `messages_by_id` and `messages_by_room`) so that `history-service` can resolve the thread without an additional MongoDB round-trip.

As a result, `parent.ThreadRoomID` is populated for any parent that has received at least one reply via the updated writer. An empty `ThreadRoomID` means no replies yet (or the reply lacked `ThreadParentMessageCreatedAt` so the stamp was skipped) — the handler returns an empty page in that case (step 7 above).

## Access Control

Reuses the two gates already applied across the service:

1. `getAccessSince(account, roomID)` — subscription existence + `historySharedSince` lookup via `SubscriptionRepository`. Non-subscribers receive `ErrForbidden("not subscribed to room")`. `roomID` is the NATS subject parameter, consistent with the other history endpoints.
2. `accessSince` window check — if `historySharedSince` is set and the **parent** was created before it, the whole thread is treated as outside the access window and returns `ErrForbidden`. Matches the old Meteor behavior of "if you can see the parent, you can see the thread."

No additional validation of `tshow` / `tcount` on the parent. A message with no replies simply produces an empty page.

## Error Matrix

| Condition | Response | Code |
|-----------|----------|------|
| `ThreadMessageID` empty | `ErrBadRequest` | `bad_request` |
| Parent not found (`GetMessageByID` returns nil) | `ErrNotFound("message not found")` | `not_found` |
| Parent lookup DB error | `ErrInternal("failed to retrieve message")` | — |
| Caller not subscribed to parent's room | `ErrForbidden("not subscribed to room")` | `forbidden` |
| Parent predates caller's `historySharedSince` | `ErrForbidden("thread is outside access window")` | `forbidden` |
| Invalid cursor | `ErrBadRequest("invalid pagination cursor")` | `bad_request` |
| Thread message query error | `ErrInternal("failed to load thread messages")` | — |
| Success (thread with no replies) | `{ messages: [], hasNext: false }` | — |

## Testing

### Unit (`history-service/internal/service/messages_test.go`)

Table-driven, mocked repos, following the existing pattern in the same file. Covers:

| Case | Setup | Expected |
|------|-------|----------|
| Happy path, first page | Parent exists, caller subscribed, no `historySharedSince`, repo returns 3 messages + empty cursor | Messages returned, `hasNext=false`, `nextCursor=""` |
| Happy path, paged | Repo returns 20 messages + non-empty cursor | `hasNext=true`, cursor passed through |
| Cursor continuation | Request with non-empty cursor | Repo called with decoded cursor |
| Empty `ThreadMessageID` | — | `ErrBadRequest` |
| Parent not found | `GetMessageByID` returns `(nil, nil)` | `ErrNotFound`, no `getAccessSince` call |
| Parent lookup repo error | `GetMessageByID` returns error | `ErrInternal`, no `getAccessSince` call |
| Not subscribed | `GetHistorySharedSince` returns `(nil, false, nil)` | `ErrForbidden("not subscribed to room")` |
| `historySharedSince` error | `GetHistorySharedSince` returns error | `ErrInternal` |
| Parent before `accessSince` | Parent `CreatedAt` < `historySharedSince` | `ErrForbidden("thread is outside access window")` |
| Parent at/after `accessSince` | Parent `CreatedAt` >= `historySharedSince` | Success |
| Invalid cursor | Malformed base64 in `Cursor` | `ErrBadRequest("invalid pagination cursor")` |
| Limit defaulting | `Limit = 0` | Repo called with `PageSize = 20` |
| Limit clamping | `Limit = 500` | Repo called with `PageSize = 100` |
| Negative limit | `Limit = -1` | Repo called with `PageSize = 20` |
| Thread query repo error | `GetThreadMessages` returns error | `ErrInternal` |
| Subject `roomID` drives the subscription check | Any test | Assert `GetHistorySharedSince` called with the subject `roomID`; `findMessage` cross-checks `msg.RoomID == roomID` |

Every test sets `parent.ThreadRoomID` to a realistic value (e.g. `"tr-test-1"`). No test uses `"N/A"`.

TDD: tests land first, fail, then the handler + repo method are written to make them pass. `make generate SERVICE=history-service` runs before tests once the interface changes.

### Integration (`history-service/internal/cassrepo/integration_test.go`)

Extends the existing testcontainer-based suite. Seed data:

- One room (`room-A`) containing two threads with different `thread_room_id`s (`tr-1` and `tr-2`), each with several replies at distinct `created_at` timestamps.
- A second room (`room-B`) containing a thread with `thread_room_id = tr-1` (same value, different partition) to prove the partition key isolates rooms.

Cases:

| Case | Assertion |
|------|-----------|
| Query `(room-A, tr-1)` | Returns only `tr-1` replies from `room-A`, `created_at DESC` order |
| Query `(room-A, tr-2)` | Returns only `tr-2` replies, no crossover |
| Query `(room-B, tr-1)` | Returns only `room-B`'s `tr-1` replies, not `room-A`'s |
| Query `(room-A, tr-unknown)` | Empty page, `hasNext=false` |
| Pagination | Limit below total → `hasNext=true` and a non-empty cursor; second call with that cursor returns the remainder, no overlap, no gaps |
| Column scan | All populated columns marshal back into `models.Message` correctly (reuse the round-trip assertion style already in the suite) |

No seed row uses `"N/A"` for `thread_room_id`.

## File Change Surface

| File | Change |
|------|--------|
| `pkg/subject/subject.go` | Add `MsgThreadPattern` + `MsgThreadWildcard` |
| `pkg/subject/subject_test.go` | Cover the two new builders |
| `history-service/internal/models/message.go` | Add `GetThreadMessagesRequest` + `GetThreadMessagesResponse` |
| `history-service/internal/service/service.go` | Extend `MessageRepository` interface; register handler |
| `history-service/internal/service/messages.go` | Add `GetThreadMessages` handler |
| `history-service/internal/service/messages_test.go` | Table-driven unit tests |
| `history-service/internal/service/mocks/mock_repository.go` | Regenerated via `make generate SERVICE=history-service` |
| `history-service/internal/cassrepo/repository.go` | Add `threadMessageColumns`, `threadMessageScanDest`, `GetThreadMessages` |
| `history-service/internal/cassrepo/integration_test.go` | Seed + cases above |

No edits to: `message-worker`, `message-gatekeeper`, `broadcast-worker`, `chat-frontend`, `room-service`, `docker-local/**`, `docs/cassandra_message_model.md`.

## Design Decisions

| Decision | Rationale |
|----------|-----------|
| Query `thread_messages_by_room` keyed on `thread_room_id` (not `thread_parent_id`) | `thread_room_id` is the first clustering column, so equality is a native slice seek with no `ALLOW FILTERING`. `thread_parent_id` is a plain column and would force a full-partition scan. The table was designed for this access shape — the writer is just lagging. |
| No new table; reuse the existing one | Adding a `thread_messages_by_parent` table was considered. Once we accepted that `thread_room_id` will eventually be populated per the DDL comment, the existing table already supports the query efficiently. No need for denormalization or a second write. |
| `message-worker` write path brought in scope | The original design deferred the writer change. During implementation it was determined that the reader was only useful with a real writer, so `message-worker/handler.go` and `store_cassandra.go` were included in the same PR to create `ThreadRoom` UUIDs and stamp `thread_room_id` on parent rows. |
| No `"N/A"` defensive check in the handler | Requested. Pre-prod, no real caller. Returning stale/wrong data when the writer hasn't been updated yet is an acceptable failure mode — the spec documents exactly when it becomes correct (the future threadRooms writer). |
| Read `{roomID}` from the subject | The NATS subject `roomID` parameter is trusted for the subscription/access check — it is authenticated by the NATS callout layer, so the client cannot forge it. Using the subject value is consistent with all other history endpoints. The parent's own `room_id` is used only as a secondary cross-check inside `findMessage`. |
| Keep `{roomID}` in the subject pattern | Consistency with the other three history endpoints. Dropping it is a cross-cutting cleanup that should touch all four together, not one in isolation. |
| Parent excluded from response | The caller necessarily already has the parent (they sourced the thread ID from it). Returning it again wastes bytes and complicates pagination boundaries. |
| Access check uses parent's `CreatedAt`, not per-reply | Matches the old Meteor semantics: visibility of the thread is gated on the parent. Per-reply filtering would make pagination return inconsistent result sizes and doesn't map to any stored access rule. |
| Subscription check runs before parent fetch | Consistent with `GetMessageByID` and `LoadSurroundingMessages`: an unauthenticated caller receives 403 before the service touches Cassandra, preventing the endpoint from being used as a message-existence oracle. |
| Cursor-based pagination | Consistent with `LoadNextMessages`; native Cassandra `PageState` is the cheapest and most correct option. The old Meteor `skip/limit` model doesn't survive the translation to Cassandra cleanly. |
| Dedicated `threadMessageColumns` / scan helper rather than reusing `baseColumns` | `thread_messages_by_room` has a strict subset of columns; reusing `baseColumns` would reference columns the table doesn't have and fail at prepare time. Following the existing `baseScanDest` / `messageByIDScanDest` pattern keeps the repo file consistent. |
| Default limit 20, max 100 | Matches `LoadHistory` and `LoadNextMessages` exactly. |
