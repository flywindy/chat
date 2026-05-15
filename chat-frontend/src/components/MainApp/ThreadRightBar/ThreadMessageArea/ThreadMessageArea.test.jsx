import { render, screen, fireEvent, within } from '@testing-library/react'
import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest'
import ThreadMessageArea from './ThreadMessageArea'

beforeAll(() => {
  window.HTMLElement.prototype.scrollIntoView = vi.fn()
})

const activeParent = { roomId: 'r1', siteId: 's1', messageId: 'p1', createdAtMs: 1000 }
const retryReply = vi.fn()
const dismissReply = vi.fn()
const threadDispatch = vi.fn()
const roomDispatch = vi.fn()
const publish = vi.fn()
const jumpToMessage = vi.fn()

vi.mock('@/context/ThreadEventsContext', () => ({
  useThreadEvents: () => ({
    activeParent,
    messages: [
      { id: 'reply-1', content: 'first reply', createdAt: '2026-05-13T10:01:00Z', sender: { account: 'bob' } },
      { id: 'reply-2', content: 'optimistic', _local: true, _status: 'failed', sender: { account: 'alice' } },
    ],
    hasLoadedHistory: true,
    historyLoading: false,
    historyError: null,
    retryReply, dismissReply,
    dispatch: threadDispatch,
  }),
}))
vi.mock('@/context/RoomEventsContext', () => ({
  useRoomEvents: () => ({
    messages: [{ id: 'p1', content: 'parent body', createdAt: '2026-05-13T10:00:00Z', sender: { account: 'alice' } }],
  }),
  useRoomDispatch: () => roomDispatch,
  useRoomSummaries: () => ({ jumpToMessage }),
}))
vi.mock('@/context/NatsContext', () => ({
  useNats: () => ({ user: { account: 'alice', siteId: 's1' }, publish }),
}))
vi.mock('@/components/shared/MessageList/MessageList', () => ({
  default: ({ messages, emptyText, context, onReply, onRetry, onDismiss,
              onEdit, onDelete, onJumpToMessage, historyLoading, historyError }) => (
    <div data-testid="list">
      <span>context:{context}</span>
      <span>count:{messages.length}</span>
      <span>loading:{String(!!historyLoading)}</span>
      <span>error:{historyError ?? 'none'}</span>
      <span>empty:{emptyText ?? 'none'}</span>
      {messages.map((m) => (
        <div key={m.id} data-row={m.id}>{m.content || '[deleted]'}{m._status === 'failed' ? ' (failed)' : ''}</div>
      ))}
      <button type="button" onClick={() => onReply?.({ id: 'reply-1', sender: { account: 'bob' }, content: 'first reply' })}>fire-reply</button>
      <button type="button" onClick={() => onRetry?.('reply-2')}>fire-retry</button>
      <button type="button" onClick={() => onDismiss?.('reply-2')}>fire-dismiss</button>
      <button type="button" onClick={() => onJumpToMessage?.('quoted-orig')}>fire-jump</button>
      <button type="button" onClick={() => onEdit?.({ id: 'reply-1', content: 'original reply' })}>fire-edit-reply</button>
      <button type="button" onClick={() => onDelete?.({ id: 'reply-1', createdAt: '2026-05-13T10:01:00Z' })}>fire-delete-reply</button>
      <button type="button" onClick={() => onEdit?.({ id: 'p1', content: 'original parent' })}>fire-edit-parent</button>
      <button type="button" onClick={() => onDelete?.({ id: 'p1', createdAt: '2026-05-13T10:00:00Z' })}>fire-delete-parent</button>
    </div>
  ),
}))

