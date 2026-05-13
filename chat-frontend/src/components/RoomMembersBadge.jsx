import { useEffect, useState } from 'react'
import { useNats } from '../context/NatsContext'
import { memberList } from '../lib/subjects'

// Clickable "N members" widget that lives at the top of the chat-main pane
// for channel rooms. Replaces the standalone "Members" button that used to
// sit in the global chat-header — placing it on the room toolbar keeps the
// affordance next to the room it acts on. DMs don't surface this because
// their membership is fixed at create-time.
//
// Props:
//   room       — current selectedRoom (null is OK; widget hides itself)
//   onOpen     — invoked on click; parent uses this to open ManageMembersDialog
//   refreshKey — opaque value bumped by the parent to force a refetch
//                (e.g. after the dialog closes a member-add may have changed
//                the count). The badge already refetches when room.id changes.
export default function RoomMembersBadge({ room, onOpen, refreshKey = 0 }) {
  const { user, request } = useNats()
  const account = user?.account
  const [count, setCount] = useState(null)
  const [errored, setErrored] = useState(false)

  const isChannel = room?.type === 'channel'

  useEffect(() => {
    if (!isChannel || !account) return
    let cancelled = false
    setCount(null)
    setErrored(false)
    ;(async () => {
      try {
        const resp = await request(memberList(account, room.id, room.siteId), {})
        if (!cancelled) setCount((resp?.members ?? []).length)
      } catch {
        if (!cancelled) setErrored(true)
      }
    })()
    return () => { cancelled = true }
  }, [isChannel, request, account, room?.id, room?.siteId, refreshKey])

  if (!isChannel) return null

  let label
  if (errored) {
    label = 'members'
  } else if (count == null) {
    label = '…'
  } else {
    label = `${count} ${count === 1 ? 'member' : 'members'}`
  }

  return (
    <button type="button" className="room-members-badge" onClick={onOpen}>
      {label}
    </button>
  )
}
