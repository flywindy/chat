# ECDH Performance — HKDF-Only Versioned Symmetric Key Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the ECIES-style per-message ephemeral-key encryption in `pkg/roomcrypto` with a versioned symmetric AES-256-GCM key derived once per `(roomId, version)` via HKDF-SHA256. Implement the first chat-frontend decoder end-to-end so the in-repo client decrypts real messages instead of rendering a placeholder. Drop the legacy scheme entirely; no dual-scheme migration window.

**Architecture:** Server-side: a new `roomcrypto.Encoder` type owns a per-`(roomId, version)` `cipher.AEAD` cache; `broadcast-worker` constructs one in `main.go` and replaces direct `Encode()` calls. Client-side: a new `RoomKeysContext` bootstraps room private keys via a new `room-service` RPC, subscribes to live key-rotation events, lazily derives `CryptoKey`s, and exposes `decryptEvent(evt)` to the `RoomEvents` dispatcher so the reducer only ever sees fully-decoded events.

**Tech Stack:** Go 1.25 (`crypto/aes`, `crypto/cipher`, `crypto/rand`, `golang.org/x/crypto/hkdf`); TypeScript / React (Web Crypto `crypto.subtle`); NATS request/reply + subscribe; Vitest + @testing-library/react.

**Spec reference:** `docs/superpowers/specs/2026-05-20-ecdh-performance-analysis-design.md`

---

## File map

### Server (Go)

| Path | What changes |
|---|---|
| `pkg/roomcrypto/roomcrypto.go` | Drop `EphemeralPublicKey`; remove free `Encode`; add `Encoder` struct, `NewEncoder` + options, `(*Encoder).Encode` |
| `pkg/roomcrypto/roomcrypto_test.go` | Remove legacy tests; add tests for `Encoder` |
| `pkg/roomcrypto/bench_test.go` | Update to benchmark `(*Encoder).Encode` |
| `pkg/roomcrypto/integration_test.go` | Update TS decrypt script + fixtures for new scheme |
| `pkg/roomcrypto/testdata/decrypt.ts` | Rewrite TS reference decoder |
| `broadcast-worker/handler.go` | Hold `*roomcrypto.Encoder`; call `Encode(roomID, content, privKey, version)` |
| `broadcast-worker/handler_test.go` | Pass real encoder into handler under test |
| `broadcast-worker/main.go` | Parse `ROOM_CRYPTO_CACHE_SIZE`; construct encoder |
| `pkg/subject/subject.go` | Add `RoomsKeysBootstrap(account)`, `RoomsKeysBootstrapWildcard()` |
| `pkg/model/event.go` | Add `RoomsKeysResponse`, `RoomsKeysEntry` |
| `room-service/handler.go` | New `natsListRoomKeys` handler; register in `RegisterCRUD` |
| `room-service/handler_test.go` | Tests for `natsListRoomKeys` |
| `docs/client-api.md` | Document new wire format + new RPC |

### chat-frontend (TS/JS)

| Path | What changes |
|---|---|
| `chat-frontend/src/lib/roomcrypto/roomcrypto.ts` | NEW — `deriveAesKey`, `decryptRoomMessage`, `b64decode` |
| `chat-frontend/src/lib/roomcrypto/roomcrypto.test.ts` | NEW — round-trip + error cases |
| `chat-frontend/src/lib/roomcrypto/index.ts` | NEW — barrel |
| `chat-frontend/scripts/gen-crypto-fixtures.go` | NEW — generates a known-plaintext fixture |
| `chat-frontend/test/fixtures/encrypted-message.json` | NEW — committed fixture |
| `chat-frontend/src/api/_transport/subjects.ts` | Add `userRoomKey`, `roomsKeysBootstrap` |
| `chat-frontend/src/api/_transport/subjects.test.js` | Cover the new builders |
| `chat-frontend/src/api/types.ts` | Add `RoomKeyEvent`, `RoomKeysEntry`, `RoomKeysResponse` |
| `chat-frontend/src/api/subscribeToRoomKeyEvents/index.ts` | NEW op |
| `chat-frontend/src/api/subscribeToRoomKeyEvents/index.test.ts` | NEW |
| `chat-frontend/src/api/fetchRoomKeysBootstrap/index.ts` | NEW op |
| `chat-frontend/src/api/fetchRoomKeysBootstrap/index.test.ts` | NEW |
| `chat-frontend/src/api/index.ts` | Add new barrel re-exports |
| `chat-frontend/src/context/RoomKeysContext/reducer.ts` | NEW |
| `chat-frontend/src/context/RoomKeysContext/reducer.test.ts` | NEW |
| `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.tsx` | NEW |
| `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.test.tsx` | NEW |
| `chat-frontend/src/context/RoomKeysContext/index.tsx` | NEW — barrel |
| `chat-frontend/src/App.jsx` | Wrap in `<RoomKeysProvider>` |
| `chat-frontend/src/context/RoomEventsContext/RoomEventsContext.tsx` | Pass `decryptEvent` to `useRoomSubscriptions` |
| `chat-frontend/src/context/RoomEventsContext/useRoomSubscriptions.js` | Decrypt before dispatch |
| `chat-frontend/src/context/RoomEventsContext/reducer.test.js` | Add scheme-1 round-trip case |

---

## Phase 1 — Server `pkg/roomcrypto` rewrite

### Task 1: Add `Encoder` struct alongside legacy `Encode` (red)

**Files:**
- Test: `pkg/roomcrypto/roomcrypto_test.go`

The legacy `Encode` stays for now so `broadcast-worker` keeps compiling. We add the new type alongside.

- [ ] **Step 1: Append the new failing test to `roomcrypto_test.go`**

```go
func TestEncoder_Encode_HappyPath(t *testing.T) {
	// Use a fixed 32-byte private key (a P-256 scalar's worth of entropy).
	priv := make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i + 1)
	}

	enc := NewEncoder()
	got, err := enc.Encode("room-1", "hello", priv, 7)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 7, got.Version)
	assert.Len(t, got.Nonce, 12)
	assert.NotEmpty(t, got.Ciphertext)
	// EphemeralPublicKey field must be empty/unset on the new scheme output.
	assert.Empty(t, got.EphemeralPublicKey)
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test ./pkg/roomcrypto/ -run TestEncoder_Encode_HappyPath -v`
Expected: FAIL — `undefined: NewEncoder` (or similar).

- [ ] **Step 3: Implement `Encoder` in `pkg/roomcrypto/roomcrypto.go`**

Append to the bottom of the file (do NOT modify the existing legacy `Encode` yet):

```go
// Encoder holds the per-(roomId, version) AES-GCM cipher cache for the
// HKDF-only encryption scheme. Construct one per process and share it
// across goroutines.
type Encoder struct {
	mu    sync.RWMutex
	cache map[encoderCacheKey]cipher.AEAD
	rand  io.Reader
	max   int
}

type encoderCacheKey struct {
	roomID  string
	version int
}

// EncoderOption configures an Encoder at construction time.
type EncoderOption func(*Encoder)

// WithMaxCacheEntries sets the upper bound on the per-(roomId, version)
// AES-GCM cache. When exceeded, the entry with the lowest version is
// evicted. Default 4096.
func WithMaxCacheEntries(n int) EncoderOption {
	return func(e *Encoder) { e.max = n }
}

// WithRand overrides the source of randomness used for nonce generation.
// Intended for testing only.
func WithRand(r io.Reader) EncoderOption {
	return func(e *Encoder) { e.rand = r }
}

// NewEncoder constructs an Encoder with default cache size 4096 and
// crypto/rand.Reader as the randomness source.
func NewEncoder(opts ...EncoderOption) *Encoder {
	e := &Encoder{
		cache: make(map[encoderCacheKey]cipher.AEAD),
		rand:  rand.Reader,
		max:   4096,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Encode encrypts content under the AES key derived from roomPrivateKey
// for the given (roomID, version). The derived AES-GCM cipher is cached
// on the Encoder; repeat calls for the same (roomID, version) skip key
// derivation.
func (e *Encoder) Encode(roomID, content string, roomPrivateKey []byte, version int) (*EncryptedMessage, error) {
	gcm, err := e.aeadFor(roomID, roomPrivateKey, version)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(e.rand, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, []byte(content), nil)
	return &EncryptedMessage{
		Version:    version,
		Nonce:      nonce,
		Ciphertext: ciphertext,
	}, nil
}

func (e *Encoder) aeadFor(roomID string, roomPrivateKey []byte, version int) (cipher.AEAD, error) {
	key := encoderCacheKey{roomID: roomID, version: version}

	e.mu.RLock()
	gcm, ok := e.cache[key]
	e.mu.RUnlock()
	if ok {
		return gcm, nil
	}

	if len(roomPrivateKey) != 32 {
		return nil, fmt.Errorf("room private key must be 32 bytes, got %d", len(roomPrivateKey))
	}

	aesKey := make([]byte, 32)
	r := hkdf.New(sha256.New, roomPrivateKey, nil, []byte("room-message-encryption-v2"))
	if _, err := io.ReadFull(r, aesKey); err != nil {
		return nil, fmt.Errorf("deriving AES key: %w", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	newGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM wrapper: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	// Double-check under write lock — another goroutine may have populated.
	if existing, ok := e.cache[key]; ok {
		return existing, nil
	}
	if len(e.cache) >= e.max {
		e.evictLowestVersionLocked()
	}
	e.cache[key] = newGCM
	return newGCM, nil
}

// evictLowestVersionLocked drops the entry with the lowest version
// across all rooms. Caller must hold e.mu for writing.
func (e *Encoder) evictLowestVersionLocked() {
	var (
		victim    encoderCacheKey
		haveFirst bool
	)
	for k := range e.cache {
		if !haveFirst || k.version < victim.version {
			victim = k
			haveFirst = true
		}
	}
	if haveFirst {
		delete(e.cache, victim)
	}
}
```

Add the missing imports to the existing import block:

```go
import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/hkdf"
)
```

(The `crypto/ecdh` import stays because legacy `Encode` still uses it.)

Add the `EphemeralPublicKey` field tag change in the existing struct so the new scheme can omit it cleanly:

```go
type EncryptedMessage struct {
	Version            int    `json:"version"`
	EphemeralPublicKey []byte `json:"ephemeralPublicKey,omitempty"` // legacy scheme; empty on the new scheme
	Nonce              []byte `json:"nonce"`
	Ciphertext         []byte `json:"ciphertext"`
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./pkg/roomcrypto/ -run TestEncoder_Encode_HappyPath -v`
Expected: PASS.

- [ ] **Step 5: Run the full package to make sure legacy tests still pass**

Run: `go test ./pkg/roomcrypto/ -v`
Expected: All tests pass (legacy `TestEncode*` + the one new test).

- [ ] **Step 6: Commit**

```bash
git add pkg/roomcrypto/roomcrypto.go pkg/roomcrypto/roomcrypto_test.go
git commit -m "feat(roomcrypto): add HKDF-only Encoder type alongside legacy Encode"
```

---

### Task 2: Encoder cache-hit test

**Files:**
- Test: `pkg/roomcrypto/roomcrypto_test.go`

We need to assert that two encodes for the same `(roomID, version)` re-use the cached cipher. Easiest signal: the cache map size after two calls equals one.

- [ ] **Step 1: Add a test-only accessor in `pkg/roomcrypto/roomcrypto.go`**

Append:

```go
// cacheLen is exported only for tests in the same package.
func (e *Encoder) cacheLen() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.cache)
}
```

- [ ] **Step 2: Add the failing test**

```go
func TestEncoder_Encode_CacheHit(t *testing.T) {
	priv := bytes.Repeat([]byte{0xAB}, 32)
	enc := NewEncoder()

	_, err := enc.Encode("room-1", "msg1", priv, 1)
	require.NoError(t, err)
	_, err = enc.Encode("room-1", "msg2", priv, 1)
	require.NoError(t, err)

	assert.Equal(t, 1, enc.cacheLen(), "same (roomID, version) must not re-derive the AES key")
}

func TestEncoder_Encode_DistinctVersionsCacheSeparately(t *testing.T) {
	priv := bytes.Repeat([]byte{0xAB}, 32)
	enc := NewEncoder()

	_, err := enc.Encode("room-1", "msg1", priv, 1)
	require.NoError(t, err)
	_, err = enc.Encode("room-1", "msg2", priv, 2)
	require.NoError(t, err)

	assert.Equal(t, 2, enc.cacheLen(), "different versions must occupy distinct cache entries")
}
```

- [ ] **Step 3: Run the tests**

Run: `go test ./pkg/roomcrypto/ -run TestEncoder_Encode_CacheHit -v && go test ./pkg/roomcrypto/ -run TestEncoder_Encode_DistinctVersions -v`
Expected: PASS (the implementation from Task 1 already handles caching).

- [ ] **Step 4: Commit**

```bash
git add pkg/roomcrypto/roomcrypto.go pkg/roomcrypto/roomcrypto_test.go
git commit -m "test(roomcrypto): assert Encoder caches AEAD per (roomID, version)"
```

---

### Task 3: Encoder cache-eviction test

**Files:**
- Test: `pkg/roomcrypto/roomcrypto_test.go`

- [ ] **Step 1: Add the failing test**

```go
func TestEncoder_Encode_EvictsLowestVersion(t *testing.T) {
	priv := bytes.Repeat([]byte{0x42}, 32)
	enc := NewEncoder(WithMaxCacheEntries(2))

	_, err := enc.Encode("room-A", "a", priv, 1)
	require.NoError(t, err)
	_, err = enc.Encode("room-A", "b", priv, 2)
	require.NoError(t, err)
	_, err = enc.Encode("room-A", "c", priv, 3)
	require.NoError(t, err)

	assert.Equal(t, 2, enc.cacheLen(), "cache must not exceed max")
	// Encoding for version 1 again must miss the cache (we evicted it),
	// while versions 2 and 3 must hit.
	prevLen := enc.cacheLen()
	_, err = enc.Encode("room-A", "d", priv, 2)
	require.NoError(t, err)
	assert.Equal(t, prevLen, enc.cacheLen(), "version 2 must still be cached (hit)")

	_, err = enc.Encode("room-A", "e", priv, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, enc.cacheLen(), "after re-inserting v=1, cache stays at max (now v=2 should have been evicted as lowest)")
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./pkg/roomcrypto/ -run TestEncoder_Encode_EvictsLowestVersion -v`
Expected: PASS (the Task 1 implementation already does lowest-version eviction).

- [ ] **Step 3: Commit**

```bash
git add pkg/roomcrypto/roomcrypto_test.go
git commit -m "test(roomcrypto): assert Encoder evicts lowest version when full"
```

---

### Task 4: Encoder rand-reader error path

**Files:**
- Test: `pkg/roomcrypto/roomcrypto_test.go`

- [ ] **Step 1: Add the failing test**

```go
func TestEncoder_Encode_NonceReaderError(t *testing.T) {
	priv := bytes.Repeat([]byte{0xAA}, 32)
	enc := NewEncoder(WithRand(&failReader{}))

	got, err := enc.Encode("room-1", "hello", priv, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generating nonce")
	assert.Nil(t, got)
}

func TestEncoder_Encode_InvalidKeyLength(t *testing.T) {
	enc := NewEncoder()
	got, err := enc.Encode("room-1", "hello", make([]byte, 31), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be 32 bytes")
	assert.Nil(t, got)
}
```

`failReader` already exists in `roomcrypto_test.go` from the legacy tests.

- [ ] **Step 2: Run the tests**

Run: `go test ./pkg/roomcrypto/ -run "TestEncoder_Encode_NonceReaderError|TestEncoder_Encode_InvalidKeyLength" -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/roomcrypto/roomcrypto_test.go
git commit -m "test(roomcrypto): cover Encoder error paths (nonce reader, key length)"
```

---

### Task 5: Round-trip test for Encoder (inline decrypt)

**Files:**
- Test: `pkg/roomcrypto/roomcrypto_test.go`

- [ ] **Step 1: Add the failing test**

```go
func TestEncoder_Encode_RoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "non-empty", content: "hello, world"},
		{name: "empty", content: ""},
		{name: "unicode", content: "héllo 🌎"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priv := bytes.Repeat([]byte{0x55}, 32)
			enc := NewEncoder()
			msg, err := enc.Encode("room-1", tc.content, priv, 3)
			require.NoError(t, err)
			require.NotNil(t, msg)

			// Re-derive the AES key with the same HKDF parameters and decrypt.
			aesKey := make([]byte, 32)
			r := hkdf.New(sha256.New, priv, nil, []byte("room-message-encryption-v2"))
			_, err = io.ReadFull(r, aesKey)
			require.NoError(t, err)

			block, err := aes.NewCipher(aesKey)
			require.NoError(t, err)
			gcm, err := cipher.NewGCM(block)
			require.NoError(t, err)

			plaintext, err := gcm.Open(nil, msg.Nonce, msg.Ciphertext, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.content, string(plaintext))
		})
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./pkg/roomcrypto/ -run TestEncoder_Encode_RoundTrip -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/roomcrypto/roomcrypto_test.go
git commit -m "test(roomcrypto): round-trip Encoder output via inline HKDF/GCM"
```

---

### Task 6: Update `bench_test.go` to benchmark the Encoder

**Files:**
- Modify: `pkg/roomcrypto/bench_test.go`

- [ ] **Step 1: Append a new benchmark to `bench_test.go`**

