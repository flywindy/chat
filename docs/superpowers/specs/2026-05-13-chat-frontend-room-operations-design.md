# Chat-Frontend Room Operations Overhaul — Design

**Date:** 2026-05-13
**Status:** Implemented (retrospective)
**Scope:** `chat-frontend/` only — no backend changes
**Related specs:**
- `2026-04-13-chat-frontend-design.md` (NATS-from-the-browser baseline)
- `2026-04-21-room-member-management-frontend-design.md` (the manage-members flow this work rebuilds end-to-end)
- `2026-04-28-create-room-design.md` (server-side `room.create` payload contract)
- `2026-04-20-room-members-list-design.md` (`member.list?enrich`)
- `2026-05-05-create-room-v2-cleanups.md` (DM dedup sync-reply shape)

## Summary

Brings the chat-frontend in line with the room-service async-job protocol that's been live on the backend since `2026-04-28-create-room-design.md`. Rebuilds the create-room and manage-members flows end-to-end: correct canonical subjects, correct payload shapes, two-phase reply handling, a single chip-input picker that accepts comma-separated values, a member roster with role-aware controls, a per-room badge that opens the dialog, DM name fallbacks, encryption-resilient broadcast rendering, and a dialog that parks open until the server's `subscription.update` event has landed (so the user's first message in a freshly-created channel actually echoes back instead of being lost to a subscription race).

Implemented entirely in `chat-frontend/` — no backend changes.

## Problem

Four user-visible gaps left the chat-frontend unable to complete the
create-room or manage-members flows correctly:

### 1. CreateRoomDialog published to a non-existent subject

```js
// old
publish(`chat.user.${account}.request.rooms.create`, {
  type: 'group',
  members: 'alice,bob',  // comma-separated string
})
```

Server expects `chat.user.{account}.request.room.{siteID}.create`
(note: `room` singular + `siteID` segment) with payload
`{name, users, orgs, channels}` and infers the room type server-side
from payload shape (`name` present → channel; empty name + one user
→ DM/botDM). The dialog's request never matched any handler. Rooms
appeared to "create" only because of an optimistic local sidebar
update; the actual server state was never written.

### 2. Member-management flows didn't await the async result

Room-service's mutating RPCs (`room.create`, `member.add`,
`member.remove`, `member.role-update`) follow a **two-phase reply**
contract:

1. **Sync reply** on the request's inbox: typically
   `{status: "accepted"}` (or `{error}` for synchronous rejects
   like "DM already exists").
2. **Async result**: an `AsyncJobResult` published to
   `chat.user.{account}.response.{requestID}` once the underlying
   worker finishes (member fan-out, federation outbox, Mongo writes).
   The worker keys the response subject off the `X-Request-ID` header
   on the request.

The old frontend treated `nc.request()` as one-shot — it took the
sync `accepted` as success and never observed the async result. Real
failures (worker rejected the role update, member-add hit an
authorization check, federation outbox failed) silently produced no
UI feedback.

### 3. Member payloads sent the wrong shape

`AddMembersForm` used a comma-separated string-of-strings for channels:

```js
{ channels: 'r1,r2,r3' }  // server expects ChannelRef[]
```

Server's add-member handler decodes `channels` as
`[]model.ChannelRef = [{roomId, siteId}]`. The string flavor parsed
to an empty slice, so cross-site channel adds silently no-op'd.

### 4. ManageMembers UI was four typed-text tabs

Add / Remove / Role / Remove-Org — each its own form that typed
account IDs by hand. No roster view, no surfacing of who actually
was in the room. The user had to know the account ID of every
person they wanted to touch.

### 5. Live channel broadcasts were silently dropped when encrypted

`broadcast-worker` emits channel events in one of two shapes depending
on its `BROADCAST_ENCRYPTION_ENABLED` env:

- **Plaintext**: `evt.message` populated with `ClientMessage`
- **Encrypted**: `evt.message` dropped via `json:omitempty`;
  `evt.encryptedMessage` holds the AEAD ciphertext

The reducer's `MESSAGE_RECEIVED` previously silently dropped the
encrypted case at `if (!evt.message || !evt.message.id) return state`.
That left the room visually frozen — the user sent a message, the
server persisted and broadcast it, the frontend received the event,
the reducer dropped it, the user saw nothing on their own screen, and
they had to refresh to load the message via the Cassandra-backed
history RPC. The drop was easy to miss because no log line surfaced.

### 6. CreateRoomDialog closed before subscription.update landed

`ChatPage` has an effect that auto-deselects `selectedRoom` when it
isn't in `summaries`. After Create:

```text
t=0   CreateRoomDialog → sync ack {roomId} → onCreated → handleSelectRoom
t=0+  ChatPage auto-deselect effect runs: room not in summaries yet → setSelectedRoom(null)
t=Δ   subscription.update arrives → ROOM_ADDED + openChannelSub → too late
```

So the user was bounced back to the empty state. Worse, any message
they typed in the gap was published to a room whose
`chat.room.{roomId}.event` subject the frontend hadn't yet subscribed
to — the message arrived at the server, was persisted, was broadcast,
and the broadcast missed the frontend entirely.

### 7. DM rooms displayed nothing in the sidebar

