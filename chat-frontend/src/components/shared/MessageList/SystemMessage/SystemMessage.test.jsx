import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import SystemMessage from './SystemMessage'

// Helper: build a base64-encoded sysMsgData payload (mirrors how Go's
// json.Marshal of `[]byte` produces base64).
function encodeSysData(obj) {
  return btoa(JSON.stringify(obj))
}

const sender = { account: 'alice', engName: 'Alice' }

describe('SystemMessage', () => {
  it('renders members_added with a named list when ≤3 individuals', () => {
    const data = { individuals: ['bob', 'carol'], orgs: [], channels: [], addedUsersCount: 2 }
    render(
      <SystemMessage
        message={{
          id: 'm1',
          type: 'members_added',
          sender,
          sysMsgData: encodeSysData(data),
          createdAt: '2026-05-13T10:00:00Z',
        }}
      />
    )
    expect(screen.getByText(/Alice added bob, carol/)).toBeInTheDocument()
  })

  it('renders members_added with a count summary when more than 3', () => {
    const data = { individuals: ['a', 'b', 'c', 'd', 'e'], orgs: [], channels: [], addedUsersCount: 5 }
    render(
      <SystemMessage
        message={{
          id: 'm2',
          type: 'members_added',
          sender,
          sysMsgData: encodeSysData(data),
          createdAt: '2026-05-13T10:00:00Z',
        }}
      />
    )
    expect(screen.getByText(/Alice added 5 members/)).toBeInTheDocument()
  })

  it('renders members_added singular for count === 1', () => {
    const data = { individuals: [], orgs: [], channels: [{ roomId: 'r1', siteId: 's1' }], addedUsersCount: 1 }
    render(
      <SystemMessage
        message={{
          id: 'm3',
          type: 'members_added',
          sender,
          sysMsgData: encodeSysData(data),
          createdAt: '2026-05-13T10:00:00Z',
        }}
      />
    )
    expect(screen.getByText(/Alice added 1 member\b/)).toBeInTheDocument()
  })

  it('renders room_created with the channel name when present', () => {
    const data = { name: 'general', users: ['alice', 'bob'] }
    render(
      <SystemMessage
        message={{
          id: 'm4',
          type: 'room_created',
          sender,
          sysMsgData: encodeSysData(data),
          createdAt: '2026-05-13T10:00:00Z',
        }}
      />
    )
    expect(screen.getByText(/Alice created #general/)).toBeInTheDocument()
  })

  it('falls back to a generic label for unknown system types', () => {
    render(
      <SystemMessage
        message={{
          id: 'm5',
          type: 'something_new',
          sender,
          createdAt: '2026-05-13T10:00:00Z',
          content: '',
        }}
      />
    )
    expect(screen.getByText('Room event')).toBeInTheDocument()
  })

  it('preserves Content as the fallback text when type is unknown', () => {
    render(
      <SystemMessage
        message={{
          id: 'm6',
          type: 'something_new',
          sender,
          createdAt: '2026-05-13T10:00:00Z',
          content: 'A custom system event happened',
        }}
      />
    )
    expect(screen.getByText('A custom system event happened')).toBeInTheDocument()
  })

  it('uses userAccount when sender is missing entirely', () => {
    const data = { individuals: ['bob'], addedUsersCount: 1 }
    render(
      <SystemMessage
        message={{
          id: 'm7',
          type: 'members_added',
          userAccount: 'system',
          sysMsgData: encodeSysData(data),
          createdAt: '2026-05-13T10:00:00Z',
        }}
      />
    )
    expect(screen.getByText(/system added bob/)).toBeInTheDocument()
  })

  it('renders a time stamp alongside the system text', () => {
    render(
      <SystemMessage
        message={{
          id: 'm8',
          type: 'room_created',
          sender,
          createdAt: '2026-05-13T10:42:00Z',
        }}
      />
    )
    // Time formats vary by locale; just assert *something* time-shaped is rendered.
    expect(screen.getByText(/\d{1,2}:\d{2}/)).toBeInTheDocument()
  })

  it('survives a malformed sysMsgData payload without throwing', () => {
    render(
      <SystemMessage
        message={{
          id: 'm9',
          type: 'members_added',
          sender,
          sysMsgData: 'not-base64-or-json',
          createdAt: '2026-05-13T10:00:00Z',
        }}
      />
    )
    // Falls through to the no-data branch of describeMembersAdded.
    expect(screen.getByText(/Alice added members/)).toBeInTheDocument()
  })
})
