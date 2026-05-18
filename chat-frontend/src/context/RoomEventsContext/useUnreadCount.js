import { useEffect, useState } from 'react'
import { getUnreadCount } from '@/api'

/**
 * App-wide unread total for the header badge, sourced from the
 * `subscription.count` RPC instead of derived from `state.summaries`.
 *
 * Fetches once on mount and again whenever `activeRoomId` changes —
 * opening/reading a room is exactly when the server-side unread total
 * moves, so the badge re-pulls then. (Against the current hardcoded
 * mock this re-pull is a harmless no-op; with the real RPC it reflects
 * the new count.)
 *
 * Each fetch carries an `ignore` flag flipped by the effect cleanup, so
 * a slow earlier request can't overwrite a newer one and a resolution
 * after unmount is dropped.
 *
 * @param {{ user: { account: string, siteId: string } }} nats
 * @param {string|null} activeRoomId
 * @returns {number} unread total (0 until the first fetch resolves)
 */
export function useUnreadCount(nats, activeRoomId) {
  const [total, setTotal] = useState(0)

  useEffect(() => {
    let ignore = false
    getUnreadCount(nats)
      .then((resp) => {
        if (!ignore) setTotal(resp.count ?? 0)
      })
      .catch(() => {
        if (!ignore) setTotal(0)
      })
    return () => {
      ignore = true
    }
  }, [nats, activeRoomId])

  return total
}
