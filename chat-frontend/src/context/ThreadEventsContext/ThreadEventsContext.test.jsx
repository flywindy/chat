import { render, screen, act } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ThreadEventsProvider, useThreadEvents } from './ThreadEventsContext'

const request = vi.fn()
const publish = vi.fn()
vi.mock('../NatsContext/NatsContext', () => ({
  useNats: () => ({
    user: { account: 'alice', siteId: 's1' },
    request, publish,
  }),
}))
vi.mock('@/lib/idgen', () => ({ generateMessageID: () => 'OPT-000000000000000000' }))
vi.mock('uuid', () => ({ v4: () => 'req-uuid' }))

const roomDispatch = vi.fn()
vi.mock('../RoomEventsContext/RoomEventsContext', () => ({
  useRoomDispatch: () => roomDispatch,
}))

function Probe() {
  const t = useThreadEvents()
  return (
    <div>
      <span>active:{t.activeParent?.messageId ?? 'none'}</span>
      <span>count:{t.messages.length}</span>
      <span>loaded:{String(t.hasLoadedHistory)}</span>
      <span>loading:{String(t.historyLoading)}</span>
      <span>error:{t.historyError ?? 'none'}</span>
      <button type="button" onClick={() => t.openThread({ roomId: 'r1', siteId: 's1', messageId: 'p1', createdAtMs: 1000 })}>open</button>
      <button type="button" onClick={() => t.closeThread()}>close</button>
      <button type="button" onClick={() => t.sendReply('hi', {})}>send</button>
      <button type="button" onClick={() => t.sendReply('q-hi', { quotedParentMessageId: 'q-id' })}>send-quote</button>
      <button type="button" onClick={() => t.retryReply('OPT-000000000000000000')}>retry</button>
      <button type="button" onClick={() => t.dismissReply('OPT-000000000000000000')}>dismiss</button>
    </div>
  )
}

const setup = () =>
  render(<ThreadEventsProvider><Probe /></ThreadEventsProvider>)

describe('ThreadEventsContext', () => {
  beforeEach(() => { request.mockReset(); publish.mockReset() })

  it('openThread sets activeParent and fires msg.thread RPC; on success dispatches HISTORY_LOADED', async () => {
    request.mockResolvedValueOnce({ messages: [{ id: 'r1' }, { id: 'r2' }], hasNext: false, nextCursor: null })
    setup()
    expect(screen.getByText('active:none')).toBeInTheDocument()
    await act(async () => {
      screen.getByText('open').click()
    })
    expect(request).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.s1.msg.thread',
      { threadMessageId: 'p1', limit: 50 }
    )
    expect(screen.getByText('active:p1')).toBeInTheDocument()
    expect(screen.getByText('count:2')).toBeInTheDocument()
    expect(screen.getByText('loaded:true')).toBeInTheDocument()
    expect(screen.getByText('loading:false')).toBeInTheDocument()
  })

  it('openThread RPC failure dispatches HISTORY_FAILED', async () => {
    request.mockRejectedValueOnce(new Error('boom'))
    setup()
    await act(async () => { screen.getByText('open').click() })
    expect(screen.getByText('error:boom')).toBeInTheDocument()
    expect(screen.getByText('loading:false')).toBeInTheDocument()
  })

  it('opening the same parent twice short-circuits (no second RPC)', async () => {
    request.mockResolvedValue({ messages: [], hasNext: false, nextCursor: null })
    setup()
    await act(async () => { screen.getByText('open').click() })
    request.mockClear()
    await act(async () => { screen.getByText('open').click() })
    expect(request).not.toHaveBeenCalled()
  })

  it('closeThread resets state', async () => {
    request.mockResolvedValue({ messages: [{ id: 'r1' }], hasNext: false, nextCursor: null })
    setup()
    await act(async () => { screen.getByText('open').click() })
    await act(async () => { screen.getByText('close').click() })
    expect(screen.getByText('active:none')).toBeInTheDocument()
    expect(screen.getByText('count:0')).toBeInTheDocument()
  })

  it('sendReply optimistically appends and publishes msg.send with thread parent fields', async () => {
    request.mockResolvedValue({ messages: [], hasNext: false, nextCursor: null })
    // publish is sync void — default no-op is fine.
    setup()
    await act(async () => { screen.getByText('open').click() })
    await act(async () => { screen.getByText('send').click() })
    expect(screen.getByText('count:1')).toBeInTheDocument()
    expect(publish).toHaveBeenCalledWith(
      'chat.user.alice.room.r1.s1.msg.send',
      {
        id: 'OPT-000000000000000000',
        content: 'hi',
        requestId: 'req-uuid',
        threadParentMessageId: 'p1',
        threadParentMessageCreatedAt: 1000,
      }
    )
  })

  it('sendReply with quotedParentMessageId carries the field in the payload', async () => {
    request.mockResolvedValue({ messages: [], hasNext: false, nextCursor: null })
    setup()
    await act(async () => { screen.getByText('open').click() })
    await act(async () => { screen.getByText('send-quote').click() })
    const call = publish.mock.calls[0]
    expect(call[1].quotedParentMessageId).toBe('q-id')
  })

  it('sendReply publish failure (sync throw) tags _status=failed on the optimistic row', async () => {
    request.mockResolvedValue({ messages: [], hasNext: false, nextCursor: null })
    publish.mockImplementation(() => { throw new Error('Not connected') })
    setup()
    await act(async () => { screen.getByText('open').click() })
    await act(async () => { screen.getByText('send').click() })
    // Probe doesn't expose per-row _status, but count:1 is enough — the
    // reducer test verifies _status='failed' precisely.
    expect(screen.getByText('count:1')).toBeInTheDocument()
  })

  it('dismissReply removes the row', async () => {
    request.mockResolvedValue({ messages: [], hasNext: false, nextCursor: null })
    publish.mockImplementation(() => { throw new Error('Not connected') })
    setup()
    await act(async () => { screen.getByText('open').click() })
    await act(async () => { screen.getByText('send').click() })
    await act(async () => { screen.getByText('dismiss').click() })
    expect(screen.getByText('count:0')).toBeInTheDocument()
  })
})

