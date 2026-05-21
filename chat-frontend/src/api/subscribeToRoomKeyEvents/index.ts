import { userRoomKey } from '../_transport/subjects'
import type { Nats, NatsSubscription, SubscriptionCallback } from '../types'

/** Subscribe to the calling user's room-key event stream. */
export function subscribeToRoomKeyEvents(
  { subscribe, user }: Pick<Nats, 'subscribe' | 'user'>,
  callback: SubscriptionCallback,
): NatsSubscription {
  return subscribe(userRoomKey(user.account), callback)
}
