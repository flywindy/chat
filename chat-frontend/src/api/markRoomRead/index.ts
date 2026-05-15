import { messageRead } from '../_transport/subjects'
import type { Nats } from '../types'

export interface MarkRoomReadArgs {
  roomId: string
  /** Site the room lives on. Caller usually resolves this from the room
   *  summary; falls back to `user.siteId` at the call site if absent. */
  siteId: string
}

/**
 * Advance the current user's `lastSeenAt` on the server for one room.
 * Fire-and-forget — UI doesn't need to wait, and errors are swallowed
 * because a failed mark-as-read just means the next read-receipt RPC
 * will reflect slightly stale state.
 *
 * Called in two contexts (both in RoomEventsContext / useRoomSubscriptions):
 *  - When the user opens a room (`setActiveRoom`)
 *  - When a new message arrives in the currently-active room from a
 *    different sender
 */
export function markRoomRead(
  { user, request }: Nats,
  { roomId, siteId }: MarkRoomReadArgs,
): void {
  // Don't await — fire-and-forget, swallow errors. The promise is dropped
  // intentionally; the .catch keeps unhandled-rejection warnings off the
  // console when the server takes a moment to reply.
  request(messageRead(user.account, roomId, siteId), {}).catch(() => {})
}
