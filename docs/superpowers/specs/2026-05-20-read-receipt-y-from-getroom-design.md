# Read-receipt Y from `getRoom` (not `listRoomMembers`)

## Problem

`MessageActionMenu` renders "Read by X of Y" in the kebab popover. Today Y
is sourced from `listRoomMembers(...).members.length - 1`. A `RoomMember`
row can be of type `"individual"` or `"org"` — an org-membership occupies
a single row regardless of how many users it expands to. In rooms that
have an org as a member, Y therefore under-reports the true recipient
count.

`room.userCount` is the correct number: `room-worker.ReconcileMemberCounts`
maintains it by aggregating over the `subscriptions` collection, which
holds one document per individual user (orgs are expanded at subscription
time). The existing `getRoom` RPC returns the full `Room` record, so its
`userCount` is the right source for Y.

## Change

In `chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.jsx`,
replace the on-open `listRoomMembers` call with `getRoom`, and derive Y
from `response.userCount - 1`.

Behavior:

- On kebab open: fire `fetchReadReceipt` and `getRoom` in parallel.
- `Y = recipientCount ?? Math.max(0, (room?.userCount ?? 1) - 1)` where
  `recipientCount` is `Math.max(0, getRoomResponse.userCount - 1)`.
- `getRoom` failure is non-blocking: readers (X) still render; Y falls
  back to the room prop's `userCount - 1`, clamped at 0. This mirrors the
  current degradation pattern.
- Refetch on every kebab reopen — unchanged.

## Files touched

1. `chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.jsx`
   - Swap import: `listRoomMembers` → `getRoom`.
   - Swap the parallel RPC call.
   - Derive recipient count from `getRoomResponse.userCount` instead of
     `members.length`.
   - Update the surrounding comments to explain that the on-demand
     `getRoom` returns the canonical `userCount` (orgs already expanded),
     which is necessary because the room prop's `userCount` can be stale
     after a cold-start re-login AND, separately, because per-row
     `listRoomMembers` length undercounts when orgs are present.

2. `chat-frontend/src/components/shared/MessageList/MessageRow/MessageActions/MessageActionMenu/MessageActionMenu.test.jsx`
   - Replace the hardcoded `MEMBER_LIST_SUBJECT` constant with a
     `ROOMS_GET_SUBJECT` constant matching the subject `getRoom` builds
     (`chat.user.alice.request.rooms.get.r1`, per
     `api/_transport/subjects.ts:roomsGet`).
   - Update the request-routing mocks: branches that match
     `member.list` become branches that match `rooms.get` and return a
     `Room` shape with the desired `userCount`.
   - Rename the existing "recipient count (Y) sourcing" subtests to
     describe sourcing from `getRoom`:
     - Derives Y from `getRoom.userCount - 1` when the room prop's
       `userCount` is stale (cold-start regression — kept).
     - Calls `getRoom` on the expected subject with `{roomId}`.
     - Falls back to `room.userCount - 1` when `getRoom` rejects.
   - Update the "refetches on reopen" test's subject-routing mock from
     `member.list` to `rooms.get`.
   - Add: clamps Y at 0 when `getRoom.userCount` is 0 or 1.

## Out of scope

- No backend changes. `getRoom` already exists and returns `userCount`.
- No changes to other consumers of `listRoomMembers` (e.g.,
  `RoomMembersBadge`). Their semantics are different — they list
  membership rows, including org rows by design.
- No caching across opens. Refetching on each open matches today's
  pattern and keeps Y fresh against membership changes.

## TDD order

1. Update the test file first: rewrite the three "(Y) sourcing" subtests
   to mock `rooms.get` instead of `member.list`, update the refetch test,
   add the userCount=0/1 clamp test. Run tests — they fail because
   production code still calls `listRoomMembers`.
2. Update `MessageActionMenu.jsx` to call `getRoom` and derive Y from
   `userCount - 1`. Run tests — green.
3. `npm run typecheck` and `npm test` both pass.
