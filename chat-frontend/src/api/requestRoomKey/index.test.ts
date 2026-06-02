import { describe, expect, it, vi } from 'vitest'
import type { Nats } from '../types'
import { requestRoomKey } from './index'

function makeNats(reply: unknown): Nats {
  return {
    user: { account: 'alice', siteId: 'site-a' } as Nats['user'],
    request: vi.fn().mockResolvedValue(reply),
    publish: vi.fn(),
    subscribe: vi.fn(),
    requestWithAsyncResult: vi.fn(),
  }
}

describe('requestRoomKey', () => {
  it('builds the per-user room subject and forwards an empty payload when version is omitted', async () => {
    const nats = makeNats({ roomId: 'r1', version: 5, privateKey: 'AAAA' })
    const got = await requestRoomKey(nats, { roomId: 'r1', siteId: 'site-a' })
    expect(nats.request).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.site-a.key.get',
      {},
    )
    expect(got).toEqual({ roomId: 'r1', version: 5, privateKey: 'AAAA' })
  })

  it('forwards an explicit version in the payload', async () => {
    const nats = makeNats({ roomId: 'r1', version: 3, privateKey: 'AAAA' })
    await requestRoomKey(nats, { roomId: 'r1', siteId: 'site-a', version: 3 })
    expect(nats.request).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.site-a.key.get',
      { version: 3 },
    )
  })
})
