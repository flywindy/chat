import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import RoomMessageInput from './RoomMessageInput'

const publish = vi.fn()
const dispatch = vi.fn()
vi.mock('@/context/NatsContext', () => ({
  useNats: () => ({ user: { account: 'alice', siteId: 's1' }, publish }),
}))
vi.mock('@/context/RoomEventsContext', () => ({
  useRoomDispatch: () => dispatch,
}))
vi.mock('@/lib/idgen', () => ({ generateMessageID: () => '12345678901234567890' }))
vi.mock('uuid', () => ({ v4: () => 'req-uuid' }))

const room = { id: 'r1', name: 'general', type: 'channel' }

describe('RoomMessageInput', () => {
  beforeEach(() => { publish.mockClear(); dispatch.mockClear() })

  it('renders the form disabled when no room is selected', () => {
    render(<RoomMessageInput room={null} />)
    expect(screen.getByPlaceholderText(/select a room/i)).toBeDisabled()
  })

  it('publishes msg.send on submit with the correct payload', () => {
    render(<RoomMessageInput room={room} />)
    const input = screen.getByPlaceholderText(/general/i)
    fireEvent.change(input, { target: { value: 'hello' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(publish).toHaveBeenCalledWith(
      'chat.user.alice.room.r1.s1.msg.send',
      { id: '12345678901234567890', content: 'hello', requestId: 'req-uuid' }
    )
  })

  it('clears the text after publish', () => {
    render(<RoomMessageInput room={room} />)
    const input = screen.getByPlaceholderText(/general/i)
    fireEvent.change(input, { target: { value: 'hello' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(input).toHaveValue('')
  })

  it('does not publish when text is empty or whitespace', () => {
    render(<RoomMessageInput room={room} />)
    const input = screen.getByPlaceholderText(/general/i)
    fireEvent.change(input, { target: { value: '   ' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(publish).not.toHaveBeenCalled()
  })

  it('forwards quotedTarget and onClearQuote to MessageInputForm', () => {
    const onClearQuote = vi.fn()
    render(
      <RoomMessageInput
        room={room}
        quotedTarget={{ id: 'q', senderName: 'bob', content: 'orig' }}
        onClearQuote={onClearQuote}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: /clear quoted message/i }))
    expect(onClearQuote).toHaveBeenCalled()
  })

  it('includes quotedParentMessageId in the publish payload when quotedTarget is set', () => {
    render(
      <RoomMessageInput
        room={room}
        quotedTarget={{ id: 'q123', senderName: 'bob', content: 'orig' }}
        onClearQuote={() => {}}
      />
    )
    const input = screen.getByPlaceholderText(/general/i)
    fireEvent.change(input, { target: { value: 'reply' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(publish).toHaveBeenCalledWith(
      'chat.user.alice.room.r1.s1.msg.send',
      { id: '12345678901234567890', content: 'reply', requestId: 'req-uuid', quotedParentMessageId: 'q123' }
    )
  })

  it('dispatches an optimistic MESSAGE_SENT_LOCAL alongside the publish', () => {
    render(<RoomMessageInput room={room} />)
    const input = screen.getByPlaceholderText(/general/i)
    fireEvent.change(input, { target: { value: 'hi' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'MESSAGE_SENT_LOCAL',
      roomId: 'r1',
      message: expect.objectContaining({
        id: '12345678901234567890',
        content: 'hi',
        sender: expect.objectContaining({ account: 'alice' }),
        _local: true,
      }),
    }))
  })

  it('optimistic message carries a server-shaped quotedParentMessage snapshot', () => {
    render(
      <RoomMessageInput
        room={room}
        quotedTarget={{ id: 'q123', senderName: 'bob', content: 'orig' }}
        onClearQuote={() => {}}
      />
    )
    const input = screen.getByPlaceholderText(/general/i)
    fireEvent.change(input, { target: { value: 'reply' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    const call = dispatch.mock.calls.find((c) => c[0]?.type === 'MESSAGE_SENT_LOCAL')
    expect(call).toBeDefined()
    expect(call[0].message.quotedParentMessage).toEqual({
      messageId: 'q123',
      sender: { engName: 'bob', account: 'bob' },
      msg: 'orig',
    })
  })
})
