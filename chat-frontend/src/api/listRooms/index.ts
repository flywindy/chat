import { roomsList } from '../_transport/subjects'
import type { Nats, Room } from '../types'

export interface ListRoomsResponse {
  rooms: Room[]
}

/** List all rooms the current user is subscribed to. */
export async function listRooms({ user, request }: Nats): Promise<ListRoomsResponse> {
  return request<ListRoomsResponse>(roomsList(user.account), {})
}
