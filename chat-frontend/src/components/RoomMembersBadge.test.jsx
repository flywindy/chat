import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import RoomMembersBadge from './RoomMembersBadge'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../context/NatsContext'

function setupNats(overrides = {}) {
  const request = vi.fn().mockResolvedValue({ members: [{}, {}, {}] })
  useNats.mockReturnValue({
    user: { account: 'alice', siteId: 'site-A' },
    request,
    ...overrides,
  })
  return { request }
}

const channel = { id: 'r1', siteId: 'site-A', name: 'general', type: 'channel' }
const dm = { id: 'r-dm', siteId: 'site-A', name: '', type: 'dm' }

describe('RoomMembersBadge', () => {
  beforeEach(() => useNats.mockReset())

  it('renders nothing when room is null', () => {
    setupNats()
    const { container } = render(<RoomMembersBadge room={null} onOpen={vi.fn()} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders nothing for DM-type rooms (membership is fixed at create)', () => {
    setupNats()
    const { container } = render(<RoomMembersBadge room={dm} onOpen={vi.fn()} />)
    expect(container.firstChild).toBeNull()
  })

  it('fetches member.list on mount and shows "N members"', async () => {
    const { request } = setupNats()
    render(<RoomMembersBadge room={channel} onOpen={vi.fn()} />)
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith(
        'chat.user.alice.request.room.r1.site-A.member.list',
        expect.anything()
      )
    )
    expect(await screen.findByRole('button', { name: /3 members/i })).toBeInTheDocument()
  })

  it('renders singular "1 member" when the room has exactly one member', async () => {
    setupNats({ request: vi.fn().mockResolvedValue({ members: [{}] }) })
    render(<RoomMembersBadge room={channel} onOpen={vi.fn()} />)
    expect(await screen.findByRole('button', { name: /^1 member$/i })).toBeInTheDocument()
  })

  it('clicking the badge invokes onOpen so the parent can open ManageMembersDialog', async () => {
    setupNats()
    const onOpen = vi.fn()
    render(<RoomMembersBadge room={channel} onOpen={onOpen} />)
    const btn = await screen.findByRole('button', { name: /3 members/i })
    fireEvent.click(btn)
    expect(onOpen).toHaveBeenCalledTimes(1)
  })

  it('refetches when the room changes', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce({ members: [{}, {}] })
      .mockResolvedValueOnce({ members: [{}, {}, {}, {}] })
    setupNats({ request })
    const { rerender } = render(<RoomMembersBadge room={channel} onOpen={vi.fn()} />)
    await screen.findByRole('button', { name: /2 members/i })
    rerender(<RoomMembersBadge room={{ ...channel, id: 'r2' }} onOpen={vi.fn()} />)
    await screen.findByRole('button', { name: /4 members/i })
    expect(request).toHaveBeenCalledTimes(2)
  })

  it('refetches when refreshKey prop bumps (e.g. after the dialog closes)', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce({ members: [{}, {}] })
      .mockResolvedValueOnce({ members: [{}, {}, {}] })
    setupNats({ request })
    const { rerender } = render(<RoomMembersBadge room={channel} onOpen={vi.fn()} refreshKey={0} />)
    await screen.findByRole('button', { name: /2 members/i })
    rerender(<RoomMembersBadge room={channel} onOpen={vi.fn()} refreshKey={1} />)
    await screen.findByRole('button', { name: /3 members/i })
  })

  it('falls back to a generic label on fetch failure rather than disappearing', async () => {
    setupNats({ request: vi.fn().mockRejectedValue(new Error('not subscribed')) })
    render(<RoomMembersBadge room={channel} onOpen={vi.fn()} />)
    expect(await screen.findByRole('button', { name: /members/i })).toBeInTheDocument()
  })
})
