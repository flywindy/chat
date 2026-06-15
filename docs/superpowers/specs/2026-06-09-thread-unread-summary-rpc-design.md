# Thread Unread Summary RPC (room-service)

**Date:** 2026-06-09
**Status:** Approved — ready for implementation plan

## Problem

Other sites need to query, per user, a rollup of that site's thread unread
state so a caller can fan out across sites and OR the results into a global
badge. There is no per-user/per-site thread-unread rollup today; the facts live
only at the individual `thread_subscriptions` level.

## RPC contract

Server-to-server (site-internal) NATS request/reply, served by room-service,
one subscription per site.

**Subject** (`pkg/subject/subject.go`, following the `RoomsInfoBatch`
precedent):

```go
func ThreadUnreadSummary(siteID string) string {
    return fmt.Sprintf("chat.server.request.room.%s.thread.unread.summary", siteID)
}
func ThreadUnreadSummarySubscribe(siteID string) string {
    return fmt.Sprintf("chat.server.request.room.%s.thread.unread.summary", siteID)
}
```

**Request / response** (`pkg/model`):

```go
type ThreadUnreadSummaryRequest struct {
    UserAccount string `json:"userAccount"`
}

type ThreadUnreadSummaryResponse struct {
    Unread              bool   `json:"unread"`
    UnreadDirectMessage bool   `json:"unreadDirectMessage"`
    UnreadMention       bool   `json:"unreadMention"`
    LastMessageAt       *int64 `json:"lastMessageAt,omitempty"` // UnixMilli; nil when the user has no threads
}
```

The query filters on `userAccount` (not `userId`): every existing
`thread_subscriptions` query and index keys off `userAccount`, and the unique
index is `(threadRoomId, userAccount)`. Callers that only hold a `userId`
resolve it to an account on their side.

## Field semantics

All four are scoped to threads homed on the serving site
(`thread_subscriptions.siteId == h.siteID`), so a caller fans out to every site
and combines (OR the booleans, max the timestamp).

| Field | Rule |
|-------|------|
| `unread` | ∃ thread sub where `threadRoom.lastMsgAt > sub.lastSeenAt`. A nil `lastSeenAt` (never seen) counts as unread. |
| `unreadDirectMessage` | Same as `unread` **and** the parent room `type == "dm"`. |
| `unreadMention` | ∃ thread sub where `hasMention == true`. |
| `lastMessageAt` | `max(threadRoom.lastMsgAt)` over all the user's thread rooms at this site (regardless of read state). nil when the user has no threads. |

## Performance decision

Investigation confirmed there is **no existing rollup** to exploit:

- `subscription.ThreadUnread[]` is only *cleared* on "mark thread read", never
  appended to when a reply arrives — not a reliable "has unread" signal.
- `subscription.HasMention` is room-level, not thread-level. Thread mentions
  live only on `thread_subscriptions.hasMention`.
- `thread_rooms` does not store the parent room type.
- `thread_subscriptions` has no TTL/pruning — they grow unbounded per user.

So the summary is genuinely computed by reading the user's thread subs. The
chosen approach (**Option 1: optimized aggregation, surgical variant**) keeps
the change contained to room-service + `pkg`:

- A single MongoDB aggregation, computed server-side, returning one small doc.
- Mention folds into the same pass (`$max hasMention` — no extra query, no
  join), so there is exactly one aggregation per call.
- DM detection uses a `rooms` join (no write-path denormalization, no backfill).

Cost is `O(N)` in the user's thread-sub count at the site, with two indexed
point-lookups per row (`thread_rooms._id`, `rooms._id`). Acceptable for the
expected scale. The **wire contract is independent of the computation**, so if
profiling later shows the join hurting, we can graduate to denormalizing
`lastMsgAt`/`roomType` onto `thread_subscriptions` (write-time) or a per-user
summary counter without changing callers.

## Aggregation pipeline

On `thread_subscriptions`:

1. `$match {userAccount: account, siteId: siteID}` — served by the new
   `(userAccount, siteId)` index.
2. `$lookup thread_rooms` on `threadRoomId → _id` as `tr`; `$unwind tr`
   (no `preserveNullAndEmptyArrays` — drops orphaned subs whose thread room was
   deleted).
