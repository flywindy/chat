import { useState } from 'react'
import { v4 as uuidv4 } from 'uuid'
import { useNats } from '../context/NatsContext'
import { msgSend } from '../lib/subjects'
import { generateMessageID } from '../lib/idgen'
import { roomPrefix, roomDisplayName } from '../lib/roomFormat'

export default function MessageInput({ room }) {
  const { user, publish } = useNats()
  const [text, setText] = useState('')

  const handleSubmit = (e) => {
    e.preventDefault()
    if (!text.trim() || !room || !user) return

    const account = user.account
    const siteId = user.siteId
    // Message.ID must be a 20-char base62 string per pkg/idgen.IsValidMessageID;
    // message-gatekeeper drops anything else (including UUIDs). requestId
    // remains a hyphenated UUID — it's used for X-Request-ID propagation, not
    // for the message identity.
    publish(msgSend(account, room.id, siteId), {
      id: generateMessageID(),
      content: text.trim(),
      requestId: uuidv4(),
    })

    setText('')
  }

  const handleKeyDown = (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSubmit(e)
    }
  }

  return (
    <form className="message-input" onSubmit={handleSubmit}>
      <input
        type="text"
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={handleKeyDown}
        placeholder={room ? `Message ${roomPrefix(room.type)}${roomDisplayName(room)}` : 'Select a room...'}
        disabled={!room}
      />
      <button type="submit" disabled={!room || !text.trim()}>
        Send
      </button>
    </form>
  )
}
