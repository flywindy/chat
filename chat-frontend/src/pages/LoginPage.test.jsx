import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

vi.mock('../lib/runtimeConfig', () => ({
  DEFAULT_SITE_ID: 'site-local',
  DEV_MODE: true,
}))

vi.mock('../lib/oidcClient', () => ({
  getOidcManager: vi.fn(),
}))

import LoginPage from './LoginPage'
import { useNats } from '../context/NatsContext'
import * as runtimeConfig from '../lib/runtimeConfig'
import { getOidcManager } from '../lib/oidcClient'

beforeEach(() => {
  useNats.mockReset()
  getOidcManager.mockReset()
  window.sessionStorage.clear()
  // Reset DEV_MODE default before each test.
  runtimeConfig.DEV_MODE = true
})

afterEach(() => {
  window.sessionStorage.clear()
})

describe('LoginPage in DEV_MODE=true', () => {
  beforeEach(() => {
    runtimeConfig.DEV_MODE = true
  })

  it('renders the dev account form', () => {
    useNats.mockReturnValue({ connect: vi.fn(), error: null })
    render(<LoginPage />)
    expect(screen.getByLabelText(/account/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/site id/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /keycloak/i })).not.toBeInTheDocument()
  })

  it('submits with {mode: "dev", account, siteId}', async () => {
    const connect = vi.fn().mockResolvedValue(undefined)
    useNats.mockReturnValue({ connect, error: null })

    render(<LoginPage />)

    fireEvent.change(screen.getByLabelText(/account/i), { target: { value: 'alice' } })
    fireEvent.change(screen.getByLabelText(/site id/i), { target: { value: 'site-A' } })
    fireEvent.click(screen.getByRole('button', { name: /connect/i }))

    await waitFor(() => {
      expect(connect).toHaveBeenCalledWith({
        mode: 'dev',
        account: 'alice',
        siteId: 'site-A',
      })
    })
  })

  it('renders connect error from connect()', async () => {
    const connect = vi.fn().mockRejectedValue(new Error('boom'))
    useNats.mockReturnValue({ connect, error: null })

    render(<LoginPage />)
    fireEvent.change(screen.getByLabelText(/account/i), { target: { value: 'alice' } })
    fireEvent.click(screen.getByRole('button', { name: /connect/i }))

    await waitFor(() => {
      expect(screen.getByText(/boom/i)).toBeInTheDocument()
    })
  })
})

describe('LoginPage in DEV_MODE=false', () => {
  beforeEach(() => {
    runtimeConfig.DEV_MODE = false
  })

  it('renders a Keycloak login button instead of the account form', () => {
    useNats.mockReturnValue({ connect: vi.fn(), error: null })
    render(<LoginPage />)
    expect(screen.getByRole('button', { name: /keycloak/i })).toBeInTheDocument()
    expect(screen.queryByLabelText(/account/i)).not.toBeInTheDocument()
  })

  it('stores siteId in sessionStorage and calls signinRedirect on button click', async () => {
    useNats.mockReturnValue({ connect: vi.fn(), error: null })
    const signinRedirect = vi.fn().mockResolvedValue(undefined)
    getOidcManager.mockReturnValue({ signinRedirect })

    render(<LoginPage />)
    fireEvent.change(screen.getByLabelText(/site id/i), { target: { value: 'site-B' } })
    fireEvent.click(screen.getByRole('button', { name: /keycloak/i }))

    await waitFor(() => {
      expect(signinRedirect).toHaveBeenCalled()
    })
    expect(window.sessionStorage.getItem('oidc.siteId')).toBe('site-B')
  })

  it('shows an error if signinRedirect throws', async () => {
    useNats.mockReturnValue({ connect: vi.fn(), error: null })
    const signinRedirect = vi.fn().mockRejectedValue(new Error('idp down'))
    getOidcManager.mockReturnValue({ signinRedirect })

    render(<LoginPage />)
    fireEvent.click(screen.getByRole('button', { name: /keycloak/i }))

    await waitFor(() => {
      expect(screen.getByText(/idp down/i)).toBeInTheDocument()
    })
  })
})
