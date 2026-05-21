/** Maximum stored versions per room. Matches Valkey's previous-key grace
 *  slot (one previous in addition to current). */
export const MAX_VERSIONS_PER_ROOM = 2

export type StoredKey = {
  privateKey: Uint8Array
}

export type RoomKeysState = {
  byRoom: Record<string, Record<number, StoredKey>>
}

export const initialRoomKeysState: RoomKeysState = {
  byRoom: {},
}

export type RoomKeysAction =
  | {
      type: 'KEY_RECEIVED'
      roomId: string
      version: number
      privateKey: Uint8Array
    }
  | { type: 'CLEAR_KEYS' }

export function roomKeysReducer(state: RoomKeysState, action: RoomKeysAction): RoomKeysState {
  switch (action.type) {
    case 'KEY_RECEIVED': {
      const existing = state.byRoom[action.roomId]?.[action.version]
      if (existing && bytesEqual(existing.privateKey, action.privateKey)) {
        return state // idempotent no-op
      }
      const room = { ...(state.byRoom[action.roomId] ?? {}) }
      room[action.version] = { privateKey: action.privateKey }
      return {
        ...state,
        byRoom: { ...state.byRoom, [action.roomId]: trimVersions(room) },
      }
    }
    case 'CLEAR_KEYS':
      return initialRoomKeysState
    default:
      return state
  }
}

function trimVersions(room: Record<number, StoredKey>): Record<number, StoredKey> {
  const versions = Object.keys(room).map(Number).sort((a, b) => b - a)
  if (versions.length <= MAX_VERSIONS_PER_ROOM) return room
  const out: Record<number, StoredKey> = {}
  for (const v of versions.slice(0, MAX_VERSIONS_PER_ROOM)) {
    out[v] = room[v]
  }
  return out
}

export function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}
