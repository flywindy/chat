import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, act, waitFor } from '@testing-library/react'
import { NatsContext } from './NatsContext'
import { RoomEventsProvider, useRoomEvents, useRoomSummaries } from './RoomEventsContext'
import { BUFFER_MODE } from '../lib/roomEventsReducer'
// jumpToMessage / resetToLiveTail tests — see suite below

function mockNats({ request, subscribe, user = { account: 'alice', siteId: 'site-A' } } = {}) {
  return {
    connected: true,
    user,
    error: null,
    connect: vi.fn(),
    request: request ?? vi.fn().mockResolvedValue({ rooms: [] }),
    publish: vi.fn(),
    subscribe: subscribe ?? vi.fn().mockReturnValue({ unsubscribe: vi.fn() }),
    disconnect: vi.fn(),
  }
}

function wrap(ui, nats) {
  return (
    <NatsContext.Provider value={nats}>
      <RoomEventsProvider>{ui}</RoomEventsProvider>
    </NatsContext.Provider>
  )
}

function SummariesProbe() {
  const { summaries } = useRoomSummaries()
  return <div data-testid="count">{summaries.length}</div>
}

function EventsProbe({ roomId }) {
  const { messages, hasLoadedHistory, historyError } = useRoomEvents(roomId)
  return (
    <div>
      <div data-testid="messages">{messages.map((m) => m.id).join(',')}</div>
      <div data-testid="loaded">{String(hasLoadedHistory)}</div>
      <div data-testid="error">{historyError ?? ''}</div>
    </div>
  )
}

describe('RoomEventsProvider', () => {
  beforeEach(() => vi.clearAllMocks())

  it('exposes empty summaries before rooms load', async () => {
    const nats = mockNats()
    render(wrap(<SummariesProbe />, nats))
    expect(screen.getByTestId('count').textContent).toBe('0')
    // Let the empty rooms.list promise settle
    await waitFor(() => expect(nats.request).toHaveBeenCalled())
  })

  it('loadHistory requests msg.history and populates messages', async () => {
    const history = [
      { id: 'm1', roomId: 'a', content: 'old', createdAt: '2026-04-17T10:00:00Z', sender: { account: 'bob' } },
    ]
    const request = vi.fn().mockImplementation((subject) => {
      if (subject.includes('.msg.history')) return Promise.resolve({ messages: [...history] })
      if (subject.endsWith('.rooms.list')) return Promise.resolve({ rooms: [] })
      throw new Error('unexpected subject: ' + subject)
    })
    const nats = mockNats({ request })

    function Trigger() {
      const { messages, loadHistory } = useRoomEvents('a')
      return (
        <div>
          <button onClick={() => loadHistory()}>load</button>
          <div data-testid="messages">{messages.map((m) => m.id).join(',')}</div>
        </div>
      )
    }

    render(wrap(<Trigger />, nats))
    await act(async () => {
      screen.getByText('load').click()
    })
    await waitFor(() => expect(screen.getByTestId('messages').textContent).toBe('m1'))
    expect(request).toHaveBeenCalledWith(
      'chat.user.alice.request.room.a.site-A.msg.history',
      { limit: 50 }
    )
  })

  it('loadHistory surfaces historyError on failure', async () => {
    const request = vi.fn().mockImplementation((subject) => {
      if (subject.includes('.msg.history')) return Promise.reject(new Error('boom'))
      return Promise.resolve({ rooms: [] })
    })
    const nats = mockNats({ request })

    function Trigger() {
      const { loadHistory, historyError } = useRoomEvents('a')
      return (
        <div>
          <button onClick={() => loadHistory().catch(() => {})}>load</button>
          <div data-testid="error">{historyError ?? ''}</div>
        </div>
      )
    }

    render(wrap(<Trigger />, nats))
    await act(async () => {
      screen.getByText('load').click()
    })
    await waitFor(() => expect(screen.getByTestId('error').textContent).toBe('boom'))
  })

  it('useRoomEvents returns a stable loadHistory across renders for the same roomId', async () => {
    const nats = mockNats()
    const captured = []
    function Probe() {
      const { loadHistory } = useRoomEvents('a')
      captured.push(loadHistory)
      return null
    }
    const { rerender } = render(wrap(<Probe />, nats))
    rerender(wrap(<Probe />, nats))
    rerender(wrap(<Probe />, nats))
    expect(captured.length).toBeGreaterThanOrEqual(2)
    for (let i = 1; i < captured.length; i++) {
      expect(captured[i]).toBe(captured[0])
    }
    await waitFor(() => expect(nats.request).toHaveBeenCalled())
  })
})

