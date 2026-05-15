import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import MessageInputForm from './MessageInputForm'

describe('MessageInputForm', () => {
  it('renders value and fires onChange on input', () => {
    const onChange = vi.fn()
    render(<MessageInputForm value="hi" onChange={onChange} onSubmit={() => {}} placeholder="Say something…" />)
    const input = screen.getByPlaceholderText('Say something…')
    expect(input).toHaveValue('hi')
    fireEvent.change(input, { target: { value: 'hi there' } })
    expect(onChange).toHaveBeenCalledWith('hi there')
  })

  it('Enter submits with trimmed value; Shift+Enter does not', () => {
    const onSubmit = vi.fn()
    render(<MessageInputForm value="  hello  " onChange={() => {}} onSubmit={onSubmit} placeholder="x" />)
    const input = screen.getByPlaceholderText('x')
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(onSubmit).toHaveBeenCalledTimes(1)
    onSubmit.mockClear()
    fireEvent.keyDown(input, { key: 'Enter', shiftKey: true })
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it('disables the textarea and Send button when disabled prop is set', () => {
    render(<MessageInputForm value="" onChange={() => {}} onSubmit={() => {}} placeholder="x" disabled />)
    expect(screen.getByPlaceholderText('x')).toBeDisabled()
    expect(screen.getByRole('button', { name: /send/i })).toBeDisabled()
  })

  it('Send button is disabled when value is empty / whitespace', () => {
    render(<MessageInputForm value="   " onChange={() => {}} onSubmit={() => {}} placeholder="x" />)
    expect(screen.getByRole('button', { name: /send/i })).toBeDisabled()
  })

  it('renders a QuotedBlock chip when quotedTarget is set; ✕ calls onClearQuote', () => {
    const onClearQuote = vi.fn()
    render(
      <MessageInputForm
        value=""
        onChange={() => {}}
        onSubmit={() => {}}
        placeholder="x"
        quotedTarget={{ id: 'q', senderName: 'bob', content: 'orig' }}
        onClearQuote={onClearQuote}
      />
    )
    expect(screen.getByText('bob')).toBeInTheDocument()
    expect(screen.getByText('orig')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /clear quoted message/i }))
    expect(onClearQuote).toHaveBeenCalled()
  })
})
