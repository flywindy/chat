# ECDH Ephemeral Key Performance — Analysis and Design Spec

**Date:** 2026-05-20
**Status:** Draft — awaiting review
**Scope:** Replace the per-message ECIES-style encryption scheme in
`pkg/roomcrypto` with a versioned symmetric AES key derived once per room key
version via HKDF, eliminating ECDH and ephemeral-keygen from the hot path.
Includes both the `broadcast-worker` change and the first chat-frontend
decoder implementation. Drops the legacy scheme entirely; no dual-scheme
migration window.

---

## Overview

`pkg/roomcrypto.Encode` is called on every broadcast event for an encrypted
channel room (see `broadcast-worker/handler.go:210, 254`). It generates a
fresh P-256 ephemeral key pair, performs ECDH against the room public key,
HKDFs the shared secret into a 32-byte AES key, and AEAD-seals the message
under that key. The ephemeral public key is sent on the wire so clients
could repeat the ECDH on their side.

This spec replaces the scheme with a versioned symmetric AES key derived
directly from the room private key:
`aesKey_v = HKDF-SHA256(roomPriv_v, salt=nil, info="room-message-encryption-v2")`.
The wire format drops `ephemeralPublicKey` outright. Both server and
chat-frontend are updated in this branch. The Swift client lives outside this
repo and will need its own update; coordination of that work is out of scope
here.

---

## Today's implementation

Per call to `roomcrypto.Encode(content, roomPublicKey, version)`:

1. Parse the 65-byte uncompressed P-256 public key.
2. Generate a fresh ephemeral P-256 key pair (`ecdh.P256().GenerateKey`).
3. ECDH between the ephemeral private key and the room public key.
4. HKDF-SHA256 the shared secret with `info="room-message-encryption"` into a
   32-byte AES key.
5. AES-256-GCM seal with a random 96-bit nonce.
6. Return `{version, ephemeralPublicKey, nonce, ciphertext}`.

The room key pair lives in Valkey (`pkg/roomkeystore`). The server holds
both halves; only the public half is used at encrypt time. Clients receive
the private half via `room-key-sender` on the subject
`chat.user.{account}.event.room.key`.

Today **no client in this monorepo actually decrypts.** chat-frontend
renders an `[encrypted message]` placeholder
(`chat-frontend/src/context/RoomEventsContext/reducer.js:300-318`). The
Swift client (out-of-repo) does decode but is treated as a separate-track
update for this change.

---

## Performance measurement

Benchmark file: `pkg/roomcrypto/bench_test.go`. Run with
`go test -bench=. -benchmem -benchtime=2s -run=^$ ./pkg/roomcrypto/`.

Hardware used for these numbers: Intel Xeon @ 2.80 GHz, Linux amd64,
Go 1.25.10 (the dev container; production silicon is comparable or faster).

| Operation                          | ns/op   | allocs/op | Share of `Encode` |
| ---------------------------------- | ------- | --------- | ----------------- |
| Full `Encode` (current)            | 87,185  | 39        | 100%              |
| P-256 ECDH (scalar mult)           | 64,823  | 2         | 74%               |
| P-256 ephemeral keygen             | 16,905  | 7         | 19%               |
| Parse uncompressed P-256 pub       | 380     | 5         | 0.4%              |
| **Symmetric-only path (proposed)** | **200** | **1**     | **0.2%**          |
| X25519 keygen (reference)          | 54,115  | 5         | —                 |
| X25519 ECDH (reference)            | 53,847  | 1         | —                 |

### Implications at sustained throughput

| Rate       | Current `Encode` CPU | Proposed CPU | CPU saved        |
| ---------- | -------------------- | ------------ | ---------------- |
| 1K msg/s   | 87 ms/s (~8.7% core) | 0.2 ms/s     | ~99.8%           |
| 10K msg/s  | 870 ms/s (~87% core) | 2 ms/s       | ~99.8%           |
| 100K msg/s | 8.7 s/s (~8.7 cores) | 20 ms/s      | effectively all  |

