# notification-worker — system-message notification allowlist

**Date:** 2026-06-04
**Status:** Approved (design)
**Scope:** `notification-worker` only. One change, shippable as a single commit.

## Problem

`notification-worker` consumes **every** message off `MESSAGES_CANONICAL` and is
itself the point that decides whether a message produces a mobile push. Today it
has no positive notion of a "notifiable" message — it runs the full fan-out for
all messages and relies on incidental facts to keep system messages quiet.

That suppression is **emergent, not stated**. `members_added` (and other system
messages) carry non-empty `Content` and flow through the entire fan-out
(`handler.go` `HandleMessage`). They produce no push only because:

1. member add/remove are guarded to channels, and
2. for channels, `EligibleForPush` requires `mentioned` (`handler.go:152`), and
   the system text happens not to parse as an `@mention`.

A future system-message type that (a) is delivered to a DM/group where push does
not require a mention, or (b) contains text `mention.Parse` reads as a handle,
**will leak a push**. With new system-message types planned, the worker needs a
rule that is safe-by-default rather than an implicit one-condition blocklist.

### Legacy contract being restored

In the legacy system the push service sat behind a filter and only ever saw a
small allowlist of *regular* types (`""`, and historically `tcard`,
`tcard_execute`, `app_execute`). The long, growing list of system types
(joined/left/renamed/archived/privacy/avatar/topic/e2e/role-changed/
`members_added`/mute/whiteboard/meet-started/…) never reached it. This spec
restores that allowlist semantics at the worker, which is now the filter point.

## Constraint: cache invalidation rides on member system-messages

The worker also performs a **side effect** on three member-change system
messages — it invalidates the room's member cache (`handler.go:74-80`,
"Option C"):

```go
switch msg.Type {
case model.MessageTypeMembersAdded, model.MessageTypeMemberLeft, model.MessageTypeMemberRemoved:
    h.deps.Members.Invalidate(ctx, msg.RoomID)
}
```

A naive "skip system messages early" guard at the top of `HandleMessage` would
short-circuit *before* this invalidation and silently break membership freshness
(masked by the TTL, so it would not fail loudly). The fix must keep the side
effect while gating the notification.

## Design

Split `HandleMessage` into two explicitly ordered phases.

**Phase 1 — side effects (runs for all messages):** the existing member-change
`Type` switch that calls `Invalidate`. Unchanged in behavior; runs first.

**Phase 2 — notification gate (safe-by-default allowlist):** immediately after
Phase 1 and **before** `GetMembers`, return early unless the type is notifiable:

```go
// Phase 1 — side effects: member-change sys-messages invalidate the member cache.
switch msg.Type {
case model.MessageTypeMembersAdded, model.MessageTypeMemberLeft, model.MessageTypeMemberRemoved:
    h.deps.Members.Invalidate(ctx, msg.RoomID)
}

// Phase 2 — notification gate: only regular message types push. Every system
// type (current and future) is silently non-notifying.
if !isNotifiable(msg.Type) {
    return nil
}

// ...existing fan-out (GetMembers → candidates → emit) unchanged...
```

```go
// isNotifiable reports whether a message type produces push notifications.
// Allowlist semantics restore the legacy "push service only sees regular
// messages" guarantee: new system types are non-notifying by construction,
// not by accident of mention-parsing.
func isNotifiable(msgType string) bool {
    switch msgType {
    case "": // plain user message. tcard/tcard_execute/app_execute join here if/when they land.
        return true
    default:
        return false
    }
}
```

`isNotifiable` is local to `notification-worker`; no new `pkg/model` constant.
The allowlist is `{""}` today — that is the only notifiable type in the current
system. New regular types are added here as one-line cases.

### Behavior changes

1. **Closes the latent leak.** `room_created`, `room_renamed`, `room_restricted`,
   and `members_added` are excluded by type, not by "channel + no parsed
   mention". A future system message with mention-like text, or one delivered to
   a DM/group, can no longer leak a push.
2. **Gate sits after invalidation, before `GetMembers`.** Member-change system
   messages still invalidate but no longer fetch members or run fan-out. No type
   is both invalidating *and* notifiable (the sets are disjoint), so the
   "invalidate before read" ordering guarantee still holds for the messages that
   matter, and we drop a pointless member lookup + fan-out for every system
   message.

## Tests (TDD — Red first)

- **New** — every system type (`room_created`, `room_renamed`, `room_restricted`,
  `members_added`, `member_left`, `member_removed`) produces **zero** emitter
  calls.
- **New (leak regression)** — a system message with non-empty `Content` *and* an
  `@handle`-like token produces zero push. This is the test that would have
  caught the latent leak.
- **Update** `TestHandle_InvalidatesCacheOnMemberChangeSysMessage` — assert
  `Invalidate` is called but `GetMembers`/`Emit` are **not** (the
  `["inval:r1","get:r1"]` expectation becomes `["inval:r1"]`).
- **Unchanged** — a regular `""` message still invalidates nothing and still
  fans out.

Coverage: `isNotifiable` is exhaustively table-tested; handler tests cover the
gate for each system type and the regular happy path.

## Non-goals

- No `pkg/model` changes, no new constants, no new dependencies.
- Member-cache optimizations are out of scope — tracked separately.

## Docs / scope notes

`notification-worker` is a JetStream consumer, not a client-facing request/reply
or HTTP handler, so **no `docs/client-api.md` change** is required.

## Follow-up — 2026-06-10

As of `claude/reactions-followups`, the durable consumer's `FilterSubjects` is
further narrowed to `{MsgCanonicalCreated}` only. `MsgCanonicalReacted` moved
to `broadcast-worker.handleReacted` so all reaction wire effects (room fan-out
+ author notification) live in one handler with the same wire format. The
in-place `CreateOrUpdateConsumer` pattern from this spec covers the rollout —
NATS 2.10+ applies the narrowed filter without resetting the cursor.
