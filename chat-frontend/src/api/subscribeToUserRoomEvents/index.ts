import { userRoomEvent } from '../_transport/subjects'
import type { Nats, NatsSubscription, SubscriptionCallback } from '../types'

/** Subscribe to per-user room events (DM message broadcasts). */
export function subscribeToUserRoomEvents(
  { user, subscribe }: Nats,
  callback: SubscriptionCallback,
): NatsSubscription {
  return subscribe(userRoomEvent(user.account), callback)
}
