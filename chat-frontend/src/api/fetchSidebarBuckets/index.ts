import {
  userSubscriptionGetCurrent,
  userSubscriptionGetApps,
  userSubscriptionGetRooms,
} from '../_transport/subjects'
import type { Nats, DMSubscription } from '../types'

/** Wire shape of every `subscription.get*` reply.
 *  Both fields are non-omitempty on the Go side
 *  (`mock-user-service/handler.go::subscriptionListResp`) — `Subscriptions`
 *  is always a slice (possibly empty), and `Total` is always an int.
 *
 *  Each entry is typed `DMSubscription` (= Subscription ∪ { hrInfo? }) to
 *  match Go's flattened JSON for both subscription kinds: channels/groups
 *  ship plain Subscription (hrInfo absent ⇒ typed `undefined`), DM rooms
 *  ship DMSubscription (hrInfo present). One type covers both since
 *  DMSubscription extends Subscription. */
interface SidebarBucketReply {
  subscriptions: DMSubscription[]
  total: number
}

export interface SidebarBuckets {
  favoriteIds: string[]
  appIds: string[]
  channelDmIds: string[]
  /** Per-roomId map of the full subscription record (DM variant typing
   *  covers both kinds — see SidebarBucketReply above). The reducer
   *  stores this directly under `state.subscriptions` so components
   *  consume the live per-room state via `useSubscription(roomId)`. */
  subscriptions: Record<string, DMSubscription>
}

/**
 * Fetch the three sidebar bucket lists in parallel: favorites, apps,
 * and non-app rooms (channels / DMs / discussions). Each comes from
 * its own user-service RPC.
 *
 * Returns a single merged result so the caller doesn't have to know
 * about the three underlying subjects. The reducer's `BUCKETS_LOADED`
 * action consumes this shape directly.
 */
export async function fetchSidebarBuckets({ user, request }: Nats): Promise<SidebarBuckets> {
  const [favResp, appResp, roomResp] = await Promise.all([
    request<SidebarBucketReply>(
      userSubscriptionGetCurrent(user.account, user.siteId),
      { favorite: true },
    ),
    request<SidebarBucketReply>(userSubscriptionGetApps(user.account, user.siteId), {}),
    request<SidebarBucketReply>(userSubscriptionGetRooms(user.account, user.siteId), {}),
  ])
  const subscriptions: Record<string, DMSubscription> = {}
  const collect = (resp: SidebarBucketReply) => {
    for (const s of resp.subscriptions) {
      if (!s?.roomId) continue
      // Later sources overwrite earlier ones, but the three responses
      // describe the same Subscription record so collisions are benign.
      subscriptions[s.roomId] = s
    }
  }
  collect(favResp)
  collect(appResp)
  collect(roomResp)
  return {
    favoriteIds: favResp.subscriptions.map((s) => s.roomId),
    appIds: appResp.subscriptions.map((s) => s.roomId),
    channelDmIds: roomResp.subscriptions.map((s) => s.roomId),
    subscriptions,
  }
}
