import { useRoomSummaries } from '../context/RoomEventsContext'
import { roomPrefix, roomDisplayName } from '../lib/roomFormat'

function mentionBadge(summary) {
  if (summary.mentionAll) return <span className="room-badge-mention-all">!</span>
  if (summary.hasMention) return <span className="room-badge-mention">@</span>
  return null
}

export default function RoomList({ selectedRoomId, onSelectRoom }) {
  const { summaries, error } = useRoomSummaries()

  return (
    <div className="room-list">
      <div className="room-list-header">Rooms</div>
      {error && <div className="room-list-error">{error}</div>}
      <div className="room-list-items">
        {summaries.map((room) => {
          const isSelected = room.id === selectedRoomId
          const unread = room.unreadCount > 0
          const classes = ['room-item']
          if (isSelected) classes.push('room-item-selected')
          if (unread) classes.push('room-item-unread')
          return (
            <div
              key={room.id}
              className={classes.join(' ')}
              onClick={() => onSelectRoom(room)}
            >
              <span className="room-name">
                {roomPrefix(room.type)}{roomDisplayName(room)}
              </span>
              {mentionBadge(room)}
              <span className="room-meta">{room.userCount}</span>
              {unread && <span className="room-badge-unread">{room.unreadCount}</span>}
            </div>
          )
        })}
        {summaries.length === 0 && !error && (
          <div className="room-list-empty">No rooms yet</div>
        )}
      </div>
    </div>
  )
}
