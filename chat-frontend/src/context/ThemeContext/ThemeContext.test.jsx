import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, act } from '@testing-library/react'
import { ThemeProvider, useTheme } from './ThemeContext'
import { setMatchMedia, resetThemeState } from '../../../test/themeTestUtils'

function Probe() {
  const { theme } = useTheme()
  return <span data-testid="theme">{theme}</span>
}

function ToggleProbe() {
  const { theme, toggleTheme, setTheme } = useTheme()
  return (
    <>
      <span data-testid="theme">{theme}</span>
      <button data-testid="toggle" onClick={toggleTheme}>
        toggle
      </button>
      <button data-testid="set-dark" onClick={() => setTheme('dark')}>
        set dark
      </button>
      <button data-testid="set-light" onClick={() => setTheme('light')}>
        set light
      </button>
    </>
  )
}

beforeEach(resetThemeState)

afterEach(() => {
  resetThemeState()
  vi.unstubAllGlobals()
})

describe('ThemeProvider initial state', () => {
  it('uses stored "dark" from localStorage', () => {
    localStorage.setItem('theme', 'dark')
    setMatchMedia(false)
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('dark')
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
  })

  it('uses stored "light" from localStorage even when system prefers dark', () => {
    localStorage.setItem('theme', 'light')
    setMatchMedia(true)
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('light')
    expect(document.documentElement.getAttribute('data-theme')).toBe('light')
  })

  it('falls back to matchMedia when localStorage is empty', () => {
    setMatchMedia(true)
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('dark')
  })

  it('treats invalid localStorage value as absent', () => {
    localStorage.setItem('theme', 'turquoise')
    setMatchMedia(false)
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('light')
  })

  it('defaults to light when matchMedia is undefined', () => {
    delete window.matchMedia
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('light')
  })

  it('does not crash when localStorage.getItem throws on initial read', () => {
    const original = Storage.prototype.getItem
    Storage.prototype.getItem = vi.fn(() => {
      throw new Error('localStorage unavailable')
    })
    setMatchMedia(true)
    try {
      render(
        <ThemeProvider>
          <Probe />
        </ThemeProvider>
      )
      expect(screen.getByTestId('theme').textContent).toBe('dark')
    } finally {
      Storage.prototype.getItem = original
    }
  })
})

describe('ThemeProvider mutations', () => {
  it('setTheme updates state, attribute, and localStorage', () => {
    setMatchMedia(false)
    render(
      <ThemeProvider>
        <ToggleProbe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('light')

    act(() => {
      screen.getByTestId('set-dark').click()
    })

    expect(screen.getByTestId('theme').textContent).toBe('dark')
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
    expect(localStorage.getItem('theme')).toBe('dark')
  })

  it('toggleTheme flips light <-> dark and persists', () => {
    setMatchMedia(false)
    render(
      <ThemeProvider>
        <ToggleProbe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('light')

    act(() => {
      screen.getByTestId('toggle').click()
    })
    expect(screen.getByTestId('theme').textContent).toBe('dark')
    expect(localStorage.getItem('theme')).toBe('dark')

    act(() => {
      screen.getByTestId('toggle').click()
    })
    expect(screen.getByTestId('theme').textContent).toBe('light')
    expect(localStorage.getItem('theme')).toBe('light')
  })

  it('does not crash when localStorage.setItem throws', () => {
    setMatchMedia(false)
    const original = Storage.prototype.setItem
    Storage.prototype.setItem = vi.fn(() => {
      throw new Error('quota exceeded')
    })
    try {
      render(
        <ThemeProvider>
          <ToggleProbe />
        </ThemeProvider>
      )
      act(() => {
        screen.getByTestId('set-dark').click()
      })
      expect(screen.getByTestId('theme').textContent).toBe('dark')
      expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
    } finally {
      Storage.prototype.setItem = original
    }
  })
})

describe('ThemeProvider system-preference subscription', () => {
  it('updates theme when system pref changes and source=system', () => {
    const mql = setMatchMedia(false)
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('light')

    act(() => {
      mql.dispatchEvent({ matches: true })
    })
    expect(screen.getByTestId('theme').textContent).toBe('dark')
  })

  it('ignores system pref changes when an explicit choice was stored', () => {
    localStorage.setItem('theme', 'light')
    const mql = setMatchMedia(false)
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('light')

    act(() => {
      mql.dispatchEvent({ matches: true })
    })
    expect(screen.getByTestId('theme').textContent).toBe('light')
  })

  it('ignores system pref changes after an explicit setTheme', () => {
    const mql = setMatchMedia(false)
    render(
      <ThemeProvider>
        <ToggleProbe />
      </ThemeProvider>
    )
    expect(screen.getByTestId('theme').textContent).toBe('light')

    act(() => {
      screen.getByTestId('set-light').click()
    })
    act(() => {
      mql.dispatchEvent({ matches: true })
    })
    expect(screen.getByTestId('theme').textContent).toBe('light')
  })
})
