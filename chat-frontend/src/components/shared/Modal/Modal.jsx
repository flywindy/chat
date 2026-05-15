import { useEffect } from 'react'

/**
 * Modal — reusable base dialog. Renders a centered card on a greyed-out
 * overlay. Click on overlay or Esc closes (calls `onClose`). Click inside
 * the card does NOT close.
 *
 * Esc is captured at the window level (capture phase + stopPropagation) so
 * the dialog "claims" Esc and ancestor handlers (e.g. ThreadRightBar's
 * close-thread shortcut) don't fire alongside it.
 */
export default function Modal({ onClose, children, labelledBy }) {
  useEffect(() => {
    const handler = (e) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        onClose?.()
      }
    }
    window.addEventListener('keydown', handler, true)
    return () => window.removeEventListener('keydown', handler, true)
  }, [onClose])

  return (
    <div
      className="dialog-overlay"
      onMouseDown={(e) => {
        // Backdrop click → close. Only when the actual overlay (not a child)
        // is the mousedown target.
        if (e.target === e.currentTarget) onClose?.()
      }}
    >
      <div
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={labelledBy}
        onMouseDown={(e) => e.stopPropagation()}
      >
        {children}
      </div>
    </div>
  )
}
