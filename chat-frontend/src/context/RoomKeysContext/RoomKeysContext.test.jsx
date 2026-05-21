import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, waitFor, act } from '@testing-library/react'
import { RoomKeysProvider, useRoomKeys } from './RoomKeysContext'

vi.mock('@/api', () => ({
  subscribeToRoomKeyEvents: vi.fn(),
}))

vi.mock('@/context/NatsContext', () => ({
  useNats: () => ({
    user: { account: 'alice', id: 'u1' },
    request: vi.fn(),
    subscribe: vi.fn(),
    publish: vi.fn(),
    requestWithAsyncResult: vi.fn(),
    connected: true,
    error: null,
  }),
}))

import { subscribeToRoomKeyEvents } from '@/api'

let lastDecryptHook = null

function Probe() {
  lastDecryptHook = useRoomKeys()
  return null
}

describe('RoomKeysProvider', () => {
  beforeEach(() => {
    lastDecryptHook = null
    vi.mocked(subscribeToRoomKeyEvents).mockReset()
  })

  it('dispatches KEY_RECEIVED for each live event', async () => {
    let savedCb
    vi.mocked(subscribeToRoomKeyEvents).mockImplementation((_n, cb) => {
      savedCb = cb
      return { unsubscribe: vi.fn() }
    })

    render(
      <RoomKeysProvider>
        <Probe />
      </RoomKeysProvider>,
    )

    await waitFor(() => expect(savedCb).toBeDefined())

    act(() => {
      savedCb({
        roomId: 'r2',
        version: 3,
        privateKey: btoa(String.fromCharCode(...new Uint8Array(32).fill(1))),
        timestamp: Date.now(),
      })
    })

    await waitFor(() => expect(lastDecryptHook?.hasKey('r2', 3)).toBe(true))
  })

  it('unsubscribes and clears state on unmount', async () => {
    const unsub = { unsubscribe: vi.fn() }
    vi.mocked(subscribeToRoomKeyEvents).mockReturnValue(unsub)

    const { unmount } = render(
      <RoomKeysProvider>
        <Probe />
      </RoomKeysProvider>,
    )
    unmount()
    expect(unsub.unsubscribe).toHaveBeenCalled()
  })
})