`Room.Name` is empty server-side for DMs (the friendly text lives on
the per-user `Subscription.Name`, not the `Room`). The sidebar / header
rendered an empty span where the room name should have been.

### 8. Members + Leave buttons looked like chat-wide controls

The two per-room actions sat in the global chat-header between the
search bar and the user info, alongside theme + logout. They read as
if they applied to the whole chat, not the current room.

## Goals

- **Correct wire format.** Every mutating chat-frontend call lands on
  the canonical room-service subject with the server-expected payload
  shape. Removes the orphan `roomsCreate` builder.
- **Two-phase reply support.** Mutating components await the
  `AsyncJobResult` and surface its `status: ok|error` to the user.
  Error messages distinguish wire-level failures from server-reported
  ones so a transient WebSocket drop reads differently than "not an
  admin".
- **Single picker for users / orgs / channels.** One reusable
  `MemberPicker` used by both `CreateRoomDialog` and `AddMembersForm`,
  with channel typeahead via `search.rooms`. Eliminates the
  "type-the-account-ID-by-hand" UX.
- **Free-text comma-separated entry.** Users / orgs have no
  server-side search endpoint yet; the picker must let users paste
  `"alice, bob, charlie"` and submit without remembering to press
  Enter on each chip.
- **Roster view for manage-members.** Replace the four typed-text
  tabs with a `Members` tab listing current members and an `Add` tab.
  Inline Promote / Demote / Remove buttons per row, gated on the
  current user's owner status. Self-row gets `Leave` instead of
  `Remove` (you can't kick yourself; you can leave).
- **Encryption resilience.** The reducer must produce a visible row
  for encrypted broadcasts so the room never looks frozen, even
  without client-side crypto.
- **Subscription-update wait on create.** Dialog parks open until
  the server confirms via `subscription.update` (the same event lane
  that opens the channel sub). The user's first message after Create
  echoes back live.
- **DM name fallback.** Sidebar / header show *something* for DM
  rooms — prefer the subscription's per-user friendly name; otherwise
  a `"(DM)"` placeholder.
- **Per-room affordances on the room itself.** "N members" badge in
  the MessageArea header, not in the global header.

## Non-Goals

- **Server-side changes.** Subjects, payload shapes, the two-phase
  reply contract, broadcast encryption, and the absence of
  user/org search endpoints are all owned by `room-service` /
  `room-worker` / `broadcast-worker`. This work only catches the
  frontend up to what those services already do.
- **User and org search endpoints.** Channel typeahead is wired
  (`search.rooms` already exists); users and orgs stay free-text /
  comma-separated until matching server endpoints land.
- **Client-side decryption.** Encrypted broadcast events render as
  `"[encrypted message]"` placeholders. Real decryption requires
  client-side key management and is a separate effort.
- **Optimistic UI with rollback.** All mutations show a spinner /
  "Waiting…" label until confirmation. No optimistic state followed
  by a rollback dance — too racy with multiple actions in flight.
- **Generic async-job framework.** `requestWithAsyncResult` is one
  function, not a hook + reducer + cancellation-token system. The
  contract is narrow (sync ack + one async result) and intentionally
  doesn't grow.
- **Pagination / virtualization of the roster.** `member.list?enrich`
  returns the full set today. If room sizes grow past the point where
  a flat list is reasonable, that's a follow-up.
- **Eager channel subscription on selection.** Considered as an
  alternative to the wait-for-subscription-update pattern (see
  Trade-offs), but the wait approach was simpler and correct.

## Design

The work is layered by component / responsibility. Each layer is one
or more commits; the commit boundaries reflect logical groupings, not
the chronological order in which the work shipped.

### Layer 1 — `lib/asyncJob.js` (foundation)

Single exported function plus an error-kinds enum:

```js
export const ASYNC_JOB_ERROR_KINDS = Object.freeze({
  SyncError: 'sync-error',                   // sync reply timed out or carried {error}
  AsyncError: 'async-error',                 // AsyncJobResult arrived with status:'error'
  AsyncTimeout: 'async-timeout',             // no AsyncJobResult within asyncTimeout ms
  SubscriptionClosed: 'subscription-closed', // response-subject sub closed mid-wait
})

export async function requestWithAsyncResult(nc, account, subject, payload, opts) {
  // → { requestId, sync, async }
}
```

**Three invariants the helper enforces:**

1. **Subscribe-before-publish.** The response subject
   (`chat.user.{account}.response.{requestID}`) is subscribed *before*
   the request goes out. A fast worker that publishes its
   `AsyncJobResult` between the request being acked and the client
   starting to listen would otherwise drop the result on the floor.
2. **`X-Request-ID` header on the request.** `room-worker` reads this
   header and publishes its `AsyncJobResult` to
   `chat.user.{requesterAccount}.response.{headerValue}`. No header =
   no async result. The helper sets it from the `requestId` opt (or
   mints a fresh UUIDv7).
3. **Tagged errors.** Every error thrown carries `.kind` from
   `ASYNC_JOB_ERROR_KINDS` so callers can branch on category without
   string-matching the message. A companion `formatAsyncJobError(err)`
   produces user-facing text: server-supplied messages pass through
   verbatim (already sanitized at service boundaries per the Go
   error-handling rules); wire-level failures get a friendlier hint
   ("connection interrupted before the server confirmed").

**`treatAsSuccess` for sync-error-but-actually-success replies.** The
DM-create handler returns `{error: "dm already exists", roomId:
existingId}` when the DM is already there. Wire-level this is a
sync-error reply, but semantically it's the success the caller wants
(open the existing room). Callers opt in:

```js
await requestWithAsyncResult(roomCreate(...), payload, {
  treatAsSuccess: isDMExistsReply,
})
```

`isDMExistsReply` is co-located with `ERR_DM_ALREADY_EXISTS` in
`lib/constants.js` — same module that owns the wire constant.

### Layer 2 — `NatsContext` integration

The provider exposes `requestWithAsyncResult(subject, payload, opts)`
that injects `ncRef.current` and `user.account` so components don't
thread the connection through props:

```jsx
const { requestWithAsyncResult } = useNats()
const { sync, async } = await requestWithAsyncResult(subject, payload, opts)
```

Two collateral fixes in the same provider:

- **`useMemo` on the context value.** Consumers that only read stable
  callbacks re-rendered on every provider render because the value
  object identity flipped. Now the identity flips only when
  `connected | user | error` change.
- **JSDoc on every callback.** `subscribe` is flagged: the returned
  subscription must be `.unsubscribe()`'d on cleanup or the iterator
  + server-side sid leak.

### Layer 3 — `MemberPicker` (the shared chip input)

One component, three fields, one config. The same `EntityField`
renders all three (users, orgs, channels):

```jsx
<EntityField
  parseEntries={...}      // text → Entry[]  (comma-split, trimmed, deduped)
  entryKey={...}          // entry → stable key for dedup
  entryLabel={...}        // entry → chip text
  searchFetcher={...}     // optional: q → Promise<Result[]>
  renderResult={...}      // result → dropdown row JSX
  entryFromResult={...}   // result → entry (after dropdown pick)
  resultKey={...}         // result → React key for the dropdown row
/>
```

Output shapes match the server contract:

| Field | Type | Notes |
|---|---|---|
| `users` | `string[]` | Account IDs. Free-text + comma-separated only — no user-search endpoint yet. |
| `orgs` | `string[]` | Org IDs. Free-text + comma-separated only. |
| `channels` | `ChannelRef[]` = `[{roomId, siteId}]` | Typeahead via `search.rooms`; free-text commit assumes local site. |

**Comma-separated parsing.** `parseCommaList(text)` splits on `,`,
trims each segment, drops empty. Single-value entry still works
(`parseCommaList("alice")` returns `["alice"]`). The Enter handler
takes the parsed array and merges into existing chips with dedup via
`entryKey`. Same logic powers the flush path (next section).

**Imperative API: `flushAndGetEntries()`.** `MemberPicker` is a
`forwardRef` component exposing this method via
`useImperativeHandle`. `CreateRoomDialog` and `AddMembersForm` call
it at the top of their submit handlers:

```jsx
const { users, orgs, channels } =
  pickerRef.current?.flushAndGetEntries() ?? { users: stateUsers, orgs: stateOrgs, channels: stateChannels }
```

The method takes whatever text is in each field right now, comma-
splits + dedups it against the current entries, calls `onChange` for
the host's state, clears the inputs, and **returns the merged arrays
synchronously** so the caller can use them for the in-flight submit
without waiting for React to re-render. This is what makes
`type "alice, bob" + click Create` work without pressing Enter first.

**Module-scope spec functions.** `parseStringEntries`, `identity`,
`renderChannelResult`, `channelEntryKey`, `channelEntryLabel`, etc.
are hoisted out of the component so `EntityField`'s prop identities
are stable across parent renders. The one exception is the channel
`parseEntries`, which captures `user.siteId` and stays inline.

**`useDebouncedSearch` companion hook.** The picker uses
`useDebouncedSearch({delay, minLen, fetcher})` for query→results. The
hook's `fetcher` is optional: when null/undefined it's a controlled
input tracker only — `query` updates but no timer fires and `results`
stays `[]`. This is what makes the users/orgs fields work without a
search endpoint without forcing every field to special-case the no-
search path.

### Layer 4 — `CreateRoomDialog` rewire + subscription-update wait

```jsx
const { sync } = await requestWithAsyncResult(
  roomCreate(user.account, user.siteId),
  { name: trimmedName, users, orgs, channels },
  { treatAsSuccess: isDMExistsReply },
)
const roomId = sync.roomId
const roomType = sync.roomType || (isDMExistsReply(sync) ? 'dm' : undefined)
const displayName = trimmedName || users[0] || ''
setPendingRoom({ id: roomId, type: roomType, siteId: user.siteId, name: displayName })
// Dialog stays open with "Waiting for server confirmation…" until
// `subscription.update` arrives, ROOM_ADDED dispatches, openChannelSub
// fires for channels, and `summaries` contains the new roomId.
```

**Behaviors worth flagging:**

- **No type dropdown.** Type is inferred server-side from payload
  shape. The dialog enforces only "name present OR at least one
  picker entry / pending text".
- **DM dedup branch.** When `treatAsSuccess(sync)` returns true, the
  helper returns `{sync, async: null}` instead of throwing. The
  dialog reads `sync.roomId` and opens the existing room — the user
  can't tell whether they created or rejoined, which is the desired
  UX.
- **Display-name fallback to `users[0]`.** DMs and botDMs have empty
  `name` server-side. Until `subscription.update` lands with the
  counterpart-derived display name, the sidebar uses the first user
  account as a placeholder.
- **Submit button always clickable.** The old `name || chips > 0`
  gate left users staring at a greyed-out Create button when they
  had typed text without pressing Enter. The button now disables only
  while loading; the handler runs the actual empty-check after
  flushing.
- **Wait for `subscription.update`.** Two `useEffect`s drive the
  resolution path:
  1. Watches `summaries` from `useRoomSummaries`. When it contains
     `pendingRoom.id`, fires `onCreated(pendingRoom)` + `onClose()`.
  2. Arms a **3-second safety `setTimeout`**. On timeout the dialog
     does **not** auto-select the would-be room (calling `onCreated`
     in that state would set `selectedRoom` to a room missing from
     `summaries`, and ChatPage's auto-deselect effect would bounce
     the user straight back to the empty state; the channel sub
     isn't open either, so messages wouldn't echo). Instead the
     timeout surfaces an in-dialog error
     (`"Room creation is taking longer than expected. If it
     succeeds, the room will appear in your sidebar shortly — you
     can dismiss this dialog."`) and clears `pendingRoom`. The user
     dismisses with Cancel, and the room — if it was actually
     created — appears in their sidebar when `subscription.update`
     finally lands.

  `loading` (sync request in flight) and `pendingRoom` (waiting for
  `subscription.update`) are tracked separately so **Cancel is
  enabled during the wait**. Submit stays disabled the whole time
  so a second click can't fire a duplicate create.

  Button label flips through `Create → Creating… → Waiting for server confirmation… → (dialog closes via summaries-match) | error banner (via timeout)`.

`lib/subjects.js` drops the now-orphan `roomsCreate` builder and the
comma-separated-list `parseList` helper that supported the old
input shape.

### Layer 5 — `ManageMembersDialog` redesign

Four typed-text tabs collapse to two:

| Old | New |
|---|---|
| Add | Add (rewritten to use `MemberPicker` + `requestWithAsyncResult`) |
| Remove | — (replaced by Members tab's inline Remove) |
| Role | — (replaced by Members tab's inline Promote/Demote) |
| Remove Org | — (replaced by Members tab's org rows + per-row Remove) |

#### `MemberRoster` — Members tab body

Fetches `member.list?enrich=true` on mount and on every successful
action. The enriched response includes `engName` / `chineseName` /
`sectName` / `isOwner` so the roster doesn't need a second lookup.

**Single ordered list, orgs first then individuals.** No subheaders;
the row layout itself communicates the type via what it shows:

| Row type | Layout |
|---|---|
| **Org** | `sectName · {memberCount} members · [Remove (owner)]` |
| **Individual** | `engName · chineseName · [owner badge if isOwner] · [Promote/Demote (owner) + Remove (owner)] OR [Leave (self)]` |

**Self-row → `Leave`.** Kicking or demoting yourself from inside the
dialog is a UX trap (you lose admin mid-flow, or you fall through
the dialog mid-leave). The self row only ever offers `Leave`, with a
`window.confirm` first. `runAction` doesn't refetch after a
successful self-remove — the user is no longer a member; the dialog
dismisses on its own via `ChatPage`'s `selectedRoom/summaries`
effect once `subscription.update` lands.

**Owner-only controls.** `isCurrentUserOwner` is derived from the
just-fetched roster (find your own row, read `isOwner`). Promote /
Demote / Remove on other rows render only when this is true. Server
still enforces auth; this is UI parity so non-owners aren't shown
buttons that will reliably fail.

**Per-row layout uses two flex areas (`.roster-row-info` +
`.roster-row-actions`) with `justify-content: space-between` on
`.roster-row`.** The info area uses `gap: 0.75rem` so name + secondary
+ owner badge never sit flush against each other, regardless of
whether the actions area has buttons or is empty. The same spacing
applies for owner viewers (who see buttons) and non-owner viewers
(who don't).

**Individual rows show `engName + chineseName`**, not `engName +
account`. The user already knows who they're managing by name; the
chineseName is a humane secondary identifier. Falls back gracefully:
primary uses `entry.engName || entry.account`; secondary span is
omitted entirely when `chineseName` is missing.

**No Remove-by-ID inputs.** Every row already has its own Remove
button. Removed the legacy escape-hatch text inputs (for accounts
not in the enriched list) along with their state and tests.

**`runAction` returns `Promise<boolean>`.** Action handlers need to
perform side effects on success only (e.g. clearing an input — back
when those existed). The boolean return makes this explicit:

```jsx
const ok = await runAction(key, subject, payload)
if (ok) clearInputOnSuccess()
```

A naive `.then(clearInput)` would fire on failures too because
`runAction` catches its own rejections and surfaces them via
`actionError`.

**`busyKey` state for per-row disable.** A single string
(`"promote:alice"`, `"remove:bob"`, `"leave:alice"`) marks exactly
one button as busy at a time. Prevents stacking conflicting actions.

**Inline org expansion.** Clicking an org row's name toggles a
nested list of the org's members inline beneath the row. The
chevron prefix (`▸` / `▾`) and `aria-expanded` on the toggle button
make the state obvious for both pointer and keyboard users. The
expansion data comes from the existing one-phase RPC
`chat.user.{a}.request.orgs.{orgId}.members`
(`subject.orgMembers(account, orgId)` in `lib/subjects.js`) whose
response shape is `{members: [{id, account, engName, chineseName,
siteId}]}`. Three component-state maps key the behavior:

  | Map | Keyed by | Purpose |
  |---|---|---|
  | `expandedOrgs` | `orgId` | UI open/closed flag |
  | `orgChildren` | `orgId` | Cached `members[]` payload — collapse + re-expand reuses it instead of refetching |
  | `orgFetchState` | `orgId` | `'loading'` while in flight, `'error'` on failure; absent on success |

Child rows render `engName` (primary) + `chineseName` (secondary)
with **no action buttons** — the parent org row already owns the
Remove control, and the server treats the org as a single
membership unit. A `Loading members…` row appears while the fetch
is in flight; a `Failed to load members.` row appears on RPC
failure. Non-owner viewers can still expand orgs (the Remove
button is gated separately).

**Generation-counter guards for async writes.** Both the
roster-wide `fetchMembers` and the per-org `toggleOrg` fetches use
generation refs (`memberListGenRef`, `orgFetchGenRef.current[orgId]`)
to drop stale resolutions. The pattern: each invocation bumps the
gen at entry and captures it locally; on resolve it checks
`gen !== ref.current` and bails out if a newer invocation has
overtaken it. This matters because:

  - **Room switch mid-fetch** — moving from one room to another
    on a slow connection used to let the old room's `member.list?enrich`
    response overwrite the new room's members. The
    `memberListGenRef` guard drops the stale write.
  - **Rapid expand → collapse → re-expand of the same org** —
    without the per-org gen, the first fetch's response could
    overwrite the cache set by a later re-expand. The
    `orgFetchGenRef` guard keeps only the latest write.

#### `AddMembersForm`

Same `MemberPicker` instance as `CreateRoomDialog`, plus a
`shareHistory` checkbox that maps to `history: {mode: 'all'|'none'}`
on the wire. Sends `ChannelRef[]` for channels (fixes the silent
no-op bug from the problem statement).

**Deleted files.** `RemoveMemberForm.jsx`, `RoleUpdateForm.jsx`,
`RemoveOrgForm.jsx`, and their tests are gone — fully subsumed by
the roster.

### Layer 6 — `LeaveRoomButton` deleted; Leave lives only in the roster

The old standalone `LeaveRoomButton` in the chat-header was both
redundant (the roster's self-row Leave does the same thing) and a
duplicate code path (it used `request` — one-phase RPC — while the
roster's Leave uses `requestWithAsyncResult`, the right contract).

`LeaveRoomButton.jsx` and its test are removed. Leave is reachable
exclusively via the `N members` badge → `Manage Members` dialog →
own row → `Leave`. Single discoverable path.

### Layer 7 — Per-room "N members" badge in MessageArea header

`RoomMembersBadge` fetches `member.list` on mount and renders
`N member(s)` as a clickable pill. Refetches when `room.id` /
`room.siteId` change and when a parent-controlled `refreshKey` bumps.
Hides itself entirely for non-channel rooms (DMs have fixed
membership — nothing to manage).

The badge sits **inside `MessageArea`'s header**, replacing the
former static `<span className="message-area-members">{userCount} members</span>`.
It's a per-room control, so it lives on the room it acts on — the
global header carries only chat-wide actions (search, theme, logout).

`ChatPage` bumps a `membersRefreshKey` whenever the
`ManageMembersDialog` closes, so a member-add/remove inside the
dialog is reflected in the badge count immediately on dismiss.

`MessageInput`'s placeholder uses the same
`roomPrefix(room.type) + roomDisplayName(room)` helpers as
`RoomList` and the MessageArea header, so a channel shows
`Message # frontend` and a DM shows `Message @ bob` (falling back
to `Message @ (DM)` if the subscription name hasn't landed yet) —
never the misleading `Message #bob-dm` that the hardcoded
`#${room.name}` placeholder produced.

### Layer 8 — DM display name fallback

`Room.Name` is empty server-side for DMs (the friendly text lives on
`Subscription.Name`, per-user). The frontend now routes the displayed
name through `roomDisplayName(room)` in `lib/roomFormat.js`:

```js
export function roomDisplayName(room) {
  if (!room) return ''
  if (room.name) return room.name
  if (room.subscriptionName) return room.subscriptionName
  if (room.type === 'dm' || room.type === 'botDM') return '(DM)'
  return room.id ?? ''
}
```

`RoomList` and `MessageArea` use the helper. `RoomEventsContext`
merges `evt.subscription.name` onto the room before dispatching
`ROOM_ADDED`, so subscription.update events populate the DM friendly
name. The initial `rooms.list` response returns raw `Room` (no
subscription join today), so pre-existing DMs render the `"(DM)"`
placeholder until a subscription event refreshes them — a future
server-side `rooms.list` change can populate names eagerly without
touching the frontend.

### Layer 9 — Encryption-resilient broadcast handling

The reducer's `MESSAGE_RECEIVED` previously did:

```js
if (!evt.message || !evt.message.id) return state  // silent drop
```

That covered the encryption-on case where `evt.message` is dropped
via Go's `json:omitempty` and `evt.encryptedMessage` carries the AEAD
ciphertext. The drop was invisible — the room appeared frozen.

Now the reducer synthesizes a placeholder when only
`encryptedMessage` is present, using the top-level `lastMsgId` /
`lastMsgAt` that `broadcast-worker.buildRoomEvent` always populates
regardless of encryption mode:

```js
if ((!msg || !msg.id) && evt.encryptedMessage) {
  if (!evt.lastMsgId) return state
  msg = {
    id: evt.lastMsgId,
    roomId: evt.roomId,
    content: '[encrypted message]',
    createdAt: evt.lastMsgAt ?? new Date(evt.timestamp ?? Date.now()).toISOString(),
    encrypted: true,
  }
}
```

The plaintext path is unchanged and still wins if both fields are
present (forward-compatible with mixed rollouts).

### Layer 10 — Smoke harnesses

Two standalone scripts at `chat-frontend/scripts/`, deliberately
*not* wired into `npm test`:

**`asyncJob.smoke.mjs`** — vanilla nats-server, fake responders.
Wire-level smoke for `requestWithAsyncResult`:

- `X-Request-ID` header round-trip
- Sync `{status: 'accepted'}` + async `{status: 'ok'}` happy path
- Sync `{error}` → throws `SyncError`
- Async `{status: 'error'}` → throws `AsyncError`
- `treatAsSuccess(isDMExistsReply)` → returns `{async: null}` instead of throwing
- `asyncTimeout: 50` with no async response → throws `AsyncTimeout` and cleans up the response subscription
- Subscribe-before-publish race: responder publishes its async result 10 ms after acking; helper must still see it

**`liveStack.smoke.mjs`** — full stack (auth-service + room-service +
room-worker + Mongo + NATS). Verifies the helper survives the real
wire:

- `POST /auth` (DEV_MODE) returns a NATS JWT
- `requestWithAsyncResult(roomCreate(...))` creates an actual channel
- `member.list?enrich=true` returns the seeded individuals plus the requester as owner
- DM-create then DM-create-again returns the same `roomId` via the dedup branch

Both scripts use shared `check / pass / fail` helpers and exit with
status 1 on any failure so they're CI-able if/when wired up later.

## UI placement

```text
┌─────────────────────────────────────────────────────────────────────────┐
│ chat-header                                                             │
│  [Chat]                [SearchBar]               alice·site-A [☀][Logout]│
│                          ↑ absolute-centered                            │
├──────────────────┬──────────────────────────────────────────────────────┤
│ chat-sidebar     │ chat-main-content                                    │
│                  │ ┌──────────────────────────────────────────────────┐ │
│ Rooms            │ │ message-area-header                              │ │
│  # general    ●  │ │  # general                       [3 members]     │ │ ← clickable
│  # backend       │ ├──────────────────────────────────────────────────┤ │
│  @ bob           │ │  MessageArea                                     │ │
│  @ (DM)          │ │   alice 09:00  hi team                           │ │
│   ↑ DM fallback  │ │   bob   09:01  morning                           │ │
│                  │ │   …                                              │ │
│                  │ │  [encrypted message]    ← placeholder for         │ │
│                  │ │                            encrypted broadcasts   │ │
│                  │ ├──────────────────────────────────────────────────┤ │
│                  │ │  MessageInput                                    │ │
│                  │ └──────────────────────────────────────────────────┘ │
│ [+ Create Room]  │                                                      │
└──────────────────┴──────────────────────────────────────────────────────┘
```

### Entry points

| UI element | Location | Visibility | Opens |
|---|---|---|---|
| `+ Create Room` | sidebar bottom | always | `CreateRoomDialog` |
| `N members` badge | `message-area-header` (right) | channel only | `ManageMembersDialog` |

DMs intentionally get no Manage Members affordance.

### `CreateRoomDialog`

```text
┌─ dialog ─────────────────────────────────────────────┐
│ Create Room                                          │
│                                                      │
│ Name (channel) or leave empty for a DM               │
│ [e.g. frontend-team                              ]   │
│                                                      │
│ ┌─ MemberPicker ─────────────────────────────────┐   │
│ │ Users    [chip] [chip]   alice, bob ◄ comma-OK │   │
│ │ Orgs     [chip]          eng-org, ops-org      │   │
│ │ Channels [chip]          r-101  (search drop)  │   │
│ └────────────────────────────────────────────────┘   │
│                                                      │
│ [error banner — if any]                              │
│                                                      │
│             [Cancel]  [Create]                       │
│                       └─ flips through:              │
│                          Create → Creating… →        │
│                          Waiting for server confirmation… │
│                          → (dialog closes)           │
└──────────────────────────────────────────────────────┘
```

### `ManageMembersDialog` — Owner viewer

```text
┌─ dialog ─────────────────────────────────────────────┐
│ Manage Members — frontend-team                       │
│                                                      │
│ [ Members ]  [   Add   ]                             │
│                                                      │
│  ┌─ Orgs first (single list) ─────────────────────┐  │
│  │ ▸ Engineering   42 members             [Remove] │  │ ← click name to expand
│  ├────────────────────────────────────────────────┤  │
│  │ Alice A   王愛麗   [owner]            [Leave]   │  │ ← self row
│  │ Bob B     陳大文                 [Promote][Remove]│ │
│  │ Carol C   陳嘉麗   [owner]       [Demote ][Remove]│ │
│  └────────────────────────────────────────────────┘  │
│                                                      │
│              [Close]                                 │
└──────────────────────────────────────────────────────┘
```

When the user clicks an org name the row expands inline, fetching
`chat.user.{a}.request.orgs.{orgId}.members` and rendering the
returned individuals underneath with no per-row buttons (the parent
org row owns the lifecycle):

```text
│  ▾ Engineering   42 members            [Remove]      │
│    │ Dave Davies     戴文偉                          │
│    │ Erin Evans      葉伊蓮                          │
│    │ Frank Fischer   費法蘭                          │
│    │ …                                               │
```

A subsequent collapse + re-expand reuses the cached payload — no
second RPC. While the first fetch is in flight, a placeholder
`Loading members…` row is rendered; on RPC failure it becomes
`Failed to load members.`.

### `ManageMembersDialog` — Non-owner viewer (e.g. bob)

```text
┌─ dialog ─────────────────────────────────────────────┐
│ Manage Members — frontend-team                       │
│ [ Members ]  [   Add   ]                             │
│                                                      │
│  Engineering   42 members                            │
│  Alice A   王愛麗   [owner]                          │
│  Bob B     陳大文                            [Leave]  │ ← self row, Leave only
│  Carol C   陳嘉麗   [owner]                          │
│                                                      │
│              [Close]                                 │
└──────────────────────────────────────────────────────┘
```

## Trade-offs and rejected alternatives

### Hook-shaped helper (`useAsyncJob`) — rejected

Considered: `const {run, status, error} = useAsyncJob(subject)`.
Looks clean inline but doesn't compose with the actual call sites —
the dialog needs to `await` the result, branch on the dedup case,
and call `onCreated` with the resulting `roomId`. A hook that
exposes `{status, error}` makes that flow inside-out; the
imperative `await requestWithAsyncResult()` reads as the spec
describes it.

### Reuse `request` and infer two-phase from payload — rejected

Considered: have `NatsContext.request` notice an
`X-Request-ID`-bearing caller and auto-subscribe to the response
subject. Conflates the two contracts (one-phase RPC vs. two-phase
async-job) and makes every non-async-job request slower because of
the speculative subscription. Keep the two surfaces explicit.

### Optimistic UI with rollback — rejected

Considered: write to the local roster immediately, roll back if the
async result reports `status: 'error'`. Rejected for two reasons:

1. **Roster reads are cheap.** `member.list?enrich=true` is a single
   sync RPC that the server caches; refetching after a successful
   action costs ~50 ms and is correctness-wise the safest path.
2. **Rollback is racy.** A failed async result could arrive after
   the user has already taken a follow-up action against the
   optimistic state. The rollback would have to surgically undo
   *only* the failed action, which is more state machine than the
   current flow needs.

### Generic `ChipInput` with a Field abstraction — rejected

Tempting refactor: extract `EntityField` into a generic `ChipInput`
component published as a sibling. The only callers are
`MemberPicker`'s three fields, all in the same file. A second caller
would justify the extraction; for now the `EntityField` helper lives
in the same module as the picker.

### Auto-`onCreated` from a `subscription.update` listener at ChatPage level — rejected

Considered: have ChatPage listen for `subscription.update` itself
and react. Rejected because the create flow is owned by
`CreateRoomDialog` — it shouldn't reach across components to wire
up a global subscription. The dialog already has access to
`useRoomSummaries`, and dispatching ROOM_ADDED already fires the
exact reactive update we want.

### Optimistic `ROOM_ADDED` from CreateRoomDialog — rejected (after a brief trial)

Tried: have `ChatPage.onCreated` dispatch a synthetic ROOM_ADDED
immediately so the auto-deselect effect doesn't fire. Worked, but
left a different bug: the user landed in the room *before* the
channel subscription was open, so their first message didn't echo
back. The wait-for-`subscription.update` pattern solves both
problems with one mechanism — the dialog closes when the room IS in
summaries AND the channel sub IS open.

### Eager channel-sub on room selection — deferred

Considered: open the channel sub the moment a room is selected,
regardless of whether `subscription.update` has fired yet. Would
fix the "send message immediately after Create" path independently
of the wait-on-create. Deferred because (a) the wait pattern
already solves the Create case end-to-end, and (b) the eager-sub
pattern needs to handle the case where the user selects a room
they're not subscribed to (e.g. via search results) — server will
reject the subscribe, and we'd want a UI for that. Out of scope
here; revisit if the wait pattern proves insufficient.

### Decrypt encrypted events on the client — out of scope

The placeholder approach (Layer 9) renders "[encrypted message]" so
the room never looks frozen. Real decryption requires shipping the
room-key library to the browser, key rotation handling, and a key
distribution flow — all separate work.

### Per-site Mongo / Cassandra to test cross-site federation — out of scope

The chat-frontend changes here don't depend on federation. The
cross-site member-add path is exercised end-to-end by
`liveStack.smoke.mjs` against a single-site stack; multi-site
verification is a separate effort.

## Out of scope (not done, not planned here)

- **Server-side `chat.user.{a}.request.rooms.create` cleanup.** The
  obsolete subject is no longer published-to from the frontend; any
  server handler still listening on it can be retired separately.
- **User + org search.** Wire up `search.users` / `search.orgs` once
  the corresponding server endpoints land; the picker is already
  config-driven to accept their `searchFetcher` props.
- **Inline rename / icon edit in roster.** Manage-members is
  membership-only; room metadata edits are a separate UI.
- **Pagination / virtualization of the roster.** Follow-up if room
  sizes grow past a reasonable flat list.
- **Retries / idempotency UX.** The helper is one-shot. Network
  flakes surface as `AsyncTimeout` / `SubscriptionClosed`; the user
  re-clicks. A retry-on-timeout UX would be an opt-in wrapper on
  top of the helper, not a change to it.
- **Migration of `LeaveRoomButton`'s call sites elsewhere in the
  codebase.** The component was only mounted from `ChatPage`; no
  other call sites exist. The deletion is exhaustive.

## File inventory

```text
chat-frontend/src/lib/
├── asyncJob.js                # requestWithAsyncResult + ASYNC_JOB_ERROR_KINDS + formatAsyncJobError
├── asyncJob.test.js
├── constants.js               # ROLE_*, HISTORY_MODE_*, ERR_DM_ALREADY_EXISTS, isDMExistsReply
├── subjects.js                # roomCreate, memberAdd/Remove/List/RoleUpdate, userResponse, searchRooms, orgMembers, …
├── subjects.test.js
├── roomFormat.js              # roomPrefix + roomDisplayName (new) + roomFromSearchHit + searchRoomPrefix
├── roomFormat.test.js         # NEW — covers DM placeholder, subscriptionName fallback, etc.
├── roomEventsReducer.js       # MESSAGE_RECEIVED now synthesizes encrypted placeholder
├── roomEventsReducer.test.js
├── useDebouncedSearch.js
└── useDebouncedSearch.test.js

chat-frontend/src/context/
├── NatsContext.jsx            # requestWithAsyncResult on the context + JSDoc + useMemo on value
└── RoomEventsContext.jsx      # merges evt.subscription.name onto rooms before ROOM_ADDED

chat-frontend/src/components/
├── CreateRoomDialog.jsx       # rewired to roomCreate + treatAsSuccess: isDMExistsReply + subscription.update wait
├── CreateRoomDialog.test.jsx
├── MemberPicker.jsx           # shared comma-separated chip input for users/orgs/channels; forwardRef + flushAndGetEntries
├── MemberPicker.test.jsx
├── ManageMembersDialog.jsx    # two tabs: Members (roster) + Add
├── ManageMembersDialog.test.jsx
├── MessageArea.jsx            # hosts the RoomMembersBadge in its header
├── MessageArea.test.jsx
├── RoomList.jsx               # uses roomDisplayName
├── RoomMembersBadge.jsx       # NEW — "N members" pill, fetches member.list, opens dialog on click
├── RoomMembersBadge.test.jsx
├── MessageInput.jsx           # placeholder uses roomPrefix + roomDisplayName
├── MessageInput.test.jsx      # NEW — channel vs DM placeholder, falls back to (DM)
└── manageMembers/
    ├── AddMembersForm.jsx     # MemberPicker + history-mode toggle + ChannelRef[] fix
    ├── AddMembersForm.test.jsx
    ├── MemberRoster.jsx       # single list, orgs first, EngName+ChineseName, owner-gated controls, self-row Leave, inline org expansion, gen-counter fetch guards
    └── MemberRoster.test.jsx

chat-frontend/src/pages/
├── ChatPage.jsx               # Members button removed from header; threads onOpenMembers + membersRefreshKey to MessageArea
└── ChatPage.test.jsx

chat-frontend/src/styles/
└── index.css                  # .room-members-badge + .roster-* rules (incl. .roster-row-org, .roster-org-children, .roster-chevron)

chat-frontend/scripts/
├── asyncJob.smoke.mjs         # wire-level smoke against vanilla nats-server
└── liveStack.smoke.mjs        # end-to-end smoke against auth + room-service + room-worker
```

**Deleted:**

- `chat-frontend/src/components/LeaveRoomButton.jsx` + test
- `chat-frontend/src/components/manageMembers/RemoveMemberForm.jsx` + test
- `chat-frontend/src/components/manageMembers/RoleUpdateForm.jsx` + test
- `chat-frontend/src/components/manageMembers/RemoveOrgForm.jsx` + test
- `roomsCreate` subject builder + `parseList` helper from `lib/subjects.js`
- `RemoveByIdRow` component + state in `MemberRoster.jsx`

All tests pass in CI/local runs and `vite build` produces a clean
production bundle.
