import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, act, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { NatsContext } from '../NatsContext/NatsContext'
import { RoomEventsProvider, useRoomEvents, useRoomSummaries, useSidebarSections, useSubscription } from './RoomEventsContext'
import { BUFFER_MODE } from './reducer'
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
      if (subject.includes('.subscription.get')) return Promise.resolve({ subscriptions: [] })
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
      if (subject.includes('.subscription.get')) return Promise.resolve({ subscriptions: [] })
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
      if (subject.includes('.subscription.get')) return Promise.resolve({ subscriptions: [] })
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
      if (subject.includes('.subscription.get')) return Promise.resolve({ subscriptions: [] })
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
      if (subject.includes('.subscription.get')) return Promise.resolve({ subscriptions: [] })
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
      if (subject.includes('.subscription.get')) return Promise.resolve({ subscriptions: [] })
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

describe('RoomEventsProvider message.read wiring', () => {
  beforeEach(() => vi.clearAllMocks())

  function readSubjectFor(roomId, siteId = 'site-A') {
    return `chat.user.alice.request.room.${roomId}.${siteId}.message.read`
  }

  function setupWithRooms(rooms) {
    const request = vi.fn().mockImplementation((subject) => {
      if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms })
      // every other request (incl. message.read) — return ok
      return Promise.resolve({})
    })
    const handlers = new Map()
    const subscribe = vi.fn().mockImplementation((subject, cb) => {
      handlers.set(subject, cb)
      return { unsubscribe: vi.fn() }
    })
    const nats = mockNats({ request, subscribe })
    return { nats, request, handlers }
  }

  it('fires message.read when setActiveRoom is called with a non-null id', async () => {
    const rooms = [
      { id: 'g1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 3, lastMsgAt: null },
    ]
    const { nats, request } = setupWithRooms(rooms)
    let captured
    function Probe() {
      const { setActiveRoom } = useRoomSummaries()
      captured = setActiveRoom
      return null
    }
    render(wrap(<Probe />, nats))
    await waitFor(() => expect(request).toHaveBeenCalledWith('chat.user.alice.request.rooms.list', {}))

    act(() => { captured('g1') })

    expect(request).toHaveBeenCalledWith(readSubjectFor('g1'), {})
  })

  it('does NOT fire message.read when setActiveRoom is called with null', async () => {
    const { nats, request } = setupWithRooms([])
    let captured
    function Probe() {
      const { setActiveRoom } = useRoomSummaries()
      captured = setActiveRoom
      return null
    }
    render(wrap(<Probe />, nats))
    await waitFor(() => expect(request).toHaveBeenCalled())

    request.mockClear()
    act(() => { captured(null) })

    const subjects = request.mock.calls.map((c) => c[0])
    expect(subjects.some((s) => s.endsWith('.message.read'))).toBe(false)
  })

  it('fires message.read when a new_message arrives in the active channel room (after the trailing debounce)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const rooms = [
        { id: 'g1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 3, lastMsgAt: null },
      ]
      const { nats, request, handlers } = setupWithRooms(rooms)
      let setActive
      function Probe() {
        const { setActiveRoom } = useRoomSummaries()
        setActive = setActiveRoom
        return null
      }
      render(wrap(<Probe />, nats))
      await waitFor(() => expect(handlers.has('chat.room.g1.event')).toBe(true))

      act(() => { setActive('g1') })
      request.mockClear()

      act(() => {
        handlers.get('chat.room.g1.event')({
          type: 'new_message',
          roomId: 'g1',
          message: { id: 'm1', roomId: 'g1', sender: { account: 'bob' }, content: 'hi', createdAt: '2026-04-17T12:00:00Z' },
        })
      })

      // The new-message-in-active-room mark-read is debounced 500ms.
      await act(async () => { await vi.advanceTimersByTimeAsync(600) })
      expect(request).toHaveBeenCalledWith(readSubjectFor('g1'), {})
    } finally {
      vi.useRealTimers()
    }
  })

  it('does NOT fire message.read when a new_message arrives in a non-active room', async () => {
    const rooms = [
      { id: 'g1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 3, lastMsgAt: null },
      { id: 'g2', name: 'random',  type: 'channel', siteId: 'site-A', userCount: 3, lastMsgAt: null },
    ]
    const { nats, request, handlers } = setupWithRooms(rooms)
    let setActive
    function Probe() {
      const { setActiveRoom } = useRoomSummaries()
      setActive = setActiveRoom
      return null
    }
    render(wrap(<Probe />, nats))
    await waitFor(() => expect(handlers.has('chat.room.g2.event')).toBe(true))

    act(() => { setActive('g1') })
    request.mockClear()

    act(() => {
      handlers.get('chat.room.g2.event')({
        type: 'new_message',
        roomId: 'g2',
        message: { id: 'm9', roomId: 'g2', sender: { account: 'bob' }, content: 'hi', createdAt: '2026-04-17T12:00:00Z' },
      })
    })

    const subjects = request.mock.calls.map((c) => c[0])
    expect(subjects.some((s) => s.endsWith('.message.read'))).toBe(false)
  })

  it('does NOT fire message.read when the active-room message was sent by self (even after the debounce window)', async () => {
    // Self-sender path must short-circuit BEFORE scheduling. Advancing
    // fake timers past the window catches any regression that moved the
    // self-check after the schedule call.
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const rooms = [
        { id: 'g1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 3, lastMsgAt: null },
      ]
      const { nats, request, handlers } = setupWithRooms(rooms)
      let setActive
      function Probe() {
        const { setActiveRoom } = useRoomSummaries()
        setActive = setActiveRoom
        return null
      }
      render(wrap(<Probe />, nats))
      await waitFor(() => expect(handlers.has('chat.room.g1.event')).toBe(true))

      act(() => { setActive('g1') })
      request.mockClear()

      act(() => {
        handlers.get('chat.room.g1.event')({
          type: 'new_message',
          roomId: 'g1',
          message: { id: 'm1', roomId: 'g1', sender: { account: 'alice' }, content: 'self', createdAt: '2026-04-17T12:00:00Z' },
        })
      })

      // Advance past the debounce window — if the self-check is wrongly
      // placed AFTER scheduleMarkActiveRead's setTimeout, this would
      // fire the trailing RPC.
      await act(async () => { await vi.advanceTimersByTimeAsync(600) })

      const subjects = request.mock.calls.map((c) => c[0])
      expect(subjects.some((s) => s.endsWith('.message.read'))).toBe(false)
    } finally {
      vi.useRealTimers()
    }
  })

  it('cancels the pending mark-read timer on logout (user → null)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const rooms = [
        { id: 'g1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 3, lastMsgAt: null },
      ]
      const { request, handlers } = setupWithRooms(rooms)

      // Build a NatsContext where user can be unset to simulate logout.
      function ToggleProbe() {
        const { setActiveRoom } = useRoomSummaries()
        return <button onClick={() => setActiveRoom('g1')}>activate</button>
      }

      let setNatsValue
      function NatsHarness({ children }) {
        const [v, setV] = useState({
          connected: true,
          user: { account: 'alice', siteId: 'site-A' },
          error: null,
          connect: vi.fn(),
          request,
          publish: vi.fn(),
          subscribe: vi.fn().mockImplementation((subject, cb) => {
            handlers.set(subject, cb)
            return { unsubscribe: vi.fn() }
          }),
          disconnect: vi.fn(),
        })
        setNatsValue = setV
        return (
          <NatsContext.Provider value={v}>
            <RoomEventsProvider>{children}</RoomEventsProvider>
          </NatsContext.Provider>
        )
      }

      render(<NatsHarness><ToggleProbe /></NatsHarness>)
      await waitFor(() => expect(handlers.has('chat.room.g1.event')).toBe(true))
      await act(async () => { screen.getByText('activate').click() })

      // Schedule a trailing mark-read by simulating a burst…
      act(() => {
        handlers.get('chat.room.g1.event')({
          type: 'new_message',
          roomId: 'g1',
          message: { id: 'm1', sender: { account: 'bob' }, content: 'hi', createdAt: '...' },
        })
      })

      // …then logout BEFORE the debounce fires.
      const readsBefore = request.mock.calls.filter((c) => c[0].endsWith('.message.read')).length
      await act(async () => {
        setNatsValue((v) => ({ ...v, user: null }))
      })
      // Advance past the debounce window.
      await act(async () => { await vi.advanceTimersByTimeAsync(600) })

      const readsAfter = request.mock.calls.filter((c) => c[0].endsWith('.message.read')).length
      // The pending timer must have been cancelled by the effect's cleanup.
      expect(readsAfter).toBe(readsBefore)
    } finally {
      vi.useRealTimers()
    }
  })

  it('two bursts more than 500ms apart fire TWO trailing message.read RPCs', async () => {
    // Confirms the timer re-arms cleanly after firing — not a sticky one-shot.
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const rooms = [{ id: 'g1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 3, lastMsgAt: null }]
      const { nats, request, handlers } = setupWithRooms(rooms)
      let setActive
      function Probe() {
        const { setActiveRoom } = useRoomSummaries()
        setActive = setActiveRoom
        return null
      }
      render(wrap(<Probe />, nats))
      await waitFor(() => expect(handlers.has('chat.room.g1.event')).toBe(true))
      act(() => { setActive('g1') })
      request.mockClear()

      // First burst.
      act(() => {
        handlers.get('chat.room.g1.event')({
          type: 'new_message', roomId: 'g1',
          message: { id: 'b1m1', sender: { account: 'bob' }, content: 'hi', createdAt: '...' },
        })
      })
      await act(async () => { await vi.advanceTimersByTimeAsync(600) })

      // Second burst, well after the first trailing fired.
      act(() => {
        handlers.get('chat.room.g1.event')({
          type: 'new_message', roomId: 'g1',
          message: { id: 'b2m1', sender: { account: 'bob' }, content: 'hi again', createdAt: '...' },
        })
      })
      await act(async () => { await vi.advanceTimersByTimeAsync(600) })

      const reads = request.mock.calls.filter((c) => c[0].endsWith('.message.read'))
      expect(reads.length).toBe(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it('debounces a burst of active-room messages to a SINGLE trailing message.read RPC', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const rooms = [{ id: 'g1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 4, lastMsgAt: null }]
      const request = vi.fn().mockImplementation((subject) => {
        if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms })
        if (subject.includes('.subscription.get')) return Promise.resolve({ subscriptions: [] })
        return Promise.resolve({}) // every other (incl. message.read)
      })
      const handlers = new Map()
      const subscribe = vi.fn().mockImplementation((subject, cb) => {
        handlers.set(subject, cb)
        return { unsubscribe: vi.fn() }
      })
      const nats = mockNats({ request, subscribe })

      function Activator() {
        const { setActiveRoom } = useRoomSummaries()
        return <button onClick={() => setActiveRoom('g1')}>activate</button>
      }
      render(wrap(<Activator />, nats))

      // Open the room — fires immediate mark-read for setActiveRoom (1 call).
      await waitFor(() => expect(handlers.get('chat.room.g1.event')).toBeDefined())
      await act(async () => { screen.getByText('activate').click() })
      const after = request.mock.calls.filter((c) => c[0].endsWith('.message.read')).length
      expect(after).toBe(1)

      // Burst of 10 messages within the debounce window.
      await act(async () => {
        for (let i = 0; i < 10; i++) {
          handlers.get('chat.room.g1.event')({
            type: 'new_message',
            roomId: 'g1',
            message: { id: `m${i}`, roomId: 'g1', sender: { account: 'bob' }, content: 'hi', createdAt: '2026-04-17T12:00:00Z' },
          })
        }
      })

      // Inside the debounce window: no NEW message.read yet (still 1 from setActiveRoom).
      const inWindow = request.mock.calls.filter((c) => c[0].endsWith('.message.read')).length
      expect(inWindow).toBe(1)

      // Advance past the debounce window.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(600)
      })
      const afterDebounce = request.mock.calls.filter((c) => c[0].endsWith('.message.read')).length
      // Exactly one trailing call should have fired for the burst.
      expect(afterDebounce).toBe(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it('cancels the pending mark-read timer if the user switches rooms mid-burst', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const rooms = [
        { id: 'g1', name: 'general', type: 'channel', siteId: 'site-A', userCount: 4, lastMsgAt: null },
        { id: 'g2', name: 'other',   type: 'channel', siteId: 'site-A', userCount: 2, lastMsgAt: null },
      ]
      const request = vi.fn().mockImplementation((subject) => {
        if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms })
        if (subject.includes('.subscription.get')) return Promise.resolve({ subscriptions: [] })
        return Promise.resolve({})
      })
      const handlers = new Map()
      const subscribe = vi.fn().mockImplementation((subject, cb) => {
        handlers.set(subject, cb)
        return { unsubscribe: vi.fn() }
      })
      const nats = mockNats({ request, subscribe })

      function Activator() {
        const { setActiveRoom } = useRoomSummaries()
        return (
          <>
            <button onClick={() => setActiveRoom('g1')}>g1</button>
            <button onClick={() => setActiveRoom('g2')}>g2</button>
          </>
        )
      }
      render(wrap(<Activator />, nats))

      await waitFor(() => expect(handlers.get('chat.room.g1.event')).toBeDefined())
      await waitFor(() => expect(handlers.get('chat.room.g2.event')).toBeDefined())
      await act(async () => { screen.getByText('g1').click() }) // setActiveRoom: 1 read
      // Burst in g1; trailing timer queued.
      await act(async () => {
        handlers.get('chat.room.g1.event')({
          type: 'new_message',
          roomId: 'g1',
          message: { id: 'm1', sender: { account: 'bob' }, content: 'hi', createdAt: '...' },
        })
      })
      // Switch BEFORE the debounce fires.
      await act(async () => { screen.getByText('g2').click() }) // setActiveRoom: 2 reads now (g1 + g2)
      // Advance past the debounce window.
      await act(async () => { await vi.advanceTimersByTimeAsync(600) })

      const readCalls = request.mock.calls.filter((c) => c[0].endsWith('.message.read'))
      // The trailing g1 timer must NOT fire (room is no longer active).
      // Expected reads: setActiveRoom(g1) + setActiveRoom(g2) = 2.
      expect(readCalls.length).toBe(2)
      // Both setActiveRoom calls fired immediate reads — confirm by subject.
      expect(readCalls.some((c) => c[0].includes('.room.g1.'))).toBe(true)
      expect(readCalls.some((c) => c[0].includes('.room.g2.'))).toBe(true)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('RoomEventsProvider sidebar buckets bootstrap', () => {
  beforeEach(() => vi.clearAllMocks())

  it('fires the three user-service subjects on login with the documented payloads', async () => {
    const calls = []
    const request = vi.fn().mockImplementation((subject, payload) => {
      calls.push({ subject, payload })
      if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms: [] })
      if (subject.endsWith('.subscription.getCurrent'))
        return Promise.resolve({ subscriptions: [{ roomId: 'f1' }] })
      if (subject.endsWith('.subscription.getApps'))
        return Promise.resolve({ subscriptions: [{ roomId: 'a1' }] })
      if (subject.endsWith('.subscription.getRooms'))
        return Promise.resolve({ subscriptions: [{ roomId: 'c1' }] })
      throw new Error('unexpected subject: ' + subject)
    })
    const nats = mockNats({ request })

    render(wrap(<SummariesProbe />, nats))
    await waitFor(() => expect(request).toHaveBeenCalledTimes(4))

    const getCurrent = calls.find((c) => c.subject.endsWith('.subscription.getCurrent'))
    const getApps = calls.find((c) => c.subject.endsWith('.subscription.getApps'))
    const getRooms = calls.find((c) => c.subject.endsWith('.subscription.getRooms'))

    expect(getCurrent.subject).toBe(
      'chat.user.alice.request.user.site-A.subscription.getCurrent'
    )
    expect(getCurrent.payload).toEqual({ favorite: true })
    expect(getApps.subject).toBe('chat.user.alice.request.user.site-A.subscription.getApps')
    expect(getApps.payload).toEqual({})
    expect(getRooms.subject).toBe('chat.user.alice.request.user.site-A.subscription.getRooms')
    expect(getRooms.payload).toEqual({})
  })

  it('does not block rendering when one bucket RPC fails', async () => {
    const request = vi.fn().mockImplementation((subject) => {
      if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms: [] })
      if (subject.endsWith('.subscription.getCurrent'))
        return Promise.reject(new Error('boom'))
      if (subject.endsWith('.subscription.getApps'))
        return Promise.resolve({ subscriptions: [{ roomId: 'a1' }] })
      if (subject.endsWith('.subscription.getRooms'))
        return Promise.resolve({ subscriptions: [{ roomId: 'c1' }] })
      throw new Error('unexpected subject: ' + subject)
    })
    const nats = mockNats({ request })

    render(wrap(<SummariesProbe />, nats))
    await waitFor(() => expect(request).toHaveBeenCalledTimes(4))
    expect(screen.getByTestId('count').textContent).toBe('0')
  })

})

