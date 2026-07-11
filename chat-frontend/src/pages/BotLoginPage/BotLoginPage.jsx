import { useState } from 'react'
import { useNats } from '@/context/NatsContext'
import { botLogin, changePassword } from '@/api/auth/botAuth'
import { formatAsyncJobError } from '@/api'
import ChangePasswordForm from '@/pages/ChangePasswordPage'
import './style.css'

// Login, then a forced change-password step if required; authToken stays valid across
// the change so the user lands directly in chat with no re-login.
export default function BotLoginPage() {
  const { connect } = useNats()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [bundle, setBundle] = useState(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)

  const connectSession = (resolved) => connect({ mode: 'session', bundle: resolved })

  const handleLogin = async (e) => {
    e.preventDefault()
    if (!username.trim() || !password) return
    setLoading(true)
    setError(null)
    try {
      const resolved = await botLogin({ username: username.trim(), password })
      if (resolved.requirePasswordChange) {
        setBundle(resolved)
        return
      }
      await connectSession(resolved)
    } catch (err) {
      setError(formatAsyncJobError(err))
    } finally {
      setLoading(false)
    }
  }

  const handleChangePassword = async ({ oldPassword, newPassword }) => {
    setLoading(true)
    setError(null)
    try {
      await changePassword({ baseUrl: bundle.baseUrl, authToken: bundle.authToken, oldPassword, newPassword })
      await connectSession({ ...bundle, requirePasswordChange: false })
    } catch (err) {
      setError(formatAsyncJobError(err))
    } finally {
      setLoading(false)
    }
  }

  if (bundle?.requirePasswordChange) {
    return <ChangePasswordForm onSubmit={handleChangePassword} error={error} loading={loading} />
  }

  return (
    <div className="login-page">
      <form className="login-form" onSubmit={handleLogin}>
        <h1>Chat</h1>
        <p className="login-subtitle">Bot / Admin sign in</p>

        <label htmlFor="bot-username">Username</label>
        <input id="bot-username" type="text" value={username}
          onChange={(e) => setUsername(e.target.value)} autoFocus disabled={loading} />

        <label htmlFor="bot-password">Password</label>
        <input id="bot-password" type="password" value={password}
          onChange={(e) => setPassword(e.target.value)} disabled={loading} />

        <button type="submit" disabled={loading || !username.trim() || !password}>
          {loading ? 'Signing in…' : 'Sign in'}
        </button>

        {error && <div className="login-error">{error}</div>}
      </form>
    </div>
  )
}
