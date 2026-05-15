import { Suspense, lazy, useCallback, useEffect, useState } from 'react'
import { useRoomSummaries } from '@/context/RoomEventsContext'
import { useThreadEvents } from '@/context/ThreadEventsContext'
import AppHeader from './AppHeader/AppHeader'
import Sidebar from './Sidebar/Sidebar'
import ChatPage from './ChatPage/ChatPage'
import LazyFallback from '@/components/shared/LazyFallback'
import './style.css'

// SearchResultsPane and ThreadRightBar are conditional: only rendered
// when the user opens search or expands a thread. Splitting them out
// keeps them out of the initial JS bundle — first paint ships only
// header / sidebar / chat.
const SearchResultsPane = lazy(() => import('./SearchResultsPane/SearchResultsPane'))
const ThreadRightBar = lazy(() => import('./ThreadRightBar/ThreadRightBar'))

export default function MainApp() {
  const { summaries, setActiveRoom, jumpToMessage } = useRoomSummaries()
  const { activeParent } = useThreadEvents()
  const [selectedRoom, setSelectedRoom] = useState(null)
  const [searchQuery, setSearchQuery] = useState(null)

  // Clear selection if the room disappears from summaries (left / kicked).
  useEffect(() => {
    if (selectedRoom && !summaries.some((r) => r.id === selectedRoom.id)) {
      setSelectedRoom(null)
      setActiveRoom(null)
    }
  }, [summaries, selectedRoom, setActiveRoom])

  const handleSelectRoom = useCallback(
    (room) => {
      setSelectedRoom(room)
      setActiveRoom(room?.id ?? null)
      setSearchQuery(null)
    },
    [setActiveRoom]
  )

  const handleEnterSearch = useCallback((q) => setSearchQuery(q), [])

  const handleJumpToMessage = useCallback(
    (roomId, messageId) => {
      const room = summaries.find((r) => r.id === roomId)
      if (room) {
        setSelectedRoom(room)
        setActiveRoom(room.id)
      }
      setSearchQuery(null)
      if (jumpToMessage) jumpToMessage(roomId, messageId)?.catch?.(() => {})
    },
    [summaries, setActiveRoom, jumpToMessage]
  )

  return (
    <div className="app-shell">
      <AppHeader onSelectRoom={handleSelectRoom} onEnterSearch={handleEnterSearch} />
      <div className="app-row">
        <Sidebar selectedRoomId={selectedRoom?.id ?? null} onSelectRoom={handleSelectRoom} />
        {searchQuery ? (
          <Suspense fallback={<LazyFallback />}>
            <SearchResultsPane
              query={searchQuery}
              onClose={() => setSearchQuery(null)}
              onSelectRoom={handleSelectRoom}
              onJumpToMessage={handleJumpToMessage}
            />
          </Suspense>
        ) : (
          <ChatPage selectedRoom={selectedRoom} onSelectRoom={handleSelectRoom} />
        )}
        {activeParent && (
          <Suspense fallback={<LazyFallback />}>
            <ThreadRightBar />
          </Suspense>
        )}
      </div>
    </div>
  )
}
