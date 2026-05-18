import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'
import { useUnreadCount } from './useUnreadCount'
import { getUnreadCount } from '@/api'

vi.mock('@/api', () => ({ getUnreadCount: vi.fn() }))

const nats = { user: { account: 'alice', siteId: 'site-A' } }

describe('useUnreadCount', () => {
  beforeEach(() => vi.clearAllMocks())

  it('starts at 0 and resolves to the fetched count on mount', async () => {
    getUnreadCount.mockResolvedValue({ count: 42 })
    const { result } = renderHook(() => useUnreadCount(nats, null))
    expect(result.current).toBe(0)
    await waitFor(() => expect(result.current).toBe(42))
    expect(getUnreadCount).toHaveBeenCalledWith(nats)
  })

  it('re-fetches when the active room changes', async () => {
    getUnreadCount.mockResolvedValue({ count: 42 })
    const { rerender } = renderHook(({ room }) => useUnreadCount(nats, room), {
      initialProps: { room: 'r1' },
    })
    await waitFor(() => expect(getUnreadCount).toHaveBeenCalledTimes(1))
    rerender({ room: 'r2' })
    await waitFor(() => expect(getUnreadCount).toHaveBeenCalledTimes(2))
  })

  it('does not re-fetch when the active room is unchanged', async () => {
    getUnreadCount.mockResolvedValue({ count: 42 })
    const { rerender } = renderHook(({ room }) => useUnreadCount(nats, room), {
      initialProps: { room: 'r1' },
    })
    await waitFor(() => expect(getUnreadCount).toHaveBeenCalledTimes(1))
    rerender({ room: 'r1' })
    await waitFor(() => expect(getUnreadCount).toHaveBeenCalledTimes(1))
  })

  it('drops a stale in-flight result when the room changed before it resolved', async () => {
    let resolveFirst
    getUnreadCount
      .mockImplementationOnce(
        () => new Promise((res) => { resolveFirst = res }),
      )
      .mockResolvedValueOnce({ count: 7 })

    const { result, rerender } = renderHook(
      ({ room }) => useUnreadCount(nats, room),
      { initialProps: { room: 'r1' } },
    )
    rerender({ room: 'r2' })
    await waitFor(() => expect(result.current).toBe(7))

    // The first (stale) fetch resolves late — must be ignored.
    await act(async () => { resolveFirst({ count: 999 }) })
    expect(result.current).toBe(7)
  })
})
