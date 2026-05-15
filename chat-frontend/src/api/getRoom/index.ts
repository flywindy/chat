import { roomsGet } from '../_transport/subjects'
import type { Nats, Room } from '../types'

export interface GetRoomArgs {
  roomId: string
}

/**
 * Fetch a single room's metadata. The server reply on missing room is
 * `{error: "..."}` which the NATS transport throws on — callers should
 * try/catch rather than expect `null`.
 */
export async function getRoom(
  { user, request }: Nats,
  { roomId }: GetRoomArgs,
): Promise<Room> {
  return request<Room>(roomsGet(user.account, roomId), {})
}
