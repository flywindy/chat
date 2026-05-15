import { useNats } from '@/context/NatsContext'
import { leaveRoom } from '@/api'

export default function LeaveRoomButton({ room }) {
  const nats = useNats()

  if (!room || room.type !== 'channel') return null

  const handleClick = async () => {
    if (!window.confirm(`Leave "${room.name}"?`)) return
    try {
      await leaveRoom(nats, { roomId: room.id, siteId: room.siteId })
    } catch (err) {
      window.alert(`Failed to leave: ${err.message}`)
    }
  }

  return (
    <button type="button" className="chat-header-logout" onClick={handleClick}>
      Leave
    </button>
  )
}
