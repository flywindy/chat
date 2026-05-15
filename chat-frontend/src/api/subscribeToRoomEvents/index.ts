import { roomEvent } from '../_transport/subjects'
import type { Nats, NatsSubscription, SubscriptionCallback } from '../types'

/** Subscribe to a single channel's event stream. */
export function subscribeToRoomEvents(
  { subscribe }: Pick<Nats, 'subscribe'>,
  { roomId }: { roomId: string },
  callback: SubscriptionCallback,
): NatsSubscription {
  return subscribe(roomEvent(roomId), callback)
}
