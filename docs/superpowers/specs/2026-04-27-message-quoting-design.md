# Message Quoting Design

Date: 2026-04-27
Status: Approved (pending implementation plan)

## Goal

Let a user reply to a chat message with a "quote" — the new message renders alongside a snapshot of the parent message it's quoting. The snapshot must be visible to every user in the room (it ships through real-time delivery, push notifications, and persisted history).

## Scope

- Same conversation context only. Main-room messages can only quote main-room messages; messages inside thread `T` can only quote other messages in thread `T`. Quoting the thread's parent message from inside the thread is **not** allowed (strict rule). The room-scoping check from `history-service.findMessage` is necessary but no longer sufficient — gatekeeper additionally compares `parent.ThreadParentID` against the new message's `ThreadParentMessageID`.
- Thread reply and quote are independent — a single message may carry both, neither, or only one (subject to the conversation-context rule above).
- MVP snapshot fields: `messageId`, `roomId`, `sender`, `createdAt`, `msg`, `mentions`, `messageLink`, `threadParentId`, `threadParentCreatedAt`. `attachments` is out of scope for MVP.
- Quoting a deleted parent is allowed. `history-service.GetMessageByID` does not filter on the `deleted` flag, so deleted parents return a normal snapshot. The snapshot is captured at quote time; the parent's later deletion does not invalidate it.
- Hard-fail policy: any failure to resolve or accept the parent (not found, RPC error, timeout, forbidden by access-window, thread-context mismatch) fails the entire send. Gatekeeper replies to the client with the error so the user can retry. The new message is **not** published to MESSAGES_CANONICAL.

## Non-goals

- No editing of the snapshot after the fact.
- No cross-room or cross-thread-room quoting.
- No new feature flag — gated by clients sending the new request field.
- No data migration. The two new UDT fields are added via `ALTER TYPE` (additive, online-safe, no row backfill needed). See the "Cassandra schema migration" section.

## Architecture

```
client ──(SendMessageRequest{quotedParentMessageId})──> MESSAGES stream
                                                            │
                                                            ▼
                                  message-gatekeeper
                                  ├─ existing validation (UUID, content, sub)
                                  ├─ if quotedParentMessageId != "":
                                  │   ├─ NATS request to history-service
                                  │   │   subject: chat.user.{account}.request.room.{roomID}.{siteID}.msg.get
                                  │   │   payload: {"messageId": "..."}
                                  │   ├─ on success: project to *cassandra.QuotedParentMessage,
                                  │   │              set msg.QuotedParentMessage
                                  │   └─ on any error: reply error to client, ack, do NOT publish
                                  └─ publish MessageEvent → MESSAGES_CANONICAL
                                                            │
              ┌─────────────────────────────────┬───────────┴───────────────────────┐
              ▼                                 ▼                                   ▼
        message-worker                    broadcast-worker                  notification-worker
        persists snapshot in              ships snapshot to clients         no change in MVP
        quoted_parent_message             via existing fan-out              (snapshot rides on Message)
        column on insert
```

The canonical event is the single source of truth. Once gatekeeper has built the snapshot, no downstream worker re-resolves it.

## Why call history-service via RPC instead of querying Cassandra in gatekeeper?

- `history-service.GetMessageByID` already implements lookup, room match (via subject param), subscription check, and access-window enforcement. The access-window check is desirable for quoting — a user who can't see the parent shouldn't be able to surface its content in a quote.
- Keeps gatekeeper's dependency surface unchanged (Mongo only). Cassandra reads stay owned by history-service.
- Room-level scoping is enforced by `findMessage` (`history-service/internal/service/utils.go:59`) — same `roomID` between subject and parent. **Thread-level scoping is enforced by gatekeeper itself** after the RPC: it compares `parent.ThreadParentID` (returned in the `cassandra.Message` body) against `req.ThreadParentMessageID` and fails the request on mismatch. Both checks are needed because thread replies share the room ID with their main-room parent, so a `roomID` match alone does not imply same-thread.
- Trade-off accepted: synchronous NATS hop on every quoted send, bounded by a 2-second timeout. On timeout or any RPC error, the request fails and the client retries.

The user-scoped subject is published on by gatekeeper acting on behalf of the sender — the sender's `account` and `roomID` come from the inbound MESSAGES subject. natsrouter parses params from the subject regardless of publisher identity, and all auth checks pass because the sender genuinely is subscribed.

## Wire-format changes (`pkg/model`)

### `pkg/model/message.go`

