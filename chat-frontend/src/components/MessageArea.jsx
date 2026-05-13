import { useEffect, useRef } from 'react'
import { useRoomEvents } from '../context/RoomEventsContext'
import { BUFFER_MODE } from '../lib/roomEventsReducer'
import { roomPrefix } from '../lib/roomFormat'
import MessageActionMenu from './MessageActionMenu'

function formatTime(dateStr) {
  const d = new Date(dateStr)
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

function senderName(msg) {
  if (msg.sender) {
    return msg.sender.engName || msg.sender.account || msg.sender.userId || 'Unknown'
  }
  return msg.userAccount || msg.userId || 'Unknown'
}

function messageContent(msg) {
  return msg.content || msg.msg || ''
}

export default function MessageArea({ room }) {
  const {
    messages,
    hasLoadedHistory,
    historyError,
    loadHistory,
    bufferMode,
    pendingCount,
    focusMessageId,
    resetToLiveTail,
    jumpToMessage,
  } = useRoomEvents(room?.id ?? null)
  const bottomRef = useRef(null)
  const listRef = useRef(null)

  useEffect(() => {
    if (!room) return
    loadHistory().catch(() => {})
  }, [room, loadHistory])

  useEffect(() => {
    if (bufferMode === BUFFER_MODE.HISTORICAL) return
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages, bufferMode])

  useEffect(() => {
    if (bufferMode === BUFFER_MODE.LIVE) {
      bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
    }
  }, [bufferMode])

  useEffect(() => {
    if (!focusMessageId || !listRef.current) return
    const el = listRef.current.querySelector(
      `[data-message-id="${focusMessageId}"]`
    )
    if (!el) return
    el.scrollIntoView({ behavior: 'smooth', block: 'center' })
    el.classList.add('flash-jump')
    const timer = setTimeout(() => {
      el.classList.remove('flash-jump')
    }, 2000)
    return () => clearTimeout(timer)
    // messages intentionally omitted: the focus effect should fire only when
    // focusMessageId changes, not on every new live message render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusMessageId])

  if (!room) {
    return (
      <div className="message-area">
        <div className="message-area-empty">Select a room to start chatting</div>
      </div>
    )
  }

  return (
    <div className="message-area">
      <div className="message-area-header">
        <span className="message-area-room-name">
          {roomPrefix(room.type)}{room.name}
        </span>
        <span className="message-area-members">{room.userCount} members</span>
      </div>
      <div className="message-list" ref={listRef}>
        {!hasLoadedHistory && !historyError && <div className="message-loading">Loading messages...</div>}
        {historyError && <div className="message-error">{historyError}</div>}
        {messages.map((msg) => (
          <div key={msg.id} className="message" data-message-id={msg.id}>
            <span className="message-sender">{senderName(msg)}</span>
            <span className="message-time">{formatTime(msg.createdAt)}</span>
            <div className="message-content">{messageContent(msg)}</div>
            <MessageActionMenu message={msg} room={room} />
          </div>
        ))}
        <div ref={bottomRef} />
      </div>
      {bufferMode === BUFFER_MODE.HISTORICAL && pendingCount > 0 && (
        <div className="jump-latest-pill">
          <button type="button" onClick={() => resetToLiveTail()}>
            Jump to latest ({pendingCount} new)
          </button>
        </div>
      )}
    </div>
  )
}
