// The chat API has two message shapes that flow through the client:
//
//   1. Broadcast (`pkg/model.Message`) — published by broadcast-worker.
//      Field names: id, content, userAccount, createdAt.
//
//   2. History (`pkg/model/cassandra.Message`) — read by history-service.
//      Field names: messageId, msg, sender.account, createdAt.
//
// Optimistic dispatches mirror shape #1 (id + content) because the
// broadcast-worker round-trip is the canonical write path. Without a
// normaliser, historical messages arrive with `messageId`/`msg` and
// every `msg.id` access in the UI is `undefined` — Thread / Reply /
// edit / delete all fail silently on anything loaded after a refresh.
//
// Normalise at the load boundary: every fetch-history-style api op
// runs this before resolving so callers downstream of api/ only see
// shape #1.

import type { Message, HistoryMessage } from '../types'

/**
 * Map a single message into broadcast shape. Lenient by design:
 * - returns falsy input untouched (callers historically passed nulls)
 * - if the input is already broadcast-shaped (has `id` + `content`),
 *   returns it as-is. That keeps the function idempotent for tests
 *   and any caller that re-normalises an already-mapped record.
 */
export function normalizeHistoricalMessage<T extends HistoryMessage | Message | null | undefined>(
  m: T,
): T extends null | undefined ? T : Message {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  if (m == null) return m as any
  const maybeMessage = m as Partial<Message> & Partial<HistoryMessage>
  if (maybeMessage.id !== undefined && maybeMessage.content !== undefined) {
    // Already broadcast-shaped — passthrough.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    return maybeMessage as any
  }
  // Cassandra shape → broadcast. Preserve every other field — sender,
  // createdAt, tcount, deleted, editedAt, sysMsgData, type, mentions,
  // quotedParentMessage all use identical names in both shapes; only
  // id/content + thread-parent field names differ.
  const id = maybeMessage.id ?? maybeMessage.messageId ?? ''
  const content = maybeMessage.content ?? maybeMessage.msg ?? ''
  const { messageId, msg, threadParentId, threadParentCreatedAt, ...rest } =
    maybeMessage as HistoryMessage & Message
  const out: Message = { ...rest, id, content }
  if (threadParentId) out.threadParentMessageId = threadParentId
  if (threadParentCreatedAt) out.threadParentMessageCreatedAt = threadParentCreatedAt
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return out as any
}

export function normalizeHistoricalMessages(
  messages: Array<HistoryMessage | Message> | undefined | null,
): Message[] {
  if (!Array.isArray(messages)) return []
  return messages.map((m) => normalizeHistoricalMessage(m))
}