```go
import "github.com/hmchangw/chat/pkg/model/cassandra"

type SendMessageRequest struct {
    ID                           string `json:"id"`
    Content                      string `json:"content"`
    RequestID                    string `json:"requestId"`
    ThreadParentMessageID        string `json:"threadParentMessageId,omitempty"`
    ThreadParentMessageCreatedAt *int64 `json:"threadParentMessageCreatedAt,omitempty"`
    QuotedParentMessageID        string `json:"quotedParentMessageId,omitempty"` // NEW
}

type Message struct {
    // ...existing fields...
    QuotedParentMessage *cassandra.QuotedParentMessage `json:"quotedParentMessage,omitempty" bson:"quotedParentMessage,omitempty"` // NEW
}
```

### Why reuse `cassandra.QuotedParentMessage` directly

It's already defined in the shared `pkg/model/cassandra` package, has both `json` and `cql` tags, and was created for this exact use. history-service already aliases it as `models.QuotedParentMessage`. Defining a parallel `model.QuotedParentMessage` would duplicate fields and introduce conversion code on the message-worker side.

The `Sender` and `Mentions` inside the snapshot are `cassandra.Participant` (id, engName, companyName, account, appId, appName, isBot) — a slightly different shape than the top-level `model.Participant` used by `Message.Mentions` (userId, account, chineseName, engName). Both shapes are already part of the wire vocabulary (history-service ships `cassandra.Participant` directly to clients), so this isn't introducing a new shape.

### `pkg/model/model_test.go`

Add a JSON round-trip case for `Message` carrying a populated `QuotedParentMessage`.

## Subject helper (`pkg/subject`)

### `pkg/subject/subject.go`

```go
// MsgGet returns the concrete subject for issuing a GetMessageByID request.
// The natsrouter pattern variant MsgGetPattern is used by history-service
// to register the handler.
func MsgGet(account, roomID, siteID string) string {
    return fmt.Sprintf("chat.user.%s.request.room.%s.%s.msg.get", account, roomID, siteID)
}
```

Project rule (CLAUDE.md): use `pkg/subject` builders, never raw `fmt.Sprintf` at call sites.

## Gatekeeper changes (`message-gatekeeper`)

### `store.go` — new interface (kept separate from existing `Store`)

```go
//go:generate mockgen -destination=mock_store_test.go -package=main . Store,ParentMessageFetcher

type ParentMessageFetcher interface {
    // FetchQuotedParent issues an RPC to history-service and returns a snapshot
    // suitable for embedding on a quoting message. Returns an error for any
    // condition that should fail the request (not found, RPC timeout, etc.).
    FetchQuotedParent(ctx context.Context, account, roomID, siteID, messageID string) (*cassandra.QuotedParentMessage, error)
}
```

### `fetcher_history.go` — new file

Implements `ParentMessageFetcher`:

1. Build subject via `subject.MsgGet(account, roomID, siteID)`.
2. Marshal the request as `{"messageId": "..."}` (a small local struct in gatekeeper, mirroring `history-service/internal/models.GetMessageByIDRequest`; we duplicate the wire shape rather than cross-import a service-internal package).
3. `nc.Request(subj, data, 2*time.Second)`.
4. Decode the natsrouter response envelope:
   - Success → `*cassandra.Message` body. Project to `*cassandra.QuotedParentMessage` by copying `MessageID`, `RoomID`, `Sender`, `CreatedAt`, `Msg`, `Mentions`, and computing `MessageLink` (see below). Leave `Attachments` zero.
   - Error envelope (NotFound, Forbidden, Internal) → return wrapped error.
5. NATS error (no responder, timeout) → return wrapped error.

`MessageLink` is built server-side as `fmt.Sprintf("%s/%s/%s", chatBaseURL, parent.RoomID, parent.MessageID)` using a new `CHAT_BASE_URL` env var (see config). The fetcher receives `chatBaseURL` via constructor injection so unit tests can supply any value.

The 2-second timeout is the standard NATS Go client default and is hardcoded inline at the single call site. No env var is added — promote to config later if ops needs to tune it.

### `handler.go` — one new branch in `processMessage`

After existing validation (UUID, content size, thread-pair, subscription), and before constructing the `Message`. Quote resolution is delegated to a `resolveQuoteSnapshot` helper that returns `(*snapshot, error)` — the caller propagates the error so the existing validation-error path replies to the client and acks the JetStream message:

```go
quotedSnapshot, err := h.resolveQuoteSnapshot(ctx, account, roomID, siteID, req.QuotedParentMessageID, req.ThreadParentMessageID)
if err != nil {
    return nil, err
}

msg := model.Message{
    // ...existing fields...
    QuotedParentMessage: quotedSnapshot,
}
```

The helper itself:

