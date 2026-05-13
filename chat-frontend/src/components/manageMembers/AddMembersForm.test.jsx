import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import AddMembersForm from './AddMembersForm'

vi.mock('../../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../../context/NatsContext'

const room = { id: 'r1', siteId: 'site-A', name: 'general' }

function setup(overrides = {}) {
  const requestWithAsyncResult = vi.fn().mockResolvedValue({
    sync: { status: 'accepted' },
    async: { status: 'ok', operation: 'room.member.add' },
  })
  useNats.mockReturnValue({
    user: { account: 'alice', siteId: 'site-A' },
    request: vi.fn(),
    requestWithAsyncResult,
    ...overrides,
  })
  render(<AddMembersForm room={room} />)
  return { requestWithAsyncResult }
}

describe('AddMembersForm', () => {
  beforeEach(() => useNats.mockReset())

  it('submit is always clickable; clicking on an empty form is a no-op', () => {
    // Previously the button was disabled until a chip existed, which meant
    // typing "alice" in Users without pressing Enter left submit greyed out.
    // The handler now flushes pending text first and early-returns on empty.
    const { requestWithAsyncResult } = setup()
    const submit = screen.getByRole('button', { name: /^Add$/ })
    expect(submit).not.toBeDisabled()
    fireEvent.click(submit)
    expect(requestWithAsyncResult).not.toHaveBeenCalled()
  })

  it('sends ChannelRef-shaped channels (not strings) with mode=all by default', async () => {
    const { requestWithAsyncResult } = setup()
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    fireEvent.change(screen.getByLabelText(/Orgs/i), { target: { value: 'eng' } })
    fireEvent.keyDown(screen.getByLabelText(/Orgs/i), { key: 'Enter' })
    fireEvent.change(screen.getByLabelText(/Channels/i), { target: { value: 'r-x' } })
    fireEvent.keyDown(screen.getByLabelText(/Channels/i), { key: 'Enter' })
    fireEvent.click(screen.getByRole('button', { name: /^Add$/ }))
    await waitFor(() => expect(requestWithAsyncResult).toHaveBeenCalledTimes(1))
    expect(requestWithAsyncResult).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.site-A.member.add',
      {
        roomId: 'r1',
        users: ['bob'],
        orgs: ['eng'],
        channels: [{ roomId: 'r-x', siteId: 'site-A' }],
        history: { mode: 'all' },
      }
    )
  })

  it('sends mode=none when share-history is unchecked', async () => {
    const { requestWithAsyncResult } = setup()
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    fireEvent.click(screen.getByLabelText(/Share room history/i))
    fireEvent.click(screen.getByRole('button', { name: /^Add$/ }))
    await waitFor(() => expect(requestWithAsyncResult).toHaveBeenCalledTimes(1))
    expect(requestWithAsyncResult.mock.calls[0][1].history).toEqual({ mode: 'none' })
  })

  it('auto-flushes typed-but-not-Entered text in all three fields on submit', async () => {
    const { requestWithAsyncResult } = setup()
    // User types comma-separated values in each field but does NOT press Enter.
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'alice, bob' } })
    fireEvent.change(screen.getByLabelText(/Orgs/i), { target: { value: 'eng-org, ops-org' } })
    fireEvent.change(screen.getByLabelText(/Channels/i), { target: { value: 'r-x, r-y' } })
    fireEvent.click(screen.getByRole('button', { name: /^Add$/ }))
    await waitFor(() => expect(requestWithAsyncResult).toHaveBeenCalledTimes(1))
    expect(requestWithAsyncResult.mock.calls[0][1]).toMatchObject({
      users: ['alice', 'bob'],
      orgs: ['eng-org', 'ops-org'],
      channels: [
        { roomId: 'r-x', siteId: 'site-A' },
        { roomId: 'r-y', siteId: 'site-A' },
      ],
    })
  })

  it('shows error banner when the async result is an error', async () => {
    const requestWithAsyncResult = vi.fn().mockRejectedValue(new Error('only owners can add members'))
    setup({ requestWithAsyncResult })
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    fireEvent.click(screen.getByRole('button', { name: /^Add$/ }))
    expect(await screen.findByText(/only owners/)).toBeInTheDocument()
  })

  it('clears chips and shows Added on async success', async () => {
    setup()
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    expect(screen.getByText('bob')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /^Add$/ }))
    expect(await screen.findByText('Added')).toBeInTheDocument()
    expect(screen.queryByText('bob')).not.toBeInTheDocument()
  })

  it('shows "Adding..." on the submit button while waiting for the async result', async () => {
    let resolveIt
    const requestWithAsyncResult = vi.fn(
      () => new Promise((r) => { resolveIt = () => r({ sync: { status: 'accepted' }, async: { status: 'ok' } }) })
    )
    setup({ requestWithAsyncResult })
    fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
    fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
    fireEvent.click(screen.getByRole('button', { name: /^Add$/ }))
    expect(await screen.findByRole('button', { name: /Adding/i })).toBeInTheDocument()
    await act(async () => { resolveIt() })
    await waitFor(() => expect(screen.getByRole('button', { name: /^Add$/ })).toBeInTheDocument())
  })

  it('auto-dismisses the success banner after 3 seconds', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      setup()
      fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
      fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
      fireEvent.click(screen.getByRole('button', { name: /^Add$/ }))
      expect(await screen.findByText('Added')).toBeInTheDocument()
      await act(async () => { vi.advanceTimersByTime(3000) })
      await waitFor(() => expect(screen.queryByText('Added')).not.toBeInTheDocument())
    } finally {
      vi.useRealTimers()
    }
  })

  it('cleanup effect calls clearTimeout with the pending timer id on unmount', async () => {
    // The React 18+ "setState after unmount" warning was removed in React 19,
    // so checking for that warning would be vacuously true. Spy on
    // clearTimeout directly to verify the cleanup effect actually disarms
    // the pending success-banner timer.
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const setTimeoutSpy = vi.spyOn(globalThis, 'setTimeout')
    const clearSpy = vi.spyOn(globalThis, 'clearTimeout')
    try {
      const requestWithAsyncResult = vi.fn().mockResolvedValue({
        sync: { status: 'accepted' },
        async: { status: 'ok', operation: 'room.member.add' },
      })
      useNats.mockReturnValue({
        user: { account: 'alice', siteId: 'site-A' },
        request: vi.fn(),
        requestWithAsyncResult,
      })
      const { unmount } = render(<AddMembersForm room={room} />)
      fireEvent.change(screen.getByLabelText(/Users/i), { target: { value: 'bob' } })
      fireEvent.keyDown(screen.getByLabelText(/Users/i), { key: 'Enter' })
      fireEvent.click(screen.getByRole('button', { name: /^Add$/ }))
      await screen.findByText('Added')

      // The 3000ms success-banner timer should be the most recent
      // setTimeout call with delay=3000. Capture its returned id.
      const successTimerCall = setTimeoutSpy.mock.results.find(
        (r, i) => setTimeoutSpy.mock.calls[i][1] === 3000
      )
      expect(successTimerCall).toBeDefined()
      const successTimerId = successTimerCall.value

      clearSpy.mockClear()
      unmount()
      // Cleanup effect ran; clearTimeout must have been called with that id.
      expect(clearSpy).toHaveBeenCalledWith(successTimerId)
    } finally {
      setTimeoutSpy.mockRestore()
      clearSpy.mockRestore()
      vi.useRealTimers()
    }
  })
})
