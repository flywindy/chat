# Read-receipt Y from getRoom Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make "Read by X of Y" in the message-action kebab show a correct Y when a room contains an org as a member, by sourcing Y from `getRoom.userCount - 1` instead of `listRoomMembers(...).members.length - 1`.

**Architecture:** Frontend-only change in one component. On kebab open, fire `fetchReadReceipt` and `getRoom` in parallel (currently `fetchReadReceipt` and `listRoomMembers`). Derive `recipientCount = max(0, getRoomResponse.userCount - 1)`. On `getRoom` failure, fall back to the existing `room.userCount - 1` prop-derived value — non-blocking, matches today's degradation pattern.

**Tech Stack:** React (JSX), Vitest + @testing-library/react, NATS request/reply via the project's `api/` wrapper layer.

---

## Spec

Full design at `docs/superpowers/specs/2026-05-20-read-receipt-y-from-getroom-design.md`.

## Background for the implementer

- `MessageActionMenu.jsx` is the kebab popover on a user's own messages. It already fetches read receipts via `fetchReadReceipt`. Today it also fetches `listRoomMembers` purely to compute Y.
- `RoomMember` rows are typed `"individual"` or `"org"`. An org row counts as **one** row regardless of how many users it expands to, so `members.length - 1` undercounts Y in org-containing rooms.
- `Room.userCount` is maintained by `room-worker` aggregating over the `subscriptions` collection (one doc per individual user, with orgs expanded at subscription time), so it is the canonical recipient count.
- `getRoom` is an existing RPC: `chat-frontend/src/api/getRoom/index.ts`. Signature: `getRoom(nats, { roomId }) => Promise<Room>`. It throws on missing room — callers `try/catch` (or `.catch`) rather than expect `null`.
- The subject `getRoom` builds is `chat.user.{account}.request.rooms.get.{roomId}` (per `api/_transport/subjects.ts:roomsGet`).
- `fetchReadReceipt` builds subject `chat.user.{account}.request.room.{roomId}.{siteId}.message.read-receipt` and the existing test file hardcodes this string.

### Frontend conventions (from `chat-frontend/CLAUDE.md`) you must respect

- Components import from `@/api` (the barrel). Never from `@/api/_transport/...`.
- Tests live next to source as `*.test.jsx`. Framework is `vitest` with `jsdom`.
- Mock at the boundary: for this component test, mock `@/context/NatsContext` (already done in the existing test file) and route requests by subject substring inside a single `request` mock.
- `npm run typecheck` and `npm test` must pass.

### File map

- Modify: `chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.jsx`
- Modify: `chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.test.jsx`

No other files change. No backend changes. No new dependencies.

---

## Task 1: Update tests to expect `getRoom` instead of `listRoomMembers` (RED)

**Files:**
- Modify: `chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.test.jsx`

This task rewrites the test file so it specifies the new behavior. The production code still calls `listRoomMembers`, so tests will fail. That failure is the Red step of TDD.

- [ ] **Step 1: Replace the `MEMBER_LIST_SUBJECT` constant with `ROOMS_GET_SUBJECT`**

In `MessageActionMenu.test.jsx`, find the constant block near the top (line ~8–9):

```js
const READ_RECEIPT_SUBJECT = 'chat.user.alice.request.room.r1.siteA.message.read-receipt'
const MEMBER_LIST_SUBJECT = 'chat.user.alice.request.room.r1.siteA.member.list'
```

Replace with:

```js
const READ_RECEIPT_SUBJECT = 'chat.user.alice.request.room.r1.siteA.message.read-receipt'
const ROOMS_GET_SUBJECT = 'chat.user.alice.request.rooms.get.r1'
```

- [ ] **Step 2: Update the "refetches on reopen" test's subject-routing mock**

Find the test `it('refetches the RPC every time the menu is reopened', ...)` (around line 192). Inside its `request` mock, the second `if` branch reads:

```js
if (subj.includes('member.list')) {
  return Promise.resolve({ members: [
    { id: 'a', rid: 'r1', ts: '', member: { type: 'individual', id: 'u0', account: 'alice' } },
    { id: 'b', rid: 'r1', ts: '', member: { type: 'individual', id: 'u1', account: 'bob' } },
    { id: 'c', rid: 'r1', ts: '', member: { type: 'individual', id: 'u2', account: 'carol' } },
    { id: 'd', rid: 'r1', ts: '', member: { type: 'individual', id: 'u3', account: 'dave' } },
  ] })
}
```

Replace with:

```js
if (subj.includes('rooms.get')) {
  return Promise.resolve({
    id: 'r1', name: 'general', type: 'channel', createdBy: 'alice',
    siteId: 'siteA', userCount: 4, appCount: 0, lastMsgId: '',
    createdAt: '', updatedAt: '',
  })
}
```

The assertions ("Read by 0 of 3", "Read by 1 of 3") already match `userCount=4 → Y=3` and stay unchanged.

- [ ] **Step 3: Rewrite the "recipient count (Y) sourcing" describe block**

