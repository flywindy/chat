import { useEffect, useRef, useState } from 'react'
import { useNats } from '@/context/NatsContext'
import { useThreadEvents } from '@/context/ThreadEventsContext'
import { useRoomEvents, useRoomDispatch, useRoomSummaries } from '@/context/RoomEventsContext'
import { editMessage, deleteMessage } from '@/api'
import MessageList from '@/components/shared/MessageList/MessageList'
import DeleteConfirmDialog from '@/components/shared/DeleteConfirmDialog/DeleteConfirmDialog'
import TextInputDialog from '@/components/shared/TextInputDialog/TextInputDialog'
import './style.css'

export default function ThreadMessageArea({ onReply }) {
  const { activeParent, messages, hasLoadedHistory, historyLoading, historyError,
          retryReply, dismissReply, dispatch: threadDispatch } = useThreadEvents()
  const { messages: roomMessages } = useRoomEvents(activeParent?.roomId ?? null)
  const roomDispatch = useRoomDispatch()
  // useRoomSummaries exposes the unbound 2-arg jumpToMessage(roomId, msgId);
  // we need that here because the click target's room is activeParent.roomId,
  // which may differ from whatever room the user is currently viewing.
  // We also resolve the room summary here so MessageList can pass it down
  // to MessageActionMenu (the kebab needs room.id + siteId for read-receipts).
  const { summaries, jumpToMessage } = useRoomSummaries()
  const room = (summaries ?? []).find((r) => r.id === activeParent?.roomId) ?? null
  const nats = useNats()
  const { user } = nats
  const bottomRef = useRef(null)
  const [editingMessage, setEditingMessage] = useState(null)
  const [pendingDelete, setPendingDelete] = useState(null)

  // Resolve parent live from main feed buffer; fall back to a thin stub if scrolled out.
  const parent = activeParent
    ? roomMessages.find((m) => m.id === activeParent.messageId) ?? {
        id: activeParent.messageId,
        createdAt: new Date(activeParent.createdAtMs).toISOString(),
        content: '',
      }
    : null

  const combined = parent ? [parent, ...messages] : messages

  // Auto-scroll on history load and on every own optimistic append.
  useEffect(() => {
    if (!hasLoadedHistory) return
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [hasLoadedHistory])

  useEffect(() => {
    // Pin-to-bottom on append. Any "user scrolled up" finesse is out of scope for v1.
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages.length])

  const isParent = (msgId) => activeParent && msgId === activeParent.messageId

  const handleEdit = (msg) => setEditingMessage(msg)
  const handleEditCancel = () => setEditingMessage(null)
  const handleEditSave = (newContent) => {
    if (!editingMessage) return
    editMessage(nats, {
      roomId: activeParent.roomId,
      siteId: activeParent.siteId,
      payload: { messageId: editingMessage.id, newMsg: newContent },
    })
    if (isParent(editingMessage.id)) {
      roomDispatch({
        type: 'MESSAGE_EDITED_LOCAL', roomId: activeParent.roomId,
        messageId: editingMessage.id, content: newContent, editedAt: new Date().toISOString(),
      })
    } else {
      threadDispatch({
        type: 'REPLY_EDITED_LOCAL', messageId: editingMessage.id,
        content: newContent, editedAt: new Date().toISOString(),
      })
    }
    setEditingMessage(null)
  }

  const handleDelete = (msg) => setPendingDelete(msg)
  const handleDeleteCancel = () => setPendingDelete(null)
  const handleDeleteConfirm = () => {
    if (!pendingDelete) return
    deleteMessage(nats, {
      roomId: activeParent.roomId,
      siteId: activeParent.siteId,
      payload: { messageId: pendingDelete.id },
    })
    if (isParent(pendingDelete.id)) {
      roomDispatch({ type: 'MESSAGE_DELETED_LOCAL', roomId: activeParent.roomId, messageId: pendingDelete.id })
    } else {
      threadDispatch({ type: 'REPLY_DELETED_LOCAL', messageId: pendingDelete.id })
    }
    setPendingDelete(null)
  }

  if (!activeParent) return null

  const empty = hasLoadedHistory && !historyLoading && !historyError && messages.length === 0

  return (
    <div className="thread-message-area">
      <MessageList
        messages={combined}
        room={room}
        hasLoadedHistory={hasLoadedHistory}
        historyLoading={historyLoading}
        historyError={historyError}
        context="thread"
        parentMessageId={activeParent.messageId}
        currentUserAccount={user?.account}
        emptyText={empty ? 'No replies yet — be the first to reply' : undefined}
        onReply={onReply}
        onEdit={handleEdit}
        onDelete={handleDelete}
        onRetry={retryReply}
        onDismiss={dismissReply}
        onJumpToMessage={(msgId) => jumpToMessage?.(activeParent.roomId, msgId)?.catch?.(() => {})}
        bottomRef={bottomRef}
        ariaLive="polite"
      />
      {pendingDelete && (
        <DeleteConfirmDialog onConfirm={handleDeleteConfirm} onCancel={handleDeleteCancel} />
      )}
      {editingMessage && (
        <TextInputDialog
          title="Edit message"
          initialValue={editingMessage.content || editingMessage.msg || ''}
          confirmLabel="Save"
          onSave={handleEditSave}
          onCancel={handleEditCancel}
        />
      )}
    </div>
  )
}