```go
func (h *Handler) resolveQuoteSnapshot(ctx context.Context, account, roomID, siteID, quotedParentMessageID, newMessageThreadID string) (*cassandra.QuotedParentMessage, error) {
    if quotedParentMessageID == "" {
        return nil, nil
    }
    snap, err := h.parentFetcher.FetchQuotedParent(ctx, account, roomID, siteID, quotedParentMessageID)
    switch {
    case err != nil:
        return nil, fmt.Errorf("fetch quoted parent %s: %w", quotedParentMessageID, err)
    case snap == nil:
        // Treat the fetcher's contract violation as a hard failure rather than
        // silently dereferencing snap.ThreadParentID below.
        return nil, fmt.Errorf("fetch quoted parent %s: fetcher returned nil snapshot", quotedParentMessageID)
    case snap.ThreadParentID != newMessageThreadID:
        return nil, fmt.Errorf("quoted parent %s thread context mismatch: parent thread %q, new message thread %q",
            quotedParentMessageID, snap.ThreadParentID, newMessageThreadID)
    default:
        return snap, nil
    }
}
```

No UUID validation on `QuotedParentMessageID` — if the client sends garbage, the RPC's NotFound path surfaces the error to the client.

### `main.go` — wiring

Add one new config field:

```go
ChatBaseURL string `env:"CHAT_BASE_URL" envDefault:"http://localhost:3000"`
```

Default matches the chat-frontend dev port (`chat-frontend/vite.config.js:7`). Production overrides via env var.

Construct the fetcher with the existing `nc *nats.Conn` plus `cfg.ChatBaseURL`, and pass it to `NewHandler`. No new infrastructure dependencies.

### Tests (`message-gatekeeper/handler_test.go` + `fetcher_history_test.go`)

Handler table-driven cases via `MockParentMessageFetcher`:
- Quote field unset → fetcher not called.
- Quote field set, fetcher returns snapshot → snapshot embedded on canonical event.
- Quote field set, fetcher returns error → request fails with the wrapped error, message NOT published.
- Thread + quote both set → both fields propagate.

Fetcher tests against an in-process NATS server:
- History responds with success → returns projected snapshot, including `MessageLink` built from the injected `chatBaseURL`.
- History responds with error envelope → returns error.
- No responder / timeout → returns error.

## Message-worker changes (`message-worker`)

### `store_cassandra.go` — extend INSERT statements

`SaveMessage`: bind `quoted_parent_message` in both `messages_by_room` and `messages_by_id` INSERTs, value = `msg.QuotedParentMessage` (a `*cassandra.QuotedParentMessage`). gocql marshals nil as null UDT and a populated struct as the UDT via the existing `cql:` tags.

`SaveThreadMessage`: bind `quoted_parent_message` in both `messages_by_id` and `thread_messages_by_room` INSERTs, same binding.

No conversion code — `evt.Message.QuotedParentMessage` is already the storage type.

### Tests

- `handler_test.go`: extend the handler table — when the canonical event carries a snapshot, the mocked store receives the same pointer.
- `store_cassandra_test.go` (`//go:build integration`): add a case that calls `SaveMessage` (and `SaveThreadMessage`) with a populated snapshot, then reads `messages_by_id` / `messages_by_room` / `thread_messages_by_room` and asserts the UDT round-trips intact.

## What is NOT changed

- `history-service` — zero changes, we call its existing RPC.
- `broadcast-worker` — zero changes, `Message` propagates the new field automatically.
- `notification-worker` — zero changes for MVP.
- Cassandra schema (`docs/cassandra_message_model.md`, `docker-local/cassandra/init/*.cql`) — `quoted_parent_message` column already exists in all four tables.
- `cassandra.QuotedParentMessage` struct — used as-is.
- Gatekeeper's existing `Store` interface, Mongo store, subscription validation, content validation, thread validation — all untouched.

## Hard-fail policy (full enumeration)

