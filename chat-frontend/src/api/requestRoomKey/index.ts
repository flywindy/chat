import { roomKeyGet } from '../_transport/subjects'
import type { Nats } from '../types'

export interface RequestRoomKeyArgs {
  roomId: string
  siteId: string
  /** Omit to fetch the current room key. */
  version?: number
}

/** Wire shape mirrors pkg/model.RoomKeyGetResponse. privateKey is base64. */
export interface RequestRoomKeyResponse {
  roomId: string
  version: number
  privateKey: string
}

/** Fetch the room key bytes for (roomId, version?) from room-service.
 *  Returns the reply as-is; callers decode privateKey from base64. */
export async function requestRoomKey(
  { user, request }: Nats,
  { roomId, siteId, version }: RequestRoomKeyArgs,
): Promise<RequestRoomKeyResponse> {
  const payload = version === undefined ? {} : { version }
  return request<RequestRoomKeyResponse>(roomKeyGet(user.account, roomId, siteId), payload)
}
