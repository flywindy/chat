# Chat Frontend — Thread Side-Panel

**Status:** Design proposed, awaiting review
**Date:** 2026-05-13
**Scope:** `chat-frontend` only (no Go service changes)

## Summary

Add a right-hand thread panel to the chat frontend. Users open the panel
via a hover-revealed Thread icon on any message, or via a "{tcount}
replies" badge rendered on parent messages that already have replies.
The panel shows the parent message and its replies (oldest-first) and
lets the user post new replies.

A second hover action, Reply (quote), stages the hovered message as the
`quotedParentMessageId` for the next send. Quote-replies render
in-bubble (one shared message background containing both the quoted
excerpt and the new reply content) and click-jump to the original.

Two more hover actions, Edit and Delete, expose the existing edit /
delete RPCs from a single discoverable menu — usable in the main feed,
on the parent inside the thread panel, and on thread replies.

Layout is restructured: a global `AppHeader` and a left `Sidebar` are
extracted out of today's `ChatPage`, leaving `ChatPage` as the middle
column of a three-column row (sidebar / chat / thread).

## Motivation

Backend support for threads is already in place — `history-service`
exposes `chat.user.{account}.request.room.{roomID}.{siteID}.msg.thread`,
`message-gatekeeper` accepts `threadParentMessageId` +
`threadParentMessageCreatedAt` on `msg.send`, and writes thread replies
into the thread Cassandra tables. The
frontend currently has no UI to open or post in a thread, no UI to
quote-reply, and no Edit/Delete entry points after the layout split —
this PR closes all three gaps.

A side-panel is the right surface for threads because it keeps the
parent's main-feed context visible, matches user expectations from
Slack/Teams, and lets the panel and main feed scroll independently.

## Goals

- Open a thread panel from each message via a hover Thread icon.
- Open a thread panel from a "{tcount} replies" badge on any parent
  message with `tcount > 0` (no `lastReplyAt` timestamp — see Out of
  scope).
- Show parent + replies (oldest-first) loaded via the `msg.thread` RPC.
- Send replies through the existing `msg.send` subject with thread
  parent fields populated.
- Optimistically append the user's own replies; if publish fails, keep
  the row tagged as failed with a ⟳ retry and an ✕ dismiss button.
- **Client-side filter thread replies out of the main feed live
  broadcast.** Today's `broadcast-worker` publishes every thread reply
  on the main `chat.room.{roomID}.event` subject, even though
  `loadHistory` correctly excludes them. We filter in the main reducer
  (`evt.message.threadParentMessageId !== ""` → drop) to prevent
  thread replies popping into the main feed in real time during a
  session.
- Quote-reply from the hover Reply icon. Routing rules:
  - top-level message → stage in main input,
  - reply inside open thread → stage in thread input,
  - parent rendered inside its own open thread → Reply icon hidden.
  (Thread replies never appear in the main feed under the new client
  filter, so the previous "tshow-in-main-feed" case is gone.)
- Render `message.quotedParentMessage` as a `QuotedBlock` inside the
  same bubble as the reply content; click-jump to original via the
  existing `focusMessageId` plumbing; render "[deleted]" placeholder
  when the snapshot indicates the original has been removed.
- Edit and Delete hover actions on own messages, everywhere (main
  feed, parent inside thread panel, thread replies). Inline edit
  (Enter saves, Esc cancels). Delete confirm dialog.
- Cross-context: when the user posts a thread reply, bump the parent's
  `tcount` in the main `roomEventsReducer` so the badge updates
  immediately for the sender. Real-time updates for other users'
  thread replies are deferred (see Non-Goals).
- Refactor the layout so `Sidebar`, `ChatPage`, and `ThreadRightBar`
  are independent columns under a new `MainApp`.
- Keep `MessageList` / `MessageInputForm` reusable across both
  contexts via thin containers.

## Non-Goals

- A "Threads" tab that lists every thread the user participates in
  (would use `history-service` `msg.thread.parent` — explicitly out of
  scope; subject builder is **not** added in this change).
- Pagination beyond the first 50 replies (cursor plumbing is reserved
  but not wired to UI yet).
- Mark-as-read for threads (no read-receipt RPC for threads yet).
- Notifications / unread-count badges on the thread icon.
- Real-time delivery of other users' thread replies into an open
  thread panel — deferred to a separate "thread events" ticket. v1
  loads the thread via the `msg.thread` RPC on open and **optimistically
  appends the local user's own replies** after a successful publish.
  Other users' replies become visible the next time the thread is
  reopened.
- **"Also send to channel" (tshow) checkbox.** The backend doesn't
  carry `tshow` end-to-end today (no field on `SendMessageRequest`,
  not propagated by the gatekeeper, not filtered by the
  broadcast-worker). Wiring it requires changes in three Go services;
  out of scope for this frontend-only PR. A follow-up ticket can add
  the backend plumbing and the checkbox UI together.
- Encrypted-channel thread handling beyond the existing
  "skip-when-empty" path used by the main feed.

## Architecture

### Layout