3. `$lookup rooms` on `roomId → _id` as `room`; `$unwind room` with
   `preserveNullAndEmptyArrays: true` (a missing parent room leaves the row in
   for unread/mention; the DM check just resolves false).
4. `$addFields`:
   - `isUnread = {$gt: ["$tr.lastMsgAt", "$lastSeenAt"]}` — relies on BSON null
     being the smallest value, so a never-seen (`null`) `lastSeenAt` is always
     unread. This is why `ThreadSubscription.LastSeenAt` deliberately has no
     `omitempty`.
   - `isDMUnread = {$and: [isUnread, {$eq: ["$room.type", "dm"]}]}`.
5. `$group _id:null`:
   - `unread = {$max: "$isUnread"}`
   - `unreadDirectMessage = {$max: "$isDMUnread"}`
   - `unreadMention = {$max: "$hasMention"}`
   - `lastMessageAt = {$max: "$tr.lastMsgAt"}`

`$max` over booleans yields `true` if any row is true (BSON `true > false`). The
`$addFields` is structured so every row produces concrete `true`/`false`, never
missing. An empty cursor (user has no thread subs) → zero-value summary.

## Components

1. **`pkg/subject/subject.go`** — `ThreadUnreadSummary` / `ThreadUnreadSummarySubscribe`.
2. **`pkg/model`** — `ThreadUnreadSummaryRequest`, `ThreadUnreadSummaryResponse`.
3. **room-service `handler.go`** — `threadUnreadSummary(c, req)` handler:
   validate `UserAccount` non-empty (`errcode.BadRequest` otherwise), 5s context
   timeout (matching `roomsInfoBatch`), call the store, map the store struct to
   the response (`*time.Time` → `*int64` UnixMilli), debug-log
   `site_id`/`account`/`latency_ms`. Returns a typed `*errcode.Error`;
   `natsrouter` marshals the wire envelope. Registered via
   `natsrouter.Register(r, subject.ThreadUnreadSummarySubscribe(h.siteID), h.threadUnreadSummary)`.
4. **room-service `store.go`** — add to `RoomStore`:
   `GetThreadUnreadSummary(ctx, account, siteID string) (*ThreadUnreadSummary, error)`,
   with a local result struct (like `RoomCounts`):
   ```go
   type ThreadUnreadSummary struct {
       Unread              bool
       UnreadDirectMessage bool
       UnreadMention       bool
       LastMessageAt       *time.Time
   }
   ```
   Regenerate `mock_store_test.go` (`make generate SERVICE=room-service`).
5. **room-service `store_mongo.go`** — add a `threadRooms *mongo.Collection`
   handle (`db.Collection("thread_rooms")`) to `MongoStore` + constructor;
   implement `GetThreadUnreadSummary` (the pipeline above); add index
   `{userAccount: 1, siteId: 1}` on `thread_subscriptions` in `EnsureIndexes`
   (no existing index has `userAccount` as a prefix).

## Edge cases

- No thread subscriptions → all booleans false, `LastMessageAt` nil/omitted.
- `lastSeenAt` null / never seen → counts as unread.
- Thread room deleted (orphaned sub) → excluded (`$unwind tr` drops it).
- Parent room missing → unread/mention still count; DM check resolves false.
- Empty `UserAccount` in request → `errcode.BadRequest`.

## Testing (TDD, Red → Green)

- **Handler unit test** (`handler_test.go`, mocked `RoomStore`, mirrors
  `TestHandler_handleRoomsInfoBatch`): all-four-field mapping; empty
  `UserAccount` → `BadRequest`; store-error path; nil-`LastMessageAt` mapping.
  Table-driven.
- **Store integration test** (`integration_test.go`, `//go:build integration`,
  real Mongo via `testutil.MongoDB`): seed `thread_subscriptions` /
  `thread_rooms` / `rooms` covering — unread true & false, DM-unread,
  mention-only, `lastMessageAt` = max, never-seen sub, wrong-site filtered out,
  orphaned thread room, parent room missing, empty result.

## Docs

The subject is `chat.server.*`, not `chat.user.*`, so the `docs/client-api.md`
guardrail (which applies only to `chat.user.` RPCs) does not apply. No
client-api change required.
