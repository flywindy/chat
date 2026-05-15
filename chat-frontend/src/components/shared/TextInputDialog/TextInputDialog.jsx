import { useEffect, useRef, useState } from 'react'
import Modal from '../Modal/Modal'

/**
 * TextInputDialog — small modal for editing a single line of text.
 * Used for in-place message edits (and other "quick edit" UX). Reuses
 * the project's Modal primitive so backdrop / Esc / focus behave like
 * every other dialog.
 *
 * Props:
 *   title         — heading text, used as the dialog accessible name.
 *   initialValue  — pre-fills the input.
 *   confirmLabel  — defaults to "Save".
 *   placeholder   — input placeholder.
 *   onSave(value) — called with trimmed value on confirm; close handled by parent.
 *   onCancel()    — called on backdrop click, Esc, or Cancel button.
 */
export default function TextInputDialog({
  title,
  initialValue = '',
  confirmLabel = 'Save',
  placeholder,
  onSave,
  onCancel,
}) {
  const [text, setText] = useState(initialValue)
  const inputRef = useRef(null)
  const headingId = 'text-input-dialog-heading'

  useEffect(() => {
    inputRef.current?.focus()
    inputRef.current?.select()
  }, [])

  const trimmed = text.trim()
  const canSave = trimmed.length > 0 && trimmed !== initialValue.trim()

  const submit = () => {
    if (!canSave) return
    onSave?.(trimmed)
  }

  return (
    <Modal onClose={onCancel} labelledBy={headingId}>
      <h2 id={headingId}>{title}</h2>
      <input
        ref={inputRef}
        type="text"
        value={text}
        placeholder={placeholder}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault()
            submit()
          }
        }}
      />
      <div className="dialog-actions">
        <button type="button" className="dialog-cancel" onClick={onCancel}>Cancel</button>
        <button type="button" onClick={submit} disabled={!canSave}>{confirmLabel}</button>
      </div>
    </Modal>
  )
}