```
<App>
  <NatsProvider>
    <AppContent>
      ├─ /oidc-callback  → <OidcCallback>
      ├─ !connected      → <LoginPage>
      └─ connected       → <RoomEventsProvider>
                            └─ <ThreadEventsProvider>          NEW
                                 └─ <MainApp>                  NEW
                                      ├─ <AppHeader>           NEW (global bar)
                                      └─ <main-row>            flex row
                                           ├─ <Sidebar>        NEW (room list + create)
                                           ├─ <ChatPage>       slimmed: middle column
                                           └─ <ThreadRightBar> NEW (mounted only when open)
```

- `AppHeader` holds global controls: global search, theme toggle, user
  chip, logout. Room-specific buttons (`Members`, `Leave room`) move out
  of the current `chat-header` into a new strip inside `ChatPage`.
- `Sidebar` is extracted from today's `.chat-sidebar` block in
  `ChatPage` (room list + "+ Create" + dialog mount). It is a fixed-width
  left rail.
- `ChatPage` becomes the middle column only: room-header strip,
  `RoomMessageArea`, `RoomMessageInput`, `InRoomSearch`.
- `ThreadRightBar` is a fixed-width right rail (~360–420 px). It mounts
  only while `activeThreadParent !== null`; when closed, the component
  is unmounted (no `display:none`) so the flex math is clean.
- `InRoomSearch` and `ThreadRightBar` are mutually exclusive right-rail
  occupants. Opening the thread closes in-room search; opening in-room
  search closes the thread.

### State management — new files

`src/lib/threadEventsReducer.js` (mirrors `roomEventsReducer.js` shape,
scoped to one open thread at a time):

```js
initialState = {
  activeParent: null,       // { roomId, siteId, messageId, createdAtMs } | null
  messages: [],             // replies, oldest-first; each may carry { _status: 'failed', _local: true }
  hasLoadedHistory: false,
  historyLoading: false,    // true between OPEN_THREAD and HISTORY_LOADED/HISTORY_FAILED
  historyError: null,
  nextCursor: null,         // reserved for future pagination
  hasNext: false,
}

actions:
  OPEN_THREAD       { parent }                    // short-circuits if activeParent.messageId === parent.messageId
  CLOSE_THREAD      {}
  HISTORY_LOADING   { parentId }                  // sets historyLoading=true
  HISTORY_LOADED    { parentId, resp }            // ignored if parentId !== activeParent.messageId
  HISTORY_FAILED    { parentId, error }
  REPLY_SENT_LOCAL  { message }                   // optimistic append; message has _local: true
  REPLY_SEND_FAILED { messageId, error }          // tag the optimistic message as _status: 'failed'
  REPLY_RETRIED     { messageId }                 // clear failed marker before re-publish attempt
  REPLY_DISMISSED   { messageId }                 // remove a failed _local message permanently
  RESET             {}                            // on logout / provider unmount
```

`_local: true` distinguishes optimistic rows from server-confirmed
rows so a future history reload doesn't accidentally re-send or
confuse them with real messages. `mergeById` from the shared
`pkg/messageBuffer` utility (see below) preserves `_local` markers
when merging.

`appendBounded` and `mergeById` helpers are reused verbatim from the
existing reducer (extract them into a shared module under `src/lib/` if
they aren't already, or duplicate — implementer's call).

`src/context/ThreadEventsContext.jsx`:

- Wraps the reducer.
- On `activeParent` change: dispatch `HISTORY_LOADING` then issue a
  cancellable `request(msgThread(account, roomId, siteId), {
  threadMessageId, limit: 50 })` with the same `cancelledRef` +
  `generationRef` race-discard pattern used in `RoomEventsContext`.
  On success → `HISTORY_LOADED`. On error → `HISTORY_FAILED`.
- **No NATS subscription of its own** in v1. Backend live broadcasts
  for thread replies are deferred (see Non-Goals).
- **No cross-context observer in v1.** Without `tshow` plumbing,
  thread replies don't appear in the main feed live broadcast (the
  client-side filter drops them), and there is no main-feed event the
  thread panel needs to observe. Drop the observer pattern entirely.
- On `sendReply` publish: append the optimistic reply with
  `_local: true` via `REPLY_SENT_LOCAL` immediately (before awaiting
  NATS), then publish. On publish error, dispatch `REPLY_SEND_FAILED
  { messageId }` to tag the row as failed. `retryReply(messageId)`
  clears the failed marker (`REPLY_RETRIED`) and re-publishes.
  `dismissReply(messageId)` removes a failed local reply permanently
  (`REPLY_DISMISSED`).
- On every successful `sendReply` (publish acknowledged with no
  error), the context **also** dispatches `OWN_THREAD_REPLY_SENT
  { roomId, parentId }` into the main `RoomEventsContext` so the
  parent's `tcount` badge updates immediately for the sender. See
  "Cross-context: parent reply-count bumping" below.
- On unmount / `RESET` / `CLOSE_THREAD`: cancel in-flight requests
  and clear all state.
