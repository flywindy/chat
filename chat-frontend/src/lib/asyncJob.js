// requestWithAsyncResult bridges the room-service two-phase reply contract:
//   1. sync NATS reply  → typically {status:"accepted", …} (or {error})
//   2. async result     → AsyncJobResult on chat.user.{account}.response.{requestID}
//
// The response subscription is opened BEFORE the request is published so a
// fast worker can't beat us to the punch. The X-Request-ID header on the
// request is what tells room-worker which response subject to publish on —
// without it the async result is never emitted.

import { StringCodec, headers as natsHeaders } from 'nats.ws'
import { v7 as uuidv7 } from 'uuid'
import { userResponse } from './subjects'

const sc = StringCodec()

const DEFAULT_SYNC_TIMEOUT = 5000
const DEFAULT_ASYNC_TIMEOUT = 30000

// Error kinds attached to every Error thrown from this helper so callers
// can distinguish wire-level failures from server-reported ones without
// string-matching the message.
export const ASYNC_JOB_ERROR_KINDS = Object.freeze({
  SyncError: 'sync-error',
  AsyncError: 'async-error',
  AsyncTimeout: 'async-timeout',
  SubscriptionClosed: 'subscription-closed',
})

function taggedError(message, kind, cause) {
  const err = new Error(message)
  err.kind = kind
  if (cause) err.cause = cause
  return err
}

/**
 * User-facing message for an error thrown by `requestWithAsyncResult`.
 *
 * Server-side errors (`SyncError`, `AsyncError`) already carry a sanitised,
 * user-safe message and are returned as-is. Wire-level failures
 * (`AsyncTimeout`, `SubscriptionClosed`) get a friendlier hint that says what
 * happened and what the user can do about it — the raw "async result timeout"
 * isn't actionable.
 *
 * @param {Error & {kind?: string}} err
 * @returns {string}
 */
export function formatAsyncJobError(err) {
  if (!err) return ''
  switch (err.kind) {
    case ASYNC_JOB_ERROR_KINDS.AsyncTimeout:
      return "The server didn't respond in time. The action may still complete — refresh to check."
    case ASYNC_JOB_ERROR_KINDS.SubscriptionClosed:
      return 'Connection interrupted before the server confirmed. Refresh to check the result.'
    default:
      return err.message ?? ''
  }
}

/**
 * Issues a NATS request whose handler responds in two phases: a sync reply
 * (typically `{status:"accepted"}` or `{error}`) followed by an
 * `AsyncJobResult` published to `chat.user.{account}.response.{requestID}`
 * once the underlying worker finishes.
 *
 * Subscribes to the response subject BEFORE publishing so a fast worker
 * can't beat the client to the punch. Sets the `X-Request-ID` NATS header
 * so the worker knows where to publish the async result.
 *
 * @param {object} nc                 nats.ws connection
 * @param {string} account            requester's account (used to build the response subject)
 * @param {string} subject            request subject (e.g. result of `roomCreate(...)`)
 * @param {object} payload            JSON-serialisable request body
 * @param {object} [opts]
 * @param {string} [opts.requestId]   defaults to a fresh UUIDv7
 * @param {number} [opts.syncTimeout=5000]    ms to wait for sync reply
 * @param {number} [opts.asyncTimeout=30000]  ms to wait for AsyncJobResult after sync accept
 * @param {(reply: object) => boolean} [opts.treatAsSuccess]
 *   Predicate over the sync reply. Returning true short-circuits: caller gets
 *   `{requestId, sync, async: null}` instead of throwing on `sync.error`.
 *   Use this for "200-with-error-but-actually-success" replies (e.g. DM-exists
 *   dedup — see `isDMExistsReply`).
 *
 * @returns {Promise<{requestId: string, sync: object, async: object|null}>}
 * @throws {Error & {kind: string}} On any failure, with `err.kind` set to one
 *   of {@link ASYNC_JOB_ERROR_KINDS}. Pass to `formatAsyncJobError(err)` for
 *   user-facing text.
 */
export async function requestWithAsyncResult(nc, account, subject, payload, opts = {}) {
  const {
    requestId = uuidv7(),
    syncTimeout = DEFAULT_SYNC_TIMEOUT,
    asyncTimeout = DEFAULT_ASYNC_TIMEOUT,
    treatAsSuccess,
  } = opts

  const sub = nc.subscribe(userResponse(account, requestId), { max: 1 })

  // Register before request resolves so a result that arrives during the
  // sync window is buffered, not dropped. Tagged-envelope resolves never
  // reject so late cleanup signals can't surface as unhandled rejections.
  let resolveAsync
  const asyncPromise = new Promise((res) => { resolveAsync = res })
  ;(async () => {
    try {
      for await (const msg of sub) {
        resolveAsync({ kind: 'data', data: JSON.parse(sc.decode(msg.data)) })
        return
      }
      resolveAsync({ kind: 'closed' })
    } catch (err) {
      resolveAsync({ kind: 'error', error: err })
    }
  })()

  const cleanupSub = () => {
    try { sub.unsubscribe() } catch { /* already closed */ }
  }

  let sync
  try {
    const h = natsHeaders()
    h.set('X-Request-ID', requestId)
    const resp = await nc.request(subject, sc.encode(JSON.stringify(payload)), {
      timeout: syncTimeout,
      headers: h,
    })
    sync = JSON.parse(sc.decode(resp.data))
  } catch (err) {
    cleanupSub()
    throw taggedError(err.message, ASYNC_JOB_ERROR_KINDS.SyncError, err)
  }

  if (sync.error) {
    // DM-exists and similar "200 with error+roomId" replies are success cases
    // for the caller. The caller opts into this with treatAsSuccess(reply).
    if (treatAsSuccess && treatAsSuccess(sync)) {
      cleanupSub()
      return { requestId, sync, async: null }
    }
    cleanupSub()
    throw taggedError(sync.error, ASYNC_JOB_ERROR_KINDS.SyncError)
  }

  let timer
  try {
    const timeoutPromise = new Promise((resolve) => {
      timer = setTimeout(() => resolve({ kind: 'timeout' }), asyncTimeout)
    })
    const envelope = await Promise.race([asyncPromise, timeoutPromise])
    clearTimeout(timer)
    if (envelope.kind === 'timeout') {
      throw taggedError('async result timeout', ASYNC_JOB_ERROR_KINDS.AsyncTimeout)
    }
    if (envelope.kind === 'error') {
      throw taggedError(envelope.error?.message ?? 'subscription error',
        ASYNC_JOB_ERROR_KINDS.SubscriptionClosed, envelope.error)
    }
    if (envelope.kind === 'closed') {
      throw taggedError('subscription closed before result arrived',
        ASYNC_JOB_ERROR_KINDS.SubscriptionClosed)
    }
    const asyncResult = envelope.data
    if (asyncResult.status === 'error') {
      throw taggedError(asyncResult.error || 'operation failed',
        ASYNC_JOB_ERROR_KINDS.AsyncError)
    }
    cleanupSub()
    return { requestId, sync, async: asyncResult }
  } catch (err) {
    clearTimeout(timer)
    cleanupSub()
    throw err
  }
}