Allocations drop from 39/op to 1/op; at 10K msg/s that's ~390K allocs/s of
GC pressure removed from the broadcast hot path.

### Note on X25519

X25519 is conventionally faster than P-256, but Go's standard library ships
amd64 assembly for P-256 (`p256_asm_amd64.s` using ADX/BMI2) and not for
X25519. On amd64 our benchmarks show P-256 ECDH (~65 µs) is **faster** than
X25519 ECDH (~54 µs in keygen, ~54 µs in ECDH). Curve switching is rejected
on benchmark evidence in this deployment. (On arm64 the picture would
invert; revisit if the deployment substrate changes.)

---

## Threat model and forward-secrecy analysis

This system is **not** end-to-end encrypted. The server holds the room
private key in Valkey (`pkg/roomkeystore/roomkeystore.go:18-19`). The
encryption scheme protects ciphertexts in two places only:

1. **In-flight broadcast events** carried over NATS (including federation via
   OUTBOX/INBOX).
2. **Nothing else.** `message-worker` writes messages to Cassandra in
   plaintext; `history-service` and `search-service` do not touch
   `roomcrypto`. Encrypted ciphertexts are transient — once a broadcast event
   is consumed and acknowledged it is gone from the system.

The realistic threats this encryption blunts:

- A NATS observer who can see traffic but does not have Valkey access cannot
  read message bodies.
- A federation peer that mishandles inbound traffic cannot read message
  bodies without the sending site's Valkey contents.

The realistic threats this encryption does **not** blunt:

- Anyone with Valkey access reads every room private key and can decrypt
  every ciphertext, past and present, because the ephemeral public key
  travels alongside on the wire.

### What about forward secrecy?

Forward secrecy means *past sessions remain confidential after long-term
key compromise*. Here, the long-term key is the room private key in Valkey.
Per-message ephemeral keys give **no** forward secrecy against Valkey
compromise: an attacker with `room_priv` runs ECDH against the
`ephemeralPublicKey` already on the wire, regenerates the per-message AES
key, and decrypts.

The only FS this system actually has is at the **key-rotation boundary**:
once a `VersionedKeyPair` ages out of Valkey's previous-key slot (per
`VALKEY_KEY_GRACE_PERIOD`), ciphertexts encrypted under the retired version
are no longer decryptable from current Valkey state. The new scheme
preserves this property unchanged — per-version forward secrecy is exactly
what the versioned symmetric key gives you.

### What the ephemeral key actually buys today

1. **Wire-level non-determinism.** Also provided by the random GCM nonce.
2. **An AES-GCM nonce-reuse safety net.** Since the AES key is per-message,
   a nonce collision across messages cannot break confidentiality. This is
   real defense-in-depth, but indirect: it hedges against a separate bug in
   the OS CSPRNG, which would catastrophically break the whole system.
3. **A compliance/audit talking point** that each message has its own key.

Items 1 and 3 are not security properties. Item 2 is the only real value
and is discussed under Risks below.

---

## Alternatives considered

### A. Status quo

Keep paying ~87 µs per message. Easiest to ship (zero work). Rejected
because the broadcast hot path keeps burning CPU on cryptographic
operations that provide no security benefit beyond what the proposed
scheme provides for free.

### B. Cache ephemeral key per (room, key-version), keep wire format

Server-only change: cache the ephemeral keypair the first time a key
version is used, then reuse it for every subsequent message in that
version. Wire format and client code unchanged. Captures ~99% of the perf
win with zero client coordination. Rejected because the user has accepted
the client coordination cost in exchange for the cleaner end state, and
because chat-frontend has no existing decoder to break — there's nothing
to coordinate with on this side of the wire.

### C. Switch curve to X25519

Conventional wisdom says X25519 beats P-256. False on amd64 Go — see the
X25519 note above. Rejected on benchmark evidence.

### D. Precompute a pool of ephemeral keys

Amortizes keygen but does nothing about the ECDH cost, which is 74% of
`Encode`. Doesn't move the needle enough to justify the engineering.
Rejected.

### E. **HKDF-only versioned symmetric key** (recommended)