- Exposes:

  ```js
  {
    activeParent, messages, hasLoadedHistory, historyLoading,
    historyError,
    openThread({ roomId, siteId, messageId, createdAtMs }),
    closeThread(),
    sendReply(content, { quotedParentMessageId }),
    retryReply(messageId),
    dismissReply(messageId),
  }
  ```

  `sendReply(content, { quotedParentMessageId })` publishes
  `msgSend(account, roomId, siteId)` with the standard fields plus
  `threadParentMessageId`, `threadParentMessageCreatedAt`, and the
  optional `quotedParentMessageId` when set.

  `ThreadMessageInput` owns one piece of local state above the
  textarea:
  - `quotedTarget: { id, content, sender } | null` — rendered as a
    `QuotedBlock` chip above the textarea when set. Cleared via the
    chip's ✕ button, or on publish success (the chip stays visible
    during in-flight publish so the user can still see what they
    quoted; it disappears only once NATS acks).

  On submit: call `sendReply(content, { quotedParentMessageId:
  quotedTarget?.id })`. Gatekeeper note:
  `message-gatekeeper/handler.go:249` rejects quote-replies whose
  quoted message's `ThreadParentID` differs from the new message's
  `ThreadParentID`. The `MessageActions` routing rule prevents the UI
  from staging a quote that violates this; if a publish nonetheless
  fails (e.g. parent thread deleted out from under us), tag the
  optimistic row as failed (see retry flow).

### Cross-context: parent reply-count bumping

Backend rules (verified):

- `loadHistory` returns only top-level messages — thread replies are
  written by `message-worker` to `messages_by_id` +
  `thread_messages_by_room`, **never to `messages_by_room`**, so the
  history query naturally excludes them
  (`message-worker/handler.go:87-104`, `store_cassandra.go:60-100`).
- `broadcast-worker/handler.go:publishChannelEvent` **publishes every
  message** — including thread replies — to
  `subject.RoomEvent(roomId)` unconditionally. There is no
  thread-parent or tshow check today. **The frontend MUST filter** to
  avoid thread replies flickering into the main feed in real time.
- `msg.thread` and per-message lookups by ID are the only paths that
  intentionally return thread replies.

Given this, the main `roomEventsReducer.js` extensions are:

- **Client-side filter in `MESSAGE_RECEIVED`.** If
  `evt.message.threadParentMessageId !== ""`, skip the append. Thread
  replies do not belong in the main feed buffer. They will surface
  when the user opens the thread.
- **Tcount bump on own thread reply.** New action
  `OWN_THREAD_REPLY_SENT { roomId, parentId }`, dispatched from
  `ThreadEventsContext` after a successful `sendReply`. The handler
  finds the parent in `roomState[roomId].messages` and increments its
  `tcount` by 1. If the parent isn't in the buffer, the action is a
  no-op.
- `HISTORY_LOADED` already returns only main-feed-eligible messages —
  no filtering change. The badge value rendered from `message.tcount`
  is authoritative on every history reload.

Real-time `tcount` updates for **other users'** thread replies are
deferred to the events ticket — there is no client-visible signal
today.

The parent badge does **not** render a "last reply at" timestamp —
backend Message payload doesn't carry one and we explicitly chose to
drop it from v1 (see Out of scope).

### Shared utilities

The existing `roomEventsReducer.js` inlines its dedup logic
(`existingIds` Set inside `HISTORY_LOADED`) and `appendBounded` is
local to it. Extract both into a new shared module:

```
src/lib/messageBuffer.js
  appendBounded(messages, incoming, max)   // identical to today's
  mergeById(current, incoming)             // dedupe by message.id, preserve order, preserve _local markers
```

Both reducers (`roomEventsReducer.js` and `threadEventsReducer.js`)
import from this module. `mergeById` MUST preserve `_local: true` and
`_status: 'failed'` markers when merging — never overwrite them with
fresh server values that lack those fields (since server doesn't know
about them).

### NATS subject builders — additions to `src/lib/subjects.js`

