// NATS subject builders — mirrors Go pkg/subject/subject.go
// Keep in sync with the Go definitions when adding new subjects.

export function msgSend(account, roomId, siteId) {
  return `chat.user.${account}.room.${roomId}.${siteId}.msg.send`
}

export function msgHistory(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.msg.history`
}

export function msgSurrounding(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.msg.surrounding`
}

export function roomEvent(roomId) {
  return `chat.room.${roomId}.event`
}

export function roomsList(account) {
  return `chat.user.${account}.request.rooms.list`
}

export function roomsGet(account, roomId) {
  return `chat.user.${account}.request.rooms.get.${roomId}`
}

export function roomsCreate(account) {
  return `chat.user.${account}.request.rooms.create`
}

export function subscriptionUpdate(account) {
  return `chat.user.${account}.event.subscription.update`
}

export function roomMetadataUpdate(account) {
  return `chat.user.${account}.event.room.metadata.update`
}

export function userRoomEvent(account) {
  return `chat.user.${account}.event.room`
}

export function memberAdd(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.member.add`
}

export function memberRemove(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.member.remove`
}

export function memberRoleUpdate(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.member.role-update`
}

export function readReceipt(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.message.read-receipt`
}

export function searchRooms(account) {
  return `chat.user.${account}.request.search.rooms`
}

export function searchMessages(account) {
  return `chat.user.${account}.request.search.messages`
}