Find the block starting at `describe('MessageActionMenu recipient count (Y) sourcing', ...)` (around line 265) and replace its entire contents through its closing `})` with the version below. This rewrites the `mkRequest` helper and the three sub-tests to mock `rooms.get` instead of `member.list`:

```js
describe('MessageActionMenu recipient count (Y) sourcing', () => {
  const msg = { id: 'm1', sender: { account: 'alice' } }

  function mkRequest({ readers, roomResp, roomError }) {
    return vi.fn((subj) => {
      if (subj.includes('read-receipt')) return Promise.resolve({ readers })
      if (subj.includes('rooms.get')) {
        if (roomError) return Promise.reject(roomError)
        return Promise.resolve(roomResp)
      }
      return Promise.reject(new Error('unexpected subject: ' + subj))
    })
  }

  it('derives Y from getRoom.userCount - 1 even when room prop userCount is stale', async () => {
    // Regression: after Alice logs out and back in, the room summary's
    // userCount can be hydrated from a record that doesn't carry the
    // field, so it collapses to 0. The kebab must still show the right
    // denominator by fetching getRoom itself.
    //
    // This test ALSO covers the org-expansion bug: getRoom.userCount is
    // the canonical recipient count (room-worker aggregates over the
    // per-user `subscriptions` collection, with orgs expanded), so it
    // works correctly whether members are individuals or orgs.
    const request = mkRequest({
      readers: [
        { userId: 'u1', account: 'bob', engName: 'Bob' },
        { userId: 'u2', account: 'carol', engName: 'Carol' },
      ],
      roomResp: {
        id: 'r1', name: 'general', type: 'channel', createdBy: 'alice',
        siteId: 'siteA', userCount: 5, appCount: 0, lastMsgId: '',
        createdAt: '', updatedAt: '',
      },
    })
    useNats.mockReturnValue({ user: { account: 'alice', siteId: 'siteA' }, request })
    render(
      <MessageActionMenu
        message={msg}
        room={{ id: 'r1', siteId: 'siteA', userCount: 0 }}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(await screen.findByText('Read by 2 of 4')).toBeInTheDocument()
  })

  it('calls getRoom on the rooms.get subject with the room id', async () => {
    const request = mkRequest({
      readers: [],
      roomResp: {
        id: 'r1', name: 'general', type: 'channel', createdBy: 'alice',
        siteId: 'siteA', userCount: 4, appCount: 0, lastMsgId: '',
        createdAt: '', updatedAt: '',
      },
    })
    useNats.mockReturnValue({ user: { account: 'alice', siteId: 'siteA' }, request })
    render(
      <MessageActionMenu
        message={msg}
        room={{ id: 'r1', siteId: 'siteA', userCount: 4 }}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(request).toHaveBeenCalledWith(ROOMS_GET_SUBJECT, {})
  })

  it('falls back to room prop userCount - 1 when getRoom rejects', async () => {
    // getRoom failure is non-blocking: Y degrades to the prior behavior
    // so the X side of the menu still renders. The room prop's userCount
    // can be stale in some flows, but it's the best best-effort we have.
    const request = mkRequest({
      readers: [{ userId: 'u1', account: 'bob', engName: 'Bob' }],
      roomError: new Error('getRoom down'),
    })
    useNats.mockReturnValue({ user: { account: 'alice', siteId: 'siteA' }, request })
    render(
      <MessageActionMenu
        message={msg}
        room={{ id: 'r1', siteId: 'siteA', userCount: 4 }}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(await screen.findByText('Read by 1 of 3')).toBeInTheDocument()
  })

  it('clamps Y at 0 when getRoom.userCount is 1 (sender only)', async () => {
    const request = mkRequest({
      readers: [],
      roomResp: {
        id: 'r1', name: 'general', type: 'channel', createdBy: 'alice',
        siteId: 'siteA', userCount: 1, appCount: 0, lastMsgId: '',
        createdAt: '', updatedAt: '',
      },
    })
    useNats.mockReturnValue({ user: { account: 'alice', siteId: 'siteA' }, request })
    render(
      <MessageActionMenu
        message={msg}
        room={{ id: 'r1', siteId: 'siteA', userCount: 1 }}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(await screen.findByText('Read by 0 of 0')).toBeInTheDocument()
  })

  it('clamps Y at 0 when getRoom.userCount is 0 (edge case)', async () => {
    const request = mkRequest({
      readers: [],
      roomResp: {
        id: 'r1', name: 'general', type: 'channel', createdBy: 'alice',
        siteId: 'siteA', userCount: 0, appCount: 0, lastMsgId: '',
        createdAt: '', updatedAt: '',
      },
    })
    useNats.mockReturnValue({ user: { account: 'alice', siteId: 'siteA' }, request })
    render(
      <MessageActionMenu
        message={msg}
        room={{ id: 'r1', siteId: 'siteA', userCount: 0 }}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(await screen.findByText('Read by 0 of 0')).toBeInTheDocument()
  })
})
```

- [ ] **Step 4: Run the tests and confirm they fail**

Run from `chat-frontend/`:

