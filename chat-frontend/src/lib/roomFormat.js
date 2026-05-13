export function roomPrefix(type) {
  return type === 'dm' || type === 'botDM' ? '@ ' : '# '
}

// Resolves the text label for a room in the sidebar / chat header. DM rooms
// have no canonical Room.Name on the server (the server only stores
// subscription-side names for them — each user has their own friendly name
// per DM, typically the counterpart account). When the user-scoped
// subscription event flowed into the reducer it brought that subscription
// name with it; otherwise we fall back to "(DM)" rather than rendering an
// empty string that the user can't act on.
export function roomDisplayName(room) {
  if (!room) return ''
  if (room.name) return room.name
  if (room.subscriptionName) return room.subscriptionName
  if (room.type === 'dm' || room.type === 'botDM') return '(DM)'
  return room.id ?? ''
}

export function roomFromSearchHit(hit) {
  return {
    id: hit.roomId,
    name: hit.roomName,
    type: hit.roomType,
    siteId: hit.siteId,
  }
}

// Backend uses model.RoomType strings ("channel" / "dm" / ...). Same logic as
// roomPrefix above but without the trailing space — search rows place the
// prefix in its own span/div so spacing is handled by CSS, not the string.
export function searchRoomPrefix(roomType) {
  return roomType === 'dm' || roomType === 'botDM' ? '@' : '#'
}
