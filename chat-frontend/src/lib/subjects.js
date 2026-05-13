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

// roomCreate is the room-service create subject. The site segment is the
// requester's site — room-service queue-subscribes on its own siteID, so a
// caller from site-A always lands its create on the site-A room-service.
export function roomCreate(account, siteId) {
  return `chat.user.${account}.request.room.${siteId}.create`
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

export function memberList(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.member.list`
}

// userResponse is where room-worker publishes AsyncJobResult after finishing
// a deferred operation. The client subscribes here before publishing the
// request and X-Request-ID header so it can match the result back.
export function userResponse(account, requestId) {
  return `chat.user.${account}.response.${requestId}`
}

export function searchRooms(account) {
  return `chat.user.${account}.request.search.rooms`
}

export function searchMessages(account) {
  return `chat.user.${account}.request.search.messages`
}

// orgMembers requests the enriched member list of a single org (sect).
// Used by MemberRoster to expand an org row into its individual members.
// Response shape: { members: [{ id, account, engName, chineseName, siteId }] }.
// Mirrors pkg/subject/subject.go::OrgMembers.
export function orgMembers(account, orgId) {
  return `chat.user.${account}.request.orgs.${orgId}.members`
}