describe('RoomEventsProvider subscriptions', () => {
  beforeEach(() => vi.clearAllMocks())

  it('fetches rooms on mount and subscribes to user-scoped events', async () => {
    const rooms = [
      { id: 'g1', name: 'general-channel', type: 'channel', siteId: 'site-A', userCount: 3, lastMsgAt: '2026-04-17T10:00:00Z' },
      { id: 'd1', name: 'dm',    type: 'dm',    siteId: 'site-A', userCount: 2, lastMsgAt: '2026-04-17T11:00:00Z' },
    ]
    const request = vi.fn().mockImplementation((subject) => {
      if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms })
      throw new Error('unexpected request: ' + subject)
    })
    const subjects = []
    const subscribe = vi.fn().mockImplementation((subject) => {
      subjects.push(subject)
      return { unsubscribe: vi.fn() }
    })
    const nats = mockNats({ request, subscribe })

    render(wrap(<SummariesProbe />, nats))
    await waitFor(() => expect(screen.getByTestId('count').textContent).toBe('2'))

    expect(subjects).toContain('chat.user.alice.event.room')
    expect(subjects).toContain('chat.user.alice.event.subscription.update')
    expect(subjects).toContain('chat.user.alice.event.room.metadata.update')
    expect(subjects).toContain('chat.room.g1.event')
    expect(subjects).not.toContain('chat.room.d1.event')
  })

  it('applies DM events from the user-scoped subscription', async () => {
    const rooms = [{ id: 'd1', name: 'dm', type: 'dm', siteId: 'site-A', userCount: 2, lastMsgAt: null }]
    const request = vi.fn().mockResolvedValue({ rooms })
    const handlers = new Map()
    const subscribe = vi.fn().mockImplementation((subject, cb) => {
      handlers.set(subject, cb)
      return { unsubscribe: vi.fn() }
    })
    const nats = mockNats({ request, subscribe })

    render(wrap(<EventsProbe roomId="d1" />, nats))
    await waitFor(() => expect(subscribe).toHaveBeenCalled())

    act(() => {
      handlers.get('chat.user.alice.event.room')({
        type: 'new_message',
        roomId: 'd1',
        hasMention: false,
        lastMsgAt: '2026-04-17T12:00:00Z',
        lastMsgId: 'mdm1',
        message: { id: 'mdm1', roomId: 'd1', content: 'hey', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob' } },
      })
    })
    await waitFor(() => expect(screen.getByTestId('messages').textContent).toBe('mdm1'))
  })

  it('opens a new channel subscription when a channel room is added', async () => {
    const request = vi.fn().mockImplementation((subject) => {
      if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms: [] })
      if (subject === 'chat.user.alice.request.rooms.get.g2') {
        return Promise.resolve({ id: 'g2', name: 'new', type: 'channel', siteId: 'site-A', userCount: 1, lastMsgAt: null })
      }
      throw new Error('unexpected request: ' + subject)
    })
    const handlers = new Map()
    const subscribe = vi.fn().mockImplementation((subject, cb) => {
      handlers.set(subject, cb)
      return { unsubscribe: vi.fn() }
    })
    const nats = mockNats({ request, subscribe })

    render(wrap(<SummariesProbe />, nats))
    await waitFor(() => expect(subscribe).toHaveBeenCalled())

    act(() => {
      handlers.get('chat.user.alice.event.subscription.update')({
        action: 'added',
        subscription: { roomId: 'g2' },
      })
    })
    await waitFor(() =>
      expect(subscribe.mock.calls.map((c) => c[0])).toContain('chat.room.g2.event')
    )
    expect(screen.getByTestId('count').textContent).toBe('1')
  })

  it('drops state and unsubscribes on room removal', async () => {
    const rooms = [{ id: 'g1', name: 'g', type: 'channel', siteId: 'site-A', userCount: 2, lastMsgAt: null }]
    const request = vi.fn().mockResolvedValue({ rooms })
    const unsubs = []
    const handlers = new Map()
    const subscribe = vi.fn().mockImplementation((subject, cb) => {
      handlers.set(subject, cb)
      const sub = { unsubscribe: vi.fn() }
      if (subject === 'chat.room.g1.event') unsubs.push(sub)
      return sub
    })
    const nats = mockNats({ request, subscribe })

    render(wrap(<SummariesProbe />, nats))
    await waitFor(() => expect(screen.getByTestId('count').textContent).toBe('1'))

    act(() => {
      handlers.get('chat.user.alice.event.subscription.update')({
        action: 'removed',
        subscription: { roomId: 'g1' },
      })
    })
    await waitFor(() => expect(screen.getByTestId('count').textContent).toBe('0'))
    expect(unsubs[0].unsubscribe).toHaveBeenCalled()
  })

  it('tears down old subscriptions and opens new ones when the user changes', async () => {
    const request = vi.fn().mockResolvedValue({ rooms: [] })
    const subs = []
    const subscribe = vi.fn().mockImplementation((subject) => {
      const sub = { subject, unsubscribe: vi.fn() }
      subs.push(sub)
      return sub
    })
    const aliceNats = mockNats({ request, subscribe, user: { account: 'alice', siteId: 'site-A' } })
    const bobNats = mockNats({ request, subscribe, user: { account: 'bob', siteId: 'site-A' } })

    const { rerender } = render(wrap(<SummariesProbe />, aliceNats))
    await waitFor(() => expect(subs.some((s) => s.subject === 'chat.user.alice.event.room')).toBe(true))
    const aliceSubs = subs.filter((s) => s.subject.includes('alice'))

    rerender(wrap(<SummariesProbe />, bobNats))
    await waitFor(() =>
      expect(subs.some((s) => s.subject === 'chat.user.bob.event.room')).toBe(true)
    )

    for (const s of aliceSubs) {
      expect(s.unsubscribe).toHaveBeenCalled()
    }
  })

  async function setupMentionScenario() {
    const rooms = [{ id: 'g1', name: 'g', type: 'channel', siteId: 'site-A', userCount: 2, lastMsgAt: null }]
    const request = vi.fn().mockResolvedValue({ rooms })
    const handlers = new Map()
    const subscribe = vi.fn().mockImplementation((subject, cb) => {
      handlers.set(subject, cb)
      return { unsubscribe: vi.fn() }
    })
    const nats = mockNats({ request, subscribe })

    const captured = { summaries: null }
    function MentionProbe() {
      const { summaries } = useRoomSummaries()
      captured.summaries = summaries
      return <div data-testid="count">{summaries.length}</div>
    }
    render(wrap(<MentionProbe />, nats))
    await waitFor(() => expect(screen.getByTestId('count').textContent).toBe('1'))
    return { handlers, captured }
  }

  it('computes hasMention from mentions[] for channel events', async () => {
    const { handlers, captured } = await setupMentionScenario()
    act(() => {
      handlers.get('chat.room.g1.event')({
        type: 'new_message',
        roomId: 'g1',
        mentions: [{ account: 'alice', engName: 'Alice' }],
        mentionAll: false,
        lastMsgAt: '2026-04-17T12:00:00Z',
        lastMsgId: 'mg1',
        message: { id: 'mg1', roomId: 'g1', content: '@alice hi', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob' } },
      })
    })
    await waitFor(() => {
      expect(captured.summaries.find((r) => r.id === 'g1')?.hasMention).toBe(true)
    })
  })

  it('does not set hasMention for channel events that do not mention the user', async () => {
    const { handlers, captured } = await setupMentionScenario()
    act(() => {
      handlers.get('chat.room.g1.event')({
        type: 'new_message',
        roomId: 'g1',
        mentions: [{ account: 'charlie' }],
        mentionAll: false,
        lastMsgAt: '2026-04-17T12:00:00Z',
        lastMsgId: 'mg2',
        message: { id: 'mg2', roomId: 'g1', content: '@charlie hi', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob' } },
      })
    })
    await waitFor(() => {
      expect(captured.summaries.find((r) => r.id === 'g1')?.hasMention).toBe(false)
    })
  })

  it('does not dispatch HISTORY_LOADED after the user changes (cancelledRef guard)', async () => {
    // This tests the real bug: user A starts a loadHistory, user switches to B, the
    // cleanup sets cancelledRef=true and the new effect sets it back to false. Without
    // the guard on the dispatch, user A's late resolve would dispatch into user B's state.
    let resolveAliceHistory
    const request = vi.fn().mockImplementation((subject) => {
      if (subject.endsWith('.rooms.list')) return Promise.resolve({ rooms: [] })
      if (subject.includes('alice') && subject.includes('.msg.history')) {
        return new Promise((resolve) => { resolveAliceHistory = resolve })
      }
      if (subject.includes('bob') && subject.includes('.msg.history')) {
        return new Promise(() => {}) // bob's history never resolves in this test
      }
      throw new Error('unexpected: ' + subject)
    })
    const subscribe = vi.fn().mockReturnValue({ unsubscribe: vi.fn() })

    const aliceNats = mockNats({ request, subscribe, user: { account: 'alice', siteId: 'site-A' } })
    const bobNats   = mockNats({ request, subscribe, user: { account: 'bob',   siteId: 'site-A' } })

    // Trigger alice's loadHistory, then switch user to bob mid-flight
    function Trigger() {
      const { loadHistory } = useRoomEvents('a')
      return <button onClick={() => { loadHistory().catch(() => {}) }}>load</button>
    }

    const { rerender } = render(wrap(<Trigger />, aliceNats))
    await waitFor(() => expect(subscribe).toHaveBeenCalled())
    await act(async () => { screen.getByText('load').click() })

    // Switch to bob — this triggers cleanup (cancelledRef=true) then new effect (cancelledRef=false)
    let bobMessages
    function BobProbe() {
      const { messages } = useRoomEvents('a')
      bobMessages = messages
      return null
    }
    rerender(wrap(<BobProbe />, bobNats))
    await waitFor(() => expect(subscribe.mock.calls.some((c) => c[0].includes('bob'))).toBe(true))

    // Now alice's inflight history resolves — the guard must prevent it landing in bob's state
    await act(async () => {
      resolveAliceHistory({ messages: [{ id: 'alice-msg', roomId: 'a', content: 'hi', createdAt: '2026-04-17T10:00:00Z', sender: { account: 'alice' } }] })
      await Promise.resolve()
      await Promise.resolve()
    })

    // Bob's state should be empty — the stale alice dispatch must not have gone through
    expect(bobMessages).toEqual([])
  })
})

