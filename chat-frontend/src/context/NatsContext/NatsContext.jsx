import { createContext, useContext, useRef, useState, useCallback, useEffect, useMemo } from 'react'
import { connect as natsConnect, StringCodec, headers as natsHeaders } from 'nats.ws'
import { createUser } from 'nkeys.js'
import { PORTAL_URL } from '@/lib/runtimeConfig'
import { useDebug } from '@/context/DebugContext'
import { useJwtRefresh } from './useJwtRefresh'
import {
  requestWithAsyncResult as asyncJobRequest,
  requestSync as asyncJobRequestSync,
  AsyncJobError,
  ASYNC_JOB_ERROR_KINDS,
} from '@/api/_transport/asyncJob'

export const NatsContext = createContext(null)

const sc = StringCodec()

// Both services emit the errcode envelope {code, reason?, error, metadata?}.
// Legacy deployments may return {error} only — callers fall back to message.
async function throwEnvelopeError(resp, fallbackMsg) {
  const errBody = await resp.json().catch(() => ({}))
  throw new AsyncJobError(
    errBody.error || `${fallbackMsg}: ${resp.status}`,
    ASYNC_JOB_ERROR_KINDS.SyncError,
    { code: errBody.code, reason: errBody.reason, metadata: errBody.metadata },
  )
}

