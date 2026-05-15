import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import LeaveRoomButton from './LeaveRoomButton'

vi.mock('@/context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '@/context/NatsContext'

const channelRoom = { id: 'r1', siteId: 'site-A', name: 'general', type: 'channel' }
const dmRoom = { id: 'r2', siteId: 'site-A', name: 'bob-dm', type: 'dm' }

function setup(room, overrides = {}) {
  const request = vi.fn().mockResolvedValue({ status: 'accepted' })
  useNats.mockReturnValue({
    user: { account: 'alice' },
    request,
    ...overrides,
  })
  return { request, ...render(<LeaveRoomButton room={room} />) }
}

describe('LeaveRoomButton', () => {
  beforeEach(() => {
    useNats.mockReset()
    vi.restoreAllMocks()
  })

  it('renders nothing when room type is dm', () => {
    const { container } = setup(dmRoom)
    expect(container.firstChild).toBeNull()
  })

  it('renders nothing when room is null', () => {
    const { container } = setup(null)
    expect(container.firstChild).toBeNull()
  })

  it('submits {roomId, account=self} after the user confirms', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const { request } = setup(channelRoom)
    fireEvent.click(screen.getByRole('button', { name: /Leave/i }))
    await waitFor(() => expect(request).toHaveBeenCalledTimes(1))
    expect(request).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.site-A.member.remove',
      { roomId: 'r1', account: 'alice' }
    )
  })

  it('does nothing when the user cancels the confirm', () => {
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    const { request } = setup(channelRoom)
    fireEvent.click(screen.getByRole('button', { name: /Leave/i }))
    expect(request).not.toHaveBeenCalled()
  })

  it('alerts the user when the request fails', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const alertSpy = vi.spyOn(window, 'alert').mockImplementation(() => {})
    const request = vi.fn().mockRejectedValue(new Error('cannot leave: you are the last owner'))
    setup(channelRoom, { request })
    fireEvent.click(screen.getByRole('button', { name: /Leave/i }))
    await waitFor(() => expect(alertSpy).toHaveBeenCalledTimes(1))
    expect(alertSpy.mock.calls[0][0]).toMatch(/^Failed to leave:.*last owner/)
  })
})