Drop ECDH entirely. The room private key (32 bytes of high-entropy scalar)
is already perfectly good IKM. Derive the per-version AES key as
`HKDF-SHA256(roomPriv_v, salt=nil, info="room-message-encryption-v2")`,
cache it on both sides keyed on `(roomId, version)`, and use it directly.
Wire format becomes `{version, nonce, ciphertext}` — no
`ephemeralPublicKey`. Per-version FS preserved; per-message FS (which we
never had) unchanged. Hot path becomes a nonce read + GCM seal: ~200 ns per
message.

> **Implemented note (2026-05-21):** The HKDF step was subsequently removed.
> The 32-byte room secret is used directly as the AES-256-GCM key
> (`aesKey = roomPrivateKey`) — no key derivation. Since the secret is
> generated by `crypto/rand` it is already uniform random material; HKDF
> provided no confidentiality benefit. The `golang.org/x/crypto/hkdf` import
> is gone; the info string `"room-message-encryption-v2"` is retired.

---

## Recommendation

Adopt Alternative E. Replace the existing scheme entirely (no dual-scheme
migration window). Server and chat-frontend ship together in this branch.

---

## New wire format

```go
// pkg/roomcrypto/roomcrypto.go
type EncryptedMessage struct {
    // Version is the room key version; matches roomkeystore VersionedKeyPair.Version.
    Version int `json:"version"`

    // Nonce is the 12-byte AES-GCM nonce.
    Nonce []byte `json:"nonce"`

    // Ciphertext is content || 16-byte GCM tag.
    Ciphertext []byte `json:"ciphertext"`
}
```

This is a **breaking** change to the wire format. The old `EphemeralPublicKey`
field is removed entirely. Clients on the old scheme (i.e., the Swift
client, until separately updated) will fail to decode messages from a
server running this change and will display whatever fallback their
decoder surfaces. chat-frontend's existing "[encrypted message]"
placeholder code is replaced with a real decoder in this PR, so it sees no
regression.

---

## Algorithm

### Server `(*Encoder).Encode(roomID, content, roomPrivateKey, version)`

1. Look up (or compute and cache) `aesKey_v` for `(roomID, version)`. On
   cache miss:
   `aesKey_v = HKDF-SHA256(roomPrivateKey, salt=nil, info=[]byte("room-message-encryption-v2"))`,
   read 32 bytes; construct an `aes.NewCipher` + `cipher.NewGCM`; store the
   resulting `cipher.AEAD` in the cache.
2. Generate a fresh 12-byte nonce from `crypto/rand`.
3. `gcm.Seal(nil, nonce, []byte(content), nil)`.
4. Return `EncryptedMessage{Version: version, Nonce: nonce, Ciphertext: ciphertext}`.

Two notable API changes:

- **`Encode` now takes the room private key, not the public key.** The
  server already has `key.KeyPair.PrivateKey` in
  `broadcast-worker/handler.go`. Lines 210 and 254 swap
  `key.KeyPair.PublicKey` for `key.KeyPair.PrivateKey`.
- **`Encode` is now a method on `*Encoder`,** not a free function. The
  encoder owns the per-version AES-GCM cipher cache, eviction policy, and
  the `crypto/rand` reader.

```go
// pkg/roomcrypto/roomcrypto.go
type cacheKey struct {
    roomID  string
    version int
}

type Encoder struct {
    mu    sync.RWMutex
    cache map[cacheKey]cipher.AEAD
    rand  io.Reader // crypto/rand.Reader; overridable for tests
    max   int       // MaxCacheEntries
}

func NewEncoder(opts ...EncoderOption) *Encoder

func (e *Encoder) Encode(roomID, content string, roomPrivateKey []byte, version int) (*EncryptedMessage, error)
```

The free function `roomcrypto.Encode` is **removed** in this PR. All
callers (`broadcast-worker/handler.go:210, 254`) move to the encoder.

### Client decrypt (chat-frontend)

