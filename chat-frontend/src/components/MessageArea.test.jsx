import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import MessageArea from './MessageArea'
import { BUFFER_MODE } from '../lib/roomEventsReducer'

beforeEach(() => {
  window.HTMLElement.prototype.scrollIntoView = vi.fn()
})

vi.mock('../context/RoomEventsContext', () => ({
  useRoomEvents: vi.fn(),
}))

import { useRoomEvents } from '../context/RoomEventsContext'

describe('MessageArea', () => {
  it('shows the empty-state when no room is selected', () => {
    useRoomEvents.mockReturnValue({
      messages: [], hasLoadedHistory: false, historyError: null, loadHistory: vi.fn(),
    })
    render(<MessageArea room={null} />)
    expect(screen.getByText(/Select a room/i)).toBeInTheDocument()
  })

  it('renders messages from the provider', () => {
    useRoomEvents.mockReturnValue({
      messages: [
        { id: 'm1', content: 'hello', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob', engName: 'Bob' } },
      ],
      hasLoadedHistory: true,
      historyError: null,
      loadHistory: vi.fn().mockResolvedValue(),
    })
    render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)
    expect(screen.getByText('hello')).toBeInTheDocument()
    expect(screen.getByText('Bob')).toBeInTheDocument()
  })

  it('surfaces historyError', () => {
    useRoomEvents.mockReturnValue({
      messages: [],
      hasLoadedHistory: false,
      historyError: 'boom',
      loadHistory: vi.fn().mockResolvedValue(),
    })
    render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)
    expect(screen.getByText('boom')).toBeInTheDocument()
  })

  it('calls loadHistory when room changes', () => {
    const loadHistory = vi.fn().mockResolvedValue()
    useRoomEvents.mockReturnValue({
      messages: [], hasLoadedHistory: false, historyError: null, loadHistory,
    })
    render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)
    expect(loadHistory).toHaveBeenCalled()
  })

  it('shows the jump-to-latest pill in historical mode and resets to live on click', () => {
    const resetToLiveTail = vi.fn()
    useRoomEvents.mockReturnValue({
      messages: [],
      hasLoadedHistory: true,
      historyError: null,
      loadHistory: vi.fn().mockResolvedValue(),
      bufferMode: BUFFER_MODE.HISTORICAL,
      pendingCount: 3,
      resetToLiveTail,
    })

    render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)

    const pillButton = screen.getByRole('button', { name: /Jump to latest \(3 new\)/i })
    expect(pillButton).toBeInTheDocument()

    fireEvent.click(pillButton)
    expect(resetToLiveTail).toHaveBeenCalledTimes(1)
  })

  it('hides the jump-to-latest pill when bufferMode is live', () => {
    useRoomEvents.mockReturnValue({
      messages: [],
      hasLoadedHistory: true,
      historyError: null,
      loadHistory: vi.fn().mockResolvedValue(),
      bufferMode: BUFFER_MODE.LIVE,
      pendingCount: 0,
      resetToLiveTail: vi.fn(),
    })

    render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)

    expect(screen.queryByRole('button', { name: /Jump to latest/i })).not.toBeInTheDocument()
  })

  it('renders messages with data-message-id attribute', () => {
    useRoomEvents.mockReturnValue({
      messages: [
        { id: 'm-focus', content: 'target', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob' } },
        { id: 'm-other', content: 'sibling', createdAt: '2026-04-17T12:01:00Z', sender: { account: 'bob' } },
      ],
      hasLoadedHistory: true,
      historyError: null,
      loadHistory: vi.fn().mockResolvedValue(),
      bufferMode: BUFFER_MODE.HISTORICAL,
      pendingCount: 0,
      focusMessageId: null,
      resetToLiveTail: vi.fn(),
    })
    const { container } = render(
      <MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />
    )
    expect(container.querySelector('[data-message-id="m-focus"]')).toBeInTheDocument()
    expect(container.querySelector('[data-message-id="m-other"]')).toBeInTheDocument()
  })

  it('scrolls to and flashes the focus message; flash class clears after 2s', () => {
    vi.useFakeTimers()
    try {
      const scrollSpy = vi.fn()
      window.HTMLElement.prototype.scrollIntoView = scrollSpy
      useRoomEvents.mockReturnValue({
        messages: [
          { id: 'm-focus', content: 'target', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob' } },
          { id: 'm-other', content: 'sibling', createdAt: '2026-04-17T12:01:00Z', sender: { account: 'bob' } },
        ],
        hasLoadedHistory: true,
        historyError: null,
        loadHistory: vi.fn().mockResolvedValue(),
        bufferMode: BUFFER_MODE.HISTORICAL,
        pendingCount: 0,
        focusMessageId: 'm-focus',
        resetToLiveTail: vi.fn(),
      })
      const { container } = render(
        <MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />
      )
      const focusEl = container.querySelector('[data-message-id="m-focus"]')
      expect(focusEl).toBeInTheDocument()
      // Has the flash class right after render
      expect(focusEl.classList.contains('flash-jump')).toBe(true)
      // scrollIntoView called on the focused message
      expect(scrollSpy).toHaveBeenCalled()
      // After 2s the class is removed
      vi.advanceTimersByTime(2000)
      expect(focusEl.classList.contains('flash-jump')).toBe(false)
    } finally {
      vi.useRealTimers()
    }
  })

  it('does not auto-scroll to bottom while in historical mode', () => {
    const scrollSpy = vi.fn()
    window.HTMLElement.prototype.scrollIntoView = scrollSpy
    useRoomEvents.mockReturnValue({
      messages: [
        { id: 'm1', content: 'a', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob' } },
      ],
      hasLoadedHistory: true,
      historyError: null,
      loadHistory: vi.fn().mockResolvedValue(),
      bufferMode: BUFFER_MODE.HISTORICAL,
      pendingCount: 0,
      focusMessageId: null,
      resetToLiveTail: vi.fn(),
    })
    render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)
    // No scroll: bottomRef effect bails on historical mode and there's no focus to scroll to
    expect(scrollSpy).not.toHaveBeenCalled()
  })
})
