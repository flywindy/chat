import { memberRemove } from '../_transport/subjects'
import type { Nats } from '../types'

export interface LeaveRoomArgs {
  roomId: string
  siteId: string
}

/** Leave a channel as the current user. Same subject as removeMember
 *  but sync-only — the caller doesn't wait on the async worker result. */
export async function leaveRoom(
  { user, request }: Nats,
  { roomId, siteId }: LeaveRoomArgs,
): Promise<unknown> {
  return request<unknown>(memberRemove(user.account, roomId, siteId), {
    roomId,
    account: user.account,
  })
}