```js
export function msgThread(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.msg.thread`
}
export function msgEdit(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.msg.edit`
}
export function msgDelete(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.msg.delete`
}
```

The edit + delete subjects already exist server-side
(`history-service/internal/service/messages.go:322, 401`) but are
**not** in today's `subjects.js` — adding them is part of this PR.
Reserved for later (not added now): `msgThreadParent`.

### Payload shapes

| Direction | Payload |
|---|---|
| Open thread (request) | `{ threadMessageId: <parentId>, limit: 50 }` |
| Open thread (response) | `{ messages: Message[], nextCursor?, hasNext }` — oldest-first |
| Send reply (publish) | existing `msg.send` payload + `threadParentMessageId` (20-char base62) and `threadParentMessageCreatedAt` (UTC ms). Optional `quotedParentMessageId` when the user staged a quote-reply. **No `tshow` field** — backend doesn't carry it through today; deferred. |
| Main-feed send with quote (publish) | existing `msg.send` payload + `quotedParentMessageId` when a quote-reply was staged in the main input |
| Main-feed broadcast (subscribe) | Backend's `broadcast-worker` publishes every message — including thread replies — to `chat.room.{roomId}.event`. **The frontend filters thread replies out** in `roomEventsReducer.MESSAGE_RECEIVED` (drop when `evt.message.threadParentMessageId !== ""`). |
| Edit message (publish) | `msgEdit(account, roomId, siteId)` — payload shape verified in Red phase against `history-service/internal/service/messages.go:322`. |
| Delete message (publish) | `msgDelete(account, roomId, siteId)` — payload shape verified in Red phase against `history-service/internal/service/messages.go:401`. |

`threadParentMessageCreatedAt` derives from the parent's `createdAt` via
`new Date(parent.createdAt).getTime()`. Gatekeeper rejects either field
missing — never publish without both.

### Component refactor (presentational pattern — Approach A)

Today `MessageArea.jsx` calls `useRoomEvents(roomId)` internally and
`MessageInput.jsx` publishes via `useNats()` internally. Split each:

```
src/components/messages/
  MessageList.jsx          pure: messages, focusMessageId, onJumpToMessage, onAction
  MessageRow.jsx           NEW: single message render — quoted-block bubble, content, MessageActions
  MessageActions.jsx       NEW: hover row of Thread + Reply + Edit + Delete buttons
  MessageInputForm.jsx     pure: value, onChange, onSubmit, quotedTarget, onClearQuote
  QuotedBlock.jsx          NEW: shared "sender + one-line ellipsized content" render,
                                  used both in-bubble (above reply body) and as the staging chip

src/components/
  RoomMessageArea.jsx      container: useRoomEvents(roomId)     → <MessageList/>
  RoomMessageInput.jsx     container: useNats() + msgSend; owns local quotedTarget
  ThreadMessageArea.jsx    container: useThreadEvents()         → <MessageList/>
  ThreadMessageInput.jsx   container: useThreadEvents().sendReply; owns local quotedTarget
  ThreadRightBar.jsx       header strip + ThreadMessageArea + ThreadMessageInput
  AppHeader.jsx            global bar — global search, theme toggle, user chip, logout
  Sidebar.jsx              left rail — RoomList + "+ Create room" at top
  MainApp.jsx              app shell (header + flex row + providers)
