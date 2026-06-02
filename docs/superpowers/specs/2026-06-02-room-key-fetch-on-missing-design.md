# Room key fetch on missing — design

## Problem

When `chat-frontend` receives a channel message carrying an `encryptedMessage`
envelope (`{version, nonce, ciphertext}`), it can only decrypt if it already
holds the room key for that `(roomId, version)`. Keys reach the frontend today
only via live `RoomKeyEvent`s on `chat.user.{account}.event.room.key`, which
fire on room create, add-member, and key-rotation — never on demand. If a
client reconnects after missing the original event, or receives a message
encrypted with a version it doesn't yet hold, the frontend has no way to ask
for the key. The reducer falls back to a `[encrypted message]` placeholder
(`src/context/RoomEventsContext/reducer.js:309`) and the message stays
undecryptable for the session.

The docs flag a future bulk bootstrap via `subscription.get*`
(`docs/client-api.md:2036`), but it isn't implemented and covers a different
use case (all rooms at connect time, not the per-message gap addressed here).

## Goal

Let the frontend fetch a single `(roomId, version)` key on demand when a
received message can't be decrypted, decrypt the message, and continue
rendering normally. Out of scope: history-load decryption, DM keys (DMs are
plaintext today), bulk reconnect bootstrap.

## Approach

Add a new client-callable NATS request/reply RPC on `room-service` that
returns the room key for a given `(roomId, version?)` to a requesting member.
Wire `chat-frontend` to call it when `RoomKeysContext.decrypt` returns `null`,
dispatch the result through the same `KEY_RECEIVED` reducer path that live
events use, and retry decrypt once. Persistent failure falls through to the
existing placeholder UI.

The alternatives considered:

- **Repurpose `chat.server.request.room.{siteID}.key.ensure` as a client RPC**
  that publishes a `RoomKeyEvent` via `roomkeysender` as a side effect.
  Rejected: the subject sits outside the per-user auth-callout namespace, the
  reply doesn't carry key bytes, and the implicit "wait for the event" loop
  is racy and hard to unit-test.

- **Wait for `subscription.get*` bulk bootstrap.** Rejected: bigger scope,
  different shape (all rooms at once vs. one missing key), and doesn't help
  mid-session gaps after the bootstrap completes.

## Architecture

### `pkg/subject`

```
RoomKeyGet(account, roomID, siteID) string
RoomKeyGetWildcard(siteID) string  // "chat.user.*.request.room.*.<siteID>.key.get"
```

Subject `chat.user.{account}.request.room.{roomID}.{siteID}.key.get` mirrors
the existing member-list / role-update RPC pattern and parses via the existing
`ParseUserRoomSubject` helper. The wildcard is what `room-service` subscribes
to under its service queue group.

### `pkg/model`

```go
type RoomKeyGetRequest struct {
    Version *int `json:"version,omitempty"`  // nil → current
}

type RoomKeyGetResponse struct {
    RoomID     string `json:"roomId"`
    Version    int    `json:"version"`
    PrivateKey []byte `json:"privateKey"`  // base64-encoded over the wire
}
```

The response intentionally mirrors `RoomKeyEvent` minus `Timestamp` so the
frontend can feed it through the same reducer path without reshaping.

### `room-service`

- `store.go`: extend `RoomKeyStore` with
  `GetByVersion(ctx, roomID string, version int) (*roomkeystore.VersionedKeyPair, error)`.
  The valkey adapter already implements `GetByVersion`; this just lifts it
  into the room-service consumer interface.

- `handler.go`:
  - Register `RoomKeyGetWildcard(h.siteID)` alongside the other QueueSubscribe
    calls in `RegisterHandlers`.
  - `natsGetRoomKey(m)` is the entry point; delegates to
    `handleGetRoomKey(ctx, subj, data) ([]byte, error)`.
  - `handleGetRoomKey`:
    1. `subject.ParseUserRoomSubject(subj)` → `(requesterAccount, roomID)`.
    2. `store.GetSubscription(ctx, requesterAccount, roomID)` →
       `errNotRoomMember` on `ErrSubscriptionNotFound`.
    3. Parse `RoomKeyGetRequest` from body (empty body OK → current version).
    4. If `req.Version == nil`: `keyStore.Get(ctx, roomID)`. Else
       `keyStore.GetByVersion(ctx, roomID, *req.Version)`.
    5. `(nil, nil)` from the store → return a new `errRoomKeyAbsent` (lifted
       from the existing `room-worker` constant or reintroduced locally) so
       `sanitizeError` produces a `room_key_absent`-coded user error.
    6. On success: marshal `RoomKeyGetResponse{RoomID, Version, PrivateKey}`.