describe('ThreadMessageArea', () => {
  it('renders the parent as the first row, then the replies', () => {
    render(<ThreadMessageArea onReply={() => {}} />)
    const ids = Array.from(document.querySelectorAll('[data-row]')).map((el) => el.getAttribute('data-row'))
    expect(ids).toEqual(['p1', 'reply-1', 'reply-2'])
  })

  it('passes context="thread" to MessageList', () => {
    render(<ThreadMessageArea onReply={() => {}} />)
    expect(screen.getByText('context:thread')).toBeInTheDocument()
  })

  it('forwards onReply with the reply payload', () => {
    const onReply = vi.fn()
    render(<ThreadMessageArea onReply={onReply} />)
    fireEvent.click(screen.getByText('fire-reply'))
    expect(onReply).toHaveBeenCalled()
  })

  it('forwards retry / dismiss to ThreadEventsContext', () => {
    render(<ThreadMessageArea onReply={() => {}} />)
    fireEvent.click(screen.getByText('fire-retry'))
    expect(retryReply).toHaveBeenCalledWith('reply-2')
    fireEvent.click(screen.getByText('fire-dismiss'))
    expect(dismissReply).toHaveBeenCalledWith('reply-2')
  })

  it('forwards onJumpToMessage to useRoomSummaries().jumpToMessage with activeParent.roomId', () => {
    jumpToMessage.mockClear()
    render(<ThreadMessageArea onReply={() => {}} />)
    fireEvent.click(screen.getByText('fire-jump'))
    expect(jumpToMessage).toHaveBeenCalledWith('r1', 'quoted-orig')
  })
})

describe('ThreadMessageArea — Edit / Delete on thread reply', () => {
  beforeEach(() => { publish.mockClear(); threadDispatch.mockClear(); roomDispatch.mockClear() })

  it('saving the edit dialog on a reply publishes msg.edit and dispatches REPLY_EDITED_LOCAL', () => {
    render(<ThreadMessageArea onReply={() => {}} />)
    fireEvent.click(screen.getByText('fire-edit-reply'))
    const dialog = screen.getByRole('dialog')
    const input = within(dialog).getByDisplayValue('original reply')
    fireEvent.change(input, { target: { value: 'edited' } })
    fireEvent.click(within(dialog).getByRole('button', { name: /save/i }))
    expect(publish).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.s1.msg.edit',
      { messageId: 'reply-1', newMsg: 'edited' }
    )
    expect(threadDispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'REPLY_EDITED_LOCAL', messageId: 'reply-1', content: 'edited',
    }))
  })

  it('confirming delete on a reply publishes msg.delete and dispatches REPLY_DELETED_LOCAL', () => {
    render(<ThreadMessageArea onReply={() => {}} />)
    fireEvent.click(screen.getByText('fire-delete-reply'))
    fireEvent.click(screen.getByRole('button', { name: /^delete$/i }))
    expect(publish).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.s1.msg.delete',
      { messageId: 'reply-1' }
    )
    expect(threadDispatch).toHaveBeenCalledWith({ type: 'REPLY_DELETED_LOCAL', messageId: 'reply-1' })
  })

  it('saving the edit dialog on the PARENT dispatches MESSAGE_EDITED_LOCAL to the ROOM reducer', () => {
    render(<ThreadMessageArea onReply={() => {}} />)
    fireEvent.click(screen.getByText('fire-edit-parent'))
    const dialog = screen.getByRole('dialog')
    const input = within(dialog).getByDisplayValue('original parent')
    fireEvent.change(input, { target: { value: 'edited-parent' } })
    fireEvent.click(within(dialog).getByRole('button', { name: /save/i }))
    expect(publish).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.s1.msg.edit',
      { messageId: 'p1', newMsg: 'edited-parent' }
    )
    expect(roomDispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'MESSAGE_EDITED_LOCAL', roomId: 'r1', messageId: 'p1', content: 'edited-parent',
    }))
    expect(threadDispatch).not.toHaveBeenCalled()
  })

  it('delete on the PARENT dispatches MESSAGE_DELETED_LOCAL to the ROOM reducer', () => {
    render(<ThreadMessageArea onReply={() => {}} />)
    fireEvent.click(screen.getByText('fire-delete-parent'))
    fireEvent.click(screen.getByRole('button', { name: /^delete$/i }))
    expect(roomDispatch).toHaveBeenCalledWith({
      type: 'MESSAGE_DELETED_LOCAL', roomId: 'r1', messageId: 'p1',
    })
  })
})
