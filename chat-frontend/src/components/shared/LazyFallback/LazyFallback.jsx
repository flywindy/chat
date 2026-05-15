import './style.css'

/**
 * Suspense fallback for our lazy-loaded surfaces. Two variants:
 *
 *   variant="dialog"  — full-viewport overlay with a centered spinner;
 *                       reuses the dialog overlay so a click that
 *                       opened a lazy dialog lands on something
 *                       visually anchored to where the dialog will
 *                       appear.
 *
 *   variant="inline"  — a small spinner that fills the parent panel.
 *                       Used for side panes / right rails (search
 *                       results, thread right-bar, in-room search).
 *
 * Default (no variant) is `inline`. Pass `label` if you want a hint
 * under the spinner; otherwise it stays purely visual (aria-busy is
 * set so screen readers still announce the loading state).
 */
export default function LazyFallback({ variant = 'inline', label }) {
  if (variant === 'dialog') {
    return (
      <div className="dialog-overlay" aria-busy="true" aria-live="polite">
        <div className="lazy-fallback-spinner" />
        {label && <span className="lazy-fallback-label">{label}</span>}
      </div>
    )
  }
  return (
    <div className="lazy-fallback-inline" aria-busy="true" aria-live="polite">
      <div className="lazy-fallback-spinner" />
      {label && <span className="lazy-fallback-label">{label}</span>}
    </div>
  )
}
