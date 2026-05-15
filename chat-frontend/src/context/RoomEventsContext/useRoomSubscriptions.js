import { useEffect, useMemo, useRef } from 'react'
import {
  fetchSidebarBuckets,
  getRoom,
  listRooms,
  markRoomRead,
  subscribeToRoomEvents,
  subscribeToRoomMetadataUpdates,
  subscribeToSubscriptionUpdates,
  subscribeToUserRoomEvents,
} from '@/api'

/** Trailing-debounce window for the active-room mark-read RPC. 500ms
 *  collapses a burst of "10 msg/sec" room chatter into ONE RPC at the
 *  trailing edge. Long enough that bursts coalesce; short enough that
 *  the server's lastSeenAt for the active user stays current. */
const MARK_READ_DEBOUNCE_MS = 500

/**
 * Owns every backend subscription + the initial-room-list fetch that
 * keeps RoomEventsContext.state in sync with the server, AND owns the
 * connection-cycle generation counter that the provider's async
 * callbacks consult to drop late dispatches.
 *
 * The effect runs once per real login cycle — depending on `user`
 * only. The `nats` context value is captured via a ref that the hook
 * keeps current on every render. Why: NatsContext's memoised value
 * flips identity on every `connected` / `error` change (e.g. a
 * transient disconnect notice on line 67 of NatsContext.jsx), and
 * including `nats` in the dep array would tear down all four
 * subscriptions + dispatch RESET on every flicker. The user identity
 * is stable for the session (only `connectToNats` writes it), so
 * gating on `[user]` rebuilds subs only when login actually changes.
 *
 * Behaviour:
 *   - On `user` flip from null to truthy: open four subscriptions
 *     (DM events, per-channel events, subscription.update,
 *     room.metadata.update) and fire listRooms() once.
 *   - On `user` flip back to null (logout): tear every subscription
 *     down, dispatch RESET, bump cancellation.
 *
 * Dispatch guard: `safeDispatch` + cancellationRef stop a late
 * resolving promise from writing to a torn-down reducer.
 *
 * Returns `{ currentGeneration }` — a stable getter the provider's
 * `loadHistory` / `jumpToMessage` use to detect "I started in
 * generation N, but generation is N+1 by the time I resolved — drop
 * this dispatch." Keeps the generation ref encapsulated in the hook
 * instead of threading it across the module boundary.
 *
 * The `stateRef` parameter is the provider's `useRef(state)` mirror —
 * the hook reads `stateRef.current.activeRoomId` + `summaries` from
 * inside long-lived subscription callbacks to decide whether to fire
 * a `markRoomRead` RPC on incoming messages.
 */
