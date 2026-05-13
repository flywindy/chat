import { useCallback, useEffect, useRef, useState } from 'react'
import { useNats } from '../context/NatsContext'
import { readReceipt } from '../lib/subjects'

function formatReaderName(r) {
  const eng = r.engName || r.account || ''
  return r.chineseName ? `${eng} ${r.chineseName}`.trim() : eng
}

export default function MessageActionMenu({ message, room }) {
  const { user, request } = useNats()
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const [readers, setReaders] = useState(null)
  const [tooltipOpen, setTooltipOpen] = useState(false)
  const rootRef = useRef(null)
  const mountedRef = useRef(true)

  useEffect(() => () => { mountedRef.current = false }, [])

  const close = useCallback(() => {
    setOpen(false)
    setTooltipOpen(false)
    setLoading(false)
    setError(null)
    setReaders(null)
  }, [])

  useEffect(() => {
    if (!open) return
    const onMouseDown = (e) => {
      if (rootRef.current && !rootRef.current.contains(e.target)) close()
    }
    const onKeyDown = (e) => { if (e.key === 'Escape') close() }
    document.addEventListener('mousedown', onMouseDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onMouseDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open, close])

  const isOwnMessage = !!user && message?.sender?.account === user.account

  const handleKebabClick = () => {
    if (open) { close(); return }
    setOpen(true)
    setLoading(true)
    setError(null)
    setReaders(null)
    setTooltipOpen(false)
    const siteId = room?.siteId ?? user.siteId
    const subject = readReceipt(user.account, room.id, siteId)
    Promise.resolve(request(subject, { messageId: message.id }))
      .then((resp) => {
        if (!mountedRef.current) return
        setReaders(resp?.readers ?? [])
        setLoading(false)
      })
      .catch((err) => {
        if (!mountedRef.current) return
        setError(err?.message || 'Failed to load read receipts')
        setLoading(false)
      })
  }

  if (!isOwnMessage || !room?.id) return null

  const X = readers?.length ?? 0
  const Y = Math.max(0, (room?.userCount ?? 1) - 1)
  const hasReaders = readers != null && X > 0

  return (
    <div className="message-action-menu" ref={rootRef}>
      <button
        type="button"
        className="message-action-kebab"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Message actions"
        onClick={handleKebabClick}
      >
        ⋮
      </button>
      {open && (
        <div className="message-action-popover" role="menu">
          {loading && <div className="read-receipt-row read-receipt-loading">Loading…</div>}
          {error && <div className="read-receipt-row read-receipt-error">{error}</div>}
          {!loading && !error && readers != null && (
            hasReaders ? (
              <button
                type="button"
                role="menuitem"
                className="read-receipt-row"
                onMouseEnter={() => setTooltipOpen(true)}
                onMouseLeave={() => setTooltipOpen(false)}
                onFocus={() => setTooltipOpen(true)}
                onBlur={() => setTooltipOpen(false)}
              >
                Read by {X} of {Y}
                {tooltipOpen && (
                  <ul className="read-receipt-tooltip" role="tooltip">
                    {readers.map((r) => (
                      <li key={r.userId}>{formatReaderName(r)}</li>
                    ))}
                  </ul>
                )}
              </button>
            ) : (
              <div
                role="menuitem"
                aria-disabled="true"
                className="read-receipt-row read-receipt-empty"
              >
                Read by {X} of {Y}
              </div>
            )
          )}
        </div>
      )}
    </div>
  )
}
