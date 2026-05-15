import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import RoomMessageArea from './RoomMessageArea'
import { BUFFER_MODE } from '@/context/RoomEventsContext/reducer'

// jsdom does not implement scrollIntoView; stub it to avoid the error thrown
// by RoomMessageArea's useEffect that calls bottomRef.current.scrollIntoView.
window.HTMLElement.prototype.scrollIntoView = vi.fn()

const jumpToMessage = vi.fn(async () => {})
vi.mock('@/context/NatsContext', () => ({
  useNats: () => ({ user: { account: 'alice', siteId: 's1' }, publish: vi.fn() }),
}))
vi.mock('@/context/RoomEventsContext', () => ({
  useRoomEvents: () => ({
    messages: [
      {
        id: 'reply-1',
        content: 'reply text',
        createdAt: '2026-05-13T10:30:00Z',
        sender: { account: 'alice' },
        quotedParentMessage: { id: 'orig-1', senderName: 'bob', content: 'the original' },
      },
    ],
    hasLoadedHistory: true,
    historyError: null,
    loadHistory: vi.fn(async () => {}),
    bufferMode: BUFFER_MODE.LIVE,
    pendingCount: 0,
    focusMessageId: null,
    resetToLiveTail: vi.fn(),
    jumpToMessage,
    dispatch: vi.fn(),
  }),
}))

const room = { id: 'r1', name: 'general', type: 'channel', siteId: 's1', userCount: 1 }

describe('RoomMessageArea — click-to-jump', () => {
  beforeEach(() => jumpToMessage.mockClear())

  it('clicking the in-bubble QuotedBlock fires the per-room jumpToMessage(snapshot.id)', () => {
    // useRoomEvents(roomId).jumpToMessage is the curried 1-arg form.
    const { container } = render(<RoomMessageArea room={room} onThread={() => {}} onReply={() => {}} />)
    const bubble = container.querySelector('.quoted-block-bubble')
    expect(bubble).not.toBeNull()
    fireEvent.click(bubble)
    expect(jumpToMessage).toHaveBeenCalledWith('orig-1')
  })
})
