import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import ManageMembersDialog from './ManageMembersDialog'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../context/NatsContext'

const room = { id: 'r1', siteId: 'site-A', name: 'general', type: 'channel' }

beforeEach(() => {
  useNats.mockReset()
  useNats.mockReturnValue({
    user: { account: 'alice', siteId: 'site-A' },
    request: vi.fn().mockResolvedValue({ members: [] }),
    requestWithAsyncResult: vi.fn().mockResolvedValue({ sync: { status: 'accepted' }, async: { status: 'ok' } }),
  })
})

describe('ManageMembersDialog', () => {
  it('shows the Members tab by default with the roster', () => {
    render(<ManageMembersDialog room={room} onClose={vi.fn()} />)
    expect(screen.getByRole('tab', { name: /^Members$/ })).toHaveAttribute('aria-selected', 'true')
    // The roster renders either members or an empty notice — both are fine
    expect(screen.getByRole('heading', { name: /Members/i })).toBeInTheDocument()
  })

  it('switches to the Add tab and renders the picker-based form', () => {
    render(<ManageMembersDialog room={room} onClose={vi.fn()} />)
    fireEvent.click(screen.getByRole('tab', { name: /^Add$/ }))
    expect(screen.getByRole('tab', { name: /^Add$/ })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByLabelText(/Users/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/Orgs/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/Channels/i)).toBeInTheDocument()
  })

  it('calls onClose when Close is clicked', () => {
    const onClose = vi.fn()
    render(<ManageMembersDialog room={room} onClose={onClose} />)
    fireEvent.click(screen.getByRole('button', { name: /Close/i }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('calls onClose when the overlay is clicked', () => {
    const onClose = vi.fn()
    const { container } = render(<ManageMembersDialog room={room} onClose={onClose} />)
    fireEvent.click(container.querySelector('.dialog-overlay'))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('does not call onClose when the dialog body is clicked', () => {
    const onClose = vi.fn()
    const { container } = render(<ManageMembersDialog room={room} onClose={onClose} />)
    fireEvent.click(container.querySelector('.dialog'))
    expect(onClose).not.toHaveBeenCalled()
  })
})
