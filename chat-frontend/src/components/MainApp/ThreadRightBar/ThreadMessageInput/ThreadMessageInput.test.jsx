import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import ThreadMessageInput from './ThreadMessageInput'

const sendReply = vi.fn()  // sync void; throws on failure
vi.mock('@/context/ThreadEventsContext', () => ({
  useThreadEvents: () => ({ sendReply, activeParent: { messageId: 'p1' } }),
}))

describe('ThreadMessageInput', () => {
  beforeEach(() => sendReply.mockClear())

  it('renders with placeholder "Reply…"', () => {
    render(<ThreadMessageInput />)
    expect(screen.getByPlaceholderText('Reply…')).toBeInTheDocument()
  })

  it('Enter calls sendReply with content and clears the textbox', () => {
    render(<ThreadMessageInput />)
    const input = screen.getByPlaceholderText('Reply…')
    fireEvent.change(input, { target: { value: 'in-thread' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(sendReply).toHaveBeenCalledWith('in-thread', {})
    expect(input).toHaveValue('')
  })

  it('does not send empty / whitespace', () => {
    render(<ThreadMessageInput />)
    fireEvent.keyDown(screen.getByPlaceholderText('Reply…'), { key: 'Enter' })
    expect(sendReply).not.toHaveBeenCalled()
  })

  it('passes quotedTarget id through sendReply opts and renders the chip', () => {
    const onClearQuote = vi.fn()
    render(
      <ThreadMessageInput
        quotedTarget={{ id: 'q1', senderName: 'bob', content: 'orig' }}
        onClearQuote={onClearQuote}
      />
    )
    expect(screen.getByText('bob')).toBeInTheDocument()
    const input = screen.getByPlaceholderText('Reply…')
    fireEvent.change(input, { target: { value: 'reply' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(sendReply).toHaveBeenCalledWith('reply', {
      quotedParentMessageId: 'q1',
      quotedSnapshot: { senderName: 'bob', content: 'orig' },
    })
  })

  it('clears the staged quote chip after send (sync — no broker ack)', () => {
    const onClearQuote = vi.fn()
    render(
      <ThreadMessageInput
        quotedTarget={{ id: 'q1', senderName: 'bob', content: 'orig' }}
        onClearQuote={onClearQuote}
      />
    )
    const input = screen.getByPlaceholderText('Reply…')
    fireEvent.change(input, { target: { value: 'reply' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(onClearQuote).toHaveBeenCalled()
  })
})
