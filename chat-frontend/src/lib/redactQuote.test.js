import { describe, it, expect } from 'vitest'
import { redactInaccessibleQuoteSnapshot, UNAVAILABLE_QUOTE_MSG } from './redactQuote'

describe('redactInaccessibleQuoteSnapshot', () => {
  it('returns null when snapshot is null or undefined', () => {
    expect(redactInaccessibleQuoteSnapshot(null, '2026-05-19T10:00:00Z')).toBe(null)
    expect(redactInaccessibleQuoteSnapshot(undefined, '2026-05-19T10:00:00Z')).toBe(undefined)
  })

  it('returns the original snapshot when no historySharedSince gate is set', () => {
    const snap = { messageId: 'q1', msg: 'secret', createdAt: '2026-01-01T00:00:00Z' }
    expect(redactInaccessibleQuoteSnapshot(snap, null)).toBe(snap)
    expect(redactInaccessibleQuoteSnapshot(snap, undefined)).toBe(snap)
    expect(redactInaccessibleQuoteSnapshot(snap, '')).toBe(snap)
  })

  it('redacts when the quote createdAt is strictly before historySharedSince', () => {
    const snap = {
      messageId: 'q1',
      roomId: 'r1',
      sender: { engName: 'Bob', account: 'bob' },
      createdAt: '2026-05-19T09:00:00Z',
      msg: 'top secret',
      messageLink: 'https://chat/r1/q1',
    }
    const out = redactInaccessibleQuoteSnapshot(snap, '2026-05-19T10:00:00Z')
    expect(out).not.toBe(snap)
    expect(out).toEqual({ msg: UNAVAILABLE_QUOTE_MSG })
  })

  it('keeps the original when quote createdAt equals historySharedSince', () => {
    const snap = { messageId: 'q1', msg: 'fine', createdAt: '2026-05-19T10:00:00Z' }
    expect(redactInaccessibleQuoteSnapshot(snap, '2026-05-19T10:00:00Z')).toBe(snap)
  })

  it('keeps the original when quote createdAt is after historySharedSince', () => {
    const snap = { messageId: 'q1', msg: 'fine', createdAt: '2026-05-19T11:00:00Z' }
    expect(redactInaccessibleQuoteSnapshot(snap, '2026-05-19T10:00:00Z')).toBe(snap)
  })

  it('redacts conservatively when the quote has no createdAt', () => {
    const snap = { messageId: 'q1', msg: 'no-time' }
    const out = redactInaccessibleQuoteSnapshot(snap, '2026-05-19T10:00:00Z')
    expect(out).toEqual({ msg: UNAVAILABLE_QUOTE_MSG })
  })

  it('redacts when createdAt is unparseable', () => {
    const snap = { messageId: 'q1', msg: 'bad-time', createdAt: 'not-a-date' }
    const out = redactInaccessibleQuoteSnapshot(snap, '2026-05-19T10:00:00Z')
    expect(out).toEqual({ msg: UNAVAILABLE_QUOTE_MSG })
  })
})
