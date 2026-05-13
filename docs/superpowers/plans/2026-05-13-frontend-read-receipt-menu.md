# Frontend Read-Receipt Kebab Menu Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a kebab message-action menu in `chat-frontend` whose first item invokes the existing `message.read-receipt` RPC and surfaces `Read by X of Y` with a hover sub-tooltip listing readers.

**Architecture:** A new self-contained `MessageActionMenu` component owns the kebab button, a popover with the read-receipt row, the RPC lifecycle, and dismissal handling. It returns `null` for other people's messages, so `MessageArea` can render it unconditionally for every message. RPC subject is added to the existing `subjects.js` builder module.

**Tech Stack:** React 19, Vite, NATS via `useNats()` (in `NatsContext`), Vitest + `@testing-library/react` for tests.

**Spec:** `docs/superpowers/specs/2026-05-13-frontend-read-receipt-menu-design.md`

**Working directory:** repo root (`/home/user/chat`). All frontend commands run from `chat-frontend/`.

**Branch:** `claude/add-read-receipt-menu-IYLRf` (already checked out).

---

## File Map

| File | Action | Purpose |
|------|--------|---------|
| `chat-frontend/src/lib/subjects.js` | Modify | Add `readReceipt(account, roomId, siteId)` builder |
| `chat-frontend/src/lib/subjects.test.js` | Modify | Test for the new builder |
| `chat-frontend/src/components/MessageActionMenu.jsx` | Create | Kebab + popover + read-receipt + sub-tooltip |
| `chat-frontend/src/components/MessageActionMenu.test.jsx` | Create | Vitest suite for the component |
| `chat-frontend/src/components/MessageArea.jsx` | Modify | Render `<MessageActionMenu>` per message row |
| `chat-frontend/src/components/MessageArea.test.jsx` | Modify | Mock `useNats`; assert kebab visibility |
| `chat-frontend/src/styles/index.css` | Modify | Styles for menu/popover/tooltip |

---

## Task 1: Read-receipt subject builder

**Files:**
- Modify: `chat-frontend/src/lib/subjects.js`
- Modify: `chat-frontend/src/lib/subjects.test.js`

- [ ] **Step 1: Write the failing test**

Open `chat-frontend/src/lib/subjects.test.js` and add (placement: alongside the other builder tests):

```js
import { readReceipt } from './subjects'

describe('readReceipt', () => {
  it('builds the request subject for the read-receipt RPC', () => {
    expect(readReceipt('alice', 'room1', 'site1')).toBe(
      'chat.user.alice.request.room.room1.site1.message.read-receipt'
    )
  })
})
```

If the file already has a single top-level `describe`, add the case inside it as `it('builds the read-receipt subject', ...)`. Use whichever shape matches the existing style (read the file first).

- [ ] **Step 2: Run the test to verify it fails**

```
cd chat-frontend && npx vitest run src/lib/subjects.test.js
```

Expected: FAIL with `readReceipt is not a function` (or similar — the export does not exist yet).

- [ ] **Step 3: Add the builder**

In `chat-frontend/src/lib/subjects.js`, add the function after `memberRoleUpdate` (keep it grouped with the room-scoped request subjects):

```js
export function readReceipt(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.message.read-receipt`
}
```

- [ ] **Step 4: Run the test to verify it passes**

```
cd chat-frontend && npx vitest run src/lib/subjects.test.js
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add chat-frontend/src/lib/subjects.js chat-frontend/src/lib/subjects.test.js
git commit -m "feat(chat-frontend): add readReceipt subject builder"
```

---

## Task 2: `MessageActionMenu` — render-gating (own-messages only)

**Files:**
- Create: `chat-frontend/src/components/MessageActionMenu.jsx`
- Create: `chat-frontend/src/components/MessageActionMenu.test.jsx`

- [ ] **Step 1: Write the failing tests**

Create `chat-frontend/src/components/MessageActionMenu.test.jsx`:

```jsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import MessageActionMenu from './MessageActionMenu'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../context/NatsContext'

const room = { id: 'r1', siteId: 'siteA', userCount: 4 }

beforeEach(() => {
  useNats.mockReset()
  useNats.mockReturnValue({
    user: { account: 'alice', siteId: 'siteA' },
    request: vi.fn(),
  })
})