describe('RoomEventsProvider jumpToMessage / resetToLiveTail', () => {
  beforeEach(() => vi.clearAllMocks())

  it('jumpToMessage requests msg.surrounding using the room siteId and replaces the buffer', async () => {
    const rooms = [
      { id: 'r1', name: 'general', type: 'channel', siteId: 'site-B', userCount: 2, lastMsgAt: null },
    ]
    const surrounding = [
      { id: 'm10', roomId: 'r1', content: 'before', createdAt: '2026-04-17T11:00:00Z', sender: { account: 'bob' } },
      { id: 'm11', roomId: 'r1', content: 'hit',    createdAt: '2026-04-17T11:01:00Z', sender: { account: 'bob' } },
      { id: 'm12', roomId: 'r1', content: 'after',  createdAt: '2026-04-17T11:02:00Z', sender: { account: 'bob' } },
    ]
    const request = vi.fn().mockImplementation((subject) => {
      if (subject.endsWith('.rooms.list')) return Promise.resolve({ rooms })
      if (subject.includes('.msg.surrounding')) {
        return Promise.resolve({ messages: surrounding })
      }
      throw new Error('unexpected subject: ' + subject)
    })
    const nats = mockNats({ request })

    function Probe() {
      const { messages, focusMessageId, bufferMode, jumpToMessage } = useRoomEvents('r1')
      return (
        <div>
          <button onClick={() => jumpToMessage('m11').catch(() => {})}>jump</button>
          <div data-testid="messages">{messages.map((m) => m.id).join(',')}</div>
          <div data-testid="focus">{focusMessageId ?? ''}</div>
          <div data-testid="mode">{bufferMode}</div>
        </div>
      )
    }

    render(wrap(<Probe />, nats))
    // Wait for rooms list to load so summary is present (so jumpToMessage uses room siteId)
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith('chat.user.alice.request.rooms.list', {})
    )

    await act(async () => {
      screen.getByText('jump').click()
    })

    await waitFor(() =>
      expect(request).toHaveBeenCalledWith(
        'chat.user.alice.request.room.r1.site-B.msg.surrounding',
        { messageId: 'm11' }
      )
    )

    await waitFor(() =>
      expect(screen.getByTestId('messages').textContent).toBe('m10,m11,m12')
    )
    expect(screen.getByTestId('focus').textContent).toBe('m11')
    expect(screen.getByTestId('mode').textContent).toBe(BUFFER_MODE.HISTORICAL)
  })

  it('exposes pendingCount when in historical mode and live messages arrive', async () => {
    const rooms = [
      { id: 'r1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 2, lastMsgAt: null },
    ]
    const handlers = new Map()
    const request = vi.fn().mockImplementation((subject) => {
      if (subject.endsWith('.rooms.list')) return Promise.resolve({ rooms })
      if (subject.includes('.msg.surrounding')) {
        return Promise.resolve({
          messages: [
            { id: 'old', roomId: 'r1', content: 'old', createdAt: '2026-04-17T10:00:00Z', sender: { account: 'bob' } },
          ],
        })
      }
      throw new Error('unexpected subject: ' + subject)
    })
    const subscribe = vi.fn().mockImplementation((subject, cb) => {
      handlers.set(subject, cb)
      return { unsubscribe: vi.fn() }
    })
    const nats = mockNats({ request, subscribe })

    function Probe() {
      const { pendingCount, bufferMode, jumpToMessage, resetToLiveTail, messages } = useRoomEvents('r1')
      return (
        <div>
          <button onClick={() => jumpToMessage('old').catch(() => {})}>jump</button>
          <button onClick={() => resetToLiveTail()}>reset</button>
          <div data-testid="pending">{pendingCount}</div>
          <div data-testid="mode">{bufferMode}</div>
          <div data-testid="messages">{messages.map((m) => m.id).join(',')}</div>
        </div>
      )
    }

    render(wrap(<Probe />, nats))
    await waitFor(() => expect(handlers.has('chat.room.r1.event')).toBe(true))
    await act(async () => { await Promise.resolve(); await Promise.resolve() })

    await act(async () => {
      screen.getByText('jump').click()
      await Promise.resolve()
    })
    await waitFor(() => expect(screen.getByTestId('mode').textContent).toBe(BUFFER_MODE.HISTORICAL))

    act(() => {
      handlers.get('chat.room.r1.event')({
        type: 'new_message',
        roomId: 'r1',
        mentions: [],
        mentionAll: false,
        lastMsgAt: '2026-04-17T12:00:00Z',
        lastMsgId: 'live1',
        message: { id: 'live1', roomId: 'r1', content: 'live!', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob' } },
      })
    })
    await waitFor(() => expect(screen.getByTestId('pending').textContent).toBe('1'))
    expect(screen.getByTestId('messages').textContent).toBe('old')

    act(() => {
      screen.getByText('reset').click()
    })
    await waitFor(() => expect(screen.getByTestId('mode').textContent).toBe(BUFFER_MODE.LIVE))
    expect(screen.getByTestId('messages').textContent).toBe('old,live1')
    expect(screen.getByTestId('pending').textContent).toBe('0')
  })

  it('useRoomSummaries exposes jumpToMessage', async () => {
    const nats = mockNats()
    let captured
    function Probe() {
      const { jumpToMessage } = useRoomSummaries()
      captured = jumpToMessage
      return null
    }
    render(wrap(<Probe />, nats))
    await waitFor(() => expect(nats.request).toHaveBeenCalled())
    expect(typeof captured).toBe('function')
  })
})
