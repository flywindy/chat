import { useState } from 'react'
import { useNats } from '@/context/NatsContext'
import { DEFAULT_SITE_ID, DEV_MODE } from '@/lib/runtimeConfig'
import { getOidcManager } from '@/api/auth/oidcClient'
import './style.css'

export default function LoginPage() {
  const { connect, error: natsError } = useNats()
  const defaultSiteId = DEFAULT_SITE_ID

  const [account, setAccount] = useState('')
  const [siteId, setSiteId] = useState(defaultSiteId)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)

  const handleDevSubmit = async (e) => {
    e.preventDefault()
    if (!account.trim()) return

    setLoading(true)
    setError(null)
    try {
      await connect({
        mode: 'dev',
        account: account.trim(),
        siteId: siteId.trim(),
      })
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }

  const handleKeycloakLogin = async () => {
    setLoading(true)
    setError(null)
    try {
      // Stash siteId so the /oidc-callback page can read it after redirect.
      window.sessionStorage.setItem('oidc.siteId', siteId.trim())
      const manager = getOidcManager()
      await manager.signinRedirect()
      // Browser navigates away — code below this point is unreachable in prod.
    } catch (err) {
      setError(err.message)
      setLoading(false)
    }
  }

  if (DEV_MODE) {
    return (
      <div className="login-page">
        <form className="login-form" onSubmit={handleDevSubmit}>
          <h1>Chat</h1>
          <p className="login-subtitle">Dev Mode Login</p>

          <label htmlFor="account">Account</label>
          <input
            id="account"
            type="text"
            value={account}
            onChange={(e) => setAccount(e.target.value)}
            placeholder="e.g. alice"
            autoFocus
            disabled={loading}
          />

          <label htmlFor="siteId">Site ID</label>
          <input
            id="siteId"
            type="text"
            value={siteId}
            onChange={(e) => setSiteId(e.target.value)}
            disabled={loading}
          />

          <button type="submit" disabled={loading || !account.trim()}>
            {loading ? 'Connecting...' : 'Connect'}
          </button>

          {(error || natsError) && (
            <div className="login-error">{error || natsError}</div>
          )}
        </form>
      </div>
    )
  }

  return (
    <div className="login-page">
      <div className="login-form">
        <h1>Chat</h1>
        <p className="login-subtitle">Sign in with Keycloak</p>

        <label htmlFor="siteId">Site ID</label>
        <input
          id="siteId"
          type="text"
          value={siteId}
          onChange={(e) => setSiteId(e.target.value)}
          disabled={loading}
        />

        <button type="button" onClick={handleKeycloakLogin} disabled={loading}>
          {loading ? 'Redirecting...' : 'Sign in with Keycloak'}
        </button>

        {(error || natsError) && (
          <div className="login-error">{error || natsError}</div>
        )}
      </div>
    </div>
  )
}
