import { memberRemove } from '../_transport/subjects'
import type { Nats, AsyncJobOptions, AsyncJobResult } from '../types'

export interface RemoveMemberArgs {
  roomId: string
  siteId: string
  /** Individual target. Mutually exclusive with `orgId`. */
  account?: string
  /** Org target. Mutually exclusive with `account`. */
  orgId?: string
}

/**
 * Remove a member from a channel. Two-phase. The server accepts either
 * an `account` (remove a single individual) or an `orgId` (remove an
 * entire org membership) on the same subject — exactly one must be set.
 * Use `leaveRoom` for the "leave myself" path; that's the sync variant.
 */
export async function removeMember(
  { user, requestWithAsyncResult }: Nats,
  args: RemoveMemberArgs,
  opts?: AsyncJobOptions,
): Promise<AsyncJobResult> {
  const { roomId, siteId, account, orgId } = args
  const payload: Record<string, unknown> = { roomId }
  if (account) payload.account = account
  if (orgId) payload.orgId = orgId
  return requestWithAsyncResult(memberRemove(user.account, roomId, siteId), payload, opts)
}
