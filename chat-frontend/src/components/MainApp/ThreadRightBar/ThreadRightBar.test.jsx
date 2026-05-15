import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import ThreadRightBar from './ThreadRightBar'

const closeThread = vi.fn()
vi.mock('@/context/ThreadEventsContext', () => ({
  useThreadEvents: () => ({ activeParent: { messageId: 'p1' }, closeThread }),
}))
vi.mock('./ThreadMessageArea/ThreadMessageArea', () => ({
  default: ({ onReply }) => (
    <div>
      area
      <button type="button" onClick={() => onReply?.({ id: 'r-orig', sender: { account: 'bob' }, content: 'orig' })}>
        fire-reply
      </button>
    </div>
  ),
}))
vi.mock('./ThreadMessageInput/ThreadMessageInput', () => ({
  default: ({ quotedTarget, onClearQuote }) => (
    <div>
      input
      {quotedTarget && (
        <>
          <span data-testid="t-staged">{quotedTarget.id}</span>
          <button type="button" onClick={onClearQuote}>t-clear</button>
        </>
      )}
    </div>
  ),
}))

describe('ThreadRightBar', () => {
  beforeEach(() => closeThread.mockClear())

  it('renders header, area, and input', () => {
    render(<ThreadRightBar />)
    expect(screen.getByText('Thread')).toBeInTheDocument()
    expect(screen.getByText('area')).toBeInTheDocument()
    expect(screen.getByText('input')).toBeInTheDocument()
  })

  it('✕ close button calls closeThread', () => {
    render(<ThreadRightBar />)
    fireEvent.click(screen.getByRole('button', { name: /close thread/i }))
    expect(closeThread).toHaveBeenCalled()
  })

  it('Reply inside the thread stages a quote in the thread input', () => {
    render(<ThreadRightBar />)
    fireEvent.click(screen.getByText('fire-reply'))
    expect(screen.getByTestId('t-staged').textContent).toBe('r-orig')
  })

  it('clearing the chip removes the staged quote', () => {
    render(<ThreadRightBar />)
    fireEvent.click(screen.getByText('fire-reply'))
    fireEvent.click(screen.getByText('t-clear'))
    expect(screen.queryByTestId('t-staged')).not.toBeInTheDocument()
  })
})

describe('ThreadRightBar — focus on mount', () => {
  it('focuses the ✕ close button when mounted', () => {
    render(<ThreadRightBar />)
    const closeBtn = screen.getByRole('button', { name: /close thread/i })
    expect(document.activeElement).toBe(closeBtn)
  })
})

describe('ThreadRightBar — Esc to close', () => {
  beforeEach(() => closeThread.mockClear())

  it('Esc on the panel closes the thread', () => {
    const { container } = render(<ThreadRightBar />)
    fireEvent.keyDown(container.querySelector('.thread-rightbar'), { key: 'Escape' })
    expect(closeThread).toHaveBeenCalled()
  })

  it('Esc does NOT close when target is an inner input (let inputs handle their own Esc)', () => {
    const { container } = render(<ThreadRightBar />)
    const input = document.createElement('input')
    container.querySelector('.thread-rightbar').appendChild(input)
    closeThread.mockClear()
    fireEvent.keyDown(input, { key: 'Escape' })
    expect(closeThread).not.toHaveBeenCalled()
  })
})
