import { useEffect, useRef, useState } from 'react'
import { useThreadEvents } from '@/context/ThreadEventsContext'
import MessageInputForm from '@/components/shared/MessageInputForm/MessageInputForm'

export default function ThreadMessageInput({ quotedTarget, onClearQuote }) {
  const { sendReply, activeParent } = useThreadEvents()
  const [text, setText] = useState('')
  const inputRef = useRef(null)

  // Auto-focus on staged-quote change so a Reply-in-thread click jumps the
  // cursor straight into the input without an extra click.
  useEffect(() => {
    if (quotedTarget?.id) {
      inputRef.current?.focus()
    }
  }, [quotedTarget?.id])

  const handleSubmit = () => {
    if (!text.trim() || !activeParent) return
    // Pass the full snapshot (senderName, content) alongside the id so
    // ThreadEventsContext can build the optimistic quotedParentMessage
    // with a sender label — without it the optimistic snapshot only carries
    // the id and renders as "Unknown" until the server broadcast arrives.
    const opts = quotedTarget
      ? {
          quotedParentMessageId: quotedTarget.id,
          quotedSnapshot: {
            senderName: quotedTarget.senderName,
            content: quotedTarget.content,
          },
        }
      : {}
    const content = text.trim()
    setText('')
    sendReply(content, opts)  // sync; thread context handles _status='failed' on throw
    onClearQuote?.()
  }

  return (
    <MessageInputForm
      inputRef={inputRef}
      value={text}
      onChange={setText}
      onSubmit={handleSubmit}
      placeholder="Reply…"
      disabled={!activeParent}
      quotedTarget={quotedTarget}
      onClearQuote={onClearQuote}
    />
  )
}
