import { msgSend } from '../_transport/subjects'
import type { Nats } from '../types'

export interface SendMessagePayload {
  id: string
  content: string
  requestId: string
  quotedParentMessageId?: string
  threadParentMessageId?: string
  threadParentMessageCreatedAt?: number
}

export interface SendMessageArgs {
  roomId: string
  siteId: string
  payload: SendMessagePayload
}

/** Submit a new message into a room. Fire-and-forget. */
export function sendMessage(
  { user, publish }: Nats,
  { roomId, siteId, payload }: SendMessageArgs,
): void {
  publish(msgSend(user.account, roomId, siteId), payload)
}
