import { searchMessages as searchMessagesSubject } from '../_transport/subjects'
import type { Nats } from '../types'

export interface SearchMessagesArgs {
  searchText: string
  /** Limit hits to these rooms; empty/omitted searches everywhere. */
  roomIds?: string[]
  size: number
}

export interface SearchMessageHit {
  messageId: string
  roomId: string
  siteId: string
  userId: string
  userAccount: string
  content: string
  createdAt: string
  threadParentMessageId?: string
  threadParentMessageCreatedAt?: string
}

export interface SearchMessagesResponse {
  total: number
  results: SearchMessageHit[]
}

/** Full-text search across messages. Optionally scope to a room subset. */
export async function searchMessages(
  { user, request }: Nats,
  args: SearchMessagesArgs,
): Promise<SearchMessagesResponse> {
  return request<SearchMessagesResponse>(searchMessagesSubject(user.account), args)
}
