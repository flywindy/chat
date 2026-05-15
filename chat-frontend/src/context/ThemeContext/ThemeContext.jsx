import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'

const STORAGE_KEY = 'theme'
const MEDIA_QUERY = '(prefers-color-scheme: dark)'

const ThemeContext = createContext(null)

function resolveInitialTheme() {
  let stored = null
  try {
    stored = localStorage.getItem(STORAGE_KEY)
  } catch {
    // localStorage unavailable; treat as no stored value
  }
  if (stored === 'light' || stored === 'dark') {
    return { theme: stored, source: 'user' }
  }
  if (typeof window.matchMedia === 'function') {
    const prefersDark = window.matchMedia(MEDIA_QUERY).matches
    return { theme: prefersDark ? 'dark' : 'light', source: 'system' }
  }
  return { theme: 'light', source: 'system' }
}

function persistTheme(theme) {
  try {
    localStorage.setItem(STORAGE_KEY, theme)
  } catch {
    // tolerate private mode / quota errors
  }
}

export function ThemeProvider({ children }) {
  const [state, setState] = useState(() => resolveInitialTheme())
  const sourceRef = useRef(state.source)

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', state.theme)
  }, [state.theme])

  useEffect(() => {
    sourceRef.current = state.source
  }, [state.source])

  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return
    const mql = window.matchMedia(MEDIA_QUERY)
    const handler = (event) => {
      if (sourceRef.current !== 'system') return
      const next = event.matches ? 'dark' : 'light'
      setState((prev) => (prev.theme === next ? prev : { theme: next, source: 'system' }))
    }
    mql.addEventListener('change', handler)
    return () => mql.removeEventListener('change', handler)
  }, [])

  const setTheme = useCallback((next) => {
    if (next !== 'light' && next !== 'dark') return
    persistTheme(next)
    setState((prev) =>
      prev.theme === next && prev.source === 'user' ? prev : { theme: next, source: 'user' },
    )
  }, [])

  const toggleTheme = useCallback(() => {
    setState((prev) => {
      const next = prev.theme === 'dark' ? 'light' : 'dark'
      persistTheme(next)
      return { theme: next, source: 'user' }
    })
  }, [])

  const value = useMemo(
    () => ({ theme: state.theme, setTheme, toggleTheme }),
    [state.theme, setTheme, toggleTheme],
  )

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
}

export function useTheme() {
  const ctx = useContext(ThemeContext)
  if (ctx === null) {
    throw new Error('useTheme must be used within a ThemeProvider')
  }
  return ctx
}
