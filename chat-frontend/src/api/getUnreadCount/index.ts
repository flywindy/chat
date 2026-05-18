import { userSubscriptionCount } from '../_transport/subjects'
import type { Nats } from '../types'

export interface UnreadCountResponse {
  count: number
}

/**
 * Fetch the caller's unread-message total for the header badge.
 *
 * Sync request/reply to the user-service `subscription.count` subject.
 * In local dev this is served by `mock-user-service` (hardcoded count);
 * the real user-service answers it in deployed environments.
 */
export async function getUnreadCount({
  user,
  request,
}: Nats): Promise<UnreadCountResponse> {
  return request<UnreadCountResponse>(
    userSubscriptionCount(user.account, user.siteId),
    { unread: true },
  )
}
