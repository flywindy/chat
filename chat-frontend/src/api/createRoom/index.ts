import { roomCreate } from '../_transport/subjects'
import type {
  Nats,
  ChannelRef,
  AsyncJobOptions,
  AsyncJobResult,
  RoomType,
} from '../types'

export interface CreateRoomArgs {
  /** Channel name. Leave empty for DM/BotDM (server infers from `users`). */
  name?: string
  users?: string[]
  orgs?: string[]
  channels?: ChannelRef[]
}

export interface CreateRoomSyncReply {
  status?: string
  roomId: string
  roomType?: RoomType
  error?: string
}

/**
 * Create a room. The server infers the room type from the payload shape:
 * - `name` set → channel
 * - `name` empty + single user in `users` → DM (or BotDM, server decides)
 *
 * Two-phase. The caller typically passes `treatAsSuccess` so that the
 * DM-exists dedup reply isn't treated as an error.
 */
export async function createRoom(
  { user, requestWithAsyncResult }: Nats,
  args: CreateRoomArgs,
  opts?: AsyncJobOptions,
): Promise<AsyncJobResult<CreateRoomSyncReply>> {
  const { name = '', users = [], orgs = [], channels = [] } = args
  const payload = { name, users, orgs, channels }
  return requestWithAsyncResult<CreateRoomSyncReply>(
    roomCreate(user.account, user.siteId),
    payload,
    opts,
  )
}
