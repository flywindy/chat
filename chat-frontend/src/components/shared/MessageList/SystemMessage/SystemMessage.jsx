import './style.css'

/**
 * Decode a base64 `sysMsgData` payload into a JS object.
 *
 * Go's `[]byte` JSON serialisation produces base64. The payload contents
 * are always JSON (room-worker / room-service marshal a typed struct
 * before base64ing). The decode goes via `TextDecoder` so multi-byte
 * UTF-8 (e.g. CJK names in `members_added`) survives intact — a raw
 * `atob().JSON.parse()` would mangle anything outside Latin-1. Returns
 * null on any failure so callers can fall back gracefully.
 */
function decodeSysData(b64) {
  if (!b64 || typeof b64 !== 'string') return null
  try {
    const bin = atob(b64)
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
    const json = new TextDecoder('utf-8').decode(bytes)
    return JSON.parse(json)
  } catch {
    return null
  }
}

function senderLabel(msg) {
  if (msg.sender) {
    return msg.sender.engName || msg.sender.account || msg.userAccount || ''
  }
  return msg.userAccount || ''
}

function describeMembersAdded(data, who) {
  // SysMsgData shape mirrors pkg/model.MembersAdded:
  //   { individuals: [], orgs: [], channels: [{roomId, siteId}], addedUsersCount }
  if (!data) {
    return who ? `${who} added members` : 'Members added'
  }
  const count = data.addedUsersCount ?? (data.individuals?.length ?? 0)
  // Prefer naming a few users when the list is short — otherwise fall back
  // to the count summary so the bubble doesn't blow up width-wise.
  const named = data.individuals ?? []
  const subject = who ? who : 'Someone'
  if (named.length > 0 && named.length <= 3) {
    return `${subject} added ${named.join(', ')}`
  }
  if (count === 1) return `${subject} added 1 member`
  if (count > 1) return `${subject} added ${count} members`
  // No specific count — mention orgs/channels if present.
  const orgCount = data.orgs?.length ?? 0
  const chanCount = data.channels?.length ?? 0
  if (orgCount + chanCount > 0) {
    const parts = []
    if (orgCount > 0) parts.push(`${orgCount} org${orgCount === 1 ? '' : 's'}`)
    if (chanCount > 0) parts.push(`${chanCount} channel${chanCount === 1 ? '' : 's'}`)
    return `${subject} added members from ${parts.join(' and ')}`
  }
  return `${subject} added members`
}

function describeRoomCreated(data, who) {
  // SysMsgData shape mirrors pkg/model.RoomCreated:
  //   { name, users: [], orgs: [], channels: [] }
  if (data?.name) {
    return who ? `${who} created #${data.name}` : `Channel #${data.name} created`
  }
  return who ? `${who} created this channel` : 'Channel created'
}

/**
 * Render a system event row (e.g. members_added, room_created).
 *
 * System messages render as a centered, muted strip — not a regular
 * sender/bubble pair — to make them visually distinct from user
 * conversation. Unknown types fall back to the message's content (if
 * any) so the row never collapses to nothing.
 */
export default function SystemMessage({ message }) {
  const who = senderLabel(message)
  const data = decodeSysData(message.sysMsgData)

  let text
  switch (message.type) {
    case 'members_added':
      text = describeMembersAdded(data, who)
      break
    case 'room_created':
      text = describeRoomCreated(data, who)
      break
    default:
      // Unknown system type — fall back to the human-readable Content the
      // server might have set, or a generic placeholder.
      text = (message.content || message.msg || '').trim() || 'Room event'
  }

  return (
    <div
      className="system-message-row"
      data-message-id={message.id}
      data-system-type={message.type}
    >
      <span className="system-message-text">{text}</span>
      <time className="system-message-time" dateTime={message.createdAt}>
        {new Date(message.createdAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
      </time>
    </div>
  )
}
