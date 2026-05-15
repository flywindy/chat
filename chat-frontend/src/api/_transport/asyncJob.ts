// requestWithAsyncResult bridges the room-service two-phase reply contract:
//   1. sync NATS reply  → typically {status:"accepted", …} (or {error})
//   2. async result     → AsyncJobResult on chat.user.{account}.response.{requestID}
//
// The response subscription is opened BEFORE the request is published so a
// fast worker can't beat us to the punch. The X-Request-ID header on the
// request is what tells room-worker which response subject to publish on —
// without it the async result is never emitted.

import { StringCodec, headers as natsHeaders } from 'nats.ws'
import type { NatsConnection, Subscription as NatsSubscription } from 'nats.ws'
import { v7 as uuidv7 } from 'uuid'
import { userResponse } from './subjects'
import type { AsyncJobOptions, AsyncJobResult } from '../types'

const sc = StringCodec()

const DEFAULT_SYNC_TIMEOUT = 5000
const DEFAULT_ASYNC_TIMEOUT = 30000

/** Discriminated string kinds attached to every error thrown from here. */
export const ASYNC_JOB_ERROR_KINDS = {
  SyncError: 'sync-error',
  AsyncError: 'async-error',
  AsyncTimeout: 'async-timeout',
  SubscriptionClosed: 'subscription-closed',
} as const

export type AsyncJobErrorKind =
  (typeof ASYNC_JOB_ERROR_KINDS)[keyof typeof ASYNC_JOB_ERROR_KINDS]

/**
 * Error class thrown by `requestWithAsyncResult`. Use `instanceof` to
 * narrow without string-matching the message.
 *
 * Why a class (not just an interface): callers can do
 *   `if (err instanceof AsyncJobError) …`
 * which is the idiomatic way to discriminate caught `unknown` in TS.
 */
export class AsyncJobError extends Error {
  readonly kind: AsyncJobErrorKind
  constructor(message: string, kind: AsyncJobErrorKind, cause?: unknown) {
    super(message)
    this.name = 'AsyncJobError'
    this.kind = kind
    if (cause !== undefined) this.cause = cause
  }
}

/**
 * User-facing message for an error thrown by `requestWithAsyncResult`.
 *
 * Server-side errors (`SyncError`, `AsyncError`) already carry a sanitised,
 * user-safe message and are returned as-is. Wire-level failures
 * (`AsyncTimeout`, `SubscriptionClosed`) get a friendlier hint that says what
 * happened and what the user can do about it — the raw "async result timeout"
 * isn't actionable.
 */
export function formatAsyncJobError(err: unknown): string {
  if (!err) return ''
  // Prefer `instanceof AsyncJobError`, but also duck-type on `.kind` so
  // any caller that hand-rolls an Error with a `kind` field still gets
  // the friendly hints. Some test helpers do exactly that.
  const kind =
    err instanceof AsyncJobError
      ? err.kind
      : (err as { kind?: AsyncJobErrorKind })?.kind
  switch (kind) {
    case ASYNC_JOB_ERROR_KINDS.AsyncTimeout:
      return "The server didn't respond in time. The action may still complete — refresh to check."
    case ASYNC_JOB_ERROR_KINDS.SubscriptionClosed:
      return 'Connection interrupted before the server confirmed. Refresh to check the result.'
    default:
      return err instanceof Error ? err.message : String(err)
  }
}

// Internal envelope passed from the inbox loop to the awaiter. Discriminated
// so the awaiter can pattern-match on `kind` without optional chaining.
type Envelope =
  | { kind: 'data'; data: unknown }
  | { kind: 'closed' }
  | { kind: 'error'; error: unknown }
  | { kind: 'timeout' }

/** Common shape of any sync reply we treat specially — `error` triggers
 *  the failure branch, `status` is the typical 'accepted'/'error' marker. */
interface SyncReplyEnvelope {
  error?: string
  status?: string
}