describe('useSidebarSections', () => {
  beforeEach(() => vi.clearAllMocks())

  function bootstrapNats({ buckets }) {
    const rooms = [
      { id: 'f1', name: 'fav-channel', type: 'channel', siteId: 'site-A', userCount: 2, lastMsgAt: '2026-04-17T10:00:00Z' },
      { id: 'a1', name: 'app-bot',     type: 'botDM',   siteId: 'site-A', userCount: 1, lastMsgAt: '2026-04-17T11:00:00Z' },
      { id: 'c1', name: 'general',     type: 'channel', siteId: 'site-A', userCount: 5, lastMsgAt: '2026-04-17T12:00:00Z' },
      { id: 'u1', name: 'unbucketed',  type: 'channel', siteId: 'site-A', userCount: 1, lastMsgAt: '2026-04-17T09:00:00Z' },
    ]
    return mockNats({
      request: vi.fn().mockImplementation((subject) => {
        if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms })
        if (subject.endsWith('.subscription.getCurrent'))
          return Promise.resolve({ subscriptions: buckets.favoriteIds.map((id) => ({ roomId: id })) })
        if (subject.endsWith('.subscription.getApps'))
          return Promise.resolve({ subscriptions: buckets.appIds.map((id) => ({ roomId: id })) })
        if (subject.endsWith('.subscription.getRooms'))
          return Promise.resolve({ subscriptions: buckets.channelDmIds.map((id) => ({ roomId: id })) })
        throw new Error('unexpected subject: ' + subject)
      }),
    })
  }

  function SectionsProbe() {
    const sections = useSidebarSections()
    return (
      <ul>
        {sections.map((s) => (
          <li key={s.key} data-testid={`section-${s.key}`}>
            {s.title}: {s.rooms.map((r) => r.id).join(',')}
          </li>
        ))}
      </ul>
    )
  }

  it('returns three sections in fixed order', async () => {
    const nats = bootstrapNats({ buckets: { favoriteIds: ['f1'], appIds: ['a1'], channelDmIds: ['c1'] } })
    render(wrap(<SectionsProbe />, nats))
    await waitFor(() =>
      expect(screen.getByTestId('section-channelDm').textContent).toContain('c1')
    )
    const items = screen.getAllByRole('listitem').map((li) => li.getAttribute('data-testid'))
    expect(items).toEqual(['section-favorite', 'section-apps', 'section-channelDm'])
  })

  it('puts favorited rooms only in Favorite (favorite > apps > channelDm exclusivity)', async () => {
    const nats = bootstrapNats({
      buckets: { favoriteIds: ['f1', 'a1'], appIds: ['a1'], channelDmIds: ['c1', 'a1'] },
    })
    render(wrap(<SectionsProbe />, nats))
    await waitFor(() =>
      expect(screen.getByTestId('section-favorite').textContent).toContain('a1')
    )
    expect(screen.getByTestId('section-apps').textContent).not.toContain('a1')
    expect(screen.getByTestId('section-channelDm').textContent).not.toContain('a1')
  })

  it('puts apps in Apps (apps > channelDm exclusivity)', async () => {
    const nats = bootstrapNats({
      buckets: { favoriteIds: [], appIds: ['a1'], channelDmIds: ['a1', 'c1'] },
    })
    render(wrap(<SectionsProbe />, nats))
    await waitFor(() => expect(screen.getByTestId('section-apps').textContent).toContain('a1'))
    expect(screen.getByTestId('section-channelDm').textContent).not.toContain('a1')
    expect(screen.getByTestId('section-channelDm').textContent).toContain('c1')
  })

  it('does not render rooms that are in summaries but in no bucket Set', async () => {
    const nats = bootstrapNats({
      buckets: { favoriteIds: ['f1'], appIds: ['a1'], channelDmIds: ['c1'] },
    })
    render(wrap(<SectionsProbe />, nats))
    await waitFor(() =>
      expect(screen.getByTestId('section-channelDm').textContent).toContain('c1')
    )
    expect(screen.getByTestId('section-favorite').textContent).not.toContain('u1')
    expect(screen.getByTestId('section-apps').textContent).not.toContain('u1')
    expect(screen.getByTestId('section-channelDm').textContent).not.toContain('u1')
  })

  it('preserves summaries recency order within each section', async () => {
    // bootstrapNats rooms in lastMsgAt order: c1 (12:00) > a1 (11:00) > f1 (10:00) > u1 (09:00)
    const nats = bootstrapNats({
      buckets: { favoriteIds: ['f1', 'c1'], appIds: ['a1'], channelDmIds: [] },
    })
    render(wrap(<SectionsProbe />, nats))
    await waitFor(() =>
      expect(screen.getByTestId('section-favorite').textContent).toContain('f1')
    )
    expect(screen.getByTestId('section-favorite').textContent).toMatch(/c1.*f1/)
  })

  it('merges subscription name and hrInfo from the bucket RPCs into each room', async () => {
    const rooms = [
      { id: 'c1', name: 'old-room-name', type: 'channel', siteId: 'site-A', userCount: 2, lastMsgAt: '2026-04-17T10:00:00Z' },
      { id: 'd1', name: '', type: 'dm', siteId: 'site-A', userCount: 2, lastMsgAt: '2026-04-17T11:00:00Z' },
    ]
    const nats = mockNats({
      request: vi.fn().mockImplementation((subject) => {
        if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms })
        if (subject.endsWith('.subscription.getCurrent'))
          return Promise.resolve({ subscriptions: [] })
        if (subject.endsWith('.subscription.getApps'))
          return Promise.resolve({ subscriptions: [] })
        if (subject.endsWith('.subscription.getRooms'))
          return Promise.resolve({
            subscriptions: [
              { roomId: 'c1', name: 'frontend-team' },
              { roomId: 'd1', name: 'bob-dm', hrInfo: { account: 'bob', engName: 'Bob Chen', name: '鮑勃' } },
            ],
          })
        throw new Error('unexpected subject: ' + subject)
      }),
    })

    function MergeProbe() {
      const sections = useSidebarSections()
      const channelDm = sections.find((s) => s.key === 'channelDm')
      return (
        <ul>
          {channelDm.rooms.map((r) => (
            <li key={r.id} data-testid={`room-${r.id}`}>
              name={r.subscriptionName ?? '∅'};hrEng={r.hrInfo?.engName ?? '∅'};hrName={r.hrInfo?.name ?? '∅'}
            </li>
          ))}
        </ul>
      )
    }

    render(wrap(<MergeProbe />, nats))
    await waitFor(() => expect(screen.queryByTestId('room-c1')).toBeInTheDocument())
    expect(screen.getByTestId('room-c1').textContent).toContain('name=frontend-team')
    expect(screen.getByTestId('room-d1').textContent).toContain('hrEng=Bob Chen')
    expect(screen.getByTestId('room-d1').textContent).toContain('hrName=鮑勃')
  })
})

