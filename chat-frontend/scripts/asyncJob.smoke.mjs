// Wire-level smoke test for api/_transport/asyncJob.ts against a real
// NATS server. Not part of the standing test suite — invoke ad-hoc when
// verifying that the helper survives contact with the actual nats.ws
// WebSocket transport, header serialization, and response-subject
// delivery.
//
// Prereqs: a NATS server with WebSocket on ws://localhost:8443.
//   /root/go/bin/nats-server -c /tmp/nats.conf
//
// Usage: npm run smoke:asyncjob (uses Node --experimental-strip-types
// to load the .ts source directly).

import { connect, StringCodec } from 'nats.ws'
import { requestWithAsyncResult, ASYNC_JOB_ERROR_KINDS } from '../src/api/_transport/asyncJob.ts'
import { isDMExistsReply } from '../src/lib/constants.js'

const WS_URL = process.env.NATS_WS_URL || 'ws://localhost:8443'
const sc = StringCodec()

let pass = 0
let fail = 0

function check(label, ok, detail = '') {
  const tag = ok ? 'PASS' : 'FAIL'
  console.log(`  [${tag}] ${label}${detail ? ' — ' + detail : ''}`)
  if (ok) pass++
  else fail++
}

async function withResponder(nc, subject, handler) {
  const sub = nc.subscribe(subject)
  ;(async () => {
    for await (const msg of sub) {
      try {
        await handler(msg)
      } catch (err) {
        console.error('responder error:', err)
      }
    }
  })()
  return sub
}

