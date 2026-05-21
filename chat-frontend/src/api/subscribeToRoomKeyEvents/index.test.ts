import { describe, it, expect, vi } from 'vitest'
import { subscribeToRoomKeyEvents } from './index'

describe('subscribeToRoomKeyEvents', () => {
  it('subscribes to chat.user.{account}.event.room.key with the given callback', () => {
    const subscribe = vi.fn().mockReturnValue({ unsubscribe: vi.fn() })
    const nats = { subscribe, user: { account: 'alice', id: '' }, request: vi.fn(), publish: vi.fn(), requestWithAsyncResult: vi.fn(), connected: true, error: null }
    const cb = vi.fn()

    const sub = subscribeToRoomKeyEvents(nats as never, cb)

    expect(subscribe).toHaveBeenCalledWith('chat.user.alice.event.room.key', cb)
    expect(sub).toBeDefined()
  })
})