describe('useSubscription', () => {
  beforeEach(() => vi.clearAllMocks())

  it('returns the full per-room subscription once the bucket bootstrap resolves', async () => {
    const nats = mockNats({
      request: vi.fn().mockImplementation((subject) => {
        if (subject === 'chat.user.alice.request.rooms.list') return Promise.resolve({ rooms: [] })
        if (subject.endsWith('.subscription.getCurrent'))
          return Promise.resolve({ subscriptions: [] })
        if (subject.endsWith('.subscription.getApps'))
          return Promise.resolve({ subscriptions: [] })
        if (subject.endsWith('.subscription.getRooms'))
          return Promise.resolve({
            subscriptions: [
              { roomId: 'r1', name: 'general', roles: ['owner'], hasMention: false, alert: true },
            ],
          })
        throw new Error('unexpected subject: ' + subject)
      }),
    })

    function Probe() {
      const sub = useSubscription('r1')
      if (!sub) return <div>no-sub</div>
      return <div>roles={sub.roles.join(',')};name={sub.name}</div>
    }

    render(wrap(<Probe />, nats))
    await waitFor(() =>
      expect(screen.getByText(/roles=owner;name=general/)).toBeInTheDocument()
    )
  })

  it('returns undefined for an unknown roomId or before bootstrap completes', () => {
    const nats = mockNats({
      // Never resolves — exercises the pre-bootstrap empty state synchronously.
      request: vi.fn().mockReturnValue(new Promise(() => {})),
    })
    function Probe() {
      const sub = useSubscription('unknown')
      return <div>{sub === undefined ? 'absent' : 'present'}</div>
    }
    render(wrap(<Probe />, nats))
    expect(screen.getByText('absent')).toBeInTheDocument()
  })
})