```go
// BenchmarkEncoder_Encode measures the proposed hot path: encoder cache
// hit + nonce read + AES-GCM seal. This is what broadcast-worker pays
// per message after migration.
func BenchmarkEncoder_Encode(b *testing.B) {
	priv := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, priv); err != nil {
		b.Fatal(err)
	}
	enc := NewEncoder()

	// Warm the cache so we measure the steady-state cost, not the
	// one-time HKDF derivation.
	if _, err := enc.Encode("room-1", "warm", priv, 1); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := enc.Encode("room-1", "hello, world — a typical short chat message", priv, 1); err != nil {
			b.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Run the benchmark to confirm it builds and produces a number**

Run: `go test -bench=BenchmarkEncoder_Encode -benchmem -benchtime=2s -run=^$ ./pkg/roomcrypto/`
Expected: Output line `BenchmarkEncoder_Encode-N ... ns/op ... allocs/op`. Verify allocs/op is small (1–3).

- [ ] **Step 3: Commit**

```bash
git add pkg/roomcrypto/bench_test.go
git commit -m "bench(roomcrypto): add BenchmarkEncoder_Encode for the new hot path"
```

---

### Task 7: Switch `broadcast-worker` to the new Encoder

**Files:**
- Modify: `broadcast-worker/handler.go:210, 254`
- Modify: `broadcast-worker/handler.go` (handler struct + constructor)
- Modify: `broadcast-worker/main.go`
- Modify: `broadcast-worker/handler_test.go`

- [ ] **Step 1: Read the handler's NewHandler signature and constructor wiring**

Run: `grep -n "func NewHandler\|encoder\|encrypt " broadcast-worker/handler.go | head -20`

Confirm the current field set, then add `encoder` as a new dependency.

- [ ] **Step 2: Update `broadcast-worker/handler.go` — add encoder field**

Add field on the handler struct:

```go
type Handler struct {
	// ... existing fields ...
	encoder *roomcrypto.Encoder
}
```

Add it to `NewHandler(...)` parameters and the struct literal it constructs. The exact existing parameter list is fixed — append `encoder *roomcrypto.Encoder` as the last parameter to minimize churn.

- [ ] **Step 3: Update both `roomcrypto.Encode(...)` call sites**

In `encryptEditedContent` (around line 210):

```go
encrypted, err := h.encoder.Encode(roomID, edited.NewContent, key.KeyPair.PrivateKey, key.Version)
```

In `publishChannelEvent` (around line 254):

```go
encrypted, err := h.encoder.Encode(meta.ID, string(msgJSON), key.KeyPair.PrivateKey, key.Version)
```

Note the swap from `key.KeyPair.PublicKey` → `key.KeyPair.PrivateKey` and the added `roomID` first arg.

- [ ] **Step 4: Update `broadcast-worker/main.go` — parse config and construct encoder**

Add to the `config` struct:

```go
RoomCryptoCacheSize int `env:"ROOM_CRYPTO_CACHE_SIZE" envDefault:"4096"`
```

In the `main` function dependency-construction block, before `NewHandler` is called:

```go
encoder := roomcrypto.NewEncoder(roomcrypto.WithMaxCacheEntries(cfg.RoomCryptoCacheSize))
```

Pass `encoder` as the last argument to `NewHandler(...)`.

- [ ] **Step 5: Update `broadcast-worker/handler_test.go` — inject a real encoder**

In every test that constructs a `Handler` via `NewHandler(...)`, append `roomcrypto.NewEncoder()` as the last argument. Search for `NewHandler(` and update each call site.

If there is a shared test helper (e.g., `newTestHandler(t)` in `testhelpers_test.go`), update it once there and the call sites should be unaffected.

- [ ] **Step 6: Run broadcast-worker tests**

Run: `make test SERVICE=broadcast-worker`
Expected: All tests pass.

If a handler test asserts on the published-event JSON, also assert that `encryptedMessage` no longer contains `ephemeralPublicKey`. Example assertion to add (or update):

```go
// Confirm the new wire shape.
var payload map[string]json.RawMessage
require.NoError(t, json.Unmarshal(publishedEvt.EncryptedMessage, &payload))
assert.Contains(t, payload, "version")
assert.Contains(t, payload, "nonce")
assert.Contains(t, payload, "ciphertext")
assert.NotContains(t, payload, "ephemeralPublicKey")
```

- [ ] **Step 7: Build broadcast-worker to confirm it compiles**

Run: `make build SERVICE=broadcast-worker`
Expected: Build succeeds.

- [ ] **Step 8: Commit**

```bash
git add broadcast-worker/handler.go broadcast-worker/handler_test.go broadcast-worker/main.go
git commit -m "feat(broadcast-worker): use HKDF-only roomcrypto.Encoder"
```

---

### Task 8: Remove the legacy free `roomcrypto.Encode`

Now that `broadcast-worker` no longer calls the legacy function, delete it and its associated tests.

**Files:**
- Modify: `pkg/roomcrypto/roomcrypto.go`
- Modify: `pkg/roomcrypto/roomcrypto_test.go`

- [ ] **Step 1: Delete the legacy `Encode` and `encode` functions from `pkg/roomcrypto/roomcrypto.go`**

Remove every line of the legacy `Encode(content, roomPublicKey, version)` function and the internal `encode(content, roomPublicKey, version, randReader)` helper. Remove the `crypto/ecdh` import — it's no longer referenced.

The file should now contain only:
- The `EncryptedMessage` struct (with `EphemeralPublicKey` still present but `omitempty`)
- The `Encoder` type, `encoderCacheKey`, `EncoderOption`, `WithMaxCacheEntries`, `WithRand`, `NewEncoder`, `(*Encoder).Encode`, `aeadFor`, `evictLowestVersionLocked`, `cacheLen`

Keep `EphemeralPublicKey` on the struct (with `omitempty`) for one more PR so the Swift client's JSON parser doesn't need to handle the field disappearing; it will simply always read as empty. (This is the minimum compat surface; the field is deleted in a follow-up PR per spec future-work.)

- [ ] **Step 2: Delete legacy tests in `pkg/roomcrypto/roomcrypto_test.go`**

Remove the following test functions entirely:

- `TestEncode`
- `TestEncode_RoundTrip`
- `TestEncode_NonDeterminism`
- `TestEncode_RandReaderErrors`
- `TestEncode_Version`

Keep:
- `TestEncryptedMessage_JSONRoundTrip` (still valid for the struct shape)
- All `TestEncoder_*` tests added above
- `failReader` (used by Encoder tests)

- [ ] **Step 3: Add a non-determinism test for the Encoder**

```go
func TestEncoder_Encode_NonDeterminism(t *testing.T) {
	priv := bytes.Repeat([]byte{0x33}, 32)
	enc := NewEncoder()

	a, err := enc.Encode("room-1", "same content", priv, 1)
	require.NoError(t, err)
	b, err := enc.Encode("room-1", "same content", priv, 1)
	require.NoError(t, err)

	assert.False(t, bytes.Equal(a.Nonce, b.Nonce), "nonces must differ")
	assert.False(t, bytes.Equal(a.Ciphertext, b.Ciphertext), "ciphertexts must differ")
}
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./pkg/roomcrypto/ -v`
Expected: All tests pass. No references to `crypto/ecdh` remain.

- [ ] **Step 5: Run the rest of the build**

Run: `go build ./...`
Expected: Build succeeds (broadcast-worker no longer references the deleted function).

- [ ] **Step 6: Commit**

```bash
git add pkg/roomcrypto/roomcrypto.go pkg/roomcrypto/roomcrypto_test.go
git commit -m "refactor(roomcrypto): remove legacy ECIES Encode, drop crypto/ecdh dep"
```

---

### Task 9: Update integration test TS decrypt script

**Files:**
- Modify: `pkg/roomcrypto/testdata/decrypt.ts` (if it exists; otherwise look in `pkg/roomcrypto/integration_test.go` for inline TS)
- Modify: `pkg/roomcrypto/integration_test.go`

- [ ] **Step 1: Locate the TS decrypt source**

Run: `ls pkg/roomcrypto/testdata/ 2>/dev/null; grep -n "decrypt\.ts\|tsx\|TextDecoder" pkg/roomcrypto/integration_test.go | head -20`

Identify whether the TS decrypt source is a separate file under `testdata/` or embedded inline in the test. Update whichever applies.

- [ ] **Step 2: Rewrite the TS decryptor to the new scheme**

The new TS decryptor:

```ts
// decrypt.ts — invoked by integration_test.go via tsx
import { createHash, createHmac } from 'node:crypto'

type Payload = {
  privateKey: string  // base64 32-byte raw private scalar (high-entropy IKM)
  message: {
    version: number
    nonce: string       // base64
    ciphertext: string  // base64 = content || 16-byte GCM tag
  }
}

function hkdfSha256(ikm: Buffer, info: Buffer, length: number): Buffer {
  // Salt is empty per server: HKDF-Extract(salt=nil, IKM) = HMAC-SHA-256(0^32, IKM).
  const prk = createHmac('sha256', Buffer.alloc(32)).update(ikm).digest()
  const t = Buffer.alloc(0)
  const out = Buffer.alloc(0)
  let prev = Buffer.alloc(0)
  const blocks: Buffer[] = []
  const n = Math.ceil(length / 32)
  for (let i = 1; i <= n; i++) {
    const h = createHmac('sha256', prk)
    h.update(prev)
    h.update(info)
    h.update(Buffer.from([i]))
    prev = h.digest()
    blocks.push(prev)
  }
  return Buffer.concat(blocks).subarray(0, length)
}

async function main() {
  const raw = await new Promise<string>((resolve, reject) => {
    let chunks = ''
    process.stdin.setEncoding('utf-8')
    process.stdin.on('data', (c) => (chunks += c))
    process.stdin.on('end', () => resolve(chunks))
    process.stdin.on('error', reject)
  })

  const p = JSON.parse(raw) as Payload
  const privateKey = Buffer.from(p.privateKey, 'base64')
  if (privateKey.length !== 32) throw new Error(`expected 32-byte private key, got ${privateKey.length}`)

  const aesKey = hkdfSha256(privateKey, Buffer.from('room-message-encryption-v2'), 32)
  const nonce = Buffer.from(p.message.nonce, 'base64')
  const ciphertext = Buffer.from(p.message.ciphertext, 'base64')

  // Node's createDecipheriv splits ciphertext + auth tag manually.
  const tag = ciphertext.subarray(ciphertext.length - 16)
  const body = ciphertext.subarray(0, ciphertext.length - 16)

  const { createDecipheriv } = await import('node:crypto')
  const decipher = createDecipheriv('aes-256-gcm', aesKey, nonce)
  decipher.setAuthTag(tag)
  const plaintext = Buffer.concat([decipher.update(body), decipher.final()])
  process.stdout.write(plaintext.toString('utf-8'))
}

main().catch((err) => {
  process.stderr.write(`${err.stack ?? err.message ?? err}\n`)
  process.exit(1)
})
```

- [ ] **Step 3: Update `integration_test.go` `decryptPayload` struct**

```go
type decryptPayload struct {
	PrivateKey string            `json:"privateKey"` // base64(roomPrivKeyBytes) — 32 bytes
	Message    *EncryptedMessage `json:"message"`
}
```

Remove the `PublicKey` field — no longer used.

- [ ] **Step 4: Update test bodies to use the new Encoder**

In every place the integration test constructs a payload, replace any call to the legacy `Encode` with the new `Encoder`:

```go
enc := NewEncoder()
msg, err := enc.Encode("room-integration", plaintext, priv, 1)
require.NoError(t, err)

payload := decryptPayload{
	PrivateKey: base64.StdEncoding.EncodeToString(priv),
	Message:    msg,
}
```

Where `priv` is a fresh 32-byte buffer from `crypto/rand`, **not** a P-256 ECDH private key. The new scheme accepts any 32 bytes of high-entropy material as IKM; the integration test should produce one with `crypto/rand.Read`.

- [ ] **Step 5: Run the integration test (requires Docker)**

Run: `make test-integration SERVICE=roomcrypto` (or the equivalent target; check the Makefile for `pkg/` integration paths).

If `make test-integration` does not have a `pkg/roomcrypto` target, run:

`go test -tags=integration ./pkg/roomcrypto/...`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/roomcrypto/integration_test.go pkg/roomcrypto/testdata/
git commit -m "test(roomcrypto): update integration test for HKDF-only scheme"
```

---

## Phase 2 — `room-service` keys-bootstrap RPC

### Task 10: Add subject builders for the keys-bootstrap RPC

**Files:**
- Modify: `pkg/subject/subject.go`
- Modify: `pkg/subject/subject_test.go`

- [ ] **Step 1: Add the failing test**

In `pkg/subject/subject_test.go`:

```go
func TestRoomsKeysBootstrap(t *testing.T) {
	assert.Equal(t, "chat.user.alice.request.rooms.keys", RoomsKeysBootstrap("alice"))
}

func TestRoomsKeysBootstrapWildcard(t *testing.T) {
	assert.Equal(t, "chat.user.*.request.rooms.keys", RoomsKeysBootstrapWildcard())
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./pkg/subject/ -run "TestRoomsKeysBootstrap" -v`
Expected: FAIL — undefined functions.

- [ ] **Step 3: Add the subject builders to `pkg/subject/subject.go`**

Append to the user-scoped subject section (near `RoomsList`):

```go
// RoomsKeysBootstrap is the per-user request subject for fetching all
// room private keys the user is currently subscribed to. Used by clients
// on (re)connect to bootstrap their key cache.
func RoomsKeysBootstrap(account string) string {
	return fmt.Sprintf("chat.user.%s.request.rooms.keys", account)
}

// RoomsKeysBootstrapWildcard is the subscribe pattern used by room-service.
func RoomsKeysBootstrapWildcard() string {
	return "chat.user.*.request.rooms.keys"
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./pkg/subject/ -run "TestRoomsKeysBootstrap" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "feat(subject): add RoomsKeysBootstrap subject builders"
```

---

### Task 11: Add `RoomsKeysResponse` model types

**Files:**
- Modify: `pkg/model/event.go` (or a more appropriate model file — search existing patterns)
- Modify: `pkg/model/model_test.go`

- [ ] **Step 1: Add the failing round-trip test**

In `pkg/model/model_test.go` (existing roundTrip helper):

```go
func TestRoomsKeysResponse_RoundTrip(t *testing.T) {
	roundTrip(t, RoomsKeysResponse{
		Keys: []RoomsKeysEntry{
			{RoomID: "r1", Version: 7, PrivateKey: []byte{1, 2, 3}},
			{RoomID: "r2", Version: 0, PrivateKey: []byte{4, 5, 6}},
		},
	})
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test ./pkg/model/ -run TestRoomsKeysResponse_RoundTrip -v`
Expected: FAIL — undefined types.

- [ ] **Step 3: Add the types**

In an appropriate model file (likely `pkg/model/event.go` near `RoomKeyEvent`):

```go
// RoomsKeysEntry is one entry in the RoomsKeysResponse — a single (roomId,
// version, privateKey) tuple for a room the caller is subscribed to.
type RoomsKeysEntry struct {
	RoomID     string `json:"roomId"     bson:"roomId"`
	Version    int    `json:"version"    bson:"version"`
	PrivateKey []byte `json:"privateKey" bson:"privateKey"`
}

// RoomsKeysResponse is the response to RoomsKeysBootstrap — the full
// snapshot of (roomId, version, privateKey) tuples for every room the
// caller is currently subscribed to that has a key in Valkey.
type RoomsKeysResponse struct {
	Keys []RoomsKeysEntry `json:"keys" bson:"keys"`
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./pkg/model/ -run TestRoomsKeysResponse_RoundTrip -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/model/event.go pkg/model/model_test.go
git commit -m "feat(model): add RoomsKeysResponse and RoomsKeysEntry types"
```

---

### Task 12: Implement `room-service` handler `natsListRoomKeys`

**Files:**
- Modify: `room-service/handler.go`
- Modify: `room-service/handler_test.go`
- Modify: `room-service/store.go` (if a new store method is needed)

- [ ] **Step 1: Inspect existing patterns**

Run: `grep -n "natsListRooms\|natsRoomsInfoBatch\|SubscriptionStore\|ListSubscriptions" room-service/handler.go room-service/store.go | head -30`

Identify how room-service today reads the caller's subscriptions (look for the existing `natsListRooms` handler).

- [ ] **Step 2: Add the failing handler test**

In `room-service/handler_test.go`, add a test for the new handler. Follow the exact pattern of the existing `natsListRooms` test (look for it and mirror the mock setup, request construction, and assertion style).

Sketch:

```go
func TestHandler_NatsListRoomKeys_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	h := newTestHandler(t, store, keyStore)

	account := "alice"
	subs := []model.Subscription{
		{RoomID: "r1", IsSubscribed: ptrBool(true)},
		{RoomID: "r2", IsSubscribed: ptrBool(true)},
	}
	store.EXPECT().
		ListSubscriptionsByAccount(gomock.Any(), account).
		Return(subs, nil)
	keyStore.EXPECT().
		GetMany(gomock.Any(), []string{"r1", "r2"}).
		Return(map[string]*roomkeystore.VersionedKeyPair{
			"r1": {Version: 7, KeyPair: roomkeystore.RoomKeyPair{PrivateKey: []byte{1, 2}}},
			"r2": {Version: 0, KeyPair: roomkeystore.RoomKeyPair{PrivateKey: []byte{3, 4}}},
		}, nil)

	respBytes, err := h.handleListRoomKeys(t.Context(), account)
	require.NoError(t, err)

	var resp model.RoomsKeysResponse
	require.NoError(t, json.Unmarshal(respBytes, &resp))
	assert.Len(t, resp.Keys, 2)
	keysByRoom := map[string]model.RoomsKeysEntry{}
	for _, k := range resp.Keys {
		keysByRoom[k.RoomID] = k
	}
	assert.Equal(t, 7, keysByRoom["r1"].Version)
	assert.Equal(t, []byte{1, 2}, keysByRoom["r1"].PrivateKey)
}
```

If `ListSubscriptionsByAccount` doesn't exist on the store, the closest existing method should be used. Search:

`grep -n "ListSubscriptions\|FindSubscriptions" room-service/store.go`

If a suitable method exists, use it. Otherwise this task includes adding it — see Step 3a below.

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./room-service/ -run TestHandler_NatsListRoomKeys -v`
Expected: FAIL — `undefined: handleListRoomKeys` (or unmapped expectations).

- [ ] **Step 3a (only if no suitable store method exists): Add the store method**

Add to `room-service/store.go`:

```go
type Store interface {
	// ... existing methods ...
	ListSubscriptionsByAccount(ctx context.Context, account string) ([]model.Subscription, error)
}
```

Implement in `room-service/store_mongo.go`:

```go
func (m *mongoStore) ListSubscriptionsByAccount(ctx context.Context, account string) ([]model.Subscription, error) {
	cur, err := m.subscriptionsCol.Find(ctx, bson.M{"u.account": account, "isSubscribed": true})
	if err != nil {
		return nil, fmt.Errorf("find subscriptions for account %s: %w", account, err)
	}
	defer cur.Close(ctx)
	var out []model.Subscription
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	return out, nil
}
```

Then run `make generate SERVICE=room-service` to regenerate `mock_store_test.go`.

- [ ] **Step 4: Implement `handleListRoomKeys` and `natsListRoomKeys` in `room-service/handler.go`**

```go
func (h *Handler) natsListRoomKeys(m otelnats.Msg) {
	ctx := wrappedCtx(m)
	// Subject pattern: chat.user.{account}.request.rooms.keys → account at index 2.
	// Mirrors the parts-split pattern used by natsGetRoom in this file.
	parts := strings.Split(m.Msg.Subject, ".")
	if len(parts) < 6 || parts[0] != "chat" || parts[1] != "user" {
		natsutil.ReplyError(m.Msg, fmt.Errorf("invalid subject: %s", m.Msg.Subject))
		return
	}
	account := parts[2]
	resp, err := h.handleListRoomKeys(ctx, account)
	if err != nil {
		slog.Error("list room keys failed", "error", err, "account", account)
		natsutil.ReplyError(m.Msg, sanitizeError(err))
		return
	}
	if err := m.Msg.Respond(resp); err != nil {
		slog.Error("failed to respond to message", "error", err)
	}
}

func (h *Handler) handleListRoomKeys(ctx context.Context, account string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	subs, err := h.store.ListSubscriptionsByAccount(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions for %s: %w", account, err)
	}
	if len(subs) == 0 {
		return json.Marshal(model.RoomsKeysResponse{Keys: []model.RoomsKeysEntry{}})
	}

	roomIDs := make([]string, 0, len(subs))
	for i := range subs {
		roomIDs = append(roomIDs, subs[i].RoomID)
	}

	keys, err := chunkedGetKeys(ctx, h.keyStore, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("get room keys for %s: %w", account, err)
	}

	out := make([]model.RoomsKeysEntry, 0, len(keys))
	for roomID, kp := range keys {
		if kp == nil {
			continue
		}
		out = append(out, model.RoomsKeysEntry{
			RoomID:     roomID,
			Version:    kp.Version,
			PrivateKey: kp.KeyPair.PrivateKey,
		})
	}
	return json.Marshal(model.RoomsKeysResponse{Keys: out})
}
```

(The `strings.Split` pattern shown is the convention used by `natsGetRoom` in the same file. There is no shared `subject.AccountFromSubject` helper today.)

- [ ] **Step 5: Register the handler in `RegisterCRUD`**

Add a new line in `RegisterCRUD`:

```go
if _, err := nc.QueueSubscribe(subject.RoomsKeysBootstrapWildcard(), queue, h.natsListRoomKeys); err != nil {
	return fmt.Errorf("subscribe rooms keys bootstrap: %w", err)
}
```

- [ ] **Step 6: Run the handler test**

Run: `make test SERVICE=room-service`
Expected: All tests pass.

- [ ] **Step 7: Run the full build**

Run: `go build ./...`
Expected: Build succeeds.

- [ ] **Step 8: Commit**

```bash
git add room-service/
git commit -m "feat(room-service): add chat.user.{account}.request.rooms.keys RPC"
```

---

### Task 13: Integration test for the new RPC (optional but recommended)

**Files:**
- Modify: `room-service/integration_test.go`

- [ ] **Step 1: Add an integration test that exercises the full RPC**

Pattern-match against the existing `RoomsInfoBatch` integration test in the same file. The new test should:

1. Insert two subscriptions for account `alice` into Mongo via the real store
2. Insert two room keys into Valkey via the real keystore
3. Issue a `nc.Request(subject.RoomsKeysBootstrap("alice"), nil, 2*time.Second)`
4. Decode `model.RoomsKeysResponse`
5. Assert both rooms appear with their keys

- [ ] **Step 2: Run the integration test**

Run: `make test-integration SERVICE=room-service`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add room-service/integration_test.go
git commit -m "test(room-service): integration test for rooms keys bootstrap RPC"
```

---

## Phase 3 — chat-frontend crypto module

### Task 14: Add `lib/roomcrypto` module — `b64decode` helper

**Files:**
- Create: `chat-frontend/src/lib/roomcrypto/roomcrypto.ts`
- Create: `chat-frontend/src/lib/roomcrypto/roomcrypto.test.ts`
- Create: `chat-frontend/src/lib/roomcrypto/index.ts`

- [ ] **Step 1: Write the failing test**

`chat-frontend/src/lib/roomcrypto/roomcrypto.test.ts`:

```ts
import { describe, it, expect } from 'vitest'
import { b64decode } from './roomcrypto'

describe('b64decode', () => {
  it('decodes a known base64 string', () => {
    expect(Array.from(b64decode('aGVsbG8='))).toEqual([104, 101, 108, 108, 111])
  })

  it('decodes an empty string to an empty Uint8Array', () => {
    expect(b64decode('').length).toBe(0)
  })

  it('round-trips with btoa', () => {
    const original = new Uint8Array([1, 2, 3, 250])
    const encoded = btoa(String.fromCharCode(...original))
    expect(Array.from(b64decode(encoded))).toEqual([1, 2, 3, 250])
  })
})
```

- [ ] **Step 2: Run the test to confirm failure**

Run: `cd chat-frontend && npx vitest run src/lib/roomcrypto/`
Expected: FAIL — module not found.

- [ ] **Step 3: Create the module**

`chat-frontend/src/lib/roomcrypto/roomcrypto.ts`:

```ts
/**
 * Decode a standard-base64 string to a Uint8Array.
 *
 * Note: this is base64, not base64url. The server emits standard base64
 * via Go's encoding/json default for []byte fields (StdEncoding).
 */
export function b64decode(s: string): Uint8Array {
  const binary = atob(s)
  const out = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i)
  return out
}
```

`chat-frontend/src/lib/roomcrypto/index.ts`:

```ts
export { b64decode, deriveAesKey, decryptRoomMessage } from './roomcrypto'
```

(The `deriveAesKey` and `decryptRoomMessage` exports won't exist yet — TypeScript will complain. That's fine; Task 15 adds them.)

- [ ] **Step 4: Run the test**

Run: `cd chat-frontend && npx vitest run src/lib/roomcrypto/`
Expected: `b64decode` tests pass. Index file may have a TS error referencing undefined names — that's expected; Task 15 fixes it.

For now, temporarily simplify `index.ts` to only export what exists:

```ts
export { b64decode } from './roomcrypto'
```

We'll expand it as we add functions.

- [ ] **Step 5: Commit**

```bash
git add chat-frontend/src/lib/roomcrypto/
git commit -m "feat(chat-frontend): scaffold lib/roomcrypto with b64decode helper"
```

---

### Task 15: Add `deriveAesKey` using Web Crypto API

**Files:**
- Modify: `chat-frontend/src/lib/roomcrypto/roomcrypto.ts`
- Modify: `chat-frontend/src/lib/roomcrypto/roomcrypto.test.ts`
- Modify: `chat-frontend/src/lib/roomcrypto/index.ts`

- [ ] **Step 1: Add the failing test**

```ts
import { deriveAesKey } from './roomcrypto'

describe('deriveAesKey', () => {
  it('returns a non-extractable AES-GCM CryptoKey usable for decrypt', async () => {
    const priv = new Uint8Array(32)
    priv.fill(0x42)
    const key = await deriveAesKey(priv)
    expect(key.type).toBe('secret')
    expect(key.algorithm).toMatchObject({ name: 'AES-GCM', length: 256 })
    expect(key.usages).toEqual(['decrypt'])
    expect(key.extractable).toBe(false)
  })

  it('produces a deterministic key for the same input', async () => {
    // Cross-check: derive twice, exporting once via deriveBits with the same
    // algorithm. We can't export an AES-GCM key directly when extractable=false,
    // so instead test by encrypting+decrypting and verifying round-trip.
    const priv = new Uint8Array(32)
    priv.fill(0x07)
    const k1 = await deriveAesKey(priv)
    const k2 = await deriveAesKey(priv)
    const nonce = new Uint8Array(12)
    crypto.getRandomValues(nonce)
    const plaintext = new TextEncoder().encode('hello')
    // Derive a separate encryption-capable key from the same IKM for the test.
    const encKey = await crypto.subtle.deriveKey(
      { name: 'HKDF', hash: 'SHA-256', salt: new Uint8Array(0), info: new TextEncoder().encode('room-message-encryption-v2') },
      await crypto.subtle.importKey('raw', priv, 'HKDF', false, ['deriveKey']),
      { name: 'AES-GCM', length: 256 },
      false,
      ['encrypt'],
    )
    const ct = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce, tagLength: 128 }, encKey, plaintext))
    // Decrypt with k1 and k2 — both must succeed.
    const pt1 = new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: nonce, tagLength: 128 }, k1, ct))
    const pt2 = new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: nonce, tagLength: 128 }, k2, ct))
    expect(new TextDecoder().decode(pt1)).toBe('hello')
    expect(new TextDecoder().decode(pt2)).toBe('hello')
  })

  it('rejects a private key of wrong length', async () => {
    await expect(deriveAesKey(new Uint8Array(31))).rejects.toThrow(/32 bytes/)
  })
})
```

- [ ] **Step 2: Run the test to confirm failure**

Run: `cd chat-frontend && npx vitest run src/lib/roomcrypto/`
Expected: FAIL — `deriveAesKey is not defined`.

- [ ] **Step 3: Implement `deriveAesKey`**

Append to `chat-frontend/src/lib/roomcrypto/roomcrypto.ts`:

```ts
const HKDF_INFO = new TextEncoder().encode('room-message-encryption-v2')
const HKDF_SALT = new Uint8Array(0)

/**
 * Derive an AES-256-GCM CryptoKey from a 32-byte room private key via
 * HKDF-SHA-256 with empty salt and info "room-message-encryption-v2".
 *
 * The returned key is non-extractable and has the single usage 'decrypt'.
 */
export async function deriveAesKey(roomPrivateKey: Uint8Array): Promise<CryptoKey> {
  if (roomPrivateKey.length !== 32) {
    throw new Error(`room private key must be 32 bytes, got ${roomPrivateKey.length}`)
  }
  const ikm = await crypto.subtle.importKey('raw', roomPrivateKey, 'HKDF', false, ['deriveKey'])
  return crypto.subtle.deriveKey(
    { name: 'HKDF', hash: 'SHA-256', salt: HKDF_SALT, info: HKDF_INFO },
    ikm,
    { name: 'AES-GCM', length: 256 },
    false,
    ['decrypt'],
  )
}
```

Update `chat-frontend/src/lib/roomcrypto/index.ts`:

```ts
export { b64decode, deriveAesKey } from './roomcrypto'
```

- [ ] **Step 4: Run the tests**

Run: `cd chat-frontend && npx vitest run src/lib/roomcrypto/`
Expected: All three `deriveAesKey` tests pass.

- [ ] **Step 5: Commit**

```bash
git add chat-frontend/src/lib/roomcrypto/
git commit -m "feat(chat-frontend): add deriveAesKey via Web Crypto HKDF-SHA-256"
```

---

### Task 16: Add `decryptRoomMessage`

**Files:**
- Modify: `chat-frontend/src/lib/roomcrypto/roomcrypto.ts`
- Modify: `chat-frontend/src/lib/roomcrypto/roomcrypto.test.ts`
- Modify: `chat-frontend/src/lib/roomcrypto/index.ts`

- [ ] **Step 1: Add the failing tests**

```ts
import { decryptRoomMessage } from './roomcrypto'

describe('decryptRoomMessage', () => {
  it('decrypts ciphertext produced via the matching encrypt path', async () => {
    const priv = new Uint8Array(32)
    priv.fill(0x99)
    const aesKey = await deriveAesKey(priv)

    // Build a matching encrypt key (extractable=false but with encrypt usage).
    const encKey = await crypto.subtle.deriveKey(
      { name: 'HKDF', hash: 'SHA-256', salt: new Uint8Array(0), info: new TextEncoder().encode('room-message-encryption-v2') },
      await crypto.subtle.importKey('raw', priv, 'HKDF', false, ['deriveKey']),
      { name: 'AES-GCM', length: 256 },
      false,
      ['encrypt'],
    )

    const nonce = new Uint8Array(12)
    crypto.getRandomValues(nonce)
    const plaintext = new TextEncoder().encode('héllo 🌎')
    const ciphertext = new Uint8Array(
      await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce, tagLength: 128 }, encKey, plaintext),
    )

    const got = await decryptRoomMessage(ciphertext, nonce, aesKey)
    expect(got).toBe('héllo 🌎')
  })

  it('throws on tag mismatch', async () => {
    const priv = new Uint8Array(32)
    priv.fill(0x11)
    const aesKey = await deriveAesKey(priv)
    const nonce = new Uint8Array(12)
    const bogusCiphertext = new Uint8Array(32) // all-zero bytes, will fail tag verification
    await expect(decryptRoomMessage(bogusCiphertext, nonce, aesKey)).rejects.toBeDefined()
  })
})
```

- [ ] **Step 2: Run the tests to confirm failure**

Run: `cd chat-frontend && npx vitest run src/lib/roomcrypto/`
Expected: FAIL — `decryptRoomMessage is not defined`.

- [ ] **Step 3: Implement `decryptRoomMessage`**

Append to `chat-frontend/src/lib/roomcrypto/roomcrypto.ts`:

```ts
/**
 * Decrypt a server-produced {nonce, ciphertext} pair using the AES key
 * derived via deriveAesKey. The ciphertext is body || 16-byte GCM tag,
 * matching Go's cipher.AEAD.Seal output.
 */
export async function decryptRoomMessage(
  ciphertext: Uint8Array,
  nonce: Uint8Array,
  aesKey: CryptoKey,
): Promise<string> {
  if (nonce.length !== 12) {
    throw new Error(`nonce must be 12 bytes, got ${nonce.length}`)
  }
  const plaintext = await crypto.subtle.decrypt(
    { name: 'AES-GCM', iv: nonce, tagLength: 128 },
    aesKey,
    ciphertext,
  )
  return new TextDecoder('utf-8').decode(plaintext)
}
```

Update `chat-frontend/src/lib/roomcrypto/index.ts`:

```ts
export { b64decode, deriveAesKey, decryptRoomMessage } from './roomcrypto'
```

- [ ] **Step 4: Run the tests**

Run: `cd chat-frontend && npx vitest run src/lib/roomcrypto/`
Expected: All tests pass.

- [ ] **Step 5: Commit**

```bash
git add chat-frontend/src/lib/roomcrypto/
git commit -m "feat(chat-frontend): add decryptRoomMessage via Web Crypto AES-GCM"
```

---

### Task 17: Cross-language fixture — Go encode → TS decode

**Files:**
- Create: `chat-frontend/scripts/gen-crypto-fixtures.go`
- Create: `chat-frontend/test/fixtures/encrypted-message.json`
- Modify: `chat-frontend/src/lib/roomcrypto/roomcrypto.test.ts`

- [ ] **Step 1: Write the failing test**

Append to `chat-frontend/src/lib/roomcrypto/roomcrypto.test.ts`:

```ts
import fixture from '../../../test/fixtures/encrypted-message.json'

describe('cross-language fixture', () => {
  it('decrypts a fixture produced by the Go server encoder', async () => {
    const priv = b64decode(fixture.privateKey)
    const nonce = b64decode(fixture.message.nonce)
    const ciphertext = b64decode(fixture.message.ciphertext)

    const aesKey = await deriveAesKey(priv)
    const plaintext = await decryptRoomMessage(ciphertext, nonce, aesKey)
    expect(plaintext).toBe(fixture.plaintext)
  })
})
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `cd chat-frontend && npx vitest run src/lib/roomcrypto/`
Expected: FAIL — fixture file not found.

- [ ] **Step 3: Create the Go fixture-generator program**

`chat-frontend/scripts/gen-crypto-fixtures.go`:

```go
//go:build ignore

// gen-crypto-fixtures generates a known-plaintext encrypted message for
// chat-frontend's lib/roomcrypto round-trip tests. Run with:
//
//   go run chat-frontend/scripts/gen-crypto-fixtures.go > chat-frontend/test/fixtures/encrypted-message.json
//
// Commit the output. The fixture exists to lock the cross-language wire
// format: any change in the server encoder's HKDF parameters or wire
// shape MUST update this fixture together with the corresponding TS
// decoder.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/hmchangw/chat/pkg/roomcrypto"
)

