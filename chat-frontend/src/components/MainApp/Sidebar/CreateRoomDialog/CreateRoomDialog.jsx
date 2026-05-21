import { useEffect, useRef, useState } from 'react'
import { useNats } from '@/context/NatsContext'
import { useRoomSummaries } from '@/context/RoomEventsContext'
import { createRoom } from '@/api'
import { isDMExistsReply } from '@/lib/constants'
import { formatAsyncJobError } from '@/api'
import MemberPicker from '@/components/MainApp/ChatPage/ManageMembersDialog/MemberPicker/MemberPicker'

// How long to wait for the server's subscription.update event before
// giving up and closing the dialog anyway. With the event the user lands
// in a room that already has its channel subscription opened (so messages
// echo back immediately); without it, the message stream is racy for the
// first second or so, but at least the dialog doesn't hang forever.
const SUBSCRIPTION_WAIT_TIMEOUT_MS = 3000

// Type inferred server-side from payload shape (name vs. members).
export default function CreateRoomDialog({ onClose, onCreated }) {
  const nats = useNats()
  const { user } = nats
  const { summaries } = useRoomSummaries()
  const [name, setName] = useState('')
  const [users, setUsers] = useState([])
  const [orgs, setOrgs] = useState([])
  const [channels, setChannels] = useState([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  // The just-created room we're waiting for confirmation on. Holds the
  // {id, type, siteId, name} payload destined for onCreated. While set,
  // the dialog stays open with the "Waiting for server confirmation…"
  // label; once `summaries` contains a row matching `id` we fire the
  // callback. ROOM_ADDED is only dispatched via the subscription.update
  // event handler, which also calls openChannelSub() for channels — so by
  // the time we close, the channel subscription is already open and the
  // user's first message will echo back live.
  const [pendingRoom, setPendingRoom] = useState(null)
  const pickerRef = useRef(null)

  // Close when the room shows up in summaries (subscription.update arrived
  // → ROOM_ADDED dispatched → channel sub opened). Idempotent: stays open
  // until match.
  useEffect(() => {
    if (!pendingRoom) return
    if (summaries.some((r) => r.id === pendingRoom.id)) {
      onCreated(pendingRoom)
      onClose()
    }
  }, [summaries, pendingRoom, onCreated, onClose])

  // Safety net: if subscription.update never arrives (server bug, NATS
  // outage), don't auto-select the would-be room. Calling onCreated here
  // would set selectedRoom on ChatPage, but summaries doesn't contain the
  // room yet so the page's auto-deselect effect would bounce the user
  // back to the empty state; even if it didn't, the channel subscription
  // for the new room hasn't been opened (also gated on
  // subscription.update), so any message the user tried to send would
  // not echo back live. Surface an error inside the dialog instead,
  // clear pendingRoom so the wait state unwinds (re-enabling Cancel),
  // and let the user dismiss. If the room actually was created, it'll
  // appear in their sidebar shortly when subscription.update finally
  // lands — they can click it there.
  useEffect(() => {
    if (!pendingRoom) return
    const t = setTimeout(() => {
      setError(
        'Room creation is taking longer than expected. If it succeeds, the room will appear in your sidebar shortly — you can dismiss this dialog.'
      )
      setPendingRoom(null)
    }, SUBSCRIPTION_WAIT_TIMEOUT_MS)
    return () => clearTimeout(t)
  }, [pendingRoom])

  const trimmedName = name.trim()

  const handleSubmit = async (e) => {
    e.preventDefault()
    if (!user) return
    // Auto-commit any typed-but-not-Entered text in the picker so users
    // don't have to remember to press Enter before clicking Create. Use the
    // returned values for this submit because the state updates queued by
    // flushAndGetEntries don't land until the next render.
    const { users: finalUsers, orgs: finalOrgs, channels: finalChannels } =
      pickerRef.current?.flushAndGetEntries() ?? { users, orgs, channels }
    if (!trimmedName && finalUsers.length + finalOrgs.length + finalChannels.length === 0) return
    setLoading(true)
    setError(null)
    try {
      const { sync } = await createRoom(
        nats,
        { name: trimmedName, users: finalUsers, orgs: finalOrgs, channels: finalChannels },
        { treatAsSuccess: isDMExistsReply }
      )
      const roomId = sync.roomId
      // For DM/BotDM the name is empty; fall back to the counterpart account
      // so the sidebar + header have something to show until the canonical
      // name arrives via subscription.update.
      const displayName = trimmedName || finalUsers[0] || ''

      if (isDMExistsReply(sync)) {
        // Dedup branch: server already confirmed the DM; skip the summaries-wait that can trip the 3s banner on a BUCKETS_LOADED race.
        onCreated({ id: roomId, type: 'dm', siteId: user.siteId, name: displayName })
        onClose()
        return
      }

      // On the normal branch the server returns roomType explicitly.
      // Request is done; flip loading off so Cancel becomes re-enabled
      // during the subscription.update wait. The pendingRoom state marks
      // the wait phase — see the two useEffects above for the resolution
      // paths (summaries-match success vs timeout error).
      setPendingRoom({ id: roomId, type: sync.roomType, siteId: user.siteId, name: displayName })
    } catch (err) {
      setError(formatAsyncJobError(err))
    } finally {
      setLoading(false)
    }
  }

  const buttonLabel = pendingRoom
    ? 'Waiting for server confirmation…'
    : loading
      ? 'Creating…'
      : 'Create'

  return (
    <div
      className="dialog-overlay"
      onClick={() => {
        if (loading) return
        onClose()
      }}
    >
      <div className="dialog create-room-dialog" onClick={(e) => e.stopPropagation()}>
        <h2>Create Room</h2>
        <form onSubmit={handleSubmit}>
          <label htmlFor="room-name">Name (channel) or leave empty for a DM</label>
          <input
            id="room-name"
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. frontend-team"
            autoFocus
            disabled={loading}
          />

          <MemberPicker
            ref={pickerRef}
            users={users}
            orgs={orgs}
            channels={channels}
            onUsersChange={setUsers}
            onOrgsChange={setOrgs}
            onChannelsChange={setChannels}
            disabled={loading}
          />

          {error && <div className="dialog-error">{error}</div>}

          <div className="dialog-actions">
            {/* Cancel is enabled while waiting so users can back out of a
                slow subscription.update without having to wait for the
                timeout. It stays disabled only while the request itself
                is in flight. */}
            <button type="button" className="dialog-cancel" onClick={onClose} disabled={loading}>
              Cancel
            </button>
            {/* Submit is disabled both while the request is in flight
                AND while we're waiting for subscription.update; clicking
                again would fire a duplicate create. */}
            <button type="submit" disabled={loading || !!pendingRoom}>
              {buttonLabel}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}
