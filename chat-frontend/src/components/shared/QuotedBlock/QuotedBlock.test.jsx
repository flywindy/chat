import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import QuotedBlock from './QuotedBlock'

const snap = { id: 'm1', senderName: 'alice', content: 'hello world from another time' }

describe('QuotedBlock — chip variant', () => {
  it('renders sender and content', () => {
    render(<QuotedBlock variant="chip" snapshot={snap} onClear={() => {}} />)
    expect(screen.getByText('alice')).toBeInTheDocument()
    expect(screen.getByText(/hello world/)).toBeInTheDocument()
  })

  it('clicking ✕ invokes onClear', () => {
    const onClear = vi.fn()
    render(<QuotedBlock variant="chip" snapshot={snap} onClear={onClear} />)
    fireEvent.click(screen.getByRole('button', { name: /clear quoted message/i }))
    expect(onClear).toHaveBeenCalled()
  })
})

describe('QuotedBlock — bubble variant', () => {
  it('renders sender and content, click invokes onClick with snapshot.id', () => {
    const onClick = vi.fn()
    render(<QuotedBlock variant="bubble" snapshot={snap} onClick={onClick} />)
    fireEvent.click(screen.getByText(/hello world/).closest('.quoted-block'))
    expect(onClick).toHaveBeenCalledWith('m1')
  })

  it('renders no ✕ in bubble variant', () => {
    render(<QuotedBlock variant="bubble" snapshot={snap} onClick={() => {}} />)
    expect(screen.queryByRole('button', { name: /clear/i })).not.toBeInTheDocument()
  })
})

describe('QuotedBlock — deleted snapshot', () => {
  it('renders "[message deleted]" placeholder and disables click in bubble variant', () => {
    const onClick = vi.fn()
    render(
      <QuotedBlock
        variant="bubble"
        snapshot={{ id: 'm2', senderName: 'alice', content: '', deleted: true }}
        onClick={onClick}
      />
    )
    expect(screen.getByText(/message deleted/i)).toBeInTheDocument()
    fireEvent.click(screen.getByText(/message deleted/i).closest('.quoted-block'))
    expect(onClick).not.toHaveBeenCalled()
  })
})
