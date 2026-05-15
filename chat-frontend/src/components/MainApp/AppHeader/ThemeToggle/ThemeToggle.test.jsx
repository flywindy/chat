import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, act } from '@testing-library/react'
import { ThemeProvider } from '@/context/ThemeContext'
import ThemeToggle from './ThemeToggle'
import { setMatchMedia, resetThemeState } from '../../../../../test/themeTestUtils'

beforeEach(resetThemeState)

afterEach(() => {
  resetThemeState()
  vi.unstubAllGlobals()
})

describe('ThemeToggle', () => {
  it('renders moon icon and "switch to dark" label when current theme is light', () => {
    setMatchMedia(false)
    render(
      <ThemeProvider>
        <ThemeToggle />
      </ThemeProvider>
    )
    const btn = screen.getByRole('button', { name: /switch to dark theme/i })
    expect(btn).toBeInTheDocument()
    expect(btn.querySelector('[data-icon="moon"]')).not.toBeNull()
  })

  it('renders sun icon and "switch to light" label when current theme is dark', () => {
    localStorage.setItem('theme', 'dark')
    setMatchMedia(false)
    render(
      <ThemeProvider>
        <ThemeToggle />
      </ThemeProvider>
    )
    const btn = screen.getByRole('button', { name: /switch to light theme/i })
    expect(btn).toBeInTheDocument()
    expect(btn.querySelector('[data-icon="sun"]')).not.toBeNull()
  })

  it('clicking flips the theme and updates the data-theme attribute', () => {
    setMatchMedia(false)
    render(
      <ThemeProvider>
        <ThemeToggle />
      </ThemeProvider>
    )
    expect(document.documentElement.getAttribute('data-theme')).toBe('light')

    act(() => {
      screen.getByRole('button', { name: /switch to dark theme/i }).click()
    })

    expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
    expect(localStorage.getItem('theme')).toBe('dark')
  })
})
