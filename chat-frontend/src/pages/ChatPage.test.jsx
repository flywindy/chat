import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import ChatPage from './ChatPage'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))
vi.mock('../context/RoomEventsContext', () => ({
  useRoomSummaries: vi.fn(),
  useRoomEvents: vi.fn(),
}))
vi.mock('../components/RoomList', () => ({
  default: ({ onSelectRoom }) => (
    <div data-testid="room-list">
      <button onClick={() => onSelectRoom({ id: 'r1', name: 'general', type: 'channel', siteId: 'site-A' })}>
        pick-channel
      </button>
      <button onClick={() => onSelectRoom({ id: 'r2', name: 'bob-dm', type: 'dm', siteId: 'site-A' })}>
        pick-dm
      </button>
    </div>
  ),
}))
// MessageArea now owns the per-room "N members" badge in its header — the
// mock surfaces a clickable members button when a channel is passed in so
// ChatPage's wiring tests (open dialog on click, refresh after close) keep
// working without rendering the real MessageArea + its NATS deps.
vi.mock('../components/MessageArea', () => ({
  default: ({ room, onOpenMembers }) => (
    <div data-testid="message-area">
      {room?.type === 'channel' && (
        <button onClick={onOpenMembers}>{`${room.userCount ?? 0} members`}</button>
      )}
    </div>
  ),
}))
vi.mock('../components/MessageInput', () => ({ default: () => <div data-testid="message-input" /> }))
vi.mock('../components/CreateRoomDialog', () => ({ default: () => null }))
vi.mock('../components/SearchBar', () => ({
  default: ({ onSelectRoom, onEnterSearch }) => (
    <input
      data-testid="search-bar"
      onKeyDown={(e) => {
        if (e.key === 'Enter') onEnterSearch('test-query')
      }}
    />
  ),
}))
vi.mock('./SearchResultsPane', () => ({
  default: ({ onClose, onJumpToMessage }) => (
    <div data-testid="search-results">
      <button onClick={() => onJumpToMessage('r1', 'm-target')}>jump-to-msg</button>
      <button onClick={() => onClose()}>close-search</button>
    </div>
  ),
}))
vi.mock('../components/InRoomSearch', () => ({
  default: ({ roomId }) => (
    <div data-testid="in-room-search" data-room-id={roomId} />
  ),
}))
vi.mock('../components/ThemeToggle', () => ({
  default: () => <button data-testid="theme-toggle" />,
}))
import { useNats } from '../context/NatsContext'
import { useRoomSummaries } from '../context/RoomEventsContext'

beforeEach(() => {
  useNats.mockReset()
  useRoomSummaries.mockReset()
  useNats.mockReturnValue({
    user: { account: 'alice', siteId: 'site-A' },
    request: vi.fn().mockResolvedValue({ status: 'accepted' }),
    disconnect: vi.fn(),
  })
  useRoomSummaries.mockReturnValue({
    summaries: [
      { id: 'r1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 2, lastMsgAt: null, unreadCount: 0, hasMention: false, mentionAll: false },
      { id: 'r2', name: 'bob-dm', type: 'dm', siteId: 'site-A', userCount: 2, lastMsgAt: null, unreadCount: 0, hasMention: false, mentionAll: false },
    ],
    setActiveRoom: vi.fn(),
    jumpToMessage: vi.fn().mockResolvedValue(),
    error: null,
  })
})

describe('ChatPage room actions', () => {
  it('hides the members badge when no room is selected', () => {
    render(<ChatPage />)
    expect(screen.queryByRole('button', { name: /members$/ })).not.toBeInTheDocument()
  })

  it('hides the members badge on a DM room (no manage flow for DMs)', () => {
    render(<ChatPage />)
    fireEvent.click(screen.getByText('pick-dm'))
    expect(screen.queryByRole('button', { name: /members$/ })).not.toBeInTheDocument()
  })

  it('shows the "N members" badge inside the MessageArea header for a channel', () => {
    // The badge now lives in MessageArea's header (replacing the static
    // "N members" span). Leave is reachable via the dialog's roster
    // (self-row → Leave). The global header keeps only chat-wide actions:
    // search, theme, logout.
    render(<ChatPage />)
    fireEvent.click(screen.getByText('pick-channel'))
    expect(screen.getByRole('button', { name: /members$/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^Leave$/ })).not.toBeInTheDocument()
  })

  it('clicking the members badge opens ManageMembersDialog', () => {
    render(<ChatPage />)
    fireEvent.click(screen.getByText('pick-channel'))
    expect(screen.queryByRole('heading', { name: /Manage Members/i })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /members$/ }))
    expect(screen.getByRole('heading', { name: /Manage Members — general/i })).toBeInTheDocument()
  })

  it('closing the dialog dismisses it (and bumps the badge refresh key)', () => {
    render(<ChatPage />)
    fireEvent.click(screen.getByText('pick-channel'))
    fireEvent.click(screen.getByRole('button', { name: /members$/ }))
    fireEvent.click(screen.getByRole('button', { name: /^Close$/ }))
    expect(screen.queryByRole('heading', { name: /Manage Members/i })).not.toBeInTheDocument()
  })
})

describe('ChatPage Ctrl+F in-room search', () => {
  it('opens the side panel when a room is selected', () => {
    render(<ChatPage />)
    fireEvent.click(screen.getByText('pick-channel'))
    expect(screen.queryByTestId('in-room-search')).not.toBeInTheDocument()
    fireEvent.keyDown(window, { key: 'f', ctrlKey: true })
    expect(screen.getByTestId('in-room-search')).toHaveAttribute('data-room-id', 'r1')
  })

  it('Esc closes the side panel', () => {
    render(<ChatPage />)
    fireEvent.click(screen.getByText('pick-channel'))
    fireEvent.keyDown(window, { key: 'f', ctrlKey: true })
    expect(screen.getByTestId('in-room-search')).toBeInTheDocument()
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.queryByTestId('in-room-search')).not.toBeInTheDocument()
  })

  it('does not open while the full-search pane is showing', () => {
    render(<ChatPage />)
    fireEvent.click(screen.getByText('pick-channel'))
    // Open full-search via SearchBar Enter; the Ctrl+F shortcut should be gated.
    fireEvent.keyDown(screen.getByTestId('search-bar'), { key: 'Enter' })
    expect(screen.getByTestId('search-results')).toBeInTheDocument()
    fireEvent.keyDown(window, { key: 'f', ctrlKey: true })
    expect(screen.queryByTestId('in-room-search')).not.toBeInTheDocument()
  })
})

describe('ChatPage jump-to-message wiring', () => {
  it('jumping from search results selects the room, closes search, and calls jumpToMessage', () => {
    const setActiveRoom = vi.fn()
    const jumpToMessage = vi.fn().mockResolvedValue()
    useRoomSummaries.mockReturnValue({
      summaries: [
        { id: 'r1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 2, lastMsgAt: null, unreadCount: 0, hasMention: false, mentionAll: false },
      ],
      setActiveRoom,
      jumpToMessage,
      error: null,
    })
    render(<ChatPage />)
    // Open search results pane via the SearchBar's Enter
    fireEvent.keyDown(screen.getByTestId('search-bar'), { key: 'Enter' })
    expect(screen.getByTestId('search-results')).toBeInTheDocument()

    fireEvent.click(screen.getByText('jump-to-msg'))

    // Search pane is closed
    expect(screen.queryByTestId('search-results')).not.toBeInTheDocument()
    expect(setActiveRoom).toHaveBeenCalledWith('r1')
    expect(jumpToMessage).toHaveBeenCalledWith('r1', 'm-target')
  })
})
