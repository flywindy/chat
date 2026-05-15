import { memberList } from '../_transport/subjects'
import type { Nats, MemberEntry } from '../types'

export interface ListRoomMembersArgs {
  roomId: string
  siteId: string
  /** With `true` the server fattens each entry with engName/chineseName
   *  etc; without it, callers get the compact roster used for counts. */
  enrich?: boolean
}

export interface ListRoomMembersResponse {
  members: MemberEntry[]
}

/** List the members of a room. */
export async function listRoomMembers(
  { user, request }: Nats,
  { roomId, siteId, enrich = false }: ListRoomMembersArgs,
): Promise<ListRoomMembersResponse> {
  const payload = enrich ? { enrich: true } : {}
  return request<ListRoomMembersResponse>(memberList(user.account, roomId, siteId), payload)
}
