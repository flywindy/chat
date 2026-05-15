import { readReceipt } from '../_transport/subjects'
import type { Nats, Reader } from '../types'

export interface FetchReadReceiptArgs {
  roomId: string
  siteId: string
  messageId: string
}

export interface FetchReadReceiptResponse {
  readers: Reader[]
}

/** Fetch the read-receipt aggregate for a single message. */
export async function fetchReadReceipt(
  { user, request }: Nats,
  args: FetchReadReceiptArgs,
): Promise<FetchReadReceiptResponse> {
  const { roomId, siteId, messageId } = args
  return request<FetchReadReceiptResponse>(readReceipt(user.account, roomId, siteId), { messageId })
}