```

`MessageList` keeps the existing render and focus-message scroll
behaviour but no longer owns load-history. The owning container drives
that effect with whatever state it has (`hasLoadedHistory`,
`onLoadHistory`).

`MessageInputForm` is the visual form. The container owns submit logic
so the same form can `msgSend` for the main feed or call `sendReply`
for the thread.

CSS lives in `src/styles/index.css` alongside the existing component
rules, using the same CSS-variable tokens already defined under
`src/styles/tokens.css`. New selectors: `.app-header`, `.app-sidebar`,
`.thread-rightbar`, `.thread-header`, `.message-actions`,
`.message-reply-badge`, `.quoted-block`, `.message-failed`. No new
stylesheet files. Thread panel width: **380 px fixed**.

### Hover action menu (`MessageActions`)

Absolutely positioned top-right of each `MessageRow`, revealed on the
row's `:hover` (CSS-only — no JS hover state). Buttons:

| Button | Visibility | Behaviour |
|---|---|---|
| **Thread** | Always | `openThread({ roomId, siteId, messageId, createdAtMs })`. Hidden when the row is the parent rendered inside its own open thread panel (you're already in it). |
| **Reply** (quote) | Conditional, see routing rule below | Stages the hovered message as the quoted target in the appropriate input. |
| **Edit** | Own messages only (`message.sender.account === currentUser.account`) | Enters inline-edit mode on the row: replace content with a textarea (Enter → publish edit RPC, Esc → cancel). Available everywhere: main feed, parent inside thread panel, thread replies. |
| **Delete** | Own messages only | Pops a small "Delete this message?" confirm dialog. On confirm, publish the existing delete RPC. Available everywhere: main feed, parent inside thread panel, thread replies. |

Reply routing rule:

1. Hovered message is **top-level** in main feed
   (`threadParentMessageId` empty): stage in `RoomMessageInput`.
   Gatekeeper allows it (both quote and new message have empty
   thread parent → match).
2. Hovered message is a **thread reply** inside the currently-open
   thread (`threadParentMessageId === activeParent.messageId`): stage
   in `ThreadMessageInput`. Gatekeeper allows it (both share the same
   thread parent → match).
3. Hovered message is the **parent** rendered as the first item in
   its open thread panel: Reply icon **hidden**. Quoting the parent
   from inside its own thread is a boundary mismatch — the new
   message's thread parent is the parent's ID, but the parent itself
   has empty `ThreadParentID` → gatekeeper would reject.

Thread replies never appear in the main feed once the client-side
filter is in place, so the previous "tshow reply in main feed with
thread closed" case is gone.

Staging means: the target input's container holds a `quotedTarget`
piece of state (`{ id, content, sender }`) and renders a `<QuotedBlock>`
chip above the textarea. Submit includes `quotedParentMessageId:
quotedTarget.id`. The chip clears on successful publish or via its ✕
button.

### Quoted-message rendering (`QuotedBlock`)

Single shared component used in two places, both with the same content
shape (sender top-line, single-line ellipsized content beneath):

**As staging chip above an input (`MessageInputForm`):**

```
┌─ (chip background) ────────────────────────┐
│  alice                                     │
│  this is the quoted message excerpt…    ✕  │
└────────────────────────────────────────────┘
```

Row 1: sender. Row 2: content left-justified, one line, ellipsized;
✕ right-justified clears `quotedTarget`. No bubble, no message
background — the chip sits in the input frame.

**As an in-bubble quoted block above a delivered reply (`MessageRow`):**

```
┌─ one message bubble ─────────────────────┐
│  alice                                   │
│  this is the quoted message excerpt…     │
│  ────────────────────────────────────────│
│  bob's reply content goes here,          │
│  spanning as many lines as it needs.     │
└──────────────────────────────────────────┘
```

The quoted block and the reply content share one message background —
they're part of the same bubble. Source: `message.quotedParentMessage`
(snapshot embedded by `message-gatekeeper` —
`pkg/model/message.go:22`).

**Click-to-jump:** clicking the in-bubble quoted block scrolls the main
feed to the original message. Reuses the existing `focusMessageId`
plumbing in `RoomEventsContext`. If the quoted message lives in a
different room or has scrolled out of the buffer beyond what we have
loaded, the click does nothing (no error, just a no-op) — fetching
arbitrary messages by ID for jump-target is deferred.

**Deleted-original handling:** the snapshot stored on
`message.quotedParentMessage` is captured at send time and remains
intact even if the original is later deleted. If the snapshot itself
carries a `deleted: true` flag, render the second line as
`*[message deleted]*` in muted text and disable the click-to-jump.
The Red phase verifies whether the backend exposes such a flag;
if not, render the snapshot as-is and accept that the quote may
display content that has since been removed.

**Content rendering:** the second line is **plain text only** in v1
— no markdown, no mention highlighting, no emoji parsing. One-line
ellipsization via `text-overflow: ellipsis` on a container with
`max-width` set by the bubble layout. Special characters are
HTML-escaped (React default).

### Parent-message reply badge

`MessageRow` renders an inline pill beneath any message with
`tcount > 0`:

```
[💬  {tcount} {tcount === 1 ? 'reply' : 'replies'}]
```

Click → `openThread(...)`. No timestamp — backend Message payload
doesn't carry `lastReplyAt` (the only "last activity" field lives on
`ThreadRoom.LastMsgAt` and isn't fetched by this PR).

### Inline edit / delete UX

- **Inline edit** (Edit hover button): the `MessageRow` swaps its
  content area for a `MessageInputForm` pre-populated with the
  message's content. `Enter` publishes via the existing edit RPC and
  the row optimistically shows the new content; `Esc` cancels and
  restores the original. While editing, the hover menu is hidden on
  that row. Edit applies in the main feed, on the parent inside the
  thread panel, and on thread replies.
- **Delete confirm dialog** (Delete hover button): a small modal —
  "Delete this message? This cannot be undone." with `Delete` /
  `Cancel`. On confirm, publish the existing delete RPC. The local
  row is marked deleted optimistically and re-renders as a "Message
  deleted" placeholder; the authoritative broadcast eventually
  confirms.

The exact edit / delete subject builders and payload shapes are
inherited from today's chat-frontend implementation — the Red phase
verifies them in code, no new subjects added.

### Thread panel layout (`ThreadRightBar`)

```
┌─ "Thread"                                       ✕ ─┐  ← header (close button)
│                                                    │
│  alice  10:42         [hover menu: Edit Delete]    │  ← parent message (rendered
│  the parent message body                           │     by MessageRow; Thread &
│                                                    │     Reply hidden — see actions)
│                                                    │
│  bob    10:44                       [Reply E D]    │  ← reply 1 (oldest first)
│  …                                                 │
│                                                    │
│  alice  10:46  ⟳ ✕  (failed)                       │  ← optimistic failed row
│  …                                                 │     ⟳ retries; ✕ dismisses
│                                                    │
│  ┌──────────────────────────────────────────────┐  │
│  │  ┌── QuotedBlock chip (if staged) ─────┐  ✕  │  │  ← optional staging chip
│  │  │  alice                              │     │  │
│  │  │  the quoted excerpt…                │     │  │
│  │  └─────────────────────────────────────┘     │  │
│  ├──────────────────────────────────────────────┤  │
│  │ Reply…                                       │  │  ← textarea; placeholder "Reply…"
│  └──────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────┘
```

- **No sticky parent.** The parent renders as the first item in the
  scrolling list. Rendered identically to other messages — no
  background tint, no "Parent" label — only the position differs.
- **Empty state** (parent has no replies yet, history loaded): after
  the parent, render `"No replies yet — be the first to reply"` in
  muted text.
- **Loading state** (between `OPEN_THREAD` and `HISTORY_LOADED`):
  render the parent (looked up live from `RoomEventsContext`) plus a
  small "Loading replies…" line in muted text below it. The input
  is interactive throughout — the user can start typing before history
  arrives.
- **Error state** (`HISTORY_FAILED`): render the parent plus a
  "Couldn't load replies." line with a "Try again" button that
  re-dispatches `OPEN_THREAD` with the same parent.
- **Auto-scroll.** On `HISTORY_LOADED`, scroll to the bottom (newest
  reply, or the parent if no replies). On every own optimistic append
  (`REPLY_SENT_LOCAL`), scroll to bottom unconditionally — the user
  just sent it, they expect to see it. "Pinned to bottom" detection
  uses `(scrollHeight - scrollTop - clientHeight) < 20`.
- **Failed-reply UI.** A reply tagged `_status: 'failed'` renders with
  muted-error styling and two buttons: ⟳ (calls `retryReply(messageId)`)
  and ✕ (calls `dismissReply(messageId)`). Failed messages persist
  across the thread session but **are dropped on `CLOSE_THREAD`** —
  closing the panel discards them silently. Closing on logout also
  drops them.
- **Hover actions on the parent** *inside* the thread panel: Thread
  hidden (you're already in it), Reply hidden (parent quote inside its
  own thread is a boundary mismatch), Edit + Delete present when
  parent is own message.

### Loading, error, and empty states (summary)

| Surface | Loading | Error | Empty | Failure |
|---|---|---|---|---|
| `ThreadRightBar` | parent + "Loading replies…" | parent + "Couldn't load replies." + Try-again button | parent + "No replies yet — be the first to reply" | inline failed-row with ⟳ / ✕ |
| `QuotedBlock` (in-bubble) | n/a (snapshot is sync) | n/a | n/a | `*[message deleted]*` if snapshot flags removed; click-to-jump disabled |
| `QuotedBlock` (input chip) | stays visible during in-flight publish | n/a | n/a | n/a |
| Edit inline | textarea + Saving spinner if RPC pending | "Couldn't save edit" toast; row stays in edit mode | n/a | n/a |
| Delete confirm | dialog "Deleting…" while in-flight | "Couldn't delete" toast; dialog stays open | n/a | n/a |

### AppHeader / room-header / Sidebar split

Locked in for v1:

- **AppHeader** (full-width, top): global search, theme toggle, user
  chip, logout button.
- **ChatPage room-header** (strip above `MessageList`): current room
  name, Members button, Leave-room button.
- **Sidebar** (left rail): `RoomList` + `+ Create room` button at the
  top.

Mobile / narrow viewports are explicitly out of scope.

### Keyboard & accessibility

- **Hover-menu reveal.** `MessageActions` is revealed on
  `:hover, :focus-within` of the message row. Each message row is
  focusable (`tabindex="0"`); `Tab` from a row enters the actions,
  `Shift+Tab` returns. This makes the menu reachable without a mouse.
- **Action buttons.** Each button has an `aria-label` ("Reply in
  thread", "Quote this message", "Edit message", "Delete message").
  Disabled buttons set `aria-disabled="true"`.
- **Tab order:** `AppHeader` → `Sidebar` → `ChatPage` (room-header
  → message list → main input) → `ThreadRightBar` (header → thread
  list → thread input). Within the thread panel, `Tab` lands first
  on the close (✕) button, then the message list, then the input.
- **Esc behavior:**
  - In a message row's inline-edit mode → Esc cancels the edit and
    restores the row.
  - In the Delete confirm dialog → Esc dismisses (same as Cancel).
  - In the thread panel (focus anywhere inside, no modal open) → Esc
    closes the thread (`closeThread()`).
- **ARIA live regions.** The thread message list is an
  `aria-live="polite"` region so screen readers announce new
  optimistic replies.
- **Focus management.** Opening the thread moves focus to the
  thread's ✕ close button (escape-friendly). Closing the thread
  returns focus to whichever element triggered the open (the Thread
  hover button or the tcount badge).
- **Color contrast.** All new selectors use existing CSS tokens
  (`src/styles/tokens.css`), inheriting their light/dark contrast
  guarantees.

Out of scope: chord shortcuts (no `r` for reply, no `t` for thread),
mention-keyboard-picker improvements, and full screen-reader audit
beyond the above — defer to a separate a11y pass.

### Edge cases & teardown

- **Room switched while thread open:** if `selectedRoom.id` changes and
  `activeParent.roomId !== selectedRoom.id`, close the thread.
- **Selected room removed (kicked / left):** existing `ChatPage`
  cleanup also dispatches `CLOSE_THREAD`.
- **User logout:** `ThreadEventsContext` watches `user` from
  `useNats()`; when it goes null, dispatch `RESET` and cancel any
  in-flight RPC.
- **Stale request races:** same `generationRef` pattern as
  `RoomEventsContext` — drop responses from prior generations.
- **Duplicate replies:** thread reducer dedupes by `message.id` via the
  same `mergeById` semantics as the main feed (`REPLY_SENT_LOCAL`
  goes through the same merge so a duplicate cannot appear if a
  future events-ticket ever delivers the same reply back over a
  subscription).
- **Empty / encrypted broadcasts:** main reducer keeps the existing
  "skip when `evt.message` is empty" path; no thread-specific change.
- **InRoomSearch ↔ ThreadRightBar:** mutual exclusion enforced in
  `ChatPage` — opening one calls the closer of the other. Pressing
  Ctrl-F while a thread is open closes the thread first.
- **Reply broadcast arrives for a closed thread:** the main reducer's
  client-side filter drops it (`threadParentMessageId !== ""`). The
  parent's `tcount` does NOT bump in real time for other users'
  replies — the next history reload is authoritative.
- **Parent message edited / deleted while thread is open:** the thread
  panel renders the parent by looking it up by ID in the main
  `RoomEventsContext` buffer (live), and falls back to the
  `activeParent` snapshot only when the parent has scrolled out of
  the buffer. Existing `MESSAGE_EDITED` / `MESSAGE_DELETED` handlers in
  the main reducer keep the parent up to date; no extra wiring needed.
- **Re-clicking Thread on the already-open parent:** the
  `OPEN_THREAD` reducer branch detects `activeParent.messageId ===
  parent.messageId` and short-circuits — no new RPC, no buffer reset.
- **ChatPage ↔ ThreadEventsContext coupling:** `ChatPage` watches
  `useThreadEvents().activeParent` via `useEffect` and closes its
  in-room search panel when the value transitions from `null` →
  non-null. The opposite direction (opening in-room search) calls
  `closeThread()` from `useThreadEvents()`.
- **Edit / delete of own thread replies:** the Edit/Delete hover
  actions work the same way on thread replies as on main-feed messages.
  Other users' edits/deletes don't propagate into the open thread panel
  in v1 (no live subscription); deferred to the events ticket.
- **Send failure (publish error):** the optimistic reply remains in
  the thread list, tagged `_status: 'failed' _local: true`, with ⟳
  retry and ✕ dismiss buttons. Retry clears the marker and
  re-publishes. Dismiss removes the row permanently. Closing the
  thread silently drops any remaining failed messages.
- **Repeated failure:** retry that fails again re-applies the failed
  marker. The most recent error is shown; there's no error history.
- **`_local` markers in `mergeById`:** any reducer dispatch that
  merges with `mergeById` MUST preserve `_local` and `_status` on
  existing rows. The server can't see those markers, so the merge
  logic checks the existing row's flags before overwriting.
- **Cross-room thread state:** room switch dispatches
  `CLOSE_THREAD`, which clears thread state including failed
  optimistic rows. Switching back does NOT restore a previously-open
  thread; the user re-opens it manually.
- **Cross-room main-input quote:** when the selected room changes,
  the `RoomMessageInput` container clears its local `quotedTarget`
  state (the quote target was tied to the previous room).

## Testing strategy

TDD per `CLAUDE.md`: Red tests for every new module before the
implementation. Coverage targets ≥ 80 % overall, ≥ 90 % on contexts and
reducers.

| New file | Tests |
|---|---|
| `lib/messageBuffer.js` | `appendBounded` shrinks beyond max; `mergeById` dedupes by id, preserves order, **preserves `_local` and `_status` markers** when merging server-side rows over local ones |
| `lib/threadEventsReducer.js` | every action; race-discard via generation/parentId mismatch; `mergeById` dedupe; `REPLY_SENT_LOCAL` appends with `_local: true`; `REPLY_SEND_FAILED` tags `_status='failed'`; `REPLY_RETRIED` clears it; `REPLY_DISMISSED` removes the row entirely; `OPEN_THREAD` short-circuits on same parent; `HISTORY_LOADING` flips the flag; `RESET` clears everything including failed locals |
| `context/ThreadEventsContext.jsx` | RPC fires once per `openThread`; same-parent re-open short-circuits; cancels on close / unmount / logout; `sendReply` optimistically appends, then publishes, then tags on error; `retryReply` re-publishes; `dismissReply` removes the row; on publish success, dispatches `OWN_THREAD_REPLY_SENT` into `RoomEventsContext` |
| `lib/roomEventsReducer.js` | extended — `MESSAGE_RECEIVED` **drops messages with `threadParentMessageId !== ""`** (client-side filter); new `OWN_THREAD_REPLY_SENT` action increments `tcount` on the parent, no-op when parent isn't buffered; existing tests for top-level messages unchanged |
| `components/messages/MessageList.jsx` | pure render; reply-count badge renders only when `tcount > 0`; badge click fires `onOpenThread`; loading / error / empty-state placeholders rendered when caller passes the corresponding props |
| `components/messages/MessageRow.jsx` | renders `QuotedBlock` when `message.quotedParentMessage` set; in-bubble quote click fires `onJumpToMessage(originalId)`; failed-status row shows ⟳ + ✕ with correct handlers; inline-edit mode swap on Edit action; `tabindex="0"` and `:focus-within` reveals actions |
| `components/messages/MessageActions.jsx` | reveal on hover OR focus-within; Thread always shown except on parent-in-its-own-thread; Reply routing rule cases 1–3 (case 3 hidden); Edit/Delete only on own messages; `aria-label` + `aria-disabled` on every button |
| `components/messages/QuotedBlock.jsx` | renders sender + one-line ellipsized plain-text content; chip variant exposes `onClear` (✕); bubble variant exposes `onClick` for jump-to-original; deleted-snapshot renders `*[message deleted]*` and disables click |
| `components/messages/MessageInputForm.jsx` | controlled form; submit on Enter; Shift+Enter newline; quote-chip rendered when `quotedTarget` set and stays during in-flight publish; disabled state |
| `components/RoomMessageArea.jsx` / `RoomMessageInput.jsx` | wired to `RoomEventsContext` / `useNats()`; quote chip cleared on send success; main-input `quotedTarget` cleared on selected-room change |
| `components/ThreadMessageArea.jsx` / `ThreadMessageInput.jsx` | wired to `ThreadEventsContext`; `quotedParentMessageId` passed through to `sendReply`; failed reply shows ⟳ + ✕; ⟳ triggers `retryReply`, ✕ triggers `dismissReply`; aria-live region on the list |
| `components/ThreadRightBar.jsx` | header ✕ dispatches `CLOSE_THREAD`; opens focus on ✕; restores focus on close; unmounts entirely when no active parent; mutual exclusion with `InRoomSearch`; Esc closes thread; auto-scroll on `HISTORY_LOADED` and on every `REPLY_SENT_LOCAL` |
| `components/AppHeader.jsx`, `Sidebar.jsx`, `MainApp.jsx` | render-and-wire: AppHeader holds search/theme/user/logout only; Sidebar holds RoomList + Create at top; MainApp wires the provider stack and conditional `ThreadRightBar` mount |
| `pages/ChatPage.jsx` | room-header strip renders room name + Members + Leave; room-switch closes thread; logout resets state; opening in-room search closes thread and vice versa; existing tests adapt to slimmed layout |
| `lib/subjects.js` | new `msgThread` / `msgEdit` / `msgDelete` builders return the documented strings |
| Inline edit / delete | per-row: Edit swaps row → input; Enter publishes via `msgEdit` and exits edit mode; Esc cancels; Delete opens confirm dialog; confirm publishes via `msgDelete` and renders deleted-placeholder optimistically; Esc dismisses dialog |

Existing `MessageArea.test.jsx` and `MessageInput.test.jsx` are split
across the new presentational + container test files. No backend Go
tests — no backend changes.

## Out of scope / future work

- "Threads" tab listing all threads (would consume
  `chat.user.{account}.request.msg.thread.parent`).
- Reply pagination beyond the initial 50.
- Mark-as-read for threads + unread badges.
- Thread notifications (per-mention / per-subscription).
- Real-time delivery of thread replies from other users into an open
  thread panel — own ticket, owns the broadcast subject / consumer.
- Edits and deletes of other users' thread replies propagating into an
  open thread panel — paired with the events ticket.
- "Last reply at" timestamp on the reply-count badge — the field
  isn't on the Message payload today; would require a per-room
  ThreadRoom hydration RPC. Not in v1.
- Jump-to-original for quoted messages that are out of the loaded
  buffer or in another room — needs a per-message lookup fallback;
  out of v1 (no-op for now).
- **"Also send to channel" / `tshow` checkbox.** Needs backend work
  in `pkg/model` (add field to `SendMessageRequest`), gatekeeper
  (propagate), `message-worker` (dual-write `messages_by_room` when
  `tshow`), and `broadcast-worker` (filter live broadcast unless top-
  level or `tshow`). Spec out separately.
- **Broadcast-worker thread-reply filter (server-side).** Verified
  pipeline: client → gatekeeper → `MESSAGES_CANONICAL` stream →
  `broadcast-worker` consumes directly (no intermediate FANOUT
  despite the README), `publishChannelEvent` / `publishDMEvents` have
  no thread-parent check. The right architectural fix is an early
  return in `broadcast-worker/handler.go` when
  `msg.ThreadParentMessageID != ""`. **Deliberately deferred** — for
  v1 we filter client-side in `roomEventsReducer.MESSAGE_RECEIVED`
  instead. Worth pairing with the `tshow` ticket since the same
  function is touched.

## Open questions

None — all decisions captured above. Red-phase verification items the
implementer should confirm in code before writing implementation:

1. Exact request/response payload shapes for `msg.edit` and
   `msg.delete` (read `history-service/internal/service/messages.go:322,
   401`); reuse the matching DTOs from `pkg/model`.
2. Whether `message.quotedParentMessage` snapshot carries a `deleted`
   flag (or similar) that the `QuotedBlock` should use to render the
   "[message deleted]" placeholder. If not, render as-is.
3. Whether `broadcast-worker` already filters anything for DM rooms vs
   channel rooms that affects how the new thread-reply client filter
   should behave. (Spot-check `publishDMEvents` vs
   `publishChannelEvent`.)