The handler is identical in shape to `handleListMembers` and uses the same
sanitization boundary.

### `chat-frontend`

- `src/api/_transport/subjects.ts`: add
  `roomKeyGetSubject(account, roomId, siteId)` mirroring the existing
  `memberListSubject` shape.

- `src/api/requestRoomKey/index.ts`: new op.
  ```ts
  export type RequestRoomKeyArgs = { roomId: string; siteId: string; version?: number }
  export type RequestRoomKeyResponse = { roomId: string; version: number; privateKey: string }

  export function requestRoomKey(nats: Nats, args: RequestRoomKeyArgs): Promise<RequestRoomKeyResponse>
  ```
  Calls `nats.request<RequestRoomKeyResponse>(subject, payload)` — fits the
  "Sync RPC, immediate reply" row of the api-layer table in `CLAUDE.md`.

- `src/api/index.ts`: re-export `requestRoomKey` + types.

- `src/context/RoomKeysContext/RoomKeysContext.tsx`: extend the context with
  ```ts
  ensureKey(roomId: string, version: number, siteId: string): Promise<boolean>
  ```
  - Maintains `pendingRequestsRef: Map<string, Promise<boolean>>` keyed by
    `${roomId}|${version}`. Concurrent callers share one in-flight request.
  - On resolve: decode `privateKey` via `b64decode`, dispatch
    `{ type: 'KEY_RECEIVED', roomId, version, privateKey }`. The reducer's
    existing `bytesEqual` no-ops on identical bytes (live-event race-safe).
  - On reject: record `(roomId, version)` in `failedKeysRef: Map<string, number>`
    with the failure timestamp. Subsequent `ensureKey` calls for the same key
    within `KEY_RETRY_BACKOFF_MS = 60_000` short-circuit to `false`. After the
    window expires, the next caller can retry.
  - The pending Promise is removed from the map on settle so a successful
    retry can later occur even after a recorded failure expires.

- `src/context/RoomEventsContext/RoomEventsContext.tsx`: pull `ensureKey` from
  `useRoomKeys()` and forward it to `useRoomSubscriptions` alongside `decrypt`.

- `src/context/RoomEventsContext/useRoomSubscriptions.js`:
  - Accept `ensureKey = async () => false` with a matching ref
    (`ensureKeyRef`).
  - In `decryptAndDispatch`, when the first `decryptRef.current(...)` returns
    `null` and `evt.roomId` + `enc.version` are present, call
    `ensureKeyRef.current(evt.roomId, enc.version, evt.siteId ?? userSiteId)`.
    On `true`, retry `decryptRef.current(...)` once.
  - The per-room `enqueueByRoom` chain already serializes work, so the extra
    await doesn't reorder messages within a room.

- `docs/client-api.md` §5: new "Requesting a missing key" subsection
  documenting subject, request/response payloads, error codes
  (`room_key_absent`, `not_room_member`), and explicit "complements, does not
  replace, live `RoomKeyEvent`s".

## Data flow

1. Message arrives carrying `encryptedMessage { version, nonce, ciphertext }`.
2. `useRoomSubscriptions.decryptAndDispatch` calls
   `decryptRef.current({ roomId, version, nonceB64, ciphertextB64 })`.
3. `RoomKeysContext.decrypt` reads `state.byRoom[roomId][version]`. Hit →
   AES-GCM decrypt and return plaintext (unchanged today). Miss → return
   `null`.
4. **New:** on `null`, `decryptAndDispatch` calls
   `ensureKeyRef.current(roomId, version, siteId)`.
5. `ensureKey` either awaits an existing in-flight Promise or starts a new
   `requestRoomKey` RPC. On success: dispatch `KEY_RECEIVED`, resolve `true`.
   On failure: record the failure with backoff, resolve `false`.