| Failure mode | Gatekeeper behavior |
|---|---|
| Client sends invalid UUID for quotedParentMessageId | RPC returns NotFound → request fails, error replied to client |
| Parent message not found in Cassandra | RPC returns NotFound → request fails, error replied to client |
| Parent in a different room (different `roomId`) | RPC returns NotFound (room param in subject doesn't match) → request fails, error replied to client |
| Parent has a different thread context (`parent.ThreadParentID != req.ThreadParentMessageID`) | gatekeeper post-RPC check → request fails, error replied to client. Covers: main-room msg quoting a thread reply; thread-T msg quoting main; thread-T msg quoting thread-T'; thread-T msg quoting its own thread-parent main-room message. |
| Parent has `deleted = true` | history-service returns the row (no deleted filter in `findMessage`) → snapshot embedded normally, message ships |
| Sender's `historySharedSince` is after parent's createdAt | RPC returns Forbidden → request fails, error replied to client |
| history-service unreachable | NATS no-responder error → request fails, error replied to client |
| history-service slow (>2s) | NATS timeout → request fails, error replied to client |
| Any other error (including buggy fetcher returning `(nil, nil)`) | Wrapped error returned → request fails, error replied to client |

The user explicitly intended to send a quoted reply; silently stripping the quote and shipping a plain message would be misleading. Failing the whole send gives the user clear feedback so they can correct the request (fix the parent ID, retry after a transient hiccup, drop the quote, etc.).

Errors are classified as **validation errors** (plain `error`), not `infraError`, so the existing handler ack-and-reply path runs: the JetStream message is acked (no redelivery loop), and the client receives the error on its response subject.

## Cassandra schema migration

Two new fields are appended to the `QuotedParentMessage` UDT. UDT field additions in Cassandra are additive, online-safe, and require no row backfill — existing rows simply read back the new fields as NULL.

### Updated UDT (single source of truth: `docs/cassandra_message_model.md`)

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
  thread_parent_id         TEXT,        -- NEW
  thread_parent_created_at TIMESTAMP    -- NEW
);
```

### Production migration

```cql
ALTER TYPE chat."QuotedParentMessage" ADD thread_parent_id TEXT;
ALTER TYPE chat."QuotedParentMessage" ADD thread_parent_created_at TIMESTAMP;
```

### Local-dev DDL

`docker-local/cassandra/init/06-udt-quoted_parent_message.cql` is updated with the two new fields. The local stack typically runs the init scripts on a fresh keyspace, so `CREATE TYPE IF NOT EXISTS` simply produces the right shape from scratch. Long-lived dev databases need the same `ALTER TYPE` as prod.

### Hand-rolled UDTs in tests and standalone dev stacks

Two more places repeat the UDT shape and must stay in sync:

- `history-service/internal/cassrepo/integration_test.go:42` declares the UDT inline for testcontainers.
- `history-service/docker-local/docker-compose.yml:68` declares the UDT in the standalone history-service local-dev stack (separate from the project-wide `docker-local/cassandra/init/*.cql`).

Both are updated alongside the canonical UDT in this PR.

## Deploy order

1. Run the two `ALTER TYPE` statements against each site's Cassandra keyspace.
2. Deploy new `message-worker` (gocql binds the two new fields when present, writes NULL when nil — safe regardless of which gatekeeper version is producing events).
3. Deploy new `message-gatekeeper` (starts populating the new snapshot fields and enforcing the thread-context rule).

## TDD ordering

Per CLAUDE.md, every change lands as Red → Green → Refactor → Commit:

1. `cassandra.QuotedParentMessage` UDT — Go struct + tests + `cassandra_message_model.md` + local-dev DDL + history-service inline UDT.
2. `pkg/model` field additions + round-trip test (uses the new UDT fields in fixtures).
3. `pkg/subject.MsgGet` helper + test.
4. `message-gatekeeper` `ParentMessageFetcher` interface + handler branch (incl. thread-context guard) + mock + handler tests.
5. `message-gatekeeper` `fetcher_history.go` (projects the two new fields) + fetcher tests against in-process NATS.
6. `message-gatekeeper` `main.go` wiring.
7. `message-worker` INSERT extensions + handler test extensions.
8. `message-worker` integration test for round-trip persistence (asserts new fields).

## Backward compatibility & rollout

- All new fields are `omitempty` — old clients and old service binaries see no behavior change.
- Old gatekeeper deployed against new model: just never sets the field; canonical event has nil snapshot; everything works.
- Old message-worker deployed against new event: gocql binds nil UDT; INSERTs without the new column are still valid (Cassandra ignores unspecified columns). However, message-worker SHOULD be deployed before frontend starts sending quotes, so snapshots are persisted from day one.
- The two new UDT fields require an `ALTER TYPE` migration (additive, online-safe; see "Cassandra schema migration"). Run before deploying the new gatekeeper. Old binaries reading the type after the migration are unaffected — extra UDT fields are tolerated by gocql.
- No feature flag.
- One new env var on gatekeeper (`CHAT_BASE_URL`, defaults to `http://localhost:3000`).

## Observability

Quote-resolution failures surface through the existing `"process message failed"` ERROR log in `HandleJetStreamMsg` (with `error`, `account`, `roomID` fields) — the wrapped error from `resolveQuoteSnapshot` describes which arm tripped (RPC failure, thread-context mismatch, or buggy fetcher).

No new metrics in MVP.
