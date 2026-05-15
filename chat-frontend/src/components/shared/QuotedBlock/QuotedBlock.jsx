import './style.css'
function senderLabel(snapshot) {
  return snapshot.senderName || snapshot.sender?.engName || snapshot.sender?.account || 'Unknown'
}

function excerpt(snapshot) {
  if (snapshot.deleted) return '[message deleted]'
  return snapshot.content || snapshot.msg || ''
}

export default function QuotedBlock({ variant, snapshot, onClear, onClick }) {
  if (!snapshot) return null
  const deleted = !!snapshot.deleted
  // Server-side snapshots (cassandra.QuotedParentMessage) carry `messageId`;
  // optimistic / client-staged snapshots use `id`. Accept either so the
  // click-to-jump works regardless of source.
  const targetId = snapshot.messageId || snapshot.id
  const handleClick = () => {
    if (deleted || !onClick || !targetId) return
    onClick(targetId)
  }

  if (variant === 'chip') {
    return (
      <div className="quoted-block quoted-block-chip">
        <div className="quoted-block-body">
          <div className="quoted-block-sender">{senderLabel(snapshot)}</div>
          <div className="quoted-block-content">{excerpt(snapshot)}</div>
        </div>
        <button
          type="button"
          className="quoted-block-clear"
          aria-label="Clear quoted message"
          onClick={onClear}
        >
          ✕
        </button>
      </div>
    )
  }

  return (
    <div
      className={`quoted-block quoted-block-bubble${deleted ? ' quoted-block-deleted' : ''}`}
      onClick={handleClick}
      role={deleted ? undefined : 'button'}
      tabIndex={deleted ? -1 : 0}
      onKeyDown={(e) => {
        if (deleted) return
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          handleClick()
        }
      }}
    >
      <div className="quoted-block-sender">{senderLabel(snapshot)}</div>
      <div className="quoted-block-content">{excerpt(snapshot)}</div>
    </div>
  )
}
