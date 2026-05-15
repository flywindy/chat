import { msgDelete } from '../_transport/subjects'
import type { Nats } from '../types'

export interface DeleteMessagePayload {
  messageId: string
}

export interface DeleteMessageArgs {
  roomId: string
  siteId: string
  payload: DeleteMessagePayload
}

/** Soft-delete a message. Fire-and-forget. */
export function deleteMessage(
  { user, publish }: Nats,
  { roomId, siteId, payload }: DeleteMessageArgs,
): void {
  publish(msgDelete(user.account, roomId, siteId), payload)
}