```bash
cd chat-frontend && npx vitest run src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.test.jsx
```

Expected: failures in the "recipient count (Y) sourcing" describe block and in "refetches the RPC every time the menu is reopened". They fail because the production code still calls `listRoomMembers`, which is now treated as an "unexpected subject" by the rewritten mocks.

If failures don't match this description, stop and debug before continuing.

- [ ] **Step 5: Commit the failing tests**

```bash
git add chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.test.jsx
git commit -m "test(read-receipt): expect Y from getRoom (red)

Rewrite the recipient-count subtests to mock getRoom and assert
Y = userCount - 1. Add clamp-at-0 cases for userCount 0 and 1.
Production still calls listRoomMembers; these tests fail until the
component is updated."
```

---

## Task 2: Switch `MessageActionMenu` from `listRoomMembers` to `getRoom` (GREEN)

**Files:**
- Modify: `chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.jsx`

- [ ] **Step 1: Swap the import**

In `MessageActionMenu.jsx` line 3, change:

```jsx
import { fetchReadReceipt, listRoomMembers } from '@/api'
```

to:

```jsx
import { fetchReadReceipt, getRoom } from '@/api'
```

- [ ] **Step 2: Update the comments and the parallel-fetch block**

Replace lines 18–22 (the `recipientCount` state comment) with:

```jsx
  // Recipient count sourced from getRoom.userCount when the kebab opens.
  // Falls back to the room prop's userCount when the RPC fails or hasn't
  // resolved — the prop can be stale (0) after a cold-start re-login.
  // getRoom is preferred over listRoomMembers because the latter counts
  // membership rows (one per org, regardless of expansion) and would
  // under-report Y in rooms containing an org as a member.
  const [recipientCount, setRecipientCount] = useState(null)
```

Then replace the on-open fetch block (lines 68–95 — from the `// member.list is authoritative...` comment through the closing `})` of the `.catch`) with:

```jsx
    // getRoom returns the canonical userCount (room-worker maintains it
    // by aggregating over the per-user `subscriptions` collection, with
    // orgs expanded). This is correct for org-containing rooms where
    // listRoomMembers would under-count. Failure is non-blocking — the
    // read-receipt side still renders and Y degrades to room.userCount - 1.
    const memberCountP = Promise.resolve(
      getRoom(nats, { roomId: room.id }),
    )
      .then((resp) => (typeof resp?.userCount === 'number' ? resp.userCount : null))
      .catch(() => null)
    Promise.all([
      fetchReadReceipt(nats, { roomId: room.id, siteId, messageId }),
      memberCountP,
    ])
      .then(([receipt, memberCount]) => {
        if (!mountedRef.current) return
        setReaders(receipt?.readers ?? [])
        if (memberCount != null) {
          setRecipientCount(Math.max(0, memberCount - 1))
        }
        setLoading(false)
      })
      .catch((err) => {
        if (!mountedRef.current) return
        setError(err?.message || 'Failed to load read receipts')
        setLoading(false)
      })
```

Leave the `Y` derivation on line 100 unchanged — it already does the right thing:

```jsx
const Y = recipientCount ?? Math.max(0, (room?.userCount ?? 1) - 1)
```

- [ ] **Step 3: Run the tests and confirm they pass**

Run from `chat-frontend/`:

```bash
cd chat-frontend && npx vitest run src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.test.jsx
```

Expected: all tests in the file PASS.

- [ ] **Step 4: Run typecheck**

```bash
cd chat-frontend && npm run typecheck
```

Expected: no errors. (`getRoom` is already exported from `@/api`; `listRoomMembers` removal should not break other files because no other component in this file imports it.)

- [ ] **Step 5: Run the full frontend test suite as a regression check**

```bash
cd chat-frontend && npm test -- --run
```

Expected: all tests PASS.

- [ ] **Step 6: Commit the implementation**

```bash
git add chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.jsx
git commit -m "fix(read-receipt): source Y from getRoom.userCount

listRoomMembers counts membership rows — an org appears as a single
row regardless of expansion, so Y under-reported in org-containing
rooms. getRoom.userCount is the canonical recipient count
(room-worker maintains it over the per-user subscriptions
collection, with orgs expanded). Fall back to the room prop's
userCount on RPC failure to keep behavior non-blocking."
```

---

## Task 3: Push the branch

- [ ] **Step 1: Push**

```bash
git push -u origin claude/fix-read-receipt-count-9bhw6
```

Expected: branch updated on remote. No PR is created from this plan — that step is at the user's discretion.

---

## Done criteria

- The "recipient count (Y) sourcing" describe block in `MessageActionMenu.test.jsx` covers: stale-prop regression sourced from `getRoom`, subject assertion, fallback to room prop on getRoom rejection, clamp-at-0 for `userCount` ∈ {0, 1}.
- `MessageActionMenu.jsx` no longer imports `listRoomMembers`; it imports and calls `getRoom`.
- `npx vitest run` on the component test file is green.
- `npm run typecheck` and full `npm test -- --run` are green.
- Branch `claude/fix-read-receipt-count-9bhw6` is pushed.
