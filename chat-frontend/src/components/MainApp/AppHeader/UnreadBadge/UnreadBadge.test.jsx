import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import UnreadBadge from './UnreadBadge'

const useUnreadCount = vi.fn()
vi.mock('@/context/RoomEventsContext', () => ({
  useUnreadCount: () => useUnreadCount(),
}))

describe('UnreadBadge', () => {
  beforeEach(() => vi.clearAllMocks())

  it('renders nothing when the count is zero', () => {
    useUnreadCount.mockReturnValue(0)
    const { container } = render(<UnreadBadge />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders the count when there are unread messages', () => {
    useUnreadCount.mockReturnValue(5)
    render(<UnreadBadge />)
    const badge = screen.getByLabelText('5 unread messages')
    expect(badge).toHaveTextContent('5')
  })

  it('caps display at 99+ past 99', () => {
    useUnreadCount.mockReturnValue(150)
    render(<UnreadBadge />)
    expect(screen.getByText('99+')).toBeInTheDocument()
  })

  it('singularizes the aria-label for a single unread', () => {
    useUnreadCount.mockReturnValue(1)
    render(<UnreadBadge />)
    expect(screen.getByLabelText('1 unread message')).toBeInTheDocument()
  })

  it('never applies a mention modifier class', () => {
    useUnreadCount.mockReturnValue(2)
    render(<UnreadBadge />)
    expect(screen.getByLabelText('2 unread messages')).not.toHaveClass(
      'unread-badge--mention',
    )
  })
})