export function NatsProvider({ children }) {
  const ncRef = useRef(null)
  // Bumped on every connect and disconnect so a superseded connection's
  // long-lived nc.closed() callback can detect it is stale and drop its write
  // (codebase "stale-cycle protection" convention).
  const connectGenRef = useRef(0)
  const [connected, setConnected] = useState(false)
  const [user, setUser] = useState(null)
  const [error, setError] = useState(null)

  // Resolved per user by the portal lookup at connect time; the JWT-refresh
  // loop reads it through the getter so re-mints follow the resolved site.
  const authUrlRef = useRef(null)
  const getAuthUrl = useCallback(() => authUrlRef.current, [])

  // Keep the live debug settings in refs so the transport callbacks can read
  // them at send time without being recreated (and re-rendering consumers) on
  // every change. When the level is not 'off' every request/publish carries an
  // `X-Debug: <level>` header; when payload capture is on it also carries
  // `X-Debug-Payload: 1` (independent of the level).
  const { level: debugLevel, payload: debugPayload } = useDebug()
  const debugLevelRef = useRef(debugLevel)
  const debugPayloadRef = useRef(debugPayload)
  useEffect(() => { debugLevelRef.current = debugLevel }, [debugLevel])
  useEffect(() => { debugPayloadRef.current = debugPayload }, [debugPayload])

  const buildHeaders = useCallback(() => {
    const lvl = debugLevelRef.current
    const payload = debugPayloadRef.current
    const wantsDebug = lvl && lvl !== 'off'
    if (!wantsDebug && !payload) return undefined
    const h = natsHeaders()
    if (wantsDebug) h.set('X-Debug', lvl)
    if (payload) h.set('X-Debug-Payload', '1')
    return h
  }, [])

  const { authenticator, setCredentials, stop } = useJwtRefresh({ getAuthUrl, ncRef })

  /**
   * Resolve the user's home site via the portal lookup, authenticate
   * against that site's auth-service, and open the NATS WebSocket
   * connection to that site. On success, `user`/`connected` flip true and
   * any subsequent server-initiated close updates `error`.
   *
   * @param {Object} opts
   * @param {'dev'|'sso'} opts.mode
   * @param {string} [opts.account]   Dev mode: account name to log in as.
   * @param {string} [opts.ssoToken]  Production mode: OIDC access token.
   * @throws if the portal lookup or auth-service rejects, or the NATS
   *   handshake fails.
   */
  const connectToNats = useCallback(async (opts) => {
    const myGen = ++connectGenRef.current
    setError(null)

    const { mode, account, ssoToken } = opts || {}

    // 1) Site discovery: which auth-service, which NATS, which siteId. Discovery
    // only — the portal validates no token; the account (derived from the SSO
    // token's preferred_username in prod) is the lookup key. auth-service is the
    // real gate that re-validates the token before minting the JWT.
    const lookupResp = await fetch(`${PORTAL_URL}/api/userInfo?account=${encodeURIComponent(account ?? '')}`)
    if (!lookupResp.ok) {
      await throwEnvelopeError(lookupResp, 'Portal lookup failed')
    }
    const portal = await lookupResp.json()
    const nextAuthUrl = portal.authServiceUrl

    // 2) Mint the NATS JWT at the resolved site's auth-service.
    const nkey = createUser()
    const natsPublicKey = nkey.getPublicKey()

    const body =
      mode === 'sso'
        ? { ssoToken, natsPublicKey }
        : { account, natsPublicKey }

    const authResp = await fetch(`${nextAuthUrl}/auth`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })

    if (!authResp.ok) {
      await throwEnvelopeError(authResp, 'Auth failed')
    }

    const { natsJwt, user: userInfo } = await authResp.json()

    // A failed dial must roll back: stop() disarms the refresh loop the
    // staged credentials armed, and the auth URL is committed only on success.
    try {
      // Populate the credential refs BEFORE connecting so the dynamic
      // authenticator's getters return the right values during the handshake.
      setCredentials({
        jwt: natsJwt,
        seed: nkey.getSeed(),
        natsPublicKey,
        refreshable: mode === 'sso',
      })

      // 3) Dial the resolved site's NATS.
      const nc = await natsConnect({
        servers: portal.natsUrl,
        authenticator,
      })

      authUrlRef.current = nextAuthUrl
      ncRef.current = nc
      setUser({ ...userInfo, siteId: portal.siteId })
      setConnected(true)

      nc.closed().then((err) => {
        // A newer connect or a disconnect bumped the generation; this old
        // link's close must not clobber the live session's state.
        if (myGen !== connectGenRef.current) return
        if (err) {
          setError(`Disconnected: ${err.message}`)
        }
        setConnected(false)
      })
    } catch (err) {
      stop()
      throw err
    }
  }, [authenticator, setCredentials, stop])

  /**
   * Send a synchronous NATS request/reply. Use this for handlers that
   * return their full result inline (e.g. `member.list`, `search.rooms`).
   * For deferred-result operations use `requestWithAsyncResult` instead.
   *
   * @param {string} subject
   * @param {unknown} [data={}]  JSON-serialisable payload.
   * @returns {Promise<unknown>} Parsed JSON reply.
   * @throws {AsyncJobError} On error replies the thrown error carries
   *   `.code` (always) and `.reason`/`.metadata` (when the backend emits
   *   them). Branch on `reason ?? code`; `.message` is the user-safe text
   *   for display only. Wire-level failures (not connected, request
   *   timeout) still throw a plain Error.
   */
  const request = useCallback(async (subject, data = {}) => {
    if (!ncRef.current) throw new Error('Not connected')
    return asyncJobRequestSync(ncRef.current, subject, data, {
      debugLevel: debugLevelRef.current,
      debugPayload: debugPayloadRef.current,
    })
  }, [])

  /**
   * Two-phase request/reply for operations whose sync reply is just
   * "accepted" — the real outcome arrives later on the per-request
   * response subject as an AsyncJobResult. Components await this and
   * get the final ok/error from the worker, not the optimistic accept.
   *
   * Injects the current `user.account` and the live `nc`; for the full
   * contract see {@link asyncJobRequest} in `api/_transport/asyncJob.js`.
   *
   * @param {string} subject
   * @param {unknown} [data={}]
   * @param {Object} [opts]  Forwarded to the helper (`treatAsSuccess`,
   *   `requestId`, `syncTimeout`, `asyncTimeout`).
   * @returns {Promise<{requestId: string, sync: unknown, async: unknown}>}
   * @throws Tagged Error with `.kind` from ASYNC_JOB_ERROR_KINDS on every
   *   failure path; use `formatAsyncJobError` for user-facing text.
   */
  const requestWithAsyncResult = useCallback(async (subject, data = {}, opts = {}) => {
    if (!ncRef.current) throw new Error('Not connected')
    const account = user?.account
    if (!account) throw new Error('Not authenticated')
    return asyncJobRequest(ncRef.current, account, subject, data, {
      debugLevel: debugLevelRef.current,
      debugPayload: debugPayloadRef.current,
      ...opts,
    })
  }, [user])

  /**
   * Fire-and-forget JSON publish. Use for events the server consumes
   * via QueueSubscribe (no reply expected); for request/reply use
   * `request` or `requestWithAsyncResult`.
   *
   * @param {string} subject
   * @param {unknown} [data={}]
   * @throws if not connected.
   */
  const publish = useCallback((subject, data = {}) => {
    if (!ncRef.current) throw new Error('Not connected')
    const payload = sc.encode(JSON.stringify(data))
    const h = buildHeaders()
    ncRef.current.publish(subject, payload, h ? { headers: h } : undefined)
  }, [buildHeaders])

  /**
   * Subscribe to a subject pattern and dispatch parsed JSON messages
   * to `callback`. Malformed JSON is silently skipped (server
   * canonical events are always JSON).
   *
   * @param {string} subject
   * @param {(data: unknown) => void} callback
   * @returns {{unsubscribe: () => void}} The underlying NATS
   *   subscription. Callers MUST call `.unsubscribe()` on unmount /
   *   cleanup to avoid leaking the iterator and the server-side sid.
   * @throws if not connected.
   */
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

  /**
   * Drain the NATS connection (flushes pending publishes, then closes)
   * and reset `user`/`connected`. Idempotent: calling on a disconnected
   * provider is a no-op.
   */
  const disconnect = useCallback(async () => {
    // Invalidate any in-flight connect and the live link's closed() callback so
    // a late close can't resurrect error/connected state after we tore down.
    connectGenRef.current += 1
    stop()
    if (ncRef.current) {
      await ncRef.current.drain()
      ncRef.current = null
    }
    setConnected(false)
    setUser(null)
  }, [stop])

  // Memoise so consumers that only read stable callbacks don't re-render
  // on every provider render. The value identity flips only when one of
  // the listed primitives/refs flips.
  const value = useMemo(
    () => ({
      connected, user, error,
      connect: connectToNats, request, requestWithAsyncResult, publish, subscribe, disconnect,
    }),
    [connected, user, error, connectToNats, request, requestWithAsyncResult, publish, subscribe, disconnect]
  )

  return <NatsContext.Provider value={value}>{children}</NatsContext.Provider>
}

export function useNats() {
  const ctx = useContext(NatsContext)
  if (!ctx) throw new Error('useNats must be used within NatsProvider')
  return ctx
}
