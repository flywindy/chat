// Frontend mirror of the server contract: constants the wire uses and a few
// tightly-coupled predicates that test them. Keep in sync with:
//   pkg/model/subscription.go (Role)
//   pkg/model/member.go       (HistoryMode)
//   room-service/helper.go    (dmExistsError.Error())

export const ROLE_OWNER = 'owner'
export const ROLE_MEMBER = 'member'

export const HISTORY_MODE_ALL = 'all'
export const HISTORY_MODE_NONE = 'none'

// Server's "DM already exists" sync-reply error string. The dedup reply is
// shape {error: this, roomId: existingId} — a 200-with-error that callers
// treat as success (open the existing room).
export const ERR_DM_ALREADY_EXISTS = 'dm already exists'

// Predicate for the DM-exists sync reply. Co-located with the constant
// because they encode the same contract; any caller that wants to treat
// dedup as success should use this rather than re-deriving the check.
export function isDMExistsReply(reply) {
  return reply?.error === ERR_DM_ALREADY_EXISTS && !!reply.roomId
}
