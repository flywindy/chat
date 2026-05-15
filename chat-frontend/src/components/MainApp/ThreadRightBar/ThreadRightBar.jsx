import { useEffect, useRef, useState } from 'react'
import { useThreadEvents } from '@/context/ThreadEventsContext'
import ThreadMessageArea from './ThreadMessageArea/ThreadMessageArea'
import ThreadMessageInput from './ThreadMessageInput/ThreadMessageInput'
import './style.css'

export default function ThreadRightBar() {
  const { activeParent, closeThread } = useThreadEvents()
  const [quotedTarget, setQuotedTarget] = useState(null)
  const closeButtonRef = useRef(null)

  useEffect(() => {
    closeButtonRef.current?.focus()
  }, [])

  if (!activeParent) return null

  const handleReply = (msg) => {
    setQuotedTarget({
      id: msg.id,
      senderName: msg.sender?.engName || msg.sender?.account || msg.userAccount || 'Unknown',
      content: msg.content || msg.msg || '',
    })
  }

  const handleKeyDown = (e) => {
    if (e.key !== 'Escape') return
    // Let inputs / textareas keep their own Esc semantics (edit cancel, etc).
    const tag = (e.target.tagName || '').toLowerCase()
    if (tag === 'input' || tag === 'textarea') return
    closeThread()
  }

  return (
    <aside className="thread-rightbar" onKeyDown={handleKeyDown}>
      <header className="thread-header">
        <span className="thread-header-title">Thread</span>
        <button
          type="button"
          ref={closeButtonRef}
          className="thread-header-close"
          aria-label="Close thread"
          onClick={closeThread}
        >
          ✕
        </button>
      </header>
      <ThreadMessageArea onReply={handleReply} />
      <ThreadMessageInput
        quotedTarget={quotedTarget}
        onClearQuote={() => setQuotedTarget(null)}
      />
    </aside>
  )
}
