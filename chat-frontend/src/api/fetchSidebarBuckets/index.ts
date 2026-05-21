import {
  userSubscriptionGetCurrent,
  userSubscriptionGetApps,
  userSubscriptionGetRooms,
} from '../_transport/subjects'
import type { Nats, DMSubscription, Room } from '../types'

/** Wire shape of every `subscription.get*` reply.
 *  Both fields are non-omitempty on the Go side
 *  (`mock-user-service/handler.go::subscriptionListResp`) — `Subscriptions`
 *  is always a slice (possibly empty), and `Total` is always an int.
 *
 *  Each entry is typed `DMSubscription` (= Subscription ∪ { hrInfo? }) to
 *  match Go's flattened JSON for both subscription kinds: channels/groups
 *  ship plain Subscription (hrInfo absent ⇒ typed `undefined`), DM rooms
 *  ship DMSubscription (hrInfo present). One type covers both since
 *  DMSubscription extends Subscription. The real user-service additionally
 *  embeds room-level metadata (userCount, lastMsgAt, lastMsgId) inline so
 *  the frontend doesn't need a separate `rooms.list` call. */
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
  /** Room records derived from the union of the three subscription
   *  responses, deduped by roomId. The reducer's BUCKETS_LOADED case
   *  consumes this to build `state.summaries` — no separate rooms.list
   *  RPC is needed because the real user-service embeds room metadata
   *  inline on each subscription reply. */
  rooms: Room[]
}

/**
 * Bootstrap the sidebar by fetching three lists from user-service in
 * parallel:
 *   1. `getCurrent({ favorite: true })` — favorited subscriptions, drives
 *      the Favorite section.
 *   2. `getApps()` — app subscriptions, drives the Apps section.
 *   3. `getRooms()` — non-app room subscriptions (channels / DMs /
 *      discussions), drives the Channels and DMs section.
 *
 * Each subscription record carries its room metadata inline on the real
 * user-service, so we derive `rooms` from the union of all three replies
 * (deduped by roomId). The reducer's `BUCKETS_LOADED` action consumes
 * this shape directly. Partition exclusivity (favorite > apps > channelDm)
 * is enforced at render time by `useSidebarSections`, so a room ID can
 * appear in more than one bucket without double-render.
 *
 * Uses `Promise.allSettled` so a single bucket RPC failure degrades that
 * one bucket to empty rather than black-holing the whole bootstrap.
 */
export async function fetchSidebarBuckets({ user, request }: Nats): Promise<SidebarBuckets> {
  const favSubject = userSubscriptionGetCurrent(user.account, user.siteId)
  const appsSubject = userSubscriptionGetApps(user.account, user.siteId)
  const roomsSubject = userSubscriptionGetRooms(user.account, user.siteId)
  const results = await Promise.allSettled([
    request<SidebarBucketReply>(favSubject, { favorite: true }),
    request<SidebarBucketReply>(appsSubject, {}),
    request<SidebarBucketReply>(roomsSubject, {}),
  ])
  const empty: SidebarBucketReply = { subscriptions: [], total: 0 }
  const unwrap = (
    result: PromiseSettledResult<SidebarBucketReply>,
    label: string,
  ): SidebarBucketReply => {
    if (result.status === 'fulfilled') {
      // TEMP DEBUG: compact summary so we can verify what each
      // subscription RPC returns on cold start. Remove once verified.
      console.log('[sidebar-bootstrap]', label, {
        count: result.value.subscriptions.length,
        roomIds: result.value.subscriptions.map((s) => s.roomId),
      })
      return result.value
    }
    const err = result.reason
    console.warn(
      '[sidebar-bootstrap]',
      label,
      'FAILED:',
      err?.message ?? err,
    )
    return empty
  }
  const favResp = unwrap(results[0], `${favSubject} {favorite:true}`)
  const appResp = unwrap(results[1], appsSubject)
  const roomResp = unwrap(results[2], roomsSubject)

  const subscriptions: Record<string, DMSubscription> = {}
  const rooms: Room[] = []
  const collect = (resp: SidebarBucketReply) => {
    for (const s of resp.subscriptions) {
      if (!s?.roomId) continue
      // Later sources overwrite earlier ones, but the three responses
      // describe the same Subscription record so collisions are benign.
      const first = subscriptions[s.roomId] === undefined
      subscriptions[s.roomId] = s
      if (first) rooms.push(subToRoom(s, user.siteId))
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
    rooms,
  }
}

/** Derive a `Room` from a subscription record. The real user-service
 *  embeds the fields we actually need (userCount, lastMsgAt, lastMsgId);
 *  fields the reducer's `toSummary` doesn't read default to neutral
 *  zero/empty values so the type contract is satisfied. */
function subToRoom(sub: DMSubscription, fallbackSiteId: string): Room {
  return {
    id: sub.roomId,
    name: sub.name ?? '',
    type: sub.roomType,
    siteId: sub.siteId ?? fallbackSiteId,
    userCount: sub.userCount ?? 0,
    appCount: 0,
    lastMsgId: sub.lastMsgId ?? '',
    lastMsgAt: sub.lastMsgAt ?? undefined,
    createdAt: '',
    updatedAt: '',
  }
}
