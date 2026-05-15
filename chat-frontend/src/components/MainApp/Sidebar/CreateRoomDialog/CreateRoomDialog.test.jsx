import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, render, screen, fireEvent, waitFor } from '@testing-library/react'
import CreateRoomDialog from './CreateRoomDialog'

vi.mock('@/context/NatsContext', () => ({
  useNats: vi.fn(),
}))
// useRoomSummaries is consumed by the dialog so it can wait for the
// just-created room to appear in summaries (i.e. for the server's
// subscription.update event to arrive) before closing. Tests default the
// mock to summaries already containing every roomId the various success
// fixtures return; tests that exercise the wait path override this.
vi.mock('@/context/RoomEventsContext', () => ({
  useRoomSummaries: vi.fn(),
}))

import { useNats } from '@/context/NatsContext'
import { useRoomSummaries } from '@/context/RoomEventsContext'

// Pre-populate summaries with the roomIds the success fixtures return so
// the dialog's "wait for subscription.update" useEffect resolves on the
// first render after submit and the happy-path tests pass synchronously.
const DEFAULT_SUMMARIES = [
  { id: 'r-new', name: 'frontend', type: 'channel', siteId: 'site-A' },
  { id: 'r-dm', name: '', type: 'dm', siteId: 'site-A' },
  { id: 'r-existing', name: '', type: 'dm', siteId: 'site-A' },
]

function setup(overrides = {}) {
  const requestWithAsyncResult = vi.fn().mockResolvedValue({
    sync: { status: 'accepted', roomId: 'r-new', roomType: 'channel' },
    async: { status: 'ok', roomId: 'r-new', operation: 'room.create' },
  })
  const request = vi.fn()
  useNats.mockReturnValue({
    user: { account: 'alice', siteId: 'site-A' },
    request,
    requestWithAsyncResult,
    ...overrides,
  })
  useRoomSummaries.mockReturnValue({
    summaries: overrides.summaries ?? DEFAULT_SUMMARIES,
  })
  const onClose = vi.fn()
  const onCreated = vi.fn()
  render(<CreateRoomDialog onClose={onClose} onCreated={onCreated} />)
  return { requestWithAsyncResult, request, onClose, onCreated }
}