func main() {
	// Deterministic private key so the fixture is stable across runs.
	priv := make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i + 1)
	}
	const plaintext = "fixture plaintext — encoded by the Go server, decoded by chat-frontend"
	const version = 1
	const roomID = "fixture-room"

	// Construct an Encoder with a fixed-nonce reader so the ciphertext is
	// reproducible. Twelve-byte zero nonce is fine for a fixture (it's a
	// known-key test, not real traffic).
	enc := roomcrypto.NewEncoder(roomcrypto.WithRand(zeroReader{}))
	msg, err := enc.Encode(roomID, plaintext, priv, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
		os.Exit(1)
	}

	out := struct {
		PrivateKey string                      `json:"privateKey"`
		Plaintext  string                      `json:"plaintext"`
		Message    *roomcrypto.EncryptedMessage `json:"message"`
	}{
		PrivateKey: base64.StdEncoding.EncodeToString(priv),
		Plaintext:  plaintext,
		Message:    msg,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
```

- [ ] **Step 4: Run the generator**

Run: `go run chat-frontend/scripts/gen-crypto-fixtures.go > chat-frontend/test/fixtures/encrypted-message.json`

Verify the output file exists and contains base64-encoded fields.

- [ ] **Step 5: Configure Vitest to allow JSON imports if needed**

If the test fails with "cannot find module" for the JSON import, check `chat-frontend/tsconfig.json` for `resolveJsonModule: true`. Add it if absent.

- [ ] **Step 6: Run the test**

Run: `cd chat-frontend && npx vitest run src/lib/roomcrypto/`
Expected: All tests pass, including the new cross-language one.

- [ ] **Step 7: Commit**

```bash
git add chat-frontend/scripts/gen-crypto-fixtures.go chat-frontend/test/fixtures/ chat-frontend/src/lib/roomcrypto/roomcrypto.test.ts
git commit -m "test(chat-frontend): cross-language fixture for Go encode → TS decode"
```

---

## Phase 4 — chat-frontend API ops

### Task 18: Add subject builders for room-key subjects

**Files:**
- Modify: `chat-frontend/src/api/_transport/subjects.ts`
- Modify: `chat-frontend/src/api/_transport/subjects.test.js`

- [ ] **Step 1: Add the failing tests**

In `subjects.test.js`:

```js
import { userRoomKey, roomsKeysBootstrap } from './subjects'

describe('userRoomKey', () => {
  it('builds the per-user room-key event subject', () => {
    expect(userRoomKey('alice')).toBe('chat.user.alice.event.room.key')
  })
})

describe('roomsKeysBootstrap', () => {
  it('builds the per-user keys-bootstrap RPC subject', () => {
    expect(roomsKeysBootstrap('alice')).toBe('chat.user.alice.request.rooms.keys')
  })
})
```

- [ ] **Step 2: Run the tests to verify failure**

Run: `cd chat-frontend && npx vitest run src/api/_transport/subjects.test.js`
Expected: FAIL — undefined exports.

- [ ] **Step 3: Add the builders to `subjects.ts`**

Append:

```ts
export function userRoomKey(account: string): string {
  return `chat.user.${account}.event.room.key`
}

export function roomsKeysBootstrap(account: string): string {
  return `chat.user.${account}.request.rooms.keys`
}
```

- [ ] **Step 4: Run the tests**

Run: `cd chat-frontend && npx vitest run src/api/_transport/subjects.test.js`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add chat-frontend/src/api/_transport/subjects.ts chat-frontend/src/api/_transport/subjects.test.js
git commit -m "feat(chat-frontend/api): subject builders for room-key events and bootstrap RPC"
```

---

### Task 19: Add wire types `RoomKeyEvent`, `RoomKeysEntry`, `RoomKeysResponse`

**Files:**
- Modify: `chat-frontend/src/api/types.ts`

- [ ] **Step 1: Append to `api/types.ts`**

```ts
/**
 * Mirrors pkg/model.RoomKeyEvent — payload of
 * chat.user.{account}.event.room.key. PrivateKey is base64-encoded on
 * the wire (Go's encoding/json default for []byte). PublicKey is
 * omitted from the client wire payload.
 */
export interface RoomKeyEvent {
  roomId: string
  version: number
  privateKey: string  // base64
  timestamp: number
}

/** One entry in RoomKeysResponse — see pkg/model.RoomsKeysEntry. */
export interface RoomKeysEntry {
  roomId: string
  version: number
  privateKey: string  // base64
}

/** Response to roomsKeysBootstrap RPC — see pkg/model.RoomsKeysResponse. */
export interface RoomKeysResponse {
  keys: RoomKeysEntry[]
}
```

- [ ] **Step 2: Re-export from the barrel**

`chat-frontend/src/api/index.ts`: add `RoomKeyEvent`, `RoomKeysEntry`, `RoomKeysResponse` to the type re-exports.

- [ ] **Step 3: Typecheck**

Run: `cd chat-frontend && npm run typecheck`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add chat-frontend/src/api/types.ts chat-frontend/src/api/index.ts
git commit -m "feat(chat-frontend/api): add RoomKeyEvent + RoomKeys{Entry,Response} types"
```

---

### Task 20: New API op — `subscribeToRoomKeyEvents`

**Files:**
- Create: `chat-frontend/src/api/subscribeToRoomKeyEvents/index.ts`
- Create: `chat-frontend/src/api/subscribeToRoomKeyEvents/index.test.ts`
- Modify: `chat-frontend/src/api/index.ts`

- [ ] **Step 1: Write the failing test**

`chat-frontend/src/api/subscribeToRoomKeyEvents/index.test.ts`:

```ts
import { describe, it, expect, vi } from 'vitest'
import { subscribeToRoomKeyEvents } from './index'

describe('subscribeToRoomKeyEvents', () => {
  it('subscribes to chat.user.{account}.event.room.key with the given callback', () => {
    const subscribe = vi.fn().mockReturnValue({ unsubscribe: vi.fn() })
    const nats = { subscribe, user: { account: 'alice', id: '' }, request: vi.fn(), publish: vi.fn(), requestWithAsyncResult: vi.fn(), connected: true, error: null }
    const cb = vi.fn()

    const sub = subscribeToRoomKeyEvents(nats as never, cb)

    expect(subscribe).toHaveBeenCalledWith('chat.user.alice.event.room.key', cb)
    expect(sub).toBeDefined()
  })
})
```

- [ ] **Step 2: Run the test**

Run: `cd chat-frontend && npx vitest run src/api/subscribeToRoomKeyEvents/`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the op**

`chat-frontend/src/api/subscribeToRoomKeyEvents/index.ts`:

```ts
import { userRoomKey } from '../_transport/subjects'
import type { Nats, NatsSubscription, SubscriptionCallback } from '../types'

/** Subscribe to the calling user's room-key event stream. */
export function subscribeToRoomKeyEvents(
  { subscribe, user }: Pick<Nats, 'subscribe' | 'user'>,
  callback: SubscriptionCallback,
): NatsSubscription {
  return subscribe(userRoomKey(user.account), callback)
}
```

Add to `chat-frontend/src/api/index.ts`:

```ts
export { subscribeToRoomKeyEvents } from './subscribeToRoomKeyEvents'
```

- [ ] **Step 4: Run the test**

Run: `cd chat-frontend && npx vitest run src/api/subscribeToRoomKeyEvents/`
Expected: PASS.

- [ ] **Step 5: Typecheck**

Run: `cd chat-frontend && npm run typecheck`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add chat-frontend/src/api/subscribeToRoomKeyEvents/ chat-frontend/src/api/index.ts
git commit -m "feat(chat-frontend/api): subscribeToRoomKeyEvents"
```

---

### Task 21: New API op — `fetchRoomKeysBootstrap`

**Files:**
- Create: `chat-frontend/src/api/fetchRoomKeysBootstrap/index.ts`
- Create: `chat-frontend/src/api/fetchRoomKeysBootstrap/index.test.ts`
- Modify: `chat-frontend/src/api/index.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect, vi } from 'vitest'
import { fetchRoomKeysBootstrap } from './index'

describe('fetchRoomKeysBootstrap', () => {
  it('issues a request on chat.user.{account}.request.rooms.keys with an empty payload and returns the parsed RoomKeysResponse', async () => {
    const request = vi.fn().mockResolvedValue({ keys: [{ roomId: 'r1', version: 7, privateKey: 'AAAA' }] })
    const nats = { request, user: { account: 'alice', id: '' }, subscribe: vi.fn(), publish: vi.fn(), requestWithAsyncResult: vi.fn(), connected: true, error: null }

    const got = await fetchRoomKeysBootstrap(nats as never)

    expect(request).toHaveBeenCalledWith('chat.user.alice.request.rooms.keys', {})
    expect(got).toEqual({ keys: [{ roomId: 'r1', version: 7, privateKey: 'AAAA' }] })
  })
})
```

- [ ] **Step 2: Run the test**

Run: `cd chat-frontend && npx vitest run src/api/fetchRoomKeysBootstrap/`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the op**

```ts
// chat-frontend/src/api/fetchRoomKeysBootstrap/index.ts
import { roomsKeysBootstrap } from '../_transport/subjects'
import type { Nats, RoomKeysResponse } from '../types'

/** Fetch the full snapshot of (roomId, version, privateKey) for every
 *  room the caller is subscribed to. Call once on (re)connect. */
export function fetchRoomKeysBootstrap(
  { request, user }: Pick<Nats, 'request' | 'user'>,
): Promise<RoomKeysResponse> {
  return request<RoomKeysResponse>(roomsKeysBootstrap(user.account), {})
}
```

Add to `chat-frontend/src/api/index.ts`:

```ts
export { fetchRoomKeysBootstrap } from './fetchRoomKeysBootstrap'
```

- [ ] **Step 4: Run the test and typecheck**

Run: `cd chat-frontend && npx vitest run src/api/fetchRoomKeysBootstrap/ && npm run typecheck`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add chat-frontend/src/api/fetchRoomKeysBootstrap/ chat-frontend/src/api/index.ts
git commit -m "feat(chat-frontend/api): fetchRoomKeysBootstrap RPC wrapper"
```

---

## Phase 5 — chat-frontend `RoomKeysContext`

### Task 22: `RoomKeysContext` reducer

**Files:**
- Create: `chat-frontend/src/context/RoomKeysContext/reducer.ts`
- Create: `chat-frontend/src/context/RoomKeysContext/reducer.test.ts`

- [ ] **Step 1: Write the failing tests**

`chat-frontend/src/context/RoomKeysContext/reducer.test.ts`:

```ts
import { describe, it, expect } from 'vitest'
import { roomKeysReducer, initialRoomKeysState, type RoomKeysState } from './reducer'

const seed = (): RoomKeysState => ({ ...initialRoomKeysState, byRoom: { ...initialRoomKeysState.byRoom } })

describe('roomKeysReducer', () => {
  it('BOOTSTRAP_LOADED merges all entries by (roomId, version)', () => {
    const next = roomKeysReducer(seed(), {
      type: 'BOOTSTRAP_LOADED',
      keys: [
        { roomId: 'r1', version: 1, privateKey: new Uint8Array([1, 2, 3]) },
        { roomId: 'r2', version: 5, privateKey: new Uint8Array([9]) },
      ],
    })
    expect(next.bootstrapped).toBe(true)
    expect(next.byRoom.r1[1].privateKey).toEqual(new Uint8Array([1, 2, 3]))
    expect(next.byRoom.r2[5].privateKey).toEqual(new Uint8Array([9]))
  })

  it('KEY_RECEIVED inserts a single (roomId, version) entry', () => {
    const next = roomKeysReducer(seed(), {
      type: 'KEY_RECEIVED',
      roomId: 'r1',
      version: 2,
      privateKey: new Uint8Array([7]),
    })
    expect(next.byRoom.r1[2].privateKey).toEqual(new Uint8Array([7]))
  })

  it('KEY_RECEIVED is idempotent — same input does not duplicate', () => {
    const action = {
      type: 'KEY_RECEIVED' as const,
      roomId: 'r1',
      version: 2,
      privateKey: new Uint8Array([7]),
    }
    const once = roomKeysReducer(seed(), action)
    const twice = roomKeysReducer(once, action)
    expect(Object.keys(twice.byRoom.r1)).toEqual(['2'])
  })

  it('KEY_RECEIVED keeps at most MAX_VERSIONS_PER_ROOM newest versions', () => {
    let s = seed()
    // Insert versions 1..5 — MAX_VERSIONS_PER_ROOM is 2, so only 4 and 5 should remain.
    for (let v = 1; v <= 5; v++) {
      s = roomKeysReducer(s, {
        type: 'KEY_RECEIVED',
        roomId: 'r1',
        version: v,
        privateKey: new Uint8Array([v]),
      })
    }
    const versions = Object.keys(s.byRoom.r1).map(Number).sort((a, b) => a - b)
    expect(versions).toEqual([4, 5])
  })

  it('CLEAR_KEYS resets state', () => {
    const populated: RoomKeysState = {
      bootstrapped: true,
      byRoom: { r1: { 1: { privateKey: new Uint8Array([1]) } } },
    }
    const next = roomKeysReducer(populated, { type: 'CLEAR_KEYS' })
    expect(next).toEqual(initialRoomKeysState)
  })
})
```

- [ ] **Step 2: Run the tests**

Run: `cd chat-frontend && npx vitest run src/context/RoomKeysContext/`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the reducer**

`chat-frontend/src/context/RoomKeysContext/reducer.ts`:

```ts
/** Maximum stored versions per room. Matches Valkey's previous-key grace
 *  slot (one previous in addition to current). */
export const MAX_VERSIONS_PER_ROOM = 2

export type StoredKey = {
  privateKey: Uint8Array
}

export type RoomKeysState = {
  byRoom: Record<string, Record<number, StoredKey>>
  bootstrapped: boolean
}

export const initialRoomKeysState: RoomKeysState = {
  byRoom: {},
  bootstrapped: false,
}

export type RoomKeysAction =
  | {
      type: 'BOOTSTRAP_LOADED'
      keys: Array<{ roomId: string; version: number; privateKey: Uint8Array }>
    }
  | {
      type: 'KEY_RECEIVED'
      roomId: string
      version: number
      privateKey: Uint8Array
    }
  | { type: 'CLEAR_KEYS' }

export function roomKeysReducer(state: RoomKeysState, action: RoomKeysAction): RoomKeysState {
  switch (action.type) {
    case 'BOOTSTRAP_LOADED': {
      const byRoom: Record<string, Record<number, StoredKey>> = { ...state.byRoom }
      for (const k of action.keys) {
        const room = { ...(byRoom[k.roomId] ?? {}) }
        room[k.version] = { privateKey: k.privateKey }
        byRoom[k.roomId] = trimVersions(room)
      }
      return { ...state, byRoom, bootstrapped: true }
    }
    case 'KEY_RECEIVED': {
      const existing = state.byRoom[action.roomId]?.[action.version]
      if (existing && bytesEqual(existing.privateKey, action.privateKey)) {
        return state // idempotent no-op
      }
      const room = { ...(state.byRoom[action.roomId] ?? {}) }
      room[action.version] = { privateKey: action.privateKey }
      return {
        ...state,
        byRoom: { ...state.byRoom, [action.roomId]: trimVersions(room) },
      }
    }
    case 'CLEAR_KEYS':
      return initialRoomKeysState
    default:
      return state
  }
}

function trimVersions(room: Record<number, StoredKey>): Record<number, StoredKey> {
  const versions = Object.keys(room).map(Number).sort((a, b) => b - a)
  if (versions.length <= MAX_VERSIONS_PER_ROOM) return room
  const out: Record<number, StoredKey> = {}
  for (const v of versions.slice(0, MAX_VERSIONS_PER_ROOM)) {
    out[v] = room[v]
  }
  return out
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}
```

- [ ] **Step 4: Run the tests**

Run: `cd chat-frontend && npx vitest run src/context/RoomKeysContext/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add chat-frontend/src/context/RoomKeysContext/
git commit -m "feat(chat-frontend): RoomKeysContext reducer with idempotent key insert"
```

---

### Task 23: `RoomKeysContext` provider + `useRoomKeys`

**Files:**
- Create: `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.tsx`
- Create: `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.test.tsx`
- Create: `chat-frontend/src/context/RoomKeysContext/index.tsx`

- [ ] **Step 1: Write the failing test**

`chat-frontend/src/context/RoomKeysContext/RoomKeysContext.test.tsx`:

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, waitFor, act } from '@testing-library/react'
import { RoomKeysProvider, useRoomKeys } from './RoomKeysContext'

vi.mock('@/api', () => ({
  fetchRoomKeysBootstrap: vi.fn(),
  subscribeToRoomKeyEvents: vi.fn(),
}))

vi.mock('@/context/NatsContext', () => ({
  useNats: () => ({
    user: { account: 'alice', id: 'u1' },
    request: vi.fn(),
    subscribe: vi.fn(),
    publish: vi.fn(),
    requestWithAsyncResult: vi.fn(),
    connected: true,
    error: null,
  }),
}))

import { fetchRoomKeysBootstrap, subscribeToRoomKeyEvents } from '@/api'

let lastDecryptHook: ReturnType<typeof useRoomKeys> | null = null

function Probe() {
  lastDecryptHook = useRoomKeys()
  return null
}

describe('RoomKeysProvider', () => {
  beforeEach(() => {
    lastDecryptHook = null
    vi.mocked(fetchRoomKeysBootstrap).mockReset()
    vi.mocked(subscribeToRoomKeyEvents).mockReset()
  })

  it('calls fetchRoomKeysBootstrap on mount and seeds state', async () => {
    vi.mocked(fetchRoomKeysBootstrap).mockResolvedValue({
      keys: [{ roomId: 'r1', version: 1, privateKey: btoa(String.fromCharCode(...new Uint8Array(32).fill(7))) }],
    })
    const unsub = { unsubscribe: vi.fn() }
    vi.mocked(subscribeToRoomKeyEvents).mockReturnValue(unsub)

    render(
      <RoomKeysProvider>
        <Probe />
      </RoomKeysProvider>,
    )

    await waitFor(() => {
      expect(fetchRoomKeysBootstrap).toHaveBeenCalled()
    })

    await waitFor(() => {
      expect(lastDecryptHook?.hasKey('r1', 1)).toBe(true)
    })
  })

  it('dispatches KEY_RECEIVED for each live event', async () => {
    vi.mocked(fetchRoomKeysBootstrap).mockResolvedValue({ keys: [] })
    let savedCb: ((evt: unknown) => void) | undefined
    vi.mocked(subscribeToRoomKeyEvents).mockImplementation((_n, cb) => {
      savedCb = cb as never
      return { unsubscribe: vi.fn() }
    })

    render(
      <RoomKeysProvider>
        <Probe />
      </RoomKeysProvider>,
    )

    await waitFor(() => expect(savedCb).toBeDefined())

    act(() => {
      savedCb!({
        roomId: 'r2',
        version: 3,
        privateKey: btoa(String.fromCharCode(...new Uint8Array(32).fill(1))),
        timestamp: Date.now(),
      })
    })

    await waitFor(() => expect(lastDecryptHook?.hasKey('r2', 3)).toBe(true))
  })

  it('unsubscribes and clears state on unmount', async () => {
    vi.mocked(fetchRoomKeysBootstrap).mockResolvedValue({ keys: [] })
    const unsub = { unsubscribe: vi.fn() }
    vi.mocked(subscribeToRoomKeyEvents).mockReturnValue(unsub)

    const { unmount } = render(
      <RoomKeysProvider>
        <Probe />
      </RoomKeysProvider>,
    )
    unmount()
    expect(unsub.unsubscribe).toHaveBeenCalled()
  })
})
```

- [ ] **Step 2: Run the test**

Run: `cd chat-frontend && npx vitest run src/context/RoomKeysContext/RoomKeysContext.test.tsx`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the provider**

`chat-frontend/src/context/RoomKeysContext/RoomKeysContext.tsx`:

```tsx
import { createContext, useCallback, useContext, useEffect, useReducer, useRef } from 'react'
import { fetchRoomKeysBootstrap, subscribeToRoomKeyEvents } from '@/api'
import type { RoomKeyEvent } from '@/api'
import { useNats } from '@/context/NatsContext'
import { b64decode, deriveAesKey, decryptRoomMessage } from '@/lib/roomcrypto'
import { initialRoomKeysState, roomKeysReducer } from './reducer'

type DecryptInput = {
  roomId: string
  version: number
  nonceB64: string
  ciphertextB64: string
}

type RoomKeysContextValue = {
  hasKey(roomId: string, version: number): boolean
  /** Returns null if the key is not (yet) known for that (roomId, version),
   *  or if decryption fails. */
  decrypt(input: DecryptInput): Promise<string | null>
}

const RoomKeysContext = createContext<RoomKeysContextValue | null>(null)

export function useRoomKeys(): RoomKeysContextValue {
  const ctx = useContext(RoomKeysContext)
  if (!ctx) throw new Error('useRoomKeys called outside RoomKeysProvider')
  return ctx
}

export function RoomKeysProvider({ children }: { children: React.ReactNode }) {
  const [state, dispatch] = useReducer(roomKeysReducer, initialRoomKeysState)
  const nats = useNats()

  // CryptoKey cache lives in a ref — derived lazily, not React state.
  // Keyed by `${roomId}|${version}`.
  const aesKeyCacheRef = useRef<Map<string, Promise<CryptoKey>>>(new Map())
  const stateRef = useRef(state)
  stateRef.current = state

  useEffect(() => {
    if (!nats.user) return
    let cancelled = false

    fetchRoomKeysBootstrap(nats)
      .then((resp) => {
        if (cancelled) return
        dispatch({
          type: 'BOOTSTRAP_LOADED',
          keys: resp.keys.map((k) => ({
            roomId: k.roomId,
            version: k.version,
            privateKey: b64decode(k.privateKey),
          })),
        })
      })
      .catch((err) => {
        // eslint-disable-next-line no-console
        console.error('roomKeysBootstrap failed:', err)
      })

    const sub = subscribeToRoomKeyEvents(nats, (raw) => {
      const evt = raw as RoomKeyEvent
      if (!evt || typeof evt.roomId !== 'string' || typeof evt.version !== 'number' || typeof evt.privateKey !== 'string') return
      dispatch({
        type: 'KEY_RECEIVED',
        roomId: evt.roomId,
        version: evt.version,
        privateKey: b64decode(evt.privateKey),
      })
    })

    return () => {
      cancelled = true
      sub.unsubscribe()
      aesKeyCacheRef.current.clear()
      dispatch({ type: 'CLEAR_KEYS' })
    }
    // user identity is stable for the session — see useRoomSubscriptions for prior art.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nats.user])

  const hasKey = useCallback((roomId: string, version: number) => {
    return !!stateRef.current.byRoom[roomId]?.[version]
  }, [])

  const decrypt = useCallback(async ({ roomId, version, nonceB64, ciphertextB64 }: DecryptInput): Promise<string | null> => {
    const entry = stateRef.current.byRoom[roomId]?.[version]
    if (!entry) return null

    const cacheKey = `${roomId}|${version}`
    let pending = aesKeyCacheRef.current.get(cacheKey)
    if (!pending) {
      pending = deriveAesKey(entry.privateKey)
      aesKeyCacheRef.current.set(cacheKey, pending)
    }
    try {
      const aesKey = await pending
      return await decryptRoomMessage(b64decode(ciphertextB64), b64decode(nonceB64), aesKey)
    } catch (err) {
      // eslint-disable-next-line no-console
      console.warn('roomKeysContext.decrypt failed:', err)
      return null
    }
  }, [])

  const value: RoomKeysContextValue = { hasKey, decrypt }

  return <RoomKeysContext.Provider value={value}>{children}</RoomKeysContext.Provider>
}
```

`chat-frontend/src/context/RoomKeysContext/index.tsx`:

```tsx
export { RoomKeysProvider, useRoomKeys } from './RoomKeysContext'
```

- [ ] **Step 4: Run the tests**

Run: `cd chat-frontend && npx vitest run src/context/RoomKeysContext/`
Expected: PASS.

- [ ] **Step 5: Typecheck**

Run: `cd chat-frontend && npm run typecheck`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add chat-frontend/src/context/RoomKeysContext/
git commit -m "feat(chat-frontend): RoomKeysProvider with bootstrap + live subscription"
```

---

### Task 24: Wire `RoomKeysProvider` into `App.jsx`

**Files:**
- Modify: `chat-frontend/src/App.jsx`

- [ ] **Step 1: Read the existing provider tree**

Run: `grep -n "Provider\|<NatsProvider\|<ThemeProvider\|<RoomEventsProvider" chat-frontend/src/App.jsx | head -20`

Identify where `RoomEventsProvider` is mounted — `RoomKeysProvider` must sit BETWEEN `NatsProvider` and `RoomEventsProvider` (RoomEvents will consume RoomKeys; both need Nats).

- [ ] **Step 2: Add the provider**

Wrap `RoomEventsProvider` with `RoomKeysProvider`:

```jsx
import { RoomKeysProvider } from '@/context/RoomKeysContext'
// ...
<NatsProvider>
  <RoomKeysProvider>
    <RoomEventsProvider>
      {/* ...existing tree... */}
    </RoomEventsProvider>
  </RoomKeysProvider>
</NatsProvider>
```

- [ ] **Step 3: Build + smoke**

Run: `cd chat-frontend && npm run typecheck && npm test`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add chat-frontend/src/App.jsx
git commit -m "feat(chat-frontend): mount RoomKeysProvider in app tree"
```

---

## Phase 6 — chat-frontend RoomEvents integration

### Task 25: Plumb `decrypt` from `RoomKeysContext` into `useRoomSubscriptions`

**Files:**
- Modify: `chat-frontend/src/context/RoomEventsContext/RoomEventsContext.tsx`
- Modify: `chat-frontend/src/context/RoomEventsContext/useRoomSubscriptions.js`

- [ ] **Step 1: Read the current provider and useRoomSubscriptions signature**

Run: `grep -n "useRoomSubscriptions\|useRoomKeys" chat-frontend/src/context/RoomEventsContext/RoomEventsContext.tsx | head -10`

- [ ] **Step 2: Update the provider to pass `decrypt` to `useRoomSubscriptions`**

In `RoomEventsContext.tsx`:

```tsx
import { useRoomKeys } from '@/context/RoomKeysContext'
// ...
const { decrypt } = useRoomKeys()
// ...
useRoomSubscriptions(
  nats,
  dispatch,
  stateRef,
  threadReplyHandlerRef,
  threadMessageMutationHandlerRef,
  decrypt,
)
```

- [ ] **Step 3: Extend `useRoomSubscriptions` signature**

In `useRoomSubscriptions.js`, add the new parameter and a ref so callbacks see the latest:

```js
export function useRoomSubscriptions(
  nats,
  dispatch,
  stateRef,
  threadReplyHandlerRef,
  threadMessageMutationHandlerRef,
  decrypt,
) {
  // ... existing code ...
  const decryptRef = useRef(decrypt)
  decryptRef.current = decrypt
  // ... use decryptRef.current(...) inside the subscription callbacks ...
}
```

- [ ] **Step 4: Decrypt new-message events before dispatching**

Replace the `evt?.type === 'new_message'` branch in both `subscribeToUserRoomEvents` and the `openChannelSub` body. The dispatcher becomes async — extract a helper:

```js
async function decryptAndDispatch(evt, dispatchFn) {
  if (evt.encryptedMessage && !evt.message) {
    const enc = evt.encryptedMessage
    if (typeof enc.version === 'number' && enc.nonce && enc.ciphertext) {
      const plaintext = await decryptRef.current({
        roomId: evt.roomId,
        version: enc.version,
        nonceB64: enc.nonce,
        ciphertextB64: enc.ciphertext,
      })
      if (plaintext != null) {
        try {
          const msg = JSON.parse(plaintext)
          // Reuse the existing reducer path by populating evt.message.
          evt = { ...evt, message: msg, encryptedMessage: undefined }
        } catch (err) {
          // eslint-disable-next-line no-console
          console.warn('decrypted message is not valid JSON', err)
        }
      }
    }
  }
  if (evt.messageEdited && evt.messageEdited.encryptedNewContent && !evt.messageEdited.newContent) {
    const enc = evt.messageEdited.encryptedNewContent
    if (typeof enc.version === 'number' && enc.nonce && enc.ciphertext) {
      const plaintext = await decryptRef.current({
        roomId: evt.roomId,
        version: enc.version,
        nonceB64: enc.nonce,
        ciphertextB64: enc.ciphertext,
      })
      if (plaintext != null) {
        evt = {
          ...evt,
          messageEdited: { ...evt.messageEdited, newContent: plaintext, encryptedNewContent: undefined },
        }
      }
    }
  }
  dispatchFn(evt)
}
```

Then in each subscription callback:

```js
const dmSub = subscribeToUserRoomEvents(liveNats, (evt) => {
  if (evt?.type === 'new_message') {
    decryptAndDispatch(evt, (decoded) => {
      safeDispatch({ type: 'MESSAGE_RECEIVED', event: decoded })
      fanThreadReply(decoded)
      if (!decoded.message?.threadParentMessageId) {
        scheduleMarkActiveRead(decoded.roomId)
      }
    }).catch((err) => console.warn('decryptAndDispatch DM failed', err))
    return
  }
  handleMutationEvent(evt)
})

const openChannelSub = (roomId) => {
  if (channelSubs.current.has(roomId)) return
  const sub = subscribeToRoomEvents(natsRef.current, { roomId }, (evt) => {
    if (evt?.type === 'new_message') {
      decryptAndDispatch(evt, (decoded) => {
        const hasMention = (decoded.mentions ?? []).some((p) => p.account === user.account)
        const normalized = { ...decoded, hasMention }
        safeDispatch({ type: 'MESSAGE_RECEIVED', event: normalized })
        fanThreadReply(normalized)
        if (!decoded.message?.threadParentMessageId) {
          scheduleMarkActiveRead(decoded.roomId ?? roomId)
        }
      }).catch((err) => console.warn('decryptAndDispatch channel failed', err))
      return
    }
    if (evt?.type === 'message_edited') {
      decryptAndDispatch(evt, (decoded) => {
        handleMutationEvent(decoded)
      }).catch((err) => console.warn('decryptAndDispatch edit failed', err))
      return
    }
    handleMutationEvent(evt)
  })
  channelSubs.current.set(roomId, sub)
}
```

- [ ] **Step 5: Run the existing tests**

Run: `cd chat-frontend && npm test`
Expected: All existing tests pass. Some may need a small tweak if they pass `useRoomSubscriptions` without the new `decrypt` arg — provide a no-op default for the param to keep backward compatibility:

```js
export function useRoomSubscriptions(
  nats, dispatch, stateRef, threadReplyHandlerRef, threadMessageMutationHandlerRef,
  decrypt = async () => null,
) {
```

- [ ] **Step 6: Typecheck**

Run: `cd chat-frontend && npm run typecheck`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add chat-frontend/src/context/RoomEventsContext/
git commit -m "feat(chat-frontend): decrypt new-message + edit events before dispatch"
```

---

### Task 26: Reducer test — decoded event flows through normally

**Files:**
- Modify: `chat-frontend/src/context/RoomEventsContext/reducer.test.js`

The existing test at line 190 covers the "no plaintext, only encryptedMessage" fallback. Add a new test covering the post-decrypt happy path: when `evt.message` is populated (by the dispatcher) and `evt.encryptedMessage` is absent, the reducer treats it as a normal new-message event.

- [ ] **Step 1: Append the test**

```js
it('treats a decrypted MESSAGE_RECEIVED as a normal new-message event', () => {
  const initial = {
    /* paste the standard initial-state fixture used by other tests in this file */
  }
  const reducer = /* import the existing reducer used by the file */
  const action = {
    type: 'MESSAGE_RECEIVED',
    event: newMessageEvent({
      message: { id: 'm1', roomId: 'r1', content: 'decrypted body', createdAt: '2026-05-20T00:00:00Z' },
    }),
  }
  const next = reducer(initial, action)
  const inserted = next.roomState.r1.messages.find((m) => m.id === 'm1')
  expect(inserted).toBeDefined()
  expect(inserted.content).toBe('decrypted body')
  expect(inserted.encrypted).toBeFalsy()
})
```

Adapt `newMessageEvent` and `initial`/`reducer` imports to match the file's existing patterns — read the top of the file first to see the existing helpers.

- [ ] **Step 2: Run the test**

Run: `cd chat-frontend && npx vitest run src/context/RoomEventsContext/reducer.test.js`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add chat-frontend/src/context/RoomEventsContext/reducer.test.js
git commit -m "test(chat-frontend): reducer handles decrypted event identically to plaintext"
```

---

## Phase 7 — Docs and cleanup

### Task 27: Update `docs/client-api.md`

**Files:**
- Modify: `docs/client-api.md`

- [ ] **Step 1: Locate the encryption section**

Run: `grep -n "encryption\|encryptedMessage\|EphemeralPublicKey\|roomcrypto" docs/client-api.md`

- [ ] **Step 2: Rewrite the encryption section**

Update the wire-format description to match the new shape `{version, nonce, ciphertext}`. Remove every reference to `ephemeralPublicKey`. Add a new subsection for the bootstrap RPC `chat.user.{account}.request.rooms.keys`. Update the §4.1 RoomEvent example so the embedded `encryptedMessage` matches the new format.

Concrete content to add (paste verbatim into the appropriate place):

````markdown
### Room message encryption

`encryptedMessage` on a `RoomEvent` is JSON with the following shape:

```json
{
  "version": 7,
  "nonce": "base64-12-bytes",
  "ciphertext": "base64-content-plus-16-byte-tag"
}
```

Decoding procedure:

1. Look up the room private key for `version` from the client-side key
   store. If absent, render the message as `[encrypted message]` until
   the key arrives via `chat.user.{account}.event.room.key` or a fresh
   bootstrap call.
2. Derive an AES-256 key:
   `aesKey = HKDF-SHA256(roomPrivateKey, salt=empty, info="room-message-encryption-v2", length=32)`.
3. Decrypt: `AES-GCM-Decrypt(aesKey, nonce, ciphertext, aad=empty)`. The
   ciphertext already includes the 16-byte GCM tag at the end (Go's
   `cipher.AEAD.Seal` format).
4. The plaintext is a UTF-8-encoded JSON `ClientMessage` for
   `encryptedMessage`, or a UTF-8 string for
   `messageEdited.encryptedNewContent`.

### `chat.user.{account}.request.rooms.keys`

Request payload: empty object `{}`.

Response: `{"keys": [{"roomId": "...", "version": 0, "privateKey": "base64-32-bytes"}, ...]}`.

The server returns one entry per room the caller is currently subscribed
to and for which a key exists in Valkey. Clients call this RPC on
(re)connect to seed their key cache before subscribing to live updates
on `chat.user.{account}.event.room.key`.
````

- [ ] **Step 3: Commit**

```bash
git add docs/client-api.md
git commit -m "docs(client-api): document HKDF-only encryption and keys-bootstrap RPC"
```

---

### Task 28: Final verification

- [ ] **Step 1: Server lint + tests**

Run:
```bash
make lint
make test
make sast
```
Expected: All pass. Fix any new issues.

- [ ] **Step 2: chat-frontend typecheck + tests + build**

Run:
```bash
cd chat-frontend
npm run typecheck
npm test
npm run build
```
Expected: All pass.

- [ ] **Step 3: Integration tests (Docker required)**

Run:
```bash
make test-integration SERVICE=roomcrypto
make test-integration SERVICE=room-service
make test-integration SERVICE=broadcast-worker
```
Expected: All pass.

- [ ] **Step 4: Final benchmark capture**

Run: `go test -bench=. -benchmem -benchtime=2s -run=^$ ./pkg/roomcrypto/`

Confirm `BenchmarkEncoder_Encode` is in the hundreds of nanoseconds range (vs the legacy `BenchmarkEncode` was ~87 µs). If the number is dramatically different from expectations, investigate before merging.

- [ ] **Step 5: Final commit (optional — only if there are stragglers)**

If steps 1–4 surface any new fixes, commit them with a concise message; otherwise this task ends here.

---

## Plan summary

- **Tasks 1–9:** Server `pkg/roomcrypto` + `broadcast-worker` migration. New `Encoder` introduced TDD, legacy `Encode` removed once callers migrate, integration test updated to new TS decoder.
- **Tasks 10–13:** New `room-service` RPC `chat.user.{account}.request.rooms.keys` plus model types and an optional integration test.
- **Tasks 14–17:** chat-frontend `lib/roomcrypto` module via Web Crypto API, locked against the Go encoder by a committed cross-language fixture.
- **Tasks 18–21:** chat-frontend API ops — subject builders, types, `subscribeToRoomKeyEvents`, `fetchRoomKeysBootstrap`.
- **Tasks 22–24:** chat-frontend `RoomKeysContext` — reducer, provider with bootstrap-on-mount + live subscription, ref-backed `CryptoKey` cache, mounted in `App.jsx`.
- **Tasks 25–26:** chat-frontend `RoomEventsContext` integration — decrypt-then-dispatch in `useRoomSubscriptions`, new reducer test for the decoded happy path.
- **Tasks 27–28:** `docs/client-api.md` update and final verification gate.