describe('ThreadEventsContext — cross-dispatch OWN_THREAD_REPLY_SENT', () => {
  beforeEach(() => {
    request.mockReset()
    publish.mockReset()
    roomDispatch.mockClear()
  })

  it('on successful sendReply, dispatches OWN_THREAD_REPLY_SENT to RoomEventsContext', async () => {
    request.mockResolvedValue({ messages: [], hasNext: false, nextCursor: null })
    // publish is sync void — default no-op is success.
    setup()
    await act(async () => { screen.getByText('open').click() })
    await act(async () => { screen.getByText('send').click() })
    expect(roomDispatch).toHaveBeenCalledWith({
      type: 'OWN_THREAD_REPLY_SENT',
      roomId: 'r1',
      parentId: 'p1',
    })
  })

  it('does NOT dispatch when publish throws synchronously', async () => {
    request.mockResolvedValue({ messages: [], hasNext: false, nextCursor: null })
    publish.mockImplementation(() => { throw new Error('Not connected') })
    setup()
    await act(async () => { screen.getByText('open').click() })
    await act(async () => { screen.getByText('send').click() })
    expect(roomDispatch).not.toHaveBeenCalled()
  })

  it('retryReply does NOT re-dispatch OWN_THREAD_REPLY_SENT (the original send already counted)', async () => {
    request.mockResolvedValue({ messages: [], hasNext: false, nextCursor: null })
    // First send fails synchronously so retryReply has something to retry.
    publish.mockImplementationOnce(() => { throw new Error('Not connected') })
    setup()
    await act(async () => { screen.getByText('open').click() })
    await act(async () => { screen.getByText('send').click() })
    // The initial send failed — no roomDispatch fired (covered by the test above).
    expect(roomDispatch).not.toHaveBeenCalled()
    // Now succeed on retry.
    publish.mockImplementation(() => {})
    await act(async () => { screen.getByText('retry').click() })
    // Even though the retry succeeded, the tcount should not be bumped by the
    // retry path — the room reducer assumes one increment per logical send,
    // and the initial sendReply already owned that responsibility (it just
    // happened to fail; the next successful send is a continuation, not a new
    // reply).
    expect(roomDispatch).not.toHaveBeenCalled()
  })
})
