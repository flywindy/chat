import { msgEdit } from '../_transport/subjects'
import type { Nats } from '../types'

export interface EditMessagePayload {
  messageId: string
  newMsg: string
}

export interface EditMessageArgs {
  roomId: string
  siteId: string
  payload: EditMessagePayload
}

/** Edit an existing message's content. Fire-and-forget. */
export function editMessage(
  { user, publish }: Nats,
  { roomId, siteId, payload }: EditMessageArgs,
): void {
  publish(msgEdit(user.account, roomId, siteId), payload)
}
