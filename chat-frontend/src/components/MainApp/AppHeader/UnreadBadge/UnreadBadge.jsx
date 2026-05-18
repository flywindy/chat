import { useUnreadCount } from '@/context/RoomEventsContext'
import './style.css'

/**
 * Header pill showing the app-wide unread total from `useUnreadCount`
 * (sourced from the `subscription.count` RPC).
 *
 * Renders nothing when there's nothing unread; caps the display at
 * `99+`.
 */
export default function UnreadBadge() {
  const total = useUnreadCount()
  if (total <= 0) return null

  const label = `${total} unread message${total === 1 ? '' : 's'}`

  return (
    <span className="unread-badge" aria-label={label} title={label}>
      {total > 99 ? '99+' : total}
    </span>
  )
}
