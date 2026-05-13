import { describe, it, expect } from 'vitest'
import { roomPrefix, roomDisplayName, roomFromSearchHit, searchRoomPrefix } from './roomFormat'

describe('roomPrefix', () => {
  it('uses "@ " for dm and botDM rooms, "# " for everything else', () => {
    expect(roomPrefix('dm')).toBe('@ ')
    expect(roomPrefix('botDM')).toBe('@ ')
    expect(roomPrefix('channel')).toBe('# ')
    expect(roomPrefix('unknown')).toBe('# ')
  })
})

describe('searchRoomPrefix', () => {
  it('mirrors roomPrefix without the trailing space', () => {
    expect(searchRoomPrefix('dm')).toBe('@')
    expect(searchRoomPrefix('channel')).toBe('#')
  })
})

describe('roomDisplayName', () => {
  it('returns "" for null / undefined', () => {
    expect(roomDisplayName(null)).toBe('')
    expect(roomDisplayName(undefined)).toBe('')
  })

  it('prefers room.name when set (channels)', () => {
    expect(roomDisplayName({ name: 'frontend', type: 'channel', id: 'r1' })).toBe('frontend')
  })

  it('falls back to room.subscriptionName when room.name is empty (DM with subscription event)', () => {
    expect(
      roomDisplayName({ name: '', subscriptionName: 'bob', type: 'dm', id: 'r-dm' })
    ).toBe('bob')
  })

  it('renders a "(DM)" placeholder for dm / botDM rooms with no name and no subscription name', () => {
    // The initial roomsList payload returns Room not Subscription, so DMs
    // arrive with empty name and no subscriptionName until a subscription.update
    // event lands. Show a placeholder rather than the empty string so the
    // sidebar row remains clickable + identifiable.
    expect(roomDisplayName({ name: '', type: 'dm', id: 'r-dm' })).toBe('(DM)')
    expect(roomDisplayName({ name: '', type: 'botDM', id: 'r-bot' })).toBe('(DM)')
  })

  it('falls back to room.id for an unnamed non-DM room (defensive)', () => {
    expect(roomDisplayName({ name: '', type: 'channel', id: 'r-orphan' })).toBe('r-orphan')
  })
})

describe('roomFromSearchHit', () => {
  it('maps search-hit field names onto the room shape', () => {
    const hit = { roomId: 'r1', roomName: 'frontend', roomType: 'channel', siteId: 'site-A' }
    expect(roomFromSearchHit(hit)).toEqual({
      id: 'r1',
      name: 'frontend',
      type: 'channel',
      siteId: 'site-A',
    })
  })
})
