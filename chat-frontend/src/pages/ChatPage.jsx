import { useEffect, useState } from 'react'
import { useNats } from '../context/NatsContext'
import { useRoomSummaries } from '../context/RoomEventsContext'
import RoomList from '../components/RoomList'
import MessageArea from '../components/MessageArea'
import MessageInput from '../components/MessageInput'
import CreateRoomDialog from '../components/CreateRoomDialog'
import ManageMembersDialog from '../components/ManageMembersDialog'
import LeaveRoomButton from '../components/LeaveRoomButton'
import SearchBar from '../components/SearchBar'
import SearchResultsPane from './SearchResultsPane'
import InRoomSearch from '../components/InRoomSearch'

export default function ChatPage() {
  const { user, disconnect } = useNats()
  const { summaries, setActiveRoom, jumpToMessage } = useRoomSummaries()
  const [selectedRoom, setSelectedRoom] = useState(null)
  const [showCreateRoom, setShowCreateRoom] = useState(false)
  const [showMembers, setShowMembers] = useState(false)
  const [searchQuery, setSearchQuery] = useState(null)
  const [inRoomSearchOpen, setInRoomSearchOpen] = useState(false)

  // Clear selection and any open member dialog if the selected room disappears from summaries
  useEffect(() => {
    if (selectedRoom && !summaries.some((r) => r.id === selectedRoom.id)) {
      setSelectedRoom(null)
      setActiveRoom(null)
      setShowMembers(false)
      setInRoomSearchOpen(false)
    }
  }, [summaries, selectedRoom, setActiveRoom])

  // Ctrl/Cmd-F opens the in-room search side panel; Esc closes it. Lives at
  // ChatPage level so the panel sits as a sibling of MessageArea (Teams-
  // style right rail) rather than overlaying inside the message list.
  useEffect(() => {
    // Disable the in-room shortcut while the full-search pane is showing,
    // otherwise the side panel can open invisibly behind it and pop back
    // when the user closes global search.
    if (!selectedRoom || searchQuery) return
    const handler = (e) => {
      if ((e.ctrlKey || e.metaKey) && (e.key === 'f' || e.key === 'F')) {
        e.preventDefault()
        setInRoomSearchOpen(true)
      } else if (e.key === 'Escape') {
        setInRoomSearchOpen(false)
      }
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [selectedRoom, searchQuery])

  const handleSelectRoom = (room) => {
    setSelectedRoom(room)
    setActiveRoom(room?.id ?? null)
    setShowMembers(false)
    setSearchQuery(null)
    setInRoomSearchOpen(false)
  }

  const handleJumpToMessage = (roomId, messageId) => {
    const room = summaries.find((r) => r.id === roomId)
    if (room) {
      setSelectedRoom(room)
      setActiveRoom(room.id)
      setShowMembers(false)
    }
    setSearchQuery(null)
    if (jumpToMessage) jumpToMessage(roomId, messageId)?.catch?.(() => {})
  }

  const handleInRoomJump = (msgId) => {
    if (selectedRoom && jumpToMessage) {
      jumpToMessage(selectedRoom.id, msgId)?.catch?.(() => {})
    }
  }

  const isChannel = selectedRoom?.type === 'channel'

  return (
    <div className="chat-layout">
      <div className="chat-header">
        <span className="chat-header-title">Chat</span>
        <div className="chat-header-search">
          <SearchBar
            onSelectRoom={handleSelectRoom}
            onEnterSearch={(q) => {
              setSearchQuery(q)
              setInRoomSearchOpen(false)
            }}
          />
        </div>
        {isChannel && (
          <>
            <button
              type="button"
              className="chat-header-logout"
              onClick={() => setShowMembers(true)}
            >
              Members
            </button>
            <LeaveRoomButton room={selectedRoom} />
          </>
        )}
        <span className="chat-header-user">
          {user?.account} &middot; {user?.siteId}
        </span>
        <button className="chat-header-logout" onClick={disconnect}>
          Logout
        </button>
      </div>
      <div className="chat-body">
        <div className="chat-sidebar">
          <RoomList
            selectedRoomId={selectedRoom?.id}
            onSelectRoom={handleSelectRoom}
          />
          <button
            className="create-room-btn"
            onClick={() => setShowCreateRoom(true)}
          >
            + Create Room
          </button>
        </div>
        <div className="chat-main">
          {searchQuery ? (
            <SearchResultsPane
              query={searchQuery}
              onClose={() => setSearchQuery(null)}
              onSelectRoom={handleSelectRoom}
              onJumpToMessage={handleJumpToMessage}
            />
          ) : (
            <div className="chat-main-with-side-panel">
              <div className="chat-main-content">
                <MessageArea room={selectedRoom} />
                <MessageInput room={selectedRoom} />
              </div>
              {inRoomSearchOpen && selectedRoom && (
                <InRoomSearch
                  roomId={selectedRoom.id}
                  onClose={() => setInRoomSearchOpen(false)}
                  onJumpToMessage={handleInRoomJump}
                />
              )}
            </div>
          )}
        </div>
      </div>
      {showCreateRoom && (
        <CreateRoomDialog
          onClose={() => setShowCreateRoom(false)}
          onCreated={(room) => handleSelectRoom(room)}
        />
      )}
      {showMembers && selectedRoom && (
        <ManageMembersDialog
          room={selectedRoom}
          onClose={() => setShowMembers(false)}
        />
      )}
    </div>
  )
}
