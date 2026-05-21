import { describe, it, expect } from 'vitest'
import { roomKeysReducer, initialRoomKeysState, type RoomKeysState } from './reducer'

const seed = (): RoomKeysState => ({ ...initialRoomKeysState, byRoom: { ...initialRoomKeysState.byRoom } })

describe('roomKeysReducer', () => {
  it('KEY_RECEIVED inserts a single (roomId, version) entry', () => {
    const next = roomKeysReducer(seed(), {
      type: 'KEY_RECEIVED',
      roomId: 'r1',
      version: 2,
      privateKey: new Uint8Array([7]),
    })
    expect(next.byRoom.r1[2].privateKey).toEqual(new Uint8Array([7]))
  })

  it('KEY_RECEIVED is idempotent — same input does not duplicate', () => {
    const action = {
      type: 'KEY_RECEIVED' as const,
      roomId: 'r1',
      version: 2,
      privateKey: new Uint8Array([7]),
    }
    const once = roomKeysReducer(seed(), action)
    const twice = roomKeysReducer(once, action)
    expect(Object.keys(twice.byRoom.r1)).toEqual(['2'])
  })

  it('KEY_RECEIVED keeps at most MAX_VERSIONS_PER_ROOM newest versions', () => {
    let s = seed()
    // Insert versions 1..5 — MAX_VERSIONS_PER_ROOM is 2, so only 4 and 5 should remain.
    for (let v = 1; v <= 5; v++) {
      s = roomKeysReducer(s, {
        type: 'KEY_RECEIVED',
        roomId: 'r1',
        version: v,
        privateKey: new Uint8Array([v]),
      })
    }
    const versions = Object.keys(s.byRoom.r1).map(Number).sort((a, b) => a - b)
    expect(versions).toEqual([4, 5])
  })

  it('CLEAR_KEYS resets state', () => {
    const populated: RoomKeysState = {
      byRoom: { r1: { 1: { privateKey: new Uint8Array([1]) } } },
    }
    const next = roomKeysReducer(populated, { type: 'CLEAR_KEYS' })
    expect(next).toEqual(initialRoomKeysState)
  })
})
