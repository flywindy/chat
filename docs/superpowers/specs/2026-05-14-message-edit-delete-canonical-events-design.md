# Message Edit/Delete Canonical Events — Design

**Status:** Draft (revised 2x — see Revision note)
**Date:** 2026-05-14
**Branch:** `claude/search-message-editedat-updatedat`
**Related PRs:** #176 (search-service NATS migrations), #182 (this work)

> **Revision (post-review, 2026-05-15):** The original draft claimed edit/delete
> must use per-user `chat.user.<account>.event.room` fan-out because room-scoped
> `chat.room.<roomID>.event` "doesn't federate cross-site." That premise is
> wrong. Cross-site delivery is NATS gateway interest propagation, which routes
> **any** subject a remote client subscribes to — including `chat.room.<roomID>.event`.
> The existing create path proves this: `broadcast-worker.publishChannelEvent`
> publishes channel creates on the room-scoped subject (one publish), and only
> DM creates fan out per-user (`publishDMEvents`). Edit/delete now **mirror the
> create path exactly**: one room-scoped publish for channel rooms (content
> encrypted when the encryption toggle is on), per-subscriber publishes for DM
> rooms. This removes the publish-amplification regression for large channels
> (was N publishes per edit; now 1) and keeps create/edit/delete uniform.
> Decisions D3 and D11 below are superseded by this note where they conflict.

## 1. Goal

Bring message **edits** and **soft-deletes** into the same durable event-flow that new messages already use, so two downstream concerns are addressed in one mechanism:

