import { roomMetadataUpdate } from '../_transport/subjects'
import type { Nats, NatsSubscription, SubscriptionCallback } from '../types'

/** Subscribe to per-user room-metadata updates (rename, member count
 *  bumps, last-message timestamp). */
export function subscribeToRoomMetadataUpdates(
  { user, subscribe }: Nats,
  callback: SubscriptionCallback,
): NatsSubscription {
  return subscribe(roomMetadataUpdate(user.account), callback)
}
