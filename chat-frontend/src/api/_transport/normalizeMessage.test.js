import { describe, it, expect } from 'vitest'
import { normalizeHistoricalMessage, normalizeHistoricalMessages } from './normalizeMessage'

describe('normalizeHistoricalMessage', () => {
  it('returns broadcast-shaped messages unchanged', () => {
    const broadcast = { id: 'm1', content: 'hello', sender: { account: 'alice' } }
    expect(normalizeHistoricalMessage(broadcast)).toBe(broadcast)
  })

  it('maps cassandra.Message messageId → id', () => {
    const cass = { messageId: 'm1', msg: 'hello', sender: { account: 'alice' } }
    const out = normalizeHistoricalMessage(cass)
    expect(out.id).toBe('m1')
  })

  it('maps cassandra.Message msg → content', () => {
    const cass = { messageId: 'm1', msg: 'hello world' }
    expect(normalizeHistoricalMessage(cass).content).toBe('hello world')
  })

  it('preserves all other fields (createdAt, sender, quotedParentMessage, tcount)', () => {
    const cass = {
      messageId: 'm1',
      msg: 'hi',
      createdAt: '2026-05-13T10:42:00Z',
      sender: { account: 'alice', engName: 'Alice' },
      quotedParentMessage: { messageId: 'm0', msg: 'orig', sender: { account: 'bob' } },
      tcount: 3,
      editedAt: '2026-05-13T10:43:00Z',
      deleted: false,
    }
    const out = normalizeHistoricalMessage(cass)
    expect(out.id).toBe('m1')
    expect(out.content).toBe('hi')
    expect(out.createdAt).toBe('2026-05-13T10:42:00Z')
    expect(out.sender).toEqual({ account: 'alice', engName: 'Alice' })
    expect(out.quotedParentMessage).toEqual({ messageId: 'm0', msg: 'orig', sender: { account: 'bob' } })
    expect(out.tcount).toBe(3)
    expect(out.editedAt).toBe('2026-05-13T10:43:00Z')
  })

  it('defaults content to empty string when neither content nor msg is set', () => {
    expect(normalizeHistoricalMessage({ messageId: 'm1' }).content).toBe('')
  })

  it('handles null / undefined input gracefully', () => {
    expect(normalizeHistoricalMessage(null)).toBeNull()
    expect(normalizeHistoricalMessage(undefined)).toBeUndefined()
  })
})

describe('normalizeHistoricalMessages (array)', () => {
  it('maps each entry through the single-message normalizer', () => {
    const arr = [
      { messageId: 'm1', msg: 'one' },
      { id: 'm2', content: 'two' },
      { messageId: 'm3', msg: 'three' },
    ]
    const out = normalizeHistoricalMessages(arr)
    expect(out.map((m) => m.id)).toEqual(['m1', 'm2', 'm3'])
    expect(out.map((m) => m.content)).toEqual(['one', 'two', 'three'])
  })

  it('returns [] for non-array input', () => {
    expect(normalizeHistoricalMessages(null)).toEqual([])
    expect(normalizeHistoricalMessages(undefined)).toEqual([])
  })
})