describe('CreateRoomDialog', () => {
  beforeEach(() => {
    useNats.mockReset()
    useRoomSummaries.mockReset()
  })

  it('does not show a room-type dropdown — type is inferred from inputs', () => {
    setup()
    expect(screen.queryByLabelText(/^Type$/i)).not.toBeInTheDocument()
  })

  it('submit is always clickable; clicking on a fully-empty form is a no-op', async () => {
    // Submit-button gating used to be `name || chip-count > 0`, but that meant
    // a user who typed "alice" in Users (without pressing Enter) saw a
    // disabled Create and wondered why. Drop the visual gate; the submit
    // handler does the actual empty-check after flushing pending text.
    const { requestWithAsyncResult } = setup()
    const submit = screen.getByRole('button', { name: /Create/i })
    expect(submit).not.toBeDisabled()
    fireEvent.click(submit)
    await new Promise((r) => setTimeout(r, 30))
    expect(requestWithAsyncResult).not.toHaveBeenCalled()
  })

  it('submits a channel-shaped payload to room.{siteId}.create when a name is given', async () => {
    const { requestWithAsyncResult, onCreated, onClose } = setup()
    fireEvent.change(screen.getByLabelText(/Name/i), { target: { value: 'frontend' } })
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    fireEvent.click(screen.getByRole('button', { name: /Create/i }))
    await waitFor(() => expect(requestWithAsyncResult).toHaveBeenCalledTimes(1))
    expect(requestWithAsyncResult).toHaveBeenCalledWith(
      'chat.user.alice.request.room.site-A.create',
      { name: 'frontend', users: ['bob'], orgs: [], channels: [] },
      expect.objectContaining({ treatAsSuccess: expect.any(Function) })
    )
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(
      expect.objectContaining({ id: 'r-new', type: 'channel' })
    ))
    await waitFor(() => expect(onClose).toHaveBeenCalled())
  })

  it('submits a DM-shaped payload (empty name, one user) and surfaces roomType=dm', async () => {
    const requestWithAsyncResult = vi.fn().mockResolvedValue({
      sync: { status: 'accepted', roomId: 'r-dm', roomType: 'dm' },
      async: { status: 'ok', roomId: 'r-dm', operation: 'room.create' },
    })
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request: vi.fn(),
      requestWithAsyncResult,
    })
    useRoomSummaries.mockReturnValue({ summaries: DEFAULT_SUMMARIES })
    const onCreated = vi.fn()
    render(<CreateRoomDialog onClose={vi.fn()} onCreated={onCreated} />)
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    fireEvent.click(screen.getByRole('button', { name: /Create/i }))
    await waitFor(() =>
      expect(requestWithAsyncResult).toHaveBeenCalledWith(
        'chat.user.alice.request.room.site-A.create',
        { name: '', users: ['bob'], orgs: [], channels: [] },
        expect.objectContaining({ treatAsSuccess: expect.any(Function) })
      )
    )
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(
      expect.objectContaining({ id: 'r-dm', type: 'dm' })
    ))
  })

  it('uses the counterpart account as the optimistic DM display name', async () => {
    // The server will send the canonical name via subscription.update,
    // but until then the room header would render blank if we passed
    // the empty `name` field through. Fall back to the counterpart so
    // the sidebar and header have something to show.
    const { onCreated } = setup({
      requestWithAsyncResult: vi.fn().mockResolvedValue({
        sync: { status: 'accepted', roomId: 'r-dm', roomType: 'dm' },
        async: { status: 'ok', roomId: 'r-dm', operation: 'room.create' },
      }),
    })
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    fireEvent.click(screen.getByRole('button', { name: /Create/i }))
    await waitFor(() =>
      expect(onCreated).toHaveBeenCalledWith(expect.objectContaining({ name: 'bob' }))
    )
  })

  it('treats a "dm already exists" reply as success and navigates to the existing room', async () => {
    const requestWithAsyncResult = vi.fn().mockResolvedValue({
      sync: { error: 'dm already exists', roomId: 'r-existing' },
      async: null,
    })
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request: vi.fn(),
      requestWithAsyncResult,
    })
    useRoomSummaries.mockReturnValue({ summaries: DEFAULT_SUMMARIES })
    const onCreated = vi.fn()
    const onClose = vi.fn()
    render(<CreateRoomDialog onClose={onClose} onCreated={onCreated} />)
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    fireEvent.click(screen.getByRole('button', { name: /Create/i }))
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(
      expect.objectContaining({ id: 'r-existing', type: 'dm' })
    ))
    await waitFor(() => expect(onClose).toHaveBeenCalled())
    expect(requestWithAsyncResult.mock.calls[0][2]).toMatchObject({
      treatAsSuccess: expect.any(Function),
    })
  })

  it('auto-flushes typed-but-not-Entered text into the request payload', async () => {
    const { requestWithAsyncResult } = setup()
    fireEvent.change(screen.getByLabelText(/Name/i), { target: { value: 'team' } })
    // User types but does NOT press Enter on any picker field.
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'alice, bob' } })
    fireEvent.change(screen.getByLabelText(/Orgs/i), { target: { value: 'eng-org' } })
    fireEvent.change(screen.getByLabelText(/Channels/i), { target: { value: 'r-x, r-y' } })
    fireEvent.click(screen.getByRole('button', { name: /Create/i }))
    await waitFor(() => expect(requestWithAsyncResult).toHaveBeenCalledTimes(1))
    expect(requestWithAsyncResult.mock.calls[0][1]).toEqual({
      name: 'team',
      users: ['alice', 'bob'],
      orgs: ['eng-org'],
      channels: [
        { roomId: 'r-x', siteId: 'site-A' },
        { roomId: 'r-y', siteId: 'site-A' },
      ],
    })
  })

  describe('subscription.update wait', () => {
    it('shows "Waiting for server confirmation…" and holds the dialog open after sync ack until summaries contains the room', async () => {
      // Start with the new room absent from summaries — emulates the
      // window between the synchronous create ack and the asynchronous
      // subscription.update event landing.
      const { requestWithAsyncResult, onCreated, onClose } = setup({ summaries: [] })
      fireEvent.change(screen.getByLabelText(/Name/i), { target: { value: 'frontend' } })
      fireEvent.click(screen.getByRole('button', { name: /Create/i }))
      // Wait for the sync ack to be processed (request fires + we set
      // pendingRoom).
      await waitFor(() => expect(requestWithAsyncResult).toHaveBeenCalledTimes(1))
      // After sync ack the button label flips to "Waiting…" and neither
      // callback has fired yet — the dialog is parked, watching summaries.
      expect(await screen.findByRole('button', { name: /Waiting for server confirmation/i })).toBeInTheDocument()
      expect(onCreated).not.toHaveBeenCalled()
      expect(onClose).not.toHaveBeenCalled()
    })

    it('after the 3-second timeout, surfaces an error in the dialog and does NOT auto-select the room', async () => {
      // Calling onCreated on timeout used to bounce the user back to the
      // empty state — ChatPage's auto-deselect effect kicks in because
      // summaries still doesn't contain the new roomId, and the channel
      // subscription wasn't opened either (so messages wouldn't echo
      // back). The dialog now surfaces the slow-server condition inside
      // itself; the user dismisses with Cancel, and if the room was
      // actually created they pick it up from the sidebar when
      // subscription.update finally lands.
      vi.useFakeTimers({ shouldAdvanceTime: true })
      try {
        const { onCreated, onClose } = setup({ summaries: [] })
        fireEvent.change(screen.getByLabelText(/Name/i), { target: { value: 'frontend' } })
        fireEvent.click(screen.getByRole('button', { name: /Create/i }))
        await screen.findByRole('button', { name: /Waiting/i })
        expect(onCreated).not.toHaveBeenCalled()
        await act(async () => {
          await vi.advanceTimersByTimeAsync(3001)
        })
        // Error appears, dialog stays open, neither callback fired.
        expect(await screen.findByText(/taking longer than expected/i)).toBeInTheDocument()
        expect(onCreated).not.toHaveBeenCalled()
        expect(onClose).not.toHaveBeenCalled()
        // Submit button is back to "Create" — pendingRoom has been cleared
        // so the wait state is over. User can dismiss with Cancel.
        expect(screen.getByRole('button', { name: /^Create$/ })).toBeInTheDocument()
        expect(screen.getByRole('button', { name: /^Cancel$/ })).not.toBeDisabled()
      } finally {
        vi.useRealTimers()
      }
    })

    it('Cancel is re-enabled during the "Waiting…" state so the user can back out without waiting for the timeout', async () => {
      // While the request was in flight (loading=true) Cancel stays
      // disabled — we can't safely abort a published NATS request. Once
      // the sync ack lands and we're just waiting for subscription.update
      // (pendingRoom set, loading=false), Cancel should re-enable.
      const { onClose } = setup({ summaries: [] })
      fireEvent.change(screen.getByLabelText(/Name/i), { target: { value: 'frontend' } })
      fireEvent.click(screen.getByRole('button', { name: /Create/i }))
      // Wait for the request to settle and the dialog to enter the
      // pending state.
      await screen.findByRole('button', { name: /Waiting/i })
      const cancel = screen.getByRole('button', { name: /^Cancel$/ })
      expect(cancel).not.toBeDisabled()
      fireEvent.click(cancel)
      expect(onClose).toHaveBeenCalled()
    })
  })

  it('shows the server error on a failed create and does not close', async () => {
    const requestWithAsyncResult = vi.fn().mockRejectedValue(new Error('exceeds maximum capacity (50)'))
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request: vi.fn(),
      requestWithAsyncResult,
    })
    useRoomSummaries.mockReturnValue({ summaries: DEFAULT_SUMMARIES })
    const onClose = vi.fn()
    render(<CreateRoomDialog onClose={onClose} onCreated={vi.fn()} />)
    fireEvent.change(screen.getByLabelText(/Name/i), { target: { value: 'huge' } })
    fireEvent.click(screen.getByRole('button', { name: /Create/i }))
    expect(await screen.findByText(/exceeds maximum capacity/i)).toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
  })
})
