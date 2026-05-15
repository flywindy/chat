import { render, screen, fireEvent, within } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import RoomMessageArea from './RoomMessageArea'
import { BUFFER_MODE } from '@/context/RoomEventsContext/reducer'

const loadHistory = vi.fn(async () => {})
const resetToLiveTail = vi.fn()
const jumpToMessage = vi.fn()
const dispatch = vi.fn()

vi.mock('@/context/RoomEventsContext', () => ({
  useRoomEvents: (roomId) => ({
    messages: roomId === 'r1' ? [
      { id: 'a', content: 'hi', createdAt: '2026-05-13T10:00:00Z', sender: { account: 'alice' } },
    ] : [],
    hasLoadedHistory: roomId === 'r1',
    historyError: null,
    loadHistory,
    bufferMode: BUFFER_MODE.LIVE,
    pendingCount: 0,
    focusMessageId: null,
    resetToLiveTail,
    jumpToMessage,
    dispatch,
  }),
}))

const publish = vi.fn()
vi.mock('@/context/NatsContext', () => ({
  useNats: () => ({ user: { account: 'alice', siteId: 's1' }, publish }),
}))

vi.mock('@/components/shared/MessageList/MessageList', () => ({
  default: ({ messages, onEdit, onDelete, onThread, onReply, onJumpToMessage }) => (
    <div data-testid="list">
      <span>count:{messages.length}</span>
      <button type="button" onClick={() => onThread?.({ id: 'a' })}>fire-thread</button>
      <button type="button" onClick={() => onReply?.({ id: 'a' })}>fire-reply</button>
      <button type="button" onClick={() => onJumpToMessage?.('a')}>fire-jump</button>
      <button type="button" onClick={() => onEdit?.({ id: 'a', content: 'original text' })}>fire-edit</button>
      <button type="button" onClick={() => onDelete?.({ id: 'a', createdAt: '2026-05-13T10:00:00Z' })}>fire-delete</button>
    </div>
  ),
}))

const room = { id: 'r1', name: 'general', type: 'channel', siteId: 's', userCount: 1 }

describe('RoomMessageArea', () => {
  beforeEach(() => {
    loadHistory.mockClear()
    resetToLiveTail.mockClear()
    jumpToMessage.mockClear()
  })

  it('calls loadHistory once the room is set', () => {
    render(<RoomMessageArea room={room} onThread={() => {}} onReply={() => {}} />)
    expect(loadHistory).toHaveBeenCalled()
  })

  it('renders a "select a room" placeholder when room is null', () => {
    render(<RoomMessageArea room={null} onThread={() => {}} onReply={() => {}} />)
    expect(screen.getByText(/select a room/i)).toBeInTheDocument()
  })

  it('forwards onThread / onReply to MessageList', () => {
    const onThread = vi.fn()
    const onReply = vi.fn()
    render(<RoomMessageArea room={room} onThread={onThread} onReply={onReply} />)
    fireEvent.click(screen.getByText('fire-thread'))
    expect(onThread).toHaveBeenCalledWith({ id: 'a' })
    fireEvent.click(screen.getByText('fire-reply'))
    expect(onReply).toHaveBeenCalledWith({ id: 'a' })
  })

  it('routes onJumpToMessage to the per-room jumpToMessage(msgId) curry', () => {
    // useRoomEvents(roomId).jumpToMessage is already bound to the room,
    // so it takes (messageId) only. Asserting the single-arg shape.
    render(<RoomMessageArea room={room} onThread={() => {}} onReply={() => {}} />)
    fireEvent.click(screen.getByText('fire-jump'))
    expect(jumpToMessage).toHaveBeenCalledWith('a')
  })
})

describe('RoomMessageArea — Edit (modal)', () => {
  beforeEach(() => { publish.mockClear(); dispatch.mockClear() })

  it('clicking edit opens the TextInputDialog prefilled with the message content', () => {
    render(<RoomMessageArea room={room} />)
    fireEvent.click(screen.getByText('fire-edit'))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByDisplayValue('original text')).toBeInTheDocument()
  })

  it('cancelling the edit dialog leaves no publish and no dispatch', () => {
    render(<RoomMessageArea room={room} />)
    fireEvent.click(screen.getByText('fire-edit'))
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: /cancel/i }))
    expect(publish).not.toHaveBeenCalled()
    expect(dispatch).not.toHaveBeenCalled()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('saving the edit dialog publishes msg.edit and dispatches MESSAGE_EDITED_LOCAL', () => {
    render(<RoomMessageArea room={room} />)
    fireEvent.click(screen.getByText('fire-edit'))
    const dialog = screen.getByRole('dialog')
    const input = within(dialog).getByDisplayValue('original text')
    fireEvent.change(input, { target: { value: 'new text' } })
    fireEvent.click(within(dialog).getByRole('button', { name: /save/i }))
    expect(publish).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.s1.msg.edit',
      { messageId: 'a', newMsg: 'new text' }
    )
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'MESSAGE_EDITED_LOCAL', roomId: 'r1', messageId: 'a', content: 'new text',
    }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})

describe('RoomMessageArea — Delete', () => {
  beforeEach(() => { publish.mockClear(); dispatch.mockClear() })

  it('clicking delete opens the confirm dialog', () => {
    render(<RoomMessageArea room={room} />)
    fireEvent.click(screen.getByText('fire-delete'))
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('cancelling the dialog leaves no RPC and no dispatch', () => {
    render(<RoomMessageArea room={room} />)
    fireEvent.click(screen.getByText('fire-delete'))
    const dialog = screen.getByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', { name: /cancel/i }))
    expect(publish).not.toHaveBeenCalled()
    expect(dispatch).not.toHaveBeenCalled()
  })

  it('confirming publishes msg.delete and dispatches MESSAGE_DELETED_LOCAL', () => {
    render(<RoomMessageArea room={room} />)
    fireEvent.click(screen.getByText('fire-delete'))
    fireEvent.click(screen.getByRole('button', { name: /^delete$/i }))
    expect(publish).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.s1.msg.delete',
      { messageId: 'a' }
    )
    expect(dispatch).toHaveBeenCalledWith({
      type: 'MESSAGE_DELETED_LOCAL', roomId: 'r1', messageId: 'a',
    })
  })
})
