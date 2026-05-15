import { searchRooms as searchRoomsSubject } from '../_transport/subjects'
import type { Nats, RoomType } from '../types'

export interface SearchRoomsArgs {
  searchText: string
  /** Server-accepted scope values; mirrors search-service's query_rooms.go.
   *  `'all'` returns rooms anyone can see; the type-specific values narrow. */
  scope: 'all' | 'channel' | 'dm' | 'app'
  size: number
}

export interface SearchRoomHit {
  roomId: string
  roomName: string
  roomType: RoomType
  siteId: string
  userAccount: string
  joinedAt: string
}

export interface SearchRoomsResponse {
  total: number
  results: SearchRoomHit[]
}

/**
 * Search rooms the caller is a member of (or all rooms if scope='all').
 * Mirrors search-service's `search.rooms` handler — hits sorted by relevance.
 */
export async function searchRooms(
  { user, request }: Nats,
  args: SearchRoomsArgs,
): Promise<SearchRoomsResponse> {
  return request<SearchRoomsResponse>(searchRoomsSubject(user.account), args)
}
