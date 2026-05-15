import QuotedBlock from '../QuotedBlock/QuotedBlock'
import './style.css'

export default function MessageInputForm({
  value,
  onChange,
  onSubmit,
  placeholder,
  disabled,
  quotedTarget,
  onClearQuote,
  inputRef,
}) {
  const handleSubmit = (e) => {
    e?.preventDefault?.()
    if (disabled) return
    if (!value || !value.trim()) return
    onSubmit()
  }

  const handleKeyDown = (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSubmit(e)
    }
  }

  const canSubmit = !disabled && value && value.trim().length > 0

  return (
    <form className="message-input-form" onSubmit={handleSubmit}>
      {quotedTarget && (
        <QuotedBlock variant="chip" snapshot={quotedTarget} onClear={onClearQuote} />
      )}
      <div className="message-input-row">
        <input
          ref={inputRef}
          type="text"
          value={value ?? ''}
          onChange={(e) => onChange(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder={placeholder}
          disabled={disabled}
        />
        <button type="submit" disabled={!canSubmit}>
          Send
        </button>
      </div>
    </form>
  )
}
