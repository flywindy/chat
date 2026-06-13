# Valkey Room Key Library — Design Spec

**Date:** 2026-03-30
**Status:** Approved
**Scope:** `pkg/roomkeystore` (new package)

> **Superseded (2026-06-09):** Room keys no longer live in Valkey. They moved
> into the room's document in the MongoDB `rooms` collection under an `encKey`
> sub-document, and the Valkey-backed implementation was removed. The
> `RoomKeyStore` interface is preserved. See
> `2026-06-09-room-keys-mongo-migration-design.md`. The original Valkey design
> below is retained as a historical record.

## Overview

Introduce `pkg/roomkeystore` — a shared library for storing and retrieving room encryption key pairs (P-256 public + private keys) in Valkey as primary storage. The library is consumed by any service that needs to read or write room keys (e.g. `room-service` for key generation, `message-worker` for encryption).

## Motivation

The `pkg/roomcrypto` package encrypts messages using a room's P-256 public key, but there is currently no storage layer for room key material. This library provides that storage using Valkey (Redis-compatible in-memory store), giving fast key lookups at encryption time.

## Architecture

A single new package `pkg/roomkeystore` containing:

- `RoomKeyPair` — data struct carrying raw key bytes
- `RoomKeyStore` — interface defined in the package (shared infrastructure, not per-service)
- `valkeyStore` — unexported Valkey implementation backed by `go-redis/v9`
- `Config` — typed config struct parsed via `caarlos0/env`
- `NewValkeyStore` — constructor returning `(*valkeyStore, error)`

No additional `pkg/valkeyutil` abstraction is introduced. If Valkey usage expands to other packages in the future, a utility package can be extracted then.

## Data Model

```go
// RoomKeyPair holds the raw P-256 key bytes for a room.
type RoomKeyPair struct {
    PublicKey  []byte // 65-byte uncompressed point
    PrivateKey []byte // 32-byte scalar
}
```

### Valkey Storage Layout

Each room's key pair is stored as a Valkey **hash** at:

```
room:{roomID}:key
```

| Hash field | Value | Encoding |
|---|---|---|
| `pub` | 65-byte P-256 uncompressed public key | standard base64 |
| `priv` | 32-byte P-256 private key scalar | standard base64 |

Keys persist indefinitely until explicitly deleted or rotated. No TTL is applied to current keys.

## Interface & Constructor

```go
type RoomKeyStore interface {
    Set(ctx context.Context, roomID string, pair RoomKeyPair) error
    Get(ctx context.Context, roomID string) (*RoomKeyPair, error)
    Delete(ctx context.Context, roomID string) error
}

type Config struct {
    Addr        string        `env:"VALKEY_ADDR,required"`
    Password    string        `env:"VALKEY_PASSWORD" envDefault:""`
    GracePeriod time.Duration `env:"VALKEY_KEY_GRACE_PERIOD,required"`
}

func NewValkeyStore(cfg Config) (*valkeyStore, error)
```

### Behaviour Contract

- `Set` — stores both fields in the hash with no TTL. Wraps all errors with context.
- `Get` — returns `(nil, nil)` when the key does not exist (missing key is an expected condition, not an error). Returns a decoded `*RoomKeyPair` on success. Returns a non-nil error on Valkey failures or corrupted base64.
- `Delete` — removes the hash key. No-op if the key doesn't exist. Wraps errors with context.

## Error Handling

- All errors are wrapped: `fmt.Errorf("set room key: %w", err)`, `fmt.Errorf("get room key: %w", err)`, etc.
- No sentinel errors exported — callers check `result == nil` from `Get` to detect a missing key.
- Corrupted base64 in a stored field is treated as an error (not silently ignored).

## Configuration

Environment variables parsed via `caarlos0/env`:

| Env var | Required | Description |
|---|---|---|
| `VALKEY_ADDR` | yes | `host:port` of the Valkey instance |
| `VALKEY_PASSWORD` | no | Auth password (empty = no auth) |
| `VALKEY_KEY_GRACE_PERIOD` | yes | Duration for previous key retention after rotation, e.g. `1h`, `24h` |

`GracePeriod` controls how long the previous key remains readable after a rotation.

## Dependencies

A new third-party dependency is required:

| Module | Version | Purpose |
|---|---|---|
| `github.com/redis/go-redis/v9` | latest stable | Valkey/Redis client |

`go-redis/v9` is the standard Go client for Redis-compatible stores including Valkey. No other new dependencies are introduced.

## File Layout

```
pkg/roomkeystore/
├── roomkeystore.go          # RoomKeyPair, RoomKeyStore interface, Config, NewValkeyStore, valkeyStore impl
├── roomkeystore_test.go     # Unit tests (table-driven, mock Valkey via interface)
└── integration_test.go      # Integration tests with testcontainers (//go:build integration)
```

No `store.go` split is needed — the interface and implementation are small enough for a single file.

## Testing

### Unit Tests (`roomkeystore_test.go`)

Table-driven tests covering:

| Scenario | Method |
|---|---|
| Happy path — set and get round-trip | `Set` + `Get` |
| Get missing key returns `(nil, nil)` | `Get` |
| Valkey error on `Set` | `Set` |
| Valkey error on `Get` | `Get` |
| Corrupted base64 in stored `pub` field | `Get` |
| Corrupted base64 in stored `priv` field | `Get` |
| Delete existing key | `Delete` |
| Delete missing key is no-op | `Delete` |
| Valkey error on `Delete` | `Delete` |

The `go-redis` client does not ship a mock. Unit tests inject a fake by defining a minimal local interface over the Valkey commands used (`HSet`, `HGetAll`, `Del`) and substituting a test double. This keeps unit tests fast and free of Docker.

### Integration Tests (`integration_test.go`, `//go:build integration`)

Uses `testcontainers-go` with a generic `valkey/valkey:8` container (no official testcontainers module exists — follows the same pattern as `pkg/roomcrypto`'s Node container). Tests:

- Full round-trip: `Set` → `Get` → `Delete`
- Grace period expiry: rotate with a short grace period (1s), verify old key expires after sleep
- Missing key returns `(nil, nil)`

### Coverage Target

≥ 90% for `roomkeystore.go` — all error paths and the nil-result branch must be covered.