```ts
// chat-frontend/src/lib/roomcrypto/roomcrypto.ts (new module)

export async function decryptRoomMessage(
  ciphertext: Uint8Array,
  nonce: Uint8Array,
  aesKey: CryptoKey,
): Promise<string>

export async function deriveAesKey(
  roomPrivateKey: Uint8Array, // 32-byte raw key
): Promise<CryptoKey>
```

Internals:

1. `deriveAesKey` uses `crypto.subtle.importKey('raw', roomPrivateKey, 'HKDF', ...)`
   then `crypto.subtle.deriveKey({name:'HKDF', hash:'SHA-256', salt:new Uint8Array(0), info:utf8('room-message-encryption-v2')}, ikm, {name:'AES-GCM', length:256}, false, ['decrypt'])`.
2. `decryptRoomMessage` calls `crypto.subtle.decrypt({name:'AES-GCM', iv:nonce, tagLength:128}, aesKey, ciphertext)`, then `TextDecoder('utf-8').decode(...)`.

Caller (the room-keys context) wraps `deriveAesKey` so the resulting
`CryptoKey` is cached per `(roomId, version)`.

---

## Server changes

### `pkg/roomcrypto/roomcrypto.go`

- `EncryptedMessage` struct: drop `EphemeralPublicKey`. Keep
  `Version`, `Nonce`, `Ciphertext`.
- Remove the free `Encode` function.
- Add `Encoder` struct, `NewEncoder()`, `EncoderOption`, and
  `(*Encoder).Encode(roomID, content, roomPrivateKey, version)`.
- Per-version cache: `map[cacheKey]cipher.AEAD`, protected by
  `sync.RWMutex`, bounded by `MaxCacheEntries` (default 4096; env override
  `ROOM_CRYPTO_CACHE_SIZE`) with simple lowest-version eviction described
  under Implementation Details.

### `pkg/roomcrypto/roomcrypto_test.go`

- Drop all tests that exercise the legacy `Encode` / `EphemeralPublicKey`
  path. The legacy code is gone; testing it is dead weight.
- New table-driven `TestEncoder_Encode`: happy path, empty content, too-short
  private key (31 bytes), too-long private key (33 bytes), version stamped
  on output, no `EphemeralPublicKey` field present in JSON.
- New `TestEncoder_RoundTrip`: encode, then decode inline using
  HKDF-SHA256(privKey, info="room-message-encryption-v2") + AES-GCM, content
  matches.
- New `TestEncoder_CacheHit`: two encodes for same (roomID, version) derive
  the AES key only once (instrument via a test hook on the encoder).
- New `TestEncoder_CacheEviction`: fill to `MaxCacheEntries`, add one more,
  assert lowest-version entry evicted.
- New `TestEncoder_NonDeterminism`: two encodes for same inputs produce
  different nonces and different ciphertexts (under the same cached AES key,
  this is solely a property of the random nonce).
- New `TestEncoder_RandReaderError`: inject a failing reader; nonce
  generation surfaces a wrapped error.

### `pkg/roomcrypto/bench_test.go`

- Already committed. Update to benchmark `(*Encoder).Encode` as the
  primary case, leave the curve-comparison benchmarks for posterity.

### `pkg/roomcrypto/integration_test.go`

- Existing test uses a Node container to validate a TypeScript decrypt
  script against `pkg/roomcrypto.Encode`. Update the TS decrypt script to
  the new HKDF-only algorithm; update `decryptPayload` struct accordingly
  (drop the publicKey field; keep privateKey).

### `broadcast-worker/handler.go`

- Hold `*roomcrypto.Encoder` on the handler struct, constructed in
  `main.go`.
- Lines 210 and 254: replace
  `roomcrypto.Encode(content, key.KeyPair.PublicKey, key.Version)` with
  `h.encoder.Encode(roomID, content, key.KeyPair.PrivateKey, key.Version)`.
  Pass `meta.ID` / `edited.RoomID` (or `msg.RoomID` — whichever is in
  scope) as the `roomID` argument.

### `broadcast-worker/main.go`

- Parse `ROOM_CRYPTO_CACHE_SIZE` (envDefault `4096`).
- Construct the encoder via `roomcrypto.NewEncoder(roomcrypto.WithMaxCacheEntries(cfg.RoomCryptoCacheSize))`.
- Pass to the handler constructor.

