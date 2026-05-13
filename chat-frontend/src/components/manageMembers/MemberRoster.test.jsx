import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import MemberRoster from './MemberRoster'

vi.mock('../../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../../context/NatsContext'

const room = { id: 'r1', siteId: 'site-A', name: 'general' }

const baseMembers = [
  { id: 'rm1', rid: 'r1', member: { id: 'u-alice', type: 'individual', account: 'alice', engName: 'Alice A', chineseName: '王明', isOwner: true } },
  { id: 'rm2', rid: 'r1', member: { id: 'u-bob', type: 'individual', account: 'bob', engName: 'Bob B', chineseName: '陳大文', isOwner: false } },
  { id: 'rm3', rid: 'r1', member: { id: 'u-carol', type: 'individual', account: 'carol', engName: 'Carol C', chineseName: '李美', isOwner: true } },
  { id: 'rm4', rid: 'r1', member: { id: 'org-eng', type: 'org', sectName: 'Engineering', memberCount: 42 } },
]

function setupContext(overrides = {}) {
  const request = vi.fn().mockResolvedValue({ members: baseMembers })
  const requestWithAsyncResult = vi.fn().mockResolvedValue({ sync: { status: 'accepted' }, async: { status: 'ok' } })
  useNats.mockReturnValue({
    user: { account: 'alice', siteId: 'site-A' },
    request,
    requestWithAsyncResult,
    ...overrides,
  })
  return { request, requestWithAsyncResult }
}

describe('MemberRoster', () => {
  beforeEach(() => useNats.mockReset())

  it('does not refetch when the room prop is a new object reference but same id+siteId', async () => {
    const { request } = setupContext()
    const { rerender } = render(<MemberRoster room={{ id: 'r1', siteId: 'site-A', name: 'general' }} />)
    await waitFor(() => expect(request).toHaveBeenCalledTimes(1))
    rerender(<MemberRoster room={{ id: 'r1', siteId: 'site-A', name: 'general' }} />)
    // Empty act flushes any pending effects + microtasks deterministically.
    // If the rerender mistakenly re-fires fetchMembers, it would queue a new
    // call inside this flush; since deps are now `room.id` + `room.siteId`
    // (not the room object), the count must stay at 1.
    await act(async () => {})
    expect(request).toHaveBeenCalledTimes(1)
  })

  it('drops a stale member.list response if the room changed mid-fetch', async () => {
    // Slow network: switching from room A to room B before A's
    // member.list resolves used to let A's members overwrite B's. The
    // generation guard now drops the stale write.
    let resolveA
    const aResp = new Promise((r) => { resolveA = r })
    const request = vi.fn().mockImplementation((subject) => {
      if (subject.startsWith('chat.user.alice.request.room.r1.')) return aResp
      return Promise.resolve({
        members: [
          { id: 'rm-b', rid: 'r2', member: { id: 'u-newroom', type: 'individual', account: 'newroom', engName: 'New Room User', chineseName: '', isOwner: false } },
        ],
      })
    })
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request,
      requestWithAsyncResult: vi.fn(),
    })
    const { rerender } = render(<MemberRoster room={{ id: 'r1', siteId: 'site-A', name: 'general' }} />)
    // Switch rooms while r1's fetch is still in flight.
    rerender(<MemberRoster room={{ id: 'r2', siteId: 'site-A', name: 'frontend' }} />)
    await screen.findByText('New Room User')
    // Now the stale r1 response lands with what would have overwritten
    // the r2 list. The generation guard drops it.
    await act(async () => {
      resolveA({
        members: [
          { id: 'rm-a', rid: 'r1', member: { id: 'u-oldroom', type: 'individual', account: 'oldroom', engName: 'Old Room User', chineseName: '', isOwner: false } },
        ],
      })
      await Promise.resolve()
    })
    expect(screen.queryByText('Old Room User')).not.toBeInTheDocument()
    expect(screen.getByText('New Room User')).toBeInTheDocument()
  })

  it('calls member.list with enrich on mount', async () => {
    const { request } = setupContext()
    render(<MemberRoster room={room} />)
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith(
        'chat.user.alice.request.room.r1.site-A.member.list',
        { enrich: true }
      )
    )
  })

  it('renders orgs first then individuals in a single list, with EngName + ChineseName for individuals', async () => {
    setupContext()
    render(<MemberRoster room={room} />)
    await screen.findByText('Alice A')
    expect(screen.getByText('Engineering')).toBeInTheDocument()
    expect(screen.getByText('Bob B')).toBeInTheDocument()
    // Individuals show chineseName as the secondary text (replacing the
    // old engName + accountName combo).
    expect(screen.getByText('王明')).toBeInTheDocument()
    expect(screen.getByText('陳大文')).toBeInTheDocument()
    // Single roster-list, orgs ordered before individuals.
    const listItems = screen.getAllByRole('listitem')
    expect(listItems[0]).toHaveTextContent('Engineering')
    expect(listItems[1]).toHaveTextContent('Alice A')
    expect(listItems[2]).toHaveTextContent('Bob B')
    expect(listItems[3]).toHaveTextContent('Carol C')
    // Owner badge still appears on owners; not on regular members.
    expect(listItems[1]).toHaveTextContent(/owner/i)
    expect(listItems[2]).not.toHaveTextContent(/owner/i)
    expect(listItems[3]).toHaveTextContent(/owner/i)
  })

  it('shows a loading indicator before the list resolves', async () => {
    let resolveIt
    const request = vi.fn().mockImplementation(() => new Promise((r) => { resolveIt = r }))
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request,
      requestWithAsyncResult: vi.fn(),
    })
    render(<MemberRoster room={room} />)
    expect(screen.getByText(/loading members/i)).toBeInTheDocument()
    resolveIt({ members: [] })
    await waitFor(() => expect(screen.queryByText(/loading members/i)).not.toBeInTheDocument())
  })

  it('renders an error banner when member.list fails', async () => {
    const request = vi.fn().mockRejectedValue(new Error('not a room member'))
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request,
      requestWithAsyncResult: vi.fn(),
    })
    render(<MemberRoster room={room} />)
    expect(await screen.findByText(/not a room member/i)).toBeInTheDocument()
  })

  it('Promote on a non-owner member sends member.role-update newRole=owner', async () => {
    const { requestWithAsyncResult } = setupContext()
    render(<MemberRoster room={room} />)
    await screen.findByText('Bob B')
    fireEvent.click(screen.getByRole('button', { name: /Promote bob/i }))
    await waitFor(() =>
      expect(requestWithAsyncResult).toHaveBeenCalledWith(
        'chat.user.alice.request.room.r1.site-A.member.role-update',
        { roomId: 'r1', account: 'bob', newRole: 'owner' }
      )
    )
  })

  it('Demote on another owner sends member.role-update newRole=member', async () => {
    // Run as alice (owner); demote carol (also owner). Owner-on-self is a
    // Leave button in the new design, not a Demote, so the Demote-action
    // test must target a peer owner.
    const { requestWithAsyncResult } = setupContext()
    render(<MemberRoster room={room} />)
    await screen.findByText('Carol C')
    fireEvent.click(screen.getByRole('button', { name: /Demote carol/i }))
    await waitFor(() =>
      expect(requestWithAsyncResult).toHaveBeenCalledWith(
        'chat.user.alice.request.room.r1.site-A.member.role-update',
        { roomId: 'r1', account: 'carol', newRole: 'member' }
      )
    )
  })

  it('Remove on an individual sends member.remove with account', async () => {
    const { requestWithAsyncResult } = setupContext()
    render(<MemberRoster room={room} />)
    await screen.findByText('Bob B')
    fireEvent.click(screen.getByRole('button', { name: /Remove bob/i }))
    await waitFor(() =>
      expect(requestWithAsyncResult).toHaveBeenCalledWith(
        'chat.user.alice.request.room.r1.site-A.member.remove',
        { roomId: 'r1', account: 'bob' }
      )
    )
  })

  it('Remove on an org sends member.remove with orgId', async () => {
    const { requestWithAsyncResult } = setupContext()
    render(<MemberRoster room={room} />)
    await screen.findByText('Engineering')
    fireEvent.click(screen.getByRole('button', { name: /Remove org-eng/i }))
    await waitFor(() =>
      expect(requestWithAsyncResult).toHaveBeenCalledWith(
        'chat.user.alice.request.room.r1.site-A.member.remove',
        { roomId: 'r1', orgId: 'org-eng' }
      )
    )
  })

  it('refetches the roster after a successful action', async () => {
    const { request } = setupContext()
    render(<MemberRoster room={room} />)
    await screen.findByText('Bob B')
    expect(request).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('button', { name: /Remove bob/i }))
    await waitFor(() => expect(request).toHaveBeenCalledTimes(2))
  })

  it('isolates the busy state to the row whose action is in flight (no suffix collisions)', async () => {
    // Regression: previous predicate was `busyKey?.endsWith(`:${account}`)`
    // which (a) disabled all three buttons on the active row at once and
    // (b) cross-disabled `bob` and `dynamicbob` because both end in `:bob`.
    let resolveAction
    const requestWithAsyncResult = vi.fn(
      () => new Promise((r) => { resolveAction = () => r({ sync: { status: 'accepted' }, async: { status: 'ok' } }) })
    )
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request: vi.fn().mockResolvedValue({
        members: [
          // Alice (current user) must be in the roster as owner for the
          // owner-gated Promote/Remove buttons to render. Without her,
          // isCurrentUserOwner falls through to false and the buttons
          // this test is exercising don't exist.
          { id: 'rm0', rid: 'r1', member: { id: 'u-alice', type: 'individual', account: 'alice', engName: 'Alice', isOwner: true } },
          { id: 'rm1', rid: 'r1', member: { id: 'u-bob', type: 'individual', account: 'bob', engName: 'Bob', isOwner: false } },
          { id: 'rm2', rid: 'r1', member: { id: 'u-dyn', type: 'individual', account: 'dynamicbob', engName: 'DBob', isOwner: false } },
        ],
      }),
      requestWithAsyncResult,
    })
    render(<MemberRoster room={room} />)
    await screen.findByText('DBob')
    fireEvent.click(screen.getByRole('button', { name: /Promote bob/i }))
    // Promote on bob is in flight — bob's Remove and dynamicbob's buttons must remain enabled.
    expect(screen.getByRole('button', { name: /Promote bob/i })).toBeDisabled()
    expect(screen.getByRole('button', { name: /Remove bob/i })).not.toBeDisabled()
    expect(screen.getByRole('button', { name: /Promote dynamicbob/i })).not.toBeDisabled()
    expect(screen.getByRole('button', { name: /Remove dynamicbob/i })).not.toBeDisabled()
    resolveAction()
  })

  it('does not render Remove-by-ID inputs (every row already has its own Remove button)', async () => {
    setupContext()
    render(<MemberRoster room={room} />)
    await screen.findByText('Bob B')
    expect(screen.queryByLabelText(/Remove individual by account/i)).toBeNull()
    expect(screen.queryByLabelText(/Remove org by id/i)).toBeNull()
  })

  it('inline-button callers safely ignore runAction return value on both success and failure', async () => {
    // The Promote/Demote/Remove buttons fire-and-forget; they don't branch on
    // runAction's boolean return. Lock in that ignoring the return value is
    // safe (no unhandled rejection, no post-action side effects fired wrongly).
    const requestWithAsyncResult = vi.fn()
      .mockRejectedValueOnce(new Error('only owners can remove members'))
      .mockResolvedValueOnce({ sync: { status: 'accepted' }, async: { status: 'ok' } })
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request: vi.fn().mockResolvedValue({ members: baseMembers }),
      requestWithAsyncResult,
    })
    render(<MemberRoster room={room} />)
    await screen.findByText('Bob B')

    // Failure path: error banner appears, button re-enables, no crash.
    fireEvent.click(screen.getByRole('button', { name: /Remove bob/i }))
    expect(await screen.findByText(/only owners/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Remove bob/i })).not.toBeDisabled()

    // Success path right after: no error banner, action proceeds normally.
    fireEvent.click(screen.getByRole('button', { name: /Promote bob/i }))
    await waitFor(() => expect(requestWithAsyncResult).toHaveBeenCalledTimes(2))
  })

  it('shows a Leave button on the current user\'s own row (not Remove / Promote / Demote)', async () => {
    setupContext() // current user = alice (owner)
    render(<MemberRoster room={room} />)
    await screen.findByText('Alice A')
    const aliceRow = screen.getByText('Alice A').closest('li')
    // The self row never offers Remove or role-change controls — leaving is
    // the only self-applicable action. Demote/Remove on self would be a UX
    // trap: a user could lock themselves out of admin or kick themselves
    // and lose the dialog mid-action.
    expect(aliceRow).toHaveTextContent(/Leave/i)
    expect(aliceRow.querySelector('button[aria-label^="Remove alice"]')).toBeNull()
    expect(aliceRow.querySelector('button[aria-label^="Promote alice"]')).toBeNull()
    expect(aliceRow.querySelector('button[aria-label^="Demote alice"]')).toBeNull()
  })

  it('clicking Leave on own row sends member.remove with the user\'s own account after confirm', async () => {
    const { requestWithAsyncResult } = setupContext()
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    try {
      render(<MemberRoster room={room} />)
      await screen.findByText('Alice A')
      fireEvent.click(screen.getByRole('button', { name: /^Leave$/i }))
      expect(confirmSpy).toHaveBeenCalledWith(expect.stringMatching(/Leave .*general/i))
      await waitFor(() =>
        expect(requestWithAsyncResult).toHaveBeenCalledWith(
          'chat.user.alice.request.room.r1.site-A.member.remove',
          { roomId: 'r1', account: 'alice' }
        )
      )
    } finally {
      confirmSpy.mockRestore()
    }
  })

  it('cancelling the Leave confirm dialog does not send any request', async () => {
    const { requestWithAsyncResult } = setupContext()
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    try {
      render(<MemberRoster room={room} />)
      await screen.findByText('Alice A')
      fireEvent.click(screen.getByRole('button', { name: /^Leave$/i }))
      expect(confirmSpy).toHaveBeenCalled()
      // Give a microtask to confirm no request fires.
      await new Promise((r) => setTimeout(r, 30))
      expect(requestWithAsyncResult).not.toHaveBeenCalled()
    } finally {
      confirmSpy.mockRestore()
    }
  })

  it('hides Promote / Demote / Remove on every row when the current user is not an owner', async () => {
    // Bob is a regular member; he must see only Leave (on his own row) and
    // no admin controls anywhere else.
    useNats.mockReturnValue({
      user: { account: 'bob', siteId: 'site-A' },
      request: vi.fn().mockResolvedValue({ members: baseMembers }),
      requestWithAsyncResult: vi.fn(),
    })
    render(<MemberRoster room={room} />)
    await screen.findByText('Alice A')
    // No Promote/Demote/Remove anywhere (across individuals and orgs).
    expect(screen.queryByRole('button', { name: /Promote/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /Demote/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /^Remove/i })).toBeNull()
    // Bob still sees a Leave button on his own row.
    expect(screen.getByRole('button', { name: /^Leave$/i })).toBeInTheDocument()
  })

  it('surfaces a server error from an action as a banner', async () => {
    const requestWithAsyncResult = vi.fn().mockRejectedValue(new Error('only owners can remove members'))
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'site-A' },
      request: vi.fn().mockResolvedValue({ members: baseMembers }),
      requestWithAsyncResult,
    })
    render(<MemberRoster room={room} />)
    await screen.findByText('Bob B')
    fireEvent.click(screen.getByRole('button', { name: /Remove bob/i }))
    expect(await screen.findByText(/only owners/i)).toBeInTheDocument()
  })

  describe('org expansion', () => {
    function setupWithOrgFetch(orgMembersResponse) {
      // Reuse the base roster mock, then layer the orgs.{id}.members
      // response for the second request call (the first call is
      // member.list?enrich at mount; the second is orgMembers).
      const request = vi.fn()
      request.mockImplementation((subject) => {
        if (subject.includes('.orgs.') && subject.endsWith('.members')) {
          return orgMembersResponse instanceof Error
            ? Promise.reject(orgMembersResponse)
            : Promise.resolve(orgMembersResponse)
        }
        return Promise.resolve({ members: baseMembers })
      })
      useNats.mockReturnValue({
        user: { account: 'alice', siteId: 'site-A' },
        request,
        requestWithAsyncResult: vi.fn().mockResolvedValue({ sync: { status: 'accepted' }, async: { status: 'ok' } }),
      })
      return { request }
    }

    const orgMembersPayload = {
      members: [
        { id: 'u-dave',  account: 'dave',  engName: 'Dave Davies',  chineseName: '戴文偉', siteId: 'site-A' },
        { id: 'u-erin',  account: 'erin',  engName: 'Erin Evans',   chineseName: '葉伊蓮', siteId: 'site-A' },
        { id: 'u-frank', account: 'frank', engName: 'Frank Fischer', chineseName: '費法蘭', siteId: 'site-A' },
      ],
    }

    it('clicking the org name fires chat.user.{a}.request.orgs.{id}.members and renders the expanded children', async () => {
      const { request } = setupWithOrgFetch(orgMembersPayload)
      render(<MemberRoster room={room} />)
      await screen.findByText('Engineering')
      fireEvent.click(screen.getByRole('button', { name: /Expand Engineering/i }))
      await waitFor(() =>
        expect(request).toHaveBeenCalledWith('chat.user.alice.request.orgs.org-eng.members', {})
      )
      // Each child shows engName + chineseName.
      expect(await screen.findByText('Dave Davies')).toBeInTheDocument()
      expect(screen.getByText('戴文偉')).toBeInTheDocument()
      expect(screen.getByText('Erin Evans')).toBeInTheDocument()
      expect(screen.getByText('Frank Fischer')).toBeInTheDocument()
    })

    it('child rows have no Promote / Demote / Remove buttons (org row owns the lifecycle)', async () => {
      setupWithOrgFetch(orgMembersPayload)
      render(<MemberRoster room={room} />)
      await screen.findByText('Engineering')
      fireEvent.click(screen.getByRole('button', { name: /Expand Engineering/i }))
      await screen.findByText('Dave Davies')
      // No buttons targeting an org child.
      expect(screen.queryByRole('button', { name: /Remove dave/i })).toBeNull()
      expect(screen.queryByRole('button', { name: /Promote dave/i })).toBeNull()
      expect(screen.queryByRole('button', { name: /Demote dave/i })).toBeNull()
      expect(screen.queryByRole('button', { name: /Remove erin/i })).toBeNull()
    })

    it('clicking the org name a second time collapses without refetching (cached response is reused)', async () => {
      const { request } = setupWithOrgFetch(orgMembersPayload)
      render(<MemberRoster room={room} />)
      await screen.findByText('Engineering')
      const toggle = screen.getByRole('button', { name: /Expand Engineering/i })

      // First expand → fetches.
      fireEvent.click(toggle)
      await screen.findByText('Dave Davies')
      const fetchCallsAfterOpen = request.mock.calls.filter(
        (call) => call[0] === 'chat.user.alice.request.orgs.org-eng.members'
      ).length
      expect(fetchCallsAfterOpen).toBe(1)

      // Collapse → no children visible, no new fetch.
      fireEvent.click(screen.getByRole('button', { name: /Collapse Engineering/i }))
      expect(screen.queryByText('Dave Davies')).not.toBeInTheDocument()

      // Re-expand → still cached, no second fetch.
      fireEvent.click(screen.getByRole('button', { name: /Expand Engineering/i }))
      await screen.findByText('Dave Davies')
      const fetchCallsAfterReopen = request.mock.calls.filter(
        (call) => call[0] === 'chat.user.alice.request.orgs.org-eng.members'
      ).length
      expect(fetchCallsAfterReopen).toBe(1)
    })

    it('shows a "Failed to load members" fallback when the org-members RPC errors', async () => {
      setupWithOrgFetch(new Error('not subscribed'))
      render(<MemberRoster room={room} />)
      await screen.findByText('Engineering')
      fireEvent.click(screen.getByRole('button', { name: /Expand Engineering/i }))
      expect(await screen.findByText(/Failed to load members/i)).toBeInTheDocument()
    })

    it('a stale toggle (rapid expand → collapse → re-expand) only writes the latest fetch result to state', async () => {
      // A user rapidly expanding then collapsing the same org would
      // otherwise let the first fetch's response overwrite the cached
      // state set by the second fetch. The orgFetchGenRef guard drops
      // stale resolutions.
      let resolveFirst
      const firstFetch = new Promise((r) => { resolveFirst = r })
      const secondPayload = {
        members: [{ id: 'u-late', account: 'late', engName: 'Late Arrival', chineseName: '', siteId: 'site-A' }],
      }
      let orgFetchCount = 0
      const request = vi.fn().mockImplementation((subject) => {
        if (subject.includes('.orgs.') && subject.endsWith('.members')) {
          orgFetchCount += 1
          return orgFetchCount === 1 ? firstFetch : Promise.resolve(secondPayload)
        }
        return Promise.resolve({ members: baseMembers })
      })
      useNats.mockReturnValue({
        user: { account: 'alice', siteId: 'site-A' },
        request,
        requestWithAsyncResult: vi.fn(),
      })
      render(<MemberRoster room={room} />)
      await screen.findByText('Engineering')

      // First expand → first fetch in flight (not yet resolved).
      fireEvent.click(screen.getByRole('button', { name: /Expand Engineering/i }))
      // Collapse — clears expanded state.
      fireEvent.click(screen.getByRole('button', { name: /Collapse Engineering/i }))
      // Re-expand → second fetch fires (and resolves immediately).
      fireEvent.click(screen.getByRole('button', { name: /Expand Engineering/i }))
      await screen.findByText('Late Arrival')

      // NOW the first (stale) fetch resolves with a stale payload that
      // would, without the gen guard, clobber 'Late Arrival'.
      await act(async () => {
        resolveFirst({
          members: [{ id: 'u-stale', account: 'stale', engName: 'Stale Result', chineseName: '', siteId: 'site-A' }],
        })
        await Promise.resolve()
      })
      // The stale write is dropped — the latest fetch's result wins.
      expect(screen.queryByText('Stale Result')).not.toBeInTheDocument()
      expect(screen.getByText('Late Arrival')).toBeInTheDocument()
    })

    it('non-owner viewer can still expand orgs even though no Remove button is shown', async () => {
      const request = vi.fn()
      request.mockImplementation((subject) => {
        if (subject.includes('.orgs.') && subject.endsWith('.members')) {
          return Promise.resolve(orgMembersPayload)
        }
        return Promise.resolve({ members: baseMembers })
      })
      useNats.mockReturnValue({
        user: { account: 'bob', siteId: 'site-A' }, // bob is a regular member
        request,
        requestWithAsyncResult: vi.fn(),
      })
      render(<MemberRoster room={room} />)
      await screen.findByText('Engineering')
      // No org-level Remove button for bob.
      expect(screen.queryByRole('button', { name: /Remove org-eng/i })).toBeNull()
      // But Expand still works.
      fireEvent.click(screen.getByRole('button', { name: /Expand Engineering/i }))
      expect(await screen.findByText('Dave Davies')).toBeInTheDocument()
    })
  })
})
