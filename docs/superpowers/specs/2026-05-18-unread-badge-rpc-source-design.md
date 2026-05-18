# Unread Badge: RPC source

## Problem

`UnreadBadge` derives its number from `state.summaries` via `useUnreadTotal`
/ `selectUnreadTotal`. We want the badge fed by a backend RPC instead.

The real user-service is not in this repo, but this repo already has the
established "real backend not available" pattern: `mock-user-service`, a
dev-only Go service that answers user-service RPC subjects with hardcoded
responses (it already backs `subscription.getCurrent/getRooms/getApps`).
So the unread count follows that pattern — a real sync RPC op on the
frontend, served by a new `subscription.count` route on
`mock-user-service` — rather than a hardcoded stub inside the frontend op.

## RPC contract

- **Subject:** `chat.user.{account}.request.user.{siteId}.subscription.count`
- **Request:** `{ "unread": true }` (`unread` accepted and ignored by the mock)
- **Response:** `{ "count": 42 }` (mock returns a fixed `42`)

## Changes

### Backend (Go)

1. **Subject builders** — `pkg/subject/subject.go`:
   `UserSubscriptionCount(account, siteID)` and
   `UserSubscriptionCountPattern(siteID)`, mirroring
   `UserSubscriptionGetApps` (wildcard-token panic guard included).
   Added to all three `subject_test.go` tables.

2. **mock-user-service** — `mock-user-service/handler.go`: private
   `subscriptionCountReq { Unread bool }` / `subscriptionCountResp
   { Count int }` types, a `subscriptionCount` handler that site-checks
   and returns `{ Count: mockUnreadCount }` (`mockUnreadCount = 42`),
   registered as route #13 via `UserSubscriptionCountPattern`. Handler
   test covers happy path, ignored `unread` flag, and site mismatch.

3. **Docs** — `docs/client-api.md` gains route #13; the
   `mock-user-service` design doc's "rejected" note is superseded.

### Frontend

4. **Subject builder** — `src/api/_transport/subjects.ts`:
   `userSubscriptionCount(account, siteId)`, next to the other
   `userSubscription*` builders.

5. **api op** — `src/api/getUnreadCount/index.ts`, re-exported from
   `src/api/index.ts`. A normal sync request/reply op:
   `getUnreadCount({ user, request }: Nats): Promise<UnreadCountResponse>`
   → `request<UnreadCountResponse>(userSubscriptionCount(user.account,
   user.siteId), { unread: true })`. No hardcoded value in the frontend.

6. **Hook** — `src/context/RoomEventsContext/useUnreadCount.js`
   (context-local per frontend conventions).
   - Fetches `getUnreadCount(nats)` on mount and re-fetches when the
     active room changes (`state.activeRoomId`) — the same moment the
     old summaries count used to decrement on read.
   - Returns a plain `number` (the unread count; `0` until the first
     fetch resolves). Stale in-flight results dropped via effect cleanup.

7. **`UnreadBadge.jsx`** — consume `useUnreadCount()` instead of
   `useUnreadTotal()`. Drop `hasMention` and the `unread-badge--mention`
   variant (the RPC carries no mention info). Keep: hide when `<= 0`,
   `99+` cap, `aria-label`/`title`. `.chat-header-badge:empty` collapses
   the superscript wrapper at zero unread.

8. **Delete dead code** — `useUnreadTotal` (RoomEventsContext.tsx) and
   `selectUnreadTotal` (reducer.js), plus their tests.

## Testing

- `pkg/subject`: builder + pattern + wildcard-panic table entries.
- `mock-user-service`: `subscriptionCount` happy path, ignored flag,
  site mismatch.
- `api/getUnreadCount`: requests the `subscription.count` subject with
  `{ unread: true }` and returns the reply; propagates transport errors.
- `useUnreadCount`: fetches on mount; re-fetches on `activeRoomId`
  change; stale cycles dropped.
- `UnreadBadge.test.jsx`: hides at 0, renders count, caps at `99+`; no
  mention variant.
- Removed `selectUnreadTotal` reducer tests.

## Out of scope

- The real user-service (separate; `mock-user-service` stands in for dev).
- Mention-accent styling on the badge (dropped with this change).
