import { createContext, useCallback, useContext, useEffect, useReducer, useRef } from 'react'
import { subscribeToRoomKeyEvents } from '@/api'
import type { Nats, RoomKeyEvent } from '@/api'
import { useNats } from '@/context/NatsContext'
import { b64decode, importAesKey, decryptRoomMessage } from '@/lib/roomcrypto'
import { bytesEqual, initialRoomKeysState, roomKeysReducer } from './reducer'

type DecryptInput = {
  roomId: string
  version: number
  nonceB64: string
  ciphertextB64: string
}

type RoomKeysContextValue = {
  hasKey(roomId: string, version: number): boolean
  /** Returns null if the key is not (yet) known for that (roomId, version),
   *  or if decryption fails. */
  decrypt(input: DecryptInput): Promise<string | null>
}

const RoomKeysContext = createContext<RoomKeysContextValue | null>(null)

export function useRoomKeys(): RoomKeysContextValue {
  const ctx = useContext(RoomKeysContext)
  if (!ctx) throw new Error('useRoomKeys called outside RoomKeysProvider')
  return ctx
}

export function RoomKeysProvider({ children }: { children: React.ReactNode }) {
  const [state, dispatch] = useReducer(roomKeysReducer, initialRoomKeysState)
  // `useNats()` returns `never` to TS because NatsContext.jsx does
  // `createContext(null)` without annotations. Cast here so downstream
  // callbacks see the proper Nats interface — safe because the
  // provider only renders inside the `connected` gate at App.jsx,
  // where the NATS handshake has populated user/request/etc.
  const nats = useNats() as unknown as Nats

  // CryptoKey cache lives in a ref — imported lazily, not React state.
  // Keyed by `${roomId}|${version}`.
  const aesKeyCacheRef = useRef<Map<string, Promise<CryptoKey>>>(new Map())
  const stateRef = useRef(state)
  stateRef.current = state

  // Keep a live ref to `nats` so long-lived subscription callbacks see
  // the latest connection without forcing the effect to re-run. The
  // effect depends only on user.account (a stable primitive) so it
  // rebuilds subs only when login actually changes — not on every nats
  // context value re-memoisation (see useRoomSubscriptions for prior art).
  const natsRef = useRef(nats)
  natsRef.current = nats

  const userAccount = nats.user?.account ?? null

  useEffect(() => {
    if (!userAccount) return

    const liveNats = natsRef.current

    // TODO: initial keys arrive via the subscription.get* RPCs (user-service
    // responsibility — to be extended in a follow-up). Until then,
    // RoomKeysContext populates from live RoomKeyEvent subscriptions only;
    // reconnecting users re-acquire keys when a rotation or membership change
    // next fires for each room.
    const sub = subscribeToRoomKeyEvents(liveNats, (raw) => {
      const evt = raw as RoomKeyEvent
      if (!evt || typeof evt.roomId !== 'string' || typeof evt.version !== 'number' || typeof evt.privateKey !== 'string') return
      let privateKey: Uint8Array
      try {
        privateKey = b64decode(evt.privateKey)
      } catch (err) {
        // eslint-disable-next-line no-console
        console.warn('roomKeyEvent: invalid base64 privateKey, dropping event', err)
        return
      }
      // Skip evicting the cached AES key when the rebroadcast bytes match
      // the stored bytes — the reducer no-ops on that path, so dropping
      // the derived CryptoKey would force a redundant deriveKey call.
      const existing = stateRef.current.byRoom[evt.roomId]?.[evt.version]
      if (!existing || !bytesEqual(existing.privateKey, privateKey)) {
        aesKeyCacheRef.current.delete(`${evt.roomId}|${evt.version}`)
      }
      dispatch({
        type: 'KEY_RECEIVED',
        roomId: evt.roomId,
        version: evt.version,
        privateKey,
      })
    })

    return () => {
      sub.unsubscribe()
      aesKeyCacheRef.current.clear()
      dispatch({ type: 'CLEAR_KEYS' })
    }
    // userAccount is a stable primitive (set once on login).
    // natsRef is always current — no need to list it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [userAccount])

  const hasKey = useCallback((roomId: string, version: number) => {
    return !!stateRef.current.byRoom[roomId]?.[version]
  }, [])

  const decrypt = useCallback(async ({ roomId, version, nonceB64, ciphertextB64 }: DecryptInput): Promise<string | null> => {
    const entry = stateRef.current.byRoom[roomId]?.[version]
    if (!entry) return null

    const cacheKey = `${roomId}|${version}`
    let pending = aesKeyCacheRef.current.get(cacheKey)
    if (!pending) {
      pending = importAesKey(entry.privateKey)
      aesKeyCacheRef.current.set(cacheKey, pending)
    }
    try {
      const aesKey = await pending
      return await decryptRoomMessage(b64decode(ciphertextB64), b64decode(nonceB64), aesKey)
    } catch (err) {
      // Drop the cached promise so a subsequent decrypt retries derivation
      // instead of awaiting the same rejected promise forever. If the cache
      // entry was already replaced by a newer event between read and catch,
      // only delete our own — peek before evicting.
      if (aesKeyCacheRef.current.get(cacheKey) === pending) {
        aesKeyCacheRef.current.delete(cacheKey)
      }
      // eslint-disable-next-line no-console
      console.warn('roomKeysContext.decrypt failed:', err)
      return null
    }
  }, [])

  const value: RoomKeysContextValue = { hasKey, decrypt }

  return <RoomKeysContext.Provider value={value}>{children}</RoomKeysContext.Provider>
}
