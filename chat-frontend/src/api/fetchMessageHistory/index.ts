import { msgHistory } from '../_transport/subjects'
import { normalizeHistoricalMessages } from '../_transport/normalizeMessage'
import type { Nats, Message, HistoryMessage } from '../types'

export interface FetchMessageHistoryArgs {
  roomId: string
  siteId: string
  limit?: number
  /** Cursor — UTC millis. Paginate older (server expects `before *int64`). */
  before?: number
}

export interface FetchMessageHistoryResponse {
  /** Always normalised to the broadcast `Message` shape — the api layer
   *  hides the cassandra `messageId`/`msg` wire shape from callers. */
  messages: Message[]
  /** Lowest `lastSeenAt` across the room's members; used to render
   *  "read by N" markers on history. */
  minUserLastSeenAt?: number
}

interface WireResponse {
  messages?: HistoryMessage[]
  minUserLastSeenAt?: number
}

/**
 * Fetch a page of message history for a room. Newest-first.
 *
 * Normalises the cassandra wire shape (`messageId`/`msg`) into the
 * broadcast `Message` shape before resolving so callers don't see the
 * two shapes — every renderer downstream of api/ consumes `id`/`content`.
 */
export async function fetchMessageHistory(
  { user, request }: Nats,
  args: FetchMessageHistoryArgs,
): Promise<FetchMessageHistoryResponse> {
  const { roomId, siteId, limit = 50, before } = args
  const payload: Record<string, unknown> = { limit }
  if (before !== undefined) payload.before = before
  const resp = await request<WireResponse>(msgHistory(user.account, roomId, siteId), payload)
  return {
    messages: normalizeHistoricalMessages(resp.messages),
    minUserLastSeenAt: resp.minUserLastSeenAt,
  }
}
