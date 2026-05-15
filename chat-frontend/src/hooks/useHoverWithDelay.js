import { useEffect, useRef, useState } from 'react'

/**
 * useHoverWithDelay — drives a hover-revealed UI element from JS state.
 *
 * Why not CSS :hover? CSS :hover only matches while the cursor is over the
 * element's geometric bounding box. A floating menu positioned outside the
 * trigger's box (e.g. above it via `top: -16px`) doesn't share the trigger's
 * hover state. When the cursor leaves the trigger toward the menu, :hover
 * goes false instantly and the menu disappears before the click registers.
 *
 * This hook bridges the gap: attach the same `handlers` to BOTH the trigger
 * (the bubble) and the floating UI (the menu). onMouseEnter cancels any
 * pending hide; onMouseLeave starts a `delayMs` timer. The user can freely
 * cross from one to the other without the UI vanishing.
 *
 * @param {number} delayMs - how long to wait after mouseleave before hiding.
 * @returns {{ hovered: boolean, handlers: { onMouseEnter, onMouseLeave } }}
 */
export default function useHoverWithDelay(delayMs = 200) {
  const [hovered, setHovered] = useState(false)
  const hideTimer = useRef(null)

  useEffect(() => () => clearTimeout(hideTimer.current), [])

  const onMouseEnter = () => {
    clearTimeout(hideTimer.current)
    setHovered(true)
  }
  const onMouseLeave = () => {
    clearTimeout(hideTimer.current)
    hideTimer.current = setTimeout(() => setHovered(false), delayMs)
  }

  return { hovered, handlers: { onMouseEnter, onMouseLeave } }
}
