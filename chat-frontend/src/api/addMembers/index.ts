import { memberAdd } from '../_transport/subjects'
import type { Nats, ChannelRef, HistoryConfig, AsyncJobOptions, AsyncJobResult } from '../types'

export interface AddMembersArgs {
  roomId: string
  siteId: string
  users?: string[]
  orgs?: string[]
  channels?: ChannelRef[]
  history?: HistoryConfig
}

/**
 * Add members to a channel. Two-phase: returns once the sync ACK arrives
 * AND the async worker result lands on the response subject. Mirrors
 * pkg/model.AddMembersRequest — passes through users / orgs / channels
 * (plus the history-sharing flag) unmodified.
 */
export async function addMembers(
  { user, requestWithAsyncResult }: Nats,
  args: AddMembersArgs,
  opts?: AsyncJobOptions,
): Promise<AsyncJobResult> {
  const { roomId, siteId, users = [], orgs = [], channels = [], history } = args
  const payload: Record<string, unknown> = { roomId, users, orgs, channels }
  if (history) payload.history = history
  return requestWithAsyncResult(memberAdd(user.account, roomId, siteId), payload, opts)
}