### `broadcast-worker/handler_test.go`

- Mock expectations stay the same (`pkg/roomcrypto` has no mocks; a real
  encoder is injected with no external dependencies).
- New case: published event's `encryptedMessage` JSON contains
  `version`, `nonce`, `ciphertext` but **no** `ephemeralPublicKey` field.

### `pkg/model/event.go`

- No struct changes. `RoomEvent.EncryptedMessage` remains
  `json.RawMessage`; the inner shape change lives in `pkg/roomcrypto`.

### `docs/client-api.md`

- Update §4.1 RoomEvent description and the room-encryption section to
  describe the new wire layout (no ephemeral public key) and the new
  derivation algorithm. Per project rule, this doc update is in the same
  PR as the broadcast-worker change.

### Mock regeneration

- `make generate SERVICE=broadcast-worker` after the handler signature
  changes.

---

## chat-frontend changes

The frontend changes implement room-key handling and message decryption
end-to-end. This is greenfield work — there is no existing decoder to
extend or be compatible with.

### New module: `src/lib/roomcrypto/roomcrypto.ts`

Pure utility using Web Crypto API. Per `chat-frontend/CLAUDE.md`, `lib/`
is the right home: no React, no NATS, no async I/O beyond `crypto.subtle`.

```ts
// Derive an AES-256-GCM CryptoKey from a 32-byte room private key.
export async function deriveAesKey(roomPrivateKey: Uint8Array): Promise<CryptoKey>

// Decrypt {nonce, ciphertext} produced by the server encoder.
export async function decryptRoomMessage(
  ciphertext: Uint8Array,
  nonce: Uint8Array,
  aesKey: CryptoKey,
): Promise<string>

// Decode helpers: base64 → Uint8Array. Re-exported for tests + callers.
export function b64decode(s: string): Uint8Array
```

Test file `roomcrypto.test.ts` covers:

- Round-trip against a fixed Go-produced ciphertext (committed as test
  fixture under `chat-frontend/test/fixtures/`).
- HKDF parameters match server (info string, salt=empty, 32-byte output).
- Reject ciphertext with wrong tag (GCM verification failure surfaces a
  clear error).
- Reject ciphertext with wrong nonce length.

### New API op: `src/api/subscribeToRoomKeyEvents/index.ts`

```ts
export function subscribeToRoomKeyEvents(
  nats: Nats,
  onEvent: (evt: RoomKeyEvent) => void,
): Subscription
```

Subject: `chat.user.${account}.event.room.key` (build via
`api/_transport/subjects.ts` — add `userRoomKey(account)`). Wire shape
mirrors `pkg/model.RoomKeyEvent`: `{ roomId, version, privateKey, timestamp }`
where `privateKey` is base64.

Re-exported from `src/api/index.ts`.

### Extend `RoomsInfoBatch` bootstrap (existing RPC)

`room-service/handler.go:874` already returns `privateKey` + `keyVersion`
per room from the batch RPC. chat-frontend does not call this RPC today.
Two options:

