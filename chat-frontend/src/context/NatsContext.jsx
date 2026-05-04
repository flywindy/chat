import { createContext, useContext, useRef, useState, useCallback } from 'react'
import { connect as natsConnect, StringCodec, jwtAuthenticator } from 'nats.ws'
import { createUser } from 'nkeys.js'
import { AUTH_URL, NATS_URL } from '../lib/runtimeConfig'

export const NatsContext = createContext(null)

const sc = StringCodec()

export function NatsProvider({ children }) {
  const ncRef = useRef(null)
  const [connected, setConnected] = useState(false)
  const [user, setUser] = useState(null)
  const [error, setError] = useState(null)

  const authUrl = AUTH_URL
  const natsUrl = NATS_URL

  // connectToNats accepts an opts object with one of two shapes:
  //   { mode: 'dev', account, siteId } — dev login by account name
  //   { mode: 'sso', ssoToken, siteId } — production login via OIDC access token
  const connectToNats = useCallback(async (opts) => {
    setError(null)

    const { mode, account, ssoToken, siteId } = opts || {}

    const nkey = createUser()
    const natsPublicKey = nkey.getPublicKey()

    const body =
      mode === 'sso'
        ? { ssoToken, natsPublicKey }
        : { account, natsPublicKey }

    const authResp = await fetch(`${authUrl}/auth`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })

    if (!authResp.ok) {
      const errBody = await authResp.json().catch(() => ({}))
      throw new Error(errBody.error || `Auth failed: ${authResp.status}`)
    }

    const { natsJwt, user: userInfo } = await authResp.json()

    const nc = await natsConnect({
      servers: natsUrl,
      authenticator: jwtAuthenticator(natsJwt, nkey.getSeed()),
    })

    ncRef.current = nc
    setUser({ ...userInfo, siteId })
    setConnected(true)

    nc.closed().then((err) => {
      if (err) {
        setError(`Disconnected: ${err.message}`)
      }
      setConnected(false)
    })
  }, [authUrl, natsUrl])

  const request = useCallback(async (subject, data = {}) => {
    if (!ncRef.current) throw new Error('Not connected')
    const payload = sc.encode(JSON.stringify(data))
    const resp = await ncRef.current.request(subject, payload, { timeout: 5000 })
    const parsed = JSON.parse(sc.decode(resp.data))
    if (parsed.error) throw new Error(parsed.error)
    return parsed
  }, [])

  const publish = useCallback((subject, data = {}) => {
    if (!ncRef.current) throw new Error('Not connected')
    const payload = sc.encode(JSON.stringify(data))
    ncRef.current.publish(subject, payload)
  }, [])

  const subscribe = useCallback((subject, callback) => {
    if (!ncRef.current) throw new Error('Not connected')
    const sub = ncRef.current.subscribe(subject)
    ;(async () => {
      for await (const msg of sub) {
        try {
          const data = JSON.parse(sc.decode(msg.data))
          callback(data)
        } catch {
          // skip malformed messages
        }
      }
    })()
    return sub
  }, [])

  const disconnect = useCallback(async () => {
    if (ncRef.current) {
      await ncRef.current.drain()
      ncRef.current = null
    }
    setConnected(false)
    setUser(null)
  }, [])

  return (
    <NatsContext.Provider value={{
      connected, user, error,
      connect: connectToNats, request, publish, subscribe, disconnect,
    }}>
      {children}
    </NatsContext.Provider>
  )
}

export function useNats() {
  const ctx = useContext(NatsContext)
  if (!ctx) throw new Error('useNats must be used within NatsProvider')
  return ctx
}