export function useRoomSubscriptions(nats, dispatch, stateRef) {
  const { user } = nats
  // Keep a live ref to `nats` so long-lived subscription callbacks
  // (subUpdate's getRoom call, listRooms inside the effect body) see
  // the latest connection without forcing the effect to re-run.
  const natsRef = useRef(nats)
  natsRef.current = nats

  // Bumped on every login (re)cycle so the provider's async fetch
  // callbacks can detect stale-generation dispatches.
  const generationRef = useRef(0)

  // Channel subscriptions live in a ref so subscriptionUpdate's
  // "added" branch (which opens them) and the cleanup (which closes
  // them) can both reach the same map without re-creating the effect.
  const channelSubs = useRef(new Map())
  const cancelledRef = useRef(false)

  // Trailing-edge debounce for the per-active-room mark-read RPC.
  // A chatty room (10+ msg/sec) would otherwise generate one
  // `message.read` RPC per inbound message; with this debounce a
  // burst coalesces to a single trailing call after the room goes
  // quiet for MARK_READ_DEBOUNCE_MS. The setActiveRoom path
  // (provider-side) stays immediate — that's the explicit user
  // action and not coalescable.
  const pendingMarkReadRef = useRef(null)
  const markReadTimeoutRef = useRef(null)

  useEffect(() => {
    if (!user) return
    cancelledRef.current = false
    generationRef.current += 1

    // Capture the nats value at effect-run time for the one-shot
    // listRooms() below. Long-lived callbacks read natsRef.current
    // directly so they pick up a fresh nc after a reconnect.
    const liveNats = natsRef.current

    const safeDispatch = (action) => {
      if (cancelledRef.current) return
      dispatch(action)
    }

    // Schedule a trailing `message.read` for the active room with a
    // 500ms debounce. A burst of N messages in a chatty room produces
    // ONE RPC at the end of the burst instead of N. If the user
    // switches rooms before the timer fires, the active-room check at
    // fire time skips the stale entry.
    const scheduleMarkActiveRead = (evtRoomId, senderAccount) => {
      if (!evtRoomId) return
      if (stateRef.current.activeRoomId !== evtRoomId) return
      if (senderAccount && senderAccount === user.account) return
      const summary = stateRef.current.summaries.find((r) => r.id === evtRoomId)
      const siteId = summary?.siteId ?? user.siteId
      // Clear any prior pending timer FIRST, then write the new pending
      // entry. Defensive ordering: if future code ever introduces async
      // work between these two lines, the prior timer can't race with
      // the new pending entry it was never meant to operate on.
      if (markReadTimeoutRef.current) clearTimeout(markReadTimeoutRef.current)
      pendingMarkReadRef.current = { roomId: evtRoomId, siteId }
      markReadTimeoutRef.current = setTimeout(() => {
        markReadTimeoutRef.current = null
        const pending = pendingMarkReadRef.current
        pendingMarkReadRef.current = null
        if (!pending) return
        // Re-check: only fire if the pending room is still the active
        // room. Mid-burst room switch would otherwise misfire a
        // mark-read for a room the user has already left.
        if (cancelledRef.current) return
        if (stateRef.current.activeRoomId !== pending.roomId) return
        markRoomRead(natsRef.current, pending)
      }, MARK_READ_DEBOUNCE_MS)
    }

    const dmSub = subscribeToUserRoomEvents(liveNats, (evt) => {
      if (evt?.type === 'new_message') {
        safeDispatch({ type: 'MESSAGE_RECEIVED', event: evt })
        scheduleMarkActiveRead(evt.roomId, evt.message?.sender?.account)
      }
    })

    const openChannelSub = (roomId) => {
      if (channelSubs.current.has(roomId)) return
      const sub = subscribeToRoomEvents(natsRef.current, { roomId }, (evt) => {
        if (evt?.type === 'new_message') {
          const hasMention = (evt.mentions ?? []).some(
            (p) => p.account === user.account
          )
          safeDispatch({ type: 'MESSAGE_RECEIVED', event: { ...evt, hasMention } })
          scheduleMarkActiveRead(evt.roomId ?? roomId, evt.message?.sender?.account)
        }
      })
      channelSubs.current.set(roomId, sub)
    }

    const closeChannelSub = (roomId) => {
      const sub = channelSubs.current.get(roomId)
      if (sub) {
        sub.unsubscribe()
        channelSubs.current.delete(roomId)
      }
    }

    const subUpdate = subscribeToSubscriptionUpdates(liveNats, (evt) => {
      if (cancelledRef.current) return
      if (evt.action === 'added' && evt.subscription?.roomId) {
        // Store the full subscription record FIRST so any consumer that
        // wakes up on the ROOM_ADDED dispatch already sees fresh roles /
        // hasMention / alert state. The full payload is what room-worker
        // emits on `subscription.update`.
        safeDispatch({ type: 'SUBSCRIPTION_UPSERTED', subscription: evt.subscription })
        getRoom(natsRef.current, { roomId: evt.subscription.roomId })
          .then((room) => {
            if (cancelledRef.current || !room) return
            // DM rooms have no canonical Room.Name server-side — the friendly
            // text lives on the user's Subscription. Stash that here so the
            // sidebar + header can fall back to it via roomDisplayName(room).
            const merged = evt.subscription?.name
              ? { ...room, subscriptionName: evt.subscription.name }
              : room
            safeDispatch({ type: 'ROOM_ADDED', room: merged })
            if (room.type === 'channel') openChannelSub(room.id)
          })
          .catch(() => {})
      } else if (evt.action === 'removed') {
        const roomId = evt.subscription?.roomId
        if (!roomId) return
        closeChannelSub(roomId)
        safeDispatch({ type: 'ROOM_REMOVED', roomId })
      }
    })

    const metaUpdate = subscribeToRoomMetadataUpdates(liveNats, (evt) => {
      safeDispatch({
        type: 'ROOM_METADATA_UPDATED',
        roomId: evt.roomId,
        name: evt.name,
        userCount: evt.userCount,
        lastMsgAt: evt.lastMsgAt,
      })
    })

    listRooms(liveNats)
      .then((resp) => {
        if (cancelledRef.current) return
        const rooms = resp.rooms ?? []
        safeDispatch({ type: 'ROOMS_LOADED', rooms })
        for (const r of rooms) {
          if (r.type === 'channel') openChannelSub(r.id)
        }
      })
      .catch((err) => {
        if (!cancelledRef.current) safeDispatch({ type: 'ROOMS_FAILED', error: err.message })
      })

    // Fetch the three sidebar buckets (favorites / apps / channel+dm) in
    // parallel. The api op orchestrates the three user-service RPCs and
    // hands back a merged shape ready for the reducer. Non-fatal: if the
    // user-service RPCs fail the sidebar grouping degrades to the legacy
    // single-list rendering, but the rooms themselves still arrive via
    // listRooms.
    fetchSidebarBuckets(liveNats)
      .then((buckets) => {
        if (cancelledRef.current) return
        safeDispatch({ type: 'BUCKETS_LOADED', ...buckets })
      })
      .catch((err) => {
        // eslint-disable-next-line no-console
        console.warn('sidebar bucket bootstrap failed:', err?.message ?? err)
      })

    return () => {
      cancelledRef.current = true
      dmSub.unsubscribe()
      subUpdate.unsubscribe()
      metaUpdate.unsubscribe()
      for (const sub of channelSubs.current.values()) sub.unsubscribe()
      channelSubs.current.clear()
      // Cancel any in-flight mark-read trailing timer so it doesn't
      // fire after teardown (would `markRoomRead` against a dead nc).
      if (markReadTimeoutRef.current) {
        clearTimeout(markReadTimeoutRef.current)
        markReadTimeoutRef.current = null
      }
      pendingMarkReadRef.current = null
      // RESET runs even when cancelled — it IS the cleanup.
      dispatch({ type: 'RESET' })
    }
    // `stateRef` is consumed only via `.current` — stable across renders
    // by construction; not in the dep array.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [user, dispatch])

  // Memoised so the provider's downstream useMemo + useCallback that
  // depend on this value don't churn on every render.
  return useMemo(() => ({
    currentGeneration: () => generationRef.current,
  }), [])
}
