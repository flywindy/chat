import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import MainApp from './MainApp'

let mockActiveParent = null
vi.mock('@/context/ThreadEventsContext', () => ({
  useThreadEvents: () => ({ activeParent: mockActiveParent }),
}))
vi.mock('./ThreadRightBar/ThreadRightBar', () => ({
  default: () => <aside>RIGHT-BAR</aside>,
}))

vi.mock('./AppHeader/AppHeader', () => ({
  default: ({ onSelectRoom, onEnterSearch }) => (
    <header>
      <button type="button" onClick={() => onSelectRoom({ id: 'r-from-search', name: 'r' })}>
        header-pick
      </button>
      <button type="button" onClick={() => onEnterSearch('hello')}>
        header-enter-search
      </button>
    </header>
  ),
}))
vi.mock('./Sidebar/Sidebar', () => ({
  default: ({ selectedRoomId, onSelectRoom }) => (
    <aside>
      <span>side:{selectedRoomId ?? 'none'}</span>
      <button type="button" onClick={() => onSelectRoom({ id: 'r-from-side', name: 's' })}>
        side-pick
      </button>
    </aside>
  ),
}))
vi.mock('./ChatPage/ChatPage', () => ({
  default: ({ selectedRoom }) => <section>page:{selectedRoom?.id ?? 'none'}</section>,
}))
vi.mock('./SearchResultsPane/SearchResultsPane', () => ({
  default: ({ query, onClose }) => (
    <section>
      results:{query}
      <button type="button" onClick={onClose}>close-results</button>
    </section>
  ),
}))
// Mock summaries are populated with the rooms the tests will click. The
// MainApp's "evict on summary disappearance" effect compares selectedRoom
// against summaries; without the rooms here the effect would clear the
// selection mid-test.
vi.mock('@/context/RoomEventsContext', () => ({
  useRoomSummaries: () => ({
    summaries: [
      { id: 'r-from-side', name: 's' },
      { id: 'r-from-search', name: 'r' },
    ],
    setActiveRoom: vi.fn(),
    jumpToMessage: vi.fn(),
  }),
}))

describe('MainApp', () => {
  it('starts with no room and renders ChatPage with null room', () => {
    render(<MainApp />)
    expect(screen.getByText('page:none')).toBeInTheDocument()
    expect(screen.getByText('side:none')).toBeInTheDocument()
  })

  it('selecting from the Sidebar updates the selected room everywhere', () => {
    render(<MainApp />)
    fireEvent.click(screen.getByText('side-pick'))
    expect(screen.getByText('side:r-from-side')).toBeInTheDocument()
    expect(screen.getByText('page:r-from-side')).toBeInTheDocument()
  })

  // SearchResultsPane + ThreadRightBar are lazy-loaded — assertions
  // that follow the opening event use findBy* to await the chunk.
  it('entering a search query swaps ChatPage for SearchResultsPane', async () => {
    render(<MainApp />)
    fireEvent.click(screen.getByText('header-enter-search'))
    expect(await screen.findByText('results:hello')).toBeInTheDocument()
    expect(screen.queryByText(/^page:/)).not.toBeInTheDocument()
  })

  it('closing search results restores ChatPage', async () => {
    render(<MainApp />)
    fireEvent.click(screen.getByText('header-enter-search'))
    fireEvent.click(await screen.findByText('close-results'))
    expect(screen.getByText('page:none')).toBeInTheDocument()
  })
})

describe('MainApp — ThreadRightBar mount', () => {
  beforeEach(() => { mockActiveParent = null })

  it('does not mount ThreadRightBar when no thread is active', () => {
    mockActiveParent = null
    render(<MainApp />)
    expect(screen.queryByText('RIGHT-BAR')).not.toBeInTheDocument()
  })

  it('mounts ThreadRightBar when activeParent is set', async () => {
    mockActiveParent = { messageId: 'p1' }
    render(<MainApp />)
    expect(await screen.findByText('RIGHT-BAR')).toBeInTheDocument()
  })
})
