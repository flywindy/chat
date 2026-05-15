import { msgSurrounding } from '../_transport/subjects'
import { normalizeHistoricalMessages } from '../_transport/normalizeMessage'
import type { Nats, Message, HistoryMessage } from '../types'

export interface FetchSurroundingMessagesArgs {
  roomId: string
  siteId: string
  messageId: string
}

export interface FetchSurroundingMessagesResponse {
  messages: Message[]
}

interface WireResponse {
  messages?: HistoryMessage[]
}

/**
 * Fetch the window of messages around a specific message — used when
 * jumping to a search hit or a quoted reference that isn't currently
 * in the room's history buffer. Normalises to broadcast shape.
 */
export async function fetchSurroundingMessages(
  { user, request }: Nats,
  args: FetchSurroundingMessagesArgs,
): Promise<FetchSurroundingMessagesResponse> {
  const { roomId, siteId, messageId } = args
  const resp = await request<WireResponse>(
    msgSurrounding(user.account, roomId, siteId),
    { messageId },
  )
  return { messages: normalizeHistoricalMessages(resp.messages) }
}