1. **Server-side search consistency.** When a message is edited or deleted, `search-sync-worker` updates the home-site's ES index so `search.messages` results stay in sync with Cassandra.
2. **Client-side live updates everywhere.** Subscribers on remote sites — who never receive today's edit/delete live event because it's published as a non-per-user `chat.room.<roomID>.event` — start seeing edits/deletes in real time, **without any cross-site Cassandra/ES mirroring** (none is needed: messages are stored only at the room's home site).

The mechanism is the existing `MESSAGES_CANONICAL_<siteID>` JetStream stream, extended with two reserved subject suffixes (`.updated`, `.deleted`).

## 2. Background

### 2.1 What flows where today

```text
                                 ┌──── message-worker         (Cassandra persist, local)
                                 │
new msg ──► gatekeeper ──► canonical.<siteID>.created ──┼──── broadcast-worker     (per-user RoomEvent fan-out
                                                        │                           → federates to remote-site
                                                        │                           clients via chat.user.<acct>.event.room)
                                                        │
                                                        └──── search-sync-worker   (ES index, local)


edit msg ──► history-service ──► Cassandra (sync direct write, local only)
                            └─► chat.room.<roomID>.event   (single live publish, NOT per-user,
                                                            so it never federates to remote sites)


delete msg ──► history-service ──► Cassandra (sync direct write, local only)
                              └─► chat.room.<roomID>.event   (same problem as above)
```

**Two asymmetries:**

1. **No durable backend record of edits/deletes.** `search-sync-worker` can't observe them, so the ES index drifts from Cassandra after every edit/delete.
2. **No cross-site client notification for edits/deletes.** Creates federate to remote-site clients via NATS gateway interest propagation (DM creates use per-user `chat.user.<account>.event.room`; channel creates use room-scoped `chat.room.<roomID>.event`). Edits/deletes go out only on history-service's single non-durable publish, which doesn't ride the canonical stream, so remote-site clients learn about edits/deletes only on next refresh.

### 2.2 Multi-site data model — confirmation

Verified against the codebase:

- Messages in Cassandra (`messages_by_room`, `messages_by_id`) are written **only to the local Cassandra of the room's home site** (`message-worker/store_cassandra.go:67` and `history-service` direct writes). Not replicated cross-site.
- ES message index lives **only at the home site** (search-service uses CCS to query remote home sites for non-local rooms).
- `inbox-worker`'s OutboxEvent dispatch handles `member_added`, `member_removed`, `room_sync`, `role_updated`, `subscription_read`, `thread_subscription_upserted`, `room_created` — **zero message-content cases**. Membership replicates cross-site; message content does not.
- Cross-site live notifications work today because `broadcast-worker` publishes per-user-account subjects that the bridge routes; the bridge does NOT route `chat.room.<roomID>.event`.

**Implication:** edit/delete server-side work (Cassandra write, ES update) is **entirely local to the room's home site**. The only cross-site concern is delivering the live notification to remote-site clients, which is solved by routing edits/deletes through the same per-room-type fan-out `.created` uses (room-scoped for channels, per-subscriber for DMs).

### 2.3 What the original spec reserved

From `docs/superpowers/specs/2026-05-13-search-service-nats-migrations-design.md` §3:

> MESSAGES_CANONICAL: `chat.msg.canonical.{siteID}.created` (`.edited`, `.deleted` for future)

These suffixes are now in scope.

## 3. Architecture

### 3.1 New subject suffixes on the existing stream

`MESSAGES_CANONICAL_<siteID>` already has subject pattern `chat.msg.canonical.<siteID>.>`. Two reserved suffixes are now wired:

- `chat.msg.canonical.<siteID>.updated` — fired by history-service after a successful synchronous edit write to Cassandra. (Subject builder `subject.MsgCanonicalUpdated(siteID)` already exists in `pkg/subject/subject.go:156` from earlier work; we just start using it. The suffix is `.updated` rather than `.edited` because the codebase generalizes mutation-type names — `Message.EditedAt` remains the data field semantics.)
- `chat.msg.canonical.<siteID>.deleted` — fired by history-service after a successful synchronous soft-delete. (Subject builder `subject.MsgCanonicalDeleted(siteID)` already exists in `pkg/subject/subject.go:160`.)

**No new subject builders needed.** No stream-config change either — the wildcard `MsgCanonicalWildcard(siteID)` (`pkg/subject/subject.go:256`) already covers the new subjects, and the existing `MESSAGES_CANONICAL_<siteID>` stream's subject pattern is the same wildcard.

### 3.2 Producers and consumers (revised)

```text
canonical.<siteID>.created ─────┬──── message-worker         (FilterSubject: .created)  ← Cassandra persist
                                ├──── broadcast-worker        (FilterSubject: .>)        ← per-user RoomEvent fan-out
                                └──── search-sync-worker      (FilterSubject: .>)        ← ES index op

canonical.<siteID>.updated ─────┬──── broadcast-worker        (room-type fan-out, NEW)   ← live edit to clients (cross-site OK)
                                └──── search-sync-worker      (ES partial update, NEW)

canonical.<siteID>.deleted ─────┬──── broadcast-worker        (room-type fan-out, NEW)   ← live delete to clients (cross-site OK)
                                └──── search-sync-worker      (ES delete by _id, NEW)

NO direct live publish from history-service anymore. broadcast-worker is
the sole live-event emitter for ALL message mutations (create, edit,
delete), routing per room type exactly like the create path: room-scoped
chat.room.<roomID>.event for channels, per-subscriber
chat.user.<account>.event.room for DMs. Cross-site delivery is NATS
gateway interest propagation (same mechanism creates already rely on).
```

### 3.3 Per-consumer model: one consumer each, dispatch by subject suffix

Both `broadcast-worker` and `search-sync-worker` use a **single durable consumer** with `FilterSubject: chat.msg.canonical.<siteID>.>` and dispatch internally on subject suffix. Reasoning applies equally to both:

| Concern | One consumer (chosen) | Three separate consumers |
|---|---|---|
| **Cross-event-type ordering** | ✅ JetStream FIFO — `.created` for msg M always processed before `.edited` for msg M, `.edited` before `.deleted`. | ❌ Independent consumers drain at different rates; `.edited` could try to act on a doc whose `.created` hasn't been processed yet. |
| **Downstream serialization** | ES is per-`_id` serialized anyway; broadcast-worker per-room ordering is preserved naturally. | Theoretical concurrency that downstream serializes anyway. |
| **Operational simplicity** | One durable name per service, one metric set, one back-pressure curve. | 3× of each. |

The ordering argument is decisive — particularly for `search-sync-worker` (ES `_update` against a missing `_id` fails) and for `broadcast-worker` (clients render in stream order).

### 3.4 Why message-worker stays on `.created` only

| Concern | Decision |
|---|---|
| history-service already wrote Cassandra synchronously on edit/delete | A second write from message-worker is redundant at best, a race at worst. |
| Sync response semantics | history-service's edit/delete RPCs return `{messageId, editedAt}` to the client immediately after Cassandra succeeds. Moving persistence to message-worker would break the sync contract. |
| Single responsibility | message-worker is the **create-flow persistence step**. Edits short-circuit that pipeline by design. |

Implementation: message-worker's existing JetStream consumer gains an explicit `FilterSubject: chat.msg.canonical.<siteID>.created` so the broadened canonical stream doesn't accidentally feed it edits/deletes.

### 3.5 Why broadcast-worker is the sole live-event emitter

Today, `broadcast-worker` is the live-event fan-out for creates; `history-service` is the publisher for edit/delete live events (single-shot, separate code path). Unifying on `broadcast-worker` for all three mutation types yields:

- **Uniform fan-out.** Every mutation type routes through the same per-room-type logic broadcast-worker already uses for creates (room-scoped `chat.room.<roomID>.event` for channels, per-subscriber `chat.user.<account>.event.room` for DMs).
- **Cross-site delivery for free.** Remote-site clients receive edits/deletes via the same NATS gateway interest path that already works for creates.
- **JetStream redelivery on transient failure.** Today's `history-service` direct publish is fire-and-forget against a core-NATS subject (live, ephemeral). After this change, the live event is driven by a durable canonical consume, so a transient broadcast-worker failure just defers — it doesn't drop the notification.
- **Single source of truth.** Looking at the live event in production: it always came from broadcast-worker, regardless of mutation type. Easier to reason about, monitor, and debug.

**Tradeoff:** the live edit/delete event picks up one extra JetStream hop versus today's synchronous direct publish (canonical → broadcast-worker → fan-out). For typical NATS+JetStream latencies this is single-digit milliseconds — well below user-perceptible UI feedback thresholds. The upside (cross-site, durable redelivery, uniformity) is large.

### 3.6 Why history-service publishes the canonical event (not gatekeeper)

A natural question: creates flow through `message-gatekeeper` to canonical; shouldn't edits/deletes follow the same path? The answer is **no, and the codebase already establishes this pattern**.

**Verified producers on `MESSAGES_CANONICAL_<siteID>`:**

| Service | What it publishes | Why |
|---|---|---|
| `message-gatekeeper` | `.created` from client `msg.send` requests | Validates client message content; the gate for new messages. |
| `room-worker` | `.created` for system messages (member added/removed/role updated) | The service that performs the room mutation also announces it. (`room-worker/handler.go:366, 503, 792, 1274`) |
| `history-service` (this work) | `.updated` and `.deleted` | The service that performs the edit/delete announces it. |

**Pattern: "the service that performs the mutation publishes the canonical event."** Multi-producer canonical is already the norm.

**Why not route edit/delete through gatekeeper:**

| Concern | If gatekeeper produced `.updated`/`.deleted` |
|---|---|
| Sync response semantics | The `msg.edit` / `msg.delete` RPCs are `natsrouter` request/reply — the client expects an immediate `{messageId, editedAt}` reply. If gatekeeper consumes from the JetStream MESSAGES stream and emits canonical, there's no path to synchronously reply with the editedAt the client needs. |
| Who writes Cassandra? | If gatekeeper publishes canonical without writing, message-worker would have to grow `.updated`/`.deleted` handlers (violates D4). If history-service still writes Cassandra synchronously AND gatekeeper publishes canonical from the same client request, you'd have two parallel state operations with no clear ordering. |
| gatekeeper's existing scope | gatekeeper is a stateless validator/router for **new** message content. It doesn't read existing message state and so can't enforce sender-only edits, deleted-row protection, or the other invariants history-service already checks via Cassandra reads. |
| Symmetry argument is shallow | Creates and edits aren't symmetric: creates are fire-and-forget (no result the client cares about beyond the assigned msg ID), edits are sync confirmations of a state mutation. Different patterns warrant different producers. |

## 4. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | Use existing `MESSAGES_CANONICAL_<siteID>` stream; add subject suffixes `.updated` and `.deleted`. | Original spec reserved these (as `.edited`/`.deleted`; the codebase generalized the edit suffix to `.updated`). Stream wildcard already covers them. No new ops surface. |
| D2 | history-service publishes the canonical event **after** the synchronous Cassandra write. Best-effort (log+continue on publish failure). | Cassandra is the source of truth; the canonical publish is a downstream notification. A publish failure must not roll back the user-visible edit/delete. |
| D3 | **history-service stops publishing the live edit/delete event directly.** broadcast-worker becomes the sole live-event emitter (driven by canonical), routing per room type exactly like the create path: room-scoped `chat.room.<roomID>.event` for channels, per-subscriber `chat.user.<account>.event.room` for DMs. | Uniformity across mutation types; cross-site delivery via NATS gateway interest (same mechanism creates already rely on); durable redelivery on transient failure. Tradeoff: +single-digit-ms latency, considered acceptable. |
| D4 | message-worker adds `FilterSubject: chat.msg.canonical.<siteID>.created`. | Prevents accidental re-processing of edits/deletes on a broadened stream. |
| D5 | **broadcast-worker** uses one durable consumer with `FilterSubject: chat.msg.canonical.<siteID>.>`; dispatches by subject suffix. | Cross-event-type ordering preserved (clients see edit after create after delete for same msg). |
| D6 | **search-sync-worker** uses one durable consumer with `FilterSubject: chat.msg.canonical.<siteID>.>`; dispatches by subject suffix. | ES per-`_id` consistency requires stream-order FIFO. |
| D7 | Dispatch happens by the payload's `MessageEvent.Event` field (`EventCreated` / `EventUpdated` / `EventDeleted`), not by inspecting subject suffix. | The discriminator already exists on the payload and the existing `buildMessageAction` already branches on it. Subject suffix would require widening the `Collection.BuildAction` interface across all collections; payload-field dispatch is local to `messageCollection` and free. |
| D8 | `.updated` payload is a full `model.MessageEvent` with `Event: EventUpdated`, carrying the updated `Message` (with `EditedAt`+`UpdatedAt` populated). | Reuses the existing event type. Downstream consumers (broadcast-worker, search-sync-worker) can read whatever fields they need. |
| D9 | `.deleted` payload is also a `model.MessageEvent` with `Event: EventDeleted`. `Message` carries the bare minimum (`ID`, `RoomID`, `UpdatedAt`); `Content` may be empty. | Reuses the existing event type rather than introducing a parallel `MessageDeletedCanonicalEvent` — keeps the canonical stream uniform and lets `search-sync-worker`'s existing `EventDeleted` branch (`messages.go:121-128`) work unchanged. |
| D10 | ES action on `.updated` is a **full index op** (replace by `_id`) — matches the existing `buildMessageAction` behavior for non-deleted events. On `.deleted`: ES `_delete` by `_id` — also existing behavior. | The current code already does this correctly when given a `MessageEvent` with `Event: EventUpdated` / `EventDeleted`. No new action builders required. |
| D11 | broadcast-worker emits `model.RoomEvent` with `Type: RoomEventMessageEdited` / `RoomEventMessageDeleted`, routed by room type: channel rooms get one `subject.RoomEvent(roomID)` publish (content encrypted via `EncryptedNewContent` when `h.encrypt` is on, mirroring `publishChannelEvent`); DM rooms get one `subject.UserRoomEvent(account)` publish per subscriber (mirroring `publishDMEvents`). | Single emission path for all mutations, identical routing to `.created`. Avoids per-subscriber publish amplification on large channels. (Subject naming: clients render "edited"/"deleted"; canonical subject uses "updated" for generality.) |
| D12 | No cross-site Cassandra or ES mirroring of message content. | Verified: messages live only at the room's home site. Remote-site reads cross-site to the home. Edit/delete server-side work is entirely home-site-local. |
| D13 | No OUTBOX/INBOX wiring for `.edited`/`.deleted`. | OUTBOX/INBOX is for cross-site membership/room federation (verified: `inbox-worker` dispatch has zero message-content cases). The cross-site need here is live client notification, which the per-user-account NATS bridge already handles. |

## 5. Wire Contracts

### 5.1 Subject builders (`pkg/subject/subject.go`)

**Already in place — no changes required.** The codebase has:

```go
func MsgCanonicalCreated(siteID string) string { return fmt.Sprintf("chat.msg.canonical.%s.created", siteID) }
func MsgCanonicalUpdated(siteID string) string { return fmt.Sprintf("chat.msg.canonical.%s.updated", siteID) }
func MsgCanonicalDeleted(siteID string) string { return fmt.Sprintf("chat.msg.canonical.%s.deleted", siteID) }
func MsgCanonicalWildcard(siteID string) string { return fmt.Sprintf("chat.msg.canonical.%s.>", siteID) }
```

(Lines 152, 156, 160, 256 of `pkg/subject/subject.go`.) This work just starts publishing/consuming the previously-reserved `.updated` and `.deleted` subjects.

### 5.2 `.updated` payload — `model.MessageEvent` with `Event: EventUpdated`

Reuses the existing `pkg/model.MessageEvent` (which already has an `Event EventType` field). On an edit, `Message.EditedAt` and `Message.UpdatedAt` are populated; `Message.Content` is the new content; `Event: model.EventUpdated`.

```json
{
  "event": "updated",
  "siteId": "site-a",
  "timestamp": 1747304400000,
  "message": {
    "id": "msg-uuid",
    "roomId": "r1",
    "userId": "u1",
    "userAccount": "alice",
    "content": "hello (edited)",
    "createdAt": "2026-05-14T11:00:00Z",
    "editedAt": "2026-05-14T11:05:00Z",
    "updatedAt": "2026-05-14T11:05:00Z"
  }
}
```

### 5.3 `.deleted` payload — `model.MessageEvent` with `Event: EventDeleted`

Same envelope type, minimal `Message` payload (only the fields needed for downstream identification and timestamping):

```json
{
  "event": "deleted",
  "siteId": "site-a",
  "timestamp": 1747304400000,
  "message": {
    "id": "msg-uuid",
    "roomId": "r1",
    "userId": "u1",
    "userAccount": "alice",
    "createdAt": "2026-05-14T11:00:00Z",
    "updatedAt": "2026-05-14T11:10:00Z"
  }
}
```

`Message.UpdatedAt` is the delete time (same value Cassandra writes as `updated_at`). `Content` may be empty. The existing `buildMessageAction` (`search-sync-worker/messages.go:114-128`) already branches on `Event == EventDeleted` and produces an ES `Delete` action — works unchanged.

### 5.4 Live `RoomEvent` emitted by broadcast-worker

Broadcast-worker now emits three `RoomEvent` variants (existing + two new):

| Source canonical subject | `RoomEvent.Type` | Payload extras |
|---|---|---|
| `.created` | `RoomEventNewMessage` (existing) | Existing — message body |
| `.updated` | `RoomEventMessageEdited` (NEW) | `{messageId, newContent, editedAt, updatedAt}` |
| `.deleted` | `RoomEventMessageDeleted` (NEW) | `{messageId, deletedBy, deletedAt, updatedAt}` |

(Exact field names align with whatever the existing `model.RoomEvent` envelope/sub-payload conventions are — finalized in the implementation plan.)

The emitted live event is published per-subscriber to `subject.UserRoomEvent(account)`, the same path used for `.created`. Cross-site delivery is automatic via the existing per-user-account NATS bridge.

## 6. Per-Service Changes

### 6.1 `history-service`

| File | Change |
|---|---|
| `internal/service/messages.go` `EditMessage` | After `msgWriter.UpdateMessageContent` succeeds, build a `model.MessageEvent{Event: model.EventUpdated, SiteID: siteID, Timestamp: editedAtMs, Message: <updated Message with EditedAt+UpdatedAt set>}` and publish to `subject.MsgCanonicalUpdated(siteID)`. **Remove the direct `chat.room.<roomID>.event` publish** — broadcast-worker now handles it. |
| `internal/service/messages.go` `DeleteMessage` | After `msgWriter.SoftDeleteMessage` succeeds, build a `model.MessageEvent{Event: model.EventDeleted, ...}` carrying the minimum identifying `Message` fields and publish to `subject.MsgCanonicalDeleted(siteID)`. **Remove the direct `chat.room.<roomID>.event` publish.** |
| `internal/service/messages_test.go` | Assert canonical publishes (subject + payload); assert publish-failure tolerance (RPC still returns success); remove tests that asserted on the direct RoomEvent publish (or replace with canonical-only assertions). |

### 6.2 `message-worker`

| File | Change |
|---|---|
| `main.go` (or wherever the consumer is configured) | Add `nats.ConsumerConfig.FilterSubject: subject.MsgCanonicalCreated(siteID)` to the existing canonical consumer config. |
| `*_test.go` | Unit test asserting the FilterSubject is set. |

### 6.3 `broadcast-worker`

| File | Change |
|---|---|
| Consumer config | Update `FilterSubject` from `chat.msg.canonical.<siteID>.created` to wildcard `chat.msg.canonical.<siteID>.>`. |
| `handler.go` | Add dispatch on `MessageEvent.Event` (per D7). `EventCreated` → existing path. `EventUpdated` → build `RoomEvent{Type: RoomEventMessageEdited}`; `EventDeleted` → `RoomEvent{Type: RoomEventMessageDeleted}`. Route via `fanOutMutationEvent`: channel → single `subject.RoomEvent(roomID)` publish (encrypt `MessageEdited` content when `h.encrypt`); DM → per-subscriber `subject.UserRoomEvent(account)`. |
| `handler.go` | Helpers: `handleUpdated(ctx, evt)`, `handleDeleted(ctx, evt)`, shared `fanOutMutationEvent(...)`, plus `encryptEditedContent` for the encrypted-channel case. |
| `handler_test.go` | Dispatch tests: channel `EventUpdated`/`EventDeleted` → single room-scoped publish (+ encrypted-channel variant); DM → per-subscriber publishes. |
| `integration_test.go` | End-to-end: publish canonical `.updated`, assert the channel room subject (or DM per-user subjects) receive `RoomEventMessageEdited`. |

### 6.4 `search-sync-worker`

**No code changes needed.** Verified during spec revision:

- `messageCollection.FilterSubjects` returns `nil` (`search-sync-worker/messages.go:37`) — already subscribes to the full `MESSAGES_CANONICAL_<siteID>` stream wildcard.
- `messageCollection.BuildAction` already dispatches by `evt.Event` (`search-sync-worker/messages.go:50`).
- `buildMessageAction` already maps `Event: EventDeleted` → ES `Delete` action and everything else → ES `Index` action (which replaces the doc on `.updated`, achieving the desired upsert) (`search-sync-worker/messages.go:114-138`).
- `integration_test.go:330-400` already publishes `.created`/`.updated`/`.deleted` to canonical and verifies the ES end state. Should pass as-is once history-service starts publishing.

The only required action is to **run the existing integration test** to confirm it now exercises the full flow with real `.updated`/`.deleted` events from history-service.

### 6.5 `pkg/model`

| File | Change |
|---|---|
| `message.go` | (already in this PR) `EditedAt` + `UpdatedAt` on `Message`. No further change. |
| `event.go` | Add `RoomEventMessageEdited` and `RoomEventMessageDeleted` constants (alongside the existing `RoomEventNewMessage` near line 138) and any payload sub-types broadcast-worker needs to embed in `RoomEvent` for the new mutation types. |
| `model_test.go` | Round-trip tests for the new `RoomEvent` variants. |

(No new `MessageDeletedCanonicalEvent` — D9 reuses `MessageEvent`.)

### 6.6 `pkg/subject`

**No changes.** `MsgCanonicalUpdated` and `MsgCanonicalDeleted` builders already exist (D1/§5.1).

## 7. Error Handling

| Condition | Behavior |
|---|---|
| history-service Cassandra edit/delete succeeds; canonical publish fails | Log `WARN` with `messageID`/`roomID`/`error`; RPC still returns success. The Cassandra mutation is durable; ES + live notification recover on next successful publish. (Matches today's "best-effort" model for the live event publish.) |
| broadcast-worker `.edited` → subscriber lookup fails | NACK + redelivery via JetStream; recurring failures eventually move to dead-letter. |
| broadcast-worker per-user publish fails (transient NATS issue) | Log + continue with remaining subscribers; the broadcast-worker's existing per-subscriber error handling for `.created` applies. |
| search-sync-worker tries to `update` a non-existent ES doc | **Cannot happen with the single-consumer FIFO design** — `.created` always processed before `.edited` for the same msg. If the doc is simply missing for unrelated reasons (e.g., ES wipe), the partial update fails; NACK + redeliver; eventually dead-letter. |
| search-sync-worker tries to `delete` a non-existent ES doc | ES returns 404; treat as no-op + ack. |
| `.edited` / `.deleted` payload deserialization fails | Log + ack (poison-message handling matches the existing `.created` path). |

## 8. Observability

| Metric | Service |
|---|---|
| `history_service_canonical_publish_total{result, kind}` | history-service: `kind` ∈ {`edited`, `deleted`}; `result` ∈ {`ok`, `error`}. New. |
| `broadcast_worker_room_events_total{type}` | broadcast-worker: `type` ∈ {`new_message`, `message_edited`, `message_deleted`}. Either extend existing metric or add. |
| `search_sync_worker_index_ops_total{op}` | search-sync-worker: `op` ∈ {`index`, `update`, `delete`}. Either extend existing or add. |

Per-event logs: `INFO` for successful publishes / index ops; `WARN` for canonical publish failures in history-service; `ERROR` for non-recoverable index errors in search-sync-worker after retries.

## 9. Testing Strategy

Per CLAUDE.md §4: TDD red→green→refactor→commit.

**Unit tests:**
- `pkg/subject/subject_test.go` — new builder rows.
- `pkg/model/model_test.go` — round-trip tests for the new `RoomEvent` variants (`RoomEventMessageEdited`, `RoomEventMessageDeleted`).
- `history-service/internal/service/messages_test.go` — assert canonical publishes on `EditMessage` and `DeleteMessage`; assert publish-failure tolerance (RPC still succeeds); remove (or rewrite) tests that asserted on the old direct RoomEvent publish.
- `message-worker/*_test.go` — assert consumer config has `FilterSubject: chat.msg.canonical.<siteID>.created`.
- `broadcast-worker/handler_test.go` — dispatch tests: `.created` (existing) → new-message fan-out; `.updated`/`.deleted` → channel room-scoped publish (+ encrypted-channel variant) and DM per-subscriber publish, with `RoomEventMessageEdited` / `RoomEventMessageDeleted` payloads.
- `search-sync-worker/messages_test.go` — dispatch tests: `.created` → ES index; `.edited` → ES update; `.deleted` → ES delete.

**Integration tests:**
- `broadcast-worker/integration_test.go` (if present, else added) — wire a NATS testcontainer, publish a canonical edit, assert per-user subjects receive the `RoomEventMessageEdited`.
- `search-sync-worker/messages_integration_test.go` — testcontainers ES: create → assert doc indexed; edit → assert doc updated; delete → assert 404.

## 10. Out of Scope

- Reactions, pins, and other future mutations — pattern is the same (`.reaction-added`, etc.) but each is a separate PR.
- A separate "soft-delete in search" mode (keep deleted messages with a `deletedAt` flag in ES). We hard-delete. Additive change later if needed.
- Edit-trail UI rendering (`(edited)` badges, hover for "edited at HH:MM"). Frontend PR.
- Live `MessageEditedEvent` / `MessageDeletedEvent` model-type changes — broadcast-worker emits `RoomEvent` variants instead. The legacy live-event types in `history-service/internal/models/event.go` become unused by backend code after history-service stops publishing them directly; whether to delete them now or leave them for a cleanup PR is left to implementer judgment (delete-now is preferred if no live client code consumes them as a typed struct).

## 11. Follow-ups

1. **Edit-trail UI.** With `editedAt` + `updatedAt` now flowing through to both `search.messages` results and live `RoomEvent`s, the frontend can render "(edited)" badges and hover details. Separate frontend PR.
2. **Analytics consumer.** The canonical `.edited` / `.deleted` events could feed an analytics aggregator (counts per user/hour, etc.). Drop-in additional consumer on the existing stream.
3. **Reactions on canonical.** Same pattern: `chat.msg.canonical.<siteID>.reaction-added` / `.reaction-removed`. Add when reactions land.
4. **Cleanup of legacy live event types** (if not done in this PR): remove `models.MessageEditedEvent` / `models.MessageDeletedEvent` from `history-service` once we confirm no client/library code consumes them as a typed shape.
