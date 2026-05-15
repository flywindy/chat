import { useEffect, useRef, useState } from 'react'
import { v4 as uuidv4 } from 'uuid'
import { useNats } from '@/context/NatsContext'
import { useRoomDispatch } from '@/context/RoomEventsContext'
import { sendMessage } from '@/api'
import { generateMessageID } from '@/lib/idgen'
import { roomPrefix, roomDisplayName } from '@/lib/roomFormat'
import MessageInputForm from '@/components/shared/MessageInputForm/MessageInputForm'

export default function RoomMessageInput({ room, quotedTarget, onClearQuote }) {
  const nats = useNats()
  const { user } = nats
  const dispatch = useRoomDispatch()
  const [text, setText] = useState('')
  const inputRef = useRef(null)

  // Auto-focus the input when a quote is freshly staged (e.g., user clicked
  // Reply on a message) so they can start typing immediately without an
  // extra click. Re-runs whenever the staged target changes id.
  useEffect(() => {
    if (quotedTarget?.id) {
      inputRef.current?.focus()
    }
  }, [quotedTarget?.id])

  const placeholder = room
    ? `Message ${roomPrefix(room.type)}${roomDisplayName(room)}`
    : 'Select a room...'
  const disabled = !room || !user

  const handleSubmit = () => {
    if (disabled || !text.trim()) return
    const id = generateMessageID()
    const content = text.trim()
    const payload = {
      id,
      content,
      requestId: uuidv4(),
    }
    // Build an optimistic message that mirrors the server's broadcast shape
    // (so MessageRow / QuotedBlock render it identically). The snapshot uses
    // the server-side cassandra.QuotedParentMessage field names (messageId,
    // sender.engName, msg) so a later broadcast for the same id is a no-op.
    const optimistic = {
      id,
      content,
      createdAt: new Date().toISOString(),
      sender: { account: user.account, engName: user.engName },
      _local: true,
    }
    if (quotedTarget?.id) {
      payload.quotedParentMessageId = quotedTarget.id
      optimistic.quotedParentMessage = {
        messageId: quotedTarget.id,
        sender: { engName: quotedTarget.senderName, account: quotedTarget.senderName },
        msg: quotedTarget.content,
      }
    }
    dispatch({ type: 'MESSAGE_SENT_LOCAL', roomId: room.id, message: optimistic })
    sendMessage(nats, { roomId: room.id, siteId: user.siteId, payload })
    setText('')
    onClearQuote?.()
  }

  return (
    <MessageInputForm
      inputRef={inputRef}
      value={text}
      onChange={setText}
      onSubmit={handleSubmit}
      placeholder={placeholder}
      disabled={disabled}
      quotedTarget={quotedTarget}
      onClearQuote={onClearQuote}
    />
  )
}