async function main() {
  console.log(`Connecting to ${WS_URL}…`)
  const nc = await connect({ servers: WS_URL })

  // ── case 1: full happy path ────────────────────────────────────────────────
  console.log('\n[1] happy-path: sync accept + async ok')
  {
    let observedHeader = null
    let observedSubject = null
    const responder = await withResponder(
      nc,
      'chat.user.*.request.room.*.create',
      async (msg) => {
        observedSubject = msg.subject
        observedHeader = msg.headers?.get('X-Request-ID') ?? null
        // sync reply: accepted
        msg.respond(sc.encode(JSON.stringify({
          status: 'accepted',
          roomId: 'r-new',
          roomType: 'channel',
        })))
        // async reply: publish to response subject — derive account from the
        // request subject (chat.user.{a}.request.room.{site}.create).
        const acct = msg.subject.split('.')[2]
        const responseSubject = `chat.user.${acct}.response.${observedHeader}`
        // Tiny gap so the client's async-result wait actually waits.
        setTimeout(() => {
          nc.publish(responseSubject, sc.encode(JSON.stringify({
            requestId: observedHeader,
            operation: 'room.create',
            status: 'ok',
            roomId: 'r-new',
            timestamp: Date.now(),
          })))
        }, 50)
      }
    )

    const result = await requestWithAsyncResult(
      nc,
      'alice',
      'chat.user.alice.request.room.site-A.create',
      { name: 'frontend', users: [], orgs: [], channels: [] }
    )
    check('request reached responder', !!observedSubject, observedSubject)
    check('X-Request-ID header survived the wire', !!observedHeader && observedHeader.length >= 30,
      observedHeader)
    check('sync reply parsed', result.sync.status === 'accepted',
      `roomId=${result.sync.roomId} roomType=${result.sync.roomType}`)
    check('async result delivered', result.async?.status === 'ok',
      `operation=${result.async?.operation} roomId=${result.async?.roomId}`)
    check('requestId on AsyncJobResult matches header',
      result.async?.requestId === observedHeader,
      `${result.async?.requestId} vs ${observedHeader}`)

    await responder.unsubscribe()
  }

  // ── case 2: DM-exists treatAsSuccess branch ────────────────────────────────
  console.log('\n[2] DM-exists: {error, roomId} reply is treated as success')
  {
    const responder = await withResponder(
      nc,
      'chat.user.*.request.room.*.create',
      async (msg) => {
        msg.respond(sc.encode(JSON.stringify({
          error: 'dm already exists',
          roomId: 'r-existing',
        })))
      }
    )

    let err
    let result
    try {
      result = await requestWithAsyncResult(
        nc,
        'alice',
        'chat.user.alice.request.room.site-A.create',
        { name: '', users: ['bob'], orgs: [], channels: [] },
        { treatAsSuccess: isDMExistsReply }
      )
    } catch (e) {
      err = e
    }
    check('did not throw', !err, err?.message)
    check('sync.error preserved', result?.sync?.error === 'dm already exists')
    check('sync.roomId preserved', result?.sync?.roomId === 'r-existing')
    check('async is null (no follow-up)', result?.async === null)
    await responder.unsubscribe()
  }

  // ── case 3: async error ────────────────────────────────────────────────────
  console.log('\n[3] async error: AsyncJobResult.status=error bubbles up')
  {
    const responder = await withResponder(
      nc,
      'chat.user.*.request.room.*.*.member.add',
      async (msg) => {
        const reqId = msg.headers?.get('X-Request-ID')
        msg.respond(sc.encode(JSON.stringify({ status: 'accepted' })))
        const acct = msg.subject.split('.')[2]
        setTimeout(() => {
          nc.publish(`chat.user.${acct}.response.${reqId}`, sc.encode(JSON.stringify({
            requestId: reqId,
            operation: 'room.member.add',
            status: 'error',
            error: 'only owners can add members',
            timestamp: Date.now(),
          })))
        }, 25)
      }
    )

    let err
    try {
      await requestWithAsyncResult(
        nc,
        'alice',
        'chat.user.alice.request.room.r1.site-A.member.add',
        { roomId: 'r1', users: ['bob'], orgs: [], channels: [], history: { mode: 'all' } }
      )
    } catch (e) {
      err = e
    }
    check('threw with server error message', /only owners/.test(err?.message ?? ''), err?.message)
    check('err.kind = async-error', err?.kind === ASYNC_JOB_ERROR_KINDS.AsyncError, err?.kind)
    await responder.unsubscribe()
  }

  // ── case 4: async timeout (no responder publishes AsyncJobResult) ──────────
  console.log('\n[4] async timeout: helper rejects + cleans up subscription')
  {
    const responder = await withResponder(
      nc,
      'chat.user.*.request.room.*.*.member.remove',
      async (msg) => {
        msg.respond(sc.encode(JSON.stringify({ status: 'accepted' })))
        // deliberately do NOT publish an AsyncJobResult
      }
    )

    const t0 = Date.now()
    let err
    try {
      await requestWithAsyncResult(
        nc,
        'alice',
        'chat.user.alice.request.room.r1.site-A.member.remove',
        { roomId: 'r1', account: 'bob' },
        { asyncTimeout: 400 }
      )
    } catch (e) {
      err = e
    }
    const dt = Date.now() - t0
    check('err.kind = async-timeout', err?.kind === ASYNC_JOB_ERROR_KINDS.AsyncTimeout, err?.kind)
    check('rejection happened ~asyncTimeout (≥350ms, ≤2000ms)', dt >= 350 && dt <= 2000, `${dt}ms`)
    await responder.unsubscribe()
  }

  // ── case 5: subscribe-before-publish ordering ──────────────────────────────
  console.log('\n[5] subscribe-before-publish: AsyncJobResult published immediately is still received')
  {
    // Race condition probe: responder publishes the async result the same tick
    // it sends the sync reply. If the helper subscribes AFTER the request, the
    // async result is lost. If it subscribes BEFORE (as designed), it lands.
    const responder = await withResponder(
      nc,
      'chat.user.*.request.room.*.create',
      async (msg) => {
        const reqId = msg.headers?.get('X-Request-ID')
        const acct = msg.subject.split('.')[2]
        // Publish async result FIRST, then sync reply. The helper must have
        // already subscribed to the response subject for this to work.
        nc.publish(`chat.user.${acct}.response.${reqId}`, sc.encode(JSON.stringify({
          requestId: reqId,
          operation: 'room.create',
          status: 'ok',
          roomId: 'r-fast',
          timestamp: Date.now(),
        })))
        msg.respond(sc.encode(JSON.stringify({
          status: 'accepted',
          roomId: 'r-fast',
          roomType: 'channel',
        })))
      }
    )

    let result, err
    try {
      result = await requestWithAsyncResult(
        nc,
        'alice',
        'chat.user.alice.request.room.site-A.create',
        { name: 'fast', users: [], orgs: [], channels: [] },
        { asyncTimeout: 1000 }
      )
    } catch (e) { err = e }
    check('completed without timeout (sub was open in time)', !err, err?.message)
    check('async.status=ok', result?.async?.status === 'ok')
    await responder.unsubscribe()
  }

  await nc.drain()

  console.log(`\n=== ${pass} passed, ${fail} failed ===`)
  process.exit(fail === 0 ? 0 : 1)
}

main().catch((err) => {
  console.error('FATAL:', err)
  process.exit(2)
})
