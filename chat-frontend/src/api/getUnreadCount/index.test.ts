import { describe, it, expect, vi } from 'vitest'
import { getUnreadCount } from './index'
import type { Nats } from '../types'

function fakeNats(request: unknown) {
  return {
    user: { account: 'alice', siteId: 'site-A' },
    request,
  } as unknown as Nats
}

describe('getUnreadCount', () => {
  it('requests the subscription-count subject with {unread:true} and returns the count', async () => {
    const request = vi.fn().mockResolvedValue({ count: 7 })
    const nats = fakeNats(request)

    await expect(getUnreadCount(nats)).resolves.toEqual({ count: 7 })
    expect(request).toHaveBeenCalledWith(
      'chat.user.alice.request.user.site-A.subscription.count',
      { unread: true },
    )
  })

  it('propagates a transport error', async () => {
    const request = vi.fn().mockRejectedValue(new Error('boom'))
    await expect(getUnreadCount(fakeNats(request))).rejects.toThrow('boom')
  })
})
