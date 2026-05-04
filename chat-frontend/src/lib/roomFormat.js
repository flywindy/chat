export function roomPrefix(type) {
  return type === 'dm' ? '@ ' : '# '
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
  return roomType === 'dm' ? '@' : '#'
}