6. On `true`, `decryptAndDispatch` retries `decryptRef.current` once. On
   success: `MESSAGE_RECEIVED` with the decrypted body. On `false` or still
   null: fall through to the existing `[encrypted message]` placeholder
   branch (`reducer.js:309`).
7. A live `RoomKeyEvent` arriving concurrently still wins via the same
   reducer path; `bytesEqual` no-ops on identical bytes so the cached AES
   `CryptoKey` is preserved.

## Error handling

| Failure | Server returns | Client behavior |
| --- | --- | --- |
| Subject doesn't parse | `errInvalidSubject` | n/a (auth-callout subjects always parse) |
| Requester not a member | `errNotRoomMember` (code `not_room_member`) | `ensureKey` resolves `false`; backoff recorded; placeholder rendered |
| Key absent (rotated past grace / never existed) | `errRoomKeyAbsent` (code `room_key_absent`) | same as above |
| Store error | sanitized internal error | same as above |
| NATS request timeout | `nats.request` rejects | same as above (transient — backoff lets a later message retry after 60s) |

`ensureKey` never throws into the dispatch callback. The worst case is the
existing `[encrypted message]` UI. No infinite retry: the 60s per-key backoff
covers transient hiccups without stampeding when the key is permanently gone.

Authorization is enforced twice: the auth-callout subject grant already
restricts a client to `chat.user.{theirAccount}.>`, and the server re-checks
membership in case the user was removed between subscribe and publish.

## Testing

### Backend (TDD per `CLAUDE.md` Section 4)

- `pkg/subject/subject_test.go`: builder and wildcard string entries for
  `RoomKeyGet` / `RoomKeyGetWildcard`.
- `pkg/model/model_test.go`: `RoomKeyGetRequest` and `RoomKeyGetResponse`
  round-trip via the generic `roundTrip` helper, including `Version: nil`.
- `room-service/handler_test.go` (table-driven, mocked `store` + `keyStore`):
  - Happy path versionless → current key returned.
  - Happy path explicit version → that version returned via `GetByVersion`.
  - Non-member → `errNotRoomMember`.
  - `keyStore.Get` returns `(nil, nil)` → `errRoomKeyAbsent`.
  - `keyStore.GetByVersion` returns `(nil, nil)` → `errRoomKeyAbsent`.
  - Store error → sanitized internal error.
  - Malformed JSON body → request decode error.
- `room-service/integration_test.go` (testcontainers, mongo + valkey):
  - Write key via valkey store, subscribe `RoomKeyGetWildcard`, requester
    with a subscription gets the key bytes back and decodes them.
  - Non-member request rejected with `not_room_member`.

### Frontend (vitest + `@testing-library/react`)

- `api/requestRoomKey/index.test.ts`: subject is built correctly and
  `nats.request` is called with the exact `(subject, payload)` pair.
- `context/RoomKeysContext/RoomKeysContext.test.jsx`:
  - `ensureKey` calls `requestRoomKey` once for concurrent same-key callers
    (in-flight dedupe).
  - On success, a subsequent `decrypt` for the same `(roomId, version)`
    returns plaintext.
  - On error, `ensureKey` returns `false` and the next call within 60s
    short-circuits without hitting `requestRoomKey` again (fake timers).
  - After 60s the next call retries.
- `context/RoomEventsContext/useRoomSubscriptions` (extend existing tests):
  - Missing-key path triggers `ensureKey`, then `MESSAGE_RECEIVED` carries
    the decrypted body when the retry succeeds.
  - When `ensureKey` returns `false`, the existing placeholder dispatch path
    fires, no retry storm.
  - When `decrypt` succeeds the first time, `ensureKey` is not called.

### Smoke / docs

- `docs/client-api.md` example block exercised by an integration test (the
  same one that round-trips the subject) so the documented payload doesn't
  drift from `model.RoomKeyGetRequest` / `Response`.

## Out of scope

- History-load decryption (`fetchMessageHistory` path) — same gap, different
  call site; trivial follow-up after this lands.
- The `subscription.get*` bulk bootstrap from `docs/client-api.md:2036`.
- DM/botDM key fetches — DM rooms broadcast plaintext today, so no
  `encryptedMessage` ever needs a missing key.
