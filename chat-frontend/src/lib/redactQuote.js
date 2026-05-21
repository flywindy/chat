export const UNAVAILABLE_QUOTE_MSG = 'This message is unavailable'

// Mirrors history-service's redactUnavailableQuote (utils.go) so the live
// broadcast path doesn't leak quotes pointing at messages older than the
// reader's historySharedSince. Returns the original snapshot when the gate
// doesn't apply, or a flat placeholder otherwise.
export function redactInaccessibleQuoteSnapshot(snapshot, historySharedSince) {
  if (!snapshot) return snapshot
  if (!historySharedSince) return snapshot
  const sinceMs = Date.parse(historySharedSince)
  if (Number.isNaN(sinceMs)) return snapshot
  const createdAt = snapshot.createdAt
  if (!createdAt) return { msg: UNAVAILABLE_QUOTE_MSG }
  const createdMs = Date.parse(createdAt)
  if (Number.isNaN(createdMs)) return { msg: UNAVAILABLE_QUOTE_MSG }
  if (createdMs < sinceMs) return { msg: UNAVAILABLE_QUOTE_MSG }
  return snapshot
}