describe('MessageActionMenu render-gating', () => {
  it('renders the kebab button for the current user\'s own message', () => {
    const msg = { id: 'm1', sender: { account: 'alice' } }
    render(<MessageActionMenu message={msg} room={room} />)
    expect(screen.getByRole('button', { name: /Message actions/i })).toBeInTheDocument()
  })

  it('renders nothing for messages sent by other users', () => {
    const msg = { id: 'm1', sender: { account: 'bob' } }
    const { container } = render(<MessageActionMenu message={msg} room={room} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when there is no signed-in user', () => {
    useNats.mockReturnValue({ user: null, request: vi.fn() })
    const msg = { id: 'm1', sender: { account: 'alice' } }
    const { container } = render(<MessageActionMenu message={msg} room={room} />)
    expect(container).toBeEmptyDOMElement()
  })
})
```

- [ ] **Step 2: Run the tests to verify they fail**

```
cd chat-frontend && npx vitest run src/components/MessageActionMenu.test.jsx
```

Expected: FAIL — module not found.

- [ ] **Step 3: Create the minimal component**

Create `chat-frontend/src/components/MessageActionMenu.jsx`:

```jsx
import { useNats } from '../context/NatsContext'

export default function MessageActionMenu({ message, room }) {
  const { user } = useNats()
  const isOwnMessage = !!user && message?.sender?.account === user.account
  if (!isOwnMessage) return null

  return (
    <div className="message-action-menu">
      <button
        type="button"
        className="message-action-kebab"
        aria-haspopup="menu"
        aria-expanded={false}
        aria-label="Message actions"
      >
        ⋮
      </button>
    </div>
  )
}

// room is referenced by callers; lint suppression not needed because the prop
// is wired in later tasks. Leaving the param in the signature now keeps the
// API stable across tasks.
```

- [ ] **Step 4: Run the tests to verify they pass**

```
cd chat-frontend && npx vitest run src/components/MessageActionMenu.test.jsx
```

Expected: PASS (3 cases).

- [ ] **Step 5: Commit**

```
git add chat-frontend/src/components/MessageActionMenu.jsx chat-frontend/src/components/MessageActionMenu.test.jsx
git commit -m "feat(chat-frontend): scaffold MessageActionMenu with own-message gating"
```

---

## Task 3: Popover open/close and dismissal

**Files:**
- Modify: `chat-frontend/src/components/MessageActionMenu.jsx`
- Modify: `chat-frontend/src/components/MessageActionMenu.test.jsx`

- [ ] **Step 1: Write the failing tests**

Append to `MessageActionMenu.test.jsx`:

```jsx
import { fireEvent } from '@testing-library/react'

describe('MessageActionMenu open/close', () => {
  const msg = { id: 'm1', sender: { account: 'alice' } }

  function setup({ request = vi.fn().mockReturnValue(new Promise(() => {})) } = {}) {
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    return render(<MessageActionMenu message={msg} room={room} />)
  }

  it('opens the popover when the kebab is clicked', () => {
    setup()
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(screen.getByRole('menu')).toBeInTheDocument()
  })

  it('closes the popover when the kebab is clicked again (toggle)', () => {
    setup()
    const kebab = screen.getByRole('button', { name: /Message actions/i })
    fireEvent.click(kebab)
    fireEvent.click(kebab)
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })

  it('closes the popover when the user clicks outside', () => {
    setup()
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(screen.getByRole('menu')).toBeInTheDocument()
    fireEvent.mouseDown(document.body)
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })

  it('closes the popover when Escape is pressed', () => {
    setup()
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })

  it('does not close when clicking inside the popover', () => {
    setup()
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    fireEvent.mouseDown(screen.getByRole('menu'))
    expect(screen.getByRole('menu')).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run the tests to verify they fail**

```
cd chat-frontend && npx vitest run src/components/MessageActionMenu.test.jsx
```

Expected: FAIL — there is no `<menu>` role yet because `open` state and the popover element don't exist.

- [ ] **Step 3: Implement open/close and dismissal**

Replace the body of `chat-frontend/src/components/MessageActionMenu.jsx` with:

```jsx
import { useCallback, useEffect, useRef, useState } from 'react'
import { useNats } from '../context/NatsContext'

export default function MessageActionMenu({ message, room }) {
  const { user } = useNats()
  const [open, setOpen] = useState(false)
  const rootRef = useRef(null)

  const close = useCallback(() => setOpen(false), [])

  useEffect(() => {
    if (!open) return
    const onMouseDown = (e) => {
      if (rootRef.current && !rootRef.current.contains(e.target)) close()
    }
    const onKeyDown = (e) => { if (e.key === 'Escape') close() }
    document.addEventListener('mousedown', onMouseDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onMouseDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open, close])

  const isOwnMessage = !!user && message?.sender?.account === user.account
  if (!isOwnMessage) return null

  const handleKebabClick = () => setOpen((v) => !v)

  return (
    <div className="message-action-menu" ref={rootRef}>
      <button
        type="button"
        className="message-action-kebab"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Message actions"
        onClick={handleKebabClick}
      >
        ⋮
      </button>
      {open && (
        <div className="message-action-popover" role="menu">
          {/* read-receipt row added in Task 4 */}
        </div>
      )}
    </div>
  )
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```
cd chat-frontend && npx vitest run src/components/MessageActionMenu.test.jsx
```

Expected: PASS (all cases — Task 2's plus Task 3's).

- [ ] **Step 5: Commit**

```
git add chat-frontend/src/components/MessageActionMenu.jsx chat-frontend/src/components/MessageActionMenu.test.jsx
git commit -m "feat(chat-frontend): MessageActionMenu open/close + click-outside/Escape dismissal"
```

---

## Task 4: Read-receipt RPC lifecycle + `Read by X of Y` math

**Files:**
- Modify: `chat-frontend/src/components/MessageActionMenu.jsx`
- Modify: `chat-frontend/src/components/MessageActionMenu.test.jsx`

- [ ] **Step 1: Write the failing tests**

Append to `MessageActionMenu.test.jsx`:

```jsx
import { waitFor, act } from '@testing-library/react'
import { readReceipt } from '../lib/subjects'

describe('MessageActionMenu read-receipt RPC', () => {
  const msg = { id: 'm1', sender: { account: 'alice' } }

  function deferred() {
    let resolve, reject
    const promise = new Promise((res, rej) => { resolve = res; reject = rej })
    return { promise, resolve, reject }
  }

  it('shows Loading… immediately after opening the menu', () => {
    const d = deferred()
    const request = vi.fn().mockReturnValue(d.promise)
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    render(<MessageActionMenu message={msg} room={room} />)
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(screen.getByText(/Loading…/i)).toBeInTheDocument()
  })

  it('calls the RPC with the correct subject and payload', () => {
    const d = deferred()
    const request = vi.fn().mockReturnValue(d.promise)
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    render(<MessageActionMenu message={msg} room={room} />)
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(request).toHaveBeenCalledWith(
      readReceipt('alice', 'r1', 'siteA'),
      { messageId: 'm1' }
    )
  })

  it('renders "Read by X of Y" once the RPC resolves', async () => {
    const d = deferred()
    const request = vi.fn().mockReturnValue(d.promise)
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    render(<MessageActionMenu message={msg} room={room} />)
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    await act(async () => {
      d.resolve({ readers: [
        { userId: 'u1', account: 'bob', engName: 'Bob', chineseName: '' },
        { userId: 'u2', account: 'carol', engName: 'Carol', chineseName: '' },
      ] })
    })
    expect(await screen.findByText('Read by 2 of 3')).toBeInTheDocument()
  })

  it('clamps Y at 0 for a single-member room', async () => {
    const d = deferred()
    const request = vi.fn().mockReturnValue(d.promise)
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    render(
      <MessageActionMenu
        message={msg}
        room={{ id: 'r1', siteId: 'siteA', userCount: 1 }}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    await act(async () => { d.resolve({ readers: [] }) })
    expect(await screen.findByText('Read by 0 of 0')).toBeInTheDocument()
  })

  it('renders the RPC error message inline', async () => {
    const d = deferred()
    const request = vi.fn().mockReturnValue(d.promise)
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    render(<MessageActionMenu message={msg} room={room} />)
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    await act(async () => { d.reject(new Error('only the message sender can view read receipts')) })
    expect(
      await screen.findByText(/only the message sender can view read receipts/i)
    ).toBeInTheDocument()
  })

  it('refetches the RPC every time the menu is reopened', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce({ readers: [] })
      .mockResolvedValueOnce({ readers: [{ userId: 'u1', account: 'bob', engName: 'Bob', chineseName: '' }] })
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    render(<MessageActionMenu message={msg} room={room} />)
    const kebab = screen.getByRole('button', { name: /Message actions/i })

    fireEvent.click(kebab)
    await waitFor(() => expect(screen.getByText('Read by 0 of 3')).toBeInTheDocument())
    fireEvent.click(kebab) // close
    fireEvent.click(kebab) // reopen
    await waitFor(() => expect(screen.getByText('Read by 1 of 3')).toBeInTheDocument())
    expect(request).toHaveBeenCalledTimes(2)
  })

  it('falls back to user.siteId when room.siteId is missing', () => {
    const d = deferred()
    const request = vi.fn().mockReturnValue(d.promise)
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    render(
      <MessageActionMenu
        message={msg}
        room={{ id: 'r1', userCount: 3 }}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    expect(request).toHaveBeenCalledWith(
      readReceipt('alice', 'r1', 'siteA'),
      { messageId: 'm1' }
    )
  })
})
```

- [ ] **Step 2: Run the tests to verify they fail**

```
cd chat-frontend && npx vitest run src/components/MessageActionMenu.test.jsx
```

Expected: FAIL — no RPC call, no `Loading…`, no `Read by X of Y` text.

- [ ] **Step 3: Implement RPC lifecycle and X/Y rendering**

Replace `chat-frontend/src/components/MessageActionMenu.jsx` with:

```jsx
import { useCallback, useEffect, useRef, useState } from 'react'
import { useNats } from '../context/NatsContext'
import { readReceipt } from '../lib/subjects'

export default function MessageActionMenu({ message, room }) {
  const { user, request } = useNats()
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const [readers, setReaders] = useState(null)
  const rootRef = useRef(null)
  const mountedRef = useRef(true)

  useEffect(() => () => { mountedRef.current = false }, [])

  const close = useCallback(() => {
    setOpen(false)
    setLoading(false)
    setError(null)
    setReaders(null)
  }, [])

  useEffect(() => {
    if (!open) return
    const onMouseDown = (e) => {
      if (rootRef.current && !rootRef.current.contains(e.target)) close()
    }
    const onKeyDown = (e) => { if (e.key === 'Escape') close() }
    document.addEventListener('mousedown', onMouseDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onMouseDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open, close])

  const isOwnMessage = !!user && message?.sender?.account === user.account

  const handleKebabClick = () => {
    if (open) { close(); return }
    setOpen(true)
    setLoading(true)
    setError(null)
    setReaders(null)
    const siteId = room?.siteId ?? user.siteId
    const subject = readReceipt(user.account, room.id, siteId)
    Promise.resolve(request(subject, { messageId: message.id }))
      .then((resp) => {
        if (!mountedRef.current) return
        setReaders(resp?.readers ?? [])
        setLoading(false)
      })
      .catch((err) => {
        if (!mountedRef.current) return
        setError(err?.message || 'Failed to load read receipts')
        setLoading(false)
      })
  }

  if (!isOwnMessage) return null

  const X = readers?.length ?? 0
  const Y = Math.max(0, (room?.userCount ?? 1) - 1)

  return (
    <div className="message-action-menu" ref={rootRef}>
      <button
        type="button"
        className="message-action-kebab"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Message actions"
        onClick={handleKebabClick}
      >
        ⋮
      </button>
      {open && (
        <div className="message-action-popover" role="menu">
          {loading && <div className="read-receipt-row read-receipt-loading">Loading…</div>}
          {error && <div className="read-receipt-row read-receipt-error">{error}</div>}
          {!loading && !error && readers != null && (
            <div className="read-receipt-row" role="menuitem">
              Read by {X} of {Y}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```
cd chat-frontend && npx vitest run src/components/MessageActionMenu.test.jsx
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add chat-frontend/src/components/MessageActionMenu.jsx chat-frontend/src/components/MessageActionMenu.test.jsx
git commit -m "feat(chat-frontend): wire read-receipt RPC and render Read by X of Y"
```

---

## Task 5: Reader sub-tooltip on hover/focus

**Files:**
- Modify: `chat-frontend/src/components/MessageActionMenu.jsx`
- Modify: `chat-frontend/src/components/MessageActionMenu.test.jsx`

- [ ] **Step 1: Write the failing tests**

Append to `MessageActionMenu.test.jsx`:

```jsx
describe('MessageActionMenu reader sub-tooltip', () => {
  const msg = { id: 'm1', sender: { account: 'alice' } }

  async function openMenuWith(readers) {
    const request = vi.fn().mockResolvedValue({ readers })
    useNats.mockReturnValue({
      user: { account: 'alice', siteId: 'siteA' },
      request,
    })
    render(<MessageActionMenu message={msg} room={room} />)
    fireEvent.click(screen.getByRole('button', { name: /Message actions/i }))
    // Wait for resolved RPC to render.
    await screen.findByText(/Read by /)
  }

  it('does not render the tooltip when X = 0', async () => {
    await openMenuWith([])
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
    // Hovering an empty row must still not open it.
    fireEvent.mouseEnter(screen.getByText('Read by 0 of 3'))
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })

  it('opens the tooltip on hover when X > 0 and closes on mouse-leave', async () => {
    await openMenuWith([
      { userId: 'u1', account: 'bob', engName: 'Bob', chineseName: '鮑勃' },
    ])
    const row = screen.getByRole('menuitem', { name: /Read by 1 of 3/i })
    fireEvent.mouseEnter(row)
    expect(screen.getByRole('tooltip')).toBeInTheDocument()
    fireEvent.mouseLeave(row)
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })

  it('opens the tooltip on keyboard focus and closes on blur', async () => {
    await openMenuWith([
      { userId: 'u1', account: 'bob', engName: 'Bob', chineseName: '' },
    ])
    const row = screen.getByRole('menuitem', { name: /Read by 1 of 3/i })
    fireEvent.focus(row)
    expect(screen.getByRole('tooltip')).toBeInTheDocument()
    fireEvent.blur(row)
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })

  it('formats reader names as "engName chineseName" when both are present', async () => {
    await openMenuWith([
      { userId: 'u1', account: 'bob', engName: 'Bob', chineseName: '鮑勃' },
    ])
    fireEvent.mouseEnter(screen.getByRole('menuitem', { name: /Read by 1 of 3/i }))
    expect(screen.getByRole('tooltip')).toHaveTextContent('Bob 鮑勃')
  })

  it('formats reader names as "engName" when chineseName is empty', async () => {
    await openMenuWith([
      { userId: 'u1', account: 'bob', engName: 'Bob', chineseName: '' },
    ])
    fireEvent.mouseEnter(screen.getByRole('menuitem', { name: /Read by 1 of 3/i }))
    expect(screen.getByRole('tooltip')).toHaveTextContent('Bob')
    expect(screen.getByRole('tooltip')).not.toHaveTextContent('Bob ')
  })

  it('falls back to account when engName is empty', async () => {
    await openMenuWith([
      { userId: 'u1', account: 'bob', engName: '', chineseName: '鮑勃' },
    ])
    fireEvent.mouseEnter(screen.getByRole('menuitem', { name: /Read by 1 of 3/i }))
    expect(screen.getByRole('tooltip')).toHaveTextContent('bob 鮑勃')
  })

  it('lists all readers in the tooltip', async () => {
    await openMenuWith([
      { userId: 'u1', account: 'bob', engName: 'Bob', chineseName: '' },
      { userId: 'u2', account: 'carol', engName: 'Carol', chineseName: '凱蘿' },
    ])
    fireEvent.mouseEnter(screen.getByRole('menuitem', { name: /Read by 2 of 3/i }))
    const items = screen.getAllByRole('listitem')
    expect(items.map((li) => li.textContent)).toEqual(['Bob', 'Carol 凱蘿'])
  })
})
```

- [ ] **Step 2: Run the tests to verify they fail**

```
cd chat-frontend && npx vitest run src/components/MessageActionMenu.test.jsx
```

Expected: FAIL — there is no `role="menuitem"` interactive row, no tooltip, no `<li>` reader entries yet.

- [ ] **Step 3: Implement the sub-tooltip**

Replace the read-receipt-row block inside `MessageActionMenu.jsx`. The full updated file becomes:

```jsx
import { useCallback, useEffect, useRef, useState } from 'react'
import { useNats } from '../context/NatsContext'
import { readReceipt } from '../lib/subjects'

function formatReaderName(r) {
  const eng = r.engName || r.account || ''
  return r.chineseName ? `${eng} ${r.chineseName}`.trim() : eng
}

export default function MessageActionMenu({ message, room }) {
  const { user, request } = useNats()
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const [readers, setReaders] = useState(null)
  const [tooltipOpen, setTooltipOpen] = useState(false)
  const rootRef = useRef(null)
  const mountedRef = useRef(true)

  useEffect(() => () => { mountedRef.current = false }, [])

  const close = useCallback(() => {
    setOpen(false)
    setTooltipOpen(false)
    setLoading(false)
    setError(null)
    setReaders(null)
  }, [])

  useEffect(() => {
    if (!open) return
    const onMouseDown = (e) => {
      if (rootRef.current && !rootRef.current.contains(e.target)) close()
    }
    const onKeyDown = (e) => { if (e.key === 'Escape') close() }
    document.addEventListener('mousedown', onMouseDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onMouseDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open, close])

  const isOwnMessage = !!user && message?.sender?.account === user.account

  const handleKebabClick = () => {
    if (open) { close(); return }
    setOpen(true)
    setLoading(true)
    setError(null)
    setReaders(null)
    setTooltipOpen(false)
    const siteId = room?.siteId ?? user.siteId
    const subject = readReceipt(user.account, room.id, siteId)
    Promise.resolve(request(subject, { messageId: message.id }))
      .then((resp) => {
        if (!mountedRef.current) return
        setReaders(resp?.readers ?? [])
        setLoading(false)
      })
      .catch((err) => {
        if (!mountedRef.current) return
        setError(err?.message || 'Failed to load read receipts')
        setLoading(false)
      })
  }

  if (!isOwnMessage) return null

  const X = readers?.length ?? 0
  const Y = Math.max(0, (room?.userCount ?? 1) - 1)
  const hasReaders = readers != null && X > 0

  return (
    <div className="message-action-menu" ref={rootRef}>
      <button
        type="button"
        className="message-action-kebab"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Message actions"
        onClick={handleKebabClick}
      >
        ⋮
      </button>
      {open && (
        <div className="message-action-popover" role="menu">
          {loading && <div className="read-receipt-row read-receipt-loading">Loading…</div>}
          {error && <div className="read-receipt-row read-receipt-error">{error}</div>}
          {!loading && !error && readers != null && (
            hasReaders ? (
              <button
                type="button"
                role="menuitem"
                className="read-receipt-row"
                onMouseEnter={() => setTooltipOpen(true)}
                onMouseLeave={() => setTooltipOpen(false)}
                onFocus={() => setTooltipOpen(true)}
                onBlur={() => setTooltipOpen(false)}
              >
                Read by {X} of {Y}
                {tooltipOpen && (
                  <ul className="read-receipt-tooltip" role="tooltip">
                    {readers.map((r) => (
                      <li key={r.userId}>{formatReaderName(r)}</li>
                    ))}
                  </ul>
                )}
              </button>
            ) : (
              <div
                role="menuitem"
                aria-disabled="true"
                className="read-receipt-row read-receipt-empty"
              >
                Read by {X} of {Y}
              </div>
            )
          )}
        </div>
      )}
    </div>
  )
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```
cd chat-frontend && npx vitest run src/components/MessageActionMenu.test.jsx
```

Expected: PASS for every case in the file.

- [ ] **Step 5: Commit**

```
git add chat-frontend/src/components/MessageActionMenu.jsx chat-frontend/src/components/MessageActionMenu.test.jsx
git commit -m "feat(chat-frontend): add reader sub-tooltip on Read by X of Y row"
```

---

## Task 6: Integrate `MessageActionMenu` into `MessageArea`

**Files:**
- Modify: `chat-frontend/src/components/MessageArea.jsx`
- Modify: `chat-frontend/src/components/MessageArea.test.jsx`

- [ ] **Step 1: Write/update the failing tests**

Edit `chat-frontend/src/components/MessageArea.test.jsx`. Add a `useNats` mock at the top (under the existing `useRoomEvents` mock), and add two new cases. Insert after the existing mock setup:

```jsx
vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../context/NatsContext'
```

Then before each test that renders messages, make sure `useNats` is set. Add this to the `beforeEach` block:

```jsx
beforeEach(() => {
  window.HTMLElement.prototype.scrollIntoView = vi.fn()
  useNats.mockReturnValue({
    user: { account: 'alice', siteId: 'siteA' },
    request: vi.fn().mockReturnValue(new Promise(() => {})),
  })
})
```

(Replace the existing `beforeEach` — keep the `scrollIntoView` line.)

Then append two new tests inside the `describe('MessageArea', ...)` block:

```jsx
it('renders MessageActionMenu kebab for the current user\'s own messages', () => {
  useRoomEvents.mockReturnValue({
    messages: [
      { id: 'm1', content: 'mine', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'alice', engName: 'Alice' } },
    ],
    hasLoadedHistory: true,
    historyError: null,
    loadHistory: vi.fn().mockResolvedValue(),
  })
  render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)
  expect(screen.getByRole('button', { name: /Message actions/i })).toBeInTheDocument()
})

it('does not render a kebab for other users\' messages', () => {
  useRoomEvents.mockReturnValue({
    messages: [
      { id: 'm1', content: 'theirs', createdAt: '2026-04-17T12:00:00Z', sender: { account: 'bob', engName: 'Bob' } },
    ],
    hasLoadedHistory: true,
    historyError: null,
    loadHistory: vi.fn().mockResolvedValue(),
  })
  render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)
  expect(screen.queryByRole('button', { name: /Message actions/i })).not.toBeInTheDocument()
})
```

- [ ] **Step 2: Run the tests to verify they fail**

```
cd chat-frontend && npx vitest run src/components/MessageArea.test.jsx
```

Expected: FAIL — `Message actions` button not found, because `MessageArea` doesn't yet render `MessageActionMenu`.

- [ ] **Step 3: Wire `MessageActionMenu` into `MessageArea`**

Edit `chat-frontend/src/components/MessageArea.jsx`. Add the import at the top:

```jsx
import MessageActionMenu from './MessageActionMenu'
```

Inside the messages map, replace the existing `<div key={msg.id} ...>` block with:

```jsx
{messages.map((msg) => (
  <div key={msg.id} className="message" data-message-id={msg.id}>
    <span className="message-sender">{senderName(msg)}</span>
    <span className="message-time">{formatTime(msg.createdAt)}</span>
    <div className="message-content">{messageContent(msg)}</div>
    <MessageActionMenu message={msg} room={room} />
  </div>
))}
```

(`MessageActionMenu` returns `null` for messages not authored by the current user, so it's safe to render unconditionally.)

- [ ] **Step 4: Run the full chat-frontend test suite**

```
cd chat-frontend && npx vitest run
```

Expected: ALL tests pass. Pay attention to existing `MessageArea` tests — they should still pass thanks to the updated `beforeEach` that supplies a `useNats` stub.

- [ ] **Step 5: Commit**

```
git add chat-frontend/src/components/MessageArea.jsx chat-frontend/src/components/MessageArea.test.jsx
git commit -m "feat(chat-frontend): render MessageActionMenu in each message row"
```

---

## Task 7: Styles

**Files:**
- Modify: `chat-frontend/src/styles/index.css`

- [ ] **Step 1: Read existing `.message` rule for context**

Open `chat-frontend/src/styles/index.css` and skim for the existing `.message`, `.message-sender`, `.message-time` rules. Note the tokens used (e.g. `var(--color-bg-elevated)`, `var(--color-text-muted)` — pick up the actual token names from the file).

- [ ] **Step 2: Add menu styles**

Append to the end of `chat-frontend/src/styles/index.css`:

```css
/* Message action menu (kebab + popover + read-receipt + reader tooltip) */
.message {
  position: relative;
}

.message-action-menu {
  position: absolute;
  top: 4px;
  right: 6px;
}

.message-action-kebab {
  background: transparent;
  border: 0;
  padding: 2px 6px;
  font-size: 18px;
  line-height: 1;
  cursor: pointer;
  color: var(--color-text-muted, #888);
  opacity: 0;
  transition: opacity 80ms ease;
}

.message:hover .message-action-kebab,
.message-action-kebab:focus-visible,
.message-action-menu:focus-within .message-action-kebab {
  opacity: 1;
}

.message-action-popover {
  position: absolute;
  top: 100%;
  right: 0;
  min-width: 180px;
  background: var(--color-bg-elevated, #fff);
  color: var(--color-text, #111);
  border: 1px solid var(--color-border, #ddd);
  border-radius: 4px;
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.12);
  padding: 4px 0;
  z-index: 20;
}

.read-receipt-row {
  display: block;
  width: 100%;
  text-align: left;
  background: transparent;
  border: 0;
  padding: 6px 12px;
  font: inherit;
  color: inherit;
  cursor: default;
  position: relative;
}

.read-receipt-row[role="menuitem"]:not([aria-disabled="true"]) {
  cursor: help;
}

.read-receipt-row[role="menuitem"]:not([aria-disabled="true"]):hover,
.read-receipt-row[role="menuitem"]:not([aria-disabled="true"]):focus-visible {
  background: var(--color-bg-hover, rgba(0, 0, 0, 0.04));
  outline: none;
}

.read-receipt-loading,
.read-receipt-error,
.read-receipt-empty {
  color: var(--color-text-muted, #888);
}

.read-receipt-error {
  color: var(--color-error, #c0392b);
}

.read-receipt-tooltip {
  position: absolute;
  top: 100%;
  right: 0;
  margin: 4px 0 0 0;
  padding: 6px 10px;
  list-style: none;
  min-width: 160px;
  background: var(--color-bg-elevated, #fff);
  color: var(--color-text, #111);
  border: 1px solid var(--color-border, #ddd);
  border-radius: 4px;
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.12);
  z-index: 21;
}

.read-receipt-tooltip li {
  padding: 2px 0;
  white-space: nowrap;
}
```

If the file uses different token names (check Step 1), substitute them — the fallback values in `var(name, fallback)` make this resilient either way.

- [ ] **Step 3: Run the full test suite to confirm nothing regressed**

```
cd chat-frontend && npx vitest run
```

Expected: PASS.

- [ ] **Step 4: Manual smoke-test in the browser**

Start the dev server:

```
cd chat-frontend && npm run dev
```

Open the printed URL, sign in, and verify in a room with more than 1 member that:

1. Hovering one of your own messages reveals a `⋮` in the top-right of the row.
2. Hovering someone else's message shows no kebab.
3. Clicking the kebab opens a small panel; it briefly says `Loading…` and then `Read by X of Y`.
4. Hovering the `Read by X of Y` row (when X > 0) reveals a list with names like `Bob 鮑勃` (or `Bob` alone if no Chinese name).
5. Clicking outside the menu or pressing Escape closes it.

If anything in the visual layout looks off (kebab clipped, popover behind another element, tooltip overflows the viewport), tweak `top` / `right` / `z-index` in the rules above and re-run the smoke test. **Do not** add features beyond the spec while polishing.

- [ ] **Step 5: Commit**

```
git add chat-frontend/src/styles/index.css
git commit -m "feat(chat-frontend): style MessageActionMenu kebab, popover, and reader tooltip"
```

---

## Task 8: Final verification and push

**Files:** (none modified — verification only)

- [ ] **Step 1: Run the full chat-frontend test suite one more time**

```
cd chat-frontend && npx vitest run
```

Expected: PASS for every file.

- [ ] **Step 2: Push the branch**

```
git push -u origin claude/add-read-receipt-menu-IYLRf
```

If push fails due to a network error, retry up to 4 times with 2s/4s/8s/16s backoff.

- [ ] **Step 3: Stop**

Do NOT open a pull request unless the user explicitly asks. The branch is now ready for the user's review.

---

## Notes for the implementer

- **TDD discipline:** every task follows Red → Green → Commit. If a Red step accidentally passes, the test is wrong — fix it before implementing.
- **Hooks rules:** all `useState` / `useEffect` / `useCallback` calls in `MessageActionMenu` must appear before the `if (!isOwnMessage) return null` early-return. The provided code already respects this; keep it that way if you refactor.
- **`docs/client-api.md`:** no change is required. The RPC is already documented and the frontend is the consumer side. `CLAUDE.md` §5 only requires updating that doc when a *handler* changes.
- **Out of scope:** no backend changes, no other message actions (Edit/Delete/Reply), no live read-receipt updates after the menu opens. These are all left for future work per the spec.