/** Common shape of any async-job result envelope we receive on the
 *  response subject. Both `status` and `error` may be set; status === 'error'
 *  takes the failure path. */
interface AsyncReplyEnvelope {
  status?: string
  error?: string
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
 * @throws {AsyncJobError} On any failure, with `.kind` set to one of
 *   the `AsyncJobErrorKind` values. Pass to `formatAsyncJobError(err)`
 *   for user-facing text.
 */
export async function requestWithAsyncResult<S = unknown, A = unknown>(
  nc: NatsConnection,
  account: string,
  subject: string,
  payload: unknown,
  opts: AsyncJobOptions = {},
): Promise<AsyncJobResult<S, A>> {
  const {
    requestId = uuidv7(),
    syncTimeout = DEFAULT_SYNC_TIMEOUT,
    asyncTimeout = DEFAULT_ASYNC_TIMEOUT,
    treatAsSuccess,
  } = opts

  const sub: NatsSubscription = nc.subscribe(userResponse(account, requestId), { max: 1 })

  // Register before request resolves so a result that arrives during the
  // sync window is buffered, not dropped. Tagged-envelope resolves never
  // reject so late cleanup signals can't surface as unhandled rejections.
  let resolveAsync!: (env: Envelope) => void
  const asyncPromise = new Promise<Envelope>((res) => { resolveAsync = res })
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

  let sync: S
  try {
    const h = natsHeaders()
    h.set('X-Request-ID', requestId)
    const resp = await nc.request(subject, sc.encode(JSON.stringify(payload)), {
      timeout: syncTimeout,
      headers: h,
    })
    sync = JSON.parse(sc.decode(resp.data)) as S
  } catch (err) {
    cleanupSub()
    const msg = err instanceof Error ? err.message : String(err)
    throw new AsyncJobError(msg, ASYNC_JOB_ERROR_KINDS.SyncError, err)
  }

  // `sync` is generic, but we always inspect the same envelope fields.
  const syncEnv = sync as unknown as SyncReplyEnvelope
  if (syncEnv?.error) {
    // DM-exists and similar "200 with error+roomId" replies are success cases
    // for the caller. The caller opts into this with treatAsSuccess(reply).
    if (treatAsSuccess && treatAsSuccess(sync)) {
      cleanupSub()
      return { requestId, sync, async: null }
    }
    cleanupSub()
    throw new AsyncJobError(syncEnv.error, ASYNC_JOB_ERROR_KINDS.SyncError)
  }

  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    const timeoutPromise = new Promise<Envelope>((resolve) => {
      timer = setTimeout(() => resolve({ kind: 'timeout' }), asyncTimeout)
    })
    const envelope = await Promise.race([asyncPromise, timeoutPromise])
    if (timer) clearTimeout(timer)
    if (envelope.kind === 'timeout') {
      throw new AsyncJobError('async result timeout', ASYNC_JOB_ERROR_KINDS.AsyncTimeout)
    }
    if (envelope.kind === 'error') {
      const cause = envelope.error
      const msg = cause instanceof Error ? cause.message : 'subscription error'
      throw new AsyncJobError(msg, ASYNC_JOB_ERROR_KINDS.SubscriptionClosed, cause)
    }
    if (envelope.kind === 'closed') {
      throw new AsyncJobError(
        'subscription closed before result arrived',
        ASYNC_JOB_ERROR_KINDS.SubscriptionClosed,
      )
    }
    const asyncEnv = envelope.data as AsyncReplyEnvelope
    if (asyncEnv.status === 'error') {
      throw new AsyncJobError(
        asyncEnv.error || 'operation failed',
        ASYNC_JOB_ERROR_KINDS.AsyncError,
      )
    }
    cleanupSub()
    return { requestId, sync, async: envelope.data as A }
  } catch (err) {
    if (timer) clearTimeout(timer)
    cleanupSub()
    throw err
  }
}
