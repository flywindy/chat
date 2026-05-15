import { render, screen } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import MessageList from './MessageList'

vi.mock('./MessageRow/MessageRow', () => ({
  default: ({ message, context }) => (
    <div data-testid={`row-${message.id}`} data-context={context}>
      {message.content}
    </div>
  ),
}))

const msgs = [
  { id: 'a', content: 'first', createdAt: '2026-05-13T10:00:00Z' },
  { id: 'b', content: 'second', createdAt: '2026-05-13T10:01:00Z' },
]

describe('MessageList', () => {
  it('renders one row per message', () => {
    render(<MessageList messages={msgs} hasLoadedHistory context="main" />)
    expect(screen.getByTestId('row-a')).toBeInTheDocument()
    expect(screen.getByTestId('row-b')).toBeInTheDocument()
  })

  it('renders loading placeholder when historyLoading is true', () => {
    render(<MessageList messages={[]} historyLoading context="main" />)
    expect(screen.getByText(/loading/i)).toBeInTheDocument()
  })

  it('renders error placeholder when historyError is set', () => {
    render(<MessageList messages={[]} historyError="oops" context="main" />)
    expect(screen.getByText(/oops|couldn.t/i)).toBeInTheDocument()
  })

  it('renders emptyText when history is loaded and messages is empty', () => {
    render(
      <MessageList
        messages={[]}
        hasLoadedHistory
        context="main"
        emptyText="No messages yet"
      />
    )
    expect(screen.getByText('No messages yet')).toBeInTheDocument()
  })

  it('omits emptyText when messages is non-empty', () => {
    render(
      <MessageList messages={msgs} hasLoadedHistory context="main" emptyText="None" />
    )
    expect(screen.queryByText('None')).not.toBeInTheDocument()
  })

  it('marks the parent row context as thread-parent inside a thread list', () => {
    const list = [{ id: 'p1', content: 'parent' }, { id: 'r1', content: 'reply' }]
    render(
      <MessageList
        messages={list}
        hasLoadedHistory
        context="thread"
        parentMessageId="p1"
      />
    )
    expect(screen.getByTestId('row-p1').getAttribute('data-context')).toBe('thread-parent')
    expect(screen.getByTestId('row-r1').getAttribute('data-context')).toBe('thread')
  })
})