- **Option F1 (recommended):** add a new lightweight RPC
  `chat.user.{account}.request.rooms.keys` that, given the caller's
  subscribed room IDs (derived server-side from the user's subscriptions),
  returns `[{roomId, version, privateKey}]`. Cleaner API surface; isolates
  the key-bootstrap concern.
- **Option F2:** call the existing `RoomsInfoBatch` RPC for the keys. Less
  new server-side surface, but couples key bootstrap to room-info fetch
  (which today is on-demand, not on-connect).

For this spec, go with **F1**. New handler in `room-service`:

- Subject: `chat.user.{account}.request.rooms.keys`.
- Request: empty (or `{roomIds: []}` — but the server has the
  authoritative subscription list, so it can derive without trusting
  client input).
- Response: `[{roomId, version, privateKey}]` — only rooms the caller is
  subscribed to AND that have a key in Valkey.

Add tests in `room-service/handler_test.go` and update `docs/client-api.md`.

### New context: `src/context/RoomKeysContext/`

Folder layout per `chat-frontend/CLAUDE.md`:

```
context/RoomKeysContext/
├── RoomKeysContext.tsx     ← provider + useRoomKeys() hook
├── reducer.ts              ← state machine
├── useKeyBootstrap.ts      ← initial fetch hook
├── index.tsx               ← re-export
└── *.test.{ts,tsx}
```

State shape:

```ts
type StoredKey = {
  privateKey: Uint8Array   // raw 32-byte scalar (base64-decoded)
  aesKey?: CryptoKey       // lazily derived, cached
}

type RoomKeysState = {
  // outer key: roomId; inner key: version
  byRoom: Record<string, Record<number, StoredKey>>
  bootstrapped: boolean
}
```

Actions:

- `BOOTSTRAP_LOADED`: bulk insert from the new keys RPC.
- `KEY_RECEIVED`: insert one `{roomId, version, privateKey}` from the
  subscription. Idempotent — replacing an existing entry is fine (the same
  privateKey should arrive).
- `CLEAR_KEYS`: on logout/reconnect, clear all state.

Provider responsibilities:

1. On mount (post-login), call the new keys-RPC; dispatch
   `BOOTSTRAP_LOADED`.
2. Subscribe to `subscribeToRoomKeyEvents`; dispatch `KEY_RECEIVED` on each
   event.
3. Unsubscribe + clear on unmount/reconnect.

Exported hook `useDecryptMessage(roomId, version, ciphertextB64, nonceB64): Promise<string | null>`:

- Look up `byRoom[roomId]?.[version]`. If absent, return `null` (caller
  surfaces placeholder).
- If `aesKey` not cached, derive it via `deriveAesKey(privateKey)` and
  store back into state via a dispatch (or write directly into the
  ref-backed cache; see Implementation Details).
- Call `decryptRoomMessage(ciphertext, nonce, aesKey)`; return the
  plaintext on success or `null` on failure.

### Reducer integration: `src/context/RoomEventsContext/`

Today's `MESSAGE_RECEIVED` handler at `reducer.js:296-318` does the
"synthesize a placeholder" fallback when `evt.encryptedMessage` is
present and `evt.message` is not. Replace this with a decrypt attempt
**before** the placeholder fallback:

The trick: the reducer is pure synchronous JS. Decryption is async.
Resolution: do the decrypt in the **dispatcher** (the
`subscribeToRoomEvents` callback in `useRoomSubscriptions.js:221`), not in
the reducer. When `evt.encryptedMessage` is present:

1. Look up the key from the room-keys context.
2. Attempt to decrypt via `useDecryptMessage` (or the underlying async
   function exposed by the context).
3. If decrypt succeeds: parse the JSON `ClientMessage`, replace
   `evt.message` with the decoded message, drop `evt.encryptedMessage`,
   dispatch `MESSAGE_RECEIVED` as usual.
4. If decrypt fails (no key, wrong version, GCM tag mismatch): dispatch a
   variant that keeps the placeholder path (today's behavior).

The reducer itself only sees fully-decoded events. The placeholder
fallback at lines 308-318 stays as the last-resort branch for events that
arrived before the key did.

Edits (`MessageEditedPayload.encryptedNewContent` at
`pkg/model/event.go:152`) take the same treatment: decrypt in the
dispatcher, hand the plaintext `NewContent` to the reducer.

### Tests

Following `chat-frontend/CLAUDE.md` testing conventions:

- `lib/roomcrypto/roomcrypto.test.ts`: round-trip with a Go-produced
  fixture (see Implementation Details for how the fixture is generated).
- `api/subscribeToRoomKeyEvents/index.test.ts`: argument-to-payload
  mapping; subject correctness; callback receives parsed event.
- `context/RoomKeysContext/reducer.test.ts`: pure JS tests on all three
  actions; idempotency of `KEY_RECEIVED`.
- `context/RoomKeysContext/RoomKeysContext.test.tsx`: provider mounts,
  calls bootstrap RPC, subscribes, dispatches; unmount unsubscribes.
- `context/RoomEventsContext/reducer.test.js`: existing
  "placeholder when only encryptedMessage" case stays — covers the
  no-key-yet branch. **New** case: decrypted event arrives with
  populated `message`, reducer treats it as a normal new-message event.
- `context/RoomEventsContext/useRoomSubscriptions.test.js` (or a new
  test file): dispatcher path decrypts successfully, then dispatches the
  decoded event.

### Smoke

Add a new `scripts/encryption.smoke.mjs` (or extend the existing
`smoke-test.mjs`) that runs end-to-end against the live stack: send an
encrypted message, then assert chat-frontend's decoder produces the
expected plaintext.

### What this does NOT change in chat-frontend

- `RoomEvent` wire type stays the same (`encryptedMessage` is still
  `unknown`/`json.RawMessage`-equivalent at the TS layer).
- `subscribeToRoomEvents` stays the same.
- The placeholder rendering code path is preserved as a fallback.
- No UI changes — decrypted messages flow through the same render path
  plaintext messages do today.

---

## Migration plan

One deploy. Server and chat-frontend ship in the same PR. Encrypted
broadcast events are transient; there are no historical scheme-0
ciphertexts to support. The Swift client (out-of-repo) will see decode
failures from the moment the server flips — coordination of that update
is outside this branch's scope and tracked separately.

Deployment order if any: chat-frontend first, then server. Either order
is safe (chat-frontend's placeholder fallback handles any scheme it can't
decode), but frontend-first means users see the new decoded messages the
moment the server flips, with no in-flight gap.

Rollback: revert the PR. Encrypted ciphertexts are transient; nothing
persistent depends on the new format.

---

## Implementation details

### Cache key (server)

The per-version AES key depends on `(roomID, version)` because different
rooms have different private keys. Cache key:

```go
type cacheKey struct {
    roomID  string
    version int
}
```

### Cache size and eviction (server)

- Default `MaxCacheEntries = 4096` (override via env
  `ROOM_CRYPTO_CACHE_SIZE`, parsed in `broadcast-worker/main.go`).
- Eviction: on insert over the limit, drop the entry with the lowest
  `version` value across all rooms. Deliberately simple — hot rooms keep
  their entries, rare rooms with old versions get dropped first. Linear
  scan per eviction is acceptable at n=4096.
- Cache entries are `cipher.AEAD` values, not raw bytes — derived once
  via `aes.NewCipher` + `cipher.NewGCM` and reused for every `Seal`.

### Key-version rotation correctness

On rotation, `broadcast-worker` reads the new `VersionedKeyPair` from
Valkey on its next message. `(*Encoder).Encode` misses on the new
version, derives, caches. Old entries age out under the size bound. The
old AES key is never asked for again because no encrypt path requests an
old version. chat-frontend keeps the old version in `RoomKeysState`
indefinitely so any in-flight messages encrypted under the old version
(during the rotation grace window) still decrypt; eviction policy on the
client side is "drop after `VALKEY_KEY_GRACE_PERIOD * 2`" or simpler
"keep up to N most recent versions per room"; see open question O3.

### Nonce hygiene

With one AES key per (room, version), nonce collision becomes a real
concern. `crypto/rand` 96-bit random nonces collide with probability
~2⁻³² after ~2³² messages **per key version**. Mitigations:

1. **Document the rotation cadence as a security parameter.** Room-key
   rotation policy must keep "messages per key version" comfortably below
   2³² ≈ 4.3B. At 10K msg/s on one room that's ~5 days between mandatory
   rotations — trivially satisfied at realistic chat scale.
2. **Counter-based nonces if rotation cadence is ever in doubt.** Out of
   scope here; flagged as future work.

### Fixture generation for chat-frontend roundtrip tests

A small Go program under `chat-frontend/scripts/gen-crypto-fixtures.go`
(invoked manually, output committed to
`chat-frontend/test/fixtures/encrypted-message.json`) encodes a known
plaintext with a known private key + version. The TS test reads the
fixture and verifies decryption produces the original plaintext. Cross-
language round-trip coverage in one direction is sufficient; the
integration test in `pkg/roomcrypto/integration_test.go` covers the
other direction (Go encode → Node decode via tsx).

### Test reproducibility

Benchmark numbers in this document were captured on the dev container's
Xeon @ 2.80 GHz. The relative ratios (Encode vs symmetric-only) should
hold across CPU generations.

---

## Risks and open questions

### R1. PRNG failure mode

If `crypto/rand.Read` returns identical bytes on two calls under the
same AES key, GCM authenticity is broken and the auth key can be
recovered. The current scheme hides this behind per-message key
derivation; the new scheme is exposed to it directly. Mitigation: this
is a system-wide catastrophe under either scheme (every NKey signature,
JWT generation, etc. is equally affected), so localizing the concern to
this change is misleading. Accept the risk.

### R2. Forgetting to rotate

If a room never rotates, its AES key encrypts the room's entire history
under one key. Mitigation: document the rotation cadence as a security
parameter (see Nonce hygiene). If rotation discipline is a real concern,
follow-up work can add an automated rotation policy in `roomkeystore`.
Out of scope here.

### R3. Swift client breaks at the moment of server flip

The Swift client expects scheme-0 (with `ephemeralPublicKey`) on the
wire. The moment the server flips, Swift cannot decrypt. Mitigation:
out-of-scope for this branch; tracked separately. If Swift is on the
critical path, defer this change until the Swift update is ready.

### O1. New RPC vs. extending an existing one for key bootstrap

This spec proposes a new `chat.user.{account}.request.rooms.keys` RPC.
Alternative is to extend `RoomsInfoBatch` so chat-frontend calls it on
connect. Choosing the new RPC keeps responsibilities separate and avoids
a client behavior change ("now we call RoomsInfoBatch on connect, not
on-demand"). Confirm with reviewer before implementation.

### O2. AES key derivation cached at decrypt site (client)

`useDecryptMessage` returns a `Promise<string | null>` and lazily caches
the derived `CryptoKey` per (roomId, version). Caching via dispatch
loops through the React reducer; caching via a ref is faster but lives
outside React state. Recommend: ref-backed map inside the context
provider, exposed via a stable callback. State carries only the raw
`privateKey`; the derived `CryptoKey` is a transient cache.

### O3. Old-version retention policy (client)

chat-frontend's `RoomKeysState.byRoom[roomId]` accumulates versions over
time. Two reasonable policies:

- Keep all versions ever received in this session (memory leak in
  long-lived tabs).
- Keep last N versions per room (default N=2 — current + previous).

Recommend the latter, default N=2 (matches Valkey's previous-key grace
slot). Flag for reviewer.

### O4. Edit-content key version

`MessageEditedPayload.encryptedNewContent` is encrypted under whatever
the current key is at edit time. The decryptor needs the version. The
`EncryptedMessage` JSON inside `encryptedNewContent` carries `version`
already — no payload change needed. Just confirmed for the spec.

---

## Future work (deferred)

- **Counter-based nonces.** If sustained per-room throughput approaches
  2³² messages between rotations.
- **Automated key rotation policy.** Tie rotation to a max-messages or
  max-age policy enforced in `roomkeystore`.
- **Swift client update.** Tracked separately.
- **Removing the chat-frontend `[encrypted message]` fallback once the
  bootstrap path is rock-solid.** For now it stays as the last-resort
  branch for events that arrive before their key does.
- **Persisting room keys to IndexedDB across reloads.** Today every
  reload re-bootstraps via the keys RPC. Cheap at low room counts; revisit
  if room counts per user grow large.

---

## Out of scope

- Swift client decoder implementation.
- Re-encrypting historical messages (none are persisted encrypted).
- Changes to `roomkeystore`, `room-key-sender`, or NATS subjects
  beyond the new keys-bootstrap RPC.
- Changes to `message-worker`, `history-service`, `search-service`.
- Performance work outside `roomcrypto` and its caller in
  `broadcast-worker`.
- Persisting room keys client-side beyond the lifetime of the tab.
