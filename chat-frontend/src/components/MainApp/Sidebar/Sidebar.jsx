import { Suspense, lazy, useState } from 'react'
import RoomList from './RoomList/RoomList'
import LazyFallback from '@/components/shared/LazyFallback'
import './style.css'

// CreateRoomDialog pulls in MemberPicker + its search + the dialog
// primitives — split it out so it only ships when the user opens it.
const CreateRoomDialog = lazy(() => import('./CreateRoomDialog/CreateRoomDialog'))

export default function Sidebar({ selectedRoomId, onSelectRoom }) {
  const [showCreateRoom, setShowCreateRoom] = useState(false)

  const handleCreated = (room) => {
    setShowCreateRoom(false)
    onSelectRoom(room)
  }

  return (
    <aside className="app-sidebar">
      <RoomList selectedRoomId={selectedRoomId} onSelectRoom={onSelectRoom} />
      <button
        type="button"
        className="create-room-btn"
        onClick={() => setShowCreateRoom(true)}
      >
        + Create Room
      </button>
      {showCreateRoom && (
        <Suspense fallback={<LazyFallback variant="dialog" />}>
          <CreateRoomDialog
            onClose={() => setShowCreateRoom(false)}
            onCreated={handleCreated}
          />
        </Suspense>
      )}
    </aside>
  )
}
