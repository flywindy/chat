import { subscriptionUpdate } from '../_transport/subjects'
import type { Nats, NatsSubscription, SubscriptionCallback } from '../types'

/** Subscribe to per-user subscription updates (added / removed
 *  membership events from room-worker). */
export function subscribeToSubscriptionUpdates(
  { user, subscribe }: Nats,
  callback: SubscriptionCallback,
): NatsSubscription {
  return subscribe(subscriptionUpdate(user.account), callback)
}
